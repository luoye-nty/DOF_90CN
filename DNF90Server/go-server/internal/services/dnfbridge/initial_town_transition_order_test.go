package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestInitialTownTransitionIsWithheldWhenPlayerStateInitializationFails(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	session.selectedCharacterID = 29
	service.armCurrentInitialTownRoute(session, 29)

	ctx := context.Background()
	characterID, characterName, character, hasCharacter := service.selectedCharacterForEnter(ctx, session)
	if characterID != 29 || !hasCharacter {
		t.Fatalf("selected character id=%d found=%t", characterID, hasCharacter)
	}
	objectBody := service.buildCurrentSceneObjectListBodyForSession(
		ctx,
		session,
		characterID,
		characterName,
		character,
		hasCharacter,
	)
	if eventArg, ok := currentSceneObjectTailActorStateEventArgForLog(objectBody); !ok || eventArg != currentSceneActorStateEventArg {
		t.Fatalf("initial-town mode0 actor-state event arg=%d ok=%t, want generation initialization %d", eventArg, ok, currentSceneActorStateEventArg)
	}
	objectKey := currentSceneActorObjectKey(characterID)
	mode1Body := buildCurrentActorBindingMode1Body(objectKey, 0)
	transitionBody, err := buildCurrentSceneTransitionBody(1, 0, []currentSceneTransitionRow{{
		ObjectOrResourceKey: objectKey,
		Value1:              474,
		Value2:              234,
		Value3:              5,
		Value4:              3,
	}})
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("pre-transition player-state write failed")
	connection := &failNthDungeonWriteConn{failAt: 5, err: wantErr}
	session.conn = connection
	session.townMu.Lock()
	err = service.sendCurrentInitialTownActorRoutePacketsLocked(
		session,
		characterID,
		1,
		0,
		currentSceneTransitionRow{
			ObjectOrResourceKey: objectKey,
			Value1:              474,
			Value2:              234,
			Value3:              5,
			Value4:              3,
		},
		objectBody,
		mode1Body,
		transitionBody,
	)
	stage := session.initialTownRouteStage
	session.townMu.Unlock()
	if !errors.Is(err, wantErr) {
		t.Fatalf("initialization failure=%v want=%v", err, wantErr)
	}
	if stage >= currentInitialTownRouteTransitionSent {
		t.Fatalf("failed initialization committed route stage=%d", stage)
	}

	for stream := connection.write.Bytes(); len(stream) > 0; {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packetLooksLikeInitialTownTransition(packet) {
			t.Fatalf("failed initialization emitted op24 body=%x", packet.Body)
		}
		stream = rest
	}
}

func TestInitialTownPostOp24CommitDoesNotReplaySkillSnapshot(t *testing.T) {
	skillBody := []byte{1, 0, 0, 0, 0x08, 0x21}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                         connection,
		selectedCharacterID:          29,
		initialTownRouteCharacterID:  29,
		initialTownRouteStage:        currentInitialTownRoutePlayerStatePrepared,
		initialTownSkillInfoPrepared: true,
		initialTownSkillInfoSent:     true,
		initialTownSkillInfo: currentSceneSkillInfoProjection{
			body:            append([]byte(nil), skillBody...),
			characterID:     "29",
			job:             0,
			learnedCount:    1,
			learnedSkillIDs: []int64{33},
		},
	}
	row := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(29),
		Value1:              474,
		Value2:              234,
		Value3:              5,
		Value4:              3,
	}
	transitionBody, err := buildCurrentSceneTransitionBody(1, 0, []currentSceneTransitionRow{row})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	if err := service.sendCurrentTownTransitionLocked(session, 29, 1, 0, row, transitionBody); err != nil {
		t.Fatalf("commit town transition: %v", err)
	}
	transition, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if transition.Header.Classification != 0 || transition.Header.MsgID != currentSceneTransitionMsgID || len(rest) != 0 {
		t.Fatalf("transition=%+v body=%x rest=%x", transition.Header, transition.Body, rest)
	}
	if session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent ||
		session.initialTownSkillInfoPrepared || !session.initialTownSkillInfoSent ||
		len(session.initialTownSkillInfo.body) != 0 || session.townSceneReadyCharacterID != 29 {
		t.Fatalf(
			"resumed state stage=%d prepared=%t sent=%t retained=%d ready=%d",
			session.initialTownRouteStage,
			session.initialTownSkillInfoPrepared,
			session.initialTownSkillInfoSent,
			len(session.initialTownSkillInfo.body),
			session.townSceneReadyCharacterID,
		)
	}
	if got := session.townPositionSnapshot; !got.PositionValid || got.CharacterID != 29 || got.TownID != 1 || got.AreaID != 0 || got.PositionX != 474 || got.PositionY != 234 || got.MovementCode != 5 {
		t.Fatalf("typed op24 spawn snapshot=%+v", got)
	}
	if origin, ok := bindCurrentTownSelectorOrigin(session); !ok || !origin.PositionValid || origin.PositionX != 474 || origin.PositionY != 234 {
		t.Fatalf("first selector origin after character switch = %+v found=%t", origin, ok)
	}
	written := connection.write.Len()
	if err := service.sendCurrentTownTransitionLocked(session, 29, 1, 0, row, transitionBody); err != nil {
		t.Fatalf("duplicate completed transition: %v", err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate completed transition replayed %d bytes", connection.write.Len()-written)
	}
}

