package dnfbridge

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCustomAbilityListPath = "etc/customability/individualcustomability.lst"
	currentCustomAbilityInfoPath = "etc/customability/individualcustominfo.etc"

	// ParseCurrentItemRow copies raw+0x72..+0x76 to equipment state
	// +0xBC..+0xC0. The current upgrade window reads the two individual
	// option IDs at +0xBC/+0xBD and the D/C/B/A grade at +0xC0.
	currentCustomAbilityRawBlockStart   = currentEpicProductionAbilityTailOffset
	currentCustomAbilityOption0Offset   = currentCustomAbilityRawBlockStart
	currentCustomAbilityOption1Offset   = currentCustomAbilityRawBlockStart + 1
	currentCustomAbilitySetRecordOffset = currentCustomAbilityRawBlockStart + 3
	currentCustomAbilityGradeOffset     = currentCustomAbilityRawBlockStart + 4
	currentCustomAbilityRawBlockEnd     = currentCustomAbilityRawBlockStart + currentEpicProductionAbilityTailSize

	// Compatibility-only offsets written by the first local implementation.
	// They are not consumed by the current upgrade window.
	currentCustomAbilityLegacyOption0Offset   = 0x67
	currentCustomAbilityLegacyOption1Offset   = 0x68
	currentCustomAbilityLegacySetRecordOffset = 0x6a
	currentCustomAbilityLegacyGradeOffset     = 0x6b
)

var errCurrentCustomAbilityPVF = errors.New("current custom ability PVF is invalid")

type currentCustomAbilityGradeWeight struct {
	grade  byte
	weight int
}

type currentCustomAbilityMaterialCost struct {
	itemID uint32
	count  int64
}

type currentCustomAbilityCatalog struct {
	optionIDs       map[byte]struct{}
	optionPaths     map[byte]string
	groups          [][]byte
	gradeWeight     []currentCustomAbilityGradeWeight
	totalWeight     int
	modifyMaterial  map[byte]map[byte]currentCustomAbilityMaterialCost
	upgradeMax      byte
	upgradeMaterial map[byte]currentCustomAbilityMaterialCost
}

type currentCustomAbilityItemProfile struct {
	applicable    bool
	optionCount   byte
	partSetIndex  byte
	equipmentType string
	equipmentSlot int16
	subType       int64
	hasSubType    bool
}

type currentCustomAbilityState struct {
	option0   byte
	option1   byte
	setRecord byte
	grade     byte
}

type currentCustomAbilityDrawIndex func(limit int) (int, error)
type currentCustomAbilityProfileResolver func(itemID uint32) (currentCustomAbilityItemProfile, error)

func parseCurrentCustomAbilityCatalog(source initialEquipmentTextSource) (*currentCustomAbilityCatalog, error) {
	if source == nil {
		return nil, errCurrentCustomAbilityPVF
	}
	listText, err := source.ReadText(currentCustomAbilityListPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityListPath, err)
	}
	listDocument, err := dnfpvf.Parse(currentCustomAbilityListPath, listText)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityListPath, err)
	}
	optionIDs := make(map[byte]struct{})
	optionPaths := make(map[byte]string)
	for _, entry := range dnfpvf.ParseList(listDocument) {
		if entry.ID <= 0 || entry.ID > math.MaxUint8 {
			return nil, fmt.Errorf("%w: option id=%d", errCurrentCustomAbilityPVF, entry.ID)
		}
		optionID := byte(entry.ID)
		if _, duplicate := optionIDs[optionID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate option id=%d", errCurrentCustomAbilityPVF, entry.ID)
		}
		optionIDs[optionID] = struct{}{}
		optionPath := strings.TrimSpace(strings.ReplaceAll(entry.Path, "\\", "/"))
		if !strings.HasPrefix(strings.ToLower(optionPath), "etc/customability/") {
			optionPath = "etc/customability/" + strings.TrimLeft(optionPath, "/")
		}
		if err := validateCurrentCustomAbilityOptionDocument(source, optionID, optionPath); err != nil {
			return nil, err
		}
		optionPaths[optionID] = optionPath
	}
	if len(optionIDs) == 0 {
		return nil, fmt.Errorf("%w: option list empty", errCurrentCustomAbilityPVF)
	}

	infoText, err := source.ReadText(currentCustomAbilityInfoPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityInfoPath, err)
	}
	infoDocument, err := dnfpvf.Parse(currentCustomAbilityInfoPath, infoText)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityInfoPath, err)
	}
	abilityType, found := infoDocument.Text("custom ability type")
	if !found || !strings.EqualFold(strings.TrimSpace(abilityType), "warriors dungeon") {
		return nil, fmt.Errorf("%w: ability type=%q", errCurrentCustomAbilityPVF, abilityType)
	}
	groups, err := parseCurrentCustomAbilityGroups(infoDocument, optionIDs)
	if err != nil {
		return nil, err
	}
	weights, totalWeight, err := parseCurrentCustomAbilityGradeWeights(infoDocument)
	if err != nil {
		return nil, err
	}
	modifyMaterial, err := parseCurrentCustomAbilityModifyMaterials(infoDocument, optionIDs)
	if err != nil {
		return nil, err
	}
	upgradeMax, upgradeMaterial, err := parseCurrentCustomAbilityUpgradeMaterials(infoDocument)
	if err != nil {
		return nil, err
	}
	return &currentCustomAbilityCatalog{
		optionIDs:       optionIDs,
		optionPaths:     optionPaths,
		groups:          groups,
		gradeWeight:     weights,
		totalWeight:     totalWeight,
		modifyMaterial:  modifyMaterial,
		upgradeMax:      upgradeMax,
		upgradeMaterial: upgradeMaterial,
	}, nil
}

