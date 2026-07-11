#!/bin/bash
# updater.sh 的 7 场景 mock 测试台（U3）。自包含：mock fleet(HTTP) + mock systemctl
# （restart 结果取决于当前二进制内容 → 能演出"新版 crash 回滚后旧版起得来"）+ 沙箱路径。
# 本地与 CI 都跑：bash test/updater_test.sh；任一场景失败退非 0。
# 依赖：bash / python3 / curl / sha256sum（ubuntu-latest 与节点 Debian 都有）。
set -u

REPO_DIR=$(cd "$(dirname "$0")/.." && pwd)
UPD=$REPO_DIR/updater.sh
SB=$(mktemp -d)
trap 'kill $SRV 2>/dev/null; rm -rf "$SB"' EXIT
cd "$SB"
mkdir -p opt units

PORT=$(( 20000 + RANDOM % 10000 ))
FAILS=0
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1"; FAILS=$((FAILS+1)); }
check() { # check <desc> <cmd...>
    local desc="$1"; shift
    if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

# ── mock systemctl：restart 后 active 与否取决于 agent 二进制内容（crash 标记）──
cat > systemctl-mock << 'EOF'
#!/bin/bash
echo "$*" >> "$MOCK_LOG"
case "$1" in
  is-active) [ "$(cat "$MOCK_STATE")" = "active" ] ;;
  restart)
    if grep -q "$MOCK_CRASH_MARKER" "$MOCK_AGENT" 2>/dev/null; then echo inactive > "$MOCK_STATE"; else echo active > "$MOCK_STATE"; fi
    exit 0 ;;
  *) exit 0 ;;
esac
EOF
cat > journalctl-mock << 'EOF'
#!/bin/bash
echo "mock-journal: agent panicked at startup"
EOF
chmod +x systemctl-mock journalctl-mock

# ── mock fleet + /dl/：记录 plan 拉取/下载鉴权/状态上报 ──
cat > server.py << 'EOF'
import http.server, sys
PLAN, DLFILE, PORT, LOG = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith('/api/node/upgrade-plan'):
            open(LOG,'a').write('GET plan %s auth=%s\n' % (self.path, self.headers.get('Authorization','')))
            body = open(PLAN,'rb').read()
        elif self.path.startswith('/dl/'):
            open(LOG,'a').write('GET dl auth=%s\n' % self.headers.get('Authorization',''))
            body = open(DLFILE,'rb').read()
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        n = int(self.headers.get('Content-Length',0))
        open(LOG,'a').write('POST status %s\n' % self.rfile.read(n).decode())
        self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true}')
    def log_message(self,*a): pass
http.server.HTTPServer(('127.0.0.1',PORT),H).serve_forever()
EOF

cat > agent.unit << EOF
[Service]
Environment="NODE_API_KEY=test-node-key"
Environment="NODE_ID=test-node"
Environment="OTUN_API_URL=https://otun.example"
Environment="FLEET_API_URL=http://127.0.0.1:$PORT"
EOF

echo new-agent-binary > payload-new
NEWSHA=$(sha256sum payload-new | cut -d' ' -f1)
printf '{"upgrade":{"task_id":7,"version":"u3-target-sha","url":"http://127.0.0.1:%s/dl/agent-linux-amd64","sha256":"%s","dl_token":"u3-dl-token"}}' "$PORT" "$NEWSHA" > plan-good.json
printf '{"upgrade":null}' > plan-null.json

python3 server.py plan.json payload-serve "$PORT" server.log & SRV=$!
sleep 1

run_updater() { # run_updater <plan-file> <initial-active-state> <crash-marker>
    cp "$1" plan.json; echo "$2" > state; : > mock.log; : > server.log
    MOCK_LOG=$SB/mock.log MOCK_STATE=$SB/state MOCK_AGENT=$SB/opt/agent MOCK_CRASH_MARKER="${3:-NEVER}" \
    UPDATER_INSTALL_DIR=$SB/opt UPDATER_AGENT_UNIT=$SB/agent.unit UPDATER_UNIT_DIR=$SB/units \
    UPDATER_SYSTEMCTL=$SB/systemctl-mock UPDATER_JOURNALCTL=$SB/journalctl-mock UPDATER_SLEEP_SCALE=0 \
    bash "$UPD" > updater.out 2>&1
}

