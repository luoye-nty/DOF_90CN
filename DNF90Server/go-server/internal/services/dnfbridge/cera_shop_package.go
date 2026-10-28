package dnfbridge

import (
	"fmt"
	"math"
)

// currentCeraShopAutoOpenReward is one reward selected from a Cera-shop
// [cera package].  Unlike [usable cera package], this package is expanded by
// op64 checkout and never enters a player container as a source box.
type currentCeraShopAutoOpenReward struct {
	Definition dungeonDropItemDefinition
	Count      uint32
	Avatar     bool
	Option     byte
}

type currentCeraShopAutoOpenPackageDefinition struct {
	Source          dungeonDropItemDefinition
	FixedRewards    []currentCeraShopAutoOpenReward
	SelectionGroups [][]currentCeraShopAutoOpenReward
}

func resolveCurrentCeraShopAutoOpenRewards(
	catalog *pvfCeraShopCatalog,
	source dungeonDropItemDefinition,
	productCount uint32,
	avatarChoices []currentCeraPackageChoice,
	selections []currentCeraShopPackageSelection,
) ([]currentCeraShopAutoOpenReward, bool, error) {
	if catalog == nil || catalog.items == nil || catalog.source == nil || source.ItemID == 0 || productCount == 0 {
		return nil, false, errCurrentCeraShopProductUnavailable
	}
	if source.Kind != dungeonDropItemStackable {
		return nil, false, nil
	}
	parsed, recognized, err := resolveCurrentCeraPackageDefinitionForSource(catalog.items, source)
	if err != nil {
		return nil, true, fmt.Errorf("%w: cera package item=%d PVF=%s: %v", errCurrentCeraShopProductUnavailable, source.ItemID, source.PVFPath, err)
	}
	if !recognized || parsed.StackableType != "[cera package]" {
		return nil, false, nil
	}
	if err := validateCurrentCeraPackageRewardGraph(catalog.items, source.ItemID); err != nil {
		return nil, true, fmt.Errorf("%w: cera package item=%d graph: %v", errCurrentCeraShopProductUnavailable, source.ItemID, err)
	}
	convert := func(rewards []currentCeraPackageReward) []currentCeraShopAutoOpenReward {
		converted := make([]currentCeraShopAutoOpenReward, 0, len(rewards))
		for _, reward := range rewards {
			converted = append(converted, currentCeraShopAutoOpenReward{
				Definition: reward.Definition,
				Count:      reward.Count,
				Avatar:     reward.Avatar,
			})
		}
		return converted
	}
	definition := currentCeraShopAutoOpenPackageDefinition{
		Source:       source,
		FixedRewards: convert(parsed.Rewards),
	}
	for _, group := range parsed.SelectionGroups {
		definition.SelectionGroups = append(definition.SelectionGroups, convert(group))
	}

	if len(selections) != len(definition.SelectionGroups) {
		return nil, true, fmt.Errorf("%w: cera package item=%d selections=%d groups=%d", errCurrentCeraShopProductUnavailable, source.ItemID, len(selections), len(definition.SelectionGroups))
	}
	selectedByGroup := make(map[uint16]currentCeraShopPackageSelection, len(selections))
	for _, selection := range selections {
		if selection.PackageItemID != source.ItemID || int(selection.GroupIndex) >= len(definition.SelectionGroups) {
			return nil, true, fmt.Errorf("%w: cera package item=%d selection package=%d group=%d", errCurrentCeraShopProductUnavailable, source.ItemID, selection.PackageItemID, selection.GroupIndex)
		}
		if _, duplicate := selectedByGroup[selection.GroupIndex]; duplicate {
			return nil, true, fmt.Errorf("%w: cera package item=%d duplicate group=%d", errCurrentCeraShopProductUnavailable, source.ItemID, selection.GroupIndex)
		}
		group := definition.SelectionGroups[selection.GroupIndex]
		if int(selection.ChoiceIndex) >= len(group) {
			return nil, true, fmt.Errorf("%w: cera package item=%d group=%d choice=%d options=%d", errCurrentCeraShopProductUnavailable, source.ItemID, selection.GroupIndex, selection.ChoiceIndex, len(group))
		}
		selectedByGroup[selection.GroupIndex] = selection
	}

	rewards := append([]currentCeraShopAutoOpenReward(nil), definition.FixedRewards...)
	for groupIndex, group := range definition.SelectionGroups {
		selection, ok := selectedByGroup[uint16(groupIndex)]
		if !ok {
			return nil, true, fmt.Errorf("%w: cera package item=%d missing group=%d", errCurrentCeraShopProductUnavailable, source.ItemID, groupIndex)
		}
		rewards = append(rewards, group[selection.ChoiceIndex])
	}

	avatarItemIDs := make([]uint32, 0)
	seenAvatarItemIDs := make(map[uint32]struct{})
	for _, reward := range rewards {
		if !reward.Avatar {
			continue
		}
		if _, duplicate := seenAvatarItemIDs[reward.Definition.ItemID]; duplicate {
			continue
		}
		seenAvatarItemIDs[reward.Definition.ItemID] = struct{}{}
		avatarItemIDs = append(avatarItemIDs, reward.Definition.ItemID)
	}
	options, err := validateCurrentCeraPackageChoices(currentCeraPackageDefinition{AvatarItemIDs: avatarItemIDs}, avatarChoices)
	if err != nil {
		return nil, true, fmt.Errorf("%w: cera package item=%d avatar choices: %v", errCurrentCeraShopProductUnavailable, source.ItemID, err)
	}
	sharedRewards := make([]currentCeraPackageReward, 0, len(rewards))
	for _, reward := range rewards {
		sharedRewards = append(sharedRewards, currentCeraPackageReward{
			Definition: reward.Definition,
			Count:      reward.Count,
			Avatar:     reward.Avatar,
		})
	}
	sharedRewards, err = expandCurrentDeterministicNestedCeraPackages(catalog.items, sharedRewards)
	if err != nil {
		return nil, true, fmt.Errorf("%w: cera package item=%d nested rewards: %v", errCurrentCeraShopProductUnavailable, source.ItemID, err)
	}
	rewards = convert(sharedRewards)

	var total uint64
	for index := range rewards {
		count := uint64(rewards[index].Count) * uint64(productCount)
		if count == 0 || count > math.MaxUint32 {
			return nil, true, fmt.Errorf("%w: cera package item=%d reward=%d count=%d", errCurrentCeraShopProductUnavailable, source.ItemID, rewards[index].Definition.ItemID, count)
		}
		total += count
		if total > currentCeraPackageMaxRewardAmount {
			return nil, true, fmt.Errorf("%w: cera package item=%d reward_total=%d", errCurrentCeraShopProductUnavailable, source.ItemID, total)
		}
		rewards[index].Count = uint32(count)
		if rewards[index].Avatar {
			rewards[index].Option = options[rewards[index].Definition.ItemID]
		}
	}
	return rewards, true, nil
}
