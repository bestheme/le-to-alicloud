# OSS 自定义域名证书绑定 provider 设计

日期：2026-09-07
状态：已与所有者逐节确认（§1–§7），待实施
上游 spec：`docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md`（下文简称「主 spec」）。本文只写增量；凡未提及的行为以主 spec 为准。

## 0. 摘要

为 `AliyunCertificateBinding` 新增第二个 target 类型 `OSSCustomDomain`：把 cert-manager 签发、operator 已上传到 CAS 的证书绑定到 OSS bucket 的自定义域名（CNAME）上。OSS 与 FC3 的本质差异是**目标上存的不是 PEM 而是 CAS 的 certId**，因此本设计同时落实主 spec §7 预留但至今闲置的两条能力位 `ReferencesCertByID` / `RequiresCASUpload`，并给通用层加一条「按 certId 对账」的支路。日后 CDN / CLB 这类同样按 certId 引用的目标直接复用这条支路。

首个生产目标：`www.bestheme.ac.cn` → bucket `applanding-102181`（cn-hangzhou）。

## 1. 已核实事实

来源：阿里云 OSS API 文档（PutCname / ListCname）、`github.com/aliyun/alibabacloud-oss-go-sdk-v2 v1.6.0` 的 `go doc`，2026-09-07 核实。

| # | 事实 | 对设计的影响 |
|---|---|---|
| O1 | `PutCname` 请求体 `BucketCnameConfiguration.Cname.CertificateConfiguration` 含 `CertId`、`Certificate`、`PrivateKey`、`PreviousCertId`、`Force`、`DeleteCertificate` 六个字段 | 支持按 CAS certId 引用，私钥不必再经 OSS 一条链路 |
| O2 | `CertId` 形如 `493****-cn-hangzhou`，带区域后缀 | 通用层需要 CAS 区域来拼字符串（→ `CertMaterial.CASRegion`） |
| O3 | 不带 `PreviousCertId` 时必须 `Force=true`，否则服务端校验旧 certId 不匹配即报错 | Apply 恒带 `Force=true`，接受 last-write-wins（与主 spec §6.3 FC3 同一取舍） |
| O4 | `DeleteCertificate=true` 只摘证书，CNAME 记录保留 | Unbind 语义天然成立 |
| O5 | `ListCname` 返回 `Bucket`、`Owner`（账号）与每个 `Cname{Domain, LastModified, Status, Certificate{Type(CAS/Upload), CertId, Status, CreationDate, Fingerprint, ValidStartDate, ValidEndDate}, IsWildCard}` | `Owner` 供账号 fencing；`CertId` 供幂等与漂移判断 |
| O6 | `ListCname.Certificate.Fingerprint` 文档只说「证书签名」，示例为冒号分隔十六进制且打码，算法未标明 | **不用它做幂等判断**（见 §16 决策 D22） |
| O7 | RAM action 为 `oss:PutCname`、`oss:ListCname`；bucket 级资源 ARN `acs:oss:*:<accountId>:<bucket>`。绑定证书时另需 `yundun-cert:DescribeSSLCertificatePrivateKey`、`yundun-cert:DescribeSSLCertificatePublicKeyDetail`、`yundun-cert:CreateSSLCertificate`（PutCname 文档明载；`yundun-cert` 无资源级授权，只能 `*`）——2026-09-07 实测补正：不授这三条时 `ListCname` 通而 `PutCname` 回 `AccessDenied` | §9 策略 |
| O8 | SDK v2 凭证接口为单方法 `credentials.CredentialsProvider.GetCredentials(ctx) (Credentials{AccessKeyID, AccessKeySecret, SecurityToken, Expires}, error)`；client 用 `oss.NewClient(oss.LoadDefaultConfig().WithRegion(r).WithCredentialsProvider(p)...)` | 现有 `aliyun.Credentials.Build()` 的产物套一层适配器即可 |
| O9 | SDK v2 错误为 `*oss.ServiceError{Code, Message, RequestID, EC, StatusCode, Snapshot, …}` | 只取 `Code` / `StatusCode` 进分类，丢弃其余（主 spec §6.4 私钥保护、§13） |
| O10 | 主 spec §2.2 #9：CAS 不同 endpoint 的证书集合互相隔离 | certId 的区域后缀必须与上传时的 CAS 区域一致；后缀语义见 §8 待实测 T-OSS2 |

