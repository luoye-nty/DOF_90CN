package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	dnfachievement "longheng.io/server/internal/modules/dnf/achievement"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftitlebook "longheng.io/server/internal/modules/dnf/titlebook"
)

// Title book uses inventory list type 100 with slot encoding:
// slot = category * 1000 + bookIndex
const (
	currentTitleBookListType       byte   = dnftitlebook.ListType
	currentTitleBookSlotBase       int16  = dnftitlebook.SlotBase
	currentTitleBookMsgID          uint16 = 0x0166
	currentTitleBookPutMsgID       uint16 = uint16(dnfenum.CmdPacketTitleBookPut)
	currentTitleBookGetMsgID       uint16 = uint16(dnfenum.CmdPacketTitleBookGet)
	currentTitleBookMaxPerCategory        = dnftitlebook.MaxPerCategory

	// currentTitleBookEquipmentItemSpace 是客户端 op413 里表示「目标＝装备栏」的物品空间号。
	// 实测 2026-09-18：右键「使用」发 space=3（+目标槽 13），「拿出来」发 space=0（+期望背包格）。
	currentTitleBookEquipmentItemSpace int32 = 3
)

// currentTitleBookEntry is one title in the title book. The current EXE reads
// the 22-byte fixed projection followed by u32 raw_len and raw_len bytes.
type currentTitleBookEntry struct {
	SlotIndex      uint16
	ItemID         int32
	Value          int32
	Attr           byte
	Durability     uint16
	SealFlag       byte
	EnchantIndex   int32
	EnchantUpgrade byte
	AmplifyType    byte
	AmplifyValue   uint16
}

func currentTitleBookSlotKey(category int32, index int32) string {
	slot := int16(category)*currentTitleBookSlotBase + int16(index)
	return fmt.Sprintf("%d:%d", currentTitleBookListType, slot)
}

func currentTitleBookCategoryFromSlot(slot int16) int32 {
	return int32(slot / currentTitleBookSlotBase)
}

func currentTitleBookIndexFromSlot(slot int16) int32 {
	return int32(slot % currentTitleBookSlotBase)
}

// handleCurrentTitleBookPut processes CMD 0x019C (412): put a title from
// inventory/equipment into the title book.
func (s *Service) handleCurrentTitleBookPut(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if len(body) < 20 {
		s.logGameEvent(session, "game-title-book-put-blocked", "body_len", len(body), "reason", "body_too_short")
		return nil
	}
	itemSpace := int32(binary.LittleEndian.Uint32(body[0:4]))
	slot := int16(int32(binary.LittleEndian.Uint32(body[4:8])))
	itemID := int32(binary.LittleEndian.Uint32(body[8:12]))
	category := int32(binary.LittleEndian.Uint32(body[12:16]))
	bookIndex := int32(binary.LittleEndian.Uint32(body[16:20]))

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil
	}
	owner, err := dnftitlebook.NewOwner(repos)
	if err != nil {
		return nil
	}
	result, err := owner.Put(ctx, dnftitlebook.PutCommand{
		SelectedCharacterID: session.selectedCharacterID,
		ItemSpace:           itemSpace,
		SourceSlot:          slot,
		ItemID:              itemID,
		Category:            category,
		BookIndex:           bookIndex,
	})
	switch {
	case errors.Is(err, dnftitlebook.ErrInventoryNotFound):
		s.logGameEvent(session, "game-title-book-put-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "inventory_not_found")
		return s.sendTitleBookFailure(session, currentTitleBookPutMsgID, 1, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrSourceNotFound):
		s.logGameEvent(session, "game-title-book-put-blocked",
			"char_id", session.selectedCharacterID,
			"source_list", byte(itemSpace),
			"source_slot", slot,
			"item_id", itemID,
			"reason", "source_not_found")
		return s.sendTitleBookFailure(session, currentTitleBookPutMsgID, 2, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrCategoryFull):
		s.logGameEvent(session, "game-title-book-put-blocked",
			"char_id", session.selectedCharacterID,
			"category", category,
			"reason", "category_full")
		return s.sendTitleBookFailure(session, currentTitleBookPutMsgID, 3, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrCategoryInvalid):
		return s.sendTitleBookFailure(session, currentTitleBookPutMsgID, 4, itemSpace, category)
	case err != nil:
		s.logGameEvent(session, "game-title-book-put-failed",
			"char_id", session.selectedCharacterID,
			"reason", err)
		return nil
	}

	s.logGameEvent(session, "game-title-book-put-success",
		"char_id", result.CharacterID,
		"item_id", result.ItemID,
		"category", result.Category,
		"book_index", result.BookIndex,
		"source_list", result.SourceList,
		"source_slot", result.SourceSlot,
		"target_slot", result.TargetSlot)

	if err := s.sendTitleBookPutAck(
		session,
		result.ItemSpace,
		result.SourceSlot,
		result.Category,
		result.BookIndex,
	); err != nil {
		return err
	}
	var emptyEntry currentItemListEntry
	emptyEntry.patchCore(result.SourceSlot, 0xFFFFFFFF, 0)
	updateBody := buildCurrentItemUpdateBody(result.SourceList, []currentItemListEntry{emptyEntry})
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), updateBody, 0); err != nil {
		return err
	}
	return s.sendCurrentTitleBookList(session, ctx, repos, result.CharacterID, result.Category)
}

