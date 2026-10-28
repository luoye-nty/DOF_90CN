package inventory

import (
	"context"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfweaknesspotion "longheng.io/server/internal/modules/dnf/weaknesspotion"
)

// ApplyWeaknessPotion records the 防虚弱药剂 effect of one consumed item.
//
// The client applies the potion's buff locally and never reports it (live
// capture 2026-09-15 00:43: the only uplink around a use is class-1/op44), so
// the server has to recognise the item itself and remember the effect. The
// effect has two halves, both owned here because both are character state:
//
//   - the current penalty is cleared immediately ("自动解除虚弱状态"), which is
//     the same stamina/weakness_recovery_at pair dnfbridge's forced-return path
//     writes, so the next weakness refresh publishes level 0 on its own;
//   - the immunity window is stored as a durable unix second, because several
//     variants state the effect survives a death, a town return and a relog.
//
// Items outside the family return false without touching the database, so an
// unrelated consumable keeps exactly the behaviour it had before.
func (o *Owner) ApplyWeaknessPotion(
	ctx context.Context,
	characterID string,
	itemID int64,
	now time.Time,
) (bool, error) {
	seconds, inFamily := dnfweaknesspotion.ImmunitySeconds(itemID)
	if !inFamily || o == nil || o.assets == nil || characterID == "" {
		return false, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	until := now.Add(time.Duration(seconds) * time.Second).Unix()
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 4)
		}
		// Drinking a second potion must never shorten the window that is already
		// running (a 60-minute 网吧 potion followed by a 10-minute one).
		if existing := dnfweaknesspotion.ImmuneUntil(character.Stats); existing > until {
			until = existing
		}
		character.Stats[dnfweaknesspotion.ImmuneUntilStatKey] = until
		character.Stats[dnfweaknesspotion.WeaknessLevelStatKey] = 0
		character.Stats[dnfweaknesspotion.WeaknessRecoveryAtStatKey] = 0
		character.UpdatedAt = now
		return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
