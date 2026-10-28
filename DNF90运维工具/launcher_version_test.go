package main

// 登录器版本校验：直链归一化 / 配置落盘 / 对外快照的纯逻辑测试。
//
// 这里锁住的是"最坏情况"：
//  1. 内置版本号为空（运营手滑改坏常量）→ 必须对外按**未开启校验**返回，
//     否则全服玩家会被一句"请联系管理员"永久锁在更新界面外；
//  2. "开启校验 + 直链为空"是**合法**配置（需求确认过的分支：登录器弹请联系管理员），
//     不能被静默改成"关闭校验"，也不能被拒；
//  3. 直链必须校验到"能真的下载"的程度（http/https + 有主机名 + 无中文空格），
//     否则玩家更新时才发现下不动。
//
// 注意：这些用例不启动 GUI、不连网络，只跑纯函数。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
)

// TestLVNormalizeURLAcceptsAndRejects 直链归一化：合法原样保留（去首尾空白），非法一律报错。
func TestLVNormalizeURLAcceptsAndRejects(t *testing.T) {
	ok := []struct {
		name string
		in   string
		want string
	}{
		{"空串合法（= 未配置直链）", "", ""},
		{"只有空白视同未配置", "   \t\r\n ", ""},
		{"https 直链", "https://example.com/launcher.zip", "https://example.com/launcher.zip"},
		{"http 直链", "http://1.2.3.4:8080/a/b.7z", "http://1.2.3.4:8080/a/b.7z"},
		{"首尾空白被去掉", "  https://example.com/x.rar  ", "https://example.com/x.rar"},
		{"带查询串", "https://cdn.example.com/d?f=1&t=2", "https://cdn.example.com/d?f=1&t=2"},
	}
	for _, tc := range ok {
		got, err := lvNormalizeURL(tc.in)
		if err != nil {
			t.Fatalf("%s: 不应报错，实际 %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: 归一化 = %q，期望 %q", tc.name, got, tc.want)
		}
	}

	bad := []struct {
		name string
		in   string
	}{
		{"缺协议头", "example.com/launcher.zip"},
		{"ftp 不支持", "ftp://example.com/launcher.zip"},
		{"file 协议不支持", "file:///C:/launcher.zip"},
		{"只有协议没主机名", "https://"},
		{"主机名是空白", "https://   /a.zip"},
		{"含中文", "https://example.com/登录器.zip"},
		{"含空格", "https://example.com/my launcher.zip"},
		{"含换行（粘贴带进来的）", "https://example.com/a.zip\nhttps://example.com/b.zip"},
		{"超长", "https://example.com/" + strings.Repeat("a", lvMaxURLLen)},
	}
	for _, tc := range bad {
		if got, err := lvNormalizeURL(tc.in); err == nil {
			t.Fatalf("%s: 应当报错，实际通过并返回 %q", tc.name, got)
		}
	}
}

// TestLVApplyPersistsAndRestores 应用 → 落盘 → 重新加载，开关与直链都不能丢。
// 特别是"开关必须落盘"：运维工具重启后如果悄悄变成不校验，等于版本校验被静默撤销了。
func TestLVApplyPersistsAndRestores(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a := &App{cfg: cfg}

	const wantURL = "https://cdn.example.com/launcher-1.5.6.zip"
	st, err := a.lvApply(context.Background(), true, "  "+wantURL+"  ")
	if err != nil {
		t.Fatalf("lvApply: %v", err)
	}
	if !st.Enabled {
		t.Fatal("开启校验后快照里 enabled 应为 true")
	}
	if st.DownloadURL != wantURL {
		t.Fatalf("直链应归一化为 %q，实际 %q", wantURL, st.DownloadURL)
	}
	if st.RequiredVersion != lvEffectiveVersion() {
		t.Fatalf("快照里的只读版本号 = %q，期望内置常量 %q", st.RequiredVersion, lvEffectiveVersion())
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新加载配置: %v", err)
	}
	if !reloaded.LauncherVersionCheck {
		t.Fatal("重启后校验开关丢了（必须落盘）")
	}
	if reloaded.LauncherDownloadURL != wantURL {
		t.Fatalf("重启后直链 = %q，期望 %q", reloaded.LauncherDownloadURL, wantURL)
	}

	// 关闭校验：直链保留（下次再开不用重填），开关置 false。
	st, err = a.lvApply(context.Background(), false, reloaded.LauncherDownloadURL)
	if err != nil {
		t.Fatalf("关闭校验: %v", err)
	}
	if st.Enabled {
		t.Fatal("关闭后 enabled 应为 false")
	}
	again, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("再次加载配置: %v", err)
	}
	if again.LauncherVersionCheck {
		t.Fatal("重新加载后仍是开启状态")
	}
	if again.LauncherDownloadURL != wantURL {
		t.Fatalf("关闭校验不应清空直链，实际 %q", again.LauncherDownloadURL)
	}
}

