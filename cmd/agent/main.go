package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antsbtw/otun-s-egress/egress"

	"otun-realm-agent/internal/api"
	"otun-realm-agent/internal/config"
	"otun-realm-agent/internal/obs"
	"otun-realm-agent/internal/quota"
	"otun-realm-agent/internal/realm"
	"otun-realm-agent/internal/stats"
)

// Agent 是 realm-agent 主控制器。固定 remote 模式（§7.7：无本地/混合）。
//
// 六协议库化（scheme 甲）：数据面不再是单 sing-box fork 进程，而是进程内 import egress
// 库、按 manager 下发的 protocols 起 N 个 egress.Node（每协议一个本地 UDP 口 + 一个派生
// realm_id 注册会合面）。
//
// ★共享 meter Registry（egress C.1）：六个 Node 全部注入同一个 *egress.Registry。
// 计费/踢人/obs 对这一个 Registry 一次调用即覆盖六协议，无 IPC、天然合账（施工单 §3.7），
// 不写"六 node 汇总"补丁。usermap 仍每 Node 独立（认证索引各协议不共享，否则串号）。
type Agent struct {
	cfg       *config.AgentConfig
	syncer    *config.Syncer
	cache     *config.Cache
	selfsign  *config.SelfSigner
	generator *config.RealmGenerator
	monitor   *quota.Monitor

	// —— 数据面：进程内 egress 库，六 Node 共享一个 Registry ——
	nodeMu   sync.Mutex                // 保护 nodes/conns/registry 的重建
	nodes    map[string]egress.Node    // key=protocol；每协议一个 egress.Node
	conns    map[string]net.PacketConn // key=protocol；每 Node 各自的 UDP socket
	registry *egress.Registry          // ★六 Node 共享的 meter Registry（计费/踢人/obs 一次覆盖全协议）
	baseCtx  context.Context

	// 计费上报（→manager，§7.3.1）。共享 Registry CollectStats(true) 供账。
	billReporter *stats.Reporter

	// 运营平面通路（→OBS_ENDPOINT，§7.3.1）。
	obsReporter  *obs.Reporter
	lifecycle    *obs.LifecycleTracker
	behavior     *obs.BehaviorAggregator
	probeTargets []obs.ProbeTarget

	// realm 重注册（§7.9）。
	registrar   *realm.Registrar
	reloadCount             int // 累积 rebuild 计数（§7.5 realm_health：频繁 rebuild = 出口在抖动）
	lastReportedReloadCount int // 上次 realm_health 上报时的 reloadCount 基线（用于算窗口增量）
	lastRendezvous          *config.RendezvousHealth // 最近一次自检快照（1c，随注册/心跳上报 fleet）

	dataDir             string
	currentVersion      string             // 顶层合并 version（老 manager 无双 version 时用它判定）
	currentUserVersion  string             // user 集投影哈希（仅它变 → update 热更）
	currentRealmVersion string             // realm 投影哈希（它变 → rebuild）
	currentRealm        *config.RealmBlock // 最近一次下发的 realm 块（供 §7.9 探测 server_url）
	realmActive         bool               // 是否已用下发/缓存的 realm 配置建过 node
	activeProtocols     []string           // 当前实际起来的协议（供 register 上报实况）
	mu                  sync.RWMutex
}

// isRealmActive 线程安全读取 realm 是否已生效（供 health 端点）。
func (a *Agent) isRealmActive() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.realmActive
}

// isNodeRunning 报告数据面是否已建起至少一个 node（供 health / 决策）。
func (a *Agent) isNodeRunning() bool {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	return len(a.nodes) > 0
}

// logDegraded 打一条醒目的降级提示，说清"为什么 realm 没生效"。
func (a *Agent) logDegraded(reason string) {
	log.Printf("================ REALM NOT ACTIVE (degraded) ================")
	log.Printf("  reason : %s", reason)
	log.Printf("  effect : no egress node built — no protocol listeners, no billing.")
	log.Printf("  why    : realm token/stun/sni/protocols MUST be pushed by manager (design §3);")
	log.Printf("           install.sh deliberately does not carry them, so the agent cannot")
	log.Printf("           bootstrap a realm egress on its own without a prior good sync.")
	log.Printf("  action : bring otun-manager online and ensure it serves this realm egress")
	log.Printf("           (node_kind=realm) via /api/node/users. Agent keeps retrying.")
	log.Printf("=============================================================")
}

