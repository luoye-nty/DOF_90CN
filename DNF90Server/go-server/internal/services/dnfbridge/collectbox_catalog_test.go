package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	dnfcharstat "longheng.io/server/internal/modules/dnf/charstat"
	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestBuildCurrentCollectboxCatalogParsesRowsAndSlots(t *testing.T) {
	document, err := dnfpvf.Parse("collectboxinfo.etc", "[Collect Box Info]\n[Index] 2\n[max expiration date] `2030-01-01 00:00:00`\n[Item Position Info] 2 2 1001 3 1002 1 1003 2 1004 4\n[Collect Box Info]\n[Index] 4\n[Item Position Info] 1 1 2001 1\n")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildCurrentCollectboxCatalog(document)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := collectboxDefinition(catalog, 2)
	if !ok || len(definition.Slots) != 4 || definition.Slots[2].ItemID != 1003 || definition.Slots[2].BuffType != 2 {
		t.Fatalf("definition = %+v, found=%t", definition, ok)
	}
}

func TestBuildCurrentCollectboxCatalogParsesPVFBuffTypes(t *testing.T) {
	buffs, err := parseCurrentCollectboxBuffs("[Enchant Info]\n3\n[equipment physical attack]\n8 0\n[equipment magical attack]\n8 0\n[separate attack]\n12 0\n[/Enchant Info]\n")
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse("collectboxinfo.etc", "[Collect Box Info]\n[Index] 5\n[Item Position Info] 1 1 490007148 3\n")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildCurrentCollectboxCatalogWithBuffs(document, buffs)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := collectboxDefinition(catalog, 5)
	if !ok || definition.Slots[0].Bonus.PhysicalAttack != 8 || definition.Slots[0].Bonus.MagicalAttack != 8 || definition.Slots[0].Bonus.IndependentAttack != 12 {
		t.Fatalf("definition = %+v, found=%t", definition, ok)
	}
}

func TestCurrentCollectboxCatalogReadsPatchedRealPVF(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run against Script.pvf")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := archive.ReadText("localization/collectbox/collectboxinfo.etc")
	if err != nil {
		t.Fatal(err)
	}
	buffText, err := archive.ReadText("localization/collectbox/collectboxbuffinfo.etc")
	if err != nil {
		t.Fatal(err)
	}
	buffs, err := parseCurrentCollectboxBuffs(buffText)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse("localization/collectbox/collectboxinfo.etc", info)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildCurrentCollectboxCatalogWithBuffs(document, buffs)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.indexes) != 5 || catalog.indexes[0] != 1 || catalog.indexes[4] != 5 {
		t.Fatalf("indexes = %v", catalog.indexes)
	}
	box3, ok := collectboxDefinition(catalog, 3)
	if !ok || len(box3.Slots) != 11 || box3.Slots[0].Bonus.PhysicalAttack != 8 || box3.Slots[0].Bonus.IndependentAttack != 12 {
		t.Fatalf("box3 = %+v found=%t", box3, ok)
	}
	box1, ok := collectboxDefinition(catalog, 1)
	if !ok || len(box1.Slots) != 15 || box1.Slots[0].ItemID != 10007322 {
		t.Fatalf("SAO box1 = %+v found=%t", box1, ok)
	}
	box5, ok := collectboxDefinition(catalog, defaultCollectboxIndex)
	if defaultCollectboxIndex != 5 || !ok || len(box5.Slots) != 9 || box5.Slots[0].ItemID != 490007148 ||
		box5.Slots[0].Bonus.PhysicalAttack != 8 || box5.Slots[0].Bonus.MagicalAttack != 8 ||
		box5.Slots[0].Bonus.IndependentAttack != 12 {
		t.Fatalf("兽人守护神 box5 = %+v found=%t default=%d", box5, ok, defaultCollectboxIndex)
	}
}

func TestBuildCurrentCollectboxCatalogSkipsPVFEmptySlotAndReadsExtensionItem(t *testing.T) {
	document, err := dnfpvf.Parse("collectboxinfo.etc", "[Collect Box Info]\n[Index] 3\n[Extend Item]\n10007344 1\n[/Extend Item]\n[Item Position Info] 1 3 490000282 3 490000289 3 -1 3\n")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildCurrentCollectboxCatalog(document)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := collectboxDefinition(catalog, 3)
	if !ok || len(definition.Slots) != 2 || definition.ExtendItemID != 10007344 || definition.ExtendItemCount != 1 {
		t.Fatalf("definition = %+v, found=%t", definition, ok)
	}
}

