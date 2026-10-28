package dnfbridge

import (
	cryptorand "crypto/rand"
	"math"
	"math/big"
	"strconv"
	"strings"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentDisjointPVFConfigCached memoizes the parsed disjoint config on the
// service. The config is immutable for a given PVF archive, but the raw
// loader re-tokenizes etc/disjoint.etc, etc/emblemcompound.etc and every
// emblemlistinfo_<grade>.etc document on every call; before memoization that
// ran inside each equipment/avatar disjoint, emblem compound and expert-store
// disjoint request. A load failure is memoized too, and returned without
// touching the nil config pointer.
func (s *Service) currentDisjointPVFConfigCached(source dnfpvf.Source) (currentDisjointPVFConfig, error) {
	if s == nil || source == nil {
		return currentDisjointPVFConfig{}, errCurrentDisjointUnavailable
	}
	s.disjointConfigMu.Lock()
	defer s.disjointConfigMu.Unlock()
	if s.disjointPVFConfig != nil {
		return *s.disjointPVFConfig, nil
	}
	if s.disjointPVFConfigErr != nil {
		return currentDisjointPVFConfig{}, s.disjointPVFConfigErr
	}
	config, err := loadCurrentDisjointPVFConfig(source)
	if err != nil {
		s.disjointPVFConfigErr = err
		return currentDisjointPVFConfig{}, err
	}
	s.disjointPVFConfig = &config
	return config, nil
}

func loadCurrentDisjointPVFConfig(source dnfpvf.Source) (currentDisjointPVFConfig, error) {
	if source == nil {
		return currentDisjointPVFConfig{}, errCurrentDisjointUnavailable
	}
	disjoint, err := parseDungeonCardPVFDocument(source, "etc/disjoint.etc")
	if err != nil {
		return currentDisjointPVFConfig{}, err
	}
	config := currentDisjointPVFConfig{
		CubeBase:          150,
		IndependentByItem: make(map[uint32][]currentDisjointReward),
		AvatarByJob:       make(map[string][]uint32),
		EmblemBoosters:    make(map[[2]int]uint32),
	}
	priceTable, err := parseDungeonCardPVFDocument(source, "equipment/pricetable.tbl")
	if err != nil {
		return currentDisjointPVFConfig{}, err
	}
	rates := priceTable.Ints("rate")
	if len(rates) == 0 || rates[0] <= 0 || rates[0] > math.MaxInt32 {
		return currentDisjointPVFConfig{}, errCurrentDisjointRewardInvalid
	}
	config.EquipmentSellRate = rates[0]
	for _, section := range disjoint.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "cube index") {
			continue
		}
		tokens := currentDisjointSectionTokens(disjoint, section)
		if len(tokens) == 0 || !strings.EqualFold(strings.TrimSpace(strings.Trim(tokens[0].Value, "`")), "[no element]") {
			continue
		}
		for _, token := range tokens[1:] {
			if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxUint32 {
				config.CubeItemID = uint32(token.Int)
				break
			}
		}
	}
	values := disjoint.Numbers("cube creation const")
	if len(values) > 0 && values[0] > 0 {
		config.CubeBase = values[0]
		config.CubeMultipliers = append(config.CubeMultipliers, values[1:]...)
	}
	config.Additional = currentDisjointParseAdditional(disjoint.Ints("additional result"))
	config.AdditionalConsts = currentDisjointParseAdditionalConsts(disjoint.Numbers("additional result const"), config.Additional)
	config.Expands = currentDisjointParseExpands(disjoint.Ints("additional result expand"), disjoint.Numbers("additional result expand const"), len(config.Additional))
	config.IndependentByItem = currentDisjointParseIndependentResults(disjoint)
	config.AvatarByJob = currentDisjointParseAvatarByJob(disjoint)
	config.AvatarEmblemTables = currentDisjointLoadAvatarEmblemTables(source)

	emblem, err := parseDungeonCardPVFDocument(source, "etc/emblemcompound.etc")
	if err != nil {
		return currentDisjointPVFConfig{}, err
	}
	config.EmblemBoosters = currentDisjointParseEmblemBoosters(emblem.Ints("emblem compound info"))
	if config.CubeItemID == 0 || config.EquipmentSellRate <= 0 || len(config.CubeMultipliers) == 0 ||
		len(config.Additional) != len(config.CubeMultipliers) || len(config.AdditionalConsts) != len(config.Additional) ||
		len(config.Expands) != len(config.Additional) || len(config.AvatarByJob) == 0 || len(config.EmblemBoosters) == 0 {
		return currentDisjointPVFConfig{}, errCurrentDisjointRewardInvalid
	}
	return config, nil
}

