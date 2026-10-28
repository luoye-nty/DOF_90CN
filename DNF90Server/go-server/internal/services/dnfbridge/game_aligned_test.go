// 本文件验证 DNF game 端已对齐命令的桥接回包顺序。
package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

type orderedPartyWrite struct {
	connection string
	data       []byte
}

type orderedPartyWriteLog struct {
	mu     sync.Mutex
	writes []orderedPartyWrite
}

type orderedPartyConn struct {
	*bufferConn
	name string
	log  *orderedPartyWriteLog
}

type contextBoundSkillRulesCharacterRepository struct {
	dnfrepo.CharacterRepository
}

func (repository *contextBoundSkillRulesCharacterRepository) Load(ctx context.Context, characterID string) (dnfrepo.CharacterRecord, bool, error) {
	<-ctx.Done()
	return dnfrepo.CharacterRecord{}, false, ctx.Err()
}

func (connection *orderedPartyConn) Write(data []byte) (int, error) {
	connection.log.mu.Lock()
	connection.log.writes = append(connection.log.writes, orderedPartyWrite{
		connection: connection.name,
		data:       append([]byte(nil), data...),
	})
	connection.log.mu.Unlock()
	return connection.bufferConn.Write(data)
}

func TestGameAlignedRegistryRoutesRegisteredDomains(t *testing.T) {
	tests := []struct {
		name            string
		opcode          dnfenum.CmdPacket
		domain          dnfenum.AlignedDomain
		responseAllowed bool
	}{
		{name: "inventory", opcode: dnfenum.CmdPacketDeleteItem, domain: dnfenum.AlignedDomainInventory, responseAllowed: true},
		{name: "pet", opcode: dnfenum.CmdPacketHatchCreatureEgg, domain: dnfenum.AlignedDomainPet},
		{name: "mail", opcode: dnfenum.CmdPacketMailboxOpen, domain: dnfenum.AlignedDomainMail, responseAllowed: true},
		{name: "quest", opcode: dnfenum.CmdPacketAcceptQuest, domain: dnfenum.AlignedDomainQuest},
		{name: "skill", opcode: dnfenum.CmdPacketBuySkill, domain: dnfenum.AlignedDomainSkill, responseAllowed: true},
		{name: "cargo", opcode: dnfenum.CmdPacketCreateAccountCargo, domain: dnfenum.AlignedDomainCargo},
		{name: "item lock", opcode: dnfenum.CmdPacketRequestItemLock, domain: dnfenum.AlignedDomainItemLock},
		{name: "item sort lock", opcode: dnfenum.CmdPacketRequestItemSortLock, domain: dnfenum.AlignedDomainItemLock},
		{name: "dungeon", opcode: dnfenum.CmdPacketGetItem, domain: dnfenum.AlignedDomainDungeon},
		{name: "avatar title", opcode: dnfenum.CmdPacketTitleBookPut, domain: dnfenum.AlignedDomainAvatarTitle},
		{name: "package", opcode: dnfenum.CmdPacketUseRandomboxItem, domain: dnfenum.AlignedDomainPackage},
		{name: "package expand", opcode: dnfenum.CmdPacketUseRandomboxItemExpand, domain: dnfenum.AlignedDomainPackage},
		{name: "party", opcode: dnfenum.CmdPacketSetPartyInfo, domain: dnfenum.AlignedDomainParty, responseAllowed: true},
		{name: "raid", opcode: dnfenum.CmdPacketRaidManagerWork, domain: dnfenum.AlignedDomainRaid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := gameAlignedRegistry.Route(context.Background(), alignedcmd.Request{Opcode: uint16(tt.opcode)})
			if err != nil {
				t.Fatalf("Route error = %v", err)
			}
			if !ok {
				t.Fatalf("Route(%d) not classified", tt.opcode)
			}
			if got.Decision.Domain != tt.domain {
				t.Fatalf("domain = %q, want %q", got.Decision.Domain, tt.domain)
			}
			if got.ResponseAllowed != tt.responseAllowed {
				t.Fatalf("responseAllowed = %v, want %v", got.ResponseAllowed, tt.responseAllowed)
			}
			if strings.Contains(got.Reason, "not registered") {
				t.Fatalf("opcode %d was not handled by registered module: %q", tt.opcode, got.Reason)
			}
		})
	}
}

func TestLegacyItemSortLockAndUnlockRoundTrip(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:64": {ItemID: 1001, Count: 1, Extra: map[string]string{"equipment_lock_state": "1"}},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn: connection, connID: "legacy-sort-lock",
		channel:             channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID: 77,
	}
	body := []byte{0, 0x40, 0x00}
	wantACKBody := []byte{1, 0, 0x40, 0x00}

	for index, opcode := range []dnfenum.CmdPacket{
		dnfenum.CmdPacketRequestItemSortLock,
		dnfenum.CmdPacketRequestItemSortUnlock,
	} {
		connection.write.Reset()
		frame := buildLegacyGamePacketForBridgeTest(byte(dnfenum.GameCmdCommand), uint16(opcode), uint16(index+1), body)
		if err := service.handleLegacyGamePacket(session, frame); err != nil {
			t.Fatalf("handle legacy opcode %d: %v", opcode, err)
		}
		ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
		if len(trailing) != 0 || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
			ack.Header.MsgID != uint16(opcode) || !bytes.Equal(ack.Body, wantACKBody) {
			t.Fatalf("opcode %d ACK header=%+v body=%x trailing=%d", opcode, ack.Header, ack.Body, len(trailing))
		}
		record, found, err := repos.Inventory.Load(ctx, "77")
		if err != nil || !found {
			t.Fatalf("load inventory after opcode %d: found=%t err=%v", opcode, found, err)
		}
		wantLocked := opcode == dnfenum.CmdPacketRequestItemSortLock
		if got := dnfitemlock.IsSortLocked(record.Slots["0:64"]); got != wantLocked {
			t.Fatalf("opcode %d sort locked=%t want=%t", opcode, got, wantLocked)
		}
		if record.Slots["0:64"].Extra["equipment_lock_state"] != "1" {
			t.Fatalf("opcode %d changed ordinary item lock: %+v", opcode, record.Slots["0:64"].Extra)
		}
	}
}

func TestGameAlignedRegistryCoversAlignedDomains(t *testing.T) {
	for _, command := range dnfenum.AlignedCommands() {
		if command.Domain == dnfenum.AlignedDomainCharacter {
			continue
		}
		t.Run(dnfenum.CmdPacketName(uint16(command.Opcode)), func(t *testing.T) {
			got, ok, err := gameAlignedRegistry.Route(context.Background(), alignedcmd.Request{Opcode: uint16(command.Opcode)})
			if err != nil {
				t.Fatalf("Route error = %v", err)
			}
			if !ok {
				t.Fatalf("Route(%d) not classified", command.Opcode)
			}
			if got.Decision.Domain != command.Domain {
				t.Fatalf("domain = %q, want %q", got.Decision.Domain, command.Domain)
			}
			if strings.Contains(got.Reason, "not registered") {
				t.Fatalf("aligned opcode %d has no registered module: %q", command.Opcode, got.Reason)
			}
		})
	}
}

func TestAlignedSkillOwnerRulesRequiredForMutationCommands(t *testing.T) {
	tests := []struct {
		name   string
		opcode dnfenum.CmdPacket
		want   bool
	}{
		{name: "change slot", opcode: dnfenum.CmdPacketChangeSkillslot, want: true},
		{name: "buy skill", opcode: dnfenum.CmdPacketBuySkill, want: true},
		{name: "skill init", opcode: dnfenum.CmdPacketSkillInit, want: true},
		{name: "unrelated command", opcode: dnfenum.CmdPacketMailboxOpen, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := alignedSkillOwnerRulesRequired(tt.opcode); got != tt.want {
				t.Fatalf("alignedSkillOwnerRulesRequired(%d) = %t, want %t", tt.opcode, got, tt.want)
			}
		})
	}
}

func TestBuySkillRulesPreflightCancellationReturnsNativeFailure(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	repositories.Character = &contextBoundSkillRulesCharacterRepository{CharacterRepository: repositories.Character}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	connection := &bufferConn{}
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	session := &gameSession{
		ctx:                 requestContext,
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 121, Type: 1, Name: "ch.121", Port: 10121},
		selectedCharacterID: 4,
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuySkill),
		[]byte{0, 1, 100, 0, 0, 1, 0},
	)
	if err != nil {
		t.Fatalf("handle canceled op29 preflight: %v", err)
	}
	if !handled {
		t.Fatal("canceled op29 preflight was not handled")
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketBuySkill) ||
		!bytes.Equal(packet.Body, []byte{0, 0x12}) || len(trailing) != 0 {
		t.Fatalf("canceled op29 ACK header=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
}

func TestHandleAlignedGameCommandSendsMailboxOpenAck(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		},
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		channel:             channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID: 77,
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketMailboxOpen),
		nil,
	)
	if err != nil {
		t.Fatalf("handleAlignedGameCommand error = %v", err)
	}
	if !handled {
		t.Fatalf("MailboxOpen should be handled by aligned module")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	snapshot, trailing := splitGameServerUpperPacket(t, rest)
	if packet.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("classification = %d, want %d", packet.Header.Classification, dnfproto.DefaultChannelClassification)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketMailboxOpen) {
		t.Fatalf("msgID = %d, want %d", packet.Header.MsgID, dnfenum.CmdPacketMailboxOpen)
	}
	if !bytes.Equal(packet.Body, []byte{0x01, 0x00, 0x00}) {
		t.Fatalf("body = % X, want 01 00 00", packet.Body)
	}
	if snapshot.Header.Classification != 0 || snapshot.Header.MsgID != 0x0061 || !bytes.Equal(snapshot.Body, []byte{0, 0, 0, 0, 0, 0}) {
		t.Fatalf("mailbox snapshot header=%+v body=%x, want class0/op97 empty snapshot", snapshot.Header, snapshot.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("mailbox open trailing bytes = %d, want only op96 ACK and class0/op97 snapshot", len(trailing))
	}
}

func TestHandleAlignedGameCommandRepliesToMailboxAccountRoleRequestWithOp718(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:mail-picker", Name: "sender", Slot: 0},
		{CharacterID: "2", AccountID: "dnf:mail-picker", Name: "target", Slot: 1},
		{CharacterID: "3", AccountID: "another-account", Name: "unrelated", Slot: 0},
	} {
		if err := repos.Character.Save(ctx, character); err != nil {
			t.Fatalf("save character %s: %v", character.CharacterID, err)
		}
	}
	service := &Service{
		options: options{
			accountID:          "dnf:mail-picker",
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		},
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		channel:             channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID: 1,
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestServerCharacterList),
		[]byte{0},
	)
	if err != nil {
		t.Fatalf("handleAlignedGameCommand error = %v", err)
	}
	if !handled {
		t.Fatalf("mailbox account-role request should be handled by aligned module")
	}
	packet, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("mailbox account-role trailing bytes = %d, want 0", len(trailing))
	}
	if got := packet.Header; got.Classification != 0 || got.MsgID != 718 {
		t.Fatalf("mailbox account-role header = %+v, want class0/op718", got)
	}
	want := []byte{0, 1, 2, 0, 0, 0, 6, 0, 0, 0, 't', 'a', 'r', 'g', 'e', 't', 0}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("mailbox account-role body = % X, want % X", packet.Body, want)
	}
}

