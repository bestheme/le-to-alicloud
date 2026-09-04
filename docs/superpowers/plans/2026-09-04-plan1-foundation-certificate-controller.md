# Plan 1 — 基础与证书 controller 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个可运行的 operator：`AliyunCertificate` 创建后经 cert-manager 签发证书、校验并规范化 PEM、上传阿里云 CAS、续期时轮换并按保留策略回收、删除时按正确顺序清理。

**Architecture:** Operator SDK（go/v4 插件，controller-runtime）单二进制。`api/v1alpha1` 定义两个 CRD 的类型（Binding 的 controller 留到 Plan 2，但类型现在就定，因为回收守卫和删除阻塞要读它）。`pkg/pki` 负责证书解析 / 校验 / 规范化，`pkg/aliyun` 用窄接口封装 CAS SDK 并提供 fake，`internal/controller` 实现证书 controller 的状态机。Secret 不缓存，一律直读 API server。

**Tech Stack:** Go ≥ 1.24、Operator SDK v1.42.3（go/v4）、controller-runtime（随脚手架）、cert-manager Go 类型 v1.21.1、`github.com/alibabacloud-go/cas-20200407/v4 v4.7.0`、`github.com/alibabacloud-go/darabonba-openapi/v2`、`github.com/alibabacloud-go/tea`（`dara`）、`github.com/aliyun/credentials-go v1.4.13`、`golang.org/x/time/rate`、envtest + Ginkgo v2（脚手架默认）。

**Spec:** `docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md`

## Global Constraints

- Go module 路径：`git.dev.bestheme.ac.cn/infra/le-to-alicloud`（如需更换，在 Task 1 完成前统一 `sed`，之后不改）。
- API group / version：`certs.bestheme.ac.cn/v1alpha1`；domain `bestheme.ac.cn`；kinds `AliyunCertificate`、`AliyunCertificateBinding`；两者 namespaced。
- Finalizer 名：`certs.bestheme.ac.cn/finalizer`。管理 label：`certs.bestheme.ac.cn/managed: "true"`。
- 最低 k8s 1.25（CEL）；CRD 必须有 `subresources.status`。
- **Secret 不进 informer cache**：manager client 对 `corev1.Secret` 设置 `DisableFor`；代码中禁止对 Secret 调用 `List` / 使用 `Watches`。RBAC 上 secrets 只允许 `get`、`delete`。
- **私钥内容绝不出现在日志、event、status、error message 中**；日志只允许 `namespace/name`、`domainName`、指纹前 8 位。
- 私钥默认编码 PKCS#1；输出 PEM 由 DER 重新编码（leaf → intermediates，无空行）。
- CAS 证书名：`sanitize(CR名)[:50] + "_" + fingerprint[:12]`，字符集 `[A-Za-z0-9_]`，总长 ≤ 63。ClientToken：`uid去连字符[:16] + fingerprint[:32]`（48 字符）。
- 所有阿里云调用必须带派生自 reconcile ctx、超时为 `--cloud-call-timeout`（默认 30s）的 context，并经过按 `(accessKeyId, service)` 的出站限流器：CAS list ≤ 8 QPS，CAS upload/delete ≤ 50 QPS。
- 错误分类：`Throttling*` / `ServiceUnavailable` / `InternalError` / HTTP ≥ 500 / 网络超时 → 可重试；`InvalidAccessKeyId*` / `SignatureDoesNotMatch` / `Forbidden*` / `NoPermission*` → 凭证类（不可重试，长 requeue）；其余 → 不可重试。
- Commit message 不加 `Co-Authored-By`（用户偏好）。Commit 使用 conventional commits（`feat:` / `test:` / `chore:` / `docs:`）。
- 代码注释用中文；标识符、API 名保持英文。
- 每个 Task 结束时 `make build && make test` 必须通过。

---

## 文件结构

| 路径 | 职责 |
|---|---|
| `cmd/main.go` | 脚手架入口；flags；manager 配置（禁用 Secret cache、leader election、field index）；注入 reconciler 依赖 |
| `api/v1alpha1/groupversion_info.go` | 脚手架生成 |
| `api/v1alpha1/conditions.go` | condition type 与 reason 常量、label / finalizer 常量 |
| `api/v1alpha1/aliyuncertificate_types.go` | `AliyunCertificate` spec/status、kubebuilder markers、CEL |
| `api/v1alpha1/aliyuncertificatebinding_types.go` | `AliyunCertificateBinding` spec/status、CEL（不可变、union）；`TargetKey()` |
| `pkg/pki/bundle.go` | `ParseBundle`、链 / 密钥匹配校验、指纹、`IsSelfSigned`、`CertPEM()` / `KeyPEM()` 规范化 |
| `pkg/pki/sans.go` | RFC 6125 通配符匹配 `Covers`、`Missing` |
| `pkg/pki/testutil/testutil.go` | 测试用 CA / leaf / 自签证书生成器（被 controller 测试复用） |
| `pkg/naming/naming.go` | `CASName`、`ClientToken` |
| `pkg/aliyun/cas.go` | `CASClient` 接口、`CertSummary` |
| `pkg/aliyun/errors.go` | `Error`、`Classify`、`ClassOf` |
| `pkg/aliyun/ratelimit.go` | `Limiters`（按 key 的 `rate.Limiter`） |
| `pkg/aliyun/credentials.go` | `CredentialsFromSecret`、`Build()` → `credential.Credential` |
| `pkg/aliyun/cas_sdk.go` | 真实 CAS client（SDK v4，`WithContext` 方法，限流，错误分类） |
| `pkg/aliyun/cache.go` | 按 `(namespace, name, resourceVersion, region, endpoint)` 缓存 client |
| `pkg/aliyun/fake/cas.go` | 内存 fake：token 幂等、名字唯一、错误注入、「服务端成功但响应丢失」 |
| `internal/controller/aliyuncertificate_controller.go` | `Reconcile` 主流程、`SetupWithManager` |
| `internal/controller/issuer.go` | `ResolveIssuerRef`（spec > status > existing > flag） |
| `internal/controller/desired.go` | 期望态 `cmapi.CertificateSpec` 构造、`secretNameFor` |
| `internal/controller/material.go` | 读 Secret → `pki.Bundle`，全部校验规则（含临时证书合取） |
| `internal/controller/upload.go` | CAS 上传、write-ahead `pendingUpload`、history 推进 |
| `internal/controller/retention.go` | 三重护栏回收 |
| `internal/controller/deletion.go` | finalizer：阻塞判断、有界 CAS 清理、Certificate → 等 NotFound → Secret |
| `internal/controller/status.go` | condition helpers、Ready 聚合、status patch |
| `internal/controller/metrics.go` | Prometheus 指标注册与更新 |
| `internal/controller/suite_test.go` | envtest（加载本项目 CRD + cert-manager CRD）、fake CAS、helper |
| `internal/controller/*_test.go` | 每个任务对应的 Ginkgo 测试 |
| `test/crds/cert-manager.crds.yaml` | cert-manager v1.21.1 CRD，仅 envtest 用 |

---

### Task 1: 工具链与脚手架

**Files:**
- Create: 由 `operator-sdk init` / `create api` 生成整个骨架（`cmd/main.go`、`api/v1alpha1/*`、`internal/controller/*`、`config/*`、`Makefile`、`Dockerfile`、`go.mod`）
- Create: `test/crds/cert-manager.crds.yaml`
- Modify: `go.mod`（追加依赖）

**Interfaces:**
- Produces: 可编译的骨架；`make manifests generate build test` 全绿；后续所有任务在此之上工作。

- [ ] **Step 1: 安装工具链（macOS）**

```bash
brew install go operator-sdk
go version            # 期望 go1.24 或更高
operator-sdk version  # 期望 v1.42.x
```

Linux 主机上用：`curl -LO https://github.com/operator-framework/operator-sdk/releases/download/v1.42.3/operator-sdk_linux_amd64 && chmod +x operator-sdk_linux_amd64 && sudo mv operator-sdk_linux_amd64 /usr/local/bin/operator-sdk`。

- [ ] **Step 2: 初始化项目**

在仓库根目录（已有 `docs/` 与 `.git/`）执行：

```bash
operator-sdk init --domain bestheme.ac.cn \
  --repo git.dev.bestheme.ac.cn/infra/le-to-alicloud \
  --plugins go/v4 \
  --project-name le-to-alicloud
```

Expected: 生成 `cmd/main.go`、`Makefile`、`PROJECT`、`config/`、`go.mod`，无报错。

- [ ] **Step 3: 创建两个 API**

```bash
operator-sdk create api --group certs --version v1alpha1 --kind AliyunCertificate --resource --controller
operator-sdk create api --group certs --version v1alpha1 --kind AliyunCertificateBinding --resource --controller=false
```

Expected: `api/v1alpha1/aliyuncertificate_types.go`、`api/v1alpha1/aliyuncertificatebinding_types.go`、`internal/controller/aliyuncertificate_controller.go` 存在；`PROJECT` 文件里两个 resource 条目。

- [ ] **Step 4: 追加依赖**

```bash
go get github.com/cert-manager/cert-manager@v1.21.1
go get github.com/alibabacloud-go/cas-20200407/v4@v4.7.0
go get github.com/alibabacloud-go/darabonba-openapi/v2@latest
go get github.com/alibabacloud-go/tea@latest
go get github.com/aliyun/credentials-go@v1.4.13
go get golang.org/x/time@latest
go mod tidy
```

Expected: `go mod tidy` 成功。若报 `k8s.io/*` 版本冲突，把 `k8s.io/api`、`k8s.io/apimachinery`、`k8s.io/client-go` 升到 cert-manager v1.21.1 的 `go.mod` 所要求的版本（`go list -m -json github.com/cert-manager/cert-manager@v1.21.1` 查看），再 `go mod tidy`。

- [ ] **Step 5: 下载 cert-manager CRD 供 envtest 使用**

```bash
mkdir -p test/crds
curl -sSL -o test/crds/cert-manager.crds.yaml \
  https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.crds.yaml
grep -c "^kind: CustomResourceDefinition" test/crds/cert-manager.crds.yaml
```

Expected: 计数为 6（Certificate、CertificateRequest、Issuer、ClusterIssuer、Order、Challenge）。

- [ ] **Step 6: 验证脚手架可构建可测试**

```bash
make manifests generate build test
```

Expected: 全部成功；`make test` 会自动下载 envtest 二进制并运行脚手架自带的空测试。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "chore: scaffold operator with operator-sdk go/v4 and pin dependencies"
```

---

### Task 2: `AliyunCertificate` 类型、常量与 CRD 校验测试

**Files:**
- Create: `api/v1alpha1/conditions.go`
- Modify: `api/v1alpha1/aliyuncertificate_types.go`（整体替换脚手架内容）
- Modify: `internal/controller/suite_test.go`（加载 cert-manager CRD、注册 scheme）
- Create: `internal/controller/crd_validation_test.go`

**Interfaces:**
- Produces（后续任务依赖的精确名字）：
  - `v1alpha1.AliyunCertificate{Spec AliyunCertificateSpec; Status AliyunCertificateStatus}`
  - `AliyunCertificateSpec{SecretName string; CertificateTemplate CertificateTemplate; Aliyun AliyunSpec; Retention RetentionSpec}`
  - `CertificateTemplate{IssuerRef *cmmeta.IssuerReference; CommonName string; DNSNames []string; IPAddresses []string; Duration, RenewBefore *metav1.Duration; Subject *cmapi.X509Subject; Usages []cmapi.KeyUsage; PrivateKey *cmapi.CertificatePrivateKey; SecretTemplate *cmapi.CertificateSecretTemplate}`
  - `AliyunSpec{CredentialsRef LocalSecretReference; Region, CASRegion, EndpointOverride, ResourceGroupID string; UploadToCAS *bool}`；方法 `(a AliyunSpec) UploadEnabled() bool`
  - `RetentionSpec{KeepLast int32; MinAge *metav1.Duration}`；方法 `(r RetentionSpec) MinAgeOrDefault() time.Duration`
  - `AliyunCertificateStatus{ObservedGeneration int64; EffectiveIssuerRef *cmmeta.IssuerReference; SecretName, CertManagerCertificateName string; Issuance *IssuanceStatus; Current *CertificateGeneration; History []CertificateGeneration; PendingUpload *PendingUpload; CASProbedAt *metav1.Time; Conditions []metav1.Condition}`
  - `CertificateGeneration{Fingerprint string; CertID *int64; CASName string; NotBefore, NotAfter, UploadedAt metav1.Time}`
  - `PendingUpload{Fingerprint, CASName, ClientToken string; StartedAt metav1.Time}`
  - `IssuanceStatus{Revision *int; RenewalTime, LastFailureTime *metav1.Time; FailedIssuanceAttempts *int}`
  - 常量：`ConditionReady/ConditionIssued/ConditionUploaded/ConditionIssuerDefaultDiverged`，全部 `Reason*`，`FinalizerName`，`LabelManaged`

- [ ] **Step 1: 写常量文件**

`api/v1alpha1/conditions.go`：

```go
package v1alpha1

// Condition types
const (
	ConditionReady                 = "Ready"
	ConditionIssued                = "Issued"
	ConditionUploaded              = "Uploaded"
	ConditionIssuerDefaultDiverged = "IssuerDefaultDiverged" // 不参与 Ready 聚合
	ConditionApplied               = "Applied"
	ConditionConflict              = "Conflict"
)

// AliyunCertificate reasons
const (
	ReasonNoIssuer                 = "NoIssuer"
	ReasonIssuerDefaultDiverged    = "IssuerDefaultDiverged"
	ReasonSecretNameConflict       = "SecretNameConflict"
	ReasonCertificateNotReady      = "CertificateNotReady"
	ReasonIssuanceStalled          = "IssuanceStalled"
	ReasonSecretNotFound           = "SecretNotFound"
	ReasonSecretInvalid            = "SecretInvalid"
	ReasonSelfSignedDuringIssuance = "SelfSignedDuringIssuance"
	ReasonSANsMismatch             = "SANsMismatch"
	ReasonCredentialsNotFound      = "CredentialsSecretNotFound"
	ReasonCredentialsInvalid       = "CredentialsInvalid"
	ReasonUploadFailed             = "UploadFailed"
	ReasonThrottled                = "Throttled"
	ReasonDeletionBlocked          = "DeletionBlockedByBindings"
	ReasonCleanupAbandoned         = "CleanupAbandoned"
	ReasonUploadDisabled           = "UploadDisabled"
	ReasonReady                    = "Ready"
)

// AliyunCertificateBinding reasons（Plan 2 使用，先定下来）
const (
	ReasonCertificateNotFound = "CertificateNotFound"
	ReasonTargetNotFound      = "TargetNotFound"
	ReasonDomainNotCovered    = "DomainNotCovered"
	ReasonConflictingBinding  = "ConflictingBinding"
	ReasonAccountMismatch     = "AccountMismatch"
	ReasonApplyFailed         = "ApplyFailed"
	ReasonDriftCorrected      = "DriftCorrected"
	ReasonApplied             = "Applied"
)

const (
	// FinalizerName 两个 CRD 共用
	FinalizerName = "certs.bestheme.ac.cn/finalizer"
	// LabelManaged 打在 operator 下发的 cert-manager Certificate 及其 Secret（经 secretTemplate）上
	LabelManaged = "certs.bestheme.ac.cn/managed"
)
```

- [ ] **Step 2: 写 `AliyunCertificate` 类型**

整体替换 `api/v1alpha1/aliyuncertificate_types.go`：

```go
package v1alpha1

import (
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalSecretReference 只允许引用同 namespace 的 Secret；故意不提供 namespace 字段。
type LocalSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// CertificateTemplate 是透传给 cert-manager Certificate 的字段子集。
// 顶层字段由本项目挑选，嵌套类型复用 cert-manager 的定义。
// +kubebuilder:validation:XValidation:rule="(has(self.dnsNames) && size(self.dnsNames) > 0) || (has(self.commonName) && self.commonName != '')",message="certificateTemplate 至少需要 dnsNames 或 commonName 之一"
type CertificateTemplate struct {
	// 缺省时回退到 operator 的 --default-issuer-* flag；首次生效后被固化到 status.effectiveIssuerRef。
	// +optional
	IssuerRef *cmmeta.IssuerReference `json:"issuerRef,omitempty"`
	// +optional
	CommonName string `json:"commonName,omitempty"`
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`
	// +optional
	IPAddresses []string `json:"ipAddresses,omitempty"`
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
	// +optional
	RenewBefore *metav1.Duration `json:"renewBefore,omitempty"`
	// +optional
	Subject *cmapi.X509Subject `json:"subject,omitempty"`
	// +optional
	Usages []cmapi.KeyUsage `json:"usages,omitempty"`
	// 缺省 encoding 为 PKCS1（阿里云 CAS / FC3 / CDN 三处文档一致要求）。
	// +optional
	PrivateKey *cmapi.CertificatePrivateKey `json:"privateKey,omitempty"`
	// +optional
	SecretTemplate *cmapi.CertificateSecretTemplate `json:"secretTemplate,omitempty"`
}

// AliyunSpec 描述阿里云侧的账号与位置。
type AliyunSpec struct {
	CredentialsRef LocalSecretReference `json:"credentialsRef"`
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// CAS 的 region；缺省等于 region。SDK 按 region 自动选择 endpoint（cn-* 全部落到 cas.aliyuncs.com）。
	// +optional
	CASRegion string `json:"casRegion,omitempty"`
	// 非空时覆盖 SDK 选出的 endpoint（VPC / 专有云）。
	// +optional
	EndpointOverride string `json:"endpointOverride,omitempty"`
	// +optional
	ResourceGroupID string `json:"resourceGroupId,omitempty"`
	// 为 false 时跳过 CAS 上传 / 回收 / 探测，Uploaded 不参与 Ready 聚合。
	// +kubebuilder:default=true
	// +optional
	UploadToCAS *bool `json:"uploadToCAS,omitempty"`
}

// UploadEnabled 把可选指针折叠成布尔，默认 true。
func (a AliyunSpec) UploadEnabled() bool {
	return a.UploadToCAS == nil || *a.UploadToCAS
}

// EffectiveCASRegion 返回 CAS 调用使用的 region。
func (a AliyunSpec) EffectiveCASRegion() string {
	if a.CASRegion != "" {
		return a.CASRegion
	}
	return a.Region
}

// RetentionSpec 控制 CAS 中保留多少代证书。
type RetentionSpec struct {
	// current + history 的总代数。
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	KeepLast int32 `json:"keepLast,omitempty"`
	// 代次自上传起未满 minAge 不回收。
	// +kubebuilder:default="24h"
	// +optional
	MinAge *metav1.Duration `json:"minAge,omitempty"`
}

const defaultMinAge = 24 * time.Hour

// MinAgeOrDefault 在 API server 未做默认（如单元测试直接构造对象）时兜底。
func (r RetentionSpec) MinAgeOrDefault() time.Duration {
	if r.MinAge == nil {
		return defaultMinAge
	}
	return r.MinAge.Duration
}

// KeepLastOrDefault 同上。
func (r RetentionSpec) KeepLastOrDefault() int {
	if r.KeepLast < 1 {
		return 2
	}
	return int(r.KeepLast)
}

// AliyunCertificateSpec 定义期望状态。
type AliyunCertificateSpec struct {
	// cert-manager 写入证书的 Secret 名；缺省 "<metadata.name>-tls"。
	// +optional
	SecretName string `json:"secretName,omitempty"`

	CertificateTemplate CertificateTemplate `json:"certificateTemplate"`

	Aliyun AliyunSpec `json:"aliyun"`

	// +kubebuilder:default={}
	// +optional
	Retention RetentionSpec `json:"retention,omitempty"`
}

// CertificateGeneration 是一代已上传（或已观测）的证书；不含 PEM。
type CertificateGeneration struct {
	// SHA-256(leaf DER)，小写 hex。
	Fingerprint string `json:"fingerprint"`
	// uploadToCAS=false 时为空。
	// +optional
	CertID *int64 `json:"certId,omitempty"`
	// +optional
	CASName    string      `json:"casName,omitempty"`
	NotBefore  metav1.Time `json:"notBefore"`
	NotAfter   metav1.Time `json:"notAfter"`
	UploadedAt metav1.Time `json:"uploadedAt"`
}

// PendingUpload 是 CAS 上传的 write-ahead 记录，成功后清空。
type PendingUpload struct {
	Fingerprint string      `json:"fingerprint"`
	CASName     string      `json:"casName"`
	ClientToken string      `json:"clientToken"`
	StartedAt   metav1.Time `json:"startedAt"`
}

// IssuanceStatus 镜像 cert-manager Certificate 的关键 status 字段。
type IssuanceStatus struct {
	// +optional
	Revision *int `json:"revision,omitempty"`
	// +optional
	RenewalTime *metav1.Time `json:"renewalTime,omitempty"`
	// +optional
	FailedIssuanceAttempts *int `json:"failedIssuanceAttempts,omitempty"`
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
}

// AliyunCertificateStatus 定义观测状态。
type AliyunCertificateStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// 实际生效并被固化（Pin）的 issuer。
	// +optional
	EffectiveIssuerRef *cmmeta.IssuerReference `json:"effectiveIssuerRef,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	CertManagerCertificateName string `json:"certManagerCertificateName,omitempty"`
	// +optional
	Issuance *IssuanceStatus `json:"issuance,omitempty"`
	// +optional
	Current *CertificateGeneration `json:"current,omitempty"`
	// 最多 keepLast-1 条。
	// +optional
	History []CertificateGeneration `json:"history,omitempty"`
	// +optional
	PendingUpload *PendingUpload `json:"pendingUpload,omitempty"`
	// +optional
	CASProbedAt *metav1.Time `json:"casProbedAt,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Issued",type=string,JSONPath=`.status.conditions[?(@.type=="Issued")].status`
// +kubebuilder:printcolumn:name="Uploaded",type=string,JSONPath=`.status.conditions[?(@.type=="Uploaded")].status`
// +kubebuilder:printcolumn:name="Effective-Issuer",type=string,JSONPath=`.status.effectiveIssuerRef.name`
// +kubebuilder:printcolumn:name="Not-After",type=string,JSONPath=`.status.current.notAfter`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AliyunCertificate 描述一张由 cert-manager 签发并托管到阿里云 CAS 的证书。
type AliyunCertificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AliyunCertificateSpec   `json:"spec,omitempty"`
	Status AliyunCertificateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AliyunCertificateList contains a list of AliyunCertificate
type AliyunCertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AliyunCertificate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AliyunCertificate{}, &AliyunCertificateList{})
}
```

- [ ] **Step 3: 生成代码与 CRD**

```bash
make generate manifests && go build ./...
grep -n "keepLast" -A3 config/crd/bases/certs.bestheme.ac.cn_aliyuncertificates.yaml | head -20
```

Expected: 编译通过；CRD 中 `keepLast` 带 `default: 2`、`minimum: 1`；`retention` 带 `default: {}`。

- [ ] **Step 4: 改 envtest suite，加载 cert-manager CRD 并注册 scheme**

替换 `internal/controller/suite_test.go` 中 `BeforeSuite` 的 `testEnv` 与 scheme 部分为：

```go
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	// 脚手架已有 BinaryAssetsDirectory 的处理，保留。

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(certsv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(cmapi.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
```

并在 import 中加入 `cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"`。

- [ ] **Step 5: 写 CRD 校验的失败测试**

`internal/controller/crd_validation_test.go`：

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

var _ = Describe("AliyunCertificate CRD 校验", func() {
	ctx := context.Background()

	newAC := func(name string) *certsv1alpha1.AliyunCertificate {
		return &certsv1alpha1.AliyunCertificate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: certsv1alpha1.AliyunCertificateSpec{
				CertificateTemplate: certsv1alpha1.CertificateTemplate{
					DNSNames: []string{"api.example.com"},
				},
				Aliyun: certsv1alpha1.AliyunSpec{
					CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun"},
					Region:         "cn-hangzhou",
				},
			},
		}
	}

	It("应用 retention 与 uploadToCAS 的默认值", func() {
		ac := newAC("defaults")
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		got := &certsv1alpha1.AliyunCertificate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "defaults", Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Retention.KeepLast).To(Equal(int32(2)))
		Expect(got.Spec.Retention.MinAge).NotTo(BeNil())
		Expect(got.Spec.Retention.MinAge.Duration.Hours()).To(Equal(24.0))
		Expect(got.Spec.Aliyun.UploadToCAS).NotTo(BeNil())
		Expect(*got.Spec.Aliyun.UploadToCAS).To(BeTrue())
	})

	It("拒绝 keepLast < 1", func() {
		ac := newAC("keeplast-zero")
		ac.Spec.Retention.KeepLast = 0
		ac.Spec.Retention.MinAge = &metav1.Duration{}
		// keepLast=0 会被 omitempty 省略进而被默认成 2，所以用 -1 触发 minimum
		ac.Spec.Retention.KeepLast = -1
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})

	It("拒绝 dnsNames 与 commonName 都为空", func() {
		ac := newAC("no-names")
		ac.Spec.CertificateTemplate.DNSNames = nil
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})

	It("拒绝缺少 credentialsRef.name", func() {
		ac := newAC("no-creds")
		ac.Spec.Aliyun.CredentialsRef.Name = ""
		Expect(k8sClient.Create(ctx, ac)).NotTo(Succeed())
	})
})
```

- [ ] **Step 6: 运行测试，确认先失败**

```bash
make test 2>&1 | tail -30
```

Expected: 若 Step 2/3 未完成会编译失败；完成后四个用例全部 PASS（这一任务的「失败测试」主要用于锁定 CRD 默认值与校验行为——先运行一次确认 CEL 规则真的被 API server 执行，若 `拒绝 dnsNames 与 commonName 都为空` 意外通过 Create，说明 CEL marker 没生成到 CRD，检查 `make manifests` 输出中的 `x-kubernetes-validations`）。

- [ ] **Step 7: 提交**

```bash
git add api/v1alpha1 config/crd internal/controller/suite_test.go internal/controller/crd_validation_test.go
git commit -m "feat(api): define AliyunCertificate types, conditions and CRD validation tests"
```

---

### Task 3: `AliyunCertificateBinding` 类型与 CEL 不可变 / union 校验

**Files:**
- Modify: `api/v1alpha1/aliyuncertificatebinding_types.go`（整体替换脚手架内容）
- Create: `internal/controller/binding_crd_validation_test.go`

**Interfaces:**
- Produces：
  - `v1alpha1.AliyunCertificateBinding{Spec AliyunCertificateBindingSpec; Status AliyunCertificateBindingStatus}`
  - `AliyunCertificateBindingSpec{CertificateRef LocalObjectReference; Target BindingTarget; CredentialsRef *LocalSecretReference; DeletionPolicy DeletionPolicy}`
  - `BindingTarget{Type string; FC3CustomDomain *FC3CustomDomainTarget}`；`FC3CustomDomainTarget{Region, DomainName string; EnsureHTTPSProtocol bool}`
  - `DeletionPolicy` 常量 `DeletionPolicyOrphan = "Orphan"`、`DeletionPolicyUnbind = "Unbind"`
  - `AliyunCertificateBindingStatus{ObservedGeneration int64; AppliedFingerprint string; LastAppliedTime, LastObservedTime *metav1.Time; BoundAccountID string; Conditions []metav1.Condition}`
  - `func (b *AliyunCertificateBinding) TargetKey() string` → `"<type>/<region>/<identifier>"`（Plan 2 的冲突索引键，Plan 1 的回收守卫不用）
  - 索引常量 `IndexBindingByCertificate = "spec.certificateRef.name"`

- [ ] **Step 1: 写类型**

整体替换 `api/v1alpha1/aliyuncertificatebinding_types.go`：

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalObjectReference 引用同 namespace 的 AliyunCertificate。
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// DeletionPolicy 决定删除 Binding 时是否解绑云侧证书。
// +kubebuilder:validation:Enum=Orphan;Unbind
type DeletionPolicy string

const (
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
	DeletionPolicyUnbind DeletionPolicy = "Unbind"
)

// TargetTypeFC3CustomDomain 是第一个 provider 类型。
const TargetTypeFC3CustomDomain = "FC3CustomDomain"

// IndexBindingByCertificate 是 controller-runtime field index 的键名。
const IndexBindingByCertificate = "spec.certificateRef.name"

// FC3CustomDomainTarget 指向一个 FC3 自定义域名。证书归属于域名，与函数无关。
type FC3CustomDomainTarget struct {
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// +kubebuilder:validation:MinLength=1
	DomainName string `json:"domainName"`
	// 为 true 且域名当前 protocol 不含 HTTPS 时，才把 protocol 改为 "HTTP,HTTPS"。默认不动。
	// +optional
	EnsureHTTPSProtocol bool `json:"ensureHTTPSProtocol,omitempty"`
}

// BindingTarget 是 discriminated union：type 决定哪个内嵌块必须存在。
// +kubebuilder:validation:XValidation:rule="self.type == 'FC3CustomDomain' ? has(self.fc3CustomDomain) : !has(self.fc3CustomDomain)",message="fc3CustomDomain 必须且只能在 type=FC3CustomDomain 时设置"
type BindingTarget struct {
	// +kubebuilder:validation:Enum=FC3CustomDomain
	Type string `json:"type"`
	// +optional
	FC3CustomDomain *FC3CustomDomainTarget `json:"fc3CustomDomain,omitempty"`
}

// AliyunCertificateBindingSpec 定义期望状态。
type AliyunCertificateBindingSpec struct {
	CertificateRef LocalObjectReference `json:"certificateRef"`

	// 不可变：改目标请新建 Binding。
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target 不可变，请新建 Binding"
	Target BindingTarget `json:"target"`

	// 缺省继承 certificateRef 所指证书的 aliyun.credentialsRef。
	// +optional
	CredentialsRef *LocalSecretReference `json:"credentialsRef,omitempty"`

	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// AliyunCertificateBindingStatus 定义观测状态。
type AliyunCertificateBindingStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// 目标上实际生效的证书指纹。
	// +optional
	AppliedFingerprint string `json:"appliedFingerprint,omitempty"`
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
	// 首次成功 Apply 时固化，用于账号 fencing。
	// +optional
	BoundAccountID string `json:"boundAccountId,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="Applied")].status`
