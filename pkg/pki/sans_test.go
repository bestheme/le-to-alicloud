package pki_test

import (
	"reflect"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

func TestCovers(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"api.example.com", "api.example.com", true},
		{"API.Example.COM", "api.example.com", true},
		{"api.example.com.", "api.example.com", true},
		{"api.example.com", "www.example.com", false},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.b.example.com", false},
		{"*.com", "example.com", false},              // 通配符不能覆盖公共后缀层级
		{"a*.example.com", "abc.example.com", false}, // 只接受整标签通配
		{"*.*.example.com", "a.b.example.com", false},
		{"", "api.example.com", false},
		{"api.example.com", "", false},
	}
	for _, c := range cases {
		if got := pki.Covers(c.pattern, c.host); got != c.want {
			t.Errorf("Covers(%q,%q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestMissing(t *testing.T) {
	got := pki.Missing(
		[]string{"*.example.com", "example.com"},
		[]string{"api.example.com", "example.com", "deep.a.example.com", "other.org", "api.example.com"},
	)
	want := []string{"deep.a.example.com", "other.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %v, want %v", got, want)
	}
	if m := pki.Missing([]string{"a.example.com"}, nil); len(m) != 0 {
		t.Errorf("空 required 应返回空，得到 %v", m)
	}
}
