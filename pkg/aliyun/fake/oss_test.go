package fake

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func TestOSS_RoundTrip(t *testing.T) {
	ctx := context.Background()
	f := NewOSS()
	f.SetOwner("1234567890")
	f.AddCname("b1", CnameRecord{Domain: "www.example.com"})

	c, err := f.GetCname(ctx, "b1", "WWW.example.com")
	if err != nil || c.CertRef != "" || c.AccountID != "1234567890" {
		t.Fatalf("空证书 CNAME 观测不对: %+v %v", c, err)
	}
	if err := f.PutCnameCert(ctx, "b1", "www.example.com", "1-cn-hangzhou"); err != nil {
		t.Fatal(err)
	}
	c, _ = f.GetCname(ctx, "b1", "www.example.com")
	if c.CertRef != "1-cn-hangzhou" || c.CertType != "CAS" {
		t.Fatalf("写入后应观测到 certRef: %+v", c)
	}
	if err := f.DeleteCnameCert(ctx, "b1", "www.example.com"); err != nil {
		t.Fatal(err)
	}
	c, _ = f.GetCname(ctx, "b1", "www.example.com")
	if c.CertRef != "" {
		t.Fatalf("摘证书后 certRef 应为空: %+v", c)
	}
	if f.ListCallsFor("b1", "www.example.com") != 3 || f.PutCallsFor("b1", "www.example.com") != 2 {
		t.Fatalf("计数不对: list=%d put=%d", f.ListCallsFor("b1", "www.example.com"), f.PutCallsFor("b1", "www.example.com"))
	}
}

func TestOSS_NotFoundAndInjection(t *testing.T) {
	ctx := context.Background()
	f := NewOSS()
	if _, err := f.GetCname(ctx, "nope", "d"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("bucket 不存在: %v", err)
	}
	f.AddBucket("b1")
	if _, err := f.GetCname(ctx, "b1", "d"); !errors.Is(err, ErrCnameNotFound) {
		t.Fatalf("CNAME 不存在: %v", err)
	}
	if err := f.PutCnameCert(ctx, "b1", "d", ""); aliyun.ClassOf(err) != aliyun.ClassPermanent {
		t.Fatalf("空 certRef 应为 Permanent: %v", err)
	}
	boom := &aliyun.Error{Class: aliyun.ClassAuth, Op: aliyun.ActionPutCname, Code: "AccessDenied", Err: errors.New("x")}
	f.AddCname("b1", CnameRecord{Domain: "d"})
	f.AlwaysPutErr(boom)
	if err := f.PutCnameCert(ctx, "b1", "d", "1-cn-hangzhou"); !errors.Is(err, boom) {
		t.Fatalf("AlwaysPutErr 应生效: %v", err)
	}
	if rec, _ := f.Cname("b1", "d"); rec.CertRef != "" {
		t.Fatal("失败的写入不该生效")
	}
}
