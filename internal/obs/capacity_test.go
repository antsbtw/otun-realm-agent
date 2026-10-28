package obs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 6 个窗口,每个窗口全节点合计不同;跨分钟出一行,速率按窗口求、再取 p50/p95/max。
func TestNodeCapacityPerWindowSums(t *testing.T) {
	a := NewNodeCapacityAggregator()
	downs := []int64{1 << 20, 2 << 20, 3 << 20, 4 << 20, 5 << 20, 6 << 20}
	var rx, tx int64 = 1000, 2000
	for i, d := range downs {
		rx += 2 * d
		tx += 3 * d
		w := WindowStats{Elapsed: 10 * time.Second, DownBytes: d, UpBytes: d / 10, Conns: 10 + i, ActiveUsers: 3 + i, BusyUsers: i}
		if p := a.Observe(w, NICCounters{Iface: "eth0", RxByte: rx, TxByte: tx, OK: true}, at(i*10)); p != nil {
			t.Fatalf("payload before minute boundary at %d", i)
		}
	}
	p := a.Observe(WindowStats{Elapsed: 10 * time.Second}, NICCounters{Iface: "eth0", RxByte: rx, TxByte: tx, OK: true}, at(60))
	if p == nil {
		t.Fatal("want payload at minute boundary")
	}
	if p.Minute != "2026-10-19T03:00:00Z" || p.Iface != "eth0" || p.Windows != 6 {
		t.Fatalf("identity: %+v", p)
	}
	// 1 MiB / 10s = 838 kbps;下中位取第 3 个窗口
	if p.UserDownKbpsP50 != 2516 || p.UserDownKbpsMax != 5033 || p.UserDownKbpsP95 != 5033 {
		t.Fatalf("user down p50/p95/max = %d/%d/%d", p.UserDownKbpsP50, p.UserDownKbpsP95, p.UserDownKbpsMax)
	}
	// 第一个窗口网卡只有基线,所以 NIC 只有 5 个窗口
	if p.NICWindows != 5 || p.NICTxKbpsMax != 15099 || p.NICRxKbpsMax != 10066 {
		t.Fatalf("nic windows=%d tx max=%d rx max=%d", p.NICWindows, p.NICTxKbpsMax, p.NICRxKbpsMax)
	}
	if p.BusyUsersMax != 5 || p.BusyUsersP50 != 2 || p.ActiveUsersMax != 8 || p.ConnsMax != 15 {
		t.Fatalf("users: %+v", p)
	}
}

// 网卡读不到、换网卡、计数回退(重启/换卡)时不出该窗口的网卡速率,也不出负数。
func TestNodeCapacityNICGaps(t *testing.T) {
	a := NewNodeCapacityAggregator()
	w := WindowStats{Elapsed: 10 * time.Second, DownBytes: 1 << 20}
	a.Observe(w, NICCounters{Iface: "eth0", RxByte: 5000, TxByte: 5000, OK: true}, at(0))
	a.Observe(w, NICCounters{}, at(10))                                                    // 没读到
	a.Observe(w, NICCounters{Iface: "eth0", RxByte: 6000, TxByte: 6000, OK: true}, at(20)) // 上次没读到 → 只记基线
	a.Observe(w, NICCounters{Iface: "eth1", RxByte: 9000, TxByte: 9000, OK: true}, at(30)) // 换网卡
	a.Observe(w, NICCounters{Iface: "eth1", RxByte: 100, TxByte: 100, OK: true}, at(40))   // 回退
	a.Observe(w, NICCounters{Iface: "eth1", RxByte: 1350, TxByte: 1350, OK: true}, at(50)) // 正常 1250B/10s = 1 kbps
	p := a.Observe(WindowStats{}, NICCounters{}, at(60))
	if p.NICWindows != 1 || p.NICRxKbpsMax != 1 || p.Iface != "eth1" || p.Windows != 6 {
		t.Fatalf("nic gaps: %+v", p)
	}
}

// 一分钟内没有任何有效窗口 → 不出 payload。
func TestNodeCapacityEmptyMinute(t *testing.T) {
	a := NewNodeCapacityAggregator()
	a.Observe(WindowStats{}, NICCounters{}, at(0))
	if p := a.Observe(WindowStats{}, NICCounters{}, at(60)); p != nil {
		t.Fatalf("empty minute must not emit, got %+v", p)
	}
}

func TestNICReaderDefaultRoute(t *testing.T) {
	dir := t.TempDir()
	route := filepath.Join(dir, "route")
	os.WriteFile(route, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+
		"enp0s31f6\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"), 0o644)
	stat := filepath.Join(dir, "net", "enp0s31f6", "statistics")
	os.MkdirAll(stat, 0o755)
	os.WriteFile(filepath.Join(stat, "rx_bytes"), []byte("123\n"), 0o644)
	os.WriteFile(filepath.Join(stat, "tx_bytes"), []byte("456\n"), 0o644)
	r := &NICReader{RouteFile: route, SysNet: filepath.Join(dir, "net")}
	if c := r.Read(); !c.OK || c.Iface != "enp0s31f6" || c.RxByte != 123 || c.TxByte != 456 {
		t.Fatalf("read: %+v", c)
	}
	os.RemoveAll(stat) // 网卡消失 → 读失败,下次重新找默认路由
	if c := r.Read(); c.OK {
		t.Fatalf("want not ok after iface gone, got %+v", c)
	}
}

func TestPctls(t *testing.T) {
	if a, b, c := pctls(nil); a != 0 || b != 0 || c != 0 {
		t.Fatal("empty")
	}
	v := make([]int64, 100)
	for i := range v {
		v[i] = int64(100 - i)
	}
	if p50, p95, mx := pctls(v); p50 != 50 || p95 != 95 || mx != 100 {
		t.Fatalf("p50/p95/max = %d/%d/%d", p50, p95, mx)
	}
}
