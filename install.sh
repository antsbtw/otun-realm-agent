#!/bin/bash
set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  OTun Realm Agent Installer v2.0.0${NC}"
echo -e "${GREEN}  six-protocol over-realm egress (egress-inproc)${NC}"
echo -e "${GREEN}========================================${NC}"

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Please run as root (sudo)${NC}"
    exit 1
fi

# 清理已有环境
echo -e "${YELLOW}Cleaning up existing installation...${NC}"
systemctl stop otun-realm-agent 2>/dev/null || true
systemctl disable otun-realm-agent 2>/dev/null || true
pkill -9 otun-realm-agent 2>/dev/null || true
sleep 1
rm -f /opt/otun-realm-agent/agent 2>/dev/null || true
rm -f /etc/systemd/system/otun-realm-agent.service 2>/dev/null || true
systemctl daemon-reload
echo -e "${GREEN}Cleanup completed${NC}"

# 安装必要依赖
echo -e "${GREEN}Installing dependencies...${NC}"
apt-get update -qq
apt-get install -y -qq curl

# ============ 参数（realm 六协议身份 + 首启引导最小集）============
# realm_token/stun/obfs/sni/dns/protocols/reality 密钥全部由 manager 下发，故意不带（动态可调）。
# ★六协议本地端口 agent 用约定常量（hy2=51820...vmess=51825），不从参数传、不对外、不进 manager。
NODE_API_KEY=""
NODE_ID="realm-$(hostname)"
API_URL="https://otun-manager-v3.situstechnologies.com"
# ★Batch 5：fleet-manager 地址（纳管 register/heartbeat 直连 fleet）。
# 默认空 → agent 侧 NewSyncer 回退用 OTUN_API_URL（不传 --fleet-url = 旧行为，register/heartbeat 仍打 otun，零回归）。
FLEET_URL=""
REALM_ID=""
REGION=""
OBS_ENDPOINT=""
# 二进制分发 token（/dl/ 唯一源）：env 默认，--dl-token 参数覆盖。
# 仅装机时用于拉二进制，不写进 systemd/配置，运行期不需要。
# ★2026-07-19 agent 仓转私有：GitHub release / 源码 clone 匿名均不可达，/dl/ 是唯一
#   分发源，所有节点（含海外）装机必须带 token。token 真源：situs-web
#   /etc/nginx/conf.d/dl_auth.conf（fleet admin 生成的装机指令已自带）。
DL_TOKEN="${DL_TOKEN:-}"

while [[ $# -gt 0 ]]; do
    case $1 in
        --api-key)      NODE_API_KEY="$2"; shift 2 ;;
        --node-id)      NODE_ID="$2"; shift 2 ;;
        --api-url)      API_URL="$2"; shift 2 ;;
        --fleet-url)    FLEET_URL="$2"; shift 2 ;;
        --realm-id)     REALM_ID="$2"; shift 2 ;;
        --region)       REGION="$2"; shift 2 ;;
        --obs-endpoint) OBS_ENDPOINT="$2"; shift 2 ;;
        --dl-token)     DL_TOKEN="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "$NODE_API_KEY" ]; then
    echo -e "${RED}Error: --api-key is required${NC}"
    echo "Usage: $0 --api-key <key> --realm-id <id> --dl-token <token> [--node-id <id>] [--region <r>] [--api-url <url>] [--fleet-url <url>] [--obs-endpoint <url>]"
    exit 1
fi
if [ -z "$REALM_ID" ]; then
    echo -e "${RED}Error: --realm-id is required (realm slot base id, e.g. iptv-sg-01)${NC}"
    exit 1
fi
if [ -z "$DL_TOKEN" ]; then
    echo -e "${RED}Error: --dl-token is required (agent repo is private; /dl/ is the only distribution source)${NC}"
    echo "Token source: situs-web /etc/nginx/conf.d/dl_auth.conf, or copy the install command from fleet admin."
    exit 1
fi

echo -e "${YELLOW}Node ID:    ${NODE_ID}${NC}"
echo -e "${YELLOW}Realm ID:   ${REALM_ID} (base; each protocol registers <base>-<proto>)${NC}"
echo -e "${YELLOW}Region:     ${REGION:-(auto GeoIP)}${NC}"
echo -e "${YELLOW}Protocols:  pushed by manager; local ports hy2=51820 tuic=51821 reality=51822 trojan=51823 ss=51824 vmess=51825${NC}"

INSTALL_DIR="/opt/otun-realm-agent"
mkdir -p "$INSTALL_DIR/data/certs"

ARCH=$(uname -m)
case $ARCH in
    x86_64)  AGENT_ARCH="amd64" ;;
    aarch64) AGENT_ARCH="arm64" ;;
    *) echo -e "${RED}Unsupported architecture: $ARCH${NC}"; exit 1 ;;
esac

