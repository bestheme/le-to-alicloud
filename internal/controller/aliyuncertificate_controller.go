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
	// Now 是时钟注入点，nil 表示用真实时间。构造时可以直接赋值；manager 跑起来之后
	// 只许经 SetNow 改（测试会在运行中推进假时钟，而每一轮 reconcile 都在读它）。
	Now func() time.Time

	// ResyncInterval 与 Now 一样是运行中会被改的字段（用例把它调短来验证自愈路径），
	// 而每一轮 reconcile 的返回值都在读它。构造时可以直接赋值；manager 跑起来之后
	// 只许经 SetResyncInterval 改，读只许经 resyncInterval()。
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

// SetNow 线程安全地替换时钟。上一个 reconcile 的收尾常常还在跑（RequeueAfter 与 watch
// 都会自己排队），换钟的那一刻它可能正好在读，所以必须走锁。
func (r *AliyunCertificateReconciler) SetNow(fn func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Now = fn
}

func (r *AliyunCertificateReconciler) now() time.Time {
	r.mu.RLock()
	fn := r.Now
	r.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// SetResyncInterval 线程安全地改周期。理由与 SetNow 逐字相同：用例在 manager 跑起来
// 之后调短它来验证「靠 RequeueAfter 自愈」那几条路径，而那一刻很可能正有一轮 reconcile
// 在读——裸赋值就是 -race 抓得到的数据竞争。
func (r *AliyunCertificateReconciler) SetResyncInterval(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResyncInterval = d
}

func (r *AliyunCertificateReconciler) resyncInterval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ResyncInterval
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
		// Update 会触发本对象的 watch 事件，不需要显式 requeue。
		return ctrl.Result{}, nil
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
	r.detectIssuerDivergence(ac, source)
	log.V(1).Info("issuer resolved", "name", issuer.Name, "kind", issuer.Kind, "source", source)

	// 2. 检查 Secret 名冲突：首次创建前，以及之后每一次 spec.secretName 改指到新目标时。
	//
	// 第二个条件不能省。spec.secretName 可变（CRD 上没有不可变约束，也没有 webhook），
	// 而护栏一旦只在首次创建时跑，用户后来把它改指到别人的 Secret，下面第 3 步就会把新
	// 名字 Update 到 Certificate 上——cert-manager 随即用本证书的私钥覆写受害 Secret，
	// 并把归属注解改成指向本 CR。那之后删除期的注解复核会一路放行，于是覆写加删除。
	// 所以真正的护栏必须在这里、在 Update 之前拦住，删除期的复核只是兜底。
	conflict := false
	if existing == nil || existing.Spec.SecretName != secretNameFor(ac) {
		var err error
		if conflict, err = r.secretNameConflict(ctx, ac); err != nil {
			return ctrl.Result{}, err
		}
	}
	if conflict && existing == nil {
		// Certificate 还没建出来：没有在役 Secret 可维护，后面每一步都无从谈起，只能早退。
		// 这条路径上也没有任何 watch 能唤醒我们（Owns 无对象，Secret 刻意不进 cache 也不
		// watch），占用者被删掉后只能靠定时重试自愈。
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse,
			certsv1alpha1.ReasonSecretNameConflict, secretNameConflictMessage(ac))
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, r.patchStatus(ctx, ac, orig)
	}

	// 3. CreateOrUpdate cert-manager Certificate（只有 spec 真变了才会发出 Update）
	//
	// 冲突时跳过这一步，但**不早退**：Certificate 与它的在役 Secret 都还在，里面躺着一张
	// 有效、正在服役的证书，cert-manager 照样会给它续期。早退会让 operator 在冲突挂着的
	// 整段时间里彻底停止维护它——不探测 CAS、不回收、不复读 Secret、续期出的新代次永远
	// 传不上去——与步骤 4「签发停滞绝不结束本轮」是同一条原则。后面每一步用的都是
	// existing 与它的在役 Secret（见 loadMaterial 里的 servingSecretName），不会碰到用户
	// 刚指过去的那个受害 Secret。
	cert := existing
	if !conflict {
		cert = &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: certManagerNameFor(ac), Namespace: ac.Namespace}}
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
		if op == controllerutil.OperationResultCreated && ac.Status.CertManagerCertificateName != "" {
			// 之前已经创建过一次又不见了：重建会消耗 LE 配额，必须可观测
			certManagerCertRecreatedTotal.WithLabelValues(ac.Namespace, ac.Name).Inc()
			r.Recorder.Event(ac, corev1.EventTypeWarning, "CertificateRecreated", "cert-manager Certificate was recreated")
		}
		ac.Status.CertManagerCertificateName = cert.Name
		// 这一行是 secretNameGuardHolding 的全部依据：status.secretName 只在这里被赋值，
		// 冲突时走不到，于是它与 spec 分叉，aggregateReady 据此一票否决 Ready。
		ac.Status.SecretName = cert.Spec.SecretName
	}

	// 4. 镜像 cert-manager status；未 Ready 则等 watch
	mirrorIssuance(ac, cert)
	// 停滞只置 condition + 事件，绝不结束本轮（spec §5.2 步骤 4 也只要求这三样）。
	// 续期停滞是能持续好几天的场景（LE 速率限制、DNS-01 solver 坏掉），而这期间
	// status.current 指着的仍是一张有效、正在服役的证书。早退会让 operator 在这几天里
	// 完全停止维护它：不探测 CAS 存在性、不按保留策略回收、不复读 Secret。
	stalled := false
	if issuing, since := certIssuingSince(cert); issuing && !since.IsZero() && r.now().Sub(since) > r.IssuanceStallThreshold {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonIssuanceStalled,
			fmt.Sprintf("cert-manager Issuing 已持续 %s", r.now().Sub(since).Truncate(time.Minute)))
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonIssuanceStalled, "cert-manager issuance stalled")
		stalled = true
	}
	if !certReady(cert) {
		// 停滞时不覆写 Issued：CertificateNotReady 会把「已经卡了好几个小时」这条信息
		// 抹平成「正在签发」，aliyuncert_certificate_issuance_stalled 也跟着掉回 0。
		if !stalled {
			setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonCertificateNotReady, "等待 cert-manager 签发")
		}
		r.aggregateReady(ac)
		if stalled {
			// 首次签发就停滞：Certificate 还没 Ready，没有任何 watch 会因为「又过了一小时」
			// 而唤醒我们，只能定时重来。
			return ctrl.Result{RequeueAfter: r.resyncInterval()}, r.patchStatus(ctx, ac, orig)
		}
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
	}

	return r.reconcileIssued(ctx, ac, orig, cert, stalled)
}

