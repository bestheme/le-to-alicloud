# Plan 4 — OSS 自定义域名证书绑定 provider 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `AliyunCertificateBinding` 支持第二个 target 类型 `OSSCustomDomain`：把已上传到 CAS 的证书按 certId 绑到 OSS bucket 的自定义域名（CNAME）上，续期自动换绑、漂移自动纠正、`Unbind` 时摘证书。

**Architecture:** OSS 上存的不是 PEM 而是 CAS certId，所以通用层新增一条「按 certRef 对账」的支路（`certIdentity` 三元组），并落实主 spec §7 预留的 `ReferencesCertByID` / `RequiresCASUpload` 两条能力位；`pkg/aliyun` 追加窄接口 `OSSClient`（SDK v2 + 凭证适配器），`pkg/provider/oss` 是第二个 provider；provider 工厂的 client cache 改为按 target 类型分派。FC3 路径行为逐字节不变。

**Tech Stack:** 沿用 Plan 1–3 的工具链（Go 1.26 / controller-runtime v0.24.1 / envtest + Ginkgo v2 / golangci-lint v2.13.2 含 gosec、nolintlint、dupl、lll）。新增 `github.com/aliyun/alibabacloud-oss-go-sdk-v2 v1.6.0`。

**Spec:** `docs/superpowers/specs/2026-09-07-oss-provider-design.md`（增量 spec；主 spec `docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md` 对未提及的行为仍然有效）

## Global Constraints

以下沿用 Plan 1–3（仍然全部生效），末尾七条是 Plan 4 新增。

- Go module 路径：`git.dev.bestheme.ac.cn/infra/le-to-alicloud`。API group / version：`certs.bestheme.ac.cn/v1alpha1`。
- **Secret 不进 informer cache**：代码中禁止对 Secret 调用 `List` / `Watches`。
- **私钥、AK/SK、SDK 响应体绝不出现在日志、event、status、error message、测试报告中**；日志只允许 `namespace/name`、`domainName`、`bucket`、certRef、指纹前 8 位。
- 错误分类沿用 `aliyun.classifyCode`；provider 层只认 `aliyun.ErrClass`，不按错误码字面量做推测式兜底。
- 事件 Message 不含变量（spec §10.2）；事件与计数器只在跃迁时记一次。
- Commit message **不加 `Co-Authored-By` / `Claude-Session` trailer**（仓库所有者规则）。conventional commits：`feat:` / `test:` / `refactor:` / `docs:` / `chore:`。
- 代码注释用中文；标识符、API 名英文。lll 120 列；每个 `//nolint` 点名 linter 并同行写理由。
- 每个 Task 结束时 `make build && make test && make lint` 通过；Task 9 起还要 `make verify-ram-policy` 通过。
- **（新）OSS 绑定只传 CAS certId，绝不向 OSS 内联 `Certificate` / `PrivateKey`。** `PutCname` 请求体里只允许 `CertId` + `Force`，或 `DeleteCertificate`。
- **（新）certRef 字符串 `"<certId>-<casRegion>"` 只在 `provider.CertMaterial.CASCertRef()` 一处拼装。** 任何其它地方需要它都调这个方法。
- **（新）通用层对「目标上是哪张证书」的一切比对都走 `certIdentity` 三元组**（`internal/controller/binding_identity.go`），不再直接读 `obs.CurrentFingerprint` / `obs.CurrentCertRef`。
- **（新）`RequiresCASUpload` 未满足时 Binding 报 `CASUploadRequired`，不替 AliyunCertificate 开上传**（OSS spec §5.2、D21）。
- **（新）OSS 调用经 `LimitOSS`（5 QPS / burst 1）限流，超时 `--cloud-call-timeout`，SDK 自身重试关掉（`WithRetryMaxAttempts(1)`）。** 重试节奏由 controller-runtime 决定。
- **（新）现有 FC3 路径行为不变**：所有既有测试原样通过；对 FC3 Binding，`status.appliedCertRef` / `driftedCertRef` 恒为空。
- **（新）Plan 4 不做**：CNAME 记录的创建 / 删除 / 所有权验证、向 OSS 内联 PEM、CDN / CLB provider。

---

## 文件结构

| 路径 | 职责 | Task |
|---|---|---|
| `api/v1alpha1/aliyuncertificatebinding_types.go`（改） | `OSSCustomDomainTarget`、union 第二条 CEL、`TargetKey` 分支、status 新字段 | 1 |
| `api/v1alpha1/conditions.go`（改） | `ReasonCASUploadRequired` | 1 |
| `api/v1alpha1/targetkey_test.go` | `TargetKey()` 两个类型的单测 | 1 |
| `pkg/provider/provider.go`（改） | `CertMaterial.CASRegion` / `CASCertRef()`、`ObservedState.CurrentCertRef` | 2 |
| `pkg/provider/material_test.go` | `CASCertRef()` 三态 | 2 |
| `pkg/aliyun/oss.go` | `OSSClient` 窄接口、`Cname`、`OSSClientConfig`、action 常量 | 3 |
| `pkg/aliyun/oss_sdk.go` | SDK v2 实现、凭证适配器、`classifyOSS`、`cnameFromList` | 3 |
| `pkg/aliyun/oss_sdk_test.go` / `oss_contract_test.go` | 纯函数单测、SDK 契约 | 3 |
| `pkg/aliyun/ratelimit.go`（改） | `LimitOSS` | 3 |
| `pkg/aliyun/fake/oss.go` | 内存 fake OSS | 3 |
| `pkg/provider/internal/aliyunerr/errors.go` | fc3 与 oss 共用的 `aliyun.Error → ProviderError` 映射 | 4 |
| `pkg/provider/fc3/errors.go`（改） | 委派给 `aliyunerr` | 4 |
| `pkg/provider/oss/provider.go` | OSS provider 三方法 | 4 |
| `pkg/provider/oss/provider_test.go` | 用 fake OSS 覆盖三方法与错误映射 | 4 |
| `internal/controller/binding_identity.go` | `certIdentity` / `identityOf` / `ensureHTTPSOf` / `casUploadGate` | 5, 6 |
| `internal/controller/binding_observe.go`、`binding_apply.go`、`binding_deletion.go`、`aliyuncertificatebinding_controller.go`（改） | 比对改走三元组；步骤 3c | 5, 6 |
| `internal/controller/binding_status.go`、`binding_material.go`、`provider_factory.go`（改） | target 辅助函数 OSS 分支；`CASRegion`；工厂分派 | 5, 6, 8 |
| `internal/controller/suite_oss_test.go` | envtest 里的 fake OSS 接线与 OSS fixture | 6 |
| `internal/controller/binding_oss_test.go` | OSS 路径 envtest | 6, 7 |
| `pkg/aliyun/cache.go`（改） | `ClientKey.Type` | 8 |
| `cmd/main.go`（改） | 单一 `ClientCache[provider.Client]` | 8 |
| `docs/ram/binding-oss-policy.json`、`full-policy.json`（改）、`hack/verify-ram-policy.py`（改）、`README.md`（改）、主 spec（改）、`config/samples/*`（改） | RAM 与文档 | 9 |
| `test/integration/oss_test.go`、`env.go`（改）、`env.example.sh`（改） | 探针 #15–#20 | 10 |
| `deploy/argocd/application-operator.yaml`（改） | v0.2.0 | 11 |

---

### Task 1: API 类型、CEL 与 status 字段

**Files:**
- Modify: `api/v1alpha1/aliyuncertificatebinding_types.go`
- Modify: `api/v1alpha1/conditions.go`
- Create: `api/v1alpha1/targetkey_test.go`
- Modify: `internal/controller/binding_crd_validation_test.go`
- Generated: `api/v1alpha1/zz_generated.deepcopy.go`、`config/crd/bases/certs.bestheme.ac.cn_aliyuncertificatebindings.yaml`

**Interfaces:**
- Produces: `certsv1alpha1.TargetTypeOSSCustomDomain = "OSSCustomDomain"`；`type OSSCustomDomainTarget struct{Region, Bucket, DomainName string}`；`BindingTarget.OSSCustomDomain *OSSCustomDomainTarget`；`AliyunCertificateBindingStatus.AppliedCertRef / DriftedCertRef string`；`certsv1alpha1.ReasonCASUploadRequired = "CASUploadRequired"`；`TargetKey()` 对 OSS 返回 `OSSCustomDomain/<region>/<bucket>/<domainName>`。

- [ ] **Step 1: 写 `TargetKey` 的失败单测**

创建 `api/v1alpha1/targetkey_test.go`（带 boilerplate 头，同目录其它文件照抄）：

```go
package v1alpha1

import "testing"

func TestTargetKey(t *testing.T) {
	cases := []struct {
		name string
		b    AliyunCertificateBinding
		want string
	}{
		{"fc3", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type:            TargetTypeFC3CustomDomain,
			FC3CustomDomain: &FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: "api.example.com"},
		}}}, "FC3CustomDomain/cn-hangzhou/api.example.com"},
		{"oss", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type:            TargetTypeOSSCustomDomain,
			OSSCustomDomain: &OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: "b1", DomainName: "www.example.com"},
		}}}, "OSSCustomDomain/cn-hangzhou/b1/www.example.com"},
		{"oss 缺内嵌块", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{
			Type: TargetTypeOSSCustomDomain,
		}}}, ""},
		{"未知类型", AliyunCertificateBinding{Spec: AliyunCertificateBindingSpec{Target: BindingTarget{Type: "Nope"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.TargetKey(); got != tc.want {
				t.Errorf("TargetKey() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认编译失败**

Run: `go test ./api/v1alpha1/ -run TestTargetKey`
Expected: FAIL，`undefined: TargetTypeOSSCustomDomain`。

- [ ] **Step 3: 改类型**

`api/v1alpha1/aliyuncertificatebinding_types.go`：

在 `const TargetTypeFC3CustomDomain = "FC3CustomDomain"` 下面加：

```go
// TargetTypeOSSCustomDomain 是第二个 provider 类型：OSS bucket 的自定义域名（CNAME）。
// 与 FC3 内联 PEM 不同，OSS 按 CAS certId 引用证书（Capabilities.ReferencesCertByID）。
const TargetTypeOSSCustomDomain = "OSSCustomDomain"
```

在 `FC3CustomDomainTarget` 结构体之后加：

```go
// OSSCustomDomainTarget 指向一个 OSS bucket 上已绑定、已通过所有权验证的自定义域名。
//
// 没有 ensureHTTPSProtocol：OSS CNAME 没有协议开关。
type OSSCustomDomainTarget struct {
	// bucket 所在 region，如 cn-hangzhou。决定 OSS endpoint，与 CAS 区域无关。
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	DomainName string `json:"domainName"`
}
```

把 `BindingTarget` 整段替换为：

```go
// BindingTarget 是 discriminated union：type 决定哪个内嵌块必须存在。两条 CEL 合起来
// 保证任意时刻恰好一个内嵌块存在。
// +kubebuilder:validation:XValidation:rule="self.type == 'FC3CustomDomain' ? has(self.fc3CustomDomain) : !has(self.fc3CustomDomain)",message="fc3CustomDomain 必须且只能在 type=FC3CustomDomain 时设置"
// +kubebuilder:validation:XValidation:rule="self.type == 'OSSCustomDomain' ? has(self.ossCustomDomain) : !has(self.ossCustomDomain)",message="ossCustomDomain 必须且只能在 type=OSSCustomDomain 时设置"
type BindingTarget struct {
	// +kubebuilder:validation:Enum=FC3CustomDomain;OSSCustomDomain
	Type string `json:"type"`
	// +optional
	FC3CustomDomain *FC3CustomDomainTarget `json:"fc3CustomDomain,omitempty"`
	// +optional
	OSSCustomDomain *OSSCustomDomainTarget `json:"ossCustomDomain,omitempty"`
}
```

在 status 的 `DriftedFingerprint` 字段之后加：

```go
	// 目标上实际引用的 CAS certId 字符串（形如 27087165-cn-hangzhou）。
	// 只有按 certId 引用证书的 provider（OSS）写；FC3 恒为空。appliedFingerprint 照旧写入，
	// 证书 controller 的保留护栏 3 只认它。
	// +optional
	AppliedCertRef string `json:"appliedCertRef,omitempty"`
	// 上一轮观测到的「漂移 certRef」，语义与 driftedFingerprint 逐条对应。
	// +optional
	DriftedCertRef string `json:"driftedCertRef,omitempty"`
```

`TargetKey()` 的 switch 加分支：

```go
	case TargetTypeOSSCustomDomain:
		if t.OSSCustomDomain == nil {
			return ""
		}
		return t.Type + "/" + t.OSSCustomDomain.Region + "/" + t.OSSCustomDomain.Bucket + "/" + t.OSSCustomDomain.DomainName
```

并把 `TargetKey` 的 doc 注释改为「返回 "<type>/<region>/<identifier>"（OSS 为 "<type>/<region>/<bucket>/<domainName>"），用于同目标冲突索引」。

`api/v1alpha1/conditions.go` 的 Binding reasons 块里，`ReasonDomainNotCovered` 之后加：

```go
	// ReasonCASUploadRequired：目标类型按 certId 引用证书（RequiresCASUpload），而证书的
	// spec.aliyun.uploadToCAS 是 false。Binding 不替证书开上传，等用户改证书 spec。
	ReasonCASUploadRequired = "CASUploadRequired"
```

- [ ] **Step 4: 重新生成并跑单测**

Run: `make manifests generate && go test ./api/v1alpha1/ -run TestTargetKey -v`
Expected: PASS；`git diff --stat` 里出现 `zz_generated.deepcopy.go` 与 CRD yaml。检查 CRD yaml 里 `enum` 含 `OSSCustomDomain`、两条 `x-kubernetes-validations` 都在。

- [ ] **Step 5: 加 CRD 校验 envtest**

`internal/controller/binding_crd_validation_test.go` 的 `Describe` 里、`newBinding` 之后加 helper 与三条用例：

```go
	newOSSBinding := func(name string) *certsv1alpha1.AliyunCertificateBinding {
		return &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "cert"},
				Target: certsv1alpha1.BindingTarget{
					Type: certsv1alpha1.TargetTypeOSSCustomDomain,
					OSSCustomDomain: &certsv1alpha1.OSSCustomDomainTarget{
						Region: "cn-hangzhou", Bucket: "bucket-" + ns,
						DomainName: fmt.Sprintf("%s.%s.example.com", name, ns),
					},
				},
			},
		}
	}

	It("接受 type=OSSCustomDomain 且 TargetKey 含 bucket", func() {
		b := newOSSBinding("oss-ok")
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		got := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "oss-ok", Namespace: ns}, got)).To(Succeed())
		Expect(got.TargetKey()).To(Equal(fmt.Sprintf("OSSCustomDomain/cn-hangzhou/bucket-%s/oss-ok.%s.example.com", ns, ns)))
	})

	It("拒绝 type=OSSCustomDomain 但缺少 ossCustomDomain", func() {
		b := newOSSBinding("oss-missing")
		b.Spec.Target.OSSCustomDomain = nil
		Expect(k8sClient.Create(ctx, b)).To(MatchError(ContainSubstring(
			"ossCustomDomain 必须且只能在 type=OSSCustomDomain 时设置")))
	})

	It("拒绝同时带 fc3CustomDomain 与 ossCustomDomain", func() {
		b := newBinding("both-blocks")
		b.Spec.Target.OSSCustomDomain = &certsv1alpha1.OSSCustomDomainTarget{
			Region: "cn-hangzhou", Bucket: "b", DomainName: "x.example.com",
		}
		Expect(k8sClient.Create(ctx, b)).To(MatchError(ContainSubstring(
			"ossCustomDomain 必须且只能在 type=OSSCustomDomain 时设置")))
	})

	It("拒绝不合法的 bucket 名", func() {
		b := newOSSBinding("oss-bad-bucket")
		b.Spec.Target.OSSCustomDomain.Bucket = "Bad_Bucket"
		Expect(k8sClient.Create(ctx, b)).To(MatchError(ContainSubstring("spec.target.ossCustomDomain.bucket")))
	})
```

- [ ] **Step 6: 跑 envtest 套件与 lint**

Run: `make test && make lint`
Expected: 全部 PASS。这些 Binding 没有 provider 也没有证书，reconciler 会停在 `CertificateNotFound`，不影响其它用例。

- [ ] **Step 7: Commit**

```bash
git add api/ config/crd/ internal/controller/binding_crd_validation_test.go
git commit -m "feat(api): add the OSSCustomDomain binding target and certRef status fields"
```

---

### Task 2: provider 契约扩展

**Files:**
- Modify: `pkg/provider/provider.go`
- Create: `pkg/provider/material_test.go`

**Interfaces:**
- Produces: `CertMaterial.CASRegion string`；`func (m CertMaterial) CASCertRef() string`；`ObservedState.CurrentCertRef string`。

- [ ] **Step 1: 失败单测**

`pkg/provider/material_test.go`：

```go
package provider

import "testing"

