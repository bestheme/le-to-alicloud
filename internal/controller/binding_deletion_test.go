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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func bindingGone(ctx context.Context, ns, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &certsv1alpha1.AliyunCertificateBinding{})
	return apierrors.IsNotFound(err)
}

// switchToUnbind 把 deletionPolicy 改成 Unbind，并等到**这一代已经被 reconcile 过**。
//
// 只等 spec 可见是不够的，会留下一个真实的竞态：Update 会立刻唤醒一轮 reconcile，而
// 用例紧接着就要去改云侧状态（「目标上是别人的证书」那一例把域名上的证书换成别人的）。
// 那一轮 reconcile 若恰好在改完之后才读到域名，就会判成漂移并把我们自己的证书重新
// 写回去——于是删除时 Observe 看到的指纹又等于 appliedFingerprint，本该「一动不动」的
// 断言会随机翻车。等 observedGeneration 追上这一代，就等于等那一轮跑完；此后下一次
// 唤醒要等 DriftCheckInterval（套件里是 1h），窗口关上了。
func switchToUnbind(ctx context.Context, ns, name string) {
	b := getBinding(ctx, ns, name)
	b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
	ExpectWithOffset(1, k8sClient.Update(ctx, b)).To(Succeed())
	gen := b.Generation
	EventuallyWithOffset(1, func() bool {
		cur := &certsv1alpha1.AliyunCertificateBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cur); err != nil {
			return false
		}
		return cur.Spec.DeletionPolicy == certsv1alpha1.DeletionPolicyUnbind &&
			cur.Status.ObservedGeneration >= gen
	}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
}

var _ = Describe("绑定 controller：删除", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("Orphan（默认）：摘 finalizer，云侧一动不动", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTPS")
		before, _ := currentFC3().Domain(domain)
		// 按域名分账而不是全局计数：envtest 从不回收 namespace，先前每个用例建的
		// Binding 都还活着，任何一个被唤醒都会推动全局计数器（见 fake.FC3 的说明）。
		writes := currentFC3().UpdateCallsFor(domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b1") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).To(Equal(before.CertName))
		Expect(currentFC3().UpdateCallsFor(domain)).To(Equal(writes), "一个 kubectl delete 不该打穿生产 HTTPS")
	})

	It("Unbind：清空自己的证书，纯 HTTPS 域名降为 HTTP", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b2.%s.example.com", ns)
		issueAndBind(ctx, ns, "c2", "b2", domain, "HTTPS")
		switchToUnbind(ctx, ns, "b2")

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b2"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b2") })

		d, _ := currentFC3().Domain(domain)
		Expect(d.CertName).To(BeEmpty())
		Expect(d.Protocol).To(Equal("HTTP"), "纯 HTTPS 域名拿掉证书后必须降为 HTTP，否则彻底不可用")
		Expect(d.Echo).To(Equal("routes"))
	})

	It("Unbind：目标上是别人的证书时一动不动", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b3.%s.example.com", ns)
		issueAndBind(ctx, ns, "c3", "b3", domain, "HTTP,HTTPS")
		switchToUnbind(ctx, ns, "b3")

		// 有人在我们之后把证书换成了别的。解绑只该解自己那一张。
		otherCA := testutil.NewCA(GinkgoT())
		otherPEM, _ := testutil.IssueLeaf(GinkgoT(), otherCA, domain)
		d, _ := currentFC3().Domain(domain)
		d.CertName = "someone-elses"
		d.CertPEM = otherPEM
		currentFC3().AddDomain(d)
		reads := currentFC3().GetCallsFor(domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b3"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b3") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).To(Equal("someone-elses"))
		// 光断言「证书没变」是不够的：一个什么都不做的删除分支（Task 7 的存根）也能过。
		// 追加一条「确实去读了目标」，本用例才真正把「Observe 之后主动放弃解绑」与
		// 「压根没走 Unbind」区分开。
		Expect(currentFC3().GetCallsFor(domain)).To(BeNumerically(">", reads),
			"Unbind 必须先 Observe 才能判断这张证书是不是自己写的")
	})

	It("Unbind：目标已经不存在时直接摘 finalizer", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b4.%s.example.com", ns)
		issueAndBind(ctx, ns, "c4", "b4", domain, "HTTPS")
		switchToUnbind(ctx, ns, "b4")

		currentFC3().RemoveDomain(domain)
		reads := currentFC3().GetCallsFor(domain)
		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b4"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b4") })
		// 同上：finalizer 被摘掉这一点，一个什么都不做的存根也能做到。真正要立的是
		// 「走了 Unbind、Observe 撞上 TargetNotFound、把它当成功收场」这条路径。
		Expect(currentFC3().GetCallsFor(domain)).To(BeNumerically(">", reads),
			"Unbind 必须先 Observe；域名不存在才是这一路径要吸收的那个错误")
	})

	It("Unbind：清理持续失败，超过宽限期后 Abandon", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b5.%s.example.com", ns)
		issueAndBind(ctx, ns, "c5", "b5", domain, "HTTPS")
		switchToUnbind(ctx, ns, "b5")

		// 假时钟：与证书 controller 的删除用例同一套手法（deletion_test.go）。那边的
		// mu/fakeNow 是各自 Describe 里的局部闭包，不是包级 helper，这里只能自己再搭一套。
		var mu sync.Mutex
		fakeNow := time.Now()
		bindingReconciler.SetNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return fakeNow })
		DeferCleanup(func() { bindingReconciler.SetNow(nil) })

		for i := 0; i < 50; i++ {
			currentFC3().QueueGetErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
				Code: "InternalError", Err: errors.New("boom"),
			})
		}
		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b5"))).To(Succeed())
		// 这里必须用不硬失败的读法：getBinding 内部是 ExpectWithOffset(...).To(Succeed())，
		// 对象一旦在轮询窗口里被摘掉 finalizer 删干净，整个用例会以断言失败告终而不是
		// 继续收敛。直接 Get，err != nil 就返回 false 让 eventually 接着轮询。
		eventually(func() bool {
			cur := &certsv1alpha1.AliyunCertificateBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b5"}, cur); err != nil {
				return false
			}
			return cur.Status.CleanupStartedAt != nil
		})

		mu.Lock()
		fakeNow = fakeNow.Add(16 * time.Minute)
		mu.Unlock()

		eventually(func() bool { return bindingGone(ctx, ns, "b5") })
		// 证书还留在云上——这正是 Abandon 的含义，有事件与计数器可查。
		d, _ := currentFC3().Domain(domain)
		Expect(d.CertName).NotTo(BeEmpty())
	})
})
