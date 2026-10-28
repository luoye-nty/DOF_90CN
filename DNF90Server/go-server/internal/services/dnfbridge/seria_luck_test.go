package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/packageitem"
)

func TestBuildSeriaLuckGaugeBodyUsesDisplayUnits(t *testing.T) {
	got, ok := buildSeriaLuckGaugeBody(7, 8)
	if !ok || !bytes.Equal(got, []byte{35, 40}) {
		t.Fatalf("body=%x ok=%t, want 23 28", got, ok)
	}
	got, ok = buildSeriaLuckGaugeBody(8, 0)
	if !ok || !bytes.Equal(got, []byte{40, 0}) {
		t.Fatalf("reset body=%x ok=%t, want 28 00", got, ok)
	}
}

func TestBuildSeriaLuckGaugeBodyRejectsInvalidStage(t *testing.T) {
	for _, stages := range [][2]int64{{-1, 0}, {0, 9}, {9, 9}} {
		if got, ok := buildSeriaLuckGaugeBody(stages[0], stages[1]); ok || got != nil {
			t.Fatalf("stages=%v body=%x ok=%t, want rejected", stages, got, ok)
		}
	}
}

func TestDeferredSelectSceneTailRestoresPersistedSeriaLuckGauge(t *testing.T) {
	service, session := newDeferredTailResumeFixture(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Account == nil {
		t.Fatal("account repository unavailable")
	}
	account, found, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	account.Metadata[packageitem.SeriaLuckMetadataKey] = "4"
	if err := repositories.Account.Save(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	if err := service.sendDeferredSelectSceneTail(session, "test_seria_luck_hud"); err != nil {
		t.Fatalf("send scene tail: %v", err)
	}
	packets := deferredTailPacketSignatures(t, session.conn.(*bufferConn).write.Bytes())
	var gauge *deferredTailPacketSignature
	for index := range packets {
		if packets[index].classification == 0 && packets[index].msgID == currentSeriaLuckGaugeMsgID {
			gauge = &packets[index]
		}
	}
	if gauge == nil || !bytes.Equal(gauge.body, []byte{20, 20}) {
		t.Fatalf("BOOSTER_GAGE=%+v, want class-0 0x019c body 1414", gauge)
	}
}

func TestInitialSeriaLuckGaugeSkipsAbsentDurableValue(t *testing.T) {
	service, session := newDeferredTailResumeFixture(t)
	if err := service.sendInitialSeriaLuckGauge(session, "test_missing"); err != nil {
		t.Fatalf("send initial gauge: %v", err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("missing durable value emitted %x", session.conn.(*bufferConn).write.Bytes())
	}
}

func TestAlignedMagicBoxSendsLuckGaugeAfterNativeResultAndItemProjection(t *testing.T) {
	service, session := newDeferredTailResumeFixture(t)
	result := alignedcmd.Result{
		Operation: "use_randombox_item",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketUseRandomboxItem),
			Body:           []byte{0x01},
			Classification: 1,
		}},
		PostItemSlotRefreshResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketWalkoutPartyMember),
			Body:           []byte{0x55},
			Classification: 0,
		}},
		SeriaLuck: &alignedcmd.SeriaLuckProjection{Before: 4, After: 5},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("send aligned responses: %v", err)
	}
	packets := deferredTailPacketSignatures(t, session.conn.(*bufferConn).write.Bytes())
	if len(packets) != 3 {
		t.Fatalf("packets=%+v, want result ACK, item projection, luck gauge", packets)
	}
	if packets[0].classification != 1 || packets[0].msgID != uint16(dnfenum.CmdPacketUseRandomboxItem) || !bytes.Equal(packets[0].body, []byte{0x01}) {
		t.Fatalf("packet[0]=%+v, want native magic-box result", packets[0])
	}
	if packets[1].classification != 0 || packets[1].msgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || !bytes.Equal(packets[1].body, []byte{0x55}) {
		t.Fatalf("packet[1]=%+v, want committed item projection", packets[1])
	}
	if packets[2].classification != 0 || packets[2].msgID != currentSeriaLuckGaugeMsgID || !bytes.Equal(packets[2].body, []byte{20, 25}) {
		t.Fatalf("packet[2]=%+v, want Seria luck 4->5 after ACK and items", packets[2])
	}
}
