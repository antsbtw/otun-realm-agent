package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func genJSON(t *testing.T, users []User, realm *RealmBlock, dns *DNSBlock) string {
	t.Helper()
	g := NewRealmGenerator(51820, "/c.crt", "/c.key")
	cfg := g.Generate(users, realm, dns)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func TestGenerator_NoDNSSection(t *testing.T) {
	// 对齐 PoC + 规避 sing-box 1.14 DNS 迁移：agent 不生成 dns 段（即便 manager 下发了 dns 块）。
	// 经官方 1.14.0-alpha.25 `check` 核实：含 realm、无 dns 段的配置返回 0。
	out := genJSON(t, nil, nil, &DNSBlock{Country: "CN", Servers: []string{"223.5.5.5"}})
	if strings.Contains(out, "\"dns\"") {
		t.Errorf("不应生成 dns 段（对齐 PoC，避免 1.14 旧 DNS 格式报错）; got: %s", out)
	}
	if strings.Contains(out, "223.5.5.5") {
		t.Errorf("下发的 dns servers 不应进入配置; got: %s", out)
	}
}

func TestGenerator_BillingExperimental(t *testing.T) {
	// 照搬 node-agent 计费方案：必须生成 v2ray_api（per-user 精确计量）+ clash_api（踢人/采集）。
	// 守护这一不变量：防止 v2ray_api 被误删（误删则与 node-agent 计费不一致）。
	out := genJSON(t, []User{{UUID: "u-1", Enabled: true}}, nil, nil)
	if !strings.Contains(out, "v2ray_api") {
		t.Errorf("应生成 v2ray_api（per-user 精确计费，对齐 node-agent）; got: %s", out)
	}
	if !strings.Contains(out, "clash_api") {
		t.Errorf("应生成 clash_api（踢人/连接采集）; got: %s", out)
	}
}

func TestGenerator_StunServersPassthrough(t *testing.T) {
	// 真机核实（PoC + 官方 sing-box 1.14.0-alpha.25 `check` 通过）：域名 STUN 可用。
	// 故 STUN 原样下发，不过滤——含域名与 IP 都应原样出现在配置中。
	realm := &RealmBlock{
		RealmID:     "iptv-cn-sh",
		ServerURL:   "https://situstechnologies.com/realm",
		Token:       "tok",
		StunServers: []string{"stun.l.google.com:19302", "74.125.250.129:19302"}, // 域名 + IP
	}
	out := genJSON(t, nil, realm, nil)
	if !strings.Contains(out, "stun.l.google.com:19302") {
		t.Errorf("域名 STUN 应原样下发（真机已验证可用）; got: %s", out)
	}
	if !strings.Contains(out, "74.125.250.129:19302") {
		t.Errorf("IP STUN 应保留")
	}
}

func TestGenerator_NoHTTPClientDetour_Pitfall3(t *testing.T) {
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t"}
	out := genJSON(t, nil, realm, nil)
	if strings.Contains(out, "http_client") {
		t.Errorf("realm 块内不得有 http_client (坑#3); got: %s", out)
	}
}

func TestGenerator_UUIDAsHy2Password(t *testing.T) {
	users := []User{{UUID: "uuid-1", Enabled: true}}
	out := genJSON(t, users, nil, nil)
	// per-user UUID 既作 name 又作 password。
	if strings.Count(out, "uuid-1") < 2 {
		t.Errorf("UUID 应同时作 name 和 password; got: %s", out)
	}
}

func TestGenerator_DisabledUserExcluded(t *testing.T) {
	users := []User{
		{UUID: "on", Enabled: true},
		{UUID: "off", Enabled: false},
	}
	out := genJSON(t, users, nil, nil)
	if strings.Contains(out, "off") {
		t.Errorf("禁用用户不应出现在配置中; got: %s", out)
	}
}
