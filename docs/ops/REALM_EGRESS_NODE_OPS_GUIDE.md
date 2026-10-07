# Realm 出口节点 安装 & 运维手册（六协议 egress-inproc）

> 对象：运维/开发。给一台机器装成 realm 六协议出口节点，并日常维护。
> 适用：otun-realm-agent v2.0.0（egress 内嵌六协议，无 sing-box fork）。**这是唯一的 realm 出口运维手册**——老 sing-box realm 节点/老 situs-web 会合面已全部退役（2026-07-06），旧的 hy2+realm{} 单协议 agent 手册已归档，不再适用。
> 最后更新：2026-07-06（移入 ops/，合并远程接入/dl 分发章节，随实操核实修正）。

---

## 0. 一句话拓扑

```
客户端 ──打洞──> 会合面(otun-s :9443) <──打洞── 出口节点(otun-realm-agent 六协议)
                                                          │ register/heartbeat/users/stats
                                                          ▼
                                                    otun-manager(控制面)
```

- **出口节点**跑 `otun-realm-agent`，进程内起六协议 egress node（hy2/reality/tuic/trojan/ss/vmess），本地 UDP 端口 51820-51825（**约定常量，不对外、不进 manager**）。
- 出口不监听公网协议端口——它主动**打洞**连到**会合面**（`realm_server_url`，如 `http://<会合面IP>:9443`）。
- 节点身份/配置全部由 **manager 下发**：token/stun/obfs/sni/dns/protocols/reality 密钥都在 `GET /api/node/users`，装机时**不带**。
- 节点靠 `node_id + api_key` 向 manager 注册（不靠 IP）。

**三种凭证别混**（历史踩坑）：
| 用途 | 谁生成 | 装机是否带 |
|---|---|---|
| **api_key**（rk_live_…） | manager 建 egress 时生成 | ✅ `--api-key` |
| realm_token（会合面 token） | 建 egress 时指定 / L3 池 | ❌ manager 下发 |
| reality 密钥对 | manager 建 egress 时生成(x25519) | ❌ manager 下发 private，客户端拿 public |

---

## 1. 安装一个出口节点（两步：建 egress → 装 agent）

### 步骤 A：在 manager 建 egress（拿到 node_id + api_key）

**前提**：会合面（otun-s）已跑起来、token 已知。egress 的 `realm_token` + `realm_server_url` 必须指向一个**会合面认这个 token** 的实例（否则打洞 401）。token 来源二选一：
- 走 **L3 池**（`realm_l3_pool`）：不填 `realm_token`，manager 自动挑一个有容量的池 token（生产默认）。
- **显式指定**（测试/独立会合面）：填 `realm_token` + `realm_server_url`。

```bash
# 在 manager 所在节点（或能到 8022 的地方）执行。ADMIN_API_KEY 从 manager 容器 env 取。
ADMINKEY=$(docker inspect otun-manager --format '{{range .Config.Env}}{{println .}}{{end}}' | grep '^ADMIN_API_KEY=' | cut -d= -f2-)

curl -s -X POST http://localhost:8022/api/cluster/realm-egress \
  -H "X-Admin-Key: $ADMINKEY" -H "Content-Type: application/json" \
  -d '{
    "node_id": "egress-<region>-01",
    "realm_id": "iptv-<region>-01",
    "geo_country": "SG",
    "region_override": "Singapore",
    "reality_handshake_server": "<借壳目标IP,如会合面公网IP>",
    "realm_token": "<会合面token,走L3池则省略>",
    "realm_server_url": "https://<会合面IP>:9443"
  }'
# ★realm_server_url 必须 https(otun-s 自签 TLS,出口 https+insecure 连);写 http 会撞墙连不上。
# 返回: {"egress_id":"egress-<region>-01","api_key":"rk_live_...."}
# manager 自动: 生成 api_key + reality x25519 密钥对 + short_id + obfs + ss_method 默认 + protocols 全六
```

- `node_id` = `egress_id`（装机 `--node-id` 用它）。
- `realm_id` 是 **base**；agent 自动派生 `<base>-<proto>`（hy2/reality/tuic/trojan/ss/vmess），manager/客户端只认 base。
- `reality_handshake_server` **存 IP 不存域名**（reality 借壳目标）；一般填会合面公网 IP。
- 建完 DB 里 `nodes` 行是 `node_kind=realm, status=pending`，等 agent 注册后转 `online`。

