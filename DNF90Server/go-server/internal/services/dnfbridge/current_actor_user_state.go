package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Current NoPack registers class0/op357 at sub_1D7F3B2 and handles the
	// exact u16 actor-key + u8 state-bits body in sub_1D56150.
	currentActorUserStateMsgID       uint16 = 357
	currentActorDefaultUserStateBits byte   = 3
)

func clearCurrentActorUserStateRestoreSnapshot(session *gameSession) {
	if session == nil {
		return
	}
	session.initialTownActorUserStateRestoreCharacterID = 0
}

func buildCurrentActorUserStateBody(actorKey uint16, stateBits byte) []byte {
	body := make([]byte, 3)
	binary.LittleEndian.PutUint16(body[0:2], actorKey)
	body[2] = stateBits
	return body
}

// sendSelectedActorUserStateRefresh reapplies the repository-backed visibility
// flags to the already-bound live actor. Current NoPack's sub_2690230 does not
// treat an unchanged value as a no-op: when the actor model owner exists it
// detaches and reinserts the visible equipment/avatar components. This is the
// native redraw boundary after op19; unlike class0/op2 mode0 it never replaces
// the actor object and therefore preserves the town camera binding.
func (s *Service) sendSelectedActorUserStateRefresh(session *gameSession, source string) error {
	return s.sendSelectedActorUserStateRefreshWithPolicy(session, source, true)
}

// sendSelectedActorUserStateRefreshIfNonDefault is used after an actor rebuild.
// The mode0/mode1 actor snapshot already carries the native default value, so
// only a persisted non-default byte needs the extra op357 redraw. Direct option
// edits use the forced variant above so toggling back to the default applies
// immediately in the current scene.
func (s *Service) sendSelectedActorUserStateRefreshIfNonDefault(session *gameSession, source string) error {
	return s.sendSelectedActorUserStateRefreshWithPolicy(session, source, false)
}

func (s *Service) sendSelectedActorUserStateRefreshWithPolicy(session *gameSession, source string, force bool) error {
	return s.sendActorUserStateRefreshWithPolicy(session, session, source, force)
}

// target receives the visibility of actor, including when a PVP peer has just
// been bound to the native room. Reload the actor's persisted option instead of
// retaining an entry-time copy that becomes stale after an option edit.
func (s *Service) sendActorUserStateRefreshWithPolicy(target, actor *gameSession, source string, force bool) error {
	if target == nil || actor == nil || actor.selectedCharacterID == 0 {
		return fmt.Errorf("selected actor user state requires an active character")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return fmt.Errorf("selected actor user state repository is unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(actor.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("load selected actor user state character %s: %w", characterID, err)
	}
	if !found {
		return fmt.Errorf("selected actor user state character %s was not found", characterID)
	}
	stateBits := currentActorDefaultUserStateBits
	if character.Stats != nil {
		if value, exists := character.Stats["user_state_bits"]; exists {
			if value < 0 || value > 0xff {
				return fmt.Errorf("selected actor user state bits %d are outside u8", value)
			}
			stateBits = byte(value)
		}
	}
	if !force && stateBits == currentActorDefaultUserStateBits {
		return nil
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(actor)); accountID != "" &&
		accountID != strings.TrimSpace(character.AccountID) {
		return fmt.Errorf("selected actor user state character %s owner mismatch", characterID)
	}
	actorKey := currentSceneActorObjectKey(actor.selectedCharacterID)
	body := buildCurrentActorUserStateBody(actorKey, stateBits)
	s.logPacketEvent("game-upper-selected-actor-user-state-refresh-send",
		"conn_id", target.connID,
		"channel_id", target.channel.ID,
		"source", source,
		"char_id", actor.selectedCharacterID,
		"actor_object_key", actorKey,
		"msg_id", currentActorUserStateMsgID,
		"classification", 0,
		"user_state_bits", stateBits,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D56150_u16_actor_key_u8_state_bits_sub_2690230_native_redraw")
	return s.sendGameUpperRawClass(target, currentActorUserStateMsgID, body, 0)
}

// sendInitialTownActorUserStateAfterHUDReadyOnce reapplies the persisted model
// visibility byte after the current client has recreated its selected town
// actor. The game-option table restores the checkbox, while op357 is the
// separate native model-redraw input.
func (s *Service) sendInitialTownActorUserStateAfterHUDReadyOnce(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}

	session.townMu.Lock()
	characterID := session.selectedCharacterID
	ready := characterID != 0 &&
		session.initialTownRouteCharacterID == characterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent &&
		session.initialTownLegacySceneReadyAccepted &&
		!session.sceneBootstrapTailDeferred &&
		session.sceneBootstrapTailSent &&
		session.initialTownActorUserStateRestoreCharacterID != characterID
	session.townMu.Unlock()
	if !ready || currentDungeonSceneActive(session) {
		return nil
	}

	if err := s.sendSelectedActorUserStateRefreshIfNonDefault(session, source); err != nil {
		return err
	}

	session.townMu.Lock()
	defer session.townMu.Unlock()
	if session.selectedCharacterID != characterID ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRoutePlayerStateSent ||
		!session.initialTownLegacySceneReadyAccepted ||
		session.sceneBootstrapTailDeferred ||
		!session.sceneBootstrapTailSent {
		return nil
	}
	session.initialTownActorUserStateRestoreCharacterID = characterID
	return nil
}
