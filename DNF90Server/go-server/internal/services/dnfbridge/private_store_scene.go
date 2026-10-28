package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// currentSendPrivateStoreActorRefresh reprojects the current actor after a
// contract purchase. class0/op66 updates the premium manager, but the idle
// town companion permission is consumed from mode0 actor initialization.
// This is an existing-actor refresh, so suppress the generation-only actor
// state event for both the owner and every committed same-area peer.
func (s *Service) currentSendPrivateStoreActorRefresh(ctx context.Context, session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if session.dungeon.runtime != nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	charID := session.selectedCharacterID
	loadedID, charName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if loadedID != charID || !hasCharacter {
		return fmt.Errorf("load selected private-store actor: requested=%d loaded=%d found=%t", charID, loadedID, hasCharacter)
	}
	ownerChannel := currentTownActorOwnerContext(session)
	body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx, session, charID, charName, character, true, ownerChannel,
	)
	if err != nil {
		return fmt.Errorf("build selected private-store mode0 refresh: %w", err)
	}
	permission, permissionOK := currentSceneObjectTailPrivateStorePermissionForLog(body)
	if !permissionOK || permission != 1 {
		return fmt.Errorf("private-store mode0 permission=%d ok=%t", permission, permissionOK)
	}
	if !suppressCurrentSceneObjectActorStateEvent(body) {
		return fmt.Errorf("suppress selected private-store actor state event")
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return err
	}

	peerCount := 0
	if s.onlinePlayers != nil {
		for _, peer := range s.onlinePlayers.PeersInSameArea(charID) {
			if peer.Session == nil || peer.Session.conn == nil {
				continue
			}
			peerOwner := currentTownRemoteActorOwnerContext(peer.Session, charID)
			if peerOwner == currentSceneObjectContext {
				continue
			}
			peerBody, peerErr := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
				ctx, session, charID, charName, character, true, peerOwner,
			)
			if peerErr == nil && !suppressCurrentSceneObjectActorStateEvent(peerBody) {
				peerErr = fmt.Errorf("suppress peer private-store actor state event")
			}
			if peerErr == nil {
				peerErr = s.sendGameUpperRawClass(peer.Session, uint16(dnfenum.CmdPacketSetUDPIPPort), peerBody, 0)
			}
			if peerErr != nil {
				s.logWarn("dnfbridge deferred private-store peer actor refresh",
					"actor_char_id", charID,
					"peer_char_id", peer.CharacterID,
					"error", peerErr)
				continue
			}
			peerCount++
		}
	}

	s.logGameEvent(session, "game-private-store-actor-refresh-sent",
		"source", source,
		"char_id", charID,
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"mode", 0,
		"owner_channel", ownerChannel,
		"private_store_permission", permission,
		"peer_refresh_count", peerCount,
		"body_source", "current_exe_mode0_v116_private_store_permission")
	return nil
}
