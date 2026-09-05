//go:build integration

package integration

import (
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q3 = "CAS ClientToken 语义：同 token 重复上传返回同 certId 还是报错"
	q4 = "CAS 证书 Name 是否接受 - 与 ."
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
		var ae *aliyun.Error
		code := "无码"
		if asAliyunError(err2, &ae) {
			code = ae.Code
		}
		Record(t, "#3", q3, "同 token 重复上传报错", "code="+code)
	}
}

// TestCASDuplicateNameError：不同 token、同名字、同内容。这条路径决定
// isDuplicateName 该认哪个错误码——认错了，重传就会一直失败而不是认领既有证书。
func TestCASDuplicateNameError(t *testing.T) {
	cred, region := requireCAS(t, "#13", "CAS 同名不同 token 上传返回的错误码")
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	name := itName(t, "dup")

	if _, err := uploadForTest(t, c, name, certPEM, keyPEM, randToken(t)); err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	_, err := uploadForTest(t, c, name, certPEM, keyPEM, randToken(t))
	if err == nil {
		Record(t, "#13", "CAS 同名不同 token 上传返回的错误码",
			"未报错：同名可以共存", "Name 唯一性约束不成立")
		t.Error("CAS 允许了同名证书，spec §2.2 的「同账号内 Name 唯一」需要修正")
		return
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
	Record(t, "#13", "CAS 同名不同 token 上传返回的错误码", result,
		"class="+aliyun.ClassOf(err).String())
	if !known {
		t.Errorf("upload.go 的 isDuplicateName 认不出 %q，DuplicateName→findByName 的认领路径会失效", code)
	}
}

// TestCASNameCharset：naming.sanitize 现在把 [^A-Za-z0-9_] 全换成下划线。如果 CAS
// 其实接受 - 和 .，规则可以放宽，CAS 名与 CR 名的对应关系会更好认。
func TestCASNameCharset(t *testing.T) {
	cred, region := requireCAS(t, "#4", q4)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())

	for _, tc := range []struct {
		label string
		name  string
	}{
		{"连字符", "itest-" + randHex(t, 6) + "-hyphen"},
		{"点号", "itest." + randHex(t, 6) + ".dot"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			certID, err := uploadForTest(t, c, tc.name, certPEM, keyPEM, randToken(t))
			result, detail := outcome(certID, err)
			Record(t, "#4", q4+"（"+tc.label+"）", result, detail)
			if err != nil && aliyun.ClassOf(err) == aliyun.ClassRetryable {
				t.Fatalf("%s 被判成可重试错误，错误分类需要修正: %v", tc.label, err)
			}
		})
	}
}
