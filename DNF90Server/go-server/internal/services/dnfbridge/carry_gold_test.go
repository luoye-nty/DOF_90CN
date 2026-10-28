package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfcarrygold "longheng.io/server/internal/modules/dnf/carrygold"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// currentCarryGoldRequest reproduces the client builder: 14 bytes of stack
// residue whose only initialized field is the trailing requested stage.
func currentCarryGoldRequest(stage byte) []byte {
	body := []byte{0x50, 0x8F, 0xEC, 0x3A, 0, 0, 0, 0, 0xD8, 0xF8, 0x19, 0x00, 0x30, 0x00}
	body[currentCarryGoldStageOffset] = stage
	return body
}

func newCarryGoldTest(t *testing.T, gold int64, stage int64) (*Service, *gameSession, dnfrepo.Group, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	stats := map[string]int64{"gold": gold}
	if stage > 0 {
		stats[dnfcarrygold.StageStat] = stage
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Level: 90, Stats: stats,
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	return service, &gameSession{conn: connection, selectedCharacterID: 17}, repositories, connection
}

func assertCarryGoldReply(t *testing.T, connection *bufferConn, wantStage byte, wantResult uint32) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketUpgradeCarryGold) ||
		packet.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("carry-gold reply header=%+v", packet.Header)
	}
	// The client requires a non-zero envelope byte, then reads u8 stage and u32
	// result; a non-zero result takes its failure branch.
	if len(packet.Body) != 1+currentCarryGoldResponseSize || packet.Body[0] != 1 ||
		packet.Body[1] != wantStage ||
		binary.LittleEndian.Uint32(packet.Body[2:6]) != wantResult {
		t.Fatalf("carry-gold reply body=%x want stage=%d result=%d", packet.Body, wantStage, wantResult)
	}
	return rest
}

func TestUpgradeCarryGoldChargesGoldAndAdvancesOneStage(t *testing.T) {
	service, session, repositories, connection := newCarryGoldTest(t, 250_000_000, 0)
	if err := service.handleCurrentUpgradeCarryGold(session, currentCarryGoldRequest(1)); err != nil {
		t.Fatal(err)
	}
	rest := assertCarryGoldReply(t, connection, 1, currentCarryGoldResultOK)
	// The wallet row is the client's only gold display, so the charge is
	// republished right after the result.
	walletUpdate, trailing := splitGameServerUpperPacket(t, rest)
	if walletUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || len(trailing) != 0 {
		t.Fatalf("wallet refresh header=%+v trailing=%x", walletUpdate.Header, trailing)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "17")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if stored.Stats["gold"] != 250_000_000-dnfcarrygold.StageCost {
		t.Fatalf("gold after upgrade = %d", stored.Stats["gold"])
	}
	if stored.Stats[dnfcarrygold.StageStat] != 1 {
		t.Fatalf("stage after upgrade = %d", stored.Stats[dnfcarrygold.StageStat])
	}
	// The purchased stage has to move the limit that gold gains are measured
	// against, otherwise the upgrade is a number nobody reads.
	if got := stored.Stats[dnfcarrygold.StageStat]; dnfcarrygold.StageBonus(byte(got)) != dnfcarrygold.Unit {
		t.Fatalf("carry limit bonus after upgrade = %d", dnfcarrygold.StageBonus(byte(got)))
	}
}

func TestUpgradeCarryGoldRefusesWithoutMutationWhenGoldIsShort(t *testing.T) {
	service, session, repositories, connection := newCarryGoldTest(t, dnfcarrygold.StageCost-1, 0)
	if err := service.handleCurrentUpgradeCarryGold(session, currentCarryGoldRequest(1)); err != nil {
		t.Fatal(err)
	}
	if rest := assertCarryGoldReply(t, connection, 0, currentCarryGoldResultInsufficientGold); len(rest) != 0 {
		t.Fatalf("insufficient-gold reply trailing=%x", rest)
	}
	stored, _, _ := repositories.Character.Load(context.Background(), "17")
	if stored.Stats["gold"] != dnfcarrygold.StageCost-1 ||
		stored.Stats[dnfcarrygold.StageStat] != 0 {
		t.Fatalf("refused upgrade mutated character stats=%+v", stored.Stats)
	}
}

