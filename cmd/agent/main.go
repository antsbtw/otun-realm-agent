package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"otun-realm-agent/internal/api"
	"otun-realm-agent/internal/config"
	"otun-realm-agent/internal/obs"
	"otun-realm-agent/internal/quota"
	"otun-realm-agent/internal/realm"
	"otun-realm-agent/internal/singbox"
	"otun-realm-agent/internal/stats"
)

// Agent 是 realm-agent 主控制器。固定 remote 模式（§7.7：无本地/混合）。
type Agent struct {
	cfg       *config.AgentConfig
	syncer    *config.Syncer
	cache     *config.Cache
	selfsign  *config.SelfSigner
	generator *config.RealmGenerator
	manager   *singbox.Manager
	connMgr   *singbox.ConnectionManager
	hotReload *singbox.HotReloadClient // WP-3：仅用户集变时走它（不 reload、不断连）
	monitor   *quota.Monitor

	// 计费通路（→manager，§7.3.1）。
	billCollector *stats.Collector
	billReporter  *stats.Reporter

	// 运营平面通路（→OBS_ENDPOINT，§7.3.1）。
	obsReporter  *obs.Reporter
	lifecycle    *obs.LifecycleTracker
	behavior     *obs.BehaviorAggregator
	probeTargets []obs.ProbeTarget

	// realm 重注册（§7.9）。
	registrar *realm.Registrar

	dataDir             string
	currentVersion      string              // 顶层合并 version（向后兼容；老 manager 无双 version 时仍用它判定）
	currentUserVersion  string              // WP-3：user 集投影哈希（仅它变 → 热更）
	currentRealmVersion string              // WP-3：realm+dns 投影哈希（它变 → reload）
	currentRealm        *config.RealmBlock  // 最近一次下发的 realm 块（供 §7.9 探测 server_url）
	realmActive         bool                // 是否已用 manager 下发/缓存的 realm 配置生成过 sing-box 配置
	currentUserSet      map[string]struct{} // 最近一次应在线的 user UUID 集（用于热更删人时算出谁掉出→主动踢）
	mu                  sync.RWMutex
}

// isRealmActive 线程安全读取 realm 是否已生效（供 health 端点）。
func (a *Agent) isRealmActive() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.realmActive
}

// logDegraded 打一条醒目的降级提示，说清"为什么 realm 没生效"，
// 避免运维从一堆 522 / grpc deadline 报错里自己推断（manager 不通时的体验改进）。
func (a *Agent) logDegraded(reason string) {
	log.Printf("================ REALM NOT ACTIVE (degraded) ================")
	log.Printf("  reason : %s", reason)
	log.Printf("  effect : sing-box is running an EMPTY config — no hy2/realm inbound,")
	log.Printf("           no v2ray_api. Expect 'grpc deadline' (stats) and nothing on")
	log.Printf("           the hy2 port until a realm config is applied.")
	log.Printf("  why    : realm_token/stun/sni/dns MUST be pushed by manager (design §4.1);")
	log.Printf("           install.sh deliberately does not carry them, so the agent cannot")
	log.Printf("           bootstrap a realm inbound on its own without a prior good sync.")
	log.Printf("  action : bring otun-manager online and ensure it serves this realm egress")
	log.Printf("           (node_kind=realm) via /api/node/users. Agent keeps retrying.")
	log.Printf("=============================================================")
}

func main() {
	log.Println("========================================")
	log.Println("  OTun Realm Agent v1.0.0")
	log.Println("========================================")

	cfg := config.LoadFromEnv()
	if cfg.NodeAPIKey == "" {
		log.Fatal("NODE_API_KEY is required")
	}
	if cfg.RealmID == "" {
		log.Println("Warning: REALM_ID not set; manager-issued realm block will drive registration")
	}

	log.Printf("Node ID:    %s", cfg.NodeID)
	log.Printf("Realm ID:   %s", cfg.RealmID)
	log.Printf("API URL:    %s", cfg.APIURL)
	log.Printf("HY2 Port:   %d", cfg.HY2Port)
	if cfg.OBSEndpoint == "" {
		log.Println("OBS_ENDPOINT not set — operational reporting runs in no-op mode (§7.3.2)")
	}

	agent, err := NewAgent(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutdown signal received...")
		cancel()
	}()

	agent.Run(ctx)
}

