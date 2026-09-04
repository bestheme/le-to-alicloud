// Package pki 负责证书材料的解析、校验与 PEM 规范化。
// 这是 Secret → 阿里云之间唯一的数据变换点，所有格式要求在这里一次满足。
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

var (
	ErrNoCertificate  = errors.New("pki: 未找到 CERTIFICATE PEM 块")
	ErrNoPrivateKey   = errors.New("pki: 未找到私钥 PEM 块")
	ErrEncryptedKey   = errors.New("pki: 私钥已加密，阿里云不接受")
	ErrUnsupportedKey = errors.New("pki: 不支持的私钥类型（仅 RSA / ECDSA）")
	ErrKeyMismatch    = errors.New("pki: 私钥与 leaf 公钥不匹配")
	ErrBrokenChain    = errors.New("pki: 证书链不连续")
)

// Bundle 是解析并校验过的证书材料。
type Bundle struct {
	Leaf          *x509.Certificate
	Intermediates []*x509.Certificate
	Signer        crypto.Signer
	// Fingerprint = hex(sha256(Leaf.Raw))，全系统幂等基准。
	Fingerprint string
}

// ParseBundle 解析 tls.crt / tls.key，校验链连续性与公私钥匹配。
// 不校验有效期、不校验信任锚（那是 cert-manager 的职责）。
func ParseBundle(certPEM, keyPEM []byte) (*Bundle, error) {
	certs, err := parseCertificates(certPEM)
	if err != nil {
		return nil, err
	}
	signer, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Leaf: certs[0], Intermediates: certs[1:], Signer: signer}

	// 链连续性：certs[i] 必须由 certs[i+1] 签发
	for i := 0; i+1 < len(certs); i++ {
		if err := certs[i].CheckSignatureFrom(certs[i+1]); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 张不是由第 %d 张签发: %v", ErrBrokenChain, i, i+1, err)
		}
	}

	// 公私钥匹配
	pub, ok := b.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil, ErrKeyMismatch
	}

	sum := sha256.Sum256(b.Leaf.Raw)
	b.Fingerprint = hex.EncodeToString(sum[:])
	return b, nil
}

func parseCertificates(certPEM []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := certPEM
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue // 忽略 bundle 里夹带的其它块
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pki: 解析证书失败: %w", err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrNoCertificate
	}
	return out, nil
}

func parsePrivateKey(keyPEM []byte) (crypto.Signer, error) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, ErrNoPrivateKey
	}
	if procType, ok := blk.Headers["Proc-Type"]; ok && bytes.Contains([]byte(procType), []byte("ENCRYPTED")) {
		return nil, ErrEncryptedKey
	}
	var key any
	var err error
	switch blk.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	case "ENCRYPTED PRIVATE KEY":
		return nil, ErrEncryptedKey
	default:
		return nil, fmt.Errorf("%w: PEM 类型 %q", ErrUnsupportedKey, blk.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("pki: 解析私钥失败: %w", err)
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return k, nil
	default:
		return nil, ErrUnsupportedKey
	}
}

// IsSelfSigned 判断 leaf 是否自签（Issuer == Subject 且签名可由自身公钥验证）。
func (b *Bundle) IsSelfSigned() bool {
	if !bytes.Equal(b.Leaf.RawIssuer, b.Leaf.RawSubject) {
		return false
	}
	// 不能用 CheckSignatureFrom：它会对「父证书」施加 CA 约束（BasicConstraints/KeyUsage
	// CertSign），而自签 leaf（如 cert-manager 的临时证书）本来就不是 CA。这里只需要验证
	// 签名确实来自自身公钥。
	return b.Leaf.CheckSignature(b.Leaf.SignatureAlgorithm, b.Leaf.RawTBSCertificate, b.Leaf.Signature) == nil
}

// DNSNames 返回 leaf 的 SANs；若 CN 不在 SANs 中且非空，则追加（兼容老证书）。
func (b *Bundle) DNSNames() []string {
	names := append([]string(nil), b.Leaf.DNSNames...)
	cn := b.Leaf.Subject.CommonName
	if cn == "" {
		return names
	}
	for _, n := range names {
		if n == cn {
			return names
		}
	}
	return append(names, cn)
}

// CertPEM 由 DER 重新编码：leaf 在前、中间证书紧随、无空行、64 字符/行。
func (b *Bundle) CertPEM() []byte {
	var buf bytes.Buffer
	buf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Leaf.Raw}))
	for _, c := range b.Intermediates {
		buf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return buf.Bytes()
}

// KeyPEM 输出阿里云期望的私钥编码：RSA → PKCS#1，ECDSA → SEC1。
func (b *Bundle) KeyPEM() ([]byte, error) {
	switch k := b.Signer.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("pki: 编码 EC 私钥失败: %w", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
	default:
		return nil, ErrUnsupportedKey
	}
}
