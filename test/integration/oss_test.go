//go:build integration

package integration

import (
	"context"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q15oss = "OSS PutCname 带 CertId + Force=true 的首绑与换绑是否都成功（T-OSS1）"
	q16oss = "OSS CertId 区域后缀取 CAS 区域还是 bucket 区域（T-OSS2）"
	q17oss = "ListCname 回报的 CertId 是否与写入字符串逐字相同（T-OSS3）"
	q18oss = "DeleteCertificate=true 后 CNAME 记录是否保留（T-OSS4）"
	q19oss = "CAS 删除被 OSS 引用的证书是否被拒、错误码（T-OSS5）"
	q20oss = "缺 oss:PutCname 权限时的错误码与 HTTP 状态（T-OSS6）"
)

// requireOSSTarget 取 OSS 探针的 bucket 与域名。缺任一项就把整组记成「未实测」并 skip。
//
// 所有者决定（2026-09-07 spec D24）不另建牺牲 bucket；这里的 skip 文案要把「怎么补测」说全。
func requireOSSTarget(t *testing.T) (bucket, domain string) {
	t.Helper()
	bucket, domain = env(EnvOSSTestBucket), env(EnvOSSTestDomain)
	if bucket == "" || domain == "" {
		why := "未实测：未设置 " + EnvOSSTestBucket + " / " + EnvOSSTestDomain +
			"。本组要真的换绑一个 OSS CNAME 上的证书，没有可供改写的 bucket + 已验证域名就无从测起；" +
			"首次实测由 www.bestheme.ac.cn 的受控首绑完成（spec 2026-09-07 D24）"
		for _, q := range []struct{ id, q string }{
			{"#15", q15oss}, {"#16", q16oss}, {"#17", q17oss}, {"#18", q18oss}, {"#19", q19oss}, {"#20", q20oss},
		} {
			recordSkipNoStop(t, q.id, q.q, why)
		}
		t.Skip(why)
	}
	return bucket, domain
}

// recordSkipNoStop 与 RecordSkip 同一条记录，但不 t.Skip——一组六个编号共用一个原因时，
// 逐条 RecordSkip 会在第一条就停下。
func recordSkipNoStop(t *testing.T, id, question, why string) {
	t.Helper()
	why = scrub(why)
	findingsMu.Lock()
	findings = append(findings, finding{ID: id, Question: question, Result: "未实测", Detail: why, Skipped: true})
	findingsMu.Unlock()
}

func newOSS(t *testing.T, cred *aliyun.Credentials, region string) aliyun.OSSClient {
	t.Helper()
	built, err := cred.Build()
	if err != nil {
		t.Fatalf("构造凭证失败: %v", err)
	}
	c, err := aliyun.NewOSSClient(built, aliyun.OSSClientConfig{
		Region: region, Timeout: callTimeout, Limiters: aliyun.NewLimiters(), LimiterKey: cred.LimiterKey(),
	})
	if err != nil {
		t.Fatalf("构造 OSS client 失败: %v", err)
	}
	return c
}

// authSeen 记录本组是否已经因偶遇 Auth 类错误而写下过 #20；结尾据此决定要不要补一行「未实测」。
var authSeen bool

// noteAuth 是 #20 唯一诚实的观测来源：本组任何一次调用撞上 Auth 类错误就顺带记下。
// 阈值式的「故意用无权限子账号」不做——那要第二套凭证，纪律与 #10 相同。
//
// 「有没有记下」只通过 authSeen 传出去，不作返回值：每个调用点都只是顺带记一笔、
// 不据此分支，留一个没人读的 bool 会被 unparam 报出来。
func noteAuth(t *testing.T, err error) {
	t.Helper()
	if err == nil || aliyun.ClassOf(err) != aliyun.ClassAuth {
		return
	}
	authSeen = true
	Record(t, "#20", q20oss, "偶遇一次鉴权失败：错误码="+errCode(err)+"，aliyun.classifyCode 判为 Auth",
		sdkSummary(err)+"；来自本组 OSS 探针的一次调用，未刻意构造缺权限账号")
}

