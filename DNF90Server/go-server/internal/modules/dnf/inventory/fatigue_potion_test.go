package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestUseStackableAction58RestoresFatigueAndConsumesOnePotion(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	dayID := premiumServiceDayForTest(now)
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Stats: map[string]int64{
			"fatigue": 65, "fatigue_limit": 188, "fatigue_display_update": 123,
			dnffatigue.DailyRecoveryDayStatKey: dayID, dnffatigue.ItemUsageDayStatKey: dayID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:115": {ItemID: 490702338, Count: 2, Extra: map[string]string{"pvf_path": "stackable/cash/chn_490702338.stk"}},
	}}); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 115)
	binary.LittleEndian.PutUint32(body[7:11], fatiguePotionActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackableAction), Body: body,
		AccountID: "acc", SelectedCharacterID: 77, Repositories: repos,
		FatiguePotionNow: now,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID: itemID, PVFPath: "stackable/cash/chn_490702338.stk", StackableType: "[expert town potion]",
				FatigueRecovery: 50, FatigueMaximumRemaining: 106, DailyUseLimit: 1,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{1, 115, 0, 0, 0, 1, 0, 0, 0, 1}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("op515 ACK=%x want=%x", got, wantAck)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].SlotIndex != 115 ||
		len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedFatigue {
		t.Fatalf("refresh result=%+v", result)
	}
	bag, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found || bag.Slots["0:115"].Count != 1 {
		t.Fatalf("inventory=%+v found=%t err=%v", bag, found, err)
	}
	character, found, err := repos.Character.Load(ctx, "77")
	if err != nil || !found || character.Stats["fatigue"] != 115 || character.Stats["fatigue_display_update"] != 73 ||
		character.Stats[dnffatigue.ItemUsageStatPrefix+"490702338"] != 1 {
		t.Fatalf("character=%+v found=%t err=%v", character, found, err)
	}
}

func TestUseStackableAction58RejectsDailyLimitWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	dayID := premiumServiceDayForTest(now)
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{
		"fatigue": 65, "fatigue_limit": 188,
		dnffatigue.DailyRecoveryDayStatKey:        dayID,
		dnffatigue.ItemUsageDayStatKey:                   dayID,
		dnffatigue.ItemUsageStatPrefix + "490702338": 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:115": {ItemID: 490702338, Count: 2, Extra: map[string]string{"pvf_path": "stackable/cash/chn_490702338.stk"}},
	}}); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 115)
	binary.LittleEndian.PutUint32(body[7:11], fatiguePotionActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackableAction), Body: body, SelectedCharacterID: 77, Repositories: repos,
		FatiguePotionNow: now,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{ItemID: itemID, PVFPath: "stackable/cash/chn_490702338.stk", StackableType: "[expert town potion]", FatigueRecovery: 50, FatigueMaximumRemaining: 106, DailyUseLimit: 1}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, []byte{0, useStackableActionFailureCode}) {
		t.Fatalf("result=%+v", result)
	}
	bag, _, _ := repos.Inventory.Load(ctx, "77")
	if got := bag.Slots["0:115"].Count; got != 2 {
		t.Fatalf("count=%d want 2", got)
	}
}

func TestUseFatiguePotionClearsAllPriorDayPerItemCounters(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{
		"fatigue": 10, "fatigue_limit": 156, dnffatigue.ItemUsageDayStatKey: premiumServiceDayForTest(now.Add(-24 * time.Hour)),
		dnffatigue.DailyRecoveryDayStatKey:   premiumServiceDayForTest(now),
		dnffatigue.ItemUsageStatPrefix + "1001": 3, dnffatigue.ItemUsageStatPrefix + "1002": 2,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:1": {ItemID: 1001, Count: 1, Extra: map[string]string{"pvf_path": "stackable/fatigue.stk"}},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.UseFatiguePotionAction(ctx, Command{SelectedCharacterID: 77, SourceListType: listTypeMain, SourceSlotIndex: 1, ActionIndex: fatiguePotionActionIndex}, func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{ItemID: itemID, PVFPath: "stackable/fatigue.stk", StackableType: "[expert town potion]", FatigueRecovery: 1, FatigueMaximumRemaining: 155, DailyUseLimit: 3}, nil
	}, now, false)
	if err != nil {
		t.Fatal(err)
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats[dnffatigue.ItemUsageStatPrefix+"1001"] != 1 || character.Stats[dnffatigue.ItemUsageStatPrefix+"1002"] != 0 {
		t.Fatalf("stats=%+v", character.Stats)
	}
}

func premiumServiceDayForTest(now time.Time) int64 {
	return (now.Unix() + int64(2*time.Hour/time.Second)) / int64(24*time.Hour/time.Second)
}

// 每日上限被打满时的拒绝必须是**可识别**的错误：桥接层据此给玩家一条看得见的提示，
// 而不是只回一个客户端渲染成沉默的失败码（活体 2026-09-14 23:05：疲劳药用满 3/3 后
// 第 4 次点击毫无反馈）。
func TestUseFatiguePotionActionReportsDailyLimitAsTypedRejection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 15, 5, 0, 0, time.UTC)
	day := premiumServiceDayForTest(now)
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Level: 90,
		Stats: map[string]int64{
			"fatigue": 60, "fatigue_limit": 156,
			dnffatigue.FatigueDailyDateStatKey: day,
			dnffatigue.PotionUsageDayStatKey:    day,
			dnffatigue.ItemUsageDayStatKey:      day,
			dnffatigue.ItemUsageStatPrefix + "1001": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:1": {ItemID: 1001, Count: 5, Extra: map[string]string{"pvf_path": "stackable/fatigue.stk"}},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.UseFatiguePotionAction(ctx, Command{
		SelectedCharacterID: 77, SourceListType: listTypeMain, SourceSlotIndex: 1,
		ActionIndex: fatiguePotionActionIndex,
	}, func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: "stackable/fatigue.stk", StackableType: "[expert town potion]",
			FatigueRecovery: 1, FatigueMaximumRemaining: 155, DailyUseLimit: 3,
		}, nil
	}, now, false)

	var limitErr FatiguePotionDailyLimitError
	if !errors.Is(err, ErrFatiguePotionDailyLimit) || !errors.As(err, &limitErr) ||
		limitErr.ItemID != 1001 || limitErr.Used != 3 || limitErr.Limit != 3 {
		t.Fatalf("每日上限拒绝不是可识别的错误: %v", err)
	}
	if !errors.Is(err, ErrFatiguePotionContract) {
		t.Fatalf("契约哨兵丢失: %v", err)
	}
	// 被拒的那一次不能消耗道具，也不能改写计数。
	inventory, _, loadErr := repos.Inventory.Load(ctx, "77")
	if loadErr != nil || inventory.Slots["0:1"].Count != 5 {
		t.Fatalf("被拒的使用改动了道具: %+v err=%v", inventory.Slots, loadErr)
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats[dnffatigue.ItemUsageStatPrefix+"1001"] != 3 {
		t.Fatalf("被拒的使用改动了计数: %+v", character.Stats)
	}

	// 提示由客户端自己处理（它已经从 op541 拿到计数），服务端不再手搓文案。
	_ = fmt.Sprintf("%v", err)
}
