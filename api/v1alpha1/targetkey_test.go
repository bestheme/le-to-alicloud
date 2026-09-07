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
package v1alpha1

import "testing"

func TestTargetKey(t *testing.T) {
	cases := []struct {
		name string
		b    AliyunCertificateBinding
		want string
	}{
		{"fc3", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type:            TargetTypeFC3CustomDomain,
			FC3CustomDomain: &FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: "api.example.com"},
		}}}, "FC3CustomDomain/cn-hangzhou/api.example.com"},
		{"oss", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type:            TargetTypeOSSCustomDomain,
			OSSCustomDomain: &OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: "b1", DomainName: "www.example.com"},
		}}}, "OSSCustomDomain/cn-hangzhou/b1/www.example.com"},
		{"oss 缺内嵌块", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type: TargetTypeOSSCustomDomain,
		}}}, ""},
		{"未知类型", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{Type: "Nope"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.TargetKey(); got != tc.want {
				t.Errorf("TargetKey() = %q, want %q", got, tc.want)
			}
		})
	}
}
