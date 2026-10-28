package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentVendingMachineListPVFPath = "etc/vendingmachine.lst"

var (
	errCurrentVendingMachineCatalogUnavailable = errors.New("dnf current vending-machine PVF catalog is unavailable")
	errCurrentVendingMachineDefinitionInvalid  = errors.New("dnf current vending-machine PVF definition is invalid")
	errCurrentVendingMachineGroupUnavailable   = errors.New("dnf current vending-machine group is unavailable")
)

// currentVendingMachineOutput is one relative-weight row from a PVF [output]
// section. Current NoPack always includes Weight in the draw table; JackpotFlag
// additionally adds the item/count to its separate machine-announcement list.
type currentVendingMachineOutput struct {
	ItemID      uint32
	Weight      uint64
	Count       uint32
	JackpotFlag byte
}

type currentVendingMachineGroup struct {
	MachineID      uint32
	GroupID        uint32
	PVFPath        string
	MaterialItemID uint32
	MaterialCount  uint32
	Outputs        []currentVendingMachineOutput
	TotalWeight    uint64
}

type currentVendingMachineCatalog struct {
	groups map[uint32]map[uint32]currentVendingMachineGroup
}

func newCurrentVendingMachineCatalog(source dnfpvf.Source) (*currentVendingMachineCatalog, error) {
	if source == nil {
		return nil, errCurrentVendingMachineCatalogUnavailable
	}
	listText, err := source.ReadText(currentVendingMachineListPVFPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errCurrentVendingMachineCatalogUnavailable, currentVendingMachineListPVFPath, err)
	}
	listDocument, err := dnfpvf.Parse(currentVendingMachineListPVFPath, listText)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errCurrentVendingMachineCatalogUnavailable, currentVendingMachineListPVFPath, err)
	}
	entries := dnfpvf.ParseList(listDocument)
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s contains no machines", errCurrentVendingMachineCatalogUnavailable, currentVendingMachineListPVFPath)
	}

	catalog := &currentVendingMachineCatalog{groups: make(map[uint32]map[uint32]currentVendingMachineGroup)}
	listedMachines := make(map[uint32]struct{}, len(entries))
	for _, entry := range entries {
		if entry.ID <= 0 || entry.ID > math.MaxUint32 {
			return nil, fmt.Errorf("%w: machine id=%d is outside u32", errCurrentVendingMachineDefinitionInvalid, entry.ID)
		}
		machineID := uint32(entry.ID)
		if _, duplicate := listedMachines[machineID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate machine id=%d", errCurrentVendingMachineDefinitionInvalid, machineID)
		}
		listedMachines[machineID] = struct{}{}

		documentPath, document, readErr := readCurrentVendingMachineDocument(source, entry.Path)
		if readErr != nil {
			return nil, fmt.Errorf("%w: machine=%d path=%s: %v", errCurrentVendingMachineCatalogUnavailable, machineID, entry.Path, readErr)
		}
		groups, parseErr := parseCurrentVendingMachineDefinition(machineID, documentPath, document)
		if parseErr != nil {
			return nil, parseErr
		}
		// The runtime list contains an intentionally empty dummy definition. Empty
		// [item group] blocks do not create a requestable machine or group.
		if len(groups) == 0 {
			continue
		}
		catalog.groups[machineID] = groups
	}
	if len(catalog.groups) == 0 {
		return nil, fmt.Errorf("%w: no usable machine groups", errCurrentVendingMachineCatalogUnavailable)
	}
	return catalog, nil
}

