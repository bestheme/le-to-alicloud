//go:build integration

package integration

import (
	"context"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q3  = "CAS ClientToken 语义：同 token 重复上传返回同 certId 还是报错"
	q4  = "CAS 证书 Name 是否接受 - 与 ."
	q13 = "CAS 同名不同 token 上传返回的错误码"
)

// TestCASClientTokenSameContent：write-ahead 幂等的核心假设——进程在「云侧已成功、
// 响应还没回来」时崩溃，重启后用同一个 token 重试，应该拿回同一个 certId。
func TestCASClientTokenSameContent(t *testing.T) {
	cred, region := requireCAS(t, "#3", q3)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	name, token := itName(t, "tok"), randToken(t)

	first, err := uploadForTest(t, c, name, certPEM, keyPEM, token)
	if err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	second, err2 := uploadForTest(t, c, name, certPEM, keyPEM, token)

	switch {
	case err2 == nil && second == first:
		Record(t, "#3", q3, "幂等：同 token 同内容返回同一 certId",
			"certId="+itoa(first))
	case err2 == nil && second != first:
		Record(t, "#3", q3, "不幂等：同 token 产生了第二张证书",
			"first="+itoa(first)+" second="+itoa(second))
		t.Errorf("同 token 上传出了两张证书，write-ahead 的幂等兜底不成立")
	default:
		// 可重试的错误（超时、网络、限流）是测试基础设施故障，不是云的语义。
		// Classify 把它们同样包成带 Code 的 *aliyun.Error（Timeout / NetError /
		// Throttling…），照着记就会在 RESULTS.md 留下一条绿灯的假结论——而 #3 是
		// 「write-ahead 能不能靠 ClientToken 兜底」在 spec 里的唯一证据。宁可红，
		// 也不能把一次超时写成「云拒绝了重复上传」。
		if aliyun.ClassOf(err2) == aliyun.ClassRetryable {
			t.Fatalf("第二次上传撞上可重试错误，本次没有测到 ClientToken 语义，请重跑: %v", err2)
		}
		var ae *aliyun.Error
		code := "无码"
		if asAliyunError(err2, &ae) {
			code = ae.Code
		}
		// class 与首张 certId 一并留证：读报告的人要能判断这次报错是不是永久性拒绝，
		// 以及那张先上传成功的证书确实存在。
		Record(t, "#3", q3, "同 token 重复上传报错",
			"code="+code+" class="+aliyun.ClassOf(err2).String()+" first="+itoa(first))
	}
}

// TestCASDuplicateNameError：不同 token、同名字、同内容。这条路径决定
// isDuplicateName 该认哪个错误码——认错了，重传就会一直失败而不是认领既有证书。
func TestCASDuplicateNameError(t *testing.T) {
	cred, region := requireCAS(t, "#13", q13)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	name := itName(t, "dup")

	if _, err := uploadForTest(t, c, name, certPEM, keyPEM, randToken(t)); err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	_, err := uploadForTest(t, c, name, certPEM, keyPEM, randToken(t))
	if err == nil {
		Record(t, "#13", q13, "未报错：同名可以共存", "Name 唯一性约束不成立")
		t.Error("CAS 允许了同名证书，spec §2.2 的「同账号内 Name 唯一」需要修正")
		return
	}
	if aliyun.ClassOf(err) == aliyun.ClassRetryable {
		t.Fatalf("第二次上传撞上可重试错误，本次没有测到重名错误码，请重跑: %v", err)
	}
	var ae *aliyun.Error
	code := "无码"
	if asAliyunError(err, &ae) {
		code = ae.Code
	}
	// 这一串必须与 internal/controller/upload.go 的 isDuplicateName 保持一致——它是
	// 「云真的返回了一个生产代码认不出的码」的守卫。NameRepeat 是 2026-09-05 在
	// cn-hangzhou 实测到的真码，已补进 isDuplicateName；另外三个是 Plan 1 的原始候选。
	known := code == "NameRepeat" || code == "CertNameDuplicated" ||
		code == "DuplicateCertificateName" || code == "CertNameExisted"
	result := "错误码=" + code
	if !known {
		result += "（不在 isDuplicateName 的候选里，必须补进去）"
	}
	Record(t, "#13", q13, result, "class="+aliyun.ClassOf(err).String())
	if !known {
		t.Errorf("upload.go 的 isDuplicateName 认不出 %q，DuplicateName→findByName 的认领路径会失效", code)
	}
}

