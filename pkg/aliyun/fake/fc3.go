package fake

import (
	"context"
	"errors"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrDomainNotFound 模拟 FC3 对不存在域名的 404。
//
// spec §12.3 #14：已实测（2026-09-05，RESULTS.md #14）——真实错误码即 `DomainNameNotFound`、
// HTTP 404，`aliyun.classifyCode` 据此归为 `ClassNotFound`，`CodeTargetNotFound` 分支按预期
// 走得到。这个哨兵不再是猜的，与云侧一致。对照名（`.invalid` 保留顶级域，不可能是真实域名）
// 返回同一个码，说明该码不依赖域名是否形如可注册域名。实测结论见 test/integration/RESULTS.md
//
// Get 与 Update 共用这一个哨兵值：下游测试既可以 errors.Is 也可以直接比较指针。
// 代价是 Update 路径上报的 Op 也写着 GetCustomDomain——分类与哨兵身份都对，只有这个
// 标签不精确，不值得为它牺牲哨兵的唯一性。
var ErrDomainNotFound = &aliyun.Error{
	Class: aliyun.ClassNotFound, Op: aliyun.ActionGetCustomDomain,
	Code: "DomainNameNotFound", Err: errors.New("custom domain not found"),
}

// Domain 是 fake 里的一个自定义域名——服务端侧的完整状态，**含私钥**：
// 它扮演的就是云，云上确实存着私钥。上层拿到的 aliyun.CustomDomain 仍然不含私钥。
type Domain struct {
	DomainName string
	Protocol   string
	CertName   string
	CertPEM    []byte
	KeyPEM     []byte
	// Echo 是「本项目从不解释、但必须原样回填」的那部分配置的替身（真实世界里是
	// routeConfig / wafConfig 等）。测试断言它在一次 Apply 后仍然相等，就等于断言
	// read-modify-write 没有把别人的配置洗掉。
	Echo any
}

// FC3 实现 aliyun.FC3Client。所有状态都在 mu 之下；请通过 Domain / GetCalls /
// UpdateCalls 读取，这些方法持锁，可以安全地在 reconciler 与测试 goroutine 之间并发使用。
type FC3 struct {
	mu sync.Mutex

	accountID string
	domains   map[string]Domain

	getErrs    []error
	updateErrs []error

	failAfterCommit error

	getCalls    int
	updateCalls int
	// 按域名分账的计数。envtest 里 reconciler 是常驻的，而 envtest 从不回收 namespace，
	// 所以**先前每个用例建的 Binding 都还活着、还在被 reconcile**；任何一个被 watch
	// 唤醒都会撞进全局计数里。用例真正想断言的从来都是「**我这个目标**上没发生云调用」，
	// 而域名按 <name>.<namespace>.example.com 命名，天然是每个用例独占的——
	// 于是按域名分账正好就是断言本来的那条隔离边界。
	getCallsFor    map[string]int
	updateCallsFor map[string]int
}

func NewFC3() *FC3 {
	return &FC3{
		domains:        map[string]Domain{},
		getCallsFor:    map[string]int{},
		updateCallsFor: map[string]int{},
	}
}

// SetAccountID 设置 GetCustomDomain 回报的账号（账号 fencing 用例要靠它切换账号）。
func (f *FC3) SetAccountID(id string) { f.mu.Lock(); f.accountID = id; f.mu.Unlock() }

// AddDomain 新建或覆盖一个域名。
func (f *FC3) AddDomain(d Domain) {
	f.mu.Lock()
	f.domains[d.DomainName] = d
	f.mu.Unlock()
}

// RemoveDomain 删除域名，用于模拟「域名被 Terraform 删掉了」。
func (f *FC3) RemoveDomain(name string) { f.mu.Lock(); delete(f.domains, name); f.mu.Unlock() }

// Domain 返回域名的服务端快照。
func (f *FC3) Domain(name string) (Domain, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[name]
	return d, ok
}

// QueueGetErr 让下一次 GetCustomDomain 返回该错误。
func (f *FC3) QueueGetErr(err error) { f.mu.Lock(); f.getErrs = append(f.getErrs, err); f.mu.Unlock() }

// QueueUpdateErr 让下一次 UpdateCustomDomain 在生效前返回该错误。
func (f *FC3) QueueUpdateErr(err error) {
	f.mu.Lock()
	f.updateErrs = append(f.updateErrs, err)
	f.mu.Unlock()
}

// FailNextUpdateAfterCommit 让下一次 Update 先写入（服务端成功）再返回 err（响应丢失）。
func (f *FC3) FailNextUpdateAfterCommit(err error) {
	f.mu.Lock()
	f.failAfterCommit = err
	f.mu.Unlock()
}

// GetCalls 返回 GetCustomDomain 的**全局**调用次数。持锁，可与 reconciler goroutine 并发调用。
//
// 多个 reconciler 并发跑时这个数字不属于任何一个用例，见 getCallsFor 的说明。
// envtest 里请一律用 GetCallsFor。
func (f *FC3) GetCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.getCalls }

