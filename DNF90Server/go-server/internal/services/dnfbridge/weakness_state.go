// weakness_state.go owns the weakness (虚弱) penalty a forced
// dungeon exit applies, the way that penalty wears off, and the server-side
// rule for who is exempt.
//
// State. The penalty is a level in character stat `stamina`, 0 = healthy. The
// column already existed (TINYINT UNSIGNED) and was already projected into the
// select-character userinfo tail, but nothing in this server had ever written
// it, so a force quit carried no penalty at all. The client models the same
// value as a 0..100 condition; the EXE compares it against 100 everywhere
// (sub_30C6B10 `cmp eax, 0x64` and nine other sites) and 100 means healthy.
//
// Shape of the penalty, per operator decision 2026-09-12: a forced exit applies
// it whole (currentWeaknessForcedReturnLevel, a 19% health cap) rather than one
// level at a time, and it is then worked off by time, one level per
// currentWeaknessRecoveryInterval. The paid/contract 解除虚弱 NPC recovery
// clears whatever is left outright.
//
// Exemption. 黑钻 is a passive account subscription: while it is active the
// penalty is skipped and nothing is consumed. 魔王契約 is the selectable
// devil-contract perk in slot 5 (premium.DevilSlotFreeWeakness); it is no longer
// an exemption for a forced exit at all - the server only ever spends that quota
// when the player picks the contract button in the recovery UI (op9), so a
// give-up can never burn a daily use the player never chose to pay with. Rooms
// on the currentWeaknessExemptDungeonIDs whitelist (修炼场 5000、转职体验「领悟时间」
// 7142), the op16 practice protocol mode and PVP channels never weaken at all.
//
// Superseded 2026-09-11/12: this header used to claim the condition had "a
// single write site that always stores 100", so a stored penalty could never be
// rendered. That is wrong. The client renders the stored level as the health
// cap, and class-0 opcode 33 publishes any 0..100 value on demand - see
// currentWeaknessConditionMsg for the three write sites and which to use.
//
// Known client limit, unchanged: this EXE draws no dedicated weakness HUD
// widget. What the player sees is the reduced health cap itself.
package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfweaknesspotion "longheng.io/server/internal/modules/dnf/weaknesspotion"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	// currentWeaknessStatKey is the character stat the penalty level lives in.
	currentWeaknessStatKey = "stamina"
	// currentWeaknessMaximumLevel matches the client's own clamp: the condition
	// value bottoms out at 100 - 9*10 = 10.
	currentWeaknessMaximumLevel int64 = 10
	// currentWeaknessConditionStep is the condition lost per penalty level.
	currentWeaknessConditionStep int64 = 9
	// currentWeaknessHealthyValue is the condition the client treats as healthy.
	currentWeaknessHealthyValue int64 = 100
	// currentWeaknessForcedReturnLevel is what one forced exit costs. Operator
	// decision 2026-09-12: the penalty lands at its full weight immediately -
	// 9 levels, i.e. a 19% health cap - and is then worked off by time, rather
	// than creeping down 9 points per give-up. Recovery, not repetition, is
	// what varies the number now.
	currentWeaknessForcedReturnLevel int64 = 9
	// currentWeaknessRecoveryInterval is how long one penalty level takes to
	// wear off on its own. Operator decision 2026-09-14: one level per minute
	// (was five), so a forced exit's 9 levels clear in 9 minutes and even the
	// 10-level ceiling clears in 10 - no penalty can outlast ten minutes. Until
	// recovery existed the penalty only ever went up - the NPC recovery was the
	// single way down - which is how a test character reached level 9 (19%
	// health) purely by abandoning six dungeon runs.
	currentWeaknessRecoveryInterval = time.Minute
	// currentWeaknessRecoveryAtStatKey holds the **在线秒**（不是 unix 秒）当前这一级
	// 开始倒计时的位置。用在线秒而不是墙上时钟，是因为虚弱计时在离线期间必须冻结
	// （运营要求 2026-09-15）。它是 overflow stat（dnf_character_stats），不需要改表结构。
	currentWeaknessRecoveryAtStatKey = "weakness_recovery_at"

	// currentWeaknessOnlineSecondsStatKey 累计该角色**已结清**的真实在线秒数，
	// 是虚弱恢复唯一的时钟来源。离线时段不计入，所以"离线时计时锁定、上线继续走"。
	currentWeaknessOnlineSecondsStatKey = "weakness_online_seconds"

	// currentWeaknessOnlineWindowStatKey 是"本次在线窗口的墙上起点"。离线时被清零
	// （见 stopCurrentWeaknessOnlineClock），于是离线期间在线秒不会前进；
	// 老存档没有这个键（=0）时在线秒也不前进，惩罚不会被凭空勾销。
	currentWeaknessOnlineWindowStatKey = "weakness_online_window_start"
)

