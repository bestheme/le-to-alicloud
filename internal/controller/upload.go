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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

// tokenPrefixLen 是 ClientToken 中「CR + 指纹」稳定前缀保留的长度；其余 8 位留给时间后缀。
const tokenPrefixLen = 40

// ensureUploaded 保证 status.current 对应 b 的指纹；需要时上传 CAS。
// 返回 changed=true 表示 status.current 被推进（新代次）。
//
// orig 是本轮 patch 的基准快照。这里要能改它：write-ahead 记录是在本轮中途单独落盘的，
// 基准若不跟着走，轮末的 MergeFrom 就算不出正确的差异（见 patchPendingUpload）。
func (r *AliyunCertificateReconciler) ensureUploaded(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, b *pki.Bundle) (bool, error) {
	log := logf.FromContext(ctx).WithValues("fingerprint", shortFP(b.Fingerprint))

	if ac.Status.Current != nil && ac.Status.Current.Fingerprint == b.Fingerprint {
		return false, nil // 指纹未变，短路
	}

	// 到这里说明要推进代次，而推进代次会调云。informer cache 可能还没追上上一轮 reconcile
	// 自己写下的 status——write-ahead patch 会立刻唤醒下一轮，那时 cache 往往还停在更早的
	// 版本。动手之前直读一次 API server，换成权威值再判断一遍。
	if err := r.refreshUploadState(ctx, ac, orig); err != nil {
		return false, err
	}
	if ac.Status.Current != nil && ac.Status.Current.Fingerprint == b.Fingerprint {
		return false, nil
	}

	now := r.now()
	gen := certsv1alpha1.CertificateGeneration{
		Fingerprint: b.Fingerprint,
		NotBefore:   metav1.NewTime(b.Leaf.NotBefore),
		NotAfter:    metav1.NewTime(b.Leaf.NotAfter),
		UploadedAt:  metav1.NewTime(now),
	}

	if ac.Spec.Aliyun.UploadEnabled() {
		// write-ahead：先把意图写进 status，再调云；崩溃后重启能用同一 token 续上。
		if ac.Status.PendingUpload == nil || ac.Status.PendingUpload.Fingerprint != b.Fingerprint {
			// token 的时间后缀与 StartedAt 必须取同一个 now，两者才对得上。
			startedAt := r.now()
			pending := &certsv1alpha1.PendingUpload{
				Fingerprint: b.Fingerprint,
				CASName:     naming.CASName(ac.Name, b.Fingerprint),
				ClientToken: uploadToken(ac.UID, b.Fingerprint, startedAt),
				StartedAt:   metav1.NewTime(startedAt),
				// 快照下当时的域名：认领这张证书唯一的办法是按域名列出再比名字，而
				// spec.dnsNames 在上传之后随时可能被改，改完就再也找不到它了。
				DomainHint: casDomainHint(ac),
			}
			if err := r.patchPendingUpload(ctx, ac, orig, pending); err != nil {
				return false, err
			}
		}

		cas, err := r.casClient(ctx, ac)
		if err != nil {
			return false, err
		}
		certPEM := b.CertPEM()
		keyPEM, err := b.KeyPEM()
		if err != nil {
			return false, err
		}
		// token 只从 pendingUpload 读：重试必须复用已落盘的那一个，否则幂等失效。
		certID, err := cas.Upload(ctx, ac.Status.PendingUpload.CASName, certPEM, keyPEM, ac.Status.PendingUpload.ClientToken)
		if err != nil {
			// 同名已存在 = 之前上传成功但响应丢失且 token 未命中：兜底按域名查找
			if aliyun.ClassOf(err) == aliyun.ClassPermanent && isDuplicateName(err) {
				if found, ferr := r.findByName(ctx, cas, b, ac.Status.PendingUpload.CASName); ferr == nil && found != 0 {
					certID, err = found, nil
				}
			}
		}
		// 认领成功也算成功：那张证书确实在云上，这一次调用达成了目的。
		casUploadTotal.WithLabelValues(casResult(err)).Inc()
		if err != nil {
			return false, err
		}
		gen.CertID = &certID
		gen.CASName = ac.Status.PendingUpload.CASName
		// 刚上传等于刚确认这张证书在 CAS 侧存在，探测时间戳一并推进。
		ac.Status.CASProbedAt = &metav1.Time{Time: now}
		// 只在内存里抹掉 write-ahead 记录，落盘留给轮末那一次 patchStatus——它同时写
		// current / history / casProbedAt 与 pendingUpload:null。分成两次写会开出一个
		// 「记录已删、代次未记」的窗口：那一刻崩溃，CAS 上这张证书就再没有任何东西指向它。
		ac.Status.PendingUpload = nil
		r.Recorder.Event(ac, corev1.EventTypeNormal, "Uploaded", "certificate uploaded to CAS")
		log.Info("uploaded to CAS", "certId", certID)
	}

	if ac.Status.Current != nil {
		ac.Status.History = append([]certsv1alpha1.CertificateGeneration{*ac.Status.Current}, ac.Status.History...)
	}
	ac.Status.Current = &gen
	return true, nil
}

