package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/experiencebonus"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentExperienceEffectItemInfoBodyUsesItemAndRemainingSeconds(t *testing.T) {
	body := buildCurrentExperienceEffectItemInfoBody(10005931, 1799)
	if len(body) != 9 || body[0] != 1 {
		t.Fatalf("body len/count=%d/%d, want 9/1: %x", len(body), body[0], body)
	}
	if got := binary.LittleEndian.Uint32(body[1:5]); got != 10005931 {
		t.Fatalf("item id=%d, want 10005931", got)
	}
	if got := binary.LittleEndian.Uint32(body[5:9]); got != 1799 {
		t.Fatalf("duration=%d, want 1799", got)
	}
}

func TestSendCurrentExperienceConsumableBuffProjectionRestoresOriginalEffectItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	account := dnfrepo.AccountRecord{AccountID: "dnf:1", State: "active"}
	if _, err := experiencebonus.ActivateForCharacter(
		&account,
		"19",
		10005931,
		200,
		30*time.Minute,
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "experience-effect-item-restore-test",
		selectedCharacterID: 19,
	}
	if err := service.sendCurrentExperienceConsumableBuffProjection(session, "test"); err != nil {
		t.Fatal(err)
	}

	packet, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentLoadEffectItemInfoMsgID {
		t.Fatalf("restore packet header=%+v rest=%x", packet.Header, rest)
	}
	if len(packet.Body) != 9 || packet.Body[0] != 1 {
		t.Fatalf("restore body=%x, want one 9-byte op174 record", packet.Body)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[1:5]); got != 10005931 {
		t.Fatalf("restore item id=%d, want 10005931", got)
	}
	remaining := binary.LittleEndian.Uint32(packet.Body[5:9])
	if remaining < 1798 || remaining > 1800 {
		t.Fatalf("restore remaining seconds=%d, want 1798..1800", remaining)
	}
	if !session.experienceConsumableProjectionSent {
		t.Fatal("restore projection was not marked sent")
	}

	written := connection.write.Len()
	if err := service.sendCurrentExperienceConsumableBuffProjection(session, "duplicate"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate restore wrote %d additional bytes", connection.write.Len()-written)
	}
}
