// 本文件负责原生婚姻协议、在线邀请和双方状态投影；持久化交给 wedding owner。
package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

// sub_182A570 starts the native invitation window's 6000 ms timer.
const weddingInvitationTimeout = 6 * time.Second

// currentWeddingPriestNPCID 是主持订婚/婚礼的神父 NPC（PVF npc/npc.lst 的
// chn_father.npc，摆在城镇 39 区域 0 的 (2365,156)）。求婚要求双方都站在它旁边，
// 容差由 currentTownQuestMeetingRangeX/Y（±120/±80）决定。
const currentWeddingPriestNPCID int64 = 20013

// Full IDs from sub_18385E0's push imm32 instructions. The decompiler's
// incorrect char prototype for sub_225C940 truncates these to their low byte.
const (
	currentWeddingInvitationMsgID  uint16 = 997
	currentWeddingResultMsgID      uint16 = 998
	currentWeddingEndMsgID         uint16 = 999
	currentWeddingStateMsgID       uint16 = 1000
	currentWeddingRingMsgID        uint16 = 1001
	currentWeddingBreakupMsgID     uint16 = 1002
	currentWeddingLoveMsgID        uint16 = 1003
	currentWeddingReservationMsgID uint16 = 1004
	currentWeddingStartMsgID       uint16 = 1007
	currentWeddingActorMsgID       uint16 = 1013
)

type currentWeddingInvitation struct {
	from, to               boundGameSessionCharacter
	operation, kind, delay byte
	expires                time.Time
	timer                  *time.Timer
}

func (s *Service) currentWeddingCatalog() (*wedding.Catalog, error) {
	s.weddingCatalogMu.Lock()
	defer s.weddingCatalogMu.Unlock()
	if s.weddingCatalog != nil || s.weddingCatalogLoadErr != nil {
		return s.weddingCatalog, s.weddingCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.weddingCatalog, err = wedding.LoadCatalog(archive)
	}
	s.weddingCatalogLoadErr = err
	return s.weddingCatalog, err
}

func weddingGameplayModule() gameplayModuleDefinition {
	m := gameplayModuleDefinition{Name: "wedding", LegacyHandlers: map[uint16]gameplayHandler{}, UpperHandlers: map[uint16]gameplayHandler{}, LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{}}
	for _, route := range []struct {
		id     uint16
		size   int
		handle func(*Service, *gameSession, []byte) error
	}{
		{1032, 20, (*Service).handleWeddingRequest},
		{1033, 21, (*Service).handleWeddingResponse},
		{1035, 17, (*Service).handleWeddingPartner},
		{1036, 18, (*Service).handleWeddingRing},
		{1037, 17, (*Service).handleWeddingLove},
		{1039, 22, (*Service).handleWeddingEnter},
	} {
		m.LegacyHandlers[route.id] = func(s *Service, session *gameSession, r gameplayRequest) error {
			return route.handle(s, session, r.Body)
		}
		m.UpperHandlers[route.id] = defaultClassGameplayHandler("game-wedding-class-rejected", "current_exe_wedding_requires_default_classification", route.handle)
		m.LegacyNormalizers[route.id] = func(body []byte) []byte { return stripLegacyTransportTrailer(body, route.size) }
	}
	for opcode, size := range map[uint16]int{1045: 14, 1046: 27, 1047: 23, 1048: 19, 1049: 19, 1050: 0, 1051: 0} {
		handle := func(s *Service, session *gameSession, body []byte) error {
			return s.weddingRoomCommand(session, opcode, body)
		}
		m.LegacyHandlers[opcode] = func(s *Service, session *gameSession, r gameplayRequest) error { return handle(s, session, r.Body) }
		m.UpperHandlers[opcode] = defaultClassGameplayHandler("game-wedding-room-class-rejected", "current_exe_wedding_requires_default_classification", handle)
		m.LegacyNormalizers[opcode] = func(body []byte) []byte { return stripLegacyTransportTrailer(body, size) }
	}
	for _, opcode := range []uint16{1052, 1053, 1054} {
		handle := func(s *Service, session *gameSession, body []byte) error {
			return s.weddingCapsuleCommand(session, opcode, body)
		}
		m.LegacyHandlers[opcode] = func(s *Service, session *gameSession, r gameplayRequest) error { return handle(s, session, r.Body) }
		m.UpperHandlers[opcode] = defaultClassGameplayHandler("game-wedding-capsule-class-rejected", "current_exe_wedding_requires_default_classification", handle)
		m.LegacyNormalizers[opcode] = func(body []byte) []byte {
			if len(body) == 4 {
				return nil
			} // legacy transport trailer; native payload is empty
			return body
		}
	}
	return m
}

