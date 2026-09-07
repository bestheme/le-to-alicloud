package aliyun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	credential "github.com/aliyun/credentials-go/credentials"
)

func TestCnameFromList(t *testing.T) {
	res := &oss.ListCnameResult{
		Bucket: oss.Ptr("b1"), Owner: oss.Ptr("1234567890"),
		Cnames: []oss.CnameInfo{
			{Domain: oss.Ptr("bare.example.com"), Status: oss.Ptr("Enabled")},
			{Domain: oss.Ptr("www.example.com"), Status: oss.Ptr("Enabled"),
				Certificate: &oss.CnameCertificate{Type: oss.Ptr("CAS"), CertId: oss.Ptr("27087165-cn-hangzhou")}},
		},
	}
	got, err := cnameFromList(res, "WWW.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.CertRef != "27087165-cn-hangzhou" || got.CertType != "CAS" || got.AccountID != "1234567890" ||
		got.Domain != "www.example.com" {
		t.Fatalf("剪裁结果不对: %+v", got)
	}

	bare, err := cnameFromList(res, "bare.example.com")
	if err != nil || bare.CertRef != "" || bare.CertType != "" {
		t.Fatalf("无证书的 CNAME 应给出空 certRef: %+v, %v", bare, err)
	}

	_, err = cnameFromList(res, "absent.example.com")
	if ClassOf(err) != ClassNotFound {
		t.Fatalf("找不到 domain 应为 NotFound: %v", err)
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "CnameNotFound" {
		t.Fatalf("Code 应为 CnameNotFound: %v", err)
	}
}

func TestClassifyOSS(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrClass
		code string
	}{
		{"nil", nil, ClassPermanent, ""},
		{"NoSuchBucket 404", &oss.ServiceError{Code: "NoSuchBucket", StatusCode: 404}, ClassNotFound, "NoSuchBucket"},
		{"AccessDenied 403", &oss.ServiceError{Code: "AccessDenied", StatusCode: 403}, ClassAuth, "AccessDenied"},
		{"InvalidAccessKeyId", &oss.ServiceError{Code: "InvalidAccessKeyId", StatusCode: 403},
			ClassAuth, "InvalidAccessKeyId"},
		{"5xx", &oss.ServiceError{Code: "InternalError", StatusCode: 500}, ClassRetryable, "InternalError"},
		{"429 限流", &oss.ServiceError{Code: "TooManyRequests", StatusCode: 429},
			ClassRetryable, "TooManyRequests"},
		{"参数错误", &oss.ServiceError{Code: "InvalidArgument", StatusCode: 400}, ClassPermanent, "InvalidArgument"},
		{"超时", context.DeadlineExceeded, ClassRetryable, "Timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOSS(ActionPutCname, tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("nil 应原样返回: %v", got)
				}
				return
			}
			if ClassOf(got) != tc.want {
				t.Errorf("class = %s, want %s", ClassOf(got), tc.want)
			}
			var ae *Error
			if !errors.As(got, &ae) || ae.Code != tc.code || ae.Op != ActionPutCname {
				t.Errorf("Code/Op 不对: %v", got)
			}
		})
	}
}

// 响应体绝不能进错误链：Message 与 Snapshot 都是可能回显请求参数的地方。
func TestClassifyOSS_DropsBody(t *testing.T) {
	err := classifyOSS(ActionListCname, &oss.ServiceError{
		Code: "AccessDenied", StatusCode: 403, Message: "SECRET-IN-MESSAGE", Snapshot: []byte("SECRET-IN-BODY"),
	})
	for _, leak := range []string{"SECRET-IN-MESSAGE", "SECRET-IN-BODY"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误链带出了响应内容: %v", err)
		}
	}
}

// stubCredential 只实现本测试用到的 GetCredential；其余方法返回零值。
type stubCredential struct {
	credential.Credential
	model *credential.CredentialModel
	err   error
}

func (s stubCredential) GetCredential() (*credential.CredentialModel, error) { return s.model, s.err }

func TestOSSCredentials_Adapter(t *testing.T) {
	p := ossCredentials{cred: stubCredential{model: &credential.CredentialModel{
		AccessKeyId:     dara.String("LTAI-FAKE-ID"),
		AccessKeySecret: dara.String("FAKE-SECRET"),
		SecurityToken:   dara.String("FAKE-STS"),
	}}}
	got, err := p.GetCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "LTAI-FAKE-ID" || got.AccessKeySecret != "FAKE-SECRET" || got.SecurityToken != "FAKE-STS" {
		t.Fatalf("字段搬运不对: %+v", got)
	}

	failing := ossCredentials{cred: stubCredential{err: errors.New("boom")}}
	if _, err := failing.GetCredentials(context.Background()); err == nil {
		t.Fatal("底层错误应透传")
	}
	if _, err := (ossCredentials{cred: stubCredential{}}).GetCredentials(context.Background()); err == nil {
		t.Fatal("空模型应报错而不是返回空凭证")
	}
}

func TestNewOSSClient_RequiresRegion(t *testing.T) {
	if _, err := NewOSSClient(stubCredential{}, OSSClientConfig{}); ClassOf(err) != ClassPermanent {
		t.Fatalf("缺 region 应为 Permanent: %v", err)
	}
	c, err := NewOSSClient(stubCredential{}, OSSClientConfig{Region: "cn-hangzhou"})
	if err != nil || c == nil {
		t.Fatalf("有 region 应构造成功: %v", err)
	}
}