// NewAgent 创建 realm-agent 实例。
func NewAgent(cfg *config.AgentConfig) (*Agent, error) {
	dataDir := "./data"
	statsCache := "./data/stats"
	obsCache := "./data/obs"
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(statsCache, 0755)
	os.MkdirAll(obsCache, 0755)

	// §7.1.3 自签 iptv.local：无则生成、有则复用。
	selfsign := config.NewSelfSigner(dataDir, "")
	if err := selfsign.EnsureCert(); err != nil {
		return nil, err
	}

	connMgr := singbox.NewConnectionManager(config.ClashAPIAddr)

	agent := &Agent{
		cfg:           cfg,
		syncer:        config.NewSyncer(cfg.APIURL, cfg.NodeAPIKey),
		cache:         config.NewCache(dataDir),
		selfsign:      selfsign,
		generator:     config.NewRealmGenerator(cfg.HY2Port, selfsign.CertPath(), selfsign.KeyPath()),
		manager:       singbox.NewManager(cfg.SingboxBin, cfg.SingboxConfig),
		connMgr:       connMgr,
		hotReload:     singbox.NewHotReloadClient(config.HotReloadAddr),
		billCollector: stats.NewCollector(config.V2RayAPIAddr),
		billReporter:  stats.NewReporter(cfg.APIURL, cfg.NodeAPIKey, statsCache),
		obsReporter: obs.NewReporter(cfg.OBSEndpoint, obs.Identity{
			NodeID:  cfg.NodeID,
			RealmID: cfg.RealmID,
			Region:  cfg.Region,
		}, obsCache),
		lifecycle: obs.NewLifecycleTracker(),
		// §7.4.4：上线初期"只观测不处置"，阈值留 0（不触发 suspected_abuse）。
		behavior:     obs.NewBehaviorAggregator(300, obs.AbuseThresholds{}, nil),
		probeTargets: obs.DefaultProbeTargets(),
		registrar:    realm.NewRegistrar(),
		dataDir:      dataDir,
	}

	// 限额监控器（超限/过期 → 踢人，复用计费闭环）。
	agent.monitor = quota.NewMonitor(func(uuid, reason string) {
		log.Printf("User quota exceeded: %s (%s), kicking...", uuid, reason)
		if kicked, err := connMgr.KickUser(uuid); err != nil {
			log.Printf("Failed to kick user %s: %v", uuid, err)
		} else if kicked > 0 {
			log.Printf("Kicked %d connections for user %s", kicked, uuid)
		}
	})

	return agent, nil
}

// Run 启动 realm-agent 主循环。
func (a *Agent) Run(ctx context.Context) {
	a.startHTTPServer()

	// 注册（不靠 IP，靠 node_id+api_key，§5.1）。
	if err := a.syncer.Register(a.cfg.NodeID, a.cfg.RealmID, a.cfg.Region, a.cfg.HY2Port); err != nil {
		log.Printf("Node registration failed: %v (will keep retrying via heartbeat/sync)", err)
	}

	// 首次同步：拉 users + realm + dns 块并生成配置。
	if err := a.syncAndApply(); err != nil {
		log.Printf("Initial sync failed: %v", err)
		if a.cache.HasCache() {
			log.Println("Using cached configuration from a previous successful sync...")
			if err := a.applyFromCache(); err != nil {
				log.Printf("Failed to apply cache: %v", err)
				a.logDegraded("manager unreachable and cached config could not be applied")
			}
		} else {
			// 全新装 + manager 从没通过：没有 realm_token/stun（§4.1 锁定为必须下发，
			// install.sh 故意不带），无法凭空兜底起 realm inbound。只能明确提示，不让运维
			// 从一堆 522/grpc 报错里自己推断。
			a.logDegraded("manager unreachable on first start and no cached config exists")
		}
	}

	if err := a.billReporter.FlushCache(); err != nil {
		log.Printf("Failed to flush stats cache: %v", err)
	}

	// 启动 sing-box。
	if os.Getenv("SKIP_SINGBOX") != "true" {
		if err := a.manager.Start(); err != nil {
			log.Printf("Failed to start sing-box: %v", err)
		}
	} else {
		log.Println("SKIP_SINGBOX=true, skipping sing-box start")
	}

	a.runMainLoop(ctx)
}

