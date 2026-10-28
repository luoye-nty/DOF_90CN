package adventuregroup

import (
	"math"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestProjectActivitySumsRecommendedDungeonClearsAndSaturates(t *testing.T) {
	got := ProjectActivity(24, []dnfrepo.CharacterRecord{
		{Stats: map[string]int64{RecommendedDungeonClearStatKey: 7}},
		{Stats: map[string]int64{RecommendedDungeonClearStatKey: -1}},
		{Stats: map[string]int64{RecommendedDungeonClearStatKey: math.MaxUint32}},
	})
	if got.ConsecutiveLoginDays != 24 {
		t.Fatalf("consecutive days=%d, want 24", got.ConsecutiveLoginDays)
	}
	if got.ContentCounts[ContentTypeRecommendedDungeon] != math.MaxUint32 {
		t.Fatalf("recommended clears=%d, want %d", got.ContentCounts[ContentTypeRecommendedDungeon], math.MaxUint32)
	}
	for index := 1; index < ContentTypeCount; index++ {
		if got.ContentCounts[index] != 0 {
			t.Fatalf("unsupported content type %d=%d, want 0", index, got.ContentCounts[index])
		}
	}
}