func TestBuildCollectboxStateBodyUsesCurrentLayout(t *testing.T) {
	definition := dnfcollectbox.Definition{Index: 3, Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}}
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"3": {"0": 9001}}}
	body := buildCollectboxStateBody(definition, state)
	if len(body) != 12 || body[0] != 3 || body[1] != 1 || body[7] != 1 {
		t.Fatalf("body header = %x", body)
	}
	if got := binary.LittleEndian.Uint32(body[8:12]); got != 9001 {
		t.Fatalf("item id = %d", got)
	}
}

func TestBuildCollectboxStateBodyUsesOp1027ExpiryContract(t *testing.T) {
	expiry := time.Date(2037, 1, 5, 6, 0, 0, 0, time.Local)
	definition := dnfcollectbox.Definition{Index: 3, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}}
	body := buildCollectboxStateBodyAt(definition, dnfcollectbox.State{}, false, expiry.Add(-time.Hour))
	if body[0] != 3 || body[1] != 0 || binary.LittleEndian.Uint32(body[2:6]) != 3600 || body[6] != 0 || body[7] != 0 {
		t.Fatalf("active op1027 body = %x", body)
	}
	body = buildCollectboxStateBodyAt(definition, dnfcollectbox.State{}, true, expiry)
	if body[1] != 1 || binary.LittleEndian.Uint32(body[2:6]) != 0 || body[6] != 0 {
		t.Fatalf("expired op1027 body = %x", body)
	}
}

func TestCollectboxInitializationSelectsOrcGuardianAndIsIdempotent(t *testing.T) {
	if defaultCollectboxIndex != 5 {
		t.Fatalf("default collect-box index=%d, want 5 (兽人守护神)", defaultCollectboxIndex)
	}
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-3"}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{
				1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}},
				2: {Index: 2, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9002, Count: 1}}},
				5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
			},
			indexes: []byte{1, 2, 5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: &bufferConn{}, accountID: "account-3", selectedCharacterID: 29, connID: "collectbox-init-test"}
	if err := service.sendCollectboxInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if !session.collectboxInitializationSent {
		t.Fatal("collect-box initialization was not marked sent")
	}
	stream := session.conn.(*bufferConn).write.Bytes()
	for index, wantIndex := range []byte{1, 2, 5} {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID {
			t.Fatalf("packet[%d] header=%+v", index, packet.Header)
		}
		if packet.Body[0] != wantIndex || packet.Body[1] != 0 {
			t.Fatalf("packet[%d] body=%x, want index=%d refresh=0", index, packet.Body, wantIndex)
		}
	}
	selectionACK, stream := splitGameServerUpperPacket(t, stream)
	if selectionACK.Header.Classification != 1 ||
		selectionACK.Header.MsgID != uint16(dnfenum.CmdPacketSelectCollectbox) ||
		!bytes.Equal(selectionACK.Body, []byte{1, defaultCollectboxIndex}) {
		t.Fatalf("selection ACK header=%+v body=%x", selectionACK.Header, selectionACK.Body)
	}
	packet, stream := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID || packet.Body[0] != defaultCollectboxIndex || packet.Body[1] != 1 {
		t.Fatalf("selected state header=%+v body=%x", packet.Header, packet.Body)
	}
	if session.selectedCollectboxIndex != defaultCollectboxIndex {
		t.Fatalf("selected collect-box index=%d, want %d", session.selectedCollectboxIndex, defaultCollectboxIndex)
	}
	if len(stream) != 0 {
		t.Fatalf("unexpected trailing bytes=%x", stream)
	}
	written := session.conn.(*bufferConn).write.Len()
	if err := service.sendCollectboxInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() != written {
		t.Fatal("duplicate collect-box initialization emitted packets")
	}
	if err := service.sendCollectboxSceneInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	replayed := session.conn.(*bufferConn).write.Bytes()[written:]
	for index, wantIndex := range []byte{1, 2, 5} {
		packet, rest := splitGameServerUpperPacket(t, replayed)
		replayed = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID || packet.Body[0] != wantIndex || packet.Body[1] != 0 {
			t.Fatalf("scene replay packet[%d] header=%+v body=%x", index, packet.Header, packet.Body)
		}
	}
	selectionACK, replayed = splitGameServerUpperPacket(t, replayed)
	if selectionACK.Header.Classification != 1 ||
		selectionACK.Header.MsgID != uint16(dnfenum.CmdPacketSelectCollectbox) ||
		!bytes.Equal(selectionACK.Body, []byte{1, defaultCollectboxIndex}) {
		t.Fatalf("scene selection ACK header=%+v body=%x", selectionACK.Header, selectionACK.Body)
	}
	packet, replayed = splitGameServerUpperPacket(t, replayed)
	if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID || packet.Body[0] != defaultCollectboxIndex || packet.Body[1] != 1 {
		t.Fatalf("scene selected state header=%+v body=%x", packet.Header, packet.Body)
	}
	if len(replayed) != 0 {
		t.Fatalf("unexpected scene replay trailing bytes=%x", replayed)
	}
	sceneWritten := session.conn.(*bufferConn).write.Len()
	if err := service.sendCollectboxSceneInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() != sceneWritten {
		t.Fatal("duplicate collection-box projection emitted for the same actor generation")
	}
}

