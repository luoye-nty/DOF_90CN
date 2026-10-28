package dungeon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

type PickupItemKind string

const (
	PickupItemStackable PickupItemKind = "stackable"
	PickupItemEquipment PickupItemKind = "equipment"
)

var (
	ErrPickupItemInvalid   = errors.New("dungeon pickup item is invalid")
	ErrPickupInventoryFull = errors.New("dungeon pickup inventory category is full")
	ErrPickupStackLimit    = errors.New("dungeon pickup exceeds stack limit")
	ErrInventoryNotFound   = errors.New("dungeon inventory is not found")
)

type PickupItemDefinition struct {
	ItemID          uint32
	Kind            PickupItemKind
	StackLimit      int64
	SlotStart       int16
	SlotEnd         int16
	PreferQuickSlot bool
}

type PickupItemPlacement struct {
	Definition PickupItemDefinition
	Amount     uint32
	// ListType is the inventory list the pickup is placed in. Zero keeps the
	// ordinary main bag, which is what every drop used before guild medals
	// ([flag]) and guardian gems ([flag gem]) needed list 38: the client can
	// only wear a medal or embed a gem from that list, so a pickup written to
	// list 0 sat in the bag unusable until the login migration moved it.
	ListType          byte
	BuildNew          func(int16) (dnfrepo.ItemStack, error)
	NormalizeExisting func(dnfrepo.ItemStack) (dnfrepo.ItemStack, error)
	// ReservedSlots 是本容器里"还空着、但已经许给别人"的格位，开新行时必须跳过。
	//
	// 目前唯一的来源是副本内丢弃：弃置物躺在地上期间必须能回到它被丢下的那一格
	// （discard origin 记着 source slot），而装备不能并进已占用的行。一旦有新拾取
	// 物按"最小空格优先"占掉那一格，那件弃置物就再也捡不起来，会在副本里一直留到
	// 本局结束（2026-09-27 实测：新拾取占了 43 格之后，地面对象 504 永久无法拾取）。
	// 归还原格位这条规则本身不动，这里只是把"已许诺"的格位从新行候选里排除。
	// nil 表示没有任何保留，除副本拾取以外的调用方都用 nil。
	ReservedSlots map[int16]struct{}
}

// minimumPickupSlot is the lowest slot a pickup may occupy on a given list.
//
// On the main bag slot 0 is the gold wallet's pseudo row, never a real item, so
// a page starting at 0 there is malformed. List 38 has no such row: the guild
// medal page legitimately starts at slot 0, and rejecting it made every medal
// pickup fail with "dungeon pickup item is invalid".
func minimumPickupSlot(listType byte) int16 {
	if listType == dnfrepo.MainInventoryListType {
		return 1
	}
	return 0
}

// pickupSlotKey is the repository key for one placed pickup. It exists so the
// placer is not hard-wired to the main bag; mainSlotKey stays the zero-value
// behaviour.
func pickupSlotKey(listType byte, slot uint16) string {
	if listType == 0 {
		return mainSlotKey(slot)
	}
	return strconv.FormatUint(uint64(listType), 10) + ":" + strconv.FormatUint(uint64(slot), 10)
}

type PickupItemCommand struct {
	CharacterID string
	Placement   PickupItemPlacement
	UpdatedAt   time.Time
	Finalize    func(int16, dnfrepo.ItemStack) (dnfrepo.ItemStack, error)
}

type PickupItemResult struct {
	Slot  uint16
	Stack dnfrepo.ItemStack
}

// AccountSharedPickupItemCommand places one stack in an account-owned list-0
// slot. The current client renders these fixed slots in the crystal/soul bars
// beside the character backpack even though their durable owner is the account.
type AccountSharedPickupItemCommand struct {
	AccountID   string
	CharacterID string
	SharedSlot  int16
	Placement   PickupItemPlacement
	UpdatedAt   time.Time
	Finalize    func(int16, dnfrepo.ItemStack) (dnfrepo.ItemStack, error)
}

func (o *Owner) GrantPickupItem(ctx context.Context, cmd PickupItemCommand) (PickupItemResult, error) {
	if o == nil || o.items == nil || strings.TrimSpace(cmd.CharacterID) == "" {
		return PickupItemResult{}, ErrOwnerUnavailable
	}
	ctx = contextOrBackground(ctx)
	now := updatedAtOrNow(cmd.UpdatedAt)

	var result PickupItemResult
	err := dnfrepo.WithinCharacterItemsScoped(ctx, o.items, cmd.CharacterID, func(
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		record, found, err := inventories.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		slot, stack, err := PlacePickupItem(&record, cmd.Placement)
		if err != nil {
			return err
		}
		if cmd.Finalize != nil {
			stack, err = cmd.Finalize(int16(slot), stack)
			if err != nil {
				return err
			}
			record.Slots[pickupSlotKey(cmd.Placement.ListType, slot)] = cloneStack(stack)
		}
		record.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, record, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		result = PickupItemResult{Slot: slot, Stack: cloneStack(stack)}
		return nil
	})
	if err != nil {
		return PickupItemResult{}, err
	}
	return result, nil
}