// sub_18347E0/sub_1831280 consume i32 result, u32 peer, u8 operation/kind/delay.
// sub_1831980/sub_1831310 consume the same fields plus u8 decision.
func weddingResultBody(code int32, target uint16, operation, kind, delay byte, decision ...byte) []byte {
	var w packetWriter
	w.writeInt32(int(code))
	w.writeUint32(uint32(target))
	w.writeByte(operation)
	w.writeByte(kind)
	w.writeByte(delay)
	if len(decision) != 0 {
		w.writeByte(decision[0])
	}
	return w.bytes()
}

func (s *Service) weddingReply(session *gameSession, opcode uint16, target uint16, operation, kind, delay byte, err error, decision ...byte) error {
	var code wedding.Error
	if err != nil && !errors.As(err, &code) {
		code = wedding.Invalid
	}
	if err != nil {
		s.logGameEvent(session, "game-wedding-rejected", "opcode", opcode, "target_id", target, "operation", operation, "error", err)
	}
	// The command dispatcher success marker enables the handler; business errors are signed i32.
	return s.sendGameUpperSuccess(session, opcode, weddingResultBody(int32(code), target, operation, kind, delay, decision...))
}

func (s *Service) weddingCharacter(ctx context.Context, identity boundGameSessionCharacter) (dnfrepo.CharacterRecord, error) {
	if !s.boundGameSessionCharacterCurrent(identity) {
		return dnfrepo.CharacterRecord{}, wedding.Missing
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.ErrRepoMissing
	}
	c, found, err := repos.Character.Load(ctx, strconv.Itoa(int(identity.character)))
	if err != nil {
		return c, err
	}
	if !found || c.AccountID != s.accountIDForSession(identity.session) || c.Stats["delete_flag"] != 0 {
		return c, wedding.Missing
	}
	return c, nil
}

// weddingAtPriest 只用于诊断日志：报告该角色离神父 NPC 的距离判定结果。
// currentWeddingRequiresPriest=false（当前运营口径）时它**不参与任何放行判定**，
// 仅供 game-wedding-propose-peers-missing 打印"离神父还有多远"。
func (s *Service) weddingAtPriest(identity boundGameSessionCharacter) bool {
	if !s.boundGameSessionCharacterCurrent(identity) {
		return false
	}
	session := identity.session
	// Town publication promotes the session while holding townMu. A transition
	// in progress is not an invitation boundary; do not invert that lock order.
	if !session.townMu.TryLock() {
		return false
	}
	pos := session.townPositionSnapshot
	ready := session.townSceneReadyCharacterID == identity.character
	session.townMu.Unlock()
	return ready && pos.CharacterID == identity.character && slices.Contains(s.currentTownNearbyNPCIDs(pos), currentWeddingPriestNPCID)
}

// currentWeddingRequiresPriest 控制"求婚/应答是否必须双方都贴着神父"。
//
// 上游（以及本移植的第一版）要求双方都站在神父 NPC 20013 旁，容差沿用任务会面的
// ±120/±80。实测这个口径太苛刻：神父站在主街 y≈156，而角色自然站位在 y≈240
// （客户端视觉上已经是"站在神父面前"），实测**差 5 像素**就被拒，而客户端只会弹
// "未能搜索到该玩家"，很容易被当成功能坏了。
//
// 运营口径（2026-10-04）放宽为：**只要同频道 + 同区域 + 双方场景就绪**。
// 想恢复上游的"必须在神父面前"，把这里改成 true 即可 —— weddingAtPriest 仍然
// 保留着（并被诊断日志使用），不需要改任何其它代码。
const currentWeddingRequiresPriest = false

