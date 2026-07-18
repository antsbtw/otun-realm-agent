package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/antsbtw/otun-s-egress/egress"
)

// egressConfig aliases egress.Config so test helpers read cleanly.
type egressConfig = egress.Config

func TestBuildUsers_UUIDAsPassword(t *testing.T) {
	users := []User{{UUID: "uuid-1", Enabled: true}}
	got := BuildUsers(users)
	if len(got) != 1 {
		t.Fatalf("want 1 user, got %d", len(got))
	}
	// ★一 UUID 贯穿六协议：UUID 既作 UUID 又作 Password（hy2/trojan/tuic 密码 = UUID）。
	if got[0].UUID != "uuid-1" || got[0].Password != "uuid-1" {
		t.Errorf("UUID 应同时作 UUID 和 Password; got %+v", got[0])
	}
}

func TestBuildUsers_DisabledUserExcluded(t *testing.T) {
	users := []User{
		{UUID: "on", Enabled: true},
		{UUID: "off", Enabled: false},
	}
	got := BuildUsers(users)
	if len(got) != 1 || got[0].UUID != "on" {
		t.Errorf("禁用用户不应出现; got %+v", got)
	}
}

func TestBuildUsers_SSPasswordKeepsUUIDPassthrough(t *testing.T) {
	// ★egress.User 单 Password 字段限制下，仍以 UUID 贯穿全协议（ss_password 占位不破坏其余协议）。
	users := []User{{UUID: "uuid-1", Enabled: true, SSPassword: "ss-secret"}}
	got := BuildUsers(users)
	if len(got) != 1 || got[0].Password != "uuid-1" {
		t.Errorf("即便下发 ss_password，Password 仍应为 UUID（贯穿）; got %+v", got)
	}
}

func TestBuildConfigs_NilRealmNotReady(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	if cfgs := g.BuildConfigs(nil, ""); len(cfgs) != 0 {
		t.Errorf("realm 为 nil 时 BuildConfigs 应返回空（尚不能起 node）; got %d", len(cfgs))
	}
}

func TestBuildConfigs_SixProtocols(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{
		RealmID:              "iptv-cn-sh",
		ServerURL:            "http://rendezvous:9443",
		Token:                "tok",
		StunServers:          []string{"74.125.250.129:19302"},
		Protocols:            []string{"hysteria2", "tuic", "reality", "trojan", "shadowsocks", "vmess"},
		ObfsPassword:         "obfs-secret",
		SSMethod:             "aes-256-gcm",
		CongestionControl:    "bbr",
		RealityPrivateKey:    "priv",
		RealityShortID:       "0123456789abcdef",
		RealityHandshake:     "1.2.3.4",
		RealityHandshakePort: 443,
	}
	cfgs := g.BuildConfigs(realm, "seed-uuid")
	if len(cfgs) != 6 {
		t.Fatalf("want 6 configs, got %d", len(cfgs))
	}

	byProto := Config2(cfgs)
	// realm_id 派生 <base>-<proto>（对齐客户端范本短名）。
	assertRealmID(t, byProto, "hysteria2", "iptv-cn-sh-hy2")
	assertRealmID(t, byProto, "tuic", "iptv-cn-sh-tuic")
	assertRealmID(t, byProto, "reality", "iptv-cn-sh-reality")
	assertRealmID(t, byProto, "trojan", "iptv-cn-sh-trojan")
	assertRealmID(t, byProto, "shadowsocks", "iptv-cn-sh-ss")
	assertRealmID(t, byProto, "vmess", "iptv-cn-sh-vmess")

	// 公共字段透传。
	for _, c := range cfgs {
		if c.ServerURL != realm.ServerURL || c.Token != realm.Token {
			t.Errorf("%s: rendezvous 未透传; got %+v", c.Protocol, c)
		}
		if c.SNI != "iptv.local" {
			t.Errorf("%s: 默认 SNI 应 iptv.local; got %q", c.Protocol, c.SNI)
		}
	}

	// 协议级凭证。
	if byProto["hysteria2"].ObfsPassword != "obfs-secret" {
		t.Errorf("hy2 obfs 未透传")
	}
	if byProto["tuic"].CongestionControl != "bbr" {
		t.Errorf("tuic cc 未透传")
	}
	if byProto["shadowsocks"].Method != "aes-256-gcm" {
		t.Errorf("ss method 未透传; got %q", byProto["shadowsocks"].Method)
	}
	r := byProto["reality"]
	if r.PrivateKey != "priv" || r.ShortID != "0123456789abcdef" || r.HandshakeServer != "1.2.3.4" || r.HandshakePort != 443 {
		t.Errorf("reality 参数未透传; got %+v", r)
	}
	if r.ServerName != "www.apple.com" {
		t.Errorf("reality server_name 空时应默认 www.apple.com; got %q", r.ServerName)
	}
}

