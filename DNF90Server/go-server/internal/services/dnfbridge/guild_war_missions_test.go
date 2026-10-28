package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func warMissionSource() dungeonDropCatalogTestSource {
	s := warOperationSource()
	s["etc/agitwar.etc"] += "\n[agitwar day]\n0 1 2 3 4 5 6\n[/agitwar day]\n[war limit time]\n`5:30` `6:00`\n[play time]\n900\n[daily max challenge]\n5\n[hp]\n2\n[enemy hp]\n100\n[enemy agit]\n[index]\n8514\n[clear damage]\n10\n[required clear dungeon]\n8515\n[/required clear dungeon]\n[/enemy agit]\n[enemy barricade dungeon]\n8515\n[/enemy barricade dungeon]\n[challenge chance]\n0 100 200 300 400 500\n[/challenge chance]\n[mission system]\n[daily title mission index]\n1\n[season title mission index]\n10\n"
	for _, v := range []struct {
		id                  int
		category, condition string
		required            int
	}{{1, "daily", "mission", 1}, {2, "daily", "join", 1}, {10, "season", "mission", 1}, {11, "season", "clear", 2}} {
		s["etc/agitwar.etc"] += fmt.Sprintf("[mission]\n[index]\n%d\n[category]\n`%s`\n[title]\n`mission %d`\n[condition]\n`%s` %d\n[reward]\n10161523 1\n[/reward]\n[/mission]\n", v.id, v.category, v.id, v.condition, v.required)
	}
	s["etc/agitwar.etc"] += "[/mission system]"
	return s
}

func TestGuildWarRealMissionsAndRules(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("real PVF not selected")
	}
	a, e := platformpvf.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	c, e := dnfguild.LoadWarMissionsCatalog(a)
	if e != nil {
		t.Fatal(e)
	}
	r, e := dnfguild.LoadWarRules(a)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Missions) != 30 || c.DailyTitle != 1 || c.SeasonTitle != 10 || c.Missions[0].Required != 8 || r.DailyMax != 5 || len(r.Dungeons) != 11 || r.EnemyDungeon != 8514 {
		t.Fatal(c, r)
	}
	items, e := newPVFDungeonDropCatalog(a)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range c.Missions {
		for _, v := range m.Rewards {
			def, e := items.ResolveItem(v.ItemID)
			if e != nil || def.Kind != dungeonDropItemStackable {
				t.Fatal(v, e)
			}
		}
	}
	b := buildCurrentGuildWarMissionsBody(c, dnfguild.WarMissionState{})
	if len(b) != 278 || binary.LittleEndian.Uint32(b) != 9 || binary.LittleEndian.Uint32(b[85:]) != 21 {
		t.Fatalf("native task packet %x", b)
	}
	if r.FriendlyHP != 2 || r.PauseStart != 330 || r.PauseEnd != 360 || r.PlaySeconds != 900 {
		t.Fatal("PVF schedule and health", r)
	}
	guardians, e := dnfguild.LoadWarGuardians(a)
	if e != nil {
		t.Fatal(e)
	}
	if id, e := guardians.Monster(dnfguild.Guardian{ID: 1, Level: 21}); e != nil || id != 71023 {
		t.Fatal("PVF guardian star", id, e)
	}

}

func TestGuildWarMissionsRejectMissingCategory(t *testing.T) {
	s := warMissionSource()
	s["etc/agitwar.etc"] = strings.Replace(s["etc/agitwar.etc"], "[category]\n`daily`\n", "", 1)
	if _, e := dnfguild.LoadWarMissionsCatalog(s); e == nil {
		t.Fatal("missing category accepted")
	}
}