func (a *Agent) startHTTPServer() {
	mux := http.NewServeMux()
	healthServer := api.NewHealthServer(
		func() bool {
			return a.manager.IsRunning() || os.Getenv("SKIP_SINGBOX") == "true"
		},
		a.isRealmActive,
	)
	mux.HandleFunc("/health", healthServer.HandleHealth)
	mux.HandleFunc("/ready", healthServer.HandleReady)

	go func() {
		log.Println("HTTP server starting on :8080")
		server := &http.Server{
			Addr:         ":8080",
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()
}

// runMainLoop 主循环。
//
// 定时器：
//   - syncTicker      : 拉配置（§7.2 version diff 决定是否 reload）
//   - statsTicker     : 计费上报（→manager）+ 流量画像快照（→运营平面）
//   - heartbeatTicker : 心跳
//   - connectionsTicker: 计费连接上报 + obs 连接级采集（lifecycle/behavior）
//   - egressTicker    : 出口质量探测（§7.3.5）
//   - realmTicker     : §7.9 按需重注册 + realm_health 上报
//   - obsFlushTicker  : 批量推送运营平面缓冲
//   - quotaTicker     : 过期/限额周期检查
func (a *Agent) runMainLoop(ctx context.Context) {
	syncTicker := time.NewTicker(a.cfg.SyncInterval)
	statsTicker := time.NewTicker(a.cfg.StatsInterval)
	heartbeatTicker := time.NewTicker(30 * time.Second)
	connectionsTicker := time.NewTicker(10 * time.Second)
	egressTicker := time.NewTicker(60 * time.Second)
	realmTicker := time.NewTicker(a.cfg.RealmInterval)
	obsFlushTicker := time.NewTicker(10 * time.Second)
	quotaTicker := time.NewTicker(10 * time.Second)
	defer syncTicker.Stop()
	defer statsTicker.Stop()
	defer heartbeatTicker.Stop()
	defer connectionsTicker.Stop()
	defer egressTicker.Stop()
	defer realmTicker.Stop()
	defer obsFlushTicker.Stop()
	defer quotaTicker.Stop()

	log.Println("Realm agent is running")

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping agent...")
			a.collectAndReportBilling()
			a.obsReporter.Flush()
			a.manager.Stop()
			return

		case <-syncTicker.C:
			if err := a.syncAndApply(); err != nil {
				log.Printf("Sync error: %v", err)
			}

		case <-statsTicker.C:
			a.collectAndReportBilling()
			a.snapshotTrafficProfile()

		case <-heartbeatTicker.C:
			a.sendHeartbeat()

		case <-connectionsTicker.C:
			a.reportConnectionsAndObserve()

		case <-egressTicker.C:
			a.probeEgressQuality()

		case <-realmTicker.C:
			a.realmReconcile()

		case <-obsFlushTicker.C:
			a.obsReporter.Flush()

		case <-quotaTicker.C:
			a.monitor.CheckAllUsers()
		}
	}
}

