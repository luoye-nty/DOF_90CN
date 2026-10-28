package dnfbridge

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfvending "longheng.io/server/internal/modules/dnf/vending"
)

type currentVendingMachinePending struct {
	CharacterID   uint16
	Request       currentVendingMachineRequest
	CompletionKey string
	MaterialID    uint32
	MaterialCount uint32
	Output        currentVendingMachineOutput
}

type currentVendingMachineSessionState struct {
	mu      sync.Mutex
	pending *currentVendingMachinePending
}

type currentVendingMachineCharacterLockKey struct {
	service     *Service
	characterID uint16
}

type currentVendingMachineCharacterLock struct {
	mu   sync.Mutex
	refs int
}

var currentVendingMachineCharacterLocks = struct {
	sync.Mutex
	entries map[currentVendingMachineCharacterLockKey]*currentVendingMachineCharacterLock
}{entries: make(map[currentVendingMachineCharacterLockKey]*currentVendingMachineCharacterLock)}

// lockCurrentVendingMachineCharacter serializes the full current op215
// lifecycle across an old and replacement game session for the same character.
// Repository transactions serialize individual writes, but without this outer
// boundary another use can replace the single durable completion receipt in the
// gap between an indeterminate commit result and its receipt-confirming retry.
func lockCurrentVendingMachineCharacter(service *Service, characterID uint16) func() {
	key := currentVendingMachineCharacterLockKey{service: service, characterID: characterID}
	currentVendingMachineCharacterLocks.Lock()
	entry := currentVendingMachineCharacterLocks.entries[key]
	if entry == nil {
		entry = &currentVendingMachineCharacterLock{}
		currentVendingMachineCharacterLocks.entries[key] = entry
	}
	entry.refs++
	currentVendingMachineCharacterLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		currentVendingMachineCharacterLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(currentVendingMachineCharacterLocks.entries, key)
		}
		currentVendingMachineCharacterLocks.Unlock()
	}
}

func vendingMachineGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketUseVendingMachine)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentVendingMachine(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "vending-machine",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: handler,
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(
					service,
					session,
					request,
					"game-vending-machine-blocked",
					"current_exe_op215_command_class_mismatch",
				) {
					return nil
				}
				return handler(service, session, request)
			},
		},
	}
}

