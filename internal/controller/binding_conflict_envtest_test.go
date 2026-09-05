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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：冲突仲裁", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("同目标的第二个 Binding 被判 Conflict 且不写云", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		// 这个用例要的就是「两个 Binding 抢同一个目标」，所以两边共用一个域名；
		// 域名本身仍带 namespace，免得跨文件撞上别的用例（仲裁是跨 namespace 的）。
		domain := fmt.Sprintf("first.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		eventually(func() bool {
			return condStatusOf(ctx, ns, "c1", certsv1alpha1.ConditionIssued) == metav1.ConditionTrue
		})

		createBinding(ctx, ns, "first", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "first", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		waitForNextCreationSecond(ctx, ns, "first")
		writesAfterFirst := currentFC3().UpdateCallsFor(domain)

		createBinding(ctx, ns, "second", "c1", domain, nil)
		eventually(func() bool {
			c := bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonConflictingBinding
		})
		Expect(bindingCond(ctx, ns, "second", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		// 输家一次云写入都不该发出——两个 Binding 轮流写同一个域名比不写更危险。
		// 按域名分账而不是全局计数：envtest 从不回收 namespace，别的用例的 Binding
		// 还活着，随时会把全局计数推上去（Task 11 实测过的 1/4 偶发）。这里两个
		// Binding 共用同一个域名，分账计数因此正好就是「这个目标上发生了几次写」。
		Consistently(func() int { return currentFC3().UpdateCallsFor(domain) },
			"2s", "200ms").Should(Equal(writesAfterFirst))
	})

	It("胜者被删掉后输家接管", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("first.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "first", "c1", domain, nil)
		waitForNextCreationSecond(ctx, ns, "first")
		createBinding(ctx, ns, "second", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict).Status == metav1.ConditionTrue
		})

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "first"))).To(Succeed())
		eventually(func() bool {
			c := bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionFalse
		})
		eventually(func() bool {
			return bindingCond(ctx, ns, "second", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})

	It("不同域名互不冲突", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domainA := fmt.Sprintf("ba.%s.example.com", ns)
		domainB := fmt.Sprintf("bb.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domainA, domainB)
		currentFC3().AddDomain(fake.Domain{DomainName: domainA, Protocol: "HTTP"})
		currentFC3().AddDomain(fake.Domain{DomainName: domainB, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domainA, domainB)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "ba", "c1", domainA, nil)
		createBinding(ctx, ns, "bb", "c1", domainB, nil)

		eventually(func() bool {
			return bindingCond(ctx, ns, "ba", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue &&
				bindingCond(ctx, ns, "bb", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})
})

// waitForNextCreationSecond 等到墙钟跨过 name 的 creationTimestamp 所在的那一秒，
// 好让**下一个**建出来的同目标 Binding 拿到严格更晚的时间戳。
//
// 没有这一步，「先建的赢」在这个套件里是掷硬币：creationTimestamp 只精确到秒
// （metav1.Time 序列化到秒），同一秒内建出来的两个 Binding 会落到 lessBinding 的第二
// 把钥匙 UID 上，而 UID 是随机的——用例名里的 first 有一半概率其实是输家，断言随之翻车。
// lessBinding 的主键本来就是创建时间，把两次创建拉开一秒，测的才是它想测的那条规则；
// UID 兜底那一条由 TestPickWinner_TieBreaksOnUID 的纯函数单测覆盖。
func waitForNextCreationSecond(ctx context.Context, ns, name string) {
	ts := getBinding(ctx, ns, name).CreationTimestamp.Time
	if d := time.Until(ts.Add(time.Second)); d > 0 {
		time.Sleep(d + 100*time.Millisecond)
	}
}

// condStatusOf 读 AliyunCertificate 的 condition 状态。
//
// 与 retention_test.go 里的 condStatus(ac, t) 只是名字相近，不冲突。
func condStatusOf(ctx context.Context, ns, name, condType string) metav1.ConditionStatus {
	ac := &certsv1alpha1.AliyunCertificate{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ac); err != nil {
		return metav1.ConditionUnknown
	}
	for _, c := range ac.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}
