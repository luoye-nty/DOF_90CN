package dnfbridge

import (
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

// 运营规则（2026-09-15 起）：每通关 10 次公会地下城发 1 个公会地下城奖励礼盒，
// 没有上限，也就是每通关一次涨 10%。（此前是 20 次 / 5%。）
func TestCurrentGuildDungeonRepeatBonusOwed(t *testing.T) {
	for _, row := range []struct {
		name   string
		before int64
		after  int64
		want   int64
	}{
		{name: "第一次通关", before: 0, after: 1, want: 0},
		{name: "第 9 次仍未满", before: 8, after: 9, want: 0},
		{name: "第 10 次发一个", before: 9, after: 10, want: 1},
		{name: "第 11 次不再发", before: 10, after: 11, want: 0},
		{name: "第 20 次再发一个（无上限）", before: 19, after: 20, want: 1},
		{name: "一次跨过两个整块就补两个", before: 9, after: 21, want: 2},
		{name: "计数没动就不发", before: 10, after: 10, want: 0},
		{name: "重放（after 落后）不发", before: 40, after: 20, want: 0},
		{name: "负数计数按 0 起算", before: -5, after: 1, want: 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := currentGuildDungeonRepeatBonusOwed(row.before, row.after); got != row.want {
				t.Fatalf("owed(%d -> %d) = %d, want %d", row.before, row.after, got, row.want)
			}
		})
	}
}

func TestCurrentGuildDungeonRepeatBonusPercent(t *testing.T) {
	for _, row := range []struct {
		count int64
		want  int64
	}{
		{count: 0, want: 0},
		{count: 1, want: 10},
		{count: 2, want: 20},
		{count: 5, want: 50},
		{count: 9, want: 90},
		{count: 10, want: 0},
		{count: 11, want: 10},
		{count: 21, want: 10},
	} {
		if got := currentGuildDungeonRepeatBonusPercent(row.count); got != row.want {
			t.Fatalf("percent(%d) = %d, want %d", row.count, got, row.want)
		}
	}
}

// 百分比必须是从次数阈值算出来的，不是另写的一个常量：改阈值时两者不能脱节。
func TestCurrentGuildDungeonRepeatBonusPercentTracksTheThreshold(t *testing.T) {
	step := 100 / currentGuildDungeonRepeatClearsPerBonus
	for clears := int64(1); clears < currentGuildDungeonRepeatClearsPerBonus; clears++ {
		if got, want := currentGuildDungeonRepeatBonusPercent(clears), clears*step; got != want {
			t.Fatalf("percent(%d) = %d, want %d", clears, got, want)
		}
	}
	if got := currentGuildDungeonRepeatBonusPercent(currentGuildDungeonRepeatClearsPerBonus); got != 0 {
		t.Fatalf("a completed block reads %d, want 0", got)
	}
}

// 礼盒与净化槽必须是两套独立的东西：槽只在 99%/100% 发公会贡献证书。
func TestCurrentGuildDungeonRepeatBonusIsNotThePurificationGauge(t *testing.T) {
	if currentGuildDungeonRepeatClearBonusItem == 10154439 {
		t.Fatal("repeat-clear bonus must not be the contribution certificate")
	}
	if currentGuildDungeonRepeatClearBonusItem != 10156320 {
		t.Fatalf("repeat-clear bonus item = %d, want 10156320 公会地下城奖励礼盒",
			currentGuildDungeonRepeatClearBonusItem)
	}
	if currentGuildDungeonRepeatClearsPerBonus != 10 {
		t.Fatalf("clears per bonus = %d, want 10", currentGuildDungeonRepeatClearsPerBonus)
	}
}

// 净化率翻倍只能在入账侧做：客户端的 clears * [gauge on clear] / [gauge limit]
// 在它自己的 PVF 里，服务端改不动。一次通关记 2 份槽，除回来就是 2 次，8500 的
// 1.5% 因此显示成 3%。
func TestCurrentGuildDungeonGaugeOnClearIsDoubled(t *testing.T) {
	stage := dnfguild.DungeonStage{GaugeOnClear: 150, GaugeLimit: 10000}
	if got := currentGuildDungeonGaugeOnClear(stage); got != 300 {
		t.Fatalf("gauge on clear = %d, want 300 (原版 150 的两倍)", got)
	}
	// 除回来给客户端的次数必须跟着翻倍，否则显示不会变。
	if got := currentGuildDungeonClears(stage, currentGuildDungeonGaugeOnClear(stage)); got != 2 {
		t.Fatalf("clears after one run = %d, want 2 so the client draws 3%%", got)
	}
	// 满槽仍然封顶在 100%，不会因为翻倍而画过头。
	if got := currentGuildDungeonClears(stage, 999999); got != stage.GaugeLimit/stage.GaugeOnClear {
		t.Fatalf("over-credited gauge rendered %d clears, want the full-bar clamp", got)
	}
}

func TestCurrentGuildDungeonGaugeOnClearHandlesDegenerateStages(t *testing.T) {
	if got := currentGuildDungeonGaugeOnClear(dnfguild.DungeonStage{}); got != 0 {
		t.Fatalf("a stage with no gauge credited %d, want 0", got)
	}
	// 翻倍不能溢出成一个更小的数。
	huge := dnfguild.DungeonStage{GaugeOnClear: ^uint32(0) - 1}
	if got := currentGuildDungeonGaugeOnClear(huge); got != huge.GaugeOnClear {
		t.Fatalf("overflowing stage credited %d, want the un-doubled %d", got, huge.GaugeOnClear)
	}
}
