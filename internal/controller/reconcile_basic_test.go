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
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func baseAC(ns, name string) *certsv1alpha1.AliyunCertificate {
	return &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: []string{"api.example.com"}},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun"},
				Region:         "cn-hangzhou",
			},
		},
	}
}

func getAC(ctx context.Context, ns, name string) *certsv1alpha1.AliyunCertificate {
	ac := &certsv1alpha1.AliyunCertificate{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ac)).To(Succeed())
	return ac
}

func getCert(ctx context.Context, ns, name string) (*cmapi.Certificate, error) {
	c := &cmapi.Certificate{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, c)
	return c, err
}

func condReason(ac *certsv1alpha1.AliyunCertificate, t string) string {
	c := meta.FindStatusCondition(ac.Status.Conditions, t)
	if c == nil {
		return ""
	}
	return c.Reason
}

var _ = Describe("证书 controller：基础 reconcile", func() {
	ctx := context.Background()

	BeforeEach(func() {
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
	})

	It("创建 cert-manager Certificate、加 finalizer、固化 issuer", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "c1"))).To(Succeed())

		eventually(func() bool { _, err := getCert(ctx, ns, "c1"); return err == nil })
		cert, _ := getCert(ctx, ns, "c1")
		Expect(cert.Spec.SecretName).To(Equal("c1-tls"))
		Expect(cert.Spec.IssuerRef).To(Equal(cmmeta.IssuerReference{Name: "letsencrypt-prod", Kind: "ClusterIssuer", Group: "cert-manager.io"}))
		Expect(cert.Spec.DNSNames).To(Equal([]string{"api.example.com"}))
		Expect(cert.Spec.PrivateKey.Encoding).To(Equal(cmapi.PKCS1))
		Expect(cert.Spec.SecretTemplate.Labels).To(HaveKeyWithValue(certsv1alpha1.LabelManaged, "true"))
		Expect(cert.OwnerReferences).To(HaveLen(1))
		Expect(cert.OwnerReferences[0].Name).To(Equal("c1"))

		eventually(func() bool {
			ac := getAC(ctx, ns, "c1")
			return controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) &&
				ac.Status.EffectiveIssuerRef != nil && ac.Status.EffectiveIssuerRef.Name == "letsencrypt-prod" &&
				ac.Status.SecretName == "c1-tls" &&
				condReason(ac, certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonCertificateNotReady
		})
	})

	It("Pin：flag 变更后已存在证书的 issuerRef 不变", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "pin"))).To(Succeed())
		eventually(func() bool {
			ac := getAC(ctx, ns, "pin")
			return ac.Status.EffectiveIssuerRef != nil
		})

		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "zerossl", Kind: "ClusterIssuer"})
		// 触发一次 reconcile
		ac := getAC(ctx, ns, "pin")
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())

		Consistently(func() string {
			cert, err := getCert(ctx, ns, "pin")
			if err != nil {
				return ""
			}
			return cert.Spec.IssuerRef.Name
		}, "2s", "200ms").Should(Equal("letsencrypt-prod"))

		// 新建的 CR 用新默认
		Expect(k8sClient.Create(ctx, baseAC(ns, "fresh"))).To(Succeed())
		eventually(func() bool {
			cert, err := getCert(ctx, ns, "fresh")
			return err == nil && cert.Spec.IssuerRef.Name == "zerossl"
		})
	})

	It("spec.issuerRef 覆盖 flag 默认", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "explicit")
		ac.Spec.CertificateTemplate.IssuerRef = &cmmeta.IssuerReference{Name: "internal-ca"}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		eventually(func() bool {
			cert, err := getCert(ctx, ns, "explicit")
			return err == nil && cert.Spec.IssuerRef.Name == "internal-ca" && cert.Spec.IssuerRef.Kind == "Issuer"
		})
	})

	It("无 issuer 可用时 Ready=False/NoIssuer 且不创建 Certificate", func() {
		reconciler.SetIssuerDefaults(IssuerDefaults{})
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "noissuer"))).To(Succeed())
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "noissuer"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonNoIssuer
		})
		_, err := getCert(ctx, ns, "noissuer")
		Expect(err).To(HaveOccurred())
	})

	It("目标 Secret 已被占用时 Ready=False/SecretNameConflict", func() {
		// 冲突分支没有 watch 能唤醒它，只能靠 RequeueAfter 自愈；requeue 的时长在冲突那一次
		// reconcile 时就定死了，所以必须在创建 CR 之前把周期调短，否则要等满 1h。
		reconciler.ResyncInterval = 500 * time.Millisecond
		DeferCleanup(func() { reconciler.ResyncInterval = time.Hour })

		ns := newNamespace(ctx)
		occupied := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "taken-tls", Namespace: ns},
			Data:       map[string][]byte{"foo": []byte("bar")},
		}
		Expect(k8sClient.Create(ctx, occupied)).To(Succeed())
		Expect(k8sClient.Create(ctx, baseAC(ns, "taken"))).To(Succeed())
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "taken"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonSecretNameConflict
		})
		_, err := getCert(ctx, ns, "taken")
		Expect(err).To(HaveOccurred())

		// 占用者被删掉后，下一次 requeue 应当自愈：创建 Certificate、脱离 SecretNameConflict。
		Expect(k8sClient.Delete(ctx, occupied)).To(Succeed())
		eventually(func() bool { _, err := getCert(ctx, ns, "taken"); return err == nil })
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "taken"), certsv1alpha1.ConditionReady) != certsv1alpha1.ReasonSecretNameConflict
		})
	})

	It("Certificate Ready 但 Secret 缺失时 Issued=False/SecretNotFound", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "ready"))).To(Succeed())
		eventually(func() bool { _, err := getCert(ctx, ns, "ready"); return err == nil })
		cert, _ := getCert(ctx, ns, "ready")
		rev := 1
		cert.Status.Revision = &rev
		cert.Status.Conditions = []cmapi.CertificateCondition{{Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, LastTransitionTime: &metav1.Time{Time: metav1.Now().Time}}}
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())
		// Certificate Ready 只是必要条件：Secret 还没落地，Issued 必须留在 False。
		eventually(func() bool {
			ac := getAC(ctx, ns, "ready")
			return condReason(ac, certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonSecretNotFound &&
				!condTrue(ac, certsv1alpha1.ConditionIssued) &&
				ac.Status.Issuance != nil && ac.Status.Issuance.Revision != nil && *ac.Status.Issuance.Revision == 1
		})
	})
})
