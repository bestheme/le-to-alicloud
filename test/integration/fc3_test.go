//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// spec §12.3 的问题文本。编号是契约，措辞与表里那一行对齐，方便 Task 14 按编号回填。
const (
	q1fc3  = "FC3 CertConfig 对 PKCS#1 / PKCS#8 / SEC1 EC 私钥的接受情况"
	q2fc3  = "UpdateCustomDomain 是全量替换还是部分合并"
	q5fc3  = "FC3 是否接受 LE 链形状（leaf+intermediate、仅 leaf）"
	q10fc3 = "FC3 API 账号级频控阈值与 Throttling 错误码"
	q14fc3 = "FC3 GetCustomDomain 对不存在域名的错误码与 HTTP 状态"
)

// TestFC3GetCustomDomainNotFound（#14）：绑定 controller 的 Observe 靠 ClassNotFound
// 区分「目标域名不存在」与「调用失败」。分错的后果是把一个永远不会出现的域名当成
// 可重试故障无限重试。
//
// 这是本文件里唯一一条**不需要测试域名**的探测：对一个必然不存在的域名做一次只读的
// GetCustomDomain，不创建、不修改任何云资源。
func TestFC3GetCustomDomainNotFound(t *testing.T) {
	cred, region := requireCAS(t, "#14", q14fc3)
	c := newFC3(t, cred, region)

	// 两个探测名一起用，是为了把**域名格式校验**这个混淆变量消掉。
	//
	// FC3 自定义域名要求真实注册的域名（中国区还要 ICP 备案），所以服务端完全可能在
	// 做存在性查找**之前**就以 InvalidArgument / ParameterInvalid / DomainNameInvalid
	// 一类把探测名按参数非法拒掉。那种码不是「FC3 说这个域名不存在」，把它当成 #14 的
	// 结论，会连带指示把它补进 classifyCode 的 NotFound 桶——那样**每一次域名参数
	// 错误都会被 Observe 判成「目标不存在」**。
	//
	//   - primary：`.example.com`（RFC 2606 保留给文档用）语法完全正常，能过格式校验，
	//     但本账号必然没有绑定过它。结论以这个名字的应答为准。
	//   - control：`.invalid`（RFC 2606 保留后缀）谁都注册不了，必然过不了「真实域名」
	//     这一关。它只做交叉对照：两个名字返回同一个码，才说明这个码与格式无关。
	//
	// 两个名字都带随机前缀，保证既不属于本账号也撞不上别人的域名。
	primary := "it-absent-" + randHex(t, 6) + ".example.com"
	control := "it-absent-" + randHex(t, 6) + ".integration.invalid"

	code, err := probeAbsentDomain(t, c, primary)
	if err == nil {
		Record(t, "#14", q14fc3, "无错误：不存在的域名也返回了对象", "查询的域名="+primary)
		t.Fatalf("GetCustomDomain 对不存在的域名 %q 没有报错", primary)
	}
	class := aliyun.ClassOf(err)

	// 守卫一：传输层故障（含拿不到错误码的兜底）说明这次调用压根没拿到 FC3 的回答。
	// 它既不是「域名不存在」也不是「FC3 拒绝」，只能记成未实测。
	//
	// code == "" 走同一条路：那意味着错误不是 *aliyun.Error。当前 fc3_sdk.go 的每条
	// 返回路径都经过 Classify，实际不可达，但一旦哪天可达了，"错误码= class=Permanent"
	// 这种没有诊断价值的结论比不记还糟。
	if code == "" || isTransportFailure(code) {
		RecordSkip(t, "#14", q14fc3,
			"未实测：调用没有拿回可解析的 FC3 应答（"+sdkSummary(err)+"），"+
				"得不出 #14 的结论。网络通畅时重跑本用例即可")
	}

	// 守卫二：鉴权失败与「域名不存在」是两回事。凭证子账号可能只有 CAS 权限，那样
	// 这里拿到的是 403 / AccessDenied——它证明的是「没有 FC3 权限」，**不是** FC3 对
	// 缺失域名的应答形状。把它写成 #14 的结论就是伪造。
	//
	// 注意 AccessDenied 本身是有歧义的：FC 的 RAM 是逐域名 ARN
	// （acs:fc:{region}:{accountId}:custom-domains/{domainName}），所以「压根没有
	// fc: 权限」与「有权限但这个域名不在授权的 ARN 集合里」会给出同一个码。要分辨
	// 就再调一次不针对具体域名的只读动作（如 ListCustomDomains）：它也 AccessDenied
	// 才说明是前者。本探针不替调用者做这一步——那要绕过 pkg/aliyun 的窄接口直接用
	// SDK，把探针和 SDK 内部结构耦上，代价大于收益。
	if class == aliyun.ClassAuth {
		RecordSkip(t, "#14", q14fc3,
			"未实测：GetCustomDomain 返回 "+sdkSummary(err)+"，是鉴权失败而非「域名不存在」，"+
				"据此得不出 #14 的结论。给子账号授予 fc:GetCustomDomain（资源可用 "+
				"custom-domains/*）后重跑本用例即可")
	}

	// 守卫三：限流同理，它说明的是账号被频控，不是域名不存在。顺带把这个偶遇的证据
	// 记进 #10——controller 明确要求「别为了触发限流而狂打真实云」，那么偶然撞上的
	// 一次就是 #10 唯一诚实的观测来源。
	if strings.HasPrefix(code, "Throttling") {
		Record(t, "#10", q10fc3,
			"偶遇一次限流：错误码="+code+"，**以 Throttling 开头**，aliyun.classifyCode 判为 "+
				class.String(), "来自 #14 探针的一次只读 GetCustomDomain；阈值本身未实测")
		RecordSkip(t, "#14", q14fc3,
			"未实测：本次 GetCustomDomain 被频控（"+sdkSummary(err)+"），不是「域名不存在」的应答")
	}

	// control 名只在下面两处出现：作为「非 NotFound 码」的旁证，和作为结论的交叉对照。
	// 它自己被拒掉是预期之内的，所以它的应答**永远不单独构成结论**。
	cCode, cErr := probeAbsentDomain(t, c, control)
	controlNote := "对照名（" + control + "）"
	switch {
	case cErr == nil:
		controlNote += "居然读到了对象"
	case cCode == "" || isTransportFailure(cCode):
		controlNote += "是传输层故障（code=" + cCode + "），无参考价值"
	case cCode == code:
		controlNote += "返回同一个码，说明该码与域名格式无关"
	default:
		controlNote += "返回 code=" + cCode + "，与主探测不同——两者之间至少有一个是格式校验的结果"
	}

	// 守卫四：只有确实拿到「不存在」这个语义的应答才落结论。判定口径与
	// pkg/aliyun 的 classifyCode 对齐：class 已是 NotFound，或错误码含 NotFound /
	// NotExist，或 HTTP 状态是 404。三者都不满足就说明这个码有别的解释（最可能是
	// 域名格式 / 参数校验），据此落结论就是假结论。
	if !isNotFoundAnswer(err, code, class) {
		RecordSkip(t, "#14", q14fc3,
			"未实测：拿到 "+sdkSummary(err)+"，无法排除它是域名格式 / 参数校验的结果而非"+
				"「域名不存在」的应答（FC3 自定义域名要求真实注册域名，中国区还要 ICP 备案）。"+
				controlNote+"。请改用一个语法正常、账号确实未绑定、且已备案的域名重跑本用例")
	}

	Record(t, "#14", q14fc3, "错误码="+code+" class="+class.String(),
		sdkSummary(err)+"；查询的域名="+primary+"；"+controlNote)
	if class != aliyun.ClassNotFound {
		t.Errorf("不存在的域名被判成 %s 而不是 NotFound，"+
			"必须把错误码 %q 补进 pkg/aliyun/errors.go 的 classifyCode 再重跑", class, code)
	}
}