// uploadToken 生成本次上传的幂等令牌：naming.ClientToken 的稳定前缀 + startedAt 的 8 位
// 时间后缀，仍是 48 个 hex 字符。前缀让同一代证书的重试复用同一 token；时间后缀让
// 「CAS 侧被删后重传」拿到全新 token，不会命中云端仍保留的旧映射。生成后立即写入
// pendingUpload，此后的每次重试都只读那一份，不再重新生成——所以 startedAt 必须与
// PendingUpload.StartedAt 是同一个时刻，token 才能由记录本身复算出来。
func uploadToken(uid types.UID, fingerprint string, startedAt time.Time) string {
	base := naming.ClientToken(uid, fingerprint)
	if len(base) > tokenPrefixLen {
		base = base[:tokenPrefixLen]
	}
	return base + fmt.Sprintf("%08x", uint32(startedAt.Unix())) //nolint:gosec // 只取低 32 位做时间后缀
}

// refreshUploadState 用 APIReader 直读 CR，把代次与 write-ahead 记录换成 API server 上的
// 权威值。落后的 cache 会造成两种真实损害：按它判断会对同一代证书多调一次 CAS Upload
// （幂等，但白费一次云调用，且测试里数得出来）；更糟的是它看不到已落盘的 write-ahead 记录，
// 于是生成新 token 覆盖旧的，重试就不再幂等。
//
// ac 与 orig 都要换：orig 是轮末 MergeFrom 的基准，留着旧 cache 快照的话，基准与目标就
// 分属两个不同版本，算出来的差异可能只含嵌套对象的部分字段，拼出一个缺 notBefore /
// notAfter / uploadedAt 的残缺 CertificateGeneration。
func (r *AliyunCertificateReconciler) refreshUploadState(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate) error {
	reader := client.Reader(r.APIReader)
	if reader == nil {
		reader = r.Client
	}
	fresh := &certsv1alpha1.AliyunCertificate{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(ac), fresh); err != nil {
		return err
	}
	// 两份各自深拷贝：共享指针会让轮末的 MergeFrom 看不出任何差异。
	adoptUploadState(ac, fresh)
	adoptUploadState(orig, fresh)
	return nil
}

// adoptUploadState 把 src 的代次与 write-ahead 记录深拷贝进 dst。
func adoptUploadState(dst, src *certsv1alpha1.AliyunCertificate) {
	dst.Status.Current = src.Status.Current.DeepCopy()
	dst.Status.PendingUpload = src.Status.PendingUpload.DeepCopy()
	dst.Status.CASProbedAt = src.Status.CASProbedAt.DeepCopy()
	dst.Status.History = nil
	if src.Status.History != nil {
		dst.Status.History = make([]certsv1alpha1.CertificateGeneration, len(src.Status.History))
		for i := range src.Status.History {
			src.Status.History[i].DeepCopyInto(&dst.Status.History[i])
		}
	}
}

