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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func binding(gen int64, observed int64, applied string) certsv1alpha1.AliyunCertificateBinding {
	return certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status:     certsv1alpha1.AliyunCertificateBindingStatus{ObservedGeneration: observed, AppliedFingerprint: applied},
	}
}

func TestReclaimable(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	old := certsv1alpha1.CertificateGeneration{Fingerprint: "old", UploadedAt: metav1.NewTime(now.Add(-48 * time.Hour))}
	young := certsv1alpha1.CertificateGeneration{Fingerprint: "young", UploadedAt: metav1.NewTime(now.Add(-1 * time.Hour))}

	cases := []struct {
		name     string
		gen      certsv1alpha1.CertificateGeneration
		bindings []certsv1alpha1.AliyunCertificateBinding
		want     bool
	}{
		{"无 Binding、已过 minAge", old, nil, true},
		{"未过 minAge", young, nil, false},
		{"有 Binding 仍在用该代", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 1, "old")}, false},
		{"Binding 已推进到别的代", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 1, "new")}, true},
		{"Binding 尚未完成首次 reconcile（observedGeneration 落后）", old, []certsv1alpha1.AliyunCertificateBinding{binding(2, 1, "new")}, false},
		{"Binding 刚创建 appliedFingerprint 为空且未 reconcile", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 0, "")}, false},
	}
	for _, c := range cases {
		ok, why := reclaimable(c.gen, now, 24*time.Hour, c.bindings)
		if ok != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, ok, why, c.want)
		}
	}
}

var _ = Describe("证书 controller：回收时序", func() {
	ctx := context.Background()
	var ca *testutil.CA
	var clock time.Time

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		ca = testutil.NewCA(GinkgoT())
		clock = time.Now()
		reconciler.Now = func() time.Time { return clock }
		DeferCleanup(func() { reconciler.Now = nil })
	})

	setBindingStatus := func(ns, name string, applied string) {
		b := &certsv1alpha1.AliyunCertificateBinding{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b)).To(Succeed())
		b.Status.ObservedGeneration = b.Generation
		b.Status.AppliedFingerprint = applied
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, b)).To(Succeed())
	}

	// touch 改一个注解来强制触发一次 reconcile。必须重读重试：reconciler 每轮都会
	// patch status，而 status 子资源同样会顶掉 metadata.resourceVersion，读与写之间
	// 撞上一次就是 Conflict。
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

	It("gen1 只有在所有 Binding 推进且过 minAge 后才被删", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "rot")
		ac.Spec.Retention.KeepLast = 1
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		for _, bn := range []string{"b1", "b2"} {
			Expect(k8sClient.Create(ctx, &certsv1alpha1.AliyunCertificateBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bn, Namespace: ns},
				Spec: certsv1alpha1.AliyunCertificateBindingSpec{
					CertificateRef: certsv1alpha1.LocalObjectReference{Name: "rot"},
					Target: certsv1alpha1.BindingTarget{Type: certsv1alpha1.TargetTypeFC3CustomDomain,
						FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: bn + ".example.com"}},
				},
			})).To(Succeed())
		}

		crt1, key1 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 1, crt1, key1)
		eventually(func() bool {
			a := getAC(ctx, ns, "rot")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		gen1 := *getAC(ctx, ns, "rot").Status.Current
		setBindingStatus(ns, "b1", gen1.Fingerprint)
		setBindingStatus(ns, "b2", gen1.Fingerprint)

		// 续期 → gen2
		clock = clock.Add(30 * 24 * time.Hour)
		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 2, crt2, key2)
		eventually(func() bool { a := getAC(ctx, ns, "rot"); return len(a.Status.History) == 1 })
		gen2 := *getAC(ctx, ns, "rot").Status.Current

		// 两个 Binding 都还在 gen1：不删
		touch(ns, "rot", "1")
		Consistently(func() bool { return currentCAS().Has(*gen1.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 一个推进：仍不删
		setBindingStatus(ns, "b1", gen2.Fingerprint)
		touch(ns, "rot", "2")
		Consistently(func() bool { return currentCAS().Has(*gen1.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 都推进：gen1 已 30 天前上传，minAge 24h 满足 → 删
		setBindingStatus(ns, "b2", gen2.Fingerprint)
		touch(ns, "rot", "3")
		eventually(func() bool { return !currentCAS().Has(*gen1.CertID) })
		eventually(func() bool { return len(getAC(ctx, ns, "rot").Status.History) == 0 })

		// gen3 立刻到来：gen2 进入 history 但年龄 < minAge，不删
		crt3, key3 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 3, crt3, key3)
		eventually(func() bool { a := getAC(ctx, ns, "rot"); return len(a.Status.History) == 1 })
		gen3 := *getAC(ctx, ns, "rot").Status.Current
		setBindingStatus(ns, "b1", gen3.Fingerprint)
		setBindingStatus(ns, "b2", gen3.Fingerprint)
		touch(ns, "rot", "4")
		Consistently(func() bool { return currentCAS().Has(*gen2.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 时钟越过 minAge → 删
		clock = clock.Add(25 * time.Hour)
		touch(ns, "rot", "5")
		eventually(func() bool { return !currentCAS().Has(*gen2.CertID) })
	})
})
