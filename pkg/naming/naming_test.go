package naming_test

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

const fp = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

var casNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func TestCASName_Basic(t *testing.T) {
	got := naming.CASName("timehorse-api", fp)
	if got != "timehorse_api_ab12cd34ef56" {
		t.Fatalf("got %q", got)
	}
}

func TestCASName_SanitizesAndTruncates(t *testing.T) {
	long := strings.Repeat("api.example.com-", 6) // 96 字符，含 . 和 -
	got := naming.CASName(long, fp)
	if len(got) > 63 {
		t.Errorf("长度 %d 超过 63", len(got))
	}
	if !casNameRe.MatchString(got) {
		t.Errorf("含非法字符: %q", got)
	}
	if !strings.HasSuffix(got, "_ab12cd34ef56") {
		t.Errorf("应以 _<指纹12位> 结尾: %q", got)
	}
	if strings.Contains(got, "__ab12") {
		t.Errorf("截断后不应留下尾部下划线导致双下划线: %q", got)
	}
}

func TestCASName_EmptyOrAllInvalidPrefix(t *testing.T) {
	got := naming.CASName("...", fp)
	if !strings.HasPrefix(got, "cert_") {
		t.Errorf("全非法字符的名字应回退为 cert_ 前缀: %q", got)
	}
}

func TestCASName_DifferentFingerprintsDiffer(t *testing.T) {
	a := naming.CASName("x", fp)
	b := naming.CASName("x", "ff"+fp[2:])
	if a == b {
		t.Errorf("不同指纹必须得到不同名字")
	}
}

func TestClientToken(t *testing.T) {
	got := naming.ClientToken(types.UID("6ba7b810-9dad-11d1-80b4-00c04fd430c8"), fp)
	if len(got) != 48 {
		t.Fatalf("长度 %d, want 48: %q", len(got), got)
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(got) {
		t.Errorf("应为 48 位小写 hex: %q", got)
	}
	if !strings.HasPrefix(got, "6ba7b8109dad11d1") {
		t.Errorf("前 16 位应为去连字符的 UID: %q", got)
	}
	if !strings.HasSuffix(got, fp[:32]) {
		t.Errorf("后 32 位应为指纹前 32 位")
	}
}