echo "═══ 1. 无任务 → 静默退出 + 清残留备份 ═══"
echo old-agent-binary > opt/agent
touch opt/agent.prev
run_updater plan-null.json active NEVER
check "agent.prev 已清理" test ! -f opt/agent.prev
check "未上报任何状态" test ! -s server.log.post 2>/dev/null || ! grep -q "POST status" server.log

echo "═══ 2. happy path：下载→校验→原子替换→重启→验证→succeeded ═══"
cp payload-new payload-serve
run_updater plan-good.json active NEVER
check "二进制已换新" grep -q new-agent-binary opt/agent
check "成功后备份已清" test ! -f opt/agent.prev
check "上报序列 downloading" grep -q '"state":"downloading"' server.log
check "上报序列 applied" grep -q '"state":"applied"' server.log
check "上报序列 verifying" grep -q '"state":"verifying"' server.log
check "上报序列 succeeded" grep -q '"state":"succeeded"' server.log
check "下载带 fleet 下发的 dl_token" grep -q 'GET dl auth=Bearer u3-dl-token' server.log
check "plan 请求带节点 api_key" grep -q 'auth=Bearer test-node-key' server.log

echo "═══ 3. 已是目标版本（幂等）→ 不下载,报 succeeded ═══"
run_updater plan-good.json active NEVER
check "未重复下载" bash -c '! grep -q "GET dl" server.log'
check "幂等报 succeeded" grep -q '"state":"succeeded"' server.log

echo "═══ 4. sha 不匹配 → 硬拒绝,旧版原封不动,零残留 ═══"
echo old-agent-binary > opt/agent
echo corrupted-content > payload-serve
run_updater plan-good.json active NEVER
check "agent 未被替换" grep -q old-agent-binary opt/agent
check "无 agent.new 残留" test ! -f opt/agent.new
check "报 failed + sha mismatch" bash -c 'grep -q "\"state\":\"failed\"" server.log && grep -q "sha256 mismatch" server.log'

echo "═══ 5. 新版 crash-loop → 自动回滚 + rolled_back + journal 摘录 ═══"
echo old-agent-binary > opt/agent
cp payload-new payload-serve
run_updater plan-good.json active "new-agent-binary"
check "已回滚到旧版" grep -q old-agent-binary opt/agent
check "报 rolled_back" grep -q '"state":"rolled_back"' server.log
check "last_error 带 journal 摘录" grep -q 'mock-journal' server.log

echo "═══ 6. 二进制已是目标但 agent 挂着 → timer 救活 ═══"
cp payload-new opt/agent
run_updater plan-good.json inactive NEVER
check "执行了 restart（救活）" grep -q '^restart' mock.log
check "救活后报 succeeded" grep -q '"state":"succeeded"' server.log

echo "═══ 7. --install → 写两 unit + enable --now ═══"
: > mock.log
MOCK_LOG=$SB/mock.log MOCK_STATE=$SB/state MOCK_AGENT=$SB/opt/agent MOCK_CRASH_MARKER=NEVER \
UPDATER_UNIT_DIR=$SB/units UPDATER_SYSTEMCTL=$SB/systemctl-mock bash "$UPD" --install > /dev/null 2>&1
check "service unit 已写" test -f units/otun-agent-updater.service
check "timer unit 已写" test -f units/otun-agent-updater.timer
check "timer 每 5 分钟" grep -q 'OnUnitActiveSec=5min' units/otun-agent-updater.timer
check "service oneshot" grep -q 'Type=oneshot' units/otun-agent-updater.service
check "enable --now 已调" grep -q 'enable --now otun-agent-updater.timer' mock.log

echo
if [ "$FAILS" -gt 0 ]; then echo "❌ $FAILS check(s) FAILED"; exit 1; fi
echo "✅ all scenarios passed"
