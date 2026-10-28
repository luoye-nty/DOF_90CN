package dnfbridge

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentUpgradeSuccessBase    = 100000
	currentUpgradeNoticeDefault  = -1
	currentUpgradePVFPath        = "etc/upgrade.etc"
	currentAmplifyUpgradePVFPath = "etc/amplifyupgrade.etc"

	currentUpgradeTableShortRowWidth = 16
	currentUpgradeTableFullRowWidth  = 24
	currentUpgradeTableTailWidth     = 8
)

// currentUpgradeTableRow is one level row from [table type] in etc/upgrade.etc.
//
// The row's values[1] is deliberately not carried. It is a weight inside the
// success/penalty table, not the coefficient the client prices with; charging
// from it over-charged 增幅 by 4.5x (live 2026-09-25, see
// currentUpgradeTable.CostWeightByUpgradeLevel).
type currentUpgradeTableRow struct {
	TargetLevel    int
	FailureWeight  int // out of 100000
	PenaltyType    int // 0=no change, 1=one-level downgrade, 2=value downgrade, 3=destroy
	PenaltyValue   int
	MaterialItemID int
	MaterialCount  int
}

func (r currentUpgradeTableRow) SuccessWeight() int {
	w := currentUpgradeSuccessBase - r.FailureWeight
	if w < 0 {
		return 0
	}
	return w
}

// currentUpgradeTable is the parsed PVF upgrade/amplify configuration.
type currentUpgradeTable struct {
	Rows                []currentUpgradeTableRow
	NoticeLevel         int
	Costs               []int
	CostWeightsByRarity []float64
	TypeWeights         []float64
	// CostWeightByUpgradeLevel is [cost weight by upgrade level], indexed by the
	// *current* upgrade level (0-based). It is the client's own per-level gold
	// coefficient, and the 强化器 / 增幅器 popup price is exactly
	//
	//	[cost][grade] * [cost weights by rarity][rarity] * [type][slot] * this
	//
	// Empty means the file carries no such section: that is the 增幅 case
	// (etc/amplifyupgrade.etc) and the client then prices at the base cost with
	// no level factor at all.
	CostWeightByUpgradeLevel []float64
	DestroyLevelByRarity     map[string]int
	DisjointBonusItemID      int
	DisjointEquipLevelConst  int
	DisjointWeightByRarity   map[string]float64
	DisjointLevelConst       int
	// DisjointCorrectionGradeByRarity is upgrade.etc [disjoint] [correction
	// grade by rarity]: the equipment grade is lowered by this much before it
	// is compared with [equip level const].
	DisjointCorrectionGradeByRarity map[string]float64
}

func (t *currentUpgradeTable) RowForLevel(level int) (currentUpgradeTableRow, bool) {
	if level < 1 || level > len(t.Rows) {
		return currentUpgradeTableRow{}, false
	}
	return t.Rows[level-1], true
}

// costWeightForCurrentLevel returns the client's gold coefficient for an attempt
// that starts at currentLevel (i.e. targetLevel-1).
//
// Three cases, each pinned by a live reading rather than by preference:
//
//   - The section exists and covers the level: use it. etc/upgrade.etc carries
//     19 `level weight weight` entries for +0..+18, and they reproduce the 强化器
//     popup at +0->+1 (0.50), +1->+2 (0.50) and +14->+15 (8.50).
//   - The section is missing: coefficient 1, the bare base price. That is 增幅,
//     whose etc/amplifyupgrade.etc has no such section; its popup quoted exactly
//     44100 * 2.50 * 2.00 = 220500 for a +10->+11 attempt while the old
//     weight*25 term charged 992250.
//   - The section exists but is unusable (not a clean level-0..n-1 triple run, or
//     a non-positive weight): also 1. Never zero -- an unreadable PVF must not
//     hand the attempt out for free.
//
// Levels past the section's last entry hold that last entry instead of dropping
// to 1: [max upgrade level by rarity] allows far beyond +18, and falling back
// would make +19 cheaper than +18.
func (t *currentUpgradeTable) costWeightForCurrentLevel(currentLevel int) float64 {
	weights := t.CostWeightByUpgradeLevel
	if len(weights) == 0 {
		return 1
	}
	if currentLevel < 0 {
		return weights[0]
	}
	if currentLevel >= len(weights) {
		return weights[len(weights)-1]
	}
	return weights[currentLevel]
}

