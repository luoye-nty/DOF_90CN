package dnfbridge

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	currentCustomAbilityPartSetInfoPath = "etc/customability/partsetcustominfo.etc"
	currentCustomAbilityPartSetListPath = "etc/customability/partsetcustomability.lst"
)

type currentCustomAbilitySetOptionRule struct {
	points   int
	group    int
	minParts byte
}

type currentCustomAbilitySetCatalog struct {
	partSetIndex byte
	minParts     byte
	maxParts     byte
	maxOptions   byte
	maxPoints    map[byte]map[byte]int
	materialID   uint32
	materialRate float64
	options      map[byte]currentCustomAbilitySetOptionRule
}

func parseCurrentCustomAbilitySetCatalog(source initialEquipmentTextSource) (*currentCustomAbilitySetCatalog, error) {
	if source == nil {
		return nil, errCurrentCustomAbilityPVF
	}
	listText, err := source.ReadText(currentCustomAbilityPartSetListPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityPartSetListPath, err)
	}
	listDocument, err := dnfpvf.Parse(currentCustomAbilityPartSetListPath, listText)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityPartSetListPath, err)
	}
	optionIDs := make(map[byte]struct{})
	for _, entry := range dnfpvf.ParseList(listDocument) {
		if entry.ID <= 0 || entry.ID > math.MaxUint8 {
			return nil, fmt.Errorf("%w: part-set option id=%d", errCurrentCustomAbilityPVF, entry.ID)
		}
		optionID := byte(entry.ID)
		if _, duplicate := optionIDs[optionID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate part-set option=%d", errCurrentCustomAbilityPVF, optionID)
		}
		optionIDs[optionID] = struct{}{}
	}
	if len(optionIDs) == 0 {
		return nil, fmt.Errorf("%w: empty part-set option list", errCurrentCustomAbilityPVF)
	}

	infoText, err := source.ReadText(currentCustomAbilityPartSetInfoPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityPartSetInfoPath, err)
	}
	infoDocument, err := dnfpvf.Parse(currentCustomAbilityPartSetInfoPath, infoText)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errCurrentCustomAbilityPVF, currentCustomAbilityPartSetInfoPath, err)
	}
	partSetIndex, partSetFound := infoDocument.Int("part set index")
	minParts, minPartsFound := infoDocument.Int("min part count")
	maxParts, maxPartsFound := infoDocument.Int("max part count")
	maxOptions, maxOptionsFound := infoDocument.Int("enchant max count")
	if !partSetFound || !minPartsFound || !maxPartsFound || !maxOptionsFound ||
		partSetIndex <= 0 || partSetIndex > math.MaxUint8 || minParts <= 0 ||
		maxParts < minParts || maxParts > math.MaxUint8 || maxOptions <= 0 || maxOptions > 5 {
		return nil, fmt.Errorf("%w: part-set bounds index=%d parts=%d..%d options=%d",
			errCurrentCustomAbilityPVF, partSetIndex, minParts, maxParts, maxOptions)
	}

	maxPointTable, err := parseCurrentCustomAbilitySetMaxPoints(infoDocument, byte(minParts), byte(maxParts))
	if err != nil {
		return nil, err
	}
	materialID, materialRate, err := parseCurrentCustomAbilitySetMaterialRate(infoDocument)
	if err != nil {
		return nil, err
	}
	points, err := parseCurrentCustomAbilitySetOptionPoints(infoDocument, optionIDs)
	if err != nil {
		return nil, err
	}
	options, err := parseCurrentCustomAbilitySetGroups(infoDocument, optionIDs, points, byte(minParts), byte(maxParts))
	if err != nil {
		return nil, err
	}
	return &currentCustomAbilitySetCatalog{
		partSetIndex: byte(partSetIndex),
		minParts:     byte(minParts),
		maxParts:     byte(maxParts),
		maxOptions:   byte(maxOptions),
		maxPoints:    maxPointTable,
		materialID:   materialID,
		materialRate: materialRate,
		options:      options,
	}, nil
}