func main() {
	log.Println("========================================")
	log.Println("  OTun Realm Agent v2.0.0 (egress-inproc, 6-protocol)")
	log.Println("========================================")

	cfg := config.LoadFromEnv()
	if cfg.NodeAPIKey == "" {
		log.Fatal("NODE_API_KEY is required")
	}
	if cfg.RealmID == "" {
		log.Println("Warning: REALM_ID not set; manager-issued realm block will drive registration")
	}

	log.Printf("Node ID:    %s", cfg.NodeID)
	log.Printf("Realm ID:   %s (base; protocols derive <base>-<proto>)", cfg.RealmID)
	log.Printf("API URL:    %s", cfg.APIURL)
	log.Printf("Local ports: hy2=%d tuic=%d reality=%d trojan=%d ss=%d vmess=%d",
		config.PortHysteria2, config.PortTUIC, config.PortReality,
		config.PortTrojan, config.PortShadowsocks, config.PortVMess)
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

	// §7.1.3 自签 iptv.local：无则生成、有则复用。产物 PEM 注入 hy2/tuic 外层 TLS。
	selfsign := config.NewSelfSigner(dataDir, "")
	if err := selfsign.EnsureCert(); err != nil {
		return nil, err
	}

	agent := &Agent{
		cfg:          cfg,
		// ★Batch 5：Syncer 拆 URL——register/heartbeat 走 FleetURL（空则回退 APIURL），
		//   FetchUsers/ReportConnections 仍走 APIURL（otun）。billReporter（计费 stats）不动，仍 APIURL。
		syncer:       config.NewSyncer(cfg.APIURL, cfg.FleetURL, cfg.NodeAPIKey),
		cache:        config.NewCache(dataDir),
		selfsign:     selfsign,
		generator:    config.NewRealmGenerator(selfsign.CertPath(), selfsign.KeyPath()),
		billReporter: stats.NewReporter(cfg.APIURL, cfg.NodeAPIKey, statsCache),
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
		nodes:        map[string]egress.Node{},
		conns:        map[string]net.PacketConn{},
	}

	// 限额监控器（超限/过期 → 踢人）。★踢人走共享 Registry：一次踢该用户全协议连接。
	agent.monitor = quota.NewMonitor(func(uuid, reason string) {
		log.Printf("User quota exceeded: %s (%s), kicking...", uuid, reason)
		if kicked := agent.kickUser(uuid); kicked > 0 {
			log.Printf("Kicked %d connections for user %s (all protocols)", kicked, uuid)
		}
	})

	return agent, nil
}

// Run 启动 realm-agent 主循环。
func (a *Agent) Run(ctx context.Context) {
	a.baseCtx = ctx
	a.startHTTPServer()

	// 注册（不靠 IP，靠 node_id+api_key，§5.1）。首启还没起 node，上报空协议实况。
	if err := a.register(); err != nil {
		log.Printf("Node registration failed: %v (will keep retrying via heartbeat/sync)", err)
	}

	// 首次同步：拉 users + realm 块并建六 node。
	if err := a.syncAndApply(); err != nil {
		log.Printf("Initial sync failed: %v", err)
		if a.cache.HasCache() {
			log.Println("Using cached configuration from a previous successful sync...")
			if err := a.applyFromCache(); err != nil {
				log.Printf("Failed to apply cache: %v", err)
				a.logDegraded("manager unreachable and cached config could not be applied")
			}
		} else {
			a.logDegraded("manager unreachable on first start and no cached config exists")
		}
	}

	if err := a.billReporter.FlushCache(); err != nil {
		log.Printf("Failed to flush stats cache: %v", err)
	}

	a.runMainLoop(ctx)
}

// register 上报节点实况（node_id + realm_id_base + 实际启用协议 + 各本地端口）给 manager。
// 1c：捎带最近一次会合面自检快照（注册常在 rebuild 刚完成时发生，此时携带的是上一窗快照）。
func (a *Agent) register() error {
	a.mu.RLock()
	protocols := append([]string(nil), a.activeProtocols...)
	rendezvous := a.lastRendezvous
	a.mu.RUnlock()
	return a.syncer.Register(a.cfg.NodeID, a.cfg.RealmID, a.cfg.Region, protocols, rendezvous)
}

