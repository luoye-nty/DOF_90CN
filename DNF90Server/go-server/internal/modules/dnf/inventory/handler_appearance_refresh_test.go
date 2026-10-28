package inventory

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerAppliedEquipmentMoveUsesCommittedEndpointsWithOneAppearanceRefresh(t *testing.T) {
	for _, test := range []struct {
		slot int16
	}{
		{slot: 0},
		{slot: 8},
		{slot: 9},
		{slot: 10},
		{slot: 11},
		{slot: 12},
		{slot: 24},
		{slot: 25},
		{slot: 26},
		{slot: 27},
		{slot: 29},
		{slot: 30},
		{slot: 32},
	} {
		result := ownerAppliedEquipmentMoveResult(19, Command{
			Operation:            "move_itemspace",
			SourceListType:       listTypeAvatar,
			SourceSlotIndex:      10,
			DestinationListType:  listTypeActorWornAlt,
			DestinationSlotIndex: test.slot,
			MoveCount:            1,
		}, dnfequip.MoveResult{
			Mode:                 "equip",
			SourceListType:       listTypeAvatar,
			SourceSlotIndex:      11,
			DestinationListType:  listTypeEquipment,
			DestinationSlotIndex: test.slot,
			Changed:              true,
		})
		wantActions := []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		}
		if test.slot == 24 {
			wantActions = append(wantActions, alignedcmd.PostActionRefreshSelectedActorExpandedEquipment)
		}
		if len(result.PostActions) != len(wantActions) {
			t.Fatalf("slot %d post actions=%v, want %v", test.slot, result.PostActions, wantActions)
		}
		for index := range wantActions {
			if result.PostActions[index] != wantActions[index] {
				t.Fatalf("slot %d post actions=%v, want %v", test.slot, result.PostActions, wantActions)
			}
		}
		if len(result.ItemSlotRefreshes) != 0 {
			t.Fatalf("slot %d wear move emitted duplicate item slot refreshes=%v", test.slot, result.ItemSlotRefreshes)
		}
		if len(result.UpperResponses) != 1 || len(result.UpperResponses[0].Body) != 11 {
			t.Fatalf("slot %d ACK=%+v", test.slot, result.UpperResponses)
		}
		ack := result.UpperResponses[0].Body
		if ack[1] != listTypeAvatar ||
			binary.LittleEndian.Uint16(ack[2:4]) != 11 ||
			ack[8] != listTypeActorWornAlt ||
			binary.LittleEndian.Uint16(ack[9:11]) != uint16(test.slot) {
			t.Fatalf("slot %d ACK endpoints=%x", test.slot, ack)
		}
	}
}

func TestEquipmentAndAvatarWearMovesNeverEmitItemSlotRefreshes(t *testing.T) {
	for _, test := range []struct {
		name    string
		command Command
		result  dnfequip.MoveResult
	}{
		{
			name: "equipment_equip",
			command: Command{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      44,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 12,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      44,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 12,
				Changed:              true,
			},
		},
		{
			name: "equipment_unequip",
			command: Command{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      45,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 12,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      45,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 12,
				Changed:              true,
			},
		},
		{
			name: "avatar_equip",
			command: Command{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      3,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 8,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      3,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 8,
				Changed:              true,
			},
		},
		{
			name: "avatar_unequip",
			command: Command{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      4,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 7,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      4,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 7,
				Changed:              true,
			},
		},
		{
			name: "alternate_worn_endpoint_list17",
			command: Command{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      5,
				DestinationListType:  listTypeActorWornAlt,
				DestinationSlotIndex: 6,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeAvatar,
				SourceSlotIndex:      5,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 6,
				Changed:              true,
			},
		},
		{
			name: "unchanged_move_has_no_refresh",
			command: Command{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      9,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 11,
			},
			result: dnfequip.MoveResult{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      9,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 11,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.command.Operation = "move_itemspace"
			test.command.MoveCount = 1
			result := ownerAppliedEquipmentMoveResult(19, test.command, test.result)
			if len(result.ItemSlotRefreshes) != 0 {
				t.Fatalf("wear move emitted duplicate item slot refreshes=%v", result.ItemSlotRefreshes)
			}
			var wantActions []alignedcmd.PostAction
			if test.result.Changed {
				wantActions = []alignedcmd.PostAction{
					alignedcmd.PostActionRefreshSelectedActorAppearance,
				}
			}
			if len(result.PostActions) != len(wantActions) {
				t.Fatalf("wear move post actions=%v, want %v", result.PostActions, wantActions)
			}
			for index := range wantActions {
				if result.PostActions[index] != wantActions[index] {
					t.Fatalf("wear move post actions=%v, want %v", result.PostActions, wantActions)
				}
			}
			if len(result.UpperResponses) != 1 || result.UpperResponses[0].MsgID != 19 {
				t.Fatalf("wear move responses=%+v, want one op19 ACK", result.UpperResponses)
			}
		})
	}
}

