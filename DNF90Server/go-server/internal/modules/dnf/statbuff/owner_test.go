package statbuff

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestActivateEpicDropRateStoresWindowAndExpires(t *testing.T) {
	now := time.Date(2026, time.October, 3, 18, 0, 0, 0, time.UTC)
	character := dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{}}
	effect, err := ActivateEpicDropRate(&character, 490002458, 1136, 200, 10*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if effect.MultiplierPercent != 200 || effect.ItemID != 490002458 ||
		!effect.ExpireAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("effect=%+v", effect)
	}
	if character.Stats[EpicDropRatePercentStatKey] != 200 ||
		character.Stats[EpicDropRateItemStatKey] != 490002458 {
		t.Fatalf("stats=%v", character.Stats)
	}

	active, ok := ActiveEpicDropRate(character, now.Add(9*time.Minute))
	if !ok || active.MultiplierPercent != 200 || active.ItemID != 490002458 {
		t.Fatalf("within window active=%t effect=%+v", ok, active)
	}
	// 边界：正好到期不算有效（"持续10分钟"），过期更不算。
	if _, ok := ActiveEpicDropRate(character, now.Add(10*time.Minute)); ok {
		t.Fatalf("exactly-expired window must not be active")
	}
	if _, ok := ActiveEpicDropRate(character, now.Add(11*time.Minute)); ok {
		t.Fatalf("expired window must not be active")
	}
}

func TestActiveEpicDropRateFailsClosedOnMissingOrBrokenState(t *testing.T) {
	now := time.Date(2026, time.October, 3, 18, 0, 0, 0, time.UTC)
	cases := map[string]dnfrepo.CharacterRecord{
		"nil stats":        {CharacterID: "11"},
		"no percent":       {CharacterID: "11", Stats: map[string]int64{EpicDropRateExpireStatKey: now.Add(time.Hour).Unix()}},
		"no expire":        {CharacterID: "11", Stats: map[string]int64{EpicDropRatePercentStatKey: 200}},
		"zero percent":     {CharacterID: "11", Stats: map[string]int64{EpicDropRatePercentStatKey: 0, EpicDropRateExpireStatKey: now.Add(time.Hour).Unix()}},
		"negative percent": {CharacterID: "11", Stats: map[string]int64{EpicDropRatePercentStatKey: -200, EpicDropRateExpireStatKey: now.Add(time.Hour).Unix()}},
		"negative expire":  {CharacterID: "11", Stats: map[string]int64{EpicDropRatePercentStatKey: 200, EpicDropRateExpireStatKey: -5}},
		"zero expire":      {CharacterID: "11", Stats: map[string]int64{EpicDropRatePercentStatKey: 200, EpicDropRateExpireStatKey: 0}},
	}
	for name, character := range cases {
		if _, ok := ActiveEpicDropRate(character, now); ok {
			t.Fatalf("%s: must not be active", name)
		}
	}
}

// TestActivateEpicDropRateOverwritesInsteadOfStacking 锁住"不与其他史诗Buff药剂叠加"：
// 第二次施加是**覆盖**，绝不会把倍率乘起来。
func TestActivateEpicDropRateOverwritesInsteadOfStacking(t *testing.T) {
	now := time.Date(2026, time.October, 3, 18, 0, 0, 0, time.UTC)
	character := dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{}}
	if _, err := ActivateEpicDropRate(&character, 490002458, 1136, 200, 10*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(5 * time.Minute)
	effect, err := ActivateEpicDropRate(&character, 490006963, 1136, 200, 10*time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if effect.MultiplierPercent != 200 {
		t.Fatalf("percent=%d, 叠加会得到 400", effect.MultiplierPercent)
	}
	active, ok := ActiveEpicDropRate(character, later.Add(time.Minute))
	if !ok || active.MultiplierPercent != 200 || active.ItemID != 490006963 {
		t.Fatalf("active=%t effect=%+v", ok, active)
	}
	if !active.ExpireAt.Equal(later.Add(10 * time.Minute)) {
		t.Fatalf("expire=%s, 覆盖时窗口要按最后一次使用重新计时", active.ExpireAt)
	}
}

func TestActivateEpicDropRateRejectsInvalidInput(t *testing.T) {
	now := time.Date(2026, time.October, 3, 18, 0, 0, 0, time.UTC)
	character := dnfrepo.CharacterRecord{CharacterID: "11"}
	for _, tc := range []struct {
		name     string
		target   *dnfrepo.CharacterRecord
		itemID   int64
		percent  int64
		duration time.Duration
	}{
		{name: "nil character", target: nil, itemID: 1, percent: 200, duration: time.Minute},
		{name: "zero item", target: &character, itemID: 0, percent: 200, duration: time.Minute},
		{name: "zero percent", target: &character, itemID: 1, percent: 0, duration: time.Minute},
		{name: "zero duration", target: &character, itemID: 1, percent: 200},
	} {
		if _, err := ActivateEpicDropRate(tc.target, tc.itemID, 0, tc.percent, tc.duration, now); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
	}
}
