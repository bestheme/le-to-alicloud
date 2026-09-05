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
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// targetNotFoundRequeue 是「域名还不存在」时的固定重试间隔（spec §6.2 步骤 5）。
// 不用指数退避：域名可能由 Terraform 稍后创建，这是等待而不是故障。
//
//nolint:unused // 由 Task 11 的 TargetNotFound 分支引用；两个重试间隔在 Task 7 一次定死。
const targetNotFoundRequeue = 5 * time.Minute

// credentialsRequeue 是凭证类错误的长 requeue：等人换 AK 或补授权，重试再快也没用。
const credentialsRequeue = 5 * time.Minute

// ProviderFactory 按 Binding 解析出 provider 与已经构造好的云 client。
//
// 凭证与 client 构造归通用层（spec §7 职责边界表），所以这一步在 controller 侧完成，
// provider 只收现成的 client。ac 用来继承凭证（Binding.credentialsRef 缺省时）。
type ProviderFactory func(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding,
	ac *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error)

// AliyunCertificateBindingReconciler 实现 spec §6 的绑定 controller。
type AliyunCertificateBindingReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder

	ProviderFactory ProviderFactory

	DriftCheckInterval   time.Duration
	CleanupGracePeriod   time.Duration
	CleanupFailurePolicy string

	// Now 是时钟注入点，nil 表示真实时间。运行中只许经 SetNow 改（与证书 controller 同构）。
	Now func() time.Time
	mu  sync.RWMutex
}

// SetNow 线程安全地替换时钟。
func (r *AliyunCertificateBindingReconciler) SetNow(fn func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Now = fn
}

// now 与 SetNow 成对：只留 SetNow 会让时钟注入点只写不读。读取点是 appliedLag
// （binding_status.go），Task 11 的 lastAppliedTime 会再加一个。
func (r *AliyunCertificateBindingReconciler) now() time.Time {
	r.mu.RLock()
	fn := r.Now
	r.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates,verbs=get;list;watch

// Reconcile 实现 spec §6.2 的步骤。
func (r *AliyunCertificateBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	rd := newBindingRound(b)

	// 0. 删除分支（binding_deletion.go；Task 13 之前是存根）
	if !b.DeletionTimestamp.IsZero() {
		return r.reconcileBindingDelete(ctx, rd)
	}

	// finalizer 先加上，哪怕 deletionPolicy 现在是 Orphan：策略随时可以改成 Unbind，
	// 而没有 finalizer 的对象删除时我们根本收不到通知。
	if !controllerutil.ContainsFinalizer(b, certsv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(b, certsv1alpha1.FinalizerName)
		if err := r.Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
		// Update 会触发本对象的 watch 事件，不需要显式 requeue
		return ctrl.Result{}, nil
	}

	b.Status.ObservedGeneration = b.Generation

	// 1. 取证书。NotFound 不在这里 return——先把 lag 算了。
	ac := &certsv1alpha1.AliyunCertificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}, ac)
	switch {
	case apierrors.IsNotFound(err):
		ac = nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// 1b. 滞后时长在任何早退之前算好。aliyuncert_binding_applied_age_seconds 是 spec §10.1
	// 唯一点名「必须告警」的绑定侧指标，而 patchBinding → recordBindingMetrics 每次都会用
	// rd.lag 刷它；算晚了，「冲突 / Secret 丢了 / 域名不覆盖」这三种最该告警的状态反而
	// 被刷成 0，spec §10.3 的 AliyunCertificateBindingStale 永远不触发。
	// ac 不存在、或 status.current 为空时无从判断滞后，取 0——那两种状态由
	// aliyuncert_binding_ready=0 覆盖告警。
	rd.lag = r.appliedLag(b, ac)

	if ac == nil {
		// 不碰 Applied：目标上那张证书还在正常服役，证书 CR 不见了说明不了它有问题
		// （典型场景是 Argo CD 正在换名字重建）。只降 Ready，靠 watch 唤醒。
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotFound,
			fmt.Sprintf("AliyunCertificate %q 不存在", b.Spec.CertificateRef.Name))
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}
	if !certIssued(ac) {
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotReady, "证书尚未通过校验")
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}

	return r.reconcileBindingReady(ctx, rd, ac)
}