func TestHandleGameUpperFallbackRoutesUnlistedAccountCargoToOwner(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:upper-cargo"}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "91",
		AccountID:   "dnf:upper-cargo",
		Stats:       map[string]int64{"gold": 500000},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	service := &Service{
		options: options{
			accountID:                "dnf:upper-cargo",
			gameUpperBodyCodec:       gameUpperBodyCodecPlain,
			gameUpperClientBodyCodec: gameUpperClientBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		},
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "game-upper-cargo-fallback",
		channel:             channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID: 91,
	}
	frame, err := dnfproto.BuildChannelPacket(uint16(dnfenum.CmdPacketCreateAccountCargo), nil, 0, dnfproto.DefaultChannelClassification)
	if err != nil {
		t.Fatalf("build frame: %v", err)
	}

	if err := service.handleGameUpper(session, frame); err != nil {
		t.Fatalf("handleGameUpper create account cargo: %v", err)
	}

	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCreateAccountCargo) ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("create cargo ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	gold, rest := splitGameServerUpperPacket(t, rest)
	if gold.Header.Classification != 0 || gold.Header.MsgID != 0x000E || len(gold.Body) != 3+0x77 {
		t.Fatalf("gold refresh = class %d msg %d len=%d body=% X", gold.Header.Classification, gold.Header.MsgID, len(gold.Body), gold.Body)
	}
	if got := binary.LittleEndian.Uint32(gold.Body[9:13]); got != 400000 {
		t.Fatalf("gold refresh value = %d, want 400000", got)
	}
	// 开通/升级金库改变了容量：金库列表之前必须先补一发仓库底板重建（op391）。
	pad, rest := splitGameServerUpperPacketWithHeader(t, rest, dnfproto.GameServerUpperHeaderSize16)
	if pad.Header.Classification != 0 || pad.Header.MsgID != uint16(dnfenum.CmdPacketCancelCargoPad) {
		t.Fatalf("create cargo must reset the warehouse pad first: header=%+v", pad.Header)
	}
	accountCargo, rest := splitGameServerUpperPacketWithHeader(t, rest, dnfproto.GameServerUpperHeaderSize16)
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %d", len(rest))
	}
	if accountCargo.Header.Classification != 0 ||
		accountCargo.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) ||
		len(accountCargo.Body) != 9 ||
		accountCargo.Body[0] != 12 ||
		binary.LittleEndian.Uint16(accountCargo.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint32(accountCargo.Body[3:7]) != 0 ||
		binary.LittleEndian.Uint16(accountCargo.Body[7:9]) != 0 {
		t.Fatalf("account cargo refresh packet = class %d msg %d body=% X", accountCargo.Header.Classification, accountCargo.Header.MsgID, accountCargo.Body)
	}
	account, ok, err := repos.Account.Load(ctx, "dnf:upper-cargo")
	if err != nil || !ok {
		t.Fatalf("load account ok=%t err=%v", ok, err)
	}
	if account.Metadata["account_cargo_created"] != "true" || account.Metadata["account_cargo_level"] != "1" {
		t.Fatalf("account cargo metadata = %+v", account.Metadata)
	}
	character, ok, err := repos.Character.Load(ctx, "91")
	if err != nil || !ok {
		t.Fatalf("load character ok=%t err=%v", ok, err)
	}
	if got := character.Stats["gold"]; got != 400000 {
		t.Fatalf("character gold = %d, want 400000", got)
	}
}

func TestHandleAlignedGameCommandSendsSingleMemberPartySequence(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                      conn,
		channel:                   channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID:       1001,
		townSceneReadyCharacterID: 1001,
	}
	service.bindGameSessionCharacter(session, 1001)
	body := []byte{0x00, 0x03, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x9c, 0x00}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketSetPartyInfo),
		body,
	)
	if err != nil {
		t.Fatalf("handleAlignedGameCommand error = %v", err)
	}
	if !handled {
		t.Fatalf("SetPartyInfo should be handled by aligned module")
	}

	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketSetPartyInfo) ||
		!bytes.Equal(ack.Body, []byte{0x01}) {
		t.Fatalf("ack packet = class %d msg %d body % X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	assertCurrentPartyFrameSelectedSlot(t, rest, 0)
	rest = assertRuntimePartySceneRefresh(t, rest, 1)
	if len(rest) != 0 {
		t.Fatalf("unexpected unsolicited party-directory bytes: %d", len(rest))
	}
}

func TestHandleOnlineEntryIntoPartyLinksOnlineSessions(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	writeLog := &orderedPartyWriteLog{}
	sourceConn := &orderedPartyConn{bufferConn: &bufferConn{}, name: "source", log: writeLog}
	targetConn := &orderedPartyConn{bufferConn: &bufferConn{}, name: "target", log: writeLog}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	source := &gameSession{
		conn:                sourceConn,
		channel:             channel,
		selectedCharacterID: 1001,
		party: partySessionState{state: alignedcmd.PartyState{
			PartyID:    77,
			UserID:     1001,
			UserState:  1,
			NameBytes:  []byte("party"),
			MaxMembers: 4,
			Members: []alignedcmd.PartyMemberState{
				{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			},
		}},
	}
	target := &gameSession{
		conn:                targetConn,
		channel:             channel,
		selectedCharacterID: 1002,
	}
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(target, 1002)
	registerPartyPeerEndpointForTest(source, 3, 5063)
	registerPartyPeerEndpointForTest(target, 3, 60359)

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketEntryIntoParty),
		[]byte{0xea, 0x03, 0x00, 0x00},
	)
	if err != nil {
		t.Fatalf("handle entry into party: %v", err)
	}
	if !handled {
		t.Fatalf("EntryIntoParty should be handled by online party bridge")
	}
	if len(source.party.state.Members) != 2 || len(target.party.state.Members) != 2 {
		t.Fatalf("member counts source=%d target=%d", len(source.party.state.Members), len(target.party.state.Members))
	}

	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketEntryIntoParty) ||
		!bytes.Equal(ack.Body, []byte{1, 0xea, 0x03, 0x00, 0x00, 0xe9, 0x03, 0x00, 0x00}) {
		t.Fatalf("entry ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	assertCurrentPartyFrameSelectedSlot(t, rest, 0)
	rest = assertRuntimePartySnapshot(t, rest, 2)
	if len(rest) != 0 {
		t.Fatalf("source trailing bytes = %d", len(rest))
	}
	targetData := targetConn.write.Bytes()
	assertCurrentPartyFrameSelectedSlot(t, targetData, 0)
	targetRest := assertRuntimePartySnapshot(t, targetData, 2)
	if len(targetRest) != 0 {
		t.Fatalf("target trailing bytes = %d", len(targetRest))
	}

	// The two peer tables must be installed before either op9 starts NoPack's
	// native peer controller. Per-connection ordering alone is insufficient:
	// source op11 -> source op9 -> target op11 drops source's first handshake.
	writeLog.mu.Lock()
	writes := append([]orderedPartyWrite(nil), writeLog.writes...)
	writeLog.mu.Unlock()
	peerTablesBeforeFirstPartyFrame := make(map[string]bool, 2)
	firstPartyFrameSeen := false
	for _, write := range writes {
		packet, trailing := splitGameServerUpperPacket(t, write.data)
		if len(trailing) != 0 {
			t.Fatalf("%s write contains %d trailing bytes", write.connection, len(trailing))
		}
		if packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case 0x000b:
			if firstPartyFrameSeen {
				t.Fatalf("%s peer table arrived after the first party op9", write.connection)
			}
			peerTablesBeforeFirstPartyFrame[write.connection] = true
		case uint16(dnfenum.CmdPacketRecoverStamina):
			if !firstPartyFrameSeen {
				firstPartyFrameSeen = true
				if !peerTablesBeforeFirstPartyFrame["source"] || !peerTablesBeforeFirstPartyFrame["target"] {
					t.Fatalf("first party op9 arrived before both op11 tables: %+v", peerTablesBeforeFirstPartyFrame)
				}
			}
		}
	}
	if !firstPartyFrameSeen {
		t.Fatal("party formation did not emit op9")
	}
}

func TestSendCurrentPartyFrameProjectionRejectsPartyMissingReceiver(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	receiverConn := &bufferConn{}
	receiver := &gameSession{
		conn:                receiverConn,
		selectedCharacterID: 1001,
	}
	other := &gameSession{
		conn:                &bufferConn{},
		selectedCharacterID: 1002,
	}
	service.bindGameSessionCharacter(receiver, 1001)
	service.bindGameSessionCharacter(other, 1002)

	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1002,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1002, UserState: 1},
		},
	}
	if _, err := service.sendCurrentPartyFrameProjection(receiver, state, "test_missing_receiver"); err == nil {
		t.Fatal("party projection without the receiving character should fail closed")
	}
	if receiverConn.write.Len() != 0 {
		t.Fatalf("fail-closed party projection wrote %d bytes", receiverConn.write.Len())
	}

	emptyState := alignedcmd.PartyState{
		PartyID:    78,
		UserID:     1001,
		MaxMembers: 4,
	}
	if _, err := service.sendCurrentPartyFrameProjection(receiver, emptyState, "test_empty_active_party"); err == nil {
		t.Fatal("active party projection without members should fail closed")
	}
	if receiverConn.write.Len() != 0 {
		t.Fatalf("empty active party projection wrote %d bytes", receiverConn.write.Len())
	}
}

func TestSendCurrentPartyActorFrameProjectionUpdatesRemoteJoinState(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	receiverConn := &bufferConn{}
	receiver := &gameSession{conn: receiverConn, selectedCharacterID: 1001}
	actor := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1002}
	service.bindGameSessionCharacter(receiver, 1001)
	service.bindGameSessionCharacter(actor, 1002)
	createdSoloParty := alignedcmd.PartyState{
		PartyID: 1002, UserID: 1002, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{{UserID: 1002, UserState: 1}},
	}
	if sent, err := service.sendCurrentPartyActorFrameProjection(receiver, actor, createdSoloParty, "test_remote_party_created"); err != nil || !sent {
		t.Fatalf("active remote projection sent=%t err=%v", sent, err)
	}
	packet, rest := splitGameServerUpperPacket(t, receiverConn.write.Bytes())
	if len(packet.Body) < 6 || binary.LittleEndian.Uint16(packet.Body[4:6]) != currentSceneActorObjectKey(1002) {
		t.Fatalf("remote actor key body=% X", packet.Body)
	}
	assertCurrentPartyFrameProjection(t, receiverConn.write.Bytes(), 1, true)
	if len(rest) != 0 {
		t.Fatalf("active remote projection trailing=%d", len(rest))
	}

	receiverConn.write.Reset()
	if sent, err := service.sendCurrentPartyActorFrameProjection(receiver, actor, alignedcmd.PartyState{}, "test_remote_party_cleared"); err != nil || !sent {
		t.Fatalf("empty remote projection sent=%t err=%v", sent, err)
	}
	removed, rest := splitGameServerUpperPacket(t, receiverConn.write.Bytes())
	wantRemoved := buildCurrentSceneOp9ActorRemovalBodyInContext(
		currentSceneActorObjectKey(1002),
		receiver.townActorOwnerChannel,
	)
	if removed.Header.Classification != 0 || removed.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		!bytes.Equal(removed.Body, wantRemoved) || len(rest) != 0 {
		t.Fatalf("empty remote projection class=%d msg=%d body=% X trailing=%d",
			removed.Header.Classification, removed.Header.MsgID, removed.Body, len(rest))
	}
}

