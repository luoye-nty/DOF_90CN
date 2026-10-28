package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrental "longheng.io/server/internal/modules/dnf/rental"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentRentalPointMetadataKey = dnfrental.PointMetadataKey
	currentRentalItemSource       = "pvf_rental"
	currentRentalDuration         = 24 * time.Hour
)

var (
	errCurrentRentalOwnerMismatch = dnfrental.ErrOwnerMismatch
	errCurrentRentalStateMissing  = dnfrental.ErrStateMissing
	errCurrentRentalPoints        = dnfrental.ErrPointsInsufficient
	errCurrentRentalGold          = dnfrental.ErrGoldInsufficient
	errCurrentRentalLimit         = dnfrental.ErrPointLimit
	errCurrentRentalInventoryFull = errors.New("dnf rental equipment inventory is full")
)

type currentRentalAssetResult struct {
	Points   uint32
	Gold     int64
	ExpireAt time.Time
	Slot     int16
	Equipped bool
}

// currentRentalItemAttributes are the per-tier item attributes the rental
// catalog declares and that every granted row has to carry. Both are written
// into the durable stack's Extra, so they survive unequip/re-equip and the
// repository round trip instead of living only on the row we happen to send.
type currentRentalItemAttributes struct {
	Reinforce int64
	Forge     int64
}

func cleanupExpiredCurrentRentalEquipment(
	ctx context.Context,
	owner *dnfrental.Owner,
	accountID string,
	characterID string,
	now time.Time,
) (int, error) {
	if owner == nil {
		return 0, dnfrepo.ErrRentalAssetTransactionUnavailable
	}
	removed := 0
	err := owner.Cleanup(ctx, dnfrental.CleanupCommand{
		AccountID:   accountID,
		CharacterID: characterID,
		UpdatedAt:   now,
		Project: func(assets *dnfrental.Assets) (dnfrental.Changes, error) {
			inventoryDirty := false
			for key, stack := range assets.Inventory.Slots {
				if !currentRentalItemExpired(stack.Extra, stack.ExpireAt, now) {
					continue
				}
				delete(assets.Inventory.Slots, key)
				inventoryDirty = true
				removed++
			}
			equipmentDirty := false
			for key, entry := range assets.Equipment.Entries {
				if !currentRentalItemExpired(entry.Extra, entry.ExpireAt, now) {
					continue
				}
				delete(assets.Equipment.Entries, key)
				equipmentDirty = true
				removed++
			}
			return dnfrental.Changes{Inventory: inventoryDirty, Equipment: equipmentDirty}, nil
		},
	})
	if err != nil {
		return 0, currentRentalMutationError(err)
	}
	return removed, nil
}

func purchaseCurrentRentalPoints(
	ctx context.Context,
	owner *dnfrental.Owner,
	accountID string,
	characterID string,
	count uint32,
	limit uint32,
	goldPerPoint int64,
	now time.Time,
) (currentRentalAssetResult, error) {
	if owner == nil {
		return currentRentalAssetResult{}, dnfrepo.ErrRentalAssetTransactionUnavailable
	}
	result, err := owner.Charge(ctx, dnfrental.ChargeCommand{
		AccountID:    accountID,
		CharacterID:  characterID,
		Count:        count,
		Limit:        limit,
		GoldPerPoint: goldPerPoint,
		UpdatedAt:    now,
	})
	if err != nil {
		return currentRentalAssetResult{}, currentRentalMutationError(err)
	}
	return currentRentalAssetResult{Points: result.Points, Gold: result.Gold}, nil
}

func rentCurrentEquipment(
	ctx context.Context,
	owner *dnfrental.Owner,
	accountID string,
	characterID string,
	itemID uint32,
	pointCost uint32,
	definition dungeonDropItemDefinition,
	attributes currentRentalItemAttributes,
	now time.Time,
) (currentRentalAssetResult, error) {
	if owner == nil || itemID == 0 || pointCost == 0 || definition.ItemID != itemID ||
		definition.Kind != dungeonDropItemEquipment || definition.Durability == 0 {
		return currentRentalAssetResult{}, dnfrepo.ErrRentalAssetTransactionUnavailable
	}
	expireAt := now.UTC().Add(currentRentalDuration)
	result := currentRentalAssetResult{ExpireAt: expireAt}
	wallet, err := owner.Rent(ctx, dnfrental.RentCommand{
		AccountID:   accountID,
		CharacterID: characterID,
		PointCost:   pointCost,
		UpdatedAt:   now,
		Project: func(assets *dnfrental.Assets) (dnfrental.Changes, error) {
			inventory := assets.Inventory
			equipment := assets.Equipment
			inventoryDirty, equipmentDirty := false, false
			foundExisting := false
			for key, stack := range inventory.Slots {
				listType, slot, ok := parseSceneInventorySlotKey(key)
				if !ok || listType != 0 || slot < definition.SlotStart || slot > definition.SlotEnd ||
					stack.ItemID != int64(itemID) ||
					!strings.EqualFold(strings.TrimSpace(stack.Extra["source"]), currentRentalItemSource) {
					continue
				}
				stack = currentRentalStack(stack, itemID, definition, attributes, expireAt)
				inventory.Slots[key] = stack
				inventoryDirty = true
				foundExisting = true
				result.Slot = slot
			}
			for key, entry := range equipment.Entries {
				if entry.ItemID != int64(itemID) ||
					!strings.EqualFold(strings.TrimSpace(entry.Extra["source"]), currentRentalItemSource) {
					continue
				}
				entry.ExpireAt = expireAt
				entry.Extra = currentRentalExtra(entry.Extra, definition, attributes, expireAt)
				equipment.Entries[key] = entry
				equipmentDirty = true
				foundExisting = true
				result.Slot = entry.SlotIndex
				result.Equipped = true
			}
			if !foundExisting {
				slot, ok := firstCurrentRentalInventorySlot(inventory.Slots, definition.SlotStart, definition.SlotEnd)
				if !ok {
					return dnfrental.Changes{}, errCurrentRentalInventoryFull
				}
				stack := currentRentalStack(dnfrepo.ItemStack{}, itemID, definition, attributes, expireAt)
				inventory.Slots[fmt.Sprintf("0:%d", slot)] = stack
				inventoryDirty = true
				result.Slot = slot
			}
			return dnfrental.Changes{Inventory: inventoryDirty, Equipment: equipmentDirty}, nil
		},
	})
	if err != nil {
		return currentRentalAssetResult{}, currentRentalMutationError(err)
	}
	result.Points = wallet.Points
	result.Gold = wallet.Gold
	return result, nil
}

