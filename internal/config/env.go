package config

import (
	"os"
	"strconv"
	"time"
)

// LoadFromEnv 从环境变量加载 realm-agent 配置。
// 环境变量映射（install.sh 通过 systemd Environment= 注入）：
//
//	--realm-id → REALM_ID（base，各协议派生 <base>-<proto>）
//	--region   → REALM_REGION
//	--node-id  → NODE_ID
//	--api-url  → OTUN_API_URL
//	--api-key  → NODE_API_KEY
//	并固定 MANAGEMENT_MODE=remote（realm-agent 无本地/混合模式）。
//
// egress 库化 + 六协议后删除的 env：
//   - V2RAY_API_ADDR / CLASH_API_ADDR / HOT_RELOAD_ADDR / SINGBOX_BIN / SINGBOX_CONFIG
//     （不再 exec sing-box、无本地 IPC）。
//   - HY2_PORT / PROTOCOL：六协议本地端口用 config.LocalPort 常量约定，启用协议由
//     manager 下发（RealmBlock.Protocols），都不从 env 取。
//   - REALM_SERVER_URL：会合面 URL 由 manager 下发，不本地写死。
func LoadFromEnv() *AgentConfig {
	return &AgentConfig{
		APIURL:        getEnv("OTUN_API_URL", "https://otun-manager-v3.situstechnologies.com"),
		NodeAPIKey:    getEnv("NODE_API_KEY", ""),
		NodeID:        getEnv("NODE_ID", "realm-default"),
		SyncInterval:  getDurationEnv("SYNC_INTERVAL", 60) * time.Second,
		StatsInterval: getDurationEnv("STATS_INTERVAL", 60) * time.Second,
		RealmInterval: getDurationEnv("REALM_INTERVAL", 45) * time.Second, // §7.9 30-60s

		RealmID: getEnv("REALM_ID", ""),
		Region:  getEnv("REALM_REGION", ""),

		LogLevel: getEnv("LOG_LEVEL", "info"),

		OBSEndpoint: getEnv("OBS_ENDPOINT", ""), // §7.3.2：私网采集端点，空则不上报
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getIntEnv(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getDurationEnv(key string, defaultVal int) time.Duration {
	return time.Duration(getIntEnv(key, defaultVal))
}
