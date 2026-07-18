package config

import (
	"log"
	"net"
	"os"

	"github.com/antsbtw/otun-s-egress/egress"
)

// resolveHandshakeIP 把 reality 借壳目标规整成一个纯 IPv4 字符串。
//
// 为什么必须是纯 IPv4：egress 库建 reality 服务时，若 HandshakeServer 是空串或
// 域名，sing-box 会走 domain-resolver 分支，而 egress 的裸 context 没有注册 DNS
// transport → NewServer 返回 error → egress buildRealityServer 内 panic(err) →
// 整个 agent 进程崩溃（reality 全网不可用的根因）。填纯 IPv4 绕开该分支。
//
// 入参 handshake 是 manager 下发的 reality_handshake_server（约定存 IP，但历史上
// 可能为空或误存域名）；serverName 是借壳 SNI（默认 www.apple.com）。
//   - handshake 已是纯 IPv4：原样用。
//   - 否则在节点本地把 serverName 解析成一个 IPv4（每个出口拿到自己就近的 CDN IP，
//     正是借壳目标应有的「本地可达」属性）。
//
// 返回空串表示无法得到可用 IPv4（解析失败）——调用方据此跳过 reality，避免 panic，
// 靠下次 sync 自动重试。
func resolveHandshakeIP(handshake, serverName string) string {
	if ip := net.ParseIP(handshake); ip != nil && ip.To4() != nil {
		return handshake
	}
	if serverName == "" {
		return ""
	}
	addrs, err := net.LookupIP(serverName)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if v4 := a.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// HY2InboundTag 保留为逻辑常量（日志/兼容），egress 库内部不需要 inbound_tag。
const HY2InboundTag = "hy2-in"

// placeholderUUID 是无启用用户时喂给 tuic/reality/vmess egress.New 的合法占位 UUID
// （nil UUID）。仅为让 New 不因空 UUID 失败；UpdateUsers(空集) 随后把它清掉。
const placeholderUUID = "00000000-0000-0000-0000-000000000000"

// SeedUUID 返回用户集里第一个启用用户的 UUID 作 seed（一 UUID 贯穿六协议）。
// 无启用用户返回空串，调用方（或 BuildConfigs）退回 placeholderUUID。
func SeedUUID(users []User) string {
	for _, u := range users {
		if u.Enabled {
			return u.UUID
		}
	}
	return ""
}

// RealmGenerator 把 manager 下发的 users + realm 块「翻译」成 egress 库入参
// （六协议：每启用协议一个 egress.Config + 全协议共享的 []egress.User）。
// 不再渲染 sing-box JSON、不落盘、不生成 v2ray_api/clash_api/hot_reload 段——这些随
// exec-fork 方案一并删除（scheme 甲）。
//
// 自签 iptv.local 证书仍由 SelfSigner 生成并持久化（跨重启稳定）；本生成器把 PEM
// 内容读出来注入 hy2/tuic 的 Config.CertPEM/KeyPEM（其余协议 WrapTLS 洞内自签）。
type RealmGenerator struct {
	certPath string
	keyPath  string
}

// NewRealmGenerator 创建 realm 入参生成器。
// 六协议后不再持单一 hy2Port——本地端口按协议用 config.LocalPort 常量派生。
func NewRealmGenerator(certPath, keyPath string) *RealmGenerator {
	return &RealmGenerator{
		certPath: certPath,
		keyPath:  keyPath,
	}
}

// BuildUsers 把 manager 下发的 users 映射成 egress.User（仅 Enabled），★一 UUID 贯穿六协议：
// egress.User 只有一个 Password 字段，被 hy2/tuic/trojan/ss 共用，各 node 的 UpdateUsers 按
// 各自协议只读它需要的字段（hy2 读 Password 作 hy2 密码、ss 读 Password 作 ss 密码、tuic/
// vmess/reality 读 UUID 作 wire uuid）。当前实现一律 Password=UUID、UUID=UUID，使一 UUID 同时
// 承载六协议凭证。
//
// SSPassword（下发契约字段）：egress.User 目前无独立 ss password 字段，无法在保持"一 UUID
// 贯穿"的同时给 ss 单独 password（会与其余协议的 UUID 冲突）。故当前保持贯穿、ss 也用 UUID；
// 该字段为契约占位，待 egress 增独立 ss 字段后启用。
func BuildUsers(users []User) []egress.User {
	out := make([]egress.User, 0, len(users))
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		out = append(out, egress.User{
			UUID:     u.UUID,
			Password: u.UUID, // 一 UUID 贯穿：既作 UUID 又作各协议 password
		})
	}
	return out
}

