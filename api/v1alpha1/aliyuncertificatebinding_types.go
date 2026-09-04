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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// AliyunCertificateBindingSpec defines the desired state of AliyunCertificateBinding.
type AliyunCertificateBindingSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of AliyunCertificateBinding. Edit aliyuncertificatebinding_types.go to remove/update
	Foo string `json:"foo,omitempty"`
}

// AliyunCertificateBindingStatus defines the observed state of AliyunCertificateBinding.
type AliyunCertificateBindingStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AliyunCertificateBinding is the Schema for the aliyuncertificatebindings API.
type AliyunCertificateBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AliyunCertificateBindingSpec   `json:"spec,omitempty"`
	Status AliyunCertificateBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AliyunCertificateBindingList contains a list of AliyunCertificateBinding.
type AliyunCertificateBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AliyunCertificateBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AliyunCertificateBinding{}, &AliyunCertificateBindingList{})
}