// Reason codes the current EXE's op9 failure branch (sub_1D11900) decodes.
const (
	currentWeaknessReasonNotWeakened  byte = 0x12
	currentWeaknessReasonCannotAfford byte = 0x16
)

var (
	errCurrentWeaknessUnavailable = errors.New("current weakness runtime is unavailable")
	errCurrentWeaknessNotWeakened = errors.New("current character carries no weakness")
	errCurrentWeaknessCannotPay   = errors.New("current character cannot pay the weakness recovery cost")
)

// currentWeaknessExemption records which rule spared a forced exit, for the log
// and for the tests that pin the precedence.
type currentWeaknessExemption string

const (
	currentWeaknessExemptionNone         currentWeaknessExemption = "none"
	currentWeaknessExemptionBlackDiamond currentWeaknessExemption = "black_diamond"
	// currentWeaknessExemptionDevilSlot is the player-requested op9 contract
	// route, not a forced-exit exemption: the quota is spent only when the
	// player presses the contract button in the 解除虚弱 UI.
	currentWeaknessExemptionDevilSlot currentWeaknessExemption = "devil_contract_slot5"
	// currentWeaknessExemptionPotItem is the 防虚弱药剂 route. The potion's buff
	// lives in the client only - a live capture shows the use arrives as the
	// ordinary class-1/op44 request and no later packet carries the state - so
	// the server credits the durable immunity window the inventory owner writes
	// when one of the family's 70 usable items is consumed.
	currentWeaknessExemptionPotItem currentWeaknessExemption = "weakness_potion_item"
	// currentWeaknessExemptionPotHeld is the passive variant (十周年防虚弱青石):
	// merely carrying it in the inventory already prevents the penalty, so no
	// use and no window are involved.
	currentWeaknessExemptionPotHeld currentWeaknessExemption = "weakness_potion_held_item"
)

// currentWeaknessGoldSource names where a paid 解除虚弱 actually took its gold
// from, so the caller can refresh the matching client-side wallet.
type currentWeaknessGoldSource string

const (
	currentWeaknessGoldSourceNone currentWeaknessGoldSource = "none"
	// currentWeaknessGoldSourceCharacter is the authoritative store on this
	// server: gold is the character's own `gold` stat, and that column is what
	// sendSelectedMainWalletsRefresh projects into main-list slot 0.
	currentWeaknessGoldSourceCharacter currentWeaknessGoldSource = "character_gold_stat"
	// currentWeaknessGoldSourceLegacySlot is the main-list slot-0 inventory row
	// this file used to charge. Nothing else in this tree reads or writes that
	// row as a wallet, so it is honoured only when the character column cannot
	// cover the price: a deployment still carrying such a row keeps paying
	// instead of being answered "cannot afford".
	currentWeaknessGoldSourceLegacySlot currentWeaknessGoldSource = "legacy_main_list_slot0_wallet_row"
)

// currentWeaknessLevel reads the stored penalty level, clamped to the range the
// client can render.
func currentWeaknessLevel(character dnfrepo.CharacterRecord) int64 {
	if character.Stats == nil {
		return 0
	}
	level := character.Stats[currentWeaknessStatKey]
	if level < 0 {
		return 0
	}
	if level > currentWeaknessMaximumLevel {
		return currentWeaknessMaximumLevel
	}
	return level
}

// startCurrentWeaknessOnlineClock 在某角色刚上线（被选中）时调用一次：
// 结算上一次在线窗口的秒数，并把"本次在线窗口起点"设为 now。
//
// 离线时段因此不出现在任何一项里：虚弱恢复只累加真实在线窗口。
// 调用是幂等的 —— 已经在线的角色重复调用只会把窗口起点往后推，最多丢一点秒数。
func (s *Service) startCurrentWeaknessOnlineClock(session *gameSession) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	now := s.gameplayNow()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	if currentWeaknessOnlineWindowStart(character) == 0 {
		// 窗内累计秒 + 起点：两者一起落库，"上次在线到哪一刻"就持久了。
		character.Stats[currentWeaknessOnlineSecondsStatKey] = currentWeaknessOnlineSeconds(character, now)
		character.Stats[currentWeaknessOnlineWindowStatKey] = now.Unix()
		character.UpdatedAt = now
		if err := repositories.Character.Save(ctx, character); err != nil {
			s.logGameEvent(session, "game-weakness-online-clock-start-failed",
				"char_id", characterID, "error", err)
			return
		}
	}
	s.logGameEvent(session, "game-weakness-online-clock-started",
		"char_id", characterID,
		"online_seconds", character.Stats[currentWeaknessOnlineSecondsStatKey],
		"window_start", character.Stats[currentWeaknessOnlineWindowStatKey])
}

