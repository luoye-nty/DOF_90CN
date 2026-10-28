package worldmap

// compactMapForRuntime removes parser evidence after all runtime fields have
// been materialized. The typed map state remains unchanged.
func compactMapForRuntime(value *Map) {
	if value == nil {
		return
	}
	value.SourceSections = nil
	value.OpaqueSections = nil
	value.UnknownSections = nil
	value.Extensions = nil
	for index := range value.Summons {
		value.Summons[index].SourceSections = nil
		value.Summons[index].UnknownSections = nil
		value.Summons[index].Extensions = nil
	}
}

func compactAreaForRuntime(value *Area) {
	if value == nil {
		return
	}
	value.SourceSections = nil
	value.OpaqueSections = nil
	value.UnknownSections = nil
	value.Extensions = nil
}

func compactDungeonForRuntime(value *Dungeon) {
	if value == nil {
		return
	}
	value.SourceSections = nil
	value.OpaqueSections = nil
	value.UnknownSections = nil
	value.Extensions = nil
	value.Metadata.WorldMapPatternInfo = nil
	value.Metadata.WorldMapInfo = nil
	for mazeIndex := range value.Mazes {
		maze := &value.Mazes[mazeIndex]
		maze.BossMapSpecifications = nil
		maze.LayeredMapSpecifications = nil
		maze.SourceSections = nil
		maze.OpaqueSections = nil
		maze.UnknownSections = nil
		maze.Extensions = nil
		for scriptIndex := range maze.RandomizedObjects {
			script := &maze.RandomizedObjects[scriptIndex]
			script.SourceSections = nil
			script.UnknownSections = nil
			script.Extensions = nil
			for objectIndex := range script.Objects {
				object := &script.Objects[objectIndex]
				object.SourceSections = nil
				object.UnknownSections = nil
				object.Extensions = nil
			}
		}
	}
}
