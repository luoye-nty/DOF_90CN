package titlebook

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	ListType       byte  = 100
	SlotBase       int16 = 1000
	MaxPerCategory       = 200

	// TitleEquipmentSlot 是当前客户端 list-3（穿戴栏）里的称号槽位号。
	//
	// 依据 2026-09-18 玩家实测：从背包正常穿戴称号时客户端发的是
	// op19 MOVE_ITEMSPACE「源 list0:11 → 目标 list3:13」（packet_log 15:36:07），
	// 服务端按该槽位落库后小退重登依然在。称号簿的「右键使用」客户端只会本地穿上
	// （同一次操作里没有再发任何装备请求），所以服务端必须自己落到这个槽位。
	TitleEquipmentSlot int16 = 13
)

var (
	ErrOwnerUnavailable  = errors.New("title book owner unavailable")
	ErrCharacterRequired = errors.New("selected character id required")
	ErrInventoryNotFound = errors.New("title book inventory not found")
	ErrSourceNotFound    = errors.New("title book source item not found")
	ErrCategoryInvalid   = errors.New("title book category or index is invalid")
	ErrCategoryFull      = errors.New("title book category is full")
	ErrTargetFull        = errors.New("title book target inventory is full")
	ErrEquipmentNotFound = errors.New("title book character equipment not found")
)

// EquipResult 是「从称号簿直接使用（装备）」的结果。
// 称号按官方语义从簿里移出（见 Equip 注释），所以调用方要刷新称号簿页。
type EquipResult struct {
	CharacterID    string
	ItemSpace      int32
	ItemID         int32
	Category       int32
	BookIndex      int32
	EquipmentSlot  int16
	EquipmentEntry dnfrepo.EquipmentEntry
	// Replaced 为真表示原来穿着另一个称号，它被放回背包 ReturnSlot。
	Replaced   bool
	ReturnSlot int16
	ReturnItem dnfrepo.ItemStack
}

type PutCommand struct {
	SelectedCharacterID uint16
	ItemSpace           int32
	SourceSlot          int16
	ItemID              int32
	Category            int32
	BookIndex           int32
	UpdatedAt           time.Time
}

type PutResult struct {
	CharacterID string
	ItemSpace   int32
	SourceList  byte
	SourceSlot  int16
	ItemID      int32
	Category    int32
	BookIndex   int32
	TargetSlot  int16
}

type GetCommand struct {
	SelectedCharacterID uint16
	ItemSpace           int32
	SourceSlot          int16
	// PreferredSlot 是客户端在 op413 里指定的背包落点：客户端会按回包里的槽位把物品
	// 画在那个格子上，所以能放下就必须用这个槽（放不下再退回第一个空格）。
	PreferredSlot int16
	ItemID        int32
	Category      int32
	BookIndex     int32
	UpdatedAt     time.Time
}

type GetResult struct {
	CharacterID string
	ItemSpace   int32
	SourceSlot  int16
	ItemID      int32
	Category    int32
	BookIndex   int32
	TargetSlot  int16
	TargetStack dnfrepo.ItemStack
}

type Owner struct {
	settlements dnfrepo.CharacterSettlementUnitOfWork
}

func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.Inventory == nil || repos.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{settlements: repos.CharacterSettlement}, nil
}

