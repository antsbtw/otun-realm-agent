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

# ============ sing-box（★带 realm 打洞 + v2ray_api 计费，自编译预发布）============
# 照搬 otun-node-agent 的方案：realm-agent 自己用 build-singbox 工作流编译
# “1.14.0-alpha.25 源码（realm 默认）+ with_v2ray_api（per-user 计费）+ with_quic（hy2）”，
# 发布到 realm-agent 自己的 release（tag singbox-v<版本>，资产为裸二进制）。
# 不复用 node-agent 的二进制（那个版本旧、无 realm）；也不用官方预编译包（那个无 v2ray_api）。
echo -e "${GREEN}Downloading sing-box (realm + v2ray_api)...${NC}"
SINGBOX_VERSION="1.14.0-alpha.25"
SINGBOX_URL="https://github.com/antsbtw/otun-realm-agent/releases/download/singbox-v${SINGBOX_VERSION}/sing-box-linux-${SINGBOX_ARCH}"
if ! curl -fsSL "$SINGBOX_URL" -o /usr/local/bin/sing-box; then
    echo -e "${YELLOW}Pre-built download failed, building from source...${NC}"
    apt-get install -y -qq git
    GO_VERSION="1.25.0"
    rm -rf /usr/local/go
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${AGENT_ARCH}.tar.gz" -o /tmp/go.tar.gz
    tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    cd /tmp && rm -rf sing-box-src
    git clone --depth 1 --branch "v${SINGBOX_VERSION}" https://github.com/SagerNet/sing-box.git sing-box-src
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
if curl -fsSL "$AGENT_URL" -o $INSTALL_DIR/agent; then
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