// probeAbsentDomain 对一个必然不存在的域名做只读 GetCustomDomain，返回最后一次的
// 错误码与 error（error 为 nil 时错误码为空串）。
//
// 重试是必需的，不是保险：实测中到 fcv3 endpoint 的**首次**连接会撞满 5s 的
// ConnectTimeout（endpoint 有多条 A 记录，第一条可能连不上），而那是一次传输层故障，
// 跟「FC3 怎么回答不存在的域名」毫无关系。不重试就会把一次网络抖动写成 #14 的结论。
func probeAbsentDomain(t *testing.T, c aliyun.FC3Client, domain string) (string, error) {
	t.Helper()
	const attempts = 3
	var (
		err  error
		code string
	)
	for i := 1; i <= attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		_, err = c.GetCustomDomain(ctx, domain)
		cancel()
		if err == nil {
			return "", nil
		}
		code = errCode(err)
		if !isTransportFailure(code) {
			break
		}
		if i < attempts {
			t.Logf("第 %d/%d 次 GetCustomDomain 是传输层故障（code=%s），重试", i, attempts, code)
			continue
		}
		t.Logf("第 %d/%d 次 GetCustomDomain 仍是传输层故障（code=%s），不再重试", i, attempts, code)
	}
	return code, err
}

// TestFC3CertConfigEncodings（#1、#5 的 FC3 侧）：CAS 与 FC3 是两套独立的校验，
// 结论可能不同——operator 输出的 PEM 必须同时满足两边。
//
// 本用例会**真的改写** FC3_TEST_DOMAIN 的 certConfig，见 restoreCustomDomain 的注释：
// 跑完之后域名上留的是探针自签的证书，恢复不回去。
func TestFC3CertConfigEncodings(t *testing.T) {
	cred, region := requireCAS(t, "#1", q1fc3)
	domain := requireFC3Domain(t, "#1", q1fc3)
	c := newFC3(t, cred, region)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	before, err := c.GetCustomDomain(ctx, domain)
	if err != nil {
		Record(t, "#1", q1fc3, "无法判定：读不到测试域名", sdkSummary(err))
		t.Fatalf("读 %s 失败: %s", domain, sdkSummary(err))
	}
	t.Cleanup(func() { restoreCustomDomain(t, c, domain, before) })

	ca := testutil.NewCA(t)
	// IssueLeafRSA 的 certPEM 是「leaf + 其签发 CA」两块，私钥是 PKCS#1；
	// IssueLeaf 同形状但私钥是 SEC1 EC。EC 必须在 FC3 侧单独测一次：CAS 接受不代表
	// FC3 接受。
	rsaCert, rsaKey := testutil.IssueLeafRSA(t, ca, domain)
	ecCert, ecKey := testutil.IssueLeaf(t, ca, domain)
	leafOnly := joinPEM(splitPEM(t, rsaCert)[0])

	for _, tc := range []struct {
		id, question, label string
		cert, key           []byte
	}{
		{"#1", q1fc3, "PKCS#1 RSA", rsaCert, rsaKey},
		{"#1", q1fc3, "PKCS#8", rsaCert, testutil.ToPKCS8(t, rsaKey)},
		{"#1", q1fc3, "SEC1 EC", ecCert, ecKey},
		{"#5", q5fc3, "leaf+intermediate", rsaCert, rsaKey},
		{"#5", q5fc3, "仅 leaf", leafOnly, rsaKey},
	} {
		t.Run(tc.label, func(t *testing.T) {
			cctx, ccancel := context.WithTimeout(context.Background(), callTimeout)
			defer ccancel()
			uerr := c.UpdateCustomDomain(cctx, domain,
				mergedInput(before, itName(t, "fc3"), tc.cert, tc.key))
			result, detail := "接受", "UpdateCustomDomain 成功"
			if uerr != nil {
				result, detail = "拒绝", sdkSummary(uerr)
			}
			Record(t, tc.id, tc.question+"（"+tc.label+"）", result, detail)
		})
	}
}

