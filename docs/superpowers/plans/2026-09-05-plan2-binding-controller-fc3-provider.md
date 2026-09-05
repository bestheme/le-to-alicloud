# Plan 2 — 绑定 controller 与 FC3 provider 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 `AliyunCertificateBinding` 的 controller 与第一个 provider：证书签发后自动写入 FC3 自定义域名的 `certConfig`，周期性检测并纠正云侧漂移，同目标多 Binding 时确定性仲裁出唯一写入者，删除时按 `deletionPolicy` 决定是否解绑。

**Architecture:** 与证书 controller 同一二进制、同一 manager。`pkg/provider` 定义 provider 无关的 `Target` / `CertMaterial` / `ObservedState` / `Provider` 接口与注册表；`pkg/provider/fc3` 是第一个实现，只做 read-modify-write 与协议判断；`pkg/aliyun` 追加窄接口 `FC3Client`（与 `CASClient` 同包，复用 `Classify` / `Limiters` / `OnCall` 三套既有机制），私钥在这一层被剔除，绝不向上传递。`internal/controller` 的绑定 controller 负责仲裁、SANs 覆盖、账号 fencing、drift 判定、status 与指标——即 spec §7 职责边界表里归「通用层」的全部内容。

**Tech Stack:** 沿用 Plan 1 的工具链（Go 1.26 / controller-runtime v0.24.1 / envtest + Ginkgo v2 / cert-manager v1.21.1 类型）。新增 `github.com/alibabacloud-go/fc-20230330/v4 v4.8.2`（依赖 `darabonba-openapi/v2 v2.2.4`；FC SDK 要求 `tea >= v1.5.2`，本仓库现为 `v1.5.3`，不触发升级）。

**Spec:** `docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md`

## Global Constraints

以下条目沿用 Plan 1（逐条仍然生效），末尾五条是 Plan 2 新增。

- Go module 路径：`git.dev.bestheme.ac.cn/infra/le-to-alicloud`。
- API group / version：`certs.bestheme.ac.cn/v1alpha1`；kinds `AliyunCertificate`、`AliyunCertificateBinding`；两者 namespaced。
- Finalizer 名：`certs.bestheme.ac.cn/finalizer`（两个 CRD 共用，已在 `api/v1alpha1/conditions.go`）。管理 label：`certs.bestheme.ac.cn/managed: "true"`。
- 最低 k8s 1.25（CEL）；CRD 必须有 `subresources.status`。
- **Secret 不进 informer cache**：manager client 对 `corev1.Secret` 已设置 `DisableFor`；代码中禁止对 Secret 调用 `List` / 使用 `Watches`。RBAC 上 secrets 只允许 `get`、`delete`。
- **私钥内容绝不出现在日志、event、status、error message 中**；日志只允许 `namespace/name`、`domainName`、指纹前 8 位。
- 私钥默认编码 PKCS#1；输出 PEM 由 DER 重新编码（leaf → intermediates，无空行）——由 `pki.Bundle.CertPEM()` / `KeyPEM()` 保证。
- CAS 证书名：`sanitize(CR名)[:50] + "_" + fingerprint[:12]`，字符集 `[A-Za-z0-9_]`，总长 ≤ 63（`naming.CASName`）。
- 错误分类沿用 `aliyun.Classify`：`Throttling*` / `ServiceUnavailable` / `InternalError` / HTTP ≥ 500 / 网络错误 → `ClassRetryable`；`InvalidAccessKeyId*` / `SignatureDoesNotMatch` / `Forbidden*` / `NoPermission*` / 401 / 403 → `ClassAuth`；`*NotFound*` / `*NotExist*` / 404 → `ClassNotFound`；其余 → `ClassPermanent`。
- Commit message 不加 `Co-Authored-By`（用户偏好）。Commit 使用 conventional commits（`feat:` / `test:` / `chore:` / `docs:`）。
- 代码注释用中文；标识符、API 名保持英文。
- 每个 Task 结束时 `make build && make test` 必须通过。
- **（新）绑定 controller 与证书 controller 同一二进制、同一 manager、同一 leader election。** 不新增 Deployment、不新增进程。
- **（新）所有 FC3 调用带派生自 reconcile ctx、超时为 `--cloud-call-timeout`（默认 30s）的 context，并经过新增的 `LimitFC3` 限流通道。** 该通道取 5 QPS / burst 1 的保守值——**FC3 的账号级频控阈值与 Throttling 错误码均未核实**（spec §12.3 #10），实测前不得放宽。
- **（新）`GetCustomDomain` 响应含明文私钥。** 剔除动作发生在 `pkg/aliyun` 的 `customDomainFromSDK` 里，是全进程唯一读到它的地方；`aliyun.CustomDomain`、`aliyun.DomainEcho` 与其上的任何结构体都不得携带私钥。
- **（新）「旁路失败不降级」**（控制器裁决 R21 / R24 / R25 的推广）：只有确实说明「目标上那张证书有问题」的失败才允许把 `Applied` / `Ready` 打成 False。`Observe` 的未知类错误属于旁路——发 Warning 事件、保留既有 condition、继续退避重试。`TargetNotFound` 与凭证类错误不属于旁路（域名不存在、AK 被吊销都直接决定了「写不进去」），照常降级。
- **（新）Plan 2 不做**：README、Argo CD 清单、PrometheusRule、真实云环境集成测试（均属 Plan 3）；CDN / CLB / ALB provider（spec §15，只留扩展点）。

---

## 文件结构

| 路径 | 职责 |
|---|---|
| `pkg/provider/provider.go` | `Target` / `CertMaterial` / `ObservedState` / `Capabilities` / `ApplyOptions` / `Client` / `DeletionPolicy` / `Provider` 接口 |
| `pkg/provider/errors.go` | `ProviderError`、错误码常量、`ErrorOf` |
| `pkg/provider/registry.go` | `Register` / `Get` / `Names` |
| `pkg/aliyun/fc3.go` | `FC3Client` 窄接口与 `CustomDomain` / `CertConfig` / `UpdateCustomDomainInput` / `DomainEcho`，含脱敏格式化 |
| `pkg/aliyun/fc3_sdk.go` | 真实 FC3 client（SDK v4.8.2、`WithContext`、限流、错误分类、私钥剔除） |
| `pkg/aliyun/fc3_contract_test.go` | 对 SDK 字段名与方法签名的编译期契约测试 |
| `pkg/aliyun/ratelimit.go`（改） | 新增 `LimitFC3` 通道 |
| `pkg/aliyun/cache.go`（改） | `ClientCache` 泛型化，供 CAS 与 FC3 各持一份 |
| `pkg/aliyun/fake/fc3.go` | 内存 fake FC3：域名表、故障注入、「服务端成功但响应丢失」 |
| `pkg/pki/bundle.go`（改） | `LeafFingerprint`（无私钥时从 PEM 算指纹） |
| `pkg/provider/fc3/provider.go` | FC3 provider：`Observe` / `Apply` / `Cleanup` |
| `pkg/provider/fc3/protocol.go` | `protocol` 字段的解析与判断 |
| `pkg/provider/fc3/errors.go` | `aliyun.Error` → `provider.ProviderError` 映射 |
| `internal/controller/aliyuncertificatebinding_controller.go` | 绑定 `Reconcile` 主流程、`SetupWithManager` |
| `internal/controller/binding_status.go` | Binding 的 condition helper、Ready 聚合、status patch、指标刷新 |
| `internal/controller/binding_conflict.go` | 同目标冲突仲裁 |
| `internal/controller/binding_material.go` | 读证书 status + Secret → `provider.CertMaterial`；域名覆盖校验 |
| `internal/controller/binding_deletion.go` | finalizer：`Orphan` / `Unbind` 两态、有界清理 |
| `internal/controller/provider_factory.go` | 凭证继承（Binding > 证书）、provider 查找、FC3 client 构造 |
| `internal/controller/indexes.go`（改） | 追加 `TargetKey` 索引（空键跳过） |
| `internal/controller/metrics.go`（改） | binding 侧指标；`OnCall` 钩子按 service 参数化 |
| `internal/controller/cas_factory.go`（改） | 适配泛型 `ClientCache` |
| `internal/controller/suite_test.go`（改） | 只在 `BeforeSuite` 里接线绑定 reconciler（并补该接线需要的 import），不放任何 FC3 helper |
| `internal/controller/suite_fc3_test.go` | FC3 fake 的全局变量与全部 binding 测试 helper：`resetFC3` / `currentFC3` / `setFC3FactoryErr` / `createBinding` / `createCertificate` / `bindingCond` / `getBinding`（Task 7 创建） |
| `internal/controller/binding_*_test.go` | 每个任务对应的单元与 Ginkgo 测试（`binding_basic` / `binding_conflict` / `binding_conflict_envtest` / `binding_material` / `binding_observe` / `binding_apply` / `binding_deletion`）；`binding_conflict_envtest_test.go` 由 Task 12 创建（Task 8 只交付纯函数单测） |
| `api/v1alpha1/aliyuncertificatebinding_types.go`（改） | `IndexBindingByTarget` 常量；`status.cleanupStartedAt` |
| `cmd/main.go`（改） | 接线绑定 reconciler，消费 `--drift-check-interval` |
| `docs/ram/binding-fc3-policy.json` | spec §8.3 的 FC3 资源级 ARN 策略样例 |

---

### Task 1: 钉版 FC3 SDK 并用编译期契约测试锁死字段名

**Files:**
- Modify: `go.mod` / `go.sum`
- Create: `pkg/aliyun/fc3_contract_test.go`

**Interfaces:**
- Produces：`github.com/alibabacloud-go/fc-20230330/v4 v4.8.2` 进入 `go.mod`；后续任务可以直接使用下列已核实的名字。

**本计划作者已在 2026-09-05 用 module cache 核对过 v4.8.2 并编译验证；执行者仍须按本任务复核一遍，因为 `go get` 可能取到更新的版本。若复核结果与下表不符，以复核结果为准，并把差异写进 ledger。**

| 名字 | 形状 |
|---|---|
| `client.CertConfig` | `CertName *string` / `Certificate *string` / `PrivateKey *string` |
| `client.UpdateCustomDomainInput` | `AuthConfig *AuthConfig` / `CertConfig *CertConfig` / `CorsConfig *CORSConfig` / `Protocol *string` / `RouteConfig *RouteConfig` / `TlsConfig *TLSConfig` / `WafConfig *WAFConfig` |
| `client.UpdateCustomDomainRequest` | `Body *UpdateCustomDomainInput` |
| `client.CustomDomain` | 含 `AccountId` / `DomainName` / `Protocol` / `CertConfig` / `AuthConfig` / `CorsConfig` / `RouteConfig` / `TlsConfig` / `WafConfig` |
| `client.GetCustomDomainResponse` | `Body *CustomDomain` |
| `client.UpdateCustomDomainResponse` | `Body *CustomDomain` |
| `(*client.Client).GetCustomDomainWithContext` | `(ctx, domainName *string, headers map[string]*string, runtime *dara.RuntimeOptions) (*GetCustomDomainResponse, error)` |
| `(*client.Client).UpdateCustomDomainWithContext` | `(ctx, domainName *string, request *UpdateCustomDomainRequest, headers map[string]*string, runtime *dara.RuntimeOptions) (*UpdateCustomDomainResponse, error)` |
| `client.NewClient` | `(*openapiutil.Config) (*Client, error)`，与 CAS 同构 |
| endpoint | `EndpointRule = "regional"`，`EndpointMap` 为 `fcv3.<region>.aliyuncs.com` |

- [ ] **Step 1: 钉版并确认不触发依赖升级**

```bash
go get github.com/alibabacloud-go/fc-20230330/v4@v4.8.2
grep -E 'darabonba-openapi/v2|alibabacloud-go/tea ' go.mod
```

Expected: `go.mod` 出现 `github.com/alibabacloud-go/fc-20230330/v4 v4.8.2`；`darabonba-openapi/v2` 仍为 `v2.2.4`、`tea` 仍为 `v1.5.3`（FC SDK 只要求 `>= v2.2.4` / `>= v1.5.2`）。若这两行被抬高了版本，先 `go mod graph | grep <模块>` 找出抬高者，再决定是否接受。

**先别跑 `go mod tidy`**：Plan 1 Task 1 的环境事实里记着「有 import 之前裸跑 `go mod tidy` 会剪掉 indirect 钉住的依赖」。本任务的 Step 2 会立刻产生 import，tidy 留到 Step 4。

- [ ] **Step 2: 写契约测试（先写、必然编译失败，因为还没 `go mod tidy`）**

`pkg/aliyun/fc3_contract_test.go`：

```go
package aliyun

import (
	"context"
	"testing"

	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
)

// TestFC3SDKContract 是一份编译期契约。
//
// 它不发起任何调用，也不断言任何行为——它的全部价值在于「引用了每一个我们依赖的字段名
// 与方法签名」。SDK 是代码生成的，字段改名对我们是静默的：少填一个 routeConfig 只会在
// 生产环境把别人的路由表清空。把名字写进一个必须编译通过的文件里，升级 SDK 时编译器
// 就会先替我们发现问题。
func TestFC3SDKContract(t *testing.T) {
	cc := &fc.CertConfig{
		CertName:    dara.String("n"),
		Certificate: dara.String("cert"),
		PrivateKey:  dara.String("key"),
	}
	in := &fc.UpdateCustomDomainInput{
		AuthConfig:  &fc.AuthConfig{},
		CertConfig:  cc,
		CorsConfig:  &fc.CORSConfig{},
		Protocol:    dara.String("HTTP,HTTPS"),
		RouteConfig: &fc.RouteConfig{},
		TlsConfig:   &fc.TLSConfig{},
		WafConfig:   &fc.WAFConfig{},
	}
	req := &fc.UpdateCustomDomainRequest{Body: in}
	if req.Body.CertConfig.CertName == nil {
		t.Fatal("UpdateCustomDomainRequest.Body.CertConfig.CertName 不可达")
	}

	body := &fc.CustomDomain{
		AccountId:   dara.String("1234567890"),
		DomainName:  dara.String("api.example.com"),
		Protocol:    dara.String("HTTPS"),
		CertConfig:  cc,
		AuthConfig:  in.AuthConfig,
		CorsConfig:  in.CorsConfig,
		RouteConfig: in.RouteConfig,
		TlsConfig:   in.TlsConfig,
		WafConfig:   in.WafConfig,
	}
	if dara.StringValue((&fc.GetCustomDomainResponse{Body: body}).Body.AccountId) != "1234567890" {
		t.Fatal("GetCustomDomainResponse.Body.AccountId 不可达")
	}
	if (&fc.UpdateCustomDomainResponse{Body: body}).Body == nil {
		t.Fatal("UpdateCustomDomainResponse.Body 不可达")
	}

	// 方法值：只做签名绑定，不调用。对指针接收者取方法值，nil 接收者是安全的。
	var c *fc.Client
	var get func(context.Context, *string, map[string]*string, *dara.RuntimeOptions) (*fc.GetCustomDomainResponse, error) = c.GetCustomDomainWithContext
	var upd func(context.Context, *string, *fc.UpdateCustomDomainRequest, map[string]*string, *dara.RuntimeOptions) (*fc.UpdateCustomDomainResponse, error) = c.UpdateCustomDomainWithContext
	var newClient func(*openapiutil.Config) (*fc.Client, error) = fc.NewClient
	if get == nil || upd == nil || newClient == nil {
		t.Fatal("方法值绑定失败")
	}
}
```

- [ ] **Step 3: 运行，确认失败**

```bash
go test ./pkg/aliyun/ -run TestFC3SDKContract
```

Expected: FAIL，`missing go.sum entry for module providing package github.com/alibabacloud-go/fc-20230330/v4/client`。

- [ ] **Step 4: 补齐 go.sum 并运行通过**

```bash
go mod tidy
go test ./pkg/aliyun/ -run TestFC3SDKContract -v
```

Expected: PASS。若编译报「unknown field」，说明取到的版本与上表不符：用下面的命令把真实形状读出来，改正测试与本计划后续所有引用这些名字的代码块，并把差异记进 ledger。

```bash
FC=$(go env GOMODCACHE)/github.com/alibabacloud-go/fc-20230330/v4@<实际版本>/client
grep -n 'type CertConfig struct' -A 30 $FC/cert_config_model.go
grep -n 'type UpdateCustomDomainInput struct' -A 30 $FC/update_custom_domain_input_model.go
grep -n 'type CustomDomain struct' -A 70 $FC/custom_domain_model.go
grep -n 'CustomDomainWithContext' $FC/client_context_func.go
```

- [ ] **Step 5: 全量校验并提交**

```bash
make build && make test
git add go.mod go.sum pkg/aliyun/fc3_contract_test.go
git commit -m "chore: pin fc-20230330/v4 and lock the SDK field contract"
```

---

### Task 2: `pkg/provider` — provider 无关的接口、错误与注册表

**Files:**
- Create: `pkg/provider/provider.go`
- Create: `pkg/provider/errors.go`
- Create: `pkg/provider/registry.go`
- Create: `pkg/provider/registry_test.go`
- Create: `pkg/provider/errors_test.go`

**Interfaces:**
- Produces（Task 6 的 fc3 实现与 Task 7–13 的 controller 全部依赖这些精确签名）：
  - `provider.Target{Type, Region, Identifier string; Spec any}`
  - `provider.CertMaterial{Fingerprint string; CertPEM, KeyPEM []byte; CertID *int64; CASName string; NotAfter time.Time; DNSNames []string}`
  - `provider.ObservedState{Exists bool; CurrentFingerprint, Protocol, AccountID string}`
  - `provider.Capabilities{ReferencesCertByID, SupportsProtocolSwitch, RequiresCASUpload bool}`
  - `provider.ApplyOptions{EnsureHTTPSProtocol bool; PreviousFingerprint string}`
  - `provider.Client any`
  - `provider.DeletionPolicy string`，常量 `provider.DeletionPolicyOrphan = "Orphan"`、`provider.DeletionPolicyUnbind = "Unbind"`
  - `provider.Provider` 接口：`Name() string`、`Capabilities() Capabilities`、`Observe(ctx, Target, Client) (ObservedState, error)`、`Apply(ctx, Target, Client, CertMaterial, ApplyOptions) error`、`Cleanup(ctx, Target, Client, DeletionPolicy) error`
  - `provider.ProviderError{Code string; Retryable bool; Reason string; Err error}`，方法 `Error() string`（**只回显 Code / Retryable / Reason，不回显被包住的 `Err`**）、`Unwrap() error`；构造 `provider.Errorf(code string, retryable bool, reason string, err error) *ProviderError`
  - 错误码常量：`CodeTargetNotFound`、`CodeAuth`、`CodeThrottled`、`CodeRetryable`、`CodePermanent`、`CodeInvalidClient`、`CodeInvalidTarget`
  - `provider.ErrorOf(err error) *ProviderError`（非本类型返回 nil）
  - `provider.Register(p Provider)`、`provider.Get(typeName string) (Provider, bool)`、`provider.Names() []string`

- [ ] **Step 1: 写失败测试**

`pkg/provider/errors_test.go`：

```go
package provider_test

import (
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

func TestErrorOf_UnwrapsWrapped(t *testing.T) {
	inner := errors.New("boom")
	pe := provider.Errorf(provider.CodeThrottled, true, "Throttled", inner)
	wrapped := errors.Join(errors.New("outer"), pe)

	got := provider.ErrorOf(wrapped)
	if got == nil {
		t.Fatal("包装后应仍能取出 ProviderError")
	}
	if got.Code != provider.CodeThrottled || !got.Retryable || got.Reason != "Throttled" {
		t.Errorf("字段丢失: %+v", got)
	}
	if !errors.Is(wrapped, inner) {
		t.Error("Unwrap 链断了")
	}
}

func TestErrorOf_PlainError(t *testing.T) {
	if provider.ErrorOf(errors.New("plain")) != nil {
		t.Error("非 ProviderError 应返回 nil")
	}
	if provider.ErrorOf(nil) != nil {
		t.Error("nil 应返回 nil")
	}
}

// 消息是唯一会被原样写进日志的字段（condition 的 message 是固定文案）。这里把一段
// 「响应体形状」的敏感片段注入被包住的 err，断言它不会经由 Error() 泄漏出来——只有
// Code / Retryable / Reason 这三个取值有界的字段允许出现。
func TestProviderError_MessageHasNoPayload(t *testing.T) {
	const payload = "-----BEGIN RSA PRIVATE KEY-----MIIEowIBAAKCAQEA-----END RSA PRIVATE KEY-----"
	pe := provider.Errorf(provider.CodeAuth, false, "CredentialsInvalid", errors.New("upstream: "+payload))
	msg := pe.Error()
	if contains(msg, payload) || contains(msg, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("被包住的错误内容泄漏进了消息: %s", msg)
	}
	for _, want := range []string{provider.CodeAuth, "CredentialsInvalid"} {
		if !contains(msg, want) {
			t.Errorf("消息里应含 %q: %s", want, msg)
		}
	}
	// 内容本身没有丢，只是不进消息：需要完整上下文的地方走 Unwrap。
	if !contains(pe.Unwrap().Error(), payload) {
		t.Error("Unwrap 应保留原始错误")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

`pkg/provider/registry_test.go`：

```go
package provider_test

import (
	"context"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

type stubProvider struct{ name string }

func (s *stubProvider) Name() string                       { return s.name }
func (s *stubProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (s *stubProvider) Observe(context.Context, provider.Target, provider.Client) (provider.ObservedState, error) {
	return provider.ObservedState{}, nil
}

func (s *stubProvider) Apply(context.Context, provider.Target, provider.Client, provider.CertMaterial, provider.ApplyOptions) error {
	return nil
}

func (s *stubProvider) Cleanup(context.Context, provider.Target, provider.Client, provider.DeletionPolicy) error {
	return nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	provider.Register(&stubProvider{name: "StubTarget"})

	got, ok := provider.Get("StubTarget")
	if !ok {
		t.Fatal("注册后应能取回")
	}
	if got.Name() != "StubTarget" {
		t.Errorf("取回了别的 provider: %s", got.Name())
	}
	if _, ok := provider.Get("NoSuchTarget"); ok {
		t.Error("未注册的类型不该命中")
	}

	found := false
	for _, n := range provider.Names() {
		if n == "StubTarget" {
			found = true
		}
	}
	if !found {
		t.Errorf("Names 里应含 StubTarget: %v", provider.Names())
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("重复注册应 panic —— 这是编译期就该发现的接线错误")
		}
	}()
	provider.Register(&stubProvider{name: "DupTarget"})
	provider.Register(&stubProvider{name: "DupTarget"})
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./pkg/provider/
```

Expected: FAIL，`no required module provides package .../pkg/provider` 或 `undefined: provider.Errorf`。

- [ ] **Step 3: 实现**

`pkg/provider/provider.go`：

```go
// Package provider 定义「把一张证书装到一个云资源上」这件事的 provider 无关接口。
//
// spec §7 的职责边界表把工作切成两半：provider 只负责「云上现在是什么」与「把它改成
// 什么」，其余（SANs 校验、幂等短路、重试退避、condition、event、指标、凭证与 client
// 构造）一律归通用层。这个包里因此没有任何 Kubernetes 类型，也没有任何 SDK 类型。
package provider

import (
	"context"
	"time"
)

// Target 是 provider 无关的目标标识。通用层用 (Type, Region, Identifier) 做 field index
// 与冲突仲裁——因此这三项必须唯一确定一个云上资源，不能有 provider 私有的补充条件。
type Target struct {
	// Type 与 CRD 的 spec.target.type 取值一致，也是注册表的键。
	Type       string
	Region     string
	Identifier string // FC3: domainName
	// Spec 指向 CRD 内嵌的目标结构体，provider 自行断言。
	Spec any
}

// CertMaterial 由通用层准备，provider 只读。
//
// CertPEM / KeyPEM 一定来自 Secret 经 pki 规范化后的输出，不来自 status——「写入内容的
// 确定性」（spec §3）：多副本短暂重叠时，最坏情况是两次写入同样的字节。
type CertMaterial struct {
	Fingerprint string // SHA-256(leaf DER)，小写 hex
	CertPEM     []byte // leaf + intermediates
	KeyPEM      []byte // PKCS#1 / SEC1
	CertID      *int64 // CAS certId；ReferencesCertByID=false 的 provider 忽略
	CASName     string // 云侧证书名；FC3 用作 certConfig.certName
	NotAfter    time.Time
	DNSNames    []string
}

// ObservedState 是 Observe 的结论，也是幂等判断唯一的真相来源。
type ObservedState struct {
	Exists             bool
	CurrentFingerprint string // 目标上实际证书的指纹；"" = 目标上没有证书
	Protocol           string
	AccountID          string // 账号 fencing 用
}

// Capabilities 描述 provider 的形状，通用层据此决定要不要强制 CAS 上传等。
type Capabilities struct {
	ReferencesCertByID     bool // true = 目标存 certId（CDN/CLB）；false = 内联 PEM（FC3）
	SupportsProtocolSwitch bool
	RequiresCASUpload      bool // true ⇒ 强制 uploadToCAS，用户不可关
}

// ApplyOptions 是本次写入的调节项。
type ApplyOptions struct {
	EnsureHTTPSProtocol bool
	// PreviousFingerprint 供「替换需指定旧值」的未来 provider（乐观并发）；FC3 忽略。
	PreviousFingerprint string
}

// Client 是通用层构造好、交给 provider 使用的云客户端。
//
// 具体类型由 provider 自行断言（FC3 断言为 aliyun.FC3Client），与 Target.Spec 同构。
// 用 any 而不是给 Provider 加类型参数：注册表是异构的 map[string]Provider，类型参数
// 会让它无法成立。断言失败返回 CodeInvalidClient，是接线错误而非运行时错误。
type Client any

// DeletionPolicy 与 CRD 的 spec.deletionPolicy 取值一一对应。
//
// 刻意在本包里重新定义而不是引用 api/v1alpha1：pkg/provider 不该依赖 CRD 类型，
// 否则以后想把 provider 抽成独立模块就要连 API 包一起搬。通用层负责字符串转换。
type DeletionPolicy string

const (
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
	DeletionPolicyUnbind DeletionPolicy = "Unbind"
)

// Provider 是一类云目标的读写实现。
//
// 三个方法都必须是幂等的，且都必须返回经过分类的 *ProviderError（除非 error 为 nil）：
// 通用层不认识任何 SDK 错误，只认 ProviderError 的 Code / Retryable / Reason。
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Observe(ctx context.Context, t Target, c Client) (ObservedState, error)
	Apply(ctx context.Context, t Target, c Client, m CertMaterial, o ApplyOptions) error
	Cleanup(ctx context.Context, t Target, c Client, policy DeletionPolicy) error
}
```

`pkg/provider/errors.go`：

```go
package provider

import (
	"errors"
	"fmt"
)

// 错误码。取值集合有界，可以安全地进日志与（经 Reason 转换后）进 condition。
const (
	CodeTargetNotFound = "TargetNotFound" // 目标资源不存在
	CodeAuth           = "Auth"           // 凭证 / 授权问题
	CodeThrottled      = "Throttled"      // 被限流
	CodeRetryable      = "Retryable"      // 其它瞬时故障
	CodePermanent      = "Permanent"      // 不重试就不会变好
	CodeInvalidClient  = "InvalidClient"  // 通用层传错了 client 类型（接线错误）
	CodeInvalidTarget  = "InvalidTarget"  // Target.Spec 断言失败（接线错误）
)

// ProviderError 是 provider 对失败的分类结论。
//
// Reason 是 provider 建议通用层写进 condition 的 reason 字符串（v1alpha1 的 Reason* 常量
// 之一）。让 provider 给出建议而不是让通用层猜：只有 provider 知道 404 到底意味着
// 「域名没建」还是「证书没绑」。
type ProviderError struct {
	Code      string
	Retryable bool
	Reason    string
	Err       error
}

// Error 只回显取值有界的三个字段。
//
// 被包住的 err 进 Unwrap 链、不进消息：它的文本由 SDK / 云侧决定，长度与内容都不
// 受我们控制，而这条消息会被原样写进日志（Global Constraints 的「私钥内容绝不出现在
// 日志、event、status、error message 中」）。需要完整上下文时用 errors.Unwrap。
func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider error %s (retryable=%t, reason=%s)", e.Code, e.Retryable, e.Reason)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// Errorf 构造 ProviderError。
func Errorf(code string, retryable bool, reason string, err error) *ProviderError {
	return &ProviderError{Code: code, Retryable: retryable, Reason: reason, Err: err}
}

// ErrorOf 从错误链里取出 ProviderError；不是这个类型时返回 nil。
func ErrorOf(err error) *ProviderError {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
```

`pkg/provider/registry.go`：

```go
package provider

import (
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register 注册一个 provider，键取 p.Name()（必须等于 CRD 的 spec.target.type 取值）。
//
// 重复注册直接 panic：注册发生在 init() 里，重名意味着两个包争同一个 target type，
// 这是编译期就该被发现的接线错误，静默覆盖会让「写进了哪个云」变成启动顺序的函数。
func Register(p Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if _, dup := registry[name]; dup {
		panic("provider: 重复注册 " + name)
	}
	registry[name] = p
}

// Get 按 target type 取 provider。
func Get(typeName string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[typeName]
	return p, ok
}

// Names 返回已注册的 target type，已排序（供日志与错误消息使用）。
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: 运行，确认通过**

```bash
go test ./pkg/provider/ -v
```

Expected: 4 个用例全 PASS。

- [ ] **Step 5: 提交**

```bash
make build && make test
git add pkg/provider/
git commit -m "feat: add provider-agnostic Target/CertMaterial/Provider interfaces and registry"
```

---

### Task 3: `pkg/aliyun` — FC3 窄接口与类型、脱敏、`LimitFC3`、`ClientCache` 泛型化

**Files:**
- Create: `pkg/aliyun/fc3.go`
- Create: `pkg/aliyun/fc3_test.go`
- Modify: `pkg/aliyun/ratelimit.go`
- Modify: `pkg/aliyun/cache.go`
- Modify: `pkg/aliyun/cache_test.go`
- Modify: `internal/controller/cas_factory.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Consumes：`aliyun.Limiters` / `aliyun.LimitKind`（已存在）；`aliyun.ClientKey{Namespace, Name, ResourceVersion, Region, Endpoint, ResourceGroupID string}`（已存在）。
- Produces：
  - `aliyun.CertConfig{CertName string; CertPEM, KeyPEM []byte}`，方法 `String() string`、`GoString() string`、`MarshalJSON() ([]byte, error)`（值接收者）
  - `aliyun.DomainEcho{Payload any}`
  - `aliyun.CustomDomain{AccountID, DomainName, Protocol, CertName, CertificatePEM string; Echo DomainEcho}`
  - `aliyun.UpdateCustomDomainInput{Protocol string; CertConfig *CertConfig; ClearCert bool; Echo DomainEcho}`，同样三个脱敏方法
  - `aliyun.FC3Client` 接口：`GetCustomDomain(ctx, domain string) (*CustomDomain, error)`、`UpdateCustomDomain(ctx, domain string, in *UpdateCustomDomainInput) error`
  - `aliyun.LimitFC3`（新的 `LimitKind` 取值）
  - `aliyun.ClientCache[T any]`：`NewClientCache[T any]() *ClientCache[T]`、`(c *ClientCache[T]) GetOrBuild(key ClientKey, build func() (T, error)) (T, error)`、`(c *ClientCache[T]) Len() int`
  - `controller.NewCASFactory(reader client.Reader, cache *aliyun.ClientCache[aliyun.CASClient], limiters *aliyun.Limiters, timeout time.Duration) CASFactory`（签名变更）

- [ ] **Step 1: 写失败测试**

`pkg/aliyun/fc3_test.go`：

```go
package aliyun_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

const fakeKeyPEM = "-----BEGIN RSA PRIVATE KEY-----\nSUPERSECRETKEYMATERIAL\n-----END RSA PRIVATE KEY-----\n"

// 私钥泄漏的真实通道是结构化日志：zap 对未知类型走反射 JSON 编码，导出字段会被原样写出。
// 这三个用例分别堵住 %v / %#v / json.Marshal 三条路，值与指针两种形态都覆盖。
func TestCertConfig_StringRedactsKey(t *testing.T) {
	cc := aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)}
	for _, s := range []string{cc.String(), cc.GoString()} {
		if strings.Contains(s, "SUPERSECRET") {
			t.Fatalf("私钥出现在格式化输出里: %s", s)
		}
		if !strings.Contains(s, "n") {
			t.Errorf("应保留 certName: %s", s)
		}
	}
}

func TestCertConfig_MarshalJSONRedactsKey(t *testing.T) {
	cc := aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)}
	for _, v := range []any{cc, &cc} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "SUPERSECRET") {
			t.Fatalf("私钥出现在 JSON 里: %s", b)
		}
	}
}

