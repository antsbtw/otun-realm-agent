package stats

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	statsService "github.com/v2fly/v2ray-core/v5/app/stats/command"
)

// Collector 从 sing-box V2Ray API (gRPC) 收集 per-user 流量统计。
type Collector struct {
	apiAddr string
}

// UserStats 用户流量统计。
type UserStats struct {
	Upload   int64
	Download int64
}

// NewCollector 创建统计收集器。
func NewCollector(apiAddr string) *Collector {
	return &Collector{apiAddr: apiAddr}
}

// Collect 收集所有用户流量统计。
//
// reset 控制是否清零计数（V2Ray Reset_ 标志）：
//   - 计费通路（→manager）用 reset=true，避免重复计费（§7.6）。
//   - 运营平面流量画像（traffic_profile）用 reset=false 做快照，【绝不复用同一次 reset=true 调用】，
//     否则会和计费抢着清零导致少计费（§7.3.4 warning）。
func (c *Collector) Collect(reset bool) (map[string]*UserStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, c.apiAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to grpc: %w", err)
	}
	defer conn.Close()

	client := statsService.NewStatsServiceClient(conn)

	resp, err := client.QueryStats(ctx, &statsService.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  reset,
	})
	if err != nil {
		return nil, fmt.Errorf("query stats: %w", err)
	}

	// V2Ray stats 格式: user>>>uuid>>>traffic>>>uplink 或 >>>downlink
	stats := make(map[string]*UserStats)
	for _, stat := range resp.Stat {
		parts := strings.Split(stat.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		uuid := parts[1]
		direction := parts[3]
		if _, ok := stats[uuid]; !ok {
			stats[uuid] = &UserStats{}
		}
		if direction == "uplink" {
			stats[uuid].Upload = stat.Value
		} else if direction == "downlink" {
			stats[uuid].Download = stat.Value
		}
	}
	return stats, nil
}
