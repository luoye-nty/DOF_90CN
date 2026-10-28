package dnfbridge

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

// 两笔实测请求（2026-09-15，packet_log.txt），字段含义靠比对数据库里的 list 38
// 逐个确认：
//
//	38:0  = 100380023 耀眼的勋章（稀有度 0）
//	38:1  = 100380023 耀眼的勋章（稀有度 0）
//	38:49 = 90002     耀眼之光守护珠（稀有度 2）×10
const (
	liveCompoundFlagBody = "fd3de600a2f38b22d8e0145b00" +
		"02000000" + "00000000" + "0000000001000000ffffffffffffffff"
	liveCompoundGemBody = "f8c2f548809d0a52d89ebc3a80" +
		"04000000" + "02000000" + "31000000310000003100000031000000"
)

func TestParseCurrentGuildCompoundRequestMatchesTheLiveSamples(t *testing.T) {
	for _, row := range []struct {
		name      string
		body      string
		wantFlag  byte
		wantCount uint32
		wantRare  uint32
		wantSlots []int16
	}{
		{
			name: "勋章合成", body: liveCompoundFlagBody,
			wantFlag: 0x00, wantCount: 2, wantRare: 0, wantSlots: []int16{0, 1},
		},
		{
			name: "守护珠合成", body: liveCompoundGemBody,
			wantFlag: 0x80, wantCount: 4, wantRare: 2, wantSlots: []int16{49, 49, 49, 49},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			body, err := hex.DecodeString(row.body)
			if err != nil {
				t.Fatalf("fixture is not hex: %v", err)
			}
			if len(body) != currentGuildCompoundRequestSize {
				t.Fatalf("fixture is %d bytes, want %d", len(body), currentGuildCompoundRequestSize)
			}
			request, err := parseCurrentGuildCompoundRequest(body)
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			if request.Flag != row.wantFlag {
				t.Fatalf("flag = %#x, want %#x", request.Flag, row.wantFlag)
			}
			if request.Count != row.wantCount {
				t.Fatalf("count = %d, want %d", request.Count, row.wantCount)
			}
			if request.Rarity != row.wantRare {
				t.Fatalf("rarity = %d, want %d", request.Rarity, row.wantRare)
			}
			if len(request.Slots) != len(row.wantSlots) {
				t.Fatalf("slots = %v, want %v", request.Slots, row.wantSlots)
			}
			for index, slot := range row.wantSlots {
				if request.Slots[index] != slot {
					t.Fatalf("slots = %v, want %v", request.Slots, row.wantSlots)
				}
			}
		})
	}
}

func TestParseCurrentGuildCompoundRequestRejectsMalformedBodies(t *testing.T) {
	good, _ := hex.DecodeString(liveCompoundGemBody)

	if _, err := parseCurrentGuildCompoundRequest(good[:20]); !errors.Is(err, errCurrentGuildCompoundRequestMalformed) {
		t.Fatalf("short body error = %v", err)
	}

	// count 与实际填了几个槽必须一致，否则就是伪造的请求。
	mismatched := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(mismatched[13:17], 3)
	if _, err := parseCurrentGuildCompoundRequest(mismatched); !errors.Is(err, errCurrentGuildCompoundRequestMalformed) {
		t.Fatalf("count mismatch error = %v", err)
	}

	// 槽位必须落在 list 38 之内。
	outOfRange := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(outOfRange[21:25], 9999)
	if _, err := parseCurrentGuildCompoundRequest(outOfRange); !errors.Is(err, errCurrentGuildCompoundRequestMalformed) {
		t.Fatalf("slot range error = %v", err)
	}

	// 一个槽都没填也不行。
	empty := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(empty[13:17], 0)
	for index := 0; index < currentGuildCompoundSlotCount; index++ {
		// 0xFFFFFFFF 就是 currentGuildCompoundEmptySlot(-1) 的线上写法。
		binary.LittleEndian.PutUint32(empty[21+index*4:25+index*4], 0xFFFFFFFF)
	}
	if _, err := parseCurrentGuildCompoundRequest(empty); !errors.Is(err, errCurrentGuildCompoundRequestMalformed) {
		t.Fatalf("empty request error = %v", err)
	}
}

// 成功包必须是 [1] + u32 产物 ID：sub_1D2A7E0 / sub_1D2A660 在成功分支上只读一个
// u32，多读少读都会错位。
func TestBuildCurrentGuildCompoundSuccessBody(t *testing.T) {
	body := buildCurrentGuildCompoundSuccessBody(100380031)
	if len(body) != 5 {
		t.Fatalf("body len = %d, want 5", len(body))
	}
	if body[0] != 1 {
		t.Fatalf("success flag = %d, want 1", body[0])
	}
	if got := binary.LittleEndian.Uint32(body[1:5]); got != 100380031 {
		t.Fatalf("result item = %d, want 100380031", got)
	}
}

