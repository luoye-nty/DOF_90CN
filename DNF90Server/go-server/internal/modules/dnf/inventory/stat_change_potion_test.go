package inventory

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 现场 2026-10-03（服主报，角色 测试x = char 11）：史诗Buff药剂 490002458 连点 15 次，
// op44 每次都被 "[waste] provenance" 那道门静默拒绝（error 17）——既没消耗也没效果。
// 本文件锁住修好之后的落地结果：背包减 1 与掉落率增益窗口**同笔**生效。
const testEpicBuffPotionItemID = 490002458

func newStatChangePotionFixture(t *testing.T, ctx context.Context, count int64, stats map[string]int64) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "42",
		AccountID:   "acc",
		Stats:       stats,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "42",
		Slots: map[string]dnfrepo.ItemStack{
			"0:8": {
				ItemID: testEpicBuffPotionItemID,
				Count:  count,
				Extra:  map[string]string{"pvf_path": "stackable/490002001/chn_490002458.stk"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return repos
}

func newStatChangePotionRequest(
	repos dnfrepo.Group,
	instance uint32,
	percent int64,
	durationSeconds int64,
) alignedcmd.Request {
	return alignedcmd.Request{
		Opcode:              uint16(430),
		AccountID:           "acc",
		SelectedCharacterID: 42,
		Repositories:        repos,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID:        itemID,
				PVFPath:       "stackable/490002001/chn_490002458.stk",
				StackableType: "[expert town potion]",
				StatChangePotion: &alignedcmd.StatChangePotionResolution{
					EffectDurationSeconds: durationSeconds,
					EpicDropRatePercent:   percent,
				},
			}, nil
		},
		// op44 的成功回包里带 instance，命令层要求它非零（现场 body 就是这个形状）。
		Command: 0,
	}
}

// useStatChangePotionThroughOwner 直接调用 owner（不经过 op44 的 action 判据），
// 与 useExperienceConsumable 的用例同一写法。
func useStatChangePotionThroughOwner(
	t *testing.T,
	ctx context.Context,
	repos dnfrepo.Group,
	instance int32,
	percent int64,
	durationSeconds int64,
	now time.Time,
) (UseStackableResult, error) {
	t.Helper()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return now }
	cmd := Command{
		Operation:           "use_stackable",
		AccountID:           "acc",
		SelectedCharacterID: 42,
		SourceListType:      listTypeMain,
		SourceSlotIndex:     8,
		SourceInstanceValue: instance,
		ItemTemplateID:      testEpicBuffPotionItemID,
	}
	resolution := alignedcmd.StackableUseResolution{
		ItemID:        testEpicBuffPotionItemID,
		PVFPath:       "stackable/490002001/chn_490002458.stk",
		StackableType: "[expert town potion]",
		StatChangePotion: &alignedcmd.StatChangePotionResolution{
			EffectDurationSeconds: durationSeconds,
			EpicDropRatePercent:   percent,
		},
	}
	return owner.useStatChangePotion(ctx, cmd, resolution)
}

