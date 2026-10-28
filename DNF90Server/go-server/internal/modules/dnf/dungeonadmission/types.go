package dungeonadmission

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrOwnerUnavailable = errors.New("dungeon admission counter owner is unavailable")
	ErrCommandInvalid   = errors.New("dungeon admission counter command is invalid")
	ErrLimitReached     = errors.New("dungeon admission counter limit reached")
	ErrReceiptRestored  = errors.New("dungeon admission counter receipt was restored")
	ErrReceiptState     = errors.New("dungeon admission counter receipt state is inconsistent")
	ErrAllowanceLimit   = errors.New("dungeon admission allowance limit reached")
	ErrCharacterMissing = errors.New("dungeon admission counter character is missing")
	ErrAccountMissing   = errors.New("dungeon admission counter account is missing")
)

// Scope identifies the durable owner of one admission counter. Character and
// account limits may both apply to the same dungeon; callers must pass every
// policy returned by Catalog.PoliciesForDungeon to Preflight and Commit.
type Scope byte

const (
	ScopeCharacter Scope = iota + 1
	ScopeAccount
)

// Period identifies the service-time window used by one counter.
type Period byte

const (
	PeriodDaily Period = iota + 1
	PeriodWeekly
)

// Policy is one typed, PVF-backed admission limit. Key is a stable semantic
// identity shared by all dungeons that consume the same counter.
type Policy struct {
	Key           string
	SourcePath    string
	SourceSection string
	Scope         Scope
	Period        Period
	Limit         uint32
	ResetWeekday  time.Weekday
	DungeonIDs    []uint32
}

// Window is the exact Beijing service-time interval owning a counter value.
// Daily and weekly boundaries both roll at 06:00.
type Window struct {
	Start time.Time
	End   time.Time
}

// Snapshot is the authoritative counter projection before or after a commit.
type Snapshot struct {
	Policy         Policy
	Window         Window
	Used           uint32
	Bonus          uint32
	EffectiveLimit uint32
	Remaining      uint32
	Replayed       bool
	Restored       bool
}

// AllowanceGrant adds a bounded, service-window-scoped amount to one authored
// admission policy. The base Policy limit remains immutable; Preflight and
// Commit use Policy.Limit plus the durable Bonus until the next reset window.
type AllowanceGrant struct {
	Policy       Policy
	Amount       uint32
	MaximumBonus uint32
}

// AllowanceUseLimit is an optional per-character daily-use cap for one ticket
// family. It is independent of the source inventory slot so moving or sorting
// the ticket cannot reset the count.
type AllowanceUseLimit struct {
	Key   string
	Limit uint32
}

// AllowanceResult describes one committed ticket grant.
type AllowanceResult struct {
	Snapshots     []Snapshot
	Uses          uint32
	RemainingUses uint32
}

// TowerFamily identifies one current-PVF single-floor tower progression.
// The ordered dungeon list is authored by [each layer matching dungeon].
type TowerFamily string

const (
	TowerFamilyDespair TowerFamily = "tower_of_despair"
	TowerFamilyGrief   TowerFamily = "tower_of_grief"
)

// TowerUseItem is one item/count pair carried by the current client's tower
// entry projection. Negative counts are authored sentinels and are preserved.
type TowerUseItem struct {
	ItemID uint32
	Count  int32
}

// TowerRoute is the immutable active-PVF layer mapping for one tower family.
type TowerRoute struct {
	Family        TowerFamily
	DungeonIDs    []uint32
	UseAllowItems []TowerUseItem
}

// LimitError retains the exhausted policy and current service-time snapshot
// while remaining compatible with errors.Is(err, ErrLimitReached).
type LimitError struct {
	Snapshot Snapshot
}

func (e *LimitError) Error() string {
	if e == nil {
		return ErrLimitReached.Error()
	}
	return fmt.Sprintf("%s: policy=%s used=%d limit=%d", ErrLimitReached, e.Snapshot.Policy.Key, e.Snapshot.Used, e.Snapshot.EffectiveLimit)
}

func (e *LimitError) Unwrap() error { return ErrLimitReached }

var serviceCalendar = time.FixedZone("Asia/Shanghai", 8*60*60)

// ServiceWindow applies the project's established DNF service boundary: a
// Beijing service day begins at 06:00. Weekly policies begin at 06:00 on the
// PVF-authored reset weekday.
func ServiceWindow(policy Policy, now time.Time) (Window, error) {
	if err := validatePolicy(policy); err != nil {
		return Window{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	local := now.In(serviceCalendar)
	serviceDate := local.Add(-6 * time.Hour)
	start := time.Date(serviceDate.Year(), serviceDate.Month(), serviceDate.Day(), 6, 0, 0, 0, serviceCalendar)
	switch policy.Period {
	case PeriodDaily:
		return Window{Start: start, End: start.AddDate(0, 0, 1)}, nil
	case PeriodWeekly:
		days := (int(serviceDate.Weekday()) - int(policy.ResetWeekday) + 7) % 7
		start = start.AddDate(0, 0, -days)
		return Window{Start: start, End: start.AddDate(0, 0, 7)}, nil
	default:
		return Window{}, ErrCommandInvalid
	}
}

func normalizePolicies(policies []Policy) ([]Policy, error) {
	if len(policies) == 0 {
		return nil, fmt.Errorf("%w: no policies", ErrCommandInvalid)
	}
	out := make([]Policy, len(policies))
	for index, policy := range policies {
		policy.Key = strings.TrimSpace(policy.Key)
		policy.SourcePath = strings.TrimSpace(policy.SourcePath)
		policy.SourceSection = strings.TrimSpace(policy.SourceSection)
		policy.DungeonIDs = append([]uint32(nil), policy.DungeonIDs...)
		if err := validatePolicy(policy); err != nil {
			return nil, err
		}
		out[index] = policy
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].Key != out[right].Key {
			return out[left].Key < out[right].Key
		}
		return out[left].Scope < out[right].Scope
	})
	for index := 1; index < len(out); index++ {
		if out[index-1].Key == out[index].Key && out[index-1].Scope == out[index].Scope {
			return nil, fmt.Errorf("%w: duplicate policy=%s scope=%d", ErrCommandInvalid, out[index].Key, out[index].Scope)
		}
	}
	return out, nil
}

func validatePolicy(policy Policy) error {
	if strings.TrimSpace(policy.Key) == "" || policy.Limit == 0 ||
		(policy.Scope != ScopeCharacter && policy.Scope != ScopeAccount) ||
		(policy.Period != PeriodDaily && policy.Period != PeriodWeekly) ||
		(policy.Period == PeriodWeekly && (policy.ResetWeekday < time.Sunday || policy.ResetWeekday > time.Saturday)) {
		return fmt.Errorf("%w: policy=%q scope=%d period=%d limit=%d reset_weekday=%d", ErrCommandInvalid, policy.Key, policy.Scope, policy.Period, policy.Limit, policy.ResetWeekday)
	}
	return nil
}