// 失败码不能落在 sub_32DDC70 的通用码表里，否则客户端会弹一句和合成无关的话。
func TestCurrentGuildCompoundFailureCodesAvoidTheGenericTable(t *testing.T) {
	generic := map[byte]struct{}{
		12: {}, 54: {}, 77: {}, 105: {}, 114: {}, 115: {}, 122: {}, 134: {}, 136: {},
		137: {}, 167: {}, 208: {}, 210: {}, 214: {}, 229: {}, 230: {}, 232: {}, 234: {},
		237: {}, 239: {},
	}
	for name, code := range map[string]byte{
		"roll_failed": currentGuildCompoundErrorRollFailed,
		"rejected":    currentGuildCompoundErrorRejected,
		"no_space":    currentGuildCompoundErrorNoSpace,
	} {
		if _, taken := generic[code]; taken {
			t.Fatalf("%s code %d is claimed by the generic sub_32DDC70 table", name, code)
		}
	}
	// 95 是 op864/op835 自己码表里的「请整理物品栏的空间后再试。」，必须留给它。
	if currentGuildCompoundErrorNoSpace != 95 {
		t.Fatalf("no-space code = %d, want 95 (dstr 33345)", currentGuildCompoundErrorNoSpace)
	}
	// 另外两个必须落在默认分支上：60/95 都被 handler 显式接管，60 会弹
	//「正在使用个人商店， 无法进行合成。」，那句在合成失败时是错的。
	for name, code := range map[string]byte{
		"roll_failed": currentGuildCompoundErrorRollFailed,
		"rejected":    currentGuildCompoundErrorRejected,
	} {
		if code == 60 || code == 95 {
			t.Fatalf("%s code = %d, it must fall through to the default branch", name, code)
		}
	}
}

// 掷骰失败的文案只能靠 0x298 公告窗顶替：这个包的原生码表里没有"合成失败"，
// 而失败 ACK 又必须发（0x01D2A948 对窗口 430 调 sub_E622C0(1) 解锁界面）。
// 顶替帧走的是和 ACK 弹窗同一个窗口，90CN.dll 开窗前会先关掉已开的那个。
func TestCurrentGuildCompoundFailureNoticeUsesTheAnnouncementWindow(t *testing.T) {
	if noticeSurfaceNativeWindow.Backend != noticeBackendNativeWindow {
		t.Fatalf("failure notice surface = %+v, want the 0x298 announcement window",
			noticeSurfaceNativeWindow)
	}
	if currentGuildCompoundFailureNotice != "合成失败" {
		t.Fatalf("failure notice text = %q, want dstr 19212 的原文",
			currentGuildCompoundFailureNotice)
	}
}

// 守护珠同属性升档：900<属性><档位>，所以同一个 ID 四件的产物就是 ID+1。
func TestCurrentGuildCompoundGemFamilySuccessor(t *testing.T) {
	candidates := []uint32{90003, 90013, 90023}
	if got, ok := currentGuildCompoundGemFamilySuccessor([]uint32{90002, 90002, 90002, 90002}, 3, candidates); !ok || got != 90003 {
		t.Fatalf("successor = %d, %t; want 90003, true", got, ok)
	}
	// 材料属性不一致时没有"同属性"可言，退回随机。
	if _, ok := currentGuildCompoundGemFamilySuccessor([]uint32{90002, 90012, 90002, 90002}, 3, candidates); ok {
		t.Fatal("mixed materials must not resolve to a family successor")
	}
	// 推出来的 ID 必须真的存在于候选集里。
	if _, ok := currentGuildCompoundGemFamilySuccessor([]uint32{90122, 90122}, 3, []uint32{90003}); ok {
		t.Fatal("a successor missing from the candidate set must fall back to random")
	}
}

func TestCurrentGuildCompoundPickResultPrefersTheGemFamily(t *testing.T) {
	got, err := currentGuildCompoundPickResult(
		dnfguild.CompoundGem, []uint32{90002, 90002, 90002, 90002}, 3, []uint32{90003, 90013})
	if err != nil || got != 90003 {
		t.Fatalf("pick = %d, %v; want 90003", got, err)
	}
	// 勋章没有同族对应关系，只能随机，但必须落在候选集里。
	medals := []uint32{100380031, 100380032, 100380033}
	for attempt := 0; attempt < 32; attempt++ {
		picked, err := currentGuildCompoundPickResult(
			dnfguild.CompoundFlag, []uint32{100380021, 100380021, 100380021, 100380021}, 2, medals)
		if err != nil {
			t.Fatalf("pick error = %v", err)
		}
		found := false
		for _, candidate := range medals {
			if candidate == picked {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("picked %d, which is not a rarity-2 medal", picked)
		}
	}
	if _, err := currentGuildCompoundPickResult(dnfguild.CompoundFlag, nil, 2, nil); !errors.Is(err, errCurrentGuildCompoundNoCandidate) {
		t.Fatalf("empty candidate error = %v", err)
	}
}

// 建候选索引时必须先按路径预筛。没有它要对 PVF 里每一件可堆叠物与装备各跑一次
// ResolveItem，实测第一次合成因此卡了 8.66 秒——玩家看到的就是"合成会卡住一段
// 时间"。这里钉住预筛判据本身，别被人顺手删掉。
func TestCurrentGuildCompoundPathPrefilter(t *testing.T) {
	catalog := &pvfDungeonDropCatalog{
		itemRefs: map[uint32]dungeonDropItemReference{
			90002:     {path: "stackable/flaggem/90002.stk"},
			100380031: {path: "equipment/character/common/flag/100380031.equ"},
			3011:      {path: "stackable/material/3011.stk"},
		},
	}
	for itemID, want := range map[uint32]bool{
		90002:     true,
		100380031: true,
		3011:      false,
		999999:    false, // 不在 itemRefs 里
	} {
		if got := currentGuildCompoundPathMayHold(catalog, itemID); got != want {
			t.Fatalf("path prefilter for %d = %t, want %t", itemID, got, want)
		}
	}
}
