package config

import (
	"encoding/json"
	"time"
)

// AgentConfig 是 realm-agent 的运行配置。
// 相对 otun-node-agent：固定 remote 模式、六协议全上、无 server_ip。
// 参考 SIX_PROTOCOL_IMPLEMENTATION_DESIGN.md（六协议总设计）。
//
// 六协议改造后删掉的字段（不再从 env/参数取）：
//   - HY2Port / Protocol：六协议本地端口用 LocalPort 常量约定（不对外、不进 manager）；
//     启用哪些协议由 manager 下发的 RealmBlock.Protocols 决定，不再是单协议入口。
//   - RealmServerURL：会合面 URL 由 manager 下发（RealmBlock.ServerURL），不本地写死。
type AgentConfig struct {
	APIURL        string // otun-manager 地址（用户下发 FetchUsers + 计费 ReportConnections/stats）
	FleetURL      string // ★Batch 5：fleet-manager 地址（纳管 register/heartbeat）；空则回退 APIURL（零回归）
	NodeAPIKey    string // 出口身份 api-key（§5.1：不靠 IP，靠 node_id+api_key）
	NodeID        string // 出口 node_id，如 realm-cn-sh-01
	SyncInterval  time.Duration
	StatsInterval time.Duration
	RealmInterval time.Duration // §7.9 按需重注册循环周期

	// realm 首启引导值（install.sh 注入；动态调整后以 manager 下发为准，§7.2）
	RealmID string // realm slot 标识（base），如 iptv-cn-sh；各协议派生 <base>-<proto>
	Region  string // 出口 region 初值，空则后端 GeoIP 自动判定（§5.2）

	LogLevel string

	// 运营平面（§7.3：第二条上报通路，独立于计费）
	OBSEndpoint string // 运营平面采集端点（私网地址），空则不上报
}

// 六协议本地 UDP 端口约定常量（总设计 §1 表 + agent 施工单 §3.2/§7）。
// 这些是 agent 本地打洞用的监听口——对外无意义、不进 manager 数据模型、不从 env 传。
// 每协议一个口，因为每协议是一个独立 egress.Node、各开各的 UDP socket。
const (
	PortHysteria2   = 51820
	PortTUIC        = 51821
	PortReality     = 51822
	PortTrojan      = 51823
	PortShadowsocks = 51824
	PortVMess       = 51825
)

// LocalPort 返回某协议约定的本地 UDP 端口。未知协议返回 0（调用方据此跳过）。
func LocalPort(protocol string) int {
	switch protocol {
	case "hysteria2":
		return PortHysteria2
	case "tuic":
		return PortTUIC
	case "reality":
		return PortReality
	case "trojan":
		return PortTrojan
	case "shadowsocks":
		return PortShadowsocks
	case "vmess":
		return PortVMess
	default:
		return 0
	}
}