// currentDisjointParseIndependentResults reads the active PVF's exact-item
// additive result table. The current file contains only deterministic 1000
// per-mille rows; fail closed on any other shape instead of inventing a roll
// policy for a different client/PVF compatibility unit.
func currentDisjointParseIndependentResults(document *dnfpvf.Document) map[uint32][]currentDisjointReward {
	out := make(map[uint32][]currentDisjointReward)
	if document == nil {
		return out
	}
	active := false
	inParticular := false
	var inputs []uint32
	var outputs []currentDisjointReward
	valid := true
	commit := func() {
		if !inParticular || !valid || len(inputs) == 0 || len(outputs) == 0 {
			return
		}
		for _, itemID := range inputs {
			out[itemID] = append([]currentDisjointReward(nil), outputs...)
		}
	}
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "independent result":
			active = true
			continue
		case "/independent result":
			commit()
			return out
		}
		if !active {
			continue
		}
		switch name {
		case "particular item":
			commit()
			inParticular = true
			inputs = nil
			outputs = nil
			valid = true
		case "/particular item":
			commit()
			inParticular = false
			inputs = nil
			outputs = nil
		case "input index":
			if !inParticular {
				valid = false
				continue
			}
			for _, value := range currentDisjointTokensInts(currentDisjointSectionTokens(document, section)) {
				if value <= 0 || value > math.MaxUint32 {
					valid = false
					continue
				}
				inputs = append(inputs, uint32(value))
			}
		case "output index":
			if !inParticular {
				valid = false
				continue
			}
			values := currentDisjointTokensInts(currentDisjointSectionTokens(document, section))
			if len(values) == 0 || len(values)%3 != 0 {
				valid = false
				continue
			}
			for offset := 0; offset+2 < len(values); offset += 3 {
				itemID, probability, count := values[offset], values[offset+1], values[offset+2]
				if itemID <= 0 || itemID > math.MaxUint32 || probability != 1000 || count <= 0 || count > math.MaxUint32 {
					valid = false
					continue
				}
				outputs = append(outputs, currentDisjointReward{ItemID: uint32(itemID), Count: uint32(count)})
			}
		}
	}
	commit()
	return out
}

func currentDisjointSectionTokens(document *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if document == nil || section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
		return nil
	}
	return document.Tokens[section.Start:section.End]
}

func currentDisjointParseAdditional(values []int64) [][]uint32 {
	out := make([][]uint32, 0)
	for offset := 0; offset < len(values); {
		count := int(values[offset])
		offset++
		if count < 0 || offset+count > len(values) {
			return nil
		}
		row := make([]uint32, 0, count)
		for _, value := range values[offset : offset+count] {
			if value > 0 && value <= math.MaxUint32 {
				row = append(row, uint32(value))
			}
		}
		offset += count
		out = append(out, row)
	}
	return out
}

func currentDisjointParseAdditionalConsts(values []float64, additional [][]uint32) []currentDisjointAdditionalConst {
	out := make([]currentDisjointAdditionalConst, 0, len(additional))
	offset := 0
	for _, candidates := range additional {
		if len(candidates) == 0 {
			if offset >= len(values) {
				return nil
			}
			// Disabled rarity rows retain one placeholder in the PVF.
			out = append(out, currentDisjointAdditionalConst{})
			offset++
			continue
		}
		if offset+3 >= len(values) {
			return nil
		}
		out = append(out, currentDisjointAdditionalConst{
			GreatDivisor: values[offset], NormalDivisor: values[offset+1],
			GreatChance: values[offset+2], NormalChance: values[offset+3],
		})
		offset += 4
	}
	if offset != len(values) {
		return nil
	}
	return out
}

