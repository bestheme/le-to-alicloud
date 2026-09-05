//go:build integration

package integration

import (
	"sort"
	"strings"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// TestHarnessFindingsSortNumerically 验证报告按编号的数值排序：Task 14 要按 spec §12.3
// 的编号逐行回填，#10 排在 #2 前面会让人工核对成本翻倍。
// 刻意只对局部切片排序，不碰全局 findings——本包此刻不该产生 RESULTS.md。
func TestHarnessFindingsSortNumerically(t *testing.T) {
	got := []finding{{ID: "#10"}, {ID: "#2"}, {ID: "harness"}, {ID: "#1"}, {ID: "#11"}}
	sort.SliceStable(got, func(i, j int) bool { return byID(got[i], got[j]) })

	want := []string{"#1", "#2", "#10", "#11", "harness"}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("第 %d 项期望 %s，得到 %s（完整顺序 %v）", i, w, got[i].ID, got)
		}
	}
}

// TestHarnessSplitJoinRoundTrip 验证 splitPEM / joinPEM 是可逆的——后面所有
// 「改变链形状」的用例都建立在这个前提上。
func TestHarnessSplitJoinRoundTrip(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, testDomain())

	blocks := splitPEM(t, certPEM)
	if len(blocks) != 2 {
		t.Fatalf("期望 leaf + CA 两个块，得到 %d", len(blocks))
	}
	if got := joinPEM(blocks...); string(got) != string(certPEM) {
		t.Fatal("split 后再 join 与原文不一致")
	}
	if strings.Contains(string(joinPEM(blocks...)), "\n\n") {
		t.Fatal("拼接结果里出现了空行，阿里云会拒绝")
	}
}

// TestHarnessScrubRemovesSecrets 验证报告器不会把凭证写进日志与 RESULTS.md。
//
// 用 t.Setenv 造假值而不是读真凭证：这是本计划后果最严重的失败模式（AK/SK 泄漏进
// 提交物）的唯一防线，它必须在每一次 CI 与本机运行里都真的被执行过一遍，而不是在
// 没有凭证的机器上悄悄 skip 掉。t.Setenv 会在用例结束时自动还原。
func TestHarnessScrubRemovesSecrets(t *testing.T) {
	const (
		fakeID     = "FAKE-AK-ID-FOR-SCRUB-TEST"
		fakeSecret = "FAKE-SECRET-FOR-SCRUB-TEST"
		fakeToken  = "FAKE-STS-TOKEN-FOR-SCRUB-TEST"
	)
	t.Setenv(EnvAccessKeyID, fakeID)
	t.Setenv(EnvAccessKeySecret, fakeSecret)
	t.Setenv(EnvSecurityToken, fakeToken)

	// 断言刻意不打印 got：它在失败时正含着未被抹掉的凭证。
	got := scrub("SDK 回显了请求参数：id=" + fakeID + " secret=" + fakeSecret + " token=" + fakeToken + "（结束）")
	for name, v := range map[string]string{
		EnvAccessKeyID:     fakeID,
		EnvAccessKeySecret: fakeSecret,
		EnvSecurityToken:   fakeToken,
	} {
		if strings.Contains(got, v) {
			t.Fatalf("scrub 没有抹掉 %s", name)
		}
	}
	if !strings.HasSuffix(got, "（结束）") {
		t.Fatal("凭证之外的文本被吞掉了")
	}
	if n := strings.Count(got, "***"); n != 3 {
		t.Fatalf("期望 3 个 *** 占位符，得到 %d", n)
	}
}

