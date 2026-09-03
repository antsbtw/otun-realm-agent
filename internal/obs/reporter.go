package obs

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

// Identity 出口身份，注入到每个上报信封（§7.3.3）。
type Identity struct {
	NodeID  string
	RealmID string
	Region  string
}

// Reporter 是运营平面的第二个 reporter（§7.3.1），与计费 reporter 并行、互不影响。
//
// 最小可替换形态（§7.3.2）：HTTP POST JSON 到 OBS_ENDPOINT（私网，env 注入），
// 批量、可缓存重传（照搬计费 reporter 的 FlushCache 模式）。运营平面定了别的协议
// （gRPC/MQTT…），只换本实现，采集点与 schema 不变。
//
// endpoint 为空时进入【no-op】模式：只在本地 log，不外发——便于运营平面未就绪时先跑起来。
type Reporter struct {
	endpoint   string
	apiKey     string // Bearer token（与计费/配置同步同源，见 NewReporter）
	identity   Identity
	cacheDir   string
	httpClient *http.Client

	mu      sync.Mutex
	pending []Envelope // 批量缓冲（★有上限，见 maxPendingEnvelopes）
	dropped int        // 因缓冲满被丢弃的信封数（累计，Flush 时打日志并清零）
}

// maxPendingEnvelopes 是内存缓冲上限。
//
// ★为什么必须有上限：出口节点是 2 vCPU / 442 MB 的小机（2026-09-02 实测 jp-02/sg-02），
// agent 常驻仅 22–28 MB。若 OBS_ENDPOINT 不可达（网络分区/对端挂了），无界 append 会一直
// 涨到 OOM —— 观测组件把主链路打死，是绝不可接受的故障模式。
// 丢观测数据可接受（有 traffic_logs 等其他真源），丢 agent 不可接受。
//
// 取值依据：单节点峰值约 3000 事件/小时（24h 实测 1458 opens/h × open+close），
// 60s 窗口约 50 条 events → 一个 conn_lifecycle 信封。2000 个信封 ≈ 33 小时的量，
// 远超任何合理的对端不可用时长。
const maxPendingEnvelopes = 2000

// NewReporter 创建运营平面上报器。endpoint 空 → no-op 模式。
// apiKey 用作 Authorization: Bearer —— 与计费 reporter / 配置同步用的是同一把
// （agent 已持有，无需另发凭证）。collector 侧灰度期 fail-open，空 key 也能上报。
func NewReporter(endpoint, apiKey string, identity Identity, cacheDir string) *Reporter {
	os.MkdirAll(cacheDir, 0755)
	return &Reporter{
		endpoint: endpoint,
		apiKey:   apiKey,
		identity: identity,
		cacheDir: cacheDir,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Enabled 报告是否真正外发（endpoint 非空）。
func (r *Reporter) Enabled() bool { return r.endpoint != "" }

// Enqueue 把一个 payload 包成信封加入批量缓冲（不立即发送）。
func (r *Reporter) Enqueue(schema string, payload any) {
	env := Envelope{
		NodeID:  r.identity.NodeID,
		RealmID: r.identity.RealmID,
		Region:  r.identity.Region,
		TS:      nowRFC3339(),
		Schema:  schema,
		Payload: payload,
	}
	r.mu.Lock()
	// ★缓冲满 → 丢【最旧】的（新数据对排障更有价值：正在发生的故障比几小时前的重要）。
	if len(r.pending) >= maxPendingEnvelopes {
		r.pending = r.pending[1:]
		r.dropped++
	}
	r.pending = append(r.pending, env)
	r.mu.Unlock()
}

// Flush 批量推送缓冲的信封。失败则落盘缓存，下次重传。
func (r *Reporter) Flush() {
	r.mu.Lock()
	// ★丢弃必须留痕：静默丢观测数据会让人误以为「没有故障」，而不是「没有数据」。
	if r.dropped > 0 {
		log.Printf("[obs] WARN dropped %d envelopes (buffer cap %d reached; endpoint unreachable?)",
			r.dropped, maxPendingEnvelopes)
		r.dropped = 0
	}
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	batch := r.pending
	r.pending = nil
	r.mu.Unlock()

	if !r.Enabled() {
		// no-op 模式：本地 log 摘要，便于运营平面未就绪时观察。
		log.Printf("[obs] (no endpoint) dropping %d envelope(s); set OBS_ENDPOINT to enable", len(batch))
		return
	}

	if err := r.send(batch); err != nil {
		log.Printf("[obs] flush failed (%v), caching for retry", err)
		r.saveToCache(batch)
		return
	}

	// 推送成功后顺带尝试重传历史缓存。
	r.flushCache()
}

func (r *Reporter) send(batch []Envelope) error {
	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}

	req, err := http.NewRequest("POST", r.endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// ★带上 node token：conn_lifecycle 用于判断故障归属，被灌噪声后果重于其他 obs 数据。
	// collector 灰度期 fail-open（老 agent 无头也收），收紧后无头即 401。
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("obs endpoint error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (r *Reporter) saveToCache(batch []Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()

	filename := fmt.Sprintf("obs_%d.json", time.Now().UnixNano())
	path := filepath.Join(r.cacheDir, filename)
	data, err := json.Marshal(batch)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

// flushCache 重传历史缓存（成功一个删一个，失败即停）。
func (r *Reporter) flushCache() {
	r.mu.Lock()
	defer r.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(r.cacheDir, "obs_*.json"))
	if err != nil {
		return
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var batch []Envelope
		if err := json.Unmarshal(data, &batch); err != nil {
			os.Remove(file)
			continue
		}
		if err := r.send(batch); err != nil {
			return // 仍失败，留待下次
		}
		os.Remove(file)
	}
}
