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
	"sync"
	"time"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// 续期停滞是一个能持续好几天的真实场景（LE 速率限制、DNS-01 solver 坏掉）。这期间
// status.current 指着的仍是一张有效、正在服役的证书，operator 必须继续维护它——
// 停滞分支一旦早退，探测、回收与 Secret 复读会一起被挂起好几天，而且完全无声。
var _ = Describe("证书 controller：签发停滞", func() {
	ctx := context.Background()
	var ca *testutil.CA

	// 假时钟必须带锁：用例在自己的 goroutine 里推进它，reconciler 每轮都在读它。
	var clockMu sync.Mutex
	var clock time.Time
	fakeNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		if clock.IsZero() {
			return time.Now()
		}
		return clock
	}
	freeze := func(t time.Time) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = t
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(d)
	}

	touch := func(ns, name, v string) {
		EventuallyWithOffset(1, func() error {
			ac := &certsv1alpha1.AliyunCertificate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ac); err != nil {
				return err
			}
			if ac.Annotations == nil {
				ac.Annotations = map[string]string{}
			}
			ac.Annotations["touch"] = v
			return k8sClient.Update(ctx, ac)
		}, "10s", "100ms").Should(Succeed())
	}

	// startStalledRenewal 把 Certificate 置为「续期中」（Ready=True + Issuing=True），
	// 再把假时钟推过停滞阈值。这正是 cert-manager 卡在续期时的样子：旧证书仍然 Ready。
	startStalledRenewal := func(ns, name string) {
		setCertificateStatus(ctx, ns, name, 1, cmmeta.ConditionTrue)
		// 13h 同时越过 6h 停滞阈值与 12h 探测周期，好观察停滞期间探测是否照常进行。
		advance(13 * time.Hour)
	}

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		freeze(time.Time{})
		reconciler.SetNow(fakeNow)
		DeferCleanup(func() { reconciler.SetNow(nil) })
		ca = testutil.NewCA(GinkgoT())
	})

	It("停滞时置 Issued=False/IssuanceStalled，但探测与上传状态照常维护", func() {
		ns := newNamespace(ctx)
		freeze(time.Now())

		Expect(k8sClient.Create(ctx, baseAC(ns, "api"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "api", 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.Current != nil && a.Status.Current.CertID != nil && condTrue(a, certsv1alpha1.ConditionReady)
		})
		id1 := *getAC(ctx, ns, "api").Status.Current.CertID

		startStalledRenewal(ns, "api")
		touch(ns, "api", "stalled")

		eventually(func() bool {
			return condReasonIs(getAC(ctx, ns, "api"), certsv1alpha1.ConditionIssued, certsv1alpha1.ReasonIssuanceStalled)
		})
		got := getAC(ctx, ns, "api")
		Expect(condStatus(got, certsv1alpha1.ConditionIssued)).To(Equal(metav1.ConditionFalse),
			"Secret 里躺着的旧证书能通过校验，但 Issued 不许因此翻回 True——那会让停滞在 condition 上消失")
		Expect(condStatus(got, certsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionFalse))

		// 关键：证书本身仍在服役，维护状态一样都不能丢
		Expect(condStatus(got, certsv1alpha1.ConditionUploaded)).To(Equal(metav1.ConditionTrue),
			"上传状态与续期是否卡住无关")
		Expect(got.Status.Current).NotTo(BeNil(), "status.current 指向仍在服役的证书，不许被清")
		Expect(*got.Status.Current.CertID).To(Equal(id1))
		eventually(func() bool {
			return acWarningEventMessage(ctx, ns, "api", certsv1alpha1.ReasonIssuanceStalled) != ""
		})

		// 探测不再被挂起：13h 已越过 12h 周期，这一轮必须真的去列过 CAS
		Expect(currentCAS().FindCalls()).To(BeNumerically(">=", 1),
			"停滞期间探测必须继续，否则 CAS 侧被删掉也没人发现")
		Expect(currentCAS().UploadCalls()).To(Equal(1), "证书没变，不该重传")

		Expect(promtestutil.ToFloat64(certIssuanceStalled.WithLabelValues(ns, "api"))).To(Equal(1.0))
	})

	It("停滞期间 CAS 上的证书被删掉，仍会被探测出来并重传", func() {
		ns := newNamespace(ctx)
		freeze(time.Now())

		Expect(k8sClient.Create(ctx, baseAC(ns, "api"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "api", 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		id1 := *getAC(ctx, ns, "api").Status.Current.CertID

		// 有人手工删掉了云上那张证书，而与此同时续期正卡着
		Expect(currentCAS().Delete(ctx, id1, "")).To(Succeed())
		startStalledRenewal(ns, "api")
		touch(ns, "api", "stalled-missing")

		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.Current != nil && a.Status.Current.CertID != nil && *a.Status.Current.CertID != id1
		})
		got := getAC(ctx, ns, "api")
		Expect(currentCAS().Has(*got.Status.Current.CertID)).To(BeTrue())
		Expect(currentCAS().UploadCalls()).To(Equal(2), "探测发现丢失后必须重传")
		Expect(condStatus(got, certsv1alpha1.ConditionUploaded)).To(Equal(metav1.ConditionTrue))
		// 重传成功也不该把停滞抹掉：续期仍然卡着，Ready 必须继续是 False
		Expect(condReasonIs(got, certsv1alpha1.ConditionIssued, certsv1alpha1.ReasonIssuanceStalled)).To(BeTrue())
		Expect(condStatus(got, certsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionFalse))
	})
})
