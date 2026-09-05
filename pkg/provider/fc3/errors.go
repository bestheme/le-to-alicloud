package fc3

import (
	"errors"
	"fmt"
	"strings"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// toProviderError 把 aliyun 的错误分类翻译成通用层认识的形状。
//
// 通用层不认识任何 SDK 错误——这正是 provider 抽象的意义。翻译只在这一处发生，
// Reason 用的是 v1alpha1 的常量，好让 condition 的取值集合仍然是有界的。
//
// failReason 是「说不出更具体的话时」写进 condition 的 reason，由调用方给：一次读失败
// 建议 ApplyFailed 是错的建议——它告诉用户我们尝试过写入，而此刻连「云上现在是什么」
// 都还没读到。三条路径因此各给各的：Observe → ObserveFailed，Apply → ApplyFailed，
// Cleanup → CleanupFailed。bool 表达不了这三选一。
//
// op 作为错误消息前缀（`op + "[" + code + "]: " + …`）：一个 ProviderError 只说
// 「Permanent」时分不清是 Get 还是 Update 挂了，加上前缀才有诊断价值——顺带让 unparam
// 不再把它报成未使用形参。阿里云错误码一并进前缀：ProviderError.Error() 刻意不回显被
// 包住的错误，没有 Code 的话这条日志就只剩「permanent failure」，说不出是哪个云侧错误。
// Code 的取值有界且已脱敏（pkg/aliyun 的 fromSDKError 只保留 code / status），消息里
// 因此绝不含任何 SDK 响应体。
//
// 分类完全交给 aliyun.classifyCode，不在这里按错误码字面量做推测式兜底：
// 「Code 含 DomainName 且 Permanent 就当 TargetNotFound」之类的规则会把
// InvalidDomainName 这种真·永久错误误判成「域名还没建」，然后每 5 分钟空转一次。
// 集成测试已量出真实错误码（spec §12.3 #14，2026-09-05；RESULTS.md #14）：
// DomainNameNotFound / HTTP 404，aliyun.ClassOf 归为 ClassNotFound，下面的 TargetNotFound
// 分支按预期走得到，classifyCode 无需修改。将来若又有 FC3 错误码该归 NotFound 而没被认出，
// 仍是回去修 aliyun.classifyCode（错误码归类的唯一落点），而不是在本文件加分支。
func toProviderError(op string, err error, failReason string) error {
	if err == nil {
		return nil
	}
	ae := asAliyunError(err)
	// withOp 只做前缀，不追加任何新内容；%w 保住 Unwrap 链，errors.As 仍能取到 *aliyun.Error。
	withOp := func(e error) error {
		if ae != nil {
			return fmt.Errorf("%s[%s]: %w", op, ae.Code, e)
		}
		return fmt.Errorf("%s: %w", op, e)
	}
	switch aliyun.ClassOf(err) {
	case aliyun.ClassNotFound:
		// 域名不存在不是瞬时故障：可能是 Terraform 还没建。Retryable=false，
		// 让通用层用固定 5m 的长 requeue 而不是指数退避（spec §6.2 步骤 5）。
		return withOp(provider.Errorf(provider.CodeTargetNotFound, false, certsv1alpha1.ReasonTargetNotFound, err))
	case aliyun.ClassAuth:
		return withOp(provider.Errorf(provider.CodeAuth, false, certsv1alpha1.ReasonCredentialsInvalid, err))
	case aliyun.ClassRetryable:
		if ae != nil && isThrottling(ae.Code) {
			return withOp(provider.Errorf(provider.CodeThrottled, true, certsv1alpha1.ReasonThrottled, err))
		}
		return withOp(provider.Errorf(provider.CodeRetryable, true, failReason, err))
	default:
		return withOp(provider.Errorf(provider.CodePermanent, false, failReason, err))
	}
}

func asAliyunError(err error) *aliyun.Error {
	var ae *aliyun.Error
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// isThrottling 沿用 CAS 的经验：阿里云的限流码都以 Throttling 开头。
//
// spec §12.3 #10：未实测（FC3 的限流阈值与限流错误码是否同样以 `Throttling` 开头未核实；
// 若不是，被限流会落进 CodeRetryable 走指数退避而不是 CodeThrottled，指标里也看不到
// throttled），实测结论见 test/integration/RESULTS.md
func isThrottling(code string) bool { return strings.HasPrefix(code, "Throttling") }

// swallowNotFound 把「目标已经不在了」翻译成成功，供 Cleanup 的两个调用点共用。
//
// 解绑要达到的是「云上不再挂着我们的证书」，域名整个消失同样满足这个目的——
// pkg/aliyun 给 ClassNotFound 写的定义本来就是「资源不存在（Delete 时视为成功）」。
//
// 必须同时盖住 Get 与 Update：域名可能恰好在这两次调用之间被 Terraform 删掉。漏掉
// Update 那一侧的代价不是多重试一次——CodeTargetNotFound 的 Retryable 是 false，通用层
// 会用固定的长 requeue 反复空转，finalizer 永远摘不掉，Binding 一直卡在 Terminating。
func swallowNotFound(err error) error {
	if pe := provider.ErrorOf(err); pe != nil && pe.Code == provider.CodeTargetNotFound {
		return nil
	}
	return err
}
