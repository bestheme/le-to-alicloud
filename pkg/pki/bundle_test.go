package pki_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func TestParseBundle_LeafPlusIntermediate(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "api.example.com", "www.example.com")

	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("期望成功，得到 %v", err)
	}
	if b.Leaf.Subject.CommonName != "api.example.com" {
		t.Errorf("leaf CN = %q", b.Leaf.Subject.CommonName)
	}
	if len(b.Intermediates) != 1 {
		t.Errorf("intermediates = %d, want 1", len(b.Intermediates))
	}
	sum := sha256.Sum256(b.Leaf.Raw)
	if b.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Errorf("指纹不等于 sha256(leaf DER)")
	}
	if b.IsSelfSigned() {
		t.Errorf("CA 签发的 leaf 不应被判为自签")
	}
	if got := b.DNSNames(); len(got) != 2 {
		t.Errorf("DNSNames = %v", got)
	}
}

func TestParseBundle_SelfSigned(t *testing.T) {
	certPEM, keyPEM := testutil.SelfSigned(t, "tmp.example.com")
	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("自签证书本身应可解析: %v", err)
	}
	if !b.IsSelfSigned() {
		t.Errorf("应判为自签")
	}
}

func TestParseBundle_KeyMismatch(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	_, otherKey := testutil.IssueLeaf(t, ca, "b.example.com")
	_, err := pki.ParseBundle(certPEM, otherKey)
	if !errors.Is(err, pki.ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
}

func TestParseBundle_BrokenChain(t *testing.T) {
	ca1 := testutil.NewCA(t)
	ca2 := testutil.NewCA(t)
	leafPEM, keyPEM := testutil.IssueLeaf(t, ca1, "a.example.com")
	// 把链里的 CA 换成不相关的 ca2
	blk, rest := pem.Decode(leafPEM)
	_ = rest
	bad := append(pem.EncodeToMemory(blk), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca2.Cert.Raw})...)
	_, err := pki.ParseBundle(bad, keyPEM)
	if !errors.Is(err, pki.ErrBrokenChain) {
		t.Fatalf("want ErrBrokenChain, got %v", err)
	}
}

func TestParseBundle_EncryptedKeyRejected(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	_, err := pki.ParseBundle(certPEM, testutil.Encrypted(t))
	if !errors.Is(err, pki.ErrEncryptedKey) {
		t.Fatalf("want ErrEncryptedKey, got %v", err)
	}
}

func TestParseBundle_EmptyInputs(t *testing.T) {
	if _, err := pki.ParseBundle(nil, []byte("x")); !errors.Is(err, pki.ErrNoCertificate) {
		t.Errorf("want ErrNoCertificate, got %v", err)
	}
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	if _, err := pki.ParseBundle(certPEM, nil); !errors.Is(err, pki.ErrNoPrivateKey) {
		t.Errorf("want ErrNoPrivateKey, got %v", err)
	}
}

func TestBundle_CertPEM_Normalized(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "a.example.com")
	// 人为加入空行与注释，规范化后必须消失
	dirty := append([]byte("# bundle comment\n\n"), bytes.ReplaceAll(certPEM, []byte("-----END CERTIFICATE-----\n"), []byte("-----END CERTIFICATE-----\n\n"))...)
	b, err := pki.ParseBundle(dirty, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	out := b.CertPEM()
	if bytes.Contains(out, []byte("\n\n")) {
		t.Errorf("规范化输出不应含空行")
	}
	if bytes.Contains(out, []byte("#")) {
		t.Errorf("规范化输出不应含注释")
	}
	if !bytes.HasPrefix(out, []byte("-----BEGIN CERTIFICATE-----\n")) {
		t.Errorf("应以 BEGIN CERTIFICATE 开头")
	}
	if c := bytes.Count(out, []byte("-----BEGIN CERTIFICATE-----")); c != 2 {
		t.Errorf("应含 2 张证书，得到 %d", c)
	}
	// leaf 必须在前
	first, _ := pem.Decode(out)
	if !bytes.Equal(first.Bytes, b.Leaf.Raw) {
		t.Errorf("第一张必须是 leaf")
	}
}

