package dnfbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentGuildDungeonRun reports whether this run is a 公会地下城 run.
//
// Upstream tests runtime.guildDungeonID != 0 for this. This tree deliberately does
// not record that flag (decision D5): it identifies a guild dungeon by looking the
// dungeon id back up in the same catalog, which is the predicate
// currentWeddingGuildDungeon already relies on for its "公会本不算夫妻通关" rule and
// currentWeddingGuildDungeon now delegates here. Two independent notions of "is
// this a guild dungeon" would be free to disagree, and the wedding feature is
// already live on this one.
//
// A catalog load failure returns false: better to miss the guild-dungeon rule than
// to swallow an ordinary clear.
func (s *Service) currentGuildDungeonRun(runtime *runtimeDungeonState) bool {
	if s == nil || runtime == nil || runtime.Dungeon.ID <= 0 || runtime.Dungeon.ID > int64(^uint32(0)) {
		return false
	}
	catalog, err := s.currentGuildDungeonCatalog()
	if err != nil {
		return false
	}
	_, found := catalog.Stage(uint32(runtime.Dungeon.ID))
	return found
}

func (s *Service) currentGuildContributionRules(ctx context.Context) (dnfguild.ContributionCatalog, *dnfguild.ContributionPolicy, error) {
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil, nil, dnfguild.ErrRepositoryUnavailable
	}
	p, err := dnfguild.LoadContributionPolicy(ctx, repos.Settings)
	if err != nil || p == nil {
		return nil, p, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, nil, err
	}
	c, err := dnfguild.LoadContributionCatalog(items.source)
	return c, p, err
}

func (s *Service) currentGuildContributionGrant() (func(*dnfrepo.InventoryRecord, uint32) error, error) {
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	def, err := items.ResolveItem(dnfguild.ContributionCoinID)
	if err != nil {
		return nil, err
	}
	if def.Kind != dungeonDropItemStackable || def.InventoryListType != dnfrepo.MainInventoryListType {
		return nil, errDungeonPickupItemInvalid
	}
	return func(inv *dnfrepo.InventoryRecord, count uint32) error {
		// Same two-fork signature split wedding_grant.go documents: upstream calls
		// grantCurrentCeraShopProduct(inv, def, count), this tree requires the
		// character's purchased main-inventory row count as a fourth argument, and
		// getting it wrong silently drops the coins into a row the client does not
		// render. The callback signature carries no ctx, so the write-path timeout
		// is used, exactly as grantCurrentWeddingMainProduct does.
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		_, err := grantCurrentCeraShopProduct(inv, def, count, s.currentInventoryPlacementMainSlotCount(ctx, inv, ""))
		if errors.Is(err, errDungeonPickupInventoryFull) {
			return dnfguild.ErrContributionInventoryFull
		}
		return err
	}, nil
}

// The caller runs on the selected character's mailbox. All state and inventory
// writes below share the existing character settlement transaction and guild lock.
func (s *Service) applyCurrentGuildContribution(session *gameSession, kind string) ([]currentItemListEntry, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return nil, dnfguild.ErrCharacterNotFound
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var c dnfguild.ContributionCatalog
	var p *dnfguild.ContributionPolicy
	var err error
	if kind != "" {
		c, p, err = s.currentGuildContributionRules(ctx)
		if err != nil {
			return nil, err
		}
	}
	var grant func(*dnfrepo.InventoryRecord, uint32) error
	if p != nil || kind == "" {
		// A pending ledger must also be deliverable after the policy is disabled.
		state, e := s.loadCurrentGuildContribution(ctx, session)
		if e != nil {
			return nil, e
		}
		if p != nil || state.PendingCoins > 0 {
			grant, err = s.currentGuildContributionGrant()
			if err != nil {
				return nil, err
			}
		} else if kind == "" {
			if state.PaidCoins > 0 {
				ch, e := s.currentGuildOwnedSelectedCharacter(session)
				if e != nil {
					return nil, e
				}
				repos, _ := s.repositoryGroup()
				if repos.Inventory == nil {
					return nil, dnfguild.ErrRepositoryUnavailable
				}
				inv, found, e := repos.Inventory.Load(ctx, ch.CharacterID)
				if e != nil {
					return nil, e
				}
				if !found || inv.CharacterID != ch.CharacterID {
					return nil, dnfguild.ErrCharacterNotFound
				}
				return currentGuildContributionInventoryEntries(inv), nil
			}
			return nil, nil
		}
	}
	var updates []currentItemListEntry
	var pending, paid uint32
	now := s.gameplayNow()
	var receipt [16]byte
	if _, err = rand.Read(receipt[:]); err != nil {
		return nil, err
	}
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, r *dnfguild.Registry, ch *dnfrepo.CharacterRecord) error {
		_, _, found := r.GuildForCharacter(ch.CharacterID)
		if kind != "" && !found {
			return dnfguild.ErrNotMember
		}
		if kind == dnfguild.ContributionAttendance {
			// Existing members already marked today by the previous server receive
			// no synthetic backfill. Retries only flush an existing pending award.
			attended, e := r.Attend(ch.CharacterID, now.In(time.FixedZone("CST", 8*3600)))
			if e != nil {
				return e
			}
			if !attended {
				kind = ""
			}
		}
		if p == nil && grant == nil {
			return nil
		}
		state, e := dnfguild.LoadContribution(ctx, tx.Settings, ch.CharacterID)
		if e != nil {
			return e
		}
		if p != nil && kind != "" {
			key := hex.EncodeToString(receipt[:])
			points, e := state.Observe(c, *p, kind, key, 0, now, now)
			if e != nil {
				return e
			}
			if points > 0 {
				if e = r.AddContributionPoints(ch.CharacterID, points, now); e != nil {
					return e
				}
			}
		}
		if state.PendingCoins > 0 || state.PaidCoins > 0 {
			if tx.Inventory == nil {
				return dnfguild.ErrRepositoryUnavailable
			}
			inv, found, e := tx.Inventory.Load(ctx, ch.CharacterID)
			if e != nil {
				return e
			}
			if !found || inv.CharacterID != ch.CharacterID {
				return dnfguild.ErrCharacterNotFound
			}
			before := state.PendingCoins
			delivered, e := state.Deliver(&inv, grant)
			if e != nil {
				return e
			}
			if delivered {
				inv.UpdatedAt = now
				if e = dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inv, dnfrepo.InventoryFieldSlots); e != nil {
					return e
				}
				updates = currentGuildContributionInventoryEntries(inv)
				paid = before
			}
			// A previous write may have committed before its op14 socket failed.
			updates = currentGuildContributionInventoryEntries(inv)
		}
		pending = state.PendingCoins
		return dnfguild.SaveContribution(ctx, tx.Settings, ch.CharacterID, state, now)
	})
	if err == nil && (paid > 0 || pending > 0) {
		s.logGameEvent(session, "game-guild-contribution-settled", "activity", kind, "paid", paid, "pending", pending)
	}
	return updates, err
}

