package inventory

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func (o *Owner) useTPBook(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
	skillCatalog *dnfskill.Table,
	initialSkillLevels map[uint16]int,
	skillPointBaseline *dnfrepo.SkillPointState,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || resolution.TPBook == nil {
		return UseStackableResult{}, ErrTPBookContract
	}
	effect := *resolution.TPBook
	legacyAddTP := effect.Kind == alignedcmd.SkillPointBookUnknown && effect.Delta > 0
	// Preserve the old internal test/helper contract. Production PVF
	// resolutions always carry an explicit kind.
	if effect.Kind == alignedcmd.SkillPointBookUnknown && effect.Delta > 0 {
		effect.Kind = alignedcmd.SkillPointBookAddTP
	}
	resolvedType := normalizeTPBookStackableType(resolution.StackableType)
	if cmd.SourceListType != listTypeMain || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" || resolvedType != "[etc]" ||
		effect.MinimumLevel <= 0 || !validSkillPointBookEffect(effect) {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrTPBookContract, cmd.ItemTemplateID, resolution)
	}
	if effect.Kind != alignedcmd.SkillPointBookAddTP || resolution.TPBook.Kind != alignedcmd.SkillPointBookUnknown {
		if skillPointBaseline == nil {
			return UseStackableResult{}, fmt.Errorf("%w: item=%d has no active-PVF point baseline", ErrTPBookContract, cmd.ItemTemplateID)
		}
	}
	if effect.Kind == alignedcmd.SkillPointBookResetTP && (skillCatalog == nil || initialSkillLevels == nil) {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d has no active-PVF TP skill rules", ErrTPBookContract, cmd.ItemTemplateID)
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	updatedAt := time.Now().UTC()
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil || tx.Inventory == nil {
			return ErrTPBookContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != characterID ||
			(strings.TrimSpace(cmd.AccountID) != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID)) {
			return ErrCharacterRequired
		}
		if int64(character.Level) < effect.MinimumLevel ||
			(effect.MaximumLevel > 0 && int64(character.Level) > effect.MaximumLevel) {
			return fmt.Errorf("%w: item=%d character_level=%d allowed=%d..%d", ErrTPBookContract, resolution.ItemID, character.Level, effect.MinimumLevel, effect.MaximumLevel)
		}
		jobValue := uint64(0)
		if !legacyAddTP || strings.TrimSpace(character.Job) != "" {
			jobValue, err = strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
			if err != nil {
				return fmt.Errorf("%w: item=%d job=%q", ErrTPBookContract, resolution.ItemID, character.Job)
			}
		}
		job := byte(jobValue)

		skill, found, err := tx.Skill.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || skill.CharacterID != characterID {
			return ErrTPBookContract
		}
		skill = dnfrepo.CloneSkill(skill)
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		bonusSP, bonusTP, err := validatedBookBonuses(character.Stats)
		if err != nil {
			return err
		}
		pointBaseline := skillPointBookBaseline(skill, skillPointBaseline)
		baselineBonusSP := bonusSP
		baselineBonusTP := bonusTP
		if skillPointBaseline == nil {
			// Legacy internal callers supplied an already-bonused durable ledger.
			// Production callers always provide the ordinary PVF baseline.
			baselineBonusSP = 0
			baselineBonusTP = 0
		}
		target, err := skillPointBookTarget(pointBaseline, character.Level, baselineBonusSP, baselineBonusTP)
		if err != nil {
			return err
		}

		previousSP := skill.Points.RemainingSP
		previousTP := skill.Points.RemainingTP
		skill.Points, err = rebaseSkillPointBookLedger(skill.Points, target, skill.Skills, skillCatalog, job, initialSkillLevels)
		if err != nil {
			return fmt.Errorf("%w: primary tree: %v", ErrTPBookContract, err)
		}
		for treeIndex, tree := range skill.Trees {
			tree.Points, err = rebaseSkillPointBookLedger(tree.Points, target, tree.Skills, skillCatalog, job, initialSkillLevels)
			if err != nil {
				return fmt.Errorf("%w: alternate tree=%d: %v", ErrTPBookContract, treeIndex, err)
			}
			skill.Trees[treeIndex] = tree
		}
		// Report the repaired authoritative pre-use values rather than a stale
		// legacy ledger that could have caused the old intermittent rejection.
		previousSP = skill.Points.RemainingSP
		previousTP = skill.Points.RemainingTP

		skillFields := []dnfrepo.SkillField{dnfrepo.SkillFieldPoints, dnfrepo.SkillFieldTrees}
		switch effect.Kind {
		case alignedcmd.SkillPointBookAddSP:
			if err := addSkillPointBookPoints(&skill, int(effect.Delta), 0); err != nil {
				return err
			}
			if bonusSP > math.MaxUint16-int(effect.Delta) {
				return fmt.Errorf("%w: persisted bonus_sp=%d delta=%d", ErrTPBookContract, bonusSP, effect.Delta)
			}
			character.Stats["bonus_sp"] = int64(bonusSP) + effect.Delta
		case alignedcmd.SkillPointBookAddTP:
			if err := addSkillPointBookPoints(&skill, 0, int(effect.Delta)); err != nil {
				return err
			}
			if bonusTP > math.MaxUint16-int(effect.Delta) {
				return fmt.Errorf("%w: persisted bonus_tp=%d delta=%d", ErrTPBookContract, bonusTP, effect.Delta)
			}
			character.Stats["bonus_tp"] = int64(bonusTP) + effect.Delta
		case alignedcmd.SkillPointBookResetTP:
			resetPrimary, err := resetTPBookTree(skill.Skills, skill.Layouts[0], skill.Points, skillCatalog, job, initialSkillLevels)
			if err != nil {
				return err
			}
			skill.Skills = resetPrimary.Skills
			skill.Points = resetPrimary.Points
			if skill.Layouts == nil {
				skill.Layouts = make(map[int]dnfrepo.SkillLayout)
			}
			skill.Layouts[0] = resetPrimary.Layout
			for treeIndex, tree := range skill.Trees {
				resetTree, resetErr := resetTPBookTree(tree.Skills, skill.Layouts[treeIndex], tree.Points, skillCatalog, job, initialSkillLevels)
				if resetErr != nil {
					return fmt.Errorf("%w: alternate tree=%d: %v", ErrTPBookContract, treeIndex, resetErr)
				}
				tree.Skills = resetTree.Skills
				tree.Points = resetTree.Points
				skill.Trees[treeIndex] = tree
				skill.Layouts[treeIndex] = resetTree.Layout
			}
			skillFields = append(skillFields, dnfrepo.SkillFieldSkills, dnfrepo.SkillFieldLayouts)
		default:
			return ErrTPBookContract
		}
		skill.UpdatedAt = updatedAt
		character.UpdatedAt = updatedAt

		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
		}
		durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
		durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
		durableType := normalizeTPBookStackableType(stack.Extra["stackable_type"])
		if durableKind != "stackable" || durablePath == "" ||
			!strings.EqualFold(strings.ReplaceAll(durablePath, "\\", "/"), strings.ReplaceAll(strings.TrimSpace(resolution.PVFPath), "\\", "/")) ||
			(durableType != "" && durableType != resolvedType) {
			return fmt.Errorf("%w: item=%d durable_kind=%q durable_path=%q durable_type=%q resolved_type=%q", ErrTPBookContract, stack.ItemID, durableKind, durablePath, durableType, resolvedType)
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			updateStackRawAmount(&stack)
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = updatedAt
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if effect.Kind == alignedcmd.SkillPointBookAddSP || effect.Kind == alignedcmd.SkillPointBookAddTP {
			if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill, skillFields...); err != nil {
			return err
		}

		result = UseStackableResult{
			CharacterID:              characterID,
			AccountID:                cmd.AccountID,
			ListType:                 cmd.SourceListType,
			SlotIndex:                cmd.SourceSlotIndex,
			InstanceValue:            cmd.SourceInstanceValue,
			ItemID:                   stack.ItemID,
			RemainingCount:           remaining,
			Changed:                  true,
			PVFPath:                  durablePath,
			StackableType:            resolution.StackableType,
			TPBookApplied:            true,
			TPBookDelta:              skillPointBookLegacyTPDelta(effect),
			TPBookPreviousRemaining:  previousTP,
			TPBookNewRemaining:       skill.Points.RemainingTP,
			SkillPointBookKind:       effect.Kind,
			SkillPointBookSPDelta:    skillPointBookSPDelta(effect),
			SkillPointBookTPDelta:    skillPointBookTPDelta(effect),
			SkillPointBookTPReset:    effect.Kind == alignedcmd.SkillPointBookResetTP,
			SkillPointBookPreviousSP: previousSP,
			SkillPointBookNewSP:      skill.Points.RemainingSP,
			SkillPointBookPreviousTP: previousTP,
			SkillPointBookNewTP:      skill.Points.RemainingTP,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func validSkillPointBookEffect(effect alignedcmd.TPBookResolution) bool {
	switch effect.Kind {
	case alignedcmd.SkillPointBookAddSP, alignedcmd.SkillPointBookAddTP:
		return effect.Delta > 0 && effect.Delta <= math.MaxUint16
	case alignedcmd.SkillPointBookResetTP:
		return effect.Delta == 0
	default:
		return false
	}
}

func validatedBookBonuses(stats map[string]int64) (int, int, error) {
	bonusSP := stats["bonus_sp"]
	bonusTP := stats["bonus_tp"]
	if bonusSP < 0 || bonusSP > math.MaxUint16 || bonusTP < 0 || bonusTP > math.MaxUint16 {
		return 0, 0, fmt.Errorf("%w: bonus_sp=%d bonus_tp=%d", ErrTPBookContract, bonusSP, bonusTP)
	}
	return int(bonusSP), int(bonusTP), nil
}

func skillPointBookTarget(baseline dnfrepo.SkillPointState, level, bonusSP, bonusTP int) (dnfrepo.SkillPointState, error) {
	if baseline.SyncedLevel != level || baseline.TotalSP < 0 || baseline.TotalTP < 0 ||
		baseline.TotalSP > math.MaxUint16-bonusSP || baseline.TotalTP > math.MaxUint16-bonusTP {
		return dnfrepo.SkillPointState{}, fmt.Errorf("%w: level=%d baseline=%+v bonus_sp=%d bonus_tp=%d", ErrTPBookContract, level, baseline, bonusSP, bonusTP)
	}
	baseline.TotalSP += bonusSP
	baseline.RemainingSP = baseline.TotalSP
	baseline.TotalTP += bonusTP
	baseline.RemainingTP = baseline.TotalTP
	return baseline, nil
}

func rebaseSkillPointBookLedger(
	current dnfrepo.SkillPointState,
	target dnfrepo.SkillPointState,
	states map[int64]dnfrepo.SkillState,
	catalog *dnfskill.Table,
	job byte,
	initial map[uint16]int,
) (dnfrepo.SkillPointState, error) {
	if target.SyncedLevel <= 0 || target.TotalSP < 0 || target.TotalTP < 0 || target.TotalSP > math.MaxUint16 || target.TotalTP > math.MaxUint16 {
		return dnfrepo.SkillPointState{}, fmt.Errorf("invalid target ledger %+v", target)
	}
	spentSP := 0
	spentTP := 0
	if current == (dnfrepo.SkillPointState{}) {
		var err error
		spentSP, spentTP, err = learnedSkillPointBookCosts(catalog, job, initial, states)
		if err != nil {
			return dnfrepo.SkillPointState{}, err
		}
	} else {
		if current.SyncedLevel <= 0 || current.SyncedLevel > target.SyncedLevel || current.TotalSP < 0 || current.RemainingSP < 0 || current.RemainingSP > current.TotalSP ||
			current.TotalTP < 0 || current.RemainingTP < 0 || current.RemainingTP > current.TotalTP {
			return dnfrepo.SkillPointState{}, fmt.Errorf("invalid current ledger %+v", current)
		}
		spentSP = current.TotalSP - current.RemainingSP
		spentTP = current.TotalTP - current.RemainingTP
	}
	if spentSP > target.TotalSP || spentTP > target.TotalTP {
		return dnfrepo.SkillPointState{}, fmt.Errorf("spent points exceed target: spent_sp=%d spent_tp=%d target=%+v", spentSP, spentTP, target)
	}
	target.RemainingSP = target.TotalSP - spentSP
	target.RemainingTP = target.TotalTP - spentTP
	return target, nil
}

func learnedSkillPointBookCosts(catalog *dnfskill.Table, job byte, initial map[uint16]int, states map[int64]dnfrepo.SkillState) (int, int, error) {
	if len(states) == 0 {
		return 0, 0, nil
	}
	if catalog == nil || initial == nil {
		return 0, 0, fmt.Errorf("missing active-PVF skill rules for empty ledger")
	}
	spentSP := 0
	spentTP := 0
	for rawID, state := range states {
		if state.Level <= 0 {
			continue
		}
		if rawID < 0 || rawID > math.MaxUint16 {
			return 0, 0, fmt.Errorf("invalid skill id %d", rawID)
		}
		skillID := uint16(rawID)
		floor := initial[skillID]
		if state.Level < floor {
			return 0, 0, fmt.Errorf("skill=%d level=%d floor=%d", skillID, state.Level, floor)
		}
		definition, found := catalog.Find(job, skillID)
		if !found {
			if state.Level == floor {
				continue
			}
			return 0, 0, fmt.Errorf("skill=%d is absent from job=%d active PVF", skillID, job)
		}
		cost := 0
		for level := floor; level < state.Level; level++ {
			levelCost := definition.LevelCost(level)
			if levelCost < 0 || cost > math.MaxUint16-levelCost {
				return 0, 0, fmt.Errorf("skill=%d level=%d invalid cost=%d", skillID, level, levelCost)
			}
			cost += levelCost
		}
		if definition.IsTPSkill() {
			spentTP += cost
		} else {
			spentSP += cost
		}
	}
	return spentSP, spentTP, nil
}

func addSkillPointBookPoints(skill *dnfrepo.SkillRecord, spDelta, tpDelta int) error {
	if skill == nil || spDelta < 0 || tpDelta < 0 || (spDelta == 0 && tpDelta == 0) {
		return ErrTPBookContract
	}
	add := func(points *dnfrepo.SkillPointState) error {
		if points.TotalSP > math.MaxUint16-spDelta || points.RemainingSP > math.MaxUint16-spDelta ||
			points.TotalTP > math.MaxUint16-tpDelta || points.RemainingTP > math.MaxUint16-tpDelta {
			return fmt.Errorf("%w: SP/TP overflow ledger=%+v sp_delta=%d tp_delta=%d", ErrTPBookContract, *points, spDelta, tpDelta)
		}
		points.TotalSP += spDelta
		points.RemainingSP += spDelta
		points.TotalTP += tpDelta
		points.RemainingTP += tpDelta
		return nil
	}
	if err := add(&skill.Points); err != nil {
		return err
	}
	for treeIndex, tree := range skill.Trees {
		if err := add(&tree.Points); err != nil {
			return fmt.Errorf("%w: alternate tree=%d: %v", ErrTPBookContract, treeIndex, err)
		}
		skill.Trees[treeIndex] = tree
	}
	return nil
}

type tpBookResetTree struct {
	Skills map[int64]dnfrepo.SkillState
	Points dnfrepo.SkillPointState
	Layout dnfrepo.SkillLayout
}

func resetTPBookTree(
	states map[int64]dnfrepo.SkillState,
	layout dnfrepo.SkillLayout,
	points dnfrepo.SkillPointState,
	catalog *dnfskill.Table,
	job byte,
	initial map[uint16]int,
) (tpBookResetTree, error) {
	reset := tpBookResetTree{
		Skills: make(map[int64]dnfrepo.SkillState, len(states)),
		Points: points,
		Layout: make(dnfrepo.SkillLayout, len(layout)),
	}
	for id, state := range states {
		reset.Skills[id] = state
	}
	for slot, id := range layout {
		reset.Layout[slot] = id
	}
	for rawID, state := range reset.Skills {
		if state.Level <= 0 {
			continue
		}
		if rawID < 0 || rawID > math.MaxUint16 {
			return tpBookResetTree{}, fmt.Errorf("%w: invalid skill id=%d", ErrTPBookContract, rawID)
		}
		skillID := uint16(rawID)
		definition, found := catalog.Find(job, skillID)
		if !found || !definition.IsTPSkill() {
			continue
		}
		floor := initial[skillID]
		if state.Level <= floor {
			continue
		}
		if floor == 0 {
			delete(reset.Skills, rawID)
			for slot, layoutID := range reset.Layout {
				if layoutID == skillID {
					delete(reset.Layout, slot)
				}
			}
		} else {
			reset.Skills[rawID] = dnfrepo.SkillState{Level: floor, Enabled: true}
		}
	}
	reset.Points.RemainingTP = reset.Points.TotalTP
	return reset, nil
}

func skillPointBookBaseline(skill dnfrepo.SkillRecord, baseline *dnfrepo.SkillPointState) dnfrepo.SkillPointState {
	if baseline != nil {
		return *baseline
	}
	return skill.Points
}

func skillPointBookSPDelta(effect alignedcmd.TPBookResolution) int {
	if effect.Kind == alignedcmd.SkillPointBookAddSP {
		return int(effect.Delta)
	}
	return 0
}

func skillPointBookTPDelta(effect alignedcmd.TPBookResolution) int {
	if effect.Kind == alignedcmd.SkillPointBookAddTP {
		return int(effect.Delta)
	}
	return 0
}

func skillPointBookLegacyTPDelta(effect alignedcmd.TPBookResolution) int {
	return skillPointBookTPDelta(effect)
}

func normalizeTPBookStackableType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
}
