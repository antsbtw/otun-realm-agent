package main

import (
	"encoding/json"
	"testing"

	"github.com/antsbtw/otun-s-egress/egress"

	"otun-realm-agent/internal/config"
)

// TestDistinctUserCount 去重口径：容量水位=按 UUID 去重的在连用户数，
// 与连接总数（使用率）是两个维度。
func TestDistinctUserCount(t *testing.T) {
	cases := []struct {
		name  string
		conns []egress.ConnInfo
		want  int
	}{
		{"空快照", nil, 0},
		// 3 条连接 UUID=[A,A,B] → 2 个用户（同用户多条连接只算 1）。
		{"同用户多连接去重", []egress.ConnInfo{
			{UUID: "A", Protocol: "hysteria2"},
			{UUID: "A", Protocol: "hysteria2"},
			{UUID: "B", Protocol: "hysteria2"},
		}, 2},
		// 跨协议全局去重：hy2 的 A + reality 的 A → 1（共享 Registry 的 Snapshot
		// 已并全协议，同一用户跨协议只占 1 个名额，不是各协议 count 相加）。
		{"跨协议同用户去重", []egress.ConnInfo{
			{UUID: "A", Protocol: "hysteria2"},
			{UUID: "A", Protocol: "reality"},
		}, 1},
		// 六协议混合：A 占五协议 5 条 + B 一条 → 2 用户 6 连接。
		{"六协议混合", []egress.ConnInfo{
			{UUID: "A", Protocol: "hysteria2"},
			{UUID: "A", Protocol: "reality"},
			{UUID: "A", Protocol: "tuic"},
			{UUID: "A", Protocol: "trojan"},
			{UUID: "A", Protocol: "shadowsocks"},
			{UUID: "B", Protocol: "vmess"},
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctUserCount(tc.conns); got != tc.want {
				t.Fatalf("distinctUserCount=%d, want %d (conns=%d)", got, tc.want, len(tc.conns))
			}
		})
	}
}

// TestHeartbeatPayloadActiveUsers 心跳 payload 两维都在且互不覆盖：
// active_connections（使用率）与 active_users（容量水位）各自独立字段；
// user_count（服务名单数）保留兼容。
func TestHeartbeatPayloadActiveUsers(t *testing.T) {
	conns := []egress.ConnInfo{
		{UUID: "A", Protocol: "hysteria2"},
		{UUID: "A", Protocol: "reality"},
		{UUID: "B", Protocol: "hysteria2"},
	}
	load := config.NodeLoad{
		ActiveConnections: len(conns),
		ActiveUsers:       distinctUserCount(conns),
		UserCount:         8, // 服务名单数（含未连的），与在连数无关
	}
	raw, err := json.Marshal(load)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got["active_users"]; !ok || v.(float64) != 2 {
		t.Fatalf("payload active_users=%v, want 2 (A 跨协议去重+B)", got["active_users"])
	}
	if v := got["active_connections"].(float64); v != 3 {
		t.Fatalf("payload active_connections=%v, want 3", v)
	}
	if v := got["user_count"].(float64); v != 8 {
		t.Fatalf("payload user_count=%v, want 8（保留兼容不被挤掉）", v)
	}
}
