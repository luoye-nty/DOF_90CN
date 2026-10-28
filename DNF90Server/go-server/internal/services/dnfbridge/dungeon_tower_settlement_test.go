package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func enableTowerCountsForTest(t *testing.T, service *Service, sessions ...*gameSession) {
	t.Helper()
	service.adventureGroupTable = loadAdventureGroupTestTables(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Account == nil {
		t.Fatal("tower account repository missing")
	}
	for _, session := range sessions {
		accountID := service.accountIDForSession(session)
		if _, found, err := repositories.Account.Load(context.Background(), accountID); err != nil {
			t.Fatal(err)
		} else if !found {
			if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: accountID}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertTowerCountsPacket(t *testing.T, stream, want []byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.MsgID != 1294 || packet.Header.Classification != 0 || !bytes.Equal(packet.Body, want) {
		t.Fatalf("native tower counters: header=%+v body=%v want=%v", packet.Header, packet.Body, want)
	}
	return rest
}

func assertTowerCharacterEntries(t *testing.T, stream []byte, despair, grief byte) []byte {
	t.Helper()
	for i, remaining := range []byte{despair, grief} {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.MsgID != 546 || packet.Header.Classification != 0 || len(packet.Body) != 5 ||
			binary.LittleEndian.Uint32(packet.Body) != [2]uint32{11008, 5115}[i] || packet.Body[4] != remaining {
			t.Fatalf("native character entry: header=%+v body=%x want remaining=%d", packet.Header, packet.Body, remaining)
		}
		stream = rest
	}
	return stream
}

func assertTowerCountsBeforeSelector(t *testing.T, stream, want []byte) {
	t.Helper()
	foundCounts, foundSelector := false, false
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		switch packet.Header.MsgID {
		case 1294:
			assertTowerCountsPacket(t, stream, want)
			if foundCounts || foundSelector {
				t.Fatal("selector counters duplicated or sent after the floor context")
			}
			foundCounts = true
		case 27:
			if !foundCounts {
				t.Fatal("selector opened before counter refresh")
			}
			foundSelector = true
		}
		stream = rest
	}
	if !foundCounts || !foundSelector {
		t.Fatal("selector counts or floor context missing")
	}
}

func TestTowerSettlementDisplaysNativeRewardAndReturnsWithoutOrdinaryCards(t *testing.T) {
	for _, tower := range []struct {
		flag string
		id   uint16
	}{{"tower of despair", 348}, {"tower of grief", 1263}} {
		for _, exit := range []string{"op42", "op72"} {
			t.Run(tower.flag+"/"+exit, func(t *testing.T) {
				service, runtime, session, conn := prepareOrdinaryCompletedSettlementRuntimeForExitTest(t)
				enableTowerCountsForTest(t, service, session)
				repositories, _ := service.repositoryGroup()
				enableTownMoveSkillProjection(t, service, repositories, "99")
				runtime.Dungeon.Metadata.TutorialDungeon.Set = false
				runtime.Dungeon.Metadata.Flags = map[string]bool{tower.flag: true}
				plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
				plan.TowerRewardMsgID = tower.id
				plan.TowerRewardBody, _ = buildCurrentDungeonTowerRewardBody(12345, 1, nil)
				runtime.settlementResultPlan = &plan
				conn.write.Reset()
				if err := service.handleDungeonSetPlayResult(session, make([]byte, currentDungeonPlayResultBaseSize)); err != nil {
					t.Fatal(err)
				}
				remaining := conn.write.Bytes()
				for _, id := range []uint16{34, 37, 35, 546, 546, 1294, tower.id} {
					packet, tail := splitGameServerUpperPacket(t, remaining)
					if packet.Header.MsgID != id || packet.Header.Classification != 0 {
						t.Fatalf("packet=%+v want class0/%d", packet.Header, id)
					}
					if id == tower.id && !bytes.Equal(packet.Body, plan.TowerRewardBody) {
						t.Fatalf("tower body=%x", packet.Body)
					}
					remaining = tail
				}
				if len(remaining) != 0 || runtime.settlementStatisticReceived ||
					!completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) ||
					completedDungeonSettlementExitReady(runtime, session.selectedCharacterID+1) {
					t.Fatal("tower completion/character barrier is incorrect")
				}
				conn.write.Reset()
				if err := service.handleDungeonSetPlayResult(session, make([]byte, currentDungeonPlayResultBaseSize)); err != nil {
					t.Fatal(err)
				}
				if err := service.handleDungeonScoreScrollState(session, nil); err != nil {
					t.Fatal(err)
				}
				if err := service.handleDungeonCardSelectRightState(session, nil); err != nil {
					t.Fatal(err)
				}
				if conn.write.Len() != 0 || runtime.settlementCardRewardState != nil ||
					runtime.settlementCardLayoutSent || runtime.settlementCardRewardCommitted {
					t.Fatal("tower replay or card request entered the ordinary reward chain")
				}
				var err error
				if exit == "op42" {
					err = service.handleDungeonGiveupGame(session, nil)
				} else {
					err = service.handleDungeonEplpCommand(session, []byte{1, 2})
				}
				if err != nil {
					t.Fatal(err)
				}
				if !runtime.townReturnOp24Sent || session.dungeon.runtime != nil {
					t.Fatal("tower exit failed to send town transition and detach completed runtime")
				}
				foundTown := false
				for remaining = conn.write.Bytes(); len(remaining) != 0; {
					packet, tail := splitGameServerUpperPacket(t, remaining)
					foundTown = foundTown || packet.Header.MsgID == 24 && packet.Header.Classification == 0
					remaining = tail
				}
				if !foundTown {
					t.Fatal("tower exit has no wire town transition")
				}
			})
		}
	}
}

func TestTowerSettlementFailedNotificationKeepsExitClosedAndResumesOnce(t *testing.T) {
	for _, failAt := range []int{4, 5, 6, 7} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
			enableTowerCountsForTest(t, service, session)
			runtime.Dungeon.Metadata.TutorialDungeon.Set = false
			runtime.Dungeon.Metadata.Flags = map[string]bool{"tower of despair": true}
			plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
			plan.TowerRewardMsgID = 348
			plan.TowerRewardBody, _ = buildCurrentDungeonTowerRewardBody(1, 1, nil)
			runtime.settlementResultPlan = &plan
			wantErr := errors.New("tower notification write failed")
			conn := &failNthDungeonWriteConn{failAt: failAt, err: wantErr}
			session.conn = conn
			request := make([]byte, currentDungeonPlayResultBaseSize)
			if err := service.handleDungeonSetPlayResult(session, request); !errors.Is(err, wantErr) {
				t.Fatalf("send error=%v", err)
			}
			if runtime.settlementTowerRewardSent || completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
				t.Fatal("failed tower notification opened exit")
			}
			if runtime.settlementTowerCountsSent != (failAt == 7) {
				t.Fatal("tower counter delivery was marked before a successful write")
			}
			written := conn.write.Len()
			if err := service.handleDungeonGiveupGame(session, nil); err != nil {
				t.Fatal(err)
			}
			if conn.write.Len() != written || session.dungeon.runtime != runtime {
				t.Fatal("op42 crossed failed tower notification")
			}
			if err := service.handleDungeonSetPlayResult(session, request); err != nil {
				t.Fatal(err)
			}
			rest := conn.write.Bytes()[written:]
			if failAt < 7 {
				rest = assertTowerCharacterEntries(t, rest, 1, 1)
				rest = assertTowerCountsPacket(t, rest, []byte{3, 3, 1, 1})
			}
			packet, tail := splitGameServerUpperPacket(t, rest)
			if packet.Header.MsgID != 348 || len(tail) != 0 || !completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
				t.Fatal("retry did not resume exactly the missing tower packet")
			}
		})
	}
}

