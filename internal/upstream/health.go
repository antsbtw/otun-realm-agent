// Package upstream 上游住宅代理出口（UPSTREAM_PROXY）的健康自检。
//
// 数据面由 egress.UpstreamDialer 负责（fail closed：上游不通即断流，绝不回落直连）。
// 本包只做观测：定期经同一个 dialer 请求出口 IP 回显服务，比对 UPSTREAM_EXPECT_IP，
// 结果随心跳上报（load_info.upstream）并在状态变化时打日志。自检失败不改变数据面行为。
package upstream

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// echoURLs 出口 IP 回显服务，依次尝试（第一个成功即可），避免单一服务故障误报。
var echoURLs = []string{
	"https://api.ipify.org",
	"https://ipinfo.io/ip",
	"https://ifconfig.me/ip",
}

// Health 一次自检快照（随心跳上报，落 fleet nodes.load_info.upstream）。
type Health struct {
	Enabled             bool      `json:"enabled"`
	Server              string    `json:"server"`               // host:port，不含凭据
	OK                  bool      `json:"ok"`                   // 经上游可达且出口 IP 符合预期（未配预期则只看可达）
	ExitIP              string    `json:"exit_ip,omitempty"`    // 实测出口 IP
	ExpectIP            string    `json:"expect_ip,omitempty"`  // UPSTREAM_EXPECT_IP
	Error               string    `json:"error,omitempty"`      // 不可达原因 / 出口不符
	ConsecutiveFailures int       `json:"consecutive_failures"` // 连续失败次数
	DirectDomains       int       `json:"direct_domains"`       // 直连名单条数（0 = 全部走上游）
	CheckedAt           time.Time `json:"checked_at"`
}

// Checker 周期自检器。
type Checker struct {
	dialer        N.Dialer
	server        string
	expectIP      string
	directDomains int
	client        *http.Client

	mu   sync.RWMutex
	last *Health
}

// NewChecker dialer 须是数据面实际使用的同一个上游 dialer（测的就是用户流量的路径）。
func NewChecker(dialer N.Dialer, server, expectIP string, directDomains int) *Checker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
		DisableKeepAlives:   true, // 每次自检都真实走一遍上游握手
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Checker{
		dialer:        dialer,
		server:        server,
		expectIP:      strings.TrimSpace(expectIP),
		directDomains: directDomains,
		client:        &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}
}

// Last 最近一次自检结果；尚未自检返回 nil（心跳 omitempty 不上报）。
func (c *Checker) Last() *Health {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.last == nil {
		return nil
	}
	h := *c.last
	return &h
}

// Run 立即自检一次，之后每 interval 一次，直到 ctx 结束。
func (c *Checker) Run(ctx context.Context, interval time.Duration) {
	c.CheckOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.CheckOnce(ctx)
		}
	}
}

// CheckOnce 执行一次自检并记录结果。
func (c *Checker) CheckOnce(ctx context.Context) *Health {
	h := &Health{
		Enabled:       true,
		Server:        c.server,
		ExpectIP:      c.expectIP,
		DirectDomains: c.directDomains,
		CheckedAt:     time.Now().UTC(),
	}
	ip, err := c.fetchExitIP(ctx)
	switch {
	case err != nil:
		h.Error = err.Error()
	case c.expectIP != "" && ip != c.expectIP:
		h.ExitIP = ip
		h.Error = "exit ip mismatch"
	default:
		h.ExitIP = ip
		h.OK = true
	}

	c.mu.Lock()
	prev := c.last
	if !h.OK {
		if prev != nil {
			h.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		} else {
			h.ConsecutiveFailures = 1
		}
	}
	c.last = h
	c.mu.Unlock()

	// 只在状态翻转时打日志，避免每分钟刷屏；持续失败每 10 次提醒一次。
	switch {
	case h.OK && (prev == nil || !prev.OK):
		log.Printf("[upstream] OK: exit_ip=%s via %s", h.ExitIP, c.server)
	case !h.OK && (prev == nil || prev.OK || h.ConsecutiveFailures%10 == 0):
		log.Printf("[upstream] ALERT: %s (exit_ip=%q expect=%q, failures=%d) — user traffic via %s is failing closed or exiting unexpectedly",
			h.Error, h.ExitIP, c.expectIP, h.ConsecutiveFailures, c.server)
	}
	return h
}

func (c *Checker) fetchExitIP(ctx context.Context) (string, error) {
	var lastErr error
	for _, u := range echoURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		ip := strings.TrimSpace(string(body))
		if resp.StatusCode != http.StatusOK || net.ParseIP(ip) == nil {
			lastErr = &badEcho{url: u, status: resp.StatusCode}
			continue
		}
		return ip, nil
	}
	return "", lastErr
}

type badEcho struct {
	url    string
	status int
}

func (e *badEcho) Error() string {
	return "unexpected echo response from " + e.url + " (status " + http.StatusText(e.status) + ")"
}