## 2. 目标与非目标

**目标**

- 一个 `AliyunCertificateBinding` 能把证书绑到 OSS bucket 的一个 CNAME 域名上，续期后自动换绑，漂移自动纠正，`deletionPolicy: Unbind` 时摘证书。
- 现有 FC3 Binding 行为逐字节不变；现有 envtest / 单元测试原样通过。
- RAM 文档与 `make verify-ram-policy` 门禁覆盖 OSS 段。

**非目标**

- 不创建 / 删除 CNAME 记录，不做域名所有权验证（`CreateCnameToken`）。CNAME 必须已存在且已验证。
- 不支持向 OSS 内联上传 PEM（`Certificate` / `PrivateKey` 字段）。
- 不做 OSS 访问域名之外的任何 bucket 配置。
- 不改 `AliyunCertificate` 的 spec 或状态机。

## 3. API 变更（`api/v1alpha1`）

### 3.1 target union

```go
const TargetTypeOSSCustomDomain = "OSSCustomDomain"

// OSSCustomDomainTarget 是 OSS bucket 自定义域名（CNAME）。
type OSSCustomDomainTarget struct {
    // bucket 所在 region，如 cn-hangzhou。决定 OSS endpoint，与 CAS 区域无关。
    // +kubebuilder:validation:MinLength=1
    Region string `json:"region"`
    // +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`
    Bucket string `json:"bucket"`
    // 已绑定到该 bucket 且已通过所有权验证的自定义域名。
    // +kubebuilder:validation:MinLength=1
    DomainName string `json:"domainName"`
}
```

`BindingTarget`：

- `Type` 枚举扩为 `FC3CustomDomain;OSSCustomDomain`。
- 新增 `OSSCustomDomain *OSSCustomDomainTarget \`json:"ossCustomDomain,omitempty"\``。
- 第二条 CEL：`self.type == 'OSSCustomDomain' ? has(self.ossCustomDomain) : !has(self.ossCustomDomain)`，message「ossCustomDomain 必须且只能在 type=OSSCustomDomain 时设置」。现有 FC3 那条不动。两条合起来保证任意时刻恰好一个内嵌块存在。
- `target` 整体不可变规则不动。

`TargetKey()` 新增分支：`OSSCustomDomain/<region>/<bucket>/<domainName>`。同 key 的 Binding 走现有仲裁。

没有 `ensureHTTPSProtocol`：OSS CNAME 没有协议开关。

### 3.2 status 新字段

```go
// 目标上实际引用的 CAS certId 字符串（形如 27087165-cn-hangzhou）。
// 只有 ReferencesCertByID 的 provider 写；FC3 恒为空。
// +optional
AppliedCertRef string `json:"appliedCertRef,omitempty"`
// 上一轮观测到的「漂移 certRef」，语义与 driftedFingerprint 逐条对应。
// +optional
DriftedCertRef string `json:"driftedCertRef,omitempty"`
```

`appliedFingerprint` / `driftedFingerprint` 对 OSS Binding **照旧写入**：证书 controller 的保留护栏 3（主 spec §5.7）靠 `appliedFingerprint` 识别在役代，不需要知道 provider 是哪一种。

不复用旧字段装 certId：字段名与内容不符是评审必挑的坑。

### 3.3 新 Reason

`ReasonCASUploadRequired = "CASUploadRequired"`，写在 `Applied=False` 上，`Ready` 聚合后为 `False`。Message 固定：「目标类型要求证书上传到 CAS，请把 AliyunCertificate 的 spec.aliyun.uploadToCAS 设为 true」。

### 3.4 样例

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificateBinding
metadata:
  name: www-oss
  namespace: bestheme-web
spec:
  certificateRef: { name: www-bestheme }
  target:
    type: OSSCustomDomain
    ossCustomDomain:
      region: cn-hangzhou
      bucket: applanding-102181
      domainName: www.bestheme.ac.cn
  deletionPolicy: Orphan
```

进 `config/samples/`。

## 4. Provider 契约变更（`pkg/provider`）

```go
type CertMaterial struct {
    // …现有字段不变…
    CASRegion string // 证书上传所在的 CAS 区域（EffectiveCASRegion()）；拼 certRef 用
}

// CASCertRef 返回目标按 ID 引用证书时使用的字符串 "<certId>-<casRegion>"。
// CertID 为 nil 或 CASRegion 为空时返回 ""。这是该字符串唯一的拼装点。
func (m CertMaterial) CASCertRef() string

