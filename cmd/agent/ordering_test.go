package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antsbtw/otun-s-egress/egress"
	"github.com/antsbtw/otun-s-egress/node/meter"

	"otun-realm-agent/internal/config"
	"otun-realm-agent/internal/obs"
	"otun-realm-agent/internal/quota"
	"otun-realm-agent/internal/stats"
)

// recordingNode is a fake egress.Node that records UpdateUsers/Close so tests can
// assert lifecycle. Metering/kick/obs now go through the SHARED Registry (C.1),
// tested separately against a real registry.
type recordingNode struct {
	proto       string
	clock       *int64 // shared monotonic tick (order across nodes)
	updateAt    atomic.Int64
	updateCount atomic.Int32
	failUpdate  bool
	closed      atomic.Bool
	started     atomic.Bool
}

func (n *recordingNode) Start(ctx context.Context, conn net.PacketConn) error {
	n.started.Store(true)
	return nil
}
func (n *recordingNode) Close() error { n.closed.Store(true); return nil }
func (n *recordingNode) UpdateUsers(users []egress.User) error {
	n.updateCount.Add(1)
	n.updateAt.Store(atomic.AddInt64(n.clock, 1))
	if n.failUpdate {
		return errTest
	}
	return nil
}
func (n *recordingNode) CollectStats(reset bool) []egress.UserStat { return nil }
func (n *recordingNode) KickUser(uuid string) int                  { return 0 }
func (n *recordingNode) ActiveConnections() []egress.ConnInfo      { return nil }
func (n *recordingNode) ActiveUserCount() int                      { return 0 }

type testErr string

func (e testErr) Error() string { return string(e) }

const errTest = testErr("simulated UpdateUsers failure")

// seedRegistry accrues upload bytes for uuid on proto (via Track + a pipe read),
// so CollectStats(true/false) has real data to bill/snapshot. Leaves the tracked
// conn open so Snapshot sees a live conn.
func seedRegistry(t *testing.T, reg *egress.Registry, uuid, proto string, bytesN int) {
	t.Helper()
	client, server := net.Pipe()
	tracked := reg.Track(uuid, server, meter.ConnMeta{Protocol: proto})
	done := make(chan struct{})
	go func() {
		buf := make([]byte, bytesN)
		_, _ = tracked.Read(buf) // counts upload on server side
		close(done)
	}()
	if _, err := client.Write(make([]byte, bytesN)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	<-done
	t.Cleanup(func() { _ = client.Close(); _ = tracked.Close() })
}

// newTestAgent builds a driveable agent with a REAL shared Registry + fake nodes.
func newTestAgent(t *testing.T, reg *egress.Registry, nodes map[string]egress.Node) *Agent {
	t.Helper()
	cache := t.TempDir()
	return &Agent{
		cfg:          &config.AgentConfig{NodeID: "test", StatsInterval: 60 * time.Second},
		cache:        config.NewCache(t.TempDir()),
		billReporter: stats.NewReporter("http://127.0.0.1:1/unreachable", "k", cache),
		obsReporter:  obs.NewReporter("", obs.Identity{}, t.TempDir()),
		monitor:      quota.NewMonitor(nil),
		generator:    config.NewRealmGenerator("/nope.crt", "/nope.key"),
		registry:     reg,
		nodes:        nodes,
		conns:        map[string]net.PacketConn{},
	}
}

func registryTotal(reg *egress.Registry) int64 {
	var total int64
	for _, s := range reg.CollectStats(false) {
		total += s.Upload + s.Download
	}
	return total
}

// applyUpdate must CollectStats(true)+Report on the shared Registry BEFORE any node
// UpdateUsers (§2 删人先采账铁律): removed users' last window must be billed. We
// prove the collect(reset=true) ran by observing the shared registry was zeroed.
func TestApplyUpdate_CollectBeforeUpdate(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-1", "hysteria2", 500)
	if registryTotal(reg) == 0 {
		t.Fatalf("precondition: registry should hold u-1 bytes")
	}

	var clock int64
	hy2 := &recordingNode{proto: "hysteria2", clock: &clock}
	tuic := &recordingNode{proto: "tuic", clock: &clock}
	a := newTestAgent(t, reg, map[string]egress.Node{"hysteria2": hy2, "tuic": tuic})

	resp := &config.UsersResponse{
		Version: "v2", UserVersion: "u2", RealmVersion: "r1",
		Realm: &config.RealmBlock{RealmID: "r", Protocols: []string{"hysteria2", "tuic"}},
		Users: []config.User{{UUID: "u-1", Enabled: true}},
	}
	if err := a.applyUpdate(resp); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}

	if hy2.updateCount.Load() != 1 || tuic.updateCount.Load() != 1 {
		t.Fatalf("each node should get 1 UpdateUsers; hy2=%d tuic=%d",
			hy2.updateCount.Load(), tuic.updateCount.Load())
	}
	// Billing collect(reset=true) must have run BEFORE UpdateUsers → the seeded bytes
	// are gone (bill-once). If UpdateUsers had run first without a prior collect, the
	// removed-user window would be lost — the rule exists to prevent exactly that.
	if residual := registryTotal(reg); residual != 0 {
		t.Fatalf("collect(reset=true) must have zeroed the shared registry before UpdateUsers; residual=%d", residual)
	}
}

