// Package provider 定义「把一张证书装到一个云资源上」这件事的 provider 无关接口。
//
// spec §7 的职责边界表把工作切成两半：provider 只负责「云上现在是什么」与「把它改成
// 什么」，其余（SANs 校验、幂等短路、重试退避、condition、event、指标、凭证与 client
// 构造）一律归通用层。这个包里因此没有任何 Kubernetes 类型，也没有任何 SDK 类型。
package provider

import (
	"context"
	"strconv"
	"time"
)

// Target 是 provider 无关的目标标识。通用层用 (Type, Region, Identifier) 做 field index
// 与冲突仲裁——因此这三项必须唯一确定一个云上资源，不能有 provider 私有的补充条件。
type Target struct {
	// Type 与 CRD 的 spec.target.type 取值一致，也是注册表的键。
	Type       string
	Region     string
	Identifier string // FC3: domainName
	// Spec 指向 CRD 内嵌的目标结构体，provider 自行断言。
	Spec any
}

// CertMaterial 由通用层准备，provider 只读。
//
// CertPEM / KeyPEM 一定来自 Secret 经 pki 规范化后的输出，不来自 status——「写入内容的
// 确定性」（spec §3）：多副本短暂重叠时，最坏情况是两次写入同样的字节。
type CertMaterial struct {
	Fingerprint string // SHA-256(leaf DER)，小写 hex
	CertPEM     []byte // leaf + intermediates
	KeyPEM      []byte // PKCS#1 / SEC1
	CertID      *int64 // CAS certId；ReferencesCertByID=false 的 provider 忽略，=true 的 provider 经 CASCertRef() 使用
	CASName     string // 云侧证书名；FC3 用作 certConfig.certName
	NotAfter    time.Time
	DNSNames    []string
	// CASRegion 是证书上传所在的 CAS 区域（AliyunSpec.EffectiveCASRegion()）。只用来拼
	// CASCertRef；内联 PEM 的 provider 忽略它。
	CASRegion string
}

// CASCertRef 返回按 ID 引用证书的目标（OSS）使用的字符串 "<certId>-<casRegion>"。
// CertID 为 nil 或 CASRegion 为空时返回 ""——那两种情况都还没有一个可引用的云侧证书。
//
// 这是该字符串**唯一**的拼装点：通用层的对账、provider 的写入、测试的断言全部从这里取值，
// 免得三处各拼一套、某一处少个连字符就永远短路不了。
func (m CertMaterial) CASCertRef() string {
	if m.CertID == nil || m.CASRegion == "" {
		return ""
	}
	return strconv.FormatInt(*m.CertID, 10) + "-" + m.CASRegion
}

// ObservedState 是 Observe 的结论，也是幂等判断唯一的真相来源。
//
// 刻意没有 Exists 字段：「目标不存在」是一个**错误**（CodeTargetNotFound），不是一种
// 观测结果——Observe 返回 nil error 就已经意味着目标在。留一个恒为 true 的布尔只会让
// 下一个 provider 以为自己可以用它表达别的意思。
type ObservedState struct {
	CurrentFingerprint string // 内联 PEM 的目标：实际证书指纹；"" = 无证书。OSS 恒为 ""
	CurrentCertRef     string // 按 ID 引用的目标：实际引用的 certRef；"" = 无证书。FC3 恒为 ""
	Protocol           string
	AccountID          string // 账号 fencing 用
}

// Capabilities 描述 provider 的形状，通用层据此决定要不要强制 CAS 上传等。
type Capabilities struct {
	ReferencesCertByID     bool // true = 目标存 certId（CDN/CLB）；false = 内联 PEM（FC3）
	SupportsProtocolSwitch bool
	RequiresCASUpload      bool // true ⇒ 证书必须已上传 CAS，否则 Binding 报 CASUploadRequired，不替证书开上传（D21）
}

// ApplyOptions 是本次写入的调节项。
type ApplyOptions struct {
	EnsureHTTPSProtocol bool
	// PreviousFingerprint 供「替换需指定旧值」的未来 provider（乐观并发）；FC3 忽略。
	PreviousFingerprint string
}

// Client 是通用层构造好、交给 provider 使用的云客户端。
//
// 具体类型由 provider 自行断言（FC3 断言为 aliyun.FC3Client），与 Target.Spec 同构。
// 用 any 而不是给 Provider 加类型参数：注册表是异构的 map[string]Provider，类型参数
// 会让它无法成立。断言失败返回 CodeInvalidClient，是接线错误而非运行时错误。
type Client any

// DeletionPolicy 与 CRD 的 spec.deletionPolicy 取值一一对应。
//
// 刻意在本包里重新定义而不是引用 api/v1alpha1：pkg/provider 不该依赖 CRD 类型，
// 否则以后想把 provider 抽成独立模块就要连 API 包一起搬。通用层负责字符串转换。
type DeletionPolicy string

const (
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
	DeletionPolicyUnbind DeletionPolicy = "Unbind"
)

// Provider 是一类云目标的读写实现。
//
// 三个方法都必须是幂等的，且都必须返回经过分类的 *ProviderError（除非 error 为 nil）：
// 通用层不认识任何 SDK 错误，只认 ProviderError 的 Code / Retryable / Reason。
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Observe(ctx context.Context, t Target, c Client) (ObservedState, error)
	Apply(ctx context.Context, t Target, c Client, m CertMaterial, o ApplyOptions) error
	Cleanup(ctx context.Context, t Target, c Client, policy DeletionPolicy) error
}
