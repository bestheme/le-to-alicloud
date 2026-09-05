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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// 这两个用例走的是 reconcileBindingReady 步骤 4：证书、材料、域名覆盖、证书闸门全部
// 通过，唯独 ProviderFactory 造不出 client。注入点是 suite 里的 fc3FactoryErr，
// 因为生产工厂的失败（Secret 不在、AK 写坏）在 envtest 里无法稳定构造出来。
var _ = Describe("绑定 controller：凭证失败", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	// readyBinding 把一个 Binding 推到「只差 provider client」的状态，并返回它的目标域名
	// ——调用方要用它按域名断言云调用次数（全局计数会被别的用例仍在跑的 reconcile 撞脏）。
	readyBinding := func(ns, name string) string {
		domain := fmt.Sprintf("%s.%s.example.com", name, ns)
		certName := name + "-cert"
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		createCertificate(ctx, ns, certName, domain)
		simulateIssuance(ctx, ns, certName, 1, certPEM, keyPEM)
		createBinding(ctx, ns, name, certName, domain, nil)
		return domain
	}

	It("凭证 Secret 不存在时 Ready=False/CredentialsSecretNotFound，不碰 Applied 也不写云", func() {
		ns := newNamespace(ctx)
		// 先注入再建对象：反过来的话第一轮 reconcile 可能已经把 Applied=True 写上了。
		setFC3FactoryErr(&credentialsError{certsv1alpha1.ReasonCredentialsNotFound,
			errors.New("凭证 Secret \"fc-cred\" 不存在")})
		domain := readyBinding(ns, "b1")

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCredentialsNotFound
		})
		// 凭证失败会降级（吊销的 AK 确实决定了什么都写不进去），但它不是「证书在目标上
		// 是错的」这一结论，所以 Applied 必须**根本不存在**——断言零值而不是「不为 True」。
		Expect(bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status).To(BeEmpty())
		// 早退路径绝不写 appliedFingerprint（证书 controller 的保留策略依赖这一点）。
		Expect(getBinding(ctx, ns, "b1").Status.AppliedFingerprint).To(BeEmpty())
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
	})

	It("接线错误（未注册 provider）落在 Ready=False/ApplyFailed，并带上原因", func() {
		ns := newNamespace(ctx)
		const wiring = "没有注册 target.type 的 provider"
		setFC3FactoryErr(errors.New(wiring))
		domain := readyBinding(ns, "b2")

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b2", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonApplyFailed
		})
		// message 必须来自工厂错误本身：aggregateBindingReady 在「Applied 缺失」时也会
		// 写一个 Ready=False，而它写的 message 是空串——不断言 message，这个用例在
		// handleFactoryError 被整条删掉之后照样会通过。
		// （那条兜底的 reason 现在是 ObserveFailed，见 aggregateBindingReady；
		// 此处的 ApplyFailed 因此只可能来自 handleFactoryError 的接线错误分支。）
		Expect(bindingCond(ctx, ns, "b2", certsv1alpha1.ConditionReady).Message).To(Equal(wiring))
		Expect(currentFC3().UpdateCallsFor(domain)).To(BeZero())
	})
})
