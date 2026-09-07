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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：OSS 目标", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
		resetOSS()
	})

	It("证书 uploadToCAS=false 时 Applied=False/CASUploadRequired，且不碰 OSS", func() {
		ns := newNamespace(ctx)
		domain, bucket := ossDomainFor(ns, "o1"), ossBucketFor(ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain})
		createCertificate(ctx, ns, "c1", domain) // 这个 helper 关着上传
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createOSSBinding(ctx, ns, "o1", "c1", bucket, domain)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCASUploadRequired
		})
		Expect(bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionReady).Reason).To(Equal(certsv1alpha1.ReasonCASUploadRequired))
		Expect(currentOSS().ListCallsFor(bucket, domain)).To(BeZero())
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(BeZero())
		b := getBinding(ctx, ns, "o1")
		Expect(b.Status.AppliedFingerprint).To(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(BeEmpty())
	})

	It("首绑：按 certRef 写入 OSS，status 同时记 appliedFingerprint 与 appliedCertRef", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c2", "o2")

		want := certRefOf(ctx, ns, "c2")
		Expect(want).NotTo(BeEmpty(), "fake CAS 应已给证书分配 certId")
		rec, ok := currentOSS().Cname(bucket, domain)
		Expect(ok).To(BeTrue())
		Expect(rec.CertRef).To(Equal(want))
		Expect(rec.CertType).To(Equal("CAS"))

		b := getBinding(ctx, ns, "o2")
		Expect(b.Status.AppliedCertRef).To(Equal(want))
		Expect(b.Status.AppliedFingerprint).To(Equal(getAC(ctx, ns, "c2").Status.Current.Fingerprint))
		Expect(b.Status.BoundAccountID).To(Equal(testOSSOwner))
		Expect(bindingCond(ctx, ns, "o2", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(1))
	})

	It("certRef 一致时短路：只 List 不 Put", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c3", "o3")
		puts, lists := currentOSS().PutCallsFor(bucket, domain), currentOSS().ListCallsFor(bucket, domain)

		pokeBinding(ctx, ns, "o3")
		eventually(func() bool { return currentOSS().ListCallsFor(bucket, domain) > lists })
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts))
		Expect(getBinding(ctx, ns, "o3").Status.AppliedCertRef).To(Equal(certRefOf(ctx, ns, "c3")))
	})

	It("漂移：OSS 上是别的 certRef 时纠正一次并只发一条 DriftCorrected", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c4", "o4")
		want := certRefOf(ctx, ns, "c4")
		puts := currentOSS().PutCallsFor(bucket, domain)

		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain, CertRef: "999999-cn-hangzhou", CertType: "CAS"})
		pokeBinding(ctx, ns, "o4")

		eventually(func() bool {
			rec, _ := currentOSS().Cname(bucket, domain)
			return rec.CertRef == want && currentOSS().PutCallsFor(bucket, domain) == puts+1
		})
		eventually(func() bool { return bindingEventCount(ctx, ns, "o4", certsv1alpha1.ReasonDriftCorrected) == 1 })
		b := getBinding(ctx, ns, "o4")
		Expect(b.Status.DriftedCertRef).To(BeEmpty(), "纠正成功后痕迹必须抹掉")
		Expect(b.Status.DriftedFingerprint).To(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(Equal(want))

		// 再推一轮：没有新漂移就不许再发事件。
		pokeBinding(ctx, ns, "o4")
		Consistently(func() int { return bindingEventCount(ctx, ns, "o4", certsv1alpha1.ReasonDriftCorrected) },
			"1s", "200ms").Should(Equal(1))
	})

	It("Unbind：certRef 是自己的才摘，CNAME 保留", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c5", "o5")
		switchToUnbind(ctx, ns, "o5")
		puts := currentOSS().PutCallsFor(bucket, domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "o5"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "o5") })

		rec, ok := currentOSS().Cname(bucket, domain)
		Expect(ok).To(BeTrue(), "Unbind 只摘证书，CNAME 记录必须保留")
		Expect(rec.CertRef).To(BeEmpty())
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts + 1))
	})

	It("Unbind：OSS 上是别人的 certRef 时一动不动", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c6", "o6")
		switchToUnbind(ctx, ns, "o6")
		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain, CertRef: "888888-cn-hangzhou", CertType: "CAS"})
		puts, lists := currentOSS().PutCallsFor(bucket, domain), currentOSS().ListCallsFor(bucket, domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "o6"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "o6") })

		rec, _ := currentOSS().Cname(bucket, domain)
		Expect(rec.CertRef).To(Equal("888888-cn-hangzhou"))
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts), "解绑别人的证书是越权")
		Expect(currentOSS().ListCallsFor(bucket, domain)).To(Equal(lists+1), "删除只该 Observe 一次")
	})

	It("FC3 Binding 的 appliedCertRef 恒为空（回归门）", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b7.%s.example.com", ns)
		issueAndBind(ctx, ns, "c7", "b7", domain, "HTTP")
		b := getBinding(ctx, ns, "b7")
		Expect(b.Status.AppliedFingerprint).NotTo(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(BeEmpty())
		Expect(b.Status.DriftedCertRef).To(BeEmpty())
	})
})

// issueAndBindOSS 建开着 CAS 上传的证书、fake OSS 上的空 CNAME 与 OSS Binding，等到 Applied=True。
func issueAndBindOSS(ctx context.Context, ns, certName, bindingName string) (bucket, domain string) {
	bucket, domain = ossBucketFor(ns), ossDomainFor(ns, bindingName)
	ca := testutil.NewCA(GinkgoT())
	certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
	currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain})
	createCertificateWithCAS(ctx, ns, certName, domain)
	simulateIssuance(ctx, ns, certName, 1, certPEM, keyPEM)
	createOSSBinding(ctx, ns, bindingName, certName, bucket, domain)
	eventually(func() bool {
		return bindingCond(ctx, ns, bindingName, certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
	})
	return bucket, domain
}

// pokeBinding 用 annotation 推一轮 reconcile（bindingMeaningfulChange 认 annotation 变化）。
func pokeBinding(ctx context.Context, ns, name string) {
	b := getBinding(ctx, ns, name)
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations["poke"] = fmt.Sprintf("%d", len(b.Annotations["poke"])+1)
	ExpectWithOffset(1, k8sClient.Update(ctx, b)).To(Succeed())
}

// quiesceOSSTarget 是 quiesceTarget 的 OSS 版：按 (bucket, domain) 盯 GetCname 的次数，
// 连续两次采样不变就说明没有 reconcile 在飞。switchToUnbind 按目标类型二选一调用它，
// 理由与 quiesceTarget 那里逐字相同。
func quiesceOSSTarget(bucket, domain string) {
	last := -1
	EventuallyWithOffset(1, func() bool {
		cur := currentOSS().ListCallsFor(bucket, domain)
		stable := cur == last
		last = cur
		return stable
	}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
}
