package aliyun_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

//nolint:gosec // G101 误报：PEM 头尾之间是字面量占位串，不是可解析的密钥，用来断言它不会被打印出来
const fakeKeyPEM = "-----BEGIN RSA PRIVATE KEY-----\nSUPERSECRETKEYMATERIAL\n-----END RSA PRIVATE KEY-----\n"

// 私钥泄漏的真实通道是结构化日志：zap 对未知类型走反射 JSON 编码，导出字段会被原样写出。
// 这三个用例分别堵住 %v / %#v / json.Marshal 三条路，值与指针两种形态都覆盖。
func TestCertConfig_StringRedactsKey(t *testing.T) {
	// certName 必须取一个区分度足够的值：用单字符 "n" 时，格式串里的 "aliyun." 就已经
	// 含有它，断言即使在 String() 完全丢掉 CertName 的情况下也会通过，等于没测。
	cc := aliyun.CertConfig{CertName: "cert-abc123", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)}
	for _, s := range []string{cc.String(), cc.GoString()} {
		if strings.Contains(s, "SUPERSECRET") {
			t.Fatalf("私钥出现在格式化输出里: %s", s)
		}
		if !strings.Contains(s, "cert-abc123") {
			t.Errorf("应保留 certName: %s", s)
		}
	}
}

// CustomDomain 自己没有任何方法，Echo.Payload 又是 any：%v / %#v / json.Marshal 三条路
// 都会走反射钻进去，把 payload 里的东西原样打出来。挡住它的必须是 DomainEcho 自己的
// 脱敏方法，而不是「别往 Payload 里塞 certConfig」这句注释——注释拦不住下一个作者。
func TestCustomDomain_RedactsEchoPayload(t *testing.T) {
	cd := aliyun.CustomDomain{
		DomainName: "d.example.com",
		Echo: aliyun.DomainEcho{Payload: map[string]any{
			"certConfig": map[string]any{"privateKey": fakeKeyPEM},
		}},
	}
	b, err := json.Marshal(cd)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v", cd), fmt.Sprintf("%#v", cd), string(b)} {
		if strings.Contains(s, "SUPERSECRET") {
			t.Fatalf("Echo.Payload 里的私钥经反射逃逸: %s", s)
		}
	}
	if !strings.Contains(string(b), "d.example.com") {
		t.Errorf("domainName 应保留: %s", b)
	}
}

func TestCertConfig_MarshalJSONRedactsKey(t *testing.T) {
	cc := aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)}
	for _, v := range []any{cc, &cc} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "SUPERSECRET") {
			t.Fatalf("私钥出现在 JSON 里: %s", b)
		}
	}
}

func TestUpdateCustomDomainInput_RedactsNestedKey(t *testing.T) {
	in := aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)},
		Echo:       aliyun.DomainEcho{Payload: "opaque"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SUPERSECRET") || strings.Contains(in.String(), "SUPERSECRET") {
		t.Fatalf("嵌套私钥泄漏: %s / %s", b, in.String())
	}
	if !strings.Contains(string(b), "HTTP,HTTPS") {
		t.Errorf("protocol 应保留: %s", b)
	}
}

func TestClientCache_TypedPerService(t *testing.T) {
	c := aliyun.NewClientCache[aliyun.FC3Client]()
	key := aliyun.ClientKey{Namespace: "ns", Name: "cred", ResourceVersion: "1", Region: "cn-hangzhou"}
	calls := 0
	build := func() (aliyun.FC3Client, error) { calls++; return nil, nil }

	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("同 resourceVersion 应复用，build 调用了 %d 次", calls)
	}
	key.ResourceVersion = "2"
	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("resourceVersion 变化应重建，build 调用了 %d 次", calls)
	}
}

// 独立通道的判据不是「两个常量都存在」，而是「耗尽 FC3 的桶不会拖慢 CAS 写」。
// 只调一次 Wait 的写法在两个 kind 共用一个桶时同样会通过，等于没测。
func TestLimitFC3_HasOwnBucket(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := t.Context()
	// FC3 是 5 QPS / burst 1：把 burst 里那一个令牌取走，桶就空了。
	if err := l.Wait(ctx, "ak", aliyun.LimitFC3); err != nil {
		t.Fatal(err)
	}
	// 桶空之后再取一个必须等满一个补充周期（1/5 s = 200ms）。判据放宽到 150ms，
	// 给 CI 上的调度抖动留余量。
	start := time.Now()
	if err := l.Wait(ctx, "ak", aliyun.LimitFC3); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("FC3 桶耗尽后应等待约 200ms，实际 %v——限流通道没有独立的 rate/burst", d)
	}
	// 同一个 key 的 CAS 写通道（50 QPS / burst 10）必须完全不受影响。
	start = time.Now()
	if err := l.Wait(ctx, "ak", aliyun.LimitCASWrite); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 50*time.Millisecond {
		t.Errorf("CAS 写通道应立即放行，实际等了 %v——两个 kind 共用了同一个桶", d)
	}
}
