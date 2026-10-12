package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 未启用上游代理的节点：心跳 load 序列化结果必须与改动前逐字节一致（不出现 upstream 键）。
func TestNodeLoadOmitsUpstreamWhenDisabled(t *testing.T) {
	b, err := json.Marshal(NodeLoad{CPUPercent: 1, ActiveUsers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "upstream") {
		t.Fatalf("disabled node leaks upstream key: %s", b)
	}
}

func TestUpstreamEnvDefaultsOff(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "")
	t.Setenv("UPSTREAM_DIRECT_DOMAINS", "")
	cfg := LoadFromEnv()
	if cfg.UpstreamProxy != "" || len(cfg.UpstreamDirectDomains) != 0 {
		t.Fatalf("upstream must default off, got %+v / %v", cfg.UpstreamProxy, cfg.UpstreamDirectDomains)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" googlevideo.com, nflxvideo.net ,,\ttiktokcdn.com ")
	want := []string{"googlevideo.com", "nflxvideo.net", "tiktokcdn.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