func TestUpdateCustomDomainInput_RedactsNestedKey(t *testing.T) {
	in := aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n", CertPEM: []byte("CERT"), KeyPEM: []byte(fakeKeyPEM)},
		Echo:       aliyun.DomainEcho{Payload: "opaque"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SUPERSECRET") || strings.Contains(in.String(), "SUPERSECRET") {
		t.Fatalf("嵌套私钥泄漏: %s / %s", b, in.String())
	}
	if !strings.Contains(string(b), "HTTP,HTTPS") {
		t.Errorf("protocol 应保留: %s", b)
	}
}

func TestClientCache_TypedPerService(t *testing.T) {
	c := aliyun.NewClientCache[aliyun.FC3Client]()
	key := aliyun.ClientKey{Namespace: "ns", Name: "cred", ResourceVersion: "1", Region: "cn-hangzhou"}
	calls := 0
	build := func() (aliyun.FC3Client, error) { calls++; return nil, nil }

	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("同 resourceVersion 应复用，build 调用了 %d 次", calls)
	}
	key.ResourceVersion = "2"
	if _, err := c.GetOrBuild(key, build); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("resourceVersion 变化应重建，build 调用了 %d 次", calls)
	}
}

// 独立通道的判据不是「两个常量都存在」，而是「耗尽 FC3 的桶不会拖慢 CAS 写」。
// 只调一次 Wait 的写法在两个 kind 共用一个桶时同样会通过，等于没测。
func TestLimitFC3_HasOwnBucket(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := t.Context()
	// FC3 是 5 QPS / burst 1：把 burst 里那一个令牌取走，桶就空了。
	if err := l.Wait(ctx, "ak", aliyun.LimitFC3); err != nil {
		t.Fatal(err)
	}
	// 桶空之后再取一个必须等满一个补充周期（1/5 s = 200ms）。判据放宽到 150ms，
	// 给 CI 上的调度抖动留余量。
	start := time.Now()
	if err := l.Wait(ctx, "ak", aliyun.LimitFC3); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("FC3 桶耗尽后应等待约 200ms，实际 %v——限流通道没有独立的 rate/burst", d)
	}
	// 同一个 key 的 CAS 写通道（50 QPS / burst 10）必须完全不受影响。
	start = time.Now()
	if err := l.Wait(ctx, "ak", aliyun.LimitCASWrite); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 50*time.Millisecond {
		t.Errorf("CAS 写通道应立即放行，实际等了 %v——两个 kind 共用了同一个桶", d)
	}
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./pkg/aliyun/ -run 'TestCertConfig|TestUpdateCustomDomainInput|TestClientCache_Typed|TestLimitFC3'
```

Expected: FAIL，`undefined: aliyun.CertConfig`。

- [ ] **Step 3: 实现类型与脱敏**

`pkg/aliyun/fc3.go`：

```go
package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
)

// redactedKey 是私钥在任何格式化输出里的占位符。
const redactedKey = "<redacted>"

// CertConfig 是要写入 FC3 自定义域名的证书。
//
// KeyPEM 是明文私钥。它必须能被序列化进 HTTP 请求体，又绝不能进日志 / 事件 / status；
// 下面三个方法就是这条约束的护栏，与 pki.Bundle 用的是同一套手法（值接收者，让值与
// 指针两种形态都被覆盖）。
type CertConfig struct {
	CertName string
	CertPEM  []byte
	KeyPEM   []byte
}

func (c CertConfig) redacted() any {
	return struct {
		CertName  string `json:"certName"`
		CertBytes int    `json:"certBytes"`
		Key       string `json:"key"`
	}{c.CertName, len(c.CertPEM), redactedKey}
}

func (c CertConfig) String() string {
	return fmt.Sprintf("aliyun.CertConfig{certName=%s, certBytes=%d, key=%s}", c.CertName, len(c.CertPEM), redactedKey)
}

func (c CertConfig) GoString() string { return c.String() }

func (c CertConfig) MarshalJSON() ([]byte, error) { return json.Marshal(c.redacted()) }

// DomainEcho 携带 read-modify-write 必须原样回填、而本项目从不解释的字段
// （authConfig / corsConfig / routeConfig / tlsConfig / wafConfig）。
//
// Payload 只由构造它的 client 解释：真实 client 放 SDK 的写入体，fake 放一个标记值。
// 其它任何代码都不许读它——这既让 fake 能断言「回填没丢」，也保证证书配置永远不会
// 混进回填体（私钥因此不可能从这条路径逃逸）。
type DomainEcho struct {
	Payload any
}

// CustomDomain 是 GetCustomDomain 响应的裁剪。**私钥已在构造时丢弃**，本结构体不含它。
type CustomDomain struct {
	AccountID  string
	DomainName string
	Protocol   string
	CertName   string
	// CertificatePEM 是云侧当前证书的公开部分，用来算指纹；没有证书时为空。
	CertificatePEM string
	Echo           DomainEcho
}

// UpdateCustomDomainInput 是 read-modify-write 的写入体。
//
// 调用方从 CustomDomain 拿到 Echo 原样带上，只决定 Protocol 与证书这两件事。
type UpdateCustomDomainInput struct {
	Protocol string
	// CertConfig 非 nil 时写入这套证书。
	CertConfig *CertConfig
	// ClearCert 为 true 时把云侧 certConfig 显式清空（解绑）。优先于 CertConfig。
	ClearCert bool
	Echo      DomainEcho
}

func (in UpdateCustomDomainInput) redacted() any {
	var cc any
	if in.CertConfig != nil {
		cc = in.CertConfig.redacted()
	}
	return struct {
		Protocol   string `json:"protocol"`
		CertConfig any    `json:"certConfig,omitempty"`
		ClearCert  bool   `json:"clearCert"`
	}{in.Protocol, cc, in.ClearCert}
}

func (in UpdateCustomDomainInput) String() string {
	cert := "<none>"
	if in.CertConfig != nil {
		cert = in.CertConfig.String()
	}
	return fmt.Sprintf("aliyun.UpdateCustomDomainInput{protocol=%s, clearCert=%t, certConfig=%s}",
		in.Protocol, in.ClearCert, cert)
}

func (in UpdateCustomDomainInput) GoString() string { return in.String() }

func (in UpdateCustomDomainInput) MarshalJSON() ([]byte, error) { return json.Marshal(in.redacted()) }

// FC3Client 是本项目对函数计算 3.0 的全部依赖。
//
// 只有两个方法：证书归属于域名，与函数无关（spec §2.1），所以创建 / 删除域名、路由表、
// WAF 这些都不在 operator 的职责里。
type FC3Client interface {
	// GetCustomDomain 读取域名的完整配置。响应中的明文私钥在实现层被丢弃。
	GetCustomDomain(ctx context.Context, domain string) (*CustomDomain, error)
	// UpdateCustomDomain 提交 read-modify-write 的结果。
	UpdateCustomDomain(ctx context.Context, domain string, in *UpdateCustomDomainInput) error
}
```

**注意**：`pkg/pki/bundle.go` 里已有一个同名的包级常量 `redactedKey`。两者在不同包中，互不冲突。

- [ ] **Step 4: 追加 `LimitFC3`**

`pkg/aliyun/ratelimit.go`，在 `LimitCASWrite` 之后追加取值，并同步 `newLimiter` 与 `kindName`：

```go
const (
	// LimitCASList 对应 ListUserCertificateOrder（官方 QPS 10），留余量取 8。
	LimitCASList LimitKind = iota
	// LimitCASWrite 对应 Upload / Delete（官方 QPS 100），取 50。
	LimitCASWrite
	// LimitFC3 对应 GetCustomDomain / UpdateCustomDomain。
	//
	// 阿里云没有公布 FC3 的账号级频控阈值，spec §12.3 #10 把它列为必测项。在实测出来
	// 之前取 5 QPS / burst 1 这个保守值：drift 检测每小时一轮，即使几百个 Binding 同时
	// 被唤醒，排队一分钟也远比在生产上撞出 Throttling 便宜。实测后再放宽。
	LimitFC3
)

func newLimiter(k LimitKind) *rate.Limiter {
	switch k {
	case LimitCASList:
		return rate.NewLimiter(rate.Limit(8), 1)
	case LimitFC3:
		return rate.NewLimiter(rate.Limit(5), 1)
	default:
		return rate.NewLimiter(rate.Limit(50), 10)
	}
}

func kindName(k LimitKind) string {
	switch k {
	case LimitCASList:
		return "cas-list"
	case LimitFC3:
		return "fc3"
	default:
		return "cas-write"
	}
}
```

- [ ] **Step 5: `ClientCache` 泛型化**

`pkg/aliyun/cache.go`：把 `entry` 与 `ClientCache` 改成类型参数化，`ClientKey` 与注释原样保留。

```go
type entry[T any] struct {
	rv     string
	client T
}

// ClientCache 每个 (namespace, name, region, endpoint, resourceGroupId) 只保留最新
// resourceVersion 的 client。
//
// 泛型是因为 CAS 与 FC3 的 client 类型不同，而两者的缓存规则完全一样：与其让缓存存
// any 再到处断言，不如让每个服务各持一份类型确定的缓存。
type ClientCache[T any] struct {
	mu sync.Mutex
	m  map[string]entry[T]
}

func NewClientCache[T any]() *ClientCache[T] { return &ClientCache[T]{m: map[string]entry[T]{}} }

// GetOrBuild 返回缓存 client，或用 build 构造并替换旧版本。
func (c *ClientCache[T]) GetOrBuild(key ClientKey, build func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := key.identity()
	if e, ok := c.m[id]; ok && e.rv == key.ResourceVersion {
		return e.client, nil
	}
	cl, err := build()
	if err != nil {
		var zero T
		return zero, err
	}
	c.m[id] = entry[T]{rv: key.ResourceVersion, client: cl}
	return cl, nil
}

// Len 返回缓存条目数（测试用）。
func (c *ClientCache[T]) Len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.m) }
```

- [ ] **Step 6: 更新三处调用点**

```bash
sed -i '' 's/aliyun\.NewClientCache()/aliyun.NewClientCache[aliyun.CASClient]()/g' pkg/aliyun/cache_test.go
sed -i '' 's/casCache := aliyun\.NewClientCache()/casCache := aliyun.NewClientCache[aliyun.CASClient]()/' cmd/main.go
sed -i '' 's/cache \*aliyun\.ClientCache,/cache *aliyun.ClientCache[aliyun.CASClient],/' internal/controller/cas_factory.go
grep -rn 'NewClientCache\|ClientCache\[' pkg/aliyun/cache_test.go cmd/main.go internal/controller/cas_factory.go
```

Expected: 三个文件里再无裸的 `NewClientCache()` 或 `*aliyun.ClientCache`（不带类型参数）。

- [ ] **Step 7: 运行，确认通过**

```bash
go test ./pkg/aliyun/... -v -run 'TestCertConfig|TestUpdateCustomDomainInput|TestClientCache|TestLimitFC3'
make build && make test
```

Expected: 新增 5 个用例 PASS，原有 `pkg/aliyun` 与 `internal/controller` 测试不回归。

- [ ] **Step 8: 提交**

```bash
git add pkg/aliyun/ internal/controller/cas_factory.go cmd/main.go
git commit -m "feat: add FC3 narrow-interface types with key redaction and a typed client cache"
```

---

### Task 4: `pkg/aliyun/fc3_sdk.go` — 真实 FC3 client

**Files:**
- Create: `pkg/aliyun/fc3_sdk.go`
- Create: `pkg/aliyun/fc3_sdk_test.go`

**Interfaces:**
- Consumes：`aliyun.FC3Client` / `CustomDomain` / `UpdateCustomDomainInput` / `CertConfig` / `DomainEcho`（Task 3）；`aliyun.Classify` / `Error` / `Limiters` / `LimitFC3` / `callCode`（已存在，`callCode` 在 `cas_sdk.go` 中且是包内可见）。
- Produces：
  - `aliyun.FC3ClientConfig{Region, Endpoint string; Timeout time.Duration; Limiters *Limiters; LimiterKey string; OnCall func(action, code string, d time.Duration)}`
  - `aliyun.NewFC3Client(cred credential.Credential, cfg FC3ClientConfig) (FC3Client, error)`
  - 常量 `aliyun.ActionGetCustomDomain = "GetCustomDomain"`、`aliyun.ActionUpdateCustomDomain = "UpdateCustomDomain"`
  - 包内函数 `customDomainFromSDK(*fc.CustomDomain) *CustomDomain`、`updateInputToSDK(*UpdateCustomDomainInput) (*fc.UpdateCustomDomainInput, error)`（Task 4 的测试直接测这两个纯函数——真实 HTTP 调用无法在单测里覆盖）

- [ ] **Step 1: 写失败测试**

`pkg/aliyun/fc3_sdk_test.go`（**包内测试**，因为要测两个未导出的转换函数）：

```go
package aliyun

import (
	"strings"
	"testing"

	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
)

func TestCustomDomainFromSDK_DropsPrivateKey(t *testing.T) {
	auth := &fc.AuthConfig{}
	route := &fc.RouteConfig{}
	waf := &fc.WAFConfig{}
	body := &fc.CustomDomain{
		AccountId:  dara.String("1234567890"),
		DomainName: dara.String("api.example.com"),
		Protocol:   dara.String("HTTP,HTTPS"),
		CertConfig: &fc.CertConfig{
			CertName:    dara.String("cert_abc"),
			Certificate: dara.String("PUBLIC-CERT"),
			PrivateKey:  dara.String("SUPERSECRETKEYMATERIAL"),
		},
		AuthConfig:  auth,
		RouteConfig: route,
		WafConfig:   waf,
	}

	cd := customDomainFromSDK(body)

	if cd.AccountID != "1234567890" || cd.DomainName != "api.example.com" || cd.Protocol != "HTTP,HTTPS" {
		t.Fatalf("基础字段丢失: %+v", cd)
	}
	if cd.CertName != "cert_abc" || cd.CertificatePEM != "PUBLIC-CERT" {
		t.Fatalf("证书公开部分丢失: %+v", cd)
	}
	// 私钥必须在这一层就消失：整个结构体（含 Echo 的回填体）里都不该有它。
	echo, ok := cd.Echo.Payload.(*fc.UpdateCustomDomainInput)
	if !ok {
		t.Fatalf("Echo 应携带 SDK 写入体，得到 %T", cd.Echo.Payload)
	}
	if echo.CertConfig != nil {
		t.Fatal("回填体里绝不能带 certConfig —— 那是私钥唯一可能残留的地方")
	}
	if echo.AuthConfig != auth || echo.RouteConfig != route || echo.WafConfig != waf {
		t.Error("回填体必须原样保留 authConfig / routeConfig / wafConfig")
	}
	if strings.Contains(cd.CertificatePEM, "SUPERSECRET") {
		t.Fatal("私钥泄漏")
	}
}

func TestCustomDomainFromSDK_NoCertConfig(t *testing.T) {
	cd := customDomainFromSDK(&fc.CustomDomain{DomainName: dara.String("d"), Protocol: dara.String("HTTP")})
	if cd.CertName != "" || cd.CertificatePEM != "" {
		t.Errorf("无证书的域名应给出空值: %+v", cd)
	}
}

func TestUpdateInputToSDK_SetsCertOverEcho(t *testing.T) {
	route := &fc.RouteConfig{}
	echo := &fc.UpdateCustomDomainInput{RouteConfig: route, Protocol: dara.String("HTTP")}
	in := &UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &CertConfig{CertName: "n", CertPEM: []byte("C"), KeyPEM: []byte("K")},
		Echo:       DomainEcho{Payload: echo},
	}

	out, err := updateInputToSDK(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.RouteConfig != route {
		t.Error("回填体必须原样带上")
	}
	if dara.StringValue(out.Protocol) != "HTTP,HTTPS" {
		t.Errorf("protocol 应被覆盖: %v", dara.StringValue(out.Protocol))
	}
	if dara.StringValue(out.CertConfig.PrivateKey) != "K" {
		t.Errorf("私钥必须写进请求体: %+v", out.CertConfig)
	}
	// 覆盖必须作用在副本上，否则同一个 Echo 被两次 Apply 复用时会互相污染。
	if dara.StringValue(echo.Protocol) != "HTTP" || echo.CertConfig != nil {
		t.Error("不得就地修改 Echo 携带的原始写入体")
	}
}

