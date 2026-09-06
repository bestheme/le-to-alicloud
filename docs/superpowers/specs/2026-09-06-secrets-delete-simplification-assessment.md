# 设计评估：能否凭 §12.3 #6 的结论去掉 `secrets: delete` 与「等 Certificate NotFound 再删 Secret」

- 日期：2026-09-06
- 范围：只评估，不改代码、不改 spec 正文、不改 README
- 被评估的假设来源：`docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md:832`（§12.3 #6 行注「若保留，可消掉 `secrets: delete` 和 finalizer 顺序难题」）

---

## 1. 结论先行

1. **不建议采纳**。维持现状（方案 A）。
2. #6 只证明了「ownerRef 在 cert-manager 重签后不被抹掉」，**没有**证明 GC 会兑现这个 ownerRef，也**没有**证明 Certificate 删除时 Secret 的去向。前者是后两者的必要条件，不是充分条件。
3. 最关键的一点：ownerRef 得由 operator 自己写到 Secret 上（cert-manager 的 `secretTemplate` 只管 labels/annotations，`config/rbac/role.yaml:18-20` 现在也只有 `get`/`delete`）。写 ownerRef 需要 `secrets: patch` 或 `update`。
4. 于是所谓「收窄 RBAC」实际是把 `delete` 换成 `patch`。cluster-wide 的 `secrets: patch` 允许改写集群里任意 Secret 的内容，比 `delete` 更危险，与 spec `:610` 那段「诚实的表述」的方向相反。
5. 完全去掉顺序约束（方案 B）会引入一个孤儿窗口：GC 对同一 owner 的多个 dependent 无序并发处理，Secret 可能先于 Certificate 被删，而 cert-manager 的 informer 还看得见 Certificate，于是重建出一份不带任何 ownerRef 的 Secret，私钥永久留下。窗口短、属概率事件，但不可逆（**不是**「Certificate 被 finalizer 拖住」——cert-manager 只给 ACME Challenge 挂 finalizer，见 §3.3）。
6. 折中方案 C（保留「等 Certificate NotFound」，只把显式删 Secret 换成 GC）在正确性上仍有一个缺口：ownerRef 若从未写上（CR 短命、operator 短暂不可用、存量对象未回填），Secret 就成永久孤儿。堵它必须「摘 finalizer 前确认 ownerRef 已存在，否则回落显式删除」，而回落路径要求 `delete` 留着——**C 的收益因此归零**（§4.8）。
7. envtest 没有 GC controller（spec `:80`），`internal/controller/deletion_test.go:244` 与 `:346` 现在断言的「Secret 已消失」在 B/C 下无法在 envtest 里成立，必须改成断言 ownerRef 存在，删除语义的验证整体外移到真实集群。
8. 若将来仍要做，前置条件是补齐 #15（GC 是否兑现）与 #16（重建竞速）这两个新探针，而不是复用 #6（见 §5.2）。
9. 有一个**不花任何代价**的替代动作：在部署文档里把 cert-manager 的 `--enable-certificate-owner-ref=true` 记为可选加固项。它不需要 operator 改一行代码、不需要任何 RBAC 变更，且能覆盖「operator 被卸载后 Secret 无人回收」这个 A/B/C 都没覆盖的场景。
10. 附带发现（独立于本题）：`SecretNameConflict` 护栏只在 Certificate 尚未创建时跑一次（`internal/controller/aliyuncertificate_controller.go:162`）。之后改 `spec.secretName` 会让 cert-manager **先覆写**别人的 Secret、再由 `deletion.go:145` 删掉它；「删前复核注解」这个直觉修法在这条路径上会复核通过，因为注解已被改成指向本 CR。见 §4.6 与 Task 9。

---

## 2. 事实核对

以下每条都逐条读过：仓库文件在 worktree `docs/secrets-delete-assessment` 里读，`.superpowers/` 未被 git 跟踪、worktree 下不存在，那份 ledger 只能在主仓库读；cert-manager 源码读的是 go.mod 钉住的 `cert-manager@v1.21.1` 模块。标「推断」的是我的推理，不是文件里的话。

### 2.1 现状：删除顺序

spec §5.6（`docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md:401-419`）：

```
 a. 活着的 Binding 阻塞
 b. CAS 清理（有界，Abandon/Block）
 c. 显式删除 cmapi.Certificate，等待其 NotFound
 d. 删除 Secret
 e. 摘 finalizer
```

`:419` 明确写出硬约束：「唯一硬约束是 **Certificate 必须先于 Secret 死**」。决策表 D12（`:894`）同义重复一次。

代码实现逐条对上：

| 步骤 | 位置 |
|---|---|
| a 阻塞 | `internal/controller/deletion.go:72-81` |
| b CAS 清理 | `internal/controller/deletion.go:93-119` |
| c 删 Certificate + 等 NotFound | `internal/controller/deletion.go:128-143`（NotFound 之前一直 `RequeueAfter: requeueDeletionWait`，即 2s，见 `:40`） |
| d 删 Secret | `internal/controller/deletion.go:144-148` |
| e 摘 finalizer | `internal/controller/deletion.go:156-158` |

`deletion.go:128-129` 的注释把理由写死了：「ownerRef 级联删除是异步的，而 cert-manager 只要还看得见 Certificate 就会把我们下一步删掉的 Secret 再建回来。」

### 2.2 现状：RBAC 与 Secret 的读写面

- kubebuilder marker：`internal/controller/aliyuncertificate_controller.go:113` → `resources=secrets,verbs=get;delete`
- 生成物：`config/rbac/role.yaml:14-20` → `secrets: [delete, get]`
- spec §8.1：`docs/.../2026-09-04-...md:600-601`，行内注「`delete` 只因 cert-manager 默认不 own Secret」
- spec `:610` 的诚实表述：cluster-wide `secrets: get` 等价于读遍集群，真实收益是零缓存与审计噪音下降

