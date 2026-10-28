package onlineevent

import (
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const WeeklyPVFPath = "event/chn_event/chn_totalattendance.evt"
const WeeklyMetadataKey = "event_weekly_attendance_20260910_v1"

// The local twelve-week edition starts on Thursday at the DNF reset hour.
func WeeklyStarts() time.Time { return time.Date(2026, 9, 10, 6, 0, 0, 0, ChinaCalendar) }
func WeeklyEnds() time.Time   { return WeeklyStarts().AddDate(0, 0, 12*7) }

func WeeklyWeek(now time.Time) int {
	if now.Before(WeeklyStarts()) || !now.Before(WeeklyEnds()) {
		return -1
	}
	return int(now.Sub(WeeklyStarts()) / (7 * 24 * time.Hour))
}

type WeeklyCatalog struct {
	RequiredClears, CatchupClears, CatchupLimit uint32
	Rewards                                     [12]AttendancePVFItemReward
	Bonus                                       [3]AttendancePVFItemReward
	Thresholds                                  [3]uint32
}

func LoadWeeklyCatalog(ctx context.Context, source dnfpvf.Source) (*WeeklyCatalog, error) {
	text, err := source.ReadText(WeeklyPVFPath)
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse(WeeklyPVFPath, text)
	if err != nil {
		return nil, err
	}
	c := &WeeklyCatalog{}
	for _, spec := range []struct {
		name  string
		count int
	}{
		{"event duration", 1}, {"attendance condition", 2}, {"bonus attendance count", 1}, {"reward by the week", 1},
		{"attendance week", 36}, {"total attendance week", 9},
	} {
		v, err := attendanceIntegerSection(doc, spec.name, spec.count)
		if err != nil {
			return nil, err
		}
		switch spec.name {
		case "event duration":
			if v[0] != 12 {
				return nil, fmt.Errorf("weekly duration must match twelve native slots")
			}
		case "attendance condition":
			if v[0] < 1 || v[0] > 10000 || v[1] < 1 || v[1] > 10000 {
				return nil, fmt.Errorf("weekly clear thresholds invalid")
			}
			c.RequiredClears, c.CatchupClears = uint32(v[0]), uint32(v[1])
		case "bonus attendance count":
			if v[0] < 0 || v[0] > 12 {
				return nil, fmt.Errorf("weekly catchup limit invalid")
			}
			c.CatchupLimit = uint32(v[0])
		case "reward by the week":
			if v[0] != 0 {
				return nil, fmt.Errorf("weekly reward mode is not sequential")
			}
		default:
			for i := 0; i < len(v)/3; i++ {
				key, id, count := v[i*3], v[i*3+1], v[i*3+2]
				if id <= 0 || id > 0xffffffff || count <= 0 || count > 0xffffffff {
					return nil, fmt.Errorf("weekly reward invalid")
				}
				r := AttendancePVFItemReward{ItemID: id, Count: count}
				if spec.name == "attendance week" {
					if key != int64(i+1) {
						return nil, fmt.Errorf("weekly reward index invalid")
					}
					c.Rewards[i] = r
				} else {
					if key != []int64{4, 8, 11}[i] {
						return nil, fmt.Errorf("weekly bonus threshold differs from native slots")
					}
					c.Bonus[i], c.Thresholds[i] = r, uint32(key)
				}
			}
		}
	}
	all := append(append([]AttendancePVFItemReward{}, c.Rewards[:]...), c.Bonus[:]...)
	paths, err := resolveAttendanceRewardDefinitions(ctx, source, all)
	if err != nil {
		return nil, err
	}
	attachAttendanceDefinitionPaths(c.Rewards[:], paths)
	attachAttendanceDefinitionPaths(c.Bonus[:], paths)
	return c, nil
}

type weeklyState struct {
	Clears   [12]uint32 `json:"clears"`
	Signed   uint16     `json:"signed"`
	Catchups uint32     `json:"catchups"`
	Bonus    uint8      `json:"bonus"`
}

func loadWeeklyState(account dnfrepo.AccountRecord, c *WeeklyCatalog) (weeklyState, error) {
	var state weeklyState
	if raw := account.Metadata[WeeklyMetadataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return state, fmt.Errorf("weekly state invalid: %w", err)
		}
	}
	if state.Signed>>12 != 0 || state.Bonus > 7 || state.Catchups > c.CatchupLimit || state.Catchups > uint32(bits.OnesCount16(state.Signed)) {
		return state, fmt.Errorf("weekly state exceeds catalog limits")
	}
	for _, count := range state.Clears {
		if count > c.RequiredClears+c.CatchupClears*c.CatchupLimit {
			return state, fmt.Errorf("weekly clear count invalid")
		}
	}
	for i, threshold := range c.Thresholds {
		if state.Bonus&(1<<i) != 0 && uint32(bits.OnesCount16(state.Signed)) < threshold {
			return state, fmt.Errorf("weekly bonus state invalid")
		}
	}
	return state, nil
}

