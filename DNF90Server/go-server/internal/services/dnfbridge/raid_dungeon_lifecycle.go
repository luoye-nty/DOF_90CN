package dnfbridge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

const (
	runtimeRaidDungeonPhaseOne byte = 1
	runtimeRaidDungeonPhaseTwo byte = 2
)

type runtimeRaidDungeonLifecycleKind byte

const (
	runtimeRaidDungeonSelected runtimeRaidDungeonLifecycleKind = iota + 1
	runtimeRaidDungeonLoaded
	runtimeRaidDungeonCompleted
	runtimeRaidDungeonAbandoned
)

type runtimeRaidDungeonBinding struct {
	RaidKey        uint32
	Phase          byte
	MapID          int
	LifecycleToken uint64

	selectedNotified  bool
	loadedNotified    bool
	completedNotified bool
	abandonedNotified bool
}

type runtimeRaidDungeonLifecycleEvent struct {
	Kind           runtimeRaidDungeonLifecycleKind
	RaidKey        uint32
	Phase          byte
	MapID          int
	LifecycleToken uint64
	CharacterID    uint16
}

// runtimeRaidDungeonLifecycleSink is the narrow hand-off to the raid state
// machine. The dungeon owner deliberately does not know any raid packet body,
// reward, timer, or final-cleanup rule.
type runtimeRaidDungeonLifecycleSink interface {
	applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonLifecycleEvent) error
}

type runtimeRaidDungeonBeginner interface {
	beginRuntimeRaidDungeon(characterID uint16, mapID int, lifecycleToken uint64) (runtimeRaidDungeonBinding, error)
}

func runtimeRaidUint32MemberKeys(members []uint16) []uint32 {
	keys := make([]uint32, 0, len(members))
	for _, member := range members {
		keys = append(keys, uint32(member))
	}
	return keys
}

func runtimeRaidDungeonPhaseForMap(mapID int) (byte, bool) {
	// 卢克队伍攻坚的门不是一个连续区间：一阶段是 5054..5056，二阶段是 5057..5066
	// 外加离得很远的两个祭坛入口 5311/5313。见 runtimeRaidLukeTeamDungeonPhases。
	// 非卢克 id 落到下面原来的分支，行为不变。
	if phase, ok := runtimeRaidLukeTeamDungeonPhases[mapID]; ok {
		return phase, true
	}
	switch {
	case mapID >= 5314 && mapID <= 5322:
		return runtimeRaidDungeonPhaseOne, true
	case mapID >= 5323 && mapID <= 5334:
		return runtimeRaidDungeonPhaseOne, true
	case mapID >= 210 && mapID <= 216:
		return runtimeRaidDungeonPhaseOne, true
	case mapID >= 218 && mapID <= 224:
		return runtimeRaidDungeonPhaseTwo, true
	// 超时空攻坚战：etc/raid/fiendwar/fiendwar.etc 的 [DUNGEON INDEX] 覆盖
	// 4953..4978；PVF 的 dungeon/dungeon.lst 里 4952 与 4979 都不存在，所以这是一段
	// 封闭区间。超时空只有一个阶段（[PHASE MAX] 1），因此全部落在一阶段。
	// 与既有区间（210..224、5314..5334）无交集，不改变任何旧 id 的判定。
	case mapID >= 4953 && mapID <= 4978:
		return runtimeRaidDungeonPhaseOne, true
	default:
		return 0, false
	}
}

