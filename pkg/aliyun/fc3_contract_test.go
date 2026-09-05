package aliyun

import (
	"context"
	"testing"

	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
)

// TestFC3SDKContract 是一份编译期契约。
//
// 它不发起任何调用，也不断言任何行为——它的全部价值在于「引用了每一个我们依赖的字段名
// 与方法签名」。SDK 是代码生成的，字段改名对我们是静默的：少填一个 routeConfig 只会在
// 生产环境把别人的路由表清空。把名字写进一个必须编译通过的文件里，升级 SDK 时编译器
// 就会先替我们发现问题。
func TestFC3SDKContract(t *testing.T) {
	cc := &fc.CertConfig{
		CertName:    dara.String("n"),
		Certificate: dara.String("cert"),
		PrivateKey:  dara.String("key"),
	}
	in := &fc.UpdateCustomDomainInput{
		AuthConfig:  &fc.AuthConfig{},
		CertConfig:  cc,
		CorsConfig:  &fc.CORSConfig{},
		Protocol:    dara.String("HTTP,HTTPS"),
		RouteConfig: &fc.RouteConfig{},
		TlsConfig:   &fc.TLSConfig{},
		WafConfig:   &fc.WAFConfig{},
	}
	req := &fc.UpdateCustomDomainRequest{Body: in}
	if req.Body.CertConfig.CertName == nil {
		t.Fatal("UpdateCustomDomainRequest.Body.CertConfig.CertName 不可达")
	}

	body := &fc.CustomDomain{
		AccountId:   dara.String("1234567890"),
		DomainName:  dara.String("api.example.com"),
		Protocol:    dara.String("HTTPS"),
		CertConfig:  cc,
		AuthConfig:  in.AuthConfig,
		CorsConfig:  in.CorsConfig,
		RouteConfig: in.RouteConfig,
		TlsConfig:   in.TlsConfig,
		WafConfig:   in.WafConfig,
	}
	if dara.StringValue((&fc.GetCustomDomainResponse{Body: body}).Body.AccountId) != "1234567890" {
		t.Fatal("GetCustomDomainResponse.Body.AccountId 不可达")
	}
	if (&fc.UpdateCustomDomainResponse{Body: body}).Body == nil {
		t.Fatal("UpdateCustomDomainResponse.Body 不可达")
	}

	// 方法值：只做签名绑定，不调用。对指针接收者取方法值，nil 接收者是安全的。
	// 声明式写成多行只是为了不超过 lll 的 120 列上限，签名本身与 SDK 完全一致。
	var c *fc.Client
	//nolint:staticcheck // QF1011: 显式函数类型正是本契约测试的意义，类型推断会让签名漂移无法被编译期发现
	var get func(
		context.Context, *string, map[string]*string, *dara.RuntimeOptions,
	) (*fc.GetCustomDomainResponse, error) = c.GetCustomDomainWithContext
	//nolint:staticcheck // QF1011: 显式函数类型正是本契约测试的意义，类型推断会让签名漂移无法被编译期发现
	var upd func(
		context.Context, *string, *fc.UpdateCustomDomainRequest,
		map[string]*string, *dara.RuntimeOptions,
	) (*fc.UpdateCustomDomainResponse, error) = c.UpdateCustomDomainWithContext
	//nolint:staticcheck // QF1011: 显式函数类型正是本契约测试的意义，类型推断会让签名漂移无法被编译期发现
	var newClient func(*openapiutil.Config) (*fc.Client, error) = fc.NewClient
	if get == nil || upd == nil || newClient == nil {
		t.Fatal("方法值绑定失败")
	}
}
