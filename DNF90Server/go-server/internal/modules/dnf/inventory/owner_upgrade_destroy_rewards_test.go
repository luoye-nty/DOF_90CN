package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// A destructive failure grants the upgrade.etc compensation first and then
// every fixed extra row (异界 equipment refunds 异次元碎片); the op50 body lists
// all of them under one u8 count and the incremental refresh covers every
// granted slot.
func TestOwnerUpgradeDestroyGrantsCompensationAndExtraRewards(t *testing.T) {
	for _, mode := range []string{"reinforce", "amplify"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, mode, 10, 7804, 2)
			cmd.OptionalTicketSlot = -1
			cmd.UpgradePenaltyType = 3
			cmd.UpgradeDestroyBonusItemID = 3243
			cmd.UpgradeDestroyBonusCount = 6
			cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{{ItemID: 3311, Count: 20}}
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil || !result.Success || result.UpgradeSucceeded || result.ResultCode != upgradeResultCodeDestroy {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.DestroyBonusItemID != 3243 || result.DestroyBonusCount != 6 || len(result.DestroyExtraRewards) != 1 ||
				result.DestroyExtraRewards[0].ItemID != 3311 || result.DestroyExtraRewards[0].Count != 20 {
				t.Fatalf("destroy rewards=%+v extra=%+v", result, result.DestroyExtraRewards)
			}
			if len(result.DestroyBonusSlots) != 2 || result.DestroyBonusSlots[0] == result.DestroyBonusSlots[1] ||
				result.DestroyBonusSlots[0] != result.DestroyBonusSlot || result.DestroyBonusSlots[1] != result.DestroyExtraRewards[0].Slot {
				t.Fatalf("destroy slots=%v bonus=%d extra=%d", result.DestroyBonusSlots, result.DestroyBonusSlot, result.DestroyExtraRewards[0].Slot)
			}
			bag := loadTestInventory(t, ctx, repos, "77")
			if _, found := bag.Slots[slotKey(listTypeMain, 9)]; found {
				t.Fatal("destroyed target survived")
			}
			if stack := bag.Slots[slotKey(listTypeMain, result.DestroyBonusSlot)]; stack.ItemID != 3243 || stack.Count != 6 {
				t.Fatalf("compensation stack=%+v", stack)
			}
			if stack := bag.Slots[slotKey(listTypeMain, result.DestroyExtraRewards[0].Slot)]; stack.ItemID != 3311 || stack.Count != 20 {
				t.Fatalf("refund stack=%+v", stack)
			}

			ack := buildUpgradeItemSuccessAck(result)
			if len(ack) != 19+1+2*10 || ack[12] != upgradeResultCodeDestroy || ack[19] != 2 {
				t.Fatalf("ack=% X", ack)
			}
			for index, want := range []UpgradeDestroyGrantedReward{
				{Slot: result.DestroyBonusSlot, ItemID: 3243, Count: 6},
				{Slot: result.DestroyExtraRewards[0].Slot, ItemID: 3311, Count: 20},
			} {
				row := ack[20+index*10:]
				if int16(binary.LittleEndian.Uint16(row)) != want.Slot ||
					int32(binary.LittleEndian.Uint32(row[2:])) != want.ItemID ||
					int32(binary.LittleEndian.Uint32(row[6:])) != want.Count {
					t.Fatalf("ack row %d=% X want %+v", index, row[:10], want)
				}
			}
			entries := upgradeItemUpdateEntries(result, false)
			refreshed := map[int16]bool{}
			for _, entry := range entries {
				refreshed[entry.slot] = true
			}
			if !refreshed[result.DestroyBonusSlot] || !refreshed[result.DestroyExtraRewards[0].Slot] {
				t.Fatalf("refresh entries=%+v miss a granted slot", entries)
			}
		})
	}
}

// Without a compensation item the destroy block still carries the fixed
// extra rows, and without either it stays an empty list.
func TestUpgradeDestroyAckListsOnlyGrantedRows(t *testing.T) {
	base := UpgradeResult{Mode: "reinforce", Success: true, TargetSlotIndex: 9, MaterialSlotIndex: 121, OptionalTicketSlotIndex: -1, OldLevel: 12, ResultCode: upgradeResultCodeDestroy}
	empty := buildUpgradeItemSuccessAck(base)
	if len(empty) != 20 || empty[19] != 0 {
		t.Fatalf("empty destroy ack=% X", empty)
	}
	withExtra := base
	withExtra.DestroyExtraRewards = []UpgradeDestroyGrantedReward{{Slot: 40, ItemID: 3311, Count: 20}}
	ack := buildUpgradeItemSuccessAck(withExtra)
	if len(ack) != 30 || ack[19] != 1 || int16(binary.LittleEndian.Uint16(ack[20:])) != 40 || int32(binary.LittleEndian.Uint32(ack[22:])) != 3311 {
		t.Fatalf("extra-only destroy ack=% X", ack)
	}
}

