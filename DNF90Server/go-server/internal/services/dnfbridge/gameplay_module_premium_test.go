package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func newPremiumServiceTestRuntime(
	t *testing.T,
	activeSlots ...int64,
) (*Service, *gameSession, dnfrepo.Group, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	account := dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  make(map[string]string),
	}
	for _, slot := range activeSlots {
		premium.Upsert(&account, premium.DevilSlotType(slot), 7*24*60*60, 1, now)
	}
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   account.AccountID,
		Stats:       make(map[string]int64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       make(map[string]dnfrepo.ItemStack),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     make(map[string]dnfrepo.EquipmentEntry),
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          account.AccountID,
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	return service, &gameSession{
		conn:                connection,
		connID:              "premium-service-test",
		selectedCharacterID: 19,
	}, repositories, connection
}

func premiumServicePacketUsage(t *testing.T, packetBytes []byte, slot int64) uint8 {
	t.Helper()
	packet, trailing := splitGameServerUpperPacket(t, packetBytes)
	if len(trailing) != 0 ||
		packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketPremiumService) {
		t.Fatalf("premium packet header=%+v trailing=%d", packet.Header, len(trailing))
	}
	// Every non-tail premium send is the silent variant (serviceType 4); only the
	// login/entry scene tail uses 1, which also consumes the client's one-shot
	// contract-panel flag.
	if len(packet.Body) != 77 || !bytes.Equal(packet.Body[:3], []byte{1, byte(premium.ServiceTypeSilent), 0}) {
		t.Fatalf("premium packet body=%x", packet.Body)
	}
	return packet.Body[3+10+slot*9]
}

func TestCurrentPremiumServiceExplicitRequestReturnsSingleOp903State(t *testing.T) {
	service, session, _, connection := newPremiumServiceTestRuntime(
		t,
		premium.DevilSlotAutoRepair,
	)

	if err := service.handleCurrentPremiumService(session, nil); err != nil {
		t.Fatal(err)
	}

	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 ||
		packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketPremiumService) {
		t.Fatalf("explicit premium request packet header=%+v trailing=%x", packet.Header, trailing)
	}
	// The client's own op903 request is answered with the silent serviceType: the
	// one-shot contract-panel flag belongs to the login/entry scene tail.
	if len(packet.Body) != 77 || !bytes.Equal(packet.Body[:3], []byte{1, byte(premium.ServiceTypeSilent), 0}) {
		t.Fatalf("explicit premium request body=%x", packet.Body)
	}
	expiryOffset := 3 + 6 + premium.DevilSlotAutoRepair*9
	if got := binary.LittleEndian.Uint32(packet.Body[expiryOffset : expiryOffset+4]); got == 0 {
		t.Fatalf("explicit premium request auto-repair expiry=%d, want active absolute expiry", got)
	}
}

func TestCurrentPremiumSevenBuffUseConsumesOneDailyUse(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(
		t,
		premium.DevilSlotSevenBuff,
	)
	body := make([]byte, 2+currentPremiumServiceBlobSize)
	binary.LittleEndian.PutUint16(body[:2], 1)
	binary.LittleEndian.PutUint16(body[2:4], uint16(premium.DevilSlotSevenBuff))

	if err := service.handleCurrentPremiumService(session, body); err != nil {
		t.Fatal(err)
	}
	if got := premiumServicePacketUsage(t, connection.write.Bytes(), premium.DevilSlotSevenBuff); got != 1 {
		t.Fatalf("wire seven-buff usage=%d, want 1", got)
	}
	character, found, err := repositories.Character.Load(context.Background(), strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if got := premium.DailyUsage(character, premium.DevilSlotSevenBuff, time.Now().UTC()); got != 1 {
		t.Fatalf("persisted seven-buff usage=%d, want 1", got)
	}
}

// op9 on a character that is not weakened must be refused on both routes, and
// must spend nothing. This replaces the previous expectation - that the
// contract flag alone spent a daily quota - which was the "魔王契约 is consumed
// even when it did nothing" defect: the client's flag is a request, and the
// server decides.
func TestCurrentPremiumFreeWeaknessRefusesAHealthyCharacterAndSpendsNothing(t *testing.T) {
	for name, body := range map[string][]byte{
		"paid route":     {0, 0},
		"contract route": {1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			service, session, repositories, connection := newPremiumServiceTestRuntime(
				t,
				premium.DevilSlotFreeWeakness,
			)
			if err := service.handleCurrentPremiumFreeWeakness(session, body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
				len(packet.Body) != 2 || packet.Body[0] != 0 ||
				packet.Body[1] != currentWeaknessReasonNotWeakened || len(trailing) != 0 {
				t.Fatalf("healthy refusal packet=%+v body=%x trailing=%x",
					packet.Header, packet.Body, trailing)
			}
			character, found, err := repositories.Character.Load(context.Background(), "19")
			if err != nil || !found {
				t.Fatalf("load character found=%t err=%v", found, err)
			}
			if got := premium.DailyUsage(character, premium.DevilSlotFreeWeakness, service.gameplayNow()); got != 0 {
				t.Fatalf("a refused recovery spent %d contract quota", got)
			}
		})
	}
}