// weddingActorInTownScene 判断该角色是否已经在一个城镇场景里就绪。
//
// 它取代上游"必须在神父旁"的那道单人门（用于离婚/解除）：只要求角色已进城、
// 场景就绪、坐标快照属于本人，不要求贴着任何 NPC。
func (s *Service) weddingActorInTownScene(identity boundGameSessionCharacter) bool {
	if s == nil || identity.session == nil || !s.boundGameSessionCharacterCurrent(identity) {
		return false
	}
	session := identity.session
	// 与 weddingAtPriest 同款：拿不到 townMu 就当作"未就绪"，绝不阻塞探测。
	if !session.townMu.TryLock() {
		return false
	}
	ready := session.townSceneReadyCharacterID == identity.character
	pos := session.townPositionSnapshot
	session.townMu.Unlock()
	return ready && pos.CharacterID == identity.character
}

// weddingPeersPresent 是"双人婚姻手势"的准入判据（求婚 / 应答）。
//
// 默认口径（currentWeddingRequiresPriest=false）：同频道 + 同区域 + 双方场景就绪。
// 注意它仍然要求双方注册在**同一个场景实例**里（PeerInSameArea），所以婚房/教会的
// 分实例场景不会串到别人身上。
func (s *Service) weddingPeersPresent(a, b boundGameSessionCharacter) bool {
	if a.character == b.character || a.session == nil || b.session == nil {
		return false
	}
	if a.session.channel.ID != b.session.channel.ID || s.onlinePlayers == nil {
		return false
	}
	if s.onlinePlayers.SessionForCharacter(a.character) != a.session ||
		s.onlinePlayers.SessionForCharacter(b.character) != b.session ||
		!s.onlinePlayers.PeerInSameArea(a.character, b.character) {
		return false
	}
	if currentWeddingRequiresPriest {
		return s.weddingAtPriest(a) && s.weddingAtPriest(b)
	}
	return s.weddingActorInTownScene(a) && s.weddingActorInTownScene(b)
}