func (s *Service) handleCurrentVendingMachine(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketUseVendingMachine)
	request, err := parseCurrentVendingMachineRequest(body)
	if err != nil || session == nil || session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-vending-machine-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(
			session,
			opcode,
			buildCurrentVendingMachineFailureBody(currentVendingMachineError),
			dnfproto.DefaultChannelClassification,
		)
	}

	characterID := session.selectedCharacterID
	// The session lock freezes one socket's random result and completion key.
	// The character lock extends the same boundary across an old/replacement
	// reconnect socket through prepare, both commit attempts, row projection and
	// the native ACK.
	session.vendingMachine.mu.Lock()
	defer session.vendingMachine.mu.Unlock()
	unlockCharacter := lockCurrentVendingMachineCharacter(s, characterID)
	defer unlockCharacter()
	if session.selectedCharacterID != characterID {
		return s.sendGameUpperRawClass(
			session,
			opcode,
			buildCurrentVendingMachineFailureBody(currentVendingMachineError),
			dnfproto.DefaultChannelClassification,
		)
	}
	pending := session.vendingMachine.pending
	if pending == nil || pending.CharacterID != characterID || pending.Request != request {
		pending, err = s.prepareCurrentVendingMachinePending(characterID, request)
		if err != nil {
			s.logGameEvent(session, "game-vending-machine-rejected",
				"machine_id", request.MachineID,
				"draw_count", request.DrawCount,
				"material_slot", request.MaterialSlot,
				"reason", err)
			return s.sendGameUpperRawClass(
				session,
				opcode,
				buildCurrentVendingMachineFailureBody(currentVendingMachineError),
				dnfproto.DefaultChannelClassification,
			)
		}
		session.vendingMachine.pending = pending
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentVendingMachine(ctx, session, *pending)
	if err != nil {
		// A repository can report an error after its commit outcome became
		// unknowable. The stable receipt makes this single immediate replay safe;
		// deterministic failures simply fail the same validation twice.
		firstResult := result
		result, err = s.commitCurrentVendingMachine(ctx, session, *pending)
		if err == nil && result.Replayed && currentVendingMachineSameCommittedResult(firstResult, result) &&
			len(firstResult.RewardSnapshots) == len(result.RewardSnapshots) {
			// A receipt-confirmed replay proves the first transaction committed.
			// Preserve its transaction-time rows even if another inventory owner
			// changed the same stack before the backend reported its outcome.
			result.RewardSnapshots = firstResult.RewardSnapshots
		}
	}
	if err != nil {
		s.logGameEvent(session, "game-vending-machine-rejected",
			"machine_id", request.MachineID,
			"draw_count", request.DrawCount,
			"material_slot", request.MaterialSlot,
			"material_item", pending.MaterialID,
			"reward_item", pending.Output.ItemID,
			"reward_count", pending.Output.Count,
			"reason", err)
		return s.sendGameUpperRawClass(
			session,
			opcode,
			buildCurrentVendingMachineFailureBody(currentVendingMachineError),
			dnfproto.DefaultChannelClassification,
		)
	}

	rewardRows, err := currentVendingMachineRewardRows(result.RewardSnapshots)
	if err != nil {
		return err
	}
	successBody, err := buildCurrentVendingMachineSuccessBody(currentVendingMachineSuccess{
		MaterialSlot:      result.MaterialSlot,
		MaterialRemaining: result.MaterialRemaining,
		RewardItemID:      result.RewardItemID,
		RewardCount:       result.RewardCount,
		RewardRows:        rewardRows,
	})
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, opcode, successBody, dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	session.vendingMachine.pending = nil
	s.logGameEvent(session, "game-vending-machine-committed",
		"machine_id", request.MachineID,
		"draw_count", request.DrawCount,
		"material_slot", result.MaterialSlot,
		"material_item", result.MaterialItemID,
		"material_remaining", result.MaterialRemaining,
		"reward_item", result.RewardItemID,
		"reward_count", result.RewardCount,
		"reward_slots", fmt.Sprint(result.TouchedSlots),
		"jackpot_flag", pending.Output.JackpotFlag,
		"receipt_replayed", result.Replayed)
	return nil
}

func (s *Service) prepareCurrentVendingMachinePending(
	characterID uint16,
	request currentVendingMachineRequest,
) (*currentVendingMachinePending, error) {
	if characterID == 0 {
		return nil, dnfvending.ErrCharacterRequired
	}
	catalog, err := s.currentVendingMachinePVFCatalog()
	if err != nil {
		return nil, err
	}
	group, err := catalog.Resolve(request.MachineID, currentVendingMachineGroupID)
	if err != nil {
		return nil, err
	}
	output, err := s.rollCurrentVendingMachineOutput(group)
	if err != nil {
		return nil, err
	}
	key, err := s.newCurrentVendingMachineCompletionKey()
	if err != nil {
		return nil, err
	}
	return &currentVendingMachinePending{
		CharacterID:   characterID,
		Request:       request,
		CompletionKey: key,
		MaterialID:    group.MaterialItemID,
		MaterialCount: group.MaterialCount,
		Output:        output,
	}, nil
}

func (s *Service) newCurrentVendingMachineCompletionKey() (string, error) {
	if s != nil && s.vendingMachineCompletionKey != nil {
		return s.vendingMachineCompletionKey()
	}
	var nonce [16]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "vending-machine:" + hex.EncodeToString(nonce[:]), nil
}

