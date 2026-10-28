package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	runtimeRaidRewardReceiptVersion  = 1
	runtimeRaidRewardReceiptValueKey = "runtime_raid_reward_delivery_v1"
	// runtimeRaidRewardResultType is the 0x0258 result type that actually makes
	// the client reveal the phase rewards.
	//
	// The 90 client maps the body's first byte through the jump table at
	// 0x167DF7C (f(0)=0, f(1)=0, f(2)=1, f(3)=2, f(>=4)=4) and only resultType 3
	// reaches 0x1683B90, the reward-window refresh (guarded at 0x1D7CC73
	// `cmp byte ptr [ebp-0x31],3 / jne`, whose only call site in the image is
	// there). That guard is also the only place the ordinal gate
	// (0x1D7CCC3 / 0x1D7CD0E, "因未满足条件�?无法获得奖励�? = 60456) runs.
	//
	// Live evidence overrides the earlier reading, though: the 2026-09-16T12:28
	// settlement -- the last one this server sent with resultType 1, captured in
	// the user's own screenshot -- did reveal both squad cards, while every
	// resultType 3 settlement since 13:11 left them face down. The row delivery
	// 0x1D7CBDA `call [model.vtable[0]](resultType, memberKey, cardIndex, sentinel)`
	// is what the window acts on, so the refresh at 0x1683B90 is not the reveal.
	// Keep 1 (86JP's own value) until R2 lands the matching cardIndex semantics.
	runtimeRaidRewardResultType = 1
)

// currentRaidRewardWireCardIndex is the wire ordinal of one member's clicked card
// slot. Keep it identical to the clicked slot until R2's seat semantics land.
//
// The 0/2 normalization below was added for the resultType==3 branch, whose seat
// gate (0x1D7CCC3 / 0x1D7CCD7 test ordinal 2, 0x1D7CD0E / 0x1D7CD1B test ordinal
// 0) pops 60456 "因未满足条件�?无法获得奖励�? for anything else -- live
// 2026-09-16T13:11Z, character 22 clicked slot 1 and got that popup. The client
// sends the raw clicked slot (0..3, sender 0x0165F390), and the 12:28 settlement
// that did reveal the cards carried exactly those raw ordinals (1 and 2), so the
// normalization only deserves to exist while resultType is 3.
func currentRaidRewardWireCardIndex(clicked byte) byte {
	return clicked
}

var (
	errRuntimeRaidRewardRepository = errors.New("runtime raid reward repository is unavailable")
	errRuntimeRaidRewardReceipt    = errors.New("runtime raid reward receipt is invalid")
	errRuntimeRaidRewardGrant      = errors.New("runtime raid reward grant is invalid")
)

type runtimeRaidRewardInventoryRefresh struct {
	CharacterID uint16
	Slots       []int16
}

type runtimeRaidRewardWalletRefresh struct {
	CharacterID uint16
	AccountID   string
}

type runtimeRaidRewardDeliveryCache struct {
	Phase              byte
	Token              uint64
	ReceiptScope       string
	Rewards            raid.RaidSettlementRewards
	RewardBody         []byte
	PersonalRewardBody []byte
	TeamRewardBody     []byte
	InventoryRefreshes []runtimeRaidRewardInventoryRefresh
	WalletRefreshes    []runtimeRaidRewardWalletRefresh
}

type runtimeRaidRewardCompletionKey struct {
	RaidKey uint32
	Phase   byte
	Token   uint64
}

type runtimeRaidRewardReceipt struct {
	Version             int                              `json:"version"`
	RaidKey             uint32                           `json:"raid_key"`
	StartAttempt        uint64                           `json:"start_attempt"`
	StartedAtUnixNano   int64                            `json:"started_at_unix_nano"`
	Phase               byte                             `json:"phase"`
	Token               uint64                           `json:"token"`
	Rewards             raid.RaidSettlementRewards       `json:"rewards"`
	RewardBody          []byte                           `json:"reward_body"`
	PersonalRewardBody  []byte                           `json:"personal_reward_body,omitempty"`
	TeamRewardBody      []byte                           `json:"team_reward_body,omitempty"`
	Members             []runtimeRaidRewardReceiptMember `json:"members"`
	CommittedAtUnixNano int64                            `json:"committed_at_unix_nano"`
}

type runtimeRaidRewardReceiptMember struct {
	CharacterID uint16                              `json:"character_id"`
	AccountID   string                              `json:"account_id,omitempty"`
	Slots       []int16                             `json:"slots"`
	PostState   []runtimeRaidRewardReceiptSlotState `json:"post_state"`
	Voucher     dnfcerashop.VoucherCounts           `json:"voucher_credits,omitempty"`
	// Overflow 是这一位成员**背包那一栏放不下、改走邮件**的奖励行。
	//
	// 它同时是重投的依据：邮件在事务外发（邮件有自己的工作单元），所以"回执已落、
	// 邮件还没发"这个窗口只能靠回执重放补上 —— deliverRuntimeRaidRewardOverflowMail
	// 每次都按这份清单重投，AppendSystemMail 的 completion_key 保证只落一封。
	Overflow []runtimeRaidRewardOverflowRow `json:"overflow,omitempty"`
}

// runtimeRaidRewardOverflowRow 是一行走邮件的奖励（已经是解析后的最终物品）。
type runtimeRaidRewardOverflowRow struct {
	ItemID int64 `json:"item_id"`
	Count  int64 `json:"count"`
}

type runtimeRaidRewardReceiptSlotState struct {
	Slot  int16             `json:"slot"`
	Stack dnfrepo.ItemStack `json:"stack"`
}

type runtimeRaidRewardGrantPlan struct {
	Resolved   antonRewardPVFResolved
	Definition dungeonDropItemDefinition
}

type runtimeRaidRewardMemberPlan struct {
	Member    runtimeRaidMemberState
	CardIndex byte
	Grants    []runtimeRaidRewardGrantPlan
	Voucher   dnfcerashop.VoucherCounts
}

type runtimeRaidRewardPlan struct {
	Rewards      raid.RaidSettlementRewards
	Body         []byte
	PersonalBody []byte
	TeamBody     []byte
	Members      []runtimeRaidRewardMemberPlan
}

type runtimeRaidRewardRandomSource struct {
	next func() uint64
}

func (source runtimeRaidRewardRandomSource) Uint64() uint64 {
	if source.next != nil {
		return source.next()
	}
	return rand.Uint64()
}

