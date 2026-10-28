package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentBlackDiamondNameplateStat = "black_diamond_nameplate_active"

type currentBlackDiamondSelectionResult struct {
	Account   dnfrepo.AccountRecord
	Character dnfrepo.CharacterRecord
	Found     bool
	Active    bool
	ExpiresAt time.Time
}

func (s *Service) currentBlackDiamondActive(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.blackDiamondMu.RLock()
	expiresAt := session.blackDiamondExpiresAt
	session.blackDiamondMu.RUnlock()
	return expiresAt.After(s.gameplayNow().UTC())
}

func (s *Service) setCurrentBlackDiamondExpiry(session *gameSession, expiresAt time.Time) {
	if session == nil {
		return
	}
	session.blackDiamondMu.Lock()
	session.blackDiamondExpiresAt = expiresAt.UTC()
	session.blackDiamondMu.Unlock()
}

func currentBlackDiamondExpiration(account dnfrepo.AccountRecord) time.Time {
	expireAt := premium.BlackDiamondExpireAt(account)
	if expireAt <= 0 {
		return time.Time{}
	}
	return time.Unix(expireAt, 0).UTC()
}

// activateBlackDiamondForCharacter is the single durable entitlement owner
// shared by the payment portal and the loopback GM API. The account metadata
// and the selected character's fatigue projection commit in one repository
// transaction, which also keeps the SQLite single-connection backend free of
// transaction-external reads.
func (s *Service) activateBlackDiamondForCharacter(
	ctx context.Context,
	accountID string,
	characterID string,
	duration time.Duration,
) (currentBlackDiamondSelectionResult, error) {
	var result currentBlackDiamondSelectionResult
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if s == nil || accountID == "" || characterID == "" || duration <= 0 {
		return result, fmt.Errorf("Black Diamond activation request is invalid")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RentalAssets == nil {
		return result, fmt.Errorf("Black Diamond repositories are unavailable")
	}
	now := s.gameplayNow().UTC()
	err := repositories.RentalAssets.WithinRentalAssets(
		ctx,
		accountID,
		characterID,
		func(accounts dnfrepo.AccountRepository, characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("Black Diamond account was not found")
			}
			character, found, err := characters.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(character.AccountID) != accountID {
				return fmt.Errorf("Black Diamond character was not found")
			}
			account = dnfrepo.CloneAccount(account)
			character = dnfrepo.CloneCharacter(character)
			premium.Upsert(&account, premium.TypeBlackDiamondEntitlement, int64(duration/time.Second), 1, now)
			if character.Stats == nil {
				character.Stats = make(map[string]int64, 4)
			}
			desired := dnffatigue.DesiredBlackDiamondLimit(character.Stats["fatigue_limit"], true)
			remaining, limit, _ := dnffatigue.ReconcileLimit(
				character.Stats["fatigue"],
				character.Stats["fatigue_limit"],
				desired,
			)
			character.Stats["fatigue"], character.Stats["fatigue_limit"] = remaining, limit
			account.UpdatedAt, character.UpdatedAt = now, now
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			result = currentBlackDiamondSelectionResult{
				Account: account, Character: character, Found: true, Active: true,
				ExpiresAt: currentBlackDiamondExpiration(account),
			}
			return nil
		},
	)
	if err != nil {
		return currentBlackDiamondSelectionResult{}, err
	}
	return result, nil
}