func validateCurrentCustomAbilityOptionDocument(source initialEquipmentTextSource, optionID byte, path string) error {
	text, err := source.ReadText(path)
	if err != nil {
		return fmt.Errorf("%w: read option=%d path=%s: %v", errCurrentCustomAbilityPVF, optionID, path, err)
	}
	document, err := dnfpvf.Parse(path, text)
	if err != nil {
		return fmt.Errorf("%w: parse option=%d path=%s: %v", errCurrentCustomAbilityPVF, optionID, path, err)
	}
	validGrades := map[string]struct{}{"D": {}, "C": {}, "B": {}, "A": {}}
	seen := make(map[string]struct{}, len(validGrades))
	inParameter := false
	wantGrade := false
	for _, token := range document.Tokens {
		if token.Kind == dnfpvf.TokenSection {
			switch strings.ToLower(strings.TrimSpace(token.Value)) {
			case "parameter":
				inParameter = true
				wantGrade = false
			case "/parameter":
				inParameter = false
				wantGrade = false
			case "custom grade":
				wantGrade = inParameter
			default:
				wantGrade = false
			}
			continue
		}
		if !wantGrade || (token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent) {
			continue
		}
		grade := strings.ToUpper(strings.TrimSpace(token.Value))
		if _, ok := validGrades[grade]; !ok {
			return fmt.Errorf("%w: option=%d invalid grade=%q", errCurrentCustomAbilityPVF, optionID, grade)
		}
		if _, duplicate := seen[grade]; duplicate {
			return fmt.Errorf("%w: option=%d duplicate grade=%s", errCurrentCustomAbilityPVF, optionID, grade)
		}
		seen[grade] = struct{}{}
		wantGrade = false
	}
	if len(seen) != len(validGrades) {
		return fmt.Errorf("%w: option=%d grade_count=%d", errCurrentCustomAbilityPVF, optionID, len(seen))
	}
	return nil
}

func parseCurrentCustomAbilityGroups(document *dnfpvf.Document, valid map[byte]struct{}) ([][]byte, error) {
	if document == nil {
		return nil, errCurrentCustomAbilityPVF
	}
	groups := make([][]byte, 0, 3)
	inGroup := false
	inIndexes := false
	var current []byte
	covered := make(map[byte]struct{})
	for _, token := range document.Tokens {
		if token.Kind == dnfpvf.TokenSection {
			switch strings.ToLower(strings.TrimSpace(token.Value)) {
			case "group":
				inGroup = true
				inIndexes = false
				current = nil
			case "/group":
				if inGroup {
					if len(current) == 0 {
						return nil, fmt.Errorf("%w: empty option group", errCurrentCustomAbilityPVF)
					}
					groups = append(groups, append([]byte(nil), current...))
				}
				inGroup = false
				inIndexes = false
			case "indexes":
				inIndexes = inGroup
			case "/indexes":
				inIndexes = false
			}
			continue
		}
		if !inIndexes || token.Kind != dnfpvf.TokenInt {
			continue
		}
		if token.Int <= 0 || token.Int > math.MaxUint8 {
			return nil, fmt.Errorf("%w: group option=%d", errCurrentCustomAbilityPVF, token.Int)
		}
		optionID := byte(token.Int)
		if _, ok := valid[optionID]; !ok {
			return nil, fmt.Errorf("%w: group references unknown option=%d", errCurrentCustomAbilityPVF, optionID)
		}
		if _, duplicate := covered[optionID]; duplicate {
			return nil, fmt.Errorf("%w: option=%d appears in multiple groups", errCurrentCustomAbilityPVF, optionID)
		}
		covered[optionID] = struct{}{}
		current = append(current, optionID)
	}
	if len(groups) == 0 || len(covered) != len(valid) {
		return nil, fmt.Errorf("%w: group coverage=%d options=%d", errCurrentCustomAbilityPVF, len(covered), len(valid))
	}
	return groups, nil
}

