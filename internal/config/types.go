package config

import "time"

// AgentConfig 是 realm-agent 的运行配置。
// 相对 otun-node-agent：固定 remote 模式、单 hy2+realm inbound、无多协议、无 server_ip。
// 参考 OTUN_REALM_AGENT_DESIGN.md §7.7（install.sh 参数全集）。
type AgentConfig struct {
	APIURL        string // manager 地址
	NodeAPIKey    string // 出口身份 api-key（§5.1：不靠 IP，靠 node_id+api_key）
	NodeID        string // 出口 node_id，如 realm-cn-sh-01
	SyncInterval  time.Duration
	StatsInterval time.Duration
	RealmInterval time.Duration // §7.9 按需重注册循环周期

	// realm 首启引导值（install.sh 注入；动态调整后以 manager 下发为准，§7.2）
	RealmID        string // realm slot 标识，如 iptv-cn-sh
	RealmServerURL string // L3 会合服务 URL（首启引导）
	Region         string // 出口 region 初值，空则后端 GeoIP 自动判定（§5.2）
	HY2Port        int    // hy2 inbound 本地监听口，默认 51820

	SingboxBin    string
	SingboxConfig string
	LogLevel      string

	// 运营平面（§7.3：第二条上报通路，独立于计费）
	OBSEndpoint string // 运营平面采集端点（私网地址），空则不上报
}

// RealmBlock 是 manager 下发的 realm{} 块参数（§7.2.2）。
// 这些是 §4.1 锁定的"必须动态下发"参数，不写死在 install.sh。
type RealmBlock struct {
	RealmID      string   `json:"realm_id"`
	ServerURL    string   `json:"server_url"`
	Token        string   `json:"token"`         // ★出口级共享 token（per-出口，已拍板）
	StunServers  []string `json:"stun_servers"`  // 坑#2：必须 IP 不能域名
	ObfsPassword string   `json:"obfs_password"` // 可选 salamander
	SNI          string   `json:"sni"`           // 默认 iptv.local
}

// DNSBlock 是 manager 按出口国家下发的 DNS 段（§4.2 / §7.1.4）。
// agent 不判断国家、不硬编码 223.5.5.5——只渲染下发的 server 列表。
type DNSBlock struct {
	Country string   `json:"country"` // 国家级 region key（如 CN / US）
	Servers []string `json:"servers"` // proxy-dns / direct-dns 用的 DNS server IP 列表
}

// User 是从 manager 获取的 realm 用户。per-user 凭证只有 UUID（=hy2 密码，§7.8.2）。
type User struct {
	UUID         string     `json:"uuid"`
	Enabled      bool       `json:"enabled"`
	TrafficLimit int64      `json:"traffic_limit"`
	TrafficUsed  int64      `json:"traffic_used"`
	ExpireAt     *time.Time `json:"expire_at"`
}

// UsersResponse 是 manager /api/node/users 对 realm 出口的扩展响应（§7.2.2）。
// version 哈希源已扩大为 users + realm + dns（manager 侧）。
type UsersResponse struct {
	Version string      `json:"version"`
	Users   []User      `json:"users"`
	Realm   *RealmBlock `json:"realm"` // realm 出口才有
	DNS     *DNSBlock   `json:"dns"`   // 按出口国家
}

// HeartbeatRequest 心跳请求（计费通路，复用方言）。
type HeartbeatRequest struct {
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Load      NodeLoad  `json:"load"`
	PublicIP  string    `json:"public_ip,omitempty"` // §5.1：仅可观测，不参与连接
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