// reconcileCurrentBlackDiamondSelection is the account/character transaction
// that closes the membership lifecycle before the select ACK is built. It
// reads only entitlement 1000 and preserves consumed fatigue while moving only
// the native 156/188 limits.
func (s *Service) reconcileCurrentBlackDiamondSelection(
	ctx context.Context,
	session *gameSession,
	selected dnfrepo.CharacterRecord,
) (currentBlackDiamondSelectionResult, error) {
	result := currentBlackDiamondSelectionResult{Character: selected}
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return result, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RentalAssets == nil {
		return result, nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if accountID == "" {
		return result, nil
	}
	now := s.gameplayNow().UTC()
	err := repositories.RentalAssets.WithinRentalAssets(
		ctx,
		accountID,
		characterID,
		func(accounts dnfrepo.AccountRepository, characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil || !found {
				return err
			}
			character, found, err := characters.Load(ctx, characterID)
			if err != nil || !found {
				return err
			}
			account = dnfrepo.CloneAccount(account)
			character = dnfrepo.CloneCharacter(character)
			expiresAt := currentBlackDiamondExpiration(account)
			active := expiresAt.After(now)
			if character.Stats == nil {
				character.Stats = make(map[string]int64, 4)
			}
			currentLimit := character.Stats["fatigue_limit"]
			desiredLimit := dnffatigue.DesiredBlackDiamondLimit(currentLimit, active)
			remaining, limit, fatigueChanged := dnffatigue.ReconcileLimit(
				character.Stats["fatigue"],
				currentLimit,
				desiredLimit,
			)
			if fatigueChanged {
				character.Stats["fatigue"] = remaining
				character.Stats["fatigue_limit"] = limit
				character.UpdatedAt = now
				if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
					return err
				}
			}
			result = currentBlackDiamondSelectionResult{
				Account: account, Character: character, Found: true,
				Active: active, ExpiresAt: expiresAt,
			}
			return nil
		},
	)
	if err != nil {
		return currentBlackDiamondSelectionResult{Character: selected}, fmt.Errorf("reconcile black diamond selection: %w", err)
	}
	if result.Found {
		s.setCurrentBlackDiamondExpiry(session, result.ExpiresAt)
	}
	return result, nil
}

// refreshCurrentBlackDiamondAfterActivation publishes only live state that
// the current town scene can safely consume. Dungeon sessions keep the durable
// entitlement and receive the complete projection on their normal town return.
func (s *Service) refreshCurrentBlackDiamondAfterActivation(
	ctx context.Context,
	session *gameSession,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	_, _, selected, found := s.selectedCharacterForEnter(ctx, session)
	if !found {
		return nil
	}
	result, err := s.reconcileCurrentBlackDiamondSelection(ctx, session, selected)
	if err != nil || !result.Found {
		return err
	}
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if inDungeon || session.townSceneReadyCharacterID != session.selectedCharacterID {
		s.logGameEvent(session, "game-black-diamond-live-refresh-deferred",
			"char_id", session.selectedCharacterID,
			"source", source,
			"in_dungeon", inDungeon,
			"town_ready", session.townSceneReadyCharacterID == session.selectedCharacterID)
		return nil
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentFatigueMsgID,
		buildCurrentFatigueBody(result.Character, true),
		0,
	); err != nil {
		return err
	}
	body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx,
		session,
		session.selectedCharacterID,
		result.Character.Name,
		result.Character,
		true,
		currentTownActorOwnerContext(session),
	)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-black-diamond-live-refresh-sent",
		"char_id", session.selectedCharacterID,
		"source", source,
		"fatigue", result.Character.Stats["fatigue"],
		"fatigue_limit", result.Character.Stats["fatigue_limit"],
		"expires_at", result.ExpiresAt)
	return nil
}

// sendCurrentBlackDiamondActivationState publishes the proved current-client
// activation route only when the selected actor is town-ready. Dungeon and
// scene-transition sessions retain the durable entitlement and intentionally
// defer presentation packets until the ordinary town reconstruction.
func (s *Service) sendCurrentBlackDiamondActivationState(
	ctx context.Context,
	session *gameSession,
	expiresAt time.Time,
	source string,
) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	s.setCurrentBlackDiamondExpiry(session, expiresAt)
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	townReady := session.townSceneReadyCharacterID == session.selectedCharacterID
	if inDungeon || !townReady {
		s.logGameEvent(session, "game-black-diamond-activation-deferred",
			"char_id", session.selectedCharacterID,
			"source", source,
			"in_dungeon", inDungeon,
			"town_ready", townReady)
		return true, nil
	}
	remaining := expiresAt.Unix() - s.gameplayNow().UTC().Unix()
	if remaining < 0 {
		remaining = 0
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentPremiumActivatedMsgID,
		buildCurrentPremiumActivatedBody(premium.TypeBlackDiamondWire, remaining),
		0,
	); err != nil {
		return false, err
	}
	return false, s.refreshCurrentBlackDiamondAfterActivation(ctx, session, source)
}
