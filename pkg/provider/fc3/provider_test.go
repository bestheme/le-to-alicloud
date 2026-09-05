package fc3_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"
)

const testDomain = "api.example.com"

// errAny 是注入用的哨兵：分类由 aliyun.Error.Class 决定，被包住的错误内容无关紧要。
var errAny = errors.New("injected")

func targetFor() provider.Target {
	return provider.Target{
		Type:       certsv1alpha1.TargetTypeFC3CustomDomain,
		Region:     "cn-hangzhou",
		Identifier: testDomain,
		Spec:       &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: testDomain},
	}
}

func materialFor(t *testing.T) (provider.CertMaterial, string) {
	t.Helper()
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, testDomain)
	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	key, err := b.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	return provider.CertMaterial{
		Fingerprint: b.Fingerprint,
		CertPEM:     b.CertPEM(),
		KeyPEM:      key,
		CASName:     "cert_" + b.Fingerprint[:12],
		NotAfter:    b.Leaf.NotAfter,
		DNSNames:    b.DNSNames(),
	}, b.Fingerprint
}

func TestProvider_Identity(t *testing.T) {
	p := &fc3.Provider{}
	if p.Name() != certsv1alpha1.TargetTypeFC3CustomDomain {
		t.Errorf("Name 必须与 CRD 的 target.type 取值一致: %s", p.Name())
	}
	c := p.Capabilities()
	if c.ReferencesCertByID || c.RequiresCASUpload || !c.SupportsProtocolSwitch {
		t.Errorf("FC3 内联 PEM、不引用 certId、不强制上传 CAS、支持切协议: %+v", c)
	}
	if got, ok := provider.Get(certsv1alpha1.TargetTypeFC3CustomDomain); !ok || got.Name() != p.Name() {
		t.Error("init() 应把自己注册进 registry")
	}
}

func TestProvider_ObserveEmptyAndCertified(t *testing.T) {
	ctx := context.Background()
	m, fp := materialFor(t)
	f := fake.NewFC3()
	f.SetAccountID("1234567890")
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP", Echo: "routes"})
	p := &fc3.Provider{}

	obs, err := p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists || obs.CurrentFingerprint != "" || obs.Protocol != "HTTP" || obs.AccountID != "1234567890" {
		t.Fatalf("空证书域名的观测不对: %+v", obs)
	}

	if err := p.Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	obs, err = p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentFingerprint != fp {
		t.Errorf("写入后应观测到同一指纹: %s != %s", obs.CurrentFingerprint, fp)
	}
}

func TestProvider_ObserveTargetNotFound(t *testing.T) {
	f := fake.NewFC3()
	_, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), f)
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodeTargetNotFound {
		t.Fatalf("应映射为 CodeTargetNotFound: %v", err)
	}
	if pe.Retryable {
		t.Error("域名不存在不该被指数退避重试；通用层用固定 5m requeue")
	}
	if pe.Reason != certsv1alpha1.ReasonTargetNotFound {
		t.Errorf("Reason 应是 TargetNotFound: %s", pe.Reason)
	}
}

func TestProvider_ApplyPreservesEchoAndProtocol(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP", Echo: "routes"})

	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain(testDomain)
	if d.Echo != "routes" {
		t.Errorf("回填体丢失: %v", d.Echo)
	}
	if d.Protocol != "HTTP" {
		t.Errorf("EnsureHTTPSProtocol=false 时绝不能动 protocol: %s", d.Protocol)
	}
	if d.CertName != m.CASName || string(d.KeyPEM) != string(m.KeyPEM) {
		t.Errorf("证书未写入: certName=%s keyBytes=%d", d.CertName, len(d.KeyPEM))
	}
}

func TestProvider_ApplyEnsureHTTPS(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP"})

	opts := provider.ApplyOptions{EnsureHTTPSProtocol: true}
	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, opts); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.Domain(testDomain); d.Protocol != "HTTP,HTTPS" {
		t.Errorf("应升为 HTTP,HTTPS: %s", d.Protocol)
	}

	// 已经含 HTTPS 时不许改：把 "HTTPS" 改成 "HTTP,HTTPS" 等于替用户打开了明文入口。
	f2 := fake.NewFC3()
	f2.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS"})
	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f2, m, opts); err != nil {
		t.Fatal(err)
	}
	if d, _ := f2.Domain(testDomain); d.Protocol != "HTTPS" {
		t.Errorf("已含 HTTPS 时不该改动: %s", d.Protocol)
	}
}