// TestStatChangePotionConsumesOneAndStartsTheDropRateWindow：
// 一次使用 = 背包 2→1 + 角色身上出现 200%/10 分钟的掉落率窗口。
func TestStatChangePotionConsumesOneAndStartsTheDropRateWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 19, 0, 0, 0, time.UTC)
	repos := newStatChangePotionFixture(t, ctx, 2, map[string]int64{"fatigue": 100})

	result, err := useStatChangePotionThroughOwner(t, ctx, repos, 0x1D34D81A, 200, 600, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StatChangePotionApplied || result.RemainingCount != 1 || !result.Changed {
		t.Fatalf("result=%+v", result)
	}
	if result.EpicDropRatePercent != 200 || result.EpicDropRateExpireAt != now.Add(10*time.Minute).Unix() {
		t.Fatalf("buff fields=%+v", result)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "42")
	if err != nil || !found {
		t.Fatalf("inventory load found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:8"]; stack.Count != 1 || stack.ItemID != testEpicBuffPotionItemID {
		t.Fatalf("slot after use=%+v", stack)
	}
	character, found, err := repos.Character.Load(ctx, "42")
	if err != nil || !found {
		t.Fatalf("character load found=%t err=%v", found, err)
	}
	effect, active := statbuff.ActiveEpicDropRate(character, now.Add(time.Minute))
	if !active || effect.MultiplierPercent != 200 || effect.ItemID != testEpicBuffPotionItemID {
		t.Fatalf("active=%t effect=%+v stats=%v", active, effect, character.Stats)
	}
	if character.Stats["fatigue"] != 100 {
		t.Fatalf("unrelated stat changed: %v", character.Stats)
	}
	// 10 分钟之后同一条记录必须自己失效：不需要任何人来清理。
	if _, active := statbuff.ActiveEpicDropRate(character, now.Add(11*time.Minute)); active {
		t.Fatalf("window must expire after 10 minutes")
	}
}

// TestStatChangePotionRemovesTheRowAtTheLastOne：最后一张吃完，格子必须消失。
func TestStatChangePotionRemovesTheRowAtTheLastOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 19, 0, 0, 0, time.UTC)
	repos := newStatChangePotionFixture(t, ctx, 1, map[string]int64{})

	result, err := useStatChangePotionThroughOwner(t, ctx, repos, 0x0000ABCD, 200, 600, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemainingCount != 0 {
		t.Fatalf("remaining=%d", result.RemainingCount)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := inventory.Slots["0:8"]; found {
		t.Fatalf("consumed row must be deleted: %+v", inventory.Slots)
	}
}

// TestStatChangePotionWithoutADropRateEffectOnlyConsumes：
// 同族里不影响掉落率的（强化秘药/经验药剂一类）照样能用，但不写增益窗口 ——
// 服务端不假装自己有那个效果。
func TestStatChangePotionWithoutADropRateEffectOnlyConsumes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 19, 0, 0, 0, time.UTC)
	repos := newStatChangePotionFixture(t, ctx, 1, map[string]int64{})

	result, err := useStatChangePotionThroughOwner(t, ctx, repos, 0x00001111, 0, 1800, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StatChangePotionApplied || result.RemainingCount != 0 {
		t.Fatalf("result=%+v", result)
	}
	if result.EpicDropRatePercent != 0 || result.EpicDropRateExpireAt != 0 {
		t.Fatalf("no drop-rate effect expected: %+v", result)
	}
	character, _, err := repos.Character.Load(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	if _, active := statbuff.ActiveEpicDropRate(character, now.Add(time.Minute)); active {
		t.Fatalf("must not start a drop-rate window: %v", character.Stats)
	}
	if len(character.Stats) != 0 {
		t.Fatalf("character stats must stay untouched: %v", character.Stats)
	}
}

// TestStatChangePotionRejectsMismatchedRowsAndContracts：持久行/契约不匹配一律拒绝，
// 且什么都不动。
func TestStatChangePotionRejectsMismatchedRowsAndContracts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		slot            int16
		itemTemplateID  int32
		pvfPath         string
		durationSeconds int64
	}{
		{name: "empty slot", slot: 9, itemTemplateID: testEpicBuffPotionItemID, pvfPath: "stackable/490002001/chn_490002458.stk", durationSeconds: 600},
		{name: "item mismatch", slot: 8, itemTemplateID: 12345, pvfPath: "stackable/490002001/chn_490002458.stk", durationSeconds: 600},
		{name: "zero duration", slot: 8, itemTemplateID: testEpicBuffPotionItemID, pvfPath: "stackable/490002001/chn_490002458.stk"},
		{name: "path mismatch", slot: 8, itemTemplateID: testEpicBuffPotionItemID, pvfPath: "stackable/other.stk", durationSeconds: 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := newStatChangePotionFixture(t, ctx, 2, map[string]int64{})
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			owner.now = func() time.Time { return now }
			cmd := Command{
				Operation:           "use_stackable",
				AccountID:           "acc",
				SelectedCharacterID: 42,
				SourceListType:      listTypeMain,
				SourceSlotIndex:     tc.slot,
				SourceInstanceValue: 1,
				ItemTemplateID:      tc.itemTemplateID,
			}
			resolution := alignedcmd.StackableUseResolution{
				ItemID:        testEpicBuffPotionItemID,
				PVFPath:       tc.pvfPath,
				StackableType: "[expert town potion]",
				StatChangePotion: &alignedcmd.StatChangePotionResolution{
					EffectDurationSeconds: tc.durationSeconds,
					EpicDropRatePercent:   200,
				},
			}
			if tc.itemTemplateID != testEpicBuffPotionItemID {
				resolution.ItemID = int64(tc.itemTemplateID)
			}
			if _, err := owner.useStatChangePotion(ctx, cmd, resolution); err == nil {
				t.Fatalf("expected rejection")
			}
			inventory, _, err := repos.Inventory.Load(ctx, "42")
			if err != nil {
				t.Fatal(err)
			}
			if stack := inventory.Slots["0:8"]; stack.Count != 2 {
				t.Fatalf("rejection must not consume: %+v", stack)
			}
			character, _, err := repos.Character.Load(ctx, "42")
			if err != nil {
				t.Fatal(err)
			}
			if _, active := statbuff.ActiveEpicDropRate(character, now); active {
				t.Fatalf("rejection must not start a window: %v", character.Stats)
			}
		})
	}
}