// GrantAccountSharedPickupItem grants the complete pickup to its fixed shared
// slot when possible. A full or conflicting legacy slot falls back as a whole
// to the ordinary character backpack so one pickup has one client destination.
func (o *Owner) GrantAccountSharedPickupItem(ctx context.Context, cmd AccountSharedPickupItemCommand) (PickupItemResult, error) {
	accountID := strings.TrimSpace(cmd.AccountID)
	characterID := strings.TrimSpace(cmd.CharacterID)
	if o == nil || o.accountAssets == nil || accountID == "" || characterID == "" ||
		!dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, cmd.SharedSlot) {
		return PickupItemResult{}, ErrOwnerUnavailable
	}
	if err := validateAccountSharedPickupPlacement(cmd.Placement); err != nil {
		return PickupItemResult{}, err
	}
	ctx = contextOrBackground(ctx)
	now := updatedAtOrNow(cmd.UpdatedAt)

	var result PickupItemResult
	err := o.accountAssets.WithinAccountCharacterAssets(ctx, accountID, characterID, func(
		accounts dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.AccountID) != accountID {
			return ErrCharacterNotFound
		}

		account, found, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found {
			account = dnfrepo.AccountInventoryRecord{AccountID: accountID}
		} else {
			account = dnfrepo.CloneAccountInventory(account)
		}
		if account.Slots == nil {
			account.Slots = make(map[string]dnfrepo.ItemStack)
		}

		key := dnfrepo.AccountSharedInventorySlotKey(cmd.SharedSlot)
		stack, occupied := account.Slots[key]
		amount := int64(cmd.Placement.Amount)
		itemID := int64(cmd.Placement.Definition.ItemID)
		// 账号共享晶石/灵魂格属于账号金库这一个容器：额度按容器里同 id 总量算，
		// 该道具已经落在别的共享格时也不再开第二行（整包退回角色背包那条既有回退）。
		space := dnfrepo.ContainerItemSpace(
			account.Slots,
			dnfrepo.MainInventoryListType,
			itemID,
			cmd.Placement.Definition.StackLimit,
		)
		canUseShared := !occupied || stack.ItemID == itemID
		if occupied && (stack.Count < 0 || cmd.Placement.Definition.StackLimit > 0 && amount > space) {
			canUseShared = false
		}
		if rowKey, _, exists := dnfrepo.ContainerItemRow(account.Slots, dnfrepo.MainInventoryListType, itemID); exists && rowKey != key {
			canUseShared = false
		}
		if canUseShared {
			if !occupied {
				stack, err = cmd.Placement.BuildNew(cmd.SharedSlot)
				if err != nil {
					return err
				}
			} else {
				if cmd.Placement.NormalizeExisting != nil {
					stack, err = cmd.Placement.NormalizeExisting(stack)
					if err != nil {
						return err
					}
				}
				stack.Count += amount
			}
			if stack.ItemID != int64(cmd.Placement.Definition.ItemID) || stack.Count <= 0 {
				return ErrPickupItemInvalid
			}
			if cmd.Finalize != nil {
				stack, err = cmd.Finalize(cmd.SharedSlot, stack)
				if err != nil {
					return err
				}
			}
			account.Slots[key] = cloneStack(stack)
			account.UpdatedAt = now
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			result = PickupItemResult{Slot: uint16(cmd.SharedSlot), Stack: cloneStack(stack)}
			return nil
		}

		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		slot, fallback, err := PlacePickupItem(&inventory, cmd.Placement)
		if err != nil {
			return err
		}
		if cmd.Finalize != nil {
			fallback, err = cmd.Finalize(int16(slot), fallback)
			if err != nil {
				return err
			}
			inventory.Slots[mainSlotKey(slot)] = cloneStack(fallback)
		}
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		result = PickupItemResult{Slot: slot, Stack: cloneStack(fallback)}
		return nil
	})
	if err != nil {
		return PickupItemResult{}, err
	}
	return result, nil
}

