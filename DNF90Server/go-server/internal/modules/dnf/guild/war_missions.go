package guild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrWarMissionIncomplete = errors.New("guild war mission is incomplete")

type WarMissionReward struct{ ItemID, Count uint32 }
type WarMission struct {
	ID               uint32
	Season           bool
	Title, Condition string
	Required         uint32
	Dungeons         []uint32
	Rewards          []WarMissionReward
}
type WarMissionCatalog struct {
	Missions                []WarMission
	DailyTitle, SeasonTitle uint32
}

func (c *WarMissionCatalog) Find(id uint32) (WarMission, bool) {
	if c != nil {
		for _, m := range c.Missions {
			if m.ID == id {
				return m, true
			}
		}
	}
	return WarMission{}, false
}

type WarMissionValue struct {
	Progress uint32 `json:"progress"`
	Claimed  bool   `json:"claimed,omitempty"`
}
type WarMissionState struct {
	DayStart    int64                      `json:"day_start"`
	SeasonStart int64                      `json:"season_start"`
	Values      map[uint32]WarMissionValue `json:"values"`
	Events      map[string]int64           `json:"events,omitempty"`
}

// Guild war days reset at 06:00 Beijing. No historical progress is synthesized.
func WarDay(now time.Time) (start, end time.Time) {
	local := now.In(time.FixedZone("CST", 8*60*60))
	start = time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, local.Location())
	if local.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start, start.AddDate(0, 0, 1)
}

func (s *WarMissionState) Reset(now time.Time, catalog *WarMissionCatalog) {
	day, _ := WarDay(now)
	season, _ := WarSeason(now)
	if s.Values == nil {
		s.Values = make(map[uint32]WarMissionValue)
	}
	if s.Events == nil {
		s.Events = make(map[string]int64)
	}
	for id := range s.Values {
		m, found := catalog.Find(id)
		if !found || (m.Season && s.SeasonStart != season.Unix()) || (!m.Season && s.DayStart != day.Unix()) {
			delete(s.Values, id)
		}
	}
	for key, at := range s.Events {
		if at < season.Unix() {
			delete(s.Events, key)
		}
	}
	s.DayStart, s.SeasonStart = day.Unix(), season.Unix()
}

func warMissionScope(characterID string) string {
	return "character:" + characterID + ":guild_war_missions:v1"
}
func LoadWarMissions(ctx context.Context, repo dnfrepo.SettingsRepository, characterID string, catalog *WarMissionCatalog, now time.Time) (WarMissionState, error) {
	var s WarMissionState
	if repo == nil || characterID == "" || catalog == nil {
		return s, ErrRepositoryUnavailable
	}
	record, found, err := repo.Load(ctx, warMissionScope(characterID))
	if err != nil {
		return s, err
	}
	if found && record.Values["state"] != "" {
		if err = json.Unmarshal([]byte(record.Values["state"]), &s); err != nil {
			return s, err
		}
	}
	s.Reset(now, catalog)
	return s, nil
}
func SaveWarMissions(ctx context.Context, repo dnfrepo.SettingsRepository, characterID string, s WarMissionState, now time.Time) error {
	if repo == nil || characterID == "" {
		return ErrRepositoryUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return repo.Save(ctx, dnfrepo.SettingsRecord{Scope: warMissionScope(characterID), Values: map[string]string{"state": string(b)}, UpdatedAt: now})
}

// Observe is called only for an accepted server-owned battle entry or clear.
// Its receipt covers retries, including a retry after the daily reset.
func (s *WarMissionState) Observe(catalog *WarMissionCatalog, kind string, dungeon uint32, key string, occurredAt, now time.Time) error {
	if catalog == nil || key == "" || (kind != "join" && kind != "clear") || occurredAt.After(now) {
		return ErrInvalidContent
	}
	s.Reset(now, catalog)
	if occurredAt.Unix() < s.SeasonStart {
		return ErrInvalidContent
	}
	key = kind + ":" + key
	if _, found := s.Events[key]; found {
		return nil
	}
	s.Events[key] = occurredAt.Unix()
	for _, m := range catalog.Missions {
		if m.Condition != kind || (!m.Season && occurredAt.Unix() < s.DayStart) {
			continue
		}
		if len(m.Dungeons) > 0 {
			matched := false
			for _, id := range m.Dungeons {
				matched = matched || id == dungeon
			}
			if !matched {
				continue
			}
		}
		v := s.Values[m.ID]
		if v.Progress < m.Required {
			v.Progress++
		}
		s.Values[m.ID] = v
	}
	s.refreshTitles(catalog)
	return nil
}

func (s *WarMissionState) refreshTitles(catalog *WarMissionCatalog) {
	for _, title := range catalog.Missions {
		if title.Condition != "mission" {
			continue
		}
		var completed uint32
		for _, m := range catalog.Missions {
			if m.Season == title.Season && m.Condition != "mission" && s.Values[m.ID].Progress >= m.Required {
				completed++
			}
		}
		v := s.Values[title.ID]
		v.Progress = min(completed, title.Required)
		s.Values[title.ID] = v
	}
}

// Claim changes the receipt in memory. The caller commits it with all item
// grants in one repository transaction. An already claimed request is a no-op.
func (s *WarMissionState) Claim(catalog *WarMissionCatalog, id uint32, now time.Time) ([]WarMissionReward, error) {
	s.Reset(now, catalog)
	m, found := catalog.Find(id)
	if !found {
		return nil, ErrInvalidContent
	}
	v := s.Values[id]
	if v.Claimed {
		return nil, nil
	}
	if v.Progress < m.Required {
		return nil, ErrWarMissionIncomplete
	}
	v.Claimed = true
	s.Values[id] = v
	return append([]WarMissionReward(nil), m.Rewards...), nil
}

// ExperienceRanks uses competition ranking: 1, 1, 3 for tied experience.
// It derives ranks from all persisted guilds, including offline guilds.
func (r Registry) ExperienceRanks() map[uint32]uint32 {
	list := make([]*Guild, 0, len(r.Guilds))
	for _, g := range r.Guilds {
		if g != nil && g.ID != 0 {
			list = append(list, g)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Experience != list[j].Experience {
			return list[i].Experience > list[j].Experience
		}
		return list[i].ID < list[j].ID
	})
	ranks := make(map[uint32]uint32, len(list))
	var rank uint32
	for i, g := range list {
		if i == 0 || g.Experience != list[i-1].Experience {
			rank = uint32(i + 1)
		}
		ranks[g.ID] = rank
	}
	return ranks
}

func invalidWarMission(id uint32) error {
	return fmt.Errorf("guild war mission %d: %w", id, ErrInvalidContent)
}
