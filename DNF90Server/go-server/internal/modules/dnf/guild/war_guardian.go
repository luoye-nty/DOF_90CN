package guild

import dnfpvf "longheng.io/server/internal/modules/dnf/pvf"

type WarGuardianCatalog struct {
	Dummy    uint32
	Monsters map[uint32][]uint32
}

func LoadWarGuardians(source dnfpvf.Source) (*WarGuardianCatalog, error) {
	doc, err := readWarDocument(source, "etc/agitwar.etc")
	if err != nil {
		return nil, err
	}
	tokens, _ := doc.Section("guardian dummy monster")
	v, err := warUnsigned(tokens)
	if err != nil || len(v) != 1 || v[0] == 0 {
		return nil, ErrInvalidContent
	}
	c := &WarGuardianCatalog{Dummy: v[0], Monsters: make(map[uint32][]uint32)}
	inside := false
	var id uint32
	for _, sec := range doc.Sections {
		switch sec.Name {
		case "guardian list":
			inside = true
		case "/guardian list":
			inside = false
		case "guardian":
			if inside {
				values, e := warUnsigned(doc.Tokens[sec.Start:sec.End])
				if e != nil || len(values) != 1 || values[0] == 0 {
					return nil, ErrInvalidContent
				}
				id = values[0]
			}
		case "/guardian":
			id = 0
		case "index":
			if inside && id != 0 {
				values, e := warUnsigned(doc.Tokens[sec.Start:sec.End])
				if e != nil || len(values) != 5 || c.Monsters[id] != nil {
					return nil, ErrInvalidContent
				}
				for _, value := range values {
					if value == 0 {
						return nil, ErrInvalidContent
					}
				}
				c.Monsters[id] = values
			}
		}
	}
	if len(c.Monsters) == 0 {
		return nil, ErrInvalidContent
	}
	return c, nil
}

func (g *Guild) WarGuardian() (Guardian, bool) {
	if g != nil {
		for _, guardian := range g.Guardians {
			if guardian.ID == g.ActiveGuardian && guardian.ID != 0 && guardian.Level >= 1 && guardian.Level <= 50 {
				return guardian, true
			}
		}
	}
	return Guardian{}, false
}

func (c *WarGuardianCatalog) Monster(guardian Guardian) (uint32, error) {
	if c == nil || guardian.Level < 1 || guardian.Level > 50 {
		return 0, ErrInvalidContent
	}
	ids := c.Monsters[guardian.ID]
	// Native D4F69C..D4F6B5 derives the guardian star from (level-1)/10.
	star := int(guardian.Level-1) / 10
	if star >= len(ids) {
		return 0, ErrInvalidContent
	}
	return ids[star], nil
}
