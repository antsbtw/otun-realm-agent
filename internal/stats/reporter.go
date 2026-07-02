package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatsEntry 单个用户统计。
type StatsEntry struct {
	UUID     string `json:"uuid"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}

// UserStats 用户流量统计（原 stats/collector.go；collector 已随 egress 库化删除，
// 类型保留在此供 Reporter 消费。字节数由 egress 库 CollectStats 提供）。
type UserStats struct {
	Upload   int64
	Download int64
}

// StatsReport 统计上报数据（计费通路 → manager /api/node/stats）。
type StatsReport struct {
	Timestamp time.Time    `json:"timestamp"`
	Stats     []StatsEntry `json:"stats"`
}

// Reporter 负责上报 per-user 计费流量（照搬 otun-node-agent，不动）。
type Reporter struct {
	apiURL     string
	apiKey     string
	cacheDir   string
	httpClient *http.Client
	mu         sync.Mutex
}

// NewReporter 创建统计上报器。
func NewReporter(apiURL, apiKey, cacheDir string) *Reporter {
	os.MkdirAll(cacheDir, 0755)
	return &Reporter{
		apiURL:   apiURL,
		apiKey:   apiKey,
		cacheDir: cacheDir,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Report 上报统计数据，失败则缓存到本地。
func (r *Reporter) Report(stats map[string]*UserStats) error {
	if len(stats) == 0 {
		return nil
	}

	report := StatsReport{
		Timestamp: time.Now().UTC(),
		Stats:     make([]StatsEntry, 0, len(stats)),
	}
	for uuid, s := range stats {
		if s.Upload > 0 || s.Download > 0 {
			report.Stats = append(report.Stats, StatsEntry{UUID: uuid, Upload: s.Upload, Download: s.Download})
		}
	}
	if len(report.Stats) == 0 {
		return nil
	}

	if err := r.send(&report); err != nil {
		return r.saveToCache(&report)
	}
	return nil
}

func (r *Reporter) send(report *StatsReport) error {
	url := fmt.Sprintf("%s/api/node/stats", r.apiURL)

	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (r *Reporter) saveToCache(report *StatsReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	filename := fmt.Sprintf("stats_%d.json", time.Now().UnixNano())
	path := filepath.Join(r.cacheDir, filename)

	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// FlushCache 上报缓存的统计数据。
func (r *Reporter) FlushCache() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(r.cacheDir, "stats_*.json"))
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var report StatsReport
		if err := json.Unmarshal(data, &report); err != nil {
			os.Remove(file)
			continue
		}
		if err := r.send(&report); err != nil {
			return err
		}
		os.Remove(file)
	}
	return nil
}

// GetCacheCount 获取缓存文件数量。
func (r *Reporter) GetCacheCount() int {
	files, _ := filepath.Glob(filepath.Join(r.cacheDir, "stats_*.json"))
	return len(files)
}