// +kubebuilder:printcolumn:name="Conflict",type=string,JSONPath=`.status.conditions[?(@.type=="Conflict")].status`
// +kubebuilder:printcolumn:name="Certificate",type=string,JSONPath=`.spec.certificateRef.name`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.fc3CustomDomain.domainName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AliyunCertificateBinding 把一张 AliyunCertificate 部署到一个阿里云目标。
type AliyunCertificateBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AliyunCertificateBindingSpec   `json:"spec,omitempty"`
	Status AliyunCertificateBindingStatus `json:"status,omitempty"`
}

// TargetKey 返回 "<type>/<region>/<identifier>"，用于同目标冲突索引。
func (b *AliyunCertificateBinding) TargetKey() string {
	t := b.Spec.Target
	switch t.Type {
	case TargetTypeFC3CustomDomain:
		if t.FC3CustomDomain == nil {
			return ""
		}
		return t.Type + "/" + t.FC3CustomDomain.Region + "/" + t.FC3CustomDomain.DomainName
	default:
		return ""
	}
}

// +kubebuilder:object:root=true

// AliyunCertificateBindingList contains a list of AliyunCertificateBinding
type AliyunCertificateBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AliyunCertificateBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AliyunCertificateBinding{}, &AliyunCertificateBindingList{})
}
```

- [ ] **Step 2: 生成并编译**

```bash
make generate manifests && go build ./...
grep -n "x-kubernetes-validations" -A4 config/crd/bases/certs.bestheme.ac.cn_aliyuncertificatebindings.yaml | head -30
```

Expected: 两条 CEL 规则（union、`self == oldSelf`）出现在 CRD 中。

- [ ] **Step 3: 写校验测试**

`internal/controller/binding_crd_validation_test.go`：

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

var _ = Describe("AliyunCertificateBinding CRD 校验", func() {
	ctx := context.Background()

	newBinding := func(name string) *certsv1alpha1.AliyunCertificateBinding {
		return &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "cert"},
				Target: certsv1alpha1.BindingTarget{
					Type: certsv1alpha1.TargetTypeFC3CustomDomain,
					FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{
						Region: "cn-hangzhou", DomainName: "api.example.com",
					},
				},
			},
		}
	}

	It("默认 deletionPolicy 为 Orphan", func() {
		b := newBinding("defaults")
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		got := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "defaults", Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.DeletionPolicy).To(Equal(certsv1alpha1.DeletionPolicyOrphan))
		Expect(got.TargetKey()).To(Equal("FC3CustomDomain/cn-hangzhou/api.example.com"))
	})

	It("拒绝 type=FC3CustomDomain 但缺少 fc3CustomDomain", func() {
		b := newBinding("union-missing")
		b.Spec.Target.FC3CustomDomain = nil
		Expect(k8sClient.Create(ctx, b)).NotTo(Succeed())
	})

	It("target 不可变", func() {
		b := newBinding("immutable")
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		b.Spec.Target.FC3CustomDomain.DomainName = "other.example.com"
		Expect(k8sClient.Update(ctx, b)).NotTo(Succeed())
	})

	It("拒绝未知 deletionPolicy", func() {
		b := newBinding("bad-policy")
		b.Spec.DeletionPolicy = "Delete"
		Expect(k8sClient.Create(ctx, b)).NotTo(Succeed())
	})
})
```

- [ ] **Step 4: 运行测试**

```bash
make test 2>&1 | tail -20
```

Expected: 全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add api/v1alpha1 config/crd internal/controller/binding_crd_validation_test.go
git commit -m "feat(api): define AliyunCertificateBinding types with immutable target and union CEL"
```

---

### Task 4: `pkg/pki` — 解析、校验、指纹、PEM 规范化

**Files:**
- Create: `pkg/pki/bundle.go`
- Create: `pkg/pki/testutil/testutil.go`
- Create: `pkg/pki/bundle_test.go`

**Interfaces:**
- Produces：
  - `pki.ParseBundle(certPEM, keyPEM []byte) (*Bundle, error)`
  - `type Bundle struct { Leaf *x509.Certificate; Intermediates []*x509.Certificate; Signer crypto.Signer; Fingerprint string }`
  - `(b *Bundle) CertPEM() []byte`、`(b *Bundle) KeyPEM() ([]byte, error)`、`(b *Bundle) IsSelfSigned() bool`、`(b *Bundle) DNSNames() []string`
  - 哨兵错误：`ErrNoCertificate`、`ErrNoPrivateKey`、`ErrEncryptedKey`、`ErrUnsupportedKey`、`ErrKeyMismatch`、`ErrBrokenChain`
  - `testutil.NewCA(t)`, `testutil.IssueLeaf(t, ca, dnsNames...)`, `testutil.SelfSigned(t, dnsNames...)`，均返回 `(certPEM, keyPEM []byte)`；`testutil.CA` 暴露 `Cert *x509.Certificate` 与 `Key crypto.Signer`；`testutil.ToPKCS8(t, keyPEM) []byte`；`testutil.Encrypted(t) []byte`

- [ ] **Step 1: 写测试工具（不是测试本身，供多处复用）**

`pkg/pki/testutil/testutil.go`：

```go
// Package testutil 生成测试用证书材料。只在 _test 中使用。
package testutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// CA 是测试用中间 CA（自签根，直接签 leaf，模拟 LE 的 leaf+intermediate 形状时把它当 intermediate）。
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

func mustKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return k
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("生成序列号失败: %v", err)
	}
	return n
}

// NewCA 生成一个自签 CA。
func NewCA(t testing.TB) *CA {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "Test Intermediate CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 CA 失败: %v", err)
	}
	return &CA{Cert: cert, Key: key}
}

// IssueLeaf 由 ca 签发 leaf，返回「leaf + ca」的证书 PEM 与 leaf 的 EC 私钥 PEM（SEC1）。
func IssueLeaf(t testing.TB, ca *CA, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		t.Fatalf("签发 leaf 失败: %v", err)
	}
	certPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})...)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥失败: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// SelfSigned 生成自签 leaf（模拟 cert-manager 临时证书或 SelfSigned issuer）。
func SelfSigned(t testing.TB, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("创建自签证书失败: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// ToPKCS8 把 SEC1 EC 私钥 PEM 转成 PKCS#8 PEM。
func ToPKCS8(t testing.TB, keyPEM []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(keyPEM)
	k, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		t.Fatalf("解析 EC 私钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("PKCS8 编码失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// Encrypted 返回一个带 Proc-Type: 4,ENCRYPTED 头的 PEM 块（内容随意，用于测试拒绝路径）。
func Encrypted(t testing.TB) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"},
		Bytes:   []byte("not-a-real-key"),
	})
}
```

- [ ] **Step 2: 写失败测试**

`pkg/pki/bundle_test.go`：

```go
package pki_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func TestParseBundle_LeafPlusIntermediate(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "api.example.com", "www.example.com")

	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("期望成功，得到 %v", err)
	}
	if b.Leaf.Subject.CommonName != "api.example.com" {
		t.Errorf("leaf CN = %q", b.Leaf.Subject.CommonName)
	}
	if len(b.Intermediates) != 1 {
		t.Errorf("intermediates = %d, want 1", len(b.Intermediates))
	}
	sum := sha256.Sum256(b.Leaf.Raw)
	if b.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Errorf("指纹不等于 sha256(leaf DER)")
	}
	if b.IsSelfSigned() {
		t.Errorf("CA 签发的 leaf 不应被判为自签")
	}
	if got := b.DNSNames(); len(got) != 2 {
		t.Errorf("DNSNames = %v", got)
	}
}

func TestParseBundle_SelfSigned(t *testing.T) {
	certPEM, keyPEM := testutil.SelfSigned(t, "tmp.example.com")
	b, err := pki.ParseBundle(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("自签证书本身应可解析: %v", err)
	}
	if !b.IsSelfSigned() {
		t.Errorf("应判为自签")
	}
}

func TestParseBundle_KeyMismatch(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	_, otherKey := testutil.IssueLeaf(t, ca, "b.example.com")
	_, err := pki.ParseBundle(certPEM, otherKey)
	if !errors.Is(err, pki.ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
}

func TestParseBundle_BrokenChain(t *testing.T) {
	ca1 := testutil.NewCA(t)
	ca2 := testutil.NewCA(t)
	leafPEM, keyPEM := testutil.IssueLeaf(t, ca1, "a.example.com")
	// 把链里的 CA 换成不相关的 ca2
	blk, rest := pem.Decode(leafPEM)
	_ = rest
	bad := append(pem.EncodeToMemory(blk), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca2.Cert.Raw})...)
	_, err := pki.ParseBundle(bad, keyPEM)
	if !errors.Is(err, pki.ErrBrokenChain) {
		t.Fatalf("want ErrBrokenChain, got %v", err)
	}
}

func TestParseBundle_EncryptedKeyRejected(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	_, err := pki.ParseBundle(certPEM, testutil.Encrypted(t))
	if !errors.Is(err, pki.ErrEncryptedKey) {
		t.Fatalf("want ErrEncryptedKey, got %v", err)
	}
}

func TestParseBundle_EmptyInputs(t *testing.T) {
	if _, err := pki.ParseBundle(nil, []byte("x")); !errors.Is(err, pki.ErrNoCertificate) {
		t.Errorf("want ErrNoCertificate, got %v", err)
	}
	ca := testutil.NewCA(t)
	certPEM, _ := testutil.IssueLeaf(t, ca, "a.example.com")
	if _, err := pki.ParseBundle(certPEM, nil); !errors.Is(err, pki.ErrNoPrivateKey) {
		t.Errorf("want ErrNoPrivateKey, got %v", err)
	}
}

func TestBundle_CertPEM_Normalized(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "a.example.com")
	// 人为加入空行与注释，规范化后必须消失
	dirty := append([]byte("# bundle comment\n\n"), bytes.ReplaceAll(certPEM, []byte("-----END CERTIFICATE-----\n"), []byte("-----END CERTIFICATE-----\n\n"))...)
	b, err := pki.ParseBundle(dirty, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	out := b.CertPEM()
	if bytes.Contains(out, []byte("\n\n")) {
		t.Errorf("规范化输出不应含空行")
	}
	if bytes.Contains(out, []byte("#")) {
		t.Errorf("规范化输出不应含注释")
	}
	if !bytes.HasPrefix(out, []byte("-----BEGIN CERTIFICATE-----\n")) {
		t.Errorf("应以 BEGIN CERTIFICATE 开头")
	}
	if c := bytes.Count(out, []byte("-----BEGIN CERTIFICATE-----")); c != 2 {
		t.Errorf("应含 2 张证书，得到 %d", c)
	}
	// leaf 必须在前
	first, _ := pem.Decode(out)
	if !bytes.Equal(first.Bytes, b.Leaf.Raw) {
		t.Errorf("第一张必须是 leaf")
	}
}

func TestBundle_KeyPEM_PKCS8InputBecomesSEC1(t *testing.T) {
	ca := testutil.NewCA(t)
	certPEM, keyPEM := testutil.IssueLeaf(t, ca, "a.example.com")
	pkcs8 := testutil.ToPKCS8(t, keyPEM)
	b, err := pki.ParseBundle(certPEM, pkcs8)
	if err != nil {
		t.Fatalf("PKCS8 输入应可解析: %v", err)
	}
	out, err := b.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(out)
	if blk.Type != "EC PRIVATE KEY" {
		t.Errorf("EC 密钥应输出 SEC1 (EC PRIVATE KEY)，得到 %q", blk.Type)
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./pkg/pki/... 2>&1 | head
```

Expected: 编译失败，`undefined: pki.ParseBundle` 等。

- [ ] **Step 4: 实现 `bundle.go`**

`pkg/pki/bundle.go`：

```go
// Package pki 负责证书材料的解析、校验与 PEM 规范化。
// 这是 Secret → 阿里云之间唯一的数据变换点，所有格式要求在这里一次满足。
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

var (
	ErrNoCertificate  = errors.New("pki: 未找到 CERTIFICATE PEM 块")
	ErrNoPrivateKey   = errors.New("pki: 未找到私钥 PEM 块")
	ErrEncryptedKey   = errors.New("pki: 私钥已加密，阿里云不接受")
	ErrUnsupportedKey = errors.New("pki: 不支持的私钥类型（仅 RSA / ECDSA）")
	ErrKeyMismatch    = errors.New("pki: 私钥与 leaf 公钥不匹配")
	ErrBrokenChain    = errors.New("pki: 证书链不连续")
)

// Bundle 是解析并校验过的证书材料。
type Bundle struct {
	Leaf          *x509.Certificate
	Intermediates []*x509.Certificate
	Signer        crypto.Signer
	// Fingerprint = hex(sha256(Leaf.Raw))，全系统幂等基准。
	Fingerprint string
}

// ParseBundle 解析 tls.crt / tls.key，校验链连续性与公私钥匹配。
// 不校验有效期、不校验信任锚（那是 cert-manager 的职责）。
func ParseBundle(certPEM, keyPEM []byte) (*Bundle, error) {
	certs, err := parseCertificates(certPEM)
	if err != nil {
		return nil, err
	}
	signer, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Leaf: certs[0], Intermediates: certs[1:], Signer: signer}

	// 链连续性：certs[i] 必须由 certs[i+1] 签发
	for i := 0; i+1 < len(certs); i++ {
		if err := certs[i].CheckSignatureFrom(certs[i+1]); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 张不是由第 %d 张签发: %v", ErrBrokenChain, i, i+1, err)
		}
	}

	// 公私钥匹配
	pub, ok := b.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil, ErrKeyMismatch
	}

	sum := sha256.Sum256(b.Leaf.Raw)
	b.Fingerprint = hex.EncodeToString(sum[:])
	return b, nil
}

func parseCertificates(certPEM []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := certPEM
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue // 忽略 bundle 里夹带的其它块
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pki: 解析证书失败: %w", err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrNoCertificate
	}
	return out, nil
}

func parsePrivateKey(keyPEM []byte) (crypto.Signer, error) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, ErrNoPrivateKey
	}
	if procType, ok := blk.Headers["Proc-Type"]; ok && bytes.Contains([]byte(procType), []byte("ENCRYPTED")) {
		return nil, ErrEncryptedKey
	}
	var key any
	var err error
	switch blk.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	case "ENCRYPTED PRIVATE KEY":
		return nil, ErrEncryptedKey
	default:
		return nil, fmt.Errorf("%w: PEM 类型 %q", ErrUnsupportedKey, blk.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("pki: 解析私钥失败: %w", err)
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return k, nil
	default:
		return nil, ErrUnsupportedKey
	}
}

// IsSelfSigned 判断 leaf 是否自签（Issuer == Subject 且签名可由自身公钥验证）。
func (b *Bundle) IsSelfSigned() bool {
	if !bytes.Equal(b.Leaf.RawIssuer, b.Leaf.RawSubject) {
		return false
	}
	return b.Leaf.CheckSignatureFrom(b.Leaf) == nil
}

// DNSNames 返回 leaf 的 SANs；若 CN 不在 SANs 中且非空，则追加（兼容老证书）。
func (b *Bundle) DNSNames() []string {
	names := append([]string(nil), b.Leaf.DNSNames...)
	cn := b.Leaf.Subject.CommonName
	if cn == "" {
		return names
	}
	for _, n := range names {
		if n == cn {
			return names
		}
	}
	return append(names, cn)
}

// CertPEM 由 DER 重新编码：leaf 在前、中间证书紧随、无空行、64 字符/行。
func (b *Bundle) CertPEM() []byte {
	var buf bytes.Buffer
	buf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Leaf.Raw}))
	for _, c := range b.Intermediates {
		buf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return buf.Bytes()
}

// KeyPEM 输出阿里云期望的私钥编码：RSA → PKCS#1，ECDSA → SEC1。
func (b *Bundle) KeyPEM() ([]byte, error) {
	switch k := b.Signer.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("pki: 编码 EC 私钥失败: %w", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
	default:
		return nil, ErrUnsupportedKey
	}
}
```

- [ ] **Step 5: 运行测试**

```bash
go test ./pkg/pki/... -v 2>&1 | tail -20
```

Expected: 8 个测试 PASS。

- [ ] **Step 6: 提交**

```bash
git add pkg/pki
git commit -m "feat(pki): parse, validate and normalize certificate bundles"
```

---

### Task 5: `pkg/pki/sans.go` — RFC 6125 域名覆盖判断

**Files:**
- Create: `pkg/pki/sans.go`
- Create: `pkg/pki/sans_test.go`

**Interfaces:**
- Produces：`pki.Covers(pattern, host string) bool`、`pki.Missing(dnsNames, required []string) []string`（返回 required 中未被任何 dnsNames 覆盖者，已排序去重）

- [ ] **Step 1: 写失败测试**

`pkg/pki/sans_test.go`：

```go
package pki_test

