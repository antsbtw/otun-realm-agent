package main

import (
	"strings"
	"testing"
	"time"

	"github.com/antsbtw/otun-s-egress/egress"
)

// 10-28:hy2 同一用户的流共用一条 QUIC 连接(source 相同),并发连同一 host:port 时
// 旧 id 前四段完全相同,conn_quality 字节被反复计入。开始时间不同 → id 必须不同;
// 下游脚本按 split_part(conn_id,'|',3/4) 取来源/目标,这两段位置不能变。
func TestConnIDUniquePerConnAndKeepsFields(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	a := egress.ConnInfo{UUID: "u1", Protocol: "hysteria2", Source: "1.2.3.4:5555", Destination: "cdn.x.com:443", Start: t0}
	b := a
	b.Start = t0.Add(3 * time.Millisecond)
	if connID(a) == connID(b) {
		t.Fatalf("concurrent conns to same destination must not share id: %s", connID(a))
	}
	if connID(a) != connID(a) {
		t.Fatal("id must be stable for the same conn")
	}
	f := strings.Split(connID(a), "|")
	if len(f) != 5 || f[0] != "hysteria2" || f[1] != "u1" || f[2] != "1.2.3.4:5555" || f[3] != "cdn.x.com:443" {
		t.Fatalf("fields: %q", f)
	}
	if got := strings.Split(connID(egress.ConnInfo{UUID: "u1"}), "|")[4]; got != "0" {
		t.Fatalf("zero start → %q, want 0", got)
	}
}
