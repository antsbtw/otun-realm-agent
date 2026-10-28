package obs

import (
	"testing"
	"time"
)

var qBase = time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return qBase.Add(time.Duration(sec) * time.Second) }

// running 返回已经采样过一次的聚合器(模拟 agent 运行中):进程第一次采样只记基线。
func running() *QualityAggregator {
	a := NewQualityAggregator()
	a.Observe(nil, nil, at(-10))
	return a
}

// 6 个 10s 窗口:前 3 个各下行 1 MB(忙),后 3 个各 10 KB(闲);跨分钟才出 payload。
func TestQualityBusyWindowsAndGoodput(t *testing.T) {
	a := running()
	var down int64
	for i := 0; i < 6; i++ {
		if i < 3 {
			down += 1 << 20
		} else {
			down += 10 << 10
		}
		conns := []QualityConn{{ID: "c1", User: "u1", Protocol: "hysteria2", Destination: "x.com:443", Upload: 100, Download: down}}
		if p := a.Observe(conns, nil, at(i*10)); p != nil {
			t.Fatalf("payload before minute boundary at window %d", i)
		}
	}
	p := a.Observe(nil, nil, at(60))
	if p == nil || len(p.Users) != 1 {
		t.Fatalf("want one user at minute boundary, got %+v", p)
	}
	u := p.Users[0]
	if p.Minute != "2026-10-19T03:00:00Z" || u.UserUUID != "u1" || u.Protocol != "hysteria2" {
		t.Fatalf("identity: %+v / %s", u, p.Minute)
	}
	if u.Windows != 6 || u.BusyWindows != 3 {
		t.Fatalf("windows=%d busy=%d, want 6/3", u.Windows, u.BusyWindows)
	}
	// 1 MiB / 10s = 838 kbps
	if u.GoodputP50Kbps != 838 || u.GoodputMaxKbps != 838 {
		t.Fatalf("goodput p50=%d max=%d, want 838", u.GoodputP50Kbps, u.GoodputMaxKbps)
	}
	if u.DownBytes != down || u.UpBytes != 100 {
		t.Fatalf("bytes up=%d down=%d", u.UpBytes, u.DownBytes)
	}
	// 新一分钟没有任何数据 → 不出 payload。
	if p := a.Observe(nil, nil, at(130)); p != nil {
		t.Fatalf("empty minute must not emit, got %+v", p)
	}
}

func TestQualityQUICDeltasAndRTT(t *testing.T) {
	a := NewQualityAggregator()
	q := func(sent, lost uint64, srtt time.Duration) []QualityQUIC {
		return []QualityQUIC{{Key: "q1", User: "u1", Protocol: "reality", CongestionControl: "",
			Source: "39.144.248.21:50000", MinRTT: 70 * time.Millisecond, SmoothedRTT: srtt,
			PacketsSent: sent, PacketsLost: lost}}
	}
	a.Observe(nil, q(1000, 10, 90*time.Millisecond), at(0))
	a.Observe(nil, q(3000, 50, 100*time.Millisecond), at(10))
	a.Observe(nil, q(4000, 40, 400*time.Millisecond), at(20)) // lost 回落 → 本窗口记 0
	p := a.Observe(nil, nil, at(60))
	if p == nil || len(p.Users) != 1 {
		t.Fatalf("got %+v", p)
	}
	u := p.Users[0]
	if u.PktsSent != 4000 || u.PktsLost != 50 {
		t.Fatalf("sent=%d lost=%d, want 4000/50", u.PktsSent, u.PktsLost)
	}
	if u.SRTTP50Ms != 100 || u.SRTTMaxMs != 400 || u.MinRTTMs != 70 || u.QUICSamples != 3 {
		t.Fatalf("rtt: %+v", u)
	}
	if u.PeerPrefix != "39.144.248.0/24" || u.QUICConns != 1 {
		t.Fatalf("peer/conns: %+v", u)
	}
}

func TestQualityBrutalRate(t *testing.T) {
	a := NewQualityAggregator()
	a.Observe(nil, []QualityQUIC{{Key: "q1", User: "u1", Protocol: "hysteria2",
		CongestionControl: "brutal", BrutalRxBPS: 3_750_000, SmoothedRTT: time.Millisecond}}, at(0))
	u := a.Observe(nil, nil, at(60)).Users[0]
	if u.CongestionControl != "brutal" || u.BrutalRxMbps != 30 {
		t.Fatalf("cc=%q rx=%v, want brutal/30", u.CongestionControl, u.BrutalRxMbps)
	}
}

// 客户端发了请求、5s+ 无下行 → 计一次;同一次停顿跨多个窗口不重复计;非 80/443 不计。
func TestQualityStallCountedOnce(t *testing.T) {
	a := running()
	req := at(0)
	stalled := QualityConn{ID: "c1", User: "u1", Protocol: "trojan", Destination: "img.x.com:443",
		Upload: 900, Download: 0, LastRead: req}
	push := QualityConn{ID: "c2", User: "u1", Protocol: "trojan", Destination: "push.x.com:5223",
		Upload: 900, LastRead: req}
	for _, s := range []int{1, 10, 20, 30} {
		a.Observe([]QualityConn{stalled, push}, nil, at(s))
	}
	u := a.Observe(nil, nil, at(60)).Users[0]
	if u.StallCount != 1 {
		t.Fatalf("stall_count=%d, want 1", u.StallCount)
	}
}

