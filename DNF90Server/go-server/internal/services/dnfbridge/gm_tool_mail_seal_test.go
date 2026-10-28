package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GM item mail hand-builds its stack, so it has to apply the same creation
// rule the shared constructor does: `[attach type] [sealing]` equipment is
// delivered sealed (+0x0D = 1). Delivered unsealed, the client treats the
// piece as bound -- untradeable, and the 合成器 rejects it ("解除封装的装备无法
// 进行合成。", dstr 25065).
func TestGMMailSealsSealingAttachTypeEquipment(t *testing.T) {
	for _, rarity := range []int64{0, 2, 4, 5, 6} {
		sealing := dungeonDropItemDefinition{
			ItemID:        101010047,
			Kind:          dungeonDropItemEquipment,
			Rarity:        rarity,
			EquipmentType: "[weapon]",
			AttachType:    "`[sealing]`",
		}
		stack := dnfrepo.ItemStack{ItemID: 101010047, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}
		gmApplyMailEquipmentSeal(&stack, sealing)
		if stack.Extra["seal_flag"] != "1" {
			t.Fatalf("rarity=%d extra=%+v want seal_flag=1", rarity, stack.Extra)
		}
		// The mail attachment and the claimed bag row are both projected from
		// this stack, so the seal has to survive into the wire row.
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
		if entry.data[0x0D] != 1 {
			t.Fatalf("rarity=%d row +0x0D=%d want 1", rarity, entry.data[0x0D])
		}
	}

	// Every other attach type stays unsealed, and rarity never seals anything.
	for _, attach := range []string{"`[trade]`", "`[trade delete]`", "`[free]`", "`[account]`", "`[sealing trade]`", ""} {
		plain := dungeonDropItemDefinition{
			ItemID:        101010047,
			Kind:          dungeonDropItemEquipment,
			Rarity:        6,
			EquipmentType: "[weapon]",
			AttachType:    attach,
		}
		stack := dnfrepo.ItemStack{ItemID: 101010047, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}
		gmApplyMailEquipmentSeal(&stack, plain)
		if _, sealed := stack.Extra["seal_flag"]; sealed {
			t.Fatalf("attach=%q was sealed: %+v", attach, stack.Extra)
		}
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
		if entry.data[0x0D] != 0 {
			t.Fatalf("attach=%q row +0x0D=%d want 0", attach, entry.data[0x0D])
		}
	}

	// Stackables are never sealed, whatever their attach type says.
	stackable := dungeonDropItemDefinition{ItemID: 3037, Kind: dungeonDropItemStackable, AttachType: "`[sealing]`"}
	stack := dnfrepo.ItemStack{ItemID: 3037, Count: 5}
	gmApplyMailEquipmentSeal(&stack, stackable)
	if _, sealed := stack.Extra["seal_flag"]; sealed {
		t.Fatalf("stackable was sealed: %+v", stack.Extra)
	}
}
