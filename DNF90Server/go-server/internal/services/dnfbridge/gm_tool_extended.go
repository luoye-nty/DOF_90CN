package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GMAccountDetail is the bounded account-management projection exposed to the
// local GM application. Metadata is deliberately projected as named fields so
// unrelated runtime keys are never leaked or accidentally overwritten.
type GMAccountDetail struct {
	Account                 GMAccountSummary `json:"account"`
	Cera                    int64            `json:"cera"`
	HonorExperience         uint64           `json:"honor_experience"`
	HonorLevel              int              `json:"honor_level"`
	HonorGrade              int              `json:"honor_grade"`
	HonorMaximumExperience  uint64           `json:"honor_maximum_experience"`
	GrowthCapsuleExperience uint64           `json:"growth_capsule_experience"`
	GrowthCapsuleMaximum    uint64           `json:"growth_capsule_maximum"`
	CargoGold               int64            `json:"cargo_gold"`
	CargoLevel              int64            `json:"cargo_level"`
	CargoCreated            bool             `json:"cargo_created"`
	OnlineCharacters        int              `json:"online_characters"`
}

type GMCharacterDetail struct {
	Character GMCharacterSummary      `json:"character"`
	Stats     map[string]int64        `json:"stats"`
	Skill     dnfrepo.SkillPointState `json:"skill_points"`
	BonusSP   int64                   `json:"bonus_sp"`
	BonusTP   int64                   `json:"bonus_tp"`
}

type GMInventoryItem struct {
	Container string            `json:"container"`
	Key       string            `json:"key"`
	ListType  int               `json:"list_type"`
	Slot      int               `json:"slot"`
	ItemID    int64             `json:"item_id"`
	Name      string            `json:"name"`
	Count     int64             `json:"count"`
	Bind      bool              `json:"bind"`
	ExpireAt  time.Time         `json:"expire_at,omitempty"`
	ReadOnly  bool              `json:"read_only"`
	Extra     map[string]string `json:"extra,omitempty"`
}

type GMInventorySnapshot struct {
	Character GMCharacterSummary `json:"character"`
	Items     []GMInventoryItem  `json:"items"`
}

type GMAccountCube struct {
	Selection int    `json:"selection"`
	Slot      int    `json:"slot"`
	ItemID    int64  `json:"item_id"`
	Name      string `json:"name"`
	Count     int64  `json:"count"`
}

type GMAccountCubeSnapshot struct {
	Character GMCharacterSummary `json:"character"`
	Cubes     []GMAccountCube    `json:"cubes"`
}

type GMItemQuery struct {
	Query      string
	Kind       string
	Tag        string
	Segment    string
	Group      string
	Job        string
	MinLevel   int64
	MaxLevel   int64
	Rarity     int64
	Expiration string
	Limit      int
	Offset     int
}

type GMItemCategoryCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type GMItemCategories struct {
	Total     int                   `json:"total"`
	Equipment []GMItemCategoryCount `json:"equipment"`
	Stackable []GMItemCategoryCount `json:"stackable"`
}

