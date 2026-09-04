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

package v1alpha1

import (
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalSecretReference 只允许引用同 namespace 的 Secret；故意不提供 namespace 字段。
type LocalSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// CertificateTemplate 是透传给 cert-manager Certificate 的字段子集。
// 顶层字段由本项目挑选，嵌套类型复用 cert-manager 的定义。
// +kubebuilder:validation:XValidation:rule="(has(self.dnsNames) && size(self.dnsNames) > 0) || (has(self.commonName) && self.commonName != \"\")",message="certificateTemplate 至少需要 dnsNames 或 commonName 之一"
type CertificateTemplate struct {
	// 缺省时回退到 operator 的 --default-issuer-* flag；首次生效后被固化到 status.effectiveIssuerRef。
	// +optional
	IssuerRef *cmmeta.IssuerReference `json:"issuerRef,omitempty"`
	// +optional
	CommonName string `json:"commonName,omitempty"`
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`
	// +optional
	IPAddresses []string `json:"ipAddresses,omitempty"`
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
	// +optional
	RenewBefore *metav1.Duration `json:"renewBefore,omitempty"`
	// +optional
	Subject *cmapi.X509Subject `json:"subject,omitempty"`
	// +optional
	Usages []cmapi.KeyUsage `json:"usages,omitempty"`
	// 缺省 encoding 为 PKCS1（阿里云 CAS / FC3 / CDN 三处文档一致要求）。
	// +optional
	PrivateKey *cmapi.CertificatePrivateKey `json:"privateKey,omitempty"`
	// +optional
	SecretTemplate *cmapi.CertificateSecretTemplate `json:"secretTemplate,omitempty"`
}

// AliyunSpec 描述阿里云侧的账号与位置。
type AliyunSpec struct {
	CredentialsRef LocalSecretReference `json:"credentialsRef"`
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// CAS 的 region；缺省等于 region。SDK 按 region 自动选择 endpoint（cn-* 全部落到 cas.aliyuncs.com）。
	// +optional
	CASRegion string `json:"casRegion,omitempty"`
	// 非空时覆盖 SDK 选出的 endpoint（VPC / 专有云）。
	// +optional
	EndpointOverride string `json:"endpointOverride,omitempty"`
	// +optional
	ResourceGroupID string `json:"resourceGroupId,omitempty"`
	// 为 false 时跳过 CAS 上传 / 回收 / 探测，Uploaded 不参与 Ready 聚合。
	// +kubebuilder:default=true
	// +optional
	UploadToCAS *bool `json:"uploadToCAS,omitempty"`
}

// UploadEnabled 把可选指针折叠成布尔，默认 true。
func (a AliyunSpec) UploadEnabled() bool {
	return a.UploadToCAS == nil || *a.UploadToCAS
}

// EffectiveCASRegion 返回 CAS 调用使用的 region。
func (a AliyunSpec) EffectiveCASRegion() string {
	if a.CASRegion != "" {
		return a.CASRegion
	}
	return a.Region
}

// RetentionSpec 控制 CAS 中保留多少代证书。
type RetentionSpec struct {
	// current + history 的总代数。
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	KeepLast int32 `json:"keepLast,omitempty"`
	// 代次自上传起未满 minAge 不回收。
	// +kubebuilder:default="24h"
	// +optional
	MinAge *metav1.Duration `json:"minAge,omitempty"`
}

const defaultMinAge = 24 * time.Hour

// MinAgeOrDefault 在 API server 未做默认（如单元测试直接构造对象）时兜底。
func (r RetentionSpec) MinAgeOrDefault() time.Duration {
	if r.MinAge == nil {
		return defaultMinAge
	}
	return r.MinAge.Duration
}

// KeepLastOrDefault 同上。
func (r RetentionSpec) KeepLastOrDefault() int {
	if r.KeepLast < 1 {
		return 2
	}
	return int(r.KeepLast)
}

// AliyunCertificateSpec 定义期望状态。
type AliyunCertificateSpec struct {
	// cert-manager 写入证书的 Secret 名；缺省 "<metadata.name>-tls"。
	// +optional
	SecretName string `json:"secretName,omitempty"`

	CertificateTemplate CertificateTemplate `json:"certificateTemplate"`

	Aliyun AliyunSpec `json:"aliyun"`

	// +kubebuilder:default={}
	// +optional
	Retention RetentionSpec `json:"retention,omitempty"`
}

// CertificateGeneration 是一代已上传（或已观测）的证书；不含 PEM。
type CertificateGeneration struct {
	// SHA-256(leaf DER)，小写 hex。
	Fingerprint string `json:"fingerprint"`
	// uploadToCAS=false 时为空。
	// +optional
	CertID *int64 `json:"certId,omitempty"`
	// +optional
	CASName    string      `json:"casName,omitempty"`
	NotBefore  metav1.Time `json:"notBefore"`
	NotAfter   metav1.Time `json:"notAfter"`
	UploadedAt metav1.Time `json:"uploadedAt"`
}

// PendingUpload 是 CAS 上传的 write-ahead 记录，成功后清空。
type PendingUpload struct {
	Fingerprint string      `json:"fingerprint"`
	CASName     string      `json:"casName"`
	ClientToken string      `json:"clientToken"`
	StartedAt   metav1.Time `json:"startedAt"`
}

// IssuanceStatus 镜像 cert-manager Certificate 的关键 status 字段。
type IssuanceStatus struct {
	// +optional
	Revision *int `json:"revision,omitempty"`
	// +optional
	RenewalTime *metav1.Time `json:"renewalTime,omitempty"`
	// +optional
	FailedIssuanceAttempts *int `json:"failedIssuanceAttempts,omitempty"`
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
}

// AliyunCertificateStatus 定义观测状态。
type AliyunCertificateStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// 实际生效并被固化（Pin）的 issuer。
	// +optional
	EffectiveIssuerRef *cmmeta.IssuerReference `json:"effectiveIssuerRef,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	CertManagerCertificateName string `json:"certManagerCertificateName,omitempty"`
	// +optional
	Issuance *IssuanceStatus `json:"issuance,omitempty"`
	// +optional
	Current *CertificateGeneration `json:"current,omitempty"`
	// 最多 keepLast-1 条。
	// +optional
	History []CertificateGeneration `json:"history,omitempty"`
	// +optional
	PendingUpload *PendingUpload `json:"pendingUpload,omitempty"`
	// +optional
	CASProbedAt *metav1.Time `json:"casProbedAt,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Issued",type=string,JSONPath=`.status.conditions[?(@.type=="Issued")].status`
// +kubebuilder:printcolumn:name="Uploaded",type=string,JSONPath=`.status.conditions[?(@.type=="Uploaded")].status`
// +kubebuilder:printcolumn:name="Effective-Issuer",type=string,JSONPath=`.status.effectiveIssuerRef.name`
// +kubebuilder:printcolumn:name="Not-After",type=string,JSONPath=`.status.current.notAfter`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AliyunCertificate 描述一张由 cert-manager 签发并托管到阿里云 CAS 的证书。
type AliyunCertificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AliyunCertificateSpec   `json:"spec,omitempty"`
	Status AliyunCertificateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AliyunCertificateList contains a list of AliyunCertificate
type AliyunCertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AliyunCertificate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AliyunCertificate{}, &AliyunCertificateList{})
}
