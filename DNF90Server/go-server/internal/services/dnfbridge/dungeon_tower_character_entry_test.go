package dnfbridge

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The two towers consume their per-character daily entry independently, and the
// 06:00 service-day boundary restores each of them on its own.
func TestTowerCharacterMode1PreservesIndependentDailyEntries(t *testing.T) {
	ctx := context.Background()
	reset := time.Date(2026, 9, 15, 6, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	for _, tc := range []struct {
		name          string
		usedDay       int64
		now           time.Time
		wantRemaining byte
	}{
		{"unused", 0, reset.Add(-time.Second), 1},
		{"cleared_before_relogin", premium.ServiceDayID(reset.Add(-time.Hour)), reset.Add(-time.Second), 0},
		{"next_service_day", premium.ServiceDayID(reset.Add(-time.Hour)), reset, 1},
	} {
		for _, kind := range []string{"despair", "grief"} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				service := &Service{characterStats: testCharacterStatTable(t),
					gameplayTimers: &processGameplayTimeQueue{now: func() time.Time { return tc.now }}}
				character := dnfrepo.CharacterRecord{CharacterID: "19", Level: 1,
					Stats: map[string]int64{"tower_" + kind + "_last_clear_at": reset.Add(-time.Hour).Unix()}}
				if tc.usedDay != 0 {
					character.Stats["tower_"+kind+"_character_entry_day"] = tc.usedDay
				}
				body := service.buildCurrentSelectedUserInfoMode1Body(ctx, nil, nil, character, true, 19)
				if len(body) != currentMode1BaseWireSize {
					t.Fatalf("mode1 shape changed: len=%d", len(body))
				}
				// Native sub_2002BE0 reads flag B after resources, creature level,
				// the empty dungeon-count list and flag A, then stores !flagB.
				resourceOffset := currentMode1CreateRowsOffset + 4 + 2 + 1 + 8
				flag := body[resourceOffset+6]
				if kind == "grief" {
					flag = body[len(body)-1]
				}
				remaining := byte(0)
				if flag == 0 {
					remaining = 1
				}
				if remaining != tc.wantRemaining {
					t.Fatalf("native character entry=%d/1 want=%d/1 flagB=%d", remaining, tc.wantRemaining, flag)
				}
				otherFlag := body[len(body)-1]
				if kind == "grief" {
					otherFlag = body[resourceOffset+6]
				}
				if otherFlag != 0 {
					t.Fatal("one tower consumed the other tower's character entry")
				}
			})
		}
	}
}
