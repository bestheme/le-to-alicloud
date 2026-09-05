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

// 本文件是 spec §6.2 步骤 6–8（Apply / 状态固化 / Ready 聚合）的落点：写入成功后的
// 状态跃迁、写入失败的分档处置，以及这一步的指标 label。
//
// setApplied 与四条事件文案是从 binding_observe.go 搬过来的——Task 11 拆文件时它们
// 落在了那里，但 Applied=True 是 Apply 这一步的结论，Observe 只是借用了短路分支。
// 搬家不改一个字节的行为。状态机主干（Reconcile / reconcileBindingReady）仍在
// aliyuncertificatebinding_controller.go。

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// 事件文案。spec §10.2 要求 Message 不含变量——K8s 只聚合 Reason+Message 完全相同的
// 事件，带上域名或指纹就等于每个对象各刷一条，很快把 etcd 里的事件淹掉。变量只进日志。
//
// 四条文案在这里一次定死，是为了让「事件 Message 不含变量」这条约束有一处集中的落点，
// 而不是散在各分支里。observeFailedMessage / driftCorrectedMessage 的调用点在
// binding_observe.go，同包可见。
const (
	observeFailedMessage  = "failed to observe the binding target; the applied state is unchanged"
	driftCorrectedMessage = "target certificate was changed outside the operator; re-applying"
	appliedMessage        = "certificate applied to the binding target"
	applyFailedMessage    = "failed to apply the certificate to the binding target"
)

// bindingApplyResult 把一次写入折叠成固定的 result label（与 CAS 侧同一套取值）。
//
// 限流单独成一档，理由与 casResult 相同：它是「稍后重试就好」，与真正的失败混在一起
// 会让告警分不清该找人还是该等。这里读的是 provider.ProviderError 而不是 aliyun.Error
// ——通用层只认 provider 的分类结论（spec §7 职责边界表）。
func bindingApplyResult(err error) string {
	if err == nil {
		return resultSuccess
	}
	if pe := provider.ErrorOf(err); pe != nil && pe.Code == provider.CodeThrottled {
		return resultThrottled
	}
	return resultError
}

// handleApplyError 处置写入失败。
//
// 与 Observe 的旁路原则相反：Apply 失败就是「没写成」，Applied=False 是对事实的陈述，
// 必须降级。分档只影响重试节奏——限流与瞬时故障走指数退避，凭证问题走 5m 长 requeue，
// 永久错误（参数被拒）走 drift 周期，等人改 spec。
//
// **失败路径一个字节都不写 status.appliedFingerprint / lastAppliedTime**：证书
// controller 的保留护栏 3 会从「observedGeneration 已追平」推断 appliedFingerprint 可信，
// 而这里云上根本没有这张证书，写下去等于让护栏拿一个凭空的指纹去比对。
//
// **不回滚 CAS**（spec §6.2）：CAS 上传成功、FC3 应用失败时多出一张没人引用的证书，
// keepLast 会管住它；删掉再传只会让重试变成上传/删除死循环。
func (r *AliyunCertificateBindingReconciler) handleApplyError(
	ctx context.Context, rd *bindingRound, err error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pe := provider.ErrorOf(err)
	reason := certsv1alpha1.ReasonApplyFailed
	if pe != nil && pe.Reason != "" {
		reason = pe.Reason
	}
	// 错误原文只进日志：事件是广播给用户的对象，云错误里可能夹带 request id。
	//
	// 这行日志的「零凭证泄漏」保证**建立在 provider 契约上**，不是这里自证的：
	// provider.Provider 的注释要求三个方法返回的 error 一律是经过分类的 *ProviderError，
	// 而 ProviderError.Error() 只回显有界的 Code/Retryable/Reason，被包住的 SDK 错误进
	// Unwrap 链、不进消息。上面 pe == nil 的兜底分支因此是**契约被违反**时才走得到的：
	// 今天 fc3.Provider.Apply 的每一条返回路径都过 provider.Errorf / toProviderError，
	// 所以到不了；但将来某个 provider 直接返回原始 SDK 错误的话，这一行渲染的就是它
	// 自己的文本——那时该修的是那个 provider，不是在这里做截断。

	log.Error(err, "写入目标失败", "domain", targetIdentifier(rd.b))
	// 事件先于 setBindingCondition 发：eventOnReasonChange 比的是 rd.orig 上一轮的
	// reason，而 rd.b 马上就要被改成本轮的 reason（spec §10.2「只在状态跃迁时发」——
	// 一次限流会连着失败很多轮，每轮一条 Warning 只是噪声）。
	//
	// 这条跃迁判断是收敛的，而 handleObserveError 那条不是：下面这行
	// setBindingCondition 无条件把 reason 写进 Applied，所以下一轮 rd.orig 上一定读得到
	// 同一个 reason。Observe 侧要额外用 noted 把事件 gate 起来，正是因为它有一支
	// 什么痕迹都不写的分支（noteObserveFailed 在 Applied 不存在时返回 false）。
	r.eventOnReasonChange(rd, certsv1alpha1.ConditionApplied, reason,
		corev1.EventTypeWarning, certsv1alpha1.ReasonApplyFailed, applyFailedMessage)
	setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, reason, "写入目标失败")
	aggregateBindingReady(rd.b)
	if perr := r.patchBinding(ctx, rd); perr != nil {
		return ctrl.Result{}, perr
	}
	switch {
	case pe != nil && pe.Retryable:
		return ctrl.Result{}, err // 指数退避
	case pe != nil && pe.Code == provider.CodeAuth:
		return ctrl.Result{RequeueAfter: credentialsRequeue}, nil
	default:
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, nil
	}
}