func (t *currentUpgradeTable) DestroyLevel(rarity int) int {
	name := currentUpgradeRarityName(rarity)
	if lv, ok := t.DestroyLevelByRarity[name]; ok {
		return lv
	}
	return -1
}

var (
	currentUpgradeTableMu      sync.Mutex
	currentUpgradeTableCache   *currentUpgradeTable
	currentUpgradeTableLoadErr error
	currentAmplifyTableCache   *currentUpgradeTable
	currentAmplifyTableLoadErr error
)

var currentUpgradeTokenRE = regexp.MustCompile("`[^`]*`|-?\\d+(?:\\.\\d+)?")

func (s *Service) currentUpgradeTableForMode(mode string) (*currentUpgradeTable, error) {
	currentUpgradeTableMu.Lock()
	defer currentUpgradeTableMu.Unlock()
	path := currentUpgradePVFPath
	if mode == "amplify" {
		if currentAmplifyTableCache != nil {
			return currentAmplifyTableCache, nil
		}
		if currentAmplifyTableLoadErr != nil {
			return nil, currentAmplifyTableLoadErr
		}
		path = currentAmplifyUpgradePVFPath
	} else {
		if currentUpgradeTableCache != nil {
			return currentUpgradeTableCache, nil
		}
		if currentUpgradeTableLoadErr != nil {
			return nil, currentUpgradeTableLoadErr
		}
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.setCurrentUpgradeTableLoadErrLocked(mode, err)
		s.logPacketEvent("dnf-upgrade-table-load-failed", "mode", mode, "path", path, "stage", "open_pvf", "error", err)
		return nil, err
	}
	text, err := archive.ReadText(path)
	if err != nil {
		loadErr := fmt.Errorf("read %s: %w", path, err)
		s.setCurrentUpgradeTableLoadErrLocked(mode, loadErr)
		s.logPacketEvent("dnf-upgrade-table-load-failed", "mode", mode, "path", path, "stage", "read", "error", loadErr)
		return nil, loadErr
	}
	doc, err := dnfpvf.Parse(path, text)
	if err != nil {
		loadErr := fmt.Errorf("parse %s: %w", path, err)
		s.setCurrentUpgradeTableLoadErrLocked(mode, loadErr)
		s.logPacketEvent("dnf-upgrade-table-load-failed", "mode", mode, "path", path, "stage", "parse", "error", loadErr)
		return nil, loadErr
	}
	table := parseCurrentUpgradeTable(doc, text)
	if mode == "amplify" {
		currentAmplifyTableCache = table
	} else {
		currentUpgradeTableCache = table
	}
	s.logPacketEvent("dnf-upgrade-table-loaded", "mode", mode, "rows", len(table.Rows), "notice_level", table.NoticeLevel)
	return table, nil
}

func (s *Service) setCurrentUpgradeTableLoadErrLocked(mode string, err error) {
	if mode == "amplify" {
		currentAmplifyTableLoadErr = err
		return
	}
	currentUpgradeTableLoadErr = err
}

func parseCurrentUpgradeTable(doc *dnfpvf.Document, rawText string) *currentUpgradeTable {
	table := &currentUpgradeTable{
		NoticeLevel:             currentUpgradeNoticeDefault,
		DestroyLevelByRarity:    make(map[string]int),
		DisjointWeightByRarity:  make(map[string]float64),
		DisjointBonusItemID:     -1,
		DisjointEquipLevelConst: -1,
		DisjointLevelConst:      -1,

		DisjointCorrectionGradeByRarity: make(map[string]float64),
	}
	// Parse [table] rows by their authoritative gameplay tail. The current
	// runtime PVF is not the old fixed-17-column shape: target level +1 is a
	// short row, later target levels are full rows, and both shapes end with
	// failure/penalty controls plus material item/count.
	tableTokens := extractCurrentUpgradeTableTokens(rawText, "table")
	for offset := 0; offset < len(tableTokens); {
		values, next, ok := nextCurrentUpgradeTableRowValues(tableTokens, offset)
		if !ok {
			break
		}
		tail := values[len(values)-currentUpgradeTableTailWidth:]
		// values[1] is intentionally not read here. It was charged as the gold
		// coefficient until 2026-09-25 and is not one: the 增幅器 popup quoted
		// 220500 while the server took 992250, because values[1] (0.18) * 25 was
		// multiplied onto a price the client never scaled. Gold now comes from
		// [cost weight by upgrade level] only.
		table.Rows = append(table.Rows, currentUpgradeTableRow{
			TargetLevel:    len(table.Rows) + 1,
			FailureWeight:  int(math.Round(tail[0])),
			PenaltyType:    int(math.Round(tail[2])),
			PenaltyValue:   int(math.Round(tail[3])),
			MaterialItemID: int(math.Round(tail[6])),
			MaterialCount:  int(math.Round(tail[7])),
		})
		offset = next
	}
	// [notice]
	if values := doc.Ints("notice"); len(values) > 0 {
		table.NoticeLevel = int(values[0])
	}
	// [cost]
	for _, v := range doc.Ints("cost") {
		table.Costs = append(table.Costs, int(v))
	}
	// [cost weights by rarity]
	table.CostWeightsByRarity = doc.Numbers("cost weights by rarity")
	// [type]
	table.TypeWeights = doc.Numbers("type")
	// [cost weight by upgrade level]
	table.CostWeightByUpgradeLevel = parseCurrentUpgradeCostWeightByUpgradeLevel(doc.Numbers("cost weight by upgrade level"))
	// [destroy level by rarity]
	parseCurrentUpgradeStringIntPairs(rawText, "destroy level by rarity", table.DestroyLevelByRarity)
	// [disjoint] section
	parseCurrentUpgradeDisjoint(rawText, table)
	return table
}

