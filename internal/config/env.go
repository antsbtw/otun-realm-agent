package config

import (
	"os"
	"strconv"
	"time"
)

// LoadFromEnv 从环境变量加载 realm-agent 配置。
// 环境变量映射见 OTUN_REALM_AGENT_DESIGN.md §7.7.1：
//
//	--realm-id          → REALM_ID
//	--realm-server-url  → REALM_SERVER_URL
//	--region            → REALM_REGION
//	--hy2-port          → HY2_PORT
//	并固定 MANAGEMENT_MODE=remote（realm-agent 无本地/混合模式）。
func LoadFromEnv() *AgentConfig {
	return &AgentConfig{
		APIURL:        getEnv("OTUN_API_URL", "https://otun-manager.situstechnologies.com"),
		NodeAPIKey:    getEnv("NODE_API_KEY", ""),
		NodeID:        getEnv("NODE_ID", "realm-default"),
		SyncInterval:  getDurationEnv("SYNC_INTERVAL", 60) * time.Second,
		StatsInterval: getDurationEnv("STATS_INTERVAL", 60) * time.Second,
		RealmInterval: getDurationEnv("REALM_INTERVAL", 45) * time.Second, // §7.9 30-60s

		RealmID:        getEnv("REALM_ID", ""),
		RealmServerURL: getEnv("REALM_SERVER_URL", "https://situstechnologies.com/realm"),
		Region:         getEnv("REALM_REGION", ""),
		HY2Port:        getIntEnv("HY2_PORT", 51820),

		SingboxBin:    getEnv("SINGBOX_BIN", "/usr/local/bin/sing-box"),
		SingboxConfig: getEnv("SINGBOX_CONFIG", "/etc/sing-box/config.json"),
		LogLevel:      getEnv("LOG_LEVEL", "info"),

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