func TestSendCurrentPartyActorFrameProjectionUsesConnectionOwnerAfterLocalActorContextReset(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	receiver := &gameSession{
		conn:                            conn,
		channel:                         channelcatalog.Channel{ID: 121},
		selectedCharacterID:             1001,
		connectionTownActorOwnerChannel: 121,
		townActorOwnerChannel:           currentSceneObjectContext,
	}
	service.bindGameSessionCharacter(receiver, 1001)
	state := alignedcmd.PartyState{
		PartyID:    1,
		UserID:     1001,
		IsLeader:   true,
		MaxMembers: 4,
		Members:    []alignedcmd.PartyMemberState{{UserID: 1001, UserState: 1}},
	}

	sent, err := service.sendCurrentPartyFrameProjection(receiver, state, "test_connection_owner_after_local_context_reset")
	if err != nil || !sent {
		t.Fatalf("party projection sent=%t err=%v", sent, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) {
		t.Fatalf("party projection packet msg=%d rest=%d", packet.Header.MsgID, len(rest))
	}
	if len(packet.Body) < 12 || packet.Body[7] != 121 || packet.Body[10] != 1 || packet.Body[11] != 0 {
		t.Fatalf("party aggregate owner header=%x want channel=121 server=1 custom-name=0", packet.Body[:minInt(len(packet.Body), 12)])
	}
}

func TestSendRuntimePartySnapshotDefersWholeSequenceForUnboundSession(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		selectedCharacterID: 1001,
	}
	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1001,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1},
		},
	}
	if err := service.sendRuntimePartySnapshot(session, state); err != nil {
		t.Fatalf("deferred party snapshot: %v", err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("deferred party snapshot wrote a partial sequence of %d bytes", conn.write.Len())
	}
}

func TestHandleResponsePeerAcceptsOnlinePartyInvite(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	inviterConn := &bufferConn{}
	acceptorConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	inviter := &gameSession{
		conn:                inviterConn,
		channel:             channel,
		selectedCharacterID: 1001,
		party: partySessionState{state: alignedcmd.PartyState{
			PartyID:    88,
			UserID:     1001,
			UserState:  1,
			NameBytes:  []byte("party"),
			MaxMembers: 4,
			Members: []alignedcmd.PartyMemberState{
				{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			},
		}},
	}
	acceptor := &gameSession{
		conn:                acceptorConn,
		channel:             channel,
		selectedCharacterID: 1002,
	}
	service.bindGameSessionCharacter(inviter, 1001)
	service.bindGameSessionCharacter(acceptor, 1002)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1001,
		TownID:      1,
		AreaID:      2,
		Session:     inviter,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1002,
		TownID:      1,
		AreaID:      2,
		Session:     acceptor,
	})
	registerPartyPeerEndpointForTest(inviter, 3, 5063)
	registerPartyPeerEndpointForTest(acceptor, 3, 60359)
	inviterIdentity, inviterBound := service.boundGameSessionCharacterSnapshot(inviter)
	acceptorIdentity, acceptorBound := service.boundGameSessionCharacterSnapshot(acceptor)
	if !inviterBound || !acceptorBound || !service.runtimePartyManagerForService().RecordInvite(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 0, 0,
	) {
		t.Fatal("could not install a current-session party invite")
	}

	handled, err := service.handleAlignedGameCommand(
		acceptor,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketResponsePeer),
		[]byte{0xe9, 0x03, 0, 0, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("handle response peer: %v", err)
	}
	if !handled {
		t.Fatalf("ResponsePeer should be handled by online party bridge")
	}
	if _, pending := service.runtimePartyManagerForService().ConsumePeerRequest(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 0,
	); pending {
		t.Fatal("central pending invite was not consumed")
	}
	ack, rest := splitGameServerUpperPacket(t, acceptorConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketResponsePeer) ||
		!bytes.Equal(ack.Body, []byte{1, 0xe9, 0x03, 0}) {
		t.Fatalf("response peer ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	// The class1/op11 acceptance ACK already completed the ordinary invite.
	// Sending class1/op705 here makes the current client enter its raid flow.
	assertCurrentPartyFrameSelectedSlot(t, acceptorConn.write.Bytes(), 0)
	rest = assertRuntimePartySnapshot(t, rest, 2)
	rest = assertCurrentPartyFrameProjection(t, rest, 2, true)
	// The peer projection leaves the frame on a kind-0 op9; the publish closes it with
	// the op153 rows again so the client's party HUD keeps its HP/MP values.
	rest = assertRuntimePartyRealtimeRows(t, rest, 2)
	if len(rest) != 0 {
		t.Fatalf("acceptor trailing bytes = %d", len(rest))
	}
	// The same-version invitation path never sends the generic class0/op8
	// peer-action notice to the inviter. Its first post-acceptance packet is the
	// native class0/op11 endpoint table, immediately followed by op9 and op153.
	inviterRest := inviterConn.write.Bytes()
	assertCurrentPartyFrameSelectedSlot(t, inviterRest, 0)
	inviterRest = assertRuntimePartySnapshot(t, inviterRest, 2)
	inviterRest = assertCurrentPartyFrameProjection(t, inviterRest, 2, true)
	inviterRest = assertRuntimePartyRealtimeRows(t, inviterRest, 2)
	if len(inviterRest) != 0 {
		t.Fatalf("inviter trailing bytes = %d", len(inviterRest))
	}
}

func TestPendingPartyInviteRejectsReplacedInviterSession(t *testing.T) {
	service := &Service{}
	inviterOld := &gameSession{}
	acceptor := &gameSession{}
	service.bindGameSessionCharacter(inviterOld, 1001)
	service.bindGameSessionCharacter(acceptor, 1002)

	oldIdentity, oldBound := service.boundGameSessionCharacterSnapshot(inviterOld)
	acceptorIdentity, acceptorBound := service.boundGameSessionCharacterSnapshot(acceptor)
	if !oldBound || !acceptorBound || !service.runtimePartyManagerForService().RecordInvite(
		acceptorIdentity.character, acceptorIdentity.generation,
		oldIdentity.character, oldIdentity.generation, 0, 13,
	) {
		t.Fatal("could not register original invite")
	}

	// A reconnect keeps the same character ID but creates a new game session.
	// An old invitation must not attach the acceptor to that new connection.
	inviterReplacement := &gameSession{}
	service.bindGameSessionCharacter(inviterReplacement, 1001)
	replacementIdentity, replacementBound := service.onlineGameSessionCharacterSnapshot(1001)
	if !replacementBound || replacementIdentity.session != inviterReplacement {
		t.Fatal("replacement inviter was not installed as the authoritative session")
	}
	if _, accepted := service.runtimePartyManagerForService().ConsumePeerRequest(
		acceptorIdentity.character, acceptorIdentity.generation,
		replacementIdentity.character, replacementIdentity.generation, 13,
	); accepted {
		t.Fatal("invite accepted after inviter session replacement")
	}

	if !service.runtimePartyManagerForService().RecordInvite(
		acceptorIdentity.character, acceptorIdentity.generation,
		replacementIdentity.character, replacementIdentity.generation, 0, 13,
	) {
		t.Fatal("could not replace stale invite with current session")
	}
	if _, accepted := service.runtimePartyManagerForService().ConsumePeerRequest(
		acceptorIdentity.character, acceptorIdentity.generation,
		replacementIdentity.character, replacementIdentity.generation, 13,
	); !accepted {
		t.Fatal("current-session invite was not accepted")
	}
}

func TestOnlinePartyCommandIgnoresReplacedSourceSession(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	staleSource := &gameSession{}
	target := &gameSession{}
	service.bindGameSessionCharacter(staleSource, 1001)
	service.bindGameSessionCharacter(target, 1002)

	// Replace character 1001 before its old connection's direct-join command
	// is processed. The old packet must not form a party with the live target.
	replacement := &gameSession{}
	service.bindGameSessionCharacter(replacement, 1001)
	handled, err := service.handleOnlinePartyCommand(
		staleSource,
		uint16(dnfenum.CmdPacketEntryIntoParty),
		[]byte{0xea, 0x03, 0x00, 0x00},
	)
	if err != nil || !handled {
		t.Fatalf("stale direct join handled=%t err=%v", handled, err)
	}
	if got := runtimePartyStateSnapshot(target); got.PartyID != 0 || len(got.Members) != 0 {
		t.Fatalf("stale direct join changed target party: %+v", got)
	}
	if got := runtimePartyStateSnapshot(replacement); got.PartyID != 0 || len(got.Members) != 0 {
		t.Fatalf("stale direct join changed replacement party: %+v", got)
	}
}

func TestHandleRequestPeerAcknowledgesVisibleTownTarget(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	sourceConn := &bufferConn{}
	source := &gameSession{
		conn:                sourceConn,
		connID:              "peer-source",
		selectedCharacterID: 1001,
	}
	target := &gameSession{
		conn:                &bufferConn{},
		connID:              "peer-target",
		selectedCharacterID: 1002,
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1001,
		TownID:      1,
		AreaID:      2,
		Session:     source,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1002,
		TownID:      1,
		AreaID:      2,
		Session:     target,
	})
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(target, 1002)

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestPeer),
		[]byte{0xea, 0x03, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("handle request peer: %v", err)
	}
	if !handled {
		t.Fatal("RequestPeer should be handled by online peer bridge")
	}
	packet, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestPeer) ||
		!bytes.Equal(packet.Body, []byte{1}) {
		t.Fatalf("request peer ack = class %d msg %d body=% X",
			packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
	partyState, rest := splitGameServerUpperPacket(t, rest)
	wantPartyless := buildCurrentSceneOp9ActorRemovalBodyInContext(
		currentSceneActorObjectKey(1002),
		source.townActorOwnerChannel,
	)
	if partyState.Header.Classification != 0 ||
		partyState.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		!bytes.Equal(partyState.Body, wantPartyless) {
		t.Fatalf("request peer partyless target state = class %d msg %d body=% X",
			partyState.Header.Classification, partyState.Header.MsgID, partyState.Body)
	}
	selection, rest := splitGameServerUpperPacket(t, rest)
	if selection.Header.Classification != 0 || selection.Header.MsgID != 0x0007 ||
		!bytes.Equal(selection.Body, []byte{0xea, 0x03, 15, 0, 0, 0, 0, 0xff, 0xff}) {
		t.Fatalf("request peer selection notice = class %d msg %d body=% X",
			selection.Header.Classification, selection.Header.MsgID, selection.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("request peer trailing bytes = %d", len(rest))
	}
}

func TestHandleRequestPeerRefreshesActiveTargetBeforeOpeningMenu(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	sourceConn := &bufferConn{}
	source := &gameSession{conn: sourceConn, connID: "peer-source-active", selectedCharacterID: 1001}
	target := &gameSession{conn: &bufferConn{}, connID: "peer-target-active", selectedCharacterID: 1002}
	// The target's party has to be a genuine two-member one: a one-member
	// aggregate is deliberately projected to the clicking client exactly like a
	// partyless character, so that its own menu offers invite instead of a join
	// the client can never complete. See
	// TestTownPeerSelectionOffersInviteForAOneMemberParty.
	teammate := &gameSession{conn: &bufferConn{}, connID: "peer-target-teammate", selectedCharacterID: 1003}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1001, TownID: 1, AreaID: 2, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1002, TownID: 1, AreaID: 2, Session: target})
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(target, 1002)
	service.bindGameSessionCharacter(teammate, 1003)
	storeRuntimePartyState(target, alignedcmd.PartyState{
		PartyID: 1002, UserID: 1002, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1002, UserState: 1},
			{UserID: 1003, UserState: 1},
		},
	})

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestPeer),
		[]byte{0xea, 0x03, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	)
	if err != nil || !handled {
		t.Fatalf("handle active target request peer handled=%t err=%v", handled, err)
	}
	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketRequestPeer) ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("active target request ack class=%d msg=%d body=%x", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	rest = assertCurrentPartyFrameProjection(t, rest, 2, true)
	selection, rest := splitGameServerUpperPacket(t, rest)
	if selection.Header.Classification != 0 || selection.Header.MsgID != 0x0007 ||
		!bytes.Equal(selection.Body, []byte{0xea, 0x03, 15, 0, 0, 0, 0, 0, 0}) || len(rest) != 0 {
		t.Fatalf("active target selection class=%d msg=%d body=%x trailing=%d",
			selection.Header.Classification, selection.Header.MsgID, selection.Body, len(rest))
	}
}

