package dnfbridge

// 回归：变更券产出的称号必须能正常穿到装备栏 13。
//
// 现场（2026-10-16，角色 1）：用 490702318「热舞一夏称号随机变更券」把背包里的称号
// 变成 400330118 之后，右键穿不上，客户端弹「物品栏已满，或该道具超过可持有上限」。
// 根因在 equip.Owner 那条穿戴链上的"raw entry 证据"判定：
//   - 变更券产出的装备由 applyCurrentPVFItemClassification 投影，只有 PVF 里写了
//     [durability] 才会落下 durability extra；
//   - title 类 PVF 没有 [durability]，于是这一格没有 raw_entry、没有 durability、
//     也没有 raw_entry_hex，三重证据全空 → op19 直接回错误码 17；
//   - 原来只有 source=dungeon_card_reward 有"零耐久豁免"，变更券的
//     source=limited_cube_result 不在其列。
// 本用例用真实 Script.pvf 把整条 op19 穿装备链路跑一遍，锁住修复。
//
// 运行：DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf 路径> go test ./internal/services/dnfbridge -run TestRealPVFLimitedCubeResultTitleCanBeWorn -v

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFLimitedCubeResultTitleCanBeWorn(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate the 变更券 title equip route")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	const (
		accountID      = "dnf:limited-cube-title-equip"
		transformedID  = 400330118 // 变更券产出的称号（热浪舞者）
		wornTitleID    = 400330105 // 穿在身上的旧称号（兽人守护神）
		bagSlot        = int16(26)
		equipSlot      = int16(13)
		transformedQty = "963678678" // 现场那一行的品级种子
	)
	def, err := catalog.ResolveItem(transformedID)
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != dungeonDropItemEquipment || def.EquipmentType != "[title name]" {
		t.Fatalf("transformed title template = %+v", def)
	}

	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: accountID, Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "1",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"13": {
				SlotIndex: equipSlot,
				ItemID:    wornTitleID,
				Extra: map[string]string{
					"item_kind":      "equipment",
					"equipment_type": "[title name]",
					"pvf_path":       "equipment/character/common/title/chn_400330105.equ",
					"quality_seed":   "95149641",
					"source":         "booster_item",
					"equipped_slot":  "13",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 现场那一行的字段照抄：没有 raw_entry、没有 durability、没有 raw_entry_hex。
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:26": {
				ItemID: transformedID,
				Count:  1,
				Extra: map[string]string{
					"item_kind":             "equipment",
					"equipment_type":        "[title name]",
					"pvf_path":              "equipment/character/common/title/chn_400330118.equ",
					"quality_seed":          transformedQty,
					"seal_flag":             "0",
					"source":                "limited_cube_result",
					"enchant_card_id":       "490702344",
					"value_a":               "490702344",
					"enchant_upgrade_count": "0",
					"byte_12":               "0",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := &Service{
		initialEquipmentArchive: archive,
		pvfItemCatalog:          catalog,
		options:                 options{accountID: accountID},
	}
	validator, err := service.newCurrentEquipmentPlacementValidator(ctx, accountID, 1, repos)
	if err != nil {
		t.Fatal(err)
	}
	placement := func(c context.Context, req alignedcmd.EquipmentPlacementRequest) error {
		return validator.ValidateEquipmentPlacement(c, dnfequip.Placement{
			CharacterID:     req.CharacterID,
			ItemID:          req.ItemID,
			SourceListType:  req.SourceListType,
			SourceSlotIndex: req.SourceSlotIndex,
			TargetSlotIndex: req.TargetSlotIndex,
		})
	}

	// 真机 op19：背包 0:26 → 装备栏 13，+3 处是客户端回显的物品号。
	body := make([]byte, 28)
	body[0] = dnfrepo.MainInventoryListType
	binary.LittleEndian.PutUint16(body[1:3], uint16(bagSlot))
	binary.LittleEndian.PutUint32(body[3:7], transformedID)
	binary.LittleEndian.PutUint32(body[7:11], 1)
	body[11] = 3
	binary.LittleEndian.PutUint16(body[12:14], uint16(equipSlot))

	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command:             byte(dnfenum.GameCmdCommand),
		CommandKnown:        true,
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                body,
		AccountID:           accountID,
		SelectedCharacterID: 1,
		Repositories:        repos,
		EquipmentPlacement:  placement,
	})
	if err != nil || !routed {
		t.Fatalf("equip route routed=%t err=%v (reason=%s)", routed, err, result.Reason)
	}
	if len(result.UpperResponses) != 1 || result.UpperResponses[0].MsgID != uint16(dnfenum.CmdPacketMoveItemspace) {
		t.Fatalf("op19 response = %+v reason=%s", result.UpperResponses, result.Reason)
	}
	if ack := result.UpperResponses[0].Body; len(ack) == 0 || ack[0] != 1 {
		t.Fatalf("op19 rejected body=%x reason=%s", ack, result.Reason)
	}

	inventory, _, err := repos.Inventory.Load(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	equipment, _, err := repos.Equipment.Load(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	// 换下来的旧称号回到原背包格，新称号进入装备栏并带上可重建的 raw entry。
	if swapped := inventory.Slots["0:26"]; swapped.ItemID != wornTitleID {
		t.Fatalf("swapped bag row = %+v, want the displaced title %d", swapped, wornTitleID)
	}
	worn := equipment.Entries["13"]
	if worn.ItemID != transformedID || len(worn.RawEntry) == 0 {
		t.Fatalf("worn title entry = %+v, want item=%d with raw entry evidence", worn, transformedID)
	}
	if worn.Extra["enchant_card_id"] != "490702344" || worn.Extra["seal_flag"] != "0" {
		t.Fatalf("worn title dropped the 宝珠/封装 fields: %+v", worn.Extra)
	}
}