func (o *Owner) Put(ctx context.Context, cmd PutCommand) (PutResult, error) {
	if o == nil || o.settlements == nil {
		return PutResult{}, ErrOwnerUnavailable
	}
	if err := validateBookLocation(cmd.Category, cmd.BookIndex); err != nil {
		return PutResult{}, err
	}
	characterID, err := selectedCharacterKey(cmd.SelectedCharacterID)
	if err != nil {
		return PutResult{}, err
	}
	var result PutResult
	err = o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		inventory, loadErr := loadInventory(ctx, tx.Inventory, characterID)
		if loadErr != nil {
			return loadErr
		}
		sourceList := byte(cmd.ItemSpace)
		sourceKey := inventoryKey(sourceList, cmd.SourceSlot)
		sourceStack, found := inventory.Slots[sourceKey]
		if !found || sourceStack.ItemID != int64(cmd.ItemID) || sourceStack.ItemID <= 0 {
			return fmt.Errorf(
				"%w: list=%d slot=%d item=%d",
				ErrSourceNotFound,
				sourceList,
				cmd.SourceSlot,
				cmd.ItemID,
			)
		}
		targetSlot, bookIndex, found := firstBookSlot(inventory.Slots, cmd.Category, cmd.BookIndex)
		if !found {
			return fmt.Errorf("%w: category=%d", ErrCategoryFull, cmd.Category)
		}

		delete(inventory.Slots, sourceKey)
		titleStack := sourceStack
		titleStack.Count = 1
		if titleStack.Extra == nil {
			titleStack.Extra = make(map[string]string, 4)
		}
		titleStack.Extra["title_book_category"] = strconv.FormatInt(int64(cmd.Category), 10)
		titleStack.Extra["title_book_index"] = strconv.FormatInt(int64(bookIndex), 10)
		titleStack.Extra["source"] = "title_book_put"
		inventory.Slots[inventoryKey(ListType, targetSlot)] = titleStack
		inventory.UpdatedAt = mutationTime(cmd.UpdatedAt)
		if saveErr := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		result = PutResult{
			CharacterID: characterID,
			ItemSpace:   cmd.ItemSpace,
			SourceList:  sourceList,
			SourceSlot:  cmd.SourceSlot,
			ItemID:      cmd.ItemID,
			Category:    cmd.Category,
			BookIndex:   bookIndex,
			TargetSlot:  targetSlot,
		}
		return nil
	})
	return result, err
}

func (o *Owner) Get(ctx context.Context, cmd GetCommand) (GetResult, error) {
	if o == nil || o.settlements == nil {
		return GetResult{}, ErrOwnerUnavailable
	}
	if err := validateBookLocation(cmd.Category, cmd.BookIndex); err != nil {
		return GetResult{}, err
	}
	characterID, err := selectedCharacterKey(cmd.SelectedCharacterID)
	if err != nil {
		return GetResult{}, err
	}
	var result GetResult
	err = o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		inventory, loadErr := loadInventory(ctx, tx.Inventory, characterID)
		if loadErr != nil {
			return loadErr
		}
		sourceKey := BookSlotKey(cmd.Category, cmd.BookIndex)
		titleStack, found := inventory.Slots[sourceKey]
		if !found || titleStack.ItemID <= 0 ||
			(cmd.ItemID > 0 && titleStack.ItemID != int64(cmd.ItemID)) {
			return fmt.Errorf(
				"%w: category=%d index=%d item=%d",
				ErrSourceNotFound,
				cmd.Category,
				cmd.BookIndex,
				cmd.ItemID,
			)
		}
		targetSlot, found := preferredMainInventorySlot(inventory.Slots, cmd.PreferredSlot)
		if !found {
			return ErrTargetFull
		}

		delete(inventory.Slots, sourceKey)
		targetStack := titleStack
		targetStack.Count = 1
		if targetStack.Extra == nil {
			targetStack.Extra = make(map[string]string, 2)
		}
		targetStack.Extra["source"] = "title_book_get"
		inventory.Slots[inventoryKey(dnfrepo.MainInventoryListType, targetSlot)] = targetStack
		inventory.UpdatedAt = mutationTime(cmd.UpdatedAt)
		if saveErr := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		result = GetResult{
			CharacterID: characterID,
			ItemSpace:   cmd.ItemSpace,
			SourceSlot:  cmd.SourceSlot,
			ItemID:      int32(titleStack.ItemID),
			Category:    cmd.Category,
			BookIndex:   cmd.BookIndex,
			TargetSlot:  targetSlot,
			TargetStack: targetStack,
		}
		return nil
	})
	return result, err
}

