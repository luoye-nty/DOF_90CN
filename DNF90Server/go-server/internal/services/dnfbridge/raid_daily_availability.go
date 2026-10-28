package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
)

const (
	// Current NoPack binds class-0/op1303 to the team-raid daily-state reader.
	// The enum's historical name refers to an unrelated C2S command, so keep the
	// current-client S2C meaning local to this owner.
	currentRaidDailyAvailabilityMsgID   uint16 = 1303
	currentRaidDailyAvailabilityRowSize        = 5

	currentRaidAntonMode    byte = 1
	currentRaidLukeMode     byte = 2
	currentRaidFiendWarMode byte = 3

	// Fallback limits used only when the account counters cannot be read. The
	// real values come from the same policies the start command charges:
	// etc/raid/{anton,luke/fiendwar}.etc [DAILY ACCOUNT ENTER LIMIT] (all 10).
	currentRaidAntonDailyLimit    byte = 10
	currentRaidLukeDailyLimit     byte = 10
	currentRaidFiendWarDailyLimit byte = 10

	// currentRaidAntonDailyPolicyKey and its siblings are the admission
	// policies reserveRuntimeRaidAdmissionCounters charges, so this viewer must
	// read exactly those or the channel-select entry count lies about the value
	// the start command enforces.
	currentRaidAntonDailyPolicyKey    = "raid.anton.account.daily"
	currentRaidLukeDailyPolicyKey     = "raid.luke.account.daily"
	currentRaidFiendWarDailyPolicyKey = "raid.fiend_war.account.daily"
)

// currentRaidAdmissionPolicyKeys is the complete set of policies one raid's
// start charges, in catalog order. It is the single source of truth for BOTH
// the charge (raid_admission_counter.go) and the window (this file), so the
// numbers the player reads can never drift from the numbers that gate entry.
//
// There is no Fiend-War entry: this server implements only the Anton and Luke
// raid owners (raid_luke_runtime.go:12-13), so 超时空之战 has nothing to charge.
// Its PVF limits are still catalogued and rendered (mode 3), which is why the
// keys above exist for all three modes.
func currentRaidAdmissionPolicyKeys(ownerMode byte) []string {
	switch ownerMode {
	case runtimeRaidOwnerAnton:
		return []string{
			currentRaidAntonDailyPolicyKey,
			dnfdungeonadmission.AntonWeeklyAccountPolicyKey,
			dnfdungeonadmission.AntonWeeklyCharacterPolicyKey,
		}
	case runtimeRaidOwnerLuke:
		return []string{
			currentRaidLukeDailyPolicyKey,
			dnfdungeonadmission.LukeWeeklyAccountPolicyKey,
			dnfdungeonadmission.LukeWeeklyCharacterPolicyKey,
		}
	case runtimeRaidOwnerFiendWar:
		// 超时空现在真的能开团了，所以它也必须真的计费。此前这里没有 FiendWar
		// 分支 —— 上面的注释当时写的"本服务端只实现安东与卢克两个 owner，超时空
		// 没什么可扣"是事实，现在不成立了。三个 key 与另外两组同形：日次数是
		// 账号级，周次数是账号级 + 角色级两条。
		//
		// 返回值同时是"扣什么"和"显示什么"的唯一来源(currentRaidAdmissionPolicyKeys
		// 被扣费路径与本文件的 op1303 读取路径共用)，所以接上它之后，
		// 选频道界面上的超时空次数与真正生效的限制不可能再各说各话。
		return []string{
			currentRaidFiendWarDailyPolicyKey,
			dnfdungeonadmission.FiendWarWeeklyAccountPolicyKey,
			dnfdungeonadmission.FiendWarWeeklyCharacterPolicyKey,
		}
	default:
		return nil
	}
}

// currentRaidDailyAvailability is one 5-byte row of S2C class0/op1303.
//
// The byte meanings were settled from the client's own reader and from the two
// windows it fills (sub_17B71B0 reads five bytes per row; the window-A tooltip
// formatter 0x017C3180 prints the DAILY pair from the record at +0x14/+0x18 and
// the WEEKLY pair from +0x1c/+0x20, i.e. the five row bytes map one-for-one onto
// those record fields in order):
//
//	byte 0  raid mode (1=安东, 2=卢克, 3=超时空)
//	byte 1  account DAILY used   -> "帐号日入场场数: (limit-used)/limit"
//	byte 2  account DAILY limit
//	byte 3  account WEEKLY used  -> "帐号周入场场数: ..."
//	byte 4  account WEEKLY limit
//
// Field4/Field5 used to be pinned at zero with the comment "the remaining bytes
// stay zero until their meanings have independent current-client evidence".
// The evidence now exists: etc/raid/*.etc [WEEKLY ENTER COUNT] is `30 3` /
// `20 2` / `20 2`, and the shipped client renders exactly `30/30`, `20/20`,
// `20/20` as 帐号周入场场数 while rendering the second operands as window B's
// 角色周入场次数. So bytes 3/4 carry the account weekly pair.
type currentRaidDailyAvailability struct {
	Mode        byte
	Used        byte
	Limit       byte
	WeeklyBonus byte
}