func parseCurrentCustomAbilityGradeWeights(document *dnfpvf.Document) ([]currentCustomAbilityGradeWeight, int, error) {
	tokens, ok := document.Section("grade probability")
	if !ok {
		return nil, 0, fmt.Errorf("%w: grade probability missing", errCurrentCustomAbilityPVF)
	}
	gradeValue := map[string]byte{"D": 0, "C": 1, "B": 2, "A": 3}
	weights := make([]currentCustomAbilityGradeWeight, 0, len(gradeValue))
	seen := make(map[byte]struct{}, len(gradeValue))
	for index := 0; index+1 < len(tokens); index++ {
		if tokens[index].Kind != dnfpvf.TokenString && tokens[index].Kind != dnfpvf.TokenIdent {
			continue
		}
		grade, found := gradeValue[strings.ToUpper(strings.TrimSpace(tokens[index].Value))]
		if !found || tokens[index+1].Kind != dnfpvf.TokenInt || tokens[index+1].Int <= 0 || tokens[index+1].Int > math.MaxInt {
			continue
		}
		if _, duplicate := seen[grade]; duplicate {
			return nil, 0, fmt.Errorf("%w: duplicate grade=%d", errCurrentCustomAbilityPVF, grade)
		}
		seen[grade] = struct{}{}
		weights = append(weights, currentCustomAbilityGradeWeight{grade: grade, weight: int(tokens[index+1].Int)})
		index++
	}
	if len(seen) != len(gradeValue) {
		return nil, 0, fmt.Errorf("%w: grade weights=%d", errCurrentCustomAbilityPVF, len(seen))
	}
	total := 0
	for _, weight := range weights {
		if weight.weight > math.MaxInt-total {
			return nil, 0, fmt.Errorf("%w: grade weight overflow", errCurrentCustomAbilityPVF)
		}
		total += weight.weight
	}
	return weights, total, nil
}

func parseCurrentCustomAbilityModifyMaterials(
	document *dnfpvf.Document,
	valid map[byte]struct{},
) (map[byte]map[byte]currentCustomAbilityMaterialCost, error) {
	if document == nil || len(valid) == 0 {
		return nil, errCurrentCustomAbilityPVF
	}
	gradeValue := map[string]byte{"D": 0, "C": 1, "B": 2, "A": 3}
	materials := make(map[byte]map[byte]currentCustomAbilityMaterialCost, len(valid))
	inInfo := false
	wantIndex := false
	inMaterial := false
	wantGrade := false
	inList := false
	optionID := byte(0)
	gradeLabel := ""
	materialValues := make([]int64, 0, 2)
	current := make(map[byte]currentCustomAbilityMaterialCost, len(gradeValue))

	commitMaterial := func() error {
		grade, gradeOK := gradeValue[gradeLabel]
		if !gradeOK || len(materialValues) != 2 || materialValues[0] <= 0 ||
			materialValues[0] > math.MaxUint32 || materialValues[1] <= 0 {
			return fmt.Errorf("%w: invalid modify row option=%d grade=%q material=%v", errCurrentCustomAbilityPVF, optionID, gradeLabel, materialValues)
		}
		if _, duplicate := current[grade]; duplicate {
			return fmt.Errorf("%w: duplicate modify row option=%d grade=%s", errCurrentCustomAbilityPVF, optionID, gradeLabel)
		}
		current[grade] = currentCustomAbilityMaterialCost{itemID: uint32(materialValues[0]), count: materialValues[1]}
		return nil
	}
	commitInfo := func() error {
		if _, ok := valid[optionID]; !ok || len(current) != len(gradeValue) {
			return fmt.Errorf("%w: invalid modify info option=%d grades=%d", errCurrentCustomAbilityPVF, optionID, len(current))
		}
		if _, duplicate := materials[optionID]; duplicate {
			return fmt.Errorf("%w: duplicate modify info option=%d", errCurrentCustomAbilityPVF, optionID)
		}
		materials[optionID] = current
		return nil
	}

	for _, token := range document.Tokens {
		if token.Kind == dnfpvf.TokenSection {
			switch strings.ToLower(strings.TrimSpace(token.Value)) {
			case "info":
				if inInfo {
					return nil, fmt.Errorf("%w: nested modify info", errCurrentCustomAbilityPVF)
				}
				inInfo = true
				wantIndex = false
				inMaterial = false
				wantGrade = false
				inList = false
				optionID = 0
				current = make(map[byte]currentCustomAbilityMaterialCost, len(gradeValue))
			case "/info":
				if inMaterial || !inInfo {
					return nil, fmt.Errorf("%w: unterminated modify material", errCurrentCustomAbilityPVF)
				}
				if err := commitInfo(); err != nil {
					return nil, err
				}
				inInfo = false
			case "index":
				wantIndex = inInfo && !inMaterial
			case "material":
				if !inInfo || inMaterial || optionID == 0 {
					return nil, fmt.Errorf("%w: invalid modify material scope option=%d", errCurrentCustomAbilityPVF, optionID)
				}
				inMaterial = true
				wantGrade = false
				inList = false
				gradeLabel = ""
				materialValues = materialValues[:0]
			case "/material":
				if !inMaterial {
					return nil, fmt.Errorf("%w: unexpected modify material end", errCurrentCustomAbilityPVF)
				}
				if err := commitMaterial(); err != nil {
					return nil, err
				}
				inMaterial = false
				wantGrade = false
				inList = false
			case "grade":
				wantGrade = inMaterial
				inList = false
			case "list":
				inList = inMaterial
				wantGrade = false
				materialValues = materialValues[:0]
			case "/list":
				inList = false
			default:
				wantIndex = false
				wantGrade = false
			}
			continue
		}
		if wantIndex && token.Kind == dnfpvf.TokenInt {
			if token.Int <= 0 || token.Int > math.MaxUint8 {
				return nil, fmt.Errorf("%w: modify option=%d", errCurrentCustomAbilityPVF, token.Int)
			}
			optionID = byte(token.Int)
			wantIndex = false
			continue
		}
		if wantGrade && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent) {
			gradeLabel = strings.ToUpper(strings.TrimSpace(token.Value))
			wantGrade = false
			continue
		}
		if inList && token.Kind == dnfpvf.TokenInt {
			materialValues = append(materialValues, token.Int)
		}
	}
	if inInfo || inMaterial || len(materials) != len(valid) {
		return nil, fmt.Errorf("%w: modify material options=%d expected=%d", errCurrentCustomAbilityPVF, len(materials), len(valid))
	}
	return materials, nil
}

