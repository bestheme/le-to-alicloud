package aliyun

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	osscred "github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	credential "github.com/aliyun/credentials-go/credentials"
)

// ossCredentials 把 credentials-go 的 Credential 适配成 OSS SDK v2 的 CredentialsProvider。
//
// 两边都是「每次调用时取一次当前凭证」的语义，AK/SK、STS、RoleARN、OIDC 因此全部沿现有
// 路径生效：轮换与刷新由 credentials-go 负责，这里只做搬运。
type ossCredentials struct{ cred credential.Credential }

func (p ossCredentials) GetCredentials(_ context.Context) (osscred.Credentials, error) {
	m, err := p.cred.GetCredential()
	if err != nil {
		return osscred.Credentials{}, err
	}
	if m == nil {
		return osscred.Credentials{}, errors.New("credentials-go 返回了空的凭证模型")
	}
	return osscred.Credentials{
		AccessKeyID:     dara.StringValue(m.AccessKeyId),
		AccessKeySecret: dara.StringValue(m.AccessKeySecret),
		SecurityToken:   dara.StringValue(m.SecurityToken),
	}, nil
}

type sdkOSS struct {
	c   *oss.Client
	cfg OSSClientConfig
}

// NewOSSClient 用 OSS SDK v2 构造 OSSClient。
//
// WithRetryMaxAttempts(1)：SDK 自带的重试会绕开 Limiters，而重试节奏本来就归
// controller-runtime。超时拆成连接 5s + 读写 cfg.Timeout，与 FC3 client 同一形状。
func NewOSSClient(cred credential.Credential, cfg OSSClientConfig) (OSSClient, error) {
	if cfg.Region == "" {
		return nil, &Error{Class: ClassPermanent, Op: "NewOSSClient", Code: "RegionRequired",
			Err: errors.New("OSS client 需要 region")}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Limiters == nil {
		cfg.Limiters = NewLimiters()
	}
	c := oss.NewClient(oss.LoadDefaultConfig().
		WithRegion(cfg.Region).
		WithCredentialsProvider(ossCredentials{cred: cred}).
		WithConnectTimeout(5 * time.Second).
		WithReadWriteTimeout(cfg.Timeout).
		WithRetryMaxAttempts(1).
		WithUserAgent("le-to-alicloud"))
	return &sdkOSS{c: c, cfg: cfg}, nil
}

func (s *sdkOSS) observe(action string, start time.Time, err error) {
	if s.cfg.OnCall == nil {
		return
	}
	s.cfg.OnCall(action, callCode(err), time.Since(start))
}

func (s *sdkOSS) GetCname(ctx context.Context, bucket, domain string) (*Cname, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitOSS); err != nil {
		return nil, Classify(ActionListCname, err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	start := time.Now()
	res, err := s.c.ListCname(cctx, &oss.ListCnameRequest{Bucket: oss.Ptr(bucket)})
	cerr := classifyOSS(ActionListCname, err)
	s.observe(ActionListCname, start, cerr)
	if cerr != nil {
		return nil, cerr
	}
	return cnameFromList(res, domain)
}

func (s *sdkOSS) PutCnameCert(ctx context.Context, bucket, domain, certRef string) error {
	if certRef == "" {
		return &Error{Class: ClassPermanent, Op: ActionPutCname, Code: "CertRefRequired",
			Err: errors.New("certRef 为空")}
	}
	// 只带 CertId + Force：不带 PreviousCertId 时 OSS 要求 Force=true（文档明说），而
	// Certificate / PrivateKey 字段在本项目里永远不填——私钥不走 OSS 这条链路。
	return s.putCname(ctx, bucket, domain, &oss.CertificateConfiguration{
		CertId: oss.Ptr(certRef),
		Force:  oss.Ptr(true),
	})
}

func (s *sdkOSS) DeleteCnameCert(ctx context.Context, bucket, domain string) error {
	return s.putCname(ctx, bucket, domain, &oss.CertificateConfiguration{
		DeleteCertificate: oss.Ptr(true),
	})
}

func (s *sdkOSS) putCname(ctx context.Context, bucket, domain string, cc *oss.CertificateConfiguration) error {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitOSS); err != nil {
		return Classify(ActionPutCname, err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	start := time.Now()
	_, err := s.c.PutCname(cctx, &oss.PutCnameRequest{
		Bucket: oss.Ptr(bucket),
		BucketCnameConfiguration: &oss.BucketCnameConfiguration{
			Cname: &oss.Cname{Domain: oss.Ptr(domain), CertificateConfiguration: cc},
		},
	})
	cerr := classifyOSS(ActionPutCname, err)
	s.observe(ActionPutCname, start, cerr)
	return cerr
}

// classifyOSS 把 OSS SDK v2 的错误折进本包的分类。
//
// *oss.ServiceError 只取 Code 与 StatusCode——Message、Snapshot（响应体）一律丢弃，
// 与 fromSDKError 对 tea/dara 错误的处理完全一致。其它错误（超时、网络）交给 Classify。
func classifyOSS(op string, err error) error {
	if err == nil {
		return nil
	}
	var se *oss.ServiceError
	if errors.As(err, &se) {
		code, status := se.Code, se.StatusCode
		return fromSDKError(op, &code, &status)
	}
	return Classify(op, err)
}

// cnameFromList 在 ListCname 的结果里找 domain。找不到就是 NotFound——对 Observe 而言
// 「bucket 在、CNAME 不在」与「bucket 不在」一样都是目标不存在。
func cnameFromList(res *oss.ListCnameResult, domain string) (*Cname, error) {
	if res == nil {
		return nil, &Error{Class: ClassRetryable, Op: ActionListCname, Code: CodeEmptyResponse,
			Err: errors.New("响应为空")}
	}
	for i := range res.Cnames {
		c := &res.Cnames[i]
		if !strings.EqualFold(oss.ToString(c.Domain), domain) {
			continue
		}
		out := &Cname{
			Domain:    oss.ToString(c.Domain),
			Status:    oss.ToString(c.Status),
			AccountID: oss.ToString(res.Owner),
		}
		if c.Certificate != nil {
			out.CertRef = oss.ToString(c.Certificate.CertId)
			out.CertType = oss.ToString(c.Certificate.Type)
		}
		return out, nil
	}
	return nil, &Error{Class: ClassNotFound, Op: ActionListCname, Code: "CnameNotFound",
		Err: errors.New("bucket 上没有该自定义域名")}
}

var _ OSSClient = (*sdkOSS)(nil)
var _ osscred.CredentialsProvider = ossCredentials{}