// currentRaidAvailabilityRowSource is one raid mode together with every policy
// its two windows render and the op1206 event id that selects its window element:
//
//   - the account daily pair (op1303 bytes 1/2, live counters),
//   - the account weekly policy whose granted allowance feeds op1303 byte 3,
//   - the account AND character weekly policies whose USED counts are the two
//     numbers op1206 carries (see currentRaidWeeklySchedules).
//
// The limit fallback is the shape-only default used when no catalog is
// available; it is the shipped PVF value.
type currentRaidAvailabilityRowSource struct {
	mode               byte
	eventID            uint32
	dailyAccountKey    string
	weeklyAccountKey   string
	weeklyCharacterKey string
	dailyLimitFallback byte
}

func currentRaidAvailabilityRowSources() []currentRaidAvailabilityRowSource {
	return []currentRaidAvailabilityRowSource{
		{
			mode: currentRaidAntonMode, eventID: currentAntonRaidEventID,
			dailyAccountKey:    currentRaidAntonDailyPolicyKey,
			weeklyAccountKey:   dnfdungeonadmission.AntonWeeklyAccountPolicyKey,
			weeklyCharacterKey: dnfdungeonadmission.AntonWeeklyCharacterPolicyKey,
			dailyLimitFallback: currentRaidAntonDailyLimit,
		},
		{
			mode: currentRaidLukeMode, eventID: currentLukeRaidEventID,
			dailyAccountKey:    currentRaidLukeDailyPolicyKey,
			weeklyAccountKey:   dnfdungeonadmission.LukeWeeklyAccountPolicyKey,
			weeklyCharacterKey: dnfdungeonadmission.LukeWeeklyCharacterPolicyKey,
			dailyLimitFallback: currentRaidLukeDailyLimit,
		},
		{
			mode: currentRaidFiendWarMode, eventID: currentFiendWarEventID,
			dailyAccountKey:    currentRaidFiendWarDailyPolicyKey,
			weeklyAccountKey:   dnfdungeonadmission.FiendWarWeeklyAccountPolicyKey,
			weeklyCharacterKey: dnfdungeonadmission.FiendWarWeeklyCharacterPolicyKey,
			dailyLimitFallback: currentRaidFiendWarDailyLimit,
		},
	}
}

// defaultCurrentRaidDailyAvailabilityRows is the shape-only default used by the
// packet-template path: it carries no account state, so the live sender
// (currentRaidDailyAvailabilityRows) is the only one a player sees.
func defaultCurrentRaidDailyAvailabilityRows() []currentRaidDailyAvailability {
	sources := currentRaidAvailabilityRowSources()
	rows := make([]currentRaidDailyAvailability, 0, len(sources))
	for _, source := range sources {
		rows = append(rows, currentRaidDailyAvailability{
			Mode:  source.mode,
			Limit: source.dailyLimitFallback,
		})
	}
	return rows
}

// currentRaidDailyAvailabilityRows reports the account's live daily AND weekly
// entry counters for the three raid modes.
//
// Live 2026-09-16: the channel-select window showed 入场次数 10/10 while the start
// command was already rejecting with 0xD9, because this packet used to hardcode
// Used=0. It now reads the same policies the start charges, so the number the
// player sees is the number that decides whether the start succeeds.
func (s *Service) currentRaidDailyAvailabilityRows(session *gameSession) []currentRaidDailyAvailability {
	// Test aid: with runtime/state/raid-availability-probe present the three rows
	// carry unmistakable numbers instead of live counters, so the byte-to-slot
	// mapping of both team-mode windows can be read straight off a screenshot.
	// Display only -- enforcement never consults these bytes. See
	// raid_availability_probe.go.
	if probe, probing := currentRaidAvailabilityProbeRows(); probing {
		return probe
	}
	rows := defaultCurrentRaidDailyAvailabilityRows()
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return rows
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return rows
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return rows
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	characterID := fmt.Sprint(session.selectedCharacterID)
	for index, source := range currentRaidAvailabilityRowSources() {
		if index >= len(rows) {
			break
		}
		if used, limit, ok := s.currentRaidAdmissionRowPair(ctx, owner, characterID, source.dailyAccountKey, now); ok {
			rows[index].Used = used
			rows[index].Limit = limit
		}
		if bonus, ok := s.currentRaidAdmissionWeeklyBonus(ctx, owner, characterID, source.weeklyAccountKey, now); ok {
			rows[index].WeeklyBonus = bonus
		}
	}
	return rows
}