func TestDeferredSelectSceneTailResumesInitialTownRouteFromHeartbeatWithoutProgress36(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	ctx := context.Background()
	skillCatalog, err := buildSkillCatalogFromSource(ctx, initialEquipmentMemSource{
		"skill/skilllist.lst":    "0 `job0.lst`\n",
		"skill/job0.lst":         "46 `job0/initial.skl`\n",
		"skill/job0/initial.skl": "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.initialSkillsByJob = map[byte][]initialSkillEntry{0: {{SkillID: 46, Level: 1}}}
	service.initialSPTable = map[int]int{90: 3710}
	service.initialTPTable = map[int]int{90: 41}
	service.skillCatalog = skillCatalog
	service.questCatalog = buildQuestListTestCatalog(t)
	service.options.accountPrefix = defaultAccountPrefix
	service.adventureGroupTable = loadAdventureGroupTestTables(t)
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: defaultAccountPrefix + "1",
		Metadata:  map[string]string{currentRentalPointMetadataKey: "0"},
	}); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(ctx, "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 90
	character.Stats[currentDungeonTutorialCompletedKey] = currentDungeonTutorialCompleteFlag
	character.Stats[currentOpenAuraSkinSlotFlagStat] = 1
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(character, "relogin", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}

	session.selectedCharacterID = 29
	session.characterGeneration = 1
	session.townSceneReadyCharacterID = 0
	session.sceneBootstrapTailDeferred = true
	session.channel = channelcatalog.Channel{
		ServerID: 1,
		ID:       11,
		Type:     1,
		Name:     "ch.11",
		Port:     10011,
	}
	session.residentChannel = session.channel
	service.armCurrentInitialTownRoute(session, 29)
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendDeferredSelectSceneTail(session, "upper_check_user_connection_after_ack"); err != nil {
		t.Fatalf("resume scene tail from heartbeat: %v", err)
	}
	if connection.write.Len() == 0 {
		t.Fatal("heartbeat resume wrote no town route or scene tail")
	}
	fullStream := append([]byte(nil), connection.write.Bytes()...)
	actorOwner := currentTownActorOwnerContext(session)
	partyOwner := currentPartySceneOwnerContext(session)
	mode0Count := 0
	mode1Count := 0
	var emittedMode0 []byte
	var emittedMode1 []byte
	op9Count := 0
	lastContainerIndex := -1
	auraRestoreCount := 0
	auraRestoreIndex := -1
	adventureInfoIndex := -1
	packetIndex := 0
	for ownerStream := fullStream; len(ownerStream) > 0; {
		packet, next := splitCurrentGameServerUpperPacketAuto(t, ownerStream)
		ownerStream = next
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketLeaveParty) {
			lastContainerIndex = packetIndex
		}
		if packet.Header.Classification == dnfproto.DefaultChannelClassification &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketOpenAuraSkinSlot) {
			auraRestoreCount++
			auraRestoreIndex = packetIndex
			if string(packet.Body) != string([]byte{1, 'A', 'U', 'R', 'A'}) {
				t.Fatalf("cold-login aura restore body=%x want 0141555241", packet.Body)
			}
		}
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == currentAdventureInfoPushMsgID {
			adventureInfoIndex = packetIndex
		}
		switch {
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) >= 5 &&
			(packet.Body[0] == 0 || packet.Body[0] == 1):
			if packet.Body[3] != 0 || packet.Body[4] != actorOwner {
				t.Fatalf(
					"heartbeat town/deferred mode%d owner=%x want 00/%02x",
					packet.Body[0],
					packet.Body[:5],
					actorOwner,
				)
			}
			if packet.Body[0] == 0 {
				mode0Count++
				emittedMode0 = append([]byte(nil), packet.Body...)
			} else {
				mode1Count++
				emittedMode1 = append([]byte(nil), packet.Body...)
			}
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina):
			assertCurrentPartylessTownOp9Body(t, packet.Body, partyOwner)
			op9Count++
		}
		packetIndex++
	}
	if auraRestoreCount != 1 || auraRestoreIndex <= lastContainerIndex ||
		adventureInfoIndex <= auraRestoreIndex {
		t.Fatalf(
			"cold-login aura restore count=%d index=%d containers_end=%d adventure=%d",
			auraRestoreCount,
			auraRestoreIndex,
			lastContainerIndex,
			adventureInfoIndex,
		)
	}
	if mode0Count != 1 || mode1Count != 1 || op9Count < 1 {
		t.Fatalf(
			"heartbeat owner-bearing packets mode0=%d mode1=%d op9=%d, want initial mode0, full mode1, and op9",
			mode0Count,
			mode1Count,
			op9Count,
		)
	}
	projection, projectionFound := currentPartyActorProjectionSnapshot(
		session,
		29,
		session.characterGeneration,
	)
	if !projectionFound || !bytes.Equal(projection.mode0, emittedMode0) ||
		!bytes.Equal(projection.mode1, emittedMode1) {
		session.partyActorProjectionMu.RLock()
		cached := session.partyActorProjection
		session.partyActorProjectionMu.RUnlock()
		t.Fatalf(
			"cold-login party projection found=%t mode0_equal=%t mode1_equal=%t session_generation=%d cached_character=%d cached_generation=%d cached_mode0=%d cached_mode1=%d",
			projectionFound,
			bytes.Equal(projection.mode0, emittedMode0),
			bytes.Equal(projection.mode1, emittedMode1),
			session.characterGeneration,
			cached.characterID,
			cached.generation,
			len(cached.mode0),
			len(cached.mode1),
		)
	}

	_, transition, rest := splitInitialTownPreparedStateBeforeTransition(t, fullStream)
	if transition.Header.Classification != 0 || transition.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("transition header=%+v", transition.Header)
	}
	rest = requireNoCurrentChannelNoticeAfterTownTransition(t, rest)
	rest = requireInitialTownPostTransitionQuestRefresh(t, rest)
	rest = requireInitialTownGuildStateProjection(t, rest, guild)
	rest = requireInitialTownCeraShopWalletProjection(t, rest)
	if len(rest) != 0 {
		t.Fatalf("heartbeat post-op24 quest refresh trailing=%x", rest)
	}
	if session.townPostTransition.stage != currentTownPostTransitionIdle {
		t.Fatalf("heartbeat actor tail post-transition stage=%d want idle before type1345", session.townPostTransition.stage)
	}
	if session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent ||
		session.sceneBootstrapTailSent || !session.sceneBootstrapTailDeferred ||
		!session.selectedUserInfoRefreshSent || session.selectedUserInfoMode3Sent {
		t.Fatalf("route/tail flags stage=%d tail_sent=%t tail_deferred=%t mode1=%t mode3_sent=%t",
			session.initialTownRouteStage,
			session.sceneBootstrapTailSent,
			session.sceneBootstrapTailDeferred,
			session.selectedUserInfoRefreshSent,
			session.selectedUserInfoMode3Sent)
	}

	connection.write.Reset()
	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatalf("type1345 HUD boundary: %v", err)
	}
	playerState, gotInitialHUDSkillBody, type1345Trailing := splitInitialTownType1345PlayerState(
		t,
		connection.write.Bytes(),
		actorOwner,
	)
	if len(playerState) != 0 || len(gotInitialHUDSkillBody) != 0 {
		t.Fatalf(
			"type1345 player state=%x skill=%x want no actor/skill replay",
			playerState,
			gotInitialHUDSkillBody,
		)
	}
	sceneTail := type1345Trailing
	if len(sceneTail) == 0 {
		t.Fatal("type1345 did not resume the existing deferred scene tail")
	}
	rentalStates := 0
	mode3States := 0
	for stream := sceneTail; len(stream) > 0; {
		packet, next := splitCurrentGameServerUpperPacketAuto(t, stream)
		stream = next
		if packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case currentRentalStateMsgID:
			rentalStates++
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			if len(packet.Body) > 0 && packet.Body[0] == 3 {
				mode3States++
				continue
			}
			fallthrough
		case currentCreatureStateTableMsgID,
			currentCreatureGrowthMsgID,
			uint16(dnfenum.CmdPacketFinishLoading),
			currentIncreaseStatusResultMsgID,
			uint16(dnfenum.CmdPacketRequestBlacklist):
			t.Fatalf(
				"type1345 deferred tail rebuilt player state msg_id=%d body=%x",
				packet.Header.MsgID,
				packet.Body,
			)
		}
	}
	if rentalStates != 1 {
		t.Fatalf("type1345 deferred tail rental=%d want=1", rentalStates)
	}
	if mode3States != 0 {
		t.Fatalf("type1345 selected actor mode3=%d want=0", mode3States)
	}
	if session.townPostTransition.stage != currentTownPostTransitionIdle {
		t.Fatalf("type1345 post-transition stage=%d want idle", session.townPostTransition.stage)
	}
	if !session.sceneBootstrapTailSent || session.sceneBootstrapTailDeferred {
		t.Fatalf("type1345 tail sent=%t deferred=%t", session.sceneBootstrapTailSent, session.sceneBootstrapTailDeferred)
	}
	firstHUDWriteLen := connection.write.Len()
	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatalf("duplicate type1345 HUD boundary: %v", err)
	}
	if connection.write.Len() != firstHUDWriteLen {
		t.Fatalf("duplicate type1345 grew HUD stream from %d to %d", firstHUDWriteLen, connection.write.Len())
	}
}

