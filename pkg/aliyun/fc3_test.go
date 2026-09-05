package aliyun_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

const fakeKeyPEM = "-----BEGIN RSA PRIVATE KEY-----\nSUPERSECRETKEYMATERIAL\n-----END RSA PRIVATE KEY-----\n"

// 私钥泄漏的真实通道是结构化日志：zap 对未知类型走反射 JSON 编码，导出字段会被原样写出。
// 这三个用例分别堵住 %v / %#v / json.Marshal 三条路，值与指针两种形态都覆盖。
func TestCertConfig_StringRedactsKey(t *testing.T) {
	cc := aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)}
	for _, s := range []string{cc.String(), cc.GoString()} {
		if strings.Contains(s, "SUPERSECRET") {
			t.Fatalf("私钥出现在格式化输出里: %s", s)
		}
		if !strings.Contains(s, "n") {
			t.Errorf("应保留 certName: %s", s)
		}
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