// syncAndApply 同步配置并应用（WP-3：双 version 分流 reload / 热更）。
//
// 决策（见 REALM_HOT_RELOAD_WP3.md 决策表）：
//   - realm_version 变（含首次 realm 激活、realm 块从无到有）→ reload（inbound 重建无法热更，断连）。
//   - 仅 user_version 变（realm_version 不变）→ 热更端点推全量 user 集（不 reload、不断连）；
//     热更失败 → 降级回 reload（最终一致）。
//   - 都不变 → 原样 return。
//
// 兼容老 manager：双 version 字段缺失（空串）时退回顶层 version 判定，一律走 reload（安全侧）。
func (a *Agent) syncAndApply() error {
	resp, err := a.syncer.FetchUsers()
	if err != nil {
		return err
	}

	// 即便 version 不变也刷新限额视图（用量可能已变）。
	a.monitor.UpdateUsers(resp.Users)

	a.mu.RLock()
	prev := syncState{
		version:      a.currentVersion,
		userVersion:  a.currentUserVersion,
		realmVersion: a.currentRealmVersion,
		realmActive:  a.realmActive,
	}
	a.mu.RUnlock()

	switch decideSyncAction(prev, resp) {
	case actionReload:
		log.Printf("Config change → reload (version=%s user_version=%s realm_version=%s, %d users, §7.0 drops connections)",
			resp.Version, resp.UserVersion, resp.RealmVersion, len(resp.Users))
		return a.applyFullReload(resp)

	case actionHotReload:
		log.Printf("Only user_version changed: %s → %s (%d users) → hot-reload (no drop)",
			prev.userVersion, resp.UserVersion, len(resp.Users))
		return a.applyHotReload(resp)

	default: // actionNone
		return nil
	}
}

// syncAction 是 syncAndApply 的决策结果。
type syncAction int

const (
	actionNone      syncAction = iota // 配置无变化，不动
	actionHotReload                   // 仅用户集变 → 热更（不断连）
	actionReload                      // realm 变 / 首次激活 / 老 manager 兜底 → 整进程 reload
)

// syncState 是 agent 本地缓存的上一次下发的版本快照（决策输入，纯数据便于测试）。
type syncState struct {
	version      string
	userVersion  string
	realmVersion string
	realmActive  bool
}

// decideSyncAction 纯决策：对比本地缓存版本与新响应，决定走 reload / 热更 / 不动。
// 抽成纯函数便于单测（不依赖 sing-box 进程、syncer、缓存）。
//
//   - 双 version 缺失（老 manager）→ 退回顶层 version 判定，变则一律 reload（安全侧）。
//   - realm_version 变 或 realm 块从无到有（首次激活）→ reload（inbound 重建无法热更）。
//   - 仅 user_version 变（realm_version 不变）→ 热更。
//   - 都不变 → 不动。
func decideSyncAction(prev syncState, resp *config.UsersResponse) syncAction {
	dualVersion := resp.UserVersion != "" && resp.RealmVersion != ""

	if !dualVersion {
		if prev.version == resp.Version {
			return actionNone
		}
		return actionReload
	}

	realmChanged := prev.realmVersion != resp.RealmVersion
	realmFirstActivation := resp.Realm != nil && !prev.realmActive
	if realmChanged || realmFirstActivation {
		return actionReload
	}
	if prev.userVersion != resp.UserVersion {
		return actionHotReload
	}
	return actionNone
}

// applyFullReload 写配置 + 缓存 + 整进程 reload（realm 变更 / 首次激活 / 兜底路径）。
func (a *Agent) applyFullReload(resp *config.UsersResponse) error {
	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}

	singboxCfg := a.generator.Generate(resp.Users, resp.Realm, resp.DNS)
	if err := a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig); err != nil {
		return err
	}

	a.mu.Lock()
	a.currentVersion = resp.Version
	a.currentUserVersion = resp.UserVersion
	a.currentRealmVersion = resp.RealmVersion
	a.currentRealm = resp.Realm
	a.currentUserSet = uuidSet(enabledUUIDs(resp.Users))
	wasActive := a.realmActive
	a.realmActive = resp.Realm != nil // 只有 manager 真下发了 realm 块才算 realm 生效
	nowActive := a.realmActive
	a.mu.Unlock()

	if nowActive && !wasActive {
		log.Printf("REALM ACTIVE: applied realm config from manager (realm_id=%s)", resp.Realm.RealmID)
	} else if !nowActive {
		// 拉到了 users 但 manager 没带 realm 块——多半是 manager 还没做 realm 对接（§13）。
		a.logDegraded("manager responded but did not include a realm block (realm integration not deployed yet?)")
	}

	if a.manager.IsRunning() {
		log.Println("Config version changed, reloading sing-box (drops connections, §7.0)...")
		return a.manager.Reload()
	}
	return nil
}

