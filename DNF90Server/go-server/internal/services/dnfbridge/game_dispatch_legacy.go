package dnfbridge

import (
	"encoding/hex"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func (s *Service) handleLegacyGamePacket(session *gameSession, raw []byte) error {
	packet, err := dnfproto.ParseLegacyGamePacket(raw)
	if err != nil {
		return err
	}
	body := normalizeLegacyGameBody(packet.Header.Type, packet.Body)
	runtimeName := runtimeCmdPacketName(packet.Header.Cmd, packet.Header.Type)
	runtimeKnown := runtimeCmdPacketKnown(packet.Header.Cmd, packet.Header.Type)
	if packet.Header.Type == uint16(dnfenum.CmdPacketNewbieGuideOption) {
		// Diagnostic probe only: keep the packet on its existing unhandled path.
		// The four-byte body is needed to identify per-guide close/help actions
		// before adding account-scoped persistence.
		s.logInfo("dnfbridge newbie guide option probe",
			"conn_id", session.connID,
			"account_id", s.accountIDForSession(session),
			"character_id", session.selectedCharacterID,
			"sequence", packet.Header.Sequence,
			"cmd", packet.Header.Cmd,
			"type", packet.Header.Type,
			"body_len", len(packet.Body),
			"body_hex", hex.EncodeToString(packet.Body))
	}
	s.logGamePacket(session, "RECV", "game-legacy", raw,
		"cmd", packet.Header.Cmd,
		"type", packet.Header.Type,
		"runtime_cmd_name", runtimeName,
		"runtime_cmd_known", runtimeKnown,
		"header_seq", packet.Header.Sequence,
		"body_len", len(packet.Body))
	s.logPacketEvent("game-legacy-meta",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"cmd", packet.Header.Cmd,
		"type", packet.Header.Type,
		"runtime_cmd_name", runtimeName,
		"runtime_cmd_known", runtimeKnown,
		"sequence", packet.Header.Sequence,
		"body_len", len(packet.Body),
		"dispatch_body_len", len(body))
	if len(body) != len(packet.Body) {
		s.logPacketEvent("game-legacy-body-normalized",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"type", packet.Header.Type,
			"runtime_cmd_name", runtimeName,
			"runtime_cmd_known", runtimeKnown,
			"raw_body_len", len(packet.Body),
			"dispatch_body_len", len(body))
	}
	return s.handleGameCommand(session, packet.Header.Cmd, packet.Header.Type, body)
}

func normalizeLegacyGameBody(typ uint16, body []byte) []byte {
	if normalized, ok := currentGameplayModules.NormalizeLegacy(typ, body); ok {
		return normalized
	}
	if dnfenum.GameType(typ) == dnfenum.GameTypeSelectCharacter {
		return stripLegacyCodecPrefix(body, 11)

	}
	return body
}

func stripLegacyBuySkillTransportTrailer(body []byte) []byte {
	const (
		headerLen  = 2
		entryLen   = 4
		tailLen    = 1
		trailerLen = 4
	)
	if len(body) < headerLen+tailLen+trailerLen {
		return body
	}
	semanticLen := headerLen + int(body[1])*entryLen + tailLen
	if len(body) != semanticLen+trailerLen {
		return body
	}
	return append([]byte(nil), body[:semanticLen]...)
}

func stripLegacyEmblemCompoundTransportTrailer(body []byte) []byte {
	if len(body) < 1+currentEmblemCompoundMinInputs*currentEmblemCompoundInputSize+4 {
		return body
	}
	count := int(body[0])
	semanticLength := 1 + count*currentEmblemCompoundInputSize
	if count < currentEmblemCompoundMinInputs || count > currentEmblemCompoundMaxInputs || len(body) != semanticLength+4 {
		return body
	}
	return append([]byte(nil), body[:semanticLength]...)
}

func stripLegacyCodecPrefix(body []byte, payloadLen int) []byte {
	if payloadLen <= 0 || len(body) != payloadLen+5 {
		return body
	}
	return append([]byte(nil), body[5:]...)
}

func stripLegacyTransportTrailer(body []byte, payloadLen int) []byte {
	const trailerLen = 4
	if payloadLen <= 0 || len(body) != payloadLen+trailerLen {
		return body
	}
	return append([]byte(nil), body[:payloadLen]...)
}

func normalizeLegacySetQuestTriggerBody(body []byte) []byte {
	const (
		opcodeEchoLow  = byte(dnfenum.CmdPacketSetQuestTrigger)
		opcodeEchoHigh = byte(uint16(dnfenum.CmdPacketSetQuestTrigger) >> 8)
	)
	// A masked seven-byte op33 is native current-client data, not a zero
	// trailer. Retain its 0x10/0x20/0x40 lane selector so the upper and legacy
	// routes apply the same packed Hell Party objective transition. Only the
	// old scalar six-byte clone with an appended zero can be safely shortened.
	if len(body) == currentSetQuestTriggerMaskedRequestBodySize &&
		body[0] == opcodeEchoLow &&
		body[1] == opcodeEchoHigh &&
		body[4] == 0 &&
		(body[5] == 0 || body[5] == 1) &&
		body[len(body)-1] == 0 {
		return append([]byte(nil), body[:currentSetQuestTriggerRequestBodySize]...)
	}
	return stripLegacyTransportTrailer(body, currentSetQuestTriggerRequestBodySize)
}

func stripLegacyDieCharacterZeroTail(body []byte) []byte {
	const legacyTailLen = 2
	if len(body) != currentDungeonDieCharacterBodySize+legacyTailLen ||
		body[currentDungeonDieCharacterBodySize] != 0 ||
		body[currentDungeonDieCharacterBodySize+1] != 0 {
		return body
	}
	return append([]byte(nil), body[:currentDungeonDieCharacterBodySize]...)
}

func runtimeCmdPacketName(cmd byte, typ uint16) string {
	if dnfenum.GameCmd(cmd) != dnfenum.GameCmdCommand {
		return ""
	}
	return dnfenum.CmdPacketName(typ)
}

func runtimeCmdPacketKnown(cmd byte, typ uint16) bool {
	if dnfenum.GameCmd(cmd) != dnfenum.GameCmdCommand {
		return false
	}
	return dnfenum.IsKnownCmdPacket(typ)
}

// slowLegacyRequestFields 构造 game-slow-legacy-request 的字段集。
//
// 与 slowUpperRequestFields 同理，独立成函数以便测试和线上共用同一份字段定义：
// 事件里刻意避开 "msg_id"/"type" 字段名，否则会被 suppressPacketLogEvent 的
// msg_id(1276/1516/1518) 与 type(35) 规则整条丢弃，诊断静默失效。
func slowLegacyRequestFields(
	session *gameSession,
	cmd byte,
	typ uint16,
	bodyLen int,
	elapsed time.Duration,
) []any {
	return []any{
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"request_type", typ,
		"cmd", cmd,
		"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
		"runtime_cmd_known", runtimeCmdPacketKnown(cmd, typ),
		"body_len", bodyLen,
		"elapsed_ms", elapsed.Milliseconds(),
	}
}

func (s *Service) handleGameCommand(session *gameSession, cmd byte, typ uint16, body []byte) error {
	// 慢请求自诊断：放在 handleGameCommand 入口而不是它的调用方，因为现网进游戏
	// 的主力流量是 kind=game-legacy，走 handleLegacyGamePacket -> handleGameCommand，
	// 而 handleGameFrame 只覆盖 kind=game(transport)。放在这里一处即覆盖全部调用方。
	// handleGameCommand 内有大量提前 return，故用 defer 统一收口；纯诊断，不改变
	// 分发、返回值或任何发包内容。
	slowRequestStarted := time.Now()
	defer func() {
		if elapsed := time.Since(slowRequestStarted); elapsed >= slowGameRequestThreshold {
			s.logPacketEvent("game-slow-legacy-request",
				slowLegacyRequestFields(session, cmd, typ, len(body), elapsed)...)
		}
	}()
	if dnfenum.GameCmd(cmd) != dnfenum.GameCmdCommand {
		return nil
	}
	if typ == currentRobotSessionBindOpcode {
		return s.handleCurrentRobotSessionBind(session, body)
	}
	if handled, err := currentGameplayModules.DispatchLegacy(s, session, typ, body); handled || err != nil {
		return err
	}
	switch dnfenum.GameType(typ) {
	// class1/op689 在 legacy 形状下也要走同一条多路复用器：活动（升级预约 222、
	// 周签到 2208）与建角后置状态共用这个 opcode，只按长度分不出来。
	case dnfenum.GameType(dnfenum.UpperMsgCreatePostState):
		return s.handleUpperCreatePostState(session, body)
	// 活动的 legacy 形状（class 1）领取命令。同上：handler 已存在但没注册。
	case dnfenum.GameType(812):
		return s.handleCurrentReturneeNPCGift(session, body)
	case dnfenum.GameType(currentMonthlyAttendanceClaimID):
		return s.handleCurrentMonthlyAttendanceClaim(session, body)
	case dnfenum.GameType(currentWeeklyAttendanceClaimID):
		return s.handleCurrentWeeklyAttendanceClaim(session, body)
	case dnfenum.GameTypeLogin:
		// Current EXE evidence proves only the upper class1/op1 590/598-byte
		// request emitted after CHANNELINFO. Keep legacy-shaped op1 silent.
		s.logGameEvent(session, "game-legacy-endpoint-op1-ignored",
			"body_len", len(body),
			"reason", "legacy_endpoint_request_shape_not_proved")
		return nil
	case dnfenum.GameType(dnfenum.CmdPacketExit):
		// Current op3/one-byte channel-exit is structurally ambiguous with the
		// legacy frame and reaches this decoder in live traffic. It still owns
		// the same class1/op3 ACK as the upper route.
		return s.handleCurrentChannelExit(session, body, "legacy_plain_op3")
	case dnfenum.GameTypeGetUserInfo:
		// The name-change-record window reuses this opcode with mode=5. It must
		// be answered with its own notification: the roster bootstrap below is
		// only safe on the character-select screen and crashes the client when
		// it is replayed into a live scene.
		if handled, err := s.handleCurrentCharacterNameChangeLogRequest(session, body); handled || err != nil {
			return err
		}
		if handled, err := s.handleCurrentPeerUserInfoRequest(session, body); handled || err != nil {
			return err
		}
		// The hidden-probe-deferred and old WSTR bootstrap experiments both
		// leave the current source-built upper-body-bypass client on a black
		// selector.  The user-verified visible role-select chain uses the
		// fixed15 character-list route for legacy GET_USERINFO; upper op8 keeps
		// the ordinary upper envelope.
		return s.sendLegacyGetUserInfoBootstrap(session)
	case dnfenum.GameTypeSelectCharacter:
		s.logInfo("dnfbridge latest select character received",
			"channel_id", session.channel.ID,
			"body_len", len(body))
		return s.sendSelectCharacterState(session, body)
	case dnfenum.GameTypeEnterSelectDungeon:
		s.logInfo("dnfbridge latest enter-select-dungeon received",
			"channel_id", session.channel.ID,
			"body_len", len(body))
		return s.sendEnterSelectDungeonState(session, "game_cmd_15", true, true)
	case dnfenum.GameType(dnfenum.CmdPacketDecreaseDurability):
		return s.handleDungeonDecreaseDurability(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketPVPChannelInfo):
		return s.handlePVPChannelInfo(session, body, "legacy")
	case dnfenum.GameType(dnfenum.CmdPacketMakePVPRoom):
		return s.handleMakePVPRoom(session, body, "legacy")
	case dnfenum.GameType(dnfenum.CmdPacketFairPVPScore):
		return s.handleCurrentPVPRecord(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketEnterPVPRoom):
		return s.handleEnterPVPRoom(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPSeatState):
		return s.handleSetPVPSeatState(session, body, "legacy")
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPReadyState):
		return s.handleSetPVPReadyState(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketRequestIntegratedMatching), dnfenum.GameType(dnfenum.CmdPacketCancelIntegratedMatching), dnfenum.GameType(dnfenum.CmdPacketIntegrateMatchingDirectComplete):
		return s.handleCurrentPVPMatching(session, typ, body)
	case dnfenum.GameType(dnfenum.CmdPacketDiePVPCharacter):
		return s.handleCurrentPVPDeath(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketPVPMissionHpPercent), dnfenum.GameType(dnfenum.CmdPacketPVPMissionWinPose), dnfenum.GameType(dnfenum.CmdPacketPVPMissionFirstAerialAttack), dnfenum.GameType(dnfenum.CmdPacketPVPMissionComboDamageRate):
		return s.handleCurrentPVPMissionReport(session, typ, body)
	case dnfenum.GameType(dnfenum.CmdPacketPVPTimeOut):
		return s.handleCurrentPVPTimeout(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketUsagePVPFatigue):
		return s.handleCurrentPVPFatigueUsage(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketArcademodePVPRoundInfo):
		return s.handleCurrentPVPArcadeRoundInfo(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketPVPFatigueReward):
		return s.handleCurrentPVPArcadeResult(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketEndPVPResult):
		return s.handleCurrentPVPResultEnd(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketCompleteLoadPVP), dnfenum.GameType(dnfenum.CmdPacketConnectP2pPVP):
		return s.handleCurrentPVPMatchSignal(session, typ, body)
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPTeamMode):
		return s.handleSetPVPTeamMode(session, body, "legacy")
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPMapIndex):
		return s.handleSetPVPMapIndex(session, body, "legacy")

	case dnfenum.GameTypeFinishLoading:
		return s.sendFinishLoadingStatus(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketAgitWarInfo):
		// Live current-client traffic sends legacy type1345 with u32(2)
		// immediately after the first typed op24. It completes the deferred
		// scene tail and HUD gauge only. Replaying actor/finish-loading state at
		// this callback leaves the current EXE update loop with a null actor.
		//
		// The scene-ready check itself now lives INSIDE handleCurrentGuildWarInfo,
		// which retains the already-owned scene tail first and only then reads the
		// same packet as a 公会战 status query. That is upstream's order, and it is
		// load-bearing in both directions: keeping the check here would swallow the
		// war query, while dropping it would stop the type1345 ACK from completing
		// the deferred tail at all and strand the player entering town.
		if isCurrentLegacyTownSceneReadyAck(body) {
			s.logGameEvent(session, "game-legacy-town-scene-ready-ack-accepted",
				"type", typ,
				"body_len", len(body),
				"boundary", "after_typed_op24_deferred_tail_and_hud_gauge_only")
		}
		return s.handleCurrentGuildWarInfo(session, body)
	case dnfenum.GameTypeContentsPlayInfo:
		return s.sendContentsPlayInfoState(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketSendMessage):
		return s.handleCurrentTownChat(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketItemHyperlinkMessage):
		return s.handleCurrentTownChatHyperlink(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketSequentialDungeonInfo):
		return s.handleCurrentSequentialDungeonInfo(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketRegisiterToBlacklist):
		// legacy cmd=1 type=118, body = u32 nameLength + name (captured 2026-09-22).
		return s.handleCurrentBlacklistRegister(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketDeleteToBlacklist):
		// legacy cmd=1 type=119, same body shape as 118.
		return s.handleCurrentBlacklistDelete(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketRequestBlacklist):
		return s.sendRequestBlacklistState(session, "legacy_120_request", body)
	case dnfenum.GameType(dnfenum.CmdPacketRequestUserChannel):
		// 频道跟随: legacy cmd=1 type=804, body = u8 region + dstr 目标名。
		// 响应必须是 class-1 同号 804(客户端 class-1 处理器 sub_3250DC0),
		// 不能用 class-0 的同号包(class-0 的 804 是另一个功能)。
		return s.handleCurrentUserChannelRequest(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketGuildAllMemberList):
		return s.sendGuildAllMemberListState(session, "legacy_140_request", body)

	case dnfenum.GameType(dnfenum.CmdPacketFpsDevideCollect):
		return s.handleGameFpsDevideCollect(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketInformNotice2nd), dnfenum.GameType(dnfenum.CmdPacketOverflowInfo):
		s.logPacketEvent("game-legacy-passive-report-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"body_len", len(body),
			"reason", "client_runtime_report_no_reference_s2c_ack")
		return nil
	case dnfenum.GameTypeCreateCharacter:
		return s.handleGameCreateCharacter(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketCheckPVPPrivateCharacter):
		return s.handleCheckPVPPrivateCharacter(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPTotalMatchCharacOrder):
		return s.handleCurrentPVPTotalOrder(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketSetPVPTotalMatchTeam), dnfenum.GameType(dnfenum.CmdPacketCheckPVPTotalMatchTeamName), dnfenum.GameType(dnfenum.CmdPacketSetPVPTotalMatchTeamName):
		return s.handlePVPTotalTeam(session, uint16(typ), body)
	case dnfenum.GameTypeStaticsRuntimeTing:
		s.logInfo("dnfbridge latest statics runtime report received; no response",
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"body_len", len(body))
		return nil
	case dnfenum.GameTypeCheckName:
		return s.handleGameCheckName(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketDeleteCharacter):
		return s.handleDeleteCharacter(session, body)
	case dnfenum.GameType(dnfenum.CmdPacketChangeCharacSlot):
		return s.handleChangeCharacterSlot(session, body, false)
	case dnfenum.GameType(currentCmdRepresentNameDuplicateCheck):
		return s.handleRepresentAccountNameDuplicateCheck(session, body)
	case dnfenum.GameType(currentCmdUpdateRepresentAccountName), dnfenum.GameType(currentCmdChangeRepresentAccountName):
		return s.handleUpdateRepresentAccountName(session, body, typ)
	case dnfenum.GameType(dnfenum.CmdPacketStoryDigestUpdate):
		// The current client reaches bodyless op1445 through the legacy game
		// decoder even when the semantic command is a class-1 upper command.
		// Keep one owner for both transports so the accepted story digest is
		// advanced monotonically instead of replaying on every login.
		return s.handleCurrentStoryDigestAccepted(session, dnfproto.DefaultChannelClassification, body)
	case dnfenum.GameType(dnfenum.CmdPacketNewbieGuideOption):
		return s.handleCurrentNewbieGuideOption(session, body)

	default:
		if handled, err := s.handleAlignedGameCommand(session, cmd, typ, body); handled || err != nil {
			return err
		}
		if rejected, err := s.rejectUnsupportedInteractiveCommand(
			session,
			typ,
			len(body),
			"legacy",
		); rejected || err != nil {
			return err
		}
		s.logInfo("dnfbridge unhandled latest game command",
			"cmd", cmd,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"runtime_cmd_known", runtimeCmdPacketKnown(cmd, typ),
			"body_len", len(body))
	}
	return nil
}

func isCurrentLegacyTownSceneReadyAck(body []byte) bool {
	return len(body) == 4 &&
		body[0] == 2 &&
		body[1] == 0 &&
		body[2] == 0 &&
		body[3] == 0
}
