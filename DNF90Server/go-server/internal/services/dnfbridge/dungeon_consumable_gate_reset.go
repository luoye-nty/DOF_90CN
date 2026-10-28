package dnfbridge

// The current 90CN client reuses numeric opcode 42 in opposite directions:
// class-1 C2S op42 is GiveupGame, while empty class-0 S2C op42 calls
// sub_1CF4FB0 and clears the native dungeon object's item-use wait byte through
// sub_26906D0(object, 0). Keep this wire constant separate from the C2S enum so
// the two meanings cannot be conflated by a later protocol cleanup.
const currentDungeonConsumableUseGateResetWireOpcode uint16 = 42

func (s *Service) sendCurrentConsumableUseGateResetPacket(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonConsumableUseGateResetWireOpcode,
		nil,
		0,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-dungeon-consumable-use-gate-reset-sent",
		"char_id", session.selectedCharacterID,
		"source", source,
		"msg_id", currentDungeonConsumableUseGateResetWireOpcode,
		"classification", 0,
		"body_len", 0,
		"body_source", "current_exe_class0_op42_sub_1CF4FB0_calls_sub_26906D0_object_gate_zero")
	return nil
}

// sendCurrentDungeonEntryConsumableUseGateReset clears the client gate once
// for each newly committed dungeon generation. A failed socket write leaves
// the generation armed, so a repeated finish-loading request retries only the
// missing reset. Room transitions within that generation do not replay it.
func (s *Service) sendCurrentDungeonEntryConsumableUseGateReset(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.consumableUseGateResetSent {
		return nil
	}
	if err := s.sendCurrentConsumableUseGateResetPacket(session, source); err != nil {
		return err
	}
	runtime.consumableUseGateResetSent = true
	return nil
}
