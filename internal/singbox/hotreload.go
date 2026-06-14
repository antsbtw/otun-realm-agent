package singbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HotReloadClient 调用 WP-1 fork sing-box 暴露的本地热更端点。
//
// 契约（WP-1 已定，权威）：
//   - POST http://<addr>/hotreload/users，body {"users":[{"uuid":"<uuid>"},...]}
//   - 全量集语义（当前应在线的所有用户，非增量）、幂等、重复 uuid 自动去重、空 uuid 报 400。
//   - 成功 200 {"ok":true,"user_count":N,"billing_sync":true}
//     （billing_sync=true 表示 v2ray_api 计费集已同步，agent 调它即可，无需另外同步计费）。
//   - 失败 400(体坏/空uuid)/404(inbound_tag找不到)/405(非POST)/500(内部/不支持/panic)，
//     统一 {"ok":false,"error":"..."}。
//
// 该端点同时刷 hy2 认证 + v2ray_api 计费集 → 一次调用即可，加用户不断现有连接。
type HotReloadClient struct {
	addr       string // 热更端点地址，如 127.0.0.1:10086
	httpClient *http.Client
}

// NewHotReloadClient 创建热更客户端。
func NewHotReloadClient(addr string) *HotReloadClient {
	return &HotReloadClient{
		addr: addr,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// hotReloadUser 是请求体里的单个用户（仅 uuid，对齐 WP-1 契约）。
type hotReloadUser struct {
	UUID string `json:"uuid"`
}

// hotReloadRequest 是 POST /hotreload/users 的请求体。
type hotReloadRequest struct {
	Users []hotReloadUser `json:"users"`
}

// HotReloadResponse 是热更端点的响应（成功/失败统一解析）。
type HotReloadResponse struct {
	OK          bool   `json:"ok"`
	UserCount   int    `json:"user_count"`
	BillingSync bool   `json:"billing_sync"`
	Error       string `json:"error"`
}

// UpdateUsers 全量推送当前应在线的 user 集（uuid 列表）到运行中的 sing-box。
// 这是全量 set 语义：传完整列表，sing-box 据此替换 hy2 认证集 + v2ray_api 计费集。
// 不断现有连接（底层 QUIC session 不回查认证 map）。失败返回 error，调用方据此降级回 reload。
func (c *HotReloadClient) UpdateUsers(uuids []string) (*HotReloadResponse, error) {
	body := hotReloadRequest{Users: make([]hotReloadUser, 0, len(uuids))}
	for _, u := range uuids {
		body.Users = append(body.Users, hotReloadUser{UUID: u})
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal hotreload request: %w", err)
	}

	url := fmt.Sprintf("http://%s/hotreload/users", c.addr)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		// 连接被拒（sing-box 没起 / 老二进制无端点）→ 让调用方降级回 reload。
		return nil, fmt.Errorf("hotreload request failed: %w", err)
	}
	defer resp.Body.Close()

	var result HotReloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode hotreload response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK || !result.OK {
		return &result, fmt.Errorf("hotreload returned %d: %s", resp.StatusCode, result.Error)
	}
	return &result, nil
}