func requireInitialTownPostTransitionQuestRefresh(t *testing.T, stream []byte) []byte {
	t.Helper()
	for index, msgID := range []uint16{
		currentAcceptableQuestListMsgID,
		currentActiveQuestSnapshotMsgID,
		currentClearQuestListMsgID,
	} {
		if len(stream) == 0 {
			t.Fatalf("post-op24 quest refresh stopped before packet %d/3", index+1)
		}
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != msgID {
			t.Fatalf("post-op24 quest refresh packet[%d]=%d/%d want=0/%d", index, packet.Header.Classification, packet.Header.MsgID, msgID)
		}
		stream = rest
	}
	return stream
}

func requireInitialTownGuildStateProjection(
	t *testing.T,
	stream []byte,
	wantGuild *dnfguild.Guild,
) []byte {
	t.Helper()
	// A guildless character gets only the grade sentinel. op70's handler shows
	// the notice once per client run gated on nothing but the global flag at
	// 0x519297C (01D8B8A2 / 01D8B8BD) -- it checks neither whether the notice
	// is empty nor whether the viewer is in a guild -- so the empty projection
	// this used to send popped a blank 公会公告 line on every login.
	expected := []uint16{70, 127}
	if wantGuild == nil {
		expected = expected[1:]
	}
	for _, msgID := range expected {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != msgID {
			t.Fatalf("initial town guild management=%+v want=0/%d", packet.Header, msgID)
		}
		if msgID == 127 {
			wantGrade := byte(100)
			if wantGuild != nil {
				wantGrade = dnfguild.DefaultMasterGrade
			}
			if !bytes.Equal(packet.Body, []byte{wantGrade}) {
				t.Fatalf("initial town guild grade=%x want=%d", packet.Body, wantGrade)
			}
		}
		stream = rest
	}
	// writeCurrentGuildManagementState now ends with the guild insignia aura block
	// and the DLL-private guild experience ranking, which upstream also places
	// between op127 and the membership notification. The aura shares opcode
	// CmdPacketSetUDPIPPort with the actor packets, and it is emitted even for a
	// guildless character (with the native -1 insignia indices).
	auraPacket, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
	if auraPacket.Header.Classification != 0 ||
		auraPacket.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		!isCurrentGuildAuraBody(auraPacket.Body) {
		t.Fatalf("initial town guild aura=%+v body=%x", auraPacket.Header, auraPacket.Body)
	}
	stream = rest
	rankingPacket, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
	if rankingPacket.Header.Classification != 0 || rankingPacket.Header.MsgID != 413 || len(rankingPacket.Body) != 12 {
		t.Fatalf("initial town guild ranking=%+v body=%x", rankingPacket.Header, rankingPacket.Body)
	}
	stream = rest
	wantBody := buildCurrentGuildMembershipNotificationBody(wantGuild)
	if len(stream) == 0 {
		t.Fatal("initial town stream ended before current guild state projection")
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentGuildMembershipNotificationMsgID ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf(
			"initial town guild state=%d/%d body=%x want=%d/%d body=%x",
			packet.Header.Classification,
			packet.Header.MsgID,
			packet.Body,
			0,
			currentGuildMembershipNotificationMsgID,
			wantBody,
		)
	}
	for _, msgID := range []uint16{
		uint16(dnfenum.CmdPacketRequestAccountGuildList),
		uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars),
	} {
		if len(rest) == 0 {
			t.Fatalf("initial town stream ended before guild cache projection op%d", msgID)
		}
		packet, next := splitCurrentGameServerUpperPacketAuto(t, rest)
		if packet.Header.Classification != 1 || packet.Header.MsgID != msgID || len(packet.Body) == 0 || packet.Body[0] != 1 {
			t.Fatalf("initial town guild cache projection=%d/%d body=%x want=1/%d success", packet.Header.Classification, packet.Header.MsgID, packet.Body, msgID)
		}
		rest = next
	}
	return rest
}