func TestCurrentCollectboxBonusProjectsIntoSceneStats(t *testing.T) {
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"5": {"0": 9005}}}
	raw, err := dnfcollectbox.EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-collectbox-stats",
		Metadata:  map[string]string{dnfcollectbox.MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{5: {
				Index:              5,
				BaseExpirationDate: "2037-01-05 06:00:00",
				Slots: []dnfcollectbox.Slot{{
					ItemID: 9005,
					Count:  1,
					Bonus: dnfcollectbox.Bonus{
						PhysicalAttack:    11,
						PhysicalDefense:   12,
						MagicalAttack:     13,
						MagicalDefense:    14,
						IndependentAttack: 15,
					},
				}},
			}},
			indexes: []byte{5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{accountID: "account-collectbox-stats", selectedCharacterID: 29}
	bonus, err := service.currentCollectboxBonus(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	stats := applyCurrentCollectboxBonus(dnfcharstat.Vector{
		PhysicalAttack:    100,
		PhysicalDefense:   200,
		MagicalAttack:     300,
		MagicalDefense:    400,
		IndependentAttack: 500,
	}, bonus)
	if stats.PhysicalAttack != 111 || stats.PhysicalDefense != 212 || stats.MagicalAttack != 313 || stats.MagicalDefense != 414 || stats.IndependentAttack != 515 {
		t.Fatalf("scene stats after collect-box projection = %+v", stats)
	}
}

func TestTownActorPrerequisitesProjectsCollectboxAfterMode1(t *testing.T) {
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"5": {"0": 9005}}}
	raw, err := dnfcollectbox.EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-town-collectbox",
		Metadata:  map[string]string{dnfcollectbox.MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{
				1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}},
				5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
			},
			indexes: []byte{1, 5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                        connection,
		accountID:                   "account-town-collectbox",
		selectedCharacterID:         29,
		initialTownRouteCharacterID: 29,
		initialTownRouteStage:       currentInitialTownRouteArmed,
	}
	if err := service.sendCurrentTownActorPrerequisitesLocked(session, 29, []byte{0}, []byte{1}); err != nil {
		t.Fatal(err)
	}

	mode1Index, cacheIndex, selectionIndex, selectedIndex := -1, -1, -1, -1
	stream := connection.write.Bytes()
	packetIndex := 0
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		switch {
		case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && bytes.Equal(packet.Body, []byte{1}):
			mode1Index = packetIndex
		case packet.Header.Classification == 0 && packet.Header.MsgID == collectboxNotificationMsgID && len(packet.Body) >= 8 && packet.Body[1] == 0:
			cacheIndex = packetIndex
		case packet.Header.Classification == 1 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectCollectbox):
			selectionIndex = packetIndex
		case packet.Header.Classification == 0 && packet.Header.MsgID == collectboxNotificationMsgID && len(packet.Body) >= 12 && packet.Body[0] == defaultCollectboxIndex && packet.Body[1] == 1:
			if binary.LittleEndian.Uint32(packet.Body[8:12]) != 9005 {
				t.Fatalf("selected collection-box item body=%x", packet.Body)
			}
			selectedIndex = packetIndex
		}
		packetIndex++
		stream = rest
	}
	if !(mode1Index >= 0 && mode1Index < cacheIndex && cacheIndex < selectionIndex && selectionIndex < selectedIndex) {
		t.Fatalf("town collection-box order mode1=%d cache=%d select=%d selected=%d", mode1Index, cacheIndex, selectionIndex, selectedIndex)
	}
	if !session.collectboxSceneProjectionSent {
		t.Fatal("town actor generation did not commit collection-box projection")
	}
}