func (s *Service) commitCurrentVendingMachine(
	ctx context.Context,
	session *gameSession,
	pending currentVendingMachinePending,
) (dnfvending.Result, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		pending.CharacterID == 0 || pending.CharacterID != session.selectedCharacterID ||
		pending.CompletionKey == "" || pending.MaterialID == 0 || pending.MaterialCount == 0 ||
		pending.Output.ItemID == 0 || pending.Output.Count == 0 {
		return dnfvending.Result{}, dnfvending.ErrCommandInvalid
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnfvending.Result{}, dnfvending.ErrOwnerUnavailable
	}
	owner, err := dnfvending.NewOwner(repositories)
	if err != nil {
		return dnfvending.Result{}, err
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return dnfvending.Result{}, err
	}
	definition, err := itemCatalog.ResolveItem(pending.Output.ItemID)
	if err != nil {
		return dnfvending.Result{}, err
	}
	now := time.Now().UTC()
	definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return dnfvending.Result{}, err
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	return owner.Use(ctx, dnfvending.Command{
		AccountID:      accountID,
		CharacterID:    characterID,
		CompletionKey:  pending.CompletionKey,
		MaterialSlot:   pending.Request.MaterialSlot,
		MaterialItemID: pending.MaterialID,
		MaterialCount:  pending.MaterialCount,
		RewardItemID:   pending.Output.ItemID,
		RewardCount:    pending.Output.Count,
		UpdatedAt:      now,
		Project: func(candidate *dnfrepo.InventoryRecord, rewardItemID, rewardCount uint32) ([]int16, error) {
			if candidate == nil || rewardItemID != definition.ItemID || rewardCount != pending.Output.Count {
				return nil, dnfvending.ErrProjectionInvalid
			}
			slots, grantErr := grantCurrentVendingShopProduct(candidate, definition, rewardCount, s.currentInventoryPlacementMainSlotCount(ctx, candidate, ""))
			if grantErr != nil {
				if errors.Is(grantErr, errDungeonPickupInventoryFull) {
					return nil, errors.Join(dnfvending.ErrInventoryFull, grantErr)
				}
				return nil, grantErr
			}
			touched := make([]int16, 0, len(slots))
			for _, slot := range slots {
				if slot > math.MaxInt16 {
					return nil, dnfvending.ErrProjectionInvalid
				}
				signedSlot := int16(slot)
				key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, signedSlot)
				stack, found := candidate.Slots[key]
				if !found || stack.ItemID != int64(rewardItemID) || stack.Count <= 0 {
					return nil, dnfvending.ErrProjectionInvalid
				}
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 4)
				}
				stack.Extra["source"] = "vending_machine"
				stack.Extra["last_grant_source"] = "vending_machine"
				stack.Extra["vending_machine_id"] = strconv.FormatUint(uint64(pending.Request.MachineID), 10)
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, signedSlot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				candidate.Slots[key] = stack
				touched = append(touched, signedSlot)
			}
			return touched, nil
		},
	})
}

func currentVendingMachineRewardRows(
	snapshots []dnfvending.RewardSnapshot,
) ([]currentVendingMachineRewardRow, error) {
	if len(snapshots) == 0 || len(snapshots) > math.MaxUint16 {
		return nil, dnfvending.ErrProjectionInvalid
	}
	rows := make([]currentVendingMachineRewardRow, 0, len(snapshots))
	seen := make(map[int16]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Slot < 0 ||
			dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, snapshot.Slot) ||
			snapshot.Stack.ItemID <= 0 || snapshot.Stack.Count <= 0 ||
			len(snapshot.Stack.RawEntry) != currentItemListEntryWireSize {
			return nil, dnfvending.ErrProjectionInvalid
		}
		if _, duplicate := seen[snapshot.Slot]; duplicate {
			return nil, dnfvending.ErrProjectionInvalid
		}
		seen[snapshot.Slot] = struct{}{}
		entry := currentItemListEntryFromRaw(snapshot.Stack.RawEntry, nil)
		if binary.LittleEndian.Uint16(entry.data[0:2]) != uint16(snapshot.Slot) ||
			binary.LittleEndian.Uint32(entry.data[2:6]) != uint32(snapshot.Stack.ItemID) ||
			binary.LittleEndian.Uint32(entry.data[6:10]) == 0 {
			return nil, dnfvending.ErrProjectionInvalid
		}
		rows = append(rows, currentVendingMachineRewardRow{
			ListType: dnfrepo.MainInventoryListType,
			Entry:    entry,
		})
	}
	return rows, nil
}

func currentVendingMachineSameCommittedResult(left, right dnfvending.Result) bool {
	if left.MaterialSlot != right.MaterialSlot ||
		left.MaterialItemID != right.MaterialItemID ||
		left.MaterialCount != right.MaterialCount ||
		left.MaterialRemaining != right.MaterialRemaining ||
		left.MaterialRemoved != right.MaterialRemoved ||
		left.RewardItemID != right.RewardItemID ||
		left.RewardCount != right.RewardCount ||
		len(left.TouchedSlots) != len(right.TouchedSlots) {
		return false
	}
	for index := range left.TouchedSlots {
		if left.TouchedSlots[index] != right.TouchedSlots[index] {
			return false
		}
	}
	return true
}
