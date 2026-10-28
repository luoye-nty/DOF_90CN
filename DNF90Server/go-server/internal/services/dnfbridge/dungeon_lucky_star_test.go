package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	dnfrental "longheng.io/server/internal/modules/dnf/rental"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestSendSuitableDungeonLuckyStarStateSendsCommittedStateOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{dnfrental.PointMetadataKey: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{
		conn:                connection,
		accountID:           "account-1",
		selectedCharacterID: 19,
	}
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{ID: 244},
		Character: dnfrepo.CharacterRecord{
			CharacterID: "19",
			AccountID:   "account-1",
			Level:       90,
		},
		settlementLuckyStarResolved:       true,
		settlementLuckyStarRefreshPending: true,
		settlementLuckyStarBefore:         0,
		settlementLuckyStarApplied:        1,
		settlementLuckyStarAfter:          1,
		settlementLuckyStarLimit:          400,
	}

	service.sendSuitableDungeonLuckyStarState(session, runtime)
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 1 || packet.Header.MsgID != currentRentalPointResultMsgID {
		t.Fatalf("lucky-star notice class=%d msg=%d want class1/op%d", packet.Header.Classification, packet.Header.MsgID, currentRentalPointResultMsgID)
	}
	if len(packet.Body) != 13 || packet.Body[0] != 1 ||
		binary.LittleEndian.Uint32(packet.Body[1:5]) != currentRentalPointSourceDungeonClear ||
		binary.LittleEndian.Uint32(packet.Body[5:9]) != 0 ||
		binary.LittleEndian.Uint32(packet.Body[9:13]) != 1 {
		t.Fatalf("lucky-star notice body=%x want success,type2,not-capped,points1", packet.Body)
	}
	packet, rest = splitGameServerUpperPacket(t, rest)
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentRentalStateMsgID {
		t.Fatalf("lucky-star refresh class=%d msg=%d want class0/op%d", packet.Header.Classification, packet.Header.MsgID, currentRentalStateMsgID)
	}
	if len(rest) != 0 {
		next, _ := splitGameServerUpperPacket(t, rest)
		t.Fatalf("unexpected lucky-star trailing packet msg=%d", next.Header.MsgID)
	}
	if len(packet.Body) != 8 || binary.LittleEndian.Uint32(packet.Body[:4]) != 1 ||
		binary.LittleEndian.Uint32(packet.Body[4:8]) != 0 {
		t.Fatalf("rental state body=%x want points=1 active_count=0", packet.Body)
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	points, err := dnfrental.Points(account)
	if err != nil || points != 1 {
		t.Fatalf("committed rental points=%d err=%v", points, err)
	}

	written := connection.write.Len()
	service.sendSuitableDungeonLuckyStarState(session, runtime)
	account, _, err = repositories.Account.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	points, err = dnfrental.Points(account)
	if err != nil || points != 1 || connection.write.Len() != written {
		t.Fatalf("replay points=%d bytes_before=%d bytes_after=%d err=%v", points, written, connection.write.Len(), err)
	}
}