// applyHotReload 仅用户集变化：推全量 user 集给运行中的 sing-box（不 reload、不断连）。
// 仍同步缓存 + 限额视图 + 盘上配置（防 sing-box 崩溃重启后从旧盘配置恢复丢新用户——热更只改内存）。
// 任一前提不满足或端点调用失败 → 降级回 applyFullReload（reload），保证最终一致。
func (a *Agent) applyHotReload(resp *config.UsersResponse) error {
	// sing-box 未运行：没有进程可热更，退回 reload 路径（其内部 IsRunning 为 false 时只写盘不重启）。
	if !a.manager.IsRunning() {
		log.Println("Hot-reload requested but sing-box not running → fall back to reload path (write config only)")
		return a.applyFullReload(resp)
	}

	// 盘上配置必须随热更同步（防崩溃重启丢新用户）。realm 块不变，用当前缓存的 realm/dns 重渲染。
	singboxCfg := a.generator.Generate(resp.Users, resp.Realm, resp.DNS)
	if err := a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig); err != nil {
		// 盘上配置写失败：不要再热更（内存与盘会不一致，崩溃即丢用户）。退回 reload。
		log.Printf("Hot-reload: failed to persist config to disk (%v) → fall back to reload", err)
		return a.applyFullReload(resp)
	}

	// 推全量 user 集（仅 enabled，对齐 generator 的 hy2 users 过滤）。
	uuids := enabledUUIDs(resp.Users)

	// 算出"本次掉出集合"的用户（删除/禁用）——热更只移除其认证，已建连接不会自动断，
	// 需主动踢（§任务5：被删用户自身连接由 clash_api KickUser 断掉，不影响其他人）。
	removed := a.diffRemovedUsers(uuids)

	hrResp, err := a.hotReload.UpdateUsers(uuids)
	if err != nil {
		// 端点拒绝/非 200/sing-box 没起端点（老二进制无热更）→ 降级回 reload + 记日志。
		log.Printf("Hot-reload endpoint failed (%v) → fall back to reload (drops connections)", err)
		return a.applyFullReload(resp)
	}

	// 热更成功：缓存 + version 缓存同步（realm_version 不变，user_version 推进）。
	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}
	a.mu.Lock()
	a.currentVersion = resp.Version
	a.currentUserVersion = resp.UserVersion
	a.currentRealmVersion = resp.RealmVersion
	a.currentRealm = resp.Realm
	a.currentUserSet = uuidSet(uuids)
	a.mu.Unlock()

	// 被删用户的现有连接不会因热更自动断 → 主动踢（不影响其他在线用户，§任务5）。
	if len(removed) > 0 {
		log.Printf("Hot-reload: %d user(s) removed from set, kicking their live connections: %v", len(removed), removed)
		a.kickUsers(removed)
	}

	log.Printf("Hot-reload OK: %d users live, billing_sync=%v (no connections dropped)", hrResp.UserCount, hrResp.BillingSync)
	return nil
}

// diffRemovedUsers 返回"上次在集合、本次不在"的 UUID（删除/禁用的用户），供热更后主动踢连接。
func (a *Agent) diffRemovedUsers(newUUIDs []string) []string {
	next := uuidSet(newUUIDs)
	a.mu.RLock()
	prev := a.currentUserSet
	a.mu.RUnlock()

	var removed []string
	for uuid := range prev {
		if _, ok := next[uuid]; !ok {
			removed = append(removed, uuid)
		}
	}
	return removed
}

// uuidSet 把 UUID 切片转成集合。
func uuidSet(uuids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(uuids))
	for _, u := range uuids {
		set[u] = struct{}{}
	}
	return set
}

// enabledUUIDs 提取启用用户的 UUID 列表（对齐 generator_realm.go 的 hy2 users 过滤：仅 Enabled）。
func enabledUUIDs(users []config.User) []string {
	uuids := make([]string, 0, len(users))
	for _, u := range users {
		if u.Enabled {
			uuids = append(uuids, u.UUID)
		}
	}
	return uuids
}

