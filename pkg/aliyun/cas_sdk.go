package aliyun

import (
	"context"
	"errors"
	"strings"
	"time"

	cas "github.com/alibabacloud-go/cas-20200407/v4/client"
	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/tea/dara"
	credential "github.com/aliyun/credentials-go/credentials"
)

// CASClientConfig 是构造真实 client 的参数。
type CASClientConfig struct {
	Region          string
	Endpoint        string // 非空时覆盖 SDK 的 region → endpoint 映射
	ResourceGroupID string
	Timeout         time.Duration
	Limiters        *Limiters
	LimiterKey      string
}

type sdkCAS struct {
	c   *cas.Client
	cfg CASClientConfig
}

// NewCASClient 用 SDK v4 构造 CASClient。所有调用走 WithContext 方法并受限流器约束。
func NewCASClient(cred credential.Credential, cfg CASClientConfig) (CASClient, error) {
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
	c, err := cas.NewClient(oc)
	if err != nil {
		return nil, Classify("NewClient", err)
	}
	return &sdkCAS{c: c, cfg: cfg}, nil
}

func (s *sdkCAS) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

func (s *sdkCAS) Upload(ctx context.Context, name string, certPEM, keyPEM []byte, clientToken string) (int64, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASWrite); err != nil {
		return 0, Classify("Upload", err)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	req := &cas.UploadUserCertificateRequest{
		Name:        dara.String(name),
		Cert:        dara.String(string(certPEM)),
		Key:         dara.String(string(keyPEM)),
		ClientToken: dara.String(clientToken),
	}
	if s.cfg.ResourceGroupID != "" {
		req.ResourceGroupId = dara.String(s.cfg.ResourceGroupID)
	}
	resp, err := s.c.UploadUserCertificateWithContext(ctx, req, &dara.RuntimeOptions{})
	if err != nil {
		return 0, Classify("Upload", err)
	}
	if resp == nil || resp.Body == nil || resp.Body.CertId == nil {
		return 0, &Error{Class: ClassRetryable, Op: "Upload", Code: "EmptyResponse", Err: errors.New("响应缺少 CertId")}
	}
	return *resp.Body.CertId, nil
}

func (s *sdkCAS) Delete(ctx context.Context, certID int64, clientToken string) error {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASWrite); err != nil {
		return Classify("Delete", err)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	id := certID
	req := &cas.DeleteUserCertificateRequest{CertId: &id}
	if clientToken != "" {
		req.ClientToken = dara.String(clientToken)
	}
	_, err := s.c.DeleteUserCertificateWithContext(ctx, req, &dara.RuntimeOptions{})
	return Classify("Delete", err)
}

func (s *sdkCAS) FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error) {
	var out []CertSummary
	var page int64 = 1
	const size int64 = 50
	for {
		if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASList); err != nil {
			return nil, Classify("ListUserCertificateOrder", err)
		}
		cctx, cancel := s.withTimeout(ctx)
		p, sz := page, size
		req := &cas.ListUserCertificateOrderRequest{
			OrderType:   dara.String("UPLOAD"),
			Keyword:     dara.String(domainHint),
			CurrentPage: &p,
			ShowSize:    &sz,
		}
		resp, err := s.c.ListUserCertificateOrderWithContext(cctx, req, &dara.RuntimeOptions{})
		cancel()
		if err != nil {
			return nil, Classify("ListUserCertificateOrder", err)
		}
		if resp == nil || resp.Body == nil {
			break
		}
		for _, it := range resp.Body.CertificateOrderList {
			if it == nil || it.CertificateId == nil {
				continue
			}
			cs := CertSummary{CertID: *it.CertificateId}
			if it.Name != nil {
				cs.Name = *it.Name
			}
			if it.Sans != nil {
				cs.SANs = strings.Split(*it.Sans, ",")
			}
			if it.EndDate != nil {
				cs.EndDate = *it.EndDate
			}
			out = append(out, cs)
		}
		total := int64(0)
		if resp.Body.TotalCount != nil {
			total = *resp.Body.TotalCount
		}
		if page*size >= total || len(resp.Body.CertificateOrderList) == 0 {
			break
		}
		page++
	}
	return out, nil
}

var _ CASClient = (*sdkCAS)(nil)