func currentDisjointParseExpands(ids []int64, values []float64, rows int) []currentDisjointExpand {
	if rows <= 0 {
		return nil
	}
	out := make([]currentDisjointExpand, 0, rows)
	idOffset := 0
	for row := 0; row < rows; row++ {
		if idOffset >= len(ids) {
			return nil
		}
		count := int(ids[idOffset])
		idOffset++
		if count < 0 || idOffset+count > len(ids) {
			return nil
		}
		expand := currentDisjointExpand{}
		if count > 0 {
			itemID := ids[idOffset]
			if count != 1 || itemID <= 0 || itemID > math.MaxUint32 {
				return nil
			}
			expand.Enabled, expand.ItemID = true, uint32(itemID)
		}
		idOffset += count
		out = append(out, expand)
	}
	if idOffset != len(ids) {
		return nil
	}
	valueOffset := 0
	for index := range out {
		if !out[index].Enabled {
			if valueOffset >= len(values) {
				return nil
			}
			// Disabled rarity rows retain one divisor placeholder.
			valueOffset++
			continue
		}
		if valueOffset+2 >= len(values) {
			return nil
		}
		out[index].LevelDivisor, out[index].GreatChance, out[index].NormalChance = values[valueOffset], values[valueOffset+1], values[valueOffset+2]
		valueOffset += 3
	}
	if valueOffset != len(values) {
		return nil
	}
	return out
}

// currentDisjointLoadAvatarEmblemTables reads the per-grade avatar disjoint
// reward tables etc/avatardisjoint/emblemlistinfo_<grade>.etc until the first
// missing or unusable file. Missing tables are not an error here; only the
// avatar disjoint reward path requires them.
func currentDisjointLoadAvatarEmblemTables(source dnfpvf.Source) []currentAvatarEmblemGradeTable {
	tables := make([]currentAvatarEmblemGradeTable, 0, 6)
	for grade := 0; grade < 32; grade++ {
		document, err := parseDungeonCardPVFDocument(source, "etc/avatardisjoint/emblemlistinfo_"+strconv.Itoa(grade)+".etc")
		if err != nil {
			break
		}
		table := currentDisjointParseAvatarEmblemTable(document)
		if len(table.Pools) == 0 {
			break
		}
		tables = append(tables, table)
	}
	return tables
}

func currentDisjointParseAvatarEmblemTable(document *dnfpvf.Document) currentAvatarEmblemGradeTable {
	table := currentAvatarEmblemGradeTable{}
	if document == nil {
		return table
	}
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "result info") {
			continue
		}
		values := currentDisjointTokensInts(currentDisjointSectionTokens(document, section))
		if len(values) < 5 || values[0] <= 0 {
			continue
		}
		pool := currentAvatarEmblemPool{PickCount: int(values[0])}
		for offset := 1; offset+3 < len(values); offset += 4 {
			item, weight, count := values[offset], values[offset+1], values[offset+2]
			if item <= 0 || item > math.MaxUint32 || weight <= 0 || weight > math.MaxUint32 || count <= 0 || count > math.MaxUint32 {
				continue
			}
			pool.Rewards = append(pool.Rewards, currentAvatarEmblemReward{
				ItemID:  uint32(item),
				Weight:  uint32(weight),
				Count:   uint32(count),
				Special: values[offset+3] != 0,
			})
		}
		if len(pool.Rewards) > 0 {
			table.Pools = append(table.Pools, pool)
		}
	}
	return table
}

func currentDisjointParseAvatarByJob(document *dnfpvf.Document) map[string][]uint32 {
	out := make(map[string][]uint32)
	active := false
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "avatar disjoint info":
			active = true
			continue
		case "/avatar disjoint info":
			active = false
			continue
		}
		if !active || strings.HasPrefix(name, "/") {
			continue
		}
		values := make([]uint32, 0, 15)
		for _, token := range currentDisjointSectionTokens(document, section) {
			if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxUint32 {
				values = append(values, uint32(token.Int))
			}
		}
		if len(values) >= 15 {
			out[currentDisjointNormalizeJob(name)] = append([]uint32(nil), values[:15]...)
		}
	}
	return out
}