// stopCurrentWeaknessOnlineClock 在角色下线时调用：把本次窗口的秒数结清并清掉窗口标记，
// 这样离线期间再有任何读路径调用 settle，在线秒都不会前进。
func (s *Service) stopCurrentWeaknessOnlineClock(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	characterID := ""
	if session.selectedCharacterID != 0 {
		characterID = strconv.Itoa(int(session.selectedCharacterID))
	}
	repositories, ok := s.repositoryGroup()
	if characterID == "" || !ok || repositories.Character == nil {
		return
	}
	now := s.gameplayNow()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	// 结算前先算总量（= 已结清 + 本次窗口），再清窗口标记，顺序不能反。
	total := currentWeaknessOnlineSeconds(character, now)
	character.Stats[currentWeaknessOnlineSecondsStatKey] = total
	character.Stats[currentWeaknessOnlineWindowStatKey] = 0
	character.UpdatedAt = now
	if err := repositories.Character.Save(ctx, character); err != nil {
		s.logGameEvent(session, "game-weakness-online-clock-stop-failed",
			"char_id", characterID, "error", err)
		return
	}
	s.logGameEvent(session, "game-weakness-online-clock-stopped",
		"char_id", characterID,
		"online_seconds", total,
		"weakness_level", currentWeaknessLevel(character))
}

// currentWeaknessOnlineWindowStart 读"本次在线窗口起点"：先看进程内，再看持久化的 stat。
func currentWeaknessOnlineWindowStart(character dnfrepo.CharacterRecord) int64 {
	if character.Stats == nil {
		return 0
	}
	return character.Stats[currentWeaknessOnlineWindowStatKey]
}

// currentWeaknessOnlineSeconds 返回该角色"到目前为止"的累计在线秒。
//
// = 已结清窗口的累计秒 + 本次在线窗口里流逝的秒数。离线时段不在任何一项里，
// 所以离线时这个值不动 —— 这正是"离线锁定计时"的实现方式。
// 窗口起点缺失（老存档、或角色不在线）时只返回已累计值：既不发奖励，也不勾销惩罚。
func currentWeaknessOnlineSeconds(character dnfrepo.CharacterRecord, now time.Time) int64 {
	if character.Stats == nil {
		return 0
	}
	stored := character.Stats[currentWeaknessOnlineSecondsStatKey]
	if stored < 0 {
		stored = 0
	}
	start := currentWeaknessOnlineWindowStart(character)
	if start <= 0 || now.Unix() <= start {
		return stored
	}
	return stored + (now.Unix() - start)
}