**operator 今天不往 Secret 上写任何东西。** 全仓库唯一的 `SetControllerReference` 在 `internal/controller/aliyuncertificate_controller.go:184`，目标是 `cmapi.Certificate`，不是 Secret。Secret 只被 `Get`（TLS Secret：`internal/controller/material.go:50-66`、`internal/controller/aliyuncertificate_controller.go:404`；凭证 Secret：`internal/controller/cas_factory.go:46`、`internal/controller/provider_factory.go:95`）和 `Delete`（`internal/controller/deletion.go:146`）——全仓库对 Secret 的写只有那一处 `Delete`。

### 2.3 #6 探针到底证明了什么

实现在 `test/integration/cluster_test.go:305-411`。它做的事：

1. 用 SelfSigned Issuer 签一张证书，拿到 Secret（`:313`）
2. 建一个真实的 AliyunCertificate 当 owner（`:316-340`）
3. 用 `c.Update` 给 Secret **追加**一条 ownerRef（`:345-357`），没有设 `Controller`、也没有设 `BlockOwnerDeletion`，所以是一条普通的非 controller ownerRef
4. 改 `dnsNames` 触发重签（`:363-373`）
5. 轮询 3 分钟，先确认新 SAN 出现在 `tls.crt` 里（`:389`，即「确实重签了」），再看 ownerRef 还在不在（`:393-397`）

结论记在 `test/integration/RESULTS.md:21`：「保留：重签后 ownerRef 仍在」，证据是「cert-manager v1.20.3，SelfSigned Issuer，改 dnsNames 触发重签并已观测到新 SAN 生效」。

**它证明了**：在 cert-manager v1.20.3 + SelfSigned Issuer + 改 dnsNames 触发的那一类重签下，cert-manager 写回 Secret 时不会抹掉一条外来的非 controller ownerRef。

**它没有证明**（逐条，都是在探针代码里读不到对应动作的）：

| 没证明的事 | 依据 |
|---|---|
| GC 会因为这条 ownerRef 删掉 Secret | 探针从头到尾没有删过那个 owner AliyunCertificate。`:316-340` 建了它，之后只在 `:394` 用它的名字做比对 |
| Certificate 被删时 Secret 的去向 | 探针没有删过 Certificate，只 `Update` 过它的 `dnsNames` |
| GC 的延迟量级 | 同上，一次都没观测过 |
| 续期 / `issuerRef` 变更等其它重签触发路径 | 只测了 `dnsNames`（`:368`）。**但这一条在机制上不重要**：所有路径共用同一次 SSA Apply，见 §4.9 |
| 用 SSA（而非 `Update`）添加 ownerRef 时的字段管理器归属 | 探针用的是 `c.Update`（`:356`） |
| 探针集群上 cert-manager 写 Secret 走的是不是 SSA 路径 | `certManagerVersion`（`:181-196`）只读 CRD 上的 `app.kubernetes.io/version` label，不读 controller 的 feature gate。探针题面（`:32`）写的是「cert-manager 的 SSA 是否保留它」，但观测里没有任何东西能确认。**这一条已由源码而非观测关掉**，见 §4.9 |
| ACME issuer 下的行为 | 探针刻意只用 SelfSigned（`:319-323` 的注释解释了为什么绝不能碰 ACME） |
| `--enable-certificate-owner-ref=true` 时的双 owner 交互 | 探针没有开这个开关，也没有观测 Secret 上是否已有 cert-manager 的 ownerRef |

spec `:832` 的措辞其实已经守住了这条线：「已核实保留；据此简化 RBAC/finalizer 属于后续设计变更，本轮未做」。本评估就是那个「后续设计变更」的第一步。

### 2.4 相邻的两条已核实事实

- `#7`（`RESULTS.md:22`、`cluster_test.go:418`）：Secret 被换成另一张合法证书时 cert-manager 会重签并 bump `revision`。这条说明 cert-manager 对 Secret 内容的漂移是敏感的，但它测的是「内容被改」，不是「Secret 被删」。**推断**：cert-manager 对「Secret 不存在」的反应至少不弱于对「Secret 内容不对」的反应，所以 `deletion.go:128-129` 那句「只要还看得见 Certificate 就会把 Secret 再建回来」在方向上是可信的，只是没有一手观测。
- `#11`（`RESULTS.md:26`）：`cert-manager.io/certificate-name` 等四个注解稳定存在，这是 `secretNameConflict`（`aliyuncertificate_controller.go:411-412`）的判定依据。

### 2.5 环境约束

- `--enable-certificate-owner-ref` 默认关闭，Certificate 默认不拥有它产出的 Secret（spec `:69`，来源是 cert-manager 的 controller CLI 文档）
- envtest 没有 GC controller，ownerRef 级联删除不会发生（spec `:80`）
- Secret 被 `client.CacheOptions.DisableFor` 排除（`cmd/main.go:275`），`Client.Get` 等同直读 API server（spec `:338`）
- leader election 可选，默认关（`cmd/main.go:147`）；开启后只有一个活跃 reconciler
- Plan 1 的 ledger（`.superpowers/sdd/2026-09-04-plan1-foundation-certificate-controller/progress.md`）里 R19–R22 分别是 write-ahead patch 合并（R19）、testutil 跨包改动（R20）、回收失败不降级（R21）、pendingUpload 纳入 CAS 清理（R22）。**这四条都与 Secret 删除顺序无关**，ledger 里也没有任何一条 ruling 讨论过 §5.6 的 c/d 顺序——这个顺序是 spec 直接定的，没有经过评审争议。

### 2.6 现有测试对删除路径的覆盖

`internal/controller/deletion_test.go`：

| 用例 | 行 | 与本题的关系 |
|---|---|---|
| 正常删除：CAS、Certificate、Secret 全部清理 | `:233-248` | `:244` 直接断言 Secret NotFound |
| pendingUpload 在删除时被清理 | `:250` | 无关 |
| 活着的 Binding 阻塞 | `:285` | 无关 |
| Abandon 策略 | `:320-352` | `:346` 同样断言 Secret NotFound |
| Block 策略 | `:354` | 无关 |

