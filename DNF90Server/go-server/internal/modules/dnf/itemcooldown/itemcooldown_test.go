package itemcooldown

import "testing"

// 同组共享一个窗口：PVF [cooltime group] 的语义是"这一组道具共用一次冷却"，
// 现场 2026-09-17 的 66 组里就放着首日/次日/三日/第N天礼盒。
func TestBlocksSharesOneWindowPerCooltimeGroup(t *testing.T) {
	now := int64(1_000_000)
	stats := map[string]int64{}
	Set(stats, 490702504, now+86_400)
	groups := map[int64]int64{490702504: 66, 490702505: 66, 490702506: 66, 999: 7}
	groupOf := func(itemID int64) int64 { return groups[itemID] }

	if entry, blocked := Blocks(stats, now, 490702504, 66, groupOf); !blocked || entry.ItemID != 490702504 {
		t.Fatalf("same item not blocked: %+v blocked=%t", entry, blocked)
	}
	if entry, blocked := Blocks(stats, now, 490702505, 66, groupOf); !blocked || entry.ItemID != 490702504 {
		t.Fatalf("same group item not blocked: %+v blocked=%t", entry, blocked)
	}
	if _, blocked := Blocks(stats, now, 999, 7, groupOf); blocked {
		t.Fatal("an unrelated group was blocked")
	}
	// 组 0 = 只锁自己：另一件没有组的道具不受影响。
	Set(stats, 490000001, now+60)
	if _, blocked := Blocks(stats, now, 490000002, 0, groupOf); blocked {
		t.Fatal("a groupless item blocked another groupless item")
	}
}

func TestBlocksIgnoresExpiredWindows(t *testing.T) {
	now := int64(1_000_000)
	stats := map[string]int64{StatKey(3037): now - 1, StatKey(3038): now + 5}
	if _, blocked := Blocks(stats, now, 3037, 0, nil); blocked {
		t.Fatal("an expired window still blocked the item")
	}
	if _, blocked := Blocks(stats, now, 3038, 0, nil); !blocked {
		t.Fatal("a running window did not block the item")
	}
	if active := ActiveEntries(stats, now); len(active) != 1 || active[0].ItemID != 3038 {
		t.Fatalf("active entries = %+v", active)
	}
	// 过期行惰性清理：清掉之后同一个 stats 里只剩还在跑的那一条。
	if !Prune(stats, now) {
		t.Fatal("Prune reported no change with an expired row present")
	}
	if _, found := stats[StatKey(3037)]; found {
		t.Fatalf("expired row survived prune: %+v", stats)
	}
	if got, found := ExpireAt(stats, 3038); !found || got != now+5 {
		t.Fatalf("running row lost by prune: %d found=%t", got, found)
	}
	if Prune(stats, now) {
		t.Fatal("Prune reported a change with nothing expired")
	}
}

func TestMillisecondsToSecondsRoundsUpSubSecondWindows(t *testing.T) {
	cases := []struct {
		milliseconds int64
		want         int64
	}{
		{milliseconds: 0, want: 0},
		{milliseconds: -5, want: 0},
		{milliseconds: 1, want: 1},
		{milliseconds: 999, want: 1},
		{milliseconds: 1_000, want: 1},
		{milliseconds: 10_000, want: 10},
		{milliseconds: 1_800_000, want: 1_800},
		{milliseconds: 86_400_000, want: 86_400},
		{milliseconds: 2_147_483_647, want: 2_147_484},
	}
	for _, testCase := range cases {
		if got := MillisecondsToSeconds(testCase.milliseconds); got != testCase.want {
			t.Fatalf("MillisecondsToSeconds(%d) = %d, want %d", testCase.milliseconds, got, testCase.want)
		}
	}
}

// Entries 只认自己的前缀：每日次数（item_daily_used_*）之类的邻居 stat 不能串进来。
func TestEntriesIgnoreForeignStatKeys(t *testing.T) {
	stats := map[string]int64{
		StatKey(3037):             5,
		"item_daily_used_3037":    2,
		"weakness_online_seconds": 900,
		"item_cooltime_":          7,
		"item_cooltime_abc":       7,
	}
	entries := Entries(stats)
	if len(entries) != 1 || entries[0].ItemID != 3037 || entries[0].ExpireAt != 5 {
		t.Fatalf("entries = %+v", entries)
	}
}