func (a *Agent) startHTTPServer() {
	mux := http.NewServeMux()
	healthServer := api.NewHealthServer(
		func() bool { return a.isNodeRunning() || a.isRealmActive() },
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
			a.collectAndReportBilling() // 退出前采最后一窗账（共享 Registry 一次）
			a.obsReporter.Flush()
			a.closeAllNodes()
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

// —— §4 决策：none / update / rebuild（reload 概念消失）——

// syncAction 是 syncAndApply 的决策结果。
type syncAction int

const (
	actionNone    syncAction = iota // 配置无变化，不动
	actionUpdate                    // 仅用户集变 → 六 node 各 UpdateUsers（不断连）
	actionRebuild                   // realm 变 / 首次激活 / 老 manager 兜底 → 重建六 node
)

// syncState 是 agent 本地缓存的上一次下发的版本快照（决策输入，纯数据便于测试）。
type syncState struct {
	version      string
	userVersion  string
	realmVersion string
	realmActive  bool
}

// decideSyncAction 纯决策：对比本地缓存版本与新响应，决定 none / update / rebuild。
//
//   - 双 version 缺失（老 manager）→ 退回顶层 version 判定，变则一律 rebuild（安全侧）。
//   - realm_version 变 或 realm 块从无到有（首次激活）→ rebuild。
//   - 仅 user_version 变（realm_version 不变）→ update（六 node UpdateUsers 热更，不断连）。
//   - 都不变 → none。
func decideSyncAction(prev syncState, resp *config.UsersResponse) syncAction {
	dualVersion := resp.UserVersion != "" && resp.RealmVersion != ""

	if !dualVersion {
		if prev.version == resp.Version {
			return actionNone
		}
		return actionRebuild
	}

	realmChanged := prev.realmVersion != resp.RealmVersion
	realmFirstActivation := resp.Realm != nil && !prev.realmActive
	if realmChanged || realmFirstActivation {
		return actionRebuild
	}
	if prev.userVersion != resp.UserVersion {
		return actionUpdate
	}
	return actionNone
}

// syncAndApply 同步配置并应用（§4 三分支）。
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
	case actionRebuild:
		log.Printf("Config change → rebuild nodes (version=%s user_version=%s realm_version=%s, %d users; drops connections)",
			resp.Version, resp.UserVersion, resp.RealmVersion, len(resp.Users))
		return a.applyRebuild(resp)

	case actionUpdate:
		log.Printf("Only user_version changed: %s → %s (%d users) → UpdateUsers (no drop)",
			prev.userVersion, resp.UserVersion, len(resp.Users))
		return a.applyUpdate(resp)

	default: // actionNone
		return nil
	}
}

// applyRebuild realm 参数变 / 首次激活 / 老 manager 兜底：重建六 node。
func (a *Agent) applyRebuild(resp *config.UsersResponse) error {
	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}

	a.mu.Lock()
	a.currentVersion = resp.Version
	a.currentUserVersion = resp.UserVersion
	a.currentRealmVersion = resp.RealmVersion
	a.currentRealm = resp.Realm
	wasActive := a.realmActive
	a.realmActive = resp.Realm != nil
	nowActive := a.realmActive
	a.mu.Unlock()

	if !nowActive {
		// 拉到了 users 但 manager 没带 realm 块——多半 manager 还没做 realm 对接。
		a.logDegraded("manager responded but did not include a realm block (realm integration not deployed yet?)")
		a.closeAllNodes()
		return nil
	}

	if nowActive && !wasActive {
		log.Printf("REALM ACTIVE: applied realm config from manager (realm_id base=%s, protocols=%v)",
			resp.Realm.RealmID, resp.Realm.Protocols)
	}

	// ★删人先采账铁律同样适用于 rebuild：Close 会连未采字节一起丢。
	// 重建前先把当前所有 node 的账（共享 Registry 一次）采干净并上报，再关。
	if a.isNodeRunning() {
		a.collectAndReportBilling()
	}

	return a.rebuildAllNodes(resp)
}

