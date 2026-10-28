package store

import "testing"

func TestLooksLikeAccountID(t *testing.T) {
	const sha = "dnf:e3319b58ec68c92d20b223abdb895f90f4bb05ccc7a3ce3bf34c68c7b50ebb3e" // 新库 sha256
	const md5 = "dnf:992d0c4d27b0493a4a1a4eaa337d266e"                                 // 老库 md5（迁移过来的）

	yes := []string{
		sha,
		md5,
		"  " + sha + "  ",          // 前后空白要容忍（输入框复制粘贴很常见）
		"dnf:" + upperHex(sha[4:]), // 大写十六进制也认
	}
	no := []string{
		"",
		"1000000001",                       // 纯用户名
		"dnf:",                             // 只有前缀
		"dnf:abc",                          // 太短
		"dnf:zzzz",                         // 非十六进制
		"e3319b58ec68c92d20b223abdb895f90", // 少了 dnf: 前缀
		"account:1234",
	}
	for _, s := range yes {
		if !LooksLikeAccountID(s) {
			t.Fatalf("LooksLikeAccountID(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if LooksLikeAccountID(s) {
			t.Fatalf("LooksLikeAccountID(%q) = true, want false", s)
		}
	}
}

func upperHex(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 32
		}
	}
	return string(b)
}

// TestMachineBanCandidates 锁"换机器码不丢旧封禁"这条兼容规则：
// 机器维度封禁要按 [上报值(硬件码), 计算机名] 两个候选值依次比对，
// 因为 2026-09-24 之前写入的 machine 封禁行按计算机名存，之后按 hw:<hex> 存。
func TestMachineBanCandidates(t *testing.T) {
	const hwid = "hw:0123456789abcdef0123456789abcdef"

	cases := []struct {
		name string
		in   MachineIdentity
		want []string
	}{
		{"新登录器：硬件码 + 计算机名", MachineIdentity{MachineID: hwid, Name: "DESKTOP-ABC"}, []string{hwid, "DESKTOP-ABC"}},
		{"老登录器：两字段相同只比一次", MachineIdentity{MachineID: "DESKTOP-ABC", Name: "DESKTOP-ABC"}, []string{"DESKTOP-ABC"}},
		{"大小写不同也算同一个值", MachineIdentity{MachineID: "desktop-abc", Name: "DESKTOP-ABC"}, []string{"desktop-abc"}},
		{"只有计算机名（老封禁兜底）", MachineIdentity{Name: "DESKTOP-ABC"}, []string{"DESKTOP-ABC"}},
		{"只有硬件码（新登录器、无计算机名）", MachineIdentity{MachineID: hwid}, []string{hwid}},
		{"前后空白要裁掉", MachineIdentity{MachineID: "  " + hwid + " ", Name: " DESKTOP-ABC "}, []string{hwid, "DESKTOP-ABC"}},
		{"都没有", MachineIdentity{}, nil},
		{"空白视为空", MachineIdentity{MachineID: "   ", Name: "\t"}, nil},
	}
	for _, tc := range cases {
		got := machineBanCandidates(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%s：候选值 = %v，期望 %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s：候选值 = %v，期望 %v", tc.name, got, tc.want)
			}
		}
		if tc.in.empty() != (len(tc.want) == 0) {
			t.Fatalf("%s：empty() = %v，与候选值数量不符", tc.name, tc.in.empty())
		}
	}
}
