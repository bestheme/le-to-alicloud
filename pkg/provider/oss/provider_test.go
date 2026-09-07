package oss_test

import (
	"context"
	"errors"
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"
)

const (
	testBucket = "applanding-test"
	testDomain = "www.example.com"
	testRef    = "27087165-cn-hangzhou"
)

func targetFor() provider.Target {
	spec := &certsv1alpha1.OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: testBucket, DomainName: testDomain}
	return provider.Target{
		Type: certsv1alpha1.TargetTypeOSSCustomDomain, Region: "cn-hangzhou", Identifier: testDomain, Spec: spec,
	}
}

func materialFor() provider.CertMaterial {
	id := int64(27087165)
	return provider.CertMaterial{Fingerprint: "abc123", CertID: &id, CASRegion: "cn-hangzhou"}
}

func TestProvider_Identity(t *testing.T) {
	p := &oss.Provider{}
	if p.Name() != certsv1alpha1.TargetTypeOSSCustomDomain {
		t.Errorf("Name 必须与 CRD 的 target.type 取值一致: %s", p.Name())
	}
	c := p.Capabilities()
	if !c.ReferencesCertByID || !c.RequiresCASUpload || c.SupportsProtocolSwitch {
		t.Errorf("OSS 按 certId 引用、强制上传 CAS、无协议开关: %+v", c)
	}
	if got, ok := provider.Get(certsv1alpha1.TargetTypeOSSCustomDomain); !ok || got.Name() != p.Name() {
		t.Error("init() 应把自己注册进 registry")
	}
}

func TestProvider_ObserveApplyRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := fake.NewOSS()
	f.SetOwner("1234567890")
	f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
	p := &oss.Provider{}

	obs, err := p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentCertRef != "" || obs.CurrentFingerprint != "" || obs.AccountID != "1234567890" {
		t.Fatalf("空证书 CNAME 的观测不对: %+v", obs)
	}

	if err := p.Apply(ctx, targetFor(), f, materialFor(), provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	obs, err = p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentCertRef != testRef {
		t.Errorf("写入后应观测到同一 certRef: %s != %s", obs.CurrentCertRef, testRef)
	}
	if obs.CurrentFingerprint != "" {
		t.Error("OSS 不产出指纹，CurrentFingerprint 必须恒为空")
	}
	if f.PutCallsFor(testBucket, testDomain) != 1 {
		t.Errorf("Apply 应恰好写一次: %d", f.PutCallsFor(testBucket, testDomain))
	}
}

func TestProvider_ApplyRejectsEmptyCertRef(t *testing.T) {
	f := fake.NewOSS()
	f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
	m := materialFor()
	m.CertID = nil
	err := (&oss.Provider{}).Apply(context.Background(), targetFor(), f, m, provider.ApplyOptions{})
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodePermanent || pe.Reason != certsv1alpha1.ReasonApplyFailed {
		t.Fatalf("空 certRef 应为 Permanent/ApplyFailed: %v", err)
	}
	if f.PutCallsFor(testBucket, testDomain) != 0 {
		t.Error("空 certRef 不该发任何云调用")
	}
}

func TestProvider_ObserveTargetNotFound(t *testing.T) {
	p := &oss.Provider{}
	for name, f := range map[string]*fake.OSS{
		"bucket 不存在": fake.NewOSS(),
		"CNAME 不存在":  func() *fake.OSS { f := fake.NewOSS(); f.AddBucket(testBucket); return f }(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Observe(context.Background(), targetFor(), f)
			pe := provider.ErrorOf(err)
			if pe == nil || pe.Code != provider.CodeTargetNotFound || pe.Retryable ||
				pe.Reason != certsv1alpha1.ReasonTargetNotFound {
				t.Fatalf("应映射为不可重试的 TargetNotFound: %v", err)
			}
		})
	}
}

func TestProvider_ErrorMapping(t *testing.T) {
	ctx := context.Background()
	mk := func(class aliyun.ErrClass, code string) *aliyun.Error {
		return &aliyun.Error{Class: class, Op: aliyun.ActionPutCname, Code: code, Err: errors.New("injected")}
	}
	cases := []struct {
		name   string
		inject *aliyun.Error
		code   string
		retry  bool
		reason string
	}{
		{"鉴权", mk(aliyun.ClassAuth, "AccessDenied"), provider.CodeAuth, false, certsv1alpha1.ReasonCredentialsInvalid},
		{"限流", mk(aliyun.ClassRetryable, "Throttling.User"), provider.CodeThrottled, true, certsv1alpha1.ReasonThrottled},
		{"瞬时", mk(aliyun.ClassRetryable, "InternalError"), provider.CodeRetryable, true, certsv1alpha1.ReasonApplyFailed},
		{"永久", mk(aliyun.ClassPermanent, "InvalidArgument"), provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.NewOSS()
			f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
			f.QueuePutErr(tc.inject)
			err := (&oss.Provider{}).Apply(ctx, targetFor(), f, materialFor(), provider.ApplyOptions{})
			pe := provider.ErrorOf(err)
			if pe == nil || pe.Code != tc.code || pe.Retryable != tc.retry || pe.Reason != tc.reason {
				t.Fatalf("映射不对: %v", err)
			}
			if !errors.Is(err, tc.inject) {
				t.Error("必须保住 Unwrap 链")
			}
		})
	}
}

func TestProvider_Cleanup(t *testing.T) {
	ctx := context.Background()
	p := &oss.Provider{}

	t.Run("Orphan 零调用", func(t *testing.T) {
		f := fake.NewOSS()
		f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain, CertRef: testRef, CertType: "CAS"})
		if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyOrphan); err != nil {
			t.Fatal(err)
		}
		if f.ListCallsFor(testBucket, testDomain)+f.PutCallsFor(testBucket, testDomain) != 0 {
			t.Error("Orphan 不该发任何云调用")
		}
	})
	t.Run("Unbind 摘证书、CNAME 保留", func(t *testing.T) {
		f := fake.NewOSS()
		f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain, CertRef: testRef, CertType: "CAS"})
		if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyUnbind); err != nil {
			t.Fatal(err)
		}
		rec, ok := f.Cname(testBucket, testDomain)
		if !ok || rec.CertRef != "" {
			t.Fatalf("Unbind 后 CNAME 应保留且无证书: %+v %v", rec, ok)
		}
	})
	t.Run("Unbind 时目标已不存在视为成功", func(t *testing.T) {
		if err := p.Cleanup(ctx, targetFor(), fake.NewOSS(), provider.DeletionPolicyUnbind); err != nil {
			t.Fatalf("NotFound 应被吞掉: %v", err)
		}
	})
}

func TestProvider_InvalidClient(t *testing.T) {
	_, err := (&oss.Provider{}).Observe(context.Background(), targetFor(), "not-a-client")
	if pe := provider.ErrorOf(err); pe == nil || pe.Code != provider.CodeInvalidClient {
		t.Fatalf("传错 client 类型应为 CodeInvalidClient: %v", err)
	}
}

func TestProvider_InvalidTargetSpec(t *testing.T) {
	tg := targetFor()
	tg.Spec = &certsv1alpha1.FC3CustomDomainTarget{}
	_, err := (&oss.Provider{}).Observe(context.Background(), tg, fake.NewOSS())
	if pe := provider.ErrorOf(err); pe == nil || pe.Code != provider.CodeInvalidTarget {
		t.Fatalf("Target.Spec 断言失败应为 CodeInvalidTarget: %v", err)
	}
}