func TestCertMaterial_CASCertRef(t *testing.T) {
	id := int64(27087165)
	cases := []struct {
		name string
		m    CertMaterial
		want string
	}{
		{"正常", CertMaterial{CertID: &id, CASRegion: "cn-hangzhou"}, "27087165-cn-hangzhou"},
		{"certId 为 nil", CertMaterial{CASRegion: "cn-hangzhou"}, ""},
		{"CAS 区域为空", CertMaterial{CertID: &id}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.CASCertRef(); got != tc.want {
				t.Errorf("CASCertRef() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/provider/ -run TestCertMaterial_CASCertRef`
Expected: FAIL，`m.CASCertRef undefined`。

- [ ] **Step 3: 改契约**

`pkg/provider/provider.go`：`CertMaterial` 在 `DNSNames` 之后加字段，并加方法（放在结构体定义之后）：

```go
	// CASRegion 是证书上传所在的 CAS 区域（AliyunSpec.EffectiveCASRegion()）。只用来拼
	// CASCertRef；内联 PEM 的 provider 忽略它。
	CASRegion string
```

```go
// CASCertRef 返回按 ID 引用证书的目标（OSS）使用的字符串 "<certId>-<casRegion>"。
// CertID 为 nil 或 CASRegion 为空时返回 ""——那两种情况都还没有一个可引用的云侧证书。
//
// 这是该字符串**唯一**的拼装点：通用层的对账、provider 的写入、测试的断言全部从这里取值，
// 免得三处各拼一套、某一处少个连字符就永远短路不了。
func (m CertMaterial) CASCertRef() string {
	if m.CertID == nil || m.CASRegion == "" {
		return ""
	}
	return strconv.FormatInt(*m.CertID, 10) + "-" + m.CASRegion
}
```

（import 加 `"strconv"`。）

`ObservedState` 改为：

```go
type ObservedState struct {
	CurrentFingerprint string // 内联 PEM 的目标：实际证书指纹；"" = 无证书。OSS 恒为 ""
	CurrentCertRef     string // 按 ID 引用的目标：实际引用的 certRef；"" = 无证书。FC3 恒为 ""
	Protocol           string
	AccountID          string // 账号 fencing 用
}
```

`CertID` 字段注释改为 `// CAS certId；ReferencesCertByID=false 的 provider 忽略，=true 的 provider 经 CASCertRef() 使用`。

- [ ] **Step 4: 跑测试**

Run: `go test ./pkg/provider/... && make build`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add pkg/provider/provider.go pkg/provider/material_test.go
git commit -m "feat(provider): carry the CAS region and a certRef view for ID-referencing targets"
```

---

### Task 3: `pkg/aliyun` 的 OSS client、凭证适配器、限流与 fake

**Files:**
- Create: `pkg/aliyun/oss.go`、`pkg/aliyun/oss_sdk.go`、`pkg/aliyun/oss_sdk_test.go`、`pkg/aliyun/oss_contract_test.go`
- Modify: `pkg/aliyun/ratelimit.go`
- Create: `pkg/aliyun/fake/oss.go`、`pkg/aliyun/fake/oss_test.go`
- Modify: `go.mod` / `go.sum`

**Interfaces:**
- Produces: `aliyun.OSSClient`（`GetCname` / `PutCnameCert` / `DeleteCnameCert`）、`aliyun.Cname{Domain, Status, CertRef, CertType, AccountID}`、`aliyun.OSSClientConfig{Region, Timeout, Limiters, LimiterKey, OnCall}`、`aliyun.NewOSSClient(cred credential.Credential, cfg OSSClientConfig) (OSSClient, error)`、`aliyun.ActionListCname = "ListCname"`、`aliyun.ActionPutCname = "PutCname"`、`aliyun.LimitOSS`；`fake.OSS`（`NewOSS`、`SetOwner`、`AddBucket`、`AddCname`、`Cname`、`QueueListErr`、`QueuePutErr`、`AlwaysPutErr`、`ListCallsFor`、`PutCallsFor`）、`fake.ErrNoSuchBucket`、`fake.ErrCnameNotFound`。
- 已核实的 SDK 事实（2026-09-07 `go doc`）：`oss.PutCnameRequest{Bucket *string; BucketCnameConfiguration *BucketCnameConfiguration}`；`oss.BucketCnameConfiguration.Cname *oss.Cname`（顶层 `Domain` / `CertificateConfiguration` 已 Deprecated，不要用）；`oss.Cname{Domain *string; CertificateConfiguration *CertificateConfiguration}`；`oss.CertificateConfiguration{CertId, Certificate, PrivateKey, PreviousCertId *string; Force, DeleteCertificate *bool}`；`oss.ListCnameRequest{Bucket *string}`；`oss.ListCnameResult{Cnames []CnameInfo; Bucket, Owner *string}`；`oss.CnameInfo{Domain, LastModified, Status *string; Certificate *CnameCertificate; IsWildCard *bool}`；`oss.CnameCertificate{Type, CertId, Status, CreationDate, Fingerprint, ValidStartDate, ValidEndDate *string}`；`oss.ServiceError{Code, Message, RequestID, EC string; StatusCode int; …}`；`oss.Ptr[T]`、`oss.ToString(*string)`；`oss.LoadDefaultConfig().WithRegion / WithCredentialsProvider / WithConnectTimeout / WithReadWriteTimeout / WithRetryMaxAttempts / WithUserAgent`；`oss.NewClient(cfg *Config, ...) *Client`；`credentials.CredentialsProvider` 单方法 `GetCredentials(ctx) (credentials.Credentials, error)`；credentials-go `Credential.GetCredential() (*CredentialModel, error)`，模型字段 `AccessKeyId / AccessKeySecret / SecurityToken *string`。

- [ ] **Step 1: 引入依赖**

```bash
go get github.com/aliyun/alibabacloud-oss-go-sdk-v2@v1.6.0 && go mod tidy
```

Expected: `go.mod` 出现 `github.com/aliyun/alibabacloud-oss-go-sdk-v2 v1.6.0`（tidy 后它可能标 `// indirect`，加了代码之后再 tidy 一次会转正）。

- [ ] **Step 2: 写 `oss.go`（接口）**

```go
package aliyun

import (
	"context"
	"time"
)

// OSS 的 OpenAPI action 名，供 OnCall 指标与 Error.Op 使用。
//
// Put 与 Delete 走同一个 OpenAPI（PutCname，只是 CertificateConfiguration 的字段不同），
// 所以指标 action 同名；Op 里区分不了两者是有意的——RAM 上它们也是同一个 oss:PutCname。
const (
	ActionListCname = "ListCname"
	ActionPutCname  = "PutCname"
)

// Cname 是 ListCname 里一条 CNAME 记录被剪裁后的只读视图。没有私钥、没有响应体。
type Cname struct {
	Domain    string
	Status    string // Enabled | Disabled
	CertRef   string // Certificate.CertId，形如 "27087165-cn-hangzhou"；无证书时 ""
	CertType  string // CAS | Upload；无证书时 ""
	AccountID string // ListCname.Owner，账号 fencing 用
}

// OSSClient 是本项目对 OSS 的全部依赖：读一条 CNAME 的证书引用、按 CAS certId 换绑、摘证书。
//
// 不创建 / 删除 CNAME，不做所有权验证（CreateCnameToken）：证书归属于已存在的域名，
// 与 FC3 侧「证书归属于域名，与函数无关」是同一条边界。
type OSSClient interface {
	// GetCname 列出 bucket 的 CNAME 并按 domain 精确匹配（大小写不敏感）。
	// 找不到该 domain 返回 Class=NotFound、Code=CnameNotFound；bucket 不存在由 SDK 的
	// NoSuchBucket / 404 自然落入 NotFound。
	GetCname(ctx context.Context, bucket, domain string) (*Cname, error)
	// PutCnameCert 以 Force=true 覆盖式地把 certRef（"<certId>-<casRegion>"）绑到 domain。
	PutCnameCert(ctx context.Context, bucket, domain, certRef string) error
	// DeleteCnameCert 摘掉 domain 上的证书，CNAME 记录保留。
	DeleteCnameCert(ctx context.Context, bucket, domain string) error
}

// OSSClientConfig 是构造真实 OSS client 的参数，与 FC3ClientConfig 同构。
//
// 没有 Endpoint 覆盖：endpoint 一律由 SDK 按 region 推出（oss-<region>.aliyuncs.com），
// 理由与 FC3 相同——AliyunCertificate 的 endpointOverride 是给 CAS 用的。
type OSSClientConfig struct {
	Region     string
	Timeout    time.Duration
	Limiters   *Limiters
	LimiterKey string
	// OnCall 语义与 FC3ClientConfig.OnCall 完全一致。
	OnCall func(action, code string, d time.Duration)
}
```

- [ ] **Step 3: 写 `oss_sdk.go`**

```go
package aliyun

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	osscred "github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	credential "github.com/aliyun/credentials-go/credentials"
)

// ossCredentials 把 credentials-go 的 Credential 适配成 OSS SDK v2 的 CredentialsProvider。
//
// 两边都是「每次调用时取一次当前凭证」的语义，AK/SK、STS、RoleARN、OIDC 因此全部沿现有
// 路径生效：轮换与刷新由 credentials-go 负责，这里只做搬运。
type ossCredentials struct{ cred credential.Credential }

func (p ossCredentials) GetCredentials(_ context.Context) (osscred.Credentials, error) {
	m, err := p.cred.GetCredential()
	if err != nil {
		return osscred.Credentials{}, err
	}
	if m == nil {
		return osscred.Credentials{}, errors.New("credentials-go 返回了空的凭证模型")
	}
	return osscred.Credentials{
		AccessKeyID:     dara.StringValue(m.AccessKeyId),
		AccessKeySecret: dara.StringValue(m.AccessKeySecret),
		SecurityToken:   dara.StringValue(m.SecurityToken),
	}, nil
}

type sdkOSS struct {
	c   *oss.Client
	cfg OSSClientConfig
}

// NewOSSClient 用 OSS SDK v2 构造 OSSClient。
//
// WithRetryMaxAttempts(1)：SDK 自带的重试会绕开 Limiters，而重试节奏本来就归
// controller-runtime。超时拆成连接 5s + 读写 cfg.Timeout，与 FC3 client 同一形状。
func NewOSSClient(cred credential.Credential, cfg OSSClientConfig) (OSSClient, error) {
	if cfg.Region == "" {
		return nil, &Error{Class: ClassPermanent, Op: "NewOSSClient", Code: "RegionRequired",
			Err: errors.New("OSS client 需要 region")}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Limiters == nil {
		cfg.Limiters = NewLimiters()
	}
	c := oss.NewClient(oss.LoadDefaultConfig().
		WithRegion(cfg.Region).
		WithCredentialsProvider(ossCredentials{cred: cred}).
		WithConnectTimeout(5 * time.Second).
		WithReadWriteTimeout(cfg.Timeout).
		WithRetryMaxAttempts(1).
		WithUserAgent("le-to-alicloud"))
	return &sdkOSS{c: c, cfg: cfg}, nil
}

func (s *sdkOSS) observe(action string, start time.Time, err error) {
	if s.cfg.OnCall == nil {
		return
	}
	s.cfg.OnCall(action, callCode(err), time.Since(start))
}

func (s *sdkOSS) GetCname(ctx context.Context, bucket, domain string) (*Cname, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitOSS); err != nil {
		return nil, Classify(ActionListCname, err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	start := time.Now()
	res, err := s.c.ListCname(cctx, &oss.ListCnameRequest{Bucket: oss.Ptr(bucket)})
	cerr := classifyOSS(ActionListCname, err)
	s.observe(ActionListCname, start, cerr)
	if cerr != nil {
		return nil, cerr
	}
	return cnameFromList(res, domain)
}

func (s *sdkOSS) PutCnameCert(ctx context.Context, bucket, domain, certRef string) error {
	if certRef == "" {
		return &Error{Class: ClassPermanent, Op: ActionPutCname, Code: "CertRefRequired",
			Err: errors.New("certRef 为空")}
	}
	// 只带 CertId + Force：不带 PreviousCertId 时 OSS 要求 Force=true（文档明说），而
	// Certificate / PrivateKey 字段在本项目里永远不填——私钥不走 OSS 这条链路。
	return s.putCname(ctx, bucket, domain, &oss.CertificateConfiguration{
		CertId: oss.Ptr(certRef),
		Force:  oss.Ptr(true),
	})
}

func (s *sdkOSS) DeleteCnameCert(ctx context.Context, bucket, domain string) error {
	return s.putCname(ctx, bucket, domain, &oss.CertificateConfiguration{
		DeleteCertificate: oss.Ptr(true),
	})
}

func (s *sdkOSS) putCname(ctx context.Context, bucket, domain string, cc *oss.CertificateConfiguration) error {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitOSS); err != nil {
		return Classify(ActionPutCname, err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	start := time.Now()
	_, err := s.c.PutCname(cctx, &oss.PutCnameRequest{
		Bucket: oss.Ptr(bucket),
		BucketCnameConfiguration: &oss.BucketCnameConfiguration{
			Cname: &oss.Cname{Domain: oss.Ptr(domain), CertificateConfiguration: cc},
		},
	})
	cerr := classifyOSS(ActionPutCname, err)
	s.observe(ActionPutCname, start, cerr)
	return cerr
}

// classifyOSS 把 OSS SDK v2 的错误折进本包的分类。
//
// *oss.ServiceError 只取 Code 与 StatusCode——Message、Snapshot（响应体）一律丢弃，
// 与 fromSDKError 对 tea/dara 错误的处理完全一致。其它错误（超时、网络）交给 Classify。
func classifyOSS(op string, err error) error {
	if err == nil {
		return nil
	}
	var se *oss.ServiceError
	if errors.As(err, &se) {
		code, status := se.Code, se.StatusCode
		return fromSDKError(op, &code, &status)
	}
	return Classify(op, err)
}

// cnameFromList 在 ListCname 的结果里找 domain。找不到就是 NotFound——对 Observe 而言
// 「bucket 在、CNAME 不在」与「bucket 不在」一样都是目标不存在。
func cnameFromList(res *oss.ListCnameResult, domain string) (*Cname, error) {
	if res == nil {
		return nil, &Error{Class: ClassRetryable, Op: ActionListCname, Code: "EmptyResponse",
			Err: errors.New("响应为空")}
	}
	for i := range res.Cnames {
		c := &res.Cnames[i]
		if !strings.EqualFold(oss.ToString(c.Domain), domain) {
			continue
		}
		out := &Cname{
			Domain:    oss.ToString(c.Domain),
			Status:    oss.ToString(c.Status),
			AccountID: oss.ToString(res.Owner),
		}
		if c.Certificate != nil {
			out.CertRef = oss.ToString(c.Certificate.CertId)
			out.CertType = oss.ToString(c.Certificate.Type)
		}
		return out, nil
	}
	return nil, &Error{Class: ClassNotFound, Op: ActionListCname, Code: "CnameNotFound",
		Err: errors.New("bucket 上没有该自定义域名")}
}

var _ OSSClient = (*sdkOSS)(nil)
var _ osscred.CredentialsProvider = ossCredentials{}
```

- [ ] **Step 4: `ratelimit.go` 加 `LimitOSS`**

在 `LimitFC3` 之后加：

```go
	// LimitOSS 对应 ListCname / PutCname。OSS 没有公布 CNAME 接口的频控阈值，与 FC3 同取
	// 5 QPS / burst 1 的保守值（spec 2026-09-07 §6.4）；实测后再放宽。
	LimitOSS
```

`newLimiter` 的 switch 把 `case LimitFC3:` 改成 `case LimitFC3, LimitOSS:`。

- [ ] **Step 5: 写单测 `oss_sdk_test.go`**

```go
package aliyun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	credential "github.com/aliyun/credentials-go/credentials"
)

func TestCnameFromList(t *testing.T) {
	res := &oss.ListCnameResult{
		Bucket: oss.Ptr("b1"), Owner: oss.Ptr("1234567890"),
		Cnames: []oss.CnameInfo{
			{Domain: oss.Ptr("bare.example.com"), Status: oss.Ptr("Enabled")},
			{Domain: oss.Ptr("www.example.com"), Status: oss.Ptr("Enabled"),
				Certificate: &oss.CnameCertificate{Type: oss.Ptr("CAS"), CertId: oss.Ptr("27087165-cn-hangzhou")}},
		},
	}
	got, err := cnameFromList(res, "WWW.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.CertRef != "27087165-cn-hangzhou" || got.CertType != "CAS" || got.AccountID != "1234567890" ||
		got.Domain != "www.example.com" {
		t.Fatalf("剪裁结果不对: %+v", got)
	}

	bare, err := cnameFromList(res, "bare.example.com")
	if err != nil || bare.CertRef != "" || bare.CertType != "" {
		t.Fatalf("无证书的 CNAME 应给出空 certRef: %+v, %v", bare, err)
	}

	_, err = cnameFromList(res, "absent.example.com")
	if ClassOf(err) != ClassNotFound {
		t.Fatalf("找不到 domain 应为 NotFound: %v", err)
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "CnameNotFound" {
		t.Fatalf("Code 应为 CnameNotFound: %v", err)
	}
}

func TestClassifyOSS(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrClass
		code string
	}{
		{"nil", nil, ClassPermanent, ""},
		{"NoSuchBucket 404", &oss.ServiceError{Code: "NoSuchBucket", StatusCode: 404}, ClassNotFound, "NoSuchBucket"},
		{"AccessDenied 403", &oss.ServiceError{Code: "AccessDenied", StatusCode: 403}, ClassAuth, "AccessDenied"},
		{"InvalidAccessKeyId", &oss.ServiceError{Code: "InvalidAccessKeyId", StatusCode: 403}, ClassAuth, "InvalidAccessKeyId"},
		{"5xx", &oss.ServiceError{Code: "InternalError", StatusCode: 500}, ClassRetryable, "InternalError"},
		{"参数错误", &oss.ServiceError{Code: "InvalidArgument", StatusCode: 400}, ClassPermanent, "InvalidArgument"},
		{"超时", context.DeadlineExceeded, ClassRetryable, "Timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOSS(ActionPutCname, tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("nil 应原样返回: %v", got)
				}
				return
			}
			if ClassOf(got) != tc.want {
				t.Errorf("class = %s, want %s", ClassOf(got), tc.want)
			}
			var ae *Error
			if !errors.As(got, &ae) || ae.Code != tc.code || ae.Op != ActionPutCname {
				t.Errorf("Code/Op 不对: %v", got)
			}
		})
	}
}

// 响应体绝不能进错误链：Message 与 Snapshot 都是可能回显请求参数的地方。
func TestClassifyOSS_DropsBody(t *testing.T) {
	err := classifyOSS(ActionListCname, &oss.ServiceError{
		Code: "AccessDenied", StatusCode: 403, Message: "SECRET-IN-MESSAGE", Snapshot: []byte("SECRET-IN-BODY"),
	})
	for _, leak := range []string{"SECRET-IN-MESSAGE", "SECRET-IN-BODY"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误链带出了响应内容: %v", err)
		}
	}
}

// stubCredential 只实现本测试用到的 GetCredential；其余方法返回零值。
type stubCredential struct {
	credential.Credential
	model *credential.CredentialModel
	err   error
}

func (s stubCredential) GetCredential() (*credential.CredentialModel, error) { return s.model, s.err }

func TestOSSCredentials_Adapter(t *testing.T) {
	p := ossCredentials{cred: stubCredential{model: &credential.CredentialModel{
		AccessKeyId: dara.String("LTAI-FAKE-ID"), AccessKeySecret: dara.String("FAKE-SECRET"), SecurityToken: dara.String("FAKE-STS"),
	}}}
	got, err := p.GetCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "LTAI-FAKE-ID" || got.AccessKeySecret != "FAKE-SECRET" || got.SecurityToken != "FAKE-STS" {
		t.Fatalf("字段搬运不对: %+v", got)
	}

	if _, err := (ossCredentials{cred: stubCredential{err: errors.New("boom")}}).GetCredentials(context.Background()); err == nil {
		t.Fatal("底层错误应透传")
	}
	if _, err := (ossCredentials{cred: stubCredential{}}).GetCredentials(context.Background()); err == nil {
		t.Fatal("空模型应报错而不是返回空凭证")
	}
}

func TestNewOSSClient_RequiresRegion(t *testing.T) {
	if _, err := NewOSSClient(stubCredential{}, OSSClientConfig{}); ClassOf(err) != ClassPermanent {
		t.Fatalf("缺 region 应为 Permanent: %v", err)
	}
	c, err := NewOSSClient(stubCredential{}, OSSClientConfig{Region: "cn-hangzhou"})
	if err != nil || c == nil {
		t.Fatalf("有 region 应构造成功: %v", err)
	}
}
```

- [ ] **Step 6: 写契约测试 `oss_contract_test.go`**

```go
package aliyun

import (
	"context"
	"testing"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
)

// TestOSSSDKContract 把本项目依赖的 SDK v2 字段名与方法签名钉在编译期：SDK 升级改了名字，
// 这里先红。只做绑定，不发请求。
func TestOSSSDKContract(t *testing.T) {
	req := &oss.PutCnameRequest{
		Bucket: oss.Ptr("b"),
		BucketCnameConfiguration: &oss.BucketCnameConfiguration{
			Cname: &oss.Cname{
				Domain: oss.Ptr("www.example.com"),
				CertificateConfiguration: &oss.CertificateConfiguration{
					CertId: oss.Ptr("1-cn-hangzhou"), Force: oss.Ptr(true), DeleteCertificate: oss.Ptr(false),
				},
			},
		},
	}
	if req.BucketCnameConfiguration.Cname.CertificateConfiguration.CertId == nil {
		t.Fatal("PutCnameRequest…CertificateConfiguration.CertId 不可达")
	}
	res := &oss.ListCnameResult{Owner: oss.Ptr("1"), Cnames: []oss.CnameInfo{{
		Domain: oss.Ptr("d"), Status: oss.Ptr("Enabled"),
		Certificate: &oss.CnameCertificate{CertId: oss.Ptr("1-cn-hangzhou"), Type: oss.Ptr("CAS")},
	}}}
	if res.Cnames[0].Certificate.CertId == nil {
		t.Fatal("ListCnameResult.Cnames[].Certificate.CertId 不可达")
	}
	var c *oss.Client
	//nolint:staticcheck // QF1011: 显式函数类型正是本契约测试的意义，类型推断会让签名漂移无法被编译期发现
	var list func(context.Context, *oss.ListCnameRequest, ...func(*oss.Options)) (*oss.ListCnameResult, error) = c.ListCname
	//nolint:staticcheck // QF1011: 同上
	var put func(context.Context, *oss.PutCnameRequest, ...func(*oss.Options)) (*oss.PutCnameResult, error) = c.PutCname
	if list == nil || put == nil {
		t.Fatal("方法值绑定失败")
	}
	se := &oss.ServiceError{Code: "c", StatusCode: 400}
	if se.Code == "" || se.StatusCode == 0 {
		t.Fatal("ServiceError.Code / StatusCode 不可达")
	}
}
```

- [ ] **Step 7: 跑 aliyun 包测试**

Run: `go test ./pkg/aliyun/ -run 'OSS|Cname|ClassifyOSS' -v`
Expected: 全部 PASS。

- [ ] **Step 8: 写 fake `pkg/aliyun/fake/oss.go`**

```go
package fake

import (
	"context"
	"errors"
	"strings"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrNoSuchBucket / ErrCnameNotFound 是 fake 的两个 NotFound 哨兵，与真实 client 的分类一致。
var (
	ErrNoSuchBucket = &aliyun.Error{
		Class: aliyun.ClassNotFound, Op: aliyun.ActionListCname,
		Code: "NoSuchBucket", Err: errors.New("bucket 不存在"),
	}
	ErrCnameNotFound = &aliyun.Error{
		Class: aliyun.ClassNotFound, Op: aliyun.ActionListCname,
		Code: "CnameNotFound", Err: errors.New("bucket 上没有该自定义域名"),
	}
)

// CnameRecord 是 fake 里一条 CNAME 的服务端状态。
type CnameRecord struct {
	Domain   string
	CertRef  string // "" = 无证书
	CertType string // 绑定后为 "CAS"
}

// OSS 实现 aliyun.OSSClient。所有状态在 mu 之下；调用计数按 (bucket, domain) 分账，
// 理由与 fake.FC3 的 getCallsFor 相同：envtest 里 reconciler 常驻，其它用例的 Binding
// 还活着，全局计数不属于任何一个用例。
type OSS struct {
	mu sync.Mutex

	owner   string
	buckets map[string]map[string]CnameRecord // bucket → 小写 domain → 记录

	listErrs     []error
	putErrs      []error
	alwaysPutErr error

	listCallsFor map[string]int
	putCallsFor  map[string]int
}

func NewOSS() *OSS {
	return &OSS{
		buckets:      map[string]map[string]CnameRecord{},
		listCallsFor: map[string]int{},
		putCallsFor:  map[string]int{},
	}
}

func key(bucket, domain string) string { return bucket + "/" + strings.ToLower(domain) }

// SetOwner 设置 ListCname 回报的账号（账号 fencing 用例靠它切换账号）。
func (f *OSS) SetOwner(id string) { f.mu.Lock(); f.owner = id; f.mu.Unlock() }

// AddBucket 新建一个空 bucket（已存在则不动）。
func (f *OSS) AddBucket(bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[bucket]; !ok {
		f.buckets[bucket] = map[string]CnameRecord{}
	}
}

// AddCname 在 bucket 上新建或覆盖一条 CNAME；bucket 不存在时顺带建出来。
func (f *OSS) AddCname(bucket string, rec CnameRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[bucket]; !ok {
		f.buckets[bucket] = map[string]CnameRecord{}
	}
	f.buckets[bucket][strings.ToLower(rec.Domain)] = rec
}

// Cname 返回一条 CNAME 的服务端快照。
func (f *OSS) Cname(bucket, domain string) (CnameRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return CnameRecord{}, false
	}
	rec, ok := b[strings.ToLower(domain)]
	return rec, ok
}

// QueueListErr 让下一次 GetCname 返回该错误。
func (f *OSS) QueueListErr(err error) { f.mu.Lock(); f.listErrs = append(f.listErrs, err); f.mu.Unlock() }

// QueuePutErr 让下一次 PutCnameCert / DeleteCnameCert 在生效前返回该错误。
func (f *OSS) QueuePutErr(err error) { f.mu.Lock(); f.putErrs = append(f.putErrs, err); f.mu.Unlock() }

// AlwaysPutErr 让此后每一次写入都在生效前返回该错误（模拟缺 oss:PutCname 权限）。
func (f *OSS) AlwaysPutErr(err error) { f.mu.Lock(); f.alwaysPutErr = err; f.mu.Unlock() }

// ListCallsFor 返回某条 CNAME 上 GetCname 的调用次数。
func (f *OSS) ListCallsFor(bucket, domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCallsFor[key(bucket, domain)]
}

// PutCallsFor 返回某条 CNAME 上写入（Put 与 Delete 合计）的调用次数。
func (f *OSS) PutCallsFor(bucket, domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCallsFor[key(bucket, domain)]
}

func (f *OSS) GetCname(_ context.Context, bucket, domain string) (*aliyun.Cname, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCallsFor[key(bucket, domain)]++
	if err := pop(&f.listErrs); err != nil {
		return nil, err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return nil, ErrNoSuchBucket
	}
	rec, ok := b[strings.ToLower(domain)]
	if !ok {
		return nil, ErrCnameNotFound
	}
	return &aliyun.Cname{
		Domain: rec.Domain, Status: "Enabled",
		CertRef: rec.CertRef, CertType: rec.CertType, AccountID: f.owner,
	}, nil
}

func (f *OSS) PutCnameCert(_ context.Context, bucket, domain, certRef string) error {
	if certRef == "" {
		return &aliyun.Error{Class: aliyun.ClassPermanent, Op: aliyun.ActionPutCname,
			Code: "InvalidArgument", Err: errors.New("certRef 为空")}
	}
	return f.write(bucket, domain, func(rec *CnameRecord) { rec.CertRef, rec.CertType = certRef, "CAS" })
}

func (f *OSS) DeleteCnameCert(_ context.Context, bucket, domain string) error {
	return f.write(bucket, domain, func(rec *CnameRecord) { rec.CertRef, rec.CertType = "", "" })
}

func (f *OSS) write(bucket, domain string, mutate func(*CnameRecord)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCallsFor[key(bucket, domain)]++
	if f.alwaysPutErr != nil {
		return f.alwaysPutErr
	}
	if err := pop(&f.putErrs); err != nil {
		return err
	}
	b, ok := f.buckets[bucket]
	if !ok {
		return ErrNoSuchBucket
	}
	rec, ok := b[strings.ToLower(domain)]
	if !ok {
		return ErrCnameNotFound
	}
	mutate(&rec)
	b[strings.ToLower(domain)] = rec
	return nil
}

var _ aliyun.OSSClient = (*OSS)(nil)
```

`pop` 已在 `fake/cas.go` 或 `fake/fc3.go` 里定义（同包），直接用。若 `key` 与同包既有标识符冲突，改名为 `cnameKey`。

- [ ] **Step 9: fake 单测 `pkg/aliyun/fake/oss_test.go`**

```go
package fake

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func TestOSS_RoundTrip(t *testing.T) {
	ctx := context.Background()
	f := NewOSS()
	f.SetOwner("1234567890")
	f.AddCname("b1", CnameRecord{Domain: "www.example.com"})

	c, err := f.GetCname(ctx, "b1", "WWW.example.com")
	if err != nil || c.CertRef != "" || c.AccountID != "1234567890" {
		t.Fatalf("空证书 CNAME 观测不对: %+v %v", c, err)
	}
	if err := f.PutCnameCert(ctx, "b1", "www.example.com", "1-cn-hangzhou"); err != nil {
		t.Fatal(err)
	}
	c, _ = f.GetCname(ctx, "b1", "www.example.com")
	if c.CertRef != "1-cn-hangzhou" || c.CertType != "CAS" {
		t.Fatalf("写入后应观测到 certRef: %+v", c)
	}
	if err := f.DeleteCnameCert(ctx, "b1", "www.example.com"); err != nil {
		t.Fatal(err)
	}
	c, _ = f.GetCname(ctx, "b1", "www.example.com")
	if c.CertRef != "" {
		t.Fatalf("摘证书后 certRef 应为空: %+v", c)
	}
	if f.ListCallsFor("b1", "www.example.com") != 3 || f.PutCallsFor("b1", "www.example.com") != 2 {
		t.Fatalf("计数不对: list=%d put=%d", f.ListCallsFor("b1", "www.example.com"), f.PutCallsFor("b1", "www.example.com"))
	}
}

func TestOSS_NotFoundAndInjection(t *testing.T) {
	ctx := context.Background()
	f := NewOSS()
	if _, err := f.GetCname(ctx, "nope", "d"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("bucket 不存在: %v", err)
	}
	f.AddBucket("b1")
	if _, err := f.GetCname(ctx, "b1", "d"); !errors.Is(err, ErrCnameNotFound) {
		t.Fatalf("CNAME 不存在: %v", err)
	}
	if err := f.PutCnameCert(ctx, "b1", "d", ""); aliyun.ClassOf(err) != aliyun.ClassPermanent {
		t.Fatalf("空 certRef 应为 Permanent: %v", err)
	}
	boom := &aliyun.Error{Class: aliyun.ClassAuth, Op: aliyun.ActionPutCname, Code: "AccessDenied", Err: errors.New("x")}
	f.AddCname("b1", CnameRecord{Domain: "d"})
	f.AlwaysPutErr(boom)
	if err := f.PutCnameCert(ctx, "b1", "d", "1-cn-hangzhou"); !errors.Is(err, boom) {
		t.Fatalf("AlwaysPutErr 应生效: %v", err)
	}
	if rec, _ := f.Cname("b1", "d"); rec.CertRef != "" {
		t.Fatal("失败的写入不该生效")
	}
}
```

- [ ] **Step 10: 全量验证与 tidy**

Run: `go mod tidy && make build && make test && make lint`
Expected: 全部通过；`go.mod` 里 OSS SDK 不再标 `// indirect`。

- [ ] **Step 11: Commit**

```bash
git add go.mod go.sum pkg/aliyun/
git commit -m "feat(aliyun): add an OSS CNAME certificate client with a credentials adapter and an in-memory fake"
```

---

### Task 4: 共享错误映射与 OSS provider

**Files:**
- Create: `pkg/provider/internal/aliyunerr/errors.go`
- Modify: `pkg/provider/fc3/errors.go`（改成委派；`fc3` 的既有测试一个字不改）
- Create: `pkg/provider/oss/provider.go`、`pkg/provider/oss/errors.go`、`pkg/provider/oss/provider_test.go`

**Interfaces:**
- Consumes: Task 2 的 `CertMaterial.CASCertRef()`、`ObservedState.CurrentCertRef`；Task 3 的 `aliyun.OSSClient`、`fake.OSS`。
- Produces: `aliyunerr.ToProviderError(op string, err error, failReason string) error`、`aliyunerr.SwallowNotFound(err error) error`；`oss.Provider{}`（注册名 `OSSCustomDomain`，Capabilities `{ReferencesCertByID: true, SupportsProtocolSwitch: false, RequiresCASUpload: true}`）。

- [ ] **Step 1: 把 fc3 的映射搬进共享包**

创建 `pkg/provider/internal/aliyunerr/errors.go`。内容是 `pkg/provider/fc3/errors.go` 里 `toProviderError` / `asAliyunError` / `isThrottling` / `swallowNotFound` 四个函数**原样搬过来**（连同它们的注释），只做三处改动：`package aliyunerr`；`toProviderError` → `ToProviderError`、`swallowNotFound` → `SwallowNotFound`（导出）；文件头加一段包注释：

```go
// Package aliyunerr 是 fc3 与 oss 两个 provider 共用的 aliyun.Error → provider.ProviderError
// 映射。放在 pkg/provider/internal 下：只有 pkg/provider 子树能 import 它，通用层拿不到——
// 通用层按设计只认 ProviderError 的 Code / Retryable / Reason（spec §7 职责边界表）。
//
// 两个 provider 从前各持一份逐字相同的映射，dupl 会报，而更要紧的是错误码归类的落点该只有
// 一处（aliyun.classifyCode）、映射到 provider 语义的落点也该只有一处（这里）。
package aliyunerr
```

然后把 `pkg/provider/fc3/errors.go` 的四个函数体换成委派，保留原有的文件头注释：

```go
func toProviderError(op string, err error, failReason string) error {
	return aliyunerr.ToProviderError(op, err, failReason)
}

func swallowNotFound(err error) error { return aliyunerr.SwallowNotFound(err) }
```

（`asAliyunError` / `isThrottling` 在 fc3 包内若无其它调用点就删掉；`go vet` / `unused` 会告诉你。）

- [ ] **Step 2: 验证 fc3 无回归**

Run: `go test ./pkg/provider/... && make lint`
Expected: PASS；fc3 的 `provider_test.go` / `protocol_test.go` 一行未改仍然全绿。

- [ ] **Step 3: 写 OSS provider 的失败单测**

`pkg/provider/oss/provider_test.go`（外部测试包 `oss_test`，与 fc3 的写法一致）：

```go
package oss_test

import (
	"context"
	"errors"
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"
)

const (
	testBucket = "applanding-test"
	testDomain = "www.example.com"
	testRef    = "27087165-cn-hangzhou"
)

func targetFor() provider.Target {
	spec := &certsv1alpha1.OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: testBucket, DomainName: testDomain}
	return provider.Target{
		Type: certsv1alpha1.TargetTypeOSSCustomDomain, Region: "cn-hangzhou", Identifier: testDomain, Spec: spec,
	}
}

func materialFor() provider.CertMaterial {
	id := int64(27087165)
	return provider.CertMaterial{Fingerprint: "abc123", CertID: &id, CASRegion: "cn-hangzhou"}
}

func TestProvider_Identity(t *testing.T) {
	p := &oss.Provider{}
	if p.Name() != certsv1alpha1.TargetTypeOSSCustomDomain {
		t.Errorf("Name 必须与 CRD 的 target.type 取值一致: %s", p.Name())
	}
	c := p.Capabilities()
	if !c.ReferencesCertByID || !c.RequiresCASUpload || c.SupportsProtocolSwitch {
		t.Errorf("OSS 按 certId 引用、强制上传 CAS、无协议开关: %+v", c)
	}
	if got, ok := provider.Get(certsv1alpha1.TargetTypeOSSCustomDomain); !ok || got.Name() != p.Name() {
		t.Error("init() 应把自己注册进 registry")
	}
}

func TestProvider_ObserveApplyRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := fake.NewOSS()
	f.SetOwner("1234567890")
	f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
	p := &oss.Provider{}

	obs, err := p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentCertRef != "" || obs.CurrentFingerprint != "" || obs.AccountID != "1234567890" {
		t.Fatalf("空证书 CNAME 的观测不对: %+v", obs)
	}

	if err := p.Apply(ctx, targetFor(), f, materialFor(), provider.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	obs, err = p.Observe(ctx, targetFor(), f)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CurrentCertRef != testRef {
		t.Errorf("写入后应观测到同一 certRef: %s != %s", obs.CurrentCertRef, testRef)
	}
	if obs.CurrentFingerprint != "" {
		t.Error("OSS 不产出指纹，CurrentFingerprint 必须恒为空")
	}
	if f.PutCallsFor(testBucket, testDomain) != 1 {
		t.Errorf("Apply 应恰好写一次: %d", f.PutCallsFor(testBucket, testDomain))
	}
}

func TestProvider_ApplyRejectsEmptyCertRef(t *testing.T) {
	f := fake.NewOSS()
	f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
	m := materialFor()
	m.CertID = nil
	err := (&oss.Provider{}).Apply(context.Background(), targetFor(), f, m, provider.ApplyOptions{})
	pe := provider.ErrorOf(err)
	if pe == nil || pe.Code != provider.CodePermanent || pe.Reason != certsv1alpha1.ReasonApplyFailed {
		t.Fatalf("空 certRef 应为 Permanent/ApplyFailed: %v", err)
	}
	if f.PutCallsFor(testBucket, testDomain) != 0 {
		t.Error("空 certRef 不该发任何云调用")
	}
}

func TestProvider_ObserveTargetNotFound(t *testing.T) {
	p := &oss.Provider{}
	for name, f := range map[string]*fake.OSS{
		"bucket 不存在": fake.NewOSS(),
		"CNAME 不存在":  func() *fake.OSS { f := fake.NewOSS(); f.AddBucket(testBucket); return f }(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Observe(context.Background(), targetFor(), f)
			pe := provider.ErrorOf(err)
			if pe == nil || pe.Code != provider.CodeTargetNotFound || pe.Retryable ||
				pe.Reason != certsv1alpha1.ReasonTargetNotFound {
				t.Fatalf("应映射为不可重试的 TargetNotFound: %v", err)
			}
		})
	}
}

func TestProvider_ErrorMapping(t *testing.T) {
	ctx := context.Background()
	mk := func(class aliyun.ErrClass, code string) *aliyun.Error {
		return &aliyun.Error{Class: class, Op: aliyun.ActionPutCname, Code: code, Err: errors.New("injected")}
	}
	cases := []struct {
		name   string
		inject *aliyun.Error
		code   string
		retry  bool
		reason string
	}{
		{"鉴权", mk(aliyun.ClassAuth, "AccessDenied"), provider.CodeAuth, false, certsv1alpha1.ReasonCredentialsInvalid},
		{"限流", mk(aliyun.ClassRetryable, "Throttling.User"), provider.CodeThrottled, true, certsv1alpha1.ReasonThrottled},
		{"瞬时", mk(aliyun.ClassRetryable, "InternalError"), provider.CodeRetryable, true, certsv1alpha1.ReasonApplyFailed},
		{"永久", mk(aliyun.ClassPermanent, "InvalidArgument"), provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.NewOSS()
			f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain})
			f.QueuePutErr(tc.inject)
			err := (&oss.Provider{}).Apply(ctx, targetFor(), f, materialFor(), provider.ApplyOptions{})
			pe := provider.ErrorOf(err)
			if pe == nil || pe.Code != tc.code || pe.Retryable != tc.retry || pe.Reason != tc.reason {
				t.Fatalf("映射不对: %v", err)
			}
			if !errors.Is(err, tc.inject) {
				t.Error("必须保住 Unwrap 链")
			}
		})
	}
}

func TestProvider_Cleanup(t *testing.T) {
	ctx := context.Background()
	p := &oss.Provider{}

	t.Run("Orphan 零调用", func(t *testing.T) {
		f := fake.NewOSS()
		f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain, CertRef: testRef, CertType: "CAS"})
		if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyOrphan); err != nil {
			t.Fatal(err)
		}
		if f.ListCallsFor(testBucket, testDomain)+f.PutCallsFor(testBucket, testDomain) != 0 {
			t.Error("Orphan 不该发任何云调用")
		}
	})
	t.Run("Unbind 摘证书、CNAME 保留", func(t *testing.T) {
		f := fake.NewOSS()
		f.AddCname(testBucket, fake.CnameRecord{Domain: testDomain, CertRef: testRef, CertType: "CAS"})
		if err := p.Cleanup(ctx, targetFor(), f, provider.DeletionPolicyUnbind); err != nil {
			t.Fatal(err)
		}
		rec, ok := f.Cname(testBucket, testDomain)
		if !ok || rec.CertRef != "" {
			t.Fatalf("Unbind 后 CNAME 应保留且无证书: %+v %v", rec, ok)
		}
	})
	t.Run("Unbind 时目标已不存在视为成功", func(t *testing.T) {
		if err := p.Cleanup(ctx, targetFor(), fake.NewOSS(), provider.DeletionPolicyUnbind); err != nil {
			t.Fatalf("NotFound 应被吞掉: %v", err)
		}
	})
}

func TestProvider_InvalidClient(t *testing.T) {
	_, err := (&oss.Provider{}).Observe(context.Background(), targetFor(), "not-a-client")
	if pe := provider.ErrorOf(err); pe == nil || pe.Code != provider.CodeInvalidClient {
		t.Fatalf("传错 client 类型应为 CodeInvalidClient: %v", err)
	}
}

func TestProvider_InvalidTargetSpec(t *testing.T) {
	tg := targetFor()
	tg.Spec = &certsv1alpha1.FC3CustomDomainTarget{}
	_, err := (&oss.Provider{}).Observe(context.Background(), tg, fake.NewOSS())
	if pe := provider.ErrorOf(err); pe == nil || pe.Code != provider.CodeInvalidTarget {
		t.Fatalf("Target.Spec 断言失败应为 CodeInvalidTarget: %v", err)
	}
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `go test ./pkg/provider/oss/`
Expected: 编译失败，包不存在。

- [ ] **Step 5: 写 `pkg/provider/oss/errors.go`**

```go
package oss

import (
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/internal/aliyunerr"
)

// toProviderError / swallowNotFound 与 fc3 共用一份映射（pkg/provider/internal/aliyunerr）。
// 保留这两个薄包装，是为了让本包三个方法的错误路径读起来与 fc3 逐行同构。
func toProviderError(op string, err error, failReason string) error {
	return aliyunerr.ToProviderError(op, err, failReason)
}

func swallowNotFound(err error) error { return aliyunerr.SwallowNotFound(err) }
```

- [ ] **Step 6: 写 `pkg/provider/oss/provider.go`**

```go
package oss

import (
	"context"
	"errors"
	"fmt"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// Provider 是第二个 provider：OSS bucket 自定义域名（CNAME）上的证书。无状态，零值可用。
//
// 与 FC3 的本质差别是**目标上存的是 CAS certId 而不是 PEM**：Observe 回报 CurrentCertRef、
// Apply 只传 certRef、通用层按 certRef 对账（Capabilities.ReferencesCertByID）。私钥因此
// 完全不经过 OSS 这条链路。
type Provider struct{}

func init() { provider.Register(&Provider{}) }

func (p *Provider) Name() string { return certsv1alpha1.TargetTypeOSSCustomDomain }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ReferencesCertByID: true,
		// OSS CNAME 没有协议开关。
		SupportsProtocolSwitch: false,
		// 引用 certId 就必须先有 certId：通用层据此在证书 uploadToCAS=false 时报
		// CASUploadRequired，在 certId 尚未就位时等待（spec 2026-09-07 §5.2）。
		RequiresCASUpload: true,
	}
}

// clientOf 断言通用层传进来的 client。失败是接线错误，与 fc3.clientOf 同一套说法。
func clientOf(c provider.Client) (aliyun.OSSClient, error) {
	cl, ok := c.(aliyun.OSSClient)
	if !ok {
		return nil, provider.Errorf(provider.CodeInvalidClient, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 aliyun.OSSClient，得到 %T", c))
	}
	return cl, nil
}

// specOf 断言 Target.Spec。bucket 只在这里，Target.Identifier 只带 domainName。
func specOf(t provider.Target) (*certsv1alpha1.OSSCustomDomainTarget, error) {
	s, ok := t.Spec.(*certsv1alpha1.OSSCustomDomainTarget)
	if !ok || s == nil {
		return nil, provider.Errorf(provider.CodeInvalidTarget, false, certsv1alpha1.ReasonApplyFailed,
			fmt.Errorf("需要 *OSSCustomDomainTarget，得到 %T", t.Spec))
	}
	return s, nil
}

func (p *Provider) Observe(ctx context.Context, t provider.Target, c provider.Client) (provider.ObservedState, error) {
	cl, err := clientOf(c)
	if err != nil {
		return provider.ObservedState{}, err
	}
	s, err := specOf(t)
	if err != nil {
		return provider.ObservedState{}, err
	}
	cn, err := cl.GetCname(ctx, s.Bucket, s.DomainName)
	if err != nil {
		return provider.ObservedState{},
			toProviderError(aliyun.ActionListCname, err, certsv1alpha1.ReasonObserveFailed)
	}
	// CurrentFingerprint 刻意留空：OSS 只回报 certId，没有 PEM 可算指纹。通用层按
	// ReferencesCertByID 选用 CurrentCertRef 对账。
	return provider.ObservedState{CurrentCertRef: cn.CertRef, AccountID: cn.AccountID}, nil
}

func (p *Provider) Apply(
	ctx context.Context, t provider.Target, c provider.Client,
	m provider.CertMaterial, _ provider.ApplyOptions,
) error {
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	s, err := specOf(t)
	if err != nil {
		return err
	}
	ref := m.CASCertRef()
	if ref == "" {
		// 通用层的步骤 3c 已经挡住了 certId 未就位的情形；这里是防御，与 fc3 对空 CASName
		// 的处理同一形状。空字符串发上云只会换回一个说不清的参数错误。
		return provider.Errorf(provider.CodePermanent, false, certsv1alpha1.ReasonApplyFailed,
			errors.New("CertMaterial 没有可引用的 CAS certId（CertID 为 nil 或 CASRegion 为空）"))
	}
	// 不做 read-modify-write：PutCname 的 CertificateConfiguration 只碰证书配置，CNAME 的其它
	// 属性不受影响（OSS spec O1）。Force=true 是不带 PreviousCertId 时的必填（O3），
	// 与 FC3 一样接受 last-write-wins。ApplyOptions 整个忽略：没有协议开关，
	// PreviousFingerprint 对按 certId 引用的目标没有意义。
	if err := cl.PutCnameCert(ctx, s.Bucket, s.DomainName, ref); err != nil {
		return toProviderError(aliyun.ActionPutCname, err, certsv1alpha1.ReasonApplyFailed)
	}
	return nil
}

// Cleanup 在 deletionPolicy=Unbind 时摘掉证书，CNAME 记录保留（O4）。
//
// 「只解绑自己的」判断在通用层按 appliedCertRef 比对（spec §7 职责边界表），这里不做。
func (p *Provider) Cleanup(
	ctx context.Context, t provider.Target, c provider.Client, policy provider.DeletionPolicy,
) error {
	if policy != provider.DeletionPolicyUnbind {
		return nil // Orphan：云侧一动不动，连一次 List 都不发。
	}
	cl, err := clientOf(c)
	if err != nil {
		return err
	}
	s, err := specOf(t)
	if err != nil {
		return err
	}
	if err := cl.DeleteCnameCert(ctx, s.Bucket, s.DomainName); err != nil {
		// bucket 或 CNAME 已经不在了：解绑的目的已经达到。
		return swallowNotFound(toProviderError(aliyun.ActionPutCname, err, certsv1alpha1.ReasonCleanupFailed))
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)
```

- [ ] **Step 7: 跑测试**

Run: `go test ./pkg/provider/... -v -run 'Provider' && make lint`
Expected: 全部 PASS。`TestProvider_ErrorMapping` 的「限流」用例依赖 `aliyunerr.isThrottling` 对 `Throttling.User` 前缀的识别。

- [ ] **Step 8: Commit**

```bash
git add pkg/provider/
git commit -m "feat(provider): add the OSS custom-domain provider and share the aliyun error mapping with fc3"
```

---

### Task 5: 通用层——证书身份三元组与 target 辅助函数

**Files:**
- Create: `internal/controller/binding_identity.go`、`internal/controller/binding_identity_test.go`
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`（步骤 5 短路与 Apply 的 `ensureHTTPS` 读取）
- Modify: `internal/controller/binding_observe.go`（`noteDrift`）
- Modify: `internal/controller/binding_apply.go`（`freezeApplied`）
- Modify: `internal/controller/binding_deletion.go`（`unbindTarget`）
- Modify: `internal/controller/binding_status.go`（`targetIdentifier` / `targetRegion`）
- Modify: `internal/controller/binding_material.go`（`requiredDomainsOf`）
- Modify: `internal/controller/provider_factory.go`（`targetOf`）
- Modify: `internal/controller/provider_factory_test.go`、`internal/controller/binding_material_test.go`（补 OSS 分支断言）

**Interfaces:**
- Consumes: Task 1 的 API 类型；Task 2 的 `CASCertRef()` / `CurrentCertRef`；Task 4 的 `oss.Provider`（仅经注册表间接使用）。
- Produces: `type certIdentity struct{ want, current, applied string; byRef bool }`；`func identityOf(b *Binding, obs provider.ObservedState, m provider.CertMaterial) certIdentity`；`func capabilitiesOf(b *Binding) provider.Capabilities`；`func ensureHTTPSOf(b *Binding) bool`；`func identityLabel(id certIdentity, v string) string`。`noteDrift` / `freezeApplied` / `unbindTarget` **签名不变**（既有单测直接调它们）。

- [ ] **Step 1: 写失败单测 `binding_identity_test.go`**

```go
package controller

import (
	"testing"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
	// 注册表里要有两个 provider，capabilitiesOf 才有东西可查。provider_factory.go 已经
	// 空导入了 fc3；oss 在 Task 8 之前还没接进生产代码，这里先由测试导入。
	_ "git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"
)

func ossBinding(bucket, domain string) *certsv1alpha1.AliyunCertificateBinding {
	return &certsv1alpha1.AliyunCertificateBinding{
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			Target: certsv1alpha1.BindingTarget{
				Type:            certsv1alpha1.TargetTypeOSSCustomDomain,
				OSSCustomDomain: &certsv1alpha1.OSSCustomDomainTarget{Region: "cn-hangzhou", Bucket: bucket, DomainName: domain},
			},
		},
	}
}

func TestIdentityOf(t *testing.T) {
	id := int64(42)
	m := provider.CertMaterial{Fingerprint: "ffff", CertID: &id, CASRegion: "cn-hangzhou"}

	fc := bindingWithDomain("api.example.com")
	fc.Status.AppliedFingerprint, fc.Status.AppliedCertRef = "aaaa", "should-be-ignored"
	got := identityOf(fc, provider.ObservedState{CurrentFingerprint: "cccc", CurrentCertRef: "ignored"}, m)
	if got.byRef || got.want != "ffff" || got.current != "cccc" || got.applied != "aaaa" {
		t.Errorf("FC3 应按指纹取三元组: %+v", got)
	}

	o := ossBinding("b1", "www.example.com")
	o.Status.AppliedFingerprint, o.Status.AppliedCertRef = "aaaa", "41-cn-hangzhou"
	got = identityOf(o, provider.ObservedState{CurrentFingerprint: "ignored", CurrentCertRef: "40-cn-hangzhou"}, m)
	if !got.byRef || got.want != "42-cn-hangzhou" || got.current != "40-cn-hangzhou" || got.applied != "41-cn-hangzhou" {
		t.Errorf("OSS 应按 certRef 取三元组: %+v", got)
	}

	unknown := &certsv1alpha1.AliyunCertificateBinding{}
	unknown.Spec.Target.Type = "Nope"
	if got := identityOf(unknown, provider.ObservedState{CurrentFingerprint: "cccc"}, m); got.byRef || got.current != "cccc" {
		t.Errorf("没注册的类型退回指纹路径: %+v", got)
	}
}

func TestEnsureHTTPSOf(t *testing.T) {
	fc := bindingWithDomain("api.example.com")
	if ensureHTTPSOf(fc) {
		t.Error("默认 false")
	}
	fc.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol = true
	if !ensureHTTPSOf(fc) {
		t.Error("FC3 读字段")
	}
	if ensureHTTPSOf(ossBinding("b1", "www.example.com")) {
		t.Error("OSS 恒为 false")
	}
	if ensureHTTPSOf(&certsv1alpha1.AliyunCertificateBinding{}) {
		t.Error("内嵌块缺失时不得 panic，返回 false")
	}
}

func TestIdentityLabel(t *testing.T) {
	if got := identityLabel(certIdentity{byRef: true}, "27087165-cn-hangzhou"); got != "27087165-cn-hangzhou" {
		t.Errorf("certRef 不脱敏也不截断: %s", got)
	}
	if got := identityLabel(certIdentity{}, "0123456789abcdef"); got != "01234567" {
		t.Errorf("指纹只留前 8 位: %s", got)
	}
}

func TestTargetHelpers_OSS(t *testing.T) {
	b := ossBinding("b1", "www.example.com")
	tg, err := targetOf(b)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Type != certsv1alpha1.TargetTypeOSSCustomDomain || tg.Region != "cn-hangzhou" || tg.Identifier != "www.example.com" {
		t.Fatalf("Target 不对: %+v", tg)
	}
	if tg.Spec != b.Spec.Target.OSSCustomDomain {
		t.Error("Spec 应指向 CRD 内嵌结构体本身，provider 才能断言出 bucket")
	}
	if got := requiredDomainsOf(b); len(got) != 1 || got[0] != "www.example.com" {
		t.Errorf("OSS 要求覆盖 domainName: %v", got)
	}
	if targetIdentifier(b) != "www.example.com" || targetRegion(b) != "cn-hangzhou" {
		t.Errorf("日志 / label 辅助函数应认得 OSS: %s %s", targetIdentifier(b), targetRegion(b))
	}
	b.Spec.Target.OSSCustomDomain = nil
	if _, err := targetOf(b); err == nil {
		t.Error("缺内嵌块应报错")
	}
	if targetIdentifier(b) != "" || targetRegion(b) != "" {
		t.Error("内嵌块缺失时不得 panic")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controller/ -run 'TestIdentityOf|TestEnsureHTTPSOf|TestIdentityLabel|TestTargetHelpers_OSS'`
Expected: 编译失败（`identityOf` 等未定义）。

- [ ] **Step 3: 写 `binding_identity.go`**

```go
package controller

import (
	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"
)

// certIdentity 抹平「按指纹」与「按 certRef」两种对账方式（spec 2026-09-07 §5.1）。
//
// 内联 PEM 的目标（FC3）三个值都是 SHA-256 指纹；按 certId 引用的目标（OSS）三个值都是
// "<certId>-<casRegion>"。通用层对「目标上是哪张证书」的一切比对——幂等短路、漂移判定、
// 「只解绑自己的」——都只看这三个字段，不再直接读 obs.CurrentFingerprint / CurrentCertRef。
type certIdentity struct {
	want    string // 本轮该在目标上的
	current string // 目标上实际的；"" = 无证书
	applied string // 上次我们写的；"" = 从没写成过
	byRef   bool   // true = 三个值是 certRef，false = 指纹
}

// capabilitiesOf 查注册表取 provider 的能力位；没注册的类型返回零值（等于「内联 PEM、
// 不强制上传」的最保守形状），由步骤 4 的工厂错误路径去报「未知 target.type」。
func capabilitiesOf(b *certsv1alpha1.AliyunCertificateBinding) provider.Capabilities {
	if p, ok := provider.Get(b.Spec.Target.Type); ok {
		return p.Capabilities()
	}
	return provider.Capabilities{}
}

// identityOf 按 provider 的能力位构造三元组。m 可为零值（删除分支不需要 want）。
func identityOf(
	b *certsv1alpha1.AliyunCertificateBinding, obs provider.ObservedState, m provider.CertMaterial,
) certIdentity {
	if capabilitiesOf(b).ReferencesCertByID {
		return certIdentity{want: m.CASCertRef(), current: obs.CurrentCertRef, applied: b.Status.AppliedCertRef, byRef: true}
	}
	return certIdentity{want: m.Fingerprint, current: obs.CurrentFingerprint, applied: b.Status.AppliedFingerprint}
}

// identityLabel 把三元组里的一个值折成可以进日志的形状：指纹只留前 8 位（与 shortFP 一致），
// certRef 本身不敏感也不长，原样输出。
func identityLabel(id certIdentity, v string) string {
	if id.byRef {
		return v
	}
	return shortFP(v)
}

// ensureHTTPSOf 读 ensureHTTPSProtocol。**必须 nil-safe**，且只有 FC3 有这个开关：
// 其它类型恒 false，于是 protocolSatisfied 对它们永远满足，短路判定只剩身份比对。
func ensureHTTPSOf(b *certsv1alpha1.AliyunCertificateBinding) bool {
	if fc := b.Spec.Target.FC3CustomDomain; fc != nil {
		return fc.EnsureHTTPSProtocol
	}
	return false
}
```

- [ ] **Step 4: 改 `reconcileBindingReady` 的短路与 Apply**

`aliyuncertificatebinding_controller.go` 里把

```go
	// 走到这里 targetOf 已经确认过 FC3CustomDomain 非 nil，可以安全解引用。
	ensureHTTPS := b.Spec.Target.FC3CustomDomain.EnsureHTTPSProtocol
	if obs.CurrentFingerprint == m.Fingerprint && protocolSatisfied(obs, ensureHTTPS) {
```

改成

```go
	ensureHTTPS := ensureHTTPSOf(b)
	id := identityOf(b, obs, m)
	if id.current == id.want && protocolSatisfied(obs, ensureHTTPS) {
```

短路分支那段注释里「Observe 刚刚核实了云上装的就是 m.Fingerprint」改为「Observe 刚刚核实了云上装的就是本轮该写的那一张（按指纹或 certRef，见 identityOf）」。其余（`r.freezeApplied(ctx, rd, obs, m, false)`、`r.noteDrift(ctx, rd, obs, m)`、`p.Apply(... PreviousFingerprint: b.Status.AppliedFingerprint)`）不动。

- [ ] **Step 5: 改 `noteDrift`**

`binding_observe.go` 的 `noteDrift` 函数体改为（签名不变）：

```go
	id := identityOf(rd.b, obs, m)
	cur := id.current
	if cur == "" || cur == id.applied || cur == id.want {
		rd.b.Status.DriftedFingerprint, rd.b.Status.DriftedCertRef = "", ""
		return
	}
	// 漂移值写进与身份同种的字段：certRef 不是指纹，不能塞进 driftedFingerprint。
	prev := rd.orig.Status.DriftedFingerprint
	if id.byRef {
		rd.b.Status.DriftedCertRef = cur
		prev = rd.orig.Status.DriftedCertRef
	} else {
		rd.b.Status.DriftedFingerprint = cur
	}
	if prev == cur {
		return
	}
	bindingDriftTotal.WithLabelValues(rd.provider).Inc()
	logf.FromContext(ctx).Info("检测到云侧证书漂移",
		"domain", targetIdentifier(rd.b),
		"observed", identityLabel(id, cur), "expected", identityLabel(id, id.want))
	r.Recorder.Event(rd.b, corev1.EventTypeWarning, certsv1alpha1.ReasonDriftCorrected, driftCorrectedMessage)
```

函数上方注释里「判定要求 obs.CurrentFingerprint 非空」改成「判定要求身份三元组的 current 非空」，其余保留。

- [ ] **Step 6: 改 `freezeApplied`**

`binding_apply.go` 的 `freezeApplied` 里，把

```go
	b.Status.AppliedFingerprint = m.Fingerprint
	// …注释…
	b.Status.DriftedFingerprint = ""
```

改成

```go
	// appliedFingerprint 对所有 provider 都写：证书 controller 的保留护栏 3 只认它。
	// 按 certId 引用的 provider 另写 appliedCertRef，那是它自己对账用的身份。
	b.Status.AppliedFingerprint = m.Fingerprint
	if capabilitiesOf(b).ReferencesCertByID {
		b.Status.AppliedCertRef = m.CASCertRef()
	}
	// 漂移已经被这一轮解决（写完了，或短路核实了云上装的就是这一张），两种痕迹都抹掉，
	// 否则同一张证书日后再次漂移会被 noteDrift 误判成「还是上一轮那次」而不发事件。
	// 短路路径压根不经过 noteDrift，这里是它唯一的清空点。
	b.Status.DriftedFingerprint, b.Status.DriftedCertRef = "", ""
```

- [ ] **Step 7: 改 `unbindTarget`**

`binding_deletion.go` 里把

```go
	if obs.CurrentFingerprint != b.Status.AppliedFingerprint {
		// 目标上不是我们写的那张：可能是别的 Binding 接管了，也可能是人工换过。
		// 动它等于替别人做主。
		logf.FromContext(ctx).Info("目标上的证书不是本 Binding 写入的，跳过解绑",
			"domain", tg.Identifier, "observed", shortFP(obs.CurrentFingerprint))
		return nil
	}
```

改成

```go
	// want 在这里没有意义（删除分支没有材料），只比 current 与 applied。
	id := identityOf(b, obs, provider.CertMaterial{})
	if id.current != id.applied {
		// 目标上不是我们写的那张：可能是别的 Binding 接管了，也可能是人工换过。
		// 动它等于替别人做主。
		logf.FromContext(ctx).Info("目标上的证书不是本 Binding 写入的，跳过解绑",
			"domain", tg.Identifier, "observed", identityLabel(id, id.current))
		return nil
	}
```

（文件顶部 `unbindTarget` 的注释「指纹比对放在通用层」改成「身份比对（指纹或 certRef，见 identityOf）放在通用层」。）

- [ ] **Step 8: target 辅助函数加 OSS 分支**

`provider_factory.go` 的 `targetOf` switch 加：

```go
	case certsv1alpha1.TargetTypeOSSCustomDomain:
		o := b.Spec.Target.OSSCustomDomain
		if o == nil {
			return provider.Target{}, fmt.Errorf("target.type=%s 但缺少 ossCustomDomain", b.Spec.Target.Type)
		}
		return provider.Target{
			Type:       b.Spec.Target.Type,
			Region:     o.Region,
			Identifier: o.DomainName,
			Spec:       o,
		}, nil
```

`binding_material.go` 的 `requiredDomainsOf` 改为：

```go
func requiredDomainsOf(b *certsv1alpha1.AliyunCertificateBinding) []string {
	t := b.Spec.Target
	switch {
	case t.Type == certsv1alpha1.TargetTypeFC3CustomDomain && t.FC3CustomDomain != nil:
		return []string{t.FC3CustomDomain.DomainName}
	case t.Type == certsv1alpha1.TargetTypeOSSCustomDomain && t.OSSCustomDomain != nil:
		return []string{t.OSSCustomDomain.DomainName}
	default:
		return nil
	}
}
```

`binding_status.go` 的 `targetIdentifier` / `targetRegion` 各加一个分支：

```go
func targetIdentifier(b *certsv1alpha1.AliyunCertificateBinding) string {
	switch t := b.Spec.Target; {
	case t.FC3CustomDomain != nil:
		return t.FC3CustomDomain.DomainName
	case t.OSSCustomDomain != nil:
		return t.OSSCustomDomain.DomainName
	default:
		return b.TargetKey()
	}
}

func targetRegion(b *certsv1alpha1.AliyunCertificateBinding) string {
	switch t := b.Spec.Target; {
	case t.FC3CustomDomain != nil:
		return t.FC3CustomDomain.Region
	case t.OSSCustomDomain != nil:
		return t.OSSCustomDomain.Region
	default:
		return ""
	}
}
```

- [ ] **Step 9: 跑全部测试与 lint**

Run: `make build && make test && make lint`
Expected: 全部 PASS，包括 `binding_observe_test.go` 里直接调 `noteDrift` / `freezeApplied` 的既有单测（签名未变、FC3 路径行为未变）。若 `binding_observe_test.go` 有对 `DriftedFingerprint` 清空的断言，它们仍然成立。

- [ ] **Step 10: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): route every target certificate comparison through a fingerprint-or-certRef identity"
```

---

### Task 6: 步骤 3c（CAS 上传门）、材料的 CASRegion、envtest 里的 fake OSS 接线

**Files:**
- Modify: `internal/controller/binding_identity.go`（加 `casUploadGate`）
- Modify: `internal/controller/binding_identity_test.go`（加 `TestCASUploadGate`）
- Modify: `internal/controller/aliyuncertificatebinding_controller.go`（插入步骤 3c）
- Modify: `internal/controller/binding_material.go`（`loadBindingMaterial` 补 `CASRegion`）
- Modify: `internal/controller/binding_material_test.go`（若有对 `loadBindingMaterial` 的 envtest 断言，补 `CASRegion`）
- Create: `internal/controller/suite_oss_test.go`
- Modify: `internal/controller/suite_test.go`（`ProviderFactory` 按 target 类型分派）
- Create: `internal/controller/binding_oss_test.go`（本 Task 只放 3c 的 envtest；Task 7 续写）

**Interfaces:**
- Produces: `func casUploadGate(caps provider.Capabilities, ac *AliyunCertificate, m provider.CertMaterial) (reason, message string)`（reason 为空 = 放行）；envtest helper `currentOSS() *fake.OSS`、`resetOSS()`、`createCertificateWithCAS(ctx, ns, name string, dnsNames ...string)`、`createOSSBinding(ctx, ns, name, certName, bucket, domain string)`、`ossBucketFor(ns string) string`、`certRefOf(ctx, ns, certName string) string`；常量 `casUploadRequiredMessage`、`casCertIDPendingMessage`。

- [ ] **Step 1: `casUploadGate` 的失败单测**

在 `binding_identity_test.go` 追加：

```go
func TestCASUploadGate(t *testing.T) {
	on, off := true, false
	id := int64(7)
	withID := provider.CertMaterial{Fingerprint: "ffff", CertID: &id, CASRegion: "cn-hangzhou"}
	noID := provider.CertMaterial{Fingerprint: "ffff", CASRegion: "cn-hangzhou"}
	acWith := func(u *bool) *certsv1alpha1.AliyunCertificate {
		return &certsv1alpha1.AliyunCertificate{Spec: certsv1alpha1.AliyunCertificateSpec{
			Aliyun: certsv1alpha1.AliyunSpec{Region: "cn-hangzhou", UploadToCAS: u},
		}}
	}
	byRef := provider.Capabilities{ReferencesCertByID: true, RequiresCASUpload: true}

	cases := []struct {
		name   string
		caps   provider.Capabilities
		ac     *certsv1alpha1.AliyunCertificate
		m      provider.CertMaterial
		reason string
	}{
		{"FC3 不要求上传，certId 为 nil 也放行", provider.Capabilities{}, acWith(&off), noID, ""},
		{"OSS + uploadToCAS=false → CASUploadRequired", byRef, acWith(&off), withID, certsv1alpha1.ReasonCASUploadRequired},
		{"OSS + 默认上传 + certId 未就位 → CertificateNotReady", byRef, acWith(nil), noID, certsv1alpha1.ReasonCertificateNotReady},
		{"OSS + 显式上传 + certId 就位 → 放行", byRef, acWith(&on), withID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := casUploadGate(tc.caps, tc.ac, tc.m)
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
			if (reason == "") != (msg == "") {
				t.Errorf("reason 与 message 必须同空同非空: %q / %q", reason, msg)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/controller/ -run TestCASUploadGate`
Expected: 编译失败。

- [ ] **Step 3: 实现 `casUploadGate` 与两条固定文案**

`binding_identity.go` 追加：

```go
// 步骤 3c 的两条固定文案。Message 不含变量（spec §10.2），变量只进日志。
const (
	casUploadRequiredMessage = "目标类型要求证书上传到 CAS，请把 AliyunCertificate 的 spec.aliyun.uploadToCAS 设为 true"
	casCertIDPendingMessage  = "证书尚未取得 CAS certId"
)

// casUploadGate 是 spec 2026-09-07 §5.2 的步骤 3c：按 certId 引用证书的 provider
// （RequiresCASUpload）在两种情形下不能写云。
//
//   - 证书关掉了上传：这是配置错误，Binding 不替证书开上传（D21），报 CASUploadRequired
//     等人改证书 spec。
//   - 这一代还没传完 CAS：m.CertID 只在 status.current 的指纹与 Secret 一致**且**上传成功
//     后才非 nil（loadBindingMaterial），所以这里同时覆盖了「续期后新代次已进 Secret、
//     CAS 还没传完」的窗口。沿用 CertificateNotReady，与 certificateGate 同一节奏重试。
//
// reason 为空表示放行。两条早退都不碰 appliedFingerprint（失败与旁路早退不写 applied）。
func casUploadGate(
	caps provider.Capabilities, ac *certsv1alpha1.AliyunCertificate, m provider.CertMaterial,
) (reason, message string) {
	if !caps.RequiresCASUpload {
		return "", ""
	}
	if !ac.Spec.Aliyun.EffectiveUploadToCAS() {
		return certsv1alpha1.ReasonCASUploadRequired, casUploadRequiredMessage
	}
	if m.CertID == nil {
		return certsv1alpha1.ReasonCertificateNotReady, casCertIDPendingMessage
	}
	return "", ""
}
```

- [ ] **Step 4: 在 `reconcileBindingReady` 插入步骤 3c**

`aliyuncertificatebinding_controller.go`，紧跟 3b（`certificateGate`）那段 `}` 之后、「// 4. 解析凭证」之前：

```go
	// 3c. 按 certId 引用证书的目标必须先有 certId（spec 2026-09-07 §5.2）。只查注册表，
	// 不碰凭证、不发云调用——凭证错误不该盖住一个更早、更便宜就能发现的配置错误。
	if reason, msg := casUploadGate(capabilitiesOf(b), ac, m); reason != "" {
		setBindingCondition(b, certsv1alpha1.ConditionApplied, metav1.ConditionFalse, reason, msg)
		aggregateBindingReady(b)
		requeue := r.DriftCheckInterval
		if reason == certsv1alpha1.ReasonCertificateNotReady {
			requeue = certificateGateRequeue // certId 通常一轮就到；证书 watch 也会唤醒
		}
		return ctrl.Result{RequeueAfter: requeue}, r.patchBinding(ctx, rd)
	}
```

- [ ] **Step 5: `loadBindingMaterial` 补 `CASRegion`**

`binding_material.go` 里构造 `m = provider.CertMaterial{...}` 时加一行 `CASRegion: ac.Spec.Aliyun.EffectiveCASRegion(),`，并在函数注释「status.current 只用来补两样 Secret 里没有的东西」旁补一句「CASRegion 取自 spec（EffectiveCASRegion），与 certId 一起拼成 OSS 用的 certRef」。

- [ ] **Step 6: 跑单测**

Run: `go test ./internal/controller/ -run 'TestCASUploadGate|TestIdentityOf' && make build`
Expected: PASS。

- [ ] **Step 7: envtest 接线 `suite_oss_test.go`**

```go
package controller

import (
	"context"
	"fmt"
	"strconv"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

// 与 fakeFC3 同构，共用 suite_test.go 里的 fakeMu。
var fakeOSS *fake.OSS

// testOSSOwner 是 fake OSS 默认回报的账号。与 FC3 的 testAccountID 取不同值，账号 fencing
// 的用例里两者不会混。
const testOSSOwner = "9876543210"

func currentOSS() *fake.OSS { fakeMu.Lock(); defer fakeMu.Unlock(); return fakeOSS }

func resetOSS() {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fakeOSS = fake.NewOSS()
	fakeOSS.SetOwner(testOSSOwner)
}

// ossBucketFor 给每个 namespace 一个独占的 bucket 名：TargetKey 含 bucket，跨用例撞名会被
// 仲裁判成 Conflict。
func ossBucketFor(ns string) string { return "bucket-" + ns }

// createCertificateWithCAS 建一个**开着** CAS 上传的 AliyunCertificate。
//
// 与 createCertificate 刻意相反：OSS 按 certId 引用证书，没有 certId 就到不了 Apply。
// 代价是 suite_fc3_test.go 里说的那颗地雷——这张证书会在后续每个用例 resetCAS() 之后
// 发现「云上没有我」而重传。所以本 helper 用 DeferCleanup 在用例结束时把证书删掉
// （finalizer 走 fake CAS，瞬间完成），常驻证书集合里不留开着上传的对象。
// 调用方必须先删 Binding 再让这个 cleanup 跑：Binding 的 finalizer 会等证书。
func createCertificateWithCAS(ctx context.Context, ns, name string, dnsNames ...string) {
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
	DeferCleanup(func() {
		got := &certsv1alpha1.AliyunCertificate{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, got)
		eventually(func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name},
				&certsv1alpha1.AliyunCertificate{}) != nil
		})
	})
}

// createOSSBinding 建一个指向 OSS CNAME 的 Binding，并登记 DeferCleanup 先于证书删除它。
func createOSSBinding(ctx context.Context, ns, name, certName, bucket, domain string) {
	b := &certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateBindingSpec{
			CertificateRef: certsv1alpha1.LocalObjectReference{Name: certName},
			Target: certsv1alpha1.BindingTarget{
				Type: certsv1alpha1.TargetTypeOSSCustomDomain,
				OSSCustomDomain: &certsv1alpha1.OSSCustomDomainTarget{
					Region: "cn-hangzhou", Bucket: bucket, DomainName: domain,
				},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, b)).To(Succeed())
	// DeferCleanup 是 LIFO：本 helper 在 createCertificateWithCAS 之后调用，所以它登记的
	// 清理先跑——Binding 先于证书消失，证书的 finalizer 不会被 Binding 卡住。
	DeferCleanup(func() {
		got := &certsv1alpha1.AliyunCertificateBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
			return
		}
		_ = k8sClient.Delete(ctx, got)
		eventually(func() bool { return bindingGone(ctx, ns, name) })
	})
}

// ossDomainFor 与 FC3 用例同一命名纪律：<binding>.<ns>.example.com。
func ossDomainFor(ns, binding string) string { return fmt.Sprintf("%s.%s.example.com", binding, ns) }

// certRefOf 读证书 status.current.certId 拼出 OSS 侧应观测到的 certRef。
// 拼法必须与 provider.CertMaterial.CASCertRef() 一致（"<certId>-<casRegion>"）。
func certRefOf(ctx context.Context, ns, certName string) string {
	ac := getAC(ctx, ns, certName)
	if ac.Status.Current == nil || ac.Status.Current.CertID == nil {
		return ""
	}
	return strconv.FormatInt(*ac.Status.Current.CertID, 10) + "-" + ac.Spec.Aliyun.EffectiveCASRegion()
}
```

（`getAC`、`bindingGone`、`eventually` 已在同包测试里定义。）

`suite_test.go` 的 `BeforeSuite`：在 `resetFC3()` 旁加 `resetOSS()`，并把 `ProviderFactory` 闭包改为按类型分派：

```go
		ProviderFactory: func(_ context.Context, b *certsv1alpha1.AliyunCertificateBinding,
			_ *certsv1alpha1.AliyunCertificate) (provider.Provider, provider.Client, error) {
			if err := currentFC3FactoryErr(); err != nil {
				return nil, nil, err
			}
			if b.Spec.Target.Type == certsv1alpha1.TargetTypeOSSCustomDomain {
				return &oss.Provider{}, currentOSS(), nil
			}
			return &fc3.Provider{}, currentFC3(), nil
		},
```

（import 加 `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"`。）

- [ ] **Step 8: 3c 的 envtest `binding_oss_test.go`**

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

var _ = Describe("绑定 controller：OSS 目标", func() {
	ctx := context.Background()

	BeforeEach(func() {
		resetCAS()
		resetFC3()
		resetOSS()
	})

	It("证书 uploadToCAS=false 时 Applied=False/CASUploadRequired，且不碰 OSS", func() {
		ns := newNamespace(ctx)
		domain, bucket := ossDomainFor(ns, "o1"), ossBucketFor(ns)
		ca := testutil.NewCA(GinkgoT())
		certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain})
		createCertificate(ctx, ns, "c1", domain) // 这个 helper 关着上传
		simulateIssuance(ctx, ns, "c1", 1, certPEM, keyPEM)
		createOSSBinding(ctx, ns, "o1", "c1", bucket, domain)

		eventually(func() bool {
			c := bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionApplied)
			return c.Status == metav1.ConditionFalse && c.Reason == certsv1alpha1.ReasonCASUploadRequired
		})
		Expect(bindingCond(ctx, ns, "o1", certsv1alpha1.ConditionReady).Reason).To(Equal(certsv1alpha1.ReasonCASUploadRequired))
		Expect(currentOSS().ListCallsFor(bucket, domain)).To(BeZero())
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(BeZero())
		b := getBinding(ctx, ns, "o1")
		Expect(b.Status.AppliedFingerprint).To(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(BeEmpty())
	})
})
```

- [ ] **Step 9: 跑全套**

Run: `make test && make lint`
Expected: PASS。FC3 全部既有用例不变。

- [ ] **Step 10: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): gate ID-referencing targets on a CAS certId and wire the fake OSS into envtest"
```

---

### Task 7: OSS 路径的 envtest——首绑、短路、漂移、Unbind

**Files:**
- Modify: `internal/controller/binding_oss_test.go`（在 Task 6 的 `Describe` 里追加用例）

**Interfaces:**
- Consumes: Task 6 的 `createCertificateWithCAS` / `createOSSBinding` / `certRefOf` / `currentOSS` / `ossBucketFor` / `ossDomainFor`；既有 `simulateIssuance`、`bindingCond`、`getBinding`、`bindingEventCount`、`switchToUnbind`、`bindingGone`、`eventually`。
- Produces: helper `issueAndBindOSS(ctx, ns, certName, bindingName string) (bucket, domain string)`、`pokeBinding(ctx, ns, name string)`。

- [ ] **Step 1: 追加 helper 与四条用例**

在 `binding_oss_test.go` 文件末尾（`Describe` 之外）加：

```go
// issueAndBindOSS 建开着 CAS 上传的证书、fake OSS 上的空 CNAME 与 OSS Binding，等到 Applied=True。
func issueAndBindOSS(ctx context.Context, ns, certName, bindingName string) (bucket, domain string) {
	bucket, domain = ossBucketFor(ns), ossDomainFor(ns, bindingName)
	ca := testutil.NewCA(GinkgoT())
	certPEM, keyPEM := testutil.IssueLeaf(GinkgoT(), ca, domain)
	currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain})
	createCertificateWithCAS(ctx, ns, certName, domain)
	simulateIssuance(ctx, ns, certName, 1, certPEM, keyPEM)
	createOSSBinding(ctx, ns, bindingName, certName, bucket, domain)
	eventually(func() bool {
		return bindingCond(ctx, ns, bindingName, certsv1alpha1.ConditionApplied).Status == metav1.ConditionTrue
	})
	return bucket, domain
}

// pokeBinding 用 annotation 推一轮 reconcile（bindingMeaningfulChange 认 annotation 变化）。
func pokeBinding(ctx context.Context, ns, name string) {
	b := getBinding(ctx, ns, name)
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations["poke"] = fmt.Sprintf("%d", len(b.Annotations["poke"])+1)
	ExpectWithOffset(1, k8sClient.Update(ctx, b)).To(Succeed())
}
```

（import 加 `"fmt"`。）

在 `Describe` 里、Task 6 那条用例之后加：

```go
	It("首绑：按 certRef 写入 OSS，status 同时记 appliedFingerprint 与 appliedCertRef", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c2", "o2")

		want := certRefOf(ctx, ns, "c2")
		Expect(want).NotTo(BeEmpty(), "fake CAS 应已给证书分配 certId")
		rec, ok := currentOSS().Cname(bucket, domain)
		Expect(ok).To(BeTrue())
		Expect(rec.CertRef).To(Equal(want))
		Expect(rec.CertType).To(Equal("CAS"))

		b := getBinding(ctx, ns, "o2")
		Expect(b.Status.AppliedCertRef).To(Equal(want))
		Expect(b.Status.AppliedFingerprint).To(Equal(getAC(ctx, ns, "c2").Status.Current.Fingerprint))
		Expect(b.Status.BoundAccountID).To(Equal(testOSSOwner))
		Expect(bindingCond(ctx, ns, "o2", certsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(1))
	})

	It("certRef 一致时短路：只 List 不 Put", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c3", "o3")
		puts, lists := currentOSS().PutCallsFor(bucket, domain), currentOSS().ListCallsFor(bucket, domain)

		pokeBinding(ctx, ns, "o3")
		eventually(func() bool { return currentOSS().ListCallsFor(bucket, domain) > lists })
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts))
		Expect(getBinding(ctx, ns, "o3").Status.AppliedCertRef).To(Equal(certRefOf(ctx, ns, "c3")))
	})

	It("漂移：OSS 上是别的 certRef 时纠正一次并只发一条 DriftCorrected", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c4", "o4")
		want := certRefOf(ctx, ns, "c4")
		puts := currentOSS().PutCallsFor(bucket, domain)

		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain, CertRef: "999999-cn-hangzhou", CertType: "CAS"})
		pokeBinding(ctx, ns, "o4")

		eventually(func() bool {
			rec, _ := currentOSS().Cname(bucket, domain)
			return rec.CertRef == want && currentOSS().PutCallsFor(bucket, domain) == puts+1
		})
		eventually(func() bool { return bindingEventCount(ctx, ns, "o4", certsv1alpha1.ReasonDriftCorrected) == 1 })
		b := getBinding(ctx, ns, "o4")
		Expect(b.Status.DriftedCertRef).To(BeEmpty(), "纠正成功后痕迹必须抹掉")
		Expect(b.Status.DriftedFingerprint).To(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(Equal(want))

		// 再推一轮：没有新漂移就不许再发事件。
		pokeBinding(ctx, ns, "o4")
		Consistently(func() int { return bindingEventCount(ctx, ns, "o4", certsv1alpha1.ReasonDriftCorrected) },
			"1s", "200ms").Should(Equal(1))
	})

	It("Unbind：certRef 是自己的才摘，CNAME 保留", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c5", "o5")
		switchToUnbind(ctx, ns, "o5")
		puts := currentOSS().PutCallsFor(bucket, domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "o5"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "o5") })

		rec, ok := currentOSS().Cname(bucket, domain)
		Expect(ok).To(BeTrue(), "Unbind 只摘证书，CNAME 记录必须保留")
		Expect(rec.CertRef).To(BeEmpty())
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts + 1))
	})

	It("Unbind：OSS 上是别人的 certRef 时一动不动", func() {
		ns := newNamespace(ctx)
		bucket, domain := issueAndBindOSS(ctx, ns, "c6", "o6")
		switchToUnbind(ctx, ns, "o6")
		currentOSS().AddCname(bucket, fake.CnameRecord{Domain: domain, CertRef: "888888-cn-hangzhou", CertType: "CAS"})
		puts, lists := currentOSS().PutCallsFor(bucket, domain), currentOSS().ListCallsFor(bucket, domain)

		Expect(k8sClient.Delete(ctx, getBinding(ctx, ns, "o6"))).To(Succeed())
		eventually(func() bool { return bindingGone(ctx, ns, "o6") })

		rec, _ := currentOSS().Cname(bucket, domain)
		Expect(rec.CertRef).To(Equal("888888-cn-hangzhou"))
		Expect(currentOSS().PutCallsFor(bucket, domain)).To(Equal(puts), "解绑别人的证书是越权")
		Expect(currentOSS().ListCallsFor(bucket, domain)).To(Equal(lists+1), "删除只该 Observe 一次")
	})

	It("FC3 Binding 的 appliedCertRef 恒为空（回归门）", func() {
		ns := newNamespace(ctx)
		domain := fmt.Sprintf("b7.%s.example.com", ns)
		issueAndBind(ctx, ns, "c7", "b7", domain, "HTTP")
		b := getBinding(ctx, ns, "b7")
		Expect(b.Status.AppliedFingerprint).NotTo(BeEmpty())
		Expect(b.Status.AppliedCertRef).To(BeEmpty())
		Expect(b.Status.DriftedCertRef).To(BeEmpty())
	})
```

- [ ] **Step 2: 跑 envtest**

Run: `make test`
Expected: PASS。若「漂移」用例里 `puts+1` 偶发变成 `puts+2`，说明 pokeBinding 与证书 watch 各触发了一轮且两轮都观测到漂移——那是 fake 写入后仍被判漂移的 bug，不是测试问题，回去看 `noteDrift` / `freezeApplied` 是否把 `AppliedCertRef` 写对了。

- [ ] **Step 3: lint 与 commit**

Run: `make lint`

```bash
git add internal/controller/binding_oss_test.go
git commit -m "test(controller): cover the OSS binding path end to end in envtest"
```

---

### Task 8: provider 工厂按 target 类型分派、`ClientKey.Type`、`main.go`

**Files:**
- Modify: `pkg/aliyun/cache.go`、`pkg/aliyun/cache_test.go`
- Modify: `internal/controller/provider_factory.go`、`internal/controller/provider_factory_test.go`
- Modify: `internal/controller/metrics.go`（`serviceOSS`）
- Modify: `cmd/main.go`

**Interfaces:**
- Consumes: Task 3 的 `aliyun.NewOSSClient`；Task 4 的 `oss` 包（空导入注册）。
- Produces: `aliyun.ClientKey.Type string`；`NewProviderFactory(reader client.Reader, cache *aliyun.ClientCache[provider.Client], limiters *aliyun.Limiters, timeout time.Duration) ProviderFactory`；`func buildProviderClient(typeName, region string, cred credential.Credential, limiterKey string, limiters *aliyun.Limiters, timeout time.Duration) (provider.Client, error)`；`serviceOSS = "oss"`。

- [ ] **Step 1: cache 的失败单测**

`pkg/aliyun/cache_test.go` 的 `TestClientCache_DistinguishesNonCredentialComponents` 的 `variants` 里加一行：

```go
		"Type":            func(k *aliyun.ClientKey) { k.Type = "OSSCustomDomain" },
```

Run: `go test ./pkg/aliyun/ -run TestClientCache` → 编译失败（无 `Type` 字段）。

- [ ] **Step 2: `ClientKey` 加 `Type`**

`pkg/aliyun/cache.go`：结构体加字段并进 identity：

```go
type ClientKey struct {
	Namespace       string
	Name            string
	ResourceVersion string
	Region          string
	Endpoint        string
	ResourceGroupID string
	// Type 是 provider client 的种类（target.type）。同一份凭证、同一个 region 下 FC3 与 OSS
	// 的 client 是两个不同的对象，缺了它两者会串用。CAS 的缓存留空。
	Type string
}

func (k ClientKey) identity() string {
	return k.Namespace + "/" + k.Name + "/" + k.Region + "/" + k.Endpoint + "/" + k.ResourceGroupID + "/" + k.Type
}
```

`ClientCache` 的注释「每个 (namespace, name, region, endpoint, resourceGroupId)」补上 `type`。

Run: `go test ./pkg/aliyun/ -run TestClientCache` → PASS。

- [ ] **Step 3: 工厂测试先改签名并加 OSS 用例**

`internal/controller/provider_factory_test.go`：`factoryFixture.cache` 类型改为 `*aliyun.ClientCache[provider.Client]`，`newFactoryFixture` 里 `aliyun.NewClientCache[aliyun.FC3Client]()` 改为 `aliyun.NewClientCache[provider.Client]()`。追加：

```go
func TestNewProviderFactory_OSSTargetBuildsOSSClient(t *testing.T) {
	fx := newFactoryFixture(t, akSecret("cas-cred"))
	fx.b = ossBinding("b1", "www.example.com")
	fx.b.Namespace, fx.b.Name = "ns1", "b1"

	p, cl, err := fx.call(t)
	if err != nil {
		t.Fatalf("OSS 目标应能构造 client: %v", err)
	}
	if p.Name() != certsv1alpha1.TargetTypeOSSCustomDomain {
		t.Errorf("应取到 OSS provider: %s", p.Name())
	}
	if _, ok := cl.(aliyun.OSSClient); !ok {
		t.Fatalf("OSS 目标应拿到 aliyun.OSSClient，得到 %T", cl)
	}
	if fx.cache.Len() != 1 {
		t.Errorf("应缓存一个 client: %d", fx.cache.Len())
	}

	// 同一份凭证、同一 region 下再要一个 FC3 client：必须是另一个对象、另一个缓存槽。
	fx.b = bindingWithDomain("api.example.com")
	fx.b.Namespace, fx.b.Name = "ns1", "b1"
	_, fcl, err := fx.call(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fcl.(aliyun.FC3Client); !ok {
		t.Fatalf("FC3 目标应拿到 aliyun.FC3Client，得到 %T", fcl)
	}
	if fx.cache.Len() != 2 {
		t.Errorf("FC3 与 OSS 的 client 不许串用一个缓存槽: %d", fx.cache.Len())
	}
}
```

Run: `go test ./internal/controller/ -run TestNewProviderFactory` → 编译失败（签名不符）。

- [ ] **Step 4: 改工厂**

`internal/controller/provider_factory.go`：

空导入处加 `_ "git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/oss"`，注释改为「让 fc3 / oss 的 init() 把自己注册进 registry」。import 加 `credential "github.com/aliyun/credentials-go/credentials"`。

`NewProviderFactory` 签名改为 `cache *aliyun.ClientCache[provider.Client]`；key 与 build 改为：

```go
		// key 必须囊括 build 闭包里读到的每一个会改变 client 行为的字段：Region 与 Type。
		// Endpoint 恒为空：AliyunCertificate 的 endpointOverride 是给 CAS 用的，FC3 / OSS 的
		// endpoint 一律由各自 SDK 按 region 选出。
		key := aliyun.ClientKey{
			Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion,
			Region: tg.Region, Type: tg.Type,
		}
		cl, berr := cache.GetOrBuild(key, func() (provider.Client, error) {
			cred, err := creds.Build()
			if err != nil {
				return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
			}
			// 一律读 key 而不是 tg：缓存命中与否只由 key 决定。
			return buildProviderClient(key.Type, key.Region, cred, creds.LimiterKey(), limiters, timeout)
		})
```

文件末尾加：

```go
// buildProviderClient 按 target 类型构造对应的云 client。
//
// 分派只在这一处：provider 注册表是异构的（spec §7），client 的构造却要各自的 SDK 配置，
// 所以由通用层按类型 switch，而不是让 Provider 接口多一个 NewClient 方法把 SDK 依赖
// 带进 pkg/provider。
func buildProviderClient(
	typeName, region string, cred credential.Credential, limiterKey string,
	limiters *aliyun.Limiters, timeout time.Duration,
) (provider.Client, error) {
	switch typeName {
	case certsv1alpha1.TargetTypeFC3CustomDomain:
		return aliyun.NewFC3Client(cred, aliyun.FC3ClientConfig{
			Region: region, Timeout: timeout, Limiters: limiters, LimiterKey: limiterKey,
			OnCall: aliyunAPICallRecorder(serviceFC3),
		})
	case certsv1alpha1.TargetTypeOSSCustomDomain:
		return aliyun.NewOSSClient(cred, aliyun.OSSClientConfig{
			Region: region, Timeout: timeout, Limiters: limiters, LimiterKey: limiterKey,
			OnCall: aliyunAPICallRecorder(serviceOSS),
		})
	default:
		return nil, fmt.Errorf("没有 target.type %q 的 client 构造器", typeName)
	}
}
```

`internal/controller/metrics.go` 的 service 常量块加 `serviceOSS = "oss"`。

- [ ] **Step 5: `cmd/main.go`**

把

```go
	fc3Cache := aliyun.NewClientCache[aliyun.FC3Client]()
```

改为

```go
	// FC3 与 OSS 的 client 共用一个按 (凭证, region, target.type) 分槽的缓存。
	providerCache := aliyun.NewClientCache[provider.Client]()
```

`ProviderFactory: controller.NewProviderFactory(mgr.GetClient(), providerCache, limiters, opts.CloudCallTimeout)`；import 加 `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider"`。

- [ ] **Step 6: 全量验证**

Run: `make build && make test && make lint`
Expected: PASS。`TestNewProviderFactory_*` 既有用例（凭证继承、轮换重建、零泄漏）不变。

- [ ] **Step 7: Commit**

```bash
git add pkg/aliyun/cache.go pkg/aliyun/cache_test.go internal/controller/ cmd/main.go
git commit -m "feat(controller): dispatch provider clients by target type behind one typed cache"
```

---

### Task 9: RAM 策略、README、主 spec、样例

**Files:**
- Create: `docs/ram/binding-oss-policy.json`
- Modify: `docs/ram/full-policy.json`、`hack/verify-ram-policy.py`
- Modify: `README.md`（「概述」不动；「CRD 参考 › AliyunCertificateBinding」、「RAM 权限」整节、「集成测试 runbook」）
- Modify: `docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md`（§2 新增 2.6、§4.2、§6.2、新增 §6.6、§7、§8.3、§12.3、§16）
- Create: `config/samples/certs_v1alpha1_aliyuncertificatebinding_oss.yaml`；Modify: `config/samples/kustomization.yaml`

**Interfaces:**
- Consumes: Task 1–8 已落地的字段名与 reason（`ossCustomDomain.{region,bucket,domainName}`、`appliedCertRef`、`driftedCertRef`、`CASUploadRequired`）。
- Produces: `make verify-ram-policy` 通过（README、spec §8.3、三份 + 一份 JSON 六处一致）。

- [ ] **Step 1: 先让门禁红**

创建 `docs/ram/binding-oss-policy.json`：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "oss:ListCname",
        "oss:PutCname"
      ],
      "Resource": [
        "acs:oss:*:<accountId>:<bucket>"
      ]
    }
  ]
}
```

`docs/ram/full-policy.json` 的 `Statement` 数组末尾追加同一条 Statement（逐字相同，含两空格缩进）。

Run: `make verify-ram-policy`
Expected: FAIL——README 与 spec §8.3 的内联策略与 `full-policy.json` 不一致，且脚本仍只合并两份。

- [ ] **Step 2: 改 `hack/verify-ram-policy.py`**

- 文档字符串：「五处副本」→「六处副本」；第 3 条改为「certificate-cas-policy.json、binding-fc3-policy.json、binding-oss-policy.json 的 Statement 按顺序拼起来 == full-policy.json 的 Statement 列表」。
- 常量区加 `OSS = ROOT / "docs" / "ram" / "binding-oss-policy.json"`。
- `main()` 里 `merged = load(CAS)["Statement"] + load(FC3)["Statement"]` 改为 `merged = load(CAS)["Statement"] + load(FC3)["Statement"] + load(OSS)["Statement"]`；两条失败文案里的「certificate-cas-policy.json + binding-fc3-policy.json」改为「certificate-cas-policy.json + binding-fc3-policy.json + binding-oss-policy.json」，diff 标签「cas + fc3 合并」改为「cas + fc3 + oss 合并」；成功文案改为「README、spec §8.3 与 docs/ram/ 下四份策略一致」。

- [ ] **Step 3: README「RAM 权限」一节**

按下面逐处改（用 Edit 精确替换，别重排其它段落）：

1. 开头一句「operator 对阿里云只发 **5 个 OpenAPI 动作**」→「**7 个 OpenAPI 动作**」；「`fc` 逐域名 ARN，`yundun-cert` 只能 `*`」→「`fc` 逐域名 ARN，`oss` 逐 bucket ARN，`yundun-cert` 只能 `*`」。
2. 完整策略 JSON 块：在 `fc` 那条 Statement 之后追加 OSS 那条（与 `full-policy.json` 逐字一致，门禁会比）。
3. 紧随其后的占位符说明句「`<fc3Region>` / `<accountId>` / `<domainName>` 是占位符」→「`<fc3Region>` / `<accountId>` / `<domainName>` / `<bucket>` 是占位符」。
4. 「每个 Action 用在哪、缺了会怎样」表末尾加两行：

```markdown
| `oss:ListCname` | OSS 绑定的每一轮 Observe（漂移检测）、`deletionPolicy: Unbind` 解绑前的读取 | `Ready=False`，reason `CredentialsInvalid`；**`Applied` 一个字节都不动**，与 `fc:GetCustomDomain` 缺失同一条规矩。固定 5 分钟 requeue |
| `oss:PutCname` | OSS 绑定的 Apply（按 CAS certId 换绑：首次绑定、证书换代、漂移纠正）与 `Unbind` 时摘证书 | `Applied=False` reason `CredentialsInvalid`，`Ready` 跟着 `False`，事件 `ApplyFailed`；固定 5 分钟 requeue。解绑路径走 `CleanupFailed` → `Abandon` / `Block` |
```

表下那段「`fc` 的两条动作只在 ARN 命中的域名上生效」之后加一段：

```markdown
`oss` 的两条动作以 bucket 为资源粒度（`acs:oss:*:<accountId>:<bucket>`），这是 OSS 允许的最细粒度：持有它就能改这个 bucket 上**任意** CNAME 的证书。OSS 绑定按 CAS certId 引用证书，所以 **OSS Binding 必须同时保留 `yundun-cert` 那条**——证书的 `spec.aliyun.uploadToCAS` 关着时 Binding 会停在 `Applied=False` / `CASUploadRequired`。
```

5. 「按你的部署裁剪」列表加两条：

```markdown
- **没有 OSS Binding**：删掉 `oss` 那条 Statement。
- **有 OSS Binding**：`oss` 那条按 bucket 逐个列 ARN；`yundun-cert` 那条不能删（见上）。
```

并把第一条「`spec.aliyun.uploadToCAS: false`：只留 `fc` 那条 Statement」补一句「（这种证书不能被 OSS Binding 引用）」。

6. 「把占位符填成真实值」列表加一条：

```markdown
- **`<bucket>`** 取 Binding 的 `spec.target.ossCustomDomain.bucket`。OSS 的 ARN 里 region 位写 `*`（bucket 名全局唯一，OSS 的资源级授权不按 region 收窄）。
```

7. 「文件版」：表格改为四行（加 `| docs/ram/binding-oss-policy.json | 只有 oss 那条，按 bucket ARN 授权 |`），第一行「两条 Statement」→「三条 Statement」；`jq` 命令加上第四个文件；「这五处副本（README 里那份、spec §8.3 里那份，加三个文件）」→「这六处副本（README 里那份、spec §8.3 里那份，加四个文件）」。
8. 「集成测试的额外权限」首段「配了 `FC3_TEST_DOMAIN` 时还会真的改写那个域名的 `certConfig`」后补「，配了 `OSS_TEST_BUCKET` / `OSS_TEST_DOMAIN` 时还会真的换绑那个 CNAME 上的证书」；末尾加一句「OSS 探针同样只用上面这份策略里的 `oss:ListCname` / `oss:PutCname`」。

- [ ] **Step 4: README「CRD 参考 › AliyunCertificateBinding」**

字段表：`target.type` 一行「目前只有 `FC3CustomDomain`」→「`FC3CustomDomain` 或 `OSSCustomDomain`」。在 `target.fc3CustomDomain.ensureHTTPSProtocol` 之后加三行：

```markdown
| `target.ossCustomDomain.region` | string | **是** | — | `type=OSSCustomDomain` 时必须且只能设置这个块；bucket 所在 region，与 CAS 区域无关 |
| `target.ossCustomDomain.bucket` | string | **是** | — | bucket 名（`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`） |
| `target.ossCustomDomain.domainName` | string | **是** | — | 该 bucket 上已绑定、已通过所有权验证的自定义域名。OSS 按 **CAS certId** 引用证书，所以证书的 `spec.aliyun.uploadToCAS` 必须为 `true`（默认值），否则 Binding 停在 `Applied=False` / `CASUploadRequired` |
```

`deletionPolicy` 一行末尾补「；OSS 上是摘掉 CNAME 的证书、CNAME 记录保留」。

status 表在 `driftedFingerprint` 之后加两行：

```markdown
| `appliedCertRef` | OSS 目标上实际引用的 CAS certId 字符串（形如 `27087165-cn-hangzhou`）；FC3 恒为空。`appliedFingerprint` 对两种目标都写 |
| `driftedCertRef` | 与 `driftedFingerprint` 同义，只是内容是 certRef；OSS 目标用它做漂移事件的跃迁基准 |
```

示例之后再加一个 OSS 示例：

```markdown
OSS 示例（`config/samples/certs_v1alpha1_aliyuncertificatebinding_oss.yaml`）：

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificateBinding
metadata:
  name: www-oss
