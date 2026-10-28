package onlineevent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Test edition requested by the owner. This is elapsed time, not a date rollover.
const ReturneeOfflinePeriod = 24 * time.Hour
const ReturneeDuration = 30 * 24 * time.Hour

// Owner-selected local rule: the returning player's monster and clear EXP
// each gain 20%, in solo and party play. This is not an inferred PVF formula.
const ReturneeWelcomeExperiencePercent int64 = 20
const ReturneeMetadataKey = "event_seria_returnee_v1"
const ReturneeQuestCountKey = "seria_returnee_quest_count"
const ReturneeDisjointCountKey = "seria_returnee_disjoint_count"
const ReturneeGiftCountKey = "seria_returnee_gift_count"
const ReturneeSkillCountKey = "seria_returnee_skill_count"

type ReturneeMission struct {
	ID        uint32
	Condition string
	Required  uint32
	Reward    AttendancePVFItemReward
}

type ReturneeCatalog struct {
	Missions            [10]ReturneeMission
	MailTitle, MailBody string
}

func LoadReturneeCatalog(ctx context.Context, source dnfpvf.Source) (*ReturneeCatalog, error) {
	text, err := source.ReadText("etc/seriawelcome.etc")
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse("etc/seriawelcome.etc", text)
	if err != nil {
		return nil, err
	}
	c := &ReturneeCatalog{}
	definitions := map[uint32]ReturneeMission{}
	var m ReturneeMission
	inList, inGroups, inMission, inCondition := false, false, false, false
	group := int64(-1)
	var ids []uint32
	for _, section := range doc.Sections {
		v := doc.Tokens[section.Start:section.End]
		integer := func(i int) int64 {
			if i < len(v) && v[i].Kind == dnfpvf.TokenInt {
				return v[i].Int
			}
			return -1
		}
		switch section.Name {
		case "mission list":
			if inGroups && group == 1 {
				for _, t := range v {
					if t.Kind != dnfpvf.TokenInt || t.Int < 1 {
						return nil, fmt.Errorf("invalid returnee mission group")
					}
					ids = append(ids, uint32(t.Int))
				}
			} else if !inGroups {
				inList = true
			}
		case "/mission list":
			if !inGroups {
				inList = false
			}
		case "mission group":
			inGroups = true
		case "/mission group":
			inGroups = false
		case "group":
			group = integer(0)
		case "mission":
			if inList {
				m = ReturneeMission{}
				inMission = true
			}
		case "/mission":
			if inMission {
				definitions[m.ID] = m
				inMission = false
			}
			inCondition = false
		case "index":
			if inMission {
				m.ID = uint32(integer(0))
			}
		case "condition":
			if inMission {
				inCondition = true
			}
		case "/condition":
			inCondition = false
		case "reward item":
			if inMission && len(v) == 2 {
				m.Reward = AttendancePVFItemReward{ItemID: integer(0), Count: integer(1)}
			}
		case "reward mail":
			if len(v) == 2 {
				c.MailTitle, c.MailBody = v[0].Value, v[1].Value
			}
		default:
			if inCondition && section.Name != "condition value" && len(v) == 1 && integer(0) > 0 {
				m.Condition, m.Required = section.Name, uint32(integer(0))
			}
		}
	}
	if len(ids) != len(c.Missions) || c.MailTitle == "" {
		return nil, fmt.Errorf("returnee native layout requires ten missions and mail text")
	}
	for i, id := range ids {
		m, ok := definitions[id]
		if !ok || m.Required == 0 || m.Reward.ItemID <= 0 || m.Reward.Count <= 0 {
			return nil, fmt.Errorf("invalid returnee mission %d", id)
		}
		switch m.Condition {
		case "clear quest", "join guild", "learn skill", "proper dungeon clear", "disjoint item", "level up count", "present npc", "mission clear count":
		default:
			return nil, fmt.Errorf("unsupported returnee condition %q", m.Condition)
		}
		c.Missions[i] = m
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

type ReturneeObservation struct {
	Level, Quests, Clears, Disjoint, Gifts, Learned int64
	Guild                                           bool
}

type ReturneeState struct {
	LastSeen  time.Time                      `json:"last_seen"`
	Started   time.Time                      `json:"started,omitempty"`
	ExpiresAt time.Time                      `json:"expires_at,omitempty"`
	Guide     uint64                         `json:"guide,omitempty"`
	Progress  [10]uint32                     `json:"progress"`
	Claimed   uint16                         `json:"claimed"`
	Observed  map[string]ReturneeObservation `json:"observed,omitempty"`
}

func (s ReturneeState) ActiveAt(now time.Time) bool {
	// Mission claims do not shorten the owner's 30-day welcome benefit.
	if s.Started.IsZero() {
		return false
	}
	expiresAt := s.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = s.Started.Add(ReturneeDuration)
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Before(expiresAt)
}

func (s ReturneeState) Active() bool { return s.ActiveAt(time.Now()) }

func LoadReturneeState(a dnfrepo.AccountRecord) (ReturneeState, error) {
	var s ReturneeState
	if raw := a.Metadata[ReturneeMetadataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return s, fmt.Errorf("invalid returnee state: %w", err)
		}
	}
	if s.Claimed > 0x3ff {
		return s, fmt.Errorf("invalid returnee claim mask")
	}
	if !s.Started.IsZero() {
		if s.ExpiresAt.IsZero() {
			s.ExpiresAt = s.Started.Add(ReturneeDuration)
		} else if s.ExpiresAt.Before(s.Started) {
			return s, fmt.Errorf("invalid returnee expiration")
		}
	}
	return s, nil
}

// Observe uses durable gameplay counters; client progress reports
// cannot complete a mission. Each account/character snapshot is consumed once.
func (s *ReturneeState) Observe(c *ReturneeCatalog, characterID string, o ReturneeObservation, now time.Time, login bool) {
	if login && !s.ActiveAt(now) && !s.LastSeen.IsZero() && now.Sub(s.LastSeen) >= ReturneeOfflinePeriod {
		*s = ReturneeState{Started: now, ExpiresAt: now.Add(ReturneeDuration), LastSeen: s.LastSeen, Guide: s.Guide}
	}
	if now.After(s.LastSeen) {
		s.LastSeen = now
	}
	if !s.ActiveAt(now) {
		return
	}
	if s.Observed == nil {
		s.Observed = map[string]ReturneeObservation{}
	}
	old, known := s.Observed[characterID]
	if !known {
		old = o
	}
	for i, m := range c.Missions {
		var n int64
		switch m.Condition {
		case "clear quest":
			n = o.Quests - old.Quests
		case "proper dungeon clear":
			n = o.Clears - old.Clears
		case "disjoint item":
			n = o.Disjoint - old.Disjoint
		case "present npc":
			n = o.Gifts - old.Gifts
		case "level up count":
			n = o.Level - old.Level
		case "learn skill":
			n = o.Learned - old.Learned
		case "join guild":
			if o.Guild {
				n = int64(m.Required)
			}
		}
		s.Progress[i] = uint32(min(int64(m.Required), int64(s.Progress[i])+max(n, 0)))
	}
	var completed uint32
	for i, m := range c.Missions {
		if m.Condition != "mission clear count" && s.Progress[i] >= m.Required {
			completed++
		}
	}
	for i, m := range c.Missions {
		if m.Condition == "mission clear count" {
			s.Progress[i] = min(completed, m.Required)
		}
	}
	s.Observed[characterID] = o
}

type ReturneeCommand struct {
	AccountID, CharacterID string
	Now                    time.Time
	Login                  bool
	Claim                  uint32
	Guide                  *uint32
	Observation            ReturneeObservation
	Deliver                func(dnfrepo.MailboxRepository, AttendancePVFItemReward, string) (string, error)
}

func SettleReturnee(ctx context.Context, repos dnfrepo.Group, c *ReturneeCatalog, cmd ReturneeCommand) (ReturneeState, []string, error) {
	var state ReturneeState
	var mails []string
	if c == nil || repos.MailboxAssets == nil || cmd.Now.IsZero() || cmd.AccountID == "" || cmd.CharacterID == "" {
		return state, nil, ErrOwnerUnavailable
	}
	err := repos.MailboxAssets.WithinMailboxAssets(ctx, cmd.CharacterID, cmd.CharacterID, func(accounts dnfrepo.AccountRepository, _ dnfrepo.AccountInventoryRepository, chars dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, mailbox dnfrepo.MailboxRepository) error {
		ch, found, err := chars.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || ch.AccountID != cmd.AccountID {
			return fmt.Errorf("returnee character ownership mismatch")
		}
		a, found, err := accounts.Load(ctx, cmd.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("returnee account missing")
		}
		state, err = LoadReturneeState(a)
		if err != nil {
			return err
		}
		o := cmd.Observation
		o.Level, o.Quests, o.Disjoint, o.Gifts = int64(ch.Level), ch.Stats[ReturneeQuestCountKey], ch.Stats[ReturneeDisjointCountKey], ch.Stats[ReturneeGiftCountKey]
		o.Clears = ch.Stats["adventure_recommended_dungeon_clears"]
		o.Learned = ch.Stats[ReturneeSkillCountKey]
		state.Observe(c, cmd.CharacterID, o, cmd.Now, cmd.Login)
		if cmd.Guide != nil && *cmd.Guide < 41 {
			state.Guide |= uint64(1) << *cmd.Guide
		}
		if cmd.Claim != 0 {
			if !state.ActiveAt(cmd.Now) {
				return fmt.Errorf("returnee period expired or inactive")
			}
			index := -1
			for i, m := range c.Missions {
				if m.ID == cmd.Claim {
					index = i
				}
			}
			if index < 0 || state.Started.IsZero() || state.Progress[index] < c.Missions[index].Required {
				return fmt.Errorf("returnee mission not complete")
			}
			if state.Claimed&(1<<index) == 0 {
				if cmd.Deliver == nil {
					return ErrOwnerUnavailable
				}
				key := fmt.Sprintf("%s:%d:%d", ReturneeMetadataKey, state.Started.UnixNano(), cmd.Claim)
				id, err := cmd.Deliver(mailbox, c.Missions[index].Reward, key)
				if err != nil {
					return err
				}
				if id == "" {
					return fmt.Errorf("returnee reward mail missing")
				}
				mails = append(mails, id)
				state.Claimed |= 1 << index
			}
		}
		wantFlag := int64(0)
		if state.ActiveAt(cmd.Now) {
			wantFlag = 1
		}
		if ch.Stats["is_return_user"] != wantFlag {
			ch = dnfrepo.CloneCharacter(ch)
			if ch.Stats == nil {
				ch.Stats = map[string]int64{}
			}
			ch.Stats["is_return_user"] = wantFlag
			ch.UpdatedAt = cmd.Now
			if err := dnfrepo.SaveCharacterFields(ctx, chars, ch, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return dnfrepo.SaveAccountMetadataEntry(ctx, accounts, a, ReturneeMetadataKey, string(raw), cmd.Now)
	})
	return state, mails, err
}