import (
	"reflect"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

func TestCovers(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"api.example.com", "api.example.com", true},
		{"API.Example.COM", "api.example.com", true},
		{"api.example.com.", "api.example.com", true},
		{"api.example.com", "www.example.com", false},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.b.example.com", false},
		{"*.com", "example.com", false},        // 通配符不能覆盖公共后缀层级
		{"a*.example.com", "abc.example.com", false}, // 只接受整标签通配
		{"*.*.example.com", "a.b.example.com", false},
		{"", "api.example.com", false},
		{"api.example.com", "", false},
	}
	for _, c := range cases {
		if got := pki.Covers(c.pattern, c.host); got != c.want {
			t.Errorf("Covers(%q,%q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestMissing(t *testing.T) {
	got := pki.Missing(
		[]string{"*.example.com", "example.com"},
		[]string{"api.example.com", "example.com", "deep.a.example.com", "other.org", "api.example.com"},
	)
	want := []string{"deep.a.example.com", "other.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %v, want %v", got, want)
	}
	if m := pki.Missing([]string{"a.example.com"}, nil); len(m) != 0 {
		t.Errorf("空 required 应返回空，得到 %v", m)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./pkg/pki/... 2>&1 | head -5
```

Expected: `undefined: pki.Covers`。

- [ ] **Step 3: 实现**

`pkg/pki/sans.go`：

```go
package pki

import (
	"sort"
	"strings"
)

// Covers 按 RFC 6125 §6.4.3 判断证书中的 pattern 是否覆盖 host：
//   - 大小写不敏感，忽略尾部 "."
//   - 通配符只允许作为最左标签且必须是整个标签（"*.example.com"）
//   - 通配符恰好匹配一个标签，且 pattern 至少要有三个标签（不允许 "*.com"）
func Covers(pattern, host string) bool {
	p := strings.ToLower(strings.TrimSuffix(pattern, "."))
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if p == "" || h == "" {
		return false
	}
	if p == h {
		return true
	}
	if !strings.HasPrefix(p, "*.") {
		return false
	}
	pl := strings.Split(p, ".")
	hl := strings.Split(h, ".")
	if len(pl) < 3 || len(pl) != len(hl) {
		return false
	}
	for i := 1; i < len(pl); i++ {
		if pl[i] == "" || strings.Contains(pl[i], "*") || pl[i] != hl[i] {
			return false
		}
	}
	return hl[0] != ""
}

// Missing 返回 required 中未被任何 dnsNames 覆盖的域名（去重、排序）。
func Missing(dnsNames, required []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range required {
		key := strings.ToLower(strings.TrimSuffix(r, "."))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		covered := false
		for _, n := range dnsNames {
			if Covers(n, r) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: 运行测试**

```bash
go test ./pkg/pki/... -run 'TestCovers|TestMissing' -v 2>&1 | tail -6
```

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add pkg/pki/sans.go pkg/pki/sans_test.go
git commit -m "feat(pki): RFC 6125 wildcard coverage check"
```

---

### Task 6: `pkg/naming` — CAS 证书名与 ClientToken

**Files:**
- Create: `pkg/naming/naming.go`
- Create: `pkg/naming/naming_test.go`

**Interfaces:**
- Produces：`naming.CASName(crName, fingerprint string) string`、`naming.ClientToken(uid types.UID, fingerprint string) string`
- 约束：CASName 字符集 `[A-Za-z0-9_]`，长度 ≤ 63，格式 `<prefix≤50>_<fp[:12]>`；ClientToken 48 字符 `[0-9a-f]`

- [ ] **Step 1: 写失败测试**

`pkg/naming/naming_test.go`：

```go
package naming_test

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

const fp = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

var casNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func TestCASName_Basic(t *testing.T) {
	got := naming.CASName("timehorse-api", fp)
	if got != "timehorse_api_ab12cd34ef56" {
		t.Fatalf("got %q", got)
	}
}

func TestCASName_SanitizesAndTruncates(t *testing.T) {
	long := strings.Repeat("api.example.com-", 6) // 96 字符，含 . 和 -
	got := naming.CASName(long, fp)
	if len(got) > 63 {
		t.Errorf("长度 %d 超过 63", len(got))
	}
	if !casNameRe.MatchString(got) {
		t.Errorf("含非法字符: %q", got)
	}
	if !strings.HasSuffix(got, "_ab12cd34ef56") {
		t.Errorf("应以 _<指纹12位> 结尾: %q", got)
	}
	if strings.Contains(got, "__ab12") {
		t.Errorf("截断后不应留下尾部下划线导致双下划线: %q", got)
	}
}

func TestCASName_EmptyOrAllInvalidPrefix(t *testing.T) {
	got := naming.CASName("...", fp)
	if !strings.HasPrefix(got, "cert_") {
		t.Errorf("全非法字符的名字应回退为 cert_ 前缀: %q", got)
	}
}

func TestCASName_DifferentFingerprintsDiffer(t *testing.T) {
	a := naming.CASName("x", fp)
	b := naming.CASName("x", "ff"+fp[2:])
	if a == b {
		t.Errorf("不同指纹必须得到不同名字")
	}
}

func TestClientToken(t *testing.T) {
	got := naming.ClientToken(types.UID("6ba7b810-9dad-11d1-80b4-00c04fd430c8"), fp)
	if len(got) != 48 {
		t.Fatalf("长度 %d, want 48: %q", len(got), got)
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(got) {
		t.Errorf("应为 48 位小写 hex: %q", got)
	}
	if !strings.HasPrefix(got, "6ba7b8109dad11d1") {
		t.Errorf("前 16 位应为去连字符的 UID: %q", got)
	}
	if !strings.HasSuffix(got, fp[:32]) {
		t.Errorf("后 32 位应为指纹前 32 位")
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./pkg/naming/... 2>&1 | head -3
```

- [ ] **Step 3: 实现**

`pkg/naming/naming.go`：

```go
// Package naming 生成阿里云侧的名字与幂等令牌。
package naming

import (
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	maxCASNameLen = 63
	prefixBudget  = 50
	fpSuffixLen   = 12
	tokenUIDLen   = 16
	tokenFPLen    = 32
)

// CASName 生成 CAS 证书名：sanitize(crName)[:50] + "_" + fingerprint[:12]。
// CAS 文档只承诺接受字母、数字、下划线，且账号内唯一、≤ 63 字符；
// 指纹后缀保证同一 CR 的每一代名字不同、同一代重试名字相同。
func CASName(crName, fingerprint string) string {
	prefix := sanitize(crName)
	if len(prefix) > prefixBudget {
		prefix = prefix[:prefixBudget]
	}
	prefix = strings.TrimRight(prefix, "_")
	if prefix == "" {
		prefix = "cert"
	}
	suffix := fingerprint
	if len(suffix) > fpSuffixLen {
		suffix = suffix[:fpSuffixLen]
	}
	name := prefix + "_" + suffix
	if len(name) > maxCASNameLen {
		name = name[:maxCASNameLen]
	}
	return name
}

func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ClientToken 生成 CAS 幂等令牌：uid 去连字符前 16 位 + fingerprint 前 32 位，共 48 个 hex 字符。
// 同一 CR 的同一代证书无论重试多少次 token 都相同。
func ClientToken(uid types.UID, fingerprint string) string {
	u := strings.ReplaceAll(string(uid), "-", "")
	if len(u) > tokenUIDLen {
		u = u[:tokenUIDLen]
	}
	f := fingerprint
	if len(f) > tokenFPLen {
		f = f[:tokenFPLen]
	}
	return u + f
}
```

- [ ] **Step 4: 运行测试**

```bash
go test ./pkg/naming/... -v 2>&1 | tail -8
```

Expected: 5 个 PASS。

- [ ] **Step 5: 提交**

```bash
git add pkg/naming
git commit -m "feat(naming): CAS certificate name and idempotency token"
```

---

### Task 7: `pkg/aliyun` — CAS 窄接口、错误分类、出站限流、fake

**Files:**
- Create: `pkg/aliyun/cas.go`
- Create: `pkg/aliyun/errors.go`
- Create: `pkg/aliyun/ratelimit.go`
- Create: `pkg/aliyun/fake/cas.go`
- Create: `pkg/aliyun/errors_test.go`
- Create: `pkg/aliyun/ratelimit_test.go`
- Create: `pkg/aliyun/fake/cas_test.go`

**Interfaces:**
- Produces：
  - `aliyun.CASClient` 接口：`Upload(ctx, name string, certPEM, keyPEM []byte, clientToken string) (int64, error)`、`Delete(ctx, certID int64, clientToken string) error`、`FindUploaded(ctx, domainHint string) ([]CertSummary, error)`
  - `aliyun.CertSummary{CertID int64; Name string; SANs []string; EndDate string}`
  - `aliyun.ErrClass`：`ClassRetryable`、`ClassAuth`、`ClassNotFound`、`ClassPermanent`
  - `aliyun.Error{Class ErrClass; Op, Code string; Err error}`；`aliyun.Classify(op string, err error) error`；`aliyun.ClassOf(err error) ErrClass`（非 `*Error` → `ClassPermanent`）
  - `aliyun.Limiters`：`NewLimiters() *Limiters`、`(l *Limiters) Wait(ctx, key string, kind LimitKind) error`；`LimitKind`：`LimitCASList`（8 QPS/burst 1）、`LimitCASWrite`（50 QPS/burst 10）
  - `fake.CAS`：`fake.NewCAS() *CAS`，实现 `aliyun.CASClient`；字段 `UploadCalls, DeleteCalls, FindCalls int`；方法 `QueueUploadErr(err)`、`QueueDeleteErr(err)`、`QueueFindErr(err)`、`FailNextUploadAfterCommit(err)`、`Certs() []Cert`、`Has(certID int64) bool`；`fake.Cert{ID int64; Name string; CertPEM, KeyPEM []byte; Token string}`；哨兵 `fake.ErrDuplicateName`（`*aliyun.Error`，Permanent，Code `CertNameDuplicated`）

- [ ] **Step 1: 写接口与错误分类（无测试的纯类型先写）**

`pkg/aliyun/cas.go`：

```go
// Package aliyun 在阿里云 SDK 之上提供窄接口，便于 fake 与错误分类。
package aliyun

import "context"

// CertSummary 是 ListUserCertificateOrder 返回项的裁剪。
type CertSummary struct {
	CertID  int64
	Name    string
	SANs    []string
	EndDate string // YYYY-MM-DD，仅展示，不用于判断过期
}

// CASClient 是本项目对数字证书管理服务的全部依赖。
type CASClient interface {
	// Upload 上传 PEM 证书与私钥，返回 CertId。clientToken 用于幂等。
	Upload(ctx context.Context, name string, certPEM, keyPEM []byte, clientToken string) (int64, error)
	// Delete 删除证书。不存在时返回 ClassNotFound 错误。
	Delete(ctx context.Context, certID int64, clientToken string) error
	// FindUploaded 以域名为 Keyword 列出已上传证书（CAS 不支持按 Name 查）。
	FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error)
}
```

`pkg/aliyun/errors.go`：

```go
package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/alibabacloud-go/tea/dara"
)

// ErrClass 决定 controller 如何处置错误。
type ErrClass int

const (
	ClassPermanent ErrClass = iota // 不重试，置 condition，等 spec / 环境变化
	ClassRetryable                 // 指数退避重试
	ClassAuth                      // 凭证 / 权限问题，长 requeue
	ClassNotFound                  // 资源不存在（Delete 时视为成功）
)

func (c ErrClass) String() string {
	switch c {
	case ClassRetryable:
		return "Retryable"
	case ClassAuth:
		return "Auth"
	case ClassNotFound:
		return "NotFound"
	default:
		return "Permanent"
	}
}

// Error 是分类过的阿里云错误。绝不携带 request / response body。
type Error struct {
	Class ErrClass
	Op    string
	Code  string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("aliyun %s: %s (%s): %v", e.Op, e.Code, e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf 返回错误分类；非本包错误一律 Permanent。
func ClassOf(err error) ErrClass {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassPermanent
}

// Classify 把 SDK / 网络错误包装成 *Error。nil 原样返回。
func Classify(op string, err error) error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return err
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Class: ClassRetryable, Op: op, Code: "Timeout", Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Class: ClassRetryable, Op: op, Code: "NetTimeout", Err: err}
	}

	var sdkErr *dara.SDKError
	if errors.As(err, &sdkErr) {
		code := ""
		if sdkErr.Code != nil {
			code = *sdkErr.Code
		}
		status := 0
		if sdkErr.StatusCode != nil {
			status = *sdkErr.StatusCode
		}
		// 只保留 Code 与 StatusCode，丢弃 Message/Data 以免回显请求体
		safe := fmt.Errorf("sdk error code=%s status=%d", code, status)
		return &Error{Class: classifyCode(code, status), Op: op, Code: code, Err: safe}
	}

	return &Error{Class: ClassPermanent, Op: op, Code: "Unknown", Err: err}
}

func classifyCode(code string, status int) ErrClass {
	switch {
	case strings.HasPrefix(code, "Throttling"),
		code == "ServiceUnavailable",
		code == "InternalError",
		strings.HasPrefix(code, "ServiceUnavailable"),
		status >= 500:
		return ClassRetryable
	case strings.HasPrefix(code, "InvalidAccessKeyId"),
		code == "SignatureDoesNotMatch",
		strings.HasPrefix(code, "Forbidden"),
		strings.HasPrefix(code, "NoPermission"),
		code == "InvalidSecurityToken.Expired",
		status == 401, status == 403:
		return ClassAuth
	case strings.Contains(code, "NotExist"),
		strings.Contains(code, "NotFound"),
		status == 404:
		return ClassNotFound
	default:
		return ClassPermanent
	}
}
```

`pkg/aliyun/ratelimit.go`：

```go
package aliyun

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// LimitKind 区分不同 QPS 上限的调用通道。
type LimitKind int

const (
	// LimitCASList 对应 ListUserCertificateOrder（官方 QPS 10），留余量取 8。
	LimitCASList LimitKind = iota
	// LimitCASWrite 对应 Upload / Delete（官方 QPS 100），取 50。
	LimitCASWrite
)

func newLimiter(k LimitKind) *rate.Limiter {
	switch k {
	case LimitCASList:
		return rate.NewLimiter(rate.Limit(8), 1)
	default:
		return rate.NewLimiter(rate.Limit(50), 10)
	}
}

// Limiters 按 (key, kind) 维护限流器。key 通常是 accessKeyId，因为阿里云限流按账号计。
type Limiters struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

func NewLimiters() *Limiters { return &Limiters{m: map[string]*rate.Limiter{}} }

// Wait 阻塞直到获得配额或 ctx 结束。
func (l *Limiters) Wait(ctx context.Context, key string, kind LimitKind) error {
	l.mu.Lock()
	id := key + "/" + kindName(kind)
	lim, ok := l.m[id]
	if !ok {
		lim = newLimiter(kind)
		l.m[id] = lim
	}
	l.mu.Unlock()
	return lim.Wait(ctx)
}

func kindName(k LimitKind) string {
	if k == LimitCASList {
		return "cas-list"
	}
	return "cas-write"
}
```

- [ ] **Step 2: 写错误分类与限流测试**

`pkg/aliyun/errors_test.go`：

```go
package aliyun_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alibabacloud-go/tea/dara"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func sdkErr(code string, status int) error {
	c, s := code, status
	return &dara.SDKError{Code: &c, StatusCode: &s, Message: dara.String("secret body must not leak")}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want aliyun.ErrClass
	}{
		{"throttling", sdkErr("Throttling.User", 400), aliyun.ClassRetryable},
		{"5xx", sdkErr("Whatever", 503), aliyun.ClassRetryable},
		{"internal", sdkErr("InternalError", 500), aliyun.ClassRetryable},
		{"bad ak", sdkErr("InvalidAccessKeyId.NotFound", 404), aliyun.ClassAuth},
		{"forbidden", sdkErr("Forbidden.RAM", 403), aliyun.ClassAuth},
		{"not exist", sdkErr("CertNotExist", 400), aliyun.ClassNotFound},
		{"param", sdkErr("InvalidParameter", 400), aliyun.ClassPermanent},
		{"ctx deadline", context.DeadlineExceeded, aliyun.ClassRetryable},
		{"plain", errors.New("boom"), aliyun.ClassPermanent},
	}
	for _, c := range cases {
		got := aliyun.Classify("Upload", c.err)
		if aliyun.ClassOf(got) != c.want {
			t.Errorf("%s: class = %v, want %v (%v)", c.name, aliyun.ClassOf(got), c.want, got)
		}
	}
}

func TestClassify_DoesNotLeakMessage(t *testing.T) {
	got := aliyun.Classify("Upload", sdkErr("InvalidParameter", 400))
	if s := got.Error(); strings.Contains(s, "secret body") {
		t.Errorf("错误文本不应包含 SDK Message: %s", s)
	}
}

func TestClassify_Idempotent(t *testing.T) {
	first := aliyun.Classify("Upload", sdkErr("Throttling", 400))
	second := aliyun.Classify("Retry", first)
	if first != second {
		t.Errorf("已分类错误不应被重新包装")
	}
}
```

（import 增加 `"strings"`。）

`pkg/aliyun/ratelimit_test.go`：

```go
package aliyun_test

import (
	"context"
	"testing"
	"time"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func TestLimiters_ListChannelIsSlow(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx, "ak1", aliyun.LimitCASList); err != nil {
			t.Fatal(err)
		}
	}
	// 8 QPS、burst 1：第 2、3 次各需等 ~125ms
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("3 次 list 调用应至少耗时 ~250ms，实际 %v", el)
	}
}

func TestLimiters_KeysAreIndependent(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx := context.Background()
	_ = l.Wait(ctx, "ak1", aliyun.LimitCASList)
	start := time.Now()
	if err := l.Wait(ctx, "ak2", aliyun.LimitCASList); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("不同 key 不应互相等待，实际等待 %v", el)
	}
}

func TestLimiters_RespectsContext(t *testing.T) {
	l := aliyun.NewLimiters()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_ = l.Wait(ctx, "ak", aliyun.LimitCASList)
	if err := l.Wait(ctx, "ak", aliyun.LimitCASList); err == nil {
		t.Errorf("ctx 超时后 Wait 应返回错误")
	}
}
```

- [ ] **Step 3: 写 fake 与其测试**

`pkg/aliyun/fake/cas.go`：

```go
// Package fake 提供内存版 CASClient，支持错误注入与「服务端已成功但响应丢失」场景。
package fake

import (
	"context"
	"errors"
	"strings"
	"sync"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// ErrDuplicateName 模拟 CAS 的同名拒绝。
var ErrDuplicateName = &aliyun.Error{Class: aliyun.ClassPermanent, Op: "Upload", Code: "CertNameDuplicated", Err: errors.New("name already exists")}

// Cert 是 fake 中的一张证书。
type Cert struct {
	ID      int64
	Name    string
	CertPEM []byte
	KeyPEM  []byte
	Token   string
}

// CAS 实现 aliyun.CASClient。所有导出字段读取前请先调用 Snapshot 或在单 goroutine 中使用。
type CAS struct {
	mu sync.Mutex

	nextID  int64
	certs   map[int64]Cert
	byToken map[string]int64

	uploadErrs []error
	deleteErrs []error
	findErrs   []error

	failAfterCommit error

	UploadCalls int
	DeleteCalls int
	FindCalls   int
}

func NewCAS() *CAS {
	return &CAS{nextID: 1000, certs: map[int64]Cert{}, byToken: map[string]int64{}}
}

// QueueUploadErr 让下一次 Upload 在写入前返回该错误。
func (f *CAS) QueueUploadErr(err error) { f.mu.Lock(); f.uploadErrs = append(f.uploadErrs, err); f.mu.Unlock() }
func (f *CAS) QueueDeleteErr(err error) { f.mu.Lock(); f.deleteErrs = append(f.deleteErrs, err); f.mu.Unlock() }
func (f *CAS) QueueFindErr(err error)   { f.mu.Lock(); f.findErrs = append(f.findErrs, err); f.mu.Unlock() }

// FailNextUploadAfterCommit 让下一次 Upload 先写入（服务端成功）再返回 err（响应丢失）。
func (f *CAS) FailNextUploadAfterCommit(err error) { f.mu.Lock(); f.failAfterCommit = err; f.mu.Unlock() }

func pop(q *[]error) error {
	if len(*q) == 0 {
		return nil
	}
	e := (*q)[0]
	*q = (*q)[1:]
	return e
}

func (f *CAS) Upload(_ context.Context, name string, certPEM, keyPEM []byte, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UploadCalls++
	if err := pop(&f.uploadErrs); err != nil {
		return 0, err
	}
	// ClientToken 幂等：同 token 返回同一 ID
	if id, ok := f.byToken[token]; ok && token != "" {
		return id, nil
	}
	for _, c := range f.certs {
		if c.Name == name {
			return 0, ErrDuplicateName
		}
	}
	f.nextID++
	id := f.nextID
	f.certs[id] = Cert{ID: id, Name: name, CertPEM: append([]byte(nil), certPEM...), KeyPEM: append([]byte(nil), keyPEM...), Token: token}
	if token != "" {
		f.byToken[token] = id
	}
	if f.failAfterCommit != nil {
		err := f.failAfterCommit
		f.failAfterCommit = nil
		return 0, err
	}
	return id, nil
}

func (f *CAS) Delete(_ context.Context, certID int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCalls++
	if err := pop(&f.deleteErrs); err != nil {
		return err
	}
	if _, ok := f.certs[certID]; !ok {
		return &aliyun.Error{Class: aliyun.ClassNotFound, Op: "Delete", Code: "CertNotExist", Err: errors.New("not found")}
	}
	delete(f.certs, certID)
	return nil
}

func (f *CAS) FindUploaded(_ context.Context, domainHint string) ([]aliyun.CertSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FindCalls++
	if err := pop(&f.findErrs); err != nil {
		return nil, err
	}
	var out []aliyun.CertSummary
	for _, c := range f.certs {
		// fake 不解析 PEM；用名字前缀近似 Keyword 匹配域名的行为
		if domainHint == "" || strings.Contains(c.Name, sanitizeHint(domainHint)) {
			out = append(out, aliyun.CertSummary{CertID: c.ID, Name: c.Name})
		}
	}
	return out, nil
}

func sanitizeHint(s string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(strings.SplitN(s, ".", 2)[0])
}

// Has 判断 certID 是否存在。
func (f *CAS) Has(id int64) bool { f.mu.Lock(); defer f.mu.Unlock(); _, ok := f.certs[id]; return ok }

// Certs 返回快照。
func (f *CAS) Certs() []Cert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Cert, 0, len(f.certs))
	for _, c := range f.certs {
		out = append(out, c)
	}
	return out
}

var _ aliyun.CASClient = (*CAS)(nil)
```

`pkg/aliyun/fake/cas_test.go`：

```go
package fake_test

