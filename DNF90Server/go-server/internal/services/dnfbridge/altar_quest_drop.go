package dnfbridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentDungeonAltarQuestDropReceiptVersion = 1

var errCurrentDungeonAltarQuestDropInvalid = errors.New("current dungeon altar quest drop is invalid")

type currentDungeonAltarQuestDropReceipt struct {
	Version     int    `json:"version"`
	MonsterCode int64  `json:"monster_code"`
	DropSeed    uint32 `json:"drop_seed"`
	UpdateBody  string `json:"update_body,omitempty"`
}

type currentDungeonAltarQuestDropCommit struct {
	Body     []byte
	Changed  bool
	Replayed bool
}

// commitCurrentDungeonAltarQuestDropsLocked runs before altar sequences are
// retired. All newly reported sequences share one CharacterSettlement
// transaction, so an op255 batch cannot fan out into as many as 100 catalog
// loads and database transactions. Successful inventory grants and their
// sequence receipts commit atomically; a failure leaves every sequence live.
func (s *Service) commitCurrentDungeonAltarQuestDropsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	deaths []currentDungeonAltarDeathSequence,
) error {
	if s == nil || session == nil || runtime == nil || runtime.altar == nil || len(deaths) == 0 ||
		len(deaths) > currentDungeonAltarMaximumDeathSequences {
		return errCurrentDungeonAltarQuestDropInvalid
	}
	seen := make(map[uint16]struct{}, len(deaths))
	uncommitted := make([]currentDungeonAltarDeathSequence, 0, len(deaths))
	for _, death := range deaths {
		if death.Sequence == 0 || death.Owner.MonsterCode <= 0 || death.Owner.QuestDropSeed == 0 {
			return errCurrentDungeonAltarQuestDropInvalid
		}
		if _, duplicate := seen[death.Sequence]; duplicate {
			return errCurrentDungeonAltarQuestDropInvalid
		}
		seen[death.Sequence] = struct{}{}
		if _, cached := runtime.altar.questDropResults[death.Sequence]; !cached {
			uncommitted = append(uncommitted, death)
		}
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		for _, death := range uncommitted {
			runtime.altar.questDropResults[death.Sequence] = currentDungeonAltarQuestDropResult{Delivered: true}
		}
		uncommitted = nil
	}
	if len(uncommitted) != 0 {
		sequence := reserveCurrentDungeonQuestProgress(session)
		var commits map[uint16]currentDungeonAltarQuestDropCommit
		var commitErr error
		runCurrentDungeonQuestProgressOrdered(session, sequence, func() {
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			defer cancel()
			commits, commitErr = s.commitCurrentDungeonAltarQuestDrops(ctx, session, runtime, uncommitted)
		})
		if commitErr != nil {
			return commitErr
		}
		for _, death := range uncommitted {
			commit, ok := commits[death.Sequence]
			if !ok {
				return errCurrentDungeonAltarQuestDropInvalid
			}
			runtime.altar.questDropResults[death.Sequence] = currentDungeonAltarQuestDropResult{
				Body:      append([]byte(nil), commit.Body...),
				Delivered: len(commit.Body) == 0,
			}
			s.logGameEvent(session, "game-dungeon-altar-quest-drop-committed",
				"dungeon_id", runtime.Dungeon.ID,
				"sequence", death.Sequence,
				"monster_id", death.Owner.MonsterCode,
				"inventory_changed", commit.Changed,
				"receipt_replayed", commit.Replayed,
				"update_body_len", len(commit.Body),
				"op255_batch_size", len(deaths))
		}
	}
	for _, death := range deaths {
		result, ok := runtime.altar.questDropResults[death.Sequence]
		if !ok {
			return errCurrentDungeonAltarQuestDropInvalid
		}
		if !result.Delivered && len(result.Body) != 0 {
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), result.Body, 0); err != nil {
				return err
			}
			result.Delivered = true
			runtime.altar.questDropResults[death.Sequence] = result
		}
	}
	return nil
}