// TestOrdinaryWasteStillNeedsWasteProvenance：修这一族**不能**顺手放松普通消耗品那道门。
func TestOrdinaryWasteStillNeedsWasteProvenance(t *testing.T) {
	ctx := context.Background()
	repos := newStatChangePotionFixture(t, ctx, 2, map[string]int64{})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := Command{
		Operation:           "use_stackable",
		AccountID:           "acc",
		SelectedCharacterID: 42,
		SourceListType:      listTypeMain,
		SourceSlotIndex:     8,
		SourceInstanceValue: 1,
		ItemTemplateID:      testEpicBuffPotionItemID,
	}
	// 没有 StatChangePotion 契约、PVF 又证明不了 [waste] ⇒ 仍然必须被拒。
	resolution := alignedcmd.StackableUseResolution{
		ItemID:        testEpicBuffPotionItemID,
		PVFPath:       "stackable/490002001/chn_490002458.stk",
		StackableType: "[expert town potion]",
	}
	resolver := func(int64) (alignedcmd.StackableUseResolution, error) { return resolution, nil }
	_, err = owner.useStackableWithResolvers(
		ctx, cmd, nil, nil, nil, nil, nil, nil, nil, nil, resolver)
	if err == nil {
		t.Fatalf("a non-[waste], non-stat-change item must still be refused")
	}
}

// TestStatChangePotionRollsTheBuffTableTier：数值来自 PVF 的 buff 表
// （etc/variousbuffstackablelist.etc + etc/variousbufflist.etc），不是猜的。
//
// 赛丽亚的幸运掉落药剂 = `[probability buff list] 1090 60 1091 30 1092 10`，
// 三档 buff 的 `epic_rarity_item_drop_rate` param 分别是 100/150/200 ⇒ 倍率 200/250/300。
// 这里注入确定性的 random，逐档验证"掷到哪一档就写哪一档"，并确认 buff id 被记下来。
func TestStatChangePotionRollsTheBuffTableTier(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 20, 0, 0, 0, time.UTC)
	rolls := []alignedcmd.StatChangePotionBuffRoll{
		{BuffID: 1090, Weight: 60, BonusPercent: 100},
		{BuffID: 1091, Weight: 30, BonusPercent: 150},
		{BuffID: 1092, Weight: 10, BonusPercent: 200},
	}
	for _, tc := range []struct {
		name       string
		pick       int
		wantBuffID int64
		wantFactor int64
	}{
		{name: "lower tier (weight 60)", pick: 0, wantBuffID: 1090, wantFactor: 200},
		{name: "middle tier (weight 30)", pick: 60, wantBuffID: 1091, wantFactor: 250},
		{name: "upper tier (weight 10)", pick: 90, wantBuffID: 1092, wantFactor: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := newStatChangePotionFixture(t, ctx, 1, map[string]int64{})
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			owner.now = func() time.Time { return now }
			owner.statChangeRandom = func(int) int { return tc.pick }
			cmd := Command{
				Operation:           "use_stackable",
				AccountID:           "acc",
				SelectedCharacterID: 42,
				SourceListType:      listTypeMain,
				SourceSlotIndex:     8,
				SourceInstanceValue: 1,
				ItemTemplateID:      testEpicBuffPotionItemID,
			}
			resolution := alignedcmd.StackableUseResolution{
				ItemID:        testEpicBuffPotionItemID,
				PVFPath:       "stackable/490002001/chn_490002458.stk",
				StackableType: "[expert town potion]",
				StatChangePotion: &alignedcmd.StatChangePotionResolution{
					EffectDurationSeconds: 600,
					BuffRolls:             rolls,
				},
			}
			result, err := owner.useStatChangePotion(ctx, cmd, resolution)
			if err != nil {
				t.Fatal(err)
			}
			if result.EpicDropRatePercent != tc.wantFactor || result.EpicDropRateBuffID != tc.wantBuffID {
				t.Fatalf("result percent=%d buff=%d want percent=%d buff=%d",
					result.EpicDropRatePercent, result.EpicDropRateBuffID, tc.wantFactor, tc.wantBuffID)
			}
			character, _, err := repos.Character.Load(ctx, "42")
			if err != nil {
				t.Fatal(err)
			}
			effect, active := statbuff.ActiveEpicDropRate(character, now.Add(time.Minute))
			if !active || effect.MultiplierPercent != tc.wantFactor || effect.BuffID != tc.wantBuffID {
				t.Fatalf("stored active=%t effect=%+v stats=%v", active, effect, character.Stats)
			}
			if character.Stats[statbuff.EpicDropRateBuffIDStatKey] != tc.wantBuffID {
				t.Fatalf("buff id stat=%d want=%d", character.Stats[statbuff.EpicDropRateBuffIDStatKey], tc.wantBuffID)
			}
		})
	}
}

