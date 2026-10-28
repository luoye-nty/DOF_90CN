package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// The raid-team skill panel (攻坚队技能 / 小队技能) is one five-slot panel:
//
//	slots 0/1/2 = "小队技能" -- they need a designated squad, PVF target [PARTY]
//	slots 3/4   = "攻坚队"   -- raid-wide,                PVF target [RAID]
//
// The client hard-codes that split at 0x015E27CF (`cmp dword ptr [esi+0x1C8],3`
// then `jl`: below three it first makes the player pick a member target, at three
// and above it sends 0x02B6 straight away). The on-screen labels in the live
// client agree, and so does etc/raid/anton.etc.buff, which lists exactly three
// [PARTY] entries and two [RAID] ones.
//
// C2S 0x02B6 (CmdPacketRaidBuffSystem) is the activation request. Its only sender
// in the whole image is 0x010D7BB6 inside sub_10D7A00, and its body is a fixed
// five bytes: u8 category, u16 target, u16 extra. `选取技能(DEL)` is NOT that
// sender -- it is input action 0x12, which only switches the panel's state.
// See tools/_probe_694_report.md.

// Client raid-buff categories. These are the bytes the client sends in 0x02B6 and
// the key bytes it looks up in 0x0279. They are NOT the panel slot order: the
// client maps slot -> category through the jump table at 0x10D7C90, whose five
// targets yield exactly {2,3,4,1,0} -- the same array 86JP calls
// AntonClientBuffTypes (RaidHandler.Situation.cs:260) and the same one this server
// already uses for the 0x0279 group order (runtimeRaidClientBuffSlots).
const (
	runtimeRaidBuffIncreaseTime  byte = 0
	runtimeRaidBuffAttackBonus   byte = 1
	runtimeRaidBuffInvincible    byte = 2
	runtimeRaidBuffRestore       byte = 3
	runtimeRaidBuffIncreaseCoin  byte = 4
	runtimeRaidBuffCategoryCount      = 5
)

// runtimeRaidBuffDefinition is one [BUFF] entry of etc/raid/anton.etc.buff.
//
// The values are the shipped PVF's, read from
// D:\马维斯0705\etc\raid\anton.etc.buff (the same file this client's PVF holds), and
// they match 86JP's hardcoded fallback in AntonRaidRewardProvider.cs:290-312 entry
// for entry. They are constants rather than PVF reads because this server does not
// parse anton.etc.buff yet; wiring that up is a follow-up, and until it exists a
// constant table that says where it came from beats silently reporting zero
// cooldowns.
type runtimeRaidBuffDefinition struct {
	Category byte
	// RaidWide is the PVF target: [RAID] applies to every member, [PARTY] applies
	// to the one designated squad.
	RaidWide     bool
	CooldownSecs uint32
	DurationSecs uint32
	// EffectValue is the PVF's third number. Only the two skills that have a
	// server-side meaning consume it -- INCREASE TIME adds it to the phase
	// deadline, INCREASE COIN adds it to the squad's granted coins. The three
	// remaining skills' values (15 / 0 / 7) are display-only: 86JP never applied
	// them server-side either, and its client applied nothing itself
	// (tools/_probe_279_report.md section 5), so the panel state is the whole
	// effect there.
	EffectValue uint32
}

var runtimeRaidBuffDefinitions = [runtimeRaidBuffCategoryCount]runtimeRaidBuffDefinition{
	runtimeRaidBuffIncreaseTime: {Category: runtimeRaidBuffIncreaseTime, RaidWide: true, CooldownSecs: 7200, DurationSecs: 10, EffectValue: 300},
	runtimeRaidBuffAttackBonus:  {Category: runtimeRaidBuffAttackBonus, RaidWide: true, CooldownSecs: 420, DurationSecs: 30, EffectValue: 15},
	runtimeRaidBuffInvincible:   {Category: runtimeRaidBuffInvincible, RaidWide: false, CooldownSecs: 360, DurationSecs: 30, EffectValue: 0},
	runtimeRaidBuffRestore:      {Category: runtimeRaidBuffRestore, RaidWide: false, CooldownSecs: 240, DurationSecs: 60, EffectValue: 7},
	runtimeRaidBuffIncreaseCoin: {Category: runtimeRaidBuffIncreaseCoin, RaidWide: false, CooldownSecs: 300, DurationSecs: 10, EffectValue: 4},
}

