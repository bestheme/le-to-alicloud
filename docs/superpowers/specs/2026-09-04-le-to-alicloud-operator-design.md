# le_to_alicloud Operator 设计文档

- 日期：2026-09-04
- 状态：设计已定，待实现计划
- API Group：`certs.bestheme.ac.cn/v1alpha1`
- 技术栈：Go + Operator SDK（controller-runtime）

---

## 1. 目标与非目标

### 1.1 目标

一个 Kubernetes Operator，让「阿里云上的一张 TLS 证书」成为集群里的声明式资源：

1. 用户创建 `AliyunCertificate`，operator 通过 cert-manager 申请证书（第一版用 Let's Encrypt，但设计上只依赖 cert-manager 的 `Issuer` 抽象，换任何签发后端都不改 operator）。
2. 证书签发后自动上传到阿里云数字证书管理服务（CAS），续期后自动上传新代次并按保留策略回收旧代次。
3. 用户创建 `AliyunCertificateBinding`，把证书部署到指定的阿里云服务。第一版只支持 **FC3（函数计算 3.0）自定义域名**，但 provider 层为 CDN / CLB / ALB 等预留扩展点。
4. 云侧配置被人工改动时能检测并纠正（drift correction）。

### 1.2 非目标

- 不创建 / 管理 cert-manager 的 `Issuer` / `ClusterIssuer`，不处理 ACME 账号和 DNS-01 solver 配置。
- 不纳管非 cert-manager 产出的现成证书（商业证书导入是另一个用例）。
- 第一版不做 OLM bundle 发布、不做多版本 CRD、不做 webhook（defaulting / conversion）。
- 不实现 RRSA / OIDC 联邦凭证（但 Secret 契约预留字段）。

### 1.3 目标运行环境

自管 OpenShift（k8s ≥ 1.25），Argo CD 部署，集群内已有 cert-manager，external-dns 使用阿里云 DNS。生产工作负载在阿里云 FC3。

---

## 2. 已核实事实与未核实项

设计中的所有 API 断言都经官方文档核实。**未核实项在 §12.3 列为集成测试的必测清单，实现前必须先实测。**

### 2.1 阿里云 FC3

