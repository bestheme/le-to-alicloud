# 设计评估：能否凭 §12.3 #6 的结论去掉 `secrets: delete` 与「等 Certificate NotFound 再删 Secret」

- 日期：2026-09-06
- 范围：只评估，不改代码、不改 spec 正文、不改 README
- 被评估的假设来源：`docs/superpowers/specs/2026-09-04-le-to-alicloud-operator-design.md:832`（§12.3 #6 行注「若保留，可消掉 `secrets: delete` 和 finalizer 顺序难题」）

---

## 1. 结论先行

1. **不建议采纳**。维持现状（方案 A）。
2. #6 只证明了「ownerRef 在 cert-manager 重签后不被抹掉」，**没有**证明 GC 会兑现这个 ownerRef，也**没有**证明 Certificate 删除时 Secret 的去向。前者是后两者的必要条件，不是充分条件。
3. 最关键的一点：ownerRef 得由 operator 自己写到 Secret 上（cert-manager 的 `secretTemplate` 只管 labels/annotations，`config/rbac/role.yaml:17` 现在也只有 `get`/`delete`）。写 ownerRef 需要 `secrets: patch` 或 `update`。
4. 于是所谓「收窄 RBAC」实际是把 `delete` 换成 `patch`。cluster-wide 的 `secrets: patch` 允许改写集群里任意 Secret 的内容，比 `delete` 更危险，与 spec `:610` 那段「诚实的表述」的方向相反。
5. 完全去掉顺序约束（方案 B）会引入一个真实的孤儿窗口：AliyunCertificate 消失 → GC 删 Secret，而 cmapi.Certificate 因 cert-manager 自己的 finalizer 仍可能活着 → cert-manager 重建 Secret，重建出来的那份不带任何 ownerRef，私钥永久留在集群里。
6. 折中方案 C（保留「等 Certificate NotFound」，只把显式删 Secret 换成 GC）在正确性上站得住，但它换来的是「少 4 行删除代码」，付出的是「热路径新增一个幂等 ownerRef 写入 + RBAC 升级为写权限 + envtest 断言全部失效」。净复杂度上升。
7. envtest 没有 GC controller（spec `:80`），`internal/controller/deletion_test.go:244` 与 `:346` 现在断言的「Secret 已消失」在 B/C 下无法在 envtest 里成立，必须改成断言 ownerRef 存在，删除语义的验证整体外移到真实集群。
8. 若将来仍要做，前置条件是补齐 4 个新探针（#15–#18，见 §5），而不是复用 #6。
9. 有一个**不花任何代价**的替代动作：在部署文档里把 cert-manager 的 `--enable-certificate-owner-ref=true` 记为可选加固项。它不需要 operator 改一行代码、不需要任何 RBAC 变更，且能覆盖「operator 被卸载后 Secret 无人回收」这个 A/B/C 都没覆盖的场景。
10. 附带发现（与本题相关但独立）：`internal/controller/deletion.go:145` 按名字无条件删 Secret，没有复核 `cert-manager.io/certificate-name` 注解，而创建期的冲突检查（`internal/controller/aliyuncertificate_controller.go:402`）只在 Certificate 尚不存在时跑一次。这是现状里已经存在的一个误删面，见 §4.6。

---

## 2. 事实核对

以下每条都在 worktree `docs/secrets-delete-assessment` 里逐条读过。标「推断」的是我的推理，不是文件里的话。

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

**operator 今天不往 Secret 上写任何东西。** 全仓库唯一的 `SetControllerReference` 在 `internal/controller/aliyuncertificate_controller.go:184`，目标是 `cmapi.Certificate`，不是 Secret。Secret 只被 `Get`（`internal/controller/material.go:50-66`、`internal/controller/aliyuncertificate_controller.go:404`）和 `Delete`（`internal/controller/deletion.go:146`）。

### 2.3 #6 探针到底证明了什么

实现在 `test/integration/cluster_test.go:305-411`。它做的事：

