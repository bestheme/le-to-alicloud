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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// targetNotFoundRequeue 是「域名还不存在」时的固定重试间隔（spec §6.2 步骤 5）。
// 不用指数退避：域名可能由 Terraform 稍后创建，这是等待而不是故障。
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
	// APIReader 是绕过 informer cache 的直读口子。Reconcile 用它读本对象——理由见那里，
	// 一句话：status patch 的差量基准必须是 API server 上的真值，不能是落后一拍的 cache。
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

// now 与 SetNow 成对：只留 SetNow 会让时钟注入点只写不读。读取点有三处：appliedLag
// （binding_status.go）、status.lastObservedTime 与 status.lastAppliedTime。
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
	// 本对象走 APIReader（绕过 informer cache）直读，其余读取照旧走 cache。
	//
	// 这不是洁癖，是 status patch 的正确性前提：patchBinding 发的是
	// MergeFrom(rd.orig) 算出来的差量，而 rd.orig 就是这一次读到的那份。cache 落后一拍时
	// 「上一轮写下的 ObserveFailed」在这份副本里还不存在，本轮把 reason 写回 Applied 算出来的
	// 差量因此是**空的**——API server 上那个 ObserveFailed 就再也没人擦得掉，一个健康的
	// Binding 会一直挂着它到下一次漂移检查（1 小时）。
	//
	// 这条竞态一直都在（retryable 失败的重试排在 5ms 后，informer 常常还没追上），
	// 从前被自唤醒循环盖住了：那个循环会一轮一轮地重来，总有一轮读到的是新的。
	// 谓词把多余的轮次去掉之后，重试就只剩一轮，读到旧的就等于把更新丢了。
	// 直读把「差量的基准」钉成 API server 上的真值，整类丢更新就此消失。
	//
	// 代价是每轮多一次不走 cache 的 GET。绑定 controller 的轮次本来就稀（漂移周期 1 小时，
	// 外加证书变化与 spec 变化），这点读放大远小于它换来的确定性。
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, b); err != nil {
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
		// **不能**在这里 return：加 finalizer 只动 metadata.finalizers，既不推进
		// generation 也不碰 annotation / label，bindingMeaningfulChange 会把这次 Update
		// 事件整条滤掉——早先那句「Update 会触发本对象的 watch 事件，不需要显式 requeue」
		// 从加上谓词那一刻起就成了假话，照着写下去每一个新建的 Binding 都会卡在
		// 「有 finalizer、没 status」上，直到有人手动推它一下。
		//
		// 就地接着往下跑，而不是补一个 requeue：r.Update 已经把服务端那一份（含新的
		// resourceVersion 与 finalizers）写回 b，本轮手里的对象是最新的。rd.orig 要跟着
		// 重取，否则后面的 status patch 会把 finalizers 也算进 diff。
		rd = newBindingRound(b)
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

	// 这两条早退都返回 drift 周期（spec §6.1：每一次成功的 reconcile 都返回漂移检查间隔），
	// 与其余每一条终态路径一致。证书的 watch 已经能唤醒它们，所以这不是为了「别卡住」，
	// 而是为了让 aliyuncert_binding_ready / _applied_age 在这两种状态下仍按周期刷新——
	// 只靠 watch 的话，证书对象一直不变时这两个 gauge 就一直停在最后一次观测值上。
	if ac == nil {
		// 不碰 Applied：目标上那张证书还在正常服役，证书 CR 不见了说明不了它有问题
		// （典型场景是 Argo CD 正在换名字重建）。只降 Ready，靠 watch 唤醒。
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotFound,
			fmt.Sprintf("AliyunCertificate %q 不存在", b.Spec.CertificateRef.Name))
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}
	if !certIssued(ac) {
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotReady, "证书尚未通过校验")
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	return r.reconcileBindingReady(ctx, rd, ac)
}

