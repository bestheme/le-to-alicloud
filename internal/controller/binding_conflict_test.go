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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func candidate(name string, created time.Time, uid string, deleting bool) certsv1alpha1.AliyunCertificateBinding {
	b := certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
			UID:               types.UID(uid),
		},
	}
	if deleting {
		t := metav1.NewTime(created.Add(time.Hour))
		b.DeletionTimestamp = &t
		b.Finalizers = []string{certsv1alpha1.FinalizerName}
	}
	return b
}

func TestPickWinner_OldestWins(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("newer", t0.Add(time.Minute), "uid-a", false),
		candidate("older", t0, "uid-z", false),
	}
	w := pickWinner(items)
	if w == nil || w.Name != "older" {
		t.Fatalf("最早创建者应获胜: %+v", w)
	}
}

func TestPickWinner_TieBreaksOnUID(t *testing.T) {
	// creationTimestamp 只精确到秒，同一秒内建出来的两个 Binding 完全可能撞上。
	// 没有第二把钥匙的话，两边会各自认定自己是胜者并轮流覆写对方的证书。
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("b", t0, "uid-zzz", false),
		candidate("a", t0, "uid-aaa", false),
	}
	w := pickWinner(items)
	if w == nil || string(w.UID) != "uid-aaa" {
		t.Fatalf("同刻应按 UID 取最小: %+v", w)
	}
	// 顺序无关：同一批候选者无论以什么顺序出现，结论必须一致。
	items[0], items[1] = items[1], items[0]
	if w2 := pickWinner(items); w2 == nil || string(w2.UID) != "uid-aaa" {
		t.Fatalf("排序不确定: %+v", w2)
	}
}

func TestPickWinner_SkipsDeleting(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("dying", t0, "uid-a", true),
		candidate("alive", t0.Add(time.Minute), "uid-b", false),
	}
	w := pickWinner(items)
	if w == nil || w.Name != "alive" {
		t.Fatalf("正在删除的不该继续占着目标: %+v", w)
	}
	// 顺序无关也要在「活的 + 正在删除的」混合集合上成立：跳过逻辑放错位置时，
	// 结论会随候选者出现的顺序变化，而两元素同刻那条用例照样能过。
	items[0], items[1] = items[1], items[0]
	if w2 := pickWinner(items); w2 == nil || w2.Name != "alive" {
		t.Fatalf("混合集合排序不确定: %+v", w2)
	}
}

func TestPickWinner_AllDeleting(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{candidate("dying", t0, "uid-a", true)}
	if w := pickWinner(items); w != nil {
		t.Fatalf("没有活着的候选者时应返回 nil: %+v", w)
	}
}

// TestPickWinner_EmptyAndNil 钉住空输入：arbitrate 在 cache 还没看到任何候选者时会把
// 空列表交进来，这条路径必须返回 nil 而不是 panic——它正是「按自己算」兜底的入口。
func TestPickWinner_EmptyAndNil(t *testing.T) {
	if w := pickWinner(nil); w != nil {
		t.Fatalf("nil 切片应返回 nil: %+v", w)
	}
	if w := pickWinner([]certsv1alpha1.AliyunCertificateBinding{}); w != nil {
		t.Fatalf("空切片应返回 nil: %+v", w)
	}
}

// targeted 建一个指向给定域名的候选者，用于 arbitrate 的 fake client 用例。
func targeted(ns, name, uid, domain string, created time.Time) *certsv1alpha1.AliyunCertificateBinding {
	return &certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CertificateRef: certsv1alpha1.LocalObjectReference{Name: "c1"},
			Target: certsv1alpha1.BindingTarget{
				Type: certsv1alpha1.TargetTypeFC3CustomDomain,
				FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
					Region: "cn-hangzhou", DomainName: domain,
				},
			},
		},
	}
}

// arbitrateReconciler 用 fake client 拼一个只够跑仲裁的 reconciler。withIndex=false 用来
// 制造 List 失败（fake client 对未注册的 field selector 直接报错）。
func arbitrateReconciler(t *testing.T, withIndex bool,
	objs ...client.Object) *AliyunCertificateBindingReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := certsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	bld := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	if withIndex {
		bld = bld.WithIndex(&certsv1alpha1.AliyunCertificateBinding{},
			certsv1alpha1.IndexBindingByTarget, bindingTargetIndex)
	}
	c := bld.Build()
	return &AliyunCertificateBindingReconciler{Client: c, APIReader: c}
}

// TestArbitrate_CrossNamespace 钉住跨 namespace 仲裁：TargetKey() 不含 namespace，
// 两个 namespace 指向同一个域名就是同一场冲突，最早创建者获胜。
func TestArbitrate_CrossNamespace(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	older := targeted("team-a", "first", "uid-a", "shared.example.com", t0)
	newer := targeted("team-b", "second", "uid-b", "shared.example.com", t0.Add(time.Minute))
	r := arbitrateReconciler(t, true, older, newer)

	w, err := r.arbitrate(context.Background(), newer)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if w == nil || w.Namespace != "team-a" || w.Name != "first" {
		t.Fatalf("跨 namespace 应选中最早创建者: %+v", w)
	}
	// 胜者自己问同一个问题，答案必须一样，否则两边会互判冲突或互相覆写。
	w2, err := r.arbitrate(context.Background(), older)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if w2 == nil || w2.UID != older.UID {
		t.Fatalf("胜者应选中自己: %+v", w2)
	}
}

