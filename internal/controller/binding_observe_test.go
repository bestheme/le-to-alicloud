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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// issueAndBind 建证书 + 域名 + Binding，并等到 Applied=True。后续用例的公共前置。
//
// domain 由调用方给，一律写成 fmt.Sprintf("%s.%s.example.com", bindingName, ns)：
// 仲裁是跨 namespace 的，域名撞车会让后建的 Binding 一直停在 Conflict（见 Task 7 Step 8）。
// Task 13 的删除用例也用这个 helper（跨文件依赖，同包）。
func issueAndBind(ctx context.Context, ns, certName, bindingName, domain, protocol string) {
	ca := testutil.NewCA(GinkgoT())
	certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
	currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: protocol, Echo: "routes"})
	createCertificate(ctx, ns, certName, domain)
	simulateIssuance(ctx, ns, certName, 1, certPEM, keyPEM)
	createBinding(ctx, ns, bindingName, certName, domain, nil)
	eventually(func() bool {
		return bindingCond(ctx, ns, bindingName, certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
	})
}

// bindingEventCount 统计某个对象上某个 reason 的事件**总次数**。
//
// 只数 Event 对象个数是不够的：K8s 的事件聚合会把 reason+message 完全相同的事件并进
// 同一个对象并累加 Count，而「又发了一条」正表现为 Count 从 1 变成 2。所以这里累加
// Count 而不是 len(items)——否则一场刷屏在断言里看着仍然只有「一条事件」。
func bindingEventCount(ctx context.Context, ns, name, reason string) int {
	list := &corev1.EventList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		return -1
	}
	total := 0
	for i := range list.Items {
		e := &list.Items[i]
		if e.InvolvedObject.Name != name || e.Reason != reason {
			continue
		}
		if e.Count <= 0 {
			total++ // 刚创建、还没被聚合器回填 Count
			continue
		}
		total += int(e.Count)
	}
	return total
}

