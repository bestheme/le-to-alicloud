package aliyun

import (
	"context"
	"time"
)

// OSS 的 OpenAPI action 名，供 OnCall 指标与 Error.Op 使用。
//
// Put 与 Delete 走同一个 OpenAPI（PutCname，只是 CertificateConfiguration 的字段不同），
// 所以指标 action 同名；Op 里区分不了两者是有意的——RAM 上它们也是同一个 oss:PutCname。
const (
	ActionListCname = "ListCname"
	ActionPutCname  = "PutCname"
)

// Cname 是 ListCname 里一条 CNAME 记录被剪裁后的只读视图。没有私钥、没有响应体。
type Cname struct {
	Domain    string
	Status    string // Enabled | Disabled
	CertRef   string // Certificate.CertId，形如 "27087165-cn-hangzhou"；无证书时 ""
	CertType  string // CAS | Upload；无证书时 ""
	AccountID string // ListCname.Owner，账号 fencing 用
}

// OSSClient 是本项目对 OSS 的全部依赖：读一条 CNAME 的证书引用、按 CAS certId 换绑、摘证书。
//
// 不创建 / 删除 CNAME，不做所有权验证（CreateCnameToken）：证书归属于已存在的域名，
// 与 FC3 侧「证书归属于域名，与函数无关」是同一条边界。
type OSSClient interface {
	// GetCname 列出 bucket 的 CNAME 并按 domain 精确匹配（大小写不敏感）。
	// 找不到该 domain 返回 Class=NotFound、Code=CnameNotFound；bucket 不存在由 SDK 的
	// NoSuchBucket / 404 自然落入 NotFound。
	GetCname(ctx context.Context, bucket, domain string) (*Cname, error)
	// PutCnameCert 以 Force=true 覆盖式地把 certRef（"<certId>-<casRegion>"）绑到 domain。
	PutCnameCert(ctx context.Context, bucket, domain, certRef string) error
	// DeleteCnameCert 摘掉 domain 上的证书，CNAME 记录保留。
	DeleteCnameCert(ctx context.Context, bucket, domain string) error
}

// OSSClientConfig 是构造真实 OSS client 的参数，与 FC3ClientConfig 同构。
//
// 没有 Endpoint 覆盖：endpoint 一律由 SDK 按 region 推出（oss-<region>.aliyuncs.com），
// 理由与 FC3 相同——AliyunCertificate 的 endpointOverride 是给 CAS 用的。
type OSSClientConfig struct {
	Region     string
	Timeout    time.Duration
	Limiters   *Limiters
	LimiterKey string
	// OnCall 语义与 FC3ClientConfig.OnCall 完全一致。
	OnCall func(action, code string, d time.Duration)
}
