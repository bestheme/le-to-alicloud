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
	"sync"
	"testing"
	"time"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// TestShortFP 固定住防 panic 的截断：status 里的指纹是用户可写字段，被手工改短之后
// 直接切片会把 controller 打进崩溃循环。
func TestShortFP(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"abc":              "abc",
		"0123456":          "0123456",
		"01234567":         "01234567",
		"0123456789abcdef": "01234567",
	}
	for in, want := range cases {
		if got := shortFP(in); got != want {
			t.Errorf("shortFP(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCASFindHint 固定 hint 的优先级：write-ahead 快照 > 调用方现算的域名 > spec。
func TestCASFindHint(t *testing.T) {
	ac := func(hint string) *certsv1alpha1.AliyunCertificate {
		a := &certsv1alpha1.AliyunCertificate{Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: []string{"spec.example.com"}},
		}}
		if hint != "" {
			a.Status.PendingUpload = &certsv1alpha1.PendingUpload{DomainHint: hint}
		}
		return a
	}
	if got := casFindHint(ac("pending.example.com"), "leaf.example.com"); got != "pending.example.com" {
		t.Errorf("有快照时应优先用快照，得到 %q", got)
	}
	if got := casFindHint(ac(""), "leaf.example.com"); got != "leaf.example.com" {
		t.Errorf("没有快照时应用调用方给的域名，得到 %q", got)
	}
	if got := casFindHint(ac(""), ""); got != "spec.example.com" {
		t.Errorf("两者都没有时应回退 spec，得到 %q", got)
	}
}

var _ = Describe("证书 controller：Diverged 与 CAS 探测", func() {
	ctx := context.Background()
	var ca *testutil.CA

	// 假时钟必须带锁：用例在自己的 goroutine 里推进它，reconciler 每一轮都在自己的
	// goroutine 里通过 r.now() 读它（探测判 12h 节流时每轮都读）。裸变量会被 -race
	// 抓个正着；换钟本身也一样，所以走 SetNow 而不是直接写字段。零值表示「跟随真实
	// 时间」，与 Now == nil 的语义一致。
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

	// touch 改一个注解来强制立刻触发一次 reconcile。必须重读重试：reconciler 每轮都会
	// patch status，而 status 子资源同样会顶掉 metadata.resourceVersion，读与写之间撞上
	// 一次就是 Conflict。值每次都要换，否则写回去内容没变，watch 也就不会响。
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

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		freeze(time.Time{})
		reconciler.SetNow(fakeNow)
		DeferCleanup(func() { reconciler.SetNow(nil) })
		ca = testutil.NewCA(GinkgoT())
	})

	It("flag 变更后 IssuerDefaultDiverged=True 但 Ready 不受影响", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "div"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "div", 1, crt, key)
		eventually(func() bool { return condTrue(getAC(ctx, ns, "div"), certsv1alpha1.ConditionReady) })
		Expect(condStatus(getAC(ctx, ns, "div"), certsv1alpha1.ConditionIssuerDefaultDiverged)).
			To(Equal(metav1.ConditionFalse), "flag 与固化值一致时不该报 Diverged")

		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "zerossl", Kind: "ClusterIssuer"})
		touch(ns, "div", "1")

		eventually(func() bool {
			got := getAC(ctx, ns, "div")
			return condTrue(got, certsv1alpha1.ConditionIssuerDefaultDiverged) && condTrue(got, certsv1alpha1.ConditionReady)
		})
		eventually(func() bool {
			return acWarningEventMessage(ctx, ns, "div", certsv1alpha1.ReasonIssuerDefaultDiverged) != ""
		})
		// Diverged 只是提示：Pin 依旧生效，下发的 Certificate 不许跟着 flag 走。
		cert, err := getCert(ctx, ns, "div")
		Expect(err).NotTo(HaveOccurred())
		Expect(cert.Spec.IssuerRef.Name).To(Equal("letsencrypt-prod"))

		// 显式把 issuerRef 写成新值 → 期望态与 flag 一致，Diverged 消失
		Eventually(func() error {
			ac := getAC(ctx, ns, "div")
			ac.Spec.CertificateTemplate.IssuerRef = &cmmeta.IssuerReference{Name: "zerossl", Kind: "ClusterIssuer"}
			return k8sClient.Update(ctx, ac)
		}, "10s", "100ms").Should(Succeed())
		eventually(func() bool {
			got := getAC(ctx, ns, "div")
			return !condTrue(got, certsv1alpha1.ConditionIssuerDefaultDiverged) &&
				got.Status.EffectiveIssuerRef != nil && got.Status.EffectiveIssuerRef.Name == "zerossl"
		})
	})

	It("CAS 侧证书被删后，探测周期到达时重新上传", func() {
		ns := newNamespace(ctx)
		freeze(time.Now())

		// CR 名决定 CAS 名（naming.CASName 用的是 CR 名，不是域名），而 fake 的
		// FindUploaded 只能拿名字近似 Keyword 匹配（它不解析 PEM）。取 "api" 才能让
		// fake 的列表行为和真实 CAS 一致：那张证书的 SAN 确实是 api.example.com。
		Expect(k8sClient.Create(ctx, baseAC(ns, "api"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "api", 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		first := *getAC(ctx, ns, "api").Status.Current
		id1 := *first.CertID

		// 有人手工删掉了 CAS 上那张证书；探测周期未到 → 一次云调用都不该发生
		Expect(currentCAS().Delete(ctx, id1, "")).To(Succeed())
		touch(ns, "api", "1")
		Consistently(func() int { return currentCAS().UploadCalls() }, "1500ms", "200ms").Should(Equal(1))
		Expect(currentCAS().FindCalls()).To(Equal(0), "12h 节流内不该去列 CAS")

		// 时钟越过 12h → 探测发现丢失 → 重传
		advance(13 * time.Hour)
		touch(ns, "api", "2")
		eventually(func() bool {
			a := getAC(ctx, ns, "api")
			return a.Status.Current != nil && a.Status.Current.CertID != nil && *a.Status.Current.CertID != id1
		})
		Expect(currentCAS().UploadCalls()).To(Equal(2))
		got := getAC(ctx, ns, "api")
		Expect(currentCAS().Has(*got.Status.Current.CertID)).To(BeTrue())
		Expect(got.Status.Current.Fingerprint).To(Equal(first.Fingerprint), "证书本身没变，重传的还是同一张")
		Expect(got.Status.PendingUpload).To(BeNil())
		Expect(condTrue(got, certsv1alpha1.ConditionReady)).To(BeTrue())

		// 探测把 current 清成了指纹为空的占位，它不指向任何证书，绝不能被推进 history：
		// 三重护栏一条都拦不住它，只会白占一个 keepLast 槽位并逼着回收多删一代真证书。
		Expect(got.Status.History).To(BeEmpty(), "重传前只有一代，占位不该进 history")

		// 探测时间戳跟着推进：不会每一轮都去列一次，更不会反复重传
		Consistently(func() int { return currentCAS().UploadCalls() }, "1500ms", "200ms").Should(Equal(2))
	})

	// R24 + R25：探测是旁路的一致性检查。RAM 少给一个 ListUserCertificateOrder 权限就够
	// 触发它，而上传与删除完全正常。既不能把健康证书打成 Ready=False（会让 Binding 侧连锁
	// 停摆），也不能就此结束本轮——casProbedAt 只在列表成功后推进，持续失败下每一轮都会
	// 重新探测，早退就等于把续期签出来的新指纹永远挡在上传之外，而且完全无声。
	It("探测持续失败不阻塞续期上传，只发 ProbeFailed 事件", func() {
		ns := newNamespace(ctx)
		freeze(time.Now())
		Expect(k8sClient.Create(ctx, baseAC(ns, "apifail"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "apifail", 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, "apifail")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		first := *getAC(ctx, ns, "apifail").Status.Current
		id1 := *first.CertID

		// 此后每一次 List 都失败：只缺 ListUserCertificateOrder 权限，上传与删除照常
		for i := 0; i < 20; i++ {
			currentCAS().QueueFindErr(&aliyun.Error{
				Class: aliyun.ClassAuth, Op: "ListUserCertificateOrder", Code: "Forbidden.RAM", Err: errors.New("denied")})
		}
		advance(13 * time.Hour)
		touch(ns, "apifail", "1")
		eventually(func() bool {
			return acWarningEventMessage(ctx, ns, "apifail", "ProbeFailed") == probeFailedMessage
		})

		// 列不出清单 ≠ 证书丢了：不许重传，也不许把 current 清掉
		Consistently(func() bool {
			a := getAC(ctx, ns, "apifail")
			return currentCAS().UploadCalls() == 1 &&
				condStatus(a, certsv1alpha1.ConditionUploaded) == metav1.ConditionTrue &&
				condStatus(a, certsv1alpha1.ConditionReady) == metav1.ConditionTrue
		}, "1500ms", "200ms").Should(BeTrue())
		Expect(currentCAS().Has(id1)).To(BeTrue())
		Expect(*getAC(ctx, ns, "apifail").Status.Current.CertID).To(Equal(id1))

		// 关键：探测仍在每轮失败，续期出的新指纹必须照常上传
		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "apifail", 2, crt2, key2)
		eventually(func() bool {
			a := getAC(ctx, ns, "apifail")
			return a.Status.Current != nil && a.Status.Current.CertID != nil &&
				a.Status.Current.Fingerprint != first.Fingerprint
		})
		Expect(currentCAS().UploadCalls()).To(Equal(2))
		Expect(currentCAS().FindCalls()).To(BeNumerically(">=", 1), "探测必须真的试过")
		got := getAC(ctx, ns, "apifail")
		Expect(condStatus(got, certsv1alpha1.ConditionUploaded)).To(Equal(metav1.ConditionTrue))
		Expect(condStatus(got, certsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionTrue))
	})

	It("current 已过期时跳过探测，不把过期证书误判成丢失", func() {
		ns := newNamespace(ctx)
		freeze(time.Now())
		Expect(k8sClient.Create(ctx, baseAC(ns, "api2"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "api2", 1, crt, key)
		eventually(func() bool {
			a := getAC(ctx, ns, "api2")
			return a.Status.Current != nil && a.Status.Current.CertID != nil
		})
		id1 := *getAC(ctx, ns, "api2").Status.Current.CertID

		// leaf 有效期 90 天：越过它就同时越过了 12h 探测周期。真实 CAS 的列表接口不再
		// 返回已过期的证书，照常探测会把它误判成「被人删了」而重传一张同样过期的证书。
		advance(100 * 24 * time.Hour)
		touch(ns, "api2", "1")
		Consistently(func() int { return currentCAS().FindCalls() }, "1500ms", "200ms").Should(Equal(0))
		Expect(currentCAS().Has(id1)).To(BeTrue())
		Expect(currentCAS().UploadCalls()).To(Equal(1))
	})
})
