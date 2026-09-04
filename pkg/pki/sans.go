package pki

import (
	"sort"
	"strings"
)

// Covers 按 RFC 6125 §6.4.3 判断证书中的 pattern 是否覆盖 host：
//   - 大小写不敏感，忽略尾部 "."
//   - 通配符只允许作为最左标签且必须是整个标签（"*.example.com"）
//   - 通配符恰好匹配一个标签，且 pattern 至少要有三个标签（不允许 "*.com"）
func Covers(pattern, host string) bool {
	p := strings.ToLower(strings.TrimSuffix(pattern, "."))
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if p == "" || h == "" {
		return false
	}
	if p == h {
		return true
	}
	if !strings.HasPrefix(p, "*.") {
		return false
	}
	pl := strings.Split(p, ".")
	hl := strings.Split(h, ".")
	if len(pl) < 3 || len(pl) != len(hl) {
		return false
	}
	for i := 1; i < len(pl); i++ {
		if pl[i] == "" || strings.Contains(pl[i], "*") || pl[i] != hl[i] {
			return false
		}
	}
	return hl[0] != ""
}

// Missing 返回 required 中未被任何 dnsNames 覆盖的域名（去重、排序）。
func Missing(dnsNames, required []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range required {
		key := strings.ToLower(strings.TrimSuffix(r, "."))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		covered := false
		for _, n := range dnsNames {
			if Covers(n, r) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}