// applyUpdate 仅用户集变化：六 node 各 UpdateUsers（不断连）。
// 若没有运行中的 node（首启还没成功建过），退化为 rebuild。
func (a *Agent) applyUpdate(resp *config.UsersResponse) error {
	if !a.isNodeRunning() {
		log.Println("Update requested but no node running → fall back to rebuild path")
		return a.applyRebuild(resp)
	}

	// ★删人先采账铁律（§2）：UpdateUsers 内部对 removed 用户 EvictUser，会连未采字节一起丢。
	// 先 CollectStats(true)+Report（共享 Registry 一次覆盖六协议），再 UpdateUsers。
	a.collectAndReportBilling()

	users := config.BuildUsers(resp.Users)
	// ★六 node 各 UpdateUsers；哪个协议失败只 rebuild 该协议（其余不动）。
	if failedProtos := a.updateUsersAll(users); len(failedProtos) > 0 {
		log.Printf("UpdateUsers failed on %v → rebuilding only those protocols", failedProtos)
		if err := a.rebuildProtocols(resp, failedProtos); err != nil {
			log.Printf("per-protocol rebuild failed (%v) → fall back to full rebuild (drops all)", err)
			return a.applyRebuild(resp)
		}
	}

	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}
	a.mu.Lock()
	a.currentVersion = resp.Version
	a.currentUserVersion = resp.UserVersion
	a.currentRealmVersion = resp.RealmVersion
	a.currentRealm = resp.Realm
	a.mu.Unlock()

	log.Printf("UpdateUsers OK: %d users live across %d protocols (no connections dropped)",
		len(users), len(a.snapshotProtocols()))
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
	a.currentVersion = resp.Version
	a.currentUserVersion = resp.UserVersion
	a.currentRealmVersion = resp.RealmVersion
	a.currentRealm = resp.Realm
	a.realmActive = resp.Realm != nil
	a.mu.Unlock()

	if resp.Realm == nil {
		return nil // 缓存里也没 realm 块，无法建 node
	}
	log.Printf("REALM ACTIVE (from cache): realm_id base=%s protocols=%v", resp.Realm.RealmID, resp.Realm.Protocols)
	return a.rebuildAllNodes(resp)
}

// —— 数据面 node 生命周期（六 Node 共享 Registry）——

// rebuildAllNodes 关掉全部旧 node、建一个新的共享 Registry、按下发 protocols 起六 node。
// 顶层对 node 相关操作用 nodeMu 串行化（§6 隔离）。
func (a *Agent) rebuildAllNodes(resp *config.UsersResponse) error {
	cfgs := a.generator.BuildConfigs(resp.Realm, config.SeedUUID(resp.Users))
	if len(cfgs) == 0 {
		return fmt.Errorf("rebuildAllNodes: no realm block / no enabled protocols, cannot build egress configs")
	}

	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()

	// 关旧全部（账已在 applyRebuild 里采过；这里只释放资源），并新建共享 Registry。
	a.closeAllNodesLocked()
	a.registry = egress.NewRegistry() // ★六 Node 共享的新 Registry

	users := config.BuildUsers(resp.Users)
	started := make([]string, 0, len(cfgs))
	for _, cfg := range cfgs {
		if err := a.startOneLocked(cfg, users); err != nil {
			// 某协议 Start 失败不拖垮其余（log + 继续，failover）。
			log.Printf("egress node start failed for protocol %s: %v (continuing with others)", cfg.Protocol, err)
			continue
		}
		started = append(started, cfg.Protocol)
	}

	a.reloadCount++
	a.setActiveProtocols(started)
	log.Printf("egress nodes started: %v (rebuild#%d, %d users, shared Registry)", started, a.reloadCount, len(users))

	if len(started) == 0 {
		return fmt.Errorf("rebuildAllNodes: all protocol nodes failed to start")
	}

	a.mu.Lock()
	a.currentUserVersion = resp.UserVersion
	a.mu.Unlock()

	// 起来协议集变化 → 重报实况给 manager（异步，失败不致命）。
	go func() {
		if err := a.register(); err != nil {
			log.Printf("re-register after rebuild failed: %v", err)
		}
	}()
	return nil
}