func TestChangedPetEquipSwapAndUnequipRefreshPetInventoryCreatureStateThenAppearance(t *testing.T) {
	// A creature move only changes list 7 and actor slot 26. Rebuilding the four
	// unrelated containers (0/1/2/38) kept the client busy rebuilding the bag for
	// ~1.7s over a real network before it reached the pet row and mode0, which is
	// why the icon/pet used to appear late after an equip or an unequip. The
	// expected order is therefore the pet-inventory-only rebuild followed by the
	// unchanged creature-state and appearance projections.
	wantActions := []alignedcmd.PostAction{
		alignedcmd.PostActionRefreshSelectedWornCreatureInventory,
		alignedcmd.PostActionRefreshSelectedCreatureState,
		alignedcmd.PostActionRefreshSelectedActorAppearance,
	}
	for _, mode := range []string{"equip", "equip_swap", "unequip"} {
		t.Run(mode, func(t *testing.T) {
			result := ownerAppliedPetEquipmentMoveResult(19, Command{
				Operation:            "move_itemspace",
				SourceListType:       listTypePet,
				SourceSlotIndex:      48,
				DestinationListType:  listTypeActorWornAlt,
				DestinationSlotIndex: 26,
				MoveCount:            1,
			}, dnfequip.MoveResult{
				Mode:                 mode,
				SourceListType:       listTypePet,
				SourceSlotIndex:      48,
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 26,
				Changed:              true,
			})
			if len(result.ItemSlotRefreshes) != 0 {
				t.Fatalf("pet %s emitted duplicate item slot refreshes=%v", mode, result.ItemSlotRefreshes)
			}
			if len(result.PostActions) != len(wantActions) {
				t.Fatalf("pet %s post actions=%v, want %v", mode, result.PostActions, wantActions)
			}
			for index := range wantActions {
				if result.PostActions[index] != wantActions[index] {
					t.Fatalf("pet %s post actions=%v, want %v", mode, result.PostActions, wantActions)
				}
			}
		})
	}
}

// A worn creature (actor slot 26) is a dedicated client-side object whose only
// in-session destroy path is a list-3 op14 row carrying itemID 0xFFFFFFFF
// (sub_1D73120's list-3 branch releases the object, clears the slot and drops
// the equipment UI row). The owner removes the worn entry only for a plain
// unequip, so exactly that outcome may emit the synthetic empty row; every
// other outcome keeps slot 26 occupied and must stay silent.
func TestPetUnequipEmitsWornCreatureSlotClearRowOnlyForAPlainUnequip(t *testing.T) {
	petMove := func(mode string, destinationSlot int16, changed bool) alignedcmd.Result {
		return ownerAppliedPetEquipmentMoveResult(19, Command{
			Operation:            "move_itemspace",
			SourceListType:       listTypePet,
			SourceSlotIndex:      48,
			DestinationListType:  listTypeActorWornAlt,
			DestinationSlotIndex: 26,
			MoveCount:            1,
		}, dnfequip.MoveResult{
			Mode:                 mode,
			SourceListType:       listTypePet,
			SourceSlotIndex:      48,
			DestinationListType:  listTypeEquipment,
			DestinationSlotIndex: destinationSlot,
			Changed:              changed,
		})
	}

	t.Run("plain unequip clears worn creature slot", func(t *testing.T) {
		result := petMove("unequip", 26, true)
		if len(result.PostResponseItemSlotClears) != 1 {
			t.Fatalf("unequip clears=%v, want exactly one worn-creature clear",
				result.PostResponseItemSlotClears)
		}
		// The wire endpoint may be list 17; the clear row must still be list 3,
		// which is the only list the client's row handler resolves as worn gear.
		want := alignedcmd.ItemSlotRefresh{ListType: listTypeEquipment, SlotIndex: 26}
		if result.PostResponseItemSlotClears[0] != want {
			t.Fatalf("unequip clear=%+v, want %+v", result.PostResponseItemSlotClears[0], want)
		}
	})

	for _, mode := range []string{"equip", "equip_swap", "unequip_swap"} {
		t.Run(mode+" keeps worn creature slot", func(t *testing.T) {
			if got := petMove(mode, 26, true).PostResponseItemSlotClears; len(got) != 0 {
				t.Fatalf("%s clears=%v, want none while slot 26 stays occupied", mode, got)
			}
		})
	}

	t.Run("unchanged move clears nothing", func(t *testing.T) {
		if got := petMove("unequip", 26, false).PostResponseItemSlotClears; len(got) != 0 {
			t.Fatalf("unchanged unequip clears=%v, want none", got)
		}
	})

	for slot := int16(27); slot <= 29; slot++ {
		t.Run("unequipped pet artifact stays untouched", func(t *testing.T) {
			if got := petMove("unequip", slot, true).PostResponseItemSlotClears; len(got) != 0 {
				t.Fatalf("artifact slot %d clears=%v, want none: the client ignores list-3 rows for that slot",
					slot, got)
			}
		})
	}
}

