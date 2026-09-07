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
