package dnfbridge

const (
	currentDungeonAreaMaterialWeight uint32 = 100
	currentDungeonBirthBudItemID     uint32 = 10161610
)

type currentDungeonAreaMaterialDefinition struct {
	FirstDungeonID int64
	LastDungeonID  int64
	ItemID         uint32
}

// currentDungeonAreaMaterialDefinitions owns the compatibility mapping that
// the PVF does not encode in monster [item], Independent_Drop.etc or
// WorldDrop.etc. The active PVF's birth-bud item explicitly names Magic World
// regional dungeons as its source, while the selectable Metro Center dungeon
// list bounds that ordinary source to 310..315. Dungeon 316 is the dedicated
// particular-Hell map and 317 is a quest-only forest; neither belongs to this
// ordinary regional-material range.
var currentDungeonAreaMaterialDefinitions = [...]currentDungeonAreaMaterialDefinition{
	{FirstDungeonID: 310, LastDungeonID: 315, ItemID: currentDungeonBirthBudItemID},
}

func currentDungeonAreaMaterialPoolEntry(dungeonID int64) (dungeonMonsterDropPoolEntry, bool) {
	for _, definition := range currentDungeonAreaMaterialDefinitions {
		if dungeonID < definition.FirstDungeonID || dungeonID > definition.LastDungeonID {
			continue
		}
		return dungeonMonsterDropPoolEntry{
			ItemID: definition.ItemID,
			Weight: currentDungeonAreaMaterialWeight,
		}, true
	}
	return dungeonMonsterDropPoolEntry{}, false
}