// freezeApplied 固化「证书已经在目标上生效」这一结论（spec §6.2 步骤 7）。
//
// 两条成功路径共用它：幂等短路（Observe 核实了云上装的就是这一张，本轮没写云）与真的
// 写完一次。唯一的差别由 wrote 表达——lastAppliedTime 的含义是「上一次**真的写成功**
// 是什么时候」，短路一个字节都没写，就不该动它。
//
// 合并这两段不是为了省几行：它里面带着账号 fencing 的 fail-closed 不变量
// （boundAccountId 只许从**非空**观测里记下来）。留成两份独立副本，以后有人只改其中
// 一份——把观测到的账号做一次归一化、或者放宽这道守卫——另一份会静静地跟着分叉，而
// 两条路径里只有一条有测试会发现。安全属性不该同时存在两个可编辑的副本。
//
// obs.AccountID != "" 这道守卫正是 fenceAccount 敢失败关闭的全部依据：bound 非空就
// 证明 provider 至少为这个目标报出过一次真实账号，此后再读到空值是异常而非常态。
// 从空观测里记一次，这条推理就变成假的了——bound 为空时 fenceAccount 本来就放行，
// 所以写空值不会立刻出事，代价全在「以后闸门凭什么敢关」上。
func (r *AliyunCertificateBindingReconciler) freezeApplied(
	ctx context.Context, rd *bindingRound, obs provider.ObservedState, m provider.CertMaterial, wrote bool,
) {
	b := rd.b
	b.Status.AppliedFingerprint = m.Fingerprint
	// 漂移已经被这一轮解决（写完了，或短路核实了云上装的就是这一张），痕迹必须抹掉，
	// 否则同一张证书日后再次漂移会被 noteDrift 误判成「还是上一轮那次」而不发事件。
	// 短路路径压根不经过 noteDrift，这里是它唯一的清空点。
	b.Status.DriftedFingerprint = ""
	if wrote {
		b.Status.LastAppliedTime = &metav1.Time{Time: r.now()}
	}
	if b.Status.BoundAccountID == "" && obs.AccountID != "" {
		// 首次成功（含短路认下的那一次）才固化账号：走到这里就证明这个账号确实是我们
		// 该写的那个。
		b.Status.BoundAccountID = obs.AccountID
	}
	rd.lag = 0
	r.setApplied(ctx, rd)
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
