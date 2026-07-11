package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// 1c 契约：RendezvousHealth 的 JSON 字段名是对 fleet 的 wire 契约
// （fleet node_handler 按 rendezvous_health/server_url/slots/realm_id/registered/
// last_register_error 解），字段名漂移 = fleet 落库静默变空。
func TestRendezvousHealthWireShape(t *testing.T) {
	hb := HeartbeatRequest{
		NodeID: "egress-nj-01",
		RendezvousHealth: &RendezvousHealth{
			ServerURL: "https://54.255.172.86:9443",
			Slots: []RendezvousSlotHealth{
				{RealmID: "nj-01-hy2", Registered: true},
				{RealmID: "nj-01-tuic", Registered: false},
			},
			LastRegisterError: "resolve STUN server: lookup stun.l.google.com: connection refused",
		},
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"rendezvous_health"`, `"server_url"`, `"slots"`,
		`"realm_id":"nj-01-hy2"`, `"registered":true`,
		`"realm_id":"nj-01-tuic"`, `"registered":false`,
		`"last_register_error"`,
	} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("heartbeat JSON missing %s: %s", key, data)
		}
	}

	// nil 快照 → 字段整体省略（fleet 据此不覆盖旧值）。
	empty, _ := json.Marshal(HeartbeatRequest{NodeID: "n"})
	if strings.Contains(string(empty), "rendezvous_health") {
		t.Fatalf("nil snapshot must omit rendezvous_health entirely: %s", empty)
	}

	// 无错误时 last_register_error 省略（不上报空串噪音）。
	noErr, _ := json.Marshal(RendezvousHealth{ServerURL: "u", Slots: nil})
	if strings.Contains(string(noErr), "last_register_error") {
		t.Fatalf("empty error must be omitted: %s", noErr)
	}

	// RegisterRequest 同样携带（任务书契约：两个请求都扩展）。
	reg, _ := json.Marshal(RegisterRequest{NodeID: "n", RendezvousHealth: &RendezvousHealth{ServerURL: "u"}})
	if !strings.Contains(string(reg), `"rendezvous_health"`) {
		t.Fatalf("register JSON missing rendezvous_health: %s", reg)
	}
}

// realmLogger Warn+ 捕获最近一次错误文本（1c last_register_error 数据源；
// sing-quic 不向上层返回注册错误，logger 是唯一截获点）。
func TestLastRealmErrorCapture(t *testing.T) {
	l := realmLogger{}
	l.Warn("STUN re-discovery on reset: resolve STUN server: ", "connection refused")
	if got := LastRealmError(); !strings.Contains(got, "connection refused") {
		t.Fatalf("Warn not captured: %q", got)
	}
	// Error 覆盖 Warn（只留最近一条）。
	l.Error("register: no session ID")
	if got := LastRealmError(); got != "register: no session ID" {
		t.Fatalf("Error must overwrite: %q", got)
	}
	// Info/Debug 不覆盖（只捕获 Warn 及以上）。
	l.Info("re-registered with control, session: abc")
	if got := LastRealmError(); got != "register: no session ID" {
		t.Fatalf("Info must not overwrite: %q", got)
	}
}
