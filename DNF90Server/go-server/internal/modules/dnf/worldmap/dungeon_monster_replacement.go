package worldmap

// ReplaceMonster resolves a configured placeholder in one private run layout.
// Coordinates, rank, drops and all other PVF fields stay with the map spawn.
// Neither the shared resolver nor another player's topology is mutated.
func (t *DungeonTopology) ReplaceMonster(dummy, monster, level int64) int {
	if t == nil || dummy <= 0 || monster <= 0 || level <= 0 {
		return 0
	}
	count := 0
	for key, room := range t.rooms {
		if room.Map == nil {
			continue
		}
		prepared := cloneDungeonRoom(room)
		for i := range prepared.Map.Map.Monsters {
			spawn := &prepared.Map.Map.Monsters[i]
			if spawn.MonsterID == dummy {
				spawn.MonsterID = monster
				spawn.Level = level
				spawn.AutoLevel = 0
				count++
			}
		}
		t.rooms[key] = prepared
	}
	return count
}