func TestUpgradeCarryGoldRejectsOutOfSequenceAndExhaustedStages(t *testing.T) {
	// A stage that is not exactly current+1 must not skip ahead.
	service, session, repositories, connection := newCarryGoldTest(t, 900_000_000, 1)
	if err := service.handleCurrentUpgradeCarryGold(session, currentCarryGoldRequest(3)); err != nil {
		t.Fatal(err)
	}
	assertCarryGoldReply(t, connection, 1, currentCarryGoldResultInvalidStage)
	stored, _, _ := repositories.Character.Load(context.Background(), "17")
	if stored.Stats["gold"] != 900_000_000 || stored.Stats[dnfcarrygold.StageStat] != 1 {
		t.Fatalf("out-of-sequence upgrade mutated stats=%+v", stored.Stats)
	}

	// The client stops offering upgrades at stage 4; the server refuses too.
	service, session, repositories, connection = newCarryGoldTest(t, 900_000_000, int64(dnfcarrygold.MaxStage))
	if err := service.handleCurrentUpgradeCarryGold(
		session, currentCarryGoldRequest(dnfcarrygold.MaxStage+1),
	); err != nil {
		t.Fatal(err)
	}
	assertCarryGoldReply(t, connection, dnfcarrygold.MaxStage, currentCarryGoldResultInvalidStage)
	stored, _, _ = repositories.Character.Load(context.Background(), "17")
	if stored.Stats["gold"] != 900_000_000 {
		t.Fatalf("exhausted upgrade charged gold stats=%+v", stored.Stats)
	}
}

func TestUpgradeCarryGoldAlwaysAnswersAMalformedRequest(t *testing.T) {
	service, session, _, connection := newCarryGoldTest(t, 900_000_000, 0)
	if err := service.handleCurrentUpgradeCarryGold(session, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// A silent drop is exactly what left the confirm button dead, so even an
	// unparsable request gets the failure branch.
	assertCarryGoldReply(t, connection, 0, currentCarryGoldResultInvalidStage)
}

func TestSendCurrentCarryGoldStagePublishesTheDurableStage(t *testing.T) {
	service, session, _, connection := newCarryGoldTest(t, 0, 3)
	if err := service.sendCurrentCarryGoldStage(session, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacketWithHeader(
		t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16,
	)
	if packet.Header.MsgID != currentCarryGoldStageMsgID || packet.Header.Classification != 0 ||
		len(packet.Body) != 1 || packet.Body[0] != 3 || len(trailing) != 0 {
		t.Fatalf("carry-gold stage packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
}

func TestCarryGoldGameplayModuleRoutesLegacyAndUpperOp978(t *testing.T) {
	opcode := uint16(dnfenum.CmdPacketUpgradeCarryGold)
	service, session, _, connection := newCarryGoldTest(t, 250_000_000, 0)
	handled, err := currentGameplayModules.DispatchLegacy(service, session, opcode, currentCarryGoldRequest(1))
	if err != nil || !handled {
		t.Fatalf("legacy dispatch handled=%t err=%v", handled, err)
	}
	assertCarryGoldReply(t, connection, 1, currentCarryGoldResultOK)

	service, session, _, connection = newCarryGoldTest(t, 250_000_000, 0)
	handled, err = currentGameplayModules.DispatchUpper(
		service, session, opcode, dnfproto.DefaultChannelClassification, currentCarryGoldRequest(1),
	)
	if err != nil || !handled {
		t.Fatalf("upper dispatch handled=%t err=%v", handled, err)
	}
	assertCarryGoldReply(t, connection, 1, currentCarryGoldResultOK)
}
