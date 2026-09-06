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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// tickingClock 让每一轮 reconcile 都落在**不同的一秒**上，把生产上的那个循环搬进 envtest。
//
// status.lastObservedTime 是 metav1.Time，序列化到整秒，而 noteObserved 只在秒级取值真的
// 会变时才写。生产上一轮 reconcile ≈ 两次真实云调用 ≈ 1 秒，相邻两轮几乎总落在不同秒，
// 于是每一轮都写出一次非空 status patch；fake FC3 是内存里的 map，一轮不到 1 毫秒，相邻
// 两轮几乎总落在同一秒，patch 是空的、API server 不会写、watch 也就不响——真实时钟下这个
// 缺陷在 envtest 里根本不发生（已实测：不换时钟，下面的用例在修复前照样绿）。
//
// 每读一次前进一秒，是对「每一轮都跨秒」这个生产条件的最小复刻，而不是把轮次放大：
// 收敛的实现下时钟被读几次由轮次决定，读得再快也变不出额外的一轮。
func tickingClock() func() time.Time {
	var mu sync.Mutex
	base := time.Now()
	var ticks int
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}
}

var _ = Describe("绑定 controller：status 写入不自唤醒", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
		bindingReconciler.SetNow(tickingClock())
		DeferCleanup(func() { bindingReconciler.SetNow(nil) })
	})

	It("Apply 因权限被拒时不自唤醒：一个失败周期只打一次云", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})

		// 现场（2026-09-06）就是这一条：RAM 上少一条 fc:UpdateCustomDomain，
		// UpdateCustomDomain 返回 AccessDenied，Apply 每一轮都被拒、且不会自己恢复。
		currentFC3().AlwaysUpdateErr(&aliyun.Error{
			Class: aliyun.ClassAuth, Op: aliyun.ActionUpdateCustomDomain,
			Code: "AccessDenied", Err: errors.New("no permission"),
		})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		// 先确认这条路径真的走到了 Apply（否则下面的上界断言是空断言），
		// 并且按设计停在 5 分钟的长 requeue 上。
		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCredentialsInvalid
		})

		// 基线取在「已经落进 credentialsRequeue（5 分钟）」这个终态之后，而不是写死一个
		// 绝对上界：建对象前后证书 CR 的 status 还会动几次，而对 AliyunCertificate 的那条
		// watch 按设计没有谓词，被它唤醒几轮取决于机器快慢（-race 下实测比不带 race 多一轮），
		// 那是**设置阶段**的噪声，不是本用例要钉的东西。
		//
		// 要钉的是终态之后的增长：5 分钟的窗口里一轮都不该再有。容差 +1 留给基线那一刻
		// 可能正在飞的那一轮。修复前这里不是 +1 而是每秒好几轮，一路涨到窗口结束。
		updates, gets := currentFC3().UpdateCallsFor(domain), currentFC3().GetCallsFor(domain)
		Consistently(func() int { return currentFC3().UpdateCallsFor(domain) },
			"15s", "250ms").Should(BeNumerically("<=", updates+1),
			"status 写入不该把自己唤醒——一个 5 分钟的失败周期只该打一次云")
		Expect(currentFC3().GetCallsFor(domain)).To(BeNumerically("<=", gets+1),
			"Observe 的次数同样受轮次约束")
	})

	It("spec 变更仍然触发一轮", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b2.%s.example.com", ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})
		createCertificate(ctx, ns, "c2", domain)
		simulateIssuance(ctx, ns, "c2", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b2", "c2", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b2", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})

		// 静置到「只剩 DriftCheckInterval（1 小时）能唤醒它」的稳定态，否则下面的增长
		// 断言可能只是上一轮的尾巴。
		gets := currentFC3().GetCallsFor(domain)
		Consistently(func() int { return currentFC3().GetCallsFor(domain) },
			"2s", "200ms").Should(Equal(gets))

		// spec.target 是 CRD 层不可变的，改 deletionPolicy 同样推进 generation。
		b := getBinding(ctx, ns, "b2")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		wantGen := b.Generation

		// 谓词过窄（比如只留 generation 之外的判据、或把 For 整条 filter 掉）会让这条
		// 断言超时——一个再也醒不过来的 controller 比多跑几轮危险得多。
		eventually(func() bool { return currentFC3().GetCallsFor(domain) > gets })
		eventually(func() bool {
			return getBinding(ctx, ns, "b2").Status.ObservedGeneration == wantGen
		})
	})
})
