package relaymgr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeEnv 把包级路径重定向到临时目录，并装配可编程的 systemctl/download 假件。
type fakeEnv struct {
	m        *Manager
	calls    []string          // 记录 systemctl 调用序列
	isActive string            // is-active 的应答
	binBytes []byte            // download 假件返回的二进制内容
	dlErr    error
	dlCount  int
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	dir := t.TempDir()
	oldBin, oldUnit := binaryPath, unitPath
	binaryPath = filepath.Join(dir, "otun-relay")
	unitPath = filepath.Join(dir, "otun-relay.service")
	t.Cleanup(func() { binaryPath, unitPath = oldBin, oldUnit })

	f := &fakeEnv{isActive: "inactive"}
	m := New()
	m.runCmd = func(name string, args ...string) (string, error) {
		call := name
		for _, a := range args {
			call += " " + a
		}
		f.calls = append(f.calls, call)
		if len(args) >= 1 && args[0] == "is-active" {
			return f.isActive, nil
		}
		return "", nil
	}
	m.download = func(d *Download, arch string) ([]byte, error) {
		f.dlCount++
		if f.dlErr != nil {
			return nil, f.dlErr
		}
		return f.binBytes, nil
	}
	f.m = m
	return f
}

func (f *fakeEnv) called(sub string) bool {
	for _, c := range f.calls {
		if c == sub {
			return true
		}
	}
	return false
}

func shaOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func directiveWithDownload(bin []byte) *Directive {
	return &Directive{
		RelayID: "relay-test", Port: 51830, DesiredState: "enabled",
		Download: &Download{
			URLBase: "https://dl.example", Token: "t",
			Artifacts: map[string]Artifact{
				"linux-amd64": {File: "otun-relay-linux-amd64", SHA256: shaOf(bin)},
				"linux-arm64": {File: "otun-relay-linux-arm64", SHA256: shaOf(bin)},
			},
		},
	}
}