envtest 里的 Secret 是 `simulateIssuance` 造出来的（没有真的 cert-manager），删除靠 `deletion.go:146` 那一行显式 `Delete`。**去掉那一行，这两条断言必然失败**，因为 envtest 没有 GC。

---

## 3. 方案对比

### 3.0 一句话定义

| 方案 | 定义 |
|---|---|
| **A** | 维持现状：不给 Secret 加 ownerRef，删除路径显式删 Secret，RBAC 保留 `secrets: get;delete` |
| **B** | operator 给 Secret 加 ownerRef，删除路径同时去掉 c（等 Certificate NotFound）与 d（删 Secret），完全交给 GC |
| **C** | operator 给 Secret 加 ownerRef，保留 c（仍等 Certificate NotFound），只去掉 d，RBAC 由 `delete` 换成 `patch`。**注意**：§4.8 表明要达到 A 的可靠性还须保留 `delete` 作回落，届时 RBAC 是 `get;patch;delete`，比现状更宽 |
| **D** | 代码与 RBAC 一律不动，只在部署文档里把 cert-manager 的 `--enable-certificate-owner-ref=true` 记为可选加固 |

### 3.1 对比表

| 维度 | A 维持现状 | B 全去掉 | C 折中 | D 只加文档 |
|---|---|---|---|---|
| **代码改动面** | 无 | 新增 `ensureSecretOwnerRef`（reconcile 热路径，需幂等 + 冲突重试）；`deletion.go:128-148` 整段删除 | 新增 `ensureSecretOwnerRef`；只删 `deletion.go:144-148` | 无 |
| **RBAC** | `secrets: get;delete` | `secrets: get;patch` | `secrets: get;patch`；若按 §4.8 补回落则为 `get;patch;delete` | 不变 |
| **RBAC 净效果** | — | `delete` → `patch`，从「能删」升级为「能改写任意 Secret 内容」 | 同 B；补回落后是 `delete` **加上** `patch`，纯粹变宽 | — |
| **删除时序** | 见 §3.2 | 见 §3.3 | 见 §3.4 | 同 A |
| **孤儿 Secret 风险** | 低（operator 显式删；失败会重试到 finalizer 摘除前） | **高**：GC 无序 + informer 延迟导致 cert-manager 重建出无 owner 的 Secret（§3.3），叠加下一行 | **中**：竞速窗口被消掉，但「ownerRef 从未写上」仍留永久孤儿（§4.8） | 低（多一道兜底） |
| **私钥残留窗口**（统一从用户发起删除起算） | CAS 清理 + 等 Certificate NotFound + 一次 Delete | B 省掉「等 Certificate NotFound」那一段，但末尾多一段未实测的 GC 排队延迟，端到端未必比 A 慢 | 与 A 同前半段，末尾换成未实测的 GC 延迟 | 同 A |
| **ownerRef 从未写上就删 CR** | 不适用（不依赖任何前置写入） | **永久孤儿** | **永久孤儿**，除非保留 `delete` 作回落（§4.8） | 不适用 |
| **operator 崩溃恢复** | 好：finalizer 未摘，重启后从头重跑 | 中：finalizer 一摘，剩下全靠 GC，operator 无法再介入 | 中：同 B，但 Certificate 已确认死亡，可接受 | 好 |
| **operator 被卸载后**（前提：CR 的 finalizer 被人工强摘或 CRD 被删——否则 CR 不消失，任何 GC 链都不会启动） | Secret 永久残留 | GC 会清（若 ownerRef 已写上） | 同 B | GC 会清，且 Certificate 也被 cert-manager 自己的 owner 链管住 |
| **`--cascade=orphan` 删 CR** | 仍正确（finalizer 照跑，显式删） | **失效**：GC 会剥掉 ownerRef，Secret 永久孤儿 | 同 B | 同 A |
| **envtest 可测性** | 完全可测 | **不可测**（无 GC controller，spec `:80`） | 不可测 | 完全可测 |
| **多副本** | 默认单副本（spec D15 `:897`；`--leader-elect` 默认 false，`cmd/main.go:147`），开了 leader election 才有单写者保证（`:268`）。四个方案无差别 | 同 A | 同 A | 同 A |
| **依赖集群外部条件** | 无 | GC controller 正常工作 | GC controller 正常工作 | 需要集群管理员改 cert-manager 部署参数 |
| **需要新增探针** | 0 | #15 与 #16 硬前置，#17 视集群条件，#18 可选 | #15 硬前置，#17 视集群条件，#18 可选（#16 只用于确认 B 比 C 差多少） | #17 |

### 3.2 A 的时序

```
用户 delete AliyunCertificate
  → finalizer 拦住，对象进入 Terminating
  → a 无活 Binding
  → b CAS 各代删除（有界，超时 Abandon）
  → c Delete(Certificate)；每 2s 重查，直到 NotFound
        （Certificate 没有 finalizer，etcd 里立刻就没了；这几轮重查
         等的是本 operator 的 informer cache 收敛，见 §3.4）
  → d Delete(Secret)                    ← 此刻 Certificate 已确认不存在，不会被重建
  → e RemoveFinalizer → AliyunCertificate 真正消失
```

### 3.3 B 的时序（以及它的失效点）

```
用户 delete AliyunCertificate
  → a、b 同上
  → e RemoveFinalizer → AliyunCertificate 立刻消失
  → GC 看到 Certificate 的 controller ownerRef 失效 → 删 Certificate
  → GC 看到 Secret 的 ownerRef 失效       → 删 Secret
        ↑ 这两件事没有先后保证
```

**失效机制（两个窗口叠加，均为推断，但每一段的前提都已查证）**：

先排除一个想当然的解释——**cert-manager 不给 Certificate 挂 finalizer**。在 go.mod 钉住的 `cert-manager@v1.21.1` 里，`pkg/controller/` 下唯一挂 finalizer 的地方是 ACME Challenge（`pkg/controller/acmechallenges/sync.go:142`，常量 `cmacme.ACMEDomainQualifiedFinalizer`，定义在 `pkg/apis/acme/v1/const.go:21`）；`pkg/apis/certmanager/v1/` 里没有任何 Certificate finalizer 常量。所以「Certificate 会被自己的 finalizer 拖住很久」不成立，不能拿它当失效机制。