func TestProvider_ApplyErrorClasses(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	const op = "UpdateCustomDomain"
	cases := []struct {
		name      string
		inject    error
		code      string
		retryable bool
		reason    string
	}{
		{
			name:   "throttling",
			inject: &aliyun.Error{Class: aliyun.ClassRetryable, Op: op, Code: "Throttling", Err: errAny},
			code:   provider.CodeThrottled, retryable: true, reason: certsv1alpha1.ReasonThrottled,
		},
		{
			name:   "auth",
			inject: &aliyun.Error{Class: aliyun.ClassAuth, Op: op, Code: "Forbidden", Err: errAny},
			code:   provider.CodeAuth, retryable: false, reason: certsv1alpha1.ReasonCredentialsInvalid,
		},
		{
			name:   "permanent",
			inject: &aliyun.Error{Class: aliyun.ClassPermanent, Op: op, Code: "InvalidParameter", Err: errAny},
			code:   provider.CodePermanent, retryable: false, reason: certsv1alpha1.ReasonApplyFailed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fake.NewFC3()
			f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP"})
			f.QueueUpdateErr(c.inject)
			err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, provider.ApplyOptions{})
			pe := provider.ErrorOf(err)
			if pe == nil {
				t.Fatalf("应包成 ProviderError: %v", err)
			}
			if pe.Code != c.code || pe.Retryable != c.retryable || pe.Reason != c.reason {
				t.Errorf("分类错误: %+v", pe)
			}
		})
	}
}

func TestProvider_CleanupUnbind(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS", Echo: "routes"})
	p := &fc3.Provider{}
	if err := p.Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyUnbind); err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain(testDomain)
	if d.CertName != "" || len(d.KeyPEM) != 0 {
		t.Fatalf("证书应被清空: certName=%s keyBytes=%d", d.CertName, len(d.KeyPEM))
	}
	if d.Protocol != "HTTP" {
		t.Errorf("纯 HTTPS 域名清掉证书后必须降为 HTTP，否则域名彻底不可用: %s", d.Protocol)
	}
	if d.Echo != "routes" {
		t.Errorf("解绑也要保住回填体: %v", d.Echo)
	}
}

func TestProvider_CleanupOrphanTouchesNothing(t *testing.T) {
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS", CertName: "c1"})
	err := (&fc3.Provider{}).Cleanup(context.Background(), targetFor(), f, provider.DeletionPolicyOrphan)
	if err != nil {
		t.Fatal(err)
	}
	if f.GetCalls() != 0 || f.UpdateCalls() != 0 {
		t.Errorf("Orphan 一次云调用都不该发生: get=%d update=%d", f.GetCalls(), f.UpdateCalls())
	}
}

func TestProvider_CleanupMissingDomainIsSuccess(t *testing.T) {
	f := fake.NewFC3()
	err := (&fc3.Provider{}).Cleanup(context.Background(), targetFor(), f, provider.DeletionPolicyUnbind)
	if err != nil {
		t.Errorf("域名已经不在了，解绑的目的已经达到: %v", err)
	}
}

func TestProvider_RejectsForeignClient(t *testing.T) {
	_, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), "not a client")
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodeInvalidClient {
		t.Fatalf("client 类型不对应报 CodeInvalidClient: %v", err)
	}
}

func TestProvider_ErrorMessageCarriesOpAndCodeOnly(t *testing.T) {
	const marker = "leaky-response-body"
	f := fake.NewFC3()
	f.QueueGetErr(&aliyun.Error{
		Class: aliyun.ClassPermanent, Op: aliyun.ActionGetCustomDomain,
		Code: "InvalidParameter", Err: errors.New(marker),
	})
	_, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), f)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	// ProviderError.Error() 刻意不回显被包住的错误，所以「哪个操作、云侧报的哪个码」
	// 只能由前缀带出来；缺了它，日志里就只剩一句「permanent failure」。
	if !strings.Contains(msg, aliyun.ActionGetCustomDomain+"[InvalidParameter]") {
		t.Errorf("错误消息缺少 op[code] 前缀: %s", msg)
	}
	// 但也仅止于此：被包住的错误文本（真实世界里可能是 SDK 响应体）绝不能进消息。
	if strings.Contains(msg, marker) {
		t.Error("错误消息回显了被包住的错误内容")
	}
}

func TestProvider_ApplyRejectsEmptyCASName(t *testing.T) {
	m, _ := materialFor(t)
	m.CASName = ""
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP"})

	err := (&fc3.Provider{}).Apply(context.Background(), targetFor(), f, m, provider.ApplyOptions{})
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodePermanent {
		t.Fatalf("空 certName 应就地判永久失败: %v", err)
	}
	// 在发请求之前挡住：让云侧用一句「参数不合法」来告诉我们这件事既慢又难读。
	if f.GetCalls() != 0 || f.UpdateCalls() != 0 {
		t.Errorf("不该发出任何云调用: get=%d update=%d", f.GetCalls(), f.UpdateCalls())
	}
}

func TestProvider_ObserveUnparsableCertIsNoCert(t *testing.T) {
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{
		DomainName: testDomain, Protocol: "HTTPS",
		CertName: "someone-elses", CertPEM: []byte("-----BEGIN CERTIFICATE-----\nnot base64\n"),
	})

	obs, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), f)
	// 解析不了 = 肯定不是我们写的那张。当成「没有证书」，通用层照常覆盖；
	// 在这里报错只会让 Binding 永久卡在一张我们根本管不着的证书上。
	if err != nil {
		t.Fatalf("云上证书解析失败不该让 Observe 报错: %v", err)
	}
	if !obs.Exists || obs.CurrentFingerprint != "" || obs.Protocol != "HTTPS" {
		t.Errorf("应观测成「域名在、但没有我们的证书」: %+v", obs)
	}
}
