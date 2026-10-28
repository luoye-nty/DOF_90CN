package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
)

func (s *Service) reserveRuntimeRaidAdmissionCounters(
	ctx context.Context,
	state runtimeRaidState,
	ownerMode byte,
	now time.Time,
) (*currentDungeonAdmissionCounterBatch, error) {
	policyKeys := currentRaidAdmissionPolicyKeys(ownerMode)
	if len(policyKeys) == 0 {
		// There is no current Fiend-War runtime owner: this server has no
		// 超时空之战 raid entry path at all (only the Anton/Luke owners exist,
		// raid_luke_runtime.go:12-13). Its PVF limits stay catalogued and are
		// rendered in the channel-select window, but there is nothing to charge.
		return nil, nil
	}
	// Every operand the client's two team-mode windows display is charged here,
	// in ONE Preflight/Commit pair per member: the account daily row (bytes 2/3
	// of the channel-select window), the account weekly row and the character
	// weekly row (byte 4/5 of that window, and 角色周入场次数 of the entry-info
	// window). Charging all three from the same policy list is what keeps the
	// number the player sees equal to the number that decides the start, and it
	// is also the enforcement point: Owner.Preflight refuses with a LimitError
	// once any of the three is exhausted, so the raid start fails with the
	// native "daily entry count exceeded" prompt.
	policySet := make([]dnfdungeonadmission.Policy, 0, len(policyKeys))
	chargedPolicyKeys := make([]string, 0, len(policyKeys))
	for _, policyKey := range policyKeys {
		policy, found, err := s.currentDungeonAdmissionPolicy(policyKey)
		if err != nil {
			return nil, err
		}
		if !found {
			// A missing key means the catalog came from a document set that does
			// not declare the section. LoadCatalog refuses to build such a
			// catalog for the raids, so this is a defensive branch: charge what
			// exists rather than turning a raid start into a hard failure.
			continue
		}
		policySet = append(policySet, policy)
		chargedPolicyKeys = append(chargedPolicyKeys, policy.Key)
	}
	if len(policySet) == 0 {
		return nil, nil
	}
	if len(state.FrozenMembers) == 0 || state.RaidKey == 0 || state.StartAttempt == 0 {
		return nil, errors.New("raid admission counter reservation is incomplete")
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfdungeonadmission.ErrOwnerUnavailable
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	bootNonce, err := s.currentDungeonAdmissionReceiptNonce()
	if err != nil {
		return nil, err
	}
	commits := make([]*currentDungeonAdmissionCounterCommit, 0, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		if member.CharID == 0 {
			return nil, errors.New("raid admission counter member is missing")
		}
		session, _ := s.onlineGameSession(member.CharID)
		commit := &currentDungeonAdmissionCounterCommit{
			session: session, characterID: member.CharID, ownerKind: "raid", policies: policySet,
			receipt: runtimeRaidAdmissionReceipt(bootNonce, state, ownerMode, member.CharID),
		}
		if _, err := owner.Preflight(ctx, fmt.Sprint(member.CharID), policySet, now); err != nil {
			return nil, fmt.Errorf("preflight character %d raid admission counter: %w", member.CharID, err)
		}
		commits = append(commits, commit)
	}
	batch := &currentDungeonAdmissionCounterBatch{
		owner: owner, reservedAt: now,
		commits: make([]*currentDungeonAdmissionCounterCommit, 0, len(commits)),
	}
	for _, commit := range commits {
		// Register before Commit so an outcome-ambiguous SQL commit can still be
		// located and compensated by the exact durable receipt.
		batch.commits = append(batch.commits, commit)
		if _, err := owner.Commit(ctx, fmt.Sprint(commit.characterID), policySet, commit.receipt, now); err != nil {
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(batch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			return nil, fmt.Errorf("reserve character %d raid admission counter: %w", commit.characterID, err)
		}
		if commit.session != nil {
			s.logGameEvent(commit.session, "game-raid-admission-counter-reserved",
				"raid_key", state.RaidKey,
				"raid_mode", ownerMode,
				"start_attempt", state.StartAttempt,
				"char_id", commit.characterID,
				"policy", strings.Join(chargedPolicyKeys, ","),
				"charge_point", "raid_start_entry_not_clear")
		}
	}
	return batch, nil
}

// runtimeRaidAdmissionReceipt is the exact durable receipt one member's raid
// admission charge commits under, and therefore the only handle a later
// compensation can address.
//
// Both sides build it HERE on purpose. dungeonadmission.Owner.Restore is
// receipt-addressed: it walks the policies and decrements only the counter whose
// stored receipt matches this digest, leaving a restored tombstone behind. A
// refund that spelled the string even slightly differently would not fail -- it
// would silently match nothing and return as a no-op, which is the one outcome
// nobody would notice until players complained their 入场次数 never came back.
//
// The operands are all immutable for one attempt: RaidKey names the run,
// StartAttempt is bumped by reserveRuntimeRaidStart before the charge and never
// during it, ownerMode is the owner the charge was made under (and is what
// commitRuntimeRaidStartWithOwner freezes into state.RaidMode), and the boot
// nonce pins the process, so a restart in the same service window cannot replay
// an older run's receipt.
func runtimeRaidAdmissionReceipt(bootNonce string, state runtimeRaidState, ownerMode byte, characterID uint16) string {
	return fmt.Sprintf(
		"raid-admission-v1:boot:%s:raid:%d:attempt:%d:mode:%d:character:%d",
		bootNonce, state.RaidKey, state.StartAttempt, ownerMode, characterID,
	)
}

func (s *Service) currentDungeonAdmissionPolicy(key string) (dnfdungeonadmission.Policy, bool, error) {
	if s == nil || key == "" {
		return dnfdungeonadmission.Policy{}, false, nil
	}
	s.worldMapMu.RLock()
	catalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if catalog == nil {
		s.initialEquipmentMu.Lock()
		hasRuntimePVF := s.initialEquipmentArchive != nil
		s.initialEquipmentMu.Unlock()
		if hasRuntimePVF {
			return dnfdungeonadmission.Policy{}, false, errors.New("dungeon admission counter catalog is unavailable")
		}
		return dnfdungeonadmission.Policy{}, false, nil
	}
	policy, found := catalog.Policy(key)
	return policy, found, nil
}
