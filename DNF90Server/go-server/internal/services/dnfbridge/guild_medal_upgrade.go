package dnfbridge

import (
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// alignedGuildMedalUpgradePolicyResolver resolves one 公会徽章 (guild flag) upgrade.
//
// The two forks declare alignedcmd.UpgradePolicyResolver differently:
//
//   - upstream: func(mode string, currentLevel int) (Resolution, error)
//   - this tree: func(mode string, currentLevel int, targetItemID int64, amplifyType int) (Resolution, error)
//
// The wider form exists because this tree's equipment path needs the request's own
// target item and the amplify/reinforce discriminator. Neither is needed here: the
// flag being upgraded is already bound into itemID by the caller, and
// resolveCurrentGuildMedalUpgrade reads its policy from the flag_upgrade_table row
// for the given level. They are therefore ignored rather than threaded through.
func (s *Service) alignedGuildMedalUpgradePolicyResolver(itemID uint32) alignedcmd.UpgradePolicyResolver {
	return func(mode string, currentLevel int, _ int64, _ int) (alignedcmd.UpgradePolicyResolution, error) {
		catalog, err := s.currentPVFItemCatalog()
		if err != nil {
			return alignedcmd.UpgradePolicyResolution{}, err
		}
		return resolveCurrentGuildMedalUpgrade(catalog, itemID, mode, currentLevel)
	}
}

func resolveCurrentGuildMedalUpgrade(catalog *pvfDungeonDropCatalog, itemID uint32, mode string, level int) (alignedcmd.UpgradePolicyResolution, error) {
	var result alignedcmd.UpgradePolicyResolution
	if _, err := resolveCurrentGuardianGemMedal(catalog, itemID); err != nil {
		return result, err
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return result, err
	}
	text, err := catalog.source.ReadText(currentUpgradePVFPath)
	if err != nil {
		return result, err
	}
	doc, err := dnfpvf.Parse(currentUpgradePVFPath, text)
	if err != nil {
		return result, err
	}
	var values []float64
	for _, section := range doc.Sections {
		if section.Name != "table type" {
			continue
		}
		tokens := doc.Tokens[section.Start:section.End]
		if len(tokens) == 0 || tokens[0].Value != "flag_upgrade_table" {
			continue
		}
		for _, token := range tokens[1:] {
			switch token.Kind {
			case dnfpvf.TokenInt:
				values = append(values, float64(token.Int))
			case dnfpvf.TokenFloat:
				values = append(values, token.Float)
			default:
				return result, fmt.Errorf("invalid flag upgrade table token %q", token.Raw)
			}
		}
		break
	}
	var row []float64
	for current, offset := 0, 0; offset < len(values); current++ {
		parsed, next, ok := nextCurrentUpgradeTableRowValues(values, offset)
		if !ok {
			return result, fmt.Errorf("invalid flag upgrade row %d", current)
		}
		if current == level {
			row = parsed
			break
		}
		offset = next
	}
	weights := doc.Numbers("cost weights by guild flag rarity")
	if mode != "reinforce" || level < 0 || len(row) == 0 || definition.Rarity < 0 || definition.Rarity >= int64(len(weights)) {
		return result, fmt.Errorf("flag upgrade policy missing: item=%d rarity=%d level=%d", itemID, definition.Rarity, level)
	}
	tail := row[len(row)-currentUpgradeTableTailWidth:]
	// sub_3866046 multiplies integer material count by an f32 weight in x87,
	// then truncates without first rounding the product back to f32.
	count := tail[7] * float64(float32(weights[definition.Rarity]))
	if math.IsNaN(count) || count < 1 || count > math.MaxInt32 || tail[0] < 0 || tail[0] > currentUpgradeSuccessBase || tail[2] < 1 || tail[2] > 3 || tail[3] < 0 {
		return result, fmt.Errorf("invalid flag upgrade row at level %d", level)
	}
	penalty := int(tail[2])
	if penalty == 1 {
		penalty = 0
	} // Native flag policy: retain, reduce by value, destroy.
	result = alignedcmd.UpgradePolicyResolution{
		GuildMedal: true, CurrentLevel: level, GoldCost: 1,
		SuccessWeight: currentUpgradeSuccessBase - int(tail[0]),
		PenaltyType:   penalty, PenaltyValue: int(tail[3]),
		MaterialItemID: int(tail[6]), MaterialCount: int(count), NoticeLevel: -1,
	}
	return result, nil
}
