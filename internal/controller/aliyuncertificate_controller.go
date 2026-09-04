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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// CleanupFailurePolicy 取值。
const (
	CleanupPolicyAbandon = "Abandon"
	CleanupPolicyBlock   = "Block"
)

// CASFactory 按 CR 构造 CAS client。生产实现读凭证 Secret；测试注入 fake。
type CASFactory func(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error)

// AliyunCertificateReconciler 实现 spec §5 的证书 controller。
type AliyunCertificateReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder

	CASFactory CASFactory
	Now        func() time.Time

	ResyncInterval         time.Duration
	CASProbeInterval       time.Duration
	IssuanceStallThreshold time.Duration
	CleanupGracePeriod     time.Duration
	CleanupFailurePolicy   string

	mu       sync.RWMutex
	defaults IssuerDefaults
}

// SetIssuerDefaults 线程安全地设置 flag 默认（测试中会在运行时修改以验证 Pin）。
func (r *AliyunCertificateReconciler) SetIssuerDefaults(d IssuerDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults = d
}

func (r *AliyunCertificateReconciler) issuerDefaults() IssuerDefaults {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaults
}

func (r *AliyunCertificateReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates/finalizers,verbs=update
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile 实现 spec §5.2 的步骤。
func (r *AliyunCertificateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ac := &certsv1alpha1.AliyunCertificate{}
	if err := r.Get(ctx, req.NamespacedName, ac); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := ac.DeepCopy()

	// 0. 删除分支（deletion.go）
	if !ac.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ac, orig)
	}

	if !controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(ac, certsv1alpha1.FinalizerName)
		if err := r.Update(ctx, ac); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	ac.Status.ObservedGeneration = ac.Generation

	// 1. 解析 issuer
	existing := &cmapi.Certificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: certManagerNameFor(ac)}, existing)
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return ctrl.Result{}, err
	}
	issuer, source, ok := ResolveIssuerRef(&ac.Spec.CertificateTemplate, &ac.Status, existing, r.issuerDefaults())
	if !ok {
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonNoIssuer,
			"spec.certificateTemplate.issuerRef 未指定，且 operator 未配置 --default-issuer-name")
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
	}
	ac.Status.EffectiveIssuerRef = &issuer
	log.V(1).Info("issuer resolved", "name", issuer.Name, "kind", issuer.Kind, "source", source)

	// 2. 首次创建前检查 Secret 名冲突
	if existing == nil {
		conflict, err := r.secretNameConflict(ctx, ac)
		if err != nil {
			return ctrl.Result{}, err
		}
		if conflict {
			setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonSecretNameConflict,
				fmt.Sprintf("Secret %q 已存在且不属于本证书", secretNameFor(ac)))
			// 这条路径上没有任何 watch 能唤醒我们：Certificate 从未创建（Owns 无对象），
			// Secret 刻意不进 cache 也不 watch。占用者被删掉后只能靠定时重试自愈。
			return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
		}
	}

	// 3. CreateOrUpdate cert-manager Certificate（只有 spec 真变了才会发出 Update）
	cert := &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: certManagerNameFor(ac), Namespace: ac.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.Spec = desiredCertificateSpec(ac, issuer)
		if cert.Labels == nil {
			cert.Labels = map[string]string{}
		}
		cert.Labels[certsv1alpha1.LabelManaged] = "true"
		return controllerutil.SetControllerReference(ac, cert, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("同步 cert-manager Certificate 失败: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("cert-manager Certificate synced", "operation", op)
	}
	ac.Status.CertManagerCertificateName = cert.Name
	ac.Status.SecretName = cert.Spec.SecretName

	// 4. 镜像 cert-manager status；未 Ready 则等 watch
	mirrorIssuance(ac, cert)
	if issuing, since := certIssuingSince(cert); issuing && !since.IsZero() && r.now().Sub(since) > r.IssuanceStallThreshold {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonIssuanceStalled,
			fmt.Sprintf("cert-manager Issuing 已持续 %s", r.now().Sub(since).Truncate(time.Minute)))
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonIssuanceStalled, "cert-manager issuance stalled")
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
	if !certReady(cert) {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonCertificateNotReady, "等待 cert-manager 签发")
		r.aggregateReady(ac)
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
	}

	// 5–10 由 Task 11 / 12 / 14 接入
	return r.reconcileIssued(ctx, ac, orig, cert)
}