真正的窗口有两段：

1. **GC 对同一 owner 的多个 dependent 无序并发处理**（**推断**，Kubernetes GC 的一般语义，本仓库无实测）。AliyunCertificate 有两个 dependent：Certificate（controller ref，`aliyuncertificate_controller.go:184`）与 Secret（我们新加的非 controller ref）。谁先被删没有任何保证，「Secret 已删、Certificate 未删」是一个合法的中间态。
2. **cert-manager 的 issuing controller 是 watch 驱动的**，它的 informer cache 相对 etcd 有非零延迟。即使 Certificate 在 etcd 里已经消失，cert-manager 仍可能在缓存里看到它、发现 Secret 没了，于是走 `secrets_manager` 的 Apply 路径（`cert-manager@v1.21.1/pkg/controller/certificates/issuing/internal/secret.go:127`）把 Secret 建回来。

两段窗口都很短，所以这是**概率事件而非必然**。但后果不可逆：重建出来的 Secret 是全新对象，**不带任何 ownerRef**（默认不开 `--enable-certificate-owner-ref` 时 cert-manager 自己不挂，`secret.go:116-123`；我们的 owner 已经不存在，也没人再去加）。私钥就此永久留在集群里，没有任何 CR 能把它关联回来，人工排查只剩 `certs.bestheme.ac.cn/managed=true` 这个 label（`desired.go:69`）可用。

方案 C 把窗口 1 整个消掉（等到 Certificate NotFound 才摘 finalizer，Secret 的删除必然排在 Certificate 之后），窗口 2 也随之关闭（cert-manager 的缓存有充足时间收敛）。这正是 spec `:419` 那句硬约束要防的事。**#6 的结论对这条失效路径一个字都没说。**

### 3.4 C 的时序

```
用户 delete AliyunCertificate
  → a、b 同上
  → c Delete(Certificate)；等到 NotFound     ← 顺序约束保留
  → e RemoveFinalizer → AliyunCertificate 消失
  → GC 删 Secret                              ← 此刻已无人会重建它
```

C 在这条路径上成立，而且比看上去更稳：`deletion.go:130` 的 `r.Get` 走的是 informer cache（Certificate 由 `Owns` 纳入 watch，未被 `DisableFor` 排除），所以「读到 NotFound」意味着 etcd 已删除**且**本 operator 的缓存已收敛，窗口 2 那一段也一并盖住。

剩下两个未解决项：「GC 什么时候真的动手」这个未实测的延迟，以及 §4.8 那条 C 独立于时序的缺口——ownerRef 若从未写上，这张时序图根本不会开始。

---

## 4. 风险与缺失证据

### 4.1 RBAC 是升级不是降级

`patch` 允许改写任意 Secret 的 `data`。今天 operator 的能力是「读遍集群 + 按名字删」；改造后是「读遍集群 + 按名字改写」。RBAC 无法把 `patch` 限制在 `metadata.ownerReferences` 一个字段上。spec `:610` 已经承认 `get` 在机密性上没有质变，但 `patch` 是**完整性**上的质变：一个被攻陷的 operator 可以替换任意 namespace 里的凭证、CA bundle、ServiceAccount token 类 Secret。

如果同时配了 `--watch-namespaces` 走 namespace 级 Role（spec `:610`、`README.md:412`），这个差异会小很多，但那是另一个开关，不能拿来抵消默认部署的风险。

### 4.2 cert-manager 与 GC 的竞速（B 独有）

见 §3.3。这是 B 与 C 的分水岭，也是本评估否掉 B 的唯一理由。

需要如实说明它的强度：机制是「GC 无序并发 + cert-manager informer 延迟」两段短窗口叠加，不是「Certificate 被 finalizer 长时间拖住」——后者经查证不存在（`cert-manager@v1.21.1/pkg/controller/acmechallenges/sync.go:142` 是全 `pkg/controller/` 唯一的 finalizer 挂载点，对象是 Challenge 不是 Certificate）。所以这是**低概率、不可逆**的失效，不是必然发生的失效。窗口有多长本仓库没有任何观测（已列入 §6）。即便如此，方案 C 以零额外成本消掉整个窗口，没有理由选 B。

### 4.3 私钥残留时间窗

统一从用户发起删除起算，三个方案的前半段（阻塞判断 + CAS 清理）完全相同，差别只在末段：

- **A**：等 Certificate NotFound（每 2s 重试一次，`deletion.go:40`）+ 一次 `Delete` 返回即完成。末段是亚秒到数秒。
- **C**：前半段与 A 相同，末段的 `Delete` 换成 GC 排队延迟。
- **B**：省掉「等 Certificate NotFound」，但末段同样是 GC 排队延迟。端到端未必比 A 慢。

GC 排队延迟**本仓库没有任何观测数据**；在 Secret 数量大的集群上可能是分钟级。对「私钥应尽快消失」这个诉求，A 的末段是确定的、B/C 的末段是未知的——这是差别所在，不是「A 一定更快」。

### 4.4 `--enable-certificate-owner-ref` 打开时的双 owner

若集群管理员开了这个开关，Secret 上会同时有：

- cert-manager 挂的、指向 `Certificate` 的 ownerRef。**已查证**是 controller ref 且带 `BlockOwnerDeletion`：`cert-manager@v1.21.1/pkg/controller/certificates/issuing/internal/secret.go:117` 是 `ref := *metav1.NewControllerRef(crt, certificateGvk)`，`:121` 原样透传 `ref.Controller` 与 `ref.BlockOwnerDeletion`
- 我们挂的、指向 `AliyunCertificate` 的非 controller ownerRef

GC 的规则是「所有 owner 都消失才删 dependent」（**推断**，来自 Kubernetes GC 的一般语义，本仓库无实测）。删 AliyunCertificate 时两个 owner 都会走掉（Certificate 本身是 AliyunCertificate 的 dependent，`aliyuncertificate_controller.go:184`），所以最终结果一致，只是链条更长。

