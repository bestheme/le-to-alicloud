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

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// simulateIssuance 模拟 cert-manager：写 Secret，再把 Certificate 置为 Ready/revision。
func simulateIssuance(ctx context.Context, ns, name string, rev int, certPEM, keyPEM []byte) {
	writeTLSSecret(ctx, ns, name, certPEM, keyPEM)
	setCertificateStatus(ctx, ns, name, rev, cmmeta.ConditionFalse)
}

// writeTLSSecret 写 cert-manager 会写的那个 Secret。
func writeTLSSecret(ctx context.Context, ns, name string, certPEM, keyPEM []byte) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: ns,
		Annotations: map[string]string{"cert-manager.io/certificate-name": name}}}
	_, err := ctrlCreateOrUpdateSecret(ctx, s, certPEM, keyPEM)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// setCertificateStatus 一次写完 revision + Ready/Issuing。
// Ready 与 Issuing 必须同一次写入：分两步写会让 reconciler 看见「Ready=True 且
// Issuing=False」的中间态，临时自签证书在那一瞬间会被当成正式证书传上云。
func setCertificateStatus(ctx context.Context, ns, name string, rev int, issuing cmmeta.ConditionStatus) {
	cert := &cmapi.Certificate{}
	EventuallyWithOffset(1, func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cert)
	}).Should(Succeed())
	r := rev
	cert.Status.Revision = &r
	now := metav1.Now()
	cert.Status.Conditions = []cmapi.CertificateCondition{
		{Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, LastTransitionTime: &now},
		{Type: cmapi.CertificateConditionIssuing, Status: issuing, LastTransitionTime: &now},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, cert)).To(Succeed())
}

func ctrlCreateOrUpdateSecret(ctx context.Context, s *corev1.Secret, certPEM, keyPEM []byte) (bool, error) {
	existing := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, existing)
	if err == nil {
		existing.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
		return false, k8sClient.Update(ctx, existing)
	}
	s.Type = corev1.SecretTypeTLS
	s.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
	return true, k8sClient.Create(ctx, s)
}

var _ = Describe("证书 controller：上传", func() {
	ctx := context.Background()
	var ca *testutil.CA

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		ca = testutil.NewCA(GinkgoT())
	})

	It("签发后上传一次，重复 reconcile 不重复上传", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "up"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "up", 1, crt, key)

		eventually(func() bool {
			ac := getAC(ctx, ns, "up")
			return condTrue(ac, certsv1alpha1.ConditionReady) && ac.Status.Current != nil && ac.Status.Current.CertID != nil
		})
		ac := getAC(ctx, ns, "up")
		Expect(ac.Status.Current.CASName).To(HavePrefix("up_"))
		Expect(ac.Status.PendingUpload).To(BeNil())
		Expect(ac.Status.CASProbedAt).NotTo(BeNil())
		Expect(currentCAS().UploadCalls()).To(Equal(1))

		// 再触发一次 reconcile
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
		Consistently(func() int { return currentCAS().UploadCalls() }, "2s", "200ms").Should(Equal(1))
	})

	It("续期：新指纹上传新证书，旧代次进 history", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "renew"))).To(Succeed())
		crt1, key1 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "renew", 1, crt1, key1)
		eventually(func() bool { ac := getAC(ctx, ns, "renew"); return ac.Status.Current != nil })
		first := getAC(ctx, ns, "renew").Status.Current.Fingerprint

		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "renew", 2, crt2, key2)
		eventually(func() bool {
			ac := getAC(ctx, ns, "renew")
			return ac.Status.Current != nil && ac.Status.Current.Fingerprint != first && len(ac.Status.History) == 1
		})
		ac := getAC(ctx, ns, "renew")
		Expect(ac.Status.History[0].Fingerprint).To(Equal(first))
		Expect(currentCAS().UploadCalls()).To(Equal(2))
		Expect(currentCAS().Certs()).To(HaveLen(2))
	})

	It("uploadToCAS=false：不调 CAS，Ready 仍为 True", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "nocas")
		f := false
		ac.Spec.Aliyun.UploadToCAS = &f
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "nocas", 1, crt, key)
		eventually(func() bool {
			got := getAC(ctx, ns, "nocas")
			return condTrue(got, certsv1alpha1.ConditionReady) && got.Status.Current != nil && got.Status.Current.CertID == nil &&
				condReason(got, certsv1alpha1.ConditionUploaded) == certsv1alpha1.ReasonUploadDisabled
		})
		Expect(currentCAS().UploadCalls()).To(Equal(0))
	})

	It("临时自签证书 + Issuing=True 不上传", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "tmp"))).To(Succeed())
		crt, key := testutil.SelfSigned(GinkgoT(), "api.example.com")
		writeTLSSecret(ctx, ns, "tmp", crt, key)
		// Ready=True 与 Issuing=True 一起写：cert-manager 发临时证书时就是这个组合。
		setCertificateStatus(ctx, ns, "tmp", 1, cmmeta.ConditionTrue)

		eventually(func() bool {
			return condReason(getAC(ctx, ns, "tmp"), certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonSelfSignedDuringIssuance
		})
		Consistently(func() int { return currentCAS().UploadCalls() }, "1s", "200ms").Should(Equal(0))
	})

	It("SANs 不覆盖 spec.dnsNames 时不上传", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "sans")
		ac.Spec.CertificateTemplate.DNSNames = []string{"api.example.com", "www.example.com"}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "sans", 1, crt, key)
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "sans"), certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonSANsMismatch
		})
		Expect(currentCAS().UploadCalls()).To(Equal(0))
	})

	It("上传响应丢失后重试命中 ClientToken，不产生重复证书", func() {
		ns := newNamespace(ctx)
		currentCAS().FailNextUploadAfterCommit(&aliyun.Error{Class: aliyun.ClassRetryable, Op: "Upload", Code: "Timeout", Err: errors.New("timeout")})
		Expect(k8sClient.Create(ctx, baseAC(ns, "lost"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "lost", 1, crt, key)

		eventually(func() bool {
			ac := getAC(ctx, ns, "lost")
			return ac.Status.Current != nil && ac.Status.Current.CertID != nil
		})
		Expect(currentCAS().Certs()).To(HaveLen(1))
		Expect(currentCAS().UploadCalls()).To(BeNumerically(">=", 2))
		Expect(getAC(ctx, ns, "lost").Status.PendingUpload).To(BeNil())
	})

	It("凭证 Secret 缺失时 Uploaded=False/CredentialsSecretNotFound", func() {
		ns := newNamespace(ctx)
		prev := reconciler.CASFactory
		reconciler.CASFactory = func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound, errors.New("missing")}
		}
		DeferCleanup(func() { reconciler.CASFactory = prev })

		Expect(k8sClient.Create(ctx, baseAC(ns, "nocred"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "nocred", 1, crt, key)
		eventually(func() bool {
			ac := getAC(ctx, ns, "nocred")
			return condReason(ac, certsv1alpha1.ConditionUploaded) == certsv1alpha1.ReasonCredentialsNotFound && !condTrue(ac, certsv1alpha1.ConditionReady)
		})
	})
})
