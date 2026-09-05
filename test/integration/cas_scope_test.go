//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q8  = "CAS 单账号已上传证书数量与配额余量"
	q9  = "同账号在不同 CAS endpoint 上传的证书是否互相可见"
	q12 = "CAS Keyword 对通配符域名（*.example.com）的匹配行为"
)

// TestCASUploadedInventory：把账号里已上传证书的数量记下来。Abandon 清理策略会留
// 孤儿证书，配额一旦打满，续期就会直接上传失败——这条数据是「cleanup_abandoned
// 必须配告警」这个结论的依据。
func TestCASUploadedInventory(t *testing.T) {
	cred, region := requireCAS(t, "#8", q8)
	c := newCAS(t, cred, region)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	// 两次列举，都是只读调用、不占配额：
	//   - 空 Keyword 想拿账号级总数——#8 问的就是「单账号已上传证书数量」，按域名
	//     过滤后的条数答不了这个问题（探针域名是保留域名，几乎恒为 0）；
	//   - Keyword=testDomain() 拿探针自己那批的可见量，用来和上一条对照。
	all, err := c.FindUploaded(ctx, "")
	if err != nil {
		t.Fatalf("列出账号全部已上传证书失败: %v", err)
	}
	list, err := c.FindUploaded(ctx, testDomain())
	if err != nil {
		t.Fatalf("列出已上传证书失败: %v", err)
	}
	orphans := 0
	for _, s := range all {
		if strings.HasPrefix(s.Name, "itest_") {
			orphans++
		}
	}

	// 空 Keyword 返回 0 条有两种可能：账号真的没有已上传证书，或者 CAS 把空
	// Keyword 当成「匹配不到任何东西」。不区分这两者就等于把没测出来的东西写成
	// 结论，所以上传一张哨兵证书再列举一次：它出现在结果里，上面的计数才作数。
	// 这一张走 uploadForTest，用例结束时自动删除，净占用配额为 0。
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	sentinelID, err := uploadForTest(t, c, itName(t, "inv"), certPEM, keyPEM, randToken(t))
	if err != nil {
		t.Fatalf("上传哨兵证书失败: %v", err)
	}
	after, err := c.FindUploaded(ctx, "")
	if err != nil {
		t.Fatalf("上传哨兵后重新列举失败: %v", err)
	}
	sentinelVisible := false
	for _, s := range after {
		if s.CertID == sentinelID {
			sentinelVisible = true
			break
		}
	}

	if !sentinelVisible {
		Record(t, "#8", q8,
			"无法经 API 测得：空 Keyword 列举不返回账号全集（刚上传的哨兵证书不在结果里）",
			"空 Keyword 返回 "+itoa(int64(len(all)))+" 条、上传哨兵后仍为 "+
				itoa(int64(len(after)))+" 条；Keyword="+testDomain()+" 命中 "+
				itoa(int64(len(list)))+" 张。账号总量与配额需在控制台"+
				"「数字证书管理服务 → 证书管理 → 上传证书」页核对并写进 README")
		return
	}
	Record(t, "#8", q8,
		"账号级（空 Keyword）已上传证书 "+itoa(int64(len(all)))+" 张，其中 Keyword="+
			testDomain()+" 命中 "+itoa(int64(len(list)))+" 张",
		"空 Keyword 确实返回账号全集（哨兵证书上传后条数 "+itoa(int64(len(after)))+
			"）；itest_ 前缀的残留 "+itoa(int64(orphans))+" 张。配额上限本探针不实测"+
			"（撞上限会污染账号），需在控制台「数字证书管理服务 → 证书管理 → 上传证书」"+
			"页核对并写进 README")
	if orphans > 20 {
		t.Errorf("残留了 %d 张 itest_ 证书，registerCleanup 没有生效，先清理再继续", orphans)
	}
}