// resolveHandshakeIP 是 reality 借壳目标兜底的核心：纯 IPv4 原样，空/域名时本地解析，
// 解析不出 IPv4 返回空。纯逻辑用例（不依赖网络）覆盖 IP 透传与「无 SNI 可解析」跳过。
func TestResolveHandshakeIP_PureLogic(t *testing.T) {
	cases := []struct {
		name      string
		handshake string
		serverName string
		want      string
	}{
		{"纯IPv4原样透传", "1.2.3.4", "www.apple.com", "1.2.3.4"},
		{"IPv6不算IPv4_无SNI跳过", "2606:2800:220:1:248:1893:25c8:1946", "", ""},
		{"空handshake且空SNI返回空", "", "", ""},
		{"域名handshake且空SNI返回空", "www.apple.com", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveHandshakeIP(c.handshake, c.serverName); got != c.want {
				t.Errorf("resolveHandshakeIP(%q,%q)=%q want %q", c.handshake, c.serverName, got, c.want)
			}
		})
	}
}

// 借壳目标是纯 IP 时 reality 被正常构建（6 协议齐全）；handshake 为空且 SNI 无法本地
// 解析出 IPv4 时 reality 被跳过（其余五协议照常），绝不 panic 整进程。
func TestBuildConfigs_RealitySkippedWhenNoUsableHandshakeIP(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{
		RealmID:     "iptv-cn-sh",
		ServerURL:   "http://rendezvous:9443",
		Token:       "tok",
		StunServers: []string{"74.125.250.129:19302"},
		Protocols:   []string{"hysteria2", "reality", "trojan"},
		// RealityHandshake 空 + RealityServerName 空 → generator 内默认 SNI=www.apple.com，
		// 但为避免测试依赖 DNS，这里把 SNI 设成一个必然解析失败的保留域名，逼出「跳过」路径。
		RealityServerName:    "invalid.reality.handshake.test.",
		RealityPrivateKey:    "priv",
		RealityShortID:       "0123456789abcdef",
		RealityHandshake:     "",
		RealityHandshakePort: 443,
	}
	cfgs := g.BuildConfigs(realm, "seed-uuid")
	byProto := Config2(cfgs)
	if _, ok := byProto["reality"]; ok {
		t.Errorf("借壳目标解析失败时 reality 应被跳过; got cfgs=%+v", cfgs)
	}
	if _, ok := byProto["hysteria2"]; !ok {
		t.Errorf("reality 跳过不应影响 hysteria2")
	}
	if _, ok := byProto["trojan"]; !ok {
		t.Errorf("reality 跳过不应影响 trojan")
	}
}

func TestBuildConfigs_EmptyProtocolsFallsBackHy2(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t"} // 无 protocols
	cfgs := g.BuildConfigs(realm, "seed-uuid")
	if len(cfgs) != 1 || cfgs[0].Protocol != "hysteria2" {
		t.Errorf("无 protocols 应退回单 hy2; got %+v", cfgs)
	}
}

func TestBuildConfigs_SSDefaultMethod(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t", Protocols: []string{"shadowsocks"}}
	cfgs := g.BuildConfigs(realm, "seed-uuid")
	if len(cfgs) != 1 || cfgs[0].Method != "aes-128-gcm" {
		t.Errorf("ss method 空时应默认 aes-128-gcm; got %+v", cfgs)
	}
}

