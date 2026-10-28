package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 攻坚队窗口标题走 runtimeRaidObjectMode0 -> raid.BuildRaidObjectMode0Body。
// 这条路径曾经把队名转成 UTF-16LE（runtimeRaidUTF16LE），而客户端用读窄字节的
// sub_3457D20(dst,0x100,GetString(0x29D)) 读它，于是：
//   - 纯数字队名 "1111" 读到第一个 00 截断，窗口标题只显示 "1"；
//   - "礼白1" 的 UTF-16LE 3c 79 7d 76 31 被逐字节渲染成 "<y}v1"。
//
// 客户端上行本来就发 GB18030，服务端只要原样透传即可（86 参考端同样如此）。
func TestRuntimeRaidObjectMode0KeepsClientEncodedTitle(t *testing.T) {
	// 礼白1 的 GB18030 字节，与实机抓包里队长名 c0 f1 b0 d7 36 36 36 同族。
	nameBytes := []byte{0xc0, 0xf1, 0xb0, 0xd7, 0x31}
	state := runtimeRaidState{
		RaidKey:   0x00170001,
		LeaderID:  1,
		NameBytes: append([]byte(nil), nameBytes...),
		TailFlag:  1,
		Members: []runtimeRaidMemberState{
			{CharID: 1, Name: "礼白1", GroupIndex: 1},
		},
	}

	object := runtimeRaidObjectMode0(state)
	if !bytes.Equal(object.TitleBytes, nameBytes) {
		t.Fatalf("对象标题 = % X，want % X（客户端自带的 GB18030 字节）", object.TitleBytes, nameBytes)
	}

	body, err := raid.BuildRaidObjectMode0Body(object)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	// op591 = u32 raid_key + u32 mode + [对象基座]；对象基座 = u32 raid_key + dstr 标题，
	// 所以 dstr 长度在 12、载荷在 16。
	if got := binary.LittleEndian.Uint32(body[12:16]); got != uint32(len(nameBytes)) {
		t.Fatalf("op591 标题 dstr 声明长度 = %d，want %d（UTF-16LE 会是 %d）",
			got, len(nameBytes), len(nameBytes)*2)
	}
	if payload := body[16 : 16+len(nameBytes)]; !bytes.Equal(payload, nameBytes) {
		t.Fatalf("op591 标题载荷 = % X，want % X", payload, nameBytes)
	}
	if utf16LE := []byte{0x3c, 0x79, 0x7d, 0x76, 0x31, 0x00}; bytes.Contains(body, utf16LE) {
		t.Fatalf("op591 包里仍有 UTF-16LE 拼写 % X", utf16LE)
	}
}

// 纯数字队名单独钉一条：实机症状是「只显示第一位数字」，根因就是 UTF-16LE 的
// 31 00 让窄字节读取器在第一个数字后结束。
func TestRuntimeRaidObjectMode0DigitsOnlyTitleStaysWholeName(t *testing.T) {
	state := runtimeRaidState{
		RaidKey:   0x00170001,
		LeaderID:  1,
		NameBytes: []byte("1111"),
		Members:   []runtimeRaidMemberState{{CharID: 1, Name: "hero", GroupIndex: 1}},
	}
	object := runtimeRaidObjectMode0(state)
	if !bytes.Equal(object.TitleBytes, []byte("1111")) {
		t.Fatalf("对象标题 = % X，want 31 31 31 31", object.TitleBytes)
	}
	body, err := raid.BuildRaidObjectMode0Body(object)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	if got := binary.LittleEndian.Uint32(body[12:16]); got != 4 {
		t.Fatalf("op591 标题 dstr 声明长度 = %d，want 4（UTF-16LE 会是 8）", got)
	}
}

// 反向对照（负例，不是需求）：把已经删掉的 runtimeRaidUTF16LE 原样搬回来，证明
// 上面两条用例断言的确实是这个 bug，而不是自说自话 —— 旧实现对同一个名字产出的
// 正是 2026-09-27 实机抓包里的那 6 个字节，也正是玩家看到的 "<y}v1"。
//
// 抓包原文（packet_log.txt，op591 mode=0）：
//
//	01 00 17 00 | 00 00 00 00 | 01 00 17 00 | 06 00 00 00 | 3c 79 7d 76 31 00
func TestLegacyUTF16TitleConversionReproducesTheReportedSymptom(t *testing.T) {
	gb18030Name := []byte{0xc0, 0xf1, 0xb0, 0xd7, 0x31} // 礼白1
	captured := []byte{0x3c, 0x79, 0x7d, 0x76, 0x31, 0x00}

	legacy := legacyRaidTitleUTF16LE(gb18030Name)
	if !bytes.Equal(legacy, captured) {
		t.Fatalf("旧实现产出 % X，want % X（实机抓到的 op591 标题载荷）", legacy, captured)
	}

	// 客户端按当前代码页逐字节读它，于是 礼白1 显示成 "<y}v1"：前五个字节都是
	// ASCII 就照原样出字形，第六个字节 0x00 解出来正是客户端宽字符串的终止符，
	// 所以窗口标题上可见的正好是五个字符。
	rendered, err := simplifiedchinese.GB18030.NewDecoder().Bytes(legacy)
	if err != nil {
		t.Fatalf("按 GB18030 解释抓包载荷失败：%v", err)
	}
	if string(rendered) != "<y}v1\x00" {
		t.Fatalf("实机显示串 = %q，want %q", string(rendered), "<y}v1\x00")
	}
	if visible := string(rendered[:bytes.IndexByte(rendered, 0)]); visible != "<y}v1" {
		t.Fatalf("标题上可见的字符 = %q，want %q", visible, "<y}v1")
	}

	// 纯数字名同理：31 00 让窄字节读取器在第一个数字后就结束，只显示 "1"。
	digits := legacyRaidTitleUTF16LE([]byte("1111"))
	if !bytes.Equal(digits, []byte{0x31, 0, 0x31, 0, 0x31, 0, 0x31, 0}) {
		t.Fatalf("旧实现的纯数字名载荷 = % X", digits)
	}
	if first := digits[:bytes.IndexByte(digits, 0)]; !bytes.Equal(first, []byte("1")) {
		t.Fatalf("窄字节读到第一个 NUL 为止 = %q，want %q", string(first), "1")
	}
}

// legacyRaidTitleUTF16LE 是被 runtimeRaidTitleBytes 取代的那段实现，逐行照抄，
// 只服务于上面那条反例。
func legacyRaidTitleUTF16LE(raw []byte) []byte {
	if len(raw) > 0 && len(raw)%2 == 0 {
		hasZero := false
		for _, value := range raw {
			if value == 0 {
				hasZero = true
				break
			}
		}
		if hasZero || !utf8.Valid(raw) {
			return append([]byte(nil), raw...)
		}
	}
	value, err := decodeCharacterName(raw)
	if err != nil {
		value = string(raw)
	}
	units := utf16.Encode([]rune(value))
	out := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}
