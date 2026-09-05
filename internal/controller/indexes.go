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
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByCertificate, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok || b.Spec.CertificateRef.Name == "" {
				return nil
			}
			return []string{b.Spec.CertificateRef.Name}
		}); err != nil {
		return err
	}
	// 同目标索引。空键必须跳过而不是索引成 ""：TargetKey() 对未知 type 或缺失内嵌块
	// 返回空串，把它们全都索引到同一个键上，会让一批毫无关系的 Binding 互相判定冲突。
	// CRD 的 CEL 已经挡住了这些形状，但索引函数在 cache 层运行，先于任何校验。
	return idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByTarget, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok {
				return nil
			}
			key := b.TargetKey()
			if key == "" {
				return nil
			}
			return []string{key}
		})
}
