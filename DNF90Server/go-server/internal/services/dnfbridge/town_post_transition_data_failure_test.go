package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type failingTownPostEquipmentRepository struct {
	dnfrepo.EquipmentRepository
	err error
}

func (repository *failingTownPostEquipmentRepository) Load(
	context.Context,
	string,
) (dnfrepo.EquipmentRecord, bool, error) {
	return dnfrepo.EquipmentRecord{}, false, repository.err
}

type failingTownPostCharacterRepository struct {
	dnfrepo.CharacterRepository
	err error
}

func (repository *failingTownPostCharacterRepository) Load(
	context.Context,
	string,
) (dnfrepo.CharacterRecord, bool, error) {
	return dnfrepo.CharacterRecord{}, false, repository.err
}

func TestTownPostTransitionDoesNotAdvanceMode0OnEquipmentLoadFailure(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	wantErr := errors.New("equipment load failed")
	failing := repositories
	failing.Equipment = &failingTownPostEquipmentRepository{
		EquipmentRepository: repositories.Equipment,
		err:                 wantErr,
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return failing, true }
	service.armCurrentTownPostTransitionPlayerState(session, "test_equipment_failure")
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	err := service.sendCurrentTownPostTransitionPlayerState(session, "test_equipment_failure")
	if !errors.Is(err, wantErr) {
		t.Fatalf("equipment failure=%v want=%v", err, wantErr)
	}
	if session.townPostTransition.stage != currentTownPostTransitionPending {
		t.Fatalf("equipment failure advanced stage=%d", session.townPostTransition.stage)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("equipment failure wrote stale mode0=%x", connection.write.Bytes())
	}

	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	if err := service.sendCurrentTownPostTransitionPlayerState(session, "test_equipment_resume"); err != nil {
		t.Fatalf("equipment resume: %v", err)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"mode0", "mode1", "op105", "op37", "op30", "op559", "op120"},
	) {
		t.Fatalf("equipment resume=%v", got)
	}
	projection, found := currentPartyActorProjectionSnapshot(
		session,
		session.selectedCharacterID,
		session.characterGeneration,
	)
	if !found {
		t.Fatal("town post-transition did not freeze the owner-produced party actor projection")
	}
	var sentMode0, sentMode1 []byte
	for _, packet := range splitAllCurrentUpperPackets(t, connection.write.Bytes()) {
		if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(packet.Body) == 0 {
			continue
		}
		switch packet.Body[0] {
		case 0:
			sentMode0 = packet.Body
		case 1:
			sentMode1 = packet.Body
		}
	}
	if !bytes.Equal(projection.mode0, sentMode0) || !bytes.Equal(projection.mode1, sentMode1) {
		t.Fatalf("party actor projection differs from successful wire rows mode0=%t mode1=%t",
			bytes.Equal(projection.mode0, sentMode0), bytes.Equal(projection.mode1, sentMode1))
	}
}

func TestTownPostTransitionRestoresDurableBuffRegistrationAfterFinalMode1(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	ctx := context.Background()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: 101010864, Count: 1, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save BUFF inventory: %v", err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "29", skillSwitchInventoryState{
		SkillID: 46,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: 101010864}},
	}); err != nil {
		t.Fatalf("save BUFF registration: %v", err)
	}
	service.armCurrentTownPostTransitionPlayerState(session, "test_buff_registration")
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendCurrentTownPostTransitionPlayerState(session, "test_buff_registration"); err != nil {
		t.Fatal(err)
	}

	mode1Index, buffIndex, placementIndex := -1, -1, -1
	for index, packet := range splitAllCurrentUpperPackets(t, connection.write.Bytes()) {
		switch {
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 && packet.Body[0] == 1:
			mode1Index = index
		case packet.Header.Classification == 0 && packet.Header.MsgID == skillSwitchInventoryAllDataWire:
			buffIndex = index
			if want := []byte{46, 0, 1, 1, 12, 0, 17, 0}; !bytes.Equal(packet.Body, want) {
				t.Fatalf("post-op24 BUFF registration=%x want=%x", packet.Body, want)
			}
		case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketRequestBlacklist):
			placementIndex = index
		}
	}
	if !(mode1Index >= 0 && mode1Index < buffIndex && buffIndex < placementIndex) {
		t.Fatalf("post-op24 BUFF registration order mode1=%d buff=%d placement=%d", mode1Index, buffIndex, placementIndex)
	}
}

