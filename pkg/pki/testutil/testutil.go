// Package testutil 生成测试用证书材料。只在 _test 中使用。
package testutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// TB 是 testutil 需要的最小测试接口。
// 不用 testing.TB：Go 1.25 起 testing.TB 新增了 ArtifactDir，而 Ginkgo 的 GinkgoT()
// 尚未实现它，envtest 用例就没法把 GinkgoT() 传进来。这三个方法 *testing.T 与
// GinkgoT() 都提供。
type TB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// CA 是测试用中间 CA（自签根，直接签 leaf，模拟 LE 的 leaf+intermediate 形状时把它当 intermediate）。
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

func mustKey(t TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return k
}

func serial(t TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("生成序列号失败: %v", err)
	}
	return n
}

// NewCA 生成一个自签 CA。
func NewCA(t TB) *CA {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "Test Intermediate CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 CA 失败: %v", err)
	}
	return &CA{Cert: cert, Key: key}
}

// leafTemplate 是 leaf 证书模板，供 EC / RSA 两种签发路径共用。
func leafTemplate(t TB, dnsNames []string) *x509.Certificate {
	t.Helper()
	return &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// issueLeaf 由 ca 用给定公钥签发 leaf，返回「leaf + ca」的证书 PEM。
func issueLeaf(t TB, ca *CA, pub crypto.PublicKey, dnsNames []string) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, leafTemplate(t, dnsNames), ca.Cert, pub, ca.Key)
	if err != nil {
		t.Fatalf("签发 leaf 失败: %v", err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})...)
}

// IssueLeaf 由 ca 签发 leaf，返回「leaf + ca」的证书 PEM 与 leaf 的 EC 私钥 PEM（SEC1）。
func IssueLeaf(t TB, ca *CA, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key := mustKey(t)
	certPEM = issueLeaf(t, ca, key.Public(), dnsNames)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// IssueLeafRSA 与 IssueLeaf 相同，但 leaf 用 2048 位 RSA 密钥（cert-manager 的默认密钥类型），
// 私钥以 PKCS#1（"RSA PRIVATE KEY"）输出。
func IssueLeafRSA(t TB, ca *CA, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	certPEM = issueLeaf(t, ca, key.Public(), dnsNames)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// SelfSigned 生成自签 leaf（模拟 cert-manager 临时证书或 SelfSigned issuer）。
func SelfSigned(t TB, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("创建自签证书失败: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// ToPKCS8 把 SEC1 EC 私钥 PEM（"EC PRIVATE KEY"）或 PKCS#1 RSA 私钥 PEM
// （"RSA PRIVATE KEY"）转成 PKCS#8 PEM。
func ToPKCS8(t TB, keyPEM []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		t.Fatal("解析私钥失败: 未找到 PEM 块")
	}
	var (
		k   crypto.PrivateKey
		err error
	)
	switch blk.Type {
	case "EC PRIVATE KEY":
		k, err = x509.ParseECPrivateKey(blk.Bytes)
	case "RSA PRIVATE KEY":
		k, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	default:
		t.Fatalf("不支持的私钥 PEM 类型: %q", blk.Type)
	}
	if err != nil {
		t.Fatalf("解析私钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("PKCS8 编码失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// Encrypted 返回一个带 Proc-Type: 4,ENCRYPTED 头的 PEM 块（内容随意，用于测试拒绝路径）。
func Encrypted(t TB) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"},
		Bytes:   []byte("not-a-real-key"),
	})
}