spec:
  certificateRef:
    name: www-bestheme
  target:
    type: OSSCustomDomain
    ossCustomDomain:
      region: cn-hangzhou
      bucket: applanding-102181
      domainName: www.bestheme.ac.cn
```
```

「上手」一段「`config/samples/` 里的三个样本」→「四个样本」，并补一句「OSS 那个样本引用的证书名与另一个样本不同，按需改成你自己的」。

- [ ] **Step 5: README「集成测试 runbook」**

第 1 段「配了 `FC3_TEST_DOMAIN` 时还会真的改写那个域名的 `certConfig`」后补「；配了 `OSS_TEST_BUCKET` / `OSS_TEST_DOMAIN` 时会真的换绑那个 CNAME 上的证书（探针结束时会还原原来的 certId，还不回去时会在 `RESULTS.md` 里写明）」。

- [ ] **Step 6: 样例**

创建 `config/samples/certs_v1alpha1_aliyuncertificatebinding_oss.yaml`（内容同 Step 4 的 OSS 示例），`kustomization.yaml` 的 `resources` 加一行 `- certs_v1alpha1_aliyuncertificatebinding_oss.yaml`。

- [ ] **Step 7: 主 spec**

`docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md`：

1. §2 在「### 2.5 未核实项（必测）」之前插入：

