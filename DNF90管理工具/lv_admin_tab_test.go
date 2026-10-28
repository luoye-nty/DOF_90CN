package main

// 登录器版本校验：直链前端预校验的纯函数测试（不建窗口、不连服务器）。
//
// 口径必须与服务器端 manage.launcherVersionMaxURLLen / 主程序 lvNormalizeURL 完全一致，
// 否则会出现"本工具说没问题、服务器 400"或反过来的尴尬。
// 最要紧的一条：**空串必须合法** —— 它对应需求确认过的"勾选但不填直链 →
// 登录器弹请联系管理员"分支，前端要是把它判成非法，这条路就走不通了。

import (
	"strings"
	"testing"
)

func TestLvValidateURLAccepts(t *testing.T) {
	ok := []string{
		"",                             // 空 = 未配置直链（合法，对应"请联系管理员"分支）
		"   ",                          // 只有空白视同未配置
		"https://example.com/a.zip",
		"http://1.2.3.4:8080/a/b.7z",
		"https://cdn.example.com/d?f=1&t=2",
		"  https://example.com/x.rar  ", // 首尾空白由调用方 TrimSpace 处理，这里也要放行
	}
	for _, in := range ok {
		if msg, valid := lvValidateURL(in); !valid {
			t.Fatalf("lvValidateURL(%q) 应当通过，实际被拒：%s", in, msg)
		}
	}
}

func TestLvValidateURLRejects(t *testing.T) {
	bad := []struct {
		name string
		in   string
	}{
		{"缺协议头", "example.com/a.zip"},
		{"ftp 不支持", "ftp://example.com/a.zip"},
		{"只有协议没主机名", "https://"},
		{"主机名是空白", "https://   /a.zip"},
		{"含中文", "https://example.com/登录器.zip"},
		{"含空格", "https://example.com/my launcher.zip"},
		{"含换行（粘贴带进来的）", "https://example.com/a.zip\nhttps://example.com/b.zip"},
		{"超长", "https://example.com/" + strings.Repeat("a", lvURLMaxLen)},
	}
	for _, tc := range bad {
		if msg, valid := lvValidateURL(tc.in); valid {
			t.Fatalf("%s：应当被拒，实际通过（%q）", tc.name, tc.in)
		} else if msg == "" {
			t.Fatalf("%s：被拒时必须给出可读原因", tc.name)
		}
	}
}

// TestLvValidateURLMaxLenMatchesServer 前端长度上限必须与服务器端同值。
// 两边不一致时，运营会先在本工具里填进去、保存时才被服务器拒，
// 报错还指向不了具体原因。
func TestLvValidateURLMaxLenMatchesServer(t *testing.T) {
	// 与运维工具 internal/manage/handlers_launcher_version.go 的
	// launcherVersionMaxURLLen 同口径。
	const serverLimit = 2048
	if lvURLMaxLen != serverLimit {
		t.Fatalf("前端直链长度上限 %d 与服务器端 %d 不一致", lvURLMaxLen, serverLimit)
	}
}

// TestLvVersionText 版本号显示文案：空值要给可读提示，不能渲染成空白。
func TestLvVersionText(t *testing.T) {
	if got := lvVersionText("1.5.5"); got != "V1.5.5" {
		t.Fatalf("lvVersionText(1.5.5) = %q, want V1.5.5", got)
	}
	if got := lvVersionText("  1.5.5  "); got != "V1.5.5" {
		t.Fatalf("lvVersionText 应去空白，实际 %q", got)
	}
	if got := lvVersionText(""); got == "" || got == "V" {
		t.Fatalf("空版本号必须给出可读提示，实际 %q", got)
	}
}