func (s *Service) runtimeRaidAntonRewardCatalog() (*antonRewardPVFCatalog, error) {
	if s == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	s.raidAntonRewardMu.Lock()
	defer s.raidAntonRewardMu.Unlock()
	if s.raidAntonReward != nil || s.raidAntonRewardErr != nil {
		return s.raidAntonReward, s.raidAntonRewardErr
	}
	items, err := s.currentPVFItemCatalog()
	if err == nil {
		var config *raid.AntonConfig
		config, err = s.runtimeRaidAntonConfig()
		if err == nil {
			s.raidAntonReward, err = newAntonRewardPVFCatalog(items, config)
			if err == nil {
				s.attachRaidRewardOverrides(s.raidAntonReward, "anton")
			}
		}
	}
	if err != nil {
		s.raidAntonRewardErr = fmt.Errorf("load current anton reward catalog: %w", err)
	}
	return s.raidAntonReward, s.raidAntonRewardErr
}

func (s *Service) runtimeRaidLukeRewardCatalog() (*antonRewardPVFCatalog, error) {
	if s == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	s.raidLukeRewardMu.Lock()
	defer s.raidLukeRewardMu.Unlock()
	if s.raidLukeReward != nil || s.raidLukeRewardErr != nil {
		return s.raidLukeReward, s.raidLukeRewardErr
	}
	items, err := s.currentPVFItemCatalog()
	if err == nil {
		var config *raid.LukeConfig
		config, err = s.runtimeRaidLukeConfig()
		if err == nil {
			s.raidLukeReward, err = newRaidRewardPVFCatalog(items, config.Raid, nil, false, currentLukeRewardPhaseCount)
			if err == nil {
				s.attachRaidRewardOverrides(s.raidLukeReward, "luke")
			}
		}
	}
	if err != nil {
		s.raidLukeRewardErr = fmt.Errorf("load current luke reward catalog: %w", err)
	}
	return s.raidLukeReward, s.raidLukeRewardErr
}

func (s *Service) runtimeRaidRewardCatalogForState(state runtimeRaidState) (*antonRewardPVFCatalog, error) {
	// 超时空的奖励表不在 etc/raid/fiendwar 之外的任何地方：它的 [CLEAR REWARD ITEM]
	// 由 parseFiendWarBattle 挂到合成的 Battle.Raid 上，所以目录用 PVF 物品表 +
	// runtimeRaidBaseConfig 现搭，而不是去读 luke/anton 的奖励文档。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			return nil, err
		}
		catalog, err := newRaidRewardPVFCatalog(items, runtimeRaidBaseConfig(state), nil, false, currentFiendWarRewardPhaseCount)
		if err != nil {
			return nil, err
		}
		s.attachRaidRewardOverrides(catalog, "fiendwar")
		return catalog, nil
	}
	if state.RaidMode == runtimeRaidOwnerLuke {
		return s.runtimeRaidLukeRewardCatalog()
	}
	return s.runtimeRaidAntonRewardCatalog()
}