type GMItemPage struct {
	Items  []GMItemSummary `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func (s *Service) GMAccount(ctx context.Context, accountID string) (GMAccountDetail, error) {
	accountID = strings.TrimSpace(accountID)
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil || accountID == "" {
		return GMAccountDetail{}, ErrGMInvalidRequest
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return GMAccountDetail{}, err
	}
	if !found {
		return GMAccountDetail{}, ErrGMAccountNotFound
	}
	detail := GMAccountDetail{
		Account: GMAccountSummary{
			AccountID: account.AccountID, State: account.State,
			RepresentAccountName: account.RepresentAccountName, UpdatedAt: account.UpdatedAt,
		},
		Cera:            gmMetadataInt(account.Metadata, "account_cera"),
		HonorExperience: account.HonorExp,
		CargoGold:       gmMetadataInt(account.Metadata, "account_cargo_gold"),
		CargoLevel:      gmMetadataInt(account.Metadata, "account_cargo_level"),
		CargoCreated:    gmMetadataBool(account.Metadata, "account_cargo_created"),
	}
	characters, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return GMAccountDetail{}, err
	}
	for _, character := range characters {
		_, numericID, parseErr := gmCharacterID(character.CharacterID)
		if parseErr == nil {
			if _, online := s.onlineGameSession(numericID); online {
				detail.OnlineCharacters++
			}
		}
	}
	if table, tableErr := s.loadHonorTable(ctx); tableErr == nil {
		progress, resolveErr := table.Resolve(account.HonorExp)
		if resolveErr != nil {
			return GMAccountDetail{}, resolveErr
		}
		detail.HonorExperience = progress.TotalExperience
		detail.HonorLevel = progress.Level
		detail.HonorGrade = progress.Grade
		detail.HonorMaximumExperience = table.MaxTotalExperience()
	}
	if s.adventureGroupTable != nil {
		config := s.adventureGroupTable.Runtime()
		state, stateErr := adventuregroup.ParseRuntimeState(account, config, s.gameplayNow())
		if stateErr != nil {
			return GMAccountDetail{}, stateErr
		}
		detail.GrowthCapsuleExperience = state.GrowthExperience
		detail.GrowthCapsuleMaximum = config.Capsule.MaximumExperience
	}
	return detail, nil
}

// GMSetAccountValue changes one explicit account field. Account writes are
// offline-only because the selected-session projection caches all four values.
func (s *Service) GMSetAccountValue(ctx context.Context, accountID, field string, value uint64) (GMAccountDetail, error) {
	accountID = strings.TrimSpace(accountID)
	field = strings.ToLower(strings.TrimSpace(field))
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil || accountID == "" {
		return GMAccountDetail{}, ErrGMInvalidRequest
	}
	if online, err := s.gmAccountOnline(ctx, repositories, accountID); err != nil {
		return GMAccountDetail{}, err
	} else if online {
		return GMAccountDetail{}, ErrGMCharacterOnline
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return GMAccountDetail{}, err
	}
	if !found {
		return GMAccountDetail{}, ErrGMAccountNotFound
	}
	now := time.Now().UTC()
	switch field {
	case "cera":
		// 上限必须是 **MaxInt32** 而不是 MaxUint32：客户端把点券按有符号 32 位读，
		// 一旦写进 (MaxInt32, MaxUint32] 区间的值，玩家看到的是负数。
		// 与 cerashop.MaxWalletBalance / WalletCurrency.SetOn 的口径保持一致。
		if value > uint64(dnfcerashop.MaxWalletBalance) {
			return GMAccountDetail{}, fmt.Errorf("%w: cera exceeds the client's signed 32-bit range", ErrGMInvalidRequest)
		}
		err = dnfrepo.SaveAccountMetadataEntry(ctx, repositories.Account, account, "account_cera", strconv.FormatUint(value, 10), now)
	case "cargo_gold":
		if value > math.MaxInt64 {
			return GMAccountDetail{}, fmt.Errorf("%w: cargo gold exceeds int64", ErrGMInvalidRequest)
		}
		err = dnfrepo.SaveAccountMetadataEntry(ctx, repositories.Account, account, "account_cargo_gold", strconv.FormatUint(value, 10), now)
	case "honor_experience":
		if table, tableErr := s.loadHonorTable(ctx); tableErr == nil && value > table.MaxTotalExperience() {
			value = table.MaxTotalExperience()
		}
		account = dnfrepo.CloneAccount(account)
		account.HonorExp = value
		account.UpdatedAt = now
		err = repositories.Account.Save(ctx, account)
	case "growth_capsule_experience":
		if s.adventureGroupTable == nil {
			return GMAccountDetail{}, ErrGMUnavailable
		}
		config := s.adventureGroupTable.Runtime()
		if config.Capsule.MaximumExperience > 0 && value > config.Capsule.MaximumExperience {
			value = config.Capsule.MaximumExperience
		}
		state, stateErr := adventuregroup.ParseRuntimeState(account, config, s.gameplayNow())
		if stateErr != nil {
			return GMAccountDetail{}, stateErr
		}
		state.GrowthExperience = value
		// 这条 GM 路径没有选中角色（没有 CharacterSettlement 事务可挂），
		// 所以直接写账号元数据；走不到 saveCurrentAdventureRuntime 的 CAS 校验。
		encoded, marshalErr := state.Marshal()
		if marshalErr != nil {
			return GMAccountDetail{}, marshalErr
		}
		err = dnfrepo.SaveAccountMetadataEntry(
			ctx, repositories.Account, account, adventuregroup.RuntimeStateMetadataKey, encoded, now,
		)
	default:
		return GMAccountDetail{}, fmt.Errorf("%w: unsupported account field %q", ErrGMInvalidRequest, field)
	}
	if err != nil {
		return GMAccountDetail{}, err
	}
	return s.GMAccount(ctx, accountID)
}

func (s *Service) GMCharacterDetail(ctx context.Context, characterID string) (GMCharacterDetail, error) {
	character, err := s.GMCharacter(ctx, characterID)
	if err != nil {
		return GMCharacterDetail{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Skill == nil {
		return GMCharacterDetail{}, ErrGMUnavailable
	}
	record, found, err := repositories.Character.Load(ctx, character.CharacterID)
	if err != nil || !found {
		if err != nil {
			return GMCharacterDetail{}, err
		}
		return GMCharacterDetail{}, ErrGMCharacterNotFound
	}
	skill, found, err := repositories.Skill.Load(ctx, character.CharacterID)
	if err != nil {
		return GMCharacterDetail{}, err
	}
	if !found {
		skill = dnfrepo.SkillRecord{CharacterID: character.CharacterID}
	}
	stats := make(map[string]int64, len(record.Stats))
	for key, value := range record.Stats {
		stats[key] = value
	}
	return GMCharacterDetail{Character: character, Stats: stats, Skill: skill.Points, BonusSP: stats["bonus_sp"], BonusTP: stats["bonus_tp"]}, nil
}

// GMSetCharacterSkillBonus persists absolute bonus SP/TP values and adjusts
// both totals and remaining balances by the same delta, preserving spent points.
func (s *Service) GMSetCharacterSkillBonus(ctx context.Context, characterID string, bonusSP, bonusTP int64) (GMCharacterDetail, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil || bonusSP < 0 || bonusTP < 0 || bonusSP > math.MaxUint16 || bonusTP > math.MaxUint16 {
		return GMCharacterDetail{}, fmt.Errorf("%w: bonus SP/TP must be between 0 and %d", ErrGMInvalidRequest, uint64(math.MaxUint16))
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMCharacterDetail{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterProgression == nil {
		return GMCharacterDetail{}, ErrGMUnavailable
	}
	err = repositories.CharacterProgression.WithinCharacterProgression(ctx, characterID, func(characters dnfrepo.CharacterRepository, skills dnfrepo.SkillRepository) error {
		character, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		skill, found, loadErr := skills.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: skill ledger is missing", ErrGMInvalidRequest)
		}
		character = dnfrepo.CloneCharacter(character)
		skill = dnfrepo.CloneSkill(skill)
		oldSP, oldTP := character.Stats["bonus_sp"], character.Stats["bonus_tp"]
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		spDelta, tpDelta := int(bonusSP-oldSP), int(bonusTP-oldTP)
		nextTotalSP, nextRemainingSP := skill.Points.TotalSP+spDelta, skill.Points.RemainingSP+spDelta
		nextTotalTP, nextRemainingTP := skill.Points.TotalTP+tpDelta, skill.Points.RemainingTP+tpDelta
		if nextTotalSP < 0 || nextRemainingSP < 0 || nextRemainingSP > nextTotalSP || nextTotalSP > math.MaxUint16 ||
			nextTotalTP < 0 || nextRemainingTP < 0 || nextRemainingTP > nextTotalTP || nextTotalTP > math.MaxUint16 {
			return fmt.Errorf("%w: bonus would invalidate the current spent-point ledger", ErrGMInvalidRequest)
		}
		character.Stats["bonus_sp"], character.Stats["bonus_tp"] = bonusSP, bonusTP
		skill.Points.TotalSP, skill.Points.RemainingSP = nextTotalSP, nextRemainingSP
		skill.Points.TotalTP, skill.Points.RemainingTP = nextTotalTP, nextRemainingTP
		now := time.Now().UTC()
		character.UpdatedAt, skill.UpdatedAt = now, now
		if saveErr := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); saveErr != nil {
			return saveErr
		}
		return dnfrepo.SaveSkillFields(ctx, skills, skill, dnfrepo.SkillFieldPoints)
	})
	if err != nil {
		return GMCharacterDetail{}, err
	}
	return s.GMCharacterDetail(ctx, characterID)
}

func (s *Service) GMInventory(ctx context.Context, characterID string) (GMInventorySnapshot, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Inventory == nil || repositories.AccountInventory == nil || repositories.Equipment == nil {
		return GMInventorySnapshot{}, ErrGMUnavailable
	}
	characterRecord, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if !found {
		return GMInventorySnapshot{}, ErrGMCharacterNotFound
	}
	inventory, _, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	accountInventory, _, err := repositories.AccountInventory.Load(ctx, characterRecord.AccountID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	equipment, _, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	items := make([]GMInventoryItem, 0, len(inventory.Slots)+len(inventory.Warehouse)+len(accountInventory.Slots)+len(equipment.Entries))
	appendStackMap := func(container string, entries map[string]dnfrepo.ItemStack) {
		for key, stack := range entries {
			listType, slot := gmSlotKey(key)
			items = append(items, GMInventoryItem{Container: container, Key: key, ListType: listType, Slot: slot,
				ItemID: stack.ItemID, Name: s.gmItemName(stack.ItemID), Count: stack.Count, Bind: stack.Bind,
				ExpireAt: stack.ExpireAt, Extra: gmCloneStringMap(stack.Extra)})
		}
	}
	appendStackMap("inventory", inventory.Slots)
	appendStackMap("warehouse", inventory.Warehouse)
	appendStackMap("account", accountInventory.Slots)
	for key, entry := range equipment.Entries {
		items = append(items, GMInventoryItem{Container: "equipment", Key: key, ListType: 3, Slot: int(entry.SlotIndex),
			ItemID: entry.ItemID, Name: s.gmItemName(entry.ItemID), Count: 1, Bind: entry.Bind,
			ExpireAt: entry.ExpireAt, ReadOnly: true, Extra: gmCloneStringMap(entry.Extra)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Container != items[j].Container {
			return items[i].Container < items[j].Container
		}
		if items[i].ListType != items[j].ListType {
			return items[i].ListType < items[j].ListType
		}
		if items[i].Slot != items[j].Slot {
			return items[i].Slot < items[j].Slot
		}
		return items[i].Key < items[j].Key
	})
	return GMInventorySnapshot{Character: s.gmCharacterSummary(characterRecord), Items: items}, nil
}

// GMAccountCubes returns all six fixed account-shared crystal slots, including
// empty ones, in the current-client native order.
func (s *Service) GMAccountCubes(ctx context.Context, characterID string) (GMAccountCubeSnapshot, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.AccountInventory == nil {
		return GMAccountCubeSnapshot{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	if !found {
		return GMAccountCubeSnapshot{}, ErrGMCharacterNotFound
	}
	inventory, _, err := repositories.AccountInventory.Load(ctx, character.AccountID)
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	premiumCatalog, err := s.currentPremiumCatalog()
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	cubeItemIDs := premiumCatalog.crystalCubeIDs
	cubes := make([]GMAccountCube, 0, len(cubeItemIDs))
	for selection, itemID := range cubeItemIDs {
		if itemID <= 0 {
			return GMAccountCubeSnapshot{}, ErrGMUnavailable
		}
		slot := dnfrepo.CrystalWarehouseFirstSlot + int16(selection)
		stack := inventory.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]
		count := int64(0)
		if stack.ItemID == itemID && stack.Count > 0 {
			count = stack.Count
		}
		cubes = append(cubes, GMAccountCube{Selection: selection, Slot: int(slot), ItemID: itemID, Name: s.gmItemName(itemID), Count: count})
	}
	return GMAccountCubeSnapshot{Character: s.gmCharacterSummary(character), Cubes: cubes}, nil
}

// GMSetAccountCube repairs one fixed account-shared crystal stack. It rejects
// any online character on the account because list-0 is cached in live scenes.
func (s *Service) GMSetAccountCube(ctx context.Context, characterID string, selection int, count int64) (GMAccountCubeSnapshot, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil || selection < 0 || selection >= 6 || count < 0 || uint64(count) > math.MaxUint32 {
		return GMAccountCubeSnapshot{}, fmt.Errorf("%w: cube selection or count is out of range", ErrGMInvalidRequest)
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMAccountCubeSnapshot{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.AccountItems == nil {
		return GMAccountCubeSnapshot{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	if !found {
		return GMAccountCubeSnapshot{}, ErrGMCharacterNotFound
	}
	if online, onlineErr := s.gmAccountOnline(ctx, repositories, character.AccountID); onlineErr != nil {
		return GMAccountCubeSnapshot{}, onlineErr
	} else if online {
		return GMAccountCubeSnapshot{}, ErrGMCharacterOnline
	}
	premiumCatalog, err := s.currentPremiumCatalog()
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	itemID := premiumCatalog.crystalCubeIDs[selection]
	if itemID <= 0 || itemID > math.MaxUint32 {
		return GMAccountCubeSnapshot{}, ErrGMUnavailable
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	definition, err := catalog.ResolveItem(uint32(itemID))
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	slot := dnfrepo.CrystalWarehouseFirstSlot + int16(selection)
	key := dnfrepo.AccountSharedInventorySlotKey(slot)
	err = repositories.AccountItems.WithinAccountCharacterItems(ctx, character.AccountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository) error {
		record, exists, loadErr := accounts.Load(ctx, character.AccountID)
		if loadErr != nil {
			return loadErr
		}
		if !exists {
			record = dnfrepo.AccountInventoryRecord{AccountID: character.AccountID, Slots: make(map[string]dnfrepo.ItemStack)}
		} else {
			record = dnfrepo.CloneAccountInventory(record)
		}
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if count == 0 {
			delete(record.Slots, key)
		} else {
			stack := record.Slots[key]
			if stack.ItemID != itemID {
				stack = dnfrepo.ItemStack{ItemID: itemID, Extra: make(map[string]string)}
			}
			// PVF [stack limit] 是账号金库这一个容器里该道具的总量上限：GM 直接设值
			// 也不能把容器写过上限（同 id 的其它行，包含历史拆行，都要计入）。
			if definition.StackLimit > 0 {
				others := dnfrepo.ContainerItemTotal(record.Slots, dnfrepo.MainInventoryListType, int64(itemID))
				if stack.ItemID == itemID && stack.Count > 0 {
					others -= stack.Count
				}
				if others > definition.StackLimit-count {
					return fmt.Errorf(
						"%w: cube selection=%d count=%d stack_limit=%d already=%d",
						ErrGMInvalidRequest, selection, count, definition.StackLimit, others,
					)
				}
			}
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.ItemID, stack.Count = itemID, count
			stack.Extra["item_kind"] = string(definition.Kind)
			stack.Extra["pvf_path"] = definition.PVFPath
			stack.Extra["stackable_type"] = definition.StackableType
			if definition.StackLimit > 0 {
				stack.Extra["stack_limit"] = strconv.FormatInt(definition.StackLimit, 10)
			}
			entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			record.Slots[key] = stack
		}
		record.UpdatedAt = time.Now().UTC()
		return accounts.Save(ctx, record)
	})
	if err != nil {
		return GMAccountCubeSnapshot{}, err
	}
	return s.GMAccountCubes(ctx, characterID)
}

// GMDeleteInventoryItem 删除某容器里指定格子的一件/一堆物品。
//
// 在线角色现在也允许删（旧实现直接返回 ErrGMCharacterOnline，等于"删个东西先踢人"）。
// 安全依据：仓储层每次改动都在事务内重新 Load → 不存在服务端内存快照覆盖问题；
// 真正会不一致的只有客户端自己缓存的容器显示，提交后用 gmPushInventorySlotRefreshes
// 走与游戏内改动同一条 op14 增量刷新路径补发变更行（空格子以删除行下发，客户端立刻消失）。
func (s *Service) GMDeleteInventoryItem(ctx context.Context, characterID, container, key string, count int64) (GMInventorySnapshot, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil || strings.TrimSpace(key) == "" || count < 0 {
		return GMInventorySnapshot{}, ErrGMInvalidRequest
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return GMInventorySnapshot{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if !found {
		return GMInventorySnapshot{}, ErrGMCharacterNotFound
	}
	removedKeys := []string{strings.TrimSpace(key)}
	container = strings.ToLower(strings.TrimSpace(container))
	switch container {
	case "inventory", "warehouse":
		if repositories.CharacterItems == nil {
			return GMInventorySnapshot{}, ErrGMUnavailable
		}
		err = repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
			record, exists, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !exists {
				return ErrGMCharacterNotFound
			}
			record = dnfrepo.CloneInventory(record)
			entries, field := record.Slots, dnfrepo.InventoryFieldSlots
			if container == "warehouse" {
				entries, field = record.Warehouse, dnfrepo.InventoryFieldWarehouse
			}
			if mutateErr := gmRemoveStack(entries, key, count); mutateErr != nil {
				return mutateErr
			}
			record.UpdatedAt = time.Now().UTC()
			return dnfrepo.SaveInventoryFields(ctx, inventories, record, field)
		})
	case "account":
		if repositories.AccountItems == nil {
			return GMInventorySnapshot{}, ErrGMUnavailable
		}
		// 账号金库（账号共享的晶体/灵魂仓库）同样放开在线：它们是 list 0 的 354..365 槽，
		// 增量刷新路径对这几个槽有专门处理（账号记录优先），删除行会把对应小面板清掉，
		// 正是"删掉了"该有的表现。
		err = repositories.AccountItems.WithinAccountCharacterItems(ctx, character.AccountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository) error {
			record, exists, loadErr := accounts.Load(ctx, character.AccountID)
			if loadErr != nil {
				return loadErr
			}
			if !exists {
				return fmt.Errorf("%w: account inventory is missing", ErrGMInvalidRequest)
			}
			record = dnfrepo.CloneAccountInventory(record)
			if mutateErr := gmRemoveStack(record.Slots, key, count); mutateErr != nil {
				return mutateErr
			}
			record.UpdatedAt = time.Now().UTC()
			return accounts.Save(ctx, record)
		})
	default:
		return GMInventorySnapshot{}, fmt.Errorf("%w: container is read-only or unsupported", ErrGMInvalidRequest)
	}
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	// 提交成功后再推刷新：仓库已改，客户端那份显示必须跟上。
	if refreshErr := s.gmPushInventorySlotRefreshes(ctx, characterID, removedKeys, "gm_admin_inventory_delete"); refreshErr != "" {
		s.logWarn("dnfbridge GM inventory delete live refresh deferred",
			"character_id", characterID, "container", container, "keys", removedKeys, "error", refreshErr)
	}
	return s.GMInventory(ctx, characterID)
}

// GMClearInventory 清空整个容器（不可撤销）。
//
// 与 GMDeleteInventoryItem 一样放开在线角色：清空是"把该容器每一格都变成空"，
// 用这些格子的增量刷新（空格 → 删除行）就能让在线客户端立刻清空。
// 清空前的占用格子必须在事务内先取出来——清完就没法知道要给客户端发哪些行了。
func (s *Service) GMClearInventory(ctx context.Context, characterID, container string) (GMInventorySnapshot, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	container = strings.ToLower(strings.TrimSpace(container))
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return GMInventorySnapshot{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if !found {
		return GMInventorySnapshot{}, ErrGMCharacterNotFound
	}
	clearedKeys := make([]string, 0, 64)
	switch container {
	case "inventory", "warehouse":
		if repositories.CharacterItems == nil {
			return GMInventorySnapshot{}, ErrGMUnavailable
		}
		err = repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
			record, exists, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !exists {
				return ErrGMCharacterNotFound
			}
			record = dnfrepo.CloneInventory(record)
			field := dnfrepo.InventoryFieldSlots
			if container == "inventory" {
				clearedKeys = inventoryStackKeys(record.Slots)
				record.Slots = make(map[string]dnfrepo.ItemStack)
			} else {
				clearedKeys = inventoryStackKeys(record.Warehouse)
				record.Warehouse = make(map[string]dnfrepo.ItemStack)
				field = dnfrepo.InventoryFieldWarehouse
			}
			record.UpdatedAt = time.Now().UTC()
			return dnfrepo.SaveInventoryFields(ctx, inventories, record, field)
		})
	case "account":
		if repositories.AccountItems == nil {
			return GMInventorySnapshot{}, ErrGMUnavailable
		}
		err = repositories.AccountItems.WithinAccountCharacterItems(ctx, character.AccountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository) error {
			record, exists, loadErr := accounts.Load(ctx, character.AccountID)
			if loadErr != nil {
				return loadErr
			}
			if !exists {
				return nil
			}
			record = dnfrepo.CloneAccountInventory(record)
			clearedKeys = inventoryStackKeys(record.Slots)
			record.Slots = make(map[string]dnfrepo.ItemStack)
			record.UpdatedAt = time.Now().UTC()
			return accounts.Save(ctx, record)
		})
	default:
		return GMInventorySnapshot{}, fmt.Errorf("%w: container is read-only or unsupported", ErrGMInvalidRequest)
	}
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if refreshErr := s.gmPushInventorySlotRefreshes(ctx, characterID, clearedKeys, "gm_admin_inventory_clear"); refreshErr != "" {
		s.logWarn("dnfbridge GM inventory clear live refresh deferred",
			"character_id", characterID, "container", container, "slot_count", len(clearedKeys), "error", refreshErr)
	}
	return s.GMInventory(ctx, characterID)
}

// inventoryStackKeys 列出容器里当前占用的格子键（供清空时给在线客户端发删除行）。
func inventoryStackKeys(entries map[string]dnfrepo.ItemStack) []string {
	if len(entries) == 0 {
		return nil
	}
	keys := make([]string, 0, len(entries))
	for key, stack := range entries {
		if stack.ItemID > 0 && stack.Count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// gmPushInventorySlotRefreshes 把若干格子的最新状态推给在线客户端，返回刷新失败原因（空=成功或无人在线）。
//
// 为什么 GM 直改仓储之后必须补这一步：
//
//	GM 写的是仓储（repository），玩家屏幕上显示的是客户端自己缓存的容器。
//	旧实现因为怕两边不一致，干脆拒绝在线角色（ErrGMCharacterOnline）——代价是
//	"删个东西还得先把人踢下线"。实际上仓储层每次改动都在事务内重新 Load，
//	服务端并不存在内存快照互相覆盖的问题；会不一致的只有客户端那份显示。
//	所以在提交之后，用与游戏内改动同一条 op14 增量刷新路径把变更行发下去：
//	占用的格子发实际物品行，空掉的格子发 0xFFFFFFFF 删除行，客户端立刻反映。
//
// 刷新失败**不影响**返回值：仓储改动已经提交，这只是一次尽力而为的实时同步，
// 失败时由调用方记录日志、下一次打开容器时自然同步。
func (s *Service) gmPushInventorySlotRefreshes(ctx context.Context, characterID string, keys []string, source string) string {
	if s == nil || len(keys) == 0 {
		return ""
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		listType, slot := gmSlotKey(key)
		if listType < 0 || slot < 0 {
			// 键形态不认识就跳过这一个，不要因为一格的问题放弃整批刷新。
			continue
		}
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  byte(listType),
			SlotIndex: int16(slot),
		})
	}
	if len(refreshes) == 0 {
		return ""
	}
	result := s.gmRefreshOnlineCharacter(characterID, source, func(session *gameSession) error {
		return s.sendSelectedIncrementalItemSlotRefreshes(session, source, refreshes)
	})
	return result.RefreshError
}

func (s *Service) GMSearchItemPage(ctx context.Context, query GMItemQuery) (GMItemPage, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 200 || query.Offset < 0 {
		return GMItemPage{}, ErrGMInvalidRequest
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMItemPage{}, err
	}
	q, kind, expiration := strings.ToLower(strings.TrimSpace(query.Query)), strings.ToLower(strings.TrimSpace(query.Kind)), strings.ToLower(strings.TrimSpace(query.Expiration))
	tag, segment := gmItemTypeTag(query.Tag), strings.TrimSpace(query.Segment)
	group, job := strings.ToLower(strings.TrimSpace(query.Group)), gmItemTypeTag(query.Job)
	catalog.mu.Lock()
	ids := make([]uint32, 0, len(catalog.itemRefs))
	for id := range catalog.itemRefs {
		ids = append(ids, id)
	}
	catalog.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	matches := make([]GMItemSummary, 0, query.Limit)
	total := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return GMItemPage{}, err
		}
		definition, resolveErr := catalog.ResolveItem(id)
		if resolveErr != nil {
			continue
		}
		item := gmItemSummary(definition)
		if q != "" && !strings.Contains(strings.ToLower(item.Name), q) && !strings.Contains(strings.ToLower(item.PVFPath), q) && strconv.FormatUint(uint64(item.ItemID), 10) != q {
			continue
		}
		if kind != "" && kind != "all" && strings.ToLower(item.Kind) != kind {
			continue
		}
		if tag != "" && gmItemTypeTag(item.EquipmentType) != tag {
			continue
		}
		if segment != "" && gmStackableSegment(item.StackableType) != segment {
			continue
		}
		if group != "" && gmItemGroup(item) != group {
			continue
		}
		if job != "" {
			effectiveJobs := gmItemEffectiveJobs(catalog, definition)
			if len(effectiveJobs) > 0 {
				if !gmJobTagsContain(effectiveJobs, job) {
					continue
				}
				item.UsableJobs = gmBracketJobTags(effectiveJobs)
			} else if !gmItemMatchesJob(item, job) {
				continue
			}
		}
		if query.MinLevel > 0 && item.MinimumLevel < query.MinLevel {
			continue
		}
		if query.MaxLevel > 0 && item.MinimumLevel > query.MaxLevel {
			continue
		}
		if query.Rarity >= 0 && item.Rarity != query.Rarity {
			continue
		}
		if expiration == "permanent" && (item.UsablePeriodDays > 0 || !item.ExpirationDate.IsZero()) {
			continue
		}
		if expiration == "timed" && item.UsablePeriodDays <= 0 && item.ExpirationDate.IsZero() {
			continue
		}
		if total >= query.Offset && len(matches) < query.Limit {
			matches = append(matches, item)
		}
		total++
	}
	return GMItemPage{Items: matches, Total: total, Limit: query.Limit, Offset: query.Offset}, nil
}

func gmItemTypeTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
}

func gmStackableSegment(stackableType string) string {
	value := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stackableType, "`", "")))
	switch {
	case strings.HasPrefix(value, "[material]"):
		if strings.HasSuffix(value, "4") {
			return "特殊材料"
		}
		return "材料"
	case strings.HasPrefix(value, "[quest]"):
		return "任务品"
	case strings.HasPrefix(value, "[material expert job]"):
		return "副职业材料"
	case strings.HasPrefix(value, "[avatar emblem]"):
		return "徽章"
	default:
		return "消耗品"
	}
}

