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

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

// 与 currentCAS / resetCAS 同构，共用 suite_test.go 里的 fakeMu。
var (
	bindingReconciler *AliyunCertificateBindingReconciler
	fakeFC3           *fake.FC3
	fc3FactoryErr     error
)

// testAccountID 是 fake FC3 默认回报的账号，用于账号 fencing 用例。
const testAccountID = "1234567890"

// currentFC3 让每个测试可以替换 fakeFC3 而 manager 无需重启。
func currentFC3() *fake.FC3 { fakeMu.Lock(); defer fakeMu.Unlock(); return fakeFC3 }

// resetFC3 换上一个全新的 fake，并清掉上一轮注入的工厂错误。
//
// 无返回值：没有任何调用点用得上它，留着会被 unparam 报出来（与 resetCAS 一致）。
// 需要拿到 fake 的地方一律用 currentFC3()。
func resetFC3() {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fakeFC3 = fake.NewFC3()
	fakeFC3.SetAccountID(testAccountID)
	fc3FactoryErr = nil
}

// setFC3FactoryErr 让 ProviderFactory 直接失败，用来测凭证分支。
func setFC3FactoryErr(err error) { fakeMu.Lock(); fc3FactoryErr = err; fakeMu.Unlock() }

func currentFC3FactoryErr() error { fakeMu.Lock(); defer fakeMu.Unlock(); return fc3FactoryErr }

// createCertificate 建一个最小可用的 AliyunCertificate，**关掉 CAS 上传**。
//
// 无返回值：没有任何调用点用得上它（与 resetFC3 同理），留着会被 unparam 报出来。
// 需要读回对象的地方用 getAC()。
//
// uploadToCAS=false 有两个理由，一个是语义的、一个是套件卫生的：
//
// 语义上这才是绑定侧该有的形态。FC3 内联 PEM（Capabilities.RequiresCASUpload=false），
// 一个只用 FC3 的用户完全可以不授 yundun-cert:*（spec §8.3）。绑定用例关心的是 Issued
// 与 status.current，从来不关心 CAS——status.current 在关掉上传时照样会写，只是没有
// certId，而 certificateGate 只比指纹。
//
// 套件卫生上，这是在拆掉一颗跨用例的地雷。envtest 从不回收 namespace，先前每个用例建的
// AliyunCertificate 都还活着；而 resetCAS() 每个用例换上一个**空的** CAS fake，于是任何
// 一个老证书被唤醒（绑定 status 每次 patch 都会经证书 controller 的 Binding watch 唤醒
// 它自己那张证书）都会发现「云上没有我的证书」而重传一次，撞进当前用例的
// currentCAS().UploadCalls() 里。Task 11 把十几张常驻证书加进套件之后，
// stall_test 的 UploadCalls()==1/==2 断言开始偶发翻车（实测约 1/20）。
// 关掉上传，这些证书就再也不是重传源——probe 与 retention 也都对它们直接早退。
func createCertificate(ctx context.Context, ns, name string, dnsNames ...string) {
	noUpload := false
	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: dnsNames},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun-credentials"},
				Region:         "cn-hangzhou",
				UploadToCAS:    &noUpload,
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ac)).To(Succeed())
}

// createBinding 建一个指向 FC3 自定义域名的 Binding。mutate 可为 nil。
//
// domain 一律由调用方显式传入，且必须带上 namespace：
// `fmt.Sprintf("%s.%s.example.com", bindingName, ns)`。仲裁是**跨 namespace** 按
// TargetKey() 检索的（spec §6.2），而 TargetKey() 只含 type/region/domainName，不含
// namespace——两个测试文件用同一个字面量域名，先建的那个 Binding 会一直把后建的判成
// Conflict，Applied 永远不为 True。helper 不给默认域名，就是为了逼调用方写出这一点。
//
// 无返回值：全部十四份 task brief 里都没有 `x := createBinding(...)`，这个返回值是
// 永久死的（与 createCertificate 同理）。需要读回对象的地方用 getBinding()。
//
// mutate 目前所有调用点都传 nil，Task 12 才会用它改 deletionPolicy / credentialsRef。
//
//nolint:unparam // mutate 恒为 nil，调用点由 Task 12 补上。
func createBinding(ctx context.Context, ns, name, certName, domain string,
	mutate func(*certsv1alpha1.AliyunCertificateBinding)) {
	b := &certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CertificateRef: certsv1alpha1.LocalObjectReference{Name: certName},
			Target: certsv1alpha1.BindingTarget{
				Type: certsv1alpha1.TargetTypeFC3CustomDomain,
				FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
					Region: "cn-hangzhou", DomainName: domain,
				},
			},
		},
	}
	if mutate != nil {
		mutate(b)
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
}

// bindingCond 读回一个 condition；不存在时返回零值。
func bindingCond(ctx context.Context, ns, name, condType string) metav1.Condition {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b); err != nil {
		return metav1.Condition{}
	}
	for _, c := range b.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

// getBinding 读回整个对象。
func getBinding(ctx context.Context, ns, name string) *certsv1alpha1.AliyunCertificateBinding {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b)).To(Succeed())
	return b
}
