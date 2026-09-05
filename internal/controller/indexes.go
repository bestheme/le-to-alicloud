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
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// RegisterIndexes 注册所有 field index。必须且只能调用一次（main.go 与测试 suite 各一次）。
func RegisterIndexes(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByCertificate, bindingCertificateIndex); err != nil {
		return err
	}
	return idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByTarget, bindingTargetIndex)
}

// bindingCertificateIndex 按 spec.certificateRef.name 索引，供证书变化时反查 Binding。
func bindingCertificateIndex(o client.Object) []string {
	b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
	if !ok || b.Spec.CertificateRef.Name == "" {
		return nil
	}
	return []string{b.Spec.CertificateRef.Name}
}

// bindingTargetIndex 按 TargetKey() 索引，供同目标冲突仲裁使用。
//
// 空键必须跳过而不是索引成 ""：TargetKey() 对未知 type 或缺失内嵌块返回空串，把它们全都
// 索引到同一个键上，会让一批毫无关系的 Binding 互相判定冲突。CRD 的 CEL 已经挡住了这些
// 形状，但索引函数在 cache 层运行，先于任何校验——所以这里必须自己扛住。
// 提成具名函数是为了能被直接断言：闭包没法在用例里调用。
func bindingTargetIndex(o client.Object) []string {
	b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
	if !ok {
		return nil
	}
	key := b.TargetKey()
	if key == "" {
		return nil
	}
	return []string{key}
}
