package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Cache 管理本地配置缓存（离线容错）。照搬 otun-node-agent。
type Cache struct {
	cacheDir string
}

// NewCache 创建缓存管理器。
func NewCache(cacheDir string) *Cache {
	os.MkdirAll(cacheDir, 0755)
	return &Cache{cacheDir: cacheDir}
}

// SaveUsers 保存下发响应（含 realm/dns 块）到本地。
func (c *Cache) SaveUsers(resp *UsersResponse) error {
	path := filepath.Join(c.cacheDir, "users.json")
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// LoadUsers 从本地加载下发响应。
func (c *Cache) LoadUsers() (*UsersResponse, error) {
	path := filepath.Join(c.cacheDir, "users.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cache: %w", err)
	}

	var resp UsersResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal cache: %w", err)
	}
	return &resp, nil
}

// HasCache 检查是否有缓存。
func (c *Cache) HasCache() bool {
	path := filepath.Join(c.cacheDir, "users.json")
	_, err := os.Stat(path)
	return err == nil
}
