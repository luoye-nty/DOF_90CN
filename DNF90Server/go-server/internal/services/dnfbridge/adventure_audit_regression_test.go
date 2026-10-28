package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The four native tower counters are read by the client at body+6..9 of the
// adventure-group info push and at +22..25 of the actor refresh. Both are u8
// fields, so the value saturates rather than wrapping.
func TestBuildCurrentAdventureInfoBodyWritesTowerCounters(t *testing.T) {
	for _, roster := range []struct {
		name       string
		characters []dnfrepo.CharacterRecord
	}{
		{name: "empty"},
		{name: "populated", characters: []dnfrepo.CharacterRecord{{
			CharacterID: "20", Name: "冒险家", Job: "11", Level: 90,
			Stats: map[string]int64{"grow_type": 0x22, "server_group_id": 2},
		}}},
	} {
		for _, check := range []struct {
			name      string
			remaining [adventuregroup.TowerTypeCount]uint32
			limits    [adventuregroup.TowerTypeCount]uint32
			want      []byte
		}{
			{"available", [2]uint32{2, 1}, [2]uint32{3, 1}, []byte{2, 3, 1, 1}},
			{"exhausted", [2]uint32{0, 0}, [2]uint32{3, 1}, []byte{0, 3, 0, 1}},
			{"byte_limit", [2]uint32{255, 254}, [2]uint32{255, 255}, []byte{255, 255, 254, 255}},
			{"saturate", [2]uint32{256, 0}, [2]uint32{300, ^uint32(0)}, []byte{255, 255, 0, 255}},
		} {
			t.Run(roster.name+"/"+check.name, func(t *testing.T) {
				projection := adventuregroup.Projection{TowerRemaining: check.remaining, TowerLimits: check.limits}
				body := buildCurrentAdventureInfoBodyWithState(20, adventuregroup.Summary{}, roster.characters, "group", 20260806, projection)
				if got := body[6:10]; !bytes.Equal(got, check.want) {
					t.Errorf("native tower bytes=%v, want %v", got, check.want)
				}
				actor := buildCurrentAdventureActorRefreshBody(20, adventuregroup.Summary{}, projection)
				if got := actor[22:26]; !bytes.Equal(got, check.want) {
					t.Errorf("native actor refresh tower bytes=%v, want %v", got, check.want)
				}
			})
		}
	}
}

// The expedition attribute set rotates on the UTC+8 local midnight, not on UTC
// midnight: the client's preview and the server's paid reward must agree on the
// same day boundary.
func TestAreaAttributesRotateOnUTC8Midnight(t *testing.T) {
	c := adventuregroup.RuntimeConfig{
		ExpeditionAreas: []adventuregroup.ExpeditionArea{{Index: 1, DesignatedGroups: []string{"g"}}},
		AttributeGroups: map[string][]string{"g": {"a", "b", "c"}},
		AttributeIDs:    map[string]byte{"a": 1, "b": 2, "c": 3}, RotationDays: 1,
	}
	before := time.Date(2026, 9, 13, 15, 59, 59, 0, time.UTC)
	a, b := c.AreaAttributes(1, before), c.AreaAttributes(1, before.Add(time.Second))
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("local midnight did not rotate: %v -> %v", a, b)
	}
	utcMidnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	if got := c.AreaAttributes(1, utcMidnight); got[0] != b[0] {
		t.Fatalf("rotated again at UTC midnight: %v -> %v", b, got)
	}
}

// The client reads its character list from the native idle table in area 0, and
// identifies an already-sent character by durable ID rather than by roster slot.
func TestAdventureExpeditionNativeCandidatesAndIdentity(t *testing.T) {
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "1", Slot: 0, Job: "0", Level: 90, Name: "A", Stats: map[string]int64{"grow_type": 0x21}},
		{CharacterID: "2", Slot: 1, Job: "0", Level: 90, Name: "B", Stats: map[string]int64{"grow_type": 0x22}},
	}
	requests := []currentAdventureExpeditionMemberRequest{{Selector: 0, Job: 0, GrowType: 1}, {Selector: 1, Job: 0, GrowType: 2}}
	members, _, ok := resolveCurrentAdventureExpeditionMembers(requests, characters, adventuregroup.RuntimeState{})
	if !ok || len(members) != 2 || members[0].PersistentCharacterID != "1" || members[1].PersistentCharacterID != "2" {
		t.Fatalf("members=%+v accepted=%t", members, ok)
	}

	state := currentAdventureSelectedState{Characters: characters, Config: adventuregroup.RuntimeConfig{
		ExpeditionAreas: []adventuregroup.ExpeditionArea{{Index: 1}},
	}}
	projection := currentAdventureExpeditionProjection(state, time.Unix(100, 0))
	if len(projection.Expeditions["0"].Members) != 2 || len(state.Runtime.Expeditions) != 0 {
		t.Fatalf("idle candidates/projection mutated source: %+v", projection)
	}
	wire := buildCurrentAdventureExpeditionStateBody(projection, time.Unix(100, 0))
	if len(wire) < 14 || wire[0] != 2 || wire[1] != 0 || wire[2] != 0 || binary.LittleEndian.Uint16(wire[12:14]) != 2 {
		t.Fatalf("native idle area header=%x", wire[:14])
	}

	state.Runtime.Expeditions = map[string]adventuregroup.Expedition{"1": {Area: 1, State: 1, Members: members[:1]}}
	characters[0].Slot = 3
	requests[0].Selector = 3
	if _, _, ok := resolveCurrentAdventureExpeditionMembers(requests[:1], characters, state.Runtime); ok {
		t.Fatal("roster reordering bypassed occupied persistent identity")
	}
	if got := currentAdventureExpeditionProjection(state, time.Unix(100, 0)); len(got.Expeditions["0"].Members) != 1 {
		t.Fatal("occupied character remained in idle candidates")
	}
	requests[0] = currentAdventureExpeditionMemberRequest{Selector: 1, Job: 0, GrowType: 1}
	if _, _, ok := resolveCurrentAdventureExpeditionMembers(requests[:1], characters, adventuregroup.RuntimeState{}); ok {
		t.Fatal("numeric character ID was accepted as a different slot")
	}
	requests = []currentAdventureExpeditionMemberRequest{{Selector: 1, Job: 0, GrowType: 2}, {Selector: 1, Job: 0, GrowType: 2}}
	if _, _, ok := resolveCurrentAdventureExpeditionMembers(requests, characters, adventuregroup.RuntimeState{}); ok {
		t.Fatal("duplicate persistent character accepted")
	}
}
