package guild

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

var (
	ErrWarClosed     = errors.New("guild war is closed")
	ErrWarNoOpponent = errors.New("no eligible rival guild")
	ErrWarActive     = errors.New("guild war already active")
	ErrWarNotActive  = errors.New("guild war is not active")
	ErrWarLimit      = errors.New("guild war daily participation limit")
	ErrWarGuardian   = errors.New("guild war requires an active guardian")
)

type WarRules struct {
	Days                                        [7]bool
	PauseStart, PauseEnd                        int
	PlaySeconds, DailyMax, EnemyHP, ClearDamage uint32
	FriendlyHP                                  uint32
	EnemyDungeon                                uint32
	Dungeons                                    map[uint32]bool
	RequiredClear                               []uint32
	ChallengeCosts                              []uint32
}

func LoadWarRules(source dnfpvf.Source) (*WarRules, error) {
	doc, err := readWarDocument(source, "etc/agitwar.etc")
	if err != nil {
		return nil, err
	}
	r := &WarRules{Dungeons: make(map[uint32]bool)}
	values := func(name string) ([]uint32, error) {
		t, ok := doc.Section(name)
		if !ok {
			return nil, ErrInvalidContent
		}
		return warUnsigned(t)
	}
	days, err := values("agitwar day")
	if err != nil || len(days) == 0 {
		return nil, ErrInvalidContent
	}
	for _, day := range days {
		if day > 6 {
			return nil, ErrInvalidContent
		}
		r.Days[day] = true
	}
	pause, ok := doc.Section("war limit time")
	if !ok || len(pause) != 2 {
		return nil, ErrInvalidContent
	}
	for i, t := range pause {
		v := strings.Split(t.Value, ":")
		if len(v) != 2 {
			return nil, ErrInvalidContent
		}
		h, e := strconv.Atoi(v[0])
		m, e2 := strconv.Atoi(v[1])
		if e != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, ErrInvalidContent
		}
		if i == 0 {
			r.PauseStart = h*60 + m
		} else {
			r.PauseEnd = h*60 + m
		}
	}
	for _, s := range []struct {
		name string
		dst  *uint32
	}{{"play time", &r.PlaySeconds}, {"daily max challenge", &r.DailyMax}, {"enemy hp", &r.EnemyHP}, {"clear damage", &r.ClearDamage}} {
		v, e := values(s.name)
		if e != nil || len(v) != 1 || v[0] == 0 {
			return nil, ErrInvalidContent
		}
		*s.dst = v[0]
	}
	if r.PlaySeconds > 86400 || r.DailyMax > 100 || r.ClearDamage > r.EnemyHP {
		return nil, ErrInvalidContent
	}
	if v, e := values("hp"); e == nil && len(v) > 0 {
		r.FriendlyHP = v[0]
	} else {
		return nil, ErrInvalidContent
	}
	r.ChallengeCosts, err = values("challenge chance")
	if err != nil || len(r.ChallengeCosts) == 0 {
		return nil, ErrInvalidContent
	}
	r.RequiredClear, err = values("required clear dungeon")
	if err != nil || len(r.RequiredClear) == 0 {
		return nil, ErrInvalidContent
	}
	scope := ""
	for _, sec := range doc.Sections {
		switch sec.Name {
		case "enemy agit", "friend agit", "cannon", "support dungeon", "underpass dungeon":
			scope = sec.Name
		case "/enemy agit", "/friend agit", "/cannon", "/support dungeon", "/underpass dungeon":
			scope = ""
		case "index":
			if scope != "" {
				v, e := warUnsigned(doc.Tokens[sec.Start:sec.End])
				if e != nil || len(v) != 1 || v[0] == 0 {
					return nil, ErrInvalidContent
				}
				r.Dungeons[v[0]] = true
				if scope == "enemy agit" {
					r.EnemyDungeon = v[0]
				}
			}
		case "enemy barricade dungeon", "friend barricade dungeon":
			v, e := warUnsigned(doc.Tokens[sec.Start:sec.End])
			if e != nil {
				return nil, e
			}
			for _, id := range v {
				if id == 0 {
					return nil, ErrInvalidContent
				}
				r.Dungeons[id] = true
			}
		}
	}
	if r.EnemyDungeon == 0 {
		return nil, ErrInvalidContent
	}
	for _, id := range r.RequiredClear {
		if !r.Dungeons[id] {
			return nil, ErrInvalidContent
		}
	}
	return r, nil
}

func (r *WarRules) Open(now time.Time) bool {
	if r == nil {
		return false
	}
	local := now.In(time.FixedZone("CST", 8*3600))
	day, _ := WarDay(now)
	minute := local.Hour()*60 + local.Minute()
	paused := minute >= r.PauseStart && minute < r.PauseEnd
	if r.PauseStart > r.PauseEnd {
		paused = minute >= r.PauseStart || minute < r.PauseEnd
	}
	return r.Days[int(day.Weekday())] && !paused
}

