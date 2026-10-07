# otun-realm-agent 私库化分发运维手册（Runbook）

> 状态：⭐ 运维真源。2026-07-19 `antsbtw/otun-realm-agent` 仓转私有，本文是私库化后**装机/升级/分发全链路**的排障与维护手册。
> 配套：[REALM_EGRESS_NODE_OPS_GUIDE.md](REALM_EGRESS_NODE_OPS_GUIDE.md)（节点日常运维）。

---

## 1. 一页看懂：私库化后东西从哪来

```
push main ──► GitHub Actions (build-agent.yml, 私库计分钟数)
                 ├─ test + build(amd64/arm64) + manifest.json(版本+sha256 真源)
                 ├─ sync-dl ──受限SSH key──► situs-web(13.214.21.114) /var/www/dl/
                 │      发布物: install.sh / agent-linux-{amd64,arm64} / updater.sh / manifest.json
                 │      末步公网闭环验证 manifest version（红了=没发上去）
                 └─ update-latest ──► GitHub "latest" release（仅维护者可见，归档用）

装机:  curl -H "Authorization: Bearer <DL_TOKEN>" https://situstechnologies.com/dl/install.sh
       └─ install.sh 再用同 token 拉 manifest.json + 二进制 + updater.sh（sha256 校验）
升级:  fleet 建任务 → 节点 updater timer 问 fleet upgrade-plan → 按下发的 /dl/ URL+sha+临时token 拉 → 原子替换
```

**核心事实**：

| 事实 | 说明 |
|---|---|
| `/dl/` 是**唯一**分发源 | GitHub 兜底、源码自编译兜底已从 install.sh 删除（私库匿名不可达，留着只会静默误导） |
| 所有装机必须 `--dl-token` | 海外节点也不例外；缺失 install.sh 直接报错退出 |
| **升级链路与 GitHub 无关** | fleet 从 `/dl/manifest.json` 取真源、向节点下发 /dl/ URL+sha+token，仓库可见性不影响存量节点升级 |
| 发版 = 一次 push main | CI 全自动到 /dl/；**无需人工回填 sha**（manifest 是校验真源） |
| 发版 ≠ 节点升级 | 节点升级要 fleet 显式建任务（admin 升级编排），/dl/ 换新只影响"新装机拿到的版本" |

## 2. 密钥/凭证清单（在哪、谁用、怎么换）

| 凭证 | 真源位置 | 谁用 | 轮换方法 |
|---|---|---|---|
| `DL_TOKEN`（/dl/ Bearer） | situs-web `/etc/nginx/conf.d/dl_auth.conf`（不进 git） | 装机命令、fleet（env `DL_TOKEN`，下发给 updater）、CI secret `DL_TOKEN`（闭环验证用） | 改 nginx map 加新值→同步 fleet env+CI secret→观察后删旧值（map 可并存多 token 平滑轮换） |
| `DL_SSH_KEY`（CI→situs-web 发布） | GitHub 仓 Settings→Secrets（`DL_SSH_KEY/DL_HOST/DL_USER`） | 仅 CI sync-dl | situs-web 侧 `dl-ci` 用户 `authorized_keys`（restrict+强制命令 `/usr/local/bin/dl-publish`）换 key 对；★别与 `DEPLOY_*` secrets 混用 |
| 节点 `NODE_API_KEY` | 建 egress 时 otun-manager 返回 | agent 注册/心跳、updater 问 fleet | 与本文无关，见节点运维手册 |

**服务器侧发布门**：`/usr/local/bin/dl-publish`（situs-web，root:root 0755）。白名单硬编码：`agent-linux-amd64 | agent-linux-arm64 | manifest.json | install.sh | updater.sh`。**新增分发文件必须同时改**：① dl-publish 白名单（situs-web）② CI sync-dl 的 publish 列表——两处不同步 = CI 发布报 `name not in whitelist`。

## 3. 排障速查表（症状 → 查什么 → 常见根因）

### 3.1 装机失败

| 症状 | 查什么 | 常见根因/处置 |
|---|---|---|
| `curl /dl/install.sh` 得 403 | token 与 situs-web `dl_auth.conf` 逐字符比对（Bearer 后无多余空格/引号） | token 抄错/被轮换。从 fleet admin 重新复制 install_command |
| `curl /dl/install.sh` 得 404 | `ssh situs-web 'ls -la /var/www/dl/'` 有没有 install.sh | 私库化后第一次 CI 还没跑完/失败 → 查 Actions；应急走 §4 人工发布 |
| install.sh 报 `--dl-token is required` | — | 预期行为：私库化后必填。老笔记里不带 token 的命令都过时了 |
| install.sh 报 `Download from /dl/ failed` | ① 节点 `curl -v https://situstechnologies.com/dl/manifest.json -H "Authorization: Bearer ..."` ② situs-web nginx 活着吗 ③ CI sync-dl 绿吗 | 按 ①→③ 顺序收敛：token 错(403)/网络不通(超时)/文件缺失(404) |
| `sha256 mismatch` 且重试仍现 | `/dl/manifest.json` 的 version 与二进制是否同批（比对 CI run） | 撞上发布换文件窗口会自愈（重试即可）；持续 mismatch = 有人绕过 dl-publish 手工改了 /var/www/dl → 用 §4 重新原子发布 |
| 拿不到 install.sh 想手工救急 | — | 维护者本机有私库 clone：`scp install.sh 节点:` 再 `sudo bash install.sh --dl-token ...`（二进制仍从 /dl/ 拉，不绕校验） |