import (
	"context"
	"errors"
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func TestFake_TokenIdempotent(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	id1, err := f.Upload(ctx, "a_1", []byte("c"), []byte("k"), "tok")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.Upload(ctx, "a_1", []byte("c"), []byte("k"), "tok")
	if err != nil || id1 != id2 {
		t.Fatalf("同 token 应返回同 ID: %d %d %v", id1, id2, err)
	}
	if len(f.Certs()) != 1 {
		t.Errorf("只应有 1 张证书")
	}
}

func TestFake_DuplicateName(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	_, _ = f.Upload(ctx, "dup", nil, nil, "t1")
	_, err := f.Upload(ctx, "dup", nil, nil, "t2")
	if !errors.Is(err, fake.ErrDuplicateName) {
		t.Fatalf("want ErrDuplicateName, got %v", err)
	}
}

func TestFake_FailAfterCommit(t *testing.T) {
	f := fake.NewCAS()
	ctx := context.Background()
	f.FailNextUploadAfterCommit(&aliyun.Error{Class: aliyun.ClassRetryable, Code: "Timeout"})
	_, err := f.Upload(ctx, "x", nil, nil, "tok")
	if err == nil {
		t.Fatal("首次应返回错误")
	}
	if len(f.Certs()) != 1 {
		t.Fatal("服务端应已写入")
	}
	id, err := f.Upload(ctx, "x", nil, nil, "tok")
	if err != nil || id != f.Certs()[0].ID {
		t.Fatalf("重试应命中 token 返回同一 ID: %d %v", id, err)
	}
}

func TestFake_DeleteNotFound(t *testing.T) {
	f := fake.NewCAS()
	err := f.Delete(context.Background(), 42, "")
	if aliyun.ClassOf(err) != aliyun.ClassNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}
```

- [ ] **Step 4: 运行测试**

```bash
go test ./pkg/aliyun/... -v 2>&1 | tail -25
```

Expected: 全部 PASS（限流测试约耗时 0.3s）。

- [ ] **Step 5: 提交**

```bash
git add pkg/aliyun
git commit -m "feat(aliyun): CAS client interface, error classification, rate limiting and fake"
```

---

### Task 8: `pkg/aliyun` — 凭证、真实 CAS client、client 缓存

**Files:**
- Create: `pkg/aliyun/credentials.go`
- Create: `pkg/aliyun/cas_sdk.go`
- Create: `pkg/aliyun/cache.go`
- Create: `pkg/aliyun/credentials_test.go`
- Create: `pkg/aliyun/cache_test.go`

**Interfaces:**
- Produces：
  - `aliyun.Credentials{AccessKeyID, AccessKeySecret, SecurityToken, RoleARN, OIDCProviderARN, OIDCTokenFilePath string}`
  - `aliyun.CredentialsFromSecret(s *corev1.Secret) (*Credentials, error)`；`(c *Credentials) Build() (credential.Credential, error)`；`(c *Credentials) LimiterKey() string`（= AccessKeyID，或 RoleARN）
  - `aliyun.CASClientConfig{Region, Endpoint, ResourceGroupID string; Timeout time.Duration; Limiters *Limiters; LimiterKey string}`
  - `aliyun.NewCASClient(cred credential.Credential, cfg CASClientConfig) (CASClient, error)`
  - `aliyun.ClientCache`：`NewClientCache() *ClientCache`；`(c *ClientCache) GetOrBuild(key ClientKey, build func() (CASClient, error)) (CASClient, error)`；`ClientKey{Namespace, Name, ResourceVersion, Region, Endpoint string}`
  - 常量：Secret 字段名 `KeyAccessKeyID = "accessKeyId"` 等；`SecretTypeAliyunCredentials = "certs.bestheme.ac.cn/aliyun-credentials"`

- [ ] **Step 1: 写凭证与缓存的失败测试**

`pkg/aliyun/credentials_test.go`：

```go
package aliyun_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

func secretWith(data map[string]string) *corev1.Secret {
	s := &corev1.Secret{Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestCredentialsFromSecret_AccessKey(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyAccessKeyID: "LTAI123", aliyun.KeyAccessKeySecret: "sec",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "LTAI123" || c.AccessKeySecret != "sec" {
		t.Errorf("解析错误: %+v", c)
	}
	if c.LimiterKey() != "LTAI123" {
		t.Errorf("LimiterKey 应为 accessKeyId")
	}
	cred, err := c.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cred == nil {
		t.Fatal("credential 为 nil")
	}
}

func TestCredentialsFromSecret_TrimsWhitespace(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyAccessKeyID: " LTAI123\n", aliyun.KeyAccessKeySecret: "sec\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "LTAI123" || c.AccessKeySecret != "sec" {
		t.Errorf("应去除首尾空白: %+v", c)
	}
}

func TestCredentialsFromSecret_MissingSecretKey(t *testing.T) {
	_, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{aliyun.KeyAccessKeyID: "LTAI123"}))
	if err == nil {
		t.Fatal("缺 accessKeySecret 应报错")
	}
}

func TestCredentialsFromSecret_OIDC(t *testing.T) {
	c, err := aliyun.CredentialsFromSecret(secretWith(map[string]string{
		aliyun.KeyRoleARN: "acs:ram::123:role/x", aliyun.KeyOIDCProviderARN: "acs:ram::123:oidc-provider/y", aliyun.KeyOIDCTokenFilePath: "/var/run/token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.LimiterKey() != "acs:ram::123:role/x" {
		t.Errorf("OIDC 模式 LimiterKey 应为 RoleARN")
	}
}

func TestCredentialsFromSecret_Empty(t *testing.T) {
	if _, err := aliyun.CredentialsFromSecret(secretWith(nil)); err == nil {
		t.Fatal("空 Secret 应报错")
	}
}
```

`pkg/aliyun/cache_test.go`：

```go
package aliyun_test

import (
	"testing"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"
)

func TestClientCache_ReusesSameResourceVersion(t *testing.T) {
	c := aliyun.NewClientCache()
	builds := 0
	build := func() (aliyun.CASClient, error) { builds++; return fake.NewCAS(), nil }
	k := aliyun.ClientKey{Namespace: "ns", Name: "s", ResourceVersion: "1", Region: "cn-hangzhou"}
	a, _ := c.GetOrBuild(k, build)
	b, _ := c.GetOrBuild(k, build)
	if a != b || builds != 1 {
		t.Fatalf("同 key 应复用，builds=%d", builds)
	}
}

func TestClientCache_RebuildsOnResourceVersionChange(t *testing.T) {
	c := aliyun.NewClientCache()
	builds := 0
	build := func() (aliyun.CASClient, error) { builds++; return fake.NewCAS(), nil }
	k1 := aliyun.ClientKey{Namespace: "ns", Name: "s", ResourceVersion: "1", Region: "cn-hangzhou"}
	k2 := k1
	k2.ResourceVersion = "2"
	a, _ := c.GetOrBuild(k1, build)
	b, _ := c.GetOrBuild(k2, build)
	if a == b || builds != 2 {
		t.Fatalf("resourceVersion 变化必须重建，builds=%d", builds)
	}
	// 旧版本应被逐出，避免无限增长
	if c.Len() != 1 {
		t.Errorf("同 namespace/name 只保留最新一个，实际 %d", c.Len())
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./pkg/aliyun/ 2>&1 | head -5
```

- [ ] **Step 3: 实现凭证**

`pkg/aliyun/credentials.go`：

```go
package aliyun

import (
	"errors"
	"fmt"
	"strings"

	credential "github.com/aliyun/credentials-go/credentials"
	corev1 "k8s.io/api/core/v1"
)

// Secret 契约（见 spec §4.3）。
const (
	SecretTypeAliyunCredentials corev1.SecretType = "certs.bestheme.ac.cn/aliyun-credentials"

	KeyAccessKeyID       = "accessKeyId"
	KeyAccessKeySecret   = "accessKeySecret"
	KeySecurityToken     = "securityToken"
	KeyRoleARN           = "roleArn"
	KeyOIDCProviderARN   = "oidcProviderArn"
	KeyOIDCTokenFilePath = "oidcTokenFilePath"
)

// Credentials 是从 Secret 读出的凭证材料。不要把它写进日志。
type Credentials struct {
	AccessKeyID       string
	AccessKeySecret   string
	SecurityToken     string
	RoleARN           string
	OIDCProviderARN   string
	OIDCTokenFilePath string
}

// CredentialsFromSecret 解析并校验凭证 Secret。
func CredentialsFromSecret(s *corev1.Secret) (*Credentials, error) {
	get := func(k string) string { return strings.TrimSpace(string(s.Data[k])) }
	c := &Credentials{
		AccessKeyID:       get(KeyAccessKeyID),
		AccessKeySecret:   get(KeyAccessKeySecret),
		SecurityToken:     get(KeySecurityToken),
		RoleARN:           get(KeyRoleARN),
		OIDCProviderARN:   get(KeyOIDCProviderARN),
		OIDCTokenFilePath: get(KeyOIDCTokenFilePath),
	}
	switch {
	case c.isOIDC():
		return c, nil
	case c.AccessKeyID != "" && c.AccessKeySecret != "":
		return c, nil
	case c.AccessKeyID != "" || c.AccessKeySecret != "":
		return nil, fmt.Errorf("凭证 Secret 必须同时包含 %s 与 %s", KeyAccessKeyID, KeyAccessKeySecret)
	default:
		return nil, errors.New("凭证 Secret 为空：需要 accessKeyId/accessKeySecret 或 roleArn/oidcProviderArn/oidcTokenFilePath")
	}
}

func (c *Credentials) isOIDC() bool {
	return c.RoleARN != "" && c.OIDCProviderARN != "" && c.OIDCTokenFilePath != ""
}

// LimiterKey 是限流器分桶键：阿里云按账号限流，用 AK 或角色 ARN 近似账号。
func (c *Credentials) LimiterKey() string {
	if c.isOIDC() {
		return c.RoleARN
	}
	return c.AccessKeyID
}

// Build 构造 credentials-go 的 Credential。RRSA 第一版只预留，不在文档中承诺。
func (c *Credentials) Build() (credential.Credential, error) {
	cfg := new(credential.Config)
	switch {
	case c.isOIDC():
		cfg.SetType("oidc_role_arn").
			SetRoleArn(c.RoleARN).
			SetOIDCProviderArn(c.OIDCProviderARN).
			SetOIDCTokenFilePath(c.OIDCTokenFilePath)
	case c.SecurityToken != "":
		cfg.SetType("sts").
			SetAccessKeyId(c.AccessKeyID).
			SetAccessKeySecret(c.AccessKeySecret).
			SetSecurityToken(c.SecurityToken)
	default:
		cfg.SetType("access_key").
			SetAccessKeyId(c.AccessKeyID).
			SetAccessKeySecret(c.AccessKeySecret)
	}
	return credential.NewCredential(cfg)
}
```

若 `credential.Config` 的 setter 方法名与上面不一致（`go build` 会立刻暴露），以 `go doc github.com/aliyun/credentials-go/credentials Config` 的输出为准改名，不要改语义。

- [ ] **Step 4: 实现真实 CAS client**

`pkg/aliyun/cas_sdk.go`：

```go
package aliyun

import (
	"context"
	"errors"
	"strings"
	"time"

	cas "github.com/alibabacloud-go/cas-20200407/v4/client"
	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/tea/dara"
	credential "github.com/aliyun/credentials-go/credentials"
)

// CASClientConfig 是构造真实 client 的参数。
type CASClientConfig struct {
	Region          string
	Endpoint        string // 非空时覆盖 SDK 的 region → endpoint 映射
	ResourceGroupID string
	Timeout         time.Duration
	Limiters        *Limiters
	LimiterKey      string
}

type sdkCAS struct {
	c   *cas.Client
	cfg CASClientConfig
}

// NewCASClient 用 SDK v4 构造 CASClient。所有调用走 WithContext 方法并受限流器约束。
func NewCASClient(cred credential.Credential, cfg CASClientConfig) (CASClient, error) {
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
	c, err := cas.NewClient(oc)
	if err != nil {
		return nil, Classify("NewClient", err)
	}
	return &sdkCAS{c: c, cfg: cfg}, nil
}

func (s *sdkCAS) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

func (s *sdkCAS) Upload(ctx context.Context, name string, certPEM, keyPEM []byte, clientToken string) (int64, error) {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASWrite); err != nil {
		return 0, Classify("Upload", err)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	req := &cas.UploadUserCertificateRequest{
		Name:        dara.String(name),
		Cert:        dara.String(string(certPEM)),
		Key:         dara.String(string(keyPEM)),
		ClientToken: dara.String(clientToken),
	}
	if s.cfg.ResourceGroupID != "" {
		req.ResourceGroupId = dara.String(s.cfg.ResourceGroupID)
	}
	resp, err := s.c.UploadUserCertificateWithContext(ctx, req, &dara.RuntimeOptions{})
	if err != nil {
		return 0, Classify("Upload", err)
	}
	if resp == nil || resp.Body == nil || resp.Body.CertId == nil {
		return 0, &Error{Class: ClassRetryable, Op: "Upload", Code: "EmptyResponse", Err: errors.New("响应缺少 CertId")}
	}
	return *resp.Body.CertId, nil
}

func (s *sdkCAS) Delete(ctx context.Context, certID int64, clientToken string) error {
	if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASWrite); err != nil {
		return Classify("Delete", err)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	id := certID
	req := &cas.DeleteUserCertificateRequest{CertId: &id}
	if clientToken != "" {
		req.ClientToken = dara.String(clientToken)
	}
	_, err := s.c.DeleteUserCertificateWithContext(ctx, req, &dara.RuntimeOptions{})
	return Classify("Delete", err)
}

func (s *sdkCAS) FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error) {
	var out []CertSummary
	var page int64 = 1
	const size int64 = 50
	for {
		if err := s.cfg.Limiters.Wait(ctx, s.cfg.LimiterKey, LimitCASList); err != nil {
			return nil, Classify("ListUserCertificateOrder", err)
		}
		cctx, cancel := s.withTimeout(ctx)
		p, sz := page, size
		req := &cas.ListUserCertificateOrderRequest{
			OrderType:   dara.String("UPLOAD"),
			Keyword:     dara.String(domainHint),
			CurrentPage: &p,
			ShowSize:    &sz,
		}
		resp, err := s.c.ListUserCertificateOrderWithContext(cctx, req, &dara.RuntimeOptions{})
		cancel()
		if err != nil {
			return nil, Classify("ListUserCertificateOrder", err)
		}
		if resp == nil || resp.Body == nil {
			break
		}
		for _, it := range resp.Body.CertificateOrderList {
			if it == nil || it.CertificateId == nil {
				continue
			}
			cs := CertSummary{CertID: *it.CertificateId}
			if it.Name != nil {
				cs.Name = *it.Name
			}
			if it.Sans != nil {
				cs.SANs = strings.Split(*it.Sans, ",")
			}
			if it.EndDate != nil {
				cs.EndDate = *it.EndDate
			}
			out = append(out, cs)
		}
		total := int64(0)
		if resp.Body.TotalCount != nil {
			total = *resp.Body.TotalCount
		}
		if page*size >= total || len(resp.Body.CertificateOrderList) == 0 {
			break
		}
		page++
	}
	return out, nil
}

var _ CASClient = (*sdkCAS)(nil)
```

- [ ] **Step 5: 实现 client 缓存**

`pkg/aliyun/cache.go`：

```go
package aliyun

import "sync"

// ClientKey 决定 client 何时必须重建：凭证 Secret 的 resourceVersion 一变就重建，
// 否则轮换后的 AK 直到 Pod 重启才生效。
type ClientKey struct {
	Namespace       string
	Name            string
	ResourceVersion string
	Region          string
	Endpoint        string
}

func (k ClientKey) identity() string { return k.Namespace + "/" + k.Name + "/" + k.Region + "/" + k.Endpoint }

// ClientCache 每个 (namespace, name, region, endpoint) 只保留最新 resourceVersion 的 client。
type ClientCache struct {
	mu sync.Mutex
	m  map[string]entry
}

type entry struct {
	rv     string
	client CASClient
}

func NewClientCache() *ClientCache { return &ClientCache{m: map[string]entry{}} }

// GetOrBuild 返回缓存 client，或用 build 构造并替换旧版本。
func (c *ClientCache) GetOrBuild(key ClientKey, build func() (CASClient, error)) (CASClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := key.identity()
	if e, ok := c.m[id]; ok && e.rv == key.ResourceVersion {
		return e.client, nil
	}
	cl, err := build()
	if err != nil {
		return nil, err
	}
	c.m[id] = entry{rv: key.ResourceVersion, client: cl}
	return cl, nil
}

// Len 返回缓存条目数（测试用）。
func (c *ClientCache) Len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.m) }
```

- [ ] **Step 6: 运行测试与构建**

```bash
go build ./... && go test ./pkg/aliyun/... 2>&1 | tail -10
```

Expected: 编译通过（这一步同时验证 SDK v4 字段名 `ClientToken`、`CertId *int64`、`WithContext` 方法签名与本计划一致）；测试全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add pkg/aliyun
git commit -m "feat(aliyun): credentials loader, SDK-backed CAS client and client cache"
```

---

### Task 9: `internal/controller/issuer.go` — issuerRef 解析与 Pin

**Files:**
- Create: `internal/controller/issuer.go`
- Create: `internal/controller/issuer_test.go`（纯单元测试，`package controller`，不依赖 envtest）

**Interfaces:**
- Produces：
  - `controller.IssuerDefaults{Name, Kind, Group string}`；`(d IssuerDefaults) Ref() *cmmeta.IssuerReference`（Name 为空返回 nil）
  - `controller.IssuerSource`：`IssuerSourceSpec`、`IssuerSourceStatus`、`IssuerSourceExisting`、`IssuerSourceDefault`
  - `controller.ResolveIssuerRef(tpl *v1alpha1.CertificateTemplate, status *v1alpha1.AliyunCertificateStatus, existing *cmapi.Certificate, defaults IssuerDefaults) (cmmeta.IssuerReference, IssuerSource, bool)`
  - `controller.NormalizeIssuerRef(r cmmeta.IssuerReference) cmmeta.IssuerReference`（Kind 缺省 `Issuer`，Group 缺省 `cert-manager.io`）
  - `controller.IssuerRefEqual(a, b cmmeta.IssuerReference) bool`

优先级（spec §5.3）：spec > status（Pin）> 已存在的 cmapi.Certificate（bootstrap）> flag 默认。

- [ ] **Step 1: 写失败测试**

`internal/controller/issuer_test.go`：

```go
package controller

import (
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func ref(name, kind string) cmmeta.IssuerReference {
	return cmmeta.IssuerReference{Name: name, Kind: kind, Group: "cert-manager.io"}
}

func TestResolveIssuerRef_Precedence(t *testing.T) {
	defaults := IssuerDefaults{Name: "flag", Kind: "ClusterIssuer", Group: "cert-manager.io"}
	specRef := ref("spec", "ClusterIssuer")
	statusRef := ref("pinned", "ClusterIssuer")
	existing := &cmapi.Certificate{Spec: cmapi.CertificateSpec{IssuerRef: ref("existing", "Issuer")}}

	cases := []struct {
		name     string
		tpl      *certsv1alpha1.CertificateTemplate
		status   *certsv1alpha1.AliyunCertificateStatus
		existing *cmapi.Certificate
		defaults IssuerDefaults
		want     string
		source   IssuerSource
		ok       bool
	}{
		{"spec 优先", &certsv1alpha1.CertificateTemplate{IssuerRef: &specRef}, &certsv1alpha1.AliyunCertificateStatus{EffectiveIssuerRef: &statusRef}, existing, defaults, "spec", IssuerSourceSpec, true},
		{"status 其次", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{EffectiveIssuerRef: &statusRef}, existing, defaults, "pinned", IssuerSourceStatus, true},
		{"existing 再次", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, existing, defaults, "existing", IssuerSourceExisting, true},
		{"flag 兜底", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, nil, defaults, "flag", IssuerSourceDefault, true},
		{"全无", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, nil, IssuerDefaults{}, "", IssuerSourceDefault, false},
		{"existing 无 issuerRef 时跳到 flag", &certsv1alpha1.CertificateTemplate{}, &certsv1alpha1.AliyunCertificateStatus{}, &cmapi.Certificate{}, defaults, "flag", IssuerSourceDefault, true},
	}
	for _, c := range cases {
		got, src, ok := ResolveIssuerRef(c.tpl, c.status, c.existing, c.defaults)
		if ok != c.ok || got.Name != c.want || src != c.source {
			t.Errorf("%s: got (%q,%s,%v) want (%q,%s,%v)", c.name, got.Name, src, ok, c.want, c.source, c.ok)
		}
	}
}

func TestResolveIssuerRef_NormalizesDefaults(t *testing.T) {
	tpl := &certsv1alpha1.CertificateTemplate{IssuerRef: &cmmeta.IssuerReference{Name: "x"}}
	got, _, _ := ResolveIssuerRef(tpl, &certsv1alpha1.AliyunCertificateStatus{}, nil, IssuerDefaults{})
	if got.Kind != "Issuer" || got.Group != "cert-manager.io" {
		t.Errorf("应补全 Kind/Group 默认值: %+v", got)
	}
}

func TestIssuerRefEqual(t *testing.T) {
	a := cmmeta.IssuerReference{Name: "x"}
	b := cmmeta.IssuerReference{Name: "x", Kind: "Issuer", Group: "cert-manager.io"}
	if !IssuerRefEqual(a, b) {
		t.Errorf("缺省值补全后应相等")
	}
	if IssuerRefEqual(a, cmmeta.IssuerReference{Name: "x", Kind: "ClusterIssuer"}) {
		t.Errorf("Kind 不同不应相等")
	}
}

func TestIssuerDefaults_Ref(t *testing.T) {
	if (IssuerDefaults{}).Ref() != nil {
		t.Errorf("空默认应返回 nil")
	}
	r := (IssuerDefaults{Name: "le"}).Ref()
	if r == nil || r.Kind != "Issuer" || r.Group != "cert-manager.io" {
		t.Errorf("应补全默认 Kind/Group: %+v", r)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/controller/ -run 'TestResolveIssuerRef|TestIssuerRefEqual|TestIssuerDefaults' 2>&1 | head -5
```

Expected: `undefined: IssuerDefaults`。

- [ ] **Step 3: 实现**

`internal/controller/issuer.go`：

```go
package controller

import (
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

const (
	defaultIssuerKind  = "Issuer"
	defaultIssuerGroup = "cert-manager.io"
)

// IssuerDefaults 来自 --default-issuer-* flag，与 cert-manager 同名同语义。
type IssuerDefaults struct {
	Name  string
	Kind  string
	Group string
}

// Ref 把 flag 转成 IssuerReference；未配置 Name 时返回 nil。
func (d IssuerDefaults) Ref() *cmmeta.IssuerReference {
	if d.Name == "" {
		return nil
	}
	r := NormalizeIssuerRef(cmmeta.IssuerReference{Name: d.Name, Kind: d.Kind, Group: d.Group})
	return &r
}

// IssuerSource 记录 issuer 从哪一层解析出来，用于日志与 Diverged 判断。
type IssuerSource string

const (
	IssuerSourceSpec     IssuerSource = "spec"
	IssuerSourceStatus   IssuerSource = "status"
	IssuerSourceExisting IssuerSource = "existing"
	IssuerSourceDefault  IssuerSource = "default"
)

// NormalizeIssuerRef 补全 cert-manager 的缺省值，便于比较。
func NormalizeIssuerRef(r cmmeta.IssuerReference) cmmeta.IssuerReference {
	if r.Kind == "" {
		r.Kind = defaultIssuerKind
	}
	if r.Group == "" {
		r.Group = defaultIssuerGroup
	}
	return r
}

// IssuerRefEqual 在补全缺省值后比较。
func IssuerRefEqual(a, b cmmeta.IssuerReference) bool {
	a, b = NormalizeIssuerRef(a), NormalizeIssuerRef(b)
	return a.Name == b.Name && a.Kind == b.Kind && a.Group == b.Group
}

// ResolveIssuerRef 按 spec > status(Pin) > existing(bootstrap) > flag 的顺序解析。
//
// Pin 的关键在于：只要 status.effectiveIssuerRef 已存在，就以它为期望态，flag 不再参与——
// 否则改一次 flag 会让所有未显式指定 issuerRef 的证书同时重签。
// bootstrap 的关键在于：status 为空但 Certificate 已存在时，先采纳 Certificate 里的值，
// 防止「升级 operator 顺手改了 flag」触发全量重签。
func ResolveIssuerRef(
	tpl *certsv1alpha1.CertificateTemplate,
	status *certsv1alpha1.AliyunCertificateStatus,
	existing *cmapi.Certificate,
	defaults IssuerDefaults,
) (cmmeta.IssuerReference, IssuerSource, bool) {
	if tpl != nil && tpl.IssuerRef != nil && tpl.IssuerRef.Name != "" {
		return NormalizeIssuerRef(*tpl.IssuerRef), IssuerSourceSpec, true
	}
	if status != nil && status.EffectiveIssuerRef != nil && status.EffectiveIssuerRef.Name != "" {
		return NormalizeIssuerRef(*status.EffectiveIssuerRef), IssuerSourceStatus, true
	}
	if existing != nil && existing.Spec.IssuerRef.Name != "" {
		return NormalizeIssuerRef(existing.Spec.IssuerRef), IssuerSourceExisting, true
	}
	if d := defaults.Ref(); d != nil {
		return *d, IssuerSourceDefault, true
	}
	return cmmeta.IssuerReference{}, IssuerSourceDefault, false
}
```

- [ ] **Step 4: 运行测试**

```bash
go test ./internal/controller/ -run 'TestResolveIssuerRef|TestIssuerRefEqual|TestIssuerDefaults' -v 2>&1 | tail -8
```

Expected: PASS。（注意：`internal/controller` 包里 Ginkgo 的 suite 也会被 `go test` 触发；用 `-run` 只跑这几个即可。）

- [ ] **Step 5: 提交**

```bash
git add internal/controller/issuer.go internal/controller/issuer_test.go
git commit -m "feat(controller): issuerRef resolution with pin and bootstrap semantics"
```

---

### Task 10: Reconcile 骨架 — finalizer、期望态 Certificate、status 镜像、envtest 接线

**Files:**
- Modify: `internal/controller/aliyuncertificate_controller.go`（整体替换脚手架）
- Create: `internal/controller/desired.go`
- Create: `internal/controller/status.go`
- Create: `internal/controller/indexes.go`
- Modify: `internal/controller/suite_test.go`（启动 manager、注入 fake CAS）
- Create: `internal/controller/reconcile_basic_test.go`

**Interfaces:**
- Produces（后续任务在此结构体与函数上扩展）：
  - `controller.AliyunCertificateReconciler{Client client.Client; APIReader client.Reader; Scheme *runtime.Scheme; Recorder record.EventRecorder; CASFactory CASFactory; Now func() time.Time; ResyncInterval, CASProbeInterval, IssuanceStallThreshold, CleanupGracePeriod time.Duration; CleanupFailurePolicy string}`；方法 `SetIssuerDefaults(IssuerDefaults)`、`issuerDefaults() IssuerDefaults`、`now() time.Time`、`SetupWithManager(mgr) error`
  - `controller.CASFactory func(ctx context.Context, ac *v1alpha1.AliyunCertificate) (aliyun.CASClient, error)`
  - `controller.RegisterIndexes(mgr ctrl.Manager) error`（只注册一次；main 与 suite 调用）
  - `desired.go`：`secretNameFor(ac) string`、`certManagerNameFor(ac) string`、`desiredCertificateSpec(ac, issuer cmmeta.IssuerReference) cmapi.CertificateSpec`
  - `status.go`：`setCondition(ac, condType string, status metav1.ConditionStatus, reason, message string)`、`condTrue(ac, condType) bool`、`(r) patchStatus(ctx, ac, orig) error`、`mirrorIssuance(ac, cert)`、`certCondition(cert, t) *cmapi.CertificateCondition`、`certReady(cert) bool`、`certIssuingSince(cert) (bool, time.Time)`
  - 常量 `CleanupPolicyAbandon = "Abandon"`、`CleanupPolicyBlock = "Block"`
  - 测试 suite 全局：`k8sClient client.Client`、`reconciler *AliyunCertificateReconciler`、`fakeCAS *fake.CAS`（每个 `BeforeEach` 可重置）、helper `newNamespace(ctx) string`、`eventually(fn func() bool)`

- [ ] **Step 1: 期望态与 status 辅助（纯函数，先写）**

`internal/controller/desired.go`：

```go
package controller

import (
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// secretNameFor 返回 cert-manager 写入的 Secret 名。
func secretNameFor(ac *certsv1alpha1.AliyunCertificate) string {
	if ac.Spec.SecretName != "" {
		return ac.Spec.SecretName
	}
	return ac.Name + "-tls"
}

// certManagerNameFor 返回 operator 创建的 cmapi.Certificate 名（与 CR 同名）。
func certManagerNameFor(ac *certsv1alpha1.AliyunCertificate) string { return ac.Name }

// desiredCertificateSpec 从 CertificateTemplate 构造 cert-manager 的期望态。
// issuer 已由 ResolveIssuerRef 决定（含 Pin），这里只做透传与两处默认：PKCS1、managed label。
func desiredCertificateSpec(ac *certsv1alpha1.AliyunCertificate, issuer cmmeta.IssuerReference) cmapi.CertificateSpec {
	t := ac.Spec.CertificateTemplate
	spec := cmapi.CertificateSpec{
		SecretName:  secretNameFor(ac),
		IssuerRef:   NormalizeIssuerRef(issuer),
		CommonName:  t.CommonName,
		DNSNames:    append([]string(nil), t.DNSNames...),
		IPAddresses: append([]string(nil), t.IPAddresses...),
		Duration:    t.Duration.DeepCopy(),
		RenewBefore: t.RenewBefore.DeepCopy(),
		Subject:     t.Subject.DeepCopy(),
		Usages:      append([]cmapi.KeyUsage(nil), t.Usages...),
	}

	pk := &cmapi.CertificatePrivateKey{}
	if t.PrivateKey != nil {
		pk = t.PrivateKey.DeepCopy()
	}
	if pk.Encoding == "" {
		pk.Encoding = cmapi.PKCS1
	}
	spec.PrivateKey = pk

	st := &cmapi.CertificateSecretTemplate{}
	if t.SecretTemplate != nil {
		st = t.SecretTemplate.DeepCopy()
	}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	st.Labels[certsv1alpha1.LabelManaged] = "true"
	spec.SecretTemplate = st
	return spec
}
```

`metav1.Duration` 没有 `DeepCopy` 方法时（取决于 apimachinery 版本），把 `t.Duration.DeepCopy()` 换成下面的辅助函数：

```go
func copyDuration(d *metav1.Duration) *metav1.Duration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}
```

`internal/controller/status.go`：

```go
package controller

import (
	"context"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// setCondition 写入 condition，observedGeneration 取 CR 当前 generation。
func setCondition(ac *certsv1alpha1.AliyunCertificate, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ac.Generation,
	})
}

func condTrue(ac *certsv1alpha1.AliyunCertificate, condType string) bool {
	return meta.IsStatusConditionTrue(ac.Status.Conditions, condType)
}

// patchStatus 用 MergeFrom 补丁提交 status，避免整对象 Update 的冲突。
func (r *AliyunCertificateReconciler) patchStatus(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate) error {
	return r.Status().Patch(ctx, ac, client.MergeFrom(orig))
}

// mirrorIssuance 把 cert-manager Certificate 的关键 status 镜像到本 CR。
func mirrorIssuance(ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) {
	ac.Status.Issuance = &certsv1alpha1.IssuanceStatus{
		Revision:               cert.Status.Revision,
		RenewalTime:            cert.Status.RenewalTime,
		FailedIssuanceAttempts: cert.Status.FailedIssuanceAttempts,
		LastFailureTime:        cert.Status.LastFailureTime,
	}
}

func certCondition(cert *cmapi.Certificate, t cmapi.CertificateConditionType) *cmapi.CertificateCondition {
	for i := range cert.Status.Conditions {
		if cert.Status.Conditions[i].Type == t {
			return &cert.Status.Conditions[i]
		}
	}
	return nil
}

func certReady(cert *cmapi.Certificate) bool {
	c := certCondition(cert, cmapi.CertificateConditionReady)
	return c != nil && c.Status == cmmeta.ConditionTrue
}

// certIssuingSince 返回 Issuing 是否为 True 及其起始时间。
func certIssuingSince(cert *cmapi.Certificate) (bool, time.Time) {
	c := certCondition(cert, cmapi.CertificateConditionIssuing)
	if c == nil || c.Status != cmmeta.ConditionTrue {
		return false, time.Time{}
	}
	if c.LastTransitionTime == nil {
		return true, time.Time{}
	}
	return true, c.LastTransitionTime.Time
}
```

`internal/controller/indexes.go`：

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
	return mgr.GetFieldIndexer().IndexField(context.Background(), &certsv1alpha1.AliyunCertificateBinding{},
		certsv1alpha1.IndexBindingByCertificate, func(o client.Object) []string {
			b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
			if !ok || b.Spec.CertificateRef.Name == "" {
				return nil
			}
			return []string{b.Spec.CertificateRef.Name}
		})
}
```

- [ ] **Step 2: 写 Reconciler 骨架**

整体替换 `internal/controller/aliyuncertificate_controller.go`：

```go
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
)

