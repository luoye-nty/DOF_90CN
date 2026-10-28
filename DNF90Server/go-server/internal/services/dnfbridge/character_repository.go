package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) listCharacters(ctx context.Context, repos dnfrepo.Group, accountID string) ([]dnfrepo.CharacterRecord, error) {
	characters, _, err := s.listCharactersWithLimit(ctx, repos, accountID)
	return characters, err
}

func (s *Service) listCharactersWithLimit(ctx context.Context, repos dnfrepo.Group, accountID string) ([]dnfrepo.CharacterRecord, int, error) {
	if repos.Character == nil {
		return nil, defaultCharacterSlots, errCharacterRepositoryMissing
	}
	limit, err := s.accountCharacterSlotLimit(ctx, repos, accountID)
	if err != nil {
		return nil, defaultCharacterSlots, err
	}
	// 仓储读取按硬上限取，不按账号级限额取。账号记录缺失或元数据损坏时限额会回落到
	// 基线 32，如果连读取都跟着截断，坐在 32..59 号槽位上的角色会直接从选角页消失，
	// 而且 op679 扩展批次也再也拿不到它们。限额只用来决定线上投影，不该决定读多少。
	records, err := repos.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return nil, limit, err
	}
	// Lazy repair for the Cera shop rename card that the 2026-10-02 recognition
	// bug delivered to a bag instead of arming the rename. It runs here - on the
	// roster read every login already performs - and before the rows below are
	// built, so the pending-rename marker is projected on the very same character
	// select rather than one login later. Characters inside the rename cooldown
	// are left untouched. See character_rename_bag_repair.go.
	if s.repairCurrentCharacterRenameShopCardsForAccount(ctx, accountID, records) {
		// Something changed (a bag row removed and/or an entitlement added), so
		// the roster projection must not use the stale copies. The common case -
		// nothing to repair - pays nothing.
		if records, err = repos.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit); err != nil {
			return nil, limit, err
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Slot == records[j].Slot {
			return records[i].CharacterID < records[j].CharacterID
		}
		return records[i].Slot < records[j].Slot
	})
	for idx := range records {
		s.attachEquipmentSummary(ctx, repos, &records[idx])
		records[idx].Roster.PVPEntryQuestCompleted = false
		if repos.Quest != nil {
			quest, found, err := repos.Quest.Load(ctx, records[idx].CharacterID)
			if err != nil {
				return nil, limit, fmt.Errorf("load roster PVP entry quest for %s: %w", records[idx].CharacterID, err)
			}
			if found {
				if strings.TrimSpace(quest.CharacterID) != records[idx].CharacterID {
					return nil, limit, fmt.Errorf("roster PVP entry quest owner mismatch: character=%s quest=%q", records[idx].CharacterID, quest.CharacterID)
				}
				records[idx].Roster.PVPEntryQuestCompleted = townQuestStateCompleted(quest, 2670)
			}
		}
	}
	return records, limit, nil
}

func (s *Service) accountCharacterSlotLimit(ctx context.Context, repos dnfrepo.Group, accountID string) (int, error) {
	if repos.Account == nil {
		return defaultCharacterSlots, nil
	}
	account, found, err := repos.Account.Load(ctx, accountID)
	if err != nil {
		return defaultCharacterSlots, err
	}
	if !found {
		return defaultCharacterSlots, nil
	}
	return dnfrepo.AccountCharacterSlotLimit(account), nil
}

func (s *Service) characterNameExists(ctx context.Context, repos dnfrepo.Group, records []dnfrepo.CharacterRecord, name string) (bool, error) {
	if repos.Character == nil {
		return false, errCharacterRepositoryMissing
	}
	_, found, err := repos.Character.FindIDByName(ctx, name)
	return found, err
}

func (s *Service) nextCharacterID(ctx context.Context, repos dnfrepo.Group, records []dnfrepo.CharacterRecord) (int, error) {
	if repos.Character == nil {
		return 0, errCharacterRepositoryMissing
	}
	return repos.Character.NextNumericID(ctx)
}

// nextCharacterSlot 在限额范围内找第一个空槽位，找不到时返回 -1。
//
// 以前找不到就 return len(records)：槽位有空洞时这个下标正好落在一个已占用的槽位
// 上，写进去会撞 (account_id, slot) 唯一约束，创建从此报服务端错误。宁可回 -1 让
// 调用方按“栏位已满”失败，也不能交出一个可能重复的槽位。
func nextCharacterSlot(records []dnfrepo.CharacterRecord, limit ...int) int {
	capacity := defaultCharacterSlots
	if len(limit) > 0 {
		capacity = limit[0]
	}
	if capacity < defaultCharacterSlots {
		capacity = defaultCharacterSlots
	}
	if capacity > dnfrepo.MaxCharacterSlotLimit {
		capacity = dnfrepo.MaxCharacterSlotLimit
	}
	used := make(map[int]struct{}, len(records))
	for _, record := range records {
		used[record.Slot] = struct{}{}
	}
	for slot := 0; slot < capacity; slot++ {
		if _, ok := used[slot]; !ok {
			return slot
		}
	}
	return -1
}