// settleCurrentWeaknessRecovery ages a stored penalty forward to `now` and
// reports the settled level plus whether the record actually changed.
//
// **时钟是在线秒，不是墙上时钟**：离线时段不计入恢复（运营要求 2026-09-15：
// "角色不在线就锁定当前虚弱计时，上线才继续"）。因此它不再"边离线边恢复"，
// 但仍然对服务器重启安全 —— 累计在线秒存在角色 stat 里，重启后继续累加。
//
// The leftover remainder is carried (the clock advances by whole intervals
// only), otherwise every settle would restart the countdown and a character
// that is looked at often would never recover.
func settleCurrentWeaknessRecovery(character *dnfrepo.CharacterRecord, now time.Time) (int64, bool) {
	if character == nil {
		return 0, false
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	level := currentWeaknessLevel(*character)
	if level <= 0 {
		// A healthy character carries no countdown. Drop a stale marker so the
		// next penalty starts its own clock from scratch.
		if _, found := character.Stats[currentWeaknessRecoveryAtStatKey]; found {
			delete(character.Stats, currentWeaknessRecoveryAtStatKey)
			return 0, true
		}
		return 0, false
	}
	interval := int64(currentWeaknessRecoveryInterval / time.Second)
	// 在线秒是唯一的时钟：先把"已结清窗口 + 本次在线窗口"算出来并落库，
	// 再拿它和倒计时起点比较。
	online := currentWeaknessOnlineSeconds(*character, now)
	if online < 0 {
		online = 0
	}
	character.Stats[currentWeaknessOnlineSecondsStatKey] = online
	startedAt, present := character.Stats[currentWeaknessRecoveryAtStatKey]
	// 倒计时锚点的语义：
	//   缺失  -> 没有在跑的倒计时（惩罚等级为 0 时就是这样，见下）;
	//   -1    -> 惩罚刚下、还没有锚点，从当前在线秒起算;
	//   其它  -> 在线秒位置。老存档里可能是 unix 秒（远大于在线秒），
	//            那不能当成"已经过了很久"，重锚到当前在线秒而不是免罚。
	if !present {
		return level, false
	}
	if startedAt < 0 || startedAt > online {
		character.Stats[currentWeaknessRecoveryAtStatKey] = online
		return level, true
	}
	steps := (online - startedAt) / interval
	if steps <= 0 {
		return level, false
	}
	// 一次结算最多掉一级：设计口径是"每在线一分钟掉一级"（currentWeaknessRecoveryInterval），
	// 而 (online-startedAt) 的两端取整差可能刚好凑出两个 interval（实测 2026-09-15：
	// 8 -> 6 一跳两级）。多出来的余量不会浪费 —— 下次结算按同一公式自然补上。
	if steps > 1 {
		steps = 1
	}
	if steps > level {
		steps = level
	}
	level -= steps
	character.Stats[currentWeaknessStatKey] = level
	if level == 0 {
		// 恢复完成：删掉倒计时，下一次惩罚重新起算。
		delete(character.Stats, currentWeaknessRecoveryAtStatKey)
	} else {
		character.Stats[currentWeaknessRecoveryAtStatKey] = startedAt + steps*interval
	}
	return level, true
}

// currentWeaknessConditionMsg is the class-0 notification that publishes the
// health cap on its own.
//
// Found by disassembly on 2026-09-11, which also corrects this file's original
// claim that the condition had "a single write site that always stores 100".
// Three sites write the local player's condition setter (vtable+0xAD4):
//
//   - sub_1D11820, the op9 success ACK, with a hardcoded 0x64. That is the only
//     one the server had been using, so the client could only ever be told
//     "fully healthy" - a partial value was unsendable.
//   - sub_1D7FA40, class-0 opcode 4. Reads one byte and sets it. Shows the
//     "recovered" message when the byte is exactly 0x64, nothing otherwise.
//   - sub_1D3A260, class-0 opcode 33, used here. Also reads one byte, but when
//     the byte is not 0x64 it first runs the weakened-state presentation
//     (0x1D3A289..0x1D3A307) before setting the value, and at exactly 0x64 it
//     jumps straight to the set. So it both shows the penalty properly and
//     clears it quietly - op4 can do neither half as well.
//
// Opcode 33 is also what 86JPPP sends (GamePacketEnvelopeBuilder.Build(0x00,
// 0x0021, {100}) in its StaminaHandler), so this one notification id really
// does carry across from 86JP to this 90CN client - which is not the general
// rule for notification ids and is worth remembering as the exception.
//
// Class 0 is populated by three registration functions (sub_2262A00 calls
// sub_1D927E0, sub_1D50DB0 and sub_1D7E7D0 on one table), which is why a scan
// of the block owning op14/op15 alone shows class 0 skipping from 1 to 9 and
// misses both of these.
const currentWeaknessConditionMsg uint16 = 33

// sendCurrentWeaknessConditionRefresh publishes the character's current health
// cap. Must be called after the town player-state rebuild, never before it.
// It also re-arms the recovery tick, so a weakened player standing still in
// town keeps seeing the cap move.
func (s *Service) sendCurrentWeaknessConditionRefresh(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		s.logGameEvent(session, "game-weakness-condition-refresh-skipped",
			"char_id", session.selectedCharacterID, "source", source, "found", found, "error", err)
		return nil
	}
	// Age the penalty forward first so the value published is the one the
	// player has actually recovered to, then persist it.
	changed := s.settleWeaknessForProjection(ctx, repositories, session, &character)
	level := currentWeaknessLevel(character)
	// A healthy character that did not just become healthy needs no packet at
	// all; publish a penalty, or the one push that clears a penalty.
	if level == 0 && !changed {
		return nil
	}
	condition := currentWeaknessCondition(level)
	s.logGameEvent(session, "game-weakness-condition-refresh-send",
		"char_id", session.selectedCharacterID,
		"source", source,
		"weakness_level", level,
		"client_condition", condition,
		"msg_id", currentWeaknessConditionMsg,
		"classification", 0,
		"client_handler", "sub_1D3A260")
	if err := s.sendGameUpperRawClassCodec(
		session, currentWeaknessConditionMsg, []byte{byte(condition)}, 0, true,
	); err != nil {
		return err
	}
	s.armCurrentWeaknessRecoveryTimer(session, character, level, source)
	return nil
}

// currentWeaknessRecoveryTimerName keys the per-character recovery tick.
func currentWeaknessRecoveryTimerName(characterID uint16) string {
	return "weakness-recovery:" + strconv.Itoa(int(characterID))
}

// currentWeaknessRecoveryDue is how long until the next level wears off, given
// the countdown the settle left behind. A record without a countdown owes a
// full interval.
func currentWeaknessRecoveryDue(character dnfrepo.CharacterRecord, now time.Time) time.Duration {
	startedAt := int64(0)
	if character.Stats != nil {
		startedAt = character.Stats[currentWeaknessRecoveryAtStatKey]
	}
	if startedAt <= 0 || now.Unix() < startedAt {
		return currentWeaknessRecoveryInterval
	}
	remainder := (time.Duration(now.Unix()-startedAt) * time.Second) % currentWeaknessRecoveryInterval
	due := currentWeaknessRecoveryInterval - remainder
	if due <= 0 {
		return currentWeaknessRecoveryInterval
	}
	return due
}