// reconcileIssued 处理 Certificate Ready 之后的步骤 5–10。
//
// stalled 表示本轮已经判定签发停滞。此时 Secret 里躺着的仍是上一代次那张有效证书，
// 校验会照常通过——但 Issued 必须保持 False/IssuanceStalled，否则「续期已经卡了三天」
// 这件事在 condition 与指标上都消失了。除这一句之外，后面的探测 / 上传 / 回收全都照跑。
func (r *AliyunCertificateReconciler) reconcileIssued(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate, stalled bool) (ctrl.Result, error) {
	// 5. 读 Secret 并校验
	b, me := loadMaterial(ctx, r.Client, ac, cert)
	if me != nil {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, me.Reason, me.Message)
		if me.Reason == certsv1alpha1.ReasonSelfSignedDuringIssuance {
			r.Recorder.Event(ac, corev1.EventTypeWarning, me.Reason, me.Message)
		}
		r.aggregateReady(ac)
		// 不清空 status.current：Secret 短暂异常不能被当成新代次
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, r.patchStatus(ctx, ac, orig)
	}
	if !stalled {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "Secret 通过校验")
	}

	// 9. CAS 存在性探测（12h 节流）。spec 把它列在上传之后，代码里必须提前到这里：探测
	// 的全部作用就是清空 certId 与指纹，好让紧接着的 ensureUploaded 把它当成新代次重传；
	// 排在上传之后的话，这一轮就什么也不会发生。
	primary := ""
	if names := b.DNSNames(); len(names) > 0 {
		primary = names[0]
	}
	missing, err := r.probeCAS(ctx, ac, primary)
	if err != nil {
		// R25：探测失败绝不能结束本轮。casProbedAt 只在列表成功后推进，所以「只缺一个
		// ListUserCertificateOrder 权限」这类持续性失败会让每一轮都在这里重新探测；一旦
		// 早退，续期签出来的新指纹就永远走不到 ensureUploaded，而 Uploaded / Ready 还停在
		// 上一轮的 True —— 证书悄悄地再也不更新了。只记一笔，继续往下走。
		r.noteProbeFailure(ctx, ac, err)
	}
	if missing {
		// 探测的结论必须先落盘。ensureUploaded 会用 APIReader 直读 CR 把代次换成权威值，
		// 只清在内存里的 certId / 指纹会被原样读回来，于是重传永远不会发生。
		if err := r.patchStatus(ctx, ac, orig); err != nil {
			return ctrl.Result{}, err
		}
		orig = ac.DeepCopy()
	}

	// 6–7. 上传（含短路）
	if _, err := r.ensureUploaded(ctx, ac, orig, b); err != nil {
		return r.handleCloudError(ctx, ac, orig, "Upload", err)
	}
	r.setUploadedCondition(ac)

	// 8. 保留策略回收。纯清理动作：失败只发事件并择机重试，绝不降级 Uploaded / Ready（R21）
	if err := r.reclaimOldGenerations(ctx, ac); err != nil {
		return r.handleReclaimError(ctx, ac, orig, err)
	}

	// 10. 汇总 Ready 并落盘
	r.aggregateReady(ac)
	return ctrl.Result{RequeueAfter: r.resyncInterval()}, r.patchStatus(ctx, ac, orig)
}

