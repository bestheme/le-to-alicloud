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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	if w2 := pickWinner(items); string(w2.UID) != "uid-aaa" {
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
}

func TestPickWinner_AllDeleting(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{candidate("dying", t0, "uid-a", true)}
	if w := pickWinner(items); w != nil {
		t.Fatalf("没有活着的候选者时应返回 nil: %+v", w)
	}
}
