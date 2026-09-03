package obs

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// ★出口节点是 2 vCPU / 442 MB 的小机，agent 常驻 22–28 MB。
// 缓冲无上限时，OBS_ENDPOINT 不可达会一路 append 到 OOM——
// 观测组件把主链路打死是绝不可接受的故障模式。下面钉死上限行为。
func TestReporter_EnqueueIsBounded(t *testing.T) {
	r := NewReporter("http://127.0.0.1:1/never", "", Identity{NodeID: "n1"}, t.TempDir())

	for i := 0; i < maxPendingEnvelopes+500; i++ {
		r.Enqueue(SchemaConnLifecycle, ConnLifecyclePayload{})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) != maxPendingEnvelopes {
		t.Fatalf("pending must be capped at %d, got %d", maxPendingEnvelopes, len(r.pending))
	}
	if r.dropped != 500 {
		t.Fatalf("dropped counter must record the 500 evicted, got %d", r.dropped)
	}
}

// 缓冲满时丢【最旧】的：正在发生的故障比几小时前的更有排障价值。
func TestReporter_EvictsOldestNotNewest(t *testing.T) {
	r := NewReporter("http://127.0.0.1:1/never", "", Identity{NodeID: "n1"}, t.TempDir())

	for i := 0; i < maxPendingEnvelopes; i++ {
		r.Enqueue("filler", nil)
	}
	r.Enqueue("newest", nil) // 顶掉一个 filler

	r.mu.Lock()
	defer r.mu.Unlock()
	if got := r.pending[len(r.pending)-1].Schema; got != "newest" {
		t.Fatalf("newest envelope must survive, last schema=%s", got)
	}
}

// 上报必须带 Bearer node token（与计费同源）。
func TestReporter_SendsBearerToken(t *testing.T) {
	var (
		mu   sync.Mutex
		auth string
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		auth = req.Header.Get("Authorization")
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewReporter(srv.URL, "s3cret", Identity{NodeID: "n1"}, t.TempDir())
	r.Enqueue(SchemaConnLifecycle, ConnLifecyclePayload{})
	r.Flush()

	mu.Lock()
	defer mu.Unlock()
	if hits == 0 {
		t.Fatal("reporter did not POST")
	}
	if auth != "Bearer s3cret" {
		t.Fatalf("want Bearer token, got %q", auth)
	}
}

// ★apiKey 为空时不发头（而不是发 "Bearer "）——collector 灰度期靠"无头"识别未升级的 agent。
func TestReporter_NoTokenSendsNoHeader(t *testing.T) {
	var (
		mu     sync.Mutex
		hasHdr bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		_, hasHdr = req.Header["Authorization"]
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewReporter(srv.URL, "", Identity{NodeID: "n1"}, t.TempDir())
	r.Enqueue(SchemaConnLifecycle, ConnLifecyclePayload{})
	r.Flush()

	mu.Lock()
	defer mu.Unlock()
	if hasHdr {
		t.Fatal("empty apiKey must send no Authorization header at all")
	}
}