// CleanupFailurePolicy 取值。
const (
	CleanupPolicyAbandon = "Abandon"
	CleanupPolicyBlock   = "Block"
)

// CASFactory 按 CR 构造 CAS client。生产实现读凭证 Secret；测试注入 fake。
type CASFactory func(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error)

// AliyunCertificateReconciler 实现 spec §5 的证书 controller。
type AliyunCertificateReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder

	CASFactory CASFactory
	Now        func() time.Time

	ResyncInterval         time.Duration
	CASProbeInterval       time.Duration
	IssuanceStallThreshold time.Duration
	CleanupGracePeriod     time.Duration
	CleanupFailurePolicy   string

	mu       sync.RWMutex
	defaults IssuerDefaults
}

// SetIssuerDefaults 线程安全地设置 flag 默认（测试中会在运行时修改以验证 Pin）。
func (r *AliyunCertificateReconciler) SetIssuerDefaults(d IssuerDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults = d
}

func (r *AliyunCertificateReconciler) issuerDefaults() IssuerDefaults {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaults
}

func (r *AliyunCertificateReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificates/finalizers,verbs=update
// +kubebuilder:rbac:groups=certs.bestheme.ac.cn,resources=aliyuncertificatebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile 实现 spec §5.2 的步骤。
func (r *AliyunCertificateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ac := &certsv1alpha1.AliyunCertificate{}
	if err := r.Get(ctx, req.NamespacedName, ac); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := ac.DeepCopy()

	// 0. 删除分支（Task 13 实现完整清理；此处先保证 finalizer 可被摘除）
	if !ac.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ac, orig)
	}

	if !controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(ac, certsv1alpha1.FinalizerName)
		if err := r.Update(ctx, ac); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	ac.Status.ObservedGeneration = ac.Generation

	// 1. 解析 issuer
	existing := &cmapi.Certificate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: certManagerNameFor(ac)}, existing)
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return ctrl.Result{}, err
	}
	issuer, source, ok := ResolveIssuerRef(&ac.Spec.CertificateTemplate, &ac.Status, existing, r.issuerDefaults())
	if !ok {
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonNoIssuer,
			"spec.certificateTemplate.issuerRef 未指定，且 operator 未配置 --default-issuer-name")
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
	}
	ac.Status.EffectiveIssuerRef = &issuer
	log.V(1).Info("issuer resolved", "name", issuer.Name, "kind", issuer.Kind, "source", source)

	// 2. 首次创建前检查 Secret 名冲突
	if existing == nil {
		conflict, err := r.secretNameConflict(ctx, ac)
		if err != nil {
			return ctrl.Result{}, err
		}
		if conflict {
			setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonSecretNameConflict,
				fmt.Sprintf("Secret %q 已存在且不属于本证书", secretNameFor(ac)))
			return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
		}
	}

	// 3. CreateOrUpdate cert-manager Certificate（只有 spec 真变了才会发出 Update）
	cert := &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: certManagerNameFor(ac), Namespace: ac.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.Spec = desiredCertificateSpec(ac, issuer)
		if cert.Labels == nil {
			cert.Labels = map[string]string{}
		}
		cert.Labels[certsv1alpha1.LabelManaged] = "true"
		return controllerutil.SetControllerReference(ac, cert, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("同步 cert-manager Certificate 失败: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("cert-manager Certificate synced", "operation", op)
	}
	ac.Status.CertManagerCertificateName = cert.Name
	ac.Status.SecretName = cert.Spec.SecretName

	// 4. 镜像 cert-manager status；未 Ready 则等 watch
	mirrorIssuance(ac, cert)
	if issuing, since := certIssuingSince(cert); issuing && !since.IsZero() && r.now().Sub(since) > r.IssuanceStallThreshold {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonIssuanceStalled,
			fmt.Sprintf("cert-manager Issuing 已持续 %s", r.now().Sub(since).Truncate(time.Minute)))
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonIssuanceStalled, "cert-manager issuance stalled")
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonIssuanceStalled, "issuance stalled")
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
	if !certReady(cert) {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, certsv1alpha1.ReasonCertificateNotReady, "等待 cert-manager 签发")
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonCertificateNotReady, "等待 cert-manager 签发")
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig)
	}

	// 5–10 由 Task 11 / 12 / 14 接入
	return r.reconcileIssued(ctx, ac, orig, cert)
}

// reconcileIssued 是 Certificate Ready 之后的流程（Task 11 起替换本实现）。
func (r *AliyunCertificateReconciler) reconcileIssued(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, _ *cmapi.Certificate) (ctrl.Result, error) {
	setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "cert-manager Certificate Ready")
	setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "")
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
}

// reconcileDelete 是删除分支（Task 13 替换本实现）。
func (r *AliyunCertificateReconciler) reconcileDelete(ctx context.Context, ac, _ *certsv1alpha1.AliyunCertificate) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) {
		controllerutil.RemoveFinalizer(ac, certsv1alpha1.FinalizerName)
		return ctrl.Result{}, r.Update(ctx, ac)
	}
	return ctrl.Result{}, nil
}

// secretNameConflict 判断目标 Secret 是否已被别人占用。
// Secret 由 manager client 直读 API server（cache 已对 Secret 禁用）。
func (r *AliyunCertificateReconciler) secretNameConflict(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (bool, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: secretNameFor(ac)}, s)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// cert-manager 会在它写的 Secret 上标注来源 Certificate；同名即视为我们的
	return s.Annotations["cert-manager.io/certificate-name"] != certManagerNameFor(ac), nil
}

// SetupWithManager 注册 watch：主资源、owned Certificate、以及引用本证书的 Binding。
func (r *AliyunCertificateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certsv1alpha1.AliyunCertificate{}).
		Owns(&cmapi.Certificate{}).
		Watches(&certsv1alpha1.AliyunCertificateBinding{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				b, ok := o.(*certsv1alpha1.AliyunCertificateBinding)
				if !ok || b.Spec.CertificateRef.Name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.CertificateRef.Name}}}
			})).
		Named("aliyuncertificate").
		Complete(r)
}
```

- [ ] **Step 3: 改 suite，启动 manager 并注入 fake**

在 `internal/controller/suite_test.go` 的全局变量区加入：

```go
var (
	reconciler *AliyunCertificateReconciler
	fakeCAS    *fake.CAS
	fakeMu     sync.Mutex
	nsCounter  int
)

// currentCAS 让每个测试可以替换 fakeCAS 而 manager 无需重启。
func currentCAS() *fake.CAS { fakeMu.Lock(); defer fakeMu.Unlock(); return fakeCAS }
func resetCAS() *fake.CAS   { fakeMu.Lock(); defer fakeMu.Unlock(); fakeCAS = fake.NewCAS(); return fakeCAS }

// newNamespace 为每个用例创建独立 namespace，避免资源名冲突。
func newNamespace(ctx context.Context) string {
	nsCounter++
	name := fmt.Sprintf("t%d-%d", GinkgoParallelProcess(), nsCounter)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

func eventually(fn func() bool) {
	EventuallyWithOffset(1, fn, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
}
```

在 `BeforeSuite` 创建 `k8sClient` 之后追加：

```go
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}}, // 与生产一致：Secret 不进 cache
		}},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(RegisterIndexes(k8sManager)).To(Succeed())

	resetCAS()
	reconciler = &AliyunCertificateReconciler{
		Client:                 k8sManager.GetClient(),
		APIReader:              k8sManager.GetAPIReader(),
		Scheme:                 k8sManager.GetScheme(),
		Recorder:               k8sManager.GetEventRecorderFor("aliyuncertificate"),
		CASFactory:             func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) { return currentCAS(), nil },
		ResyncInterval:         time.Hour,
		CASProbeInterval:       12 * time.Hour,
		IssuanceStallThreshold: 6 * time.Hour,
		CleanupGracePeriod:     15 * time.Minute,
		CleanupFailurePolicy:   CleanupPolicyAbandon,
	}
	reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
	Expect(reconciler.SetupWithManager(k8sManager)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(k8sManager.Start(ctx)).To(Succeed())
	}()
```

import 需要：`"fmt"`, `"sync"`, `"time"`, `corev1 "k8s.io/api/core/v1"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"`, `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"`, `"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun/fake"`。脚手架 suite 已有 `ctx, cancel = context.WithCancel(...)` 与 `AfterSuite` 里的 `cancel()`，保留。

- [ ] **Step 4: 写 reconcile 基础行为测试**

`internal/controller/reconcile_basic_test.go`：

```go
package controller

import (
	"context"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func baseAC(ns, name string) *certsv1alpha1.AliyunCertificate {
	return &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: []string{"api.example.com"}},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "aliyun"},
				Region:         "cn-hangzhou",
			},
		},
	}
}

func getAC(ctx context.Context, ns, name string) *certsv1alpha1.AliyunCertificate {
	ac := &certsv1alpha1.AliyunCertificate{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ac)).To(Succeed())
	return ac
}

func getCert(ctx context.Context, ns, name string) (*cmapi.Certificate, error) {
	c := &cmapi.Certificate{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, c)
	return c, err
}

func condReason(ac *certsv1alpha1.AliyunCertificate, t string) string {
	c := meta.FindStatusCondition(ac.Status.Conditions, t)
	if c == nil {
		return ""
	}
	return c.Reason
}

var _ = Describe("证书 controller：基础 reconcile", func() {
	ctx := context.Background()

	BeforeEach(func() {
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
	})

	It("创建 cert-manager Certificate、加 finalizer、固化 issuer", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "c1"))).To(Succeed())

		eventually(func() bool { _, err := getCert(ctx, ns, "c1"); return err == nil })
		cert, _ := getCert(ctx, ns, "c1")
		Expect(cert.Spec.SecretName).To(Equal("c1-tls"))
		Expect(cert.Spec.IssuerRef).To(Equal(cmmeta.IssuerReference{Name: "letsencrypt-prod", Kind: "ClusterIssuer", Group: "cert-manager.io"}))
		Expect(cert.Spec.DNSNames).To(Equal([]string{"api.example.com"}))
		Expect(cert.Spec.PrivateKey.Encoding).To(Equal(cmapi.PKCS1))
		Expect(cert.Spec.SecretTemplate.Labels).To(HaveKeyWithValue(certsv1alpha1.LabelManaged, "true"))
		Expect(cert.OwnerReferences).To(HaveLen(1))
		Expect(cert.OwnerReferences[0].Name).To(Equal("c1"))

		eventually(func() bool {
			ac := getAC(ctx, ns, "c1")
			return controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) &&
				ac.Status.EffectiveIssuerRef != nil && ac.Status.EffectiveIssuerRef.Name == "letsencrypt-prod" &&
				ac.Status.SecretName == "c1-tls" &&
				condReason(ac, certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonCertificateNotReady
		})
	})

	It("Pin：flag 变更后已存在证书的 issuerRef 不变", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "pin"))).To(Succeed())
		eventually(func() bool {
			ac := getAC(ctx, ns, "pin")
			return ac.Status.EffectiveIssuerRef != nil
		})

		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "zerossl", Kind: "ClusterIssuer"})
		// 触发一次 reconcile
		ac := getAC(ctx, ns, "pin")
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())

		Consistently(func() string {
			cert, err := getCert(ctx, ns, "pin")
			if err != nil {
				return ""
			}
			return cert.Spec.IssuerRef.Name
		}, "2s", "200ms").Should(Equal("letsencrypt-prod"))

		// 新建的 CR 用新默认
		Expect(k8sClient.Create(ctx, baseAC(ns, "fresh"))).To(Succeed())
		eventually(func() bool {
			cert, err := getCert(ctx, ns, "fresh")
			return err == nil && cert.Spec.IssuerRef.Name == "zerossl"
		})
	})

	It("spec.issuerRef 覆盖 flag 默认", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "explicit")
		ac.Spec.CertificateTemplate.IssuerRef = &cmmeta.IssuerReference{Name: "internal-ca"}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		eventually(func() bool {
			cert, err := getCert(ctx, ns, "explicit")
			return err == nil && cert.Spec.IssuerRef.Name == "internal-ca" && cert.Spec.IssuerRef.Kind == "Issuer"
		})
	})

	It("无 issuer 可用时 Ready=False/NoIssuer 且不创建 Certificate", func() {
		reconciler.SetIssuerDefaults(IssuerDefaults{})
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "noissuer"))).To(Succeed())
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "noissuer"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonNoIssuer
		})
		_, err := getCert(ctx, ns, "noissuer")
		Expect(err).To(HaveOccurred())
	})

	It("目标 Secret 已被占用时 Ready=False/SecretNameConflict", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "taken-tls", Namespace: ns},
			Data:       map[string][]byte{"foo": []byte("bar")},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, baseAC(ns, "taken"))).To(Succeed())
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "taken"), certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonSecretNameConflict
		})
		_, err := getCert(ctx, ns, "taken")
		Expect(err).To(HaveOccurred())
	})

	It("Certificate Ready 后 Issued=True（Secret 校验由 Task 11 收紧）", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "ready"))).To(Succeed())
		eventually(func() bool { _, err := getCert(ctx, ns, "ready"); return err == nil })
		cert, _ := getCert(ctx, ns, "ready")
		rev := 1
		cert.Status.Revision = &rev
		cert.Status.Conditions = []cmapi.CertificateCondition{{Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, LastTransitionTime: &metav1.Time{Time: metav1.Now().Time}}}
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())
		eventually(func() bool {
			ac := getAC(ctx, ns, "ready")
			return condTrue(ac, certsv1alpha1.ConditionIssued) && ac.Status.Issuance != nil && ac.Status.Issuance.Revision != nil && *ac.Status.Issuance.Revision == 1
		})
	})
})
```

- [ ] **Step 5: 运行**

```bash
make manifests && make test 2>&1 | tail -30
```

Expected: 编译通过、全部 PASS。`config/rbac/role.yaml` 中 secrets 只有 `get`、`delete`。

- [ ] **Step 6: 提交**

```bash
git add internal/controller config/rbac
git commit -m "feat(controller): reconcile skeleton with cert-manager Certificate sync, issuer pin and status mirroring"
```

---

### Task 11: Secret 校验、CAS 上传（write-ahead + ClientToken）、history 推进

**Files:**
- Create: `internal/controller/material.go`
- Create: `internal/controller/upload.go`
- Create: `internal/controller/cas_factory.go`
- Modify: `internal/controller/aliyuncertificate_controller.go`（替换 `reconcileIssued`）
- Create: `internal/controller/material_test.go`（纯单元）
- Create: `internal/controller/upload_test.go`（envtest）

**Interfaces:**
- Produces：
  - `material.go`：`loadMaterial(ctx, r client.Reader, ac, cert) (*pki.Bundle, *materialError)`；`type materialError struct{ Reason, Message string }`；规则函数 `validateMaterial(b *pki.Bundle, ac, cert) *materialError`
  - `upload.go`：`(r) ensureUploaded(ctx, ac, b *pki.Bundle) (changed bool, err error)`；`(r) casClient(ctx, ac) (aliyun.CASClient, error)`
  - `cas_factory.go`：`NewCASFactory(reader client.Reader, cache *aliyun.ClientCache, limiters *aliyun.Limiters, timeout time.Duration) CASFactory`（生产实现，main.go 使用）
  - condition 语义：`Issued=True` 仅当 Certificate Ready **且** Secret 通过全部校验；`Uploaded=True` 当 `status.current.certId != nil`（或 uploadToCAS=false 时 reason `UploadDisabled`）

- [ ] **Step 1: 校验规则的单元测试**

`internal/controller/material_test.go`：

```go
package controller

import (
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func acWithNames(names ...string) *certsv1alpha1.AliyunCertificate {
	return &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Spec:       certsv1alpha1.AliyunCertificateSpec{CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: names}},
	}
}

func certWithIssuing(issuing bool) *cmapi.Certificate {
	c := &cmapi.Certificate{}
	st := cmmeta.ConditionFalse
	if issuing {
		st = cmmeta.ConditionTrue
	}
	c.Status.Conditions = []cmapi.CertificateCondition{{Type: cmapi.CertificateConditionIssuing, Status: st}}
	return c
}

func TestValidateMaterial_OK(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	if me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(false)); me != nil {
		t.Fatalf("应通过，得到 %+v", me)
	}
}

func TestValidateMaterial_SANsMismatch(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	me := validateMaterial(b, acWithNames("api.example.com", "www.example.com"), certWithIssuing(false))
	if me == nil || me.Reason != certsv1alpha1.ReasonSANsMismatch {
		t.Fatalf("want SANsMismatch, got %+v", me)
	}
}

func TestValidateMaterial_TemporaryCertRejected(t *testing.T) {
	crt, key := testutil.SelfSigned(t, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(true))
	if me == nil || me.Reason != certsv1alpha1.ReasonSelfSignedDuringIssuance {
		t.Fatalf("自签 + Issuing=True 必须拒绝, got %+v", me)
	}
}