func TestHandleRequestPeerDoesNotAcknowledgeTargetInAnotherArea(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	sourceConn := &bufferConn{}
	source := &gameSession{
		conn:                sourceConn,
		connID:              "peer-source",
		selectedCharacterID: 1001,
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1001,
		TownID:      1,
		AreaID:      2,
		Session:     source,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1002,
		TownID:      1,
		AreaID:      3,
		Session:     &gameSession{selectedCharacterID: 1002},
	})

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestPeer),
		[]byte{0xea, 0x03, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("handle request peer outside area: %v", err)
	}
	if !handled {
		t.Fatal("RequestPeer should be consumed even when the target is outside the area")
	}
	if sourceConn.write.Len() != 0 {
		t.Fatalf("outside-area request wrote %d bytes", sourceConn.write.Len())
	}
}

func TestHandleRequestPeerForwardsOnlineAssaultInvite(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	sourceConn := &bufferConn{}
	targetConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	source := &gameSession{
		conn:                sourceConn,
		channel:             channel,
		selectedCharacterID: 1001,
	}
	target := &gameSession{
		conn:                targetConn,
		channel:             channel,
		selectedCharacterID: 1002,
	}
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(target, 1002)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1001, TownID: 1, AreaID: 2, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1002, TownID: 1, AreaID: 2, Session: target})

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestPeer),
		[]byte{0xea, 0x03, 13, 0, 0, 0, 0, 0, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("handle request peer invite forward: %v", err)
	}
	if !handled {
		t.Fatalf("RequestPeer invite should be handled by online party bridge")
	}
	sourceAck, sourceRest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if sourceAck.Header.Classification != dnfproto.DefaultChannelClassification ||
		sourceAck.Header.MsgID != uint16(dnfenum.CmdPacketRequestPeer) || !bytes.Equal(sourceAck.Body, []byte{1}) {
		t.Fatalf("request peer ack = class %d msg %d body=% X trailing=%d", sourceAck.Header.Classification, sourceAck.Header.MsgID, sourceAck.Body, len(sourceRest))
	}
	wait, trailing := splitGameServerUpperPacket(t, sourceRest)
	if len(trailing) != 0 || wait.Header.Classification != 0 || wait.Header.MsgID != 444 || !bytes.Equal(wait.Body, []byte{0xea, 3, 13}) {
		t.Fatalf("assault wait dialog = %+v trailing=%x", wait, trailing)
	}
	sourceIdentity, sourceBound := service.boundGameSessionCharacterSnapshot(source)
	targetIdentity, targetBound := service.boundGameSessionCharacterSnapshot(target)
	if !sourceBound || !targetBound {
		t.Fatal("invite endpoints were not bound")
	}
	if _, pending := service.runtimePartyManagerForService().ConsumePeerRequest(
		targetIdentity.character, targetIdentity.generation,
		sourceIdentity.character, sourceIdentity.generation, 13,
	); !pending {
		t.Fatal("central invite was not recorded")
	}
	packet, rest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("target trailing bytes = %d", len(rest))
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != 443 ||
		!bytes.Equal(packet.Body, []byte{0xe9, 0x03, 13}) {
		t.Fatalf("forwarded invite packet = class %d msg %d body=% X", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
}

func TestHandleResponsePeerRejectsPendingInviteWithoutForwarding(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	inviterConn := &bufferConn{}
	acceptorConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	inviter := &gameSession{
		conn:                inviterConn,
		channel:             channel,
		selectedCharacterID: 1001,
	}
	acceptor := &gameSession{
		conn:                acceptorConn,
		channel:             channel,
		selectedCharacterID: 1002,
	}
	service.bindGameSessionCharacter(inviter, 1001)
	service.bindGameSessionCharacter(acceptor, 1002)
	inviterIdentity, inviterBound := service.boundGameSessionCharacterSnapshot(inviter)
	acceptorIdentity, acceptorBound := service.boundGameSessionCharacterSnapshot(acceptor)
	if !inviterBound || !acceptorBound || !service.runtimePartyManagerForService().RecordInvite(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 0, 13,
	) {
		t.Fatal("could not install a current-session party invite")
	}

	handled, err := service.handleAlignedGameCommand(
		acceptor,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketResponsePeer),
		[]byte{0xe9, 0x03, 13, 0, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("handle response peer reject: %v", err)
	}
	if !handled {
		t.Fatalf("ResponsePeer reject should be handled by online party bridge")
	}
	if _, pending := service.runtimePartyManagerForService().ConsumePeerRequest(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 13,
	); pending {
		t.Fatal("central pending invite was not consumed")
	}
	ack, acceptorRest := splitGameServerUpperPacket(t, acceptorConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != uint16(dnfenum.CmdPacketResponsePeer) ||
		!bytes.Equal(ack.Body, []byte{1, 0xe9, 0x03, 13}) || len(acceptorRest) != 0 {
		t.Fatalf("reject ack = class %d msg %d body=% X trailing=%d", ack.Header.Classification, ack.Header.MsgID, ack.Body, len(acceptorRest))
	}
	reject, rest := splitGameServerUpperPacket(t, inviterConn.write.Bytes())
	if len(rest) != 0 || reject.Header.Classification != 1 || reject.Header.MsgID != 11 || !bytes.Equal(reject.Body, []byte{0, 0xeb, 13}) {
		t.Fatalf("assault decline notification = %+v trailing=%x", reject, rest)
	}
}

func TestHandleRequestAndResponsePeerForwardsTradePrompt(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	sourceConn := &bufferConn{}
	targetConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	source := &gameSession{conn: sourceConn, channel: channel, selectedCharacterID: 1}
	target := &gameSession{conn: targetConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 1, AreaID: 2, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 1, AreaID: 2, Session: target})

	requestBody := []byte{5, 0, 1, 0x0a, 0x82, 0x01, 0, 0, 0, 0, 0}
	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRequestPeer), requestBody)
	if err != nil || !handled {
		t.Fatalf("trade request handled=%t err=%v", handled, err)
	}
	requestAck, sourceRest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if requestAck.Header.Classification != dnfproto.DefaultChannelClassification ||
		requestAck.Header.MsgID != uint16(dnfenum.CmdPacketRequestPeer) || !bytes.Equal(requestAck.Body, []byte{1}) || len(sourceRest) != 0 {
		t.Fatalf("trade request ack = class %d msg %d body=% X trailing=%d", requestAck.Header.Classification, requestAck.Header.MsgID, requestAck.Body, len(sourceRest))
	}
	prompt, targetRest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
	if prompt.Header.Classification != 0 || prompt.Header.MsgID != 7 ||
		!bytes.Equal(prompt.Body, []byte{1, 0, 1, 0x0a, 0x82, 0x01, 0, 0, 0, 0, 0}) || len(targetRest) != 0 {
		t.Fatalf("trade prompt = class %d msg %d body=% X trailing=%d", prompt.Header.Classification, prompt.Header.MsgID, prompt.Body, len(targetRest))
	}
	// The response below consumes the generation-bound central request. Its ACK
	// and forwarded notice prove that this request was recorded without
	// destructively reading the pending-invite table here.

	sourceConn.write.Reset()
	targetConn.write.Reset()
	handled, err = service.handleAlignedGameCommand(target, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketResponsePeer), []byte{1, 0, 1, 0, 0, 0, 0})
	if err != nil || !handled {
		t.Fatalf("trade response handled=%t err=%v", handled, err)
	}
	responseAck, targetRest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
	if responseAck.Header.Classification != dnfproto.DefaultChannelClassification ||
		responseAck.Header.MsgID != uint16(dnfenum.CmdPacketResponsePeer) ||
		!bytes.Equal(responseAck.Body, []byte{1, 1, 0, 1, 0}) || len(targetRest) != 0 {
		t.Fatalf("trade response ack = class %d msg %d body=% X trailing=%d", responseAck.Header.Classification, responseAck.Header.MsgID, responseAck.Body, len(targetRest))
	}
	notice, sourceRest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if notice.Header.Classification != 0 || notice.Header.MsgID != 8 ||
		!bytes.Equal(notice.Body, []byte{5, 0, 1, 0, 0, 0, 0}) || len(sourceRest) != 0 {
		t.Fatalf("trade response notice = class %d msg %d body=% X trailing=%d", notice.Header.Classification, notice.Header.MsgID, notice.Body, len(sourceRest))
	}
}

// assertRuntimePartyMembershipCleared consumes the frame that takes a member's
// own party window away: the op9 kind-3 record removal for its old party id
// followed by an empty 0x0099 row table. It is what the native leave handler
// publishes for the leaver, and what the single-member disband rule publishes
// for both sides.
func assertRuntimePartyMembershipCleared(t *testing.T, data []byte, wantPartyID uint16) []byte {
	t.Helper()
	frame, rest := splitGameServerUpperPacket(t, data)
	if frame.Header.Classification != 0 ||
		frame.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(frame.Body) != 10 ||
		binary.LittleEndian.Uint16(frame.Body[2:4]) != currentSceneOp9StableSceneValue ||
		binary.LittleEndian.Uint16(frame.Body[4:6]) != wantPartyID ||
		frame.Body[9] != currentSceneOp9ActorRemoveKind {
		t.Fatalf("party membership clear = class %d msg %d body=% X, want kind3 removal of party %d",
			frame.Header.Classification, frame.Header.MsgID, frame.Body, wantPartyID)
	}
	realtime, rest := splitGameServerUpperPacket(t, rest)
	if realtime.Header.Classification != 0 || realtime.Header.MsgID != 0x0099 ||
		len(realtime.Body) == 0 || realtime.Body[0] != 0 {
		t.Fatalf("party membership clear realtime = class %d msg %d body=% X, want an empty table",
			realtime.Header.Classification, realtime.Header.MsgID, realtime.Body)
	}
	return rest
}

// assertRuntimePartySoloRowThenCleared consumes the dissolve projection a member
// left behind by a dissolved party receives: the native kind-2 one-member row
// (multiplayer-to-solo confirmation) with its 0x0099 rows, and then the kind-3
// clear that removes the party object itself.
func assertRuntimePartySoloRowThenCleared(t *testing.T, data []byte, wantLeaderSlot byte, wantPartyID uint16) []byte {
	t.Helper()
	rest := assertRuntimePartyMembershipReduction(t, data, 1, wantLeaderSlot, 1)
	return assertRuntimePartyMembershipCleared(t, rest, wantPartyID)
}

func TestHandleOnlineLeavePartyDisbandsTheTwoMemberParty(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1001,
		IsLeader:   true,
		NameBytes:  []byte("party"),
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
		},
	}
	leader := &gameSession{conn: leaderConn, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: cloneRuntimePartyState(state)}}
	member := &gameSession{conn: memberConn, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: cloneRuntimePartyState(state)}}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketLeaveParty),
		nil,
	)
	if err != nil {
		t.Fatalf("handle leave party: %v", err)
	}
	if !handled {
		t.Fatalf("LeaveParty should be handled by online party bridge")
	}
	if leader.party.state.PartyID != 0 {
		t.Fatalf("leader party was not cleared: %+v", leader.party.state)
	}
	// Operator rule 2026-09-22 「一个人不成队」: the leave dropped the party to a
	// single member, so that member is detached as well instead of being left in
	// a party window it cannot see or leave.
	if member.party.state.PartyID != 0 || len(member.party.state.Members) != 0 {
		t.Fatalf("survivor kept a one-member aggregate = %+v", member.party.state)
	}
	if snapshot, found := service.runtimePartyManagerForService().SnapshotByUser(1002, member.characterGeneration); found {
		t.Fatalf("survivor remained in the central party: %+v", snapshot)
	}

	ack, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("leave ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	rest = assertRuntimePartyMembershipCleared(t, rest, uint16(state.PartyID))
	if len(rest) != 0 {
		t.Fatalf("leader trailing bytes = %d", len(rest))
	}
	memberRest := assertRuntimePartySoloRowThenCleared(
		t, memberConn.write.Bytes(), 1, uint16(state.PartyID))
	if len(memberRest) != 0 {
		t.Fatalf("member trailing bytes = %d", len(memberRest))
	}
}

