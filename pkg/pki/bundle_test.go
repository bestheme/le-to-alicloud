package pki_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
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
