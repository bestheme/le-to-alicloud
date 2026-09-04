package aliyun

import "testing"

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