#### 实例：美国加第二个出口，连 otun-s-test 会合面（2026-07-06 实测可用）

现有美国出口 `realm-us-01` 已占用 → 新出口用 `realm-us-02`/`iptv-us-02`。会合面 = otun-s-test（`54.255.172.86:9443`，token `jwhCQ82…`）。

**先看某国现有出口 id（避免撞车）**：
```bash
ssh v3-portal-01 'K=$(docker inspect otun-manager --format "{{range .Config.Env}}{{println .}}{{end}}" | grep "^ADMIN_API_KEY=" | cut -d= -f2-); curl -s -H "X-Admin-Key: $K" http://localhost:8022/api/cluster/realm-egress'
```

**建 egress（两种执行方式，★引号写法不同，别混）**：

**方式 1 — 已经 SSH 进 v3-portal-01，在节点内直接跑**（JSON 用普通单引号即可）：
```bash
K=$(docker inspect otun-manager --format '{{range .Config.Env}}{{println .}}{{end}}' | grep '^ADMIN_API_KEY=' | cut -d= -f2-)

curl -s -X POST http://localhost:8022/api/cluster/realm-egress \
  -H "X-Admin-Key: $K" -H "Content-Type: application/json" \
  -d '{
    "node_id": "realm-us-02",
    "realm_id": "iptv-us-02",
    "geo_country": "US",
    "region_override": "United States",
    "reality_handshake_server": "54.255.172.86",
    "realm_token": "jwhCQ82CRclSSeHHcXDvWjO0Cp5fV51dJ0csZtWdg",
    "realm_server_url": "https://54.255.172.86:9443"
  }'
```

**方式 2 — 从本地一次性远程执行**（外层套 `ssh v3-portal-01 '...'`，内层 JSON 的单引号必须转义成 `'"'"'`）：
```bash
ssh v3-portal-01 '
K=$(docker inspect otun-manager --format "{{range .Config.Env}}{{println .}}{{end}}" | grep "^ADMIN_API_KEY=" | cut -d= -f2-)
curl -s -X POST http://localhost:8022/api/cluster/realm-egress \
  -H "X-Admin-Key: $K" -H "Content-Type: application/json" \
  -d '"'"'{
    "node_id": "realm-us-02",
    "realm_id": "iptv-us-02",
    "geo_country": "US",
    "region_override": "United States",
    "reality_handshake_server": "54.255.172.86",
    "realm_token": "jwhCQ82CRclSSeHHcXDvWjO0Cp5fV51dJ0csZtWdg",
    "realm_server_url": "https://54.255.172.86:9443"
  }'"'"'
'
```

> ⚠️ **踩坑（2026-07-06）**：把方式 2 的命令（含 `'"'"'`）粘进**已经进了节点的 shell** 里跑 → 引号错乱 → curl 收到破 JSON 报 `invalid character`。**在节点内就用方式 1（普通单引号）**；只有从本地远程套 ssh 才用方式 2。
>
> 返回 `{"egress_id":"realm-us-02","api_key":"rk_live_..."}` —— 记下 `api_key`，步骤 B 装 agent 的 `--api-key` 用它。
>
> `$ADMINKEY` 无需手抄，命令里现场从容器 env 取；单独查：`ssh v3-portal-01 'docker inspect otun-manager --format "{{range .Config.Env}}{{println .}}{{end}}" | grep "^ADMIN_API_KEY="'`

**校验建好了**（在 DB 节点 v3-db-01；把 `realm-us-02` 换成你的 egress_id）：
```bash
ssh v3-db-01 "docker exec v3-postgres psql -U saas_user -d otun_manager_db -x -c \
 \"SELECT egress_id,realm_id,protocols,realm_server_url,ss_method,
         reality_private_key IS NOT NULL AS has_reality,reality_handshake_server
  FROM realm_egress WHERE egress_id='realm-us-02';\""
```

### 步骤 B：在出口机器装 agent（install.sh 正规路径）

**前提**：出口机器能出网到 `https://otun-manager-v3.situstechnologies.com`（注册/下发）、会合面 `:9443`（打洞）和 `https://situstechnologies.com/dl/`（拉脚本+二进制）。x86_64/arm64 均可，**不需要预装 go**。

