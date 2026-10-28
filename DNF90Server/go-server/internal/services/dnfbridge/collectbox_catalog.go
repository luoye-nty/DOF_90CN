package dnfbridge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

type currentCollectboxCatalog struct {
	byIndex map[byte]dnfcollectbox.Definition
	indexes []byte
}

func (s *Service) currentCollectboxCatalog() (*currentCollectboxCatalog, error) {
	if s == nil {
		return nil, fmt.Errorf("collect box catalog: service required")
	}
	s.collectboxMu.Lock()
	defer s.collectboxMu.Unlock()
	if s.collectboxCatalog != nil || s.collectboxCatalogLoadErr != nil {
		return s.collectboxCatalog, s.collectboxCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		text, readErr := archive.ReadText("localization/collectbox/collectboxinfo.etc")
		if readErr != nil {
			err = fmt.Errorf("read localization/collectbox/collectboxinfo.etc: %w", readErr)
		} else {
			document, parseErr := dnfpvf.Parse("localization/collectbox/collectboxinfo.etc", text)
			if parseErr != nil {
				err = fmt.Errorf("parse localization/collectbox/collectboxinfo.etc: %w", parseErr)
			} else {
				buffText, buffErr := archive.ReadText("localization/collectbox/collectboxbuffinfo.etc")
				if buffErr != nil {
					err = fmt.Errorf("read localization/collectbox/collectboxbuffinfo.etc: %w", buffErr)
				} else {
					buffs, parseBuffErr := parseCurrentCollectboxBuffs(buffText)
					if parseBuffErr != nil {
						err = parseBuffErr
					} else {
						s.collectboxCatalog, err = buildCurrentCollectboxCatalogWithBuffs(document, buffs)
					}
				}
			}
		}
	}
	s.collectboxCatalogLoadErr = err
	return s.collectboxCatalog, err
}

func buildCurrentCollectboxCatalog(document *dnfpvf.Document) (*currentCollectboxCatalog, error) {
	return buildCurrentCollectboxCatalogWithBuffs(document, nil)
}

func buildCurrentCollectboxCatalogWithBuffs(document *dnfpvf.Document, buffs map[int64]dnfcollectbox.Bonus) (*currentCollectboxCatalog, error) {
	if document == nil {
		return nil, fmt.Errorf("collect box document is nil")
	}
	catalog := &currentCollectboxCatalog{byIndex: make(map[byte]dnfcollectbox.Definition)}
	for index, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "collect box info") {
			continue
		}
		end := len(document.Sections)
		for next := index + 1; next < len(document.Sections); next++ {
			if strings.EqualFold(strings.TrimSpace(document.Sections[next].Name), "collect box info") {
				end = next
				break
			}
		}
		definition, ok, err := parseCurrentCollectboxDefinitionWithBuffs(document, section, document.Sections[index+1:end], buffs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, exists := catalog.byIndex[definition.Index]; exists {
			return nil, fmt.Errorf("collect box index %d is duplicated", definition.Index)
		}
		catalog.byIndex[definition.Index] = definition
		catalog.indexes = append(catalog.indexes, definition.Index)
	}
	sort.Slice(catalog.indexes, func(i, j int) bool { return catalog.indexes[i] < catalog.indexes[j] })
	if len(catalog.indexes) == 0 {
		return nil, fmt.Errorf("collect box document has no valid definitions")
	}
	return catalog, nil
}

func parseCurrentCollectboxDefinition(document *dnfpvf.Document, parent dnfpvf.Section, children []dnfpvf.Section) (dnfcollectbox.Definition, bool, error) {
	return parseCurrentCollectboxDefinitionWithBuffs(document, parent, children, nil)
}

