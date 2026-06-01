package obs

import (
	"net"
	"strings"
	"time"
)

// ProbeTarget 一个出口质量探测目标（§7.3.5）。清单由运营平面/manager 下发或 agent 配置。
type ProbeTarget struct {
	Target string // "223.5.5.5" 或 "www.iqiyi.com:443"
	Type   string // "icmp" | "tcp"
}

// DefaultProbeTargets 是出口质量的默认探测清单（可被下发覆盖）。
// 全部用 TCP 连接探测（icmp 需 raw socket，家用 debian 不保证权限；tcp 更稳）。
// 对纯 IP 目标补默认端口 53（DNS，多数出口可达）。
func DefaultProbeTargets() []ProbeTarget {
	return []ProbeTarget{
		{Target: "223.5.5.5:53", Type: "tcp"},
		{Target: "1.1.1.1:53", Type: "tcp"},
		{Target: "www.iqiyi.com:443", Type: "tcp"},
	}
}

// ProbeEgress 探一组目的地的可达性/RTT，产出 egress_quality payload。
// publicIP 由上层（stats.GetPublicIPv4）传入，§5.1：仅可观测。
func ProbeEgress(targets []ProbeTarget, publicIP string) EgressQualityPayload {
	out := EgressQualityPayload{PublicIP: publicIP}
	for _, t := range targets {
		addr := normalizeTCPAddr(t.Target)
		rtt, ok := tcpProbe(addr, 3*time.Second)
		out.Probes = append(out.Probes, EgressProbe{
			Target: t.Target,
			Type:   "tcp", // 实际探测一律 tcp（见上）
			RTTMs:  rtt,
			OK:     ok,
		})
	}
	return out
}

// normalizeTCPAddr 给没带端口的纯 IP/host 补默认端口 53。
func normalizeTCPAddr(target string) string {
	if strings.Contains(target, ":") {
		// 已带端口（IPv4:port）或是 IPv6 字面量——简单判定：最后一段是端口则直接用。
		if _, _, err := net.SplitHostPort(target); err == nil {
			return target
		}
	}
	return target + ":53"
}

// tcpProbe 做一次 TCP 拨号，返回 RTT 毫秒与是否成功。
func tcpProbe(addr string, timeout time.Duration) (int64, bool) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	rtt := time.Since(start).Milliseconds()
	if err != nil {
		return rtt, false
	}
	conn.Close()
	return rtt, true
}