func gmItemGroup(item GMItemSummary) string {
	if item.Kind == string(dungeonDropItemStackable) {
		return "stackable"
	}
	tag := gmItemTypeTag(item.EquipmentType)
	switch tag {
	case "creature", "artifact red", "artifact blue", "artifact green":
		return "pet"
	case "hat avatar", "hair avatar", "face avatar", "coat avatar", "breast avatar", "waist avatar", "pants avatar", "shoes avatar", "skin avatar", "aurora avatar", "weapon avatar":
		return "avatar"
	case "weapon", "coat", "shoulder", "pants", "shoes", "waist", "amulet", "wrist", "ring", "support", "magic stone", "earring", "support weapon", "title name", "name tag":
		return "equipment"
	default:
		return "other"
	}
}

func gmItemMatchesJob(item GMItemSummary, job string) bool {
	if len(item.UsableJobs) == 0 {
		return true
	}
	for _, candidate := range item.UsableJobs {
		normalized := gmItemTypeTag(candidate)
		if normalized == job || normalized == "all" {
			return true
		}
	}
	return false
}

// gmItemEffectiveJobs returns explicit profession restrictions first. For a
// package whose outer PVF declares [all], it walks [package data] and derives
// restrictions from contained equipment or nested packages. Truly generic
// consumables remain unrestricted because no specific descendant job exists.
func gmItemEffectiveJobs(catalog *pvfDungeonDropCatalog, definition dungeonDropItemDefinition) []string {
	return gmCollectItemEffectiveJobs(catalog, definition, make(map[uint32]struct{}), 0)
}

