package dnfbridge

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCharacterRosterProjectsPersistedPVPEntryQuest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress string
		state    string
		want     byte
	}{
		{name: "missing"},
		{name: "active", state: "active"},
		{name: "completed", state: "completed", want: 1},
		{name: "legacy completed", progress: "cleared", want: 1},
		{name: "canonical active overrides legacy", progress: "completed", state: "active"},
		{name: "canonical completed overrides legacy", progress: "active", state: "done", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			character := dnfrepo.CharacterRecord{
				CharacterID: "16", AccountID: "pvp-quest", Name: "hero", Job: "0", Level: 90,
				Roster: dnfrepo.CharacterRoster{Entry: dnfrepo.CharacterRosterEntry{Flag6Eq1: 1}},
			}
			if err := repos.Character.Save(ctx, character); err != nil {
				t.Fatal(err)
			}
			quest := dnfrepo.QuestRecord{CharacterID: "16", States: map[int64]dnfrepo.QuestState{}, Progress: map[int64]dnfrepo.QuestState{}}
			if tc.progress != "" {
				quest.Progress[2670] = dnfrepo.QuestState{Status: tc.progress}
			}
			if tc.state != "" {
				quest.States[2670] = dnfrepo.QuestState{Status: tc.state}
			}
			if tc.name != "missing" {
				if err := repos.Quest.Save(ctx, quest); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeFound, err := repos.Quest.Load(ctx, "16")
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{}
			rows, _, err := s.listCharactersWithLimit(ctx, repos, "pvp-quest")
			if err != nil || len(rows) != 1 {
				t.Fatalf("list characters: rows=%d err=%v", len(rows), err)
			}
			initial := buildNoPackRosterBody(rows)
			rows[0].Slot = 36
			extension, ok := buildNoPackRosterExtensionBodyForLimit(rows, 37, 36)
			if !ok {
				t.Fatal("extension missing")
			}
			for _, packet := range []struct {
				body  []byte
				start int
			}{{initial, 15}, {extension, 5}} {
				start := noPackRosterPostEquipStart(packet.start, rows[0])
				got := packet.body[start : start+38]
				want := append(make([]byte, 24), 3, 0, 0, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, tc.want)
				if !bytes.Equal(got, want) {
					t.Fatalf("native roster tail=%x want=%x", got, want)
				}
			}
			after, afterFound, err := repos.Quest.Load(ctx, "16")
			if err != nil || beforeFound != afterFound || !reflect.DeepEqual(before, after) {
				t.Fatalf("roster mutated quest state: err=%v", err)
			}
			stored, _, err := repos.Character.Load(ctx, "16")
			if err != nil || stored.Roster.PVPEntryQuestCompleted {
				t.Fatalf("roster projection persisted: err=%v", err)
			}
		})
	}
}
