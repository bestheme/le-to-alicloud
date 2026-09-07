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
	"errors"
	"strings"
	"time"

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

// crLabels 是「一个 CR 一条 series」的 label 集合。抽成变量不只是去重：它保证五个
// 指标的 label 顺序永远一致，recordCertMetrics 里 WithLabelValues(ns, n) 才是对的。
var crLabels = []string{labelNamespace, "name"}

// 跨多个指标复用的 label 名。抽成常量与 crLabels 同理：写错一个字面量，只有到
// WithLabelValues panic 时才会发现。
const (
	labelResult    = "result"
	labelProvider  = "provider"
	labelNamespace = "namespace"
)

// label 集合刻意很小：绝不把 fingerprint / certId 放进 label（每次轮换都会新增永不消失的 series）。
var (
	certNotAfter = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_not_after_timestamp_seconds",
		Help: "Unix time when the current certificate expires",
	}, crLabels)
	certReadyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_ready", Help: "1 if Ready condition is True",
	}, crLabels)
	certIssuanceStalled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_issuance_stalled", Help: "1 if cert-manager issuance is stalled",
	}, crLabels)
	issuerDefaultDiverged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_issuer_default_diverged", Help: "1 if pinned issuer differs from current --default-issuer-*",
	}, crLabels)
	casUploadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_upload_total", Help: "CAS upload attempts by result",
	}, []string{labelResult})
	casDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_delete_total", Help: "CAS delete attempts by result",
	}, []string{labelResult})
	certManagerCertRecreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_certmanager_certificate_recreated_total",
		Help: "Times the cert-manager Certificate was (re)created after first creation; should stay 0",
	}, crLabels)
	// 两个 controller 共用这一个计数器，reason label 上因此跑着**两套词表**
	// （见 providerErrClass 的注释）。help 文本必须自己说清楚这件事：代码注释给不到
	// 看板与告警的作者，他们读到的只有这一行。
	cleanupAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cleanup_abandoned_total",
		Help: "Deletions that abandoned cloud cleanup after the grace period: " +
			"AliyunCertificate giving up on CAS certificate deletion, or " +
			"AliyunCertificateBinding giving up on unbinding the certificate from its target. " +
			"The reason label carries two vocabularies, one per controller " +
			"(aliyun error class vs provider error code)",
	}, []string{"region", "reason"})
	// 与 cleanupAbandonedTotal 同一个理由：这条路径也会「留下需要人工收拾的东西」，
	// 而事件会随 namespace 一起过期，指标才是能长期告警的那一份痕迹。
	//
	// label 只有 namespace，没有 name：CR 删掉之后 clearCertMetrics 会清掉按 CR 打的
	// gauge，counter 却必须活过对象本身——按 name 打 label 等于给每一个删掉的 CR 留一条
	// 永不消失的 series。
	secretDeletionSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_secret_deletion_skipped_total",
		Help: "Deletions that left the TLS Secret in place because it is not owned by " +
			"this AliyunCertificate (cert-manager.io/certificate-name points elsewhere)",
	}, []string{labelNamespace})
	// spec §10.1 的两个 API 级指标。casUploadTotal / casDeleteTotal 只覆盖写通道，
	// 而 ListUserCertificateOrder 才是限流最紧（QPS 8、burst 1）也最容易被 RAM 权限
	// 卡住的那一条：没有它就没人答得上「探测是不是一直在失败」「list 配额打满没有」。
	aliyunAPIRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_aliyun_api_requests_total",
		Help: "Aliyun OpenAPI calls by service, action and result code",
	}, []string{"service", "action", "code"})
	aliyunAPIDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aliyuncert_aliyun_api_duration_seconds",
		Help:    "Aliyun OpenAPI call latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"service", "action"})
	bindingReadyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_ready", Help: "1 if the binding Ready condition is True",
	}, crLabels)
	bindingConflictGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_conflict", Help: "1 if the binding Conflict condition is True",
	}, crLabels)
	// aliyuncert_binding_applied_age_seconds 是 spec §10.1 点名「必须告警」的那一个：
	// 独有的失败模式是「证书续期成功了，但没推到线上」。
	//
	// 语义是**滞后时长**（证书 CR 的 status.current 推进之后、本 Binding 尚未把该代
	// 应用到目标的持续时间；同步时为 0），而不是字面的「生效证书年龄」：后者在一切
	// 正常时也会随着证书服役时间一路涨到 90 天，而 spec §10.3 的告警式子是
	// `applied_age > 86400 and certificate_ready == 1`——用字面语义，每一张健康证书在
	// 第二天就会误报。指标名不变；spec §10.1 与 §10.3 的文字由 Plan 3 的 spec 对齐
	// 任务更新（team lead 裁决，2026-09-05）。
	bindingAppliedAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_applied_age_seconds",
		Help: "Seconds the target has been behind the certificate's current generation; 0 when up to date",
	}, []string{labelNamespace, "name", labelProvider})
	bindingApplyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_binding_apply_total", Help: "Binding apply attempts by provider and result",
	}, []string{labelProvider, labelResult})
	bindingDriftTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_binding_drift_detected_total",
		Help: "Times the target certificate was found changed outside the operator",
	}, []string{labelProvider})
)