func parseCurrentCustomAbilitySetMaxPoints(document *dnfpvf.Document, minParts byte, maxParts byte) (map[byte]map[byte]int, error) {
	tokens, found := document.Section("max point table")
	if !found {
		return nil, fmt.Errorf("%w: part-set max point table missing", errCurrentCustomAbilityPVF)
	}
	gradeValues := map[string]byte{"D": 0, "C": 1, "B": 2, "A": 3}
	result := make(map[byte]map[byte]int, len(gradeValues))
	currentGrade := byte(0)
	haveGrade := false
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent {
			grade, ok := gradeValues[strings.ToUpper(strings.TrimSpace(token.Value))]
			if !ok {
				return nil, fmt.Errorf("%w: part-set max point grade=%q", errCurrentCustomAbilityPVF, token.Value)
			}
			currentGrade = grade
			haveGrade = true
			continue
		}
		if token.Kind != dnfpvf.TokenInt {
			continue
		}
		if !haveGrade || index+1 >= len(tokens) || tokens[index+1].Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%w: malformed part-set max point table", errCurrentCustomAbilityPVF)
		}
		partCount := token.Int
		maximum := tokens[index+1].Int
		index++
		if partCount < int64(minParts) || partCount > int64(maxParts) || maximum <= 0 || maximum > math.MaxInt {
			return nil, fmt.Errorf("%w: part-set max point grade=%d parts=%d value=%d",
				errCurrentCustomAbilityPVF, currentGrade, partCount, maximum)
		}
		if result[currentGrade] == nil {
			result[currentGrade] = make(map[byte]int)
		}
		if _, duplicate := result[currentGrade][byte(partCount)]; duplicate {
			return nil, fmt.Errorf("%w: duplicate part-set max point grade=%d parts=%d",
				errCurrentCustomAbilityPVF, currentGrade, partCount)
		}
		result[currentGrade][byte(partCount)] = int(maximum)
	}
	for grade := byte(0); grade < 4; grade++ {
		for partCount := minParts; partCount <= maxParts; partCount++ {
			if result[grade][partCount] <= 0 {
				return nil, fmt.Errorf("%w: missing part-set max point grade=%d parts=%d",
					errCurrentCustomAbilityPVF, grade, partCount)
			}
		}
	}
	return result, nil
}

func parseCurrentCustomAbilitySetMaterialRate(document *dnfpvf.Document) (uint32, float64, error) {
	tokens, found := document.Section("material rate by point")
	if !found {
		return 0, 0, fmt.Errorf("%w: part-set material rate missing", errCurrentCustomAbilityPVF)
	}
	numbers := make([]float64, 0, 2)
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			numbers = append(numbers, float64(token.Int))
		case dnfpvf.TokenFloat:
			numbers = append(numbers, token.Float)
		}
	}
	if len(numbers) != 2 || numbers[0] <= 0 || numbers[0] > math.MaxUint32 ||
		numbers[0] != math.Trunc(numbers[0]) || numbers[1] <= 0 || math.IsNaN(numbers[1]) || math.IsInf(numbers[1], 0) {
		return 0, 0, fmt.Errorf("%w: invalid part-set material rate=%v", errCurrentCustomAbilityPVF, numbers)
	}
	return uint32(numbers[0]), numbers[1], nil
}

func parseCurrentCustomAbilitySetOptionPoints(document *dnfpvf.Document, valid map[byte]struct{}) (map[byte]int, error) {
	values := document.Ints("spend grade point")
	if len(values) == 0 || len(values)%2 != 0 {
		return nil, fmt.Errorf("%w: malformed part-set option points", errCurrentCustomAbilityPVF)
	}
	result := make(map[byte]int, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		optionValue, pointValue := values[index], values[index+1]
		if optionValue <= 0 || optionValue > math.MaxUint8 || pointValue <= 0 || pointValue > math.MaxInt {
			return nil, fmt.Errorf("%w: part-set option point id=%d value=%d", errCurrentCustomAbilityPVF, optionValue, pointValue)
		}
		optionID := byte(optionValue)
		if _, ok := valid[optionID]; !ok {
			return nil, fmt.Errorf("%w: part-set points reference unknown option=%d", errCurrentCustomAbilityPVF, optionID)
		}
		if _, duplicate := result[optionID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate part-set option point=%d", errCurrentCustomAbilityPVF, optionID)
		}
		result[optionID] = int(pointValue)
	}
	if len(result) != len(valid) {
		return nil, fmt.Errorf("%w: part-set point coverage=%d options=%d", errCurrentCustomAbilityPVF, len(result), len(valid))
	}
	return result, nil
}

