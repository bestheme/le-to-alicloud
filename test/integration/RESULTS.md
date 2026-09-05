# 真实云集成测试结论（spec §12.3）

- 生成时间：2026-09-05T16:24:16Z
- region：`cn-hangzhou`；备用 CAS region：`ap-southeast-1`
- 本文件由 `make test-integration` 生成，不要手改。

| # | 待核实 | 结论 | 证据 |
|---|---|---|---|
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#1 RSA） | 接受 | 上传成功，certId=27079929 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（SEC1 EC） | 接受 | 上传成功，certId=27079930 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#8） | 接受 | 上传成功，certId=27079931 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（带 Proc-Type: 4,ENCRYPTED 头的私钥块） | 拒绝 | code=PrivateKeyFormatException class=Permanent |
| #3 | CAS ClientToken 语义：同 token 重复上传返回同 certId 还是报错 | 同 token 重复上传报错 | code=NameRepeat class=Permanent first=27079937 |
| #4 | CAS 证书 Name 是否接受 - 与 .（连字符） | 接受 | 上传成功，certId=27079939 云上存的名字=itest_d14f05c6f291-hyphen |
| #4 | CAS 证书 Name 是否接受 - 与 .（点号） | 接受 | 上传成功，certId=27079940 云上存的名字=itest_d9c9948e7457.dot |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（leaf + 其签发 CA，两块） | 接受 | 上传成功，certId=27079932 |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（intermediate 在前） | 拒绝 | code=NotMatch.CertificateAndPrivateKey class=Permanent |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（仅 leaf） | 接受 | 上传成功，certId=27079933 |
| #6 | 给 TLS Secret 追加 ownerRef 后 cert-manager 的 SSA 是否保留它 | 未实测 | 集群上没有这些 API 类型：cert-manager.io/v1 Certificate, cert-manager.io/v1 Issuer, certs.bestheme.ac.cn/v1alpha1 AliyunCertificate；装好 cert-manager（与本项目 CRD）后重跑本用例即可得到结论 |
| #7 | Secret 被替换为另一张合法证书时 cert-manager 是否重签并 bump revision | 未实测 | 集群上没有这些 API 类型：cert-manager.io/v1 Certificate, cert-manager.io/v1 Issuer；装好 cert-manager（与本项目 CRD）后重跑本用例即可得到结论 |
| #8 | CAS 单账号已上传证书数量与配额余量 | 空 Keyword 列举返回 0 张已上传证书（Status 留空，不含已过期证书；配了 ALIYUN_RESOURCE_GROUP_ID 时仅限该资源组），其中 Keyword=it.integration.invalid 命中 0 张 | 哨兵证书上传后可见（条数 0 → 1），说明空 Keyword 至少不是「匹配不到任何东西」；但这只证明列举包含哨兵，不足以证明它是账号全集。itest_ 前缀的残留 0 张。配额上限本探针不实测（撞上限会污染账号），账号总量与上限需在控制台「数字证书管理服务 → 证书管理 → 上传证书」页核对并写进 README |
| #9 | 同账号在不同 CAS endpoint 上传的证书是否互相可见 | 不可见：两个 endpoint 的证书集合互相隔离（双向验证） | 同一份 PEM，上传于 cn-hangzhou 得 certId=27079935、上传于 ap-southeast-1 另得 certId=584583（两个 ID 不在同一量级，是两套独立 ID 空间而非复制延迟）；前者在 ap-southeast-1 列举不可见，后者在本地可见、在 cn-hangzhou 同样不可见 |
| #11 | cert-manager 打在 Secret 上的注解是否稳定存在 | 未实测 | 集群上没有这些 API 类型：cert-manager.io/v1 Certificate, cert-manager.io/v1 Issuer；装好 cert-manager（与本项目 CRD）后重跑本用例即可得到结论 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=*.it.integration.invalid） | 能匹配到 | 证书 SAN=*.it.integration.invalid，本次列举共返回 1 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=it.integration.invalid） | 能匹配到 | 证书 SAN=*.it.integration.invalid，本次列举共返回 1 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=probe.it.integration.invalid） | 匹配不到 | 证书 SAN=*.it.integration.invalid，本次列举共返回 0 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=integration） | 能匹配到 | 证书 SAN=*.it.integration.invalid，本次列举共返回 1 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=ntegratio） | 能匹配到 | 证书 SAN=*.it.integration.invalid，本次列举共返回 1 条 |
| #13 | CAS 同名不同 token 上传返回的错误码 | 错误码=NameRepeat | class=Permanent |