func gmCollectItemEffectiveJobs(
	catalog *pvfDungeonDropCatalog,
	definition dungeonDropItemDefinition,
	visited map[uint32]struct{},
	depth int,
) []string {
	if jobs := gmSpecificJobTags(definition.UsableJobs); len(jobs) > 0 {
		return jobs
	}
	if catalog == nil || definition.Kind != dungeonDropItemStackable || len(definition.PackageRewardIDs) == 0 || depth >= 8 {
		return nil
	}
	if _, exists := visited[definition.ItemID]; exists {
		return nil
	}
	visited[definition.ItemID] = struct{}{}
	defer delete(visited, definition.ItemID)

	jobSet := make(map[string]struct{})
	for _, rewardID := range definition.PackageRewardIDs {
		reward, err := catalog.ResolveItem(rewardID)
		if err != nil {
			continue
		}
		for _, job := range gmCollectItemEffectiveJobs(catalog, reward, visited, depth+1) {
			jobSet[job] = struct{}{}
		}
	}
	if len(jobSet) == 0 {
		return nil
	}
	jobs := make([]string, 0, len(jobSet))
	for job := range jobSet {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	return jobs
}

func gmSpecificJobTags(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	jobs := make([]string, 0, len(values))
	for _, value := range values {
		job := gmItemTypeTag(value)
		if job == "" || job == "all" {
			continue
		}
		if _, duplicate := seen[job]; duplicate {
			continue
		}
		seen[job] = struct{}{}
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	return jobs
}

func gmJobTagsContain(values []string, job string) bool {
	job = gmItemTypeTag(job)
	for _, value := range values {
		if gmItemTypeTag(value) == job {
			return true
		}
	}
	return false
}

func gmBracketJobTags(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = gmItemTypeTag(value); value != "" {
			out = append(out, "["+value+"]")
		}
	}
	return out
}

func (s *Service) GMItemCategories(ctx context.Context) (GMItemCategories, error) {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMItemCategories{}, err
	}
	catalog.mu.Lock()
	ids := make([]uint32, 0, len(catalog.itemRefs))
	for id := range catalog.itemRefs {
		ids = append(ids, id)
	}
	catalog.mu.Unlock()
	equipment, stackable := make(map[string]int), make(map[string]int)
	total := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return GMItemCategories{}, err
		}
		definition, resolveErr := catalog.ResolveItem(id)
		if resolveErr != nil {
			continue
		}
		total++
		if definition.Kind == dungeonDropItemEquipment {
			tag := gmItemTypeTag(definition.EquipmentType)
			if tag == "" {
				tag = "(无标签)"
			}
			equipment[tag]++
		} else if definition.Kind == dungeonDropItemStackable {
			stackable[gmStackableSegment(definition.StackableType)]++
		}
	}
	toCounts := func(values map[string]int) []GMItemCategoryCount {
		result := make([]GMItemCategoryCount, 0, len(values))
		for name, count := range values {
			result = append(result, GMItemCategoryCount{Name: name, Count: count})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
		return result
	}
	return GMItemCategories{Total: total, Equipment: toCounts(equipment), Stackable: toCounts(stackable)}, nil
}

