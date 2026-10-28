package dnfbridge

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// Keep the later current-client dungeon selector and NPC-purchase call sites
// connected to the PVF-backed raid owner introduced by the extracted source.
var errAntonRaidEntryBlocked = errors.New("anton raid entry requires a started, paid raid roster and an open current-phase objective")

// errRuntimeRaidLukeRouteLocked 自成一个错误，这样"路线被锁"不会被报成通用的
// 编成/目标失败。原生客户端不会在另一条路线的门上画锁，被绑定的成员会一直点它；
// 用共用错误的话日志会写成"需要已开团、已付费的攻坚编成"，既是假的也没法定位。
//
// 移植自上游 raid_current_compat.go（bfb2098 / ede0bf5）。
var errRuntimeRaidLukeRouteLocked = errors.New("luke phase two member is bound to the other route")

// currentRaidLukeRouteLockedNoticeText 是客户端自己对这次拒绝的措辞，逐字取自
// 客户端字符串表里卢克攻坚那一段（docs/client-dstr-zh.txt, 67018；67013..67021
// 就是原生 루크 레이드 那一组）。"其他队伍已经入场或者归属于其他地下城" 正好就是
// 路线锁的含义，所以玩家看到的是游戏自己的句子，不是我们编的。
//
// 这条按服务端文本走：提示信封里带的是文本而不是 dstr id，措辞钉在上面那一条
// 才能保持原生。
const currentRaidLukeRouteLockedNoticeText = "其他队伍已经入场或者归属于其他地下城， 目前无法入场。"

func sessionClientPID(session *gameSession) uint32 {
	if session == nil {
		return 0
	}
	return session.clientPID
}

func isAntonRaidEntryDungeonID(dungeonID int64) bool {
	_, ok := runtimeRaidDungeonPhaseForMap(int(dungeonID))
	return ok
}

// The current EXE renders the single-Anton raid with its native raid selector
// slots, but submits the legacy Anton slot IDs (210-224) through the ordinary
// SelectDungeon command.  Those values are UI identities only in a started
// single raid: the dungeon runtime, PVF topology and settlement must continue
// to own the dedicated 5314-5322 maps.
func runtimeRaidSingleSelectorMapID(singleRaidType byte, raidMapID int) (int, bool) {
	switch raidMapID {
	case 5314:
		return 210, true // 黑雾之源
	case 5315:
		return 212, singleRaidType == 3 // 震颤的大地
	case 5316:
		return 216, singleRaidType == 6 // 舰炮防御战
	case 5317:
		return 213, singleRaidType == 0 // 擎天之柱
	case 5318:
		return 218, singleRaidType == 3 // 能量阻截战
	case 5321:
		return 218, singleRaidType == 6 // 能量阻截战
	case 5322:
		return 218, singleRaidType == 0 // 能量阻截战
	case 5319:
		return 219, true // 黑色火山
	case 5320:
		return 220, true // 安徒恩的心脏
	default:
		return 0, false
	}
}

func runtimeRaidSinglePVFMapID(singleRaidType byte, selectorMapID int) (int, bool) {
	for raidMapID := 5314; raidMapID <= 5322; raidMapID++ {
		mapped, ok := runtimeRaidSingleSelectorMapID(singleRaidType, raidMapID)
		if ok && mapped == selectorMapID {
			return raidMapID, true
		}
	}
	return 0, false
}

func runtimeRaidSingleSelectorMapIDForState(state runtimeRaidState, raidMapID int) (int, bool) {
	if state.RaidMode == runtimeRaidOwnerLuke {
		switch state.SingleRaidType {
		case runtimeRaidLukeLightRoute:
			return raidMapID, raidMapID >= 5323 && raidMapID <= 5328
		case runtimeRaidLukeDarkRoute:
			return raidMapID, raidMapID >= 5329 && raidMapID <= 5334
		default:
			return 0, false
		}
	}
	return runtimeRaidSingleSelectorMapID(state.SingleRaidType, raidMapID)
}

