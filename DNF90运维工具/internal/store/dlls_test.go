package store

// 「服务端指定 DLL」的纯函数测试。
//
// 这里钉住的是"上传什么就存什么、不能有惊喜"：
//   - 文件名必须**只是文件名**：带路径/盘符/上跳的一律拒绝
//     （登录器只在客户端根目录找它，存进去一个 ..\..\ 就等于给登录器挖坑）；
//   - 哈希必须是小写 64 位 hex（大小写混写在写库前统一，避免登录器比对失败）；
//   - 未知的失败策略一律按最保守的"跳过注入"处理。

import (
	"strings"
	"testing"
)

func TestNormalizeDLLFileName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"90CN.dll", "90CN.dll"},
		{"  90CN.dll  ", "90CN.dll"},
		{"MyMod.DLL", "MyMod.DLL"},
		{"中文模块.dll", "中文模块.dll"},
	}
	for _, tc := range ok {
		got, err := NormalizeDLLFileName(tc.in)
		if err != nil {
			t.Fatalf("NormalizeDLLFileName(%q) 期望通过，实际 %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeDLLFileName(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"", "   ",
		`C:\Windows\System32\evil.dll`,
		`..\..\evil.dll`,
		`sub/evil.dll`,
		`sub\evil.dll`,
		"..",
		"90CN.txt",
		"90CN",
	}
	for _, in := range bad {
		if got, err := NormalizeDLLFileName(in); err == nil {
			t.Fatalf("NormalizeDLLFileName(%q) 应被拒绝，实际通过并返回 %q", in, got)
		}
	}
}

func TestNormalizeDLLSHA256(t *testing.T) {
	sum := strings.Repeat("a1", 32) // 64 位
	got, err := NormalizeDLLSHA256("  " + strings.ToUpper(sum) + "  ")
	if err != nil {
		t.Fatalf("合法哈希应通过，实际 %v", err)
	}
	if got != sum {
		t.Fatalf("哈希应统一成小写，实际 %q", got)
	}

	bad := []string{
		"",
		"abc",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("z", 64), // 非 hex
		strings.Repeat("a", 63) + "-",
	}
	for _, in := range bad {
		if got, err := NormalizeDLLSHA256(in); err == nil {
			t.Fatalf("NormalizeDLLSHA256(%q) 应被拒绝，实际通过并返回 %q", in, got)
		}
	}
}

func TestNormalizeDLLMismatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"block", DLLMismatchBlock},
		{"BLOCK", DLLMismatchBlock},
		{" block ", DLLMismatchBlock},
		{"skip", DLLMismatchSkip},
		{"", DLLMismatchSkip},
		{"whatever", DLLMismatchSkip}, // 未知值按最保守处理
	}
	for _, tc := range cases {
		if got := normalizeDLLMismatch(tc.in); got != tc.want {
			t.Fatalf("normalizeDLLMismatch(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
