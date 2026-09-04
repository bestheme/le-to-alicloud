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
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// CAS 调用结果的 label 取值。刻意只有三个：label 基数必须是常数，错误码 / certId /
// 指纹这类每次都可能不同的值一旦进了 label，Prometheus 侧就会长出永不消失的 series。
const (
	resultSuccess   = "success"
	resultThrottled = "throttled"
	resultError     = "error"
)

// label 集合刻意很小：绝不把 fingerprint / certId 放进 label（每次轮换都会新增永不消失的 series）。
var (
	certNotAfter = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_not_after_timestamp_seconds",
		Help: "Unix time when the current certificate expires",
	}, []string{"namespace", "name"})
	certReadyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_ready", Help: "1 if Ready condition is True",
	}, []string{"namespace", "name"})
	certIssuanceStalled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_issuance_stalled", Help: "1 if cert-manager issuance is stalled",
	}, []string{"namespace", "name"})
	issuerDefaultDiverged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_issuer_default_diverged", Help: "1 if pinned issuer differs from current --default-issuer-*",
	}, []string{"namespace", "name"})
	casUploadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_upload_total", Help: "CAS upload attempts by result",
	}, []string{"result"})
	casDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_delete_total", Help: "CAS delete attempts by result",
	}, []string{"result"})
	certManagerCertRecreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_certmanager_certificate_recreated_total",
		Help: "Times the cert-manager Certificate was (re)created after first creation; should stay 0",
	}, []string{"namespace", "name"})
	cleanupAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cleanup_abandoned_total",
		Help: "Number of AliyunCertificate deletions that abandoned CAS cleanup",
	}, []string{"region", "reason"})
)

func init() {
	metrics.Registry.MustRegister(certNotAfter, certReadyGauge, certIssuanceStalled, issuerDefaultDiverged,
		casUploadTotal, casDeleteTotal, certManagerCertRecreatedTotal, cleanupAbandonedTotal)
}

// recordCertMetrics 在每次 status patch 前刷新 gauge。
func recordCertMetrics(ac *certsv1alpha1.AliyunCertificate) {
	ns, n := ac.Namespace, ac.Name
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	if ac.Status.Current != nil {
		certNotAfter.WithLabelValues(ns, n).Set(float64(ac.Status.Current.NotAfter.Unix()))
	}
	certReadyGauge.WithLabelValues(ns, n).Set(b2f(condTrue(ac, certsv1alpha1.ConditionReady)))
	certIssuanceStalled.WithLabelValues(ns, n).Set(b2f(condReasonIs(ac, certsv1alpha1.ConditionIssued, certsv1alpha1.ReasonIssuanceStalled)))
	issuerDefaultDiverged.WithLabelValues(ns, n).Set(b2f(condTrue(ac, certsv1alpha1.ConditionIssuerDefaultDiverged)))
}

// clearCertMetrics 在 CR 删除后移除 series。少了这一步，被删掉的证书会永远停在最后一次
// 观测值上——一张 Ready=0 的墓碑会一直告警下去。
func clearCertMetrics(namespace, name string) {
	for _, g := range []*prometheus.GaugeVec{certNotAfter, certReadyGauge, certIssuanceStalled, issuerDefaultDiverged} {
		g.DeleteLabelValues(namespace, name)
	}
}

func condReasonIs(ac *certsv1alpha1.AliyunCertificate, condType, reason string) bool {
	for _, c := range ac.Status.Conditions {
		if c.Type == condType {
			return c.Reason == reason
		}
	}
	return false
}

// casResult 把一次云调用折叠成固定的 result label。限流单独成一档：它是「稍后重试就好」，
// 与真正的失败混在一起会让告警分不清该找人还是该等。
func casResult(err error) string {
	if err == nil {
		return resultSuccess
	}
	var ae *aliyun.Error
	if errors.As(err, &ae) && strings.HasPrefix(ae.Code, "Throttling") {
		return resultThrottled
	}
	return resultError
}

// recordCASDelete 记一次删除。NotFound 算成功：证书已经不在云上，删除的目的已经达到，
// controller 本身也是这么处置的，记成 error 只会造出一条永远不会有人处理的告警。
func recordCASDelete(err error) {
	if err != nil && aliyun.ClassOf(err) == aliyun.ClassNotFound {
		casDeleteTotal.WithLabelValues(resultSuccess).Inc()
		return
	}
	casDeleteTotal.WithLabelValues(casResult(err)).Inc()
}
