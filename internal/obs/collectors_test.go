package obs

import (
	"testing"
	"time"
)

func TestLifecycleDiff_OpenThenClose(t *testing.T) {
	tr := NewLifecycleTracker()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// 第一轮：两条新连接 → 两个 open。
	ev := tr.Diff([]Conn{
		{ID: "c1", User: "u1", Start: t0},
		{ID: "c2", User: "u2", Start: t0},
	}, t0)
	if len(ev) != 2 {
		t.Fatalf("expected 2 open events, got %d", len(ev))
	}
	for _, e := range ev {
		if e.Event != "open" {
			t.Errorf("expected open, got %s", e.Event)
		}
	}

	// 第二轮：c2 消失 → 一个 close，带时长/字节。
	t1 := t0.Add(30 * time.Minute)
	ev = tr.Diff([]Conn{
		{ID: "c1", User: "u1", Start: t0},
	}, t1)
	if len(ev) != 1 {
		t.Fatalf("expected 1 close event, got %d", len(ev))
	}
	if ev[0].Event != "close" || ev[0].ConnID != "c2" {
		t.Errorf("expected close c2, got %s %s", ev[0].Event, ev[0].ConnID)
	}
	if ev[0].DurationSec != 1800 {
		t.Errorf("expected duration 1800s, got %d", ev[0].DurationSec)
	}
}

func TestBehaviorAggregator_ConnRateAndDedup(t *testing.T) {
	// 窗口 300s，阈值 conn_rate=240/min。
	agg := NewBehaviorAggregator(300, AbuseThresholds{ConnRatePerMin: 240}, nil)

	// 同一连接在多个 tick 出现，只能算 1 个新连接（按 conn_id 去重）。
	for i := 0; i < 5; i++ {
		agg.Observe([]Conn{{ID: "c1", User: "u1", Destination: "a.com:443"}})
	}
	out := agg.Build()
	if len(out.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(out.Users))
	}
	u := out.Users[0]
	// 1 个新连接 / 300s * 60 = 0/min（整数除法）→ 不触发。
	if u.ConnRatePerMin != 0 {
		t.Errorf("expected conn_rate 0, got %d", u.ConnRatePerMin)
	}
	if u.SuspectedAbuse {
		t.Errorf("should not be suspected with 1 conn")
	}
}

func TestBehaviorAggregator_SuspectedAbuseAttachesSamples(t *testing.T) {
	agg := NewBehaviorAggregator(60, AbuseThresholds{DistinctDestHosts: 2}, []string{"evil.com"})

	agg.Observe([]Conn{
		{ID: "c1", User: "u1", Destination: "a.com:443"},
		{ID: "c2", User: "u1", Destination: "b.com:443"},
		{ID: "c3", User: "u1", Destination: "evil.com:80"},
	})
	out := agg.Build()
	if len(out.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(out.Users))
	}
	u := out.Users[0]
	if u.DistinctDestHosts != 3 {
		t.Errorf("expected 3 distinct hosts, got %d", u.DistinctDestHosts)
	}
	if u.BlacklistHits != 1 {
		t.Errorf("expected 1 blacklist hit, got %d", u.BlacklistHits)
	}
	if !u.SuspectedAbuse {
		t.Errorf("expected suspected_abuse=true (3 >= threshold 2)")
	}
	if len(u.Samples) == 0 {
		t.Errorf("expected samples attached when suspected")
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"a.com:443", "a.com", 443},
		{"1.2.3.4:80", "1.2.3.4", 80},
		{"noport.com", "noport.com", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		h, p := splitHostPort(c.in)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("splitHostPort(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.wantHost, c.wantPort)
		}
	}
}