func currentDisjointParseEmblemBoosters(values []int64) map[[2]int]uint32 {
	out := make(map[[2]int]uint32)
	for offset := 0; offset+1 < len(values); {
		grade, maxCount := int(values[offset]), int(values[offset+1])
		offset += 2
		if grade <= 0 || maxCount < 2 || offset+maxCount-1 > len(values) {
			return map[[2]int]uint32{}
		}
		for inputCount := 2; inputCount <= maxCount; inputCount++ {
			value := values[offset]
			offset++
			if value > 0 && value <= math.MaxUint32 {
				out[[2]int{grade, inputCount}] = uint32(value)
			}
		}
	}
	return out
}

// currentDisjointerRules is the slice of character/expertjob/disjointer.exj
// that ordinary (NPC) disassembly borrows: the machine-grade-0 [disjoint
// result] row and the [additional result] / [big win result] tables.
// *dnfexpertjob.DisjointerConfig implements it.
type currentDisjointerRules interface {
	ResultRule(machineGradeColumn, rarity, state int) (dnfexpertjob.DisjointerResultRule, bool)
	SelectAdditional(table, level int, rng dnfexpertjob.RandomFunc) (dnfexpertjob.DisjointerSelectionRule, bool)
	SelectBigWin(table, level int, rng dnfexpertjob.RandomFunc) (dnfexpertjob.DisjointerSelectionRule, bool)
	// ChronicleSetResult is [chronicle set disjoint result]; disassembly does
	// not grant it, the upgrade destruction path does.
	ChronicleSetResult() (itemID, count int64, ok bool)
}

// currentDisjointChronicleRarity is the PVF rarity index of 异界 (chronicle)
// equipment such as 裂魂之异次元 sets.
const currentDisjointChronicleRarity = 5

func currentEquipmentDisjointRewards(config currentDisjointPVFConfig, itemID uint32, document *dnfpvf.Document) ([]currentDisjointReward, error) {
	return currentEquipmentDisjointRewardsForSource(config, nil, itemID, document, dnfrepo.ItemStack{})
}

// currentEquipmentDisjointRewardsForSource resolves ordinary disassembly:
// etc/disjoint.etc supplies the no-element cube, the rarity additional row,
// the soul expansion and the exact-item rows. Two families are not covered
// by that file in the live PVF and come from disjointer.exj instead:
//
//   - 异界 (rarity 5) equipment: the etc row still names the legacy awakening
//     material 混沌魔石碎片 at 21%, while the exj machine-grade-0 row for rarity
//     5 rolls table 5 — 虚空魔石碎片 (level 67+) / 混沌魔石 (below) — and its
//     9% big win rolls 虚空魔石. The 虚空魔石 item texts say exactly that
//     ("可以通过分解勇者装备获得"), so 异界 equipment follows the exj tables.
//   - equipment carrying an otherverse aura (红字, amplify type != 0) rolls
//     the exj otherverse table on top: 浓缩的异界精髓 30% / 矛盾的结晶体 70%,
//     one item on level% of the rolls (86JP
//     MySqlInventoryStore.Disjoint → PlayerDisjointResultCalculator.AddOtherverseBonus).
//
// A nil rules value keeps the pure etc behaviour.
func currentEquipmentDisjointRewardsForSource(
	config currentDisjointPVFConfig,
	rules currentDisjointerRules,
	itemID uint32,
	document *dnfpvf.Document,
	source dnfrepo.ItemStack,
) ([]currentDisjointReward, error) {
	cubeReward, rarity, minimumLevel, err := currentEquipmentDisjointCubeReward(config, document)
	if err != nil {
		return nil, err
	}
	rewards := []currentDisjointReward{cubeReward}
	chronicleHandled := false
	if rules != nil && rarity == currentDisjointChronicleRarity {
		granted, chronicleErr := currentDisjointAppendChronicleRewards(&rewards, rules, int(minimumLevel))
		if chronicleErr != nil {
			return nil, chronicleErr
		}
		chronicleHandled = granted
	}
	if !chronicleHandled && int(rarity) < len(config.Additional) && len(config.Additional[rarity]) > 0 {
		count, granted, err := currentDisjointAdditionalCount(config, int(rarity), int(minimumLevel))
		if err != nil {
			return nil, err
		}
		if granted {
			index, indexErr := currentDisjointRandomIndex(len(config.Additional[rarity]))
			if indexErr != nil {
				return nil, indexErr
			}
			currentDisjointMergeReward(&rewards, currentDisjointReward{ItemID: config.Additional[rarity][index], Count: count})
		}
	}
	if reward, granted, err := currentDisjointExpandReward(config, int(rarity), int(minimumLevel)); err != nil {
		return nil, err
	} else if granted {
		currentDisjointMergeReward(&rewards, reward)
	}
	if rules != nil && currentDisjointSourceAmplifyType(source) != 0 {
		if err := currentDisjointAppendOtherverseReward(&rewards, rules, int(minimumLevel)); err != nil {
			return nil, err
		}
	}
	for _, reward := range config.IndependentByItem[itemID] {
		currentDisjointMergeReward(&rewards, reward)
	}
	return rewards, nil
}