> ★**2026-07-19 起 agent 仓（`antsbtw/otun-realm-agent`）已私有**：GitHub release / raw / clone 匿名全部不可达。install.sh 与二进制**只从 `/dl/` 分发**（CI 自动同步），装机必须带 `--dl-token`。分发链路排障看
> [AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md](AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md)。

```bash
# 在出口机器（root/sudo）。最省事：直接复制 fleet admin 建节点返回的 install_command（已含全部参数和 token）。
# 手动版：
DL_TOKEN=<situs-web dl_auth.conf 里的 token>
curl -fsSL -H "Authorization: Bearer $DL_TOKEN" https://situstechnologies.com/dl/install.sh -o install.sh
sudo bash install.sh \
  --api-key   rk_live_<建egress返回的key> \
  --node-id   egress-<region>-01 \
  --realm-id  iptv-<region>-01 \
  --region    Singapore \
  --dl-token  "$DL_TOKEN" \
  --api-url   https://otun-manager-v3.situstechnologies.com
# --api-url 默认就是上面这个 v3 域名，可省。★别用废弃域名 otun-manager(无v3),会 522。
```

install.sh 会：清理旧装 → 拉二进制(sha256校验)到 `/opt/otun-realm-agent/agent` → 写
`systemd unit`（env: NODE_API_KEY/NODE_ID/OTUN_API_URL/MANAGEMENT_MODE=remote/REALM_ID/
REALM_REGION）→ enable+start → 装 `realm` 管理命令。

**install.sh 参数全集**（以 agent 仓 install.sh 为准，2026-07-06 核实）：

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `--api-key` | ✅ | — | 建 egress 返回的 `rk_live_…`。★注意是 `--api-key` 不是 `--node-api-key`（旧交接里写错过） |
| `--realm-id` | ✅ | — | egress 的 base realm_id，如 `iptv-us-02`；agent 自动派生六协议后缀 |
| `--node-id` | 否 | `realm-$(hostname)` | 建议显式传，= egress_id |
| `--region` | 否 | 空（后端 GeoIP 判定） | 出口 region，如 `"United States"` |
| `--api-url` | 否 | `https://otun-manager-v3.situstechnologies.com` | manager 地址。★别用废弃域名 otun-manager(无v3)→522 |
| `--fleet-url` | 否 | 空 | fleet-manager 地址（Batch 5）；**不传则 agent 回退用 OTUN_API_URL，零回归**，一般无需传 |
| `--obs-endpoint` | 否 | 空（上报 no-op） | ops-collector 采集端点（私网），如 `http://172.26.x.x:8026`；不传则不上报运营 obs |
| `--dl-token` | ✅（2026-07-19 起） | 空 | `/dl/` 分发 token。仓库私有后 `/dl/` 是唯一分发源，**海外节点也必须传**；缺失直接报错退出 |

> sha256 校验：install.sh 运行时从 `/dl/manifest.json`（CI 与二进制同批原子发布）取预期 sha 校验，**无需人工钉 sha**（旧的"发版回填 DL_SHA256"流程已废，见 §5）。

### 步骤 C：验证注册成功

```bash
# 节点侧：日志应见 "REALM ACTIVE" + 六个 "egress node up"
sudo journalctl -u otun-realm-agent -n 30 --no-pager

# manager 侧（DB 节点）：status=online + 观测列有六协议+端口
docker exec v3-postgres psql -U saas_user -d otun_manager_db -x -c \
 "SELECT n.status,n.version,n.last_heartbeat,e.active_protocols,e.protocol_ports
  FROM nodes n JOIN realm_egress e ON e.egress_id=n.id WHERE n.id='egress-<region>-01';"
# 期望: status=online, version=2.0.0, active_protocols={全六}, protocol_ports={hy2:51820...vmess:51825}
```

---

## 2. 日常维护

节点上有 `realm` 命令（install.sh 装的）：
```bash
realm status     # 服务状态
realm logs       # 实时日志（journalctl -f）
realm restart    # 重启
realm start / stop
```

直接 systemd 也行：`sudo systemctl {status|restart|stop} otun-realm-agent`。

**看节点跑了哪些协议/端口**（manager 侧，运维纳管）：
```sql
SELECT egress_id, active_protocols, protocol_ports FROM realm_egress WHERE egress_id='...';
```