| 事实 | 来源 |
|---|---|
| `CustomDomain` 是顶级资源，`PUT /2023-03-30/custom-domains/{domainName}`，域名是主键。证书配置 `certConfig` 与 `routeConfig`（path → functionName）、`protocol`、`tlsConfig`、`wafConfig`、`authConfig`、`corsConfig` 平级。**证书归属于域名，与函数无关。** | [UpdateCustomDomain](https://www.alibabacloud.com/help/en/functioncompute/fc-3-0/developer-reference/api-fc-2023-03-30-updatecustomdomain) |
| `CertConfig` 只有 `certName` / `certificate` / `privateKey` 三个字段，全部内联 PEM，**没有 `certId`**。Serverless Devs 的 `certId` 是客户端语法糖。 | [CertConfig](https://www.alibabacloud.com/help/en/functioncompute/fc/developer-reference/api-fc-2023-03-30-struct-certconfig) |
| `UpdateCustomDomainInput` 所有字段均标注非必填；**全量替换还是部分合并，文档未说明**。 | [UpdateCustomDomainInput](https://www.alibabacloud.com/help/en/functioncompute/fc-3-0/developer-reference/api-fc-2023-03-30-struct-updatecustomdomaininput) |
| `GetCustomDomain` 响应含 `accountId`；响应中的 `certConfig` **含明文 `privateKey`**。 | [GetCustomDomain](https://www.alibabacloud.com/help/en/functioncompute/fc-3-0/developer-reference/api-fc-2023-03-30-getcustomdomain) |
| RAM code 为 `fc`，**支持资源级授权**：`acs:fc:{regionId}:{accountId}:custom-domains/{domainName}`。 | [FC RAM](https://help.aliyun.com/zh/functioncompute/fc-3-0/developer-reference/api-fc-2023-03-30-ram) |
| Go SDK：`github.com/alibabacloud-go/fc-20230330/v4` | GitHub |

### 2.2 阿里云 CAS（数字证书管理服务）

| 事实 | 来源 |
|---|---|
| `UploadUserCertificate(Name, Cert, Key, ClientToken, ResourceGroupId)` 返回 `CertId`；**同账号内 `Name` 唯一**，≤ 63 字符，文档描述字符集为「字母、数字、下划线」（**未提及 `-` 和 `.`**）；QPS 100。**实测（2026-09-05, cn-hangzhou，§12.3 #4）：`-` 与 `.` 都接受，且云上原样保存、不做归一化**（回查列举里的名字与提交值逐字相同），因此 `findByName` 可以按提交的名字精确比对。**实测（2026-09-05, cn-hangzhou，§12.3 #13）：同名冲突返回 `NameRepeat`（Permanent）**——不在 Plan 1 猜的三个候选码里，已追加进 `internal/controller/upload.go` 的 `isDuplicateName`。**实测（2026-09-05, cn-hangzhou，§12.3 #3）：`ClientToken` 不做上传幂等**——同 token、同 Name 重传同样报 `NameRepeat`，而不是回放首次的 `CertId`。 | [UploadUserCertificate](https://help.aliyun.com/zh/ssl-certificate/developer-reference/api-cas-2020-04-07-uploadusercertificate)；实测见 `test/integration/RESULTS.md` #3 / #4 / #13 |
| `DeleteUserCertificate(CertId, ClientToken)`；QPS 100。 | [DeleteUserCertificate](https://help.aliyun.com/zh/ssl-certificate/developer-reference/api-cas-2020-04-07-deleteusercertificate) |
| `ListUserCertificateOrder` 的 `Keyword` **只匹配域名或资源 ID，不匹配证书 `Name`**；`OrderType=UPLOAD` 时返回项含 `CertificateId`、`Name`、`Sans`、`EndDate`（`YYYY-MM-DD`）等；**QPS 仅 10**。**实测（2026-09-05, cn-hangzhou，§12.3 #12）：`Keyword` 对证书域名做任意子串匹配，且不做 DNS 通配符展开**——SAN 为 `*.it.integration.invalid` 的证书，用 `*.it.integration.invalid` / `it.integration.invalid` / `integration` / 甚至非标签边界的 `ntegratio` 都能查到，而通配符本应覆盖的 `probe.it.integration.invalid` 查不到。本项目的列举恒不传 `Status`（`pkg/aliyun/cas_sdk.go:177`——SDK 的取值是单选枚举，没有表示「全部」的值）。据该枚举语义**推导**：结果不含已过期证书；配了 `ResourceGroupId` 时仅限该资源组。**这两条未经实测**——`#8` 探针跑的账号里空 `Keyword` 列举返回 0 张，没有已过期证书可供被包含或排除，这个排除在构造上就不可观测；`RESULTS.md` `#8` 行里的同一串字面文字是探针被要求附上的**范围限定**，不是发现，同行的证据列也写明「不足以证明它是账号全集」。`#8` 实测到的只有那个 0 计数。 | [ListUserCertificateOrder](https://help.aliyun.com/zh/ssl-certificate/developer-reference/api-cas-2020-04-07-listusercertificateorder)；实测见 `test/integration/RESULTS.md` #12（#8 只有 0 计数是实测） |
| `GetUserCertificateDetail` **会返回私钥**。本设计不使用、不授权。 | [GetUserCertificateDetail](https://help.aliyun.com/zh/ssl-certificate/developer-reference/api-cas-2020-04-07-getusercertificatedetail) |
| CAS 拒绝删除已部署到阿里云产品的证书；FC3 使用内联 PEM，CAS 侧不视为「已部署」。 | [吊销和删除证书](https://help.aliyun.com/zh/ssl-certificate/revoke-and-delete-a-certificate) |
| **RAM code 为 `yundun-cert`，不是 `cas`**。全部 action 的资源类型为「全部资源」，**无法资源级收窄**。 | [CAS RAM](https://help.aliyun.com/zh/ssl-certificate/developer-reference/api-cas-2020-04-07-ram) |
| **仅支持 PEM 编码**（「数字证书管理服务仅支持上传PEM格式编码（.pem和.crt后缀）的证书文件」）。证书链顺序：服务器证书 → 中间证书 → 根证书。私钥接受 `-----BEGIN RSA PRIVATE KEY-----`（PKCS#1）与 `-----BEGIN EC PRIVATE KEY-----`；**加密私钥被拒绝**（需 `openssl rsa -in encrypted.key -out decrypted.key` 解密）。证书与私钥不匹配时报「证书与私钥不匹配」。控制台的格式转换工具把「PFX、JKS、PKCS8」转为 PEM——暗示 PKCS#8 不是 CAS 的原生期望格式。**实测（2026-09-05, cn-hangzhou，§12.3 #1）：PKCS#1 RSA、SEC1 EC、PKCS#8 三种未加密编码 CAS 一律接受**（各自上传成功）——控制台提供 PKCS#8 转换工具，不等于 API 会拒绝 PKCS#8；带 `Proc-Type: 4,ENCRYPTED` 头的私钥块被拒，错误码 `PrivateKeyFormatException`（Permanent）——即 CAS 报的是**格式**错误而非配对错误，格式校验先于配对校验。（这一条只说明「带 ENCRYPTED 头的块被拒」，**不**说明 CAS 能解析并拒绝一个格式良好的加密私钥；FC3 侧未测。）**实测（2026-09-05, cn-hangzhou，§12.3 #5）：leaf + 其签发 CA（两块）接受，仅 leaf 也接受**，故不要求带根；把 CA 放在 leaf 前面**被拒**（`NotMatch.CertificateAndPrivateKey`，Permanent），即顺序敏感、leaf 必须在首位。这两条合起来说明 LE 的「leaf + intermediate、无 root」形状在 CAS 可用。FC3 侧未测。 | [上传 SSL 证书](https://help.aliyun.com/zh/ssl-certificate/user-guide/upload-an-ssl-certificate)；实测见 `test/integration/RESULTS.md` #1 / #5 |
| 阿里云统一的 PEM 规范（CDN 文档）：「将服务器证书放在第一位，中间证书放在第二位，证书之间不能有空行」；每行 64 字符；PKCS#8 私钥须用 `openssl rsa -in old.pem -out new.pem` 转为 PKCS#1；密钥长度建议 ≥ 2048。 | [CDN 证书格式](https://help.aliyun.com/zh/cdn/user-guide/certificate-formats) |

### 2.3 cert-manager

| 事实 | 来源 |
|---|---|
| `CertificateSpec` 必填字段只有 `secretName` 和 `issuerRef`；`issuerRef.name` 必填，`kind` 默认 `Issuer`，`group` 默认 `cert-manager.io`。 | [API docs](https://cert-manager.io/docs/reference/api-docs/) |
| `Certificate` 的 condition 只有 `Ready` 和 `Issuing`；status 含 `revision`、`renewalTime`、`failedIssuanceAttempts`、`lastFailureTime`、`notBefore`、`notAfter`。 | 同上 |
| controller 有 `--default-issuer-name` / `--default-issuer-kind`（默认 `Issuer`）/ `--default-issuer-group`（默认 `cert-manager.io`）。 | [controller CLI](https://cert-manager.io/docs/cli/controller/) |
| **`--enable-certificate-owner-ref` 默认关闭**：Certificate 默认不拥有它产出的 Secret。 | 同上 |
| **修改 `issuerRef` 会触发重签**（与 dnsNames、subject、duration 等并列）。 | [Certificate resource](https://cert-manager.io/docs/usage/certificate/) |
| `cert-manager.io/issue-temporary-certificate: "true"` 会让 Secret 先出现**自签名临时证书**。 | 同上 |
| `secretTemplate` 的 labels / annotations 被强制维持。 | 同上 |

### 2.4 Kubernetes / 生态

| 事实 | 备注 |
|---|---|
| 对象 annotations 总量上限 262144 字节；客户端 `kubectl apply` 的 `last-applied-configuration` 在大 CRD 上会撞限，修法是 server-side apply。 | cert-manager 的 `issuers.cert-manager.io` 是经典案例 |
| `x-kubernetes-validations`（CEL）需 k8s ≥ 1.25；OpenShift 4.12 = k8s 1.25。 | |
| envtest 没有 GC controller，ownerRef 级联删除不会发生。 | 影响测试设计 |
| controller-runtime 默认 workqueue 限速是每对象指数退避 + 全局约 10 QPS 令牌桶，**不约束出站 HTTP**。 | 出站限流需自建 |

### 2.5 未核实项（必测）

见 §12.3。**截至 2026-09-05 的进度**：CAS 侧的 #1 / #3 / #4 / #5 / #9 / #12 / #13 已核实（依据 `test/integration/RESULTS.md`），
cert-manager 侧的 #6 / #7 / #11 也已核实（同一依据；这三项实测于 2026-09-05，cert-manager v1.20.3，SelfSigned Issuer），
FC3 侧的 #14 也已核实（同一依据；实测于 2026-09-05，错误码 `DomainNameNotFound`、HTTP 404），
#8 **部分核实**（实测到的只有「当前账号空 `Keyword` 列举返回 0 张」这个计数；列举语义是据 SDK 枚举推导、非实测，配额上限也未测）；余下 FC3 侧的 #2 / #10
（以及 #1 / #5 的 FC3 那一半）**仍未核实**，原因逐条写在 §12.3 对应行——**不要把它们当成已核实**。
两处例外要单独说明：#1 / #5 的 FC3 一半在 §12.3 只写了「FC3 侧未测」没写原因，原因是
**未设置 `FC3_TEST_DOMAIN`**（`RESULTS.md` #1 的 FC3 行；本项要真的改写一个 FC3 自定义域名的
`certConfig`，没有专用测试域名就无从测起），而 #5 的 FC3 一半连 `RESULTS.md` 行都没有，
见 §12.3 表下的说明。

---

## 3. 架构总览

```
┌──────────────────────────── operator (单二进制, replicas=1) ────────────────────────────┐
│                                                                                          │
│  AliyunCertificate ──▶ 证书 controller ──┬─▶ cert-manager Certificate (owned)             │
│         ▲                                ├─▶ 读 Secret (Client, 已 DisableFor)                 │
│         │ status.current 变化             ├─▶ CAS UploadUserCertificate / Delete           │
│         │ (field index 反查)              └─▶ status: current / history / conditions      │
│         │                                                                                │
│  AliyunCertificateBinding ─▶ 绑定 controller ─▶ Provider.Observe / Apply / Cleanup        │
│                                                     └─▶ FC3: GetCustomDomain / Update    │
│                                                                                          │
│  pkg/provider: interface + registry        pkg/aliyun: 窄接口 CASClient / FC3Client       │
│  pkg/provider/fc3: 第一个实现               pkg/aliyun/fake: 故障注入                        │
└──────────────────────────────────────────────────────────────────────────────────────────┘
```

核心原则：

- **两个 CRD**：证书本身与「证书 ↔ 目标」的关联是两种生命周期，分开。
- **SHA-256 指纹是全系统的幂等基准**：证书侧用它命名 CAS 证书、判定是否需要上传；绑定侧用它判定目标是否已是最新。
- **上传 CAS 与绑定 FC3 是并行副作用，不是串行依赖**：两者都只依赖同一个 Secret。对 FC3，CAS 上传是归档 / 控制台可见性，不是功能必需。
- **写入内容的确定性**：FC3 写入的证书内容只由 Secret 决定，不由 status 决定。这保证多副本短暂重叠时最坏情况是重复写同样内容。
- **宁可停在旧证书，也不把坏证书推到线上**：任何校验不通过都拒绝上传 / 应用，而不是继续。
- **level-triggered**：status 短路只能跳过写，不能跳过周期性的读。

---

## 4. API 设计

### 4.1 AliyunCertificate

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificate
metadata:
  name: timehorse-api
  namespace: timehorse
spec:
  # 可选，默认 "<metadata.name>-tls"
  secretName: timehorse-api-tls

  # 自有结构体，字段类型复用 cert-manager 的 Go 类型
  certificateTemplate:
    issuerRef:                       # 可选；缺省回退到 --default-issuer-* flag
      name: letsencrypt-prod
      kind: ClusterIssuer
    dnsNames: [api.timehorse.bestheme.ac.cn]
    # 以下均可选：commonName, ipAddresses, duration, renewBefore,
    #            subject, usages, privateKey, secretTemplate

  aliyun:
    credentialsRef: { name: aliyun-cas-credentials }   # 同 namespace，无 namespace 字段
    region: cn-hangzhou
    casRegion: ""                    # 可选，默认 = region（CAS 是否 region 化待实测）
    endpointOverride: ""             # 可选，VPC 内网 / 专有云
    resourceGroupId: ""              # 可选
    uploadToCAS: true                # 默认 true；false 时跳过 CAS 全部逻辑

  retention:
    keepLast: 2                      # 默认 2，最小 1；current + history 的总数
    minAge: 24h                      # 默认 24h；代次年龄不足不回收

status:
  observedGeneration: 3
  effectiveIssuerRef: { name: letsencrypt-prod, kind: ClusterIssuer, group: cert-manager.io }
  secretName: timehorse-api-tls
  certManagerCertificateName: timehorse-api
  issuance:                          # 镜像 cert-manager Certificate.status
    revision: 4
    renewalTime: "2026-11-01T00:00:00Z"
    failedIssuanceAttempts: 0
    lastFailureTime: null
  current:
    fingerprint: "ab12cd34…"         # SHA-256(DER)，小写 hex
    certId: 123456                   # uploadToCAS=false 时为空
    casName: "timehorse_api_ab12cd34ef56"
    notBefore: "…"
    notAfter: "…"
    uploadedAt: "…"
  history:                           # ≤ keepLast-1 条，不含 PEM
    - { fingerprint: "…", certId: 123455, casName: "…", notAfter: "…", uploadedAt: "…" }
  pendingUpload:                     # write-ahead 记录，上传成功后清空
    fingerprint: "…"
    casName: "…"
    clientToken: "…"
    startedAt: "…"
  casProbedAt: "…"                   # CAS 存在性探测的节流时间戳
  conditions:
    - type: Ready                    # 汇总
    - type: Issued                   # Certificate Ready 且 Secret 通过全部校验
    - type: Uploaded                 # CAS 存在 current 对应的证书；uploadToCAS=false 时不参与 Ready
    - type: IssuerDefaultDiverged    # flag 默认值 ≠ 固化值；不参与 Ready
```

**`CertificateTemplate` Go 定义**（自有顶层字段，嵌套类型复用 cert-manager）：

```go
type CertificateTemplate struct {
    IssuerRef      *cmmeta.IssuerReference          `json:"issuerRef,omitempty"`
    CommonName     string                           `json:"commonName,omitempty"`
    DNSNames       []string                         `json:"dnsNames,omitempty"`
    IPAddresses    []string                         `json:"ipAddresses,omitempty"`
    Duration       *metav1.Duration                 `json:"duration,omitempty"`
    RenewBefore    *metav1.Duration                 `json:"renewBefore,omitempty"`
    Subject        *cmapi.X509Subject               `json:"subject,omitempty"`
    Usages         []cmapi.KeyUsage                 `json:"usages,omitempty"`
    PrivateKey     *cmapi.CertificatePrivateKey     `json:"privateKey,omitempty"`
    SecretTemplate *cmapi.CertificateSecretTemplate `json:"secretTemplate,omitempty"`
}
```

设计取舍（已反复权衡，不再翻）：不直接嵌入 `cmapi.CertificateSpec`。原因：会继承 `secretName` 必填、CRD 体积膨胀、暴露 `isCA` / `keystores` 等对本链路无意义的字段。代价是 cert-manager 新增字段时需跟进一次。

**CRD 校验（CEL / kubebuilder marker）**：

- `dnsNames` 与 `commonName` 至少一个非空
- `retention.keepLast >= 1`
- `aliyun.credentialsRef.name` 必填，`aliyun.region` 必填
- printcolumns：`READY`、`ISSUED`、`UPLOADED`、`EFFECTIVE-ISSUER`、`NOT-AFTER`、`AGE`

### 4.2 AliyunCertificateBinding

```yaml
apiVersion: certs.bestheme.ac.cn/v1alpha1
kind: AliyunCertificateBinding
metadata:
  name: timehorse-api-fc3
  namespace: timehorse
spec:
  certificateRef: { name: timehorse-api }     # 同 namespace
  target:                                     # 不可变
    type: FC3CustomDomain                     # discriminated union 判别字段
    fc3CustomDomain:
      region: cn-hangzhou
      domainName: api.timehorse.bestheme.ac.cn
      ensureHTTPSProtocol: false              # true 且当前 protocol 不含 HTTPS 时才改 protocol
  credentialsRef: { name: aliyun-fc-credentials }   # 可选，默认继承证书的
  deletionPolicy: Orphan                      # Orphan（默认）| Unbind

status:
  observedGeneration: 2
  appliedFingerprint: "ab12cd34…"             # 目标上实际生效的指纹
  driftedFingerprint: "ef56ab78…"             # 可选；上一轮观测到的「漂移证书」指纹
                                              # （既不是我们上次写的，也不是当前该写的）；
                                              # 未观测到漂移时为空。存在的唯一理由是给
                                              # DriftCorrected 事件与 drift 计数器一个
                                              # 「上一轮」的比较基准——漂移在 Apply 修好
                                              # 之前每一轮都还在，没有这条痕迹事件就会
                                              # 随重试节奏一轮一轮重发（§10.2「只在状态
                                              # 跃迁时发」）
  lastAppliedTime: "…"
  lastObservedTime: "…"                       # 最近一次 Observe 的时间（drift 检测）
  boundAccountId: "1234567890"                # 首次成功 Apply 时固化，用于账号 fencing
  conditions:
    - type: Ready                             # Applied && !Conflict
    - type: Applied                           # 目标实际指纹 == 证书 current 指纹
    - type: Conflict                          # 同目标存在其它 Binding，或 accountId 不匹配
```

**CRD 校验**：

- `target` 整体不可变：`self == oldSelf`（transition rule）
- union 一致性：`self.type == 'FC3CustomDomain' ? has(self.fc3CustomDomain) : true`，并禁止出现与 `type` 不匹配的内嵌块
- `deletionPolicy` enum `Orphan | Unbind`
- printcolumns：`READY`、`APPLIED`、`CONFLICT`、`TARGET`、`AGE`

### 4.3 凭证 Secret 契约

```yaml
apiVersion: v1
kind: Secret
type: certs.bestheme.ac.cn/aliyun-credentials     # 自定义 type，便于 RBAC / 审计筛选
metadata:
  name: aliyun-cas-credentials
stringData:
  accessKeyId: LTAI…
  accessKeySecret: …
  securityToken: ""            # 可选，STS
  # 预留给 RRSA / OIDC；第一版不实现，但 key 名先定下
  roleArn: ""
  oidcProviderArn: ""
  oidcTokenFilePath: ""
```

底层用 `github.com/aliyun/credentials-go` 的 provider chain 构造凭证，将来实现 RRSA 不改 CRD、不改 Secret 契约。

**跨 namespace 引用：禁止。** CRD 里不放 `namespace` 字段，用类型系统禁止。理由：K8s 没有通用的跨 namespace 引用授权机制，允许跨 namespace 等于让任意 namespace 读取别人的云凭证。

### 4.4 Conditions 与 Reason 常量

```go
// AliyunCertificate
const (
    ReasonNoIssuer                 = "NoIssuer"
    ReasonIssuerDefaultDiverged    = "IssuerDefaultDiverged"     // 不参与 Ready
    ReasonSecretNameConflict       = "SecretNameConflict"
    ReasonCertificateNotReady      = "CertificateNotReady"
    ReasonIssuanceStalled          = "IssuanceStalled"
    ReasonSecretNotFound           = "SecretNotFound"
    ReasonSecretInvalid            = "SecretInvalid"             // 链断 / 公私钥不匹配
    ReasonSelfSignedDuringIssuance = "SelfSignedDuringIssuance"  // 临时证书
    ReasonSANsMismatch             = "SANsMismatch"
    ReasonCredentialsNotFound      = "CredentialsSecretNotFound"
    ReasonCredentialsInvalid       = "CredentialsInvalid"
    ReasonUploadFailed             = "UploadFailed"
    ReasonThrottled                = "Throttled"
    ReasonDeletionBlocked          = "DeletionBlockedByBindings"
    ReasonCleanupFailed            = "CleanupFailed"
    ReasonCleanupAbandoned         = "CleanupAbandoned"
    ReasonUploadDisabled           = "UploadDisabled"            // uploadToCAS=false 时 Uploaded 的 reason
    ReasonReady                    = "Ready"
)

// AliyunCertificateBinding（CredentialsNotFound / CredentialsInvalid / CertificateNotReady 与证书侧共用，不重复声明）
// 注意 ReasonCertificateNotReady 两边共用：它只在上面的证书块里声明一次
// （api/v1alpha1/conditions.go），绑定侧直接引用，这里不再重复声明。
const (
    ReasonCertificateNotFound = "CertificateNotFound"
    ReasonTargetNotFound      = "TargetNotFound"
    ReasonDomainNotCovered    = "DomainNotCovered"
    ReasonConflictingBinding  = "ConflictingBinding"
    ReasonNoConflict          = "NoConflict"        // Conflict=False 的 reason：同目标只有自己，或自己是仲裁胜者
    ReasonAccountMismatch     = "AccountMismatch"
    ReasonApplyFailed         = "ApplyFailed"
    ReasonObserveFailed       = "ObserveFailed"     // Observe 失败；旁路 reason，只在 Applied 已存在时覆写
    ReasonDriftCorrected      = "DriftCorrected"
    ReasonApplied             = "Applied"
)
```

---

## 5. 证书 controller

### 5.1 Watch 与触发

- 主资源：`AliyunCertificate`
- Owns：`cmapi.Certificate`（cert-manager 每次签发 / 续期都会推进 `status.revision` 与 `notAfter`，这是我们读 Secret 的触发信号）
- Watches：`AliyunCertificateBinding` → 映射到其 `spec.certificateRef`（用于回收守卫及时生效、以及删除阻塞及时解除）
- **不 watch、不缓存任何 Secret**。TLS Secret 与凭证 Secret 都走普通的 `mgr.GetClient()`（`material.go` / `cas_factory.go` / `provider_factory.go`），之所以不缓存是因为 manager 的 `client.CacheOptions.DisableFor` 里列了 `&corev1.Secret{}`（`cmd/main.go`）——被 DisableFor 的类型上，`Client.Get` 等同直读 API server。**不是**通过 `mgr.GetAPIReader()`（那个只用在 live list Binding 与 refreshUploadState 上）。收益：内存中无 Secret 副本、RBAC 上 secrets 不需要 `list`/`watch`。代价：周期 resync 成为 Secret 漂移检测的承重通道，定为 1h（`--certificate-resync-interval`）。
- 每次成功 reconcile 返回 `RequeueAfter: certificate-resync-interval`。

### 5.2 Reconcile 步骤

```
 0. deletionTimestamp != nil → §5.6 清理分支
 1. 解析 issuerRef（§5.3）；无法解析 → Ready=False/NoIssuer，不 requeue（等 spec 变更）
 2. 首次创建前检查 spec.secretName 对应的 Secret：已存在且不是我们 Certificate 的产物
    → Ready=False/SecretNameConflict，不 requeue
 3. CreateOrUpdate cmapi.Certificate（ownerRef 指向自己）
    - 期望态比对后才 update，避免无谓写入（LE 速率限制护栏）
    - 绝不因为「Secret 内容不对」删除并重建 Certificate
    - secretTemplate 注入 label certs.bestheme.ac.cn/managed=true（排查用）
    - privateKey.encoding 缺省填 PKCS1（FC3 示例、CAS 私钥头列表、CDN 转换指引三处一致指向 PKCS#1）
 4. 镜像 Certificate.status 到 status.issuance
    - Issuing=True 持续超过 --issuance-stall-threshold（默认 6h）
      → Issued=False/IssuanceStalled + Warning event + metric，**但不结束本轮**：
        status.current 指向的仍是一张有效、正在服役的证书，探测 / 回收 / 复读 Secret
        都必须照常进行（与 §5.2 步骤 9 旁路化、§5.7 回收失败不降级同一条原则）
    - Certificate 未 Ready → Issued=False/CertificateNotReady，return（靠 watch）；
      停滞时不覆写这个 reason，否则「已经卡了几小时」这条信息会被抹平
 5. 读 Secret（`Client`，已 DisableFor，等同直读），校验（§5.4）；任一不通过 → Issued=False/<reason>，不上传，
    **不清空 status.current**（Secret 短暂消失再回来时不能被当作新代次）
 6. 指纹 == status.current.fingerprint → 跳到 8
 7. uploadToCAS=true 时上传（§5.5）；旧 current 推进 history，写新 current
    uploadToCAS=false 时只更新 status.current（certId / casName 为空）
 8. 保留策略回收（§5.7）
 9. CAS 存在性探测（uploadToCAS=true 且 now - casProbedAt ≥ --cas-probe-interval，默认 12h）：
    ListUserCertificateOrder(OrderType=UPLOAD, Keyword=<主域名>) 客户端按 Name 过滤，
    current.certId 不在 → 重新上传并更新 certId。FC3 不引用 certId，故不惊动 Binding。
10. Ready = Issued && (Uploaded || !uploadToCAS)
```

### 5.3 issuerRef 解析与 Pin 语义

优先级：`spec.certificateTemplate.issuerRef` > `status.effectiveIssuerRef` > 已存在的 `cmapi.Certificate.spec.issuerRef` > `--default-issuer-*` flag。

- **Pin 写在期望态计算里**：`status.effectiveIssuerRef` 非空时，期望态的 `issuerRef` 就是它，flag 不参与。否则「改 flag → 下次 reconcile 把 Certificate 的 issuerRef 改成新值 → 触发全集群重签」。
- **bootstrap**：`status.effectiveIssuerRef` 为空（首次创建、从旧版本升级、status 丢失）时，先从已存在的 `cmapi.Certificate.spec.issuerRef` 采纳，没有 Certificate 才用 flag。防止「升级 operator 顺手改了 flag」触发全量重签。
- 检测到 flag 当前值 ≠ `effectiveIssuerRef` 且 spec 未显式指定 → condition `IssuerDefaultDiverged=True`（**不参与 Ready**）+ 一次 Warning event + gauge。证书本身健康，不能标 NotReady。
- 切换 issuer 的方式：显式写 `spec.certificateTemplate.issuerRef`。不提供 `issuerRefPolicy` 字段（批量切换 = 批量写 spec）。

### 5.4 Secret 校验

全部通过才允许上传 / 应用：

1. `tls.crt`、`tls.key` 存在，PEM 可解析，leaf 可提取
2. 证书链连续（每张证书由后一张签发）
3. 公钥与私钥匹配
4. leaf SANs ⊇ `spec.certificateTemplate.dnsNames`（含 `commonName`）；不满足 → `SANsMismatch`
5. **临时证书拒绝**：`leaf 自签（Issuer == Subject）AND Certificate.Issuing == True` → `SelfSignedDuringIssuance`。只用合取，不单独拒绝自签证书——`SelfSigned` issuer 是合法用法。
6. 计算 SHA-256(DER) 指纹、`notBefore` / `notAfter`（**不使用** CAS 返回的 `EndDate`，它精度只到天）
7. **PEM 规范化**：不信任 Secret 里的原始文本，从解析出的 DER **重新编码**生成 `CertMaterial.CertPEM` / `KeyPEM`——leaf 在前、中间证书按链顺序紧随、证书之间无空行、64 字符/行、无多余 bundle 注释；私钥必须是未加密的 PKCS#1（`RSA PRIVATE KEY`）或 SEC1（`EC PRIVATE KEY`），加密私钥或无法识别的 PEM 块 → `SecretInvalid`。这是阿里云 CAS / CDN / FC3 共同的 PEM 规范（§2.2），在 operator 内一次做对，下游 provider 全部受益。

### 5.5 CAS 上传与幂等

- **命名**：`sanitize(metadata.name)[:50] + "_" + fingerprint[:12]`，`sanitize` 把 `[^A-Za-z0-9_]` 替换为 `_`。总长 ≤ 63。字符集须实测（§12.3）。
- **ClientToken**：`uid[:16] + fingerprint[:32]`（48 字符纯字母数字）。
- **write-ahead**：上传前先写 `status.pendingUpload{fingerprint, casName, clientToken, startedAt}`，成功后转正到 `current` 并清空。进程崩溃 / 响应丢失后重启：发现 `pendingUpload` 非空 → 用同一 `clientToken` 重试；若 CAS 不支持 token 幂等（待实测），兜底走 `ListUserCertificateOrder(Keyword=主域名)` 客户端按 `casName` 过滤。
- **错误分类**：`Throttling*` / `ServiceUnavailable` / `InternalError` / 网络超时 → 可重试（指数退避）；`InvalidAccessKeyId*` / `Forbidden` / `InvalidParameter*` → 不可重试，置 condition，长 requeue（等凭证 / spec 修复）。
- **出站限流**：按 `(accessKeyId, service)` 维度的 `golang.org/x/time/rate` 限流器包在 `CASClient` 上：list 通道 ≤ 8 QPS，upload / delete ≤ 50 QPS。

### 5.6 清理分支（finalizer `certs.bestheme.ac.cn/finalizer`）

```
 a. 存在 deletionTimestamp == nil 的 Binding 引用本证书
    → condition DeletionBlockedByBindings + Warning event，return（正在删除中的 Binding 不计入）
 b. 依据 **status**（current + history + pendingUpload）删除 CAS 证书（不依据 spec.uploadToCAS，
    否则 true→false 后删 CR 会留永久孤儿）；每个 certId 用 DeleteUserCertificate + ClientToken
    - 有界清理：--cleanup-grace-period（默认 15m）内指数退避重试
    - 超时后按 --cleanup-failure-policy（默认 Abandon）：
      Abandon → Warning event CleanupAbandoned + Error 日志（含 region / certId 列表）
                + counter aliyuncert_cleanup_abandoned_total{region,reason}，继续 c
      Block   → 保持 finalizer，持续重试
 c. 显式删除 cmapi.Certificate，**等待其 NotFound**（ownerRef 级联是异步的；
    若 Secret 先删而 Certificate 还在，cert-manager 会立刻重签并重建 Secret）
 d. 删除 Secret
 e. 摘 finalizer
```

CAS 放在 Certificate 之前只是就近安排，无正确性差异；唯一硬约束是 **Certificate 必须先于 Secret 死**。

部署文档约束：凭证 Secret 应放在与 CR **不同的 Argo CD Application**，避免 prune 时先于 CR 消失。

### 5.7 保留策略回收

保留 `keepLast` 代（current + history）。history 超出时，从最老开始回收，每一代必须同时满足：

1. `now - uploadedAt ≥ retention.minAge`
2. **live list**（`APIReader`，绕过 informer cache）所有引用本证书的 Binding：
   - 每个 Binding 的 `status.observedGeneration == metadata.generation`（已完成一次 reconcile）
   - 没有任何 Binding 的 `appliedFingerprint == 该代 fingerprint`
3. 任一不满足 → 跳过本轮，下次 resync 再试

这三重护栏针对的 TOCTOU：刚创建、`appliedFingerprint` 还为空的 Binding 会被 cache 版本误判为「没人用旧代次」。

---

## 6. 绑定 controller

### 6.1 Watch 与触发

- 主资源：`AliyunCertificateBinding`
- Watches：`AliyunCertificate` → field index `spec.certificateRef.name`（同 namespace）反查引用它的 Binding，证书 `status.current` 变化即唤醒
- 每次成功 reconcile 返回 `RequeueAfter: --drift-check-interval`（默认 1h）

### 6.2 Reconcile 步骤

```
 0. deletionTimestamp != nil → §6.5
 1. 取证书；不存在 → Ready=False/CertificateNotFound；Issued=False → Ready=False/CertificateNotReady，return
 2. **冲突仲裁**：field index 按 (target.type, region, identifier) 查同目标 Binding
    - 同目标 Binding 按 (creationTimestamp, UID) 排序，只有最小者允许写
    - 本 Binding 不是最小者 → Conflict=True/ConflictingBinding，不写云，return
    - cache 短暂不一致时，这个确定性规则保证最终收敛到唯一胜者
 3. **域名覆盖校验**：leaf SANs（RFC 6125 通配符规则）覆盖 target.domainName？
    否 → Ready=False/DomainNotCovered，不 Apply，不快速重试（硬失败）
 4. 解析凭证（Binding.credentialsRef > 证书.aliyun.credentialsRef），构造 provider client
 5. Provider.Observe(target)：
    - TargetNotFound → Ready=False，固定长 requeue 5m（域名可能由 Terraform 稍后创建）
    - **账号 fencing**：status.boundAccountId 非空且 ≠ observed.AccountID
      → Conflict=True/AccountMismatch，不写，return
    - observed.CurrentFingerprint == 证书 current 指纹 且 protocol 满足要求
      → 更新 lastObservedTime，Applied=True，return（短路只跳过写，不跳过 Observe）
    - observed.CurrentFingerprint == appliedFingerprint ≠ current → 正常轮换
    - observed.CurrentFingerprint ∉ {appliedFingerprint, current} → drift，
      记 counter + Warning event DriftCorrected，继续 Apply
 6. Provider.Apply(target, material)
 7. 成功 → appliedFingerprint = current，boundAccountId 首次固化，lastAppliedTime，Applied=True
    失败 → Applied=False/ApplyFailed，按错误分类退避
 8. Ready = Applied && !Conflict
```

**不回滚原则**：CAS 上传成功、FC3 应用失败时不回滚 CAS。多一张未被引用的证书无害（`keepLast` 会管住），而删除后重传会让重试变成上传 / 删除死循环。

### 6.3 FC3 provider 的 Apply

```
 1. GetCustomDomain(domainName)  → 完整对象（含 accountId、现有 certConfig、routeConfig 等）
 2. 构造完整 body：authConfig / corsConfig / routeConfig / tlsConfig / wafConfig / protocol 原样回填
 3. certConfig = { certName: material.CASName, certificate: material.CertPEM, privateKey: material.KeyPEM }
 4. ensureHTTPSProtocol=true 且 protocol 不含 HTTPS → protocol = "HTTP,HTTPS"
 5. UpdateCustomDomain(domainName, body)
```

read-modify-write 在「全量替换」和「部分合并」两种语义下都正确。已知限制：Get 与 Update 之间无已确认的乐观锁，是 last-write-wins；缓解是窗口极短、写入频率极低（正常一年 4–6 次 + drift 纠正）。

### 6.4 私钥保护

- `GetCustomDomain` 响应含明文私钥。SDK 之上的 `FC3Client` 层负责 redact：日志、error wrapping、event、status 中**绝不出现 PEM 片段**，只出现 `domainName` + 指纹前 8 位。
- 最高日志级别也不打印 request / response body。

### 6.5 清理分支（finalizer）

- `deletionPolicy: Orphan`（默认）→ 直接摘 finalizer，云侧不动。一个 `kubectl delete` 不应打穿生产 HTTPS。
- `deletionPolicy: Unbind` → `Observe`；目标上的证书指纹 == 本 Binding 的 `appliedFingerprint` 时才清空 `certConfig`（**只解绑自己的证书**），若 protocol 为纯 HTTPS 则降为 HTTP（否则域名不可用）；指纹不是我们的 → 不动，直接摘 finalizer。同样受 `--cleanup-grace-period` / `--cleanup-failure-policy` 约束。

---

## 7. Provider 接口

```go
package provider

// Target 是 provider 无关的目标标识；通用层用 (Type, Region, Identifier) 做索引与冲突仲裁。
type Target struct {
    Type       string // "FC3CustomDomain"
    Region     string
    Identifier string // FC3: domainName
    Spec       any    // 指向 CRD 内嵌 struct，provider 自行断言
}

// CertMaterial 由通用层准备，provider 只读。
type CertMaterial struct {
    Fingerprint string   // SHA-256(DER) 小写 hex
    CertPEM     []byte   // leaf + intermediates
    KeyPEM      []byte   // 按 spec 编码，默认 PKCS#1
    CertID      *int64   // CAS certId；ReferencesCertByID=false 的 provider 忽略
    CASName     string
    NotAfter    time.Time
    DNSNames    []string
}

// 刻意没有 Exists 字段：「目标不存在」是一个错误（CodeTargetNotFound），不是一种观测
// 结果——Observe 返回 nil error 就已经意味着目标在。
type ObservedState struct {
    CurrentFingerprint string // 目标实际证书的指纹；"" = 无证书
    Protocol           string
    AccountID          string // fencing
}

type Capabilities struct {
    ReferencesCertByID     bool // true = 目标存 certId（CDN/CLB）；false = 内联 PEM（FC3）
    SupportsProtocolSwitch bool
    RequiresCASUpload      bool // true ⇒ 强制 uploadToCAS，用户不可关
}

// Client 是通用层构造好、交给 provider 使用的云客户端；具体类型由 provider 自行断言
// （FC3 断言为 aliyun.FC3Client），与 Target.Spec 同构。
type Client any

type Provider interface {
    Name() string
    Capabilities() Capabilities
    Observe(ctx context.Context, t Target, c Client) (ObservedState, error)
    Apply(ctx context.Context, t Target, c Client, m CertMaterial, o ApplyOptions) error
    Cleanup(ctx context.Context, t Target, c Client, policy DeletionPolicy) error
}

type ApplyOptions struct {
    EnsureHTTPSProtocol bool
    PreviousFingerprint string // 供「替换需指定旧值」的未来 provider；FC3 忽略
}

// provider 负责错误分类；通用层据此决定退避 / 停止 / condition reason
type ProviderError struct {
    Code      string
    Retryable bool
    Reason    string
    Err       error
}
```

**职责边界**

| 关注点 | 归属 |
|---|---|
| 幂等判断的真相来源 | `Observe`（provider） |
| 幂等快路径短路 | 通用层 status 比对，但被 `drift-check-interval` 周期性打破 |
| SANs 覆盖校验 | 通用层 |
| CAS 上传 | 通用层；`RequiresCASUpload` ⇒ 强制 |
| 重试 / 退避 / status / event | 通用层；provider 只返回分类过的 error |
| 凭证与 client 构造 | 通用层；provider 收到已构造的 client |

**三个方法的第三形参为什么是 `Client`（`any`）而不是 `Credentials`**：不同 provider 需要的是不同的云 client（FC3 要 `FC3Client`，将来的 CDN / CLB 要各自的），而注册表把它们放在同一个 `map[string]Provider` 里——**注册表是异构的，不能给 `Provider` 加类型参数**，否则这个 map 无法成立。所以接口收的是一个已经构造好的 `Client`，由 provider 自己断言成它要的具体类型（断言失败返回 `CodeInvalidClient`，属接线错误而非运行时错误），而不是原始 `Credentials`；凭证解析与 client 构造留在通用层，与上表最后一行一致。

**注册**：`init()` 中 `provider.Register(&fc3.Provider{})`，通用层 `map[string]Provider`。新增 provider = 一个包 + 一个 CRD 内嵌字段 + 一条 CEL 规则，不动状态机。新增 provider 需 CRD 与 operator 同版本发布（sync-wave 已覆盖）。

**`uploadToCAS` 与 provider 的关系**：`RequiresCASUpload=true` 的 provider 出现时，即便 spec 写了 `uploadToCAS: false` 也强制上传，并在 condition 中说明。

---

## 8. 凭证、RBAC 与 RAM

### 8.1 Operator ClusterRole

```yaml
rules:
- apiGroups: ["certs.bestheme.ac.cn"]
  resources: ["aliyuncertificates", "aliyuncertificatebindings"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: ["certs.bestheme.ac.cn"]
  resources: ["aliyuncertificates/status","aliyuncertificatebindings/status"]
  verbs: ["get","update","patch"]
- apiGroups: ["certs.bestheme.ac.cn"]
  resources: ["aliyuncertificates/finalizers","aliyuncertificatebindings/finalizers"]
  verbs: ["update"]
- apiGroups: ["cert-manager.io"]
  resources: ["certificates"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get","delete"]              # 不需要 list/watch（§5.1）；delete 只因 cert-manager 默认不 own Secret
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create","patch"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get","list","watch","create","update","patch"]
```

诚实的表述：cluster-wide `secrets: get` 在知道名字的前提下仍等价于读遍集群，机密性上没有质变；真实收益是**零缓存**和审计噪音下降。要真正收窄，用 `--watch-namespaces` 限定范围并改用 namespace 级 Role。

### 8.2 凭证处理

- 凭证 Secret 走 `Client`（已 `DisableFor`，等同直读），不缓存 Secret 本身。
- SDK client 缓存 key = `(namespace, name, resourceVersion)`——否则轮换后的 AK 直到 Pod 重启才生效。
- 凭证错误（`InvalidAccessKeyId*`、`Forbidden`）不可重试，置 `CredentialsInvalid`，长 requeue。

### 8.3 阿里云 RAM 最小权限

两份策略，对应 `uploadToCAS` 两态。

**含 CAS（uploadToCAS=true）**：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "yundun-cert:UploadUserCertificate",
        "yundun-cert:DeleteUserCertificate",
        "yundun-cert:ListUserCertificateOrder"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": ["fc:GetCustomDomain", "fc:UpdateCustomDomain"],
      "Resource": ["acs:fc:cn-hangzhou:<accountId>:custom-domains/api.timehorse.bestheme.ac.cn"]
    }
  ]
}
```

**仅 FC3（uploadToCAS=false）**：只保留第二条 Statement。

要点：

- `yundun-cert:*` **无法资源级收窄**——operator 的 AK 能删账号下任意上传证书。这是不可回避的爆炸半径，必须写进 README，并建议独立 RAM 子账号 + 独立 AK。这也是 `uploadToCAS` 开关存在的理由。
- `fc` 支持逐域名 ARN 授权，必须用上。
- **不授 `yundun-cert:GetUserCertificateDetail`**（返回私钥，本设计不需要）。

---

## 9. Operator 配置（flags）

| flag | 默认 | 说明 |
|---|---|---|
| `--default-issuer-name` / `--default-issuer-kind` / `--default-issuer-group` | 空 / `Issuer` / `cert-manager.io` | 与 cert-manager 同名；仅在 CR 未指定且无已 pin 值时参与 |
| `--certificate-resync-interval` | `1h` | 证书 controller 周期 resync（Secret 漂移检测承重通道） |
| `--drift-check-interval` | `1h` | 绑定 controller 周期 Observe |
| `--cas-probe-interval` | `12h` | CAS 存在性探测节流 |
| `--issuance-stall-threshold` | `6h` | `Issuing=True` 持续超过即 `IssuanceStalled` |
| `--cloud-call-timeout` | `30s` | 所有阿里云调用的 ctx 超时（独立于 leader election） |
| `--cleanup-grace-period` | `15m` | finalizer 内云侧清理的有界时长 |
| `--cleanup-failure-policy` | `Abandon` | `Abandon` / `Block` |
| `--watch-namespaces` | 空（全集群） | 逗号分隔；配合 namespace 级 Role |
| `--leader-elect` | `false`（部署清单显式传 `--leader-elect` 开启） | `LeaderElectionReleaseOnCancel: true` |

---

## 10. 可观测性

### 10.1 Metrics（controller-runtime 自带指标之外）

```
# 到期与新鲜度——最重要的两个
aliyuncert_certificate_not_after_timestamp_seconds{namespace,name}     gauge
aliyuncert_binding_applied_age_seconds{namespace,name,provider}        gauge
  # 滞后时长：证书 CR 的 status.current 推进后，Binding 尚未把该代应用到目标的持续秒数；
  # 已同步时为 0。独有失败模式：续期成功但没推到线上。必须告警。

# 状态
aliyuncert_certificate_ready{namespace,name}                           gauge 0/1
aliyuncert_certificate_issuance_stalled{namespace,name}                gauge 0/1
aliyuncert_issuer_default_diverged{namespace,name}                     gauge 0/1
aliyuncert_binding_ready{namespace,name}                               gauge 0/1
aliyuncert_binding_conflict{namespace,name}                            gauge 0/1

# 动作计数
aliyuncert_cas_upload_total{result}                                    counter
aliyuncert_cas_delete_total{result}                                    counter
aliyuncert_binding_apply_total{provider,result}                        counter
aliyuncert_binding_drift_detected_total{provider}                      counter
aliyuncert_cleanup_abandoned_total{region,reason}                      counter
aliyuncert_certmanager_certificate_recreated_total{namespace,name}     counter  # 应恒为 0（LE 速率限制护栏）

# API
aliyuncert_aliyun_api_requests_total{service,action,code}              counter
aliyuncert_aliyun_api_duration_seconds{service,action}                 histogram
```

**基数约束**：绝不把 `fingerprint` / `certId` 放进 label。`result` 用固定小集合（`success` / `throttled` / `error`）。

### 10.2 Events

只在**状态跃迁**时发，Message 为固定文案、不含变量（避免 event 洪水，K8s 只聚合
Reason+Message 完全相同的事件）。下表是证书 controller 实际发出的全部事件，与
`internal/controller/*.go` 的 `Recorder.Event` 调用一一对应。

| 类型 | Reason | 发出点 | 触发条件 |
|---|---|---|---|
| Warning | `CertificateRecreated` | `aliyuncertificate_controller.go` | 已建过的 cert-manager Certificate 又被创建了一次（LE 配额护栏，应恒为 0） |
| Warning | `IssuanceStalled` | `aliyuncertificate_controller.go` | `Issuing=True` 超过 `--issuance-stall-threshold` |
| Warning | `SelfSignedDuringIssuance` | `aliyuncertificate_controller.go` | Secret 里是自签临时证书且 `Issuing=True`；`loadMaterial` 的其它 reason 只置 condition 不发事件 |
| Warning | `IssuerDefaultDiverged` | `aliyuncertificate_controller.go` | 固化的 issuer 与当前 `--default-issuer-*` 不一致 |
| Warning | `UploadFailed` | `aliyuncertificate_controller.go` | `handleCloudError` 的 default 分支（不可重试的云错误）；Retryable 与 Auth 分支只置 condition，不发事件 |
| Warning | `ReclaimFailed` | `retention.go` → `aliyuncertificate_controller.go` | 保留策略回收失败；不改 `Uploaded` / `Ready` |
| Warning | `ProbeFailed` | `probe.go` | CAS 存在性探测失败；不改 condition、不中断本轮 |
| Warning | `CASCertificateMissing` | `probe.go` | 探测发现 `current.certId` 已不在 CAS，将重新上传 |
| Warning | `DeletionBlockedByBindings` | `deletion.go` | 删除被存活的 Binding 阻塞 |
| Warning | `CleanupAbandoned` | `deletion.go` | 有界清理超时且策略为 `Abandon`，CAS 侧留下孤儿证书 |
| Normal | `Reclaimed` | `retention.go` | 一代旧证书被回收 |
| Normal | `Uploaded` | `upload.go` | 新代次上传成功 |

**与早期草案的差异（以代码为准）**：`DeletionBlocked` 的实际 reason 是
`DeletionBlockedByBindings`（`api/v1alpha1/conditions.go` 的 `ReasonDeletionBlocked`
常量值）；`CertificateRecreated`、`CASCertificateMissing`、`ProbeFailed`、
`ReclaimFailed`、`IssuanceStalled` 五个是实现期新增的。

**绑定 controller 的事件**由 Plan 2 交付，reason 常量已在 `api/v1alpha1/conditions.go`
中定义：

| 类型 | Reason | 发出点 | 触发条件 |
|---|---|---|---|
| Normal | `Applied` | `binding_apply.go` | `Applied` condition 由「不存在 / False」跃迁到 True（健康对象每轮都发就成了噪声） |
| Warning | `ApplyFailed` | `binding_apply.go` | Apply 失败，且失败 reason 相对上一轮发生了变化 |
| Warning | `ObserveFailed` | `binding_observe.go` | Observe 失败，且失败 reason 相对上一轮发生了变化 |
| Warning | `DriftCorrected` | `binding_observe.go` | 观测到的指纹既不是 `appliedFingerprint` 也不是 current（漂移），将纠正 |
| Warning | `CleanupAbandoned` | `binding_deletion.go` | `Unbind` 解绑超出 `--cleanup-grace-period` 且策略为 `Abandon` |

「reason 变化时才发」与表头那句「只在状态跃迁时发」是同一条规则：一个持续失败的目标不该
每个 `--drift-check-interval` 就刷一条事件。跃迁的比较基准来自本轮开始前 API server 上的
那一份 status——`ApplyFailed` 比 `Applied` condition 的 reason，`DriftCorrected` 比
`status.driftedFingerprint`。`ObserveFailed` 额外受一条约束：`Applied` condition 尚不存在
时（从没成功绑过的对象）不凭空造 `Applied=False`，因而也没有可收敛的基准，这一轮不发事件。

`NoConflict` 是 `Conflict=False` 的 condition reason，**不发事件**（正常态无需广播）。

### 10.3 建议随附的 PrometheusRule

两条独立告警，缺一不可：

- `aliyuncert_certificate_not_after_timestamp_seconds - time() < 7*86400`（证书即将过期）
- `aliyuncert_binding_applied_age_seconds > 86400`（证书更新了但线上没跟上）。按滞后语义，该式子表示「证书已推进超过一天而线上仍是旧代」——同步完成时该指标归 0，因此不会对健康证书误报。

  **原式子里的 `and on (namespace, name) aliyuncert_certificate_ready == 1` 守卫已去掉**（Ruling P3-R42，`config/prometheus/prometheusrule.yaml` 为准）。理由：两个指标的 `namespace` / `name` 指的不是同一个对象——`aliyuncert_binding_applied_age_seconds` 打的是 **Binding** 的 namespace/name，`aliyuncert_certificate_ready` 打的是 **AliyunCertificate** 的（见 `internal/controller/metrics.go`）。本仓库自带样例就是 Certificate `timehorse-api` 配 Binding `timehorse-api-fc3`，`on (namespace, name)` 根本匹配不上，整条表达式**恒为空**，这条本节点名必配的告警会静默失效。

  去掉守卫是安全的：它本来就是冗余而非承重。`appliedLag`（`internal/controller/binding_status.go:139-141`）在证书 `status.current` 为 nil 或 `fingerprint` 为空时直接返回 0，未签发的证书产生不了非零 lag。更完备的做法是给 `bindingAppliedAge` 加一个 `certificate` label 再用 `group_left` 关联回 `aliyuncert_certificate_ready`，那要改 `metrics.go`，尚未做。**不要因为「少了个守卫」把 `on (namespace, name)` 加回来**——那会让这条告警重新变成恒空。

以及 `increase(aliyuncert_certmanager_certificate_recreated_total[1d]) > 0`、`increase(aliyuncert_cleanup_abandoned_total[1d]) > 0`。

---

## 11. 部署

- **不做 OLM bundle**（OLM 与 Argo CD 是两套互斥的所有权模型）。保留 Operator SDK 自带的 `make bundle`，不进 CI 必过项。
- **Argo CD**：`ServerSideApply=true`（CRD 体积 + 262144 字节注解上限）；CRD 与 controller 分两个 Application 或用 sync-wave（CRD −2、RBAC/SA −1、Deployment 0、CR 1）；`ignoreDifferences` 忽略 CRD 与 CR 的 `/status`；**绝不对 CRD 用 `Replace=true`**（会删 CR）；凭证 Secret 放独立 Application。
- **CRD 声明 `subresources.status`**（Operator SDK 默认），否则 Argo 会把 status 当 drift 无限 sync。
- **副本数 1** + leader election。证书轮换是天级事件，operator 不可用 60 秒业务影响为零；HA 换来的是并发写云资源的风险。
- **OpenShift SCC（`restricted-v2`）**：`runAsNonRoot: true`、`seccompProfile: RuntimeDefault`、`allowPrivilegeEscalation: false`、`readOnlyRootFilesystem: true`、`capabilities.drop: [ALL]`；**不设 `runAsUser` / `runAsGroup`**（kubebuilder 脚手架的 `65532` 会被 restricted-v2 拒绝）。
- **镜像**：`gcr.io/distroless/static:nonroot`，`linux/amd64` + `linux/arm64`。
- **README 前置条件**：
  - external-dns 会把 cert-manager 的 `_acme-challenge.*` TXT 当无主记录删掉。需 `--policy=upsert-only`，或 `--exclude-domains` 排除 `_acme-challenge`，或独立 zone。
  - 强烈建议先用 LE staging issuer 验证（速率限制：50 证书/注册域/周，5 重复证书/周）。
  - 最低 k8s 1.25 / OpenShift 4.12（CEL）。

---

## 12. 测试策略

### 12.1 单元测试

- 命名与 sanitize、ClientToken 生成、指纹计算
- PEM 解析、链校验、公私钥匹配、SANs 通配符匹配（RFC 6125）
- 自签 + Issuing 合取逻辑
- 错误分类
- 冲突仲裁排序

### 12.2 envtest（装 cert-manager CRD YAML，手工写 `Certificate.status`，不跑真 cert-manager）

- CRD 校验：`target` 不可变、union 一致性、`keepLast >= 1`、必填字段
- 证书状态机全路径：无 issuer / issuer 来自 flag / Pin 生效（改 flag 后 Certificate 的 issuerRef 不变）/ bootstrap 从已有 Certificate 采纳 / Certificate 未 Ready / Secret 缺失 / 自签 + Issuing / 公私钥不匹配 / SANs 不覆盖 / 正常上传 / 指纹未变短路 / 轮换推进 history / `uploadToCAS=false`
- 绑定状态机：证书未 Ready / 目标不存在 / 冲突仲裁 / 域名不覆盖 / 幂等短路 / drift 纠正 / accountId 不匹配 / `Unbind` 只解绑自己的证书
- finalizer：CAS → Certificate → 等 NotFound → Secret 顺序（envtest 无 GC 恰好逼出显式删除路径）；阻塞删除只计活着的 Binding；`cleanup-grace-period` 耗尽后的 Abandon / Block
- **轮换时序**（fake aliyun client + fake clock）：
  1. 建证书 + 2 个 Binding，等 gen1 上传并绑定
  2. 改 Secret 模拟续期 → gen2
  3. 断言 gen2 上传、gen1 进 history、两个 Binding 仍在 gen1 时 gen1 **不删**
  4. 一个 Binding 推进到 gen2、另一个仍 gen1 → gen1 **仍不删**
  5. 两个都推进但 `minAge` 未到 → **仍不删**
  6. 推进 fake clock 过 `minAge` → gen1 被删
- **故障注入**（`pkg/aliyun/fake`）：上传 `Throttling` → 退避成功；上传超时但服务端成功 → 重试 `ClientToken` 命中同一 certId；上传成功但写 status 失败 → 重启 → `pendingUpload` 恢复无重复证书；FC3 Get 成功 Update 失败 → 不回滚 CAS

阿里云 client 的抽象：在 SDK 之上包**窄接口**，不 mock SDK 生成的 struct。

```go
type CASClient interface {
    Upload(ctx context.Context, name string, certPEM, keyPEM []byte, clientToken string) (certID int64, err error)
    Delete(ctx context.Context, certID int64, clientToken string) error
    FindUploaded(ctx context.Context, domainHint string) ([]CertSummary, error)
}
type FC3Client interface {
    GetCustomDomain(ctx context.Context, domain string) (*CustomDomain, error)
    UpdateCustomDomain(ctx context.Context, domain string, in *UpdateCustomDomainInput) error
}
```

### 12.3 真实环境集成测试（必测清单；结论回填本表与代码注释）

编号是契约：`test/integration/` 的探针按这里的编号 `Record`，`test/integration/RESULTS.md`
按同一套编号回填，因此**已定的编号不再改动**，新问题只在表末追加。已核实的行**以 `RESULTS.md`、或本行明确标注的其它一手依据为准**（例如 #9 的前半来自 Go SDK v4 内置的 `EndpointMap`，#8 的两条范围限定来自 SDK 的 `Status` 枚举语义——这类依据必须在行内写明「推导」二字），不在此处推断超出依据的结论。

| # | 待核实 | 影响 |
|---|---|---|
| 1 | CAS 与 FC3 对 PKCS#1 / PKCS#8 / `EC PRIVATE KEY` 私钥的接受情况（**未画删除线：只关闭了 CAS 一半**）。**CAS 侧已核实**：PKCS#1 RSA、SEC1 EC、PKCS#8 三种未加密编码**一律接受**（各自上传成功）；带 `Proc-Type: 4,ENCRYPTED` 头的私钥块被拒，错误码 `PrivateKeyFormatException`（Permanent）——CAS 报的是格式错误而非配对错误。**FC3 侧未测**（`CertConfig.privateKey` 的接受情况由 FC3 探针补测，仍记在本行下） | 决定 6 的默认编码；`privateKey.encoding: PKCS8` 是否要在 CRD 层直接拒绝。CAS 侧已确定三种编码都不需要转换 |
| 2 | `UpdateCustomDomain` 全量替换 vs 部分合并（构造含 routeConfig+wafConfig+tlsConfig 的域名，只提交 certConfig）。**仍未核实**：探针已写好（`test/integration/fc3_test.go`），但它需要一个可被改写 `certConfig` 的 `FC3_TEST_DOMAIN`，实测那一轮没有配（`RESULTS.md` #2） | read-modify-write 两种语义下都安全（§6.3 走的就是 read-modify-write），所以这一行不阻塞实现，只决定 last-write-wins 的风险大小 |
| 3 | ~~CAS `ClientToken` 语义（同 token 重复上传返回同 certId？报错？有效期？）~~ **已核实**：`ClientToken` **不做上传幂等**——同 token、同 Name 重传直接报 `NameRepeat`（Permanent），而不是回放首次的 certId | write-ahead 幂等能否落地；结论见 `test/integration/RESULTS.md`。既然不幂等，write-ahead 的崩溃恢复 100% 依赖 `isDuplicateName` → `findByName` 认领既有 certId 这条路径 |
| 4 | ~~CAS `Name` 是否接受 `-` / `.`~~ **已核实**：两者都**接受**，且**原样保存、不做归一化**（回查云上存的名字与提交值逐字相同） | 命名 sanitize 规则；`findByName` 可以按提交的名字精确比对，无需考虑云侧改名 |
| 5 | LE 链（leaf + intermediate，无 root）FC3 与 CAS 是否都接受、是否要求带根证书、顺序是否敏感（**未画删除线：只关闭了 CAS 一半**）。**CAS 侧已核实**：leaf + 其签发 CA（两块）**接受**，**仅 leaf 也接受**（故不要求带根）；把 CA 放在 leaf 前面**被拒**（`NotMatch.CertificateAndPrivateKey`，Permanent）——**顺序敏感，leaf 必须在首位**。这两条合起来说明 LE 的「leaf + intermediate、无 root」形状在 CAS 可用。**FC3 侧未测**（由 FC3 探针补测，仍记在本行下） | §5.4 第 7 条 PEM 规范化的输出形状：只需保证 leaf 在首位，不需要补根 |
| 6 | ~~给 TLS Secret 追加指向 AliyunCertificate 的 ownerRef，cert-manager 的 SSA 是否保留~~ **已核实**：**保留——重签后 ownerRef 仍在**（实测于 2026-09-05，cert-manager v1.20.3，SelfSigned Issuer，改 `dnsNames` 触发重签并已观测到新 SAN 生效，见 `RESULTS.md` #6） | 若保留，可消掉 `secrets: delete` 和 finalizer 顺序难题。已核实保留；据此简化 RBAC/finalizer 属于后续设计变更，本轮未做 |
| 7 | ~~Secret 被替换为「符合 spec 的不同合法证书」时 cert-manager 是否重签 / bump `revision`~~ **已核实**：**会重签并 bump `revision`，watch Certificate 即可发现**（实测于 2026-09-05，cert-manager v1.20.3，SelfSigned Issuer，替换前 `revision=1`、基线在 `status.revision` 就绪后才取，见 `RESULTS.md` #7） | 不缓存 Secret 决定的盲区大小。盲区因此收窄为「替换后 cert-manager 不 bump `revision`」这一情形——本轮未观测到它发生 |
| 8 | CAS 单账号上传证书数量配额（**未画删除线：配额上限这一半没关**）。**部分核实**：空 `Keyword` 列举当前返回 **0 张**已上传证书（附带的两条限定——`Status` 留空的列举不含已过期证书、配了 `ALIYUN_RESOURCE_GROUP_ID` 时仅限该资源组——是**据 SDK 枚举语义推导的，未经实测**：0 张证书的账号里这个排除在构造上就不可观测，见 §2.2）。**配额上限本身未实测**——撞上限会污染账号，需在控制台「数字证书管理服务 → 证书管理 → 上传证书」页核对账号总量与上限 | `cleanup_abandoned_total` 是否必须配告警。列举语义**据 SDK 枚举推导、未经实测**：若推导成立，存在性探测按 Name 客户端过滤时要意识到过期证书不在默认结果里 |
| 9 | ~~CAS endpoint 是否 region 化~~ **已核实**（Go SDK v4 内置 `EndpointMap`）：全部中国区域及 `eu-west-1` / `us-east-1` / `us-west-1` 映射到同一个 `cas.aliyuncs.com`；`ap-southeast-1` / `ap-southeast-2` / `ap-northeast-1` / `eu-central-1` / `me-central-1` / `ap-south-1` / `me-east-1` 各有独立 endpoint（`cas.<region>.aliyuncs.com`）。结论：CAS **部分 region 化**，`casRegion` 字段保留；实现上把 `casRegion` 作为 SDK `RegionId` 传入，由 SDK 的 `EndpointRule=regional` 自动选 endpoint，`endpointOverride` 非空时直接覆盖。**跨 endpoint 可见性也已核实**：两个 endpoint 的证书集合**互相隔离（双向验证）**——同一份 PEM 在 `cn-hangzhou` 与 `ap-southeast-1` 分别上传得到两个不在同一量级的 certId，各自在对方的列举里都看不见，是**两套独立的 ID 空间**而非复制延迟 | `casRegion` 语义已定。隔离意味着 `casRegion` 一旦改变，`status.current.certId` 在新 endpoint 上必然查不到，存在性探测会重新上传一次——这是预期行为，不是 bug |
| 10 | FC3 API 账号级频控阈值与 Throttling 错误码，**特别是错误码是否以 `Throttling` 开头**。**仍未核实**：阈值**刻意不测**——触发账号级频控只能对真实云连续打满请求，会波及同账号的其它调用，违反探针「不污染账号」的纪律；且 `pkg/aliyun` 的 `LimitFC3` 是 5 QPS / burst 1 的客户端限流，探针先被自己限住，摸不到云侧阈值。错误码这一半留给只读探针偶遇限流时顺带记录，实测那一轮未偶遇（`RESULTS.md` #10） | drift 1h 在数百 Binding 下是否安全；错误分类表。不以 `Throttling` 开头则 `aliyun.Classify` 会把限流判成不可重试，退避逻辑失效 |
| 11 | ~~cert-manager 在 Secret 上打的 `cert-manager.io/certificate-name` 等注解是否稳定存在~~ **已核实**：`certificate-name` / `issuer-name` / `issuer-kind` / `issuer-group` **四个注解全部存在**（实测于 2026-09-05，cert-manager v1.20.3，SelfSigned Issuer，见 `RESULTS.md` #11） | `SecretNameConflict` 判定依据。四个注解齐全，可作为判定依据 |
| 12 | ~~CAS `Keyword` 对通配符域名（`*.example.com`）的匹配行为~~ **已核实**：`Keyword` 对证书域名字符串做**任意子串匹配**，且**不做 DNS 通配符展开**。SAN 为 `*.it.integration.invalid` 的证书，用 `*.it.integration.invalid`、`it.integration.invalid`、`integration`、甚至非标签边界的 `ntegratio` 都能查到，而通配符本应覆盖的 `probe.it.integration.invalid` **查不到**。这同时排除了 DNS 通配符语义、后缀匹配、前缀匹配、按标签对齐的包含四种候选规则 | 通配符证书的首个 SAN 会被原样当 Keyword 传给 `ListUserCertificateOrder`；匹配不到就会让存在性探测持续误判「证书丢了」并反复重传。结论是安全的：原样传 SAN 一定能命中自己 |
| 13 | ~~CAS 同名不同 `ClientToken` 上传返回的真实错误码~~ **已核实**：`NameRepeat`（Permanent）。**不在**生产代码原先猜的三个候选码里，已追加进 `internal/controller/upload.go` 的 `isDuplicateName` | 认错则 `DuplicateName → findByName` 的认领路径失效，write-ahead 崩溃恢复会退化成反复失败的上传 |
| 14 | ~~FC3 `GetCustomDomain` 对**不存在的域名**返回的错误码与 HTTP 状态~~ **已核实**：错误码 `DomainNameNotFound`、HTTP **404**（实测于 2026-09-05（UTC，`RESULTS.md` 生成时间见文件头），见 `RESULTS.md` #14）。`aliyun.ClassOf` 据此把它归为 `ClassNotFound`，因此 `pkg/aliyun/errors.go` 的 `classifyCode` **无需修改**，绑定 controller 的「域名不存在 → 5m 定时重试」分支按预期工作。对照名（非法格式的 `it-absent-c7a962243765.integration.invalid`）返回**同一个码**，说明该码与域名格式无关 | 绑定 controller 的 Observe 靠 `ClassNotFound` 区分「目标不存在」与「调用失败」；分错会把不存在的域名当成可重试故障无限重试。已核实认得出：`classifyCode` 现有的 `Contains(code, "NotFound")` 与 `status == 404` 两条规则都命中，不必为 FC3 追加任何条件性分支 |

**上表唯一没有 `RESULTS.md` 行的是 #5 的 FC3 一半**：FC3 的链形状用例与私钥编码用例同在 `TestFC3CertConfigEncodings` 里，而缺 `FC3_TEST_DOMAIN` 时的 `requireFC3Domain` 把跳过记在了 **`#1`** 名下（`test/integration/fc3_test.go`），于是 `RESULTS.md` 里一行 `#5` 的 FC3 记录都没有。其余各行——包括 #2 / #10 与 #1 的 FC3 一半——的「仍未核实」与其原因都能在 `RESULTS.md` 里逐行找到；#14 已在 2026-09-05 那轮落了结论，同样逐行可查。

**FC3 侧的三项探测分别归到哪一行**（刻意不新开编号——同一个问题两个编号会让 `RESULTS.md` 的回填对不上）：`GetCustomDomain` 对不存在域名的错误码归 **#14**（新增）；限流错误码是否以 `Throttling` 开头归 **#10**（既有行，措辞已补全）；`CertConfig` 对 PKCS#1 / EC 私钥的接受情况归 **#1**、对 LE 链形状（leaf+intermediate、仅 leaf）的接受情况归 **#5**（两行本来就写的是「FC3 与 CAS 各自 / 都」，只是至今只测了 CAS 一侧）。

---

## 13. 安全与威胁模型

- **私钥流经 operator 内存**并写入 FC3 API 请求体；`GetCustomDomain` 响应也含私钥。能读 operator 日志 / 内存 / `oc exec` 的主体 = 持有所有被管证书私钥。建议：operator 独立 namespace、限制 `exec`/`debug` 权限、日志不采集 debug 级。
- **CAS 权限无法收窄**：泄露 AK 可删账号下任意证书。建议独立子账号 + 独立 AK；FC3-only 用户用 `uploadToCAS: false` 彻底不授 `yundun-cert:*`。
- **凭证同 namespace 约束**由类型系统保证，无跨 namespace 逃逸。
- **不越权改线上配置**：`ensureHTTPSProtocol` 默认 false；`deletionPolicy` 默认 Orphan；drift 纠正只动 `certConfig`。

---

## 14. 已知限制与风险

1. FC3 Get/Update 之间无已确认的乐观锁，last-write-wins（§6.3）。
2. `yundun-cert:*` 无法资源级收窄（§8.3）。
3. 不缓存 Secret 的盲区：Secret 被替换为合法但不同的证书且 cert-manager 不 bump revision 时，最多延迟一个 resync 周期（1h）才被发现（§12.3 #7）。**已实测收窄**：Secret 被替换为另一张合法证书时 cert-manager **会重签并 bump revision**，watch Certificate 即可发现，所以盲区只剩「替换后不 bump revision」这个理论情形——本轮未观测到它发生，也没有证据排除它。
4. ~~CAS 命名字符集、ClientToken 语义、region 化均待实测~~ 三项均已实测（§12.3 #3 / #4 / #9）：`ClientToken` **不提供上传幂等**，write-ahead 的崩溃恢复已按预案退化为 `DuplicateName → findByName` 的认领路径（`ListUserCertificateOrder`，QPS 10）。
5. `Abandon` 清理策略可能在 CAS 留下孤儿证书（有计数器和 event，需人工清理）。
6. cert-manager 依赖是编译期的：pin module 版本，README 写明最低支持的 cert-manager 版本。
7. CAS 证书名不含 namespace（`sanitize(CR名)[:50] + "_" + fingerprint[:12]`）。撞名的前提是两个不同 namespace 的同名 CR 持有完全相同的 leaf DER，cert-manager 正常签发不会产生这种形状；兜底是 `DuplicateName → findByName` 认领既有 certId，以及 12h 的存在性探测。**引入 `ReferencesCertByID: true` 的 provider（CDN / CLB / ALB）之前必须重新评估**——那时 certId 悬空的代价会大得多。
8. `record.EventRecorder`（旧 events API）在 controller-runtime v0.24 已弃用。迁移到 `events.EventRecorder` 要改动全部事件调用点（证书侧 12 处 + 绑定侧 4 处）并为每条事件补一个 `action` 参数，属于行为变更，暂以 `.golangci.yml` 的一条排除规则挂起。

---

## 15. 未来演进

- **新 provider**（CDN / DCDN / CLB / ALB）：这些引用 CAS `certId`，`Capabilities{ReferencesCertByID: true, RequiresCASUpload: true}`；保留策略的守卫和 CAS 存在性探测的「重新上传 + 触发 Apply」分支为此而设。
- **RRSA / OIDC**：Secret 契约已预留 key，`credentials-go` provider chain 直接支持。
- **defaulting webhook**：如需「只填域名」体验（issuerRef 由 webhook 填），可后补，不改 CRD schema。
- **v1beta1**：`spec.target` 形状若需改变，上 conversion webhook；不留 `RawExtension` 逃生舱。

---

## 16. 决策记录

| # | 决策 | 备选 | 理由 |
|---|---|---|---|
| D1 | 两个 CRD | 单 CRD 内联 targets | 证书与绑定生命周期不同；用户明确要求 |
| D2 | Go + Operator SDK | Kubebuilder / Quarkus JOSDK / Kopf | 生态、cert-manager Go 类型直接复用、OpenShift |
| D3 | provider 为 Go interface 内嵌 | 独立 webhook / 不抽象 | 抽象几乎免费，且逼着现在把 `spec.target` 设计成可扩展 union |
| D4 | `certificateTemplate` 自有结构体 | 嵌入 `cmapi.CertificateSpec` / RawExtension | 避免 `secretName` 必填、CRD 膨胀、无意义字段；已反复一次 |
| D5 | `issuerRef` 可选 + flag 分层默认，Pin 语义 | 必填 / Follow / `issuerRefPolicy` 字段 | 与 cert-manager shim 同构；Pin 防全集群重签；不加字段（YAGNI） |
| D6 | 凭证 CRD 内 `credentialsRef`，同 namespace | 全局单一 / Provider CRD / 全局默认+覆盖 | 多账号多 region；权限跟 namespace 走 |
| D7 | `keepLast` 默认 2，`minAge` 24h | 3 / 由 provider 驱动 | FC3 下旧代次无回滚价值；留一份排查 |
| D8 | `uploadToCAS` 默认 true 可关 | 永远上传 / provider 自动决定 | 满足「FC3-only 且不给删证书权限」的合理诉求；默认保持原意 |
| D9 | `target` 不可变 | 可变 + 清理旧目标 | 状态机复杂度翻倍换一个罕见场景 |
| D10 | Binding `deletionPolicy` 默认 Orphan | Unbind | `kubectl delete` 不应打穿生产 HTTPS |
| D11 | 不 watch / 不缓存 Secret | label selector 收窄 cache | 零 Secret 副本；RBAC 去掉 list/watch；resync 1h 承重 |
| D12 | finalizer：CAS → Certificate → 等 NotFound → Secret | 先 Certificate | 唯一硬约束是 Certificate 先于 Secret 死；CAS 位置无差别 |
| D13 | 有界清理，默认 Abandon | 只特判凭证缺失 / 永久 Block | 凭证吊销、RAM 撤权等同样卡死；与 Orphan 默认语义一致 |
| D14 | 临时证书拒绝 = 自签 AND Issuing | 只看自签 / 只看 Issuing | 纯自签误杀 `SelfSigned` issuer；纯 Issuing 有 livelock |
| D15 | replicas 1 | 2 + leader election | 天级事件不需要 HA；避免并发写云 |
| D16 | drift 检测默认 1h | 关闭 / 6h | GitOps 语境下不纠正 drift 的 operator 违反预期 |
| D17 | SANs 不覆盖目标域名硬失败 | 仅告警 / 逃生舱 | 推错证书 = 全站 TLS 报错 |
| D18 | 不做 OLM bundle | 做 | 与 Argo CD 所有权冲突 |
| D19 | 出站限流按 (AK, service) | 依赖 workqueue 限速 | workqueue 限速不约束 HTTP；CAS list QPS 10 |
| D20 | 云调用超时独立 flag 30s | 绑 RenewDeadline 8s | replicas 1 后 split-brain 动机消失；8s 制造假失败 |
