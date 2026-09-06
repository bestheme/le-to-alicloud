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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

func TestCredentialsSecretNameFor_InheritsFromCertificate(t *testing.T) {
	ac := &certsv1alpha1.AliyunCertificate{
		Spec: certsv1alpha1.AliyunCertificateSpec{
			Aliyun: certsv1alpha1.AliyunSpec{CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "cas-cred"}},
		},
	}
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if got := credentialsSecretNameFor(b, ac); got != "cas-cred" {
		t.Errorf("缺省应继承证书的凭证: %s", got)
	}

	b.Spec.CredentialsRef = &certsv1alpha1.LocalSecretReference{Name: "fc-cred"}
	if got := credentialsSecretNameFor(b, ac); got != "fc-cred" {
		t.Errorf("Binding 自己的凭证应优先: %s", got)
	}
}

func TestCredentialsSecretNameFor_NoCertificate(t *testing.T) {
	// 删除分支里证书可能已经不在了，此时只有 Binding 自己的 credentialsRef 可用。
	b := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CredentialsRef: &certsv1alpha1.LocalSecretReference{Name: "fc-cred"},
		},
	}
	if got := credentialsSecretNameFor(b, nil); got != "fc-cred" {
		t.Errorf("证书缺失时应仍能取到自己的凭证: %s", got)
	}
	if got := credentialsSecretNameFor(&certsv1alpha1.AliyunCertificateBinding{}, nil); got != "" {
		t.Errorf("两边都没有时应返回空串: %q", got)
	}
}

func TestTargetOf(t *testing.T) {
	b := bindingWithDomain("api.example.com")
	tg, err := targetOf(b)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Type != certsv1alpha1.TargetTypeFC3CustomDomain || tg.Region != "cn-hangzhou" || tg.Identifier != "api.example.com" {
		t.Fatalf("Target 不对: %+v", tg)
	}
	if tg.Spec != b.Spec.Target.FC3CustomDomain {
		t.Error("Spec 应指向 CRD 内嵌结构体本身，provider 才能断言出 ensureHTTPSProtocol")
	}

	bad := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{Target: certsv1alpha1.BindingTarget{Type: "Nope"}},
	}
	if _, err := targetOf(bad); err == nil {
		t.Error("未知 target 类型应报错")
	}
}

// --- 生产工厂本体 ---------------------------------------------------------

// 假 AK。只出现在测试的输入里，断言会逐条检查它们不出现在任何错误消息中。
const (
	testAKID     = "LTAI-test-id"
	testAKSecret = "test-access-key-secret"
)

func factoryScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := certsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("certsv1alpha1.AddToScheme: %v", err)
	}
	return s
}

func credSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
		Type:       aliyun.SecretTypeAliyunCredentials,
		Data:       data,
	}
}

func akSecret(name string) *corev1.Secret {
	return credSecret(name, map[string][]byte{
		aliyun.KeyAccessKeyID:     []byte(testAKID),
		aliyun.KeyAccessKeySecret: []byte(testAKSecret),
	})
}

// factoryFixture 把一次工厂调用需要的全部零件收在一起。证书的凭证叫 cas-cred，
// Binding 自己的（若设置）叫 fc-cred，两者都真实存在于 fake client 里。
type factoryFixture struct {
	f     ProviderFactory
	c     client.Client
	cache *aliyun.ClientCache[aliyun.FC3Client]
	b     *certsv1alpha1.AliyunCertificateBinding
	ac    *certsv1alpha1.AliyunCertificate
}

func newFactoryFixture(t *testing.T, objs ...client.Object) *factoryFixture {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(factoryScheme(t)).WithObjects(objs...).Build()
	cache := aliyun.NewClientCache[aliyun.FC3Client]()
	b := bindingWithDomain("api.example.com")
	b.Namespace = "ns1"
	b.Name = "b1"
	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "c1"},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "cas-cred"},
			},
		},
	}
	return &factoryFixture{
		f: NewProviderFactory(c, cache, aliyun.NewLimiters(), 5*time.Second),
		c: c, cache: cache, b: b, ac: ac,
	}
}

// call 跑一次工厂。
func (fx *factoryFixture) call(t *testing.T) (provider.Provider, provider.Client, error) {
	t.Helper()
	return fx.f(context.Background(), fx.b, fx.ac)
}

// rotate 改写凭证 Secret 的内容，fake client 会随之推进 resourceVersion——
// 这正是线上轮换 AK 的形状。
func (fx *factoryFixture) rotate(t *testing.T, name string) {
	t.Helper()
	s := &corev1.Secret{}
	if err := fx.c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns1", Name: name}, s); err != nil {
		t.Fatalf("取凭证 Secret: %v", err)
	}
	before := s.ResourceVersion
	s.Data[aliyun.KeyAccessKeySecret] = []byte(testAKSecret + "-rotated")
	if err := fx.c.Update(context.Background(), s); err != nil {
		t.Fatalf("轮换凭证 Secret: %v", err)
	}
	if err := fx.c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns1", Name: name}, s); err != nil {
		t.Fatalf("回读凭证 Secret: %v", err)
	}
	// 这条断言守的是 fixture 本身：fake client 若不推进 resourceVersion，
	// 下面的「轮换后必须重建」用例就会变成一个什么都不检验的用例。
	if s.ResourceVersion == before {
		t.Fatalf("fake client 没有推进 resourceVersion，用例前提不成立")
	}
}