// currentRaidAdmissionWeeklyBonus reads the weekly policy's granted allowance
// (Snapshot.Bonus) as the one byte the weekly readout consumes.
//
// It is NOT the used count: the probe of 2026-09-30 proved byte 4 is ADDED to the
// client's own weekly limit (sending 13 next to Anton's limit of 30 rendered
// 43/43, and 26 next to Luke's 20 rendered 46/46), while the daily pair two bytes
// earlier subtracts. dungeonadmission.Snapshot.Bonus is exactly "a bounded,
// service-window-scoped amount added to one authored admission policy"
// (dungeonadmission/types.go:72-79), so that is what belongs here. With no
// allowance granted -- the normal case -- the readout is limit/limit, which is
// precisely what the shipped official screenshots show.
func (s *Service) currentRaidAdmissionWeeklyBonus(
	ctx context.Context,
	owner *dnfdungeonadmission.Owner,
	characterID string,
	policyKey string,
	now time.Time,
) (byte, bool) {
	policy, found, err := s.currentDungeonAdmissionPolicy(policyKey)
	if err != nil || !found {
		return 0, false
	}
	snapshot, ok := currentRaidDailyAvailabilitySnapshot(ctx, owner, characterID, policy, now)
	if !ok {
		return 0, false
	}
	bonus := snapshot.Bonus
	if bonus > 0xff {
		bonus = 0xff
	}
	return byte(bonus), true
}

// currentRaidAdmissionRowPair reads one account-scoped policy as the two bytes a
// row carries. ok is false when the catalog has no such policy or the limit does
// not fit a byte, in which case the caller keeps that row's fallback.
func (s *Service) currentRaidAdmissionRowPair(
	ctx context.Context,
	owner *dnfdungeonadmission.Owner,
	characterID string,
	policyKey string,
	now time.Time,
) (byte, byte, bool) {
	policy, found, err := s.currentDungeonAdmissionPolicy(policyKey)
	if err != nil || !found {
		return 0, 0, false
	}
	snapshot, ok := currentRaidDailyAvailabilitySnapshot(ctx, owner, characterID, policy, now)
	if !ok || snapshot.EffectiveLimit == 0 || snapshot.EffectiveLimit > 0xff {
		return 0, 0, false
	}
	used := snapshot.Used
	if used > 0xff {
		used = 0xff
	}
	return byte(used), byte(snapshot.EffectiveLimit), true
}

// currentRaidDailyAvailabilitySnapshot reads one policy. Preflight reports an
// exhausted policy as a LimitError whose snapshot still carries the used/limit
// pair, and that is exactly the state this viewer has to display.
func currentRaidDailyAvailabilitySnapshot(
	ctx context.Context,
	owner *dnfdungeonadmission.Owner,
	characterID string,
	policy dnfdungeonadmission.Policy,
	now time.Time,
) (dnfdungeonadmission.Snapshot, bool) {
	snapshots, err := owner.Preflight(ctx, characterID, []dnfdungeonadmission.Policy{policy}, now)
	if err == nil {
		if len(snapshots) != 1 {
			return dnfdungeonadmission.Snapshot{}, false
		}
		return snapshots[0], true
	}
	var limitErr *dnfdungeonadmission.LimitError
	if errors.As(err, &limitErr) && limitErr != nil {
		return limitErr.Snapshot, true
	}
	return dnfdungeonadmission.Snapshot{}, false
}

