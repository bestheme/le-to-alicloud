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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// bindingCleanupAbandonedMessage 是 CleanupAbandoned 事件的固定文案。域名与错误原文
// 只进日志：事件面向用户广播，不该夹带 request id 这类细节。
const bindingCleanupAbandonedMessage = "gave up unbinding the certificate from the target after the cleanup grace period; see operator logs"

// reconcileBindingDelete 实现 spec §6.5。
//
//	Orphan（默认）→ 直接摘 finalizer，云侧不动。一个 kubectl delete 不应打穿生产 HTTPS。
//	Unbind        → Observe 确认目标上那张确实是自己写的，才清空 certConfig；
//	                纯 HTTPS 域名同时降为 HTTP。受 cleanup-grace-period /
//	                cleanup-failure-policy 约束。
//
// 有界清理是这条分支存在的理由：没有宽限期，一朵永久失败的云就能把对象永远钉在
// Terminating 上，而 finalizer 是我们自己加的——那是一个只能人工 patch 才解得开的死局。
func (r *AliyunCertificateBindingReconciler) reconcileBindingDelete(ctx context.Context, rd *bindingRound) (ctrl.Result, error) {
	b := rd.b
	if !controllerutil.ContainsFinalizer(b, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	if b.Spec.DeletionPolicy != certsv1alpha1.DeletionPolicyUnbind {
		return r.finishBindingDeletion(ctx, rd)
	}
	if b.Status.AppliedFingerprint == "" {
		// 从没写成功过，云上没有属于我们的东西。
		return r.finishBindingDeletion(ctx, rd)
	}

	// 宽限期从「真正开始清理」起算，与证书 controller 同一考量。
	//
	// 落盘一律走 patchBindingStatus 而不是 patchBinding：删除分支手里的 rd.lag 是零值，
	// 刷 gauge 会把 applied_age 永久钉在 0 上，理由见 patchBindingStatus 的注释。
	if b.Status.CleanupStartedAt == nil {
		b.Status.CleanupStartedAt = &metav1.Time{Time: r.now()}
		if err := r.patchBindingStatus(ctx, rd); err != nil {
			return ctrl.Result{}, err
		}
		rd.orig = b.DeepCopy()
	}

	if err := r.unbindTarget(ctx, rd); err != nil {
		elapsed := r.now().Sub(b.Status.CleanupStartedAt.Time)
		if !shouldAbandonCleanup(r.CleanupFailurePolicy, elapsed, r.CleanupGracePeriod) {
			log.Error(err, "解绑失败，重试中", "domain", targetIdentifier(b))
			// 集群里也要留下痕迹，与证书 controller 的清理失败分支同构：只有日志的话，
			// 一个卡在 Terminating 的 Binding 上 kubectl describe 看到的还是删除前的
			// 状态——没有 condition、没有事件、（gauge 已按上面的理由不再刷新）没有
			// 指标变化，运维手里一条线索都没有。
			setBindingReadyFalse(b, certsv1alpha1.ReasonCleanupFailed, "解绑失败，重试中: "+err.Error())
			if perr := r.patchBindingStatus(ctx, rd); perr != nil {
				return ctrl.Result{}, perr
			}
			// ready gauge 要跟着 condition 走，否则看板上这个卡死的对象仍然是绿的；
			// applied_age 不刷，理由见 patchBindingStatus。
			//
			// **必须在 patch 成功之后**，否则会立一块永远清不掉的墓碑：对象在本轮进行中
			// 被删干净了（Reconcile 开头的直读看见它还在，此后另一条路径摘掉了 finalizer），
			// 先刷 gauge 就会把 ready=0 重新建出来，紧接着 patch 以 NotFound 失败——而
			// clearBindingMetrics 在这条路径上再也不会被走到。于是一个已经不存在的对象留下
			// 一条永久为 0 的告警 series，正是 clearBindingMetrics 存在的理由被反过来违反。
			// patch 成功了才说明对象还在，这时刷 gauge 才有对应的清理点。
			//
			// 这个窗口现在很窄。从前它每次删除都会发生：删除分支读的是 informer cache，
			// 对象真删之后缓存里那份带 finalizer 的旧版本还会再唤起一轮。Reconcile 改用
			// APIReader 直读之后（见那里的注释），那一轮在开头就 NotFound 早退了，剩下的
			// 只有真正的并发。窄不等于没有，顺序照旧。
			recordBindingReadiness(rd)
			return ctrl.Result{}, err // 指数退避
		}
		// Abandon：把足以人工兜底的信息留在日志里，然后走完删除。
		log.Error(err, "cleanup abandoned",
			"domain", targetIdentifier(b),
			"fingerprint", shortFP(b.Status.AppliedFingerprint),
			"gracePeriod", r.CleanupGracePeriod)
		r.Recorder.Event(b, corev1.EventTypeWarning, certsv1alpha1.ReasonCleanupAbandoned, bindingCleanupAbandonedMessage)
		cleanupAbandonedTotal.WithLabelValues(targetRegion(b), providerErrClass(err)).Inc()
	}
	return r.finishBindingDeletion(ctx, rd)
}

// unbindTarget 只解绑属于自己的那一张证书（spec §6.5）。
//
// 身份比对（指纹或 certRef，见 identityOf）放在通用层而不是 provider：Cleanup 的签名里
// 没有 appliedFingerprint，而「幂等判断的真相来源是 Observe」（spec §7 职责边界表）。
// 先 Observe 再决定要不要 Cleanup，也让「目标已经不存在」和「上面是别人的证书」两种
// 情形都以成功收场。
func (r *AliyunCertificateBindingReconciler) unbindTarget(ctx context.Context, rd *bindingRound) error {
	b := rd.b

	// 证书 CR 可能已经先被删掉了（Argo CD 会同时 prune 两者）。取不到就传 nil，
	// 凭证只能来自 Binding 自己的 credentialsRef——取不到时 ProviderFactory 会给出
	// CredentialsNotFound，走 Abandon 分支。
	var ac *certsv1alpha1.AliyunCertificate
	fetched := &certsv1alpha1.AliyunCertificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}, fetched)
	switch {
	case err == nil:
		ac = fetched
	case !apierrors.IsNotFound(err):
		return err
	}

	// 走 providerClient 而不是直接调 r.ProviderFactory：那层薄封装挡的是「工厂没接线」
	// 的 nil 调用（Task 8 补的守卫）。在这条路径上尤其不能少——删除分支里一次 panic 会
	// 打在 worker 上，而对象已经带着 finalizer 进了 Terminating，正是最不该失去
	// reconcile 能力的时刻；走错误返回则会被下面的宽限期兜住。
	p, cl, err := r.providerClient(ctx, b, ac)
	if err != nil {
		return err
	}
	tg, err := targetOf(b)
	if err != nil {
		return err
	}

	obs, err := p.Observe(ctx, tg, cl)
	if err != nil {
		if pe := provider.ErrorOf(err); pe != nil && pe.Code == provider.CodeTargetNotFound {
			return nil // 域名没了，解绑的目的已经达到
		}
		return err
	}
	// 账号 fencing 在这条路径上同样成立。只比指纹是不够的：指纹是**叶子证书的哈希**，
	// 同一张证书部署到两个账号的同名域名上，指纹一模一样。迁移期的现实场景——Binding
	// 先在账号 A 上 apply 并固化了 boundAccountId，随后凭证被指向账号 B，于是普通
	// reconcile 全部停在 fenceAccount 上一个字节都不写；此时删除对象，Unbind 观测到的
	// 是账号 B 里那个同名域名，指纹又恰好相等——闸门形同虚设，我们会把一个从来不属于
	// 本 Binding 的生产 HTTPS 域名降成 HTTP。
	//
	// spec §6.5 只点名了指纹，但同一道闸在 spec §6.2 步骤 5 的写入路径上是强制的；
	// 删除同样是写入，没有理由在这里放行（controller 裁决，2026-09-05）。
	if bound := b.Status.BoundAccountID; bound != "" && bound != obs.AccountID {
		logf.FromContext(ctx).Info("目标所属账号与首次绑定时不一致，跳过解绑",
			"domain", tg.Identifier)
		return nil
	}
	// want 在这里没有意义（删除分支没有材料），只比 current 与 applied。
	id := identityOf(b, obs, provider.CertMaterial{})
	if id.current != id.applied {
		// 目标上不是我们写的那张：可能是别的 Binding 接管了，也可能是人工换过。
		// 动它等于替别人做主。
		logf.FromContext(ctx).Info("目标上的证书不是本 Binding 写入的，跳过解绑",
			"domain", tg.Identifier, "observed", identityLabel(id, id.current))
		return nil
	}
	return p.Cleanup(ctx, tg, cl, provider.DeletionPolicyUnbind)
}

