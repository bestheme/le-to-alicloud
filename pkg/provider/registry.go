package provider

import (
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register 注册一个 provider，键取 p.Name()（必须等于 CRD 的 spec.target.type 取值）。
//
// 重复注册直接 panic：注册发生在 init() 里，重名意味着两个包争同一个 target type，
// 这是编译期就该被发现的接线错误，静默覆盖会让「写进了哪个云」变成启动顺序的函数。
func Register(p Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if _, dup := registry[name]; dup {
		panic("provider: 重复注册 " + name)
	}
	registry[name] = p
}

// Get 按 target type 取 provider。
func Get(typeName string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[typeName]
	return p, ok
}

// Names 返回已注册的 target type，已排序（供日志与错误消息使用）。
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
