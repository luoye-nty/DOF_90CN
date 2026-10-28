package dnfbridge

import (
	"context"
	"fmt"
	"time"
)

// currentClearQuestListHUDReadyReplayDelays are the offsets after the HUD-ready
// task refresh at which the authoritative task trio (op574 -> op356 -> op21) is
// replayed. Three offsets, because the client rebuild that loses the completed
// table is not at a fixed moment (see below), and each replay is idempotent.
//
// The current EXE rebuilds the selected character's quest state late in the
// loading route, and that rebuild drops every row op356 wrote before it. A login
// whose selection ACK carries no in-progress quest row (all thirty 0xFFFF slots)
// ends up with an EMPTY completed-quest table, and the client builds the
// team-mode channel row from exactly that table (sub_1A028C0(questMgr, 12829)),
// so the row renders as "prerequisite not met" (gray) until something replays
// op356. Live 2026-09-22, character 9999 (char_id=22), freshly started client:
//
//	07:33:14.549  client dispatches op356 (554 rows)  -> table = 554
//	07:33:14.581  client dispatches op356 again       -> table = 554
//	07:33:14.679  server sends the HUD-ready op21/op574 refresh
//	07:33:14.697  table = 0                           (rebuilt away)
//
// The player-visible workaround was to change town: a town transition replays
// op356 long after that rebuild, which is why 切一次城镇地图就好.
//
// The rebuild that drops the pre-ready rows is not at a fixed delay: two logins
// measured 116 ms and ~470 ms after the op356 write, so a single replay can land
// inside it on a slower client. Replaying the same idempotent trio three times
// over the first seconds means the last write always observes a settled table,
// and the first one is early enough that a channel window opened right after
// login already sees the filled table (the client builds its channel rows when
// it (re)applies the channel directory, i.e. at login, when the window is
// opened, and on the client's own later refreshes - not when a quest packet
// arrives, which is why a filled table alone used to look like it did nothing).
//
// Live 2026-09-22, character 9999 (char_id=22, no quest in progress), pid 16780:
//
//	07:51:34.164/262  op356 (554 rows) written, table = 554
//	07:51:34.876      table = 0                     (client rebuild)
//	07:51:35.025      client re-applied the channel directory -> rows built GRAY
//	07:51:36.029      replay delivered, table = 554  (rows stayed stale)
//	07:51:45.087      client re-applied again -> rows built from the filled table
var currentClearQuestListHUDReadyReplayDelays = []time.Duration{
	700 * time.Millisecond,
	1600 * time.Millisecond,
	3500 * time.Millisecond,
}

// currentClearQuestListHUDReadyReplayDelayMillis reports the replay offsets in
// milliseconds, so the armed log's delays_ms field and its value agree.
func currentClearQuestListHUDReadyReplayDelayMillis() []int64 {
	millis := make([]int64, 0, len(currentClearQuestListHUDReadyReplayDelays))
	for _, delay := range currentClearQuestListHUDReadyReplayDelays {
		millis = append(millis, delay.Milliseconds())
	}
	return millis
}

const currentClearQuestListHUDReadyReplayTimerPrefix = "dnf-clear-quest-hud-ready"

func currentClearQuestListHUDReadyReplayTimerName(
	session *gameSession,
	characterID uint16,
	index int,
) string {
	if session == nil {
		return currentClearQuestListHUDReadyReplayTimerPrefix
	}
	return fmt.Sprintf(
		"%s:%d:%s:%d",
		currentClearQuestListHUDReadyReplayTimerPrefix,
		index,
		session.connID,
		characterID,
	)
}