// runtimeRaidFiendWarRewardCatalog 给网页 GM 的“奖励池”页提供一份超时空
// 全局目录。它和实时结算使用同一份 fiendwar.etc 配置与同一套 PVF 解析器，
// 但不依赖某一场正在进行的团本状态。
func (s *Service) runtimeRaidFiendWarRewardCatalog() (*antonRewardPVFCatalog, error) {
	if s == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	config, err := s.runtimeRaidFiendWarConfig()
	if err != nil {
		return nil, err
	}
	if config == nil || config.Battle == nil || config.Battle.Raid == nil {
		return nil, fmt.Errorf("超时空奖励配置不可用")
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	catalog, err := newRaidRewardPVFCatalog(items, config.Battle.Raid, nil, false, currentFiendWarRewardPhaseCount)
	if err != nil {
		return nil, err
	}
	s.attachRaidRewardOverrides(catalog, "fiendwar")
	return catalog, nil
}

func (s *Service) commitRuntimeRaidSettlementRewards(
	raidKey uint32,
	phase byte,
	now time.Time,
) (runtimeRaidSettlementRewardDelivery, error) {
	if s == nil {
		return runtimeRaidSettlementRewardDelivery{}, errRuntimeRaidRewardRepository
	}
	now = runtimeRaidNormalizeTime(now)
	s.raidRewardCommitMu.Lock()
	defer s.raidRewardCommitMu.Unlock()

	state, token, err := s.reserveRuntimeRaidRewardDelivery(raidKey, phase, now)
	if err != nil {
		return runtimeRaidSettlementRewardDelivery{}, err
	}
	if cache := state.Lifecycle.RewardDelivery; cache != nil && state.Lifecycle.RewardDeliveryCommitted {
		return runtimeRaidRewardDeliveryFromCache(state, cache), nil
	}

	receiptScope := runtimeRaidRewardReceiptScope(state, phase, token)
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RaidRewards == nil {
		return runtimeRaidSettlementRewardDelivery{}, errRuntimeRaidRewardRepository
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()

	characterIDs := make([]string, 0, len(state.FrozenMembers))
	accountIDs := make([]string, 0, len(state.FrozenMembers))
	accountByCharacter := make(map[string]string, len(state.FrozenMembers))
	seenAccounts := make(map[string]struct{}, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		characterID := strconv.Itoa(int(member.CharID))
		character, found, loadErr := repositories.Character.Load(ctx, characterID)
		if loadErr != nil {
			return runtimeRaidSettlementRewardDelivery{}, loadErr
		}
		if !found || strings.TrimSpace(character.AccountID) == "" {
			return runtimeRaidSettlementRewardDelivery{}, fmt.Errorf("%w: character=%s account missing", errRuntimeRaidRewardGrant, characterID)
		}
		accountID := strings.TrimSpace(character.AccountID)
		characterIDs = append(characterIDs, characterID)
		accountByCharacter[characterID] = accountID
		if _, duplicate := seenAccounts[accountID]; !duplicate {
			seenAccounts[accountID] = struct{}{}
			accountIDs = append(accountIDs, accountID)
		}
	}
	rewardAssets, ok := repositories.RaidRewards.(dnfrepo.RaidRewardAssetUnitOfWork)
	if !ok {
		return runtimeRaidSettlementRewardDelivery{}, errRuntimeRaidRewardRepository
	}
	var committed runtimeRaidRewardReceipt
	var plan runtimeRaidRewardPlan
	walletRefreshes := make([]runtimeRaidRewardWalletRefresh, 0, len(state.FrozenMembers))
	planResolved := false
	err = rewardAssets.WithinRaidRewardAssets(
		ctx,
		characterIDs,
		accountIDs,
		receiptScope,
		func(accounts dnfrepo.AccountRepository, inventories dnfrepo.InventoryRepository, receipts dnfrepo.SettingsRepository) error {
			receiptRecord, found, loadErr := receipts.Load(ctx, receiptScope)
			if loadErr != nil {
				return loadErr
			}
			if found {
				existing, decodeErr := decodeRuntimeRaidRewardReceipt(receiptRecord)
				if decodeErr != nil {
					return decodeErr
				}
				if validateErr := s.validateRuntimeRaidRewardReceipt(&state, phase, token, existing); validateErr != nil {
					return validateErr
				}
				if validateErr := validateRuntimeRaidRewardReceiptInventories(ctx, inventories, existing); validateErr != nil {
					return validateErr
				}
				committed = existing
				return nil
			}

			if !planResolved {
				catalog, catalogErr := s.runtimeRaidRewardCatalogForState(state)
				if catalogErr != nil {
					return catalogErr
				}
				resolved, resolveErr := s.resolveRuntimeRaidRewardPlan(state, phase, now, catalog)
				if resolveErr != nil {
					return resolveErr
				}
				plan = resolved
				planResolved = true
			}

			receipt := runtimeRaidRewardReceipt{
				Version:             runtimeRaidRewardReceiptVersion,
				RaidKey:             state.RaidKey,
				StartAttempt:        state.StartAttempt,
				StartedAtUnixNano:   state.Lifecycle.StartedAt.UnixNano(),
				Phase:               phase,
				Token:               token,
				Rewards:             cloneRuntimeRaidSettlementRewards(plan.Rewards),
				RewardBody:          append([]byte(nil), plan.Body...),
				PersonalRewardBody:  append([]byte(nil), plan.PersonalBody...),
				TeamRewardBody:      append([]byte(nil), plan.TeamBody...),
				Members:             make([]runtimeRaidRewardReceiptMember, 0, len(plan.Members)),
				CommittedAtUnixNano: now.UnixNano(),
			}
			for _, memberPlan := range plan.Members {
				characterID := strconv.Itoa(int(memberPlan.Member.CharID))
				inventory, inventoryFound, inventoryErr := inventories.Load(ctx, characterID)
				if inventoryErr != nil {
					return inventoryErr
				}
				if !inventoryFound {
					return fmt.Errorf("%w: character=%s inventory missing", errRuntimeRaidRewardGrant, characterID)
				}
				inventory = dnfrepo.CloneInventory(inventory)
				touched := make(map[int16]struct{})
				overflow := make([]runtimeRaidRewardOverflowRow, 0, 1)
				if !memberPlan.Voucher.IsZero() {
					accountID := accountByCharacter[characterID]
					account, accountFound, accountErr := accounts.Load(ctx, accountID)
					if accountErr != nil {
						return accountErr
					}
					if !accountFound {
						return fmt.Errorf("%w: character=%s account=%s missing", errRuntimeRaidRewardGrant, characterID, accountID)
					}
					account = dnfrepo.CloneAccount(account)
					if _, creditErr := dnfcerashop.CreditVoucherCounts(&account, memberPlan.Voucher); creditErr != nil {
						return fmt.Errorf("%w: character=%s voucher credit: %v", errRuntimeRaidRewardGrant, characterID, creditErr)
					}
					account.UpdatedAt = now
					if saveErr := accounts.Save(ctx, account); saveErr != nil {
						return saveErr
					}
					walletRefreshes = append(walletRefreshes, runtimeRaidRewardWalletRefresh{
						CharacterID: memberPlan.Member.CharID,
						AccountID:   accountID,
					})
				}
				for _, grant := range memberPlan.Grants {
					slots, grantErr := grantCurrentCeraShopProduct(
						&inventory,
						grant.Definition,
						grant.Resolved.Count,
						s.currentInventoryPlacementMainSlotCount(ctx, &inventory, characterID),
					)
					if grantErr != nil {
						// 背包那一栏放不下 —— **这一位**的行改走邮件，不是整队失败。
						//
						// 这条路上的"全队一个事务"曾经是致命的：只要队里任何一个人的
						// 对应分页满了，第一个 grantErr 就让整个闭包返回错误、事务整体
						// 回滚，四个人的奖励一个都发不出去，回执也不落，客户端的牌就
						// 永远停在"翻不开"（真机 2026-10-02 16:42，raid 12058625，
						// 分片 20261002-019：
						//   game-raid-reward-auto-flip-commit-failed flipped=4
						//   error=... item=490008672 ... range=65..120）。
						//
						// grantCurrentCeraShopProductWithCarryLimit 在失败时已经把背包
						// 快照回滚了（它自己的注释就写着"由调用方按原有溢出（邮件）分支
						// 处理"），所以这里 continue 到下一行是安全的：不会有"背包放进
						// 一部分 + 邮件又发一整份"的半截状态。
						// 两个哨兵一起认：dungeon_drop_pickup.go 的映射会把底层错误
						// 用 %v 吃掉、只留 bridge 自己的哨兵，所以只认一个不够稳
						// （与 epic_book.go:208 / adventure_runtime.go:48 同一写法）。
						if errors.Is(grantErr, errDungeonPickupInventoryFull) ||
							errors.Is(grantErr, dnfdungeon.ErrPickupInventoryFull) {
							overflow = append(overflow, runtimeRaidRewardOverflowRow{
								ItemID: int64(grant.Resolved.ItemID),
								Count:  int64(grant.Resolved.Count),
							})
							continue
						}
						return fmt.Errorf(
							"%w: character=%s item=%d count=%d: %v",
							errRuntimeRaidRewardGrant,
							characterID,
							grant.Resolved.ItemID,
							grant.Resolved.Count,
							grantErr,
						)
					}
					for _, slot := range slots {
						if slot > math.MaxInt16 {
							return fmt.Errorf("%w: character=%s slot=%d", errRuntimeRaidRewardGrant, characterID, slot)
						}
						touched[int16(slot)] = struct{}{}
					}
				}
				// 整份奖励都进了邮件时这一位没有任何格位被动过，不必写回背包
				// （克隆出来的 inventory 直接丢掉）。
				if len(touched) != 0 {
					inventory.UpdatedAt = now
					if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
						return saveErr
					}
				}
				slots := sortedRuntimeRaidRewardSlots(touched)
				postState, snapshotErr := runtimeRaidRewardReceiptPostState(inventory, slots)
				if snapshotErr != nil {
					return fmt.Errorf("%w: character=%s: %v", errRuntimeRaidRewardReceipt, characterID, snapshotErr)
				}
				receipt.Members = append(receipt.Members, runtimeRaidRewardReceiptMember{
					CharacterID: memberPlan.Member.CharID,
					AccountID:   accountByCharacter[characterID],
					Slots:       slots,
					PostState:   postState,
					Voucher:     memberPlan.Voucher,
					Overflow:    overflow,
				})
			}
			rawReceipt, encodeErr := json.Marshal(receipt)
			if encodeErr != nil {
				return encodeErr
			}
			if saveErr := receipts.Save(ctx, dnfrepo.SettingsRecord{
				Scope: receiptScope,
				Values: map[string]string{
					runtimeRaidRewardReceiptValueKey: string(rawReceipt),
				},
				UpdatedAt: now,
			}); saveErr != nil {
				return saveErr
			}
			committed = receipt
			return nil
		},
	)
	if err != nil {
		return runtimeRaidSettlementRewardDelivery{}, err
	}
	if len(walletRefreshes) == 0 {
		for _, member := range committed.Members {
			if member.Voucher.IsZero() || strings.TrimSpace(member.AccountID) == "" {
				continue
			}
			walletRefreshes = append(walletRefreshes, runtimeRaidRewardWalletRefresh{
				CharacterID: member.CharacterID,
				AccountID:   member.AccountID,
			})
		}
	}
	// 溢出邮件在事务**外**发：邮件有自己的工作单元（WithinMailboxAssets），进不了
	// WithinRaidRewards 那个事务。奖励已经 durable 提交，所以这里失败只记日志、绝不
	// 回滚 —— 与 currentUpgradeRefundMailer 的口径一致（为了补一封邮件去撤销一次
	// 已经落库的结算，是更坏的结局）。回执里记着 Overflow，所以重投随时能补上，
	// AppendSystemMail 的 completion_key 保证同一份回执只落一封邮件。
	s.deliverRuntimeRaidRewardOverflowMail(ctx, receiptScope, committed)
	cache, err := s.runtimeRaidRewardDeliveryCacheFromReceipt(&state, receiptScope, committed, walletRefreshes)
	if err != nil {
		return runtimeRaidSettlementRewardDelivery{}, err
	}
	state, err = s.storeRuntimeRaidRewardDeliveryCommitted(raidKey, phase, token, cache, now)
	if err != nil {
		return runtimeRaidSettlementRewardDelivery{}, err
	}
	return runtimeRaidRewardDeliveryFromCache(state, cache), nil
}

func (s *Service) completeRuntimeRaidSettlementRewards(
	raidKey uint32,
	phase byte,
	token uint64,
	now time.Time,
) (runtimeRaidSettlementRewardCompletion, error) {
	if s == nil || token == 0 {
		return runtimeRaidSettlementRewardCompletion{}, errRuntimeRaidRewardToken
	}
	now = runtimeRaidNormalizeTime(now)
	s.raidRewardCommitMu.Lock()
	defer s.raidRewardCommitMu.Unlock()
	key := runtimeRaidRewardCompletionKey{RaidKey: raidKey, Phase: phase, Token: token}
	if completed, found := s.raidRewardCompleted[key]; found {
		return cloneRuntimeRaidRewardCompletion(completed), nil
	}
	if err := s.validateRuntimeRaidRewardDeliveryCommitted(raidKey, phase, token); err != nil {
		return runtimeRaidSettlementRewardCompletion{}, err
	}

	switch phase {
	case runtimeRaidDungeonPhaseOne:
		if s.runtimeRaidIsSingle(raidKey) {
			finished, err := s.finalizeRuntimeRaid(raidKey, token, now)
			if err != nil {
				return runtimeRaidSettlementRewardCompletion{}, err
			}
			completion := runtimeRaidSettlementRewardCompletion{Raid: finished}
			s.storeRaidRewardCompletionLocked(key, cloneRuntimeRaidRewardCompletion(completion))
			return completion, nil
		}
		state, timer, err := s.enterRuntimeRaidStandby(raidKey, phase, now)
		if err != nil {
			return runtimeRaidSettlementRewardCompletion{}, err
		}
		return runtimeRaidSettlementRewardCompletion{Raid: state, StandbyTimer: &timer}, nil
	case runtimeRaidDungeonPhaseTwo:
		state, err := s.finalizeRuntimeRaid(raidKey, token, now)
		if err != nil {
			return runtimeRaidSettlementRewardCompletion{}, err
		}
		completion := runtimeRaidSettlementRewardCompletion{Raid: state}
		s.storeRaidRewardCompletionLocked(key, cloneRuntimeRaidRewardCompletion(completion))
		return completion, nil
	default:
		return runtimeRaidSettlementRewardCompletion{}, errRuntimeRaidSettlementPhase
	}
}

func (s *Service) reserveRuntimeRaidRewardDelivery(
	raidKey uint32,
	phase byte,
	now time.Time,
) (runtimeRaidState, uint64, error) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, 0, errRuntimeRaidNotFound
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if state.Lifecycle.Status == runtimeRaidLifecycleStandby && phase == runtimeRaidDungeonPhaseOne &&
		state.Lifecycle.Phase == phase && state.Lifecycle.RewardDeliveryCommitted && state.Lifecycle.RewardDelivery != nil {
		return cloneRuntimeRaidState(*state), state.Lifecycle.RewardToken, nil
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.Phase != phase ||
		(phase != runtimeRaidDungeonPhaseOne && phase != runtimeRaidDungeonPhaseTwo) ||
		!s.runtimeRaidRewardRosterConfirmedLocked(state) {
		return runtimeRaidState{}, 0, errRuntimeRaidRewardNotReady
	}
	token := state.Lifecycle.RewardToken
	if token == 0 {
		// The eligible roster is materialised here, once: the token and every
		// receipt planned from it must describe the same member set, so the record
		// of members the settlement gave up on stops moving at this instant.
		s.freezeRuntimeRaidRewardSkipsLocked(state)
		token = s.runtimeRaidRewardTokenLocked(state, phase)
		state.Lifecycle.RewardToken = token
		if state.Lifecycle.RewardOpenedAt.IsZero() {
			state.Lifecycle.RewardOpenedAt = now
		}
		state.Lifecycle.Revision++
	}
	if cache := state.Lifecycle.RewardDelivery; cache != nil && (cache.Token != token || cache.Phase != phase) {
		return runtimeRaidState{}, 0, errRuntimeRaidRewardToken
	}
	return cloneRuntimeRaidState(*state), token, nil
}

func runtimeRaidRewardPoolKinds(catalog *antonRewardPVFCatalog, phaseIndex int) []string {
	granted := [...]string{"party_card", "squad_item"}
	kinds := make([]string, 0, len(granted))
	for _, kind := range granted {
		pool, ok := catalog.Pool(phaseIndex, kind)
		if !ok || len(pool.Entries) == 0 || pool.TotalWeight == 0 {
			continue
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

func (s *Service) resolveRuntimeRaidRewardPlan(
	state runtimeRaidState,
	phase byte,
	now time.Time,
	catalog *antonRewardPVFCatalog,
) (runtimeRaidRewardPlan, error) {
	if catalog == nil || runtimeRaidBaseConfig(state) == nil || phase == 0 {
		return runtimeRaidRewardPlan{}, errRuntimeRaidSettlementPhase
	}
	phaseIndex, ok := runtimeRaidRewardPhaseIndex(state, phase)
	if !ok {
		return runtimeRaidRewardPlan{}, errRuntimeRaidSettlementPhase
	}
	// 本阶段实际声明了哪些奖池。卢克一阶段（etc/raid/luke/luke.etc [PHASE] 进入源生宫殿）
	// 只声明 party_card 与 pcroom_card，没有 squad_item；以前这里写死
	// {"party_card", "squad_item"}，问它要一个不存在的 squad_item 池会让 ResolvePool
	// 直接报 "anton reward PVF pool is unresolved: phase=0 kind=squad_item" ——
	// 整个结算因此失败：选牌后不发奖、不进等待、二阶段永远起不来。
	// （移植上游 bb52082 之后的 runtimeRaidRewardPoolKinds；本树此前漏了它）
	poolKinds := runtimeRaidRewardPoolKinds(catalog, phaseIndex)
	if len(poolKinds) == 0 {
		return runtimeRaidRewardPlan{}, fmt.Errorf("%w: phase=%d no granted reward pool", errRuntimeRaidRewardGrant, phase)
	}
	members := s.runtimeRaidRewardEligibleMembersLocked(&state)
	runtimeRaidSortMembers(members)
	plan := runtimeRaidRewardPlan{
		Rewards: raid.RaidSettlementRewards{
			ResultType: runtimeRaidRewardResultType,
			Members:    make([]raid.RaidSettlementMemberRewards, 0, len(members)),
		},
		Members: make([]runtimeRaidRewardMemberPlan, 0, len(members)),
	}
	teamRewards := raid.RaidSettlementRewards{
		ResultType: runtimeRaidTeamRewardResultType,
		Members:    make([]raid.RaidSettlementMemberRewards, 0, len(members)),
	}
	personalRewards := raid.RaidSettlementRewards{
		ResultType: 1,
		Members:    make([]raid.RaidSettlementMemberRewards, 0, len(members)),
	}
	rng := runtimeRaidRewardRandomSource{next: s.raidRewardRandom}
	for _, member := range members {
		selection, found := state.Lifecycle.RewardSelections[member.CharID]
		if !found || selection.Mode != 0 {
			return runtimeRaidRewardPlan{}, errRuntimeRaidRewardRosterPending
		}
		memberPlan := runtimeRaidRewardMemberPlan{Member: member, CardIndex: selection.Index, Grants: make([]runtimeRaidRewardGrantPlan, 0, len(poolKinds))}
		// selection.Index is already normalised to the client's accepted ordinals
		// (0 or 2) by selectRuntimeRaidSettlementCard, so the stored receipt, the
		// plan and the wire row agree.
		wireMember := raid.RaidSettlementMemberRewards{MemberKey: member.CharID, CardIndex: selection.Index, Rewards: make([]raid.RaidSettlementReward, 0, len(poolKinds))}
		// 超时空：清数没达到 [MINIMAL DUNGEON CLEAR COUNT] 的成员照样走完结算
		// 流程（要确认影片、要选牌，否则整团的结算门永远等不满），但一个池子都不给。
		// runtimeFiendWarRewardEligible 对安东/卢克恒真，所以这条对它们零影响。
		memberKinds := poolKinds
		if !runtimeFiendWarRewardEligible(state, member.CharID) {
			memberKinds = nil
		}
		teamWireMember := raid.RaidSettlementMemberRewards{
			MemberKey: member.CharID,
			CardIndex: 2,
			Rewards:   make([]raid.RaidSettlementReward, 0, len(memberKinds)),
		}
		personalWireMember := raid.RaidSettlementMemberRewards{
			MemberKey: member.CharID,
			CardIndex: selection.Index,
			Rewards:   make([]raid.RaidSettlementReward, 0, len(memberKinds)),
		}
		for _, kind := range memberKinds {
			resolvedRewards, err := catalog.ResolvePoolRewards(phaseIndex, kind, rng, member)
			if err != nil {
				return runtimeRaidRewardPlan{}, err
			}
			for _, resolved := range resolvedRewards {
				if resolved.ItemID == 0 || resolved.Count == 0 || resolved.Count > math.MaxUint16 {
					return runtimeRaidRewardPlan{}, fmt.Errorf(
						"%w: character=%d phase=%d kind=%s item=%d count=%d",
						errRuntimeRaidRewardGrant,
						member.CharID,
						phase,
						kind,
						resolved.ItemID,
						resolved.Count,
					)
				}
				definition, err := catalog.ResolveFinalItem(resolved.ItemID)
				if err != nil {
					return runtimeRaidRewardPlan{}, err
				}
				definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
				if err != nil {
					return runtimeRaidRewardPlan{}, err
				}
				if definition.ItemID != resolved.ItemID || definition.Kind != resolved.ItemKind {
					return runtimeRaidRewardPlan{}, fmt.Errorf(
						"%w: character=%d resolved=%d/%s definition=%d/%s",
						errRuntimeRaidRewardGrant,
						member.CharID,
						resolved.ItemID,
						resolved.ItemKind,
						definition.ItemID,
						definition.Kind,
					)
				}
				wireReward := raid.RaidSettlementReward{
					Kind:          resolved.Flag,
					ItemIDOrValue: resolved.ItemID,
					CountOrAux:    uint16(resolved.Count),
				}
				wireMember.Rewards = append(wireMember.Rewards, wireReward)
				switch resolved.PoolKind {
				case "party_card":
					personalWireMember.Rewards = append(personalWireMember.Rewards, wireReward)
				case "squad_item":
					teamWireMember.Rewards = append(teamWireMember.Rewards, wireReward)
				}
				if voucher := dnfcerashop.VoucherKindOf(int64(resolved.ItemID)); voucher != dnfcerashop.VoucherNone {
					counts := dnfcerashop.VoucherCounts{}
					if voucher == dnfcerashop.VoucherTokenCera {
						counts.TokenCera = int64(resolved.Count)
					} else {
						counts.HappyTokenCera = int64(resolved.Count)
					}
					memberPlan.Voucher = memberPlan.Voucher.Add(counts)
					continue
				}
				memberPlan.Grants = append(memberPlan.Grants, runtimeRaidRewardGrantPlan{Resolved: resolved, Definition: definition})
			}
		}
		plan.Members = append(plan.Members, memberPlan)
		plan.Rewards.Members = append(plan.Rewards.Members, wireMember)
		if len(teamWireMember.Rewards) > 0 {
			teamRewards.Members = append(teamRewards.Members, teamWireMember)
		}
		if len(personalWireMember.Rewards) > 0 {
			personalRewards.Members = append(personalRewards.Members, personalWireMember)
		}
	}
	body, err := raid.BuildRaidSettlementRewardsBody(plan.Rewards)
	if err != nil {
		return runtimeRaidRewardPlan{}, err
	}
	plan.Body = body
	if len(personalRewards.Members) > 0 {
		personalBody, err := raid.BuildRaidSettlementRewardsBody(personalRewards)
		if err != nil {
			return runtimeRaidRewardPlan{}, err
		}
		plan.PersonalBody = personalBody
	}
	if len(teamRewards.Members) > 0 {
		teamBody, err := raid.BuildRaidSettlementRewardsBody(teamRewards)
		if err != nil {
			return runtimeRaidRewardPlan{}, err
		}
		plan.TeamBody = teamBody
	}
	return plan, nil
}

func (s *Service) storeRuntimeRaidRewardDeliveryCommitted(
	raidKey uint32,
	phase byte,
	token uint64,
	cache *runtimeRaidRewardDeliveryCache,
	now time.Time,
) (runtimeRaidState, error) {
	if cache == nil || cache.Token != token || cache.Phase != phase || token == 0 {
		return runtimeRaidState{}, errRuntimeRaidRewardReceipt
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.Phase != phase ||
		state.Lifecycle.RewardToken != token || !s.runtimeRaidRewardRosterConfirmedLocked(state) {
		return runtimeRaidState{}, errRuntimeRaidRewardNotReady
	}
	if existing := state.Lifecycle.RewardDelivery; existing != nil {
		if existing.Token != token || existing.Phase != phase || !bytes.Equal(existing.RewardBody, cache.RewardBody) {
			return runtimeRaidState{}, errRuntimeRaidRewardReceipt
		}
	} else {
		state.Lifecycle.RewardDelivery = cloneRuntimeRaidRewardDeliveryCache(cache)
	}
	if !state.Lifecycle.RewardDeliveryCommitted {
		state.Lifecycle.RewardDeliveryCommitted = true
		state.Lifecycle.RewardDeliveryCommittedAt = now
		state.Lifecycle.Revision++
	}
	return cloneRuntimeRaidState(*state), nil
}

func (s *Service) validateRuntimeRaidRewardDeliveryCommitted(raidKey uint32, phase byte, token uint64) error {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return errRuntimeRaidNotFound
	}
	cache := state.Lifecycle.RewardDelivery
	if state.Lifecycle.Phase != phase || state.Lifecycle.RewardToken != token || token == 0 ||
		!state.Lifecycle.RewardDeliveryCommitted || cache == nil || cache.Phase != phase || cache.Token != token ||
		!s.runtimeRaidRewardRosterConfirmedLocked(state) {
		return errRuntimeRaidRewardNotReady
	}
	return nil
}

func (s *Service) sendRuntimeRaidSettlementInventoryRefreshes(refreshes []runtimeRaidRewardInventoryRefresh) error {
	ordered := cloneRuntimeRaidRewardInventoryRefreshes(refreshes)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CharacterID < ordered[j].CharacterID })
	for _, refresh := range ordered {
		session, online := s.onlineGameSession(refresh.CharacterID)
		if !online || len(refresh.Slots) == 0 {
			continue
		}
		itemRefreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(refresh.Slots))
		for _, slot := range refresh.Slots {
			itemRefreshes = append(itemRefreshes, alignedcmd.ItemSlotRefresh{
				ListType:  dnfrepo.MainInventoryListType,
				SlotIndex: slot,
			})
		}
		if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "runtime_anton_raid_reward", itemRefreshes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendRuntimeRaidSettlementWalletRefreshes(refreshes []runtimeRaidRewardWalletRefresh) error {
	if len(refreshes) == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return errRuntimeRaidRewardRepository
	}
	ordered := append([]runtimeRaidRewardWalletRefresh(nil), refreshes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CharacterID < ordered[j].CharacterID })
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	for _, refresh := range ordered {
		session, online := s.onlineGameSession(refresh.CharacterID)
		if !online || session == nil {
			continue
		}
		_, found, err := repositories.Account.Load(ctx, refresh.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: character=%d account=%s missing", errRuntimeRaidRewardRepository, refresh.CharacterID, refresh.AccountID)
		}
		if err := s.sendCurrentCeraShopWalletProjection(ctx, session, "runtime_raid_reward_voucher_credit"); err != nil {
			return err
		}
	}
	return nil
}

// runtimeRaidRewardTokenLocked keys the settlement on the raid, its start
// attempt and every member the receipt will actually pay -- a member the
// settlement gave up on is not part of the key, because it is not part of the
// grant. Caller holds raidMu.
func (s *Service) runtimeRaidRewardTokenLocked(state *runtimeRaidState, phase byte) uint64 {
	hash := fnv.New64a()
	var buffer [8]byte
	binary.LittleEndian.PutUint32(buffer[:4], state.RaidKey)
	_, _ = hash.Write(buffer[:4])
	binary.LittleEndian.PutUint64(buffer[:], state.StartAttempt)
	_, _ = hash.Write(buffer[:])
	binary.LittleEndian.PutUint64(buffer[:], uint64(state.Lifecycle.StartedAt.UnixNano()))
	_, _ = hash.Write(buffer[:])
	_, _ = hash.Write([]byte{phase})
	members := s.runtimeRaidRewardEligibleMembersLocked(state)
	runtimeRaidSortMembers(members)
	for _, member := range members {
		binary.LittleEndian.PutUint16(buffer[:2], member.CharID)
		_, _ = hash.Write(buffer[:2])
		selection := state.Lifecycle.RewardSelections[member.CharID]
		_, _ = hash.Write([]byte{selection.Mode, selection.Index})
	}
	token := hash.Sum64()
	if token == 0 {
		return 1
	}
	return token
}

func runtimeRaidRewardReceiptScope(state runtimeRaidState, phase byte, token uint64) string {
	return fmt.Sprintf(
		"raid_reward:v1:%08x:%x:%x:%d:%016x",
		state.RaidKey,
		state.StartAttempt,
		uint64(state.Lifecycle.StartedAt.UnixNano()),
		phase,
		token,
	)
}

func decodeRuntimeRaidRewardReceipt(record dnfrepo.SettingsRecord) (runtimeRaidRewardReceipt, error) {
	raw := ""
	if record.Values != nil {
		raw = record.Values[runtimeRaidRewardReceiptValueKey]
	}
	if raw == "" {
		return runtimeRaidRewardReceipt{}, errRuntimeRaidRewardReceipt
	}
	var receipt runtimeRaidRewardReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return runtimeRaidRewardReceipt{}, fmt.Errorf("%w: %v", errRuntimeRaidRewardReceipt, err)
	}
	return receipt, nil
}