func (s *Service) commitCurrentDungeonAltarQuestDrops(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	deaths []currentDungeonAltarDeathSequence,
) (map[uint16]currentDungeonAltarQuestDropCommit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return nil, fmt.Errorf("%w: character settlement repository unavailable", errCurrentDungeonAltarQuestDropInvalid)
	}
	characterKey := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	receiptScope := ""
	receiptKeys := make(map[uint16]string, len(deaths))
	for _, death := range deaths {
		scope, key, identityErr := currentDungeonAltarQuestDropReceiptIdentity(characterKey, runtime, death)
		if identityErr != nil {
			return nil, identityErr
		}
		if receiptScope != "" && scope != receiptScope {
			return nil, errCurrentDungeonAltarQuestDropInvalid
		}
		receiptScope = scope
		receiptKeys[death.Sequence] = key
	}
	accountID := s.accountIDForSession(session)
	questHelperActive := false
	growthQuestDropPercent := int64(0)
	if repositories.Account != nil {
		account, found, loadErr := repositories.Account.Load(ctx, accountID)
		questHelperActive = loadErr == nil && found &&
			premium.Active(account, premium.DevilSlotType(premium.DevilSlotQuestHelper), s.gameplayNow().UTC())
	}
	if growthEffect, growthActive := s.currentGrowthContractEffect(ctx, accountID); growthActive {
		growthQuestDropPercent = growthEffect.QuestItemDropRatePercent
	}
	now := s.gameplayNow().UTC()
	committed := make(map[uint16]currentDungeonAltarQuestDropCommit, len(deaths))
	err := repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterKey, func(tx dnfrepo.Group) error {
		if tx.Settings == nil || tx.Quest == nil || tx.Inventory == nil {
			return dnfrepo.ErrCharacterSettlementTransactionUnavailable
		}
		receipts, found, loadErr := tx.Settings.Load(ctx, receiptScope)
		if loadErr != nil {
			return loadErr
		}
		pending := make([]currentDungeonAltarDeathSequence, 0, len(deaths))
		for _, death := range deaths {
			committed[death.Sequence] = currentDungeonAltarQuestDropCommit{}
			if found {
				if raw, exists := receipts.Values[receiptKeys[death.Sequence]]; exists {
					receipt, decodeErr := decodeCurrentDungeonAltarQuestDropReceipt(raw, death.Owner)
					if decodeErr != nil {
						return decodeErr
					}
					committed[death.Sequence] = currentDungeonAltarQuestDropCommit{
						Body: receipt, Changed: len(receipt) != 0, Replayed: true,
					}
					continue
				}
			}
			pending = append(pending, death)
		}
		if len(pending) == 0 {
			return nil
		}

		questRecord, questFound, loadErr := tx.Quest.Load(ctx, characterKey)
		if loadErr != nil {
			return loadErr
		}
		if !questFound {
			return nil
		}
		activeIDs := currentQuestDropActiveIDs(questRecord)
		if len(activeIDs) == 0 {
			return nil
		}
		catalog, catalogErr := s.loadQuestCatalog(ctx)
		if catalogErr != nil || catalog == nil {
			return fmt.Errorf("%w: load quest catalog: %v", errCurrentDungeonAltarQuestDropInvalid, catalogErr)
		}
		candidatesByMonster := make(map[int64][]currentQuestDropCandidate)
		anyCandidates := false
		for _, death := range pending {
			candidates, cached := candidatesByMonster[death.Owner.MonsterCode]
			if !cached {
				candidates = s.currentQuestDropMatchMonster(
					catalog,
					activeIDs,
					runtime.Dungeon.ID,
					int64(runtime.Request.Difficulty),
					death.Owner.MonsterCode,
				)
				candidatesByMonster[death.Owner.MonsterCode] = candidates
			}
			anyCandidates = anyCandidates || len(candidates) != 0
		}
		if !anyCandidates {
			return nil
		}

		inventory, inventoryFound, loadErr := tx.Inventory.Load(ctx, characterKey)
		if loadErr != nil {
			return loadErr
		}
		if !inventoryFound {
			return dnfdungeon.ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		// 账号共享仓要跟角色背包写在同一个事务里：无色/灵魂的落点是账号仓那个固定格，
		// 所以先把它取进事务。取不到（账号仓仓库不可用/账号 id 对不上）时**不报错**，
		// account 保持 nil，落点退回改造前的"只进背包" —— 宁可少搬一次家，也不能
		// 让本来能掉的掉落整体失败（合服/单机库都可能没有账号仓这条记录）。
		var account *dnfrepo.AccountInventoryRecord
		if tx.AccountInventory != nil && accountID != "" {
			record, accountFound, accountErr := tx.AccountInventory.Load(ctx, accountID)
			if accountErr != nil {
				s.logGameEvent(session, "game-dungeon-altar-quest-drop-account-unavailable",
					"account_id", accountID,
					"error", accountErr)
			} else {
				if !accountFound {
					record = dnfrepo.AccountInventoryRecord{AccountID: accountID}
				}
				record = dnfrepo.CloneAccountInventory(record)
				record.AccountID = accountID
				if record.Slots == nil {
					record.Slots = make(map[string]dnfrepo.ItemStack)
				}
				account = &record
			}
		}
		itemCatalog, catalogErr := s.currentPVFItemCatalog()
		if catalogErr != nil || itemCatalog == nil {
			return fmt.Errorf("%w: load item catalog: %v", errCurrentDungeonAltarQuestDropInvalid, catalogErr)
		}
		inventoryChanged := false
		accountChanged := false
		receiptsChanged := false
		if !found {
			receipts = dnfrepo.SettingsRecord{Scope: receiptScope, Values: make(map[string]string)}
		} else {
			receipts = dnfrepo.CloneSettings(receipts)
			if receipts.Values == nil {
				receipts.Values = make(map[string]string)
			}
		}
		for _, death := range pending {
			candidates := candidatesByMonster[death.Owner.MonsterCode]
			if len(candidates) == 0 {
				continue
			}
			rng := newCurrentDungeonDropLCG(death.Owner.QuestDropSeed)
			grantedSlots := make(map[int16]struct{})
			grantedAccountSlots := make([]int16, 0, 2)
			for _, candidate := range candidates {
				held := currentQuestDropHeldCount(inventory, int64(candidate.ItemID))
				if candidate.MaxStack > 0 && held >= candidate.MaxStack {
					continue
				}
				dropRate := currentQuestItemDropRate(candidate.DropRate, growthQuestDropPercent, questHelperActive)
				dropCount := currentQuestDropRoll(int(rng.NextUint32()), candidate.Count, dropRate, candidate.MaxStack, held)
				if dropCount <= 0 {
					continue
				}
				definition, resolveErr := itemCatalog.ResolveItem(candidate.ItemID)
				if resolveErr != nil {
					return resolveErr
				}
				// 落点规则与普通地图杀怪共用同一份实现：账号共享仓优先，余量进背包。
				placement, placeErr := currentQuestDropPlaceCandidate(&inventory, account, definition, uint32(dropCount))
				if placement.AccountChanged {
					accountChanged = true
					grantedAccountSlots = append(grantedAccountSlots, placement.AccountSlot)
				}
				if placement.CharacterChanged {
					grantedSlots[placement.CharacterSlot] = struct{}{}
				}
				if placeErr != nil {
					if errors.Is(placeErr, errDungeonPickupInventoryFull) {
						continue
					}
					return placeErr
				}
			}
			if len(grantedSlots) == 0 && len(grantedAccountSlots) == 0 {
				continue
			}
			updates := make([]currentItemListEntry, 0, len(grantedSlots)+len(grantedAccountSlots))
			for slot := range grantedSlots {
				key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
				updates = append(updates, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, inventory.Slots[key]))
			}
			// 账号共享格也是 list-0 线地址（354..365），ACK 行必须从账号记录里取，
			// 否则客户端看不到这次掉落、只会等下一次列表刷新。
			if account != nil {
				for _, slot := range grantedAccountSlots {
					key := dnfrepo.AccountSharedInventorySlotKey(slot)
					row, rowFound := account.Slots[key]
					if !rowFound {
						continue
					}
					updates = append(updates, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, row))
				}
			}
			sortCurrentItemListEntries(updates)
			body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, updates)
			committed[death.Sequence] = currentDungeonAltarQuestDropCommit{Body: body, Changed: true}
			rawReceipt, encodeErr := encodeCurrentDungeonAltarQuestDropReceipt(death.Owner, body)
			if encodeErr != nil {
				return encodeErr
			}
			receipts.Values[receiptKeys[death.Sequence]] = rawReceipt
			inventoryChanged = true
			receiptsChanged = true
		}
		if inventoryChanged {
			inventory.UpdatedAt = now
			if saveErr := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
				return saveErr
			}
		}
		if accountChanged && account != nil {
			account.UpdatedAt = now
			if saveErr := tx.AccountInventory.Save(ctx, *account); saveErr != nil {
				return saveErr
			}
		}
		if receiptsChanged {
			receipts.UpdatedAt = now
			if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, receipts, dnfrepo.SettingsFieldValues); saveErr != nil {
				return saveErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
}

