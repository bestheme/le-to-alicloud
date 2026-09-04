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
	ctrl "sigs.k8s.io/controller-runtime"
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
			log.V(1).Info("skip reclaim", "fingerprint", shortFP(oldest.Fingerprint), "why", why)
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
			recordCASDelete(err)
			if err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
				// 唯一一处知道「删的是哪一张」的地方；错误详情只到日志为止，事件里不带。
				log.Error(err, "delete CAS certificate failed", "certId", *oldest.CertID, "fingerprint", shortFP(oldest.Fingerprint))
				return err
			}
			r.Recorder.Event(ac, corev1.EventTypeNormal, "Reclaimed", "old CAS certificate reclaimed")
			log.Info("reclaimed CAS certificate", "certId", *oldest.CertID, "fingerprint", shortFP(oldest.Fingerprint))
		}
		ac.Status.History = ac.Status.History[:len(ac.Status.History)-1]
		excess--
	}
	return nil
}

// reclaimFailedMessage 是 ReclaimFailed 事件的固定文案。事件是广播给用户的对象，云错误
// 原文可能夹带 request id 之类的细节，不该进这里；详情只进日志。
const reclaimFailedMessage = "failed to reclaim an old CAS certificate; the current certificate is unaffected"

// handleReclaimError 处理回收失败（控制器裁决 R21）。
//
// 回收是纯清理动作：当前这一代早就上传成功、正在服役，删不掉一张早已没人引用的旧证书
// 说明不了它有任何问题。若沿用 handleCloudError，一次清理失败会把 Uploaded / Ready 打成
// False，Binding 侧会跟着认为证书不可用而连锁停摆——用一个无害的失败换来一场真实的故障。
// 所以这里一个 condition 都不碰，只发一条 Warning 事件，把既有判定原样保留下来。
func (r *AliyunCertificateReconciler) handleReclaimError(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, err error) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(err, "保留策略回收失败")
	r.Recorder.Event(ac, corev1.EventTypeWarning, "ReclaimFailed", reclaimFailedMessage)
	// aggregateReady 只读 Issued / Uploaded，上面没动过，判定与回收成功时完全一致。
	r.aggregateReady(ac)
	// 本轮可能已经成功删掉了更老的几代：那部分 history 必须落盘，否则下一轮会对着同一批
	// certId 再删一次。
	if perr := r.patchStatus(ctx, ac, orig); perr != nil {
		return ctrl.Result{}, perr
	}
	if aliyun.ClassOf(err) == aliyun.ClassRetryable {
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}
