/*
Copyright 2026.

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
	"fmt"
	"sync"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func TestActiveBindingNames(t *testing.T) {
	now := metav1.Now()
	bs := []certsv1alpha1.AliyunCertificateBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "alive"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "dying", DeletionTimestamp: &now}},
	}
	got := activeBindingNames(bs)
	if len(got) != 1 || got[0] != "alive" {
		t.Fatalf("正在删除中的 Binding 不应计入: %v", got)
	}
}

// uploadOnceThenFail 精确制造出 in-flight 窗口：第一次 Upload 照常打到底层 fake（服务端
// 真的落地一张证书）但返回 retryable 错误，模拟「服务端成功、响应丢失」；之后每一次都在
// 到达 fake 之前失败，免得 ClientToken 幂等把 pendingUpload 又自动接上。
//
// 用包装器而不是 fake 的 QueueUploadErr：排队的错误在 fake 里是最先被 pop 的，会抢在
// 写入之前返回，那样服务端上根本不会有证书，也就没有孤儿可回收了。
type uploadOnceThenFail struct {
	aliyun.CASClient
	mu   sync.Mutex
	done bool
}

func (u *uploadOnceThenFail) Upload(ctx context.Context, name string, certPEM, keyPEM []byte, token string) (int64, error) {
	u.mu.Lock()
	first := !u.done
	u.done = true
	u.mu.Unlock()

	lost := &aliyun.Error{Class: aliyun.ClassRetryable, Op: "Upload", Code: "Timeout", Err: errors.New("response lost")}
	if !first {
		return 0, lost
	}
	if _, err := u.CASClient.Upload(ctx, name, certPEM, keyPEM, token); err != nil {
		return 0, err
	}
	return 0, lost
}

// TestCleanupPendingUploadNotOnServer 覆盖「write-ahead 记录存在，但那次上传其实从未在
// 服务端落地」：CAS 上没有同名证书，应当直接把记录抹掉，既不报错也不乱删别人的证书。
func TestCleanupPendingUploadNotOnServer(t *testing.T) {
	ctx := context.Background()
	f := fake.NewCAS()
	// 云上放一张同域名的别的证书，确保「通过」不是因为列表本来就是空的
	other, err := f.Upload(ctx, "api_other_0011223344", nil, nil, "tok-other")
	if err != nil {
		t.Fatal(err)
	}
	r := &AliyunCertificateReconciler{
		CASFactory: func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) { return f, nil },
	}
	ac := &certsv1alpha1.AliyunCertificate{
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: []string{"api.example.com"}},
		},
		Status: certsv1alpha1.AliyunCertificateStatus{
			PendingUpload: &certsv1alpha1.PendingUpload{CASName: "api_deadbeefcafe", ClientToken: "tok"},
		},
	}

	if err := r.cleanupCAS(ctx, ac); err != nil {
		t.Fatalf("列表里没有同名证书说明从未上传成功，不该报错: %v", err)
	}
	if ac.Status.PendingUpload != nil {
		t.Error("pendingUpload 应被清空")
	}
	if f.FindCalls() != 1 {
		t.Errorf("FindCalls = %d, want 1（必须真的去云上认领过一次）", f.FindCalls())
	}
	if f.DeleteCalls() != 0 {
		t.Errorf("DeleteCalls = %d, want 0（不该误删同域名的其它证书）", f.DeleteCalls())
	}
	if !f.Has(other) {
		t.Error("别人的证书不该被删掉")
	}
}

// TestCasDomainHint 固定 hint 的回退顺序：dnsNames[0] → commonName。
func TestCasDomainHint(t *testing.T) {
	tmpl := func(dns []string, cn string) *certsv1alpha1.AliyunCertificate {
		return &certsv1alpha1.AliyunCertificate{Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: dns, CommonName: cn},
		}}
	}
	if got := casDomainHint(tmpl([]string{"a.example.com", "b.example.com"}, "cn.example.com")); got != "a.example.com" {
		t.Errorf("有 dnsNames 时应取第一个，得到 %q", got)
	}
	if got := casDomainHint(tmpl(nil, "cn.example.com")); got != "cn.example.com" {
		t.Errorf("没有 dnsNames 时应回退 commonName，得到 %q", got)
	}
}

var _ = Describe("证书 controller：删除", func() {
	ctx := context.Background()
	var ca *testutil.CA

	// 假时钟必须带锁：Abandon 用例要在 reconciler 正忙着重试的时候把时钟推过宽限期，
	// 而 reconciler 每一轮都在自己的 goroutine 里通过 r.now() 读它。裸变量会被 -race
	// 抓个正着；换钟本身也一样，所以走 SetNow 而不是直接写字段。
	// 零值表示「跟随真实时间」，与 Now == nil 的语义一致。
	var clockMu sync.Mutex
	var clock time.Time
	fakeNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		if clock.IsZero() {
			return time.Now()
		}
		return clock
	}
	freeze := func(t time.Time) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = t
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(d)
	}

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		reconciler.CleanupFailurePolicy = CleanupPolicyAbandon
		reconciler.CleanupGracePeriod = 15 * time.Minute
		freeze(time.Time{})
		reconciler.SetNow(fakeNow)
		DeferCleanup(func() { reconciler.SetNow(nil) })
		ca = testutil.NewCA(GinkgoT())
	})

	issuedAC := func(ns, name string) (*certsv1alpha1.AliyunCertificate, int64) {
		Expect(k8sClient.Create(ctx, baseAC(ns, name))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, name, 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, name)
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		ac := getAC(ctx, ns, name)
		return ac, *ac.Status.Current.CertID
	}

	It("正常删除：CAS、Certificate、Secret 全部清理，finalizer 摘除", func() {
		ns := newNamespace(ctx)
		ac, certID := issuedAC(ns, "del")
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(currentCAS().Has(certID)).To(BeFalse())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del"}, &cmapi.Certificate{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del-tls"}, &corev1.Secret{}))).To(BeTrue())
	})

	// in-flight 的 pendingUpload：证书已经在服务端落地，响应却丢了，status 上只有
	// write-ahead 记录、没有 certId。删除路径不会再走 upload.go 的解析，必须自己按名字
	// 去云上认领，否则这张证书会被永久且无痕地孤儿化。
	It("pendingUpload 在删除时被清理", func() {
		ns := newNamespace(ctx)
		// CR 名字决定 CAS 名（naming.CASName 用的是 CR 名，不是域名），而 fake 的
		// FindUploaded 只能拿名字近似 Keyword 匹配（它不解析 PEM）。取 "api" 才能让
		// fake 表现得和真实 CAS 一样：那张证书的 SAN 确实是 api.example.com。
		Expect(k8sClient.Create(ctx, baseAC(ns, "api"))).To(Succeed())

		wrapped := &uploadOnceThenFail{CASClient: currentCAS()}
		prev := reconciler.CASFactory
		reconciler.CASFactory = func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
			return wrapped, nil
		}
		DeferCleanup(func() { reconciler.CASFactory = prev })

		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "api", 1, crt, key)

		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.PendingUpload != nil && a.Status.Current == nil
		})
		Expect(currentCAS().Certs()).To(HaveLen(1), "服务端应已落地一张无人认领的证书")
		orphan := currentCAS().Certs()[0].ID
		Expect(getAC(ctx, ns, "api").Status.PendingUpload.CASName).To(Equal(currentCAS().Certs()[0].Name))

		Expect(k8sClient.Delete(ctx, getAC(ctx, ns, "api"))).To(Succeed())

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(currentCAS().Has(orphan)).To(BeFalse(), "in-flight 的那张证书必须被回收，不能静默孤儿化")
		Expect(currentCAS().FindCalls()).To(BeNumerically(">=", 1), "回收它只能靠按名字去云上认领")
	})

	It("有活着的 Binding 引用时阻塞，Binding 删除后继续", func() {
		ns := newNamespace(ctx)
		ac, _ := issuedAC(ns, "blocked")
		b := &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "blocked"},
				Target: certsv1alpha1.BindingTarget{Type: certsv1alpha1.TargetTypeFC3CustomDomain,
					// 域名带 namespace：TargetKey() 不含 namespace，撞名会被同目标仲裁判成 Conflict。
					FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
						Region: "cn-hangzhou", DomainName: fmt.Sprintf("b.%s.example.com", ns)}},
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		eventually(func() bool {
			got := &certsv1alpha1.AliyunCertificate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, got); err != nil {
				return false
			}
			return condReason(got, certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonDeletionBlocked
		})
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "1500ms", "200ms").Should(BeTrue())

		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
	})

	It("CAS 持续失败：Abandon 策略在 grace period 后放弃并摘 finalizer", func() {
		ns := newNamespace(ctx)
		ac, certID := issuedAC(ns, "abandon")
		// 之后所有 Delete 都失败
		for i := 0; i < 50; i++ {
			currentCAS().QueueDeleteErr(&aliyun.Error{Class: aliyun.ClassAuth, Op: "Delete", Code: "Forbidden.RAM", Err: errors.New("denied")})
		}
		freeze(time.Now())
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		// grace period 内不摘 finalizer
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "1500ms", "200ms").Should(BeTrue())

		// 时钟越过 grace period
		advance(16 * time.Minute)
		touchAC(ctx, ns, "abandon") // 立刻唤醒，不用等退避走完

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(currentCAS().Has(certID)).To(BeTrue(), "Abandon 后 CAS 证书应仍然存在（孤儿）")
		// Certificate 与 Secret 仍应被清理
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon-tls"}, &corev1.Secret{}))).To(BeTrue())
		// 放弃必须留下痕迹，否则孤儿证书就无声无息了
		Expect(acEventMessage(ctx, ns, "abandon", certsv1alpha1.ReasonCleanupAbandoned)).To(Equal(cleanupAbandonedMessage))
		// 事件会随 namespace 一起过期，指标才是能长期告警的那一份痕迹
		Expect(promtestutil.ToFloat64(cleanupAbandonedTotal.WithLabelValues("cn-hangzhou", aliyun.ClassAuth.String()))).
			To(BeNumerically(">=", 1))
	})

	It("Block 策略下持续失败不摘 finalizer", func() {
		reconciler.CleanupFailurePolicy = CleanupPolicyBlock
		reconciler.CleanupGracePeriod = time.Millisecond
		ns := newNamespace(ctx)
		ac, _ := issuedAC(ns, "block")
		for i := 0; i < 50; i++ {
			currentCAS().QueueDeleteErr(&aliyun.Error{Class: aliyun.ClassAuth, Op: "Delete", Code: "Forbidden.RAM", Err: errors.New("denied")})
		}
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())
		// 宽限期只有 1ms，真实时钟一轮重试就走过去了；还留着 finalizer 只可能是
		// Block 策略拦下来的。
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "block"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "2s", "200ms").Should(BeTrue())

		// CAS 恢复后必须能自愈：Block 是「一直重试」，不是「永远卡死」。顺带避免把一个
		// 永不结束的重试循环泄漏给后面的用例——它会和 BeforeEach 里换钟
		// 撞成 data race。
		resetCAS()
		touchAC(ctx, ns, "block") // watch 事件直接入队，绕开已经涨到几秒的退避
		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "block"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
	})
})

// acEventMessage 返回 ns 下打在 name 上、reason 为指定值的第一条事件的 message；没有则返回 ""。
func acEventMessage(ctx context.Context, ns, name, reason string) string {
	list := &corev1.EventList{}
	ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
	for i := range list.Items {
		if list.Items[i].InvolvedObject.Name == name && list.Items[i].Reason == reason {
			return list.Items[i].Message
		}
	}
	return ""
}

// touchAC 改一个注解来强制立刻触发一次 reconcile。必须重读重试：reconciler 每轮都会
// patch status，而 status 子资源同样会顶掉 metadata.resourceVersion，读与写之间撞上
// 一次就是 Conflict。
func touchAC(ctx context.Context, ns, name string) {
	EventuallyWithOffset(1, func() error {
		ac := &certsv1alpha1.AliyunCertificate{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ac); err != nil {
			return err
		}
		if ac.Annotations == nil {
			ac.Annotations = map[string]string{}
		}
		ac.Annotations["touch"] = "1"
		return k8sClient.Update(ctx, ac)
	}, "10s", "100ms").Should(Succeed())
}