// parseCurrentUpgradeCostWeightByUpgradeLevel folds [cost weight by upgrade
// level] into a level-indexed coefficient. The live section is a flat run of 19
// `level weight weight` triples covering levels 0..18, and the two weights are
// equal in every entry, so the first is kept -- nothing in the file says which
// column is which mode. A run that does not start at level 0 and step by one, or
// that carries a non-positive weight, is rejected as a whole: silently keeping a
// shifted or zeroed row would move every level's price.
func parseCurrentUpgradeCostWeightByUpgradeLevel(values []float64) []float64 {
	if len(values) == 0 || len(values)%3 != 0 {
		return nil
	}
	weights := make([]float64, 0, len(values)/3)
	for i := 0; i+2 < len(values); i += 3 {
		if int(math.Round(values[i])) != len(weights) {
			return nil
		}
		weight := values[i+1]
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return nil
		}
		weights = append(weights, weight)
	}
	return weights
}

func nextCurrentUpgradeTableRowValues(tokens []float64, offset int) ([]float64, int, bool) {
	for _, width := range []int{currentUpgradeTableShortRowWidth, currentUpgradeTableFullRowWidth} {
		end := offset + width
		if end > len(tokens) {
			continue
		}
		tailStart := end - currentUpgradeTableTailWidth
		if !isCurrentUpgradeTableTail(tokens[tailStart:end]) {
			continue
		}
		return tokens[offset:end], end, true
	}
	return nil, offset, false
}

func isCurrentUpgradeTableTail(values []float64) bool {
	if len(values) != currentUpgradeTableTailWidth {
		return false
	}
	itemID := int(math.Round(values[6]))
	count := int(math.Round(values[7]))
	if math.Abs(values[6]-float64(itemID)) > 0.0001 || math.Abs(values[7]-float64(count)) > 0.0001 {
		return false
	}
	return itemID >= 1000 && count > 0
}

func parseCurrentUpgradeDisjoint(rawText string, table *currentUpgradeTable) {
	lower := strings.ToLower(rawText)
	idx := strings.Index(lower, "[disjoint bonus item]")
	if idx >= 0 {
		after := rawText[idx+len("[disjoint bonus item]"):]
		tokens := currentUpgradeTokenRE.FindAllString(after, 1)
		if len(tokens) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(tokens[0])); err == nil {
				table.DisjointBonusItemID = v
			}
		}
	}
	idx = strings.Index(lower, "[equip level const]")
	if idx >= 0 {
		after := rawText[idx+len("[equip level const]"):]
		tokens := currentUpgradeTokenRE.FindAllString(after, 1)
		if len(tokens) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(tokens[0])); err == nil {
				table.DisjointEquipLevelConst = v
			}
		}
	}
	idx = strings.Index(lower, "[upgrade const for bonus item count]")
	if idx >= 0 {
		after := rawText[idx+len("[upgrade const for bonus item count]"):]
		tokens := currentUpgradeTokenRE.FindAllString(after, 1)
		if len(tokens) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(tokens[0])); err == nil {
				table.DisjointLevelConst = v
			}
		}
	}
	parseCurrentUpgradeStringFloatPairs(rawText, "upgrade failed bonus weight by rarity", table.DisjointWeightByRarity)
	if table.DisjointCorrectionGradeByRarity == nil {
		table.DisjointCorrectionGradeByRarity = make(map[string]float64)
	}
	parseCurrentUpgradeStringFloatPairs(rawText, "correction grade by rarity", table.DisjointCorrectionGradeByRarity)
}