至于**写入冲突**：如果我们用 SSA 加 ownerRef，而 cert-manager 也在用 SSA 管理同一个 `metadata.ownerReferences` 列表，两个 field manager 就会争夺同一字段。**机制上不该发生**——不开 owner-ref flag 时 cert-manager 的 apply 配置根本不声明 `ownerReferences`（`secret.go:109-111`、`:116-123`），这也正是 #6 观测到「保留」的原因。但开了 flag 之后 cert-manager 就会声明它，那时两个 manager 是否相安无事没有任何实测；#6 用的是 `c.Update`（`cluster_test.go:356`），完全没有触及这条路径。

### 4.5 用户手工指定已有 Secret

`spec.secretName` 可以指向任意名字（`api/v1alpha1/aliyuncertificate_types.go:126-128`、`desired.go:27-33`）。创建期有 `SecretNameConflict` 护栏（`aliyuncertificate_controller.go:163-174`），依据是 `cert-manager.io/certificate-name` 注解（`:411-412`，靠 #11 背书）。

但这个护栏**只在 `existing == nil` 时跑**（`:163`），也就是只在 cmapi.Certificate 尚未创建的那一次。之后把 `spec.secretName` 改到一个别人的 Secret 上，护栏不会再触发。

- A 下的后果：删 CR 时 `deletion.go:145-146` 按名字把别人的 Secret 删掉。
- B/C 下的后果：ownerRef 被盖到别人的 Secret 上，删 CR 时 GC 把它删掉。后果等价，但多了一层「对象上留了一条别人不认识的 ownerRef」的困惑。

两边都不好。**这不是 B/C 引入的新问题，也不是采纳 B/C 就能解决的问题**，见 §4.6。

### 4.6 附带发现：现状里的一个误删面（比上一节写的更重）

上一节只说了「删」。实际上删是第二重伤害，**第一重是覆写**，而且它发生得更早。

完整路径（每一步都经代码核实）：

1. 创建 CR，`existing == nil`，`secretNameConflict` 跑一次并通过（`aliyuncertificate_controller.go:162-174`）。
2. Certificate 建出来（`:177-185`）。此后每一轮 reconcile `existing != nil`，`:162` 的 `if` 不成立，**护栏再也不跑**。
3. `spec.secretName` 可变：`api/v1alpha1/aliyuncertificate_types.go:126-128` 上没有不可变约束（该文件里唯一的 `XValidation` 在 `:35`，管的是 `dnsNames`/`commonName` 二选一）；`config/crd/bases/certs.bestheme.ac.cn_aliyuncertificates.yaml:337-339` 的 `secretName` 只有 `type: string`，无 CEL；仓库无 webhook（`internal/webhook` 不存在）。
4. 用户把 `spec.secretName` 改指到别人的 Secret。
5. `desiredCertificateSpec`（`desired.go:42`）把新名字写进 `cert.Spec.SecretName`，`CreateOrUpdate`（`aliyuncertificate_controller.go:177-185`）Update 上去。
6. **cert-manager 用本证书的私钥与证书覆写受害 Secret**，并打上 `cert-manager.io/certificate-name=<本 CR 名>` 与 `certs.bestheme.ac.cn/managed=true`（`desired.go:69`）。
7. 删 CR → `deletion.go:145-146` 按名字无条件 `Delete`。

三个后果：

| # | 后果 |
|---|---|
| 1 | **受害 Secret 被覆写**（步骤 6），内容与元数据都被换成本证书的 |
| 2 | **受害 Secret 被删**（步骤 7） |
| 3 | **旧的 `<name>-tls` 永久孤儿**：改名后它不再被任何代码路径引用，删 CR 时也不会被删 |

后果 1 有一个直接的设计含义：**「删前复核 `cert-manager.io/certificate-name` 注解」这个看起来自然的修法，在这条路径上会复核通过**——步骤 6 已经把注解改成指向本 CR 了。同理，B/C 下「加 ownerRef 前复核注解」（Task 5）有完全相同的盲区。

真正的护栏必须在步骤 2 之后仍然生效：把 `secretNameConflict` 移出 `existing == nil` 分支，在 `spec.secretName` 变更时、**更新 Certificate 之前**重新执行；或者干脆把 `spec.secretName` 定为不可变。删前复核只能作为兜底，单独做是无效修复。

**现有 envtest 完全没有覆盖这条路径。** `reconcile_basic_test.go:151-176` 那个 `SecretNameConflict` 用例是**创建期**场景（先建裸 Secret 再建 CR，`:158-162`），占用者不带任何 cert-manager 注解；全仓库没有任何用例修改过一个已存在 CR 的 `spec.secretName`。顺带确认兜底那一半是低风险的：envtest 的 Secret 由 `upload_test.go:49-54` 的 `writeTLSSecret` 造出，名字 `<name>-tls`、注解 `cert-manager.io/certificate-name: <name>`，与 `certManagerNameFor` 一致，加复核不会打破现有用例。

**建议作为独立条目处理，不要与本评估的取舍绑定**，优先级中：它需要用户对 CR 有写权限并主动改 `secretName`，不构成外部攻击面；但 GitOps 场景下一次模板误改就能触发，后果是覆写加删除他人 Secret，且不可逆。验收见 Task 9。

### 4.7 可测性倒退

A 下删除语义完全落在 envtest 里（`deletion_test.go:233-248`、`:320-352`）。B/C 下 envtest 只能断言到「ownerRef 已挂上」，「Secret 最终消失」这半句必须搬到真实集群探针。用一条 CI 里跑不到的断言，换掉一条每次 `make test` 都在跑的断言，是可靠性上的实打实损失。

### 4.8 B/C 独有：ownerRef 从未被写上

这是 B 与 C 共有的一条失效模式，前面的分析漏了它，而它对 C 尤其要命——§3.4 论证 C 的正确性时默认了「ownerRef 一定在」。