func requireNoCurrentChannelNoticeAfterTownTransition(t *testing.T, stream []byte) []byte {
	t.Helper()
	for remaining := stream; len(remaining) > 0; {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, remaining)
		remaining = rest
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.UpperMsgGameEndpoint) {
			t.Fatalf(
				"town route emitted class0/op1 after endpoint initialization body=%x",
				packet.Body,
			)
		}
	}
	return stream
}

func assertNoCurrentChannelNoticeInTownRoute(t *testing.T, stream []byte) {
	t.Helper()
	op24Index := -1
	for index := 0; len(stream) > 0; index++ {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		stream = rest
		if packetLooksLikeInitialTownTransition(packet) {
			if op24Index >= 0 {
				t.Fatalf("ordinary town route emitted multiple typed op24 packets")
			}
			op24Index = index
		}
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.UpperMsgGameEndpoint) {
			t.Fatalf(
				"ordinary town route emitted class0/op1 at packet %d after endpoint initialization body=%x",
				index,
				packet.Body,
			)
		}
	}
	if op24Index < 0 {
		t.Fatal("ordinary town route emitted no typed op24")
	}
}

func countCurrentActorModePackets(t *testing.T, stream []byte, mode byte) int {
	t.Helper()
	count := 0
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		stream = rest
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 &&
			packet.Body[0] == mode {
			count++
		}
	}
	return count
}

