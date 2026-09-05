package aliyun

import (
	"context"
	"errors"
	"fmt"
	"time"

	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
	credential "github.com/aliyun/credentials-go/credentials"
)

// FC3 的 OpenAPI 动作名。作为指标 label 使用，取值必须是常量。
const (
	ActionGetCustomDomain    = "GetCustomDomain"
	ActionUpdateCustomDomain = "UpdateCustomDomain"
)

// FC3ClientConfig 是构造真实 FC3 client 的参数，与 CASClientConfig 同构。
//
// 没有 ResourceGroupID：自定义域名不按资源组授权，FC 的 RAM 是逐域名 ARN（spec §8.3）。
type FC3ClientConfig struct {
	Region     string
	Endpoint   string // 非空时覆盖 SDK 的 region → endpoint 映射
	Timeout    time.Duration
	Limiters   *Limiters
	LimiterKey string
	// OnCall 在每次 OpenAPI 调用返回后被调用一次；可选，nil 表示不观测。
	// 语义与 CASClientConfig.OnCall 完全一致：code 由 callCode 折叠成有界取值，
	// d 是该次调用的耗时（不含限流等待）。
	OnCall func(action, code string, d time.Duration)
}

type sdkFC3 struct {
	c   *fc.Client
	cfg FC3ClientConfig
}

// NewFC3Client 用 SDK v4 构造 FC3Client。SDK 的 EndpointRule 是 regional，
// 会按 RegionId 自动选出 fcv3.<region>.aliyuncs.com。
func NewFC3Client(cred credential.Credential, cfg FC3ClientConfig) (FC3Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Limiters == nil {
		cfg.Limiters = NewLimiters()
	}
	ms := int(cfg.Timeout / time.Millisecond)
	connect := 5000
	oc := &openapiutil.Config{
		Credential:     cred,
		RegionId:       dara.String(cfg.Region),
		ReadTimeout:    &ms,
		ConnectTimeout: &connect,
		UserAgent:      dara.String("le-to-alicloud"),
	}
	if cfg.Endpoint != "" {
		oc.Endpoint = dara.String(cfg.Endpoint)
	}
	c, err := fc.NewClient(oc)
	if err != nil {
		return nil, Classify("NewFC3Client", err)
	}
	return &sdkFC3{c: c, cfg: cfg}, nil
}

func (s *sdkFC3) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

// observe 上报一次 OpenAPI 调用。err 必须是已经过 Classify 的错误。
func (s *sdkFC3) observe(action string, start time.Time, err error) {
	if s.cfg.OnCall == nil {
		return
	}
	s.cfg.OnCall(action, callCode(err), time.Since(start))
}

func (s *sdkFC3) GetCustomDomain(ctx context.Context, domain string) (*CustomDomain, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitFC3); err != nil {
		return nil, Classify(ActionGetCustomDomain, err)
	}
	cctx, cancel := s.withTimeout(ctx)
	defer cancel()
	start := time.Now()
	resp, err := s.c.GetCustomDomainWithContext(cctx, dara.String(domain), nil, &dara.RuntimeOptions{})
	cerr := Classify(ActionGetCustomDomain, err)
	s.observe(ActionGetCustomDomain, start, cerr)
	if cerr != nil {
		return nil, cerr
	}
	if resp == nil || resp.Body == nil {
		return nil, &Error{
			Class: ClassRetryable, Op: ActionGetCustomDomain, Code: "EmptyResponse",
			Err: errors.New("响应缺少 body"),
		}
	}
	return customDomainFromSDK(resp.Body), nil
}

