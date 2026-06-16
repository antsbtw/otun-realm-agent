#!/bin/bash
set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  OTun Realm Agent Installer v1.0.0${NC}"
echo -e "${GREEN}  hy2 + realm{} residential egress${NC}"
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
pkill -9 sing-box 2>/dev/null || true
pkill -9 otun-realm-agent 2>/dev/null || true
sleep 1
rm -f /opt/otun-realm-agent/agent 2>/dev/null || true
rm -f /etc/sing-box/config.json 2>/dev/null || true
rm -f /etc/systemd/system/otun-realm-agent.service 2>/dev/null || true
systemctl daemon-reload
echo -e "${GREEN}Cleanup completed${NC}"

# 安装必要依赖
echo -e "${GREEN}Installing dependencies...${NC}"
apt-get update -qq
apt-get install -y -qq curl

# ============ 参数（§7.7.1 realm 参数全集）============
# 身份 + 首启引导最小集；realm_token/stun/obfs/sni/dns 由 manager 下发，故意不带（§4.1 动态可调）。
NODE_API_KEY=""
NODE_ID="realm-$(hostname)"
API_URL="https://otun-manager-v3.situstechnologies.com"
HY2_PORT=51820
REALM_ID=""
REALM_SERVER_URL="https://situstechnologies.com/realm"
REGION=""
OBS_ENDPOINT=""
# 二进制分发 token（方案A /dl/ 镜像源）：env 默认，--dl-token 参数覆盖。
# 仅装机时用于拉二进制，不写进 systemd/配置，运行期不需要。
# 不带 token 时自动跳过 /dl/、直接走 GitHub（海外/不受 GFW 节点）。
DL_TOKEN="${DL_TOKEN:-}"

while [[ $# -gt 0 ]]; do
    case $1 in
        --api-key)          NODE_API_KEY="$2"; shift 2 ;;
        --node-id)          NODE_ID="$2"; shift 2 ;;
        --api-url)          API_URL="$2"; shift 2 ;;
        --hy2-port)         HY2_PORT="$2"; shift 2 ;;
        --realm-id)         REALM_ID="$2"; shift 2 ;;
        --realm-server-url) REALM_SERVER_URL="$2"; shift 2 ;;
        --region)           REGION="$2"; shift 2 ;;
        --obs-endpoint)     OBS_ENDPOINT="$2"; shift 2 ;;
        --dl-token)         DL_TOKEN="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "$NODE_API_KEY" ]; then
    echo -e "${RED}Error: --api-key is required${NC}"
    echo "Usage: $0 --api-key <key> --realm-id <id> [--node-id <id>] [--region <r>] [--hy2-port <port>] [--realm-server-url <url>] [--obs-endpoint <url>]"
    exit 1
fi
if [ -z "$REALM_ID" ]; then
    echo -e "${RED}Error: --realm-id is required (realm slot identifier, e.g. iptv-cn-sh)${NC}"
    exit 1
fi

echo -e "${YELLOW}Node ID:    ${NODE_ID}${NC}"
echo -e "${YELLOW}Realm ID:   ${REALM_ID}${NC}"
echo -e "${YELLOW}HY2 Port:   ${HY2_PORT}${NC}"
echo -e "${YELLOW}Region:     ${REGION:-(auto GeoIP)}${NC}"

INSTALL_DIR="/opt/otun-realm-agent"
mkdir -p $INSTALL_DIR/data/certs /etc/sing-box

ARCH=$(uname -m)
case $ARCH in
    x86_64)  AGENT_ARCH="amd64"; SINGBOX_ARCH="amd64" ;;
    aarch64) AGENT_ARCH="arm64"; SINGBOX_ARCH="arm64" ;;
    *) echo -e "${RED}Unsupported architecture: $ARCH${NC}"; exit 1 ;;
esac