func TestValidateMaterial_SelfSignedIssuerAccepted(t *testing.T) {
	crt, key := testutil.SelfSigned(t, "api.example.com")
	b, _ := pki.ParseBundle(crt, key)
	if me := validateMaterial(b, acWithNames("api.example.com"), certWithIssuing(false)); me != nil {
		t.Fatalf("自签 + Issuing=False（SelfSigned issuer 正式证书）应接受, got %+v", me)
	}
}

func TestValidateMaterial_WildcardCoversRequired(t *testing.T) {
	ca := testutil.NewCA(t)
	crt, key := testutil.IssueLeaf(t, ca, "*.example.com")
	b, _ := pki.ParseBundle(crt, key)
	ac := acWithNames("*.example.com")
	ac.Spec.CertificateTemplate.CommonName = "api.example.com"
	if me := validateMaterial(b, ac, certWithIssuing(false)); me != nil {
		t.Fatalf("通配符应覆盖 commonName, got %+v", me)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/controller/ -run TestValidateMaterial 2>&1 | head -3
```

- [ ] **Step 3: 实现 material.go**

```go
package controller

import (
	"context"
	"fmt"
	"strings"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

// materialError 是可直接写进 condition 的校验失败。
type materialError struct {
	Reason  string
	Message string
}

// loadMaterial 直读 Secret（不经 cache），解析并校验。任何失败都不上传。
func loadMaterial(ctx context.Context, reader client.Reader, ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) (*pki.Bundle, *materialError) {
	s := &corev1.Secret{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: secretNameFor(ac)}, s)
	if apierrors.IsNotFound(err) {
		return nil, &materialError{certsv1alpha1.ReasonSecretNotFound, fmt.Sprintf("Secret %q 不存在", secretNameFor(ac))}
	}
	if err != nil {
		return nil, &materialError{certsv1alpha1.ReasonSecretInvalid, "读取 Secret 失败: " + err.Error()}
	}
	b, err := pki.ParseBundle(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		// pki 的错误不含密钥内容，可安全写入 message
		return nil, &materialError{certsv1alpha1.ReasonSecretInvalid, err.Error()}
	}
	if me := validateMaterial(b, ac, cert); me != nil {
		return nil, me
	}
	return b, nil
}

// validateMaterial 实施 spec §5.4 的规则 4–5（1–3 与 6 在 pki.ParseBundle 内完成）。
func validateMaterial(b *pki.Bundle, ac *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) *materialError {
	// 规则 5：临时证书 = 自签 AND cert-manager 正在签发。
	// 只用合取：SelfSigned issuer 的正式证书也是自签，但此时 Issuing=False，必须放行。
	if issuing, _ := certIssuingSince(cert); issuing && b.IsSelfSigned() {
		return &materialError{certsv1alpha1.ReasonSelfSignedDuringIssuance, "Secret 中是 cert-manager 的临时自签证书，等待正式签发"}
	}

	// 规则 4：leaf SANs 必须覆盖 spec 声明的全部域名。
	required := append([]string(nil), ac.Spec.CertificateTemplate.DNSNames...)
	if cn := ac.Spec.CertificateTemplate.CommonName; cn != "" {
		required = append(required, cn)
	}
	if missing := pki.Missing(b.DNSNames(), required); len(missing) > 0 {
		return &materialError{certsv1alpha1.ReasonSANsMismatch, "证书未覆盖: " + strings.Join(missing, ", ")}
	}
	return nil
}
```

- [ ] **Step 4: 实现 cas_factory.go（生产用）**

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
)

// credentialsError 让调用方区分「凭证 Secret 不存在」与「凭证内容无效」。
type credentialsError struct {
	Reason string
	Err    error
}

func (e *credentialsError) Error() string { return e.Err.Error() }

// NewCASFactory 构造生产用 CASFactory：读同 namespace 的凭证 Secret，按 resourceVersion 缓存 client。
func NewCASFactory(reader client.Reader, cache *aliyun.ClientCache, limiters *aliyun.Limiters, timeout time.Duration) CASFactory {
	return func(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
		s := &corev1.Secret{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: ac.Spec.Aliyun.CredentialsRef.Name}, s)
		if apierrors.IsNotFound(err) {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound, fmt.Errorf("凭证 Secret %q 不存在", ac.Spec.Aliyun.CredentialsRef.Name)}
		}
		if err != nil {
			return nil, err
		}
		creds, err := aliyun.CredentialsFromSecret(s)
		if err != nil {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
		}
		key := aliyun.ClientKey{
			Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion,
			Region: ac.Spec.Aliyun.EffectiveCASRegion(), Endpoint: ac.Spec.Aliyun.EndpointOverride,
		}
		return cache.GetOrBuild(key, func() (aliyun.CASClient, error) {
			cred, err := creds.Build()
			if err != nil {
				return nil, &credentialsError{certsv1alpha1.ReasonCredentialsInvalid, err}
			}
			return aliyun.NewCASClient(cred, aliyun.CASClientConfig{
				Region: key.Region, Endpoint: key.Endpoint, ResourceGroupID: ac.Spec.Aliyun.ResourceGroupID,
				Timeout: timeout, Limiters: limiters, LimiterKey: creds.LimiterKey(),
			})
		})
	}
}
```

- [ ] **Step 5: 实现 upload.go**

```go
package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki"
)

// ensureUploaded 保证 status.current 对应 b 的指纹；需要时上传 CAS。
// 返回 changed=true 表示 status.current 被推进（新代次）。
func (r *AliyunCertificateReconciler) ensureUploaded(ctx context.Context, ac *certsv1alpha1.AliyunCertificate, b *pki.Bundle) (bool, error) {
	log := logf.FromContext(ctx).WithValues("fingerprint", b.Fingerprint[:8])

	if ac.Status.Current != nil && ac.Status.Current.Fingerprint == b.Fingerprint {
		return false, nil // 指纹未变，短路
	}

	gen := certsv1alpha1.CertificateGeneration{
		Fingerprint: b.Fingerprint,
		NotBefore:   metav1.NewTime(b.Leaf.NotBefore),
		NotAfter:    metav1.NewTime(b.Leaf.NotAfter),
		UploadedAt:  metav1.NewTime(r.now()),
	}

	if ac.Spec.Aliyun.UploadEnabled() {
		casName := naming.CASName(ac.Name, b.Fingerprint)
		token := naming.ClientToken(ac.UID, b.Fingerprint)

		// write-ahead：先把意图写进 status，再调云；崩溃后重启能用同一 token 续上。
		if ac.Status.PendingUpload == nil || ac.Status.PendingUpload.Fingerprint != b.Fingerprint {
			ac.Status.PendingUpload = &certsv1alpha1.PendingUpload{
				Fingerprint: b.Fingerprint, CASName: casName, ClientToken: token, StartedAt: metav1.NewTime(r.now()),
			}
			// 立刻持久化 write-ahead（独立于本轮末尾的 patch）
			orig := ac.DeepCopy()
			orig.Status.PendingUpload = nil
			if err := r.patchStatus(ctx, ac, orig); err != nil {
				return false, err
			}
		}

		cas, err := r.casClient(ctx, ac)
		if err != nil {
			return false, err
		}
		certPEM := b.CertPEM()
		keyPEM, err := b.KeyPEM()
		if err != nil {
			return false, err
		}
		certID, err := cas.Upload(ctx, ac.Status.PendingUpload.CASName, certPEM, keyPEM, ac.Status.PendingUpload.ClientToken)
		if err != nil {
			// 同名已存在 = 之前上传成功但响应丢失且 token 未命中：兜底按域名查找
			if aliyun.ClassOf(err) == aliyun.ClassPermanent && isDuplicateName(err) {
				if found, ferr := r.findByName(ctx, cas, b, ac.Status.PendingUpload.CASName); ferr == nil && found != 0 {
					certID = found
					err = nil
				}
			}
			if err != nil {
				return false, err
			}
		}
		gen.CertID = &certID
		gen.CASName = ac.Status.PendingUpload.CASName
		ac.Status.PendingUpload = nil
		r.Recorder.Event(ac, corev1.EventTypeNormal, "Uploaded", "certificate uploaded to CAS")
		log.Info("uploaded to CAS", "certId", certID)
	}

	if ac.Status.Current != nil {
		ac.Status.History = append([]certsv1alpha1.CertificateGeneration{*ac.Status.Current}, ac.Status.History...)
	}
	ac.Status.Current = &gen
	return true, nil
}

// casClient 通过 CASFactory 获取 client；凭证错误转成 condition reason 由调用方处理。
func (r *AliyunCertificateReconciler) casClient(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
	if r.CASFactory == nil {
		return nil, errors.New("CASFactory 未配置")
	}
	return r.CASFactory(ctx, ac)
}

func isDuplicateName(err error) bool {
	var e *aliyun.Error
	if !errors.As(err, &e) {
		return false
	}
	// 阿里云真实错误码待实测确认；fake 使用 CertNameDuplicated
	return e.Code == "CertNameDuplicated" || e.Code == "DuplicateCertificateName" || e.Code == "CertNameExisted"
}

// findByName 用域名做 Keyword 拉取后按 Name 过滤（CAS 不支持按名查）。
func (r *AliyunCertificateReconciler) findByName(ctx context.Context, cas aliyun.CASClient, b *pki.Bundle, casName string) (int64, error) {
	hint := ""
	if names := b.DNSNames(); len(names) > 0 {
		hint = names[0]
	}
	list, err := cas.FindUploaded(ctx, hint)
	if err != nil {
		return 0, err
	}
	for _, c := range list {
		if c.Name == casName {
			return c.CertID, nil
		}
	}
	return 0, fmt.Errorf("CAS 中未找到名为 %s 的证书", casName)
}
```

- [ ] **Step 6: 把 `reconcileIssued` 替换为真正流程**

在 `aliyuncertificate_controller.go` 中替换 `reconcileIssued`：

```go
// reconcileIssued 处理 Certificate Ready 之后的步骤 5–10。
func (r *AliyunCertificateReconciler) reconcileIssued(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, cert *cmapi.Certificate) (ctrl.Result, error) {
	// 5. 读 Secret 并校验
	b, me := loadMaterial(ctx, r.Client, ac, cert)
	if me != nil {
		setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionFalse, me.Reason, me.Message)
		if me.Reason == certsv1alpha1.ReasonSelfSignedDuringIssuance {
			r.Recorder.Event(ac, corev1.EventTypeWarning, me.Reason, me.Message)
		}
		r.aggregateReady(ac)
		// 不清空 status.current：Secret 短暂异常不能被当成新代次
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
	setCondition(ac, certsv1alpha1.ConditionIssued, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "Secret 通过校验")

	// 6–7. 上传（含短路）
	if _, err := r.ensureUploaded(ctx, ac, b); err != nil {
		return r.handleCloudError(ctx, ac, orig, "Upload", err)
	}
	r.setUploadedCondition(ac)

	// 8–9. 回收与探测由 Task 12 / 14 接入
	r.aggregateReady(ac)
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
}

// setUploadedCondition 依据 status.current 与 uploadToCAS 设置 Uploaded。
func (r *AliyunCertificateReconciler) setUploadedCondition(ac *certsv1alpha1.AliyunCertificate) {
	switch {
	case !ac.Spec.Aliyun.UploadEnabled():
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadDisabled, "spec.aliyun.uploadToCAS=false")
	case ac.Status.Current != nil && ac.Status.Current.CertID != nil:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionTrue, certsv1alpha1.ReasonReady, fmt.Sprintf("certId=%d", *ac.Status.Current.CertID))
	default:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, "尚未上传")
	}
}

// aggregateReady：Ready = Issued && (Uploaded || !uploadToCAS)。IssuerDefaultDiverged 不参与。
func (r *AliyunCertificateReconciler) aggregateReady(ac *certsv1alpha1.AliyunCertificate) {
	issued := condTrue(ac, certsv1alpha1.ConditionIssued)
	uploadedOK := !ac.Spec.Aliyun.UploadEnabled() || condTrue(ac, certsv1alpha1.ConditionUploaded)
	if issued && uploadedOK {
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionTrue, certsv1alpha1.ReasonReady, "")
		return
	}
	reason := certsv1alpha1.ReasonCertificateNotReady
	if c := meta.FindStatusCondition(ac.Status.Conditions, certsv1alpha1.ConditionIssued); c != nil && c.Status != metav1.ConditionTrue {
		reason = c.Reason
	} else if c := meta.FindStatusCondition(ac.Status.Conditions, certsv1alpha1.ConditionUploaded); c != nil && c.Status != metav1.ConditionTrue {
		reason = c.Reason
	}
	setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "")
}

// handleCloudError 把阿里云 / 凭证错误映射为 condition 与 requeue 策略。
func (r *AliyunCertificateReconciler) handleCloudError(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate, op string, err error) (ctrl.Result, error) {
	var ce *credentialsError
	if errors.As(err, &ce) {
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, ce.Reason, ce.Error())
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, r.patchStatus(ctx, ac, orig)
	}
	switch aliyun.ClassOf(err) {
	case aliyun.ClassRetryable:
		reason := certsv1alpha1.ReasonUploadFailed
		var ae *aliyun.Error
		if errors.As(err, &ae) && strings.HasPrefix(ae.Code, "Throttling") {
			reason = certsv1alpha1.ReasonThrottled
		}
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, reason, op+" 失败，将重试")
		r.aggregateReady(ac)
		if perr := r.patchStatus(ctx, ac, orig); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err // 交给 controller-runtime 指数退避
	case aliyun.ClassAuth:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonCredentialsInvalid, op+" 被拒绝: "+err.Error())
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, r.patchStatus(ctx, ac, orig)
	default:
		setCondition(ac, certsv1alpha1.ConditionUploaded, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, op+" 失败: "+err.Error())
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonUploadFailed, op+" failed")
		r.aggregateReady(ac)
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, r.patchStatus(ctx, ac, orig)
	}
}
```

import 需追加 `"errors"`、`"strings"`、`"k8s.io/apimachinery/pkg/api/meta"`。同时把 Task 10 中 Reconcile 里步骤 4 的两处 `setCondition(ac, ConditionReady, ...)` 改为调用 `r.aggregateReady(ac)`。

- [ ] **Step 7: 写 envtest 上传测试**

`internal/controller/upload_test.go`：

```go
package controller

import (
	"context"
	"errors"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

// simulateIssuance 模拟 cert-manager：写 Secret，再把 Certificate 置为 Ready/revision。
func simulateIssuance(ctx context.Context, ns, name string, rev int, certPEM, keyPEM []byte) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: ns,
		Annotations: map[string]string{"cert-manager.io/certificate-name": name}}}
	_, err := ctrlCreateOrUpdateSecret(ctx, s, certPEM, keyPEM)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cert := &cmapi.Certificate{}
	EventuallyWithOffset(1, func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cert)
	}).Should(Succeed())
	r := rev
	cert.Status.Revision = &r
	now := metav1.Now()
	cert.Status.Conditions = []cmapi.CertificateCondition{
		{Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, LastTransitionTime: &now},
		{Type: cmapi.CertificateConditionIssuing, Status: cmmeta.ConditionFalse, LastTransitionTime: &now},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, cert)).To(Succeed())
}

func ctrlCreateOrUpdateSecret(ctx context.Context, s *corev1.Secret, certPEM, keyPEM []byte) (bool, error) {
	existing := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, existing)
	if err == nil {
		existing.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
		return false, k8sClient.Update(ctx, existing)
	}
	s.Type = corev1.SecretTypeTLS
	s.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
	return true, k8sClient.Create(ctx, s)
}

var _ = Describe("证书 controller：上传", func() {
	ctx := context.Background()
	var ca *testutil.CA

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		ca = testutil.NewCA(GinkgoT())
	})

	It("签发后上传一次，重复 reconcile 不重复上传", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "up"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "up", 1, crt, key)

		eventually(func() bool {
			ac := getAC(ctx, ns, "up")
			return condTrue(ac, certsv1alpha1.ConditionReady) && ac.Status.Current != nil && ac.Status.Current.CertID != nil
		})
		ac := getAC(ctx, ns, "up")
		Expect(ac.Status.Current.CASName).To(HavePrefix("up_"))
		Expect(ac.Status.PendingUpload).To(BeNil())
		Expect(currentCAS().UploadCalls).To(Equal(1))

		// 再触发一次 reconcile
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
		Consistently(func() int { return currentCAS().UploadCalls }, "2s", "200ms").Should(Equal(1))
	})

	It("续期：新指纹上传新证书，旧代次进 history", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "renew"))).To(Succeed())
		crt1, key1 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "renew", 1, crt1, key1)
		eventually(func() bool { ac := getAC(ctx, ns, "renew"); return ac.Status.Current != nil })
		first := getAC(ctx, ns, "renew").Status.Current.Fingerprint

		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "renew", 2, crt2, key2)
		eventually(func() bool {
			ac := getAC(ctx, ns, "renew")
			return ac.Status.Current != nil && ac.Status.Current.Fingerprint != first && len(ac.Status.History) == 1
		})
		ac := getAC(ctx, ns, "renew")
		Expect(ac.Status.History[0].Fingerprint).To(Equal(first))
		Expect(currentCAS().UploadCalls).To(Equal(2))
		Expect(len(currentCAS().Certs())).To(Equal(2))
	})

	It("uploadToCAS=false：不调 CAS，Ready 仍为 True", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "nocas")
		f := false
		ac.Spec.Aliyun.UploadToCAS = &f
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "nocas", 1, crt, key)
		eventually(func() bool {
			got := getAC(ctx, ns, "nocas")
			return condTrue(got, certsv1alpha1.ConditionReady) && got.Status.Current != nil && got.Status.Current.CertID == nil &&
				condReason(got, certsv1alpha1.ConditionUploaded) == certsv1alpha1.ReasonUploadDisabled
		})
		Expect(currentCAS().UploadCalls).To(Equal(0))
	})

	It("临时自签证书 + Issuing=True 不上传", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "tmp"))).To(Succeed())
		crt, key := testutil.SelfSigned(GinkgoT(), "api.example.com")
		simulateIssuance(ctx, ns, "tmp", 1, crt, key)
		// 把 Issuing 翻成 True
		cert := &cmapi.Certificate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tmp"}, cert)).To(Succeed())
		now := metav1.Now()
		cert.Status.Conditions[1] = cmapi.CertificateCondition{Type: cmapi.CertificateConditionIssuing, Status: cmmeta.ConditionTrue, LastTransitionTime: &now}
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())

		eventually(func() bool {
			return condReason(getAC(ctx, ns, "tmp"), certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonSelfSignedDuringIssuance
		})
		Consistently(func() int { return currentCAS().UploadCalls }, "1s", "200ms").Should(Equal(0))
	})

	It("SANs 不覆盖 spec.dnsNames 时不上传", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "sans")
		ac.Spec.CertificateTemplate.DNSNames = []string{"api.example.com", "www.example.com"}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "sans", 1, crt, key)
		eventually(func() bool {
			return condReason(getAC(ctx, ns, "sans"), certsv1alpha1.ConditionIssued) == certsv1alpha1.ReasonSANsMismatch
		})
		Expect(currentCAS().UploadCalls).To(Equal(0))
	})

	It("上传响应丢失后重试命中 ClientToken，不产生重复证书", func() {
		ns := newNamespace(ctx)
		currentCAS().FailNextUploadAfterCommit(&aliyun.Error{Class: aliyun.ClassRetryable, Op: "Upload", Code: "Timeout", Err: errors.New("timeout")})
		Expect(k8sClient.Create(ctx, baseAC(ns, "lost"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "lost", 1, crt, key)

		eventually(func() bool {
			ac := getAC(ctx, ns, "lost")
			return ac.Status.Current != nil && ac.Status.Current.CertID != nil
		})
		Expect(len(currentCAS().Certs())).To(Equal(1))
		Expect(currentCAS().UploadCalls).To(BeNumerically(">=", 2))
		Expect(getAC(ctx, ns, "lost").Status.PendingUpload).To(BeNil())
	})

	It("凭证 Secret 缺失时 Uploaded=False/CredentialsSecretNotFound", func() {
		ns := newNamespace(ctx)
		prev := reconciler.CASFactory
		reconciler.CASFactory = func(context.Context, *certsv1alpha1.AliyunCertificate) (aliyun.CASClient, error) {
			return nil, &credentialsError{certsv1alpha1.ReasonCredentialsNotFound, errors.New("missing")}
		}
		DeferCleanup(func() { reconciler.CASFactory = prev })

		Expect(k8sClient.Create(ctx, baseAC(ns, "nocred"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "nocred", 1, crt, key)
		eventually(func() bool {
			ac := getAC(ctx, ns, "nocred")
			return condReason(ac, certsv1alpha1.ConditionUploaded) == certsv1alpha1.ReasonCredentialsNotFound && !condTrue(ac, certsv1alpha1.ConditionReady)
		})
	})
})
```

- [ ] **Step 8: 运行**

```bash
make test 2>&1 | tail -30
```

Expected: 全部 PASS。若「响应丢失」用例中 `UploadCalls` 只有 1，说明错误没有触发 requeue——检查 `handleCloudError` 对 `ClassRetryable` 是否返回了 `err`。

- [ ] **Step 9: 提交**

```bash
git add internal/controller
git commit -m "feat(controller): validate secret material and upload to CAS with write-ahead idempotency"
```

---

### Task 12: 保留策略回收（三重护栏）

**Files:**
- Create: `internal/controller/retention.go`
- Modify: `internal/controller/aliyuncertificate_controller.go`（在 `reconcileIssued` 中接入）
- Create: `internal/controller/retention_test.go`（纯单元 + envtest 各一部分）

**Interfaces:**
- Produces：
  - `(r) reclaimOldGenerations(ctx, ac) error`：按 spec §5.7 回收，成功删除的代次从 `status.history` 移除；任一护栏不满足则跳过该代（不报错）
  - 纯函数 `reclaimable(gen CertificateGeneration, now time.Time, minAge time.Duration, bindings []AliyunCertificateBinding) (ok bool, why string)`
  - `(r) liveBindingsFor(ctx, ac) ([]AliyunCertificateBinding, error)`：用 `APIReader` + field index **不可用**（APIReader 不支持 field selector on CRD index）→ 改为 `APIReader.List` 全 namespace 后按 `spec.certificateRef.name` 过滤

- [ ] **Step 1: 纯函数测试**

`internal/controller/retention_test.go`（先写这部分）：

```go
package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

func binding(gen int64, observed int64, applied string) certsv1alpha1.AliyunCertificateBinding {
	return certsv1alpha1.AliyunCertificateBinding{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status:     certsv1alpha1.AliyunCertificateBindingStatus{ObservedGeneration: observed, AppliedFingerprint: applied},
	}
}

func TestReclaimable(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	old := certsv1alpha1.CertificateGeneration{Fingerprint: "old", UploadedAt: metav1.NewTime(now.Add(-48 * time.Hour))}
	young := certsv1alpha1.CertificateGeneration{Fingerprint: "young", UploadedAt: metav1.NewTime(now.Add(-1 * time.Hour))}

	cases := []struct {
		name     string
		gen      certsv1alpha1.CertificateGeneration
		bindings []certsv1alpha1.AliyunCertificateBinding
		want     bool
	}{
		{"无 Binding、已过 minAge", old, nil, true},
		{"未过 minAge", young, nil, false},
		{"有 Binding 仍在用该代", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 1, "old")}, false},
		{"Binding 已推进到别的代", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 1, "new")}, true},
		{"Binding 尚未完成首次 reconcile（observedGeneration 落后）", old, []certsv1alpha1.AliyunCertificateBinding{binding(2, 1, "new")}, false},
		{"Binding 刚创建 appliedFingerprint 为空且未 reconcile", old, []certsv1alpha1.AliyunCertificateBinding{binding(1, 0, "")}, false},
	}
	for _, c := range cases {
		ok, why := reclaimable(c.gen, now, 24*time.Hour, c.bindings)
		if ok != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, ok, why, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./internal/controller/ -run TestReclaimable 2>&1 | head -3
```

- [ ] **Step 3: 实现 retention.go**

```go
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

// reclaimable 判断某一代能否回收：三重护栏（spec §5.7）。
//  1. 年龄 ≥ minAge
//  2. 所有引用本证书的 Binding 都已完成对当前 generation 的 reconcile
//  3. 没有任何 Binding 的 appliedFingerprint 等于该代
func reclaimable(gen certsv1alpha1.CertificateGeneration, now time.Time, minAge time.Duration, bindings []certsv1alpha1.AliyunCertificateBinding) (bool, string) {
	if now.Sub(gen.UploadedAt.Time) < minAge {
		return false, "minAge 未到"
	}
	for i := range bindings {
		b := &bindings[i]
		if b.Status.ObservedGeneration < b.Generation {
			return false, fmt.Sprintf("Binding %s 尚未完成 reconcile", b.Name)
		}
		if b.Status.AppliedFingerprint == gen.Fingerprint {
			return false, fmt.Sprintf("Binding %s 仍在使用该代", b.Name)
		}
	}
	return true, ""
}

// liveBindingsFor 绕过 informer cache 直读引用本证书的 Binding（护栏 2/3 的 TOCTOU 防线）。
// APIReader 不支持自定义 field index，因此按 namespace 列出后客户端过滤。
func (r *AliyunCertificateReconciler) liveBindingsFor(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) ([]certsv1alpha1.AliyunCertificateBinding, error) {
	list := &certsv1alpha1.AliyunCertificateBindingList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(ac.Namespace)); err != nil {
		return nil, err
	}
	var out []certsv1alpha1.AliyunCertificateBinding
	for _, b := range list.Items {
		if b.Spec.CertificateRef.Name == ac.Name {
			out = append(out, b)
		}
	}
	return out, nil
}

// reclaimOldGenerations 让 current+history 的总数收敛到 keepLast。
// 从最老的一代开始尝试；护栏不满足就停在那一代（更新的代次年龄更小，也不会满足）。
func (r *AliyunCertificateReconciler) reclaimOldGenerations(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	if !ac.Spec.Aliyun.UploadEnabled() {
		return nil
	}
	keep := ac.Spec.Retention.KeepLastOrDefault()
	excess := 1 + len(ac.Status.History) - keep // current 占 1
	if excess <= 0 {
		return nil
	}
	log := logf.FromContext(ctx)

	bindings, err := r.liveBindingsFor(ctx, ac)
	if err != nil {
		return err
	}
	var cas aliyun.CASClient
	now := r.now()
	minAge := ac.Spec.Retention.MinAgeOrDefault()

	// history[0] 最新，末尾最老
	for excess > 0 && len(ac.Status.History) > 0 {
		oldest := ac.Status.History[len(ac.Status.History)-1]
		ok, why := reclaimable(oldest, now, minAge, bindings)
		if !ok {
			log.V(1).Info("skip reclaim", "fingerprint", oldest.Fingerprint[:8], "why", why)
			return nil
		}
		if oldest.CertID != nil {
			if cas == nil {
				if cas, err = r.casClient(ctx, ac); err != nil {
					return err
				}
			}
			token := naming.ClientToken(ac.UID, oldest.Fingerprint) + "d" // 与上传 token 区分
			if len(token) > 64 {
				token = token[:64]
			}
			err := cas.Delete(ctx, *oldest.CertID, token)
			if err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
				return err
			}
			r.Recorder.Event(ac, corev1.EventTypeNormal, "Reclaimed", "old CAS certificate reclaimed")
			log.Info("reclaimed CAS certificate", "certId", *oldest.CertID, "fingerprint", oldest.Fingerprint[:8])
		}
		ac.Status.History = ac.Status.History[:len(ac.Status.History)-1]
		excess--
	}
	return nil
}
```

- [ ] **Step 4: 接入 reconcileIssued**

在 `reconcileIssued` 的 `r.setUploadedCondition(ac)` 之后、`r.aggregateReady(ac)` 之前插入：

```go
	// 8. 保留策略回收（失败按云错误处理，不影响 Ready 判定）
	if err := r.reclaimOldGenerations(ctx, ac); err != nil {
		return r.handleCloudError(ctx, ac, orig, "Delete", err)
	}
```

- [ ] **Step 5: 追加 envtest 时序测试**

在 `internal/controller/retention_test.go` 末尾追加（同文件、同 package，Ginkgo 部分）：

```go
var _ = Describe("证书 controller：回收时序", func() {
	ctx := context.Background()
	var ca *testutil.CA
	var clock time.Time

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		ca = testutil.NewCA(GinkgoT())
		clock = time.Now()
		reconciler.Now = func() time.Time { return clock }
		DeferCleanup(func() { reconciler.Now = nil })
	})

	setBindingStatus := func(ns, name string, applied string) {
		b := &certsv1alpha1.AliyunCertificateBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, b)).To(Succeed())
		b.Status.ObservedGeneration = b.Generation
		b.Status.AppliedFingerprint = applied
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
	}

	touch := func(ns, name, v string) {
		ac := getAC(ctx, ns, name)
		if ac.Annotations == nil {
			ac.Annotations = map[string]string{}
		}
		ac.Annotations["touch"] = v
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
	}

	It("gen1 只有在所有 Binding 推进且过 minAge 后才被删", func() {
		ns := newNamespace(ctx)
		ac := baseAC(ns, "rot")
		ac.Spec.Retention.KeepLast = 1
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		for _, bn := range []string{"b1", "b2"} {
			Expect(k8sClient.Create(ctx, &certsv1alpha1.AliyunCertificateBinding{
				ObjectMeta: metav1.ObjectMeta{Name: bn, Namespace: ns},
				Spec: certsv1alpha1.AliyunCertificateBindingSpec{
					CertificateRef: certsv1alpha1.LocalObjectReference{Name: "rot"},
					Target: certsv1alpha1.BindingTarget{Type: certsv1alpha1.TargetTypeFC3CustomDomain,
						FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: bn + ".example.com"}},
				},
			})).To(Succeed())
		}

		crt1, key1 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 1, crt1, key1)
		eventually(func() bool { a := getAC(ctx, ns, "rot"); return a.Status.Current != nil && a.Status.Current.CertID != nil })
		gen1 := *getAC(ctx, ns, "rot").Status.Current
		setBindingStatus(ns, "b1", gen1.Fingerprint)
		setBindingStatus(ns, "b2", gen1.Fingerprint)

		// 续期 → gen2
		clock = clock.Add(30 * 24 * time.Hour)
		crt2, key2 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 2, crt2, key2)
		eventually(func() bool { a := getAC(ctx, ns, "rot"); return len(a.Status.History) == 1 })
		gen2 := *getAC(ctx, ns, "rot").Status.Current

		// 两个 Binding 都还在 gen1：不删
		touch(ns, "rot", "1")
		Consistently(func() bool { return currentCAS().Has(*gen1.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 一个推进：仍不删
		setBindingStatus(ns, "b1", gen2.Fingerprint)
		touch(ns, "rot", "2")
		Consistently(func() bool { return currentCAS().Has(*gen1.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 都推进，但 gen1 距离「本次上传」不满 minAge —— gen1.UploadedAt 是 30 天前，minAge 24h 已满足；
		// 用 gen2 的场景验证 minAge：先把 keepLast 保持 1，让 gen2 在下一轮成为 history 时受 minAge 保护
		setBindingStatus(ns, "b2", gen2.Fingerprint)
		touch(ns, "rot", "3")
		eventually(func() bool { return !currentCAS().Has(*gen1.CertID) })
		eventually(func() bool { return len(getAC(ctx, ns, "rot").Status.History) == 0 })

		// gen3 立刻到来：gen2 进入 history 但年龄 < minAge，不删
		crt3, key3 := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "rot", 3, crt3, key3)
		eventually(func() bool { a := getAC(ctx, ns, "rot"); return len(a.Status.History) == 1 })
		gen3 := *getAC(ctx, ns, "rot").Status.Current
		setBindingStatus(ns, "b1", gen3.Fingerprint)
		setBindingStatus(ns, "b2", gen3.Fingerprint)
		touch(ns, "rot", "4")
		Consistently(func() bool { return currentCAS().Has(*gen2.CertID) }, "1500ms", "200ms").Should(BeTrue())

		// 时钟越过 minAge → 删
		clock = clock.Add(25 * time.Hour)
		touch(ns, "rot", "5")
		eventually(func() bool { return !currentCAS().Has(*gen2.CertID) })
	})
})
```

需要 import：`"context"`、`. "github.com/onsi/ginkgo/v2"`、`. "github.com/onsi/gomega"`、`"k8s.io/apimachinery/pkg/types"`、`"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"`。注意 `reconciler.Now` 在这里被替换，`ensureUploaded` 的 `UploadedAt` 因此使用 fake 时钟——这正是 minAge 断言能成立的原因。

- [ ] **Step 6: 运行**

```bash
make test 2>&1 | tail -30
```

Expected: PASS。若 `Consistently` 阶段意外删除，优先检查 `liveBindingsFor` 是否真的用了 `APIReader`（用 cache 会读到旧 status）。

- [ ] **Step 7: 提交**

```bash
git add internal/controller
git commit -m "feat(controller): reclaim old CAS generations behind minAge and live binding guards"
```

---

### Task 13: 删除分支 — 阻塞判断、有界 CAS 清理、Certificate → 等 NotFound → Secret

**Files:**
- Create: `internal/controller/deletion.go`
- Modify: `internal/controller/aliyuncertificate_controller.go`（删除 Task 10 的临时 `reconcileDelete`）
- Modify: `api/v1alpha1/aliyuncertificate_types.go`（status 增加 `cleanupStartedAt`）
- Create: `internal/controller/deletion_test.go`

**Interfaces:**
- Produces：
  - `(r) reconcileDelete(ctx, ac, orig) (ctrl.Result, error)`，顺序：活着的 Binding → 阻塞；CAS 各代（有界）→ 显式删 Certificate → 等 NotFound → 删 Secret → 摘 finalizer
  - `status.cleanupStartedAt *metav1.Time`：首次进入删除分支时写入，用于 `--cleanup-grace-period` 计时
  - 纯函数 `activeBindingNames(bindings []AliyunCertificateBinding) []string`（只统计 `DeletionTimestamp == nil` 的）
  - 常量 `requeueDeletionWait = 2 * time.Second`

- [ ] **Step 1: status 增加字段**

在 `AliyunCertificateStatus` 中 `CASProbedAt` 之后加入：

```go
	// 首次进入删除分支的时间，用于 --cleanup-grace-period 计时。
	// +optional
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`
```

运行 `make generate manifests`。

- [ ] **Step 2: 写测试**

`internal/controller/deletion_test.go`：

```go
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

func TestActiveBindingNames(t *testing.T) {
	now := metav1.Now()
	bs := []certsv1alpha1.AliyunCertificateBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "alive"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "dying", DeletionTimestamp: &now}},
	}
	got := activeBindingNames(bs)
	if len(got) != 1 || got[0] != "alive" {
		t.Fatalf("正在删除中的 Binding 不应计入: %v", got)
	}
}

