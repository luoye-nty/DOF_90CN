package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func currentGuildTestDSTR(value string) []byte {
	var writer packetWriter
	writeClientNameDstr(&writer, value)
	return writer.bytes()
}

// isCurrentGuildAuraBody reports whether body is the 公会徽章 (guild aura)
// appearance block that writeCurrentGuildAuraState emits.
//
// It matters because that block rides opcode CmdPacketSetUDPIPPort -- the very
// same opcode the actor creation/binding packets use -- so a test that walks a
// wire capture looking for the actor packet now meets the aura first. The block
// is a fixed 32 bytes opening with the mode-6 scene-object route header, while
// every mode0/mode1 body is far longer, so this discriminates cleanly.
func isCurrentGuildAuraBody(body []byte) bool {
	return len(body) == 32 && body[0] == 6 && body[1] == 1 && body[2] == 0 && body[3] == currentSceneObjectRoute
}

func assertCurrentGuildActorState(t *testing.T, body []byte, guildID, mark uint32, name string) {
	t.Helper()
	_, tailStart, ok := currentSceneObjectLevelForLog(body)
	if !ok {
		t.Fatalf("missing actor tail: %x", body)
	}
	tail := body[tailStart:]
	equipEnd, ok := currentSceneObjectEquipSummaryEnd(tail, 6)
	if !ok {
		t.Fatalf("invalid actor equipment: %x", tail)
	}
	guildIDOffset := equipEnd + 4 + 4 + 8 + 1
	if got := binary.LittleEndian.Uint32(tail[guildIDOffset:]); got != guildID {
		t.Fatalf("actor guild ID=%d want=%d", got, guildID)
	}
	nameOffset := currentMode0OrganizationDSTRStartForTest(t, tail)
	gotName, next, ok := readCurrentRawDstr(tail, nameOffset)
	if !ok || !bytes.Equal(gotName, rosterNameBytes(name)) {
		t.Fatalf("actor guild name=%x want=%q", gotName, name)
	}
	if got := binary.LittleEndian.Uint32(tail[next:]); got != mark {
		t.Fatalf("actor guild mark=%d want=%d", got, mark)
	}
}

func TestCurrentGuildActorMembershipRefreshLifecycle(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	member := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:2", Name: "member", Level: 80}
	for _, character := range []dnfrepo.CharacterRecord{master, member} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Level = 3
	guild.Mark = 4
	// 审批流用例：公会必须是「接受后加入」(JoinMode 0)，否则默认的立即加入会直接进会。
	guild.JoinMode = 0
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	masterSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: master.AccountID}
	memberSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: member.AccountID}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{1: masterSession, 2: memberSession},
	}
	// Login and peer appearance must resolve the actor's membership, not the viewer's.
	for _, owner := range []byte{currentSceneObjectContext, 1} {
		body := service.buildCurrentSceneObjectListBodyForSessionInContext(ctx, memberSession, 1, master.Name, master, true, owner)
		assertCurrentGuildActorState(t, body, guild.ID, 4, guild.Name)
		member.Stats = map[string]int64{"__scene_guild_id": 99, "__scene_guild_mark": 9}
		body = service.buildCurrentSceneObjectListBodyForSessionInContext(ctx, masterSession, 2, member.Name, member, true, owner)
		assertCurrentGuildActorState(t, body, 0, 0, "")
		if member.Stats["__scene_guild_id"] != 99 || master.Stats != nil {
			t.Fatal("actor projection mutated source character stats")
		}
	}
	assertRefresh := func(session *gameSession, wantID, wantLevel uint32, wantName string) {
		t.Helper()
		seen := false
		for wire := session.conn.(*bufferConn).write.Bytes(); len(wire) != 0; {
			packet, rest := splitGameServerUpperPacket(t, wire)
			wire = rest
			if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) {
				assertCurrentGuildActorState(t, packet.Body, wantID, wantLevel, wantName)
				seen = true
			}
			if packet.Header.MsgID == currentGuildMembershipNotificationMsgID && !seen {
				t.Fatal("guild board updated before actor membership")
			}
		}
		if !seen {
			t.Fatal("missing actor membership refresh")
		}
		session.conn.(*bufferConn).write.Reset()
	}
	applyBody := append(currentGuildTestDSTR(guild.Name), currentGuildTestDSTR("please")...)
	if err := service.handleCurrentGuildApply(memberSession, applyBody); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildApprove(masterSession, []byte{2, 0, 0, 0, currentGuildMemberRegion}); err != nil {
		t.Fatal(err)
	}
	assertRefresh(memberSession, guild.ID, 4, guild.Name)
	if err := service.handleCurrentGuildLeave(memberSession, nil); err != nil {
		t.Fatal(err)
	}
	assertRefresh(memberSession, 0, 0, "")
	if err := service.handleCurrentGuildInvite(masterSession, currentGuildTestDSTR(member.Name)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildInviteReply(memberSession, []byte{1}); err != nil {
		t.Fatal(err)
	}
	assertRefresh(memberSession, guild.ID, 4, guild.Name)
	if err := service.handleCurrentGuildDisband(masterSession, nil); err != nil {
		t.Fatal(err)
	}
	assertRefresh(memberSession, 0, 0, "")
	assertRefresh(masterSession, 0, 0, "")
}