// TestFC3UpdateSemantics（#2）：只提交 certConfig、其余字段一律不带，再回读，看
// 未提交的字段还在不在。结论决定 spec §6.3 的 read-modify-write 是「必需」还是「保险」。
//
// 判别观测用的是 protocol：CustomDomain 这个窄视图不暴露 routeConfig（Echo 的载荷
// 按设计只有构造它的 client 能解释，探针不许拆），而 protocol 是视图里唯一一个
// 「域名本来就有、本次没提交」的字段，它是否幸存正好把两种语义分开。
// **局限已知并如实记录**：protocol 幸存只说明「至少 protocol 被合并」，严格来说
// 不能替 routeConfig 打包票。
func TestFC3UpdateSemantics(t *testing.T) {
	cred, region := requireCAS(t, "#2", q2fc3)
	domain := requireFC3Domain(t, "#2", q2fc3)
	c := newFC3(t, cred, region)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	before, err := c.GetCustomDomain(ctx, domain)
	if err != nil {
		Record(t, "#2", q2fc3, "无法判定：读不到测试域名", sdkSummary(err))
		t.Fatalf("读 %s 失败: %s", domain, sdkSummary(err))
	}
	if before.Protocol == "" {
		RecordSkip(t, "#2", q2fc3,
			"未实测："+domain+" 的 protocol 为空，缺少判别性观测量")
	}
	t.Cleanup(func() { restoreCustomDomain(t, c, domain, before) })

	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeafRSA(t, ca, domain)
	// 每次云调用一份独立的 callTimeout 预算：共用一个 ctx 会让后面的调用继承前面已经
	// 用掉的时间，超时就变成「探针自己跑得慢」而不是真实的云侧超时。
	uctx, ucancel := context.WithTimeout(context.Background(), callTimeout)
	defer ucancel()
	if uerr := c.UpdateCustomDomain(uctx, domain,
		certOnlyInput(itName(t, "fc3sem"), certPEM, keyPEM)); uerr != nil {
		Record(t, "#2", q2fc3, "无法判定：只提交 certConfig 的 Update 被拒", sdkSummary(uerr))
		t.Fatalf("只提交 certConfig 的 Update 失败: %s", sdkSummary(uerr))
	}

	rctx, rcancel := context.WithTimeout(context.Background(), callTimeout)
	defer rcancel()
	after, err := c.GetCustomDomain(rctx, domain)
	if err != nil {
		Record(t, "#2", q2fc3, "无法判定：回读失败", sdkSummary(err))
		t.Fatalf("回读 %s 失败: %s", domain, sdkSummary(err))
	}

	detail := "只提交 certConfig 后 protocol 由 " + before.Protocol + " 变成 " +
		after.Protocol + "；routeConfig 不在窄视图里，本探针观测不到"
	if after.Protocol == before.Protocol {
		Record(t, "#2", q2fc3,
			"部分合并（就 protocol 而言）：未提交的 protocol 被保留，"+
				"read-modify-write 是保险而非必需", detail)
		return
	}
	Record(t, "#2", q2fc3,
		"全量替换：只提交 certConfig 会丢掉未提交的 protocol，read-modify-write 是必需的",
		detail)
}