// buildCurrentRaidDailyAvailabilityBody matches sub_17B71B0's fixed loop:
// three five-byte rows indexed by raid mode. Byte one is the mode; the client's
// own reader takes the remaining four as four independent counters, and its
// window-A formatter prints them as the account daily pair followed by the
// account weekly pair -- so the writer order here is load-bearing.
func buildCurrentRaidDailyAvailabilityBody(
	rows []currentRaidDailyAvailability,
) ([]byte, error) {
	if len(rows) != 3 {
		return nil, fmt.Errorf("raid daily availability rows=%d want=3", len(rows))
	}
	var writer packetWriter
	for _, row := range rows {
		if row.Mode == 0 {
			return nil, fmt.Errorf("raid daily availability contains mode 0")
		}
		writer.writeByte(row.Mode)
		writer.writeByte(row.Used)
		writer.writeByte(row.Limit)
		writer.writeByte(row.WeeklyBonus)
		// Byte 5 is inert for both team-mode windows: the live probe of
		// 2026-09-30 sent 130/240/250 in it and not one of those numbers appeared
		// anywhere on screen, while byte 4 alone moved the weekly readout. Its
		// meaning is therefore still unproven and it stays zero, exactly as it was
		// before this packet was touched at all.
		writer.writeByte(0)
	}
	return writer.bytes(), nil
}

func (s *Service) sendCurrentRaidDailyAvailability(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	rows := s.currentRaidDailyAvailabilityRows(session)
	body, err := buildCurrentRaidDailyAvailabilityBody(rows)
	if err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-daily-availability-sent",
		"source", source,
		"msg_id", currentRaidDailyAvailabilityMsgID,
		"classification", 0,
		"anton_mode", rows[0].Mode,
		"anton_today_used", rows[0].Used,
		"anton_daily_limit", rows[0].Limit,
		"luke_mode", rows[1].Mode,
		"luke_today_used", rows[1].Used,
		"luke_daily_limit", rows[1].Limit,
		"fiendwar_mode", rows[2].Mode,
		"fiendwar_today_used", rows[2].Used,
		"fiendwar_daily_limit", rows[2].Limit,
		"body_len", len(body),
		"layout", "three_rows_mode_used_limit_field4_field5")
	return s.sendGameUpperRawClass(session, currentRaidDailyAvailabilityMsgID, body, 0)
}

// currentRaidWeeklySchedule is one op1206 raid-schedule record: the event id that
// selects the window element, plus the three counters that element renders.
type currentRaidWeeklySchedule struct {
	EventID             uint32
	AccountWeeklyUsed   uint32
	CharacterWeeklyUsed uint32
	DayMask             uint32
}

// currentRaidWeeklySchedules reads, per raid mode, the two weekly USED counters
// that decide whether a start is allowed.
//
// These are the ONLY source of 角色周入场次数 / 帐号周入场场数's numerator: the client
// stores them on the per-mode window element (raw[0] -> +0x30, raw[1] -> +0x08)
// and both readouts print (limit - used) against limits it takes from its own
// Script.pvf. Enforcement charges exactly the same two policies
// (currentRaidAdmissionPolicyKeys), so filling them is what makes the number the
// player reads equal the number Owner.Preflight refuses on.
//
// It deliberately returns no error and degrades to the zero default: this is an
// extra projection on the login path, and the pre-fix wire content was all zero,
// so an unreadable catalog must not gain a new way to fail the sequence.
func (s *Service) currentRaidWeeklySchedules(session *gameSession) []currentRaidWeeklySchedule {
	sources := currentRaidAvailabilityRowSources()
	schedules := make([]currentRaidWeeklySchedule, 0, len(sources))
	for _, source := range sources {
		schedules = append(schedules, currentRaidWeeklySchedule{
			EventID: source.eventID,
			DayMask: currentRaidScheduleInitialDayMask,
		})
	}
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return schedules
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return schedules
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return schedules
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	characterID := fmt.Sprint(session.selectedCharacterID)
	for index, source := range sources {
		if index >= len(schedules) {
			break
		}
		if used, ok := s.currentRaidAdmissionPolicyUsed(ctx, owner, characterID, source.weeklyAccountKey, now); ok {
			schedules[index].AccountWeeklyUsed = used
		}
		if used, ok := s.currentRaidAdmissionPolicyUsed(ctx, owner, characterID, source.weeklyCharacterKey, now); ok {
			schedules[index].CharacterWeeklyUsed = used
		}
	}
	return schedules
}

// currentRaidAdmissionPolicyUsed reads one policy's live used count through the
// same Preflight reader the op1303 viewer and the charge use.
func (s *Service) currentRaidAdmissionPolicyUsed(
	ctx context.Context,
	owner *dnfdungeonadmission.Owner,
	characterID string,
	policyKey string,
	now time.Time,
) (uint32, bool) {
	if policyKey == "" {
		return 0, false
	}
	policy, found, err := s.currentDungeonAdmissionPolicy(policyKey)
	if err != nil || !found {
		return 0, false
	}
	snapshot, ok := currentRaidDailyAvailabilitySnapshot(ctx, owner, characterID, policy, now)
	if !ok {
		return 0, false
	}
	return snapshot.Used, true
}