func TestHandleOnlineWalkoutPartyMemberDisbandsTheTwoMemberParty(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1001,
		IsLeader:   true,
		NameBytes:  []byte("party"),
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
		},
	}
	leader := &gameSession{conn: leaderConn, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: cloneRuntimePartyState(state)}}
	member := &gameSession{conn: memberConn, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: cloneRuntimePartyState(state)}}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	// The walkout path publishes the removal under the manager's own party
	// generation (snapshot.StateFor), not the id the fixture cached, so pin the
	// id the client is actually told about.
	managed, managedFound := service.bootstrapManagedRuntimePartyForSession(leader)
	if !managedFound || managed.ID == 0 {
		t.Fatalf("walkout fixture party was not imported: %+v found=%t", managed, managedFound)
	}
	managedPartyID := managed.ID

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		[]byte{1},
	)
	if err != nil {
		t.Fatalf("handle walkout party member: %v", err)
	}
	if !handled {
		t.Fatalf("WalkoutPartyMember should be handled by online party bridge")
	}
	// Kicking the only other member leaves the kicker alone: same single-member
	// state, same dissolve.
	if leader.party.state.PartyID != 0 || len(leader.party.state.Members) != 0 {
		t.Fatalf("leader survivor aggregate = %+v", leader.party.state)
	}
	if member.party.state.PartyID != 0 {
		t.Fatalf("kicked member party was not cleared: %+v", member.party.state)
	}

	ack, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("walkout ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	rest = assertRuntimePartyWalkoutNotice(t, rest, 1, 0)
	rest = assertRuntimePartySoloRowThenCleared(t, rest, 0, managedPartyID)
	if len(rest) != 0 {
		t.Fatalf("leader trailing bytes = %d", len(rest))
	}
	memberRest := assertRuntimePartyWalkoutNotice(t, memberConn.write.Bytes(), 1, 0)
	memberRest = assertRuntimePartyMembershipCleared(t, memberRest, managedPartyID)
	if len(memberRest) != 0 {
		t.Fatalf("member trailing bytes = %d", len(memberRest))
	}
}

func TestHandleOnlineChangePartyMemberPositionBroadcastsSlots(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	leaderConn := &bufferConn{}
	memberAConn := &bufferConn{}
	memberBConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1001,
		IsLeader:   true,
		NameBytes:  []byte("party"),
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
			{UserID: 1003, UserState: 1, HPPercent: 70, MPPercent: 60},
		},
	}
	leader := &gameSession{conn: leaderConn, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: cloneRuntimePartyState(state)}}
	memberA := &gameSession{conn: memberAConn, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: cloneRuntimePartyState(state)}}
	memberB := &gameSession{conn: memberBConn, channel: channel, selectedCharacterID: 1003, party: partySessionState{state: cloneRuntimePartyState(state)}}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(memberA, 1002)
	service.bindGameSessionCharacter(memberB, 1003)

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketChangePartyMemberPosition),
		[]byte{1, 3},
	)
	if err != nil {
		t.Fatalf("handle change party member position: %v", err)
	}
	if !handled {
		t.Fatalf("ChangePartyMemberPosition should be handled by online party bridge")
	}
	for name, session := range map[string]*gameSession{
		"leader":  leader,
		"memberA": memberA,
		"memberB": memberB,
	} {
		if len(session.party.state.Members) != 3 ||
			session.party.state.Members[0].UserID != 1002 ||
			session.party.state.Members[1].UserID != 1001 ||
			session.party.state.Members[2].UserID != 1003 {
			t.Fatalf("%s party members = %+v, want 1002,1001,1003", name, session.party.state.Members)
		}
	}
	for name, test := range map[string]struct {
		data       []byte
		leaderSlot byte
	}{
		"leader":  {data: leaderConn.write.Bytes(), leaderSlot: 1},
		"memberA": {data: memberAConn.write.Bytes(), leaderSlot: 1},
		"memberB": {data: memberBConn.write.Bytes(), leaderSlot: 1},
	} {
		_, frameData := splitGameServerUpperPacket(t, test.data)
		assertCurrentPartyFrameSelectedSlot(t, frameData, test.leaderSlot)
		rest := assertRuntimePartyChangePosition(t, test.data, 1, 0)
		rest = assertRuntimePartySceneRefresh(t, rest, 3)
		if len(rest) != 0 {
			t.Fatalf("%s trailing bytes = %d", name, len(rest))
		}
	}
}

func TestHandleOnlineChangeHostPreservesStableSlotsAndRefreshesLeaderSlot(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	state := alignedcmd.PartyState{
		PartyID:    1001,
		UserID:     1001,
		IsLeader:   true,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
		},
	}
	leader := &gameSession{conn: leaderConn, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: cloneRuntimePartyState(state)}}
	member := &gameSession{conn: memberConn, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: cloneRuntimePartyState(state)}}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketChangeHost),
		[]byte{1},
	)
	if err != nil {
		t.Fatalf("handle change host: %v", err)
	}
	if !handled {
		t.Fatal("ChangeHost should be handled by online party bridge")
	}
	for name, session := range map[string]*gameSession{"old_leader": leader, "new_leader": member} {
		got := runtimePartyStateSnapshot(session)
		members := runtimePartyMembers(got)
		if got.UserID != 1002 || got.PartyID != 2 || len(members) != 2 ||
			members[0].UserID != 1001 || members[0].Slot != 0 ||
			members[1].UserID != 1002 || members[1].Slot != 1 {
			t.Fatalf("%s party state=%+v members=%+v", name, got, members)
		}
	}
	if leaderConn.write.Len() == 0 || memberConn.write.Len() == 0 {
		t.Fatalf("change host did not rebuild both clients: leader=%d member=%d", leaderConn.write.Len(), memberConn.write.Len())
	}
	for name, data := range map[string][]byte{
		"old_leader": leaderConn.write.Bytes(),
		"new_leader": memberConn.write.Bytes(),
	} {
		var msgIDs []uint16
		for len(data) > 0 {
			packet, rest := splitGameServerUpperPacket(t, data)
			msgIDs = append(msgIDs, packet.Header.MsgID)
			data = rest
		}
		if !reflect.DeepEqual(msgIDs, []uint16{
			uint16(dnfenum.CmdPacketRecoverStamina),
			0x0099,
		}) {
			t.Fatalf("%s change-host packets=%v, want stable op9 aggregate then op153 without op11", name, msgIDs)
		}
		assertCurrentPartyFrameSelectedSlot(t, map[string][]byte{
			"old_leader": leaderConn.write.Bytes(),
			"new_leader": memberConn.write.Bytes(),
		}[name], 1)
	}
}

func TestHandleOnlineReserveLeavePartyBroadcastsToMembers(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	sourceConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	state := alignedcmd.PartyState{
		PartyID:    77,
		UserID:     1001,
		IsLeader:   true,
		NameBytes:  []byte("party"),
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
		},
	}
	source := &gameSession{conn: sourceConn, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: cloneRuntimePartyState(state)}}
	member := &gameSession{conn: memberConn, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: cloneRuntimePartyState(state)}}
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(member, 1002)

	handled, err := service.handleAlignedGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketReserveLeaveParty),
		[]byte{1},
	)
	if err != nil {
		t.Fatalf("handle reserve leave party: %v", err)
	}
	if !handled {
		t.Fatalf("ReserveLeaveParty should be handled by online party bridge")
	}
	for name, data := range map[string][]byte{
		"source": sourceConn.write.Bytes(),
		"member": memberConn.write.Bytes(),
	} {
		packet, rest := splitGameServerUpperPacket(t, data)
		if len(rest) != 0 {
			t.Fatalf("%s trailing bytes = %d", name, len(rest))
		}
		if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
			packet.Header.MsgID != uint16(dnfenum.CmdPacketReserveLeaveParty) ||
			!bytes.Equal(packet.Body, []byte{1, 1, 0xe9, 0x03}) {
			t.Fatalf("%s reserve packet = class %d msg %d body=% X",
				name, packet.Header.Classification, packet.Header.MsgID, packet.Body)
		}
	}
}

func TestHandleOnlineEntryIntoPartyFinishUsesCurrentExeClassZeroBody(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	state := alignedcmd.PartyState{
		PartyID:   1,
		UserID:    1001,
		UserState: 1,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1},
			{UserID: 1002, UserState: 1},
		},
	}
	session := &gameSession{
		conn:                conn,
		channel:             channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038},
		selectedCharacterID: 1001,
		party:               partySessionState{state: cloneRuntimePartyState(state)},
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketEntryIntoPartyFinish),
		nil,
	)
	if err != nil {
		t.Fatalf("handle entry into party finish: %v", err)
	}
	if !handled {
		t.Fatalf("EntryIntoPartyFinish should be handled by online party bridge")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("entry finish trailing bytes = %d", len(rest))
	}
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketEntryIntoPartyFinish) ||
		!bytes.Equal(packet.Body, []byte{1, 0}) {
		t.Fatalf("entry finish packet = class %d msg %d body=% X",
			packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
}

func TestHandleOnlineCreateRaidBuildsRuntimeRefresh(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	session := &gameSession{conn: conn, channel: channel}
	service.bindGameSessionCharacter(session, 1001)

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("anton"), 5),
	)
	if err != nil {
		t.Fatalf("handle create raid: %v", err)
	}
	if !handled {
		t.Fatalf("CreateRaid should be handled by online raid bridge")
	}
	wantRaidKey := uint32(200)<<16 | 1001
	state := service.raids[wantRaidKey]
	if state == nil || state.LeaderID != 1001 || len(state.Members) != 1 || !bytes.Equal(state.NameBytes, []byte("anton")) {
		t.Fatalf("runtime raid state = %+v, want leader 1001 with one member", state)
	}

	object, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	wantObjectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(*state))
	if err != nil {
		t.Fatalf("build raid object: %v", err)
	}
	if object.Header.Classification != 0 || object.Header.MsgID != raid.RaidObjectMsgID ||
		!bytes.Equal(object.Body, wantObjectBody) {
		t.Fatalf("create raid object = class %d msg %d body=% X", object.Header.Classification, object.Header.MsgID, object.Body)
	}
	buff, rest := splitGameServerUpperPacket(t, rest)
	wantBuffBody := mustRuntimeRaidBuffRegistrationSnapshotBody(t, *state)
	if buff.Header.Classification != 0 || buff.Header.MsgID != raid.RaidBuffMsgID ||
		!bytes.Equal(buff.Body, wantBuffBody) {
		t.Fatalf("create raid buff = class %d msg %d body=% X", buff.Header.Classification, buff.Header.MsgID, buff.Body)
	}
	ack, rest := splitGameServerUpperPacket(t, rest)
	wantCreateBody := raid.BuildCreateRaidResultBody(wantRaidKey)
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCreateRaid) ||
		!bytes.Equal(ack.Body, wantCreateBody) {
		t.Fatalf("create raid ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	refresh, rest := splitGameServerUpperPacket(t, rest)
	wantRefreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*state))
	if err != nil {
		t.Fatalf("build raid member refresh: %v", err)
	}
	if refresh.Header.Classification != 0 || refresh.Header.MsgID != raid.RaidMemberRefreshMsgID ||
		!bytes.Equal(refresh.Body, wantRefreshBody) {
		t.Fatalf("create raid member refresh = class %d msg %d body=% X", refresh.Header.Classification, refresh.Header.MsgID, refresh.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("create raid trailing bytes = %d", len(rest))
	}
}

