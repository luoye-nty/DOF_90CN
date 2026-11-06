package dnfbridge

import "testing"

func TestDirectExperienceAdventureGainUsesOnlyExperienceBelowLevelCap(t *testing.T) {
	const capTotal = uint32(1_732_686_583)
	tests := []struct {
		name               string
		gain               uint32
		previousLevel      int
		newLevel           int
		previousExperience uint32
		capTotal           uint32
		want               uint32
	}{
		{
			name: "entire gain below cap",
			gain: 1_000_000, previousLevel: 20, newLevel: 30,
			previousExperience: 100, want: 1_000_000,
		},
		{
			name: "crossing into cap trims overflow",
			gain: 100_000_000, previousLevel: 89, newLevel: 90,
			previousExperience: capTotal - 32_686_583, capTotal: capTotal, want: 32_686_583,
		},
		{
			name: "already at cap contributes zero",
			gain: 9_999, previousLevel: 90, newLevel: 90,
			previousExperience: capTotal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := directExperienceAdventureGain(
				test.gain,
				test.previousLevel,
				test.newLevel,
				test.previousExperience,
				test.capTotal,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("gain=%d want=%d", got, test.want)
			}
		})
	}
}
