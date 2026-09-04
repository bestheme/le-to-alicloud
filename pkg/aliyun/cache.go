package aliyun

import "sync"

// ClientKey 决定 client 何时必须重建：凭证 Secret 的 resourceVersion 一变就重建，
// 否则轮换后的 AK 直到 Pod 重启才生效。
type ClientKey struct {
	Namespace       string
	Name            string
	ResourceVersion string
	Region          string
	Endpoint        string
}

func (k ClientKey) identity() string {
	return k.Namespace + "/" + k.Name + "/" + k.Region + "/" + k.Endpoint
}

// ClientCache 每个 (namespace, name, region, endpoint) 只保留最新 resourceVersion 的 client。
type ClientCache struct {
	mu sync.Mutex
	m  map[string]entry
}

type entry struct {
	rv     string
	client CASClient
}

func NewClientCache() *ClientCache { return &ClientCache{m: map[string]entry{}} }

// GetOrBuild 返回缓存 client，或用 build 构造并替换旧版本。
func (c *ClientCache) GetOrBuild(key ClientKey, build func() (CASClient, error)) (CASClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := key.identity()
	if e, ok := c.m[id]; ok && e.rv == key.ResourceVersion {
		return e.client, nil
	}
	cl, err := build()
	if err != nil {
		return nil, err
	}
	c.m[id] = entry{rv: key.ResourceVersion, client: cl}
	return cl, nil
}

// Len 返回缓存条目数（测试用）。
func (c *ClientCache) Len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.m) }