func currentDungeonAltarQuestDropReceiptIdentity(
	characterKey string,
	runtime *runtimeDungeonState,
	death currentDungeonAltarDeathSequence,
) (string, string, error) {
	characterKey = strings.TrimSpace(characterKey)
	if characterKey == "" || runtime == nil || runtime.lifecycleToken == 0 || runtime.startedAt.IsZero() ||
		death.Sequence == 0 || death.Owner.RoomGeneration == 0 || death.Owner.RoundGeneration == 0 ||
		death.Owner.MonsterCode <= 0 || death.Owner.QuestDropSeed == 0 {
		return "", "", errCurrentDungeonAltarQuestDropInvalid
	}
	runIdentity := fmt.Sprintf(
		"character=%s:dungeon=%d:maze=%d:seed=%d:lifecycle=%d:started=%d",
		characterKey,
		runtime.Dungeon.ID,
		runtime.MazeIndex,
		runtime.Seed,
		runtime.lifecycleToken,
		runtime.startedAt.UnixNano(),
	)
	digest := sha256.Sum256([]byte(runIdentity))
	scope := fmt.Sprintf("character:%s:dungeon_altar_quest_drop:v1:%x", characterKey, digest[:12])
	key := fmt.Sprintf("room:%d:round:%d:sequence:%d", death.Owner.RoomGeneration, death.Owner.RoundGeneration, death.Sequence)
	return scope, key, nil
}

