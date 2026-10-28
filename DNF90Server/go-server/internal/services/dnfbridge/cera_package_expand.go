package dnfbridge

import (
	"fmt"
	"math"
)

// expandCurrentDeterministicNestedCeraPackages follows historical
// [cera package] rewards which are themselves only transport wrappers. These
// wrappers are normally expanded by Cera-shop checkout and are not usable
// inventory boxes. Expanding their deterministic fixed/singleton contents at
// the owning package boundary prevents a player from receiving an inert inner
// box. Random boosters, booster selections, [usable cera package] items,
// packages with alternative selections, and packages containing avatars stay
// as terminal inventory items so their own current-EXE UI/protocol remains the
// authority.
func expandCurrentDeterministicNestedCeraPackages(
	catalog *pvfDungeonDropCatalog,
	rewards []currentCeraPackageReward,
) ([]currentCeraPackageReward, error) {
	if catalog == nil || catalog.source == nil {
		return nil, errCurrentCeraPackagePVFInvalid
	}
	result := make([]currentCeraPackageReward, 0, len(rewards))
	var total uint64
	var walk func(currentCeraPackageReward, int, map[uint32]struct{}) error
	walk = func(reward currentCeraPackageReward, depth int, path map[uint32]struct{}) error {
		if reward.Definition.ItemID == 0 || reward.Count == 0 {
			return errCurrentCeraPackagePVFInvalid
		}
		definition, recognized, err := resolveCurrentCeraPackageDefinitionForSource(catalog, reward.Definition)
		if err != nil {
			return err
		}
		deterministic := recognized && definition.StackableType == "[cera package]" && len(definition.AvatarItemIDs) == 0
		if deterministic {
			for _, group := range definition.SelectionGroups {
				if len(group) != 1 {
					deterministic = false
					break
				}
			}
		}
		if !deterministic {
			if len(result) >= currentCeraPackageMaxRewardRows {
				return fmt.Errorf("%w: expanded reward rows exceed %d", errCurrentCeraPackagePVFInvalid, currentCeraPackageMaxRewardRows)
			}
			total += uint64(reward.Count)
			if total > currentCeraPackageMaxRewardAmount {
				return fmt.Errorf("%w: expanded reward total=%d", errCurrentCeraPackagePVFInvalid, total)
			}
			result = append(result, reward)
			return nil
		}
		if depth >= currentCeraPackageGraphMaxDepth {
			return fmt.Errorf("%w: nested package depth exceeds %d at item=%d", errCurrentCeraPackagePVFInvalid, currentCeraPackageGraphMaxDepth, reward.Definition.ItemID)
		}
		if _, cycle := path[reward.Definition.ItemID]; cycle {
			return fmt.Errorf("%w: nested package cycle at item=%d", errCurrentCeraPackagePVFInvalid, reward.Definition.ItemID)
		}
		nextPath := make(map[uint32]struct{}, len(path)+1)
		for itemID := range path {
			nextPath[itemID] = struct{}{}
		}
		nextPath[reward.Definition.ItemID] = struct{}{}
		children := append([]currentCeraPackageReward(nil), definition.Rewards...)
		for _, group := range definition.SelectionGroups {
			children = append(children, group[0])
		}
		for _, child := range children {
			count := uint64(reward.Count) * uint64(child.Count)
			if count == 0 || count > math.MaxUint32 || count > currentCeraPackageMaxRewardAmount {
				return fmt.Errorf("%w: nested package item=%d reward=%d count=%d", errCurrentCeraPackagePVFInvalid, reward.Definition.ItemID, child.Definition.ItemID, count)
			}
			child.Count = uint32(count)
			if err := walk(child, depth+1, nextPath); err != nil {
				return err
			}
		}
		return nil
	}
	for _, reward := range rewards {
		if err := walk(reward, 0, nil); err != nil {
			return nil, err
		}
	}
	return result, nil
}
