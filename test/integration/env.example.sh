#!/usr/bin/env bash
# 集成测试环境样板。复制成一个 .env 文件（已被 .gitignore 挡住）后填真实值：
#   cp test/integration/env.example.sh local.env && $EDITOR local.env
#   set -a && source local.env && set +a && make test-integration
#
# 强烈建议用一个只给了 README「RAM 权限」一节那两条策略的独立子账号，
# 且不要用生产账号：yundun-cert:* 无法资源级收窄，AK 泄漏可删账号下任意上传证书。

export ALIYUN_ACCESS_KEY_ID=REPLACE_ME
export ALIYUN_ACCESS_KEY_SECRET=REPLACE_ME
# 可选：STS 临时凭证
export ALIYUN_SECURITY_TOKEN=

# CAS 与 FC3 的主 region
export ALIYUN_REGION=cn-hangzhou
# 可选：用于验证 CAS 跨 region 可见性（§12.3 #9）。留空则跳过该项。
# 选一个有独立 endpoint 的 region：ap-southeast-1 / ap-northeast-1 / eu-central-1 …
export ALIYUN_CAS_REGION_ALT=ap-southeast-1
# 可选：资源组
export ALIYUN_RESOURCE_GROUP_ID=

# 可选：FC3 自定义域名（§12.3 #2 / #10 会真的改它的 certConfig，别用生产域名）
export FC3_TEST_DOMAIN=

# 可选：跑集群侧用例（§12.3 #6 / #7 / #11）所需的 kubeconfig。留空则跳过。
export INTEGRATION_KUBECONFIG=

# 可选：设为 1 时保留上传的测试证书，便于人工在控制台核对
export INTEGRATION_KEEP_UPLOADED=
