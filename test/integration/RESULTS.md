# 真实云集成测试结论（spec §12.3）

- 生成时间：2026-09-05T14:23:32Z
- region：`cn-hangzhou`；备用 CAS region：`ap-southeast-1`
- 本文件由 `make test-integration` 生成，不要手改。

| # | 待核实 | 结论 | 证据 |
|---|---|---|---|
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#1 RSA） | 接受 | 上传成功，certId=27079053 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（SEC1 EC） | 接受 | 上传成功，certId=27079054 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#8） | 接受 | 上传成功，certId=27079055 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（加密私钥） | 拒绝 | code=PrivateKeyFormatException class=Permanent |
| #3 | CAS ClientToken 语义：同 token 重复上传返回同 certId 还是报错 | 同 token 重复上传报错 | code=NameRepeat |
| #4 | CAS 证书 Name 是否接受 - 与 .（连字符） | 接受 | 上传成功，certId=27079063 |
| #4 | CAS 证书 Name 是否接受 - 与 .（点号） | 接受 | 上传成功，certId=27079064 |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（leaf+intermediate，无 root） | 接受 | 上传成功，certId=27079056 |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（intermediate 在前） | 拒绝 | code=NotMatch.CertificateAndPrivateKey class=Permanent |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（仅 leaf） | 接受 | 上传成功，certId=27079057 |
| #8 | CAS 单账号已上传证书数量与配额余量 | 账号级（空 Keyword）已上传证书 0 张，其中 Keyword=it.integration.invalid 命中 0 张 | 空 Keyword 确实返回账号全集（哨兵证书上传后条数 1）；itest_ 前缀的残留 0 张。配额上限本探针不实测（撞上限会污染账号），需在控制台「数字证书管理服务 → 证书管理 → 上传证书」页核对并写进 README |
| #9 | 同账号在不同 CAS endpoint 上传的证书是否互相可见 | 不可见：两个 endpoint 的证书集合互相隔离（双向验证） | 上传于 cn-hangzhou 的 certId=27079059 在 ap-southeast-1 列举不可见；上传于 ap-southeast-1 的 certId=584552 在本地可见、在 cn-hangzhou 同样不可见 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=*.it.integration.invalid） | 能匹配到 | 返回 1 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=it.integration.invalid） | 能匹配到 | 返回 1 条 |
| #12 | CAS Keyword 对通配符域名（*.example.com）的匹配行为（Keyword=probe.it.integration.invalid） | 匹配不到 | 返回 0 条 |
| #13 | CAS 同名不同 token 上传返回的错误码 | 错误码=NameRepeat | class=Permanent |