1. 用 SelfSigned Issuer 签一张证书，拿到 Secret（`:314`）
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
| 续期 / `issuerRef` 变更等其它重签触发路径 | 只测了 `dnsNames`（`:368`） |
| 用 SSA（而非 `Update`）添加 ownerRef 时的字段管理器归属 | 探针用的是 `c.Update`（`:356`） |
| 探针集群上 cert-manager 写 Secret 走的是不是 SSA 路径 | `certManagerVersion`（`:181-196`）只读 CRD 上的 `app.kubernetes.io/version` label，不读 controller 的 feature gate。探针题面（`:32`）写的是「cert-manager 的 SSA 是否保留它」，但观测里没有任何东西能确认那一路真的是 SSA |
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
| **C** | operator 给 Secret 加 ownerRef，保留 c（仍等 Certificate NotFound），只去掉 d，RBAC 由 `delete` 换成 `patch` |
| **D** | 代码与 RBAC 一律不动，只在部署文档里把 cert-manager 的 `--enable-certificate-owner-ref=true` 记为可选加固 |

### 3.1 对比表

| 维度 | A 维持现状 | B 全去掉 | C 折中 | D 只加文档 |
|---|---|---|---|---|
| **代码改动面** | 无 | 新增 `ensureSecretOwnerRef`（reconcile 热路径，需幂等 + 冲突重试）；`deletion.go:128-148` 整段删除 | 新增 `ensureSecretOwnerRef`；只删 `deletion.go:144-148` | 无 |
| **RBAC** | `secrets: get;delete` | `secrets: get;patch` | `secrets: get;patch` | 不变 |
| **RBAC 净效果** | — | `delete` → `patch`，从「能删」升级为「能改写任意 Secret 内容」 | 同 B | — |
| **删除时序** | 见 §3.2 | 见 §3.3 | 见 §3.4 | 同 A |
| **孤儿 Secret 风险** | 低（operator 显式删；失败会重试到 finalizer 摘除前） | **高**：Certificate 未死透时 cert-manager 重建的 Secret 无 owner | 低 | 低（多一道兜底） |
| **私钥残留窗口** | 从 Certificate NotFound 到 Delete 返回，亚秒级 | GC 排队延迟，未实测 | GC 排队延迟，未实测 | 同 A |
| **operator 崩溃恢复** | 好：finalizer 未摘，重启后从头重跑 | 中：finalizer 一摘，剩下全靠 GC，operator 无法再介入 | 中：同 B，但 Certificate 已确认死亡，可接受 | 好 |
| **operator 被卸载后** | Secret 永久残留（无人跑 finalizer） | GC 仍会清（若 ownerRef 已写上） | 同 B | GC 仍会清 |
| **`--cascade=orphan` 删 CR** | 仍正确（finalizer 照跑，显式删） | **失效**：GC 会剥掉 ownerRef，Secret 永久孤儿 | 同 B | 同 A |
| **envtest 可测性** | 完全可测 | **不可测**（无 GC controller，spec `:80`） | 不可测 | 完全可测 |
| **多副本** | leader election 保证单写者（`cmd/main.go:268`），无差别 | 无差别 | 无差别 | 无差别 |
| **依赖集群外部条件** | 无 | GC controller 正常工作 | GC controller 正常工作 | 需要集群管理员改 cert-manager 部署参数 |
| **需要新增探针** | 0 | #15/#16/#17/#18 全要 | #15/#17/#18 | #17 |

### 3.2 A 的时序

```
用户 delete AliyunCertificate
  → finalizer 拦住，对象进入 Terminating
  → a 无活 Binding
  → b CAS 各代删除（有界，超时 Abandon）
  → c Delete(Certificate)；每 2s 重查，直到 NotFound
        （cert-manager 自己的 finalizer 在这期间跑完）
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

**失效点（推断，但机制清楚）**：GC 删 Secret 与 GC 删 Certificate 是两条独立的清理，而 Certificate 上有 cert-manager 自己的 finalizer，删除会被拖住一段时间。在那段时间里 Certificate 仍然存在且 Ready，cert-manager 发现 Secret 没了，就按 `deletion.go:128-129` 描述的行为重建它。重建出来的 Secret 是全新对象，**不带任何 ownerRef**（owner 已经不存在了，也没人再去加），于是私钥永久留在集群里，且没有任何 CR 能把它关联回来——连人工排查都只剩 `certs.bestheme.ac.cn/managed=true` 这个 label（`desired.go:69`）可用。

这正是 spec `:419` 那句硬约束要防的事。#6 的结论对这条失效路径**一个字都没说**。

### 3.4 C 的时序

```
用户 delete AliyunCertificate
  → a、b 同上
  → c Delete(Certificate)；等到 NotFound     ← 顺序约束保留
  → e RemoveFinalizer → AliyunCertificate 消失
  → GC 删 Secret                              ← 此刻已无人会重建它