type ObservedState struct {
    CurrentFingerprint string // 内联 PEM 的目标：实际证书指纹；"" = 无证书。OSS 恒为 ""
    CurrentCertRef     string // 按 ID 引用的目标：实际引用的 certRef；"" = 无证书。FC3 恒为 ""
    Protocol           string
    AccountID          string
}
```

`Capabilities`、`ApplyOptions`、`Provider` 接口签名不变。OSS provider 声明：

```go
provider.Capabilities{ReferencesCertByID: true, SupportsProtocolSwitch: false, RequiresCASUpload: true}
```

## 5. 通用层（`internal/controller`）

### 5.1 证书身份三元组

新增内部类型，每轮构造一次：

```go
// certIdentity 抹平「按指纹」与「按 certRef」两种对账方式。
type certIdentity struct {
    want    string // 本轮该在目标上的：m.Fingerprint 或 m.CASCertRef()
    current string // 目标上实际的：obs.CurrentFingerprint 或 obs.CurrentCertRef
    applied string // 上次我们写的：status.appliedFingerprint 或 status.appliedCertRef
}

func identityOf(caps provider.Capabilities, b *Binding, obs provider.ObservedState, m provider.CertMaterial) certIdentity
```

以下比对全部改走三元组，不再各处直接读 `CurrentFingerprint`：

| 位置 | 现在 | 改后 |
|---|---|---|
| 幂等短路（步骤 5→6） | `obs.CurrentFingerprint == m.Fingerprint` | `id.current == id.want` |
| `noteDrift` | `cur == "" \|\| cur == applied \|\| cur == m.Fingerprint` | 同式，取 `id.*`；漂移值写进 `driftedFingerprint`（FC3）或 `driftedCertRef`（OSS） |
| `freezeApplied` | 写 `appliedFingerprint`，清 `driftedFingerprint` | 始终写 `appliedFingerprint = m.Fingerprint`；ByID 时**另**写 `appliedCertRef = m.CASCertRef()`；两个 drifted 字段一并清空 |
| `unbindTarget` | `obs.CurrentFingerprint != b.Status.AppliedFingerprint` ⇒ 跳过 | `id.current != id.applied` ⇒ 跳过 |

`protocolSatisfied` 只在 `SupportsProtocolSwitch` 为真时参与短路判定；`ensureHTTPSProtocol` 的读取改成 nil 安全的 `ensureHTTPSOf(b) bool`（FC3 取字段，其它类型恒 false），替换现有对 `b.Spec.Target.FC3CustomDomain` 的直接解引用。

### 5.2 Reconcile 新增步骤 3c

位置：certificateGate（3b）之后、构造 provider client（4）之前。只查注册表，不碰凭证、不发云调用：

```
p, _ := provider.Get(b.Spec.Target.Type)      // 认不出的类型交给步骤 4 现有的工厂错误路径
if p.Capabilities().RequiresCASUpload {
    if !ac.Spec.Aliyun.UploadEnabled() {
        Applied=False / CASUploadRequired；aggregate；RequeueAfter DriftCheckInterval
    }
    if m.CertID == nil {                       // 这一代还没传完 CAS
        Applied=False / CertificateNotReady，message「证书尚未取得 CAS certId」；
        RequeueAfter certificateGateRequeue
    }
}
```

两条早退都不碰 `appliedFingerprint`（主 spec §6.2「失败与旁路早退不写 applied」）。`m.CertID` 由 `loadBindingMaterial` 从 `ac.Status.Current` 带出，只在 `Current.Fingerprint == m.Fingerprint` 时非 nil，所以这一步同时覆盖了「续期后新代次已进 Secret、CAS 还没传完」的窗口。证书 CR 的 watch 会在 status.current 推进时唤醒 Binding。

`loadBindingMaterial` 补一行：`m.CASRegion = ac.Spec.Aliyun.EffectiveCASRegion()`。

**对主 spec §7 的偏离**：原文「`RequiresCASUpload` 的 provider 出现时即便 `uploadToCAS: false` 也强制上传」。Binding 不拥有 `AliyunCertificate`，替它开上传会让证书 controller 反向依赖 Binding 的 provider。改为 Binding 报 `CASUploadRequired`，等用户改证书 spec。记入 §16 D21。

### 5.3 target 相关辅助函数

`targetOf`、`requiredDomainsOf`、`targetIdentifier`、`targetRegion` 各加 `OSSCustomDomain` 分支：`Target{Type, Region: oss.Region, Identifier: oss.DomainName, Spec: oss}`；要求覆盖的域名为 `oss.DomainName`；日志标识用 domainName。

### 5.4 工厂

- `NewProviderFactory(reader, cache *aliyun.ClientCache[provider.Client], limiters, timeout)`；`aliyun.ClientKey` 加 `Type string`。
- 按 `tg.Type` 分派：`FC3CustomDomain` → 现有 `NewFC3Client(...)`；`OSSCustomDomain` → `NewOSSClient(cred, OSSClientConfig{Region, Timeout, Limiters, LimiterKey, OnCall: aliyunAPICallRecorder(serviceOSS)})`。
- `cmd/main.go`：`fc3Cache := aliyun.NewClientCache[aliyun.FC3Client]()` 改为 `providerCache := aliyun.NewClientCache[provider.Client]()`。
- Endpoint 恒由 SDK 按 region 推出（`oss-<region>.aliyuncs.com`），不提供覆盖，理由与 FC3 相同。

## 6. OSS client（`pkg/aliyun`）

### 6.1 接口

```go
// Cname 是 ListCname 里一条 CNAME 记录被剪裁后的只读视图。
type Cname struct {
    Domain    string
    Status    string // Enabled | Disabled
    CertRef   string // Certificate.CertId；无证书时 ""
    CertType  string // CAS | Upload；无证书时 ""
    AccountID string // ListCname.Owner
}