// 每个用例用自己的一组对象名（b1/c1 … b12/c12）而不是全体叫 b1/c1。两个理由：
// ① 失败信息里一眼看得出是哪个用例的对象；② issueAndBind 的 certName / bindingName /
// domain / protocol 因此真的各取各的值，unparam 的「always receives」不再成立。
// unparam 报得对，而正确的修法是让 fixture 名字真的不同：它一次只报一个形参，逐个删
// 下去会把这个 helper 削成 issueAndBind(ctx, ns)——而「每个用例显式写出自己的
// namespace 化域名」正是 createBinding 不给默认域名要立的规矩。挂 //nolint 同样不对：
// 没有任何一个已知调用点会让它们变化，那会是一条永远等不到调用点的死抑制。
var _ = Describe("绑定 controller：Observe", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("域名不存在时 Ready=False/TargetNotFound", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		// 故意不 AddDomain：目标域名在云上还不存在。
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonTargetNotFound
		})
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
	})

	// 本用例要先有一次成功的 Apply（issueAndBind 等的是 Applied=True）。Task 11 交付时
	// Apply 还不存在，所以它连同下面三个用例一起是待定状态；Task 12 补上写入之后转正。
	It("指纹一致时短路：不写云，但仍然 Observe", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b2.%s.example.com", ns)
		issueAndBind(ctx, ns, "c2", "b2", domain, "HTTP")

		writes := currentFC3().UpdateCallsFor(domain)
		getsBefore := currentFC3().GetCallsFor(domain)

		// 推一次 reconcile（spec.target 不可变，改 deletionPolicy 推进 generation）。
		b := getBinding(ctx, ns, "b2")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyOrphan
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool { return currentFC3().GetCallsFor(domain) > getsBefore })
		// 短路只跳过写，不跳过读（spec §3「level-triggered」）。
		Expect(currentFC3().UpdateCallsFor(domain)).To(Equal(writes))
		Expect(getBinding(ctx, ns, "b2").Status.LastObservedTime).NotTo(BeNil())
	})

	// 同上：Task 12 补上 Apply 之后解除。
	It("账号变了就 fencing：Conflict=True/AccountMismatch 且不写", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b3.%s.example.com", ns)
		issueAndBind(ctx, ns, "c3", "b3", domain, "HTTPS")
		Expect(getBinding(ctx, ns, "b3").Status.BoundAccountID).To(Equal(testAccountID))

		writes := currentFC3().UpdateCallsFor(domain)
		// 同一个域名在另一个账号下：AK 被换成了别人的，再写就是在写别人的资源。
		currentFC3().SetAccountID("9999999999")
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTPS"})

		b := getBinding(ctx, ns, "b3")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b3", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonAccountMismatch
		})
		Expect(bindingCond(ctx, ns, "b3", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(currentFC3().UpdateCallsFor(domain)).To(Equal(writes))
	})

	// 同上：Task 12 补上 Apply 之后解除。
	It("云侧被人换了证书时判定为 drift 并纠正", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b4.%s.example.com", ns)
		// 真实的漂移场景发生在一个已经在跑 HTTPS 的域名上；顺带钉住 read-modify-write
		// 必须把 protocol 原样带回（fake 的 validateUpdate 只挡空值，挡不住「悄悄改成
		// HTTP」）。
		issueAndBind(ctx, ns, "c4", "b4", domain, "HTTP,HTTPS")
		writes := currentFC3().UpdateCallsFor(domain)

		// 有人手工把证书换成了另一张。指纹既不是 appliedFingerprint 也不是 current。
		otherCA := testutil.NewCA(GinkgoT())
		otherPEM, _ := testutil.IssueLeaf(GinkgoT(), otherCA, domain)
		d, _ := currentFC3().Domain(domain)
		d.CertName = "someone-elses"
		d.CertPEM = otherPEM
		currentFC3().AddDomain(d)

		b := getBinding(ctx, ns, "b4")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool { return currentFC3().UpdateCallsFor(domain) > writes })
		eventually(func() bool {
			return bindingCond(ctx, ns, "b4", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		// 必须重新读回来再断言：d 是写入之前抓的本地副本，对它断言恒真，测不到
		// read-modify-write 有没有把 routeConfig 之类的旁路字段原样回填。
		d2, _ := currentFC3().Domain(domain)
		Expect(d2.Echo).To(Equal("routes"))
		Expect(d2.Protocol).To(Equal("HTTP,HTTPS"))
		Expect(d2.CertName).NotTo(Equal("someone-elses"))
	})

	// 同上：Task 12 补上 Apply 之后解除。
	It("Observe 未知失败属于旁路：不降级 Applied", func() {
		ns := newNamespace(ctx)
		issueAndBind(ctx, ns, "c5", "b5", fmt.Sprintf("b5.%s.example.com", ns), "HTTP")

		// 一个说不出「目标有没有问题」的错误。若因此把 Applied 打成 False，
		// Ready 也会掉，运维会以为线上 HTTPS 坏了——而它好好的。
		currentFC3().QueueGetErr(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
			Code: "InternalError", Err: errors.New("boom"),
		})
		b := getBinding(ctx, ns, "b5")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		Consistently(func() metav1.ConditionStatus {
			return bindingCond(ctx, ns, "b5", certsv1alpha1.ConditionApplied).Status
		}, "2s", "200ms").Should(Equal(metav1.ConditionTrue))
	})

	// 上面四个用例全都要先有一次成功的 Apply 才能起步（Task 11 交付时因此是待定的）。
	// 下面三个用例走**接管**这条路进同一批状态：云上本来就装着同一张证书，Observe 一
	// 比对指纹就短路，Applied=True 完全不经过 Apply。它们因此在 Task 11 当天就是活的，
	// 而且顺带覆盖了上面那几个用例覆盖不到的东西——「首次接管」正是 appliedFingerprint
	// / boundAccountId 由空变成有值的那一刻。
	It("目标上已经是同一张证书：短路接管，一个字节都不写", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b6.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		fp, ferr := pki.LeafFingerprint(certPEM)
		Expect(ferr).NotTo(HaveOccurred())
		// 云上早就由 Terraform / 人工装好了同一张证书，operator 现在来接管它。
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTP,HTTPS",
			CertName: "pre-existing", CertPEM: certPEM, KeyPEM: keyPEM, Echo: "routes",
		})
		createCertificate(ctx, ns, "c6", domain)
		simulateIssuance(ctx, ns, "c6", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b6", "c6", domain, nil)

		eventually(func() bool {
			return bindingCond(ctx, ns, "b6", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		b := getBinding(ctx, ns, "b6")
		// 接管必须把这两项记下来：护栏 3（appliedFingerprint == 该代 ⇒ 不回收）与账号
		// fencing 都靠它们，而短路路径上 Apply 永远不会跑，没有别处能补记。
		Expect(b.Status.AppliedFingerprint).To(Equal(fp))
		Expect(b.Status.BoundAccountID).To(Equal(testAccountID))
		Expect(b.Status.LastObservedTime).NotTo(BeNil())
		// 短路只跳过写，不跳过读（spec §3「level-triggered」）。
		Expect(currentFC3().GetCallsFor(domain)).NotTo(BeZero())
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
		// 云上那张证书连名字都不该被换掉。
		d, _ := currentFC3().Domain(domain)
		Expect(d.CertName).To(Equal("pre-existing"))
	})

	It("接管之后账号被换掉：fencing 拦下，Conflict=True/AccountMismatch 且不写", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b7.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTPS",
			CertName: "pre-existing", CertPEM: certPEM, KeyPEM: keyPEM, Echo: "routes",
		})
		createCertificate(ctx, ns, "c7", domain)
		simulateIssuance(ctx, ns, "c7", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b7", "c7", domain, nil)
		eventually(func() bool {
			return getBinding(ctx, ns, "b7").Status.BoundAccountID == testAccountID
		})

		// 凭证 Secret 被换成了另一个账号的 AK，而同名域名恰好也存在于那个账号。
		// 没有这道闸，operator 会安静地把证书写进陌生人的资源。
		currentFC3().SetAccountID("9999999999")
		b := getBinding(ctx, ns, "b7")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b7", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonAccountMismatch
		})
		Expect(bindingCond(ctx, ns, "b7", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())

		// fencing 每一轮都要把 Conflict 从仲裁刚写下的 False 再翻回 True（步骤 2 排在
		// Observe 之前，顺序由 spec §6.2 定死）。这一翻若每轮都盖一个新的
		// lastTransitionTime，① 运维再也读不出「从什么时候起被 fencing 的」，
		// ② 每一次外部唤醒都多出一次非空 status patch 和它引发的一轮 reconcile。
		//
		// 静置断言测不到这一条——被 fencing 的对象一小时才自己醒一次，窗口里根本没有
		// reconcile。必须**主动再唤醒一次**，然后要求跃迁时间纹丝不动。
		fencedAt := bindingCond(ctx, ns, "b7", certsv1alpha1.ConditionConflict).LastTransitionTime
		Expect(fencedAt.IsZero()).To(BeFalse())
		// 必须跨过一个整秒再唤醒：metav1.Time 序列化到**秒**精度，同一秒内重新盖的
		// lastTransitionTime 和原值比起来是相等的，churn 就此隐形——这条断言会变成
		// 一个看着通过、其实什么都没测的空断言（已实测：不睡这一秒，去掉修复它照样绿）。
		time.Sleep(1100 * time.Millisecond)
		gets := currentFC3().GetCallsFor(domain)
		b = getBinding(ctx, ns, "b7")
		b.Annotations = map[string]string{"poke": "2"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool { return currentFC3().GetCallsFor(domain) > gets }) // 确实又跑了一轮

		c := bindingCond(ctx, ns, "b7", certsv1alpha1.ConditionConflict)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(certsv1alpha1.ReasonAccountMismatch))
		Expect(c.LastTransitionTime).To(Equal(fencedAt),
			"fencing 是幂等的：同一个判定重复写不该盖新的 lastTransitionTime")
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
	})

	It("观测到空账号时 fencing 也必须拦下（fail closed）", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b10.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTPS",
			CertName: "pre-existing", CertPEM: certPEM, KeyPEM: keyPEM, Echo: "routes",
		})
		createCertificate(ctx, ns, "c10", domain)
		simulateIssuance(ctx, ns, "c10", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b10", "c10", domain, nil)
		eventually(func() bool {
			return getBinding(ctx, ns, "b10").Status.BoundAccountID == testAccountID
		})

		// AK 被换成了另一个账号的，而那个账号的响应恰好没带账号 ID。放行的话，
		// 下一步的 Apply 就把证书写进了陌生人的域名——这正是这道闸存在的理由。
		// boundAccountId 只会从非空观测里记下来，所以此刻读到空值是异常而非常态。
		currentFC3().SetAccountID("")
		b := getBinding(ctx, ns, "b10")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b10", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonAccountMismatch
		})
		Expect(bindingCond(ctx, ns, "b10", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
	})

	It("接管之后 Observe 未知失败：旁路，不降级 Applied，只把 reason 换成 ObserveFailed", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b8.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTP",
			CertName: "pre-existing", CertPEM: certPEM, KeyPEM: keyPEM, Echo: "routes",
		})
		createCertificate(ctx, ns, "c8", domain)
		simulateIssuance(ctx, ns, "c8", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b8", "c8", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b8", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})

		// 一个说不出「目标有没有问题」的错误。若因此把 Applied 打成 False，
		// Ready 也会掉，运维会以为线上 HTTPS 坏了——而它好好的。
		currentFC3().QueueGetErr(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
			Code: "InternalError", Err: errors.New("boom"),
		})
		b := getBinding(ctx, ns, "b8")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		Consistently(func() metav1.ConditionStatus {
			return bindingCond(ctx, ns, "b8", certsv1alpha1.ConditionApplied).Status
		}, "2s", "200ms").Should(Equal(metav1.ConditionTrue))
		// 而观测恢复之后 reason 必须写回 Applied——否则一次瞬时故障会让一个健康对象
		// 永远显示 ObserveFailed。这是 noteObserveFailed 的反方向，由 setApplied 负责。
		eventually(func() bool {
			return bindingCond(ctx, ns, "b8", certsv1alpha1.ConditionApplied).Reason == certsv1alpha1.ReasonApplied
		})
	})

	It("已 Applied 的对象观测连续失败：reason 变成 ObserveFailed，事件只发一条", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b11.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTP",
			CertName: "pre-existing", CertPEM: certPEM, KeyPEM: keyPEM, Echo: "routes",
		})
		createCertificate(ctx, ns, "c11", domain)
		simulateIssuance(ctx, ns, "c11", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b11", "c11", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b11", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})

		// Retryable：controller-runtime 会以 5ms 起步的退避一轮轮重来，制造出真实的
		// 「旁路失败反复重试」场景。排够多的错误，让它在整个断言窗口里都恢复不了。
		for range 200 {
			currentFC3().QueueGetErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
				Code: "InternalError", Err: errors.New("boom"),
			})
		}
		b := getBinding(ctx, ns, "b11")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		// 痕迹：status 仍是 True（旁路不降级），reason 换成 ObserveFailed。
		// 这条 reason 此前没有任何用例覆盖，而它正是跃迁判断的比较基准。
		eventually(func() bool {
			c := bindingCond(ctx, ns, "b11", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonObserveFailed
		})
		// 事件只在跃迁那一轮发。等到失败轮次远多于事件数，「每轮一条」就无处躲藏。
		eventually(func() bool { return currentFC3().GetCallsFor(domain) >= 8 })
		n := bindingEventCount(ctx, ns, "b11", certsv1alpha1.ReasonObserveFailed)
		Expect(n).To(BeNumerically(">=", 1), "跃迁那一轮必须发一条")
		// 不断言恰好等于 1：跃迁判据取自 informer cache 里的那一份（rd.orig），缓存
		// 滞后时紧邻的一两轮可能仍看着旧 reason，于是多发一条——**有界**的重复。
		// 要钉死的是「有界」：坏掉的实现是每一轮都发，事件数跟着轮次一起涨。
		Expect(n).To(BeNumerically("<=", 3),
			fmt.Sprintf("已失败 %d 轮却发了 %d 条事件——事件数不该跟轮次一起涨",
				currentFC3().GetCallsFor(domain), n))
	})

	It("从没 Applied 过的对象观测反复失败：不留痕迹，也一条事件都不发", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b12.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})
		// 每一轮观测都失败，而且是 Retryable——controller-runtime 会以 5ms、10ms、20ms…
		// 的退避一轮轮重来。这正是事件刷屏的场景：跃迁判断的基准是
		// noteObserveFailed 留下的痕迹，而这种对象上它什么都不写，基准永远是空串。
		for range 40 {
			currentFC3().QueueGetErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
				Code: "InternalError", Err: errors.New("boom"),
			})
		}
		createCertificate(ctx, ns, "c12", domain)
		simulateIssuance(ctx, ns, "c12", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b12", "c12", domain, nil)

		// 先确认重试风暴真的发生了，否则下面的「零事件」是空断言。
		eventually(func() bool { return currentFC3().GetCallsFor(domain) >= 5 })
		// 绝不凭空造 Applied=False。
		Expect(bindingCond(ctx, ns, "b12", certsv1alpha1.ConditionApplied).Status).To(BeEmpty())
		// 「还没绑成功」由 Ready 说就够了，不需要每一轮再发一条 Warning 重复一遍。
		Consistently(func() int {
			return bindingEventCount(ctx, ns, "b12", certsv1alpha1.ReasonObserveFailed)
		}, "2s", "200ms").Should(BeZero(), "没有痕迹可比较，就不该发事件——否则永不收敛")
	})

	It("云上装着别人的证书时记一次 drift", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b9.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		otherCA := testutil.NewCA(GinkgoT())
		otherPEM, otherKey := testutil.IssueLeaf(GinkgoT(), otherCA, domain)
		// 指纹既不是 appliedFingerprint（还是空的）也不是我们要写的那张。
		currentFC3().AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTP",
			CertName: "someone-elses", CertPEM: otherPEM, KeyPEM: otherKey, Echo: "routes",
		})
		before := promtestutil.ToFloat64(bindingDriftTotal.WithLabelValues(certsv1alpha1.TargetTypeFC3CustomDomain))

		createCertificate(ctx, ns, "c9", domain)
		simulateIssuance(ctx, ns, "c9", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b9", "c9", domain, nil)

		// 这里只钉「检测到了」这一半；纠正那一半由上面的 drift 用例断言（它从一个
		// 已经 Applied 的对象出发，能区分「重写了一次」和「本来就没写过」）。
		eventually(func() bool {
			return promtestutil.ToFloat64(
				bindingDriftTotal.WithLabelValues(certsv1alpha1.TargetTypeFC3CustomDomain)) > before
		})
		// 指标是给告警看的，事件是给 kubectl describe 看的，两者都要有。
		eventually(func() bool {
			return bindingEventCount(ctx, ns, "b9", certsv1alpha1.ReasonDriftCorrected) > 0
		})
	})
})