// TestCASNameCharset：naming.sanitize 现在把 [^A-Za-z0-9_] 全换成下划线。如果 CAS
// 其实接受 - 和 .，规则可以放宽，CAS 名与 CR 名的对应关系会更好认。
//
// 光看「上传返回了 certId」不够：CAS 若把名字静默归一化，这里照样是「接受」，而
// findByName 用的是精确相等（upload.go 的 c.Name == casName）。放宽 sanitize 之后
// 本地名与云上名一旦对不上，write-ahead 的认领路径就会静默失配。所以每个子用例都
// 回查一次云上真正存下来的 Name。
func TestCASNameCharset(t *testing.T) {
	cred, region := requireCAS(t, "#4", q4)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())

	for _, tc := range []struct {
		label string
		name  string
	}{
		// 前缀保持 itest_：Task 6 的账号残留统计按 "itest_" 前缀认自己的证书，
		// 而「云侧已建、响应没回来」正是最可能漏清理的路径。名字中段照样带 - 与 .，
		// 待测的字符一个不少。
		{"连字符", "itest_" + randHex(t, 6) + "-hyphen"},
		{"点号", "itest_" + randHex(t, 6) + ".dot"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			certID, err := uploadForTest(t, c, tc.name, certPEM, keyPEM, randToken(t))
			result, detail := outcome(certID, err)
			if err == nil {
				stored, roundTripped := cloudName(t, c, certID)
				detail += " 云上存的名字=" + stored
				switch {
				case !roundTripped:
					// 回查本身没成功（列举报错，或列举里没有这张）。这既不是「名字被
					// 改写」也不是「往返通过」——把查不到写成被改写，会让 RESULTS.md
					// 落下一条假结论。cloudName 已经留了红灯，这里只把话说准。
					result += "，但云上名字未能回查（Name 往返本轮未验证）"
				case stored != tc.name:
					// 云静默改了名字：sanitize 一旦放宽到保留这个字符，findByName
					// 的精确相等就会失配，认领路径又会断在同一个地方。
					result += "，但云上名字被改写"
					t.Errorf("%s：上传时用 %q，云上存成了 %q——sanitize 不能按这条结论放宽",
						tc.label, tc.name, stored)
				}
			}
			Record(t, "#4", q4+"（"+tc.label+"）", result, detail)
			if err != nil && aliyun.ClassOf(err) == aliyun.ClassRetryable {
				t.Fatalf("%s 被判成可重试错误，错误分类需要修正: %v", tc.label, err)
			}
		})
	}
}

// cloudName 回查 certID 那张证书在云上真正存下来的 Name。CAS 不支持按 Name 查，
// 只能拿域名当 Keyword 拉一批回来再按 certId 认——这与 upload.go 的 findByName
// 走的是同一条路，所以这里量到的也正是那条路径能不能对上号。
//
// 查不到或对不上都只 t.Errorf 而不 Fatal：#4 的「接受/拒绝」已经测到了，名字这一
// 项失败不该把整条结论一起抹掉，但必须留下红灯。
//
// 第二个返回值区分「回查成功」与「没查成」。两个哨兵字符串（"回查失败" / "未列出"）
// 只供 detail 展示，**绝不能进等值比较**：它们必然不等于提交的名字，调用方若只比字符串，
// 就会把「没查到」写成「云上名字被改写」这条假结论。
func cloudName(t *testing.T, c aliyun.CASClient, certID int64) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	list, err := c.FindUploaded(ctx, testDomain())
	if err != nil {
		t.Errorf("回查云上名字失败，本次没能验证 Name 往返: %v", err)
		return "回查失败", false
	}
	for _, s := range list {
		if s.CertID == certID {
			return s.Name, true
		}
	}
	t.Errorf("FindUploaded 没列出刚上传的 certId=%d，Name 往返未验证", certID)
	return "未列出", false
}
