package obs

import (
	"testing"
	"time"
)

// ★2026-09-03 实缺陷：agent 的 reportConnectionsAndObserve 在 len(connections)==0 时
// 直接 return，导致 Diff 永远不会在「连接归零」那一轮被调用 ——
// 最后一批连接的 close 事件（唯一带 duration + 字节数的事件）永远发不出去，
// 「零字节长连接」判据也就永远拿不到数据。
//
// 本测试钉死 Diff 对空输入的契约：必须把 prev 里全部连接产出为 close。
func TestLifecycleTracker_EmptyInputDrainsAllAsClose(t *testing.T) {
	tr := NewLifecycleTracker()
	start := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

	// 第一轮：两条连接建立
	openEvents := tr.Diff([]Conn{
		{ID: "hy2|u1|1.2.3.4:1|8.8.8.8:443", User: "u1", Start: start},
		{ID: "reality|u2|5.6.7.8:2|1.1.1.1:443", User: "u2", Start: start, Upload: 10, Download: 20},
	}, start)
	if len(openEvents) != 2 {
		t.Fatalf("first round should emit 2 opens, got %d", len(openEvents))
	}

	// 第二轮：连接全部消失（这一轮 agent 曾经直接 return，close 就丢了）
	later := start.Add(120 * time.Second)
	closeEvents := tr.Diff(nil, later)

	if len(closeEvents) != 2 {
		t.Fatalf("empty input must drain prev as 2 closes, got %d", len(closeEvents))
	}
	for _, e := range closeEvents {
		if e.Event != "close" {
			t.Fatalf("want close, got %q", e.Event)
		}
		if e.DurationSec != 120 {
			t.Fatalf("close must carry duration 120, got %d", e.DurationSec)
		}
	}

	// 再来一轮空的：prev 已清空，不该再重复产出 close
	if again := tr.Diff(nil, later.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("closes must not be emitted twice, got %d", len(again))
	}
}

// 零字节长连接——本项目的核心判据，必须能从 close 事件里辨认出来。
func TestLifecycleTracker_ZeroByteLongConnectionIsVisible(t *testing.T) {
	tr := NewLifecycleTracker()
	start := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

	tr.Diff([]Conn{{ID: "c-dead", User: "u1", Start: start}}, start)                            // 零字节
	tr.prev["c-live"] = Conn{ID: "c-live", User: "u2", Start: start, Upload: 5, Download: 9000} // 有流量

	events := tr.Diff(nil, start.Add(300*time.Second))

	var dead, live int
	for _, e := range events {
		if e.Upload+e.Download == 0 && e.DurationSec > 60 {
			dead++
		} else {
			live++
		}
	}
	if dead != 1 || live != 1 {
		t.Fatalf("want exactly 1 dead + 1 live connection, got dead=%d live=%d", dead, live)
	}
}