func TestGuildWarMissionClaimAtomicAndReplay(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			fail := &guildFailSettlement{base: repos.CharacterSettlement}
			repos.CharacterSettlement = fail
			c := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "master", Level: 90, Stats: map[string]int64{"gold": 100}}
			if e := repos.Character.Save(ctx, c); e != nil {
				t.Fatal(e)
			}
			if e := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "16", Slots: map[string]dnfrepo.ItemStack{}}); e != nil {
				t.Fatal(e)
			}
			r := dnfguild.NewRegistry()
			g, e := r.Create(c, "guild", serviceTestNow())
			if e != nil {
				t.Fatal(e)
			}
			g.ActiveGuardian = 1
			if e = dnfguild.Save(ctx, repos.Settings, r, serviceTestNow()); e != nil {
				t.Fatal(e)
			}
			items, e := newPVFDungeonDropCatalog(warMissionSource())
			if e != nil {
				t.Fatal(e)
			}
			s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, pvfItemCatalog: items, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
			ss := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: c.AccountID, initialTownRouteCharacterID: 16, initialTownRouteStage: currentInitialTownRoutePlayerStateSent}
			send := func(id uint32, want byte) []byte {
				t.Helper()
				ss.conn.(*bufferConn).write.Reset()
				b := make([]byte, 4)
				binary.LittleEndian.PutUint32(b, id)
				h := guildGameplayModule().UpperHandlers[1346]
				if legacy {
					h = guildGameplayModule().LegacyHandlers[1346]
				}
				if e := h(s, ss, gameplayRequest{Opcode: 1346, Classification: 1, Body: b}); e != nil {
					t.Fatal(e)
				}
				ack, rest := splitGameServerUpperPacket(t, ss.conn.(*bufferConn).write.Bytes())
				v := []byte{1}
				if want != 0 {
					v = []byte{0, want}
				}
				if ack.Header.MsgID != 1346 || !bytes.Equal(ack.Body, v) {
					t.Fatalf("ack %x want %x", ack.Body, v)
				}
				return rest
			}
			send(2, 251)
			catalog, e := s.currentGuildWarMissionsCatalog()
			if e != nil {
				t.Fatal(e)
			}
			var state dnfguild.WarMissionState
			now := s.gameplayNow()
			if e = state.Observe(catalog, "join", 8515, "battle", now, now); e != nil {
				t.Fatal(e)
			}
			if e = dnfguild.SaveWarMissions(ctx, repos.Settings, "16", state, now); e != nil {
				t.Fatal(e)
			}
			before, _, e := repos.Inventory.Load(ctx, "16")
			if e != nil {
				t.Fatal(e)
			}
			fail.fail = true
			send(2, 253)
			fail.fail = false
			after, _, e := repos.Inventory.Load(ctx, "16")
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("commit failure changed inventory", e)
			}
			check, e := dnfguild.LoadWarMissions(ctx, repos.Settings, "16", catalog, now)
			if e != nil || check.Values[2].Claimed {
				t.Fatal("commit failure consumed claim", e)
			}
			full := dnfrepo.CloneInventory(before)
			full.Slots = fullCurrentNPCShopWasteInventory()
			if e = repos.Inventory.Save(ctx, full); e != nil {
				t.Fatal(e)
			}
			send(2, 252)
			after, _, e = repos.Inventory.Load(ctx, "16")
			if e != nil || !reflect.DeepEqual(full, after) {
				t.Fatal("full inventory changed", e)
			}
			check, e = dnfguild.LoadWarMissions(ctx, repos.Settings, "16", catalog, now)
			if e != nil || check.Values[2].Claimed {
				t.Fatal("full inventory consumed claim", e)
			}
			if e = repos.Inventory.Save(ctx, before); e != nil {
				t.Fatal(e)
			}
			rest := send(2, 0)
			if len(rest) == 0 {
				t.Fatal("missing item refresh")
			}
			first, _, e := repos.Inventory.Load(ctx, "16")
			if e != nil {
				t.Fatal(e)
			}
			rest = send(2, 0)
			p, remaining := splitGameServerUpperPacket(t, rest)
			if p.Header.MsgID != 1283 || len(remaining) != 0 {
				t.Fatal("replay updated inventory")
			}
			second, _, e := repos.Inventory.Load(ctx, "16")
			if e != nil || len(first.Slots) != len(second.Slots) {
				t.Fatal("claim twice")
			}
			for key, v := range first.Slots {
				if second.Slots[key].Count != v.Count {
					t.Fatal("quantity changed")
				}
			}
		})
	}
}
