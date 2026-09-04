package controller

import (
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

const (
	defaultIssuerKind  = "Issuer"
	defaultIssuerGroup = "cert-manager.io"
)

// IssuerDefaults 来自 --default-issuer-* flag，与 cert-manager 同名同语义。
type IssuerDefaults struct {
	Name  string
	Kind  string
	Group string
}

// Ref 把 flag 转成 IssuerReference；未配置 Name 时返回 nil。
func (d IssuerDefaults) Ref() *cmmeta.IssuerReference {
	if d.Name == "" {
		return nil
	}
	r := NormalizeIssuerRef(cmmeta.IssuerReference{Name: d.Name, Kind: d.Kind, Group: d.Group})
	return &r
}

// IssuerSource 记录 issuer 从哪一层解析出来，用于日志与 Diverged 判断。
type IssuerSource string

const (
	IssuerSourceSpec     IssuerSource = "spec"
	IssuerSourceStatus   IssuerSource = "status"
	IssuerSourceExisting IssuerSource = "existing"
	IssuerSourceDefault  IssuerSource = "default"
)

// NormalizeIssuerRef 补全 cert-manager 的缺省值，便于比较。
func NormalizeIssuerRef(r cmmeta.IssuerReference) cmmeta.IssuerReference {
	if r.Kind == "" {
		r.Kind = defaultIssuerKind
	}
	if r.Group == "" {
		r.Group = defaultIssuerGroup
	}
	return r
}

// IssuerRefEqual 在补全缺省值后比较。
func IssuerRefEqual(a, b cmmeta.IssuerReference) bool {
	a, b = NormalizeIssuerRef(a), NormalizeIssuerRef(b)
	return a.Name == b.Name && a.Kind == b.Kind && a.Group == b.Group
}

// ResolveIssuerRef 按 spec > status(Pin) > existing(bootstrap) > flag 的顺序解析。
//
// Pin 的关键在于：只要 status.effectiveIssuerRef 已存在，就以它为期望态，flag 不再参与——
// 否则改一次 flag 会让所有未显式指定 issuerRef 的证书同时重签。
// bootstrap 的关键在于：status 为空但 Certificate 已存在时，先采纳 Certificate 里的值，
// 防止「升级 operator 顺手改了 flag」触发全量重签。
func ResolveIssuerRef(
	tpl *certsv1alpha1.CertificateTemplate,
	status *certsv1alpha1.AliyunCertificateStatus,
	existing *cmapi.Certificate,
	defaults IssuerDefaults,
) (cmmeta.IssuerReference, IssuerSource, bool) {
	if tpl != nil && tpl.IssuerRef != nil && tpl.IssuerRef.Name != "" {
		return NormalizeIssuerRef(*tpl.IssuerRef), IssuerSourceSpec, true
	}
	if status != nil && status.EffectiveIssuerRef != nil && status.EffectiveIssuerRef.Name != "" {
		return NormalizeIssuerRef(*status.EffectiveIssuerRef), IssuerSourceStatus, true
	}
	if existing != nil && existing.Spec.IssuerRef.Name != "" {
		return NormalizeIssuerRef(existing.Spec.IssuerRef), IssuerSourceExisting, true
	}
	if d := defaults.Ref(); d != nil {
		return *d, IssuerSourceDefault, true
	}
	return cmmeta.IssuerReference{}, IssuerSourceDefault, false
}
