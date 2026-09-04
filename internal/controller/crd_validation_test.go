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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

var _ = Describe("AliyunCertificate CRD 校验", func() {
	ctx := context.Background()

	newAC := func(name string) *certsv1alpha1.AliyunCertificate {
		return &certsv1alpha1.AliyunCertificate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: certsv1alpha1.AliyunCertificateSpec{
				CertificateTemplate: certsv1alpha1.CertificateTemplate{
					DNSNames: []string{"api.example.com"},
				},
				Aliyun: certsv1alpha1.AliyunSpec{
					CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun"},
					Region:         "cn-hangzhou",
				},
			},
		}
	}

	It("应用 retention 与 uploadToCAS 的默认值", func() {
		ac := newAC("defaults")
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		got := &certsv1alpha1.AliyunCertificate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "defaults", Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Retention.KeepLast).To(Equal(int32(2)))
		Expect(got.Spec.Retention.MinAge).NotTo(BeNil())
		Expect(got.Spec.Retention.MinAge.Duration.Hours()).To(Equal(24.0))
		Expect(got.Spec.Aliyun.UploadToCAS).NotTo(BeNil())
		Expect(*got.Spec.Aliyun.UploadToCAS).To(BeTrue())
	})

	It("拒绝 keepLast < 1", func() {
		ac := newAC("keeplast-zero")
		// keepLast=0 会被 omitempty 省略进而被默认成 2，所以用 -1 触发 minimum
		ac.Spec.Retention.KeepLast = -1
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})

	It("拒绝 dnsNames 与 commonName 都为空", func() {
		ac := newAC("no-names")
		ac.Spec.CertificateTemplate.DNSNames = nil
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})

	It("拒绝缺少 credentialsRef.name", func() {
		ac := newAC("no-creds")
		ac.Spec.Aliyun.CredentialsRef.Name = ""
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})
})
