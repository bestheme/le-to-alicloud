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

// 凭证之外的每一个分量都会改变 client 的实际行为，所以都必须各占一个缓存槽。
// ResourceGroupID 尤其要紧：串用会把证书传进别人的资源组，而探测在错误的资源组里
// 永远找不到它——症状是每轮重传，且完全不报错。
func TestClientCache_DistinguishesNonCredentialComponents(t *testing.T) {
	base := aliyun.ClientKey{Namespace: "ns", Name: "s", ResourceVersion: "1", Region: "cn-hangzhou"}
	variants := map[string]func(k *aliyun.ClientKey){
		"ResourceGroupID": func(k *aliyun.ClientKey) { k.ResourceGroupID = "rg-other" },
		"Region":          func(k *aliyun.ClientKey) { k.Region = "cn-shanghai" },
		"Endpoint":        func(k *aliyun.ClientKey) { k.Endpoint = "cas.vpc.example.com" },
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			c := aliyun.NewClientCache()
			builds := 0
			build := func() (aliyun.CASClient, error) { builds++; return fake.NewCAS(), nil }
			other := base
			mutate(&other)
			a, _ := c.GetOrBuild(base, build)
			b, _ := c.GetOrBuild(other, build)
			if a == b {
				t.Errorf("%s 不同必须各自构建 client，实际复用了同一个", name)
			}
			if builds != 2 {
				t.Errorf("%s 不同应构建两次，实际 builds=%d", name, builds)
			}
			if c.Len() != 2 {
				t.Errorf("%s 不同应各占一个缓存槽，实际 %d", name, c.Len())
			}
		})
	}
}