func TestBundle_KeyPEM_PKCS8InputBecomesSEC1(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "a.example.com")
	pkcs8 := testutil.ToPKCS8(t, keyPEM)
	b, err := pki.ParseBundle(certPEM, pkcs8)
	if err != nil {
		t.Fatalf("PKCS8 输入应可解析: %v", err)
	}
	out, err := b.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(out)
	if blk.Type != "EC PRIVATE KEY" {
		t.Errorf("EC 密钥应输出 SEC1 (EC PRIVATE KEY)，得到 %q", blk.Type)
	}
}

// RSA 是 cert-manager 的默认密钥类型，整条 RSA 路径必须有断言经过：
// PKCS#1 与 PKCS#8 两种输入的解析、公私钥匹配、以及 KeyPEM() 输出 PKCS#1。
func TestBundle_KeyPEM_RSAOutputsPKCS1(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, "rsa.example.com")

	pkcs1Out := func(t *testing.T, key []byte) []byte {
		t.Helper()
		b, err := pki.ParseBundle(certPEM, key)
		if err != nil {
			t.Fatalf("RSA leaf 应可解析: %v", err)
		}
		if _, ok := b.Signer.(*rsa.PrivateKey); !ok {
			t.Fatalf("Signer 应为 *rsa.PrivateKey，得到 %T", b.Signer)
		}
		out, err := b.KeyPEM()
		if err != nil {
			t.Fatalf("KeyPEM 失败: %v", err)
		}
		blk, _ := pem.Decode(out)
		if blk == nil {
			t.Fatal("KeyPEM 输出不是合法 PEM")
		}
		if blk.Type != "RSA PRIVATE KEY" {
			t.Errorf("RSA 密钥应输出 PKCS#1 (RSA PRIVATE KEY)，得到 %q", blk.Type)
		}
		if _, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err != nil {
			t.Errorf("输出应能按 PKCS#1 解析: %v", err)
		}
		return out
	}

	fromPKCS1 := pkcs1Out(t, keyPEM)
	fromPKCS8 := pkcs1Out(t, testutil.ToPKCS8(t, keyPEM))
	if !bytes.Equal(fromPKCS1, fromPKCS8) {
		t.Errorf("PKCS#1 与 PKCS#8 输入应规范化成同一份输出")
	}
}

// Bundle 携带私钥，绝不能在日志 / event / status 里泄漏。
// 真正的泄漏面是 JSON（controller-runtime 的 zap logger 用 JSON 反射编码器序列化
// `log.Info("bundle", "b", bundle)` 的值），fmt 的各个动词只是顺带加固。
func TestBundle_FormatRedactsPrivateKey(t *testing.T) {
	ca := testutil.NewCA(t)
	ecCert, ecKey := testutil.IssueLeaf(t, ca, "ec.example.com")
	rsaCert, rsaKey := testutil.IssueLeafRSA(t, ca, "rsa.example.com")

	for _, tc := range []struct {
		name            string
		certPEM, keyPEM []byte
	}{
		{"ecdsa", ecCert, ecKey},
		{"rsa", rsaCert, rsaKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := pki.ParseBundle(tc.certPEM, tc.keyPEM)
			if err != nil {
				t.Fatal(err)
			}

			var d *big.Int
			switch k := b.Signer.(type) {
			case *rsa.PrivateKey:
				d = k.D
			case *ecdsa.PrivateKey:
				d = k.D
			default:
				t.Fatalf("意外的 Signer 类型 %T", b.Signer)
			}
			// 十六进制与十进制两种渲染都要检查：fmt 用十进制打印 big.Int，
			// encoding/json 也把它序列化成十进制数字。
			secrets := []string{prefix(d.Text(16), 16), prefix(d.String(), 16)}

			check := func(label, got string) {
				t.Helper()
				for _, s := range secrets {
					if strings.Contains(got, s) {
						t.Errorf("%s 泄漏了私钥标量: %s", label, got)
					}
				}
				if strings.Contains(got, "PRIVATE") {
					t.Errorf("%s 输出含 PRIVATE: %s", label, got)
				}
				if !strings.Contains(got, "redacted") {
					t.Errorf("%s 应把私钥标记为 redacted: %s", label, got)
				}
				if !strings.Contains(got, b.Fingerprint[:8]) {
					t.Errorf("%s 应含指纹前 8 位: %s", label, got)
				}
			}

			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				check(verb+" 指针", fmt.Sprintf(verb, b))
				check(verb+" 值", fmt.Sprintf(verb, *b))
			}

			j, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("json.Marshal 失败: %v", err)
			}
			check("json.Marshal", string(j))
		})
	}
}

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