// TestLVApplyAllowsEnabledWithoutURL 这是需求里"登录器弹窗提示请联系管理员"的那条分支：
// 开启校验但没填直链，必须**保存成功**（不能报错，也不能被静默关掉）。
func TestLVApplyAllowsEnabledWithoutURL(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a := &App{cfg: cfg}

	st, err := a.lvApply(context.Background(), true, "")
	if err != nil {
		// 这个分支对应"登录器弹窗提示请联系管理员"，保存必须成功。
		t.Fatalf("开启校验 + 空直链必须被接受，实际报错 %v", err)
	}
	if !st.Enabled {
		t.Fatal("开关应为 true")
	}
	if st.DownloadURL != "" {
		t.Fatalf("直链应为空，实际 %q", st.DownloadURL)
	}

	// 非法直链必须整批拒绝：不能出现"开关已改、直链没写进去"的半写入状态。
	enabledBefore := cfg.LauncherVersionCheck
	urlBefore := cfg.LauncherDownloadURL
	if _, err := a.lvApply(context.Background(), true, "登录器.zip"); err == nil {
		t.Fatal("非法直链（缺协议头 + 含中文）应当报错")
	}
	if cfg.LauncherVersionCheck != enabledBefore || cfg.LauncherDownloadURL != urlBefore {
		t.Fatalf("非法直链被拒后配置不应发生任何变化：enabled %v→%v, url %q→%q",
			enabledBefore, cfg.LauncherVersionCheck, urlBefore, cfg.LauncherDownloadURL)
	}
}

// TestLVStatusDefaultsOffWithoutConfig 配置指针为 nil 的 App 也必须安全
// （认证服务可能在配置加载前被调用）。
func TestLVStatusDefaultsOffWithoutConfig(t *testing.T) {
	empty := &App{}
	st := empty.lvStatus()
	if st.Enabled {
		t.Fatalf("无配置时应视为未开启校验，实际 %+v", st)
	}
	if st.DownloadURL != "" {
		t.Fatalf("无配置时直链应为空，实际 %q", st.DownloadURL)
	}
	// 内置版本号是编译期常量，即使配置没加载也应该能读出来给管理工具显示。
	if st.RequiredVersion != lvEffectiveVersion() {
		t.Fatalf("无配置时版本号 = %q，期望 %q", st.RequiredVersion, lvEffectiveVersion())
	}
}

// TestBuiltinLauncherVersionSane 内置版本号本身必须是干净可用的：
// 带空白/带 "v" 前缀会让登录器那条"完全一致"比对永远失败（全服都进不去）。
func TestBuiltinLauncherVersionSane(t *testing.T) {
	v := lvEffectiveVersion()
	if v == "" {
		t.Fatal("内置登录器版本号不能为空（发布前必须填）")
	}
	if v != strings.TrimSpace(v) {
		t.Fatalf("内置版本号首尾不能有空白：%q", v)
	}
	if strings.HasPrefix(v, "v") || strings.HasPrefix(v, "V") {
		t.Fatalf("内置版本号不要带 v 前缀（登录器侧同样不带）：%q", v)
	}
	if !strings.Contains(v, ".") {
		t.Fatalf("内置版本号看起来不是 x.y.z 形式：%q", v)
	}
}
