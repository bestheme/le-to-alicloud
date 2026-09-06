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
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// certManagerCertificateNameAnnotation 是 cert-manager 打在它写的 Secret 上的来源
// Certificate 名。集群探针 #11（test/integration/RESULTS.md:26）核实过它稳定存在，
// 归属判定就建立在这一条上。
const certManagerCertificateNameAnnotation = "cert-manager.io/certificate-name"

// secretNameFor 返回 cert-manager 写入的 Secret 名。
func secretNameFor(ac *certsv1alpha1.AliyunCertificate) string {
	if ac.Spec.SecretName != "" {
		return ac.Spec.SecretName
	}
	return ac.Name + "-tls"
}

// certManagerNameFor 返回 operator 创建的 cmapi.Certificate 名（与 CR 同名）。
func certManagerNameFor(ac *certsv1alpha1.AliyunCertificate) string { return ac.Name }

// secretOwnedByUs 判断一个已经存在的 Secret 是不是本 CR 的 Certificate 产出的。
//
// 判据只有一条：cert-manager 写 Secret 时打的来源注解逐字等于本 CR 的 Certificate 名。
// 注解缺失（人工建的 Secret）与注解指向别的 Certificate 一样，都不算我们的——这正是
// secretNameConflict 一直以来的语义，抽出来是为了让删除期能用同一份判断。
func secretOwnedByUs(s *corev1.Secret, ac *certsv1alpha1.AliyunCertificate) bool {
	return s.Annotations[certManagerCertificateNameAnnotation] == certManagerNameFor(ac)
}

// desiredCertificateSpec 从 CertificateTemplate 构造 cert-manager 的期望态。
// issuer 已由 ResolveIssuerRef 决定（含 Pin），这里只做透传与两处默认：PKCS1、managed label。
func desiredCertificateSpec(ac *certsv1alpha1.AliyunCertificate, issuer cmmeta.IssuerReference) cmapi.CertificateSpec {
	t := ac.Spec.CertificateTemplate
	spec := cmapi.CertificateSpec{
		SecretName:  secretNameFor(ac),
		IssuerRef:   NormalizeIssuerRef(issuer),
		CommonName:  t.CommonName,
		DNSNames:    append([]string(nil), t.DNSNames...),
		IPAddresses: append([]string(nil), t.IPAddresses...),
		Duration:    t.Duration.DeepCopy(),
		RenewBefore: t.RenewBefore.DeepCopy(),
		Subject:     t.Subject.DeepCopy(),
		Usages:      append([]cmapi.KeyUsage(nil), t.Usages...),
	}

	pk := &cmapi.CertificatePrivateKey{}
	if t.PrivateKey != nil {
		pk = t.PrivateKey.DeepCopy()
	}
	if pk.Encoding == "" {
		pk.Encoding = cmapi.PKCS1
	}
	spec.PrivateKey = pk

	st := &cmapi.CertificateSecretTemplate{}
	if t.SecretTemplate != nil {
		st = t.SecretTemplate.DeepCopy()
	}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	st.Labels[certsv1alpha1.LabelManaged] = "true"
	spec.SecretTemplate = st
	return spec
}
