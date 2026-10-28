package dnfbridge

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 服主 2026-10-03 的口径："决定是否出史诗前，如果用了药剂且在持续时间内，就把出史诗的
// 可能性翻一倍"。本用例锁住这条算术在**掉落路径真正调用的那个函数**上的行为。
func TestCurrentHellPartyEpicDropRatePercentAppliesThePotionWindow(t *testing.T) {
	now := time.Now().UTC()
	buffStats := func(expireAt time.Time) map[string]int64 {
		return map[string]int64{
			statbuff.EpicDropRatePercentStatKey: 200,
			statbuff.EpicDropRateExpireStatKey:  expireAt.Unix(),
			statbuff.EpicDropRateItemStatKey:    490002458,
		}
	}
	cases := []struct {
		name  string
		gm    int
		stats map[string]int64
		want  int
	}{
		{name: "no potion keeps the GM percent", gm: 100, stats: nil, want: 100},
		{name: "active 2x doubles it", gm: 100, stats: buffStats(now.Add(time.Hour)), want: 200},
		{name: "expired window is ignored", gm: 100, stats: buffStats(now.Add(-time.Minute)), want: 100},
		{name: "GM 300 with a 2x potion becomes 600", gm: 300, stats: buffStats(now.Add(time.Hour)), want: 600},
		{name: "GM 0 stays 0 (the switch still wins)", gm: 0, stats: buffStats(now.Add(time.Hour)), want: 0},
		{name: "clamped to the GM ceiling", gm: gmHellEpicDropRateMaximumPercent, stats: buffStats(now.Add(time.Hour)), want: gmHellEpicDropRateMaximumPercent},
		{name: "broken state fails closed", gm: 100, stats: map[string]int64{
			statbuff.EpicDropRatePercentStatKey: 200,
		}, want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{gmGameplaySettingsReady: true, gmHellEpicDropRatePct: tc.gm}
			character := dnfrepo.CharacterRecord{CharacterID: "11", Stats: tc.stats}
			if got := service.currentHellPartyEpicDropRatePercent(character); got != tc.want {
				t.Fatalf("percent=%d want=%d", got, tc.want)
			}
		})
	}
}

// TestCurrentHellPartyEpicDropRatePercentHonoursTheTemporaryTestMultiplier：
// 服主要"先改一下倍率，用来肉眼确认药剂真的生效，后面再改回从 PVF 读取"。
// 那个旋钮（GM 设置 epic_drop_rate_potion_test_multiplier_percent）的行为必须被锁住：
//
//	① 它**只替换倍数**，绝不绕过"药剂窗口仍在生效"这一前置条件（过期/没吃药 = 不生效）；
//	② 0 = 关闭，回到 PVF 表里查出来的倍数；
//	③ 换档到别的角色记录（别人没吃药）不受影响 —— 窗口是角色级的。
func TestCurrentHellPartyEpicDropRatePercentHonoursTheTemporaryTestMultiplier(t *testing.T) {
	now := time.Now().UTC()
	stats := map[string]int64{
		statbuff.EpicDropRatePercentStatKey: 200,
		statbuff.EpicDropRateExpireStatKey:  now.Add(time.Hour).Unix(),
		statbuff.EpicDropRateItemStatKey:    10008219,
	}
	expiredStats := map[string]int64{
		statbuff.EpicDropRatePercentStatKey: 200,
		statbuff.EpicDropRateExpireStatKey:  now.Add(-time.Second).Unix(),
		statbuff.EpicDropRateItemStatKey:    10008219,
	}
	cases := []struct {
		name           string
		gm             int
		testMultiplier int
		stats          map[string]int64
		want           int
	}{
		{name: "override off uses the PVF multiplier", gm: 100, testMultiplier: 0, stats: stats, want: 200},
		{name: "override 2000 turns 2x into 20x", gm: 100, testMultiplier: 2000, stats: stats, want: 2000},
		{name: "override still needs an active window", gm: 100, testMultiplier: 2000, stats: expiredStats, want: 100},
		{name: "override without a potion at all is inert", gm: 100, testMultiplier: 2000, stats: nil, want: 100},
		{name: "override respects the GM ceiling", gm: gmHellEpicDropRateMaximumPercent, testMultiplier: 2000,
			stats: stats, want: gmHellEpicDropRateMaximumPercent},
		{name: "override with GM 0 stays 0", gm: 0, testMultiplier: 2000, stats: stats, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{
				gmGameplaySettingsReady:               true,
				gmHellEpicDropRatePct:                 tc.gm,
				gmEpicDropRatePotionTestMultiplierPct: tc.testMultiplier,
			}
			character := dnfrepo.CharacterRecord{CharacterID: "11", Stats: tc.stats}
			if got := service.currentHellPartyEpicDropRatePercent(character); got != tc.want {
				t.Fatalf("percent=%d want=%d (gm=%d test=%d)", got, tc.want, tc.gm, tc.testMultiplier)
			}
		})
	}
}

// TestScaleHellEpicDropRateByPotionIsPlainMultiplication：纯函数口径（百分比整数运算）。
func TestScaleHellEpicDropRateByPotionIsPlainMultiplication(t *testing.T) {
	for _, tc := range []struct {
		percent    int
		multiplier int
		want       int
	}{
		{percent: 100, multiplier: 200, want: 200},
		{percent: 100, multiplier: 2000, want: 2000},
		{percent: 300, multiplier: 200, want: 600},
		{percent: 0, multiplier: 2000, want: 0},
		{percent: 100, multiplier: 0, want: 0},
	} {
		if got := scaleHellEpicDropRateByPotion(tc.percent, tc.multiplier); got != tc.want {
			t.Fatalf("scale(%d,%d)=%d want=%d", tc.percent, tc.multiplier, got, tc.want)
		}
	}
	if got := clampHellEpicDropRatePercent(gmHellEpicDropRateMaximumPercent + 1); got != gmHellEpicDropRateMaximumPercent {
		t.Fatalf("clamp high=%d", got)
	}
	if got := clampHellEpicDropRatePercent(-1); got != gmHellEpicDropRateMinimumPercent {
		t.Fatalf("clamp low=%d", got)
	}
}
