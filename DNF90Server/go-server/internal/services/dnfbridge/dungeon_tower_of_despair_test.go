package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Tower of Despair floors listed in the PVF [npc layer] section make the client
// wait for op347 before it accepts op29. Without it the loading gate stays shut.
func TestRealScriptPVFDespairNPCEntry(t *testing.T) {
	path := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify native despair NPC entry")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	table, err := worldmap.LoadSource(ctx, archive, worldmap.Options{CompactRuntime: true})
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
	aiCharacters, err := newPVFDungeonAICharacterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	adventure, err := adventuregroup.LoadComplete(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, floor := range []int64{9, 10, 11, 20, 30, 40, 50, 60, 70, 80, 90, 100} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) {
			service, old := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
			service.initialEquipmentArchive = archive
			service.adventureGroupTable = adventure
			service.worldMapTable, service.worldMapResolver = table, resolver
			service.dungeonMonsterTable, service.dungeonAICharacterTable = monsters, aiCharacters
			repositories, _ := service.repositoryGroup()
			character := old.Character
			character.Level = 90
			character.Stats["tower_despair_floor"] = floor
			if err := repositories.Character.Save(ctx, character); err != nil {
				t.Fatal(err)
			}
			// The adventure-group level is derived from the character roster by
			// currentAccountAdventureGroupSummary, so the account needs no extra
			// metadata here.
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: character.AccountID,
				Metadata: map[string]string{}}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "99",
				Slots: map[string]dnfrepo.ItemStack{"0:121": {ItemID: 4473, Count: 5}}}); err != nil {
				t.Fatal(err)
			}
			enableTownMoveSkillProjection(t, service, repositories, "99")
			conn := &bufferConn{}
			session := &gameSession{conn: conn, selectedCharacterID: 99, enterSelectDungeonContextSent: true}
			bindDungeonSelectorOriginForTestAt(t, service, session, 7, 3, 474, 234)
			request := make([]byte, 21)
			binary.LittleEndian.PutUint32(request, 11008)
			binary.LittleEndian.PutUint16(request[9:11], 0xffff)
			if err := service.handleDungeonSelectUpper(session, request); err != nil {
				t.Fatal(err)
			}
			info, opponent, start := -1, -1, -1
			for i, packet := range splitAllCurrentGameServerUpperPackets(t, conn.write.Bytes()) {
				if packet.Header.Classification != 0 {
					continue
				}
				switch packet.Header.MsgID {
				case 28:
					info = i
					if got := binary.LittleEndian.Uint32(packet.Body); got != uint32(11007+floor) {
						t.Fatalf("floor skipped: dungeon=%d", got)
					}
				case 347:
					if opponent >= 0 {
						t.Fatal("duplicate NPC initialization")
					}
					opponent = i
					assertNativeDespairSelfOpponent(t, packet.Body)
				case 29:
					start = i
				case 348:
					t.Fatal("entry fabricated a tower clear")
				}
			}
			if info < 0 || start <= info {
				t.Fatalf("entry missing: op28=%d op29=%d wire=%x", info, start, conn.write.Bytes())
			}
			if floor%10 == 0 && floor < 100 {
				if opponent <= info || opponent >= start {
					t.Fatalf("native loading gate remains closed: op28=%d op347=%d op29=%d", info, opponent, start)
				}
			} else if opponent >= 0 {
				t.Fatal("ordinary floor received NPC initialization")
			}
			after, _, _ := repositories.Character.Load(ctx, "99")
			inventory, _, _ := repositories.Inventory.Load(ctx, "99")
			if after.Stats["tower_despair_floor"] != floor || inventory.Slots["0:121"].Count != 4 {
				t.Fatalf("entry changed progression or consumed wrong ticket count: floor=%d inventory=%+v", after.Stats["tower_despair_floor"], inventory.Slots)
			}
			conn.write.Reset()
			if err := service.handleDungeonSelectUpper(session, request); err != nil {
				t.Fatal(err)
			}
			for _, packet := range splitAllCurrentGameServerUpperPackets(t, conn.write.Bytes()) {
				if packet.Header.Classification == 0 && (packet.Header.MsgID == 28 || packet.Header.MsgID == 29 || packet.Header.MsgID == 347) {
					t.Fatal("duplicate select reset native loading or opponent state")
				}
			}
			retried, _, _ := repositories.Inventory.Load(ctx, "99")
			if retried.Slots["0:121"].Count != 4 {
				t.Fatal("duplicate select consumed another ticket")
			}
		})
	}
}

// Follow the current EXE reader's wire offsets, including all three u32-sized
// string lengths and all 26 equipment slots. An empty name selects sub_17F0950.
func assertNativeDespairSelfOpponent(t *testing.T, body []byte) {
	t.Helper()
	if len(body) != 129 || body[0] != 1 {
		t.Fatalf("invalid active opponent packet: %x", body)
	}
	for _, offset := range []int{1, 8, 120} {
		if binary.LittleEndian.Uint32(body[offset:]) != 0 {
			t.Fatalf("native self-opponent requires empty string at %d", offset)
		}
	}
	if !bytes.Equal(body[1:], make([]byte, 128)) {
		t.Fatal("native self-opponent contains invented descriptor data")
	}
}
