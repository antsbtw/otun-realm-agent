package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthServer 提供健康检查接口。
type HealthServer struct {
	startTime   time.Time
	isHealthy   func() bool
	realmActive func() bool // realm 是否已生效（manager 下发/缓存的 realm 配置已应用）
}

// NewHealthServer 创建健康检查服务。
// realmActive 可为 nil（则 health 不输出该字段）。
func NewHealthServer(isHealthy func() bool, realmActive func() bool) *HealthServer {
	return &HealthServer{
		startTime:   time.Now(),
		isHealthy:   isHealthy,
		realmActive: realmActive,
	}
}

// HandleHealth 处理健康检查请求。
func (s *HealthServer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	code := http.StatusOK
	if !s.isHealthy() {
		status = "unhealthy"
		code = http.StatusServiceUnavailable
	}

	body := map[string]any{
		"status":  status,
		"uptime":  time.Since(s.startTime).String(),
		"service": "otun-realm-agent",
		"version": "1.0.0",
	}
	// realm_active=false 表示进程在跑、但 sing-box 还是空配置（manager 未下发 realm）。
	// 让运维一眼区分"agent 活着" vs "realm 真生效了"。
	if s.realmActive != nil {
		body["realm_active"] = s.realmActive()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// HandleReady 处理就绪检查请求。
func (s *HealthServer) HandleReady(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