func TestPeerPrefix(t *testing.T) {
	cases := map[string]string{
		"39.144.248.21:50000":           "39.144.248.0/24",
		"[2409:8a1e:1234:5678::1]:443":  "2409:8a1e:1234::/48",
		"[::ffff:117.181.51.117]:51820": "117.181.51.0/24",
		"":                              "",
		"not-an-ip:1":                   "",
	}
	for in, want := range cases {
		if got := peerPrefix(in); got != want {
			t.Errorf("peerPrefix(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestConnQualityEnabledOnlyOne(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		t.Setenv(ConnQualityEnv, v)
		if got := ConnQualityEnabled(); got != want {
			t.Errorf("%s=%q → %v, want %v", ConnQualityEnv, v, got, want)
		}
	}
}

// 下载完成后客户端只发了几十字节控制包(WINDOW_UPDATE/ACK)→ 不是卡顿;
// 之后又发了一个真实请求(≥512B)且 5s 无回应 → 计一次。
func TestQualityStallIgnoresSmallControlFrames(t *testing.T) {
	a := running()
	c := QualityConn{ID: "c1", User: "u1", Protocol: "hysteria2", Destination: "pbs.twimg.com:443",
		Upload: 600, Download: 2 << 20, LastWrite: at(0), LastRead: at(1)}
	a.Observe([]QualityConn{c}, nil, at(0)) // 首见:下行增长,基线上行=600
	c.Upload = 640                          // 40 字节控制包
	c.LastRead = at(2)
	a.Observe([]QualityConn{c}, nil, at(10))
	a.Observe([]QualityConn{c}, nil, at(20))
	c.Upload = 640 + 900 // 新请求
	c.LastRead = at(25)
	a.Observe([]QualityConn{c}, nil, at(31))
	a.Observe([]QualityConn{c}, nil, at(41))
	u := a.Observe(nil, nil, at(60)).Users[0]
	if u.StallCount != 1 {
		t.Fatalf("stall_count=%d, want 1 (control frames ignored, real request counted once)", u.StallCount)
	}
}

// 10-28 回归:同一用户两条并发连接连同一目标。id 唯一时各自求差,字节 = 真实增量;
// 旧 id(不含开始时间)撞成一个时,大连接每个窗口都会按"累计值 − 小连接累计值"重复计入。
func TestQualityConcurrentConnsSameDestination(t *testing.T) {
	a := running()
	big := QualityConn{ID: "hysteria2|u1|1.2.3.4:5|cdn.x.com:443|100", User: "u1", Protocol: "hysteria2", Destination: "cdn.x.com:443"}
	small := QualityConn{ID: "hysteria2|u1|1.2.3.4:5|cdn.x.com:443|200", User: "u1", Protocol: "hysteria2", Destination: "cdn.x.com:443"}
	var want int64
	for i := 0; i < 6; i++ {
		big.Download += 1 << 20
		small.Download += 1 << 10
		want += 1<<20 + 1<<10
		a.Observe([]QualityConn{big, small}, nil, at(i*10))
	}
	u := a.Observe(nil, nil, at(60)).Users[0]
	if u.DownBytes != want {
		t.Fatalf("down=%d, want %d (no double counting)", u.DownBytes, want)
	}
	if u.Conns != 2 {
		t.Fatalf("conns=%d, want 2", u.Conns)
	}
}

// 进程第一次采样:已存在连接的累计字节不属于这 10 秒,只记基线。
func TestQualityFirstObserveIsBaseline(t *testing.T) {
	a := NewQualityAggregator()
	c := QualityConn{ID: "c1", User: "u1", Protocol: "trojan", Destination: "x.com:443", Upload: 5 << 20, Download: 500 << 20}
	a.Observe([]QualityConn{c}, nil, at(0))
	if w := a.LastWindow(); w.DownBytes != 0 || w.UpBytes != 0 || w.ActiveUsers != 1 || w.Conns != 1 {
		t.Fatalf("first window: %+v", w)
	}
	c.Download += 1 << 20
	a.Observe([]QualityConn{c}, nil, at(10))
	u := a.Observe(nil, nil, at(60)).Users[0]
	if u.DownBytes != 1<<20 || u.UpBytes != 0 || u.BusyWindows != 1 {
		t.Fatalf("down=%d up=%d busy=%d, want 1MiB/0/1", u.DownBytes, u.UpBytes, u.BusyWindows)
	}
}

// LastWindow:全节点按窗口合计;忙用户按用户合计(跨协议),不按 user×protocol。
func TestQualityLastWindowTotals(t *testing.T) {
	a := running()
	conns := []QualityConn{
		{ID: "a", User: "u1", Protocol: "hysteria2", Destination: "x.com:443"},
		{ID: "b", User: "u1", Protocol: "reality", Destination: "y.com:443"},
		{ID: "c", User: "u2", Protocol: "hysteria2", Destination: "z.com:443"},
	}
	a.Observe(conns, nil, at(0))
	conns[0].Download, conns[1].Download, conns[2].Download = 150<<10, 150<<10, 10<<10
	conns[2].Upload = 4 << 10
	a.Observe(conns, nil, at(10))
	w := a.LastWindow()
	if w.DownBytes != 310<<10 || w.UpBytes != 4<<10 || w.Conns != 3 || w.ActiveUsers != 2 || w.BusyUsers != 1 || w.Elapsed != 10*time.Second {
		t.Fatalf("window: %+v", w)
	}
}
