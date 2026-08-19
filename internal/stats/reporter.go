package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
//
// ★计费不丢铁律（配套 egress Registry.RestoreStats）：调用方传进来的字节是
// CollectStats(true) 破坏性读的产物 —— 计数器已归零，这里是它们唯一的副本。
// 因此本函数的返回值必须让调用方能区分「已交接」与「已丢失」：
//   - nil        ：已上报成功，或已落盘待重传（两者都算交接完成，不可回滚，
//     否则重复计费）
//   - non-nil    ：上报失败【且】落盘也失败 —— 字节只在调用方内存里，
//     调用方必须 RestoreStats 把它们加回计数器，否则永久丢失。
//
// 历史缺陷：老实现在 send 失败时 `return r.saveToCache(...)`，落盘失败虽然返回了
// 错误，但调用方只打一行日志就 return，归零的字节随栈帧一起消失（at-most-once）。
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
	// 全零窗口无账可交接，回滚也没意义（RestoreStats 本身会跳过零行）。
	if len(report.Stats) == 0 {
		return nil
	}

	if err := r.send(&report); err != nil {
		// 上报失败 → 落盘兜底。落盘成功即交接完成（FlushCache 会重传）。
		if cerr := r.saveToCache(&report); cerr != nil {
			// 双重失败：字节无处存身，必须让调用方回滚。
			return fmt.Errorf("report failed (%v) and spool failed: %w", err, cerr)
		}
		log.Printf("[Billing] report failed (%v) → spooled %d entries to disk for retry", err, len(report.Stats))
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
