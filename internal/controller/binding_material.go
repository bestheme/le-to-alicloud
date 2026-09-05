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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// certificateGateRequeue 是「Secret 已换、证书 CR 还没追上」时的等待间隔。
// 短到人察觉不出延迟，又不至于把 API server 打成忙音。
const certificateGateRequeue = 30 * time.Second

// loadBindingMaterial 组装要写进云的证书材料。
//
// 内容只来自 Secret，不来自 status（spec §3「写入内容的确定性」）：多副本短暂重叠时，
// 最坏情况是两次写入同样的字节，而不是把 A 的指纹配上 B 的 PEM。
//
// status.current 只用来补两样 Secret 里没有的东西：CAS 的 certId 与证书名。而且只在
// 指纹对得上时才用——续期在途时两者会短暂不一致，此时按 Secret 的指纹重新派生名字，
// 绝不把上一代的 certId 贴到新证书上。那种不一致的材料也不会被写出去，见 certificateGate。
//
// reader 必须是能直读 Secret 的 client（manager client 对 corev1.Secret 关了 cache），
// 全程只 Get 单个对象、绝不 List。
func loadBindingMaterial(
	ctx context.Context, reader client.Reader, ac *certsv1alpha1.AliyunCertificate,
) (provider.CertMaterial, *materialError) {
	var m provider.CertMaterial

	s := &corev1.Secret{}
	name := secretNameFor(ac)
	err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return m, &materialError{certsv1alpha1.ReasonSecretNotFound, fmt.Sprintf("Secret %q 不存在", name)}
	}
	if err != nil {
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, "读取 Secret 失败: " + err.Error()}
	}
	b, perr := pki.ParseBundle(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if perr != nil {
		// pki 的错误只描述格式问题，不含密钥内容，可安全写进 condition。
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, perr.Error()}
	}
	keyPEM, kerr := b.KeyPEM()
	if kerr != nil {
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, kerr.Error()}
	}

	// spec §12.3 #1：未实测（KeyPEM 是 PKCS#1（RSA）/ SEC1（ECDSA），FC3 是否要求
	// PKCS#8 没有实测过），实测结论见 test/integration/RESULTS.md
	// spec §12.3 #5：未实测（CertPEM 是 leaf + intermediates、无根、无空行的 LE 链，
	// FC3 是否要求带根、是否对顺序敏感没有实测过），实测结论见 test/integration/RESULTS.md
	//
	// 这两条猜错的失效方式都是静默的：UpdateCustomDomain 直接返回一个参数类错误，
	// 被归成 Permanent，域名上仍是旧证书。
	m = provider.CertMaterial{
		Fingerprint: b.Fingerprint,
		CertPEM:     b.CertPEM(),
		KeyPEM:      keyPEM,
		CASName:     naming.CASName(ac.Name, b.Fingerprint),
		NotAfter:    b.Leaf.NotAfter,
		DNSNames:    b.DNSNames(),
	}
	if cur := ac.Status.Current; cur != nil && cur.Fingerprint == b.Fingerprint {
		m.CertID = cur.CertID
		if cur.CASName != "" {
			m.CASName = cur.CASName
		}
	}
	return m, nil
}

// requiredDomainsOf 返回目标要求证书必须覆盖的域名。
//
// 认不出的 target 类型返回空：宁可不校验，也不能凭空造出一个域名去比对——那会把一次
// 配置错误变成「证书不覆盖域名」的误报。真正的未知类型由 CRD 的枚举与 provider
// 注册表拦下。
func requiredDomainsOf(b *certsv1alpha1.AliyunCertificateBinding) []string {
	if b.Spec.Target.Type == certsv1alpha1.TargetTypeFC3CustomDomain && b.Spec.Target.FC3CustomDomain != nil {
		return []string{b.Spec.Target.FC3CustomDomain.DomainName}
	}
	return nil
}

// checkDomainCoverage 实施 spec §6.2 步骤 3：leaf SANs 必须按 RFC 6125 覆盖目标域名。
//
// 硬失败、不快速重试：推错证书会让整个域名的 TLS 报错（spec D17）。这不是「等一等就会
// 好」的故障，只有改 spec 才能修，所以退避再快也只是白烧配额。
func checkDomainCoverage(m provider.CertMaterial, b *certsv1alpha1.AliyunCertificateBinding) *materialError {
	required := requiredDomainsOf(b)
	if len(required) == 0 {
		return nil
	}
	if missing := pki.Missing(m.DNSNames, required); len(missing) > 0 {
		return &materialError{
			certsv1alpha1.ReasonDomainNotCovered,
			"证书未覆盖目标域名: " + strings.Join(missing, ", "),
		}
	}
	return nil
}

// certificateGate 判断证书 CR 是否已经认下 Secret 里的这一代。
//
// 写入内容由 Secret 决定（spec §3），但**要不要写这一次由证书 CR 说了算**。
// status.current 推进意味着证书 controller 已经跑完 spec §5.4 的全套校验：链连续、
// 公私钥匹配、不是签发中的临时自签、SANs 覆盖 spec 声明的域名。
//
// 续期在途时，cert-manager 先把新证书写进 Secret，证书 controller 稍后才校验并推进
// status.current。Binding 直读 Secret，天然比证书 CR 早一步看到新指纹——此刻抢先写云，
// 等于绕过那套校验把一张没人验过的证书推上生产 HTTPS。cert-manager 的临时自签证书正是
// 这样一张：它会先出现在 Secret 里，而证书 controller 会拒绝它。
//
// 所以不一致时就等：30 秒后再看，证书 controller 通常一轮就追平了。
func certificateGate(m provider.CertMaterial, ac *certsv1alpha1.AliyunCertificate) bool {
	cur := ac.Status.Current
	return cur != nil && cur.Fingerprint == m.Fingerprint
}
