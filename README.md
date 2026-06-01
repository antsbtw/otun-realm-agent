# otun-realm-agent

realm 住宅IP出口（国内/海外家用 debian）上常驻的节点 agent。**独立服务**，与
[otun-node-agent](../otun-node-agent/) 平级。实现规格见
[document/realm/OTUN_REALM_AGENT_DESIGN.md](../../document/realm/OTUN_REALM_AGENT_DESIGN.md)。

## 为什么独立建

otun-node-agent 是成熟稳定系统，不为 realm 的形态差异改它（避免回归风险）。所有 realm 的
"不一样"封在本 agent 里；与 manager 说【同一套 `/api/node/*` 方言】，manager 几乎不动
（register / users / stats / heartbeat / connections / kick 闭环原样复用）。

## 与现有节点的 4 个差异

| # | 现有节点 | realm 出口 | 本 agent 如何应对 |
|---|---|---|---|
| ① | 公网入口、固定 443 | NAT 后无入口、端口动态 | 靠 realm 打洞，不需公网入口（§7.9） |
| ② | hosting-service 自动开机 | 人工部署 | `install.sh` 一键自注册 |
| ③ | tls-service 下发证书 | 自签 `iptv.local` | agent 自签自持，不调 tls-service（`internal/config/selfsign.go`） |
| ④ | hy2 直连 inbound | hy2 + `realm{}` 注册块 | `RealmGenerator` 新增 realm 分支（`internal/config/generator_realm.go`） |

## 两条上报通路（必须分清）

| 通路 | 端点 | 内容 | 代码 |
|---|---|---|---|
| **计费**（复用方言） | manager `/api/node/stats` `/connections` `/heartbeat` | per-user 字节、活跃连接、负载 | `internal/stats` |
| **运营平面**（新，独立） | `OBS_ENDPOINT`（私网，HTTP POST JSON） | 连接生命周期 / 流量画像 / 出口行为(风控) / 打洞质量 / 出口质量 | `internal/obs` |

> 计费用 `Reset_:true` 清零，运营平面用 `Reset_:false` 快照——**不共用同一次 QueryStats 调用**，
> 否则会和计费抢着 reset 导致少计费（§7.3.4）。

### 运营平面 5 个 schema

| schema | 类别 | 数据源 |
|---|---|---|
| `conn_lifecycle` | 第2 连接上下线/时长 | clash_api `/connections` diff |
| `traffic_profile` | 第3 流量画像 | V2Ray stats（Reset_:false 快照） |
| `egress_behavior` | 第4 出口行为（风控生死线） | clash_api `Metadata.Destination` 聚合（仅元数据，**不解包/不记 path**） |
| `realm_health` | 第5 打洞质量（realm 独有） | agent 自测 L3 探测 + reload 计数（register 日志解析待核实） |
| `egress_quality` | 第6 出口质量 | agent 主动探固定目的地 + 查出口 IP |

## sing-box 配置生成的 4 个固化坑（§4 / §7.1）

1. direct-dns 不写 `detour`
2. STUN 必须 IP（`stun_servers` 仅 IPv4，规避 `409 realm_taken`）
3. realm 块内不写 `http_client.detour`
4. proxy-dns 按【出口国家】下发（不硬编码 223.5.5.5）——manager 维护"国家→DNS"映射下发

均有单测覆盖：`internal/config/generator_realm_test.go`。

## reload = 整进程重启（§7.0 硬事实）

`Manager.Reload()` 不是热重载，而是 stop(SIGTERM)+start——**断掉该出口所有活跃连接**。因此：
- version 变更才 reload（`syncAndApply` 靠 version diff，§7.2）。
- realm 重注册循环【绝不无条件定时 reload】，只在探测到 L3 注册失效时 reload（§7.9 `internal/realm`）。

## 部署

```bash
curl -fsSL https://raw.githubusercontent.com/antsbtw/otun-realm-agent/main/install.sh | sudo bash -s -- \
  --api-key  rk_live_xxxxxxxx \
  --node-id  realm-cn-sh-01 \
  --realm-id iptv-cn-sh \
  --region   cn-sh-mobile
```

`realm_token`/`stun`/`obfs`/`sni`/`dns` 由 manager 经 `/api/node/users` 下发，install.sh 故意不带
（§4.1：这些必须动态可调）。

## 构建 / 测试

```bash
go build ./cmd/agent
go vet ./...
go test ./...
```

## 三个 sing-box 内部行为待核实（均有兜底，不阻塞）

1. **`realm{}` 字段精确键名/层级**（§7.1.2）→ 实现前用 `sing-box check -c` 对真机配置核验；
   键名沿用客户端 PoC 已验证语义。
2. **realm 注册日志格式**（§7.5.1）→ 真机抓 `journalctl` 确认；未核实前 `realm_health` 先用
   agent 自测的 `l3_reachable`/`reload_count` 等可靠指标。
3. **sing-box 是否自带周期重注册**（§7.9.3）→ Mac 源码 / 真机断 L3 观察。本期取**分支(b)基线**
   （`Evaluate(serverURL, true)`，只探 L3 可达）；若核实为不自带，改**分支(a)**：实现 lookup-自己
   并把真实 `registeredSelf` 传入 `Registrar.Evaluate`。

## manager 侧 / 数据表

§7.8 的 `realm_egress` / `realm_user_assignment` 表是**设计草案，非本服务执行**（不在本仓 migrate）。
manager 侧把 realm/dns 块纳入 `/api/node/users` 响应 + version 哈希（§7.2）是 manager 仓的改动，
本 agent 已按该响应 schema 解析（`internal/config/types.go` `UsersResponse`）。
