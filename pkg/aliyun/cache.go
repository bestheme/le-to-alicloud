package aliyun

import "sync"

// ClientKey 决定 client 何时必须重建：凭证 Secret 的 resourceVersion 一变就重建，
// 否则轮换后的 AK 直到 Pod 重启才生效。
//
// Region / Endpoint / ResourceGroupID 都必须进 identity()：三者都会改变同一份凭证
// 构造出来的 client 的实际行为。尤其是 ResourceGroupID——它决定证书被传进哪个资源组、
// 存在性探测又在哪个资源组里查——漏掉它会让同 namespace、同凭证、同 region 但不同
// 资源组的两个 CR 串用一个 client：证书落进别人的资源组，探测再也找不到它，于是每轮
// 都判定「被人删了」并重传。
type ClientKey struct {
	Namespace       string
	Name            string
	ResourceVersion string
	Region          string
	Endpoint        string
	ResourceGroupID string
	// Type 是 provider client 的种类（target.type）。同一份凭证、同一个 region 下 FC3 与 OSS
	// 的 client 是两个不同的对象，缺了它两者会串用。CAS 的缓存留空。
	Type string
}

func (k ClientKey) identity() string {
	return k.Namespace + "/" + k.Name + "/" + k.Region + "/" + k.Endpoint + "/" + k.ResourceGroupID + "/" + k.Type
}

type entry[T any] struct {
	rv     string
	client T
}

// ClientCache 每个 (namespace, name, region, endpoint, resourceGroupId, type) 只保留最新
// resourceVersion 的 client。
//
// 泛型是因为 CAS 与 FC3 的 client 类型不同，而两者的缓存规则完全一样：与其让缓存存
// any 再到处断言，不如让每个服务各持一份类型确定的缓存。
type ClientCache[T any] struct {
	mu sync.Mutex
	m  map[string]entry[T]
}

func NewClientCache[T any]() *ClientCache[T] { return &ClientCache[T]{m: map[string]entry[T]{}} }

// GetOrBuild 返回缓存 client，或用 build 构造并替换旧版本。
func (c *ClientCache[T]) GetOrBuild(key ClientKey, build func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := key.identity()
	if e, ok := c.m[id]; ok && e.rv == key.ResourceVersion {
		return e.client, nil
	}
	cl, err := build()
	if err != nil {
		var zero T
		return zero, err
	}
	c.m[id] = entry[T]{rv: key.ResourceVersion, client: cl}
	return cl, nil
}

// Len 返回缓存条目数（测试用）。
func (c *ClientCache[T]) Len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.m) }
