//go:build integration

package integration

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// finding 是清单里的一行结论。
type finding struct {
	ID       string // spec §12.3 的编号，例如 "#3"
	Question string
	Result   string // 一句话结论
	Detail   string // 证据：错误码、certId、返回字段
	Skipped  bool
}

var (
	findingsMu sync.Mutex
	findings   []finding
)

// Record 登记一条结论。result 与 detail 都要经 scrub——RESULTS.md 是要提交进
// 仓库的，绝不能夹带 AK/SK。
func Record(t *testing.T, id, question, result, detail string) {
	t.Helper()
	t.Logf("[%s] %s => %s (%s)", id, question, result, detail)
	findingsMu.Lock()
	defer findingsMu.Unlock()
	findings = append(findings, finding{
		ID: id, Question: question, Result: scrub(result), Detail: scrub(detail),
	})
}

// RecordSkip 登记一条「未实测」并 skip 当前用例。它不返回。
func RecordSkip(t *testing.T, id, question, why string) {
	t.Helper()
	findingsMu.Lock()
	findings = append(findings, finding{
		ID: id, Question: question, Result: "未实测", Detail: why, Skipped: true,
	})
	findingsMu.Unlock()
	t.Skip(why)
}

// privateKeyBlock 匹配一整块 PEM 私钥，从 BEGIN 行到配对的 END 行。`[A-Z ]*` 覆盖
// "EC PRIVATE KEY" / "RSA PRIVATE KEY" / "PRIVATE KEY"（PKCS#8）/ "ENCRYPTED PRIVATE KEY"
// 这几种前缀；`(?s)` 让 `.` 吃换行，非贪婪保证多块时逐块替换而不是从头吞到尾。
var privateKeyBlock = regexp.MustCompile(
	`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// redactedKey 是私钥块被抹掉后留下的占位符。留一个可见的记号而不是删成空白，
// 是为了让读报告的人知道「这里原本有过一段私钥」，而不是以为证据缺了一块。
const redactedKey = "[REDACTED PRIVATE KEY]"

// scrub 把凭证明文与私钥从报告里抹掉。云 SDK 的错误文本偶尔会回显请求参数，而探针
// 手里真的握着私钥，这一层是「报告绝不含凭证与私钥」这条约束唯一的执行点。
//
// 注意它的能力边界：只抹三个环境变量的当前明文值与 PEM 私钥块，挡不住 SDK 响应体
// 里的其它敏感字段。各个探针仍要自己保证不把响应体整个塞进 Record 的 detail。
func scrub(s string) string {
	for _, k := range []string{EnvAccessKeySecret, EnvAccessKeyID, EnvSecurityToken} {
		if v := env(k); v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return privateKeyBlock.ReplaceAllLiteralString(s, redactedKey)
}

// resultsFile 相对 cwd；go test 的 cwd 就是包目录。
const resultsFile = "RESULTS.md"

// idOrder 把 "#3" 这样的编号解析成数值。第二个返回值为 false 表示它不是纯数字编号。
func idOrder(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "#"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// byID 按编号的数值排序，让 #2 排在 #10 前面——Task 14 要按编号逐行回填 spec，
// 字符串序（#10 < #2）会让人工核对成本翻倍。非数字编号退化为字符串比较并排在数字之后。
func byID(a, b finding) bool {
	na, oka := idOrder(a.ID)
	nb, okb := idOrder(b.ID)
	switch {
	case oka && okb:
		return na < nb
	case oka != okb:
		return oka
	default:
		return a.ID < b.ID
	}
}

// writeResults 把本次运行攒下的 findings 落盘。
//
// 不变量：RESULTS.md 是**整文件覆盖**，写进去的只有本次进程记下的结论。用 `-run`
// 只跑一部分探针，得到的就是一份被截断的半截报告——这种报告只能自己看，**不得提交**。
// 要产出完整的 RESULTS.md，必须整包跑一次完整的 `make test-integration`。
func writeResults() error {
	findingsMu.Lock()
	defer findingsMu.Unlock()
	if len(findings) == 0 {
		return nil
	}
	// 一次没有凭证的运行不该把已有的真实结论覆盖成一片「未实测」。
	allSkipped := true
	for _, f := range findings {
		if !f.Skipped {
			allSkipped = false
			break
		}
	}
	if allSkipped {
		if _, err := os.Stat(resultsFile); err == nil {
			fmt.Fprintln(os.Stderr, "全部用例被 skip，保留已有的 RESULTS.md")
			return nil
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return byID(findings[i], findings[j]) })

	var b strings.Builder
	b.WriteString("# 真实云集成测试结论（spec §12.3）\n\n")
	fmt.Fprintf(&b, "- 生成时间：%s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- region：`%s`；备用 CAS region：`%s`\n", env(EnvRegion), env(EnvCASRegionAlt))
	b.WriteString("- 本文件由 `make test-integration` 生成，不要手改。\n\n")
	b.WriteString("| # | 待核实 | 结论 | 证据 |\n|---|---|---|---|\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			f.ID, mdCell(f.Question), mdCell(f.Result), mdCell(f.Detail))
	}
	return os.WriteFile(resultsFile, []byte(b.String()), 0o644)
}

// mdCell 转义竖线与换行，保证一段多行的云错误文本不会把表格撑破。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", "<br>")
}
