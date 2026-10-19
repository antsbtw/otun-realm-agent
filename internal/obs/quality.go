package obs

// quality.go —— 连接体验质量(conn_quality)采集:速度 / 延迟 / 丢包 / 卡顿。
// 设计稿:ops-collector/docs/CONN_EXPERIENCE_QUALITY_DESIGN.md §2–§3。
//
// 数据源都在 egress 库的共享 Registry 上,只读:
//   - Snapshot():每条连接的累计字节 + 最近读写时间 → 吞吐、忙窗口、卡顿;
//   - QUICStats():每条 QUIC 连接的 RTT / 发包 / 丢包 / 拥塞控制 → 用户↔节点段质量。
//
// 每 10s 喂一次 Observe,按 user×protocol 累进当前分钟的桶;跨分钟时 Observe 返回
// 上一分钟的 payload,由调用方入队上报。纯数据、无 IO,便于单测。
//
// ★ 开关同 OTUN_PROBE_TRACE:环境变量、默认关、只认 "1"。

import (
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"
)

// SchemaConnQuality schema=conn_quality(ops-collector 消费落 obs_conn_quality)。
const SchemaConnQuality = "conn_quality"

// ConnQualityEnv 打开 conn_quality 采集的环境变量,值为 "1" 时开启。
const ConnQualityEnv = "OTUN_CONN_QUALITY"

// ConnQualityEnabled 报告本进程是否开启 conn_quality 采集。
func ConnQualityEnabled() bool {
	return os.Getenv(ConnQualityEnv) == "1"
}

// 判定阈值初值(设计稿 §2.2/§2.3,基线后修正)。
const (
	// BusyWindowBytes 一个采样窗口内下行达到此值即为"忙窗口"(用户在用)。
	BusyWindowBytes = 256 * 1024
	// StallAfter 客户端发出数据后超过此时长没有任何下行 = 一次卡顿。
	StallAfter = 5 * time.Second
)

// QualityConn 一条连接的快照(由上层从 egress.ConnInfo 整形传入)。
type QualityConn struct {
	ID          string
	User        string
	Protocol    string
	Destination string // host:port,只用来挑 80/443 做卡顿判定
	Upload      int64
	Download    int64
	LastRead    time.Time // 最近一次收到客户端数据
	LastWrite   time.Time // 最近一次发给客户端数据
}

// QualityQUIC 一条 QUIC 连接的累计传输计数(由上层从 egress.QUICStat 整形传入)。
type QualityQUIC struct {
	Key               string
	User              string
	Protocol          string
	CongestionControl string
	BrutalRxBPS       uint64 // 字节/秒
	Source            string // 客户端 ip:port
	MinRTT            time.Duration
	SmoothedRTT       time.Duration
	PacketsSent       uint64
	PacketsLost       uint64
}

// ConnQualityPayload schema=conn_quality:一个节点一分钟内所有活跃 user×protocol。
type ConnQualityPayload struct {
	Minute    string            `json:"minute"` // RFC3339,分钟起点(UTC)
	WindowSec int               `json:"window_sec"`
	Users     []ConnQualityUser `json:"users"`
}

// ConnQualityUser 一个 user×protocol 在一分钟内的聚合。RTT 类字段在没有 QUIC
// 样本时为 0 且 QUICSamples=0,消费端据此区分"没测到"和"0ms"。
type ConnQualityUser struct {
	UserUUID          string  `json:"user_uuid"`
	Protocol          string  `json:"protocol"`
	CongestionControl string  `json:"cc,omitempty"`
	BrutalRxMbps      float64 `json:"brutal_rx_mbps,omitempty"`
	PeerPrefix        string  `json:"peer_prefix,omitempty"` // v4 /24、v6 /48
	Conns             int     `json:"conns"`
	QUICConns         int     `json:"quic_conns"`
	QUICSamples       int     `json:"quic_samples"`
	SRTTP50Ms         int64   `json:"srtt_p50_ms"`
	SRTTMaxMs         int64   `json:"srtt_max_ms"`
	MinRTTMs          int64   `json:"min_rtt_ms"`
	PktsSent          uint64  `json:"pkts_sent"`
	PktsLost          uint64  `json:"pkts_lost"`
	UpBytes           int64   `json:"up_bytes"`
	DownBytes         int64   `json:"down_bytes"`
	Windows           int     `json:"windows"` // 本分钟采样窗口数
	BusyWindows       int     `json:"busy_windows"`
	GoodputP50Kbps    int64   `json:"goodput_p50_kbps"` // 只统计忙窗口
	GoodputMaxKbps    int64   `json:"goodput_max_kbps"`
	StallCount        int     `json:"stall_count"`
}

