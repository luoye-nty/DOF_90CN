package dnfbridge

import (
	"context"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// sweepExpiredInventoryItems 删除角色背包/仓库、账号仓库和穿戴装备里
// 已经过期的道具。穿戴装备同时覆盖时装、名称牌等非背包格子。
//
// 客户端只会把过期道具画成"已超过使用期限"的灰标，服务端此前从不删除，
// 于是它们永久占着格子（stack_limit=1 的道具还会挡住新获得的同名道具）。
// 这里在"发送道具列表快照之前"删掉过期行 —— 客户端收到的列表里直接就没有它，
// 不需要额外的刷新包。
func (s *Service) sweepExpiredInventoryItems(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Inventory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	accountID := s.accountIDForSession(session)

	removed := 0
	if record, found, err := repos.Inventory.Load(ctx, characterID); err != nil {
		s.logInfo("dnfbridge expired-item sweep load failed", "char_id", characterID, "error", err)
	} else if found {
		if n := dropExpiredStacks(record.Slots, now) + dropExpiredStacks(record.Warehouse, now); n > 0 {
			if saveErr := repos.Inventory.Save(ctx, record); saveErr != nil {
				s.logInfo("dnfbridge expired-item sweep save failed", "char_id", characterID, "error", saveErr)
			} else {
				removed += n
			}
		}
	}
	if repos.AccountInventory != nil && accountID != "" {
		if record, found, err := repos.AccountInventory.Load(ctx, accountID); err != nil {
			s.logInfo("dnfbridge expired-item sweep load failed", "account_id", accountID, "error", err)
		} else if found {
			if n := dropExpiredStacks(record.Slots, now); n > 0 {
				if saveErr := repos.AccountInventory.Save(ctx, record); saveErr != nil {
					s.logInfo("dnfbridge expired-item sweep save failed", "account_id", accountID, "error", saveErr)
				} else {
					removed += n
				}
			}
		}
	}
	equipmentRemoved, nameTagCleared, equipmentErr := sweepExpiredEquipmentRecord(
		ctx,
		repos.Character,
		repos.Equipment,
		characterID,
		now,
	)
	if equipmentErr != nil {
		s.logInfo("dnfbridge expired-equipment sweep failed",
			"char_id", characterID, "error", equipmentErr)
	}
	if nameTagCleared {
		s.logInfo("dnfbridge expired name-tag state cleared",
			"char_id", characterID)
	}
	removed += equipmentRemoved
	if removed > 0 {
		s.logInfo("dnfbridge expired-item sweep removed items",
			"conn_id", session.connID, "char_id", characterID,
			"source", source, "removed", removed,
			"equipment_removed", equipmentRemoved)
	}
}

// sweepExpiredInventoryItemsThrottled is the entry used by the real item-list
// builder. The old wrapper that called sweepExpiredInventoryItems was not on
// the live send path, so expired items survived login. Sweep once per
// character per 30 seconds to keep every list snapshot cheap.
func (s *Service) sweepExpiredInventoryItemsThrottled(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	charID := uint32(session.selectedCharacterID)
	session.expiredItemSweepMu.Lock()
	if session.expiredItemSweepCharID == charID &&
		time.Since(session.expiredItemSweepAt) < 30*time.Second {
		session.expiredItemSweepMu.Unlock()
		return
	}
	session.expiredItemSweepCharID = charID
	session.expiredItemSweepAt = time.Now()
	session.expiredItemSweepMu.Unlock()
	s.sweepExpiredInventoryItems(session, source)
}

// dropExpiredStacks 就地删除 map 里已到期的分组，返回删除条数。
func dropExpiredStacks(slots map[string]dnfrepo.ItemStack, now time.Time) int {
	removed := 0
	for key, stack := range slots {
		// Prefer the exact timestamp the client uses (extra/raw 0x77 row).
		// The DATETIME column and the wire timestamp can differ by the MySQL
		// session timezone; using only ExpireAt delayed deletion by up to 8h.
		expireUnix := currentItemListStackExpire(stack)
		if expireUnix != 0 && int64(expireUnix) <= now.Unix() {
			delete(slots, key)
			removed++
		}
	}
	return removed
}

func dropExpiredEquipmentEntries(entries map[string]dnfrepo.EquipmentEntry, now time.Time) int {
	removed := 0
	for key, entry := range entries {
		expireUnix := currentItemListEquipmentExpire(entry)
		if expireUnix != 0 && int64(expireUnix) <= now.Unix() {
			delete(entries, key)
			removed++
		}
	}
	return removed
}

// sweepExpiredEquipmentRecord removes expired worn items, including avatar
// costumes and the slot-30 name tag. Name-tag state lives in character.Stats,
// so removing that worn row must clear the paired fields in the same sweep.
func sweepExpiredEquipmentRecord(
	ctx context.Context,
	characters dnfrepo.CharacterRepository,
	equipment dnfrepo.EquipmentRepository,
	characterID string,
	now time.Time,
) (int, bool, error) {
	if equipment == nil || characterID == "" {
		return 0, false, nil
	}
	removed := 0
	if record, found, err := equipment.Load(ctx, characterID); err != nil {
		return 0, false, err
	} else if found {
		if n := dropExpiredEquipmentEntries(record.Entries, now); n > 0 {
			if saveErr := equipment.Save(ctx, record); saveErr != nil {
				return 0, false, saveErr
			}
			removed = n
		}
	}

	nameTagCleared := false
	if characters != nil {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return removed, false, err
		}
		if found && cleanupExpiredNameTagStateAt(&character, now) {
			if saveErr := dnfrepo.SaveCharacterFields(
				ctx, characters, character, dnfrepo.CharacterFieldStats,
			); saveErr != nil {
				return removed, false, saveErr
			}
			nameTagCleared = true
		}
	}
	return removed, nameTagCleared, nil
}