func currentRentalPoints(account dnfrepo.AccountRecord) (uint32, error) {
	return dnfrental.Points(account)
}

func currentRentalStack(stack dnfrepo.ItemStack, itemID uint32, definition dungeonDropItemDefinition, attributes currentRentalItemAttributes, expireAt time.Time) dnfrepo.ItemStack {
	stack.ItemID = int64(itemID)
	stack.Count = 1
	stack.Bind = false
	stack.ExpireAt = expireAt
	stack.Extra = currentRentalExtra(stack.Extra, definition, attributes, expireAt)
	entry := currentItemListEntryFromStack(0, 0, stack)
	// 续租是拿上一行的 RawEntry 当底再重投影的，而 setByte 对 0 是 no-op（那是
	// 有意为之的全局规则：让服务端不拥有的字节透传）。结果是本档把锻造降成 0 时，
	// 上一行的旧锻造字节会原样留下。租赁行的 +0x0A 与 +0x57 完全由目录拥有
	// （PVF 把 `upgrade`/`separate upgrade` 列为不可操作），所以在这里显式写死，
	// 只影响租赁行，不动共享的 setByte 语义。
	entry.data[0x0A] = byte(attributes.Reinforce)
	entry.data[currentItemListForgingLevelOffset] = byte(attributes.Forge)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	return stack
}

func currentRentalExtra(extra map[string]string, definition dungeonDropItemDefinition, attributes currentRentalItemAttributes, expireAt time.Time) map[string]string {
	out := make(map[string]string, len(extra)+10)
	for key, value := range extra {
		out[key] = value
	}
	out["source"] = currentRentalItemSource
	out["item_kind"] = string(dungeonDropItemEquipment)
	out["pvf_path"] = definition.PVFPath
	out["durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
	out["max_durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
	out["expire_time"] = strconv.FormatInt(expireAt.Unix(), 10)
	out["rental_duration_hours"] = "24"
	// 强化/锻造只能在这里写入：租赁武器自己的 PVF 把 `upgrade` 与
	// `separate upgrade` 都列进 `[impossible contents]`，玩家没有任何途径事后补上，
	// 所以发放行一旦漏掉，这把武器就永远是 +0。
	//
	// 写入的键沿用仓库既有约定（gm_admin.go / gm_tool.go 同样成对写）：
	//   ext_data0       —— 背包行 +0x0A 与穿戴行 +0x0A 都读它，只写它是够的；
	//   reinforce       —— 同一等级的规范别名，穿戴侧读取顺序里排在最后；
	//   forging_level   —— +0x57 锻造字节，背包行与穿戴行同键。
	// 三个键的取值都是十进制等级本身，没有 <<1 之类的打包（那是 mode0 角色外观
	// 包 sub_20026C0 的另一种编码，不要混用）。
	reinforce := strconv.FormatInt(attributes.Reinforce, 10)
	out["reinforce"] = reinforce
	out["ext_data0"] = reinforce
	out["forging_level"] = strconv.FormatInt(attributes.Forge, 10)
	// 品级：不写这个键，列表行投影就解析不出种子（itemquality.StackSeed 返回 0），
	// 于是 +0x06 落到数量回退值 1，客户端按种子 1 重画成「最下级」——正是现场看到的
	// 「预览最上级 90%、到手最下级 13%」。这是背包行与穿戴行唯一的品级来源键，
	// 写在这一处即可覆盖首租/续租/穿戴续租三条路径。
	out["quality_seed"] = strconv.FormatUint(uint64(currentRentalQualitySeed), 10)
	return out
}

func firstCurrentRentalInventorySlot(slots map[string]dnfrepo.ItemStack, first, last int16) (int16, bool) {
	occupied := make(map[int16]struct{}, len(slots))
	for key := range slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == 0 {
			occupied[slot] = struct{}{}
		}
	}
	for slot := first; slot <= last; slot++ {
		if _, used := occupied[slot]; !used {
			return slot, true
		}
	}
	return 0, false
}

func currentRentalItemExpired(extra map[string]string, expireAt time.Time, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(extra["source"]), currentRentalItemSource) {
		return false
	}
	if raw := strings.TrimSpace(extra["expire_time"]); raw != "" {
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > 0 {
			return unix <= now.Unix()
		}
	}
	return !expireAt.IsZero() && !expireAt.After(now)
}