# ============ 二进制分发：/dl/ 镜像（方案A）+ GitHub 兜底 + sha256 校验 ============
# 下载优先级：
#   1) https://situstechnologies.com/dl/<artifact>（带 token，绕开 GFW，中国节点用）
#   2) GitHub release（海外/不受 GFW 节点天然走这层；/dl/ 失败也回退到这）
#   3) 调用方自行的源码编译兜底（fetch_binary 全失败返回非 0，由调用处接管）
# 每层下载后做 sha256 校验，校验不过视同该层失败、继续往下兜底。
DL_BASE="https://situstechnologies.com/dl"
# 预期 sha256（随脚本走 git = 可信来源；勿与二进制放同目录）。
# ⚠️ agent tag=latest 滚动：每次 agent 发版，下面两行 agent-* 的 sha256 需同步更新。
#    sing-box 固定 tag，不变。
declare -A DL_SHA256=(
    [sing-box-linux-amd64]=efd99e964718219bad3897daecb35b8ebca219fc8165cde55ead112c9a4c1597
    [sing-box-linux-arm64]=a73719b7d7b83399845fa8ba1623529b480815263684d208eb182bc248afdaf5
    [agent-linux-amd64]=aaf297d8bade7c87f502977ef2e18d14ee9c78aca5f9f9c5a5f8df8d8f43ce58
    [agent-linux-arm64]=2339a4e166b6e11ee86a48525f528ce61afb7a09dbde3bbed8da9dde3dcc190f
)

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

# fetch_binary <artifact-name> <github-url> <out-path> -> 0 成功且校验过 / 1 全失败（调用方走源码编译）
# token 不进日志（curl -H 不回显）。
fetch_binary() {
    local name="$1" gh_url="$2" out="$3"
    # 第 1 层：/dl/（仅当有 token）
    if [ -n "$DL_TOKEN" ]; then
        echo -e "${GREEN}  [1/2] /dl/ mirror: $name${NC}"
        if curl -fsSL -H "Authorization: Bearer ${DL_TOKEN}" "$DL_BASE/$name" -o "$out" \
           && _verify_sha256 "$out" "$name"; then
            return 0
        fi
        echo -e "${YELLOW}  /dl/ failed or sha256 mismatch, falling back to GitHub${NC}"
        rm -f "$out"
    fi
    # 第 2 层：GitHub release
    echo -e "${GREEN}  [2/2] GitHub: $name${NC}"
    if curl -fsSL "$gh_url" -o "$out" && _verify_sha256 "$out" "$name"; then
        return 0
    fi
    rm -f "$out"
    return 1   # 两层都失败 → 调用方走源码编译
}

# ============ sing-box（★带 realm 打洞 + v2ray_api 计费 + hy2 热更，自编译预发布）============
# 照搬 otun-node-agent 的方案：realm-agent 自己用 build-singbox 工作流编译
# “1.14.0-alpha.25 源码（realm 默认）+ with_v2ray_api（per-user 计费）+ with_quic（hy2）+ WP-1 热更 patch”，
# 发布到 realm-agent 自己的 release（tag singbox-v<版本>，资产为裸二进制）。
# 不复用 node-agent 的二进制（那个版本旧、无 realm）；也不用官方预编译包（那个无 v2ray_api 也无热更）。
#
# ★WP-3：sing-box 源已切到 antsbtw/sing-box fork（分支 realm-hot-reload，含 WP-1 hy2 运行时热更端点）。
#   主路径（预编译）：URL 不变，但其 release 资产由 build-singbox 工作流从 fork 重编出（已切源）。
#   兜底路径（源码自编译）：必须 clone fork 而非 SagerNet 官方——否则编出无热更端点的官方版，
#   agent 调 127.0.0.1:10086/hotreload/users 全 404/拒绝、generator 的 hot_reload 配置还会 check 失败。
echo -e "${GREEN}Downloading sing-box (realm + v2ray_api + hot-reload)...${NC}"
SINGBOX_VERSION="1.14.0-alpha.25"
SINGBOX_FORK_BRANCH="realm-hot-reload"
SINGBOX_URL="https://github.com/antsbtw/otun-realm-agent/releases/download/singbox-v${SINGBOX_VERSION}/sing-box-linux-${SINGBOX_ARCH}"
if ! fetch_binary "sing-box-linux-${SINGBOX_ARCH}" "$SINGBOX_URL" /usr/local/bin/sing-box; then
    echo -e "${YELLOW}Pre-built download failed, building from source (antsbtw fork w/ hot-reload)...${NC}"
    apt-get install -y -qq git
    # fork 的 go.mod 已降到 go 1.24.7；用 1.25 工具链编译兼容（向下兼容 1.24 module）。
    GO_VERSION="1.25.0"
    rm -rf /usr/local/go
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${AGENT_ARCH}.tar.gz" -o /tmp/go.tar.gz
    tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    cd /tmp && rm -rf sing-box-src
    git clone --depth 1 --branch "$SINGBOX_FORK_BRANCH" https://github.com/antsbtw/sing-box.git sing-box-src
    cd sing-box-src
    go build -tags "with_v2ray_api,with_clash_api,with_quic,with_utls" -o /usr/local/bin/sing-box ./cmd/sing-box
    cd /tmp && rm -rf sing-box-src