// RealmBlock 是 manager 下发的 realm{} 块参数（六协议扩展版，总设计 §3）。
// 这些是"必须动态下发"参数，不写死在 install.sh。
// ★json tag 与 manager models 必须逐字段对齐（manager 需求单 §2 M3）；
// 权威样例见 document/realm/reference/expected_node_users_downlink_sample.json。
type RealmBlock struct {
	// —— 公共（每协议派生一个 realm_id：见总设计 §4）——
	RealmID     string   `json:"realm_id"`     // 基 id，各协议派生 <base>-<proto>
	ServerURL   string   `json:"server_url"`   // 会合面 URL
	Token       string   `json:"token"`        // ★会合面 token（一 token 多 realm_id）
	StunServers []string `json:"stun_servers"` // 坑#2：必须 IP 不能域名
	// DirectAddresses 非空 → direct 模式：本节点是固定公网 IP（或 1:1 静态 NAT，
	// 如 AWS Elastic IP），跳过 STUN 反射与双向打洞对撞，直接把这些 "IP:port"
	// 上报给客户端，打洞仅被动应答。
	// 动因：客户端若在对称 NAT 后（实测中国移动蜂窝即是），双向对撞必败；
	// 而本节点地址固定可直连，客户端主动发包后其 NAT 会放行回程。
	// 空 = 走原打洞逻辑，其它节点零影响。
	DirectAddresses []string `json:"direct_addresses,omitempty"`
	// RelayAddresses 非空 → 启用中继回退（RELAY_FALLBACK_DESIGN.md §3.2）：
	// 收到会合面打洞事件时，本节点在打洞的同时向这些中继报到（报同一 nonce）；
	// 客户端打洞失败转中继时两条流对接，握手端到端跑通，出口仍是本节点住宅 IP。
	//
	// 与 DirectAddresses 的区别：那个要求本节点有固定公网 IP（机房 VPS 才成立），
	// 私宅节点没有固定入口，只能靠双方各自主动出站连中继。
	// 空 = 不启用，行为与改动前一致。
	RelayAddresses []string `json:"relay_addresses,omitempty"`
	SNI            string   `json:"sni"` // 默认 iptv.local
	// 会合面用自签证书（纯 IP 部署、无公网 CA）时置 true，出口跳过会合面 TLS 证书校验。
	// 仅控制通道，不影响打洞数据面隧道 TLS。透传到 egress.Config.RendezvousInsecureTLS。
	RendezvousInsecure bool `json:"rendezvous_insecure,omitempty"`

	// —— 启用哪些协议（★核心：manager 只管这个，不管端口）——
	Protocols []string `json:"protocols"` // 如 ["hysteria2","reality","tuic",...]

	// —— 协议级凭证（节点级，非 per-user）——
	ObfsPassword      string `json:"obfs_password"`                // hy2 salamander
	SSMethod          string `json:"ss_method"`                    // ss 加密方法（如 aes-128-gcm）
	CongestionControl string `json:"congestion_control,omitempty"` // tuic 可选

	// —— reality 节点级（★manager 生成，private 下发；总设计 §7）——
	RealityPrivateKey    string `json:"reality_private_key"`
	RealityShortID       string `json:"reality_short_id"`
	RealityServerName    string `json:"reality_server_name"`      // 借壳 SNI，默认 www.apple.com
	RealityHandshake     string `json:"reality_handshake_server"` // 借壳目标 host（★必须 IP）
	RealityHandshakePort uint16 `json:"reality_handshake_port"`   // 默认 443
}

// DNSBlock 是 manager 按出口国家下发的 DNS 段（§4.2 / §7.1.4）。
// agent 不判断国家、不硬编码 223.5.5.5——只渲染下发的 server 列表。
type DNSBlock struct {
	Country string   `json:"country"` // 国家级 region key（如 CN / US）
	Servers []string `json:"servers"` // proxy-dns / direct-dns 用的 DNS server IP 列表
}

// User 是从 manager 获取的 realm 用户（六协议扩展版，总设计 §3）。
// ★一 UUID 贯穿六协议：hy2 pw / tuic uuid / reality uuid / trojan pw / vmess uuid 都用它。
// 只有 shadowsocks 需要 per-user SSPassword（节点级 method 在 RealmBlock.SSMethod）。
type User struct {
	UUID         string     `json:"uuid"`
	Enabled      bool       `json:"enabled"`
	TrafficLimit int64      `json:"traffic_limit"`
	TrafficUsed  int64      `json:"traffic_used"`
	ExpireAt     *time.Time `json:"expire_at"`
	SSPassword   string     `json:"ss_password,omitempty"` // ★仅 ss 需要 per-user password
}

// UsersResponse 是 manager /api/node/users 对 realm 出口的扩展响应（§7.2.2）。
// version 哈希源已扩大为 users + realm + dns（manager 侧）。
//
// WP-2 双 version 契约（manager 已部署，字段名权威）：
//   - Version      = 现状合并哈希（users+realm+dns），向后兼容，老 agent 仍用。
//   - UserVersion  = 仅哈希 user 集（剔 traffic_used + 按 uuid 排序）。只它变 → 走热更（不断连）。
//   - RealmVersion = 仅哈希 realm 块 + dns 块（六协议后含 protocols + 各协议凭证）。它变 → 重建受影响协议。
//
// 老 manager 不返回这两个字段时它们为空串，据此降级回纯 version reload（见 syncAndApply）。
type UsersResponse struct {
	Version      string      `json:"version"`
	UserVersion  string      `json:"user_version"`
	RealmVersion string      `json:"realm_version"`
	Users        []User      `json:"users"`
	Realm        *RealmBlock `json:"realm"` // realm 出口才有
	DNS          *DNSBlock   `json:"dns"`   // 按出口国家
}

