package aliyun_test

import (
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func TestClientCache_ReusesSameResourceVersion(t *testing.T) {
	c := aliyun.NewClientCache()
	builds := 0
	build := func() (aliyun.CASClient, error) { builds++; return fake.NewCAS(), nil }
	k := aliyun.ClientKey{Namespace: "ns", Name: "s", ResourceVersion: "1", Region: "cn-hangzhou"}
	a, _ := c.GetOrBuild(k, build)
	b, _ := c.GetOrBuild(k, build)
	if a != b || builds != 1 {
		t.Fatalf("同 key 应复用，builds=%d", builds)
	}
}

func TestClientCache_RebuildsOnResourceVersionChange(t *testing.T) {
	c := aliyun.NewClientCache()
	builds := 0
	build := func() (aliyun.CASClient, error) { builds++; return fake.NewCAS(), nil }
	k1 := aliyun.ClientKey{Namespace: "ns", Name: "s", ResourceVersion: "1", Region: "cn-hangzhou"}
	k2 := k1
	k2.ResourceVersion = "2"
	a, _ := c.GetOrBuild(k1, build)
	b, _ := c.GetOrBuild(k2, build)
	if a == b || builds != 2 {
		t.Fatalf("resourceVersion 变化必须重建，builds=%d", builds)
	}
	// 旧版本应被逐出，避免无限增长
	if c.Len() != 1 {
		t.Errorf("同 namespace/name 只保留最新一个，实际 %d", c.Len())
	}
}
