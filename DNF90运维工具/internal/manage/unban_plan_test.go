//go:build paid

package manage

// 账号型解封目标规划的单测。
//
// 锁的是 2026-09-24 修的这条主流程 bug：
//   管理工具「封禁」页 → 解封区（类型=账号）→ 填**用户名** → 解封
//   → 旧实现拿用户名去 WHERE ban_value=? 精确匹配，而账号型封禁写入的
//     ban_value 是 account_id（dnf:<sha256hex>）→ 必然
//     `no-active-ban-found (HTTP 404)`，运营看到的就是"解封坏了"。
// 封禁侧一直有"用户名→account_id"解析，解封侧缺失，属于两侧语义不对称。
//
// 这里只测纯函数（不连库）：planAccountUnban 决定"动哪几条"，
// 需要连库的部分（UnbanTargets / BanLookup）由已上线的既有 SQL 路径承担。

import (
	"database/sql"
	"testing"
	"time"

	"dnf90ops/internal/store"
)

const testAccountID = "dnf:e3319b58ec68c92d20b223abdb895f90f4bb05ccc7a3ce3bf34c68c7b50ebb3e"

func targetStrings(t *testing.T, plan unbanPlan) []string {
	t.Helper()
	out := make([]string, 0, len(plan.Targets))
	for _, tg := range plan.Targets {
		out = append(out, tg.BanType+"="+tg.BanValue)
	}
	return out
}

func assertTargets(t *testing.T, plan unbanPlan, want []string) {
	t.Helper()
	got := targetStrings(t, plan)
	if len(got) != len(want) {
		t.Fatalf("目标数 = %d %v，期望 %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个目标 = %q，期望 %q（整体 %v）", i, got[i], want[i], got)
		}
	}
}

// TestPlanAccountUnban_TypedUsernameIsResolved 是本轮修复的核心回归锁：
// 填用户名也必须解掉「账号」那一条（用解析出来的 account_id）。
func TestPlanAccountUnban_TypedUsernameIsResolved(t *testing.T) {
	plan := planAccountUnban(accountUnbanInput{
		TypedValue: "1000000001",
		AccountID:  testAccountID,
		Username:   "1000000001",
	})
	if !plan.Resolved {
		t.Fatal("解析到账号时 Resolved 应为 true")
	}
	if plan.AccountID != testAccountID {
		t.Fatalf("AccountID = %q，期望 %q", plan.AccountID, testAccountID)
	}
	assertTargets(t, plan, []string{"account=" + testAccountID})
}

// TestPlanAccountUnban_LiftsRelatedIPAndMachine：与一键三封对称——
// 解封账号时把该账号名下（username 关联）与最近 IP/机器码对应的封禁一并解除。
func TestPlanAccountUnban_LiftsRelatedIPAndMachine(t *testing.T) {
	plan := planAccountUnban(accountUnbanInput{
		TypedValue: "1000000001",
		AccountID:  testAccountID,
		Username:   "1000000001",
		LastIP:     "1.2.3.4",
		MachineID:  "hw:aa11",
		Related: []store.BanRecord{
			{BanType: store.BanIP, BanValue: "5.6.7.8", Username: "1000000001"},
			{BanType: store.BanMachine, BanValue: "hw:bb22", Username: "1000000001"},
		},
	})
	assertTargets(t, plan, []string{
		"account=" + testAccountID,
		"ip=5.6.7.8",
		"machine=hw:bb22",
		"ip=1.2.3.4",
		"machine=hw:aa11",
	})
}

// TestPlanAccountUnban_DedupesSameTarget：关联行与"最近IP/机器码"重合时只解一次
// （同一 (type,value) 在库里本来就是 UNIQUE 一行）。
func TestPlanAccountUnban_DedupesSameTarget(t *testing.T) {
	plan := planAccountUnban(accountUnbanInput{
		TypedValue: "1000000001",
		AccountID:  testAccountID,
		Username:   "1000000001",
		LastIP:     "1.2.3.4",
		MachineID:  "hw:aa11",
		Related: []store.BanRecord{
			{BanType: store.BanIP, BanValue: "1.2.3.4"},
			{BanType: store.BanMachine, BanValue: "hw:aa11"},
			{BanType: store.BanIP, BanValue: "1.2.3.4"}, // 重复行（防御性）
		},
	})
	assertTargets(t, plan, []string{
		"account=" + testAccountID,
		"ip=1.2.3.4",
		"machine=hw:aa11",
	})
}

// TestPlanAccountUnban_RemovesDuplicateAccountTarget：关联里混进账号维度时不得重复加。
func TestPlanAccountUnban_IgnoresNonIPMachineRelated(t *testing.T) {
	plan := planAccountUnban(accountUnbanInput{
		TypedValue: "1000000001",
		AccountID:  testAccountID,
		Username:   "1000000001",
		Related: []store.BanRecord{
			{BanType: store.BanAccount, BanValue: testAccountID},
			{BanType: "triple", BanValue: "x"}, // 非法维度也要忽略
		},
	})
	assertTargets(t, plan, []string{"account=" + testAccountID})
}

// TestPlanAccountUnban_UnresolvedFallsBackToExactValue：
// 账号库里没有（例如老库迁移来的 dnf:<md5> 直封）→ 保留原值精确匹配，行为与修复前一致。
func TestPlanAccountUnban_UnresolvedFallsBackToExactValue(t *testing.T) {
	const legacyID = "dnf:992d0c4d27b0493a4a1a4eaa337d266e"
	plan := planAccountUnban(accountUnbanInput{TypedValue: " " + legacyID + " "})
	if plan.Resolved {
		t.Fatal("没解析到账号时 Resolved 应为 false")
	}
	assertTargets(t, plan, []string{"account=" + legacyID})
}

// TestPlanAccountUnban_SkipsEmptyFields：没有最近IP/机器码时不能塞空目标
// （空值写库会得到 ban-target-invalid）。
func TestPlanAccountUnban_SkipsEmptyFields(t *testing.T) {
	plan := planAccountUnban(accountUnbanInput{
		TypedValue: "1000000001",
		AccountID:  testAccountID,
		Username:   "1000000001",
		LastIP:     "   ",
		MachineID:  "",
	})
	assertTargets(t, plan, []string{"account=" + testAccountID})
}

// TestLiftedBansOut：解封结果要按列表接口同一套字段回给调用方，
// 运营才能从响应里看到"到底解除了哪几条"。
func TestLiftedBansOut(t *testing.T) {
	now := time.Date(2026, 9, 24, 1, 30, 0, 0, time.Local)
	out := liftedBansOut([]store.BanRecord{{
		ID:        7,
		BanType:   store.BanAccount,
		BanValue:  testAccountID,
		Username:  "1000000001",
		RevokedAt: sql.NullTime{Time: now, Valid: true},
		BannedAt:  now.Add(-2 * time.Minute),
	}})
	if len(out) != 1 {
		t.Fatalf("len = %d，期望 1", len(out))
	}
	got := out[0]
	if got.ID != 7 || got.BanType != "account" || got.BanValue != testAccountID {
		t.Fatalf("字段没对齐：%+v", got)
	}
	if got.State != "已解封" {
		t.Fatalf("state = %q，期望 已解封", got.State)
	}
	if got.RevokedAt == "" {
		t.Fatal("revoked_at 为空，调用方无法确认解封时间")
	}
}