// finishBindingDeletion 摘 finalizer 并清掉指标 series。
//
// NotFound 必须吸收掉：对象没了，摘 finalizer 的目的已经达到，把它当错误往上抛只会
// 推高 controller_runtime_reconcile_errors_total 并打一条 reconciler error 日志——
// 而那个指标正是运维配告警的地方。
//
// 这里守的是**并发**，不再是一条每次都走的路径。从前 Reconcile 读的是 informer cache：
// finalizer 摘掉、对象被 API server 真正删除之后，缓存里那份带 finalizer 的旧版本还会
// 再唤起一轮删除，那一轮的 Update 必然 NotFound，于是**每一次 Binding 删除**（Orphan
// 也不例外）都会抛一次。Reconcile 改用 APIReader 直读之后，那一轮在开头就 NotFound
// 早退，进不到这里。剩下的是真正的并发：本轮进行中对象被另一条路径删掉。
// 罕见不是不会——这行吸收留着，代价为零。
func (r *AliyunCertificateBindingReconciler) finishBindingDeletion(ctx context.Context, rd *bindingRound) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(rd.b, certsv1alpha1.FinalizerName)
	if err := client.IgnoreNotFound(r.Update(ctx, rd.b)); err != nil {
		return ctrl.Result{}, err
	}
	// 对象没了，它的 gauge 也必须跟着消失：留下的 Ready=0 会一直告警下去。
	clearBindingMetrics(rd.b.Namespace, rd.b.Name, rd.provider)
	logf.FromContext(ctx).Info("binding deleted", "name", rd.b.Name)
	return ctrl.Result{}, nil
}