// applyFromCache 从缓存应用配置（离线兜底）。
func (a *Agent) applyFromCache() error {
	resp, err := a.cache.LoadUsers()
	if err != nil {
		return err
	}
	a.monitor.UpdateUsers(resp.Users)

	a.mu.Lock()
	a.currentRealm = resp.Realm
	a.realmActive = resp.Realm != nil
	a.currentUserSet = uuidSet(enabledUUIDs(resp.Users))
	a.mu.Unlock()

	if resp.Realm != nil {
		log.Printf("REALM ACTIVE (from cache): realm_id=%s", resp.Realm.RealmID)
	}

	singboxCfg := a.generator.Generate(resp.Users, resp.Realm, resp.DNS)
	return a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig)
}

// sendHeartbeat 发送心跳（计费通路）。
func (a *Agent) sendHeartbeat() {
	sysLoad := stats.GetSystemLoad()
	connections, _ := a.connMgr.GetActiveConnections()

	req := &config.HeartbeatRequest{
		NodeID:    a.cfg.NodeID,
		Timestamp: time.Now().UTC(),
		Load: config.NodeLoad{
			CPUPercent:        sysLoad.CPUPercent,
			MemoryPercent:     sysLoad.MemoryPercent,
			ActiveConnections: len(connections),
			UserCount:         a.monitor.GetUserCount(),
		},
		PublicIP: stats.GetPublicIPv4(), // §5.1：仅可观测
	}

	resp, err := a.syncer.Heartbeat(req)
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}

	if len(resp.KickUsers) > 0 {
		a.kickUsers(resp.KickUsers)
	}
	if resp.ReloadUsers {
		log.Println("Manager requested user reload")
		if err := a.syncAndApply(); err != nil {
			log.Printf("Reload-on-request failed: %v", err)
		}
	}
}

// reportConnectionsAndObserve 上报计费连接（→manager）+ obs 连接级采集（→运营平面）。
// 一次 GetActiveConnections，喂两条通路（计费 + lifecycle/behavior），避免重复抓取。
func (a *Agent) reportConnectionsAndObserve() {
	connections, err := a.connMgr.GetActiveConnections()
	if err != nil {
		return // sing-box 可能未运行
	}

	now := time.Now().UTC()

	// —— 计费通路 ——
	if len(connections) > 0 {
		report := &config.ConnectionsReport{
			NodeID:      a.cfg.NodeID,
			Timestamp:   now,
			Connections: make([]config.Connection, 0, len(connections)),
		}
		for _, conn := range connections {
			clientIP := conn.Metadata.Source
			if host, _, err := net.SplitHostPort(clientIP); err == nil {
				clientIP = host
			}
			connectedAt, _ := time.Parse(time.RFC3339, conn.Start)
			report.Connections = append(report.Connections, config.Connection{
				UserUUID:    conn.Metadata.User,
				ClientIP:    clientIP,
				ConnectedAt: connectedAt,
				Upload:      conn.Upload,
				Download:    conn.Download,
			})
		}
		if resp, err := a.syncer.ReportConnections(report); err != nil {
			log.Printf("Report connections failed: %v", err)
		} else if len(resp.KickUsers) > 0 {
			a.kickUsers(resp.KickUsers)
		}
	}

	// —— 运营平面通路（§7.4/§7.6）——
	obsConns := make([]obs.Conn, 0, len(connections))
	for _, conn := range connections {
		start, _ := time.Parse(time.RFC3339, conn.Start)
		obsConns = append(obsConns, obs.Conn{
			ID:          conn.ID,
			User:        conn.Metadata.User,
			Destination: conn.Metadata.Destination,
			Upload:      conn.Upload,
			Download:    conn.Download,
			Start:       start,
		})
	}

	// 第 2 类：连接生命周期 diff。
	if events := a.lifecycle.Diff(obsConns, now); len(events) > 0 {
		a.obsReporter.Enqueue(obs.SchemaConnLifecycle, obs.ConnLifecyclePayload{Events: events})
	}
	// 第 4 类：出口行为窗口累计（在 egressTicker 周期 Build 上报）。
	a.behavior.Observe(obsConns)
}