func TestUpdateInputToSDK_ClearCertUsesExplicitEmptyStrings(t *testing.T) {
	out, err := updateInputToSDK(&UpdateCustomDomainInput{Protocol: "HTTP", ClearCert: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.CertConfig == nil {
		t.Fatal("解绑必须显式提交 certConfig，省略它在合并语义下等于不动")
	}
	for name, v := range map[string]*string{
		"certName": out.CertConfig.CertName, "certificate": out.CertConfig.Certificate, "privateKey": out.CertConfig.PrivateKey,
	} {
		if v == nil || *v != "" {
			t.Errorf("%s 应为显式空串，得到 %v", name, v)
		}
	}
}

func TestUpdateInputToSDK_RejectsForeignEcho(t *testing.T) {
	_, err := updateInputToSDK(&UpdateCustomDomainInput{Echo: DomainEcho{Payload: "not-an-sdk-body"}})
	if err == nil {
		t.Fatal("回填体类型不匹配必须报错，而不是静默丢掉整张路由表")
	}
	if ClassOf(err) != ClassPermanent {
		t.Errorf("接线错误应是 Permanent，得到 %v", ClassOf(err))
	}
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./pkg/aliyun/ -run 'TestCustomDomainFromSDK|TestUpdateInputToSDK'
```

Expected: FAIL，`undefined: customDomainFromSDK`。

- [ ] **Step 3: 实现**

`pkg/aliyun/fc3_sdk.go`：

```go
package aliyun

import (
	"context"
	"errors"
	"fmt"
	"time"

	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	fc "github.com/alibabacloud-go/fc-20230330/v4/client"
	"github.com/alibabacloud-go/tea/dara"
	credential "github.com/aliyun/credentials-go/credentials"
)

// FC3 的 OpenAPI 动作名。作为指标 label 使用，取值必须是常量。
const (
	ActionGetCustomDomain    = "GetCustomDomain"
	ActionUpdateCustomDomain = "UpdateCustomDomain"
)

// FC3ClientConfig 是构造真实 FC3 client 的参数，与 CASClientConfig 同构。
//
// 没有 ResourceGroupID：自定义域名不按资源组授权，FC 的 RAM 是逐域名 ARN（spec §8.3）。
type FC3ClientConfig struct {
	Region   string
	Endpoint string // 非空时覆盖 SDK 的 region → endpoint 映射
	Timeout  time.Duration
	Limiters *Limiters
	LimiterKey string
	// OnCall 在每次 OpenAPI 调用返回后被调用一次；可选，nil 表示不观测。
	OnCall func(action, code string, d time.Duration)
}

type sdkFC3 struct {
	c   *fc.Client
	cfg FC3ClientConfig
}

// NewFC3Client 用 SDK v4 构造 FC3Client。SDK 的 EndpointRule 是 regional，
// 会按 RegionId 自动选出 fcv3.<region>.aliyuncs.com。
func NewFC3Client(cred credential.Credential, cfg FC3ClientConfig) (FC3Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Limiters == nil {
		cfg.Limiters = NewLimiters()
	}
	ms := int(cfg.Timeout / time.Millisecond)
	connect := 5000
	oc := &openapiutil.Config{
		Credential:     cred,
		RegionId:       dara.String(cfg.Region),
		ReadTimeout:    &ms,
		ConnectTimeout: &connect,
		UserAgent:      dara.String("le-to-alicloud"),
	}
	if cfg.Endpoint != "" {
		oc.Endpoint = dara.String(cfg.Endpoint)
	}
	c, err := fc.NewClient(oc)
	if err != nil {
		return nil, Classify("NewFC3Client", err)
	}
	return &sdkFC3{c: c, cfg: cfg}, nil
}

func (s *sdkFC3) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

// observe 上报一次 OpenAPI 调用。err 必须是已经过 Classify 的错误。
func (s *sdkFC3) observe(action string, start time.Time, err error) {
	if s.cfg.OnCall == nil {
		return
	}
	s.cfg.OnCall(action, callCode(err), time.Since(start))
}

func (s *sdkFC3) GetCustomDomain(ctx context.Context, domain string) (*CustomDomain, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitFC3); err != nil {
		return nil, Classify(ActionGetCustomDomain, err)
	}
	cctx, cancel := s.withTimeout(ctx)
	defer cancel()
	start := time.Now()
	resp, err := s.c.GetCustomDomainWithContext(cctx, dara.String(domain), nil, &dara.RuntimeOptions{})
	cerr := Classify(ActionGetCustomDomain, err)
	s.observe(ActionGetCustomDomain, start, cerr)
	if cerr != nil {
		return nil, cerr
	}
	if resp == nil || resp.Body == nil {
		return nil, &Error{Class: ClassRetryable, Op: ActionGetCustomDomain, Code: "EmptyResponse", Err: errors.New("响应缺少 body")}
	}
	return customDomainFromSDK(resp.Body), nil
}

func (s *sdkFC3) UpdateCustomDomain(ctx context.Context, domain string, in *UpdateCustomDomainInput) error {
	if in == nil {
		return &Error{Class: ClassPermanent, Op: ActionUpdateCustomDomain, Code: "NilInput", Err: errors.New("写入体为空")}
	}
	body, err := updateInputToSDK(in)
	if err != nil {
		return err
	}
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitFC3); err != nil {
		return Classify(ActionUpdateCustomDomain, err)
	}
	cctx, cancel := s.withTimeout(ctx)
	defer cancel()
	start := time.Now()
	_, uerr := s.c.UpdateCustomDomainWithContext(cctx, dara.String(domain),
		&fc.UpdateCustomDomainRequest{Body: body}, nil, &dara.RuntimeOptions{})
	cerr := Classify(ActionUpdateCustomDomain, uerr)
	s.observe(ActionUpdateCustomDomain, start, cerr)
	return cerr
}

// customDomainFromSDK 把 SDK 响应裁剪成本项目的视图。
//
// 明文私钥就在 d.CertConfig.PrivateKey 里。这个函数是它在本进程中唯一被读到的位置，
// 而这里读它只是为了**不带走**：既不复制进 CustomDomain，也不放进 Echo。越过这一层
// 之后，上层代码想泄漏都拿不到。
func customDomainFromSDK(d *fc.CustomDomain) *CustomDomain {
	out := &CustomDomain{
		AccountID:  dara.StringValue(d.AccountId),
		DomainName: dara.StringValue(d.DomainName),
		Protocol:   dara.StringValue(d.Protocol),
	}
	if d.CertConfig != nil {
		out.CertName = dara.StringValue(d.CertConfig.CertName)
		out.CertificatePEM = dara.StringValue(d.CertConfig.Certificate)
	}
	// 回填体刻意不含 CertConfig：证书由调用方重新给定，私钥永不经过这里。
	out.Echo = DomainEcho{Payload: &fc.UpdateCustomDomainInput{
		AuthConfig:  d.AuthConfig,
		CorsConfig:  d.CorsConfig,
		RouteConfig: d.RouteConfig,
		TlsConfig:   d.TlsConfig,
		WafConfig:   d.WafConfig,
	}}
	return out
}

// updateInputToSDK 在 Echo 携带的回填体之上覆盖 protocol 与 certConfig。
//
// 必须先复制再覆盖：Echo 的 payload 是 Get 那一刻构造的对象，同一个 CustomDomain
// 可能被 Observe 与 Apply 先后使用，就地修改会让第二次读到第一次的残留。
func updateInputToSDK(in *UpdateCustomDomainInput) (*fc.UpdateCustomDomainInput, error) {
	base := &fc.UpdateCustomDomainInput{}
	if in.Echo.Payload != nil {
		p, ok := in.Echo.Payload.(*fc.UpdateCustomDomainInput)
		if !ok {
			return nil, &Error{
				Class: ClassPermanent, Op: ActionUpdateCustomDomain, Code: "InvalidEcho",
				Err: fmt.Errorf("回填体类型不匹配: %T", in.Echo.Payload),
			}
		}
		cp := *p
		base = &cp
	}
	if in.Protocol != "" {
		base.Protocol = dara.String(in.Protocol)
	}
	switch {
	case in.ClearCert:
		// 显式三个空串，而不是省略 certConfig：UpdateCustomDomain 是全量替换还是部分
		// 合并，文档没说（spec §2.1，§12.3 #2 列为必测）。省略在「合并」语义下等于
		// 「保持原样」，解绑就白做了；显式空串在两种语义下都表达「这里没有证书」。
		base.CertConfig = &fc.CertConfig{
			CertName: dara.String(""), Certificate: dara.String(""), PrivateKey: dara.String(""),
		}
	case in.CertConfig != nil:
		base.CertConfig = &fc.CertConfig{
			CertName:    dara.String(in.CertConfig.CertName),
			Certificate: dara.String(string(in.CertConfig.CertPEM)),
			PrivateKey:  dara.String(string(in.CertConfig.KeyPEM)),
		}
	}
	return base, nil
}

var _ FC3Client = (*sdkFC3)(nil)
```

- [ ] **Step 4: 运行，确认通过**

```bash
go test ./pkg/aliyun/ -v -run 'TestCustomDomainFromSDK|TestUpdateInputToSDK'
make build && make test
```

Expected: 5 个新用例 PASS。

- [ ] **Step 5: 提交**

```bash
git add pkg/aliyun/fc3_sdk.go pkg/aliyun/fc3_sdk_test.go
git commit -m "feat: implement the real FC3 client with rate limiting and private-key stripping"
```

---

### Task 5: `pkg/aliyun/fake/fc3.go` — 内存 fake 与故障注入

**Files:**
- Create: `pkg/aliyun/fake/fc3.go`
- Create: `pkg/aliyun/fake/fc3_test.go`

**Interfaces:**
- Consumes：`aliyun.FC3Client` / `CustomDomain` / `UpdateCustomDomainInput` / `CertConfig` / `DomainEcho` / `Error` / `ClassNotFound`（Task 3）；`aliyun.ActionGetCustomDomain` / `aliyun.ActionUpdateCustomDomain` / `aliyun.ClassPermanent`（Task 4，在 `fc3_sdk.go` 里产出——本 Task 必须排在 Task 4 之后）。
- Produces（Task 6 的 provider 单测与 Task 7–13 的 envtest 都用它）：
  - `fake.FC3`，构造 `fake.NewFC3() *FC3`
  - `fake.Domain{DomainName, Protocol, CertName string; CertPEM, KeyPEM []byte; Echo any}`（服务端侧快照，**含私钥**，因为它扮演的就是云）
  - `(f *FC3) SetAccountID(id string)`
  - `(f *FC3) AddDomain(d Domain)`；`(f *FC3) RemoveDomain(name string)`
  - `(f *FC3) Domain(name string) (Domain, bool)`
  - `(f *FC3) QueueGetErr(err error)`；`(f *FC3) QueueUpdateErr(err error)`
  - `(f *FC3) FailNextUpdateAfterCommit(err error)`
  - `(f *FC3) GetCalls() int`；`(f *FC3) UpdateCalls() int`（方法而非字段，持锁，与 `fake.CAS` 的 R10 约定一致）
  - `fake.ErrDomainNotFound`（`*aliyun.Error`，`ClassNotFound`，Code `DomainNameNotFound`）

- [ ] **Step 1: 写失败测试**

`pkg/aliyun/fake/fc3_test.go`：

```go
package fake_test

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func newFC3WithDomain() *fake.FC3 {
	f := fake.NewFC3()
	f.SetAccountID("1234567890")
	f.AddDomain(fake.Domain{
		DomainName: "api.example.com",
		Protocol:   "HTTP",
		Echo:       "route-table-marker",
	})
	return f
}

func TestFakeFC3_GetReturnsAccountAndEchoButNoKey(t *testing.T) {
	f := newFC3WithDomain()
	f.AddDomain(fake.Domain{
		DomainName: "tls.example.com", Protocol: "HTTPS",
		CertName: "c1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET"), Echo: "e2",
	})

	cd, err := f.GetCustomDomain(context.Background(), "tls.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cd.AccountID != "1234567890" || cd.Protocol != "HTTPS" || cd.CertName != "c1" {
		t.Fatalf("字段不对: %+v", cd)
	}
	if cd.CertificatePEM != "PUB" {
		t.Errorf("应返回公开证书: %q", cd.CertificatePEM)
	}
	if cd.Echo.Payload != "e2" {
		t.Errorf("Echo 应原样返回: %v", cd.Echo.Payload)
	}
	if f.GetCalls() != 1 {
		t.Errorf("GetCalls=%d", f.GetCalls())
	}
}

func TestFakeFC3_GetMissingDomain(t *testing.T) {
	f := newFC3WithDomain()
	_, err := f.GetCustomDomain(context.Background(), "nope.example.com")
	if aliyun.ClassOf(err) != aliyun.ClassNotFound {
		t.Fatalf("不存在的域名应是 ClassNotFound，得到 %v (%v)", aliyun.ClassOf(err), err)
	}
}

func TestFakeFC3_UpdateAppliesCertAndKeepsEcho(t *testing.T) {
	f := newFC3WithDomain()
	err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET")},
		Echo:       aliyun.DomainEcho{Payload: "route-table-marker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := f.Domain("api.example.com")
	if !ok {
		t.Fatal("域名不见了")
	}
	if d.Protocol != "HTTP,HTTPS" || d.CertName != "n1" || string(d.KeyPEM) != "SECRET" {
		t.Fatalf("写入未生效: %+v", d)
	}
	// read-modify-write 的核心断言：调用方必须把回填体原样带回来，否则路由表就没了。
	if d.Echo != "route-table-marker" {
		t.Errorf("回填体丢失: %v", d.Echo)
	}
	if f.UpdateCalls() != 1 {
		t.Errorf("UpdateCalls=%d", f.UpdateCalls())
	}
}

func TestFakeFC3_UpdateClearCert(t *testing.T) {
	f := newFC3WithDomain()
	f.AddDomain(fake.Domain{DomainName: "tls.example.com", Protocol: "HTTPS", CertName: "c1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET")})
	err := f.UpdateCustomDomain(context.Background(), "tls.example.com",
		&aliyun.UpdateCustomDomainInput{Protocol: "HTTP", ClearCert: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain("tls.example.com")
	if d.CertName != "" || len(d.CertPEM) != 0 || len(d.KeyPEM) != 0 {
		t.Fatalf("证书应被清空: %+v", d)
	}
	if d.Protocol != "HTTP" {
		t.Errorf("纯 HTTPS 域名解绑后应降为 HTTP: %s", d.Protocol)
	}
}

func TestFakeFC3_QueuedErrors(t *testing.T) {
	f := newFC3WithDomain()
	boom := errors.New("boom")
	f.QueueGetErr(boom)
	if _, err := f.GetCustomDomain(context.Background(), "api.example.com"); !errors.Is(err, boom) {
		t.Fatalf("排队的错误应被返回，得到 %v", err)
	}
	if _, err := f.GetCustomDomain(context.Background(), "api.example.com"); err != nil {
		t.Fatalf("队列耗尽后应恢复正常: %v", err)
	}

	f.QueueUpdateErr(boom)
	if err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{Protocol: "HTTP"}); !errors.Is(err, boom) {
		t.Fatalf("排队的错误应被返回，得到 %v", err)
	}
}

func TestFakeFC3_FailNextUpdateAfterCommit(t *testing.T) {
	f := newFC3WithDomain()
	lost := errors.New("response lost")
	f.FailNextUpdateAfterCommit(lost)

	err := f.UpdateCustomDomain(context.Background(), "api.example.com", &aliyun.UpdateCustomDomainInput{
		Protocol:   "HTTP,HTTPS",
		CertConfig: &aliyun.CertConfig{CertName: "n1", CertPEM: []byte("PUB"), KeyPEM: []byte("SECRET")},
	})
	if !errors.Is(err, lost) {
		t.Fatalf("应返回注入的错误: %v", err)
	}
	// 服务端已经改了，只是调用方不知道。这正是重试必须幂等的理由。
	d, _ := f.Domain("api.example.com")
	if d.CertName != "n1" {
		t.Fatal("提交后失败：写入必须已经生效")
	}
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./pkg/aliyun/fake/ -run TestFakeFC3
```

Expected: FAIL，`undefined: fake.NewFC3`。

- [ ] **Step 3: 实现**

`pkg/aliyun/fake/fc3.go`：

```go
package fake

import (
	"context"
	"errors"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrDomainNotFound 模拟 FC3 对不存在域名的 404。
//
// spec §12.3：未实测（FC3 对不存在的自定义域名返回的真实错误码与 HTTP 状态码未核实，
// 这里的 `DomainNameNotFound` 是猜的；若真实码既不含 `NotFound` 也不是 404，
// `aliyun.classifyCode` 会把它归成 `ClassPermanent`，`CodeTargetNotFound` 分支就永远走不到），
// 实测结论见 test/integration/RESULTS.md
var ErrDomainNotFound = &aliyun.Error{
	Class: aliyun.ClassNotFound, Op: aliyun.ActionGetCustomDomain,
	Code: "DomainNameNotFound", Err: errors.New("custom domain not found"),
}

// Domain 是 fake 里的一个自定义域名——服务端侧的完整状态，**含私钥**：
// 它扮演的就是云，云上确实存着私钥。上层拿到的 aliyun.CustomDomain 仍然不含私钥。
type Domain struct {
	DomainName string
	Protocol   string
	CertName   string
	CertPEM    []byte
	KeyPEM     []byte
	// Echo 是「本项目从不解释、但必须原样回填」的那部分配置的替身（真实世界里是
	// routeConfig / wafConfig 等）。测试断言它在一次 Apply 后仍然相等，就等于断言
	// read-modify-write 没有把别人的配置洗掉。
	Echo any
}

// FC3 实现 aliyun.FC3Client。所有状态都在 mu 之下；请通过 Domain / GetCalls /
// UpdateCalls 读取，这些方法持锁，可以安全地在 reconciler 与测试 goroutine 之间并发使用。
type FC3 struct {
	mu sync.Mutex

	accountID string
	domains   map[string]Domain

	getErrs    []error
	updateErrs []error

	failAfterCommit error

	getCalls    int
	updateCalls int
}

func NewFC3() *FC3 { return &FC3{domains: map[string]Domain{}} }

// SetAccountID 设置 GetCustomDomain 回报的账号（账号 fencing 用例要靠它切换账号）。
func (f *FC3) SetAccountID(id string) { f.mu.Lock(); f.accountID = id; f.mu.Unlock() }

// AddDomain 新建或覆盖一个域名。
func (f *FC3) AddDomain(d Domain) {
	f.mu.Lock()
	f.domains[d.DomainName] = d
	f.mu.Unlock()
}

// RemoveDomain 删除域名，用于模拟「域名被 Terraform 删掉了」。
func (f *FC3) RemoveDomain(name string) { f.mu.Lock(); delete(f.domains, name); f.mu.Unlock() }

// Domain 返回域名的服务端快照。
func (f *FC3) Domain(name string) (Domain, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[name]
	return d, ok
}

// QueueGetErr 让下一次 GetCustomDomain 返回该错误。
func (f *FC3) QueueGetErr(err error) { f.mu.Lock(); f.getErrs = append(f.getErrs, err); f.mu.Unlock() }

// QueueUpdateErr 让下一次 UpdateCustomDomain 在生效前返回该错误。
func (f *FC3) QueueUpdateErr(err error) {
	f.mu.Lock()
	f.updateErrs = append(f.updateErrs, err)
	f.mu.Unlock()
}

// FailNextUpdateAfterCommit 让下一次 Update 先写入（服务端成功）再返回 err（响应丢失）。
func (f *FC3) FailNextUpdateAfterCommit(err error) {
	f.mu.Lock()
	f.failAfterCommit = err
	f.mu.Unlock()
}

// GetCalls 返回 GetCustomDomain 的调用次数。持锁，可与 reconciler goroutine 并发调用。
func (f *FC3) GetCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.getCalls }

// UpdateCalls 返回 UpdateCustomDomain 的调用次数。持锁，可与 reconciler goroutine 并发调用。
func (f *FC3) UpdateCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.updateCalls }

func (f *FC3) GetCustomDomain(_ context.Context, domain string) (*aliyun.CustomDomain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if err := pop(&f.getErrs); err != nil {
		return nil, err
	}
	d, ok := f.domains[domain]
	if !ok {
		return nil, ErrDomainNotFound
	}
	// 与真实 client 一致：私钥不出这一层。
	return &aliyun.CustomDomain{
		AccountID:      f.accountID,
		DomainName:     d.DomainName,
		Protocol:       d.Protocol,
		CertName:       d.CertName,
		CertificatePEM: string(d.CertPEM),
		Echo:           aliyun.DomainEcho{Payload: d.Echo},
	}, nil
}

func (f *FC3) UpdateCustomDomain(_ context.Context, domain string, in *aliyun.UpdateCustomDomainInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	if err := pop(&f.updateErrs); err != nil {
		return err
	}
	d, ok := f.domains[domain]
	if !ok {
		return ErrDomainNotFound
	}
	if in == nil {
		return &aliyun.Error{Class: aliyun.ClassPermanent, Op: aliyun.ActionUpdateCustomDomain, Code: "NilInput", Err: errors.New("写入体为空")}
	}
	if in.Protocol != "" {
		d.Protocol = in.Protocol
	}
	switch {
	case in.ClearCert:
		d.CertName, d.CertPEM, d.KeyPEM = "", nil, nil
	case in.CertConfig != nil:
		d.CertName = in.CertConfig.CertName
		d.CertPEM = append([]byte(nil), in.CertConfig.CertPEM...)
		d.KeyPEM = append([]byte(nil), in.CertConfig.KeyPEM...)
	}
	// 回填体照单全收：调用方漏带就等于把它清成 nil，测试因此能抓到 read-modify-write
	// 少读了一次的 bug。
	d.Echo = in.Echo.Payload
	f.domains[domain] = d

	if f.failAfterCommit != nil {
		err := f.failAfterCommit
		f.failAfterCommit = nil
		return err
	}
	return nil
}

var _ aliyun.FC3Client = (*FC3)(nil)
```

`pop` 已由 `pkg/aliyun/fake/cas.go` 提供（同包，无需重复定义）。

- [ ] **Step 4: 运行，确认通过**

```bash
go test ./pkg/aliyun/fake/ -v -run TestFakeFC3
```

Expected: 6 个用例全 PASS。

- [ ] **Step 5: 提交**

```bash
make build && make test
git add pkg/aliyun/fake/fc3.go pkg/aliyun/fake/fc3_test.go
git commit -m "test: add an in-memory FC3 fake with fault injection"
```

---

### Task 6: `pkg/pki.LeafFingerprint` 与 `pkg/provider/fc3` — 第一个 provider

**Files:**
- Modify: `pkg/pki/bundle.go`
- Modify: `pkg/pki/bundle_test.go`
- Create: `pkg/provider/fc3/provider.go`
- Create: `pkg/provider/fc3/protocol.go`
- Create: `pkg/provider/fc3/errors.go`
- Create: `pkg/provider/fc3/protocol_test.go`
- Create: `pkg/provider/fc3/provider_test.go`
- Create: `docs/ram/binding-fc3-policy.json`

**Interfaces:**
- Consumes：`provider.Target/CertMaterial/ObservedState/Capabilities/ApplyOptions/Client/DeletionPolicy/Provider/ProviderError/Errorf/ErrorOf` 与错误码常量（Task 2）；`aliyun.FC3Client/CustomDomain/UpdateCustomDomainInput/CertConfig/Error/ClassOf/ClassNotFound/ClassAuth/ClassRetryable`（Task 3）；`fake.FC3/Domain`（Task 5）；`certsv1alpha1.TargetTypeFC3CustomDomain` 与 `Reason*` 常量（已存在）。
- Produces：
  - `pki.LeafFingerprint(certPEM []byte) (string, error)`
  - `fc3.Provider`（零值可用），`(*fc3.Provider) Name/Capabilities/Observe/Apply/Cleanup`；`init()` 中 `provider.Register(&Provider{})`
  - 包内：`protocolHasHTTPS(p string) bool`、`isHTTPSOnly(p string) bool`、常量 `protocolHTTP = "HTTP"`、`protocolBoth = "HTTP,HTTPS"`
  - 包内：`toProviderError(op string, err error, failReason string) error`（`failReason` 是「说不出更具体的话时」写进 condition 的 reason，Observe 与 Apply 传不同的值，bool 表达不了；`op` 作为错误消息前缀被真正用掉，见 Step 4）

- [ ] **Step 1: 写失败测试**

`pkg/pki/bundle_test.go` 追加：

```go
func TestLeafFingerprint_MatchesBundle(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "api.example.com")
	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pki.LeafFingerprint(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	// 两条路径必须给出同一个指纹：一条有私钥（证书侧），一条没有（云侧只能看到公开证书）。
	// 不相等就意味着 Binding 永远认为云上那张不是自己写的，于是每一轮都重写一次。
	if got != b.Fingerprint {
		t.Errorf("指纹不一致: %s != %s", got, b.Fingerprint)
	}
}

func TestLeafFingerprint_Garbage(t *testing.T) {
	if _, err := pki.LeafFingerprint([]byte("not a pem")); err == nil {
		t.Error("非 PEM 应报错")
	}
}
```

`pkg/provider/fc3/protocol_test.go`：

```go
package fc3

import "testing"

func TestProtocolPredicates(t *testing.T) {
	cases := []struct {
		in       string
		hasHTTPS bool
		only     bool
	}{
		{"HTTP", false, false},
		{"HTTPS", true, true},
		{"HTTP,HTTPS", true, false},
		{"https", true, true},
		{" HTTP , HTTPS ", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := protocolHasHTTPS(c.in); got != c.hasHTTPS {
			t.Errorf("protocolHasHTTPS(%q)=%t，期望 %t", c.in, got, c.hasHTTPS)
		}
		if got := isHTTPSOnly(c.in); got != c.only {
			t.Errorf("isHTTPSOnly(%q)=%t，期望 %t", c.in, got, c.only)
		}
	}
}
```

`pkg/provider/fc3/provider_test.go`：

```go
package fc3_test

import (
	"context"
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"
)

const testDomain = "api.example.com"

func targetFor() provider.Target {
	return provider.Target{
		Type:       certsv1alpha1.TargetTypeFC3CustomDomain,
		Region:     "cn-hangzhou",
		Identifier: testDomain,
		Spec:       &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: testDomain},
	}
}

func materialFor(t *testing.T) (provider.CertMaterial, string) {
	t.Helper()
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, testDomain)
	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	key, err := b.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	return provider.CertMaterial{
		Fingerprint: b.Fingerprint,
		CertPEM:     b.CertPEM(),
		KeyPEM:      key,
		CASName:     "cert_" + b.Fingerprint[:12],
		NotAfter:    b.Leaf.NotAfter,
		DNSNames:    b.DNSNames(),
	}, b.Fingerprint
}

func TestProvider_Identity(t *testing.T) {
	p := &fc3.Provider{}
	if p.Name() != certsv1alpha1.TargetTypeFC3CustomDomain {
		t.Errorf("Name 必须与 CRD 的 target.type 取值一致: %s", p.Name())
	}
	c := p.Capabilities()
	if c.ReferencesCertByID || c.RequiresCASUpload || !c.SupportsProtocolSwitch {
		t.Errorf("FC3 内联 PEM、不引用 certId、不强制上传 CAS、支持切协议: %+v", c)
	}
	if got, ok := provider.Get(certsv1alpha1.TargetTypeFC3CustomDomain); !ok || got.Name() != p.Name() {
		t.Error("init() 应把自己注册进 registry")
	}
}

func TestProvider_ObserveEmptyAndCertified(t *testing.T) {
	ctx := context.Background()
	m, fp := materialFor(t)
	f := fake.NewFC3()
	f.SetAccountID("1234567890")
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP", Echo: "routes"})
	p := &fc3.Provider{}

	obs, err := p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists || obs.CurrentFingerprint != "" || obs.Protocol != "HTTP" || obs.AccountID != "1234567890" {
		t.Fatalf("空证书域名的观测不对: %+v", obs)
	}

	if err := p.Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	obs, err = p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentFingerprint != fp {
		t.Errorf("写入后应观测到同一指纹: %s != %s", obs.CurrentFingerprint, fp)
	}
}

func TestProvider_ObserveTargetNotFound(t *testing.T) {
	f := fake.NewFC3()
	_, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), f)
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodeTargetNotFound {
		t.Fatalf("应映射为 CodeTargetNotFound: %v", err)
	}
	if pe.Retryable {
		t.Error("域名不存在不该被指数退避重试；通用层用固定 5m requeue")
	}
	if pe.Reason != certsv1alpha1.ReasonTargetNotFound {
		t.Errorf("Reason 应是 TargetNotFound: %s", pe.Reason)
	}
}

func TestProvider_ApplyPreservesEchoAndProtocol(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP", Echo: "routes"})

	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain(testDomain)
	if d.Echo != "routes" {
		t.Errorf("回填体丢失: %v", d.Echo)
	}
	if d.Protocol != "HTTP" {
		t.Errorf("EnsureHTTPSProtocol=false 时绝不能动 protocol: %s", d.Protocol)
	}
	if d.CertName != m.CASName || string(d.KeyPEM) != string(m.KeyPEM) {
		t.Errorf("证书未写入: %+v", d)
	}
}

func TestProvider_ApplyEnsureHTTPS(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP"})

	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, provider.ApplyOptions{EnsureHTTPSProtocol: true}); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.Domain(testDomain); d.Protocol != "HTTP,HTTPS" {
		t.Errorf("应升为 HTTP,HTTPS: %s", d.Protocol)
	}

	// 已经含 HTTPS 时不许改：把 "HTTPS" 改成 "HTTP,HTTPS" 等于替用户打开了明文入口。
	f2 := fake.NewFC3()
	f2.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS"})
	if err := (&fc3.Provider{}).Apply(ctx, targetFor(), f2, m, provider.ApplyOptions{EnsureHTTPSProtocol: true}); err != nil {
		t.Fatal(err)
	}
	if d, _ := f2.Domain(testDomain); d.Protocol != "HTTPS" {
		t.Errorf("已含 HTTPS 时不该改动: %s", d.Protocol)
	}
}

func TestProvider_ApplyErrorClasses(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	cases := []struct {
		name      string
		inject    error
		code      string
		retryable bool
		reason    string
	}{
		{"throttling", &aliyun.Error{Class: aliyun.ClassRetryable, Op: "UpdateCustomDomain", Code: "Throttling", Err: errAny}, provider.CodeThrottled, true, certsv1alpha1.ReasonThrottled},
		{"auth", &aliyun.Error{Class: aliyun.ClassAuth, Op: "UpdateCustomDomain", Code: "Forbidden", Err: errAny}, provider.CodeAuth, false, certsv1alpha1.ReasonCredentialsInvalid},
		{"permanent", &aliyun.Error{Class: aliyun.ClassPermanent, Op: "UpdateCustomDomain", Code: "InvalidParameter", Err: errAny}, provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fake.NewFC3()
			f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTP"})
			f.QueueUpdateErr(c.inject)
			err := (&fc3.Provider{}).Apply(ctx, targetFor(), f, m, provider.ApplyOptions{})
			pe := provider.ErrorOf(err)
			if pe == nil {
				t.Fatalf("应包成 ProviderError: %v", err)
			}
			if pe.Code != c.code || pe.Retryable != c.retryable || pe.Reason != c.reason {
				t.Errorf("分类错误: %+v", pe)
			}
		})
	}
}

func TestProvider_CleanupUnbind(t *testing.T) {
	ctx := context.Background()
	m, _ := materialFor(t)
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS", Echo: "routes"})
	p := &fc3.Provider{}
	if err := p.Apply(ctx, targetFor(), f, m, provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyUnbind); err != nil {
		t.Fatal(err)
	}
	d, _ := f.Domain(testDomain)
	if d.CertName != "" || len(d.KeyPEM) != 0 {
		t.Fatalf("证书应被清空: %+v", d)
	}
	if d.Protocol != "HTTP" {
		t.Errorf("纯 HTTPS 域名清掉证书后必须降为 HTTP，否则域名彻底不可用: %s", d.Protocol)
	}
	if d.Echo != "routes" {
		t.Errorf("解绑也要保住回填体: %v", d.Echo)
	}
}

func TestProvider_CleanupOrphanTouchesNothing(t *testing.T) {
	f := fake.NewFC3()
	f.AddDomain(fake.Domain{DomainName: testDomain, Protocol: "HTTPS", CertName: "c1"})
	if err := (&fc3.Provider{}).Cleanup(context.Background(), targetFor(), f, provider.DeletionPolicyOrphan); err != nil {
		t.Fatal(err)
	}
	if f.GetCalls() != 0 || f.UpdateCalls() != 0 {
		t.Errorf("Orphan 一次云调用都不该发生: get=%d update=%d", f.GetCalls(), f.UpdateCalls())
	}
}

func TestProvider_CleanupMissingDomainIsSuccess(t *testing.T) {
	f := fake.NewFC3()
	if err := (&fc3.Provider{}).Cleanup(context.Background(), targetFor(), f, provider.DeletionPolicyUnbind); err != nil {
		t.Errorf("域名已经不在了，解绑的目的已经达到: %v", err)
	}
}

