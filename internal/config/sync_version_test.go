package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// bodyRecorder 记录每个路径最后一次请求体（断言上报内容，不只是打到哪台）。
type bodyRecorder struct {
	mu     sync.Mutex
	bodies map[string][]byte // path -> last body
}

func newBodyRecorder() *bodyRecorder { return &bodyRecorder{bodies: map[string][]byte{}} }

func (r *bodyRecorder) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies[req.URL.Path] = body
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"kick_users":[],"reload_users":false}`))
	}))
}

func (r *bodyRecorder) version(t *testing.T, path string) string {
	t.Helper()
	r.mu.Lock()
	body := r.bodies[path]
	r.mu.Unlock()
	if body == nil {
		t.Fatalf("no request captured on %s", path)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal %s body: %v", path, err)
	}
	return payload.Version
}

// ★U0 核心断言：register 和 heartbeat 的请求体都携带注入的 buildVersion。
// 若谁把 Register 里的版本改回硬编码、或 Heartbeat 忘了盖章 → 本测试红
// → fleet nodes.version 回到"全网 2.0.0"的监控盲区。
func TestSyncer_ReportsInjectedBuildVersion(t *testing.T) {
	rec := newBodyRecorder()
	srv := rec.server()
	defer srv.Close()

	s := NewSyncer(srv.URL, "", "key", "abc123def")

	if err := s.Register("n1", "r1", "CN", []string{"hysteria2"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Heartbeat(&HeartbeatRequest{NodeID: "n1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if got := rec.version(t, "/api/node/register"); got != "abc123def" {
		t.Fatalf("register version = %q, want injected %q", got, "abc123def")
	}
	if got := rec.version(t, "/api/node/heartbeat"); got != "abc123def" {
		t.Fatalf("heartbeat version = %q, want injected %q", got, "abc123def")
	}
}

// buildVersion 未注入（本地构建）→ 兜底 "dev"，绝不上报空串
// （空串会把 fleet nodes.version 洗掉，比 2.0.0 硬编码还糟）。
func TestSyncer_EmptyBuildVersionFallsBackToDev(t *testing.T) {
	rec := newBodyRecorder()
	srv := rec.server()
	defer srv.Close()

	s := NewSyncer(srv.URL, "", "key", "")

	if err := s.Register("n1", "r1", "CN", []string{"hysteria2"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := rec.version(t, "/api/node/register"); got != "dev" {
		t.Fatalf("register version = %q, want fallback %q", got, "dev")
	}
}