func currentGuildTestInvite(t *testing.T, service *Service, creator *gameSession) *gameSession {
	t.Helper()
	target := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: "dnf:2", characterGeneration: 2}
	creator.characterGeneration = 1
	service.gameSessions = map[uint16]*gameSession{1: creator, 2: target}
	if err := service.handleCurrentGuildCreatePermit(creator, append([]byte{0}, currentGuildTestDSTR("helper")...)); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, target.conn.(*bufferConn).write.Bytes())
	var want packetWriter
	writeClientNameDstr(&want, "master")
	want.writeByte(0)
	writeClientNameDstr(&want, creator.guild.pendingName)
	writeClientNameDstr(&want, "welcome")
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentGuildCreateInviteOpcode ||
		!bytes.Equal(packet.Body, want.bytes()) || len(rest) != 0 {
		t.Fatalf("invite=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	t.Cleanup(func() { service.closeCurrentGuildCreateSession(creator) })
	return target
}

func TestCurrentGuildSearchDecoderMatchesObservedOpenWindowRequest(t *testing.T) {
	request, ok := parseCurrentGuildSearchRequest([]byte{2, 0, 0, 0, 0, 0})
	if !ok || request.Mode != 2 || request.Page != 0 || request.Category != 0 {
		t.Fatalf("request=%+v ok=%t", request, ok)
	}
	nameBody := append([]byte{0}, currentGuildTestDSTR("guild")...)
	request, ok = parseCurrentGuildSearchRequest(nameBody)
	if !ok || request.Query != "guild" {
		t.Fatalf("name request=%+v ok=%t", request, ok)
	}
}

func TestCurrentGuildCreateCommitsGoldRegistryAndNativeProjection(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90, Stats: map[string]int64{"gold": 500000}},
		{CharacterID: "2", AccountID: "dnf:2", Name: "helper", Level: 90, Stats: map[string]int64{"gold": 0}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	session.initialTownRouteCharacterID = 1
	session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	if err := service.handleCurrentGuildNameCheck(session, currentGuildTestDSTR("guild-one")); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildPromoterCheck(session, currentGuildTestDSTR("welcome")); err != nil {
		t.Fatal(err)
	}
	target := currentGuildTestInvite(t, service, session)
	before, _, err := repositories.Character.Load(ctx, "1")
	if err != nil || before.Stats["gold"] != 500000 {
		t.Fatalf("invitation charged gold: %+v err=%v", before, err)
	}
	if err := service.handleCurrentGuildCreatePermitReply(target, []byte{1}); err != nil {
		t.Fatal(err)
	}
	before, _, err = repositories.Character.Load(ctx, "1")
	if err != nil || before.Stats["gold"] != 500000 {
		t.Fatalf("certification charged gold: %+v err=%v", before, err)
	}
	certifiedWireLength := connection.write.Len()
	if err := service.handleCurrentGuildCreatePermitReply(target, []byte{1}); err != nil {
		t.Fatal(err)
	}
	service.expireCurrentGuildCreateInvitation(session.guild.createInvitation)
	if connection.write.Len() != certifiedWireLength || !session.guild.createInvitation.accepted {
		t.Fatal("duplicate reply or stale expiry changed certification")
	}
	if err := service.handleCurrentGuildCreate(session, []byte{1}); err != nil {
		t.Fatal(err)
	}

	character, found, err := repositories.Character.Load(ctx, "1")
	if err != nil || !found || character.Stats["gold"] != 200000 {
		t.Fatalf("character=%+v found=%t error=%v wire=%x", character, found, err, connection.write.Bytes())
	}
	registry, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	guild, member, found := registry.GuildForCharacter("1")
	if !found || guild.Name != "guild-one" || guild.Promotion != "welcome" || member.Grade != dnfguild.DefaultMasterGrade {
		t.Fatalf("guild=%+v member=%+v found=%t", guild, member, found)
	}
	if helperGuild, helper, found := registry.GuildForCharacter("2"); !found || helperGuild.ID != guild.ID || helper.Grade != dnfguild.DefaultMemberGrade {
		t.Fatalf("certifying founder missing: guild=%+v member=%+v", helperGuild, helper)
	}

	wire := connection.write.Bytes()
	want := []struct {
		opcode uint16
		body   []byte
	}{
		{uint16(dnfenum.CmdPacketCheckGuildNameDouble), []byte{1}},
		{currentGuildCreateAuthStateOpcode, []byte{currentGuildCreateAuthResetValue}},
		{uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg), []byte{1}},
		{uint16(dnfenum.CmdPacketRequestGuildCreatePermit), []byte{1}},
		{currentGuildCreateAuthStateOpcode, []byte{1}},
		{uint16(dnfenum.CmdPacketCallGuildCreateRight), []byte{1}},
	}
	for index, expected := range want {
		packet, rest := splitGameServerUpperPacket(t, wire)
		if packet.Header.MsgID != expected.opcode || !bytes.Equal(packet.Body, expected.body) {
			t.Fatalf("packet[%d] opcode=%d body=%x want_opcode=%d want_body=%x", index, packet.Header.MsgID, packet.Body, expected.opcode, expected.body)
		}
		wantClass := byte(1)
		if expected.opcode == currentGuildCreateAuthStateOpcode {
			wantClass = 0
		}
		if packet.Header.Classification != wantClass {
			t.Fatalf("packet[%d] class=%d want=%d", index, packet.Header.Classification, wantClass)
		}
		wire = rest
	}
	goldPacket, rest := splitGameServerUpperPacket(t, wire)
	if goldPacket.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("gold opcode=%d", goldPacket.Header.MsgID)
	}
	actorPacket, rest := splitGameServerUpperPacket(t, rest)
	if actorPacket.Header.Classification != 0 || actorPacket.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) {
		t.Fatalf("actor refresh header=%+v", actorPacket.Header)
	}
	assertCurrentGuildActorState(t, actorPacket.Body, guild.ID, uint32(guild.Mark), guild.Name)
	managementPacket, rest := splitGameServerUpperPacket(t, rest)
	gradePacket, rest := splitGameServerUpperPacket(t, rest)
	if managementPacket.Header.Classification != 0 || managementPacket.Header.MsgID != 70 ||
		gradePacket.Header.Classification != 0 || gradePacket.Header.MsgID != 127 || !bytes.Equal(gradePacket.Body, []byte{1}) {
		t.Fatal("missing created guild management and master grade")
	}
	// writeCurrentGuildManagementState now ends with the 公会徽章 aura block and the
	// DLL-private 公会经验排名 -- upstream places them in exactly this position --
	// so they land between op127 and the membership notification. The new guild
	// owns no Guild_Status benefit, so no 公会 BUFF row appears here.
	auraPacket, rest := splitGameServerUpperPacket(t, rest)
	if auraPacket.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || !isCurrentGuildAuraBody(auraPacket.Body) {
		t.Fatalf("created guild aura header=%+v body=%x", auraPacket.Header, auraPacket.Body)
	}
	rankingPacket, rest := splitGameServerUpperPacket(t, rest)
	if rankingPacket.Header.MsgID != 413 || len(rankingPacket.Body) != 12 {
		t.Fatalf("created guild ranking header=%+v body=%x", rankingPacket.Header, rankingPacket.Body)
	}
	statePacket, rest := splitGameServerUpperPacket(t, rest)
	if statePacket.Header.Classification != 0 ||
		statePacket.Header.MsgID != currentGuildMembershipNotificationMsgID ||
		!bytes.Equal(statePacket.Body, buildCurrentGuildMembershipNotificationBody(guild)) {
		t.Fatalf("state opcode=%d body=%x trailing=%x", statePacket.Header.MsgID, statePacket.Body, rest)
	}
	accountIndexPacket, rest := splitGameServerUpperPacket(t, rest)
	if accountIndexPacket.Header.MsgID != uint16(dnfenum.CmdPacketRequestAccountGuildList) {
		t.Fatalf("account index opcode=%d body=%x", accountIndexPacket.Header.MsgID, accountIndexPacket.Body)
	}
	assertCurrentGuildAccountIndexPacket(t, accountIndexPacket.Body, "welcome", 2, currentGuildTagsMask(guild))
	accountListPacket, rest := splitGameServerUpperPacket(t, rest)
	if accountListPacket.Header.MsgID != uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars) {
		t.Fatalf("account list opcode=%d body=%x", accountListPacket.Header.MsgID, accountListPacket.Body)
	}
	if !bytes.Equal(accountListPacket.Body, []byte{1, 0}) {
		t.Fatalf("new guild master was projected as an applicant: body=%x", accountListPacket.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing create packets=%x", rest)
	}

	connection.write.Reset()
	stalePermit := append([]byte{0}, currentGuildTestDSTR("helper")...)
	if err := service.handleCurrentGuildCreatePermit(session, stalePermit); err != nil {
		t.Fatal(err)
	}
	permitPacket, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if permitPacket.Header.MsgID != uint16(dnfenum.CmdPacketRequestGuildCreatePermit) || !bytes.Equal(permitPacket.Body, []byte{0, 0}) || len(trailing) != 0 {
		t.Fatalf("stale permit opcode=%d body=%x trailing=%x", permitPacket.Header.MsgID, permitPacket.Body, trailing)
	}
	if err := service.handleCurrentGuildCreate(session, []byte{1}); err != nil {
		t.Fatal(err)
	}
	character, found, err = repositories.Character.Load(ctx, "1")
	if err != nil || !found || character.Stats["gold"] != 200000 {
		t.Fatalf("stale permit changed character=%+v found=%t error=%v", character, found, err)
	}
	registry, err = dnfguild.Load(ctx, repositories.Settings)
	if err != nil || len(registry.Guilds) != 1 {
		t.Fatalf("stale permit changed registry=%+v error=%v", registry.Guilds, err)
	}
	helper, _, err := repositories.Character.Load(ctx, "2")
	if err != nil || helper.Stats["gold"] != 0 {
		t.Fatalf("founder gold changed: %+v err=%v", helper, err)
	}
	membershipNotifications := 0
	for wire := target.conn.(*bufferConn).write.Bytes(); len(wire) > 0; {
		packet, rest := splitGameServerUpperPacket(t, wire)
		wire = rest
		if packet.Header.MsgID == currentGuildMembershipNotificationMsgID {
			membershipNotifications++
			if packet.Header.Classification != 0 || !bytes.Equal(packet.Body, buildCurrentGuildMembershipNotificationBody(guild)) {
				t.Fatalf("founder membership=%+v", packet)
			}
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) {
			assertCurrentGuildActorState(t, packet.Body, guild.ID, uint32(guild.Mark), guild.Name)
		}
	}
	if membershipNotifications != 1 {
		t.Fatalf("founder membership notifications=%d", membershipNotifications)
	}
}

func TestCurrentGuildCreateRejectsInsufficientGoldWithoutPersistingGuild(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90, Stats: map[string]int64{"gold": 299999}},
		{CharacterID: "2", AccountID: "dnf:2", Name: "helper", Level: 90},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildNameCheck(session, currentGuildTestDSTR("guild-poor")); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildPromoterCheck(session, currentGuildTestDSTR("welcome")); err != nil {
		t.Fatal(err)
	}
	target := currentGuildTestInvite(t, service, session)
	if err := service.handleCurrentGuildCreatePermitReply(target, []byte{1}); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	if err := service.handleCurrentGuildCreate(session, []byte{1}); err != nil {
		t.Fatal(err)
	}

	character, found, err := repositories.Character.Load(ctx, "1")
	if err != nil || !found || character.Stats["gold"] != 299999 {
		t.Fatalf("character=%+v found=%t error=%v", character, found, err)
	}
	registry, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Guilds) != 0 {
		t.Fatalf("guild registry changed on rejected create: %+v", registry.Guilds)
	}

	wire := connection.write.Bytes()
	packet, rest := splitGameServerUpperPacket(t, wire)
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketCallGuildCreateRight) || !bytes.Equal(packet.Body, []byte{0, currentGuildCreateGoldError}) || len(rest) != 0 {
		t.Fatalf("failure opcode=%d body=%x trailing=%x", packet.Header.MsgID, packet.Body, rest)
	}
}

