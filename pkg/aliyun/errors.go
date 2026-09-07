package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/alibabacloud-go/tea/dara"
	tea "github.com/alibabacloud-go/tea/tea"
)

// ErrClass 决定 controller 如何处置错误。
type ErrClass int

const (
	ClassPermanent ErrClass = iota // 不重试，置 condition，等 spec / 环境变化
	ClassRetryable                 // 指数退避重试
	ClassAuth                      // 凭证 / 权限问题，长 requeue
	ClassNotFound                  // 资源不存在（Delete 时视为成功）
)

func (c ErrClass) String() string {
	switch c {
	case ClassRetryable:
		return "Retryable"
	case ClassAuth:
		return "Auth"
	case ClassNotFound:
		return "NotFound"
	default:
		return "Permanent"
	}
}

// CodeEmptyResponse 是「OpenAPI 调用本身成功、但响应体缺了必需字段」的错误码。
// 三个 client 共用它，goconst 也因此不再把这个字面量报成待抽取的常量。
const CodeEmptyResponse = "EmptyResponse"

// Error 是分类过的阿里云错误。绝不携带 request / response body。
type Error struct {
	Class ErrClass
	Op    string
	Code  string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("aliyun %s: %s (%s): %v", e.Op, e.Code, e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf 返回错误分类；非本包错误一律 Permanent。
func ClassOf(err error) ErrClass {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassPermanent
}

// Classify 把 SDK / 网络错误包装成 *Error。nil 原样返回。
func Classify(op string, err error) error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return err
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Class: ClassRetryable, Op: op, Code: "Timeout", Err: err}
	}
	// 传输层错误一律 Retryable。超时之外还有 connection refused、DNS 解析失败、
	// connection reset by peer——它们的 Timeout() 都返回 false，但按定义同样是瞬时故障。
	// 把它们丢进下面的 Unknown（=Permanent）分支，一次网络抖动就要等满一个 resync
	// 周期才重试，而整个 operator 的重试故事都建立在 ClassRetryable 上。
	// 真正的永久失败带着服务端 Code，走的是下面的 SDKError 分支，不会落到这里。
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return &Error{Class: ClassRetryable, Op: op, Code: "NetTimeout", Err: err}
		}
		return &Error{Class: ClassRetryable, Op: op, Code: "NetError", Err: err}
	}

	// dara.SDKError 是 SDK 内层的错误类型。
	var daraErr *dara.SDKError
	if errors.As(err, &daraErr) {
		return fromSDKError(op, daraErr.Code, daraErr.StatusCode)
	}

	// tea.SDKError 是 darabonba-openapi 实际返回给调用方的类型：client.go 在
	// DisableSDKError 未设置时对每个错误调用 dara.TeaSDKError()，把 *dara.SDKError
	// 转成独立的 *tea.SDKError。真实 CAS 调用走的是这一条分支。
	var teaErr *tea.SDKError
	if errors.As(err, &teaErr) {
		return fromSDKError(op, teaErr.Code, teaErr.StatusCode)
	}

	return &Error{Class: ClassPermanent, Op: op, Code: "Unknown", Err: err}
}

// fromSDKError 由 SDK 错误的 Code / StatusCode 构造分类错误。
// 只保留这两项，丢弃 Message/Data/Detail 以免把请求体回显进日志。
func fromSDKError(op string, codePtr *string, statusPtr *int) error {
	code := ""
	if codePtr != nil {
		code = *codePtr
	}
	status := 0
	if statusPtr != nil {
		status = *statusPtr
	}
	safe := fmt.Errorf("sdk error code=%s status=%d", code, status)
	return &Error{Class: classifyCode(code, status), Op: op, Code: code, Err: safe}
}

func classifyCode(code string, status int) ErrClass {
	switch {
	case strings.HasPrefix(code, "Throttling"),
		code == "ServiceUnavailable",
		code == "InternalError",
		strings.HasPrefix(code, "ServiceUnavailable"),
		status >= 500:
		return ClassRetryable
	case strings.HasPrefix(code, "InvalidAccessKeyId"),
		code == "SignatureDoesNotMatch",
		strings.HasPrefix(code, "Forbidden"),
		strings.HasPrefix(code, "NoPermission"),
		code == "InvalidSecurityToken.Expired",
		status == 401, status == 403:
		return ClassAuth
	case strings.Contains(code, "NotExist"),
		strings.Contains(code, "NotFound"),
		status == 404:
		return ClassNotFound
	default:
		return ClassPermanent
	}
}
