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
	"strings"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

// materialError 是可直接写进 condition 的校验失败。
type materialError struct {
	Reason  string
	Message string
}

// loadBundle 直读 Secret（不经 cache）并解析成 Bundle。
//
// 证书 controller（loadMaterial）与绑定 controller（loadBindingMaterial）共用这一段：
// 两边对「读 Secret」这件事的契约必须逐字相同——同一个 Secret 名、同两个 data key、
// 同一组 reason 与 message。抄成两份的话，将来任何一次改动（加 Secret 类型校验、换
// data key、改写 message、新增一类 invalid 子情形）都会只落在一份里，而用户看到的是
// 两个 controller 对同一个故障给出不一样的 condition。
//
// 零凭证泄漏的护栏也因此只有这一处：pki 的错误只描述格式问题，不含密钥内容，可安全
// 写入 message。
func loadBundle(ctx context.Context, reader client.Reader,
	ac *certsv1alpha1.AliyunCertificate) (*pki.Bundle, *materialError) {
	s := &corev1.Secret{}
	name := secretNameFor(ac)
	err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, &materialError{certsv1alpha1.ReasonSecretNotFound, fmt.Sprintf("Secret %q 不存在", name)}
	}
	if err != nil {
		return nil, &materialError{certsv1alpha1.ReasonSecretInvalid, "读取 Secret 失败: " + err.Error()}
	}
	b, err := pki.ParseBundle(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, &materialError{certsv1alpha1.ReasonSecretInvalid, err.Error()}
	}
	return b, nil
}

// loadMaterial 直读 Secret（不经 cache），解析并校验。任何失败都不上传。
func loadMaterial(ctx context.Context, reader client.Reader, ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) (*pki.Bundle, *materialError) {
	b, me := loadBundle(ctx, reader, ac)
	if me != nil {
		return nil, me
	}
	if me := validateMaterial(b, ac, cert); me != nil {
		return nil, me
	}
	return b, nil
}

// validateMaterial 实施 spec §5.4 的规则 4–5（1–3 与 6 在 pki.ParseBundle 内完成）。
func validateMaterial(b *pki.Bundle, ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) *materialError {
	// 规则 5：临时证书 = 自签 AND cert-manager 正在签发。
	// 只用合取：SelfSigned issuer 的正式证书也是自签，但此时 Issuing=False，必须放行。
	if issuing, _ := certIssuingSince(cert); issuing && b.IsSelfSigned() {
		return &materialError{certsv1alpha1.ReasonSelfSignedDuringIssuance, "Secret 中是 cert-manager 的临时自签证书，等待正式签发"}
	}

	// 规则 4：leaf SANs 必须覆盖 spec 声明的全部域名。
	required := append([]string(nil), ac.Spec.CertificateTemplate.DNSNames...)
	if cn := ac.Spec.CertificateTemplate.CommonName; cn != "" {
		required = append(required, cn)
	}
	if missing := pki.Missing(b.DNSNames(), required); len(missing) > 0 {
		return &materialError{certsv1alpha1.ReasonSANsMismatch, "证书未覆盖: " + strings.Join(missing, ", ")}
	}
	return nil
}