// reconcileBindingReady 处理证书可用之后的步骤 2–8。Task 10–12 逐步填充剩下的步骤。
func (r *AliyunCertificateBindingReconciler) reconcileBindingReady(
	ctx context.Context, rd *bindingRound, ac *certsv1alpha1.AliyunCertificate,
) (ctrl.Result, error) {
	b := rd.b

	// 2. 冲突仲裁（spec §6.2 步骤 2）
	winner, err := r.arbitrate(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if winner.UID != b.UID {
		setBindingCondition(b, certsv1alpha1.ConditionConflict, metav1.ConditionTrue,
			certsv1alpha1.ReasonConflictingBinding,
			fmt.Sprintf("同目标已由 %s/%s 绑定", winner.Namespace, winner.Name))
		aggregateBindingReady(b)
		// 云侧一个字节都不写。胜者变化或消失会经 SetupWithManager 里的同目标 watch
		// 唤醒我们（For 只入队变化的对象本身，唤不醒输者），drift 周期是兜底。
		// 这是一次早退，但 rd.lag 已在 Reconcile 里取完证书之后算好，
		// patchBinding → recordBindingMetrics 刷出的 applied_age 是真值而不是 0。
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}
	setBindingCondition(b, certsv1alpha1.ConditionConflict, metav1.ConditionFalse,
		certsv1alpha1.ReasonNoConflict, "")

	// 3. 装载材料并校验域名覆盖（spec §6.2 步骤 3）
	m, me := loadBindingMaterial(ctx, r.Client, ac)
	if me == nil {
		me = checkDomainCoverage(m, b)
	}
	if me != nil {
		// 不碰 Applied：目标上那张证书还在服役，Secret 出问题说明不了它有毛病
		// （与证书 controller 不清空 status.current 是同一条原则）。
		// rd.lag 早已算好，这次早退不会把 applied_age 刷成 0。
		setBindingReadyFalse(b, me.Reason, me.Message)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	// 3b. 证书 CR 必须已经认下 Secret 里的这一代，否则不写云（见 certificateGate）。
	if !certificateGate(m, ac) {
		setBindingCondition(b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse,
			certsv1alpha1.ReasonCertificateNotReady, "证书 CR 尚未认下 Secret 中的这一代")
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: certificateGateRequeue}, r.patchBinding(ctx, rd)
	}

	// 4. 解析凭证并构造 provider client（spec §6.2 步骤 4）
	p, cl, err := r.providerClient(ctx, b, ac)
	if err != nil {
		return r.handleFactoryError(ctx, rd, err)
	}
	tg, err := targetOf(b)
	if err != nil {
		setBindingReadyFalse(b, certsv1alpha1.ReasonApplyFailed, err.Error())
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}
	_ = p
	_ = cl
	_ = tg

	aggregateBindingReady(b)
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
}

// providerClient 通过 ProviderFactory 取 provider 与 client；与证书侧的 casClient 同构。
//
// 显式挡住未配置的工厂：接线漏了就在 worker 里 nil 函数调用 panic，而 panic 出在
// reconcile 循环里比一条 ApplyFailed 难查得多。
//
//nolint:unparam // 返回的 Provider 目前被 `_ = p` 丢弃，调用点由 Task 11 的 Observe 补上。
func (r *AliyunCertificateBindingReconciler) providerClient(ctx context.Context,
	b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate,
) (provider.Provider, provider.Client, error) {
	if r.ProviderFactory == nil {
		return nil, nil, errors.New("ProviderFactory 未配置")
	}
	return r.ProviderFactory(ctx, b, ac)
}

// handleFactoryError 处置「连 client 都没造出来」的失败。
//
// 凭证类错误不可重试：AK 被吊销、Secret 写错了 key，重试再快也没用，等的是人改配置。
// 长 requeue 5m 而不是指数退避，退避到几十分钟反而会让「改好了却迟迟不生效」。
//
// 只降 Ready、不碰 Applied：造不出 client 说明不了目标上那张证书是错的（与
// 「Secret 丢了」同一条原则），status.appliedFingerprint 也一个字节都不动。
func (r *AliyunCertificateBindingReconciler) handleFactoryError(
	ctx context.Context, rd *bindingRound, err error,
) (ctrl.Result, error) {
	var ce *credentialsError
	if errors.As(err, &ce) {
		setBindingReadyFalse(rd.b, ce.Reason, ce.Error())
		return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
	}
	// 剩下的是接线错误（未知 target.type、没注册 provider）：同样等 spec 改动。
	setBindingReadyFalse(rd.b, certsv1alpha1.ReasonApplyFailed, err.Error())
	return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
}

// reconcileBindingDelete 是删除分支的存根，Task 13 替换。
func (r *AliyunCertificateBindingReconciler) reconcileBindingDelete(
	ctx context.Context, rd *bindingRound,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rd.b, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(rd.b, certsv1alpha1.FinalizerName)
	if err := r.Update(ctx, rd.b); err != nil {
		return ctrl.Result{}, err
	}
	clearBindingMetrics(rd.b.Namespace, rd.b.Name, rd.provider)
	return ctrl.Result{}, nil
}

// certIssued 判断证书 CR 是否已经拿到可用的材料。
//
// 看 Issued 而不是 Ready：Ready 还包含 Uploaded，而 FC3 内联 PEM，根本不需要 CAS 上传
// 成功（spec §3「上传 CAS 与绑定 FC3 是并行副作用，不是串行依赖」）。CAS 挂了不该
// 连带 HTTPS 也推不上去。
func certIssued(ac *certsv1alpha1.AliyunCertificate) bool {
	return meta.IsStatusConditionTrue(ac.Status.Conditions, certsv1alpha1.ConditionIssued)
}

// bindingRequests 把一批 Binding 转成 reconcile 请求。
func bindingRequests(list *certsv1alpha1.AliyunCertificateBindingList) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
		}})
	}
	return out
}

