package dnfbridge

import (
	"context"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func buildCurrentGuildAuraBody(characterID uint16, guild *dnfguild.Guild, ownerChannel byte) []byte {
	var writer packetWriter
	writer.writeByte(6)
	writer.writeUint16(1)
	writer.writeByte(currentSceneObjectRoute)
	writer.writeByte(ownerChannel)
	writer.writeUint16(characterID)
	// sub_2005AE0 -> template+1493 -> sub_2408B50. Preserve the native
	// constructor defaults for the other decorations in this 25-byte block.
	writer.writeUint32(1)
	writer.writeUint32(0)
	frame, mark := uint32(0xffffffff), uint32(0xffffffff)
	if guild != nil {
		frame, mark = guild.AuraFrame, guild.AuraMark
	}
	writer.writeUint32(frame)
	writer.writeUint32(mark)
	writer.writeUint32(0xffffffff)
	writer.writeByte(0)
	writer.writeUint32(0)
	return writer.bytes()
}

// Call only after the target has created this actor with mode0/mode1.
func (s *Service) writeCurrentGuildAuraState(target *gameSession, characterID uint16, guild *dnfguild.Guild, ownerChannel byte) error {
	body := buildCurrentGuildAuraBody(characterID, guild, ownerChannel)
	if err := s.sendGameUpperRawClass(target, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return err
	}
	s.logGameEvent(target, "game-guild-actor-aura-sent", "actor_char_id", characterID,
		"owner_channel", ownerChannel, "appearance_body", body[7:])
	return nil
}

func (s *Service) sendCurrentGuildAuraForCharacter(ctx context.Context, target *gameSession, characterID uint16, ownerChannel byte) error {
	var guild *dnfguild.Guild
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, _ = registry.GuildForCharacter(strconv.Itoa(int(characterID)))
		return nil
	}); err != nil {
		return err
	}
	if guild == nil {
		return nil // A newly constructed nonmember already has the native -1 indices.
	}
	return s.writeCurrentGuildAuraState(target, characterID, guild, ownerChannel)
}

// currentGuildForCharacterID resolves the guild one character belongs to, or nil
// when that character is in no guild. It is the by-id form of
// currentGuildForSelectedCharacter, for callers that hold a character id rather
// than a session -- notably the actor-appearance fan-outs, which resolve the
// ACTOR's guild once and reuse it for every receiver.
func (s *Service) currentGuildForCharacterID(ctx context.Context, characterID uint16) (*dnfguild.Guild, error) {
	var guild *dnfguild.Guild
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, _ = registry.GuildForCharacter(strconv.Itoa(int(characterID)))
		return nil
	}); err != nil {
		return nil, err
	}
	return guild, nil
}

// sendCurrentGuildAurasForCharacters is the batched form of
// sendCurrentGuildAuraForCharacter: one registry read for the whole roster
// instead of one read per member.
//
// The party/raid scene projection replays this for every member of the roster,
// so a per-member read multiplied the repository traffic by the roster size --
// N reads per member, N^2 for the projection. stress_raid_projection_test.go
// caps the reads of one projection, and a 40-member raid blew through that cap,
// which is how this was caught.
func (s *Service) sendCurrentGuildAurasForCharacters(ctx context.Context, target *gameSession, characterIDs []uint16, ownerChannel byte) error {
	if len(characterIDs) == 0 {
		return nil
	}
	guilds := make(map[uint16]*dnfguild.Guild, len(characterIDs))
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		for _, characterID := range characterIDs {
			guild, _, _ := registry.GuildForCharacter(strconv.Itoa(int(characterID)))
			guilds[characterID] = guild
		}
		return nil
	}); err != nil {
		return err
	}
	for _, characterID := range characterIDs {
		guild := guilds[characterID]
		if guild == nil {
			continue // Nonmembers already carry the native -1 indices.
		}
		if err := s.writeCurrentGuildAuraState(target, characterID, guild, ownerChannel); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) publishCurrentGuildAuraToTownPeers(actor *gameSession, guild *dnfguild.Guild) {
	if s.onlinePlayers == nil {
		return
	}
	for _, peer := range s.onlinePlayers.PeersInSameArea(actor.selectedCharacterID) {
		if peer.Session == nil || peer.Session.conn == nil {
			continue
		}
		// Peer-safe read only. The upstream branch blocks here, which is a live
		// deadlock in this tree: sendCurrentGuildManagementState is reached from
		// the login route while the *actor's* own townMu is already held
		// (initial_town_route_progress.go -> publishCurrentGuildPresence ->
		// sendCurrentGuildManagementState), so waiting on a peer's town lock
		// inverts the lock order between two members signing in at the same
		// time -- the 2026-09-22 20:06 incident that wedged sessions 1111 and
		// 2222 for six minutes. See sendCurrentGuildManagementStateToPeer.
		ready, source, busy := s.currentSceneTransitionReadyForStateNonBlocking(peer.Session)
		if busy {
			s.logGameEvent(peer.Session, "game-guild-peer-aura-refresh-deferred",
				"reason", source, "actor_char_id", actor.selectedCharacterID)
			continue
		}
		if !ready {
			continue
		}
		owner := currentTownRemoteActorOwnerContext(peer.Session, actor.selectedCharacterID)
		if owner == currentSceneObjectContext {
			continue
		}
		if err := s.writeCurrentGuildAuraState(peer.Session, actor.selectedCharacterID, guild, owner); err != nil {
			s.logGameEvent(peer.Session, "game-guild-peer-aura-refresh-failed", "actor_char_id", actor.selectedCharacterID, "error", err)
		}
	}
}
