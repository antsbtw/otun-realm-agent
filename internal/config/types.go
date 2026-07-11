package config

import "time"

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
	SNI         string   `json:"sni"`          // 默认 iptv.local
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
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Load      NodeLoad  `json:"load"`
	PublicIP  string    `json:"public_ip,omitempty"` // §5.1：仅可观测，不参与连接
	// RendezvousHealth 会合面注册健康自检快照（1c，诚实上报给 fleet 落库）。
	// nil = 尚无自检结果（realm 未激活/首个 tick 未到），fleet 侧不覆盖旧值。
	RendezvousHealth *RendezvousHealth `json:"rendezvous_health,omitempty"`
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
type NodeLoad struct {
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
	ActiveConnections int     `json:"active_connections"`
	UserCount         int     `json:"user_count"`
}

// HeartbeatResponse 心跳响应。CertUpdate 对 realm-agent 忽略（§7.1.3，自签自持）。
type HeartbeatResponse struct {
	OK          bool     `json:"ok"`
	KickUsers   []string `json:"kick_users"`
	ReloadUsers bool     `json:"reload_users"`
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
