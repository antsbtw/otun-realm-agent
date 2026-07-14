package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// ★P2 切换确认握手 wire 契约：heartbeat 的 applied_user_version 字段名是对 fleet 的契约
// （fleet 落 nodes.applied_user_version，manager evalUserReady 以此为信任锚），
// 字段名漂移 = fleet 静默丢弃 → ready 判定永远走 served 兜底。
func TestAppliedUserVersionWireShape(t *testing.T) {
	hb := HeartbeatRequest{
		NodeID:             "egress-de-02",
		AppliedUserVersion: "vdcc6fe6b9ec75648",
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"applied_user_version":"vdcc6fe6b9ec75648"`) {
		t.Fatalf("heartbeat JSON missing applied_user_version: %s", data)
	}

	// 空串（尚未成功应用过 / 节点全关）→ 字段整体省略，fleet 不覆盖旧值。
	empty, _ := json.Marshal(HeartbeatRequest{NodeID: "n"})
	if strings.Contains(string(empty), "applied_user_version") {
		t.Fatalf("empty applied_user_version must be omitted entirely: %s", empty)
	}
}