// validateRuntimeRaidRewardReceipt proves a stored receipt still describes this
// raid's settlement. The member rows are checked against the same eligible
// roster the plan was built from (the frozen roster minus the members the
// settlement gave up on), never against the raw frozen roster: a member that
// dropped before it confirmed has no row and must not be demanded one.
//
// Caller holds raidMu.
func (s *Service) validateRuntimeRaidRewardReceipt(
	state *runtimeRaidState,
	phase byte,
	token uint64,
	receipt runtimeRaidRewardReceipt,
) error {
	members := s.runtimeRaidRewardEligibleMembersLocked(state)
	runtimeRaidSortMembers(members)
	if receipt.Version != runtimeRaidRewardReceiptVersion || receipt.RaidKey != state.RaidKey ||
		receipt.StartAttempt != state.StartAttempt || receipt.StartedAtUnixNano != state.Lifecycle.StartedAt.UnixNano() ||
		receipt.Phase != phase || receipt.Token != token || token == 0 || len(receipt.Members) != len(members) {
		return errRuntimeRaidRewardReceipt
	}
	body, err := raid.BuildRaidSettlementRewardsBody(receipt.Rewards)
	if err != nil || !bytes.Equal(body, receipt.RewardBody) {
		return errRuntimeRaidRewardReceipt
	}
	if len(receipt.Rewards.Members) != len(members) {
		return errRuntimeRaidRewardReceipt
	}
	for index, member := range members {
		selection, found := state.Lifecycle.RewardSelections[member.CharID]
		wireMember := receipt.Rewards.Members[index]
		receiptMember := receipt.Members[index]
		wireVoucher := runtimeRaidRewardWireVoucherCounts(wireMember.Rewards)
		voucherValid := receiptMember.Voucher.TokenCera >= 0 &&
			receiptMember.Voucher.HappyTokenCera >= 0 &&
			receiptMember.Voucher.TokenCera <= dnfcerashop.MaxWalletBalance &&
			receiptMember.Voucher.HappyTokenCera <= dnfcerashop.MaxWalletBalance &&
			receiptMember.Voucher == wireVoucher &&
			(receiptMember.Voucher.IsZero() || strings.TrimSpace(receiptMember.AccountID) != "")
		if !found || selection.Mode != 0 || wireMember.MemberKey != member.CharID || wireMember.CardIndex != selection.Index ||
			receiptMember.CharacterID != member.CharID || !voucherValid ||
			// 有资格的成员必须真的拿到东西：要么写进了背包（有格位），要么走了邮件
			// （有溢出行），代币券则直接入账号钱包。超时空只有达到 [MINIMAL DUNGEON CLEAR COUNT] 的成员才有
			// 格子；安东/卢克没有这个门槛，runtimeFiendWarRewardEligible 对它们恒真，
			// 所以这条判定的旧语义一字未改。
			//
			// 「溢出也算拿到」是 2026-10-02 补的：改动前只认格位，于是"背包满 → 整份
			// 奖励走邮件"的那位会被判成回执坏了 —— 这是同一条 bug 的另一半，光放开
			// 发奖循环还不够。
			(runtimeFiendWarRewardEligible(*state, member.CharID) &&
				len(receiptMember.Slots) == 0 && len(receiptMember.Overflow) == 0 && receiptMember.Voucher.IsZero()) ||
			len(receiptMember.PostState) != len(receiptMember.Slots) {
			return errRuntimeRaidRewardReceipt
		}
		// 反方向同样要钉：没资格的成员拿到任何东西（格位、邮件或券）就是收据坏了。
		if !runtimeFiendWarRewardEligible(*state, member.CharID) &&
			(len(wireMember.Rewards) != 0 || len(receiptMember.Slots) != 0 ||
				len(receiptMember.Overflow) != 0 || !receiptMember.Voucher.IsZero()) {
			return errRuntimeRaidRewardReceipt
		}
		for slotIndex, slot := range receiptMember.Slots {
			postState := receiptMember.PostState[slotIndex]
			if slot < 0 || (slotIndex > 0 && receiptMember.Slots[slotIndex-1] >= slot) ||
				postState.Slot != slot || postState.Stack.ItemID <= 0 || postState.Stack.Count <= 0 {
				return errRuntimeRaidRewardReceipt
			}
		}
	}
	return nil
}