func runtimeRaidBuffDefinitionFor(category byte) (runtimeRaidBuffDefinition, bool) {
	if category >= runtimeRaidBuffCategoryCount {
		return runtimeRaidBuffDefinition{}, false
	}
	definition := runtimeRaidBuffDefinitions[category]
	return definition, definition.CooldownSecs > 0
}

// runtimeRaidBuffRequest is a decoded C2S 0x02B6.
type runtimeRaidBuffRequest struct {
	Category byte
	// TargetKey is the roster member the panel had selected, as a character key.
	// It is NOT a squad number: the client resolves it through the roster
	// (sub_17B87F0) and a roster entry carries the member's name, and it writes it
	// with WriteU16 over a value whose -1 sentinel is what blocks the send.
	TargetKey uint16
	// ExtraKey is the body's third field. It comes from the same roster entry as
	// the send gate, and its meaning is still unproven, so it is carried and logged
	// but never acted on.
	ExtraKey uint16
}

// decodeRuntimeRaidBuffRequest reads one C2S 0x02B6.
//
// The shape checks are 86JP's (RaidHandler.Situation.cs:330-347) and they still
// describe this client, whose sender writes exactly one u16 target: length >= 5,
// an even (length-3), and a category inside 0..4. Unlike 86JP this does not reject
// a zero target key at decode time -- whether a target is required depends on the
// definition (raid-wide skills ignore it), so that decision belongs to the handler
// rather than to the shape.
func decodeRuntimeRaidBuffRequest(body []byte) (runtimeRaidBuffRequest, error) {
	if len(body) < 5 {
		return runtimeRaidBuffRequest{}, fmt.Errorf("raid buff request too short: len=%d", len(body))
	}
	if (len(body)-3)%2 != 0 {
		return runtimeRaidBuffRequest{}, fmt.Errorf("raid buff request has a truncated target list: len=%d", len(body))
	}
	if body[0] >= runtimeRaidBuffCategoryCount {
		return runtimeRaidBuffRequest{}, fmt.Errorf("raid buff category out of range: %d", body[0])
	}
	if targets := (len(body) - 3) / 2; targets > runtimeRaidMaxGroups*2 {
		return runtimeRaidBuffRequest{}, fmt.Errorf("raid buff request names %d targets", targets)
	}
	return runtimeRaidBuffRequest{
		Category:  body[0],
		TargetKey: uint16(body[1]) | uint16(body[2])<<8,
		// The live body is exactly five bytes, so the third field sits at 3..4 and is
		// always present on any shape this client produced. Anything past it would be
		// a second target id, which 86JP accepted but this client never writes.
		ExtraKey: uint16(body[3]) | uint16(body[4])<<8,
	}, nil
}

// runtimeRaidBuffActivationKey names one activation slot. Keyed by the raid and
// the client category, which is 86JP's rule too (RaidHandler.Situation.cs:84) and
// means one activation per skill per raid, raid-wide: not per squad, and not per
// leader.
type runtimeRaidBuffActivationKey struct {
	RaidKey  uint32
	Category byte
}

// runtimeRaidBuffActivation is one running skill. Both deadlines are absolute Unix
// seconds, because that is what 0x0279 carries and what the client compares against
// its own clock (0x017B5800 / 0x017B5812).
type runtimeRaidBuffActivation struct {
	PartyIndex    uint16
	MemberKey     uint16
	ActiveUntil   uint32
	CooldownUntil uint32
}

// runtimeRaidBuffActivationFor returns one raid's live activation for a category.
// Expired cooldowns are pruned on read, the same lazy rule 86JP uses
// (RaidHandler.Situation.cs:293-296): there is no expiry timer anywhere, and a
// stale entry is indistinguishable from no entry once its cooldown has passed.
func (s *Service) runtimeRaidBuffActivationFor(raidKey uint32, category byte) (runtimeRaidBuffActivation, bool) {
	if s == nil || raidKey == 0 {
		return runtimeRaidBuffActivation{}, false
	}
	key := runtimeRaidBuffActivationKey{RaidKey: raidKey, Category: category}
	now := uint32(time.Now().Unix())
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	activation, ok := s.raidBuffActivations[key]
	if !ok {
		return runtimeRaidBuffActivation{}, false
	}
	if activation.CooldownUntil <= now {
		delete(s.raidBuffActivations, key)
		return runtimeRaidBuffActivation{}, false
	}
	return activation, true
}

