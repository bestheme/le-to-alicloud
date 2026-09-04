package aliyun

import (
	"context"
	"errors"
	"fmt"
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

const (
	// casListPageSize 是 ListUserCertificateOrder 的每页条数（SDK 默认值也是 50）。
	casListPageSize int64 = 50
	// casListMaxPages 是翻页安全上限：服务端异常回显整页时不至于空转。
	casListMaxPages int64 = 200
)

// shouldFetchNextPage 判断收到 got 条结果的第 page 页之后是否还要继续翻页。
//
// 刻意不依赖响应里的 TotalCount：它可能缺失或为 0，一旦用 page*size >= total 作终止
// 条件，就会在第一页后静默截断结果。用「本页不足一整页即为末页」判断对末页不满、
// TotalCount 缺失、空页三种情况都正确，也不受服务端压低每页上限的影响。
func shouldFetchNextPage(got, size int, page int64) bool {
	if got <= 0 || size <= 0 || got < size {
		return false
	}
	return page < casListMaxPages
}

func (s *sdkCAS) FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error) {
	var out []CertSummary
	var page int64 = 1
	const size = casListPageSize
	for {
		if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASList); err != nil {
			return nil, Classify("ListUserCertificateOrder", err)
		}
		cctx, cancel := s.withTimeout(ctx)
		p, sz := page, size
		// Status 留空：SDK 的取值是单选枚举（ISSUED / WILLEXPIRED / EXPIRED / ...），
		// 没有表示「全部」的取值，设任何一个都会漏掉其余状态。留空时 UPLOAD 返回
		// 已签发与即将过期的证书；已过期证书由 controller 侧的探测逻辑另行处理。
		req := &cas.ListUserCertificateOrderRequest{
			OrderType:   dara.String("UPLOAD"),
			Keyword:     dara.String(domainHint),
			CurrentPage: &p,
			ShowSize:    &sz,
		}
		// 与 Upload 对称：配了资源组就把列举也限定在同一个资源组内，
		// 否则会看见受管资源组之外的同名证书。
		if s.cfg.ResourceGroupID != "" {
			req.ResourceGroupId = dara.String(s.cfg.ResourceGroupID)
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
		got := len(resp.Body.CertificateOrderList)
		if !shouldFetchNextPage(got, int(size), page) {
			// 只有「还满页但撞上安全上限」才是异常；其余都是正常读完。
			if got >= int(size) && page >= casListMaxPages {
				return nil, &Error{
					Class: ClassPermanent,
					Op:    "ListUserCertificateOrder",
					Code:  "TooManyPages",
					Err:   fmt.Errorf("翻页超过 %d 页仍未结束", casListMaxPages),
				}
			}
			break
		}
		page++
	}
	return out, nil
}

var _ CASClient = (*sdkCAS)(nil)