// collectboxSelectionFixture builds the three-box catalog shape the live PVF
// uses (1 = SAO, 2 = 生肖之灵, 5 = 兽人守护神) plus the durable state for one
// character. Box 1 carries two slots.
func collectboxSelectionFixture(t *testing.T, accountID string, state dnfcollectbox.State) (*Service, *gameSession, dnfrepo.Group) {
	t.Helper()
	raw, err := dnfcollectbox.EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: accountID,
		Metadata:  map[string]string{dnfcollectbox.MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{
				1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{
					{ItemID: 9001, Count: 1},
					{ItemID: 9002, Count: 1},
				}},
				2: {Index: 2, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9003, Count: 1}}},
				5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
			},
			indexes: []byte{1, 2, 5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                          &bufferConn{},
		accountID:                     accountID,
		selectedCharacterID:           29,
		connID:                        "collectbox-selection-test",
		collectboxSceneProjectionSent: true,
	}
	return service, session, repositories
}

func collectboxStatePackets(t *testing.T, stream []byte) (cache []byte, refresh []byte, ack []byte) {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		switch {
		case packet.Header.Classification == 1 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectCollectbox):
			ack = append(ack, packet.Body...)
		case packet.Header.Classification == 0 && packet.Header.MsgID == collectboxNotificationMsgID:
			if packet.Body[1] == 1 {
				refresh = append(refresh, packet.Body...)
			} else {
				cache = append(cache, packet.Body...)
			}
		default:
			t.Fatalf("unexpected collect-box packet header=%+v", packet.Header)
		}
	}
	return cache, refresh, ack
}

// A box that holds collected items must never be published cache-only and must
// be the box the login/actor generation selects. Live 2026-09-27: box 1 held 15
// items, every 小退 re-selected the empty box 5, and the collection-box
// attributes only returned after the player clicked box 1 by hand.
func TestCollectboxInitializationSelectsAndRefreshesPopulatedBox(t *testing.T) {
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"1": {"0": 9001, "1": 9002}}}
	service, session, _ := collectboxSelectionFixture(t, "account-populated-box", state)
	if err := service.sendCollectboxInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	cache, refresh, ack := collectboxStatePackets(t, session.conn.(*bufferConn).write.Bytes())
	// All three boxes populate the cache (16-byte box-1 state + two 8-byte empty
	// states), none is published as a refresh before the explicit selection.
	if len(cache) != 32 || cache[0] != 1 || cache[1] != 0 || cache[16] != 2 || cache[17] != 0 || cache[24] != 5 || cache[25] != 0 {
		t.Fatalf("cache pass = %x", cache)
	}
	if !bytes.Equal(ack, []byte{1, 1}) {
		t.Fatalf("selection ACK = %x, want box 1", ack)
	}
	if len(refresh) != 16 || refresh[0] != 1 || refresh[1] != 1 || refresh[7] != 2 {
		t.Fatalf("selected refresh = %x", refresh)
	}
	if got := binary.LittleEndian.Uint32(refresh[8:12]); got != 9001 {
		t.Fatalf("selected refresh slot 0 = %d", got)
	}
	if got := binary.LittleEndian.Uint32(refresh[12:16]); got != 9002 {
		t.Fatalf("selected refresh slot 1 = %d", got)
	}
	if session.selectedCollectboxIndex != 1 || session.selectedCollectboxCharacterID != 29 {
		t.Fatalf("session selection = box %d char %d", session.selectedCollectboxIndex, session.selectedCollectboxCharacterID)
	}
}

