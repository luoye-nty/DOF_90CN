package dnfbridge

import (
	"strconv"
	"strings"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 公会贡献 grants for the rules the client documents in help texts 76511..76515.
// Every entry point here follows the attendance hook's discipline exactly
// (guild_management.go's grantCurrentGuildAttendance): the caller may already sit
// inside a session-scoped critical section, guildMu is a plain non-reentrant
// mutex, and the first synchronous version of that hook deadlocked its own
// session goroutine on 2026-09-12 01:33. So each grant is a bounded TryLock retry
// on a background goroutine, and nothing is ever pushed to the client: 公会积分 is
// request-driven (op747), so a new row simply appears the next time the window is
// opened.

// currentGuildMileageGrantTimeout bounds the background grant's wait for guildMu.
const currentGuildMileageGrantTimeout = 10 * time.Second

// grantCurrentGuildChatMileage awards 公会聊天 2点, once per local day.
func (s *Service) grantCurrentGuildChatMileage(session *gameSession, source string) {
	characterID := currentGuildSessionCharacterID(session)
	if characterID == "" {
		return
	}
	go s.applyCurrentGuildMileageGrant(characterID, 0, source, func(registry *dnfguild.Registry, id string, now time.Time) (bool, uint32, error) {
		applied, err := registry.GrantChatMileage(id, now)
		return applied, dnfguild.MileageChatPoints, err
	})
}

// grantCurrentGuildPartyMileage awards 同公会成员组队通关地下城 1点 to the clearing
// actor when at least two members of the clearing party are in the same guild.
//
// The party is read inside the background goroutine, not at the call site: the
// dungeon settlement path calls this while holding session.dungeon.mu, and
// runtimePartyStateSnapshot takes session.party.mu. Reading it there keeps the two
// session locks from ever nesting, so no lock-order inversion is possible. The
// character id is captured here because the session field is only safe to read on
// its own goroutine.
func (s *Service) grantCurrentGuildPartyMileage(session *gameSession, source string) {
	characterID := currentGuildSessionCharacterID(session)
	if characterID == "" || session == nil {
		return
	}
	go func() {
		party := currentGuildPartyCharacterIDs(session)
		if len(party) < 2 {
			return
		}
		s.applyCurrentGuildMileageGrant(characterID, len(party), source, func(registry *dnfguild.Registry, id string, now time.Time) (bool, uint32, error) {
			applied, err := registry.GrantPartyMileage(id, party, now)
			return applied, dnfguild.MileagePartyPoints, err
		})
	}()
}

func currentGuildSessionCharacterID(session *gameSession) string {
	if session == nil || session.selectedCharacterID == 0 {
		return ""
	}
	return strconv.Itoa(int(session.selectedCharacterID))
}

// currentGuildPartyCharacterIDs snapshots the caller's party as character IDs.
// Solo play yields a single entry, which the party rule then rejects.
func currentGuildPartyCharacterIDs(session *gameSession) []string {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ids := []string{strconv.Itoa(int(session.selectedCharacterID))}
	for _, member := range runtimePartyMembers(runtimePartyStateSnapshot(session)) {
		if member.UserID == 0 || member.UserID == session.selectedCharacterID {
			continue
		}
		ids = append(ids, strconv.Itoa(int(member.UserID)))
	}
	return ids
}

// applyCurrentGuildMileageGrant runs one grant under a bounded TryLock retry and
// logs what it did. The grant function returns the amount it would have paid so
// the log line can name it without a second lookup. partySize is the party the
// rule was evaluated against, for the log only.
func (s *Service) applyCurrentGuildMileageGrant(
	characterID string,
	partySize int,
	source string,
	grant func(registry *dnfguild.Registry, characterID string, now time.Time) (bool, uint32, error),
) {
	if strings.TrimSpace(characterID) == "" || grant == nil {
		return
	}
	deadline := time.Now().Add(currentGuildMileageGrantTimeout)
	for {
		if s.guildMu.TryLock() {
			s.guildMu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			s.logPacketEvent("game-guild-mileage-skipped",
				"reason", "guild_lock_busy", "source", source, "char_id", characterID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	granted := false
	amount := uint32(0)
	total := uint32(0)
	if err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		applied, paid, err := grant(registry, characterID, s.gameplayNow())
		if err != nil {
			return err
		}
		if !applied {
			return nil
		}
		granted, amount = true, paid
		if guild, _, found := registry.GuildForCharacter(characterID); found && guild != nil {
			total = guild.Mileage
		}
		return nil
	}); err != nil {
		s.logPacketEvent("game-guild-mileage-grant-failed",
			"source", source, "char_id", characterID, "party", partySize, "error", err)
		return
	}
	if !granted {
		return
	}
	s.logPacketEvent("game-guild-mileage-granted",
		"source", source, "char_id", characterID, "amount", amount,
		"party", partySize, "mileage", total)
}