// detectIssuerDivergence：已 Pin 且 flag 当前值不同 → 打一个不参与 Ready 的 condition，
// 并在跳变时发一次 Warning。改 flag 不该悄悄把所有证书重签一遍（那是 Pin 的意义），但也
// 不该悄无声息——否则「改了默认却没生效」这件事只有下一次续期时才会被发现。
func (r *AliyunCertificateReconciler) detectIssuerDivergence(ac *certsv1alpha1.AliyunCertificate, source IssuerSource) {
	d := r.issuerDefaults().Ref()
	diverged := source == IssuerSourceStatus && d != nil && !IssuerRefEqual(*d, *ac.Status.EffectiveIssuerRef)
	was := condTrue(ac, certsv1alpha1.ConditionIssuerDefaultDiverged)
	if diverged {
		setCondition(ac, certsv1alpha1.ConditionIssuerDefaultDiverged, metav1.ConditionTrue, certsv1alpha1.ReasonIssuerDefaultDiverged,
			fmt.Sprintf("已固化 %s/%s，operator 默认现为 %s/%s；如需切换请显式设置 spec.certificateTemplate.issuerRef",
				ac.Status.EffectiveIssuerRef.Kind, ac.Status.EffectiveIssuerRef.Name, d.Kind, d.Name))
		if !was {
			r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonIssuerDefaultDiverged, "pinned issuer differs from operator default")
		}
		return
	}
	setCondition(ac, certsv1alpha1.ConditionIssuerDefaultDiverged, metav1.ConditionFalse, certsv1alpha1.ReasonReady, "")
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
//
// secretName 冲突一票否决。冲突期间 operator 照常维护在役证书，Issued 与 Uploaded 都会
// 正常变成 True——它们描述的是「在役的那张证书好不好」，而那张确实是好的。但用户要的
// 状态并没有达成：他改了 spec.secretName，operator 拒绝执行。Ready 是对外的那一句话，
// 必须说实话。
func (r *AliyunCertificateReconciler) aggregateReady(ac *certsv1alpha1.AliyunCertificate) {
	if secretNameGuardHolding(ac) {
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse,
			certsv1alpha1.ReasonSecretNameConflict, secretNameConflictMessage(ac))
		return
	}
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
		return ctrl.Result{RequeueAfter: r.resyncInterval()}, r.patchStatus(ctx, ac, orig)
	}
}

// handleNonFatalCloudError 处理「云调用失败了，但当前这张证书没有任何问题」的那一类错误
// （控制器裁决 R21 回收失败、R24 探测失败）。
//
// 这两件事都是旁路动作：当前代次早就上传成功、正在服役。删不掉一张没人引用的旧证书、
// 或者列不出证书清单，都说明不了它有任何问题。若沿用 handleCloudError，一次失败就会把
// Uploaded / Ready 打成 False，Binding 侧会跟着认为证书不可用而连锁停摆——用一个无害的
// 失败换来一场真实的故障。RAM 只少给一个 ListUserCertificateOrder 权限就足以触发这条路径，
// 而且每 12h 复发一次。所以这里一个 condition 都不碰，只发一条 Warning 事件。
//
// logMsg 与 kv 只进日志：事件是广播给用户的对象，云错误原文可能夹带 request id 之类的细节。
func (r *AliyunCertificateReconciler) handleNonFatalCloudError(
	ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate,
	err error, reason, message, logMsg string, kv ...any,
) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(err, logMsg, kv...)
	r.Recorder.Event(ac, corev1.EventTypeWarning, reason, message)
	// aggregateReady 只读 Issued / Uploaded，上面没动过，判定与成功时完全一致。
	r.aggregateReady(ac)
	if perr := r.patchStatus(ctx, ac, orig); perr != nil {
		return ctrl.Result{}, perr
	}
	if aliyun.ClassOf(err) == aliyun.ClassRetryable {
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	}
	return ctrl.Result{RequeueAfter: r.resyncInterval()}, nil
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
	return !secretOwnedByUs(s, ac), nil
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
