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

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// setCondition 写入 condition，observedGeneration 取 CR 当前 generation。
func setCondition(ac *certsv1alpha1.AliyunCertificate, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ac.Generation,
	})
}

func condTrue(ac *certsv1alpha1.AliyunCertificate, condType string) bool {
	return meta.IsStatusConditionTrue(ac.Status.Conditions, condType)
}

// patchStatus 用 MergeFrom 补丁提交 status，避免整对象 Update 的冲突。
//
// gauge 在这里统一刷新：每一条 return 路径最终都要经过一次 patchStatus，挂在这里就不会
// 漏掉哪一条分支，指标与落盘的 status 也永远是同一份判定。
func (r *AliyunCertificateReconciler) patchStatus(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate) error {
	recordCertMetrics(ac)
	return r.Status().Patch(ctx, ac, client.MergeFrom(orig))
}

// mirrorIssuance 把 cert-manager Certificate 的关键 status 镜像到本 CR。
func mirrorIssuance(ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) {
	ac.Status.Issuance = &certsv1alpha1.IssuanceStatus{
		Revision:               cert.Status.Revision,
		RenewalTime:            cert.Status.RenewalTime,
		FailedIssuanceAttempts: cert.Status.FailedIssuanceAttempts,
		LastFailureTime:        cert.Status.LastFailureTime,
	}
}

func certCondition(cert *cmapi.Certificate, t cmapi.CertificateConditionType) *cmapi.CertificateCondition {
	for i := range cert.Status.Conditions {
		if cert.Status.Conditions[i].Type == t {
			return &cert.Status.Conditions[i]
		}
	}
	return nil
}

func certReady(cert *cmapi.Certificate) bool {
	c := certCondition(cert, cmapi.CertificateConditionReady)
	return c != nil && c.Status == cmmeta.ConditionTrue
}

// certIssuingSince 返回 Issuing 是否为 True 及其起始时间。
func certIssuingSince(cert *cmapi.Certificate) (bool, time.Time) {
	c := certCondition(cert, cmapi.CertificateConditionIssuing)
	if c == nil || c.Status != cmmeta.ConditionTrue {
		return false, time.Time{}
	}
	if c.LastTransitionTime == nil {
		return true, time.Time{}
	}
	return true, c.LastTransitionTime.Time
}
