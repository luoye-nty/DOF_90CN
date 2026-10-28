//go:build paid

package manage

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// gmStatusErrForTest 模拟主程序 account_gm.go 的 acctGMStatusError。
//
// 它必须**只靠这两个方法**被识别 —— 测试用本地类型而不是 import 主程序那个类型，
// 正是为了钉住"跨包用匿名接口断言"这条契约：一旦有人把接口签名改掉，
// 这个测试会失败，而不是让 400 悄悄退化成 502。
type gmStatusErrForTest struct {
	status  int
	message string
}

func (e *gmStatusErrForTest) Error() string {
	return fmt.Sprintf("DNF90Server 原生 GM 接口: %s", e.message)
}
func (e *gmStatusErrForTest) GMStatusCode() int { return e.status }
func (e *gmStatusErrForTest) GMMessage() string { return e.message }

// TestClassifyAccountGMErrorPassesServerStatus 锁住"服务端状态码原样透传"。
//
// 背景（2026-10-05 现场）：跨大系转职时服务端返回 **400 + "dnf gm request is invalid"**，
// 但主程序把状态码丢了，这里兜底成 **502 Bad Gateway**，运营看到的是
// 「失败：DNF90Server 原生 GM 接口: dnf gm request is invalid (HTTP 502)」——
// 把"服务端不支持这个操作"误导成"网关/网络故障"，排查方向完全跑偏。
func TestClassifyAccountGMErrorPassesServerStatus(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantSubstrs []string
	}{
		{
			name:       "跨大系转职：400 通用拒绝码 → 400 + 可操作说明（绝不能再是 502）",
			err:        &gmStatusErrForTest{status: http.StatusBadRequest, message: "dnf gm request is invalid"},
			wantStatus: http.StatusBadRequest,
			wantSubstrs: []string{
				"gm-rejected",
				"跨大系转职", // 必须告诉运营真正的原因
				"回城",      // 以及其它常见原因
				"dnf gm request is invalid", // 原始响应要保留，便于对照服务端日志
			},
		},
		{
			name:        "其它 400 业务错误：原样透传文案",
			err:         &gmStatusErrForTest{status: http.StatusBadRequest, message: "amount out of range"},
			wantStatus:  http.StatusBadRequest,
			wantSubstrs: []string{"amount out of range"},
		},
		{
			name:        "404 → role-not-found（保持既有机器可读码）",
			err:         &gmStatusErrForTest{status: http.StatusNotFound, message: "角色不存在"},
			wantStatus:  http.StatusNotFound,
			wantSubstrs: []string{"role-not-found"},
		},
		{
			name:        "5xx 才是服务端/链路不可用 → gm-not-available + 502",
			err:         &gmStatusErrForTest{status: http.StatusInternalServerError, message: "boom"},
			wantStatus:  http.StatusBadGateway,
			wantSubstrs: []string{"gm-not-available"},
		},
		{
			name:        "不带状态码的普通错误：保持旧兜底行为（不会悄悄变 200）",
			err:         errors.New("某种没带状态码的错误"),
			wantStatus:  http.StatusBadGateway,
			wantSubstrs: []string{"某种没带状态码的错误"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, text := classifyAccountGMError(tc.err)
			if status != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d（文案 %q）", status, tc.wantStatus, text)
			}
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(text, want) {
					t.Fatalf("文案 %q 应包含 %q", text, want)
				}
			}
		})
	}
}
