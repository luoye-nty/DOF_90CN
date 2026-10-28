package inventory

// op515 的 `[action type] [use favor point]` 动作（充满爱慕的信等）。
//
// 2026-10-16 线上抓包：客户端"使用信件"走 op515 且动作号是 **157**，服务端当时回
// "use stackable action index 157 unsupported"（op515 error 17），于是客户端既不扣道具、
// 也不放行后续 —— 表现就是"弹窗不关、什么都没发生"。选中 NPC 的确认是随后的 op812
// （选中 NPC 的信息在那一条里），所以这里只登记"待落地"，扣道具/加好感度交给 op812
// （见 dnfbridge/returnee_npc_gift.go 与 onlineevent/npc_favor_letter.go）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// favorLetterActionIndex 是客户端 op515 里 `[use favor point]` 的动作号（抓包确认）。
const favorLetterActionIndex uint32 = 157

// 待落地状态写在角色 stats 上（与好感度同一份存储）：op812 带回 NPC 后消费。
const (
	favorLetterPendingItemKey = "npc_favor_letter_item"
	favorLetterPendingSlotKey = "npc_favor_letter_slot"
	favorLetterPendingListKey = "npc_favor_letter_list"
	favorLetterPendingAtKey   = "npc_favor_letter_at"
	favorLetterPendingTTL     = 10 * time.Minute
)

var ErrFavorLetterContract = errors.New("NPC favor letter contract violation")

// ArmFavorLetter 登记一封"已使用、等待选择 NPC"的好感度信件。
// 返回的 RemainingCount 是这一格当前数量（客户端据此刷新显示）。
func (o *Owner) ArmFavorLetter(ctx context.Context, cmd Command) (UseStackableResult, error) {
	if o == nil || o.repo == nil || o.characters == nil || cmd.SelectedCharacterID == 0 ||
		cmd.ActionIndex != favorLetterActionIndex || cmd.SourceSlotIndex < 0 {
		return UseStackableResult{}, ErrFavorLetterContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	inventory, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	character, characterFound, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found || !characterFound {
		return UseStackableResult{}, ErrFavorLetterContract
	}
	key := dnfrepo.InventorySlotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	stack, ok := inventory.Slots[key]
	if !ok || stack.ItemID <= 0 || stack.Count < 1 {
		return UseStackableResult{}, ErrFavorLetterContract
	}
	inventory = dnfrepo.CloneInventory(inventory)
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = map[string]int64{}
	}
	character.Stats[favorLetterPendingItemKey] = stack.ItemID
	character.Stats[favorLetterPendingSlotKey] = int64(cmd.SourceSlotIndex)
	character.Stats[favorLetterPendingListKey] = int64(cmd.SourceListType)
	character.Stats[favorLetterPendingAtKey] = time.Now().UTC().Unix()
	// 线上抓包（2026-10-16 19:01）：只登记"待落地"、数量不变时，客户端虽然播了使用动效，
	// 但**不会**发后续的 op812（选完 NPC 点确认没有任何包），弹窗也就一直挂着。
	// 也就是说"使用"这一步必须真的把券扣掉（客户端据此才继续走选 NPC）。加好感度仍留在
	// op812 落地（那一步带回选中的 NPC）。
	stack.Count--
	stack.RawEntry = nil
	if stack.Count <= 0 {
		delete(inventory.Slots, key)
	} else {
		inventory.Slots[key] = stack
	}
	inventory = dnfrepo.CloneInventory(inventory)
	if err := o.repo.Save(ctx, inventory); err != nil {
		return UseStackableResult{}, err
	}
	if err := dnfrepo.SaveCharacterFields(ctx, o.characters, character, dnfrepo.CharacterFieldStats); err != nil {
		return UseStackableResult{}, err
	}
	return UseStackableResult{
		ItemID:         stack.ItemID,
		SlotIndex:      cmd.SourceSlotIndex,
		ListType:       cmd.SourceListType,
		RemainingCount: max(stack.Count, 0),
	}, nil
}

// PendingFavorLetter 是 op812 侧读取到的"待落地信件"。
type PendingFavorLetter struct {
	ItemID    int64
	SlotIndex int16
	ListType  byte
}

// LoadPendingFavorLetter 读取角色上待落地的信件状态；没有或已过期返回 ok=false。
func LoadPendingFavorLetter(character dnfrepo.CharacterRecord, now time.Time) (PendingFavorLetter, bool) {
	if character.Stats == nil {
		return PendingFavorLetter{}, false
	}
	itemID := character.Stats[favorLetterPendingItemKey]
	slot := character.Stats[favorLetterPendingSlotKey]
	list := character.Stats[favorLetterPendingListKey]
	armedAt := character.Stats[favorLetterPendingAtKey]
	if itemID <= 0 || slot < 0 || armedAt <= 0 {
		return PendingFavorLetter{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.UTC().Sub(time.Unix(armedAt, 0)) > favorLetterPendingTTL {
		return PendingFavorLetter{}, false
	}
	return PendingFavorLetter{ItemID: itemID, SlotIndex: int16(slot), ListType: byte(list)}, true
}

// ClearPendingFavorLetter 清掉待落地状态（落地成功或作废时调用）。
func ClearPendingFavorLetter(character *dnfrepo.CharacterRecord) {
	if character == nil || character.Stats == nil {
		return
	}
	delete(character.Stats, favorLetterPendingItemKey)
	delete(character.Stats, favorLetterPendingSlotKey)
	delete(character.Stats, favorLetterPendingListKey)
	delete(character.Stats, favorLetterPendingAtKey)
}

// FavorLetterActionIndex 对外暴露动作号，供 wire 层断言/日志使用。
func FavorLetterActionIndex() uint32 { return favorLetterActionIndex }

// ownerAppliedFavorLetterArmResult 是 op515-157 的成功回执：形状与其它动作分支一致
// （`buildUseStackableActionSuccessAck`），客户端拿到它才会继续走"选 NPC → op812"。
func ownerAppliedFavorLetterArmResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"%s: NPC favor letter armed (item=%d list=%d slot=%d remaining=%d); op812 carries the chosen NPC",
			cmd.String(), result.ItemID, result.ListType, result.SlotIndex, result.RemainingCount),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
	}
}