func packetLooksLikeInitialTownTransition(packet dnfproto.ChannelPacket) bool {
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentSceneTransitionMsgID {
		return false
	}
	if len(packet.Body) < 4 || (len(packet.Body)-4)%8 != 0 {
		return false
	}
	rowCount := int(binary.LittleEndian.Uint16(packet.Body[2:4]))
	if len(packet.Body) != 4+rowCount*8 {
		return false
	}
	return packet.Body[0] != 0 || packet.Body[1] != 0
}

func splitInitialTownPreparedStateBeforeTransition(
	t *testing.T,
	stream []byte,
) ([]byte, dnfproto.ChannelPacket, []byte) {
	t.Helper()
	return splitInitialTownPreparedStateBeforeTransitionWithPolicies(t, stream, len(currentSelectInventoryBootstrapListTypes), true)
}

func splitInitialTownPreparedStateBeforeTransitionWithoutFullMode1(
	t *testing.T,
	stream []byte,
) ([]byte, dnfproto.ChannelPacket, []byte) {
	t.Helper()
	return splitInitialTownPreparedStateBeforeTransitionWithPolicies(t, stream, len(currentSelectInventoryBootstrapListTypes), false)
}

func splitInitialTownPreparedStateBeforeTransitionWithOp13Policy(
	t *testing.T,
	stream []byte,
	wantContainerLists int,
) ([]byte, dnfproto.ChannelPacket, []byte) {
	t.Helper()
	return splitInitialTownPreparedStateBeforeTransitionWithPolicies(t, stream, wantContainerLists, true)
}

