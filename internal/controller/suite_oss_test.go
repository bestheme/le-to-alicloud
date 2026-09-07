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
	"fmt"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

// 与 fakeFC3 同构，共用 suite_test.go 里的 fakeMu。
var fakeOSS *fake.OSS

// testOSSOwner 是 fake OSS 默认回报的账号。与 FC3 的 testAccountID 取不同值，账号 fencing
// 的用例里两者不会混。
const testOSSOwner = "9876543210"

func currentOSS() *fake.OSS { fakeMu.Lock(); defer fakeMu.Unlock(); return fakeOSS }

func resetOSS() {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fakeOSS = fake.NewOSS()
	fakeOSS.SetOwner(testOSSOwner)
}

// ossBucketFor 给每个 namespace 一个独占的 bucket 名：TargetKey 含 bucket，跨用例撞名会被
// 仲裁判成 Conflict。
func ossBucketFor(ns string) string { return "bucket-" + ns }

// createCertificateWithCAS 建一个**开着** CAS 上传的 AliyunCertificate。
//
// 与 createCertificate 刻意相反：OSS 按 certId 引用证书，没有 certId 就到不了 Apply。
// 代价是 suite_fc3_test.go 里说的那颗地雷——这张证书会在后续每个用例 resetCAS() 之后
// 发现「云上没有我」而重传。所以本 helper 用 DeferCleanup 在用例结束时把证书删掉
// （finalizer 走 fake CAS，瞬间完成），常驻证书集合里不留开着上传的对象。
// 调用方必须先删 Binding 再让这个 cleanup 跑：Binding 的 finalizer 会等证书。
//
//nolint:unused // Task 6 只测 3c 的 CASUploadRequired 分支（那一支要的是关着上传的证书），开着上传的这个由 Task 7 的 Apply 用例调用
func createCertificateWithCAS(ctx context.Context, ns, name string, dnsNames ...string) {
	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: dnsNames},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun-credentials"},
				Region:         "cn-hangzhou",
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ac)).To(Succeed())
	DeferCleanup(func() {
		got := &certsv1alpha1.AliyunCertificate{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, got)
		eventually(func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name},
				&certsv1alpha1.AliyunCertificate{}) != nil
		})
	})
}

// createOSSBinding 建一个指向 OSS CNAME 的 Binding，并登记 DeferCleanup 先于证书删除它。
func createOSSBinding(ctx context.Context, ns, name, certName, bucket, domain string) {
	b := &certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CertificateRef: certsv1alpha1.LocalObjectReference{Name: certName},
			Target: certsv1alpha1.BindingTarget{
				Type: certsv1alpha1.TargetTypeOSSCustomDomain,
				OSSCustomDomain: &certsv1alpha1.OSSCustomDomainTarget{
					Region: "cn-hangzhou", Bucket: bucket, DomainName: domain,
				},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
	// DeferCleanup 是 LIFO：本 helper 在 createCertificateWithCAS 之后调用，所以它登记的
	// 清理先跑——Binding 先于证书消失，证书的 finalizer 不会被 Binding 卡住。
	DeferCleanup(func() {
		got := &certsv1alpha1.AliyunCertificateBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, got)
		eventually(func() bool { return bindingGone(ctx, ns, name) })
	})
}

// ossDomainFor 与 FC3 用例同一命名纪律：<binding>.<ns>.example.com。
func ossDomainFor(ns, binding string) string { return fmt.Sprintf("%s.%s.example.com", binding, ns) }

// certRefOf 读证书 status.current.certId 拼出 OSS 侧应观测到的 certRef。
// 拼法必须与 provider.CertMaterial.CASCertRef() 一致（"<certId>-<casRegion>"）。
//
//nolint:unused // 只有走到 Apply 才有 certRef 可读；调用点是 Task 7 的 OSS Apply / 漂移用例
func certRefOf(ctx context.Context, ns, certName string) string {
	ac := getAC(ctx, ns, certName)
	if ac.Status.Current == nil || ac.Status.Current.CertID == nil {
		return ""
	}
	return strconv.FormatInt(*ac.Status.Current.CertID, 10) + "-" + ac.Spec.Aliyun.EffectiveCASRegion()
}