func TestPetMoveDetectsWornCreatureSlotAcrossWireAndCommittedEndpoints(t *testing.T) {
	for _, test := range []struct {
		name   string
		cmd    Command
		result dnfequip.MoveResult
	}{
		{
			name: "wire_destination_list17",
			cmd: Command{
				DestinationListType:  listTypeActorWornAlt,
				DestinationSlotIndex: 26,
			},
		},
		{
			name: "wire_source_list3",
			cmd: Command{
				SourceListType:  listTypeEquipment,
				SourceSlotIndex: 26,
			},
		},
		{
			name: "committed_destination_list3",
			result: dnfequip.MoveResult{
				DestinationListType:  listTypeEquipment,
				DestinationSlotIndex: 26,
			},
		},
		{
			name: "committed_source_list17",
			result: dnfequip.MoveResult{
				SourceListType:  listTypeActorWornAlt,
				SourceSlotIndex: 26,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !petMoveTouchesWornCreatureSlot(test.cmd, test.result) {
				t.Fatalf("cmd=%+v result=%+v did not detect worn creature slot", test.cmd, test.result)
			}
		})
	}

	if petMoveTouchesWornCreatureSlot(
		Command{DestinationListType: listTypeActorWornAlt, DestinationSlotIndex: 27},
		dnfequip.MoveResult{DestinationListType: listTypeEquipment, DestinationSlotIndex: 29},
	) {
		t.Fatal("pet artifact endpoints were classified as the worn creature slot")
	}
}

func TestChangedPetArtifactWearMovesRefreshPetContainer(t *testing.T) {
	for slot := int16(27); slot <= 29; slot++ {
		result := ownerAppliedPetEquipmentMoveResult(19, Command{
			Operation:            "move_itemspace",
			SourceListType:       listTypePet,
			SourceSlotIndex:      140 + slot - 27,
			DestinationListType:  listTypeActorWornAlt,
			DestinationSlotIndex: slot,
			MoveCount:            1,
		}, dnfequip.MoveResult{
			Mode:                 "equip",
			SourceListType:       listTypePet,
			SourceSlotIndex:      140 + slot - 27,
			DestinationListType:  listTypeEquipment,
			DestinationSlotIndex: slot,
			Changed:              true,
		})
		if len(result.ItemSlotRefreshes) != 0 {
			t.Fatalf("artifact slot %d emitted duplicate item slot refreshes=%v", slot, result.ItemSlotRefreshes)
		}
		if len(result.PostActions) != 1 ||
			result.PostActions[0] != alignedcmd.PostActionRefreshSelectedWornCreatureInventory {
			t.Fatalf("artifact slot %d post actions=%v, want pet-inventory-only refresh", slot, result.PostActions)
		}
		if len(result.UpperResponses) != 1 || result.UpperResponses[0].MsgID != 19 {
			t.Fatalf("artifact slot %d responses=%+v, want one op19 ACK", slot, result.UpperResponses)
		}
	}
}