operator 不 watch、不缓存 Secret（spec `:338`、D11 `:893`），所以 `ensureSecretOwnerRef` 只能在 reconcile 被触发时跑。两个后果：

1. **CR 短命**：Certificate 建出来、cert-manager 刚写完 Secret，用户随即删了 CR；或者那一小段时间里 operator 恰好不可用。ownerRef 从未写上，而 B/C 已经不再显式删 Secret——**永久孤儿，私钥永久留下**。方案 A 没有这个问题：它按名字删，不依赖任何前置写入。
2. **存量迁移**：升级到 B/C 的那一刻，集群里所有已签发的 Secret 都没有 ownerRef，各自要等下一次 reconcile 才补上（resync 默认 1h，spec `:338`）。在补上之前删 CR，后果同第 1 条。

想堵住它，只有一个办法：**摘 finalizer 之前必须确认 Secret 上的 ownerRef 已经存在，否则回落到显式删除**。而那个回落路径需要 `secrets: delete`——于是 RBAC 变成 `get + patch + delete`，比现状更宽。

**这一条本身就是支持 A 的独立论据**：C 想拿掉 `delete`，但要做到和 A 一样可靠就必须把 `delete` 留着，收益归零。

### 4.9 #6 的取样面（比初看窄得少，但仍是单版本观测）

读了 go.mod 钉住的 `cert-manager@v1.21.1` 源码之后，这一节里两条本来想写的「取样面偏窄」必须撤回。

`pkg/controller/certificates/issuing/internal/secret.go:108-127`：

```go
applyOpts := metav1.ApplyOptions{FieldManager: s.fieldManager, Force: true}
applyCnf := applycorev1.Secret(secret.Name, secret.Namespace).
    WithAnnotations(secret.Annotations).WithLabels(secret.Labels).
    WithData(secret.Data).WithType(secret.Type)
...
_, err = s.secretClient.Secrets(secret.Namespace).Apply(ctx, applyCnf, applyOpts)
```

由此确定三件事：

1. **cert-manager 写 Secret 确实走 SSA**（`Apply` + `FieldManager` + `Force: true`）。这不是观测，是钉住的依赖源码。§6 原先的假设「探针集群上是不是 SSA 无从确认」可以直接关掉。
2. **不开 owner-ref flag 时，apply 配置根本不声明 `ownerReferences`**（`:109-111` 只有 annotations/labels/data/type；`:116-123` 那段带 ownerRef 的分支被 `if s.enableSecretOwnerReferences` 挡着）。SSA 只移除「同一 field manager 之前拥有、这次省略」的字段，而我们那条 ownerRef 属于另一个 field manager，不在移除范围内。**这就是 #6 观测到「保留」的机制**——不是巧合。
3. **所有写 Secret 的路径都汇到这同一次 `Apply`**：非测试调用点共三处——`issuing_controller.go:464`（签发路径）、`secret_manager.go:99`（secretTemplate 复核路径）、`temporary.go:75`（`cert-manager.io/issue-temporary-certificate` 的临时证书路径，spec `:71` 列了这个注解，本仓库 §5.4 的临时证书拒绝规则（`:389`）与 D14（`:896`）也依赖它）——三者都调 `secretsManager.UpdateData`（接线在 `issuing_controller.go:165`）。与 issuer 类型（SelfSigned / ACME）和重签触发原因（`dnsNames` / `issuerRef` / duration）**无关**。

所以「只测了 SelfSigned」和「只测了 `dnsNames` 触发」这两条在机制上不再有分量，本节不再把它们当作论据。

剩下**仍然成立**的取样面问题只有两条：

- **只有 cert-manager v1.20.3 一个版本**，而上面那段是 v1.21.1 的实现细节，属于上游内部实现，未来可变。`cluster_test.go:179-180` 的注释自己就写着「SSA 是否保留 ownerRef 是版本敏感的行为」。
- **单次观测**：3 分钟超时（`:379`）；超时会 skip 而不是写下假结论，这一点设计得对，但也意味着「保留」这个结论只跑过一次。

**这一节的权重必须调低。** §5.1 的六条理由里，理由 1（RBAC 是放权）、2（缺 GC 兑现与 Certificate 删除路径的证据）、3（代码没变简单）、4（C 的收益可证伪）、5（可测性倒退）都不依赖 #6 的脆弱性，推荐不受影响。

顺带订正：§2.3 里「刻意只用 SelfSigned」的注释在 `cluster_test.go:319-323`（`:326-331` 是 owner 对象的字段）。

---

## 5. 推荐与 Task 清单

### 5.1 推荐

**采纳 A（维持现状）。同时把 D 作为一条零成本的部署文档补充单独排期。**

理由，按权重排序：

1. **收益是负的**：`delete` → `patch` 不是收窄权限，是放宽权限（§4.1）。spec `:600` 那条注释「`delete` 只因 cert-manager 默认不 own Secret」描述的是一个精确的、有界的能力；`patch` 不是。
2. **#6 支撑不起这个结论**：它是必要条件的一半，缺 GC 兑现、缺 Certificate 删除路径这两块（§2.3）。
3. **代码没有变简单**：删掉 `deletion.go:144-148` 五行，换来 reconcile 热路径上一个必须幂等、必须处理与 cert-manager 并发写冲突、必须处理 Secret 尚不存在的 `ensureSecretOwnerRef`。行数与心智负担都是上升的。
4. **C 的收益可证伪**：ownerRef 从未写上就删 CR 会留永久孤儿（§4.8），堵它必须保留 `delete` 作为回落路径，于是 RBAC 变成 `get + patch + delete`，比现状更宽。
5. **可测性倒退**（§4.7）。
6. **C 剩下的那点收益**——「operator 被卸载后 Secret 仍能被回收」——可以由 D 以零代码成本拿到，而且 D 拿到的版本更彻底（Certificate 也一并被 cert-manager 自己的 owner 链管住）。

关于 D 的两条限定，都要写进文档：