// armCurrentWeaknessRecoveryTimer keeps the health cap moving while the player
// is just standing in town. Recovery itself is lazy and was already correct --
// the stored level ages on read -- but nothing republished it between rebuilds,
// so a player watching their own bar saw it frozen until they re-selected a
// character. A healthy character cancels the tick instead of re-arming.
func (s *Service) armCurrentWeaknessRecoveryTimer(
	session *gameSession,
	character dnfrepo.CharacterRecord,
	level int64,
	source string,
) {
	if s == nil || s.gameplayTimers == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := session.selectedCharacterID
	name := currentWeaknessRecoveryTimerName(characterID)
	s.gameplayTimers.Cancel(name)
	if level <= 0 {
		return
	}
	due := currentWeaknessRecoveryDue(character, s.gameplayNow())
	if err := s.gameplayTimers.ScheduleAfter(name, due, func(time.Time) {
		// Re-resolve the session: the character may have logged out, switched
		// or reconnected since this was armed.
		live, online := s.onlineGameSession(characterID)
		if !online || live == nil || live.selectedCharacterID != characterID {
			return
		}
		// Settles, persists, publishes, and re-arms itself while the penalty
		// lasts.
		if err := s.sendCurrentWeaknessConditionRefresh(live, "weakness_recovery_tick"); err != nil {
			s.logGameEvent(live, "game-weakness-recovery-tick-failed",
				"char_id", characterID, "error", err)
		}
	}); err != nil {
		s.logGameEvent(session, "game-weakness-recovery-timer-failed",
			"char_id", characterID, "source", source, "error", err)
	}
}

// settleWeaknessForProjection ages the penalty forward and persists it right
// before subtype0 publishes it. A persistence failure is logged and swallowed:
// the character has still recovered as far as the caller's copy says, and a
// bookkeeping error must not block character selection.
func (s *Service) settleWeaknessForProjection(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
	character *dnfrepo.CharacterRecord,
) bool {
	if s == nil || character == nil || repositories.Character == nil {
		return false
	}
	before := currentWeaknessLevel(*character)
	level, changed := settleCurrentWeaknessRecovery(character, s.gameplayNow())
	if !changed {
		return false
	}
	character.UpdatedAt = s.gameplayNow()
	if err := repositories.Character.Save(ctx, *character); err != nil {
		if session != nil {
			s.logGameEvent(session, "game-weakness-recovery-persist-failed",
				"char_id", character.CharacterID, "error", err)
		}
		return level != before
	}
	if session != nil && level != before {
		s.logGameEvent(session, "game-weakness-recovered-over-time",
			"char_id", character.CharacterID,
			"previous_level", before,
			"weakness_level", level,
			"client_condition", currentWeaknessCondition(level),
			"interval_seconds", int64(currentWeaknessRecoveryInterval/time.Second))
	}
	return level != before
}

// currentWeaknessCondition converts a penalty level into the 0..100 condition
// the client compares against 100.
func currentWeaknessCondition(level int64) int64 {
	condition := currentWeaknessHealthyValue - level*currentWeaknessConditionStep
	if condition < 0 {
		return 0
	}
	return condition
}

// currentWeaknessExemptDungeonIDs is the room whitelist: leaving one of these,
// however abruptly, never weakens. The value is the rule name reported in the
// log, so an operator can tell which room's arm spared the run without
// cross-checking the dungeon id.
//
// 修炼场 (PoongjinTrainingRoom, dungeon 5000) is the case this exists for.
// The town-return path already carried a comment saying the practice room must
// never weaken, but it guarded on currentDungeonRuntimeIsPractice, which is the
// op16 *protocol* mode (state 1 + token 0), not the room. The training room's
// own direct-entry button sends state 1 + token 0xFFFF, which
// currentDungeonProtocolModeForWire deliberately maps back to normal mode - so
// the guard never fired for the very room it named, and every visit to 修炼场
// cost a 19% health cap on the way out.
//
// The room carries no PVF flag to key this off: its .dgn has no fatigue,
// prohibit-practice or free-room section, so the identity has to be the
// dungeon id.
//
// 转职体验「领悟时间」(dungeon 7142) joined the whitelist 2026-09-17. It shares
// 修炼场's direct-entry button shape, and leaving it is an ordinary forced exit:
// 结束体验 walks the client's own give-up route back to town (bodyless op42),
// the run commits as abandoned, and the trial - the one room a player is
// supposed to try a class in for free - charged the full 19% health cap. Live
// evidence from the same day's packet log, right after a 14-second visit:
// `char_id=4 dungeon_id=7142 exemption=none weakness_level=9
// client_condition=19`. 体验免费 is the whole point of the feature (see
// ApplyGrowupExperience), so its room must not bill a penalty on the way out
// either.
//
// 新手教程副本这一条豁免的说明见下面 currentWeaknessExemptRoom 的注释。
var currentWeaknessExemptDungeonIDs = map[int64]string{
	int64(currentPungjinTrainingDungeonID): currentWeaknessExemptRuleDungeonWhitelist,
	int64(currentJobChangeTrialDungeonID):  currentWeaknessExemptRuleJobChangeTrialDungeon,
}

