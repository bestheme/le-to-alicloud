package aliyun

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// LimitKind 区分不同 QPS 上限的调用通道。
type LimitKind int

const (
	// LimitCASList 对应 ListUserCertificateOrder（官方 QPS 10），留余量取 8。
	LimitCASList LimitKind = iota
	// LimitCASWrite 对应 Upload / Delete（官方 QPS 100），取 50。
	LimitCASWrite
	// LimitFC3 对应 GetCustomDomain / UpdateCustomDomain。
	//
	// 阿里云没有公布 FC3 的账号级频控阈值，spec §12.3 #10 把它列为必测项。在实测出来
	// 之前取 5 QPS / burst 1 这个保守值：drift 检测每小时一轮，即使几百个 Binding 同时
	// 被唤醒，排队一分钟也远比在生产上撞出 Throttling 便宜。实测后再放宽。
	LimitFC3
)

func newLimiter(k LimitKind) *rate.Limiter {
	switch k {
	case LimitCASList:
		return rate.NewLimiter(rate.Limit(8), 1)
	case LimitFC3:
		return rate.NewLimiter(rate.Limit(5), 1)
	default:
		return rate.NewLimiter(rate.Limit(50), 10)
	}
}

// Limiters 按 (key, kind) 维护限流器。key 通常是 accessKeyId，因为阿里云限流按账号计。
type Limiters struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

func NewLimiters() *Limiters { return &Limiters{m: map[string]*rate.Limiter{}} }

// Wait 阻塞直到获得配额或 ctx 结束。
func (l *Limiters) Wait(ctx context.Context, key string, kind LimitKind) error {
	l.mu.Lock()
	id := key + "/" + kindName(kind)
	lim, ok := l.m[id]
	if !ok {
		lim = newLimiter(kind)
		l.m[id] = lim
	}
	l.mu.Unlock()
	return lim.Wait(ctx)
}

func kindName(k LimitKind) string {
	switch k {
	case LimitCASList:
		return "cas-list"
	case LimitFC3:
		return "fc3"
	default:
		return "cas-write"
	}
}