- **不能设为默认或强制**：`--enable-certificate-owner-ref` 是集群级 cert-manager 部署参数，影响集群里**所有** Certificate 的 Secret 生命周期，不是本 operator 能单方面决定的，也无法在运行时低成本探测。它只能是「可选加固，副作用见 cert-manager 文档」。
- **上游打算弃用它**：`cert-manager@v1.21.1/design/20220720-per-certificate-owner-ref.md:64` 写着「We intend to remove `--enable-certificate-owner-ref` within 3 to 6 releases. Or maybe never since the maintenance burden won't be high. We will strongly recommend users to switch to `--default-secret-deletion-policy`.」——注意上游给的替代品名字是 controller flag `--default-secret-deletion-policy`，对应 per-Certificate 的 `deletionPolicy` 字段。v1.21.1 的 `CertificateSpec` 里**还没有**这个字段（`pkg/apis/certmanager/v1/types_certificate.go` 里 grep 不到 `DeletionPolicy`），所以今天不存在一个「靠 `deletionPolicy` 解决」的方案 E。推荐 D 时要说明它依赖一个上游态度不明的 flag。

### 5.2 若最终仍决定采纳 C，需要的 Task（只列标题与验收）

按 superpowers plan 粒度，**Task 1（#15）与 Task 2（#16）是硬前置**——在这两条出结论之前不应动任何生产代码。Task 3 视集群条件而定，Task 4 已降级为可选（理由见该条）。

**Task 1：新增集群探针 #15 —— GC 是否兑现这条 ownerRef**
验收：探针建 owner AliyunCertificate、给 Secret 加非 controller ownerRef、删除 owner，在有界超时内观测 Secret 是否消失并记录耗时；结论按 §12.3 编号契约回填 `test/integration/RESULTS.md`；超时必须 skip 而非写下「不删」（沿用 `cluster_test.go:375-411` 那套「reissued 与 kept 分开跟踪」的纪律）。

**Task 2：新增集群探针 #16 —— Certificate 存活时删 Secret 的重建竞速**
验收：在 Certificate 仍 Ready 时删掉 Secret，观测 cert-manager 是否重建、重建耗时、以及**重建后的 Secret 上还有没有 ownerRef**。最后一项是 §3.3 失效模式的直接证据，缺它就无法判定 B 与 C 的差别是否真实存在。

**Task 3：新增集群探针 #17 —— `--enable-certificate-owner-ref=true` 下的双 owner**
验收：在开启该开关的集群上，记录 Secret 上 cert-manager 挂的 ownerRef 是否为 controller ref、我们追加非 controller ref 是否被接受、删 AliyunCertificate 后 Secret 的去向。需要一套独立的集群前置条件，探针在开关未开时必须记「未实测」并说明原因（沿用 `RESULTS.md` #2/#10 的写法）。

**Task 4（可选，优先级低）：新增集群探针 #18 —— 非 `dnsNames` 触发的重签下 ownerRef 是否仍保留**
验收：至少覆盖 `issuerRef` 变更一路（spec `:70` 列为重签触发条件）。**这条已被 §4.9 的源码分析大幅削弱**——所有重签路径共用同一次 `UpdateData` → `Apply`，与触发原因无关，所以它只是给源码结论加一层运行时确认，不是硬前置。真正值得补的是把探针集群 cert-manager 的 feature gate 记进证据栏，补上 `certManagerVersion`（`cluster_test.go:181`）取不到的那一半。

**Task 5：实现 `ensureSecretOwnerRef`**
验收：幂等（重复调用不追加重复条目）；写的是非 controller ref 且不设 `blockOwnerDeletion`；Secret 不存在时静默跳过并等下一轮；与 cert-manager 并发写冲突时按 `RetryOnConflict` 重试；加 ownerRef 前复核 `cert-manager.io/certificate-name` 注解属于本证书（否则拒绝并置 condition）；单测覆盖以上五条。
**必须在实现里写明的盲区**：这个注解复核挡不住 §4.6 描述的 `spec.secretName` 变更路径——那条路径上 cert-manager 已经把注解改成指向本 CR，复核会通过。真正的护栏是 Task 9 的 (a) 半，Task 5 不得声称自己覆盖了它。

**Task 5b：ownerRef 未写上时不摘 finalizer（含存量回填）**
验收：摘 finalizer 前必须 `Get` Secret 并确认我们的 ownerRef 已存在；不存在则回落到显式 `Delete`（因此 RBAC 仍需保留 `delete`，Task 6 的动词改为 `get;patch;delete`）；升级后首次 reconcile 为存量对象补写 ownerRef，并有一个 metric 或日志能回答「集群里还有多少个已签发 Secret 没有 ownerRef」。
**这条 Task 存在本身就是 C 的收益证伪**（§4.8）：要达到与 A 相同的可靠性，`delete` 必须留着。派发前先确认决策者接受「RBAC 净变宽」这个结果。

**Task 6：RBAC 变更与文档同步**
验收：`aliyuncertificate_controller.go:113` 的 marker 改为 `get;patch;delete`——`delete` 必须留着，因为 Task 5b 的回落路径要用它（理由见 §4.8）；只有在明确放弃那条回落时才改成 `get;patch`。`make manifests` 后 `config/rbac/role.yaml` 一致；README 的权限说明与 spec `:610` 的「诚实的表述」段落必须**显式**写出 `patch` 是写权限、比 `delete` 影响面更大，不允许把这次改动描述成「收窄权限」。

**Task 7：删除路径改造**
验收：保留 `deletion.go:128-143`（等 Certificate NotFound），删除 `:144-148`；`deletion_test.go:244` 与 `:346` 的断言改为「Secret 仍在，但带有指向本 CR 的 ownerRef」，并在用例里注释说明 envtest 无 GC（spec `:80`）、真正的删除由 #15 在真实集群背书。

**Task 8：spec 与决策表更新**
验收：§5.6 步骤表去掉 d；§8.1 RBAC 代码块与行内注释更新；D12 行（`:894`）改写为「Certificate 先于 Secret 死」的新实现方式；§12.3 #6 行补一句指向 #15/#16 的交叉引用；§2.4 的 envtest 无 GC 那行补注「删除语义已外移至集群探针」。

