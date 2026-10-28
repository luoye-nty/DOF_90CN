package itemdailylimit

import (
	"errors"
	"testing"
	"time"

	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/premium"
)

func TestConsumeWithoutDeclaredLimitTouchesNothing(t *testing.T) {
	stats := map[string]int64{}
	for _, limit := range []int64{0, -1} {
		used, err := Consume(stats, 490003149, limit, time.Now().UTC(), 90)
		if err != nil || used != 0 {
			t.Fatalf("limit=%d used=%d err=%v", limit, used, err)
		}
	}
	if len(stats) != 0 {
		t.Fatalf("没有 [total usable count] 的道具不应该写入任何东西: %+v", stats)
	}
}

func TestConsumeEnforcesTheDeclaredCap(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	stats := map[string]int64{dnffatigue.ItemUsageDayStatKey: premium.ServiceDayID(now)}
	const itemID, limit = int64(10153501), int64(3)

	for want := int64(1); want <= limit; want++ {
		used, err := Consume(stats, itemID, limit, now, 90)
		if err != nil || used != want {
			t.Fatalf("第 %d 次 used=%d err=%v", want, used, err)
		}
	}
	used, err := Consume(stats, itemID, limit, now, 90)
	var reached LimitReachedError
	if !errors.Is(err, ErrLimitReached) || !errors.As(err, &reached) {
		t.Fatalf("超过上限应返回可识别的拒绝: %v", err)
	}
	if reached.ItemID != itemID || reached.Used != limit || reached.Limit != limit {
		t.Fatalf("拒绝里带的数字不对: %+v", reached)
	}
	if used != limit || stats[Key(itemID)] != limit {
		t.Fatalf("被拒的一次不得改动计数: used=%d stats=%+v", used, stats)
	}
}

func TestConsumeAppliesTheServiceDayBoundary(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	const itemID, limit = int64(490003149), int64(2)
	stats := map[string]int64{
		// 昨天的计数已经打满，今天必须从 0 开始。
		dnffatigue.ItemUsageDayStatKey: premium.ServiceDayID(now.Add(-24 * time.Hour)),
		Key(itemID):                    limit,
		Key(490003150):                 5,
	}
	used, err := Consume(stats, itemID, limit, now, 90)
	if err != nil || used != 1 {
		t.Fatalf("跨 06:00 后第一次应该从 0 开始: used=%d err=%v stats=%+v", used, err, stats)
	}
	if stats[Key(490003150)] != 0 {
		t.Fatalf("同一天边界必须清掉其它道具的计数: %+v", stats)
	}
	if stats[dnffatigue.ItemUsageDayStatKey] != premium.ServiceDayID(now) {
		t.Fatalf("服务日没有推进: %+v", stats)
	}
}

func TestConsumeReadsTheLegacyFatiguePotionSpelling(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	const itemID, limit = int64(490702338), int64(3)
	stats := map[string]int64{
		dnffatigue.ItemUsageDayStatKey: premium.ServiceDayID(now),
		// 旧键属于疲劳药那一天的服务日，两个日键都要在，否则 ReconcileDailyStats
		// 会先把它当成"没初始化过的一天"清掉（既有语义，不是本模块引入的）。
		dnffatigue.PotionUsageDayStatKey: premium.ServiceDayID(now),
		// 通用键出现之前写下的旧计数不能被当成 0。
		LegacyKey(itemID): 2,
	}
	if got := Used(stats, itemID); got != 2 {
		t.Fatalf("旧键读数=%d want 2", got)
	}
	used, err := Consume(stats, itemID, limit, now, 90)
	if err != nil || used != 3 {
		t.Fatalf("应从旧计数继续: used=%d err=%v", used, err)
	}
	if _, err := Consume(stats, itemID, limit, now, 90); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("旧键 + 新键合计到上限后必须拒绝: %v", err)
	}
}

func TestReleaseGivesBackASingleReservedUse(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	const itemID, limit = int64(490003149), int64(2)
	stats := map[string]int64{dnffatigue.ItemUsageDayStatKey: premium.ServiceDayID(now)}
	if _, err := Consume(stats, itemID, limit, now, 90); err != nil {
		t.Fatal(err)
	}
	if got := Release(stats, itemID); got != 0 {
		t.Fatalf("回滚后计数=%d want 0", got)
	}
	// 没有可回滚的次数时不得把它做成负数。
	if got := Release(stats, itemID); got != 0 || stats[Key(itemID)] != 0 {
		t.Fatalf("重复回滚破坏了计数: got=%d stats=%+v", got, stats)
	}
}

func TestUsedToleratesHostileStats(t *testing.T) {
	if got := Used(nil, 1001); got != 0 {
		t.Fatalf("nil stats=%d", got)
	}
	if got := Used(map[string]int64{Key(1001): -5}, 1001); got != 0 {
		t.Fatalf("负数计数=%d", got)
	}
	if got := Used(map[string]int64{}, 0); got != 0 {
		t.Fatalf("零道具 id=%d", got)
	}
}