```markdown
### 2.6 阿里云 OSS（自定义域名证书）

已核实事实与来源见 `docs/superpowers/specs/2026-09-07-oss-provider-design.md` §1（O1–O10）。要点：`PutCname` 的 `CertificateConfiguration` 支持按 CAS `CertId`（形如 `493****-cn-hangzhou`）引用，不带 `PreviousCertId` 时必须 `Force=true`，`DeleteCertificate=true` 只摘证书不删 CNAME；`ListCname` 返回 `Owner` 与每条 CNAME 的 `Certificate.CertId`；RAM action `oss:ListCname` / `oss:PutCname`，资源 `acs:oss:*:<accountId>:<bucket>`。
```

2. §4.2 的 YAML 示例里 `target:` 块之后加注释行 `# 或 type: OSSCustomDomain + ossCustomDomain: {region, bucket, domainName}（2026-09-07 起）`；status 里 `driftedFingerprint` 之后加 `appliedCertRef: "27087165-cn-hangzhou"   # OSS 目标：实际引用的 CAS certId；FC3 为空` 与 `driftedCertRef: ""`；「CRD 校验」列表的 union 一条改为「union 一致性：每个内嵌块各一条 `self.type == '<T>' ? has(self.<block>) : !has(self.<block>)`」；`target.type` 注释「discriminated union 判别字段」后补「（`FC3CustomDomain` | `OSSCustomDomain`）」。
3. §6.2 步骤列表在 `3.` 与 `4.` 之间插入：