// recordRuntimeRaidBuffActivation stores one activation. A category already inside
// its cooldown is refused, which is what makes the client's own 「技能冷却中」
// (string 30311) line truthful: the client blocks its send from the 0x0279 deadlines
// it was last given, and this is the same rule on the authoritative side.
//
// Cooldowns outlive a phase on purpose -- 86JP never clears them on a phase
// transition either (its ResetRaidPhasePresentationState omits _raidBuffActivations)
// -- but they cannot outlive the raid.
func (s *Service) recordRuntimeRaidBuffActivation(
	raidKey uint32,
	category byte,
	definition runtimeRaidBuffDefinition,
	partyIndex uint16,
	memberKey uint16,
) (runtimeRaidBuffActivation, bool) {
	if s == nil || raidKey == 0 {
		return runtimeRaidBuffActivation{}, false
	}
	key := runtimeRaidBuffActivationKey{RaidKey: raidKey, Category: category}
	now := uint32(time.Now().Unix())
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	if current, ok := s.raidBuffActivations[key]; ok && current.CooldownUntil > now {
		return runtimeRaidBuffActivation{}, false
	}
	activation := runtimeRaidBuffActivation{
		PartyIndex:    partyIndex,
		MemberKey:     memberKey,
		ActiveUntil:   now + definition.DurationSecs,
		CooldownUntil: now + definition.CooldownSecs,
	}
	if s.raidBuffActivations == nil {
		s.raidBuffActivations = make(map[runtimeRaidBuffActivationKey]runtimeRaidBuffActivation)
	}
	s.raidBuffActivations[key] = activation
	return activation, true
}

// purgeRuntimeRaidBuffActivationsLocked drops every activation of one raid. Caller
// holds s.raidMu.
func (s *Service) purgeRuntimeRaidBuffActivationsLocked(raidKey uint32) {
	if s == nil || raidKey == 0 {
		return
	}
	for key := range s.raidBuffActivations {
		if key.RaidKey == raidKey {
			delete(s.raidBuffActivations, key)
		}
	}
}

// runtimeRaidBuffStatusBody builds the 0x0279 body: the five slots in the client's
// own slot order, each with the raid's live row (or no row at all).
//
// A slot with no rows is what an idle skill looks like, and a zero-row group is
// inert rather than harmful: the client's lookup skips the key entirely
// (0x017B57E2) and falls out of the clean false return at 0x017B5869. So this body
// is byte-identical to the all-empty registration body this server used before the
// feature existed, until something is actually activated.
func (s *Service) runtimeRaidBuffStatusBody(state runtimeRaidState) ([]byte, error) {
	groups := make([]raid.RaidBuffGroup, 0, len(runtimeRaidClientBuffSlots))
	now := uint32(time.Now().Unix())
	for _, category := range runtimeRaidClientBuffSlots {
		group := raid.RaidBuffGroup{Field0: category}
		if s != nil && state.RaidKey != 0 {
			key := runtimeRaidBuffActivationKey{RaidKey: state.RaidKey, Category: category}
			s.raidMu.Lock()
			activation, ok := s.raidBuffActivations[key]
			if ok && activation.CooldownUntil <= now {
				delete(s.raidBuffActivations, key)
				ok = false
			}
			s.raidMu.Unlock()
			if ok {
				group.Entries = []raid.RaidBuffEntry{{
					PartyIndex: activation.PartyIndex,
					MemberKey:  activation.MemberKey,
					Value0:     activation.ActiveUntil,
					Value1:     activation.CooldownUntil,
				}}
			}
		}
		groups = append(groups, group)
	}
	return raid.BuildRaidBuffBody(groups)
}

// broadcastRuntimeRaidBuffStatus publishes the 0x0279 status to the whole raid.
func (s *Service) broadcastRuntimeRaidBuffStatus(state runtimeRaidState) error {
	body, err := s.runtimeRaidBuffStatusBody(state)
	if err != nil {
		return err
	}
	return s.fanOutRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
		msgID: raid.RaidBuffMsgID,
		body:  body,
	})
}