func parseCurrentCustomAbilityUpgradeMaterials(document *dnfpvf.Document) (byte, map[byte]currentCustomAbilityMaterialCost, error) {
	if document == nil {
		return 0, nil, errCurrentCustomAbilityPVF
	}
	upgradeMaxValue, found := document.Int("upgrade max")
	if !found || upgradeMaxValue != 4 {
		return 0, nil, fmt.Errorf("%w: upgrade max=%d", errCurrentCustomAbilityPVF, upgradeMaxValue)
	}
	gradeValue := map[string]byte{"D": 0, "C": 1, "B": 2, "A": 3}
	materials := make(map[byte]currentCustomAbilityMaterialCost, len(gradeValue))
	inUpgrade := false
	wantGrade := false
	inMaterial := false
	gradeLabel := ""
	materialValues := make([]int64, 0, 2)
	commit := func() error {
		grade, ok := gradeValue[gradeLabel]
		if !ok || len(materialValues) != 2 || materialValues[0] <= 0 || materialValues[0] > math.MaxUint32 || materialValues[1] <= 0 {
			return fmt.Errorf("%w: invalid upgrade row grade=%q material=%v", errCurrentCustomAbilityPVF, gradeLabel, materialValues)
		}
		if _, duplicate := materials[grade]; duplicate {
			return fmt.Errorf("%w: duplicate upgrade grade=%s", errCurrentCustomAbilityPVF, gradeLabel)
		}
		materials[grade] = currentCustomAbilityMaterialCost{itemID: uint32(materialValues[0]), count: materialValues[1]}
		return nil
	}
	for _, token := range document.Tokens {
		if token.Kind == dnfpvf.TokenSection {
			switch strings.ToLower(strings.TrimSpace(token.Value)) {
			case "upgrade info":
				inUpgrade = true
				wantGrade = false
				inMaterial = false
				gradeLabel = ""
				materialValues = materialValues[:0]
			case "/upgrade info":
				if inUpgrade {
					if err := commit(); err != nil {
						return 0, nil, err
					}
				}
				inUpgrade = false
				wantGrade = false
				inMaterial = false
			case "grade":
				wantGrade = inUpgrade
				inMaterial = false
			case "assign material":
				inMaterial = inUpgrade
				wantGrade = false
				materialValues = materialValues[:0]
			case "/assign material":
				inMaterial = false
			default:
				wantGrade = false
			}
			continue
		}
		if wantGrade && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent) {
			gradeLabel = strings.ToUpper(strings.TrimSpace(token.Value))
			wantGrade = false
			continue
		}
		if inMaterial && token.Kind == dnfpvf.TokenInt {
			materialValues = append(materialValues, token.Int)
		}
	}
	if len(materials) != len(gradeValue) {
		return 0, nil, fmt.Errorf("%w: upgrade material grades=%d", errCurrentCustomAbilityPVF, len(materials))
	}
	return byte(upgradeMaxValue), materials, nil
}

