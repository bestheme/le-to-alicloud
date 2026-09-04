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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// probeCAS 周期性确认 current.certId 仍在 CAS。被人手删时清空 certId 与指纹，交给
// ensureUploaded 重传。FC3 不引用 certId，因此这里不惊动 Binding（spec §B6.2）。
//
// 返回 missing=true 表示「云上没有了、已经在内存里清好」。调用方必须先把这个结论落盘
// 再进 ensureUploaded：那里会用 APIReader 直读 CR 覆盖内存里的代次，只在内存里清是白清。
func (r *AliyunCertificateReconciler) probeCAS(ctx context.Context, ac *certsv1alpha1.AliyunCertificate, primaryDomain string) (bool, error) {
	if !ac.Spec.Aliyun.UploadEnabled() || ac.Status.Current == nil || ac.Status.Current.CertID == nil {
		return false, nil
	}
	// 已过期的证书一律跳过。CAS 的 ListUserCertificateOrder 在 OrderType=UPLOAD 且
	// Status 为空时根本不返回过期证书，照常探测必然查不到它，于是会被误判成「被人删了」
	// 而重传一张同样过期的证书。过期本来就该由 cert-manager 的续期换代解决。
	if ac.Status.Current.NotAfter.Time.Before(r.now()) {
		return false, nil
	}
	if ac.Status.CASProbedAt != nil && r.now().Sub(ac.Status.CASProbedAt.Time) < r.CASProbeInterval {
		return false, nil
	}
	cas, err := r.casClient(ctx, ac)
	if err != nil {
		return false, err
	}
	list, err := cas.FindUploaded(ctx, casFindHint(ac, primaryDomain))
	if err != nil {
		return false, err
	}
	// 时间戳在这里推进而不是等确认结果：这一轮确实去云上问过了，无论答案是什么，
	// 12h 之内都不必再问第二遍。
	ac.Status.CASProbedAt = &metav1.Time{Time: r.now()}
	for _, c := range list {
		if c.CertID == *ac.Status.Current.CertID {
			return false, nil
		}
	}
	logf.FromContext(ctx).Info("CAS certificate missing, will re-upload",
		"certId", *ac.Status.Current.CertID, "fingerprint", shortFP(ac.Status.Current.Fingerprint))
	r.Recorder.Event(ac, corev1.EventTypeWarning, "CASCertificateMissing", "current certificate not found in CAS; re-uploading")
	// 清空 certId 与指纹以强制 ensureUploaded 走上传；保留 history。
	ac.Status.Current.CertID = nil
	ac.Status.Current.Fingerprint = ""
	return true, nil
}
