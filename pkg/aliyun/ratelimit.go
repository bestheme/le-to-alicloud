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
)

func newLimiter(k LimitKind) *rate.Limiter {
	switch k {
	case LimitCASList:
		return rate.NewLimiter(rate.Limit(8), 1)
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
	if k == LimitCASList {
		return "cas-list"
	}
	return "cas-write"
}