// sendRuntimeRaidBuffStatus answers one session with the 0x0279 status. Every
// refused activation uses this, which is 86JP's shape
// (RaidHandler.Situation.cs:423-426): the panel is driven by that packet, so a
// rejection has to refresh it or the client keeps showing a cooldown the server no
// longer believes in.
func (s *Service) sendRuntimeRaidBuffStatus(session *gameSession, state runtimeRaidState) error {
	if s == nil || session == nil {
		return nil
	}
	body, err := s.runtimeRaidBuffStatusBody(state)
	if err != nil {
		return err
	}
	return s.sendGameUpperRawClassCodec(session, raid.RaidBuffMsgID, body, 0, true)
}

// runtimeRaidBuffLeaderAuthorized reports whether this member may use the panel.
//
// 86JP's CanUseAntonRaidBuff (RaidHandler.Situation.cs:170-178) is exactly this:
// the raid is running and the caller is its leader. Nothing else in the raid is
// leader-gated, so it is one small named rule rather than an inline check.
func runtimeRaidBuffLeaderAuthorized(state runtimeRaidState, memberKey uint16) bool {
	return memberKey != 0 &&
		state.LeaderID == memberKey &&
		runtimeRaidBuffPhaseRunning(state)
}

// runtimeRaidBuffPhaseRunning reports "the raid's attack is in progress", which is
// 86JP's `raid.State == 2`.
func runtimeRaidBuffPhaseRunning(state runtimeRaidState) bool {
	return state.Started && state.Lifecycle.Status == runtimeRaidLifecyclePhaseActive
}

// runtimeRaidBuffTargetParty resolves the activation row's target from the member
// the panel had selected.
//
// The client sends a MEMBER key, not a squad number, so unlike 86JP
// (Situation.cs:374-378, which matches `group.PartyIndex == requestedPartyIndex`)
// the squad has to be looked up FROM the member. A squad-designated skill also
// requires that member to be standing in a gate, which is the same rule the client
// states in string 30313 「只能指定已进入地下城的小队。」 and the same one 86JP
// enforces through `group.DungeonId != 0` (Situation.cs:388-393).
func (s *Service) runtimeRaidBuffTargetParty(
	state runtimeRaidState,
	targetKey uint16,
) (uint16, bool) {
	if targetKey == 0 {
		return 0, false
	}
	member, ok := runtimeRaidFrozenMember(state, targetKey)
	if !ok {
		return 0, false
	}
	placements := s.collectRuntimeRaidGatePlacements(state)
	if _, placed := placements[targetKey]; !placed {
		return 0, false
	}
	return uint16(runtimeRaidMemberSquad(member.GroupIndex)), true
}

// runtimeRaidBuffEffect is what an activated skill has to announce beyond its row.
type runtimeRaidBuffEffect struct {
	// ExtendedTime is set by INCREASE TIME: the phase deadline moved, so the raid
	// needs the new 0x0254/0x0255 pair.
	ExtendedTime bool
	Deadline     time.Time
	Remaining    uint32
	// CoinGranted is set by INCREASE COIN: the squad's reported spend changed, so
	// the battle-situation rows need re-publishing.
	CoinGranted bool
	// State is the raid snapshot the effect was applied to, used to address the
	// announcement.
	State runtimeRaidState
}