// currentDisjointAppendChronicleRewards rolls the disjointer.exj machine-grade-0
// row for 异界 equipment: the big-win chance selects between the additional
// and big-win tables, one weighted row whose level range covers the
// equipment is picked, and the count is level / divisor with a fractional
// roll. It reports false when the exj carries no such row so the caller can
// fall back to the etc additional row.
func currentDisjointAppendChronicleRewards(rewards *[]currentDisjointReward, rules currentDisjointerRules, level int) (bool, error) {
	rule, found := rules.ResultRule(0, currentDisjointChronicleRarity, 0)
	if !found {
		return false, nil
	}
	bigWinRoll, err := currentDisjointRandomPercent()
	if err != nil {
		return false, err
	}
	bigWin := bigWinRoll < currentDisjointClampPercent(float64(rule.BigWinChancePercent))
	var selected dnfexpertjob.DisjointerSelectionRule
	var ok bool
	if bigWin {
		selected, ok = rules.SelectBigWin(rule.BigWinTable, level, currentDisjointRandomIntn)
	} else {
		selected, ok = rules.SelectAdditional(rule.AdditionalTable, level, currentDisjointRandomIntn)
	}
	if !ok || selected.ItemID <= 0 || selected.ItemID > math.MaxUint32 {
		return true, nil
	}
	count := dnfexpertjob.LevelDivisorCount(level, selected.CountDivisor, currentDisjointRandomIntn)
	if count <= 0 || count > math.MaxUint32 {
		return true, nil
	}
	currentDisjointMergeReward(rewards, currentDisjointReward{ItemID: uint32(selected.ItemID), Count: uint32(count)})
	return true, nil
}

// currentDisjointAppendOtherverseReward rolls the disjointer.exj otherverse
// table for equipment carrying an otherverse aura.
func currentDisjointAppendOtherverseReward(rewards *[]currentDisjointReward, rules currentDisjointerRules, level int) error {
	selected, ok := rules.SelectAdditional(dnfexpertjob.DisjointerOtherverseTable, level, currentDisjointRandomIntn)
	if !ok || selected.ItemID <= 0 || selected.ItemID > math.MaxUint32 {
		return nil
	}
	count := dnfexpertjob.LevelDivisorCount(level, selected.CountDivisor, currentDisjointRandomIntn)
	if count <= 0 || count > math.MaxUint32 {
		return nil
	}
	currentDisjointMergeReward(rewards, currentDisjointReward{ItemID: uint32(selected.ItemID), Count: uint32(count)})
	return nil
}

// currentDisjointSourceAmplifyType reads the equipment instance's amplify type
// byte (raw entry offset 0x13; nonzero means the item carries an otherverse
// aura, purified or not) from the persisted extras or the raw entry.
func currentDisjointSourceAmplifyType(source dnfrepo.ItemStack) byte {
	if value := sceneInventoryExtraByte(source.Extra, "amplify_type", "amplification_type", "amplify_attr", "amplification_attr", "byte_13", "value_13", "value_c"); value != 0 {
		return value
	}
	if len(source.RawEntry) > 0x13 {
		return source.RawEntry[0x13]
	}
	return 0
}

