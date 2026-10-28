package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// alignedExperienceConsumableResolverForCommand resolves every current-PVF
// [waste] or [unlimited waste] item that actually owns [exp bonus rate]. This
// intentionally does not classify by name or item id, so event/legacy elixirs
// with the same PVF effect continue to work while unrelated items remain
// ordinary. [unlimited waste] is activated without decrementing its stack.
func (s *Service) alignedExperienceConsumableResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.ExperienceConsumableResolver, error) {
	if opcode != dnfenum.CmdPacketUseStackable {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return func(itemID int64) (alignedcmd.ExperienceConsumableResolution, error) {
		if itemID <= 0 {
			return alignedcmd.ExperienceConsumableResolution{}, nil
		}
		definition, resolveErr := catalog.ResolveItem(uint32(itemID))
		if resolveErr != nil {
			return alignedcmd.ExperienceConsumableResolution{}, resolveErr
		}
		normalizedType := normalizeDungeonDropStackableType(definition.StackableType)
		if definition.Kind != dungeonDropItemStackable || (normalizedType != "[waste]" && normalizedType != "[unlimited waste]") ||
			definition.ExpBonusPercent <= 0 || definition.EffectDurationMS <= 0 {
			return alignedcmd.ExperienceConsumableResolution{}, nil
		}
		seconds := definition.EffectDurationMS / 1000
		if seconds <= 0 {
			return alignedcmd.ExperienceConsumableResolution{}, fmt.Errorf("experience consumable duration is below one second: item=%d ms=%d", itemID, definition.EffectDurationMS)
		}
		sourceKey := definition.ExperienceSourceKey
		if sourceKey == 0 {
			sourceKey, _ = currentExperienceConsumableCanonicalSourceKey(definition.ExpBonusPercent)
		}
		return alignedcmd.ExperienceConsumableResolution{
			ItemID:              itemID,
			BonusPercent:        definition.ExpBonusPercent,
			DurationSeconds:     seconds,
			ExperienceSourceKey: sourceKey,
			PVFPath:             definition.PVFPath,
			Reusable:            normalizedType == "[unlimited waste]",
		}, nil
	}, nil
}