```
 3c. **CAS 上传门**（按 certId 引用证书的 provider，见 2026-09-07 spec §5.2）：
    - 证书 uploadToCAS=false → Applied=False/CASUploadRequired，不写云，return
    - status.current.certId 尚未就位 → Applied=False/CertificateNotReady，30s requeue
```

步骤 5 里「observed.CurrentFingerprint == 证书 current 指纹」等三处比对前加一句「（比对走身份三元组：内联 PEM 的目标比指纹，按 certId 引用的目标比 `<certId>-<casRegion>`）」。步骤 7 「appliedFingerprint = current」后补「；按 certId 引用的目标另写 appliedCertRef」。

4. §6.5 之前插入新小节：

```markdown
### 6.6 OSS provider 的 Apply

`PutCname{CertId: "<certId>-<casRegion>", Force: true}`，不做 read-modify-write（`CertificateConfiguration` 只碰证书配置）。`Unbind` 走 `PutCname{DeleteCertificate: true}`，CNAME 记录保留。私钥不经 OSS 链路。完整设计见 `docs/superpowers/specs/2026-09-07-oss-provider-design.md` §6–§7。
```

5. §7 代码块：`CertMaterial` 加 `CASRegion string // 证书上传所在的 CAS 区域；CASCertRef() 用`，块后加一行 `func (m CertMaterial) CASCertRef() string // "<certId>-<casRegion>"，CertID 为 nil 时 ""`；`ObservedState` 加 `CurrentCertRef string // 按 ID 引用的目标：实际引用的 certRef；"" = 无证书`。「**`uploadToCAS` 与 provider 的关系**」一段改为：「`RequiresCASUpload=true` 的 provider 遇到 `uploadToCAS: false` 的证书时，Binding 报 `Applied=False/CASUploadRequired` 等用户改证书 spec，**不替证书开上传**（D21，2026-09-07；原文「强制上传」作废：Binding 不拥有证书 CR，反向依赖会把两个 controller 缠在一起）。」
6. §8.3 的完整策略 JSON 追加 OSS 那条 Statement（与 `full-policy.json` 逐字一致）；「两份策略，对应 `uploadToCAS` 两态」→「三段 Statement：CAS、FC3、OSS；按部署裁剪见 README」；要点列表加「`oss` 支持 bucket 级 ARN（`acs:oss:*:<accountId>:<bucket>`），region 位写 `*`；OSS Binding 依赖 CAS certId，不能与 `uploadToCAS: false` 搭配」。
7. §12.3 表末追加六行（编号即 RESULTS.md 的编号）：