# ============ 二进制分发：/dl/ 唯一源 + sha256 校验 ============
# ★仓库已私有（2026-07-19）：GitHub release 匿名下载、源码匿名 clone 均物理失效，
#   旧的"GitHub 兜底 / 源码自编译兜底"已删。/dl/（situs-web，经 portal 子域 CF 边缘，
#   全球含中国可达）是唯一分发源；拉不到即装机中断（不再有静默降级），排障看
#   backend-v3 仓 document/realm/ops/AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md。
#   ★主域名已切 CF Tunnel 绕过 situs-web，/dl/ 只在 portal 子域可达，勿改回主域名。
DL_BASE="https://portal.situstechnologies.com/dl"
# 预期 sha256：由 CI 发布的 manifest.json 提供（与二进制同批生成、原子发布），
# 不硬编码在本脚本里（旧模式人肉回填 sha 有自指漂移问题）。
declare -A DL_SHA256=()

# _load_manifest：拉 manifest.json → 填充 DL_SHA256。拉不到时保持空 map
# （_verify_sha256 无预期值放行 = 维持旧装机宽松语义；⚠️ 升级链路（updater）绝不
# 走这条宽松路径——它的 sha 由 fleet 下发、不匹配即硬失败）。
# 解析不依赖 jq（节点是干净 Debian）：靠 CI 生成的固定字段顺序 "file":..,"sha256":..（契约）。
_load_manifest() {
    local tmp
    tmp=$(mktemp)
    if curl -fsSL -H "Authorization: Bearer ${DL_TOKEN}" "$DL_BASE/manifest.json" -o "$tmp" 2>/dev/null; then
        echo -e "${GREEN}  manifest: /dl/${NC}"
    else
        rm -f "$tmp"
        echo -e "${YELLOW}  manifest unavailable from /dl/ — proceeding without sha256 pinning${NC}"
        return 0
    fi
    local flat name sha
    flat=$(tr -d ' \t\n' < "$tmp")
    rm -f "$tmp"
    for name in agent-linux-amd64 agent-linux-arm64 updater.sh; do
        sha=$(printf '%s' "$flat" | sed -n 's/.*"file":"'"$name"'","sha256":"\([0-9a-f]\{64\}\)".*/\1/p')
        [ -n "$sha" ] && DL_SHA256[$name]="$sha"
    done
    local ver
    ver=$(printf '%s' "$flat" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    echo -e "${GREEN}  manifest version: ${ver:0:12}${NC}"
}
_load_manifest

# _verify_sha256 <file> <artifact-name> -> 0 通过 / 1 不过（无预期值时跳过校验、视为通过）
_verify_sha256() {
    local file="$1" name="$2" expect="${DL_SHA256[$2]:-}"
    [ -z "$expect" ] && return 0   # 无预置 sha256（不该发生）→ 不阻断
    local got
    got=$(sha256sum "$file" | cut -d' ' -f1)
    if [ "$got" != "$expect" ]; then
        echo -e "${YELLOW}  sha256 mismatch for $name (got ${got:0:12}…, want ${expect:0:12}…)${NC}"
        return 1
    fi
    return 0
}

# fetch_binary <artifact-name> <out-path> -> 0 成功且校验过 / 1 失败（/dl/ 唯一源，无兜底）
# token 不进日志（curl -H 不回显）。
fetch_binary() {
    local name="$1" out="$2"
    echo -e "${GREEN}  /dl/: $name${NC}"
    if curl -fsSL -H "Authorization: Bearer ${DL_TOKEN}" "$DL_BASE/$name" -o "$out" \
       && _verify_sha256 "$out" "$name"; then
        return 0
    fi
    rm -f "$out"
    return 1
}

# ============ realm-agent 二进制（egress 内嵌六协议版）============
# ★egress 库化后 agent 不再需要任何外部代理引擎二进制：六协议引擎由 egress 库进程内驱动，
#   无 exec-fork、无落盘引擎配置文件、无 v2ray_api/clash_api/hotreload IPC。
# ★（维护者手动编译时必须带 -tags with_utls——reality 借壳握手需要 utls。装机路径
#   不再源码自编：私库匿名 clone 不可达，兜底已删。）
echo -e "${GREEN}Downloading OTun Realm Agent (egress-inproc, six-protocol)...${NC}"
if fetch_binary "agent-linux-${AGENT_ARCH}" "$INSTALL_DIR/agent"; then
    chmod +x "$INSTALL_DIR/agent"
    echo -e "${GREEN}Agent downloaded${NC}"
else
    echo -e "${RED}Download from /dl/ failed — no fallback (repo is private).${NC}"
    echo -e "${RED}Check: dl-token valid? situs-web /dl/ reachable? CI sync-dl green?${NC}"
    echo -e "${RED}Runbook: backend-v3 document/realm/ops/AGENT_PRIVATE_REPO_DISTRIBUTION_RUNBOOK.md${NC}"
    exit 1
fi
[ -f "$INSTALL_DIR/agent" ] || { echo -e "${RED}Agent binary not found${NC}"; exit 1; }
chmod +x "$INSTALL_DIR/agent"
# 六协议本地 UDP 端口均 > 1024，无需 CAP_NET_BIND_SERVICE。
echo -e "${GREEN}Agent ready${NC}"

# ============ updater（U3）：独立 systemd timer 自动升级 ============
# ★独立于 agent（agent 不能替换正在运行的自己；agent 挂了 timer 能救它）。
# unit 文件由 updater.sh --install 自己写（单一真源，本脚本不抄一遍——slot 短名跨仓
# 双写的教训）。拿不到 updater 不阻断装机：agent 照常服务，之后可单独 bootstrap。
echo -e "${GREEN}Installing agent updater (self-upgrade timer)...${NC}"
if fetch_binary "updater.sh" "$INSTALL_DIR/updater.sh"; then
    chmod +x "$INSTALL_DIR/updater.sh"
else
    echo -e "${YELLOW}updater.sh unavailable from /dl/ — skipping (agent unaffected; bootstrap later)${NC}"
fi

# ============ systemd 服务（六协议 env；固定 MANAGEMENT_MODE=remote）============
# ★去掉 HY2_PORT / REALM_SERVER_URL / 旧引擎相关 env：端口是 agent 本地常量，server_url 由
#   manager 下发，六协议引擎内嵌无需外部代理二进制。
ENV_VARS="Environment=\"NODE_API_KEY=$NODE_API_KEY\"
Environment=\"NODE_ID=$NODE_ID\"
Environment=\"OTUN_API_URL=$API_URL\"
Environment=\"MANAGEMENT_MODE=remote\"
Environment=\"REALM_ID=$REALM_ID\""
# ★Batch 5：仅当传了 --fleet-url 才注入 FLEET_API_URL；不传则不写该 env → agent 回退 OTUN_API_URL（零回归）。
if [ -n "$FLEET_URL" ]; then
    ENV_VARS="$ENV_VARS
Environment=\"FLEET_API_URL=$FLEET_URL\""
fi
if [ -n "$REGION" ]; then
    ENV_VARS="$ENV_VARS
Environment=\"REALM_REGION=$REGION\""
fi
if [ -n "$OBS_ENDPOINT" ]; then
    ENV_VARS="$ENV_VARS
Environment=\"OBS_ENDPOINT=$OBS_ENDPOINT\""
fi

cat > /etc/systemd/system/otun-realm-agent.service << SYSTEMD
[Unit]
Description=OTun Realm Agent (six-protocol egress)
# ★DNS 就绪后再启动。根因：agent 启动即做 manager sync + STUN 域名解析（会合面注册
# 依赖）。开机时若网络/DHCP 未就绪、resolv.conf 未写好 → Go 解析器 fallback [::1]:53
# → connection refused → STUN 域名解析失败 → 会合面注册发不出且不自愈 → 永久卡死。
# 不同网络环境 DHCP/DNS 就绪时间不同，故不赌 systemd target（本机 networkd/resolved
# 均未启用，network-online/nss-lookup 可能空转过早 active）。改用 ExecStartPre 主动
# 探测：循环 getent 解析关键域名，能解析（DNS 真正可用）才放行 agent。环境无关、鲁棒。
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
$ENV_VARS
# 启动前等 DNS 真正可解析（最多 120s，超时也放行，交由 agent 自身重试兜底）。
ExecStartPre=/bin/sh -c 'i=0; until getent hosts otun-manager-v3.situstechnologies.com >/dev/null 2>&1 || [ \$i -ge 120 ]; do i=\$((i+1)); sleep 1; done'
ExecStart=$INSTALL_DIR/agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SYSTEMD

systemctl daemon-reload
systemctl enable otun-realm-agent
systemctl start otun-realm-agent

# updater timer（agent unit 写好后再装：updater 运行期从 agent unit 读 api_key/fleet url）。
if [ -x "$INSTALL_DIR/updater.sh" ]; then
    "$INSTALL_DIR/updater.sh" --install
fi

# 管理命令
cat > /usr/local/bin/realm << 'CMD'
#!/bin/bash
case "$1" in
    start)   systemctl start otun-realm-agent ;;
    stop)    systemctl stop otun-realm-agent ;;
    restart) systemctl restart otun-realm-agent ;;
    status)  systemctl status otun-realm-agent ;;
    logs)    journalctl -u otun-realm-agent -f ;;
    *)       echo "Usage: realm {start|stop|restart|status|logs}" ;;
esac
CMD
chmod +x /usr/local/bin/realm

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Installation Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo -e "Node ID:  ${YELLOW}$NODE_ID${NC}"
echo -e "Realm ID: ${YELLOW}$REALM_ID${NC} (base)"
echo -e "Data:     ${YELLOW}$INSTALL_DIR/data${NC}"
echo ""
echo -e "Commands:"
echo -e "  ${YELLOW}realm status${NC}  - Check service status"
echo -e "  ${YELLOW}realm logs${NC}    - View logs"
echo -e "  ${YELLOW}realm restart${NC} - Restart service"
echo ""
echo -e "${YELLOW}Note:${NC} protocols / realm token / stun / obfs / ss_method / reality keys / sni / dns"
echo -e "      are all pushed by manager via /api/node/users (dynamic config); not set at install time."
echo -e "      Six-protocol local UDP ports are agent-internal constants (not exposed, not in manager)."
