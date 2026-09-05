package provider_test

import (
	"context"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

type stubProvider struct{ name string }

func (s *stubProvider) Name() string                        { return s.name }
func (s *stubProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (s *stubProvider) Observe(context.Context, provider.Target, provider.Client) (provider.ObservedState, error) {
	return provider.ObservedState{}, nil
}

// 签名与 Provider 接口一致，只是折行以满足 lll 的 120 列上限。
func (s *stubProvider) Apply(
	context.Context, provider.Target, provider.Client, provider.CertMaterial, provider.ApplyOptions,
) error {
	return nil
}

func (s *stubProvider) Cleanup(context.Context, provider.Target, provider.Client, provider.DeletionPolicy) error {
	return nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	// 注册表是包级全局：不复位，同一进程里的第二遍（go test -count=2）必然撞重名 panic。
	t.Cleanup(provider.ResetForTest)
	provider.Register(&stubProvider{name: "StubTarget"})

	got, ok := provider.Get("StubTarget")
	if !ok {
		t.Fatal("注册后应能取回")
	}
	if got.Name() != "StubTarget" {
		t.Errorf("取回了别的 provider: %s", got.Name())
	}
	if _, ok := provider.Get("NoSuchTarget"); ok {
		t.Error("未注册的类型不该命中")
	}

	found := false
	for _, n := range provider.Names() {
		if n == "StubTarget" {
			found = true
		}
	}
	if !found {
		t.Errorf("Names 里应含 StubTarget: %v", provider.Names())
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	t.Cleanup(provider.ResetForTest)
	// 第一次注册**必须成功**，而且必须落在 recover 的作用域之外：把两次调用罩在同一个
	// defer/recover 下，第一次注册自己 panic（上一轮跑剩的同名条目）时用例照样通过——
	// 捕获到的是错误的那个 panic，这个用例就变成了永远绿的空壳。
	provider.Register(&stubProvider{name: "DupTarget"})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("重复注册应 panic —— 这是编译期就该发现的接线错误")
			}
		}()
		provider.Register(&stubProvider{name: "DupTarget"})
	}()
}
