package fake_test

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func newFC3WithDomain() *fake.FC3 {
	f := fake.NewFC3()
	f.SetAccountID("1234567890")
	f.AddDomain(fake.Domain{
		DomainName: "api.example.com",
		Protocol:   "HTTP",
		Echo:       "route-table-marker",
	})
	return f
}

func TestFakeFC3_GetReturnsAccountAndEchoButNoKey(t *testing.T) {
	f := newFC3WithDomain()
	f.AddDomain(fake.Domain{
		DomainName: "tls.example.com", Protocol: "HTTPS",
		CertName: "c1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET"), Echo: "e2",
	})

	cd, err := f.GetCustomDomain(context.Background(), "tls.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cd.AccountID != "1234567890" || cd.Protocol != "HTTPS" || cd.CertName != "c1" {
		t.Fatalf("字段不对: %+v", cd)
	}
	if cd.CertificatePEM != "PUB" {
		t.Errorf("应返回公开证书: %q", cd.CertificatePEM)
	}
	if cd.Echo.Payload != "e2" {
		t.Errorf("Echo 应原样返回: %v", cd.Echo.Payload)
	}
	if f.GetCalls() != 1 {
		t.Errorf("GetCalls=%d", f.GetCalls())
	}
}

func TestFakeFC3_GetMissingDomain(t *testing.T) {
	f := newFC3WithDomain()
	_, err := f.GetCustomDomain(context.Background(), "nope.example.com")
	if aliyun.ClassOf(err) != aliyun.ClassNotFound {
		t.Fatalf("不存在的域名应是 ClassNotFound，得到 %v (%v)", aliyun.ClassOf(err), err)
	}
}

func TestFakeFC3_UpdateAppliesCertAndKeepsEcho(t *testing.T) {
	f := newFC3WithDomain()
	err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET")},
		Echo:       aliyun.DomainEcho{Payload: "route-table-marker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := f.Domain("api.example.com")
	if !ok {
		t.Fatal("域名不见了")
	}
	if d.Protocol != "HTTP,HTTPS" || d.CertName != "n1" || string(d.KeyPEM) != "SECRET" {
		t.Fatalf("写入未生效: %+v", d)
	}
	// read-modify-write 的核心断言：调用方必须把回填体原样带回来，否则路由表就没了。
	if d.Echo != "route-table-marker" {
		t.Errorf("回填体丢失: %v", d.Echo)
	}
	if f.UpdateCalls() != 1 {
		t.Errorf("UpdateCalls=%d", f.UpdateCalls())
	}
}

func TestFakeFC3_UpdateClearCert(t *testing.T) {
	f := newFC3WithDomain()
	f.AddDomain(fake.Domain{
		DomainName: "tls.example.com", Protocol: "HTTPS",
		CertName: "c1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET"),
	})
	err := f.UpdateCustomDomain(context.Background(), "tls.example.com",
		&aliyun.UpdateCustomDomainInput{Protocol: "HTTP", ClearCert: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain("tls.example.com")
	if d.CertName != "" || len(d.CertPEM) != 0 || len(d.KeyPEM) != 0 {
		t.Fatalf("证书应被清空: %+v", d)
	}
	if d.Protocol != "HTTP" {
		t.Errorf("纯 HTTPS 域名解绑后应降为 HTTP: %s", d.Protocol)
	}
}

func TestFakeFC3_QueuedErrors(t *testing.T) {
	f := newFC3WithDomain()
	boom := errors.New("boom")
	f.QueueGetErr(boom)
	if _, err := f.GetCustomDomain(context.Background(), "api.example.com"); !errors.Is(err, boom) {
		t.Fatalf("排队的错误应被返回，得到 %v", err)
	}
	if _, err := f.GetCustomDomain(context.Background(), "api.example.com"); err != nil {
		t.Fatalf("队列耗尽后应恢复正常: %v", err)
	}

	f.QueueUpdateErr(boom)
	err := f.UpdateCustomDomain(context.Background(), "api.example.com",
		&aliyun.UpdateCustomDomainInput{Protocol: "HTTP"})
	if !errors.Is(err, boom) {
		t.Fatalf("排队的错误应被返回，得到 %v", err)
	}
}

func TestFakeFC3_FailNextUpdateAfterCommit(t *testing.T) {
	f := newFC3WithDomain()
	lost := errors.New("response lost")
	f.FailNextUpdateAfterCommit(lost)

	err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET")},
	})
	if !errors.Is(err, lost) {
		t.Fatalf("应返回注入的错误: %v", err)
	}
	// 服务端已经改了，只是调用方不知道。这正是重试必须幂等的理由。
	d, _ := f.Domain("api.example.com")
	if d.CertName != "n1" {
		t.Fatal("提交后失败：写入必须已经生效")
	}
}

