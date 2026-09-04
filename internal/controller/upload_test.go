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
	"sync"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// gatedCAS 在第一次 Upload **提交之后** 卡住，让用例能在「云上已经有证书、status 还没
// 记下它」这个精确窗口里观察服务端状态。放行后那一次调用以 retryable 错误收场，模拟
// 「服务端成功、响应丢失」。
type gatedCAS struct {
	aliyun.CASClient
	first   sync.Once
	release func()
	entered chan struct{}
	gate    chan struct{}
}

func newGatedCAS(inner aliyun.CASClient) *gatedCAS {
	g := &gatedCAS{CASClient: inner, entered: make(chan struct{}), gate: make(chan struct{})}
	var once sync.Once
	g.release = func() { once.Do(func() { close(g.gate) }) }
	return g
}

func (g *gatedCAS) Upload(ctx context.Context, name string, certPEM, keyPEM []byte, token string) (int64, error) {
	id, err := g.CASClient.Upload(ctx, name, certPEM, keyPEM, token)
	held := false
	g.first.Do(func() { held = true })
	if !held {
		return id, err
	}
	close(g.entered)
	<-g.gate
	return 0, &aliyun.Error{Class: aliyun.ClassRetryable, Op: "Upload", Code: "Timeout", Err: errors.New("response lost")}
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
		// 重试是靠 ClientToken 命中的，不是靠同名兜底：findByName 一次都不该被调到。
		Expect(currentCAS().FindCalls()).To(Equal(0))
	})

	It("上传已提交但代次未落盘的窗口里，write-ahead 记录必须还在", func() {
		ns := newNamespace(ctx)
		inner := currentCAS()
		gate := newGatedCAS(inner)
		prev := reconciler.CASFactory
		reconciler.CASFactory = func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
			return gate, nil
		}
		DeferCleanup(func() { gate.release(); reconciler.CASFactory = prev })

		Expect(k8sClient.Create(ctx, baseAC(ns, "atomic"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "atomic", 1, crt, key)

		// 卡在「CAS 已经收下这张证书、reconcile 还没来得及写 current」这一刻。
		// 这正是 write-ahead 存在的理由：此刻进程死掉，pendingUpload 是唯一能把
		// 云上那张证书找回来的线索，所以它必须已经在服务端。
		Eventually(gate.entered, "10s").Should(BeClosed())
		Expect(inner.Certs()).To(HaveLen(1))
		ac := getAC(ctx, ns, "atomic")
		Expect(ac.Status.Current).To(BeNil())
		Expect(ac.Status.PendingUpload).NotTo(BeNil())
		// 云上那张证书带的正是落盘的那个 token，重试才找得回它。
		Expect(inner.Certs()[0].Token).To(Equal(ac.Status.PendingUpload.ClientToken))
		Expect(inner.Certs()[0].Name).To(Equal(ac.Status.PendingUpload.CASName))
		// token 的时间后缀与 StartedAt 取自同一个 now，可以由记录本身复算出来。
		Expect(ac.Status.PendingUpload.ClientToken).To(HaveLen(48))
		Expect(ac.Status.PendingUpload.ClientToken[40:]).To(
			Equal(fmt.Sprintf("%08x", uint32(ac.Status.PendingUpload.StartedAt.Unix()))))

		// 放行后这一轮以 retryable 失败收场，重试靠同一 token 命中，不产生第二张证书。
		gate.release()
		eventually(func() bool {
			got := getAC(ctx, ns, "atomic")
			return got.Status.Current != nil && got.Status.Current.CertID != nil && got.Status.PendingUpload == nil
		})
		Expect(inner.Certs()).To(HaveLen(1))
		Expect(inner.FindCalls()).To(Equal(0))
	})

	It("write-ahead 记录的清除与代次推进落在同一次写里", func() {
		ns := newNamespace(ctx)

		// 用 watch 而不是轮询：它看得到服务端写出的**每一个**版本，抽样会漏掉那些
		// 只存在几毫秒的中间态。
		wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		w, err := wc.Watch(ctx, &certsv1alpha1.AliyunCertificateList{}, client.InNamespace(ns))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(w.Stop)

		var (
			mu         sync.Mutex
			sawPending bool
			orphans    []string
			settled    = make(chan struct{})
		)
		go func() {
			defer GinkgoRecover()
			for ev := range w.ResultChan() {
				got, ok := ev.Object.(*certsv1alpha1.AliyunCertificate)
				if !ok {
					continue
				}
				mu.Lock()
				recorded := got.Status.Current != nil && got.Status.Current.CertID != nil
				switch {
				case got.Status.PendingUpload != nil:
					sawPending = true
				case sawPending && !recorded:
					// write-ahead 记录没了，代次也还没写：这一版里 CAS 上那张证书
					// 无人认领，此刻崩溃就再也找不回它。
					orphans = append(orphans, got.ResourceVersion)
				}
				if sawPending && recorded && got.Status.PendingUpload == nil {
					select {
					case <-settled:
					default:
						close(settled)
					}
				}
				mu.Unlock()
			}
		}()

		Expect(k8sClient.Create(ctx, baseAC(ns, "atomicwrite"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "atomicwrite", 1, crt, key)

		Eventually(settled, "15s").Should(BeClosed())
		mu.Lock()
		defer mu.Unlock()
		Expect(orphans).To(BeEmpty(), "这些版本里 pendingUpload 已清但代次未推进")
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
