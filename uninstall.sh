#!/bin/bash
set -e

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

echo -e "${YELLOW}Uninstalling OTun Realm Agent...${NC}"

systemctl stop otun-realm-agent 2>/dev/null || true
systemctl disable otun-realm-agent 2>/dev/null || true
pkill -9 otun-realm-agent 2>/dev/null || true

rm -f /etc/systemd/system/otun-realm-agent.service
rm -f /usr/local/bin/realm
systemctl daemon-reload

# 保留 data 目录（含自签证书），如需彻底清除请手动:
#   rm -rf /opt/otun-realm-agent
echo -e "${YELLOW}Kept /opt/otun-realm-agent/data (self-signed certs). Remove manually if desired.${NC}"
echo -e "${GREEN}Uninstall complete.${NC}"
