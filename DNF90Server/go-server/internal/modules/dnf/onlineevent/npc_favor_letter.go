package onlineevent

// 「充满爱慕的信」这类 `[action type] [use favor point] N` 道具：op812 选一个 NPC，
// 直接给该 NPC 加 N 点好感度（消耗道具本身），与"送礼"共用 op812 与同一套好感度存储。
//
// 与送礼的区别（2026-10-16 线上抓包确认）：
//   - 送礼：消耗配置数量的材料，要求玩家在该 NPC 所在区域，每 NPC 每天限次；
//   - 信件：消耗 1 个道具，只按道具自己的 `[npc select category]` 白名单校验 NPC，
//     不要求位置、不计每日送礼次数。
// 好感度上限沿用 PVF 的 `etc/npcfavorsystem.etc` + NPC 文档 `[favor level point]`。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrNPCFavorItem = errors.New("NPC favor item not usable")
	ErrNPCFavorCap  = errors.New("NPC favor cap unavailable")
)

// LoadNPCFavorCap 取某个 NPC 的好感度上限：`npc/npc.lst` → 该 NPC 文档的
// `[favor level point]` 第三个数字（与 LoadReturneeGiftRules 同一口径）。
func LoadNPCFavorCap(source dnfpvf.Source, npc uint32) (int64, error) {
	if source == nil || npc == 0 {
		return 0, ErrNPCFavorCap
	}
	read := func(p string) (*dnfpvf.Document, error) {
		s, e := source.ReadText(p)
		if e != nil {
			return nil, e
		}
		return dnfpvf.Parse(p, s)
	}
	list, err := read("npc/npc.lst")
	if err != nil {
		return 0, err
	}
	for _, entry := range dnfpvf.ParseList(list) {
		if entry.ID != int64(npc) {
			continue
		}
		if path.IsAbs(entry.Path) || path.Clean(entry.Path) == ".." {
			return 0, fmt.Errorf("invalid NPC path")
		}
		doc, e := read(path.Join("npc", entry.Path))
		if e != nil {
			return 0, e
		}
		if _, ok := doc.Section("favorable relationship 2"); !ok {
			break
		}
		values, ok := doc.Section("favor level point")
		if !ok || len(values) != 3 || values[2].Int <= 0 || values[2].Int > math.MaxInt32 {
			break
		}
		return values[2].Int, nil
	}
	return 0, fmt.Errorf("%w: npc=%d", ErrNPCFavorCap, npc)
}

// NPCFavorItemResult 是一次信件使用的写库结果。
// GrantNPCFavorPoints 只加好感度、不动道具：券在 op515「使用」那一步已经扣掉了
// （见 inventory/favor_letter_action.go 的说明），op812 落地时只补点数。
func GrantNPCFavorPoints(ctx context.Context, repos dnfrepo.Group, maxPoints int64, npc uint32, characterID string, points int64, now time.Time) (NPCFavorItemResult, error) {
	var result NPCFavorItemResult
	if repos.Character == nil || npc == 0 || points <= 0 || maxPoints <= 0 {
		return result, ErrNPCFavorItem
	}
	if now.IsZero() {
		now = time.Now()
	}
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil {
		return result, err
	}
	if !found {
		return result, ErrNPCFavorItem
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = map[string]int64{}
	}
	prefix := "npc_favor_" + strconv.FormatUint(uint64(npc), 10) + "_"
	before := min(max(character.Stats[prefix+"points"], 0), maxPoints)
	after := min(before+points, maxPoints)
	character.Stats[prefix+"points"] = after
	character.UpdatedAt = now
	if err := dnfrepo.SaveCharacterFields(ctx, repos.Character, character, dnfrepo.CharacterFieldStats); err != nil {
		return result, err
	}
	return NPCFavorItemResult{Points: after, Added: after - before}, nil
}

type NPCFavorItemResult struct {
	Points    int64
	Added     int64
	Remaining int64
	Stack     dnfrepo.ItemStack
}

// GiveNPCFavorItem 消耗 (container, slot) 里的 1 个道具，给 npc 加 points 点好感度。
// container=36 表示账号共享仓库（结晶/灵魂仓库那一段），=0 表示角色背包。
func GiveNPCFavorItem(ctx context.Context, repos dnfrepo.Group, maxPoints int64, npc uint32, accountID, characterID string, container byte, slot int16, points int64, now time.Time) (NPCFavorItemResult, error) {
	var result NPCFavorItemResult
	if repos.AccountAssets == nil || npc == 0 || points <= 0 || maxPoints <= 0 || now.IsZero() {
		return result, ErrNPCFavorItem
	}
	warehouse := slot >= dnfrepo.CrystalWarehouseFirstSlot && slot <= dnfrepo.SoulWarehouseLastSlot
	if (container != 0 && container != 36) || slot < 0 || (container == 36 && !warehouse) || (!warehouse && slot >= dnfrepo.CrystalWarehouseFirstSlot) {
		return result, ErrNPCFavorItem
	}
	e := repos.AccountAssets.WithinAccountCharacterAssets(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, items dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		ch, ok, e := characters.Load(ctx, characterID)
		if e != nil {
			return e
		}
		if !ok || ch.AccountID != accountID {
			return fmt.Errorf("NPC favor character requirement")
		}
		var account dnfrepo.AccountInventoryRecord
		var inventory dnfrepo.InventoryRecord
		var slots map[string]dnfrepo.ItemStack
		var key string
		if warehouse {
			account, _, e = accounts.Load(ctx, accountID)
			account = dnfrepo.CloneAccountInventory(account)
			slots = account.Slots
			key = dnfrepo.AccountSharedInventorySlotKey(slot)
		} else {
			inventory, _, e = items.Load(ctx, characterID)
			inventory = dnfrepo.CloneInventory(inventory)
			slots = inventory.Slots
			key = dnfrepo.InventorySlotKey(dnfrepo.MainInventoryListType, slot)
		}
		if e != nil {
			return e
		}
		stack, ok := slots[key]
		if !ok || stack.ItemID <= 0 || stack.Count < 1 {
			return ErrNPCFavorItem
		}
		prefix := "npc_favor_" + strconv.FormatUint(uint64(npc), 10) + "_"
		ch = dnfrepo.CloneCharacter(ch)
		if ch.Stats == nil {
			ch.Stats = map[string]int64{}
		}
		before := min(max(ch.Stats[prefix+"points"], 0), maxPoints)
		after := min(before+points, maxPoints)
		ch.Stats[prefix+"points"] = after
		stack.Count--
		stack.RawEntry = nil
		if stack.Count <= 0 {
			delete(slots, key)
		} else {
			slots[key] = stack
		}
		ch.UpdatedAt = now
		if warehouse {
			account.UpdatedAt = now
			e = accounts.Save(ctx, account)
		} else {
			inventory.UpdatedAt = now
			e = items.Save(ctx, inventory)
		}
		if e != nil {
			return e
		}
		if e := dnfrepo.SaveCharacterFields(ctx, characters, ch, dnfrepo.CharacterFieldStats); e != nil {
			return e
		}
		result = NPCFavorItemResult{Points: after, Added: after - before, Remaining: max(stack.Count, 0), Stack: stack}
		return nil
	})
	return result, e
}
