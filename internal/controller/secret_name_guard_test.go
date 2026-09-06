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

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// TestServingSecretName 固定「读哪个 Secret」的决策：已经生效的那个名字优先，只有它为空
// （对象还没走完第一轮）才回落到 spec。护栏拦住期间 spec 指着别人的 Secret，回落错了就是
// 读别人的私钥。
func TestServingSecretName(t *testing.T) {
	cases := []struct {
		name       string
		specSecret string
		effective  string
		want       string
	}{
		{"生效值优先于 spec", "victim-tls", "mine-tls", "mine-tls"},
		{"生效值为空时回落到 spec.secretName", "explicit-tls", "", "explicit-tls"},
		{"生效值与 spec 都为空时回落到 <name>-tls", "", "", "mine-tls"},
		{"spec 为空但已有生效值", "", "old-tls", "old-tls"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac := &certsv1alpha1.AliyunCertificate{
				ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "ns"},
				Spec:       certsv1alpha1.AliyunCertificateSpec{SecretName: c.specSecret},
			}
			if got := servingSecretName(ac, c.effective); got != c.want {
				t.Errorf("servingSecretName = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// TestSecretNameGuardHolding 固定「护栏是不是正拦着」的判据。它是 aggregateReady 一票否决
// Ready 的唯一依据：冲突期间 Issued / Uploaded 都会正常变 True，只有这一条拦得住 Ready。
func TestSecretNameGuardHolding(t *testing.T) {
	cases := []struct {
		name         string
		specSecret   string
		statusSecret string
		want         bool
	}{
		{"spec 改指到别处而在役的还是旧的", "victim-tls", "mine-tls", true},
		{"两者一致", "mine-tls", "mine-tls", false},
		{"spec 为空、在役的是默认名", "", "mine-tls", false},
		{"还没走完第一轮（status 为空）", "victim-tls", "", false},
		{"清空 spec.secretName 回落默认名，而在役的是别的", "", "custom-tls", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac := &certsv1alpha1.AliyunCertificate{
				ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "ns"},
				Spec:       certsv1alpha1.AliyunCertificateSpec{SecretName: c.specSecret},
				Status:     certsv1alpha1.AliyunCertificateStatus{SecretName: c.statusSecret},
			}
			if got := secretNameGuardHolding(ac); got != c.want {
				t.Errorf("secretNameGuardHolding = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// 冲突挂着、而 Certificate 迟迟不 Ready（CertificateRequest 永久失败，Issuing 已回落
// False 所以停滞检测也不触发）时，步骤 4 按设计返回空 Result 等 Owns watch。可是这条路上
// 唤醒源根本不存在：占用者是一个 Secret，而 Secret 刻意不进 cache 也不 watch。删掉占用者
// 也不会自愈，直到有别的什么事件碰它——而 README 与 spec 都承诺「每个 resync 周期自动重试」。
func TestReconcile_ConflictWithUnreadyCertificateStillRequeues(t *testing.T) {
	ctx := context.Background()
	const ns = "i4-ns"

	sch := factoryScheme(t)
	if err := cmapi.AddToScheme(sch); err != nil {
		t.Fatalf("cmapi.AddToScheme: %v", err)
	}

	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1", Finalizers: []string{certsv1alpha1.FinalizerName}},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			SecretName: "victim-tls",
			CertificateTemplate: certsv1alpha1.CertificateTemplate{
				DNSNames:  []string{"api.example.com"},
				IssuerRef: &cmmeta.IssuerReference{Name: "letsencrypt-prod", Kind: "ClusterIssuer"},
			},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun"},
				Region:         "cn-hangzhou",
			},
		},
		// 在役的是 c1-tls，用户想要的是 victim-tls：护栏正拦着。
		Status: certsv1alpha1.AliyunCertificateStatus{SecretName: "c1-tls"},
	}
	// Certificate 存在但没有任何 condition：既不 Ready 也不 Issuing，停滞检测不会触发。
	cert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1"},
		Spec:       cmapi.CertificateSpec{SecretName: "c1-tls", DNSNames: []string{"api.example.com"}},
	}
	victim := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "victim-tls",
		Annotations: map[string]string{certManagerCertificateNameAnnotation: "someone-else"},
	}}

	c := crfake.NewClientBuilder().WithScheme(sch).WithObjects(ac, cert, victim).
		WithStatusSubresource(&certsv1alpha1.AliyunCertificate{}).Build()

	r := &AliyunCertificateReconciler{
		Client: c, APIReader: c, Scheme: sch,
		Recorder:       record.NewFakeRecorder(8),
		ResyncInterval: 42 * time.Minute,
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "c1"}})
	if err != nil {
		t.Fatalf("这一轮不该报错: %v", err)
	}
	if res.RequeueAfter != r.resyncInterval() {
		t.Errorf("冲突挂着时必须按 resync 周期重排，得到 RequeueAfter=%v", res.RequeueAfter)
	}

	got := &certsv1alpha1.AliyunCertificate{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c1"}, got); err != nil {
		t.Fatal(err)
	}
	if r := condReason(got, certsv1alpha1.ConditionReady); r != certsv1alpha1.ReasonSecretNameConflict {
		t.Errorf("Ready 的 reason = %q，期望 SecretNameConflict", r)
	}
	// Certificate 的 secretName 一个字都不许动。
	gotCert := &cmapi.Certificate{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c1"}, gotCert); err != nil {
		t.Fatal(err)
	}
	if gotCert.Spec.SecretName != "c1-tls" {
		t.Errorf("Certificate 的 secretName 被改成了 %q", gotCert.Spec.SecretName)
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
		// 这里刻意不动 reconciler 的周期：改 spec 本身就会触发一轮 reconcile，冲突在那一轮
		// 里就判出来了，不需要等 requeue。
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

	// 被护栏拦住不等于停止维护。冲突期间在役的仍是一张有效、正在服役的证书，cert-manager
	// 照样会给它续期——早退会让这些新代次永远走不到 CAS，服役证书静默过期。理由与
	// aliyuncertificate_controller.go 里「签发停滞绝不结束本轮」那两段注释逐字相同。
	It("冲突期间继续维护在役 Secret：续期照常上传，改回原名后 Ready 恢复", func() {
		ns := newNamespace(ctx)

		victim := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "victim2-tls", Namespace: ns,
				Annotations: map[string]string{certManagerCertificateNameAnnotation: "someone-else"}},
			Data: map[string][]byte{
				corev1.TLSCertKey:       []byte("victim-cert-placeholder"),
				corev1.TLSPrivateKeyKey: []byte("victim-key-placeholder"),
			},
		}
		Expect(k8sClient.Create(ctx, victim)).To(Succeed())

		ac := baseAC(ns, "keep")
		ac.Spec.SecretName = "keep-tls"
		issueReady(ns, "keep", ac)
		gen1 := getAC(ctx, ns, "keep").Status.Current
		Expect(gen1).NotTo(BeNil())
		Expect(gen1.CertID).NotTo(BeNil())

		// 改指到别人的 Secret：护栏拦住，Certificate 上仍是 keep-tls。
		setSecretName(ns, "keep", "victim2-tls")
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "keep"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonSecretNameConflict
		})
		cert, err := getCert(ctx, ns, "keep")
		Expect(err).NotTo(HaveOccurred())
		Expect(cert.Spec.SecretName).To(Equal("keep-tls"))

		// cert-manager 给**在役** Secret 续期。被拦住期间这一代必须照常走完探测与上传。
		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "keep", 2, crt2, key2)

		eventually(func() bool {
			cur := getAC(ctx, ns, "keep").Status.Current
			return cur != nil && cur.Fingerprint != gen1.Fingerprint && cur.CertID != nil
		})
		gen2 := getAC(ctx, ns, "keep").Status.Current
		Expect(currentCAS().Has(*gen2.CertID)).To(BeTrue(), "续期出的新代次必须真的传上 CAS")
		// 维护照跑，但用户要的状态并没有达成：Ready 必须仍然停在 SecretNameConflict。
		Expect(condReason(getAC(ctx, ns, "keep"), certsv1alpha1.ConditionReady)).
			To(Equal(certsv1alpha1.ReasonSecretNameConflict))
		Expect(condTrue(getAC(ctx, ns, "keep"), certsv1alpha1.ConditionIssued)).
			To(BeTrue(), "在役证书本身是好的，Issued 应照常为 True")

		// 改回原名，冲突解除，Ready 恢复。
		setSecretName(ns, "keep", "keep-tls")
		eventually(func() bool { return condTrue(getAC(ctx, ns, "keep"), certsv1alpha1.ConditionReady) })
	})

	// 集群里先躺着一个与 CR 同名的 Certificate（人工建的，或上一次删 CR 时残留），而新建的
	// CR 的 spec.secretName 指着别人的 Secret。护栏从第一轮起就在拦，于是步骤 3 一次都没跑过。
	// Ready 的否决判据若依赖「步骤 3 曾经写过 status.secretName」，这条路上它永远为空，
	// Ready 会被聚合成 True——用户的变更其实被拒绝了，对外却说一切正常。
	It("Certificate 预先存在且 spec 指向他人 Secret 时也必须 Ready=False/SecretNameConflict", func() {
		ns := newNamespace(ctx)

		victim := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "victim3-tls", Namespace: ns,
				Annotations: map[string]string{certManagerCertificateNameAnnotation: "someone-else"}},
			Data: map[string][]byte{
				corev1.TLSCertKey:       []byte("victim-cert-placeholder"),
				corev1.TLSPrivateKeyKey: []byte("victim-key-placeholder"),
			},
		}
		Expect(k8sClient.Create(ctx, victim)).To(Succeed())

		// 残留的 Certificate：名字与 CR 相同，secretName 指着它自己的那个在役 Secret。
		stale := &cmapi.Certificate{
			ObjectMeta: metav1.ObjectMeta{Name: "pre", Namespace: ns},
			Spec: cmapi.CertificateSpec{
				SecretName: "pre-tls",
				DNSNames:   []string{"api.example.com"},
				IssuerRef:  cmmeta.IssuerReference{Name: "letsencrypt-prod", Kind: "ClusterIssuer", Group: "cert-manager.io"},
			},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())

		// 在役 Secret 里放一张真证书，好让步骤 5–10 全都走得通——否则 Issued 会因为
		// SecretNotFound 而为 False，Ready 就算不被否决也是 False，用例证明不了什么。
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		writeTLSSecret(ctx, ns, "pre", crt, key)
		setCertificateStatus(ctx, ns, "pre", 1, cmmeta.ConditionFalse)

		ac := baseAC(ns, "pre")
		ac.Spec.SecretName = "victim3-tls"
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())

		// 在役证书照常被维护到底（Issued=True 且已上传），但 Ready 必须诚实。
		eventually(func() bool {
			a := getAC(ctx, ns, "pre")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		Expect(condTrue(getAC(ctx, ns, "pre"), certsv1alpha1.ConditionIssued)).To(BeTrue())
		Expect(condReason(getAC(ctx, ns, "pre"), certsv1alpha1.ConditionReady)).
			To(Equal(certsv1alpha1.ReasonSecretNameConflict))
		// Certificate 上的 secretName 一个字都不许动。
		got, err := getCert(ctx, ns, "pre")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Spec.SecretName).To(Equal("pre-tls"))
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
		// Equal 而不是 >=：label 是 namespace，而每个用例都用一个新建的 namespace，所以
		// 这个 counter 在本用例里只可能被本用例涨。收紧到恰好 1 才能覆盖单测覆盖不到的
		// 那条重放路径（对象已真正删除，cache 里带 finalizer 的旧版本又唤起一轮）。
		Expect(promtestutil.ToFloat64(secretDeletionSkippedTotal.WithLabelValues(ns))).
			To(Equal(float64(1)))
	})
})
