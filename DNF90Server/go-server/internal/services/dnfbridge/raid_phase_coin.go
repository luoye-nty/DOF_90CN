package dnfbridge

import (
	"encoding/binary"
)

// runtimeRaidLukePhaseCoinApplies 报告这名成员当前是否处在「队伍卢克」的已激活阶段里 ——
// 也就是 [PHASE COIN LIMIT] / [coin limit] 两层复活币上限该生效的场合。
//
// ⚠️ 这是本树对上游的**有意收窄**。上游把计币挂钩接到「所有非单人模式」的复活路径上；
// 真 PVF 实测（2026-09-29，runtime\data\dnf\Script.pvf，1450 个副本里 530 个声明了
// [coin limit]）显示安徒恩的 210..229 全部声明 [coin limit] 12，而卢克一阶段的三个
// 圣所 5054/5055/5056 是 8、二阶段的关卡则没有这一节。若照搬上游，安徒恩会凭空多出
// 一层「每次进图 12 次」的封顶，改动用户已经验收过的复活币路径。
// 因此本树只对队伍卢克启用；安徒恩的复活路径逐字未动。
func (s *Service) runtimeRaidLukePhaseCoinApplies(characterID uint16) bool {
	if s == nil || characterID == 0 {
		return false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.TailFlag == 1 || !state.Started ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return false
	}
	return runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke
}

// Raid phase revive-coin cap.
//
// A phase that declares [PHASE COIN LIMIT] (only Luke phase two: 20) caps how
// many revive coins the roster may spend in it. The client enforces the cap on
// its own, before any packet: sub_1FA9D50 shows dstr 3015 「已超出复活币的使用
// 上限」 when, inside a raid map, `used >= limit`:
//
//   - limit = raid singleton sub_962750()+0xE4 (getter sub_1361F10), loaded by the
//     client from its own PVF; -1 means unlimited, which is why phase one (no
//     [PHASE COIN LIMIT]) never trips it.
//   - used  = the local player's dungeon-party object (sub_1D935D0 of the owner)
//     +0xE0, an obfuscated int read by sub_26392F0 and written by sub_2639250.
//     sub_1FA94E0 returns it for the raid branch; +0xE8 (sub_26394C0 / setter
//     sub_2639560) is the same counter for another mode.
//
// Nothing in the server ever set `used`, so it carried whatever it had collected
// before phase two and the first phase-two coin already failed (live
// 2026-09-23). Its only external writer is class-0 op659, runtime-registered
// with handler sub_12D02A0 (registration 0x12D0483, through sub_225C940, which
// overrides the static table's op659): body u16 member, u32 A -> +0xE8,
// u32 B -> +0xE0. The handler bails unless the dungeon-party object exists (in a
// dungeon only), and writes only when `member` equals the encoded global
// 0x52B5DE4 -- the local player's party-member id (compared with a party member
// row's u16 +0x38 at 0xA4D1F3; initialised to 0xFFFF by 0x40CA040).
//
// The situation board's 「攻坚队复活币」 is a different field again: the 0x027A
// monster-state row's Value0, S4A21's UsedCoinCount (raid_monster_state.go).
//
// So the server keeps the phase's roster-wide count and projects it:
//   - after every dungeon FINISH_LOADING, to the member that just loaded;
//   - after every accepted coin, to every online member;
//   - and it refuses a coin itself once the cap is reached.
// Each member gets two copies, one keyed by its character id (what this server
// uses as party-member id) and one keyed 0xFFFF (the global's initial value, in
// case the client never assigned it); a copy whose key does not match is ignored
// by the client.

const (
	runtimeRaidPhaseCoinUsageMsgID    uint16 = 659
	runtimeRaidPhaseCoinUsageUnsetKey uint16 = 0xFFFF
	// runtimeRaidSquadCoinUsedMsgID is class-0 op775: handler sub_1D575D0
	// (registered at 0x1D7F8AD) reads one u8 and stores it through
	// sub_1E69F00 into the raid-member manager [0x51B2748]+0x194.
	//
	// That field is what the client really counts once the member sits in a
	// squad with a coin total (sub_17C3120 != -1): sub_1FA94E0 then returns it
	// as "used", and the situation board's 「攻坚队复活币」 shows total minus it.
	// The server never sent op775, so the field held heap garbage: live
	// 2026-09-24 the F9 probe read 220531861 on one client (revive refused
	// silently) and -2009364085 on the other (revive allowed), with op659's
	// +0xE0 correctly at 2 on both.
	runtimeRaidSquadCoinUsedMsgID uint16 = 775
)

// runtimeRaidSquadCoinUsed is the op775 value for a raid: the phase count where
// the phase is capped, 0 otherwise -- never left unset.
func runtimeRaidSquadCoinUsed(state runtimeRaidState) byte {
	if runtimeRaidPhaseCoinLimit(state) <= 0 || state.Lifecycle.PhaseCoinsUsed <= 0 {
		return 0
	}
	if state.Lifecycle.PhaseCoinsUsed > 0xFF {
		return 0xFF
	}
	return byte(state.Lifecycle.PhaseCoinsUsed)
}