func TestProvider_RejectsForeignClient(t *testing.T) {
	_, err := (&fc3.Provider{}).Observe(context.Background(), targetFor(), "not a client")
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodeInvalidClient {
		t.Fatalf("client 类型不对应报 CodeInvalidClient: %v", err)
	}
}
```

在同文件顶部补一个共用的哨兵错误：

```go
var errAny = errors.New("injected")
```

（记得 import `"errors"`。）

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./pkg/pki/ -run TestLeafFingerprint
go test ./pkg/provider/fc3/
```

Expected: 都 FAIL，`undefined: pki.LeafFingerprint` / `no required module provides package .../pkg/provider/fc3`。

- [ ] **Step 3: 实现 `pki.LeafFingerprint`**

`pkg/pki/bundle.go` 追加（复用已有的 `parseCertificates`）：

```go
// LeafFingerprint 从 PEM 中解析出第一张证书并返回 hex(sha256(DER))。
//
// 与 Bundle.Fingerprint 同源：云侧只能拿到公开证书（FC3 的 certConfig.certificate），
// 而证书侧是从 Secret 连私钥一起解析出来的。两条路径必须给出同一个值，否则「云上这张
// 是不是我写的」这个判断永远为假，operator 会每小时重写一次同样的内容。
func LeafFingerprint(certPEM []byte) (string, error) {
	certs, err := parseCertificates(certPEM)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}
```

- [ ] **Step 4: 实现 provider**

`pkg/provider/fc3/protocol.go`：

```go
package fc3

import "strings"

// FC3 的 protocol 字段取值（spec §2.1）。
const (
	protocolHTTP  = "HTTP"
	protocolHTTPS = "HTTPS"
	protocolBoth  = "HTTP,HTTPS"
)

// splitProtocol 把 "HTTP , HTTPS" 拆成大写的集合，容忍空白与大小写。
func splitProtocol(p string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(p, ",") {
		if s := strings.ToUpper(strings.TrimSpace(part)); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

func protocolHasHTTPS(p string) bool {
	_, ok := splitProtocol(p)[protocolHTTPS]
	return ok
}

// isHTTPSOnly 判断域名是否只开了 HTTPS。这样的域名一旦被解绑证书就彻底不可访问，
// 所以解绑时必须同时降到 HTTP。
func isHTTPSOnly(p string) bool {
	set := splitProtocol(p)
	_, https := set[protocolHTTPS]
	_, http := set[protocolHTTP]
	return https && !http
}
```

`pkg/provider/fc3/errors.go`：

```go
package fc3

import (
	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// toProviderError 把 aliyun 的错误分类翻译成通用层认识的形状。
//
// 通用层不认识任何 SDK 错误——这正是 provider 抽象的意义。翻译只在这一处发生，
// Reason 用的是 v1alpha1 的常量，好让 condition 的取值集合仍然是有界的。
//
// failReason 是「说不出更具体的话时」写进 condition 的 reason：Observe 传
// ReasonApplyFailed 没有意义，所以由调用方给。
//
// op 作为错误消息前缀（`op + ": " + …`）：一个 ProviderError 只说「Permanent」时
// 分不清是 Get 还是 Update 挂了，加上前缀才有诊断价值——顺带让 unparam 不再把它
// 报成未使用形参。消息里只有 op 与已分类的错误，绝不含任何 SDK 响应体：pkg/aliyun
// 的 fromSDKError 早已只保留 code / status，ProviderError.Error() 也不回显被包住的 err。
//
// 分类完全交给 aliyun.classifyCode，不在这里按错误码字面量做推测式兜底：
// 「Code 含 DomainName 且 Permanent 就当 TargetNotFound」之类的规则会把
// InvalidDomainName 这种真·永久错误误判成「域名还没建」，然后每 5 分钟空转一次。
// 若后续的集成测试发现「不存在的自定义域名」得到的 aliyun.ClassOf(err) != ClassNotFound，
// 那时回去修 aliyun.classifyCode（错误码归类的唯一落点），而不是在本文件加分支。
func toProviderError(op string, err error, failReason string) error {
	if err == nil {
		return nil
	}
	// withOp 只做前缀，不追加任何新内容；%w 保住 Unwrap 链，errors.As 仍能取到 *aliyun.Error。
	withOp := func(e error) error { return fmt.Errorf("%s: %w", op, e) }
	switch aliyun.ClassOf(err) {
	case aliyun.ClassNotFound:
		// 域名不存在不是瞬时故障：可能是 Terraform 还没建。Retryable=false，
		// 让通用层用固定 5m 的长 requeue 而不是指数退避（spec §6.2 步骤 5）。
		return withOp(provider.Errorf(provider.CodeTargetNotFound, false, certsv1alpha1.ReasonTargetNotFound, err))
	case aliyun.ClassAuth:
		return withOp(provider.Errorf(provider.CodeAuth, false, certsv1alpha1.ReasonCredentialsInvalid, err))
	case aliyun.ClassRetryable:
		if ae := asAliyunError(err); ae != nil && isThrottling(ae.Code) {
			return withOp(provider.Errorf(provider.CodeThrottled, true, certsv1alpha1.ReasonThrottled, err))
		}
		return withOp(provider.Errorf(provider.CodeRetryable, true, failReason, err))
	default:
		return withOp(provider.Errorf(provider.CodePermanent, false, failReason, err))
	}
}
```

`fmt` 要进本文件的 import（下面那个 import 块一并给出）。返回值仍是 `error` 而不是
`*provider.ProviderError`——外面统一用 `provider.ErrorOf(err)` 取，`fmt.Errorf` 的
`%w` 包装不影响 `errors.As`。

补上两个小助手（同文件）：

```go
import (
	"errors"
	"fmt"
	"strings"
)

func asAliyunError(err error) *aliyun.Error {
	var ae *aliyun.Error
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// isThrottling 沿用 CAS 的经验：阿里云的限流码都以 Throttling 开头。
//
// spec §12.3：未实测（FC3 的限流错误码是否同样以 `Throttling` 开头未核实；若不是，
// 被限流会落进 CodeRetryable 走指数退避而不是 CodeThrottled，指标里也看不到 throttled），
// 实测结论见 test/integration/RESULTS.md
func isThrottling(code string) bool { return strings.HasPrefix(code, "Throttling") }
```

**这两个 import 块最终要合并成一个**（`goimports` 会替你做，但计划里分成两段只是为了
就近展示）：`errors.go` 的最终 import 是 `errors` / `fmt` / `strings` 加三个内部包。

`pkg/provider/fc3/provider.go`：

```go
// Package fc3 把证书装到函数计算 3.0 的自定义域名上。
//
// 证书归属于域名，与函数无关（spec §2.1）：certConfig 与 routeConfig / wafConfig /
// tlsConfig 平级，域名是主键。因此本 provider 只做一件事——把 certConfig 换成我们的
// 证书，其余字段原样回填。
package fc3

import (
	"context"
	"errors"
	"fmt"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// Provider 是 spec §7 的第一个实现。无状态，零值可用。
type Provider struct{}

func init() { provider.Register(&Provider{}) }

func (p *Provider) Name() string { return certsv1alpha1.TargetTypeFC3CustomDomain }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		// FC3 内联 PEM，没有 certId 这个概念（Serverless Devs 的 certId 是客户端语法糖）。
		ReferencesCertByID:     false,
		SupportsProtocolSwitch: true,
		// 因此也不强制 CAS 上传：FC3-only 的用户可以完全不授 yundun-cert:*（spec §8.3）。
		RequiresCASUpload: false,
	}
}

// clientOf 断言通用层传进来的 client。失败是接线错误，不是运行时故障。
func clientOf(c provider.Client) (aliyun.FC3Client, error) {
	cl, ok := c.(aliyun.FC3Client)
	if !ok {
		return nil, provider.Errorf(provider.CodeInvalidClient, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 aliyun.FC3Client，得到 %T", c))
	}
	return cl, nil
}

func (p *Provider) Observe(ctx context.Context, t provider.Target, c provider.Client) (provider.ObservedState, error) {
	cl, err := clientOf(c)
	if err != nil {
		return provider.ObservedState{}, err
	}
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		return provider.ObservedState{}, toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}
	obs := provider.ObservedState{
		Exists:    true,
		Protocol:  cd.Protocol,
		AccountID: cd.AccountID,
	}
	if cd.CertificatePEM != "" {
		fp, ferr := pki.LeafFingerprint([]byte(cd.CertificatePEM))
		if ferr != nil {
			// 云上那张证书解析不了：不是我们写的，也无从比对。当成「没有证书」处理，
			// 通用层会照常 Apply 覆盖掉它。报错反而会让 Binding 永久卡住。
			return obs, nil
		}
		obs.CurrentFingerprint = fp
	}
	return obs, nil
}

func (p *Provider) Apply(ctx context.Context, t provider.Target, c provider.Client, m provider.CertMaterial, o provider.ApplyOptions) error {
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	if m.CASName == "" {
		// certConfig.certName 在 FC3 侧是必填。空名字会被服务端拒掉，而错误信息通常
		// 只说「参数不合法」——在这里挡住比在云上猜要便宜得多。
		return provider.Errorf(provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed,
			errors.New("CertMaterial.CASName 为空，无法填写 certConfig.certName"))
	}

	// read-modify-write：先读回完整对象，再把除 certConfig / protocol 之外的一切原样回填。
	// 这在「全量替换」和「部分合并」两种语义下都正确（spec §6.3）——而这两种到底是哪一种，
	// 文档没说（§12.3 #2）。代价是 Get 与 Update 之间没有乐观锁，是 last-write-wins；
	// 缓解是窗口极短、写入频率极低（正常一年 4–6 次）。
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		return toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}

	in := &aliyun.UpdateCustomDomainInput{
		Protocol: cd.Protocol,
		Echo:     cd.Echo,
		CertConfig: &aliyun.CertConfig{
			CertName: m.CASName,
			CertPEM:  m.CertPEM,
			KeyPEM:   m.KeyPEM,
		},
	}
	// 只在「用户要求」且「当前确实没开 HTTPS」时才动 protocol。已含 HTTPS 时保持原样：
	// 把 "HTTPS" 改成 "HTTP,HTTPS" 等于替用户打开了明文入口（spec §13「不越权改线上配置」）。
	if o.EnsureHTTPSProtocol && !protocolHasHTTPS(cd.Protocol) {
		in.Protocol = protocolBoth
	}
	if err := cl.UpdateCustomDomain(ctx, t.Identifier, in); err != nil {
		return toProviderError(aliyun.ActionUpdateCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}
	return nil
}

// Cleanup 在 deletionPolicy=Unbind 时清空 certConfig。
//
// 「只解绑自己的证书」这条判断不在这里：指纹比对需要 Binding 的 appliedFingerprint，
// 而 spec §7 的职责边界表把「幂等判断」留给 Observe、把状态比对留给通用层。通用层会
// 先 Observe、比对完再决定要不要调用 Cleanup（spec §6.5）。
func (p *Provider) Cleanup(ctx context.Context, t provider.Target, c provider.Client, policy provider.DeletionPolicy) error {
	if policy != provider.DeletionPolicyUnbind {
		// Orphan：云侧一动不动，连一次 Get 都不发。
		return nil
	}
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	cd, err := cl.GetCustomDomain(ctx, t.Identifier)
	if err != nil {
		if pe := provider.ErrorOf(toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonApplyFailed)); pe != nil && pe.Code == provider.CodeTargetNotFound {
			return nil // 域名已经不在了，解绑的目的已经达到
		}
		return toProviderError(aliyun.ActionGetCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}
	in := &aliyun.UpdateCustomDomainInput{Protocol: cd.Protocol, Echo: cd.Echo, ClearCert: true}
	// 纯 HTTPS 的域名被拿掉证书后会彻底无法访问，必须同时降到 HTTP。
	if isHTTPSOnly(cd.Protocol) {
		in.Protocol = protocolHTTP
	}
	if err := cl.UpdateCustomDomain(ctx, t.Identifier, in); err != nil {
		return toProviderError(aliyun.ActionUpdateCustomDomain, err, certsv1alpha1.ReasonApplyFailed)
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)
```

- [ ] **Step 5: 写 RAM 策略样例（spec §8.3 的 FC3 部分）**

`docs/ram/binding-fc3-policy.json`：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["fc:GetCustomDomain", "fc:UpdateCustomDomain"],
      "Resource": [
        "acs:fc:cn-hangzhou:<accountId>:custom-domains/api.timehorse.bestheme.ac.cn"
      ]
    }
  ]
}
```

在 `pkg/provider/fc3/provider.go` 的包注释末尾补一句指向它：

```go
// RAM：fc 支持资源级授权，必须用上——逐域名的
// acs:fc:{regionId}:{accountId}:custom-domains/{domainName}。样例见
// docs/ram/binding-fc3-policy.json。这与 yundun-cert:* 无法收窄形成对照（spec §8.3）。
```

- [ ] **Step 6: 运行，确认通过**

```bash
go test ./pkg/pki/ ./pkg/provider/... -v -run 'TestLeafFingerprint|TestProtocol|TestProvider'
make build && make test
```

Expected: 全 PASS。

- [ ] **Step 7: 提交**

```bash
git add pkg/pki/ pkg/provider/fc3/ docs/ram/
git commit -m "feat: implement the FC3 custom domain provider with read-modify-write"
```

---

### Task 7: 绑定 controller 骨架 — 索引、finalizer、证书查找、status 与指标、envtest 接线

**Files:**
- Modify: `api/v1alpha1/aliyuncertificatebinding_types.go`
- Modify: `internal/controller/indexes.go`
- Modify: `internal/controller/metrics.go`
- Modify: `internal/controller/cas_factory.go`
- Create: `internal/controller/binding_status.go`
- Create: `internal/controller/aliyuncertificatebinding_controller.go`
- Modify: `internal/controller/suite_test.go`（只加 `BeforeSuite` 里的绑定 reconciler 接线与该接线需要的 import）
- Create: `internal/controller/suite_fc3_test.go`（FC3 fake 全局变量与全部 binding 测试 helper）
- Create: `internal/controller/binding_basic_test.go`
- Modify: `config/rbac/role.yaml`（Step 9 的 `make manifests` 生成，Step 10 一并提交）

**Interfaces:**
- Consumes：`provider.Provider` / `provider.Client` / `provider.Target`（Task 2）；`certsv1alpha1.AliyunCertificateBinding` / `TargetKey()` / `IndexBindingByCertificate` / `ConditionApplied` / `ConditionConflict` / `ConditionReady` / `Reason*` / `FinalizerName`（已存在）；`condTrue` 与 `setCondition` 是证书专用的（参数类型是 `*AliyunCertificate`），Binding 需要自己的一套。
- Produces：
  - `certsv1alpha1.IndexBindingByTarget = "spec.target"`
  - `controller.ProviderFactory func(ctx context.Context, b *v1alpha1.AliyunCertificateBinding, ac *v1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error)`（Task 10 给出生产实现，本任务只定义类型并由测试注入）
  - `controller.AliyunCertificateBindingReconciler{Client; APIReader client.Reader; Scheme *runtime.Scheme; Recorder record.EventRecorder; ProviderFactory ProviderFactory; Now func() time.Time; DriftCheckInterval, CleanupGracePeriod time.Duration; CleanupFailurePolicy string}`；方法 `SetNow(func() time.Time)`、`now() time.Time`、`SetupWithManager(mgr) error`
  - `controller.bindingRound{b, orig *v1alpha1.AliyunCertificateBinding; provider string; lag time.Duration}`；`newBindingRound(b) *bindingRound`
  - `binding_status.go`：`setBindingCondition(b, condType string, status metav1.ConditionStatus, reason, message string)`、`bindingCondTrue(b, condType) bool`、`bindingCondReason(b, condType) string`、`(r) patchBinding(ctx, rd *bindingRound) error`、`aggregateBindingReady(b)`、`setBindingReadyFalse(b, reason, message string)`、`targetIdentifier(b) string`、`targetRegion(b) string`（两者 nil-safe，供日志与 label 使用）
  - `metrics.go`：`bindingReadyGauge`、`bindingConflictGauge`、`bindingAppliedAge`、`bindingApplyTotal`、`bindingDriftTotal`；`recordBindingMetrics(rd *bindingRound)`、`clearBindingMetrics(namespace, name, providerName string)`；`aliyunAPICallRecorder(service string) func(action, code string, d time.Duration)`；常量 `serviceFC3 = "fc"`
  - 测试 suite 全局（**全部落在新文件 `internal/controller/suite_fc3_test.go`**，`suite_test.go` 里只留 `BeforeSuite` 的接线）：`bindingReconciler *AliyunCertificateBindingReconciler`、`fakeFC3 *fake.FC3`、helper `currentFC3() *fake.FC3`、`resetFC3()`（**无返回值**）、`setFC3FactoryErr(err error)`、`createBinding(ctx, ns, name, certName, domain string, mutate func(*v1alpha1.AliyunCertificateBinding)) *v1alpha1.AliyunCertificateBinding`（domain 由调用方显式给，见 Step 8 的域名约定）、`createCertificate(ctx, ns, name string, dnsNames ...string)`、`bindingCond(ctx, ns, name, condType string) metav1.Condition`、`getBinding(ctx, ns, name) *v1alpha1.AliyunCertificateBinding`

- [ ] **Step 1: 写失败测试**

`internal/controller/binding_basic_test.go`：

```go
package controller

import (
	"context"
	"fmt"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

var _ = Describe("绑定 controller：骨架", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("证书不存在时 Ready=False/CertificateNotFound，且不碰云", func() {
		ns := newNamespace(ctx)
		createBinding(ctx, ns, "b1", "no-such-cert", fmt.Sprintf("b1.%s.example.com", ns), nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotFound
		})
		Expect(currentFC3().GetCalls()).To(BeZero())
	})

	It("加 finalizer 并写 observedGeneration", func() {
		ns := newNamespace(ctx)
		createBinding(ctx, ns, "b2", "no-such-cert", fmt.Sprintf("b2.%s.example.com", ns), nil)

		b := &certsv1alpha1.AliyunCertificateBinding{}
		eventually(func() bool {
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b2"}, b); err != nil {
				return false
			}
			return len(b.Finalizers) == 1 && b.Finalizers[0] == certsv1alpha1.FinalizerName &&
				b.Status.ObservedGeneration == b.Generation
		})
	})

	It("证书存在但 Issued=False 时 Ready=False/CertificateNotReady", func() {
		ns := newNamespace(ctx)
		// 只建 AliyunCertificate，不让它走到 Issued：不写 Secret，证书 controller 会
		// 停在 Issued=False/SecretNotFound。
		domain := fmt.Sprintf("b3.%s.example.com", ns)
		createCertificate(ctx, ns, "c1", domain)
		setCertificateStatus(ctx, ns, "c1", 1, cmmeta.ConditionFalse)
		createBinding(ctx, ns, "b3", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b3", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotReady
		})
		Expect(currentFC3().GetCalls()).To(BeZero())
	})

	It("同目标索引跳过空 TargetKey 而不是 panic", func() {
		// CEL 已经挡住了 type/内嵌块不一致的对象，但索引函数仍必须能安全地处理它——
		// 索引在 CRD 校验之前就会对每一个进 cache 的对象求值。
		b := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(b.TargetKey()).To(BeEmpty())
	})

	It("fake FC3 已接进 suite", func() {
		Expect(currentFC3()).To(BeAssignableToTypeOf(&fake.FC3{}))
	})
})
```

- [ ] **Step 2: 运行，确认失败**

```bash
make test 2>&1 | tail -20
```

Expected: 编译失败，`undefined: resetFC3` / `undefined: createBinding` / `undefined: createCertificate`。

- [ ] **Step 3: 追加索引常量与索引注册**

`api/v1alpha1/aliyuncertificatebinding_types.go`，在 `IndexBindingByCertificate` 旁边：

```go
// IndexBindingByTarget 是同目标冲突仲裁用的 field index 键名，值为 TargetKey()。
const IndexBindingByTarget = "spec.target"
```

`internal/controller/indexes.go` 整体替换：

```go
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// RegisterIndexes 注册所有 field index。必须且只能调用一次（main.go 与测试 suite 各一次）。
func RegisterIndexes(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByCertificate, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok || b.Spec.CertificateRef.Name == "" {
				return nil
			}
			return []string{b.Spec.CertificateRef.Name}
		}); err != nil {
		return err
	}
	// 同目标索引。空键必须跳过而不是索引成 ""：TargetKey() 对未知 type 或缺失内嵌块
	// 返回空串，把它们全都索引到同一个键上，会让一批毫无关系的 Binding 互相判定冲突。
	// CRD 的 CEL 已经挡住了这些形状，但索引函数在 cache 层运行，先于任何校验。
	return idx.IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByTarget, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok {
				return nil
			}
			key := b.TargetKey()
			if key == "" {
				return nil
			}
			return []string{key}
		})
}
```

- [ ] **Step 4: 追加 binding 指标并把 `OnCall` 按 service 参数化**

`internal/controller/metrics.go`：在 `var (...)` 块内追加五个指标，并把 `recordAliyunAPICall` 换成工厂函数。

两个新 gauge 的 label 用 `crLabels`（`var (` 之前那个 `[]string{"namespace", "name"}` 常量，
Plan 3 的第一个任务引入；本任务开工时 main 上已经有它，见 cross-plan 协调文件 §1）。
写字面量会让 `goconst` 复现，也破坏「label 顺序与 `WithLabelValues(ns, n)` 一致」这条保证。
`bindingAppliedAge` 多一个 `provider` label，与 `crLabels` 不同型，三个 label 保留字面量。

```go
	bindingReadyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_ready", Help: "1 if the binding Ready condition is True",
	}, crLabels)
	bindingConflictGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_conflict", Help: "1 if the binding Conflict condition is True",
	}, crLabels)
	// aliyuncert_binding_applied_age_seconds 是 spec §10.1 点名「必须告警」的那一个：
	// 独有的失败模式是「证书续期成功了，但没推到线上」。
	//
	// 语义是**滞后时长**（证书 CR 的 status.current 推进之后、本 Binding 尚未把该代
	// 应用到目标的持续时间；同步时为 0），而不是字面的「生效证书年龄」：后者在一切
	// 正常时也会随着证书服役时间一路涨到 90 天，而 spec §10.3 的告警式子是
	// `applied_age > 86400 and certificate_ready == 1`——用字面语义，每一张健康证书在
	// 第二天就会误报。指标名不变；spec §10.1 与 §10.3 的文字由 Plan 3 的 spec 对齐
	// 任务更新（team lead 裁决，2026-09-05）。
	bindingAppliedAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_binding_applied_age_seconds",
		Help: "Seconds the target has been behind the certificate's current generation; 0 when up to date",
	}, []string{"namespace", "name", "provider"})
	bindingApplyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_binding_apply_total", Help: "Binding apply attempts by provider and result",
	}, []string{"provider", "result"})
	bindingDriftTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_binding_drift_detected_total", Help: "Times the target certificate was found changed outside the operator",
	}, []string{"provider"})
```

`init()` 的 `MustRegister` 参数表追加这五个。`serviceCAS` 旁边追加：

```go
// service label 的取值。与阿里云的 RAM code 无关，取的是可读的服务简称。
const (
	serviceCAS = "cas"
	serviceFC3 = "fc"
)
```

把原来的 `recordAliyunAPICall` 换成工厂（`cas_factory.go` 里的 `OnCall: recordAliyunAPICall` 同步改为 `OnCall: aliyunAPICallRecorder(serviceCAS)`）：

```go
// aliyunAPICallRecorder 返回绑定到某个 service label 的 OnCall 钩子。
//
// 走回调而不是让 pkg/aliyun 直接注册指标：那是一个纯 SDK 封装包，不该依赖
// controller-runtime 的 metrics registry。label 基数由调用方保证有界——action 是包里的
// 常量，code 来自 aliyun.callCode（服务端错误码或固定字符串），两者都不含 Message、
// certId、指纹这类每次都不同的值。
func aliyunAPICallRecorder(service string) func(action, code string, d time.Duration) {
	return func(action, code string, d time.Duration) {
		aliyunAPIRequestsTotal.WithLabelValues(service, action, code).Inc()
		aliyunAPIDuration.WithLabelValues(service, action).Observe(d.Seconds())
	}
}

// recordBindingMetrics 在每次 status patch 前刷新 binding 侧 gauge。
func recordBindingMetrics(rd *bindingRound) {
	ns, n, p := rd.b.Namespace, rd.b.Name, rd.provider
	b2f := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	bindingReadyGauge.WithLabelValues(ns, n).Set(b2f(bindingCondTrue(rd.b, certsv1alpha1.ConditionReady)))
	bindingConflictGauge.WithLabelValues(ns, n).Set(b2f(bindingCondTrue(rd.b, certsv1alpha1.ConditionConflict)))
	bindingAppliedAge.WithLabelValues(ns, n, p).Set(rd.lag.Seconds())
}

// clearBindingMetrics 在 Binding 删除后移除 series，否则墓碑会一直告警下去。
func clearBindingMetrics(namespace, name, providerName string) {
	bindingReadyGauge.DeleteLabelValues(namespace, name)
	bindingConflictGauge.DeleteLabelValues(namespace, name)
	bindingAppliedAge.DeleteLabelValues(namespace, name, providerName)
}
```

- [ ] **Step 5: 写 `binding_status.go`**

```go
package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// bindingRound 是一轮 reconcile 的局部状态。
//
// 把「要落盘的对象」「patch 基准」「本轮算出的落后时长」收在一起，省得每个 helper 都
// 拖着四个参数走；也保证指标刷新与落盘的 status 永远是同一份判定（与证书侧
// patchStatus 里刷 gauge 的理由相同）。
type bindingRound struct {
	b    *certsv1alpha1.AliyunCertificateBinding
	orig *certsv1alpha1.AliyunCertificateBinding
	// provider 是指标 label，取 spec.target.type（有界枚举）。
	provider string
	// lag 是目标落后于证书当前代次的时长；已跟上时为 0。
	lag time.Duration
}

func newBindingRound(b *certsv1alpha1.AliyunCertificateBinding) *bindingRound {
	return &bindingRound{b: b, orig: b.DeepCopy(), provider: b.Spec.Target.Type}
}

// setBindingCondition 写入 condition，observedGeneration 取 CR 当前 generation。
func setBindingCondition(b *certsv1alpha1.AliyunCertificateBinding, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: b.Generation,
	})
}

func bindingCondTrue(b *certsv1alpha1.AliyunCertificateBinding, condType string) bool {
	return meta.IsStatusConditionTrue(b.Status.Conditions, condType)
}

func bindingCondReason(b *certsv1alpha1.AliyunCertificateBinding, condType string) string {
	if c := meta.FindStatusCondition(b.Status.Conditions, condType); c != nil {
		return c.Reason
	}
	return ""
}

// setBindingReadyFalse 用于「还没走到 Applied / Conflict 就已经确定不 Ready」的早退分支
// （证书不存在、材料无效、域名不覆盖……）。这些原因在 Applied / Conflict 里无处安放，
// 只能直接写进 Ready。
func setBindingReadyFalse(b *certsv1alpha1.AliyunCertificateBinding, reason, message string) {
	setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
}

// targetIdentifier 返回目标标识，供日志使用。**必须 nil-safe**：错误处置路径也会被
// 「target.type 不认识 / 内嵌块缺失」这类失败触发，而那正是 FC3CustomDomain 为 nil 的
// 时候，直接解引用会把一次配置错误变成 panic。
func targetIdentifier(b *certsv1alpha1.AliyunCertificateBinding) string {
	if b.Spec.Target.FC3CustomDomain != nil {
		return b.Spec.Target.FC3CustomDomain.DomainName
	}
	return b.TargetKey()
}