func runtimeRaidSinglePVFMapIDForState(state runtimeRaidState, selectorMapID int) (int, bool) {
	if state.RaidMode == runtimeRaidOwnerLuke {
		return runtimeRaidSingleSelectorMapIDForState(state, selectorMapID)
	}
	return runtimeRaidSinglePVFMapID(state.SingleRaidType, selectorMapID)
}

func (s *Service) resolveAntonRaidDungeonSelection(session *gameSession, requestedDungeonID int64) (int64, bool) {
	if s == nil || session == nil || runtimeRaidModeForSession(session) == 0 {
		return requestedDungeonID, false
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok || state.TailFlag != 1 {
		return requestedDungeonID, false
	}
	if state.RaidMode == runtimeRaidOwnerLuke {
		// Luke's current selector and PVF use the same native 5323..5334
		// identities, so no translation is required at this boundary.
		return requestedDungeonID, false
	}
	if requestedDungeonID >= 5314 && requestedDungeonID <= 5322 {
		return requestedDungeonID, false
	}
	raidMapID, ok := runtimeRaidSinglePVFMapID(state.SingleRaidType, int(requestedDungeonID))
	if !ok {
		return requestedDungeonID, false
	}
	return int64(raidMapID), true
}

func (s *Service) projectSingleAntonRaidClientDungeonID(session *gameSession, raidMapID int64) (int64, bool) {
	if s == nil || session == nil || runtimeRaidModeForSession(session) == 0 {
		return raidMapID, false
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok || state.TailFlag != 1 {
		return raidMapID, false
	}
	selectorMapID, ok := runtimeRaidSingleSelectorMapIDForState(state, int(raidMapID))
	if !ok {
		return raidMapID, false
	}
	return int64(selectorMapID), true
}

func (s *Service) validateAntonRaidDungeonEntry(session *gameSession, dungeonID int64) error {
	return s.validateAntonRaidDungeonEntryForEntry(session, dungeonID, false)
}

// validateAntonRaidDungeonEntryForEntry 是门禁的完整形式；partyFollower 表示
// "这一次进图是**服务端带着队友走**的"（prepareRuntimePartyDungeonEntry 在准备队友时
// 传的就是非 nil 的 plan），而不是这名成员自己按下的 op16。
//
// 它只对超时空有影响（见下面 FiendWar 分支里那段）；安东/卢克逐字不变。
func (s *Service) validateAntonRaidDungeonEntryForEntry(session *gameSession, dungeonID int64, partyFollower bool) error {
	if session == nil || runtimeRaidModeForSession(session) == 0 || !isAntonRaidEntryDungeonID(dungeonID) {
		return nil
	}
	phase, _ := runtimeRaidDungeonPhaseForMap(int(dungeonID))
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok || !state.Started || !state.EntryCostPaid || len(state.FrozenMembers) == 0 ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.Phase != phase {
		return errAntonRaidEntryBlocked
	}
	if objective, exists := state.Lifecycle.Objectives[int(dungeonID)]; !exists || objective.Phase != phase || objective.RemainingHP <= 0 || objective.State != raid.AntonDungeonOpen {
		return errAntonRaidEntryBlocked
	}
	if !runtimeRaidAntonEntryAllowedByBarrier(state, int(dungeonID)) {
		return errAntonRaidEntryBlocked
	}
	if _, member := runtimeRaidFrozenMember(state, selectedCharacterID(session)); !member {
		return errAntonRaidEntryBlocked
	}
	// 超时空额外要求：这个区域此刻确实是本成员所在的小队占着的，而且本人不在
	// 死亡禁锢的恢复期内。战场只认"小队占区域"，所以这条同时实现了 D2 的决定 ——
	// 未编队成员 (Seat.Group == 0) 的 MemberArea 落在等待区，永远对不上任何一个
	// 区域下标，于是被挡在门外，而不是靠 battle.Enter 事后报错兜住。
	//
	// 2026-10-01 真机（raid 12058625）赔过一次：这里原来只认**本人**的
	// MemberArea，于是"同小队、同一格一起进图"永远进不去第二个人，队伍建起来
	// 又被拆掉。链条是：
	//
	//	FiendWarBattle.MemberArea 优先取 MemberAreas[本人]，只有本人没有这一项
	//	时才回落到 GroupAreas[小队]；而 town_move.go 只为**移动的那个人**调
	//	moveRuntimeFiendWarGroup，那个函数顺手把其他队员的 MemberAreas 钉在他们
	//	当时站的格子上（raid_fiendwar_battle.go 的 pin 循环）。
	//
	// 于是驱动者按下进图 → 小队记进目标区域、驱动者本人也记进目标区域，
	// **同伴却还钉在"按下进图时站的那一格"**：MemberArea(同伴) = 原格 ≠ 目标区域
	// → 这条把同伴拒掉 → 同伴被移出临时队伍 → 掉到 1 人自动解散 → 驱动者一个人
	// 进图。日志原样：
	//
	//	game-party-dungeon-entry-blocked char_id=1 dungeon_id=4955
	//	  reason=party_entry_preflight_failed_before_any_member_commit
	//	  error=prepare party member 22 for dungeon 4955: anton raid entry requires
	//	        a started, paid raid roster and an open current-phase objective
	//	game-party-disbanded-by-single-member-left char_id=22 party_id=4 removed_id=1
	//
	// "本小队已经走进这一格"（GroupAreas[小队] == 目标区域）本身就是"这一格是本小队
	// 在打"的完整证据 —— 驱动者按下的那一次进图做的正是这件事，同伴按设计就留在
	// 原来的格子里等（见 fiendWarEncounterPlan：发起者进 Area，其余人留 OriginArea）。
	// 所以本人和本小队任一命中即放行。
	//
	// 这条放松**只对真正编了队的人生效**：runtimeRaidNormalizedGroup(0) 会把未编队
	// 成员折到 runtimeRaidDefaultGroup（1 队），拿它去查 GroupAreas 等于把"1 队在打的
	// 区域"送给一个没编制的人 —— 那正是 2026-09-17 真机那次事故的形状，也是 D2 明令
	// 要挡掉的东西。所以小队那一侧用 runtimeRaidMemberSquad（0 就是 0），不折。
	if state.Lifecycle.FiendWar != nil {
		member, _ := runtimeRaidFrozenMember(state, selectedCharacterID(session))
		area, found := state.Lifecycle.FiendWar.AreaForDungeon(int(dungeonID))
		if !found {
			return errAntonRaidEntryBlocked
		}
		// 复活禁锢与"谁驱动这一场"无关，对谁都必须成立。
		if s.gameplayNow().Before(state.Lifecycle.FiendWar.RecoveryUntil[member.CharID]) {
			return errAntonRaidEntryBlocked
		}
		// 上面那条"本人或本小队占着这个区域"只对**自己按 op16 进来**的人成立，
		// 对**被服务端带进来的队友**是错的 —— 而且它与规格 D 直接矛盾：
		//
		//	进图单位 = 区域里实时站着的所有人（≤4），**与小队编制无关**。
		//
		// 2026-10-01 22:48 真机（raid 12058625，分片 030）就是它挡的：操作员在攻坚队
		// 管理器把 char 22 编到了**2 队**（game-raid-manager-regrouped target_group=2，
		// game-raid-squad-parties-projected squads=1:2,2:3），两人同格 4，char 1 走进
		// 有怪的 7 触发进图：
		//
		//	MemberArea(22)      = 4  （他被钉在按下进图时站的格子，按设计就该留在那儿）
		//	GroupAreas[2]       = 4  （他**自己**的小队也在原格 —— 走的不是 char 1 的 1 队）
		//	目标区域             = 7
		//	→ errAntonRaidEntryBlocked
		//	→ "prepare party member 22 for dungeon 4955: anton raid entry requires a
		//	   started, paid raid roster and an open current-phase objective"
		//	→ 整队被拆（single-member-left）→ 触发者一个人进去（solo-fallback entered=true）
		//
		// [B] 那次放松（2026-09-19）只补上了"同伴与**驱动者同队**"这半边，所以两个人
		// 不同队时两条仍然全不成立 —— 而"不同队也要一起进"正是这条规格的目的。
		//
		// 队友这一侧的门禁由别处的、更强的证据负责，这里不再重复问一遍：
		//   * 他是本团的冻结成员、团已开始并付过入场费、阶段有效、这一格的目标是开着的
		//     —— 上面几条通用判定；
		//   * 这一场进图是**驱动者**驱动的那一支临时队伍（遭遇战按区域名单建的），
		//     而驱动者自己走的是同一道门禁、并且已经按"人在这一格"放行；
		//   * 他与驱动者**同城镇、同区域、同实例**
		//     —— prepareRuntimePartyDungeonEntry 的 currentPartyEntryCoPresence，
		//     也就是规格里的"这一格里实时站着的人"。
		//
		// 所以这里只对 partyFollower 放行**区域**这一条；未编队成员、非本团成员、
		// 复活禁锢、目标未开、团未开始等等一条都不放松（见用例里的两个反例）。
		if !partyFollower {
			memberArea := state.Lifecycle.FiendWar.MemberArea(
				member.CharID, runtimeRaidNormalizedGroup(member.GroupIndex))
			squadArea := 0
			if squad := runtimeRaidMemberSquad(member.GroupIndex); squad != 0 {
				squadArea = state.Lifecycle.FiendWar.GroupAreas[squad]
			}
			if memberArea != area.Index && squadArea != area.Index {
				return errAntonRaidEntryBlocked
			}
		}
	}
	// 卢克二阶段是两条互斥路线：已经绑到一条的成员不能穿到另一条。
	// 安徒恩（owner != Luke）短路，路径逐字不变。
	if runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke &&
		!runtimeRaidLukeRouteEntryAllowedLocked(&state, selectedCharacterID(session), int(dungeonID)) {
		s.logGameEvent(session, "game-raid-luke-route-entry-refused",
			"raid_key", state.RaidKey,
			"char_id", selectedCharacterID(session),
			"raid_map_id", dungeonID,
			"gate_route", runtimeRaidLukeGateRoute(int(dungeonID)),
			"member_route", state.Lifecycle.LukeRouteLocks[selectedCharacterID(session)])
		return errRuntimeRaidLukeRouteLocked
	}
	// Luke's [WEEKLY ENTER COUNT] cap. Off by default: runtimeRaidLukeWeeklyRefusal
	// short-circuits on the flag before it touches the repositories, so the
	// default path is byte-for-byte the old one.
	if runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke {
		if used, limit, refuse := s.runtimeRaidLukeWeeklyRefusal(selectedCharacterID(session)); refuse {
			s.logGameEvent(session, "game-raid-luke-weekly-entry-refused",
				"raid_key", state.RaidKey,
				"char_id", selectedCharacterID(session),
				"raid_map_id", dungeonID,
				"week_used", used,
				"week_limit", limit,
				"reason", "luke_weekly_enter_count_reached")
			return errAntonRaidEntryBlocked
		}
	}
	return nil
}

// A selector refresh replays the persistent raid object, allocation, map and
// timer state. op587 state=2 is the one-shot phase-start announcement and must
// not be replayed when the already-active selector is reopened. No
// participation or dungeon-change event is forged.
func (s *Service) sendRuntimeRaidSelectorSnapshot(session *gameSession, source string) error {
	if s == nil || session == nil || runtimeRaidModeForSession(session) == 0 {
		return nil
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok || !state.Started || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return nil
	}
	// 超时空的"重开选择器"就是把整张战况表按当前战场状态重放一遍 —— 它没有
	// 0x0247 对象也没有 0x0249 表，那两样是安徒恩/卢克的格式。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return s.sendRuntimeFiendWarBoard(session, state, false, time.Now().UTC())
	}
	objectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(state))
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidObjectMsgID, objectBody, 0, true); err != nil {
		return err
	}
	// Mode 0 recreates/selects the raid object, but the current EXE owns the
	// attack-party allocation panel in mode 3.  Reopening the selector with
	// only mode 0 clears that panel even though the raid object remains valid.
	refreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidMemberRefreshMsgID, refreshBody, 0, true); err != nil {
		return err
	}
	symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(state)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidSymbolMsgID, symbolBody, 0, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidDungeonStateMsgID, dungeonBody, 0, true); err != nil {
		return err
	}
	now := time.Now().UTC()
	remaining := runtimeRaidDurationSeconds(runtimeRaidPhaseSnapshotRemaining(state, now))
	duration := state.Lifecycle.PhaseDeadline.Sub(state.Lifecycle.PhaseStartedAt)
	if duration < 0 {
		duration = 0
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidTimerMsgID, raid.BuildRaidTimerBody(raid.RaidTimer{
		TimerType: 0,
		Value0:    runtimeRaidTopTimerKey,
		Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
		Value2:    runtimeRaidDurationSeconds(duration),
	}), 0, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidRemainTimeMsgID, raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
		TimerType: 0,
		Seconds:   remaining,
	}), 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-selector-snapshot-sent",
		"source", source,
		"raid_key", state.RaidKey,
		"phase", state.Lifecycle.Phase,
		"remaining_seconds", remaining,
		"reason", "persistent_raid_selector_state_replayed_without_one_shot_state2_or_fake_dungeon_entry")
	return nil
}