// Every other populated box is refreshed before the selected box so a
// multi-box account does not leave one of them cache-only, while the window and
// its stat owner still end on the selected box.
func TestCollectboxInitializationRefreshesEveryPopulatedBox(t *testing.T) {
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{
		"1": {"0": 9001},
		"5": {"0": 9005},
	}}
	service, session, _ := collectboxSelectionFixture(t, "account-two-populated-boxes", state)
	// The player last stood on 兽人守护神; that selection must win while it still
	// holds items.
	session.selectedCollectboxIndex = 5
	session.selectedCollectboxCharacterID = 29
	if err := service.sendCollectboxInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	stream := session.conn.(*bufferConn).write.Bytes()
	var order []string
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		switch {
		case packet.Header.Classification == 1:
			order = append(order, "ack:"+string(rune('0'+packet.Body[1])))
		case packet.Body[1] == 1:
			order = append(order, "refresh:"+string(rune('0'+packet.Body[0])))
		default:
			order = append(order, "cache:"+string(rune('0'+packet.Body[0])))
		}
	}
	want := []string{"cache:1", "cache:2", "cache:5", "refresh:1", "ack:5", "refresh:5"}
	if len(order) != len(want) {
		t.Fatalf("packet order = %v, want %v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("packet order = %v, want %v", order, want)
		}
	}
	if session.selectedCollectboxIndex != 5 {
		t.Fatalf("selected box = %d, want the remembered populated box 5", session.selectedCollectboxIndex)
	}
}

// The native client applies the attributes of the selected box only, so the
// selection is player intent: a recorded choice wins even when another box holds
// far more items. Deriving it from item counts moved a player who had picked
// 生肖之灵 (2 items) back onto SAO (15 items) on every re-entry.
func TestCurrentCollectboxActiveIndexPrefersRecordedSelection(t *testing.T) {
	service, session, _ := collectboxSelectionFixture(t, "account-active-index", dnfcollectbox.State{})
	catalog, err := service.currentCollectboxCatalog()
	if err != nil {
		t.Fatal(err)
	}
	empty := dnfcollectbox.State{}
	if got := currentCollectboxActiveIndex(catalog, empty, session); got != defaultCollectboxIndex {
		t.Fatalf("untouched account active box = %d, want default %d", got, defaultCollectboxIndex)
	}
	// The session's own choice wins, even while the chosen box is empty.
	session.selectedCollectboxIndex = 5
	session.selectedCollectboxCharacterID = session.selectedCharacterID
	if got := currentCollectboxActiveIndex(catalog, empty, session); got != 5 {
		t.Fatalf("session selection = %d, want 5", got)
	}
	// A durable per-character choice wins over the most populated box.
	durable := dnfcollectbox.State{
		Boxes:    map[string]map[string]uint32{"1": {"0": 9001, "1": 9002}},
		Selected: map[string]byte{"29": 5},
	}
	session.selectedCollectboxIndex = 0
	session.selectedCollectboxCharacterID = 0
	if got := currentCollectboxActiveIndex(catalog, durable, session); got != 5 {
		t.Fatalf("durable selection = %d, want 5 (box 1 is more populated)", got)
	}
	// Only an account that never picked a box is derived from item counts.
	derived := dnfcollectbox.State{Boxes: map[string]map[string]uint32{
		"1": {"0": 9001, "1": 9002},
		"5": {"0": 9005},
	}}
	if got := currentCollectboxActiveIndex(catalog, derived, session); got != 1 {
		t.Fatalf("most-populated box = %d, want 1", got)
	}
	// A stale record for a box the PVF no longer defines falls back to it.
	stale := dnfcollectbox.State{Selected: map[string]byte{"29": 9}}
	if got := currentCollectboxActiveIndex(catalog, stale, session); got != defaultCollectboxIndex {
		t.Fatalf("stale selection = %d, want default %d", got, defaultCollectboxIndex)
	}
}

// A persisted selection is what the login publication must select and refresh,
// even when a different box holds many more items.
func TestCollectboxInitializationRestoresPersistedSelection(t *testing.T) {
	state := dnfcollectbox.State{
		Boxes: map[string]map[string]uint32{
			"1": {"0": 9001, "1": 9002},
			"5": {"0": 9005},
		},
		Selected: map[string]byte{"29": 5},
	}
	service, session, _ := collectboxSelectionFixture(t, "account-persisted-selection", state)
	if err := service.sendCollectboxInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	stream := session.conn.(*bufferConn).write.Bytes()
	var order []string
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		switch {
		case packet.Header.Classification == 1:
			order = append(order, "ack:"+string(rune('0'+packet.Body[1])))
		case packet.Body[1] == 1:
			order = append(order, "refresh:"+string(rune('0'+packet.Body[0])))
		default:
			order = append(order, "cache:"+string(rune('0'+packet.Body[0])))
		}
	}
	// Box 1 is refreshed because it holds items, but the window and the stat
	// owner must end on the persisted box 5.
	want := []string{"cache:1", "cache:2", "cache:5", "refresh:1", "ack:5", "refresh:5"}
	if len(order) != len(want) {
		t.Fatalf("packet order = %v, want %v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("packet order = %v, want %v", order, want)
		}
	}
	if session.selectedCollectboxIndex != 5 {
		t.Fatalf("selected box = %d, want persisted 5", session.selectedCollectboxIndex)
	}
}

