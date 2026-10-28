package creaturestate

import (
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestApplyFeedPreservesFractionalSatietyAndCapsAtFull(t *testing.T) {
	entry := dnfrepo.PetEntry{ItemID: 400990168, Satiety: 2, SatietyMicros: 2_567_020}
	update, err := ApplyFeed(entry, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !update.Changed || update.AfterSatiety != 32 || update.AfterSatietyMicros != 32_567_020 || update.AppliedDelta != 30 {
		t.Fatalf("feed update = %+v", update)
	}
	entry.Satiety = update.AfterSatiety
	entry.SatietyMicros = update.AfterSatietyMicros
	update, err = ApplyFeed(entry, 100)
	if err != nil {
		t.Fatal(err)
	}
	if update.AfterSatiety != 100 || update.AfterSatietyMicros != 100_000_000 || update.AppliedDelta != 68 {
		t.Fatalf("capped feed update = %+v", update)
	}
	if _, err := ApplyFeed(entry, 0); !errors.Is(err, ErrFeedDeltaInvalid) {
		t.Fatalf("zero feed delta error = %v", err)
	}
}
