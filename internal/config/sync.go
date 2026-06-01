package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Syncer 与 manager 说同一套 /api/node/* 方言（§3：复用力度②）。
// register / users / stats / heartbeat / connections —— manager 分不清对面是
// node-agent 还是 realm-agent，照样下发 users[]、收计费 stats、回 kick_users。
type Syncer struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// NewSyncer 创建配置同步器。
func NewSyncer(apiURL, apiKey string) *Syncer {
	return &Syncer{
		apiURL: apiURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegisterRequest 节点注册请求。realm 出口不靠 IP，靠 node_id+api_key（§5.1）。
type RegisterRequest struct {
	NodeID    string         `json:"node_id"`
	Version   string         `json:"version"`
	NodeKind  string         `json:"node_kind"`        // "realm"，便于 manager 走 realm 分支
	RealmID   string         `json:"realm_id"`         // 出口的 realm slot 标识
	Region    string         `json:"region,omitempty"` // 首启引导值，空则后端 GeoIP 判定
	Protocols map[string]any `json:"protocols"`        // 只有一个 hy2+realm inbound
}

// Register 向 manager 注册 realm 出口。
func (s *Syncer) Register(nodeID, realmID, region string, hy2Port int) error {
	url := fmt.Sprintf("%s/api/node/register", s.apiURL)

	req := RegisterRequest{
		NodeID:   nodeID,
		Version:  "1.0.0",
		NodeKind: "realm",
		RealmID:  realmID,
		Region:   region,
		Protocols: map[string]any{
			"hysteria2_realm": map[string]any{
				"port": hy2Port,
			},
		},
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

// Heartbeat 发送心跳（计费通路）。
func (s *Syncer) Heartbeat(req *HeartbeatRequest) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/node/heartbeat", s.apiURL)

	var resp HeartbeatResponse
	if err := s.postJSON(url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportConnections 上报活跃连接（计费通路）。
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
