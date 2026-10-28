package obs

// capacity.go —— 节点带宽需求(node_capacity):每分钟一行,用于节点带宽规划。
// 设计稿:ops-collector/docs/CONN_EXPERIENCE_QUALITY_DESIGN.md §2.4.1 / §3.3。
//
// 输入是 conn_quality 同一次 10s 采样的全节点合计(QualityAggregator.LastWindow)
// 加上默认路由网卡的累计收发字节。各用户的分钟峰值不在同一时刻,相加会高估,
// 所以这里在【同一个窗口】里求和,再看一分钟内各窗口的 p50 / p95 / max。
// 和 conn_quality 共用 OTUN_CONN_QUALITY 开关。纯数据、无 IO(读网卡在 ReadNIC)。

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaNodeCapacity schema=node_capacity(ops-collector 消费落 obs_node_capacity)。
const SchemaNodeCapacity = "node_capacity"

// NodeCapacityPayload schema=node_capacity:一个节点一分钟。速率单位 kbps。
// NIC 字段在读不到网卡时为 0 且 NICWindows=0,消费端据此区分"没测到"和"0"。
type NodeCapacityPayload struct {
	Minute          string `json:"minute"` // RFC3339,分钟起点(UTC)
	Iface           string `json:"iface,omitempty"`
	Windows         int    `json:"windows"`
	NICWindows      int    `json:"nic_windows"`
	UserDownKbpsP50 int64  `json:"user_down_kbps_p50"`
	UserDownKbpsP95 int64  `json:"user_down_kbps_p95"`
	UserDownKbpsMax int64  `json:"user_down_kbps_max"`
	UserUpKbpsP50   int64  `json:"user_up_kbps_p50"`
	UserUpKbpsP95   int64  `json:"user_up_kbps_p95"`
	UserUpKbpsMax   int64  `json:"user_up_kbps_max"`
	NICRxKbpsP50    int64  `json:"nic_rx_kbps_p50"`
	NICRxKbpsP95    int64  `json:"nic_rx_kbps_p95"`
	NICRxKbpsMax    int64  `json:"nic_rx_kbps_max"`
	NICTxKbpsP50    int64  `json:"nic_tx_kbps_p50"`
	NICTxKbpsP95    int64  `json:"nic_tx_kbps_p95"`
	NICTxKbpsMax    int64  `json:"nic_tx_kbps_max"`
	BusyUsersP50    int    `json:"busy_users_p50"`
	BusyUsersMax    int    `json:"busy_users_max"`
	ActiveUsersMax  int    `json:"active_users_max"`
	ConnsMax        int    `json:"conns_max"`
}

// NICCounters 网卡累计收发字节(OK=false 表示本次没读到)。
type NICCounters struct {
	Iface  string
	RxByte int64
	TxByte int64
	OK     bool
}

// NodeCapacityAggregator 按分钟聚合 node_capacity。非并发安全(只在主循环里调用)。
type NodeCapacityAggregator struct {
	minute              time.Time
	iface               string
	userDown, userUp    []int64
	nicRx, nicTx        []int64
	busy                []int64
	activeMax, connsMax int
	prevNIC             NICCounters
}

// NewNodeCapacityAggregator 创建聚合器。
func NewNodeCapacityAggregator() *NodeCapacityAggregator { return &NodeCapacityAggregator{} }