// extractCurrentUpgradeTableTokens reads the numeric values inside [table]...[/table],
// skipping the first token if it's a string (table type name like `normal`).
func extractCurrentUpgradeTableTokens(rawText, section string) []float64 {
	lower := strings.ToLower(rawText)
	startTag := "[" + section + "]"
	endTag := "[/" + section + "]"
	startIdx := strings.Index(lower, startTag)
	if startIdx < 0 {
		return nil
	}
	startIdx += len(startTag)
	endIdx := strings.Index(lower[startIdx:], endTag)
	var content string
	if endIdx >= 0 {
		content = rawText[startIdx : startIdx+endIdx]
	} else {
		content = rawText[startIdx:]
	}
	// Remove nested section tags like [table type]
	content = regexp.MustCompile(`(?i)\[/?table[^\]]*\]`).ReplaceAllString(content, " ")
	tokens := currentUpgradeTokenRE.FindAllString(content, -1)
	var values []float64
	for _, token := range tokens {
		if strings.HasPrefix(token, "`") {
			continue // skip string tokens (table type name)
		}
		if v, err := strconv.ParseFloat(token, 64); err == nil {
			values = append(values, v)
		}
	}
	return values
}

func parseCurrentUpgradeStringIntPairs(rawText, section string, out map[string]int) {
	lower := strings.ToLower(rawText)
	startTag := "[" + section + "]"
	endTag := "[/" + section + "]"
	startIdx := strings.Index(lower, startTag)
	if startIdx < 0 {
		return
	}
	startIdx += len(startTag)
	endIdx := strings.Index(lower[startIdx:], endTag)
	var content string
	if endIdx >= 0 {
		content = rawText[startIdx : startIdx+endIdx]
	} else {
		content = rawText[startIdx:]
	}
	tokens := currentUpgradeTokenRE.FindAllString(content, -1)
	for i := 0; i+1 < len(tokens); i += 2 {
		name := strings.Trim(tokens[i], "` ")
		if v, err := strconv.ParseFloat(tokens[i+1], 64); err == nil {
			out[strings.ToLower(name)] = int(math.Round(v))
		}
	}
}

func parseCurrentUpgradeStringFloatPairs(rawText, section string, out map[string]float64) {
	lower := strings.ToLower(rawText)
	startTag := "[" + section + "]"
	endTag := "[/" + section + "]"
	startIdx := strings.Index(lower, startTag)
	if startIdx < 0 {
		return
	}
	startIdx += len(startTag)
	endIdx := strings.Index(lower[startIdx:], endTag)
	var content string
	if endIdx >= 0 {
		content = rawText[startIdx : startIdx+endIdx]
	} else {
		content = rawText[startIdx:]
	}
	tokens := currentUpgradeTokenRE.FindAllString(content, -1)
	for i := 0; i+1 < len(tokens); i += 2 {
		name := strings.Trim(tokens[i], "` ")
		if v, err := strconv.ParseFloat(tokens[i+1], 64); err == nil {
			out[strings.ToLower(name)] = v
		}
	}
}

func currentUpgradeRarityName(rarity int) string {
	switch rarity {
	case 0:
		return "common"
	case 1:
		return "uncommon"
	case 2:
		return "rare"
	case 3:
		return "unique"
	case 4:
		return "epic"
	case 5:
		return "chronicle"
	case 6:
		return "legendary"
	default:
		return ""
	}
}