// service label 的取值。与阿里云的 RAM code 无关，取的是可读的服务简称。
const (
	serviceCAS = "cas"
	serviceFC3 = "fc"
	serviceOSS = "oss"
)

func init() {
	metrics.Registry.MustRegister(certNotAfter, certReadyGauge, certIssuanceStalled, issuerDefaultDiverged,
		casUploadTotal, casDeleteTotal, certManagerCertRecreatedTotal, cleanupAbandonedTotal,
		secretDeletionSkippedTotal,
		aliyunAPIRequestsTotal, aliyunAPIDuration,
		bindingReadyGauge, bindingConflictGauge, bindingAppliedAge, bindingApplyTotal, bindingDriftTotal)
}

// aliyunAPICallRecorder 返回绑定到某个 service label 的 OnCall 钩子。
//
// 走回调而不是让 pkg/aliyun 直接注册指标：那是一个纯 SDK 封装包，不该依赖
// controller-runtime 的 metrics registry。label 基数由调用方保证有界——action 是包里的
// 常量，code 来自 aliyun.callCode（服务端错误码或固定字符串），两者都不含 Message、
// certId、指纹这类每次都不同的值。
func aliyunAPICallRecorder(service string) func(action, code string, d time.Duration) {
	return func(action, code string, d time.Duration) {
		aliyunAPIRequestsTotal.WithLabelValues(service, action, code).Inc()
		aliyunAPIDuration.WithLabelValues(service, action).Observe(d.Seconds())
	}
}

// b2f 把布尔折成 gauge 的 0/1。证书侧与绑定侧共用一份。
func b2f(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// recordBindingMetrics 在每次 status patch 前刷新 binding 侧 gauge。
func recordBindingMetrics(rd *bindingRound) {
	recordBindingReadiness(rd)
	bindingAppliedAge.WithLabelValues(rd.b.Namespace, rd.b.Name, rd.provider).Set(rd.lag.Seconds())
}

// recordBindingReadiness 只刷 ready / conflict，**不碰 applied_age**。
//
// 删除分支专用：那里 rd.lag 恒为零值（见 patchBindingStatus），刷 applied_age 是在
// 写一个假值；但 ready 必须跟着走——清理失败时 Ready 已经被打成 False，gauge 还停在 1
// 的话，一个卡在 Terminating 里的对象在看板上依旧是健康的，而 CleanupFailurePolicy=Block
// 下这个状态会一直持续到有人来处理。
func recordBindingReadiness(rd *bindingRound) {
	ns, n := rd.b.Namespace, rd.b.Name
	bindingReadyGauge.WithLabelValues(ns, n).Set(b2f(bindingCondTrue(rd.b, certsv1alpha1.ConditionReady)))
	bindingConflictGauge.WithLabelValues(ns, n).Set(b2f(bindingCondTrue(rd.b, certsv1alpha1.ConditionConflict)))
}

// clearBindingMetrics 在 Binding 删除后移除 series，否则墓碑会一直告警下去。
func clearBindingMetrics(namespace, name, providerName string) {
	bindingReadyGauge.DeleteLabelValues(namespace, name)
	bindingConflictGauge.DeleteLabelValues(namespace, name)
	bindingAppliedAge.DeleteLabelValues(namespace, name, providerName)
}

// recordCertMetrics 在每次 status patch 前刷新 gauge。
func recordCertMetrics(ac *certsv1alpha1.AliyunCertificate) {
	ns, n := ac.Namespace, ac.Name
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