// Weakness room exemption rule names, reported in the log so an operator can
// tell which arm spared the run.
const (
	currentWeaknessExemptRulePracticeProtocol = "practice_protocol_mode"
	currentWeaknessExemptRuleDungeonWhitelist = "whitelisted_dungeon"
	// currentWeaknessExemptRuleJobChangeTrialDungeon: 转职体验副本「领悟时间」(7142)。
	// 现场 2026-09-17（packet_log）：体验 14 秒后退出回城就吃到满档虚弱
	// （exemption=none weakness_level=9 client_condition=19）。
	currentWeaknessExemptRuleJobChangeTrialDungeon = "job_change_trial_dungeon"
	// currentWeaknessExemptRuleTutorialDungeon: PVF [tutorial dungeon]，即新建角色的
	// 教程副本。现场 2026-09-15：教程中途退出会被套上 19% 血量上限的虚弱，而教程正是
	// 最不该罚、新建角色最不该带着虚弱进城镇的地方。
	currentWeaknessExemptRuleTutorialDungeon = "tutorial_dungeon"
	currentWeaknessExemptRulePVPChannel      = "pvp_channel"
)

// currentWeaknessExemptRoom reports whether this run is one the weakness
// penalty must skip, and names the rule for the log.
//
// Four ways in:
//   - the op16 practice protocol mode (unchanged behaviour);
//   - the dungeon-id whitelist above (修炼场 5000、转职体验「领悟时间」7142)，它自己
//     带规则名，所以日志能分辨是哪个房间放行的;
//   - any PVF [tutorial dungeon] (新建角色的教程副本，2026-09-15 加);
//   - any run on a PVP channel. 决斗场 never builds a dungeon runtime today
//     (pvp matching requires session.dungeon.runtime == nil), so this arm is
//     not reachable through the current PVP flow; it is here so a future PVP
//     room that does take the dungeon route cannot start charging weakness by
//     accident. Operator instruction 2026-09-14: 修炼场和 PVP 都不触发虚弱.
func currentWeaknessExemptRoom(session *gameSession, runtime *runtimeDungeonState) (string, bool) {
	if currentDungeonRuntimeIsPractice(runtime) {
		return currentWeaknessExemptRulePracticeProtocol, true
	}
	if runtime != nil {
		if rule, whitelisted := currentWeaknessExemptDungeonIDs[runtime.Dungeon.ID]; whitelisted {
			return rule, true
		}
		if isPVFTutorialDungeon(runtime) {
			return currentWeaknessExemptRuleTutorialDungeon, true
		}
	}
	if session != nil && isPVPChannel(session.channel) {
		return currentWeaknessExemptRulePVPChannel, true
	}
	return "", false
}

// currentForcedReturnWeaknessRaidExempt 报告这一次"未通关直接回城"要不要按**攻坚战**
// 免掉虚弱惩罚，以及依据哪一条规则。
//
// 两条依据：
//
//  1. `raid_gate_exit_never_weakens` —— 这一次 run 本身就带着攻坚绑定
//     (`runtime.raidDungeon.RaidKey != 0`)。原来的规则，安东/卢克/超时空的门都走这条。
//
//  2. `raid_channel_exit_never_weakens` —— 退出发生在**超时空**（频道类型 42），
//     但绑定已经被**服务器自己**清空了。失败/撤退收尾就是这么做的：
//     returnRuntimeFiendWarDungeons 在 ended 时先把 `runtime.raidDungeon`
//     清成零值再走未通关回城。只看绑定就会把服务器自己发起的"超时空失败收尾"
//     误判成普通副本中途退出，于是给全队挂上虚弱。
//
// 真机 2026-10-02 00:00:36（raid 12058625，分片 20261002-001）—— 就是第 2 种：
//
//	game-dungeon-forced-return-weakness-applied char_id=22
//	  source=fiendwar_failed_waiting_area_abandoned_run_town_return
//	  exemption=none weakness_level=9 client_condition=19
//
// 对照同一天 00:04:24 走**门**（绑定还在）的那一次，第 1 条就把虚弱免掉了：
//
//	game-dungeon-forced-return-weakness-raid-exempt char_id=1
//	  source=current_exe_bodyless_op42_active_run_giveup_abandoned_run_town_return
//	  rule=raid_gate_exit_never_weakens
//
// 操作员 2026-10-02 定案：「攻坚战无论怎么中途放弃 都不应该虚弱的啊」。
//
// 作用域刻意只加**超时空**这一条：安东/卢克的 run 一直带着绑定、走第 1 条就豁免了，
// 而清空绑定的收尾只有超时空有（returnRuntimeFiendWarDungeons），
// 所以加这一条对安东/卢克没有任何影响。
func (s *Service) currentForcedReturnWeaknessRaidExempt(session *gameSession, runtime *runtimeDungeonState) (string, bool) {
	if s == nil || session == nil {
		return "", false
	}
	if s.currentDungeonRuntimeIsRaidGate(runtime) {
		return "raid_gate_exit_never_weakens", true
	}
	if isCurrentFiendWarRaidChannel(session) {
		return "raid_channel_exit_never_weakens", true
	}
	return "", false
}