// alignedUpgradePolicyResolver returns a closure that resolves the PVF upgrade
// table row for a given mode and current level, providing success weight and
// penalty type to the inventory owner's RNG path.
func (s *Service) alignedUpgradePolicyResolver() func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
	return func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
		table, err := s.currentUpgradeTableForMode(mode)
		if err != nil {
			s.logPacketEvent("dnf-upgrade-policy-resolve-failed",
				"mode", mode,
				"current_level", currentLevel,
				"error", err)
			return alignedcmd.UpgradePolicyResolution{}, err
		}
		goldCost, err := s.currentUpgradeGoldCost(table, targetItemID, currentLevel+1)
		if err != nil {
			s.logPacketEvent("dnf-upgrade-policy-resolve-failed",
				"mode", mode,
				"current_level", currentLevel,
				"target_item_id", targetItemID,
				"error", err)
			return alignedcmd.UpgradePolicyResolution{}, err
		}
		targetLevel := currentLevel + 1
		row, ok := table.RowForLevel(targetLevel)
		if !ok {
			// Beyond table range: guaranteed success (no penalty data).
			s.logPacketEvent("dnf-upgrade-policy-resolved",
				"mode", mode,
				"current_level", currentLevel,
				"target_level", targetLevel,
				"table_rows", len(table.Rows),
				"material_item_id", 0,
				"material_count", 0,
				"reason", "beyond_table_range")
			return alignedcmd.UpgradePolicyResolution{SuccessWeight: currentUpgradeSuccessBase, GoldCost: goldCost}, nil
		}
		penaltyType, penaltyValue := currentUpgradePenaltyForAttempt(mode, currentLevel, row, table)
		resolution := alignedcmd.UpgradePolicyResolution{
			SuccessWeight:  row.SuccessWeight(),
			PenaltyType:    penaltyType,
			PenaltyValue:   penaltyValue,
			GoldCost:       goldCost,
			MaterialItemID: row.MaterialItemID,
			MaterialCount:  row.MaterialCount,
			NoticeLevel:    table.NoticeLevel,
		}
		rarity, grade := -1, -1
		if penaltyType == 3 {
			if definition, ok := s.currentUpgradeTargetDefinition(targetItemID); ok {
				rarity = int(definition.Rarity)
				grade = int(definition.Grade)
				if grade <= 0 {
					grade = int(definition.MinimumLevel)
				}
				if table.DisjointBonusItemID > 0 {
					if count := currentUpgradeDestroyBonusCount(table, currentLevel, rarity, grade); count > 0 {
						resolution.DestroyBonusItemID = table.DisjointBonusItemID
						resolution.DestroyBonusCount = count
					}
				}
				resolution.DestroyExtraRewards = currentUpgradeDestroyExtraRewards(definition, s.currentDisjointerRulesForDisjoint(nil))
				// 增幅 destruction refunds both traces. 86JP's
				// CalculateDestroyBonuses (ItemUpgradeTableProvider.cs:88-97)
				// runs the amplify table and then the reinforce table and
				// returns whichever of the two produced a count, so a
				// destroyed amplified item yields amplifyupgrade.etc's bonus
				// item (3244 in the live PVF) *and* upgrade.etc's (3243).
				// This tree only ever computed the requested mode's table.
				if mode == "amplify" {
					if extra, ok := s.currentUpgradeAmplifyCompanionRefund(currentLevel, rarity, grade); ok {
						resolution.DestroyExtraRewards = append(resolution.DestroyExtraRewards, extra)
					}
				}
				// House rule, deliberately beyond the PVF and 86JP: a target
				// destroyed by reinforcement or amplification also refunds
				// what it would have yielded in the 分解机. upgrade.etc has no
				// such table -- its only destruction payout is the [disjoint
				// bonus item] trace -- so 无色小晶块 and 灵魂 can only come
				// from the disassembly tables (etc/disjoint.etc plus
				// disjointer.exj), which is what this reuses.
				resolution.DestroyExtraRewards = append(resolution.DestroyExtraRewards,
					s.currentUpgradeDestroyDisjointRewards(definition, targetItemID, amplifyType)...)
			}
		}
		s.logPacketEvent("dnf-upgrade-policy-resolved",
			"mode", mode,
			"current_level", currentLevel,
			"target_level", targetLevel,
			"table_rows", len(table.Rows),
			"success_weight", resolution.SuccessWeight,
			"penalty_type", resolution.PenaltyType,
			"penalty_value", resolution.PenaltyValue,
			"gold_cost", resolution.GoldCost,
			"material_item_id", resolution.MaterialItemID,
			"material_count", resolution.MaterialCount,
			// Destruction refunds are entirely driven by upgrade.etc
			// [disjoint]; without the rarity and grade that fed it there is no
			// way to tell "the table says zero" from "we dropped the reward".
			"target_rarity", rarity,
			"target_grade", grade,
			"destroy_bonus_item_id", resolution.DestroyBonusItemID,
			"destroy_bonus_count", resolution.DestroyBonusCount,
			"destroy_extra_rewards", len(resolution.DestroyExtraRewards))
		return resolution, nil
	}
}

