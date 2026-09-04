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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func (r *AliyunCertificateReconciler) ensureUploaded(ctx context.Context, ac *certsv1alpha1.AliyunCertificate, b *pki.Bundle) (bool, error) {
	log := logf.FromContext(ctx).WithValues("fingerprint", b.Fingerprint[:8])

	if ac.Status.Current != nil && ac.Status.Current.Fingerprint == b.Fingerprint {
		return false, nil // 指纹未变，短路
	}

	// 到这里说明要推进代次，而推进代次会调云。informer cache 可能还没追上上一轮 reconcile
	// 自己写下的 status——write-ahead patch 会立刻唤醒下一轮，那时 cache 往往还停在更早的
	// 版本。动手之前直读一次 API server，换成权威值再判断一遍。
	if err := r.refreshUploadState(ctx, ac); err != nil {
		return false, err
	}
	if ac.Status.Current != nil && ac.Status.Current.Fingerprint == b.Fingerprint {
		return false, nil
	}

	gen := certsv1alpha1.CertificateGeneration{
		Fingerprint: b.Fingerprint,
		NotBefore:   metav1.NewTime(b.Leaf.NotBefore),
		NotAfter:    metav1.NewTime(b.Leaf.NotAfter),
		UploadedAt:  metav1.NewTime(r.now()),
	}

	if ac.Spec.Aliyun.UploadEnabled() {
		// write-ahead：先把意图写进 status，再调云；崩溃后重启能用同一 token 续上。
		if ac.Status.PendingUpload == nil || ac.Status.PendingUpload.Fingerprint != b.Fingerprint {
			pending := &certsv1alpha1.PendingUpload{
				Fingerprint: b.Fingerprint,
				CASName:     naming.CASName(ac.Name, b.Fingerprint),
				ClientToken: r.uploadToken(ac, b.Fingerprint),
				StartedAt:   metav1.NewTime(r.now()),
			}
			if err := r.patchPendingUpload(ctx, ac, pending); err != nil {
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
					certID = found
					err = nil
				}
			}
			if err != nil {
				return false, err
			}
		}
		gen.CertID = &certID
		gen.CASName = ac.Status.PendingUpload.CASName
		// 刚上传等于刚确认这张证书在 CAS 侧存在，探测时间戳一并推进。
		ac.Status.CASProbedAt = &metav1.Time{Time: r.now()}
		if err := r.patchPendingUpload(ctx, ac, nil); err != nil {
			return false, err
		}
		r.Recorder.Event(ac, corev1.EventTypeNormal, "Uploaded", "certificate uploaded to CAS")
		log.Info("uploaded to CAS", "certId", certID)
	}

	if ac.Status.Current != nil {
		ac.Status.History = append([]certsv1alpha1.CertificateGeneration{*ac.Status.Current}, ac.Status.History...)
	}
	ac.Status.Current = &gen
	return true, nil
}

// uploadToken 生成本次上传的幂等令牌：naming.ClientToken 的稳定前缀 + 8 位时间后缀，
// 仍是 48 个 hex 字符。前缀让同一代证书的重试复用同一 token；时间后缀让「CAS 侧被删后重传」
// 拿到全新 token，不会命中云端仍保留的旧映射。生成后立即写入 pendingUpload，
// 此后的每次重试都只读那一份，不再重新生成。
func (r *AliyunCertificateReconciler) uploadToken(ac *certsv1alpha1.AliyunCertificate, fingerprint string) string {
	base := naming.ClientToken(ac.UID, fingerprint)
	if len(base) > tokenPrefixLen {
		base = base[:tokenPrefixLen]
	}
	return base + fmt.Sprintf("%08x", uint32(r.now().Unix())) //nolint:gosec // 只取低 32 位做时间后缀
}

// refreshUploadState 用 APIReader 直读 CR，把代次与 write-ahead 记录换成 API server 上的
// 权威值。落后的 cache 会造成两种真实损害：按它判断会对同一代证书多调一次 CAS Upload
// （幂等，但白费一次云调用，且测试里数得出来）；更糟的是它看不到已落盘的 write-ahead 记录，
// 于是生成新 token 覆盖旧的，重试就不再幂等。
func (r *AliyunCertificateReconciler) refreshUploadState(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	reader := client.Reader(r.APIReader)
	if reader == nil {
		reader = r.Client
	}
	fresh := &certsv1alpha1.AliyunCertificate{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(ac), fresh); err != nil {
		return err
	}
	ac.Status.Current = fresh.Status.Current
	ac.Status.History = fresh.Status.History
	ac.Status.PendingUpload = fresh.Status.PendingUpload
	ac.Status.CASProbedAt = fresh.Status.CASProbedAt
	return nil
}

// patchPendingUpload 用一次最小补丁把 status.pendingUpload 改成 want（nil 表示删除）。
//
// 它必须独立于本轮末尾的 patchStatus：write-ahead 记录要在调云之前落盘，而末尾的
// MergeFrom 以本轮开头的快照为基准，看不到「本轮先写后删」这条轨迹——写进去的
// pendingUpload 会因为首尾都是 nil 而永远删不掉。
//
// Patch 传副本而不是 ac：client 会把响应解码回传入对象，直接用 ac 会把本轮已在内存里
// 设好、尚未持久化的 condition 覆盖成服务端旧值。
func (r *AliyunCertificateReconciler) patchPendingUpload(ctx context.Context, ac *certsv1alpha1.AliyunCertificate, want *certsv1alpha1.PendingUpload) error {
	base := ac.DeepCopy()
	ac.Status.PendingUpload = want
	obj := ac.DeepCopy()
	return r.Status().Patch(ctx, obj, client.MergeFrom(base))
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