func parseCurrentCollectboxDefinitionWithBuffs(document *dnfpvf.Document, parent dnfpvf.Section, children []dnfpvf.Section, buffs map[int64]dnfcollectbox.Bonus) (dnfcollectbox.Definition, bool, error) {
	definition := dnfcollectbox.Definition{}
	indexPresent := false
	for _, child := range children {
		name := strings.ToLower(strings.TrimSpace(child.Name))
		if child.Start < 0 || child.End > len(document.Tokens) || child.Start > child.End {
			continue
		}
		tokens := document.Tokens[child.Start:child.End]
		switch name {
		case "index":
			if value, ok := firstCollectboxInt(tokens); ok && value >= 0 && value <= 255 {
				definition.Index = byte(value)
				indexPresent = true
			}
		case "base expiration date":
			definition.BaseExpirationDate = firstCollectboxText(tokens)
		case "max expiration date":
			definition.MaxExpirationDate = firstCollectboxText(tokens)
		case "extend item":
			values := collectboxInts(tokens)
			if len(values) >= 1 && values[0] > 0 && values[0] <= int64(^uint32(0)) {
				definition.ExtendItemID = uint32(values[0])
				if len(values) >= 2 && values[1] > 0 {
					definition.ExtendItemCount = values[1]
				} else {
					definition.ExtendItemCount = 1
				}
			}
		case "item position info":
			values := collectboxInts(tokens)
			if len(values) < 2 || values[0] <= 0 || values[1] <= 0 {
				continue
			}
			rowCount, slotsPerRow := values[0], values[1]
			total := rowCount * slotsPerRow
			if total <= 0 || total > 255 || len(values) < 2+int(total)*2 {
				return dnfcollectbox.Definition{}, false, fmt.Errorf("collect box index info has invalid slot count %d", total)
			}
			definition.Slots = make([]dnfcollectbox.Slot, 0, total)
			for offset := int64(0); offset < total; offset++ {
				itemID := values[2+offset*2]
				buffType := values[3+offset*2]
				if itemID == -1 && buffType > 0 {
					// PVF uses -1 as an unused trailing slot sentinel.
					continue
				}
				if itemID <= 0 || buffType <= 0 || itemID > int64(^uint32(0)) {
					return dnfcollectbox.Definition{}, false, fmt.Errorf("collect box index info has invalid item/count at slot %d", offset)
				}
				definition.Slots = append(definition.Slots, dnfcollectbox.Slot{ItemID: uint32(itemID), Count: 1, BuffType: buffType, Bonus: buffs[buffType]})
			}
		}
	}
	if !indexPresent {
		return dnfcollectbox.Definition{}, false, nil
	}
	if len(definition.Slots) == 0 {
		return dnfcollectbox.Definition{}, false, fmt.Errorf("collect box index %d has no item slots", definition.Index)
	}
	return definition, true, nil
}

func parseCurrentCollectboxBuffs(text string) (map[int64]dnfcollectbox.Bonus, error) {
	document, err := dnfpvf.Parse("localization/collectbox/collectboxbuffinfo.etc", text)
	if err != nil {
		return nil, fmt.Errorf("parse localization/collectbox/collectboxbuffinfo.etc: %w", err)
	}
	buffs := make(map[int64]dnfcollectbox.Bonus)
	for start, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "enchant info") {
			continue
		}
		values := collectboxInts(document.Tokens[section.Start:section.End])
		if len(values) != 1 || values[0] <= 0 {
			return nil, fmt.Errorf("collect box buff has invalid enchant index %v", values)
		}
		var bonus dnfcollectbox.Bonus
		for index := start + 1; index < len(document.Sections); index++ {
			field := document.Sections[index]
			if strings.EqualFold(strings.TrimSpace(field.Name), "/enchant info") {
				break
			}
			ints := collectboxInts(document.Tokens[field.Start:field.End])
			if len(ints) == 0 {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(field.Name)) {
			case "physical attack", "equipment physical attack":
				bonus.PhysicalAttack = ints[0]
			case "physical defense":
				bonus.PhysicalDefense = ints[0]
			case "magical attack", "equipment magical attack":
				bonus.MagicalAttack = ints[0]
			case "magical defense":
				bonus.MagicalDefense = ints[0]
			case "separate attack":
				bonus.IndependentAttack = ints[0]
			}
		}
		buffs[values[0]] = bonus
	}
	if len(buffs) == 0 {
		return nil, fmt.Errorf("collect box PVF has no buffs")
	}
	return buffs, nil
}

func firstCollectboxInt(tokens []dnfpvf.Token) (int64, bool) {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int, true
		}
	}
	return 0, false
}

func collectboxInts(tokens []dnfpvf.Token) []int64 {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values
}

func firstCollectboxText(tokens []dnfpvf.Token) string {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent {
			return strings.TrimSpace(token.Value)
		}
	}
	return ""
}

func collectboxDefinition(catalog *currentCollectboxCatalog, index byte) (dnfcollectbox.Definition, bool) {
	if catalog == nil {
		return dnfcollectbox.Definition{}, false
	}
	definition, ok := catalog.byIndex[index]
	return definition, ok
}

func collectboxDefinitionForItem(catalog *currentCollectboxCatalog, itemID uint32) (dnfcollectbox.Definition, bool) {
	if catalog == nil || itemID == 0 {
		return dnfcollectbox.Definition{}, false
	}
	for _, index := range catalog.indexes {
		definition, ok := collectboxDefinition(catalog, index)
		if !ok {
			continue
		}
		for _, slot := range definition.Slots {
			if slot.ItemID == itemID {
				return definition, true
			}
		}
	}
	return dnfcollectbox.Definition{}, false
}

func collectboxDefinitionIndexes(catalog *currentCollectboxCatalog) []string {
	if catalog == nil {
		return nil
	}
	out := make([]string, 0, len(catalog.indexes))
	for _, index := range catalog.indexes {
		out = append(out, strconv.Itoa(int(index)))
	}
	return out
}