func validateAccountSharedPickupPlacement(placement PickupItemPlacement) error {
	definition := placement.Definition
	if placement.Amount == 0 || definition.ItemID == 0 || definition.Kind != PickupItemStackable ||
		definition.SlotStart <= 0 || definition.SlotEnd < definition.SlotStart || placement.BuildNew == nil {
		return ErrPickupItemInvalid
	}
	if definition.StackLimit > 0 && int64(placement.Amount) > definition.StackLimit {
		return ErrPickupStackLimit
	}
	return nil
}

// PlacePickupItem owns the reusable dungeon/PVF placement policy. Current
// client raw-entry construction is supplied by BuildNew/Finalize.
func PlacePickupItem(record *dnfrepo.InventoryRecord, placement PickupItemPlacement) (uint16, dnfrepo.ItemStack, error) {
	definition := placement.Definition
	if record == nil || placement.Amount == 0 || definition.ItemID == 0 ||
		definition.SlotStart < minimumPickupSlot(placement.ListType) ||
		definition.SlotEnd < definition.SlotStart {
		return 0, dnfrepo.ItemStack{}, ErrPickupItemInvalid
	}
	if definition.Kind != PickupItemStackable && definition.Kind != PickupItemEquipment {
		return 0, dnfrepo.ItemStack{}, ErrPickupItemInvalid
	}
	if definition.Kind == PickupItemEquipment && placement.Amount != 1 {
		return 0, dnfrepo.ItemStack{}, ErrPickupItemInvalid
	}
	if definition.Kind == PickupItemStackable && definition.StackLimit > 0 &&
		int64(placement.Amount) > definition.StackLimit {
		return 0, dnfrepo.ItemStack{}, fmt.Errorf(
			"%w: item=%d amount=%d limit=%d",
			ErrPickupStackLimit,
			definition.ItemID,
			placement.Amount,
			definition.StackLimit,
		)
	}
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	// 保留格位只可能出现在主背包（每一个 discard origin 都是 list 0），而行键按
	// 列表分开，所以别的列表不需要、也不应该看这份保留集。
	reserved := placement.ReservedSlots
	if placement.ListType != dnfrepo.MainInventoryListType {
		reserved = nil
	}
	// Revive coins have one home: the fixed main-list slot-1 wallet. Placing
	// them in an ordinary bag row leaves the client showing two separate
	// counters -- the wallet's old total and a loose consumable stack whose
	//右键 path stalls -- until the next character select runs the wallet
	// consolidation. This is the chokepoint for dungeon drops, quest turn-in,
	// attendance and event rewards, and the epic book, all of which reach the
	// bag through here.
	if dnfrevivecoin.IsReward(int64(definition.ItemID)) {
		_, stack, err := dnfrevivecoin.GrantSlots(
			record.Slots, int64(placement.Amount), "revive_coin_pickup")
		if err != nil {
			return 0, dnfrepo.ItemStack{}, err
		}
		return uint16(dnfrevivecoin.WalletSlot), stack, nil
	}
	// PVF [stack limit] 的"每容器总量"口径本轮先只覆盖邮件/商城/NPC 商店/礼包；
	// 拾取与贩卖机共用本落点助手（贩卖机没有溢出邮件兜底，超额会被整笔拒绝），
	// 所以这里暂按旧的按行语义，等给贩卖机接上邮件溢出后再一起打开。
	newRowStackLimit := int64(0)
	space := int64(math.MaxInt32)
	// canOpenRow 是本地这一侧的落点闸门：PVF [stack limit] 已经给这个物品配了上限、
	// 且容器里已经没有可开的新行时，不允许再新开一行。
	// 【参数 listType 来自本次合并】他们让掉落可以落到非主列表（公会勋章 → list 38），
	// 所以这里必须按落点自己的列表判定，否则主背包的开行策略会错误地影响 list 38。
	canOpenRow := func(listType byte) bool {
		return space > 0 && dnfrepo.CanOpenContainerItemRow(
			record.Slots,
			listType,
			int64(definition.ItemID),
			newRowStackLimit,
		)
	}
	// 注意：这里刻意不做"并入容器里任意位置的同 id 行"。材料/消耗品的落点按 PVF 物品页
	// 决定（快捷栏那一条只对 PreferQuickSlot 的消耗品生效），跨页强行合并会把材料拖进
	// 快捷栏的堆里，与 AddCurrentDungeonPickupKeepsMaterialsInPVFCategorySlots 的约定冲突。
	if definition.Kind == PickupItemStackable && definition.PreferQuickSlot {
		if slot, stack, ok, err := stackPickupInRange(record, placement, 3, 8, space); err != nil {
			return 0, dnfrepo.ItemStack{}, err
		} else if ok {
			return slot, stack, nil
		}
		// Quick slots and the PVF category are ranges of the same main
		// inventory. Merge every existing stack before allocating another
		// quick-slot stack so one pickup cannot split an already bagged item.
		if slot, stack, ok, err := stackPickupInRange(
			record,
			placement,
			definition.SlotStart,
			definition.SlotEnd,
			space,
		); err != nil {
			return 0, dnfrepo.ItemStack{}, err
		} else if ok {
			return slot, stack, nil
		}
		// 【合并说明 2026-09-18，来自服务端仓库 fefab3b】
		// 他们给 firstEmptyMainSlot 增加了 listType 参数（公会勋章要落进 list 38，
		// 不再一律进主背包），我们这一侧在落点前加了 canOpenRow() 这道 PVF 开行闸门。
		// 两边意图都要保留：空位搜索认 placement.ListType，闸门也按同一个列表判定；
		// 否则主背包"不允许再开新行"时会错误地挡掉 list 38 的勋章落点。
		if slot, ok := firstEmptyMainSlot(record, placement.ListType, 3, 8, reserved); ok && canOpenRow(placement.ListType) {
			return insertPickup(record, placement, slot)
		}
	}
	if slot, stack, ok, err := stackPickupInRange(
		record,
		placement,
		definition.SlotStart,
		definition.SlotEnd,
		space,
	); err != nil {
		return 0, dnfrepo.ItemStack{}, err
	} else if ok {
		return slot, stack, nil
	}
	// 同上：非快捷栏落点同样认 placement.ListType，并保留我们的开行闸门。
	if slot, ok := firstEmptyMainSlot(record, placement.ListType, definition.SlotStart, definition.SlotEnd, reserved); ok && canOpenRow(placement.ListType) {
		return insertPickup(record, placement, slot)
	}
	return 0, dnfrepo.ItemStack{}, fmt.Errorf(
		"%w: item=%d range=%d..%d",
		ErrPickupInventoryFull,
		definition.ItemID,
		definition.SlotStart,
		definition.SlotEnd,
	)
}