// D13FD0 computes the next Monday at 06:00 for the native season boundary.
func WarSeason(now time.Time) (time.Time, time.Time) {
	day, _ := WarDay(now)
	start := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
	return start, start.AddDate(0, 0, 7)
}

type WarBattle struct {
	Key               string   `json:"key"`
	Opponent          uint32   `json:"opponent"`
	OpponentGuardian  Guardian `json:"opponent_guardian"`
	OwnGuardian       Guardian `json:"own_guardian"`
	StartedAt, EndsAt int64
	EnemyHP           uint32            `json:"enemy_hp"`
	Clears            map[uint32]uint32 `json:"clears,omitempty"`
	Members           map[string]bool   `json:"members,omitempty"`
	Receipts          map[string]bool   `json:"receipts,omitempty"`
}
type WarState struct {
	DayStart, SeasonStart                                int64
	NextBattle                                           uint64            `json:"next_battle"`
	Rivals                                               []uint32          `json:"rivals,omitempty"`
	Results                                              map[uint32]uint32 `json:"results,omitempty"`
	Participants                                         map[string]uint32 `json:"participants,omitempty"`
	AttackWins, AttackLosses, DefenceWins, DefenceLosses uint32
	Battle                                               *WarBattle `json:"battle,omitempty"`
	LastBattle                                           *WarBattle `json:"last_battle,omitempty"`
}

func (r *Registry) AdvanceWars(now time.Time) {
	for _, g := range r.Guilds {
		if g != nil && g.War.Battle != nil && now.Unix() >= g.War.Battle.EndsAt {
			r.finishWar(g, false)
		}
	}
	day, _ := WarDay(now)
	season, _ := WarSeason(now)
	for _, g := range r.Guilds {
		if g == nil {
			continue
		}
		w := &g.War
		if w.DayStart != day.Unix() {
			w.DayStart = day.Unix()
			w.Rivals = nil
			w.Results = nil
			w.Participants = nil
		}
		if w.SeasonStart != season.Unix() {
			w.SeasonStart = season.Unix()
			w.AttackWins = 0
			w.AttackLosses = 0
			w.DefenceWins = 0
			w.DefenceLosses = 0
		}
	}
}

