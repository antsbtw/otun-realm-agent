package singbox

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// sing-box V2Ray API 端口（用于"端口可用"探测）。
	apiPort = "127.0.0.1:10085"
	// 最大重启尝试次数。
	maxRestartAttempts = 5
	// 端口检查最大等待时间。
	maxPortWaitTime = 30 * time.Second
)

// Manager 管理 sing-box 进程。
//
// ★§7.0 硬事实：Reload 不是 SIGHUP 热重载，而是 stop(SIGTERM)+start 整进程重启，
// 每次 reload 会断掉该出口上所有活跃 QUIC 连接（用户重连）。调用方（§7.2 version reload /
// §7.9 按需重注册）必须据此把 reload 控制在"真正需要"时才触发。
type Manager struct {
	binPath       string
	configPath    string
	cmd           *exec.Cmd
	mu            sync.Mutex
	running       bool
	restartCount  int
	lastRestartAt time.Time
	stopRequested bool

	// reload 计数（§7.5 realm_health 上报：频繁 reload = 出口在抖动）。
	reloadCount int
}

// NewManager 创建进程管理器。
func NewManager(binPath, configPath string) *Manager {
	return &Manager{
		binPath:    binPath,
		configPath: configPath,
	}
}

// Start 启动 sing-box 进程。
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

func (m *Manager) startLocked() error {
	if m.running {
		return fmt.Errorf("sing-box is already running")
	}

	if _, err := os.Stat(m.configPath); os.IsNotExist(err) {
		return fmt.Errorf("config file not found: %s", m.configPath)
	}
	if _, err := os.Stat(m.binPath); os.IsNotExist(err) {
		return fmt.Errorf("sing-box binary not found: %s", m.binPath)
	}

	if err := m.waitForPortAvailable(); err != nil {
		return fmt.Errorf("port not available: %w", err)
	}

	m.cmd = exec.Command(m.binPath, "run", "-c", m.configPath)
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	m.running = true
	m.stopRequested = false
	log.Printf("sing-box started with PID %d", m.cmd.Process.Pid)

	go m.monitor()
	return nil
}

func (m *Manager) waitForPortAvailable() error {
	startTime := time.Now()
	for {
		if time.Since(startTime) > maxPortWaitTime {
			return fmt.Errorf("timeout waiting for port %s to be available", apiPort)
		}
		conn, err := net.DialTimeout("tcp", apiPort, 100*time.Millisecond)
		if err != nil {
			return nil // 端口未被占用，可以启动
		}
		conn.Close()
		log.Printf("Port %s is still in use, waiting...", apiPort)
		time.Sleep(time.Second)
	}
}

// Stop 停止 sing-box 进程。
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked()
}

func (m *Manager) stopLocked() error {
	if !m.running || m.cmd == nil || m.cmd.Process == nil {
		m.running = false
		return nil
	}

	m.stopRequested = true
	log.Println("Stopping sing-box...")

	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("SIGTERM failed: %v, trying SIGKILL", err)
		m.cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() { done <- m.cmd.Wait() }()

	select {
	case <-done:
		log.Println("sing-box stopped gracefully")
	case <-time.After(5 * time.Second):
		m.cmd.Process.Kill()
		select {
		case <-done:
			log.Println("sing-box force killed")
		case <-time.After(2 * time.Second):
			log.Println("sing-box kill timeout, process may be zombie")
		}
	}

	m.running = false
	m.cmd = nil
	time.Sleep(500 * time.Millisecond) // 等待端口释放
	return nil
}

// Reload 重载配置（★整进程重启，断所有连接，见 §7.0）。
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running {
		return fmt.Errorf("sing-box is not running")
	}

	log.Println("Reloading sing-box config (full process restart, drops all connections)...")

	if err := m.stopLocked(); err != nil {
		return fmt.Errorf("stop sing-box for reload: %w", err)
	}
	m.restartCount = 0 // 主动 reload，不是崩溃重启

	if err := m.startLocked(); err != nil {
		return fmt.Errorf("start sing-box after reload: %w", err)
	}
	m.reloadCount++
	return nil
}

// IsRunning 检查是否运行中。
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// ReloadCount 返回累计 reload 次数（§7.5 健康指标）。
func (m *Manager) ReloadCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadCount
}

func (m *Manager) monitor() {
	if m.cmd == nil {
		return
	}

	cmd := m.cmd
	err := cmd.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopRequested {
		m.running = false
		return
	}
	if m.cmd != cmd {
		log.Printf("monitor: cmd has been replaced, skipping restart logic")
		return
	}

	wasRunning := m.running
	m.running = false
	if !wasRunning {
		return
	}

	log.Printf("sing-box exited unexpectedly: %v", err)

	if time.Since(m.lastRestartAt) < 10*time.Second {
		m.restartCount++
	} else {
		m.restartCount = 1
	}
	m.lastRestartAt = time.Now()

	if m.restartCount > maxRestartAttempts {
		log.Printf("sing-box has crashed %d times in quick succession, giving up auto-restart", m.restartCount)
		log.Println("Manual intervention required. Check logs and restart the service.")
		return
	}

	backoffSeconds := 1 << m.restartCount
	if backoffSeconds > 32 {
		backoffSeconds = 32
	}
	log.Printf("Attempting restart %d/%d in %d seconds...", m.restartCount, maxRestartAttempts, backoffSeconds)

	m.mu.Unlock()
	time.Sleep(time.Duration(backoffSeconds) * time.Second)
	m.mu.Lock()

	if m.stopRequested {
		return
	}
	if err := m.startLocked(); err != nil {
		log.Printf("Failed to restart sing-box: %v", err)
	} else {
		log.Println("sing-box restarted successfully")
	}
}

// CheckConfig 验证配置文件（sing-box check -c）。
func (m *Manager) CheckConfig() error {
	cmd := exec.Command(m.binPath, "check", "-c", m.configPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("config check failed: %s", string(output))
	}
	return nil
}
