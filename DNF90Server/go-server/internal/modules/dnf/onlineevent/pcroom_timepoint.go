package onlineevent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const PCRoomTimePointPVFPath = "etc/pcroomtimepoint.etc"
const PCRoomTimePointMetadataKey = "online_pcroom_timepoint_228_v1"

// Current EXE 0x1BD4670/0x3885270 loads these rewards independently of
// chn_AttendanceEvent.evt. Durations and period requirements are incremental.
type PCRoomTimePointCatalog struct {
	Daily   [4]AttendancePVFItemReward
	Period  [4]AttendancePVFItemReward
	Seconds [4]uint32
	Counts  [4]uint32
	UIPath  string
}

func LoadPCRoomTimePointCatalog(ctx context.Context, source dnfpvf.Source) (*PCRoomTimePointCatalog, error) {
	if source == nil {
		return nil, dnfpvf.ErrSourceRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text, err := source.ReadText(PCRoomTimePointPVFPath)
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse(PCRoomTimePointPVFPath, text)
	if err != nil {
		return nil, err
	}
	c := &PCRoomTimePointCatalog{}
	for name, want := range map[string]string{"daily reward autoget": "false", "daily reward loop": "1", "period reward loop": "4"} {
		tokens, ok := doc.Section(name)
		if !ok || len(tokens) != 1 || tokens[0].Value != want {
			return nil, fmt.Errorf("pcroom [%s] differs from current manual four-stage edition", name)
		}
	}
	for _, spec := range []struct {
		name  string
		width int
	}{{"daily reward items", 6}, {"period reward item", 5}} {
		tokens, ok := doc.Section(spec.name)
		if !ok || len(tokens) != 4*spec.width {
			return nil, fmt.Errorf("pcroom [%s] has invalid shape", spec.name)
		}
		var cumulative uint64
		for i := 0; i < 4; i++ {
			row := tokens[i*spec.width : (i+1)*spec.width]
			for j := 0; j < 4; j++ {
				if row[j].Kind != dnfpvf.TokenInt || row[j].Int <= 0 || row[j].Int > math.MaxUint32 {
					return nil, fmt.Errorf("pcroom [%s] row %d integer %d invalid", spec.name, i, j)
				}
			}
			for j := 4; j < spec.width; j++ {
				if row[j].Value != "true" {
					return nil, fmt.Errorf("pcroom [%s] row %d unsupported flag", spec.name, i)
				}
			}
			r := AttendancePVFItemReward{ItemID: row[1].Int, Count: row[2].Int}
			if spec.width == 6 {
				if row[0].Int != 1 || row[3].Int%1000 != 0 {
					return nil, fmt.Errorf("pcroom daily group/duration invalid")
				}
				cumulative += uint64(row[3].Int / 1000)
				if cumulative > secondsPerDay {
					return nil, fmt.Errorf("pcroom daily duration exceeds service day")
				}
				c.Daily[i], c.Seconds[i] = r, uint32(cumulative)
			} else {
				if row[0].Int != int64(i+1) {
					return nil, fmt.Errorf("pcroom period slot invalid")
				}
				cumulative += uint64(row[3].Int)
				if cumulative > math.MaxInt32 {
					return nil, fmt.Errorf("pcroom period count overflows client")
				}
				c.Period[i], c.Counts[i] = r, uint32(cumulative)
			}
		}
	}
	ui, ok := doc.Section("ui file")
	if !ok || len(ui) != 1 || strings.ToLower(ui[0].Value) != "ui/event/pcroomtimepoint/pcroomtimepointrewarditeminfowindow_chn.xui" {
		return nil, fmt.Errorf("pcroom native UI path differs from verified current client")
	}
	c.UIPath = ui[0].Value
	if _, err := source.ReadText(c.UIPath); err != nil {
		return nil, err
	}
	paths, err := resolveAttendanceRewardDefinitions(ctx, source, append(append([]AttendancePVFItemReward{}, c.Daily[:]...), c.Period[:]...))
	if err != nil {
		return nil, err
	}
	attachAttendanceDefinitionPaths(c.Daily[:], paths)
	attachAttendanceDefinitionPaths(c.Period[:], paths)
	return c, nil
}

type pcroomTimePointState struct {
	Version     int    `json:"version"`
	Date        string `json:"date"`
	DailyMask   byte   `json:"daily_mask"`
	DayComplete bool   `json:"day_complete"`
	Total       uint32 `json:"total"`
	PeriodMask  byte   `json:"period_mask"`
}

type PCRoomTimePointSnapshot struct {
	Seconds    uint32
	Total      uint32
	DailyMask  byte
	PeriodMask uint32
	MailIDs    []string
}

type PCRoomTimePointCommand struct {
	AccountID, CharacterID string
	Now                    time.Time
	// Reuse the existing authoritative interval-union clock, including its
	// 06:00 boundary, cross-character accounting and already persisted time.
	ClockDefinition Definition
	DailyClaim      byte
	PeriodClaim     int // -1 means no period claim; 0..3 are native button indices.
	// PeriodLoop 打开时，四个累计档全部领完之后，下一个服务日把 Total/PeriodMask
	// 归零，"累计 25 次"因此变成可重复的循环；关闭时保持封顶语义（领完即永久结束）。
	// 归零只认"已领取"，所以没把四个累计档领完的账号不会被吞掉进度。
	PeriodLoop bool
	Deliver         func(dnfrepo.MailboxRepository, AttendancePVFItemReward, string) (string, error)
}

// SettlePCRoomTimePoint counts one fully completed service day, independently
// of clicking the fourth reward. Mail and masks commit in the same transaction.
// The current PVF edition is kept open locally; counts stop at its final tier
// unless the caller asks for the repeatable period loop (cmd.PeriodLoop).
func SettlePCRoomTimePoint(ctx context.Context, repos dnfrepo.Group, c *PCRoomTimePointCatalog, cmd PCRoomTimePointCommand) (PCRoomTimePointSnapshot, error) {
	if c == nil || repos.MailboxAssets == nil || cmd.AccountID == "" || cmd.CharacterID == "" || cmd.Now.IsZero() || cmd.Deliver == nil {
		return PCRoomTimePointSnapshot{}, ErrOwnerUnavailable
	}
	if cmd.DailyClaim&^byte(15) != 0 || cmd.PeriodClaim < -1 || cmd.PeriodClaim > 3 || (cmd.DailyClaim != 0 && cmd.PeriodClaim >= 0) {
		return PCRoomTimePointSnapshot{}, ErrRewardInvalid
	}
	if err := cmd.ClockDefinition.Validate(); err != nil {
		return PCRoomTimePointSnapshot{}, err
	}
	var result PCRoomTimePointSnapshot
	err := repos.MailboxAssets.WithinMailboxAssets(ctx, cmd.CharacterID, cmd.CharacterID, func(accounts dnfrepo.AccountRepository, _ dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, mailboxes dnfrepo.MailboxRepository) error {
		character, found, err := characters.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || character.AccountID != cmd.AccountID {
			return ErrAccountMismatch
		}
		account, found, err := accounts.Load(ctx, cmd.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return ErrAccountNotFound
		}
		clock, err := parseState(account.Metadata[metadataKey(cmd.ClockDefinition.ID)], cmd.ClockDefinition)
		if err != nil {
			return err
		}
		date, _ := calendarDay(cmd.ClockDefinition.calendar(), cmd.ClockDefinition.boundary(), cmd.Now)
		if clock.CalendarDate > date {
			return ErrObservationStale
		}
		var seconds uint32
		if clock.CalendarDate == date {
			seconds = uint32(clock.OnlineSeconds)
		}
		state := pcroomTimePointState{Version: 1, Date: date}
		raw := account.Metadata[PCRoomTimePointMetadataKey]
		if raw != "" {
			decoder := json.NewDecoder(strings.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&state); err != nil {
				return fmt.Errorf("%w: pcroom %v", ErrStateInvalid, err)
			}
			if err := rejectTrailingJSON(decoder); err != nil {
				return err
			}
			if _, err := time.Parse(time.DateOnly, state.Date); err != nil {
				return ErrStateInvalid
			}
			if state.Version != 1 || state.DailyMask > 15 || state.PeriodMask > 15 || state.Total > c.Counts[3] {
				return ErrStateInvalid
			}
			for i, n := range c.Counts {
				if state.PeriodMask&(1<<i) != 0 && state.Total < n {
					return ErrStateInvalid
				}
			}
			if state.Date > date {
				return ErrObservationStale
			}
			if state.Date != date {
				state.Date = date
				state.DailyMask = 0
				state.DayComplete = false
				if cmd.PeriodLoop && state.PeriodMask == byte(1<<len(c.Period))-1 {
					// 四个累计档都已领取：换天后从 0 次重新累计。
					state.Total = 0
					state.PeriodMask = 0
				}
			}
		} else if clock.CalendarDate == date {
			// One-time cutover: retain today's already delivered legacy rewards as
			// claimed, so switching the UI does not grant a second set that day.
			for i, stage := range cmd.ClockDefinition.Stages {
				if i < 4 {
					if _, ok := clock.Claims[stage.ID]; ok {
						state.DailyMask |= 1 << i
					}
				}
			}
		}
		if !state.DayComplete && seconds >= c.Seconds[3] {
			state.DayComplete = true
			if state.Total < c.Counts[3] {
				state.Total++
			}
		}
		result = PCRoomTimePointSnapshot{Seconds: seconds, Total: state.Total, DailyMask: state.DailyMask, PeriodMask: uint32(state.PeriodMask)}
		deliver := func(reward AttendancePVFItemReward, key string) error {
			id, err := cmd.Deliver(mailboxes, reward, PCRoomTimePointMetadataKey+":"+key)
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("pcroom reward mail missing")
			}
			result.MailIDs = append(result.MailIDs, id)
			return nil
		}
		for i, reward := range c.Daily {
			bit := byte(1 << i)
			if cmd.DailyClaim&bit == 0 || state.DailyMask&bit != 0 || seconds < c.Seconds[i] {
				continue
			}
			if err := deliver(reward, fmt.Sprintf("daily:%s:%d", date, i)); err != nil {
				return err
			}
			state.DailyMask |= bit
		}
		if i := cmd.PeriodClaim; i >= 0 && state.Total >= c.Counts[i] && state.PeriodMask&(1<<i) == 0 {
			if err := deliver(c.Period[i], fmt.Sprintf("period:%d", i)); err != nil {
				return err
			}
			state.PeriodMask |= 1 << i
		}
		result.DailyMask, result.PeriodMask = state.DailyMask, uint32(state.PeriodMask)
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if string(encoded) == raw {
			return nil
		}
		return dnfrepo.SaveAccountMetadataEntry(ctx, accounts, account, PCRoomTimePointMetadataKey, string(encoded), cmd.Now)
	})
	if err != nil {
		return PCRoomTimePointSnapshot{}, err
	}
	return result, nil
}