// 全新节点：拉二进制（sha 校验）→ 写 unit → restart+enable。
func TestFreshInstall(t *testing.T) {
	f := newFakeEnv(t)
	bin := []byte("RELAY_BINARY_V1")
	f.binBytes = bin

	if err := f.m.reconcile(directiveWithDownload(bin)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil || string(got) != string(bin) {
		t.Fatalf("binary not installed: %v %q", err, got)
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if string(unit) != UnitContent(51830) {
		t.Fatalf("unit content mismatch")
	}
	if !f.called("systemctl daemon-reload") || !f.called("systemctl restart otun-relay") {
		t.Fatalf("expected daemon-reload+restart, got %v", f.calls)
	}
	if !f.called("systemctl enable otun-relay") {
		t.Fatalf("expected enable, got %v", f.calls)
	}
}

// ★幂等收编（jp-02 场景）：二进制在位但无校验源（fleet 未带 download）+ 手工 unit
// 内容不同 → 只改写 unit 并 restart，不动二进制；第二轮完全一致 → 零动作。
func TestAdoptExistingManualDeploy(t *testing.T) {
	f := newFakeEnv(t)
	// 手工部署现场：二进制已在、老 unit 内容不同、服务在跑。
	os.WriteFile(binaryPath, []byte("MANUAL_BUILD"), 0o755)
	os.WriteFile(unitPath, []byte("[Unit]\nDescription=manual otun-relay\n"), 0o644)
	f.isActive = "active"

	d := &Directive{RelayID: "relay-jp", Port: 51830, DesiredState: "enabled"} // 无 download
	if err := f.m.reconcile(d); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if f.dlCount != 0 {
		t.Fatalf("must not download when binary exists without checksum source")
	}
	unit, _ := os.ReadFile(unitPath)
	if string(unit) != UnitContent(51830) {
		t.Fatalf("unit not canonicalized")
	}
	if !f.called("systemctl restart otun-relay") {
		t.Fatalf("unit changed → expected restart, got %v", f.calls)
	}

	// 第二轮：内容已一致且 active → 绝不 restart（不扰动在途会话）。
	f.calls = nil
	if err := f.m.reconcile(d); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for _, c := range f.calls {
		if c == "systemctl restart otun-relay" || c == "systemctl daemon-reload" {
			t.Fatalf("idempotent pass must not restart: %v", f.calls)
		}
	}
}

// 二进制 sha 不符 → 拉新替换 → restart。
func TestUpgradeOnShaMismatch(t *testing.T) {
	f := newFakeEnv(t)
	os.WriteFile(binaryPath, []byte("OLD"), 0o755)
	newBin := []byte("NEW_RELAY_BINARY")
	f.binBytes = newBin
	f.isActive = "active"
	d := directiveWithDownload(newBin)
	os.WriteFile(unitPath, []byte(UnitContent(51830)), 0o644) // unit 已规范

	if err := f.m.reconcile(d); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, _ := os.ReadFile(binaryPath)
	if string(got) != string(newBin) {
		t.Fatalf("binary not upgraded")
	}
	if !f.called("systemctl restart otun-relay") {
		t.Fatalf("expected restart after binary swap")
	}
}

// 下载失败但本地有现役二进制 → 降级保持现状，不报错、不 restart。
func TestDownloadFailureKeepsExisting(t *testing.T) {
	f := newFakeEnv(t)
	os.WriteFile(binaryPath, []byte("SERVING"), 0o755)
	os.WriteFile(unitPath, []byte(UnitContent(51830)), 0o644)
	f.isActive = "active"
	f.dlErr = fmt.Errorf("dl down")
	d := directiveWithDownload([]byte("NEWER")) // sha 与现役不符 → 触发下载 → 失败

	if err := f.m.reconcile(d); err != nil {
		t.Fatalf("should degrade gracefully, got %v", err)
	}
	got, _ := os.ReadFile(binaryPath)
	if string(got) != "SERVING" {
		t.Fatalf("existing binary must be kept on download failure")
	}
	if f.called("systemctl restart otun-relay") {
		t.Fatalf("must not restart when nothing changed")
	}
}

// 假二进制（sha 校验不过）绝不落盘。
func TestShaMismatchNeverInstalls(t *testing.T) {
	f := newFakeEnv(t)
	f.binBytes = []byte("TAMPERED")
	d := directiveWithDownload([]byte("EXPECTED")) // want sha ≠ 实收

	if err := f.m.reconcile(d); err == nil {
		t.Fatalf("expected sha mismatch error")
	}
	if _, err := os.Stat(binaryPath); !os.IsNotExist(err) {
		t.Fatalf("tampered binary must not be installed")
	}
}

// disabled：在跑 → stop+disable；没跑 → 零动作。
func TestDisable(t *testing.T) {
	f := newFakeEnv(t)
	f.isActive = "active"
	if err := f.m.reconcile(&Directive{DesiredState: "disabled"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !f.called("systemctl stop otun-relay") {
		t.Fatalf("expected stop, got %v", f.calls)
	}

	f.calls, f.isActive = nil, "inactive"
	if err := f.m.reconcile(&Directive{DesiredState: "disabled"}); err != nil {
		t.Fatalf("disable idempotent: %v", err)
	}
	if f.called("systemctl stop otun-relay") {
		t.Fatalf("already stopped → must be no-op")
	}
}

// enabled 且服务没起（unit/二进制均已就位）→ 只 start，不 restart。
func TestStartWhenInactive(t *testing.T) {
	f := newFakeEnv(t)
	os.WriteFile(binaryPath, []byte("BIN"), 0o755)
	os.WriteFile(unitPath, []byte(UnitContent(51830)), 0o644)
	f.isActive = "inactive"
	d := &Directive{RelayID: "r", Port: 51830, DesiredState: "enabled"}
	if err := f.m.reconcile(d); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !f.called("systemctl start otun-relay") || f.called("systemctl restart otun-relay") {
		t.Fatalf("expected start (not restart), got %v", f.calls)
	}
}
