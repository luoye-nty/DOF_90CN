package adventuregroup

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestExpeditionRewardNativeRoundingAndPartyAttributes(t *testing.T) {
	c := RuntimeConfig{
		ExpeditionAreas:        []ExpeditionArea{{Index: 1, RewardRates: map[uint32]float64{21600: 3.5}}},
		PartyMemberRewardRates: []float64{1, 1, 2, 3, 4}, ExpeditionExtraRate: 0.5,
		AttributeRewardRates: []float64{1, 1, 1.4, 1.8, 2.2},
		AttributeIDs:         map[string]byte{"a": 1, "b": 2},
		CharacterAttributes:  map[[2]byte][]string{{0, 2}: {"a"}, {1, 2}: {"b"}},
	}
	for _, test := range []struct {
		name       string
		members    []ExpeditionMemberInput
		attributes []byte
		want       uint32
	}{
		{"round_before_extra", []ExpeditionMemberInput{{Level: 90, Job: 0, GrowType: 0x22}}, nil, 2},
		{"level_is_not_multiplier", []ExpeditionMemberInput{{Level: 100, Job: 0, GrowType: 0x22}}, nil, 2},
		{"two_members_one_attribute", []ExpeditionMemberInput{{Level: 90, Job: 0, GrowType: 2}, {Level: 90, Job: 0, GrowType: 2}}, []byte{1, 2}, 3},
		{"two_members_two_attributes", []ExpeditionMemberInput{{Level: 90, Job: 0, GrowType: 2}, {Level: 90, Job: 1, GrowType: 0x22}}, []byte{1, 2}, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := c.ExpeditionReward(1, 21600, test.members, test.attributes)
			if !ok || got != test.want {
				t.Fatalf("reward=%d/%t want=%d", got, ok, test.want)
			}
		})
	}
	if _, ok := c.ExpeditionReward(1, 21600, nil, nil); ok {
		t.Fatal("empty party accepted")
	}
	if _, ok := c.ExpeditionReward(1, 1, []ExpeditionMemberInput{{Level: 90}}, nil); ok {
		t.Fatal("unsupported duration accepted")
	}
	clone := c.Clone()
	clone.PartyMemberRewardRates[1] = 7
	if c.PartyMemberRewardRates[1] != 1 {
		t.Fatal("clone changed original party rates")
	}
}

func TestRealPVFExpeditionRewardsMatchNativePreview(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	tables, err := LoadComplete(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	c := tables.Runtime()
	if c.ExpeditionExtraRate != 0.5 || len(c.PartyMemberRewardRates) != 5 {
		t.Fatalf("native bonus config=%+v", c)
	}
	want := [4][4]uint32{{1, 2, 3, 4}, {1, 2, 3, 5}, {1, 2, 4, 6}, {1, 3, 5, 6}}
	for _, area := range c.ExpeditionAreas {
		for timeIndex, hours := range []uint32{6, 12, 18, 24} {
			for count := 1; count <= 4; count++ {
				members := make([]ExpeditionMemberInput, count)
				for i := range members {
					members[i] = ExpeditionMemberInput{Level: 90, Job: 0, GrowType: 2}
				}
				got, ok := c.ExpeditionReward(area.Index, hours*3600, members, nil)
				if !ok || got != want[area.Index-1][timeIndex] {
					t.Fatalf("area=%d hours=%d members=%d reward=%d/%t", area.Index, hours, count, got, ok)
				}
				t.Logf("NATIVE_CASE rate=%g party=%g match=1 extra=%g duration=%d count=%d reward=%d", area.RewardRates[hours*3600], c.PartyMemberRewardRates[count], c.ExpeditionExtraRate, hours*3600, count, got)
			}
		}
	}
}