func TestCurrentGuildCertificationLifecycle(t *testing.T) {
	for _, scenario := range []string{"reject", "cancel", "disconnect", "character-change", "replacement", "expire", "malformed", "forged-creator", "member-changed", "name-changed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			for _, c := range []dnfrepo.CharacterRecord{
				{CharacterID: "1", AccountID: "dnf:1", Name: "master", Stats: map[string]int64{"gold": 500000}},
				{CharacterID: "2", AccountID: "dnf:2", Name: "helper"},
			} {
				if err := repositories.Character.Save(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			service := &Service{options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
			creator := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: "dnf:1"}
			if err := service.handleCurrentGuildNameCheck(creator, currentGuildTestDSTR("guild-flow")); err != nil {
				t.Fatal(err)
			}
			if err := service.handleCurrentGuildPromoterCheck(creator, currentGuildTestDSTR("welcome")); err != nil {
				t.Fatal(err)
			}
			target := currentGuildTestInvite(t, service, creator)
			invite := creator.guild.createInvitation
			creator.conn.(*bufferConn).write.Reset()
			target.conn.(*bufferConn).write.Reset()
			var err error
			cleared := true
			switch scenario {
			case "reject":
				err = service.handleCurrentGuildCreatePermitReply(target, []byte{0})
			case "cancel":
				err = service.handleCurrentGuildCreateCancel(creator, nil)
			case "disconnect":
				service.unbindGameSession(target)
			case "character-change":
				service.bindGameSessionCharacter(target, 3)
			case "replacement":
				service.bindGameSessionCharacter(&gameSession{conn: &bufferConn{}, accountID: "dnf:2"}, 2)
			case "expire":
				service.expireCurrentGuildCreateInvitation(invite)
			case "malformed":
				for _, body := range [][]byte{nil, {2}, {1, 0}} {
					if err := service.handleCurrentGuildCreatePermitReply(target, body); err != nil {
						t.Fatal(err)
					}
				}
				cleared = false
			case "forged-creator":
				err = service.handleCurrentGuildCreatePermitReply(creator, []byte{1})
				cleared = false
			case "member-changed", "name-changed":
				if err := service.handleCurrentGuildCreatePermitReply(target, []byte{1}); err != nil {
					t.Fatal(err)
				}
				if scenario == "member-changed" {
					registry := dnfguild.NewRegistry()
					member, _, _ := repositories.Character.Load(ctx, "2")
					if _, err := registry.Create(member, "other-guild", serviceTestNow()); err != nil {
						t.Fatal(err)
					}
					if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
						t.Fatal(err)
					}
					cleared = false
				} else {
					err = service.handleCurrentGuildNameCheck(creator, currentGuildTestDSTR("new-name"))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if cleared && (creator.guild.createInvitation != nil || target.guild.createInvitation != nil) {
				t.Fatal("invitation survived cleanup")
			}
			if scenario == "reject" || scenario == "cancel" || scenario == "expire" {
				packet, _ := splitGameServerUpperPacket(t, creator.conn.(*bufferConn).write.Bytes())
				state := currentGuildCreateAuthResetValue
				if scenario == "reject" {
					state = 0
				}
				if packet.Header.Classification != 0 || packet.Header.MsgID != currentGuildCreateAuthStateOpcode || !bytes.Equal(packet.Body, []byte{state}) {
					t.Fatalf("reset class/op/body=%d/%d/%x", packet.Header.Classification, packet.Header.MsgID, packet.Body)
				}
				if scenario != "reject" {
					packet, _ = splitGameServerUpperPacket(t, target.conn.(*bufferConn).write.Bytes())
					if packet.Header.Classification != 0 || packet.Header.MsgID != currentGuildCreateCancelOpcode || len(packet.Body) != 0 {
						t.Fatalf("cancel=%+v", packet)
					}
				}
			}
			// A late reply from the retired dialog must not authorize creation.
			if cleared {
				if err := service.handleCurrentGuildCreatePermitReply(target, []byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.handleCurrentGuildCreate(creator, []byte{1}); err != nil {
				t.Fatal(err)
			}
			character, _, err := repositories.Character.Load(ctx, "1")
			if err != nil || character.Stats["gold"] != 500000 {
				t.Fatalf("gold changed: %+v err=%v", character, err)
			}
			registry, err := dnfguild.Load(ctx, repositories.Settings)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, found := registry.GuildForCharacter("1"); found {
				t.Fatal("unexpected guild creation")
			}
		})
	}
}

func TestCurrentGuildPermitReplyCannotBypassOwnedPromoterValidation(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90, Stats: map[string]int64{"gold": 500000},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildNameCheck(session, currentGuildTestDSTR("no-bypass")); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildCreatePermitReply(session, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildCreate(session, []byte{1}); err != nil {
		t.Fatal(err)
	}

	character, found, err := repositories.Character.Load(ctx, "1")
	if err != nil || !found || character.Stats["gold"] != 500000 {
		t.Fatalf("character=%+v found=%t error=%v", character, found, err)
	}
	registry, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Guilds) != 0 {
		t.Fatalf("guild registry changed after forged permit reply: %+v", registry.Guilds)
	}

	wire := connection.write.Bytes()
	for index := 0; index < 3; index++ {
		_, wire = splitGameServerUpperPacket(t, wire)
	}
	packet, rest := splitGameServerUpperPacket(t, wire)
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketCallGuildCreateRight) || !bytes.Equal(packet.Body, []byte{0, currentGuildCreateStateError}) || len(rest) != 0 {
		t.Fatalf("failure opcode=%d body=%x trailing=%x", packet.Header.MsgID, packet.Body, rest)
	}
}

func TestCurrentGuildCreatePermitRejectsOfflinePromoter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90},
		{CharacterID: "2", AccountID: "dnf:2", Name: "outsider", Level: 90},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildNameCheck(session, currentGuildTestDSTR("guild-owned")); err != nil {
		t.Fatal(err)
	}
	session.guild.promotion = "welcome"
	service.gameSessions = map[uint16]*gameSession{1: session}
	if err := service.handleCurrentGuildCreatePermit(session, append([]byte{0}, currentGuildTestDSTR("outsider")...)); err != nil {
		t.Fatal(err)
	}

	wire := connection.write.Bytes()
	for index := 0; index < 2; index++ {
		_, wire = splitGameServerUpperPacket(t, wire)
	}
	packet, rest := splitGameServerUpperPacket(t, wire)
	// op741 must answer the shared guild-failure code, never 104: the current
	// EXE renders 104 as the "wait for the 06:00 daily reset" text (dstr
	// 40150), a rule this server does not implement.
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestGuildCreatePermit) || !bytes.Equal(packet.Body, []byte{0, currentGuildCreatePermitReject}) || len(rest) != 0 {
		t.Fatalf("permit opcode=%d body=%x trailing=%x", packet.Header.MsgID, packet.Body, rest)
	}
	if currentGuildCreatePermitReject == currentGuildCreateStateError {
		t.Fatal("op741 rejection must not reuse the 06:00 daily-reset code")
	}
}

func TestCurrentGuildApplicationApprovalPersistsMembership(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90},
		{CharacterID: "2", AccountID: "dnf:1", Name: "member", Level: 80},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}, "guild-join", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	// 审批流用例：显式使用「接受后加入」。
	guild.JoinMode = 0
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	memberSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: "dnf:1"}
	applyBody := append(currentGuildTestDSTR(guild.Name), currentGuildTestDSTR("please")...)
	if err := service.handleCurrentGuildApply(memberSession, applyBody); err != nil {
		t.Fatal(err)
	}
	masterSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: "dnf:1"}
	var approveBody packetWriter
	approveBody.writeUint32(2)
	// Region byte echoed from the applicant row; 0 is rejected.
	approveBody.writeByte(currentGuildMemberRegion)
	if err := service.handleCurrentGuildApprove(masterSession, approveBody.bytes()); err != nil {
		t.Fatal(err)
	}

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	joinedGuild, joined, found := reloaded.GuildForCharacter("2")
	if !found || joinedGuild.ID != guild.ID || joined.Grade != dnfguild.DefaultMemberGrade {
		t.Fatalf("guild=%+v member=%+v found=%t", joinedGuild, joined, found)
	}
	if len(joinedGuild.Applications) != 0 {
		t.Fatalf("applications were not cleared: %+v", joinedGuild.Applications)
	}
}

// The client has no 签到 button and signs in automatically on the first login of
// the day, so the grant has to happen server-side: before this hook nothing but
// the op355 handler called Registry.Attend, the client never sends op355, and
// 公会积分 therefore listed nothing while the window still showed 已签到.
func TestCurrentGuildAttendanceGrantsOneMileageRowPerLocalDay(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, master); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-sign", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.applyCurrentGuildAttendance("1", "test")
	service.applyCurrentGuildAttendance("1", "test")

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, found := reloaded.GuildByID(guild.ID)
	if !found {
		t.Fatal("guild vanished")
	}
	if saved.Mileage != dnfguild.MileageSignInPoints || len(saved.MileageHistory) != 1 {
		t.Fatalf("mileage=%d history=%d want one daily grant of %d",
			saved.Mileage, len(saved.MileageHistory), dnfguild.MileageSignInPoints)
	}
	if entry := saved.MileageHistory[0]; entry.CharacterID != "1" || entry.Name != "master" || entry.Amount != dnfguild.MileageSignInPoints {
		t.Fatalf("mileage entry=%+v", entry)
	}
	// The window reads the granted row from op747 and the names from op746.
	rows := currentGuildMileageRows(saved)
	if len(rows) != 1 || len(rows[0]) != currentGuildMileageRowSize {
		t.Fatalf("mileage rows=%d", len(rows))
	}
	if got := rows[0][0x22]; got != currentGuildMileageRowKind {
		t.Fatalf("master reason byte=%#x want %#x (公会会长每日签到)", got, currentGuildMileageRowKind)
	}
	// A non-master row must carry the member wording: the client renders the row
	// byte as 21000-byte, so 0x0B printed 公会会长每日签到 (20989) for every row
	// until the member rows started sending 2 (20998).
	saved.Members["2"] = dnfguild.Member{CharacterID: "2", Name: "member", Grade: dnfguild.DefaultMemberGrade}
	saved.MileageHistory = append(saved.MileageHistory, dnfguild.MileageHistory{
		CharacterID: "2", Name: "member", Amount: 1, CreatedAt: service.gameplayNow(),
	})
	rows = currentGuildMileageRows(saved)
	if len(rows) != 2 {
		t.Fatalf("mileage rows=%d", len(rows))
	}
	if got := rows[1][0x22]; got != currentGuildMileageMemberReason {
		t.Fatalf("member reason byte=%#x want %#x (公会成员每日签到)",
			got, currentGuildMileageMemberReason)
	}
	if got := 21000 - int(currentGuildMileageMemberReason); got != 20998 {
		t.Fatalf("member reason maps to id %d want 20998", got)
	}
	if got := 21000 - int(currentGuildMileageRowKind); got != 20989 {
		t.Fatalf("master reason maps to id %d want 20989", got)
	}
	detail := buildCurrentGuildAttendanceDetailBody(saved, service.gameplayNow())
	if name, _, ok := parseCurrentGuildDSTR(detail, 2); !ok || name != "master" {
		t.Fatalf("attendance detail=%x", detail)
	}
}

