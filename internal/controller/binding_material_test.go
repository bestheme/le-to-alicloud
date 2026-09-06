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
	"bytes"
	"context"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

func bindingWithDomain(d string) *certsv1alpha1.AliyunCertificateBinding {
	return &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{
				Type:            certsv1alpha1.TargetTypeFC3CustomDomain,
				FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: d},
			},
		},
	}
}

func TestCheckDomainCoverage(t *testing.T) {
	cases := []struct {
		name    string
		sans    []string
		domain  string
		wantErr bool
	}{
		{"精确匹配", []string{"api.example.com"}, "api.example.com", false},
		{"通配符匹配", []string{"*.example.com"}, "api.example.com", false},
		{"通配符不跨层", []string{"*.example.com"}, "a.b.example.com", true},
		{"完全不覆盖", []string{"other.example.com"}, "api.example.com", true},
		{"大小写与尾点", []string{"API.Example.com."}, "api.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := provider.CertMaterial{DNSNames: c.sans}
			err := checkDomainCoverage(m, bindingWithDomain(c.domain))
			if c.wantErr != (err != nil) {
				t.Fatalf("覆盖判定错误: err=%v", err)
			}
			if err != nil && err.Reason != certsv1alpha1.ReasonDomainNotCovered {
				t.Errorf("reason 应是 DomainNotCovered: %s", err.Reason)
			}
		})
	}
}

func TestRequiredDomainsOf_UnknownTypeYieldsNothing(t *testing.T) {
	b := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{Type: "SomethingElse"},
		},
	}
	if got := requiredDomainsOf(b); len(got) != 0 {
		t.Errorf("未知 target 类型不该凭空造出域名: %v", got)
	}
}

// materialReader 用 fake client 拼一个只够读 Secret 的 reader。
func materialReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// materialCertificate 造一个只带 status.current 的证书 CR；名字固定，Secret 名随之定死。
func materialCertificate(cur *certsv1alpha1.CertificateGeneration) *certsv1alpha1.AliyunCertificate {
	return &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "c1"},
		Status:     certsv1alpha1.AliyunCertificateStatus{Current: cur},
	}
}

func tlsSecret(name string, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
}

// TestLoadBindingMaterial_SecretNotFound 覆盖绑定侧的 SecretNotFound 分支。
//
// 这条分支在 envtest 里够不到：证书 controller 也 watch Binding，Secret 一消失它就会把
// Issued 钉成 False/SecretNotFound，而绑定侧的 certIssued 闸门排在装载材料之前，最终态
// 必然是 CertificateNotReady（见 binding_material_envtest_test.go 的同名用例）。生产里
// 这条分支仍然活着——证书 controller 最长要等一个 ResyncInterval 才会注意到——所以它必须
// 有确定性的覆盖，只是覆盖点在这里而不是 envtest。
func TestLoadBindingMaterial_SecretNotFound(t *testing.T) {
	ac := materialCertificate(nil)
	_, me := loadBindingMaterial(context.Background(), materialReader(t), ac)
	if me == nil || me.Reason != certsv1alpha1.ReasonSecretNotFound {
		t.Fatalf("应报 SecretNotFound: %+v", me)
	}
	if !strings.Contains(me.Message, "c1-tls") {
		t.Errorf("message 应点名 Secret: %q", me.Message)
	}
}

// TestLoadBindingMaterial_SecretInvalid 钉住两件事：坏材料不放行，且 message 不含材料。
//
// 两个子用例走的是不同的失败点。第一个在证书解析处就停下，返回的是固定 sentinel，
// 没有任何插值——它证明不了脱敏。第二个才是真正的护栏：私钥 PEM 类型不认识时，
// pki 会把 **PEM 块头里的类型串** 插进错误里，那是 Secret 派生文本唯一一处进入 message
// 的地方。类型串本身不是秘密（"RSA PRIVATE KEY" 之类），但一旦有人把插值范围扩大到
// 块内容，这个用例会红。
func TestLoadBindingMaterial_SecretInvalid(t *testing.T) {
	const junk = "not a pem block at all"
	const secretBody = "U0VDUkVUS0VZQllURVM" //nolint:gosec // G101 误报：这是塞进一个故意不合法的 DSA PEM 块里的填充字节，不是凭证

	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "api.example.com")
	// 一个格式合法、但类型不被接受的私钥块：解析走到 parsePrivateKey 才失败。
	weirdKey := pem.EncodeToMemory(&pem.Block{Type: "DSA PRIVATE KEY", Bytes: []byte(secretBody)})

	cases := []struct {
		name    string
		certPEM []byte
		keyPEM  []byte
		leak    string
	}{
		{"证书就不是 PEM", []byte(junk), []byte(junk), junk},
		{"私钥 PEM 类型不认识", certPEM, weirdKey, secretBody},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := materialReader(t, tlsSecret("c1-tls", c.certPEM, c.keyPEM))
			_, me := loadBindingMaterial(context.Background(), r, materialCertificate(nil))
			if me == nil || me.Reason != certsv1alpha1.ReasonSecretInvalid {
				t.Fatalf("应报 SecretInvalid: %+v", me)
			}
			// 零凭证泄漏：错误只描述形状，绝不回显 Secret 里的任何字节。
			if strings.Contains(me.Message, c.leak) {
				t.Errorf("message 回显了 Secret 内容: %q", me.Message)
			}
		})
	}
}