// BuildConfigs 把 manager 下发的 realm 块映射成六协议 egress.Config 集。
// 按 realm.Protocols 循环，每协议构造一个 Config：
//   - 派生 realm_id = <base>-<proto>（会合面一 token 多 realm_id，总设计 §4）
//   - 填该协议 §2 表的字段
//   - 本地端口用 config.LocalPort 常量（调用方 Start 时用它开 UDP）
//
// realm 为 nil（首次注册前）或无启用协议时返回空切片，调用方据此知道还不能起 node。
// ★不注入 Config.Meter——共享 Registry 的注入交给调用方（cmd/agent 用 NewShared 或
// 逐个注入同一 Registry，见施工单 §3.7）。
//
// ★seedUUID：tuic/reality/vmess 的 egress.New 在建服务时需要一个合法 UUID 作 seed user
// （egress node 的 New 内部会用它跑一次初始 UpdateUsers，随后被 agent 的全量 UpdateUsers
// 覆盖）。传入首个启用用户的 UUID（一 UUID 贯穿六协议）；无启用用户时用占位合法 UUID，
// New 不致失败，UpdateUsers(空集) 随后清空。此值只是 bootstrap，不决定计费/认证。
func (g *RealmGenerator) BuildConfigs(realm *RealmBlock, seedUUID string) []egress.Config {
	if realm == nil {
		return nil
	}
	if seedUUID == "" {
		seedUUID = placeholderUUID
	}
	protocols := realm.Protocols
	if len(protocols) == 0 {
		// 兼容：manager 尚未下发 protocols 时，退回单 hy2（旧行为），不致完全瘫痪。
		protocols = []string{"hysteria2"}
	}

	sni := "iptv.local"
	if realm.SNI != "" {
		sni = realm.SNI
	}
	certPEM, keyPEM, haveCert := g.readCertPEM()

	out := make([]egress.Config, 0, len(protocols))
	for _, proto := range protocols {
		cfg := egress.Config{
			Protocol:    proto,
			ServerURL:   realm.ServerURL,
			Token:       realm.Token,
			RealmID:     DeriveRealmID(realm.RealmID, proto), // <base>-<proto>
			STUNServers: realm.StunServers,                   // 域名/IP 原样透传（真机已验证域名可用）
			SNI:         sni,
			ALPN:        []string{"h3"},
			// ★接会合面注册日志：此前不设 → sing-quic 用 NOP logger → STUN/Register/
			// event-stream 全过程不可观测。注入后进 journal，便于排障（如开机 DNS 未就绪
			// 致 STUN 解析失败、注册未发起等）。
			Logger: realmLogger{},
			// 会合面自签证书（纯 IP 部署）时跳过 TLS 校验；仅控制通道，不碰数据面隧道 TLS。
			RendezvousInsecureTLS: realm.RendezvousInsecure,
		}

		switch proto {
		case "hysteria2":
			cfg.Password = seedUUID // seed 凭证（hy2 password；随后被全量 UpdateUsers 覆盖）
			// hy2 salamander obfs：两端必须一致，空则库内不启用。
			cfg.ObfsPassword = realm.ObfsPassword
			injectCert(&cfg, certPEM, keyPEM, haveCert) // 外层 TLS 稳定证书（B.3）
		case "tuic":
			cfg.UUID = seedUUID     // seed user（egress.New 需合法 UUID；随后被全量 UpdateUsers 覆盖）
			cfg.Password = seedUUID // tuic per-user password 也用 UUID（一 UUID 贯穿）
			cfg.CongestionControl = realm.CongestionControl
			injectCert(&cfg, certPEM, keyPEM, haveCert)
		case "reality":
			cfg.UUID = seedUUID // seed user（同上）
			cfg.PrivateKey = realm.RealityPrivateKey
			cfg.ShortID = realm.RealityShortID
			cfg.ServerName = realm.RealityServerName
			if cfg.ServerName == "" {
				cfg.ServerName = "www.apple.com" // 借壳 SNI 默认（总设计 §7）
			}
			// 借壳目标必须是纯 IPv4（否则 egress buildRealityServer panic，见
			// resolveHandshakeIP）。manager 约定存 IP，但历史上可能空/域名——在此
			// 本地兜底解析。解析不出可用 IPv4 则跳过 reality（不 panic 整进程），
			// 其余五协议照常起，靠下次 sync 重试。
			cfg.HandshakeServer = resolveHandshakeIP(realm.RealityHandshake, cfg.ServerName)
			if cfg.HandshakeServer == "" {
				log.Printf("[generator] skip reality: no usable IPv4 handshake target (raw=%q sni=%q)", realm.RealityHandshake, cfg.ServerName)
				continue
			}
			cfg.HandshakePort = realm.RealityHandshakePort
			// reality 用洞内 WrapTLS 自签 + Reality 握手，不用外层稳定证书。
		case "trojan":
			cfg.Password = seedUUID // seed 凭证（trojan password 必填；随后被全量 UpdateUsers 覆盖）
			// trojan：per-user Password 走 egress.User；WrapTLS 洞内自签。
		case "shadowsocks":
			cfg.Password = seedUUID // seed 凭证（ss seed user 需 password；随后被全量 UpdateUsers 覆盖）
			cfg.Method = realm.SSMethod
			if cfg.Method == "" {
				cfg.Method = "aes-128-gcm" // 节点级默认（总设计 §8）
			}
			// ss per-user password 走 egress.User.Password（=UUID，见 BuildUsers）。
		case "vmess":
			cfg.UUID = seedUUID // seed user（egress.New 要求非空 UUID；随后被全量 UpdateUsers 覆盖）
			// vmess：per-user UUID 走 egress.User；WrapTLS 洞内自签。
		default:
			log.Printf("[generator] skip unknown protocol %q (not one of hy2/tuic/reality/trojan/ss/vmess)", proto)
			continue
		}

		out = append(out, cfg)
	}
	return out
}