// The first version of the online attendance hook called mutateCurrentGuild
// directly. guildMu is a plain mutex and the online publish can already run
// inside a guild critical section, so a login deadlocked its own session
// goroutine: the client hung on the loading screen and both sessions stopped
// answering (live 2026-09-12 01:33). The grant must therefore never wait for the
// lock.
func TestCurrentGuildAttendanceNeverWaitsForTheGuildLock(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: "dnf:1"}
	service.guildMu.Lock()
	done := make(chan struct{})
	go func() {
		service.grantCurrentGuildAttendance(session, "test")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		service.guildMu.Unlock()
		t.Fatal("grant blocked on guildMu: the town entry path would hang")
	}
	service.guildMu.Unlock()
}

// 公会贡献 rules beyond 签到, from the client's own help texts 76513/76515:
// 公会聊天 pays 2 once per local day, 同公会成员组队通关地下城 pays 1 per clear when
// the party holds at least two members of the actor's guild. Each grant writes its
// own row reason byte, because the client renders the row label as 21000-byte and
// a zero byte would print the 签到 wording instead.
func TestCurrentGuildMileageChatAndPartyRules(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, master); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "mileageguild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Members["2"] = dnfguild.Member{CharacterID: "2", Name: "guildmate", Grade: dnfguild.DefaultMemberGrade}
	registry.Guilds[guild.ID] = guild
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	now := service.gameplayNow()
	service.applyCurrentGuildMileageGrant("1", 0, "test_chat", func(registry *dnfguild.Registry, id string, at time.Time) (bool, uint32, error) {
		applied, err := registry.GrantChatMileage(id, at)
		return applied, dnfguild.MileageChatPoints, err
	})
	// A second chat on the same local day must not pay again.
	service.applyCurrentGuildMileageGrant("1", 0, "test_chat", func(registry *dnfguild.Registry, id string, at time.Time) (bool, uint32, error) {
		applied, err := registry.GrantChatMileage(id, at)
		return applied, dnfguild.MileageChatPoints, err
	})
	// A solo clear pays nothing, a clear with one guildmate pays 1.
	service.applyCurrentGuildMileageGrant("1", 1, "test_solo", func(registry *dnfguild.Registry, id string, at time.Time) (bool, uint32, error) {
		applied, err := registry.GrantPartyMileage(id, []string{"1"}, at)
		return applied, dnfguild.MileagePartyPoints, err
	})
	service.applyCurrentGuildMileageGrant("1", 2, "test_party", func(registry *dnfguild.Registry, id string, at time.Time) (bool, uint32, error) {
		applied, err := registry.GrantPartyMileage(id, []string{"1", "2"}, at)
		return applied, dnfguild.MileagePartyPoints, err
	})
	// A guildmate from another guild must not count towards the two.
	service.applyCurrentGuildMileageGrant("1", 2, "test_stranger", func(registry *dnfguild.Registry, id string, at time.Time) (bool, uint32, error) {
		applied, err := registry.GrantPartyMileage(id, []string{"1", "9"}, at)
		return applied, dnfguild.MileagePartyPoints, err
	})

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, found := reloaded.GuildByID(guild.ID)
	if !found {
		t.Fatal("guild vanished")
	}
	want := dnfguild.MileageChatPoints + dnfguild.MileagePartyPoints
	if saved.Mileage != want || len(saved.MileageHistory) != 2 {
		t.Fatalf("mileage=%d history=%d want %d over two rows",
			saved.Mileage, len(saved.MileageHistory), want)
	}
	chat, party := saved.MileageHistory[0], saved.MileageHistory[1]
	if chat.Amount != dnfguild.MileageChatPoints || chat.Reason != dnfguild.MileageReasonChat {
		t.Fatalf("chat row=%+v", chat)
	}
	if party.Amount != dnfguild.MileagePartyPoints || party.Reason != dnfguild.MileageReasonParty {
		t.Fatalf("party row=%+v", party)
	}
	if saved.Members["1"].ChatAt.IsZero() {
		t.Fatal("chat day marker not recorded")
	}
	rows := currentGuildMileageRows(saved)
	if len(rows) != 2 {
		t.Fatalf("mileage rows=%d", len(rows))
	}
	if got := rows[0][0x22]; got != dnfguild.MileageReasonChat {
		t.Fatalf("chat row byte=%#x want %#x", got, dnfguild.MileageReasonChat)
	}
	if got := rows[1][0x22]; got != dnfguild.MileageReasonParty {
		t.Fatalf("party row byte=%#x want %#x", got, dnfguild.MileageReasonParty)
	}
	// The party row is repeatable: a second qualifying clear pays again, while the
	// chat rule stays closed for the rest of the local day.
	if _, err := reloaded.GrantPartyMileage("1", []string{"1", "2"}, now); err != nil {
		t.Fatal(err)
	}
	if applied, err := reloaded.GrantChatMileage("1", now); err != nil || applied {
		t.Fatalf("second chat applied=%t err=%v", applied, err)
	}
	saved, found = reloaded.GuildByID(guild.ID)
	if !found || saved.Mileage != want+dnfguild.MileagePartyPoints {
		t.Fatalf("mileage=%d want %d after the second clear", saved.Mileage, want+dnfguild.MileagePartyPoints)
	}
}

// probe= must sweep exactly the listed reason bytes, which is how the newer
// rules' labels get read off the live window without a rebuild.
func TestCurrentGuildMileageProbeRowsSweepListedBytes(t *testing.T) {
	rows := currentGuildMileageProbeRows("2, 10,0x0b,12")
	if len(rows) != 4 {
		t.Fatalf("probe rows=%d want 4", len(rows))
	}
	for index, want := range []byte{2, 10, 0x0B, 12} {
		if got := rows[index][0x22]; got != want {
			t.Fatalf("row %d byte=%#x want %#x", index, got, want)
		}
	}
}

// op746's handler sub_1D04F90 reads a u16 count and then one length-prefixed
// client name per row, so the body must be a name list -- not the u32 zero an
// earlier version sent (safe only because its first two bytes are the u16 zero)
// and certainly not 48-byte rows.
func TestCurrentGuildAttendanceDetailBodyListsTodaysAttendees(t *testing.T) {
	now := serviceTestNow()
	guild := &dnfguild.Guild{Members: map[string]dnfguild.Member{
		"1": {CharacterID: "1", Name: "master", Grade: 1, AttendedAt: now},
		"2": {CharacterID: "2", Name: "member", Grade: 4, AttendedAt: now.Add(-24 * time.Hour)},
	}}
	body := buildCurrentGuildAttendanceDetailBody(guild, now)
	if len(body) < 2 || binary.LittleEndian.Uint16(body) != 1 {
		t.Fatalf("attendance detail body=%x want one names row", body)
	}
	name, next, ok := parseCurrentGuildDSTR(body, 2)
	if !ok || name != "master" || next != len(body) {
		t.Fatalf("attendance detail name=%q ok=%t next=%d body=%x", name, ok, next, body)
	}
	empty := buildCurrentGuildAttendanceDetailBody(nil, now)
	if len(empty) != 2 || binary.LittleEndian.Uint16(empty) != 0 {
		t.Fatalf("empty attendance detail body=%x want u16 zero", empty)
	}
}

// op747 must carry a u32 row count after the success byte: the client's handler
// 0x01D11310 reads it and then memcpys 0x30 bytes per row (call 0x34E3AC0 at
// 01D11380). The old stub answered with the success byte alone, so the client
// read a count out of invalid memory and crashed with 0xC0000005 at 0x03B10747
// (eax=0x098B101F, access 0x098B1000) as soon as 公会积分 was clicked.
func TestCurrentGuildMileageHistoryBodyCarriesCountAndFortyEightByteRows(t *testing.T) {
	guild := &dnfguild.Guild{
		MasterCharacterID: "7",
		Members: map[string]dnfguild.Member{
			"7": {CharacterID: "7", Name: "member", Grade: dnfguild.DefaultMasterGrade},
		},
		MileageHistory: []dnfguild.MileageHistory{{
			CharacterID: "7", Name: "member", Amount: 3, CreatedAt: time.Unix(1700000000, 0),
		}},
	}

	t.Setenv("DNF90_GUILD_MILEAGE_ROWS", "empty")
	body := currentGuildMileageHistoryBody(currentGuildMileageRows(guild))
	if len(body) != 4 || binary.LittleEndian.Uint32(body) != 0 {
		t.Fatalf("empty mileage payload=%x want count0", body)
	}

	t.Setenv("DNF90_GUILD_MILEAGE_ROWS", "real")
	body = currentGuildMileageHistoryBody(currentGuildMileageRows(guild))
	if len(body) != 4+currentGuildMileageRowSize || binary.LittleEndian.Uint32(body) != 1 {
		t.Fatalf("real mileage payload=%x want count1+%d bytes", body, currentGuildMileageRowSize)
	}
	row := body[4:]
	if got := binary.LittleEndian.Uint32(row[0x00:]); got != 1700000000 {
		t.Fatalf("mileage row time=%d body=%x", got, body)
	}
	if !bytes.HasPrefix(row[0x04:0x20], []byte("member")) {
		t.Fatalf("mileage row name=%q", row[0x04:0x20])
	}
	if got := binary.LittleEndian.Uint16(row[0x20:]); got != 0 {
		t.Fatalf("mileage row +0x20=%d want 0 (unmapped)", got)
	}
	if row[0x22] != currentGuildMileageRowKind {
		// +0x22 is the reason byte; 0x0B is the value the client itself seeds and
		// the one capture that rendered 公会成员每日签到 instead of the unrelated
		// text of id 21000.
		t.Fatalf("mileage row reason byte=%#x want %#x", row[0x22], currentGuildMileageRowKind)
	}
	if got := binary.LittleEndian.Uint32(row[0x24:]); got != 7 {
		t.Fatalf("mileage row character id=%d", got)
	}
	if got := binary.LittleEndian.Uint32(row[0x28:]); got != 3 {
		t.Fatalf("mileage row points=%d", got)
	}

	t.Setenv("DNF90_GUILD_MILEAGE_ROWS", "probe3")
	body = currentGuildMileageHistoryBody(currentGuildMileageRows(guild))
	if len(body) != 4+3*currentGuildMileageRowSize || binary.LittleEndian.Uint32(body) != 3 {
		t.Fatalf("probe mileage payload len=%d count=%d", len(body), binary.LittleEndian.Uint32(body))
	}
	if first := body[4:]; first[0x22] != 0 {
		t.Fatalf("probe row 0 reason byte=%#x want 0", first[0x22])
	}
	// A malformed row must be dropped before the count is written.
	body = currentGuildMileageHistoryBody([][]byte{{1, 2, 3}})
	if len(body) != 4 || binary.LittleEndian.Uint32(body) != 0 {
		t.Fatalf("malformed mileage row leaked into the count: %x", body)
	}
}

