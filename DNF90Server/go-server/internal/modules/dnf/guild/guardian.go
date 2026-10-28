package guild

import (
	"fmt"
	"math"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

type GuardianCost struct {
	AP uint32
	GP uint32
}

func LoadAreaCosts(source dnfpvf.Source) (map[byte]GuardianCost, error) {
	if source == nil {
		return nil, ErrRepositoryUnavailable
	}
	text, err := source.ReadText("etc/agitwar.etc")
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse("etc/agitwar.etc", text)
	if err != nil {
		return nil, err
	}
	tokens, _ := doc.Section("area extend")
	if len(tokens) == 0 || len(tokens)%3 != 0 || len(tokens)/3 > math.MaxUint8 {
		return nil, ErrInvalidContent
	}
	costs := make(map[byte]GuardianCost)
	for i := 0; i < len(tokens); i += 3 {
		for _, token := range tokens[i : i+3] {
			if token.Kind != dnfpvf.TokenInt || token.Int < 0 || token.Int > math.MaxInt32 {
				return nil, ErrInvalidContent
			}
		}
		if tokens[i].Int != int64(i/3+1) {
			return nil, ErrInvalidContent
		}
		costs[byte(tokens[i].Int)] = GuardianCost{AP: uint32(tokens[i+1].Int), GP: uint32(tokens[i+2].Int)}
	}
	return costs, nil
}

func (r *Registry) ExtendArea(characterID string, level byte, costs map[byte]GuardianCost, now time.Time) error {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return ErrNotMember
	}
	if member.Grade != DefaultMasterGrade || guild.MasterCharacterID != characterID {
		return ErrPermissionDenied
	}
	cost, valid := costs[level]
	if !valid || level == 0 || int(level) != int(guild.AreaLevel)+1 {
		return ErrInvalidContent
	}
	if guild.GuardianAP < cost.AP || guild.GuardianGP < cost.GP {
		return ErrInsufficientGuardian
	}
	guild.GuardianAP -= cost.AP
	guild.GuardianGP -= cost.GP
	guild.AreaLevel, guild.UpdatedAt = level, now
	return nil
}

func LoadGuardianCosts(source dnfpvf.Source) (map[uint32]GuardianCost, error) {
	if source == nil {
		return nil, ErrRepositoryUnavailable
	}
	text, err := source.ReadText("etc/agitwar.etc")
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse("etc/agitwar.etc", text)
	if err != nil {
		return nil, err
	}
	costs := make(map[uint32]GuardianCost)
	var id uint32
	var cost GuardianCost
	var inList, haveAP, haveGP bool
	invalid := fmt.Errorf("guild guardian catalog: %w", ErrInvalidContent)
	for _, section := range doc.Sections {
		tokens := doc.Tokens[section.Start:section.End]
		switch section.Name {
		case "guardian list":
			inList = true
		case "/guardian list":
			inList = false
		case "guardian", "ap", "gp":
			if !inList || (section.Name != "guardian" && id == 0) {
				continue
			}
			if len(tokens) != 1 || tokens[0].Kind != dnfpvf.TokenInt || tokens[0].Int < 0 || tokens[0].Int > math.MaxInt32 {
				return nil, invalid
			}
			value := uint32(tokens[0].Int)
			switch section.Name {
			case "guardian":
				if id != 0 || value == 0 {
					return nil, invalid
				}
				id, cost, haveAP, haveGP = value, GuardianCost{}, false, false
			case "ap":
				if haveAP {
					return nil, invalid
				}
				cost.AP, haveAP = value, true
			case "gp":
				if haveGP {
					return nil, invalid
				}
				cost.GP, haveGP = value, true
			}
		case "/guardian":
			if !inList {
				continue
			}
			if _, duplicate := costs[id]; duplicate || id == 0 || !haveAP || !haveGP {
				return nil, invalid
			}
			costs[id], id = cost, 0
		}
	}
	if id != 0 || inList || len(costs) == 0 || len(costs) > 5 {
		return nil, invalid
	}
	return costs, nil
}
