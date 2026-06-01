package singbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ConnectionManager 通过 clash_api 管理 sing-box 连接（§7.4/§7.6 数据源）。
type ConnectionManager struct {
	apiAddr    string // clash_api external_controller，如 127.0.0.1:9090
	httpClient *http.Client
}

// NewConnectionManager 创建连接管理器。
func NewConnectionManager(apiAddr string) *ConnectionManager {
	return &ConnectionManager{
		apiAddr: apiAddr,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// ActiveConnection clash_api /connections 返回的连接信息。
// Metadata.Destination 是连接【元数据】（目的地 host:port），不是流量内容（§7.4.1）。
type ActiveConnection struct {
	ID       string `json:"id"`
	Metadata struct {
		User        string `json:"user"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
	} `json:"metadata"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
	Start    string `json:"start"`
}

// ConnectionsResponse clash_api connections API 响应。
type ConnectionsResponse struct {
	Connections []ActiveConnection `json:"connections"`
}

// GetActiveConnections 获取所有活跃连接。
func (m *ConnectionManager) GetActiveConnections() ([]ActiveConnection, error) {
	url := fmt.Sprintf("http://%s/connections", m.apiAddr)

	resp, err := m.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request connections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connections API returned %d", resp.StatusCode)
	}

	var result ConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode connections: %w", err)
	}
	return result.Connections, nil
}

// KickConnection 断开指定连接。
func (m *ConnectionManager) KickConnection(connID string) error {
	url := fmt.Sprintf("http://%s/connections/%s", m.apiAddr, connID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("kick API returned %d", resp.StatusCode)
	}
	return nil
}

// KickUser 断开指定用户的所有连接。
func (m *ConnectionManager) KickUser(userUUID string) (int, error) {
	connections, err := m.GetActiveConnections()
	if err != nil {
		return 0, err
	}

	kicked := 0
	for _, conn := range connections {
		if conn.Metadata.User == userUUID {
			if err := m.KickConnection(conn.ID); err != nil {
				continue
			}
			kicked++
		}
	}
	return kicked, nil
}