// op70's byte right after the guild-name DSTR lands at guildManager+0x30 and is
// the only thing that enables the 公会频道设置 button (00FABC98 test eax,eax /
// je -> disabled, tooltip 31583 "只有 \"%s\" 才能使用。"). It carried 0, so the
// master could not press it either. The dialog's own save check tests permission
// 1 of the viewer's grade row, so the byte mirrors that bit.
func TestCurrentGuildManagementBodyGatesChannelButtonOnViewerPermission(t *testing.T) {
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(
		dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90},
		"guild-flag", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	channelByte := func(viewerGrade byte) byte {
		t.Helper()
		body := buildCurrentGuildManagementBody(guild, 0, serviceTestNow(), viewerGrade)
		name, next, ok := parseCurrentGuildDSTR(body, 0)
		if !ok || name != "guild-flag" {
			t.Fatalf("management body name=%q ok=%t body=%x", name, ok, body)
		}
		return body[next]
	}
	if got := channelByte(dnfguild.DefaultMasterGrade); got != 1 {
		t.Fatalf("master channel-setting byte=%d want 1 (grade 1 mask has permission 1)", got)
	}
	if got := channelByte(4); got != 0 {
		t.Fatalf("plain member channel-setting byte=%d want 0 (grade 4 mask is empty)", got)
	}
	if got := channelByte(100); got != 0 { // sub_1D85270's nonmember sentinel
		t.Fatalf("nonmember channel-setting byte=%d want 0", got)
	}
	for _, tc := range []struct {
		mode string
		want byte
	}{{"grade", 4}, {"master", 0}, {"7", 7}} {
		t.Setenv("DNF90_GUILD_CHANNEL_FLAG", tc.mode)
		if got := channelByte(4); got != tc.want {
			t.Fatalf("override %q byte=%d want=%d", tc.mode, got, tc.want)
		}
	}
}

// op352's applicant card carries four bytes between the name and the message and
// the client maps them by position: job, packed advancement, level, reserved.
// The level sits in the THIRD byte (03197DAF movzx ecx,byte [eax+0x27] feeding
// the 'Lv.%d' literal at 03197DB4) and the first byte reaches the class call at
// 03197BFB together with the advancement, so sending {level, grow, 0, 0} made
// every applicant render another class at Lv.0.
func TestCurrentGuildApplicationRowCarriesJobAdvancementAndLevel(t *testing.T) {
	application := dnfguild.Application{
		CharacterID: "43",
		AccountID:   "dnf:1",
		Name:        "applicant",
		Job:         "4",
		GrowType:    "2",
		Level:       87,
		Message:     "please",
		CreatedAt:   serviceTestNow(),
	}
	body := buildCurrentGuildApplicationListBody([]dnfguild.Application{application})
	if got := binary.LittleEndian.Uint32(body[:4]); got != 1 {
		t.Fatalf("applicant count=%d body=%x", got, body)
	}
	offset := 4
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 43 {
		t.Fatalf("applicant id=%d body=%x", got, body)
	}
	offset += 4
	name, next, ok := parseCurrentGuildDSTR(body, offset)
	if !ok || name != "applicant" {
		t.Fatalf("applicant name=%q ok=%t body=%x", name, ok, body)
	}
	if len(body)-next < 6 {
		t.Fatalf("applicant card is truncated: body=%x", body)
	}
	if body[next] != 4 || body[next+1] != 2 || body[next+2] != 87 || body[next+3] != 0 {
		t.Fatalf("applicant card job/advancement/level/reserved=%v want [4 2 87 0] body=%x",
			body[next:next+4], body)
	}
	message, next, ok := parseCurrentGuildDSTR(body, next+4)
	if !ok || message != "please" {
		t.Fatalf("applicant message=%q ok=%t body=%x", message, ok, body)
	}
	if len(body)-next != 6 {
		t.Fatalf("applicant tail=%x", body[next:])
	}
	if body[next+5] != currentGuildMemberRegion {
		t.Fatalf("applicant region=%d want=%d body=%x", body[next+4], currentGuildMemberRegion, body)
	}
}

// 立即加入 has no approver, so the joiner's own client must be told the join
// succeeded BEFORE the membership refresh: the op348 handler rewrites the join
// window from the ack body, and the captured live order (refresh at
// 15:52:21.792-.796, ack at .797) left the window on its pre-join state until it
// was closed and reopened.
func TestCurrentGuildImmediateJoinAcknowledgesBeforeMembershipRefresh(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90},
		{CharacterID: "2", AccountID: "dnf:2", Name: "joiner", Level: 70},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(
		dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90},
		"guild-imm", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	// 立即加入: op740 mode byte 1, which Registry.Create already defaults to.
	if guild.JoinMode != currentGuildImmediateJoinMode {
		t.Fatalf("default join mode=%d want=%d", guild.JoinMode, currentGuildImmediateJoinMode)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:2", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	joinerSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: "dnf:2"}
	if err := service.handleCurrentGuildApply(joinerSession, append(
		currentGuildTestDSTR(guild.Name), currentGuildTestDSTR("please")...)); err != nil {
		t.Fatal(err)
	}

	connection := joinerSession.conn.(*bufferConn)
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != uint16(dnfenum.CmdPacketRequestJoinGuild) {
		t.Fatalf("first packet after 立即加入 is op%d, want the op%d ack body=%x",
			first.Header.MsgID, uint16(dnfenum.CmdPacketRequestJoinGuild), first.Body)
	}
	if len(rest) == 0 {
		t.Fatal("no membership refresh followed the immediate-join ack")
	}

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	joinedGuild, joined, found := reloaded.GuildForCharacter("2")
	if !found || joinedGuild.ID != guild.ID || joined.Grade != dnfguild.DefaultMemberGrade {
		t.Fatalf("immediate join did not persist membership: guild=%+v member=%+v found=%t",
			joinedGuild, joined, found)
	}
	if len(joinedGuild.Applications) != 0 {
		t.Fatalf("immediate join left an application: %+v", joinedGuild.Applications)
	}
}

// op351 (deny) writes the region as a u32 through sub_3455020 while op350
// (approve) writes it as a byte through sub_3454FC0 -- 03198793..031987B7.
// The captured deny body is 2b00000001000000, and only the 5-byte approve
// shape used to be decoded, so every rejection was refused with target 0 and
// the applicant stayed on the list.
func TestCurrentGuildApplicationDenyAcceptsWideRegionAndClearsApplicant(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{
		master,
		{CharacterID: "2", AccountID: "dnf:2", Name: "member", Level: 80},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-deny", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	// 审批流用例：显式使用「接受后加入」。
	guild.JoinMode = 0
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	memberSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: "dnf:2"}
	if err := service.handleCurrentGuildApply(memberSession, append(currentGuildTestDSTR(guild.Name), currentGuildTestDSTR("please")...)); err != nil {
		t.Fatal(err)
	}
	masterSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: "dnf:1"}
	var deny packetWriter
	deny.writeUint32(2)
	deny.writeUint32(uint32(currentGuildMemberRegion))
	if err := service.handleCurrentGuildDeny(masterSession, deny.bytes()); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, masterSession.conn.(*bufferConn).write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketDenyJoinGuild) || packet.Body[0] != 1 {
		t.Fatalf("deny reply=%+v body=%x", packet.Header, packet.Body)
	}
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reloaded.GuildByID(guild.ID)
	if len(saved.Applications) != 0 {
		t.Fatalf("denied application still listed: %+v", saved.Applications)
	}
	if _, joined := saved.Members["2"]; joined {
		t.Fatal("a denied applicant must not become a member")
	}
}