// The raid opens with the leader in squad 1 and nobody else assigned: the members it
// invites are 单人 until the leader hands out a 编制 (live 2026-09-17 22:0x, the
// attack-party status window: the leader's row carries 1队, the invited member reads
// 单人). The client reads that byte straight (sub_17B6EF0 stores it at +0x2D) and
// renders 0 as 单人, so the wire has to carry exactly this.
func TestHandleOnlineCreateRaidOpensWithTheLeaderInSquadOne(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	session := &gameSession{conn: conn, channel: channel}
	service.bindGameSessionCharacter(session, 1001)

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("anton"), 0),
	)
	if err != nil || !handled {
		t.Fatalf("create raid handled=%t err=%v", handled, err)
	}
	state := service.raids[uint32(200)<<16|1001]
	if state == nil || len(state.Members) != 1 {
		t.Fatalf("runtime raid state = %+v, want one member", state)
	}
	if member := state.Members[0]; member.GroupIndex != runtimeRaidDefaultGroup || member.SlotOrder != 0 {
		t.Fatalf("leader's 编制=%d/%d, want squad 1 slot 0", member.GroupIndex, member.SlotOrder)
	}
	if record := runtimeRaidMemberRecord(*state, state.Members[0]); record.GroupIndex != runtimeRaidDefaultGroup {
		t.Fatalf("roster record group=%d, want squad %d", record.GroupIndex, runtimeRaidDefaultGroup)
	}
}

// A member the leader is already partied with joins in standby: the 编制 is the
// leader's to hand out, and the ordinary-party projection assigns the squad when the
// party itself changes.
func TestHandleOnlineCreateRaidLeavesThePartiedMembersInStandby(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: &bufferConn{}, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	storeRuntimePartyState(leader, alignedcmd.PartyState{
		PartyID: 1001,
		UserID:  1001,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	})

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("anton"), 0),
	)
	if err != nil || !handled {
		t.Fatalf("create raid handled=%t err=%v", handled, err)
	}
	state := service.raids[uint32(200)<<16|1001]
	if state == nil || len(state.Members) != 2 {
		t.Fatalf("runtime raid state = %+v, want the party's two members", state)
	}
	for _, member := range state.Members {
		if member.CharID == 1001 {
			if member.GroupIndex != runtimeRaidDefaultGroup {
				t.Fatalf("leader landed in 编制 %d, want squad %d", member.GroupIndex, runtimeRaidDefaultGroup)
			}
			continue
		}
		if member.GroupIndex != 0 || member.SlotOrder != 0 {
			t.Fatalf("invited member 1002 landed in 编制 %d/%d, want standby 0/0", member.GroupIndex, member.SlotOrder)
		}
	}
}

func TestRuntimeRaidMemberRecordCarriesRepositoryJobAndGrowType(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "1001",
		Name:        "leader",
		Job:         "11",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 0x21},
	}); err != nil {
		t.Fatalf("seed raid character: %v", err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	member := service.newRuntimeRaidMemberLocked(1001, 1, 0)
	record := runtimeRaidMemberRecord(runtimeRaidState{LeaderID: 1001}, member)
	if record.Field40 != 11 {
		t.Fatalf("raid member job field=%d, want 11", record.Field40)
	}
	if record.Field44 != 0x21 {
		t.Fatalf("raid member grow_type=%02x, want 21", record.Field44)
	}
}

func TestHandleOnlineRequestRaidInfoConfirmsExistingRuntimeRaid(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	conn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023}
	session := &gameSession{conn: conn, channel: channel}
	service.bindGameSessionCharacter(session, 1)
	const raidKey uint32 = 0x00170001
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		},
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestRaidInfo),
		buildRequestRaidInfoBodyForBridgeTest(2, raidKey),
	)
	if err != nil || !handled {
		t.Fatalf("RequestRaidInfo handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("request raid info trailing bytes = %d", len(rest))
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestRaidInfo) ||
		!bytes.Equal(packet.Body, raid.BuildRaidInfoResponseBody()) {
		t.Fatalf("request raid info packet = class %d msg %d body=% X", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
}

func TestHandleOnlineRequestRaidInfoConfirmsUnknownRaidDirectoryRequest(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023}}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestRaidInfo),
		buildRequestRaidInfoBodyForBridgeTest(1, 0x00170001),
	)
	if err != nil || !handled {
		t.Fatalf("unknown RequestRaidInfo handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("unknown raid request trailing bytes = %d", len(rest))
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestRaidInfo) ||
		!bytes.Equal(packet.Body, raid.BuildRaidInfoResponseBody()) {
		t.Fatalf("unknown raid request packet = class %d msg %d body=% X", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
}

func TestHandleOnlineOtherChannelListSendsNativeEmptyPublicRaidPage(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	session := &gameSession{
		conn:    conn,
		channel: channelcatalog.Channel{ID: 221, Type: 23, Name: "anton", Port: 10221},
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRaidOtherChannelList),
		[]byte{1},
	)
	if err != nil || !handled {
		t.Fatalf("OtherChannelList handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("other channel list trailing bytes = %d", len(rest))
	}
	want, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{})
	if err != nil {
		t.Fatalf("BuildPublicRaidListBody error = %v", err)
	}
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != raid.RaidPublicListMsgID ||
		!bytes.Equal(packet.Body, want) {
		t.Fatalf("other channel list packet = class %d msg %d body=% X, want class0 msg %d body=% X",
			packet.Header.Classification, packet.Header.MsgID, packet.Body,
			raid.RaidPublicListMsgID, want)
	}
}

func TestHandleOnlineOtherChannelListFromFiendWarChannelSendsNativeEmptyPublicRaidPage(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	session := &gameSession{
		conn:    conn,
		channel: channelcatalog.Channel{ID: 42, Type: 42, Name: "fiendwar", Port: 10042},
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRaidOtherChannelList),
		[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	)
	if err != nil || !handled {
		t.Fatalf("FiendWar OtherChannelList handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("FiendWar other channel list trailing bytes = %d", len(rest))
	}
	want, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{})
	if err != nil {
		t.Fatalf("BuildPublicRaidListBody error = %v", err)
	}
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != raid.RaidPublicListMsgID ||
		!bytes.Equal(packet.Body, want) {
		t.Fatalf("FiendWar other channel list packet = class %d msg %d body=% X, want class0 msg %d body=% X",
			packet.Header.Classification, packet.Header.MsgID, packet.Body,
			raid.RaidPublicListMsgID, want)
	}
}

func TestHandleOnlineOtherChannelListReturnsRecruitingOrdinaryRaidObjects(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:    conn,
		channel: channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023},
	}
	ordinary := runtimeRaidState{
		RaidKey:         uint32(23)<<16 | 1001,
		LeaderID:        1001,
		RouteOrRaidType: 0,
		NameBytes:       []byte("anton team"),
		Members: []runtimeRaidMemberState{{
			CharID:     1001,
			Name:       "Leader",
			GroupIndex: 1,
			UserState:  1,
		}},
	}
	service.raids[ordinary.RaidKey] = &ordinary
	single := cloneRuntimeRaidState(ordinary)
	single.RaidKey++
	single.TailFlag = 1
	service.raids[single.RaidKey] = &single
	started := cloneRuntimeRaidState(ordinary)
	started.RaidKey += 2
	started.Started = true
	service.raids[started.RaidKey] = &started

	request := []byte{0, 0, 0, 0, 0, 5, 0, 0, 1, 0, 0, 0, 0}
	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRaidOtherChannelList),
		request,
	)
	if err != nil || !handled {
		t.Fatalf("OtherChannelList handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("other channel list trailing bytes = %d", len(rest))
	}
	want, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{
		TotalCount: 1,
		Objects:    []raid.RaidObjectMode0{runtimeRaidObjectMode0(ordinary)},
	})
	if err != nil {
		t.Fatalf("BuildPublicRaidListBody error = %v", err)
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidPublicListMsgID || !bytes.Equal(packet.Body, want) {
		t.Fatalf("public raid list packet = class %d msg %d body=% X, want class0 msg %d body=% X",
			packet.Header.Classification, packet.Header.MsgID, packet.Body, raid.RaidPublicListMsgID, want)
	}
}

func TestOpenPublicRaidDirectoryPushesCreateAndDisbandSnapshotsWithoutAnotherQuery(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	conn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023}
	session := &gameSession{conn: conn, channel: channel}
	service.bindGameSessionCharacter(session, 1001)

	directoryRequest := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if handled, err := service.handleAlignedGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRaidOtherChannelList), directoryRequest); err != nil || !handled {
		t.Fatalf("initial directory query handled=%t err=%v", handled, err)
	}
	initial, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 || initial.Header.MsgID != raid.RaidPublicListMsgID {
		t.Fatalf("initial directory packet msg=%d trailing=%d", initial.Header.MsgID, len(rest))
	}
	conn.write.Reset()

	if handled, err := service.handleAlignedGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketCreateRaid), buildRaidInfoRequestForBridgeTest(0, []byte("anton"), 0)); err != nil || !handled {
		t.Fatalf("create raid handled=%t err=%v", handled, err)
	}
	wantRaidKey := uint32(23)<<16 | 1001
	state := service.raids[wantRaidKey]
	if state == nil {
		t.Fatalf("created raid %08x is missing", wantRaidKey)
	}
	wantCreated, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{
		TotalCount: 1,
		Objects:    []raid.RaidObjectMode0{runtimeRaidObjectMode0(*state)},
	})
	if err != nil {
		t.Fatalf("build created directory page: %v", err)
	}
	assertLastPublicRaidDirectoryPacket(t, conn.write.Bytes(), wantCreated)
	conn.write.Reset()

	// The body carries the sender's own character id. Live traffic confirms the
	// field is a character key, not a slot or the raid key: game-000140 (char 1)
	// sent 0001 to leave, game-000144 (char 2) sent 0002, and the leader's
	// connection sent 0002 to kick member 2. The fixture character is 1001, so the
	// leave has to name 1001 -- anything else is what a kick looks like.
	leaveBody := make([]byte, 2)
	binary.LittleEndian.PutUint16(leaveBody, 1001)
	if handled, err := service.handleAlignedGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketLeaveRaid), leaveBody); err != nil || !handled {
		t.Fatalf("leave raid handled=%t err=%v", handled, err)
	}
	wantEmpty, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{})
	if err != nil {
		t.Fatalf("build empty directory page: %v", err)
	}
	assertLastPublicRaidDirectoryPacket(t, conn.write.Bytes(), wantEmpty)
}

func assertLastPublicRaidDirectoryPacket(t *testing.T, data []byte, want []byte) {
	t.Helper()
	var publicBodies [][]byte
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == raid.RaidPublicListMsgID {
			publicBodies = append(publicBodies, append([]byte(nil), packet.Body...))
		}
		data = rest
	}
	if len(publicBodies) == 0 {
		t.Fatal("no pushed public raid directory packet")
	}
	if got := publicBodies[len(publicBodies)-1]; !bytes.Equal(got, want) {
		t.Fatalf("last public raid directory body=% X, want=% X", got, want)
	}
}

