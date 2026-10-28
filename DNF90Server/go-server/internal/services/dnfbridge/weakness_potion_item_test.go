package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfweaknesspotion "longheng.io/server/internal/modules/dnf/weaknesspotion"
)

// 防虚弱药剂的效果是客户端本地的：活体抓包（2026-09-15 00:43）显示使用只以普通
// class-1/op44 上行到达，之后没有任何包携带该状态。所以服务端按道具家族记下持久
// 免疫窗口，放弃副本时据此豁免 —— 这条测试钉住"窗口内不惩罚、窗口外照常惩罚"。
func TestForcedReturnIsExemptedByAntiWeaknessPotionWindow(t *testing.T) {
	ctx := context.Background()
	service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, 0, 0, nil)
	characterID := "5"

	arm := func(offset time.Duration) {
		t.Helper()
		character, found, err := repositories.Character.Load(ctx, characterID)
		if err != nil || !found {
			t.Fatalf("load character err=%v found=%t", err, found)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 2)
		}
		character.Stats[dnfweaknesspotion.ImmuneUntilStatKey] = service.gameplayNow().Add(offset).Unix()
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}

	// 生效中：不给惩罚，也不写任何东西。
	arm(30 * time.Minute)
	level, exemption, err := service.commitCurrentForcedReturnWeakness(session)
	if err != nil {
		t.Fatal(err)
	}
	if exemption != currentWeaknessExemptionPotItem {
		t.Fatalf("exemption=%q want %q", exemption, currentWeaknessExemptionPotItem)
	}
	if level != 0 || currentWeaknessStoredLevel(t, repositories) != 0 {
		t.Fatalf("生效期内不应写入惩罚: level=%d stored=%d", level, currentWeaknessStoredLevel(t, repositories))
	}

	// 已过期：照常按满额惩罚。
	arm(-time.Minute)
	level, exemption, err = service.commitCurrentForcedReturnWeakness(session)
	if err != nil {
		t.Fatal(err)
	}
	if exemption != currentWeaknessExemptionNone {
		t.Fatalf("过期窗口不应豁免: exemption=%q", exemption)
	}
	if level != currentWeaknessForcedReturnLevel ||
		currentWeaknessStoredLevel(t, repositories) != currentWeaknessForcedReturnLevel {
		t.Fatalf("过期后应写入满额惩罚: level=%d stored=%d", level, currentWeaknessStoredLevel(t, repositories))
	}
}

// 持有型（十周年防虚弱青石）：材料栏里带着就已经生效，不需要使用，也没有窗口。
func TestForcedReturnIsExemptedWhileHoldingPassiveAntiWeaknessItem(t *testing.T) {
	ctx := context.Background()
	service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, 0, 0, nil)

	const heldItemID = int64(490008656)
	if !dnfweaknesspotion.HeldExempts(heldItemID) {
		t.Fatalf("持有人型清单缺少 %d", heldItemID)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5",
		Slots: map[string]dnfrepo.ItemStack{
			"0:3": {ItemID: heldItemID, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	level, exemption, err := service.commitCurrentForcedReturnWeakness(session)
	if err != nil {
		t.Fatal(err)
	}
	if exemption != currentWeaknessExemptionPotHeld {
		t.Fatalf("exemption=%q want %q", exemption, currentWeaknessExemptionPotHeld)
	}
	if level != 0 || currentWeaknessStoredLevel(t, repositories) != 0 {
		t.Fatalf("持有期内不应写入惩罚: level=%d stored=%d", level, currentWeaknessStoredLevel(t, repositories))
	}
}

// 同一张表必须覆盖用户报告过的那些版本，并且时长各不相同。
func TestAntiWeaknessPotionFamilyCarriesPerItemDurations(t *testing.T) {
	for itemID, wantSeconds := range map[int64]int64{
		10087825:  1800, // 防虚弱药剂（本次报告的那一件）
		490003621: 600,
		490008213: 900,
		490007908: 3600,
		10005909:  1800, // 说明未写时长，取 [cool time]
	} {
		seconds, ok := dnfweaknesspotion.ImmunitySeconds(itemID)
		if !ok || seconds != wantSeconds {
			t.Fatalf("item=%d seconds=%d ok=%t want=%d", itemID, seconds, ok, wantSeconds)
		}
	}
	if seconds, ok := dnfweaknesspotion.ImmunitySeconds(1001); ok || seconds != 0 {
		t.Fatalf("家族外道具不应有时长: seconds=%d ok=%t", seconds, ok)
	}
}
