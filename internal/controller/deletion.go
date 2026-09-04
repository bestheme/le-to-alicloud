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
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

// cleanupAbandonedTotal 暂居于此，Task 14 会把它连同其它指标一起搬进 metrics.go。
var cleanupAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aliyuncert_cleanup_abandoned_total",
	Help: "Number of AliyunCertificate deletions that abandoned CAS cleanup",
}, []string{"region", "reason"})

func init() { metrics.Registry.MustRegister(cleanupAbandonedTotal) }

// requeueDeletionWait 是等待 cert-manager Certificate 真正消失时的重试间隔。
const requeueDeletionWait = 2 * time.Second

// cleanupAbandonedMessage 是 CleanupAbandoned 事件的固定文案。事件面向用户广播，云错误
// 原文与遗留的 certId 都可能夹带只对运维有意义的细节，那些只进日志。
const cleanupAbandonedMessage = "gave up deleting the CAS certificates after the cleanup grace period; see operator logs for the leaked certificate IDs"

// activeBindingNames 只统计未在删除中的 Binding（避免与 Argo CD prune 死锁）。
// Argo CD 会同时 prune AliyunCertificate 与引用它的 Binding；若把已带 deletionTimestamp
// 的 Binding 也算作阻塞方，两边会互相等待到谁都删不掉。
func activeBindingNames(bindings []certsv1alpha1.AliyunCertificateBinding) []string {
	var names []string
	for i := range bindings {
		if bindings[i].DeletionTimestamp.IsZero() {
			names = append(names, bindings[i].Name)
		}
	}
	return names
}

// reconcileDelete 实现 spec §5.6：
// a. 活着的 Binding → 阻塞
// b. CAS 各代（有界，Abandon/Block）
// c. 显式删 cmapi.Certificate 并等它消失（否则 cert-manager 会重建 Secret）
// d. 删 Secret
// e. 摘 finalizer
func (r *AliyunCertificateReconciler) reconcileDelete(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	// a. 阻塞判断（live read：informer cache 可能还没看到刚建出来的 Binding）
	bindings, err := r.liveBindingsFor(ctx, ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	if names := activeBindingNames(bindings); len(names) > 0 {
		msg := "仍被 Binding 引用: " + strings.Join(names, ", ")
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonDeletionBlocked, msg)
		r.Recorder.Event(ac, corev1.EventTypeWarning, "DeletionBlocked", msg)
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig) // Binding 变化会通过 watch 唤醒
	}

	// 宽限期从「真正开始清理」而不是从 deletionTimestamp 起算：被 Binding 阻塞的那段
	// 时间里一次 CAS 调用都没发生过，不该消耗预算。
	if ac.Status.CleanupStartedAt == nil {
		ac.Status.CleanupStartedAt = &metav1.Time{Time: r.now()}
		if err := r.patchStatus(ctx, ac, orig); err != nil {
			return ctrl.Result{}, err
		}
		orig = ac.DeepCopy()
	}

	// b. CAS 清理（依据 status 里记下的代次，而不是 spec.uploadToCAS：开关可能刚被关掉，
	// 之前上传的证书仍需回收）
	if err := r.cleanupCAS(ctx, ac); err != nil {
		elapsed := r.now().Sub(ac.Status.CleanupStartedAt.Time)
		if r.CleanupFailurePolicy == CleanupPolicyBlock || elapsed < r.CleanupGracePeriod {
			setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, "CAS 清理失败，重试中: "+err.Error())
			if perr := r.patchStatus(ctx, ac, orig); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, err // 指数退避
		}
		// Abandon：把足以人工兜底的信息留在日志里，然后继续走完删除。
		ids := remainingCertIDs(ac)
		region := ac.Spec.Aliyun.EffectiveCASRegion()
		log.Error(err, "cleanup abandoned", "region", region, "certIds", ids, "gracePeriod", r.CleanupGracePeriod)
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonCleanupAbandoned, cleanupAbandonedMessage)
		cleanupAbandonedTotal.WithLabelValues(region, aliyun.ClassOf(err).String()).Inc()
	}

	// c. 显式删除 Certificate 并等它消失。ownerRef 级联删除是异步的，而 cert-manager
	// 只要还看得见 Certificate 就会把我们下一步删掉的 Secret 再建回来。
	cert := &cmapi.Certificate{}
	err = r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: certManagerNameFor(ac)}, cert)
	switch {
	case err == nil:
		if cert.DeletionTimestamp.IsZero() {
			if derr := r.Delete(ctx, cert); derr != nil && !apierrors.IsNotFound(derr) {
				return ctrl.Result{}, derr
			}
		}
		return ctrl.Result{RequeueAfter: requeueDeletionWait}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// d. 删 Secret
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ac.Namespace, Name: secretNameFor(ac)}}
	if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// e. 摘 finalizer
	controllerutil.RemoveFinalizer(ac, certsv1alpha1.FinalizerName)
	if err := r.Update(ctx, ac); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("deleted", "name", ac.Name)
	return ctrl.Result{}, nil
}

// remainingCertIDs 返回 status 中仍记着 certId 的代次。
//
// status.pendingUpload 没有 certId——那条记录存在恰恰是因为上传结果没拿到。若清理时
// 正好卡在这个窗口，那张证书只能靠 CAS 探测（Task 14 的 FindUploaded）或人工兜底，
// 这里报不出它的 ID，是 Abandon 路径的已知边界。
func remainingCertIDs(ac *certsv1alpha1.AliyunCertificate) []int64 {
	var ids []int64
	if ac.Status.Current != nil && ac.Status.Current.CertID != nil {
		ids = append(ids, *ac.Status.Current.CertID)
	}
	for _, h := range ac.Status.History {
		if h.CertID != nil {
			ids = append(ids, *h.CertID)
		}
	}
	return ids
}

// cleanupCAS 删除 status 中记录的全部代次；成功的把 certId 清空，任一失败立即返回。
// 调用方在失败路径上会 patch status，于是已经删掉的代次不会在下一轮被重复删除。
func (r *AliyunCertificateReconciler) cleanupCAS(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	if len(remainingCertIDs(ac)) == 0 {
		return nil
	}
	cas, err := r.casClient(ctx, ac)
	if err != nil {
		return err
	}
	del := func(gen *certsv1alpha1.CertificateGeneration) error {
		if gen == nil || gen.CertID == nil {
			return nil
		}
		token := naming.ClientToken(ac.UID, gen.Fingerprint) + "d" // 与上传 token 区分
		if len(token) > 64 {
			token = token[:64]
		}
		if err := cas.Delete(ctx, *gen.CertID, token); err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
			return err
		}
		gen.CertID = nil
		return nil
	}
	for i := range ac.Status.History {
		if err := del(&ac.Status.History[i]); err != nil {
			return err
		}
	}
	return del(ac.Status.Current)
}