func TestCurrentGuildOpenWindowSearchAndStateAlwaysAnswer(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	// The rich row's u32 is the 公会倾向 bitmask, not a member limit.
	guild.Tags = []byte{0, 2, 5}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	expectedGuild, _, found := registry.GuildForCharacter("1")
	if !found {
		t.Fatal("created guild has no master membership")
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildSearch(session, []byte{2, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildAccountListIndex(session, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildApplicationNotice(session, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentGuildDetail(session, currentGuildTestDSTR("guild-one")); err != nil {
		t.Fatal(err)
	}
	wire := connection.write.Bytes()
	for _, opcode := range []uint16{1452, 1453, 744, currentGuildMembershipNotificationMsgID, 365, 745} {
		packet, rest := splitGameServerUpperPacket(t, wire)
		if packet.Header.MsgID != opcode {
			t.Fatalf("opcode=%d want=%d body=%x", packet.Header.MsgID, opcode, packet.Body)
		}
		if opcode == uint16(dnfenum.CmdPacketSearchGuildList) {
			if len(packet.Body) < 19 || packet.Body[0] != 1 {
				t.Fatalf("search body=%x", packet.Body)
			}
			if total := binary.LittleEndian.Uint32(packet.Body[1:5]); total != 1 {
				t.Fatalf("search total=%d want=1 body=%x", total, packet.Body)
			}
			if packet.Body[5] != 0 || packet.Body[6] != 1 {
				t.Fatalf("search page=%d rows=%d want=0/1 body=%x", packet.Body[5], packet.Body[6], packet.Body)
			}
			if id := binary.LittleEndian.Uint32(packet.Body[7:11]); id != 1 {
				t.Fatalf("search guild id=%d want=1 body=%x", id, packet.Body)
			}
			if packet.Body[11] != 1 || binary.LittleEndian.Uint32(packet.Body[12:16]) != 1|4|32 ||
				binary.LittleEndian.Uint16(packet.Body[16:18]) != 1 || packet.Body[18] != 1 {
				t.Fatalf("search rich row prefix=%x", packet.Body[7:])
			}
			guildName, next, ok := parseCurrentGuildDSTR(packet.Body, 19)
			if !ok || guildName != "guild-one" {
				t.Fatalf("search guild name=%q ok=%t body=%x", guildName, ok, packet.Body)
			}
			masterName, next, ok := parseCurrentGuildDSTR(packet.Body, next)
			if !ok || masterName != "master" {
				t.Fatalf("search master=%q ok=%t body=%x", masterName, ok, packet.Body)
			}
			promotion, next, ok := readCurrentRawDstr(packet.Body, next)
			if !ok || len(promotion) != 0 || next != len(packet.Body) {
				t.Fatalf("search promotion=%x ok=%t next=%d body=%x", promotion, ok, next, packet.Body)
			}
		}
		if opcode == uint16(dnfenum.CmdPacketRequestAccountGuildList) {
			assertCurrentGuildAccountIndexPacket(t, packet.Body, "", 1, currentGuildTagsMask(expectedGuild))
		}
		if opcode == currentGuildMembershipNotificationMsgID {
			if packet.Header.Classification != 0 || !bytes.Equal(packet.Body, buildCurrentGuildMembershipNotificationBody(expectedGuild)) {
				t.Fatalf("final membership state header=%+v body=%x", packet.Header, packet.Body)
			}
		}
		if opcode == uint16(dnfenum.CmdPacketJoinGuildInfo) {
			if want := buildCurrentGuildApplicationNoticeBody(expectedGuild.ID, 0, "master"); !bytes.Equal(packet.Body, want) {
				t.Fatalf("guild application notice body=%x want=%x", packet.Body, want)
			}
		}
		wire = rest
	}
	if len(wire) != 0 {
		t.Fatalf("trailing=%x", wire)
	}
}

func TestCurrentGuildDirectRecommendationSeedsRichGuildCacheFirst(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(character, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildRecommend(session, nil); err != nil {
		t.Fatal(err)
	}
	indexPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if indexPacket.Header.MsgID != uint16(dnfenum.CmdPacketRequestAccountGuildList) {
		t.Fatalf("first opcode=%d want=%d", indexPacket.Header.MsgID, dnfenum.CmdPacketRequestAccountGuildList)
	}
	assertCurrentGuildAccountIndexPacket(t, indexPacket.Body, "", 1, currentGuildTagsMask(guild))
	recommendPacket, rest := splitGameServerUpperPacket(t, rest)
	if recommendPacket.Header.MsgID != uint16(dnfenum.CmdPacketReqRecommendGuild) {
		t.Fatalf("recommend opcode=%d trailing=%x", recommendPacket.Header.MsgID, rest)
	}
	statePacket, rest := splitGameServerUpperPacket(t, rest)
	if statePacket.Header.Classification != 0 ||
		statePacket.Header.MsgID != currentGuildMembershipNotificationMsgID ||
		!bytes.Equal(statePacket.Body, buildCurrentGuildMembershipNotificationBody(guild)) || len(rest) != 0 {
		t.Fatalf("recommend membership state header=%+v body=%x trailing=%x", statePacket.Header, statePacket.Body, rest)
	}
}

func TestCurrentGuildApplicationNoticeBodyUsesApplicantNameAndAccountSlot(t *testing.T) {
	body := buildCurrentGuildApplicationNoticeBody(7, 7, "applicant-character")
	if got := binary.LittleEndian.Uint32(body[:4]); got != 7 {
		t.Fatalf("guild id=%d want=7 body=%x", got, body)
	}
	if body[4] != 7 {
		t.Fatalf("account slot=%d want=7 body=%x", body[4], body)
	}
	name, next, ok := parseCurrentGuildDSTR(body, 5)
	if !ok || name != "applicant-character" || next != len(body) {
		t.Fatalf("member name=%q ok=%t next=%d body=%x", name, ok, next, body)
	}
}

func TestCurrentGuildApplicationNoticeBindsSelectedMemberByAccountSlotAndName(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Slot: 7, Name: "master", Level: 90}
	sameAccountApplicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Slot: 5, Name: "same-account", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, sameAccountApplicant} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Applications[sameAccountApplicant.CharacterID] = dnfguild.Application{
		CharacterID: sameAccountApplicant.CharacterID,
		AccountID:   sameAccountApplicant.AccountID,
		Name:        sameAccountApplicant.Name,
		Level:       sameAccountApplicant.Level,
		CreatedAt:   serviceTestNow(),
	}
	guild.Applications["3"] = dnfguild.Application{
		CharacterID: "3",
		AccountID:   "dnf:other",
		Name:        "external-applicant",
		Level:       90,
		CreatedAt:   serviceTestNow().Add(-time.Minute),
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildApplicationNotice(session, nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketJoinGuildInfo) || len(rest) != 0 {
		t.Fatalf("opcode=%d trailing=%x", packet.Header.MsgID, rest)
	}
	want := buildCurrentGuildApplicationNoticeBody(guild.ID, byte(master.Slot), master.Name)
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("selected member binding body=%x want=%x", packet.Body, want)
	}

}

func TestCurrentGuildApplicationNoticeLeavesPendingNonmemberMappingUntouched(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:owner", Slot: 0, Name: "master", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:applicant", Slot: 5, Name: "applicant", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, applicant} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Applications[applicant.CharacterID] = dnfguild.Application{
		CharacterID: applicant.CharacterID,
		AccountID:   applicant.AccountID,
		Name:        applicant.Name,
		Level:       applicant.Level,
		CreatedAt:   serviceTestNow(),
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: applicant.AccountID, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 2, accountID: applicant.AccountID}
	if err := service.handleCurrentGuildApplicationNotice(session, nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	want := buildCurrentGuildApplicationNoticeBody(0, 0, "")
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketJoinGuildInfo) || len(rest) != 0 || !bytes.Equal(packet.Body, want) {
		t.Fatalf("application notice opcode=%d body=%x want=%x trailing=%x", packet.Header.MsgID, packet.Body, want, rest)
	}
}

func TestCurrentGuildAccountProjectionKeepsPersistedMasterOutOfApplicationRows(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Slot: 7, Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	if _, err := registry.Create(character, "guild-one", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	projection, guilds, err := currentGuildAccountProjection(ctx, repositories, registry, character.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 1 || len(projection) != 1 {
		t.Fatalf("guilds=%d projection=%+v", len(guilds), projection)
	}
	if member := guilds[0].Members[character.CharacterID]; member.Grade != dnfguild.DefaultMasterGrade {
		t.Fatalf("persisted master grade=%d want=%d", member.Grade, dnfguild.DefaultMasterGrade)
	}
	applications, applicationGuilds, err := currentGuildAccountApplicationProjection(ctx, repositories, registry, character.AccountID, character.CharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 0 || len(applicationGuilds) != 0 {
		t.Fatalf("master application projection=%+v guilds=%+v", applications, applicationGuilds)
	}
}

func TestCurrentGuildAccountIndexUsesAdvertisedRegionForEveryRosterSlot(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:shared", Slot: 0, Name: "master-zero", Level: 90},
		{CharacterID: "2", AccountID: "dnf:shared", Slot: 1, Name: "master-one", Level: 90},
	}
	registry := dnfguild.NewRegistry()
	for index, character := range characters {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Create(character, fmt.Sprintf("guild-slot-%d", index), serviceTestNow()); err != nil {
			t.Fatal(err)
		}
	}
	projection, guilds, err := currentGuildAccountProjection(ctx, repositories, registry, "dnf:shared")
	if err != nil {
		t.Fatal(err)
	}
	body := buildCurrentGuildAccountIndexBody(guilds, projection)
	if len(body) == 0 || body[0] != 2 {
		t.Fatalf("guild row count body=%x", body)
	}
	offset := 1
	for row := 0; row < 2; row++ {
		if offset > len(body)-12 {
			t.Fatalf("truncated rich row[%d] body=%x", row, body)
		}
		offset += 12
		for field := 0; field < 3; field++ {
			_, next, ok := readCurrentRawDstr(body, offset)
			if !ok {
				t.Fatalf("invalid rich row[%d] string[%d] body=%x", row, field, body)
			}
			offset = next
		}
	}
	if offset >= len(body) || body[offset] != 2 {
		t.Fatalf("mapping count offset=%d body=%x", offset, body)
	}
	offset++
	for mapping, wantGuildID := range []uint32{1, 2} {
		if offset > len(body)-5 {
			t.Fatalf("truncated mapping[%d] body=%x", mapping, body)
		}
		if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != wantGuildID {
			t.Fatalf("mapping[%d] guild=%d want=%d body=%x", mapping, got, wantGuildID, body)
		}
		offset += 4
		if region := body[offset]; region != 0 {
			t.Fatalf("mapping[%d] region=%d want=0 body=%x", mapping, region, body)
		}
		offset++
		_, next, ok := parseCurrentGuildDSTR(body, offset)
		if !ok {
			t.Fatalf("invalid mapping[%d] character name body=%x", mapping, body)
		}
		offset = next
	}
	if offset != len(body) {
		t.Fatalf("trailing account index bytes=%x", body[offset:])
	}
}

func TestCurrentGuildAccountApplicationProjectionContainsOnlyRealPendingRows(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:owner", Slot: 0, Name: "master", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:applicant", Slot: 5, Name: "applicant", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, applicant} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(guild.ID, applicant, "join", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	projection, guilds, err := currentGuildAccountApplicationProjection(ctx, repositories, registry, applicant.AccountID, applicant.CharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection) != 1 || len(guilds) != 1 {
		t.Fatalf("projection=%+v guilds=%+v", projection, guilds)
	}
	if projection[0].CharacterName != applicant.Name ||
		projection[0].GuildName != guild.Name ||
		projection[0].GuildID != guild.ID ||
		projection[0].ApplicationPending != currentGuildApplicationCancelable {
		t.Fatalf("pending application projection=%+v", projection[0])
	}
}

func TestCurrentGuildAccountApplicationProjectionDoesNotLeakAnotherAccountCharacter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:shared", Slot: 0, Name: "master", Level: 90}
	otherCharacter := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:shared", Slot: 1, Name: "other", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, otherCharacter} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "master-guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	otherGuild, err := registry.Create(dnfrepo.CharacterRecord{CharacterID: "3", AccountID: "dnf:other", Slot: 0, Name: "other-master", Level: 90}, "other-guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(otherGuild.ID, otherCharacter, "join", serviceTestNow()); err != nil {
		t.Fatal(err)
	}

	projection, guilds, err := currentGuildAccountApplicationProjection(ctx, repositories, registry, master.AccountID, master.CharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection) != 0 || len(guilds) != 0 {
		t.Fatalf("master must not inherit another character's application: projection=%+v guilds=%+v", projection, guilds)
	}
	projection, guilds, err = currentGuildAccountApplicationProjection(ctx, repositories, registry, otherCharacter.AccountID, otherCharacter.CharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection) != 1 || len(guilds) != 1 || projection[0].CharacterName != otherCharacter.Name || projection[0].GuildID != otherGuild.ID {
		t.Fatalf("selected applicant projection=%+v guilds=%+v", projection, guilds)
	}
	_ = guild
}

func TestCurrentGuildCancelApplyReconcilesStaleCreatorRowWithoutChangingRegistry(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{
		CharacterID: "1",
		AccountID:   "dnf:owner",
		Slot:        0,
		Name:        "master",
		Level:       90,
	}
	if err := repositories.Character.Save(ctx, master); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Promotion = "welcome"
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: master.AccountID, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: master.AccountID}
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, guild.ID)
	if err := service.handleCurrentGuildCancelApply(session, body); err != nil {
		t.Fatal(err)
	}

	wire := connection.write.Bytes()
	cancelPacket, rest := splitGameServerUpperPacket(t, wire)
	if cancelPacket.Header.MsgID != uint16(dnfenum.CmdPacketCancelJoinGuild) || !bytes.Equal(cancelPacket.Body, []byte{1}) {
		t.Fatalf("cancel opcode=%d body=%x", cancelPacket.Header.MsgID, cancelPacket.Body)
	}
	resetPacket, rest := splitGameServerUpperPacket(t, rest)
	if resetPacket.Header.Classification != 0 || resetPacket.Header.MsgID != currentGuildCreateAuthStateOpcode ||
		!bytes.Equal(resetPacket.Body, []byte{currentGuildCreateAuthResetValue}) {
		t.Fatalf("reset opcode=%d body=%x", resetPacket.Header.MsgID, resetPacket.Body)
	}
	indexPacket, rest := splitGameServerUpperPacket(t, rest)
	if indexPacket.Header.MsgID != uint16(dnfenum.CmdPacketRequestAccountGuildList) {
		t.Fatalf("index opcode=%d body=%x", indexPacket.Header.MsgID, indexPacket.Body)
	}
	assertCurrentGuildAccountIndexPacket(t, indexPacket.Body, guild.Promotion, 1, currentGuildTagsMask(guild))
	applicationPacket, rest := splitGameServerUpperPacket(t, rest)
	if applicationPacket.Header.MsgID != uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars) ||
		!bytes.Equal(applicationPacket.Body, []byte{1, 0}) {
		t.Fatalf("application opcode=%d body=%x", applicationPacket.Header.MsgID, applicationPacket.Body)
	}
	membershipPacket, rest := splitGameServerUpperPacket(t, rest)
	if membershipPacket.Header.Classification != 0 ||
		membershipPacket.Header.MsgID != currentGuildMembershipNotificationMsgID ||
		!bytes.Equal(membershipPacket.Body, buildCurrentGuildMembershipNotificationBody(guild)) ||
		len(rest) != 0 {
		t.Fatalf("membership class=%d opcode=%d body=%x trailing=%x", membershipPacket.Header.Classification, membershipPacket.Header.MsgID, membershipPacket.Body, rest)
	}

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	reloadedGuild, member, found := reloaded.GuildForCharacter(master.CharacterID)
	if !found || reloadedGuild.ID != guild.ID || member.Grade != dnfguild.DefaultMasterGrade || len(reloadedGuild.Applications) != 0 {
		t.Fatalf("registry changed during stale-row reconciliation: guild=%+v member=%+v found=%t", reloadedGuild, member, found)
	}
}

func TestCurrentGuildCancelApplyStillRemovesRealApplication(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:owner", Slot: 0, Name: "master", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:applicant", Slot: 0, Name: "applicant", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, applicant} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-one", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(guild.ID, applicant, "join", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: applicant.AccountID, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 2, accountID: applicant.AccountID}
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, guild.ID)
	if err := service.handleCurrentGuildCancelApply(session, body); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketCancelJoinGuild) ||
		!bytes.Equal(packet.Body, []byte{1}) ||
		len(rest) != 0 {
		t.Fatalf("cancel opcode=%d body=%x trailing=%x", packet.Header.MsgID, packet.Body, rest)
	}
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	reloadedGuild, found := reloaded.GuildByID(guild.ID)
	if !found {
		t.Fatal("guild disappeared after application cancellation")
	}
	if _, found := reloadedGuild.Applications[applicant.CharacterID]; found {
		t.Fatalf("real application was not removed: %+v", reloadedGuild.Applications)
	}
}

