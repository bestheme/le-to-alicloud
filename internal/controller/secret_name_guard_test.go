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
	"bytes"
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// TestSecretOwnedByUs 固定归属判定：只有注解逐字等于本 CR 的 Certificate 名才算我们的。
// 手工建的 Secret（无注解）与别人的 Secret（注解指向别的 Certificate）都不算。
func TestSecretOwnedByUs(t *testing.T) {
	ac := &certsv1alpha1.AliyunCertificate{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "ns"}}
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"注解指向本 CR 的 Certificate", map[string]string{certManagerCertificateNameAnnotation: "mine"}, true},
		{"注解指向别的 Certificate", map[string]string{certManagerCertificateNameAnnotation: "someone-else"}, false},
		{"注解值为空", map[string]string{certManagerCertificateNameAnnotation: ""}, false},
		{"有其它注解但没有归属注解", map[string]string{"foo": "bar"}, false},
		{"完全没有注解（手工建的 Secret）", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine-tls", Namespace: "ns", Annotations: c.annotations}}
			if got := secretOwnedByUs(s, ac); got != c.want {
				t.Errorf("secretOwnedByUs = %v, 期望 %v", got, c.want)
			}
		})
	}
}

var _ = Describe("证书 controller：Secret 归属护栏", func() {
	ctx := context.Background()
	var ca *testutil.CA

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		ca = testutil.NewCA(GinkgoT())
	})

	// issueReady 把一个 CR 推到「已签发并已上传 CAS」的稳定态，等价于 deletion_test 的
	// issuedAC，但这里不需要 certId，所以不返回它。
	issueReady := func(ns, name string, ac *certsv1alpha1.AliyunCertificate) {
		ExpectWithOffset(1, k8sClient.Create(ctx, ac)).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, name, 1, crt, key)
		EventuallyWithOffset(1, func() bool {
			a := getAC(ctx, ns, name)
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	}

	// setSecretName 改 spec.secretName。必须重读重试：reconciler 每轮都 patch status，
	// 而 status 子资源同样顶掉 metadata.resourceVersion（理由与 touchAC 逐字相同）。
	setSecretName := func(ns, name, secretName string) {
		EventuallyWithOffset(1, func() error {
			ac := &certsv1alpha1.AliyunCertificate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ac); err != nil {
				return err
			}
			ac.Spec.SecretName = secretName
			return k8sClient.Update(ctx, ac)
		}, "10s", "100ms").Should(Succeed())
	}

	// 覆写护栏：护栏只在首次创建时跑的话，这里的 Update 会把 Certificate 的 secretName
	// 改到受害 Secret 上，cert-manager 随即用本证书覆写它（评估文档 §4.6 步骤 6）。
	It("spec.secretName 改指到别人的 Secret 时不更新 Certificate，Ready=False/SecretNameConflict", func() {
		// 这里刻意不动 reconciler.ResyncInterval：改 spec 本身就会触发一轮 reconcile，
		// 冲突在那一轮里就判出来了，不需要等 requeue。而 ResyncInterval 是裸字段，
		// manager 的 worker goroutine 正在读它，测试线程写它就是一条 -race 能抓到的竞态。
		ns := newNamespace(ctx)

		// 受害 Secret：注解指向另一个 Certificate，内容是与本证书无关的占位字节。
		victimData := map[string][]byte{
			corev1.TLSCertKey:       []byte("victim-cert-placeholder"),
			corev1.TLSPrivateKeyKey: []byte("victim-key-placeholder"),
		}
		victim := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "victim-tls", Namespace: ns,
				Annotations: map[string]string{certManagerCertificateNameAnnotation: "someone-else"}},
			Data: victimData,
		}
		Expect(k8sClient.Create(ctx, victim)).To(Succeed())

		ac := baseAC(ns, "chg")
		ac.Spec.SecretName = "chg-tls"
		issueReady(ns, "chg", ac)

		setSecretName(ns, "chg", "victim-tls")

		eventually(func() bool {
			return condReason(getAC(ctx, ns, "chg"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonSecretNameConflict
		})

		// Certificate 必须保持旧 secretName——改上去就等于让 cert-manager 覆写受害 Secret。
		Consistently(func() string {
			cert, err := getCert(ctx, ns, "chg")
			if err != nil {
				return "<error>"
			}
			return cert.Spec.SecretName
		}, "1500ms", "200ms").Should(Equal("chg-tls"))

		// 受害 Secret 逐字节不变。用布尔断言而不是直接比对 map：失败时不该把字节倒进日志。
		//
		// 这几条在 envtest 里是**形式上的完备**，不是回归探测器：envtest 没有真的
		// cert-manager 在跑，护栏就算完全失效，被改写的也只是 cmapi.Certificate 的 spec，
		// 没有任何东西会去动这个 Secret。真正的探测器是上面那条
		// Consistently(cert.Spec.SecretName)——删掉它，这个用例就不再守任何东西了。
		got := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "victim-tls"}, got)).To(Succeed())
		gotKeys := make([]string, 0, len(got.Data))
		for k := range got.Data {
			gotKeys = append(gotKeys, k)
		}
		Expect(gotKeys).To(ConsistOf(corev1.TLSCertKey, corev1.TLSPrivateKeyKey), "受害 Secret 的 data key 集合不该变")
		for k, want := range victimData {
			Expect(bytes.Equal(got.Data[k], want)).To(BeTrue(), "受害 Secret 的 data[%s] 不该被改写", k)
		}
		Expect(got.Annotations[certManagerCertificateNameAnnotation]).To(Equal("someone-else"),
			"受害 Secret 的归属注解不该被改写")
	})

	// 删除期复核：Secret 在 CR 生命周期里被别的 Certificate 接管之后，删 CR 不能顺手把它删掉。
	It("删除时跳过不属于本 CR 的 Secret，finalizer 照常摘除", func() {
		ns := newNamespace(ctx)
		issueReady(ns, "foreign", baseAC(ns, "foreign"))

		// 模拟 Secret 已被别人接管：注解改指到另一个 Certificate。
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign-tls"}, s)).To(Succeed())
		s.Annotations[certManagerCertificateNameAnnotation] = "someone-else"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())

		Expect(k8sClient.Delete(ctx, getAC(ctx, ns, "foreign"))).To(Succeed())

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign-tls"}, &corev1.Secret{})).
			To(Succeed(), "不属于本 CR 的 Secret 不能被删除")
		// 跳过删除必须留下痕迹，否则私钥留在集群里这件事就无声无息了。
		// 事件面向用户，文案逐字钉住；类型必须是 Warning。
		Expect(acWarningEventMessage(ctx, ns, "foreign", certsv1alpha1.ReasonSecretNameConflict)).
			To(Equal(secretDeletionSkippedMessage))
		// 事件会随 namespace 一起过期，指标才是能长期告警的那一份痕迹（同 CleanupAbandoned）。
		Expect(promtestutil.ToFloat64(secretDeletionSkippedTotal.WithLabelValues(ns))).
			To(BeNumerically(">=", 1))
	})
})