type qualityKey struct{ user, protocol string }

type qualityBucket struct {
	cc          string
	brutalRx    uint64
	peer        string
	conns       map[string]struct{}
	quicConns   map[string]struct{}
	srtt        []int64
	minRTT      int64
	pktsSent    uint64
	pktsLost    uint64
	up, down    int64
	windows     int
	busy        int
	goodput     []int64
	stalls      int
	windowDown  int64 // 当前窗口内的下行(Observe 末尾结算忙窗口)
	windowTouch bool  // 当前窗口内本桶是否出现过
}

type connBytes struct{ up, down int64 }
type quicCounters struct{ sent, lost uint64 }

// QualityAggregator 按分钟聚合 conn_quality。非并发安全(只在主循环里调用)。
type QualityAggregator struct {
	minute    time.Time
	buckets   map[qualityKey]*qualityBucket
	prevConn  map[string]connBytes
	prevQUIC  map[string]quicCounters
	stalledAt map[string]time.Time // conn → 已计过卡顿的那次 LastRead,避免跨窗口重复计
	lastTick  time.Time
}

// NewQualityAggregator 创建聚合器。
func NewQualityAggregator() *QualityAggregator {
	return &QualityAggregator{
		buckets:   map[qualityKey]*qualityBucket{},
		prevConn:  map[string]connBytes{},
		prevQUIC:  map[string]quicCounters{},
		stalledAt: map[string]time.Time{},
	}
}

func (a *QualityAggregator) bucket(user, protocol string) *qualityBucket {
	k := qualityKey{user, protocol}
	b := a.buckets[k]
	if b == nil {
		b = &qualityBucket{conns: map[string]struct{}{}, quicConns: map[string]struct{}{}}
		a.buckets[k] = b
	}
	return b
}

// Observe 喂一个采样窗口。now 跨入新的一分钟时,先把上一分钟结算成 payload 返回
// (否则返回 nil),再把本窗口计入新分钟。
//
// 字节按连接求差:新出现的连接全额计入(它的字节都发生在本窗口内或首个窗口);
// 两次采样之间开了又关的连接看不到,会少计——短连接多的场景吞吐偏低,可接受。
func (a *QualityAggregator) Observe(conns []QualityConn, quic []QualityQUIC, now time.Time) *ConnQualityPayload {
	now = now.UTC()
	minute := now.Truncate(time.Minute)
	var out *ConnQualityPayload
	if !a.minute.IsZero() && minute.After(a.minute) {
		out = a.flush()
	}
	if a.minute.IsZero() || minute.After(a.minute) {
		a.minute = minute
	}
	elapsed := 10 * time.Second
	if !a.lastTick.IsZero() && now.After(a.lastTick) {
		elapsed = now.Sub(a.lastTick)
	}
	a.lastTick = now

	for _, b := range a.buckets {
		b.windowDown, b.windowTouch = 0, false
	}

	curConn := make(map[string]connBytes, len(conns))
	for _, c := range conns {
		b := a.bucket(c.User, c.Protocol)
		b.conns[c.ID] = struct{}{}
		b.windowTouch = true
		prev := a.prevConn[c.ID]
		du, dd := c.Upload-prev.up, c.Download-prev.down
		if du < 0 || dd < 0 { // 同 id 换了一条新连接(id 不含时间戳),按新连接全额计
			du, dd = c.Upload, c.Download
		}
		b.up += du
		b.down += dd
		b.windowDown += dd
		curConn[c.ID] = connBytes{c.Upload, c.Download}

		if isStall(c, now) && !a.stalledAt[c.ID].Equal(c.LastRead) {
			b.stalls++
			a.stalledAt[c.ID] = c.LastRead
		}
	}
	a.prevConn = curConn
	for id := range a.stalledAt {
		if _, live := curConn[id]; !live {
			delete(a.stalledAt, id)
		}
	}

	curQUIC := make(map[string]quicCounters, len(quic))
	for _, q := range quic {
		b := a.bucket(q.User, q.Protocol)
		b.windowTouch = true
		b.quicConns[q.Key] = struct{}{}
		if q.CongestionControl != "" {
			b.cc = q.CongestionControl
		}
		if q.BrutalRxBPS > 0 {
			b.brutalRx = q.BrutalRxBPS
		}
		if p := peerPrefix(q.Source); p != "" {
			b.peer = p
		}
		if q.SmoothedRTT > 0 {
			b.srtt = append(b.srtt, q.SmoothedRTT.Milliseconds())
		}
		if m := q.MinRTT.Milliseconds(); q.MinRTT > 0 && (b.minRTT == 0 || m < b.minRTT) {
			b.minRTT = m
		}
		prev, seen := a.prevQUIC[q.Key]
		ds := q.PacketsSent
		dl := q.PacketsLost
		if seen {
			ds = sub0(q.PacketsSent, prev.sent)
			dl = sub0(q.PacketsLost, prev.lost) // PacketsLost 非单调(判丢后又收到)
		}
		b.pktsSent += ds
		b.pktsLost += dl
		curQUIC[q.Key] = quicCounters{q.PacketsSent, q.PacketsLost}
	}
	a.prevQUIC = curQUIC

	for _, b := range a.buckets {
		if !b.windowTouch {
			continue
		}
		b.windows++
		if b.windowDown >= BusyWindowBytes {
			b.busy++
			b.goodput = append(b.goodput, b.windowDown*8*int64(time.Second)/int64(elapsed)/1000)
		}
	}
	return out
}