func splitInitialTownPreparedStateBeforeTransitionWithPolicies(
	t *testing.T,
	stream []byte,
	wantContainerLists int,
	wantFullMode1 bool,
) ([]byte, dnfproto.ChannelPacket, []byte) {
	t.Helper()
	original := stream
	consumed := 0
	overseerPages := 0
	containerLists := 0
	lastContainerListIndex := -1
	containerListTypeCap := wantContainerLists
	if containerListTypeCap < 0 {
		containerListTypeCap = 1
	}
	containerListTypes := make([]byte, 0, containerListTypeCap)
	actionTable := false
	cargoReset := false
	adventureInfo := false
	adventureInfoIndex := -1
	crystalStateCount := 0
	crystalStateIndex := -1
	fullMode1 := false
	fullMode1StartsPreparedState := false
	mode3 := false
	skillInfoBeforeTransition := 0
	skillInfoPreloadIndex := -1
	var skillInfoPreloadBody []byte
	lastFullMode1Index := -1
	nativeExpertJobInfoCount := 0
	expertJobInfoIndex := -1
	creatureStateCount := 0
	creatureStateIndex := -1
	creatureItemRefreshCount := 0
	creatureItemRefreshIndex := -1
	guardianGemMedalRefreshCount := 0
	guardianGemMedalRefreshIndex := -1
	creatureGrowthCount := 0
	creatureGrowthIndex := -1
	skillInfoIndex := -1
	acceptableQuestCount := 0
	acceptableQuestIndex := -1
	activeQuestCount := 0
	activeQuestIndex := -1
	adventureActor := false
	insertOverseer := false
	passGate := false
	clientSpec := false
	actorDisplay := false
	actorPlacement := false
	actorSceneSnapshot := false
	userPosition := false
	userArea := false
	clearQuestIndex := -1
	userAreaIndex := -1
	var msgIDs []uint16

	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		packetBytes := len(stream) - len(rest)
		if packetLooksLikeInitialTownTransition(packet) {
			containerListPolicyOK := containerLists == wantContainerLists
			if wantContainerLists < 0 {
				containerListPolicyOK = containerLists == 0 ||
					(containerLists == 1 && len(containerListTypes) == 1 && containerListTypes[0] == currentPetInventoryListType)
			}
			// The weapon-validation op19 preload precedes the full mode1; the
			// authoritative op19 follows it. Both carry the identical projection.
			preTransitionOrderOK := creatureStateCount == 1 &&
				skillInfoBeforeTransition == 2 &&
				nativeExpertJobInfoCount == 1 &&
				acceptableQuestCount == 1 &&
				activeQuestCount == 1 &&
				creatureStateIndex >= 0 &&
				skillInfoPreloadIndex > adventureInfoIndex &&
				skillInfoIndex > skillInfoPreloadIndex &&
				acceptableQuestIndex > creatureStateIndex &&
				activeQuestIndex > acceptableQuestIndex &&
				expertJobInfoIndex > skillInfoIndex
			preTransitionOrderOK = preTransitionOrderOK &&
				clearQuestIndex >= 0 && userAreaIndex >= 0 &&
				clearQuestIndex > userAreaIndex
			if lastFullMode1Index >= 0 {
				preTransitionOrderOK = preTransitionOrderOK &&
					skillInfoPreloadIndex < lastFullMode1Index &&
					skillInfoIndex > lastFullMode1Index &&
					creatureStateIndex > skillInfoIndex
			}
			if creatureItemRefreshCount > 0 {
				preTransitionOrderOK = preTransitionOrderOK &&
					creatureItemRefreshCount == 1 &&
					creatureItemRefreshIndex > lastFullMode1Index &&
					creatureStateIndex > creatureItemRefreshIndex
			}
			if guardianGemMedalRefreshCount > 0 {
				preTransitionOrderOK = preTransitionOrderOK &&
					guardianGemMedalRefreshCount == 1 &&
					(lastFullMode1Index < 0 || guardianGemMedalRefreshIndex > lastFullMode1Index) &&
					creatureStateIndex > guardianGemMedalRefreshIndex
			}
			if creatureGrowthCount > 0 {
				preTransitionOrderOK = preTransitionOrderOK &&
					creatureGrowthCount == 1 &&
					creatureGrowthIndex > creatureStateIndex &&
					acceptableQuestIndex > creatureGrowthIndex
			}
			crystalOrderOK := crystalStateCount == 1 &&
				crystalStateIndex > lastContainerListIndex &&
				adventureInfoIndex > crystalStateIndex
			if overseerPages != 5 ||
				!containerListPolicyOK || !actionTable || !cargoReset || !adventureInfo || fullMode1StartsPreparedState != wantFullMode1 ||
				mode3 || !preTransitionOrderOK || !crystalOrderOK ||
				!adventureActor || !insertOverseer || !passGate ||
				!clientSpec || !actorDisplay || !actorPlacement || !actorSceneSnapshot || !userPosition || !userArea {
				t.Fatalf(
					"op24 town initialization mismatch: pages=%d late_lists=%d late_list_types=%v action=%t cargo=%t crystal=%d@%d crystal_order=%t adventure=%t@%d mode1=%t mode3_sent=%t native_op205=%d op105=%d@%d op102=%d@%d guardian_op14=%d@%d skill_preop24=%d@%d op21=%d@%d op574=%d@%d final_mode1=%d order_ok=%t actor=%t op359=%t op356=%t op124=%t op9=%t op120=%t noti320=%t noti16=%t noti17=%t msg_ids=%v",
					overseerPages,
					containerLists,
					containerListTypes,
					actionTable,
					cargoReset,
					crystalStateCount,
					crystalStateIndex,
					crystalOrderOK,
					adventureInfo,
					adventureInfoIndex,
					fullMode1,
					mode3,
					nativeExpertJobInfoCount,
					creatureStateCount,
					creatureStateIndex,
					creatureGrowthCount,
					creatureGrowthIndex,
					guardianGemMedalRefreshCount,
					guardianGemMedalRefreshIndex,
					skillInfoBeforeTransition,
					skillInfoIndex,
					acceptableQuestCount,
					acceptableQuestIndex,
					activeQuestCount,
					activeQuestIndex,
					lastFullMode1Index,
					preTransitionOrderOK,
					adventureActor,
					insertOverseer,
					passGate,
					clientSpec,
					actorDisplay,
					actorPlacement,
					actorSceneSnapshot,
					userPosition,
					userArea,
					msgIDs,
				)
			}
			return append([]byte(nil), original[:consumed]...), packet, rest
		}

		packetIndex := len(msgIDs)
		msgIDs = append(msgIDs, packet.Header.MsgID)
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketRequestOverseer):
			overseerPages++
		case uint16(dnfenum.CmdPacketPVPMissionHpPercent):
			actionTable = true
		case uint16(dnfenum.CmdPacketCancelCargoPad):
			cargoReset = true
		case uint16(dnfenum.CmdPacketLeaveParty):
			if !cargoReset {
				t.Fatalf("initial town op13 preceded cargo reset: body=%x", packet.Body)
			}
			if wantContainerLists < 0 || wantContainerLists == 1 {
				if len(packet.Body) == 0 || packet.Body[0] != currentPetInventoryListType {
					t.Fatalf("initial town late op13 body=%x: only list7 pet-scene rehydrate is allowed before op24", packet.Body)
				}
			} else if containerLists >= len(currentSelectInventoryBootstrapListTypes) ||
				len(packet.Body) == 0 ||
				packet.Body[0] != currentSelectInventoryBootstrapListTypes[containerLists] {
				t.Fatalf(
					"initial town actor-bound op13[%d] body=%x want list%d",
					containerLists,
					packet.Body,
					currentSelectInventoryBootstrapListTypes[containerLists],
				)
			}
			containerLists++
			lastContainerListIndex = packetIndex
			if len(packet.Body) > 0 {
				containerListTypes = append(containerListTypes, packet.Body[0])
			}
		case currentCrystalContractStateMsgID:
			if packet.Header.Classification != 0 || len(packet.Body) != 2 || packet.Body[0] != 0 {
				t.Fatalf("initial town crystal state header=%+v body=%x, want class0/op898 00+selection", packet.Header, packet.Body)
			}
			crystalStateCount++
			crystalStateIndex = packetIndex
		case currentAdventureInfoPushMsgID:
			adventureInfo = true
			adventureInfoIndex = packetIndex
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			if len(packet.Body) > 0 && packet.Body[0] == 1 {
				fullMode1 = true
				lastFullMode1Index = packetIndex
			}
			if len(packet.Body) > 0 && packet.Body[0] == 3 {
				mode3 = true
			}
		case currentExpertJobInfoNotification:
			nativeExpertJobInfoCount++
			expertJobInfoIndex = packetIndex
		case currentCreatureStateTableMsgID:
			creatureStateCount++
			creatureStateIndex = packetIndex
		case currentCreatureGrowthMsgID:
			creatureGrowthCount++
			creatureGrowthIndex = packetIndex
		case currentSkillInfoMsgID:
			skillInfoBeforeTransition++
			switch skillInfoBeforeTransition {
			case 1:
				skillInfoPreloadIndex = packetIndex
				skillInfoPreloadBody = append([]byte(nil), packet.Body...)
			case 2:
				skillInfoIndex = packetIndex
				if !bytes.Equal(packet.Body, skillInfoPreloadBody) {
					t.Fatalf("initial town skill preload=%x final=%x, want identical selected-character projections", skillInfoPreloadBody, packet.Body)
				}
			default:
				t.Fatalf("initial town emitted %d pre-transition skill projections", skillInfoBeforeTransition)
			}
			if lastFullMode1Index >= 0 && lastFullMode1Index < packetIndex {
				fullMode1StartsPreparedState = true
			}
		case currentAcceptableQuestListMsgID:
			acceptableQuestCount++
			acceptableQuestIndex = packetIndex
		case currentActiveQuestSnapshotMsgID:
			activeQuestCount++
			activeQuestIndex = packetIndex
		case uint16(dnfenum.CmdPacketWalkoutPartyMember):
			if packet.Header.Classification != 0 ||
				len(packet.Body) != 3+currentItemListEntryWireSize ||
				packet.Body[0] != currentSocketListEquipment ||
				binary.LittleEndian.Uint16(packet.Body[1:3]) != 1 {
				t.Fatalf("initial town emitted malformed acquisition-style op14 body=%x", packet.Body)
			}
			switch binary.LittleEndian.Uint16(packet.Body[3:5]) {
			case 26:
				creatureItemRefreshCount++
				creatureItemRefreshIndex = packetIndex
			case uint16(currentGuildMedalActorSlot):
				if !currentGuardianGemRawSocketOccupied(packet.Body[3:]) {
					t.Fatalf("initial town guardian-medal op14 omitted raw socket state body=%x", packet.Body)
				}
				guardianGemMedalRefreshCount++
				guardianGemMedalRefreshIndex = packetIndex
			default:
				t.Fatalf("initial town emitted unsupported acquisition-style op14 slot=%d body=%x", binary.LittleEndian.Uint16(packet.Body[3:5]), packet.Body)
			}
		case currentAdventureActorRefreshMsgID:
			adventureActor = true
		case uint16(dnfenum.CmdPacketInsertOverseer):
			if len(packet.Body) < 4+currentInsertOverseerTailWireSize ||
				(len(packet.Body)-4-currentInsertOverseerTailWireSize)%currentInsertOverseerRowWireSize != 0 {
				t.Fatalf("op359 malformed current body len=%d body=%x", len(packet.Body), packet.Body)
			}
			rowCount := int(binary.LittleEndian.Uint32(packet.Body[:4]))
			if len(packet.Body) != 4+rowCount*currentInsertOverseerRowWireSize+currentInsertOverseerTailWireSize {
				t.Fatalf("op359 row count=%d len=%d body=%x", rowCount, len(packet.Body), packet.Body)
			}
			insertOverseer = true
		case uint16(dnfenum.CmdPacketPassGateObject):
			passGate = true
			clearQuestIndex = packetIndex
		case uint16(dnfenum.CmdPacketReportClientSpec):
			clientSpec = true
		case uint16(dnfenum.CmdPacketRecoverStamina):
			actorDisplay = true
		case uint16(dnfenum.CmdPacketRequestBlacklist):
			actorPlacement = true
		case currentTownActorSceneSnapshotMsgID:
			if len(packet.Body) != 4 || packet.Body[0] != 1 ||
				binary.LittleEndian.Uint16(packet.Body[1:3]) == 0 || packet.Body[3] != 0 {
				t.Fatalf("noti0x320=%x: want one existing actor with zero children", packet.Body)
			}
			actorSceneSnapshot = true
		case currentTownUserPositionNotificationMsgID:
			userPosition = true
		case currentTownUserAreaNotificationMsgID:
			userArea = true
			userAreaIndex = packetIndex
		}
		consumed += packetBytes
		stream = rest
	}

	t.Fatal("initial town stream ended before typed op24 transition")
	return nil, dnfproto.ChannelPacket{}, nil
}

