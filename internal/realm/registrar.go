// Package realm 实现 Cone NAT 端口漂移下的【按需重注册】循环（§7.9）。
//
// 边界（§7.9.2）：STUN 发现 + 注册到 L3 = sing-box realm{} 内部行为，agent 不调 STUN、不拼地址。
// agent 只负责"让 sing-box 在跑、配置正确"，并在【探测到注册失效时】触发 reload 重注册。
//
// ★§7.0 约束：reload = 整进程重启断所有连接。所以本循环【绝不无条件定时 reload】，
// 逻辑必须是"探测到失效才 reload"（§7.9.3）。
package realm

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Registrar 负责 L3 可达性探测与按需重注册。
type Registrar struct {
	httpClient         *http.Client // 验证证书(正式证会合面，如 situstechnologies.com/realm)
	httpClientInsecure *http.Client // 跳过证书验证(自签证会合面，rendezvous_insecure=true 的 IP:port)

	mu             sync.Mutex
	lastReachable  bool
	lastProbeRTTMs int64
	unregStreak    int // 连续探到未注册的 tick 数（ConfirmUnregistered）
}

// NewRegistrar 创建重注册器。
func NewRegistrar() *Registrar {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Registrar{
		httpClient: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: noRedirect, // 不跟随重定向：只关心 L3 端点是否在线。
		},
		// ★自签证会合面探测用：rendezvous_insecure=true 时会合面是自签 TLS(如 otun-s-test 的
		// https://<ip>:9443)，验证证书必失败→假报 l3_reachable=false。真实 realm 连接本就 insecure，
		// 故探测也须 skip verify，否则 obs 对自签会合面的出口(de-01 类)恒假报不可达。
		httpClientInsecure: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: noRedirect,
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
}

// ProbeResult 一次 L3 探测的结果（喂 §7.5 realm_health）。
type ProbeResult struct {
	Reachable bool
	RTTMs     int64
}

// ProbeL3 探 server_url 的 L3 可达性（§7.9.3 ①）。
// insecure=true(rendezvous_insecure)：会合面自签 TLS，探测跳过证书验证(否则假报不可达)。
func (r *Registrar) ProbeL3(serverURL string, insecure bool) ProbeResult {
	res := r.probeL3(serverURL, insecure)

	r.mu.Lock()
	r.lastReachable = res.Reachable
	r.lastProbeRTTMs = res.RTTMs
	r.mu.Unlock()

	return res
}

func (r *Registrar) probeL3(serverURL string, insecure bool) ProbeResult {
	u, err := url.Parse(serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		// 退化为 TCP 探测（取 host:port，缺端口补 443）。TCP 探不涉证书，insecure 无关。
		return r.tcpProbe(hostPortFromURL(serverURL))
	}

	start := time.Now()
	req, err := http.NewRequest("GET", serverURL, nil)
	if err != nil {
		return ProbeResult{Reachable: false}
	}
	// ★自签会合面(insecure)用 skip-verify 客户端，否则证书验证失败→假报不可达。
	client := r.httpClient
	if insecure {
		client = r.httpClientInsecure
	}
	resp, err := client.Do(req)
	rtt := time.Since(start).Milliseconds()
	if err != nil {
		return ProbeResult{Reachable: false, RTTMs: rtt}
	}
	resp.Body.Close()
	// 任何 HTTP 响应（含 4xx/5xx）都说明 L3 端点在线。
	return ProbeResult{Reachable: true, RTTMs: rtt}
}

func (r *Registrar) tcpProbe(addr string) ProbeResult {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	rtt := time.Since(start).Milliseconds()
	if err != nil {
		return ProbeResult{Reachable: false, RTTMs: rtt}
	}
	conn.Close()
	return ProbeResult{Reachable: true, RTTMs: rtt}
}

// LastProbe 返回上次探测结果（供 realm_health 上报）。
func (r *Registrar) LastProbe() ProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ProbeResult{Reachable: r.lastReachable, RTTMs: r.lastProbeRTTMs}
}

// Decision 是一次 realmTicker 跳动的判定结果（§7.9.3 ②）。
type Decision struct {
	Reachable  bool
	NeedReload bool   // 是否需要 reload 触发重注册
	Reason     string // 给日志/可观测
	ProbeRTTMs int64
}

