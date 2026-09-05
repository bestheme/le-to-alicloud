//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain 是 RESULTS.md 的唯一写入点：跑完包内全部用例后把攒下的 findings 落盘。
// 它必须待在 _test.go 里——Go 的测试主函数只从测试文件中识别，放进 report.go 之类的
// 普通文件里既不会报错也不会被调用，报告会静默地永远不生成。
func TestMain(m *testing.M) {
	code := m.Run()
	if err := writeResults(); err != nil {
		fmt.Fprintf(os.Stderr, "写 RESULTS.md 失败: %v\n", err)
	}
	os.Exit(code)
}
