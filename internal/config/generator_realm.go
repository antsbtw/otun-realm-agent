package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// 本地 sing-box API 地址（照搬 node-agent 约定）。
const (
	V2RayAPIAddr = "127.0.0.1:10085"
	ClashAPIAddr = "127.0.0.1:9090"
)

// RealmGenerator 生成 realm-agent 的 sing-box 配置（§7.1）。
// 照搬 node-agent MultiProtocolGenerator 的结构，但只生成【一个 hy2 inbound + 内嵌 realm{} 块】，
// 用自签 iptv.local（不走 tls-service）。realm{}/dns 参数全部来自 manager 下发（§7.2），不写死。
type RealmGenerator struct {
	hy2Port  int
	certPath string
	keyPath  string
}

// NewRealmGenerator 创建 realm 配置生成器。
func NewRealmGenerator(hy2Port int, certPath, keyPath string) *RealmGenerator {
	return &RealmGenerator{
		hy2Port:  hy2Port,
		certPath: certPath,
		keyPath:  keyPath,
	}
}

// Generate 生成 realm sing-box 配置。
// realm/dns 为 manager 下发块（可能为 nil，例如首次注册前——此时只生成可启动的最小配置）。
func (g *RealmGenerator) Generate(users []User, realm *RealmBlock, dns *DNSBlock) map[string]any {
	// per-user：UUID 作 hy2 密码（§7.1.1 / §7.8.2，锁定决策）。
	var hy2Users []map[string]any
	var statsUsers []string
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		hy2Users = append(hy2Users, map[string]any{
			"name":     u.UUID, // name 用于 V2Ray API 用户流量统计
			"password": u.UUID,
		})
		statsUsers = append(statsUsers, u.UUID)
	}
	if hy2Users == nil {
		hy2Users = []map[string]any{}
	}

	sni := "iptv.local"
	if realm != nil && realm.SNI != "" {
		sni = realm.SNI
	}

	inbound := map[string]any{
		"type":        "hysteria2",
		"tag":         "hy2-in",
		"listen":      "::",
		"listen_port": g.hy2Port, // 本地监听口；NAT 后对外端口由打洞动态决定（§7.9）
		"users":       hy2Users,
		"tls": map[string]any{
			"enabled":          true,
			"server_name":      sni,
			"alpn":             []string{"h3"},
			"certificate_path": g.certPath, // 自签，§7.1.3
			"key_path":         g.keyPath,
		},
	}

	if realm != nil {
		// obfs：省略下发值则整块不生成（坑：obfs 两端必须一致）。
		if realm.ObfsPassword != "" {
			inbound["obfs"] = map[string]any{
				"type":     "salamander",
				"password": realm.ObfsPassword,
			}
		}

		// ★★ realm 注册块（§7.1.2）。
		// ⚠️ 待核实：realm 字段精确键名/层级以 sing-box 上游源码为准，实现前需 sing-box check 核验。
		// 键名沿用客户端 PoC 已验证的语义。
		realmBlock := map[string]any{
			"server_url": realm.ServerURL,
			"realm_id":   realm.RealmID,
			"token":      realm.Token,
		}
		// 坑#2：stun_servers 必须 IP 不能域名；只列 IPv4 STUN，规避同 realm_id 双栈 → 409 realm_taken。
		if len(realm.StunServers) > 0 {
			realmBlock["stun_servers"] = filterIPv4Stun(realm.StunServers)
		}
		// 坑#3：realm 块内【不要】写 http_client.detour —— 这里本就不附加。
		inbound["realm"] = realmBlock
	}

	config := map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"dns":       buildDNS(dns),
		"inbounds":  []map[string]any{inbound},
		"outbounds": []map[string]any{{"type": "direct", "tag": "direct"}},
		"experimental": map[string]any{
			// per-user 计量（§7.6 复用 collector）。
			"v2ray_api": map[string]any{
				"listen": V2RayAPIAddr,
				"stats": map[string]any{
					"enabled":  true,
					"inbounds": []string{"hy2-in"},
					"users":    statsUsers,
				},
			},
			// KickUser + 连接级采集（§7.4/§7.6 复用 connections.go）。
			"clash_api": map[string]any{
				"external_controller": ClashAPIAddr,
			},
		},
	}

	return config
}

// buildDNS 渲染 DNS 段（§7.1.4）。agent 不判断国家、不硬编码 223.5.5.5——只渲染下发的 server 列表。
// 坑#1：DNS server 对象里【禁止出现 detour 键】；且对下发字段做白名单（只接受 address），防 manager 误配。
func buildDNS(dns *DNSBlock) map[string]any {
	// 未下发时给一个最小可启动的占位（注册前/离线兜底）；
	// 国家级 DNS 一旦下发即覆盖（§4.2）。
	servers := []string{"1.1.1.1"}
	if dns != nil && len(dns.Servers) > 0 {
		servers = dns.Servers
	}

	dnsServers := make([]map[string]any, 0, len(servers)+1)
	// 第一个作 proxy-dns，其余作 direct-dns；都【不写 detour】（坑#1）。
	dnsServers = append(dnsServers, map[string]any{
		"tag":     "proxy-dns",
		"address": servers[0],
	})
	directAddr := servers[0]
	if len(servers) > 1 {
		directAddr = servers[1]
	}
	dnsServers = append(dnsServers, map[string]any{
		"tag":     "direct-dns",
		"address": directAddr,
	})

	return map[string]any{
		"servers":  dnsServers,
		"final":    "proxy-dns",
		"strategy": "ipv4_only", // 配合单地址族，规避 409 realm_taken
	}
}

// filterIPv4Stun 仅保留 IPv4 STUN（坑#2 + 规避双栈 409 realm_taken）。
// 判定：含 '.' 且不含 '['（IPv6 字面量用 [::]:port 形式）。
func filterIPv4Stun(stun []string) []string {
	out := make([]string, 0, len(stun))
	for _, s := range stun {
		if len(s) == 0 {
			continue
		}
		// IPv4 STUN：含 '.' 且不含 '['（IPv6 字面量用 [::]:port 形式）。
		if strings.Contains(s, ".") && !strings.Contains(s, "[") {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		// 全被过滤掉则原样返回，避免空 stun 导致无法注册（让 sing-box check 暴露问题）。
		return stun
	}
	return out
}

// WriteToFile 将配置写入文件。
func (g *RealmGenerator) WriteToFile(config map[string]any, path string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}