func (s *Service) sendRuntimeRaidDungeonParticipationOnFinishLoading(session *gameSession) error {
	if err := s.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		return err
	}
	// The gate is now real and loaded, which is the boundary Luke's weekly count
	// is observed at (raid_luke_weekly_observation.go). Observation only: it
	// never refuses, and every failure path inside it is silent, so it cannot
	// change whether a member enters.
	s.observeRuntimeRaidLukeWeeklyEntry(session, "raid_dungeon_loaded_after_finish_loading")
	// 超时空：场景真正加载完是 PVF [ON ENTER MAP] 的触发点 —— 雷达与电池的
	// 过场动画从这里开始。它自己按 FiendWar 频道与战场状态守门，
	// 安东/卢克调用它直接返回 nil。
	if err := s.notifyRuntimeFiendWarMapLoaded(session); err != nil {
		return err
	}
	return s.replayRuntimeRaidStateForRebuiltScene(session, "raid_dungeon_loaded_after_finish_loading")
}

// replayRuntimeRaidStateForRebuiltScene re-sends the authoritative raid tables a
// client needs after ITS OWN scene was rebuilt: the 0x024F membership roster (the
// squad byte per member) and the 0x025B participation rows (which squad stands in
// which gate).
//
// Both are pushed only when they CHANGE, and a client that was mid-transition at
// that moment drops them -- live 2026-09-29: `game-raid-peer-actor-deferred …
// reason=scene_between_scenes local_owner=0` is the same gap on the actor path.
// The 战况表 then has a row whose members it can no longer classify (the operator's
// "看不到是哪队进图 就显示个单 队长头像也是黑的"), and nothing ever repairs it,
// because the members are inside a gate and no party gesture can re-label them.
//
// Replaying on finish-loading is the cheapest self-heal: that is exactly when the
// client has just proved its scene exists again, and it makes a missed push
// recoverable instead of permanent.
//
// The participation half of that replay is now gated per client (see
// replayRuntimeRaidParticipationRows): it re-states only when the client has not
// already been told this same (raid, phase, gate-row-set), because op=1 pops 32048
// and finish-loading fires once per ROOM inside a gate. The roster half below keeps
// running unconditionally -- 0x024F prints nothing, and its squad byte is half of
// what makes a board row resolvable at all.
func (s *Service) replayRuntimeRaidStateForRebuiltScene(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || !state.Started || len(state.Members) == 0 {
		return nil
	}
	if err := s.broadcastRuntimeRaidRefresh(state); err != nil {
		s.logGameEvent(session, "game-raid-scene-replay-refresh-failed",
			"source", source,
			"raid_key", state.RaidKey,
			"error", err)
		return nil
	}
	if err := s.replayRuntimeRaidParticipationRows(state, source); err != nil {
		s.logGameEvent(session, "game-raid-scene-replay-participation-failed",
			"source", source,
			"raid_key", state.RaidKey,
			"error", err)
		return nil
	}
	s.logGameEvent(session, "game-raid-scene-replay-sent",
		"source", source,
		"raid_key", state.RaidKey,
		"raid_phase", state.Lifecycle.Phase,
		"member_count", len(state.Members),
		"reason", "scene_was_rebuilt_so_the_roster_is_replayed_and_the_gate_rows_are_replayed_when_they_changed")
	return nil
}

