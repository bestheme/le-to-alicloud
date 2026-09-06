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
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func bindingGone(ctx context.Context, ns, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &certsv1alpha1.AliyunCertificateBinding{})
	return apierrors.IsNotFound(err)
}

// switchToUnbind 把 deletionPolicy 改成 Unbind，并等到**这个 Binding 彻底安静下来**。
//
// 只等 spec 可见会留下一个真实的竞态：Update 会立刻唤醒一轮 reconcile，而用例紧接着
// 就要去改云侧状态（「目标上是别人的证书」那一例把域名上的证书换成别人的）。那一轮
// reconcile 若恰好在改完之后才读到域名，就会判成漂移并把我们自己的证书重新写回去——
// 于是删除时 Observe 看到的指纹又等于 appliedFingerprint，本该「一动不动」的断言会
// 随机翻车。
//
// 等 observedGeneration 追上这一代还不够：追平这件事本身是靠一次 status patch 完成的，
// 而那次 patch 会触发本对象的 watch，**再入队一轮 reconcile**。所以还要等那一轮也跑完。
// 判据用「这个域名上的 Get 次数连续两次采样不变」——按域名分账，天然只统计本 Binding
// 的云调用；两次 200ms 采样相等就说明没有 reconcile 在飞。安静之后下一次唤醒要等
// DriftCheckInterval（套件里 1h），窗口才真正关上，用例这才可以放心改云侧状态、
// 并把此后的云调用计数全部归因于删除。
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
	quiesceTarget(b.Spec.Target.FC3CustomDomain.DomainName)
}

// quiesceTarget 等到某个域名上的 Get 调用停止增长，用来取一个可信的计数基线。
func quiesceTarget(domain string) {
	last := -1
	EventuallyWithOffset(1, func() bool {
		cur := currentFC3().GetCallsFor(domain)
		stable := cur == last
		last = cur
		return stable
	}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
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
		writes := currentFC3().UpdateCallsFor(domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b3"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b3") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).To(Equal("someone-elses"))
		// 光断言「证书没变」是不够的：一个什么都不做的删除分支（Task 7 的存根）也能过。
		// 下面两条才真正把「Observe 之后主动放弃解绑」与「压根没走 Unbind」区分开。
		//
		// 写次数用**等号**：一次都不许写，这就是「一动不动」的全部含义，且不受下面那个
		// 时序影响。
		Expect(currentFC3().UpdateCallsFor(domain)).To(Equal(writes), "解绑别人的证书是越权")
		// 读次数用**等号**：一次删除只该 Observe 一次。基线取自 switchToUnbind 之后的
		// 静默点，此后唯一能推动它的就是删除。
		//
		// 这条断言从前只能写成「至少一次」：删除分支读的是 informer cache，摘掉 finalizer
		// 之后缓存里那份带 finalizer 的旧版本还会再唤起一轮删除分支（日志里能看到第二条
		// 「跳过解绑」出现在「binding deleted」之后），实测每次删除跑 1–2 轮 unbindTarget。
		// Reconcile 改用 APIReader 直读之后那一轮在开头就 NotFound 早退，次数因此钉得死。
		Expect(currentFC3().GetCallsFor(domain)).To(Equal(reads+1),
			"Unbind 必须先 Observe 才能判断这张证书是不是自己写的，而且只该 Observe 一次")
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
		// 同样用等号，理由见上一个用例（Reconcile 直读，删除只跑一轮）。
		Expect(currentFC3().GetCallsFor(domain)).To(Equal(reads+1),
			"Unbind 必须先 Observe；域名不存在才是这一路径要吸收的那个错误")
	})

	It("Unbind：目标换了账号时一动不动，哪怕指纹相同", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b6.%s.example.com", ns)
		issueAndBind(ctx, ns, "c6", "b6", domain, "HTTPS")
		switchToUnbind(ctx, ns, "b6")
		Expect(getBinding(ctx, ns, "b6").Status.BoundAccountID).To(Equal(testAccountID))

		// 迁移期：凭证被指向另一个账号，那边恰好有同名域名、装着**同一张**证书——
		// 指纹是叶子证书的哈希，因此两边一模一样，只比指纹是拦不住的。云侧状态一个
		// 字节都不改，只把账号换掉，正是为了让指纹保持相等。
		currentFC3().SetAccountID("9999999999")

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b6"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b6") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).NotTo(BeEmpty(), "不该动一个从来不属于本 Binding 的账号里的域名")
		Expect(after.Protocol).To(Equal("HTTPS"), "更不该把陌生账号的生产 HTTPS 降成 HTTP")
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
		// 同时等 CleanupFailed 落到 Ready 上：卡在 Terminating 的对象必须在集群里留下
		// 痕迹，否则 kubectl describe 看到的还是删除前的状态，运维一条线索都没有。
		// 两件事一起等而不是分两次断言——cleanupStartedAt 在本轮的 unbindTarget 之前
		// 就落盘了，condition 要等这一轮失败之后才写，分开断言会踩中间态。
		eventually(func() bool {
			cur := &certsv1alpha1.AliyunCertificateBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b5"}, cur); err != nil {
				return false
			}
			ready := meta.FindStatusCondition(cur.Status.Conditions, certsv1alpha1.ConditionReady)
			return cur.Status.CleanupStartedAt != nil && ready != nil &&
				ready.Status == metav1.ConditionFalse && ready.Reason == certsv1alpha1.ReasonCleanupFailed
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

// notFoundOnUpdate 造一个「读得到、写不进去」的 client：Update 一律以 NotFound 失败。
//
// 它复现的是「摘 finalizer 时对象已经不在了」这个时序。从前它每次删除都会发生：删除
// 分支读的是 informer cache，对象真删之后缓存里那份带 finalizer 的旧版本还会再唤起
// 一轮，那一轮的 Update 必然打在空处。Reconcile 改用 APIReader 直读之后，剩下的是
// 本轮进行中被另一条路径删掉的并发窗口——窄了很多，但吸收 NotFound 的理由没变。
func notFoundOnUpdate(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(factoryScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&certsv1alpha1.AliyunCertificateBinding{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object,
				_ ...client.UpdateOption) error {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: certsv1alpha1.GroupVersion.Group, Resource: "aliyuncertificatebindings"},
					obj.GetName())
			},
		}).Build()
}

