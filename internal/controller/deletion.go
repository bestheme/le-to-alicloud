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
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

// requeueDeletionWait 是等待 cert-manager Certificate 真正消失时的重试间隔。
const requeueDeletionWait = 2 * time.Second

// cleanupAbandonedMessage 是 CleanupAbandoned 事件的固定文案。事件面向用户广播，云错误
// 原文与遗留的 certId 都可能夹带只对运维有意义的细节，那些只进日志。
const cleanupAbandonedMessage = "gave up deleting the CAS certificates after the cleanup grace period; see operator logs for the leaked certificate IDs"

// secretDeletionSkippedMessage 是删除期跳过删除 Secret 时的固定文案。与
// cleanupAbandonedMessage 同一个理由抽成常量：文案是用户看得见的契约，用例逐字钉住它，
// 改措辞时才会有人提醒。Secret 名不进消息——它就是 spec.secretName，用户手上已经有了，
// 而事件文案里带变量会让 K8s 的聚合失效。
const secretDeletionSkippedMessage = "Secret 不属于本 CR（cert-manager.io/certificate-name 指向别的 Certificate），跳过删除"

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
// d. 删 Secret（仅限注解指向本 CR 的那一个；不属于本 CR 则跳过并留下事件与计数）
// e. 摘 finalizer（d 跳过与否都照常摘）
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
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonDeletionBlocked, msg)
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
		// 与绑定侧共用同一个纯函数（binding_deletion.go）。「还要不要继续重试清理」是
		// 有界清理的核心判断——它决定一个对象会不会被永远钉在 Terminating 上——两份
		// 逐字相同的内联表达式迟早会分叉，而只有一份有 TestShouldAbandonCleanup 盯着。
		if !shouldAbandonCleanup(r.CleanupFailurePolicy, elapsed, r.CleanupGracePeriod) {
			// 清理失败与上传失败是两码事：这条 reason 会出现在一个正在删除的对象上，
			// 沿用 UploadFailed 会让人以为签发链路出了问题。
			setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonCleanupFailed, "CAS 清理失败，重试中: "+err.Error())
			if perr := r.patchStatus(ctx, ac, orig); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, err // 指数退避
		}
		// Abandon：把足以人工兜底的信息留在日志里，然后继续走完删除。
		region := ac.Spec.Aliyun.EffectiveCASRegion()
		kv := []any{"region", region, "certIds", remainingCertIDs(ac), "gracePeriod", r.CleanupGracePeriod}
		if p := ac.Status.PendingUpload; p != nil {
			// 这一张没有 certId 可报，名字是人工去 CAS 里找到它的唯一线索。
			kv = append(kv, "pendingCASName", p.CASName)
		}
		log.Error(err, "cleanup abandoned", kv...)
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonCleanupAbandoned, cleanupAbandonedMessage)
		cleanupAbandonedTotal.WithLabelValues(region, aliyun.ClassOf(err).String()).Inc()
	}

	// CAS 侧的进展必须先落盘再往下走。否则下一轮会对着同一批 certId 再删一次（靠
	// NotFound 吸收），而临近宽限期到期时，那多出来的一次调用只要撞上限流，就会被
	// 当成「清理仍在失败」而误报 CleanupAbandoned。
	if err := r.patchStatus(ctx, ac, orig); err != nil {
		return ctrl.Result{}, err
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

	// d. 删 Secret——但只删注解确实指向本 CR 的那一个。
	//
	// 按名字无条件删是不安全的：spec.secretName 可以在 CR 生命周期里被改指到别人的
	// Secret 上。变更期的护栏（aliyuncertificate_controller.go 第 2 步）是主防线，这里
	// 是兜底，挡住护栏上线之前就已经指歪了的存量对象。注解缺失的手工 Secret 同样不删。
	s := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: secretNameFor(ac)}, s)
	switch {
	case err == nil && secretOwnedByUs(s, ac):
		if derr := r.Delete(ctx, s); derr != nil && !apierrors.IsNotFound(derr) {
			return ctrl.Result{}, derr
		}
	case err == nil:
		// 私钥留在集群里这件事不能无声无息。三样痕迹各有各的读者：日志带 Secret 名，
		// 给运维定位；事件面向用户；计数器是唯一活得比 namespace 长的那一份，配告警用它。
		log.Info("secret is not owned by this certificate, skipping deletion", "secret", s.Name)
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonSecretNameConflict, secretDeletionSkippedMessage)
		secretDeletionSkippedTotal.WithLabelValues(ac.Namespace).Inc()
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// e. 摘 finalizer
	//
	// NotFound 必须吸收掉，理由与 finishBindingDeletion 逐字相同：删除分支读的是 informer
	// cache，对象被真正删除之后缓存里那份带 finalizer 的旧版本还会再唤起一轮，这一轮的
	// Update 打在已经不存在的对象上。抛上去等于每一次删除都推高一次
	// controller_runtime_reconcile_errors_total——运维正是拿它配告警的。
	controllerutil.RemoveFinalizer(ac, certsv1alpha1.FinalizerName)
	if err := client.IgnoreNotFound(r.Update(ctx, ac)); err != nil {
		return ctrl.Result{}, err
	}
	// 对象没了，它的 gauge 也必须跟着消失：留下来的那条 Ready=0 会一直告警下去。
	clearCertMetrics(ac.Namespace, ac.Name)
	log.Info("deleted", "name", ac.Name)
	return ctrl.Result{}, nil
}