**改节点配置**（token/stun/sni/protocols/reality/dns）：**不上节点改**，改 manager 的 `realm_egress` 行，下个 sync 周期 agent 自动拉新配置并 reload（realm_version 变触发）。reload 会**断一次连**（打洞连接重建），低峰期改。

**下线/删除节点**：
```bash
# 1. 节点侧卸载
sudo bash uninstall.sh            # 停服务/删 unit/删 realm 命令；保留 /opt/.../data(自签证书)
# 彻底清: sudo rm -rf /opt/otun-realm-agent

# 2. manager 侧删 egress（★先确认无用户 assign 到它，否则 409）
curl -s -X DELETE http://localhost:8022/api/cluster/realm-egress/egress-<region>-01 \
  -H "X-Admin-Key: $ADMINKEY"
# 有用户占用返回 409 egress_in_use + user_count；先把用户切走再删。
```

---

## 3. 排障速查

| 症状 | 先查 | 常见根因 |
|---|---|---|
| 节点日志无 REALM ACTIVE / 一直 pending | `journalctl -u otun-realm-agent` 有无 register 报错 | api_key 错/manager 不可达/api-url 用了废弃域名(522) |
| 注册成功但客户端连不上 | 会合面 `:9443` 从节点可达？token 对不对？ | 节点→会合面无路由；realm_token 与会合面 users.json 不符(401) |
| reality 连不上、其余五协议正常 | egress 的 reality_private_key 是否为空 | 老 egress(改造前建)无密钥对；重建 egress 或补 keypair |
| 客户端 "No primary node URL found" | connect_url 的 node 字段 | 见 [[project_realm_protocol_node_contract]]：node 必须 primary/backup 非 egress_id |
| 改了配置节点不生效 | realm_version 是否变 / sync 周期 | 只改 DB 不调 service 入口时的一致性；等一个 sync 周期 |
| 出口 IP 是节点本地不是会合面 | STUN 填了域名？两端 STUN 不一致？ | 见 [[project_realm_dual_uuid_mismatch]] / STUN 要填 IP |

**装机拉不到二进制**：私库化后 install.sh 只有 `/dl/` 一个源、失败即中断（无 GitHub/源码兜底）。按顺序查：token 对不对 → situs-web `/dl/` 可达性 → CI sync-dl 是否绿。完整排障表见 [AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md](AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md)。

**SSH 上节点**：见 §4.1（Cloudflare Tunnel）。

---

## 4. 远程接入 + 二进制分发（绕 GFW）

### 4.1 远程 SSH 接入（NAT 后出口的带外运维）

出口在家用 NAT 后无公网入站，SSH 进不去。**接入方案 = Cloudflare Tunnel**（已上线，2026-06-16）。

**原理**：节点装 `cloudflared`，主动出站连 Cloudflare（7844，QUIC/UDP 优先回落 TCP），运维经 Cloudflare SSH 进节点。**VPS 零暴露、无端口分配、按 hostname 区分、GFW 实测可达**。

**给一个新节点配 SSH 接入**：
1. Cloudflare Zero Trust（团队 `realm-manager`）→ 网络 → 连接器 → **新建隧道**，类型 Cloudflared，命名 `realm-<节点>`。
2. ⚠️ **致命坑**：一个节点 = 一个独立隧道，**必须用它自己隧道的 token**。把节点 A 的 token 拿到节点 B 跑 → B 变成 A 隧道的第二个连接器 → SSH 随机分到任意一台。每个新节点先在控制台**新建隧道**。
3. 节点上：`sudo apt-get install cloudflared && sudo cloudflared service install <该隧道的 token>`。验证 `systemctl is-active cloudflared` + `journalctl -u cloudflared | grep Registered`。
4. 控制台「路由隧道」配 SSH：子域名 `<节点>-ssh`，服务 `SSH`，URL `localhost:22`。
5. 运维端（任意 Linux/Mac，装 cloudflared）ssh config：
   ```
   Host realm-cn
       HostName cn-ssh-01.situstechnologies.com
       User antsbtw
       ProxyCommand cloudflared access ssh --hostname %h
   ```
   首次 `cloudflared access login <hostname>`（别用 timeout 包裹，否则接不住回传 token），token 存 `~/.cloudflared`（24h 有效）。