// targetRegion 同上，供 region label 使用；取不到时返回空串（label 允许空值）。
func targetRegion(b *certsv1alpha1.AliyunCertificateBinding) string {
	if b.Spec.Target.FC3CustomDomain != nil {
		return b.Spec.Target.FC3CustomDomain.Region
	}
	return ""
}

// aggregateBindingReady 实现 spec §6.2 步骤 8：Ready = Applied && !Conflict。
func aggregateBindingReady(b *certsv1alpha1.AliyunCertificateBinding) {
	applied := bindingCondTrue(b, certsv1alpha1.ConditionApplied)
	conflict := bindingCondTrue(b, certsv1alpha1.ConditionConflict)
	if applied && !conflict {
		setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionTrue, certsv1alpha1.ReasonApplied, "")
		return
	}
	// 冲突比「没写成」更能说明问题：写不进去正是因为不该由我们写。
	reason := certsv1alpha1.ReasonApplyFailed
	if conflict {
		reason = bindingCondReason(b, certsv1alpha1.ConditionConflict)
	} else if r := bindingCondReason(b, certsv1alpha1.ConditionApplied); r != "" {
		reason = r
	}
	setBindingCondition(b, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "")
}

// patchBinding 用 MergeFrom 提交 status，并在同一处刷新 gauge。
func (r *AliyunCertificateBindingReconciler) patchBinding(ctx context.Context, rd *bindingRound) error {
	recordBindingMetrics(rd)
	return r.Status().Patch(ctx, rd.b, client.MergeFrom(rd.orig))
}
```

- [ ] **Step 6: 写 controller 骨架**

`internal/controller/aliyuncertificatebinding_controller.go`：

```go
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// targetNotFoundRequeue 是「域名还不存在」时的固定重试间隔（spec §6.2 步骤 5）。
// 不用指数退避：域名可能由 Terraform 稍后创建，这是等待而不是故障。
const targetNotFoundRequeue = 5 * time.Minute

// credentialsRequeue 是凭证类错误的长 requeue：等人换 AK 或补授权，重试再快也没用。
const credentialsRequeue = 5 * time.Minute

// ProviderFactory 按 Binding 解析出 provider 与已经构造好的云 client。
//
// 凭证与 client 构造归通用层（spec §7 职责边界表），所以这一步在 controller 侧完成，
// provider 只收现成的 client。ac 用来继承凭证（Binding.credentialsRef 缺省时）。
type ProviderFactory func(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error)

// AliyunCertificateBindingReconciler 实现 spec §6 的绑定 controller。
type AliyunCertificateBindingReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder

	ProviderFactory ProviderFactory

	DriftCheckInterval   time.Duration
	CleanupGracePeriod   time.Duration
	CleanupFailurePolicy string

	// Now 是时钟注入点，nil 表示真实时间。运行中只许经 SetNow 改（与证书 controller 同构）。
	Now func() time.Time
	mu  sync.RWMutex
}

// SetNow 线程安全地替换时钟。
func (r *AliyunCertificateBindingReconciler) SetNow(fn func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Now = fn
}

func (r *AliyunCertificateBindingReconciler) now() time.Time {
	r.mu.RLock()
	fn := r.Now
	r.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates,verbs=get;list;watch

// Reconcile 实现 spec §6.2 的步骤。
func (r *AliyunCertificateBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	rd := newBindingRound(b)

	// 0. 删除分支（binding_deletion.go；Task 13 之前是存根）
	if !b.DeletionTimestamp.IsZero() {
		return r.reconcileBindingDelete(ctx, rd)
	}

	// finalizer 先加上，哪怕 deletionPolicy 现在是 Orphan：策略随时可以改成 Unbind，
	// 而没有 finalizer 的对象删除时我们根本收不到通知。
	if !controllerutil.ContainsFinalizer(b, certsv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(b, certsv1alpha1.FinalizerName)
		if err := r.Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
		// Update 会触发本对象的 watch 事件，不需要显式 requeue
		return ctrl.Result{}, nil
	}

	b.Status.ObservedGeneration = b.Generation

	// 1. 取证书
	ac := &certsv1alpha1.AliyunCertificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}, ac)
	switch {
	case apierrors.IsNotFound(err):
		// 不碰 Applied：目标上那张证书还在正常服役，证书 CR 不见了说明不了它有问题
		// （典型场景是 Argo CD 正在换名字重建）。只降 Ready，靠 watch 唤醒。
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotFound,
			fmt.Sprintf("AliyunCertificate %q 不存在", b.Spec.CertificateRef.Name))
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	case err != nil:
		return ctrl.Result{}, err
	}
	if !certIssued(ac) {
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotReady, "证书尚未通过校验")
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}

	return r.reconcileBindingReady(ctx, rd, ac)
}

// reconcileBindingReady 处理证书可用之后的步骤 2–8。Task 8–12 逐步填充。
func (r *AliyunCertificateBindingReconciler) reconcileBindingReady(
	ctx context.Context, rd *bindingRound, ac *certsv1alpha1.AliyunCertificate,
) (ctrl.Result, error) {
	aggregateBindingReady(rd.b)
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
}

// reconcileBindingDelete 是删除分支的存根，Task 13 替换。
func (r *AliyunCertificateBindingReconciler) reconcileBindingDelete(ctx context.Context, rd *bindingRound) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rd.b, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(rd.b, certsv1alpha1.FinalizerName)
	if err := r.Update(ctx, rd.b); err != nil {
		return ctrl.Result{}, err
	}
	clearBindingMetrics(rd.b.Namespace, rd.b.Name, rd.provider)
	return ctrl.Result{}, nil
}

// certIssued 判断证书 CR 是否已经拿到可用的材料。
//
// 看 Issued 而不是 Ready：Ready 还包含 Uploaded，而 FC3 内联 PEM，根本不需要 CAS 上传
// 成功（spec §3「上传 CAS 与绑定 FC3 是并行副作用，不是串行依赖」）。CAS 挂了不该
// 连带 HTTPS 也推不上去。
func certIssued(ac *certsv1alpha1.AliyunCertificate) bool {
	return meta.IsStatusConditionTrue(ac.Status.Conditions, certsv1alpha1.ConditionIssued)
}

// SetupWithManager 注册 watch：主资源，外加证书变化的反查。
func (r *AliyunCertificateBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certsv1alpha1.AliyunCertificateBinding{}).
		// 证书的 status.current 一变就要唤醒引用它的全部 Binding。用 field index 反查，
		// 而不是遍历：一个 namespace 里可能有几百个 Binding。
		Watches(&certsv1alpha1.AliyunCertificate{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				list := &certsv1alpha1.AliyunCertificateBindingList{}
				if err := mgr.GetClient().List(ctx, list,
					client.InNamespace(o.GetNamespace()),
					client.MatchingFields{certsv1alpha1.IndexBindingByCertificate: o.GetName()}); err != nil {
					return nil
				}
				out := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
						Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
					}})
				}
				return out
			})).
		Named("aliyuncertificatebinding").
		Complete(r)
}
```

`certIssued` 用到 `meta`，记得 import `"k8s.io/apimachinery/pkg/api/meta"`；`metav1` 暂时未用则删掉该 import（Task 8 会加回来）。

- [ ] **Step 7: 新建 `suite_fc3_test.go`，并在 `suite_test.go` 里只加接线**

FC3 的全局变量与**全部** binding 测试 helper 都进一个新文件
`internal/controller/suite_fc3_test.go`，`suite_test.go` 只留 `BeforeSuite` 里的接线。
这样做是为了把与 Plan 3 的合并冲突压到最小：Plan 3 的第一个任务会改 `suite_test.go`
里 `resetCAS()` 那一带，而那正是这些 helper 原本要插进去的位置；分文件之后两边不再
相邻，唯一预期会真报冲突的 hunk 就消失了。同包内共用 `fakeMu` / `k8sClient` 不受影响。

`internal/controller/suite_fc3_test.go`（新文件）：

```go
package controller

import (
	"context"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

// 与 currentCAS / resetCAS 同构，共用 suite_test.go 里的 fakeMu。
var (
	bindingReconciler *AliyunCertificateBindingReconciler
	fakeFC3           *fake.FC3
	fc3FactoryErr     error
)

// testAccountID 是 fake FC3 默认回报的账号，用于账号 fencing 用例。
const testAccountID = "1234567890"

// currentFC3 让每个测试可以替换 fakeFC3 而 manager 无需重启。
func currentFC3() *fake.FC3 { fakeMu.Lock(); defer fakeMu.Unlock(); return fakeFC3 }

// resetFC3 换上一个全新的 fake，并清掉上一轮注入的工厂错误。
//
// 无返回值：没有任何调用点用得上它，留着会被 unparam 报出来（与 resetCAS 一致）。
// 需要拿到 fake 的地方一律用 currentFC3()。
func resetFC3() {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fakeFC3 = fake.NewFC3()
	fakeFC3.SetAccountID(testAccountID)
	fc3FactoryErr = nil
}

// setFC3FactoryErr 让 ProviderFactory 直接失败，用来测凭证分支。
func setFC3FactoryErr(err error) { fakeMu.Lock(); fc3FactoryErr = err; fakeMu.Unlock() }

func currentFC3FactoryErr() error { fakeMu.Lock(); defer fakeMu.Unlock(); return fc3FactoryErr }
```

Step 8 的四个 helper（`createCertificate` / `createBinding` / `bindingCond` / `getBinding`）
也进这个文件，见下。

`internal/controller/suite_test.go` 里**只**在 `BeforeSuite` 的 `reconciler.SetupWithManager`
之后追加下面这一段（外加它需要的 import），不要在这个文件里加任何 helper 或全局变量：

```go
	resetFC3()
	bindingReconciler = &AliyunCertificateBindingReconciler{
		Client:    k8sManager.GetClient(),
		APIReader: k8sManager.GetAPIReader(),
		Scheme:    k8sManager.GetScheme(),
		Recorder:  k8sManager.GetEventRecorderFor("aliyuncertificatebinding"),
		ProviderFactory: func(context.Context, *certsv1alpha1.AliyunCertificateBinding, *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error) {
			if err := currentFC3FactoryErr(); err != nil {
				return nil, nil, err
			}
			return &fc3.Provider{}, currentFC3(), nil
		},
		DriftCheckInterval:   time.Hour,
		CleanupGracePeriod:   15 * time.Minute,
		CleanupFailurePolicy: CleanupPolicyAbandon,
	}
	Expect(bindingReconciler.SetupWithManager(k8sManager)).To(Succeed())
```

`suite_test.go` 的 import 追加 `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"`
与 `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"`（`context` / `time` 已有）。

- [ ] **Step 8: 写共用的测试 helper**

追加到 `internal/controller/suite_fc3_test.go` 末尾（**不要**放进 `binding_basic_test.go`：
后续 6 个测试文件都用它们，跟着 fake 的全局变量放在一起最省心，也让每个 binding 测试
文件都只剩用例、不再重复定义 helper，避开 `dupl`）：

```go
// createCertificate 建一个最小可用的 AliyunCertificate。
func createCertificate(ctx context.Context, ns, name string, dnsNames ...string) *certsv1alpha1.AliyunCertificate {
	ac := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: dnsNames},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun-credentials"},
				Region:         "cn-hangzhou",
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ac)).To(Succeed())
	return ac
}

// createBinding 建一个指向 FC3 自定义域名的 Binding。mutate 可为 nil。
//
// domain 一律由调用方显式传入，且必须带上 namespace：
// `fmt.Sprintf("%s.%s.example.com", bindingName, ns)`。仲裁是**跨 namespace** 按
// TargetKey() 检索的（spec §6.2），而 TargetKey() 只含 type/region/domainName，不含
// namespace——两个测试文件用同一个字面量域名，先建的那个 Binding 会一直把后建的判成
// Conflict，Applied 永远不为 True。helper 不给默认域名，就是为了逼调用方写出这一点。
func createBinding(ctx context.Context, ns, name, certName, domain string, mutate func(*certsv1alpha1.AliyunCertificateBinding)) *certsv1alpha1.AliyunCertificateBinding {
	b := &certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CertificateRef: certsv1alpha1.LocalObjectReference{Name: certName},
			Target: certsv1alpha1.BindingTarget{
				Type: certsv1alpha1.TargetTypeFC3CustomDomain,
				FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
					Region: "cn-hangzhou", DomainName: domain,
				},
			},
		},
	}
	if mutate != nil {
		mutate(b)
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
	return b
}

// bindingCond 读回一个 condition；不存在时返回零值。
func bindingCond(ctx context.Context, ns, name, condType string) metav1.Condition {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b); err != nil {
		return metav1.Condition{}
	}
	for _, c := range b.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

// getBinding 读回整个对象。
func getBinding(ctx context.Context, ns, name string) *certsv1alpha1.AliyunCertificateBinding {
	b := &certsv1alpha1.AliyunCertificateBinding{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b)).To(Succeed())
	return b
}
```

**注意命名冲突**：`retention_test.go` 里已有一个纯函数 helper 叫 `binding(gen, observed, applied)`。上面全部用 `createBinding` / `getBinding`，不要复用那个名字。

**envtest 域名约定（Task 7–13 全部用例都必须遵守）**：每个 Binding 的目标域名写成
`fmt.Sprintf("%s.%s.example.com", bindingName, ns)`（`newNamespace` 每个用例生成一个新
namespace，因此域名天然互不相交）。同一用例里 `createCertificate` 的 SAN、
`currentFC3().AddDomain` 的 `DomainName` 与 `createBinding` 的 domain 必须是同一个值——
先算进一个局部变量 `domain` 再三处引用，别抄三遍字面量。故意要验证「两个 Binding 抢同
一个目标」时（Task 8/12 的仲裁用例）才让两个 Binding 共用一个域名，那时也用胜者那一
方的名字来构造。

- [ ] **Step 9: 运行，确认通过**

```bash
make manifests generate
make test 2>&1 | tail -30
```

Expected: 新增 5 个 Ginkgo 用例 PASS；证书侧用例不回归。`make manifests` 会把新的 RBAC 规则写进 `config/rbac/role.yaml`。

- [ ] **Step 10: 提交**

```bash
git add api/ internal/controller/ config/rbac/
git commit -m "feat: add the binding controller skeleton with target index and binding metrics"
```

---

### Task 8: 同目标冲突仲裁

**Files:**
- Create: `internal/controller/binding_conflict.go`
- Create: `internal/controller/binding_conflict_test.go`（纯函数单测，不含 envtest）
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`

`internal/controller/binding_conflict_envtest_test.go` **不在本任务**：三个 envtest 用例要断言
`Applied=True`，Apply 到 Task 12 才落地，因此整个文件由 Task 12 创建。

**Interfaces:**
- Consumes：`bindingRound`、`setBindingCondition`、`aggregateBindingReady`、`patchBinding`（Task 7）；`certsv1alpha1.IndexBindingByTarget`、`TargetKey()`、`ConditionConflict`、`ReasonConflictingBinding`。
- Produces：
  - `pickWinner(items []certsv1alpha1.AliyunCertificateBinding) *certsv1alpha1.AliyunCertificateBinding`（纯函数，nil 表示没有候选者）
  - `lessBinding(a, b *certsv1alpha1.AliyunCertificateBinding) bool`
  - `(r *AliyunCertificateBindingReconciler) arbitrate(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding) (winner *certsv1alpha1.AliyunCertificateBinding, err error)`

- [ ] **Step 1: 写失败的单元测试**

`internal/controller/binding_conflict_test.go`：

```go
package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func candidate(name string, created time.Time, uid string, deleting bool) certsv1alpha1.AliyunCertificateBinding {
	b := certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
			UID:               types.UID(uid),
		},
	}
	if deleting {
		t := metav1.NewTime(created.Add(time.Hour))
		b.DeletionTimestamp = &t
		b.Finalizers = []string{certsv1alpha1.FinalizerName}
	}
	return b
}

func TestPickWinner_OldestWins(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("newer", t0.Add(time.Minute), "uid-a", false),
		candidate("older", t0, "uid-z", false),
	}
	w := pickWinner(items)
	if w == nil || w.Name != "older" {
		t.Fatalf("最早创建者应获胜: %+v", w)
	}
}

func TestPickWinner_TieBreaksOnUID(t *testing.T) {
	// creationTimestamp 只精确到秒，同一秒内建出来的两个 Binding 完全可能撞上。
	// 没有第二把钥匙的话，两边会各自认定自己是胜者并轮流覆写对方的证书。
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("b", t0, "uid-zzz", false),
		candidate("a", t0, "uid-aaa", false),
	}
	w := pickWinner(items)
	if w == nil || string(w.UID) != "uid-aaa" {
		t.Fatalf("同刻应按 UID 取最小: %+v", w)
	}
	// 顺序无关：同一批候选者无论以什么顺序出现，结论必须一致。
	items[0], items[1] = items[1], items[0]
	if w2 := pickWinner(items); string(w2.UID) != "uid-aaa" {
		t.Fatalf("排序不确定: %+v", w2)
	}
}

func TestPickWinner_SkipsDeleting(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{
		candidate("dying", t0, "uid-a", true),
		candidate("alive", t0.Add(time.Minute), "uid-b", false),
	}
	w := pickWinner(items)
	if w == nil || w.Name != "alive" {
		t.Fatalf("正在删除的不该继续占着目标: %+v", w)
	}
}

func TestPickWinner_AllDeleting(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	items := []certsv1alpha1.AliyunCertificateBinding{candidate("dying", t0, "uid-a", true)}
	if w := pickWinner(items); w != nil {
		t.Fatalf("没有活着的候选者时应返回 nil: %+v", w)
	}
}
```

**本任务不写 envtest。** 仲裁的 envtest 用例（`binding_conflict_envtest_test.go`）整体推迟
到 Task 12 创建：它们要断言 `Applied=True`，而 Apply 到 Task 12 才落地。写成
`Skip("等 Task 12")` 再回来解除的方案已被否决——计划里已经有四个跨任务红灯用例要跟踪，
少一个待办就少一次遗漏，而仲裁逻辑本身被上面四个纯函数单测完全覆盖。本任务交付的
只有 `binding_conflict.go` + `binding_conflict_test.go`。

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./internal/controller/ -run TestPickWinner
```

Expected: FAIL，`undefined: pickWinner`。

- [ ] **Step 3: 实现**

`internal/controller/binding_conflict.go`：

```go
package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// lessBinding 定义同目标 Binding 的全序：先比 creationTimestamp，再比 UID。
//
// 必须是全序而不只是「谁更早」：creationTimestamp 只精确到秒，同一秒内建出来的两个
// Binding 在两个副本眼里可能是不同的顺序，于是双方各自认定自己是胜者，轮流把对方的
// 证书覆盖掉。UID 是集群内唯一且不变的，作为第二把钥匙让结论与观察者无关。
func lessBinding(a, b *certsv1alpha1.AliyunCertificateBinding) bool {
	if !a.CreationTimestamp.Time.Equal(b.CreationTimestamp.Time) {
		return a.CreationTimestamp.Time.Before(b.CreationTimestamp.Time)
	}
	return a.UID < b.UID
}

// pickWinner 从同目标候选者里选出唯一允许写云的那一个；没有活着的候选者时返回 nil。
//
// 正在删除的不参与：它马上就要交出目标（Unbind 甚至已经在解绑了）。让它继续占着，
// 会把接班者卡在 Conflict 里直到 finalizer 走完。
func pickWinner(items []certsv1alpha1.AliyunCertificateBinding) *certsv1alpha1.AliyunCertificateBinding {
	var winner *certsv1alpha1.AliyunCertificateBinding
	for i := range items {
		c := &items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if winner == nil || lessBinding(c, winner) {
			winner = c
		}
	}
	return winner
}

// arbitrate 查出同目标的全部 Binding 并选出胜者。
//
// 用 informer cache（带 field index）而不是 live read：仲裁规则是确定性的，cache 短暂
// 落后最多让某一轮多判一次冲突，下一轮就会自愈；而 live read 要为每个 Binding 每小时
// 打一次 API server 的全量 List（APIReader 不支持自定义 index），代价大得多。
// 这与保留策略回收的 live read 不同——那里的 TOCTOU 会真的删掉在用的证书。
//
// 跨 namespace 一起比：同一个 FC3 域名在云上只有一份，两个 namespace 的 Binding 指向
// 它就是真冲突，不该因为 namespace 不同而被判成两件事。
//
// 已知限制：`--watch-namespaces` 生效时 cache 只覆盖被 watch 的 namespace，跨 namespace
// 仲裁随之退化为跨已 watch namespace 仲裁——cache 里看不见的 Binding 不会成为候选者。
// 代码上无法修（cache 就是那么大），只能在文档的「已知限制」里写明。
func (r *AliyunCertificateBindingReconciler) arbitrate(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding) (*certsv1alpha1.AliyunCertificateBinding, error) {
	key := b.TargetKey()
	if key == "" {
		// 索引不了的目标（CEL 已挡住这种形状）：当作只有自己，交给后续步骤去失败。
		return b, nil
	}
	list := &certsv1alpha1.AliyunCertificateBindingList{}
	if err := r.List(ctx, list, client.MatchingFields{certsv1alpha1.IndexBindingByTarget: key}); err != nil {
		return nil, err
	}
	w := pickWinner(list.Items)
	if w == nil {
		// cache 还没看到自己（刚创建的对象常见）。自己活着，就先按自己算。
		return b, nil
	}
	return w, nil
}
```

- [ ] **Step 4: 接进 Reconcile**

`aliyuncertificatebinding_controller.go` 的 `reconcileBindingReady` 开头插入步骤 2：

```go
func (r *AliyunCertificateBindingReconciler) reconcileBindingReady(
	ctx context.Context, rd *bindingRound, ac *certsv1alpha1.AliyunCertificate,
) (ctrl.Result, error) {
	b := rd.b

	// 2. 冲突仲裁（spec §6.2 步骤 2）
	winner, err := r.arbitrate(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if winner.UID != b.UID {
		setBindingCondition(b, certsv1alpha1.ConditionConflict, metav1.ConditionTrue,
			certsv1alpha1.ReasonConflictingBinding,
			fmt.Sprintf("同目标已由 %s/%s 绑定", winner.Namespace, winner.Name))
		aggregateBindingReady(b)
		// 云侧一个字节都不写。胜者消失会经 watch 唤醒我们，drift 周期是兜底。
		// 注意这里是一次早退，而 patchBinding → recordBindingMetrics 会用 rd.lag 刷
		// applied_age。Task 9 会把 rd.lag = r.appliedLag(b, ac) 提到 Reconcile 里取完
		// 证书之后、进本函数之前，所以这条路径上的 lag 是真值而不是 0；本任务里
		// appliedLag 还不存在，rd.lag 暂为 0，Task 9 落地后自动补齐。
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}
	setBindingCondition(b, certsv1alpha1.ConditionConflict, metav1.ConditionFalse,
		certsv1alpha1.ReasonApplied, "")

	aggregateBindingReady(b)
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
}
```

`SetupWithManager` 无需改动：同目标的其它 Binding 本来就是同一 kind 的主资源，`For` 已经在 watch 它们了。

- [ ] **Step 5: 运行，确认通过**

```bash
go test ./internal/controller/ -run TestPickWinner -v
make test 2>&1 | tail -20
```

Expected: 4 个单元用例 PASS；envtest 不回归。

- [ ] **Step 6: 提交**

```bash
git add internal/controller/binding_conflict.go internal/controller/binding_conflict_test.go internal/controller/aliyuncertificatebinding_controller.go
git commit -m "feat: arbitrate conflicting bindings deterministically by (creationTimestamp, UID)"
```

---

### Task 9: 证书材料装载与域名覆盖校验

**Files:**
- Create: `internal/controller/binding_material.go`
- Create: `internal/controller/binding_material_test.go`
- Create: `internal/controller/binding_material_envtest_test.go`
- Modify: `internal/controller/binding_status.go`（Step 4 往里加 `appliedLag`）
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`

**Interfaces:**
- Consumes：`secretNameFor(ac)`（`desired.go`，已存在）；`pki.ParseBundle` / `Bundle.CertPEM/KeyPEM/DNSNames/Fingerprint/Leaf`；`pki.Missing`；`naming.CASName`；`provider.CertMaterial`；`materialError{Reason, Message string}`（`material.go`，已存在，本任务复用）。
- Produces：
  - `loadBindingMaterial(ctx context.Context, reader client.Reader, ac *v1alpha1.AliyunCertificate) (provider.CertMaterial, *materialError)`
  - `requiredDomainsOf(b *v1alpha1.AliyunCertificateBinding) []string`
  - `checkDomainCoverage(m provider.CertMaterial, b *v1alpha1.AliyunCertificateBinding) *materialError`
  - `certificateGate(m provider.CertMaterial, ac *v1alpha1.AliyunCertificate) bool`
  - 常量 `certificateGateRequeue = 30 * time.Second`
  - `(r) appliedLag(b *v1alpha1.AliyunCertificateBinding, ac *v1alpha1.AliyunCertificate) time.Duration`（放 `binding_status.go`）

- [ ] **Step 1: 写失败的单元测试**

`internal/controller/binding_material_test.go`：

```go
package controller

import (
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

func bindingWithDomain(d string) *certsv1alpha1.AliyunCertificateBinding {
	return &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{
				Type:            certsv1alpha1.TargetTypeFC3CustomDomain,
				FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: d},
			},
		},
	}
}

func TestCheckDomainCoverage(t *testing.T) {
	cases := []struct {
		name    string
		sans    []string
		domain  string
		wantErr bool
	}{
		{"精确匹配", []string{"api.example.com"}, "api.example.com", false},
		{"通配符匹配", []string{"*.example.com"}, "api.example.com", false},
		{"通配符不跨层", []string{"*.example.com"}, "a.b.example.com", true},
		{"完全不覆盖", []string{"other.example.com"}, "api.example.com", true},
		{"大小写与尾点", []string{"API.Example.com."}, "api.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := provider.CertMaterial{DNSNames: c.sans}
			err := checkDomainCoverage(m, bindingWithDomain(c.domain))
			if c.wantErr != (err != nil) {
				t.Fatalf("覆盖判定错误: err=%v", err)
			}
			if err != nil && err.Reason != certsv1alpha1.ReasonDomainNotCovered {
				t.Errorf("reason 应是 DomainNotCovered: %s", err.Reason)
			}
		})
	}
}

