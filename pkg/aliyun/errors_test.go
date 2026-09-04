package aliyun_test

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/alibabacloud-go/tea/dara"
	tea "github.com/alibabacloud-go/tea/tea"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

const secret = "secret body must not leak"

func sdkErr(code string, status int) error {
	c, s := code, status
	return &dara.SDKError{Code: &c, StatusCode: &s, Message: dara.String(secret)}
}

// teaErr 构造 darabonba-openapi 实际返回给调用方的错误类型。client.go 在
// DisableSDKError 未设置时会对每个错误调用 dara.TeaSDKError()，把内层的
// *dara.SDKError 转成独立的 *tea.SDKError，所以真实调用看到的是这一种。
func teaErr(code string, status int) error {
	c, s := code, status
	return &tea.SDKError{Code: &c, StatusCode: &s, Message: tea.String(secret)}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want aliyun.ErrClass
	}{
		{"throttling", sdkErr("Throttling.User", 400), aliyun.ClassRetryable},
		{"5xx", sdkErr("Whatever", 503), aliyun.ClassRetryable},
		{"internal", sdkErr("InternalError", 500), aliyun.ClassRetryable},
		{"bad ak", sdkErr("InvalidAccessKeyId.NotFound", 404), aliyun.ClassAuth},
		{"forbidden", sdkErr("Forbidden.RAM", 403), aliyun.ClassAuth},
		{"not exist", sdkErr("CertNotExist", 400), aliyun.ClassNotFound},
		{"param", sdkErr("InvalidParameter", 400), aliyun.ClassPermanent},
		{"ctx deadline", context.DeadlineExceeded, aliyun.ClassRetryable},
		{"plain", errors.New("boom"), aliyun.ClassPermanent},

		// 同样的矩阵，但用真实调用路径上的 *tea.SDKError
		{"tea throttling", teaErr("Throttling.User", 400), aliyun.ClassRetryable},
		{"tea 5xx", teaErr("Whatever", 503), aliyun.ClassRetryable},
		{"tea forbidden", teaErr("Forbidden.RAM", 403), aliyun.ClassAuth},
		{"tea bad ak", teaErr("InvalidAccessKeyId.NotFound", 404), aliyun.ClassAuth},
		{"tea not exist", teaErr("CertNotExist", 400), aliyun.ClassNotFound},
		{"tea param", teaErr("InvalidParameter", 400), aliyun.ClassPermanent},
	}
	for _, c := range cases {
		got := aliyun.Classify("Upload", c.err)
		if aliyun.ClassOf(got) != c.want {
			t.Errorf("%s: class = %v, want %v (%v)", c.name, aliyun.ClassOf(got), c.want, got)
		}
	}
}

func TestClassify_Nil(t *testing.T) {
	if err := aliyun.Classify("Upload", nil); err != nil {
		t.Errorf("nil 应原样返回，得到 %v", err)
	}
}

func TestClassify_DoesNotLeakMessage(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"dara.SDKError", sdkErr("InvalidParameter", 400)},
		{"tea.SDKError", teaErr("InvalidParameter", 400)},
	} {
		got := aliyun.Classify("Upload", c.err)
		if s := got.Error(); strings.Contains(s, secret) {
			t.Errorf("%s: 错误文本不应包含 SDK Message: %s", c.name, s)
		}
	}
}

// timeoutErr 实现 net.Error 且 Timeout() 为 true。
// net.Error 除 error 外还要求 Timeout() 与（已废弃但仍在接口里的）Temporary()。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify_NetTimeout(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code string
	}{
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, "NetTimeout"},
		{"custom net.Error", timeoutErr{}, "NetTimeout"},
	} {
		// 先确认前提：这些错误确实满足 net.Error 且 Timeout() 为 true
		var ne net.Error
		if !errors.As(c.err, &ne) || !ne.Timeout() {
			t.Fatalf("%s: 前提不成立，应实现 net.Error 且 Timeout()==true", c.name)
		}
		got := aliyun.Classify("FindUploaded", c.err)
		if aliyun.ClassOf(got) != aliyun.ClassRetryable {
			t.Errorf("%s: class = %v, want Retryable (%v)", c.name, aliyun.ClassOf(got), got)
		}
		var e *aliyun.Error
		if errors.As(got, &e) && e.Code != c.code {
			t.Errorf("%s: Code = %q, want %q", c.name, e.Code, c.code)
		}
	}
}

func TestClassify_Idempotent(t *testing.T) {
	first := aliyun.Classify("Upload", sdkErr("Throttling", 400))
	second := aliyun.Classify("Retry", first)
	if first != second {
		t.Errorf("已分类错误不应被重新包装")
	}
}

func TestClassify_KeepsOpAndCode(t *testing.T) {
	err := aliyun.Classify("Delete", sdkErr("CertNotExist", 400))
	var e *aliyun.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *aliyun.Error, got %T", err)
	}
	if e.Op != "Delete" || e.Code != "CertNotExist" {
		t.Errorf("Op/Code = %q/%q, want Delete/CertNotExist", e.Op, e.Code)
	}
}

func TestClassOf_NonPackageError(t *testing.T) {
	if got := aliyun.ClassOf(errors.New("boom")); got != aliyun.ClassPermanent {
		t.Errorf("非本包错误应为 Permanent，得到 %v", got)
	}
}

func TestErrClass_String(t *testing.T) {
	cases := map[aliyun.ErrClass]string{
		aliyun.ClassPermanent: "Permanent",
		aliyun.ClassRetryable: "Retryable",
		aliyun.ClassAuth:      "Auth",
		aliyun.ClassNotFound:  "NotFound",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