// TestArbitrate_EmptyTargetKeyReturnsSelf 钉住空目标键的兜底：不查索引，按自己算，
// 交给后续步骤去失败。空键在索引里根本不存在，查了只会捞回一堆无关对象。
func TestArbitrate_EmptyTargetKeyReturnsSelf(t *testing.T) {
	b := targeted("team-a", "broken", "uid-a", "x.example.com", time.Now())
	b.Spec.Target.FC3CustomDomain = nil // TargetKey() 变成空串
	// 故意不注册索引：一旦这条路径真去 List，fake client 会报错，用例就会红。
	r := arbitrateReconciler(t, false)

	w, err := r.arbitrate(context.Background(), b)
	if err != nil {
		t.Fatalf("空目标键不该报错: %v", err)
	}
	if w != b {
		t.Fatalf("空目标键应原样返回自己: %+v", w)
	}
}

// TestArbitrate_NotInCacheReturnsSelf 钉住「cache 还没看到自己」的兜底：刚创建的对象
// 很常见，此时候选者列表为空，必须按自己算而不是返回 nil——调用方会解引用 winner.UID。
func TestArbitrate_NotInCacheReturnsSelf(t *testing.T) {
	b := targeted("team-a", "fresh", "uid-a", "fresh.example.com", time.Now())
	r := arbitrateReconciler(t, true) // 索引在，但一个对象都没有

	w, err := r.arbitrate(context.Background(), b)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if w != b {
		t.Fatalf("cache 里没有自己时应原样返回自己: %+v", w)
	}
}

// TestArbitrate_ListErrorPropagates 钉住唯一的错误路径：List 失败时返回错误而不是
// 悄悄按自己算——那等于在看不清全局的情况下写云。
func TestArbitrate_ListErrorPropagates(t *testing.T) {
	b := targeted("team-a", "b1", "uid-a", "err.example.com", time.Now())
	r := arbitrateReconciler(t, false, b) // 没注册索引，List 必失败

	w, err := r.arbitrate(context.Background(), b)
	if err == nil {
		t.Fatalf("List 失败时应返回错误, 却拿到: %+v", w)
	}
}

// TestBindingPeerRequests 钉住同目标 peer 唤醒：胜者被删掉时，输者必须被入队，
// 否则它的恢复窗口就是 DriftCheckInterval（默认一小时），期间域名上没有证书。
func TestBindingPeerRequests(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	winner := targeted("team-a", "first", "uid-a", "shared.example.com", t0)
	loser := targeted("team-b", "second", "uid-b", "shared.example.com", t0.Add(time.Minute))
	unrelated := targeted("team-c", "other", "uid-c", "other.example.com", t0)
	r := arbitrateReconciler(t, true, winner, loser, unrelated)
	mapFn := bindingPeerRequests(r.Client)

	got := mapFn(context.Background(), winner)
	names := map[string]bool{}
	for _, req := range got {
		names[req.Namespace+"/"+req.Name] = true
	}
	if !names["team-b/second"] {
		t.Fatalf("胜者变化必须唤醒跨 namespace 的输者, 实得: %v", got)
	}
	if names["team-c/other"] {
		t.Fatalf("不同目标的 Binding 不该被唤醒: %v", got)
	}
}

// TestBindingPeerRequests_Guards 钉住两条护栏：空目标键与非 Binding 对象都返回 nil，
// 前者与 bindingTargetIndex 的空键跳过一致（否则会捞回一堆无关对象）。
func TestBindingPeerRequests_Guards(t *testing.T) {
	b := targeted("team-a", "broken", "uid-a", "x.example.com", time.Now())
	b.Spec.Target.FC3CustomDomain = nil
	// 索引照常注册，库里也放一个同域名的对象：护栏一旦失效，空键 List 会走完全程并
	// 返回 bindingRequests 造出的**非 nil 空切片**，与护栏的 nil 可区分——
	// 下面断言的是严格的 nil，所以「碰巧为空」不会蒙混过关。
	r := arbitrateReconciler(t, true, targeted("team-a", "ok", "uid-b", "x.example.com", time.Now()))
	mapFn := bindingPeerRequests(r.Client)

	if got := mapFn(context.Background(), b); got != nil {
		t.Fatalf("空目标键应返回 nil: %v", got)
	}
	if got := mapFn(context.Background(), &certsv1alpha1.AliyunCertificate{}); got != nil {
		t.Fatalf("非 Binding 对象应返回 nil: %v", got)
	}
}
