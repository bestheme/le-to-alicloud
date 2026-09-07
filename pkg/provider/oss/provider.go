package oss

import (
	"context"
	"errors"
	"fmt"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// Provider 是第二个 provider：OSS bucket 自定义域名（CNAME）上的证书。无状态，零值可用。
//
// 与 FC3 的本质差别是**目标上存的是 CAS certId 而不是 PEM**：Observe 回报 CurrentCertRef、
// Apply 只传 certRef、通用层按 certRef 对账（Capabilities.ReferencesCertByID）。私钥因此
// 完全不经过 OSS 这条链路。
type Provider struct{}

func init() { provider.Register(&Provider{}) }

func (p *Provider) Name() string { return certsv1alpha1.TargetTypeOSSCustomDomain }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReferencesCertByID: true,
		// OSS CNAME 没有协议开关。
		SupportsProtocolSwitch: false,
		// 引用 certId 就必须先有 certId：通用层据此在证书 uploadToCAS=false 时报
		// CASUploadRequired，在 certId 尚未就位时等待（spec 2026-09-07 §5.2）。
		RequiresCASUpload: true,
	}
}

// clientOf 断言通用层传进来的 client。失败是接线错误，与 fc3.clientOf 同一套说法。
func clientOf(c provider.Client) (aliyun.OSSClient, error) {
	cl, ok := c.(aliyun.OSSClient)
	if !ok {
		return nil, provider.Errorf(provider.CodeInvalidClient, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 aliyun.OSSClient，得到 %T", c))
	}
	return cl, nil
}

// specOf 断言 Target.Spec。bucket 只在这里，Target.Identifier 只带 domainName。
func specOf(t provider.Target) (*certsv1alpha1.OSSCustomDomainTarget, error) {
	s, ok := t.Spec.(*certsv1alpha1.OSSCustomDomainTarget)
	if !ok || s == nil {
		return nil, provider.Errorf(provider.CodeInvalidTarget, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 *OSSCustomDomainTarget，得到 %T", t.Spec))
	}
	return s, nil
}

func (p *Provider) Observe(ctx context.Context, t provider.Target, c provider.Client) (provider.ObservedState, error) {
	cl, err := clientOf(c)
	if err != nil {
		return provider.ObservedState{}, err
	}
	s, err := specOf(t)
	if err != nil {
		return provider.ObservedState{}, err
	}
	cn, err := cl.GetCname(ctx, s.Bucket, s.DomainName)
	if err != nil {
		return provider.ObservedState{},
			toProviderError(aliyun.ActionListCname, err, certsv1alpha1.ReasonObserveFailed)
	}
	// CurrentFingerprint 刻意留空：OSS 只回报 certId，没有 PEM 可算指纹。通用层按
	// ReferencesCertByID 选用 CurrentCertRef 对账。
	return provider.ObservedState{CurrentCertRef: cn.CertRef, AccountID: cn.AccountID}, nil
}

func (p *Provider) Apply(
	ctx context.Context, t provider.Target, c provider.Client,
	m provider.CertMaterial, _ provider.ApplyOptions,
) error {
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	s, err := specOf(t)
	if err != nil {
		return err
	}
	ref := m.CASCertRef()
	if ref == "" {
		// 通用层的步骤 3c 已经挡住了 certId 未就位的情形；这里是防御，与 fc3 对空 CASName
		// 的处理同一形状。空字符串发上云只会换回一个说不清的参数错误。
		return provider.Errorf(provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed,
			errors.New("CertMaterial 没有可引用的 CAS certId（CertID 为 nil 或 CASRegion 为空）"))
	}
	// 不做 read-modify-write：PutCname 的 CertificateConfiguration 只碰证书配置，CNAME 的其它
	// 属性不受影响（OSS spec O1）。Force=true 是不带 PreviousCertId 时的必填（O3），
	// 与 FC3 一样接受 last-write-wins。ApplyOptions 整个忽略：没有协议开关，
	// PreviousFingerprint 对按 certId 引用的目标没有意义。
	if err := cl.PutCnameCert(ctx, s.Bucket, s.DomainName, ref); err != nil {
		return toProviderError(aliyun.ActionPutCname, err, certsv1alpha1.ReasonApplyFailed)
	}
	return nil
}

// Cleanup 在 deletionPolicy=Unbind 时摘掉证书，CNAME 记录保留（O4）。
//
// 「只解绑自己的」判断在通用层按 appliedCertRef 比对（spec §7 职责边界表），这里不做。
func (p *Provider) Cleanup(
	ctx context.Context, t provider.Target, c provider.Client, policy provider.DeletionPolicy,
) error {
	if policy != provider.DeletionPolicyUnbind {
		return nil // Orphan：云侧一动不动，连一次 List 都不发。
	}
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	s, err := specOf(t)
	if err != nil {
		return err
	}
	if err := cl.DeleteCnameCert(ctx, s.Bucket, s.DomainName); err != nil {
		// bucket 或 CNAME 已经不在了：解绑的目的已经达到。
		return swallowNotFound(toProviderError(aliyun.ActionPutCname, err, certsv1alpha1.ReasonCleanupFailed))
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)
