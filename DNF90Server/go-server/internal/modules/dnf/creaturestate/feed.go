package creaturestate

import (
	"errors"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrFeedEntryInvalid = errors.New("pet feed creature state is invalid")
	ErrFeedDeltaInvalid = errors.New("pet feed satiety delta is invalid")
)

// FeedUpdate is the pure, capped satiety change from one PVF-proven feed.
// Fractional satiety is retained so feeding cannot erase elapsed-time state.
type FeedUpdate struct {
	BeforeSatiety       byte
	BeforeSatietyMicros int64
	AfterSatiety        byte
	AfterSatietyMicros  int64
	RequestedDelta      int64
	AppliedDelta        int
	Changed             bool
}

// ApplyFeed increases satiety by the PVF [feed] int-data amount and caps the
// authoritative micro gauge at 100. Source-item consumption remains owned by
// the caller's character-pet transaction.
func ApplyFeed(entry dnfrepo.PetEntry, satietyDelta int64) (FeedUpdate, error) {
	if entry.ItemID <= 0 || entry.Satiety > 100 || entry.SatietyMicros < 0 || entry.SatietyMicros > fullSatietyMicros {
		return FeedUpdate{}, fmt.Errorf("%w: item=%d satiety=%d micros=%d", ErrFeedEntryInvalid, entry.ItemID, entry.Satiety, entry.SatietyMicros)
	}
	if satietyDelta <= 0 || satietyDelta > 100 {
		return FeedUpdate{}, fmt.Errorf("%w: %d", ErrFeedDeltaInvalid, satietyDelta)
	}
	beforeMicros := entry.SatietyMicros
	if beforeMicros == 0 && entry.Satiety > 0 {
		beforeMicros = int64(entry.Satiety) * 1_000_000
	}
	afterMicros := beforeMicros + satietyDelta*1_000_000
	if afterMicros > fullSatietyMicros {
		afterMicros = fullSatietyMicros
	}
	afterSatiety := byte(afterMicros / 1_000_000)
	return FeedUpdate{
		BeforeSatiety:       entry.Satiety,
		BeforeSatietyMicros: beforeMicros,
		AfterSatiety:        afterSatiety,
		AfterSatietyMicros:  afterMicros,
		RequestedDelta:      satietyDelta,
		AppliedDelta:        int(afterSatiety) - int(entry.Satiety),
		Changed:             afterMicros != beforeMicros,
	}, nil
}