// TestFC3ThrottlingThreshold（#10）：**刻意不实测**。
//
// 触发账号级频控只有一条路——对真实云连续打满请求，而那会连带影响同账号上跑着的
// 其它调用；本项目的探针纪律是「只操作自己创建的资源、不污染账号」，为了量一个阈值
// 去打满整个账号的 FC3 配额不符合这条纪律。
//
// 还有一层技术原因让「打满」在本 harness 里根本达不成：pkg/aliyun 的 LimitFC3 是
// 5 QPS / burst 1 的客户端限流，GetCustomDomain 每次都要先过它，探针自己就先被自己
// 限住了，云侧的账号级阈值（阿里云未公布，通常远高于 5 QPS）压根摸不到。
//
// 结论里真正要紧的那半——「错误码是否以 Throttling 开头」——由 TestFC3GetCustomDomain
// NotFound 顺带兜着：那条只读探针一旦偶遇限流，就把真实错误码记进 #10。
func TestFC3ThrottlingThreshold(t *testing.T) {
	RecordSkip(t, "#10", q10fc3,
		"未实测（刻意）：触发账号级频控需要对真实云连续打满请求，会影响同账号的其它调用，"+
			"违反探针「不污染账号」的纪律；且 pkg/aliyun 的 LimitFC3 客户端限流是 "+
			"5 QPS / burst 1，探针先被自己限住，摸不到云侧阈值。"+
			"阈值需查官方文档或提工单确认；错误码是否以 Throttling 开头，"+
			"由只读探针偶遇限流时记录（本轮未偶遇）。"+
			"**若 RESULTS.md 里另有一行 #10 记录了真实错误码，以那一行为准**——"+
			"那是 TestFC3GetCustomDomainNotFound 偶遇限流时写下的真实观测，"+
			"本行只说明「阈值」这一半没测")
}