```

C 在正确性上成立：Certificate 已确认死亡，重建者不存在。剩下的只是「GC 什么时候真的动手」这个未实测的延迟。

---

## 4. 风险与缺失证据

### 4.1 RBAC 是升级不是降级

`patch` 允许改写任意 Secret 的 `data`。今天 operator 的能力是「读遍集群 + 按名字删」；改造后是「读遍集群 + 按名字改写」。RBAC 无法把 `patch` 限制在 `metadata.ownerReferences` 一个字段上。spec `:610` 已经承认 `get` 在机密性上没有质变，但 `patch` 是**完整性**上的质变：一个被攻陷的 operator 可以替换任意 namespace 里的凭证、CA bundle、ServiceAccount token 类 Secret。

如果同时配了 `--watch-namespaces` 走 namespace 级 Role（spec `:610`、`README.md:412`），这个差异会小很多，但那是另一个开关，不能拿来抵消默认部署的风险。

### 4.2 cert-manager 与 GC 的竞速（B 独有）

见 §3.3。这是 B 与 C 的分水岭，也是本评估否掉 B 的唯一理由。

### 4.3 私钥残留时间窗

A：`Delete` 返回即完成，亚秒级。
B/C：取决于 GC controller 的队列深度与同步周期。**本仓库没有任何观测数据**。在一个 Secret 数量大的集群上这可能是分钟级。对「私钥应尽快消失」这个诉求，A 严格更好。

### 4.4 `--enable-certificate-owner-ref` 打开时的双 owner

若集群管理员开了这个开关，Secret 上会同时有：

- cert-manager 挂的、指向 `Certificate` 的 ownerRef（**推断**：按该开关的用途，它应是 controller ref）
- 我们挂的、指向 `AliyunCertificate` 的非 controller ownerRef

GC 的规则是「所有 owner 都消失才删 dependent」（**推断**，来自 Kubernetes GC 的一般语义，本仓库无实测）。删 AliyunCertificate 时两个 owner 都会走掉（Certificate 本身是 AliyunCertificate 的 dependent，`aliyuncertificate_controller.go:184`），所以最终结果一致，只是链条更长。

真正的风险是**写入冲突**：如果我们用 SSA 加 ownerRef，而 cert-manager 也在用 SSA 管理同一个 `metadata.ownerReferences` 列表，两个 field manager 会互相争夺。#6 用的是 `c.Update`（`cluster_test.go:356`），完全没有触及这条路径。

### 4.5 用户手工指定已有 Secret

`spec.secretName` 可以指向任意名字（`api/v1alpha1/aliyuncertificate_types.go:126-128`、`desired.go:27-33`）。创建期有 `SecretNameConflict` 护栏（`aliyuncertificate_controller.go:163-174`），依据是 `cert-manager.io/certificate-name` 注解（`:411-412`，靠 #11 背书）。

但这个护栏**只在 `existing == nil` 时跑**（`:163`），也就是只在 cmapi.Certificate 尚未创建的那一次。之后把 `spec.secretName` 改到一个别人的 Secret 上，护栏不会再触发。

- A 下的后果：删 CR 时 `deletion.go:145-146` 按名字把别人的 Secret 删掉。
- B/C 下的后果：ownerRef 被盖到别人的 Secret 上，删 CR 时 GC 把它删掉。后果等价，但多了一层「对象上留了一条别人不认识的 ownerRef」的困惑。

两边都不好。**这不是 B/C 引入的新问题，也不是采纳 B/C 就能解决的问题**，见 §4.6。

### 4.6 附带发现：现状里的一个误删面

`deletion.go:145` 直接用 `secretNameFor(ac)` 构造对象就删，没有先 `Get` 出来复核 `cert-manager.io/certificate-name`。既然 `#11` 已经把这四个注解确认为可靠依据（`RESULTS.md:26`），删除前加一次注解复核是一个与本题无关、成本很低的独立加固。它对 A 有效，对 B/C 同样需要（换成「加 ownerRef 前复核」）。**建议作为独立条目处理，不要与本评估的取舍绑定。**

### 4.7 可测性倒退

A 下删除语义完全落在 envtest 里（`deletion_test.go:233-248`、`:320-352`）。B/C 下 envtest 只能断言到「ownerRef 已挂上」，「Secret 最终消失」这半句必须搬到真实集群探针。用一条 CI 里跑不到的断言，换掉一条每次 `make test` 都在跑的断言，是可靠性上的实打实损失。