// The disassembly refund this server grants on top of the PVF compensation is
// best-effort: when the bag cannot take a row it is dropped, and the
// destruction plus its mandated compensation still commit. A non-best-effort
// row keeps failing the whole attempt.
func TestOwnerUpgradeDestroyBestEffortRefundSurvivesFullInventory(t *testing.T) {
	for _, bestEffort := range []bool{true, false} {
		name := "mandatory"
		if bestEffort {
			name = "best_effort"
		}
		t.Run(name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
			cmd.OptionalTicketSlot = -1
			cmd.UpgradePenaltyType = 3
			cmd.UpgradeDestroyBonusItemID = 3243
			cmd.UpgradeDestroyBonusCount = 6
			cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{
				{ItemID: 3311, Count: 20, BestEffort: bestEffort},
			}
			// firstEmptyUpgradeSlot scans main slots 60..69 then 9..59. Fill
			// everything but the target's own slot 9, which the destruction
			// frees and the mandated compensation then claims, leaving the
			// extra row with nowhere to go.
			bag := loadTestInventory(t, ctx, repos, "77")
			for slot := int16(10); slot <= 69; slot++ {
				bag.Slots[slotKey(listTypeMain, slot)] = dnfrepo.ItemStack{
					ItemID: 6000, Count: 1,
					Extra: map[string]string{"item_kind": "stackable"},
				}
			}
			saveTestInventory(t, ctx, repos, bag)
			result, err := owner.Upgrade(ctx, cmd)
			if !bestEffort {
				if err == nil {
					t.Fatalf("a mandatory row that cannot be placed must fail the upgrade, got %+v", result)
				}
				return
			}
			if err != nil || !result.Success || result.ResultCode != upgradeResultCodeDestroy {
				t.Fatalf("best-effort refund must not void the attempt: result=%+v err=%v", result, err)
			}
			if result.DestroyBonusItemID != 3243 || result.DestroyBonusCount != 6 {
				t.Fatalf("mandated compensation lost: %+v", result)
			}
			if len(result.DestroyExtraRewards) != 0 {
				t.Fatalf("unplaceable best-effort row was reported as granted: %+v", result.DestroyExtraRewards)
			}
			// The row must be reported for the mailer rather than vanish.
			if len(result.DestroyRefundOverflow) != 1 ||
				result.DestroyRefundOverflow[0].ItemID != 3311 ||
				result.DestroyRefundOverflow[0].Count != 20 {
				t.Fatalf("overflow not reported for mail: %+v", result.DestroyRefundOverflow)
			}
		})
	}
}

// The overflow mailer runs at the transaction's commit boundary, so it must
// see the rows the bag refused and must not be able to undo the destruction.
func TestOwnerUpgradeDestroyOverflowGoesToTheRefundMailer(t *testing.T) {
	for _, mailFails := range []bool{false, true} {
		name := "mailed"
		if mailFails {
			name = "mail_failed"
		}
		t.Run(name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
			cmd.OptionalTicketSlot = -1
			cmd.UpgradePenaltyType = 3
			cmd.UpgradeDestroyBonusItemID = 3243
			cmd.UpgradeDestroyBonusCount = 6
			cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{
				{ItemID: 3311, Count: 20, BestEffort: true},
			}
			bag := loadTestInventory(t, ctx, repos, "77")
			for slot := int16(10); slot <= 69; slot++ {
				bag.Slots[slotKey(listTypeMain, slot)] = dnfrepo.ItemStack{
					ItemID: 6000, Count: 1,
					Extra: map[string]string{"item_kind": "stackable"},
				}
			}
			saveTestInventory(t, ctx, repos, bag)
			mailed := []alignedcmd.UpgradeDestroyReward(nil)
			mailedCharacter := ""
			owner.SetUpgradeRefundMailer(func(_ context.Context, characterID string, rows []alignedcmd.UpgradeDestroyReward) error {
				mailedCharacter = characterID
				mailed = append(mailed, rows...)
				if mailFails {
					return errors.New("mailbox unavailable")
				}
				return nil
			})
			result, err := owner.Upgrade(ctx, cmd)
			// A mail failure must never surface as an upgrade failure: the
			// destruction and its PVF compensation already committed.
			if err != nil || !result.Success || result.ResultCode != upgradeResultCodeDestroy {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.DestroyBonusItemID != 3243 || result.DestroyBonusCount != 6 {
				t.Fatalf("mandated compensation lost: %+v", result)
			}
			if mailedCharacter != "77" || len(mailed) != 1 || mailed[0].ItemID != 3311 || mailed[0].Count != 20 {
				t.Fatalf("mailer saw character=%q rows=%+v", mailedCharacter, mailed)
			}
			if result.DestroyRefundMailed == mailFails {
				t.Fatalf("DestroyRefundMailed=%t with mailFails=%t", result.DestroyRefundMailed, mailFails)
			}
			// The destroyed target is gone either way.
			after := loadTestInventory(t, ctx, repos, "77")
			if _, found := after.Slots[slotKey(listTypeMain, 9)]; !found {
				t.Fatal("slot 9 should hold the compensation the destruction freed room for")
			}
			if stack := after.Slots[slotKey(listTypeMain, 9)]; stack.ItemID != 3243 {
				t.Fatalf("slot 9 stack=%+v want the 3243 compensation", stack)
			}
		})
	}
}
