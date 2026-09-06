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
// 仓库的，绝不能夹带 AK/SK 或私钥。
//
// scrub 必须在 t.Logf 之前完成：Makefile 的 test-integration 带 -v，这条日志
// 用例 PASS 也照打，日志与 RESULTS.md 是同一档约束（「绝不出现在日志、event、
// status、error message、测试报告中」）。只护住落盘的那一份等于漏了一半。
func Record(t *testing.T, id, question, result, detail string) {
	t.Helper()
	result, detail = scrub(result), scrub(detail)
	t.Logf("[%s] %s => %s (%s)", id, question, result, detail)
	findingsMu.Lock()
	defer findingsMu.Unlock()
	findings = append(findings, finding{
		ID: id, Question: question, Result: result, Detail: detail,
	})
}

// RecordSkip 登记一条「未实测」并 skip 当前用例。它不返回。
//
// why 同样要过 scrub：它落进的是与 Record 相同的 Detail 字段、同一份 RESULTS.md，
// 并且会经 t.Skip 进入测试日志。调用方写出 "探测失败: "+err.Error() 是完全自然的，
// 那串 error 里可能带着回显的请求参数。
func RecordSkip(t *testing.T, id, question, why string) {
	t.Helper()
	why = scrub(why)
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

// beginKeyMarker 只匹配一条私钥的 BEGIN 行，用于兜底尾部被截断的块。
var beginKeyMarker = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

// endKeyMarker 只匹配一条私钥的 END 行，用于兜底头部被截断的块。
var endKeyMarker = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)

// redactedKey 是私钥块被抹掉后留下的占位符。留一个可见的记号而不是删成空白，
// 是为了让读报告的人知道「这里原本有过一段私钥」，而不是以为证据缺了一块。
const redactedKey = "[REDACTED PRIVATE KEY]"

// redactTruncatedKeys 是私钥兜底的第二道：privateKeyBlock 跑完之后仍然残留的 BEGIN，
// 一定是没有配对 END 的截断块——云 SDK 对超长参数常做截断，回显出来的请求体恰好就是
// 这个形状，而只认配对块的正则对它一个字符都不会抹。
//
// 残块从它的 BEGIN 一直算到下一个 BEGIN 之前、或字符串结尾。宁可多抹一段正常文本，
// 也不能让半截私钥漏进日志与 RESULTS.md。
func redactTruncatedKeys(s string) string {
	locs := beginKeyMarker.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	// 首个 BEGIN 之前的文本是安全的；从它开始，每个残块折成一个占位符。
	return s[:locs[0][0]] + strings.Repeat(redactedKey, len(locs))
}

// redactHeadTruncatedKeys 是私钥兜底的第三道，与 redactTruncatedKeys 对称：配对块与
// 「有 BEGIN 无 END」都处理完之后仍然残留的 END，一定是**头部**被截断的块——BEGIN 行
// 连同它之前的内容被砍掉了，只剩 base64 正文的尾巴与 END 行。前两条规则都要求出现
// BEGIN，对这种形状一个字符都不抹，正文会原样进到日志与 RESULTS.md。
//
// 概率比尾部截断低（SDK 通常砍尾），但 scrub 是「报告绝不含私钥」这条约束唯一的执行点，
// 唯一的执行点不该留形状上的缺口。
//
// 抹的范围是「字符串开头到最后一个孤立 END 为止」的全部内容——每个 END 折成一个占位符。
// 两个 END 之间的文本也一并抹掉：那段正是上一块的正文，留不得。同样是宁可多抹一段正常
// 文本，也不让半截私钥漏出去。
//
// 调用顺序有依赖：必须排在 redactTruncatedKeys 之后。那一步会把首个 BEGIN 起的全部内容
// 折成占位符，所以走到这里时字符串里已经不可能有 BEGIN，剩下的 END 只可能是孤儿。
func redactHeadTruncatedKeys(s string) string {
	locs := endKeyMarker.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	// 最后一个 END 之后的文本是安全的；它之前的全部内容折成 len(locs) 个占位符。
	return strings.Repeat(redactedKey, len(locs)) + s[locs[len(locs)-1][1]:]
}

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
	s = privateKeyBlock.ReplaceAllLiteralString(s, redactedKey)
	return redactHeadTruncatedKeys(redactTruncatedKeys(s))
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
			mdCell(f.ID), mdCell(f.Question), mdCell(f.Result), mdCell(f.Detail))
	}
	// 0o600：RESULTS.md 是本机跑集成测试的产物，只需生成它的人可读。虽然内容已过 scrub，
	// 但它记录的是真云账号下的资源 ID 与错误原文，没有理由对同机其它用户开放。
	return os.WriteFile(resultsFile, []byte(b.String()), 0o600)
}

// mdCell 转义竖线与换行，保证一段多行的云错误文本不会把表格撑破。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", "<br>")
}
