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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

var _ = Describe("AliyunCertificateBinding CRD 校验", func() {
	ctx := context.Background()

	var ns string
	BeforeEach(func() { ns = newNamespace(ctx) })

	// 域名一律 <name>.<namespace>.example.com：TargetKey() 不含 namespace，跨 namespace
	// 撞名的 fixture 会被同目标仲裁判成 Conflict。
	newBinding := func(name string) *certsv1alpha1.AliyunCertificateBinding {
		return &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "cert"},
				Target: certsv1alpha1.BindingTarget{
					Type: certsv1alpha1.TargetTypeFC3CustomDomain,
					FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
						Region: "cn-hangzhou", DomainName: fmt.Sprintf("%s.%s.example.com", name, ns),
					},
				},
			},
		}
	}

	It("默认 deletionPolicy 为 Orphan", func() {
		b := newBinding("defaults")
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		got := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "defaults", Namespace: ns}, got)).To(Succeed())
		Expect(got.Spec.DeletionPolicy).To(Equal(certsv1alpha1.DeletionPolicyOrphan))
		Expect(got.TargetKey()).To(Equal(fmt.Sprintf("FC3CustomDomain/cn-hangzhou/defaults.%s.example.com", ns)))
	})

	It("拒绝 type=FC3CustomDomain 但缺少 fc3CustomDomain", func() {
		b := newBinding("union-missing")
		b.Spec.Target.FC3CustomDomain = nil
		Expect(k8sClient.Create(ctx, b)).To(MatchError(ContainSubstring(
			"spec.target: Invalid value: fc3CustomDomain 必须且只能在 type=FC3CustomDomain 时设置")))
	})

	It("target 不可变", func() {
		Expect(k8sClient.Create(ctx, newBinding("immutable"))).To(Succeed())
		// 不能拿创建时那一份直接改：Binding reconciler 一唤醒就会给新对象补 finalizer，
		// 那次 Update 推进 resourceVersion，于是这里的写入撞上 409 Conflict 而不是 CEL 的
		// 不可变错误，用例随机翻车（实测约 1/9）。重读一份也不够——重读与 Update 之间
		// 同样能插进 finalizer 那一次写——所以整段放进 Eventually：冲突就重来，咬定
		// 「最终报出来的是不可变」。放宽的只有时机，断言的内容一个字没动：真让 target
		// 可改了，Update 会成功、返回 nil，匹配照样失败。
		Eventually(func() error {
			got := &certsv1alpha1.AliyunCertificateBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "immutable", Namespace: ns}, got); err != nil {
				return err
			}
			got.Spec.Target.FC3CustomDomain.DomainName = fmt.Sprintf("other.%s.example.com", ns)
			return k8sClient.Update(ctx, got)
		}, "10s", "100ms").Should(MatchError(ContainSubstring(
			"spec.target: Invalid value: target 不可变，请新建 Binding")))
	})

	It("拒绝未知 deletionPolicy", func() {
		b := newBinding("bad-policy")
		b.Spec.DeletionPolicy = "Delete"
		Expect(k8sClient.Create(ctx, b)).To(MatchError(ContainSubstring(
			"spec.deletionPolicy: Unsupported value: \"Delete\": supported values: \"Orphan\", \"Unbind\"")))
	})
})
