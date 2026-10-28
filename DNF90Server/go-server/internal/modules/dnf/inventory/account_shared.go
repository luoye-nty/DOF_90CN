package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrAccountSharedMigrationConflict = errors.New("account-shared inventory migration conflict")

// MigrateAccountSharedSlots atomically moves stale character-owned crystal and
// soul slots into their account owner. Callers may invoke it before producing
// an op13 list-0 snapshot; repeated calls are safe once the migration commits.
func (o *Owner) MigrateAccountSharedSlots(ctx context.Context, accountID, characterID string) error {
	if o == nil || o.repo == nil || o.accountRepo == nil || o.accountItems == nil {
		return ErrAccountSharedOwnerUnavailable
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ErrAccountRequired
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ErrCharacterRequired
	}

	return o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
		character, exists, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrInventoryNotFound
		}
		account, _, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}

		character = dnfrepo.CloneInventory(character)
		account = dnfrepo.CloneAccountInventory(account)
		account.AccountID = accountID
		if character.Slots == nil {
			character.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if account.Slots == nil {
			account.Slots = make(map[string]dnfrepo.ItemStack)
		}

		// Preflight the complete range before staging either owner. A single
		// conflicting slot must leave all twelve slots byte-for-byte unchanged.
		for slot := dnfrepo.CrystalWarehouseFirstSlot; slot <= dnfrepo.SoulWarehouseLastSlot; slot++ {
			key := dnfrepo.AccountSharedInventorySlotKey(slot)
			legacy, legacyExists := character.Slots[key]
			current, currentExists := account.Slots[key]
			if legacyExists && currentExists && !accountSharedStacksEqual(legacy, current) {
				return fmt.Errorf("%w: slot=%d", ErrAccountSharedMigrationConflict, slot)
			}
		}

		accountDirty := false
		characterDirty := false
		for slot := dnfrepo.CrystalWarehouseFirstSlot; slot <= dnfrepo.SoulWarehouseLastSlot; slot++ {
			key := dnfrepo.AccountSharedInventorySlotKey(slot)
			legacy, legacyExists := character.Slots[key]
			if !legacyExists {
				continue
			}
			if _, currentExists := account.Slots[key]; !currentExists {
				account.Slots[key] = cloneStack(legacy)
				accountDirty = true
			}
			delete(character.Slots, key)
			characterDirty = true
		}

		// 2026-09-21 补上的收敛兜底。
		//
		// 上面那一轮只处理"同一个线地址"的旧数据（角色记录的 0:354..365 与账号记录
		// 同名同槽）。可只要某条获取途径漏查固定槽表，道具就会落在**别的背包格**里
		// ——现场：1111 用金币买的 20 个无色落在 0:154，而 0:358 另有 43890 个；
		// 那是同一个 list-0 地址空间里的另一行，上面的循环永远看不见它。
		//
		// 这里按"道具 ID → 固定槽"横扫角色自己的 list-0 格，把这类错位行并回它们的家。
		// 它是**尽力而为、绝不抛错**：调用方
		// (buildCurrentItemListBodyForSession) 一旦收到错误就整条 list-0 都发不出去，
		// 那会让玩家背包直接空白 —— 为了修一行脏数据而砸掉整个登录，不划算。
		sweptAccountDirty, sweptCharacterDirty := migrateMisplacedAccountSharedRows(character.Slots, account.Slots)
		accountDirty = accountDirty || sweptAccountDirty
		characterDirty = characterDirty || sweptCharacterDirty

		if !accountDirty && !characterDirty {
			return nil
		}
		now := time.Now()
		if characterDirty {
			character.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, characters, character, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		if accountDirty {
			account.UpdatedAt = now
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrateMisplacedAccountSharedRows 把落在角色 list-0 **错误格位**上的账号共享道具
// （六个晶块 + 六个灵魂）并回它们在账号记录里的固定槽，返回两个 owner 是否被改动。
//
// 存在的理由：这 12 个道具只允许出现在账号共享槽 354..365，可只要哪条获取途径漏查
// 固定槽表，它们就会落进普通背包格。逐个功能补丁治不了根（现场 2026-09-21 就有
// 30 多条路径没查表），这一步是"无论谁漏了都能收敛"的兜底。
//
// 只搬能安全搬的行，三条原则：
//   * 只动 list 0（背包）。个人仓库(list 2)与账号金库(list 12)是仓库，运营口径允许
//     玩家把晶块/灵魂存进去，不在这里搬走；
//   * 固定槽已被**别的**道具占着时绝不驱赶 —— 宁可留一行错位数据，也不把玩家的东西挪丢；
//   * 绑定状态或到期时间与账号行不一致时不合并 —— 合并会改变物品语义。
//
// 被跳过的行原样留在背包里；本函数不返回错误，调用方不会因此失败。
func migrateMisplacedAccountSharedRows(
	characterSlots map[string]dnfrepo.ItemStack,
	accountSlots map[string]dnfrepo.ItemStack,
) (accountDirty bool, characterDirty bool) {
	if len(characterSlots) == 0 {
		return false, false
	}
	type misplacedRow struct {
		key       string
		stack     dnfrepo.ItemStack
		fixedSlot int16
	}
	// 先收集再改动：不能一边遍历 map 一边删元素。
	rows := make([]misplacedRow, 0, 4)
	for key, stack := range characterSlots {
		listType, slot, ok := dnfrepo.ParseInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || stack.ItemID <= 0 || stack.Count <= 0 {
			continue
		}
		if dnfrepo.IsAccountSharedInventorySlot(listType, slot) {
			// 已经在 354..365 的同一线地址上：那是上面那一轮的职责，这里不碰。
			continue
		}
		fixedSlot, member := dnfrepo.AccountSharedWarehouseSlotForItem(stack.ItemID)
		if !member {
			continue
		}
		rows = append(rows, misplacedRow{key: key, stack: stack, fixedSlot: fixedSlot})
	}
	for _, row := range rows {
		targetKey := dnfrepo.AccountSharedInventorySlotKey(row.fixedSlot)
		current, occupied := accountSlots[targetKey]
		if occupied && current.ItemID > 0 {
			if current.ItemID != row.stack.ItemID ||
				current.Bind != row.stack.Bind ||
				!current.ExpireAt.Equal(row.stack.ExpireAt) {
				continue
			}
			merged := cloneStack(current)
			merged.Count += row.stack.Count
			accountSlots[targetKey] = merged
		} else {
			accountSlots[targetKey] = cloneStack(row.stack)
		}
		delete(characterSlots, row.key)
		accountDirty = true
		characterDirty = true
	}
	return accountDirty, characterDirty
}

func accountSharedStacksEqual(left, right dnfrepo.ItemStack) bool {
	return left.ItemID == right.ItemID &&
		left.Count == right.Count &&
		left.Bind == right.Bind &&
		left.ExpireAt.Equal(right.ExpireAt) &&
		bytes.Equal(left.RawEntry, right.RawEntry) &&
		maps.Equal(left.Extra, right.Extra)
}

// LoadWithAccountSharedSlots returns the character inventory with account-owned
// crystal/soul slots overlaid at their unchanged list-0 wire addresses.
func (o *Owner) LoadWithAccountSharedSlots(ctx context.Context, accountID string, selectedCharacterID uint16) (dnfrepo.InventoryRecord, bool, error) {
	if o == nil || o.repo == nil || o.accountRepo == nil {
		return dnfrepo.InventoryRecord{}, false, ErrAccountSharedOwnerUnavailable
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return dnfrepo.InventoryRecord{}, false, ErrAccountRequired
	}
	if selectedCharacterID == 0 {
		return dnfrepo.InventoryRecord{}, false, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(selectedCharacterID), 10)
	character, exists, err := o.repo.Load(ctx, characterID)
	if err != nil || !exists {
		return dnfrepo.InventoryRecord{}, exists, err
	}
	account, _, err := o.accountRepo.Load(ctx, accountID)
	if err != nil {
		return dnfrepo.InventoryRecord{}, false, err
	}
	return dnfrepo.MergeAccountSharedInventory(character, account), true, nil
}

func (o *Owner) moveAccountShared(ctx context.Context, characterID string, cmd Command) (MoveResult, error) {
	if strings.TrimSpace(cmd.AccountID) == "" {
		return MoveResult{}, ErrAccountRequired
	}
	if o.accountRepo == nil || o.accountItems == nil {
		return MoveResult{}, ErrAccountSharedOwnerUnavailable
	}
	if !o.inAccountItemTx {
		var result MoveResult
		err := o.accountItems.WithinAccountCharacterItems(ctx, cmd.AccountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
			txOwner := *o
			txOwner.accountRepo = accounts
			txOwner.repo = characters
			txOwner.inAccountItemTx = true
			var err error
			result, err = txOwner.moveAccountShared(ctx, characterID, cmd)
			return err
		})
		return result, err
	}

	character, characterExists, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	if !characterExists {
		return MoveResult{}, ErrInventoryNotFound
	}
	character = dnfrepo.CloneInventory(character)
	if character.Slots == nil {
		character.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if character.Warehouse == nil {
		character.Warehouse = make(map[string]dnfrepo.ItemStack)
	}

	account, accountExists, err := o.accountRepo.Load(ctx, cmd.AccountID)
	if err != nil {
		return MoveResult{}, err
	}
	if !accountExists {
		account = dnfrepo.AccountInventoryRecord{AccountID: strings.TrimSpace(cmd.AccountID)}
	}
	account = dnfrepo.CloneAccountInventory(account)
	account.AccountID = strings.TrimSpace(cmd.AccountID)
	if account.Slots == nil {
		account.Slots = make(map[string]dnfrepo.ItemStack)
	}

	sourceAccountOwned := dnfrepo.IsAccountSharedInventorySlot(cmd.SourceListType, cmd.SourceSlotIndex)
	destinationAccountOwned := dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex)
	srcItems, srcField := accountSharedItemMap(&character, &account, cmd.SourceListType, sourceAccountOwned)
	dstItems, dstField := accountSharedItemMap(&character, &account, cmd.DestinationListType, destinationAccountOwned)
	srcKey := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	dstKey := slotKey(cmd.DestinationListType, cmd.DestinationSlotIndex)
	result := MoveResult{
		CharacterID:          characterID,
		SourceListType:       cmd.SourceListType,
		SourceSlotIndex:      cmd.SourceSlotIndex,
		DestinationListType:  cmd.DestinationListType,
		DestinationSlotIndex: cmd.DestinationSlotIndex,
		MoveCount:            int64(cmd.MoveCount),
		Mode:                 "noop",
	}
	if sourceAccountOwned == destinationAccountOwned && srcField == dstField && srcKey == dstKey {
		return result, nil
	}

	source, sourceOK := srcItems[srcKey]
	destination, destinationOK := dstItems[dstKey]
	if sourceOK {
		source = o.canonicalStackForMove(source)
	}
	if destinationOK {
		destination = o.canonicalStackForMove(destination)
	}
	if !sourceOK {
		if !destinationOK {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if destination.Count <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		moveCount := o.normalizeMoveCount(destination, int64(cmd.MoveCount))
		if moveCount <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		if o.canSplitStack(destination, moveCount) {
			moved := cloneStack(destination)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			destination.Count -= moveCount
			updateStackRawAmount(&destination)
			dstItems[dstKey] = destination
			srcItems[srcKey] = moved
			result.MoveCount = moveCount
			result.Mode = "reverse_split"
			if cmd.SourceListType == cmd.DestinationListType {
				result, err = withAccountSharedCountRefresh(character, account, result, cmd.SourceListType)
				if err != nil {
					return MoveResult{}, err
				}
			}
			return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
		}
		delete(dstItems, dstKey)
		srcItems[srcKey] = destination
		result.MoveCount = destination.Count
		result.Mode = "reverse_move"
		markEmptiedAccountSharedSlot(&result, destinationAccountOwned, cmd.DestinationSlotIndex)
		return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
	}
	if source.Count <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}

	moveCount := o.normalizeMoveCount(source, int64(cmd.MoveCount))
	if moveCount <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}
	if !destinationOK {
		if o.canSplitStack(source, moveCount) {
			moved := cloneStack(source)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
			dstItems[dstKey] = moved
			result.MoveCount = moveCount
			result.Mode = "split"
			if cmd.SourceListType == cmd.DestinationListType {
				result, err = withAccountSharedCountRefresh(character, account, result, cmd.SourceListType)
				if err != nil {
					return MoveResult{}, err
				}
			}
			return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
		}
		delete(srcItems, srcKey)
		dstItems[dstKey] = source
		result.MoveCount = source.Count
		result.Mode = "move"
		markEmptiedAccountSharedSlot(&result, sourceAccountOwned, cmd.SourceSlotIndex)
		return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
	}

	if o.canStack(source, destination, moveCount) {
		destination = mergeIntoStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, source, moveCount)
		if source.Count <= moveCount {
			delete(srcItems, srcKey)
			markEmptiedAccountSharedSlot(&result, sourceAccountOwned, cmd.SourceSlotIndex)
		} else {
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
		}
		dstItems[dstKey] = destination
		result.MoveCount = moveCount
		result.Mode = "stack"
		result, err = withAccountSharedCountRefresh(
			character,
			account,
			result,
			cmd.SourceListType,
			cmd.DestinationListType,
		)
		if err != nil {
			return MoveResult{}, err
		}
		return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
	}

	srcItems[srcKey] = destination
	dstItems[dstKey] = source
	result.MoveCount = source.Count
	result.Mode = "swap"
	return o.saveAccountSharedMove(ctx, character, account, srcField, dstField, sourceAccountOwned, destinationAccountOwned, result)
}

