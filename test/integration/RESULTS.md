# 真实云集成测试结论（spec §12.3）

- 生成时间：2026-09-05T14:12:51Z
- region：`cn-hangzhou`；备用 CAS region：``
- 本文件由 `make test-integration` 生成，不要手改。

| # | 待核实 | 结论 | 证据 |
|---|---|---|---|
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#1 RSA） | 接受 | 上传成功，certId=27078944 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（SEC1 EC） | 接受 | 上传成功，certId=27078945 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（PKCS#8） | 接受 | 上传成功，certId=27078946 |
| #1 | CAS 对 PKCS#1 / PKCS#8 / SEC1 私钥的接受情况（加密私钥） | 拒绝 | code=PrivateKeyFormatException class=Permanent |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（leaf+intermediate，无 root） | 接受 | 上传成功，certId=27078947 |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（intermediate 在前） | 拒绝 | code=NotMatch.CertificateAndPrivateKey class=Permanent |
| #5 | CAS 是否接受 leaf+intermediate（无 root）以及链顺序是否敏感（仅 leaf） | 接受 | 上传成功，certId=27078948 |