// freezeRuntimeRaidDungeonSelection binds a started, paid raid to exactly one
// committed dungeon run. A normal dungeon, an unstarted raid, or a non-member
// keeps the zero binding and therefore follows the existing dungeon path.
func (s *Service) freezeRuntimeRaidDungeonSelection(session *gameSession, runtime *runtimeDungeonState) error {
	if s == nil || session == nil || runtime == nil || runtime.lifecycleToken == 0 {
		return nil
	}
	beginner, ok := any(s).(runtimeRaidDungeonBeginner)
	if !ok {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return nil
	}
	// 超时空：一个小队共用一个区域尝试。如果本成员所在的小队已经有人开了这个区域
	// 的局，就直接采用那份绑定而不是再开一份 —— 各开一份会让战场的"区域归属"与
	// 双方的副本实例对不上。安东/卢克没有"小队共享区域"这回事，
	// runtimeFiendWarPartyBinding 对它们恒返回 false，走的还是原来那一行。
	binding, shared := s.runtimeFiendWarPartyBinding(session, runtime)
	var err error
	if !shared {
		binding, err = beginner.beginRuntimeRaidDungeon(
			session.selectedCharacterID,
			int(runtime.Request.DungeonID),
			runtime.lifecycleToken,
		)
	}
	if err != nil {
		return err
	}
	if binding.RaidKey == 0 {
		return nil
	}
	if runtime.raidDungeon.RaidKey != 0 {
		if runtime.raidDungeon.RaidKey != binding.RaidKey ||
			runtime.raidDungeon.Phase != binding.Phase ||
			runtime.raidDungeon.MapID != binding.MapID ||
			runtime.raidDungeon.LifecycleToken != binding.LifecycleToken {
			return fmt.Errorf("raid dungeon binding changed during active run")
		}
		return s.notifyRuntimeRaidDungeonLifecycleLocked(session, runtime, runtimeRaidDungeonSelected)
	}
	if binding.MapID != int(runtime.Request.DungeonID) {
		return fmt.Errorf("raid dungeon begin returned mismatched binding")
	}
	runtime.raidDungeon = binding
	if binding.LifecycleToken != runtime.lifecycleToken {
		// The squad is already inside this gate and this session joined that run:
		// the binding belongs to the member who opened it. Its lifecycle events are
		// deliberately not published -- notifyRuntimeRaidDungeonLifecycleLocked
		// requires the tokens to match -- so the run is reported once, by its owner,
		// and a second member cannot double-count the squad's clear.
		return nil
	}
	if err := s.notifyRuntimeRaidDungeonLifecycleLocked(session, runtime, runtimeRaidDungeonSelected); err != nil {
		runtime.raidDungeon = runtimeRaidDungeonBinding{}
		return err
	}
	return nil
}

func (s *Service) notifyRuntimeRaidDungeonLoaded(session *gameSession) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return s.notifyRuntimeRaidDungeonLifecycleLocked(
		session,
		session.dungeon.runtime,
		runtimeRaidDungeonLoaded,
	)
}

func (s *Service) notifyRuntimeRaidDungeonCompletedLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	return s.notifyRuntimeRaidDungeonLifecycleLocked(session, runtime, runtimeRaidDungeonCompleted)
}

// runtimeRaidCompletedDungeonOwnsNativeSettlement reports the exact hand-off
// where the completed Raid objective has already opened the native result
// movie/card lifecycle. Ordinary op31 must not be sent after this point: the
// current EXE answers state=3 with C2S 659/660 and that protocol owns the Raid
// result, Anton card pool, durable reward, and final Raid cleanup.
//
// A completed phase is read from the objectives, not only from the lifecycle
// status: the phase-completion tail is published off the completing member's own
// call stack (scheduleRuntimeRaidPhaseCompletion, so that its forced returns never
// run under that member's session lock), which means the movie it opens is not yet
// visible at the moment this is asked. Reading the objectives answers the same
// question without depending on that timing -- and it also covers the case the old
// status-only test got wrong: a tail whose movie could not open because a gate was
// still occupied used to fall through to the ordinary op31, i.e. a card screen a
// raid gate never sends.
func (s *Service) runtimeRaidCompletedDungeonOwnsNativeSettlement(
	characterID uint16,
	binding runtimeRaidDungeonBinding,
) bool {
	if s == nil || characterID == 0 || binding.RaidKey == 0 || !binding.completedNotified {
		return false
	}
	state, ok := s.runtimeRaidForMember(characterID)
	if !ok ||
		state.RaidKey != binding.RaidKey ||
		// 超时空与单人攻坚一样自己拥有一笔副本的收尾：它没有安徒恩/卢克那种
		// 由阶段推进器统一发布的结算。TailFlag 是单人标志，所以这里要单独
		// 放行 FiendWar owner，其余 owner 的判定逐字不变。
		(state.TailFlag != 1 && state.RaidMode != runtimeRaidOwnerFiendWar) ||
		!state.Started ||
		!state.EntryCostPaid ||
		state.Lifecycle.Phase != binding.Phase {
		return false
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleAwaitingMovie {
		return true
	}
	return state.Lifecycle.Status == runtimeRaidLifecyclePhaseActive &&
		runtimeRaidPhaseObjectivesComplete(state, binding.Phase)
}

func (s *Service) notifyRuntimeRaidDungeonAbandonedLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	return s.notifyRuntimeRaidDungeonLifecycleLocked(session, runtime, runtimeRaidDungeonAbandoned)
}