> ⚠️ **token = 谁拿到谁能跑你的隧道，绝不写进 git/聊天/文档**。作废去 CF 控制台重置隧道。
> 详见 [[project_realm_node_ssh_cloudflare_tunnel]]。

### 4.2 二进制分发 /dl/（唯一分发源）

**2026-07-19 起 agent 仓私有，`/dl/` 是唯一分发源**（此前的"GitHub release 兜底 / 源码自编兜底"匿名不可达，已从 install.sh 删除）。`/dl/` 经官网 Cloudflare 边缘，全球含中国可达；所有节点装机都要 `--dl-token`：
```
curl -H "Authorization: Bearer <token>" https://situstechnologies.com/dl/agent-linux-amd64
```
- 分发内容：`install.sh` + `agent-linux-{amd64,arm64}` + `updater.sh` + `manifest.json`，全部由 CI `sync-dl` job 经受限部署 key 自动发布（服务器侧 `dl-publish` 白名单+sha 校验+原子 rename）。
- 鉴权走 **Authorization 头**（不进 URL/日志/cache key），**每次在 nginx 源站校验**（不靠 CDN 缓存，避免缓存旁路鉴权）。
- token 在 situs-web `/etc/nginx/conf.d/dl_auth.conf`，**不进 git**；install.sh 用 `/dl/manifest.json` 里的 sha256 校验二进制。
- situs-web=13.214.21.114（官网回源机，**改 nginx 只新增不动现有 server 块**）。
- 全链路细节与排障：[AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md](AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md)。

---

## 5. agent 发版流程（改了 agent 源码后）

**一次 push 全自动**（U1 起 manifest 是校验真源；2026-07-19 私库化后 install.sh 也随批发布）：

**push agent 源码到 main** → `build-agent.yml` CI 自动完成：test（含 updater 7 场景）→ build(amd64+arm64, `-ldflags "-s -w" -X main.buildVersion=<git sha>`) → 生成 `manifest.json`（version + 每 artifact sha256）→ `sync-dl` 把 **二进制 + updater.sh + install.sh + manifest.json** 发到 `/dl/`（服务器侧校验+原子发布）并从公网闭环验证 manifest version → `update-latest` 同步挂到 GitHub `latest` release（私库后仅维护者可见，仅作归档）。

- **无需人工回填 sha**：install.sh 运行时拉 `/dl/manifest.json` 校验（旧"两次 push 钉 sha"流程已废）。
- 发版后节点**不会自动升级**：升级由 fleet 建任务驱动（`upgrade-plan` → updater timer），见 fleet 升级编排。
- CI 状态查看：GitHub → antsbtw/otun-realm-agent → Actions（私库需登录）；`sync-dl` 的"Verify /dl/ serves the new manifest"步骤红了 = `/dl/` 没发上去，节点侧装机/升级还在用旧版。

**★踩过的坑（发版前必查）**：
1. **go.mod 里别留本地 `replace => /tmp/...`**：CI 干净 runner 无此目录会挂 vet/build（`replacement directory ... does not exist`）。本地能过是因为你机器上有那目录，掩盖问题。依赖走 GOPROXY（otun-s-egress 是公开仓；若它日后也私有化，CI 须配 GOPRIVATE + 拉取凭证）。
2. **CI 失败先读 Actions 的 Annotations 真错误**，别急着当"环境瞬时"去 re-run。
3. **"复现 CI 失败"要真在干净环境**（隐藏本地副本）跑，别在留着本地依赖的开发机 fresh clone 就当等价。
4. **私库 Actions 计分钟数**（公库免费）：free plan 2000 分钟/月，本仓单次全流水 ~10 分钟，正常节奏远够；异常刷版本时留意 Settings → Billing。

**验证**（测试节点跑真 install.sh）：
```bash
sudo bash uninstall.sh   # uninstall.sh 不在 /dl/，从仓库拷（维护者有私库权限；或直接 systemctl stop+disable + rm unit）
curl -fsSL -H "Authorization: Bearer <DL_TOKEN>" https://situstechnologies.com/dl/install.sh -o install.sh
sudo bash install.sh --api-key <key> --node-id <id> --realm-id <id> --dl-token <DL_TOKEN>
realm logs    # 应见拉二进制成功(sha256通过) + REALM ACTIVE + 六 node up
```

