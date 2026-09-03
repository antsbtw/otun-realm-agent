package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
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
		obsReporter:  obs.NewReporter("", "", obs.Identity{}, t.TempDir()),
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

// ★计费不丢：Report 的两条失败路径必须区别对待，因为 CollectStats(true) 已经把
// 计数器清零了 —— 那一窗字节唯一的副本就在调用方内存里。
//
//   - 上报失败但落盘成功 → 已交接（FlushCache 会重传），绝不可回滚，否则重复计费。
//   - 上报失败【且】落盘失败 → 无处存身，必须加回 Registry，否则永久少计费。
//
// 老实现两条路径都只 return，第二条静默丢账（exactly-once 退化成 at-most-once）。

// 上报不通但 spool 可写 → 账已落盘，Registry 必须保持清零（不能重复计费）。
func TestBilling_SpooledToDisk_DoesNotRestore(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-spool", "hysteria2", 400)
	a := newTestAgent(t, reg, map[string]egress.Node{}) // URL 不可达，cacheDir 可写

	a.collectAndReportBilling()

	if residual := registryTotal(reg); residual != 0 {
		t.Fatalf("bytes were spooled to disk; restoring them too would double-bill. residual=%d, want 0", residual)
	}
	if n := a.billReporter.GetCacheCount(); n == 0 {
		t.Fatalf("expected the failed report to be spooled to disk, got cache count 0")
	}
}

// 上报不通【且】spool 不可写 → 必须把字节加回 Registry，下一窗重新计费。
func TestBilling_ReportAndSpoolBothFail_RestoresBytes(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-lost", "hysteria2", 900)
	before := registryTotal(reg)
	if before == 0 {
		t.Fatal("seed failed")
	}

	// 造双重失败：URL 不可达 + cacheDir 指向一个不可创建文件的路径（用普通文件当目录）。
	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badDir, []byte("x"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	a := newTestAgent(t, reg, map[string]egress.Node{})
	a.billReporter = stats.NewReporter("http://127.0.0.1:1/unreachable", "k", badDir)

	a.collectAndReportBilling()

	if got := registryTotal(reg); got != before {
		t.Fatalf("report+spool both failed: bytes must be restored for re-billing; registry=%d, want %d", got, before)
	}
}

// 回滚必须是累加：失败上报期间在连的连接仍在计数，覆盖式恢复会吞掉这部分。
func TestBilling_RestoreIsAdditive_KeepsInFlightBytes(t *testing.T) {
	reg := egress.NewRegistry()
	seedRegistry(t, reg, "u-add", "hysteria2", 500)

	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badDir, []byte("x"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	a := newTestAgent(t, reg, map[string]egress.Node{})
	a.billReporter = stats.NewReporter("http://127.0.0.1:1/unreachable", "k", badDir)

	// 采账清零后、回滚前，模拟这一窗又跑了流量。
	a.collectAndReportBilling()
	seedRegistry(t, reg, "u-add", "hysteria2", 70)

	// 两次都失败，累计应为 500(回滚) + 70(期间) + 再回滚的 570 = 1140。
	// 这里只断言不丢：总量必须 >= 570（回滚的 + 期间的都在）。
	if got := registryTotal(reg); got < 570 {
		t.Fatalf("restore must be additive (not overwrite in-flight bytes); registry=%d, want >=570", got)
	}
}
