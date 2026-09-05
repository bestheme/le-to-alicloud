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
	"testing"
	"time"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake" // fake 已归 pkg/aliyun/fake
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

var _ = Describe("绑定 controller：骨架", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("证书不存在时 Ready=False/CertificateNotFound，且不碰云", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		createBinding(ctx, ns, "b1", "no-such-cert", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotFound
		})
		Expect(currentFC3().GetCallsFor(domain)).To(BeZero())
	})

	It("加 finalizer 并写 observedGeneration", func() {
		ns := newNamespace(ctx)
		createBinding(ctx, ns, "b2", "no-such-cert", fmt.Sprintf("b2.%s.example.com", ns), nil)

		b := &certsv1alpha1.AliyunCertificateBinding{}
		eventually(func() bool {
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b2"}, b); err != nil {
				return false
			}
			return len(b.Finalizers) == 1 && b.Finalizers[0] == certsv1alpha1.FinalizerName &&
				b.Status.ObservedGeneration == b.Generation
		})
	})

	It("证书存在但 Issued=False 时 Ready=False/CertificateNotReady", func() {
		ns := newNamespace(ctx)
		// 只建 AliyunCertificate，不让它走到 Issued：不写 Secret，证书 controller 会
		// 停在 Issued=False/SecretNotFound。
		domain := fmt.Sprintf("b3.%s.example.com", ns)
		createCertificate(ctx, ns, "c1", domain)
		setCertificateStatus(ctx, ns, "c1", 1, cmmeta.ConditionFalse)
		createBinding(ctx, ns, "b3", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b3", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotReady
		})
		Expect(currentFC3().GetCallsFor(domain)).To(BeZero())
	})

	It("删除 Binding 时摘掉 finalizer，对象真的消失", func() {
		ns := newNamespace(ctx)
		createBinding(ctx, ns, "b4", "no-such-cert", fmt.Sprintf("b4.%s.example.com", ns), nil)

		// 必须先等 finalizer 落上：没有它这个用例什么都证明不了——对象会因为压根没人拦着
		// 而立刻消失，删除分支是否真的摘过 finalizer 无从分辨。
		eventually(func() bool {
			return controllerutil.ContainsFinalizer(getBinding(ctx, ns, "b4"), certsv1alpha1.FinalizerName)
		})

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b4"))).To(Succeed())
		// 直接 Get 到 NotFound 才算数：只看 deletionTimestamp 的话，一个永远摘不掉
		// finalizer 的 Binding 也能骗过断言，而它会把整个 namespace 卡在 Terminating。
		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b4"},
				&certsv1alpha1.AliyunCertificateBinding{})
			return apierrors.IsNotFound(err)
		})
	})

	It("同目标索引跳过空 TargetKey 而不是 panic", func() {
		// CEL 已经挡住了 type/内嵌块不一致的对象，但索引函数仍必须能安全地处理它——
		// 索引在 CRD 校验之前就会对每一个进 cache 的对象求值。
		b := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(b.TargetKey()).To(BeEmpty())
		// 断言索引函数本身：TargetKey() 返回空串是 API 包的性质，跳过空键是索引的性质。
		Expect(bindingTargetIndex(b)).To(BeNil())
	})

	It("fake FC3 已接进 suite", func() {
		Expect(currentFC3()).To(BeAssignableToTypeOf(&fake.FC3{}))
	})
})

// spec §6.1：每一次成功的 reconcile 都返回漂移检查间隔。证书不存在与证书未就绪这两条
// 终态早退此前返回的是空 Result——证书的 watch 保证了它们不会卡住，但
// aliyuncert_binding_ready / _applied_age 这两个 gauge 只在证书对象变化时才刷新，
// 而这两种状态恰恰可能长时间停在那里（Argo CD 正在换名字重建、签发一直不成功）。
func TestReconcile_CertificateTerminalStatesRequeueOnDriftInterval(t *testing.T) {
	const drift = 42 * time.Minute
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "b1"}}

	fixture := func(objs ...client.Object) *AliyunCertificateBindingReconciler {
		b := bindingWithDomain("api.example.com")
		b.Namespace, b.Name = "ns1", "b1"
		b.Spec.CertificateRef = certsv1alpha1.LocalObjectReference{Name: "c1"}
		// finalizer 必须先有，否则 Reconcile 会停在「加 finalizer 再说」那一步。
		b.Finalizers = []string{certsv1alpha1.FinalizerName}
		c := crfake.NewClientBuilder().WithScheme(factoryScheme(t)).
			WithObjects(append([]client.Object{b}, objs...)...).
			WithStatusSubresource(&certsv1alpha1.AliyunCertificateBinding{}).Build()
		return &AliyunCertificateBindingReconciler{Client: c, DriftCheckInterval: drift}
	}
	readyReason := func(t *testing.T, r *AliyunCertificateBindingReconciler) string {
		t.Helper()
		b := &certsv1alpha1.AliyunCertificateBinding{}
		if err := r.Get(context.Background(), req.NamespacedName, b); err != nil {
			t.Fatalf("读回对象失败: %v", err)
		}
		return bindingCondReason(b, certsv1alpha1.ConditionReady)
	}

	t.Run("证书 CR 不存在", func(t *testing.T) {
		r := fixture()
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if res != (ctrl.Result{RequeueAfter: drift}) {
			t.Errorf("应返回 drift 周期: %+v", res)
		}
		if got := readyReason(t, r); got != certsv1alpha1.ReasonCertificateNotFound {
			t.Errorf("Ready reason = %q", got)
		}
	})

	t.Run("证书尚未 Issued", func(t *testing.T) {
		r := fixture(&certsv1alpha1.AliyunCertificate{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "c1"},
		})
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if res != (ctrl.Result{RequeueAfter: drift}) {
			t.Errorf("应返回 drift 周期: %+v", res)
		}
		if got := readyReason(t, r); got != certsv1alpha1.ReasonCertificateNotReady {
			t.Errorf("Ready reason = %q", got)
		}
	})
}
