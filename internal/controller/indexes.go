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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// RegisterIndexes 注册所有 field index。必须且只能调用一次（main.go 与测试 suite 各一次）。
func RegisterIndexes(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByCertificate, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok || b.Spec.CertificateRef.Name == "" {
				return nil
			}
			return []string{b.Spec.CertificateRef.Name}
		})
}