fi
cd "$INSTALL_DIR"
chmod +x /usr/local/bin/sing-box
setcap cap_net_bind_service=+ep /usr/local/bin/sing-box 2>/dev/null || true
if ! sing-box version > /dev/null 2>&1; then
    echo -e "${RED}sing-box verification failed${NC}"; exit 1
fi
echo -e "${GREEN}sing-box installed: $(sing-box version | head -1)${NC}"

# ============ realm-agent 二进制 ============
echo -e "${GREEN}Downloading OTun Realm Agent...${NC}"
AGENT_URL="https://github.com/antsbtw/otun-realm-agent/releases/download/latest/agent-linux-${AGENT_ARCH}"
if fetch_binary "agent-linux-${AGENT_ARCH}" "$AGENT_URL" "$INSTALL_DIR/agent"; then
    chmod +x $INSTALL_DIR/agent
    echo -e "${GREEN}Agent downloaded${NC}"
else
    echo -e "${YELLOW}Download failed, building from source...${NC}"
    apt-get install -y -qq git
    export PATH=$PATH:/usr/local/go/bin
    command -v go >/dev/null 2>&1 || { echo -e "${RED}Go not available for source build${NC}"; exit 1; }
    cd /tmp && rm -rf otun-realm-agent-src
    git clone https://github.com/antsbtw/otun-realm-agent.git otun-realm-agent-src
    cd otun-realm-agent-src
    go build -o $INSTALL_DIR/agent ./cmd/agent
    cd /tmp && rm -rf otun-realm-agent-src
fi
[ -f "$INSTALL_DIR/agent" ] || { echo -e "${RED}Agent binary not found${NC}"; exit 1; }
echo -e "${GREEN}Agent ready${NC}"

# 初始最小配置（agent 首启会用下发配置覆盖）
cat > /etc/sing-box/config.json << 'CONF'
{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}
CONF

# ============ systemd 服务（§7.7.1 环境变量映射，固定 MANAGEMENT_MODE=remote）============
ENV_VARS="Environment=\"NODE_API_KEY=$NODE_API_KEY\"
Environment=\"NODE_ID=$NODE_ID\"
Environment=\"OTUN_API_URL=$API_URL\"
Environment=\"MANAGEMENT_MODE=remote\"
Environment=\"REALM_ID=$REALM_ID\"
Environment=\"REALM_SERVER_URL=$REALM_SERVER_URL\"
Environment=\"HY2_PORT=$HY2_PORT\""
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
Description=OTun Realm Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
$ENV_VARS
ExecStart=$INSTALL_DIR/agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SYSTEMD

systemctl daemon-reload
systemctl enable otun-realm-agent
systemctl start otun-realm-agent

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
echo -e "Realm ID: ${YELLOW}$REALM_ID${NC}"
echo -e "Config:   ${YELLOW}/etc/sing-box/config.json${NC}"
echo -e "Data:     ${YELLOW}$INSTALL_DIR/data${NC}"
echo ""
echo -e "Commands:"
echo -e "  ${YELLOW}realm status${NC}  - Check service status"
echo -e "  ${YELLOW}realm logs${NC}    - View logs"
echo -e "  ${YELLOW}realm restart${NC} - Restart service"
echo ""
echo -e "${YELLOW}Note:${NC} realm_token / stun / obfs / sni / dns are pushed by manager"
echo -e "      via /api/node/users (§4.1 dynamic config); not set at install time."