---

## 6. 多出口 / 同国加第二出口（N=2 池化）

同一国家可挂**多个 egress**，fleet-manager 自动池化（N=2 主备）：

- **单出口**：该国自动 N=1（零回归）。
- **加第二出口**：在 manager 建**第二条 egress**（`node_id`/`realm_id` 不能撞现有，如美国已有 `realm-us-01` → 新的用 `realm-us-02`/`iptv-us-02`），online 后 fleet 自动把该国扩成 N=2 热备，**无需改码、无需改前端**。客户端 `/connect-bundle` 会返 primary/backup 两个 node。
- **看某国现有出口**（避免 id 撞车）：
  ```bash
  ssh v3-portal-01 'K=$(docker inspect otun-manager --format "{{range .Config.Env}}{{println .}}{{end}}" | grep "^ADMIN_API_KEY=" | cut -d= -f2-); curl -s -H "X-Admin-Key: $K" http://localhost:8022/api/cluster/realm-egress'
  ```
- 池化决策/N=2 展开细节见 [[project_fleet_manager_impl_progress]]、`design/FLEET_MANAGER_DESIGN.md`。

> ⚠️ 复用会合面 token：新出口可复用同一会合面 token（`max_realms=0` 允许多 realm），**无需改会合面 users.json、无需 reload**。只有想给节点独立 token 便于分辨时，才改 `/etc/otun-s/users.json` + `sudo systemctl reload otun-s`（热加载，别重启）。

---

## 附：本手册验证于生产

**2026-07-06 实况复核**（本次实操核实，全部通过）：会合面 otun-s-test `54.255.172.86:9443` active、version `5ad32f5`、token `jwhCQ82...` 有效（`/etc/otun-s/users.json` 单 `sg-test` max_realms=0）；manager 建 egress 路由 `POST /api/cluster/realm-egress`（200）；在线 realm 节点 `realm-cn-01`/`realm-de-01`/`realm-us-01` 均 version=2.0.0 六协议。★老 sing-box realm 节点/老 situs-web 会合面已全部退役。

**2026-07-02 首版验证**：

- 建 egress `egress-test-01`（realm_id `iptv-test-01`，会合面 otun-s-test `54.255.172.86:9443` token `jwhCQ82...`）：manager 自动生成 reality 密钥对，DB 六协议列齐。
- otun-s-out **走真实 install.sh**（从 GitHub latest release 下载 + sha256 校验 `[2/2] GitHub: agent-linux-amd64` → Agent downloaded）→ 自动注册 → manager `status=online`、version=2.0.0、`active_protocols={全六}`、`protocol_ports={hy2:51820...vmess:51825}`。装机节点二进制 sha256 与 CI 发布值逐字节一致，证明正规安装路径（下载+校验+systemd+注册）全通。
- 数据面(客户端逐协议打洞出网)见 [[project_realm_six_protocol_downlink_chain]] 后续。


sudo systemctl stop otun-realm-agent
sudo systemctl disable otun-realm-agent
sudo pkill -9 otun-realm-agent 2>/dev/null || true
sudo rm -rf /opt/otun-realm-agent
sudo rm -f /etc/systemd/system/otun-realm-agent.service
sudo rm -rf /etc/systemd/system/otun-realm-agent.service.d
sudo rm -f /usr/local/bin/realm
sudo systemctl daemon-reload


sudo bash -s <<'EOF'
set -e
DL_TOKEN="d62430f93969a4258b7d81a36338a50660283f210581fdbf"

sed -i '/^Environment="FLEET_API_URL=/d' /etc/systemd/system/otun-realm-agent.service
sed -i '/OTUN_API_URL/a Environment="FLEET_API_URL=https://otun-manager-v3.situstechnologies.com/fleet"' \
  /etc/systemd/system/otun-realm-agent.service
systemctl daemon-reload

curl -fsSL -H "Authorization: Bearer $DL_TOKEN" \
  https://situstechnologies.com/dl/updater.sh -o /opt/otun-realm-agent/updater.sh
chmod +x /opt/otun-realm-agent/updater.sh
/opt/otun-realm-agent/updater.sh --install

/opt/otun-realm-agent/updater.sh

systemctl is-active otun-realm-agent
ss -ulnH | grep -cE ':5182[0-5]\s'
EOF
