package store

// 登录器「服务器公告」栏的纯函数测试。
//
// 这里钉住的是"保存公告板时不该悄悄改坏运营输入"：
//   - 标题两侧空白要去掉，但空标题必须拒绝（否则登录页会出现一行空白公告）；
//   - 日期留空是允许的（有些公告不想显示日期），但填了就必须是 YYYY-MM-DD，
//     否则登录器要么显示成原始字符串、要么整行日期错位。

import (
	"strings"
	"testing"
)

func TestNormalizeNoticeTitle(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"常规", "  9 月 24 日例行维护公告 ", "9 月 24 日例行维护公告", false},
		{"空标题", "   ", "", true},
		{"全角空格也算空", "　", "", true},
		{"60 字刚好", strings.Repeat("告", 60), strings.Repeat("告", 60), false},
		{"61 字超限", strings.Repeat("告", 61), "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeNoticeTitle(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s：期望报错，实际通过（%q）", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s：期望通过，实际 %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s：NormalizeNoticeTitle = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeNoticeDate(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false}, // 留空=不显示日期
		{"   ", "", false},
		{"2026-09-24", "2026-09-24", false},
		{" 2026-9-4 ", "2026-09-04", false}, // 补零成规范形式
		{"2026/09/24", "", true},
		{"2026-13-01", "", true},
		{"2026-02-30", "", true},
		{"26-09-24", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeNoticeDate(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("normalizeNoticeDate(%q)：期望报错，实际通过（%q）", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeNoticeDate(%q)：期望通过，实际 %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeNoticeDate(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
