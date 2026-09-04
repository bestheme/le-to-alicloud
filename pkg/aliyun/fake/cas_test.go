package fake_test

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func TestFake_TokenIdempotent(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	id1, err := f.Upload(ctx, "a_1", []byte("c"), []byte("k"), "tok")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.Upload(ctx, "a_1", []byte("c"), []byte("k"), "tok")
	if err != nil || id1 != id2 {
		t.Fatalf("同 token 应返回同 ID: %d %d %v", id1, id2, err)
	}
	if len(f.Certs()) != 1 {
		t.Errorf("只应有 1 张证书")
	}
	if f.UploadCalls != 2 {
		t.Errorf("UploadCalls = %d, want 2", f.UploadCalls)
	}
	if !f.Has(id1) {
		t.Errorf("Has(%d) 应为 true", id1)
	}
}

func TestFake_DuplicateName(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	_, _ = f.Upload(ctx, "dup", nil, nil, "t1")
	_, err := f.Upload(ctx, "dup", nil, nil, "t2")
	if !errors.Is(err, fake.ErrDuplicateName) {
		t.Fatalf("want ErrDuplicateName, got %v", err)
	}
	if aliyun.ClassOf(err) != aliyun.ClassPermanent {
		t.Errorf("同名拒绝应为 Permanent，得到 %v", aliyun.ClassOf(err))
	}
	if len(f.Certs()) != 1 {
		t.Errorf("同名上传不应写入第二张")
	}
}

func TestFake_FailAfterCommit(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	f.FailNextUploadAfterCommit(&aliyun.Error{Class: aliyun.ClassRetryable, Code: "Timeout"})
	_, err := f.Upload(ctx, "x", nil, nil, "tok")
	if err == nil {
		t.Fatal("首次应返回错误")
	}
	if len(f.Certs()) != 1 {
		t.Fatal("服务端应已写入")
	}
	id, err := f.Upload(ctx, "x", nil, nil, "tok")
	if err != nil || id != f.Certs()[0].ID {
		t.Fatalf("重试应命中 token 返回同一 ID: %d %v", id, err)
	}
	if len(f.Certs()) != 1 {
		t.Errorf("重试不应产生第二张证书")
	}
}

func TestFake_DeleteNotFound(t *testing.T) {
	f := fake.NewCAS()
	err := f.Delete(context.Background(), 42, "")
	if aliyun.ClassOf(err) != aliyun.ClassNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestFake_DeleteRemoves(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	id, err := f.Upload(ctx, "gone", nil, nil, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, id, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if f.Has(id) {
		t.Errorf("删除后 Has 应为 false")
	}
	if f.DeleteCalls != 1 {
		t.Errorf("DeleteCalls = %d, want 1", f.DeleteCalls)
	}
}

func TestFake_QueuedErrors(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	boom := errors.New("boom")

	f.QueueUploadErr(boom)
	if _, err := f.Upload(ctx, "n", nil, nil, "t"); !errors.Is(err, boom) {
		t.Errorf("Upload 应返回排队错误，得到 %v", err)
	}
	if len(f.Certs()) != 0 {
		t.Errorf("排队错误应在写入前返回")
	}
	// 队列耗尽后恢复正常
	if _, err := f.Upload(ctx, "n", nil, nil, "t"); err != nil {
		t.Errorf("队列耗尽后应成功，得到 %v", err)
	}

	f.QueueDeleteErr(boom)
	if err := f.Delete(ctx, 1, ""); !errors.Is(err, boom) {
		t.Errorf("Delete 应返回排队错误，得到 %v", err)
	}

	f.QueueFindErr(boom)
	if _, err := f.FindUploaded(ctx, ""); !errors.Is(err, boom) {
		t.Errorf("FindUploaded 应返回排队错误，得到 %v", err)
	}
}

func TestFake_FindUploaded(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	if _, err := f.Upload(ctx, "example_com-abc", nil, nil, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Upload(ctx, "other_net-xyz", nil, nil, "t2"); err != nil {
		t.Fatal(err)
	}

	all, err := f.FindUploaded(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("空 hint 应返回全部，得到 %d", len(all))
	}

	hit, err := f.FindUploaded(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || hit[0].Name != "example_com-abc" {
		t.Errorf("按域名应只命中一张，得到 %v", hit)
	}

	if f.FindCalls != 2 {
		t.Errorf("FindCalls = %d, want 2", f.FindCalls)
	}
}