// applyCurrentForcedReturnWeakness is called once a dungeon run is committed as
// abandoned. It resolves the exemption rules first and only writes a penalty
// when none applies. A failure is logged and swallowed: the town return is
// already committed and must not be undone by a penalty bookkeeping error.
// It returns the exemption that was applied so the caller can publish the
// refreshed premium state once the town player state exists. That refresh must
// not be sent from here: this runs before the player-state rebuild, and this
// project has repeatedly broken town co-presence by injecting state packets
// ahead of that rebuild.
func (s *Service) applyCurrentForcedReturnWeakness(
	session *gameSession,
	source string,
) currentWeaknessExemption {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentWeaknessExemptionNone
	}
	level, exemption, err := s.commitCurrentForcedReturnWeakness(session)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-forced-return-weakness-skipped",
			"char_id", session.selectedCharacterID,
			"source", source,
			"error", err)
		return currentWeaknessExemptionNone
	}
	s.logGameEvent(session, "game-dungeon-forced-return-weakness-applied",
		"char_id", session.selectedCharacterID,
		"source", source,
		"exemption", string(exemption),
		"weakness_level", level,
		"client_condition", currentWeaknessCondition(level),
		"contract_quota_spent", false,
		"rule", "operator_decision_black_diamond_exempts_a_forced_exit_and_nothing_is_spent")
	return exemption
}

// currentHeldWeaknessPotion reports whether the character still carries one of
// the passive 防虚弱 items (十周年防虚弱青石) in the inventory. Those items say the
// effect applies while the item is held in the material slots, so the question
// is "is it still in the bag", not "was it used".
func currentHeldWeaknessPotion(
	ctx context.Context,
	inventory dnfrepo.InventoryRepository,
	characterID string,
) bool {
	if inventory == nil {
		return false
	}
	record, found, err := inventory.Load(ctx, characterID)
	if err != nil || !found {
		return false
	}
	for _, stack := range record.Slots {
		if stack.Count > 0 && dnfweaknesspotion.HeldExempts(stack.ItemID) {
			return true
		}
	}
	return false
}

func (s *Service) commitCurrentForcedReturnWeakness(
	session *gameSession,
) (int64, currentWeaknessExemption, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.CharacterAssets == nil {
		return 0, currentWeaknessExemptionNone, errCurrentWeaknessUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	if accountID == "" {
		return 0, currentWeaknessExemptionNone, errCurrentWeaknessUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return 0, currentWeaknessExemptionNone, err
	}
	if !found {
		return 0, currentWeaknessExemptionNone, errCurrentWeaknessUnavailable
	}

	now := s.gameplayNow()
	// 黑钻 is the only exemption. 魔王契约 used to exempt here too, spending a
	// daily quota automatically; operator decision 2026-09-12 removed that.
	// The contract is now only ever spent when the player chooses the contract
	// button in the 解除虚弱 UI (op9), so a quota can no longer disappear into
	// a give-up the player never meant to pay for.
	if premium.BlackDiamondActive(account, now) {
		return 0, currentWeaknessExemptionBlackDiamond, nil
	}

	var (
		level     int64
		exemption = currentWeaknessExemptionNone
	)
	err = repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			characters dnfrepo.CharacterRepository,
			inventory dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			character, characterFound, loadErr := characters.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !characterFound {
				return errCurrentWeaknessUnavailable
			}
			character = dnfrepo.CloneCharacter(character)
			if character.Stats == nil {
				character.Stats = make(map[string]int64)
			}
			// 防虚弱药剂, used route: the potion started a durable immunity window
			// and already cleared the standing penalty, so this forced exit is
			// free and nothing is written.
			if dnfweaknesspotion.Active(character.Stats, now) {
				exemption = currentWeaknessExemptionPotItem
				return nil
			}
			// 防虚弱药剂, passive route: carrying the item is the whole effect.
			if currentHeldWeaknessPotion(ctx, inventory, characterID) {
				exemption = currentWeaknessExemptionPotHeld
				return nil
			}
			// Age any existing penalty forward first, so the new level builds
			// on what the player has actually recovered to rather than on a
			// stale high-water mark.
			settleCurrentWeaknessRecovery(&character, now)
			// The penalty is applied whole, not incremented. An existing
			// heavier penalty is kept: a forced exit must never heal.
			level = currentWeaknessForcedReturnLevel
			if settled := currentWeaknessLevel(character); settled > level {
				level = settled
			}
			if level > currentWeaknessMaximumLevel {
				level = currentWeaknessMaximumLevel
			}
			character.Stats[currentWeaknessStatKey] = level
			// A fresh penalty restarts the countdown: the player owes the full
			// interval again before the next level wears off. 倒计时锚点是**在线秒**
			// （settleCurrentWeaknessRecovery 已经把累计值写进 stat，这里直接复用），
			// 所以离线期间这一级不会继续往下扣。
			character.Stats[currentWeaknessRecoveryAtStatKey] = character.Stats[currentWeaknessOnlineSecondsStatKey]
			character.UpdatedAt = now
			return characters.Save(ctx, character)
		},
	)
	if err != nil {
		return 0, currentWeaknessExemptionNone, err
	}
	return level, exemption, nil
}

