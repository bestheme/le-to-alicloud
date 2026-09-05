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

// orphanLimit 是 itest_ 残留的容忍上限。超过它说明 registerCleanup 整条链没生效，
// 继续跑只会往真实账号里堆更多垃圾。
const orphanLimit = 20

// listUploaded 用一次**独立的** callTimeout 预算做一次列举。
//
// 一个 ctx 跨多次云调用会让后面的调用继承前面已经用掉的时间，超时就变成「探针自己
// 跑得慢」而不是真实的云侧超时——探针的结论会因此变得不可复现。
func listUploaded(t *testing.T, c aliyun.CASClient, keyword string) ([]aliyun.CertSummary, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.FindUploaded(ctx, keyword)
}

// visibleAt 回答「这张证书在这个 client 对应的 endpoint 上列得出来吗」。
// 用空 Keyword 而不是域名，是为了不让 Keyword 的匹配规则本身（#12 正在测的东西）
// 成为跨 region 判定的混淆变量。
func visibleAt(t *testing.T, c aliyun.CASClient, certID int64) (bool, error) {
	t.Helper()
	list, err := listUploaded(t, c, "")
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

// countOrphans 数出 itest_ 前缀的残留张数。
func countOrphans(list []aliyun.CertSummary) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s.Name, "itest_") {
			n++
		}
	}
	return n
}

// TestCASUploadedInventory：把账号里已上传证书的数量记下来。Abandon 清理策略会留
// 孤儿证书，配额一旦打满，续期就会直接上传失败——这条数据是「cleanup_abandoned
// 必须配告警」这个结论的依据。
func TestCASUploadedInventory(t *testing.T) {
	cred, region := requireCAS(t, "#8", q8)
	c := newCAS(t, cred, region)

	// 两次列举，都是只读调用、不占配额：
	//   - 空 Keyword 想拿尽可能大的一批——#8 问的是「单账号已上传证书数量」，按域名
	//     过滤后的条数答不了这个问题（探针域名是保留域名，几乎恒为 0）；
	//   - Keyword=testDomain() 拿探针自己那批的可见量，用来和上一条对照。
	all, err := listUploaded(t, c, "")
	if err != nil {
		t.Fatalf("列出账号全部已上传证书失败: %v", err)
	}
	list, err := listUploaded(t, c, testDomain())
	if err != nil {
		t.Fatalf("列出已上传证书失败: %v", err)
	}

	// 空 Keyword 返回 0 条有两种可能：账号真的没有已上传证书，或者 CAS 把空 Keyword
	// 当成「匹配不到任何东西」。不区分这两者就等于把没测出来的东西写成结论，所以
	// 上传一张哨兵证书再列一次。
	//
	// 注意这只能否掉后一种可能，**证明不了**列举是完整的：FindUploaded 的 Status 留空
	// （pkg/aliyun/cas_sdk.go）会漏掉已过期证书，而已过期的上传证书照样占配额；配了
	// ALIYUN_RESOURCE_GROUP_ID 时列举还会被限定到该资源组。结论措辞必须带上这两条。
	// 哨兵走 uploadForTest，用例结束即删，净占配额为 0。
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, testDomain())
	sentinelID, err := uploadForTest(t, c, itName(t, "inv"), certPEM, keyPEM, randToken(t))
	if err != nil {
		t.Fatalf("上传哨兵证书失败: %v", err)
	}
	after, err := listUploaded(t, c, "")
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

	scope := "（Status 留空，不含已过期证书；配了 " + EnvResourceGroupID + " 时仅限该资源组）"
	if !sentinelVisible {
		Record(t, "#8", q8,
			"无法经 API 测得：空 Keyword 列举连刚上传的哨兵证书都不返回，不能当作账号清单",
			"空 Keyword 返回 "+itoa(int64(len(all)))+" 条、上传哨兵后仍为 "+
				itoa(int64(len(after)))+" 条；Keyword="+testDomain()+" 命中 "+
				itoa(int64(len(list)))+" 张。账号总量与配额上限需在控制台"+
				"「数字证书管理服务 → 证书管理 → 上传证书」页核对并写进 README")
		// 这条分支上空 Keyword 的清单不可信，从它算出的残留数跟着不可信，所以孤儿
		// 泄漏检查改用 Keyword=testDomain() 那份：探针上传的证书 SAN 恒为 testDomain()，
		// 这个口径虽窄但可信，不至于把泄漏检查整个跳过。
		if orphans := countOrphans(list); orphans > orphanLimit {
			t.Errorf("Keyword=%s 下就已残留 %d 张 itest_ 证书，registerCleanup 没有生效，先清理再继续",
				testDomain(), orphans)
		}
		return
	}

	orphans := countOrphans(all)
	Record(t, "#8", q8,
		"空 Keyword 列举返回 "+itoa(int64(len(all)))+" 张已上传证书"+scope+
			"，其中 Keyword="+testDomain()+" 命中 "+itoa(int64(len(list)))+" 张",
		"哨兵证书上传后可见（条数 "+itoa(int64(len(all)))+" → "+itoa(int64(len(after)))+
			"），说明空 Keyword 至少不是「匹配不到任何东西」；但这只证明列举包含哨兵，"+
			"不足以证明它是账号全集。itest_ 前缀的残留 "+itoa(int64(orphans))+" 张。"+
			"配额上限本探针不实测（撞上限会污染账号），账号总量与上限需在控制台"+
			"「数字证书管理服务 → 证书管理 → 上传证书」页核对并写进 README")
	if orphans > orphanLimit {
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
	visible, err := visibleAt(t, secondary, certID)
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
	altSelfVisible, err := visibleAt(t, secondary, altCertID)
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
	altVisibleAtPrimary, err := visibleAt(t, primary, altCertID)
	if err != nil {
		Record(t, "#9", q9, "主 endpoint 反向列举失败", err.Error())
		t.Fatalf("在 %s 反向列举失败: %v", region, err)
	}
	// detail 里带上两个 certId 的量级差：它是「两套独立 ID 空间」的旁证，把「隔离」
	// 和「跨 endpoint 复制还没追上」区分开——复制延迟不会让同一张证书拿到两个 ID。
	detail := "同一份 PEM，上传于 " + region + " 得 certId=" + itoa(certID) +
		"、上传于 " + alt + " 另得 certId=" + itoa(altCertID) +
		"（两个 ID 不在同一量级，是两套独立 ID 空间而非复制延迟）；前者在 " + alt +
		" 列举不可见，后者在本地可见、在 " + region + " "
	if altVisibleAtPrimary {
		Record(t, "#9", q9, "单向不可见：alt 看不到 primary 的证书，primary 却看得到 alt 的",
			detail+"可见")
		return
	}
	Record(t, "#9", q9, "不可见：两个 endpoint 的证书集合互相隔离（双向验证）",
		detail+"同样不可见")
}

