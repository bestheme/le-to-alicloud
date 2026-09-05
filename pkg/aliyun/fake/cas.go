// Package fake 提供内存版 CASClient，支持错误注入与「服务端已成功但响应丢失」场景。
package fake

import (
	"context"
	"errors"
	"strings"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrDuplicateName 模拟 CAS 的同名拒绝。
var ErrDuplicateName = &aliyun.Error{
	Class: aliyun.ClassPermanent,
	Op:    "Upload",
	Code:  "CertNameDuplicated",
	Err:   errors.New("name already exists"),
}

// Cert 是 fake 中的一张证书。
type Cert struct {
	ID      int64
	Name    string
	CertPEM []byte
	KeyPEM  []byte
	Token   string
}

// CAS 实现 aliyun.CASClient。所有状态都在 mu 之下；
// 请通过 Certs / Has / UploadCalls / DeleteCalls / FindCalls 读取，
// 这些方法持锁，可以安全地在 reconciler goroutine 与测试 goroutine 之间并发使用。
type CAS struct {
	mu sync.Mutex

	nextID  int64
	certs   map[int64]Cert
	byToken map[string]int64

	uploadErrs []error
	deleteErrs []error
	findErrs   []error

	failAfterCommit error

	uploadCalls int
	deleteCalls int
	findCalls   int
}

func NewCAS() *CAS {
	return &CAS{nextID: 1000, certs: map[int64]Cert{}, byToken: map[string]int64{}}
}

// QueueUploadErr 让下一次 Upload 在写入前返回该错误。
func (f *CAS) QueueUploadErr(err error) {
	f.mu.Lock()
	f.uploadErrs = append(f.uploadErrs, err)
	f.mu.Unlock()
}

// QueueDeleteErr 让下一次 Delete 在生效前返回该错误。
func (f *CAS) QueueDeleteErr(err error) {
	f.mu.Lock()
	f.deleteErrs = append(f.deleteErrs, err)
	f.mu.Unlock()
}

// QueueFindErr 让下一次 FindUploaded 返回该错误。
func (f *CAS) QueueFindErr(err error) {
	f.mu.Lock()
	f.findErrs = append(f.findErrs, err)
	f.mu.Unlock()
}

// FailNextUploadAfterCommit 让下一次 Upload 先写入（服务端成功）再返回 err（响应丢失）。
func (f *CAS) FailNextUploadAfterCommit(err error) {
	f.mu.Lock()
	f.failAfterCommit = err
	f.mu.Unlock()
}

func pop(q *[]error) error {
	if len(*q) == 0 {
		return nil
	}
	e := (*q)[0]
	*q = (*q)[1:]
	return e
}

func (f *CAS) Upload(_ context.Context, name string, certPEM, keyPEM []byte, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCalls++
	if err := pop(&f.uploadErrs); err != nil {
		return 0, err
	}
	// ClientToken 幂等：同 token 返回同一 ID
	if id, ok := f.byToken[token]; ok && token != "" {
		return id, nil
	}
	for _, c := range f.certs {
		if c.Name == name {
			return 0, ErrDuplicateName
		}
	}
	f.nextID++
	id := f.nextID
	f.certs[id] = Cert{
		ID:      id,
		Name:    name,
		CertPEM: append([]byte(nil), certPEM...),
		KeyPEM:  append([]byte(nil), keyPEM...),
		Token:   token,
	}
	if token != "" {
		f.byToken[token] = id
	}
	if f.failAfterCommit != nil {
		err := f.failAfterCommit
		f.failAfterCommit = nil
		return 0, err
	}
	return id, nil
}

func (f *CAS) Delete(_ context.Context, certID int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if err := pop(&f.deleteErrs); err != nil {
		return err
	}
	if _, ok := f.certs[certID]; !ok {
		return &aliyun.Error{Class: aliyun.ClassNotFound, Op: "Delete", Code: "CertNotExist", Err: errors.New("not found")}
	}
	delete(f.certs, certID)
	// 证书没了，指向它的幂等映射也必须失效：否则同 token 再次 Upload 会返回一个
	// 已经不存在的 ID，controller 会以为「云上还在」而永远不重传。
	for tok, id := range f.byToken {
		if id == certID {
			delete(f.byToken, tok)
		}
	}
	return nil
}

func (f *CAS) FindUploaded(_ context.Context, domainHint string) ([]aliyun.CertSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls++
	if err := pop(&f.findErrs); err != nil {
		return nil, err
	}
	var out []aliyun.CertSummary
	for _, c := range f.certs {
		// fake 不解析 PEM；用名字前缀近似 Keyword 匹配域名的行为
		if domainHint == "" || strings.Contains(c.Name, sanitizeHint(domainHint)) {
			out = append(out, aliyun.CertSummary{CertID: c.ID, Name: c.Name})
		}
	}
	return out, nil
}

func sanitizeHint(s string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(strings.SplitN(s, ".", 2)[0])
}

// Has 判断 certID 是否存在。
func (f *CAS) Has(id int64) bool { f.mu.Lock(); defer f.mu.Unlock(); _, ok := f.certs[id]; return ok }

// Certs 返回快照。
func (f *CAS) Certs() []Cert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Cert, 0, len(f.certs))
	for _, c := range f.certs {
		out = append(out, c)
	}
	return out
}

// UploadCalls 返回 Upload 的调用次数。持锁，可与 reconciler goroutine 并发调用。
func (f *CAS) UploadCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.uploadCalls }

// DeleteCalls 返回 Delete 的调用次数。持锁，可与 reconciler goroutine 并发调用。
func (f *CAS) DeleteCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.deleteCalls }

// FindCalls 返回 FindUploaded 的调用次数。持锁，可与 reconciler goroutine 并发调用。
func (f *CAS) FindCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.findCalls }

var _ aliyun.CASClient = (*CAS)(nil)
