package fc3

import (
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/provider/internal/aliyunerr"
)

// toProviderError 把 aliyun 的错误分类翻译成通用层认识的形状。
//
// 映射本身与 oss 共用一份（pkg/provider/internal/aliyunerr），完整说明在那里。保留这个
// 薄包装，是为了让本包三个方法的错误路径读起来与 oss 逐行同构。
func toProviderError(op string, err error, failReason string) error {
	return aliyunerr.ToProviderError(op, err, failReason)
}

// swallowNotFound 把「目标已经不在了」翻译成成功，供 Cleanup 的两个调用点共用。
func swallowNotFound(err error) error { return aliyunerr.SwallowNotFound(err) }
