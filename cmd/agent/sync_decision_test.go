package main

import (
	"testing"

	"otun-realm-agent/internal/config"
)

// egress 库化后决策表：none / update / rebuild 三分支（reload 概念消失）。
//   - 仅 user_version 变 → update（node.UpdateUsers，不断连）
//   - realm_version 变 / realm 首次激活 / 老 manager 兜底 → rebuild（Close+New+Start，断该 node）
//   - 都不变 → none
func TestDecideSyncAction(t *testing.T) {
	realm := &config.RealmBlock{RealmID: "iptv-cn-sh"}

	tests := []struct {
		name string
		prev syncState
		resp *config.UsersResponse
		want syncAction
	}{
		{
			name: "首次激活：本地空 + 下发带 realm → rebuild",
			prev: syncState{realmActive: false},
			resp: &config.UsersResponse{Version: "v1", UserVersion: "u1", RealmVersion: "r1", Realm: realm},
			want: actionRebuild,
		},
		{
			name: "仅 user_version 变（realm 不变，已激活）→ update",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2", UserVersion: "u2", RealmVersion: "r1", Realm: realm},
			want: actionUpdate,
		},
		{
			name: "realm_version 变（user 也变）→ rebuild 优先",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v3", UserVersion: "u2", RealmVersion: "r2", Realm: realm},
			want: actionRebuild,
		},
		{
			name: "realm_version 变（user 不变，如 token/stun）→ rebuild",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v4", UserVersion: "u1", RealmVersion: "r2", Realm: realm},
			want: actionRebuild,
		},
		{
			name: "两 version 都不变 → none",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v1", UserVersion: "u1", RealmVersion: "r1", Realm: realm},
			want: actionNone,
		},
		{
			name: "老 manager（无双 version），顶层 version 不变 → none",
			prev: syncState{version: "v1", realmActive: true},
			resp: &config.UsersResponse{Version: "v1"},
			want: actionNone,
		},
		{
			name: "老 manager（无双 version），顶层 version 变 → rebuild（安全侧，不冒险 update）",
			prev: syncState{version: "v1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2"},
			want: actionRebuild,
		},
		{
			name: "只有 user_version 字段缺失（半残响应）→ 当老 manager 处理，version 变则 rebuild",
			prev: syncState{version: "v1", userVersion: "u1", realmVersion: "r1", realmActive: true},
			resp: &config.UsersResponse{Version: "v2", RealmVersion: "r1"},
			want: actionRebuild,
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