// currentDisjointRandomIntn adapts the disjoint RNG to the expert-job
// RandomFunc contract.
func currentDisjointRandomIntn(limit int) int {
	if limit <= 1 {
		return 0
	}
	value, err := currentDisjointRandomUint64(uint64(limit))
	if err != nil {
		return 0
	}
	return int(value)
}

// currentEquipmentDisjointCubeReward isolates the PVF-backed no-element cube
// baseline without normal disassembly's rarity-specific additional rewards.
func currentEquipmentDisjointCubeReward(config currentDisjointPVFConfig, document *dnfpvf.Document) (currentDisjointReward, int64, int64, error) {
	if document == nil || config.CubeItemID == 0 || config.CubeBase <= 0 {
		return currentDisjointReward{}, 0, 0, errCurrentDisjointRewardInvalid
	}
	rarity, found := document.Int("rarity")
	if !found || rarity < 0 || rarity >= int64(len(config.CubeMultipliers)) {
		return currentDisjointReward{}, 0, 0, errCurrentDisjointRewardInvalid
	}
	minimumLevel, levelFound := document.Int("minimum level")
	if !levelFound || minimumLevel < 1 {
		minimumLevel = 1
	}
	value, valueFound := document.Int("value")
	if !valueFound || value <= 0 {
		value, valueFound = document.Int("price")
	}
	if !valueFound || value <= 0 {
		if currentDisjointDocumentIsTitle(document) {
			// Current title documents intentionally have no sale value. The
			// compatibility extension grants the minimum baseline cube and
			// still applies the PVF rarity-specific additional rows below.
			return currentDisjointReward{ItemID: config.CubeItemID, Count: 1}, rarity, minimumLevel, nil
		}
		return currentDisjointReward{}, 0, 0, errCurrentDisjointRewardInvalid
	}
	if value > math.MaxInt64/config.EquipmentSellRate {
		return currentDisjointReward{}, 0, 0, errCurrentDisjointRewardInvalid
	}
	sellGold := value * config.EquipmentSellRate / 1000
	if sellGold < 1 {
		sellGold = 1
	}
	cubeCount := uint32(math.Max(1, math.Floor(float64(sellGold)*config.CubeMultipliers[rarity]/config.CubeBase)))
	return currentDisjointReward{ItemID: config.CubeItemID, Count: cubeCount}, rarity, minimumLevel, nil
}

func currentDisjointExpandReward(config currentDisjointPVFConfig, rarity, level int) (currentDisjointReward, bool, error) {
	if rarity < 0 || rarity >= len(config.Expands) {
		return currentDisjointReward{}, false, nil
	}
	expand := config.Expands[rarity]
	if !expand.Enabled || expand.ItemID == 0 {
		return currentDisjointReward{}, false, nil
	}
	roll, err := currentDisjointRandomPercent()
	if err != nil {
		return currentDisjointReward{}, false, err
	}
	if roll >= currentDisjointClampPercent(expand.NormalChance) {
		return currentDisjointReward{}, false, nil
	}
	count, err := currentDisjointLevelDivisorCount(level, expand.LevelDivisor)
	if err != nil {
		return currentDisjointReward{}, false, err
	}
	greatRoll, err := currentDisjointRandomPercent()
	if err != nil {
		return currentDisjointReward{}, false, err
	}
	if greatRoll < currentDisjointClampPercent(expand.GreatChance) {
		if count == math.MaxUint32 {
			return currentDisjointReward{}, false, errCurrentDisjointRewardInvalid
		}
		count++
	}
	return currentDisjointReward{ItemID: expand.ItemID, Count: count}, true, nil
}

