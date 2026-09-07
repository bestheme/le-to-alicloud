package aliyun_test

import (
	"context"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func TestLimiters_ListChannelIsSlow(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx, "ak1", aliyun.LimitCASList); err != nil {
			t.Fatal(err)
		}
	}
	// 8 QPS、burst 1：第 2、3 次各需等 ~125ms
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("3 次 list 调用应至少耗时 ~250ms，实际 %v", el)
	}
}

func TestLimiters_WriteChannelIsFast(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	start := time.Now()
	// 50 QPS、burst 10：前 10 次应立即通过
	for i := 0; i < 10; i++ {
		if err := l.Wait(ctx, "ak1", aliyun.LimitCASWrite); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("write 通道 burst 10 应立即通过，实际 %v", el)
	}
}

func TestLimiters_KindsAreIndependent(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	// 用掉 list 通道的唯一令牌，write 通道不应因此等待
	if err := l.Wait(ctx, "ak1", aliyun.LimitCASList); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := l.Wait(ctx, "ak1", aliyun.LimitCASWrite); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("不同 kind 不应互相等待，实际等待 %v", el)
	}
}

func TestLimiters_KeysAreIndependent(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	_ = l.Wait(ctx, "ak1", aliyun.LimitCASList)
	start := time.Now()
	if err := l.Wait(ctx, "ak2", aliyun.LimitCASList); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("不同 key 不应互相等待，实际等待 %v", el)
	}
}

func TestLimiters_RespectsContext(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_ = l.Wait(ctx, "ak", aliyun.LimitCASList)
	if err := l.Wait(ctx, "ak", aliyun.LimitCASList); err == nil {
		t.Errorf("ctx 超时后 Wait 应返回错误")
	}
}

func TestLimiters_OSSChannelIsSlowAndSeparate(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	// 5 QPS、burst 1：第 2 次要等 ~200ms
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := l.Wait(ctx, "ak1", aliyun.LimitOSS); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Errorf("2 次 OSS 调用应至少耗时 ~200ms，实际 %v", el)
	}
	// OSS 用光令牌后，cas-write 通道不应被拖慢——两者共用一个 limiter 的话这里会等 200ms
	start = time.Now()
	if err := l.Wait(ctx, "ak1", aliyun.LimitCASWrite); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("OSS 与 cas-write 应是独立通道，实际等待 %v", el)
	}
}