// 宠物收窄只作用于宠物本体（槽 26）与宠物装备（槽 27-29）。普通装备走另一条
// ownerAppliedEquipmentMoveResult，仍然只发 mode0 外观投影（槽 24 额外补 op342），
// 既不发五表刷新、也不会被塞进宠物那条 list 7 刷新。这里把两边的动作集合分别钉死，
// 保证这次改动不会溢到普通装备上。
func TestWornCreatureInventoryRefreshIsScopedToWornCreatureMoves(t *testing.T) {
	if string(alignedcmd.PostActionRefreshSelectedWornCreatureInventory) != "refresh_selected_worn_creature_inventory" {
		t.Fatalf("post action wire value=%q", alignedcmd.PostActionRefreshSelectedWornCreatureInventory)
	}
	contains := func(actions []alignedcmd.PostAction, want alignedcmd.PostAction) bool {
		for _, action := range actions {
			if action == want {
				return true
			}
		}
		return false
	}

	petResult := ownerAppliedPetEquipmentMoveResult(19, Command{
		Operation:            "move_itemspace",
		SourceListType:       listTypePet,
		SourceSlotIndex:      48,
		DestinationListType:  listTypeActorWornAlt,
		DestinationSlotIndex: 26,
		MoveCount:            1,
	}, dnfequip.MoveResult{
		Mode:                 "equip",
		SourceListType:       listTypePet,
		SourceSlotIndex:      48,
		DestinationListType:  listTypeEquipment,
		DestinationSlotIndex: 26,
		Changed:              true,
	})
	if !contains(petResult.PostActions, alignedcmd.PostActionRefreshSelectedWornCreatureInventory) {
		t.Fatalf("pet move post actions=%v", petResult.PostActions)
	}
	if contains(petResult.PostActions, alignedcmd.PostActionRefreshSelectedItemContainers) {
		t.Fatalf("pet move must not replay the unrelated containers: %v", petResult.PostActions)
	}

	ordinaryResult := ownerAppliedEquipmentMoveResult(19, Command{
		Operation:            "move_itemspace",
		SourceListType:       listTypeAvatar,
		SourceSlotIndex:      10,
		DestinationListType:  listTypeActorWornAlt,
		DestinationSlotIndex: 9,
		MoveCount:            1,
	}, dnfequip.MoveResult{
		Mode:                 "equip",
		SourceListType:       listTypeAvatar,
		SourceSlotIndex:      10,
		DestinationListType:  listTypeEquipment,
		DestinationSlotIndex: 9,
		Changed:              true,
	})
	if len(ordinaryResult.PostActions) != 1 ||
		ordinaryResult.PostActions[0] != alignedcmd.PostActionRefreshSelectedActorAppearance {
		t.Fatalf("ordinary equipment post actions=%v, want mode0 appearance only", ordinaryResult.PostActions)
	}
	for _, action := range ordinaryResult.PostActions {
		if action == alignedcmd.PostActionRefreshSelectedWornCreatureInventory {
			t.Fatalf("ordinary equipment must keep its own projection: %v", ordinaryResult.PostActions)
		}
	}
}