func TestInitialTownTransitionReappliesSkyTowerQuestGateAfterSceneCommit(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	service.questCatalog = buildQuestListTestCatalog(t)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			3161: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	session.selectedCharacterID = 29
	session.initialTownRouteCharacterID = 29
	session.initialTownRouteStage = currentInitialTownRoutePlayerStatePrepared

	row := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(29),
		Value1:              1500,
		Value2:              500,
		Value3:              5,
		Value4:              3,
	}
	transitionBody, err := buildCurrentSceneTransitionBody(40, 0, []currentSceneTransitionRow{row})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.sendCurrentTownTransitionLocked(session, 29, 40, 0, row, transitionBody); err != nil {
		t.Fatal(err)
	}
	if err := service.sendCurrentPostTownTransitionQuestSnapshotsLocked(session, "test_sky_tower_after_op24"); err != nil {
		t.Fatal(err)
	}

	stream := connection.write.Bytes()
	transition, stream := splitCurrentGameServerUpperPacketAuto(t, stream)
	if transition.Header.Classification != 0 || transition.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("transition=%d/%d want=0/%d", transition.Header.Classification, transition.Header.MsgID, currentSceneTransitionMsgID)
	}
	for index, msgID := range []uint16{currentAcceptableQuestListMsgID, currentActiveQuestSnapshotMsgID} {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != msgID {
			t.Fatalf("post-op24 packet[%d]=%d/%d want=0/%d", index, packet.Header.Classification, packet.Header.MsgID, msgID)
		}
		stream = rest
	}
	completed, stream := splitLongHengGameServerUpperPacket(t, stream)
	if completed.Header.Classification != 0 || completed.Header.MsgID != currentClearQuestListMsgID || len(stream) != 0 {
		t.Fatalf("completed snapshot=%d/%d trailing=%x", completed.Header.Classification, completed.Header.MsgID, stream)
	}
	plain, err := zlibDecompress(completed.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4+currentClearQuestListPayloadSize || plain[4+3161] != 1 {
		t.Fatalf("completed snapshot len=%d q3161=%d", len(plain), plain[4+3161])
	}
	persisted, found, err := repositories.Quest.Load(context.Background(), "29")
	if err != nil || !found || persisted.States[3161].Status != "completed" || len(persisted.States) != 1 {
		t.Fatalf("projection mutated q3161 record found=%t err=%v states=%+v", found, err, persisted.States)
	}
}

