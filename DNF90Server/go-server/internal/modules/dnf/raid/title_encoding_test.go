package raid

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 攻坚队名的 dstr 载荷必须是客户端自己的窄字节编码（GB18030），而不是 UTF-16LE。
//
// 实证 —— 2026-09-27 实机抓包（D:\test\...\runtime\logs\packet_log.txt），
// op591 mode=0 的对象基座：
//
//	01 00 17 00 | 00 00 00 00 | 01 00 17 00 | 06 00 00 00 | 3c 79 7d 76 31 00 | ...
//	raid_key      mode=0        raid_key      dstr len=6    "礼白1" 的 UTF-16LE
//
// 同一包里紧随其后的队长记录用的是 c0 f1 b0 d7 36 36 36 = GB18030 "礼白666"。
// 两者用同一个读取器：sub_3457D20(dst, 0x100, GetString(0x29D))，它和线上其它
// dstr 一样读「u32 长度 + 窄字节」，再逐字节宽化（0x3457D20 -> 0x34D9CA0）。
//
// 所以 UTF-16LE 载荷会被当成当前代码页逐字节渲染，症状由名字字节决定：
//
//	"1111"  -> 31 00 31 00 ...  读到第一个 00 就结束，只显示 "1"
//	"礼白1" -> 3c 79 7d 76 31 00 逐字节 = '<' 'y' '}' 'v' '1'，即 "<y}v1"
//
// 86 参考端同族：RaidManager.Create 存请求自带的 TitleBytes，RaidPacketBuilder
// .WriteRaidObject 用 WriteRawDstr 原样发出；其 RaidHandler 自测把抓包到的
// CREATE_RAID 正文钉成窄字节 "123"（31 32 33），不是 31 00 32 00 33 00。
func TestRaidObjectTitleCarriesClientEncodedBytesNotUTF16(t *testing.T) {
	// 礼白1 的 GB18030 字节：C0 F1(礼) B0 D7(白) 31(1) —— 5 字节，奇数长度。
	gb18030 := []byte{0xc0, 0xf1, 0xb0, 0xd7, 0x31}
	// 同一串的 UTF-16LE 拼写，正是实机抓到的那 6 个字节。
	utf16LE := []byte{0x3c, 0x79, 0x7d, 0x76, 0x31, 0x00}

	object := RaidObjectMode0{
		RaidKey:    0x00170001,
		TitleBytes: gb18030,
		Leader:     MemberRecord{CharKey: 1, RawNameBytes: []byte("leader")},
		Members:    []MemberRecord{{CharKey: 1, RawNameBytes: []byte("leader")}},
	}

	body, err := BuildRaidObjectMode0Body(object)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	// op591 = u32 raid_key + u32 mode + [对象基座]，标题 dstr 因此在 12。
	if got := raidTitleDstrAt(t, body, 12); !bytes.Equal(got, gb18030) {
		t.Fatalf("op591 标题 dstr = % X，want % X（客户端 GB18030 字节）", got, gb18030)
	}
	if bytes.Contains(body, utf16LE) {
		t.Fatalf("op591 包里仍有 UTF-16LE 拼写 % X", utf16LE)
	}

	// op753 的公共目录行 = 三个 u32 页头（12 字节）+ 同一个对象基座。
	public, err := BuildPublicRaidListBody(PublicRaidListPage{TotalCount: 1, Objects: []RaidObjectMode0{object}})
	if err != nil {
		t.Fatalf("BuildPublicRaidListBody error = %v", err)
	}
	if got := raidTitleDstrAt(t, public, 16); !bytes.Equal(got, gb18030) {
		t.Fatalf("op753 标题 dstr = % X，want % X（客户端 GB18030 字节）", got, gb18030)
	}

	// 奇数长度的 GB18030 名必须能构造：旧校验要求 len%2==0，"礼白1" 会被整包拒掉。
	if len(gb18030)%2 == 0 {
		t.Fatal("这条用例本身失效了：GB18030 名字应当是奇数长度")
	}
}

// 纯数字名是症状最容易复现的一档：UTF-16LE 的 31 00 会读到第一个 00 就截断，
// 于是 "1111" 只显示 "1"。窄字节下发后 dstr 长度就是 4。
func TestRaidObjectTitleDigitsOnlyStaysWholeName(t *testing.T) {
	object := RaidObjectMode0{
		RaidKey:    0x00170001,
		TitleBytes: []byte("1111"),
		Leader:     MemberRecord{CharKey: 1, RawNameBytes: []byte("leader")},
	}
	body, err := BuildRaidObjectMode0Body(object)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	if got := len(raidTitleDstrAt(t, body, 12)); got != 4 {
		t.Fatalf("纯数字名 dstr 长 = %d，want 4（UTF-16LE 会是 8）", got)
	}
	if got := raidTitleDstrAt(t, body, 12); !bytes.Equal(got, []byte("1111")) {
		t.Fatalf("纯数字名 dstr = % X，want 31 31 31 31", got)
	}
}

// 空名发空 dstr（u32 长度 0），与 86 参考端一致：客户端会退回自己的兜底串
// （sub_3457D20 的 GetString(0x29D)），而不是收到一个 NUL 名。
func TestRaidObjectEmptyTitleSendsEmptyDstr(t *testing.T) {
	object := RaidObjectMode0{
		RaidKey: 0x00170001,
		Leader:  MemberRecord{CharKey: 1, RawNameBytes: []byte("leader")},
	}
	body, err := BuildRaidObjectMode0Body(object)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	if got := binary.LittleEndian.Uint32(body[12:16]); got != 0 {
		t.Fatalf("空名 dstr 声明长度 = %d，want 0", got)
	}
	if got := raidTitleDstrAt(t, body, 12); len(got) != 0 {
		t.Fatalf("空名 dstr 载荷 = % X，want 空", got)
	}
}

// 客户端读取器的容量是 0x100：声明长度达到它就整格丢弃并退回兜底串，所以在
// 构造层直接拒绝，而不是截断成一个看起来正常、实际被丢弃的名字。
func TestRaidObjectTitleAtReaderCapacityIsRejected(t *testing.T) {
	object := RaidObjectMode0{
		RaidKey:    0x00170001,
		TitleBytes: bytes.Repeat([]byte{'x'}, raidTitleMaxBytes),
		Leader:     MemberRecord{CharKey: 1, RawNameBytes: []byte("leader")},
	}
	if _, err := BuildRaidObjectMode0Body(object); err == nil {
		t.Fatalf("长度 %d 的标题被接受了；客户端读到 0x100 会整格丢弃", raidTitleMaxBytes)
	}

	object.TitleBytes = bytes.Repeat([]byte{'x'}, raidTitleMaxBytes-1)
	if _, err := BuildRaidObjectMode0Body(object); err != nil {
		t.Fatalf("长度 %d 的标题被拒了：%v", raidTitleMaxBytes-1, err)
	}
}

// raidTitleDstrAt 读 offset 处的「u32 长度 + 载荷」，并顺带把带内长度与实际
// 可读字节对齐，避免用例在包被截断时只报一个越界 panic。
func raidTitleDstrAt(t *testing.T, body []byte, offset int) []byte {
	t.Helper()
	if len(body) < offset+4 {
		t.Fatalf("包体长 %d，读不下 offset %d 的 dstr 长度", len(body), offset)
	}
	length := int(binary.LittleEndian.Uint32(body[offset : offset+4]))
	if length < 0 || offset+4+length > len(body) {
		t.Fatalf("dstr 声明长度 %d 越过包尾（包体长 %d，offset %d）", length, len(body), offset)
	}
	return body[offset+4 : offset+4+length]
}