// ---- 以下是本文件专用的辅助 ----

// newFC3 构造指向 region 的真实 FC3 client，与 newCAS 同构。
// FC3ClientConfig 没有 ResourceGroupID：自定义域名不按资源组授权，FC 的 RAM 是逐域名
// ARN（spec §8.3）。限流器每次新建，理由与 newCAS 相同。
func newFC3(t *testing.T, cred *aliyun.Credentials, region string) aliyun.FC3Client {
	t.Helper()
	built, err := cred.Build()
	if err != nil {
		t.Fatalf("构造凭证失败: %v", err)
	}
	c, err := aliyun.NewFC3Client(built, aliyun.FC3ClientConfig{
		Region:     region,
		Timeout:    callTimeout,
		Limiters:   aliyun.NewLimiters(),
		LimiterKey: cred.LimiterKey(),
	})
	if err != nil {
		t.Fatalf("构造 FC3 client 失败: %v", err)
	}
	return c
}

// requireFC3Domain 取 FC3 测试域名。这个域名会被真的改掉 certConfig 且**改不回去**
// （见 restoreCustomDomain），绝不能填生产域名。缺它就把该项记成「未实测」并 skip。
func requireFC3Domain(t *testing.T, id, question string) string {
	t.Helper()
	d := env(EnvFC3TestDomain)
	if d == "" {
		RecordSkip(t, id, question,
			"未实测：未设置 "+EnvFC3TestDomain+"。本项要真的改写一个 FC3 自定义域名的 "+
				"certConfig，没有可供改写的专用测试域名就无从测起")
	}
	return d
}

// certOnlyInput 只填 CertConfig，Protocol 与 Echo 一律零值——这正是 #2 要的
// 「只提交 certConfig」。
func certOnlyInput(name string, certPEM, keyPEM []byte) *aliyun.UpdateCustomDomainInput {
	return &aliyun.UpdateCustomDomainInput{
		CertConfig: &aliyun.CertConfig{CertName: name, CertPEM: certPEM, KeyPEM: keyPEM},
	}
}

// mergedInput 是 spec §6.3 的 read-modify-write：before 的 Echo（authConfig /
// corsConfig / routeConfig / tlsConfig / wafConfig）与 protocol 原样回填，只替换证书。
func mergedInput(
	before *aliyun.CustomDomain, name string, certPEM, keyPEM []byte,
) *aliyun.UpdateCustomDomainInput {
	return &aliyun.UpdateCustomDomainInput{
		Protocol:   before.Protocol,
		CertConfig: &aliyun.CertConfig{CertName: name, CertPEM: certPEM, KeyPEM: keyPEM},
		Echo:       before.Echo,
	}
}

// restoreCustomDomain 把 before 的 protocol 与 Echo 携带的字段回写。
//
// **它恢复不了原来的证书。** GetCustomDomain 的响应里确实带明文私钥，但 pkg/aliyun 在
// customDomainFromSDK 里当场就把它丢掉了（那是刻意的安全边界：私钥在本进程里只被读到
// 一次，且只为了不带走），所以探针手上根本没有原私钥可写回去。跑完之后域名上留着的是
// 探针自签的测试证书。这就是 FC3_TEST_DOMAIN 只能填一次性测试域名的原因。
func restoreCustomDomain(t *testing.T, c aliyun.FC3Client, domain string, before *aliyun.CustomDomain) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	err := c.UpdateCustomDomain(ctx, domain, &aliyun.UpdateCustomDomainInput{
		Protocol: before.Protocol,
		Echo:     before.Echo,
	})
	if err != nil {
		t.Logf("回写 %s 的原配置失败，需人工检查: %s", domain, sdkSummary(err))
		return
	}
	// 回写体既没有 CertConfig 也没有 ClearCert，所以域名上的证书变成什么样，取决于
	// UpdateCustomDomain 到底是全量替换还是部分合并（那正是 #2 要测的东西）：
	// 合并语义下留着探针签发的那张，替换语义下被清空。两种都不是原来的证书。
	t.Logf("已回写 %s 的 protocol 与回填字段；证书状态取决于 Update 的合并语义，需人工核对", domain)
}