func splitInitialTownType1345PlayerState(
	t *testing.T,
	stream []byte,
	_ byte,
) ([]byte, []byte, []byte) {
	t.Helper()
	var skillBody []byte
	trailing := make([]byte, 0, len(stream))
	for remaining := stream; len(remaining) > 0; {
		packetWire := remaining
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, remaining)
		wireLen := len(remaining) - len(rest)
		remaining = rest
		if skillBody != nil {
			t.Fatalf(
				"type1345 sent packet after retained skill msg_id=%d class=%d body=%x",
				packet.Header.MsgID,
				packet.Header.Classification,
				packet.Body,
			)
		}
		if packet.Header.Classification != 0 {
			trailing = append(trailing, packetWire[:wireLen]...)
			continue
		}
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			if len(packet.Body) > 0 && packet.Body[0] == 3 {
				trailing = append(trailing, packetWire[:wireLen]...)
				continue
			}
			fallthrough
		case currentCreatureStateTableMsgID,
			currentCreatureGrowthMsgID,
			uint16(dnfenum.CmdPacketFinishLoading),
			currentIncreaseStatusResultMsgID,
			uint16(dnfenum.CmdPacketRequestBlacklist):
			t.Fatalf(
				"type1345 rebuilt actor/HUD state msg_id=%d body=%x",
				packet.Header.MsgID,
				packet.Body,
			)
		case currentSkillInfoMsgID:
			if skillBody != nil {
				t.Fatalf("type1345 sent duplicate skill projections first=%x second=%x", skillBody, packet.Body)
			}
			skillBody = append([]byte(nil), packet.Body...)
		default:
			trailing = append(trailing, packetWire[:wireLen]...)
		}
	}
	return nil, skillBody, trailing
}
