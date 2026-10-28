package statbuff

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 强化/增幅成功率窗口：与爆率窗口同一套 fail-closed 语义，但**按种类分开存**
// （强化与增幅是两件不同的道具，各自生效、互不覆盖）。
func TestRateWindowsAreKindScopedAndFailClosed(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	character := dnfrepo.CharacterRecord{CharacterID: "11"}

	if _, ok := ActiveRate(character, KindUpgradeSuccess, now); ok {
		t.Fatal("an empty character must not report an active upgrade window")
	}
	if _, ok := ActiveRate(character, Kind("unknown"), now); ok {
		t.Fatal("an unknown kind must not report an active window")
	}

	upgrade, err := ActivateRate(&character, KindUpgradeSuccess, 10008220, 1099, 30, 10*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	amplify, err := ActivateRate(&character, KindAmplifySuccess, 490001534, 0, 10, 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if upgrade.Percent != 30 || upgrade.BuffID != 1099 || !upgrade.ExpireAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("upgrade window=%+v", upgrade)
	}
	if amplify.Percent != 10 || amplify.BuffID != 0 || !amplify.ExpireAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("amplify window=%+v", amplify)
	}

	// 两族互不覆盖：读强化拿到的必须是 30/1099，读增幅拿到的是 10/0。
	gotUpgrade, ok := ActiveRate(character, KindUpgradeSuccess, now.Add(time.Minute))
	if !ok || gotUpgrade.Percent != 30 || gotUpgrade.BuffID != 1099 || gotUpgrade.ItemID != 10008220 {
		t.Fatalf("upgrade read=%+v ok=%t", gotUpgrade, ok)
	}
	gotAmplify, ok := ActiveRate(character, KindAmplifySuccess, now.Add(time.Minute))
	if !ok || gotAmplify.Percent != 10 || gotAmplify.ItemID != 490001534 {
		t.Fatalf("amplify read=%+v ok=%t", gotAmplify, ok)
	}

	// 过期即视为没有（fail-closed），另一种类不受影响。
	if _, ok := ActiveRate(character, KindUpgradeSuccess, now.Add(11*time.Minute)); ok {
		t.Fatal("an expired upgrade window must not stay active")
	}
	if _, ok := ActiveRate(character, KindAmplifySuccess, now.Add(11*time.Minute)); !ok {
		t.Fatal("the amplify window must survive the upgrade window expiring")
	}

	// 字段缺失/非法同样按"没有增益"处理，绝不 fail-open。
	broken := dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{
		"upgrade_success_rate_buff_percent": 10, // 没有到期时间
	}}
	if _, ok := ActiveRate(broken, KindUpgradeSuccess, now); ok {
		t.Fatal("a window without an expiry must not be active")
	}
	negative := dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{
		"amplify_success_rate_buff_percent":   -5,
		"amplify_success_rate_buff_expire_at": now.Add(time.Hour).Unix(),
	}}
	if _, ok := ActiveRate(negative, KindAmplifySuccess, now); ok {
		t.Fatal("a non-positive bonus must not be active")
	}
	if _, err := ActivateRate(&character, KindUpgradeSuccess, 0, 0, 10, time.Minute, now); err == nil {
		t.Fatal("activating with a zero item id must fail")
	}
	if _, err := ActivateRate(&character, KindUpgradeSuccess, 1, 0, 10, 0, now); err == nil {
		t.Fatal("activating with a zero duration must fail")
	}
}

func TestKindForUpgradeMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want Kind
		ok   bool
	}{
		{mode: "reinforce", want: KindUpgradeSuccess, ok: true},
		{mode: "amplify", want: KindAmplifySuccess, ok: true},
		{mode: " Reinforce ", want: KindUpgradeSuccess, ok: true},
		{mode: "", ok: false},
		{mode: "normal", ok: false},
	} {
		got, ok := KindForUpgradeMode(tc.mode)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("KindForUpgradeMode(%q)=%q,%t want %q,%t", tc.mode, got, ok, tc.want, tc.ok)
		}
	}
}