func TestRequiredDomainsOf_UnknownTypeYieldsNothing(t *testing.T) {
	b := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{Type: "SomethingElse"},
		},
	}
	if got := requiredDomainsOf(b); len(got) != 0 {
		t.Errorf("未知 target 类型不该凭空造出域名: %v", got)
	}
}
```

`internal/controller/binding_material_envtest_test.go`：

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：证书材料", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("证书不覆盖目标域名时硬失败，不写云", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		// 故意签一张不覆盖 domain 的证书。
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, "other."+ns+".example.com")
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", "other."+ns+".example.com")
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonDomainNotCovered
		})
		// 推错证书 = 全站 TLS 报错（spec D17）。宁可停下也绝不写。
		Expect(currentFC3().UpdateCalls()).To(BeZero())
	})

	It("Secret 不见了时 Ready=False/SecretNotFound", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady).Reason != ""
		})

		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1-tls"}}
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		// 触发一次 reconcile：改 deletionPolicy 会推进 generation（spec.target 不可变）。
		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonSecretNotFound
		})
	})

	It("Secret 已换但证书 CR 未推进代次时不写 FC3", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		gen1Cert, gen1Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, gen1Cert, gen1Key)
		createBinding(ctx, ns, "b1", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		writes := currentFC3().UpdateCalls()

		// 让 CAS 上传持续失败，把证书 CR 钉在 gen1：Issued 仍是 True（Secret 校验通过），
		// 但 status.current 推不上去。这正是「Secret 已经是 gen2、证书 CR 还认着 gen1」
		// 那个窗口，只是被人为拉长到可断言的长度。
		for i := 0; i < 50; i++ {
			currentCAS().QueueUploadErr(&aliyun.Error{
				Class: aliyun.ClassPermanent, Op: "Upload", Code: "InvalidParameter",
				Err: errors.New("injected"),
			})
		}
		gen2Cert, gen2Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		simulateIssuance(ctx, ns, "c1", 2, gen2Cert, gen2Key)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCertificateNotReady
		})
		// gen2 绝不能在证书 controller 校验并认下它之前被推上云。
		Expect(currentFC3().UpdateCalls()).To(Equal(writes))
	})
})
```

（import 补 `"fmt"`、`corev1 "k8s.io/api/core/v1"`、`"errors"`、`"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"`。前两个用例中的「不写云」断言在 Task 12 之前也成立——那之前根本没有 Apply；**第三个用例断言 `Applied=True`，与 Task 8/11 的情形相同，要到 Task 12 才会转绿**，在 ledger 里记一笔。）

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./internal/controller/ -run 'TestCheckDomainCoverage|TestRequiredDomainsOf'
```

Expected: FAIL，`undefined: checkDomainCoverage`。

- [ ] **Step 3: 实现**

`internal/controller/binding_material.go`：

```go
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// certificateGateRequeue 是「Secret 已换、证书 CR 还没追上」时的等待间隔。
// 短到人察觉不出延迟，又不至于把 API server 打成忙音。
const certificateGateRequeue = 30 * time.Second

// loadBindingMaterial 组装要写进云的证书材料。
//
// 内容只来自 Secret，不来自 status（spec §3「写入内容的确定性」）：多副本短暂重叠时，
// 最坏情况是两次写入同样的字节，而不是把 A 的指纹配上 B 的 PEM。
//
// status.current 只用来补两样 Secret 里没有的东西：CAS 的 certId 与证书名。而且只在
// 指纹对得上时才用——续期在途时两者会短暂不一致，此时按 Secret 的指纹重新派生名字，
// 绝不把上一代的 certId 贴到新证书上。那种不一致的材料也不会被写出去，见 certificateGate。
func loadBindingMaterial(ctx context.Context, reader client.Reader, ac *certsv1alpha1.AliyunCertificate) (provider.CertMaterial, *materialError) {
	var m provider.CertMaterial

	s := &corev1.Secret{}
	name := secretNameFor(ac)
	err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return m, &materialError{certsv1alpha1.ReasonSecretNotFound, fmt.Sprintf("Secret %q 不存在", name)}
	}
	if err != nil {
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, "读取 Secret 失败: " + err.Error()}
	}
	b, perr := pki.ParseBundle(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if perr != nil {
		// pki 的错误只描述格式问题，不含密钥内容，可安全写进 condition。
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, perr.Error()}
	}
	keyPEM, kerr := b.KeyPEM()
	if kerr != nil {
		return m, &materialError{certsv1alpha1.ReasonSecretInvalid, kerr.Error()}
	}

	// spec §12.3：未实测（FC3 对私钥编码与证书链形状的接受面未核实——CertPEM 是
	// leaf + intermediates、无根、无空行的 LE 链，KeyPEM 是 PKCS#1/SEC1；FC3 是否要求
	// PKCS#8、是否要求带根、是否对顺序敏感都没有实测过。猜错的失效方式是静默的：
	// UpdateCustomDomain 直接返回一个参数类错误，被归成 Permanent），
	// 实测结论见 test/integration/RESULTS.md
	m = provider.CertMaterial{
		Fingerprint: b.Fingerprint,
		CertPEM:     b.CertPEM(),
		KeyPEM:      keyPEM,
		CASName:     naming.CASName(ac.Name, b.Fingerprint),
		NotAfter:    b.Leaf.NotAfter,
		DNSNames:    b.DNSNames(),
	}
	if cur := ac.Status.Current; cur != nil && cur.Fingerprint == b.Fingerprint {
		m.CertID = cur.CertID
		if cur.CASName != "" {
			m.CASName = cur.CASName
		}
	}
	return m, nil
}

// requiredDomainsOf 返回目标要求证书必须覆盖的域名。
func requiredDomainsOf(b *certsv1alpha1.AliyunCertificateBinding) []string {
	if b.Spec.Target.Type == certsv1alpha1.TargetTypeFC3CustomDomain && b.Spec.Target.FC3CustomDomain != nil {
		return []string{b.Spec.Target.FC3CustomDomain.DomainName}
	}
	return nil
}

// checkDomainCoverage 实施 spec §6.2 步骤 3：leaf SANs 必须按 RFC 6125 覆盖目标域名。
//
// 硬失败、不快速重试：推错证书会让整个域名的 TLS 报错（spec D17）。这不是「等一等就会
// 好」的故障，只有改 spec 才能修，所以退避再快也只是白烧配额。
func checkDomainCoverage(m provider.CertMaterial, b *certsv1alpha1.AliyunCertificateBinding) *materialError {
	required := requiredDomainsOf(b)
	if len(required) == 0 {
		return nil
	}
	if missing := pki.Missing(m.DNSNames, required); len(missing) > 0 {
		return &materialError{
			certsv1alpha1.ReasonDomainNotCovered,
			"证书未覆盖目标域名: " + strings.Join(missing, ", "),
		}
	}
	return nil
}

// certificateGate 判断证书 CR 是否已经认下 Secret 里的这一代。
//
// 写入内容由 Secret 决定（spec §3），但**要不要写这一次由证书 CR 说了算**。
// status.current 推进意味着证书 controller 已经跑完 spec §5.4 的全套校验：链连续、
// 公私钥匹配、不是签发中的临时自签、SANs 覆盖 spec 声明的域名。
//
// 续期在途时，cert-manager 先把新证书写进 Secret，证书 controller 稍后才校验并推进
// status.current。Binding 直读 Secret，天然比证书 CR 早一步看到新指纹——此刻抢先写云，
// 等于绕过那套校验把一张没人验过的证书推上生产 HTTPS。cert-manager 的临时自签证书正是
// 这样一张：它会先出现在 Secret 里，而证书 controller 会拒绝它。
//
// 所以不一致时就等：30 秒后再看，证书 controller 通常一轮就追平了。
func certificateGate(m provider.CertMaterial, ac *certsv1alpha1.AliyunCertificate) bool {
	cur := ac.Status.Current
	return cur != nil && cur.Fingerprint == m.Fingerprint
}
```

- [ ] **Step 4: 接进 Reconcile，并把 `rd.lag` 提到所有早退之前**

**先改 `Reconcile` 里取证书的那一段**：`rd.lag` 必须在任何早退之前算好。

理由是 `aliyuncert_binding_applied_age_seconds` 是 spec §10.1 唯一点名「必须告警」的绑定侧
指标，而 `recordBindingMetrics` 每次 patch 都会用 `rd.lag` 刷它。原来的写法把
`rd.lag = r.appliedLag(b, ac)` 排在材料装载与覆盖校验的早退**之后**，Task 8 的冲突分支
更在它之前就 return——于是「判定冲突 / Secret 丢了 / 证书不覆盖域名」这三种最该告警的
状态下 gauge 反而被刷成 0，spec §10.3 的 `AliyunCertificateBindingStale` 永远不触发。

改后的调用顺序固定为：Get binding → Get ac（NotFound 也继续走到算 lag）→
`rd.lag = r.appliedLag(b, ac)` → 仲裁 → 装载材料 → 覆盖校验 → `certificateGate` →
provider 工厂 → Observe → Apply。（`certificateGate` 必须排在装载材料之后：它比的是
`m.Fingerprint`，没有材料就没有指纹可比。）

```go
	// 1. 取证书。NotFound 不在这里 return——先把 lag 算了。
	ac := &certsv1alpha1.AliyunCertificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}, ac)
	switch {
	case apierrors.IsNotFound(err):
		ac = nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// 1b. 滞后时长在任何早退之前算好（见上面的理由）。ac 不存在、或 status.current
	// 为空时无从判断滞后，取 0——那两种状态由 aliyuncert_binding_ready=0 覆盖告警。
	rd.lag = r.appliedLag(b, ac)

	if ac == nil {
		// 不碰 Applied：目标上那张证书还在正常服役，证书 CR 不见了说明不了它有问题
		// （典型场景是 Argo CD 正在换名字重建）。只降 Ready，靠 watch 唤醒。
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotFound,
			fmt.Sprintf("AliyunCertificate %q 不存在", b.Spec.CertificateRef.Name))
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}
	if !certIssued(ac) {
		setBindingReadyFalse(b, certsv1alpha1.ReasonCertificateNotReady, "证书尚未通过校验")
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}

	return r.reconcileBindingReady(ctx, rd, ac)
```

**再在 `reconcileBindingReady` 的仲裁之后、`aggregateBindingReady` 之前插入步骤 3**
（此处不再算 lag，它已经在上面算过了）：

```go
	// 3. 装载材料并校验域名覆盖（spec §6.2 步骤 3）
	m, me := loadBindingMaterial(ctx, r.Client, ac)
	if me == nil {
		me = checkDomainCoverage(m, b)
	}
	if me != nil {
		// 不碰 Applied：目标上那张证书还在服役，Secret 出问题说明不了它有毛病
		// （与证书 controller 不清空 status.current 是同一条原则）。
		// rd.lag 早已算好，这次早退不会把 applied_age 刷成 0。
		setBindingReadyFalse(b, me.Reason, me.Message)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	// 3b. 证书 CR 必须已经认下 Secret 里的这一代，否则不写云（见 certificateGate）。
	if !certificateGate(m, ac) {
		setBindingCondition(b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse,
			certsv1alpha1.ReasonCertificateNotReady, "证书 CR 尚未认下 Secret 中的这一代")
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: certificateGateRequeue}, r.patchBinding(ctx, rd)
	}
```

这一步之后，`m.Fingerprint` 与 `ac.Status.Current.Fingerprint` 必然相等——Task 11 的幂等短路与 Task 12 的 `appliedFingerprint` 固化因此可以放心地把 `m.Fingerprint` 当作「证书当前代次」。

并追加 `appliedLag`（放在 `binding_status.go`）：

```go
// appliedLag 返回目标滞后于证书当前代次的时长；已同步或无从判断时为 0。
//
// 这就是 aliyuncert_binding_applied_age_seconds 的取值来源。用「滞后多久」而不是
// 「生效证书有多老」：后者在一切正常时也会一路涨到证书有效期那么长，会让 spec §10.3
// 的告警式子对每一张健康证书误报。语义已由 team lead 裁决（2026-09-05）。
//
// ac 允许为 nil（证书 CR 不存在）：调用点在取证书之后、任何早退之前，那里 ac 可能没取到。
func (r *AliyunCertificateBindingReconciler) appliedLag(
	b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate,
) time.Duration {
	if ac == nil {
		return 0
	}
	cur := ac.Status.Current
	if cur == nil || cur.Fingerprint == "" || b.Status.AppliedFingerprint == cur.Fingerprint {
		return 0
	}
	lag := r.now().Sub(cur.UploadedAt.Time)
	if lag < 0 {
		return 0
	}
	return lag
}
```

- [ ] **Step 5: 运行，确认通过**

```bash
go test ./internal/controller/ -run 'TestCheckDomainCoverage|TestRequiredDomainsOf' -v
make test 2>&1 | tail -20
```

Expected: 单元用例全 PASS；前两个 envtest 用例 PASS；「Secret 已换但证书 CR 未推进代次」要到 Task 12 才转绿（它以 `Applied=True` 为前置）。

- [ ] **Step 6: 提交**

```bash
git add internal/controller/binding_material.go internal/controller/binding_material_test.go internal/controller/binding_material_envtest_test.go internal/controller/aliyuncertificatebinding_controller.go internal/controller/binding_status.go
git commit -m "feat: load binding material from the Secret, gate on status.current and enforce SAN coverage"
```

---

### Task 10: 凭证继承与 `ProviderFactory` 生产实现

**Files:**
- Create: `internal/controller/provider_factory.go`
- Create: `internal/controller/provider_factory_test.go`
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`

**Interfaces:**
- Consumes：`ProviderFactory` 类型（Task 7）；`credentialsError{Reason string; Err error}`（`cas_factory.go`，已存在）；`aliyun.CredentialsFromSecret` / `Credentials.Build` / `LimiterKey`；`aliyun.ClientCache[aliyun.FC3Client]` / `ClientKey` / `NewFC3Client` / `FC3ClientConfig` / `Limiters`；`provider.Get`；`aliyunAPICallRecorder(serviceFC3)`。
- Produces：
  - `controller.credentialsSecretNameFor(b *v1alpha1.AliyunCertificateBinding, ac *v1alpha1.AliyunCertificate) string`
  - `controller.targetOf(b *v1alpha1.AliyunCertificateBinding) (provider.Target, error)`
  - `controller.NewProviderFactory(reader client.Reader, cache *aliyun.ClientCache[aliyun.FC3Client], limiters *aliyun.Limiters, timeout time.Duration) ProviderFactory`

- [ ] **Step 1: 写失败测试**

`internal/controller/provider_factory_test.go`：

```go
package controller

import (
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func TestCredentialsSecretNameFor_InheritsFromCertificate(t *testing.T) {
	ac := &certsv1alpha1.AliyunCertificate{
		Spec: certsv1alpha1.AliyunCertificateSpec{
			Aliyun: certsv1alpha1.AliyunSpec{CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "cas-cred"}},
		},
	}
	b := &certsv1alpha1.AliyunCertificateBinding{}
	if got := credentialsSecretNameFor(b, ac); got != "cas-cred" {
		t.Errorf("缺省应继承证书的凭证: %s", got)
	}

	b.Spec.CredentialsRef = &certsv1alpha1.LocalSecretReference{Name: "fc-cred"}
	if got := credentialsSecretNameFor(b, ac); got != "fc-cred" {
		t.Errorf("Binding 自己的凭证应优先: %s", got)
	}
}

func TestCredentialsSecretNameFor_NoCertificate(t *testing.T) {
	// 删除分支里证书可能已经不在了，此时只有 Binding 自己的 credentialsRef 可用。
	b := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CredentialsRef: &certsv1alpha1.LocalSecretReference{Name: "fc-cred"},
		},
	}
	if got := credentialsSecretNameFor(b, nil); got != "fc-cred" {
		t.Errorf("证书缺失时应仍能取到自己的凭证: %s", got)
	}
	if got := credentialsSecretNameFor(&certsv1alpha1.AliyunCertificateBinding{}, nil); got != "" {
		t.Errorf("两边都没有时应返回空串: %q", got)
	}
}

func TestTargetOf(t *testing.T) {
	b := bindingWithDomain("api.example.com")
	tg, err := targetOf(b)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Type != certsv1alpha1.TargetTypeFC3CustomDomain || tg.Region != "cn-hangzhou" || tg.Identifier != "api.example.com" {
		t.Fatalf("Target 不对: %+v", tg)
	}
	if tg.Spec != b.Spec.Target.FC3CustomDomain {
		t.Error("Spec 应指向 CRD 内嵌结构体本身，provider 才能断言出 ensureHTTPSProtocol")
	}

	bad := &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{Target: certsv1alpha1.BindingTarget{Type: "Nope"}},
	}
	if _, err := targetOf(bad); err == nil {
		t.Error("未知 target 类型应报错")
	}
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
go test ./internal/controller/ -run 'TestCredentialsSecretNameFor|TestTargetOf'
```

Expected: FAIL，`undefined: credentialsSecretNameFor`。

- [ ] **Step 3: 实现**

`internal/controller/provider_factory.go`：

```go
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"

	// 让 fc3 的 init() 把自己注册进 registry。除注册外本包不直接引用它——
	// 这正是 provider 抽象的意义：新增 provider = 加一个包 + 一条 CEL，不动状态机。
	_ "git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/fc3"
)

// credentialsSecretNameFor 实现凭证继承：Binding 自己的 credentialsRef 优先，
// 缺省回退到证书的 aliyun.credentialsRef（spec §6.2 步骤 4）。
//
// 两者都在 Binding 所在的 namespace（CRD 里没有 namespace 字段，用类型系统禁止跨
// namespace 引用）。ac 允许为 nil：删除分支里证书可能已经先被删掉了。
func credentialsSecretNameFor(b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate) string {
	if b.Spec.CredentialsRef != nil && b.Spec.CredentialsRef.Name != "" {
		return b.Spec.CredentialsRef.Name
	}
	if ac != nil {
		return ac.Spec.Aliyun.CredentialsRef.Name
	}
	return ""
}

// targetOf 把 CRD 的 discriminated union 摊平成 provider 无关的 Target。
func targetOf(b *certsv1alpha1.AliyunCertificateBinding) (provider.Target, error) {
	switch b.Spec.Target.Type {
	case certsv1alpha1.TargetTypeFC3CustomDomain:
		fc := b.Spec.Target.FC3CustomDomain
		if fc == nil {
			return provider.Target{}, fmt.Errorf("target.type=%s 但缺少 fc3CustomDomain", b.Spec.Target.Type)
		}
		return provider.Target{
			Type:       b.Spec.Target.Type,
			Region:     fc.Region,
			Identifier: fc.DomainName,
			Spec:       fc,
		}, nil
	default:
		return provider.Target{}, fmt.Errorf("未知的 target.type %q，已注册: %v", b.Spec.Target.Type, provider.Names())
	}
}

// NewProviderFactory 构造生产用 ProviderFactory。
//
// 与 NewCASFactory 同构：读同 namespace 的凭证 Secret（Secret 已从 cache 禁用，
// mgr.GetClient() 对它就是直读），按 resourceVersion 缓存 client——否则轮换后的 AK
// 直到 Pod 重启才生效。
func NewProviderFactory(reader client.Reader, cache *aliyun.ClientCache[aliyun.FC3Client], limiters *aliyun.Limiters, timeout time.Duration) ProviderFactory {
	return func(ctx context.Context, b *certsv1alpha1.AliyunCertificateBinding, ac *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error) {
		tg, err := targetOf(b)
		if err != nil {
			return nil, nil, err
		}
		p, ok := provider.Get(tg.Type)
		if !ok {
			return nil, nil, fmt.Errorf("没有注册 target.type %q 的 provider，已注册: %v", tg.Type, provider.Names())
		}

		secretName := credentialsSecretNameFor(b, ac)
		if secretName == "" {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound,
				fmt.Errorf("既未设置 spec.credentialsRef，也取不到证书的 aliyun.credentialsRef")}
		}
		s := &corev1.Secret{}
		gerr := reader.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: secretName}, s)
		if apierrors.IsNotFound(gerr) {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound,
				fmt.Errorf("凭证 Secret %q 不存在", secretName)}
		}
		if gerr != nil {
			return nil, nil, gerr
		}
		creds, cerr := aliyun.CredentialsFromSecret(s)
		if cerr != nil {
			return nil, nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, cerr}
		}

		// key 必须囊括 build 闭包里读到的每一个会改变 client 行为的字段。
		// Endpoint 恒为空：AliyunCertificate 的 endpointOverride 是给 CAS 用的，
		// 把它套到 FC3 上会把请求打到数字证书服务的地址去。FC3 的 endpoint 一律由
		// SDK 按 region 选出（fcv3.<region>.aliyuncs.com）。
		key := aliyun.ClientKey{
			Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion,
			Region: tg.Region,
		}
		cl, berr := cache.GetOrBuild(key, func() (aliyun.FC3Client, error) {
			cred, err := creds.Build()
			if err != nil {
				return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
			}
			// 一律读 key 而不是 tg：缓存命中与否只由 key 决定，闭包里再从别处取值
			// 就等于把没进 key 的字段偷偷带进 client。
			return aliyun.NewFC3Client(cred, aliyun.FC3ClientConfig{
				Region:     key.Region,
				Timeout:    timeout,
				Limiters:   limiters,
				LimiterKey: creds.LimiterKey(),
				OnCall:     aliyunAPICallRecorder(serviceFC3),
			})
		})
		if berr != nil {
			return nil, nil, berr
		}
		return p, cl, nil
	}
}
```

- [ ] **Step 4: 接进 Reconcile**

在 `reconcileBindingReady` 的域名覆盖之后插入步骤 4，并追加一个错误处置 helper（放在 `aliyuncertificatebinding_controller.go`）：

```go
	// 4. 解析凭证并构造 provider client（spec §6.2 步骤 4）
	p, cl, err := r.ProviderFactory(ctx, b, ac)
	if err != nil {
		return r.handleFactoryError(ctx, rd, err)
	}
	tg, err := targetOf(b)
	if err != nil {
		setBindingReadyFalse(b, certsv1alpha1.ReasonApplyFailed, err.Error())
		return ctrl.Result{}, r.patchBinding(ctx, rd)
	}
	_ = p
	_ = cl
	_ = tg
```

（`_ =` 三行在 Task 11 被真正的 `Observe` 调用替换。）

```go
// handleFactoryError 处置「连 client 都没造出来」的失败。
//
// 凭证类错误不可重试：AK 被吊销、Secret 写错了 key，重试再快也没用，等的是人改配置。
// 长 requeue 5m 而不是指数退避，退避到几十分钟反而会让「改好了却迟迟不生效」。
func (r *AliyunCertificateBindingReconciler) handleFactoryError(ctx context.Context, rd *bindingRound, err error) (ctrl.Result, error) {
	var ce *credentialsError
	if errors.As(err, &ce) {
		setBindingReadyFalse(rd.b, ce.Reason, ce.Error())
		return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
	}
	// 剩下的是接线错误（未知 target.type、没注册 provider）：同样等 spec 改动。
	setBindingReadyFalse(rd.b, certsv1alpha1.ReasonApplyFailed, err.Error())
	return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
}
```

`aliyuncertificatebinding_controller.go` 的 import 追加 `"errors"`（`handleFactoryError` 用
`errors.As`）。这一步不能推到 Task 11——本任务收尾就要跑 `make build`，少了它编译不过。

- [ ] **Step 5: 运行，确认通过**

```bash
go test ./internal/controller/ -run 'TestCredentialsSecretNameFor|TestTargetOf' -v
make build && make test 2>&1 | tail -20
```

Expected: 3 个单元用例 PASS，envtest 不回归。

- [ ] **Step 6: 提交**

```bash
git add internal/controller/provider_factory.go internal/controller/provider_factory_test.go internal/controller/aliyuncertificatebinding_controller.go
git commit -m "feat: resolve provider clients with credential inheritance and per-resourceVersion caching"
```

---

### Task 11: `Observe` — 幂等短路、账号 fencing、drift 检测

**Files:**
- Modify: `api/v1alpha1/conditions.go`（Binding reasons 块新增 `ReasonObserveFailed`）
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`
- Create: `internal/controller/binding_observe_test.go`

**Interfaces:**
- Consumes：`provider.Provider.Observe`、`provider.ObservedState`、`provider.ErrorOf`、`provider.Code*`（Task 2、6）；`bindingRound`、`setBindingCondition`、`aggregateBindingReady`、`bindingCondReason`（Task 7）。
- Produces：
  - `certsv1alpha1.ReasonObserveFailed = "ObserveFailed"`（放进 `api/v1alpha1/conditions.go` 的「AliyunCertificateBinding reasons」块，紧跟 `ReasonApplyFailed`）
  - `(r) handleObserveError(ctx, rd *bindingRound, err error) (ctrl.Result, error)`
  - `(r) fenceAccount(rd *bindingRound, obs provider.ObservedState) bool`（true 表示被拦下）
  - `(r) noteDrift(ctx, rd *bindingRound, obs provider.ObservedState, m provider.CertMaterial)`
  - `(r) eventOnReasonChange(rd *bindingRound, condType, reason, eventType, eventReason, message string)`（Task 12 的 `handleApplyError` 复用）
  - `noteObserveFailed(b *v1alpha1.AliyunCertificateBinding)`
  - 常量 `observeFailedMessage`、`driftCorrectedMessage`、`appliedMessage`、`applyFailedMessage`
  - `protocolSatisfied(obs provider.ObservedState, want bool) bool`

**先改 `api/v1alpha1/conditions.go`**：在「AliyunCertificateBinding reasons」的 `const` 块里，
`ReasonApplyFailed` 之后加一行

```go
	ReasonObserveFailed       = "ObserveFailed"
```

理由：这个 reason 会出现在 Warning 事件上，事件 reason 的取值集合必须仍然可枚举——
放在 controller 包内当私有常量，spec §10.2 的事件全表与 README 就收录不到它。
（Plan 3 的 spec 对齐任务会把它写进事件表，已记进 cross-plan 协调文件。）

- [ ] **Step 1: 写失败测试**

`internal/controller/binding_observe_test.go`：

```go
package controller

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// issueAndBind 建证书 + 域名 + Binding，并等到 Applied=True。后续用例的公共前置。
//
// domain 由调用方给，一律写成 fmt.Sprintf("%s.%s.example.com", bindingName, ns)：
// 仲裁是跨 namespace 的，域名撞车会让后建的 Binding 一直停在 Conflict（见 Task 7 Step 8）。
// Task 13 的删除用例也用这个 helper（跨文件依赖，同包）。
func issueAndBind(ctx context.Context, ns, certName, bindingName, domain, protocol string) {
	ca := testutil.NewCA(GinkgoT())
	certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
	currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: protocol, Echo: "routes"})
	createCertificate(ctx, ns, certName, domain)
	simulateIssuance(ctx, ns, certName, 1, certPEM, keyPEM)
	createBinding(ctx, ns, bindingName, certName, domain, nil)
	eventually(func() bool {
		return bindingCond(ctx, ns, bindingName, certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
	})
}