// remainingCertIDs 返回 status 中仍记着 certId 的代次。
//
// 只覆盖 current 与 history：pendingUpload 那一张按定义还没有 certId，它由
// cleanupPendingUpload 按名字单独认领，Abandon 时也单独记 pendingCASName。
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

// cleanupCAS 删除 status 记录的全部代次，外加一次 in-flight 的 pendingUpload；成功的
// 把 certId / pendingUpload 清空，任一失败立即返回。调用方两条路径上都会 patch status，
// 于是已经删掉的东西不会在下一轮被重复删除。
func (r *AliyunCertificateReconciler) cleanupCAS(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	// pendingUpload 也算「还有事可做」：那次上传可能已经在服务端落地了，只是响应丢了。
	if len(remainingCertIDs(ac)) == 0 && ac.Status.PendingUpload == nil {
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
		err := cas.Delete(ctx, *gen.CertID, deleteToken(naming.ClientToken(ac.UID, gen.Fingerprint)))
		recordCASDelete(err)
		if err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
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
	if err := del(ac.Status.Current); err != nil {
		return err
	}
	return r.cleanupPendingUpload(ctx, cas, ac)
}

// cleanupPendingUpload 回收 write-ahead 记录指向的那一张证书。
//
// pendingUpload 存在，恰恰意味着上传的结果没拿到：证书可能已经在服务端落地，也可能
// 根本没建成，status 里没有 certId 可用。删除路径不会再走 upload.go 的解析逻辑，所以
// 这里必须自己按名字去云上认领一次，否则那张证书会被永久且无痕地孤儿化。
func (r *AliyunCertificateReconciler) cleanupPendingUpload(ctx context.Context, cas aliyun.CASClient, ac *certsv1alpha1.AliyunCertificate) error {
	p := ac.Status.PendingUpload
	if p == nil {
		return nil
	}
	hint := casFindHint(ac, "")
	list, err := cas.FindUploaded(ctx, hint)
	if err != nil {
		return err
	}
	found := false
	for _, c := range list {
		if c.Name != p.CASName {
			continue
		}
		found = true
		derr := cas.Delete(ctx, c.CertID, deleteToken(p.ClientToken))
		recordCASDelete(derr)
		if derr != nil && aliyun.ClassOf(derr) != aliyun.ClassNotFound {
			return derr
		}
		break
	}
	if !found {
		// 列表里没有这个名字，说明那次上传从未在服务端落地。但「没找到」也可能是 hint
		// 选错了域名，那就等于放走一张孤儿证书——把两个线索都记下来，人工才查得动。
		logf.FromContext(ctx).Info("pendingUpload not found in CAS, dropping the record",
			"CASName", p.CASName, "hint", hint)
	}
	// 认领并删掉了，或者根本没有东西需要回收。两种情形都可以把记录抹掉。
	ac.Status.PendingUpload = nil
	return nil
}

// deleteToken 由上传 token 派生出删除用的幂等令牌：加后缀与上传区分，并守住 CAS 的
// 64 字符上限。
func deleteToken(uploadToken string) string {
	t := uploadToken + "d"
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}
