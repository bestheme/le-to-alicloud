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
)

// credentialsError 让调用方区分「凭证 Secret 不存在」与「凭证内容无效」。
type credentialsError struct {
	Reason string
	Err    error
}

func (e *credentialsError) Error() string { return e.Err.Error() }

func (e *credentialsError) Unwrap() error { return e.Err }

// NewCASFactory 构造生产用 CASFactory：读同 namespace 的凭证 Secret，按 resourceVersion 缓存 client。
func NewCASFactory(reader client.Reader, cache *aliyun.ClientCache[aliyun.CASClient], limiters *aliyun.Limiters, timeout time.Duration) CASFactory {
	return func(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
		s := &corev1.Secret{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: ac.Spec.Aliyun.CredentialsRef.Name}, s)
		if apierrors.IsNotFound(err) {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound, fmt.Errorf("凭证 Secret %q 不存在", ac.Spec.Aliyun.CredentialsRef.Name)}
		}
		if err != nil {
			return nil, err
		}
		creds, err := aliyun.CredentialsFromSecret(s)
		if err != nil {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
		}
		// key 必须囊括 build 闭包里读到的每一个会改变 client 行为的字段——
		// ResourceGroupID 也在内，否则同 namespace、同凭证、同 region 但不同资源组的两个
		// CR 会串用同一个 client：证书传进别人的资源组，探测又在错误的资源组里找不到它。
		key := aliyun.ClientKey{
			Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion,
			Region: ac.Spec.Aliyun.EffectiveCASRegion(), Endpoint: ac.Spec.Aliyun.EndpointOverride,
			ResourceGroupID: ac.Spec.Aliyun.ResourceGroupID,
		}
		return cache.GetOrBuild(key, func() (aliyun.CASClient, error) {
			cred, err := creds.Build()
			if err != nil {
				return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
			}
			// 一律读 key 而不是 ac：缓存命中与否只由 key 决定，闭包里再从 ac 取值就等于
			// 把没进 key 的字段偷偷带进 client，正是上面那个 bug 的形状。
			return aliyun.NewCASClient(cred, aliyun.CASClientConfig{
				Region: key.Region, Endpoint: key.Endpoint, ResourceGroupID: key.ResourceGroupID,
				Timeout: timeout, Limiters: limiters, LimiterKey: creds.LimiterKey(),
				OnCall: recordAliyunAPICall,
			})
		})
	}
}
