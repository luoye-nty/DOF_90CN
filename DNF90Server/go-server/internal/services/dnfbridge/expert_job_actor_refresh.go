package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentSendExpertJobActorRefresh republishes the current actor's mode-0 record
// so the client picks up a sub-profession change immediately.
//
// Why this is required, and why it must happen BEFORE op205:
//
// The mode-0 record is the only packet that carries the sub-profession: current
// EXE sub_2009160 -> sub_2003F20 reads one u8 (type) plus one u32 (experience)
// and stores the type at the actor profile record's +0x368. op205 itself never
// writes that field - its handler only keeps byte0 in the native manager and uses
// the type as a switch discriminant.
//
// The op205 continuation for professions 2 and 4 (sub_322E590) starts with
//
//	iVar = FUN_03a4c1a0();
//	if (*(int *)(iVar + 0x368) == 0) goto skip;   // nothing is built while it is zero
//
// so an acquisition announced mid-session lands on a record that still says "no
// profession" and the client builds neither the profession UI nor its acquisition
// feedback - the state only appeared after a relogin, when mode 0 ran before op205.
// Refreshing the record first makes the announcement work the way the login path
// already does.
//
// A give-up needs the same refresh with type 0: it clears +0x368, which is also
// what lets a later re-acquisition be a real 0 -> N transition again.
func (s *Service) currentSendExpertJobActorRefresh(ctx context.Context, session *gameSession, character dnfrepo.CharacterRecord) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if character.CharacterID == "" {
		// Reload the durable record so the refresh always projects the committed
		// state (a give-up calls this with an empty record on purpose).
		loadedID, _, loaded, hasCharacter := s.selectedCharacterForEnter(ctx, session)
		if loadedID != session.selectedCharacterID || !hasCharacter {
			return nil
		}
		character = loaded
	}
	charName := character.Name

	body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx,
		session,
		session.selectedCharacterID,
		charName,
		character,
		true,
		currentTownActorOwnerContext(session),
	)
	if err != nil {
		return fmt.Errorf("build expert-job actor refresh: %w", err)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return err
	}

	// Same-area peers hold this character's actor too, so their copies of the
	// record need the same profession state; otherwise the profession row stays
	// stale for everyone else until they re-enter the area.
	peerCount := 0
	if s.onlinePlayers != nil {
		charID := session.selectedCharacterID
		for _, peer := range s.onlinePlayers.PeersInSameArea(charID) {
			if peer.Session == nil || peer.Session.conn == nil {
				continue
			}
			peerOwner := currentTownRemoteActorOwnerContext(peer.Session, charID)
			if peerOwner == currentSceneObjectContext {
				continue
			}
			peerBody, peerErr := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
				ctx,
				session,
				charID,
				charName,
				character,
				true,
				peerOwner,
			)
			if peerErr == nil {
				peerErr = s.sendGameUpperRawClass(peer.Session, uint16(dnfenum.CmdPacketSetUDPIPPort), peerBody, 0)
			}
			if peerErr != nil {
				continue
			}
			peerCount++
		}
	}

	s.logGameEvent(session, "game-expert-job-actor-record-refresh-sent",
		"char_id", session.selectedCharacterID,
		"expert_job_type", currentSceneExpertJobType(character, true),
		"expert_job_exp", currentSceneExpertJobExperience(character, true),
		"peer_count", peerCount,
		"body_len", len(body),
		"reason", "mode0_is_the_only_carrier_of_the_actor_sub_profession_field")
	return nil
}