// handleCurrentTitleBookGet processes CMD 0x019D (413): take a title from
// the title book back to inventory.
func (s *Service) handleCurrentTitleBookGet(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if len(body) < 20 {
		s.logGameEvent(session, "game-title-book-get-blocked", "body_len", len(body), "reason", "body_too_short")
		return nil
	}
	itemSpace := int32(binary.LittleEndian.Uint32(body[0:4]))
	slot := int16(int32(binary.LittleEndian.Uint32(body[4:8])))
	itemID := int32(binary.LittleEndian.Uint32(body[8:12]))
	category := int32(binary.LittleEndian.Uint32(body[12:16]))
	bookIndex := int32(binary.LittleEndian.Uint32(body[16:20]))

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil
	}
	owner, err := dnftitlebook.NewOwner(repos)
	if err != nil {
		return nil
	}
	cmd := dnftitlebook.GetCommand{
		SelectedCharacterID: session.selectedCharacterID,
		ItemSpace:           itemSpace,
		SourceSlot:          slot,
		PreferredSlot:       slot,
		ItemID:              itemID,
		Category:            category,
		BookIndex:           bookIndex,
	}
	// 【2026-09-18 修复：按客户端给的目标空间落位】客户端在 op413 里明确给出目标物品
	// 空间并**按回包里的 (space, slot) 摆放物品**（实测两条请求：space=3 是右键「使用」
	// → 穿身上；space=0 是「拿出来」→ 回背包）。早期实现忽略了这个字段、一律穿上，
	// 于是「拿出来」时玩家看到背包里一个副本，小退重登却变成自动穿戴（服务端真实状态）。
	if itemSpace == currentTitleBookEquipmentItemSpace {
		result, equipErr := owner.Equip(ctx, cmd)
		if equipErr != nil {
			return s.handleCurrentTitleBookGetError(session, equipErr, itemSpace, category, bookIndex)
		}
		return s.sendCurrentTitleBookEquipped(session, ctx, repos, result)
	}
	result, getErr := owner.Get(ctx, cmd)
	if getErr != nil {
		return s.handleCurrentTitleBookGetError(session, getErr, itemSpace, category, bookIndex)
	}
	return s.sendCurrentTitleBookTakenOut(session, ctx, repos, result)
}

