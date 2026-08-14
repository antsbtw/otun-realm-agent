// Package relaymgr —— 兼职中继（moonlight）的节点侧对账器（A1b，
// RELAY_FLEET_BOUNDARY_DESIGN §5bis）。
//
// fleet 是期望态真源（relay_nodes.desired_state，经心跳响应下发）；本包把机器实况
// 对齐期望态：enabled → 确保二进制在位（缺失/校验不符则从 /dl/ 拉，凭据随心跳下发、
// 不常驻节点）+ systemd unit 内容规范 + 服务运行；disabled → 停用。
// 每次心跳前抓本机 /stats 快照捎带上报（§5quater：fleet 存活判据只认快照新鲜度）。
//
// 🔴 幂等收编：jp-02 有 bug 修复期手工装的 otun-relay unit——对账不是重装，
// 而是"内容不同才改写、改写才重启"；内容一致时零动作（探针复测不受扰动）。
//
// 🔴 边界：对账绝不触碰 egress 六协议路径（RESIDENTIAL_NODE_RELAY_DECISION §4 的
// 纯叠加口径）；中继进程独立 systemd，agent 自身升级/重启不影响在途中继会话。
package relaymgr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 路径用包级变量而非常量：单测重定向到临时目录（生产永不改写）。
var (
	binaryPath = "/usr/local/bin/otun-relay"
	unitPath   = "/etc/systemd/system/otun-relay.service"
	// statsURL 本机 /stats（中继 -health 固定 127.0.0.1:9100；jp-02 手工部署同款）。
	statsURL = "http://127.0.0.1:9100/stats"
)

const unitName = "otun-relay"

// Directive 是心跳响应里的 relay 期望态（fleet handleNodeHeartbeat 下发；nil = 本节点
// 未开兼职）。字段名与 fleet 侧 gin.H 逐字对齐。
type Directive struct {
	RelayID      string    `json:"relay_id"`
	Port         int       `json:"port"`
	DesiredState string    `json:"desired_state"` // enabled | disabled
	Download     *Download `json:"download,omitempty"`
}

// Download 是二进制分发凭据（fleet 从 /dl/manifest.json 取 sha，随心跳下发；
// token 不常驻节点——与 upgrade-plan 同一决策）。
type Download struct {
	URLBase string `json:"url_base"` // 如 https://portal.situstechnologies.com/dl
	Token   string `json:"token"`
	// Artifacts: "linux-amd64"/"linux-arm64" → {file, sha256}；agent 按 runtime.GOARCH 取。
	Artifacts map[string]Artifact `json:"artifacts"`
}

type Artifact struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Manager 串行执行对账（心跳每拍触发一次，幂等便宜：文件比对 + is-active 查询）。
type Manager struct {
	mu      sync.Mutex
	busy    bool
	lastErr string

	// run 可注入（单测替换掉真 systemctl/下载）。
	runCmd   func(name string, args ...string) (string, error)
	download func(d *Download, arch string) ([]byte, error)
}

func New() *Manager {
	m := &Manager{}
	m.runCmd = func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	m.download = fetchBinary
	return m
}

// Apply 按期望态对账。异步（不阻塞心跳循环）、串行（对账进行中则跳过本拍——
// 下一拍心跳会再触发，幂等收敛）。directive=nil 表示未开兼职：零动作（不主动
// 停别人手工装的服务——fleet 没下过 enabled 就没有"该由我停"的所有权）。
func (m *Manager) Apply(d *Directive) {
	if d == nil {
		return
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return
	}
	m.busy = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			m.busy = false
			m.mu.Unlock()
		}()
		if err := m.reconcile(d); err != nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			log.Printf("[relaymgr] reconcile: %v", err)
		} else {
			m.mu.Lock()
			m.lastErr = ""
			m.mu.Unlock()
		}
	}()
}

func (m *Manager) reconcile(d *Directive) error {
	switch d.DesiredState {
	case "disabled":
		return m.ensureStopped()
	case "enabled":
		return m.ensureRunning(d)
	default:
		return fmt.Errorf("unknown desired_state %q", d.DesiredState)
	}
}

func (m *Manager) ensureStopped() error {
	state, _ := m.runCmd("systemctl", "is-active", unitName)
	if state != "active" {
		return nil // 已停/从未装过：零动作
	}
	if _, err := m.runCmd("systemctl", "stop", unitName); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	_, _ = m.runCmd("systemctl", "disable", unitName)
	log.Printf("[relaymgr] relay stopped (desired_state=disabled)")
	return nil
}