// markEmptiedAccountSharedSlot 记下这次移动搬空的账号共享晶体/灵魂槽。端点是账号
// 共享槽（list 0, 354..365）且它那一行已经被删掉时才记：bridge 据此在 ACK 之后补
// 删除行，否则客户端右侧晶体/灵魂面板会把搬走的那堆继续画在原来的格子上。
func markEmptiedAccountSharedSlot(result *MoveResult, accountOwned bool, slot int16) {
	if result == nil || !accountOwned ||
		!dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
		return
	}
	for _, existing := range result.EmptiedAccountSharedSlots {
		if existing == slot {
			return
		}
	}
	result.EmptiedAccountSharedSlots = append(result.EmptiedAccountSharedSlots, slot)
}

func accountSharedItemMap(
	character *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	listType byte,
	accountOwned bool,
) (map[string]dnfrepo.ItemStack, dnfrepo.InventoryField) {
	if accountOwned {
		return account.Slots, dnfrepo.InventoryFieldSlots
	}
	return itemMapForList(character, listType)
}

func (o *Owner) saveAccountSharedMove(
	ctx context.Context,
	character dnfrepo.InventoryRecord,
	account dnfrepo.AccountInventoryRecord,
	sourceField dnfrepo.InventoryField,
	destinationField dnfrepo.InventoryField,
	sourceAccountOwned bool,
	destinationAccountOwned bool,
	result MoveResult,
) (MoveResult, error) {
	if isMainItemQuickSlot(result.SourceListType, result.SourceSlotIndex) ||
		isMainItemQuickSlot(result.DestinationListType, result.DestinationSlotIndex) {
		if err := o.validateMainQuickSlotCharmExclusivity(character); err != nil {
			return MoveResult{}, err
		}
	}
	now := time.Now()
	accountDirty := sourceAccountOwned || destinationAccountOwned
	characterFields := make([]dnfrepo.InventoryField, 0, 2)
	if !sourceAccountOwned {
		characterFields = append(characterFields, sourceField)
	}
	if !destinationAccountOwned && (len(characterFields) == 0 || destinationField != characterFields[0]) {
		characterFields = append(characterFields, destinationField)
	}
	if len(characterFields) != 0 {
		character.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, o.repo, character, characterFields...); err != nil {
			return MoveResult{}, err
		}
	}
	if accountDirty {
		account.UpdatedAt = now
		if err := o.accountRepo.Save(ctx, account); err != nil {
			return MoveResult{}, err
		}
	}
	result.Changed = true
	return result, nil
}

