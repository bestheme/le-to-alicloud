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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"

	// 让 fc3 的 init() 把自己注册进 registry。除注册外本包不直接引用它——
	// 这正是 provider 抽象的意义：新增 provider = 加一个包 + 一条 CEL，不动状态机。
	_ "git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"
)

// credentialsSecretNameFor 实现凭证继承：Binding 自己的 credentialsRef 优先，
// 缺省回退到证书的 aliyun.credentialsRef（spec §6.2 步骤 4）。
//
// 两者都在 Binding 所在的 namespace（CRD 里没有 namespace 字段，用类型系统禁止跨
// namespace 引用）。ac 允许为 nil：删除分支里证书可能已经先被删掉了。
func credentialsSecretNameFor(b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate) string {
	if b.Spec.CredentialsRef != nil && b.Spec.CredentialsRef.Name != "" {
		return b.Spec.CredentialsRef.Name
	}
	if ac != nil {
		return ac.Spec.Aliyun.CredentialsRef.Name
	}
	return ""
}

// targetOf 把 CRD 的 discriminated union 摊平成 provider 无关的 Target。
func targetOf(b *certsv1alpha1.AliyunCertificateBinding) (provider.Target, error) {
	switch b.Spec.Target.Type {
	case certsv1alpha1.TargetTypeFC3CustomDomain:
		fc := b.Spec.Target.FC3CustomDomain
		if fc == nil {
			return provider.Target{}, fmt.Errorf("target.type=%s 但缺少 fc3CustomDomain", b.Spec.Target.Type)
		}
		return provider.Target{
			Type:       b.Spec.Target.Type,
			Region:     fc.Region,
			Identifier: fc.DomainName,
			Spec:       fc,
		}, nil
	default:
		return provider.Target{}, fmt.Errorf("未知的 target.type %q，已注册: %v", b.Spec.Target.Type, provider.Names())
	}
}

// NewProviderFactory 构造生产用 ProviderFactory。
//
// 与 NewCASFactory 同构：读同 namespace 的凭证 Secret（Secret 已从 cache 禁用，
// mgr.GetClient() 对它就是直读），按 resourceVersion 缓存 client——否则轮换后的 AK
// 直到 Pod 重启才生效。
func NewProviderFactory(reader client.Reader, cache *aliyun.ClientCache[aliyun.FC3Client],
	limiters *aliyun.Limiters, timeout time.Duration) ProviderFactory {
	return func(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding,
		ac *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error) {
		tg, err := targetOf(b)
		if err != nil {
			return nil, nil, err
		}
		p, ok := provider.Get(tg.Type)
		if !ok {
			return nil, nil, fmt.Errorf("没有注册 target.type %q 的 provider，已注册: %v", tg.Type, provider.Names())
		}

		secretName := credentialsSecretNameFor(b, ac)
		if secretName == "" {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound,
				fmt.Errorf("既未设置 spec.credentialsRef，也取不到证书的 aliyun.credentialsRef")}
		}
		s := &corev1.Secret{}
		gerr := reader.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: secretName}, s)
		if apierrors.IsNotFound(gerr) {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound,
				fmt.Errorf("凭证 Secret %q 不存在", secretName)}
		}
		if gerr != nil {
			return nil, nil, gerr
		}
		creds, cerr := aliyun.CredentialsFromSecret(s)
		if cerr != nil {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, cerr}
		}

		// key 必须囊括 build 闭包里读到的每一个会改变 client 行为的字段。
		// Endpoint 恒为空：AliyunCertificate 的 endpointOverride 是给 CAS 用的，
		// 把它套到 FC3 上会把请求打到数字证书服务的地址去。FC3 的 endpoint 一律由
		// SDK 按 region 选出（fcv3.<region>.aliyuncs.com）。
		key := aliyun.ClientKey{
			Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion,
			Region: tg.Region,
		}
		cl, berr := cache.GetOrBuild(key, func() (aliyun.FC3Client, error) {
			cred, err := creds.Build()
			if err != nil {
				return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
			}
			// 一律读 key 而不是 tg：缓存命中与否只由 key 决定，闭包里再从别处取值
			// 就等于把没进 key 的字段偷偷带进 client。
			return aliyun.NewFC3Client(cred, aliyun.FC3ClientConfig{
				Region:     key.Region,
				Timeout:    timeout,
				Limiters:   limiters,
				LimiterKey: creds.LimiterKey(),
				OnCall:     aliyunAPICallRecorder(serviceFC3),
			})
		})
		if berr != nil {
			return nil, nil, berr
		}
		return p, cl, nil
	}
}