func (s *sdkFC3) UpdateCustomDomain(ctx context.Context, domain string, in *UpdateCustomDomainInput) error {
	if in == nil {
		return &Error{Class: ClassPermanent, Op: ActionUpdateCustomDomain, Code: "NilInput", Err: errors.New("写入体为空")}
	}
	body, err := updateInputToSDK(in)
	if err != nil {
		return err
	}
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitFC3); err != nil {
		return Classify(ActionUpdateCustomDomain, err)
	}
	cctx, cancel := s.withTimeout(ctx)
	defer cancel()
	start := time.Now()
	_, uerr := s.c.UpdateCustomDomainWithContext(cctx, dara.String(domain),
		&fc.UpdateCustomDomainRequest{Body: body}, nil, &dara.RuntimeOptions{})
	cerr := Classify(ActionUpdateCustomDomain, uerr)
	s.observe(ActionUpdateCustomDomain, start, cerr)
	return cerr
}

// customDomainFromSDK 把 SDK 响应裁剪成本项目的视图。
//
// 明文私钥就在 d.CertConfig.PrivateKey 里。这个函数是它在本进程中唯一被读到的位置，
// 而这里读它只是为了**不带走**：既不复制进 CustomDomain，也不放进 Echo。越过这一层
// 之后，上层代码想泄漏都拿不到。
func customDomainFromSDK(d *fc.CustomDomain) *CustomDomain {
	out := &CustomDomain{
		AccountID:  dara.StringValue(d.AccountId),
		DomainName: dara.StringValue(d.DomainName),
		Protocol:   dara.StringValue(d.Protocol),
	}
	if d.CertConfig != nil {
		out.CertName = dara.StringValue(d.CertConfig.CertName)
		out.CertificatePEM = dara.StringValue(d.CertConfig.Certificate)
	}
	// 回填体刻意不含 CertConfig：证书由调用方重新给定，私钥永不经过这里。
	out.Echo = DomainEcho{Payload: &fc.UpdateCustomDomainInput{
		AuthConfig:  d.AuthConfig,
		CorsConfig:  d.CorsConfig,
		RouteConfig: d.RouteConfig,
		TlsConfig:   d.TlsConfig,
		WafConfig:   d.WafConfig,
	}}
	return out
}

// updateInputToSDK 在 Echo 携带的回填体之上覆盖 protocol 与 certConfig。
//
// 必须先复制再覆盖：Echo 的 payload 是 Get 那一刻构造的对象，同一个 CustomDomain
// 可能被 Observe 与 Apply 先后使用，就地修改会让第二次读到第一次的残留。
func updateInputToSDK(in *UpdateCustomDomainInput) (*fc.UpdateCustomDomainInput, error) {
	base := &fc.UpdateCustomDomainInput{}
	if in.Echo.Payload != nil {
		p, ok := in.Echo.Payload.(*fc.UpdateCustomDomainInput)
		if !ok {
			return nil, &Error{
				Class: ClassPermanent, Op: ActionUpdateCustomDomain, Code: "InvalidEcho",
				Err: fmt.Errorf("回填体类型不匹配: %T", in.Echo.Payload),
			}
		}
		cp := *p
		base = &cp
	}
	if in.Protocol != "" {
		base.Protocol = dara.String(in.Protocol)
	}
	switch {
	case in.ClearCert:
		// 显式三个空串，而不是省略 certConfig：UpdateCustomDomain 是全量替换还是部分
		// 合并，文档没说（spec §2.1，§12.3 #2 列为必测）。省略在「合并」语义下等于
		// 「保持原样」，解绑就白做了；显式空串在两种语义下都表达「这里没有证书」。
		base.CertConfig = &fc.CertConfig{
			CertName: dara.String(""), Certificate: dara.String(""), PrivateKey: dara.String(""),
		}
	case in.CertConfig != nil:
		base.CertConfig = &fc.CertConfig{
			CertName:    dara.String(in.CertConfig.CertName),
			Certificate: dara.String(string(in.CertConfig.CertPEM)),
			PrivateKey:  dara.String(string(in.CertConfig.KeyPEM)),
		}
	}
	return base, nil
}

var _ FC3Client = (*sdkFC3)(nil)