func TestTownPostTransitionDoesNotAdvanceCreatureStageOnEquipmentLoadFailure(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	wantErr := errors.New("creature equipment load failed")
	failing := repositories
	failing.Equipment = &failingTownPostEquipmentRepository{
		EquipmentRepository: repositories.Equipment,
		err:                 wantErr,
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return failing, true }
	armTownPostTransitionAtStage(session, currentTownPostTransitionCreatureTableSent)
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	err := service.sendCurrentTownPostTransitionPlayerState(session, "test_creature_failure")
	if !errors.Is(err, wantErr) {
		t.Fatalf("creature failure=%v want=%v", err, wantErr)
	}
	if session.townPostTransition.stage != currentTownPostTransitionCreatureTableSent {
		t.Fatalf("creature failure advanced stage=%d", session.townPostTransition.stage)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("creature failure wrote suffix=%x", connection.write.Bytes())
	}

	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	if err := service.sendCurrentTownPostTransitionPlayerState(session, "test_creature_resume"); err != nil {
		t.Fatalf("creature resume: %v", err)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op37", "op30", "op559", "op120"},
	) {
		t.Fatalf("creature resume=%v", got)
	}
}

func TestTownPostTransitionSuppressesSkillAndExpertJobAcquisitionReplays(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	session.expertJobInfoCharacterID = session.selectedCharacterID
	service.armCurrentTownPostTransitionPlayerState(session, "test_expert_job_rebind")
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendCurrentTownPostTransitionPlayerState(session, "test_expert_job_rebind"); err != nil {
		t.Fatal(err)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 7 {
		t.Fatalf("scene-rebuild packet count=%d signatures=%v", len(packets), townPostTransitionPacketSignatures(t, connection.write.Bytes()))
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"mode0", "mode1", "op105", "op37", "op30", "op559", "op120"},
	) {
		t.Fatalf("scene-rebuild order signatures=%v", got)
	}
	for _, packet := range packets {
		if packet.Header.Classification == 0 &&
			(packet.Header.MsgID == currentSkillInfoMsgID || packet.Header.MsgID == currentExpertJobInfoNotification) {
			t.Fatalf("scene reconstruction replayed acquisition-sensitive packet msg=%d", packet.Header.MsgID)
		}
	}
}

func TestTownPostTransitionDoesNotRequireSkillCatalogForSuppressedReplay(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	catalog := service.skillCatalog
	service.skillCatalog = nil
	armTownPostTransitionAtStage(session, currentTownPostTransitionFinishCompletionSent)
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendCurrentTownPostTransitionPlayerState(session, "test_skill_missing"); err != nil {
		t.Fatalf("suppressed skill replay required catalog: %v", err)
	}
	if session.townPostTransition.stage != currentTownPostTransitionComplete {
		t.Fatalf("suppressed skill replay did not complete stage=%d", session.townPostTransition.stage)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op559", "op120"},
	) {
		t.Fatalf("suppressed skill replay suffix=%v", got)
	}
	service.skillCatalog = catalog
}

func armTownPostTransitionAtStage(
	session *gameSession,
	stage currentTownPostTransitionStage,
) {
	session.townPostTransition.characterID = session.selectedCharacterID
	session.townPostTransition.ownerChannel = currentTownActorOwnerContext(session)
	session.townPostTransition.stage = stage
	session.townPostTransition.source = "test_manual_stage"
}