func TestCurrentGuildMembershipNotificationBodyMatchesCurrentClient(t *testing.T) {
	guild := &dnfguild.Guild{
		ID:         7,
		Name:       "guild-name",
		MasterName: "guild-master",
		Promotion:  "guild-promotion",
		Level:      3,
		JoinMode:   2,
		Members: map[string]dnfguild.Member{
			"1": {},
			"2": {},
		},
	}
	body := buildCurrentGuildMembershipNotificationBody(guild)
	if body[0] != 1 || binary.LittleEndian.Uint32(body[1:5]) != guild.ID {
		t.Fatalf("notification prefix=%x", body)
	}
	offset := 5
	for _, want := range []string{guild.Name, guild.MasterName} {
		got, next, ok := parseCurrentGuildDSTR(body, offset)
		if !ok || got != want {
			t.Fatalf("notification string=%q want=%q body=%x", got, want, body)
		}
		offset = next
	}
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 0 {
		t.Fatalf("notification unused field=%d body=%x", got, body)
	}
	offset += 4
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 2 {
		t.Fatalf("notification member count=%d body=%x", got, body)
	}
	offset += 4
	if body[offset] != guild.JoinMode {
		t.Fatalf("notification join mode=%d body=%x", body[offset], body)
	}
	offset++
	promotion, next, ok := parseCurrentGuildDSTR(body, offset)
	if !ok || promotion != guild.Promotion || next != len(body) {
		t.Fatalf("notification promotion=%q ok=%t next=%d body=%x", promotion, ok, next, body)
	}
	if empty := buildCurrentGuildMembershipNotificationBody(nil); !bytes.Equal(empty, []byte{0}) {
		t.Fatalf("empty notification=%x", empty)
	}
}

func assertCurrentGuildAccountIndexPacket(t *testing.T, body []byte, promotion string, memberCount uint16, tagsMask uint32) {
	t.Helper()
	if len(body) < 14 || body[0] != 1 || body[1] != 1 {
		t.Fatalf("account guild index prefix=%x want success=1 guild-count=1", body)
	}
	offset := 2
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 1 {
		t.Fatalf("account guild id=%d want=1 body=%x", got, body)
	}
	offset += 4
	if body[offset] != 1 {
		t.Fatalf("account guild join mode=%d want=1 body=%x", body[offset], body)
	}
	offset++
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != tagsMask {
		t.Fatalf("account guild tags mask=%d want=%d body=%x", got, tagsMask, body)
	}
	offset += 4
	if got := binary.LittleEndian.Uint16(body[offset : offset+2]); got != memberCount {
		t.Fatalf("account guild member count=%d want=%d body=%x", got, memberCount, body)
	}
	offset += 2
	if body[offset] != 1 {
		t.Fatalf("account guild level=%d want=1 body=%x", body[offset], body)
	}
	offset++
	for field, want := range []string{"guild-one", "master", promotion} {
		if want == "" {
			if offset > len(body)-4 || binary.LittleEndian.Uint32(body[offset:offset+4]) != 0 {
				t.Fatalf("account guild string[%d] is not an empty DSTR body=%x", field, body)
			}
			offset += 4
			continue
		}
		got, next, ok := parseCurrentGuildDSTR(body, offset)
		if !ok || got != want {
			t.Fatalf("account guild string[%d]=%q ok=%t want=%q body=%x", field, got, ok, want, body)
		}
		offset = next
	}
	if offset >= len(body) || body[offset] != 1 {
		t.Fatalf("account character mapping count body=%x offset=%d", body, offset)
	}
	offset++
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 1 {
		t.Fatalf("account mapping guild id=%d want=1 body=%x", got, body)
	}
	offset += 4
	if body[offset] != 0 {
		t.Fatalf("account mapping slot=%d want=0 body=%x", body[offset], body)
	}
	offset++
	name, next, ok := parseCurrentGuildDSTR(body, offset)
	if !ok || name != "master" || next != len(body) {
		t.Fatalf("account mapping name=%q ok=%t next=%d body=%x", name, ok, next, body)
	}
}

