package dnfbridge

import "fmt"

const (
	currentCeraPackageGraphMaxDepth = 8
	currentCeraPackageGraphMaxNodes = 256
	currentCeraPackageGraphMaxRows  = 512
)

type currentCeraPackageGraphBudget struct {
	nodes int
	rows  int
}

// validateCurrentCeraPackageRewardGraph proves that the deterministic Cera
// package subgraph is finite and bounded before a source item is consumed.
// Fixed rows are additive; repeated selection groups contribute the largest
// possible branch, while every candidate is still visited for cycle and shape
// validation. Random/selection booster boxes are deliberate op160 boundaries
// and stay as one terminal item here, preserving their native roll/choice.
func validateCurrentCeraPackageRewardGraph(catalog *pvfDungeonDropCatalog, sourceItemID uint32) error {
	if catalog == nil || catalog.source == nil || sourceItemID == 0 {
		return errCurrentCeraPackagePVFInvalid
	}
	budget := &currentCeraPackageGraphBudget{}
	units, err := currentCeraPackageExpandedUnits(catalog, sourceItemID, 1, 0, make(map[uint32]struct{}), budget)
	if err != nil {
		return err
	}
	if units == 0 || units > currentCeraPackageMaxRewardAmount {
		return fmt.Errorf("%w: source item=%d expanded_units=%d", errCurrentCeraPackagePVFInvalid, sourceItemID, units)
	}
	return nil
}

func currentCeraPackageExpandedUnits(
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	count uint64,
	depth int,
	path map[uint32]struct{},
	budget *currentCeraPackageGraphBudget,
) (uint64, error) {
	if itemID == 0 || count == 0 || count > currentCeraPackageMaxRewardAmount || budget == nil {
		return 0, errCurrentCeraPackagePVFInvalid
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return 0, fmt.Errorf("%w: graph reward item=%d: %v", errCurrentCeraPackagePVFInvalid, itemID, err)
	}
	packageDefinition, recognized, err := resolveCurrentCeraPackageDefinitionForSource(catalog, definition)
	if err != nil {
		return 0, err
	}
	if !recognized {
		// Every non-Cera item, including [booster], [booster selection],
		// [cera booster] and legacy random boxes, is granted as one terminal
		// row. Its independent use protocol owns any later random or choice.
		return count, nil
	}
	if depth >= currentCeraPackageGraphMaxDepth {
		return 0, fmt.Errorf("%w: package graph depth exceeds %d at item=%d", errCurrentCeraPackagePVFInvalid, currentCeraPackageGraphMaxDepth, itemID)
	}
	if _, cycle := path[itemID]; cycle {
		return 0, fmt.Errorf("%w: package graph cycle at item=%d", errCurrentCeraPackagePVFInvalid, itemID)
	}
	budget.nodes++
	if budget.nodes > currentCeraPackageGraphMaxNodes {
		return 0, fmt.Errorf("%w: package graph nodes exceed %d", errCurrentCeraPackagePVFInvalid, currentCeraPackageGraphMaxNodes)
	}
	rows := len(packageDefinition.Rewards)
	for _, group := range packageDefinition.SelectionGroups {
		rows += len(group)
	}
	budget.rows += rows
	if budget.rows > currentCeraPackageGraphMaxRows {
		return 0, fmt.Errorf("%w: package graph rows exceed %d", errCurrentCeraPackagePVFInvalid, currentCeraPackageGraphMaxRows)
	}

	nextPath := make(map[uint32]struct{}, len(path)+1)
	for parent := range path {
		nextPath[parent] = struct{}{}
	}
	nextPath[itemID] = struct{}{}
	var perPackage uint64
	addReward := func(reward currentCeraPackageReward) (uint64, error) {
		multiplier, ok := currentCeraPackageBoundedProduct(count, uint64(reward.Count))
		if !ok {
			return 0, fmt.Errorf("%w: package graph item=%d reward=%d count overflow", errCurrentCeraPackagePVFInvalid, itemID, reward.Definition.ItemID)
		}
		return currentCeraPackageExpandedUnits(catalog, reward.Definition.ItemID, multiplier, depth+1, nextPath, budget)
	}
	for _, reward := range packageDefinition.Rewards {
		units, rewardErr := addReward(reward)
		if rewardErr != nil {
			return 0, rewardErr
		}
		var ok bool
		perPackage, ok = currentCeraPackageBoundedSum(perPackage, units)
		if !ok {
			return 0, fmt.Errorf("%w: package graph source=%d fixed rewards expand beyond limit", errCurrentCeraPackagePVFInvalid, itemID)
		}
	}
	for groupIndex, group := range packageDefinition.SelectionGroups {
		if len(group) == 0 {
			return 0, fmt.Errorf("%w: package graph source=%d group=%d is empty", errCurrentCeraPackagePVFInvalid, itemID, groupIndex)
		}
		var largest uint64
		for _, candidate := range group {
			units, candidateErr := addReward(candidate)
			if candidateErr != nil {
				return 0, candidateErr
			}
			if units > largest {
				largest = units
			}
		}
		var ok bool
		perPackage, ok = currentCeraPackageBoundedSum(perPackage, largest)
		if !ok {
			return 0, fmt.Errorf("%w: package graph source=%d selected rewards expand beyond limit", errCurrentCeraPackagePVFInvalid, itemID)
		}
	}
	return perPackage, nil
}

func currentCeraPackageBoundedProduct(left, right uint64) (uint64, bool) {
	if left == 0 || right == 0 || left > currentCeraPackageMaxRewardAmount || right > currentCeraPackageMaxRewardAmount ||
		left > currentCeraPackageMaxRewardAmount/right {
		return 0, false
	}
	return left * right, true
}

func currentCeraPackageBoundedSum(left, right uint64) (uint64, bool) {
	if left > currentCeraPackageMaxRewardAmount || right > currentCeraPackageMaxRewardAmount ||
		left > currentCeraPackageMaxRewardAmount-right {
		return 0, false
	}
	return left + right, true
}