type OSSClient interface {
    // GetCname 列出 bucket 的 CNAME 并按 domain 精确匹配。找不到该 domain 返回
    // Class=NotFound Code=CnameNotFound 的 *aliyun.Error；bucket 不存在由 SDK 的
    // NoSuchBucket/404 自然落入 NotFound。
    GetCname(ctx context.Context, bucket, domain string) (*Cname, error)
    // PutCnameCert 以 Force=true 覆盖式地把 certRef 绑到 domain。
    PutCnameCert(ctx context.Context, bucket, domain, certRef string) error
    // DeleteCnameCert 摘掉 domain 上的证书，CNAME 记录保留。
    DeleteCnameCert(ctx context.Context, bucket, domain string) error
}

type OSSClientConfig struct {
    Region     string
    Timeout    time.Duration
    Limiters   *Limiters
    LimiterKey string
    OnCall     func(action, code string, d time.Duration)
}

func NewOSSClient(cred credential.Credential, cfg OSSClientConfig) (OSSClient, error)
```

Action 常量：`ActionListCname = "ListCname"`、`ActionPutCname = "PutCname"`（Put 与 Delete 走同一个 OpenAPI，指标 action 同名）。

### 6.2 错误分类

`*oss.ServiceError` → `fromSDKError(op, &e.Code, &e.StatusCode)`，复用 `classifyCode`。不引入 OSS 专用分类表；O9 保证 Message / 响应体不进错误链。非 `ServiceError` 的错误（网络、超时）走现有 `Classify` 的兜底。

### 6.3 凭证适配器

```go
type ossCredentialsProvider struct{ cred credential.Credential }