func TestHandleOnlineRaidManagerWorkMovesMemberWithoutAck(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: leaderConn, channel: channel}
	member := &gameSession{conn: memberConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	raidKey := uint32(200)<<16 | 1001
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRaidManagerWork),
		buildRaidManagerWorkRequestForBridgeTest(0, 1002, 2),
	)
	if err != nil {
		t.Fatalf("handle raid manager work: %v", err)
	}
	if !handled {
		t.Fatalf("RaidManagerWork should be handled by online raid bridge")
	}
	// The regroup publishes the mode=0 raid object and the mode=3 roster, both on
	// 0x024F, separated by the buff registration that mode=0 travels with. Only
	// mode=0 carries the attack-party panel the leader's own window is built from,
	// and only mode=3 carries the per-row squad index, so both have to go out.
	for name, conn := range map[string]*bufferConn{"leader": leaderConn, "member": memberConn} {
		packets := splitAllCurrentGameServerUpperPackets(t, conn.write.Bytes())
		mode0, mode3, buff := 0, 0, 0
		var rosterBody []byte
		for _, packet := range packets {
			if packet.Header.Classification != 0 {
				continue
			}
			switch packet.Header.MsgID {
			case raid.RaidMemberRefreshMsgID:
				if len(packet.Body) >= 8 && binary.LittleEndian.Uint32(packet.Body[4:8]) == raid.RaidMemberRefreshMode3 {
					mode3++
					rosterBody = packet.Body
					continue
				}
				mode0++
			case raid.RaidBuffMsgID:
				buff++
			}
		}
		if mode0 != 1 || mode3 != 1 || buff < 1 {
			t.Fatalf("%s regroup packets mode0=%d mode3=%d buff=%d (all=%d)",
				name, mode0, mode3, buff, len(packets))
		}
		assertRaidRefreshMember(t, parseRaidRefreshBodyForTest(t, rosterBody, raidKey), 1002, 2, 0)
	}
}

func TestHandleOnlineRaidMemberChangeStateBroadcastsRefreshWithoutAck(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: leaderConn, channel: channel}
	member := &gameSession{conn: memberConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	raidKey := uint32(200)<<16 | 1001
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}

	handled, err := service.handleAlignedGameCommand(
		member,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRaidMemberChangeState),
		[]byte{3},
	)
	if err != nil {
		t.Fatalf("handle raid member change state: %v", err)
	}
	if !handled {
		t.Fatalf("RaidMemberChangeState should be handled by online raid bridge")
	}
	members, rest := splitRaidRefreshPacket(t, memberConn.write.Bytes(), raidKey)
	if len(rest) != 0 {
		t.Fatalf("member trailing bytes = %d", len(rest))
	}
	assertRaidRefreshMemberState(t, members, 1002, 1)
	members, rest = splitRaidRefreshPacket(t, leaderConn.write.Bytes(), raidKey)
	if len(rest) != 0 {
		t.Fatalf("leader trailing bytes = %d", len(rest))
	}
	assertRaidRefreshMemberState(t, members, 1002, 1)
}

func TestHandleOnlineStartRaidCreatesPartiesFromRaidGroups(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002", "1003"} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots: map[string]dnfrepo.ItemStack{
				"0:0": {ItemID: runtimeRaidEntryItemID, Count: 1},
			},
		}); err != nil {
			t.Fatalf("seed raid ticket for %s: %v", characterID, err)
		}
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidAnton:          runtimeRaidBridgeTestAntonConfig(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	leaderConn := &bufferConn{}
	memberAConn := &bufferConn{}
	memberBConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: leaderConn, channel: channel}
	memberA := &gameSession{conn: memberAConn, channel: channel}
	memberB := &gameSession{conn: memberBConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(memberA, 1002)
	service.bindGameSessionCharacter(memberB, 1003)
	raidKey := uint32(200)<<16 | 1001
	// The whole roster has to be standing in the raid waiting area before the
	// start is accepted (see raid_start_waiting_area.go).
	for _, memberSession := range []*gameSession{leader, memberA, memberB} {
		enterRuntimeRaidWaitingAreaPresenceForTest(t, service, memberSession, testAntonWaitingAreaSpec(t), raidKey)
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, Name: "memberA", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 90, MPPercent: 80},
			{CharID: 1003, Name: "memberB", GroupIndex: 2, SlotOrder: 0, UserState: 1, HPPercent: 70, MPPercent: 60},
		},
	}

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	)
	if err != nil {
		t.Fatalf("handle start raid: %v", err)
	}
	if !handled {
		t.Fatalf("StartRaid should be handled by online raid bridge")
	}
	if !service.raids[raidKey].Started {
		t.Fatalf("raid was not marked started")
	}

	ack, _ := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketStartRaid) ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("start raid ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	if got := runtimePartyStateSnapshot(leader); len(got.Members) != 2 || got.Members[0].UserID != 1001 || got.Members[1].UserID != 1002 {
		t.Fatalf("leader party state = %+v, want group 1 members 1001/1002", got)
	}
	if got := runtimePartyStateSnapshot(memberB); len(got.Members) != 1 || got.Members[0].UserID != 1003 {
		t.Fatalf("memberB party state = %+v, want solo group 2 member 1003", got)
	}
}

func TestHandleOnlineStartRaidRejectsOversizedGroup(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	conn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: conn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	raidKey := uint32(200)<<16 | 1001
	state := &runtimeRaidState{RaidKey: raidKey, LeaderID: 1001}
	for i := 0; i < runtimeRaidGroupSize+1; i++ {
		state.Members = append(state.Members, runtimeRaidMemberState{
			CharID:     uint16(1001 + i),
			GroupIndex: 1,
			SlotOrder:  byte(i),
			UserState:  1,
			HPPercent:  100,
			MPPercent:  100,
		})
	}
	service.raids[raidKey] = state

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	)
	if err != nil {
		t.Fatalf("handle start raid oversized group: %v", err)
	}
	if !handled {
		t.Fatalf("StartRaid should be handled by online raid bridge")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("oversized group trailing bytes = %d", len(rest))
	}
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketStartRaid) ||
		!bytes.Equal(packet.Body, []byte{0, runtimeRaidFailureCode}) {
		t.Fatalf("oversized group response = class %d msg %d body=% X", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
	if service.raids[raidKey].Started {
		t.Fatalf("oversized group raid was marked started")
	}
}

func TestRuntimeRaidSelectorSnapshotPreloadsMapRowsBeforeCreatingView(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain, pvfPath: "test.pvf"}, raids: make(map[uint32]*runtimeRaidState)}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023}}
	service.bindGameSessionCharacter(session, 1001)
	state := &runtimeRaidState{
		RaidKey:       0x00170001,
		LeaderID:      1001,
		NameBytes:     []byte("anton"),
		Started:       true,
		EntryCostPaid: true,
		Members: []runtimeRaidMemberState{{
			CharID: 1001, Name: "leader", GroupIndex: 1, UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
		FrozenMembers: []runtimeRaidMemberState{{
			CharID: 1001, Name: "leader", GroupIndex: 1, UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:         runtimeRaidLifecyclePhaseActive,
			Phase:          runtimeRaidDungeonPhaseOne,
			PhaseStartedAt: time.Now().UTC().Add(-time.Minute),
			PhaseDeadline:  time.Now().UTC().Add(time.Hour),
			Objectives: map[int]runtimeRaidDungeonObjective{
				210: {Phase: runtimeRaidDungeonPhaseOne, MapID: 210, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
			},
		},
	}
	service.raids[state.RaidKey] = state
	if err := service.sendRuntimeRaidSelectorSnapshot(session, "test"); err != nil {
		t.Fatalf("send selector snapshot: %v", err)
	}
	wantRefreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*state))
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []uint16{
		raid.RaidObjectMsgID,
		raid.RaidMemberRefreshMsgID,
		raid.RaidSymbolMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
	}
	stream := conn.write.Bytes()
	for index, wantID := range wantIDs {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != wantID {
			t.Fatalf("packet[%d] = class %d msg %d, want class0 msg %d", index, packet.Header.Classification, packet.Header.MsgID, wantID)
		}
		if index == 1 && !bytes.Equal(packet.Body, wantRefreshBody) {
			t.Fatalf("selector attack-party refresh body=%x", packet.Body)
		}
		stream = rest
	}
	if len(stream) != 0 {
		t.Fatalf("selector snapshot replayed an unexpected packet: %x", stream)
	}
}

// Standby members belong to no attack group. Folding group 0 into squad 1, which
// this used to do, projected a squad party onto a member the raid roster shows as
// unassigned.
func TestRuntimeRaidPartyGroupsSkipsUnassignedMembers(t *testing.T) {
	groups := runtimeRaidPartyGroups(runtimeRaidState{
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 0, SlotOrder: 0},
			{CharID: 1002, GroupIndex: 2, SlotOrder: 0},
		},
	})
	if len(groups) != 1 || len(groups[0]) != 1 {
		t.Fatalf("groups = %+v, want only the assigned member", groups)
	}
	if groups[0][0].CharID != 1002 || groups[0][0].GroupIndex != 2 {
		t.Fatalf("grouped member = %+v, want 1002 in group 2", groups[0][0])
	}
	// A raid where nobody is assigned projects no parties at all.
	if got := runtimeRaidPartyGroups(runtimeRaidState{
		Members: []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 0, SlotOrder: 0}},
	}); len(got) != 0 {
		t.Fatalf("all-standby roster produced groups: %+v", got)
	}
}

type raidRefreshMemberForTest struct {
	CharID     uint16
	State      byte
	GroupIndex byte
	SlotOrder  byte
}

func buildRaidInfoRequestForBridgeTest(route byte, name []byte, tail byte) []byte {
	body := make([]byte, 1+4+len(name)+1)
	body[0] = route
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(name)))
	copy(body[5:], name)
	body[len(body)-1] = tail
	return body
}

func buildRequestRaidInfoBodyForBridgeTest(mode byte, raidKey uint32) []byte {
	body := make([]byte, 5)
	body[0] = mode
	binary.LittleEndian.PutUint32(body[1:5], raidKey)
	return body
}

func buildRaidManagerWorkRequestForBridgeTest(action uint32, member uint32, group uint32) []byte {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:4], action)
	binary.LittleEndian.PutUint32(body[4:8], member)
	binary.LittleEndian.PutUint32(body[8:12], group)
	return body
}

func splitRaidRefreshPacket(t *testing.T, data []byte, wantRaidKey uint32) ([]raidRefreshMemberForTest, []byte) {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, data)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidMemberRefreshMsgID {
		t.Fatalf("raid refresh packet = class %d msg %d", packet.Header.Classification, packet.Header.MsgID)
	}
	members := parseRaidRefreshBodyForTest(t, packet.Body, wantRaidKey)
	return members, rest
}

func parseRaidRefreshBodyForTest(t *testing.T, body []byte, wantRaidKey uint32) []raidRefreshMemberForTest {
	t.Helper()
	if len(body) < 9 {
		t.Fatalf("raid refresh body too short: %d", len(body))
	}
	raidKey := binary.LittleEndian.Uint32(body[0:4])
	mode := binary.LittleEndian.Uint32(body[4:8])
	if raidKey != wantRaidKey || mode != raid.RaidMemberRefreshMode3 {
		t.Fatalf("raid refresh header = key 0x%08X mode %d, want key 0x%08X mode %d",
			raidKey, mode, wantRaidKey, raid.RaidMemberRefreshMode3)
	}
	count := int(body[8])
	offset := 9
	members := make([]raidRefreshMemberForTest, 0, count)
	for i := 0; i < count; i++ {
		if len(body) < offset+2+1+4 {
			t.Fatalf("raid refresh member[%d] truncated before name: offset=%d len=%d", i, offset, len(body))
		}
		member := raidRefreshMemberForTest{CharID: binary.LittleEndian.Uint16(body[offset : offset+2])}
		offset += 2
		member.State = body[offset]
		offset++
		nameLen := int(binary.LittleEndian.Uint32(body[offset : offset+4]))
		offset += 4
		if len(body) < offset+nameLen+2+2+4+1+1+1+4+2+4 {
			t.Fatalf("raid refresh member[%d] truncated after name: offset=%d name_len=%d len=%d", i, offset, nameLen, len(body))
		}
		offset += nameLen
		offset += 2
		member.GroupIndex = body[offset]
		offset++
		member.SlotOrder = body[offset]
		offset++
		offset += 4 + 1 + 1 + 1 + 4 + 2 + 4
		members = append(members, member)
	}
	if offset != len(body) {
		t.Fatalf("raid refresh body trailing bytes = %d", len(body)-offset)
	}
	return members
}