```markdown
| 15 | OSS `PutCname` 带 `CertId` + `Force=true` 的首绑与换绑是否都成功（T-OSS1） | 换绑被拒 ⇒ 续期永远 ApplyFailed |
| 16 | OSS `CertId` 区域后缀取 CAS 区域还是 bucket 区域（T-OSS2；两者同为 cn-hangzhou 时只能证明「同区域可行」） | 跨区组合下 Apply 被拒 |
| 17 | `ListCname` 回报的 `CertId` 是否与写入字符串逐字相同（T-OSS3） | 短路永不命中 ⇒ 每小时一次无谓 PutCname 并误报漂移 |
| 18 | `DeleteCertificate=true` 后 CNAME 记录是否保留（T-OSS4） | Unbind 打断线上访问 |
| 19 | CAS 删除被 OSS 引用的证书是否被拒、错误码（T-OSS5） | 回收路径持续 ReclaimFailed |
| 20 | 缺 `oss:PutCname` 权限时的错误码与 HTTP 状态（T-OSS6；只能由其它 OSS 探针偶遇 Auth 类错误时顺带记录） | 分类落入 Permanent 而非 Auth，退避节奏错 |
```

表下加一段：「#15–#20 的探针在 `test/integration/oss_test.go`，以 `OSS_TEST_BUCKET` / `OSS_TEST_DOMAIN` 门控；所有者决定（2026-09-07 D24）不另建牺牲 bucket，首次实测由 `www.bestheme.ac.cn` 的受控首绑完成。」

