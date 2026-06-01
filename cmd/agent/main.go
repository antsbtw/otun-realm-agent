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

	dataDir        string
	currentVersion string
	currentRealm   *config.RealmBlock // 最近一次下发的 realm 块（供 §7.9 探测 server_url）
	mu             sync.RWMutex
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
		log.Printf("Node registration failed: %v", err)
	}

	// 首次同步：拉 users + realm + dns 块并生成配置。
	if err := a.syncAndApply(); err != nil {
		log.Printf("Initial sync failed: %v", err)
		if a.cache.HasCache() {
			log.Println("Using cached configuration...")
			if err := a.applyFromCache(); err != nil {
				log.Printf("Failed to apply cache: %v", err)
			}
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
	healthServer := api.NewHealthServer(func() bool {
		return a.manager.IsRunning() || os.Getenv("SKIP_SINGBOX") == "true"
	})
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

// syncAndApply 同步配置并应用（§7.2 version-driven reload）。
func (a *Agent) syncAndApply() error {
	resp, err := a.syncer.FetchUsers()
	if err != nil {
		return err
	}

	a.mu.RLock()
	sameVersion := a.currentVersion == resp.Version
	a.mu.RUnlock()

	// 即便 version 不变也刷新限额视图（用量可能已变）。
	a.monitor.UpdateUsers(resp.Users)

	if sameVersion {
		// §7.2.2：靠 version diff 才能做到"只有 realm 参数真变了才 reload"，
		// 否则每次 sync 都整进程重启断所有连接（§7.0）。
		return nil
	}

	log.Printf("New configuration version: %s (%d users)", resp.Version, len(resp.Users))

	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}

	singboxCfg := a.generator.Generate(resp.Users, resp.Realm, resp.DNS)
	if err := a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig); err != nil {
		return err
	}

	a.mu.Lock()
	a.currentVersion = resp.Version
	a.currentRealm = resp.Realm
	a.mu.Unlock()

	if a.manager.IsRunning() {
		log.Println("Config version changed, reloading sing-box (drops connections, §7.0)...")
		return a.manager.Reload()
	}
	return nil
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
	a.mu.Unlock()

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