// sendCurrentRaidWeeklySchedules pushes one op1206 record per raid mode. Any
// error is returned, so a caller on an ordered sequence keeps the failure
// semantics it already had.
func (s *Service) sendCurrentRaidWeeklySchedules(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	schedules := s.currentRaidWeeklySchedules(session)
	for _, schedule := range schedules {
		body, err := buildCurrentRaidScheduleProgressBody(schedule.EventID, currentRaidScheduleCounters{
			AccountWeeklyUsed:   schedule.AccountWeeklyUsed,
			CharacterWeeklyUsed: schedule.CharacterWeeklyUsed,
			DayMask:             schedule.DayMask,
		})
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentSpendTimeProgressMsgID, body, 0); err != nil {
			return err
		}
	}
	if len(schedules) != 0 {
		s.logGameEvent(session, "game-raid-weekly-schedule-sent",
			"source", source,
			"msg_id", currentSpendTimeProgressMsgID,
			"classification", 0,
			"anton_event", schedules[0].EventID,
			"anton_account_weekly_used", schedules[0].AccountWeeklyUsed,
			"anton_character_weekly_used", schedules[0].CharacterWeeklyUsed,
			"luke_event", schedules[1].EventID,
			"luke_account_weekly_used", schedules[1].AccountWeeklyUsed,
			"luke_character_weekly_used", schedules[1].CharacterWeeklyUsed,
			"fiendwar_event", schedules[2].EventID,
			"fiendwar_account_weekly_used", schedules[2].AccountWeeklyUsed,
			"fiendwar_character_weekly_used", schedules[2].CharacterWeeklyUsed,
			"layout", "event_then_narrow_then_account_weekly_used_then_character_weekly_used_then_daymask")
	}
	return nil
}

// broadcastCurrentRaidEntryCounters re-pushes BOTH entry-count projections to
// every online member of a raid: the op1303 account rows and the op1206 weekly
// records.
//
// WHY this exists: the client never queries these numbers. Its op1206 handler
// writes them straight onto the per-mode window element (file 0xC39140) and the
// window only re-renders that stored state, so a counter that changes without a
// packet keeps showing its old value until the next relog. Charging a start
// changes the account daily, account weekly and character weekly counters of
// every member at once, which makes this the one moment they all have to be
// re-pushed -- live 2026-09-30: a player started Anton, left the party, reopened
// the entry window and still read the pre-charge numbers.
//
// PLACEMENT: called at the very END of the start command (raid_online.go), after
// the ready/standby/active lifecycle push. The raid-start handshake proper (ACK,
// op14 cost sync, mode3 roster, op633 buff registration, party snapshot, op9/
// op153 frames, ready timer) is a contract this packet must not join -- see
// TestRuntimeRaidStartFreezesOnlineRosterAndOrdersAckBeforeCostSync -- so the
// counters ride the tail instead. The login sequence already interleaves op1206
// between op108 and op1303, so the client tolerates it mid-sequence.
//
// Display only, and intentionally non-fatal: the durable commit and the command
// ACK are already behind us, so like the surrounding post-ack projections a
// refresh failure must never turn a committed start into an error.
func (s *Service) broadcastCurrentRaidEntryCounters(state runtimeRaidState, source string) {
	for _, member := range state.Members {
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendCurrentRaidDailyAvailability(session, source); err != nil {
			s.logGameEvent(session, "game-raid-entry-counter-projection-deferred",
				"source", source, "stage", "daily", "char_id", member.CharID, "error", err)
		}
		if err := s.sendCurrentRaidWeeklySchedules(session, source); err != nil {
			s.logGameEvent(session, "game-raid-entry-counter-projection-deferred",
				"source", source, "stage", "weekly", "char_id", member.CharID, "error", err)
		}
	}
}

// REFRESH POINTS for the weekly records are now four, and deliberately no more:
// the login sequence, the 06:00 service-window reset, the tail of a successful
// raid start, and the tail of a FAILED raid run -- the last two are where they
// actually change mid-session: the start charges all three counters and the
// failure refund (raid_entry_refund.go) gives them back.
//
// Known remaining gap: a start that is REJECTED charges nothing, so nothing needs
// re-pushing; and a member who was offline at charge or refund time picks the
// fresh numbers up on their next login.