8. §16 表末追加 D21–D24（内容照抄 2026-09-07 spec §16 的四行）。

- [ ] **Step 8: 门禁与全量验证**

Run: `make verify-ram-policy && make test && make lint`
Expected: `verify-ram-policy: OK（README、spec §8.3 与 docs/ram/ 下四份策略一致）`；测试与 lint 通过（本 Task 没改 Go 代码，`make test` 主要验证 `config/samples` 仍能被 kustomize 处理——若仓库有 `make build-installer`，也跑一次）。

- [ ] **Step 9: Commit**

```bash
git add docs/ram/ hack/verify-ram-policy.py README.md docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md config/samples/
git commit -m "docs: document the OSS binding target, its RAM segment and the spec deltas"
```

---

### Task 10: 真实云集成探针 #15–#20

**Files:**
- Create: `test/integration/oss_test.go`
- Modify: `test/integration/env.go`（两个环境变量常量）、`test/integration/env.example.sh`
- Modify: `test/integration/RESULTS.md`（**不要手改**——跑一次 `make test-integration` 让它生成六行「未实测」）

**Interfaces:**
- Consumes: 既有 `requireCAS`、`newCAS`、`uploadForTest`、`deleteForTest`、`itName`、`randHex`、`Record`、`RecordSkip`、`sdkSummary`、`errCode`、`callTimeout`、`testutil.NewCA / IssueLeafRSA`；Task 3 的 `aliyun.NewOSSClient`。
- Produces: 环境变量 `OSS_TEST_BUCKET`、`OSS_TEST_DOMAIN`；探针 `TestOSSBindByCertID`（#15/#16/#17/#18/#19/#20）。