// handleCurrentTitleBookGetError 把 owner 的错误映射成客户端认识的失败码。
func (s *Service) handleCurrentTitleBookGetError(session *gameSession, err error, itemSpace, category, bookIndex int32) error {
	switch {
	case errors.Is(err, dnftitlebook.ErrInventoryNotFound):
		return s.sendTitleBookFailure(session, currentTitleBookGetMsgID, 1, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrSourceNotFound):
		s.logGameEvent(session, "game-title-book-get-blocked",
			"char_id", session.selectedCharacterID,
			"category", category,
			"book_index", bookIndex,
			"reason", "not_found_in_book")
		return s.sendTitleBookFailure(session, currentTitleBookGetMsgID, 2, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrTargetFull):
		s.logGameEvent(session, "game-title-book-get-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "inventory_full")
		return s.sendTitleBookFailure(session, currentTitleBookGetMsgID, 3, itemSpace, category)
	case errors.Is(err, dnftitlebook.ErrCategoryInvalid):
		return s.sendTitleBookFailure(session, currentTitleBookGetMsgID, 4, itemSpace, category)
	case err != nil:
		s.logGameEvent(session, "game-title-book-get-failed",
			"char_id", session.selectedCharacterID,
			"reason", err)
	}
	return nil
}

// sendCurrentTitleBookEquipped 处理 space=3（右键「使用」）的成功回包。
//
// 【2026-09-18 方案A（闪退修正版）】称号簿右键使用 = 直接装备：
//   - 服务端把称号写进 list-3 称号槽并落库（owner.Equip），登录时随装备回放，
//     所以小退重登后称号还在——修掉「穿戴栏的称号小退就消失」；
//   - 不再发「物品回背包」的 op14 —— 那正是玩家看到的「背包里多一个」；
//   - 换下来的旧称号要回背包，这里补一行 op14（list0 单行，客户端已验证的形态）
//     让客户端立刻看到；
//   - **不再发 sendSelectedCurrentEquipmentSlotItemUpdates**：那是 22 行的 op14 list-3
//     全量刷新，客户端必崩（2026-09-18 23:41:17 客户端 90CN_trace：dispatch
//     opcode=14 packet_len=2637 之后立刻 0xC0000005 eip=0x03B25D03，该地址位于
//     __report_gsfailure 内，WER 同址报 0xC0000409 即栈缓冲溢出；而 list-3 历史上
//     只发过单行、共 88 次，从未出事）。最可能是客户端按行数在栈上开临时区被压爆，
//     但机制未完全坐实——不管机制如何，官方在这条路径上根本不发列表包，对齐官方最稳。
//     官方参考服务端 CTitleBook::getTitle(0x086425C4) 在装备分支末尾只调
//     CUser::send_equip(0x865DD14)，而 send_equip 对槽位 13 直接 return，
//     即官方这里根本不发任何物品列表：客户端拿 413 回包里的 (space=3, slot=13)
//     自己就穿上了。
func (s *Service) sendCurrentTitleBookEquipped(session *gameSession, ctx context.Context, repos dnfrepo.Group, result dnftitlebook.EquipResult) error {
	s.logGameEvent(session, "game-title-book-get-success",
		"char_id", result.CharacterID,
		"item_id", result.ItemID,
		"category", result.Category,
		"book_index", result.BookIndex,
		"equipment_slot", result.EquipmentSlot,
		"replaced", result.Replaced,
		"return_slot", result.ReturnSlot,
		"action", "moved_from_book_to_equipment")

	if err := s.sendTitleBookGetAck(
		session,
		currentTitleBookEquipmentItemSpace,
		int16(result.EquipmentSlot),
		result.Category,
		result.BookIndex,
	); err != nil {
		return err
	}
	if result.Replaced {
		if err := s.sendCurrentTitleBookInventoryRow(session, result.ReturnSlot, result.ReturnItem); err != nil {
			return err
		}
	}
	// 称号已穿上，这一页要用「移除后」的列表（owner.Equip 已把簿内条目删除）。
	return s.sendCurrentTitleBookList(session, ctx, repos, result.CharacterID, result.Category)
}

// sendCurrentTitleBookTakenOut 处理 space=0 等背包目标（「拿出来」）的成功回包。
// 背包里必须有这一行，客户端才会立刻看到；ack 回**真实落点**——空间号统一报背包
// （标题只会落到背包或装备栏；万一客户端给出别的空间号，也不能把背包槽号报成仓库/
// 装扮的空间，否则客户端会按错误的容器摆放物品）。
func (s *Service) sendCurrentTitleBookTakenOut(session *gameSession, ctx context.Context, repos dnfrepo.Group, result dnftitlebook.GetResult) error {
	s.logGameEvent(session, "game-title-book-get-success",
		"char_id", result.CharacterID,
		"item_id", result.ItemID,
		"category", result.Category,
		"book_index", result.BookIndex,
		"target_slot", result.TargetSlot,
		"requested_space", result.ItemSpace,
		"action", "moved_from_book_to_inventory")

	if err := s.sendTitleBookGetAck(
		session,
		int32(dnfrepo.MainInventoryListType),
		result.TargetSlot,
		result.Category,
		result.BookIndex,
	); err != nil {
		return err
	}
	if err := s.sendCurrentTitleBookInventoryRow(session, result.TargetSlot, result.TargetStack); err != nil {
		return err
	}
	return s.sendCurrentTitleBookList(session, ctx, repos, result.CharacterID, result.Category)
}

// sendCurrentTitleBookInventoryRow 发一行 list-0 单行 op14（客户端已验证的形态：
// 历史上 list-0 单行更新发过上千次，多行也一直是同一种 119 字节行）。
func (s *Service) sendCurrentTitleBookInventoryRow(session *gameSession, slot int16, stack dnfrepo.ItemStack) error {
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
	updateBody := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{entry})
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), updateBody, 0)
}