// patchPendingUpload 用一次最小补丁把 status.pendingUpload 落盘成 want。
//
// 这一次写必须独立于轮末的 patchStatus：write-ahead 记录的全部意义就是先于云调用落盘。
// 反过来，清除它绝不能走这里——那会开出「记录已删、代次未记」的窗口。
//
// Patch 传副本而不是 ac：client 会把响应解码回传入对象，直接用 ac 会把本轮已在内存里
// 设好、尚未持久化的 condition 覆盖成服务端旧值。
//
// 两个赋值都放在 Patch 成功之后：
//   - 写 ac，是因为 patch 失败时内存里不该留下一条服务端没有的记录；
//   - 写 orig，是因为它是轮末 MergeFrom 的基准。基准不跟着走，轮末就发不出
//     pendingUpload:null（首尾都是 nil，差异为空），记录会永远留在 status 里；
//     而在错误路径上，基准反而会凭空 diff 出一个删除，把这次没删成的记录真的删掉。
func (r *AliyunCertificateReconciler) patchPendingUpload(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, want *certsv1alpha1.PendingUpload) error {
	target := ac.DeepCopy()
	target.Status.PendingUpload = want
	if err := r.Status().Patch(ctx, target, client.MergeFrom(ac)); err != nil {
		return err
	}
	ac.Status.PendingUpload = want
	orig.Status.PendingUpload = want.DeepCopy()
	return nil
}

// casClient 通过 CASFactory 获取 client；凭证错误转成 condition reason 由调用方处理。
func (r *AliyunCertificateReconciler) casClient(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
	if r.CASFactory == nil {
		return nil, errors.New("CASFactory 未配置")
	}
	return r.CASFactory(ctx, ac)
}

func isDuplicateName(err error) bool {
	var e *aliyun.Error
	if !errors.As(err, &e) {
		return false
	}
	// 阿里云真实错误码待实测确认；fake 使用 CertNameDuplicated
	return e.Code == "CertNameDuplicated" || e.Code == "DuplicateCertificateName" || e.Code == "CertNameExisted"
}

// shortFP 截取指纹前 8 位用于日志。status 里的指纹是 API 上可写的字段，被人手工改短
// 之后，裸切片会 panic 并把 controller 打进崩溃循环——一次手滑不该拖垮整个 operator。
func shortFP(fp string) string {
	if len(fp) < 8 {
		return fp
	}
	return fp[:8]
}

// casDomainHint 返回 FindUploaded 的 Keyword。CAS 不支持按名字查，只能拿域名当 Keyword
// 拉一批回来再比名字。删除路径读不到 Secret 里的 SAN（Secret 可能已经不在了），只能退回
// spec 声明的域名。
func casDomainHint(ac *certsv1alpha1.AliyunCertificate) string {
	if names := ac.Spec.CertificateTemplate.DNSNames; len(names) > 0 {
		return names[0]
	}
	return ac.Spec.CertificateTemplate.CommonName
}

// casFindHint 决定这一次 FindUploaded 用哪个 Keyword。
//
// pendingUpload.DomainHint 优先级最高：它是写 write-ahead 记录那一刻的快照，而云上那张
// 证书正是按当时的域名建的；spec.dnsNames 之后被改过的话，用它去找只会一无所获，那张
// 证书就被无痕地孤儿化了。preferred 是调用方从 leaf SAN 现算出来的域名（只有拿得到
// Secret 的路径才有）。两者都没有时回退 spec。
func casFindHint(ac *certsv1alpha1.AliyunCertificate, preferred string) string {
	if p := ac.Status.PendingUpload; p != nil && p.DomainHint != "" {
		return p.DomainHint
	}
	if preferred != "" {
		return preferred
	}
	return casDomainHint(ac)
}

// findByName 用域名做 Keyword 拉取后按 Name 过滤（CAS 不支持按名查）。
func (r *AliyunCertificateReconciler) findByName(ctx context.Context, cas aliyun.CASClient, b *pki.Bundle, casName string) (int64, error) {
	hint := ""
	if names := b.DNSNames(); len(names) > 0 {
		hint = names[0]
	}
	list, err := cas.FindUploaded(ctx, hint)
	if err != nil {
		return 0, err
	}
	for _, c := range list {
		if c.Name == casName {
			return c.CertID, nil
		}
	}
	return 0, fmt.Errorf("CAS 中未找到名为 %s 的证书", casName)
}