func (p ossCredentialsProvider) GetCredentials(ctx context.Context) (credentials.Credentials, error) {
    c, err := p.cred.GetCredential()   // credentials-go：AK/SK/STS/RoleARN/OIDC 统一出口
    …
    return credentials.Credentials{AccessKeyID: *c.AccessKeyId, AccessKeySecret: *c.AccessKeySecret, SecurityToken: deref(c.SecurityToken)}, nil
}
```

不新增 Secret 键；`aliyun.Credentials` 结构不变。

### 6.4 限流与指标

- `LimitOSS`：5 QPS / burst 1，与 `LimitFC3` 同档；OSS 未公布 CNAME 接口频控，实测后再放宽。
- `serviceOSS = "oss"` 加入 `metrics.go` 的 service label 取值；现有 `aliyuncert_aliyun_api_requests_total` 与 `aliyuncert_aliyun_api_duration_seconds`（label `service,action,code`）自动覆盖。

### 6.5 依赖

`go.mod` 新增 `github.com/aliyun/alibabacloud-oss-go-sdk-v2 v1.6.0`。

## 7. OSS provider（`pkg/provider/oss`）

文件形状照抄 `pkg/provider/fc3`：`provider.go`（`init()` 注册、三个方法）、`errors.go`（`toProviderError` / `swallowNotFound`）。

| 方法 | 行为 |
|---|---|
| `Observe` | `GetCname` → `ObservedState{CurrentCertRef: c.CertRef, AccountID: c.AccountID}`。NotFound（bucket 或 CNAME）→ `CodeTargetNotFound`（通用层现有语义：目标不存在是错误，`Ready=False/TargetNotFound`） |
| `Apply` | `m.CASCertRef() == ""` → `CodePermanent`（步骤 3c 已挡，这里是防御，同 FC3 对空 CASName 的处理）；否则 `PutCnameCert(bucket, domain, ref)`。不做 read-modify-write：`PutCname` 只碰证书配置。忽略 `ApplyOptions`（无协议开关；`PreviousFingerprint` 不适用） |
| `Cleanup(Unbind)` | `DeleteCnameCert`；NotFound 吞掉视为成功。`Orphan` 不发任何调用 |

「只解绑自己的」判断在通用层按 `appliedCertRef` 比对（§5.1），provider 不做。

`clientOf` 断言 `aliyun.OSSClient`，失败 `CodeInvalidClient`。错误映射按 `aliyun.ErrClass` 折成 `Throttled / Retryable / Auth / TargetNotFound / Permanent`，与 fc3 同表。

## 8. 与 CAS 侧的耦合

- 保留护栏 3 靠 `appliedFingerprint` 不回收在役代；OSS Binding 照旧写该字段，护栏自动覆盖。
- OSS 引用的是 CAS 里的 certId，删除被引用的证书是否被拒未知（T-OSS5）。若被拒，回收路径现有处置：`ReclaimFailed` 事件、condition 不动、下一轮重试；不阻塞主流程。
- 用户把证书的 `uploadToCAS` 从 true 改回 false：证书 controller 停止上传但不删已传代次（主 spec §5.5）；Binding 下一轮在 3c 报 `CASUploadRequired`，OSS 上的证书保持不动。

## 9. RAM 与文档

新增 `docs/ram/binding-oss-policy.json`：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["oss:ListCname", "oss:PutCname"],
      "Resource": ["acs:oss:*:<accountId>:<bucket>"]
    },
    {
      "Effect": "Allow",
      "Action": [
        "yundun-cert:DescribeSSLCertificatePrivateKey",
        "yundun-cert:DescribeSSLCertificatePublicKeyDetail",
        "yundun-cert:CreateSSLCertificate"
      ],
      "Resource": "*"
    }
  ]
}
```

第二条 Statement 是 O7 的实测补正：`PutCname` 带 `CertId` 时由 OSS 以调用方身份去 CAS 取证书，这三个动作是那一步的权限要求，operator 自己从不调；它与 OSS 段同进同出，所以放在同一个文件里。

- `full-policy.json` = CAS + FC3 + OSS 三段按序拼接（OSS 段本身含上面两条 Statement）；`hack/verify-ram-policy.py` 的合并校验改为三份。
- README「RAM 权限」：完整策略 JSON 同步；「每个 Action 用在哪、缺了会怎样」加 `oss:ListCname`（Observe 与 Unbind 前读取；缺则 `Ready=False/CredentialsInvalid`，`Applied` 不动）与 `oss:PutCname`（Apply 与 Unbind；缺则 `Applied=False/CredentialsInvalid`，事件 `ApplyFailed`）两行；「按你的部署裁剪」加「不用 OSS 就删第三段」与「OSS Binding 必须保留 CAS 段」；「把占位符填成真实值」加 `<bucket>` 取 `spec.target.ossCustomDomain.bucket`，OSS ARN 的 region 位写 `*`。开头的「5 个 OpenAPI 动作」改为 7。
- 主 spec 更新：§2 加 OSS 事实（引用本文 §1）、§4.2 加 OSS 样例与 status 新字段、§6.2 加步骤 3c、新增 §6.6「OSS provider 的 Apply」、§7 契约代码块同步、§8.3 加第三段、§12.3 加 T-OSS 行、§16 加 D21–D23。
- README 用法一节加 OSS Binding 示例。

## 10. 发布