func BookSlotKey(category, index int32) string {
	return inventoryKey(ListType, int16(category)*SlotBase+int16(index))
}

// Equip 实现「称号簿右键直接使用」：把簿里登记的称号**穿到身上**并落库，
// 而不是像 Get 那样塞回背包。
//
// 背景（2026-09-18 玩家报障）：客户端在 op413 之后会自己把称号穿上，但服务端
// 只把物品放回背包，于是出现「背包一个、穿戴栏一个」，而穿戴栏那个因为服务端
// 从未落库，小退重登就消失。这里改为：直接写 list-3 称号槽（槽位号见
// TitleEquipmentSlot），登录时随装备一起回放，所以重登后仍在。
//
// 称号簿条目按官方语义**移动**（穿到身上就不再留在簿里），不是复制。依据官方
// 参考服务端 df_game_r(1)：CTitleBook::getTitle(0x086425C4) 在目标物品空间是
// 装备（space=3）时依次调用 CInventory::tryInsertItemIntoEquipment(0x8501AFA)
// → CTitleBook::_deleteTitle(0x8641D50) → CInventory::insertItemIntoEquipment
// (0x8502344)，也就是「先穿上、再把条目从簿里删掉」，最后只调
// CUser::send_equip(0x865DD14)——而 send_equip 对槽位 13（称号）直接 return
// （它只广播槽位 0..0xB 与 0x16 的外观变化）。所以客户端是凭 413 回包里的
// (space=3, slot=13) 自己完成本地穿戴的，服务端不需要也不应该再补发物品列表；
// 条目留在簿里只会让玩家同时看到「簿里一个、身上一个」。
//
// 若身上已经穿着**另一个**称号，则把它放回背包第一个空位（不这么做就会凭空
// 丢东西），并把这件事报给调用方用于刷新。
func (o *Owner) Equip(ctx context.Context, cmd GetCommand) (EquipResult, error) {
	if o == nil || o.settlements == nil {
		return EquipResult{}, ErrOwnerUnavailable
	}
	if err := validateBookLocation(cmd.Category, cmd.BookIndex); err != nil {
		return EquipResult{}, err
	}
	characterID, err := selectedCharacterKey(cmd.SelectedCharacterID)
	if err != nil {
		return EquipResult{}, err
	}
	var result EquipResult
	err = o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		inventory, loadErr := loadInventory(ctx, tx.Inventory, characterID)
		if loadErr != nil {
			return loadErr
		}
		sourceKey := BookSlotKey(cmd.Category, cmd.BookIndex)
		titleStack, found := inventory.Slots[sourceKey]
		if !found || titleStack.ItemID <= 0 ||
			(cmd.ItemID > 0 && titleStack.ItemID != int64(cmd.ItemID)) {
			return fmt.Errorf(
				"%w: category=%d index=%d item=%d",
				ErrSourceNotFound,
				cmd.Category,
				cmd.BookIndex,
				cmd.ItemID,
			)
		}
		equipment, equipFound, loadErr := tx.Equipment.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !equipFound {
			equipment = dnfrepo.EquipmentRecord{CharacterID: characterID}
		}
		if equipment.Entries == nil {
			equipment.Entries = make(map[string]dnfrepo.EquipmentEntry, 1)
		}
		slotKey := strconv.FormatInt(int64(TitleEquipmentSlot), 10)

		// 移动语义：称号离开称号簿，同一件东西不会同时存在于簿里和身上。
		delete(inventory.Slots, sourceKey)

		previous, occupied := equipment.Entries[slotKey]
		if occupied && previous.ItemID != 0 && previous.ItemID != titleStack.ItemID {
			returnSlot, slotFound := firstMainInventorySlot(inventory.Slots)
			if !slotFound {
				return ErrTargetFull
			}
			returned := dnfrepo.ItemStack{
				ItemID:   previous.ItemID,
				Count:    1,
				Bind:     previous.Bind,
				ExpireAt: previous.ExpireAt,
				RawEntry: rewriteEquipmentRowSlot(previous.RawEntry, returnSlot),
				Extra:    cloneTitleBookExtra(previous.Extra),
			}
			if returned.Extra == nil {
				returned.Extra = make(map[string]string, 2)
			}
			delete(returned.Extra, "equipment_slot")
			delete(returned.Extra, "equipped_slot")
			returned.Extra["source"] = "title_book_equip_replaced"
			inventory.Slots[inventoryKey(dnfrepo.MainInventoryListType, returnSlot)] = returned
			result.Replaced = true
			result.ReturnSlot = returnSlot
			result.ReturnItem = returned
		}

		// 【2026-09-28 修复：礼白666 型卡死】achievement.grantTitle 把称号写进称号簿时
		// **不写 RawEntry**。这里如果原样把空 raw 搬进装备行，装备行的 RawEntry 就是空的，
		// 而 equip 的 unequip() 开头就有一道 `len(source.RawEntry) == 0 →
		// ErrMoveRawEntryMissing`，于是这个称号从此摘不下来（玩家报障：称号展示栏卡了
		// 一个成就称号）。簿里没有行数据时，按称号的构造规则补一行 43 字节的。
		raw := rewriteEquipmentRowSlot(titleStack.RawEntry, TitleEquipmentSlot)
		// 2026-09-28 线上回退：补行本身没被证伪，但它和上面那三行一起上线时出现了回归，
		// 先关掉保持行为与改动前一致。
		if false && len(raw) == 0 && titleStack.ItemID > 0 && titleStack.ItemID <= int64(^uint32(0)) {
			raw = EquipmentRawEntry(titleStack.ItemID, TitleEquipmentSlot)
		}
		extra := cloneTitleBookExtra(titleStack.Extra)
		if extra == nil {
			extra = make(map[string]string, 8)
		}
		extra["equipment_slot"] = slotKey
		extra["equipped_slot"] = slotKey
		extra["item_kind"] = "equipment"
		extra["source"] = "title_book_equip"
		extra["title_book_category"] = strconv.FormatInt(int64(cmd.Category), 10)
		extra["title_book_index"] = strconv.FormatInt(int64(cmd.BookIndex), 10)
		entry := dnfrepo.EquipmentEntry{
			SlotIndex: TitleEquipmentSlot,
			ItemID:    titleStack.ItemID,
			Bind:      titleStack.Bind,
			ExpireAt:  titleStack.ExpireAt,
			RawEntry:  raw,
			Extra:     extra,
		}
		equipment.Entries[slotKey] = entry
		equipment.UpdatedAt = mutationTime(cmd.UpdatedAt)

		// 称号簿条目被删除（或旧称号被换回背包）都属于背包变更，必须落库。
		inventory.UpdatedAt = mutationTime(cmd.UpdatedAt)
		if saveErr := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		if saveErr := dnfrepo.SaveEquipmentFields(ctx, tx.Equipment, equipment, dnfrepo.EquipmentFieldEntries); saveErr != nil {
			return saveErr
		}
		result.CharacterID = characterID
		result.ItemSpace = cmd.ItemSpace
		result.ItemID = int32(titleStack.ItemID)
		result.Category = cmd.Category
		result.BookIndex = cmd.BookIndex
		result.EquipmentSlot = TitleEquipmentSlot
		result.EquipmentEntry = entry
		return nil
	})
	return result, err
}

