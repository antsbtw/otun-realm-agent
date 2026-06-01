package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthServer 提供健康检查接口。
type HealthServer struct {
	startTime time.Time
	isHealthy func() bool
}

// NewHealthServer 创建健康检查服务。
func NewHealthServer(isHealthy func() bool) *HealthServer {
	return &HealthServer{
		startTime: time.Now(),
		isHealthy: isHealthy,
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status":  status,
		"uptime":  time.Since(s.startTime).String(),
		"service": "otun-realm-agent",
		"version": "1.0.0",
	})
}

// HandleReady 处理就绪检查请求。
func (s *HealthServer) HandleReady(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