func TestBuildConfigs_CertInjectionHy2Tuic(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "iptv.local.crt")
	keyPath := filepath.Join(dir, "iptv.local.key")
	if err := os.WriteFile(certPath, []byte("CERT-PEM"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY-PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	g := NewRealmGenerator(certPath, keyPath)
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t", Protocols: []string{"hysteria2", "tuic", "reality"}}
	byProto := Config2(g.BuildConfigs(realm, "seed-uuid"))
	// hy2/tuic 外层 TLS 用稳定证书。
	if byProto["hysteria2"].CertPEM != "CERT-PEM" || byProto["hysteria2"].KeyPEM != "KEY-PEM" {
		t.Errorf("hy2 落盘证书 PEM 应注入; got %+v", byProto["hysteria2"])
	}
	if byProto["tuic"].CertPEM != "CERT-PEM" {
		t.Errorf("tuic 落盘证书 PEM 应注入")
	}
	// reality 用洞内 WrapTLS 自签，不注入外层证书。
	if byProto["reality"].CertPEM != "" {
		t.Errorf("reality 不应注入外层稳定证书（洞内 WrapTLS 自签）")
	}
}

func TestLocalPort(t *testing.T) {
	want := map[string]int{
		"hysteria2": 51820, "tuic": 51821, "reality": 51822,
		"trojan": 51823, "shadowsocks": 51824, "vmess": 51825, "bogus": 0,
	}
	for proto, port := range want {
		if got := LocalPort(proto); got != port {
			t.Errorf("LocalPort(%q) = %d, want %d", proto, got, port)
		}
	}
}

func TestDeriveRealmID(t *testing.T) {
	cases := map[string]string{
		"hysteria2": "base-hy2", "shadowsocks": "base-ss",
		"tuic": "base-tuic", "reality": "base-reality",
		"trojan": "base-trojan", "vmess": "base-vmess",
	}
	for proto, want := range cases {
		if got := DeriveRealmID("base", proto); got != want {
			t.Errorf("DeriveRealmID(base, %q) = %q, want %q", proto, got, want)
		}
	}
}

// Config2 indexes egress configs by protocol for assertions.
func Config2(cfgs []egressConfig) map[string]egressConfig {
	m := make(map[string]egressConfig, len(cfgs))
	for _, c := range cfgs {
		m[c.Protocol] = c
	}
	return m
}

func assertRealmID(t *testing.T, byProto map[string]egressConfig, proto, want string) {
	t.Helper()
	if got := byProto[proto].RealmID; got != want {
		t.Errorf("%s realm_id = %q, want %q", proto, got, want)
	}
}

func TestBuildConfigs_SeedCredentials(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t",
		Protocols: []string{"hysteria2", "tuic", "reality", "trojan", "shadowsocks", "vmess"}}
	byProto := Config2(g.BuildConfigs(realm, "seed-uuid-123"))
	// tuic/reality/vmess need a Config.UUID seed (egress.New requires non-empty UUID).
	for _, proto := range []string{"tuic", "reality", "vmess"} {
		if byProto[proto].UUID != "seed-uuid-123" {
			t.Errorf("%s Config.UUID should carry seed; got %q", proto, byProto[proto].UUID)
		}
	}
	// hy2/tuic/trojan/ss need a Config.Password seed (egress.New seeds a user).
	for _, proto := range []string{"hysteria2", "tuic", "trojan", "shadowsocks"} {
		if byProto[proto].Password != "seed-uuid-123" {
			t.Errorf("%s Config.Password should carry seed; got %q", proto, byProto[proto].Password)
		}
	}
	// hy2 doesn't need a Config.UUID seed.
	if byProto["hysteria2"].UUID != "" {
		t.Errorf("hy2 should not need a seed UUID; got %q", byProto["hysteria2"].UUID)
	}
}

func TestBuildConfigs_EmptySeedFallsBackPlaceholder(t *testing.T) {
	g := NewRealmGenerator("/nope.crt", "/nope.key")
	realm := &RealmBlock{RealmID: "r", ServerURL: "u", Token: "t", Protocols: []string{"tuic"}}
	byProto := Config2(g.BuildConfigs(realm, ""))
	if byProto["tuic"].UUID != placeholderUUID {
		t.Errorf("empty seed should fall back to placeholder UUID; got %q", byProto["tuic"].UUID)
	}
}

func TestSeedUUID(t *testing.T) {
	users := []User{{UUID: "off", Enabled: false}, {UUID: "on-1", Enabled: true}, {UUID: "on-2", Enabled: true}}
	if got := SeedUUID(users); got != "on-1" {
		t.Errorf("SeedUUID should return first enabled uuid; got %q", got)
	}
	if got := SeedUUID([]User{{UUID: "x", Enabled: false}}); got != "" {
		t.Errorf("no enabled users → empty seed; got %q", got)
	}
}