// TestCASCrossRegionVisibility：spec §12.3 #9 已核实 endpoint 是部分 region 化的，
// 剩下的问题是「同一账号在两个 endpoint 看到的是不是同一批证书」。答案决定
// casRegion 写错时的后果是「查不到 → 重复上传」还是「无所谓」。
func TestCASCrossRegionVisibility(t *testing.T) {
	alt := env(EnvCASRegionAlt)
	if alt == "" {
		RecordSkip(t, "#9", q9, "未设置 "+EnvCASRegionAlt)
	}
	cred, region := requireCAS(t, "#9", q9)
	if alt == region {
		RecordSkip(t, "#9", q9, EnvCASRegionAlt+" 与 "+EnvRegion+" 相同，无法比较")
	}

	primary := newCAS(t, cred, region)
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	certID, err := uploadForTest(t, primary, itName(t, "xregion"), certPEM, keyPEM, randToken(t))
	if err != nil {
		t.Fatalf("在 %s 上传失败: %v", region, err)
	}

	secondary := newCAS(t, cred, alt)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	visible, err := visibleAt(ctx, secondary, certID)
	if err != nil {
		Record(t, "#9", q9, "无法判定：备用 endpoint 列举失败", err.Error())
		t.Fatalf("在 %s 列举失败: %v", alt, err)
	}
	if visible {
		Record(t, "#9", q9, "可见：两个 endpoint 共享同一份证书集合",
			"上传于 "+region+"，在 "+alt+" 列举可见，certId="+itoa(certID))
		return
	}

	// 「在 alt 上看不见」还有一种平凡解释：alt 的 endpoint 根本没在正常工作（权限
	// 不足、region 不支持 CAS），那样它对任何证书都返回空，得出的「隔离」就是假的。
	// 所以反向再上传一张：alt 必须能看见自己刚上传的那张，才能说明它的列举有效；
	// 顺带验证隔离是双向的——primary 也看不见 alt 上的证书。
	// 刻意复用同一份 PEM：同一张证书在两个 endpoint 拿到两个不同的 certId，本身
	// 就是「两边各存一份、不是同一个库」的旁证。
	altCertID, err := uploadForTest(t, secondary, itName(t, "xregion_alt"), certPEM, keyPEM, randToken(t))
	if err != nil {
		Record(t, "#9", q9, "无法判定：备用 endpoint 无法上传，隔离与否分辨不了",
			"在 "+alt+" 上传失败: "+err.Error())
		t.Fatalf("在 %s 上传失败: %v", alt, err)
	}
	altSelfVisible, err := visibleAt(ctx, secondary, altCertID)
	if err != nil {
		Record(t, "#9", q9, "无法判定：备用 endpoint 二次列举失败", err.Error())
		t.Fatalf("在 %s 二次列举失败: %v", alt, err)
	}
	if !altSelfVisible {
		Record(t, "#9", q9, "无法判定：备用 endpoint 连自己刚上传的证书都列不出来",
			"在 "+alt+" 上传 certId="+itoa(altCertID)+" 后原地列举仍为空，"+
				"该 endpoint 的列举不可信，不能据此断言隔离")
		return
	}
	altVisibleAtPrimary, err := visibleAt(ctx, primary, altCertID)
	if err != nil {
		Record(t, "#9", q9, "无法判定：主 endpoint 反向列举失败", err.Error())
		t.Fatalf("在 %s 反向列举失败: %v", region, err)
	}
	detail := "上传于 " + region + " 的 certId=" + itoa(certID) + " 在 " + alt +
		" 列举不可见；上传于 " + alt + " 的 certId=" + itoa(altCertID) +
		" 在本地可见、在 " + region + " "
	if altVisibleAtPrimary {
		Record(t, "#9", q9, "单向不可见：alt 看不到 primary 的证书，primary 却看得到 alt 的",
			detail+"可见")
		return
	}
	Record(t, "#9", q9, "不可见：两个 endpoint 的证书集合互相隔离（双向验证）",
		detail+"同样不可见")
}

// visibleAt 回答「这张证书在这个 client 对应的 endpoint 上列得出来吗」。
// 用空 Keyword 列账号全集，免得受 #12 那套 Keyword 子串匹配规则的干扰。
func visibleAt(ctx context.Context, c aliyun.CASClient, certID int64) (bool, error) {
	list, err := c.FindUploaded(ctx, "")
	if err != nil {
		return false, err
	}
	for _, s := range list {
		if s.CertID == certID {
			return true, nil
		}
	}
	return false, nil
}

// TestCASKeywordWildcard：通配符证书的首个 SAN 是 "*.example.com"，probe.go 会把
// 它原样当 Keyword 传给 ListUserCertificateOrder。如果 CAS 不认这种 Keyword，
// 存在性探测就会一直判「证书丢了」并反复重传。
func TestCASKeywordWildcard(t *testing.T) {
	cred, region := requireCAS(t, "#12", q12)
	c := newCAS(t, cred, region)
	// FC3_TEST_DOMAIN 本身可能已经是通配符，先削掉前缀再拼，避免拼出 "*.*.foo"。
	base := strings.TrimPrefix(testDomain(), "*.")
	wildcard := "*." + base

	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, wildcard)
	certID, err := uploadForTest(t, c, itName(t, "wild"), certPEM, keyPEM, randToken(t))
	if err != nil {
		t.Fatalf("上传通配符证书失败: %v", err)
	}

	for _, tc := range []struct {
		label   string
		keyword string
	}{
		{"通配符原样", wildcard},
		{"裸域名", base},
		{"具体子域", "probe." + base},
	} {
		t.Run(tc.label, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			list, err := c.FindUploaded(ctx, tc.keyword)
			if err != nil {
				Record(t, "#12", q12+"（"+tc.label+"）", "列举失败", err.Error())
				t.Fatalf("Keyword=%q 列举失败: %v", tc.keyword, err)
			}
			found := false
			for _, s := range list {
				if s.CertID == certID {
					found = true
					break
				}
			}
			result := "匹配不到"
			if found {
				result = "能匹配到"
			}
			Record(t, "#12", q12+"（Keyword="+tc.keyword+"）", result,
				"返回 "+itoa(int64(len(list)))+" 条")
		})
	}
}