// Observe 喂一个采样窗口。跨入新的一分钟时先结算上一分钟返回(否则 nil),再计入新分钟。
// 网卡计数只在前后两次都读到、同一块网卡、且没有回退时才算一个窗口。
func (a *NodeCapacityAggregator) Observe(w WindowStats, nic NICCounters, now time.Time) *NodeCapacityPayload {
	now = now.UTC()
	minute := now.Truncate(time.Minute)
	var out *NodeCapacityPayload
	if !a.minute.IsZero() && minute.After(a.minute) {
		out = a.flush()
	}
	if a.minute.IsZero() || minute.After(a.minute) {
		a.minute = minute
	}

	if w.Elapsed > 0 {
		a.userDown = append(a.userDown, kbps(w.DownBytes, w.Elapsed))
		a.userUp = append(a.userUp, kbps(w.UpBytes, w.Elapsed))
		a.busy = append(a.busy, int64(w.BusyUsers))
		a.activeMax = max(a.activeMax, w.ActiveUsers)
		a.connsMax = max(a.connsMax, w.Conns)
	}
	p := a.prevNIC
	if nic.OK && p.OK && nic.Iface == p.Iface && w.Elapsed > 0 &&
		nic.RxByte >= p.RxByte && nic.TxByte >= p.TxByte {
		a.nicRx = append(a.nicRx, kbps(nic.RxByte-p.RxByte, w.Elapsed))
		a.nicTx = append(a.nicTx, kbps(nic.TxByte-p.TxByte, w.Elapsed))
	}
	if nic.OK {
		a.iface = nic.Iface
	}
	a.prevNIC = nic
	return out
}

func (a *NodeCapacityAggregator) flush() *NodeCapacityPayload {
	defer func() {
		a.userDown, a.userUp, a.nicRx, a.nicTx, a.busy = nil, nil, nil, nil, nil
		a.activeMax, a.connsMax = 0, 0
	}()
	if len(a.userDown) == 0 {
		return nil
	}
	p := &NodeCapacityPayload{Minute: a.minute.Format(time.RFC3339), Iface: a.iface,
		Windows: len(a.userDown), NICWindows: len(a.nicRx),
		ActiveUsersMax: a.activeMax, ConnsMax: a.connsMax}
	p.UserDownKbpsP50, p.UserDownKbpsP95, p.UserDownKbpsMax = pctls(a.userDown)
	p.UserUpKbpsP50, p.UserUpKbpsP95, p.UserUpKbpsMax = pctls(a.userUp)
	p.NICRxKbpsP50, p.NICRxKbpsP95, p.NICRxKbpsMax = pctls(a.nicRx)
	p.NICTxKbpsP50, p.NICTxKbpsP95, p.NICTxKbpsMax = pctls(a.nicTx)
	b50, _, bmax := pctls(a.busy)
	p.BusyUsersP50, p.BusyUsersMax = int(b50), int(bmax)
	return p
}

func kbps(bytes int64, d time.Duration) int64 {
	return bytes * 8 * int64(time.Second) / int64(d) / 1000
}

// pctls 返回 p50(偶数个取下中位)、p95(最近秩)、max;空切片返回 0,0,0。
// 一分钟只有 6 个窗口,p95 实际等于 max,留着是为了以后加密采样时口径不变。
func pctls(v []int64) (int64, int64, int64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i95 := (len(s)*95+99)/100 - 1
	return s[(len(s)-1)/2], s[i95], s[len(s)-1]
}

// —— 网卡计数 ——

// NICReader 读默认路由网卡的累计收发字节。路径可替换,便于单测。
type NICReader struct {
	RouteFile string // 默认 /proc/net/route
	SysNet    string // 默认 /sys/class/net
	iface     string
}

// NewNICReader 用系统默认路径。
func NewNICReader() *NICReader {
	return &NICReader{RouteFile: "/proc/net/route", SysNet: "/sys/class/net"}
}

// Read 读一次。网卡还没解析出来或读失败时重新找默认路由网卡(换网卡、拨号重连)。
func (r *NICReader) Read() NICCounters {
	if r.iface == "" {
		r.iface = defaultIface(r.RouteFile)
		if r.iface == "" {
			return NICCounters{}
		}
	}
	rx, err1 := readInt(filepath.Join(r.SysNet, r.iface, "statistics", "rx_bytes"))
	tx, err2 := readInt(filepath.Join(r.SysNet, r.iface, "statistics", "tx_bytes"))
	if err1 != nil || err2 != nil {
		r.iface = ""
		return NICCounters{}
	}
	return NICCounters{Iface: r.iface, RxByte: rx, TxByte: tx, OK: true}
}

// defaultIface 在 /proc/net/route 里找目标 00000000、掩码 00000000 的那一行。
func defaultIface(routeFile string) string {
	f, err := os.Open(routeFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 8 && fs[1] == "00000000" && fs[7] == "00000000" {
			return fs[0]
		}
	}
	return ""
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}