// currentRaidParticipationReplayRow is one (gate, squad) row the rebuilt-scene
// replay re-states: the same "who stands in which gate" pair the entry published.
type currentRaidParticipationReplayRow struct {
	gate    uint32
	members []uint16
}

// currentRaidParticipationReplayLedger identifies the exact participation row set
// one client was last re-stated with, so the replay can tell "this client's board
// already says this" from "this client's board has to be told". It is comparable,
// which is all the idempotency check needs.
//
// phase is part of the identity on purpose: a phase change (587 / 0x024B) makes the
// client drop and re-draw its row table, so a ledger that only looked at the row
// bytes could call the re-statement redundant right after it. raidKey is there for
// the same reason on the raid boundary -- the next raid must be replayed even if
// its rows happen to encode identically.
type currentRaidParticipationReplayLedger struct {
	raidKey uint32
	phase   byte
	rows    string
}

// runtimeRaidParticipationReplayRows collects the (gate, squad) rows the
// rebuilt-scene replay would state right now, read from the live run bindings
// only. An empty result means no squad of this raid is standing in a gate.
func (s *Service) runtimeRaidParticipationReplayRows(state runtimeRaidState) []currentRaidParticipationReplayRow {
	if s == nil {
		return nil
	}
	rows := make([]currentRaidParticipationReplayRow, 0, runtimeRaidMaxGroups)
	for _, group := range runtimeRaidPartyGroups(state) {
		if len(group) == 0 {
			continue
		}
		members := runtimeRaidMembersInSquadLiveRows(state, runtimeRaidMemberSquad(group[0].GroupIndex))
		if len(members) == 0 {
			continue
		}
		placement, placed := s.runtimeRaidSquadGatePlacement(group)
		if !placed {
			continue
		}
		rows = append(rows, currentRaidParticipationReplayRow{
			gate:    uint32(placement),
			members: members,
		})
	}
	return rows
}

