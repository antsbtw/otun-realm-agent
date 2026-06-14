package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// 本地 sing-box API 地址（照搬 node-agent 约定）。
const (
	V2RayAPIAddr = "127.0.0.1:10085"
	ClashAPIAddr = "127.0.0.1:9090"
	// HotReloadAddr 是 WP-1 fork sing-box 暴露的本地热更端点监听地址（仅本机 agent 调）。
	// agent 在「仅用户集变」时 POST 此端点全量推 user 集，让运行中的 sing-box 无损换认证 +
	// 计费集，不 reload、不断连。inbound_tag 与 generator 生成的 hy2 inbound tag 一致（HY2InboundTag）。
	HotReloadAddr = "127.0.0.1:10086"
	// HY2InboundTag 是 realm hy2 inbound 的 tag，热更端点据此定位要更新的 inbound。
	HY2InboundTag = "hy2-in"
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
		"tag":         HY2InboundTag,
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
		// stun_servers 原样下发。
		// 注：设计 §4 曾标"坑#2 必须 IP 不能域名"，但真机核实（PoC + 官方 sing-box
		// 1.14.0-alpha.25 `check` 通过）证明【域名 STUN 可用】（如 stun.l.google.com:19302）。
		// 故不再过滤域名——以 manager 下发为准，由出口表 stun_servers 决定。
		if len(realm.StunServers) > 0 {
			realmBlock["stun_servers"] = realm.StunServers
		}
		// 坑#3：realm 块内【不要】写 http_client.detour —— 这里本就不附加。
		inbound["realm"] = realmBlock
	}

	// 不生成 dns 段：对齐 PoC 已验证配置（PoC 无 dns 段，realm 出口 direct 出站用系统 DNS
	// 即可），同时规避 sing-box 1.14 移除旧 DNS server 格式 + 要求 domain_resolver 的兼容问题。
	// manager 仍可下发 dns 块（数据层保留），但 agent 本期不渲染——经官方 1.14.0-alpha.25
	// `check` 核实：含 realm 块、无 dns 段的配置返回 0。
	// 不生成 dns 段：对齐 PoC 已验证配置（PoC 无 dns 段，realm 出口 direct 出站用系统 DNS
	// 即可），同时规避 sing-box 1.14 移除旧 DNS server 格式 + 要求 domain_resolver 的兼容问题。
	// manager 仍可下发 dns 块（数据层保留），但 agent 本期不渲染。
	_ = dns
	config := map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"inbounds":  []map[string]any{inbound},
		"outbounds": []map[string]any{{"type": "direct", "tag": "direct-out"}},
		"experimental": map[string]any{
			// per-user 精确计量（§7.6）。★照搬 otun-node-agent 的计费方案：v2ray_api gRPC
			// 拿 per-user 累计字节。这要求 sing-box 带 with_v2ray_api —— 由 realm-agent 自己的
			// build-singbox 工作流编译（1.14.0-alpha.25 源码 + with_v2ray_api/with_quic 等 tag，
			// realm 默认编入），install.sh 从 realm-agent release 拉。与已上线 node-agent 计费一致。
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
			// WP-3：热更端点（WP-1 fork sing-box 提供）。agent 在「仅用户集变」时 POST
			// http://HotReloadAddr/hotreload/users 推全量 user 集 → 无损换认证 + v2ray_api 计费集，
			// 不 reload、不断连。仅绑 127.0.0.1（本机 agent 调）。inbound_tag 指向上面的 hy2 inbound。
			// 官方上游 sing-box 无此块，会 check 失败 → 故 install/build 必须用 antsbtw/sing-box fork。
			"hot_reload": map[string]any{
				"listen":      HotReloadAddr,
				"inbound_tag": HY2InboundTag,
			},
		},
	}

	return config
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
