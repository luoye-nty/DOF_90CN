package inventory

import (
	"context"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 任何在 PVF 里声明了 [total usable count] 的消耗品都按该数字限制每日使用次数。
// 改动前服务端只在疲劳药分支读这个字段：其它道具既不会被拒绝，也从不产生 op541
// 计数，于是客户端 tooltip 永远停在 0/N、重登归零，实际等于无限制。
func TestHandlerUseStackableEnforcesPVFTotalUsableCountForAnyConsumable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC)
	day := premiumServiceDayForTest(now)
	const itemID = int64(490003149)
	const limit = int64(2)

	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "78", AccountID: "acc", Level: 60,
		Stats: map[string]int64{dnffatigue.ItemUsageDayStatKey: day},
	}); err != nil {
		t.Fatal(err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "78",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 5): {
				ItemID: itemID,
				Count:  5,
				Extra: map[string]string{
					"item_kind":      "stackable",
					"pvf_path":       "stackable/490003001/chn_490003149.stk",
					"stackable_type": "[waste]",
				},
			},
		},
	})

	resolver := func(resolvedID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID:        resolvedID,
			PVFPath:       "stackable/490003001/chn_490003149.stk",
			StackableType: "[waste]",
			// The generic PVF [total usable count] contract: not fatigue-specific.
			DailyUseLimit: limit,
		}, nil
	}
	use := func() alignedcmd.Result {
		t.Helper()
		got, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode: uint16(dnfenum.CmdPacketUseStackable),
			Body: []byte{
				0x05, 0x00,
				listTypeMain,
				0x11, 0x22, 0x33, 0x44,
				0xCD, 0xDA, 0x34, 0x1D, // 490003149 little endian
				0x00, 0x00, 0x00, 0x00,
			},
			AccountID:            "acc",
			SelectedCharacterID:  78,
			Repositories:         repos,
			StackableUseResolver: resolver,
		})
		if err != nil {
			t.Fatalf("Handle error = %v", err)
		}
		return got
	}

	for want := int64(1); want <= limit; want++ {
		got := use()
		if len(got.DailyUseStates) != 1 || got.DailyUseStates[0].ItemID != uint32(itemID) ||
			int64(got.DailyUseStates[0].Used) != want {
			t.Fatalf("第 %d 次使用没有上报每日计数: %+v", want, got.DailyUseStates)
		}
	}

	// The third use is at the cap: rejected, with neither the stack nor the
	// counter touched.
	rejected := use()
	if len(rejected.DailyUseStates) != 0 || !strings.Contains(rejected.Reason, "daily use limit") {
		t.Fatalf("第三次使用应被每日上限拒绝: %+v", rejected)
	}
	loaded := loadTestInventory(t, ctx, repos, "78")
	if stack := loaded.Slots[slotKey(listTypeMain, 5)]; stack.ItemID != itemID || stack.Count != 3 {
		t.Fatalf("被拒绝的使用改动了道具: %+v", stack)
	}
	character, found, err := repos.Character.Load(ctx, "78")
	if err != nil || !found {
		t.Fatalf("character reload err=%v found=%t", err, found)
	}
	if got := character.Stats[dnffatigue.ItemUsageStatPrefix+"490003149"]; got != limit {
		t.Fatalf("每日计数=%d want=%d stats=%+v", got, limit, character.Stats)
	}
}

// 服务器必须给 op541 回放读到同一个计数器键，否则重登后客户端会把当天已用次数
// 当成 0，上限形同不存在。
func TestDailyUseStateProjectionReadsTheGenericCounterKey(t *testing.T) {
	result := UseStackableResult{
		ItemID:        490003149,
		DailyUseItemID: 490003149,
		DailyUseUsed:   2,
		DailyUseLimit:  3,
	}
	states := dailyUseStates(result)
	if len(states) != 1 || states[0].ItemID != 490003149 || states[0].Used != 2 {
		t.Fatalf("每日可用次数投影=%+v", states)
	}
	// Without a cap the projection stays silent, so uncapped items never cost
	// the client an extra state packet.
	if got := dailyUseStates(UseStackableResult{ItemID: 490003149}); len(got) != 0 {
		t.Fatalf("无上限道具不应产生投影: %+v", got)
	}
}