func currentDisjointAdditionalCount(config currentDisjointPVFConfig, rarity, level int) (uint32, bool, error) {
	if rarity < 0 || rarity >= len(config.AdditionalConsts) {
		return 0, false, nil
	}
	consts := config.AdditionalConsts[rarity]
	greatChance := currentDisjointClampPercent(consts.GreatChance)
	normalChance := currentDisjointClampPercent(consts.NormalChance)
	roll, err := currentDisjointRandomPercent()
	if err != nil {
		return 0, false, err
	}
	divisor := float64(0)
	switch {
	case roll < greatChance:
		divisor = consts.GreatDivisor
	case roll < math.Min(100, greatChance+normalChance):
		divisor = consts.NormalDivisor
	default:
		return 0, false, nil
	}
	if divisor <= 0 {
		return 0, false, nil
	}
	count, err := currentDisjointLevelDivisorCount(level, divisor)
	if err != nil {
		return 0, false, err
	}
	return count, true, nil
}

// currentAvatarDisjointRewards resolves the avatar's PVF [grade] into the
// matching etc/avatardisjoint grade table and rolls every pool's weighted
// picks (86JP AvatarDisjointConfigProvider.Calculate), merging duplicate
// items. The rewards are real emblem items, not the old fixed booster boxes.
func currentAvatarDisjointRewards(config currentDisjointPVFConfig, document *dnfpvf.Document) ([]currentDisjointReward, error) {
	if document == nil {
		return nil, errCurrentDisjointRewardInvalid
	}
	grade, gradeFound := document.Int("grade")
	equipmentType, typeFound := document.Text("equipment type")
	rule, supported := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !gradeFound || !typeFound || !supported || rule.class != currentEquipmentPlacementClassAvatar {
		return nil, errCurrentDisjointRewardInvalid
	}
	if grade < 0 || grade >= int64(len(config.AvatarEmblemTables)) {
		return nil, errCurrentDisjointRewardInvalid
	}
	rewards := make([]currentDisjointReward, 0, 4)
	for _, pool := range config.AvatarEmblemTables[int(grade)].Pools {
		for pick := 0; pick < pool.PickCount; pick++ {
			reward, err := currentDisjointRollAvatarEmblem(pool.Rewards)
			if err != nil {
				return nil, err
			}
			merged := false
			for index := range rewards {
				if rewards[index].ItemID == reward.ItemID {
					rewards[index].Count += reward.Count
					merged = true
					break
				}
			}
			if !merged {
				rewards = append(rewards, reward)
			}
		}
	}
	if len(rewards) == 0 {
		return nil, errCurrentDisjointRewardInvalid
	}
	return rewards, nil
}

func currentDisjointRollAvatarEmblem(candidates []currentAvatarEmblemReward) (currentDisjointReward, error) {
	var total uint64
	for _, candidate := range candidates {
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		return currentDisjointReward{}, errCurrentDisjointRewardInvalid
	}
	roll, err := currentDisjointRandomUint64(total)
	if err != nil {
		return currentDisjointReward{}, err
	}
	for _, candidate := range candidates {
		if roll < uint64(candidate.Weight) {
			return currentDisjointReward{ItemID: candidate.ItemID, Count: candidate.Count}, nil
		}
		roll -= uint64(candidate.Weight)
	}
	return currentDisjointReward{}, errCurrentDisjointRewardInvalid
}

func currentDisjointDocumentIsAvatar(document *dnfpvf.Document) bool {
	if document == nil {
		return false
	}
	equipmentType, found := document.Text("equipment type")
	if !found {
		return false
	}
	rule, supported := currentEquipmentPlacementRuleForPVFType(equipmentType)
	return supported && rule.class == currentEquipmentPlacementClassAvatar
}

func currentDisjointDocumentIsTitle(document *dnfpvf.Document) bool {
	if document == nil {
		return false
	}
	equipmentType, found := document.Text("equipment type")
	return found && normalizeEquipmentPlacementPVFType(equipmentType) == "[title name]"
}

func currentRollEmblemBoosterReward(catalog *pvfDungeonDropCatalog, boosterID uint32) (currentDisjointReward, error) {
	definition, err := catalog.ResolveItem(boosterID)
	if err != nil || definition.Kind != dungeonDropItemStackable {
		return currentDisjointReward{}, errCurrentDisjointRewardInvalid
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(documentText(document, "stackable type"))), "[booster]") {
		return currentDisjointReward{}, errCurrentDisjointRewardInvalid
	}
	active := false
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "booster info":
			active = true
			continue
		case "/booster info":
			return currentDisjointReward{}, errCurrentDisjointRewardInvalid
		}
		if !active || name != "stackable" {
			continue
		}
		return currentRollEmblemBoosterPool(currentDisjointTokensInts(currentDisjointSectionTokens(document, section)))
	}
	return currentDisjointReward{}, errCurrentDisjointRewardInvalid
}

