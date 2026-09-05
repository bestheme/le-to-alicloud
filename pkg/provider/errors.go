package provider

import (
	"errors"
	"fmt"
)

// 错误码。取值集合有界，可以安全地进日志与（经 Reason 转换后）进 condition。
const (
	CodeTargetNotFound = "TargetNotFound" // 目标资源不存在
	CodeAuth           = "Auth"           // 凭证 / 授权问题
	CodeThrottled      = "Throttled"      // 被限流
	CodeRetryable      = "Retryable"      // 其它瞬时故障
	CodePermanent      = "Permanent"      // 不重试就不会变好
	CodeInvalidClient  = "InvalidClient"  // 通用层传错了 client 类型（接线错误）
	CodeInvalidTarget  = "InvalidTarget"  // Target.Spec 断言失败（接线错误）
)

// ProviderError 是 provider 对失败的分类结论。
//
// Reason 是 provider 建议通用层写进 condition 的 reason 字符串（v1alpha1 的 Reason* 常量
// 之一）。让 provider 给出建议而不是让通用层猜：只有 provider 知道 404 到底意味着
// 「域名没建」还是「证书没绑」。
type ProviderError struct {
	Code      string
	Retryable bool
	Reason    string
	Err       error
}

// Error 只回显取值有界的三个字段。
//
// 被包住的 err 进 Unwrap 链、不进消息：它的文本由 SDK / 云侧决定，长度与内容都不
// 受我们控制，而这条消息会被原样写进日志（Global Constraints 的「私钥内容绝不出现在
// 日志、event、status、error message 中」）。需要完整上下文时用 errors.Unwrap。
func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider error %s (retryable=%t, reason=%s)", e.Code, e.Retryable, e.Reason)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// Errorf 构造 ProviderError。
func Errorf(code string, retryable bool, reason string, err error) *ProviderError {
	return &ProviderError{Code: code, Retryable: retryable, Reason: reason, Err: err}
}

// ErrorOf 从错误链里取出 ProviderError；不是这个类型时返回 nil。
func ErrorOf(err error) *ProviderError {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