func TestHandlerRoutesOrdinaryActorWornAltThroughEquipmentOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("equip_destination", func(t *testing.T) {
		repos := dnfrepomemory.NewMemoryGroup()
		saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
			CharacterID: "77",
			Slots: map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 5): {
					ItemID:   700,
					Count:    1,
					RawEntry: make([]byte, currentItemListEntrySize),
				},
			},
		})
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
			CharacterID: "77",
			Entries:     map[string]dnfrepo.EquipmentEntry{},
		}); err != nil {
			t.Fatal(err)
		}

		result, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
			Body:                currentMoveItemspaceBody(listTypeMain, 5, 700, 1, listTypeActorWornAlt, 11, 0, 0, -1),
			SelectedCharacterID: 77,
			Repositories:        repos,
			EquipmentPlacement: func(_ context.Context, placement alignedcmd.EquipmentPlacementRequest) error {
				if placement.SourceListType != listTypeMain || placement.SourceSlotIndex != 5 || placement.TargetSlotIndex != 11 {
					t.Fatalf("placement=%+v", placement)
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.ResponseAllowed {
			t.Fatalf("result=%+v", result)
		}
		if len(result.ItemSlotRefreshes) != 0 {
			t.Fatalf("list17 equip emitted duplicate item slot refreshes=%v", result.ItemSlotRefreshes)
		}
		wantActions := []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		}
		if len(result.PostActions) != len(wantActions) ||
			result.PostActions[0] != wantActions[0] {
			t.Fatalf("list17 equip post actions=%v, want %v", result.PostActions, wantActions)
		}
		ack := result.UpperResponses[0].Body
		if ack[8] != listTypeActorWornAlt || binary.LittleEndian.Uint16(ack[9:11]) != 11 {
			t.Fatalf("ACK endpoints=%x", ack)
		}
		if _, occupied := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 5)]; occupied {
			t.Fatal("inventory source remained occupied")
		}
		equipment, found, err := repos.Equipment.Load(ctx, "77")
		if err != nil || !found || equipment.Entries["11"].ItemID != 700 {
			t.Fatalf("equipment found=%t err=%v record=%+v", found, err, equipment)
		}
	})

	t.Run("unequip_current_exe_target_first_order", func(t *testing.T) {
		repos := dnfrepomemory.NewMemoryGroup()
		saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
			CharacterID: "77",
			Slots:       map[string]dnfrepo.ItemStack{},
		})
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
			CharacterID: "77",
			Entries: map[string]dnfrepo.EquipmentEntry{
				"11": {
					SlotIndex: 11,
					ItemID:    700,
					RawEntry:  make([]byte, currentItemListEntrySize),
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		result, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
			Body:                currentMoveItemspaceBody(listTypeMain, 5, 0, 1, listTypeActorWornAlt, 11, 700, 0, -1),
			SelectedCharacterID: 77,
			Repositories:        repos,
			EquipmentPlacement: func(context.Context, alignedcmd.EquipmentPlacementRequest) error {
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.ResponseAllowed {
			t.Fatalf("result=%+v", result)
		}
		if len(result.ItemSlotRefreshes) != 0 {
			t.Fatalf("list17 unequip emitted duplicate item slot refreshes=%v", result.ItemSlotRefreshes)
		}
		wantActions := []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		}
		if len(result.PostActions) != len(wantActions) ||
			result.PostActions[0] != wantActions[0] {
			t.Fatalf("list17 unequip post actions=%v, want %v", result.PostActions, wantActions)
		}
		ack := result.UpperResponses[0].Body
		if ack[1] != listTypeMain ||
			binary.LittleEndian.Uint16(ack[2:4]) != 5 ||
			ack[8] != listTypeActorWornAlt ||
			binary.LittleEndian.Uint16(ack[9:11]) != 11 {
			t.Fatalf("ACK endpoints=%x", ack)
		}
		if stack := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 5)]; stack.ItemID != 700 {
			t.Fatalf("unequipped stack=%+v", stack)
		}
	})
}

func TestOwnerAppliedEquipmentMoveAlwaysRefreshesAppearanceWithoutContainerReplay(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   int16
		destList byte
		dest     int16
	}{
		{name: "direct_unequip_fashion", source: 3, destList: listTypeMain, dest: 28},
		{name: "worn_swap_clears_fashion_source", source: 8, destList: listTypeEquipment, dest: 10},
		{name: "aura", source: 9, destList: listTypeMain, dest: 28},
		{name: "weapon", source: 11, destList: listTypeMain, dest: 28},
		{name: "pet_creature_uses_dedicated_flow", source: 26, destList: listTypeMain, dest: 28},
		{name: "pet_artifact_uses_native_flow", source: 28, destList: listTypeMain, dest: 28},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := ownerAppliedEquipmentMoveResult(19, Command{
				Operation:            "move_itemspace",
				SourceListType:       listTypeEquipment,
				SourceSlotIndex:      test.source,
				DestinationListType:  test.destList,
				DestinationSlotIndex: test.dest,
				MoveCount:            1,
			}, dnfequip.MoveResult{
				Mode:                 "unequip",
				SourceListType:       listTypeEquipment,
				SourceSlotIndex:      test.source,
				DestinationListType:  test.destList,
				DestinationSlotIndex: test.dest,
				Changed:              true,
			})
			wantActions := []alignedcmd.PostAction{
				alignedcmd.PostActionRefreshSelectedActorAppearance,
			}
			if len(result.PostActions) != len(wantActions) ||
				result.PostActions[0] != wantActions[0] {
				t.Fatalf("post actions = %v, want %v", result.PostActions, wantActions)
			}
			if len(result.ItemSlotRefreshes) != 0 {
				t.Fatalf("slot %d wear move emitted duplicate item slot refreshes=%v", test.source, result.ItemSlotRefreshes)
			}
			if len(result.UpperResponses) != 1 || result.UpperResponses[0].MsgID != 19 {
				t.Fatalf("slot %d responses=%+v, want one op19 ACK", test.source, result.UpperResponses)
			}
		})
	}
}
