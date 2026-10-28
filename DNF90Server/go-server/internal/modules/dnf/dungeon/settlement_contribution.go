package dungeon

import (
	"context"
	"time"

	"longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GuildContributionClear carries the 公会贡献度 side of one dungeon clear: the
// roster snapshot taken at entry, the party that actually cleared, the PVF
// catalog and local policy, plus the coin grant callback. The settlement runs it
// inside the same transaction as the rest of the clear receipt.
//
// Ported from the upstream guild branch, minus its GuildDungeon coupling: see
// applySettlementContribution for why that dependency is deliberately dropped.
type GuildContributionClear struct {
	Entry   guild.ContributionEntry
	Party   []string
	Catalog guild.ContributionCatalog
	Policy  guild.ContributionPolicy
	Grant   func(*dnfrepo.InventoryRecord, uint32) error
}

// applySettlementContribution records the 公会贡献度 events of one clear and
// delivers any coins that became due.
//
// guildDungeonAccepted is the caller's verdict on whether this clear may award
// the 公会地下城 ("guild dungeon clear") contribution event. It must be true for
// every ordinary dungeon; for a 公会地下城 clear it is our own admission verdict.
//
// Upstream expressed this as `cmd.GuildDungeon != nil && !guildDungeonAccepted`,
// because its SettlementCommand carries a *GuildDungeonClear holding the monthly
// DungeonCycle state. This tree deliberately does not model that: our 公会地下城 is
// a weekly gauge design and we identify guild-dungeon clears by looking the
// dungeon id back up in the catalog (see dnfbridge/wedding_ceremony.go
// currentWeddingGuildDungeon, which documents the same reasoning). Carrying a
// second, redundant notion of "is this a guild dungeon" in the command would let
// the two disagree, so the caller states the verdict instead and the command
// stays free of guild-dungeon state.
func applySettlementContribution(ctx context.Context, tx dnfrepo.Group, cmd SettlementCommand, inventory *dnfrepo.InventoryRecord, guildDungeonAccepted bool, now time.Time) error {
	clear := cmd.GuildContribution
	if clear == nil {
		return nil
	}
	if !guildDungeonAccepted {
		return nil
	}
	if clear.Entry.CharacterID != cmd.CharacterID || clear.Catalog == nil || clear.Grant == nil {
		return ErrSettlementStateInvalid
	}
	r, err := guild.Load(ctx, tx.Settings)
	if err != nil {
		return err
	}
	eligible, count := r.ContributionEligible(clear.Entry, clear.Party)
	if !eligible {
		return nil
	}
	state, err := guild.LoadContribution(ctx, tx.Settings, cmd.CharacterID)
	if err != nil {
		return err
	}
	var points uint32
	for _, event := range []struct {
		kind    string
		enabled bool
	}{
		{guild.ContributionDungeon, guildDungeonAccepted},
		{guild.ContributionParty, count >= clear.Catalog[guild.ContributionParty].Required && count >= 2},
	} {
		if !event.enabled {
			continue
		}
		value, e := state.Observe(clear.Catalog, clear.Policy, event.kind, cmd.CompletionKey, count, now, now)
		if e != nil {
			return e
		}
		points += value
	}
	if points > 0 {
		if err = r.AddContributionPoints(cmd.CharacterID, points, now); err != nil {
			return err
		}
	}
	delivered, err := state.Deliver(inventory, clear.Grant)
	if err != nil {
		return err
	}
	if delivered {
		inventory.UpdatedAt = now
		if err = dnfrepo.SaveInventoryFields(ctx, tx.Inventory, *inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
	}
	if err = guild.SaveContribution(ctx, tx.Settings, cmd.CharacterID, state, now); err != nil {
		return err
	}
	return guild.Save(ctx, tx.Settings, r, now)
}