func runtimeRaidRewardWireVoucherCounts(rewards []raid.RaidSettlementReward) dnfcerashop.VoucherCounts {
	counts := dnfcerashop.VoucherCounts{}
	for _, reward := range rewards {
		switch dnfcerashop.VoucherKindOf(int64(reward.ItemIDOrValue)) {
		case dnfcerashop.VoucherTokenCera:
			counts.TokenCera += int64(reward.CountOrAux)
		case dnfcerashop.VoucherHappyTokenCera:
			counts.HappyTokenCera += int64(reward.CountOrAux)
		}
	}
	return counts
}

func validateRuntimeRaidRewardReceiptInventories(
	ctx context.Context,
	inventories dnfrepo.InventoryRepository,
	receipt runtimeRaidRewardReceipt,
) error {
	if inventories == nil {
		return errRuntimeRaidRewardRepository
	}
	for _, member := range receipt.Members {
		characterID := strconv.Itoa(int(member.CharacterID))
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: character=%s inventory missing", errRuntimeRaidRewardReceipt, characterID)
		}
		for _, postState := range member.PostState {
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, postState.Slot)
			stack, occupied := inventory.Slots[key]
			if !occupied || !runtimeRaidRewardItemStacksEqual(stack, postState.Stack) {
				return fmt.Errorf("%w: character=%s slot=%d post-state changed", errRuntimeRaidRewardReceipt, characterID, postState.Slot)
			}
		}
	}
	return nil
}

