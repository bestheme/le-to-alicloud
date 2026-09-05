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

// TestCASRejectsEncryptedKey：上传一个带 Proc-Type: 4,ENCRYPTED 头的私钥块，确认
// 云侧也拒（operator 侧 pki.ParseBundle 已经拦在前面，这一项防的是有人绕过 operator
// 手工上传）。
//
// 结论的边界必须说清楚：testutil.Encrypted 的 body 是字面量 not-a-real-key、不是可解析
// 的 DER，而且这里的 certPEM 来自另一把钥匙，证书与私钥本就不配对。所以这一项能证明的
// 只是「带 ENCRYPTED 头的垃圾 key body 被拒，且格式校验先于配对校验」，**不能**证明
// 「CAS 会解析并拒绝一个格式良好的加密私钥」。下面那道 NotMatch 断言就是在守这条边界：
// 一旦 CAS 先报配对不上，这次上传连「格式校验先行」都证明不了，不该记成 key-format 结论。
func TestCASRejectsEncryptedKey(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1)
	c := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeafRSA(t, ca, testDomain())

	certID, err := uploadForTest(t, c, itName(t, "enc"), certPEM, testutil.Encrypted(t), randToken(t))
	result, detail := outcome(certID, err)
	Record(t, "#1", q1+"（带 Proc-Type: 4,ENCRYPTED 头的私钥块）", result, detail)
	if err == nil {
		t.Fatalf("CAS 接受了带 ENCRYPTED 头的私钥块（certId=%d），与 spec §2.2 的记载不符", certID)
	}
	var ae *aliyun.Error
	if asAliyunError(err, &ae) && ae.Code == "NotMatch.CertificateAndPrivateKey" {
		t.Fatalf("CAS 先做了证书/私钥配对校验（code=%s），这次上传证明不了私钥格式校验的立场", ae.Code)
	}
}

// TestCASAcceptsLeafPlusIntermediate：上传「leaf + 其签发 CA」两块，确认 CAS 接受
// 多块链。
//
// 标签必须如实：testutil.NewCA 是自签的（CreateCertificate(tmpl, tmpl)、IsCA: true），
// 尽管它的 CN 写着 "Test Intermediate CA"，第二块实际上是一张 root，**不是** LE 那种
// 在锚点前就终止的 leaf+intermediate。所以这一项只证明「两块链可用」。
// 「LE 的 leaf+intermediate 形状（无 root）可用」这个结论要由本项与 TestCASLeafOnly
// 联合推出：leaf-only 都能过，说明 CAS 对链锚点没有任何要求。
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
	Record(t, "#5", q5+"（leaf + 其签发 CA，两块）", result, detail)
	if err != nil {
		// 夹具的第二块是本地自签的 root CA，不是 LE 的中间证书；这里只能说「leaf + 其签发
		// CA 两块」被拒，不能说「LE 的标准形状被拒」。#5 的 LE 形状结论要靠本用例与
		// 「仅 leaf」用例联合推出，见 spec §12.3 #5。
		t.Fatalf("CAS 拒绝了 leaf + 其签发 CA 这种两块形状: %v", err)
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
