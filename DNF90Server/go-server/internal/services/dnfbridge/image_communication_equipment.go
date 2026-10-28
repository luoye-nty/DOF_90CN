package dnfbridge

import (
	"context"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const currentImageCommunicationEquipmentMsgID = uint16(dnfenum.CmdPacketImageCommunicationEquipmentUse)

func buildCurrentImageCommunicationEquipmentSuccessBody(actorObjectKey uint32, npcID uint32) []byte {
	var writer packetWriter
	writer.writeUint32(actorObjectKey)
	writer.writeUint32(npcID)
	return writer.bytes()
}

func buildCurrentImageCommunicationEquipmentActorNotifyBody(actorObjectKey uint32) []byte {
	var writer packetWriter
	writer.writeUint32(actorObjectKey)
	return writer.bytes()
}

// handleCurrentImageCommunicationEquipmentUse owns the current client's
// bodyless op475 request. Its direction-dependent handlers have two distinct
// bodies: class1 sub_1D0DF30 consumes actor-object u32 + remote-NPC u32 and
// opens the communication UI with the latter; class0 sub_1D55C10 consumes the
// actor-object u32 and enables the matching type-0x821 town actor animation.
func (s *Service) handleCurrentImageCommunicationEquipmentUse(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if len(body) != 0 {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"body_len", len(body),
			"reason", "current_exe_op475_requires_bodyless_request")
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"reason", "quest_repository_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"reason", "quest_catalog_unavailable",
			"error", err)
		return nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "quest_record_load_failed",
			"error", err)
		return nil
	}
	if !found || strings.TrimSpace(record.CharacterID) != characterID {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"char_id", session.selectedCharacterID,
			"quest_record_found", found,
			"record_character_id", record.CharacterID,
			"reason", "selected_character_quest_record_unavailable")
		return nil
	}
	target, resolved := catalog.ActiveImageCommunicationTarget(record)
	if !resolved {
		s.logGameEvent(session, "game-image-communication-equipment-use-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "active_image_communication_quest_missing_or_ambiguous")
		return nil
	}
	actorObjectKey := uint32(currentSceneActorObjectKey(session.selectedCharacterID))
	successBody := buildCurrentImageCommunicationEquipmentSuccessBody(actorObjectKey, target.NPCID)
	actorNotifyBody := buildCurrentImageCommunicationEquipmentActorNotifyBody(actorObjectKey)
	s.logGameEvent(session, "game-image-communication-equipment-use-accepted",
		"char_id", session.selectedCharacterID,
		"quest_id", target.QuestID,
		"npc_id", target.NPCID,
		"actor_object_key", actorObjectKey,
		"response_msg_id", currentImageCommunicationEquipmentMsgID,
		"response_sequence", "class1_success_then_class0_actor_notify",
		"success_body_source", "current_exe_sub_1D0DF30_u32_actor_object_key_u32_remote_npc_index",
		"notify_body_source", "current_exe_sub_1D55C10_u32_actor_object_key",
		"quest_mutation", "none")
	if err := s.sendGameUpperSuccess(session, currentImageCommunicationEquipmentMsgID, successBody); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, currentImageCommunicationEquipmentMsgID, actorNotifyBody, 0)
}
