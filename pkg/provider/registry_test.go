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
	defer func() {
		if recover() == nil {
			t.Error("重复注册应 panic —— 这是编译期就该发现的接线错误")
		}
	}()
	provider.Register(&stubProvider{name: "DupTarget"})
	provider.Register(&stubProvider{name: "DupTarget"})
}
