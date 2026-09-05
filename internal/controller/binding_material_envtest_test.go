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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：证书材料", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("证书不覆盖目标域名时硬失败，不写云", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		// 故意签一张不覆盖 domain 的证书。
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, "other."+ns+".example.com")
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", "other."+ns+".example.com")
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonDomainNotCovered
		})
		// 推错证书 = 全站 TLS 报错（spec D17）。宁可停下也绝不写。
		Expect(currentFC3().UpdateCalls()).To(BeZero())
		// 不碰 Applied：目标上原来那张证书（如果有）还在服役，域名不覆盖说明不了它有毛病。
		Expect(bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status).
			NotTo(Equal(metav1.ConditionTrue))
	})

	// Secret 消失后的最终态是 CertificateNotReady，**不是** SecretNotFound。
	//
	// 证书 controller 也 watch AliyunCertificateBinding，所以任何一次 Binding 写入都会唤醒
	// 它；它重读 Secret 发现不在，把 Issued 钉成 False/SecretNotFound。而绑定侧的
	// certIssued 闸门排在装载材料之前（spec §6.2 的固定顺序），于是 Ready 落在
	// CertificateNotReady 上。Issued=False 是粘住的（Secret 不会自己回来），所以这不是
	// 竞态窗口的问题——无论谁先跑，收敛点只有一个。
	//
	// 绑定侧的 SecretNotFound 分支在生产里仍然活着：证书 controller 最长要等一个
	// ResyncInterval 才注意到 Secret 没了，那段窗口里绑定会自己撞上 Get 404。它的
	// 确定性覆盖在 TestLoadBindingMaterial_SecretNotFound（fake client，不受这条竞争影响）。
	It("Secret 不见了时不 Ready、也绝不写云", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady).Reason != ""
		})

		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1-tls"}}
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		// 触发一次 reconcile：改 deletionPolicy 会推进 generation（spec.target 不可变）。
		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotReady
		})
		// 材料读不出来就一个字节都不写：这才是这条路径真正要守住的东西。
		Expect(currentFC3().UpdateCalls()).To(BeZero())
		// 证书 controller 的判定与绑定侧的 reason 同源，钉住上面那段推理。
		Expect(condReason(getAC(ctx, ns, "c1"), certsv1alpha1.ConditionIssued)).
			To(Equal(certsv1alpha1.ReasonSecretNotFound))
	})
})