var _ = Describe("绑定 controller：Observe", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("域名不存在时 Ready=False/TargetNotFound", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		// 故意不 AddDomain：目标域名在云上还不存在。
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonTargetNotFound
		})
		Expect(currentFC3().UpdateCalls()).To(BeZero())
	})

	It("指纹一致时短路：不写云，但仍然 Observe", func() {
		ns := newNamespace(ctx)
		issueAndBind(ctx, ns, "c1", "b1", fmt.Sprintf("b1.%s.example.com", ns), "HTTP")

		writes := currentFC3().UpdateCalls()
		getsBefore := currentFC3().GetCalls()

		// 推一次 reconcile（spec.target 不可变，改 deletionPolicy 推进 generation）。
		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyOrphan
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool { return currentFC3().GetCalls() > getsBefore })
		// 短路只跳过写，不跳过读（spec §3「level-triggered」）。
		Expect(currentFC3().UpdateCalls()).To(Equal(writes))
		Expect(getBinding(ctx, ns, "b1").Status.LastObservedTime).NotTo(BeNil())
	})

	It("账号变了就 fencing：Conflict=True/AccountMismatch 且不写", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTP")
		Expect(getBinding(ctx, ns, "b1").Status.BoundAccountID).To(Equal(testAccountID))

		writes := currentFC3().UpdateCalls()
		// 同一个域名在另一个账号下：AK 被换成了别人的，再写就是在写别人的资源。
		currentFC3().SetAccountID("9999999999")
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		b := getBinding(ctx, ns, "b1")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonAccountMismatch
		})
		Expect(bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(currentFC3().UpdateCalls()).To(Equal(writes))
	})

	It("云侧被人换了证书时判定为 drift 并纠正", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTP")
		writes := currentFC3().UpdateCalls()

		// 有人手工把证书换成了另一张。指纹既不是 appliedFingerprint 也不是 current。
		otherCA := testutil.NewCA(GinkgoT())
		otherPEM, _ := testutil.IssueLeaf(GinkgoT(), otherCA, domain)
		d, _ := currentFC3().Domain(domain)
		d.CertName = "someone-elses"
		d.CertPEM = otherPEM
		currentFC3().AddDomain(d)

		b := getBinding(ctx, ns, "b1")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		eventually(func() bool { return currentFC3().UpdateCalls() > writes })
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		// 必须重新读回来再断言：d 是写入之前抓的本地副本，对它断言恒真，测不到
		// read-modify-write 有没有把 routeConfig 之类的旁路字段原样回填。
		d2, _ := currentFC3().Domain(domain)
		Expect(d2.Echo).To(Equal("routes"))
		Expect(d2.CertName).NotTo(Equal("someone-elses"))
	})

	It("Observe 未知失败属于旁路：不降级 Applied", func() {
		ns := newNamespace(ctx)
		issueAndBind(ctx, ns, "c1", "b1", fmt.Sprintf("b1.%s.example.com", ns), "HTTP")

		// 一个说不出「目标有没有问题」的错误。若因此把 Applied 打成 False，
		// Ready 也会掉，运维会以为线上 HTTPS 坏了——而它好好的。
		currentFC3().QueueGetErr(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
			Code: "InternalError", Err: errors.New("boom"),
		})
		b := getBinding(ctx, ns, "b1")
		b.Annotations = map[string]string{"poke": "1"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		Consistently(func() metav1.ConditionStatus {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status
		}, "2s", "200ms").Should(Equal(metav1.ConditionTrue))
	})
})
```

- [ ] **Step 2: 运行，确认失败**

```bash
make test 2>&1 | tail -30
```

Expected: 新增用例 FAIL（`Applied` 从不变成 True，因为还没有 Observe / Apply）。

- [ ] **Step 3: 实现事件文案与判定 helper**

`aliyuncertificatebinding_controller.go` 追加：

```go
// 事件文案。spec §10.2 要求 Message 不含变量——K8s 只聚合 Reason+Message 完全相同的
// 事件，带上域名或指纹就等于每个对象各刷一条，很快把 etcd 里的事件淹掉。变量只进日志。
const (
	observeFailedMessage  = "failed to observe the binding target; the applied state is unchanged"
	driftCorrectedMessage = "target certificate was changed outside the operator; re-applying"
	appliedMessage        = "certificate applied to the binding target"
	applyFailedMessage    = "failed to apply the certificate to the binding target"
)

// protocolSatisfied 判断当前 protocol 是否已经满足 ensureHTTPSProtocol 的要求。
//
// ensureHTTPSProtocol=false 时永远满足：spec §13 明说不越权改线上配置，用户没要求就
// 不看这一项，否则每一轮都会因为「没开 HTTPS」而重写一次。
func protocolSatisfied(obs provider.ObservedState, ensureHTTPS bool) bool {
	if !ensureHTTPS {
		return true
	}
	for _, part := range strings.Split(obs.Protocol, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "HTTPS") {
			return true
		}
	}
	return false
}

// handleObserveError 处置 Observe 的失败。
//
// 分两档，是「旁路失败不降级」这条原则的落点：
//   - TargetNotFound / Auth：这两件事直接说明「写不进去」——域名不存在、AK 被吊销。
//     照常把 Applied 与 Ready 打成 False，并用固定长 requeue（域名可能由 Terraform
//     稍后创建；AK 等人来换），不做指数退避。
//   - 其余：一次 InternalError 或网络抖动说明不了目标上那张证书有任何问题。碰
//     condition 就是用一个无害的失败换来一场真实的告警（控制器裁决 R21/R24/R25）。
//     只发 Warning 事件、保留既有判定，Retryable 的交给 controller-runtime 退避。
func (r *AliyunCertificateBindingReconciler) handleObserveError(ctx context.Context, rd *bindingRound, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pe := provider.ErrorOf(err)
	switch {
	case pe != nil && pe.Code == provider.CodeTargetNotFound:
		setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, pe.Reason, "目标不存在")
		aggregateBindingReady(rd.b)
		return ctrl.Result{RequeueAfter: targetNotFoundRequeue}, r.patchBinding(ctx, rd)
	case pe != nil && pe.Code == provider.CodeAuth:
		setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, pe.Reason, "凭证被拒绝")
		aggregateBindingReady(rd.b)
		return ctrl.Result{RequeueAfter: credentialsRequeue}, r.patchBinding(ctx, rd)
	}
	log.Error(err, "Observe 失败", "domain", targetIdentifier(rd.b))
	// 只在 reason 相对上一轮变化时发事件（spec §10.2「只在状态跃迁时发」）：旁路失败
	// 每一轮都会重来，每轮发一条 Warning 就是在刷事件表。
	r.eventOnReasonChange(rd, certsv1alpha1.ConditionApplied, certsv1alpha1.ReasonObserveFailed,
		corev1.EventTypeWarning, certsv1alpha1.ReasonObserveFailed, observeFailedMessage)
	// Applied 的 status 一个字节都不改——那才是「旁路失败不降级」的含义。只把 reason
	// 换成 ObserveFailed：kubectl describe 因此看得出「证书还生效着，但上一轮观测失败」，
	// 上面那个跃迁判断也才有可比较的痕迹。
	noteObserveFailed(rd.b)
	aggregateBindingReady(rd.b)
	if perr := r.patchBinding(ctx, rd); perr != nil {
		return ctrl.Result{}, perr
	}
	if pe != nil && pe.Retryable {
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	}
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, nil
}

// eventOnReasonChange 实现 spec §10.2 的「只在状态跃迁时发」。
//
// 判据是 rd.orig——本轮开始前从 API server 读到的那一份，也就是「上一轮」的结论。
// 同一个 condition 的 reason 没变就不发：失败会一轮一轮地重来，每轮一条事件不但没有
// 新信息，还会把这个对象上真正的跃迁淹掉。
func (r *AliyunCertificateBindingReconciler) eventOnReasonChange(
	rd *bindingRound, condType, reason, eventType, eventReason, message string,
) {
	if bindingCondReason(rd.orig, condType) == reason {
		return
	}
	r.Recorder.Event(rd.b, eventType, eventReason, message)
}

// noteObserveFailed 把 Applied 的 reason 改成 ObserveFailed，status 不动。
//
// 只在 condition 已经存在时改：从没 Applied 过的对象上凭空造一个 Applied=False，
// 就把旁路失败变成了真降级，正是这条路径要避免的事。status 不变，
// metav1.SetStatusCondition 的 lastTransitionTime 语义也不受影响（这里直接改字段，
// 不走 SetStatusCondition，免得它按「新 condition」处理）。
func noteObserveFailed(b *certsv1alpha1.AliyunCertificateBinding) {
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == certsv1alpha1.ConditionApplied {
			b.Status.Conditions[i].Reason = certsv1alpha1.ReasonObserveFailed
			b.Status.Conditions[i].Message = "上一轮观测失败，保留既有判定"
			return
		}
	}
}

// fenceAccount 实现账号 fencing（spec §6.2 步骤 5）：status.boundAccountId 一旦固化，
// 观测到的账号就必须一直是它。返回 true 表示被拦下、本轮不许写。
//
// 触发场景是凭证 Secret 被换成了另一个账号的 AK，而同名域名恰好也存在于那个账号。
// 没有这道闸，operator 会安静地把证书写进陌生人的资源。
func (r *AliyunCertificateBindingReconciler) fenceAccount(rd *bindingRound, obs provider.ObservedState) bool {
	bound := rd.b.Status.BoundAccountID
	if bound == "" || obs.AccountID == "" || bound == obs.AccountID {
		return false
	}
	setBindingCondition(rd.b, certsv1alpha1.ConditionConflict, metav1.ConditionTrue,
		certsv1alpha1.ReasonAccountMismatch, "目标所属账号与首次绑定时不一致")
	return true
}

// noteDrift 在观测到「既不是我们上次写的、也不是当前该写的」证书时记一笔。
//
// 判定要求 obs.CurrentFingerprint 非空：空证书是「还没绑过」，不是漂移。
// 等于 appliedFingerprint 是正常轮换（我们写的那张还在，只是证书续期了）。
func (r *AliyunCertificateBindingReconciler) noteDrift(ctx context.Context, rd *bindingRound, obs provider.ObservedState, m provider.CertMaterial) {
	cur := obs.CurrentFingerprint
	if cur == "" || cur == rd.b.Status.AppliedFingerprint || cur == m.Fingerprint {
		return
	}
	bindingDriftTotal.WithLabelValues(rd.provider).Inc()
	logf.FromContext(ctx).Info("检测到云侧证书漂移",
		"domain", targetIdentifier(rd.b),
		"observed", shortFP(cur), "expected", shortFP(m.Fingerprint))
	r.Recorder.Event(rd.b, corev1.EventTypeWarning, certsv1alpha1.ReasonDriftCorrected, driftCorrectedMessage)
}
```

（import 追加 `"strings"`、`corev1 "k8s.io/api/core/v1"`、`logf "sigs.k8s.io/controller-runtime/pkg/log"`。`"errors"` 已在 Task 10 Step 4 加过，不要重复。）

- [ ] **Step 4: 接进 Reconcile**

把 Task 10 留下的三行 `_ =` 替换为步骤 5：

```go
	// 5. Observe（spec §6.2 步骤 5）
	obs, oerr := p.Observe(ctx, tg, cl)
	if oerr != nil {
		return r.handleObserveError(ctx, rd, oerr)
	}
	b.Status.LastObservedTime = &metav1.Time{Time: r.now()}

	if r.fenceAccount(rd, obs) {
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	// 走到这里 targetOf 已经确认过 FC3CustomDomain 非 nil，可以安全解引用。
	ensureHTTPS := b.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol
	if obs.CurrentFingerprint == m.Fingerprint && protocolSatisfied(obs, ensureHTTPS) {
		// 幂等短路：只跳过写，不跳过刚才那次 Observe（spec §3「level-triggered」）。
		// 顺手把 appliedFingerprint 与 boundAccountId 补记上——首次接管一个已经装好
		// 同一张证书的域名时，这两项本来是空的。
		b.Status.AppliedFingerprint = m.Fingerprint
		if b.Status.BoundAccountID == "" && obs.AccountID != "" {
			b.Status.BoundAccountID = obs.AccountID
		}
		rd.lag = 0
		r.setApplied(ctx, rd)
		aggregateBindingReady(b)
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
	}

	r.noteDrift(ctx, rd, obs, m)
```

`setApplied` 在 Task 12 定义；本任务先给出它的最终形态（Task 12 不再改动），放在 `aliyuncertificatebinding_controller.go`：

```go
// setApplied 置 Applied=True，并只在状态跃迁时发一次 Normal 事件。
//
// 每轮都发会让一个健康的 Binding 每小时刷一条事件；只在 False→True 时发，事件流才
// 真正对应「发生了什么」。
func (r *AliyunCertificateBindingReconciler) setApplied(ctx context.Context, rd *bindingRound) {
	was := bindingCondTrue(rd.b, certsv1alpha1.ConditionApplied)
	setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionTrue, certsv1alpha1.ReasonApplied, "")
	if !was {
		r.Recorder.Event(rd.b, corev1.EventTypeNormal, certsv1alpha1.ReasonApplied, appliedMessage)
	}
}
```

- [ ] **Step 5: 运行**

```bash
make test 2>&1 | tail -30
```

Expected: 「域名不存在」与「Observe 未知失败不降级」两个用例 PASS；另外三个仍 FAIL（需要 Task 12 的 Apply 才会出现 `Applied=True`）。这是预期内的——在 ledger 里记一笔，Task 12 结束时必须全绿。

- [ ] **Step 6: 提交**

```bash
git add api/v1alpha1/conditions.go internal/controller/aliyuncertificatebinding_controller.go internal/controller/binding_observe_test.go
git commit -m "feat: observe binding targets with account fencing, idempotent short-circuit and drift detection"
```

---

### Task 12: `Apply` — 写入、状态固化、错误分类与指标

**Files:**
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`
- Create: `internal/controller/binding_apply_test.go`
- Create: `internal/controller/binding_conflict_envtest_test.go`（Task 8 的仲裁 envtest 用例整体推迟到这里；Task 8 只交付纯函数单测）

**Interfaces:**
- Consumes：`provider.Provider.Apply`、`provider.ApplyOptions`、`provider.ErrorOf`（Task 2、6）；`setApplied`、`handleObserveError`、`noteDrift`、`eventOnReasonChange`（Task 11）；`arbitrate` / `pickWinner`（Task 8）；`bindingApplyTotal`、`bindingDriftTotal`（Task 7）。
- Produces：
  - `(r) handleApplyError(ctx, rd *bindingRound, err error) (ctrl.Result, error)`
  - `bindingApplyResult(err error) string`（`success` / `throttled` / `error`，复用 `resultSuccess` 等常量）

- [ ] **Step 1: 写失败测试**

`internal/controller/binding_apply_test.go`：

```go
package controller

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：Apply", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("首次绑定写入证书并固化 status", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		b0, _ := pki.ParseBundle(certPEM, keyPEM)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		b := getBinding(ctx, ns, "b1")
		Expect(b.Status.AppliedFingerprint).To(Equal(b0.Fingerprint))
		Expect(b.Status.BoundAccountID).To(Equal(testAccountID))
		Expect(b.Status.LastAppliedTime).NotTo(BeNil())
		Expect(bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		d, _ := currentFC3().Domain(domain)
		Expect(d.CertPEM).To(Equal(b0.CertPEM()))
		Expect(d.Echo).To(Equal("routes"), "read-modify-write 必须原样保住 routeConfig")
		Expect(d.Protocol).To(Equal("HTTP"), "ensureHTTPSProtocol 默认 false，不许动 protocol")
	})

	It("ensureHTTPSProtocol=true 时把 HTTP 升为 HTTP,HTTPS", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, func(b *certsv1alpha1.AliyunCertificateBinding) {
			b.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol = true
		})

		eventually(func() bool {
			d, ok := currentFC3().Domain(domain)
			return ok && d.Protocol == "HTTP,HTTPS"
		})
	})

	It("证书轮换后把新代次推上去", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		gen1Cert, gen1Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, gen1Cert, gen1Key)
		createBinding(ctx, ns, "b1", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})

		gen2Cert, gen2Key := testutil.IssueLeaf(GinkgoT(), ca, domain)
		g2, _ := pki.ParseBundle(gen2Cert, gen2Key)
		simulateIssuance(ctx, ns, "c1", 2, gen2Cert, gen2Key)

		// 证书 status.current 变化会经 field index 反查唤醒 Binding，无需人工推。
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Status.AppliedFingerprint == g2.Fingerprint
		})
		d, _ := currentFC3().Domain(domain)
		Expect(d.CertPEM).To(Equal(g2.CertPEM()))
	})

	It("Apply 被限流时 Applied=False/Throttled 并重试", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		currentFC3().QueueUpdateErr(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionUpdateCustomDomain,
			Code: "Throttling.User", Err: errors.New("slow down"),
		})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonThrottled
		})
		// 队列只有一个错误，退避后应自愈。
		eventually(func() bool {
			return bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})

	It("Update 提交后响应丢失：重试写同样内容，不留半成品", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		b0, _ := pki.ParseBundle(certPEM, keyPEM)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})
		currentFC3().FailNextUpdateAfterCommit(&aliyun.Error{
			Class: aliyun.ClassRetryable, Op: aliyun.ActionUpdateCustomDomain,
			Code: "Timeout", Err: errors.New("response lost"),
		})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		// 服务端其实已经写成功了；下一轮 Observe 会看到指纹已经对上，直接短路。
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Status.AppliedFingerprint == b0.Fingerprint
		})
		d, _ := currentFC3().Domain(domain)
		Expect(d.Echo).To(Equal("routes"))
	})

	It("凭证 Secret 不存在时 Ready=False/CredentialsSecretNotFound", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})
		setFC3FactoryErr(&credentialsError{certsv1alpha1.ReasonCredentialsNotFound, errors.New("凭证 Secret 不存在")})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "b1", "c1", domain, nil)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "b1", certsv1alpha1.ConditionReady)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCredentialsNotFound
		})
	})
})
```

同时新建 `internal/controller/binding_conflict_envtest_test.go`——这是 Task 8 的仲裁 envtest
用例，因为要断言 `Applied=True` 而整体推迟到了本任务：

```go
package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("绑定 controller：冲突仲裁", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("同目标的第二个 Binding 被判 Conflict 且不写云", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		// 这个用例要的就是「两个 Binding 抢同一个目标」，所以两边共用一个域名；
		// 域名本身仍带 namespace，免得跨文件撞上别的用例（仲裁是跨 namespace 的）。
		domain := fmt.Sprintf("first.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP", Echo: "routes"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		eventually(func() bool {
			return condStatusOf(ctx, ns, "c1", certsv1alpha1.ConditionIssued) == metav1.ConditionTrue
		})

		createBinding(ctx, ns, "first", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "first", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
		writesAfterFirst := currentFC3().UpdateCalls()

		createBinding(ctx, ns, "second", "c1", domain, nil)
		eventually(func() bool {
			c := bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionTrue && c.Reason == certsv1alpha1.ReasonConflictingBinding
		})
		Expect(bindingCond(ctx, ns, "second", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		// 输家一次云写入都不该发出——两个 Binding 轮流写同一个域名比不写更危险。
		Expect(currentFC3().UpdateCalls()).To(Equal(writesAfterFirst))
	})

	It("胜者被删掉后输家接管", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domain := fmt.Sprintf("first.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentFC3().AddDomain(fake.Domain{DomainName: domain, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domain)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "first", "c1", domain, nil)
		createBinding(ctx, ns, "second", "c1", domain, nil)
		eventually(func() bool {
			return bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict).Status == metav1.ConditionTrue
		})

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "first"))).To(Succeed())
		eventually(func() bool {
			c := bindingCond(ctx, ns, "second", certsv1alpha1.ConditionConflict)
			return c.Status == metav1.ConditionFalse
		})
		eventually(func() bool {
			return bindingCond(ctx, ns, "second", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})

	It("不同域名互不冲突", func() {
		ns := newNamespace(ctx)
		ca := testutil.NewCA(GinkgoT())
		domainA := fmt.Sprintf("ba.%s.example.com", ns)
		domainB := fmt.Sprintf("bb.%s.example.com", ns)
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domainA, domainB)
		currentFC3().AddDomain(fake.Domain{DomainName: domainA, Protocol: "HTTP"})
		currentFC3().AddDomain(fake.Domain{DomainName: domainB, Protocol: "HTTP"})

		createCertificate(ctx, ns, "c1", domainA, domainB)
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createBinding(ctx, ns, "ba", "c1", domainA, nil)
		createBinding(ctx, ns, "bb", "c1", domainB, nil)

		eventually(func() bool {
			return bindingCond(ctx, ns, "ba", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue &&
				bindingCond(ctx, ns, "bb", certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
		})
	})
})

// condStatusOf 读 AliyunCertificate 的 condition 状态。
//
// 与 retention_test.go 里的 condStatus(ac, t) 只是名字相近，不冲突。
func condStatusOf(ctx context.Context, ns, name, condType string) metav1.ConditionStatus {
	ac := &certsv1alpha1.AliyunCertificate{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ac); err != nil {
		return metav1.ConditionUnknown
	}
	for _, c := range ac.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}
```

- [ ] **Step 2: 运行，确认失败**

```bash
make test 2>&1 | tail -30
```

Expected: 大部分新用例 FAIL（`Applied` 从不为 True）。

- [ ] **Step 3: 实现**

`aliyuncertificatebinding_controller.go` 追加错误处置：

```go
// bindingApplyResult 把一次写入折叠成固定的 result label（与 CAS 侧同一套取值）。
func bindingApplyResult(err error) string {
	if err == nil {
		return resultSuccess
	}
	if pe := provider.ErrorOf(err); pe != nil && pe.Code == provider.CodeThrottled {
		return resultThrottled
	}
	return resultError
}

// handleApplyError 处置写入失败。
//
// 与 Observe 的旁路原则相反：Apply 失败就是「没写成」，Applied=False 是对事实的陈述，
// 必须降级。分档只影响重试节奏——限流与瞬时故障走指数退避，凭证问题走 5m 长 requeue，
// 永久错误（参数被拒）走 drift 周期，等人改 spec。
//
// **不回滚 CAS**（spec §6.2）：CAS 上传成功、FC3 应用失败时多出一张没人引用的证书，
// keepLast 会管住它；删掉再传只会让重试变成上传/删除死循环。
func (r *AliyunCertificateBindingReconciler) handleApplyError(ctx context.Context, rd *bindingRound, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pe := provider.ErrorOf(err)
	reason := certsv1alpha1.ReasonApplyFailed
	if pe != nil && pe.Reason != "" {
		reason = pe.Reason
	}
	// 错误原文只进日志：事件是广播给用户的对象，云错误里可能夹带 request id。
	log.Error(err, "写入目标失败", "domain", targetIdentifier(rd.b))
	// 事件先于 setBindingCondition 发：eventOnReasonChange 比的是 rd.orig 上一轮的
	// reason，而 rd.b 马上就要被改成本轮的 reason（spec §10.2「只在状态跃迁时发」——
	// 一次限流会连着失败很多轮，每轮一条 Warning 只是噪声）。
	r.eventOnReasonChange(rd, certsv1alpha1.ConditionApplied, reason,
		corev1.EventTypeWarning, certsv1alpha1.ReasonApplyFailed, applyFailedMessage)
	setBindingCondition(rd.b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, reason, "写入目标失败")
	aggregateBindingReady(rd.b)
	if perr := r.patchBinding(ctx, rd); perr != nil {
		return ctrl.Result{}, perr
	}
	switch {
	case pe != nil && pe.Retryable:
		return ctrl.Result{}, err // 指数退避
	case pe != nil && pe.Code == provider.CodeAuth:
		return ctrl.Result{RequeueAfter: credentialsRequeue}, nil
	default:
		return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, nil
	}
}
```

在 `reconcileBindingReady` 的 `r.noteDrift(...)` 之后补上步骤 6–8：

```go
	// 6. Apply（spec §6.2 步骤 6）
	aerr := p.Apply(ctx, tg, cl, m, provider.ApplyOptions{
		EnsureHTTPSProtocol: ensureHTTPS,
		PreviousFingerprint: b.Status.AppliedFingerprint,
	})
	bindingApplyTotal.WithLabelValues(rd.provider, bindingApplyResult(aerr)).Inc()
	if aerr != nil {
		return r.handleApplyError(ctx, rd, aerr)
	}

	// 7. 固化状态
	now := r.now()
	b.Status.AppliedFingerprint = m.Fingerprint
	b.Status.LastAppliedTime = &metav1.Time{Time: now}
	if b.Status.BoundAccountID == "" && obs.AccountID != "" {
		// 首次成功写入才固化账号：写成功证明这个账号确实是我们该写的那个。
		b.Status.BoundAccountID = obs.AccountID
	}
	rd.lag = 0
	r.setApplied(ctx, rd)

	// 8. Ready = Applied && !Conflict
	aggregateBindingReady(b)
	return ctrl.Result{RequeueAfter: r.DriftCheckInterval}, r.patchBinding(ctx, rd)
```

- [ ] **Step 4: 确认仲裁 envtest 已纳入运行**

Step 1 新建的 `binding_conflict_envtest_test.go` 就是 Task 8 推迟过来的那三个用例，不存在
「解除 `Skip`」这回事——Task 8 从头就没写过这个文件，也没写过任何 `Skip`。这里只需确认
三个用例都在跑且全绿（`go test ./internal/controller/ -run TestControllers -v | grep 冲突仲裁`）。

- [ ] **Step 5: 运行，确认全绿**

```bash
make test 2>&1 | tail -30
```

Expected: Task 8、9、11、12 的全部 envtest 用例 PASS。特别确认四个遗留用例现在通过：Task 11 的「指纹一致时短路」「账号 fencing」「drift 纠正」，以及 Task 9 的「Secret 已换但证书 CR 未推进代次时不写 FC3」。

- [ ] **Step 6: 提交**

```bash
git add internal/controller/
git commit -m "feat: apply certificate material to binding targets and record apply metrics"
```

---

### Task 13: 删除分支 — `Orphan` / `Unbind` 与有界清理

**Files:**
- Modify: `api/v1alpha1/aliyuncertificatebinding_types.go`
- Create: `internal/controller/binding_deletion.go`
- Create: `internal/controller/binding_deletion_test.go`
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`（删掉 Task 7 的存根）

**Interfaces:**
- Consumes：`provider.Provider.Observe/Cleanup`、`provider.DeletionPolicy*`（Task 2、6）；`CleanupPolicyAbandon` / `CleanupPolicyBlock`（`aliyuncertificate_controller.go`，已存在）；`cleanupAbandonedTotal`（`metrics.go`，已存在）；`issueAndBind`（**Task 11 在 `binding_observe_test.go` 里定义的测试 helper**，同包跨文件——单独重跑本 Task 前必须先有 Task 11）；`clearBindingMetrics` / `targetRegion`（Task 7）；`bindingReconciler.SetNow`（Task 7）。
- Produces：
  - `AliyunCertificateBindingStatus.CleanupStartedAt *metav1.Time`（新字段，需 `make generate manifests`）
  - `(r) reconcileBindingDelete(ctx, rd *bindingRound) (ctrl.Result, error)`（替换存根）
  - `(r) unbindTarget(ctx, rd *bindingRound) error`
  - `(r) finishBindingDeletion(ctx, rd *bindingRound) (ctrl.Result, error)`
  - 常量 `bindingCleanupAbandonedMessage`

- [ ] **Step 1: 写失败测试**

`internal/controller/binding_deletion_test.go`：

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func bindingGone(ctx context.Context, ns, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &certsv1alpha1.AliyunCertificateBinding{})
	return apierrors.IsNotFound(err)
}

var _ = Describe("绑定 controller：删除", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
	})

	It("Orphan（默认）：摘 finalizer，云侧一动不动", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTPS")
		before, _ := currentFC3().Domain(domain)
		writes := currentFC3().UpdateCalls()

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b1") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).To(Equal(before.CertName))
		Expect(currentFC3().UpdateCalls()).To(Equal(writes), "一个 kubectl delete 不该打穿生产 HTTPS")
	})

	It("Unbind：清空自己的证书，纯 HTTPS 域名降为 HTTP", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTPS")

		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Spec.DeletionPolicy == certsv1alpha1.DeletionPolicyUnbind
		})

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b1") })

		d, _ := currentFC3().Domain(domain)
		Expect(d.CertName).To(BeEmpty())
		Expect(d.Protocol).To(Equal("HTTP"), "纯 HTTPS 域名拿掉证书后必须降为 HTTP，否则彻底不可用")
		Expect(d.Echo).To(Equal("routes"))
	})

	It("Unbind：目标上是别人的证书时一动不动", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTP,HTTPS")

		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Spec.DeletionPolicy == certsv1alpha1.DeletionPolicyUnbind
		})

		// 有人在我们之后把证书换成了别的。解绑只该解自己那一张。
		otherCA := testutil.NewCA(GinkgoT())
		otherPEM, _ := testutil.IssueLeaf(GinkgoT(), otherCA, domain)
		d, _ := currentFC3().Domain(domain)
		d.CertName = "someone-elses"
		d.CertPEM = otherPEM
		currentFC3().AddDomain(d)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b1") })

		after, _ := currentFC3().Domain(domain)
		Expect(after.CertName).To(Equal("someone-elses"))
	})

	It("Unbind：目标已经不存在时直接摘 finalizer", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTPS")
		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Spec.DeletionPolicy == certsv1alpha1.DeletionPolicyUnbind
		})

		currentFC3().RemoveDomain(domain)
		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "b1") })
	})

	It("Unbind：清理持续失败，超过宽限期后 Abandon", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b1.%s.example.com", ns)
		issueAndBind(ctx, ns, "c1", "b1", domain, "HTTPS")
		b := getBinding(ctx, ns, "b1")
		b.Spec.DeletionPolicy = certsv1alpha1.DeletionPolicyUnbind
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		eventually(func() bool {
			return getBinding(ctx, ns, "b1").Spec.DeletionPolicy == certsv1alpha1.DeletionPolicyUnbind
		})

		// 假时钟：与证书 controller 的删除用例同一套手法（deletion_test.go）。
		var mu sync.Mutex
		fakeNow := time.Now()
		bindingReconciler.SetNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return fakeNow })
		DeferCleanup(func() { bindingReconciler.SetNow(nil) })

		for i := 0; i < 50; i++ {
			currentFC3().QueueGetErr(&aliyun.Error{
				Class: aliyun.ClassRetryable, Op: aliyun.ActionGetCustomDomain,
				Code: "InternalError", Err: errors.New("boom"),
			})
		}
		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "b1"))).To(Succeed())
		// 这里必须用不硬失败的读法：getBinding 内部是 ExpectWithOffset(...).To(Succeed())，
		// 对象一旦在轮询窗口里被摘掉 finalizer 删干净，整个用例会以断言失败告终而不是
		// 继续收敛。直接 Get，err != nil 就返回 false 让 eventually 接着轮询。
		eventually(func() bool {
			cur := &certsv1alpha1.AliyunCertificateBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b1"}, cur); err != nil {
				return false
			}
			return cur.Status.CleanupStartedAt != nil
		})

		mu.Lock()
		fakeNow = fakeNow.Add(16 * time.Minute)
		mu.Unlock()

		eventually(func() bool { return bindingGone(ctx, ns, "b1") })
		// 证书还留在云上——这正是 Abandon 的含义，有事件与计数器可查。
		d, _ := currentFC3().Domain(domain)
		Expect(d.CertName).NotTo(BeEmpty())
	})
})
```