// sendCurrentTitleBookList sends NOTI 0x0166 for one category.
func (s *Service) sendCurrentTitleBookList(session *gameSession, ctx context.Context, repos dnfrepo.Group, characterID string, category int32) error {
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		return nil
	}
	entries := currentTitleBookEntriesForCategory(inventory, category)
	body := buildCurrentTitleBookListBody(session.selectedCharacterID, category, entries)
	return s.sendGameUpperRawClass(session, currentTitleBookMsgID, body, 0)
}

// sendAllCurrentTitleBookLists sends NOTI 0x0166 for all categories that have entries.
func (s *Service) sendAllCurrentTitleBookLists(session *gameSession, ctx context.Context, repos dnfrepo.Group, characterID string) error {
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		return nil
	}
	categories := currentTitleBookCategories(inventory)
	for _, category := range categories {
		entries := currentTitleBookEntriesForCategory(inventory, category)
		body := buildCurrentTitleBookListBody(session.selectedCharacterID, category, entries)
		if err := s.sendGameUpperRawClass(session, currentTitleBookMsgID, body, 0); err != nil {
			return err
		}
	}
	return nil
}

func currentTitleBookCategories(inventory dnfrepo.InventoryRecord) []int32 {
	categorySet := make(map[int32]struct{})
	for key, stack := range inventory.Slots {
		if stack.ItemID <= 0 || dnfachievement.IsLegacyInventoryProgress(key, stack) {
			continue
		}
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != currentTitleBookListType {
			continue
		}
		categorySet[currentTitleBookCategoryFromSlot(slot)] = struct{}{}
	}
	categories := make([]int32, 0, len(categorySet))
	for c := range categorySet {
		categories = append(categories, c)
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i] < categories[j] })
	return categories
}

func currentTitleBookEntriesForCategory(inventory dnfrepo.InventoryRecord, category int32) []currentTitleBookEntry {
	entries := make([]currentTitleBookEntry, 0, 8)
	for key, stack := range inventory.Slots {
		if stack.ItemID <= 0 || dnfachievement.IsLegacyInventoryProgress(key, stack) {
			continue
		}
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != currentTitleBookListType {
			continue
		}
		if currentTitleBookCategoryFromSlot(slot) != category {
			continue
		}
		entries = append(entries, currentTitleBookEntry{
			SlotIndex:  uint16(currentTitleBookIndexFromSlot(slot)),
			ItemID:     int32(stack.ItemID),
			Value:      int32(sceneInventoryStackAmount(stack)),
			Attr:       sceneInventoryExtraByte(stack.Extra, "packed_flag_byte", "packed_flag", "packed", "ext_data0", "ext0"),
			Durability: sceneInventoryExtraUint16(stack.Extra, "value_b_word", "durability", "max_durability"),
			// 封装标记：0 = 已解封，1 = 封装。必须和背包行同源（currentItemListBindFlag），
			// 否则同一件称号在背包显示解封、在簿里显示封装（2026-09-18 玩家报障）。
			// 官方 CTitleBook::_putItemData 写的也是物品自己的 Inven_Item+0 字节，
			// 详见 scene_inventory.go currentRequestOverseerRowsFromInventory 的注释。
			SealFlag:       currentItemListBindFlag(stack.Bind, stack.Extra),
			EnchantIndex:   int32(sceneInventoryExtraUint32(stack.Extra, "value_a", "instance_value", "item_uid", "serial", "count_or_instance")),
			EnchantUpgrade: sceneInventoryExtraByte(stack.Extra, "unused_byte_after_value_a", "unused"),
			AmplifyType:    sceneInventoryExtraByte(stack.Extra, "value_c"),
			AmplifyValue:   sceneInventoryExtraUint16(stack.Extra, "value_d"),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].SlotIndex < entries[j].SlotIndex })
	return entries
}

