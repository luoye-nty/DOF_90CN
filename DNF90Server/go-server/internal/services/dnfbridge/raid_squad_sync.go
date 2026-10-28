package dnfbridge

// Attack-party squads and ordinary four-member parties are two different
// objects. When members of one attack party form a real ordinary party while
// the squads are still editable, the raid roster follows: those members are
// projected into one squad so the attack-group window agrees with the party
// they actually fight in. The projection is one-way. The ordinary party is
// never rewritten from the raid, and an ordinary party that dissolves leaves
// the squad labels untouched (the native grid has no unassigned row).
//
// Hypothesis source (never authority): the read-only 86JP C# server,
// RaidHandler.cs HandleNormalPartyFormedAsync / TrySynchronizeNormalPartySquadLocked.

// syncRuntimeRaidSquadsWithOrdinaryParty is called after every ordinary-party
// snapshot publication with that party's member ids. It reports whether the
// raid roster changed.
func (s *Service) syncRuntimeRaidSquadsWithOrdinaryParty(memberIDs []uint16) bool {
	return s.syncRuntimeRaidSquadsWithOrdinaryPartyLedBy(memberIDs, 0)
}

// syncRuntimeRaidSquadsWithOrdinaryPartyLedBy is the same sync with the ordinary
// party's own leader, which is what decides whose squad number the party keeps
// ("队长则保持队伍号不变").
func (s *Service) syncRuntimeRaidSquadsWithOrdinaryPartyLedBy(memberIDs []uint16, leaderID uint16) bool {
	if s == nil || len(memberIDs) < 2 {
		return false
	}
	// A party cannot even be formed while its members are inside dungeon runs, so a
	// party snapshot that names members currently inside a raid gate is not a real
	// party gesture: those snapshots are republished by the dungeon-entry and
	// loading-barrier machinery itself. Following one re-labelled members who never
	// stood together in town. Live 2026-09-19, raid 1507338: 14:24:22 and 14:35:10
	// folded two members who were fighting inside 218 into squad 1, and 14:38:05
	// pulled all four of them into one squad -- the operator's "那两个人还在副本里
	// 也没有组队的条件啊".
	if inside := s.raidMembersInsideDungeonRuns(memberIDs); len(inside) > 0 {
		s.logPacketEvent("game-raid-squad-sync-skipped-members-inside-dungeon",
			"members", memberIDs,
			"inside_members", inside,
			"reason", "an_ordinary_party_cannot_be_formed_from_inside_a_dungeon_run")
		return false
	}
	s.raidMu.Lock()
	var state *runtimeRaidState
	inRaid := make([]uint16, 0, len(memberIDs))
	for _, id := range memberIDs {
		candidate := s.runtimeRaidByMemberLocked(id)
		if candidate == nil {
			continue
		}
		if state == nil {
			state = candidate
		}
		if candidate != state {
			// Members of two different attack parties formed one ordinary party:
			// neither raid may claim the other's roster.
			s.raidMu.Unlock()
			return false
		}
		inRaid = append(inRaid, id)
	}
	if state == nil || len(inRaid) < 2 || !runtimeRaidSquadsFollowOrdinaryParty(*state) {
		s.raidMu.Unlock()
		return false
	}
	target, ok := runtimeRaidSquadForOrdinaryPartyLedBy(*state, inRaid, leaderID)
	if !ok {
		s.raidMu.Unlock()
		return false
	}
	changed := false
	for _, id := range inRaid {
		index := runtimeRaidMemberIndex(*state, id)
		if index < 0 || state.Members[index].GroupIndex == target {
			continue
		}
		state.Members[index].GroupIndex = target
		state.Members[index].SlotOrder = nextRuntimeRaidSlotExcluding(*state, target, id)
		if state.Started {
			for i := range state.FrozenMembers {
				if state.FrozenMembers[i].CharID == id {
					state.FrozenMembers[i].GroupIndex = target
					state.FrozenMembers[i].SlotOrder = state.Members[index].SlotOrder
				}
			}
		}
		changed = true
	}
	if !changed {
		s.raidMu.Unlock()
		return false
	}
	runtimeRaidSortMembers(state.Members)
	if state.Started {
		runtimeRaidSortMembers(state.FrozenMembers)
	}
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	if err := s.broadcastRuntimeRaidRefresh(snapshot); err != nil {
		s.logPacketEvent("game-raid-squad-sync-refresh-failed", "raid_key", snapshot.RaidKey, "error", err)
	}
	if err := s.broadcastRuntimeRaidBuffRegistration(snapshot, 0); err != nil {
		s.logPacketEvent("game-raid-squad-sync-buff-failed", "raid_key", snapshot.RaidKey, "error", err)
	}
	s.logPacketEvent("game-raid-squad-synced-from-ordinary-party",
		"raid_key", snapshot.RaidKey, "squad", target, "members", inRaid)
	return true
}