// applyRuntimeRaidBuffEffect performs the server-side effect of one skill.
//
// Only two skills have one, and both are deliberately narrow, matching 86JP:
//
//   - INCREASE TIME adds the PVF effect value (300s) to the phase deadline. It is
//     CAPPED AT ONE FULL PHASE, refused outright once the phase has expired, and
//     truncated when less room is left, which is 86JP's TryExtendPhaseTime
//     (RaidManager.cs:1884-1936: `availableRoom = max - currentRemaining`, false
//     when the remainder is 0, `applied = min(additional, availableRoom)`).
//   - INCREASE COIN does not hand out coins: the client's denominator is ITS OWN
//     per-gate allowance and never comes off the wire, so the only lever the server
//     has is the numerator. 86JP does the same thing -- it raises the squad's
//     `GrantedCoinCount` and reports `used - granted` -- which makes the client's
//     own `allowance - reported` come out four higher. The grant is capped by the
//     squad's ACTUAL over-spend, so a squad that has not spent beyond its grant gets
//     nothing and the skill does not burn its cooldown.
//
// The three remaining skills return applied=true with no effect, because that is
// what they are: panel state.
func (s *Service) applyRuntimeRaidBuffEffect(
	state runtimeRaidState,
	definition runtimeRaidBuffDefinition,
	targetKey uint16,
) (*runtimeRaidBuffEffect, bool, string) {
	switch definition.Category {
	case runtimeRaidBuffIncreaseTime:
		return s.applyRuntimeRaidBuffIncreaseTime(state, definition)
	case runtimeRaidBuffIncreaseCoin:
		return s.applyRuntimeRaidBuffIncreaseCoin(state, definition, targetKey)
	default:
		return nil, true, "panel_state_only"
	}
}

func (s *Service) applyRuntimeRaidBuffIncreaseTime(
	state runtimeRaidState,
	definition runtimeRaidBuffDefinition,
) (*runtimeRaidBuffEffect, bool, string) {
	if definition.EffectValue == 0 {
		return nil, false, "increase_time_effect_value_is_zero"
	}
	if !runtimeRaidBuffPhaseRunning(state) || state.Lifecycle.Phase == 0 {
		return nil, false, "increase_time_needs_a_running_phase"
	}
	_, baseSeconds, ok := runtimeRaidPhaseConfig(state, state.Lifecycle.Phase)
	if !ok || baseSeconds <= 0 {
		return nil, false, "increase_time_has_no_phase_config"
	}
	// The phase math uses the same clock as the timer that will be re-armed below.
	// Reading a different clock here would let the deadline move while the delay
	// computed for the new job was based on another "now".
	now := s.gameplayNow()
	remaining := runtimeRaidPhaseRemaining(state, now)
	if remaining <= 0 {
		// An expired phase must not be revived: its timeout has already been
		// scheduled and the raid is on its way to the failure result.
		return nil, false, "increase_time_phase_already_expired"
	}
	maxTotal := time.Duration(baseSeconds) * time.Second
	room := maxTotal - remaining
	if room <= 0 {
		return nil, false, "increase_time_phase_timer_is_already_full"
	}
	applied := time.Duration(definition.EffectValue) * time.Second
	if applied > room {
		applied = room
	}
	if applied <= 0 {
		return nil, false, "increase_time_has_no_room"
	}
	deadline := state.Lifecycle.PhaseDeadline.Add(applied)
	updated, err := s.setRuntimeRaidPhaseDeadline(state.RaidKey, state.Lifecycle.Phase, deadline)
	if err != nil {
		return nil, false, "increase_time_deadline_update_failed:" + err.Error()
	}
	// Re-arm the phase expiry under the SAME timer name, which is what replaces the
	// old deadline: leaving the previous job armed would fail the phase at the old
	// instant even though the deadline moved.
	if err := s.scheduleRuntimeRaidPhaseExpiry(updated); err != nil {
		return nil, false, "increase_time_reschedule_failed:" + err.Error()
	}
	return &runtimeRaidBuffEffect{
		ExtendedTime: true,
		Deadline:     deadline,
		Remaining:    runtimeRaidDurationSeconds(runtimeRaidPhaseRemaining(updated, now)),
		State:        updated,
	}, true, "phase_deadline_extended"
}

