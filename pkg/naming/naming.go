// Package naming 生成阿里云侧的名字与幂等令牌。
package naming

import (
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	maxCASNameLen = 63
	prefixBudget  = 50
	fpSuffixLen   = 12
	tokenUIDLen   = 16
	tokenFPLen    = 32
)

// CASName 生成 CAS 证书名：sanitize(crName)[:50] + "_" + fingerprint[:12]。
// CAS 文档只承诺接受字母、数字、下划线，且账号内唯一、≤ 63 字符；
// 指纹后缀保证同一 CR 的每一代名字不同、同一代重试名字相同。
func CASName(crName, fingerprint string) string {
	prefix := sanitize(crName)
	if len(prefix) > prefixBudget {
		prefix = prefix[:prefixBudget]
	}
	prefix = strings.TrimRight(prefix, "_")
	if prefix == "" {
		prefix = "cert"
	}
	suffix := fingerprint
	if len(suffix) > fpSuffixLen {
		suffix = suffix[:fpSuffixLen]
	}
	name := prefix + "_" + suffix
	if len(name) > maxCASNameLen {
		name = name[:maxCASNameLen]
	}
	return name
}

func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ClientToken 生成 CAS 幂等令牌：uid 去连字符前 16 位 + fingerprint 前 32 位，共 48 个 hex 字符。
// 同一 CR 的同一代证书无论重试多少次 token 都相同。
func ClientToken(uid types.UID, fingerprint string) string {
	u := strings.ReplaceAll(string(uid), "-", "")
	if len(u) > tokenUIDLen {
		u = u[:tokenUIDLen]
	}
	f := fingerprint
	if len(f) > tokenFPLen {
		f = f[:tokenFPLen]
	}
	return u + f
}
