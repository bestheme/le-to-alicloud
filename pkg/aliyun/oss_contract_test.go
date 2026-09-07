package aliyun

import (
	"context"
	"testing"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
)

// TestOSSSDKContract 把本项目依赖的 SDK v2 字段名与方法签名钉在编译期：SDK 升级改了名字，
// 这里先红。只做绑定，不发请求。
func TestOSSSDKContract(t *testing.T) {
	req := &oss.PutCnameRequest{
		Bucket: oss.Ptr("b"),
		BucketCnameConfiguration: &oss.BucketCnameConfiguration{
			Cname: &oss.Cname{
				Domain: oss.Ptr("www.example.com"),
				CertificateConfiguration: &oss.CertificateConfiguration{
					CertId: oss.Ptr("1-cn-hangzhou"), Force: oss.Ptr(true), DeleteCertificate: oss.Ptr(false),
				},
			},
		},
	}
	if req.BucketCnameConfiguration.Cname.CertificateConfiguration.CertId == nil {
		t.Fatal("PutCnameRequest…CertificateConfiguration.CertId 不可达")
	}
	res := &oss.ListCnameResult{Owner: oss.Ptr("1"), Cnames: []oss.CnameInfo{{
		Domain: oss.Ptr("d"), Status: oss.Ptr("Enabled"),
		Certificate: &oss.CnameCertificate{CertId: oss.Ptr("1-cn-hangzhou"), Type: oss.Ptr("CAS")},
	}}}
	if res.Cnames[0].Certificate.CertId == nil {
		t.Fatal("ListCnameResult.Cnames[].Certificate.CertId 不可达")
	}
	var c *oss.Client
	//nolint:staticcheck // QF1011: 显式函数类型正是本契约测试的意义，类型推断会让签名漂移无法被编译期发现
	var list func(context.Context, *oss.ListCnameRequest,
		...func(*oss.Options)) (*oss.ListCnameResult, error) = c.ListCname
	//nolint:staticcheck // QF1011: 同上
	var put func(context.Context, *oss.PutCnameRequest, ...func(*oss.Options)) (*oss.PutCnameResult, error) = c.PutCname
	if list == nil || put == nil {
		t.Fatal("方法值绑定失败")
	}
	se := &oss.ServiceError{Code: "c", StatusCode: 400}
	if se.Code == "" || se.StatusCode == 0 {
		t.Fatal("ServiceError.Code / StatusCode 不可达")
	}
}