// shouldAbandonCleanup 是「还要不要继续重试解绑」这个决定的全部内容。
//
// 抽成纯函数是为了能不起 envtest 就把它表死（见 TestShouldAbandonCleanup）：绑定侧的
// CleanupFailurePolicy 挂在常驻 reconciler 上，用例里翻转它会波及并发跑着的其他用例，
// 所以这条分支拿不到 envtest 覆盖；而「Block 下永远不放弃」是有界清理的另一半——它决定
// 一个对象会不会被永远钉在 Terminating 上，不该只靠「证书侧同构」来担保。
//
// policy 的取值由 --cleanup-failure-policy 的 flag 校验限死为 Abandon | Block，
// 因此这里只认 Block，其余一切（含空串）按 Abandon 处理，与证书侧逐字一致。
func shouldAbandonCleanup(policy string, elapsed, grace time.Duration) bool {
	return policy != CleanupPolicyBlock && elapsed >= grace
}

// providerErrClass 给 cleanup_abandoned_total 的 reason label 一个有界取值。
//
// 注意 `cleanup_abandoned_total{reason}` 这一个 label 上跑着**两套词表**：证书 controller
// 传的是 `aliyun.ErrClass` 的字符串（Permanent / Retryable / Auth / NotFound），绑定
// controller 传的是 `provider.Code*`（TargetNotFound / Auth / Throttled / Retryable /
// Permanent / InvalidClient / InvalidTarget）。两边取值都有界，不会造成基数爆炸，但看板
// 与告警的作者必须知道同一个 label 里会同时出现两种词表——这一条要进 README 的已知限制。
func providerErrClass(err error) string {
	if pe := provider.ErrorOf(err); pe != nil {
		return pe.Code
	}
	return provider.CodePermanent
}