func runtimeRaidSquadCoinUsedNotification(state runtimeRaidState) runtimeRaidWireNotification {
	return runtimeRaidWireNotification{
		msgID: runtimeRaidSquadCoinUsedMsgID,
		body:  []byte{runtimeRaidSquadCoinUsed(state)},
	}
}

func buildRuntimeRaidPhaseCoinUsageBody(memberKey uint16, used uint32) []byte {
	body := make([]byte, 10)
	binary.LittleEndian.PutUint16(body[0:2], memberKey)
	binary.LittleEndian.PutUint32(body[2:6], used)
	binary.LittleEndian.PutUint32(body[6:10], used)
	return body
}

// runtimeRaidPhaseCoinLimit is the active phase's [PHASE COIN LIMIT], or 0 when
// the phase has none or the raid is not in an active phase.
func runtimeRaidPhaseCoinLimit(state runtimeRaidState) int {
	if !state.Started || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return 0
	}
	phase, _, ok := runtimeRaidPhaseConfig(state, state.Lifecycle.Phase)
	if !ok {
		return 0
	}
	return phase.CoinLimit
}

type runtimeRaidPhaseCoinStatus struct {
	RaidKey uint32
	Limit   int
	Used    int
	// MapLimit / MapUsed are the dungeon's own [coin limit] for the entry the
	// member is in (Luke phase one: 8 per sanctum entry), 0 when it has none.
	MapLimit int
	MapUsed  int
	// RunKey is the entry the map count was charged to.
	RunKey    runtimeRaidDungeonRunKey
	MapCapped bool
	// Refused names the cap that stopped the coin: "phase" or "map".
	Refused string
}