// runtimeRaidGateRowMembers reports the member list one gate's 0x0248 participation
// row must name for the member standing in that gate. Both publishers of such a row
// -- the entry (publishRuntimeRaidDungeonParticipation) and the transfer away
// (publishRuntimeRaidDungeonTransferAwayLocked) -- must agree on it, so it lives here.
//
// WHY the mover's squad byte comes from the LIVE row and not the frozen one: the
// client classifies the row by reading EACH LISTED MEMBER's own 0x024F roster byte
// (0 renders as 单人 -- see runtimeRaidMemberSquad) and matches it against the squad
// the row is drawn for. Filtering live members by a FROZEN squad byte therefore breaks
// the moment the two disagree, and they legitimately can: a member taken out of its
// squad while the raid runs keeps its frozen squad for the phase-clear credit
// (clearRuntimeRaidSquadLabel, `frozen_row=kept_for_phase_credit`) while its live
// 0x024F label becomes 0.
//
// Live 2026-09-30 23:54:54 (raid 1835009, char 22 unassigned, own build, from the
// decrypted packet log): the entry published `mc=0 keys=[]` -- a row naming NOBODY --
// because the mover's frozen squad byte was still 2 while no live member carried
// squad 2 any more. The operator's report is exactly that symptom: 「然后单人（没队伍
// 编制 显示的标志不对 应该显示单）」. A row with an empty member list gives the client
// nothing to classify, so neither the 战况表 cell nor the row's own marker can be drawn
// as 单人.
//
// The guarantee is therefore: the row is never empty. A real squad lists that squad's
// live members; an unassigned (or frozen/live drifted) member lists itself, which is
// exactly the singleton row the client renders as 单人.
func runtimeRaidGateRowMembers(state runtimeRaidState, characterID uint16, frozenGroup byte) []uint16 {
	group := runtimeRaidMemberSquad(frozenGroup)
	if index := runtimeRaidMemberIndex(state, characterID); index >= 0 {
		// The live row is what the 0x024F roster this client holds was built from.
		group = runtimeRaidMemberSquad(state.Members[index].GroupIndex)
	}
	members := runtimeRaidMembersInSquadLiveRows(state, group)
	if group == 0 || len(members) == 0 {
		return []uint16{characterID}
	}
	return members
}

// publishRuntimeRaidDungeonTransferAwayLocked drops the squad's row for the gate a
// party has just walked out of on the way into another gate of the same raid.
//
// The warp from 能量阻截战 (218) into an open hatchery deliberately does NOT report
// the 218 attempt as abandoned (see commitDungeonRuntime): the attempt stays valid
// so the squad can re-enter 218 and the shield it has already taken off stays off.
// But the attempt and the client's row are two different things, and skipping the
// abandoned event skipped both. Nothing else removes that row either, so the 战况表
// kept drawing the squad inside 能量阻截战 for the rest of the phase while the
// standby room stayed empty. Re-confirmed by the operator on 2026-09-20 after this
// call was briefly reverted: "战况表不刷新 从孵化出来 还显示在能量里".
//
// Live 2026-09-20, raid 1507329: the only 0x0248 the transfer ever produced were
// {224,op=0}/{224,op=1} at 02:13:33.995Z (and {224,op=0}/{224,op=4} when 224 was
// cleared at 02:14:30.299Z) -- no packet ever named 218, so its row could only go
// when the raid itself ended.
//
// op=0 is exactly this operation for the current EXE: sub_1D438D0's jump table
// (0x1D43E80) maps op=0 to sub_17C0FB0, which removes the listed members from that
// dungeon's row and prints nothing, where op=3 is the SAME call plus the 32050
// 「其中1支战队闯关[%s]地下城失败。」 line -- which would be a lie here, because
// nothing failed. op=1/op=2 go to a different function (sub_17C0F20) that inserts
// instead. 86JP separates them the same way: Runtime.cs:369-403 sends the row
// update on every participation change, and only
// BroadcastRaidParticipationAfterMemberRemovalLockedAsync
// (Situation.cs:563-601) adds the failure line.
//
// Runs while session.dungeon.mu is held, like the abandoned notification it stands
// in for. Only the row travels: no lifecycle flag, no occupancy and no terminal
// record is touched, so the 218 attempt is exactly as valid as it was.
func (s *Service) publishRuntimeRaidDungeonTransferAwayLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if s == nil || session == nil || runtime == nil {
		return nil
	}
	binding := runtime.raidDungeon
	if binding.RaidKey == 0 || binding.MapID == 0 {
		return nil
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.RaidKey != binding.RaidKey || state.Lifecycle.Phase != binding.Phase {
		return nil
	}
	member, ok := runtimeRaidFrozenMember(state, session.selectedCharacterID)
	if !ok {
		return nil
	}
	// The row names the squad, exactly as the row the entry published did: a real
	// squad lists that squad and only that squad, and an unassigned member's row
	// lists nobody but itself (see runtimeRaidGateRowMembers).
	members := runtimeRaidGateRowMembers(state, session.selectedCharacterID, member.GroupIndex)
	body, err := raid.BuildRaidParticipationBody([]raid.RaidParticipationRow{{
		DungeonID:  uint32(binding.MapID),
		Op:         0,
		MemberKeys: runtimeRaidUint32MemberKeys(members),
	}})
	if err != nil {
		return err
	}
	return s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
		msgID: raid.RaidParticipationMsgID,
		body:  body,
	})
}