// rewriteEquipmentRowSlot 把一行 0x77 物品行的槽位字段改成目标槽，并返回副本。
// 称号在背包里存的是 list-0 行，穿到 list-3 后行首的槽位号必须跟着变，客户端才
// 会把它画在穿戴栏的称号格上（实测落库行首是 0D00 = 13）。
func rewriteEquipmentRowSlot(raw []byte, slot int16) []byte {
	out := append([]byte(nil), raw...)
	if len(out) >= 2 {
		out[0] = byte(slot)
		out[1] = byte(uint16(slot) >> 8)
	}
	return out
}

// EquipmentRawEntry 按当前客户端的 43 字节装备行构造规则造一行称号装备数据：
// 字节 0 是角色装备槽，+1 是 PVF 物品号，+5 是构造器要求的 create 标量 1，其余为 0。
//
// 用途：achievement.grantTitle 入簿时不写 RawEntry，穿到装备栏时若不补行，装备行
// 就会是空的，equip 的 unequip() 会以 ErrMoveRawEntryMissing 把它锁死（玩家报障
// "称号摘不下来"）。equip 包的 buildCurrentTitleEquipmentRawEntry 现在直接转调这里，
// 两份实现不会再漂移。
func EquipmentRawEntry(itemID int64, slot int16) []byte {
	raw := make([]byte, 43)
	raw[0] = byte(slot)
	binary.LittleEndian.PutUint32(raw[1:5], uint32(itemID))
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	return raw
}