v0.2.0。CRD 新增字段与 status 字段皆可选，旧对象无需迁移；CRD 与 operator 同版本经 Argo 升级（sync-wave 已覆盖）。

## 11. 测试

### 11.1 单元

- `pkg/provider/oss`：fake `OSSClient`，覆盖 Observe（有证书 / 无证书 / NotFound / Auth）、Apply（空 certRef 拒绝、成功、错误映射）、Cleanup（Orphan 零调用、Unbind、NotFound 吞掉）。
- `pkg/aliyun`：`ossCredentialsProvider` 适配（AK/SK、带 STS token）；`ServiceError` → `classifyCode` 的映射；`GetCname` 的 domain 过滤与 `CnameNotFound` 合成（用可注入的 SDK 调用桩）。
- `pkg/provider`：`CASCertRef()` 三态（nil ID / 空 region / 正常）。
- `api/v1alpha1`：`TargetKey()` OSS 分支。
- `internal/controller`：`identityOf` 两种取法；`ensureHTTPSOf` nil 安全；`targetOf` / `requiredDomainsOf` OSS 分支。

### 11.2 envtest

- CRD 校验：`type=OSSCustomDomain` 缺 `ossCustomDomain` 被拒；同时带两个内嵌块被拒；bucket 名不合法被拒（沿 `binding_crd_validation_test.go` 的路数）。
- 3c：证书 `uploadToCAS: false` ⇒ `Applied=False/CASUploadRequired`，provider 零调用。
- 3c：`status.current.certId` 为 nil ⇒ `CertificateNotReady`，certId 就位后一轮内 Apply。
- 短路：目标 `CurrentCertRef == want` ⇒ 不 Apply，补记 `appliedCertRef` 与 `appliedFingerprint`。
- 漂移：`CurrentCertRef` 为第三方 certRef ⇒ `DriftCorrected` 事件与计数器各一次，随后 Apply；连续失败不重复发。
- Unbind：`CurrentCertRef != appliedCertRef` ⇒ 跳过 Cleanup；相等 ⇒ 调用一次。
- 回归门：现有 FC3 全套测试原样通过，`appliedCertRef` 对 FC3 Binding 恒为空。

### 11.3 真实环境集成实测（进主 spec §12.3）

所有者决定（2026-09-07）：**不另建牺牲 bucket**。探针代码写好、以 `OSS_TEST_BUCKET` / `OSS_TEST_DOMAIN` 环境变量门控，未设置时在 RESULTS.md 记「未实测」；实际核实由 CRD 与 provider 就绪后对 `www.bestheme.ac.cn` 的一次受控端到端首绑完成（绑的是 LE 正式证书，`Force` 覆盖其现有证书，风险是一次证书切换），结论回填同一张表。

| # | 待核实 | 猜错的失效方式 |
|---|---|---|
| T-OSS1 | `PutCname` 带 `CertId` + `Force=true` 的首绑与换绑是否都成功。**2026-09-07 实测：首绑成功**（`CertId: "27114423-cn-hangzhou"`, `Force: true`，Binding `Applied=True` / `appliedCertRef` 同值）；**换绑那一半仍未测**，等下次续期换代。同一次实测暴露 RAM 前提：先回 `AccessDenied`，补上三个 `yundun-cert:*SSLCertificate*` 动作后才通（见 §9） | 换绑被拒 ⇒ 续期永远 `ApplyFailed` |
| T-OSS2 | `CertId` 区域后缀取 CAS 区域还是 bucket 区域（两者不同时才能分辨；首绑两者同为 cn-hangzhou，只能证明「同区域可行」）。**2026-09-07 实测：只证明了「同区域可行」**——CAS 与 bucket 同为 `cn-hangzhou`，后缀在两种解释下同值，语义仍分辨不出；跨区组合未测 | 跨区组合下 Apply 被拒 |
| T-OSS3 | `ListCname` 回报的 `CertId` 是否与写入字符串逐字相同。**2026-09-07 实测：逐字相同**（ossutil 读回 bucket `applanding-102181` 得 `27114423-cn-hangzhou`），短路必命中；同次读回 `ListCname.Owner=102181` 非空，账号 fencing 前提成立。**已关闭** | 短路永不命中 ⇒ 每小时一次无谓 `PutCname` 并误报漂移 |
| T-OSS4 | `DeleteCertificate=true` 后 CNAME 记录是否保留 | Unbind 打断线上访问 |
| T-OSS5 | CAS 删除被 OSS 引用的证书是否被拒、错误码 | 回收路径持续 `ReclaimFailed` |
| T-OSS6 | 缺 `oss:PutCname` 权限时的错误码与 HTTP 状态 | 分类落入 Permanent 而非 Auth，退避节奏错 |

