package main

// 更新维护：白名单归一化与配置读写的纯函数测试。
//
// 维护白名单形态最容易出错的两点：
//  1. 忘记归一化 → 配置里写 "Admin"、玩家填 "admin" 就放行不了（管理员被自己关在门外）；
//  2. 忘记去重/去空 → 管理工具里手打一串带空格逗号的名单会把空项也写进配置。

import (
	"context"
	"path/filepath"
	"testing"

	"dnf90ops/internal/config"
)

func TestMaintNormalizeAllowUsers(t *testing.T) {
	got := maintNormalizeAllowUsers([]string{"Admin", " admin ", "", "  ", "ops", "OPS", "player1"})
	want := []string{"admin", "ops", "player1"}
	if len(got) != len(want) {
		t.Fatalf("归一化结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("归一化结果 = %v，期望 %v（顺序也要稳定）", got, want)
		}
	}
	if empty := maintNormalizeAllowUsers(nil); len(empty) != 0 {
		t.Fatalf("空输入应得到空名单，实际 %v", empty)
	}
}

func TestMaintPublicStateDefaultsOff(t *testing.T) {
	// 配置指针为 nil 的 App 也必须安全（认证服务可能在配置加载前被调用）。
	empty := &App{}
	if st := empty.maintPublicState(); st.Enabled || len(st.AllowUsers) != 0 {
		t.Fatalf("无配置时应视为未维护，实际 %+v", st)
	}
}

// TestMaintApplyPersistsAndRestores 应用 → 读回 → 落盘 → 重新加载，开关与白名单都不能丢。
// 特别是"维护状态必须落盘"：运维工具重启后如果悄悄变成未维护，等于维护被撤销了都没人知道。
func TestMaintApplyPersistsAndRestores(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a := &App{cfg: cfg}

	st, err := a.maintApply(context.Background(), true, []string{"Admin", "ops", "ops"})
	if err != nil {
		t.Fatalf("maintApply: %v", err)
	}
	if !st.Enabled {
		t.Fatal("开启维护后快照里 enabled 应为 true")
	}
	if len(st.AllowUsers) != 2 || st.AllowUsers[0] != "admin" || st.AllowUsers[1] != "ops" {
		t.Fatalf("白名单应去重归一化为 [admin ops]，实际 %v", st.AllowUsers)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.MaintenanceMode {
		t.Fatal("重新加载后维护开关丢了（必须落盘）")
	}
	if len(reloaded.MaintenanceAllowUsers) != 2 {
		t.Fatalf("重新加载后白名单 = %v", reloaded.MaintenanceAllowUsers)
	}

	// 关闭维护：白名单保留（下次再开不用重填），开关置 false。
	st, err = a.maintApply(context.Background(), false, reloaded.MaintenanceAllowUsers)
	if err != nil {
		t.Fatalf("关闭维护: %v", err)
	}
	if st.Enabled {
		t.Fatal("关闭后 enabled 应为 false")
	}
	again, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload2: %v", err)
	}
	if again.MaintenanceMode {
		t.Fatal("重新加载后仍是维护中")
	}
	if len(again.MaintenanceAllowUsers) != 2 {
		t.Fatalf("关闭维护不应清空白名单，实际 %v", again.MaintenanceAllowUsers)
	}
}

// TestMaintStatusMatchesPublicState：管理端展示的快照必须与认证服务读到的状态同源，
// 否则会出现"管理工具显示维护中、玩家却能进"这种最坏情况。
func TestMaintStatusMatchesPublicState(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a := &App{cfg: cfg}
	if _, err := a.maintApply(context.Background(), true, []string{"admin"}); err != nil {
		t.Fatalf("maintApply: %v", err)
	}
	status := a.maintStatus()
	public := a.maintPublicState()
	if status.Enabled != public.Enabled {
		t.Fatalf("管理端 enabled=%v，认证服务读到 %v", status.Enabled, public.Enabled)
	}
	if len(status.AllowUsers) != len(public.AllowUsers) || len(status.AllowUsers) != 1 {
		t.Fatalf("白名单不同源：管理端 %v，认证服务 %v", status.AllowUsers, public.AllowUsers)
	}
}
