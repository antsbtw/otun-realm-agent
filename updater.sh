#!/bin/bash
# otun-agent-updater —— realm 节点侧升级执行器（U3，设计 AGENT_UPGRADE_CICD_DESIGN §6）。
#
# 为什么独立于 agent（架构约束，不可绕过）：
#   ① agent 不能替换正在运行的自己（进程持有自身二进制）；
#   ② 本脚本独立调 fleet（同一把 api_key，从 agent unit 读取）——agent 挂了 timer 依然
#      能拉到 plan 把它救回来（这类故障留在第 1 层，不下沉到第 2 层）；
#   ③ 升级逻辑独立迭代，不用改 agent。
#
# 四条红线（三层维护模型推导，一条都不能破）：
#   R1 断电重启必须能自愈 → 只用 rename 原子替换：任何时刻断电，agent 要么整旧要么整新，
#      绝无半截二进制 → 重启一定能起来 → 第 3 层非专业人士"拔电重启"能救。
#   R2 绝不碰 cloudflared / ssh / 系统层 → 本脚本只动 $INSTALL_DIR/agent 和 updater 自己
#      的两个 unit。SSH 隧道永远在 → 第 2 层永远可用。
#   R3 失败不静默 → 每一步状态上报 fleet（upgrade-status），失败带 last_error。
#   R4 失败自动回滚 → 减少下沉到第 2 层的次数（不是防砖，SSH 进得去）。
#
# 幂等：timer 每 5 分钟跑一遍。无任务 → 静默退出；已是目标版本（文件 sha 相等）→ 报
# succeeded 退出；同一 unit 由 systemd 保证不并发。
#
# 用法：
#   updater.sh            —— timer 调用：拉 plan → 升级
#   updater.sh --install  —— 安装/更新自己的 systemd timer+service 并启用（幂等；
#                            install.sh 与存量节点 bootstrap 都走这里，unit 单一真源）
set -u

# 生产固定路径；UPDATER_* 环境变量仅供测试注入（沙箱路径 + mock systemctl），
# systemd unit 不传它们 → 生产恒用默认值。
INSTALL_DIR="${UPDATER_INSTALL_DIR:-/opt/otun-realm-agent}"
AGENT_UNIT="${UPDATER_AGENT_UNIT:-/etc/systemd/system/otun-realm-agent.service}"
UNIT_DIR="${UPDATER_UNIT_DIR:-/etc/systemd/system}"
SYSTEMCTL="${UPDATER_SYSTEMCTL:-systemctl}"
JOURNALCTL="${UPDATER_JOURNALCTL:-journalctl}"
AGENT_SVC=otun-realm-agent

log() { echo "[otun-agent-updater] $1"; }   # stdout → journalctl -u otun-agent-updater
# pause N：生产 = sleep N；测试注入 UPDATER_SLEEP_SCALE=0 免等待。
pause() { sleep $(( $1 * ${UPDATER_SLEEP_SCALE:-1} )); }

# ============ --install：写自己的 unit（单一真源，install.sh 不再抄一遍） ============
if [ "${1:-}" = "--install" ]; then
    cat > "$UNIT_DIR/otun-agent-updater.service" << 'UNIT'
[Unit]
Description=OTun Realm Agent updater (pull upgrade plan from fleet)
# ★R2：与 cloudflared / ssh 完全无关的独立 unit。只动 agent 二进制与自身。
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/opt/otun-realm-agent/updater.sh
UNIT
    cat > "$UNIT_DIR/otun-agent-updater.timer" << 'UNIT'
[Unit]
Description=Run otun-agent-updater every 5 minutes

[Timer]
# 开机 2 分钟后首跑（等 DNS/网络就绪），此后每 5 分钟；30s 随机抖动防全网同拍打 fleet。
OnBootSec=2min
OnUnitActiveSec=5min
RandomizedDelaySec=30

