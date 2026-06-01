package config

import (
	"crypto/tls"
	"os"
	"testing"
)

func TestSelfSigner_GenerateAndReuse(t *testing.T) {
	dir, err := os.MkdirTemp("", "realm-selfsign")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewSelfSigner(dir, "iptv.local")
	if err := s.EnsureCert(); err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}

	// 证书可被 tls 加载（CN/SAN 正确、密钥匹配）。
	if _, err := tls.LoadX509KeyPair(s.CertPath(), s.KeyPath()); err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	// 私钥权限 0600。
	info, err := os.Stat(s.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("key perm = %o, want 0600", info.Mode().Perm())
	}

	// 复用：第二次 EnsureCert 不报错且不重写（证书内容不变）。
	before, _ := os.ReadFile(s.CertPath())
	if err := s.EnsureCert(); err != nil {
		t.Fatalf("second EnsureCert: %v", err)
	}
	after, _ := os.ReadFile(s.CertPath())
	if string(before) != string(after) {
		t.Errorf("EnsureCert 应复用现有证书，不应重新生成")
	}
}