func (c *currentVendingMachineCatalog) Resolve(machineID, groupID uint32) (currentVendingMachineGroup, error) {
	if c == nil || c.groups == nil {
		return currentVendingMachineGroup{}, errCurrentVendingMachineCatalogUnavailable
	}
	groups, ok := c.groups[machineID]
	if !ok {
		return currentVendingMachineGroup{}, fmt.Errorf("%w: machine=%d group=%d", errCurrentVendingMachineGroupUnavailable, machineID, groupID)
	}
	group, ok := groups[groupID]
	if !ok {
		return currentVendingMachineGroup{}, fmt.Errorf("%w: machine=%d group=%d", errCurrentVendingMachineGroupUnavailable, machineID, groupID)
	}
	group.Outputs = append([]currentVendingMachineOutput(nil), group.Outputs...)
	return group, nil
}

func (c *currentVendingMachineCatalog) MachineCount() int {
	if c == nil {
		return 0
	}
	return len(c.groups)
}

func readCurrentVendingMachineDocument(source dnfpvf.Source, listedPath string) (string, *dnfpvf.Document, error) {
	cleaned := cleanCurrentVendingMachinePath(listedPath)
	if cleaned == "" || !strings.EqualFold(path.Ext(cleaned), ".vm") {
		return "", nil, fmt.Errorf("%w: listed path=%q is not a .vm document", errCurrentVendingMachineDefinitionInvalid, listedPath)
	}
	candidates := []string{cleaned}
	if !strings.HasPrefix(strings.ToLower(cleaned), "etc/") {
		candidates = append(candidates, path.Join("etc", cleaned))
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := source.ReadText(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		document, err := dnfpvf.Parse(candidate, text)
		if err != nil {
			return "", nil, err
		}
		return candidate, document, nil
	}
	return "", nil, lastErr
}

func cleanCurrentVendingMachinePath(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "`\"'"))
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

type currentVendingMachineGroupSections struct {
	groupNum []dnfpvf.Token
	material []dnfpvf.Token
	output   []dnfpvf.Token
	seen     map[string]struct{}
}

func parseCurrentVendingMachineDefinition(
	machineID uint32,
	documentPath string,
	document *dnfpvf.Document,
) (map[uint32]currentVendingMachineGroup, error) {
	if machineID == 0 || document == nil {
		return nil, fmt.Errorf("%w: machine=%d path=%s", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath)
	}
	groups := make(map[uint32]currentVendingMachineGroup)
	var current *currentVendingMachineGroupSections
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "item group":
			if current != nil {
				return nil, fmt.Errorf("%w: machine=%d path=%s nested [item group]", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath)
			}
			current = &currentVendingMachineGroupSections{seen: make(map[string]struct{}, 3)}
		case "/item group":
			if current == nil {
				return nil, fmt.Errorf("%w: machine=%d path=%s unexpected [/item group]", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath)
			}
			group, empty, err := buildCurrentVendingMachineGroup(machineID, documentPath, *current)
			if err != nil {
				return nil, err
			}
			if !empty {
				if _, duplicate := groups[group.GroupID]; duplicate {
					return nil, fmt.Errorf("%w: machine=%d duplicate group=%d", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID)
				}
				groups[group.GroupID] = group
			}
			current = nil
		case "group num", "material", "output":
			if current == nil {
				return nil, fmt.Errorf("%w: machine=%d path=%s [%s] is outside [item group]", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath, name)
			}
			if _, duplicate := current.seen[name]; duplicate {
				return nil, fmt.Errorf("%w: machine=%d path=%s duplicate [%s]", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath, name)
			}
			current.seen[name] = struct{}{}
			tokens, err := currentVendingMachineSectionTokens(document, section)
			if err != nil {
				return nil, fmt.Errorf("%w: machine=%d path=%s [%s]: %v", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath, name, err)
			}
			switch name {
			case "group num":
				current.groupNum = tokens
			case "material":
				current.material = tokens
			case "output":
				current.output = tokens
			}
		}
	}
	if current != nil {
		return nil, fmt.Errorf("%w: machine=%d path=%s unclosed [item group]", errCurrentVendingMachineDefinitionInvalid, machineID, documentPath)
	}
	return groups, nil
}

