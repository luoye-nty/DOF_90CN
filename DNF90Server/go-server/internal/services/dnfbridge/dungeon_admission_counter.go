package dnfbridge

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type currentDungeonAdmissionCounterCommit struct {
	session     *gameSession
	runtime     *runtimeDungeonState
	characterID uint16
	dungeonID   int64
	ownerKind   string
	policies    []dnfdungeonadmission.Policy
	receipt     string
	restored    bool
}

// currentDungeonAdmissionCounterBatch is reserved before inventory and
// runtime commits. Every later failure restores it after runtime and inventory
// compensation, preserving the reverse order of the admission transaction.
type currentDungeonAdmissionCounterBatch struct {
	owner      *dnfdungeonadmission.Owner
	reservedAt time.Time
	commits    []*currentDungeonAdmissionCounterCommit
}

func (s *Service) reserveCurrentDungeonAdmissionCounters(
	ctx context.Context,
	leader *gameSession,
	leaderRuntime *runtimeDungeonState,
	preparedFollowers []*runtimePartyPreparedDungeonEntry,
) (*currentDungeonAdmissionCounterBatch, error) {
	if leader == nil || leaderRuntime == nil || currentDungeonRuntimeIsPractice(leaderRuntime) {
		return nil, nil
	}
	// Despair Tower and Grief Tower are ticket-driven in this deployment. The
	// ticket still has to be present and is consumed by the normal entry-item
	// transaction, but there is no server-side daily/account admission cap.
	switch currentDungeonTowerKind(leaderRuntime) {
	case dnfdungeon.TowerDespair, dnfdungeon.TowerGrief:
		return nil, nil
	}
	policies, err := s.currentDungeonAdmissionPolicies(uint32(leaderRuntime.Dungeon.ID))
	if err != nil || len(policies) == 0 {
		return nil, err
	}
	policies = s.applyCurrentDungeonAdmissionAccountCharacterLimit(
		ctx, leader, leaderRuntime.Character, policies,
	)
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfdungeonadmission.ErrOwnerUnavailable
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return nil, err
	}
	members := make([]*currentDungeonAdmissionCounterCommit, 0, len(preparedFollowers)+1)
	members = append(members, &currentDungeonAdmissionCounterCommit{
		session: leader, runtime: leaderRuntime, characterID: leader.selectedCharacterID,
		dungeonID: leaderRuntime.Dungeon.ID, ownerKind: "dungeon", policies: policies,
	})
	for _, prepared := range preparedFollowers {
		if prepared == nil || prepared.session == nil || prepared.runtime == nil {
			return nil, errors.New("prepared party member admission counter is incomplete")
		}
		if currentDungeonRuntimeIsPractice(prepared.runtime) {
			return nil, errors.New("party admission mixes practice and ordinary runtimes")
		}
		memberPolicies, policyErr := s.currentDungeonAdmissionPolicies(uint32(prepared.runtime.Dungeon.ID))
		if policyErr != nil {
			return nil, policyErr
		}
		memberPolicies = s.applyCurrentDungeonAdmissionAccountCharacterLimit(
			ctx, prepared.session, prepared.runtime.Character, memberPolicies,
		)
		if !sameCurrentDungeonAdmissionPolicies(policies, memberPolicies) {
			return nil, fmt.Errorf("party member %d admission policies differ from leader", prepared.session.selectedCharacterID)
		}
		members = append(members, &currentDungeonAdmissionCounterCommit{
			session: prepared.session, runtime: prepared.runtime, characterID: prepared.session.selectedCharacterID,
			dungeonID: prepared.runtime.Dungeon.ID, ownerKind: "dungeon", policies: memberPolicies,
		})
	}

	now := time.Now().UTC()
	bootNonce, err := s.currentDungeonAdmissionReceiptNonce()
	if err != nil {
		return nil, err
	}
	// The leader token identifies this complete party attempt; each receipt also
	// contains a boot-unique nonce and member ID. The nonce prevents a service
	// restart in the same service window from replaying an old durable receipt.
	attemptToken := s.allocateDungeonLifecycleToken()
	if attemptToken == 0 {
		return nil, errors.New("dungeon admission attempt token is unavailable")
	}
	for _, member := range members {
		if member.runtime.lifecycleToken != 0 {
			return nil, fmt.Errorf("character %d prepared runtime already has lifecycle token", member.session.selectedCharacterID)
		}
		member.receipt = fmt.Sprintf(
			"dungeon-admission-v2:boot:%s:attempt:%d:character:%d:dungeon:%d",
			bootNonce,
			attemptToken,
			member.session.selectedCharacterID,
			leaderRuntime.Dungeon.ID,
		)
		if _, err := owner.Preflight(ctx, fmt.Sprint(member.session.selectedCharacterID), member.policies, now); err != nil {
			return nil, fmt.Errorf("preflight character %d dungeon admission counter: %w", member.session.selectedCharacterID, err)
		}
	}

	batch := &currentDungeonAdmissionCounterBatch{owner: owner, reservedAt: now, commits: make([]*currentDungeonAdmissionCounterCommit, 0, len(members))}
	for _, member := range members {
		// Register the exact receipt before Commit. SQL commit errors can be
		// outcome-ambiguous; Restore is receipt-aware and therefore compensates
		// a commit that actually landed while remaining a no-op when it did not.
		batch.commits = append(batch.commits, member)
		if _, err := owner.Commit(ctx, fmt.Sprint(member.session.selectedCharacterID), member.policies, member.receipt, now); err != nil {
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(batch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			return nil, fmt.Errorf("reserve character %d dungeon admission counter: %w", member.session.selectedCharacterID, err)
		}
		member.runtime.lifecycleToken = s.allocateDungeonLifecycleToken()
		if member.runtime.lifecycleToken == 0 {
			err := errors.New("dungeon runtime lifecycle token is unavailable")
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(batch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			return nil, err
		}
		s.logGameEvent(member.session, "game-dungeon-admission-counter-reserved",
			"char_id", member.session.selectedCharacterID,
			"dungeon_id", leaderRuntime.Dungeon.ID,
			"policy_count", len(member.policies),
			"attempt_token", attemptToken,
			"runtime_token", member.runtime.lifecycleToken)
	}
	return batch, nil
}

func (s *Service) restoreCurrentDungeonAdmissionCounterBatch(batch *currentDungeonAdmissionCounterBatch) error {
	if batch == nil || batch.owner == nil {
		return nil
	}
	var restoreErr error
	for index := len(batch.commits) - 1; index >= 0; index-- {
		commit := batch.commits[index]
		if commit == nil || commit.characterID == 0 || commit.restored {
			continue
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		_, err := batch.owner.Restore(
			restoreCtx,
			fmt.Sprint(commit.characterID),
			commit.policies,
			commit.receipt,
			batch.reservedAt,
		)
		cancel()
		if err != nil {
			if commit.session != nil {
				s.logGameEvent(commit.session, "game-admission-counter-restore-failed",
					"owner_kind", commit.ownerKind,
					"char_id", commit.characterID,
					"error", err)
			}
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore character %d %s admission counter: %w", commit.characterID, commit.ownerKind, err))
			continue
		}
		commit.restored = true
		if commit.session != nil {
			s.logGameEvent(commit.session, "game-admission-counter-restored",
				"owner_kind", commit.ownerKind,
				"char_id", commit.characterID,
				"dungeon_id", commit.dungeonID,
				"policy_count", len(commit.policies))
		}
	}
	return restoreErr
}

func (s *Service) currentDungeonAdmissionReceiptNonce() (string, error) {
	if s == nil {
		return "", errors.New("dungeon admission receipt owner is unavailable")
	}
	s.dungeonAdmissionReceiptOnce.Do(func() {
		if s.dungeonAdmissionReceiptNonce != "" {
			return
		}
		var raw [16]byte
		if _, err := cryptorand.Read(raw[:]); err != nil {
			s.dungeonAdmissionReceiptErr = fmt.Errorf("generate cryptographic process nonce: %w", err)
			return
		}
		s.dungeonAdmissionReceiptNonce = hex.EncodeToString(raw[:])
	})
	if s.dungeonAdmissionReceiptErr != nil {
		return "", s.dungeonAdmissionReceiptErr
	}
	if s.dungeonAdmissionReceiptNonce == "" {
		return "", errors.New("dungeon admission receipt process nonce is unavailable")
	}
	return s.dungeonAdmissionReceiptNonce, nil
}

func (s *Service) currentDungeonAdmissionPolicies(dungeonID uint32) ([]dnfdungeonadmission.Policy, error) {
	if s == nil || dungeonID == 0 {
		return nil, nil
	}
	s.worldMapMu.RLock()
	catalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if catalog == nil {
		// Synthetic bridge fixtures install their own worldmap without a complete
		// active-PVF archive. Production preload always installs the catalog and
		// fails startup if its structured sources cannot be audited.
		s.initialEquipmentMu.Lock()
		hasRuntimePVF := s.initialEquipmentArchive != nil
		s.initialEquipmentMu.Unlock()
		if hasRuntimePVF {
			return nil, errors.New("dungeon admission counter catalog is unavailable")
		}
		return nil, nil
	}
	return catalog.PoliciesForDungeon(dungeonID), nil
}

func (s *Service) applyCurrentDungeonAdmissionAccountCharacterLimit(
	ctx context.Context,
	session *gameSession,
	selected dnfrepo.CharacterRecord,
	policies []dnfdungeonadmission.Policy,
) []dnfdungeonadmission.Policy {
	if s == nil || len(policies) == 0 {
		return policies
	}
	s.worldMapMu.RLock()
	catalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if catalog == nil {
		return policies
	}
	manageLevel := uint32(0)
	selectedFound := selected.CharacterID != ""
	if !selectedFound && session != nil && session.selectedCharacterID != 0 {
		if repositories, ok := s.repositoryGroup(); ok && repositories.Character != nil {
			record, found, err := repositories.Character.Load(ctx, fmt.Sprint(session.selectedCharacterID))
			if err == nil && found {
				selected, selectedFound = record, true
			}
		}
	}
	if selectedFound && session != nil {
		summary, err := s.currentAccountAdventureGroupSummary(ctx, selected, true, session)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-admission-adventure-level-deferred", "error", err)
		} else if summary.ManageLevel > 0 {
			manageLevel = uint32(summary.ManageLevel)
		}
	}
	effectiveLimit := catalog.AccountCharacterDungeonDailyLimit(manageLevel)
	if effectiveLimit == 0 {
		return policies
	}
	adjusted := append([]dnfdungeonadmission.Policy(nil), policies...)
	changed := false
	for index := range adjusted {
		if adjusted[index].Key != dnfdungeonadmission.AccountCharacterDungeonAccountPolicyKey {
			continue
		}
		adjusted[index].Limit = effectiveLimit
		changed = true
	}
	if changed && session != nil {
		s.logGameEvent(session, "game-dungeon-admission-account-character-daily-limit-resolved",
			"char_id", session.selectedCharacterID,
			"adventure_level", manageLevel,
			"base_limit", catalog.AccountCharacterDungeonDailyLimit(0),
			"effective_limit", effectiveLimit)
	}
	return adjusted
}

func sameCurrentDungeonAdmissionPolicies(left, right []dnfdungeonadmission.Policy) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Key != right[index].Key || left[index].Scope != right[index].Scope ||
			left[index].Period != right[index].Period || left[index].Limit != right[index].Limit ||
			left[index].ResetWeekday != right[index].ResetWeekday {
			return false
		}
	}
	return true
}
