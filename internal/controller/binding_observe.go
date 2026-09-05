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

// 本文件是 spec §6.2 步骤 5（Observe）的落点：幂等短路的判据、账号 fencing、
// 漂移检测，以及这一步的失败处置与事件规则。从 aliyuncertificatebinding_controller.go
// 原样搬出，行为零变化——那个文件已经是包里最大的一个，而 Apply 与删除两步还要往上加。
// 状态机主干（Reconcile / reconcileBindingReady）留在原处。

package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// 事件文案。spec §10.2 要求 Message 不含变量——K8s 只聚合 Reason+Message 完全相同的
// 事件，带上域名或指纹就等于每个对象各刷一条，很快把 etcd 里的事件淹掉。变量只进日志。
//
// applyFailedMessage 的调用点是 Task 12 的 handleApplyError；四条文案在这里一次定死，
// 是为了让「事件 Message 不含变量」这条约束有一处集中的落点，而不是散在各分支里。
const (
	observeFailedMessage  = "failed to observe the binding target; the applied state is unchanged"
	driftCorrectedMessage = "target certificate was changed outside the operator; re-applying"
	appliedMessage        = "certificate applied to the binding target"
	applyFailedMessage    = "failed to apply the certificate to the binding target"
)

// protocolSatisfied 判断当前 protocol 是否已经满足 ensureHTTPSProtocol 的要求。
//
// ensureHTTPSProtocol=false 时永远满足：spec §13 明说不越权改线上配置，用户没要求就
// 不看这一项，否则每一轮都会因为「没开 HTTPS」而重写一次。
func protocolSatisfied(obs provider.ObservedState, ensureHTTPS bool) bool {
	if !ensureHTTPS {
		return true
	}
	for _, part := range strings.Split(obs.Protocol, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "HTTPS") {
			return true
		}
	}
	return false
}

// handleObserveError 处置 Observe 的失败。
//
// 分两档，是「旁路失败不降级」这条原则的落点：
//   - TargetNotFound / Auth：这两件事直接说明「写不进去」——域名不存在、AK 被吊销。
//     照常把 Applied 与 Ready 打成 False，并用固定长 requeue（域名可能由 Terraform
//     稍后创建；AK 等人来换），不做指数退避。
//   - 其余：一次 InternalError 或网络抖动说明不了目标上那张证书有任何问题。碰
//     condition 就是用一个无害的失败换来一场真实的告警（控制器裁决 R21/R24/R25）。
//     只发 Warning 事件、保留既有判定，Retryable 的交给 controller-runtime 退避。
func (r *AliyunCertificateBindingReconciler) handleObserveError(
	ctx context.Context, rd *bindingRound, err error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pe := provider.ErrorOf(err)
	switch {
	case pe != nil && pe.Code == provider.CodeTargetNotFound:
		setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, pe.Reason, "目标不存在")
		aggregateBindingReady(rd.b)
		return ctrl.Result{RequeueAfter: targetNotFoundRequeue}, r.patchBinding(ctx, rd)
	case pe != nil && pe.Code == provider.CodeAuth:
		setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, pe.Reason, "凭证被拒绝")
		aggregateBindingReady(rd.b)
		return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
	}
	log.Error(err, "Observe 失败", "domain", targetIdentifier(rd.b))
	// Applied 的 status 一个字节都不改——那才是「旁路失败不降级」的含义。只把 reason
	// 换成 ObserveFailed：kubectl describe 因此看得出「证书还生效着，但上一轮观测失败」，
	// 上面那个跃迁判断也才有可比较的痕迹。
	//
	// 事件与这条痕迹必须同进同退，所以先写痕迹、再按它决定发不发。noteObserveFailed
	// 在 Applied 不存在时刻意什么都不写（绝不凭空造 Applied=False），而跃迁判断读的
	// 正是这条痕迹——少了它，比较基准永远是空串，**永不收敛**：一个从没 Applied 过的
	// Binding 首次观测撞上 retryable 错误，会以 5ms、10ms、20ms… 的退避一轮轮重来，
	// 每一轮都发一条 Warning。客户端事件聚合只是把它压成一条 Count 疯涨的记录，
	// 这正是「只在跃迁时发」要防的那场刷屏。
	// 这种对象「还没绑成功」由 Ready 说就够了，不需要事件再重复一遍。
	noted := noteObserveFailed(rd.b)
	if noted {
		// 只在 reason 相对上一轮变化时发事件（spec §10.2「只在状态跃迁时发」）。
		r.eventOnReasonChange(rd, certsv1alpha1.ConditionApplied, certsv1alpha1.ReasonObserveFailed,
			corev1.EventTypeWarning, certsv1alpha1.ReasonObserveFailed, observeFailedMessage)
	}
	aggregateBindingReady(rd.b)
	if perr := r.patchBinding(ctx, rd); perr != nil {
		return ctrl.Result{}, perr
	}
	if pe != nil && pe.Retryable {
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	}
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, nil
}

