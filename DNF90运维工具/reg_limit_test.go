package main

// 注册限制语义测试。
//
// 2026-09-14 按用户要求把 `每 IP 上限 = 0` 从"不限制"改成"不允许注册"。
// 这条语义最容易出的错是：某个调用点还用 0 当"不限制"的零值，
// 结果升级后把注册全关死（例如自检建号、管理员建号、没注入限制函数的服务端）。
// 下面几条用例就是钉住这一点。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
)

func TestNormalizeRegMaxPerIP(t *testing.T) {
	ptr := func(v int) *int { return &v }
	cases := []struct {
		name  string
		value *int
		want  int
	}{
		{"配置里没有这个字段 = 不限制", nil, regUnlimitedPerIP},
		{"显式 0 = 不允许注册（保持 0，不能变成不限制）", ptr(0), 0},
		{"1 = 每个 IP 一个", ptr(1), 1},
		{"1000 = 上限", ptr(1000), 1000},
		{"越界上限收敛到 1000", ptr(99999), regMaxPerIPLimit},
		{"负数统一当不限制", ptr(-5), regUnlimitedPerIP},
	}
	for _, tc := range cases {
		if got := normalizeRegMaxPerIP(tc.value); got != tc.want {
			t.Fatalf("%s: normalizeRegMaxPerIP = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRegPerIPText(t *testing.T) {
	if got := regPerIPText(regUnlimitedPerIP); got != "不限每 IP 数量" {
		t.Fatalf("不限制文案 = %q", got)
	}
	if got := regPerIPText(0); got != "不允许注册（每个 IP 上限 0）" {
		t.Fatalf("0 的文案 = %q（必须明确说明是不允许注册）", got)
	}
	if got := regPerIPText(3); got != "每个 IP 最多 3 个" {
		t.Fatalf("上限文案 = %q", got)
	}
}

// TestRegApplyAndStatusRoundTrip 应用配置 → 读回快照，值必须原样保留（0 不能被吞成"不限制"）。
func TestRegApplyAndStatusRoundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a := &App{}
	a.cfg = cfg

	// 不限制：配置里应当**不写**这个字段（下次读回来的还是"没配过"）。
	if _, err := a.regApply(nil, false, regUnlimitedPerIP, false); err != nil {
		t.Fatalf("apply unlimited: %v", err)
	}
	if a.cfg.RegistrationMaxPerIP != nil {
		t.Fatalf("不限制时不应写入字段，实际 = %v", *a.cfg.RegistrationMaxPerIP)
	}
	if st := a.regStatus(); st.MaxPerIP != regUnlimitedPerIP {
		t.Fatalf("读回不限制 = %d", st.MaxPerIP)
	}

	// 0 = 不允许注册：必须真的落成 0，读回来也是 0。
	if _, err := a.regApply(nil, false, 0, false); err != nil {
		t.Fatalf("apply 0: %v", err)
	}
	if a.cfg.RegistrationMaxPerIP == nil || *a.cfg.RegistrationMaxPerIP != 0 {
		t.Fatalf("0 没有被落盘: %v", a.cfg.RegistrationMaxPerIP)
	}
	if st := a.regStatus(); st.MaxPerIP != 0 {
		t.Fatalf("读回 0 = %d，want 0", st.MaxPerIP)
	}
	// 落盘的 JSON 里必须能看到 0（而不是被 omitempty 当成"没配过"抹掉）。
	raw, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("读配置: %v", readErr)
	}
	if !strings.Contains(string(raw), `"registration_max_per_ip": 0`) &&
		!strings.Contains(string(raw), `"registration_max_per_ip":0`) {
		t.Fatalf("配置文件里没写出 0：%s", string(raw))
	}
	// 再 Load 一次：0 必须还是 0（这是"0 被误读成不限制"的回归点）。
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.RegistrationMaxPerIP == nil || *reloaded.RegistrationMaxPerIP != 0 {
		t.Fatalf("重新加载后 0 丢失: %v", reloaded.RegistrationMaxPerIP)
	}

	// 越界要拒绝且不改动已有配置。
	if _, err := a.regApply(nil, false, 5000, false); err == nil {
		t.Fatal("越界值应当被拒绝")
	}
	if st := a.regStatus(); st.MaxPerIP != 0 {
		t.Fatalf("越界被拒后配置被改动: %d", st.MaxPerIP)
	}
	if _, err := a.regApply(nil, false, -2, false); err == nil {
		t.Fatal("小于 -1 的值应当被拒绝")
	}
}

// TestRegPublicLimitDefaultsToUnlimited：没有配置时必须是"不限制"，不能是 0（那会全拒）。
func TestRegPublicLimitDefaultsToUnlimited(t *testing.T) {
	a := &App{}
	a.cfg = config.DefaultConfig()
	p := a.regPublicLimit()
	if p.Closed || p.MaxPerIP != regUnlimitedPerIP || p.CodeRequired {
		t.Fatalf("缺省 = closed:%v max:%d code:%v, want false/%d/false", p.Closed, p.MaxPerIP, p.CodeRequired, regUnlimitedPerIP)
	}
	// 配置指针为 nil 的 App 也不能 panic（认证服务可能在配置加载前被调用）。
	empty := &App{}
	p = empty.regPublicLimit()
	if p.Closed || p.MaxPerIP != regUnlimitedPerIP || p.CodeRequired {
		t.Fatalf("无配置 = closed:%v max:%d code:%v", p.Closed, p.MaxPerIP, p.CodeRequired)
	}
}