// Billing (reset=true) and profile (reset=false) must be TWO separate Registry
// calls (§5): the profile snapshot must not consume the billable bytes.
func TestBillingAndProfile_SeparateCollectCalls(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-1", "hysteria2", 300)
	a := newTestAgent(t, reg, map[string]egress.Node{})

	a.snapshotTrafficProfile() // reset=false must NOT zero the registry
	if registryTotal(reg) == 0 {
		t.Fatalf("profile snapshot(reset=false) must not consume billable bytes")
	}

	a.collectAndReportBilling() // reset=true zeroes it
	if residual := registryTotal(reg); residual != 0 {
		t.Fatalf("billing collect(reset=true) must zero the registry; residual=%d", residual)
	}
}

// rebuildProtocols must touch ONLY the named protocol (施工单 §3.7): the healthy
// protocol's node stays open and the SHARED Registry pointer is preserved (not
// replaced). tuic's real egress.New fails (empty UUID) so its restart errors, but
// hy2 must remain untouched and a.registry must be the same instance.
func TestRebuildProtocols_OnlyTouchesNamedProtocol(t *testing.T) {
	reg := egress.NewRegistry()
	var clock int64
	hy2 := &recordingNode{proto: "hysteria2", clock: &clock}
	tuic := &recordingNode{proto: "tuic", clock: &clock}
	a := newTestAgent(t, reg, map[string]egress.Node{"hysteria2": hy2, "tuic": tuic})

	resp := &config.UsersResponse{
		Realm: &config.RealmBlock{RealmID: "r", Protocols: []string{"hysteria2", "tuic"}},
		Users: []config.User{{UUID: "u-1", Enabled: true}},
	}

	regBefore := a.registry
	err := a.rebuildProtocols(resp, []string{"tuic"})
	if err == nil {
		t.Fatalf("expected tuic rebuild to fail (empty UUID), got nil")
	}
	if a.registry != regBefore {
		t.Fatalf("per-protocol rebuild must preserve the shared Registry instance")
	}
	if hy2.closed.Load() {
		t.Fatalf("healthy protocol hy2 must NOT be closed during per-protocol rebuild of tuic")
	}
	if !tuic.closed.Load() {
		t.Fatalf("failed protocol tuic's old node should have been closed for rebuild")
	}
}

// A fully-healthy update touches every node exactly once and closes none (no
// rebuild needed) and never replaces the shared Registry.
func TestApplyUpdate_AllHealthy_NoRebuild(t *testing.T) {
	reg := egress.NewRegistry()
	var clock int64
	hy2 := &recordingNode{proto: "hysteria2", clock: &clock}
	tuic := &recordingNode{proto: "tuic", clock: &clock}
	a := newTestAgent(t, reg, map[string]egress.Node{"hysteria2": hy2, "tuic": tuic})

	resp := &config.UsersResponse{
		Version: "v2", UserVersion: "u2", RealmVersion: "r1",
		Realm: &config.RealmBlock{RealmID: "r", Protocols: []string{"hysteria2", "tuic"}},
		Users: []config.User{{UUID: "u-1", Enabled: true}},
	}
	if err := a.applyUpdate(resp); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}
	if hy2.updateCount.Load() != 1 || tuic.updateCount.Load() != 1 {
		t.Fatalf("each node should get exactly 1 UpdateUsers; hy2=%d tuic=%d",
			hy2.updateCount.Load(), tuic.updateCount.Load())
	}
	if hy2.closed.Load() || tuic.closed.Load() {
		t.Fatalf("no rebuild needed → no node should be closed")
	}
	if a.registry != reg {
		t.Fatalf("healthy update must not replace the shared Registry")
	}
}

// KickUser goes through the shared Registry once and drops the user across ALL
// protocols (C.1). Seed two protocols for one UUID; one kick closes both.
func TestKickUser_SharedRegistry_AllProtocols(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-1", "hysteria2", 100)
	seedRegistry(t, reg, "u-1", "tuic", 100)
	a := newTestAgent(t, reg, map[string]egress.Node{})

	if before := len(reg.Snapshot()); before != 2 {
		t.Fatalf("precondition: expected 2 live conns across protocols, got %d", before)
	}
	kicked := a.kickUser("u-1")
	if kicked != 2 {
		t.Fatalf("one KickUser via shared Registry must drop all protocols' conns; kicked=%d", kicked)
	}
	if after := len(reg.Snapshot()); after != 0 {
		t.Fatalf("after kick no live conns should remain; got %d", after)
	}
}