var _ = Describe("证书 controller：删除", func() {
	ctx := context.Background()
	var ca *testutil.CA

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		reconciler.CleanupFailurePolicy = CleanupPolicyAbandon
		reconciler.CleanupGracePeriod = 15 * time.Minute
		reconciler.Now = nil
		ca = testutil.NewCA(GinkgoT())
	})

	issuedAC := func(ns, name string) (*certsv1alpha1.AliyunCertificate, int64) {
		Expect(k8sClient.Create(ctx, baseAC(ns, name))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, name, 1, crt, key)
		eventually(func() bool { a := getAC(ctx, ns, name); return a.Status.Current != nil && a.Status.Current.CertID != nil })
		ac := getAC(ctx, ns, name)
		return ac, *ac.Status.Current.CertID
	}

	It("正常删除：CAS、Certificate、Secret 全部清理，finalizer 摘除", func() {
		ns := newNamespace(ctx)
		ac, certID := issuedAC(ns, "del")
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(currentCAS().Has(certID)).To(BeFalse())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del"}, &cmapi.Certificate{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "del-tls"}, &corev1.Secret{}))).To(BeTrue())
	})

	It("有活着的 Binding 引用时阻塞，Binding 删除后继续", func() {
		ns := newNamespace(ctx)
		ac, _ := issuedAC(ns, "blocked")
		b := &certsv1alpha1.AliyunCertificateBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns},
			Spec: certsv1alpha1.AliyunCertificateBindingSpec{
				CertificateRef: certsv1alpha1.LocalObjectReference{Name: "blocked"},
				Target: certsv1alpha1.BindingTarget{Type: certsv1alpha1.TargetTypeFC3CustomDomain,
					FC3CustomDomain: &certsv1alpha1.FC3CustomDomainTarget{Region: "cn-hangzhou", DomainName: "x.example.com"}},
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		eventually(func() bool {
			got := &certsv1alpha1.AliyunCertificate{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, got); err != nil {
				return false
			}
			return condReason(got, certsv1alpha1.ConditionReady) == certsv1alpha1.ReasonDeletionBlocked
		})
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "1500ms", "200ms").Should(BeTrue())

		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "blocked"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
	})

	It("CAS 持续失败：Abandon 策略在 grace period 后放弃并摘 finalizer", func() {
		ns := newNamespace(ctx)
		ac, certID := issuedAC(ns, "abandon")
		// 之后所有 Delete 都失败
		for i := 0; i < 50; i++ {
			currentCAS().QueueDeleteErr(&aliyun.Error{Class: aliyun.ClassAuth, Op: "Delete", Code: "Forbidden.RAM", Err: errors.New("denied")})
		}
		base := time.Now()
		reconciler.Now = func() time.Time { return base }
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())

		// grace period 内不摘 finalizer
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "1500ms", "200ms").Should(BeTrue())

		// 时钟越过 grace period
		reconciler.Now = func() time.Time { return base.Add(16 * time.Minute) }
		got := getAC(ctx, ns, "abandon")
		got.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())

		eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon"}, &certsv1alpha1.AliyunCertificate{})
			return apierrors.IsNotFound(err)
		})
		Expect(currentCAS().Has(certID)).To(BeTrue(), "Abandon 后 CAS 证书应仍然存在（孤儿）")
		// Certificate 与 Secret 仍应被清理
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "abandon-tls"}, &corev1.Secret{}))).To(BeTrue())
	})

	It("Block 策略下持续失败不摘 finalizer", func() {
		reconciler.CleanupFailurePolicy = CleanupPolicyBlock
		reconciler.CleanupGracePeriod = time.Millisecond
		ns := newNamespace(ctx)
		ac, _ := issuedAC(ns, "block")
		for i := 0; i < 50; i++ {
			currentCAS().QueueDeleteErr(&aliyun.Error{Class: aliyun.ClassAuth, Op: "Delete", Code: "Forbidden.RAM", Err: errors.New("denied")})
		}
		Expect(k8sClient.Delete(ctx, ac)).To(Succeed())
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "block"}, &certsv1alpha1.AliyunCertificate{})
			return err == nil
		}, "2s", "200ms").Should(BeTrue())
	})
})
```


- [ ] **Step 3: 运行确认失败**

```bash
go test ./internal/controller/ -run TestActiveBindingNames 2>&1 | head -3
```

- [ ] **Step 4: 实现 deletion.go 并删除 Task 10 的临时 `reconcileDelete`**

`internal/controller/deletion.go`：

```go
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/naming"
)

const requeueDeletionWait = 2 * time.Second

// activeBindingNames 只统计未在删除中的 Binding（避免与 Argo CD prune 死锁）。
func activeBindingNames(bindings []certsv1alpha1.AliyunCertificateBinding) []string {
	var names []string
	for i := range bindings {
		if bindings[i].DeletionTimestamp.IsZero() {
			names = append(names, bindings[i].Name)
		}
	}
	return names
}

// reconcileDelete 实现 spec §5.6：
// a. 活着的 Binding → 阻塞
// b. CAS 各代（有界，Abandon/Block）
// c. 显式删 cmapi.Certificate 并等它消失（否则 cert-manager 会重建 Secret）
// d. 删 Secret
// e. 摘 finalizer
func (r *AliyunCertificateReconciler) reconcileDelete(ctx context.Context, ac, orig *certsv1alpha1.AliyunCertificate) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ac, certsv1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	// a. 阻塞判断（live read）
	bindings, err := r.liveBindingsFor(ctx, ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	if names := activeBindingNames(bindings); len(names) > 0 {
		msg := "仍被 Binding 引用: " + strings.Join(names, ", ")
		setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonDeletionBlocked, msg)
		r.Recorder.Event(ac, corev1.EventTypeWarning, "DeletionBlocked", msg)
		return ctrl.Result{}, r.patchStatus(ctx, ac, orig) // Binding 变化会通过 watch 唤醒
	}

	if ac.Status.CleanupStartedAt == nil {
		ac.Status.CleanupStartedAt = &metav1.Time{Time: r.now()}
		if err := r.patchStatus(ctx, ac, orig); err != nil {
			return ctrl.Result{}, err
		}
		orig = ac.DeepCopy()
	}

	// b. CAS 清理（依据 status，不依据 spec.uploadToCAS）
	if err := r.cleanupCAS(ctx, ac); err != nil {
		elapsed := r.now().Sub(ac.Status.CleanupStartedAt.Time)
		if r.CleanupFailurePolicy == CleanupPolicyBlock || elapsed < r.CleanupGracePeriod {
			setCondition(ac, certsv1alpha1.ConditionReady, metav1.ConditionFalse, certsv1alpha1.ReasonUploadFailed, "CAS 清理失败，重试中: "+err.Error())
			if perr := r.patchStatus(ctx, ac, orig); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{}, err // 指数退避
		}
		// Abandon：记录后继续
		ids := remainingCertIDs(ac)
		msg := fmt.Sprintf("CAS 清理在 %s 后放弃，遗留 certId=%v", r.CleanupGracePeriod, ids)
		log.Error(err, "cleanup abandoned", "region", ac.Spec.Aliyun.EffectiveCASRegion(), "certIds", ids)
		r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonCleanupAbandoned, msg)
		cleanupAbandonedTotal.WithLabelValues(ac.Spec.Aliyun.EffectiveCASRegion(), aliyun.ClassOf(err).String()).Inc()
	}

	// c. 显式删除 Certificate 并等待消失
	cert := &cmapi.Certificate{}
	err = r.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: certManagerNameFor(ac)}, cert)
	switch {
	case err == nil:
		if cert.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: requeueDeletionWait}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// d. 删 Secret
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ac.Namespace, Name: secretNameFor(ac)}}
	if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// e. 摘 finalizer
	controllerutil.RemoveFinalizer(ac, certsv1alpha1.FinalizerName)
	if err := r.Update(ctx, ac); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("deleted", "name", ac.Name)
	return ctrl.Result{}, nil
}

func remainingCertIDs(ac *certsv1alpha1.AliyunCertificate) []int64 {
	var ids []int64
	if ac.Status.Current != nil && ac.Status.Current.CertID != nil {
		ids = append(ids, *ac.Status.Current.CertID)
	}
	for _, h := range ac.Status.History {
		if h.CertID != nil {
			ids = append(ids, *h.CertID)
		}
	}
	if p := ac.Status.PendingUpload; p != nil {
		_ = p // pendingUpload 没有 certId，只能靠 FindUploaded 兜底；Abandon 时作为已知限制记录
	}
	return ids
}

// cleanupCAS 删除 status 中记录的全部代次；成功的从 status 移除，任一失败立即返回。
func (r *AliyunCertificateReconciler) cleanupCAS(ctx context.Context, ac *certsv1alpha1.AliyunCertificate) error {
	ids := remainingCertIDs(ac)
	if len(ids) == 0 {
		return nil
	}
	cas, err := r.casClient(ctx, ac)
	if err != nil {
		return err
	}
	del := func(gen *certsv1alpha1.CertificateGeneration) error {
		if gen == nil || gen.CertID == nil {
			return nil
		}
		token := naming.ClientToken(ac.UID, gen.Fingerprint) + "d"
		if len(token) > 64 {
			token = token[:64]
		}
		if err := cas.Delete(ctx, *gen.CertID, token); err != nil && aliyun.ClassOf(err) != aliyun.ClassNotFound {
			return err
		}
		gen.CertID = nil
		return nil
	}
	for i := range ac.Status.History {
		if err := del(&ac.Status.History[i]); err != nil {
			return err
		}
	}
	if err := del(ac.Status.Current); err != nil {
		return err
	}
	return nil
}
```

`cleanupAbandonedTotal` 在 Task 14 的 `metrics.go` 中定义；本任务先在 `deletion.go` 顶部临时声明：

```go
var cleanupAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aliyuncert_cleanup_abandoned_total",
	Help: "Number of AliyunCertificate deletions that abandoned CAS cleanup",
}, []string{"region", "reason"})

func init() { metrics.Registry.MustRegister(cleanupAbandonedTotal) }
```

import：`"github.com/prometheus/client_golang/prometheus"`、`"sigs.k8s.io/controller-runtime/pkg/metrics"`。Task 14 会把它搬到 `metrics.go`。

- [ ] **Step 5: 删除 Task 10 中的临时 `reconcileDelete`**，运行：

```bash
make generate manifests && make test 2>&1 | tail -30
```

Expected: PASS。「Abandon」用例约耗时 3–4s（Consistently 窗口）。

- [ ] **Step 6: 提交**

```bash
git add api internal config
git commit -m "feat(controller): ordered deletion with binding protection and bounded CAS cleanup"
```

---

### Task 14: IssuerDefaultDiverged、CAS 存在性探测、指标

**Files:**
- Create: `internal/controller/metrics.go`
- Create: `internal/controller/probe.go`
- Modify: `internal/controller/aliyuncertificate_controller.go`（Diverged 检测；接入探测与指标）
- Modify: `internal/controller/deletion.go`（删除临时的 counter 声明）
- Create: `internal/controller/probe_test.go`

**Interfaces:**
- Produces：
  - `metrics.go` 变量：`certNotAfter *prometheus.GaugeVec{namespace,name}`、`certReady *GaugeVec{namespace,name}`、`certIssuanceStalled *GaugeVec`、`issuerDefaultDiverged *GaugeVec`、`casUploadTotal *CounterVec{result}`、`casDeleteTotal *CounterVec{result}`、`certManagerCertRecreatedTotal *CounterVec{namespace,name}`、`cleanupAbandonedTotal *CounterVec{region,reason}`；`recordCertMetrics(ac)`；`clearCertMetrics(namespace, name)`
  - `probe.go`：`(r) probeCAS(ctx, ac) (reuploaded bool, err error)`——`now - casProbedAt ≥ CASProbeInterval` 时执行；current.certId 不在 CAS → 清空 `current.certId` 并让下一步 `ensureUploaded` 重传
  - `(r) detectIssuerDivergence(ac, source IssuerSource)`：`source == IssuerSourceStatus` 且 flag 当前值非空且 ≠ effective → condition `IssuerDefaultDiverged=True` + 一次 Warning event；否则 `=False`

- [ ] **Step 1: 写 metrics.go**

```go
package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// label 集合刻意很小：绝不把 fingerprint / certId 放进 label（每次轮换都会新增永不消失的 series）。
var (
	certNotAfter = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_not_after_timestamp_seconds",
		Help: "Unix time when the current certificate expires",
	}, []string{"namespace", "name"})
	certReadyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_ready", Help: "1 if Ready condition is True",
	}, []string{"namespace", "name"})
	certIssuanceStalled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_certificate_issuance_stalled", Help: "1 if cert-manager issuance is stalled",
	}, []string{"namespace", "name"})
	issuerDefaultDiverged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aliyuncert_issuer_default_diverged", Help: "1 if pinned issuer differs from current --default-issuer-*",
	}, []string{"namespace", "name"})
	casUploadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_upload_total", Help: "CAS upload attempts by result",
	}, []string{"result"})
	casDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cas_delete_total", Help: "CAS delete attempts by result",
	}, []string{"result"})
	certManagerCertRecreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_certmanager_certificate_recreated_total",
		Help: "Times the cert-manager Certificate was (re)created after first creation; should stay 0",
	}, []string{"namespace", "name"})
	cleanupAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aliyuncert_cleanup_abandoned_total",
		Help: "Number of AliyunCertificate deletions that abandoned CAS cleanup",
	}, []string{"region", "reason"})
)