// TestOSSBindByCertID 把 #15–#19 串成一条流水：上传两张测试证书到 CAS → 首绑 → 换绑 →
// 回读比对 → 尝试删被引用的证书 → 摘证书 → 还原。串成一条是因为它们共享同一个 CNAME 的
// 状态，拆开跑会互相踩。
func TestOSSBindByCertID(t *testing.T) {
	cred, region := requireCAS(t, "#15", q15oss)
	bucket, domain := requireOSSTarget(t)
	cas := newCAS(t, cred, region)
	oss := newOSS(t, cred, region)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout*8)
	defer cancel()

	// 读原状，结束时还原：有原证书就绑回去，没有就摘掉。还不回去时写进 RESULTS，不 Fail。
	before, err := oss.GetCname(ctx, bucket, domain)
	if err != nil {
		noteAuth(t, err)
		RecordSkip(t, "#15", q15oss, "未实测：读不到测试 CNAME（"+sdkSummary(err)+"）")
	}
	t.Cleanup(func() {
		// before 为 nil 说明原状根本没读到，没有可还原的目标。上面那条 RecordSkip 会
		// t.Skip 掉整个用例、这个 Cleanup 压根不会注册，所以这里只是形状上的兜底。
		if before == nil {
			return
		}
		rctx, rcancel := context.WithTimeout(context.Background(), callTimeout)
		defer rcancel()
		var rerr error
		target := before.CertRef
		if before.CertRef != "" {
			rerr = oss.PutCnameCert(rctx, bucket, domain, before.CertRef)
		} else {
			rerr = oss.DeleteCnameCert(rctx, bucket, domain)
			target = "「无证书」（原本就没绑）"
		}
		if rerr != nil {
			Record(t, "#15", q15oss+"（还原）", "还原失败，需人工把 CNAME 证书改回 "+target, sdkSummary(rerr))
		}
	})

	ca := testutil.NewCA(t)
	cert1, key1 := testutil.IssueLeafRSA(t, ca, domain)
	cert2, key2 := testutil.IssueLeafRSA(t, ca, domain)
	id1, err := uploadForTest(t, cas, itName(t, "oss1"), cert1, key1, randToken(t))
	if err != nil {
		RecordSkip(t, "#15", q15oss, "未实测：CAS 上传测试证书失败（"+sdkSummary(err)+"）")
	}
	id2, err := uploadForTest(t, cas, itName(t, "oss2"), cert2, key2, randToken(t))
	if err != nil {
		RecordSkip(t, "#15", q15oss, "未实测：CAS 上传第二张测试证书失败（"+sdkSummary(err)+"）")
	}
	ref1, ref2 := itoa(id1)+"-"+region, itoa(id2)+"-"+region

	// #15 首绑
	if err := oss.PutCnameCert(ctx, bucket, domain, ref1); err != nil {
		noteAuth(t, err)
		Record(t, "#15", q15oss+"（首绑）", "拒绝", sdkSummary(err))
		t.Fatalf("首绑失败: %s", sdkSummary(err))
	}
	Record(t, "#15", q15oss+"（首绑）", "成功", "certRef="+ref1)

	// #17 回读逐字比对
	got, err := oss.GetCname(ctx, bucket, domain)
	if err != nil {
		Record(t, "#17", q17oss, "无法判定：回读失败", sdkSummary(err))
	} else if got.CertRef == ref1 {
		Record(t, "#17", q17oss, "逐字相同", "写入="+ref1+" 回读="+got.CertRef+" Type="+got.CertType)
	} else {
		Record(t, "#17", q17oss, "**不同**：短路会失效，需要归一化", "写入="+ref1+" 回读="+got.CertRef)
	}

	// #15 换绑（不带 PreviousCertId，Force=true）
	if err := oss.PutCnameCert(ctx, bucket, domain, ref2); err != nil {
		noteAuth(t, err)
		Record(t, "#15", q15oss+"（换绑）", "拒绝", sdkSummary(err))
	} else {
		Record(t, "#15", q15oss+"（换绑）", "成功", "certRef "+ref1+" → "+ref2)
	}

	// #16 区域后缀：两者同区域时只能证明「同区域可行」；有备用 CAS 区域时再试一次跨区。
	Record(t, "#16", q16oss, "同区域可行（CAS 区域 == bucket 区域 == "+region+"）",
		"跨区结论见本编号的 alt 行（若有）")
	if alt := env(EnvCASRegionAlt); alt != "" && alt != region {
		altCAS := newCAS(t, cred, alt)
		cert3, key3 := testutil.IssueLeafRSA(t, ca, domain)
		id3, uerr := uploadForTest(t, altCAS, itName(t, "oss3"), cert3, key3, randToken(t))
		switch {
		case uerr != nil:
			Record(t, "#16", q16oss+"（alt）", "无法判定：备用区域上传失败", sdkSummary(uerr))
		default:
			ref3 := itoa(id3) + "-" + alt
			perr := oss.PutCnameCert(ctx, bucket, domain, ref3)
			if perr == nil {
				Record(t, "#16", q16oss+"（alt）", "接受：后缀取 **CAS 区域**（证书在 "+alt+"，bucket 在 "+region+"）", "certRef="+ref3)
			} else {
				Record(t, "#16", q16oss+"（alt）", "拒绝：跨区引用不可用，certRef 后缀须与 bucket 区域一致或证书须在同区 CAS", sdkSummary(perr))
			}
			// 回到 ref2，让后面的 #19 / #18 建立在确定的状态上。
			if perr == nil {
				if rerr := oss.PutCnameCert(ctx, bucket, domain, ref2); rerr != nil {
					t.Logf("回绑 ref2 失败: %s", sdkSummary(rerr))
				}
			}
		}
	}

	// #19 删被引用的证书（此刻 CNAME 引用的是 ref2）
	if derr := deleteForTest(t, cas, id2); derr != nil {
		Record(t, "#19", q19oss, "被拒：错误码="+errCode(derr)+" class="+aliyun.ClassOf(derr).String(), sdkSummary(derr))
	} else {
		after, gerr := oss.GetCname(ctx, bucket, domain)
		detail := "DeleteUserCertificate 成功"
		if gerr == nil {
			detail += "；随后 ListCname 回报 CertId=" + after.CertRef + " Type=" + after.CertType
		}
		Record(t, "#19", q19oss, "**允许**：CAS 不阻止删除被 OSS 引用的证书", detail)
	}

	// #18 摘证书，CNAME 应保留
	if derr := oss.DeleteCnameCert(ctx, bucket, domain); derr != nil {
		noteAuth(t, derr)
		Record(t, "#18", q18oss, "无法判定：DeleteCertificate 失败", sdkSummary(derr))
	} else {
		after, gerr := oss.GetCname(ctx, bucket, domain)
		switch {
		case gerr == nil && after.CertRef == "":
			Record(t, "#18", q18oss, "保留：CNAME 仍在且无证书", "Status="+after.Status)
		case gerr == nil:
			Record(t, "#18", q18oss, "**证书仍在**：DeleteCertificate 未生效", "CertId="+after.CertRef)
		case aliyun.ClassOf(gerr) == aliyun.ClassNotFound:
			Record(t, "#18", q18oss, "**CNAME 被删**：与文档不符", sdkSummary(gerr))
		default:
			Record(t, "#18", q18oss, "无法判定：回读失败", sdkSummary(gerr))
		}
	}

	// #20 只在偶遇时由 noteAuth 记录；没遇到就留一行说明，别让编号在 RESULTS 里消失。
	if !authSeen {
		recordSkipNoStop(t, "#20", q20oss,
			"未实测（刻意）：本组调用全部通过鉴权；构造缺权限账号需要第二套凭证，纪律与 #10 相同")
	}
}
