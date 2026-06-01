package stats

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemLoad 系统负载信息。
type SystemLoad struct {
	CPUPercent    float64
	MemoryPercent float64
}

// GetSystemLoad 获取系统负载（Linux 专用）。
func GetSystemLoad() SystemLoad {
	return SystemLoad{
		CPUPercent:    getCPUUsage(),
		MemoryPercent: getMemoryUsage(),
	}
}

func getCPUUsage() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	numCPU := float64(runtime.NumCPU())
	percent := (load1 / numCPU) * 100
	if percent > 100 {
		percent = 100
	}
	return percent
}

func getMemoryUsage() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()

	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memTotal = value
		case "MemAvailable:":
			memAvailable = value
		}
	}
	if memTotal == 0 {
		return 0
	}
	used := memTotal - memAvailable
	return float64(used) / float64(memTotal) * 100
}

// 出口公网 IPv4 缓存。
//
// §5.1：realm 出口【架构上不需要静态公网 IP】，地址面走 STUN+L3 动态发现。
// 但 §5.2 自动判定 region + §7.3.5 出口落地可观测，需 agent【额外主动查一次出口 IP】
// （echo-ip 服务）。这是 realm 比普通节点多做的一步，仅作可观测、不参与连接链路。
var (
	cachedIPv4     string
	ipv4Mutex      sync.RWMutex
	ipv4LastUpdate time.Time
	ipv4CacheTTL   = 5 * time.Minute
)

// GetPublicIPv4 获取出口公网 IPv4（带缓存，只返回 IPv4）。
func GetPublicIPv4() string {
	ipv4Mutex.RLock()
	if cachedIPv4 != "" && time.Since(ipv4LastUpdate) < ipv4CacheTTL {
		ip := cachedIPv4
		ipv4Mutex.RUnlock()
		return ip
	}
	ipv4Mutex.RUnlock()

	ip := fetchPublicIPv4()
	if ip != "" {
		ipv4Mutex.Lock()
		cachedIPv4 = ip
		ipv4LastUpdate = time.Now()
		ipv4Mutex.Unlock()
	}
	return ip
}

func fetchPublicIPv4() string {
	services := []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://v4.ident.me",
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, url := range services {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if isIPv4(ip) {
			return ip
		}
	}
	return ""
}

func isIPv4(ip string) bool {
	return len(ip) >= 7 && len(ip) <= 15 && strings.Contains(ip, ".") && !strings.Contains(ip, ":")
}
