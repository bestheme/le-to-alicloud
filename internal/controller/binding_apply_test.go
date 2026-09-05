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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：Apply", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("首次绑定写入证书并固化 status", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		b0, _ := pki.ParseBundle(certPEM, keyPEM)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		b := getBinding(ctx, ns, "b1")
		Expect(b.Status.AppliedFingerprint).To(Equal(b0.Fingerprint))
		Expect(b.Status.BoundAccountID).To(Equal(testAccountID))
		Expect(b.Status.LastAppliedTime).NotTo(BeNil())
		Expect(bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		d, _ := currentFC3().Domain(domain)
		Expect(d.CertPEM).To(Equal(b0.CertPEM()))
		Expect(d.Echo).To(Equal("routes"), "read-modify-write 必须原样保住 routeConfig")
		Expect(d.Protocol).To(Equal("HTTP"), "ensureHTTPSProtocol 默认 false，不许动 protocol")
	})

	It("ensureHTTPSProtocol=true 时把 HTTP 升为 HTTP,HTTPS", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, func(b *certsv1alpha1.AliyunCertificateBinding) {
			b.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol = true
		})

		eventually(func() bool {
			d, ok := currentFC3().Domain(domain)
			return ok && d.Protocol == "HTTP,HTTPS"
		})
	})

	It("证书轮换后把新代次推上去", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		gen1Cert, gen1Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, gen1Cert, gen1Key)
		createBinding(ctx, ns, "b1", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})

		gen2Cert, gen2Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		g2, _ := pki.ParseBundle(gen2Cert, gen2Key)
		simulateIssuance(ctx, ns, "c1", 2, gen2Cert, gen2Key)

		// 证书 status.current 变化会经 field index 反查唤醒 Binding，无需人工推。
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Status.AppliedFingerprint == g2.Fingerprint
		})
		d, _ := currentFC3().Domain(domain)
		Expect(d.CertPEM).To(Equal(g2.CertPEM()))
	})

	It("Apply 被限流时 Applied=False/Throttled 并重试", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		// 排 8 个而不是 1 个。退避是 5ms 起步的指数序列，只排一个的话
		// Applied=False/Throttled 只存在几毫秒，而断言的轮询间隔是 100ms——这个用例会
		// 变成掷硬币（实测：只排一个必超时）。8 个把失败态撑到约 1.3s，既看得见又仍然
		// 会自愈，两侧断言这才都是真的。
		for range 8 {
			currentFC3().QueueUpdateErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionUpdateCustomDomain,
				Code: "Throttling.User", Err: errors.New("slow down"),
			})
		}

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonThrottled
		})
		// 队列排空之后应自愈——限流不需要人介入。
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})

	// 上一个用例只排了一个错误，自愈得太快，测不到「持续失败」这一侧的两条不变量：
	// ① 失败早退绝不写 appliedFingerprint / lastAppliedTime；② 事件只在跃迁那一轮发。
	// 这里把队列排满，让整个断言窗口都停在失败态。
	It("Apply 持续失败：不写 appliedFingerprint，事件也不跟着轮次涨", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b2.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})
		// Retryable：controller-runtime 会以 5ms 起步的退避一轮轮重来。
		for range 200 {
			currentFC3().QueueUpdateErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionUpdateCustomDomain,
				Code: "Throttling.User", Err: errors.New("slow down"),
			})
		}

		createCertificate(ctx, ns, "c2", domain)
		simulateIssuance(ctx, ns, "c2", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b2", "c2", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b2", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonThrottled
		})
		// 证书 controller 的保留护栏 3 从「observedGeneration 已追平」推断
		// appliedFingerprint 可信；写失败时云上根本没有这张证书，写下去就是让护栏拿一个
		// 凭空的指纹去比对。lastAppliedTime 同理——它是「什么时候真的写成功过」。
		Consistently(func() bool {
			b := getBinding(ctx, ns, "b2")
			return b.Status.AppliedFingerprint == "" && b.Status.LastAppliedTime == nil
		}, "2s", "200ms").Should(BeTrue(), "失败早退不许固化任何「写成了」的痕迹")
		// 事件只在跃迁那一轮发（spec §10.2）。等到失败轮次远多于事件数，
		// 「每轮一条」就无处躲藏。
		eventually(func() bool { return currentFC3().UpdateCallsFor(domain) >= 8 })
		n := bindingEventCount(ctx, ns, "b2", certsv1alpha1.ReasonApplyFailed)
		Expect(n).To(BeNumerically(">=", 1), "跃迁那一轮必须发一条")
		// 不断言恰好等于 1：跃迁判据取自本轮开始时读到的那一份，informer cache 滞后时
		// 紧邻的一两轮可能仍看着旧 reason，于是多发一条——**有界**的重复。
		Expect(n).To(BeNumerically("<=", 3),
			fmt.Sprintf("已失败 %d 轮却发了 %d 条事件——事件数不该跟轮次一起涨",
				currentFC3().UpdateCallsFor(domain), n))
	})

	It("Update 提交后响应丢失：重试写同样内容，不留半成品", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		b0, _ := pki.ParseBundle(certPEM, keyPEM)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})
		currentFC3().FailNextUpdateAfterCommit(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionUpdateCustomDomain,
			Code: "Timeout", Err: errors.New("response lost"),
		})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		// 服务端其实已经写成功了；下一轮 Observe 会看到指纹已经对上，直接短路。
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Status.AppliedFingerprint == b0.Fingerprint
		})
		d, _ := currentFC3().Domain(domain)
		Expect(d.Echo).To(Equal("routes"))
	})

	It("凭证 Secret 不存在时 Ready=False/CredentialsSecretNotFound", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		setFC3FactoryErr(&credentialsError{certsv1alpha1.ReasonCredentialsNotFound, errors.New("凭证 Secret 不存在")})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCredentialsNotFound
		})
	})
})
