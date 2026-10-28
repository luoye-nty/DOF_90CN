package dnfbridge

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfepicwish "longheng.io/server/internal/modules/dnf/epicwish"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestDecodeCurrentEpicWishUpdateMatchesCurrentEXELayout(t *testing.T) {
	want := []uint32{101000004, 101010005, 0, 101020006, 101030007}
	body, err := buildCurrentEpicWishSnapshotBody(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCurrentEpicWishUpdate(body)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decode=%v error=%v body=%x", got, err, body)
	}
	for _, invalid := range [][]byte{
		nil,
		{6},
		{1},
		{2, 1, 0, 0, 0, 1, 0, 0, 0},
	} {
		if _, err := decodeCurrentEpicWishUpdate(invalid); err == nil {
			t.Fatalf("invalid body accepted: %x", invalid)
		}
	}
}

func TestHandleCurrentEpicWishUpdateCommitsThenSendsAckAndClass0Snapshot(t *testing.T) {
	service, repositories, session := currentEpicWishTestService(t, "account-1")
	connection := &bufferConn{}
	session.conn = connection
	session.dungeon.runtime = &runtimeDungeonState{
		Character: dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"},
	}
	want := []uint32{101000004, 0, 101010005}
	body, err := buildCurrentEpicWishSnapshotBody(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentEpicWishUpdate(session, body); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load found=%t error=%v", found, err)
	}
	items, err := dnfepicwish.ItemsFromCharacter(character)
	if err != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("items=%v error=%v want=%v", items, err, want)
	}
	if runtimeItems, err := dnfepicwish.ItemsFromCharacter(session.dungeon.runtime.Character); err != nil ||
		!reflect.DeepEqual(runtimeItems, want) {
		t.Fatalf("runtime items=%v error=%v", runtimeItems, err)
	}

	ack, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if ack.Header.Classification != 1 || ack.Header.MsgID != uint16(dnfenum.CmdPacketUpdateWishItem) ||
		len(ack.Body) != 4 || ack.Body[3] != 1 {
		t.Fatalf("ack=%+v body=%x", ack.Header, ack.Body)
	}
	snapshot, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if snapshot.Header.Classification != 0 || snapshot.Header.MsgID != currentEpicWishSnapshotWireOpcode ||
		len(snapshot.Body) != 3+len(body) || !reflect.DeepEqual(snapshot.Body[3:], body) {
		t.Fatalf("snapshot=%+v body=%x want=%x", snapshot.Header, snapshot.Body, body)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing packets=%x", rest)
	}
}

func TestHandleCurrentEpicWishUpdateFailureAcknowledgesWithoutMutation(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		items     []uint32
	}{
		{name: "account mismatch", accountID: "foreign", items: []uint32{101000004}},
		{name: "non epic item", accountID: "account-1", items: []uint32{101040009}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, repositories, session := currentEpicWishTestService(t, test.accountID)
			connection := &bufferConn{}
			session.conn = connection
			body, err := buildCurrentEpicWishSnapshotBody(test.items)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleCurrentEpicWishUpdate(session, body); err != nil {
				t.Fatal(err)
			}
			character, found, err := repositories.Character.Load(context.Background(), "19")
			if err != nil || !found {
				t.Fatalf("load found=%t error=%v", found, err)
			}
			if items, err := dnfepicwish.ItemsFromCharacter(character); err != nil || len(items) != 0 {
				t.Fatalf("mutated items=%v error=%v", items, err)
			}
			failure, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
			if failure.Header.Classification != 1 ||
				failure.Header.MsgID != uint16(dnfenum.CmdPacketUpdateWishItem) ||
				len(failure.Body) != 4 || failure.Body[3] != 0 {
				t.Fatalf("failure=%+v body=%x", failure.Header, failure.Body)
			}
			if len(rest) != 0 {
				t.Fatalf("failure emitted snapshot/trailing=%x", rest)
			}
		})
	}
}