// TestCASKeywordWildcard：通配符证书的首个 SAN 是 "*.example.com"，probe.go 会把
// 它原样当 Keyword 传给 ListUserCertificateOrder。如果 CAS 不认这种 Keyword，
// 存在性探测就会一直判「证书丢了」并反复重传。
//
// 四个 Keyword 是为了把几种候选匹配规则分开，而不只是否掉 DNS 通配符语义：
// 「域名中段」既不是 SAN 字符串的前缀也不是后缀，只有子串匹配才会命中它——
// 它是「子串匹配」与「后缀匹配」之间唯一的判别性观测。
func TestCASKeywordWildcard(t *testing.T) {
	cred, region := requireCAS(t, "#12", q12)
	c := newCAS(t, cred, region)
	// FC3_TEST_DOMAIN 本身可能已经是通配符，先削掉前缀再拼，避免拼出 "*.*.foo"。
	base := strings.TrimPrefix(testDomain(), "*.")
	wildcard := "*." + base
	// 取 base 去掉首尾标签后的中段，保证它既非 wildcard 的前缀也非其后缀。
	// 域名不足三段时构造不出这样的片段，那一格只能空着——如实说明，不硬凑。
	middle := ""
	if labels := strings.Split(base, "."); len(labels) >= 3 {
		middle = strings.Join(labels[1:len(labels)-1], ".")
	}

	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, wildcard)
	certID, err := uploadForTest(t, c, itName(t, "wild"), certPEM, keyPEM, randToken(t))
	if err != nil {
		t.Fatalf("上传通配符证书失败: %v", err)
	}

	cases := []struct {
		label   string
		keyword string
	}{
		{"通配符原样", wildcard},
		{"裸域名", base},
		{"具体子域", "probe." + base},
	}
	if middle != "" {
		cases = append(cases, struct {
			label   string
			keyword string
		}{"域名中段_既非前缀也非后缀", middle})
		// 中段能命中只说明「不是后缀匹配」，还分不清「任意子串」与「按标签对齐的
		// 包含」——中段本身恰好是一整个 DNS 标签。再取一个**标签内部**的片段（掐掉
		// 首尾字符、跨不过标签边界）：它命中才谈得上「任意子串」。纯只读，不上传。
		if r := []rune(middle); len(r) >= 3 {
			cases = append(cases, struct {
				label   string
				keyword string
			}{"标签内片段_不对齐标签边界", string(r[1 : len(r)-1])})
		}
	} else {
		Record(t, "#12", q12+"（域名中段）",
			"未测：测试域名不足三段，构造不出既非前缀也非后缀的片段",
			"FC3_TEST_DOMAIN="+base+"，因此无法区分「子串匹配」与「后缀匹配」")
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			list, err := listUploaded(t, c, tc.keyword)
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
				"证书 SAN="+wildcard+"，本次列举共返回 "+itoa(int64(len(list)))+" 条")
		})
	}
}