func currentRollEmblemBoosterPool(values []int64) (currentDisjointReward, error) {
	start := 0
	if len(values) >= 4 && (len(values)-1)%3 == 0 {
		start = 1 // draw-count; op256 supports one popup result only.
	}
	if (len(values)-start)%3 != 0 || len(values) == start {
		return currentDisjointReward{}, errCurrentDisjointRewardInvalid
	}
	type candidate struct{ itemID, weight, count uint32 }
	candidates := make([]candidate, 0, (len(values)-start)/3)
	var total uint64
	for offset := start; offset+2 < len(values); offset += 3 {
		if values[offset] <= 0 || values[offset] > math.MaxUint32 || values[offset+1] <= 0 || values[offset+1] > math.MaxUint32 || values[offset+2] <= 0 || values[offset+2] > math.MaxUint32 {
			continue
		}
		entry := candidate{uint32(values[offset]), uint32(values[offset+1]), uint32(values[offset+2])}
		total += uint64(entry.weight)
		candidates = append(candidates, entry)
	}
	if total == 0 || len(candidates) == 0 {
		return currentDisjointReward{}, errCurrentDisjointRewardInvalid
	}
	roll, err := currentDisjointRandomUint64(total)
	if err != nil {
		return currentDisjointReward{}, err
	}
	for _, candidate := range candidates {
		if roll < uint64(candidate.weight) {
			return currentDisjointReward{ItemID: candidate.itemID, Count: candidate.count}, nil
		}
		roll -= uint64(candidate.weight)
	}
	return currentDisjointReward{}, errCurrentDisjointRewardInvalid
}

func documentText(document *dnfpvf.Document, name string) string {
	if document == nil {
		return ""
	}
	value, _ := document.Text(name)
	return value
}

func currentDisjointTokensInts(tokens []dnfpvf.Token) []int64 {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values
}

func currentDisjointLevelDivisorCount(level int, divisor float64) (uint32, error) {
	if divisor <= 0 {
		return 1, nil
	}
	value := float64(max(1, level)) / divisor
	count := math.Floor(value)
	roll, err := currentDisjointRandomPercent()
	if err != nil {
		return 0, err
	}
	if roll/100 < value-count {
		count++
	}
	if count > math.MaxUint32 {
		return 0, errCurrentDisjointRewardInvalid
	}
	// 86JP DisjointResultCalculator.CalculateLevelDivisorCount clamps the
	// rolled count to at least 1; a low-level source still yields one
	// additional/expand item instead of failing the whole disjoint.
	if count < 1 {
		count = 1
	}
	return uint32(count), nil
}

func currentDisjointMergeReward(rewards *[]currentDisjointReward, reward currentDisjointReward) {
	if rewards == nil || reward.ItemID == 0 || reward.Count == 0 {
		return
	}
	for index := range *rewards {
		if (*rewards)[index].ItemID == reward.ItemID {
			(*rewards)[index].Count += reward.Count
			return
		}
	}
	*rewards = append(*rewards, reward)
}

func currentDisjointNormalizeJob(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Trim(value, "`")))
	return strings.TrimSpace(strings.Trim(value, "[]"))
}

func currentDisjointClampPercent(value float64) float64 {
	return math.Max(0, math.Min(100, value))
}

func currentDisjointRandomIndex(limit int) (int, error) {
	if limit <= 0 {
		return 0, errCurrentDisjointRewardInvalid
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func currentDisjointRandomUint64(limit uint64) (uint64, error) {
	if limit == 0 || limit > math.MaxInt64 {
		return 0, errCurrentDisjointRewardInvalid
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return uint64(value.Int64()), nil
}

func currentDisjointRandomPercent() (float64, error) {
	value, err := currentDisjointRandomUint64(1_000_000)
	if err != nil {
		return 0, err
	}
	return float64(value) / 10_000, nil
}
