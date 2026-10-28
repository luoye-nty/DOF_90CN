package dnfbridge

import (
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestStampCurrentDailyDeleteItemOnlyTouchesDailyDeleteItems(t *testing.T) {
	ordinary := dnfrepo.ItemStack{ItemID: 3011}
	if _, changed := stampCurrentDailyDeleteItem(ordinary, dungeonDropItemDefinition{}, 100); changed {
		t.Fatal("a non daily-delete item must not be stamped")
	}

	stack, changed := stampCurrentDailyDeleteItem(
		dnfrepo.ItemStack{ItemID: 490001382}, dungeonDropItemDefinition{DailyDeleteItem: true}, 100)
	if !changed {
		t.Fatal("a daily-delete item must be stamped on grant")
	}
	if got := stack.Extra[currentDailyDeleteItemStateKey]; got != "100" {
		t.Fatalf("stamp = %q, want %q", got, "100")
	}

	// 重复盖同一天是幂等的，不该把行标记成脏的。
	if _, again := stampCurrentDailyDeleteItem(stack, dungeonDropItemDefinition{DailyDeleteItem: true}, 100); again {
		t.Fatal("re-stamping the same service day must be a no-op")
	}
}

func TestCurrentDailyDeleteItemDue(t *testing.T) {
	stamped := func(day string) dnfrepo.ItemStack {
		return dnfrepo.ItemStack{ItemID: 490001382, Extra: map[string]string{currentDailyDeleteItemStateKey: day}}
	}
	for _, row := range []struct {
		name       string
		stack      dnfrepo.ItemStack
		serviceDay int64
		wantDue    bool
		wantStamp  bool
	}{
		{name: "同一天不删", stack: stamped("100"), serviceDay: 100, wantDue: false, wantStamp: true},
		{name: "跨过边界就删", stack: stamped("100"), serviceDay: 101, wantDue: true, wantStamp: true},
		{name: "跨多天也删", stack: stamped("100"), serviceDay: 137, wantDue: true, wantStamp: true},
		// 时钟回拨/存档搬迁不该把东西删掉。
		{name: "戳在未来不删", stack: stamped("200"), serviceDay: 100, wantDue: false, wantStamp: true},
		// 没有戳的是存量：补盖，不是没收。
		{name: "没有戳", stack: dnfrepo.ItemStack{ItemID: 490001382}, serviceDay: 100, wantDue: false, wantStamp: false},
		// 读不懂的戳同样按存量处理，宁可多留一天也别凭坏数据删东西。
		{name: "戳坏了", stack: stamped("not-a-day"), serviceDay: 100, wantDue: false, wantStamp: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			due, hasStamp := currentDailyDeleteItemDue(row.stack, row.serviceDay)
			if due != row.wantDue || hasStamp != row.wantStamp {
				t.Fatalf("due=%t stamped=%t, want due=%t stamped=%t", due, hasStamp, row.wantDue, row.wantStamp)
			}
		})
	}
}

// 边界必须是北京 06:00，和 premium 的服务日是同一个，否则每日删除会和签到、
// 疲劳重置错开一截。
func TestDailyDeleteUsesTheServiceDayBoundary(t *testing.T) {
	// 2026-09-16 05:59 北京 = 前一个服务日；06:01 北京 = 新的服务日。
	beijing := time.FixedZone("CST", 8*3600)
	before := time.Date(2026, 9, 16, 5, 59, 0, 0, beijing)
	after := time.Date(2026, 9, 16, 6, 1, 0, 0, beijing)
	if premium.ServiceDayID(before.UTC()) == premium.ServiceDayID(after.UTC()) {
		t.Fatal("06:00 Beijing must separate two service days")
	}
	// 同一天内的两个时刻必须落在同一个服务日。
	noon := time.Date(2026, 9, 16, 12, 0, 0, 0, beijing)
	if premium.ServiceDayID(after.UTC()) != premium.ServiceDayID(noon.UTC()) {
		t.Fatal("06:01 and 12:00 on the same day must share a service day")
	}
}

// 发放路径统一在 applyCurrentPVFItemExpirationAt 里盖戳：本服屏蔽了物品期限，
// 每日删除物品大多没有 ExpirationDate，所以那个函数不能在期限为空时提前返回。
func TestApplyCurrentPVFItemExpirationStampsDailyDeleteWithoutAnExpirationDate(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	stack, changed := applyCurrentPVFItemExpirationAt(
		dnfrepo.ItemStack{ItemID: 490001382},
		dungeonDropItemDefinition{ItemID: 490001382, Kind: dungeonDropItemStackable, DailyDeleteItem: true},
		now,
	)
	if !changed {
		t.Fatal("granting a daily-delete item must mark the stack dirty")
	}
	want := premium.ServiceDayID(now)
	if got := stack.Extra[currentDailyDeleteItemStateKey]; got == "" {
		t.Fatalf("daily-delete stamp is missing; extra = %v", stack.Extra)
	} else if due, _ := currentDailyDeleteItemDue(stack, want); due {
		t.Fatal("an item granted today must not already be due")
	}
	if due, _ := currentDailyDeleteItemDue(stack, want+1); !due {
		t.Fatal("the item must be due after the next 06:00 boundary")
	}
}

// 普通物品不该因为这条路径多出任何 Extra 键。
func TestApplyCurrentPVFItemExpirationLeavesOrdinaryItemsAlone(t *testing.T) {
	stack, _ := applyCurrentPVFItemExpirationAt(
		dnfrepo.ItemStack{ItemID: 3011},
		dungeonDropItemDefinition{ItemID: 3011, Kind: dungeonDropItemStackable},
		time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC),
	)
	if _, present := stack.Extra[currentDailyDeleteItemStateKey]; present {
		t.Fatalf("ordinary item picked up a daily-delete stamp; extra = %v", stack.Extra)
	}
}

// 真 PVF 冒烟：玩家报的那两件必须被解析成每日删除，而且它们身上**没有**期限字段
// —— 这正是"客户端显示 1 天、服务端却毫无察觉"的原因。
func TestRealPVFDailyDeleteItemsAreParsed(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate daily-delete parsing")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, name := range map[uint32]string{
		490001382: "公会祝福礼盒",
		490001383: "魔界抗疲劳秘药",
	} {
		definition, err := catalog.ResolveItem(itemID)
		if err != nil {
			t.Fatalf("resolve %d (%s): %v", itemID, name, err)
		}
		if !definition.DailyDeleteItem {
			t.Fatalf("%d (%s) must parse as a daily-delete item", itemID, name)
		}
		// 本服屏蔽了期限字段，所以这两件不该有 ExpirationDate/UsablePeriodDays：
		// 每日删除必须独立于它们生效，不能挂在期限那条通道上。
		if !definition.ExpirationDate.IsZero() || definition.UsablePeriodDays != 0 {
			t.Fatalf("%d (%s) unexpectedly carries an expiry: date=%v days=%d",
				itemID, name, definition.ExpirationDate, definition.UsablePeriodDays)
		}
	}
	// 普通消耗品不该被误判。
	if definition, err := catalog.ResolveItem(3011); err == nil && definition.DailyDeleteItem {
		t.Fatal("an ordinary material must not parse as daily-delete")
	}
}