[Install]
WantedBy=timers.target
UNIT
    $SYSTEMCTL daemon-reload
    $SYSTEMCTL enable --now otun-agent-updater.timer
    log "installed + enabled otun-agent-updater.timer (every 5min, boot+2min)"
    exit 0
fi

# ============ 配置：从 agent unit 读（单一真源，不另存一份） ============
if [ ! -f "$AGENT_UNIT" ]; then
    log "agent unit not found ($AGENT_UNIT) — nothing to update"; exit 0
fi
unit_env() { sed -n "s/^Environment=\"$1=\(.*\)\"\$/\1/p" "$AGENT_UNIT" | head -1; }

NODE_API_KEY=$(unit_env NODE_API_KEY)
FLEET_URL=$(unit_env FLEET_API_URL)
[ -z "$FLEET_URL" ] && FLEET_URL=$(unit_env OTUN_API_URL)   # 老节点回退（otun 无此端点 → 下面 404 按无任务处理）
if [ -z "$NODE_API_KEY" ] || [ -z "$FLEET_URL" ]; then
    log "NODE_API_KEY / FLEET_API_URL not found in agent unit — cannot query fleet"; exit 0
fi

case "$(uname -m)" in
    x86_64)  ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *) log "unsupported arch $(uname -m)"; exit 0 ;;
esac

# ============ 上报（best-effort：报不上不阻断本地流程，fleet 侧有 30min 超时兜底） ============
TASK_ID=""
report() { # report <state> [error]
    local state="$1" err="${2:-}"
    [ -z "$TASK_ID" ] && return 0
    err=$(printf '%s' "$err" | tr -d '"\\' | head -c 500)   # 进 JSON 前去引号/反斜杠
    curl -fsS -m 15 -X POST \
        -H "Authorization: Bearer $NODE_API_KEY" -H "Content-Type: application/json" \
        -d "{\"task_id\":$TASK_ID,\"state\":\"$state\",\"to_version\":\"$VERSION\",\"error\":\"$err\"}" \
        "$FLEET_URL/api/node/upgrade-status" > /dev/null 2>&1 || log "WARN: report $state failed (fleet timeout will cover)"
}

# ============ 1. 问 fleet：我该升吗 ============
resp=$(curl -fsS -m 20 -H "Authorization: Bearer $NODE_API_KEY" \
    "$FLEET_URL/api/node/upgrade-plan?arch=$ARCH" 2>/dev/null) || { log "fleet unreachable — retry next tick"; exit 0; }

case "$resp" in
    *'"upgrade":null'*)
        rm -f "$INSTALL_DIR/agent.prev"   # 无任务：清理上次成功升级留下的备份
        exit 0 ;;
    *'"upgrade":{'*) ;;
    *) log "unexpected upgrade-plan response (endpoint missing on old manager?) — ignoring"; exit 0 ;;
esac