// errCode 取阿里云错误码；非本包错误返回空串。
func errCode(err error) string {
	var ae *aliyun.Error
	if asAliyunError(err, &ae) {
		return ae.Code
	}
	return ""
}

// isTransportFailure 判断这个错误码是不是 pkg/aliyun 在**本地**构造的、与服务端应答
// 无关的那一类：调用根本没走到 FC3，或者走到了但没拿回可解析的应答。
//
// 探针必须把它和「FC3 真的这么回答」分开：把一次连接超时记成「FC3 对不存在的域名
// 返回 Timeout」是彻头彻尾的假结论。
func isTransportFailure(code string) bool {
	switch code {
	case "Timeout", "NetTimeout", "NetError", "EmptyResponse", "Unknown":
		return true
	default:
		return false
	}
}

// isNotFoundAnswer 判断这个应答是不是「FC3 说这个域名不存在」。
//
// 口径与 pkg/aliyun 的 classifyCode 对齐：class 已是 NotFound、错误码含 NotFound /
// NotExist、或 HTTP 状态是 404，三者任一成立即可。三者都不成立时，这个码就有别的
// 解释——最可能的是域名格式 / 参数校验（FC3 自定义域名要求真实注册域名），而不是
// 「不存在」。
//
// 这道守卫必须有：#14 落结论的那一步会顺带 t.Errorf **指示**把该错误码补进
// classifyCode 的 NotFound 桶。要是把 InvalidArgument 这类码放进去，每一次域名参数
// 错误都会被 Observe 判成「目标不存在」——比它要防的无限重试更糟。
func isNotFoundAnswer(err error, code string, class aliyun.ErrClass) bool {
	return class == aliyun.ClassNotFound ||
		strings.Contains(code, "NotFound") ||
		strings.Contains(code, "NotExist") ||
		httpStatusOf(err) == 404
}

// httpStatusOf 取错误里的 HTTP 状态码，取不到返回 0。
//
// aliyun.Error 刻意不保留 StatusCode 字段，fromSDKError 构造的内层文本
// "sdk error code=… status=…" 是它唯一暴露状态码的地方，所以这里只能解析它——
// 且只解析这一种前缀，本地构造的错误分支一律返回 0。
func httpStatusOf(err error) int {
	var ae *aliyun.Error
	if !asAliyunError(err, &ae) || ae.Err == nil {
		return 0
	}
	s := ae.Err.Error()
	if !strings.HasPrefix(s, "sdk error code=") {
		return 0
	}
	const marker = " status="
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(s[i+len(marker):]))
	if convErr != nil {
		return 0
	}
	return n
}

// sdkSummary 把一个错误折成可以安全写进报告的一句话。
//
// **刻意不用 err.Error()**：那串会把整条包装链摊开，而真正有信息量的只有错误码、
// 分类与 HTTP 状态。内层文本只在它确实是 fromSDKError 构造的 "sdk error code=… status=…"
// 时才附上——那一句按设计不含请求 / 响应体；本地构造的分支（NetError 等）的内层错误
// 可能带 endpoint 等细节，一律不带出来。
func sdkSummary(err error) string {
	var ae *aliyun.Error
	if !asAliyunError(err, &ae) {
		return "非 SDK 错误（正文已省略，避免带出调用细节）"
	}
	s := "code=" + ae.Code + " class=" + aliyun.ClassOf(err).String()
	if ae.Err != nil {
		if inner := ae.Err.Error(); strings.HasPrefix(inner, "sdk error code=") {
			s += "，" + inner
		}
	}
	return s
}