// clearCurrentWeakness removes the penalty and returns the level it removed,
// the gold left behind, and which store paid.
//
// Used by both op9 recovery routes: the player-requested contract route passes
// no gold cost, the player-requested paid route passes the PVF price.
func (s *Service) clearCurrentWeakness(
	session *gameSession,
	goldCost int64,
) (int64, int64, currentWeaknessGoldSource, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return 0, 0, currentWeaknessGoldSourceNone, errCurrentWeaknessUnavailable
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()

	var (
		clearedLevel  int64
		remainingGold int64
		goldSource    = currentWeaknessGoldSourceNone
	)
	err := repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			character, characterFound, loadErr := characters.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !characterFound {
				return errCurrentWeaknessUnavailable
			}
			character = dnfrepo.CloneCharacter(character)
			// Settle first: a penalty that already timed out must report
			// "not weakened" rather than charge the player to remove nothing.
			clearedLevel, _ = settleCurrentWeaknessRecovery(&character, s.gameplayNow())
			if clearedLevel == 0 {
				return errCurrentWeaknessNotWeakened
			}
			if character.Stats == nil {
				character.Stats = make(map[string]int64)
			}
			if goldCost > 0 {
				// Gold is the character's own `gold` column, which is what the
				// client's list-0 wallet is projected from. The legacy
				// main-list slot-0 row is only consulted when that column
				// cannot cover the price; see currentWeaknessGoldSource.
				held := character.Stats["gold"]
				if held >= goldCost {
					remainingGold = held - goldCost
					character.Stats["gold"] = remainingGold
					goldSource = currentWeaknessGoldSourceCharacter
				} else {
					inventory, inventoryFound, inventoryErr := inventories.Load(ctx, characterID)
					if inventoryErr != nil {
						return inventoryErr
					}
					if !inventoryFound {
						return fmt.Errorf("%w: need=%d have=%d",
							errCurrentWeaknessCannotPay, goldCost, held)
					}
					inventory = dnfrepo.CloneInventory(inventory)
					key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 0)
					wallet, walletFound := inventory.Slots[key]
					if !walletFound || wallet.ItemID != 0 || wallet.Count < goldCost {
						return fmt.Errorf("%w: need=%d have=%d",
							errCurrentWeaknessCannotPay, goldCost, wallet.Count)
					}
					wallet.Count -= goldCost
					inventory.Slots[key] = wallet
					remainingGold = wallet.Count
					goldSource = currentWeaknessGoldSourceLegacySlot
					inventory.UpdatedAt = s.gameplayNow()
					if saveErr := dnfrepo.SaveInventoryFields(
						ctx, inventories, inventory, dnfrepo.InventoryFieldSlots,
					); saveErr != nil {
						return saveErr
					}
				}
			}
			character.Stats[currentWeaknessStatKey] = 0
			// A cleared penalty drops its countdown, so the next one starts a
			// fresh clock instead of instantly expiring.
			delete(character.Stats, currentWeaknessRecoveryAtStatKey)
			character.UpdatedAt = s.gameplayNow()
			return characters.Save(ctx, character)
		},
	)
	if err != nil {
		return 0, 0, currentWeaknessGoldSourceNone, err
	}
	return clearedLevel, remainingGold, goldSource, nil
}

// currentForcedReturnAppliesWeakness reports whether a committed run status is
// the forced exit the penalty attaches to. A cleared run never weakens.
func currentForcedReturnAppliesWeakness(status worldmap.DungeonRunStatus) bool {
	return status == worldmap.DungeonRunAbandoned
}