// rebuildProtocols 只重建指定协议的 node（热更部分失败的补救，施工单 §3.7）。
// ★重建时把共享 Registry 再注入回去（cfg.Meter = a.registry），不丢账、不换 Registry。
func (a *Agent) rebuildProtocols(resp *config.UsersResponse, protocols []string) error {
	cfgs := a.generator.BuildConfigs(resp.Realm, config.SeedUUID(resp.Users))
	want := map[string]egress.Config{}
	for _, c := range cfgs {
		want[c.Protocol] = c
	}

	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	if a.registry == nil {
		return fmt.Errorf("rebuildProtocols: no shared registry (nothing running)")
	}

	users := config.BuildUsers(resp.Users)
	var firstErr error
	for _, proto := range protocols {
		cfg, ok := want[proto]
		if !ok {
			log.Printf("rebuildProtocols: protocol %s not in current downlink, skipping", proto)
			continue
		}
		// 关该协议旧 node + conn。
		a.closeOneLocked(proto)
		if err := a.startOneLocked(cfg, users); err != nil {
			log.Printf("rebuildProtocols: restart %s failed: %v", proto, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Printf("rebuildProtocols: %s rebuilt (shared Registry re-injected)", proto)
	}
	a.setActiveProtocols(a.runningProtocolsLocked())
	return firstErr
}

// startOneLocked 为一个协议开 UDP socket、egress.New（注入共享 Registry）、Start、UpdateUsers。
// 调用方须持有 nodeMu，且 a.registry 已就绪。
func (a *Agent) startOneLocked(cfg egress.Config, users []egress.User) error {
	cfg.Meter = a.registry // ★注入共享 Registry（C.1）

	port := config.LocalPort(cfg.Protocol)
	if port == 0 {
		return fmt.Errorf("no local port for protocol %s", cfg.Protocol)
	}
	addr := &net.UDPAddr{IP: net.IPv6unspecified, Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp :%d (%s): %w", port, cfg.Protocol, err)
	}

	// egress.New 对个别协议（如 reality 的非法 private_key）会 panic 而非返回 error；
	// 用 recover 兜底，让一个坏协议配置不拖垮其余五协议（§6 隔离，扩展到 New）。
	node, err := safeNew(cfg)
	if err != nil {
		conn.Close()
		return fmt.Errorf("egress.New(%s): %w", cfg.Protocol, err)
	}

	startCtx := a.baseCtx
	if startCtx == nil {
		startCtx = context.Background()
	}
	if err := a.safeStart(node, startCtx, conn); err != nil {
		node.Close()
		conn.Close()
		return fmt.Errorf("node.Start(%s): %w", cfg.Protocol, err)
	}

	a.nodes[cfg.Protocol] = node
	a.conns[cfg.Protocol] = conn

	// 推入全量启用用户集。
	if err := node.UpdateUsers(users); err != nil {
		log.Printf("startOneLocked(%s): initial UpdateUsers failed: %v", cfg.Protocol, err)
	}
	log.Printf("egress node up: protocol=%s realm_id=%s port=%d users=%d",
		cfg.Protocol, cfg.RealmID, port, len(users))
	return nil
}

// safeNew 带 recover 调 egress.New（库对非法参数如 reality 坏 key 会 panic，转 error）。
func safeNew(cfg egress.Config) (node egress.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			node, err = nil, fmt.Errorf("panic in egress.New: %v", r)
		}
	}()
	return egress.New(cfg)
}

// safeStart 带 recover 调 node.Start（库 panic → 转 error，不崩 agent，§6 隔离，逐 node）。
func (a *Agent) safeStart(node egress.Node, ctx context.Context, conn net.PacketConn) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in node.Start: %v", r)
		}
	}()
	return node.Start(ctx, conn)
}

// updateUsersAll 对六 node 各 UpdateUsers（带 recover 逐 node 隔离）。
// 返回 UpdateUsers 失败的协议列表（调用方对这些协议做 rebuild）。
func (a *Agent) updateUsersAll(users []egress.User) []string {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	var failed []string
	for proto, node := range a.nodes {
		if err := safeUpdate(node, users); err != nil {
			log.Printf("UpdateUsers(%s) failed: %v", proto, err)
			failed = append(failed, proto)
		}
	}
	sort.Strings(failed)
	return failed
}

// safeUpdate 带 recover 调 node.UpdateUsers。
func safeUpdate(node egress.Node, users []egress.User) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in node.UpdateUsers: %v", r)
		}
	}()
	return node.UpdateUsers(users)
}

// closeAllNodes 关闭全部 node（外部持锁版）。
func (a *Agent) closeAllNodes() {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	a.closeAllNodesLocked()
}

