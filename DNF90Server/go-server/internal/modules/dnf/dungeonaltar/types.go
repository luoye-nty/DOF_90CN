package dungeonaltar

import "errors"

var (
	ErrOwnerUnavailable = errors.New("dungeon altar owner is unavailable")
	ErrCommandInvalid   = errors.New("dungeon altar command is invalid")
	ErrCharacterMissing = errors.New("dungeon altar character is missing")
	ErrGoldInsufficient = errors.New("dungeon altar gold is insufficient")
	ErrDailyLimit       = errors.New("dungeon altar daily limit is exhausted")
	ErrReceiptRestored  = errors.New("dungeon altar admission receipt was restored")
	ErrReceiptMismatch  = errors.New("dungeon altar admission receipt does not match durable state")
)

type PriceBand struct {
	MinimumLevel uint16
	MaximumLevel uint16
	Price        int64
}

type AdmissionIncrease struct {
	UpperExclusiveEnterCount uint32
	Percent                  int64
}

type LottoPoint struct {
	UpperExclusiveEnterCount uint32
	Points                   uint32
}

type AdmissionRule struct {
	PriceBands        []PriceBand
	AdmissionIncrease []AdmissionIncrease
	LottoPoints       []LottoPoint
	LottoCost         int64
	DailyLimit        uint32
}

type AdmissionSnapshot struct {
	CharacterID      string
	Fee              int64
	GoldAfter        int64
	DailyEnterCount  uint32
	WeeklyEnterCount uint32
	LottoPointEarned uint32
	LottoGoldSpent   int64
	TotalLottoPoints uint32
	TotalLottoGold   int64
	Replayed         bool
}

type RecordSnapshot struct {
	DungeonID      uint32
	BestRound      uint32
	BestTimeMillis uint32
	Changed        bool
	Replayed       bool
}