func cloneTitleBookExtra(extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(extra)+4)
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func firstBookSlot(items map[string]dnfrepo.ItemStack, category, preferred int32) (int16, int32, bool) {
	if preferred >= 0 && preferred < MaxPerCategory {
		slot := int16(category)*SlotBase + int16(preferred)
		if _, occupied := items[inventoryKey(ListType, slot)]; !occupied {
			return slot, preferred, true
		}
	}
	for index := int32(0); index < MaxPerCategory; index++ {
		slot := int16(category)*SlotBase + int16(index)
		if _, occupied := items[inventoryKey(ListType, slot)]; !occupied {
			return slot, index, true
		}
	}
	return -1, -1, false
}

func firstMainInventorySlot(items map[string]dnfrepo.ItemStack) (int16, bool) {
	for slot := int16(9); slot <= 64; slot++ {
		if _, occupied := items[inventoryKey(dnfrepo.MainInventoryListType, slot)]; !occupied {
			return slot, true
		}
	}
	return -1, false
}

// preferredMainInventorySlot 优先用客户端指定的落点，占用时退回第一个空格。
//
// 依据 2026-09-18 玩家报障：称号从簿里「拿出来」时客户端会发
// op413 (space=0, slot=<期望格子>)，并且**按回包里的槽位**把物品画在背包那个格子上。
// 旧实现忽略这个槽位、ack 还回错了空间（把装备槽 13 报给背包请求），于是玩家看到
// 「背包里有一个」，但服务端实际把它穿上了，小退重登就变成「自动穿戴」。
func preferredMainInventorySlot(items map[string]dnfrepo.ItemStack, preferred int16) (int16, bool) {
	if preferred >= 9 && preferred <= 64 {
		if _, occupied := items[inventoryKey(dnfrepo.MainInventoryListType, preferred)]; !occupied {
			return preferred, true
		}
	}
	return firstMainInventorySlot(items)
}

func inventoryKey(listType byte, slot int16) string {
	return fmt.Sprintf("%d:%d", listType, slot)
}

func mutationTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func selectedCharacterKey(selectedCharacterID uint16) (string, error) {
	if selectedCharacterID == 0 {
		return "", ErrCharacterRequired
	}
	return strconv.FormatUint(uint64(selectedCharacterID), 10), nil
}

func loadInventory(ctx context.Context, repository dnfrepo.InventoryRepository, characterID string) (dnfrepo.InventoryRecord, error) {
	if repository == nil {
		return dnfrepo.InventoryRecord{}, ErrOwnerUnavailable
	}
	inventory, found, err := repository.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.InventoryRecord{}, err
	}
	if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
		return dnfrepo.InventoryRecord{}, fmt.Errorf("%w: character=%s", ErrInventoryNotFound, characterID)
	}
	inventory = dnfrepo.CloneInventory(inventory)
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	return inventory, nil
}

func validateBookLocation(category, index int32) error {
	if category < 0 || category >= 5 || index < 0 || index >= MaxPerCategory {
		return fmt.Errorf("%w: category=%d index=%d", ErrCategoryInvalid, category, index)
	}
	return nil
}