func encodeCurrentDungeonAltarQuestDropReceipt(
	owner currentDungeonAltarSequenceOwner,
	body []byte,
) (string, error) {
	receipt := currentDungeonAltarQuestDropReceipt{
		Version:     currentDungeonAltarQuestDropReceiptVersion,
		MonsterCode: owner.MonsterCode,
		DropSeed:    owner.QuestDropSeed,
		UpdateBody:  base64.StdEncoding.EncodeToString(body),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeCurrentDungeonAltarQuestDropReceipt(
	raw string,
	owner currentDungeonAltarSequenceOwner,
) ([]byte, error) {
	var receipt currentDungeonAltarQuestDropReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, fmt.Errorf("%w: decode receipt: %v", errCurrentDungeonAltarQuestDropInvalid, err)
	}
	if receipt.Version != currentDungeonAltarQuestDropReceiptVersion || receipt.MonsterCode != owner.MonsterCode ||
		receipt.DropSeed != owner.QuestDropSeed {
		return nil, errCurrentDungeonAltarQuestDropInvalid
	}
	body, err := base64.StdEncoding.DecodeString(receipt.UpdateBody)
	if err != nil {
		return nil, fmt.Errorf("%w: decode update body: %v", errCurrentDungeonAltarQuestDropInvalid, err)
	}
	return body, nil
}
