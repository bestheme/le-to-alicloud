package main

import (
	"testing"
	"time"
)

func TestParseOperatorFlags_Defaults(t *testing.T) {
	o, err := parseOperatorFlags([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if o.ResyncInterval != time.Hour || o.DriftCheckInterval != time.Hour || o.CASProbeInterval != 12*time.Hour ||
		o.IssuanceStallThreshold != 6*time.Hour || o.CloudCallTimeout != 30*time.Second ||
		o.CleanupGracePeriod != 15*time.Minute || o.CleanupFailurePolicy != "Abandon" {
		t.Errorf("默认值不符: %+v", o)
	}
	if o.DefaultIssuerKind != "Issuer" || o.DefaultIssuerGroup != "cert-manager.io" {
		t.Errorf("issuer 默认 kind/group 应与 cert-manager 一致: %+v", o)
	}
}

func TestParseOperatorFlags_Values(t *testing.T) {
	o, err := parseOperatorFlags([]string{
		"--default-issuer-name=letsencrypt-prod", "--default-issuer-kind=ClusterIssuer",
		"--cleanup-failure-policy=Block", "--cloud-call-timeout=10s", "--watch-namespaces=a,b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultIssuerName != "letsencrypt-prod" || o.DefaultIssuerKind != "ClusterIssuer" || o.CleanupFailurePolicy != "Block" ||
		o.CloudCallTimeout != 10*time.Second || len(o.WatchNamespaces) != 2 {
		t.Errorf("解析不符: %+v", o)
	}
}

func TestParseOperatorFlags_RejectsBadPolicy(t *testing.T) {
	if _, err := parseOperatorFlags([]string{"--cleanup-failure-policy=Whatever"}); err == nil {
		t.Fatal("非法 policy 应报错")
	}
}
