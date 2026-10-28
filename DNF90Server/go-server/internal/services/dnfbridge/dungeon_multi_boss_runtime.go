package dnfbridge

import "longheng.io/server/internal/modules/dnf/worldmap"

// currentDungeonPVFMultiBossGate preserves separately authored ordinary boss
// actors in one room.  Current Script.pvf uses this shape in Mechanical
// Throne: the self-retiring Luke fixture and the quest-drop Machine Golem are
// both blocking [boss] monster rows.  One actor's death must never fabricate
// the other's death or bypass its own drop owner.
type currentDungeonPVFMultiBossGate struct {
	BossObjectKeys []uint32
	Ready          bool
}

func currentDungeonPVFMultiBossDeathGate(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) (currentDungeonPVFMultiBossGate, bool) {
	if runtime == nil || runtime.Room == nil {
		return currentDungeonPVFMultiBossGate{}, false
	}
	room := runtime.Room.Snapshot()
	if room.Coordinate != scene.Coordinate || room.MapID != scene.Map.Map.ID {
		return currentDungeonPVFMultiBossGate{}, false
	}

	blocking := make(map[worldmap.HostileReference]struct{}, len(scene.BlockingHostiles))
	for _, reference := range scene.BlockingHostiles {
		blocking[reference] = struct{}{}
	}
	defeated := make(map[uint32]struct{}, len(scene.DefeatedObjects))
	for _, objectKey := range scene.DefeatedObjects {
		defeated[objectKey] = struct{}{}
	}

	gate := currentDungeonPVFMultiBossGate{Ready: true}
	for _, monster := range room.Monsters {
		if !currentDungeonPVFMonsterRowIsBoss(monster) {
			continue
		}
		if _, ok := blocking[monster.Reference]; !ok {
			continue
		}
		gate.BossObjectKeys = append(gate.BossObjectKeys, monster.ObjectKey)
		if monster.State != runtimeDungeonMonsterDefeated {
			gate.Ready = false
			continue
		}
		if _, ok := defeated[monster.ObjectKey]; !ok {
			gate.Ready = false
		}
	}
	if len(gate.BossObjectKeys) < 2 {
		return currentDungeonPVFMultiBossGate{}, false
	}
	return gate, true
}

func currentDungeonPVFMonsterRowIsBoss(monster runtimeDungeonMonster) bool {
	return normalizeDungeonPVFSymbol(monster.Spawn.Rank) == "boss" ||
		normalizeDungeonPVFSymbol(monster.Spawn.SuffixMarker) == "boss" ||
		normalizeDungeonPVFSymbol(monster.Definition.Rank) == "boss"
}