// currentRaidParticipationReplayRowsKey renders the row set as one canonical,
// comparable string. Squads arrive in squad order (runtimeRaidPartyGroups) and
// members in member order (runtimeRaidMembersInSquadLiveRows), so the same board
// state always renders the same key. It is also what the log prints, which is why
// it stays readable instead of a hash.
func currentRaidParticipationReplayRowsKey(rows []currentRaidParticipationReplayRow) string {
	if len(rows) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, row := range rows {
		builder.WriteString(strconv.FormatUint(uint64(row.gate), 10))
		builder.WriteByte(':')
		for index, member := range row.members {
			if index > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(strconv.FormatUint(uint64(member), 10))
		}
		builder.WriteByte(';')
	}
	return builder.String()
}

// currentRaidParticipationReplayMatches reports whether this client's 战况表 was
// already brought to exactly this row set. Nothing else writes the ledger: the
// entry publish does NOT, because a push that went out while this client was
// between scenes is precisely the one it can have dropped -- only the replay runs
// after the client proved its scene exists again, so only the replay may claim the
// client has the rows.
func (session *gameSession) currentRaidParticipationReplayMatches(ledger currentRaidParticipationReplayLedger) bool {
	if session == nil {
		return false
	}
	session.raidParticipationReplayMu.Lock()
	defer session.raidParticipationReplayMu.Unlock()
	return session.raidParticipationReplay == ledger
}

