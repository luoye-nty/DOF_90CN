package dnfbridge

import (
	dnfcreaturestate "longheng.io/server/internal/modules/dnf/creaturestate"
)

// currentPetEggPVFType is the PVF `[equipment type]` shared by pets and their eggs.
const currentPetEggPVFType = "[creature]"

// installPetEggProbeOnce hands the pet domain the runtime-PVF egg predicate.
// It runs on the first aligned command the service routes, which covers every
// path that reconciles the pet record (op19 moves, enchants) and the op18 item
// delete. Until it runs, creaturestate keeps its pre-existing behaviour.
func (s *Service) installPetEggProbeOnce() {
	if s == nil {
		return
	}
	s.petEggProbeOnce.Do(func() {
		dnfcreaturestate.SetPetEggProbe(func(itemID int64) bool {
			return s.currentIsPetEggItem(itemID)
		})
	})
}

// currentIsPetEggItem proves that an item template is a pet EGG rather than a
// hatched creature.
//
// The only authority is the runtime PVF: an egg carries an `[output index]`
// section naming the creature it hatches into, a real pet does not. File names
// are deliberately NOT used -- scanning the live Script.pvf, 359
// `equipment/creature/*.equ` documents have `[output index]`, and many of them
// are named `100980042.equ` / `15_petitsandman/63483.equ`, while 6 documents
// named `egg_*.equ` have no output index at all.
//
// The catalog it reads is the same one every op19 command already warms, so this
// adds no archive scan. Results are cached per template.
func (s *Service) currentIsPetEggItem(itemID int64) bool {
	if s == nil || itemID <= 0 || itemID > int64(^uint32(0)) {
		return false
	}
	if cached, ok := s.petEggCache.Load(itemID); ok {
		value, _ := cached.(bool)
		return value
	}
	value := s.resolvePetEggItem(uint32(itemID))
	s.petEggCache.Store(itemID, value)
	return value
}

func (s *Service) resolvePetEggItem(itemID uint32) bool {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemEquipment {
		return false
	}
	doc, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil || doc == nil {
		return false
	}
	equipmentType, ok := doc.Text("equipment type")
	if !ok {
		return false
	}
	// 直接比较归一化后的 PVF 类型：currentEquipmentPlacementRuleForPVFType 只覆盖
	// 可穿戴槽位的类型表，其中没有 `[creature]`，用它会把所有宠物/蛋都判成非蛋。
	if normalizeEquipmentPlacementPVFType(equipmentType) != currentPetEggPVFType {
		return false
	}
	_, hasOutput := doc.Section("output index")
	return hasOutput
}