func (s *Service) GMCompleteQuestBatch(ctx context.Context, characterID string, questIDs []int64) (GMQuestOverview, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil || len(questIDs) == 0 || len(questIDs) > 200 {
		return GMQuestOverview{}, ErrGMInvalidRequest
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestOverview{}, ErrGMCharacterOnline
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return GMQuestOverview{}, err
	}
	seen := make(map[int64]struct{}, len(questIDs))
	for _, questID := range questIDs {
		if questID <= 0 || questID > math.MaxUint16 {
			return GMQuestOverview{}, ErrGMInvalidRequest
		}
		if _, duplicate := seen[questID]; duplicate {
			continue
		}
		seen[questID] = struct{}{}
		definition, found := catalog.Find(questID)
		if !found {
			return GMQuestOverview{}, fmt.Errorf("%w: quest %d is not present in the active Script.pvf", ErrGMInvalidRequest, questID)
		}
		if gmProfessionRewardType(definition.RewardType) {
			return GMQuestOverview{}, fmt.Errorf("%w: batch completion excludes profession quests", ErrGMInvalidRequest)
		}
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMQuestOverview{}, ErrGMUnavailable
	}
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil {
			return ErrGMUnavailable
		}
		if _, found, loadErr := tx.Character.Load(ctx, characterID); loadErr != nil {
			return loadErr
		} else if !found {
			return ErrGMCharacterNotFound
		}
		record, found, loadErr := tx.Quest.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			record = dnfrepo.QuestRecord{CharacterID: characterID}
		} else {
			record = dnfrepo.CloneQuest(record)
		}
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		now := time.Now().UTC()
		for questID := range seen {
			definition, _ := catalog.Find(questID)
			state, _ := gmQuestState(record, questID)
			if state.Extra == nil {
				state.Extra = make(map[string]string)
			}
			state.Status, state.ProgressValue, state.UpdatedAt = "completed", 0, now
			state.Extra["pvf_path"] = definition.Path
			state.Extra["gm_batch_completed"] = "true"
			state.Extra["gm_rewards_granted"] = "false"
			record.States[questID] = state
			delete(record.Progress, questID)
		}
		record.UpdatedAt = now
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, record, dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress)
	})
	if err != nil {
		return GMQuestOverview{}, err
	}
	return s.GMQuestOverview(ctx, characterID)
}