func currentCustomAbilitySecureDrawIndex(limit int) (int, error) {
	if limit <= 0 {
		return 0, errCurrentCustomAbilityPVF
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func (c *currentCustomAbilityCatalog) draw(drawIndex currentCustomAbilityDrawIndex) (currentCustomAbilityState, error) {
	if c == nil || len(c.groups) == 0 || c.totalWeight <= 0 || drawIndex == nil {
		return currentCustomAbilityState{}, errCurrentCustomAbilityPVF
	}
	drawOption := func() (byte, error) {
		groupIndex, err := drawIndex(len(c.groups))
		if err != nil || groupIndex < 0 || groupIndex >= len(c.groups) {
			return 0, fmt.Errorf("draw custom ability group: %w", err)
		}
		group := c.groups[groupIndex]
		optionIndex, err := drawIndex(len(group))
		if err != nil || optionIndex < 0 || optionIndex >= len(group) {
			return 0, fmt.Errorf("draw custom ability option: %w", err)
		}
		return group[optionIndex], nil
	}
	option0, err := drawOption()
	if err != nil {
		return currentCustomAbilityState{}, err
	}
	option1, err := drawOption()
	if err != nil {
		return currentCustomAbilityState{}, err
	}
	ticket, err := drawIndex(c.totalWeight)
	if err != nil || ticket < 0 || ticket >= c.totalWeight {
		return currentCustomAbilityState{}, fmt.Errorf("draw custom ability grade: %w", err)
	}
	for _, candidate := range c.gradeWeight {
		if ticket < candidate.weight {
			return currentCustomAbilityState{option0: option0, option1: option1, grade: candidate.grade}, nil
		}
		ticket -= candidate.weight
	}
	return currentCustomAbilityState{}, errCurrentCustomAbilityPVF
}

func currentCustomAbilityStateFromRaw(raw []byte, catalog *currentCustomAbilityCatalog) (currentCustomAbilityState, bool) {
	return currentCustomAbilityStateFromOffsets(
		raw,
		catalog,
		currentCustomAbilityOption0Offset,
		currentCustomAbilityOption1Offset,
		currentCustomAbilitySetRecordOffset,
		currentCustomAbilityGradeOffset,
	)
}

func currentCustomAbilityLegacyStateFromRaw(
	raw []byte,
	extra map[string]string,
	catalog *currentCustomAbilityCatalog,
) (currentCustomAbilityState, bool) {
	state, valid := currentCustomAbilityStateFromOffsets(
		raw,
		catalog,
		currentCustomAbilityLegacyOption0Offset,
		currentCustomAbilityLegacyOption1Offset,
		currentCustomAbilityLegacySetRecordOffset,
		currentCustomAbilityLegacyGradeOffset,
	)
	if !valid || !currentCustomAbilityExtraMatchesState(extra, state) {
		return currentCustomAbilityState{}, false
	}
	return state, true
}

func currentCustomAbilityExtraMatchesState(extra map[string]string, state currentCustomAbilityState) bool {
	if extra == nil {
		return false
	}
	for key, want := range map[string]byte{
		"custom_ability_option_0":   state.option0,
		"custom_ability_option_1":   state.option1,
		"custom_ability_set_record": state.setRecord,
		"custom_ability_grade":      state.grade,
	} {
		value, found := extra[key]
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 8)
		if !found || err != nil || byte(parsed) != want {
			return false
		}
	}
	return true
}

func currentCustomAbilityStateFromOffsets(
	raw []byte,
	catalog *currentCustomAbilityCatalog,
	option0Offset int,
	option1Offset int,
	setRecordOffset int,
	gradeOffset int,
) (currentCustomAbilityState, bool) {
	if len(raw) != currentItemListEntryWireSize || catalog == nil {
		return currentCustomAbilityState{}, false
	}
	state := currentCustomAbilityState{
		option0:   raw[option0Offset],
		option1:   raw[option1Offset],
		setRecord: raw[setRecordOffset],
		grade:     raw[gradeOffset],
	}
	if _, ok := catalog.optionIDs[state.option0]; !ok {
		return currentCustomAbilityState{}, false
	}
	if _, ok := catalog.optionIDs[state.option1]; !ok || state.grade > 3 {
		return currentCustomAbilityState{}, false
	}
	return state, true
}

func applyCurrentCustomAbilityState(raw []byte, extra map[string]string, state currentCustomAbilityState) ([]byte, map[string]string) {
	if len(raw) != currentItemListEntryWireSize {
		raw = make([]byte, currentItemListEntryWireSize)
	} else {
		raw = append([]byte(nil), raw...)
	}
	raw[currentCustomAbilityOption0Offset] = state.option0
	raw[currentCustomAbilityOption1Offset] = state.option1
	raw[currentCustomAbilitySetRecordOffset] = state.setRecord
	raw[currentCustomAbilityGradeOffset] = state.grade
	if extra == nil {
		extra = make(map[string]string)
	} else {
		clone := make(map[string]string, len(extra)+4)
		for key, value := range extra {
			clone[key] = value
		}
		extra = clone
	}
	extra["custom_ability_option_0"] = strconv.Itoa(int(state.option0))
	extra["custom_ability_option_1"] = strconv.Itoa(int(state.option1))
	extra["custom_ability_set_record"] = strconv.Itoa(int(state.setRecord))
	extra["custom_ability_grade"] = strconv.Itoa(int(state.grade))
	tailHex := hex.EncodeToString(raw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	extra["tail_data_72"] = tailHex
	for _, key := range []string{"mode1_raw_e_hex", "mode1_state_188_192_raw_hex"} {
		if _, exists := extra[key]; exists {
			extra[key] = tailHex
		}
	}
	return raw, extra
}

func resolveCurrentCustomAbilityInitialState(
	raw []byte,
	extra map[string]string,
	catalog *currentCustomAbilityCatalog,
	draw currentCustomAbilityDrawIndex,
) (currentCustomAbilityState, bool, error) {
	if state, valid := currentCustomAbilityStateFromRaw(raw, catalog); valid {
		return state, false, nil
	}
	if state, valid := currentCustomAbilityLegacyStateFromRaw(raw, extra, catalog); valid {
		return state, true, nil
	}
	state, err := catalog.draw(draw)
	return state, true, err
}

func initializeCurrentCustomAbilityInventory(
	record dnfrepo.InventoryRecord,
	catalog *currentCustomAbilityCatalog,
	resolve currentCustomAbilityProfileResolver,
	draw currentCustomAbilityDrawIndex,
) (dnfrepo.InventoryRecord, int, error) {
	return initializeCurrentCustomAbilityInventorySlots(record, nil, catalog, resolve, draw)
}

// initializeCurrentCustomAbilityInventorySlots initializes only the main
// inventory slots named by targetSlots. A nil targetSlots map preserves the
// cold-login repair behavior and scans the complete main inventory.
//
// Item-grant owners persist the newly acquired stack before the bridge builds
// the current EXE's incremental op14 row. Restricting initialization to those
// changed slots makes the first row authoritative without redrawing unrelated
// equipment or waiting for the next selected-character bootstrap.
func initializeCurrentCustomAbilityInventorySlots(
	record dnfrepo.InventoryRecord,
	targetSlots map[int16]struct{},
	catalog *currentCustomAbilityCatalog,
	resolve currentCustomAbilityProfileResolver,
	draw currentCustomAbilityDrawIndex,
) (dnfrepo.InventoryRecord, int, error) {
	if catalog == nil || resolve == nil || draw == nil {
		return record, 0, errCurrentCustomAbilityPVF
	}
	record = dnfrepo.CloneInventory(record)
	changed := 0
	for key, stack := range record.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count != 1 {
			continue
		}
		if targetSlots != nil {
			if _, selected := targetSlots[slot]; !selected {
				continue
			}
		}
		profile, err := resolve(uint32(stack.ItemID))
		if err != nil {
			return record, changed, err
		}
		if !profile.applicable || profile.optionCount != 2 {
			continue
		}
		entry := currentItemListEntryFromStack(listType, slot, stack)
		state, needsWrite, err := resolveCurrentCustomAbilityInitialState(entry.data[:], stack.Extra, catalog, draw)
		if err != nil {
			return record, changed, err
		}
		if !needsWrite {
			continue
		}
		stack.RawEntry, stack.Extra = applyCurrentCustomAbilityState(entry.data[:], stack.Extra, state)
		record.Slots[key] = stack
		changed++
	}
	return record, changed, nil
}

func initializeCurrentCustomAbilityEquipment(
	record dnfrepo.EquipmentRecord,
	catalog *currentCustomAbilityCatalog,
	resolve currentCustomAbilityProfileResolver,
	draw currentCustomAbilityDrawIndex,
) (dnfrepo.EquipmentRecord, int, error) {
	if catalog == nil || resolve == nil || draw == nil {
		return record, 0, errCurrentCustomAbilityPVF
	}
	record = dnfrepo.CloneEquipment(record)
	changed := 0
	for key, equipped := range record.Entries {
		if equipped.ItemID <= 0 || equipped.ItemID > math.MaxUint32 {
			continue
		}
		profile, err := resolve(uint32(equipped.ItemID))
		if err != nil {
			return record, changed, err
		}
		if !profile.applicable || profile.optionCount != 2 {
			continue
		}
		entry, ok := currentItemListEntryFromEquipment(equipped)
		if !ok {
			continue
		}
		state, needsWrite, err := resolveCurrentCustomAbilityInitialState(entry.data[:], equipped.Extra, catalog, draw)
		if err != nil {
			return record, changed, err
		}
		if !needsWrite {
			continue
		}
		equipped.RawEntry, equipped.Extra = applyCurrentCustomAbilityState(entry.data[:], equipped.Extra, state)
		record.Entries[key] = equipped
		changed++
	}
	return record, changed, nil
}

func currentCustomAbilityInventoryRefreshEntries(
	record dnfrepo.InventoryRecord,
	catalog *currentCustomAbilityCatalog,
	resolve currentCustomAbilityProfileResolver,
) ([]currentItemListEntry, error) {
	if catalog == nil || resolve == nil {
		return nil, errCurrentCustomAbilityPVF
	}
	entries := make([]currentItemListEntry, 0)
	for key, stack := range record.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count != 1 {
			continue
		}
		profile, err := resolve(uint32(stack.ItemID))
		if err != nil {
			return nil, err
		}
		if !profile.applicable || profile.optionCount != 2 {
			continue
		}
		entry := currentItemListEntryFromStack(listType, slot, stack)
		if _, valid := currentCustomAbilityStateFromRaw(entry.data[:], catalog); !valid {
			continue
		}
		entries = append(entries, entry)
	}
	sortCurrentItemListEntries(entries)
	return entries, nil
}

