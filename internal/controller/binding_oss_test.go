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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：OSS 目标", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
		resetOSS()
	})

	It("证书 uploadToCAS=false 时 Applied=False/CASUploadRequired，且不碰 OSS", func() {
		ns := newNamespace(ctx)
		domain, bucket := ossDomainFor(ns, "o1"), ossBucketFor(ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain})
		createCertificate(ctx, ns, "c1", domain) // 这个 helper 关着上传
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createOSSBinding(ctx, ns, "o1", "c1", bucket, domain)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCASUploadRequired
		})
		Expect(bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionReady).Reason).To(Equal(certsv1alpha1.ReasonCASUploadRequired))
		Expect(currentOSS().ListCallsFor(bucket, domain)).To(BeZero())
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(BeZero())
		b := getBinding(ctx, ns, "o1")
		Expect(b.Status.AppliedFingerprint).To(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(BeEmpty())
	})
})
