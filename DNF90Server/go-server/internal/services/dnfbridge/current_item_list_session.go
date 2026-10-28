package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) buildCurrentItemListBodyForSession(ctx context.Context, session *gameSession, listType byte) ([]byte, string, int, bool) {
	// 过期道具必须在生成任何容器快照之前从仓储删除，否则客户端仍会收到
	// 带“已超过使用期限”的旧行。这里才是登录/切角色真正走的列表入口；
	// 旧 sendSelectedCurrentItemLists 包装函数在重构后已没有调用点。
	s.sweepExpiredInventoryItemsThrottled(session, "current_item_list")
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil, "", 0, false
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	if listType == 3 {
		return s.buildCurrentEquippedItemListBodyForSession(ctx, session)
	}
	// List 12 is an account-owned container.  It used to fall through to the
	// selected character inventory/settings records, which made its header
	// change when a different character logged in and also discarded every
	// account-cargo item.  The current EXE's list-12 reader still uses the
	// ordinary 0x77 rows, but both the header and the rows are account scoped.
	if listType == 12 {
		return s.buildCurrentAccountCargoItemListBodyForSession(ctx, session, repos)
	}
	if listType == dnfrepo.MainInventoryListType {
		accountID := strings.TrimSpace(s.accountIDForSession(session))
		owner, err := dnfinventory.NewOwner(repos)
		if err == nil {
			err = owner.MigrateAccountSharedSlots(ctx, accountID, characterID)
		}
		if err != nil {
			s.logPacketEvent("game-upper-current-item-list-account-inventory-migration-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"account_id", accountID,
				"list_type", listType,
				"err", err)
			return nil, "", 0, false
		}
		reviveCoinMigration, reviveCoinErr := owner.MigrateReviveCoinWallet(ctx, characterID)
		if reviveCoinErr != nil {
			// Slot 1 is reserved for the current revive-coin wallet. Never
			// overwrite an unrelated row if legacy data violates that
			// invariant; keep serving the inventory and expose the conflict.
			s.logPacketEvent("game-upper-current-revive-coin-wallet-migration-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"list_type", listType,
				"err", reviveCoinErr)
		} else if reviveCoinMigration.Changed {
			s.logPacketEvent("game-upper-current-revive-coin-wallet-migrated",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"list_type", listType,
				"wallet_slot", 1,
				"removed_rows", reviveCoinMigration.RemovedRows,
				"converted_consumable_rows", reviveCoinMigration.ConvertedConsumableRows,
				"converted_consumable_units", reviveCoinMigration.ConvertedConsumableUnits,
				"wallet_total", reviveCoinMigration.Total,
				"source", "repository_backed_fixed_slot1_and_coin_general_consolidation")
		}
		catalog, catalogErr := s.currentPVFItemCatalog()
		if catalogErr != nil {
			s.logPacketEvent("game-upper-current-gold-wallet-slot-repair-catalog-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"list_type", listType,
				"err", catalogErr)
		} else {
			goldWalletRepair, repairErr := s.repairCurrentGoldWalletReservedSlot(ctx, repos, characterID, catalog)
			if repairErr != nil {
				s.logPacketEvent("game-upper-current-gold-wallet-slot-repair-failed",
					"conn_id", session.connID,
					"char_id", session.selectedCharacterID,
					"list_type", listType,
					"item_id", goldWalletRepair.ItemID,
					"from_slot", goldWalletRepair.FromSlot,
					"to_slot", goldWalletRepair.ToSlot,
					"err", repairErr)
			} else if goldWalletRepair.Changed {
				s.logPacketEvent("game-upper-current-gold-wallet-slot-repaired",
					"conn_id", session.connID,
					"char_id", session.selectedCharacterID,
					"list_type", listType,
					"item_id", goldWalletRepair.ItemID,
					"from_slot", goldWalletRepair.FromSlot,
					"to_slot", goldWalletRepair.ToSlot,
					"source", "runtime_pvf_page_and_atomic_inventory_owner_relocation")
			}
		}
		if activationErr := s.reconcileCurrentPremiumInventoryBeforeList(
			ctx,
			session,
			"list0_snapshot_before_client_projection",
			true,
		); activationErr != nil {
			// Keep the recoverable item row when PVF/account persistence is
			// unavailable. A later list snapshot or character selection retries
			// the same conversion without losing either the item or its days.
			s.logPacketEvent("game-upper-current-premium-inventory-auto-activation-deferred",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"account_id", accountID,
				"list_type", listType,
				"err", activationErr,
				"fallback", "retain_recoverable_contract_item")
		}
		if migrated, migrateErr := s.migrateLegacyPetItemsFromMainInventory(ctx, repos, characterID); migrateErr != nil {
			s.logPacketEvent("game-upper-current-pet-item-migration-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"err", migrateErr)
		} else if migrated > 0 {
			s.logPacketEvent("game-upper-current-pet-item-migrated",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"migrated", migrated,
				"source", "login_list0_snapshot_legacy_main_inventory_to_pet_list7")
		}
		if migrated, migrateErr := s.migrateLegacyGuildMedalItemsFromMainInventory(ctx, repos, characterID); migrateErr != nil {
			s.logPacketEvent("game-upper-current-guild-medal-item-migration-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"err", migrateErr)
		} else if migrated > 0 {
			s.logPacketEvent("game-upper-current-guild-medal-item-migrated",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"migrated", migrated,
				"source", "login_list0_snapshot_legacy_main_inventory_to_guild_medal_list38")
		}
		s.reconcileCurrentPVFItemExpirationsBestEffort(session, repos, characterID, accountID)
	}
	entries, source, known := currentItemListEntriesFromInventory(ctx, repos.Inventory, characterID, listType)
	if (!known || len(entries) == 0) && repos.LegacyInventory != nil {
		legacyEntries, err := currentItemListEntriesFromLegacy(ctx, repos.LegacyInventory, characterID, listType)
		if err != nil {
			s.logPacketEvent("game-upper-current-item-list-legacy-load-failed",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"list_type", listType,
				"err", err)
		} else if len(legacyEntries) > 0 || !known {
			entries = legacyEntries
			source = "legacy_inventory"
			known = true
		}
	}
	if listType == dnfrepo.MainInventoryListType {
		var accountKnown bool
		entries, accountKnown = s.mergeCurrentAccountSharedItemListEntries(ctx, session, repos.AccountInventory, entries)
		if accountKnown && !known {
			source = "account_inventory"
			known = true
		}
		if repos.Character != nil {
			character, found, err := repos.Character.Load(ctx, characterID)
			if err != nil {
				s.logPacketEvent("game-upper-current-gold-wallet-load-failed",
					"conn_id", session.connID,
					"char_id", session.selectedCharacterID,
					"err", err)
			} else if found && character.Stats != nil {
				if gold, hasGold := character.Stats["gold"]; hasGold {
					walletProjected := false
					walletSlotConflict := false
					for index, entry := range entries {
						if int16(binary.LittleEndian.Uint16(entry.data[0x00:0x02])) == 0 {
							if binary.LittleEndian.Uint32(entry.data[0x02:0x06]) == 0 {
								// Go keeps gold in Character.Stats, unlike 86JP's
								// single persisted wallet row. Project the current
								// authoritative value over a stale wallet sentinel.
								entries[index] = currentGoldWalletItemListEntry(gold)
								walletProjected = true
							} else {
								walletSlotConflict = true
							}
							break
						}
					}
					if !walletProjected && !walletSlotConflict {
						entries = append(entries, currentGoldWalletItemListEntry(gold))
						sortCurrentItemListEntries(entries)
						walletProjected = true
					}
					if walletProjected {
						known = true
						if source == "" {
							source = "character_gold_wallet_projection"
						} else {
							source += "+character_gold_wallet_projection"
						}
					} else if walletSlotConflict {
						s.logPacketEvent("game-upper-current-gold-wallet-slot-conflict",
							"conn_id", session.connID,
							"char_id", session.selectedCharacterID,
							"slot", 0,
							"reason", "non_wallet_inventory_row_owns_reserved_wallet_slot")
					}
				}
				// 决斗场徽章钱包：与金币钱包共用 list0 的保留槽位约定，但占用 slot 2。
				// 槽位为空时补一行；已被真正的徽章行占用时用当前余额覆盖计数；
				// 被别的物品占用时不抢（保留玩家真实物品）。
				if emblemTotal, saved := character.Stats["finish_loading_currency_slot2_total"]; saved && emblemTotal >= 0 && emblemTotal <= math.MaxInt32 {
					entry := currentPVPEmblemWalletEntry(uint32(emblemTotal))
					projected := false
					conflict := false
					for index := range entries {
						if binary.LittleEndian.Uint16(entries[index].data[0:2]) != 2 {
							continue
						}
						if binary.LittleEndian.Uint32(entries[index].data[2:6]) == 2 {
							entries[index] = entry
							projected = true
						} else {
							conflict = true
						}
						break
					}
					if !projected && !conflict {
						entries = append(entries, entry)
						sortCurrentItemListEntries(entries)
						projected = true
					}
					if projected {
						known = true
						if source == "" {
							source = "character_pvp_emblem_wallet_projection"
						} else {
							source += "+character_pvp_emblem_wallet_projection"
						}
					}
				}
			}
		}
	}
	if !known {
		return nil, "", 0, false
	}
	patchedUsePeriods, usePeriodErr := s.applyCurrentPVFUsePeriodsToEntriesWithLoadedCatalog(ctx, entries)
	if usePeriodErr != nil {
		s.logPacketEvent("game-upper-current-item-use-period-wire-projection-failed",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"list_type", listType,
			"err", usePeriodErr)
	}
	if patchedUsePeriods > 0 {
		source += "+runtime_pvf_stackable_use_period"
	}
	containerState := s.loadCurrentItemListContainerState(ctx, repos.Settings, characterID, listType)
	return buildCurrentItemListBody(listType, entries, containerState), source, len(entries), true
}

// migrateLegacyPetItemsFromMainInventory repairs characters that received a
// pet artifact or pet consumable before the Cera-shop placement fix. The item
// definition is resolved from the current PVF so an arbitrary main-bag row is
// never moved. The migration is one character-scoped atomic inventory write.
func (s *Service) migrateLegacyPetItemsFromMainInventory(
	ctx context.Context,
	repos dnfrepo.Group,
	characterID string,
) (int, error) {
	if repos.Inventory == nil || repos.CharacterAssets == nil {
		return 0, nil
	}
	catalog, err := s.currentCeraShopCatalog()
	if err != nil || catalog == nil || catalog.items == nil {
		return 0, err
	}
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found || inventory.Slots == nil {
		return 0, err
	}
	candidates := make([]dnfinventory.LegacyPetInventoryCandidate, 0, 2)
	for key, stack := range inventory.Slots {
		list, _, ok := parseSceneInventorySlotKey(key)
		if !ok || list != dnfrepo.MainInventoryListType || stack.ItemID <= 0 || stack.Count <= 0 {
			continue
		}
		definition, resolveErr := catalog.items.ResolveItem(uint32(stack.ItemID))
		if resolveErr != nil {
			continue
		}
		candidate := dnfinventory.LegacyPetInventoryCandidate{SourceKey: key, ItemID: stack.ItemID}
		switch {
		case isCurrentCeraShopPetArtifactItem(definition):
			candidate.TargetFrom, candidate.TargetTo = currentBoosterPetArtifactStart, currentBoosterPetArtifactEnd
		case isCurrentCeraShopPetConsumable(definition):
			candidate.TargetFrom, candidate.TargetTo = currentCeraShopPetConsumableSlotStart, currentCeraShopPetConsumableSlotEnd
		default:
			continue
		}
		candidates = append(candidates, candidate)
	}
	owner, err := dnfinventory.NewLegacyPetMigrationOwner(repos.CharacterAssets)
	if err != nil {
		return 0, err
	}
	result, err := owner.Migrate(ctx, characterID, candidates)
	if err != nil {
		return 0, err
	}
	return result.Migrated, nil
}

// migrateLegacyGuildMedalItemsFromMainInventory repairs characters whose
// guild medals ([flag]) or guardian gems ([flag gem]) were granted into list 0
// by the NPC-shop path before the list-38 placement fix. Rows are classified
// from the current PVF, so an arbitrary main-bag item is never moved; account
// warehouse cells are skipped. The migration is one character-scoped atomic
// inventory write.
func (s *Service) migrateLegacyGuildMedalItemsFromMainInventory(
	ctx context.Context,
	repos dnfrepo.Group,
	characterID string,
) (int, error) {
	if repos.Inventory == nil || repos.CharacterAssets == nil {
		return 0, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return 0, err
	}
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found || inventory.Slots == nil {
		return 0, err
	}
	candidates := make([]dnfinventory.LegacyGuildMedalInventoryCandidate, 0, 2)
	for key, stack := range inventory.Slots {
		list, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || list != dnfrepo.MainInventoryListType || stack.ItemID <= 0 || stack.Count <= 0 ||
			dnfrepo.IsAccountSharedInventorySlot(list, slot) {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(uint32(stack.ItemID))
		if resolveErr != nil {
			continue
		}
		start, end, isGuildMedalList := currentGuildMedalListPlacement(definition)
		if !isGuildMedalList {
			continue
		}
		candidates = append(candidates, dnfinventory.LegacyGuildMedalInventoryCandidate{
			SourceKey:  key,
			ItemID:     stack.ItemID,
			TargetFrom: start,
			TargetTo:   end,
		})
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	owner, err := dnfinventory.NewLegacyGuildMedalMigrationOwner(repos.CharacterAssets)
	if err != nil {
		return 0, err
	}
	result, err := owner.Migrate(ctx, characterID, candidates)
	if err != nil {
		return 0, err
	}
	return result.Migrated, nil
}

func (s *Service) reconcileCurrentPVFItemExpirationsBestEffort(
	session *gameSession,
	repos dnfrepo.Group,
	characterID string,
	accountID string,
) {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logPacketEvent("game-upper-current-item-use-period-reconcile-failed",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"account_id", accountID,
			"inventory_repaired", 0,
			"warehouse_repaired", 0,
			"account_repaired", 0,
			"equipment_repaired", 0,
			"err", err)
		return
	}
	reconcileCtx, cancel := context.WithTimeout(context.Background(), currentItemListPVFReconcileTimeout)
	defer cancel()
	summary, reconcileErr := s.reconcileCurrentPVFItemExpirationsWithCatalog(reconcileCtx, repos, characterID, accountID, catalog)
	if reconcileErr != nil {
		s.logPacketEvent("game-upper-current-item-use-period-reconcile-failed",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"account_id", accountID,
			"inventory_repaired", summary.Inventory,
			"warehouse_repaired", summary.Warehouse,
			"account_repaired", summary.Account,
			"equipment_repaired", summary.Equipment,
			"err", reconcileErr)
		return
	}
	if summary.Total() > 0 {
		s.logPacketEvent("game-upper-current-item-use-period-reconciled",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"account_id", accountID,
			"inventory_repaired", summary.Inventory,
			"warehouse_repaired", summary.Warehouse,
			"account_repaired", summary.Account,
			"equipment_repaired", summary.Equipment,
			"source", "runtime_pvf_expiration_date_to_expire_time_and_stackable_u16_seconds")
	}
}