（本文件用不到 `fake` 与 `metav1`，import 里就不要写它们。）

- [ ] **Step 2: 运行，确认失败**

```bash
make test 2>&1 | tail -30
```

Expected: FAIL，`b.Status.CleanupStartedAt undefined`。

- [ ] **Step 3: 给 Binding status 加 `cleanupStartedAt`**

`api/v1alpha1/aliyuncertificatebinding_types.go` 的 `AliyunCertificateBindingStatus` 追加（放在 `Conditions` 之前）：

```go
	// 首次进入删除分支的时间，用于 --cleanup-grace-period 计时。
	// +optional
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`
```

```bash
make generate manifests
```

- [ ] **Step 4: 实现删除分支**

`internal/controller/binding_deletion.go`：

```go
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// bindingCleanupAbandonedMessage 是 CleanupAbandoned 事件的固定文案。域名与错误原文
// 只进日志：事件面向用户广播，不该夹带 request id 这类细节。
const bindingCleanupAbandonedMessage = "gave up unbinding the certificate from the target after the cleanup grace period; see operator logs"

// reconcileBindingDelete 实现 spec §6.5。
//
//	Orphan（默认）→ 直接摘 finalizer，云侧不动。一个 kubectl delete 不应打穿生产 HTTPS。
//	Unbind        → Observe 确认目标上那张确实是自己写的，才清空 certConfig；
//	                纯 HTTPS 域名同时降为 HTTP。受 cleanup-grace-period /
//	                cleanup-failure-policy 约束。
func (r *AliyunCertificateBindingReconciler) reconcileBindingDelete(ctx context.Context, rd *bindingRound) (ctrl.Result, error) {
	b := rd.b
	if !controllerutil.ContainsFinalizer(b, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	if b.Spec.DeletionPolicy != certsv1alpha1.DeletionPolicyUnbind {
		return r.finishBindingDeletion(ctx, rd)
	}
	if b.Status.AppliedFingerprint == "" {
		// 从没写成功过，云上没有属于我们的东西。
		return r.finishBindingDeletion(ctx, rd)
	}

	// 宽限期从「真正开始清理」起算，与证书 controller 同一考量。
	if b.Status.CleanupStartedAt == nil {
		b.Status.CleanupStartedAt = &metav1.Time{Time: r.now()}
		if err := r.patchBinding(ctx, rd); err != nil {
			return ctrl.Result{}, err
		}
		rd.orig = b.DeepCopy()
	}

	if err := r.unbindTarget(ctx, rd); err != nil {
		elapsed := r.now().Sub(b.Status.CleanupStartedAt.Time)
		if r.CleanupFailurePolicy == CleanupPolicyBlock || elapsed < r.CleanupGracePeriod {
			log.Error(err, "解绑失败，重试中", "domain", targetIdentifier(b))
			return ctrl.Result{}, err // 指数退避
		}
		// Abandon：把足以人工兜底的信息留在日志里，然后走完删除。
		log.Error(err, "cleanup abandoned",
			"domain", targetIdentifier(b),
			"fingerprint", shortFP(b.Status.AppliedFingerprint),
			"gracePeriod", r.CleanupGracePeriod)
		r.Recorder.Event(b, corev1.EventTypeWarning, certsv1alpha1.ReasonCleanupAbandoned, bindingCleanupAbandonedMessage)
		cleanupAbandonedTotal.WithLabelValues(targetRegion(b), providerErrClass(err)).Inc()
	}
	return r.finishBindingDeletion(ctx, rd)
}

// unbindTarget 只解绑属于自己的那一张证书（spec §6.5）。
//
// 指纹比对放在通用层而不是 provider：Cleanup 的签名里没有 appliedFingerprint，而
// 「幂等判断的真相来源是 Observe」（spec §7 职责边界表）。先 Observe 再决定要不要
// Cleanup，也让「目标已经不存在」和「上面是别人的证书」两种情形都以成功收场。
func (r *AliyunCertificateBindingReconciler) unbindTarget(ctx context.Context, rd *bindingRound) error {
	b := rd.b

	// 证书 CR 可能已经先被删掉了（Argo CD 会同时 prune 两者）。取不到就传 nil，
	// 凭证只能来自 Binding 自己的 credentialsRef——取不到时 ProviderFactory 会给出
	// CredentialsNotFound，走 Abandon 分支。
	var ac *certsv1alpha1.AliyunCertificate
	fetched := &certsv1alpha1.AliyunCertificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}, fetched)
	switch {
	case err == nil:
		ac = fetched
	case !apierrors.IsNotFound(err):
		return err
	}

	p, cl, err := r.ProviderFactory(ctx, b, ac)
	if err != nil {
		return err
	}
	tg, err := targetOf(b)
	if err != nil {
		return err
	}

	obs, err := p.Observe(ctx, tg, cl)
	if err != nil {
		if pe := provider.ErrorOf(err); pe != nil && pe.Code == provider.CodeTargetNotFound {
			return nil // 域名没了，解绑的目的已经达到
		}
		return err
	}
	if obs.CurrentFingerprint != b.Status.AppliedFingerprint {
		// 目标上不是我们写的那张：可能是别的 Binding 接管了，也可能是人工换过。
		// 动它等于替别人做主。
		logf.FromContext(ctx).Info("目标上的证书不是本 Binding 写入的，跳过解绑",
			"domain", tg.Identifier, "observed", shortFP(obs.CurrentFingerprint))
		return nil
	}
	return p.Cleanup(ctx, tg, cl, provider.DeletionPolicyUnbind)
}

// finishBindingDeletion 摘 finalizer 并清掉指标 series。
func (r *AliyunCertificateBindingReconciler) finishBindingDeletion(ctx context.Context, rd *bindingRound) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(rd.b, certsv1alpha1.FinalizerName)
	if err := r.Update(ctx, rd.b); err != nil {
		return ctrl.Result{}, err
	}
	// 对象没了，它的 gauge 也必须跟着消失：留下的 Ready=0 会一直告警下去。
	clearBindingMetrics(rd.b.Namespace, rd.b.Name, rd.provider)
	logf.FromContext(ctx).Info("binding deleted", "name", rd.b.Name)
	return ctrl.Result{}, nil
}

// providerErrClass 给 cleanup_abandoned_total 的 reason label 一个有界取值。
//
// 注意 `cleanup_abandoned_total{reason}` 这一个 label 上跑着**两套词表**：证书 controller
// 传的是 `aliyun.ErrClass` 的字符串（Permanent / Retryable / Auth / NotFound），绑定
// controller 传的是 `provider.Code*`（TargetNotFound / Auth / Throttled / Retryable /
// Permanent / InvalidClient / InvalidTarget）。两边取值都有界，不会造成基数爆炸，但看板
// 与告警的作者必须知道同一个 label 里会同时出现两种词表——这一条要进 README 的已知限制。
func providerErrClass(err error) string {
	if pe := provider.ErrorOf(err); pe != nil {
		return pe.Code
	}
	return provider.CodePermanent
}
```

删掉 `aliyuncertificatebinding_controller.go` 里 Task 7 的 `reconcileBindingDelete` 存根。

- [ ] **Step 5: 运行，确认通过**

```bash
make manifests generate build
make test 2>&1 | tail -30
```

Expected: 5 个删除用例全 PASS，其余不回归。

- [ ] **Step 6: 提交**

```bash
git add api/ config/crd/ internal/controller/
git commit -m "feat: honour deletionPolicy Orphan/Unbind with bounded cleanup on binding deletion"
```

---

### Task 14: `cmd/main.go` 接线、RBAC 与 `--drift-check-interval`

**Files:**
- Modify: `cmd/main.go`
- Modify: `cmd/main_flags_test.go`
- Modify: `config/rbac/role.yaml`（由 `make manifests` 生成）

**Interfaces:**
- Consumes：`controller.AliyunCertificateBindingReconciler`（Task 7）、`controller.NewProviderFactory`（Task 10）、`aliyun.NewClientCache[aliyun.FC3Client]`（Task 3）。
- Produces：绑定 controller 在生产二进制里被启动；`--drift-check-interval` 有了消费方。

- [ ] **Step 1: 写失败测试**

`cmd/main_flags_test.go` 追加：

```go
func TestParseOperatorFlags_DriftCheckInterval(t *testing.T) {
	o, err := parseOperatorFlags([]string{"--drift-check-interval=15m"})
	if err != nil {
		t.Fatal(err)
	}
	if o.DriftCheckInterval != 15*time.Minute {
		t.Errorf("drift-check-interval 未生效: %v", o.DriftCheckInterval)
	}
	d, err := parseOperatorFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.DriftCheckInterval != time.Hour {
		t.Errorf("默认应为 1h: %v", d.DriftCheckInterval)
	}
}
```

- [ ] **Step 2: 运行，确认通过或失败**

```bash
go test ./cmd/ -run TestParseOperatorFlags_DriftCheckInterval -v
```

Expected: PASS（flag 在 Plan 1 就注册了）。这条用例的作用是把「有默认值、能被覆盖」钉住，防止后续改动把它调没。若 FAIL，先修 flag 注册。

- [ ] **Step 3: 接线**

`cmd/main.go`，在 `certReconciler.SetupWithManager(mgr)` 之后、`+kubebuilder:scaffold:builder` 之前插入：

```go
	fc3Cache := aliyun.NewClientCache[aliyun.FC3Client]()
	bindingReconciler := &controller.AliyunCertificateBindingReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("aliyuncertificatebinding"),
		// Secret 已 DisableFor，mgr.GetClient() 对它就是直读。
		ProviderFactory:      controller.NewProviderFactory(mgr.GetClient(), fc3Cache, limiters, opts.CloudCallTimeout),
		DriftCheckInterval:   opts.DriftCheckInterval,
		CleanupGracePeriod:   opts.CleanupGracePeriod,
		CleanupFailurePolicy: opts.CleanupFailurePolicy,
	}
	if err := bindingReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AliyunCertificateBinding")
		os.Exit(1)
	}
```

两个 controller 共用同一个 `limiters`：阿里云按账号限流，同一个 AK 同时被 CAS 与 FC3 使用时，桶必须是同一批（`Limiters` 内部按 `(key, kind)` 分桶，服务之间互不干扰）。

- [ ] **Step 4: 重新生成 RBAC 并核对**

```bash
make manifests
grep -A3 'aliyuncertificatebindings' config/rbac/role.yaml
```

Expected: `aliyuncertificatebindings`、`aliyuncertificatebindings/status`、`aliyuncertificatebindings/finalizers` 三条规则齐备；`secrets` 仍只有 `get` 与 `delete`（Plan 2 没有新增 Secret 权限）。

- [ ] **Step 5: 全量校验**

```bash
make build && make test
go vet ./...
```

Expected: 全绿。

- [ ] **Step 6: 提交**

```bash
git add cmd/ config/rbac/
git commit -m "feat: wire the binding controller into the operator binary"
```

---

## 自查记录（写计划时已做）

### 1. Spec 覆盖对照

| Spec 条目 | Task |
|---|---|
| §6.1 主资源 watch、证书反查、`RequeueAfter: drift-check-interval` | 7、14 |
| §6.2 步骤 0（删除分支入口） | 7（分派）、13（实现） |
| §6.2 步骤 1 证书查找：`CertificateNotFound` / `CertificateNotReady` | 7 |
| §6.2 步骤 2 冲突仲裁（`(creationTimestamp, UID)` 最小者胜） | 8 |
| §6.2 步骤 3 域名覆盖硬失败 `DomainNotCovered` | 9 |
| §5.4 校验先于任何云侧写入（`certificateGate`：Secret 指纹 ≠ `status.current` 时不写，30s 重试） | 9 |
| §6.2 步骤 4 凭证解析（Binding > 证书）与 client 构造 | 10 |
| §6.2 步骤 5 `Observe`：`TargetNotFound` 固定 5m、账号 fencing、幂等短路、drift 判定 | 11 |
| §6.2 步骤 6–7 `Apply` 与 `appliedFingerprint` / `boundAccountId` / `lastAppliedTime` 固化 | 12 |
| §6.2 步骤 8 `Ready = Applied && !Conflict` | 7（`aggregateBindingReady`）、12 |
| §6.2「不回滚原则」（FC3 失败不回滚 CAS） | 12（`handleApplyError` 注释；代码上不存在回滚路径） |
| §6.3 FC3 Apply 五步（read → 回填 → certConfig → ensureHTTPS → update） | 6 |
| §6.4 私钥保护（redact、日志只出域名 + 指纹前 8 位） | 3（脱敏方法）、4（`customDomainFromSDK` 剔除）、11/12/13（日志字段） |
| §6.5 清理分支 `Orphan` / `Unbind`、只解绑自己的证书、纯 HTTPS 降 HTTP、有界清理 | 6（provider 侧）、13（通用层判定） |
| §7 `Target` / `CertMaterial` / `ObservedState` / `Capabilities` / `ApplyOptions` / `Provider` / `ProviderError` | 2 |
| §7 职责边界表（幂等真相在 Observe、SANs 与退避在通用层、client 由通用层构造） | 2（接口形状）、6（provider 侧）、9–12（通用层侧） |
| §7 `init()` 注册 + `map[string]Provider` | 2（registry）、6（`init`）、10（`provider.Get`） |
| §8.1 RBAC：bindings / status / finalizers、aliyuncertificates get-list-watch | 7（marker）、14（生成并核对） |
| §8.2 client 缓存 key 含 resourceVersion | 10 |
| §8.3 FC3 逐域名 ARN 授权 | 6（`docs/ram/binding-fc3-policy.json` + 包注释） |
| §9 `--drift-check-interval` 的消费方 | 7（字段）、14（接线） |
| §9 `--cloud-call-timeout` 约束所有 FC3 调用 | 4（`withTimeout`）、10（传入） |
| §9 `--cleanup-grace-period` / `--cleanup-failure-policy` 复用到 Binding | 13 |
| §10.1 `binding_ready` / `binding_conflict` / `binding_applied_age_seconds` | 7 |
| §10.1 `binding_apply_total{provider,result}` | 12 |
| §10.1 `binding_drift_detected_total{provider}` | 11 |
| §10.1 `aliyun_api_requests_total` / `duration_seconds` 的 `service="fc"` | 7（recorder 参数化）、10（接线） |
| §10.2 事件 `Normal Applied` / `Warning ApplyFailed` / `Warning DriftCorrected` / `Warning CleanupAbandoned` | 11、12、13 |
| §12.1 单元：冲突仲裁排序 | 8 |
| §12.1 单元：SANs 覆盖（RFC 6125） | 9（复用 Plan 1 的 `pki.Covers` 并新增绑定侧用例） |
| §12.1 单元：错误分类（provider 侧） | 6 |
| §12.2 envtest：证书未 Ready / 目标不存在 / 冲突仲裁 / 域名不覆盖 / 幂等短路 / drift 纠正 / accountId 不匹配 / `Unbind` 只解绑自己的 | 7、9、11、12（含推迟过来的冲突仲裁 envtest）、13 |
| §12.2 故障注入（TargetNotFound / Auth / Throttling / Update 提交后响应丢失） | 5（fake 能力）、11、12 |
| §12.2 窄接口 `FC3Client`（不 mock SDK struct） | 3、4、5 |
| §15 provider 演进预留（`ReferencesCertByID` / `RequiresCASUpload` / registry） | 2、6 |

### 2. 明确不在 Plan 2 的 spec 条目

- **§5 全部**、§4.1、§4.3、§4.4 的证书侧：Plan 1 已交付。
- **§7 的 `RequiresCASUpload` 强制上传联动**：接口里的开关本任务已备好（`Capabilities()`），但**没有任何 provider 把它置 true**，而真正实现它需要证书 controller 反查引用自己的每个 Binding 的 provider 能力、并在 `uploadToCAS=false` 时覆盖用户意图 + 写 condition 说明。在第一个 `ReferencesCertByID=true` 的 provider（CDN / CLB，spec §15）落地之前，这段代码没有任何一条能被执行的路径，也没法测。**留给引入该 provider 的那个 plan。**
- **§10.3 PrometheusRule**、**§11 Argo CD 清单 / README / SCC / 镜像**、**§12.3 真实环境集成测试**、**§13 安全文档**、**§14 已知限制文档**：Plan 3。
- **FC3 的 endpoint 覆盖**（team lead 裁决，2026-09-05，YAGNI）：`spec.aliyun.endpointOverride` 只作用于 CAS；FC3 一律走 SDK 按 region 选出的默认 endpoint（`fcv3.<region>.aliyuncs.com`）。这是一条**已知限制**，由 Plan 3 Task 13 写进 README。将来确有 VPC 内网 / 专有云需求时的形状已定：给 `FC3CustomDomainTarget` 新增一个可选字段 `endpointOverride`——**给 schema 加字段不受 `spec.target` 不可变约束的影响**（transition rule 只管住已有对象的取值变更），新建的 Binding 可以设它。
- **CDN / DCDN / CLB / ALB provider**（§15）。
- **RRSA / OIDC**（§15）：`aliyun.Credentials` 已支持，Plan 2 不新增也不承诺。

### 3. 需要执行者现场核实的清单

| # | 待核实 | 影响到计划里的哪一段 |
|---|---|---|
| 1 | FC3 SDK 的实际版本与结构体字段名（`go get` 可能取到 > v4.8.2） | **Task 1 的全部内容**，以及 Task 4 的 `customDomainFromSDK` / `updateInputToSDK`。本计划已对 v4.8.2 逐字核对并编译验证（2026-09-05），但复核结果优先 |
| 2 | FC3 账号级频控阈值与 Throttling 错误码（spec §12.3 #10） | Task 3 的 `LimitFC3 = 5 QPS / burst 1` 是**猜的保守值**；Task 6 `toProviderError` 里 `Throttling` 前缀匹配沿用 CAS 的经验，FC3 是否同样以 `Throttling` 开头未证实 |
| 3 | `UpdateCustomDomain` 是全量替换还是部分合并（spec §12.3 #2） | Task 4 `updateInputToSDK` 的 `ClearCert` 分支（显式三个空串）；Task 6 的 read-modify-write 在两种语义下都正确，但**解绑只在其中一种语义下真的生效**——若实测为「合并且空串被忽略」，Unbind 需要改用别的手段（如先 `DeleteCustomDomain` 再重建，代价大得多，届时需重新决策） |
| 4 | FC3 对 PKCS#1 / SEC1 私钥的接受情况（spec §12.3 #1） | Task 9 `loadBindingMaterial` 直接用 `pki.Bundle.KeyPEM()`（RSA → PKCS#1、ECDSA → SEC1）。若 FC3 只收 PKCS#8，`pki.Bundle` 需要增加第二种输出 |
| 5 | LE 链（leaf + intermediate、无根）FC3 是否接受、顺序是否敏感（spec §12.3 #5） | Task 9 的 `m.CertPEM = b.CertPEM()`（leaf 在前、中间证书紧随、无空行、无根） |
| 6 | FC3 对不存在域名返回的真实错误码 | Task 5 的 `ErrDomainNotFound` 用 `DomainNameNotFound`，Task 6 依赖 `aliyun.classifyCode` 把它归入 `ClassNotFound`（靠 `Contains(code,"NotFound")` 或 404）。若真实码既不含 `NotFound` 也不是 404，`TargetNotFound` 分支永远走不到，`Ready` 会停在旁路的 Warning 上。**不做推测式兜底**（「Code 含 `DomainName` 且 Permanent → TargetNotFound」会把 `InvalidDomainName` 这类真·永久错误误归）：等集成测试量出真实错误码后回去修 `aliyun.classifyCode`，那是错误码归类的唯一落点 |
| 7 | 同一账号下 FC3 的 `accountId` 是否总是非空 | Task 11 `fenceAccount` 在 `obs.AccountID == ""` 时放行（无从判断则不拦）。若该字段常为空，fencing 形同虚设 |

### 4. 自查中发现并已修正的问题

1. **`aliyuncert_binding_applied_age_seconds` 的字面语义会让 spec 自己的告警误报。** spec §10.1 写「目标上生效证书的年龄」，§10.3 的告警是 `applied_age > 86400 and certificate_ready == 1`。按字面实现，任何一张健康证书在生效第二天就会触发。已把语义改为**滞后时长**（同步时为 0，滞后时从证书当前代次的 `uploadedAt` 起算），见 Task 7 的指标定义与 Task 9 的 `appliedLag`。**已由 team lead 裁决采用；指标名不变，spec §10.1 / §10.3 的文字由 Plan 3 Task 9 更新。**
2. **错误处置路径上的 nil 解引用。** `handleObserveError` / `handleApplyError` / `reconcileBindingDelete` 原本直接写 `b.Spec.Target.FC3CustomDomain.DomainName` 取日志字段，而这些路径恰恰会被「`target.type` 不认识 / 内嵌块缺失」触发——那正是该指针为 nil 的时候。已改为 nil-safe 的 `targetIdentifier(b)` / `targetRegion(b)`（Task 7）。
3. **`ClientCache` 只能装 `CASClient`。** 原本要么把它改成存 `any` 再到处断言，要么复制一份。已在 Task 3 改成泛型 `ClientCache[T]`，并把 `cache_test.go` / `cas_factory.go` / `main.go` 三处调用点的更新写成明确的 `sed` 步骤（不是「相应调整」这种占位）。
4. **测试 helper 重名。** `retention_test.go` 里已有一个纯函数 `binding(gen, observed, applied)`。新 helper 若也叫 `binding` 会在同一个包里冲突。已统一用 `createBinding` / `getBinding` / `createCertificate`，并在 Task 7 Step 8 写了显式提醒。
5. **跨任务的红灯窗口。** Task 11 的部分 envtest 用例、以及 Task 9 的第三个用例，在 Apply 落地（Task 12）之前必然失败。已在各处写明「哪些用例本任务应通过、哪些留到 Task 12」，并要求在 ledger 里记录，避免执行者误以为自己写错了。Task 8 原本也有三个这样的用例，pre-flight 裁决后整体推迟到 Task 12 创建，红灯窗口因此少了一个。
6. **envtest 目标域名跨文件撞车。** 仲裁刻意跨 namespace 按 `TargetKey()`（只含 type/region/domainName）检索，而多个测试文件原本都用 `api.example.com` 之类的字面量——先建的 Binding 会把后建的一直判成 Conflict，`Applied` 永远不为 True。已统一为 `fmt.Sprintf("%s.%s.example.com", bindingName, ns)`（pre-flight 裁决），并写进 Task 7 Step 8 的域名约定。
7. **`aliyuncert_binding_applied_age_seconds` 在早退路径被清零。** `rd.lag` 原本算在材料装载之后，而冲突分支更在它之前就 return——「判定冲突 / Secret 丢了 / 域名不覆盖」这三种最该告警的状态反而把 gauge 刷成 0。已把它提到取完证书之后、仲裁之前（Task 9 Step 4，pre-flight 裁决）。

### 5. 裁决记录（team lead，2026-09-05）

三点全部裁定，计划已按裁决改写；执行者按下表理解「为什么代码长这样」，不必再问。

1. **`aliyuncert_binding_applied_age_seconds` 采用滞后时长语义。** 值 = 证书 CR 的 `status.current` 推进之后、本 Binding 尚未把该代应用到目标的持续时间；同步时为 0。**指标名不变**；spec §10.1 的文字与 §10.3 的告警式子由 Plan 3 Task 9 的 spec 对齐任务更新。落点：Task 7 的指标定义、Task 9 的 `appliedLag`。
2. **写入内容以 Secret 为准，但以证书 CR 的 `status.current.fingerprint` 为闸。** PEM 与指纹一律同源于 Secret（spec §3「写入内容只由 Secret 决定」），但 `bundle.Fingerprint != ac.Status.Current.Fingerprint` 时**不 Apply**：置 `Applied=False/CertificateNotReady`，`RequeueAfter: 30s` 等证书 controller 追平。理由是 spec §5.4 的校验（链连续、公私钥匹配、非临时自签、SANs 覆盖）必须先于任何云侧写入——cert-manager 的临时自签证书正是「先进 Secret、后被证书 controller 拒绝」的那一种。落点：Task 9 的 `certificateGate` / `certificateGateRequeue`、Reconcile 步骤 3b，以及 envtest 用例「Secret 已换但证书 CR 未推进代次时不写 FC3」。
3. **不给 FC3 加 `endpointOverride`（YAGNI）。** `spec.aliyun.endpointOverride` 只作用于 CAS；FC3 走 SDK 的 region 默认 endpoint。已记入上面第 2 节「不在 Plan 2」，含将来需要时的字段形状；README 的已知限制由 Plan 3 Task 13 补。落点：Task 10 的 `ClientKey` 构造处注释。
4. 另两条 lead 已确认：`RequiresCASUpload` 联动留给 CDN / CLB provider 的 plan；`ClientCache` 泛型化通过。
