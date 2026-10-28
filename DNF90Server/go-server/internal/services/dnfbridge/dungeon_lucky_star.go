package dnfbridge

// sendSuitableDungeonLuckyStarState publishes the already committed PVF
// rental-wallet result. The durable award is performed inside the settlement
// transaction using its CompletionKey. The client must see op1000 before
// op985 because op985 replaces the cached wallet balance.
func (s *Service) sendSuitableDungeonLuckyStarState(session *gameSession, runtime *runtimeDungeonState) {
	if session == nil || runtime == nil || runtime.Dungeon.ID <= 0 ||
		!runtime.settlementLuckyStarResolved {
		return
	}
	if !runtime.settlementLuckyStarRefreshPending ||
		(runtime.settlementLuckyStarNoticeSent && runtime.settlementLuckyStarStateSent) {
		return
	}
	if runtime.settlementLuckyStarLimit == 0 ||
		runtime.settlementLuckyStarBefore > runtime.settlementLuckyStarAfter ||
		runtime.settlementLuckyStarApplied != runtime.settlementLuckyStarAfter-runtime.settlementLuckyStarBefore ||
		runtime.settlementLuckyStarAfter > runtime.settlementLuckyStarLimit {
		s.logGameEvent(session, "game-dungeon-lucky-star-state-send-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"points_before", runtime.settlementLuckyStarBefore,
			"points_applied", runtime.settlementLuckyStarApplied,
			"points_after", runtime.settlementLuckyStarAfter,
			"point_limit", runtime.settlementLuckyStarLimit,
			"reason", "committed_lucky_star_result_invalid")
		return
	}
	if !runtime.settlementLuckyStarNoticeSent {
		body := buildCurrentRentalPointResultBody(
			currentRentalPointSourceDungeonClear,
			runtime.settlementLuckyStarAfter == runtime.settlementLuckyStarLimit,
			runtime.settlementLuckyStarAfter,
		)
		if err := s.sendGameUpperSuccess(session, currentRentalPointResultMsgID, body); err != nil {
			s.logGameEvent(session, "game-dungeon-lucky-star-notice-send-failed",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"msg_id", currentRentalPointResultMsgID,
				"reason", err)
			return
		}
		runtime.settlementLuckyStarNoticeSent = true
		s.logGameEvent(session, "game-dungeon-lucky-star-notice-sent",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"msg_id", currentRentalPointResultMsgID,
			"source_type", currentRentalPointSourceDungeonClear,
			"limit_flag", runtime.settlementLuckyStarAfter == runtime.settlementLuckyStarLimit,
			"points_before", runtime.settlementLuckyStarBefore,
			"points_applied", runtime.settlementLuckyStarApplied,
			"points_after", runtime.settlementLuckyStarAfter)
	}
	if runtime.settlementLuckyStarStateSent {
		return
	}
	sent, err := s.sendSelectedCurrentRentalPointState(session, "dungeon_settlement_after_lucky_star", true)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-lucky-star-state-send-failed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"msg_id", currentRentalStateMsgID,
			"reason", err)
		return
	}
	if !sent {
		s.logGameEvent(session, "game-dungeon-lucky-star-state-send-deferred",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"msg_id", currentRentalStateMsgID)
		return
	}
	runtime.settlementLuckyStarStateSent = true
}
