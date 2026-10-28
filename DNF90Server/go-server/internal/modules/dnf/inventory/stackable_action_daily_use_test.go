package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// op515 的每个 action 都必须按道具**自己的** PVF `[total usable count]` 计数。
// 改动前只有 action 58（疲劳药）在自己的分支里判上限，入场券 / 租赁点 / 伤害字体 /
// 任务完成券这四类声明了上限的道具既不被拒绝、也从不产生 op541 计数。
//
// 这里用 action 167（任务完成券）代表这四类：预留发生在**分派之前**，与具体 action
// 无关，所以任何一个 action 都覆盖得到。
func TestUseStackableActionChargesTheItemsOwnPVFDailyLimit(t *testing.T) {
	ctx := context.Background()
	const itemID = int64(10164630)
	const limit = int64(1)

	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {ItemID: itemID, Count: 3, Extra: map[string]string{"pvf_path": "stackable/10164001/10164630.stk"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}

	use := func() alignedcmd.Result {
		t.Helper()
		body := make([]byte, 19)
		binary.LittleEndian.PutUint16(body[0:2], 10)
		body[2] = listTypeMain
		binary.LittleEndian.PutUint32(body[7:11], questCompletionTicketActionIndex)
		result, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
			Body:                body,
			AccountID:           "acc",
			SelectedCharacterID: 77,
			Repositories:        repositories,
			StackableUseResolver: func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
				return alignedcmd.StackableUseResolution{
					ItemID: resolvedID, PVFPath: "stackable/10164001/10164630.stk", StackableType: "[etc]",
					// PVF [total usable count]：这件券每天只能用 1 次。
					DailyUseLimit: limit,
					QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{
						ActionType: "[gm quest clear]", MinimumLevel: 90, MaximumLevel: 90, VillageOnly: true,
					},
				}, nil
			},
			QuestCompletionTicketPlanner: func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, _ alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
				record = dnfrepo.CloneQuest(record)
				if record.States == nil {
					record.States = make(map[int64]dnfrepo.QuestState)
				}
				record.States[3145] = dnfrepo.QuestState{Status: "completed", UpdatedAt: completedAt}
				record.UpdatedAt = completedAt
				return alignedcmd.QuestCompletionTicketPlan{
					Quest: record, Character: character,
					QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, CompletedQuestIDs: []int64{3145},
				}, nil
			},
		})
		if err != nil {
			t.Fatalf("Handle error = %v", err)
		}
		return result
	}

	// 第一次：动作落地，并且必须把"今天已用 1 次"投影给客户端（客户端按它画
	// 「每天可用次数」，也按它自己拦第二次点击）。
	applied := use()
	if len(applied.UpperResponses) != 1 || len(applied.DailyUseStates) != 1 ||
		applied.DailyUseStates[0].ItemID != uint32(itemID) || applied.DailyUseStates[0].Used != 1 {
		t.Fatalf("第一次使用没有按 PVF 上限计数并投影: %+v", applied)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if inventory.Slots["0:10"].Count != 2 {
		t.Fatalf("道具没有被消耗: %+v", inventory.Slots)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	if got := dnfitemdailylimit.Used(character.Stats, itemID); got != 1 {
		t.Fatalf("每日计数=%d want 1 stats=%+v", got, character.Stats)
	}

	// 第二次：到上限，必须拒绝，且不消耗道具、不改计数、不多发 op541。
	rejected := use()
	if len(rejected.UpperResponses) != 1 ||
		!bytes.Equal(rejected.UpperResponses[0].Body, []byte{0, useStackableActionFailureCode}) {
		t.Fatalf("到上限后仍被放行: %+v", rejected)
	}
	if len(rejected.DailyUseStates) != 0 {
		t.Fatalf("被拒的一次不得投影计数: %+v", rejected.DailyUseStates)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "77")
	if inventory.Slots["0:10"].Count != 2 {
		t.Fatalf("被拒的一次消耗了道具: %+v", inventory.Slots)
	}
	character, _, _ = repositories.Character.Load(ctx, "77")
	if got := dnfitemdailylimit.Used(character.Stats, itemID); got != limit {
		t.Fatalf("被拒的一次改动了计数: %d stats=%+v", got, character.Stats)
	}
}

// 没有 `[total usable count]` 的道具必须与挂载之前**逐字节相同**：不预留、不写计数、
// 不多发一个包。这是"纯增量"的证明，也是这次改动不伤既有功能的核心保证。
func TestUseStackableActionWithoutDeclaredLimitStaysUntouched(t *testing.T) {
	ctx := context.Background()
	const itemID = int64(10164630)

	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {ItemID: itemID, Count: 2, Extra: map[string]string{"pvf_path": "stackable/10164001/10164630.stk"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 10)
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[7:11], questCompletionTicketActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repositories,
		StackableUseResolver: func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID: resolvedID, PVFPath: "stackable/10164001/10164630.stk", StackableType: "[etc]",
				QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{
					ActionType: "[gm quest clear]", MinimumLevel: 90, MaximumLevel: 90, VillageOnly: true,
				},
			}, nil
		},
		QuestCompletionTicketPlanner: func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, _ alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
			return alignedcmd.QuestCompletionTicketPlan{
				Quest: record, Character: character,
				QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, CompletedQuestIDs: []int64{3145},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DailyUseStates) != 0 {
		t.Fatalf("没有声明上限的道具不得产生 op541 投影: %+v", result.DailyUseStates)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	if got := dnfitemdailylimit.Used(character.Stats, itemID); got != 0 {
		t.Fatalf("没有声明上限的道具不得写计数: %d stats=%+v", got, character.Stats)
	}
}

// 动作没落地时，预留出去的次数必须还回来 —— 否则一次失败的点击会白吃掉当天的一次
// 额度（这正是 op44 那条路用 ReleaseDailyItemUse 防的事，op515 现在同样受保护）。
func TestUseStackableActionReturnsTheReservedSlotWhenTheActionFails(t *testing.T) {
	ctx := context.Background()
	const itemID = int64(10164630)
	wantErr := errors.New("quest planner refused")

	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {ItemID: itemID, Count: 3, Extra: map[string]string{"pvf_path": "stackable/10164001/10164630.stk"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 10)
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[7:11], questCompletionTicketActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repositories,
		StackableUseResolver: func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID: resolvedID, PVFPath: "stackable/10164001/10164630.stk", StackableType: "[etc]",
				DailyUseLimit: 5,
				QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{
					ActionType: "[gm quest clear]", MinimumLevel: 90, MaximumLevel: 90, VillageOnly: true,
				},
			}, nil
		},
		QuestCompletionTicketPlanner: func(dnfrepo.CharacterRecord, dnfrepo.QuestRecord, alignedcmd.QuestCompletionTicketResolution, time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
			return alignedcmd.QuestCompletionTicketPlan{}, wantErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UpperResponses) != 1 ||
		!bytes.Equal(result.UpperResponses[0].Body, []byte{0, useStackableActionFailureCode}) {
		t.Fatalf("动作失败时应回失败码: %+v", result)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if inventory.Slots["0:10"].Count != 3 {
		t.Fatalf("失败的动作消耗了道具: %+v", inventory.Slots)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	if got := dnfitemdailylimit.Used(character.Stats, itemID); got != 0 {
		t.Fatalf("失败的动作白吃了每日额度: %d stats=%+v", got, character.Stats)
	}
}
