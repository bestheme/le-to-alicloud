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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

// reclaimable 判断某一代能否回收：三重护栏（spec §5.7）。
//  1. 年龄 ≥ minAge
//  2. 所有引用本证书的 Binding 都已完成对当前 generation 的 reconcile
//  3. 没有任何 Binding 的 appliedFingerprint 等于该代
func reclaimable(gen certsv1alpha1.CertificateGeneration, now time.Time, minAge time.Duration, bindings []certsv1alpha1.AliyunCertificateBinding) (bool, string) {
	if now.Sub(gen.UploadedAt.Time) < minAge {
		return false, "minAge 未到"
	}
	for i := range bindings {
		b := &bindings[i]
		if b.Status.ObservedGeneration < b.Generation {
			return false, fmt.Sprintf("Binding %s 尚未完成 reconcile", b.Name)
		}
		if b.Status.AppliedFingerprint == gen.Fingerprint {
			return false, fmt.Sprintf("Binding %s 仍在使用该代", b.Name)
		}
	}
	return true, ""
}

// liveBindingsFor 绕过 informer cache 直读引用本证书的 Binding（护栏 2/3 的 TOCTOU 防线）。
// APIReader 不支持自定义 field index，因此按 namespace 列出后客户端过滤。
func (r *AliyunCertificateReconciler) liveBindingsFor(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) ([]certsv1alpha1.AliyunCertificateBinding, error) {
	list := &certsv1alpha1.AliyunCertificateBindingList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(ac.Namespace)); err != nil {
		return nil, err
	}
	var out []certsv1alpha1.AliyunCertificateBinding
	for _, b := range list.Items {
		if b.Spec.CertificateRef.Name == ac.Name {
			out = append(out, b)
		}
	}
	return out, nil
}

// reclaimOldGenerations 让 current+history 的总数收敛到 keepLast。
// 从最老的一代开始尝试；护栏不满足就停在那一代（更新的代次年龄更小，也不会满足）。
func (r *AliyunCertificateReconciler) reclaimOldGenerations(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	if !ac.Spec.Aliyun.UploadEnabled() {
		return nil
	}
	keep := ac.Spec.Retention.KeepLastOrDefault()
	excess := 1 + len(ac.Status.History) - keep // current 占 1
	if excess <= 0 {
		return nil
	}
	log := logf.FromContext(ctx)

	bindings, err := r.liveBindingsFor(ctx, ac)
	if err != nil {
		return err
	}
	var cas aliyun.CASClient
	now := r.now()
	minAge := ac.Spec.Retention.MinAgeOrDefault()

	// history[0] 最新，末尾最老
	for excess > 0 && len(ac.Status.History) > 0 {
		oldest := ac.Status.History[len(ac.Status.History)-1]
		ok, why := reclaimable(oldest, now, minAge, bindings)
		if !ok {
			log.V(1).Info("skip reclaim", "fingerprint", oldest.Fingerprint[:8], "why", why)
			return nil
		}
		if oldest.CertID != nil {
			if cas == nil {
				if cas, err = r.casClient(ctx, ac); err != nil {
					return err
				}
			}
			token := naming.ClientToken(ac.UID, oldest.Fingerprint) + "d" // 与上传 token 区分
			if len(token) > 64 {
				token = token[:64]
			}
			err := cas.Delete(ctx, *oldest.CertID, token)
			if err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
				return err
			}
			r.Recorder.Event(ac, corev1.EventTypeNormal, "Reclaimed", "old CAS certificate reclaimed")
			log.Info("reclaimed CAS certificate", "certId", *oldest.CertID, "fingerprint", oldest.Fingerprint[:8])
		}
		ac.Status.History = ac.Status.History[:len(ac.Status.History)-1]
		excess--
	}
	return nil
}