- [ ] **Step 1: 环境变量**

`env.go` 常量块加：

```go
	EnvOSSTestBucket = "OSS_TEST_BUCKET"
	EnvOSSTestDomain = "OSS_TEST_DOMAIN"
```

`env.example.sh` 在 `FC3_TEST_DOMAIN` 之后加：

```bash
# 可选：OSS bucket 及其上一条已验证所有权的自定义域名（§12.3 #15–#20 会真的换绑它的证书，
# 结束时还原原来的 certId；别用生产域名）。两者必须同时设置。
export OSS_TEST_BUCKET=
export OSS_TEST_DOMAIN=
```

- [ ] **Step 2: 写 `oss_test.go`**

```go
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
func noteAuth(t *testing.T, err error) bool {
	if err == nil || aliyun.ClassOf(err) != aliyun.ClassAuth {
		return false
	}
	authSeen = true
	Record(t, "#20", q20oss, "偶遇一次鉴权失败：错误码="+errCode(err)+"，aliyun.classifyCode 判为 Auth",
		sdkSummary(err)+"；来自本组 OSS 探针的一次调用，未刻意构造缺权限账号")
	return true
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
		rctx, rcancel := context.WithTimeout(context.Background(), callTimeout)
		defer rcancel()
		var rerr error
		if before.CertRef != "" {
			rerr = oss.PutCnameCert(rctx, bucket, domain, before.CertRef)
		} else {
			rerr = oss.DeleteCnameCert(rctx, bucket, domain)
		}
		if rerr != nil {
			Record(t, "#15", q15oss+"（还原）", "还原失败，需人工把 CNAME 证书改回 "+before.CertRef, sdkSummary(rerr))
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
```

`strings` 若因此未被使用，删掉那条 import。

- [ ] **Step 3: 编译与 lint**

Run: `go vet -tags=integration ./test/integration/ && make lint`
Expected: 通过（`.golangci.yml` 已带 `integration` tag；该目录豁免 lll / dupl）。

- [ ] **Step 4: 生成 RESULTS 的「未实测」行**

在**没有** `OSS_TEST_BUCKET` 的环境里跑一次完整集成测试是不行的——它会把已有结论抹掉（README runbook 第 5 条）。正确做法：设置真实 CAS 凭证（按 §3 纪律用 awk 提取到环境变量，绝不打印）、不设 OSS 变量、整包跑 `make test-integration`，让 #15–#20 以「未实测」落进 `RESULTS.md`，其它编号照常复测。没有凭证就跳过本步，留给 Task 11 的实测一并回填，并在 commit message 里写明。

- [ ] **Step 5: Commit**

```bash
git add test/integration/
git commit -m "test(integration): add the OSS certId binding probes (#15-#20), gated on OSS_TEST_BUCKET/OSS_TEST_DOMAIN"
```

---

### Task 11: 发版 v0.2.0 与 `www.bestheme.ac.cn` 受控首绑（合并到 main 之后，需所有者放行）

> 本 Task 的每一步都作用于仓库之外（GitHub tag、GHCR、生产集群、线上域名的证书）。SDD 的四条停线里它命中两条（共享分支推送 / 发布、不可逆的线上变更）。**执行前向所有者复述将要做的事并取得明确同意；同意后再动。**

**Files:**
- Modify: `deploy/argocd/application-operator.yaml`（`controller=ghcr.io/bestheme/le-to-alicloud:v0.2.0`）
- 集群对象（GitOps 仓库或临时 manifests）：`AliyunCertificate/www-bestheme`、`AliyunCertificateBinding/www-oss`
- Modify: `test/integration/RESULTS.md`（由实测回填 #15 / #17，见 Step 6）

- [ ] **Step 1: 发版**

```bash
git tag v0.2.0 && git push origin main --tags
```

等 `Image` workflow 成功、`ghcr.io/bestheme/le-to-alicloud:v0.2.0` 匿名可拉（`curl -sI https://ghcr.io/v2/bestheme/le-to-alicloud/manifests/v0.2.0 -H "Accept: application/vnd.oci.image.index.v1+json"` 200）。

- [ ] **Step 2: 升级集群**

改 `deploy/argocd/application-operator.yaml` 的镜像 tag 为 `v0.2.0`，提交推送；Argo 同步 CRD 与 operator（namespace 仍需 `sed 's/namespace: argocd/namespace: openshift-gitops/'`，见 Plan 3 遗留项）。验证：

```bash
oc -n le-to-alicloud-system get deploy -o jsonpath='{.items[0].spec.template.spec.containers[0].image}'
oc get crd aliyuncertificatebindings.certs.bestheme.ac.cn -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.target.properties.type.enum}'
```

Expected: 镜像 `…:v0.2.0`，enum 含 `OSSCustomDomain`。现有 `timehorse-api-fc3` Binding 保持 `Ready=True`。

- [ ] **Step 3: RAM**

所有者在 operator 子账号上追加 `docs/ram/binding-oss-policy.json`（`<accountId>` = 102181 所属主账号 ID，`<bucket>` = `applanding-102181`）。

- [ ] **Step 4: 首绑（低流量时段）**

先记下当前 OSS 上的证书（回滚依据）：控制台「Bucket → 传输管理 → 域名管理 → www.bestheme.ac.cn → 证书」，或用探针的 `GetCname`。然后 apply：

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificate
metadata: { name: www-bestheme, namespace: <运行 timehorse 证书的同一 namespace 或新建> }
spec:
  certificateTemplate:
    dnsNames: [www.bestheme.ac.cn]
    issuerRef: { name: letsencrypt-prod, kind: ClusterIssuer }
  aliyun:
    credentialsRef: { name: <凭证 Secret> }
    region: cn-hangzhou
    uploadToCAS: true
---
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificateBinding
metadata: { name: www-oss, namespace: <同上> }
spec:
  certificateRef: { name: www-bestheme }
  target:
    type: OSSCustomDomain
    ossCustomDomain: { region: cn-hangzhou, bucket: applanding-102181, domainName: www.bestheme.ac.cn }
  deletionPolicy: Orphan
```

验证顺序：证书 `Issued=True`、`Uploaded=True`、`status.current.certId` 非空 → Binding `Applied=True`、`status.appliedCertRef == "<certId>-cn-hangzhou"` → `echo | openssl s_client -connect www.bestheme.ac.cn:443 -servername www.bestheme.ac.cn 2>/dev/null | openssl x509 -noout -issuer -fingerprint -sha256` 的指纹等于 `status.appliedFingerprint`。

回滚：控制台把 CNAME 证书改回 Step 4 开头记下的那一张，然后 `oc delete aliyuncertificatebinding www-oss`（Orphan，不动云）。

- [ ] **Step 5: 观察一轮漂移检查**

等一个 `--drift-check-interval`（默认 1h）或 `oc annotate aliyuncertificatebinding www-oss poke=1`：`aliyuncert_aliyun_api_requests_total{service="oss",action="PutCname"}` 不该增加，`action="ListCname"` +1。

- [ ] **Step 6: 回填实测结论**

首绑证明了 #15（首绑一半）与 #17（`appliedCertRef` 短路命中即逐字相同）；#16 只证明同区域；#18 / #19 / #20 仍未实测（不在生产域名上做）。把这三条结论写进主 spec §12.3 对应行（标明「实测于 <日期>，`www.bestheme.ac.cn` 受控首绑」）；`RESULTS.md` 由测试生成、不手改，若所有者后续提供测试 bucket 再整包跑一次回填。提交：

```bash
git commit -am "docs(spec): record the www.bestheme.ac.cn OSS first-bind results for #15/#16/#17"
```

---

## 自审记录（writing-plans Self-Review）

- **Spec 覆盖**：§3 API → Task 1；§4 契约 → Task 2；§6 client → Task 3；§7 provider → Task 4；§5.1 三元组 / §5.3 辅助函数 → Task 5；§5.2 步骤 3c / `CASRegion` → Task 6；§11.2 envtest → Task 6–7；§5.4 工厂 / §6.4 指标 → Task 8；§9 RAM 与文档 / §10 发版 / §16 决策 → Task 9、11；§11.3 探针 → Task 10；§8 CAS 耦合 → Task 10 的 #19 与 Task 9 的 spec 文字。
- **占位符**：无 TBD / TODO / 「类似 Task N」；每个代码步骤都给了完整代码。
- **类型一致性**：`identityOf(b, obs, m)` 在 Task 5 定义、Task 5/6/7 使用；`capabilitiesOf(b)` 在 Task 5 定义、Task 5/6 使用；`casUploadGate(caps, ac, m)` 在 Task 6 定义与使用；`fake.OSS` 的 `AddCname / Cname / ListCallsFor / PutCallsFor / QueuePutErr / AlwaysPutErr` 在 Task 3 定义、Task 4/6/7 使用；`aliyun.OSSClientConfig{Region, Timeout, Limiters, LimiterKey, OnCall}` 在 Task 3 定义、Task 8/10 使用；`ClientKey.Type` 在 Task 8 定义与使用；`certRefOf` 在 Task 6 定义、Task 7 使用。
