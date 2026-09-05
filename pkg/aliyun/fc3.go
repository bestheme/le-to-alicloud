package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
)

// redactedKey 是私钥在任何格式化输出里的占位符。
const redactedKey = "<redacted>"

// echoOpaque 是 DomainEcho 载荷在任何格式化输出里的占位符。
const echoOpaque = "<opaque>"

// CertConfig 是要写入 FC3 自定义域名的证书。
//
// KeyPEM 是明文私钥。它必须能被送进 HTTP 请求体，又绝不能进日志 / 事件 / status；
// 下面三个方法就是这条约束的护栏，与 pki.Bundle 用的是同一套手法（值接收者，让值与
// 指针两种形态都被覆盖）。
//
// **注意**：MarshalJSON 是一个脱敏投影，不是线上格式——它输出的是 `key=<redacted>`。
// 绝不能用 json.Marshal(certConfig) 去拼 FC3 的请求体，那样发上云的会是占位符而不是私钥。
// 构造写入体时必须从 CertPEM / KeyPEM 逐字段取值填进 SDK 的结构体。
type CertConfig struct {
	CertName string
	CertPEM  []byte
	KeyPEM   []byte
}

func (c CertConfig) redacted() any {
	return struct {
		CertName  string `json:"certName"`
		CertBytes int    `json:"certBytes"`
		Key       string `json:"key"`
	}{c.CertName, len(c.CertPEM), redactedKey}
}

func (c CertConfig) String() string {
	return fmt.Sprintf("aliyun.CertConfig{certName=%s, certBytes=%d, key=%s}",
		c.CertName, len(c.CertPEM), redactedKey)
}

func (c CertConfig) GoString() string { return c.String() }

func (c CertConfig) MarshalJSON() ([]byte, error) { return json.Marshal(c.redacted()) }

// DomainEcho 携带 read-modify-write 必须原样回填、而本项目从不解释的字段
// （authConfig / corsConfig / routeConfig / tlsConfig / wafConfig）。
//
// Payload 只由构造它的 client 解释：真实 client 放 SDK 的写入体，fake 放一个标记值。
// 其它任何代码都不许读它——这既让 fake 能断言「回填没丢」，也保证证书配置永远不会
// 混进回填体（私钥因此不可能从这条路径逃逸）。
// 既然没有任何代码许可读 Payload，它在格式化输出里也就没有任何该露面的理由。下面三个
// 方法把它整体折叠成一个占位符——这不是提醒，是编译期就生效的护栏：CustomDomain 自己
// 没有方法，%v / %#v / json.Marshal 都会走反射钻进 Payload；而 fmt 在 depth>0 处会调用
// 嵌套字段的 Stringer / GoStringer，encoding/json 也会认嵌套的 Marshaler，所以这一处
// 同时堵死了 CustomDomain 与 UpdateCustomDomainInput 两条路。
type DomainEcho struct {
	Payload any
}

func (e DomainEcho) String() string { return "aliyun.DomainEcho{" + echoOpaque + "}" }

func (e DomainEcho) GoString() string { return e.String() }

func (e DomainEcho) MarshalJSON() ([]byte, error) { return json.Marshal(echoOpaque) }

// CustomDomain 是 GetCustomDomain 响应的裁剪。**私钥已在构造时丢弃**，本结构体不含它。
type CustomDomain struct {
	AccountID  string
	DomainName string
	Protocol   string
	CertName   string
	// CertificatePEM 是云侧当前证书的公开部分，用来算指纹；没有证书时为空。
	CertificatePEM string
	Echo           DomainEcho
}

// UpdateCustomDomainInput 是 read-modify-write 的写入体。
//
// 调用方从 CustomDomain 拿到 Echo 原样带上，只决定 Protocol 与证书这两件事。
type UpdateCustomDomainInput struct {
	Protocol string
	// CertConfig 非 nil 时写入这套证书。
	CertConfig *CertConfig
	// ClearCert 为 true 时把云侧 certConfig 显式清空（解绑）。优先于 CertConfig。
	ClearCert bool
	Echo      DomainEcho
}

func (in UpdateCustomDomainInput) redacted() any {
	var cc any
	if in.CertConfig != nil {
		cc = in.CertConfig.redacted()
	}
	return struct {
		Protocol   string `json:"protocol"`
		CertConfig any    `json:"certConfig,omitempty"`
		ClearCert  bool   `json:"clearCert"`
	}{in.Protocol, cc, in.ClearCert}
}

func (in UpdateCustomDomainInput) String() string {
	cert := "<none>"
	if in.CertConfig != nil {
		cert = in.CertConfig.String()
	}
	return fmt.Sprintf("aliyun.UpdateCustomDomainInput{protocol=%s, clearCert=%t, certConfig=%s}",
		in.Protocol, in.ClearCert, cert)
}

func (in UpdateCustomDomainInput) GoString() string { return in.String() }

func (in UpdateCustomDomainInput) MarshalJSON() ([]byte, error) { return json.Marshal(in.redacted()) }

// FC3Client 是本项目对函数计算 3.0 的全部依赖。
//
// 只有两个方法：证书归属于域名，与函数无关（spec §2.1），所以创建 / 删除域名、路由表、
// WAF 这些都不在 operator 的职责里。
type FC3Client interface {
	// GetCustomDomain 读取域名的完整配置。响应中的明文私钥在实现层被丢弃。
	GetCustomDomain(ctx context.Context, domain string) (*CustomDomain, error)
	// UpdateCustomDomain 提交 read-modify-write 的结果。
	UpdateCustomDomain(ctx context.Context, domain string, in *UpdateCustomDomainInput) error
}