// snapshotTrafficProfile 第 3 类：流量画像快照（→运营平面，§7.3.4）。
// ★用 Reset_:false 另一路，绝不与计费的 Reset_:true 共用一次调用（否则少计费）。
func (a *Agent) snapshotTrafficProfile() {
	snap, err := a.billCollector.Collect(false)
	if err != nil {
		return
	}
	payload := obs.TrafficProfilePayload{WindowSec: int(a.cfg.StatsInterval.Seconds())}
	for uuid, s := range snap {
		payload.Users = append(payload.Users, obs.TrafficProfileUser{
			UserUUID: uuid,
			Upload:   s.Upload,
			Download: s.Download,
		})
	}
	if len(payload.Users) > 0 {
		a.obsReporter.Enqueue(obs.SchemaTrafficProfile, payload)
	}
}

// probeEgressQuality 第 6 类：出口质量探测（§7.3.5）+ 第 4 类窗口 Build。
func (a *Agent) probeEgressQuality() {
	// 出口质量。
	quality := obs.ProbeEgress(a.probeTargets, stats.GetPublicIPv4())
	a.obsReporter.Enqueue(obs.SchemaEgressQuality, quality)

	// 第 4 类：窗口结束，产出行为特征并重置。
	behavior := a.behavior.Build()
	if len(behavior.Users) > 0 {
		a.obsReporter.Enqueue(obs.SchemaEgressBehavior, behavior)
	}
}

// realmReconcile §7.9 按需重注册 + 第 5 类 realm_health 上报。
func (a *Agent) realmReconcile() {
	a.mu.RLock()
	realmBlock := a.currentRealm
	a.mu.RUnlock()

	serverURL := a.cfg.RealmServerURL
	if realmBlock != nil && realmBlock.ServerURL != "" {
		serverURL = realmBlock.ServerURL
	}

	// §7.9.4：registeredSelf 的来源依赖 7.9.3 待核实项。
	// 本期采用分支 (b) 基线：只探 L3 可达，假定 sing-box 自带重注册（registeredSelf=true）。
	// 若后续核实 sing-box 不自带重注册，改为分支 (a)：实现 lookup-自己并传入真实结果。
	decision := a.registrar.Evaluate(serverURL, true)

	if decision.NeedReload && a.manager.IsRunning() {
		log.Printf("[realm] %s → reload to re-register", decision.Reason)
		if err := a.manager.Reload(); err != nil {
			log.Printf("[realm] reload failed: %v", err)
		}
	}

	// 第 5 类 realm_health（agent 自测部分可靠；register_events 依赖日志解析，待核实，暂留空）。
	a.obsReporter.Enqueue(obs.SchemaRealmHealth, obs.RealmHealthPayload{
		WindowSec:    int(a.cfg.RealmInterval.Seconds()),
		L3Reachable:  decision.Reachable,
		L3ProbeRTTMs: decision.ProbeRTTMs,
		ReloadCount:  a.manager.ReloadCount(),
		ServerURL:    serverURL,
	})
}

// kickUsers 踢掉指定用户（复用计费闭环）。
func (a *Agent) kickUsers(uuids []string) {
	for _, uuid := range uuids {
		uuid = strings.TrimSpace(uuid)
		if uuid == "" {
			continue
		}
		if kicked, err := a.connMgr.KickUser(uuid); err != nil {
			log.Printf("Failed to kick user %s: %v", uuid, err)
		} else if kicked > 0 {
			log.Printf("Kicked %d connections for user %s (by Manager)", kicked, uuid)
		}
	}
}

// collectAndReportBilling 收集并上报计费统计（→manager，Reset_:true）。
func (a *Agent) collectAndReportBilling() {
	userStats, err := a.billCollector.Collect(true)
	if err != nil {
		log.Printf("Failed to collect stats: %v", err)
		return
	}
	if len(userStats) == 0 {
		return
	}

	for uuid, stat := range userStats {
		traffic := stat.Upload + stat.Download
		if traffic > 0 {
			a.monitor.CheckUser(uuid, traffic)
		}
	}

	if err := a.billReporter.Report(userStats); err != nil {
		log.Printf("Failed to report stats: %v", err)
		return
	}
	a.monitor.ResetSessionTraffic()
	if a.billReporter.GetCacheCount() > 0 {
		if err := a.billReporter.FlushCache(); err != nil {
			log.Printf("Failed to flush stats cache: %v", err)
		}
	}
}