// notifyRuntimeRaidDungeonLifecycleLocked runs while session.dungeon.mu is
// held. A flag is committed only after the optional state-machine sink accepts
// the event, so a failed callback remains retryable and duplicate client
// packets cannot advance the same transition twice.
func (s *Service) notifyRuntimeRaidDungeonLifecycleLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	kind runtimeRaidDungeonLifecycleKind,
) error {
	if s == nil || session == nil || runtime == nil || session.dungeon.runtime != runtime {
		return nil
	}
	binding := &runtime.raidDungeon
	if binding.RaidKey == 0 || binding.LifecycleToken == 0 ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return nil
	}
	if binding.LifecycleToken != runtime.lifecycleToken {
		// The squad is already inside this gate and this session joined that run:
		// the binding belongs to the member who opened it, so the squad's run
		// *bookkeeping* stays single -- one record, one occupancy, one 战况表 row --
		// and the run itself is reported once, by its owner.
		//
		// The joined session still needs the local clear fact. Live incident
		// 2026-09-16 11:14Z: the leader's own client authoritatively cleared gate
		// 210, but because this branch returned silently its binding never carried
		// completedNotified, so the exit barrier classified the cleared raid gate as
		// an ordinary dungeon and blocked 返回城镇 forever with
		// op72_cannot_cross_unfinished_card_or_reward_phase (layout_sent=false,
		// selection_sent=false, reward_committed=false) on a card screen a raid gate
		// never sends. Record the local completion; the global state machine and the
		// participation broadcast stay owned by the opener.
		if kind != runtimeRaidDungeonCompleted || binding.abandonedNotified {
			// Diagnostic only, no behaviour change. This is the FIRST place a
			// completed event can be swallowed, and it is specific to a JOINED
			// member. Live 2026-09-20 (party volcano, 219): the run owner reached
			// game-dungeon-boss-die-check-op115-send and then nothing at all
			// followed -- no completed, no settlement entry -- while a SOLO run
			// 81 ms later produced both. Without this line the operator cannot
			// tell which of the three silent returns ate it.
			s.logGameEvent(session, "game-raid-dungeon-lifecycle-skipped-joined",
				"char_id", session.selectedCharacterID,
				"raid_key", binding.RaidKey,
				"raid_phase", binding.Phase,
				"raid_map_id", binding.MapID,
				"kind", kind.String(),
				"binding_lifecycle_token", binding.LifecycleToken,
				"local_lifecycle_token", runtime.lifecycleToken,
				"abandoned_notified", binding.abandonedNotified,
				"completed_notified", binding.completedNotified,
				"reason", "joined_member_only_records_completed_and_not_abandoned")
			return nil
		}
		// This session never published the joined binding's "selected" either, so
		// record both local facts: the exit barrier reads completedNotified and a
		// later duplicate op39/op117 must not try to report the run again.
		binding.selectedNotified = true
		runtimeRaidDungeonMarkLifecycleNotified(binding, kind)
		s.logGameEvent(session, "game-raid-dungeon-lifecycle-joined-clear",
			"char_id", session.selectedCharacterID,
			"raid_key", binding.RaidKey,
			"raid_phase", binding.Phase,
			"raid_map_id", binding.MapID,
			"binding_lifecycle_token", binding.LifecycleToken,
			"local_lifecycle_token", runtime.lifecycleToken,
			"dungeon_id", runtime.Dungeon.ID,
			"global_event_published", false,
			"reason", "joined_squadmate_records_only_its_own_clear")
		// The gate itself is not the squad's bookkeeping: this member fought and
		// cleared its own instance of it, so the clear spends one point of the
		// gate's HP. See applyRuntimeRaidJoinedSquadClear.
		s.applyRuntimeRaidJoinedSquadClear(session, *binding, runtime.lifecycleToken)
		return nil
	}
	if runtimeRaidDungeonLifecycleAlreadyNotified(binding, kind) {
		// Diagnostic only. Second candidate: the same kind was already reported on
		// this binding, which is what a reused run would look like on a gate the
		// squad clears repeatedly (219 is the only Anton gate with that shape).
		s.logGameEvent(session, "game-raid-dungeon-lifecycle-skipped-already-notified",
			"char_id", session.selectedCharacterID,
			"raid_key", binding.RaidKey,
			"raid_phase", binding.Phase,
			"raid_map_id", binding.MapID,
			"kind", kind.String(),
			"selected_notified", binding.selectedNotified,
			"completed_notified", binding.completedNotified,
			"abandoned_notified", binding.abandonedNotified,
			"reason", "same_kind_already_notified_on_this_binding")
		return nil
	}
	if kind != runtimeRaidDungeonSelected && !binding.selectedNotified {
		if err := s.notifyRuntimeRaidDungeonLifecycleLocked(session, runtime, runtimeRaidDungeonSelected); err != nil {
			return err
		}
	}
	if (kind == runtimeRaidDungeonCompleted && binding.abandonedNotified) ||
		(kind == runtimeRaidDungeonAbandoned && binding.completedNotified) {
		// Diagnostic only. Third candidate, and the one whose shape matches the
		// live failure most closely: the run owner reports the clear, the binding
		// already holds the OPPOSITE terminal fact, and the completed event is
		// dropped without a trace -- which is exactly "boss-die-check sent, then
		// nothing", with no settlement entry after it and the leader's client
		// stuck on completed_settlement_entry_owner_not_ready.
		s.logGameEvent(session, "game-raid-dungeon-lifecycle-skipped-abandoned-completed-conflict",
			"char_id", session.selectedCharacterID,
			"raid_key", binding.RaidKey,
			"raid_phase", binding.Phase,
			"raid_map_id", binding.MapID,
			"kind", kind.String(),
			"completed_notified", binding.completedNotified,
			"abandoned_notified", binding.abandonedNotified,
			"selected_notified", binding.selectedNotified,
			"binding_lifecycle_token", binding.LifecycleToken,
			"local_lifecycle_token", runtime.lifecycleToken,
			"reason", "the_binding_already_holds_the_opposite_terminal_fact")
		return nil
	}

	event := runtimeRaidDungeonLifecycleEvent{
		Kind:           kind,
		RaidKey:        binding.RaidKey,
		Phase:          binding.Phase,
		MapID:          binding.MapID,
		LifecycleToken: binding.LifecycleToken,
		CharacterID:    session.selectedCharacterID,
	}
	// 超时空：一个区域是小队共用的，所以"放弃区域"只有在小队里最后一个人也离开
	// 之后才算数。队友还在里面打的时候，这里只记本地标志然后返回 —— 战场不该把
	// 还有人站着的区域放掉，否则队友会被连同区域一起弹出去。
	if kind == runtimeRaidDungeonAbandoned && isCurrentFiendWarRaidChannel(session) && runtime.partyDungeonOwner != nil {
		survivorInside := false
		for _, survivor := range runtime.partyDungeonOwner.roster() {
			if survivor.userID != session.selectedCharacterID {
				survivorInside = true
				break
			}
		}
		if survivorInside {
			runtimeRaidDungeonMarkLifecycleNotified(binding, kind)
			return nil
		}
	}
	if sink, ok := any(s).(runtimeRaidDungeonLifecycleSink); ok {
		if err := sink.applyRuntimeRaidDungeonLifecycleEvent(event); err != nil {
			return err
		}
	}
	if err := s.publishRuntimeRaidDungeonParticipation(event); err != nil {
		return err
	}
	// Entering, abandoning or clearing a raid gate is a scene change for ONE
	// member: the member that just moved lost the records for everyone who stayed
	// behind, and the members that stayed never had a record for the one inside --
	// nor for the rest of the board row that member is drawn in.
	//
	// Scope history, because both cuts were driven by live reports:
	//   - it used to sweep every ordered pair (N*(N-1)) even though only the moved
	//     member's own scene changed, which on a 12-member Anton raid meant 132
	//     installs per boundary where 22 are stale, and 88% of the `loaded`
	//     boundary's were byte-identical repeats (measured 2026-09-27, 203.0.113.11);
	//   - it was cut to the moved member's own pairs (2*(N-1)) and that is what the
	//     operator then reported: "1队进图了 2队在外面看 看不到是哪队进图 就显示个单
	//     队长头像也是黑的" -- the row is resolved through the record of the member
	//     the row STARTS with, who is typically NOT the one that entered.
	// So the scope is now the moved member's board row (its squad, <= 4 members),
	// which is what runtimeRaidBoardRowInstallScope returns and what
	// TestRuntimeRaidDungeonBoundaryInstallsTheMovedMembersBoardRow pins.
	//
	// The pairs this hook does not cover stay covered by the hooks that actually
	// invalidate them: ordinary co-presence, the channel-wide pass for another
	// area of the same channel, raid_mate_actor_after_area_enter, admission, phase
	// activation (syncRuntimeRaidMateActorsAll, still a full sweep), and the board
	// window's own refresh (ensureRuntimeRaidBoardRowActors).
	//
	// `selected` is the one boundary where the moving client may still be between
	// scenes (its channel-owned scene already gone, the next one not built yet),
	// so that boundary defers the install for such a receiver; `loaded` repeats
	// the same install once the scene exists. See
	// syncRuntimeRaidMateActorsMovedAtSceneBoundary.
	if state, ok := s.runtimeRaidForMember(event.CharacterID); ok {
		fanoutStarted := time.Now()
		s.syncRuntimeRaidMateActorsMovedAtSceneBoundary(
			state,
			event.CharacterID,
			"raid_dungeon_"+kind.String()+"_raid_mate_actors",
			kind == runtimeRaidDungeonSelected)
		// 团本记录：一次门的场景边界 = 一次演员投影扇出。wall_ms 是这条边界真正的
		// 代价（历史上这里是 N*(N-1) 全量扫，现在是 2*(N-1)），participants 是
		// 这次扇出覆盖的名单，排查"某个人看不到队友/卡在加载"时最先要看的就是它。
		s.recordRaidStateEventLight(session, "gate_"+kind.String(), &state, map[string]any{
			"raid_map_id":                event.MapID,
			"lifecycle_token":            event.LifecycleToken,
			"fanout_source":              "raid_dungeon_" + kind.String() + "_raid_mate_actors",
			"fanout_mode":                "moved_member_board_row",
			"fanout_wall_ms":             raidMillis(time.Since(fanoutStarted)),
			"defer_while_between_scenes": kind == runtimeRaidDungeonSelected,
			"participants":               raidMemberIDsFromState(state.FrozenMembers),
		})
	}
	if kind == runtimeRaidDungeonCompleted {
		// Anton phase-one mechanisms react to the committed clear first so the
		// progress snapshot below already shows the gates and timers it opened.
		if err := s.afterRuntimeRaidObjectiveAdvance(event.RaidKey, event.Phase, event.MapID); err != nil {
			s.logGameEvent(session, "game-raid-anton-mechanism-arm-failed",
				"raid_key", event.RaidKey, "raid_map_id", event.MapID, "error", err)
		}
		if err := s.publishRuntimeRaidObjectiveProgress(event.RaidKey, event.Phase); err != nil {
			return err
		}
		// The clear registration is part of the roster record, so every member
		// needs a fresh member list before its 攻击队 window can show the gate as
		// cleared. Without this push the 地下城通关 column stays at 0 for the rest
		// of the phase, and the phase settlement then flips no cards.
		if state, ok := s.runtimeRaidForMember(event.CharacterID); ok {
			if err := s.broadcastRuntimeRaidPhaseClearRegistration(state); err != nil {
				return err
			}
			s.logGameEvent(session, "game-raid-phase-clear-registration",
				"raid_key", event.RaidKey,
				"raid_phase", event.Phase,
				"raid_map_id", event.MapID,
				"char_id", event.CharacterID,
				"member_clear_counts", formatRuntimeRaidClearCounts(state),
				"minimum_clear_count", runtimeRaidPhaseMinimalClearCount(state, event.Phase),
				"roster_refreshed", true)
		}
	}
	runtimeRaidDungeonMarkLifecycleNotified(binding, kind)
	s.logGameEvent(session, "game-raid-dungeon-lifecycle",
		"raid_key", event.RaidKey,
		"raid_phase", event.Phase,
		"raid_map_id", event.MapID,
		"dungeon_lifecycle_token", event.LifecycleToken,
		"char_id", event.CharacterID,
		"event", kind.String(),
		"global_raid_retained", true)
	return nil
}