// SetupWithManager 注册 watch：主资源，外加证书变化与同目标 peer 的反查。
func (r *AliyunCertificateBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certsv1alpha1.AliyunCertificateBinding{}).
		// 证书的 status.current 一变就要唤醒引用它的全部 Binding。用 field index 反查，
		// 而不是遍历：一个 namespace 里可能有几百个 Binding。
		Watches(&certsv1alpha1.AliyunCertificate{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				list := &certsv1alpha1.AliyunCertificateBindingList{}
				if err := mgr.GetClient().List(ctx, list,
					client.InNamespace(o.GetNamespace()),
					client.MatchingFields{certsv1alpha1.IndexBindingByCertificate: o.GetName()}); err != nil {
					// 这一次唤醒就此丢失，而它守的正是「续期成功却没推到线上」那条主线。
					// DriftCheckInterval 的 requeue 会兜住，但没有这行日志，运维侧收不到任何信号。
					logf.FromContext(ctx).Error(err, "list bindings for certificate failed",
						"certificate", client.ObjectKeyFromObject(o))
					return nil
				}
				return bindingRequests(list)
			})).
		// 同目标的 peer 变化要唤醒**其余**候选者，见 bindingPeerRequests。
		Watches(&certsv1alpha1.AliyunCertificateBinding{},
			handler.EnqueueRequestsFromMapFunc(bindingPeerRequests(mgr.GetClient()))).
		Named("aliyuncertificatebinding").
		Complete(r)
}