// buildCurrentTitleBookListBody builds NOTI 0x0166 body.
// Format: u8 info_type + u16 owner_id + i32 category + i32 count + count * 22B entries.
func buildCurrentTitleBookListBody(ownerID uint16, category int32, entries []currentTitleBookEntry) []byte {
	var w packetWriter
	w.writeByte(0) // info_type: 0 = self
	w.writeUint16(ownerID)
	w.writeInt32(int(category))
	w.writeInt32(len(entries))
	for _, e := range entries {
		w.writeUint16(e.SlotIndex)
		w.writeInt32(int(e.ItemID))
		w.writeInt32(int(e.Value))
		w.writeByte(e.Attr)
		w.writeUint16(e.Durability)
		w.writeByte(e.SealFlag)
		w.writeInt32(int(e.EnchantIndex))
		w.writeByte(e.EnchantUpgrade)
		w.writeByte(e.AmplifyType)
		w.writeUint16(e.AmplifyValue)
		w.writeUint32(0)
	}
	return w.bytes()
}

func (s *Service) sendTitleBookPutAck(session *gameSession, itemSpace int32, slot int16, category, bookIndex int32) error {
	var w packetWriter
	w.writeInt32(int(itemSpace))
	w.writeInt32(int(slot))
	w.writeInt32(int(category))
	w.writeInt32(int(bookIndex))
	return s.sendGameUpperSuccess(session, currentTitleBookPutMsgID, w.bytes())
}

func (s *Service) sendTitleBookGetAck(session *gameSession, itemSpace int32, slot int16, category, bookIndex int32) error {
	var w packetWriter
	w.writeInt32(int(itemSpace))
	w.writeInt32(int(slot))
	w.writeInt32(int(category))
	w.writeInt32(int(bookIndex))
	return s.sendGameUpperSuccess(session, currentTitleBookGetMsgID, w.bytes())
}

func (s *Service) sendTitleBookFailure(session *gameSession, msgID uint16, errorCode byte, itemSpace int32, category int32) error {
	var w packetWriter
	w.writeByte(0) // failure
	w.writeByte(errorCode)
	w.writeInt32(int(itemSpace))
	w.writeInt32(int(category))
	return s.sendGameUpperRaw(session, msgID, w.bytes())
}

// isCurrentTitleBookRequest returns true if the opcode is a title book operation
// that should be handled by the dnfbridge title book handler instead of the
// aligned command system.
func isCurrentTitleBookRequest(opcode uint16) bool {
	return opcode == uint16(dnfenum.CmdPacketTitleBookPut) || opcode == uint16(dnfenum.CmdPacketTitleBookGet)
}

// --- Phase 2: Achievement progress (CMD 0x01A1 / 417) ---

const (
	currentAchievementTriggerMsgID uint16 = uint16(dnfenum.CmdPacketAchievementTrigger)
)

// resolveAchievementDefinition joins the titlebook.etc placement mapping with
// the same quest PVF catalog used by normal quest logic.
func (s *Service) resolveAchievementDefinition(ctx context.Context, questID int32) (dnfachievement.Definition, error) {
	mapping := s.currentTitleBookMapping()
	if mapping == nil {
		return dnfachievement.Definition{}, dnfachievement.ErrDefinitionNotFound
	}
	entry, ok := mapping[questID]
	if !ok {
		return dnfachievement.Definition{}, fmt.Errorf("%w: quest=%d", dnfachievement.ErrDefinitionNotFound, questID)
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return dnfachievement.Definition{}, err
	}
	questDefinition, ok := catalog.Find(int64(questID))
	if !ok {
		return dnfachievement.Definition{}, fmt.Errorf("%w: quest=%d pvf", dnfachievement.ErrDefinitionNotFound, questID)
	}
	targets := currentAchievementTargets(questDefinition)
	return dnfachievement.Definition{
		QuestID: questID,
		Target1: targets[0],
		Target2: targets[1],
		Target3: targets[2],
		Reward: dnfachievement.TitleReward{
			ItemID:    entry.titleItemID,
			Category:  entry.category,
			BookIndex: entry.bookIndex,
		},
	}, nil
}

