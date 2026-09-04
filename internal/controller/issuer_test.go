package controller

import (
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func ref(name, kind string) cmmeta.IssuerReference {
	return cmmeta.IssuerReference{Name: name, Kind: kind, Group: "cert-manager.io"}
}

func TestResolveIssuerRef_Precedence(t *testing.T) {
	defaults := IssuerDefaults{Name: "flag", Kind: "ClusterIssuer", Group: "cert-manager.io"}
	specRef := ref("spec", "ClusterIssuer")
	statusRef := ref("pinned", "ClusterIssuer")
	existing := &cmapi.Certificate{Spec: cmapi.CertificateSpec{IssuerRef: ref("existing", "Issuer")}}

	cases := []struct {
		name     string
		tpl      *certsv1alpha1.CertificateTemplate
		status   *certsv1alpha1.AliyunCertificateStatus
		existing *cmapi.Certificate
		defaults IssuerDefaults
		want     string
		source   IssuerSource
		ok       bool
	}{
		{"spec 优先", &certsv1alpha1.CertificateTemplate{IssuerRef: &specRef}, &certsv1alpha1.AliyunCertificateStatus{EffectiveIssuerRef: &statusRef}, existing, defaults, "spec", IssuerSourceSpec, true},
		{"status 其次", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{EffectiveIssuerRef: &statusRef}, existing, defaults, "pinned", IssuerSourceStatus, true},
		{"existing 再次", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, existing, defaults, "existing", IssuerSourceExisting, true},
		{"flag 兜底", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, nil, defaults, "flag", IssuerSourceDefault, true},
		{"全无", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, nil, IssuerDefaults{}, "", IssuerSourceDefault, false},
		{"existing 无 issuerRef 时跳到 flag", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, &cmapi.Certificate{}, defaults, "flag", IssuerSourceDefault, true},
	}
	for _, c := range cases {
		got, src, ok := ResolveIssuerRef(c.tpl, c.status, c.existing, c.defaults)
		if ok != c.ok || got.Name != c.want || src != c.source {
			t.Errorf("%s: got (%q,%s,%v) want (%q,%s,%v)", c.name, got.Name, src, ok, c.want, c.source, c.ok)
		}
	}
}

func TestResolveIssuerRef_NormalizesDefaults(t *testing.T) {
	tpl := &certsv1alpha1.CertificateTemplate{IssuerRef: &cmmeta.IssuerReference{Name: "x"}}
	got, _, _ := ResolveIssuerRef(tpl, &certsv1alpha1.AliyunCertificateStatus{}, nil, IssuerDefaults{})
	if got.Kind != "Issuer" || got.Group != "cert-manager.io" {
		t.Errorf("应补全 Kind/Group 默认值: %+v", got)
	}
}

func TestIssuerRefEqual(t *testing.T) {
	a := cmmeta.IssuerReference{Name: "x"}
	b := cmmeta.IssuerReference{Name: "x", Kind: "Issuer", Group: "cert-manager.io"}
	if !IssuerRefEqual(a, b) {
		t.Errorf("缺省值补全后应相等")
	}
	if IssuerRefEqual(a, cmmeta.IssuerReference{Name: "x", Kind: "ClusterIssuer"}) {
		t.Errorf("Kind 不同不应相等")
	}
}

func TestIssuerDefaults_Ref(t *testing.T) {
	if (IssuerDefaults{}).Ref() != nil {
		t.Errorf("空默认应返回 nil")
	}
	r := (IssuerDefaults{Name: "le"}).Ref()
	if r == nil || r.Kind != "Issuer" || r.Group != "cert-manager.io" {
		t.Errorf("应补全默认 Kind/Group: %+v", r)
	}
}

func TestResolveIssuerRef_TypedNilExisting(t *testing.T) {
	defaults := IssuerDefaults{Name: "flag"}
	var existing *cmapi.Certificate
	got, src, ok := ResolveIssuerRef(&certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, existing, defaults)
	if !ok || got.Name != "flag" || src != IssuerSourceDefault {
		t.Errorf("typed nil existing 应跳到 flag: got (%q,%s,%v)", got.Name, src, ok)
	}
}

func TestResolveIssuerRef_NilArgs(t *testing.T) {
	got, src, ok := ResolveIssuerRef(nil, nil, nil, IssuerDefaults{Name: "flag"})
	if !ok || got.Name != "flag" || src != IssuerSourceDefault {
		t.Errorf("全 nil 输入应回退到 flag: got (%q,%s,%v)", got.Name, src, ok)
	}
}

func TestResolveIssuerRef_EmptyNameSkipped(t *testing.T) {
	// tpl.IssuerRef 非 nil 但 Name 为空时，应跳到 status。
	empty := cmmeta.IssuerReference{Kind: "ClusterIssuer"}
	statusRef := ref("pinned", "ClusterIssuer")
	got, src, ok := ResolveIssuerRef(
		&certsv1alpha1.CertificateTemplate{IssuerRef: &empty},
		&certsv1alpha1.AliyunCertificateStatus{EffectiveIssuerRef: &statusRef},
		nil,
		IssuerDefaults{Name: "flag"},
	)
	if !ok || got.Name != "pinned" || src != IssuerSourceStatus {
		t.Errorf("空 Name 的 spec issuerRef 应被跳过: got (%q,%s,%v)", got.Name, src, ok)
	}
}