func runtimeRaidRewardReceiptPostState(
	inventory dnfrepo.InventoryRecord,
	slots []int16,
) ([]runtimeRaidRewardReceiptSlotState, error) {
	postState := make([]runtimeRaidRewardReceiptSlotState, 0, len(slots))
	for _, slot := range slots {
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		stack, found := inventory.Slots[key]
		if !found || stack.ItemID <= 0 || stack.Count <= 0 {
			return nil, fmt.Errorf("slot=%d committed stack missing", slot)
		}
		postState = append(postState, runtimeRaidRewardReceiptSlotState{
			Slot:  slot,
			Stack: cloneRuntimeRaidRewardItemStack(stack),
		})
	}
	return postState, nil
}

func cloneRuntimeRaidRewardItemStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = maps.Clone(stack.Extra)
	return stack
}

func runtimeRaidRewardItemStacksEqual(left dnfrepo.ItemStack, right dnfrepo.ItemStack) bool {
	return left.ItemID == right.ItemID &&
		left.Count == right.Count &&
		left.Bind == right.Bind &&
		left.ExpireAt.Equal(right.ExpireAt) &&
		bytes.Equal(left.RawEntry, right.RawEntry) &&
		maps.Equal(left.Extra, right.Extra)
}

func (s *Service) runtimeRaidRewardDeliveryCacheFromReceipt(
	state *runtimeRaidState,
	receiptScope string,
	receipt runtimeRaidRewardReceipt,
	walletRefreshes []runtimeRaidRewardWalletRefresh,
) (*runtimeRaidRewardDeliveryCache, error) {
	if err := s.validateRuntimeRaidRewardReceipt(state, receipt.Phase, receipt.Token, receipt); err != nil {
		return nil, err
	}
	refreshes := make([]runtimeRaidRewardInventoryRefresh, 0, len(receipt.Members))
	for _, member := range receipt.Members {
		refreshes = append(refreshes, runtimeRaidRewardInventoryRefresh{
			CharacterID: member.CharacterID,
			Slots:       append([]int16(nil), member.Slots...),
		})
	}
	return &runtimeRaidRewardDeliveryCache{
		Phase:              receipt.Phase,
		Token:              receipt.Token,
		ReceiptScope:       receiptScope,
		Rewards:            cloneRuntimeRaidSettlementRewards(receipt.Rewards),
		RewardBody:         append([]byte(nil), receipt.RewardBody...),
		PersonalRewardBody: append([]byte(nil), receipt.PersonalRewardBody...),
		TeamRewardBody:     append([]byte(nil), receipt.TeamRewardBody...),
		InventoryRefreshes: refreshes,
		WalletRefreshes:    append([]runtimeRaidRewardWalletRefresh(nil), walletRefreshes...),
	}, nil
}