// assertNoAKLeak 是零凭证泄漏那条约束的可执行版本。
func assertNoAKLeak(t *testing.T, msg string) {
	t.Helper()
	for _, secret := range []string{testAKID, testAKSecret} {
		if strings.Contains(msg, secret) {
			t.Fatalf("错误消息泄漏了凭证材料")
		}
	}
}

func TestNewProviderFactory_InheritsCertificateSecret(t *testing.T) {
	fx := newFactoryFixture(t, akSecret("cas-cred"))

	p, cl, err := fx.call(t)
	if err != nil {
		t.Fatalf("应能用证书的凭证造出 client: %v", err)
	}
	if p == nil || p.Name() != certsv1alpha1.TargetTypeFC3CustomDomain {
		t.Fatalf("应取到 fc3 provider: %v", p)
	}
	if cl == nil {
		t.Fatal("client 不应为 nil")
	}
}

// 缓存必须按**实例**断言，不能只看条目数：缓存是就地覆盖同一个 identity 的，
// 每次都重建照样只有一条，`cache.Len() == 1` 对「根本没复用」和「复用了」一样绿。
func TestNewProviderFactory_ReusesClientForSameSecretVersion(t *testing.T) {
	fx := newFactoryFixture(t, akSecret("cas-cred"))

	_, first, err := fx.call(t)
	if err != nil {
		t.Fatalf("第一次调用: %v", err)
	}
	_, second, err := fx.call(t)
	if err != nil {
		t.Fatalf("第二次调用: %v", err)
	}
	if first != second {
		t.Error("同一 (Secret, resourceVersion, region) 应复用同一个 client 实例")
	}
	if fx.cache.Len() != 1 {
		t.Errorf("缓存条目数 = %d，应为 1", fx.cache.Len())
	}
}

// 轮换 AK 之后必须换一个新 client。这是「AK 被吊销后仍被缓存里的旧 client 一直用下去」
// 那个 bug 的直接反面：ClientKey 的 identity() 刻意不含 resourceVersion，全靠工厂把
// Secret 的 resourceVersion 填进 key——把它写死成空串，本用例会红，上一个则不会。
func TestNewProviderFactory_RebuildsClientAfterSecretRotation(t *testing.T) {
	fx := newFactoryFixture(t, akSecret("cas-cred"))

	_, before, err := fx.call(t)
	if err != nil {
		t.Fatalf("轮换前: %v", err)
	}
	fx.rotate(t, "cas-cred")
	_, after, err := fx.call(t)
	if err != nil {
		t.Fatalf("轮换后: %v", err)
	}
	if before == after {
		t.Error("凭证 Secret 的 resourceVersion 变了就必须重建 client")
	}
	// 重建是替换而不是堆积：identity 不含 resourceVersion，旧条目应被就地覆盖。
	if fx.cache.Len() != 1 {
		t.Errorf("缓存条目数 = %d，应为 1（就地覆盖而不是堆积）", fx.cache.Len())
	}
}

func TestNewProviderFactory_BindingCredentialsWin(t *testing.T) {
	// 只放 fc-cred：如果工厂错误地用了证书的 cas-cred，就会 NotFound。
	fx := newFactoryFixture(t, akSecret("fc-cred"))
	fx.b.Spec.CredentialsRef = &certsv1alpha1.LocalSecretReference{Name: "fc-cred"}

	if _, _, err := fx.call(t); err != nil {
		t.Fatalf("Binding 自己的凭证应优先: %v", err)
	}
}

func TestNewProviderFactory_SecretNotFound(t *testing.T) {
	fx := newFactoryFixture(t)

	_, _, err := fx.call(t)
	var ce *credentialsError
	if !errors.As(err, &ce) || ce.Reason != certsv1alpha1.ReasonCredentialsNotFound {
		t.Fatalf("应是 CredentialsSecretNotFound: %v", err)
	}
}

func TestNewProviderFactory_NoCredentialsRefAtAll(t *testing.T) {
	fx := newFactoryFixture(t)
	// ac 为 nil 且 Binding 没设 credentialsRef：无从解析，属于凭证类失败而不是接线错误。
	fx.ac = nil

	_, _, err := fx.call(t)
	var ce *credentialsError
	if !errors.As(err, &ce) || ce.Reason != certsv1alpha1.ReasonCredentialsNotFound {
		t.Fatalf("应是 CredentialsSecretNotFound: %v", err)
	}
}

