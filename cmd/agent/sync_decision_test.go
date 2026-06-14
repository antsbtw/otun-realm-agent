package main

import (
	"testing"

	"otun-realm-agent/internal/config"
)

// WP-3 决策表单测：双 version 下 reload / 热更 / 不动 的分流，外加老 manager 兼容路径。
// 对应 REALM_HOT_RELOAD_WP3.md 的「reload vs 热更」决策表。
func TestDecideSyncAction(t *testing.T) {
	realm := &config.RealmBlock{RealmID: "iptv-cn-sh"}

	tests := []struct {
		name string
		prev syncState
		resp *config.UsersResponse
		want syncAction
	}{
		{
			name: "首次激活：本地空 + 下发带 realm → reload",
			prev: syncState{realmActive: false},
			resp: &config.UsersResponse{Version: "v1", UserVersion: "u1", RealmVersion: "r1", Realm: realm},
			want: actionReload,
		},
		{
			name: "仅 user_version 变（realm 不变，已激活）→ 热更",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2", UserVersion: "u2", RealmVersion: "r1", Realm: realm},
			want: actionHotReload,
		},
		{
			name: "realm_version 变（user 也变）→ reload 优先",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v3", UserVersion: "u2", RealmVersion: "r2", Realm: realm},
			want: actionReload,
		},
		{
			name: "realm_version 变（user 不变，如 token/stun）→ reload",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v4", UserVersion: "u1", RealmVersion: "r2", Realm: realm},
			want: actionReload,
		},
		{
			name: "两 version 都不变 → 不动",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v1", UserVersion: "u1", RealmVersion: "r1", Realm: realm},
			want: actionNone,
		},
		{
			name: "老 manager（无双 version），顶层 version 不变 → 不动",
			prev: syncState{version: "v1", realmActive: true},
			resp: &config.UsersResponse{Version: "v1"},
			want: actionNone,
		},
		{
			name: "老 manager（无双 version），顶层 version 变 → reload（安全侧，不冒险热更）",
			prev: syncState{version: "v1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2"},
			want: actionReload,
		},
		{
			name: "只有 user_version 字段缺失（半残响应）→ 当老 manager 处理，version 变则 reload",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2", RealmVersion: "r1"},
			want: actionReload,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSyncAction(tc.prev, tc.resp)
			if got != tc.want {
				t.Fatalf("decideSyncAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// diffRemovedUsers 必须只报"上次在、本次不在"的 UUID，供热更后主动踢连接。
func TestDiffRemovedUsers(t *testing.T) {
	a := &Agent{currentUserSet: uuidSet([]string{"a", "b", "c"})}

	removed := a.diffRemovedUsers([]string{"a", "c", "d"}) // b 掉出，d 新增
	if len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("removed = %v, want [b]", removed)
	}

	// 纯新增不报 removed。
	if r := a.diffRemovedUsers([]string{"a", "b", "c", "e"}); len(r) != 0 {
		t.Fatalf("pure-add removed = %v, want empty", r)
	}
}

// enabledUUIDs 只取 Enabled 用户（对齐 generator 的 hy2 users 过滤）。
func TestEnabledUUIDs(t *testing.T) {
	users := []config.User{
		{UUID: "a", Enabled: true},
		{UUID: "b", Enabled: false},
		{UUID: "c", Enabled: true},
	}
	got := enabledUUIDs(users)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("enabledUUIDs = %v, want [a c]", got)
	}
}