func init() {
	metrics.Registry.MustRegister(certNotAfter, certReadyGauge, certIssuanceStalled, issuerDefaultDiverged,
		casUploadTotal, casDeleteTotal, certManagerCertRecreatedTotal, cleanupAbandonedTotal)
}

// recordCertMetrics 在每次 status patch 前刷新 gauge。
func recordCertMetrics(ac *certsv1alpha1.AliyunCertificate) {
	ns, n := ac.Namespace, ac.Name
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	if ac.Status.Current != nil {
		certNotAfter.WithLabelValues(ns, n).Set(float64(ac.Status.Current.NotAfter.Unix()))
	}
	certReadyGauge.WithLabelValues(ns, n).Set(b2f(condTrue(ac, certsv1alpha1.ConditionReady)))
	certIssuanceStalled.WithLabelValues(ns, n).Set(b2f(condReasonIs(ac, certsv1alpha1.ConditionIssued, certsv1alpha1.ReasonIssuanceStalled)))
	issuerDefaultDiverged.WithLabelValues(ns, n).Set(b2f(condTrue(ac, certsv1alpha1.ConditionIssuerDefaultDiverged)))
}

// clearCertMetrics 在 CR 删除后移除 series。
func clearCertMetrics(namespace, name string) {
	for _, g := range []*prometheus.GaugeVec{certNotAfter, certReadyGauge, certIssuanceStalled, issuerDefaultDiverged} {
		g.DeleteLabelValues(namespace, name)
	}
}

func condReasonIs(ac *certsv1alpha1.AliyunCertificate, condType, reason string) bool {
	for _, c := range ac.Status.Conditions {
		if c.Type == condType {
			return c.Reason == reason
		}
	}
	return false
}
```

删除 `deletion.go` 顶部临时的 `cleanupAbandonedTotal` 声明与 `init()`。

- [ ] **Step 2: 写 probe.go**

```go
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
)

// probeCAS 周期性确认 current.certId 仍在 CAS。被人手删时清空 certId，交给 ensureUploaded 重传。
// FC3 不引用 certId，因此这里不惊动 Binding（spec §B6.2）。
func (r *AliyunCertificateReconciler) probeCAS(ctx context.Context, ac *certsv1alpha1.AliyunCertificate, primaryDomain string) (bool, error) {
	if !ac.Spec.Aliyun.UploadEnabled() || ac.Status.Current == nil || ac.Status.Current.CertID == nil {
		return false, nil
	}
	if ac.Status.CASProbedAt != nil && r.now().Sub(ac.Status.CASProbedAt.Time) < r.CASProbeInterval {
		return false, nil
	}
	cas, err := r.casClient(ctx, ac)
	if err != nil {
		return false, err
	}
	list, err := cas.FindUploaded(ctx, primaryDomain)
	if err != nil {
		return false, err
	}
	ac.Status.CASProbedAt = &metav1.Time{Time: r.now()}
	for _, c := range list {
		if c.CertID == *ac.Status.Current.CertID {
			return false, nil
		}
	}
	logf.FromContext(ctx).Info("CAS certificate missing, will re-upload", "certId", *ac.Status.Current.CertID)
	r.Recorder.Event(ac, corev1.EventTypeWarning, "CASCertificateMissing", "current certificate not found in CAS; re-uploading")
	// 清空 certId 与指纹以强制 ensureUploaded 走上传；保留 history。
	ac.Status.Current.CertID = nil
	ac.Status.Current.Fingerprint = ""
	return true, nil
}
```

- [ ] **Step 3: 接入 Reconcile**

在 `aliyuncertificate_controller.go`：

1. 步骤 1 解析 issuer 之后、`ac.Status.EffectiveIssuerRef = &issuer` 之后加：

```go
	r.detectIssuerDivergence(ac, source)
```

并新增方法：

```go
// detectIssuerDivergence：已 Pin 且 flag 当前值不同 → 打不参与 Ready 的 condition，并发一次 Warning。
func (r *AliyunCertificateReconciler) detectIssuerDivergence(ac *certsv1alpha1.AliyunCertificate, source IssuerSource) {
	d := r.issuerDefaults().Ref()
	diverged := source == IssuerSourceStatus && d != nil && !IssuerRefEqual(*d, *ac.Status.EffectiveIssuerRef)
	was := condTrue(ac, certsv1alpha1.ConditionIssuerDefaultDiverged)
	if diverged {
		setCondition(ac, certsv1alpha1.ConditionIssuerDefaultDiverged, metav1.ConditionTrue, certsv1alpha1.ReasonIssuerDefaultDiverged,
			fmt.Sprintf("已固化 %s/%s，operator 默认现为 %s/%s；如需切换请显式设置 spec.certificateTemplate.issuerRef",
				ac.Status.EffectiveIssuerRef.Kind, ac.Status.EffectiveIssuerRef.Name, d.Kind, d.Name))
		if !was {
			r.Recorder.Event(ac, corev1.EventTypeWarning, certsv1alpha1.ReasonIssuerDefaultDiverged, "pinned issuer differs from operator default")
		}
		return
	}
	setCondition(ac, certsv1alpha1.ConditionIssuerDefaultDiverged, metav1.ConditionFalse, certsv1alpha1.ReasonReady, "")
}
```

2. 步骤 3 `CreateOrUpdate` 之后：

```go
	if op == controllerutil.OperationResultCreated && ac.Status.CertManagerCertificateName != "" {
		// 之前已经创建过一次又不见了：重建会消耗 LE 配额，必须可观测
		certManagerCertRecreatedTotal.WithLabelValues(ac.Namespace, ac.Name).Inc()
		r.Recorder.Event(ac, corev1.EventTypeWarning, "CertificateRecreated", "cert-manager Certificate was recreated")
	}
```

3. `reconcileIssued` 中，`loadMaterial` 成功之后、`ensureUploaded` 之前加：

```go
	// 9. CAS 存在性探测（12h 节流），丢失则清空 certId 让 ensureUploaded 重传
	primary := ""
	if names := b.DNSNames(); len(names) > 0 {
		primary = names[0]
	}
	if _, err := r.probeCAS(ctx, ac, primary); err != nil {
		return r.handleCloudError(ctx, ac, orig, "ListUserCertificateOrder", err)
	}
```

4. `ensureUploaded` 成功/失败处分别 `casUploadTotal.WithLabelValues("success"|"error"|"throttled").Inc()`；`reclaimOldGenerations` 与 `cleanupCAS` 的 Delete 同理用 `casDeleteTotal`。`throttled` 判定：`aliyun.Error.Code` 前缀 `Throttling`。

5. `patchStatus` 内部在 Patch 前调用 `recordCertMetrics(ac)`；`reconcileDelete` 摘 finalizer 成功后调用 `clearCertMetrics(ac.Namespace, ac.Name)`。

- [ ] **Step 4: 写测试**

`internal/controller/probe_test.go`：

```go
package controller

import (
	"context"
	"time"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

var _ = Describe("证书 controller：Diverged 与 CAS 探测", func() {
	ctx := context.Background()
	var ca *testutil.CA

	BeforeEach(func() {
		resetCAS()
		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "letsencrypt-prod", Kind: "ClusterIssuer"})
		reconciler.Now = nil
		ca = testutil.NewCA(GinkgoT())
	})

	It("flag 变更后 IssuerDefaultDiverged=True 但 Ready 不受影响", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, baseAC(ns, "div"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "div", 1, crt, key)
		eventually(func() bool { return condTrue(getAC(ctx, ns, "div"), certsv1alpha1.ConditionReady) })

		reconciler.SetIssuerDefaults(IssuerDefaults{Name: "zerossl", Kind: "ClusterIssuer"})
		ac := getAC(ctx, ns, "div")
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())

		eventually(func() bool {
			got := getAC(ctx, ns, "div")
			return condTrue(got, certsv1alpha1.ConditionIssuerDefaultDiverged) && condTrue(got, certsv1alpha1.ConditionReady)
		})

		// 显式写 issuerRef 到新值 → Diverged 消失
		ac = getAC(ctx, ns, "div")
		ac.Spec.CertificateTemplate.IssuerRef = &cmmeta.IssuerReference{Name: "zerossl", Kind: "ClusterIssuer"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
		eventually(func() bool {
			got := getAC(ctx, ns, "div")
			return !condTrue(got, certsv1alpha1.ConditionIssuerDefaultDiverged) && got.Status.EffectiveIssuerRef.Name == "zerossl"
		})
	})

	It("CAS 侧证书被删后，探测周期到达时重新上传", func() {
		ns := newNamespace(ctx)
		base := time.Now()
		reconciler.Now = func() time.Time { return base }
		Expect(k8sClient.Create(ctx, baseAC(ns, "probe"))).To(Succeed())
		crt, key := testutil.IssueLeaf(GinkgoT(), ca, "api.example.com")
		simulateIssuance(ctx, ns, "probe", 1, crt, key)
		eventually(func() bool { a := getAC(ctx, ns, "probe"); return a.Status.Current != nil && a.Status.Current.CertID != nil })
		id1 := *getAC(ctx, ns, "probe").Status.Current.CertID

		// 手删 CAS 证书；探测周期未到 → 不动
		Expect(currentCAS().Delete(ctx, id1, "")).To(Succeed())
		ac := getAC(ctx, ns, "probe")
		ac.Annotations = map[string]string{"touch": "1"}
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
		Consistently(func() int { return currentCAS().UploadCalls }, "1500ms", "200ms").Should(Equal(1))

		// 时钟越过 12h → 探测发现丢失 → 重传
		reconciler.Now = func() time.Time { return base.Add(13 * time.Hour) }
		ac = getAC(ctx, ns, "probe")
		ac.Annotations["touch"] = "2"
		Expect(k8sClient.Update(ctx, ac)).To(Succeed())
		eventually(func() bool {
			a := getAC(ctx, ns, "probe")
			return a.Status.Current != nil && a.Status.Current.CertID != nil && *a.Status.Current.CertID != id1
		})
		Expect(currentCAS().UploadCalls).To(Equal(2))
	})
})
```

注意：探测用例要求 `ensureUploaded` 在 `Fingerprint == ""` 时把它当作「指纹变了」——Task 11 的实现里 `ac.Status.Current.Fingerprint == b.Fingerprint` 为 false 即走上传，天然满足；但会把「旧 current（certId 已清空）」推进 history，history 里出现一条 `certId == nil` 的记录，回收时 `del` 对 nil 直接跳过，也是安全的。

- [ ] **Step 5: 运行**

```bash
make test 2>&1 | tail -30
```

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/controller
git commit -m "feat(controller): issuer divergence condition, CAS presence probe and metrics"
```

---

### Task 15: `cmd/main.go` 接线 — flags、禁用 Secret cache、生产 CASFactory、RBAC 与部署清单

**Files:**
- Modify: `cmd/main.go`
- Modify: `config/manager/manager.yaml`（去掉 `runAsUser`/`runAsGroup`；replicas 1）
- Modify: `Dockerfile`（确认 distroless nonroot 基础镜像）
- Create: `config/samples/certs_v1alpha1_aliyuncertificate.yaml`（替换脚手架样例）
- Create: `config/samples/aliyun-credentials-secret.yaml`
- Create: `cmd/main_flags_test.go`

**Interfaces:**
- Produces：可运行的二进制；flags 见 spec §9；`config/rbac/role.yaml` 中 secrets 仅 `get`/`delete`

- [ ] **Step 1: 写 flag 解析的测试（把 flag 解析抽成纯函数以便测试）**

`cmd/main_flags_test.go`：

```go
package main

import (
	"testing"
	"time"
)

func TestParseOperatorFlags_Defaults(t *testing.T) {
	o, err := parseOperatorFlags([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if o.ResyncInterval != time.Hour || o.DriftCheckInterval != time.Hour || o.CASProbeInterval != 12*time.Hour ||
		o.IssuanceStallThreshold != 6*time.Hour || o.CloudCallTimeout != 30*time.Second ||
		o.CleanupGracePeriod != 15*time.Minute || o.CleanupFailurePolicy != "Abandon" {
		t.Errorf("默认值不符: %+v", o)
	}
	if o.DefaultIssuerKind != "Issuer" || o.DefaultIssuerGroup != "cert-manager.io" {
		t.Errorf("issuer 默认 kind/group 应与 cert-manager 一致: %+v", o)
	}
}

func TestParseOperatorFlags_Values(t *testing.T) {
	o, err := parseOperatorFlags([]string{
		"--default-issuer-name=letsencrypt-prod", "--default-issuer-kind=ClusterIssuer",
		"--cleanup-failure-policy=Block", "--cloud-call-timeout=10s", "--watch-namespaces=a,b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultIssuerName != "letsencrypt-prod" || o.DefaultIssuerKind != "ClusterIssuer" || o.CleanupFailurePolicy != "Block" ||
		o.CloudCallTimeout != 10*time.Second || len(o.WatchNamespaces) != 2 {
		t.Errorf("解析不符: %+v", o)
	}
}

func TestParseOperatorFlags_RejectsBadPolicy(t *testing.T) {
	if _, err := parseOperatorFlags([]string{"--cleanup-failure-policy=Whatever"}); err == nil {
		t.Fatal("非法 policy 应报错")
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./cmd/ 2>&1 | head -3
```

- [ ] **Step 3: 改写 main.go**

在脚手架 `cmd/main.go` 基础上：保留 scheme 注册、metrics/health/leader-election 的脚手架 flag；新增：

```go
// operatorOptions 是本 operator 自有 flag（spec §9）。
type operatorOptions struct {
	DefaultIssuerName, DefaultIssuerKind, DefaultIssuerGroup string
	ResyncInterval, DriftCheckInterval, CASProbeInterval      time.Duration
	IssuanceStallThreshold, CloudCallTimeout, CleanupGracePeriod time.Duration
	CleanupFailurePolicy                                        string
	WatchNamespaces                                             []string
}

// registerOperatorFlags 把 flag 注册到给定 FlagSet；main 传 flag.CommandLine，测试传新建的 FlagSet。
// 返回的 options 在 fs.Parse 之后才有值，随后必须调用 validate()。
func registerOperatorFlags(fs *flag.FlagSet) *operatorOptions {
	o := &operatorOptions{}
	fs.StringVar(&o.DefaultIssuerName, "default-issuer-name", "", "Name of the Issuer to use when spec.certificateTemplate.issuerRef is not set")
	fs.StringVar(&o.DefaultIssuerKind, "default-issuer-kind", "Issuer", "Kind of the default issuer")
	fs.StringVar(&o.DefaultIssuerGroup, "default-issuer-group", "cert-manager.io", "Group of the default issuer")
	fs.DurationVar(&o.ResyncInterval, "certificate-resync-interval", time.Hour, "Periodic resync of AliyunCertificate (Secret drift detection channel)")
	fs.DurationVar(&o.DriftCheckInterval, "drift-check-interval", time.Hour, "Periodic Observe of binding targets (used by Plan 2)")
	fs.DurationVar(&o.CASProbeInterval, "cas-probe-interval", 12*time.Hour, "How often to verify the current certificate still exists in CAS")
	fs.DurationVar(&o.IssuanceStallThreshold, "issuance-stall-threshold", 6*time.Hour, "Issuing=True longer than this marks IssuanceStalled")
	fs.DurationVar(&o.CloudCallTimeout, "cloud-call-timeout", 30*time.Second, "Timeout for every Alibaba Cloud API call")
	fs.DurationVar(&o.CleanupGracePeriod, "cleanup-grace-period", 15*time.Minute, "How long to retry cloud cleanup in finalizers before applying cleanup-failure-policy")
	fs.StringVar(&o.CleanupFailurePolicy, "cleanup-failure-policy", "Abandon", "Abandon | Block")
	fs.StringVar(&o.watchNamespacesRaw, "watch-namespaces", "", "Comma-separated namespaces to watch; empty = all")
	return o
}

// validate 在 Parse 之后校验并展开派生字段。
func (o *operatorOptions) validate() error {
	if o.CleanupFailurePolicy != controller.CleanupPolicyAbandon && o.CleanupFailurePolicy != controller.CleanupPolicyBlock {
		return fmt.Errorf("--cleanup-failure-policy 必须是 Abandon 或 Block，得到 %q", o.CleanupFailurePolicy)
	}
	o.WatchNamespaces = nil
	for _, n := range strings.Split(o.watchNamespacesRaw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			o.WatchNamespaces = append(o.WatchNamespaces, n)
		}
	}
	return nil
}

// parseOperatorFlags 供测试使用：独立 FlagSet 上注册、解析、校验。
func parseOperatorFlags(args []string) (*operatorOptions, error) {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	o := registerOperatorFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}
```



manager 构造：

```go
	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "certs.bestheme.ac.cn",
		LeaderElectionReleaseOnCancel: true,
		Client: client.Options{Cache: &client.CacheOptions{
			// Secret 不进 cache：零副本，RBAC 不需要 list/watch
			DisableFor: []client.Object{&corev1.Secret{}},
		}},
	}
	if len(opts.WatchNamespaces) > 0 {
		mgrOpts.Cache.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range opts.WatchNamespaces {
			mgrOpts.Cache.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
```

scheme 注册处追加 `utilruntime.Must(cmapi.AddToScheme(scheme))`。

reconciler 接线（替换脚手架的 `SetupWithManager` 调用）：

```go
	if err := controller.RegisterIndexes(mgr); err != nil {
		setupLog.Error(err, "unable to register indexes")
		os.Exit(1)
	}
	limiters := aliyun.NewLimiters()
	casCache := aliyun.NewClientCache()
	certReconciler := &controller.AliyunCertificateReconciler{
		Client:                 mgr.GetClient(),
		APIReader:              mgr.GetAPIReader(),
		Scheme:                 mgr.GetScheme(),
		Recorder:               mgr.GetEventRecorderFor("aliyuncertificate"),
		CASFactory:             controller.NewCASFactory(mgr.GetClient(), casCache, limiters, opts.CloudCallTimeout),
		ResyncInterval:         opts.ResyncInterval,
		CASProbeInterval:       opts.CASProbeInterval,
		IssuanceStallThreshold: opts.IssuanceStallThreshold,
		CleanupGracePeriod:     opts.CleanupGracePeriod,
		CleanupFailurePolicy:   opts.CleanupFailurePolicy,
	}
	certReconciler.SetIssuerDefaults(controller.IssuerDefaults{Name: opts.DefaultIssuerName, Kind: opts.DefaultIssuerKind, Group: opts.DefaultIssuerGroup})
	if err := certReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AliyunCertificate")
		os.Exit(1)
	}
```

`NewCASFactory` 的 reader 用 `mgr.GetClient()`——由于 Secret 已 `DisableFor`，这个 client 对 Secret 的 Get 就是直读。

- [ ] **Step 4: 部署清单调整**

`config/manager/manager.yaml`：
- `spec.replicas: 1`
- 删除 pod / container `securityContext` 中的 `runAsUser` / `runAsGroup`（OpenShift `restricted-v2` 会注入 UID）；保留 `runAsNonRoot: true`、`seccompProfile: {type: RuntimeDefault}`、`allowPrivilegeEscalation: false`、`capabilities.drop: [ALL]`，并加 `readOnlyRootFilesystem: true`
- `args` 追加：`--leader-elect`、`--default-issuer-name=letsencrypt-prod`、`--default-issuer-kind=ClusterIssuer`

`Dockerfile`：确认最后阶段 `FROM gcr.io/distroless/static:nonroot`（脚手架默认即是），无需 `USER 65532`——删掉该行，交给 OpenShift。

`config/samples/certs_v1alpha1_aliyuncertificate.yaml`：

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificate
metadata:
  name: timehorse-api
spec:
  certificateTemplate:
    dnsNames: [api.timehorse.bestheme.ac.cn]
  aliyun:
    credentialsRef: { name: aliyun-cas-credentials }
    region: cn-hangzhou
  retention:
    keepLast: 2
```

`config/samples/aliyun-credentials-secret.yaml`：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aliyun-cas-credentials
type: certs.bestheme.ac.cn/aliyun-credentials
stringData:
  accessKeyId: REPLACE_ME
  accessKeySecret: REPLACE_ME
```

- [ ] **Step 5: 生成、构建、测试**

```bash
make manifests generate build test
grep -n "secrets" -A6 config/rbac/role.yaml
```

Expected: 全部通过；`role.yaml` 中 `secrets` 的 verbs 恰为 `get`、`delete`；`cmd` 包三个 flag 测试 PASS。

- [ ] **Step 6: 本地烟雾运行（可选，需要 kubeconfig 指向装有 cert-manager 的集群）**

```bash
make install
make run ARGS="--default-issuer-name=letsencrypt-staging --default-issuer-kind=ClusterIssuer"
```

Expected: 日志出现 `Starting workers`；`kubectl apply -f config/samples/` 后 `kubectl get aliyuncertificate` 的 `ISSUED` 列随 cert-manager 签发进度变化。

- [ ] **Step 7: 提交**

```bash
git add cmd config Dockerfile
git commit -m "feat: wire operator flags, uncached Secret client, production CAS factory and deployment manifests"
```

---

## 自查记录（写计划时已做）

**Spec 覆盖对照（Plan 1 范围 = spec §4.1、§4.3、§4.4、§5、§8.1–8.2、§9、§10.1 证书侧、§11 部分、§12.1–12.2 证书侧）**

| Spec 条目 | Task |
|---|---|
| §4.1 AliyunCertificate API、CEL、printcolumns | 2 |
| §4.2 Binding 类型（Plan 1 只定义类型） | 3 |
| §4.3 凭证 Secret 契约 | 8 |
| §4.4 conditions / reasons | 2 |
| §5.1 不缓存 Secret；watch Certificate 与 Binding | 10、15 |
| §5.2 步骤 0–4 | 10 |
| §5.2 步骤 5–7 | 11 |
| §5.2 步骤 8 | 12 |
| §5.2 步骤 9 | 14 |
| §5.2 步骤 10 聚合 | 11（`aggregateReady`） |
| §5.3 issuer 解析、Pin、bootstrap、Diverged | 9、10、14 |
| §5.4 Secret 校验 1–7 | 4、11 |
| §5.5 命名、ClientToken、write-ahead、错误分类、限流 | 6、7、11 |
| §5.6 finalizer 顺序、阻塞、有界清理 | 13 |
| §5.7 三重护栏 | 12 |
| §8.1 RBAC secrets get/delete | 10（marker）、15（验证） |
| §8.2 client 缓存按 resourceVersion | 8 |
| §9 flags | 15 |
| §10.1 证书侧 metrics | 14 |
| §11 replicas 1、SCC、distroless | 15 |
| §12.1–12.2 单元 / envtest 覆盖 | 各任务 |

**不在 Plan 1 的 spec 条目**（Plan 2 / Plan 3）：§4.2 Binding 的 controller、§6 全部、§7 provider 接口与 FC3、§8.3 RAM 策略文档、§10.1 binding 侧 metrics、§10.2–10.3 events 汇总与 PrometheusRule、§11 Argo CD 清单与 README、§12.3 真实环境集成测试、§13–§15。

**已知需要执行者现场判断的点**（不是占位，是依赖版本的细节）：
- `metav1.Duration` 是否有 `DeepCopy`（Task 10 给了替代函数）。
- `credential.Config` setter 方法名（Task 8 说明以 `go doc` 为准）。
- cert-manager v1.21.1 与 operator-sdk 脚手架的 k8s 依赖版本对齐（Task 1 Step 4 给了处理方法）。
- 阿里云「同名证书」的真实错误码（Task 11 `isDuplicateName` 列了候选，实测后收窄）。