// withAccountSharedCountRefresh freezes the authoritative post-mutation views
// while both account and character owners are still inside the same unit of
// work. Explicit stack merges and same-list partial split/reverse-split use
// these full op13 snapshots; cross-list split and ordinary move/swap operations
// continue to rely on their literal op19 ACK.
func withAccountSharedCountRefresh(
	character dnfrepo.InventoryRecord,
	account dnfrepo.AccountInventoryRecord,
	result MoveResult,
	listTypes ...byte,
) (MoveResult, error) {
	result.Refresh = make(map[byte]map[string]dnfrepo.ItemStack, len(listTypes))
	result.RefreshListTypes = make([]byte, 0, len(listTypes))
	for _, listType := range listTypes {
		if _, exists := result.Refresh[listType]; exists {
			continue
		}

		var items map[string]dnfrepo.ItemStack
		switch listType {
		case listTypeMain:
			items = dnfrepo.MergeAccountSharedInventory(character, account).Slots
		case listTypeAvatar:
			items = character.Slots
		case listTypePersonalCargo:
			items = character.Warehouse
		default:
			return MoveResult{}, fmt.Errorf("%w: account-shared stack refresh list=%d", ErrUnsupportedList, listType)
		}

		result.Refresh[listType] = cloneItemListType(items, listType)
		result.RefreshListTypes = append(result.RefreshListTypes, listType)
	}
	return result, nil
}

func cloneItemListType(items map[string]dnfrepo.ItemStack, listType byte) map[string]dnfrepo.ItemStack {
	out := make(map[string]dnfrepo.ItemStack)
	for key, stack := range items {
		keyListType, _, ok := parseSlotKey(key)
		if !ok || keyListType != listType {
			continue
		}
		out[key] = cloneStack(stack)
	}
	return out
}
