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
	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// certIdentity 抹平「按指纹」与「按 certRef」两种对账方式（spec 2026-09-07 §5.1）。
//
// 内联 PEM 的目标（FC3）三个值都是 SHA-256 指纹；按 certId 引用的目标（OSS）三个值都是
// "<certId>-<casRegion>"。通用层对「目标上是哪张证书」的一切比对——幂等短路、漂移判定、
// 「只解绑自己的」——都只看这三个字段，不再直接读 obs.CurrentFingerprint / CurrentCertRef。
type certIdentity struct {
	want    string // 本轮该在目标上的
	current string // 目标上实际的；"" = 无证书
	applied string // 上次我们写的；"" = 从没写成过
	byRef   bool   // true = 三个值是 certRef，false = 指纹
}

// capabilitiesOf 查注册表取 provider 的能力位；没注册的类型返回零值（等于「内联 PEM、
// 不强制上传」的最保守形状），由步骤 4 的工厂错误路径去报「未知 target.type」。
func capabilitiesOf(b *certsv1alpha1.AliyunCertificateBinding) provider.Capabilities {
	if p, ok := provider.Get(b.Spec.Target.Type); ok {
		return p.Capabilities()
	}
	return provider.Capabilities{}
}

// identityOf 按 provider 的能力位构造三元组。m 可为零值（删除分支不需要 want）。
func identityOf(
	b *certsv1alpha1.AliyunCertificateBinding, obs provider.ObservedState, m provider.CertMaterial,
) certIdentity {
	if capabilitiesOf(b).ReferencesCertByID {
		return certIdentity{want: m.CASCertRef(), current: obs.CurrentCertRef, applied: b.Status.AppliedCertRef, byRef: true}
	}
	return certIdentity{want: m.Fingerprint, current: obs.CurrentFingerprint, applied: b.Status.AppliedFingerprint}
}

// identityLabel 把三元组里的一个值折成可以进日志的形状：指纹只留前 8 位（与 shortFP 一致），
// certRef 本身不敏感也不长，原样输出。
func identityLabel(id certIdentity, v string) string {
	if id.byRef {
		return v
	}
	return shortFP(v)
}

// ensureHTTPSOf 读 ensureHTTPSProtocol。**必须 nil-safe**，且只有 FC3 有这个开关：
// 其它类型恒 false，于是 protocolSatisfied 对它们永远满足，短路判定只剩身份比对。
func ensureHTTPSOf(b *certsv1alpha1.AliyunCertificateBinding) bool {
	if fc := b.Spec.Target.FC3CustomDomain; fc != nil {
		return fc.EnsureHTTPSProtocol
	}
	return false
}

// 步骤 3c 的两条固定文案。Message 不含变量（spec §10.2），变量只进日志。
const (
	casUploadRequiredMessage = "目标类型要求证书上传到 CAS，请把 AliyunCertificate 的 spec.aliyun.uploadToCAS 设为 true"
	casCertIDPendingMessage  = "证书尚未取得 CAS certId"
)

// casUploadGate 是 spec 2026-09-07 §5.2 的步骤 3c：按 certId 引用证书的 provider
// （RequiresCASUpload）在两种情形下不能写云。
//
//   - 证书关掉了上传：这是配置错误，Binding 不替证书开上传（D21），报 CASUploadRequired
//     等人改证书 spec。
//   - 这一代还没传完 CAS：m.CertID 只在 status.current 的指纹与 Secret 一致**且**上传成功
//     后才非 nil（loadBindingMaterial），所以这里同时覆盖了「续期后新代次已进 Secret、
//     CAS 还没传完」的窗口。沿用 CertificateNotReady，与 certificateGate 同一节奏重试。
//
// reason 为空表示放行。两条早退都不碰 appliedFingerprint（失败与旁路早退不写 applied）。
func casUploadGate(
	caps provider.Capabilities, ac *certsv1alpha1.AliyunCertificate, m provider.CertMaterial,
) (reason, message string) {
	if !caps.RequiresCASUpload {
		return "", ""
	}
	if !ac.Spec.Aliyun.UploadEnabled() {
		return certsv1alpha1.ReasonCASUploadRequired, casUploadRequiredMessage
	}
	if m.CertID == nil {
		return certsv1alpha1.ReasonCertificateNotReady, casCertIDPendingMessage
	}
	return "", ""
}