func currentVendingMachineSectionTokens(document *dnfpvf.Document, section dnfpvf.Section) ([]dnfpvf.Token, error) {
	if document == nil || section.Start < 0 || section.End < section.Start || section.End > len(document.Tokens) {
		return nil, errors.New("invalid token range")
	}
	tokens := append([]dnfpvf.Token(nil), document.Tokens[section.Start:section.End]...)
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("line=%d column=%d token=%q is not an integer", token.Line, token.Column, token.Raw)
		}
	}
	return tokens, nil
}

func buildCurrentVendingMachineGroup(
	machineID uint32,
	documentPath string,
	sections currentVendingMachineGroupSections,
) (currentVendingMachineGroup, bool, error) {
	if len(sections.seen) == 0 {
		return currentVendingMachineGroup{}, true, nil
	}
	if len(sections.groupNum) != 1 || len(sections.material) != 2 || len(sections.output) == 0 || len(sections.output)%4 != 0 {
		return currentVendingMachineGroup{}, false, fmt.Errorf(
			"%w: machine=%d path=%s group_num=%d material=%d output=%d",
			errCurrentVendingMachineDefinitionInvalid,
			machineID,
			documentPath,
			len(sections.groupNum),
			len(sections.material),
			len(sections.output),
		)
	}
	groupIDValue := sections.groupNum[0].Int
	materialIDValue := sections.material[0].Int
	materialCountValue := sections.material[1].Int
	if groupIDValue <= 0 || groupIDValue > math.MaxUint32 {
		return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d is outside u32", errCurrentVendingMachineDefinitionInvalid, machineID, groupIDValue)
	}
	if materialIDValue <= 0 || materialIDValue > math.MaxUint32 || materialCountValue <= 0 || materialCountValue > math.MaxUint32 {
		return currentVendingMachineGroup{}, false, fmt.Errorf(
			"%w: machine=%d group=%d material=%d count=%d",
			errCurrentVendingMachineDefinitionInvalid,
			machineID,
			groupIDValue,
			materialIDValue,
			materialCountValue,
		)
	}

	group := currentVendingMachineGroup{
		MachineID:      machineID,
		GroupID:        uint32(groupIDValue),
		PVFPath:        documentPath,
		MaterialItemID: uint32(materialIDValue),
		MaterialCount:  uint32(materialCountValue),
		Outputs:        make([]currentVendingMachineOutput, 0, len(sections.output)/4),
	}
	positiveWeight := false
	for index := 0; index < len(sections.output); index += 4 {
		itemIDValue := sections.output[index].Int
		weightValue := sections.output[index+1].Int
		countValue := sections.output[index+2].Int
		flagValue := sections.output[index+3].Int
		if itemIDValue <= 0 || itemIDValue > math.MaxUint32 {
			return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d output=%d item=%d", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID, index/4, itemIDValue)
		}
		if weightValue < 0 {
			return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d output=%d weight=%d", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID, index/4, weightValue)
		}
		if countValue <= 0 || countValue > math.MaxUint32 {
			return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d output=%d count=%d", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID, index/4, countValue)
		}
		if flagValue != 0 && flagValue != 1 {
			return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d output=%d jackpot_flag=%d", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID, index/4, flagValue)
		}
		weight := uint64(weightValue)
		if math.MaxUint64-group.TotalWeight < weight {
			return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d total weight overflows u64", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID)
		}
		group.TotalWeight += weight
		positiveWeight = positiveWeight || weight > 0
		group.Outputs = append(group.Outputs, currentVendingMachineOutput{
			ItemID:      uint32(itemIDValue),
			Weight:      weight,
			Count:       uint32(countValue),
			JackpotFlag: byte(flagValue),
		})
	}
	if !positiveWeight {
		return currentVendingMachineGroup{}, false, fmt.Errorf("%w: machine=%d group=%d has no positive output weight", errCurrentVendingMachineDefinitionInvalid, machineID, group.GroupID)
	}
	return group, false, nil
}
