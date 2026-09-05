package aliyun_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func secretWith(data map[string]string) *corev1.Secret {
	s := &corev1.Secret{Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestCredentialsFromSecret_AccessKey(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyAccessKeyID: "LTAI123", aliyun.KeyAccessKeySecret: "sec",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "LTAI123" || c.AccessKeySecret != "sec" {
		t.Errorf("解析错误: %+v", c)
	}
	if c.LimiterKey() != "LTAI123" {
		t.Errorf("LimiterKey 应为 accessKeyId")
	}
	cred, err := c.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cred == nil {
		t.Fatal("credential 为 nil")
	}
}

func TestCredentialsFromSecret_TrimsWhitespace(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyAccessKeyID: " LTAI123\n", aliyun.KeyAccessKeySecret: "sec\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "LTAI123" || c.AccessKeySecret != "sec" {
		t.Errorf("应去除首尾空白: %+v", c)
	}
}

func TestCredentialsFromSecret_MissingSecretKey(t *testing.T) {
	_, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{aliyun.KeyAccessKeyID: "LTAI123"}))
	if err == nil {
		t.Fatal("缺 accessKeySecret 应报错")
	}
}

func TestCredentialsFromSecret_OIDC(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyRoleARN:           "acs:ram::123:role/x",
		aliyun.KeyOIDCProviderARN:   "acs:ram::123:oidc-provider/y",
		aliyun.KeyOIDCTokenFilePath: "/var/run/token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.LimiterKey() != "acs:ram::123:role/x" {
		t.Errorf("OIDC 模式 LimiterKey 应为 RoleARN")
	}
}

func TestCredentialsFromSecret_Empty(t *testing.T) {
	if _, err := aliyun.CredentialsFromSecret(secretWith(nil)); err == nil {
		t.Fatal("空 Secret 应报错")
	}
}
