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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// aggregateBindingReady 写的 reason 是用户在 kubectl describe 里真正读到的那一行。
//
// 最要紧的一格是「Applied 根本不存在」：那只可能是一个从没 Applied 过的 Binding 首次
// 观测就失败，而 noteObserveFailed 刻意什么都不写（绝不凭空造 Applied=False）。对这种
// 对象说 ApplyFailed 是在报告一次从未发生过的写入——ReasonObserveFailed 常量当初被提前
// 引入，为的正是不让观测失败被说成写入失败。
func TestAggregateBindingReady(t *testing.T) {
	cond := func(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: condType, Status: status, Reason: reason,
			LastTransitionTime: metav1.Now()}
	}
	cases := []struct {
		name       string
		conds      []metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "Applied 缺席（首次观测就失败）→ ObserveFailed，不是 ApplyFailed",
			conds: []metav1.Condition{
				cond(certsv1alpha1.ConditionConflict, metav1.ConditionFalse, certsv1alpha1.ReasonNoConflict),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: certsv1alpha1.ReasonObserveFailed,
		},
		{
			name:       "一个 condition 都没有 → 同样是 ObserveFailed",
			conds:      nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: certsv1alpha1.ReasonObserveFailed,
		},
		{
			name: "Applied=False 时沿用它自己的 reason",
			conds: []metav1.Condition{
				cond(certsv1alpha1.ConditionApplied, metav1.ConditionFalse, certsv1alpha1.ReasonApplyFailed),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: certsv1alpha1.ReasonApplyFailed,
		},
		{
			name: "Applied=False/TargetNotFound 同样沿用",
			conds: []metav1.Condition{
				cond(certsv1alpha1.ConditionApplied, metav1.ConditionFalse, certsv1alpha1.ReasonTargetNotFound),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: certsv1alpha1.ReasonTargetNotFound,
		},
		{
			name: "冲突压过一切：写不进去正是因为不该由我们写",
			conds: []metav1.Condition{
				cond(certsv1alpha1.ConditionApplied, metav1.ConditionTrue, certsv1alpha1.ReasonApplied),
				cond(certsv1alpha1.ConditionConflict, metav1.ConditionTrue, certsv1alpha1.ReasonAccountMismatch),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: certsv1alpha1.ReasonAccountMismatch,
		},
		{
			name: "Applied && !Conflict = Ready",
			conds: []metav1.Condition{
				cond(certsv1alpha1.ConditionApplied, metav1.ConditionTrue, certsv1alpha1.ReasonApplied),
				cond(certsv1alpha1.ConditionConflict, metav1.ConditionFalse, certsv1alpha1.ReasonNoConflict),
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: certsv1alpha1.ReasonApplied,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &certsv1alpha1.AliyunCertificateBinding{}
			b.Status.Conditions = append(b.Status.Conditions, c.conds...)
			aggregateBindingReady(b)
			got := condOrZero(b, certsv1alpha1.ConditionReady)
			if got.Status != c.wantStatus || got.Reason != c.wantReason {
				t.Errorf("Ready = %s/%s, want %s/%s", got.Status, got.Reason, c.wantStatus, c.wantReason)
			}
		})
	}
}
