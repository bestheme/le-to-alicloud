/*
Copyright 2026 Hangzhou Yunqi Intelligence Technology Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func acWithNames(names ...string) *certsv1alpha1.AliyunCertificate {
	return &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Spec:       certsv1alpha1.AliyunCertificateSpec{CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: names}},
	}
}

func certWithIssuing(issuing bool) *cmapi.Certificate {
	c := &cmapi.Certificate{}
	st := cmmeta.ConditionFalse
	if issuing {
		st = cmmeta.ConditionTrue
	}
	c.Status.Conditions = []cmapi.CertificateCondition{{Type: cmapi.CertificateConditionIssuing, Status: st}}
	return c
}

func TestValidateMaterial_OK(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	if me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(false)); me != nil {
		t.Fatalf("应通过，得到 %+v", me)
	}
}

func TestValidateMaterial_SANsMismatch(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	me := validateMaterial(b, acWithNames("api.example.com", "www.example.com"), certWithIssuing(false))
	if me == nil || me.Reason != certsv1alpha1.ReasonSANsMismatch {
		t.Fatalf("want SANsMismatch, got %+v", me)
	}
}

func TestValidateMaterial_TemporaryCertRejected(t *testing.T) {
	crt, key := testutil.SelfSigned(t, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(true))
	if me == nil || me.Reason != certsv1alpha1.ReasonSelfSignedDuringIssuance {
		t.Fatalf("自签 + Issuing=True 必须拒绝, got %+v", me)
	}
}

func TestValidateMaterial_SelfSignedIssuerAccepted(t *testing.T) {
	crt, key := testutil.SelfSigned(t, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	if me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(false)); me != nil {
		t.Fatalf("自签 + Issuing=False（SelfSigned issuer 正式证书）应接受, got %+v", me)
	}
}

func TestValidateMaterial_WildcardCoversRequired(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "*.example.com")
	b, _ := pki.ParseBundle(crt, key)
	ac := acWithNames("*.example.com")
	ac.Spec.CertificateTemplate.CommonName = "api.example.com"
	if me := validateMaterial(b, ac, certWithIssuing(false)); me != nil {
		t.Fatalf("通配符应覆盖 commonName, got %+v", me)
	}
}
