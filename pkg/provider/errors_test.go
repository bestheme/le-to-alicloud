package provider_test

import (
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

func TestErrorOf_UnwrapsWrapped(t *testing.T) {
	inner := errors.New("boom")
	pe := provider.Errorf(provider.CodeThrottled, true, "Throttled", inner)
	wrapped := errors.Join(errors.New("outer"), pe)

	got := provider.ErrorOf(wrapped)
	if got == nil {
		t.Fatal("包装后应仍能取出 ProviderError")
	}
	if got.Code != provider.CodeThrottled || !got.Retryable || got.Reason != "Throttled" {
		t.Errorf("字段丢失: %+v", got)
	}
	if !errors.Is(wrapped, inner) {
		t.Error("Unwrap 链断了")
	}
}

func TestErrorOf_PlainError(t *testing.T) {
	if provider.ErrorOf(errors.New("plain")) != nil {
		t.Error("非 ProviderError 应返回 nil")
	}
	if provider.ErrorOf(nil) != nil {
		t.Error("nil 应返回 nil")
	}
}

// 消息是唯一会被原样写进日志的字段（condition 的 message 是固定文案）。这里把一段
// 「响应体形状」的敏感片段注入被包住的 err，断言它不会经由 Error() 泄漏出来——只有
// Code / Retryable / Reason 这三个取值有界的字段允许出现。
func TestProviderError_MessageHasNoPayload(t *testing.T) {
	//nolint:gosec // G101 误报：一段伪造的「像私钥」的探针文本，用例断言它不会经 Error() 泄漏
	const payload = "-----BEGIN RSA PRIVATE KEY-----MIIEowIBAAKCAQEA-----END RSA PRIVATE KEY-----"
	pe := provider.Errorf(provider.CodeAuth, false, "CredentialsInvalid", errors.New("upstream: "+payload))
	msg := pe.Error()
	if contains(msg, payload) || contains(msg, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("被包住的错误内容泄漏进了消息: %s", msg)
	}
	for _, want := range []string{provider.CodeAuth, "CredentialsInvalid"} {
		if !contains(msg, want) {
			t.Errorf("消息里应含 %q: %s", want, msg)
		}
	}
	// 内容本身没有丢，只是不进消息：需要完整上下文的地方走 Unwrap。
	if !contains(pe.Unwrap().Error(), payload) {
		t.Error("Unwrap 应保留原始错误")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