func (session *gameSession) rememberCurrentRaidParticipationReplay(ledger currentRaidParticipationReplayLedger) {
	if session == nil {
		return
	}
	session.raidParticipationReplayMu.Lock()
	defer session.raidParticipationReplayMu.Unlock()
	session.raidParticipationReplay = ledger
}

// replayRuntimeRaidParticipationRows re-states, for every squad that is standing
// in a gate right now, the same op=0/op=2 row pair the entry published -- SILENTLY.
// It never invents a row: it reads the live run bindings.
//
// It is PER RECIPIENT IDEMPOTENT, and that is the whole point of the ledger. The
// client's op=0 branch (sub_17C0FB0) deletes that dungeon's squad row silently, and
// op=2 (sub_17C0F20) inserts it -- also without text. This replay runs on every
// finish-loading, and inside a gate the client finishes loading once per ROOM, so
// 服主 saw the announcement once per room (2026-09-29, 卢克/安徒恩). The row set is
// the right identity to dedupe on: changing rooms inside one gate does not change it
// (nothing to re-state), while entering or leaving a gate does -- and that boundary
// is exactly where the client can drop the row, because its scene is being rebuilt
// when the entry publish goes out.
//
// The op pair is 0,2 and NOT 0,1: op=1 is the one branch that also pops 32048
// 「其中1支战队已进入[%s]地下城。」, and this replay is a re-statement, not a second
// announcement -- the entry publish already made the announcement. See the op table
// at the send site for the disassembly and the packet-log measurement.
//
// A client whose ledger is stale keeps the heal: a reconnecting or newly joined
// session has no ledger at all, and any change of gate re-arms every member's.
func (s *Service) replayRuntimeRaidParticipationRows(state runtimeRaidState, source string) error {
	recipients := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	if len(recipients) == 0 {
		return nil
	}
	runtimeRaidSortMembers(recipients)
	rows := s.runtimeRaidParticipationReplayRows(state)
	if len(rows) == 0 {
		// Nobody of this raid is standing in a gate, so the board these clients hold
		// carries no row at all. Forget the ledger instead of keeping the last one:
		// the op=0/op=3/op=4 that emptied those rows are what removed them from the
		// client, and the squad that walks into a gate later has to be replayed even
		// if it walks into the same gate it left.
		forgotten := 0
		for _, member := range recipients {
			session, online := s.onlineGameSession(member.CharID)
			if !online || session == nil {
				continue
			}
			session.rememberCurrentRaidParticipationReplay(currentRaidParticipationReplayLedger{})
			forgotten++
		}
		s.logPacketEvent("game-raid-participation-replay-skipped",
			"raid_key", state.RaidKey,
			"raid_phase", int(state.Lifecycle.Phase),
			"gate_rows", 0,
			"recipients", forgotten,
			"source", source,
			"reason", "no_squad_of_this_raid_is_standing_in_a_gate")
		return nil
	}
	ledger := currentRaidParticipationReplayLedger{
		raidKey: state.RaidKey,
		phase:   state.Lifecycle.Phase,
		rows:    currentRaidParticipationReplayRowsKey(rows),
	}
	packets := 0
	recipientsSent, recipientsSkipped := 0, 0
	for _, member := range recipients {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		if session.currentRaidParticipationReplayMatches(ledger) {
			recipientsSkipped++
			continue
		}
		for _, row := range rows {
			// 0 then 2, NOT 0 then 1.
			//
			// The op table of the 584 handler sub_1D438D0 (jump table 0x1D43E80, dumped
			// instruction by instruction in tools\_sit_584_tail.txt) is:
			//
			//	op=0 -> 0x01D43BEA: call sub_17C0FB0(manager, dungeonID), jmp 0x1D43D00 -- no text
			//	op=1 -> 0x01D43B7A: mov esi,0x7D30; call sub_17C0F20; jmp 0x1D43C25 -- prints 32048
			//	op=2 -> 0x01D43B44: call sub_17C0F20;          jmp 0x1D43D00 -- NO text
			//
			// The op=2 branch is the op=1 branch minus `esi` and minus the print tail: same
			// argument setup (the member vector built by 0x47DA30, the dungeonID, the raid
			// manager) and the same sub_17C0F20 roster insert. So op=2 is "re-state the row
			// silently", which is exactly what a replay is -- and it is what the reference
			// server sends: 86JP RaidHandler.Situation.cs:443-462 refeshes with {0,2}.
			//
			// Using op=1 here made every gate entry announce itself a SECOND time. The entry
			// publish (raid_dungeon_lifecycle.go, runtimeRaidDungeonLoaded: ops 0,1) already
			// popped 32048 「其中1支战队已进入[%s]地下城。」 for the members that received it,
			// and this replay deliberately does not trust that publish -- the ledger is only
			// written here (see the file header) -- so the first finish-loading inside a gate
			// always replayed. Measured on the decrypted packet log, raid 1835009,
			// 2026-09-30 23:54:31 (entering 5060 智慧之祭坛), conn game-000218:
			//
			//	pkt 3995 op=0 / 3996 op=1   <- entry publish
			//	pkt 4000 op=0 / 4001 op=1   <- this replay  => 32048 twice
			//
			// and that is the operator's report 「有时候进图 这个提示会刷两条」 -- "sometimes",
			// because a client whose scene is still being rebuilt can drop the first pair.
			// op=2 keeps the self-heal (the roster row still lands) and drops the duplicate.
			for _, op := range []uint32{0, 2} {
				body, err := raid.BuildRaidParticipationBody([]raid.RaidParticipationRow{{
					DungeonID:  row.gate,
					Op:         op,
					MemberKeys: runtimeRaidUint32MemberKeys(row.members),
				}})
				if err != nil {
					return err
				}
				if err := s.sendGameUpperRawClassCodec(
					session,
					raid.RaidParticipationMsgID,
					body,
					0,
					true,
				); err != nil {
					return err
				}
				packets++
			}
		}
		// The ledger is committed only after the whole row set went out: a send that
		// failed halfway must be retried by the next finish-loading, not remembered
		// as delivered.
		session.rememberCurrentRaidParticipationReplay(ledger)
		recipientsSent++
	}
	if recipientsSent > 0 {
		s.logPacketEvent("game-raid-participation-replay-sent",
			"raid_key", state.RaidKey,
			"raid_phase", int(state.Lifecycle.Phase),
			"rows", packets,
			"gate_rows", len(rows),
			"recipients", recipientsSent,
			"skipped_recipients", recipientsSkipped,
			"source", source,
			"reason", "rebuilt_scene_needs_the_current_gate_rows_again")
		return nil
	}
	if recipientsSkipped > 0 {
		s.logPacketEvent("game-raid-participation-replay-skipped",
			"raid_key", state.RaidKey,
			"raid_phase", int(state.Lifecycle.Phase),
			"gate_rows", len(rows),
			"gate_rows_key", ledger.rows,
			"recipients", recipientsSkipped,
			"source", source,
			"reason", "the_scene_rebuild_is_inside_the_gate_these_rows_already_describe")
	}
	return nil
}