func (s *Service) gmAccountOnline(ctx context.Context, repositories dnfrepo.Group, accountID string) (bool, error) {
	characters, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return false, err
	}
	for _, character := range characters {
		_, numericID, parseErr := gmCharacterID(character.CharacterID)
		if parseErr == nil {
			if _, online := s.onlineGameSession(numericID); online {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Service) gmItemName(itemID int64) string {
	if itemID <= 0 || itemID > math.MaxUint32 {
		return fmt.Sprintf("物品 %d", itemID)
	}
	catalog, loaded, err := s.currentPVFItemCatalogIfLoaded()
	if err != nil || !loaded || catalog == nil {
		return fmt.Sprintf("物品 %d", itemID)
	}
	definition, err := catalog.ResolveItem(uint32(itemID))
	if err != nil {
		return fmt.Sprintf("物品 %d", itemID)
	}
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		name = definition.PVFPath
	}
	return name
}

func gmMetadataInt(metadata map[string]string, key string) int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(metadata[key]), 10, 64)
	return value
}

func gmMetadataBool(metadata map[string]string, key string) bool {
	value := strings.TrimSpace(metadata[key])
	return value == "1" || strings.EqualFold(value, "true")
}

func gmSlotKey(key string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(key), ":", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	listType, firstErr := strconv.Atoi(parts[0])
	slot, secondErr := strconv.Atoi(parts[1])
	if firstErr != nil || secondErr != nil {
		return -1, -1
	}
	return listType, slot
}

func gmCloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func gmRemoveStack(entries map[string]dnfrepo.ItemStack, key string, count int64) error {
	stack, found := entries[key]
	if !found || stack.Count <= 0 {
		return fmt.Errorf("%w: inventory slot %q was not found", ErrGMInvalidRequest, key)
	}
	if count == 0 || count >= stack.Count {
		delete(entries, key)
		return nil
	}
	stack.Count -= count
	entries[key] = stack
	return nil
}
