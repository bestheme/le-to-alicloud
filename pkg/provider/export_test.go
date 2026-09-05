package provider

// ResetForTest 清空注册表。只编译进测试二进制（_test.go），生产代码取不到它。
//
// 注册表是包级全局，而 Register 对重名直接 panic——没有这个复位口子，同一个进程里把
// 用例跑第二遍（`go test -count=2 ./pkg/provider/`）就会 panic；更糟的是
// TestRegistry_DuplicatePanics 会**因为错误的理由通过**：它的 recover 捕获到的是第一次
// 注册（与上一轮重名）的 panic，而不是它想测的那次重复注册。
func ResetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Provider{}
}