func TestHandleCollectboxSelectPersistsSelection(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{CharacterID: "29", AccountID: "account-select-persist"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-select-persist"}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{
				1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}},
				2: {Index: 2, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9002, Count: 1}}},
				5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
			},
			indexes: []byte{1, 2, 5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                &bufferConn{},
		accountID:           "account-select-persist",
		selectedCharacterID: 29,
		connID:              "collectbox-select-persist-test",
	}
	catalog, err := service.currentCollectboxCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCollectboxSelect(context.Background(), session, catalog, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if session.selectedCollectboxIndex != 2 {
		t.Fatalf("session selection = %d, want 2", session.selectedCollectboxIndex)
	}
	stored, found, err := dnfcollectbox.LoadAccountState(context.Background(), repositories.Account, "account-select-persist")
	if err != nil || !found {
		t.Fatalf("stored state found=%t err=%v", found, err)
	}
	if got := stored.SelectedBox("29"); got != 2 {
		t.Fatalf("persisted selection = %d, want 2", got)
	}
	// A second entry for the same character restores it instead of a default.
	next := &gameSession{conn: &bufferConn{}, accountID: "account-select-persist", selectedCharacterID: 29, connID: "collectbox-reselect-test"}
	if err := service.sendCollectboxInitializationStates(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if next.selectedCollectboxIndex != 2 {
		t.Fatalf("re-entry selection = %d, want the persisted 2", next.selectedCollectboxIndex)
	}
}

// A dungeon round trip or any other actor-generation replay must keep the
// player's box: a derived default here is what produced "刷一次图出来就变成
// 兽人守护神".
func TestDungeonSceneResetKeepsPersistedCollectboxSelection(t *testing.T) {
	state := dnfcollectbox.State{
		Boxes:    map[string]map[string]uint32{"1": {"0": 9001}},
		Selected: map[string]byte{"29": 2},
	}
	service, session, _ := collectboxSelectionFixture(t, "account-dungeon-roundtrip", state)
	for round := 0; round < 3; round++ {
		resetDungeonEntrySceneGates(session)
		if err := service.sendCollectboxSceneInitializationStates(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		if session.selectedCollectboxIndex != 2 {
			t.Fatalf("round %d selected box = %d, want persisted 2", round, session.selectedCollectboxIndex)
		}
	}
}

func TestCollectboxReplayAfterMode1RefreshesSelectedBoxOnce(t *testing.T) {
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"1": {"0": 9001}}}
	service, session, _ := collectboxSelectionFixture(t, "account-mode1-replay", state)
	if err := service.replayCollectboxSelectedRefreshAfterMode1(context.Background(), session, "test_after_full_mode1"); err != nil {
		t.Fatal(err)
	}
	written := session.conn.(*bufferConn).write.Len()
	packet, trailing := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID ||
		len(trailing) != 0 || len(packet.Body) != 12 || packet.Body[0] != 1 || packet.Body[1] != 1 {
		t.Fatalf("post-mode1 refresh header=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[8:12]); got != 9001 {
		t.Fatalf("post-mode1 refresh item = %d", got)
	}
	if !session.collectboxSelectionRefreshSent {
		t.Fatal("post-mode1 refresh did not latch the actor generation")
	}
	// A second call in the same actor generation must not append a packet.
	if err := service.replayCollectboxSelectedRefreshAfterMode1(context.Background(), session, "test_after_full_mode1_again"); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() != written {
		t.Fatal("post-mode1 refresh repeated inside one actor generation")
	}
	// A replacement actor generation replays it again.
	session.collectboxSelectionRefreshSent = false
	if err := service.replayCollectboxSelectedRefreshAfterMode1(context.Background(), session, "test_next_actor_generation"); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() == written {
		t.Fatal("replacement actor generation did not replay the selected refresh")
	}
}
