package profession

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestApplyGrowupExperienceFreeAndNoCountAdvance(t *testing.T) {
	ctx := context.Background()
	profiles, catalog := growupChangeProfessionResources(t)
	if profiles == nil || catalog == nil {
		t.Fatal("profession resources unexpectedly nil")
	}
	repos := growupChangeSeed(t, ctx, func(r *dnfrepo.CharacterRecord) {
		r.Level = 20
		r.Stats = map[string]int64{"gold": 50000, "grow_type": 1, "growup_change_count": 1}
	})

	result, err := ApplyGrowupExperience(ctx, repos, growupChangeInput(2))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.ResultCode != GrowupChangeAckResultCodeSuccess {
		t.Fatalf("result=%+v", result)
	}
	if result.NewGrowType != 2 {
		t.Fatalf("new grow type=%d want 2", result.NewGrowType)
	}
	if result.GoldCost != 0 || result.UpdatedGold != 50000 {
		t.Fatalf("experience must be free: result=%+v", result)
	}
	if result.NewChangeCount != 1 || result.PreviousChangeCount != 1 {
		t.Fatalf("experience must not advance change count: result=%+v", result)
	}
	persisted, ok, err := repos.Character.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load persisted ok=%t err=%v", ok, err)
	}
	if persisted.Stats["grow_type"] != 2 || persisted.Stats["growup_change_count"] != 1 || persisted.Stats["gold"] != 50000 {
		t.Fatalf("persisted stats=%+v", persisted.Stats)
	}
}

func TestApplyGrowupExperienceRejectsByState(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		mutate        func(r *dnfrepo.CharacterRecord)
		target        byte
		wantAck       int
		wantGrowType  int64
		wantGrowCount int64
	}{
		{name: "level-below-min", mutate: func(r *dnfrepo.CharacterRecord) { r.Level = 14 }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 1},
		{name: "level-above-max", mutate: func(r *dnfrepo.CharacterRecord) { r.Level = 50 }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 1},
		{name: "not-transferred", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 0} }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 0},
		{name: "already-awakened", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 0x11} }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 0x11},
		{name: "target-unchanged", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 1} }, target: 1, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 1},
		{name: "target-unsupported", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 1} }, target: 5, wantAck: GrowupChangeAckResultCodeInvalidState, wantGrowType: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := growupChangeSeed(t, ctx, tc.mutate)
			result, err := ApplyGrowupExperience(ctx, repos, growupChangeInput(tc.target))
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.ResultCode != tc.wantAck {
				t.Fatalf("result=%+v", result)
			}
			persisted, ok, err := repos.Character.Load(ctx, "19")
			if err != nil || !ok {
				t.Fatalf("load persisted ok=%t err=%v", ok, err)
			}
			if persisted.Stats["grow_type"] != tc.wantGrowType {
				t.Fatalf("rejected experience mutated grow_type stats=%+v", persisted.Stats)
			}
			if persisted.Stats["growup_change_count"] != tc.wantGrowCount {
				t.Fatalf("rejected experience advanced change count stats=%+v", persisted.Stats)
			}
		})
	}
}