func (r *Registry) AddWarRivals(characterID string, initial bool, rules *WarRules, now time.Time) (*Guild, error) {
	g, m, ok := r.GuildForCharacter(characterID)
	if !ok {
		return nil, ErrNotMember
	}
	if !rules.Open(now) {
		return nil, ErrWarClosed
	}
	if !initial && g.Permissions(m.Grade)&(1<<16) == 0 {
		return nil, ErrPermissionDenied
	}
	r.AdvanceWars(now)
	if initial && len(g.War.Rivals) > 0 {
		return g, nil
	}
	if len(g.War.Rivals) >= 40 {
		return nil, ErrWarLimit
	}
	existing := map[uint32]bool{g.ID: true}
	for _, id := range g.War.Rivals {
		existing[id] = true
	}
	candidates := []uint32{}
	for id, other := range r.Guilds {
		_, hasGuardian := other.WarGuardian()
		if other != nil && !existing[id] && hasGuardian && len(other.Members) > 0 {
			candidates = append(candidates, id)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	if len(candidates) == 0 {
		return nil, ErrWarNoOpponent
	}
	costIndex := len(g.War.Rivals) / 8
	if costIndex >= len(rules.ChallengeCosts) {
		return nil, ErrWarLimit
	}
	cost := rules.ChallengeCosts[costIndex]
	if initial {
		cost = 0
	}
	if g.GuardianGP < cost {
		return nil, ErrInsufficientGuardian
	}
	g.GuardianGP -= cost
	g.War.Rivals = append(g.War.Rivals, candidates[:min(8, min(len(candidates), 40-len(g.War.Rivals)))]...)
	return g, nil
}

func (r *Registry) StartWar(characterID string, target uint32, rules *WarRules, now time.Time) (*Guild, error) {
	g, m, ok := r.GuildForCharacter(characterID)
	if !ok {
		return nil, ErrNotMember
	}
	if g.Permissions(m.Grade)&(1<<16) == 0 {
		return nil, ErrPermissionDenied
	}
	if !rules.Open(now) {
		return nil, ErrWarClosed
	}
	r.AdvanceWars(now)
	if b := g.War.Battle; b != nil {
		if b.Opponent == target {
			return g, nil
		}
		return nil, ErrWarActive
	}
	ownGuardian, ownReady := g.WarGuardian()
	if !ownReady {
		return nil, ErrWarGuardian
	}
	candidate := false
	for _, id := range g.War.Rivals {
		candidate = candidate || id == target
	}
	enemy := r.Guilds[target]
	enemyGuardian, enemyReady := enemy.WarGuardian()
	if !candidate || enemy == nil || target == g.ID || !enemyReady {
		return nil, ErrWarNoOpponent
	}
	if g.War.Results[target] != 0 {
		return nil, ErrWarLimit
	}
	if g.War.Participants[characterID] >= rules.DailyMax {
		return nil, ErrWarLimit
	}
	g.War.NextBattle++
	end := now.Add(time.Duration(rules.PlaySeconds) * time.Second)
	_, dayEnd := WarDay(now)
	pause := dayEnd.Add(time.Duration(rules.PauseStart-360) * time.Minute)
	if pause.After(now) && pause.Before(end) {
		end = pause
	}
	g.War.Battle = &WarBattle{Key: fmt.Sprintf("%d:%d:%d", g.ID, g.War.NextBattle, now.UnixNano()), Opponent: target, StartedAt: now.Unix(), EndsAt: end.Unix(), EnemyHP: rules.EnemyHP, Clears: make(map[uint32]uint32), Members: make(map[string]bool), Receipts: make(map[string]bool)}
	g.War.Battle.OpponentGuardian, g.War.Battle.OwnGuardian = enemyGuardian, ownGuardian
	return g, nil
}

func (r *Registry) finishWar(g *Guild, won bool) {
	b := g.War.Battle
	if b == nil {
		return
	}
	if g.War.Results == nil {
		g.War.Results = make(map[uint32]uint32)
	}
	enemy := r.Guilds[b.Opponent]
	if won {
		g.War.AttackWins++
		g.War.Results[b.Opponent] = 1
		if enemy != nil {
			enemy.War.DefenceLosses++
		}
	} else {
		g.War.AttackLosses++
		g.War.Results[b.Opponent] = 2
		if enemy != nil {
			enemy.War.DefenceWins++
		}
	}
	g.War.LastBattle = b
	g.War.Battle = nil
}

func (r *Registry) CancelWar(characterID string, now time.Time) (*Guild, error) {
	g, m, ok := r.GuildForCharacter(characterID)
	if !ok {
		return nil, ErrNotMember
	}
	if g.Permissions(m.Grade)&(1<<16) == 0 {
		return nil, ErrPermissionDenied
	}
	r.AdvanceWars(now)
	r.finishWar(g, false)
	return g, nil
}

func (r Registry) WarAdmission(characterID string, dungeon uint32, rules *WarRules, now time.Time) (*Guild, error) {
	g, _, ok := r.GuildForCharacter(characterID)
	if !ok {
		return nil, ErrNotMember
	}
	b := g.War.Battle
	if !rules.Open(now) || b == nil || now.Unix() >= b.EndsAt || !rules.Dungeons[dungeon] {
		return nil, ErrWarNotActive
	}
	if !b.Members[characterID] && g.War.Participants[characterID] >= rules.DailyMax {
		return nil, ErrWarLimit
	}
	if dungeon == rules.EnemyDungeon {
		unlocked := false
		for _, id := range rules.RequiredClear {
			unlocked = unlocked || b.Clears[id] > 0
		}
		if !unlocked {
			return nil, ErrInvalidContent
		}
	}
	return g, nil
}

func (r *Registry) JoinWar(characterID, key string, dungeon uint32, rules *WarRules, now time.Time) (bool, error) {
	g, err := r.WarAdmission(characterID, dungeon, rules, now)
	if err != nil {
		return false, err
	}
	b := g.War.Battle
	if b.Key != key {
		return false, ErrWarNotActive
	}
	if b.Members[characterID] {
		return false, nil
	}
	if b.Members == nil {
		b.Members = make(map[string]bool)
	}
	if g.War.Participants == nil {
		g.War.Participants = make(map[string]uint32)
	}
	b.Members[characterID] = true
	g.War.Participants[characterID]++
	return true, nil
}

func (r *Registry) ClearWar(characterID, key, receipt string, dungeon uint32, rules *WarRules, now time.Time) error {
	g, _, ok := r.GuildForCharacter(characterID)
	if !ok {
		return ErrNotMember
	}
	b := g.War.Battle
	if last := g.War.LastBattle; last != nil && last.Key == key && last.Members[characterID] && last.Receipts[receipt] {
		return nil
	}
	if b == nil || b.Key != key || !b.Members[characterID] || now.Unix() >= b.EndsAt || !rules.Dungeons[dungeon] || receipt == "" {
		return ErrWarNotActive
	}
	if b.Receipts[receipt] {
		return nil
	}
	if b.Receipts == nil {
		b.Receipts = make(map[string]bool)
	}
	if b.Clears == nil {
		b.Clears = make(map[uint32]uint32)
	}
	b.Receipts[receipt] = true
	b.Clears[dungeon]++
	if dungeon == rules.EnemyDungeon {
		b.EnemyHP -= min(b.EnemyHP, rules.ClearDamage)
		if b.EnemyHP == 0 {
			r.finishWar(g, true)
		}
	}
	return nil
}