// protocolSatisfied 的 ensureHTTPS=true 分支在 envtest 里够不到：所有 fixture 的
// spec.target.fc3CustomDomain.ensureHTTPSProtocol 都是默认的 false。而这条分支决定
// 「要不要为了开 HTTPS 再写一次云」，判错的两个方向都很贵：漏判会每轮重写一次，
// 误判会让用户要求的 HTTPS 永远开不起来。
func TestProtocolSatisfied(t *testing.T) {
	cases := []struct {
		protocol    string
		ensureHTTPS bool
		want        bool
	}{
		// 用户没要求就不看这一项，否则每一轮都会因为「没开 HTTPS」而重写一次。
		{"HTTP", false, true},
		{"", false, true},
		{"HTTPS", true, true},
		{"HTTP,HTTPS", true, true},
		// 云侧的取值形态未实测，大小写与空白都要容忍。
		{"http, https", true, true},
		{" HTTPS ", true, true},
		{"HTTP", true, false},
		{"", true, false},
		// 子串不算：HTTPSX 不是 HTTPS。
		{"HTTPSX", true, false},
	}
	for _, c := range cases {
		got := protocolSatisfied(provider.ObservedState{Protocol: c.protocol}, c.ensureHTTPS)
		if got != c.want {
			t.Errorf("protocolSatisfied(%q, %t) = %t, want %t", c.protocol, c.ensureHTTPS, got, c.want)
		}
	}
}