// closeAllNodesLocked 关闭全部 node + conn（调用方须持有 nodeMu）。
func (a *Agent) closeAllNodesLocked() {
	for proto := range a.nodes {
		a.closeOneLocked(proto)
	}
	a.setActiveProtocols(nil)
}

// closeOneLocked 关闭某协议 node + conn（调用方须持有 nodeMu）。
func (a *Agent) closeOneLocked(proto string) {
	if node := a.nodes[proto]; node != nil {
		if err := node.Close(); err != nil {
			log.Printf("node.Close(%s): %v", proto, err)
		}
		delete(a.nodes, proto)
	}
	if conn := a.conns[proto]; conn != nil {
		conn.Close()
		delete(a.conns, proto)
	}
}

// runningProtocolsLocked 列举当前起来的协议（调用方须持有 nodeMu）。
func (a *Agent) runningProtocolsLocked() []string {
	out := make([]string, 0, len(a.nodes))
	for proto := range a.nodes {
		out = append(out, proto)
	}
	sort.Strings(out)
	return out
}

// snapshotProtocols 线程安全列举当前起来的协议。
func (a *Agent) snapshotProtocols() []string {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	return a.runningProtocolsLocked()
}

// setActiveProtocols 记录实际起来的协议实况（供 register 上报）。
func (a *Agent) setActiveProtocols(protocols []string) {
	a.mu.Lock()
	a.activeProtocols = protocols
	a.mu.Unlock()
}

// kickUser 踢掉某 user 全部连接。★走共享 Registry 一次踢全协议（C.1）。
func (a *Agent) kickUser(uuid string) int {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	if a.registry == nil {
		return 0
	}
	return a.registry.KickUser(uuid)
}

// activeConnections 取全协议连接快照。★走共享 Registry 一次覆盖六协议（C.1）。
func (a *Agent) activeConnections() []egress.ConnInfo {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	if a.registry == nil {
		return nil
	}
	return a.registry.Snapshot()
}

// collectStats 取全协议 per-user 流量。★走共享 Registry 一次按 UUID 合账（C.1）。
// reset=true 供账（清零），false 快照（观测）。铁律：两者绝不复用同一次调用（§5）。
func (a *Agent) collectStats(reset bool) []egress.UserStat {
	a.nodeMu.Lock()
	defer a.nodeMu.Unlock()
	if a.registry == nil {
		return nil
	}
	return a.registry.CollectStats(reset)
}

// —— 心跳 / 观测 / 计费 ——