// currentUpgradeGoldCost applies the active upgrade/amplify table's native
// price for one attempt: grade base cost * rarity weight * equipment-type
// weight * the coefficient for the *current* level. targetLevel is the level
// being bought, i.e. currentLevel+1, so the coefficient is looked up at
// targetLevel-1. ItemStack.Extra is deliberately not consulted because ordinary
// persisted equipment rows do not carry these global PVF values.
func (s *Service) currentUpgradeGoldCost(table *currentUpgradeTable, targetItemID int64, targetLevel int) (int64, error) {
	if table == nil {
		return 0, fmt.Errorf("upgrade gold cost table is missing")
	}
	if targetItemID <= 0 || targetItemID > math.MaxUint32 {
		return 0, fmt.Errorf("upgrade gold cost target item is invalid: %d", targetItemID)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return 0, fmt.Errorf("upgrade gold cost item catalog: %w", err)
	}
	definition, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		return 0, fmt.Errorf("upgrade gold cost target item=%d: %w", targetItemID, err)
	}
	if definition.Kind != dungeonDropItemEquipment {
		return 0, fmt.Errorf("upgrade gold cost target item=%d kind=%s is not equipment", targetItemID, definition.Kind)
	}
	return calculateCurrentUpgradeGoldCost(table, definition, targetLevel)
}

// calculateCurrentUpgradeGoldCost reproduces the 强化器 / 增幅器 popup:
//
//	[cost][grade] * [cost weights by rarity][rarity] * [type][equipment type]
//	  * [cost weight by upgrade level][currentLevel]
//
// The last factor is the client's own, and it is the one that keeps going wrong:
//
//   - Before 2026-09-11 it was dropped entirely, so every level of a piece cost
//     the same.
//   - From 2026-09-11 it was replaced by [table type] values[1] * 25, an
//     empirical constant fitted to three 强化 readings (2124/2124/199422/3390174,
//     see TestCurrentUpgradeGoldCostMatchesTheClientPopup). Those readings all
//     sit on levels where the fitted curve happens to equal the client's, so the
//     fit looked right while being wrong in between: it charged 2x at +2->+3 and
//     3x at +10->+11.
//   - On 2026-09-25 the 增幅器 quoted 220500 for a +10->+11 attempt while the
//     server took 992250 -- exactly 4.5x, the fitted factor. 增幅 has no
//     [cost weight by upgrade level] section at all, so its coefficient is 1 and
//     the popup is the bare base price.
func calculateCurrentUpgradeGoldCost(table *currentUpgradeTable, definition dungeonDropItemDefinition, targetLevel int) (int64, error) {
	if table == nil {
		return 0, fmt.Errorf("upgrade gold cost table is missing")
	}
	grade := int(definition.Grade)
	if grade < 0 || grade >= len(table.Costs) {
		return 0, fmt.Errorf("upgrade gold cost target item=%d grade=%d outside cost rows=%d", definition.ItemID, grade, len(table.Costs))
	}
	// A zero base cost is never a real price, it is a grade the table does not
	// cover -- including grade 0, which is what a definition that only carries
	// [minimum level] looks like. Fail closed instead of handing out a free
	// upgrade.
	if table.Costs[grade] <= 0 {
		return 0, fmt.Errorf("upgrade gold cost target item=%d grade=%d has no positive base cost", definition.ItemID, grade)
	}
	rarity := int(definition.Rarity)
	if rarity < 0 || rarity >= len(table.CostWeightsByRarity) || table.CostWeightsByRarity[rarity] <= 0 {
		return 0, fmt.Errorf("upgrade gold cost target item=%d rarity=%d has no positive weight", definition.ItemID, rarity)
	}
	typeIndex, ok := currentUpgradeEquipmentTypeIndex(definition.EquipmentType)
	if !ok || typeIndex >= len(table.TypeWeights) || table.TypeWeights[typeIndex] <= 0 {
		return 0, fmt.Errorf("upgrade gold cost target item=%d equipment_type=%q has no positive weight", definition.ItemID, definition.EquipmentType)
	}
	if targetLevel < 1 || targetLevel > len(table.Rows) {
		return 0, fmt.Errorf("upgrade gold cost target item=%d target_level=%d outside table rows=%d", definition.ItemID, targetLevel, len(table.Rows))
	}
	raw := float64(table.Costs[grade]) * table.CostWeightsByRarity[rarity] * table.TypeWeights[typeIndex] *
		table.costWeightForCurrentLevel(targetLevel-1)
	if raw < 0 || math.IsNaN(raw) || math.IsInf(raw, 0) || raw > math.MaxInt64 {
		return 0, fmt.Errorf("upgrade gold cost target item=%d produced invalid value=%v", definition.ItemID, raw)
	}
	return int64(math.Round(raw)), nil
}