func (s *Service) applyRuntimeRaidBuffIncreaseCoin(
	state runtimeRaidState,
	definition runtimeRaidBuffDefinition,
	targetKey uint16,
) (*runtimeRaidBuffEffect, bool, string) {
	if definition.EffectValue == 0 {
		return nil, false, "increase_coin_effect_value_is_zero"
	}
	placements := s.collectRuntimeRaidGatePlacements(state)
	placement, placed := placements[targetKey]
	if !placed {
		return nil, false, "increase_coin_target_is_not_inside_a_gate"
	}
	identity, ok := runtimeRaidMonsterStateRowIdentityForMember(state, targetKey)
	if !ok {
		return nil, false, "increase_coin_target_has_no_row"
	}
	key := runtimeRaidReviveCoinKey{
		RaidKey:    state.RaidKey,
		Phase:      state.Lifecycle.Phase,
		Group:      identity.Group,
		SoloCharID: identity.SoloCharID,
		DungeonID:  placement.DungeonID,
	}
	used := s.runtimeRaidReviveCoinUses(key)
	granted := s.runtimeRaidReviveCoinGranted(key)
	overUse := uint32(0)
	if used > granted {
		overUse = used - granted
	}
	if overUse == 0 {
		// Nothing to cover: 86JP refuses here too (`appliedCount == 0` ->
		// TryGrantAdditionalCoinUses returns false, RaidManager.cs:561-563), so the
		// cooldown is not consumed for a squad that has spent nothing extra.
		return nil, false, "increase_coin_squad_has_not_overspent"
	}
	applied := definition.EffectValue
	if applied > overUse {
		applied = overUse
	}
	s.grantRuntimeRaidReviveCoins(key, applied)
	return &runtimeRaidBuffEffect{CoinGranted: true, State: state}, true, "revive_coin_grant_recorded"
}

// publishRuntimeRaidBuffEffect announces what an effect changed.
func (s *Service) publishRuntimeRaidBuffEffect(state runtimeRaidState, effect runtimeRaidBuffEffect) error {
	if effect.ExtendedTime {
		// 86JP's order for the time extension: 0x0254 then 0x0255
		// (RaidHandler.Situation.cs:151-152).
		deadline := effect.Deadline
		duration := deadline.Sub(state.Lifecycle.PhaseStartedAt)
		if duration < 0 {
			duration = 0
		}
		return s.fanOutRuntimeRaidWireSequence(effect.State,
			runtimeRaidWireNotification{
				msgID: raid.RaidTimerMsgID,
				body: raid.BuildRaidTimerBody(raid.RaidTimer{
					TimerType: 0,
					Value0:    runtimeRaidTopTimerKey,
					Value1:    uint32(deadline.Unix()),
					Value2:    runtimeRaidDurationSeconds(duration),
				}),
			},
			runtimeRaidWireNotification{
				msgID: raid.RaidRemainTimeMsgID,
				body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
					TimerType: 0,
					Seconds:   effect.Remaining,
				}),
			})
	}
	if effect.CoinGranted {
		// The squad's reported spend changed, so its battle-situation row has to be
		// re-published (86JP broadcasts its 0x027B on the coin grant too,
		// RaidHandler.Situation.cs:157).
		return s.broadcastRuntimeRaidMonsterState(effect.State)
	}
	return nil
}

