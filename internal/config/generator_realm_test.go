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

func TestGenerator_NoDetourInDNS_Pitfall1(t *testing.T) {
	out := genJSON(t, nil, nil, &DNSBlock{Country: "CN", Servers: []string{"223.5.5.5"}})
	if strings.Contains(out, "detour") {
		t.Errorf("DNS段不得出现 detour (坑#1); got: %s", out)
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

func TestGenerator_StrategyIPv4Only(t *testing.T) {
	out := genJSON(t, nil, nil, &DNSBlock{Servers: []string{"8.8.8.8", "1.1.1.1"}})
	if !strings.Contains(out, "ipv4_only") {
		t.Errorf("DNS strategy 应为 ipv4_only (规避 409 realm_taken)")
	}
}