func currentUpgradeEquipmentTypeIndex(raw string) (int, bool) {
	switch normalizeEquipmentPlacementPVFType(raw) {
	case "[weapon]":
		return 0, true
	case "[title name]":
		return 1, true
	case "[coat]":
		return 2, true
	case "[pants]":
		return 3, true
	case "[shoulder]":
		return 4, true
	case "[waist]":
		return 5, true
	case "[shoes]":
		return 6, true
	case "[amulet]":
		return 7, true
	case "[wrist]":
		return 8, true
	case "[ring]":
		return 9, true
	case "[support]":
		return 10, true
	case "[magic stone]":
		return 11, true
	case "[earring]":
		return 12, true
	default:
		return 0, false
	}
}

// currentUpgradePenaltyForAttempt keeps the PVF-authored success probability
// while applying the service's shared reinforcement/amplification protection
// boundary. A failed attempt from +7 through +9 drops exactly one level; a
// failed +10 -> +11 or higher attempt is destructive. In particular, the
// active amplifyupgrade.etc type-2 values 7/8/9 are absolute fallback levels
// in the client table, not quantities to subtract from the current level.
// Passing those values to the inventory owner's quantity-based type-2 branch
// reset +7/+8/+9 amplification to zero, while applying the table's common
// destroy threshold made +8/+9 failures destroy the equipment. Both outcomes
// violate the current service boundary and bypass its intrinsic protection.
func currentUpgradePenaltyForAttempt(mode string, currentLevel int, row currentUpgradeTableRow, table *currentUpgradeTable) (int, int) {
	if mode == "reinforce" || mode == "amplify" {
		switch {
		case currentLevel >= 10:
			return 3, 0
		case currentLevel >= 7:
			return 1, 0
		default:
			return row.PenaltyType, row.PenaltyValue
		}
	}

	return row.PenaltyType, row.PenaltyValue
}

// currentUpgradeTargetDefinition resolves the upgrade target's equipment
// definition; an unresolvable target grants no destruction rewards, like the
// references.
func (s *Service) currentUpgradeTargetDefinition(targetItemID int64) (dungeonDropItemDefinition, bool) {
	if targetItemID <= 0 || targetItemID > math.MaxUint32 {
		return dungeonDropItemDefinition{}, false
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return dungeonDropItemDefinition{}, false
	}
	definition, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil || definition.Kind != dungeonDropItemEquipment {
		return dungeonDropItemDefinition{}, false
	}
	return definition, true
}

// currentUpgradeDestroyExtraRewards lists the fixed rows a destroyed target
// refunds besides the 强烈之痕迹 compensation: 异界 (rarity 5) equipment refunds
// disjointer.exj [chronicle set disjoint result] (异次元碎片 ×20 in the live
// PVF) whenever reinforcement or amplification destroys it.
func currentUpgradeDestroyExtraRewards(definition dungeonDropItemDefinition, rules currentDisjointerRules) []alignedcmd.UpgradeDestroyReward {
	if rules == nil || definition.Rarity != currentDisjointChronicleRarity {
		return nil
	}
	itemID, count, ok := rules.ChronicleSetResult()
	if !ok || itemID <= 0 || itemID > math.MaxInt32 || count <= 0 || count > math.MaxInt32 {
		return nil
	}
	return []alignedcmd.UpgradeDestroyReward{{ItemID: int(itemID), Count: int(count)}}
}