### 4.8 #6 的取样面偏窄

- 只有 SelfSigned Issuer（`cluster_test.go:326-331` 解释了为何刻意不碰 ACME）
- 只有 `dnsNames` 触发的重签；spec `:70` 列出 `issuerRef`、subject、duration 同样会触发重签，均未测
- 只有 cert-manager v1.20.3 一个版本，且 feature gate 未记录（`certManagerVersion` 只读版本 label，`:181-196`）
- 3 分钟超时（`:379`）；超时会 skip 而不是给假结论，这一点设计得对，但也意味着「保留」这个结论是单次观测

版本敏感性这一点，`cluster_test.go:179-180` 的注释自己写得很清楚：「SSA 是否保留 ownerRef 是版本敏感的行为」。把一个版本敏感的单点观测直接兑现成 RBAC 与删除语义的永久变更，杠杆过高。

---

## 5. 推荐与 Task 清单

### 5.1 推荐

**采纳 A（维持现状）。同时把 D 作为一条零成本的部署文档补充单独排期。**

理由，按权重排序：

1. **收益是负的**：`delete` → `patch` 不是收窄权限，是放宽权限（§4.1）。spec `:600` 那条注释「`delete` 只因 cert-manager 默认不 own Secret」描述的是一个精确的、有界的能力；`patch` 不是。
2. **#6 支撑不起这个结论**：它是必要条件的一半，缺 GC 兑现、缺 Certificate 删除路径这两块（§2.3）。
3. **代码没有变简单**：删掉 `deletion.go:144-148` 五行，换来 reconcile 热路径上一个必须幂等、必须处理与 cert-manager 并发写冲突、必须处理 Secret 尚不存在的 `ensureSecretOwnerRef`。行数与心智负担都是上升的。
4. **可测性倒退**（§4.7）。
5. **C 唯一真实的收益**——「operator 被卸载后 Secret 仍能被回收」——可以由 D 以零代码成本拿到，而且 D 拿到的版本更彻底（Certificate 也一并被 cert-manager 自己的 owner 链管住）。

D 不能设为默认或强制：`--enable-certificate-owner-ref` 是集群级 cert-manager 部署参数，会影响集群里**所有** Certificate 的 Secret 生命周期，不是本 operator 能单方面决定的，也无法在运行时低成本探测。所以它只能是文档里的「可选加固，副作用见 cert-manager 文档」。

### 5.2 若最终仍决定采纳 C，需要的 Task（只列标题与验收）

按 superpowers plan 粒度，前 4 个是硬前置——在 #15 与 #16 出结论之前不应动任何生产代码。

**Task 1：新增集群探针 #15 —— GC 是否兑现这条 ownerRef**
验收：探针建 owner AliyunCertificate、给 Secret 加非 controller ownerRef、删除 owner，在有界超时内观测 Secret 是否消失并记录耗时；结论按 §12.3 编号契约回填 `test/integration/RESULTS.md`；超时必须 skip 而非写下「不删」（沿用 `cluster_test.go:375-411` 那套「reissued 与 kept 分开跟踪」的纪律）。

**Task 2：新增集群探针 #16 —— Certificate 存活时删 Secret 的重建竞速**
验收：在 Certificate 仍 Ready 时删掉 Secret，观测 cert-manager 是否重建、重建耗时、以及**重建后的 Secret 上还有没有 ownerRef**。最后一项是 §3.3 失效模式的直接证据，缺它就无法判定 B 与 C 的差别是否真实存在。

**Task 3：新增集群探针 #17 —— `--enable-certificate-owner-ref=true` 下的双 owner**
验收：在开启该开关的集群上，记录 Secret 上 cert-manager 挂的 ownerRef 是否为 controller ref、我们追加非 controller ref 是否被接受、删 AliyunCertificate 后 Secret 的去向。需要一套独立的集群前置条件，探针在开关未开时必须记「未实测」并说明原因（沿用 `RESULTS.md` #2/#10 的写法）。

**Task 4：新增集群探针 #18 —— 非 `dnsNames` 触发的重签下 ownerRef 是否仍保留**
验收：至少覆盖 `issuerRef` 变更一路（spec `:70` 列为重签触发条件）；同时把探针集群 cert-manager 的 feature gate 一并记进证据栏，补上 `certManagerVersion`（`cluster_test.go:181`）取不到的那一半。

