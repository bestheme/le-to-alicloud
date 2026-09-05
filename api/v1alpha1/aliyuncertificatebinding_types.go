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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalObjectReference 引用同 namespace 的 AliyunCertificate。
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// DeletionPolicy 决定删除 Binding 时是否解绑云侧证书。
// +kubebuilder:validation:Enum=Orphan;Unbind
type DeletionPolicy string

const (
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
	DeletionPolicyUnbind DeletionPolicy = "Unbind"
)

// TargetTypeFC3CustomDomain 是第一个 provider 类型。
const TargetTypeFC3CustomDomain = "FC3CustomDomain"

// IndexBindingByCertificate 是 controller-runtime field index 的键名。
const IndexBindingByCertificate = "spec.certificateRef.name"

// IndexBindingByTarget 是同目标冲突仲裁用的 field index 键名，值为 TargetKey()。
const IndexBindingByTarget = "spec.target"

// FC3CustomDomainTarget 指向一个 FC3 自定义域名。证书归属于域名，与函数无关。
type FC3CustomDomainTarget struct {
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// +kubebuilder:validation:MinLength=1
	DomainName string `json:"domainName"`
	// 为 true 且域名当前 protocol 不含 HTTPS 时，才把 protocol 改为 "HTTP,HTTPS"。默认不动。
	// +optional
	EnsureHTTPSProtocol bool `json:"ensureHTTPSProtocol,omitempty"`
}

// BindingTarget 是 discriminated union：type 决定哪个内嵌块必须存在。
// +kubebuilder:validation:XValidation:rule="self.type == 'FC3CustomDomain' ? has(self.fc3CustomDomain) : !has(self.fc3CustomDomain)",message="fc3CustomDomain 必须且只能在 type=FC3CustomDomain 时设置"
type BindingTarget struct {
	// +kubebuilder:validation:Enum=FC3CustomDomain
	Type string `json:"type"`
	// +optional
	FC3CustomDomain *FC3CustomDomainTarget `json:"fc3CustomDomain,omitempty"`
}

// AliyunCertificateBindingSpec 定义期望状态。
type AliyunCertificateBindingSpec struct {
	CertificateRef LocalObjectReference `json:"certificateRef"`

	// 不可变：改目标请新建 Binding。
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target 不可变，请新建 Binding"
	Target BindingTarget `json:"target"`

	// 缺省继承 certificateRef 所指证书的 aliyun.credentialsRef。
	// +optional
	CredentialsRef *LocalSecretReference `json:"credentialsRef,omitempty"`

	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// AliyunCertificateBindingStatus 定义观测状态。
type AliyunCertificateBindingStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// 目标上实际生效的证书指纹。
	// +optional
	AppliedFingerprint string `json:"appliedFingerprint,omitempty"`
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
	// 首次成功 Apply 时固化，用于账号 fencing。
	// +optional
	BoundAccountID string `json:"boundAccountId,omitempty"`
	// 首次进入删除分支的时间，用于 --cleanup-grace-period 计时。
	// +optional
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="Applied")].status`
// +kubebuilder:printcolumn:name="Conflict",type=string,JSONPath=`.status.conditions[?(@.type=="Conflict")].status`
// +kubebuilder:printcolumn:name="Certificate",type=string,JSONPath=`.spec.certificateRef.name`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.fc3CustomDomain.domainName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AliyunCertificateBinding 把一张 AliyunCertificate 部署到一个阿里云目标。
type AliyunCertificateBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   AliyunCertificateBindingSpec   `json:"spec,omitempty"`
	Status AliyunCertificateBindingStatus `json:"status,omitempty"`
}

// TargetKey 返回 "<type>/<region>/<identifier>"，用于同目标冲突索引。
func (b *AliyunCertificateBinding) TargetKey() string {
	t := b.Spec.Target
	switch t.Type {
	case TargetTypeFC3CustomDomain:
		if t.FC3CustomDomain == nil {
			return ""
		}
		return t.Type + "/" + t.FC3CustomDomain.Region + "/" + t.FC3CustomDomain.DomainName
	default:
		return ""
	}
}

// +kubebuilder:object:root=true

// AliyunCertificateBindingList contains a list of AliyunCertificateBinding
type AliyunCertificateBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AliyunCertificateBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AliyunCertificateBinding{}, &AliyunCertificateBindingList{})
}