func (s *Service) loadCurrentGuildContribution(ctx context.Context, session *gameSession) (dnfguild.ContributionState, error) {
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfguild.ContributionState{}, dnfguild.ErrRepositoryUnavailable
	}
	return dnfguild.LoadContribution(ctx, repos.Settings, strconv.Itoa(int(session.selectedCharacterID)))
}

func currentGuildContributionInventoryEntries(inv dnfrepo.InventoryRecord) []currentItemListEntry {
	var out []currentItemListEntry
	for key, stack := range inv.Slots {
		if stack.ItemID != int64(dnfguild.ContributionCoinID) {
			continue
		}
		list, slot, ok := parseSceneInventorySlotKey(key)
		if ok && list == dnfrepo.MainInventoryListType {
			out = append(out, currentItemListEntryFromStack(list, slot, stack))
		}
	}
	sortCurrentItemListEntries(out)
	return out
}

func (s *Service) sendCurrentGuildContributionUpdates(session *gameSession, updates []currentItemListEntry) error {
	if len(updates) == 0 {
		return nil
	}
	return s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, updates), 0)
}

func (s *Service) flushCurrentGuildContribution(session *gameSession) error {
	updates, err := s.applyCurrentGuildContribution(session, "")
	if err != nil {
		s.logGameEvent(session, "game-guild-contribution-delivery-deferred", "error", err)
		return nil
	}
	return s.sendCurrentGuildContributionUpdates(session, updates)
}

func (s *Service) captureCurrentGuildContributionEntry(ctx context.Context, id string) (dnfguild.ContributionEntry, error) {
	var entry dnfguild.ContributionEntry
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil {
		return entry, nil
	}
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, r dnfguild.Registry) error { entry = r.ContributionEntry(id); return nil })
	return entry, err
}

// currentGuildContributionClear assembles the 公会贡献度 side of one settlement.
//
// Two upstream gates are adapted here rather than copied:
//   - runtime.guildDungeonID is replaced by currentGuildDungeonRun (decision D5);
//   - runtime.guildWarID is kept, but this tree only sets it once the 公会战 step
//     lands, so today it is always zero and this gate is inert.
func (s *Service) currentGuildContributionClear(ctx context.Context, runtime *runtimeDungeonState) (*dnfdungeon.GuildContributionClear, error) {
	if runtime == nil || runtime.guildContributionEntry.GuildID == 0 || currentDungeonRuntimeIsPractice(runtime) || runtime.guildWarID != 0 {
		return nil, nil
	}
	if !s.currentGuildDungeonRun(runtime) && runtime.partyDungeonOwner == nil {
		return nil, nil
	}
	c, p, err := s.currentGuildContributionRules(ctx)
	if err != nil || p == nil {
		return nil, err
	}
	grant, err := s.currentGuildContributionGrant()
	if err != nil {
		return nil, err
	}
	var party []string
	if owner := runtime.partyDungeonOwner; owner != nil {
		party = owner.contributionClearRoster()
	}
	return &dnfdungeon.GuildContributionClear{Entry: runtime.guildContributionEntry, Party: party, Catalog: c, Policy: *p, Grant: grant}, nil
}