// eventOnReasonChange 实现 spec §10.2 的「只在状态跃迁时发」。
//
// 判据是 rd.orig——本轮开始前从 API server 读到的那一份，也就是「上一轮」的结论。
// 同一个 condition 的 reason 没变就不发：失败会一轮一轮地重来，每轮一条事件不但没有
// 新信息，还会把这个对象上真正的跃迁淹掉。
func (r *AliyunCertificateBindingReconciler) eventOnReasonChange(
	rd *bindingRound, condType, reason, eventType, eventReason, message string,
) {
	if bindingCondReason(rd.orig, condType) == reason {
		return
	}
	r.Recorder.Event(rd.b, eventType, eventReason, message)
}

// noteObserveFailed 把 Applied 的 reason 改成 ObserveFailed，status 不动。
// 返回是否真的留下了痕迹。
//
// 只在 condition 已经存在时改：从没 Applied 过的对象上凭空造一个 Applied=False，
// 就把旁路失败变成了真降级，正是这条路径要避免的事。status 不变，
// metav1.SetStatusCondition 的 lastTransitionTime 语义也不受影响（这里直接改字段，
// 不走 SetStatusCondition，免得它按「新 condition」处理）。
//
// 返回值是给事件用的：跃迁判断的比较基准就是这条痕迹，没写下痕迹就没有可收敛的基准，
// 调用方必须据此跳过事件（见 handleObserveError）。
//
// 反方向由 setApplied 负责：观测恢复后它会把 reason 写回 Applied，否则一次瞬时故障
// 会让一个健康的 Binding 永远显示 ObserveFailed。
func noteObserveFailed(b *certsv1alpha1.AliyunCertificateBinding) bool {
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == certsv1alpha1.ConditionApplied {
			b.Status.Conditions[i].Reason = certsv1alpha1.ReasonObserveFailed
			b.Status.Conditions[i].Message = "上一轮观测失败，保留既有判定"
			return true
		}
	}
	return false
}

// fenceAccount 实现账号 fencing（spec §6.2 步骤 5）：status.boundAccountId 一旦固化，
// 观测到的账号就必须一直是它。返回 true 表示被拦下、本轮不许写。
//
// 触发场景是凭证 Secret 被换成了另一个账号的 AK，而同名域名恰好也存在于那个账号。
// 没有这道闸，operator 会安静地把证书写进陌生人的资源。
//
// **观测到空账号也算不一致，一样拦下（fail closed）**。brief 给的是放行的写法，这里
// 按 spec §6.2 步骤 5 的原文改成失败关闭：它要求的是「非空的 boundAccountId 与观测
// 不一致就停写」，而空值与一个非空值就是不一致。失败关闭在这里不花任何代价——
// boundAccountId 只会从**非空**观测里记下来（见短路分支的 obs.AccountID != "" 守卫），
// 所以 bound 非空本身就证明 provider 至少为这个目标报出过一次真实账号；此后再读到空
// 值是异常，不是正常状态。放行的代价恰恰是这道闸存在的理由：AK 被换成另一个账号的、
// 同名域名恰好也存在于那里、而那个账号的响应没带账号 ID——闸门打开，下一步的 Apply
// 就把证书写进了陌生人的域名。
func (r *AliyunCertificateBindingReconciler) fenceAccount(rd *bindingRound, obs provider.ObservedState) bool {
	bound := rd.b.Status.BoundAccountID
	// bound 为空 = 还没固化过，本轮正是首次观测，放行（随后由短路 / Apply 记下来）。
	if bound == "" || bound == obs.AccountID {
		return false
	}
	setFencedConflict(rd)
	return true
}

