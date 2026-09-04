package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/alibabacloud-go/tea/dara"
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
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Class: ClassRetryable, Op: op, Code: "NetTimeout", Err: err}
	}

	var sdkErr *dara.SDKError
	if errors.As(err, &sdkErr) {
		code := ""
		if sdkErr.Code != nil {
			code = *sdkErr.Code
		}
		status := 0
		if sdkErr.StatusCode != nil {
			status = *sdkErr.StatusCode
		}
		// 只保留 Code 与 StatusCode，丢弃 Message/Data 以免回显请求体
		safe := fmt.Errorf("sdk error code=%s status=%d", code, status)
		return &Error{Class: classifyCode(code, status), Op: op, Code: code, Err: safe}
	}

	return &Error{Class: ClassPermanent, Op: op, Code: "Unknown", Err: err}
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