func TestSendSuitableDungeonLuckyStarStateRetriesDeferredActiveAssetLoad(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{dnfrental.PointMetadataKey: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	baseInventory := repositories.Inventory
	repositories.Inventory = rentalFailingInventoryLoadRepository{
		InventoryRepository: baseInventory,
		err:                 errors.New("forced rental inventory read failure"),
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{
		conn:                          connection,
		accountID:                     "account-1",
		selectedCharacterID:           19,
		selectedRentalWalletStateSent: true,
	}
	runtime := &runtimeDungeonState{
		Dungeon:                           worldmap.Dungeon{ID: 244},
		settlementLuckyStarResolved:       true,
		settlementLuckyStarRefreshPending: true,
		settlementLuckyStarNoticeSent:     true,
		settlementLuckyStarBefore:         0,
		settlementLuckyStarApplied:        1,
		settlementLuckyStarAfter:          1,
		settlementLuckyStarLimit:          400,
	}

	service.sendSuitableDungeonLuckyStarState(session, runtime)
	if connection.write.Len() != 0 {
		t.Fatalf("deferred active-asset load emitted op985: %x", connection.write.Bytes())
	}
	if runtime.settlementLuckyStarStateSent || session.selectedRentalWalletStateSent {
		t.Fatalf("deferred active-asset load closed retry gates runtime=%t session=%t",
			runtime.settlementLuckyStarStateSent, session.selectedRentalWalletStateSent)
	}

	repositories.Inventory = baseInventory
	service.sendSuitableDungeonLuckyStarState(session, runtime)
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentRentalStateMsgID {
		t.Fatalf("retry packet header=%+v trailing=%x", packet.Header, rest)
	}
	if !runtime.settlementLuckyStarStateSent || !session.selectedRentalWalletStateSent {
		t.Fatalf("successful retry left gates open runtime=%t session=%t",
			runtime.settlementLuckyStarStateSent, session.selectedRentalWalletStateSent)
	}
}

func TestRecommendedDungeonSettlementRefreshesRentalStateAfterResultChain(t *testing.T) {
	service, runtime, session, connection, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Account == nil {
		t.Fatal("settlement fixture has no account repository")
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{dnfrental.PointMetadataKey: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character.Level = 20
	runtime.Dungeon.Metadata.MinimumRequiredLevel.Set = true
	runtime.Dungeon.Metadata.MinimumRequiredLevel.Value = 10
	runtime.Dungeon.Metadata.BasisLevel.Set = true
	runtime.Dungeon.Metadata.BasisLevel.Value = 20
	// The domain settlement owns persistence. This test isolates the socket
	// order after that commit without requiring a real PVF archive fixture.
	runtime.settlementLuckyStarResolved = true
	runtime.settlementLuckyStarRefreshPending = true
	runtime.settlementLuckyStarBefore = 0
	runtime.settlementLuckyStarApplied = 1
	runtime.settlementLuckyStarAfter = 1
	runtime.settlementLuckyStarLimit = 400
	connection.write.Reset()

	const clientRankPoint = byte(73)
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, clientRankPoint)
	runtime.settlementResultPlan = &plan
	request := make([]byte, currentDungeonPlayResultBaseSize)
	request[currentDungeonPlayResultClientRankPointOffset] = clientRankPoint
	if err := service.handleDungeonSetPlayResult(session, request); err != nil {
		t.Fatal(err)
	}

	want := []struct {
		classification byte
		msgID          uint16
	}{
		{classification: 0, msgID: currentDungeonPlayResultNoticeMsgID},
		{classification: 0, msgID: currentDungeonCharacterStateMsgID},
		{classification: 0, msgID: currentDungeonClearRewardMsgID},
		{classification: 1, msgID: currentRentalPointResultMsgID},
		{classification: 0, msgID: currentRentalStateMsgID},
	}
	stream := connection.write.Bytes()
	for index, expected := range want {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification != expected.classification || packet.Header.MsgID != expected.msgID {
			t.Fatalf("packet[%d] class=%d msg=%d want class%d/msg%d", index, packet.Header.Classification, packet.Header.MsgID, expected.classification, expected.msgID)
		}
		stream = rest
	}
	if len(stream) != 0 {
		packet, _ := splitGameServerUpperPacket(t, stream)
		t.Fatalf("unexpected settlement trailing packet msg=%d", packet.Header.MsgID)
	}
}

func TestDungeonLuckyStarResultMarksLimitOnlyAtCap(t *testing.T) {
	for _, test := range []struct {
		name          string
		before        uint32
		applied       uint32
		after         uint32
		wantLimitFlag uint32
	}{
		{name: "below cap", before: 398, applied: 1, after: 399, wantLimitFlag: 0},
		{name: "reaches cap", before: 399, applied: 1, after: 400, wantLimitFlag: 1},
		{name: "already capped", before: 400, applied: 0, after: 400, wantLimitFlag: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
			session := &gameSession{conn: connection, selectedCharacterID: 19}
			runtime := &runtimeDungeonState{
				Dungeon:                           worldmap.Dungeon{ID: 244},
				settlementLuckyStarResolved:       true,
				settlementLuckyStarRefreshPending: true,
				settlementLuckyStarBefore:         test.before,
				settlementLuckyStarApplied:        test.applied,
				settlementLuckyStarAfter:          test.after,
				settlementLuckyStarLimit:          400,
			}

			// Stop after the native notice. The state refresh has a repository
			// dependency which is covered by the end-to-end test above.
			runtime.settlementLuckyStarStateSent = true
			service.sendSuitableDungeonLuckyStarState(session, runtime)
			packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(rest) != 0 || packet.Header.Classification != 1 || packet.Header.MsgID != currentRentalPointResultMsgID {
				t.Fatalf("notice header=%+v trailing=%d", packet.Header, len(rest))
			}
			if len(packet.Body) != 13 || packet.Body[0] != 1 ||
				binary.LittleEndian.Uint32(packet.Body[1:5]) != currentRentalPointSourceDungeonClear ||
				binary.LittleEndian.Uint32(packet.Body[5:9]) != test.wantLimitFlag ||
				binary.LittleEndian.Uint32(packet.Body[9:13]) != test.after {
				t.Fatalf("notice body=%x", packet.Body)
			}
		})
	}
}
