package dnfbridge

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// GMAvatarSetQuery is deliberately job-scoped. A package that contains
// several profession variants must never project another profession's avatar
// items into the dressing-room selection.
type GMAvatarSetQuery struct {
	Query  string
	Job    string
	Limit  int
	Offset int
}

type GMAvatarSetItem struct {
	Slot string        `json:"slot"`
	Item GMItemSummary `json:"item"`
}

type GMAvatarSetSummary struct {
	Package GMItemSummary     `json:"package"`
	Items   []GMAvatarSetItem `json:"items"`
}

type GMAvatarSetPage struct {
	Sets   []GMAvatarSetSummary `json:"sets"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

// GMSearchAvatarSets resolves real package, booster-selection and deterministic
// booster relationships from the active PVF. Only definitions that resolve at
// least three distinct avatar slots for the requested profession are returned.
// Ambiguous alternatives for the same slot remain excluded because an
// automatic "whole set" choice would otherwise invent the player's style.
func (s *Service) GMSearchAvatarSets(ctx context.Context, query GMAvatarSetQuery) (GMAvatarSetPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if query.Limit <= 0 {
		query.Limit = 24
	}
	if query.Limit > 100 || query.Offset < 0 {
		return GMAvatarSetPage{}, ErrGMInvalidRequest
	}
	job := gmItemTypeTag(query.Job)
	if job == "" || job == "all" {
		return GMAvatarSetPage{}, ErrGMInvalidRequest
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMAvatarSetPage{}, err
	}
	sets, err := gmAvatarSetsForJob(ctx, catalog, job)
	if err != nil {
		return GMAvatarSetPage{}, err
	}
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	matches := make([]GMAvatarSetSummary, 0, query.Limit)
	total := 0
	for _, summary := range sets {
		if err := ctx.Err(); err != nil {
			return GMAvatarSetPage{}, err
		}
		packageItem := summary.Package
		if needle != "" && !strings.Contains(strings.ToLower(packageItem.Name), needle) &&
			!strings.Contains(strings.ToLower(packageItem.PVFPath), needle) &&
			strconv.FormatUint(uint64(packageItem.ItemID), 10) != needle {
			continue
		}
		if total >= query.Offset && len(matches) < query.Limit {
			matches = append(matches, summary)
		}
		total++
	}
	return GMAvatarSetPage{Sets: matches, Total: total, Limit: query.Limit, Offset: query.Offset}, nil
}

func gmAvatarSetsForJob(ctx context.Context, catalog *pvfDungeonDropCatalog, job string) ([]GMAvatarSetSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalog.gmAvatarIndexMu.Lock()
	defer catalog.gmAvatarIndexMu.Unlock()
	if catalog.gmAvatarSetsByJob == nil {
		catalog.gmAvatarSetsByJob = make(map[string][]GMAvatarSetSummary)
	}
	if cached, ok := catalog.gmAvatarSetsByJob[job]; ok {
		return cached, nil
	}
	packageIDs := gmAvatarSetPackageIDs(ctx, catalog)
	sets := make([]GMAvatarSetSummary, 0, 512)
	for _, packageID := range packageIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		definition, resolveErr := catalog.ResolveItem(packageID)
		if resolveErr != nil {
			continue
		}
		effectiveJobs := gmItemEffectiveJobs(catalog, definition)
		if len(effectiveJobs) > 0 && !gmJobTagsContain(effectiveJobs, job) {
			continue
		}
		sets = append(sets, gmAvatarSetSummaries(catalog, definition, job, gmItemSummary(definition))...)
	}
	catalog.gmAvatarSetsByJob[job] = sets
	return sets, nil
}

func gmAvatarSetSummaries(
	catalog *pvfDungeonDropCatalog,
	definition dungeonDropItemDefinition,
	job string,
	packageItem GMItemSummary,
) []GMAvatarSetSummary {
	items, deterministic := gmAvatarSetItems(catalog, definition, job)
	if deterministic && len(items) >= 3 {
		return []GMAvatarSetSummary{{Package: packageItem, Items: items}}
	}
	selection := definition
	if normalizeDungeonDropStackableType(definition.StackableType) == "[cera booster]" {
		resolution, err := resolveCurrentMagicBox(catalog, catalog.source, int64(definition.ItemID))
		if err != nil || resolution.Kind != "random" || len(resolution.Groups) != 1 || len(resolution.Groups[0].Entries) != 1 {
			return nil
		}
		rewardID := resolution.Groups[0].Entries[0].ItemID
		if rewardID <= 0 || rewardID > int64(^uint32(0)) {
			return nil
		}
		selection, err = catalog.ResolveItem(uint32(rewardID))
		if err != nil || normalizeDungeonDropStackableType(selection.StackableType) != "[booster selection]" {
			return nil
		}
	}
	if normalizeDungeonDropStackableType(selection.StackableType) != "[booster selection]" {
		return nil
	}
	variants := gmAvatarSetChoiceVariants(catalog, selection, job)
	summaries := make([]GMAvatarSetSummary, 0, len(variants))
	for index, variant := range variants {
		variantPackage := packageItem
		variantPackage.ItemID = variant.source.ItemID
		variantPackage.PVFPath = variant.source.PVFPath
		variantPackage.Name = packageItem.Name + " · 款式" + strconv.Itoa(index+1)
		summaries = append(summaries, GMAvatarSetSummary{Package: variantPackage, Items: variant.items})
	}
	return summaries
}

type gmAvatarSetVariant struct {
	source dungeonDropItemDefinition
	items  []GMAvatarSetItem
}

func gmAvatarSetChoiceVariants(catalog *pvfDungeonDropCatalog, selection dungeonDropItemDefinition, job string) []gmAvatarSetVariant {
	booster, err := resolveCurrentBoosterDefinition(catalog, selection.ItemID, currentBoosterRequestSelection)
	if err != nil || booster.SelectionRequired != 1 {
		return nil
	}
	categoryIDs := make([]int, 0, len(booster.SelectionCategory))
	for categoryID := range booster.SelectionCategory {
		categoryIDs = append(categoryIDs, int(categoryID))
	}
	sort.Ints(categoryIDs)
	variants := make([]gmAvatarSetVariant, 0, 2)
	seen := make(map[string]struct{})
	for _, rawCategoryID := range categoryIDs {
		for _, candidate := range booster.SelectionCategory[uint16(rawCategoryID)] {
			reward, resolveErr := catalog.ResolveItem(candidate.ItemID)
			if resolveErr != nil {
				continue
			}
			items, deterministic := gmAvatarSetItems(catalog, reward, job)
			if !deterministic || len(items) < 3 {
				continue
			}
			signature := gmAvatarSetSignature(items)
			if _, duplicate := seen[signature]; duplicate {
				continue
			}
			seen[signature] = struct{}{}
			variants = append(variants, gmAvatarSetVariant{source: reward, items: items})
		}
	}
	return variants
}

func gmAvatarSetPackageIDs(ctx context.Context, catalog *pvfDungeonDropCatalog) []uint32 {
	catalog.gmAvatarSetMu.Lock()
	defer catalog.gmAvatarSetMu.Unlock()
	if catalog.gmAvatarSetReady {
		return append([]uint32(nil), catalog.gmAvatarSetIDs...)
	}
	ids := make([]uint32, 0, 1024)
	for _, itemID := range catalog.stackableIDs {
		if ctx.Err() != nil {
			break
		}
		definition, err := catalog.ResolveItem(itemID)
		stackableType := normalizeDungeonDropStackableType(definition.StackableType)
		if err == nil && (len(definition.PackageRewardIDs) > 0 ||
			stackableType == "[booster selection]" || stackableType == "[cera booster]") {
			ids = append(ids, itemID)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	catalog.gmAvatarSetIDs = append([]uint32(nil), ids...)
	catalog.gmAvatarSetReady = ctx.Err() == nil
	return ids
}

func gmAvatarSetItems(catalog *pvfDungeonDropCatalog, root dungeonDropItemDefinition, job string) ([]GMAvatarSetItem, bool) {
	visited := make(map[uint32]struct{})
	items, deterministic := gmResolveAvatarSetItems(catalog, root, job, visited, 0)
	if !deterministic {
		return nil, false
	}
	return gmNormalizeAvatarSetItems(items)
}

func gmNormalizeAvatarSetItems(rows []GMAvatarSetItem) ([]GMAvatarSetItem, bool) {
	bySlot := make(map[string]GMAvatarSetItem)
	for _, row := range rows {
		if row.Item.UsablePeriodDays > 0 || !row.Item.ExpirationDate.IsZero() {
			return nil, false
		}
		if previous, duplicate := bySlot[row.Slot]; duplicate && previous.Item.ItemID != row.Item.ItemID {
			return nil, false
		}
		bySlot[row.Slot] = row
	}
	items := make([]GMAvatarSetItem, 0, len(bySlot))
	for _, row := range bySlot {
		items = append(items, row)
	}
	sort.Slice(items, func(left, right int) bool {
		leftOrder, _ := gmAvatarSlot(items[left].Item.EquipmentType)
		rightOrder, _ := gmAvatarSlot(items[right].Item.EquipmentType)
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return items[left].Item.ItemID < items[right].Item.ItemID
	})
	return items, true
}

func gmResolveAvatarSetItems(
	catalog *pvfDungeonDropCatalog,
	definition dungeonDropItemDefinition,
	job string,
	visited map[uint32]struct{},
	depth int,
) ([]GMAvatarSetItem, bool) {
	if depth >= 8 {
		return nil, false
	}
	if definition.Kind == dungeonDropItemEquipment {
		item := gmItemSummary(definition)
		_, slot := gmAvatarSlot(item.EquipmentType)
		if slot != "" && gmItemMatchesJob(item, job) {
			return []GMAvatarSetItem{{Slot: slot, Item: item}}, true
		}
		return nil, true
	}
	if definition.Kind != dungeonDropItemStackable {
		return nil, true
	}
	if _, exists := visited[definition.ItemID]; exists {
		return nil, false
	}
	visited[definition.ItemID] = struct{}{}
	defer delete(visited, definition.ItemID)

	if len(definition.PackageRewardIDs) > 0 {
		items := make([]GMAvatarSetItem, 0, len(definition.PackageRewardIDs))
		for _, rewardID := range definition.PackageRewardIDs {
			reward, err := catalog.ResolveItem(rewardID)
			if err != nil {
				continue
			}
			rows, deterministic := gmResolveAvatarSetItems(catalog, reward, job, visited, depth+1)
			if !deterministic {
				return nil, false
			}
			items = append(items, rows...)
		}
		return items, true
	}

	switch normalizeDungeonDropStackableType(definition.StackableType) {
	case "[booster selection]":
		booster, err := resolveCurrentBoosterDefinition(catalog, definition.ItemID, currentBoosterRequestSelection)
		if err != nil {
			return nil, false
		}
		categoryIDs := make([]int, 0, len(booster.SelectionCategory))
		for categoryID := range booster.SelectionCategory {
			categoryIDs = append(categoryIDs, int(categoryID))
		}
		sort.Ints(categoryIDs)
		var selected []GMAvatarSetItem
		selectedSignature := ""
		for _, rawCategoryID := range categoryIDs {
			category := booster.SelectionCategory[uint16(rawCategoryID)]
			if booster.SelectionRequired > 0 && booster.SelectionRequired != len(category) {
				continue
			}
			rows := make([]GMAvatarSetItem, 0, len(category))
			deterministic := true
			for _, candidate := range category {
				reward, resolveErr := catalog.ResolveItem(candidate.ItemID)
				if resolveErr != nil {
					continue
				}
				candidateRows, candidateDeterministic := gmResolveAvatarSetItems(catalog, reward, job, visited, depth+1)
				if !candidateDeterministic {
					deterministic = false
					break
				}
				rows = append(rows, candidateRows...)
			}
			rows, deterministic = gmNormalizeAvatarSetItems(rows)
			if !deterministic || len(rows) < 3 {
				continue
			}
			signature := gmAvatarSetSignature(rows)
			if selectedSignature == "" {
				selected, selectedSignature = rows, signature
				continue
			}
			if signature != selectedSignature {
				return nil, false
			}
		}
		if selectedSignature == "" {
			return nil, true
		}
		return selected, true

	case "[cera booster]":
		resolution, err := resolveCurrentMagicBox(catalog, catalog.source, int64(definition.ItemID))
		if err != nil || resolution.Kind != "random" {
			return nil, false
		}
		items := make([]GMAvatarSetItem, 0, len(resolution.Groups))
		for _, group := range resolution.Groups {
			if len(group.Entries) != 1 {
				return nil, false
			}
			rewardID := group.Entries[0].ItemID
			if rewardID <= 0 || rewardID > int64(^uint32(0)) {
				return nil, false
			}
			reward, resolveErr := catalog.ResolveItem(uint32(rewardID))
			if resolveErr != nil {
				return nil, false
			}
			rows, deterministic := gmResolveAvatarSetItems(catalog, reward, job, visited, depth+1)
			if !deterministic {
				return nil, false
			}
			items = append(items, rows...)
		}
		return items, true
	default:
		return nil, true
	}
}

func gmAvatarSetSignature(items []GMAvatarSetItem) string {
	parts := make([]string, 0, len(items))
	for _, row := range items {
		parts = append(parts, row.Slot+":"+strconv.FormatUint(uint64(row.Item.ItemID), 10))
	}
	return strings.Join(parts, "|")
}

func gmAvatarSlot(equipmentType string) (int, string) {
	switch gmItemTypeTag(equipmentType) {
	case "hair avatar":
		return 0, "hair"
	case "hat avatar":
		return 1, "hat"
	case "face avatar":
		return 2, "face"
	case "breast avatar":
		return 3, "breast"
	case "coat avatar":
		return 4, "coat"
	case "waist avatar":
		return 5, "waist"
	case "pants avatar":
		return 6, "pants"
	case "shoes avatar":
		return 7, "shoes"
	case "skin avatar":
		return 8, "skin"
	case "aurora avatar":
		return 9, "aurora"
	case "weapon avatar":
		return 10, "weapon"
	default:
		return 100, ""
	}
}