// Evaluate 执行 §7.9.3 的按需重注册判定。
//
// registeredSelf 表示"本出口在 L3 是否还注册着自己的 realm_id"（§7.9.4 ② 的判定结果）。
// 调用方根据 7.9.3 待核实项选择如何得到它：
//   - (b) 若 sing-box 自带周期重注册 → 调用方可恒传 true（只探可达，几乎不 reload，最理想）。
//   - (a) 若不自带 → 调用方实现 lookup-自己（复用客户端 lookup 协议）并传入真实结果。
//
// 判定逻辑（§7.9.3）：
//   - L3 不可达 → 不 reload（reload 也连不上，等 L3 回来）。
//   - L3 可达 but 查不到自己（注册掉了/端口漂移失效）→ reload 触发重注册。
//   - L3 可达且注册有效 → 什么都不做（★绝不无脑 reload，保护活跃连接）。
func (r *Registrar) Evaluate(serverURL string, registeredSelf bool, insecure bool) Decision {
	res := r.ProbeL3(serverURL, insecure)

	switch {
	case !res.Reachable:
		return Decision{Reachable: false, NeedReload: false,
			Reason: "l3_unreachable_wait", ProbeRTTMs: res.RTTMs}
	case !registeredSelf:
		return Decision{Reachable: true, NeedReload: true,
			Reason: "registration_lost_reregister", ProbeRTTMs: res.RTTMs}
	default:
		return Decision{Reachable: true, NeedReload: false,
			Reason: "registration_healthy_noop", ProbeRTTMs: res.RTTMs}
	}
}

// ProbeRegistered 问会合面"这些 slot 现在注册着吗"（otun-s GET /v1/{slot}/status，
// authUser=realm token，无副作用——绝不用 409 register 探针，那会抢占空槽）。
//
// ★fail-safe 语义：只有会合面【确凿返回】200 {"registered": false} 才算未注册。
// 其余一切——网络错、401、404（老 otun-s 尚无 /status 端点）、非法 JSON——都视为
// "未知"，按已注册处理。宁可漏探不可误 rebuild（rebuild 断在线隧道），且允许 agent
// 先于会合面升级部署：老会合面对 /status 返 404 → 行为与本改动前逐字节一致。
//
// 返回 (registeredSelf, 确凿未注册的 slot 列表)。
func (r *Registrar) ProbeRegistered(serverURL, token string, slots []string, insecure bool) (bool, []string) {
	if serverURL == "" || len(slots) == 0 {
		return true, nil
	}
	client := r.httpClient
	if insecure {
		client = r.httpClientInsecure
	}
	base := strings.TrimRight(serverURL, "/")
	var unregistered []string
	for _, slot := range slots {
		req, err := http.NewRequest("GET", base+"/v1/"+url.PathEscape(slot)+"/status", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			continue // 网络错 = 未知，fail-safe 按已注册
		}
		var body struct {
			Registered *bool `json:"registered"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil || body.Registered == nil {
			continue // 老会合面 404 / 鉴权失败 / 畸形响应 = 未知
		}
		if !*body.Registered {
			unregistered = append(unregistered, slot)
		}
	}
	return len(unregistered) == 0, unregistered
}

// RebuildConfirmTicks：连续多少个 realm tick 探到未注册才触发 rebuild。
// =2 的原因：单次未注册可能撞上库自身 re-register 的瞬间（会合面重启后 sing-quic
// 的 event-stream 断线重连秒级自愈），立刻 rebuild 会白断在线隧道；连续两个 tick
// （≈2×REALM_INTERVAL）仍未注册才认定"库自愈失败"，交给 rebuild 重跑 STUN。
const RebuildConfirmTicks = 2

// ConfirmUnregistered 记录一次探测结论，返回"是否已连续 RebuildConfirmTicks 次
// 未注册、应当 rebuild"。返回 true 时清零计数（rebuild 后重新累计确认窗口）。
func (r *Registrar) ConfirmUnregistered(unregistered bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !unregistered {
		r.unregStreak = 0
		return false
	}
	r.unregStreak++
	if r.unregStreak >= RebuildConfirmTicks {
		r.unregStreak = 0
		return true
	}
	return false
}

// hostPortFromURL 从一个不带 scheme 的 server_url 提取 host:port，缺端口补 443。
func hostPortFromURL(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return s + ":443"
}