// sendHeartbeat 发送心跳（计费通路）。
func (a *Agent) sendHeartbeat() {
	sysLoad := stats.GetSystemLoad()
	connections := a.activeConnections()

	a.mu.RLock()
	rendezvous := a.lastRendezvous
	a.mu.RUnlock()

	req := &config.HeartbeatRequest{
		NodeID:    a.cfg.NodeID,
		Timestamp: time.Now().UTC(),
		Load: config.NodeLoad{
			CPUPercent:        sysLoad.CPUPercent,
			MemoryPercent:     sysLoad.MemoryPercent,
			ActiveConnections: len(connections),
			UserCount:         a.monitor.GetUserCount(),
		},
		PublicIP:         stats.GetPublicIPv4(), // §5.1：仅可观测
		RendezvousHealth: rendezvous,            // 1c：自检快照，nil=尚无（fleet 不覆盖旧值）
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
// 一次共享 Registry Snapshot，喂两条通路（计费 + lifecycle/behavior），避免重复抓取。
func (a *Agent) reportConnectionsAndObserve() {
	connections := a.activeConnections()
	if len(connections) == 0 {
		return
	}

	now := time.Now().UTC()

	// —— 计费通路 ——
	report := &config.ConnectionsReport{
		NodeID:      a.cfg.NodeID,
		Timestamp:   now,
		Connections: make([]config.Connection, 0, len(connections)),
	}
	for _, conn := range connections {
		clientIP := conn.Source
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}
		report.Connections = append(report.Connections, config.Connection{
			UserUUID:    conn.UUID,
			ClientIP:    clientIP,
			ConnectedAt: conn.Start,
			Upload:      conn.Upload,
			Download:    conn.Download,
		})
	}
	if resp, err := a.syncer.ReportConnections(report); err != nil {
		log.Printf("Report connections failed: %v", err)
	} else if len(resp.KickUsers) > 0 {
		a.kickUsers(resp.KickUsers)
	}

	// —— 运营平面通路（§7.4/§7.6）——
	// ConnInfo.Protocol 区分协议；合成稳定 id 供 lifecycle diff。
	obsConns := make([]obs.Conn, 0, len(connections))
	for _, conn := range connections {
		obsConns = append(obsConns, obs.Conn{
			ID:          connID(conn),
			User:        conn.UUID,
			Destination: conn.Destination,
			Upload:      conn.Upload,
			Download:    conn.Download,
			Start:       conn.Start,
		})
	}

	// 第 2 类：连接生命周期 diff。
	if events := a.lifecycle.Diff(obsConns, now); len(events) > 0 {
		a.obsReporter.Enqueue(obs.SchemaConnLifecycle, obs.ConnLifecyclePayload{Events: events})
	}
	// 第 4 类：出口行为窗口累计（在 egressTicker 周期 Build 上报）。
	a.behavior.Observe(obsConns)
}

// connID 为 egress ConnInfo 合成一个稳定连接标识（库未暴露 conn_id）。
// 六协议共享 Registry 后加 Protocol 维度，避免不同协议同 uuid+dst 撞 id。
func connID(c egress.ConnInfo) string {
	return c.Protocol + "|" + c.UUID + "|" + c.Source + "|" + c.Destination
}

// snapshotTrafficProfile 第 3 类：流量画像快照（→运营平面，§7.3.4）。
// ★用 reset=false，绝不与计费的 reset=true 共用一次调用（否则少计费）。
func (a *Agent) snapshotTrafficProfile() {
	snap := a.collectStats(false)
	if len(snap) == 0 {
		return
	}
	payload := obs.TrafficProfilePayload{WindowSec: int(a.cfg.StatsInterval.Seconds())}
	for _, s := range snap {
		payload.Users = append(payload.Users, obs.TrafficProfileUser{
			UserUUID: s.UUID,
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
	quality := obs.ProbeEgress(a.probeTargets, stats.GetPublicIPv4())
	a.obsReporter.Enqueue(obs.SchemaEgressQuality, quality)

	behavior := a.behavior.Build()
	if len(behavior.Users) > 0 {
		a.obsReporter.Enqueue(obs.SchemaEgressBehavior, behavior)
	}
}

// realmReconcile §7.9 按需重注册 + 第 5 类 realm_health 上报。
//
// egress 库化后：realm 注册是库内 realm{} 行为。若探测到 L3 注册失效需重注册，
// 只能重建全部 node（rebuild = Close+New+Start），语义等价旧 reload。
func (a *Agent) realmReconcile() {
	a.mu.RLock()
	realmBlock := a.currentRealm
	a.mu.RUnlock()

	serverURL := ""
	if realmBlock != nil && realmBlock.ServerURL != "" {
		serverURL = realmBlock.ServerURL
	}

	// rendezvous_insecure：自签会合面探测须跳过证书验证（否则假报不可达）。
	insecure := realmBlock != nil && realmBlock.RendezvousInsecure

	// §7.9.4 分支 (a)：真实逐 slot 自检（otun-s GET /v1/{slot}/status，阶段 1a 端点）。
	// 只在有 node 在跑时探测——没起 node 本就没注册，rebuild 归 sync 路径管；
	// ProbeRegistered 是 fail-safe 的：老会合面（无 /status）/网络错 → 视为已注册，
	// 行为与"写死 true"的旧基线一致。
	registeredSelf := true
	var slotStatuses []realm.SlotStatus
	if realmBlock != nil && serverURL != "" && a.isNodeRunning() {
		protos := a.snapshotProtocols()
		slots := make([]string, 0, len(protos))
		for _, proto := range protos {
			slots = append(slots, config.DeriveRealmID(realmBlock.RealmID, proto))
		}
		registeredSelf, slotStatuses = a.registrar.ProbeRegistered(serverURL, realmBlock.Token, slots, insecure)
	}
	var unregSlots []string
	for _, st := range slotStatuses {
		if !st.Registered {
			unregSlots = append(unregSlots, st.Slot)
		}
	}

	// 1c：存自检快照（随下次注册/心跳诚实上报 fleet）。Slots 只含确凿结论；
	// last_register_error 取 sing-quic realm 内部最近一次 Warn+ 日志（logger 是唯一截获点）。
	if realmBlock != nil && serverURL != "" {
		health := &config.RendezvousHealth{
			ServerURL:         serverURL,
			Slots:             make([]config.RendezvousSlotHealth, 0, len(slotStatuses)),
			LastRegisterError: config.LastRealmError(),
		}
		for _, st := range slotStatuses {
			health.Slots = append(health.Slots, config.RendezvousSlotHealth{RealmID: st.Slot, Registered: st.Registered})
		}
		a.mu.Lock()
		a.lastRendezvous = health
		a.mu.Unlock()
	}

	decision := a.registrar.Evaluate(serverURL, registeredSelf, insecure)

	// ★连续 RebuildConfirmTicks 个 tick 确认才 rebuild：单次未注册可能撞上库自身
	// 秒级 re-register 的瞬间（会合面重启后 event-stream 断线重连），误 rebuild 白断
	// 在线隧道。连续两窗仍未注册才认定库自愈失败。
	if decision.NeedReload {
		if !a.registrar.ConfirmUnregistered(true) {
			log.Printf("[realm] slots %v not registered at rendezvous (unconfirmed, recheck in %s before rebuild)",
				unregSlots, a.cfg.RealmInterval)
			decision.NeedReload = false
		} else {
			log.Printf("[realm] slots %v not registered at rendezvous for %d consecutive ticks → self-heal rebuild",
				unregSlots, realm.RebuildConfirmTicks)
		}
	} else {
		a.registrar.ConfirmUnregistered(false)
	}

	if decision.NeedReload && a.isNodeRunning() {
		log.Printf("[realm] %s → rebuild nodes to re-register", decision.Reason)
		// 重建前先采账（rebuild 会关旧 node，未采字节会丢）。用最近一次缓存的下发重建。
		if resp, err := a.cache.LoadUsers(); err == nil && resp.Realm != nil {
			a.collectAndReportBilling()
			if err := a.rebuildAllNodes(resp); err != nil {
				log.Printf("[realm] rebuild failed: %v", err)
			}
		} else {
			log.Printf("[realm] cannot rebuild: no cached realm config")
		}
	}

	// ★ReloadCount 上报【本窗口增量】而非累积值（否则面板把恒定累积值误读成"每窗都在 reload"）。
	// 增量 = 本次累积 - 上次上报时的基线；上报后更新基线。稳态(不 rebuild)→增量恒 0。
	a.mu.Lock()
	reloadDelta := a.reloadCount - a.lastReportedReloadCount
	a.lastReportedReloadCount = a.reloadCount
	a.mu.Unlock()

	a.obsReporter.Enqueue(obs.SchemaRealmHealth, obs.RealmHealthPayload{
		WindowSec:    int(a.cfg.RealmInterval.Seconds()),
		L3Reachable:  decision.Reachable,
		L3ProbeRTTMs: decision.ProbeRTTMs,
		ReloadCount:  reloadDelta,
		ServerURL:    serverURL,
	})
}

// kickUsers 踢掉指定用户（manager 心跳/连接上报下发的 kick_users）。★共享 Registry 一次踢全协议。
func (a *Agent) kickUsers(uuids []string) {
	for _, uuid := range uuids {
		uuid = strings.TrimSpace(uuid)
		if uuid == "" {
			continue
		}
		if kicked := a.kickUser(uuid); kicked > 0 {
			log.Printf("Kicked %d connections for user %s (by Manager, all protocols)", kicked, uuid)
		}
	}
}

// collectAndReportBilling 收集并上报计费统计（→manager，reset=true）。
// ★共享 Registry 一次 CollectStats(true) 即拿六协议按 UUID 合账（C.1），天然原子一致，
// 无需"六 node 先全采再合并"。reset=true 与观测的 reset=false 绝不复用同一次调用（§5）。
func (a *Agent) collectAndReportBilling() {
	raw := a.collectStats(true)
	if len(raw) == 0 {
		return
	}

	userStats := make(map[string]*stats.UserStats, len(raw))
	for _, s := range raw {
		userStats[s.UUID] = &stats.UserStats{Upload: s.Upload, Download: s.Download}
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
