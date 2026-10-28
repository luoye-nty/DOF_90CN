package guild

import (
	"sort"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func LoadWarMissionsCatalog(source dnfpvf.Source) (*WarMissionCatalog, error) {
	doc, err := readWarDocument(source, "etc/agitwar.etc")
	if err != nil {
		return nil, err
	}
	out := &WarMissionCatalog{}
	inSystem := false
	var m *WarMission
	categorySeen := false
	seen := make(map[uint32]bool)
	for _, sec := range doc.Sections {
		t := doc.Tokens[sec.Start:sec.End]
		if sec.Name == "mission system" {
			inSystem = true
			continue
		}
		if sec.Name == "/mission system" {
			if m != nil {
				return nil, ErrInvalidContent
			}
			inSystem = false
			continue
		}
		if !inSystem {
			continue
		}
		if sec.Name == "daily title mission index" || sec.Name == "season title mission index" {
			v, e := warUnsigned(t)
			if e != nil || len(v) != 1 || v[0] == 0 {
				return nil, ErrInvalidContent
			}
			if sec.Name == "daily title mission index" {
				out.DailyTitle = v[0]
			} else {
				out.SeasonTitle = v[0]
			}
			continue
		}
		if sec.Name == "mission" {
			if m != nil {
				return nil, ErrInvalidContent
			}
			m = &WarMission{}
			categorySeen = false
			continue
		}
		if sec.Name == "/mission" {
			if m == nil || !categorySeen {
				return nil, ErrInvalidContent
			}
			if m.ID == 0 || seen[m.ID] || m.Title == "" || m.Required == 0 || len(m.Rewards) == 0 || (m.Condition != "join" && m.Condition != "clear" && m.Condition != "mission") {
				return nil, invalidWarMission(m.ID)
			}
			seen[m.ID] = true
			out.Missions = append(out.Missions, *m)
			m = nil
			continue
		}
		if m == nil {
			continue
		}
		switch sec.Name {
		case "index":
			v, e := warUnsigned(t)
			if e != nil || len(v) != 1 {
				return nil, ErrInvalidContent
			}
			m.ID = v[0]
		case "category":
			if categorySeen {
				return nil, invalidWarMission(m.ID)
			}
			categorySeen = true
			if len(t) != 1 || (t[0].Value != "daily" && t[0].Value != "season") {
				return nil, invalidWarMission(m.ID)
			}
			m.Season = t[0].Value == "season"
		case "title":
			if len(t) != 1 {
				return nil, invalidWarMission(m.ID)
			}
			m.Title = t[0].Value
		case "condition":
			if len(t) != 2 {
				return nil, invalidWarMission(m.ID)
			}
			v, e := warUnsigned(t[1:])
			if e != nil {
				return nil, e
			}
			m.Condition = t[0].Value
			m.Required = v[0]
		case "dungeon":
			v, e := warUnsigned(t)
			if e != nil || len(v) == 0 {
				return nil, invalidWarMission(m.ID)
			}
			m.Dungeons = v
		case "reward":
			v, e := warUnsigned(t)
			if e != nil || len(v) == 0 || len(v)%2 != 0 {
				return nil, invalidWarMission(m.ID)
			}
			for i := 0; i < len(v); i += 2 {
				if v[i] == 0 || v[i+1] == 0 {
					return nil, invalidWarMission(m.ID)
				}
				m.Rewards = append(m.Rewards, WarMissionReward{v[i], v[i+1]})
			}
		}
	}
	if inSystem || m != nil || len(out.Missions) == 0 || len(out.Missions) > 1024 {
		return nil, ErrInvalidContent
	}
	for _, title := range []struct {
		id     uint32
		season bool
	}{{out.DailyTitle, false}, {out.SeasonTitle, true}} {
		m, ok := out.Find(title.id)
		if !ok || m.Season != title.season || m.Condition != "mission" {
			return nil, ErrInvalidContent
		}
	}
	sort.Slice(out.Missions, func(i, j int) bool { return out.Missions[i].ID < out.Missions[j].ID })
	return out, nil
}