// currentDungeonPickupCustomAbilityFinalizer moves the existing cold-login
// initialization into the pickup owner's transaction. Fighting-spirit
// equipment therefore persists and projects its two authored custom abilities
// in the first op14 row instead of waiting for the next character login.
func (s *Service) currentDungeonPickupCustomAbilityFinalizer(
	definition dungeonDropItemDefinition,
) (currentDungeonPickupStackFinalizer, error) {
	if definition.Kind != dungeonDropItemEquipment ||
		!strings.EqualFold(strings.TrimSpace(definition.CustomAbilityType), "warriors dungeon") {
		return nil, nil
	}
	if definition.CustomAbilityCount != 2 {
		return nil, fmt.Errorf(
			"%w: item=%d custom ability count=%d",
			errCurrentCustomAbilityPVF,
			definition.ItemID,
			definition.CustomAbilityCount,
		)
	}
	profile, err := s.resolveCurrentCustomAbilityItemProfile(definition.ItemID)
	if err != nil {
		return nil, err
	}
	if !profile.applicable || profile.optionCount != 2 {
		return nil, fmt.Errorf("%w: item=%d custom ability profile is not applicable", errCurrentCustomAbilityPVF, definition.ItemID)
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		return nil, err
	}
	return func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
		state, needsWrite, err := resolveCurrentCustomAbilityInitialState(
			entry.data[:],
			stack.Extra,
			catalog,
			currentCustomAbilitySecureDrawIndex,
		)
		if err != nil {
			return dnfrepo.ItemStack{}, err
		}
		if needsWrite {
			stack.RawEntry, stack.Extra = applyCurrentCustomAbilityState(entry.data[:], stack.Extra, state)
		}
		return stack, nil
	}, nil
}

