/*
Copyright 2026 Hangzhou Yunqi Intelligence Technology Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"

	// 注册表里要有两个 provider，capabilitiesOf 才有东西可查。provider_factory.go 已经
	// 空导入了 fc3；oss 在 Task 8 之前还没接进生产代码，这里先由测试导入。
	_ "git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"
)

func ossBinding(bucket, domain string) *certsv1alpha1.AliyunCertificateBinding {
	return &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{
				Type:            certsv1alpha1.TargetTypeOSSCustomDomain,
				OSSCustomDomain: &certsv1alpha1.OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: bucket, DomainName: domain},
			},
		},
	}
}

func TestIdentityOf(t *testing.T) {
	id := int64(42)
	m := provider.CertMaterial{Fingerprint: "ffff", CertID: &id, CASRegion: "cn-hangzhou"}

	fc := bindingWithDomain("api.example.com")
	fc.Status.AppliedFingerprint, fc.Status.AppliedCertRef = "aaaa", "should-be-ignored"
	got := identityOf(fc, provider.ObservedState{CurrentFingerprint: "cccc", CurrentCertRef: "ignored"}, m)
	if got.byRef || got.want != "ffff" || got.current != "cccc" || got.applied != "aaaa" {
		t.Errorf("FC3 应按指纹取三元组: %+v", got)
	}

	o := ossBinding("b1", "www.example.com")
	o.Status.AppliedFingerprint, o.Status.AppliedCertRef = "aaaa", "41-cn-hangzhou"
	got = identityOf(o, provider.ObservedState{CurrentFingerprint: "ignored", CurrentCertRef: "40-cn-hangzhou"}, m)
	if !got.byRef || got.want != "42-cn-hangzhou" || got.current != "40-cn-hangzhou" || got.applied != "41-cn-hangzhou" {
		t.Errorf("OSS 应按 certRef 取三元组: %+v", got)
	}

	unknown := &certsv1alpha1.AliyunCertificateBinding{}
	unknown.Spec.Target.Type = "Nope"
	if got := identityOf(unknown, provider.ObservedState{CurrentFingerprint: "cccc"}, m); got.byRef || got.current != "cccc" {
		t.Errorf("没注册的类型退回指纹路径: %+v", got)
	}
}

func TestEnsureHTTPSOf(t *testing.T) {
	fc := bindingWithDomain("api.example.com")
	if ensureHTTPSOf(fc) {
		t.Error("默认 false")
	}
	fc.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol = true
	if !ensureHTTPSOf(fc) {
		t.Error("FC3 读字段")
	}
	if ensureHTTPSOf(ossBinding("b1", "www.example.com")) {
		t.Error("OSS 恒为 false")
	}
	if ensureHTTPSOf(&certsv1alpha1.AliyunCertificateBinding{}) {
		t.Error("内嵌块缺失时不得 panic，返回 false")
	}
}

func TestIdentityLabel(t *testing.T) {
	if got := identityLabel(certIdentity{byRef: true}, "27087165-cn-hangzhou"); got != "27087165-cn-hangzhou" {
		t.Errorf("certRef 不脱敏也不截断: %s", got)
	}
	if got := identityLabel(certIdentity{}, "0123456789abcdef"); got != "01234567" {
		t.Errorf("指纹只留前 8 位: %s", got)
	}
}

func TestTargetHelpers_OSS(t *testing.T) {
	b := ossBinding("b1", "www.example.com")
	tg, err := targetOf(b)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Type != certsv1alpha1.TargetTypeOSSCustomDomain || tg.Region != "cn-hangzhou" || tg.Identifier != "www.example.com" {
		t.Fatalf("Target 不对: %+v", tg)
	}
	if tg.Spec != b.Spec.Target.OSSCustomDomain {
		t.Error("Spec 应指向 CRD 内嵌结构体本身，provider 才能断言出 bucket")
	}
	if got := requiredDomainsOf(b); len(got) != 1 || got[0] != "www.example.com" {
		t.Errorf("OSS 要求覆盖 domainName: %v", got)
	}
	if targetIdentifier(b) != "www.example.com" || targetRegion(b) != "cn-hangzhou" {
		t.Errorf("日志 / label 辅助函数应认得 OSS: %s %s", targetIdentifier(b), targetRegion(b))
	}
	b.Spec.Target.OSSCustomDomain = nil
	if _, err := targetOf(b); err == nil {
		t.Error("缺内嵌块应报错")
	}
	if targetIdentifier(b) != "" || targetRegion(b) != "" {
		t.Error("内嵌块缺失时不得 panic")
	}
}
