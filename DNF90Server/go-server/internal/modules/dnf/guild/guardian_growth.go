package guild

import (
	"fmt"
	"math"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

type GuardianGrowth struct {
	Experience  map[byte]GuardianCost
	Skills      map[byte]GuardianCost
	SkillLimits map[uint32][5]byte
}

func readWarDocument(source dnfpvf.Source, name string) (*dnfpvf.Document, error) {
	if source == nil {
		return nil, ErrRepositoryUnavailable
	}
	text, err := source.ReadText(name)
	if err != nil {
		return nil, err
	}
	return dnfpvf.Parse(name, text)
}

func warUnsigned(tokens []dnfpvf.Token) ([]uint32, error) {
	out := make([]uint32, len(tokens))
	for i, token := range tokens {
		if token.Kind != dnfpvf.TokenInt || token.Int < 0 || token.Int > math.MaxInt32 {
			return nil, ErrInvalidContent
		}
		out[i] = uint32(token.Int)
	}
	return out, nil
}

func warCostTriples(doc *dnfpvf.Document, section string) (map[byte]GuardianCost, error) {
	tokens, _ := doc.Section(section)
	values, err := warUnsigned(tokens)
	if err != nil || len(values) == 0 || len(values)%3 != 0 || len(values)/3 >= 50 {
		return nil, ErrInvalidContent
	}
	out := make(map[byte]GuardianCost)
	for i := 0; i < len(values); i += 3 {
		if values[i] != uint32(i/3+1) {
			return nil, ErrInvalidContent
		}
		out[byte(values[i])] = GuardianCost{values[i+1], values[i+2]}
	}
	return out, nil
}

func LoadGuardianGrowth(source dnfpvf.Source) (GuardianGrowth, error) {
	var out GuardianGrowth
	doc, err := readWarDocument(source, "etc/agitwar.etc")
	if err != nil {
		return out, err
	}
	out.Experience, err = warCostTriples(doc, "guardian exp")
	if err != nil {
		return out, err
	}
	out.Skills, err = warCostTriples(doc, "guardian skill")
	if err != nil {
		return out, err
	}
	// D01005..D01094 indexes the cost map by the current skill level.
	// Its native map insertion at level 0 creates the free initial unlock.
	out.Skills[0] = GuardianCost{}
	out.SkillLimits = make(map[uint32][5]byte)
	var id uint32
	inList := false
	for _, sec := range doc.Sections {
		tokens := doc.Tokens[sec.Start:sec.End]
		switch sec.Name {
		case "guardian list":
			inList = true
		case "/guardian list":
			inList = false
		case "guardian":
			if inList {
				v, e := warUnsigned(tokens)
				if e != nil || len(v) != 1 || v[0] == 0 {
					return out, ErrInvalidContent
				}
				id = v[0]
			}
		case "/guardian":
			id = 0
		default:
			if !inList || id == 0 {
				continue
			}
			for i := 0; i < 5; i++ {
				if sec.Name != fmt.Sprintf("skill_%d", i+1) {
					continue
				}
				if len(tokens) < 3 {
					return out, ErrInvalidContent
				}
				v, e := warUnsigned(tokens[:3])
				if e != nil || v[2] == 0 || v[2] > uint32(len(out.Skills)) {
					return out, ErrInvalidContent
				}
				limits := out.SkillLimits[id]
				if limits[i] != 0 {
					return out, ErrInvalidContent
				}
				limits[i] = byte(v[2])
				out.SkillLimits[id] = limits
			}
		}
	}
	if len(out.SkillLimits) == 0 {
		return out, ErrInvalidContent
	}
	for _, limits := range out.SkillLimits {
		for _, v := range limits {
			if v == 0 {
				return out, ErrInvalidContent
			}
		}
	}
	for level := byte(2); int(level) <= len(out.Experience); level++ {
		prev, next := out.Experience[level-1], out.Experience[level]
		if next.AP < prev.AP || next.GP < prev.GP {
			return out, ErrInvalidContent
		}
	}
	return out, nil
}

func (r *Registry) warManager(characterID string) (*Guild, error) {
	g, m, found := r.GuildForCharacter(characterID)
	if !found {
		return nil, ErrNotMember
	}
	// Native guardian controls check guild permission index 16 (26A3BE0).
	if g.Permissions(m.Grade)&(1<<16) == 0 {
		return nil, ErrPermissionDenied
	}
	return g, nil
}

func (r *Registry) GrowGuardian(characterID string, action byte, id, before, amount uint32, catalog GuardianGrowth, now time.Time) error {
	g, err := r.warManager(characterID)
	if err != nil {
		return err
	}
	limits, valid := catalog.SkillLimits[id]
	if !valid || action > 6 || amount == 0 {
		return ErrInvalidContent
	}
	var guardian *Guardian
	for i := range g.Guardians {
		if g.Guardians[i].ID == id {
			guardian = &g.Guardians[i]
			break
		}
	}
	if guardian == nil || guardian.Level == 0 || int(guardian.Level) > len(catalog.Experience)+1 {
		return ErrInvalidContent
	}
	if action >= 2 {
		i := int(action - 2)
		if i >= int((guardian.Level-1)/10+1) || amount != before+1 || amount > uint32(limits[i]) {
			return ErrInvalidContent
		}
		if uint32(guardian.Skills[i]) == amount {
			return nil
		}
		cost, ok := catalog.Skills[byte(before)]
		if before > math.MaxUint8 || !ok || uint32(guardian.Skills[i]) != before {
			return ErrInvalidContent
		}
		if g.GuardianAP < cost.AP || g.GuardianGP < cost.GP {
			return ErrInsufficientGuardian
		}
		g.GuardianAP -= cost.AP
		g.GuardianGP -= cost.GP
		guardian.Skills[i] = byte(amount)
	} else {
		exp, balance := &guardian.APExp, &g.GuardianAP
		if action == 1 {
			exp, balance = &guardian.GPExp, &g.GuardianGP
		}
		if uint64(before)+uint64(amount) > math.MaxInt32 {
			return ErrInvalidContent
		}
		if *exp == before+amount {
			return nil
		}
		need, ok := catalog.Experience[guardian.Level]
		if !ok || before != *exp {
			return ErrInvalidContent
		}
		limit := need.AP
		if action == 1 {
			limit = need.GP
		}
		if before+amount > limit {
			return ErrInvalidContent
		}
		if *balance < amount {
			return ErrInsufficientGuardian
		}
		*balance -= amount
		*exp += amount
		// The native progress bars display absolute AP/GP experience against
		// the current level's PVF thresholds; retain it across level changes.
		if guardian.APExp >= need.AP && guardian.GPExp >= need.GP {
			guardian.Level++
		}
	}
	g.UpdatedAt = now
	return nil
}
