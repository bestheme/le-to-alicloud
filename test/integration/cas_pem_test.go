//go:build integration

package integration

import (
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q1 = "CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况"
	q5 = "CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感"
)

// TestCASAcceptsPKCS1RSAKey：cert-manager 默认签 RSA，私钥默认编码就是 PKCS#1，
// 这是生产上最常走的一条路，必须先确认它是通的。
func TestCASAcceptsPKCS1RSAKey(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())

	certID, err := uploadForTest(t, c, itName(t, "pkcs1"), certPEM, keyPEM, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#1", q1+"（PKCS#1 RSA）", result, detail)
	if err != nil {
		t.Fatalf("PKCS#1 RSA 私钥被拒绝，这是 operator 的默认输出格式: %v", err)
	}
}

// TestCASAcceptsSEC1ECKey：EC 私钥走 SEC1（"EC PRIVATE KEY"）。spec §2.2 说 CAS
// 的私钥头列表里有它，这里确认。
func TestCASAcceptsSEC1ECKey(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, testDomain())

	certID, err := uploadForTest(t, c, itName(t, "sec1"), certPEM, keyPEM, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#1", q1+"（SEC1 EC）", result, detail)
	if err != nil {
		t.Fatalf("SEC1 EC 私钥被拒绝，spec §5.4 允许 EC 证书: %v", err)
	}
}

// TestCASPKCS8KeyOutcome：这一项是纯探针，两种结果都合法——它决定 CRD 层要不要
// 直接拒绝 privateKey.encoding: PKCS8。所以断言的是「行为可分类」而不是方向：
// 要么成功，要么给出一个被 Classify 认出来的错误码，绝不能是超时或无码错误。
func TestCASPKCS8KeyOutcome(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	pkcs8 := testutil.ToPKCS8(t, keyPEM)

	certID, err := uploadForTest(t, c, itName(t, "pkcs8"), certPEM, pkcs8, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#1", q1+"（PKCS#8）", result, detail)
	if err != nil && aliyun.ClassOf(err) == aliyun.ClassRetryable {
		t.Fatalf("PKCS#8 被判成可重试错误，错误分类需要修正: %v", err)
	}
}

// TestCASRejectsEncryptedKey：加密私钥必须被拒。operator 侧 pki.ParseBundle 已经
// 拦在前面，这一项确认云侧也是同样立场（万一有人绕过 operator 手工上传）。
func TestCASRejectsEncryptedKey(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeafRSA(t, ca, testDomain())

	certID, err := uploadForTest(t, c, itName(t, "enc"), certPEM, testutil.Encrypted(t), randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#1", q1+"（加密私钥）", result, detail)
	if err == nil {
		t.Fatalf("CAS 接受了加密私钥（certId=%d），与 spec §2.2 的记载不符", certID)
	}
}

// TestCASAcceptsLeafPlusIntermediate：LE 的链就是 leaf + intermediate、不含 root，
// 这是生产上唯一会出现的形状。testutil 的 CA 在这里扮演 intermediate。
func TestCASAcceptsLeafPlusIntermediate(t *testing.T) {
	cred, region := requireCAS(t, "#5", q5)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	if n := len(splitPEM(t, certPEM)); n != 2 {
		t.Fatalf("期望两个证书块，得到 %d", n)
	}

	certID, err := uploadForTest(t, c, itName(t, "chain"), certPEM, keyPEM, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#5", q5+"（leaf+intermediate，无 root）", result, detail)
	if err != nil {
		t.Fatalf("CAS 拒绝了 leaf+intermediate，这是 LE 的标准形状: %v", err)
	}
}

// TestCASChainOrderSensitivity：把 intermediate 放到 leaf 前面。pki 的规范化输出
// 永远是 leaf 在前，所以这一项只是确认「顺序确实要紧」——如果 CAS 也接受颠倒的
// 顺序，说明 §5.4 第 7 条的排序是防御性的而非必需的。
func TestCASChainOrderSensitivity(t *testing.T) {
	cred, region := requireCAS(t, "#5", q5)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	blocks := splitPEM(t, certPEM)
	reversed := joinPEM(blocks[1], blocks[0])

	certID, err := uploadForTest(t, c, itName(t, "revchain"), reversed, keyPEM, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#5", q5+"（intermediate 在前）", result, detail)
	if err != nil && aliyun.ClassOf(err) == aliyun.ClassRetryable {
		t.Fatalf("颠倒顺序被判成可重试错误，错误分类需要修正: %v", err)
	}
}

// TestCASLeafOnly：只交 leaf、不带 intermediate。决定 operator 在 Secret 里只有
// 一张证书时该不该拒绝上传。
func TestCASLeafOnly(t *testing.T) {
	cred, region := requireCAS(t, "#5", q5)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	leafOnly := splitPEM(t, certPEM)[0]

	certID, err := uploadForTest(t, c, itName(t, "leafonly"), leafOnly, keyPEM, randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#5", q5+"（仅 leaf）", result, detail)
	if err != nil && aliyun.ClassOf(err) == aliyun.ClassRetryable {
		t.Fatalf("仅 leaf 被判成可重试错误，错误分类需要修正: %v", err)
	}
}