func runtimeRaidRewardDeliveryFromCache(
	state runtimeRaidState,
	cache *runtimeRaidRewardDeliveryCache,
) runtimeRaidSettlementRewardDelivery {
	cloned := cloneRuntimeRaidRewardDeliveryCache(cache)
	if cloned == nil {
		return runtimeRaidSettlementRewardDelivery{Raid: cloneRuntimeRaidState(state)}
	}
	return runtimeRaidSettlementRewardDelivery{
		Raid:               cloneRuntimeRaidState(state),
		Token:              cloned.Token,
		Rewards:            cloned.Rewards,
		RewardBody:         cloned.RewardBody,
		PersonalRewardBody: cloned.PersonalRewardBody,
		TeamRewardBody:     cloned.TeamRewardBody,
		InventoryRefreshes: cloned.InventoryRefreshes,
		WalletRefreshes:    cloned.WalletRefreshes,
	}
}

func cloneRuntimeRaidRewardDeliveryCache(source *runtimeRaidRewardDeliveryCache) *runtimeRaidRewardDeliveryCache {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Rewards = cloneRuntimeRaidSettlementRewards(source.Rewards)
	clone.RewardBody = append([]byte(nil), source.RewardBody...)
	clone.PersonalRewardBody = append([]byte(nil), source.PersonalRewardBody...)
	clone.TeamRewardBody = append([]byte(nil), source.TeamRewardBody...)
	clone.InventoryRefreshes = cloneRuntimeRaidRewardInventoryRefreshes(source.InventoryRefreshes)
	clone.WalletRefreshes = append([]runtimeRaidRewardWalletRefresh(nil), source.WalletRefreshes...)
	return &clone
}