// reconcileBindingReady 处理证书可用之后的步骤 2–8：仲裁 → 装载材料 → 造 client →
// Observe → Apply → 固化状态 → 聚合 Ready。每一步的细节分别在 binding_conflict.go、
// binding_material.go、provider_factory.go、binding_observe.go、binding_apply.go。
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
	// 这一次 targetOf 在生产里不可能失败：NewProviderFactory 的第一条语句就是 targetOf，
	// 认不出的 target.type 早在上面那行就变成了工厂错误。仍然交给 handleFactoryError，
	// 而不是自己写一个分支：同一类接线错误必须给出同一个退避，否则一个等 5 分钟、
	// 一个立刻重来，而这条差异只在有人换掉工厂时才现形——那时它已经是个 bug。
	tg, err := targetOf(b)
	if err != nil {
		return r.handleFactoryError(ctx, rd, err)
	}

	// 5. Observe（spec §6.2 步骤 5）
	obs, oerr := p.Observe(ctx, tg, cl)
	if oerr != nil {
		return r.handleObserveError(ctx, rd, oerr)
	}
	r.noteObserved(rd)

	if r.fenceAccount(rd, obs) {
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	// 走到这里 targetOf 已经确认过 FC3CustomDomain 非 nil，可以安全解引用。
	ensureHTTPS := b.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol
	if obs.CurrentFingerprint == m.Fingerprint && protocolSatisfied(obs, ensureHTTPS) {
		// 幂等短路：只跳过写，不跳过刚才那次 Observe（spec §3「level-triggered」）。
		// 顺手把 appliedFingerprint 与 boundAccountId 补记上——首次接管一个已经装好
		// 同一张证书的域名时，这两项本来是空的。
		//
		// 这里写 appliedFingerprint 并不违反「早退路径绝不写 appliedFingerprint」：
		// 那条规矩防的是 Conflict / CertificateNotFound / 凭证这类**失败与旁路**早退
		// ——它们没有核实过云上装的是哪一张证书，写下去会让证书 controller 的保留护栏 3
		// （appliedFingerprint == 该代 ⇒ 不回收）拿一个凭空的指纹去比对。这里恰好相反：
		// Observe 刚刚核实了云上装的就是 m.Fingerprint，这是一次成功（且无需写云）的
		// apply。不记下来，护栏 3 反而保护不到这个 Binding 真正在服役的那一代。
		//
		// wrote=false：这一轮一个字节都没写云，lastAppliedTime 不该动。
		r.freezeApplied(ctx, rd, obs, m, false)
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	r.noteDrift(ctx, rd, obs, m)

	// 6. Apply（spec §6.2 步骤 6）
	aerr := p.Apply(ctx, tg, cl, m, provider.ApplyOptions{
		EnsureHTTPSProtocol: ensureHTTPS,
		PreviousFingerprint: b.Status.AppliedFingerprint,
	})
	bindingApplyTotal.WithLabelValues(rd.provider, bindingApplyResult(aerr)).Inc()
	if aerr != nil {
		return r.handleApplyError(ctx, rd, aerr)
	}

	// 7. 固化状态（wrote=true：这一轮真的写了云，lastAppliedTime 该跟着走）
	r.freezeApplied(ctx, rd, obs, m, true)

	// 8. Ready = Applied && !Conflict
	aggregateBindingReady(b)
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
}

// providerClient 通过 ProviderFactory 取 provider 与 client；与证书侧的 casClient 同构。
//
// 显式挡住未配置的工厂：接线漏了就在 worker 里 nil 函数调用 panic，而 panic 出在
// reconcile 循环里比一条 ApplyFailed 难查得多。
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

// bindingMeaningfulChange 是**所有**对 AliyunCertificateBinding 的 watch 共用的谓词：
// 只有 spec / annotation / label 变了才算一次值得跑的变化，status-only 的 patch 不算。
//
// 没有它，这个 controller 会被自己唤醒：每一轮成功观测都写一次 status.lastObservedTime
// （binding_observe.go 的 noteObserved），而 metav1.Time 序列化到整秒——生产上一轮
// ≈ 两次云调用 ≈ 1 秒，相邻两轮几乎总落在不同秒，于是 写 status → 自己的 watch 事件 →
// 下一轮 → 再写，循环到偶然两轮撞进同一秒才停。2026-09-06 的现场实测是每个 5 分钟的
// credentialsRequeue 周期里对 FC3 打了约 30 次 UpdateCustomDomain。
//
// 删除仍然触发：CR 的 metadata.generation 在 spec 变化时递增，而 apiserver 在给对象盖
// deletionTimestamp 时也会递增一次（registry.markAsDeleting / rest.BeforeDelete），
// 那一次 Update 因此过得了 GenerationChangedPredicate；finalizer 摘除之后的真删除是
// Delete 事件，三个谓词都只覆写 Update，Delete 一律放行。
//
// annotation / label 也算：kubectl annotate 是运维手动推一轮 reconcile 的通用手法，
// 而套件里多条用例正是靠它把一个已经稳定的对象再唤醒一次。
var bindingMeaningfulChange = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	predicate.LabelChangedPredicate{},
)

// SetupWithManager 注册 watch：主资源，外加证书变化与同目标 peer 的反查。
func (r *AliyunCertificateBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certsv1alpha1.AliyunCertificateBinding{}, builder.WithPredicates(bindingMeaningfulChange)).
		// 证书的 status.current 一变就要唤醒引用它的全部 Binding。用 field index 反查，
		// 而不是遍历：一个 namespace 里可能有几百个 Binding。
		//
		// 这条 watch **刻意不加** bindingMeaningfulChange：它守的正是「证书续期了，
		// 新的一代要推到线上」，而那个信号只存在于 AliyunCertificate 的 status.current 里。
		// generation 谓词会把它整条滤掉，续期从此只能等 DriftCheckInterval——
		// 与 Binding 侧的自唤醒不同，这里的 status 变化来自**另一个**对象，不成环。
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
		// 仲裁只看 creationTimestamp / UID / deletionTimestamp，peer 的 status 怎么变都
		// 改不了胜负，所以这条同样只响应有意义的变化——否则一个 Binding 的自唤醒会被
		// 放大成同目标全体的自唤醒。
		Watches(&certsv1alpha1.AliyunCertificateBinding{},
			handler.EnqueueRequestsFromMapFunc(bindingPeerRequests(mgr.GetClient())),
			builder.WithPredicates(bindingMeaningfulChange)).
		Named("aliyuncertificatebinding").
		Complete(r)
}
