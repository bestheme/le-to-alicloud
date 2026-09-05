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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// bindingRound 是一轮 reconcile 的局部状态。
//
// 把「要落盘的对象」「patch 基准」「本轮算出的落后时长」收在一起，省得每个 helper 都
// 拖着四个参数走；也保证指标刷新与落盘的 status 永远是同一份判定（与证书侧
// patchStatus 里刷 gauge 的理由相同）。
type bindingRound struct {
	b    *certsv1alpha1.AliyunCertificateBinding
	orig *certsv1alpha1.AliyunCertificateBinding
	// provider 是指标 label，取 spec.target.type（有界枚举）。
	provider string
	// lag 是目标落后于证书当前代次的时长；已跟上时为 0。
	lag time.Duration
}

func newBindingRound(b *certsv1alpha1.AliyunCertificateBinding) *bindingRound {
	return &bindingRound{b: b, orig: b.DeepCopy(), provider: b.Spec.Target.Type}
}

// setBindingCondition 写入 condition，observedGeneration 取 CR 当前 generation。
func setBindingCondition(b *certsv1alpha1.AliyunCertificateBinding, condType string,
	status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: b.Generation,
	})
}

func bindingCondTrue(b *certsv1alpha1.AliyunCertificateBinding, condType string) bool {
	return meta.IsStatusConditionTrue(b.Status.Conditions, condType)
}

func bindingCondReason(b *certsv1alpha1.AliyunCertificateBinding, condType string) string {
	if c := meta.FindStatusCondition(b.Status.Conditions, condType); c != nil {
		return c.Reason
	}
	return ""
}

// setBindingReadyFalse 用于「还没走到 Applied / Conflict 就已经确定不 Ready」的早退分支
// （证书不存在、材料无效、域名不覆盖……）。这些原因在 Applied / Conflict 里无处安放，
// 只能直接写进 Ready。
func setBindingReadyFalse(b *certsv1alpha1.AliyunCertificateBinding, reason, message string) {
	setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
}

// targetIdentifier 返回目标标识，供日志使用。**必须 nil-safe**：错误处置路径也会被
// 「target.type 不认识 / 内嵌块缺失」这类失败触发，而那正是 FC3CustomDomain 为 nil 的
// 时候，直接解引用会把一次配置错误变成 panic。
func targetIdentifier(b *certsv1alpha1.AliyunCertificateBinding) string {
	if b.Spec.Target.FC3CustomDomain != nil {
		return b.Spec.Target.FC3CustomDomain.DomainName
	}
	return b.TargetKey()
}

// targetRegion 同上，供 region label 使用；取不到时返回空串（label 允许空值）。
//
// 唯一调用点是 binding_deletion.go 的 Abandon 分支：
// cleanupAbandonedTotal.WithLabelValues(targetRegion(b), providerErrClass(err))。
// 绑定侧三个 gauge 的 label 里都没有 region，所以除它之外没有第二个消费者。
func targetRegion(b *certsv1alpha1.AliyunCertificateBinding) string {
	if b.Spec.Target.FC3CustomDomain != nil {
		return b.Spec.Target.FC3CustomDomain.Region
	}
	return ""
}

// aggregateBindingReady 实现 spec §6.2 步骤 8：Ready = Applied && !Conflict。
func aggregateBindingReady(b *certsv1alpha1.AliyunCertificateBinding) {
	applied := bindingCondTrue(b, certsv1alpha1.ConditionApplied)
	conflict := bindingCondTrue(b, certsv1alpha1.ConditionConflict)
	if applied && !conflict {
		setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionTrue, certsv1alpha1.ReasonApplied, "")
		return
	}
	// Applied 压根不存在时的兜底是 ObserveFailed，不是 ApplyFailed。走到这里而 Applied
	// 缺席只有一种可能：一个从没 Applied 过的 Binding 首次观测就失败，而 noteObserveFailed
	// 刻意什么都不写（绝不凭空造 Applied=False）。对这种对象说「ApplyFailed」是在报告
	// 一次从未发生过的写入——ReasonObserveFailed 常量当初被提前引入，为的正是不让一次
	// 观测失败被说成写入失败，而 Ready 是用户真正会读到的那一处。
	//
	// Applied 存在且为 False 时仍沿用它自己的 reason（下面那一支），ApplyFailed 也在其中。
	reason := certsv1alpha1.ReasonObserveFailed
	// 冲突比「没写成」更能说明问题：写不进去正是因为不该由我们写。
	if conflict {
		reason = bindingCondReason(b, certsv1alpha1.ConditionConflict)
	} else if r := bindingCondReason(b, certsv1alpha1.ConditionApplied); r != "" {
		reason = r
	}
	setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "")
}

// appliedLag 返回目标滞后于证书当前代次的时长；已同步或无从判断时为 0。
//
// 这就是 aliyuncert_binding_applied_age_seconds 的取值来源。用「滞后多久」而不是
// 「生效证书有多老」：后者在一切正常时也会一路涨到证书有效期那么长，会让 spec §10.3
// 的告警式子对每一张健康证书误报。语义已由 team lead 裁决（2026-09-05）。
//
// ac 允许为 nil（证书 CR 不存在）：调用点在取证书之后、任何早退之前，那里 ac 可能没取到。
func (r *AliyunCertificateBindingReconciler) appliedLag(
	b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate,
) time.Duration {
	if ac == nil {
		return 0
	}
	cur := ac.Status.Current
	if cur == nil || cur.Fingerprint == "" || b.Status.AppliedFingerprint == cur.Fingerprint {
		return 0
	}
	lag := r.now().Sub(cur.UploadedAt.Time)
	if lag < 0 {
		return 0
	}
	return lag
}

// patchBinding 用 MergeFrom 提交 status，并在同一处刷新 gauge。
func (r *AliyunCertificateBindingReconciler) patchBinding(ctx context.Context, rd *bindingRound) error {
	recordBindingMetrics(rd)
	return r.patchBindingStatus(ctx, rd)
}

// patchBindingStatus 只落盘 status，**不碰 gauge**。删除分支专用。
//
// 删除分支在 Reconcile 的最前面就 return 了，比 rd.lag = r.appliedLag(...) 那一行还早，
// 所以它手里的 rd.lag 永远是零值。走 patchBinding 就会把
// aliyuncert_binding_applied_age_seconds 刷成 0，而删除路径上再也没有一个知道真实
// lag 的调用点能把它刷回去。于是一个卡在 Terminating 的 Binding 会一直报告 0 秒滞后，
// spec §10.3 的 AliyunCertificateBindingStale 恰恰对最该告警的那个对象永远不触发；
// CleanupFailurePolicy=Block 下这个状态是永久的。
//
// ready / conflict 不在此列，它们由 recordBindingReadiness 单独刷——那两个 gauge 的
// 真相来源是 condition，删除分支照样写得出真值。
//
// 这正是 Reconcile 里那两条「rd.lag 早已算好」长注释在守的不变量，删除分支是唯一
// 会破坏它的地方。修法选「不刷」而不是「在删除分支之前算好 lag」：算 lag 需要先把
// 证书 CR 读出来，那会给每一轮删除都加一次 API 读，而这些 gauge 几个动作之后就会被
// clearBindingMetrics 整条删掉，刷新它们没有任何消费者。
func (r *AliyunCertificateBindingReconciler) patchBindingStatus(ctx context.Context, rd *bindingRound) error {
	return r.Status().Patch(ctx, rd.b, client.MergeFrom(rd.orig))
}
