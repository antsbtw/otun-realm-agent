package obs

import (
	"net"
	"sort"
	"time"
)

// Conn 是采集器消费的连接快照（由上层从 clash_api /connections 整形传入，避免反向依赖 singbox 包）。
type Conn struct {
	ID          string
	User        string
	Destination string // host:port（元数据，§7.4.1）
	Upload      int64
	Download    int64
	Start       time.Time
}

// —— 第 2 类：连接生命周期 diff（§7.3.4）——

// LifecycleTracker 维护"上次见到的连接集合"，diff 出新建/结束事件。
type LifecycleTracker struct {
	prev map[string]Conn // conn_id → Conn
}

// NewLifecycleTracker 创建生命周期追踪器。
func NewLifecycleTracker() *LifecycleTracker {
	return &LifecycleTracker{prev: make(map[string]Conn)}
}

// Diff 对比当前连接集与上次，产出 open/close 事件。now 由上层注入（统一 UTC）。
func (t *LifecycleTracker) Diff(current []Conn, now time.Time) []ConnEvent {
	cur := make(map[string]Conn, len(current))
	for _, c := range current {
		cur[c.ID] = c
	}

	var events []ConnEvent

	// 新出现 = open。
	for id, c := range cur {
		if _, seen := t.prev[id]; !seen {
			events = append(events, ConnEvent{
				UserUUID: c.User,
				Event:    "open",
				ConnID:   id,
				At:       now.Format(time.RFC3339),
			})
		}
	}

	// 消失 = close（带时长/字节）。
	for id, c := range t.prev {
		if _, still := cur[id]; !still {
			dur := int64(0)
			if !c.Start.IsZero() {
				dur = int64(now.Sub(c.Start).Seconds())
			}
			events = append(events, ConnEvent{
				UserUUID:    c.User,
				Event:       "close",
				ConnID:      id,
				At:          now.Format(time.RFC3339),
				DurationSec: dur,
				Upload:      c.Upload,
				Download:    c.Download,
			})
		}
	}

	t.prev = cur
	return events
}

// —— 第 4 类：出口行为特征聚合（§7.4.3，风控）——

// AbuseThresholds 风控阈值（§7.4.4：上线初期"只观测不处置"，baseline 后再定）。
// 零值表示该项不触发。
type AbuseThresholds struct {
	DistinctDestHosts int // 5min 内不同目的 host 数上限
	ConnRatePerMin    int // 新建连接频率上限
}

// BehaviorAggregator 按窗口聚合每用户的出口行为特征。
// 只采元数据（目的地 host/port、字节、频率），【不解包 TLS、不记 path/内容】（§7.4.1/§7.4.2）。
type BehaviorAggregator struct {
	windowSec  int
	thresholds AbuseThresholds
	blacklist  map[string]bool // 已知滥用/爬虫目标 host（下发或本地配置）

	// 窗口内累计（按用户）。
	users map[string]*behaviorAcc
}

type behaviorAcc struct {
	destHosts map[string]int // host → 次数
	ports     map[int]int    // port → 次数
	newConns  int            // 窗口内新建连接数
	blacklist int            // 命中黑名单次数
	seenConns map[string]bool
}

// NewBehaviorAggregator 创建行为聚合器。blacklist 可为 nil。
func NewBehaviorAggregator(windowSec int, thresholds AbuseThresholds, blacklist []string) *BehaviorAggregator {
	bl := make(map[string]bool, len(blacklist))
	for _, h := range blacklist {
		bl[h] = true
	}
	return &BehaviorAggregator{
		windowSec:  windowSec,
		thresholds: thresholds,
		blacklist:  bl,
		users:      make(map[string]*behaviorAcc),
	}
}

// Observe 把一批当前连接喂入窗口累计。应在每个采集 tick 调用。
// 用 conn_id 去重，避免同一长连接在多个 tick 被重复计为"新连接"。
func (a *BehaviorAggregator) Observe(conns []Conn) {
	for _, c := range conns {
		if c.User == "" {
			continue
		}
		acc, ok := a.users[c.User]
		if !ok {
			acc = &behaviorAcc{
				destHosts: make(map[string]int),
				ports:     make(map[int]int),
				seenConns: make(map[string]bool),
			}
			a.users[c.User] = acc
		}

		host, port := splitHostPort(c.Destination)
		if host != "" {
			acc.destHosts[host]++
			if a.blacklist[host] {
				acc.blacklist++
			}
		}
		if port > 0 {
			acc.ports[port]++
		}
		if c.ID != "" && !acc.seenConns[c.ID] {
			acc.seenConns[c.ID] = true
			acc.newConns++
		}
	}
}

// Build 产出窗口特征并重置累计。返回的 payload 可直接 Enqueue。
func (a *BehaviorAggregator) Build() EgressBehaviorPayload {
	out := EgressBehaviorPayload{WindowSec: a.windowSec}

	for uuid, acc := range a.users {
		connRate := 0
		if a.windowSec > 0 {
			connRate = acc.newConns * 60 / a.windowSec
		}

		suspected := false
		if a.thresholds.DistinctDestHosts > 0 && len(acc.destHosts) >= a.thresholds.DistinctDestHosts {
			suspected = true
		}
		if a.thresholds.ConnRatePerMin > 0 && connRate >= a.thresholds.ConnRatePerMin {
			suspected = true
		}

		u := EgressBehaviorUser{
			UserUUID:          uuid,
			DistinctDestHosts: len(acc.destHosts),
			ConnRatePerMin:    connRate,
			TopPorts:          topPorts(acc.ports, 3),
			BlacklistHits:     acc.blacklist,
			SuspectedAbuse:    suspected,
		}
		// 聚合优先：仅 suspected_abuse 时附最近样本（§7.4.2/§7.4.3）。
		if suspected {
			u.Samples = topDestSamples(acc.destHosts, 5)
		}
		out.Users = append(out.Users, u)
	}

	a.users = make(map[string]*behaviorAcc) // 重置窗口
	return out
}

// splitHostPort 拆 host:port，端口解析失败返回 0。
func splitHostPort(dest string) (string, int) {
	if dest == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return dest, 0 // 没有端口，整串当 host
	}
	port := 0
	for i := 0; i < len(portStr); i++ {
		ch := portStr[i]
		if ch < '0' || ch > '9' {
			return host, 0
		}
		port = port*10 + int(ch-'0')
	}
	return host, port
}

func topPorts(ports map[int]int, n int) []int {
	type kv struct {
		port  int
		count int
	}
	arr := make([]kv, 0, len(ports))
	for p, c := range ports {
		arr = append(arr, kv{p, c})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].count != arr[j].count {
			return arr[i].count > arr[j].count
		}
		return arr[i].port < arr[j].port
	})
	out := make([]int, 0, n)
	for i := 0; i < len(arr) && i < n; i++ {
		out = append(out, arr[i].port)
	}
	return out
}

func topDestSamples(hosts map[string]int, n int) []EgressDestSample {
	type kv struct {
		host  string
		count int
	}
	arr := make([]kv, 0, len(hosts))
	for h, c := range hosts {
		arr = append(arr, kv{h, c})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].count != arr[j].count {
			return arr[i].count > arr[j].count
		}
		return arr[i].host < arr[j].host
	})
	out := make([]EgressDestSample, 0, n)
	for i := 0; i < len(arr) && i < n; i++ {
		out = append(out, EgressDestSample{DestHost: arr[i].host, Count: arr[i].count})
	}
	return out
}