// currentUpgradeDestroyDisjointRewards returns what the destroyed target would
// have produced in the 分解机, so a blown-up piece is not a total loss. This is
// a house rule: neither upgrade.etc nor 86JP refunds disassembly products on
// destruction, so the rows come from the ordinary disassembly resolver
// (etc/disjoint.etc for the no-element cube, the rarity additional row, the
// soul expansion and the exact-item rows; disjointer.exj for the 异界 tables
// and the 红字 otherverse bonus).
//
// amplifyType is reconstructed into a synthetic stack because that is the only
// field currentEquipmentDisjointRewardsForSource reads off the source, and it
// decides the otherverse bonus -- which is precisely the case that matters for
// 增幅, since amplification is what puts 红字 on the item.
//
// Any resolution failure yields no extra rows rather than failing the upgrade:
// the trace compensation and the destruction itself must still go through.
func (s *Service) currentUpgradeDestroyDisjointRewards(
	definition dungeonDropItemDefinition,
	targetItemID int64,
	amplifyType int,
) []alignedcmd.UpgradeDestroyReward {
	if targetItemID <= 0 || targetItemID > math.MaxUint32 {
		return nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil || catalog.source == nil {
		return nil
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil || currentDisjointDocumentIsAvatar(document) {
		return nil
	}
	config, err := s.currentDisjointPVFConfigCached(catalog.source)
	if err != nil {
		return nil
	}
	source := dnfrepo.ItemStack{ItemID: targetItemID}
	if amplifyType != 0 {
		source.Extra = map[string]string{"amplify_type": strconv.Itoa(amplifyType)}
	}
	rewards, err := currentEquipmentDisjointRewardsForSource(
		config, s.currentDisjointerRulesForDisjoint(nil), uint32(targetItemID), document, source)
	if err != nil {
		s.logPacketEvent("dnf-upgrade-destroy-disjoint-refund-failed",
			"target_item_id", targetItemID, "amplify_type", amplifyType, "error", err)
		return nil
	}
	converted := make([]alignedcmd.UpgradeDestroyReward, 0, len(rewards))
	for _, reward := range rewards {
		if reward.ItemID == 0 || reward.Count == 0 {
			continue
		}
		converted = append(converted, alignedcmd.UpgradeDestroyReward{
			ItemID: int(reward.ItemID),
			Count:  int(reward.Count),
			// These rows are the house rule, not the PVF contract: a full bag
			// must cost the player a refund row, never the attempt.
			BestEffort: true,
		})
	}
	return converted
}

// currentUpgradeAmplifyCompanionRefund resolves the reinforce table's
// destruction bonus so an amplified item that breaks refunds both traces, the
// way 86JP's CalculateDestroyBonuses does. An unreadable reinforce table is
// not fatal: the amplify refund still stands on its own.
func (s *Service) currentUpgradeAmplifyCompanionRefund(currentLevel, rarity, grade int) (alignedcmd.UpgradeDestroyReward, bool) {
	table, err := s.currentUpgradeTableForMode("reinforce")
	if err != nil || table == nil || table.DisjointBonusItemID <= 0 {
		return alignedcmd.UpgradeDestroyReward{}, false
	}
	count := currentUpgradeDestroyBonusCount(table, currentLevel, rarity, grade)
	if count <= 0 {
		return alignedcmd.UpgradeDestroyReward{}, false
	}
	return alignedcmd.UpgradeDestroyReward{ItemID: table.DisjointBonusItemID, Count: count}, true
}

// currentUpgradeDestroyBonusCount calculates the 强烈之痕迹 compensation on
// destruction from upgrade.etc [disjoint] (86JP ItemUpgradeTableProvider.
// CalculateDestroyBonus, usdof upgrade_config.destroy.reward_count): the
// equipment grade minus [correction grade by rarity] must reach [equip level
// const], the failed level must exceed [upgrade const for bonus item count],
// and the count is (level - const)^2 * [upgrade failed bonus weight by
// rarity]. Zero means no compensation.
func currentUpgradeDestroyBonusCount(table *currentUpgradeTable, currentLevel, rarity, equipmentGrade int) int {
	if table == nil || currentLevel <= 0 {
		return 0
	}
	rarityName := currentUpgradeRarityName(rarity)
	correction := 0.0
	if value, ok := table.DisjointCorrectionGradeByRarity[rarityName]; ok {
		correction = value
	}
	correctedGrade := float64(equipmentGrade) - correction
	if correctedGrade < 0 {
		correctedGrade = 0
	}
	if table.DisjointEquipLevelConst > 0 && correctedGrade < float64(table.DisjointEquipLevelConst) {
		return 0
	}
	if table.DisjointLevelConst < 0 || currentLevel <= table.DisjointLevelConst {
		return 0
	}
	weight, ok := table.DisjointWeightByRarity[rarityName]
	if !ok || weight <= 0 {
		return 0
	}
	diff := currentLevel - table.DisjointLevelConst
	count := int(float64(diff*diff) * weight)
	if count <= 0 {
		return 0
	}
	return count
}
