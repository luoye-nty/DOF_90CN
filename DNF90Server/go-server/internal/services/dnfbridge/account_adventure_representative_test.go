package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentAdventureRepresentativeSetSuccessBodyMatchesCurrentClientShape(t *testing.T) {
	body := buildCurrentAdventureRepresentativeSetSuccessBody(14, 37)
	want := []byte{3, 0, 0, 0, 14, 37, 0}
	if !bytes.Equal(body, want) {
		t.Fatalf("set success body=%x want=%x", body, want)
	}
}

func TestDecodeCurrentAdventureRepresentativeSetUsesPackedGrowSlot(t *testing.T) {
	body := []byte{2, 0x0b, 0x01, 0, 0, 11, 0x24}
	request, valid := decodeCurrentAdventureRepresentativeSet(body)
	if !valid || request.GroupID != 2 || request.CharacterID != 267 ||
		request.Slot != (adventuregroup.RepresentativeSlot{Job: 11, BaseGrow: 4}) {
		t.Fatalf("decoded request=%+v valid=%t", request, valid)
	}
	body[6] = 0x14
	if _, valid := decodeCurrentAdventureRepresentativeSet(body); valid {
		t.Fatal("first-awakening representative request accepted")
	}
}

func TestBuildCurrentAdventureInfoProjectsSelectedRemovedAndFallbackRepresentatives(t *testing.T) {
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "1", Job: "0", Level: 90, Stats: map[string]int64{"grow_type": 0x21}},
		{CharacterID: "2", Name: "selected", Job: "0", Level: 90, Stats: map[string]int64{
			"grow_type": 0x31, "honor_expert_level": 8, "server_group_id": 2,
		}},
		{CharacterID: "3", Job: "1", Level: 90, Stats: map[string]int64{"grow_type": 0x22}},
	}
	projection := adventuregroup.Projection{
		RepresentativesEnabled: true,
		Representatives: adventuregroup.RepresentativeState{Selections: map[adventuregroup.RepresentativeSlot]adventuregroup.RepresentativeSelection{
			{Job: 0, BaseGrow: 1}: {CharacterID: "2"},
			{Job: 1, BaseGrow: 2}: {Removed: true},
		}},
	}
	body := buildCurrentAdventureInfoBodyWithState(1, adventuregroup.Summary{}, characters, "group", 20260819, projection)
	raw := body[18 : 18+currentAdventureInfoRawLength]
	if count := binary.LittleEndian.Uint16(raw[currentAdventureInfoCharacterCountOffset:]); count != 3 {
		t.Fatalf("account character count=%d", count)
	}
	entry := raw[currentAdventureInfoRosterOffset : currentAdventureInfoRosterOffset+currentAdventureInfoRosterEntrySize]
	if entry[0] != 0 || entry[1] != 1 || entry[currentAdventureInfoRosterRegionOffset] != 2 ||
		binary.LittleEndian.Uint32(entry[currentAdventureInfoRosterHonorOffset:]) != 8 ||
		!bytes.HasPrefix(entry[currentAdventureInfoRosterNameOffset:], []byte("selected")) {
		t.Fatalf("selected representative row=%x", entry)
	}
}

func TestHandleCurrentAdventureRepresentativeRemovePersistsTombstone(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:1", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: "dnf:1", Job: "11", Level: 90,
		Stats: map[string]int64{"grow_type": 0x24},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:             options{accountID: "dnf:1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 20}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRemoveRepresentCharacJob),
		[]byte{11, 0x24},
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRemoveRepresentCharacJob) ||
		!bytes.Equal(packet.Body, []byte{1}) {
		t.Fatalf("remove ACK header=%+v body=%x trailing=%d", packet.Header, packet.Body, len(rest))
	}
	owner, err := adventuregroup.NewRepresentativeOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	state, err := owner.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatal(err)
	}
	selection := state.Selections[adventuregroup.RepresentativeSlot{Job: 11, BaseGrow: 4}]
	if !selection.Removed {
		t.Fatalf("persisted selection=%+v", selection)
	}
}

func TestHandleCurrentAdventureRepresentativeSetRejectsForeignCandidate(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:1", State: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "20", AccountID: "dnf:1", Job: "11", Level: 90, Stats: map[string]int64{"grow_type": 0x24}},
		{CharacterID: "267", AccountID: "dnf:2", Job: "11", Level: 90, Stats: map[string]int64{"grow_type": 0x24}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	connection := &bufferConn{}
	service := &Service{
		options:             options{accountID: "dnf:1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 20}
	body := make([]byte, 7)
	body[0] = 0
	binary.LittleEndian.PutUint32(body[1:5], 267)
	body[5], body[6] = 11, 0x24
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketSetRepresentCharacJob),
		body,
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || !bytes.Equal(packet.Body, []byte{0, 3}) {
		t.Fatalf("foreign candidate response=%x trailing=%d", packet.Body, len(rest))
	}
}
