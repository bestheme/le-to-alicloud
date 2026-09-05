package fc3

import "testing"

func TestProtocolPredicates(t *testing.T) {
	cases := []struct {
		in       string
		hasHTTPS bool
		only     bool
	}{
		{"HTTP", false, false},
		{"HTTPS", true, true},
		{"HTTP,HTTPS", true, false},
		{"https", true, true},
		{" HTTP , HTTPS ", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := protocolHasHTTPS(c.in); got != c.hasHTTPS {
			t.Errorf("protocolHasHTTPS(%q)=%t，期望 %t", c.in, got, c.hasHTTPS)
		}
		if got := isHTTPSOnly(c.in); got != c.only {
			t.Errorf("isHTTPSOnly(%q)=%t，期望 %t", c.in, got, c.only)
		}
	}
}