// scheduleCurrentClearQuestListReplayAfterHUDReady arms the one-shot replays on
// the shared gameplay queue. The caller passes the character identity it already
// captured under the session lock, so every timer is bound to exactly the
// character the HUD-ready refresh was sent for. A replay is an ordering repair,
// never a state change: it re-sends the persisted table the character already
// owns and the projection never mutates quest persistence. A missing queue only
// restores the previous behaviour (the pre-rebuild write stays as the last word).
func (s *Service) scheduleCurrentClearQuestListReplayAfterHUDReady(
	session *gameSession,
	characterID uint16,
	characterGeneration uint64,
	source string,
) {
	if s == nil || session == nil || characterID == 0 {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	armed := 0
	for index, delay := range currentClearQuestListHUDReadyReplayDelays {
		replayDelay := delay
		timerName := currentClearQuestListHUDReadyReplayTimerName(session, characterID, index)
		if err := queue.ScheduleAfter(timerName, replayDelay, func(time.Time) {
			s.replayCurrentClearQuestListAfterHUDReady(
				session,
				source,
				characterID,
				characterGeneration,
				replayDelay,
			)
		}); err != nil {
			s.logGameEvent(session, "game-clear-quest-list-hud-ready-replay-schedule-failed",
				"source", source,
				"char_id", characterID,
				"timer_name", timerName,
				"delay_ms", replayDelay.Milliseconds(),
				"error", err)
			continue
		}
		armed++
	}
	if armed == 0 {
		return
	}
	s.logGameEvent(session, "game-clear-quest-list-hud-ready-replay-armed",
		"source", source,
		"char_id", characterID,
		"delays_ms", currentClearQuestListHUDReadyReplayDelayMillis(),
		"armed", armed,
		"reason", "current_client_rebuilds_the_completed_quest_table_late_in_the_loading_route")
}

// replayCurrentClearQuestListAfterHUDReady re-sends the authoritative task trio
// (op574 active snapshot -> op356 completed bitmap -> op21 acceptable list) once
// the loading route is over.
//
// The bitmap alone is NOT enough: the client only rebuilds the team-mode channel
// row when it re-requests the channel directory, and a bare op356 does not make
// it do that. Live 2026-09-22 (pid 16780): the replay at 07:51:36.029 refilled
// the table to 554 and the row stayed gray until the player accepted a quest at
// 07:51:44.309 - the client answered THAT with an off-cadence channel-directory
// request at 07:51:45.066 (its own heartbeat was strictly 10 s: ...41.559,
// 51.559) and only then rebuilt the row from the filled table. The accept path
// publishes exactly op574 -> op356 -> op21 (the same trio the GM refresh uses),
// so the replay publishes it too; the client then re-requests the directory by
// itself and the row is rebuilt from the filled table within ~1 s.
//
// The task is submitted through the session event loop, which drops it when the
// selected character or its generation changed in the meantime, so a relog or
// character switch makes it a no-op.
func (s *Service) replayCurrentClearQuestListAfterHUDReady(
	session *gameSession,
	source string,
	characterID uint16,
	characterGeneration uint64,
	delay time.Duration,
) {
	if s == nil || session == nil {
		return
	}
	// The session event loop drops stale tasks as well; this explicit check keeps
	// the same guarantee on the paths that run without a loop (and in tests), and
	// names the reason in the log.
	if session.selectedCharacterID != characterID || session.characterGeneration != characterGeneration {
		s.logGameEvent(session, "game-clear-quest-list-hud-ready-replay-stale",
			"source", source,
			"event_char_id", characterID,
			"selected_char_id", session.selectedCharacterID,
			"event_character_generation", characterGeneration,
			"selected_character_generation", session.characterGeneration)
		return
	}
	err := s.postGameSessionCharacterEvent(
		session,
		"clear_quest_list_hud_ready_replay",
		characterID,
		characterGeneration,
		func() error {
			replaySource := source + "_replay_after_hud_ready"
			repositories, ok := s.repositoryGroup()
			if !ok || repositories.Quest == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			defer cancel()
			// The active-quest snapshot comes first for the same reason the GM
			// post-action refresh sends it first: it is the row the task tracker
			// owns, and the completed bitmap that follows must not be dropped by a
			// later tracker rebuild.
			if err := s.sendCurrentActiveQuestSnapshotForSession(session, replaySource+"_active_op574"); err != nil {
				return err
			}
			sent, err := s.sendCurrentPersistedClearQuestListIfCompleted(
				ctx,
				session,
				repositories,
				replaySource+"_completed_op356",
			)
			if err != nil {
				return err
			}
			// The acceptable list closes the trio. The client treats this pair as
			// an authoritative task refresh and answers it by re-requesting the
			// channel directory, which is what actually rebuilds the team-mode
			// channel row.
			if err := s.sendCurrentAcceptableQuestListOnlyForSession(session, replaySource+"_acceptable_op21"); err != nil {
				return err
			}
			s.logGameEvent(session, "game-clear-quest-list-hud-ready-replay-sent",
				"source", source,
				"char_id", characterID,
				"msg_id", currentClearQuestListMsgID,
				"classification", 0,
				"sent", sent,
				"delay_ms", delay.Milliseconds(),
				"sequence", "active_op574_then_completed_op356_then_acceptable_op21",
				"reason", "replay_after_the_late_completed_quest_table_rebuild")
			return nil
		},
	)
	if err != nil && !isClosedGameSessionEventError(err) {
		s.logGameEvent(session, "game-clear-quest-list-hud-ready-replay-failed",
			"source", source,
			"char_id", characterID,
			"error", err)
	}
}
