package inventory

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfweaknesspotion "longheng.io/server/internal/modules/dnf/weaknesspotion"
)

// 使用防虚弱药剂后，服务端必须自己记住效果：客户端只在本地生效（活体抓包确认没有
// 任何上行包携带该状态），所以由 itemID 落一份持久免疫窗口，并立刻清掉当前虚弱。
func TestHandlerUseStackableAppliesAntiWeaknessPotionWindow(t *testing.T) {
	ctx := context.Background()
	const itemID = int64(10087825) // 防虚弱药剂（30 分钟）
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "78", AccountID: "acc", Level: 60,
		Stats: map[string]int64{
			dnfweaknesspotion.WeaknessLevelStatKey:        9,
			dnfweaknesspotion.WeaknessRecoveryAtStatKey:   1_700_000_000,
		},
	}); err != nil {
		t.Fatal(err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "78",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 5): {
				ItemID: itemID,
				Count:  2,
				Extra: map[string]string{
					"item_kind":      "stackable",
					"pvf_path":       "stackable/10087001/10087825.stk",
					"stackable_type": "[waste]",
				},
			},
		},
	})
	resolver := func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID:        resolvedID,
			PVFPath:       "stackable/10087001/10087825.stk",
			StackableType: "[waste]",
		}, nil
	}

	before := time.Now().UTC()
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackable),
		Body: []byte{
			0x05, 0x00,
			listTypeMain,
			0x11, 0x22, 0x33, 0x44,
			0x91, 0xED, 0x99, 0x00, // 10087825 little endian
			0x00, 0x00, 0x00, 0x00,
		},
		AccountID:            "acc",
		SelectedCharacterID:  78,
		Repositories:         repos,
		StackableUseResolver: resolver,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled {
		t.Fatalf("使用未被处理: %+v", got)
	}
	loaded := loadTestInventory(t, ctx, repos, "78")
	if stack := loaded.Slots[slotKey(listTypeMain, 5)]; stack.Count != 1 {
		t.Fatalf("道具未被消耗: %+v", stack)
	}
	character, found, err := repos.Character.Load(ctx, "78")
	if err != nil || !found {
		t.Fatalf("character reload err=%v found=%t", err, found)
	}
	if level := character.Stats[dnfweaknesspotion.WeaknessLevelStatKey]; level != 0 {
		t.Fatalf("当前虚弱未被清除: stamina=%d", level)
	}
	if at := character.Stats[dnfweaknesspotion.WeaknessRecoveryAtStatKey]; at != 0 {
		t.Fatalf("虚弱恢复锚点未清零: %d", at)
	}
	until := dnfweaknesspotion.ImmuneUntil(character.Stats)
	want := before.Add(1800 * time.Second).Unix()
	if until < want || until > want+5 {
		t.Fatalf("免疫窗口=%d want≈%d", until, want)
	}
}

// 家族外的消耗品不得碰虚弱字段，也不得产生 op541 计数。
func TestHandlerUseStackableLeavesWeaknessFieldsAloneForOtherItems(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "78", AccountID: "acc", Level: 60,
		Stats: map[string]int64{dnfweaknesspotion.WeaknessLevelStatKey: 5},
	}); err != nil {
		t.Fatal(err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "78",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 5): {
				ItemID: 1001,
				Count:  2,
				Extra: map[string]string{
					"item_kind":      "stackable",
					"pvf_path":       "stackable/1001.stk",
					"stackable_type": "[waste]",
				},
			},
		},
	})
	resolver := func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: resolvedID, PVFPath: "stackable/1001.stk", StackableType: "[waste]",
		}, nil
	}
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackable),
		Body: []byte{
			0x05, 0x00,
			listTypeMain,
			0x11, 0x22, 0x33, 0x44,
			0xE9, 0x03, 0x00, 0x00, // 1001
			0x00, 0x00, 0x00, 0x00,
		},
		AccountID:            "acc",
		SelectedCharacterID:  78,
		Repositories:         repos,
		StackableUseResolver: resolver,
	})
	if err != nil || !got.Handled {
		t.Fatalf("Handle err=%v result=%+v", err, got)
	}
	if len(got.DailyUseStates) != 0 {
		t.Fatalf("无上限道具不应产生每日次数投影: %+v", got.DailyUseStates)
	}
	character, _, err := repos.Character.Load(ctx, "78")
	if err != nil {
		t.Fatal(err)
	}
	if character.Stats[dnfweaknesspotion.WeaknessLevelStatKey] != 5 {
		t.Fatalf("家族外道具改动了虚弱: %+v", character.Stats)
	}
	if dnfweaknesspotion.ImmuneUntil(character.Stats) != 0 {
		t.Fatalf("家族外道具写入了免疫窗口: %+v", character.Stats)
	}
}