// runtimeRaidPhaseCoinStatusForMember reports the member's capped phase, if any.
func (s *Service) runtimeRaidPhaseCoinStatusForMember(characterID uint16) (runtimeRaidPhaseCoinStatus, bool) {
	if s == nil || characterID == 0 {
		return runtimeRaidPhaseCoinStatus{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		return runtimeRaidPhaseCoinStatus{}, false
	}
	limit := runtimeRaidPhaseCoinLimit(*state)
	if limit <= 0 {
		return runtimeRaidPhaseCoinStatus{}, false
	}
	return runtimeRaidPhaseCoinStatus{RaidKey: state.RaidKey, Limit: limit, Used: state.Lifecycle.PhaseCoinsUsed}, true
}

// commitRuntimeRaidPhaseCoin charges one coin against both raid caps: the
// phase's [PHASE COIN LIMIT] (roster-wide) and the dungeon's own [coin limit]
// for the entry the member is fighting (squad-wide, reset by re-entry).
// dungeonCoinLimit is the current dungeon's [coin limit], 0 for none. It
// refuses when either cap is reached; ok is false when neither applies.
func (s *Service) commitRuntimeRaidPhaseCoin(characterID uint16, mapID int, dungeonCoinLimit int) (status runtimeRaidPhaseCoinStatus, ok bool, allowed bool) {
	if s == nil || characterID == 0 {
		return runtimeRaidPhaseCoinStatus{}, false, true
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || !state.Started || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return runtimeRaidPhaseCoinStatus{}, false, true
	}
	status = runtimeRaidPhaseCoinStatus{
		RaidKey: state.RaidKey,
		Limit:   runtimeRaidPhaseCoinLimit(*state),
		Used:    state.Lifecycle.PhaseCoinsUsed,
	}
	var progress runtimeRaidDungeonProgress
	if dungeonCoinLimit > 0 && mapID > 0 {
		if member, found := runtimeRaidFrozenMember(*state, characterID); found {
			key := runtimeRaidDungeonRunKey{MapID: mapID, Seat: runtimeRaidSeatFor(member)}
			if current, exists := state.Lifecycle.Dungeons[key]; exists {
				progress = current
				status.RunKey = key
				status.MapCapped = true
				status.MapLimit = dungeonCoinLimit
				status.MapUsed = current.CoinsUsed
			}
		}
	}
	phaseCapped := status.Limit > 0
	if !phaseCapped && !status.MapCapped {
		return runtimeRaidPhaseCoinStatus{}, false, true
	}
	if phaseCapped && status.Used >= status.Limit {
		status.Refused = "phase"
		return status, true, false
	}
	if status.MapCapped && status.MapUsed >= status.MapLimit {
		status.Refused = "map"
		return status, true, false
	}
	if phaseCapped {
		state.Lifecycle.PhaseCoinsUsed++
		status.Used = state.Lifecycle.PhaseCoinsUsed
	}
	if status.MapCapped {
		progress.CoinsUsed++
		state.Lifecycle.Dungeons[status.RunKey] = progress
		status.MapUsed = progress.CoinsUsed
	}
	state.Lifecycle.Revision++
	return status, true, true
}

// refundRuntimeRaidPhaseCoin undoes a charge whose revive then failed (wallet
// error or no coin), so a refused revive does not eat into either cap.
func (s *Service) refundRuntimeRaidPhaseCoin(status runtimeRaidPhaseCoinStatus) {
	if s == nil || status.RaidKey == 0 {
		return
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[status.RaidKey]
	if state == nil {
		return
	}
	if status.Limit > 0 && state.Lifecycle.PhaseCoinsUsed > 0 {
		state.Lifecycle.PhaseCoinsUsed--
	}
	if status.MapCapped {
		if progress, exists := state.Lifecycle.Dungeons[status.RunKey]; exists && progress.CoinsUsed > 0 {
			progress.CoinsUsed--
			state.Lifecycle.Dungeons[status.RunKey] = progress
		}
	}
	state.Lifecycle.Revision++
}

// sendRuntimeRaidPhaseCoinUsage writes both keyed copies to one member.
func (s *Service) sendRuntimeRaidPhaseCoinUsage(session *gameSession, used int, source string) {
	if s == nil || session == nil || used < 0 {
		return
	}
	characterID := session.selectedCharacterID
	for _, key := range []uint16{currentSceneActorObjectKey(characterID), runtimeRaidPhaseCoinUsageUnsetKey} {
		if err := s.sendGameUpperRawClass(
			session,
			runtimeRaidPhaseCoinUsageMsgID,
			buildRuntimeRaidPhaseCoinUsageBody(key, uint32(used)),
			0,
		); err != nil {
			s.logGameEvent(session, "game-raid-phase-coin-usage-send-failed",
				"source", source, "char_id", characterID, "member_key", key, "error", err)
			return
		}
	}
	s.logGameEvent(session, "game-raid-phase-coin-usage-sent",
		"source", source,
		"char_id", characterID,
		"used", used,
		"response_opcode", runtimeRaidPhaseCoinUsageMsgID,
		"member_keys", "char_id_and_0xffff")
}

// syncRuntimeRaidPhaseCoinUsageAfterFinishLoading projects the phase count onto a
// member whose dungeon just finished loading: the client's counter lives on the
// dungeon-party object, which exists only now.
func (s *Service) syncRuntimeRaidPhaseCoinUsageAfterFinishLoading(session *gameSession, source string) {
	if s == nil || session == nil {
		return
	}
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if !inDungeon {
		return
	}
	if state, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok && state.Started && state.TailFlag != 1 {
		// op775 goes out for every phase, capped or not: the field it sets is
		// otherwise uninitialised and decides whether the client lets a coin go.
		notification := runtimeRaidSquadCoinUsedNotification(state)
		if err := s.sendGameUpperRawClass(session, notification.msgID, notification.body, 0); err != nil {
			s.logGameEvent(session, "game-raid-squad-coin-used-send-failed",
				"source", source, "char_id", session.selectedCharacterID, "error", err)
		}
	}
	status, capped := s.runtimeRaidPhaseCoinStatusForMember(session.selectedCharacterID)
	if !capped {
		return
	}
	s.sendRuntimeRaidPhaseCoinUsage(session, status.Used, source)
}

// broadcastRuntimeRaidPhaseCoinUsage pushes a new count to every online member.
// Members outside a dungeon drop it on the client and are caught up by their
// next FINISH_LOADING.
func (s *Service) broadcastRuntimeRaidPhaseCoinUsage(raidKey uint32, used int, source string) {
	if s == nil || raidKey == 0 {
		return
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	members := make([]uint16, 0)
	if state != nil {
		for _, member := range state.FrozenMembers {
			members = append(members, member.CharID)
		}
		if len(members) == 0 {
			for _, member := range state.Members {
				members = append(members, member.CharID)
			}
		}
	}
	var snapshot runtimeRaidState
	if state != nil {
		snapshot = cloneRuntimeRaidState(*state)
	}
	s.raidMu.Unlock()
	for _, characterID := range members {
		session, online := s.onlineGameSession(characterID)
		if !online {
			continue
		}
		s.sendRuntimeRaidPhaseCoinUsage(session, used, source)
	}
	// op775 carries the count the client actually checks and the board shows;
	// the 0x027A rows carry it too (Value0, S4A21's UsedCoinCount).
	if state != nil {
		if err := s.broadcastRuntimeRaidWireSequence(snapshot, runtimeRaidSquadCoinUsedNotification(snapshot)); err != nil {
			s.logPacketEvent("game-raid-squad-coin-used-broadcast-failed",
				"raid_key", raidKey, "source", source, "error", err)
		}
		// 本树这一版的 broadcastRuntimeRaidMonsterState 只收 state（上游那版多一个
		// source 参数，只用于日志）。这里按本树签名调用，行为一致。
		if err := s.broadcastRuntimeRaidMonsterState(snapshot); err != nil {
			s.logPacketEvent("game-raid-phase-coin-board-refresh-failed",
				"raid_key", raidKey, "source", source, "error", err)
		}
	}
}