// DeriveRealmID 派生某协议的会合面 realm_id：<base>-<proto>（总设计 §4）。
// proto 用短名对齐客户端范本（reference/six_protocol_client_config_sample.json 里的
// cn-test-hy2 / cn-test-reality ...）。
func DeriveRealmID(base, protocol string) string {
	return base + "-" + protoShort(protocol)
}

// protoShort 把 egress 协议名映射成会合面 realm_id 后缀短名（对齐客户端范本）。
func protoShort(protocol string) string {
	switch protocol {
	case "hysteria2":
		return "hy2"
	case "shadowsocks":
		return "ss"
	default:
		return protocol // tuic / reality / trojan / vmess 原样
	}
}

// injectCert 注入稳定自签证书（B.3）到 hy2/tuic 外层 TLS。
// 读失败不致命——egress 会退化为每次 New 一张新自签证书（客户端 insecure TLS 不 pin）。
func injectCert(cfg *egress.Config, certPEM, keyPEM string, have bool) {
	if have {
		cfg.CertPEM = certPEM
		cfg.KeyPEM = keyPEM
	}
}

// readCertPEM 读取 SelfSigner 落盘的 cert/key PEM 内容。
func (g *RealmGenerator) readCertPEM() (cert, key string, ok bool) {
	c, err := os.ReadFile(g.certPath)
	if err != nil {
		log.Printf("[generator] read cert %s failed (%v), egress will self-sign per start", g.certPath, err)
		return "", "", false
	}
	k, err := os.ReadFile(g.keyPath)
	if err != nil {
		log.Printf("[generator] read key %s failed (%v), egress will self-sign per start", g.keyPath, err)
		return "", "", false
	}
	return string(c), string(k), true
}