func TestTowerFloorUsesPVFMappingAndNativePacketBoundaries(t *testing.T) {
	source := bridgePVFSource{"etc/towerofgrief.etc": "[top layer]\n100\n[each layer matching dungeon]\n1 5115 -1 50 5164 -1 100 5214 -1\n[/each layer matching dungeon]"}
	for _, row := range []struct {
		id    int64
		floor uint16
	}{{5115, 1}, {5164, 50}, {5214, 100}} {
		floor, err := resolveCurrentDungeonTowerFloor(source, worldmap.Dungeon{ID: row.id}, 1263)
		if err != nil || floor != row.floor {
			t.Fatalf("dungeon=%d floor=%d err=%v", row.id, floor, err)
		}
	}
	if _, err := resolveCurrentDungeonTowerFloor(source, worldmap.Dungeon{ID: 9999}, 1263); err == nil {
		t.Fatal("unmapped dungeon was assigned a floor")
	}
	body, err := buildCurrentDungeonTowerRewardBody(12345, 50, []currentDungeonClearRewardPair{{Key: 10160931, Value: 2}})
	if err != nil || len(body) != 87 || binary.LittleEndian.Uint32(body) != 12345 ||
		binary.LittleEndian.Uint16(body[4:]) != 50 || body[6] != 10 ||
		binary.LittleEndian.Uint32(body[7:]) != 10160931 || binary.LittleEndian.Uint32(body[11:]) != 2 {
		t.Fatalf("native tower body=%x err=%v", body, err)
	}
	for slot := 1; slot < 10; slot++ {
		if binary.LittleEndian.Uint32(body[7+slot*8:]) != currentTowerEmptyRewardItemID || binary.LittleEndian.Uint32(body[11+slot*8:]) != 0 {
			t.Fatal("missing native empty card")
		}
	}
	empty, err := buildCurrentDungeonTowerRewardBody(12345, 1, nil)
	if err != nil || len(empty) != 87 || empty[6] != 10 {
		t.Fatalf("empty clear cannot create participant panel: %x %v", empty, err)
	}
	for _, flag := range []string{"tower of despair", "tower of grief"} {
		runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{SourceSections: []worldmap.RawSection{{Name: flag}}}}
		if currentDungeonTowerRewardMsgID(runtime) == 0 {
			t.Fatalf("source section %s missed", flag)
		}
		runtime.ProtocolMode = currentDungeonProtocolModePractice
		if currentDungeonTowerRewardMsgID(runtime) != 0 {
			t.Fatal("practice mode acquired real tower settlement")
		}
	}
}

