package fc3

import "strings"

// FC3 的 protocol 字段取值（spec §2.1）。
const (
	protocolHTTP  = "HTTP"
	protocolHTTPS = "HTTPS"
	protocolBoth  = "HTTP,HTTPS"
)

// splitProtocol 把 "HTTP , HTTPS" 拆成大写的集合，容忍空白与大小写。
func splitProtocol(p string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(p, ",") {
		if s := strings.ToUpper(strings.TrimSpace(part)); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

func protocolHasHTTPS(p string) bool {
	_, ok := splitProtocol(p)[protocolHTTPS]
	return ok
}

// isHTTPSOnly 判断域名是否只开了 HTTPS。这样的域名一旦被解绑证书就彻底不可访问，
// 所以解绑时必须同时降到 HTTP。
func isHTTPSOnly(p string) bool {
	set := splitProtocol(p)
	_, https := set[protocolHTTPS]
	_, http := set[protocolHTTP]
	return https && !http
}
