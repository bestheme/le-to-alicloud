package aliyun

import (
	"errors"
	"fmt"
	"strings"

	credential "github.com/aliyun/credentials-go/credentials"
	corev1 "k8s.io/api/core/v1"
)

// Secret 契约（见 spec §4.3）。
const (
	SecretTypeAliyunCredentials corev1.SecretType = "certs.bestheme.ac.cn/aliyun-credentials"

	KeyAccessKeyID       = "accessKeyId"
	KeyAccessKeySecret   = "accessKeySecret"
	KeySecurityToken     = "securityToken"
	KeyRoleARN           = "roleArn"
	KeyOIDCProviderARN   = "oidcProviderArn"
	KeyOIDCTokenFilePath = "oidcTokenFilePath" //nolint:gosec // G101 误报：Secret 的 data 键名，不是键值
)

// Credentials 是从 Secret 读出的凭证材料。不要把它写进日志。
type Credentials struct {
	AccessKeyID       string
	AccessKeySecret   string
	SecurityToken     string
	RoleARN           string
	OIDCProviderARN   string
	OIDCTokenFilePath string
}

// CredentialsFromSecret 解析并校验凭证 Secret。
func CredentialsFromSecret(s *corev1.Secret) (*Credentials, error) {
	get := func(k string) string { return strings.TrimSpace(string(s.Data[k])) }
	c := &Credentials{
		AccessKeyID:       get(KeyAccessKeyID),
		AccessKeySecret:   get(KeyAccessKeySecret),
		SecurityToken:     get(KeySecurityToken),
		RoleARN:           get(KeyRoleARN),
		OIDCProviderARN:   get(KeyOIDCProviderARN),
		OIDCTokenFilePath: get(KeyOIDCTokenFilePath),
	}
	switch {
	case c.isOIDC():
		return c, nil
	case c.AccessKeyID != "" && c.AccessKeySecret != "":
		return c, nil
	case c.AccessKeyID != "" || c.AccessKeySecret != "":
		return nil, fmt.Errorf("凭证 Secret 必须同时包含 %s 与 %s", KeyAccessKeyID, KeyAccessKeySecret)
	default:
		return nil, errors.New("凭证 Secret 为空：需要 accessKeyId/accessKeySecret 或 roleArn/oidcProviderArn/oidcTokenFilePath")
	}
}

func (c *Credentials) isOIDC() bool {
	return c.RoleARN != "" && c.OIDCProviderARN != "" && c.OIDCTokenFilePath != ""
}

// LimiterKey 是限流器分桶键：阿里云按账号限流，用 AK 或角色 ARN 近似账号。
func (c *Credentials) LimiterKey() string {
	if c.isOIDC() {
		return c.RoleARN
	}
	return c.AccessKeyID
}

// Build 构造 credentials-go 的 Credential。RRSA 第一版只预留，不在文档中承诺。
func (c *Credentials) Build() (credential.Credential, error) {
	cfg := new(credential.Config)
	switch {
	case c.isOIDC():
		cfg.SetType("oidc_role_arn").
			SetRoleArn(c.RoleARN).
			SetOIDCProviderArn(c.OIDCProviderARN).
			SetOIDCTokenFilePath(c.OIDCTokenFilePath)
	case c.SecurityToken != "":
		cfg.SetType("sts").
			SetAccessKeyId(c.AccessKeyID).
			SetAccessKeySecret(c.AccessKeySecret).
			SetSecurityToken(c.SecurityToken)
	default:
		cfg.SetType("access_key").
			SetAccessKeyId(c.AccessKeyID).
			SetAccessKeySecret(c.AccessKeySecret)
	}
	return credential.NewCredential(cfg)
}