func assertRaidRefreshMember(t *testing.T, members []raidRefreshMemberForTest, charID uint16, group byte, slot byte) {
	t.Helper()
	for _, member := range members {
		if member.CharID == charID {
			if member.GroupIndex != group || member.SlotOrder != slot {
				t.Fatalf("member %d = group %d slot %d, want group %d slot %d",
					charID, member.GroupIndex, member.SlotOrder, group, slot)
			}
			return
		}
	}
	t.Fatalf("member %d missing from raid refresh: %+v", charID, members)
}

func assertRaidRefreshMemberState(t *testing.T, members []raidRefreshMemberForTest, charID uint16, state byte) {
	t.Helper()
	for _, member := range members {
		if member.CharID == charID {
			if member.State != state {
				t.Fatalf("member %d state = %d, want %d", charID, member.State, state)
			}
			return
		}
	}
	t.Fatalf("member %d missing from raid refresh: %+v", charID, members)
}

func assertRuntimePartySnapshot(t *testing.T, data []byte, wantMembers byte) []byte {
	t.Helper()
	peerEndpoints, rest := splitGameServerUpperPacket(t, data)
	if peerEndpoints.Header.Classification != 0 || peerEndpoints.Header.MsgID != 0x000b {
		t.Fatalf("peer endpoint packet = class %d msg %d", peerEndpoints.Header.Classification, peerEndpoints.Header.MsgID)
	}
	wantPeers := byte(0)
	if wantMembers > 0 {
		wantPeers = wantMembers - 1
	}
	if len(peerEndpoints.Body) != 1+int(wantPeers)*22 || peerEndpoints.Body[0] != wantPeers {
		t.Fatalf("peer endpoint members = % X, want remote count %d", peerEndpoints.Body, wantPeers)
	}
	rest = assertCurrentPartyFrameProjection(t, rest, wantMembers, wantMembers > 0)
	realtime, rest := splitGameServerUpperPacket(t, rest)
	if realtime.Header.Classification != 0 || realtime.Header.MsgID != 0x0099 {
		t.Fatalf("realtime packet = class %d msg %d", realtime.Header.Classification, realtime.Header.MsgID)
	}
	if len(realtime.Body) != 1+int(wantMembers)*5 || realtime.Body[0] != wantMembers {
		t.Fatalf("realtime members = % X, want count %d", realtime.Body, wantMembers)
	}
	for index := 0; index < int(wantMembers); index++ {
		offset := 1 + index*5
		// +2 is the member's HP percent and +4 its native party slot (see
		// party.BuildSingleMemberRealtimeInfo): a multi-member body must carry one
		// distinct slot per row.
		if realtime.Body[offset+2] != 100 || realtime.Body[offset+3] != 0 || realtime.Body[offset+4] != byte(index) {
			t.Fatalf("realtime row %d = % X, want hp=100 helper=0 slot=%d", index, realtime.Body[offset:offset+5], byte(index))
		}
	}
	return rest
}

// assertRuntimePartySceneRefresh validates the deliberately smaller scene
// reconstruction sequence: the complete op9 aggregate precedes op153, while
// op11 is intentionally absent so a town transition does not restart the
// already-established peer relay.
func assertRuntimePartySceneRefresh(t *testing.T, data []byte, wantMembers byte) []byte {
	t.Helper()
	rest := assertCurrentPartyFrameProjection(t, data, wantMembers, wantMembers > 0)
	realtime, rest := splitGameServerUpperPacket(t, rest)
	if realtime.Header.Classification != 0 || realtime.Header.MsgID != 0x0099 {
		t.Fatalf("scene party realtime packet = class %d msg %d", realtime.Header.Classification, realtime.Header.MsgID)
	}
	if len(realtime.Body) != 1+int(wantMembers)*5 || realtime.Body[0] != wantMembers {
		t.Fatalf("scene party realtime body = % X, want count %d", realtime.Body, wantMembers)
	}
	return rest
}

// assertRuntimePartyRealtimeRows consumes one bare op153 realtime-rows packet.
//
// It is the closing packet of a member's own party frame: the same-area peer
// projection (a kind-0 op9) is followed by one more op153, because op153 is the only
// carrier of the per-member HP/MP values (party.BuildSingleMemberRealtimeInfo). Without
// it the frame ends on a record whose rows carry no health, and the client's party HUD
// health bars come up empty -- see sendManagedRuntimePartySnapshots.
func assertRuntimePartyRealtimeRows(t *testing.T, data []byte, wantMembers byte) []byte {
	t.Helper()
	realtime, rest := splitGameServerUpperPacket(t, data)
	if realtime.Header.Classification != 0 || realtime.Header.MsgID != 0x0099 {
		t.Fatalf("closing realtime packet = class %d msg %d, want the op153 rows",
			realtime.Header.Classification, realtime.Header.MsgID)
	}
	if len(realtime.Body) != 1+int(wantMembers)*5 || realtime.Body[0] != wantMembers {
		t.Fatalf("closing realtime members = % X, want count %d", realtime.Body, wantMembers)
	}
	for index := 0; index < int(wantMembers); index++ {
		offset := 1 + index*5
		if realtime.Body[offset+2] != 100 || realtime.Body[offset+4] != byte(index) {
			t.Fatalf("closing realtime row %d = % X, want hp=100 slot=%d",
				index, realtime.Body[offset:offset+5], byte(index))
		}
	}
	return rest
}

func assertRuntimePartyMembershipReduction(t *testing.T, data []byte, wantFrameMembers byte, wantLeaderSlot byte, wantRealtimeMembers byte) []byte {
	t.Helper()
	frame, data := splitGameServerUpperPacket(t, data)
	if frame.Header.Classification != 0 || frame.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(frame.Body) < 15 || frame.Body[9] != 2 || frame.Body[10] != wantFrameMembers {
		t.Fatalf("party membership reduction = class %d msg %d body=% X, want kind2 count %d",
			frame.Header.Classification, frame.Header.MsgID, frame.Body, wantFrameMembers)
	}
	if got := frame.Body[len(frame.Body)-3]; got != wantLeaderSlot {
		t.Fatalf("party membership reduction leader slot=%d want=%d body=% X", got, wantLeaderSlot, frame.Body)
	}
	realtime, rest := splitGameServerUpperPacket(t, data)
	if realtime.Header.Classification != 0 || realtime.Header.MsgID != 0x0099 {
		t.Fatalf("party removed realtime packet = class %d msg %d", realtime.Header.Classification, realtime.Header.MsgID)
	}
	if len(realtime.Body) == 0 || realtime.Body[0] != wantRealtimeMembers {
		t.Fatalf("party removed realtime = % X, want count %d", realtime.Body, wantRealtimeMembers)
	}
	return rest
}

func assertRuntimePartyWalkoutNotice(t *testing.T, data []byte, slot byte, mode byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, data)
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestPeer) ||
		!bytes.Equal(packet.Body, []byte{slot, mode}) {
		t.Fatalf("walkout notice = class %d msg %d body=% X, want class %d msg %d body=% X",
			packet.Header.Classification,
			packet.Header.MsgID,
			packet.Body,
			dnfproto.DefaultChannelClassification,
			dnfenum.CmdPacketRequestPeer,
			[]byte{slot, mode})
	}
	return rest
}

func assertRuntimePartyChangePosition(t *testing.T, data []byte, fromSlot byte, toSlot byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, data)
	want := []byte{1, fromSlot, toSlot}
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketChangePartyMemberPosition) ||
		!bytes.Equal(packet.Body, want) {
		t.Fatalf("change position = class %d msg %d body=% X, want class %d msg %d body=% X",
			packet.Header.Classification,
			packet.Header.MsgID,
			packet.Body,
			dnfproto.DefaultChannelClassification,
			dnfenum.CmdPacketChangePartyMemberPosition,
			want)
	}
	return rest
}

func assertCurrentPartyFrameProjection(t *testing.T, data []byte, wantMembers byte, wantActive bool) []byte {
	t.Helper()
	frame, rest := splitGameServerUpperPacket(t, data)
	if frame.Header.Classification != 0 || frame.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) {
		t.Fatalf("party frame packet = class %d msg %d", frame.Header.Classification, frame.Header.MsgID)
	}
	if !wantActive {
		if wantMembers != 0 {
			t.Fatalf("inactive party frame requested with %d members", wantMembers)
		}
		if len(frame.Body) != 10 ||
			binary.LittleEndian.Uint16(frame.Body[0:2]) != 1 ||
			binary.LittleEndian.Uint16(frame.Body[2:4]) != currentSceneOp9StableSceneValue ||
			frame.Body[9] != currentSceneOp9ActorRemoveKind {
			t.Fatalf("inactive party frame is not a kind-3 actor clear: len=%d body=% X", len(frame.Body), frame.Body)
		}
		return rest
	}
	if len(frame.Body) < 26 {
		t.Fatalf("party frame body too short: len=%d body=% X", len(frame.Body), frame.Body)
	}
	slotCountOffset := 25
	if frame.Body[11] == 0 {
		if len(frame.Body) < 16 {
			t.Fatalf("party frame custom name header truncated: body=% X", frame.Body)
		}
		nameLen := int(binary.LittleEndian.Uint32(frame.Body[12:16]))
		slotCountOffset = 16 + nameLen + 13
	}
	if slotCountOffset >= len(frame.Body)-4 {
		t.Fatalf("party frame slot offset=%d exceeds body len=%d body=% X", slotCountOffset, len(frame.Body), frame.Body)
	}
	if frame.Body[slotCountOffset] != wantMembers {
		t.Fatalf("party frame slots=%d want=%d body=% X", frame.Body[slotCountOffset], wantMembers, frame.Body)
	}
	// The byte at len-3 is the authoritative leader slot. Party activity is
	// represented by the current slot-table count.
	active := frame.Body[slotCountOffset] > 0
	if active != wantActive {
		t.Fatalf("party frame active=%t want=%t body=% X", active, wantActive, frame.Body)
	}
	return rest
}

func assertCurrentPartyFrameSelectedSlot(t *testing.T, data []byte, want byte) {
	t.Helper()
	var frame dnfproto.ChannelPacket
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina) {
			frame = packet
			break
		}
		data = rest
	}
	if frame.Header.Classification != 0 || frame.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) {
		t.Fatalf("party frame packet = class %d msg %d", frame.Header.Classification, frame.Header.MsgID)
	}
	if len(frame.Body) < 4 {
		t.Fatalf("party frame body too short for leader slot: len=%d body=% X", len(frame.Body), frame.Body)
	}
	wantTail := []byte{0, want, 0, 0}
	if !bytes.Equal(frame.Body[len(frame.Body)-4:], wantTail) {
		t.Fatalf("party frame leader tail=% X want=% X", frame.Body[len(frame.Body)-4:], wantTail)
	}
}
