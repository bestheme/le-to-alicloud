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

	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// lessBinding 定义同目标 Binding 的全序：先比 creationTimestamp，再比 UID。
//
// 必须是全序而不只是「谁更早」：creationTimestamp 只精确到秒，同一秒内建出来的两个
// Binding 在两个副本眼里可能是不同的顺序，于是双方各自认定自己是胜者，轮流把对方的
// 证书覆盖掉。UID 是集群内唯一且不变的，作为第二把钥匙让结论与观察者无关。
func lessBinding(a, b *certsv1alpha1.AliyunCertificateBinding) bool {
	if !a.CreationTimestamp.Time.Equal(b.CreationTimestamp.Time) {
		return a.CreationTimestamp.Time.Before(b.CreationTimestamp.Time)
	}
	return a.UID < b.UID
}

// pickWinner 从同目标候选者里选出唯一允许写云的那一个；没有活着的候选者时返回 nil。
//
// 正在删除的不参与：它马上就要交出目标（Unbind 甚至已经在解绑了）。让它继续占着，
// 会把接班者卡在 Conflict 里直到 finalizer 走完。
func pickWinner(items []certsv1alpha1.AliyunCertificateBinding) *certsv1alpha1.AliyunCertificateBinding {
	var winner *certsv1alpha1.AliyunCertificateBinding
	for i := range items {
		c := &items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if winner == nil || lessBinding(c, winner) {
			winner = c
		}
	}
	return winner
}

// arbitrate 查出同目标的全部 Binding 并选出胜者。
//
// 用 informer cache（带 field index）而不是 live read：仲裁规则是确定性的，cache 短暂
// 落后最多让某一轮多判一次冲突，下一轮就会自愈；而 live read 要为每个 Binding 每小时
// 打一次 API server 的全量 List（APIReader 不支持自定义 index），代价大得多。
// 这与保留策略回收的 live read 不同——那里的 TOCTOU 会真的删掉在用的证书。
//
// 跨 namespace 一起比：同一个 FC3 域名在云上只有一份，两个 namespace 的 Binding 指向
// 它就是真冲突，不该因为 namespace 不同而被判成两件事。
//
// 已知限制：`--watch-namespaces` 生效时 cache 只覆盖被 watch 的 namespace，跨 namespace
// 仲裁随之退化为跨已 watch namespace 仲裁——cache 里看不见的 Binding 不会成为候选者。
// 代码上无法修（cache 就是那么大），只能在文档的「已知限制」里写明。
func (r *AliyunCertificateBindingReconciler) arbitrate(
	ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding,
) (*certsv1alpha1.AliyunCertificateBinding, error) {
	key := b.TargetKey()
	if key == "" {
		// 索引不了的目标（CEL 已挡住这种形状）：当作只有自己，交给后续步骤去失败。
		return b, nil
	}
	list := &certsv1alpha1.AliyunCertificateBindingList{}
	if err := r.List(ctx, list, client.MatchingFields{certsv1alpha1.IndexBindingByTarget: key}); err != nil {
		return nil, err
	}
	w := pickWinner(list.Items)
	if w == nil {
		// cache 还没看到自己（刚创建的对象常见）。自己活着，就先按自己算。
		return b, nil
	}
	return w, nil
}