func (m *Manager) ensureRunning(d *Directive) error {
	binChanged, err := m.ensureBinary(d)
	if err != nil {
		return err
	}
	unitChanged, err := m.ensureUnit(d.Port)
	if err != nil {
		return err
	}
	state, _ := m.runCmd("systemctl", "is-active", unitName)
	switch {
	case unitChanged || binChanged:
		// 内容变了才 restart（幂等收编：一致时绝不扰动在途会话）。
		if _, err := m.runCmd("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("daemon-reload: %w", err)
		}
		if out, err := m.runCmd("systemctl", "restart", unitName); err != nil {
			return fmt.Errorf("restart: %v (%s)", err, out)
		}
		_, _ = m.runCmd("systemctl", "enable", unitName)
		log.Printf("[relaymgr] relay (re)started: bin_changed=%v unit_changed=%v port=%d", binChanged, unitChanged, d.Port)
	case state != "active":
		if out, err := m.runCmd("systemctl", "start", unitName); err != nil {
			return fmt.Errorf("start: %v (%s)", err, out)
		}
		_, _ = m.runCmd("systemctl", "enable", unitName)
		log.Printf("[relaymgr] relay started (was %s)", state)
	}
	return nil
}

// ensureBinary 确保二进制在位且（有 sha 时）校验一致。返回是否发生了替换。
// 已在位且无法校验（fleet 没带 download / manifest 无本架构）→ 保持现状（收编手工部署）。
func (m *Manager) ensureBinary(d *Directive) (bool, error) {
	arch := "linux-" + runtime.GOARCH
	var want *Artifact
	if d.Download != nil {
		if a, ok := d.Download.Artifacts[arch]; ok {
			want = &a
		}
	}
	cur, statErr := fileSHA256(binaryPath)
	exists := statErr == nil

	if want == nil {
		if !exists {
			return false, fmt.Errorf("binary missing and no download info for %s (manifest lacks relay artifacts?)", arch)
		}
		return false, nil // 在位但没有校验源：收编现状（jp-02 手工部署路径）
	}
	if exists && strings.EqualFold(cur, want.SHA256) {
		return false, nil
	}
	// 缺失或版本不符 → 拉新（临时文件 + sha 校验 + 原子 rename，半截文件永不上位）。
	raw, err := m.download(d.Download, arch)
	if err != nil {
		if exists {
			// 下载失败但本地有可用二进制：降级保持现役版本，别把活着的服务搞死。
			log.Printf("[relaymgr] download failed (%v) — keeping existing binary %s", err, cur[:12])
			return false, nil
		}
		return false, fmt.Errorf("download: %w", err)
	}
	got := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want.SHA256) {
		return false, fmt.Errorf("sha mismatch: got %x want %s", got[:8], want.SHA256[:16])
	}
	tmp := filepath.Dir(binaryPath) + "/.otun-relay.tmp"
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return false, fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, binaryPath); err != nil {
		return false, fmt.Errorf("rename: %w", err)
	}
	log.Printf("[relaymgr] binary installed sha=%s", want.SHA256[:12])
	return true, nil
}

// UnitContent 生成规范 unit（导出供单测断言形状）。与 jp-02 手工版语义一致：
// User=nobody（中继无密钥无状态，>1024 端口无需 root）、-health 固定本机 9100。
func UnitContent(port int) string {
	return fmt.Sprintf(`[Unit]
Description=OTun Relay (dumb UDP byte pipe for realm fallback)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -listen :%d -health 127.0.0.1:9100
Restart=always
RestartSec=3
User=nobody
Group=nogroup

[Install]
WantedBy=multi-user.target
`, binaryPath, port)
}

// ensureUnit 内容驱动：与规范内容逐字节一致则零动作，否则改写（含收编手工 unit）。
func (m *Manager) ensureUnit(port int) (bool, error) {
	want := UnitContent(port)
	cur, err := os.ReadFile(unitPath)
	if err == nil && string(cur) == want {
		return false, nil
	}
	if err := os.WriteFile(unitPath, []byte(want), 0o644); err != nil {
		return false, fmt.Errorf("write unit: %w", err)
	}
	log.Printf("[relaymgr] unit rewritten (port=%d, adopted=%v)", port, err == nil)
	return true, nil
}

// CollectStats 抓本机中继 /stats 快照（心跳捎带；§5quater）。
// 中继未装/未起 → nil（fleet 侧快照过期自然判不健康，不需要错误通道）。
func (m *Manager) CollectStats() json.RawMessage {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(statsURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || !json.Valid(raw) {
		return nil
	}
	return raw
}

// LastError 最近一次对账错误（空=正常）。预留给 obs/日志排障。
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchBinary 从 /dl/ 拉中继二进制（Bearer token 随心跳下发，不落盘）。
func fetchBinary(d *Download, arch string) ([]byte, error) {
	a, ok := d.Artifacts[arch]
	if !ok {
		return nil, fmt.Errorf("no artifact for %s", arch)
	}
	req, err := http.NewRequest("GET", strings.TrimRight(d.URLBase, "/")+"/"+a.File, nil)
	if err != nil {
		return nil, err
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
