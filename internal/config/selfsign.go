package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// SelfSigner 负责 realm 出口的自签 iptv.local 证书（§7.1.3）。
// realm 出口【不走 tls-service】，自带/生成自签证书：
//   - 生成方式：Go 标准库 crypto/x509 + crypto/ecdsa(P-256)，不依赖 openssl
//     （家用 debian 不保证有；亦符合"诊断不装软件"约束）。
//   - 与 node-agent 的 CertManager 区别：这里是"本地自签自持"，不接收下发、不上报 cert-ack。
type SelfSigner struct {
	certDir  string
	certPath string
	keyPath  string
	sni      string
}

// NewSelfSigner 创建自签证书管理器。sni 为空时默认 iptv.local。
func NewSelfSigner(dataDir, sni string) *SelfSigner {
	if sni == "" {
		sni = "iptv.local"
	}
	certDir := filepath.Join(dataDir, "certs")
	return &SelfSigner{
		certDir:  certDir,
		certPath: filepath.Join(certDir, "iptv.local.crt"),
		keyPath:  filepath.Join(certDir, "iptv.local.key"),
		sni:      sni,
	}
}

// CertPath 返回证书路径（供 sing-box 配置引用）。
func (s *SelfSigner) CertPath() string { return s.certPath }

// KeyPath 返回私钥路径。
func (s *SelfSigner) KeyPath() string { return s.keyPath }

// EnsureCert "无则生成、有则复用"（类比 node-agent 的 LoadOrGenerateSecrets）。
// 首次启动若 iptv.local.crt 不存在则生成一次并持久化。
func (s *SelfSigner) EnsureCert() error {
	if s.exists() {
		log.Printf("[selfsign] reusing existing self-signed cert: %s", s.certPath)
		return nil
	}
	return s.generate()
}

func (s *SelfSigner) exists() bool {
	_, certErr := os.Stat(s.certPath)
	_, keyErr := os.Stat(s.keyPath)
	return certErr == nil && keyErr == nil
}

// generate 生成自签 P-256 证书，CN/SAN = sni，有效期 10 年（自签无需轮换）。
func (s *SelfSigner) generate() error {
	if err := os.MkdirAll(s.certDir, 0700); err != nil {
		return fmt.Errorf("mkdir certs: %w", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	notBefore := time.Now().Add(-1 * time.Hour) // 容忍出口时钟轻微偏移
	notAfter := notBefore.AddDate(10, 0, 0)     // 10 年

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: s.sni},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{s.sni},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// 写证书（0644）
	certOut, err := os.OpenFile(s.certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open cert file: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		certOut.Close()
		return fmt.Errorf("encode cert: %w", err)
	}
	certOut.Close()

	// 写私钥（0600）
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyOut, err := os.OpenFile(s.keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open key file: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		keyOut.Close()
		return fmt.Errorf("encode key: %w", err)
	}
	keyOut.Close()

	log.Printf("[selfsign] generated self-signed cert (CN=%s, valid 10y): %s", s.sni, s.certPath)
	return nil
}