**Task 5：实现 `ensureSecretOwnerRef`**
验收：幂等（重复调用不追加重复条目）；写的是非 controller ref 且不设 `blockOwnerDeletion`；Secret 不存在时静默跳过并等下一轮；与 cert-manager 并发写冲突时按 `RetryOnConflict` 重试；加 ownerRef 前复核 `cert-manager.io/certificate-name` 注解属于本证书（否则拒绝并置 condition）；单测覆盖以上五条。

**Task 6：RBAC 变更与文档同步**
验收：`aliyuncertificate_controller.go:113` 的 marker 由 `get;delete` 改为 `get;patch`，`make manifests` 后 `config/rbac/role.yaml` 一致；README 的权限说明与 spec `:610` 的「诚实的表述」段落必须**显式**写出 `patch` 是写权限、比 `delete` 影响面更大，不允许把这次改动描述成「收窄权限」。

**Task 7：删除路径改造**
验收：保留 `deletion.go:128-143`（等 Certificate NotFound），删除 `:144-148`；`deletion_test.go:244` 与 `:346` 的断言改为「Secret 仍在，但带有指向本 CR 的 ownerRef」，并在用例里注释说明 envtest 无 GC（spec `:80`）、真正的删除由 #15 在真实集群背书。

**Task 8：spec 与决策表更新**
验收：§5.6 步骤表去掉 d；§8.1 RBAC 代码块与行内注释更新；D12 行（`:894`）改写为「Certificate 先于 Secret 死」的新实现方式；§12.3 #6 行补一句指向 #15/#16 的交叉引用；§2.4 的 envtest 无 GC 那行补注「删除语义已外移至集群探针」。

**Task 9（独立，可先做）：删 Secret 前复核注解**
验收：`deletion.go:145` 之前先 `Get` 并核对 `cert-manager.io/certificate-name == certManagerNameFor(ac)`，不匹配则跳过删除、发 Warning event；envtest 用例覆盖「用户把 secretName 改到别人的 Secret 上」这一场景。这条对 A/B/C 都适用，不依赖本评估的取舍。

---

## 6. 本评估未实测的假设

以下每一条都是我在上文中依赖、但**在本仓库里找不到一手观测**的东西。它们中任何一条不成立，§3 的对比表都要重算。

1. **Kubernetes GC 会真的删掉带我们 ownerRef 的 Secret。** 全仓库无观测。#6 建了 owner 却从没删过它（`cluster_test.go:322-341`）。
2. **GC 的规则是「所有 owner 都消失才删 dependent」。** 这是 Kubernetes 的一般语义，但本仓库未实测，§4.4 的双 owner 分析建立在它之上。
3. **Certificate 仍存在时删掉 Secret，cert-manager 会重建它。** 这是 `deletion.go:128-129` 与 spec `:413-414` 的立论基础，但它从未被探针验证过。#7 验证的是「内容被改」，不是「对象被删」。
4. **重建出来的 Secret 不带 ownerRef。** §3.3 的孤儿结论依赖这一条，纯推断。
5. **探针集群上 cert-manager 写 Secret 走的是 SSA。** 题面（`cluster_test.go:32`）这么假设，观测里没有任何东西能确认。
6. **#6 的结论能推广到 `dnsNames` 之外的重签触发路径。** 只测了一条。
7. **#6 的结论能推广到 ACME issuer。** 只测了 SelfSigned。
8. **#6 的结论能推广到 v1.20.3 之外的 cert-manager 版本。** 单版本单次观测，而 `cluster_test.go:179-180` 自己指出这是版本敏感行为。
9. **GC 的实际延迟量级。** §4.3 的「私钥残留窗口」一栏在 B/C 下是空白，不是小。
10. **`--cascade=orphan` 会剥掉 ownerRef 从而使 B/C 留下永久孤儿。** 这是 Kubernetes 的一般语义，本仓库未实测。
11. **cert-manager 在 `--enable-certificate-owner-ref=true` 下挂的是 controller ref。** 按用途推断，未查证也未实测。
12. **用 SSA 追加 ownerRef 与 cert-manager 的 field manager 不冲突。** 完全未探索；#6 用的是 `Update`。
13. **`patch` 与 `update` 在本场景下等价可选。** 我按「都是写权限」处理，没有区分二者在准入控制或审计上的差别。
