//go:build integration

// Package integration 是 spec §12.3 的真实云必测清单。
//
// 这些测试对真实的阿里云账号发起调用并留下真实资源，所以：
//   - 凭证只从环境变量读，仓库里不放任何凭证；
//   - 缺少凭证时 skip 而不是失败，`make test` 与 CI 完全不受影响；
//   - 每一项的结论经 Record 落进 RESULTS.md，供最后一个任务回填 spec 与代码注释。
//
// 整个目录的每个文件都带 integration build tag，因此 `go list ./...` 看不见这个包。
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// 环境变量契约。样板见 test/integration/env.example.sh。
const (
	EnvAccessKeyID     = "ALIYUN_ACCESS_KEY_ID"
	EnvAccessKeySecret = "ALIYUN_ACCESS_KEY_SECRET"
	EnvSecurityToken   = "ALIYUN_SECURITY_TOKEN"
	EnvRegion          = "ALIYUN_REGION"
	EnvCASRegionAlt    = "ALIYUN_CAS_REGION_ALT"
	EnvResourceGroupID = "ALIYUN_RESOURCE_GROUP_ID"
	EnvFC3TestDomain   = "FC3_TEST_DOMAIN"
	EnvKubeconfig      = "INTEGRATION_KUBECONFIG"
	EnvKeepUploaded    = "INTEGRATION_KEEP_UPLOADED"
)

// callTimeout 与 --cloud-call-timeout 的默认值一致，让集成测试看到的超时行为
// 与生产一致。
const callTimeout = 30 * time.Second

func env(k string) string { return strings.TrimSpace(os.Getenv(k)) }

// testDomain 是测试证书的 SAN。CAS 只做格式校验、不验证域名归属，所以没设
// FC3_TEST_DOMAIN 时用一个保留域名，避免误碰真实域名的证书。
func testDomain() string {
	if d := env(EnvFC3TestDomain); d != "" {
		return d
	}
	return "it.integration.invalid"
}

// requireCAS 取 CAS 凭证。缺任何一项就把这一项记成「未实测」并 skip——测试不该
// 因为没凭证而变红，但清单上少了哪一项必须留下痕迹。
func requireCAS(t *testing.T, id, question string) (*aliyun.Credentials, string) {
	t.Helper()
	id0, secret, region := env(EnvAccessKeyID), env(EnvAccessKeySecret), env(EnvRegion)
	if id0 == "" || secret == "" || region == "" {
		RecordSkip(t, id, question,
			"缺少 "+EnvAccessKeyID+" / "+EnvAccessKeySecret+" / "+EnvRegion)
	}
	return &aliyun.Credentials{
		AccessKeyID:     id0,
		AccessKeySecret: secret,
		SecurityToken:   env(EnvSecurityToken),
	}, region
}

// newCAS 构造指向 region 的真实 CAS client。限流器每次新建：集成测试是串行的，
// 共享限流器只会让相邻用例互相拖慢。
func newCAS(t *testing.T, cred *aliyun.Credentials, region string) aliyun.CASClient {
	t.Helper()
	built, err := cred.Build()
	if err != nil {
		t.Fatalf("构造凭证失败: %v", err)
	}
	c, err := aliyun.NewCASClient(built, aliyun.CASClientConfig{
		Region:          region,
		ResourceGroupID: env(EnvResourceGroupID),
		Timeout:         callTimeout,
		Limiters:        aliyun.NewLimiters(),
		LimiterKey:      cred.LimiterKey(),
	})
	if err != nil {
		t.Fatalf("构造 CAS client 失败: %v", err)
	}
	return c
}

// uploadForTest 上传并登记清理：成功上传的每一张证书都会经 registerCleanup 注册一个
// t.Cleanup(func(){ deleteForTest(...) })，用例结束时从真实账号里删掉。刻意返回原始
// error 而不 Fatal：多数用例要断言的恰恰是错误的形状（错误码、是否可重试），不是「成功」。
func uploadForTest(
	t *testing.T, c aliyun.CASClient, name string, certPEM, keyPEM []byte, token string,
) (int64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	certID, err := c.Upload(ctx, name, certPEM, keyPEM, token)
	if err == nil && certID != 0 {
		registerCleanup(t, c, certID)
	}
	return certID, err
}

// deleteForTest 立刻删除一张证书。它是删除的唯一入口：registerCleanup 的 t.Cleanup
// 调它，用例也可以显式调它（「先删首张再验证同名可复用」这类路径）。统一走这一个
// 函数还顺带消掉了 unused——它不会再是一个定义了没人调的死函数。
func deleteForTest(t *testing.T, c aliyun.CASClient, certID int64) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.Delete(ctx, certID, randToken(t))
}

// registerCleanup 保证测试结束后云上不留垃圾：每张探针上传的证书都在用例结束时从
// 真实账号删除。删不掉时只记日志不 Fail——测试的结论已经拿到了，清理失败该由人接手，
// 不该把结论一起抹掉。
func registerCleanup(t *testing.T, c aliyun.CASClient, certID int64) {
	t.Helper()
	if env(EnvKeepUploaded) == "1" {
		t.Logf("INTEGRATION_KEEP_UPLOADED=1，保留 certId=%d 供人工检查", certID)
		return
	}
	t.Cleanup(func() {
		if err := deleteForTest(t, c, certID); err != nil {
			t.Logf("清理 certId=%d 失败，需人工删除: %v", certID, err)
		}
	})
}

// itName 生成本次运行专用的 CAS 证书名：itest_<12 位随机>_<suffix>。
// 字符集与长度都遵守 naming.CASName 的约束（[A-Za-z0-9_]、≤ 63），只有专门探测
// 字符集的那个用例会故意越界。
func itName(t *testing.T, suffix string) string {
	t.Helper()
	return "itest_" + randHex(t, 6) + "_" + suffix
}

// randToken 生成 32 字符的纯字母数字 ClientToken。
func randToken(t *testing.T) string {
	t.Helper()
	return randHex(t, 16)
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("生成随机数失败: %v", err)
	}
	return hex.EncodeToString(b)
}

// itoa 让报告里的 certId 有个统一写法。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// asAliyunError 是 errors.As 的一层包装，让各个用例文件不必各自 import errors。
func asAliyunError(err error, target **aliyun.Error) bool {
	return errors.As(err, target)
}

// outcome 把一次上传折叠成一句可写进报告的话。探针类用例关心的是「接受还是拒绝、
// 拒绝时的错误码是什么」，这三行在六七个用例里一模一样，抽出来免得各写一遍。
func outcome(certID int64, err error) (result, detail string) {
	if err == nil {
		return "接受", "上传成功，certId=" + itoa(certID)
	}
	var ae *aliyun.Error
	if asAliyunError(err, &ae) {
		return "拒绝", "code=" + ae.Code + " class=" + aliyun.ClassOf(err).String()
	}
	return "拒绝", "非 SDK 错误: " + err.Error()
}
