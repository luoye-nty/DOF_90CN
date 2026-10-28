package dnfbridge

func (s *Service) sendSelectDungeonFatigueFailure(
	session *gameSession,
	reason byte,
) error {
	if err := s.sendSelectDungeonFailure(session, reason); err != nil {
		return err
	}
	scope := "solo"
	if reason == currentSelectDungeonPartyFatigueExhaustedReason {
		scope = "party"
	}
	s.logGameEvent(session, "game-dungeon-entry-fatigue-prompt-sent",
		"char_id", session.selectedCharacterID,
		"transport", "class1_op16_text_free_selector_prompt",
		"reason", reason,
		"scope", scope,
		"ordering", "native_selector_restore_then_native_selector_and_chat")
	return nil
}
