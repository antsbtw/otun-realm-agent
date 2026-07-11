package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Syncer 与 manager/fleet 说同一套 /api/node/* 方言（§3：复用力度②）。
// ★Batch 5 拆两个 URL（渐进迁移，零回归）：
//   - apiURL   = otun-manager：用户下发拉取（FetchUsers）+ 计费上报（ReportConnections 喂 kick/quota）。
//   - fleetURL = fleet-manager：节点纳管 register + heartbeat（liveness/实况直连 fleet，§7.1 b）。
// ★fleetURL 空 → 回退用 apiURL（不传 --fleet-url = 旧行为，register/heartbeat 仍打 otun，零回归）。
// online 判定共库同一 nodes.last_heartbeat：心跳打到 fleet 后 fleet 写同列 → otun 与 fleet 都读到，不断链。
type Syncer struct {
	apiURL   string // otun-manager（用户下发 + 计费）
	fleetURL string // fleet-manager（纳管 register/heartbeat）；空则回退 apiURL
	apiKey   string
	// buildVersion 二进制自身版本（CI ldflags 注入 main.buildVersion 后传入）。
	// ★U0：register/heartbeat 都带它上报 → fleet nodes.version = 真实版本，
	// 升级编排"谁升了/谁没升/谁失败了"全靠这个数据源。与 manager 下发的配置投影
	// 哈希（UsersResponse.Version/UserVersion/RealmVersion）是两回事，不要混。
	buildVersion string
	httpClient   *http.Client
}

// NewSyncer 创建配置同步器。fleetURL 空 → register/heartbeat 回退到 apiURL（向后兼容旧行为）。
// buildVersion 空 → "dev"（本地/未注入构建，别让 nodes.version 被清成空串）。
func NewSyncer(apiURL, fleetURL, apiKey, buildVersion string) *Syncer {
	if fleetURL == "" {
		fleetURL = apiURL // ★渐进迁移零回归：不配 fleet 则纳管仍走 otun-manager（旧路径）。
	}
	if buildVersion == "" {
		buildVersion = "dev"
	}
	return &Syncer{
		apiURL:       apiURL,
		fleetURL:     fleetURL,
		apiKey:       apiKey,
		buildVersion: buildVersion,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegisterRequest 节点注册请求。realm 出口不靠 IP，靠 node_id+api_key（§5.1）。
// ★六协议实况上报（总设计 §6 / 施工单 §3.6）：Protocols 字段带"实际启用协议 → 本地端口"，
// 供 manager 存/运维查这节点跑了哪些协议、端口多少（端口对外无意义、只作运维可见性）。
type RegisterRequest struct {
	NodeID    string         `json:"node_id"`
	Version   string         `json:"version"`
	NodeKind  string         `json:"node_kind"`        // "realm"，便于 manager 走 realm 分支
	RealmID   string         `json:"realm_id"`         // 出口的 realm slot 标识（base）
	Region    string         `json:"region,omitempty"` // 首启引导值，空则后端 GeoIP 判定
	Protocols map[string]any `json:"protocols"`        // 实况：{ "<proto>": {"port": <localPort>} }
	// RendezvousHealth 会合面注册健康自检快照（1c）。注册常发生在 rebuild 刚完成、
	// 会合面注册尚未落定时，携带的是【上一次】自检快照；nil = 尚无快照。
	RendezvousHealth *RendezvousHealth `json:"rendezvous_health,omitempty"`
}

// Register 向 manager 注册 realm 出口，上报实际启用协议 + 各本地端口。
// protocols 为空（首启还没起 node）时上报空实况，manager 侧照常记录节点存在。
// rendezvous 可为 nil（尚无自检快照）。
func (s *Syncer) Register(nodeID, realmID, region string, protocols []string, rendezvous *RendezvousHealth) error {
	// ★Batch 5：注册纳管直连 fleet（fleetURL；空则回退 apiURL=otun，零回归）。
	url := fmt.Sprintf("%s/api/node/register", s.fleetURL)

	protoMap := make(map[string]any, len(protocols))
	for _, proto := range protocols {
		protoMap[proto] = map[string]any{
			"port": LocalPort(proto),
		}
	}

	req := RegisterRequest{
		NodeID:           nodeID,
		Version:          s.buildVersion,
		NodeKind:         "realm",
		RealmID:          realmID,
		Region:           region,
		Protocols:        protoMap,
		RendezvousHealth: rendezvous,
	}

	return s.postJSON(url, req, nil)
}

// FetchUsers 从 manager 获取 realm 出口的扩展响应（含 realm/dns 块，§7.2.2）。
func (s *Syncer) FetchUsers() (*UsersResponse, error) {
	url := fmt.Sprintf("%s/api/node/users", s.apiURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result UsersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// Heartbeat 发送心跳（liveness/实况通路）。★Batch 5：直连 fleet（fleetURL；空则回退 apiURL，零回归）。
// ★只搬 liveness：心跳体是节点级 NodeLoad，不含 per-user 计费字节；kick_users 决策仍由 otun 经
//   ReportConnections 消费（该通路仍指向 apiURL）。fleet 心跳返空 kick_users → agent 无操作，等价旧无 kick。
func (s *Syncer) Heartbeat(req *HeartbeatRequest) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/node/heartbeat", s.fleetURL)

	// ★U0：心跳也带二进制版本（register 只在启动打一次；心跳带上后 fleet 的版本视图
	// 更实时，且覆盖"register 失败但心跳成功"的边界）。统一在这里盖章，调用方不用管。
	req.Version = s.buildVersion

	var resp HeartbeatResponse
	if err := s.postJSON(url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportConnections 上报活跃连接（计费/踢人通路）。★Batch 5【不搬】：仍指向 otun-manager（apiURL）。
// 原因：otun 的 /api/node/connections 消费 per-conn 数据做 kick 决策（设备指纹/过期/配额超限），
// 属计费/enforcement 消费逻辑；搬到 fleet 需在 fleet 复刻用户查库+kick 逻辑，本 Batch 不搬（宁可少搬别搬断计费）。
func (s *Syncer) ReportConnections(report *ConnectionsReport) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/node/connections", s.apiURL)

	var resp HeartbeatResponse
	if err := s.postJSON(url, report, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// postJSON 发送 JSON POST 请求。
func (s *Syncer) postJSON(url string, reqBody any, respBody any) error {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
