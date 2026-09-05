// Package fc3 把证书装到函数计算 3.0 的自定义域名上。
//
// 证书归属于域名，与函数无关（spec §2.1）：certConfig 与 routeConfig / wafConfig /
// tlsConfig 平级，域名是主键。因此本 provider 只做一件事——把 certConfig 换成我们的
// 证书，其余字段原样回填。
//
// RAM：fc 支持资源级授权，必须用上——逐域名的
// acs:fc:{regionId}:{accountId}:custom-domains/{domainName}。样例见
// docs/ram/binding-fc3-policy.json。这与 yundun-cert:* 无法收窄形成对照（spec §8.3）。
package fc3

import (
	"context"
	"errors"
	"fmt"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// Provider 是 spec §7 的第一个实现。无状态，零值可用。
type Provider struct{}

func init() { provider.Register(&Provider{}) }

func (p *Provider) Name() string { return certsv1alpha1.TargetTypeFC3CustomDomain }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		// FC3 内联 PEM，没有 certId 这个概念（Serverless Devs 的 certId 是客户端语法糖）。
		ReferencesCertByID:     false,
		SupportsProtocolSwitch: true,
		// 因此也不强制 CAS 上传：FC3-only 的用户可以完全不授 yundun-cert:*（spec §8.3）。
		RequiresCASUpload: false,
	}
}

// clientOf 断言通用层传进来的 client。失败是接线错误，不是运行时故障。
//
// 这里的 reason 三个调用方共用一个，与 toProviderError 逐操作分流的做法不同，是有意的：
// CodeInvalidClient 只可能由「通用层构造 client 的那段代码传错了类型」导致，在能跑起来的
// 构建里到不了任何用户的 condition。给它按操作分流等于假装这个取值有诊断价值——真正有
// 价值的是错误消息里的 %T。ReasonApplyFailed 在这里只是个取值有界的占位符。
func clientOf(c provider.Client) (aliyun.FC3Client, error) {
	cl, ok := c.(aliyun.FC3Client)
	if !ok {
		return nil, provider.Errorf(provider.CodeInvalidClient, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 aliyun.FC3Client，得到 %T", c))
	}
	return cl, nil
}

func (p *Provider) Observe(ctx context.Context, t provider.Target, c provider.Client) (provider.ObservedState, error) {
	cl, err := clientOf(c)
	if err != nil {
		return provider.ObservedState{}, err
	}
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		return provider.ObservedState{},
			toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonObserveFailed)
	}
	obs := provider.ObservedState{
		Exists:    true,
		Protocol:  cd.Protocol,
		AccountID: cd.AccountID,
	}
	if cd.CertificatePEM != "" {
		fp, ferr := pki.LeafFingerprint([]byte(cd.CertificatePEM))
		if ferr != nil {
			// 云上那张证书解析不了：不是我们写的，也无从比对。当成「没有证书」处理，
			// 通用层会照常 Apply 覆盖掉它。报错反而会让 Binding 永久卡住。
			return obs, nil
		}
		obs.CurrentFingerprint = fp
	}
	return obs, nil
}

func (p *Provider) Apply(
	ctx context.Context, t provider.Target, c provider.Client,
	m provider.CertMaterial, o provider.ApplyOptions,
) error {
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	if m.CASName == "" {
		// certConfig.certName 在 FC3 侧是必填。空名字会被服务端拒掉，而错误信息通常
		// 只说「参数不合法」——在这里挡住比在云上猜要便宜得多。
		return provider.Errorf(provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed,
			errors.New("CertMaterial.CASName 为空，无法填写 certConfig.certName"))
	}

	// read-modify-write：先读回完整对象，再把除 certConfig / protocol 之外的一切原样回填。
	// 这在「全量替换」和「部分合并」两种语义下都正确（spec §6.3）。代价是 Get 与 Update
	// 之间没有乐观锁，是 last-write-wins；缓解是窗口极短、写入频率极低（正常一年 4–6 次）。
	//
	// spec §12.3 #2：未实测（FC3 UpdateCustomDomain 是全量替换还是按字段合并语义未核实），
	// 实测结论见 test/integration/RESULTS.md
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		return toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}

	in := &aliyun.UpdateCustomDomainInput{
		// Protocol 永远显式给出：缺席在「合并」与「全量替换」两种语义下含义不同，
		// 而云侧现值刚从上面这次 Get 拿到，原样带回来在两种语义下都对。
		Protocol: cd.Protocol,
		// Echo 是不透明回填体：只读、只带回，绝不解释也绝不写穿它。
		Echo: cd.Echo,
		CertConfig: &aliyun.CertConfig{
			CertName: m.CASName,
			CertPEM:  m.CertPEM,
			KeyPEM:   m.KeyPEM,
		},
	}
	// 只在「用户要求」且「当前确实没开 HTTPS」时才动 protocol。已含 HTTPS 时保持原样：
	// 把 "HTTPS" 改成 "HTTP,HTTPS" 等于替用户打开了明文入口（spec §13「不越权改线上配置」）。
	if o.EnsureHTTPSProtocol && !protocolHasHTTPS(cd.Protocol) {
		in.Protocol = protocolBoth
	}
	if err := cl.UpdateCustomDomain(ctx, t.Identifier, in); err != nil {
		return toProviderError(aliyun.ActionUpdateCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}
	return nil
}

// Cleanup 在 deletionPolicy=Unbind 时清空 certConfig。
//
// 「只解绑自己的证书」这条判断不在这里：指纹比对需要 Binding 的 appliedFingerprint，
// 而 spec §7 的职责边界表把「幂等判断」留给 Observe、把状态比对留给通用层。通用层会
// 先 Observe、比对完再决定要不要调用 Cleanup（spec §6.5）。
func (p *Provider) Cleanup(
	ctx context.Context, t provider.Target, c provider.Client, policy provider.DeletionPolicy,
) error {
	if policy != provider.DeletionPolicyUnbind {
		// Orphan：云侧一动不动，连一次 Get 都不发。
		return nil
	}
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		return swallowNotFound(
			toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonCleanupFailed))
	}
	in := &aliyun.UpdateCustomDomainInput{Protocol: cd.Protocol, Echo: cd.Echo, ClearCert: true}
	// 纯 HTTPS 的域名被拿掉证书后会彻底无法访问，必须同时降到 HTTP。
	if isHTTPSOnly(cd.Protocol) {
		in.Protocol = protocolHTTP
	}
	if err := cl.UpdateCustomDomain(ctx, t.Identifier, in); err != nil {
		// 域名可能在 Get 与 Update 之间被删掉；对解绑而言那同样是成功。
		return swallowNotFound(
			toProviderError(aliyun.ActionUpdateCustomDomain, err, certsv1alpha1.ReasonCleanupFailed))
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)
