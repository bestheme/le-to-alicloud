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
	"bytes"
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"
)

// staleReader 是一个「永远落后一拍」的 client.Reader：Get 一律返回构造时给定的那份快照，
// 不管 API server 上已经变成什么样。它扮演的是没追上的 informer cache。
//
// 只覆写 Get。Reconcile 只用 APIReader 读本对象，其余（证书 CR、仲裁用的 List）照旧走
// Client；List 直接委派下去，好让这个包装可以整个替换掉 APIReader 而不改变别的行为。
type staleReader struct {
	client.Reader
	snapshot *certsv1alpha1.AliyunCertificateBinding
}

func (s staleReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	b, ok := obj.(*certsv1alpha1.AliyunCertificateBinding)
	if !ok {
		return nil
	}
	s.snapshot.DeepCopyInto(b)
	return nil
}

// TestReconcile_StatusPatchBaselineIsAPIServerTruth 钉住 Reconcile 用 APIReader 直读本对象
// 这一条，而且不靠时序。
//
// 它复现的是实测抓到的那次丢更新（-race 下 3/3，常规下不必然，所以既有的
// binding_observe_test.go「接管之后 Observe 未知失败」守不住这条改动）：
//
//	round A  rv=246  reason=Applied        → Observe 失败，写 ObserveFailed（rv 变 247）
//	round B  rv=246  reason=Applied        ← 5ms 后的重试，informer 还没追上
//
// round B 观测成功、把 reason 写回 Applied，可它手里的基准也说 Applied，于是
// Status().Patch 发的 MergeFrom(rd.orig) 差量是**空的**——API server 上那个 ObserveFailed
// 再没人擦得掉，一个健康的 Binding 会一直挂着它到下一次漂移检查（1 小时）。
//
// 用例把这个时序做成确定的：API server 上是 round A 的结果（ObserveFailed），
// 而 staleReader 交出 round A 之前的那一份（Applied）。两种读法各跑一轮，对比落盘结果。
func TestReconcile_StatusPatchBaselineIsAPIServerTruth(t *testing.T) {
	const (
		ns     = "ns1"
		name   = "b1"
		domain = "api.example.com"
	)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}

	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, domain)
	bundle, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}

	// 一个已经绑好、上一轮观测失败的 Binding：Applied 仍是 True（旁路失败不降级），
	// reason 被 noteObserveFailed 换成了 ObserveFailed。
	stored := bindingWithDomain(domain)
	stored.Namespace, stored.Name = ns, name
	stored.Finalizers = []string{certsv1alpha1.FinalizerName}
	stored.Spec.CertificateRef = certsv1alpha1.LocalObjectReference{Name: "c1"}
	stored.Status.AppliedFingerprint = bundle.Fingerprint
	// 必须是一个**稳定态**：三条 condition 都已存在、账号也已固化。否则本轮新增的
	// condition 会让 conditions 数组无论如何都进差量（JSON merge patch 对数组是整段替换），
	// 丢更新就被这个无关的差异盖住了——第一版用例正是这么写的，两支都绿，什么也没测到。
	at := metav1.NewTime(time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC))
	stored.Status.BoundAccountID = testAccountID
	stored.Status.Conditions = []metav1.Condition{
		{
			Type: certsv1alpha1.ConditionApplied, Status: metav1.ConditionTrue,
			Reason: certsv1alpha1.ReasonObserveFailed, Message: "上一轮观测失败，保留既有判定",
			LastTransitionTime: at,
		},
		{
			Type: certsv1alpha1.ConditionConflict, Status: metav1.ConditionFalse,
			Reason: certsv1alpha1.ReasonNoConflict, LastTransitionTime: at,
		},
		{
			Type: certsv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: certsv1alpha1.ReasonApplied, LastTransitionTime: at,
		},
	}

	// informer 还停在 round A 之前：同一个对象，Applied 的 reason 还是 Applied。
	staleCopy := stored.DeepCopy()
	staleCopy.Status.Conditions[0].Reason = certsv1alpha1.ReasonApplied
	staleCopy.Status.Conditions[0].Message = ""

	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1"},
		Status: certsv1alpha1.AliyunCertificateStatus{
			SecretName: "c1-tls",
			Current: &certsv1alpha1.CertificateGeneration{
				Fingerprint: bundle.Fingerprint, NotAfter: metav1.NewTime(bundle.Leaf.NotAfter),
			},
			Conditions: []metav1.Condition{{
				Type: certsv1alpha1.ConditionIssued, Status: metav1.ConditionTrue,
				Reason: certsv1alpha1.ReasonReady, LastTransitionTime: metav1.Now(),
			}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1-tls"},
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}

	// 云上装的就是这一张：本轮走幂等短路，不写云，直接 freezeApplied → setApplied。
	//
	// 落盘结果由 API server 把 merge patch 应用到**存储里那一份**上决定，而 fake client
	// 的 status 子资源是整段覆盖、不做服务端合并，复现不出「差量为空所以什么都没改」。
	// 所以断言下沉一层，直接看**发出去的 patch**：JSON merge patch 里没有 conditions，
	// 就等于 API server 上那个 ObserveFailed 一个字节都不会被碰。
	newReconciler := func(t *testing.T, sink *[]byte) (*AliyunCertificateBindingReconciler, client.Client) {
		t.Helper()
		c := crfake.NewClientBuilder().WithScheme(factoryScheme(t)).
			WithObjects(stored.DeepCopy(), ac.DeepCopy(), secret.DeepCopy()).
			WithStatusSubresource(&certsv1alpha1.AliyunCertificateBinding{}).
			WithIndex(&certsv1alpha1.AliyunCertificateBinding{},
				certsv1alpha1.IndexBindingByTarget, bindingTargetIndex).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
					obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					data, err := patch.Data(obj)
					if err != nil {
						return err
					}
					*sink = append(*sink, data...)
					return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()
		f := fake.NewFC3()
		f.SetAccountID(testAccountID)
		f.AddDomain(fake.Domain{
			DomainName: domain, Protocol: "HTTP",
			CertName: "in-service", CertPEM: certPEM, KeyPEM: keyPEM,
		})
		return &AliyunCertificateBindingReconciler{
			Client: c, Scheme: factoryScheme(t), Recorder: record.NewFakeRecorder(16),
			ProviderFactory: func(context.Context, *certsv1alpha1.AliyunCertificateBinding,
				*certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error) {
				return &fc3.Provider{}, f, nil
			},
			DriftCheckInterval: time.Hour,
		}, c
	}

	t.Run("直读：上一轮的 ObserveFailed 被写回 Applied", func(t *testing.T) {
		var sent []byte
		r, c := newReconciler(t, &sent)
		r.APIReader = c // 生产里两者背后是同一个 API server
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		// 基准是 API server 上的真值（ObserveFailed），desired 是 Applied，
		// 差量里必须带上整个 conditions 数组——那才是把 reason 擦回去的唯一途径。
		if !bytes.Contains(sent, []byte(certsv1alpha1.ReasonApplied)) {
			t.Errorf("status patch 必须把 reason 写回 Applied，实际发出的是 %s", sent)
		}
		got := &certsv1alpha1.AliyunCertificateBinding{}
		if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
			t.Fatalf("读回对象失败: %v", err)
		}
		if r := bindingCondReason(got, certsv1alpha1.ConditionApplied); r != certsv1alpha1.ReasonApplied {
			t.Errorf("观测恢复后 reason 必须写回 Applied，实际是 %q", r)
		}
	})

	// 反方向：这一条是「为什么必须直读」的证明。基准落后一拍，它和 desired 都说 Applied，
	// conditions 因此整个从差量里消失——API server 上那个 ObserveFailed 没有任何人会去碰，
	// 一个健康对象就此挂着一条假的诊断痕迹到下一次漂移检查。
	// 这一支若哪天变红了，说明丢更新已经被别的机制堵上，直读才可以重新讨论。
	t.Run("陈旧基准：conditions 整个从差量里消失", func(t *testing.T) {
		var sent []byte
		r, c := newReconciler(t, &sent)
		r.APIReader = staleReader{Reader: c, snapshot: staleCopy}
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if len(sent) == 0 {
			t.Fatal("这一轮本该发出一次 status patch（lastObservedTime 等字段仍有差量）")
		}
		if bytes.Contains(sent, []byte("conditions")) {
			t.Errorf("陈旧基准下 conditions 不该出现在差量里；出现了说明这条用例已经不再钉住直读: %s", sent)
		}
	})
}