func TestTowerSettlementRejectsMissingOrWrongNativeNotification(t *testing.T) {
	for _, body := range [][]byte{nil, {1, 0, 0, 0, 1, 0, 1}, {1, 0, 0, 0, 0, 0, 0}} {
		service, runtime, session, conn, _ := prepareCompletedSettlementRuntime(t)
		runtime.Dungeon.Metadata.Flags = map[string]bool{"tower of grief": true}
		plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
		plan.TowerRewardMsgID, plan.TowerRewardBody = 1263, body
		runtime.settlementResultPlan = &plan
		conn.write.Reset()
		if err := service.handleDungeonSetPlayResult(session, make([]byte, currentDungeonPlayResultBaseSize)); err != nil {
			t.Fatal(err)
		}
		if conn.write.Len() != 0 || runtime.settlementTowerRewardSent {
			t.Fatalf("invalid tower body was sent: %x", body)
		}
		plan.TowerRewardBody, _ = buildCurrentDungeonTowerRewardBody(1, 1, nil)
		plan.TowerRewardMsgID = 348
		if currentDungeonTowerPlanValid(runtime, &plan) {
			t.Fatal("grief accepted a despair notification")
		}
	}
}

func TestTowerPickupDisplayUsesRemainingRunDelta(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{Status: runtimeDungeonDropConsumed, DestinationSlot: 65, Item: dungeonDropItemDefinition{ItemID: 8474}, Amount: 2}
	runtime.DropOwner.byObjectKey[2] = &runtimeDungeonDrop{Status: runtimeDungeonDropConsumed, DestinationSlot: 65, Item: dungeonDropItemDefinition{ItemID: 8474}, Amount: 1}
	runtime.DropOwner.byObjectKey[3] = &runtimeDungeonDrop{Status: runtimeDungeonDropConsumed, DestinationSlot: 66, Item: dungeonDropItemDefinition{ItemID: 9000}, Amount: 50}
	rows := currentDungeonTowerPickupRewards(runtime, []currentDungeonClearRewardDrop{{ObjectKey: 65, TemplateID: 8474, StackCount: 20}})
	if len(rows) != 1 || rows[0].Key != 8474 || rows[0].Value != 3 {
		t.Fatalf("pickup delta=%+v", rows)
	}
	rows = currentDungeonTowerPickupRewards(runtime, []currentDungeonClearRewardDrop{{ObjectKey: 65, TemplateID: 8474, StackCount: 1}})
	if len(rows) != 1 || rows[0].Value != 1 {
		t.Fatalf("consumed stack projected old pickup count: %+v", rows)
	}
	if rows := currentDungeonTowerPickupRewards(runtime, nil); len(rows) != 0 {
		t.Fatalf("stale pickup projected: %+v", rows)
	}
}

