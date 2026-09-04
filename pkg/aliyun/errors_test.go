package aliyun_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alibabacloud-go/tea/dara"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func sdkErr(code string, status int) error {
	c, s := code, status
	return &dara.SDKError{Code: &c, StatusCode: &s, Message: dara.String("secret body must not leak")}
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
	got := aliyun.Classify("Upload", sdkErr("InvalidParameter", 400))
	if s := got.Error(); strings.Contains(s, "secret body") {
		t.Errorf("错误文本不应包含 SDK Message: %s", s)
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