// TestLoadBindingMaterial_IdentityFromStatusOnlyWhenFingerprintMatches 是这个函数最承重的
// 不变量：PEM 永远来自 Secret，而 certId / casName 只在指纹对得上时才从 status.current 取。
// 续期在途时两者短暂不一致，把上一代的 certId 贴到新证书上会让云侧指向一张错的证书。
func TestLoadBindingMaterial_IdentityFromStatusOnlyWhenFingerprintMatches(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "api.example.com")
	bundle, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	id := int64(4242)

	t.Run("指纹一致时继承 certId 与 casName", func(t *testing.T) {
		ac := materialCertificate(&certsv1alpha1.CertificateGeneration{
			Fingerprint: bundle.Fingerprint, CertID: &id, CASName: "inherited_name",
		})
		m, me := loadBindingMaterial(context.Background(),
			materialReader(t, tlsSecret("c1-tls", certPEM, keyPEM)), ac)
		if me != nil {
			t.Fatalf("不该失败: %+v", me)
		}
		if m.Fingerprint != bundle.Fingerprint {
			t.Errorf("指纹应来自 Secret: %s", m.Fingerprint)
		}
		if m.CertID == nil || *m.CertID != id {
			t.Errorf("certId 应继承自 status.current: %v", m.CertID)
		}
		if m.CASName != "inherited_name" {
			t.Errorf("casName 应继承自 status.current: %s", m.CASName)
		}
		if !bytes.Equal(m.CertPEM, bundle.CertPEM()) {
			t.Error("CertPEM 必须是 Secret 经 pki 规范化后的字节")
		}
		if len(m.KeyPEM) == 0 || m.NotAfter.IsZero() || len(m.DNSNames) == 0 {
			t.Error("KeyPEM / NotAfter / DNSNames 必须都从 Secret 派生出来")
		}
	})

	t.Run("指纹不一致时既不继承 certId 也不继承 casName", func(t *testing.T) {
		ac := materialCertificate(&certsv1alpha1.CertificateGeneration{
			Fingerprint: "stale", CertID: &id, CASName: "stale_name",
		})
		m, me := loadBindingMaterial(context.Background(),
			materialReader(t, tlsSecret("c1-tls", certPEM, keyPEM)), ac)
		if me != nil {
			t.Fatalf("不该失败: %+v", me)
		}
		if m.CertID != nil {
			t.Errorf("上一代的 certId 绝不能贴到新证书上: %v", *m.CertID)
		}
		if want := naming.CASName("c1", bundle.Fingerprint); m.CASName != want {
			t.Errorf("casName 应按 Secret 的指纹重新派生: got %s, want %s", m.CASName, want)
		}
	})
}

// TestCertificateGate 锁住闸门的判据：只有证书 CR 的 status.current 与 Secret 里的
// 这一代指纹相等时才放行。放宽任何一条都等于绕过证书 controller 的全套校验。
func TestCertificateGate(t *testing.T) {
	const fp = "abc123"
	cases := []struct {
		name string
		cur  *certsv1alpha1.CertificateGeneration
		want bool
	}{
		{"status.current 为空", nil, false},
		{"指纹不一致（续期在途）", &certsv1alpha1.CertificateGeneration{Fingerprint: "other"}, false},
		{"指纹一致", &certsv1alpha1.CertificateGeneration{Fingerprint: fp}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac := &certsv1alpha1.AliyunCertificate{
				Status: certsv1alpha1.AliyunCertificateStatus{Current: c.cur},
			}
			if got := certificateGate(provider.CertMaterial{Fingerprint: fp}, ac); got != c.want {
				t.Errorf("certificateGate = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAppliedLag 锁住 aliyuncert_binding_applied_age_seconds 的取值来源。
//
// 它必须 nil-safe：调用点被提到了所有早退之前，那里 ac 可能根本没取到。而「已跟上」
// 与「时钟回拨」两种情形都必须给 0，否则 spec §10.3 的告警式子会对健康证书误报。
func TestAppliedLag(t *testing.T) {
	const fp = "abc123"
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	uploaded := metav1.NewTime(now.Add(-90 * time.Second))
	future := metav1.NewTime(now.Add(time.Hour))

	gen := func(f string, at metav1.Time) *certsv1alpha1.AliyunCertificate {
		return &certsv1alpha1.AliyunCertificate{Status: certsv1alpha1.AliyunCertificateStatus{
			Current: &certsv1alpha1.CertificateGeneration{Fingerprint: f, UploadedAt: at},
		}}
	}

	cases := []struct {
		name    string
		ac      *certsv1alpha1.AliyunCertificate
		applied string
		want    time.Duration
	}{
		{"证书 CR 不存在", nil, "", 0},
		{"status.current 为空", &certsv1alpha1.AliyunCertificate{}, "", 0},
		{"指纹为空", gen("", uploaded), "", 0},
		{"已经跟上", gen(fp, uploaded), fp, 0},
		{"时钟回拨", gen(fp, future), "old", 0},
		// 刚建出来的 Binding：appliedFingerprint 还是空，而证书已经有一代了。
		// 这是生产里最常见的非零取值，也是 spec §10.3 的告警第一天会打在的形态。
		{"从未 applied 过", gen(fp, uploaded), "", 90 * time.Second},
		{"落后 90 秒", gen(fp, uploaded), "old", 90 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &AliyunCertificateBindingReconciler{Now: func() time.Time { return now }}
			b := &certsv1alpha1.AliyunCertificateBinding{
				Status: certsv1alpha1.AliyunCertificateBindingStatus{AppliedFingerprint: c.applied},
			}
			if got := r.appliedLag(b, c.ac); got != c.want {
				t.Errorf("appliedLag = %v, want %v", got, c.want)
			}
		})
	}
}
