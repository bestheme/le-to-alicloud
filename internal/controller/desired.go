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
	"fmt"

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

// servingSecretName 返回当前真正在服役的那个 Secret 名。
//
// effective 是调用方手上「已经生效」的那个值：证书 controller 传 Certificate 的
// spec.secretName，绑定 controller 传 status.secretName（两者都只在 CreateOrUpdate 成功
// 之后才有值）。为空说明这个 CR 还没走完第一轮，此时才回落到 spec 推导出来的名字。
//
// 不直接用 secretNameFor：spec.secretName 是用户随时可改的字段，而改动要过
// SecretNameConflict 护栏才会落到 Certificate 上。护栏拦住期间两者不一致，按 spec 去读
// 就是读别人的 Secret。
func servingSecretName(ac *certsv1alpha1.AliyunCertificate, effective string) string {
	if effective != "" {
		return effective
	}
	return secretNameFor(ac)
}

// secretNameConflictMessage 是 SecretNameConflict 的固定措辞。护栏判定处与 aggregateReady
// 的一票否决处各用一次，抽出来是为了两处永远说同一句话。
func secretNameConflictMessage(ac *certsv1alpha1.AliyunCertificate) string {
	return fmt.Sprintf("Secret %q 已存在且不属于本证书", secretNameFor(ac))
}

// secretNameGuardHolding 报告 SecretNameConflict 护栏此刻是否正拦着一次 spec.secretName
// 变更——也就是「用户想要的 Secret 名」与「真正在服役的那个」不一致。
//
// 判据是 status.secretName：它**只**在 Reconcile 步骤 3 的 CreateOrUpdate 成功之后被赋值
// （`ac.Status.SecretName = cert.Spec.SecretName`），所以护栏放行时它会在同一轮里被拉平，
// 只有拦住的时候才会与 spec 分叉。对象刚建出来、还没走完第一轮时它是空的，不算分叉。
//
// **改动 status.secretName 的赋值位置会悄悄破坏这个判据。** 它是 aggregateReady 施加
// 一票否决的唯一依据：冲突期间 operator 照常维护在役证书，Issued / Uploaded 会正常变成
// True，只有这一条能拦住 Ready 跟着变成 True。
func secretNameGuardHolding(ac *certsv1alpha1.AliyunCertificate) bool {
	return ac.Status.SecretName != "" && ac.Status.SecretName != secretNameFor(ac)
}

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