// raidMembersInsideDungeonRuns reports which of the given characters currently hold
// a committed raid dungeon run, i.e. are standing inside a raid gate of the running
// raid. A member in town holds no runtime at all, so this is exactly the "cannot be
// forming a party right now" set.
func (s *Service) raidMembersInsideDungeonRuns(characterIDs []uint16) []uint16 {
	return s.charactersInsideDungeonRuns(characterIDs, true)
}

// charactersInsideDungeonRuns reports which of the given characters are standing
// inside one of their own dungeon runs right now. raidOnly narrows that to the
// attack-party gates, which is the question the squad bookkeeping asks; the party
// gestures ask the wider question because 86JP refuses them for any run at all
// (Player.CurrentRun != null).
func (s *Service) charactersInsideDungeonRuns(characterIDs []uint16, raidOnly bool) []uint16 {
	if s == nil || len(characterIDs) == 0 {
		return nil
	}
	inside := make([]uint16, 0, len(characterIDs))
	for _, characterID := range characterIDs {
		if characterID == 0 {
			continue
		}
		session, ok := s.onlineGameSession(characterID)
		if !ok || session == nil {
			continue
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		inRun := runtime != nil && dungeonRuntimeOwnsCharacter(runtime, characterID)
		if inRun && raidOnly {
			inRun = runtime.raidDungeon.RaidKey != 0
		}
		session.dungeon.mu.Unlock()
		if inRun {
			inside = append(inside, characterID)
		}
	}
	return inside
}

// partyGestureInsideDungeonRun answers whether an ordinary-party gesture between
// the named characters has to be refused because one of them is inside a dungeon
// run, and names the first such character.
//
// 86JP refuses every party formation path while either side holds
// Player.CurrentRun or the manager reports the party dungeon-active: REQUEST_PEER
// (PartyHandler.cs:1633-1644, including the trade variant at :1543-1556),
// RES_PEER for an application (:1921-1929) and for an invitation (:1998-2009,
// re-checked after the party object is created at :2034-2045), and CHANGE_HOST
// (:1439-1445). Trade invites are refused with the same gate. The refusal travels
// as a peer-command failure carrying error code 0x04 (BuildPeerCommandFailureBody),
// which this server already answers with as sendPartyCommandFailure.
//
// This is the second half of the "4 个人突然组队到一起了" report: the raid roster no
// longer follows a party formed from inside a run (see
// syncRuntimeRaidSquadsWithOrdinaryParty), and a party cannot be formed from inside
// one in the first place.
func (s *Service) partyGestureInsideDungeonRun(characterIDs ...uint16) (uint16, bool) {
	inside := s.charactersInsideDungeonRuns(characterIDs, false)
	if len(inside) == 0 {
		return 0, false
	}
	return inside[0], true
}

// runtimeRaidSquadLabelTargets selects which of the two raid roster lists a
// squad-label clear rewrites. The lists are NOT interchangeable:
//
//   - state.Members is the LIVE roster. The client's 攻击队 window is built from
//     it (runtimeRaidObjectMode0 / runtimeRaidMemberRefresh), runtimeRaidPartyGroups
//     projects an ordinary party from it, and runtimeRaidFirstAvailablePosition
//     counts its occupied grid slots.
//   - state.FrozenMembers is the phase roster snapshot taken at start
//     (runtimeRaidStartReservation). It owns the phase-clear registration --
//     runtimeRaidNotePhaseClearLocked credits EVERY frozen member of the clearing
//     member's squad -- plus the 战况表 gate rows (runtimeRaidMembersInSquad) and
//     the settlement board split (runtimeRaidRewardSquadBodies).
//
// A member dropped from the frozen roster therefore stops being credited for his
// squad's clears, and "no phase clear" is the same fact as "no phase reward"
// (runtimeRaidRewardEligibleMembersLocked). That coupling is why the two scopes
// exist instead of one.
type runtimeRaidSquadLabelTargets int

const (
	// runtimeRaidSquadLabelBothRows is the ordinary party leave and the kick: the
	// member is out of the party for good, so both rows follow.
	runtimeRaidSquadLabelBothRows runtimeRaidSquadLabelTargets = iota
	// runtimeRaidSquadLabelLiveRowOnly is the solo town return. Nothing about it
	// says the member stopped contributing to the phase he is still a member of,
	// so only the row the 攻击队 window draws is rewritten and the frozen roster
	// (credits, 战况表 rows, reward board) stays exactly as it was.
	runtimeRaidSquadLabelLiveRowOnly
)

// unassignRuntimeRaidSquadOnPartyLeave clears one member's squad label when that
// member leaves the ordinary party.
//
// The squad -> party projection above is deliberately one-way: a raid never
// rewrites somebody's party. The reverse gesture is not symmetric, though. A
// member who walks out of the party is no longer fighting in that squad, and
// leaving the label behind keeps the 攻击队 window claiming a squad the player is
// not in -- the client draws the row from the roster it was last sent, and no
// later push corrects it. Live report 2026-09-17: "攻坚开始后 已经有队伍编制的人
// 退出组队应该同时退出组队编制".
//
// 86JP says the same thing: RaidHandler.HandleNormalPartyLeftAsync assigns the
// leaver to party index 0 and rebroadcasts the raid object, the member list and
// the monster status, logging "RAID_PARTY_UNASSIGN" (ServerS4A21-master,
// RaidHandler.cs:251-266). That helper is never called anywhere in that tree, so
// it is a design statement rather than verified traffic; the gate used here is
// this service's own (runtimeRaidSquadsFollowOrdinaryParty), which keeps the
// ready countdown, the settlement and a single raid out of it.
func (s *Service) unassignRuntimeRaidSquadOnPartyLeave(characterID uint16) bool {
	if s == nil || characterID == 0 {
		return false
	}
	// A member who is inside a raid gate is not walking out of anything: the party
	// object is being republished by the entry and loading-barrier machinery, not by
	// a player gesture. Their run is keyed by the seat it was opened under, so
	// stripping the label mid-run orphans that seat's occupancy and can drop the
	// clear they are about to report. 86JP refuses the same move while a member
	// holds a participation (RaidManager.cs:315-321). Live 2026-09-19, raid
	// 1507338: 14:24:07 and 14:24:16 unassigned two members who were fighting inside
	// 218 at that very second.
	if len(s.raidMembersInsideDungeonRuns([]uint16{characterID})) > 0 {
		s.logPacketEvent("game-raid-squad-unassign-skipped-inside-dungeon",
			"char_id", characterID,
			"reason", "a_member_inside_a_dungeon_run_is_not_leaving_the_party")
		return false
	}
	return s.clearRuntimeRaidSquadLabel(
		characterID,
		runtimeRaidSquadLabelBothRows,
		"game-raid-squad-unassigned-on-party-leave",
		"game-raid-squad-unassign",
		"party_leave",
	)
}

// unassignRuntimeRaidSquadOnSoloTownReturn clears the squad label of a member who
// walked home ALONE out of a dungeon run, because that gesture is already a party
// leave (「只要是自己单独回城了 视作直接退出队伍」, see
// party_solo_return_leave.go and party_forced_exit_leave.go) and the label is
// derived from that party.
//
// Operator report 2026-09-26: "小队成员在城镇退队 或者被踢出 是回到单人的（没编制）
// 但是在副本中直接返回城镇…却还在小队编制里", including the leader: "队长自己单独
// 返回也要清 相当于队长自己退队了 按现有逻辑 队长顺延". Live raid 1507329, party 5 =
// [1,2,20,21] all in squad 1: chars 20/21/2 each pressed 返回城镇 inside gate 219
// (14:05:18/33/36Z) and every one of them kept GroupIndex 1 until char 20 happened
// to be KICKED in town 1m53s later
// (game-raid-squad-unassigned-on-party-leave char_id=20 previous_group=1).
//
// Two deliberate differences from the party-leave variant above:
//
//  1. No "inside a dungeon run" guard. This caller IS the town-return route, and
//     at that point in it the returning session still owns its gate runtime --
//     sendCurrentFullDungeonReturnToTown detaches the party at :1291-1353 and only
//     retires the runtime at :1693 -- so the guard would turn every one of these
//     into a no-op. What the guard protects against (a party snapshot republished
//     by the entry/barrier machinery, not a player gesture) cannot reach here: the
//     call site is inside the branch that has already decided this member leaves
//     the party, and a resumed return is classified exitClassSquadKept.
//  2. Live row only. See runtimeRaidSquadLabelTargets: the frozen roster is what
//     credits the member for his squad's clears and what the phase reward is
//     built from, and the member is still in the raid and can still re-enter.
func (s *Service) unassignRuntimeRaidSquadOnSoloTownReturn(characterID uint16, source string) bool {
	if s == nil || characterID == 0 {
		return false
	}
	return s.clearRuntimeRaidSquadLabel(
		characterID,
		runtimeRaidSquadLabelLiveRowOnly,
		"game-raid-squad-unassigned-on-solo-town-return",
		"game-raid-squad-town-return-unassign",
		source,
	)
}

// currentDungeonTownReturnClearsRaidSquad answers the one question the town-return
// route has to ask before it clears a member's squad label: is this the gesture the
// operator's rule is about?
//
// His words, 2026-10-01, verbatim: 「在战况表里撤退 应该保持编制 如果是队内成员单独
// 自己回城 应该取消这个人的编制 差不多是这么个逻辑」. Two gestures, two outcomes:
//
//   - 战况表撤退 (leader-led whole-squad return, op72{1,2} ->
//     leadRuntimePartyHomeFromActiveRun -> sendCurrentActiveDungeonReturnToTownKeepingParty),
//     a raid objective closing its gates, a resumed return: all of those are
//     exitClassSquadKept (runtime.townReturnKeepParty / townReturnPartyDetached,
//     see currentDungeonTownExitClass) and never reach the clear at all.
//   - 单独自己回城, i.e. the member's own 返回城镇 inside the dungeon
//     (op42 handleDungeonGiveupGame / op132 handleDungeonBackToVillage), which is
//     already a party leave and therefore takes the label with it.
//
// The Fiend War returns the SERVER drives are neither of those, and they used to
// fall into the "leaves the party" class and clear the label:
//
//   - runtime.townReturnKeepsSquadLabel: the failure tail
//     (returnRuntimeFiendWarDungeons with ended=true, which must keep every squad
//     label by ruling 「攻坚队不应该被解散」) and a cleared area's auto-return;
//   - the death recovery (runtime.deathReturnWaiting): prepareRuntimeFiendWarRetreat
//     puts the member back in the waiting room and imprisons him for the PVF's
//     DIE IMPRISON TIME. He was killed, he did not walk home, and the battle model
//     still has him in his squad -- so the label stays and the next area he walks
//     into after the imprisonment still resolves a squad.
//
// Scope: the Fiend War clauses are gated on isCurrentFiendWarRaidChannel, and only
// Fiend War sets townReturnKeepsSquadLabel, so Anton and Luke keep their existing
// (unchanged) semantics byte for byte.
func (s *Service) currentDungeonTownReturnClearsRaidSquad(
	session *gameSession,
	class currentDungeonTownExitClass,
	fiendWarSquadLabelKept bool,
) bool {
	if class == exitClassSquadKept {
		return false
	}
	if !isCurrentFiendWarRaidChannel(session) {
		return true
	}
	return !fiendWarSquadLabelKept
}

// unassignRuntimeRaidSquadOnSessionLeave clears the squad label of a member
// whose CONNECTION went away -- closing the game, returning to the character
// select screen, a dropped transport, or a replacement session taking the
// character over (another client, a channel move).
//
// Operator rule (2026-09-27): 「无论是否主动退队 只要游戏关闭了或者小退到选择角色
// 界面了 一律按退队处理」, and for the raid 「攻坚队被顶号 掉线 本来就要掉编制」. All
// of those paths detach the ordinary party through detachRuntimePartySession, and
// the squad label is derived from that party, so it has to follow. Without this
// the roster kept claiming a squad for a member who is no longer in any party,
// and a reconnect inside the five-minute grace window (raid_disconnect.go) came
// back still wearing it.
//
// Two deliberate differences from unassignRuntimeRaidSquadOnPartyLeave:
//
//  1. No "inside a dungeon run" guard. That guard protects a player gesture that
//     is not a leave -- a party snapshot republished by the entry/barrier
//     machinery -- but this caller IS the disconnect. The departing session's
//     gate run is released a few statements later in the same teardown
//     (noteRuntimeRaidSessionDisconnected -> releaseRuntimeRaidRunsOwnedByLocked,
//     "Releasing the run here is what keeps a client crash from squatting on a
//     gate forever"), so at this point the guard would turn every raid
//     disconnect into a no-op -- which is precisely the defect being fixed.
//  2. Live row only, like the solo town return above: the member is still in the
//     raid and holds a reconnect reservation, so the frozen phase roster keeps
//     crediting him. The expiry of that reservation removes him from the raid
//     outright, frozen row included.
func (s *Service) unassignRuntimeRaidSquadOnSessionLeave(characterID uint16, source string) bool {
	if s == nil || characterID == 0 {
		return false
	}
	return s.clearRuntimeRaidSquadLabel(
		characterID,
		runtimeRaidSquadLabelLiveRowOnly,
		"game-raid-squad-unassigned-on-session-leave",
		"game-raid-squad-session-leave-unassign",
		source,
	)
}

// clearRuntimeRaidSquadLabel is the shared body of the clears above.
func (s *Service) clearRuntimeRaidSquadLabel(
	characterID uint16,
	targets runtimeRaidSquadLabelTargets,
	event string,
	failureEvent string,
	source string,
) bool {
	if s == nil || characterID == 0 {
		return false
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || !runtimeRaidSquadsFollowOrdinaryParty(*state) {
		s.raidMu.Unlock()
		return false
	}
	index := runtimeRaidMemberIndex(*state, characterID)
	if index < 0 || state.Members[index].GroupIndex == 0 {
		// Already standby: nothing to correct, and no packet is worth sending.
		s.raidMu.Unlock()
		return false
	}
	previousGroup := state.Members[index].GroupIndex
	previousSlot := state.Members[index].SlotOrder
	state.Members[index].GroupIndex = 0
	state.Members[index].SlotOrder = 0
	// The member was its squad's leader: the squad is now led by whoever the squad's
	// ordinary party hands it to (or by its lowest remaining slot), so drop the stale
	// name here instead of leaving the roster claiming a member who is not in it --
	// runtimeRaidMemberLeadsSquad ignores such a row, and the next party publish (or
	// the lowest-slot fallback) answers for the squad from then on.
	if squad := runtimeRaidMemberSquad(previousGroup); squad != 0 && state.SquadPartyLeaders[squad] == characterID {
		delete(state.SquadPartyLeaders, squad)
	}
	if targets == runtimeRaidSquadLabelBothRows && state.Started {
		for i := range state.FrozenMembers {
			if state.FrozenMembers[i].CharID == characterID {
				state.FrozenMembers[i].GroupIndex = 0
				state.FrozenMembers[i].SlotOrder = 0
			}
		}
	}
	runtimeRaidSortMembers(state.Members)
	if targets == runtimeRaidSquadLabelBothRows && state.Started {
		runtimeRaidSortMembers(state.FrozenMembers)
	}
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	// The mode0 object is what the current client materializes the attack-party
	// panel from and mode3 is the roster row (see the assignGroup branch in
	// handleOnlineRaidManagerWork); both have to follow the label. The buff
	// registration follows the same publish so the party buff rows agree.
	if err := s.broadcastRuntimeRaidCreate(snapshot); err != nil {
		s.logPacketEvent(failureEvent+"-create-failed", "raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	if err := s.broadcastRuntimeRaidRefresh(snapshot); err != nil {
		s.logPacketEvent(failureEvent+"-refresh-failed", "raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	if err := s.broadcastRuntimeRaidBuffRegistration(snapshot, 0); err != nil {
		s.logPacketEvent(failureEvent+"-buff-failed", "raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	s.logPacketEvent(event,
		"raid_key", snapshot.RaidKey,
		"char_id", characterID,
		"previous_group", previousGroup,
		"previous_slot", previousSlot,
		"group", 0,
		"slot", 0,
		"frozen_row", func() string {
			if targets == runtimeRaidSquadLabelBothRows {
				return "cleared_with_live_row"
			}
			return "kept_for_phase_credit"
		}(),
		"source", source)
	return true
}

// runtimeRaidSquadsFollowOrdinaryParty reports whether an ordinary party formed by
// raid members may drag the raid roster into one squad.
//
// runtimeRaidGroupsEditable answers a different question: whether the raid
// window's own squad controls work. Those freeze once the raid starts, but an
// ordinary party is a separate gesture -- after start it is the ONLY way to pull a
// member into your squad, because the raid window no longer offers the move. Live
// report 2026-09-16: the leader invited a member into the party while the raid was
// running, the party formed, and the invited member stayed a solo row in the raid
// roster.
//
// Excluded on purpose: a single raid never owns a squad, the three-second
// preparation window has a frozen roster, and a settlement in progress has already
// frozen the reward roster.
func runtimeRaidSquadsFollowOrdinaryParty(state runtimeRaidState) bool {
	// 超时空：进副本时组的是"这个区域里的人"（临时普通队伍），而**编制不变**是硬约束
	// （操作员 2026-10-01：「相当于组个临时队伍 但是攻坚小队编制不变」）。
	//
	// 所以超时空的普通队伍快照**不许**反过来改攻坚编制：跨编制的区域队伍一发布，
	// 这条同步就会把 1 队 + 2 队并成 1 队 —— 实测（raid_fiendwar_area_entry_test.go）
	// 进图后 live 编制从 1/2 变成 1/1，玩家在攻坚队窗口里能直接看见编制被并了。
	//
	// 安东/卢克逐字不变：那边"开团之后唯一能并队的途径"就是这条同步。
	if state.Lifecycle.FiendWar != nil {
		return false
	}
	if state.StartPending || state.TailFlag == 1 {
		return false
	}
	if !state.Started {
		return true
	}
	switch state.Lifecycle.Status {
	case runtimeRaidLifecyclePhaseActive, runtimeRaidLifecycleStandby:
		return true
	default:
		return false
	}
}

// runtimeRaidSquadForOrdinaryParty picks the squad that will hold the whole
// ordinary party.
//
// The operator's rule (2026-09-20), verbatim: "单人和单人的组队 应该找一个最小的
// 数字组成新的小队 比如1 2队都被占用 就自动组成3队 … 1 2 3小队都被占用 2队解散了
// 原2队成员全部变单人 此时 3队不要动 还是3队 … 再重新组队还是从2队开始".
// So a squad belongs to exactly ONE ordinary party: a new party takes the SMALLEST
// squad number that no outsider occupies, a party that already holds a squad keeps
// it, and a squad whose members all became solo frees its number for the next party.
//
// 86JP decides the same way: ResolveNormalPartySquadIndex (RaidHandler.cs:1206-1237)
// collects the PartyIndex of every raid member OUTSIDE the party, prefers an index one
// of the party's own members already sits in when no outsider is there, and otherwise
// returns the lowest index that no outsider occupies.
//
// Backfilling a squad that still has a free slot is what produced the operator's
// "多队伍成员错误显示为同属一支队伍" / "开始攻坚后 单人和单人组队会跳到1队". Live
// 2026-09-20T10:56:55Z, raid 1507338:
//
//	game-raid-squad-synced-from-ordinary-party raid_key=1507338 squad=1 members=[10 12]
//
// while 13 and 9 -- two members of a DIFFERENT ordinary party -- were already sitting
// in squad 1 from the 10:56:51 sync of [13 9]. The roster really did say GroupIndex=1
// for both parties at once.
//
// An unassigned member (GroupIndex 0) occupies nothing here: runtimeRaidMemberSquad is
// the accessor for what a member actually is (see its own note), while
// runtimeRaidNormalizedGroup folds 0 onto squad 1 for keys and presentation only --
// which is the second reason every fresh pairing used to land in squad 1.
func runtimeRaidSquadForOrdinaryParty(state runtimeRaidState, memberIDs []uint16) (byte, bool) {
	return runtimeRaidSquadForOrdinaryPartyLedBy(state, memberIDs, 0)
}

// runtimeRaidSquadForOrdinaryPartyLedBy adds the one case the plain rule got wrong:
// a party that already holds a squad KEEPS that squad, its leader's first.
//
// Operator's report, 2026-09-20: "从1队踢了个人 然后由2队队长邀请进队 进了2队 然后2队踢出
// 这个人 2队变3队了 这种情况应保持2队不变". Live raid 1507329:
//
//	13:12:02 game-raid-squad-synced-from-ordinary-party squad=2 members=[21 22 20]
//	13:12:09 game-raid-squad-synced-from-ordinary-party squad=3 members=[21 22]   ← 2 became 3
//	13:12:09 game-raid-squad-unassigned-on-party-leave char_id=20 previous_group=2  ← the kick
//
// The kicked member's label is cleared by unassignRuntimeRaidSquadOnPartyLeave, but the
// party snapshot is published first, so at sync time squad 2 still named him as an
// "outsider" and the whole party was relocated to the lowest squad nobody occupied.
// Outsiders labelled in the party's OWN squad must not veto it: they are leavers whose
// label is cleared in the same gesture, and moving the party is exactly the complaint.
// A squad the party does not hold is still never backfilled, so the "多队伍成员错误显示
// 为同属一支队伍" fix stands: two solos pairing up still take the lowest free number.
func runtimeRaidSquadForOrdinaryPartyLedBy(
	state runtimeRaidState,
	memberIDs []uint16,
	leaderID uint16,
) (byte, bool) {
	// 一小队能站几个人由团决定：超时空是 1（每队后面三格是锁住的），别的团还是 4。
	// 于是超时空里两个人在镇上组队，会被分到**两个不同的编制**，而不是挤进同一队。
	size := runtimeRaidGroupSizeFor(state)
	maxGroups := runtimeRaidGroupMaxFor(state)
	if len(memberIDs) == 0 || len(memberIDs) > size {
		return 0, false
	}
	partyMember := make(map[uint16]bool, len(memberIDs))
	for _, id := range memberIDs {
		partyMember[id] = true
	}
	occupiedOutside := make(map[byte]bool, maxGroups)
	outsiders := make(map[byte]int, maxGroups)
	heldByParty := make(map[byte]bool, maxGroups)
	leaderSquad := byte(0)
	for _, member := range state.Members {
		squad := runtimeRaidMemberSquad(member.GroupIndex)
		if squad == 0 {
			continue
		}
		if partyMember[member.CharID] {
			heldByParty[squad] = true
			if leaderID != 0 && member.CharID == leaderID {
				leaderSquad = squad
			}
			continue
		}
		occupiedOutside[squad] = true
		outsiders[squad]++
	}
	// The party keeps its own squad: the leader's number first ("队长则保持队伍号不变"),
	// then the lowest squad any of its members already holds. It must still FIT there
	// (outsiders + the whole party within the squad size); a party that cannot fit moves
	// out rather than overflowing a squad.
	fits := func(group byte) bool {
		return heldByParty[group] && outsiders[group]+len(memberIDs) <= size
	}
	if leaderSquad != 0 && fits(leaderSquad) {
		return leaderSquad, true
	}
	for group := byte(1); group <= maxGroups; group++ {
		if fits(group) {
			return group, true
		}
	}
	// A fresh party takes the smallest squad number no outsider occupies -- "1 2 队都被
	// 占用 就自动组成3队", and a number freed by a disbanded squad is reused first.
	for group := byte(1); group <= maxGroups; group++ {
		if !occupiedOutside[group] {
			return group, true
		}
	}
	return 0, false
}