func (s *Service) initializeCurrentCustomAbilityIncrementalSlots(
	ctx context.Context,
	repository dnfrepo.InventoryRepository,
	record dnfrepo.InventoryRecord,
	targetSlots map[int16]struct{},
) (dnfrepo.InventoryRecord, int, error) {
	if s == nil || repository == nil || len(targetSlots) == 0 {
		return record, 0, nil
	}

	profileCache := make(map[uint32]currentCustomAbilityItemProfile)
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		if profile, found := profileCache[itemID]; found {
			return profile, nil
		}
		profile, err := s.resolveCurrentCustomAbilityItemProfile(itemID)
		if err == nil {
			profileCache[itemID] = profile
		}
		return profile, err
	}

	hasCustomAbilityTarget := false
	for slot := range targetSlots {
		stack, found := record.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)]
		if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count != 1 {
			continue
		}
		profile, err := resolve(uint32(stack.ItemID))
		if err != nil {
			return record, 0, err
		}
		if profile.applicable && profile.optionCount == 2 {
			hasCustomAbilityTarget = true
			break
		}
	}
	if !hasCustomAbilityTarget {
		return record, 0, nil
	}

	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return record, 0, err
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		return record, 0, err
	}
	updated, changed, err := initializeCurrentCustomAbilityInventorySlots(
		record,
		targetSlots,
		catalog,
		resolve,
		currentCustomAbilitySecureDrawIndex,
	)
	if err != nil || changed == 0 {
		return updated, changed, err
	}
	updated.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveInventoryFields(ctx, repository, updated, dnfrepo.InventoryFieldSlots); err != nil {
		return record, 0, err
	}
	return updated, changed, nil
}

