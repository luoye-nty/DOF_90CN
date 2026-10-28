package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// alignedDirectExperienceConsumablePlannerForCommand owns the four immediate
// character-EXP families declared by [increase status type]. The active PVF
// supplies both the item operands and the cumulative EXP/SP/TP tables.
func (s *Service) alignedDirectExperienceConsumablePlannerForCommand(opcode dnfenum.CmdPacket) (alignedcmd.DirectExperienceConsumablePlanner, error) {
	if opcode != dnfenum.CmdPacketUseStackable && opcode != dnfenum.CmdPacketRequestEventServerLevelUp &&
		opcode != dnfenum.CmdPacketIncreaseStatus {
		// op30 is the route the client actually takes for the character
		// experience books, so it needs the same PVF-backed planner.
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return func(
		character dnfrepo.CharacterRecord,
		skill dnfrepo.SkillRecord,
		resolution alignedcmd.DirectExperienceConsumableResolution,
		updatedAt time.Time,
	) (alignedcmd.DirectExperienceConsumablePlan, error) {
		tables, loadErr := progression.Load(context.Background(), catalog.source)
		if loadErr != nil {
			return alignedcmd.DirectExperienceConsumablePlan{}, loadErr
		}
		return s.planDirectExperienceConsumable(tables, character, skill, resolution, updatedAt)
	}, nil
}

func (s *Service) planDirectExperienceConsumable(
	tables *progression.Tables,
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	resolution alignedcmd.DirectExperienceConsumableResolution,
	updatedAt time.Time,
) (alignedcmd.DirectExperienceConsumablePlan, error) {
	if tables == nil || character.CharacterID == "" || skill.CharacterID != character.CharacterID ||
		character.Level <= 0 || character.Level >= currentAdventureCharacterLevelCap ||
		skill.Points.SyncedLevel != character.Level || character.Stats == nil {
		return alignedcmd.DirectExperienceConsumablePlan{}, fmt.Errorf("direct experience persisted progression state is invalid")
	}
	if resolution.MinimumLevel > 0 && int64(character.Level) < resolution.MinimumLevel {
		return alignedcmd.DirectExperienceConsumablePlan{}, fmt.Errorf("direct experience requires level %d", resolution.MinimumLevel)
	}
	if resolution.MaximumLevel > 0 && int64(character.Level) > resolution.MaximumLevel {
		return alignedcmd.DirectExperienceConsumablePlan{}, fmt.Errorf("direct experience maximum level is %d", resolution.MaximumLevel)
	}
	totalValue, present := character.Stats["exp"]
	if !present || totalValue < 0 || uint64(totalValue) > math.MaxUint32 {
		return alignedcmd.DirectExperienceConsumablePlan{}, fmt.Errorf("direct experience total is invalid")
	}
	total := uint32(totalValue)
	gain, err := directExperienceGain(tables, character.Level, total, resolution)
	if err != nil {
		return alignedcmd.DirectExperienceConsumablePlan{}, err
	}
	planned, err := tables.PlanExperienceAndSkillPoints(
		character.Level,
		total,
		gain,
		currentAdventureCharacterLevelCap,
		skill.Points,
	)
	if err != nil {
		return alignedcmd.DirectExperienceConsumablePlan{}, err
	}
	nextCharacter := dnfrepo.CloneCharacter(character)
	nextSkill := dnfrepo.CloneSkill(skill)
	nextCharacter.Level = planned.Experience.NewLevel
	nextCharacter.Stats["exp"] = int64(planned.Experience.NewExperience)
	characterFields := []dnfrepo.CharacterField{dnfrepo.CharacterFieldStats}
	skillFields := make([]dnfrepo.SkillField, 0, 1)
	if planned.Experience.LevelsGained > 0 {
		levelStats, statErr := s.characterPVFStatValuesForLevel(character, planned.Experience.NewLevel)
		if statErr != nil {
			return alignedcmd.DirectExperienceConsumablePlan{}, statErr
		}
		for key, value := range levelStats {
			nextCharacter.Stats[key] = value
		}
		nextSkill.Points = planned.SkillPoints.New
		characterFields = append(characterFields, dnfrepo.CharacterFieldBase)
		skillFields = append(skillFields, dnfrepo.SkillFieldPoints)
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	nextCharacter.UpdatedAt = updatedAt
	nextSkill.UpdatedAt = updatedAt
	return alignedcmd.DirectExperienceConsumablePlan{
		Character:          nextCharacter,
		Skill:              nextSkill,
		CharacterFields:    characterFields,
		SkillFields:        skillFields,
		Gain:               gain,
		PreviousLevel:      planned.Experience.PreviousLevel,
		NewLevel:           planned.Experience.NewLevel,
		PreviousExperience: planned.Experience.PreviousExperience,
		NewExperience:      planned.Experience.NewExperience,
		SPGain:             planned.SkillPoints.SPGain,
		TPGain:             planned.SkillPoints.TPGain,
	}, nil
}

func directExperienceGain(
	tables *progression.Tables,
	level int,
	total uint32,
	resolution alignedcmd.DirectExperienceConsumableResolution,
) (uint32, error) {
	if tables == nil || level <= 0 {
		return 0, fmt.Errorf("direct experience tables or level are invalid")
	}
	values := resolution.EffectValues
	switch normalizeDirectExperienceType(resolution.EffectType) {
	case "[expup]":
		if err := validateDirectExperienceValues("[expup]", values); err != nil {
			return 0, err
		}
		return uint32(values[0]), nil
	case "[exppercentup]":
		if err := validateDirectExperienceValues("[exppercentup]", values); err != nil {
			return 0, err
		}
		upper, err := tables.ThresholdToNext(level)
		if err != nil {
			return 0, err
		}
		lower := uint32(0)
		if level > 1 {
			lower, err = tables.ThresholdToNext(level - 1)
			if err != nil {
				return 0, err
			}
		}
		if upper <= lower {
			return 0, fmt.Errorf("direct experience level span is invalid")
		}
		gain := uint64(upper-lower) * uint64(values[0]) / 100
		if gain == 0 || gain > math.MaxUint32 {
			return 0, fmt.Errorf("direct experience percentage result is invalid")
		}
		return uint32(gain), nil
	case "[expupbytable]":
		if err := validateDirectExperienceValues("[expupbytable]", values); err != nil {
			return 0, err
		}
		for index := 0; index < len(values); index += 3 {
			if int64(level) >= values[index] && int64(level) <= values[index+1] {
				return uint32(values[index+2]), nil
			}
		}
		return 0, fmt.Errorf("direct experience table has no row for level %d", level)
	case "[levelupbytable]":
		if err := validateDirectExperienceValues("[levelupbytable]", values); err != nil {
			return 0, err
		}
		for index := 0; index < len(values); index += 3 {
			if int64(level) < values[index] || int64(level) > values[index+1] {
				continue
			}
			targetLevel := int(values[index+2])
			if targetLevel <= level || targetLevel > currentAdventureCharacterLevelCap {
				return 0, fmt.Errorf("direct target level %d is invalid for level %d", targetLevel, level)
			}
			targetTotal, thresholdErr := tables.ThresholdToNext(targetLevel - 1)
			if thresholdErr != nil {
				return 0, thresholdErr
			}
			if targetTotal <= total {
				return 0, fmt.Errorf("direct target level experience is not above current total")
			}
			return targetTotal - total, nil
		}
		return 0, fmt.Errorf("direct target-level table has no row for level %d", level)
	case "[level up ticket]":
		if len(values) != 1 || values[0] <= 0 || values[0] > int64(currentAdventureCharacterLevelCap) {
			return 0, fmt.Errorf("invalid level-up ticket range %v", values)
		}
		targetLevel := level + int(values[0])
		if targetLevel > currentAdventureCharacterLevelCap {
			return 0, fmt.Errorf("level-up ticket target level %d exceeds cap %d", targetLevel, currentAdventureCharacterLevelCap)
		}
		targetTotal, thresholdErr := tables.ThresholdToNext(targetLevel - 1)
		if thresholdErr != nil {
			return 0, thresholdErr
		}
		if targetTotal <= total {
			return 0, fmt.Errorf("level-up ticket target experience is not above current total")
		}
		return targetTotal - total, nil
	default:
		return 0, fmt.Errorf("unsupported direct experience type %q", resolution.EffectType)
	}
}