func (s *Service) handleWeddingRequest(session *gameSession, body []byte) error {
	if len(body) != 20 {
		return nil
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	target, op, kind, delay := binary.LittleEndian.Uint32(body[13:17]), body[17], body[18], body[19]
	if target > 65535 || (op != 2 && (kind != 0 || delay != 0)) {
		return s.weddingReply(session, 1032, 0, op, kind, delay, wedding.Invalid)
	}
	if op == 1 || op == 2 || op == 7 {
		return s.weddingPropose(identity, uint16(target), op, kind, delay)
	}
	if op != 5 && op != 6 {
		return s.weddingReply(session, 1032, uint16(target), op, kind, delay, wedding.Invalid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	// ponytail: serialize wedding gestures across pairs; use per-pair locks if transaction latency becomes measurable.
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	// 离婚/解除（op 5/6/7）原本要求"本人在神父旁"。按同一口径放宽为"本人已在
	// 城镇场景里就绪"，不再要求贴着神父 —— 与求婚那条保持一致。
	if err == nil && !s.weddingActorInTownScene(identity) {
		err = wedding.Missing
	}
	state, stateErr := wedding.Read(c)
	if err == nil {
		err = stateErr
	}
	if target == 0 {
		target = uint32(state.PartnerID)
	}
	if err == nil && uint16(target) != state.PartnerID {
		err = wedding.Invalid
	}
	cat, catErr := s.currentWeddingCatalog()
	if err == nil {
		err = catErr
	}
	var result wedding.Result
	if err == nil {
		repos, _ := s.repositoryGroup()
		result, err = wedding.Commit(ctx, repos, cat, wedding.Command{ActorID: c.CharacterID, PartnerID: strconv.Itoa(int(target)), AccountID: s.accountIDForSession(session), Operation: op, Now: s.gameplayNow()})
	}
	replyErr := s.weddingReply(session, 1032, uint16(target), op, kind, delay, err)
	if err != nil {
		return replyErr
	}
	s.cancelWeddingForCharacterLocked(identity.character)
	s.cancelWeddingForCharacterLocked(uint16(target))
	return errors.Join(replyErr, s.publishWeddingCommit(result, op))
}

func (s *Service) weddingPropose(from boundGameSessionCharacter, target uint16, op, kind, delay byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	to, online := s.onlineGameSessionCharacterSnapshot(target)
	var err error
	if !online || !s.weddingPeersPresent(from, to) {
		// 只看日志、不改判定：把 weddingPeersPresent 的每个子条件单独算出来。
		// 没有它，"求婚被拒 -101" 只会告诉你"不在场"，但不会说清是不同频道、
		// 不在神父旁、不在同一场景实例，还是会话未就绪 —— 定位全靠翻 presence 日志。
		s.logGameEvent(from.session, "game-wedding-propose-peers-missing",
			s.weddingProposeDiagnostics(from, to, online)...)
		err = wedding.Missing
	}
	if err == nil && (s.weddingInvitations[from.character] != nil || s.weddingInvitations[target] != nil) {
		err = wedding.Busy
	}
	a, aErr := s.weddingCharacter(ctx, from)
	b, bErr := s.weddingCharacter(ctx, to)
	cat, catErr := s.currentWeddingCatalog()
	if err == nil {
		err = errors.Join(aErr, bErr, catErr)
	}
	if err == nil {
		err = wedding.Validate(a, b, cat, op, s.gameplayNow())
	}
	if err == nil && (len(rosterNameBytes(a.Name)) == 0 || len(rosterNameBytes(a.Name)) > 29 || len(rosterNameBytes(b.Name)) == 0 || len(rosterNameBytes(b.Name)) > 29) {
		err = wedding.Invalid
	}
	if err == nil && op == 2 && (kind > 2 || delay > 2) {
		err = wedding.Invalid
	}
	if err != nil {
		return s.weddingReply(from.session, 1032, target, op, kind, delay, err)
	}
	inv := &currentWeddingInvitation{from: from, to: to, operation: op, kind: kind, delay: delay, expires: s.gameplayNow().Add(weddingInvitationTimeout)}
	if s.weddingInvitations == nil {
		s.weddingInvitations = make(map[uint16]*currentWeddingInvitation)
	}
	s.weddingInvitations[from.character], s.weddingInvitations[target] = inv, inv
	if err = s.weddingReply(from.session, 1032, target, op, kind, delay, nil); err == nil {
		err = s.sendGameUpperRawClass(to.session, currentWeddingInvitationMsgID, weddingResultBody(0, from.character, op, kind, delay), 0)
	}
	if err != nil {
		s.removeWeddingInvitationLocked(inv)
		return errors.Join(err, s.notifyWeddingResult(inv, wedding.Missing, 2))
	}
	inv.timer = time.AfterFunc(weddingInvitationTimeout, func() { s.expireWeddingInvitation(inv) })
	s.logGameEvent(from.session, "game-wedding-invitation-sent", "from_char_id", from.character, "target_id", target, "target_conn_id", to.session.connID, "operation", op, "msg_id", currentWeddingInvitationMsgID)
	return nil
}

// weddingProposeDiagnostics 只用于日志：把 weddingPeersPresent 的每个子条件单独算出来，
// 便于定位"求婚被拒 -101"到底卡在哪一条。它**不参与判定** —— 权威判据仍然只有
// weddingPeersPresent 一个，这里多算几遍只是为了写清楚原因。
//
// 关注的五条（与 weddingPeersPresent 一一对应）：
//   - target_online          对方是否在线（有绑定会话）
//   - same_channel           是否同一频道
//   - from/target_at_priest  两人是否都在神父 NPC 20013 旁（含场景就绪）
//   - *_ready                townSceneReadyCharacterID 是否等于本人
//   - peer_same_area         是否注册在同一个场景实例
//
// 另外把坐标、所在城镇/区域和"身边识别到的 NPC 列表"一并打出来 —— 神父那条最容易
// 悄悄不成立：容差只有 ±120/±80，坐标差一点点就会被判成"不在旁边"。
func (s *Service) weddingProposeDiagnostics(
	from boundGameSessionCharacter, to boundGameSessionCharacter, online bool,
) []any {
	fields := []any{"target_online", online, "from_char", from.character, "target_char", to.character}
	if !online {
		return fields
	}
	fields = append(fields, "same_character", from.character == to.character)
	fromChannel, targetChannel := 0, 0
	if from.session != nil {
		fromChannel = from.session.channel.ID
	}
	if to.session != nil {
		targetChannel = to.session.channel.ID
	}
	fields = append(fields, "from_channel", fromChannel, "target_channel", targetChannel, "same_channel", fromChannel == targetChannel)
	fields = append(fields,
		"from_at_priest", s.weddingAtPriest(from),
		"target_at_priest", s.weddingAtPriest(to))
	fields = append(fields, s.weddingPriestPositionDiagnostics("from", from)...)
	fields = append(fields, s.weddingPriestPositionDiagnostics("target", to)...)
	if s.onlinePlayers != nil {
		fields = append(fields,
			"from_session_bound", s.onlinePlayers.SessionForCharacter(from.character) == from.session,
			"target_session_bound", s.onlinePlayers.SessionForCharacter(to.character) == to.session,
			"peer_same_area", s.onlinePlayers.PeerInSameArea(from.character, to.character))
	} else {
		fields = append(fields, "online_players_manager", "nil")
	}
	return fields
}

// weddingPriestPositionDiagnostics 报告一个会话"离神父还有多远"。
// prefix 用于区分发起方(from)与目标(target)。
func (s *Service) weddingPriestPositionDiagnostics(prefix string, identity boundGameSessionCharacter) []any {
	session := identity.session
	if session == nil {
		return []any{prefix + "_session", "nil"}
	}
	// TryLock 与 weddingAtPriest 同款：拿不到就只记锁竞争，不去阻塞探测。
	if !session.townMu.TryLock() {
		return []any{prefix + "_town_mu_busy", true}
	}
	position := session.townPositionSnapshot
	ready := session.townSceneReadyCharacterID == identity.character
	session.townMu.Unlock()
	nearby := s.currentTownNearbyNPCIDs(position)
	nearPriest := slices.Contains(nearby, currentWeddingPriestNPCID)
	return []any{
		prefix + "_ready", ready,
		prefix + "_snapshot_char", position.CharacterID,
		// pos_valid 是最容易致命的一条：换场景（op36）会把快照置为无效，
		// 而 currentTownNearbyNPCIDs 在无效时直接返回 nil，于是"在神父旁"
		// 永远判 false —— 看起来就像"搜不到玩家"。
		prefix + "_pos_valid", position.PositionValid,
		prefix + "_town", position.TownID,
		prefix + "_area", position.AreaID,
		prefix + "_x", position.PositionX,
		prefix + "_y", position.PositionY,
		prefix + "_nearby_npcs", nearby,
		prefix + "_near_priest", nearPriest,
	}
}

func (s *Service) handleWeddingResponse(session *gameSession, body []byte) error {
	if len(body) != 21 {
		return nil
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	target, op, kind, delay, decision := binary.LittleEndian.Uint32(body[13:17]), body[17], body[18], body[19], body[20]
	if target > 65535 || decision > 2 {
		return s.weddingReply(session, 1033, 0, op, kind, delay, wedding.Invalid, decision)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	inv := s.weddingInvitations[identity.character]
	// sub_182A330 sends peer zero when declining or timing out; acceptance
	// must still name the proposer, and only this recipient's invitation applies.
	if target == 0 && decision != 0 && inv != nil && inv.to == identity {
		target = uint32(inv.from.character)
	}
	// sub_1829F50 acknowledges a ceremony with zero kind/delay; retain the requester's selections.
	if inv == nil || inv.to != identity || inv.from.character != uint16(target) || inv.operation != op || kind != 0 || delay != 0 {
		return s.weddingReply(session, 1033, uint16(target), op, kind, delay, wedding.Missing, decision)
	}
	s.removeWeddingInvitationLocked(inv)
	kind, delay = inv.kind, inv.delay
	if !s.gameplayNow().Before(inv.expires) {
		decision = 2
	}
	var result wedding.Result
	var err error
	if !s.weddingPeersPresent(inv.from, inv.to) {
		err = wedding.Missing
	}
	if err == nil && decision == 0 {
		cat, e := s.currentWeddingCatalog()
		err = e
		if err == nil {
			repos, ok := s.repositoryGroup()
			if !ok {
				err = dnfrepo.ErrRepoMissing
			} else {
				room := byte(0)
				if op == 2 {
					room, err = s.weddingRoom()
				}
				cmd := wedding.Command{ActorID: strconv.Itoa(int(inv.from.character)), PartnerID: strconv.Itoa(int(inv.to.character)), AccountID: s.accountIDForSession(inv.from.session), PartnerAccountID: s.accountIDForSession(inv.to.session), Operation: op, Kind: kind, Delay: delay, Room: room, Now: s.gameplayNow()}
				cmd.ChannelID = uint16(inv.from.session.channel.ID)
				if err == nil && op == 2 {
					cmd.Grant, err = s.weddingGiftAllocator(cat, kind)
				}
				if err == nil {
					result, err = wedding.Commit(ctx, repos, cat, cmd)
				}
			}
		}
	}
	ackErr := s.weddingReply(session, 1033, uint16(target), op, kind, delay, err, decision)
	var code wedding.Error
	if err != nil && !errors.As(err, &code) {
		code = wedding.Invalid
	}
	ackErr = errors.Join(ackErr, s.notifyWeddingResult(inv, code, decision))
	s.logGameEvent(session, "game-wedding-decision", "from_char_id", target, "target_id", identity.character, "operation", op, "decision", decision, "result", int32(code))
	if err == nil && decision == 0 {
		ackErr = errors.Join(ackErr, s.publishWeddingCommit(result, op))
		if op == 2 {
			ackErr = errors.Join(ackErr, s.publishWeddingReservation(result.Characters), s.scheduleWeddingCeremony(result.Characters[0]))
			ackErr = errors.Join(ackErr, s.sendWeddingInventorySnapshot(inv.from.session, result.Inventories[0]))
		}
	}
	return ackErr
}

func (s *Service) notifyWeddingResult(inv *currentWeddingInvitation, code wedding.Error, decision byte) error {
	if !s.boundGameSessionCharacterCurrent(inv.from) {
		return nil
	}
	return s.sendGameUpperRawClass(inv.from.session, currentWeddingResultMsgID, weddingResultBody(int32(code), inv.to.character, inv.operation, inv.kind, inv.delay, decision), 0)
}

func (s *Service) removeWeddingInvitationLocked(inv *currentWeddingInvitation) {
	for _, id := range []uint16{inv.from.character, inv.to.character} {
		if s.weddingInvitations[id] == inv {
			delete(s.weddingInvitations, id)
		}
	}
	if inv.timer != nil {
		inv.timer.Stop()
	}
}

func (s *Service) expireWeddingInvitation(inv *currentWeddingInvitation) {
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	if s.weddingInvitations[inv.to.character] != inv {
		return
	}
	s.removeWeddingInvitationLocked(inv)
	if err := s.notifyWeddingResult(inv, 0, 2); err != nil {
		s.logGameEvent(inv.from.session, "game-wedding-timeout-send-failed", "error", err)
	}
}

func (s *Service) cancelWeddingForCharacterLocked(id uint16) {
	if inv := s.weddingInvitations[id]; inv != nil {
		s.removeWeddingInvitationLocked(inv)
		if err := s.notifyWeddingResult(inv, 0, 2); err != nil {
			s.logGameEvent(inv.from.session, "game-wedding-cancel-send-failed", "error", err)
		}
	}
}

func (s *Service) clearWeddingInvitations(session *gameSession) {
	if session == nil {
		return
	}
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	for _, inv := range s.weddingInvitations {
		if inv.from.session == session || inv.to.session == session {
			s.cancelWeddingForCharacterLocked(inv.from.character)
		}
	}
}

// sub_18355B0: wedding ID, status, ceremony, date, love points, ring.
func weddingStateBody(state wedding.State) []byte {
	if state.Status == 0 {
		state.PairID, state.Ring = 0xffffffff, 0xff
	}
	var w packetWriter
	w.writeUint32(state.PairID)
	w.writeByte(state.Status)
	w.writeByte(state.Ceremony)
	w.writeUint32(uint32(state.Date))
	w.writeUint32(uint32(state.Love))
	w.writeByte(state.Ring)
	return w.bytes()
}

func (s *Service) publishWeddingCommit(result wedding.Result, operation byte) error {
	var err error
	for _, c := range result.Characters {
		id, _ := strconv.ParseUint(c.CharacterID, 10, 16)
		identity, current := s.onlineGameSessionCharacterSnapshot(uint16(id))
		if !current {
			continue
		}
		// Full state (1000) destroys the native partner preview. Item changes
		// have dedicated notifications which preserve that actor and window.
		if operation == 8 {
			err = errors.Join(err, s.sendGameUpperRawClass(identity.session, currentWeddingRingMsgID, []byte{byte(c.Stats["wedding_ring"])}, 0))
			continue
		}
		if operation == 9 {
			var w packetWriter
			w.writeUint32(uint32(id))
			w.writeUint32(uint32(c.Stats["wedding_love"]))
			err = errors.Join(err, s.sendGameUpperRawClass(identity.session, currentWeddingLoveMsgID, w.bytes(), 0))
			continue
		}
		if operation >= 5 && operation <= 7 {
			err = errors.Join(err, s.sendGameUpperRawClass(identity.session, currentWeddingBreakupMsgID, []byte{operation}, 0))
		}
		err = errors.Join(err, s.sendWeddingCharacterState(identity.session, c))
		if operation == 4 || (operation >= 5 && operation <= 7) {
			err = errors.Join(err, s.sendWeddingCapsuleState(identity.session, c))
		}
		if operation == 1 || (operation >= 5 && operation <= 7) {
			err = errors.Join(err, s.sendCurrentAcceptableQuestListOnlyForSession(identity.session, "wedding_relationship_changed"))
		}
		if result.AssetsChanged {
			err = errors.Join(err, s.sendGameUpperRawClass(identity.session, 14, buildCurrentGoldStateBody(c.Stats["gold"]), 0))
		}
		state, stateErr := wedding.Read(c)
		if stateErr == nil && s.onlinePlayers != nil {
			for _, peer := range s.onlinePlayers.PeersInSameArea(identity.character) {
				if peer.Session != nil {
					err = errors.Join(err, s.sendGameUpperRawClass(peer.Session, currentWeddingActorMsgID, weddingActorStateBody(identity.character, state), 0))
				}
			}
		}
	}
	return err
}

func (s *Service) sendWeddingCharacterState(session *gameSession, c dnfrepo.CharacterRecord) error {
	state, err := wedding.Read(c)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentWeddingStateMsgID, weddingStateBody(state), 0); err != nil {
		return err
	}
	session.weddingStateSent.Store(true)
	// sub_1834DA0 clears the partner preview; sub_324E6C0 needs its name
	// before the wedding dungeon menu can locate the spouse in the party.
	if state.Status >= 1 && state.Status <= 4 {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		return s.sendWeddingPartnerState(ctx, session, c)
	}
	return nil
}

func weddingActorStateBody(id uint16, state wedding.State) []byte {
	if state.Status == 0 {
		state.PairID, state.Ring = 0xffffffff, 0xff
	}
	var w packetWriter
	w.writeUint32(uint32(id))
	w.writeUint32(state.PairID)
	w.writeByte(state.Status)
	w.writeByte(state.Ring)
	w.writeUint32(uint32(state.Date))
	return w.bytes()
}