// reconcileIssued 处理 Certificate Ready 之后的步骤 5–10。
func (r *AliyunCertificateReconciler) reconcileIssued(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) (ctrl.Result, error) {
	// 5. 读 Secret 并校验
	b, me := loadMaterial(ctx, r.Client, ac, cert)
	if me != nil {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, me.Reason, me.Message)
		if me.Reason == certsv1alpha1.ReasonSelfSignedDuringIssuance {
			r.Recorder.Event(ac, corev1.EventTypeWarning, me.Reason, me.Message)
		}
		r.aggregateReady(ac)
		// 不清空 status.current：Secret 短暂异常不能被当成新代次
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
	setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "Secret 通过校验")

	// 6–7. 上传（含短路）
	if _, err := r.ensureUploaded(ctx, ac, orig, b); err != nil {
		return r.handleCloudError(ctx, ac, orig, "Upload", err)
	}
	r.setUploadedCondition(ac)

	// 8. 保留策略回收。纯清理动作：失败只发事件并择机重试，绝不降级 Uploaded / Ready（R21）
	if err := r.reclaimOldGenerations(ctx, ac); err != nil {
		return r.handleReclaimError(ctx, ac, orig, err)
	}

	// 9. CAS 探测由 Task 14 接入
	r.aggregateReady(ac)
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
}

// setUploadedCondition 依据 status.current 与 uploadToCAS 设置 Uploaded。
func (r *AliyunCertificateReconciler) setUploadedCondition(ac *certsv1alpha1.AliyunCertificate) {
	switch {
	case !ac.Spec.Aliyun.UploadEnabled():
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadDisabled, "spec.aliyun.uploadToCAS=false")
	case ac.Status.Current != nil && ac.Status.Current.CertID != nil:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionTrue, certsv1alpha1.ReasonReady, fmt.Sprintf("certId=%d", *ac.Status.Current.CertID))
	default:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, "尚未上传")
	}
}

// aggregateReady：Ready = Issued && (Uploaded || !uploadToCAS)。IssuerDefaultDiverged 不参与。
func (r *AliyunCertificateReconciler) aggregateReady(ac *certsv1alpha1.AliyunCertificate) {
	issued := condTrue(ac, certsv1alpha1.ConditionIssued)
	uploadedOK := !ac.Spec.Aliyun.UploadEnabled() || condTrue(ac, certsv1alpha1.ConditionUploaded)
	if issued && uploadedOK {
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "")
		return
	}
	reason := certsv1alpha1.ReasonCertificateNotReady
	if c := meta.FindStatusCondition(ac.Status.Conditions, certsv1alpha1.ConditionIssued); c != nil && c.Status != metav1.ConditionTrue {
		reason = c.Reason
	} else if c := meta.FindStatusCondition(ac.Status.Conditions, certsv1alpha1.ConditionUploaded); c != nil && c.Status != metav1.ConditionTrue {
		reason = c.Reason
	}
	setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "")
}

// handleCloudError 把阿里云 / 凭证错误映射为 condition 与 requeue 策略。
func (r *AliyunCertificateReconciler) handleCloudError(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, op string, err error) (ctrl.Result, error) {
	var ce *credentialsError
	if errors.As(err, &ce) {
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, ce.Reason, ce.Error())
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, r.patchStatus(ctx, ac, orig)
	}
	switch aliyun.ClassOf(err) {
	case aliyun.ClassRetryable:
		reason := certsv1alpha1.ReasonUploadFailed
		var ae *aliyun.Error
		if errors.As(err, &ae) && strings.HasPrefix(ae.Code, "Throttling") {
			reason = certsv1alpha1.ReasonThrottled
		}
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, reason, op+" 失败，将重试")
		r.aggregateReady(ac)
		if perr := r.patchStatus(ctx, ac, orig); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	case aliyun.ClassAuth:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonCredentialsInvalid, op+" 被拒绝: "+err.Error())
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, r.patchStatus(ctx, ac, orig)
	default:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, op+" 失败: "+err.Error())
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonUploadFailed, op+" failed")
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
}

// secretNameConflict 判断目标 Secret 是否已被别人占用。
// Secret 由 manager client 直读 API server（cache 已对 Secret 禁用）。
func (r *AliyunCertificateReconciler) secretNameConflict(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (bool, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: secretNameFor(ac)}, s)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// cert-manager 会在它写的 Secret 上标注来源 Certificate；同名即视为我们的
	return s.Annotations["cert-manager.io/certificate-name"] != certManagerNameFor(ac), nil
}

// SetupWithManager 注册 watch：主资源、owned Certificate、以及引用本证书的 Binding。
func (r *AliyunCertificateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certsv1alpha1.AliyunCertificate{}).
		Owns(&cmapi.Certificate{}).
		Watches(&certsv1alpha1.AliyunCertificateBinding{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
				if !ok || b.Spec.CertificateRef.Name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}}}
			})).
		Named("aliyuncertificate").
		Complete(r)
}
