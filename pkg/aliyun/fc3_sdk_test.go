package aliyun

import (
	"fmt"
	"strings"
	"testing"

	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
)

func TestCustomDomainFromSDK_DropsPrivateKey(t *testing.T) {
	auth := &fc.AuthConfig{}
	route := &fc.RouteConfig{}
	waf := &fc.WAFConfig{}
	body := &fc.CustomDomain{
		AccountId:  dara.String("1234567890"),
		DomainName: dara.String("api.example.com"),
		Protocol:   dara.String("HTTP,HTTPS"),
		CertConfig: &fc.CertConfig{
			CertName:    dara.String("cert_abc"),
			Certificate: dara.String("PUBLIC-CERT"),
			PrivateKey:  dara.String("SUPERSECRETKEYMATERIAL"),
		},
		AuthConfig:  auth,
		RouteConfig: route,
		WafConfig:   waf,
	}

	cd := customDomainFromSDK(body)

	if cd.AccountID != "1234567890" || cd.DomainName != "api.example.com" || cd.Protocol != "HTTP,HTTPS" {
		t.Fatalf("基础字段丢失: %+v", cd)
	}
	if cd.CertName != "cert_abc" || cd.CertificatePEM != "PUBLIC-CERT" {
		t.Fatalf("证书公开部分丢失: %+v", cd)
	}
	// 私钥必须在这一层就消失：整个结构体（含 Echo 的回填体）里都不该有它。
	echo, ok := cd.Echo.Payload.(*fc.UpdateCustomDomainInput)
	if !ok {
		t.Fatalf("Echo 应携带 SDK 写入体，得到 %T", cd.Echo.Payload)
	}
	if echo.CertConfig != nil {
		t.Fatal("回填体里绝不能带 certConfig —— 那是私钥唯一可能残留的地方")
	}
	if echo.AuthConfig != auth || echo.RouteConfig != route || echo.WafConfig != waf {
		t.Error("回填体必须原样保留 authConfig / routeConfig / wafConfig")
	}
	if strings.Contains(cd.CertificatePEM, "SUPERSECRET") {
		t.Fatal("私钥泄漏")
	}
}

func TestCustomDomainFromSDK_NoCertConfig(t *testing.T) {
	cd := customDomainFromSDK(&fc.CustomDomain{DomainName: dara.String("d"), Protocol: dara.String("HTTP")})
	if cd.CertName != "" || cd.CertificatePEM != "" {
		t.Errorf("无证书的域名应给出空值: %+v", cd)
	}
}

func TestUpdateInputToSDK_SetsCertOverEcho(t *testing.T) {
	route := &fc.RouteConfig{}
	echo := &fc.UpdateCustomDomainInput{RouteConfig: route, Protocol: dara.String("HTTP")}
	in := &UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &CertConfig{CertName: "n", CertPEM: []byte("C"), KeyPEM: []byte("K")},
		Echo:       DomainEcho{Payload: echo},
	}

	out, err := updateInputToSDK(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.RouteConfig != route {
		t.Error("回填体必须原样带上")
	}
	if dara.StringValue(out.Protocol) != "HTTP,HTTPS" {
		t.Errorf("protocol 应被覆盖: %v", dara.StringValue(out.Protocol))
	}
	if dara.StringValue(out.CertConfig.PrivateKey) != "K" {
		t.Errorf("私钥必须写进请求体: %s", certConfigDigest(out.CertConfig))
	}
	// 覆盖必须作用在副本上，否则同一个 Echo 被两次 Apply 复用时会互相污染。
	if dara.StringValue(echo.Protocol) != "HTTP" || echo.CertConfig != nil {
		t.Error("不得就地修改 Echo 携带的原始写入体")
	}
}

func TestUpdateInputToSDK_ClearCertUsesExplicitEmptyStrings(t *testing.T) {
	out, err := updateInputToSDK(&UpdateCustomDomainInput{Protocol: "HTTP", ClearCert: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.CertConfig == nil {
		t.Fatal("解绑必须显式提交 certConfig，省略它在合并语义下等于不动")
	}
	// 折成三行只为满足 lll 的 120 列上限，断言的三个字段与 brief 完全一致。
	for name, v := range map[string]*string{
		"certName":    out.CertConfig.CertName,
		"certificate": out.CertConfig.Certificate,
		"privateKey":  out.CertConfig.PrivateKey,
	} {
		if v == nil || *v != "" {
			t.Errorf("%s 应为显式空串，得到 %v", name, v)
		}
	}
}

func TestUpdateInputToSDK_RejectsForeignEcho(t *testing.T) {
	_, err := updateInputToSDK(&UpdateCustomDomainInput{Echo: DomainEcho{Payload: "not-an-sdk-body"}})
	if err == nil {
		t.Fatal("回填体类型不匹配必须报错，而不是静默丢掉整张路由表")
	}
	if ClassOf(err) != ClassPermanent {
		t.Errorf("接线错误应是 Permanent，得到 %v", ClassOf(err))
	}
}

// certConfigDigest 是本包测试里**唯一**允许用来描述 SDK certConfig 的形式。
//
// 绝不把 *fc.CertConfig 交给 %v / %+v：它的 String() 会把 privateKey 原样打出来，而
// 「零凭证泄漏」同样管着测试输出——CI 日志是公开的，失败信息会被贴进 issue。
// 只报无害的 certName 与私钥长度，足够诊断「私钥有没有写进请求体」。
func certConfigDigest(c *fc.CertConfig) string {
	if c == nil {
		return "<nil certConfig>"
	}
	return fmt.Sprintf("certName=%q, len(certificate)=%d, len(privateKey)=%d",
		dara.StringValue(c.CertName),
		len(dara.StringValue(c.Certificate)),
		len(dara.StringValue(c.PrivateKey)))
}

func TestCertConfigDigest_NeverRendersThePrivateKey(t *testing.T) {
	const key = "SUPERSECRETKEYMATERIAL"
	c := &fc.CertConfig{
		CertName:    dara.String("cert_abc"),
		Certificate: dara.String("PUBLIC-CERT"),
		PrivateKey:  dara.String(key),
	}
	if got := certConfigDigest(c); strings.Contains(got, key) {
		t.Fatalf("测试输出泄漏了私钥: %s", got)
	}
	// 反向钉住这条规矩存在的理由：SDK 类型的默认格式化**确实**会把私钥打出来，
	// 所以 %+v 在本包的测试里是禁用的。哪天 SDK 自己开始脱敏，这一行会失败，
	// 那时该做的是更新上面那段说明，而不是把 %+v 放回去。
	if !strings.Contains(fmt.Sprintf("%+v", c), key) {
		t.Error("SDK 的 certConfig 不再回显私钥了——请复核 certConfigDigest 的注释")
	}
	if certConfigDigest(nil) == "" {
		t.Error("nil 也要有可读的描述，否则失败信息会是一片空白")
	}
}