type titleBookMappingEntry struct {
	titleItemID int32
	category    int32
	bookIndex   int32
	rewardCount int32
}

// currentTitleBookMapping returns the cached quest_id → title mapping from titlebook.etc.
func (s *Service) currentTitleBookMapping() map[int32]titleBookMappingEntry {
	s.titleBookMappingMu.Lock()
	defer s.titleBookMappingMu.Unlock()
	if s.titleBookMappingCache != nil {
		return s.titleBookMappingCache
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil
	}
	text, err := archive.ReadText("etc/titlebook.etc")
	if err != nil {
		s.logPacketEvent("game-titlebook-etc-read-failed", "error", err)
		return nil
	}
	mapping := parseTitleBookEtc(text)
	s.titleBookMappingCache = mapping
	s.logPacketEvent("game-titlebook-mapping-loaded", "entries", len(mapping))
	return mapping
}

// parseTitleBookEtc parses the current PVF's repeated
// [title collection info] sections. Each section starts with category name and
// max slots, then uses: book_index, marker, quest_id, reward_count, title_id.
// marker -1 denotes an empty slot and therefore has no following triple.
func parseTitleBookEtc(text string) map[int32]titleBookMappingEntry {
	mapping := make(map[int32]titleBookMappingEntry, 256)
	document, err := dnfpvf.Parse("etc/titlebook.etc", text)
	if err != nil {
		return mapping
	}
	category := int32(0)
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "title collection info") {
			continue
		}
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start >= section.End {
			category++
			continue
		}
		tokens := document.Tokens[section.Start:section.End]
		cursor := 2 // category name + max slots
		for cursor+1 < len(tokens) {
			if tokens[cursor].Kind != dnfpvf.TokenInt || tokens[cursor+1].Kind != dnfpvf.TokenInt {
				cursor++
				continue
			}
			bookIndex := tokens[cursor].Int
			marker := tokens[cursor+1].Int
			cursor += 2
			if marker == -1 {
				continue
			}
			if cursor+2 >= len(tokens) ||
				tokens[cursor].Kind != dnfpvf.TokenInt ||
				tokens[cursor+1].Kind != dnfpvf.TokenInt ||
				tokens[cursor+2].Kind != dnfpvf.TokenInt {
				break
			}
			questID := tokens[cursor].Int
			rewardCount := tokens[cursor+1].Int
			titleItemID := tokens[cursor+2].Int
			cursor += 3
			if questID <= 0 || questID > int64(^uint32(0)>>1) ||
				bookIndex < 0 || bookIndex >= int64(currentTitleBookMaxPerCategory) ||
				titleItemID <= 0 || titleItemID > int64(^uint32(0)>>1) {
				continue
			}
			mapping[int32(questID)] = titleBookMappingEntry{
				titleItemID: int32(titleItemID),
				category:    category,
				bookIndex:   int32(bookIndex),
				rewardCount: int32(rewardCount),
			}
		}
		category++
	}
	return mapping
}

func currentAchievementTargets(definition dnfquest.Definition) [3]uint16 {
	var targets [3]uint16
	for index, value := range definition.CheckCount {
		if index >= len(targets) {
			break
		}
		targets[index] = currentAchievementTargetValue(value)
	}
	if targets[0] != 0 || targets[1] != 0 || targets[2] != 0 {
		return targets
	}
	if strings.EqualFold(strings.TrimSpace(definition.Type), "[seeking]") ||
		strings.EqualFold(strings.TrimSpace(definition.Type), "seeking") {
		for index := 0; index < len(targets) && index*2+1 < len(definition.IntData); index++ {
			targets[index] = currentAchievementTargetValue(definition.IntData[index*2+1])
		}
		if targets[0] != 0 || targets[1] != 0 || targets[2] != 0 {
			return targets
		}
	}
	targets[0] = 1
	return targets
}

func currentAchievementTargetValue(value int64) uint16 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}