// TestHarnessScrubRedactsPrivateKey 验证私钥不会经 Record 漏进 RESULTS.md。探针真的
// 握着私钥，而云 SDK 的错误文本偶尔回显请求参数，所以这是纵深防御而非假想威胁。
// 这个用例不需要任何凭证，必须无条件 PASS。
func TestHarnessScrubRedactsPrivateKey(t *testing.T) {
	ca := testutil.NewCA(t)
	_, ecKey := testutil.IssueLeaf(t, ca, testDomain())
	_, rsaKey := testutil.IssueLeafRSA(t, ca, testDomain())

	cases := map[string][]byte{
		"SEC1 EC":    ecKey,
		"PKCS#1 RSA": rsaKey,
		"PKCS#8":     testutil.ToPKCS8(t, ecKey),
		"加密私钥":       testutil.Encrypted(t),
	}
	for name, keyPEM := range cases {
		t.Run(name, func(t *testing.T) {
			// 私钥正文的最后一行 base64：它出现在输出里就说明私钥没被抹干净。
			lines := strings.Split(strings.TrimSpace(string(keyPEM)), "\n")
			body := lines[len(lines)-2]

			// 断言刻意不打印 got：它在失败时恰恰含有未被抹掉的私钥，
			// 把它写进测试输出等于换个地方泄漏一遍。
			got := scrub("SDK 回显了请求参数：" + string(keyPEM) + "（结束）")
			if strings.Contains(got, body) {
				t.Fatal("scrub 没有抹掉私钥正文")
			}
			if !strings.Contains(got, redactedKey) {
				t.Fatal("私钥块没有被替换成占位符")
			}
			if !strings.HasSuffix(got, "（结束）") {
				t.Fatal("私钥块之外的文本被吞掉了")
			}
		})
	}

	// 截断块：有 BEGIN、没有配对的 END。云 SDK 对超长参数常做截断，回显出来的请求体
	// 恰好是这个形状，而只认配对块的正则对它一个字符都不会抹——这是 redactTruncatedKeys
	// 存在的全部理由。
	t.Run("BEGIN 无 END", func(t *testing.T) {
		lines := strings.Split(strings.TrimSpace(string(ecKey)), "\n")
		truncated := strings.Join(lines[:len(lines)-1], "\n") // 砍掉 END 行
		body := lines[len(lines)-2]
		const prefix = "SDK 回显了被截断的请求参数："

		got := scrub(prefix + truncated)
		if strings.Contains(got, body) {
			t.Fatal("scrub 没有抹掉截断私钥块的正文")
		}
		if !strings.Contains(got, redactedKey) {
			t.Fatal("截断私钥块没有被替换成占位符")
		}
		if !strings.HasPrefix(got, prefix) {
			t.Fatal("截断块之前的正常文本被吞掉了")
		}
	})

	// 头部截断块：有 END、没有配对的 BEGIN。SDK 通常砍尾，但只回显参数末尾若干字节
	// 同样会产生这个形状，而 privateKeyBlock 与 redactTruncatedKeys 都要求出现 BEGIN，
	// 对它一个字符都不会抹——这是 redactHeadTruncatedKeys 存在的全部理由。
	t.Run("END 无 BEGIN", func(t *testing.T) {
		lines := strings.Split(strings.TrimSpace(string(ecKey)), "\n")
		truncated := strings.Join(lines[1:], "\n") // 砍掉 BEGIN 行
		body := lines[len(lines)-2]
		const suffix = "（以上是被截断的请求参数）"

		got := scrub(truncated + suffix)
		if strings.Contains(got, body) {
			t.Fatal("scrub 没有抹掉头部截断私钥块的正文")
		}
		if !strings.Contains(got, redactedKey) {
			t.Fatal("头部截断的私钥块没有被替换成占位符")
		}
		if !strings.HasSuffix(got, suffix) {
			t.Fatal("截断块之后的正常文本被吞掉了")
		}
	})

	// 一段文本里有多个私钥块时，必须逐块替换，而不是从第一个 BEGIN 吞到最后一个 END。
	two := scrub(string(ecKey) + "中间这段要留下" + string(rsaKey))
	if !strings.Contains(two, "中间这段要留下") {
		t.Fatal("多个私钥块之间的文本被吞掉了")
	}
	if n := strings.Count(two, redactedKey); n != 2 {
		t.Fatalf("期望 2 个占位符，得到 %d", n)
	}
}
