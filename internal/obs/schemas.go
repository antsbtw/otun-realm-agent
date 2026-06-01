// Package obs 实现向【运营平面】上报的第二条通路（§6 第 2–6 类 / §7.3–§7.6）。
//
// 两条通路必须分清（§7.3.1）：
//   - 计费通路（internal/stats）→ otun-manager /api/node/stats，为扣额/踢人。
//   - 运营平面通路（本包）→ 私网采集端点，为分析/风控。
//
// 端点/协议依赖运营平面正式设计（§7.3.2，待定）。本包采用【最小可替换形态】：
// HTTP POST JSON 到 OBS_ENDPOINT，批量、可缓存重传。运营平面定了别的协议，只换
// Reporter 实现，采集点与 schema 不变。
package obs

import "time"

// schema 名常量。
const (
	SchemaConnLifecycle  = "conn_lifecycle"  // 第 2 类
	SchemaTrafficProfile = "traffic_profile" // 第 3 类
	SchemaEgressBehavior = "egress_behavior" // 第 4 类（风控）
	SchemaRealmHealth    = "realm_health"    // 第 5 类（realm 独有）
	SchemaEgressQuality  = "egress_quality"  // 第 6 类
)

// Envelope 公共信封（§7.3.3）。所有运营平面上报共用，便于按出口/时间归并。
type Envelope struct {
	NodeID  string `json:"node_id"`
	RealmID string `json:"realm_id"`
	Region  string `json:"region"`
	TS      string `json:"ts"`     // RFC3339
	Schema  string `json:"schema"` // 下列各类 schema 名
	Payload any    `json:"payload"`
}

// —— 第 2 类：连接生命周期（§7.3.4）——

// ConnLifecyclePayload schema=conn_lifecycle。
type ConnLifecyclePayload struct {
	Events []ConnEvent `json:"events"`
}

// ConnEvent 单个上下线事件。
type ConnEvent struct {
	UserUUID    string `json:"user_uuid"`
	Event       string `json:"event"` // "open" | "close"
	ConnID      string `json:"conn_id"`
	At          string `json:"at"`                     // RFC3339
	DurationSec int64  `json:"duration_sec,omitempty"` // close 才有
	Upload      int64  `json:"upload,omitempty"`
	Download    int64  `json:"download,omitempty"`
}

// —— 第 3 类：流量画像（§7.3.4）——

// TrafficProfilePayload schema=traffic_profile。
// 数据源 = 与计费同源的 V2Ray stats，但用 Reset_:false 快照（§7.3.4 warning）。
type TrafficProfilePayload struct {
	WindowSec int                  `json:"window_sec"`
	Users     []TrafficProfileUser `json:"users"`
}

// TrafficProfileUser 单用户用量快照。
type TrafficProfileUser struct {
	UserUUID string `json:"user_uuid"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}

// —— 第 4 类：出口行为特征（§7.4，风控）——

// EgressBehaviorPayload schema=egress_behavior。聚合特征，不采内容（§7.4.2 隐私边界）。
type EgressBehaviorPayload struct {
	WindowSec int                  `json:"window_sec"`
	Users     []EgressBehaviorUser `json:"users"`
}

// EgressBehaviorUser 单用户出口行为特征。
type EgressBehaviorUser struct {
	UserUUID          string             `json:"user_uuid"`
	DistinctDestHosts int                `json:"distinct_dest_hosts"` // 爬虫特征：异常高
	ConnRatePerMin    int                `json:"conn_rate_per_min"`   // 新建连接频率
	TopPorts          []int              `json:"top_ports"`           // 端口分布
	BlacklistHits     int                `json:"blacklist_hits"`      // 命中已知滥用名单次数
	SuspectedAbuse    bool               `json:"suspected_abuse"`     // agent 侧初判
	Samples           []EgressDestSample `json:"samples,omitempty"`   // 仅 suspected_abuse 时附，短 TTL
}

// EgressDestSample 疑似滥用时附的最近样本（host:port 计数，不含 path/内容）。
type EgressDestSample struct {
	DestHost string `json:"dest_host"`
	Port     int    `json:"port"`
	Count    int    `json:"count"`
}

// —— 第 5 类：打洞质量（§7.5，realm 独有）——

// RealmHealthPayload schema=realm_health。
type RealmHealthPayload struct {
	WindowSec      int                  `json:"window_sec"`
	L3Reachable    bool                 `json:"l3_reachable"`        // agent 自测，可靠
	L3ProbeRTTMs   int64                `json:"l3_probe_rtt_ms"`     // agent 自测，可靠
	ReloadCount    int                  `json:"reload_count_window"` // agent 自测，可靠
	ServerURL      string               `json:"server_url"`
	RegisterEvents []RealmRegisterEvent `json:"register_events,omitempty"` // ★依赖日志格式，待核实
}

// RealmRegisterEvent realm 注册/重注册事件（依赖 sing-box 日志解析，§7.5.1 待核实）。
type RealmRegisterEvent struct {
	Event          string `json:"event"` // "register_ok" | "register_fail"
	At             string `json:"at"`
	StunDiscoverMs int64  `json:"stun_discover_ms,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// —— 第 6 类：出口质量探测（§7.3.5）——

// EgressQualityPayload schema=egress_quality。
type EgressQualityPayload struct {
	Probes   []EgressProbe `json:"probes"`
	PublicIP string        `json:"public_ip,omitempty"` // §5.1：不用于连接，仅可观测 + GeoIP region 判定
}

// EgressProbe 单个目的地探测结果。
type EgressProbe struct {
	Target string `json:"target"`
	Type   string `json:"type"` // "icmp" | "tcp"
	RTTMs  int64  `json:"rtt_ms"`
	OK     bool   `json:"ok"`
}

// nowRFC3339 是注入式的"当前时间"，便于上层统一时区为 UTC。
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