// formatRuntimeRaidClearCounts renders the per-member phase clear registration
// of one raid for the packet log.
func formatRuntimeRaidClearCounts(state runtimeRaidState) string {
	if len(state.Lifecycle.PhaseClearCounts) == 0 {
		return ""
	}
	keys := make([]int, 0, len(state.Lifecycle.PhaseClearCounts))
	for charID := range state.Lifecycle.PhaseClearCounts {
		keys = append(keys, int(charID))
	}
	sort.Ints(keys)
	var builder strings.Builder
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.Itoa(key))
		builder.WriteByte(':')
		builder.WriteString(strconv.FormatUint(uint64(state.Lifecycle.PhaseClearCounts[uint16(key)]), 10))
	}
	return builder.String()
}

func (s *Service) publishRuntimeRaidDungeonParticipation(event runtimeRaidDungeonLifecycleEvent) error {
	state, ok := s.runtimeRaidForMember(event.CharacterID)
	if !ok || state.RaidKey != event.RaidKey || state.Lifecycle.Phase != event.Phase {
		return nil
	}
	// 超时空没有 584 参与行：它的战况表是 1452..1460 那一族，每一条副本生命周期
	// 边界的对外表示就是"按当前战场状态重发一遍差异行"。所以这里整条 584 路径
	// 直接绕开，改成发面板。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return s.broadcastRuntimeFiendWarBoard(state, false, time.Now().UTC())
	}
	member, ok := runtimeRaidFrozenMember(state, event.CharacterID)
	if !ok {
		return nil
	}
	// 584 carries one row per (dungeon, squad): the client draws the row inside the
	// dungeon node with that squad's member portraits, and the squad it belongs to
	// comes from each member's own roster byte (op 0x024F, which the client reads
	// straight -- an unassigned member's 0 renders as 单人).
	//
	// The row's CONTENT is therefore what says who is in it: a real squad's row lists
	// that squad and only that squad, and an unassigned member's row lists nobody but
	// itself. Folding the member onto squad 1 (runtimeRaidNormalizedGroup) is what put
	// squad 1's whole roster into the row an unassigned member opened, which is how it
	// was drawn as squad 1 in the 战况表 (live report 2026-09-17 21:5x). The row itself
	// stays: the member IS inside the dungeon, and the node has to show him.
	members := runtimeRaidGateRowMembers(state, event.CharacterID, member.GroupIndex)
	wireMapID := event.MapID
	if state.TailFlag == 1 {
		if selectorMapID, mapped := runtimeRaidSingleSelectorMapIDForState(state, event.MapID); mapped {
			wireMapID = selectorMapID
		}
	}

	ops := make([]uint32, 0, 2)
	switch event.Kind {
	case runtimeRaidDungeonSelected:
		// Selection only reserves the raid objective and group occupancy on the
		// server.  The current EXE's op584 handler (sub_1D438D0) treats the
		// participation operations as visible dungeon lifecycle changes; sending
		// op=0 from the op16 commit boundary announces entry before FINISH_LOADING,
		// then op=1 announces the same entry again after the scene is real.  Keep
		// the selected lifecycle flag for ordering/idempotency, but do not publish
		// participation until the client proves the dungeon scene loaded.
		return nil
	case runtimeRaidDungeonLoaded:
		// Zero first, then one: 86JP RaidHandler.Situation.cs:410-414 removes the
		// stale record immediately before every entry ("Remove a stale record
		// immediately before every entry"). The current client's op584 handler
		// (sub_1D438D0, jump table 0x1D43E80) maps op=0 to sub_17C0FB0, which
		// silently drops that dungeon's squad row, and op=1 to sub_17C0F20, which
		// inserts a row and pops 32048. Sending only op=1 leaves the previous row in
		// place, so re-entering the same gate stacks a second twin row -- the board
		// then draws dungeon_party1_%02d and dungeon_party2_%02d for one squad.
		ops = append(ops, 0)
		ops = append(ops, 1)
	case runtimeRaidDungeonAbandoned:
		ops = append(ops, 3)
	case runtimeRaidDungeonCompleted:
		// Every committed clear announces op=4, not just the clear that finishes a
		// gate's whole quota. The current EXE's op584 handler (sub_1D438D0) maps
		// op=4 to the chat line 32049 "其中1支战队已通关[%s]地下城。" and prints
		// nothing for op=0, so the old `visible` whitelist (which also omitted 213,
		// 215 and 220 -- the MaxHP=2/5 gates whose first clear leaves RemainingHP>0)
		// silently swallowed the first 擎天之柱 clear: live 2026-09-16T14:52:09Z and
		// 14:52:36Z, two-member raid 1507329, the squadmate's newest notice stayed
		// the older 黑雾之源 line and the 擎天之柱 line only appeared 10s later on the
		// second clear. 86JP RaidHandler.Progression.cs:122 sends op=4
		// unconditionally for every clear, before any per-dungeon branch.
		// op=0 goes with the clear notice, but strictly BEFORE op=4. The client's
		// op=0 target (sub_17C0FB0) erases that dungeon's squad row, which is what
		// the 黑雾之源 cutscene needs to run; leaving it after op=4 erased the row
		// the board had just been told about, so the cell went blank on every
		// clear. Dropping op=0 entirely (tried 2026-09-17T07:4xZ) also stopped the
		// squadmate's cutscene, which is worse. 86JP sends only op=4
		// (RaidHandler.Progression.cs:122), so this ordering is the closest match
		// that keeps the observed cutscene.
		ops = append(ops, 0)
		ops = append(ops, 4)
	default:
		return nil
	}

	for _, op := range ops {
		body, err := raid.BuildRaidParticipationBody([]raid.RaidParticipationRow{{
			DungeonID:  uint32(wireMapID),
			Op:         op,
			MemberKeys: runtimeRaidUint32MemberKeys(members),
		}})
		if err != nil {
			return err
		}
		if err := s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
			msgID: raid.RaidParticipationMsgID,
			body:  body,
		}); err != nil {
			return err
		}
	}
	// 参战行变一次，行数据跟着变一次。86JP 的时序不变量就是这条："每一次参战行变更之后
	// 都紧跟一条怪物状态"（Situation.cs:550/605，Progression.cs:830/1064/1122），而
	// tools\_rev_安徒恩_修复方案.md F2 记的正相反：我们的 634 只在 buff/用币/客户端刷新时
	// 才发。代价实测（2026-09-29，raid 1507329）：12:25:36 一队进 218 之后，门行数据
	// 要等到 12:31:01（另一队进图）才更新——5 分钟里那一行只有旧值/空值。
	//
	// 排进队列而不是就地广播：本函数在持有着进图会话 dungeon.mu 的情况下运行，而构行
	// 数据要读每个成员的会话锁（collectRuntimeRaidGatePlacements），就地扇出就是
	// 2026-09-20 08:32:02Z 那次死锁的形状（见
	// scheduleRuntimeRaidMonsterStateBroadcast 的注释）。
	s.scheduleRuntimeRaidMonsterStateBroadcast(
		state.RaidKey, "raid_participation_"+event.Kind.String())
	return nil
}

