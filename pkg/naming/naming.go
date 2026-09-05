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
//
// 名字在云上原样保存、不被归一化（实测，见 sanitize 上方注释），所以 findByName 按名字
// 精确比对是成立的。
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

// sanitize 把 CR 名里除字母、数字、下划线之外的字符一律换成 `_`。
//
// 实测（2026-09-05, cn-hangzhou；spec §12.3 #4，依据 test/integration/RESULTS.md）：
// CAS 的 UploadUserCertificate **接受** `-` 与 `.`，并且**原样保存、不做归一化**
// （回查列举里的名字与提交值逐字相同）。也就是说这条规则比 CAS 实际的字符集更严。
//
// **仍然保持偏严，这是有意为之**（Ruling P3-R34 允许据 #4 放宽，评估后决定不放）：
//   - 官方文档只承诺「字母、数字、下划线」。实测覆盖的是 cn-hangzhou 一个 region、
//     一个 API 版本的当前行为；换 region、换版本或云侧日后收紧校验时，偏严的一侧
//     不会因此变红，偏宽的一侧会。
//   - 放宽换不到任何功能收益。这个名字是机器生成的（CR 名 + 指纹后缀），没人手输，
//     `-` 与 `.` 只影响它在 CAS 控制台里好不好看。
//   - 放宽反而有一次性代价：k8s 对象名多半是 RFC 1123 形状、带 `-`，放宽会让几乎每个
//     CR 下一代的 CASName 都跟历史代次不同名，控制台上同一个 CR 的名字在升级前后断成
//     两截；而 status.current.casName 里存的旧名字与当前代码重算出来的也对不上，
//     人工核对时会平白多一个疑点。
//
// 已确认放宽不会伤到认领路径（这是 Ruling P3-R34 要求的前置检查）：findByName
// （internal/controller/upload.go）比对的是 status.pendingUpload.casName——那是写
// write-ahead 记录时落盘的名字，上传用的也是同一个字段，两者恒等，重算的名字不参与
// 比对；回收与 finalizer 主清理走 certId 直删，更不看名字。所以「不放宽」是取舍，
// 不是被这条比对逼出来的。
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