func cloneRuntimeRaidSettlementRewards(source raid.RaidSettlementRewards) raid.RaidSettlementRewards {
	clone := raid.RaidSettlementRewards{ResultType: source.ResultType, Members: make([]raid.RaidSettlementMemberRewards, len(source.Members))}
	for index, member := range source.Members {
		clone.Members[index] = member
		clone.Members[index].Rewards = append([]raid.RaidSettlementReward(nil), member.Rewards...)
	}
	return clone
}

func cloneRuntimeRaidRewardInventoryRefreshes(source []runtimeRaidRewardInventoryRefresh) []runtimeRaidRewardInventoryRefresh {
	clone := make([]runtimeRaidRewardInventoryRefresh, len(source))
	for index, refresh := range source {
		clone[index] = refresh
		clone[index].Slots = append([]int16(nil), refresh.Slots...)
	}
	return clone
}

func cloneRuntimeRaidRewardCompletion(source runtimeRaidSettlementRewardCompletion) runtimeRaidSettlementRewardCompletion {
	clone := runtimeRaidSettlementRewardCompletion{Raid: cloneRuntimeRaidState(source.Raid)}
	if source.StandbyTimer != nil {
		timer := *source.StandbyTimer
		clone.StandbyTimer = &timer
	}
	return clone
}

func sortedRuntimeRaidRewardSlots(source map[int16]struct{}) []int16 {
	slots := make([]int16, 0, len(source))
	for slot := range source {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	return slots
}