flat=$(printf '%s' "$resp" | tr -d ' \n\t')
TASK_ID=$(printf '%s' "$flat" | sed -n 's/.*"task_id":\([0-9][0-9]*\).*/\1/p')
VERSION=$(printf '%s' "$flat" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
URL=$(printf '%s' "$flat" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
SHA=$(printf '%s' "$flat" | sed -n 's/.*"sha256":"\([0-9a-f]\{64\}\)".*/\1/p')
DL_TOKEN=$(printf '%s' "$flat" | sed -n 's/.*"dl_token":"\([^"]*\)".*/\1/p')

if [ -z "$TASK_ID" ] || [ -z "$VERSION" ] || [ -z "$URL" ] || [ -z "$SHA" ]; then
    log "malformed plan (task=$TASK_ID ver=$VERSION sha=${SHA:0:12}) — refusing"; exit 0
fi
log "plan: task=$TASK_ID version=${VERSION:0:12} url=$URL"

file_sha() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

# ============ 2. 幂等：已是目标版本（文件 sha 相等 = 比对二进制本体，比问版本号更硬） ============
if [ "$(file_sha "$INSTALL_DIR/agent")" = "$SHA" ]; then
    if $SYSTEMCTL is-active --quiet "$AGENT_SVC"; then
        log "already at target — reporting succeeded"
        report succeeded
    else
        # 二进制已是新版但 agent 没跑（上次升级后挂了/断电重启失败）→ timer 救它（架构约束②）。
        log "binary at target but agent inactive — restarting (updater rescues agent)"
        $SYSTEMCTL restart "$AGENT_SVC"
        pause 10
        if $SYSTEMCTL is-active --quiet "$AGENT_SVC"; then report succeeded; else report failed "binary at target but agent won't start (needs layer-2 SSH)"; fi
    fi
    exit 0
fi

# ============ 3. 下载到临时文件（token 由 fleet 随 plan 下发，不常驻节点） ============
report downloading
rm -f "$INSTALL_DIR/agent.new"
if ! curl -fsSL -m 600 -H "Authorization: Bearer $DL_TOKEN" "$URL" -o "$INSTALL_DIR/agent.new"; then
    rm -f "$INSTALL_DIR/agent.new"
    log "download failed"; report failed "download failed: $URL"
    exit 0   # 机器完好，仍跑旧版；下一 tick 重试（attempt 计数在 fleet 侧可见）
fi

# ============ 4. sha256 硬门禁（★绝不继承 install.sh 无预期值放行的宽松语义） ============
got=$(file_sha "$INSTALL_DIR/agent.new")
if [ "$got" != "$SHA" ]; then
    rm -f "$INSTALL_DIR/agent.new"
    log "sha256 mismatch (got ${got:0:12} want ${SHA:0:12}) — REFUSED"
    report failed "sha256 mismatch: got ${got:0:12} want ${SHA:0:12}"
    exit 0
fi
chmod +x "$INSTALL_DIR/agent.new"

# ============ 5. 原子替换（R1 核心：rename 原子，断电无半截） ============
cp -a "$INSTALL_DIR/agent" "$INSTALL_DIR/agent.prev"    # 备份，回滚用
mv "$INSTALL_DIR/agent.new" "$INSTALL_DIR/agent"        # ★rename：要么旧要么新
report applied
$SYSTEMCTL restart "$AGENT_SVC"
report verifying

# ============ 6. 验证新版真的健康（进程起来只是起点，还要活得稳） ============
# 两段观察（10s+20s）抓 crash-loop：坏二进制会被 systemd Restart=always 反复拉起，
# 单次 is-active 会给假阳性。更强的门禁（版本上报 + 会合面六 slot 注册）在 fleet 侧
# （U2 编排 loop fail-closed），本地只负责"进程稳定活着"。
ok=1
pause 10; $SYSTEMCTL is-active --quiet "$AGENT_SVC" || ok=0
if [ "$ok" = "1" ]; then pause 20; $SYSTEMCTL is-active --quiet "$AGENT_SVC" || ok=0; fi

if [ "$ok" = "1" ]; then
    rm -f "$INSTALL_DIR/agent.prev"
    log "upgrade OK: now at ${VERSION:0:12}"
    report succeeded
    exit 0
fi

# ============ 7. 失败自动回滚（R4）→ 上报 rolled_back + 原因（R3） ============
log "new binary unhealthy — rolling back"
err=$($JOURNALCTL -u "$AGENT_SVC" -n 5 --no-pager 2>/dev/null | tail -3 | tr '\n' ';')
mv "$INSTALL_DIR/agent.prev" "$INSTALL_DIR/agent"
$SYSTEMCTL restart "$AGENT_SVC"
pause 5
if $SYSTEMCTL is-active --quiet "$AGENT_SVC"; then
    log "rollback OK — still on old version"
    report rolled_back "new binary crashed after restart, rolled back OK; journal: $err"
else
    # 回滚后仍起不来（极端）：机器仍可 SSH（R2 保证），交第 2 层。
    log "ROLLBACK ALSO UNHEALTHY — needs layer-2 SSH"
    report failed "rollback also unhealthy — needs layer-2 SSH; journal: $err"
fi
exit 0
