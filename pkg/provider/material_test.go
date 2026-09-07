package provider

import "testing"

func TestCertMaterial_CASCertRef(t *testing.T) {
	id := int64(27087165)
	cases := []struct {
		name string
		m    CertMaterial
		want string
	}{
		{"正常", CertMaterial{CertID: &id, CASRegion: "cn-hangzhou"}, "27087165-cn-hangzhou"},
		{"certId 为 nil", CertMaterial{CASRegion: "cn-hangzhou"}, ""},
		{"CAS 区域为空", CertMaterial{CertID: &id}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.CASCertRef(); got != tc.want {
				t.Errorf("CASCertRef() = %q, want %q", got, tc.want)
			}
		})
	}
}