### 3.2 CI / 发布失败

| 症状 | 查什么 | 常见根因/处置 |
|---|---|---|
| Actions 全红看不到 | 私库需登录 GitHub 才能看 Actions | 用 antsbtw 账号登录 github.com/antsbtw/otun-realm-agent/actions |
| sync-dl 报 `name not in whitelist` | dl-publish 白名单 vs CI publish 列表 | 两处清单漂移（§2 末段），补 situs-web 白名单 |
| sync-dl 报 `sha256 mismatch ... refused` | CI 日志里 publish 的 sha vs dist 文件 | 传输截断（>64MiB 上限？）或 CI 脚本改坏；dl-publish 拒绝即不落盘，重跑即可 |
| "Verify /dl/ serves the new manifest" 红 | 公网 curl manifest 的 version | 发布没生效：nginx/CF 边缘问题或 DL_TOKEN secret 过期 |
| CI 排队不跑/报计费 | Settings→Billing→Actions 分钟余量 | 私库计分钟（free 2000 分/月）；单次全流水 ~10 分钟，刷爆通常是异常循环触发 |
| push 了 CI 没触发 | 改动是否全是 `.md`（paths-ignore） | 纯文档不发版是预期；install.sh 变更**会**触发（私库化后已移出 ignore） |

### 3.3 升级异常（与私库化无关，但常被混淆）

升级链路（fleet→updater→/dl/）**不经 GitHub**，仓库转私有不影响它。升级问题查：fleet 任务状态（`upgrade-status` 上报）、节点 `journalctl -t realm-updater`、`/dl/manifest.json` version。⚠️ 升级任务须先 cancel 再建新的（见 [[project_realm_all_regions_delivery_design]]）。

## 4. 人工应急发布（CI 不可用时）

CI 挂了但急需发版/补文件时，用维护者机器模拟 CI（**走 dl-publish 门，不要 scp 直写 /var/www/dl**——绕门=绕 sha 校验+原子性）：

```bash
# 1. 本机构建（须 -tags with_utls）
cd services/otun-realm-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags with_utls \
  -ldflags "-s -w -X main.buildVersion=$(git rev-parse HEAD)" -o agent-linux-amd64 ./cmd/agent

# 2. 经 dl-publish 强制命令发布（须持 CI 部署 key 或 situs-web 上 root 直跑 dl-publish 语义）
sha=$(sha256sum agent-linux-amd64 | cut -d' ' -f1)
ssh -i <dl_key> dl-ci@13.214.21.114 "put agent-linux-amd64 $sha" < agent-linux-amd64
# manifest.json 最后发（二进制先、manifest 后——节点侧任何时刻校验都自洽）
```

没有部署 key 时：root SSH 上 situs-web，`echo "put <name> <sha>" 走不通就手工复刻 dl-publish 逻辑`——临时文件写 `/var/www/dl/.upload.xxx` → `sha256sum -c` → `chmod 0644` → `mv -f`（**必须 rename 原子替换，不许 cp 覆盖**）。

## 5. 仓库可见性回滚（万一要转回公开）

GitHub → Settings → Danger Zone → Change visibility → Public。之后**功能上什么都不用改**（/dl/ 唯一源的模型公开仓下照样成立），但注意：
- install.sh 已无 GitHub 兜底，转公开**不会**自动恢复兜底；若要恢复，revert agent 仓 commit `8d33eb7`。
- 转公开 = 六协议实现/会合面交互/装机参数重新暴露，转回前想清楚当初私有化的理由（威胁模型：渗透+封锁对抗）。

## 6. 历史与决策记录

| 日期 | 事项 |
|---|---|
| 2026-07-19 | 仓库转私有（安全决策：不再向对抗方暴露六协议实现与分发细节）。配套：install.sh 删 GitHub/源码兜底、`--dl-token` 必填（agent 仓 `8d33eb7`）；CI sync-dl 增发 install.sh；fleet 装机指令改 /dl/+token（fleet 仓 `7014900`）。dl-publish 白名单早在 U3 预留了 install.sh，服务器零改动 |
| 此前 | U1：manifest.json 成为 sha 真源，废人肉回填；U3：updater timer 自动升级 + dl-publish 受限发布门 |