// flush 结算当前分钟:只输出本分钟有过连接或 QUIC 样本的桶,然后清空。
func (a *QualityAggregator) flush() *ConnQualityPayload {
	p := &ConnQualityPayload{Minute: a.minute.Format(time.RFC3339), WindowSec: 60}
	for k, b := range a.buckets {
		if b.windows == 0 {
			continue
		}
		u := ConnQualityUser{
			UserUUID: k.user, Protocol: k.protocol,
			CongestionControl: b.cc, PeerPrefix: b.peer,
			Conns: len(b.conns), QUICConns: len(b.quicConns), QUICSamples: len(b.srtt),
			MinRTTMs: b.minRTT, PktsSent: b.pktsSent, PktsLost: b.pktsLost,
			UpBytes: b.up, DownBytes: b.down,
			Windows: b.windows, BusyWindows: b.busy, StallCount: b.stalls,
		}
		if b.brutalRx > 0 {
			u.BrutalRxMbps = float64(b.brutalRx) * 8 / 1e6
		}
		u.SRTTP50Ms, u.SRTTMaxMs = p50max(b.srtt)
		u.GoodputP50Kbps, u.GoodputMaxKbps = p50max(b.goodput)
		p.Users = append(p.Users, u)
	}
	sort.Slice(p.Users, func(i, j int) bool {
		if p.Users[i].UserUUID != p.Users[j].UserUUID {
			return p.Users[i].UserUUID < p.Users[j].UserUUID
		}
		return p.Users[i].Protocol < p.Users[j].Protocol
	})
	a.buckets = map[qualityKey]*qualityBucket{}
	if len(p.Users) == 0 {
		return nil
	}
	return p
}

// isStall:客户端最近发过数据、之后没有任何下行,且已过 StallAfter。只看 80/443,
// 长轮询/推送类端口天生"发了很久不回",会误报。
func isStall(c QualityConn, now time.Time) bool {
	if c.LastRead.IsZero() || !c.LastRead.After(c.LastWrite) {
		return false
	}
	if now.Sub(c.LastRead) < StallAfter {
		return false
	}
	_, port, err := net.SplitHostPort(c.Destination)
	return err == nil && (port == "443" || port == "80")
}

// peerPrefix 把客户端地址截成 v4 /24、v6 /48,只用于按运营商段聚类。
func peerPrefix(source string) string {
	host := source
	if h, _, err := net.SplitHostPort(source); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	pfx, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return pfx.String()
}

func sub0(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// p50max 返回中位数(偶数个取下中位)与最大值;空切片返回 0,0。
func p50max(v []int64) (int64, int64) {
	if len(v) == 0 {
		return 0, 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)/2], s[len(s)-1]
}