// setFencedConflict 写 Conflict=True/AccountMismatch，并在上一轮已经是同一个判定时
// 沿用它的 lastTransitionTime。
//
// 不能直接用 setBindingCondition：步骤 2 的仲裁排在 Observe 之前（顺序由 spec §6.2
// 定死），每一轮都会先把 Conflict 写成 False/NoConflict，fencing 随即再翻回 True。
// 两次翻转都会让 meta.SetStatusCondition 重新盖一个 lastTransitionTime，于是
// ①「从什么时候开始被 fencing 的」被刷成了「上一次 reconcile 是什么时候」，运维读不
// 出真正的起点；② 每一次外部唤醒都多出一次非空 status patch 和它引发的一轮 reconcile。
func setFencedConflict(rd *bindingRound) {
	cond := metav1.Condition{
		Type:               certsv1alpha1.ConditionConflict,
		Status:             metav1.ConditionTrue,
		Reason:             certsv1alpha1.ReasonAccountMismatch,
		Message:            "目标所属账号与首次绑定时不一致",
		ObservedGeneration: rd.b.Generation,
	}
	// 判据取 rd.orig（本轮开始前 API server 上的那一份），而不是 rd.b——b 里的
	// Conflict 刚被仲裁改成过 False，已经不是「上一轮的结论」了。
	if prev := meta.FindStatusCondition(rd.orig.Status.Conditions, certsv1alpha1.ConditionConflict); prev != nil &&
		prev.Status == metav1.ConditionTrue && prev.Reason == certsv1alpha1.ReasonAccountMismatch {
		cond.LastTransitionTime = prev.LastTransitionTime
	}
	meta.SetStatusCondition(&rd.b.Status.Conditions, cond)
}

// noteDrift 在观测到「既不是我们上次写的、也不是当前该写的」证书时记一笔。
//
// 判定要求 obs.CurrentFingerprint 非空：空证书是「还没绑过」，不是漂移。
// 等于 appliedFingerprint 是正常轮换（我们写的那张还在，只是证书续期了）。
func (r *AliyunCertificateBindingReconciler) noteDrift(
	ctx context.Context, rd *bindingRound, obs provider.ObservedState, m provider.CertMaterial,
) {
	cur := obs.CurrentFingerprint
	if cur == "" || cur == rd.b.Status.AppliedFingerprint || cur == m.Fingerprint {
		return
	}
	bindingDriftTotal.WithLabelValues(rd.provider).Inc()
	logf.FromContext(ctx).Info("检测到云侧证书漂移",
		"domain", targetIdentifier(rd.b),
		"observed", shortFP(cur), "expected", shortFP(m.Fingerprint))
	r.Recorder.Event(rd.b, corev1.EventTypeWarning, certsv1alpha1.ReasonDriftCorrected, driftCorrectedMessage)
}

// setApplied 置 Applied=True，并只在状态跃迁时发一次 Normal 事件。
//
// 每轮都发会让一个健康的 Binding 每小时刷一条事件；只在 False→True 时发，事件流才
// 真正对应「发生了什么」。
//
// 无条件重写 reason（meta.SetStatusCondition 即使 status 不变也会更新 Reason/Message），
// 这正是 noteObserveFailed 的反方向：上一轮把 reason 改成了 ObserveFailed 而 status
// 留在 True，观测一旦恢复就必须写回 Applied，否则那个诊断痕迹会永久留在一个健康对象上。
func (r *AliyunCertificateBindingReconciler) setApplied(ctx context.Context, rd *bindingRound) {
	was := bindingCondTrue(rd.b, certsv1alpha1.ConditionApplied)
	setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionTrue, certsv1alpha1.ReasonApplied, "")
	if !was {
		logf.FromContext(ctx).Info("证书已在目标上生效",
			"domain", targetIdentifier(rd.b), "fingerprint", shortFP(rd.b.Status.AppliedFingerprint))
		r.Recorder.Event(rd.b, corev1.EventTypeNormal, certsv1alpha1.ReasonApplied, appliedMessage)
	}
}
