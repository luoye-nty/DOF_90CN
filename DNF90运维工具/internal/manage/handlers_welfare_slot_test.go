//go:build paid

package manage

import (
	"context"
	"net/http"
	"testing"

	"dnf90ops/internal/config"
)

// TestWelfareSlotApplyRejectsMissingSlot 锁住"缺 slot 就不许写"这道闸。
//
// 背景（2026-09-25 审计）：/manage/v1/welfare/slot 原来把 slot 当值类型解析，
// 客户端漏传（老版本、键名拼错）时反序列化出来是全零值，而"停用 + 无奖励"正好也是全零
// → welfareValidateSlot 对停用档期只做奖励体检、直接放行 → 整档福利被悄悄抹掉并返回 200。
func TestWelfareSlotApplyRejectsMissingSlot(t *testing.T) {
	called := false
	bridge := &Bridge{
		WelfareApplySlot: func(ctx context.Context, kind string, slot config.WelfareSlot) (WelfareStatus, error) {
			called = true
			return WelfareStatus{}, nil
		},
	}
	s := newTestServer(t, bridge)

	// ① 完全没有 slot 字段 → 400，且不能碰桥接
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/welfare/slot", `{"kind":"daily"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("缺 slot 时状态码 = %d，期望 400（响应 %s）", code, raw)
	}
	if called {
		t.Fatal("缺 slot 时不应调用桥接（会把整档福利写成全零）")
	}

	// ② 键名拼错（slots）也算缺 → 400
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/welfare/slot", `{"kind":"daily","slots":{"enabled":true}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("键名拼错时状态码 = %d，期望 400（响应 %s）", code, raw)
	}

	// ③ 显式传一个"全零 / 停用"的 slot → 合法（运营确实可能想清空这一档），必须放行
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/welfare/slot",
		`{"kind":"daily","slot":{"enabled":false,"time":"20:00","rewards":[]}}`)
	if code != http.StatusOK {
		t.Fatalf("显式停用档期应放行，实际 %d（响应 %s）", code, raw)
	}
	if !called {
		t.Fatal("显式 slot 应调用桥接")
	}

	// ④ kind 非法仍然先在 kind 这一步被拒
	code, _ = doMux(t, s, http.MethodPost, "/manage/v1/welfare/slot", `{"kind":"yearly","slot":{"enabled":true}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 kind 状态码 = %d，期望 400", code)
	}
}