func parseCurrentCustomAbilitySetGroups(
	document *dnfpvf.Document,
	valid map[byte]struct{},
	points map[byte]int,
	defaultMinParts byte,
	maxParts byte,
) (map[byte]currentCustomAbilitySetOptionRule, error) {
	result := make(map[byte]currentCustomAbilitySetOptionRule, len(valid))
	inGroup := false
	inIndexes := false
	inCondition := false
	groupKey := 0
	groupOptions := make([]byte, 0)
	conditionParts := defaultMinParts
	wantKey := false
	conditionName := ""

	commit := func() error {
		if groupKey <= 0 || len(groupOptions) == 0 || conditionParts < defaultMinParts || conditionParts > maxParts {
			return fmt.Errorf("%w: invalid part-set group key=%d options=%d min_parts=%d",
				errCurrentCustomAbilityPVF, groupKey, len(groupOptions), conditionParts)
		}
		for _, optionID := range groupOptions {
			if _, ok := valid[optionID]; !ok || points[optionID] <= 0 {
				return fmt.Errorf("%w: part-set group=%d unknown option=%d", errCurrentCustomAbilityPVF, groupKey, optionID)
			}
			if _, duplicate := result[optionID]; duplicate {
				return fmt.Errorf("%w: part-set option=%d appears in multiple groups", errCurrentCustomAbilityPVF, optionID)
			}
			result[optionID] = currentCustomAbilitySetOptionRule{points: points[optionID], group: groupKey, minParts: conditionParts}
		}
		return nil
	}

	for _, token := range document.Tokens {
		if token.Kind == dnfpvf.TokenSection {
			switch strings.ToLower(strings.TrimSpace(token.Value)) {
			case "group":
				if inGroup {
					return nil, fmt.Errorf("%w: nested part-set group", errCurrentCustomAbilityPVF)
				}
				inGroup = true
				inIndexes = false
				inCondition = false
				wantKey = false
				groupKey = 0
				groupOptions = groupOptions[:0]
				conditionParts = defaultMinParts
				conditionName = ""
			case "/group":
				if !inGroup {
					continue
				}
				if err := commit(); err != nil {
					return nil, err
				}
				inGroup = false
				inIndexes = false
				inCondition = false
			case "key":
				wantKey = inGroup
				inIndexes = false
			case "indexes":
				inIndexes = inGroup
				wantKey = false
			case "/indexes":
				inIndexes = false
			case "condition":
				inCondition = inGroup
				conditionName = ""
				wantKey = false
			case "/condition":
				inCondition = false
			}
			continue
		}
		if wantKey && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent || token.Kind == dnfpvf.TokenInt) {
			value := token.Value
			if token.Kind == dnfpvf.TokenInt {
				value = strconv.FormatInt(token.Int, 10)
			}
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed <= 0 {
				return nil, fmt.Errorf("%w: invalid part-set group key=%q", errCurrentCustomAbilityPVF, value)
			}
			groupKey = parsed
			wantKey = false
			continue
		}
		if inIndexes && token.Kind == dnfpvf.TokenInt {
			if token.Int <= 0 || token.Int > math.MaxUint8 {
				return nil, fmt.Errorf("%w: invalid part-set group option=%d", errCurrentCustomAbilityPVF, token.Int)
			}
			groupOptions = append(groupOptions, byte(token.Int))
			continue
		}
		if inCondition {
			if conditionName == "" && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent) {
				conditionName = strings.ToLower(strings.TrimSpace(token.Value))
				continue
			}
			if token.Kind == dnfpvf.TokenInt {
				if conditionName != "part count" || token.Int <= 0 || token.Int > math.MaxUint8 {
					return nil, fmt.Errorf("%w: unsupported part-set group condition=%q value=%d",
						errCurrentCustomAbilityPVF, conditionName, token.Int)
				}
				conditionParts = byte(token.Int)
			}
		}
	}
	if inGroup || len(result) != len(valid) {
		return nil, fmt.Errorf("%w: part-set group coverage=%d options=%d", errCurrentCustomAbilityPVF, len(result), len(valid))
	}
	return result, nil
}

func (catalog *currentCustomAbilitySetCatalog) evaluate(options []byte, grades []byte) (byte, int, int64, error) {
	if catalog == nil || len(options) == 0 || len(options) > int(catalog.maxOptions) ||
		len(grades) < int(catalog.minParts) || len(grades) > int(catalog.maxParts) {
		return 0, 0, 0, errCurrentCustomAbilitySetInvalid
	}
	gradeTotal := 0
	for _, grade := range grades {
		if grade > 3 {
			return 0, 0, 0, errCurrentCustomAbilitySetInvalid
		}
		gradeTotal += int(grade)
	}
	setGrade := byte(gradeTotal / len(grades))
	maximum := catalog.maxPoints[setGrade][byte(len(grades))]
	if maximum <= 0 {
		return 0, 0, 0, errCurrentCustomAbilitySetInvalid
	}
	remaining := maximum
	groups := make(map[int]struct{}, len(options))
	seen := make(map[byte]struct{}, len(options))
	for _, optionID := range options {
		rule, found := catalog.options[optionID]
		if !found || rule.points <= 0 || len(grades) < int(rule.minParts) {
			return 0, 0, 0, errCurrentCustomAbilitySetInvalid
		}
		if _, duplicate := seen[optionID]; duplicate {
			return 0, 0, 0, errCurrentCustomAbilitySetInvalid
		}
		seen[optionID] = struct{}{}
		if _, duplicate := groups[rule.group]; duplicate {
			return 0, 0, 0, errCurrentCustomAbilitySetInvalid
		}
		groups[rule.group] = struct{}{}
		contribution := rule.points * (remaining + 1000) / (rule.points + 1000)
		if contribution <= 0 || contribution > remaining {
			return 0, 0, 0, errCurrentCustomAbilitySetInvalid
		}
		remaining -= contribution
	}
	spent := maximum - remaining
	materialCost := int64(math.Floor(float64(spent) * catalog.materialRate))
	if spent <= 0 || materialCost <= 0 {
		return 0, 0, 0, errCurrentCustomAbilitySetInvalid
	}
	return setGrade, spent, materialCost, nil
}
