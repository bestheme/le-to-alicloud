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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// 每个用例用自己的一组对象名（b1/c1 … b9/c9）而不是全体叫 b1/c1。两个理由：
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
		Expect(currentFC3().UpdateCalls()).To(BeZero())
	})

	// PIt：本用例要先有一次成功的 Apply（issueAndBind 等的是 Applied=True），而 Apply
	// 落在 Task 12。用例体逐字保留，Task 12 把 PIt 改回 It 即可——验收项是
	// `grep -n "PIt" internal/controller/binding_observe_test.go` 为空。
	PIt("指纹一致时短路：不写云，但仍然 Observe", func() {
		ns := newNamespace(ctx)
		issueAndBind(ctx, ns, "c2", "b2", fmt.Sprintf("b2.%s.example.com", ns), "HTTP")

		writes := currentFC3().UpdateCalls()
		getsBefore := currentFC3().GetCalls()

		// 推一次 reconcile（spec.target 不可变，改 deletionPolicy 推进 generation）。
		b := getBinding(ctx, ns, "b2")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyOrphan
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool { return currentFC3().GetCalls() > getsBefore })
		// 短路只跳过写，不跳过读（spec §3「level-triggered」）。
		Expect(currentFC3().UpdateCalls()).To(Equal(writes))
		Expect(getBinding(ctx, ns, "b2").Status.LastObservedTime).NotTo(BeNil())
	})

	// PIt：同上，Task 12 解除。
	PIt("账号变了就 fencing：Conflict=True/AccountMismatch 且不写", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b3.%s.example.com", ns)
		issueAndBind(ctx, ns, "c3", "b3", domain, "HTTPS")
		Expect(getBinding(ctx, ns, "b3").Status.BoundAccountID).To(Equal(testAccountID))

		writes := currentFC3().UpdateCalls()
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
		Expect(currentFC3().UpdateCalls()).To(Equal(writes))
	})

	// PIt：同上，Task 12 解除。
	PIt("云侧被人换了证书时判定为 drift 并纠正", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b4.%s.example.com", ns)
		// 真实的漂移场景发生在一个已经在跑 HTTPS 的域名上；顺带钉住 read-modify-write
		// 必须把 protocol 原样带回（fake 的 validateUpdate 只挡空值，挡不住「悄悄改成
		// HTTP」）。
		issueAndBind(ctx, ns, "c4", "b4", domain, "HTTP,HTTPS")
		writes := currentFC3().UpdateCalls()

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

		eventually(func() bool { return currentFC3().UpdateCalls() > writes })
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

	// PIt：同上，Task 12 解除。
	PIt("Observe 未知失败属于旁路：不降级 Applied", func() {
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

	// 上面四个 PIt 全都要先有一次成功的 Apply 才能起步，于是本任务自己产出的三条逻辑
	// （短路、fencing、旁路失败不降级）在 Task 12 之前一行都跑不到。下面三个用例走
	// **接管**这条路进同一批状态：云上本来就装着同一张证书，Observe 一比对指纹就短路，
	// Applied=True 完全不经过 Apply。它们因此今天就是活的，而且顺带覆盖了 PIt 那几个
	// 用例覆盖不到的东西——「首次接管」正是 appliedFingerprint / boundAccountId 由空
	// 变成有值的那一刻。
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
		Expect(currentFC3().GetCalls()).NotTo(BeZero())
		Expect(currentFC3().UpdateCalls()).To(BeZero())
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
		Expect(currentFC3().UpdateCalls()).To(BeZero())
		// fencing 每一轮都要把 Conflict 从仲裁刚写下的 False 再翻回 True（步骤 2 排在
		// Observe 之前，顺序由 spec §6.2 定死）。这一翻若每轮都改写 lastTransitionTime，
		// status patch 就每轮都非空，自己的 watch 会立刻把自己叫醒——一个满速自旋的
		// reconcile 循环，而云侧一个字节都没写，指标上也看不出异常。收敛之后
		// resourceVersion 必须停住。
		rv := getBinding(ctx, ns, "b7").ResourceVersion
		Consistently(func() string { return getBinding(ctx, ns, "b7").ResourceVersion },
			"2s", "200ms").Should(Equal(rv), "fencing 在稳态下必须是幂等的，不能每轮刷一次 status")
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

		// 纠正那一步要等 Task 12 的 Apply；本任务只保证「检测到了」。
		eventually(func() bool {
			return promtestutil.ToFloat64(
				bindingDriftTotal.WithLabelValues(certsv1alpha1.TargetTypeFC3CustomDomain)) > before
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
		noteObserveFailed(b)
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
		noteObserveFailed(b)
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
