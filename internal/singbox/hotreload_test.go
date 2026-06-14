package singbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 验证 HotReloadClient 按 WP-1 契约发请求并解析响应。
func TestHotReloadClient_UpdateUsers_Success(t *testing.T) {
	var gotBody hotReloadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/hotreload/users" {
			t.Errorf("path = %s, want /hotreload/users", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HotReloadResponse{OK: true, UserCount: 2, BillingSync: true})
	}))
	defer srv.Close()

	c := NewHotReloadClient(strings.TrimPrefix(srv.URL, "http://"))
	resp, err := c.UpdateUsers([]string{"uuid-a", "uuid-b"})
	if err != nil {
		t.Fatalf("UpdateUsers err = %v", err)
	}
	if !resp.OK || resp.UserCount != 2 || !resp.BillingSync {
		t.Fatalf("resp = %+v", resp)
	}
	// 全量集语义：body 应带完整 uuid 列表。
	if len(gotBody.Users) != 2 || gotBody.Users[0].UUID != "uuid-a" || gotBody.Users[1].UUID != "uuid-b" {
		t.Fatalf("request body users = %+v", gotBody.Users)
	}
}

// 端点返回非 200 / ok=false 时 UpdateUsers 必须返回 error（调用方据此降级回 reload）。
func TestHotReloadClient_UpdateUsers_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(HotReloadResponse{OK: false, Error: "inbound_tag not found"})
	}))
	defer srv.Close()

	c := NewHotReloadClient(strings.TrimPrefix(srv.URL, "http://"))
	_, err := c.UpdateUsers([]string{"uuid-a"})
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

// 连接被拒（sing-box 没起 / 老二进制无端点）时必须返回 error → 降级回 reload。
func TestHotReloadClient_UpdateUsers_ConnRefused(t *testing.T) {
	// 指向一个没人监听的端口。
	c := NewHotReloadClient("127.0.0.1:1") // 端口 1 几乎不可能被占
	_, err := c.UpdateUsers([]string{"uuid-a"})
	if err == nil {
		t.Fatal("expected error on connection refused, got nil")
	}
}