func TestRealScriptPVFTowerSettlementProducer(t *testing.T) {
	path := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify tower settlement against Script.pvf")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{CompactRuntime: true})
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
	progressionTables, err := progression.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	experience, err := progression.LoadMonsterExperienceSources(archive)
	if err != nil {
		t.Fatal(err)
	}
	ranks, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	resources := currentDungeonSettlementRewardResources{
		Progression: progressionTables, ExperienceSources: experience, ClearRankCatalog: ranks,
	}
	adventureTables, err := adventuregroup.LoadComplete(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{11008, 11107, 5115, 5117, 5119, 5164, 5214} {
		for _, exit := range []string{"op42", "op72"} {
			t.Run(fmt.Sprintf("%d/%s", id, exit), func(t *testing.T) {
				dungeon, found := table.FindDungeon(id)
				if !found {
					t.Fatalf("PVF dungeon %d missing", id)
				}
				service, runtime, session, conn := prepareOrdinaryCompletedSettlementRuntimeForExitTest(t)
				service.initialEquipmentArchive = archive
				service.adventureGroupTable = adventureTables
				service.worldMapTable, service.worldMapResolver = table, resolver
				service.dungeonMonsterTable, service.dungeonAICharacterTable = monsters, aiCharacters
				runtime.Dungeon = dungeon
				if len(dungeon.SourceSections) != 0 || currentDungeonTowerRewardMsgID(runtime) == 0 {
					t.Fatalf("production compact dungeon lost tower identity: %+v", dungeon.Metadata.Flags)
				}
				runtime.Character.Level = 90
				runtime.Character.Stats["exp"] = 0
				repositories, _ := service.repositoryGroup()
				enableTownMoveSkillProjection(t, service, repositories, "99")
				ctx := context.Background()
				account := dnfrepo.AccountRecord{AccountID: runtime.Character.AccountID,
					Metadata: map[string]string{}}
				if id == 5117 || id == 5119 {
					for i := 0; i < 2; i++ {
						if err := dnfdungeon.RecordTowerDailyClear(&account, dnfdungeon.TowerGrief, service.gameplayNow()); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := repositories.Account.Save(ctx, account); err != nil {
					t.Fatal(err)
				}
				if err := repositories.Character.Save(ctx, runtime.Character); err != nil {
					t.Fatal(err)
				}
				if id == 5119 {
					conn.write.Reset()
					if err := service.sendCurrentAdventureTowerCounts(ctx, session, "before_last_clear"); err != nil {
						t.Fatal(err)
					}
					if rest := assertTowerCountsPacket(t, assertTowerCharacterEntries(t, conn.write.Bytes(), 1, 1), []byte{6, 6, 1, 3}); len(rest) != 0 {
						t.Fatal("unexpected initial counter packets")
					}
					conn.write.Reset()
				}
				if id == 5117 {
					if err := dnfdungeon.RecordTowerDailyClear(&account, dnfdungeon.TowerGrief, service.gameplayNow()); err != nil {
						t.Fatal(err)
					}
					if err := repositories.Account.Save(ctx, account); err != nil {
						t.Fatal(err)
					}
					var projection adventuregroup.Projection
					service.projectAdventureTowerCounts(&projection, account, true, 8)
					if projection.TowerLimits != [2]uint32{6, 3} || projection.TowerRemaining != [2]uint32{6, 0} {
						t.Fatalf("Lv8 tooltip disagrees with admission: %+v", projection)
					}
				}
				if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "99", Points: dnfrepo.SkillPointState{SyncedLevel: 90}}); err != nil {
					t.Fatal(err)
				}
				runtime.settlementPlayResultReceived = true
				runtime.settlementPlayResultBody = make([]byte, currentDungeonPlayResultBaseSize)
				runtime.startedAt = time.Unix(1700000000, 0)
				runtime.clearMapCompletionAt = runtime.startedAt.Add(12345 * time.Millisecond)
				if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(ctx, session, runtime, resources); err != nil {
					t.Fatalf("dungeon %d: %v", id, err)
				}
				plan := runtime.settlementResultPlan
				if plan == nil || !currentDungeonTowerPlanValid(runtime, plan) || plan.clearRewardSnapshot.Tail.ShowResult || runtime.settlementCardRewardState != nil {
					t.Fatalf("dungeon %d produced ordinary card/result UI", id)
				}
				before, _, _ := repositories.Character.Load(ctx, "99")
				floor, err := service.currentDungeonTowerFloor(runtime)
				if err != nil {
					t.Fatal(err)
				}
				kind := currentDungeonTowerKind(runtime)
				wantFloor := floor%100 + 1
				if got, err := dnfdungeon.TowerFloor(before, kind); err != nil || got != wantFloor {
					t.Fatalf("persisted next floor=%d want=%d err=%v", got, wantFloor, err)
				}
				selector, err := service.currentTowerSelectorContext(ctx, session)
				if err != nil {
					t.Fatal(err)
				}
				selectorBody, err := selector.Build()
				if err != nil {
					t.Fatal(err)
				}
				offset := 15
				if kind == dnfdungeon.TowerGrief {
					offset = 34
				}
				if binary.LittleEndian.Uint16(selectorBody[offset:]) != wantFloor {
					t.Fatalf("selector reset floor: %x", selectorBody)
				}
				baseID, err := dnfdungeon.TowerDungeonID(archive, kind, 1)
				if err != nil {
					t.Fatal(err)
				}
				base, _ := table.FindDungeon(baseID)
				nextID, err := service.resolveCurrentTowerDungeonSelection(before, base)
				wantID, wantErr := dnfdungeon.TowerDungeonID(archive, kind, wantFloor)
				if err != nil || wantErr != nil || nextID != wantID {
					t.Fatalf("next entry=%d want=%d err=%v %v", nextID, wantID, err, wantErr)
				}
				if len(plan.TowerRewardBody) != 87 || plan.TowerRewardBody[6] != 10 {
					t.Fatalf("missing reward panel: %x", plan.TowerRewardBody)
				}
				if kind == dnfdungeon.TowerGrief && (floor == 50 || floor == 100) && binary.LittleEndian.Uint32(plan.TowerRewardBody[79:]) != 10160930 {
					t.Fatalf("missing PVF special reward: %x", plan.TowerRewardBody)
				}
				first := append([]byte(nil), plan.TowerRewardBody...)
				runtime.settlementResultPlan = nil
				if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(ctx, session, runtime, resources); err != nil {
					t.Fatal(err)
				}
				after, _, _ := repositories.Character.Load(ctx, "99")
				if before.Stats["exp"] != after.Stats["exp"] || !bytes.Equal(first, runtime.settlementResultPlan.TowerRewardBody) {
					t.Fatal("retry changed frozen tower reward or granted experience twice")
				}
				if got, _ := dnfdungeon.TowerFloor(after, kind); got != wantFloor {
					t.Fatal("replay advanced tower twice")
				}
				if id == 5117 {
					account, _, err := repositories.Account.Load(ctx, runtime.Character.AccountID)
					if err != nil {
						t.Fatal(err)
					}
					if used, err := dnfdungeon.TowerDailyUsed(account, kind, service.gameplayNow()); err != nil || used != 3 {
						t.Fatalf("accepted clear/replay usage=%d err=%v", used, err)
					}
				}
				conn.write.Reset()
				if err := service.handleDungeonSetPlayResult(session, runtime.settlementPlayResultBody); err != nil {
					t.Fatal(err)
				}
				wantCounts := []byte{6, 6, 3, 3}
				if id == 5119 {
					wantCounts = []byte{6, 6, 1, 3}
				} else if id == 5117 {
					wantCounts = []byte{6, 6, 0, 3}
				}
				foundReward, foundCounts := false, false
				for remaining := conn.write.Bytes(); len(remaining) != 0; {
					packet, tail := splitGameServerUpperPacket(t, remaining)
					if packet.Header.MsgID == 1294 {
						assertTowerCountsPacket(t, remaining, wantCounts)
						if foundCounts || foundReward {
							t.Fatal("tower counts duplicated or sent after the reward window")
						}
						foundCounts = true
					}
					if packet.Header.MsgID == plan.TowerRewardMsgID && packet.Header.Classification == 0 {
						foundReward = bytes.Equal(packet.Body, first)
					}
					remaining = tail
				}
				if !foundCounts || !foundReward || !completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
					t.Fatal("real compact tower did not send its reward notification and release the exit")
				}
				conn.write.Reset()
				if exit == "op42" {
					err = service.handleDungeonGiveupGame(session, nil)
				} else {
					err = service.handleDungeonEplpCommand(session, []byte{1, 2})
				}
				if err != nil || !runtime.townReturnOp24Sent || session.dungeon.runtime != nil {
					t.Fatalf("real compact tower exit failed: err=%v op24=%t", err, runtime.townReturnOp24Sent)
				}
				conn.write.Reset()
				if err := service.sendCurrentTownEnterSelectContext(session, "after_tower_return"); err != nil {
					t.Fatal(err)
				}
				assertTowerCountsBeforeSelector(t, conn.write.Bytes(), wantCounts)
				if id == 5117 {
					inventory, _, _ := repositories.Inventory.Load(ctx, "99")
					if inventory.Slots == nil {
						inventory.Slots = make(map[string]dnfrepo.ItemStack)
					}
					inventory.Slots["0:121"] = dnfrepo.ItemStack{ItemID: 10161065, Count: 5}
					if err := repositories.Inventory.Save(ctx, inventory); err != nil {
						t.Fatal(err)
					}
					conn.write.Reset()
					body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
					binary.LittleEndian.PutUint32(body, uint32(baseID))
					binary.LittleEndian.PutUint16(body[9:11], 0xffff)
					if err := service.handleDungeonSelectUpper(session, body); err != nil {
						t.Fatal(err)
					}
					blocked, refreshed := false, false
					for stream := conn.write.Bytes(); len(stream) > 0; {
						packet, tail := splitGameServerUpperPacket(t, stream)
						if packet.Header.MsgID == 16 {
							blocked = bytes.Equal(packet.Body, []byte{0, 133, 0})
						}
						if packet.Header.MsgID == currentTownChatNotificationMsgID {
							t.Fatal("entry refusal impersonated the character in chat")
						}
						if packet.Header.MsgID == 1294 {
							assertTowerCountsPacket(t, stream, []byte{6, 6, 0, 3})
							refreshed = true
						}
						if packet.Header.MsgID == currentDungeonInfoNotification {
							t.Fatal("exhausted account received dungeon entry")
						}
						stream = tail
					}
					unchanged, _, _ := repositories.Inventory.Load(ctx, "99")
					if !blocked || !refreshed || session.dungeon.runtime != nil || !reflect.DeepEqual(inventory.Slots, unchanged.Slots) {
						t.Fatalf("limit guard: blocked=%t refreshed=%t inventory=%+v", blocked, refreshed, unchanged.Slots)
					}
					nextDay := service.gameplayNow().Add(24 * time.Hour)
					service.gameplayTimers = &processGameplayTimeQueue{now: func() time.Time { return nextDay }}
					conn.write.Reset()
					if err := service.sendCurrentTownEnterSelectContext(session, "next_service_day"); err != nil {
						t.Fatal(err)
					}
					assertTowerCountsBeforeSelector(t, conn.write.Bytes(), []byte{6, 6, 3, 3})
				}
				if floor == 1 || floor == 3 {
					for _, state := range []byte{0, 1} {
						next, scene, err := service.prepareDungeonRuntime(ctx, session, dungeoncmd.SelectDungeonRequest{DungeonID: uint32(baseID), RuntimeState: state, RuntimeToken: 0xffff})
						if err != nil {
							t.Fatalf("next floor runtime state=%d: %v", state, err)
						}
						resolvedID := wantID
						if kind == dnfdungeon.TowerDespair {
							resolvedID, _, err = resolveCurrentTowerOfDespairSoloDungeon(table, resolver, wantID)
							if err != nil {
								t.Fatal(err)
							}
						}
						if next.Dungeon.ID != resolvedID || scene.Map.Map.ID == 0 {
							t.Fatalf("wrong next dungeon=%d expected=%d", next.Dungeon.ID, resolvedID)
						}
					}
				}
			})
		}
	}
}

// completedDungeonSettlementExitReady mirrors the aggregate exit barrier the
// production path composes inline (the cardless exit readiness in
// dungeon_card_flow.go). A tower releases the exit only once its own native
// clear window (op348/op1263) has actually been published.
func completedDungeonSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if completedCurrentDungeonAltarSettlementExitReady(runtime, characterID) {
		return true
	}
	if completedRuntimeRaidDungeonExitReady(runtime, characterID) {
		return true
	}
	if !completedDungeonSettlementResultReady(runtime, characterID) {
		return false
	}
	if currentDungeonTowerRewardMsgID(runtime) != 0 {
		return runtime.settlementTowerRewardSent
	}
	if runtime.tournament != nil {
		return runtime.tournament.RewardSelectionComplete()
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return true
	}
	return runtime.settlementCardLayoutSent && runtime.settlementCardSelectionSent &&
		runtime.settlementCardRewardCommitted &&
		runtime.settlementPhase >= currentDungeonSettlementPhaseRewardCommitted
}