func runtimeRaidDungeonLifecycleAlreadyNotified(
	binding *runtimeRaidDungeonBinding,
	kind runtimeRaidDungeonLifecycleKind,
) bool {
	if binding == nil {
		return true
	}
	switch kind {
	case runtimeRaidDungeonSelected:
		return binding.selectedNotified
	case runtimeRaidDungeonLoaded:
		return binding.loadedNotified
	case runtimeRaidDungeonCompleted:
		return binding.completedNotified
	case runtimeRaidDungeonAbandoned:
		return binding.abandonedNotified
	default:
		return true
	}
}

func runtimeRaidDungeonMarkLifecycleNotified(
	binding *runtimeRaidDungeonBinding,
	kind runtimeRaidDungeonLifecycleKind,
) {
	if binding == nil {
		return
	}
	switch kind {
	case runtimeRaidDungeonSelected:
		binding.selectedNotified = true
	case runtimeRaidDungeonLoaded:
		binding.loadedNotified = true
	case runtimeRaidDungeonCompleted:
		binding.completedNotified = true
	case runtimeRaidDungeonAbandoned:
		binding.abandonedNotified = true
	}
}

func (kind runtimeRaidDungeonLifecycleKind) String() string {
	switch kind {
	case runtimeRaidDungeonSelected:
		return "selected"
	case runtimeRaidDungeonLoaded:
		return "loaded"
	case runtimeRaidDungeonCompleted:
		return "completed"
	case runtimeRaidDungeonAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
}