// Called inside the dungeon settlement receipt transaction, never from a client count.
func RecordWeeklyClear(account *dnfrepo.AccountRecord, c *WeeklyCatalog, now time.Time) error {
	week := WeeklyWeek(now)
	if week < 0 || c == nil {
		return nil
	}
	state, err := loadWeeklyState(*account, c)
	if err != nil {
		return err
	}
	if state.Clears[week] >= c.RequiredClears+c.CatchupClears*c.CatchupLimit {
		return nil
	}
	state.Clears[week]++
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if account.Metadata == nil {
		account.Metadata = map[string]string{}
	}
	account.Metadata[WeeklyMetadataKey] = string(raw)
	return nil
}

type WeeklySnapshot struct {
	Week, Total, Clears, Catchups, BonusMask      uint32
	SignedThisWeek, CanSign, CanCatchup, CanBonus bool
	SignedWeeks                                   [20]byte
	MailIDs                                       []string
}

func weeklySnapshot(state weeklyState, c *WeeklyCatalog, week int) WeeklySnapshot {
	s := WeeklySnapshot{Total: uint32(bits.OnesCount16(state.Signed)), Catchups: state.Catchups}
	for i, threshold := range c.Thresholds {
		if state.Bonus&(1<<i) != 0 {
			s.BonusMask |= 1 << threshold
		} else if s.Total >= threshold && week >= 0 {
			s.CanBonus = true
		}
	}
	index := 0
	for i := 0; i < 12; i++ {
		if state.Signed&(1<<i) != 0 {
			s.SignedWeeks[index] = byte(i + 1)
			index++
		}
	}
	if week < 0 {
		s.Week = 13
		return s
	}
	s.Week = uint32(week + 1)
	s.Clears = state.Clears[week]
	s.SignedThisWeek = state.Signed&(1<<week) != 0
	// Each earlier missing week can be restored once. The current week is signed normally first.
	s.CanCatchup = s.SignedThisWeek && s.Total < uint32(week+1) && state.Catchups < c.CatchupLimit
	s.CanSign = (!s.SignedThisWeek && s.Clears >= c.RequiredClears) || (s.CanCatchup && s.Clears >= c.CatchupClears)
	return s
}

type WeeklyCommand struct {
	AccountID, CharacterID string
	Now                    time.Time
	Claim, ClaimBonus      bool
	Deliver                func(dnfrepo.MailboxRepository, AttendancePVFItemReward, string) (string, error)
}

func SettleWeeklyAttendance(ctx context.Context, repos dnfrepo.Group, c *WeeklyCatalog, cmd WeeklyCommand) (WeeklySnapshot, error) {
	if c == nil || repos.MailboxAssets == nil || cmd.AccountID == "" || cmd.CharacterID == "" || cmd.Now.IsZero() || cmd.Deliver == nil {
		return WeeklySnapshot{}, ErrOwnerUnavailable
	}
	var result WeeklySnapshot
	err := repos.MailboxAssets.WithinMailboxAssets(ctx, cmd.CharacterID, cmd.CharacterID, func(accounts dnfrepo.AccountRepository, _ dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, mailboxes dnfrepo.MailboxRepository) error {
		character, found, err := characters.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || character.AccountID != cmd.AccountID {
			return fmt.Errorf("weekly character ownership mismatch")
		}
		account, found, err := accounts.Load(ctx, cmd.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("weekly account missing")
		}
		state, err := loadWeeklyState(account, c)
		if err != nil {
			return err
		}
		week := WeeklyWeek(cmd.Now)
		before := weeklySnapshot(state, c, week)
		var mails []string
		deliver := func(r AttendancePVFItemReward, key string) error {
			id, err := cmd.Deliver(mailboxes, r, WeeklyMetadataKey+":"+key)
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("weekly mail missing")
			}
			mails = append(mails, id)
			return nil
		}
		if cmd.Claim && before.CanSign {
			target := week
			cost := c.RequiredClears
			if before.SignedThisWeek {
				for target = 0; target < week; target++ {
					if state.Signed&(1<<target) == 0 {
						break
					}
				}
				cost = c.CatchupClears
				state.Catchups++
			}
			if err := deliver(c.Rewards[before.Total], fmt.Sprintf("week:%d", target+1)); err != nil {
				return err
			}
			state.Signed |= 1 << target
			state.Clears[week] -= cost
		}
		if cmd.ClaimBonus && week >= 0 {
			for i, threshold := range c.Thresholds {
				if uint32(bits.OnesCount16(state.Signed)) >= threshold && state.Bonus&(1<<i) == 0 {
					if err := deliver(c.Bonus[i], fmt.Sprintf("bonus:%d", threshold)); err != nil {
						return err
					}
					state.Bonus |= 1 << i
				}
			}
		}
		result = weeklySnapshot(state, c, week)
		result.MailIDs = mails
		if len(mails) == 0 {
			return nil
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return dnfrepo.SaveAccountMetadataEntry(ctx, accounts, account, WeeklyMetadataKey, string(raw), cmd.Now)
	})
	if err != nil {
		return WeeklySnapshot{}, err
	}
	return result, nil
}
