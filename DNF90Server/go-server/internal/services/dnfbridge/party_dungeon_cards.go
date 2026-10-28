package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// The caller holds only its own dungeon lock. Party members publish snapshots
// under owner.mu; continuations run in each peer's existing session mailbox.
func (s *Service) prepareRuntimePartyDungeonCardRewardsLocked(session *gameSession, runtime *runtimeDungeonState) (bool, error) {
	owner := runtime.partyDungeonOwner
	if owner == nil {
		return true, nil
	}
	plan := runtime.settlementResultPlan
	if plan == nil || plan.clearRewardSnapshot == nil {
		return false, errors.New("party dungeon result has no owned reward snapshot")
	}
	owner.mu.Lock()
	slot := byte(0xff)
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
		if member.userID == session.selectedCharacterID && member.generation == session.characterGeneration {
			slot = member.slot
		}
	}
	if owner.retired || slot >= dungeonCardSlotsPerSide || !runtime.partyMemberIndexed || runtime.partyMemberIndex != slot ||
		owner.cardRewardDepartedSlots&(uint8(1)<<slot) != 0 {
		owner.mu.Unlock()
		return false, errors.New("party dungeon reward session no longer owns its member slot")
	}
	bit := uint8(1) << slot
	if !owner.cardRewardsFrozen && owner.cardRewardReadySlots&bit == 0 {
		for kind, table := range [3][currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair{
			plan.clearRewardSnapshot.CardSlots, plan.clearRewardSnapshot.AuxGroupsA, plan.clearRewardSnapshot.AuxGroupsB,
		} {
			owner.cardRewards[kind][slot] = append([]currentDungeonClearRewardPair(nil), table[slot]...)
		}
		owner.cardRewardReadySlots |= bit
	}
	expected &^= owner.cardRewardDepartedSlots
	newlyReady := !owner.cardRewardsFrozen && owner.cardRewardReadySlots&expected == expected
	if newlyReady {
		owner.cardRewardsFrozen = true
	}
	ready := owner.cardRewardsFrozen
	var groups [3][currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair
	if ready {
		for kind := range groups {
			for i := range groups[kind] {
				groups[kind][i] = append([]currentDungeonClearRewardPair(nil), owner.cardRewards[kind][i]...)
			}
		}
	}
	owner.mu.Unlock()
	if !ready {
		return false, nil
	}
	snapshot := *plan.clearRewardSnapshot
	snapshot.CardSlots, snapshot.AuxGroupsA, snapshot.AuxGroupsB = groups[0], groups[1], groups[2]
	body, err := buildCurrentDungeonClearRewardBody(snapshot)
	if err != nil {
		return false, err
	}
	plan.ClearRewardBody = body
	if newlyReady {
		if err := s.resumeRuntimePartyDungeonCardResults(session, owner); err != nil {
			s.logGameEvent(session, "game-party-card-result-resume-failed", "error", err)
		}
	}
	return true, nil
}

func (s *Service) resumeRuntimePartyDungeonCardResults(source *gameSession, owner *runtimePartyDungeonOwner) error {
	var result error
	owner.mu.Lock()
	departed := owner.cardRewardDepartedSlots
	owner.mu.Unlock()
	for _, member := range owner.roster() {
		if departed&(uint8(1)<<member.slot) != 0 {
			continue
		}
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.session == source || identity.generation != member.generation {
			continue
		}
		peer := identity.session
		err := s.postGameSessionCharacterEvent(peer, "party-dungeon-card-rewards-ready", identity.character, identity.generation, func() error {
			peer.dungeon.mu.Lock()
			defer peer.dungeon.mu.Unlock()
			runtime := peer.dungeon.runtime
			if runtime == nil || runtime.partyDungeonOwner != owner || runtime.settlementClearRewardSent {
				return nil
			}
			return s.sendCurrentDungeonSettlementResultsLocked(peer, runtime, "party_free_reward_table_ready")
		})
		result = errors.Join(result, err)
	}
	return result
}

// A disconnected/changing character must not hold the remaining members at
// the reward barrier. Already frozen public rows remain stable for replays.
func (s *Service) detachRuntimePartyDungeonCardRewardsLocked(session *gameSession, runtime *runtimeDungeonState) {
	if runtime == nil || runtime.partyDungeonOwner == nil {
		return
	}
	owner := runtime.partyDungeonOwner
	owner.mu.Lock()
	if owner.cardRewardsFrozen {
		owner.mu.Unlock()
		return
	}
	for _, member := range owner.members {
		if member.userID == session.selectedCharacterID && member.generation == session.characterGeneration {
			owner.cardRewardDepartedSlots |= uint8(1) << member.slot
		}
	}
	owner.mu.Unlock()
	if err := s.resumeRuntimePartyDungeonCardResults(session, owner); err != nil {
		s.logGameEvent(session, "game-party-card-result-resume-failed", "error", err)
	}
}

func runtimePartyDungeonCardMemberIndex(runtime *runtimeDungeonState) byte {
	if runtime != nil && runtime.partyDungeonOwner != nil && runtime.partyMemberIndexed {
		return runtime.partyMemberIndex
	}
	return 0
}

// publishRuntimePartyDungeonCardSelection mirrors the same-version party
// settlement projection: the selecting member owns the durable reward, while
// every other admitted member receives the public class1/op71 flip using the
// selector's stable 1P..4P slot. A peer that has not consumed op70 yet queues
// the flip so packet order remains op70 before op71 on that connection.
func (s *Service) publishRuntimePartyDungeonCardSelection(
	source *gameSession,
	runtime *runtimeDungeonState,
	side dungeonCardSide,
	memberIndex byte,
	body []byte,
) error {
	if s == nil || source == nil || runtime == nil || runtime.partyDungeonOwner == nil ||
		side >= dungeonCardSideCount || memberIndex >= dungeonCardSlotsPerSide || len(body) == 0 {
		return nil
	}
	owner := runtime.partyDungeonOwner
	member, current := owner.member(source.selectedCharacterID, source.characterGeneration)
	if !current || member.slot != memberIndex {
		return fmt.Errorf("party dungeon card source %d is not stable slot %d", source.selectedCharacterID, memberIndex)
	}
	for _, peer := range owner.roster() {
		if peer.userID == source.selectedCharacterID && peer.generation == source.characterGeneration {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(peer.userID)
		if !online || identity.session == nil || identity.generation != peer.generation {
			return fmt.Errorf("party dungeon card member %d session generation changed", peer.userID)
		}
		peerSession := identity.session
		peerBody := append([]byte(nil), body...)
		if err := s.postGameSessionCharacterEvent(
			peerSession,
			"party-dungeon-card-selection",
			identity.character,
			identity.generation,
			func() error {
				peerSession.dungeon.mu.Lock()
				defer peerSession.dungeon.mu.Unlock()
				peerRuntime := peerSession.dungeon.runtime
				if peerRuntime == nil || peerRuntime.partyDungeonOwner != owner {
					return nil
				}
				if peerRuntime.partyCardSelectionSeen[side][memberIndex] {
					return nil
				}
				if !peerRuntime.settlementCardLayoutSent {
					peerRuntime.partyCardSelectionPending = append(
						peerRuntime.partyCardSelectionPending,
						peerBody,
					)
					peerRuntime.partyCardSelectionSeen[side][memberIndex] = true
					return nil
				}
				if err := s.sendGameUpperRawClass(
					peerSession,
					uint16(dnfenum.CmdPacketSelectCard),
					peerBody,
					currentDungeonCardResponseClass,
				); err != nil {
					return err
				}
				peerRuntime.partyCardSelectionSeen[side][memberIndex] = true
				return nil
			},
		); err != nil {
			return fmt.Errorf("publish party dungeon card to member %d: %w", peer.userID, err)
		}
	}
	return nil
}

func (s *Service) flushRuntimePartyDungeonCardSelectionsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if s == nil || session == nil || runtime == nil || session.dungeon.runtime != runtime ||
		!runtime.settlementCardLayoutSent || len(runtime.partyCardSelectionPending) == 0 {
		return nil
	}
	for len(runtime.partyCardSelectionPending) != 0 {
		body := runtime.partyCardSelectionPending[0]
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketSelectCard),
			append([]byte(nil), body...),
			currentDungeonCardResponseClass,
		); err != nil {
			return err
		}
		runtime.partyCardSelectionPending = runtime.partyCardSelectionPending[1:]
	}
	return nil
}