// TestFakeFC3_UpdateRequiresProtocol 锁定「写入体必须显式带 Protocol」这条契约。
//
// 真实 FC3 的 UpdateCustomDomain 是全量替换还是部分合并没有实测（spec §12.3），而
// updateInputToSDK 在 in.Protocol == "" 时根本不往请求体里写 protocol（字段带
// omitempty）。合并语义下这等于「保持原样」，全量替换语义下这等于「把 protocol 清空」——
// 两种结果南辕北辙。fake 因此拒绝这种写入体，逼调用方永远显式给出 Protocol：
// 显式值在两种语义下含义相同，是唯一在未实测前也安全的用法。
func TestFakeFC3_UpdateRequiresProtocol(t *testing.T) {
	f := newFC3WithDomain()
	err := f.UpdateCustomDomain(context.Background(), "api.example.com",
		&aliyun.UpdateCustomDomainInput{ClearCert: true})
	if aliyun.ClassOf(err) != aliyun.ClassPermanent {
		t.Fatalf("缺少 Protocol 应是 ClassPermanent，得到 %v (%v)", aliyun.ClassOf(err), err)
	}
	// 拒绝必须发生在写入之前：服务端状态不能被半成品写入体动过。
	d, _ := f.Domain("api.example.com")
	if d.Protocol != "HTTP" || d.Echo != "route-table-marker" {
		t.Fatalf("被拒绝的写入不应改动服务端状态: %+v", d)
	}
}

// TestFakeFC3_UpdateMissingDomain 覆盖「域名被别人删了」：Update 也要报 NotFound。
func TestFakeFC3_UpdateMissingDomain(t *testing.T) {
	f := newFC3WithDomain()
	f.RemoveDomain("api.example.com")
	err := f.UpdateCustomDomain(context.Background(), "api.example.com",
		&aliyun.UpdateCustomDomainInput{Protocol: "HTTP"})
	if aliyun.ClassOf(err) != aliyun.ClassNotFound {
		t.Fatalf("域名不存在应是 ClassNotFound，得到 %v (%v)", aliyun.ClassOf(err), err)
	}
	if !errors.Is(err, fake.ErrDomainNotFound) {
		t.Errorf("应是 ErrDomainNotFound 这个哨兵值: %v", err)
	}
}

// TestFakeFC3_UpdateCopiesCertBytes 确认写入体的 []byte 被复制而不是共享底层数组：
// 调用方复用缓冲区时，fake 里的快照不能跟着变。
func TestFakeFC3_UpdateCopiesCertBytes(t *testing.T) {
	f := newFC3WithDomain()
	cert := []byte("PUB")
	key := []byte("SECRET")
	err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n1", CertPEM: cert, KeyPEM: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	cert[0], key[0] = 'X', 'X'
	d, _ := f.Domain("api.example.com")
	if string(d.CertPEM) != "PUB" || string(d.KeyPEM) != "SECRET" {
		t.Fatalf("fake 与调用方共享了底层数组: %+v", d)
	}
}

// TestFakeFC3_NilInput 确认空写入体被当作永久错误，而不是 panic。
func TestFakeFC3_NilInput(t *testing.T) {
	f := newFC3WithDomain()
	if err := f.UpdateCustomDomain(context.Background(), "api.example.com", nil); err == nil {
		t.Fatal("空写入体应报错")
	} else if aliyun.ClassOf(err) != aliyun.ClassPermanent {
		t.Fatalf("空写入体应是 ClassPermanent，得到 %v", aliyun.ClassOf(err))
	}
}

// TestFakeFC3_ConcurrentAccess 在 -race 下确认计数器与操作不会互相撕裂。
func TestFakeFC3_ConcurrentAccess(t *testing.T) {
	f := newFC3WithDomain()
	const n = 20
	done := make(chan struct{}, n*2)
	for i := 0; i < n; i++ {
		go func() {
			_, _ = f.GetCustomDomain(context.Background(), "api.example.com")
			_ = f.GetCalls()
			done <- struct{}{}
		}()
		go func() {
			_ = f.UpdateCustomDomain(context.Background(), "api.example.com",
				&aliyun.UpdateCustomDomainInput{Protocol: "HTTP"})
			_, _ = f.Domain("api.example.com")
			done <- struct{}{}
		}()
	}
	for i := 0; i < n*2; i++ {
		<-done
	}
	if f.GetCalls() != n || f.UpdateCalls() != n {
		t.Fatalf("计数丢失: get=%d update=%d", f.GetCalls(), f.UpdateCalls())
	}
}