**Task 9（独立，可先做）：堵住 `spec.secretName` 变更导致的覆写与误删**

验收是**两条并列，缺一不可**（只做 (b) 是无效修复，理由见 §4.6）：

- **(a) 护栏移出 `existing == nil` 分支。** 把 `secretNameConflict`（`aliyuncertificate_controller.go:402-413`）从 `:162` 的 `if existing == nil {` 里挪出来，在 `spec.secretName` 与 `existing.Spec.SecretName` 不一致时、**更新 Certificate 之前**（即 `:177` 的 `CreateOrUpdate` 之前）重新执行；冲突则置 `SecretNameConflict` 并早退，绝不更新 Certificate。可接受的等价替代是给 `spec.secretName` 加不可变 CEL，但那会挡掉合法的改名场景，取舍需单独裁决。
- **(b) 删前复核注解，作为兜底。** `deletion.go:145` 之前先 `Get` 并核对 `cert-manager.io/certificate-name == certManagerNameFor(ac)`，不匹配则跳过删除、发 Warning event。

**测试验收（现有 envtest 一个都没覆盖，须新增两个用例）**：

1. **覆写护栏**：CR 已就绪、Certificate 已存在，另建一个带别人内容的 Secret，把 `spec.secretName` 改指过去 → 断言 `Ready=False/SecretNameConflict`，且 `cert.Spec.SecretName` **没有**被更新、受害 Secret 内容未被改动。
2. **删前复核**：手工造一个名字匹配但注解指向别的 Certificate 的 Secret，删 CR → 断言该 Secret 仍在、发出了 Warning event。

`writeTLSSecret`（`upload_test.go:49-54`）造的 Secret 注解与 `certManagerNameFor` 一致，所以 (b) 不会打破 `deletion_test.go:244` / `:346` 这两条现有断言。

这条对 A/B/C 都适用，不依赖本评估的取舍。

---

## 6. 本评估未实测的假设

以下每一条都是我在上文中依赖、但**在本仓库里找不到一手观测**的东西。它们中任何一条不成立，§3 的对比表都要重算。评审一轮之后，原先的三条（SSA 路径、issuer 类型、重签触发原因）已由 `cert-manager@v1.21.1` 的源码关掉，移到本节末尾单列。

1. **Kubernetes GC 会真的删掉带我们 ownerRef 的 Secret。** 全仓库无观测。#6 建了 owner 却从没删过它（`cluster_test.go:316-340`）。
2. **GC 的规则是「所有 owner 都消失才删 dependent」。** Kubernetes 的一般语义，本仓库未实测，§4.4 的双 owner 分析建立在它之上。
3. **GC 对同一 owner 的多个 dependent 是无序并发处理的。** §3.3 与 §4.2 否掉方案 B 的第一段窗口，纯推断。
4. **Certificate 从被 GC 删除到真正从 etcd 消失需要多久，以及 cert-manager informer 的收敛延迟。** §3.3 的第二段窗口。两个都是「窗口有多大」的量化问题，本仓库零数据——这直接决定 B 的失效是「罕见」还是「常见」。
5. **Certificate 仍存在时删掉 Secret，cert-manager 会重建它。** `deletion.go:128-129` 与 spec `:413-414` 的立论基础，从未被探针验证。#7 验证的是「内容被改」，不是「对象被删」。
6. **重建出来的 Secret 不带 ownerRef。** §3.3 的孤儿结论依赖这一条。**部分可查证**：不开 owner-ref flag 时 cert-manager 的 apply 配置确实不声明 `ownerReferences`（`secret.go:109-111`、`:116-123`），所以「cert-manager 不会给它加」成立；未查证的是「也没有别人会加」。
7. **#6 的结论能推广到 v1.20.3 之外的 cert-manager 版本。** 单版本单次观测，而 `cluster_test.go:179-180` 自己指出这是版本敏感行为。§4.9 里唯一仍然成立的取样面问题。
8. **GC 的实际延迟量级。** §4.3 的「私钥残留窗口」一栏在 B/C 下是空白，不是小。
9. **`--cascade=orphan` 会剥掉 ownerRef 从而使 B/C 留下永久孤儿。** Kubernetes 的一般语义，本仓库未实测。
10. **用 SSA 追加 ownerRef 与 cert-manager 的 field manager 不冲突。** 完全未探索；#6 用的是 `Update`（`cluster_test.go:356`）。**机制上有支持**：`secret.go:109-111` 表明 cert-manager 的 field manager 从不声明 `ownerReferences`，所以两者不该争夺同一字段——但这是读源码推出来的，没有实测。
11. **`patch` 与 `update` 在本场景下等价可选。** 我按「都是写权限」处理，没有区分二者在准入控制或审计上的差别。
12. **升级到 B/C 时存量 Secret 的回填能在下一次 resync 内完成。** §4.8 第 2 条的前提，未量化。

### 评审后已关掉的三条（依据钉住的依赖源码，非观测）

| 原假设 | 关掉的依据 |
|---|---|
| 探针集群上 cert-manager 写 Secret 走的是不是 SSA | `secret.go:108`/`:127` 是 `Apply` + `FieldManager` + `Force: true`，确为 SSA |
| #6 能否推广到 `dnsNames` 之外的重签触发路径 | 非测试调用点共三处——`issuing_controller.go:464`、`secret_manager.go:99`、`temporary.go:75`——都汇到同一个 `UpdateData`（接线在 `issuing_controller.go:165`），与触发原因无关 |
| #6 能否推广到 ACME issuer | 同上，与 issuer 类型无关 |

另外一条从「推断」升格为**已查证**：cert-manager 在 `--enable-certificate-owner-ref=true` 下挂的是 controller ref 且带 `BlockOwnerDeletion`——`secret.go:117` 是 `ref := *metav1.NewControllerRef(crt, certificateGvk)`，`:121` 原样透传 `ref.Controller` 与 `ref.BlockOwnerDeletion`。