func TestSendCurrentDungeonEpicWishSnapshotIsTimeRiftOnly(t *testing.T) {
	service, repositories, session := currentEpicWishTestService(t, "account-1")
	owner, err := dnfepicwish.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Apply(context.Background(), dnfepicwish.Command{
		CharacterID: "19",
		AccountID:   "account-1",
		Items:       []uint32{101000004},
	})
	if err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session.conn = connection
	runtime := &runtimeDungeonState{
		Dungeon:   worldmap.Dungeon{Metadata: worldmap.DungeonMetadata{InfernoDungeon: true}},
		Character: result.Character,
	}
	if err := service.sendCurrentDungeonEpicWishSnapshot(session, runtime, "test"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentEpicWishSnapshotWireOpcode ||
		len(packet.Body) != 8 || packet.Body[3] != 1 ||
		binary.LittleEndian.Uint32(packet.Body[4:8]) != 101000004 || len(rest) != 0 {
		t.Fatalf("packet=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}

	for name, dungeon := range map[string]worldmap.Dungeon{
		"ordinary":   {},
		"magic rift": {Metadata: worldmap.DungeonMetadata{DungeonType: "[particular hell]"}},
	} {
		t.Run(name, func(t *testing.T) {
			connection := &bufferConn{}
			session.conn = connection
			runtime.Dungeon = dungeon
			if err := service.sendCurrentDungeonEpicWishSnapshot(session, runtime, "test"); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != 0 {
				t.Fatalf("unexpected snapshot=%x", connection.write.Bytes())
			}
		})
	}
}

func TestSelectCurrentTimeRiftEpicCandidateKeepsWishOutOfMissPool(t *testing.T) {
	pools := map[int][]currentDungeonHellPartyFragmentCandidate{
		85: {
			{EquipmentID: 101000004, FragmentID: 103000004, Weight: 1},
			{EquipmentID: 101010005, FragmentID: 103010005, Weight: 1},
		},
		90: {{EquipmentID: 101020006, FragmentID: 103020006, Weight: 1}},
	}
	wish := []uint32{101000004}
	wishHits := 0
	for seed := uint32(1); seed <= 1000; seed++ {
		candidate, found, wishHit := selectCurrentTimeRiftEpicCandidate(
			newCurrentDungeonDropLCG(seed),
			pools,
			90,
			wish,
		)
		if !found {
			t.Fatalf("seed=%d returned no candidate", seed)
		}
		if wishHit {
			wishHits++
			if candidate.EquipmentID != wish[0] {
				t.Fatalf("wish hit selected=%d", candidate.EquipmentID)
			}
		} else if candidate.EquipmentID == wish[0] {
			t.Fatalf("miss branch selected wished item for seed=%d", seed)
		}
	}
	if wishHits == 0 || wishHits == 1000 {
		t.Fatalf("wish branch coverage=%d", wishHits)
	}

	allWished := []uint32{101000004, 101010005, 101020006}
	misses := 0
	for seed := uint32(1); seed <= 100; seed++ {
		candidate, found, wishHit := selectCurrentTimeRiftEpicCandidate(
			newCurrentDungeonDropLCG(seed),
			pools,
			90,
			allWished,
		)
		if !found {
			t.Fatalf("all-wished rarity-4 roll disappeared for seed=%d", seed)
		}
		probe := newCurrentDungeonDropLCG(seed)
		if probe.Next(currentEpicWishHitDenominator) >= currentEpicWishHitNumerator {
			misses++
			if wishHit || candidate.EquipmentID == 0 {
				t.Fatalf("all-wished miss was relabeled as a preference hit: seed=%d candidate=%+v hit=%t", seed, candidate, wishHit)
			}
		}
	}
	if misses == 0 {
		t.Fatal("all-wished fallback did not exercise a compatibility miss")
	}
}

func TestSelectCurrentTimeRiftEpicCandidateWithoutWishPreservesBaseline(t *testing.T) {
	pools := map[int][]currentDungeonHellPartyFragmentCandidate{
		85: {
			{EquipmentID: 101000004, FragmentID: 103000004, Weight: 1},
			{EquipmentID: 101010005, FragmentID: 103010005, Weight: 4},
		},
		90: {{EquipmentID: 101020006, FragmentID: 103020006, Weight: 2}},
	}
	for seed := uint32(1); seed <= 100; seed++ {
		baselineRNG := newCurrentDungeonDropLCG(seed)
		want, wantFound := selectCurrentHellPartyFragmentCandidate(baselineRNG, pools, 90)
		wishRNG := newCurrentDungeonDropLCG(seed)
		got, gotFound, wishHit := selectCurrentTimeRiftEpicCandidate(wishRNG, pools, 90, nil)
		if gotFound != wantFound || got != want || wishHit || wishRNG.Seed() != baselineRNG.Seed() {
			t.Fatalf(
				"seed=%d got=%+v/%t/hit=%t/seed=%d want=%+v/%t/seed=%d",
				seed,
				got,
				gotFound,
				wishHit,
				wishRNG.Seed(),
				want,
				wantFound,
				baselineRNG.Seed(),
			)
		}
	}
}

func TestEpicWishClass0Op1288DoesNotReplaceClass1DamageFontOpcode(t *testing.T) {
	if currentEpicWishSnapshotWireOpcode != uint16(dnfenum.CmdPacketSelectDamageFontSkin) {
		t.Fatalf(
			"class0 wish=%d class1 damage-font=%d",
			currentEpicWishSnapshotWireOpcode,
			dnfenum.CmdPacketSelectDamageFontSkin,
		)
	}
}

func currentEpicWishTestService(
	t *testing.T,
	sessionAccountID string,
) (*Service, dnfrepo.Group, *gameSession) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":          "",
		dungeonDropStackableList:       "",
		dungeonDropEquipmentList:       "101000004 `character/a.equ`\n101010005 `character/b.equ`\n101020006 `character/c.equ`\n101040009 `character/rare.equ`\n",
		"equipment/character/a.equ":    "[rarity]\n4\n[minimum level]\n85\n[creation rate]\n1\n[equipment type]\n`[weapon]` 0\n",
		"equipment/character/b.equ":    "[rarity]\n4\n[minimum level]\n85\n[creation rate]\n1\n[equipment type]\n`[weapon]` 0\n",
		"equipment/character/c.equ":    "[rarity]\n4\n[minimum level]\n90\n[creation rate]\n1\n[equipment type]\n`[weapon]` 0\n",
		"equipment/character/rare.equ": "[rarity]\n3\n[minimum level]\n90\n[creation rate]\n1\n[equipment type]\n`[weapon]` 0\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog:     catalog,
	}
	return service, repositories, &gameSession{
		accountID:           sessionAccountID,
		selectedCharacterID: 19,
	}
}