// 摘 finalizer 时的 NotFound 必须被吸收掉。抛上去的代价是**每一次 Binding 删除**
// （Orphan 也不例外）都推高一次 controller_runtime_reconcile_errors_total 并打一条
// reconciler error 日志——而那个指标正是运维配告警的地方。
func TestFinishBindingDeletion_IgnoresNotFound(t *testing.T) {
	b := bindingWithDomain("api.example.com")
	b.Namespace, b.Name = "ns1", "b1"
	b.Finalizers = []string{certsv1alpha1.FinalizerName}

	c := notFoundOnUpdate(t, b.DeepCopy())
	r := &AliyunCertificateBindingReconciler{Client: c, APIReader: c}
	res, err := r.finishBindingDeletion(context.Background(), newBindingRound(b))
	if err != nil {
		t.Fatalf("对象已经不在了，摘 finalizer 的目的已经达到，不该报错: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("删除收尾不该要求重排: %+v", res)
	}
}

// 反方向：真正的写入失败（不是 NotFound）仍必须抛上去，否则 IgnoreNotFound 就成了
// 「吞掉一切」，一个摘不掉的 finalizer 会安静地把对象永远钉在 Terminating 上。
func TestFinishBindingDeletion_PropagatesOtherErrors(t *testing.T) {
	b := bindingWithDomain("api.example.com")
	b.Namespace, b.Name = "ns1", "b1"
	b.Finalizers = []string{certsv1alpha1.FinalizerName}

	boom := errors.New("etcd unavailable")
	c := fake.NewClientBuilder().WithScheme(factoryScheme(t)).WithObjects(b.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return boom
			},
		}).Build()

	r := &AliyunCertificateBindingReconciler{Client: c, APIReader: c}
	if _, err := r.finishBindingDeletion(context.Background(), newBindingRound(b)); !errors.Is(err, boom) {
		t.Fatalf("非 NotFound 的写入失败必须抛上去: %v", err)
	}
}

// TestShouldAbandonCleanup 把「还要不要继续重试解绑」的决定表死。
//
// 不起 envtest、不动任何全局状态：绑定侧的 CleanupFailurePolicy 挂在常驻 reconciler 上，
// 在 ginkgo 用例里翻转它会波及并发跑着的其他用例，所以 Block 这一半拿不到 envtest
// 覆盖。而它守的正是有界清理的反面——Block 下永不放弃，对象就该一直留在 Terminating
// 里等人处理，这一点不该只靠「与证书侧同构」来担保。
func TestShouldAbandonCleanup(t *testing.T) {
	const grace = 15 * time.Minute
	cases := []struct {
		name    string
		policy  string
		elapsed time.Duration
		want    bool
	}{
		{"Abandon：宽限期内不放弃", CleanupPolicyAbandon, grace - time.Second, false},
		{"Abandon：正好到点就放弃", CleanupPolicyAbandon, grace, true},
		{"Abandon：超出宽限期放弃", CleanupPolicyAbandon, grace + time.Hour, true},
		{"Block：超出再多也不放弃", CleanupPolicyBlock, 100 * grace, false},
		{"Block：宽限期内同样不放弃", CleanupPolicyBlock, 0, false},
		// flag 层已把取值限死成 Abandon | Block，空串只可能来自没接线的测试构造；
		// 与证书侧逐字一致地按 Abandon 处理——「默认不把对象钉死」是更安全的那一侧。
		{"未接线的空策略按 Abandon 处理", "", grace + time.Second, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAbandonCleanup(c.policy, c.elapsed, grace); got != c.want {
				t.Errorf("shouldAbandonCleanup(%q, %v, %v) = %v, want %v",
					c.policy, c.elapsed, grace, got, c.want)
			}
		})
	}
}