// TestStatChangePotionUseDoesNotPublishTheEffectItem：使用这一发**不许**发 class0/op174。
//
// 服主 2026-10-03 的关键反馈："药剂刚能用那一版（那时服务端一个包都没发）提示只有一行"，
// 说明**客户端自己按 op44 本地登记的**效果道具就已经打了那一行；服务端再发 op174 会让它
// 登记第二次 → 提示变两行。这与既有经验药水路径的约定一致（op174 只用于登录/重登恢复）。
func TestStatChangePotionUseDoesNotPublishTheEffectItem(t *testing.T) {
	result := UseStackableResult{
		ListType:                listTypeMain,
		SlotIndex:               8,
		ItemID:                  testEpicBuffPotionItemID,
		RemainingCount:          0,
		Changed:                 true,
		StatChangePotionApplied: true,
		StatChangePotionSeconds: 600,
		EpicDropRatePercent:     200,
		EpicDropRateBuffID:      1136,
		EpicDropRateExpireAt:    1791000000,
	}
	alignedResult := ownerAppliedUseStackableResult(uint16(44), Command{Operation: "use_stackable"}, result)
	if alignedResult.EffectItemInfo != nil {
		t.Fatalf("use path must NOT publish the op174 effect item (client registers it locally): %+v",
			alignedResult.EffectItemInfo)
	}
	if alignedResult.CharacterAddBuff == nil || alignedResult.CharacterAddBuff.BuffID != 1136 {
		t.Fatalf("buff registration must still be published: %+v", alignedResult.CharacterAddBuff)
	}
	// 没有掉落率窗口时（只消耗的那 12 件同族）同样什么都不发。
	plain := result
	plain.EpicDropRateExpireAt = 0
	plain.StatChangePotionApplied = false
	if got := ownerAppliedUseStackableResult(uint16(44), Command{Operation: "use_stackable"}, plain); got.EffectItemInfo != nil ||
		got.CharacterAddBuff != nil {
		t.Fatalf("plain potion must publish neither projection: %+v", got)
	}
}

// TestStatChangePotionUseAsksForTheBuffRegistration：一次使用要发 buff 注册 ——
// op487（buff 图标；本客户端 NOTIPACKET 基址 485，不是 A21 的 486）。
func TestStatChangePotionUseAsksForTheBuffRegistration(t *testing.T) {
	result := UseStackableResult{
		ListType:                   listTypeMain,
		SlotIndex:                  67,
		ItemID:                     490002458,
		RemainingCount:             0,
		Changed:                    true,
		StatChangePotionApplied:    true,
		StatChangePotionSeconds:    600,
		EpicDropRatePercent:        200,
		EpicDropRateBuffID:         1136,
		PreviousEpicDropRateBuffID: 0,
		EpicDropRateExpireAt:       1791000000,
	}
	alignedResult := ownerAppliedUseStackableResult(uint16(44), Command{Operation: "use_stackable"}, result)
	if alignedResult.EffectItemInfo != nil {
		t.Fatalf("op174 must not be published on the use path: %+v", alignedResult.EffectItemInfo)
	}
	if alignedResult.CharacterAddBuff == nil || alignedResult.CharacterAddBuff.BuffID != 1136 ||
		alignedResult.CharacterAddBuff.RemoveBuffID != 0 {
		t.Fatalf("op487 projection=%+v", alignedResult.CharacterAddBuff)
	}

	// 换档（赛丽亚三档互替）时要先撤掉旧 buff。
	result.PreviousEpicDropRateBuffID = 1090
	result.EpicDropRateBuffID = 1092
	replaced := ownerAppliedUseStackableResult(uint16(44), Command{Operation: "use_stackable"}, result)
	if replaced.CharacterAddBuff == nil || replaced.CharacterAddBuff.BuffID != 1092 ||
		replaced.CharacterAddBuff.RemoveBuffID != 1090 {
		t.Fatalf("tier replacement projection=%+v", replaced.CharacterAddBuff)
	}

	// 同一条 buff 重复使用不产生多余的 remove。
	result.PreviousEpicDropRateBuffID = 1092
	same := ownerAppliedUseStackableResult(uint16(44), Command{Operation: "use_stackable"}, result)
	if same.CharacterAddBuff == nil || same.CharacterAddBuff.RemoveBuffID != 0 {
		t.Fatalf("same-buff reuse must not remove: %+v", same.CharacterAddBuff)
	}
}
