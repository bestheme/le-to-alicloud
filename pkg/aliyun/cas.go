// Package aliyun 在阿里云 SDK 之上提供窄接口，便于 fake 与错误分类。
package aliyun

import "context"

// CertSummary 是 ListUserCertificateOrder 返回项的裁剪。
type CertSummary struct {
	CertID  int64
	Name    string
	SANs    []string
	EndDate string // YYYY-MM-DD，仅展示，不用于判断过期
}

// CASClient 是本项目对数字证书管理服务的全部依赖。
type CASClient interface {
	// Upload 上传 PEM 证书与私钥，返回 CertId。clientToken 用于幂等。
	Upload(ctx context.Context, name string, certPEM, keyPEM []byte, clientToken string) (int64, error)
	// Delete 删除证书。不存在时返回 ClassNotFound 错误。
	Delete(ctx context.Context, certID int64, clientToken string) error
	// FindUploaded 以域名为 Keyword 列出已上传证书（CAS 不支持按 Name 查）。
	FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error)
}