func TestNewProviderFactory_InvalidCredentials(t *testing.T) {
	// 只有 accessKeyId、没有 accessKeySecret：CredentialsFromSecret 判定无效。
	fx := newFactoryFixture(t, credSecret("cas-cred", map[string][]byte{
		aliyun.KeyAccessKeyID: []byte(testAKID),
	}))

	_, _, err := fx.call(t)
	var ce *credentialsError
	if !errors.As(err, &ce) || ce.Reason != certsv1alpha1.ReasonCredentialsInvalid {
		t.Fatalf("应是 CredentialsInvalid: %v", err)
	}
	// 这条消息会原样进 condition，任何有 namespace 读权限的人都看得见。
	assertNoAKLeak(t, ce.Error())
}

func TestNewProviderFactory_UnknownTargetType(t *testing.T) {
	fx := newFactoryFixture(t, akSecret("cas-cred"))
	fx.b.Spec.Target = certsv1alpha1.BindingTarget{Type: "Nope"}

	_, _, err := fx.call(t)
	if err == nil {
		t.Fatal("未知 target.type 应报错")
	}
	// 接线错误不是凭证错误：handleFactoryError 据此选 ApplyFailed 而不是凭证 reason。
	var ce *credentialsError
	if errors.As(err, &ce) {
		t.Errorf("不该被归成凭证错误: %v", err)
	}
}

func TestProviderClient_NilFactory(t *testing.T) {
	// 接线漏了 ProviderFactory 时给一条错误，而不是在 worker 里 nil 函数调用 panic。
	r := &AliyunCertificateBindingReconciler{}
	if _, _, err := r.providerClient(context.Background(), bindingWithDomain("a.example.com"), nil); err == nil {
		t.Error("未配置 ProviderFactory 应报错而不是 panic")
	}
}

// --- handleFactoryError ---------------------------------------------------

func handleFixture(t *testing.T) (*AliyunCertificateBindingReconciler, *bindingRound) {
	t.Helper()
	b := bindingWithDomain("api.example.com")
	b.Namespace = "ns1"
	b.Name = "b1"
	c := fake.NewClientBuilder().WithScheme(factoryScheme(t)).
		WithObjects(b.DeepCopy()).
		WithStatusSubresource(&certsv1alpha1.AliyunCertificateBinding{}).Build()
	return &AliyunCertificateBindingReconciler{Client: c, APIReader: c}, newBindingRound(b)
}

func TestHandleFactoryError_Credentials(t *testing.T) {
	r, rd := handleFixture(t)

	res, err := r.handleFactoryError(context.Background(), rd,
		&credentialsError{certsv1alpha1.ReasonCredentialsNotFound, errors.New("凭证 Secret 不存在")})
	if err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if res.RequeueAfter != credentialsRequeue {
		t.Errorf("应长 requeue %s，得到 %s", credentialsRequeue, res.RequeueAfter)
	}
	if res != (ctrl.Result{RequeueAfter: credentialsRequeue}) {
		t.Errorf("除 RequeueAfter 外不该带别的字段: %+v", res)
	}
	if got := bindingCondReason(rd.b, certsv1alpha1.ConditionReady); got != certsv1alpha1.ReasonCredentialsNotFound {
		t.Errorf("Ready reason = %q", got)
	}
	// 不是 bypass 失败，但也没有任何证据说明目标上那张证书是错的：Applied 一个字节都不动。
	// 断言 condition **根本不存在**（零值）而不是「不为 True」：后者在有人开始往这条
	// 路径写 Applied=False 时照样通过，而那正是这里要拦的事。
	if c := condOrZero(rd.b, certsv1alpha1.ConditionApplied); c.Status != "" {
		t.Errorf("不该写 Applied: %+v", *c)
	}
	if rd.b.Status.AppliedFingerprint != "" {
		t.Error("早退路径不该写 appliedFingerprint")
	}
}

func TestHandleFactoryError_WiringErrorFallsBackToApplyFailed(t *testing.T) {
	r, rd := handleFixture(t)

	res, err := r.handleFactoryError(context.Background(), rd, errors.New("没有注册 provider"))
	if err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if res.RequeueAfter != credentialsRequeue {
		t.Errorf("应长 requeue %s，得到 %s", credentialsRequeue, res.RequeueAfter)
	}
	if got := bindingCondReason(rd.b, certsv1alpha1.ConditionReady); got != certsv1alpha1.ReasonApplyFailed {
		t.Errorf("Ready reason = %q", got)
	}
}

// condOrZero 让「condition 根本不存在」与「存在但不是 True」在断言里可区分。
func condOrZero(b *certsv1alpha1.AliyunCertificateBinding, condType string) *metav1.Condition {
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == condType {
			return &b.Status.Conditions[i]
		}
	}
	return &metav1.Condition{}
}
