package server

// 每 IP 注册闸的语义测试。
//
// 2026-09-14：`每 IP 上限 = 0` 从"不限制"改成"不允许注册"。这条闸在改动前是
// `maxPerIP > 0 && remoteIP != ""`，改完之后 0 必须真的拦住，而"没有来源 IP"
// （管理员建号 / 自检工具）必须仍然放行 —— 这两种情况写反任何一个都会出事：
// 前者让"填 0 全拒"失效，后者让管理员建号被自己的规则挡住。

import (
	"testing"

	"dnf90ops/internal/config"
)

func TestRegistrationBlockedByIP(t *testing.T) {
	cases := []struct {
		name      string
		remoteIP  string
		maxPerIP  int
		existing  int
		wantBlock bool
	}{
		{"不限制(-1)：已注册多少都放行", "1.1.1.1", -1, 999, false},
		{"上限 3：已注册 2 放行", "1.1.1.1", 3, 2, false},
		{"上限 3：已注册 3 拦截", "1.1.1.1", 3, 3, true},
		{"上限 3：已注册 4 拦截", "1.1.1.1", 3, 4, true},
		{"0 = 不允许注册：一个都没有也拦", "1.1.1.1", 0, 0, true},
		{"0 = 不允许注册：已注册若干也拦", "1.1.1.1", 0, 7, true},
		{"没有来源 IP（管理员建号）：0 也不拦", "", 0, 0, false},
		{"没有来源 IP：上限 1 也不拦", "", 1, 5, false},
	}
	for _, tc := range cases {
		blocked, message := registrationBlockedByIP(tc.remoteIP, tc.maxPerIP, tc.existing)
		if blocked != tc.wantBlock {
			t.Fatalf("%s: blocked=%v, want %v", tc.name, blocked, tc.wantBlock)
		}
		if blocked && message == "" {
			t.Fatalf("%s: 拦截时必须给出中文提示", tc.name)
		}
		if !blocked && message != "" {
			t.Fatalf("%s: 放行时不应有提示，实际 %q", tc.name, message)
		}
	}
}

// TestRegistrationUnlimitedIsNegativeOne 钉住"不限制"的取值：
// 一旦有人把它改回 0，这条用例会立刻红 —— 0 现在是"不允许注册"。
func TestRegistrationUnlimitedIsNegativeOne(t *testing.T) {
	if registrationUnlimitedPerIP != -1 {
		t.Fatalf("不限制的取值 = %d, want -1（0 已被占用为不允许注册）", registrationUnlimitedPerIP)
	}
	if blocked, _ := registrationBlockedByIP("2.2.2.2", registrationUnlimitedPerIP, 100); blocked {
		t.Fatal("不限制时不应拦截")
	}
}

// TestRegistrationNeedsCode 注册码闸门（2026-09-24 新增）。
//
// "注册要不要填注册码"必须与服务端注册入口、登录器自检接口同源：
//   - registration_code_required=true → 要（哪怕 registration_mode=open）；
//   - registration_mode=invite       → 要（老语义，虽然开关没开）；
//   - 两个都没开                      → 不要（登录器注册界面保持原样）。
//
// 任何一条写反，出现的都是死局：界面不让填、服务端却要码（注册永远失败），
// 或者界面让填、服务端根本不看（玩家白填）。
func TestRegistrationNeedsCode(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		codeRequired bool
		want         bool
	}{
		{"open + 未开注册码 = 原样（不需要码）", "open", false, false},
		{"open + 开了注册码 = 需要码（本次改动的核心场景）", "open", true, true},
		{"invite 模式 = 一直要码", "invite", false, true},
		{"invite 模式 + 开关也开着 = 要码", "invite", true, true},
	}
	for _, tc := range cases {
		s := &Service{cfg: &config.Config{RegistrationMode: tc.mode}}
		policy := RegistrationPolicy{MaxPerIP: registrationUnlimitedPerIP, CodeRequired: tc.codeRequired}
		if got := s.registrationNeedsCode(policy); got != tc.want {
			t.Fatalf("%s: registrationNeedsCode = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRegistrationPolicyDefaultsWithoutInjection：没注入限制函数时（自检/测试场景）
// 缺省必须是"全放行"—— 特别是 MaxPerIP 不能是 0（那在新语义下等于"不允许注册"）。
func TestRegistrationPolicyDefaultsWithoutInjection(t *testing.T) {
	s := &Service{cfg: &config.Config{RegistrationMode: "open"}}
	p := s.registrationPolicy()
	if p.Closed || p.CodeRequired || p.MaxPerIP != registrationUnlimitedPerIP {
		t.Fatalf("缺省策略 = %+v，期望全放行（MaxPerIP=%d）", p, registrationUnlimitedPerIP)
	}
}
