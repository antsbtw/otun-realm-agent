package quota

import (
	"log"
	"sync"
	"time"

	"otun-realm-agent/internal/config"
)

// UserQuota 存储用户限额信息。
type UserQuota struct {
	UUID           string
	TrafficLimit   int64 // 0 = 无限制
	TrafficUsed    int64
	SessionTraffic int64
	ExpireAt       *time.Time
	Enabled        bool
}

// Monitor 监控用户流量限额和过期。照搬 otun-node-agent。
type Monitor struct {
	users    map[string]*UserQuota
	mu       sync.RWMutex
	onRemove func(uuid, reason string)
}

// NewMonitor 创建限额监控器。
func NewMonitor(onRemove func(uuid, reason string)) *Monitor {
	return &Monitor{
		users:    make(map[string]*UserQuota),
		onRemove: onRemove,
	}
}

// UpdateUsers 更新用户列表（从 manager 同步后调用）。
func (m *Monitor) UpdateUsers(users []config.User) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newUsers := make(map[string]*UserQuota)
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		sessionTraffic := int64(0)
		if existing, ok := m.users[u.UUID]; ok {
			sessionTraffic = existing.SessionTraffic
		}
		newUsers[u.UUID] = &UserQuota{
			UUID:           u.UUID,
			TrafficLimit:   u.TrafficLimit,
			TrafficUsed:    u.TrafficUsed,
			SessionTraffic: sessionTraffic,
			ExpireAt:       u.ExpireAt,
			Enabled:        u.Enabled,
		}
	}
	m.users = newUsers
	log.Printf("Quota monitor updated: %d active users", len(newUsers))
}

// CheckUser 检查用户是否可以继续使用。
func (m *Monitor) CheckUser(uuid string, additionalTraffic int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	user, ok := m.users[uuid]
	if !ok {
		return false
	}
	user.SessionTraffic += additionalTraffic

	if user.ExpireAt != nil && time.Now().After(*user.ExpireAt) {
		log.Printf("User %s expired", uuid)
		delete(m.users, uuid)
		if m.onRemove != nil {
			go m.onRemove(uuid, "expired")
		}
		return false
	}

	if user.TrafficLimit > 0 {
		totalUsed := user.TrafficUsed + user.SessionTraffic
		if totalUsed >= user.TrafficLimit {
			log.Printf("User %s quota exceeded: %d/%d bytes", uuid, totalUsed, user.TrafficLimit)
			delete(m.users, uuid)
			if m.onRemove != nil {
				go m.onRemove(uuid, "quota_exceeded")
			}
			return false
		}
	}
	return true
}

// ResetSessionTraffic 重置会话流量（上报后调用）。
func (m *Monitor) ResetSessionTraffic() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range m.users {
		user.SessionTraffic = 0
	}
}

// CheckAllUsers 检查所有用户的过期和流量限额状态（定时调用）。
func (m *Monitor) CheckAllUsers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for uuid, user := range m.users {
		if user.ExpireAt != nil && now.After(*user.ExpireAt) {
			log.Printf("User %s expired (periodic check)", uuid)
			delete(m.users, uuid)
			if m.onRemove != nil {
				go m.onRemove(uuid, "expired")
			}
			continue
		}
		if user.TrafficLimit > 0 {
			totalUsed := user.TrafficUsed + user.SessionTraffic
			if totalUsed >= user.TrafficLimit {
				log.Printf("User %s quota exceeded (periodic check): %d/%d bytes", uuid, totalUsed, user.TrafficLimit)
				delete(m.users, uuid)
				if m.onRemove != nil {
					go m.onRemove(uuid, "quota_exceeded")
				}
			}
		}
	}
}

// GetUserCount 获取当前活跃用户数。
func (m *Monitor) GetUserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}