// runtimeRaidSquadGatePlacement reports the gate one squad currently stands in, or
// 0 when none of its members holds a live raid run.
func (s *Service) runtimeRaidSquadGatePlacement(group []runtimeRaidMemberState) (int, bool) {
	if s == nil || len(group) == 0 {
		return 0, false
	}
	for _, member := range group {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		inside := runtime != nil && dungeonRuntimeOwnsCharacter(runtime, member.CharID) &&
			runtime.raidDungeon.RaidKey != 0
		mapID := 0
		if inside {
			mapID = runtime.raidDungeon.MapID
		}
		session.dungeon.mu.Unlock()
		if inside && mapID != 0 {
			return mapID, true
		}
	}
	return 0, false
}

func (s *Service) refreshRuntimeRaidEntryCostAfterPurchase(session *gameSession, itemID uint32) error {
	if s == nil || session == nil {
		return nil
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok || state.Started {
		return nil
	}
	entryItemID, _ := runtimeRaidEntryCostForState(state)
	if itemID != entryItemID {
		return nil
	}
	if err := s.sendRuntimeRaidEntryCostSnapshot(session, state); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-entry-cost-refreshed-after-purchase",
		"raid_key", state.RaidKey,
		"item_id", itemID,
		"member_count", len(state.Members))
	return nil
}
