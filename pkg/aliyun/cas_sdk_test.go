package aliyun

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/alibabacloud-go/tea/dara"
)

// shouldFetchNextPage 是 FindUploaded 唯一的分页决策点，单独测它就不必起 httptest。
func TestShouldFetchNextPage(t *testing.T) {
	tests := []struct {
		name string
		got  int
		size int
		page int64
		want bool
	}{
		{"满页应继续", 50, 50, 1, true},
		{"末页不满应停止", 10, 50, 1, false},
		{"末页只差一条也应停止", 49, 50, 3, false},
		{"空页应停止", 0, 50, 1, false},
		{"服务端压低每页上限时按实际条数判断", 20, 20, 1, true},
		{"上限前一页仍继续", 50, 50, casListMaxPages - 1, true},
		{"撞上页数上限应停止", 50, 50, casListMaxPages, false},
		{"超过页数上限应停止", 50, 50, casListMaxPages + 1, false},
		{"size 非法应停止", 50, 0, 1, false},
		{"got 为负应停止", -1, 50, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFetchNextPage(tc.got, tc.size, tc.page); got != tc.want {
				t.Errorf("shouldFetchNextPage(%d, %d, %d) = %v, 期望 %v", tc.got, tc.size, tc.page, got, tc.want)
			}
		})
	}
}

// TotalCount 缺失（服务端不回显）时不得提前终止：这是修复前静默只返回首页的根因。
func TestShouldFetchNextPage_IgnoresTotalCount(t *testing.T) {
	// 满页即继续，与任何 TotalCount 无关。
	for page := int64(1); page <= 5; page++ {
		if !shouldFetchNextPage(int(casListPageSize), int(casListPageSize), page) {
			t.Fatalf("第 %d 页满页时应继续翻页", page)
		}
	}
}

// callCode 是 aliyuncert_aliyun_api_requests_total 的 code label 的唯一来源，
// 它的取值集合必须有界，且绝不能夹带 SDK Message。
func TestCallCode(t *testing.T) {
	const leaky = "response body must not become a label"
	code, status := "Throttling.User", 400
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"成功", nil, "OK"},
		{"服务端错误码", Classify("Upload", &dara.SDKError{Code: &code, StatusCode: &status, Message: dara.String(leaky)}), "Throttling.User"},
		{"网络超时", Classify("Upload", &net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}), "NetTimeout"},
		{"连接被拒", Classify("Upload", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), "NetError"},
		{"未分类错误", Classify("Upload", errors.New(leaky)), "Unknown"},
		{"未经 Classify 的裸错误", errors.New(leaky), "Unknown"},
		{"Code 为空的 *Error", &Error{Class: ClassPermanent, Op: "Upload"}, "Unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := callCode(tc.err)
			if got != tc.want {
				t.Errorf("callCode(%v) = %q, 期望 %q", tc.err, got, tc.want)
			}
			if got == leaky {
				t.Errorf("code label 泄漏了错误正文")
			}
		})
	}
}

// observe 是三个方法共用的上报点：钩子为 nil 时必须静默跳过，否则给出 action + code。
func TestSDKCASObserve(t *testing.T) {
	t.Run("nil 钩子不 panic", func(t *testing.T) {
		s := &sdkCAS{}
		s.observe(ActionUploadUserCertificate, time.Now(), nil)
	})

	t.Run("按 action 与 code 上报", func(t *testing.T) {
		type call struct {
			action, code string
			d            time.Duration
		}
		var got []call
		s := &sdkCAS{cfg: CASClientConfig{OnCall: func(action, code string, d time.Duration) {
			got = append(got, call{action, code, d})
		}}}
		start := time.Now().Add(-2 * time.Millisecond)
		s.observe(ActionListUserCertificateOrder, start, nil)
		s.observe(ActionListUserCertificateOrder, start, nil)
		s.observe(ActionDeleteUserCertificate, start, &Error{Class: ClassNotFound, Op: "Delete", Code: "CertNotExist"})

		want := []call{
			{action: ActionListUserCertificateOrder, code: "OK"},
			{action: ActionListUserCertificateOrder, code: "OK"},
			{action: ActionDeleteUserCertificate, code: "CertNotExist"},
		}
		if len(got) != len(want) {
			t.Fatalf("上报次数 = %d，期望 %d", len(got), len(want))
		}
		for i := range want {
			if got[i].action != want[i].action || got[i].code != want[i].code {
				t.Errorf("第 %d 次 = %s/%s，期望 %s/%s", i, got[i].action, got[i].code, want[i].action, want[i].code)
			}
			if got[i].d <= 0 {
				t.Errorf("第 %d 次耗时 = %v，应为正数", i, got[i].d)
			}
		}
	})
}
