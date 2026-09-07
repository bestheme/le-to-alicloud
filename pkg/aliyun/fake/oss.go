package fake

import (
	"context"
	"errors"
	"strings"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrNoSuchBucket / ErrCnameNotFound 是 fake 的两个 NotFound 哨兵，与真实 client 的分类一致。
var (
	ErrNoSuchBucket = &aliyun.Error{
		Class: aliyun.ClassNotFound, Op: aliyun.ActionListCname,
		Code: "NoSuchBucket", Err: errors.New("bucket 不存在"),
	}
	ErrCnameNotFound = &aliyun.Error{
		Class: aliyun.ClassNotFound, Op: aliyun.ActionListCname,
		Code: "CnameNotFound", Err: errors.New("bucket 上没有该自定义域名"),
	}
)

// CnameRecord 是 fake 里一条 CNAME 的服务端状态。
type CnameRecord struct {
	Domain   string
	CertRef  string // "" = 无证书
	CertType string // 绑定后为 "CAS"
}

// OSS 实现 aliyun.OSSClient。所有状态在 mu 之下；调用计数按 (bucket, domain) 分账，
// 理由与 fake.FC3 的 getCallsFor 相同：envtest 里 reconciler 常驻，其它用例的 Binding
// 还活着，全局计数不属于任何一个用例。
type OSS struct {
	mu sync.Mutex

	owner   string
	buckets map[string]map[string]CnameRecord // bucket → 小写 domain → 记录

	listErrs     []error
	putErrs      []error
	alwaysPutErr error

	listCallsFor map[string]int
	putCallsFor  map[string]int
}

func NewOSS() *OSS {
	return &OSS{
		buckets:      map[string]map[string]CnameRecord{},
		listCallsFor: map[string]int{},
		putCallsFor:  map[string]int{},
	}
}

// cnameKey 是按 (bucket, domain) 分账的计数键。名字带 cname 前缀是为了不与同包测试里
// 的局部变量 key 撞上。
func cnameKey(bucket, domain string) string { return bucket + "/" + strings.ToLower(domain) }

// SetOwner 设置 ListCname 回报的账号（账号 fencing 用例靠它切换账号）。
func (f *OSS) SetOwner(id string) { f.mu.Lock(); f.owner = id; f.mu.Unlock() }

// AddBucket 新建一个空 bucket（已存在则不动）。
func (f *OSS) AddBucket(bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[bucket]; !ok {
		f.buckets[bucket] = map[string]CnameRecord{}
	}
}

// AddCname 在 bucket 上新建或覆盖一条 CNAME；bucket 不存在时顺带建出来。
func (f *OSS) AddCname(bucket string, rec CnameRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[bucket]; !ok {
		f.buckets[bucket] = map[string]CnameRecord{}
	}
	f.buckets[bucket][strings.ToLower(rec.Domain)] = rec
}

// Cname 返回一条 CNAME 的服务端快照。
func (f *OSS) Cname(bucket, domain string) (CnameRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return CnameRecord{}, false
	}
	rec, ok := b[strings.ToLower(domain)]
	return rec, ok
}

// QueueListErr 让下一次 GetCname 返回该错误。
func (f *OSS) QueueListErr(err error) {
	f.mu.Lock()
	f.listErrs = append(f.listErrs, err)
	f.mu.Unlock()
}

// QueuePutErr 让下一次 PutCnameCert / DeleteCnameCert 在生效前返回该错误。
func (f *OSS) QueuePutErr(err error) { f.mu.Lock(); f.putErrs = append(f.putErrs, err); f.mu.Unlock() }

// AlwaysPutErr 让此后每一次写入都在生效前返回该错误（模拟缺 oss:PutCname 权限）。
func (f *OSS) AlwaysPutErr(err error) { f.mu.Lock(); f.alwaysPutErr = err; f.mu.Unlock() }

// ListCallsFor 返回某条 CNAME 上 GetCname 的调用次数。
func (f *OSS) ListCallsFor(bucket, domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCallsFor[cnameKey(bucket, domain)]
}

// PutCallsFor 返回某条 CNAME 上写入（Put 与 Delete 合计）的调用次数。
func (f *OSS) PutCallsFor(bucket, domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCallsFor[cnameKey(bucket, domain)]
}

func (f *OSS) GetCname(_ context.Context, bucket, domain string) (*aliyun.Cname, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCallsFor[cnameKey(bucket, domain)]++
	if err := pop(&f.listErrs); err != nil {
		return nil, err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return nil, ErrNoSuchBucket
	}
	rec, ok := b[strings.ToLower(domain)]
	if !ok {
		return nil, ErrCnameNotFound
	}
	return &aliyun.Cname{
		Domain: rec.Domain, Status: "Enabled",
		CertRef: rec.CertRef, CertType: rec.CertType, AccountID: f.owner,
	}, nil
}

func (f *OSS) PutCnameCert(_ context.Context, bucket, domain, certRef string) error {
	if certRef == "" {
		return &aliyun.Error{Class: aliyun.ClassPermanent, Op: aliyun.ActionPutCname,
			Code: "InvalidArgument", Err: errors.New("certRef 为空")}
	}
	return f.write(bucket, domain, func(rec *CnameRecord) { rec.CertRef, rec.CertType = certRef, "CAS" })
}

func (f *OSS) DeleteCnameCert(_ context.Context, bucket, domain string) error {
	return f.write(bucket, domain, func(rec *CnameRecord) { rec.CertRef, rec.CertType = "", "" })
}

func (f *OSS) write(bucket, domain string, mutate func(*CnameRecord)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCallsFor[cnameKey(bucket, domain)]++
	if f.alwaysPutErr != nil {
		return f.alwaysPutErr
	}
	if err := pop(&f.putErrs); err != nil {
		return err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return ErrNoSuchBucket
	}
	rec, ok := b[strings.ToLower(domain)]
	if !ok {
		return ErrCnameNotFound
	}
	mutate(&rec)
	b[strings.ToLower(domain)] = rec
	return nil
}

var _ aliyun.OSSClient = (*OSS)(nil)