## 12. 安全

- 私钥不再经 OSS 链路；`PutCname` 请求体只有 certId。
- `ServiceError.Snapshot`（响应体）与 `Message` 不进错误链、日志、事件、status（O9）。
- `oss:PutCname` 能改 bucket 上任意 CNAME 的证书；ARN 收窄到 bucket 级是 OSS 允许的最细粒度，README 明示。
- OSS 绑定要求的 `yundun-cert:DescribeSSLCertificatePrivateKey`（O7）让这个 AK 能读出账号下证书的私钥，只能授在 `*` 上，绕不开——私钥不经 operator 这条链路，但 RAM 层面的泄漏面确实比只用 FC3 时大，README 与主 spec §8.3 都明示，并要求独立 RAM 子账号 + 独立 AK。
- 账号 fencing 由 `ListCname.Owner` 支撑，与 FC3 同一套闸门。

## 13. 已知限制

- 一个 CNAME 域名在 OSS 全局唯一，但 `TargetKey` 含 bucket：两个 Binding 写同一域名、不同 bucket 不会被仲裁拦下，而 OSS 会拒掉其中一个（`CnameAlreadyExists` 或找不到 CNAME）。可接受：这是配置错误，症状清晰。
- last-write-wins（O3）：与 FC3 相同的取舍，窗口极短、写入频率极低。
- `ListCname` 是全量列举后本地过滤；bucket 上 CNAME 数量通常个位数，不分页。

## 14. 实施边界（供 writing-plans 切任务）

按依赖顺序：
1. API：类型、CEL、`TargetKey`、深拷贝与 CRD 生成、CRD 校验 envtest。
2. `pkg/provider`：`CASRegion` / `CASCertRef` / `CurrentCertRef`。
3. `pkg/aliyun`：OSS client、凭证适配器、限流、错误分类；单元测试。
4. `pkg/provider/oss`：三方法与错误映射；单元测试。
5. 通用层：`identityOf`、`ensureHTTPSOf`、步骤 3c、`freezeApplied` / `noteDrift` / `unbindTarget` 改造、target 辅助函数、工厂与 `main.go`；envtest。
6. RAM / README / 主 spec / 样例 / RESULTS 探针骨架；`verify-ram-policy` 通过。
7. 集成探针代码（环境变量门控）。
8. 发版 v0.2.0 与 `www.bestheme.ac.cn` 受控首绑（回填 T-OSS 表）。

## 15. 未来演进

- CDN / CLB provider：直接复用 `ReferencesCertByID` 支路与 `certIdentity`，各自只需一个 `pkg/aliyun` client 与一个 `pkg/provider/<x>` 包。
- 若 T-OSS3 证明 `ListCname.CertId` 与写入值不逐字相同，再考虑归一化函数，放在 `CASCertRef()` 旁边。

## 16. 决策记录（续主 spec §16）

| # | 决策 | 备选 | 理由 |
|---|---|---|---|
| D21 | OSS 按 CAS certId 引用；`RequiresCASUpload` 未满足时 Binding 报 `CASUploadRequired` 而不替证书开上传 | 内联 PEM；Binding 强制证书上传 | 私钥少经一条链路；落实预留能力位；Binding 不拥有证书 CR，反向依赖会把两个 controller 缠在一起 |
| D22 | 幂等与漂移按 certRef 字符串比对，通用层加身份三元组 | provider 调 CAS `GetUserCertificateDetail` 翻译成指纹；用 `ListCname.Fingerprint` | 前者每轮多一次 CAS 调用且需双 client；后者算法未标明，猜错即静默漂移循环 |
| D23 | target 显式 `{region, bucket, domainName}` | 只给 domainName、operator 扫 bucket 找 | RAM 面收窄到 bucket 级；少一次全账号列举 |
| D24 | 不另建牺牲 bucket，实测由 `www.bestheme.ac.cn` 受控首绑完成 | 建测试 bucket + 验证域名 | 所有者决定；代价是首绑即实测，需在低流量时段执行并准备回滚（控制台重新绑回旧证书） |
