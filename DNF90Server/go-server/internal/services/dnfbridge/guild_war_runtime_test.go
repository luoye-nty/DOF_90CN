package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestGuildWarRealPVFEntry(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("real PVF not selected")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	table, err := worldmap.LoadSource(ctx, archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	ai, err := newPVFDungeonAICharacterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := dnfguild.LoadWarRules(archive)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
	repos := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "99", AccountID: "guild-test", Name: "guild-test", Job: "2", Level: 90, Stats: map[string]int64{"fatigue": 156, "exp": 0}}
	if err = repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err = repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "99"}); err != nil {
		t.Fatal(err)
	}
	r := dnfguild.NewRegistry()
	g, err := r.Create(character, "guild", now)
	if err != nil {
		t.Fatal(err)
	}
	g.ActiveGuardian = 1
	g.Guardians = []dnfguild.Guardian{{ID: 1, Level: 1}}
	enemy, err := r.Create(dnfrepo.CharacterRecord{CharacterID: "100", Name: "enemy"}, "enemy", now)
	if err != nil {
		t.Fatal(err)
	}
	enemy.ActiveGuardian = 1
	enemy.Guardians = []dnfguild.Guardian{{ID: 1, Level: 21, Skills: [5]byte{2, 2, 1}}}
	if _, err = r.AddWarRivals("99", true, rules, now); err != nil {
		t.Fatal(err)
	}
	if _, err = r.StartWar("99", enemy.ID, rules, now); err != nil {
		t.Fatal(err)
	}
	// Entry into the base is tested after a server-confirmed prerequisite clear.
	g.War.Battle.Clears[8515] = 1
	if err = dnfguild.Save(ctx, repos.Settings, r, now); err != nil {
		t.Fatal(err)
	}
	s := &Service{options: options{accountID: "guild-test", gameUpperBodyCodec: gameUpperBodyCodecPlain}, initialEquipmentArchive: archive, pvfItemCatalog: items, worldMapTable: table, worldMapResolver: resolver, dungeonMonsterTable: monsters, dungeonAICharacterTable: ai, dungeonChoice: func(int) (int, error) { return 0, nil }, dungeonSeed: func() (uint32, error) { return 1, nil }, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }, gameplayTimers: &processGameplayTimeQueue{now: func() time.Time { return now }}}
	for _, id := range []uint32{8514, 8515, 8517, 8520} {
		ss := &gameSession{conn: &bufferConn{}, selectedCharacterID: 99}
		runtime, scene, err := s.prepareDungeonRuntime(ctx, ss, dungeoncmd.SelectDungeonRequest{DungeonID: id, Difficulty: 3, RuntimeState: 1, RuntimeToken: 0xffff})
		if err != nil {
			t.Fatalf("dungeon %d: %v", id, err)
		}
		if runtime.guildWarID != g.ID || runtime.guildWarKey != g.War.Battle.Key || scene.Map.Map.ID == 0 {
			t.Fatal("war entry identity")
		}
		t.Logf("war=%d map=%d monsters=%+v summons=%+v passive=%+v", id, scene.Map.Map.ID, scene.Monsters, scene.Summons, scene.SpecialPassiveObjects)
		for _, room := range runtime.Session.Rooms() {
			if room.Map == nil {
				t.Fatal(room.ResolutionError)
			}
			m := room.Map.Map
			t.Logf("war=%d room=%v boss=%t map=%d monsters=%+v summons=%+v passive=%+v", id, room.Coordinate, room.Boss, m.ID, m.Monsters, m.Summons, m.SpecialPassiveObjects)
			if room.Boss && id == 8514 && (len(m.Monsters) != 1 || m.Monsters[0].MonsterID != 71023 || m.Monsters[0].Level != 21) {
				t.Fatal("PVF guardian placeholder was not replaced from the frozen opponent")
			}
		}
		if id != 8517 && id != 8520 {
			continue
		}
		ss.dungeon.runtime = runtime
		if _, err = runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
			t.Fatal(err)
		}
		if err = runtime.cacheCurrentDungeonRoom(); err != nil {
			t.Fatal(err)
		}
		path := [][2]byte{{1, 0}, {2, 0}, {1, 0}, {1, 1}, {1, 0}, {2, 0}, {2, 1}}
		if id == 8520 {
			path = [][2]byte{{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}}
		}
		for _, next := range path {
			ss.conn.(*bufferConn).write.Reset()
			body := make([]byte, dungeoncmd.MoveMapRequestSize)
			body[0], body[1] = next[0], next[1]
			if err = s.handleDungeonMoveMap(ss, body); err != nil {
				t.Fatal(err)
			}
			packet, _ := splitGameServerUpperPacket(t, ss.conn.(*bufferConn).write.Bytes())
			current, _ := runtime.Session.Scene()
			if current.Coordinate.X != int64(next[0]) || current.Coordinate.Y != int64(next[1]) {
				t.Fatalf("war=%d move blocked: %+v", id, current.Coordinate)
			}
			if current.Map.Map.ID == 36629 || current.Map.Map.ID == 36633 {
				if len(current.BlockingHostiles) != 0 || !current.Cleared || (len(packet.Body) > 16 && packet.Body[19+20] != 1) {
					t.Fatalf("invisible cannon blocks door: map=%d blockers=%v packet=%x", current.Map.Map.ID, current.BlockingHostiles, packet.Body)
				}
			}
			if current.Map.Map.ID == 36659 {
				if packet.Header.MsgID != 29 || packet.Body[18] != 8 || len(runtime.Room.Snapshot().ExtendedActors) != 0 {
					t.Fatalf("native guild monster template mismatch: packet=%x", packet.Body)
				}
				extra := 19 + 8*21
				if packet.Body[extra] != 1 || binary.LittleEndian.Uint32(packet.Body[extra+6:]) != 10160846 || runtime.DropOwner == nil {
					t.Fatalf("underpass treasure lost: packet=%x", packet.Body)
				}
			}
			// Defeat only real enemies. The cannon controller never emits op39.
			for _, monster := range runtime.Room.Snapshot().Monsters {
				if monster.Spawn.MonsterID == 63835 {
					continue
				}
				if _, err = runtime.Session.MarkHostileDefeated(monster.ObjectKey); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