// HeartbeatRequest 心跳请求（计费通路，复用方言）。
type HeartbeatRequest struct {
	NodeID string `json:"node_id"`
	// Version 二进制 buildVersion（U0）。由 Syncer.Heartbeat 统一盖章，调用方不用填。
	// 老 fleet/otun 不认识该字段会忽略 → 零回归。
	Version   string    `json:"version,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Load      NodeLoad  `json:"load"`
	PublicIP  string    `json:"public_ip,omitempty"` // §5.1：仅可观测，不参与连接
	// RendezvousHealth 会合面注册健康自检快照（1c，诚实上报给 fleet 落库）。
	// nil = 尚无自检结果（realm 未激活/首个 tick 未到），fleet 侧不覆盖旧值。
	RendezvousHealth *RendezvousHealth `json:"rendezvous_health,omitempty"`
	// AppliedUserVersion ★P2 切换确认握手：数据面【真实应用成功】的 user 集版本
	// （= UsersResponse.UserVersion；应用失败不更新，节点全关时清空）。fleet 落
	// nodes.applied_user_version，manager 的 ready 判定以此为信任锚。老 fleet 不认识
	// 该字段会忽略 → 零回归；空串 omitempty 不上报（fleet 侧不覆盖旧值）。
	AppliedUserVersion string `json:"applied_user_version,omitempty"`
	// RelayStats ★A1b 兼职中继（RELAY_FLEET_BOUNDARY_DESIGN §5quater）：本机 otun-relay
	// /stats 快照原样捎带。nil = 未开兼职/中继未起，fleet 侧快照过期自然判不健康。
	// 老 fleet 不认识该字段会忽略 → 零回归。
	RelayStats json.RawMessage `json:"relay_stats,omitempty"`
}

// RendezvousSlotHealth 单个 <base>-<proto> slot 在会合面的注册状态（确凿探测结论）。
type RendezvousSlotHealth struct {
	RealmID    string `json:"realm_id"`
	Registered bool   `json:"registered"`
}

// RendezvousHealth 会合面注册健康（agent §7.9 自检的最近一次快照，随注册/心跳上报 fleet）。
// ★Slots 只含【确凿】结论（会合面 /status 返 200 的 slot）；未知（老会合面 404/网络错）
// 不进列表——上报的是核实过的事实，不是猜测。老会合面下 Slots 恒空，fleet 落库后据此
// 可分辨"没探到"与"探到已注册"。
type RendezvousHealth struct {
	ServerURL         string                 `json:"server_url"`
	Slots             []RendezvousSlotHealth `json:"slots"`
	LastRegisterError string                 `json:"last_register_error,omitempty"`
}

// NodeLoad 节点负载。
// ★容量指标两维分治（2026-07-16 拍板）：
//   - ActiveConnections = 使用率（连接快照总数，1 用户可开多条）——看节点忙不忙；
//   - ActiveUsers       = 容量水位（按 UUID 去重的在连用户数）——占多少用户名额，
//     fleet 侧 ConcurrencyRatio 的分子（分母 concurrency_cap 是用户量级）。
//   - UserCount         = 服务名单数（GetUserCount，≠在连），语义模糊仅保留兼容
//     （别删，避免破坏其他读者），不再驱动任何 ratio。
type NodeLoad struct {
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
	ActiveConnections int     `json:"active_connections"`
	ActiveUsers       int     `json:"active_users"`
	UserCount         int     `json:"user_count"`
}

// HeartbeatResponse 心跳响应。CertUpdate 对 realm-agent 忽略（§7.1.3，自签自持）。
type HeartbeatResponse struct {
	OK          bool     `json:"ok"`
	KickUsers   []string `json:"kick_users"`
	ReloadUsers bool     `json:"reload_users"`
	// Relay ★A1b 兼职中继期望态（fleet 下发；nil = 本节点未开兼职）。原样透传给
	// relaymgr 对账（形状契约在 relaymgr.Directive，这里 RawMessage 避免 config 包
	// 依赖 relaymgr）。老 fleet 不返此字段 → nil → 对账零动作。
	Relay json.RawMessage `json:"relay,omitempty"`
}

// Connection 活跃连接（计费上报用）。
type Connection struct {
	UserUUID    string    `json:"user_uuid"`
	ClientIP    string    `json:"client_ip"`
	ConnectedAt time.Time `json:"connected_at"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
}

// ConnectionsReport 连接上报（计费通路）。
type ConnectionsReport struct {
	NodeID      string       `json:"node_id"`
	Timestamp   time.Time    `json:"timestamp"`
	Connections []Connection `json:"connections"`
}
