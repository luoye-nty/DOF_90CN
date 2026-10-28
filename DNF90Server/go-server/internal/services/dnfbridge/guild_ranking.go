package dnfbridge

import (
	"encoding/binary"
	"strconv"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// DLL-private 413 subtype4, version1. The native guild_ranking label has no
// packet setter; preserve op70's documented fields and send a separate value.
func buildCurrentGuildRankingBody(characterID uint16, rank, experience uint32) []byte {
	b := make([]byte, 12)
	b[0], b[1] = 4, 1
	binary.LittleEndian.PutUint16(b[2:], characterID)
	binary.LittleEndian.PutUint32(b[4:], rank)
	binary.LittleEndian.PutUint32(b[8:], experience)
	return b
}

func (s *Service) writeCurrentGuildRanking(session *gameSession, guild *dnfguild.Guild) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	var rank, experience uint32
	if guild != nil {
		if repos, ok := s.repositoryGroup(); !ok || repos.Settings == nil || repos.Character == nil {
			return nil
		}
		ctx, cancel := s.currentGuildContext()
		defer cancel()
		// Deliberate divergence from the upstream branch, which blocks here.
		// This projection runs at the tail of writeCurrentGuildManagementState,
		// which several guild handlers reach with guildMu already held; a
		// blocking read on that path is the 2026-09-22 deadlock shape again. A
		// skipped refresh is harmless -- op70 resends it -- so busy skips.
		read, err := s.readCurrentGuildNonBlocking(ctx, func(repos dnfrepo.Group, r dnfguild.Registry) error {
			g, _, found := r.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
			if found && g.ID == guild.ID {
				rank = r.ExperienceRanks()[g.ID]
				experience = g.Experience
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !read {
			s.logGameEvent(session, "game-guild-experience-rank-deferred", "reason", "guild_lock_busy")
		}
	}
	s.logGameEvent(session, "game-guild-experience-rank-sent", "rank", rank, "experience", experience, "char_id", session.selectedCharacterID, "private_abi", "90cn_guild_experience_rank_v1")
	return s.sendGameUpperRawClass(session, 413, buildCurrentGuildRankingBody(session.selectedCharacterID, rank, experience), 0)
}