func TestCurrentGuildGradeProjectionMatchesCurrentExeFieldOrder(t *testing.T) {
	// The rows carry member names, not grade names: sub_FBE4D0 joins each row
	// against a guild member record on (row string == record name,
	// row+4 == record+0x182) and sub_FBE700 looks rows up by that same pair,
	// so grade names here left the member settings list showing
	// 会长/副会长/优秀会员/普通会员 as if they were the members.
	guild := &dnfguild.Guild{
		ID:         9,
		GradeNames: map[byte]string{1: "master", 2: "vice", 3: "elite", 4: "member"},
		Members: map[string]dnfguild.Member{
			"16": {CharacterID: "16", Name: "founder", Grade: 1},
			"3":  {CharacterID: "3", Name: "deputy", Grade: 2},
			"41": {CharacterID: "41", Name: "regular", Grade: 4},
		},
	}
	body := buildCurrentGuildGradeBody(guild, true)
	if got := binary.LittleEndian.Uint32(body[:4]); got != guild.ID {
		t.Fatalf("guild id=%d", got)
	}
	if got := binary.LittleEndian.Uint16(body[4:6]); got != 3 {
		t.Fatalf("roster total=%d", got)
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 3 {
		t.Fatalf("row count=%d", got)
	}
	offset := 8
	for _, want := range []struct {
		name  string
		grade byte
	}{{"founder", 1}, {"deputy", 2}, {"regular", 4}} {
		// Region byte. sub_FBE4D0 joins this row to a member record on
		// (name, region), so it has to equal what the member list row sends
		// at member+0x182, and sub_26A3590 refuses to send op126 when the
		// menu hands it a 0.
		if body[offset] != currentGuildMemberRegion {
			t.Fatalf("region[%s]=%d want=%d", want.name, body[offset], currentGuildMemberRegion)
		}
		if currentGuildMemberRegion == 0 || currentGuildMemberRegion > 249 {
			t.Fatalf("region %d outside sub_26A3590's 1..249 range", currentGuildMemberRegion)
		}
		offset++
		name, next, ok := parseCurrentGuildDSTR(body, offset)
		if !ok || name != want.name {
			t.Fatalf("member name=%q want=%q ok=%t", name, want.name, ok)
		}
		offset = next
		// The grade drives sub_FBE280's tab filter: 2 -> 副会长, 3 -> 优秀会员,
		// 4 or 0 -> 普通会员. Sending 0 for everyone piled the whole roster
		// under the last tab.
		if body[offset] != want.grade {
			t.Fatalf("grade[%s]=%d want=%d", want.name, body[offset], want.grade)
		}
		offset++
	}
	if offset != len(body) {
		t.Fatalf("row boundary offset=%d len=%d", offset, len(body))
	}
	// op1455 appends to the container op1454 cleared, so the follow-up page
	// has to be empty or every member would be listed twice.
	next := buildCurrentGuildGradeBody(guild, false)
	if len(next) != 6 || binary.LittleEndian.Uint32(next[:4]) != guild.ID || binary.LittleEndian.Uint16(next[4:6]) != 0 {
		t.Fatalf("follow-up page=%x", next)
	}
	if offset != len(body) {
		t.Fatalf("trailing=%x", body[offset:])
	}
}

func TestCurrentGuildNextMemberPageUsesMatchingOpcodeAndCompactLayout(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	if _, err := registry.Create(character, "guild-page", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	if err := service.handleCurrentGuildMemberListNext(session, nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketGuildAllMemberListNext) || len(rest) != 0 {
		t.Fatalf("opcode=%d trailing=%x", packet.Header.MsgID, rest)
	}
	if len(packet.Body) < 7 || packet.Body[0] != 1 || binary.LittleEndian.Uint32(packet.Body[1:5]) != 1 || binary.LittleEndian.Uint16(packet.Body[5:7]) != 1 {
		t.Fatalf("body=%x", packet.Body)
	}
}

func TestCurrentGuildLegacyMemberPagesUseNativeIdentityAndCursorBranches(t *testing.T) {
	guild := &dnfguild.Guild{
		ID:    7,
		Name:  "legacy-guild",
		Level: 3,
		Members: map[string]dnfguild.Member{
			"9": {CharacterID: "9", Name: "member", Level: 90, Grade: 4},
		},
	}
	legacyFirst := buildCurrentGuildMemberListBody(guild, true, false, nil, testGuildMemberListNow())
	currentFirst := buildCurrentGuildMemberListBody(guild, true, true, nil, testGuildMemberListNow())
	if len(currentFirst) != len(legacyFirst)+4 || !bytes.Equal(currentFirst[:len(legacyFirst)], legacyFirst) {
		t.Fatalf("legacy first-page layout=%x current=%x", legacyFirst, currentFirst)
	}
	legacyNext := buildCurrentGuildMemberListBody(guild, false, false, nil, testGuildMemberListNow())
	currentNext := buildCurrentGuildMemberListBody(guild, false, true, nil, testGuildMemberListNow())
	if len(currentNext) != len(legacyNext)+4 || !bytes.Equal(currentNext[:len(legacyNext)], legacyNext) {
		t.Fatalf("legacy next-page layout=%x current=%x", legacyNext, currentNext)
	}
}

func TestCurrentGuildAuxiliaryPacketBodiesMatchCurrentExeEmptyBranches(t *testing.T) {
	if got := buildCurrentGuildRefreshInfoBody(); !bytes.Equal(got, make([]byte, 7)) {
		t.Fatalf("op339 body=%x want seven-byte stable state", got)
	}
	if got := buildCurrentGuildBoardEmptyBody(); !bytes.Equal(got, []byte{0, 0}) {
		t.Fatalf("op343 body=%x", got)
	}
	if got := buildCurrentGuildAttendanceBody(); !bytes.Equal(got, []byte{0, 0}) {
		t.Fatalf("op355 body=%x", got)
	}
	info := buildCurrentGuildInfoBody(19, &dnfguild.Guild{Name: "guild-info"})
	if len(info) < 5 || binary.LittleEndian.Uint32(info[:4]) != 19 {
		t.Fatalf("op74 prefix=%x", info)
	}
	name, next, ok := parseCurrentGuildDSTR(info, 4)
	if !ok || name != "guild-info" || next != len(info) {
		t.Fatalf("op74 name=%q next=%d ok=%t body=%x", name, next, ok, info)
	}
	grade := buildCurrentGuildLegacyGradeBody(3, "member", 2)
	if len(grade) < 4 || grade[0] != 0 || grade[1] != 3 || grade[len(grade)-1] != 2 {
		t.Fatalf("op126 body=%x", grade)
	}
	name, next, ok = parseCurrentGuildDSTR(grade, 2)
	if !ok || name != "member" || next != len(grade)-1 {
		t.Fatalf("op126 name=%q next=%d ok=%t body=%x", name, next, ok, grade)
	}
}

func TestCurrentGuildLegacyGradeCommandPersistsAndEchoesNativeFields(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	member := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Name: "member", Level: 90}
	for _, character := range []dnfrepo.CharacterRecord{master, member} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	if _, err := registry.Create(master, "guild-grade", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Invite(master.CharacterID, member, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ReplyInvitation(member, true, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	session.initialTownRouteCharacterID = 1
	session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	request := append([]byte{3}, currentGuildTestDSTR("member")...)
	request = append(request, 2)
	if err := service.handleCurrentGuildLegacyChangeGrade(session, request); err != nil {
		t.Fatal(err)
	}

	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketSetSubGuildMaster) {
		t.Fatalf("header=%+v trailing=%x", packet.Header, trailing)
	}
	// op127 is followed by the two projections writeCurrentGuildManagementState now
	// ends with: the 公会徽章 aura block (opcode CmdPacketSetUDPIPPort, shared with
	// the actor packets) and the DLL-private 公会经验排名 413. No 公会 BUFF row: this
	// guild carries no Guild_Status benefit.
	for _, msgID := range []uint16{70, 127, uint16(dnfenum.CmdPacketSetUDPIPPort), 413} {
		notice, rest := splitGameServerUpperPacket(t, trailing)
		if notice.Header.Classification != 0 || notice.Header.MsgID != msgID {
			t.Fatalf("grade refresh=%+v want=0/%d", notice.Header, msgID)
		}
		if msgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && !isCurrentGuildAuraBody(notice.Body) {
			t.Fatalf("grade refresh aura body=%x want the 公会徽章 block", notice.Body)
		}
		trailing = rest
	}
	if len(trailing) != 0 {
		t.Fatalf("grade refresh trailing=%x", trailing)
	}
	if len(packet.Body) < 5 || packet.Body[0] != 1 || packet.Body[1] != 0 || packet.Body[2] != 3 || packet.Body[len(packet.Body)-1] != 2 {
		t.Fatalf("body=%x", packet.Body)
	}
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	guild, _, found := reloaded.GuildForCharacter(member.CharacterID)
	if !found || guild.Members[member.CharacterID].Grade != 2 {
		t.Fatalf("guild=%+v found=%t", guild, found)
	}
}

func serviceTestNow() time.Time {
	return time.Date(2026, 8, 19, 12, 0, 0, 0, time.Local)
}