// handleOnlineRaidBuffSystem serves C2S 0x02B6.
//
// THERE IS DELIBERATELY NO REPLY ON THIS OPCODE, and that is not an oversight:
//
//   - 86JP answered with `cmd=1, type=0x02B6, body={0|1}`
//     (RaidHandler.cs:1935-1938), but its client had no inbound 0x02B6 handler at
//     all, so a one-byte body was free.
//   - THIS client does. It registers 0x02B6 inbound at 0x016281BA (handler
//     0x01623CD0), and that handler reads FOUR u32s off the inbound cursor
//     (0x01623D03 `mov edi,4` + 0x01623D09 `call 0x34E3A50`). The read primitives
//     perform no bounds check (tools/_probe_694_report.md section 5), so echoing
//     86JP's one-byte ack would make the client read fifteen bytes past the body --
//     the shape of bug that surfaces as a random crash much later.
//   - The panel does not need it. The five icons are driven entirely by 0x0279: the
//     lookup sub_17B5730 reads the row deadlines and sub_15C6AD0 builds the icon
//     from them. Publishing the new 0x0279 status IS the confirmation.
//
// So every path answers with 0x0279 and nothing else, including the refusal paths,
// whose resync is 86JP's own shape (Situation.cs:39-40, 59-60, 68-69, 79-80).
// Nothing here may fail the caller: the opcode sits on the client's input path.
func (s *Service) handleOnlineRaidBuffSystem(session *gameSession, typ uint16, body []byte) (bool, error) {
	if s == nil || session == nil {
		return false, nil
	}
	memberKey := session.selectedCharacterID
	state, ok := s.runtimeRaidForMember(memberKey)
	if !ok {
		return true, nil
	}
	request, err := decodeRuntimeRaidBuffRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-buff-request-invalid",
			"type", typ, "char_id", memberKey,
			"body_len", len(body), "error", err)
		_ = s.sendRuntimeRaidBuffStatus(session, state)
		return true, nil
	}
	if !runtimeRaidBuffLeaderAuthorized(state, memberKey) {
		s.logGameEvent(session, "game-raid-buff-rejected",
			"type", typ, "char_id", memberKey, "raid_key", state.RaidKey,
			"category", request.Category, "reason", "not_the_leader_or_phase_not_running")
		_ = s.sendRuntimeRaidBuffStatus(session, state)
		return true, nil
	}
	definition, ok := runtimeRaidBuffDefinitionFor(request.Category)
	if !ok {
		_ = s.sendRuntimeRaidBuffStatus(session, state)
		return true, nil
	}

	partyIndex := uint16(0)
	targetKey := memberKey
	if !definition.RaidWide {
		resolvedParty, resolved := s.runtimeRaidBuffTargetParty(state, request.TargetKey)
		if !resolved {
			s.logGameEvent(session, "game-raid-buff-rejected",
				"type", typ, "char_id", memberKey, "raid_key", state.RaidKey,
				"category", request.Category, "target", request.TargetKey,
				"reason", "designated_squad_member_is_not_inside_a_gate")
			_ = s.sendRuntimeRaidBuffStatus(session, state)
			return true, nil
		}
		partyIndex = resolvedParty
		targetKey = request.TargetKey
	} else {
		// A raid-wide skill ignores the wire target, exactly as 86JP does
		// (Situation.cs:72-82): its row names the leader's own squad and the
		// activating member.
		if member, found := runtimeRaidFrozenMember(state, memberKey); found {
			partyIndex = uint16(runtimeRaidMemberSquad(member.GroupIndex))
		}
	}

	// The effect is applied before the activation is recorded, so a skill whose
	// effect cannot be applied does not burn its cooldown. 86JP orders it the same
	// way: the effect call gates the activation record (Situation.cs:107-128).
	effect, effectApplied, effectNote := s.applyRuntimeRaidBuffEffect(state, definition, targetKey)
	if !effectApplied {
		s.logGameEvent(session, "game-raid-buff-rejected",
			"type", typ, "char_id", memberKey, "raid_key", state.RaidKey,
			"category", request.Category, "target", targetKey, "reason", effectNote)
		_ = s.sendRuntimeRaidBuffStatus(session, state)
		return true, nil
	}

	activation, activated := s.recordRuntimeRaidBuffActivation(
		state.RaidKey, request.Category, definition, partyIndex, targetKey)
	if !activated {
		s.logGameEvent(session, "game-raid-buff-rejected",
			"type", typ, "char_id", memberKey, "raid_key", state.RaidKey,
			"category", request.Category, "reason", "still_on_cooldown")
		_ = s.sendRuntimeRaidBuffStatus(session, state)
		return true, nil
	}
	s.logGameEvent(session, "game-raid-buff-activated",
		"type", typ, "char_id", memberKey, "raid_key", state.RaidKey,
		"category", request.Category, "raid_wide", definition.RaidWide,
		"target", targetKey, "party", partyIndex,
		"active_until", activation.ActiveUntil,
		"cooldown_until", activation.CooldownUntil,
		"extra_key", request.ExtraKey,
		"server_effect", effectNote)

	// Order follows 86JP (Situation.cs:144 -> :151-152): the panel status first,
	// then whatever the effect has to announce.
	if err := s.broadcastRuntimeRaidBuffStatus(state); err != nil {
		s.logGameEvent(session, "game-raid-buff-status-broadcast-failed",
			"type", typ, "char_id", memberKey, "raid_key", state.RaidKey, "error", err)
	}
	if effect != nil {
		if err := s.publishRuntimeRaidBuffEffect(state, *effect); err != nil {
			s.logGameEvent(session, "game-raid-buff-effect-publish-failed",
				"type", typ, "char_id", memberKey, "raid_key", state.RaidKey, "error", err)
		}
	}
	return true, nil
}
