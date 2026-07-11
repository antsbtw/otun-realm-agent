package config

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// urlRecorder 记录每个路径最后一次命中的 host（用来断言请求打到了哪台服务）。
type urlRecorder struct {
	mu   sync.Mutex
	hits map[string]string // path -> host
}

func newRecorder() *urlRecorder { return &urlRecorder{hits: map[string]string{}} }

func (r *urlRecorder) server(name string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[req.URL.Path] = name
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"kick_users":[],"reload_users":false,"version":"v0","users":[]}`))
	}))
}

func (r *urlRecorder) hit(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// ★零回归铁律：fleetURL 空 → register/heartbeat 回退打 otun(apiURL)（不传 --fleet-url = 旧行为）。
// 删掉 NewSyncer 里 `if fleetURL=="" { fleetURL=apiURL }` 的回退 → register/heartbeat 打到空 host → 请求失败 → 本测试红。
func TestSyncer_FleetURLEmptyFallsBackToOtun(t *testing.T) {
	rec := newRecorder()
	otun := rec.server("otun")
	defer otun.Close()

	s := NewSyncer(otun.URL, "", "key", "dev") // fleetURL 空
	if err := s.Register("n1", "r1", "CN", []string{"hysteria2"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Heartbeat(&HeartbeatRequest{NodeID: "n1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if got := rec.hit("/api/node/register"); got != "otun" {
		t.Fatalf("empty fleetURL: register should hit otun (fallback), hit %q", got)
	}
	if got := rec.hit("/api/node/heartbeat"); got != "otun" {
		t.Fatalf("empty fleetURL: heartbeat should hit otun (fallback), hit %q", got)
	}
}

// ★拆分正确：fleetURL 非空 → register/heartbeat 打 fleet；FetchUsers/ReportConnections 仍打 otun。
// 若误把 FetchUsers/ReportConnections 也切到 fleetURL（搬断计费/下发）→ 本测试红。
func TestSyncer_SplitRoutesRegisterHeartbeatToFleetKeepsRest(t *testing.T) {
	rec := newRecorder()
	otun := rec.server("otun")
	fleet := rec.server("fleet")
	defer otun.Close()
	defer fleet.Close()

	s := NewSyncer(otun.URL, fleet.URL, "key", "dev")

	if err := s.Register("n1", "r1", "CN", []string{"hysteria2"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Heartbeat(&HeartbeatRequest{NodeID: "n1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := s.FetchUsers(); err != nil {
		t.Fatalf("fetchusers: %v", err)
	}
	if _, err := s.ReportConnections(&ConnectionsReport{NodeID: "n1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("reportconnections: %v", err)
	}

	// 纳管 → fleet。
	if got := rec.hit("/api/node/register"); got != "fleet" {
		t.Fatalf("register should hit fleet, hit %q", got)
	}
	if got := rec.hit("/api/node/heartbeat"); got != "fleet" {
		t.Fatalf("heartbeat should hit fleet, hit %q", got)
	}
	// 用户下发 + 计费/踢人 → 仍 otun（不搬断计费）。
	if got := rec.hit("/api/node/users"); got != "otun" {
		t.Fatalf("FetchUsers must stay on otun, hit %q", got)
	}
	if got := rec.hit("/api/node/connections"); got != "otun" {
		t.Fatalf("ReportConnections (billing/kick) must stay on otun, hit %q", got)
	}
}