// noteObserveFailed 的「Applied 不存在就什么都不做」分支是「旁路失败不降级」的护栏本身：
// 在一个从没 Applied 过的对象上凭空造一个 Applied=False，就把一次无害的观测失败变成了
// 真降级——Ready 跟着掉，运维会以为线上 HTTPS 坏了。envtest 到不了这里（能走到 Observe
// 就说明前面几步都过了），所以只能在这里钉住。
func TestNoteObserveFailed(t *testing.T) {
	t.Run("没有 Applied 时一个 condition 都不造", func(t *testing.T) {
		b := &certsv1alpha1.AliyunCertificateBinding{}
		if noteObserveFailed(b) {
			t.Error("返回值必须是 false：没留下痕迹，调用方就不该发事件（否则跃迁判断永不收敛）")
		}
		if len(b.Status.Conditions) != 0 {
			t.Fatalf("凭空造出了 condition: %+v", b.Status.Conditions)
		}
	})

	t.Run("有 Applied 时只改 reason/message，status 一个字节不动", func(t *testing.T) {
		b := &certsv1alpha1.AliyunCertificateBinding{}
		b.Status.Conditions = []metav1.Condition{{
			Type: certsv1alpha1.ConditionApplied, Status: metav1.ConditionTrue,
			Reason: certsv1alpha1.ReasonApplied, LastTransitionTime: metav1.Now(),
		}}
		before := b.Status.Conditions[0].LastTransitionTime
		if !noteObserveFailed(b) {
			t.Error("返回值必须是 true：痕迹已写下，调用方据此决定发事件")
		}
		got := b.Status.Conditions[0]
		if got.Status != metav1.ConditionTrue {
			t.Errorf("status 被改成了 %s，旁路失败不该降级", got.Status)
		}
		if got.Reason != certsv1alpha1.ReasonObserveFailed {
			t.Errorf("reason = %q, want %q", got.Reason, certsv1alpha1.ReasonObserveFailed)
		}
		if got.Message == "" {
			t.Error("message 应当留下「上一轮观测失败」的痕迹")
		}
		// status 没变，跃迁时间就不该动——否则 kubectl describe 会显示一次并不存在的跃迁。
		if !got.LastTransitionTime.Equal(&before) {
			t.Errorf("lastTransitionTime 被改动了: %v -> %v", before, got.LastTransitionTime)
		}
	})
}
