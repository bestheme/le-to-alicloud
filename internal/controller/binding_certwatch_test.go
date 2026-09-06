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
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// certReadCounter 数「证书 controller 跑了几轮」。
//
// 代理指标取 Reconcile 第一句那次 `r.Get(ctx, req.NamespacedName, ac)`：每一轮都会执行，
// 且在任何早退之前。没有更直接的口子——controller-runtime 自己的 reconcile 计数器在
// internal 包里，而一轮什么都没改的 reconcile 不写 status、不发事件，从外面完全看不见。
// （refreshUploadState 那次直读不行：ensureUploaded 在指纹没变时先短路，根本走不到。）
//
// 按对象分账：envtest 从不回收 namespace，别的用例的证书随时可能被唤醒。
type certReadCounter struct {
	client.Client
	mu sync.Mutex
	n  map[string]int
}

func (c *certReadCounter) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*certsv1alpha1.AliyunCertificate); ok {
		c.mu.Lock()
		c.n[key.String()]++
		c.mu.Unlock()
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *certReadCounter) reads(ns, name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[ns+"/"+name]
}

// quiesceCertReads 等到某张证书上的 reconcile 停止，用来取一个可信的基线。
func quiesceCertReads(ns, name string) {
	last := -1
	EventuallyWithOffset(1, func() bool {
		cur := certReads.reads(ns, name)
		stable := cur == last
		last = cur
		return stable
	}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
}

// 这条 spec 守的是证书 controller 那一侧的谓词（aliyuncertificate_controller.go 的
// Watches(&AliyunCertificateBinding{})）。三处谓词里只有它的移除不会让别的用例发红：
// Binding 的 status patch 唤醒证书之后，证书那一轮通常算不出 status 差量，不回灌到
// Binding，所以自唤醒那两条照样绿。少了这条 spec，那道谓词就没人守。
var _ = Describe("证书 controller：Binding 的 status 写入不唤醒它", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("status-only patch 不触发证书 reconcile，generation 变化仍然触发", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTP")

		quiesceCertReads(ns, "c1")
		before := certReads.reads(ns, "c1")

		// 正是绑定 controller 每一轮都会写的那一处：只动 status，不碰 spec / annotation /
		// label。修复前这样一次写入会把证书 controller 也拖起来空跑一轮。
		b := getBinding(ctx, ns, "b1")
		b.Status.LastObservedTime = &metav1.Time{Time: time.Now().Add(time.Minute)}
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())

		Consistently(func() int { return certReads.reads(ns, "c1") }, "3s", "200ms").
			Should(Equal(before), "Binding 的 status 写入不该把证书 controller 拖起来空跑一轮")

		// 反方向同样要钉住：这条 watch 存在的理由是让回收护栏与删除阻塞及时生效
		// （spec §5.1），谓词只该滤掉 status，不该把整条 watch 变成摆设。
		b = getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool { return certReads.reads(ns, "c1") > before })
	})
})