func (s *Service) resolveCurrentCustomAbilityItemProfile(itemID uint32) (currentCustomAbilityItemProfile, error) {
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentCustomAbilityItemProfile{}, err
	}
	items.mu.Lock()
	reference, listed := items.itemRefs[itemID]
	items.mu.Unlock()
	if !listed || reference.kind != dungeonDropItemEquipment {
		return currentCustomAbilityItemProfile{}, nil
	}
	definition, err := items.ResolveItem(itemID)
	if err != nil {
		return currentCustomAbilityItemProfile{}, nil
	}
	if definition.Kind != dungeonDropItemEquipment {
		return currentCustomAbilityItemProfile{}, nil
	}
	s.initialEquipmentMu.Lock()
	archive, archiveErr := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if archiveErr != nil {
		return currentCustomAbilityItemProfile{}, archiveErr
	}
	text, err := archive.ReadText(definition.PVFPath)
	if err != nil {
		return currentCustomAbilityItemProfile{}, err
	}
	document, err := dnfpvf.Parse(definition.PVFPath, text)
	if err != nil {
		return currentCustomAbilityItemProfile{}, err
	}
	abilityType, found := document.Text("custom ability type")
	if !found || !strings.EqualFold(strings.TrimSpace(abilityType), "warriors dungeon") {
		return currentCustomAbilityItemProfile{}, nil
	}
	count, countFound := document.Int("custom ability count")
	partSetIndex, partSetFound := document.Int("custom ability part set index")
	equipmentType, equipmentTypeFound := document.Text("equipment type")
	placement, placementFound := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !countFound || count != 2 || !partSetFound || partSetIndex <= 0 || partSetIndex > math.MaxUint8 {
		return currentCustomAbilityItemProfile{}, fmt.Errorf("%w: item=%d count=%d part_set=%d", errCurrentCustomAbilityPVF, itemID, count, partSetIndex)
	}
	if !equipmentTypeFound || !placementFound || placement.class != currentEquipmentPlacementClassNormal {
		return currentCustomAbilityItemProfile{}, fmt.Errorf("%w: item=%d equipment_type=%q", errCurrentCustomAbilityPVF, itemID, equipmentType)
	}
	subType, hasSubType := document.Int("sub type")
	return currentCustomAbilityItemProfile{
		applicable:    true,
		optionCount:   byte(count),
		partSetIndex:  byte(partSetIndex),
		equipmentType: placement.pvfType,
		equipmentSlot: placement.targetSlot,
		subType:       subType,
		hasSubType:    hasSubType,
	}, nil
}

func (s *Service) ensureCurrentCustomAbilityState(ctx context.Context, characterID string) (int, int, []currentItemListEntry, error) {
	if s == nil || strings.TrimSpace(characterID) == "" {
		return 0, 0, nil, nil
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return 0, 0, nil, err
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		return 0, 0, nil, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.Equipment == nil {
		return 0, 0, nil, errors.New("custom ability repositories unavailable")
	}
	profileCache := make(map[uint32]currentCustomAbilityItemProfile)
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		if profile, found := profileCache[itemID]; found {
			return profile, nil
		}
		profile, err := s.resolveCurrentCustomAbilityItemProfile(itemID)
		if err == nil {
			profileCache[itemID] = profile
		}
		return profile, err
	}

	inventoryChanged := 0
	inventory, inventoryFound, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return 0, 0, nil, err
	}
	if inventoryFound {
		updated, changed, updateErr := initializeCurrentCustomAbilityInventory(inventory, catalog, resolve, currentCustomAbilitySecureDrawIndex)
		if updateErr != nil {
			return 0, 0, nil, updateErr
		}
		inventory = updated
		if changed > 0 {
			updated.UpdatedAt = time.Now().UTC()
			if err := dnfrepo.SaveInventoryFields(ctx, repositories.Inventory, updated, dnfrepo.InventoryFieldSlots); err != nil {
				return 0, 0, nil, err
			}
			inventoryChanged = changed
		}
	}
	refreshEntries, err := currentCustomAbilityInventoryRefreshEntries(inventory, catalog, resolve)
	if err != nil {
		return inventoryChanged, 0, nil, err
	}

	equipmentChanged := 0
	equipment, equipmentFound, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return inventoryChanged, 0, refreshEntries, err
	}
	if equipmentFound {
		updated, changed, updateErr := initializeCurrentCustomAbilityEquipment(equipment, catalog, resolve, currentCustomAbilitySecureDrawIndex)
		if updateErr != nil {
			return inventoryChanged, 0, refreshEntries, updateErr
		}
		if changed > 0 {
			updated.UpdatedAt = time.Now().UTC()
			if err := dnfrepo.SaveEquipmentFields(ctx, repositories.Equipment, updated, dnfrepo.EquipmentFieldEntries); err != nil {
				return inventoryChanged, 0, refreshEntries, err
			}
			equipmentChanged = changed
		}
	}
	return inventoryChanged, equipmentChanged, refreshEntries, nil
}