// UpdateCalls 返回 UpdateCustomDomain 的**全局**调用次数。同 GetCalls。
func (f *FC3) UpdateCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.updateCalls }

// GetCallsFor 返回某个域名上 GetCustomDomain 的调用次数。
func (f *FC3) GetCallsFor(domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCallsFor[domain]
}

// UpdateCallsFor 返回某个域名上 UpdateCustomDomain 的调用次数。
func (f *FC3) UpdateCallsFor(domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateCallsFor[domain]
}

func (f *FC3) GetCustomDomain(_ context.Context, domain string) (*aliyun.CustomDomain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	f.getCallsFor[domain]++
	if err := pop(&f.getErrs); err != nil {
		return nil, err
	}
	d, ok := f.domains[domain]
	if !ok {
		return nil, ErrDomainNotFound
	}
	// 与真实 client 一致：私钥不出这一层。
	return &aliyun.CustomDomain{
		AccountID:      f.accountID,
		DomainName:     d.DomainName,
		Protocol:       d.Protocol,
		CertName:       d.CertName,
		CertificatePEM: string(d.CertPEM),
		Echo:           aliyun.DomainEcho{Payload: d.Echo},
	}, nil
}

func (f *FC3) UpdateCustomDomain(_ context.Context, domain string, in *aliyun.UpdateCustomDomainInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	f.updateCallsFor[domain]++
	if err := pop(&f.updateErrs); err != nil {
		return err
	}
	d, ok := f.domains[domain]
	if !ok {
		return ErrDomainNotFound
	}
	if err := validateUpdate(in); err != nil {
		return err
	}
	d.Protocol = in.Protocol
	switch {
	case in.ClearCert:
		d.CertName, d.CertPEM, d.KeyPEM = "", nil, nil
	case in.CertConfig != nil:
		d.CertName = in.CertConfig.CertName
		d.CertPEM = append([]byte(nil), in.CertConfig.CertPEM...)
		d.KeyPEM = append([]byte(nil), in.CertConfig.KeyPEM...)
	default:
		// 两个都没给 = 证书保持原样。这里刻意**不**像 Protocol 那样要求显式，
		// 二者看着对称，其实不是：读取路径会丢弃私钥（customDomainFromSDK 的存在理由），
		// 所以调用方拿不到刚读回来的证书、无法把它原样回传。全量替换语义下「保持原样」
		// 因此根本无法表达——要求显式在这里是个不可实现的契约，而不是更严格的契约。
		// 何况 operator 的真实路径都不落进这一支：Apply 必定带 CertConfig，Unbind 策略下的
		// Cleanup 必定带 ClearCert，Orphan 策略下的 Cleanup 压根不调 Update。
		//
		// spec §12.3 #2：未实测（FC3 UpdateCustomDomain 是全量替换还是按字段合并语义未核实；
		// 若为全量替换，省略 certConfig 会清掉云上证书），实测结论见 test/integration/RESULTS.md
	}
	// 回填体照单全收：调用方漏带就等于把它清成 nil，测试因此能抓到 read-modify-write
	// 少读了一次的 bug。
	d.Echo = in.Echo.Payload
	f.domains[domain] = d

	if f.failAfterCommit != nil {
		err := f.failAfterCommit
		f.failAfterCommit = nil
		return err
	}
	return nil
}

// validateUpdate 校验写入体。不持锁，只看 in，由调用方在锁内调用。
//
// 空 Protocol 被拒绝，而不是「保持原样」。理由：updateInputToSDK 在 in.Protocol == ""
// 时压根不往请求体里写 protocol（SDK 结构体的 tag 带 omitempty），而 FC3 的
// UpdateCustomDomain 是全量替换还是部分合并没有实测（spec §12.3 #2）——合并语义下字段缺席
// 等于「保持原样」，全量替换语义下等于「把 protocol 清空」。fake 无权在这里替云做决定：
// 猜「保持原样」会让下游写出只在合并语义下正确的 provider，而这个 bug 直到打到真云上
// 才会暴露；猜「清空」又是在断言一个同样没实测的语义。于是选第三条：拒绝，把契约钉成
// 「调用方必须显式给出 Protocol」——显式值在两种语义下含义相同，是未实测前唯一安全的用法。
// 需要沿用云上现值时，从 GetCustomDomain 返回的 CustomDomain.Protocol 原样带回来即可。
func validateUpdate(in *aliyun.UpdateCustomDomainInput) error {
	switch {
	case in == nil:
		return updateRejected("NilInput", "写入体为空")
	case in.Protocol == "":
		return updateRejected("ProtocolRequired", "写入体必须显式给出 Protocol")
	}
	return nil
}

// updateRejected 构造一个 Update 路径上的永久错误。
func updateRejected(code, msg string) error {
	return &aliyun.Error{
		Class: aliyun.ClassPermanent, Op: aliyun.ActionUpdateCustomDomain,
		Code: code, Err: errors.New(msg),
	}
}

var _ aliyun.FC3Client = (*FC3)(nil)
