package inventory

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Protection tickets are honoured regardless of their PVF [expiration date].
// Nothing on this server acts on item expiry -- the reconcile pass only
// normalizes a row's projection, it never removes or blocks an expired stack --
// and the client promises protection from the bag count alone (dstr 72968
// 「因为您拥有装备强化保护券…」). Enforcing expiry here alone meant a player
// watched a piece blow up while holding ten tickets, because most of this PVF's
// protection tickets are dead event items: 7804 expired 2010-06-30, and
// 10007840 / 10004511 / 10007841 all expired in 2015.
func TestOwnerUpgradeHonoursProtectionTicketPastItsExpirationDate(t *testing.T) {
	for _, expiry := range []struct {
		name string
		at   time.Time
	}{
		{name: "dead 2010 event ticket", at: time.Date(2010, 6, 29, 16, 0, 0, 0, time.UTC)},
		{name: "no expiry at all", at: time.Time{}},
		{name: "still live", at: time.Now().Add(24 * time.Hour)},
	} {
		t.Run(expiry.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
			cmd.OptionalTicketSlot = -1
			cmd.UpgradePenaltyType = 3
			owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))

			bag := loadTestInventory(t, ctx, repos, "77")
			// The fixture seeds a live ticket at slot 130; drop it so the only
			// ticket on the material page is the one under test.
			delete(bag.Slots, slotKey(listTypeMain, 130))
			bag.Slots[slotKey(listTypeMain, 123)] = dnfrepo.ItemStack{
				ItemID:   7804,
				Count:    10,
				ExpireAt: expiry.at,
				Extra:    map[string]string{"item_kind": "stackable"},
			}
			saveTestInventory(t, ctx, repos, bag)

			result, err := owner.Upgrade(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			if !result.ProtectionTicketUsed {
				t.Fatalf("ticket was not used: %+v", result)
			}
			if result.ResultCode != upgradeResultCodeDowngrade {
				t.Fatalf("result code=%d want downgrade (protected), not destroy", result.ResultCode)
			}
			if result.ProtectionTicketItemID != 7804 || result.ProtectionTicketSlotIndex != 123 {
				t.Fatalf("wrong ticket consumed: %+v", result)
			}
			after := loadTestInventory(t, ctx, repos, "77")
			if stack := after.Slots[slotKey(listTypeMain, 123)]; stack.Count != 9 {
				t.Fatalf("ticket count=%d want 9 consumed by one", stack.Count)
			}
			// The target survives at +0 instead of being destroyed.
			if _, found := after.Slots[slotKey(listTypeMain, 9)]; !found {
				t.Fatalf("protected target was destroyed anyway: %+v", after.Slots)
			}
			if result.NewLevel != 0 {
				t.Fatalf("new level=%d want 0", result.NewLevel)
			}
		})
	}
}

// An explicitly named ticket slot follows the same rule.
func TestOwnerUpgradeHonoursExplicitExpiredProtectionSlot(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
	cmd.UpgradePenaltyType = 3
	owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))

	bag := loadTestInventory(t, ctx, repos, "77")
	ticket := bag.Slots[slotKey(listTypeMain, 130)]
	ticket.ExpireAt = time.Date(2010, 6, 29, 16, 0, 0, 0, time.UTC)
	bag.Slots[slotKey(listTypeMain, 130)] = ticket
	saveTestInventory(t, ctx, repos, bag)

	result, err := owner.Upgrade(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ProtectionTicketUsed || result.ResultCode != upgradeResultCodeDowngrade {
		t.Fatalf("explicit expired ticket was refused: %+v", result)
	}
}
