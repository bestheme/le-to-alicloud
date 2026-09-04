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
	"sync"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
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

var _ = Describe("证书 controller：删除", func() {
	ctx := context.Background()
	var ca *testutil.CA

	// 假时钟必须带锁：Abandon 用例要在 reconciler 正忙着重试的时候把时钟推过宽限期，
	// 而 reconciler 每一轮都在自己的 goroutine 里通过 r.now() 读它。直接改
	// reconciler.Now 这个字段（或一个裸变量）会被 -race 抓个正着，所以字段只在用例
	// 之间安静的时候写一次，用例内部一律只动锁保护的 clock。
	// 零值表示「跟随真实时间」，与 reconciler.Now == nil 的语义一致。
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
		reconciler.Now = fakeNow
		DeferCleanup(func() { reconciler.Now = nil })
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

	It("有活着的 Binding 引用时阻塞，Binding 删除后继续", func() {
		ns := newNamespace(ctx)
		ac, _ := issuedAC(ns, "blocked")
		b := &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "blocked"},
				Target: certsv1alpha1.BindingTarget{Type: certsv1alpha1.TargetTypeFC3CustomDomain,
					FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: "x.example.com"}},
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
		// 永不结束的重试循环泄漏给后面的用例——它会和 BeforeEach 里写 reconciler.Now
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