func stackPickupInRange(
	record *dnfrepo.InventoryRecord,
	placement PickupItemPlacement,
	start int16,
	end int16,
	space int64,
) (uint16, dnfrepo.ItemStack, bool, error) {
	definition := placement.Definition
	if definition.Kind != PickupItemStackable {
		return 0, dnfrepo.ItemStack{}, false, nil
	}
	for slot := start; slot <= end; slot++ {
		key := pickupSlotKey(placement.ListType, uint16(slot))
		stack, exists := record.Slots[key]
		if !exists || stack.ItemID != int64(definition.ItemID) || stack.Bind || stack.Count < 0 {
			continue
		}
		if placement.NormalizeExisting != nil {
			var err error
			stack, err = placement.NormalizeExisting(stack)
			if err != nil {
				return 0, dnfrepo.ItemStack{}, false, err
			}
		}
		amount := int64(placement.Amount)
		// 额度是"这个容器里该道具总量"的余量，不是一个格子的余量。
		if stack.Count > math.MaxInt64-amount || amount > space ||
			(definition.StackLimit > 0 && stack.Count > definition.StackLimit-amount) {
			continue
		}
		stack.Count += amount
		record.Slots[key] = stack
		return uint16(slot), cloneStack(stack), true, nil
	}
	return 0, dnfrepo.ItemStack{}, false, nil
}

func firstEmptyMainSlot(record *dnfrepo.InventoryRecord, listType byte, start int16, end int16, reserved map[int16]struct{}) (int16, bool) {
	for slot := start; slot <= end; slot++ {
		if _, promised := reserved[slot]; promised {
			continue
		}
		if _, occupied := record.Slots[pickupSlotKey(listType, uint16(slot))]; !occupied {
			return slot, true
		}
	}
	return 0, false
}

func insertPickup(
	record *dnfrepo.InventoryRecord,
	placement PickupItemPlacement,
	slot int16,
) (uint16, dnfrepo.ItemStack, error) {
	if placement.BuildNew == nil {
		return 0, dnfrepo.ItemStack{}, ErrPickupItemInvalid
	}
	stack, err := placement.BuildNew(slot)
	if err != nil {
		return 0, dnfrepo.ItemStack{}, err
	}
	if stack.ItemID != int64(placement.Definition.ItemID) ||
		stack.Count != int64(placement.Amount) {
		return 0, dnfrepo.ItemStack{}, ErrPickupItemInvalid
	}
	record.Slots[pickupSlotKey(placement.ListType, uint16(slot))] = cloneStack(stack)
	return uint16(slot), cloneStack(stack), nil
}
