package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentExpertJobCompoundRequestSize   = 8
	currentExpertJobExtractionRequestSize = 6
)

// 【op416 副职业提取器批量提取 2026-09-25】
//
// 背景：客户端（模式 17/18/19：附魔师/炼金师/控偶师 三个提取器）本来就是
// "一趟把选中装备全部发出去"，每件一个 op416。服务端过去对每个包各开一个事务、
// 各回一个 ACK、各做一次整容器重建，于是玩家看到的是"一件一件消失"。
// 这与 op26（NPC 分解机）/ op235（个人商店分解机）当年一模一样，两者都已用
// "客户端把 N 件合成一个带标记的信封 + 服务端单事务批量"解决。
// 本次按同一口径给 op416 补上。
//
// 信封（客户端 90CN 钩子合成）：
//
//	u8  extractorType | i16 extractorSlot | u8 targetList
//	| i16 0xFFFF 哨兵 | u32 "B41\x01" | u8 count | i16 slots[count]
//	长度 = 11 + 2*count
//
// 与 6 字节单件请求天然区分：单件体第 4..5 字节是 targetSlot 且必须 >= 0，
// 信封在该位置固定写 -1 哨兵。因此老客户端（只发单件）行为完全不变。
const (
	currentExpertJobExtractionBatchHeaderSize = 11
	currentExpertJobExtractionBatchMaxItems   = 127
	// 合并回包的行数上限。客户端结果窗按行循环没有上限比较（已实测），
	// 这里只做服务端侧的保护，取值与 op235 批量的 200 行对齐。
	currentExpertJobExtractionBatchMaxRows = 200
)

// currentExpertJobExtractionBatchSentinel 占住单件请求的 targetSlot 位置。
var currentExpertJobExtractionBatchSentinel int16 = -1

// currentExpertJobExtractionBatchMarker 证明信封按预期顺序写出（同 op26/op235）。
var currentExpertJobExtractionBatchMarker = [4]byte{'B', '4', '1', 0x01}

type currentExpertJobCompoundRequest struct {
	RecipeItemID int64
	Count        int
	CardSlot     int16
}

type currentExpertJobExtractionRequest struct {
	ExtractorType byte
	ExtractorSlot int16
	TargetList    byte
	TargetSlot    int16
}

// currentExpertJobExtractionBatchRequest 是一次批量提取：同一把提取器、
// 同一个目标容器、N 个目标槽。三个职业（附魔师/炼金师/控偶师）共用同一个
// opcode 与同一条客户端循环，差别只有 ExtractorType 一个字节。
type currentExpertJobExtractionBatchRequest struct {
	ExtractorType byte
	ExtractorSlot int16
	TargetList    byte
	Slots         []int16
}

type currentExpertJobCompoundResult struct {
	Plan    dnfexpertjob.CompoundPlan
	JobType byte
}

type currentExpertJobExtractionMaterial struct {
	Slot   int16
	ItemID uint32
	Count  uint32
}

type currentExpertJobExtractionResult struct {
	JobType    byte
	TargetList byte
	TargetSlot int16
	Materials  []currentExpertJobExtractionMaterial
	// ExperienceGain is what the op205 profession row has to be republished
	// for. LevelChanged alone is not enough: most extractions move the bar
	// without crossing a threshold, and the client has no other source for it.
	ExperienceGain int64
	LevelChanged   bool
}

func expertJobGameplayModule() gameplayModuleDefinition {
	compound := uint16(dnfenum.CmdPacketCompoundItemByExpertJob)
	extraction := uint16(dnfenum.CmdPacketExpertExtraction)
	giveUp := uint16(dnfenum.CmdPacketGiveupExpertJob)
	handleCompound := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentExpertJobCompound(session, request.Body)
	}
	handleExtraction := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentExpertJobExtraction(session, request.Body)
	}
	module := gameplayModuleDefinition{
		Name: "expert-job",
		LegacyHandlers: map[uint16]gameplayHandler{
			compound:   handleCompound,
			extraction: handleExtraction,
			giveUp: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentExpertJobGiveUp(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			compound: defaultClassGameplayHandler(
				"game-expert-job-compound-blocked",
				"current_exe_op238_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentExpertJobCompound(session, body)
				},
			),
			extraction: defaultClassGameplayHandler(
				"game-expert-job-extraction-blocked",
				"current_exe_op416_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentExpertJobExtraction(session, body)
				},
			),
			giveUp: defaultClassGameplayHandler(
				"game-expert-job-give-up-blocked",
				"current_exe_op239_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentExpertJobGiveUp(session, body)
				},
			),
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			compound: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentExpertJobCompoundRequestSize)
			},
			extraction: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentExpertJobExtractionRequestSize)
			},
			giveUp: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, 0)
			},
		},
	}
	registerStore := func(opcode uint16, handler func(*Service, *gameSession, uint16, []byte) error) {
		captured := opcode
		module.LegacyHandlers[captured] = func(service *Service, session *gameSession, request gameplayRequest) error {
			return handler(service, session, captured, request.Body)
		}
		module.UpperHandlers[captured] = defaultClassGameplayHandler(
			"game-expert-job-store-blocked",
			"current_exe_expert_job_store_command_class_mismatch",
			func(service *Service, session *gameSession, body []byte) error {
				return handler(service, session, captured, body)
			},
		)
	}
	registerStore(uint16(dnfenum.CmdPacketCreateExpertJobStore), (*Service).handleCurrentExpertJobStoreCreate)
	registerStore(uint16(dnfenum.CmdPacketEnterExpertJobStore), (*Service).handleCurrentExpertJobStoreEnter)
	registerStore(uint16(dnfenum.CmdPacketCloseExpertJobStore), (*Service).handleCurrentExpertJobStoreClose)
	registerStore(uint16(dnfenum.CmdPacketRepairDisjointMachine), (*Service).handleCurrentExpertJobStoreRepair)
	registerStore(uint16(dnfenum.CmdPacketRepairExpertJobStore), (*Service).handleCurrentExpertJobStoreRepair)
	registerStore(uint16(dnfenum.CmdPacketUpgradeDisjointMachine), (*Service).handleCurrentExpertJobStoreUpgrade)
	registerStore(uint16(dnfenum.CmdPacketRequestDisjointItem), (*Service).handleCurrentExpertJobStoreDisjoint)
	registerStore(uint16(dnfenum.CmdPacketUseEnchantStore), (*Service).handleCurrentExpertJobStoreEnchant)
	return module
}

func parseCurrentExpertJobCompoundRequest(body []byte) (currentExpertJobCompoundRequest, error) {
	if len(body) != currentExpertJobCompoundRequestSize {
		return currentExpertJobCompoundRequest{}, fmt.Errorf("expert job compound body=%d", len(body))
	}
	request := currentExpertJobCompoundRequest{
		RecipeItemID: int64(int32(binary.LittleEndian.Uint32(body[0:4]))),
		Count:        int(binary.LittleEndian.Uint16(body[4:6])),
		CardSlot:     int16(binary.LittleEndian.Uint16(body[6:8])),
	}
	// 【2026-09-18 放开"制作宝珠数量必须=1"的限制】
	//
	// 这里原先还有一条 `CardSlot >= 0 && Count != 1` 的判定：附魔机界面里可以把
	// 数量调到 50 / 49 再按制作，客户端会照原样发来，于是被这条校验直接判非法、
	// 回失败码 19（玩家实测「制作宝珠被服务端拒了」，日志里是
	// game-expert-job-compound-rejected reason=expert job compound request is invalid）。
	// 普通合成那条路径本来就把 Count 当批量数用（PlanCompoundWithLearned(...,
	// request.Count, ...)），所以批量是客户端本来就支持的用法，这里不再拦。
	// 数量只做下界检查；实际能做几次由卡片与配方材料的存量决定（见 commit 里的循环）。
	if request.RecipeItemID <= 0 || request.Count <= 0 || request.CardSlot < -1 {
		return currentExpertJobCompoundRequest{}, errors.New("expert job compound request is invalid")
	}
	return request, nil
}

func parseCurrentExpertJobExtractionRequest(body []byte) (currentExpertJobExtractionRequest, error) {
	if len(body) != currentExpertJobExtractionRequestSize {
		return currentExpertJobExtractionRequest{}, fmt.Errorf("expert job extraction body=%d", len(body))
	}
	request := currentExpertJobExtractionRequest{
		ExtractorType: body[0],
		ExtractorSlot: int16(binary.LittleEndian.Uint16(body[1:3])),
		TargetList:    body[3],
		TargetSlot:    int16(binary.LittleEndian.Uint16(body[4:6])),
	}
	if (request.ExtractorType != dnfexpertjob.EnchanterType && request.ExtractorType != dnfexpertjob.AlchemistType && request.ExtractorType != dnfexpertjob.DollControllerType) ||
		request.ExtractorSlot < 0 || request.TargetList != dnfrepo.MainInventoryListType ||
		request.TargetSlot < 0 || request.ExtractorSlot == request.TargetSlot {
		return currentExpertJobExtractionRequest{}, errors.New("expert job extraction request is invalid")
	}
	return request, nil
}

// currentExpertJobExtractionTypeSupported 报告这个字节是不是三个"提取器职业"之一
// （1 附魔师 / 2 炼金师 / 4 控偶师）。三个职业共用 op416 与同一条客户端循环。
func currentExpertJobExtractionTypeSupported(extractorType byte) bool {
	return extractorType == dnfexpertjob.EnchanterType ||
		extractorType == dnfexpertjob.AlchemistType ||
		extractorType == dnfexpertjob.DollControllerType
}

// parseCurrentExpertJobExtractionBatchRequest 识别客户端 90CN 钩子合成的批量提取命令。
//
// 第二个返回值表示"这是一个批量信封"（即使内容非法），让调用方能区分
// "不是批量"（照原样走 6 字节单件路径）与"批量但坏了"（回失败码）——与 op26 的
// parseCurrentDisjointBatchRequest、op235 的 parseCurrentExpertJobStoreDisjointBatchRequest
// 同一约定。
//
// 关键不变式：6 字节单件请求永远进不来这里（长度不足且哨兵位置是合法 targetSlot），
// 所以老客户端与单件提取的行为逐字节不变。
func parseCurrentExpertJobExtractionBatchRequest(body []byte) (currentExpertJobExtractionBatchRequest, bool, error) {
	if len(body) < currentExpertJobExtractionBatchHeaderSize ||
		int16(binary.LittleEndian.Uint16(body[4:6])) != currentExpertJobExtractionBatchSentinel ||
		body[6] != currentExpertJobExtractionBatchMarker[0] ||
		body[7] != currentExpertJobExtractionBatchMarker[1] ||
		body[8] != currentExpertJobExtractionBatchMarker[2] ||
		body[9] != currentExpertJobExtractionBatchMarker[3] {
		return currentExpertJobExtractionBatchRequest{}, false, nil
	}
	count := int(body[10])
	if count < 1 || count > currentExpertJobExtractionBatchMaxItems ||
		len(body) != currentExpertJobExtractionBatchHeaderSize+count*2 {
		return currentExpertJobExtractionBatchRequest{}, true,
			fmt.Errorf("expert job extraction batch count=%d body=%d", count, len(body))
	}
	request := currentExpertJobExtractionBatchRequest{
		ExtractorType: body[0],
		ExtractorSlot: int16(binary.LittleEndian.Uint16(body[1:3])),
		TargetList:    body[3],
		Slots:         make([]int16, 0, count),
	}
	if !currentExpertJobExtractionTypeSupported(request.ExtractorType) ||
		request.ExtractorSlot < 0 || request.TargetList != dnfrepo.MainInventoryListType {
		return currentExpertJobExtractionBatchRequest{}, true,
			fmt.Errorf("expert job extraction batch type=%d extractor=%d list=%d",
				request.ExtractorType, request.ExtractorSlot, request.TargetList)
	}
	seen := make(map[int16]struct{}, count)
	for index := 0; index < count; index++ {
		offset := currentExpertJobExtractionBatchHeaderSize + index*2
		slot := int16(binary.LittleEndian.Uint16(body[offset : offset+2]))
		if slot < 0 || slot == request.ExtractorSlot ||
			dnfrepo.IsAccountSharedInventorySlot(request.TargetList, slot) {
			return currentExpertJobExtractionBatchRequest{}, true,
				fmt.Errorf("expert job extraction batch index=%d slot=%d", index, slot)
		}
		if _, duplicate := seen[slot]; duplicate {
			return currentExpertJobExtractionBatchRequest{}, true,
				fmt.Errorf("expert job extraction batch duplicate slot=%d", slot)
		}
		seen[slot] = struct{}{}
		request.Slots = append(request.Slots, slot)
	}
	return request, true, nil
}

// currentExpertJobProjectionNeedsRepublish reports whether one expert-job
// operation moved state that only the class-0 op205 projection carries.
//
// The compound and extraction ACKs describe the crafted items, never the
// profession's experience bar; op205 is its only carrier, and the client-side
// hook writes it straight into the current actor. Gating the republish on a
// level change therefore left every ordinary craft's experience invisible
// until the next character select replayed the login push - the visible
// symptom for 附魔师. Both expert-job store paths have always republished
// unconditionally, so only these two lagged behind their own writes.
func currentExpertJobProjectionNeedsRepublish(experienceGain int64, levelChanged bool) bool {
	return experienceGain != 0 || levelChanged
}

func (s *Service) handleCurrentExpertJobCompound(session *gameSession, body []byte) error {
	request, err := parseCurrentExpertJobCompoundRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-expert-job-compound-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCompoundItemByExpertJob), 19)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentExpertJobCompound(ctx, session, request)
	if err != nil {
		code := currentExpertJobCompoundError(err)
		s.logGameEvent(session, "game-expert-job-compound-rejected", "recipe", request.RecipeItemID, "count", request.Count, "error", err, "error_code", code)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCompoundItemByExpertJob), code)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCompoundItemByExpertJob), buildCurrentExpertJobCompoundSuccessBody(result.Plan), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendSelectedCurrentContainerListsRefresh(session, "expert_job_compound_after_ack"); err != nil {
		return err
	}
	if currentExpertJobProjectionNeedsRepublish(result.Plan.ExperienceGain, result.Plan.LevelChanged) {
		if err := s.sendCurrentExpertJobInfoFromRepository(session, result.JobType, true); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-expert-job-compound-committed", "job_type", result.JobType, "recipe", request.RecipeItemID, "attempts", request.Count, "successes", result.Plan.SuccessCount, "failures", result.Plan.FailureCount, "experience_gain", result.Plan.ExperienceGain)
	return nil
}

func (s *Service) handleCurrentExpertJobExtraction(session *gameSession, body []byte) error {
	// 批量信封优先。第二个返回值为 false 表示"这不是信封"，原样落到下面的
	// 6 字节单件路径——单件提取的行为与本函数此前完全一致（只多了一次常量级判断）。
	batch, isBatch, batchErr := parseCurrentExpertJobExtractionBatchRequest(body)
	if batchErr != nil {
		s.logGameEvent(session, "game-expert-job-extraction-batch-rejected", "body_len", len(body), "reason", batchErr)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExpertExtraction), 19)
	}
	if isBatch {
		return s.handleCurrentExpertJobExtractionBatch(session, batch)
	}
	request, err := parseCurrentExpertJobExtractionRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-expert-job-extraction-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExpertExtraction), 19)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentExpertJobExtraction(ctx, session, request)
	if err != nil {
		code := currentExpertJobExtractionError(err)
		s.logGameEvent(session, "game-expert-job-extraction-rejected", "extractor_type", request.ExtractorType, "extractor_slot", request.ExtractorSlot, "target_slot", request.TargetSlot, "error", err, "error_code", code)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExpertExtraction), code)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketExpertExtraction), buildCurrentExpertJobExtractionSuccessBody(result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendSelectedCurrentContainerListsRefresh(session, "expert_job_extraction_after_ack"); err != nil {
		return err
	}
	if currentExpertJobProjectionNeedsRepublish(result.ExperienceGain, result.LevelChanged) {
		if err := s.sendCurrentExpertJobInfoFromRepository(session, result.JobType, true); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-expert-job-extraction-committed", "job_type", result.JobType, "extractor_slot", request.ExtractorSlot, "target_slot", request.TargetSlot, "materials", len(result.Materials), "experience_gain", result.ExperienceGain)
	return nil
}

func (s *Service) commitCurrentExpertJobCompound(ctx context.Context, session *gameSession, request currentExpertJobCompoundRequest) (currentExpertJobCompoundResult, error) {
	jobCatalog, items, owner, accountID, characterID, err := s.currentExpertJobMutationContext(session)
	if err != nil {
		return currentExpertJobCompoundResult{}, err
	}
	var result currentExpertJobCompoundResult
	err = owner.Compound(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: characterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
		jobType, experience, err := currentExpertJobCharacterState(assets.Character)
		if err != nil {
			return dnfexpertjob.Changes{}, err
		}
		if request.CardSlot >= 0 {
			if jobType != dnfexpertjob.EnchanterType {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
			}
			// 【批量制作宝珠 2026-09-18】
			//
			// 附魔机的"制作宝珠"和普通合成一样带数量：请求体是
			// {u32 配方ID, u16 数量, u16 卡片槽}，客户端把界面里选的数量原样发来。
			// 原先这条分支只处理 1 个（parse 直接把数量!=1 判非法、回失败码 19），
			// 玩家把数量调成 50 / 49 就必然被拒；现在按数量循环：
			//
			//   每次消耗 1 张卡 + 一份配方材料 → 按"当前经验"掷一次成功率
			//   → 成功才产出宝珠、并把这次的经验累加（下一件的成功率/等级按新经验算）
			//
			// 卡用完或配方材料不足时停止循环，并按已完成的次数结算；一次都没能完成
			// （第一张卡就不合法 / 第一份材料就不够）则整体失败、事务不落库，保持和
			// 原来单次制作一样的原子性。
			//
			// 回包沿用现有格式：buildCurrentExpertJobCompoundSuccessBody 本身就带
			// "产物条数 + 每条 (itemID,count) + 成功数 + 失败数"，单产物时正好 19 字节
			// （与线上成功回包一致）。产物条目按成功次数聚合；0 次成功时不带产物条目，
			// 与单次制作失败时的表现保持一致。
			cardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.CardSlot)
			compoundPlan := dnfexpertjob.CompoundPlan{}
			finalExperience := experience
			levelChanged := false
			inventoryChanged := false
			accountChanged := false
			var beadItemID int64
			attempted := 0
			for attempt := 0; attempt < request.Count; attempt++ {
				card, found := assets.Inventory.Slots[cardKey]
				if !found || card.ItemID <= 0 || card.Count <= 0 || currentNPCShopItemLocked(card) {
					if attempt == 0 {
						return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
					}
					break
				}
				// 每件都按当前经验重新掷：等级/成功率会随累计经验变化。
				beadPlan, planErr := jobCatalog.PlanEnchanterBead(finalExperience, request.RecipeItemID, card.ItemID, rand.IntN)
				if planErr != nil {
					if attempt == 0 {
						return dnfexpertjob.Changes{}, planErr
					}
					break
				}
				upgradeCount := currentExpertJobEnchantUpgradeCount(card)
				card.Count--
				if card.Count == 0 {
					delete(assets.Inventory.Slots, cardKey)
				} else {
					entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.CardSlot, card)
					card.RawEntry = append([]byte(nil), entry.data[:]...)
					assets.Inventory.Slots[cardKey] = card
				}
				consumedAccount, consumeErr := consumeCurrentExpertJobRecipe(assets.Character, assets.Inventory, assets.AccountInventory, beadPlan.Recipe.Materials, 0)
				if consumeErr != nil {
					// 这一张卡已经扣了，材料不够就停在这里：与单次制作"失败也照扣"的
					// 口径一致。首件就失败则整体不落库。
					if attempt == 0 {
						return dnfexpertjob.Changes{}, consumeErr
					}
					break
				}
				accountChanged = accountChanged || consumedAccount
				inventoryChanged = true
				attempted++
				finalExperience = beadPlan.FinalExperience
				levelChanged = levelChanged || beadPlan.LevelChanged
				compoundPlan.ExperienceGain += beadPlan.ExperienceGain
				if !beadPlan.Success {
					compoundPlan.FailureCount++
					continue
				}
				if beadPlan.BeadItemID <= 0 || beadPlan.BeadItemID > math.MaxUint32 {
					return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
				}
				definition, resolveErr := items.ResolveItem(uint32(beadPlan.BeadItemID))
				if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
					return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
				}
				if _, grantErr := grantCurrentExpertJobBead(assets.Inventory, definition, upgradeCount); grantErr != nil {
					return dnfexpertjob.Changes{}, grantErr
				}
				beadItemID = beadPlan.BeadItemID
				compoundPlan.SuccessCount++
			}
			if attempted == 0 {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
			}
			compoundPlan.FinalExperience = finalExperience
			if compoundPlan.SuccessCount > 0 {
				compoundPlan.AttemptedOutputs = []dnfexpertjob.RecipeEntry{{
					ItemID: beadItemID,
					Count:  int64(compoundPlan.SuccessCount),
				}}
				compoundPlan.Rewards = append([]dnfexpertjob.RecipeEntry(nil), compoundPlan.AttemptedOutputs...)
			}
			if assets.Character.Stats == nil {
				assets.Character.Stats = make(map[string]int64)
			}
			assets.Character.Stats["expert_job_exp"] = finalExperience
			result = currentExpertJobCompoundResult{Plan: compoundPlan, JobType: jobType}
			return dnfexpertjob.Changes{
				Character:        compoundPlan.ExperienceGain != 0 || levelChanged,
				AccountInventory: accountChanged,
				Inventory:        inventoryChanged,
			}, nil
		}
		learned := assets.Character.Stats[currentExpertJobRecipeStatKey(jobType, request.RecipeItemID)] != 0
		plan, err := jobCatalog.PlanCompoundWithLearned(jobType, experience, request.RecipeItemID, request.Count, learned, rand.IntN)
		if err != nil {
			return dnfexpertjob.Changes{}, err
		}
		accountChanged, err := consumeCurrentExpertJobRecipe(assets.Character, assets.Inventory, assets.AccountInventory, plan.Materials, plan.GoldCost)
		if err != nil {
			return dnfexpertjob.Changes{}, err
		}
		for _, reward := range plan.Rewards {
			if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 || reward.Count > math.MaxUint32 {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
			}
			definition, resolveErr := items.ResolveItem(uint32(reward.ItemID))
			if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrRecipeUnavailable
			}
			// 副职业配方产出同样要过账号共享落点：无色/灵魂类产物必须先落进
			// 354..365，装不下的余量再走角色背包。这条路径过去直接调
			// grantCurrentCeraShopProduct，产物会留在背包（2026-09-21 与 NPC 商店
			// 金币购买同类的漏点）。判定与溢出都收在下面那个可测的小函数里。
			rewardAccountChanged, rewardErr := grantCurrentExpertJobCompoundReward(
				assets.Inventory,
				assets.AccountInventory,
				definition,
				uint32(reward.Count),
				s.currentInventoryPlacementMainSlotCount(ctx, assets.Inventory, characterID),
			)
			if rewardErr != nil {
				return dnfexpertjob.Changes{}, rewardErr
			}
			accountChanged = accountChanged || rewardAccountChanged
		}
		if assets.Character.Stats == nil {
			assets.Character.Stats = make(map[string]int64)
		}
		assets.Character.Stats["expert_job_exp"] = plan.FinalExperience
		result = currentExpertJobCompoundResult{Plan: plan, JobType: jobType}
		return dnfexpertjob.Changes{Character: plan.ExperienceGain != 0, AccountInventory: accountChanged, Inventory: true}, nil
	}})
	return result, err
}

func (s *Service) commitCurrentExpertJobExtraction(ctx context.Context, session *gameSession, request currentExpertJobExtractionRequest) (currentExpertJobExtractionResult, error) {
	jobCatalog, items, owner, accountID, characterID, err := s.currentExpertJobMutationContext(session)
	if err != nil {
		return currentExpertJobExtractionResult{}, err
	}
	var result currentExpertJobExtractionResult
	err = owner.Extract(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: characterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
		jobType, experience, err := currentExpertJobCharacterState(assets.Character)
		if err != nil || jobType != request.ExtractorType {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
		}
		extractor, extractorFound := assets.Inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.ExtractorSlot)]
		targetKey := currentCeraShopInventorySlotKey(request.TargetList, request.TargetSlot)
		target, targetFound := assets.Inventory.Slots[targetKey]
		if !extractorFound || extractor.ItemID <= 0 || currentNPCShopItemLocked(extractor) || !targetFound || target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count != 1 || currentNPCShopItemLocked(target) {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
		}
		metadata, err := jobCatalog.Equipment(target.ItemID)
		if err != nil {
			return dnfexpertjob.Changes{}, err
		}
		if metadata.DisjointForbidden || metadata.AttachType == "trade delete" {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
		}
		metadata.State = currentExpertJobEquipmentState(target)
		plan, err := jobCatalog.PlanExtraction(jobType, experience, extractor.ItemID, metadata, rand.IntN)
		if err != nil {
			return dnfexpertjob.Changes{}, err
		}
		delete(assets.Inventory.Slots, targetKey)
		// 提取产物（晶块/灵魂占多数）必须先过账号共享落点。accountChanged 决定
		// 账号记录要不要跟着这次操作一起提交。
		accountChanged := false
		for _, reward := range plan.Materials {
			if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 || reward.Count > math.MaxUint32 {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
			}
			definition, resolveErr := items.ResolveItem(uint32(reward.ItemID))
			if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
			}
			// 统一入口：有固定格就先进账号共享仓，余量再走角色背包。
			rows, rewardAccountChanged, grantErr := grantCurrentExpertJobExtractionReward(
				assets.Inventory,
				assets.AccountInventory,
				definition,
				uint32(reward.Count),
				s.currentInventoryPlacementMainSlotCount(ctx, assets.Inventory, characterID),
			)
			if grantErr != nil {
				return dnfexpertjob.Changes{}, grantErr
			}
			accountChanged = accountChanged || rewardAccountChanged
			result.Materials = append(result.Materials, rows...)
		}
		if assets.Character.Stats == nil {
			assets.Character.Stats = make(map[string]int64)
		}
		assets.Character.Stats["expert_job_exp"] = plan.FinalExperience
		result.JobType = jobType
		result.TargetList = request.TargetList
		result.TargetSlot = request.TargetSlot
		result.ExperienceGain = plan.ExperienceGain
		result.LevelChanged = plan.LevelChanged
		sort.Slice(result.Materials, func(i, j int) bool { return result.Materials[i].Slot < result.Materials[j].Slot })
		return dnfexpertjob.Changes{Character: plan.ExperienceGain != 0, AccountInventory: accountChanged, Inventory: true}, nil
	}})
	return result, err
}

// handleCurrentExpertJobExtractionBatch 结算一次批量提取：一个 ACK、一次容器刷新、
// 一次 op205，取代过去"每件一套事务/回包/重建"的做法。
// 只有客户端 90CN 钩子合成的信封才会走到这里。
func (s *Service) handleCurrentExpertJobExtractionBatch(session *gameSession, request currentExpertJobExtractionBatchRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentExpertJobExtractionBatch(ctx, session, request)
	if err != nil {
		code := currentExpertJobExtractionError(err)
		s.logGameEvent(session, "game-expert-job-extraction-batch-rejected", "extractor_type", request.ExtractorType, "extractor_slot", request.ExtractorSlot, "slot_count", len(request.Slots), "error", err, "error_code", code)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExpertExtraction), code)
	}
	// 合并回包。targetSlot 只能报一个（客户端读取器整个函数只清这一个源槽，已实测），
	// 报第一个被消耗的槽；其余源槽由紧随其后的整容器重建权威刷新——与 op26/op235
	// 批量的分工完全一致，那两条已在线上验证过。
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketExpertExtraction), buildCurrentExpertJobExtractionSuccessBody(result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	// 整批只重建一次容器：与单件路径的收尾动作完全相同，只是从 N 次变成 1 次。
	if err := s.sendSelectedCurrentContainerListsRefresh(session, "expert_job_extraction_batch_after_ack"); err != nil {
		return err
	}
	if currentExpertJobProjectionNeedsRepublish(result.ExperienceGain, result.LevelChanged) {
		if err := s.sendCurrentExpertJobInfoFromRepository(session, result.JobType, true); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-expert-job-extraction-batch-committed", "job_type", result.JobType, "extractor_slot", request.ExtractorSlot, "slot_count", len(request.Slots), "materials", len(result.Materials), "experience_gain", result.ExperienceGain)
	return nil
}

// commitCurrentExpertJobExtractionBatch 在一个事务里把 N 件装备各提取一次。
//
// 逐件与单件路径逐条同源：同一把提取器（不消耗、无耐久）、同样的目标校验、
// 同样的 PlanExtraction（逐件各掷各的随机数，所以产物分布与 N 次单件请求完全一致）、
// 经验逐件往下传（第 k+1 件按第 k 件结算后的经验算等级与成功率）。
// 任何一件不合法 → 直接返回错误 → 整个事务回滚（整批原子），与 op26/op235
// 批量的口径一致：绝不允许"分了一半"。
func (s *Service) commitCurrentExpertJobExtractionBatch(ctx context.Context, session *gameSession, request currentExpertJobExtractionBatchRequest) (currentExpertJobExtractionResult, error) {
	jobCatalog, items, owner, accountID, characterID, err := s.currentExpertJobMutationContext(session)
	if err != nil {
		return currentExpertJobExtractionResult{}, err
	}
	var result currentExpertJobExtractionResult
	if len(request.Slots) == 0 {
		return currentExpertJobExtractionResult{}, dnfexpertjob.ErrExtractionInvalid
	}
	err = owner.Extract(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: characterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
		jobType, experience, err := currentExpertJobCharacterState(assets.Character)
		if err != nil || jobType != request.ExtractorType {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
		}
		// N 件共用同一把提取器：校验一次即可。提取器本身不会被消耗
		// （单件路径同样只把它传给 PlanExtraction），所以逐件复查只是重复同一判断。
		extractor, extractorFound := assets.Inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.ExtractorSlot)]
		if !extractorFound || extractor.ItemID <= 0 || currentNPCShopItemLocked(extractor) {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
		}
		finalExperience := experience
		levelChanged := false
		accountChanged := false
		for _, targetSlot := range request.Slots {
			targetKey := currentCeraShopInventorySlotKey(request.TargetList, targetSlot)
			target, targetFound := assets.Inventory.Slots[targetKey]
			if !targetFound || target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count != 1 || currentNPCShopItemLocked(target) {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
			}
			metadata, metadataErr := jobCatalog.Equipment(target.ItemID)
			if metadataErr != nil {
				return dnfexpertjob.Changes{}, metadataErr
			}
			if metadata.DisjointForbidden || metadata.AttachType == "trade delete" {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
			}
			metadata.State = currentExpertJobEquipmentState(target)
			plan, planErr := jobCatalog.PlanExtraction(jobType, finalExperience, extractor.ItemID, metadata, rand.IntN)
			if planErr != nil {
				return dnfexpertjob.Changes{}, planErr
			}
			delete(assets.Inventory.Slots, targetKey)
			for _, reward := range plan.Materials {
				if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 || reward.Count > math.MaxUint32 {
					return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
				}
				definition, resolveErr := items.ResolveItem(uint32(reward.ItemID))
				if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
					return dnfexpertjob.Changes{}, dnfexpertjob.ErrExtractionInvalid
				}
				// 与单件路径同一个入口：有固定格先进账号共享仓，余量再走角色背包。
				rows, rewardAccountChanged, grantErr := grantCurrentExpertJobExtractionReward(
					assets.Inventory,
					assets.AccountInventory,
					definition,
					uint32(reward.Count),
					s.currentInventoryPlacementMainSlotCount(ctx, assets.Inventory, characterID),
				)
				if grantErr != nil {
					return dnfexpertjob.Changes{}, grantErr
				}
				accountChanged = accountChanged || rewardAccountChanged
				result.Materials = append(result.Materials, rows...)
			}
			finalExperience = plan.FinalExperience
			levelChanged = levelChanged || plan.LevelChanged
			result.ExperienceGain += plan.ExperienceGain
		}
		result.Materials = currentExpertJobMergeExtractionBatchRows(result.Materials)
		if len(result.Materials) > currentExpertJobExtractionBatchMaxRows {
			return dnfexpertjob.Changes{}, errCurrentDisjointRewardInvalid
		}
		if assets.Character.Stats == nil {
			assets.Character.Stats = make(map[string]int64)
		}
		assets.Character.Stats["expert_job_exp"] = finalExperience
		result.JobType = jobType
		result.TargetList = request.TargetList
		result.TargetSlot = request.Slots[0]
		result.LevelChanged = levelChanged
		sort.Slice(result.Materials, func(i, j int) bool { return result.Materials[i].Slot < result.Materials[j].Slot })
		return dnfexpertjob.Changes{Character: result.ExperienceGain != 0 || levelChanged, AccountInventory: accountChanged, Inventory: true}, nil
	}})
	return result, err
}

// currentExpertJobMergeExtractionBatchRows 把 N 件产出的材料行按 (槽, 道具) 合并，
// 同一格的多行合成一行并累加数量。
//
// 为什么必须合并：客户端读取器是把回包里的行逐行应用到容器行上的，同一槽出现多行时
// 它的语义取决于底层是"置值"还是"累加"（未取证）。op26 批量的
// mergeCurrentDisjointBatchRewards 也是先聚合再回包，这里取同一口径：既避开这个歧义，
// 又把行数压到"涉及多少个格子"的规模。
func currentExpertJobMergeExtractionBatchRows(rows []currentExpertJobExtractionMaterial) []currentExpertJobExtractionMaterial {
	if len(rows) < 2 {
		return rows
	}
	type rowKey struct {
		slot   int16
		itemID uint32
	}
	merged := make([]currentExpertJobExtractionMaterial, 0, len(rows))
	seen := make(map[rowKey]int, len(rows))
	for _, row := range rows {
		key := rowKey{row.Slot, row.ItemID}
		if at, found := seen[key]; found {
			merged[at].Count += row.Count
			continue
		}
		seen[key] = len(merged)
		merged = append(merged, row)
	}
	return merged
}

func (s *Service) currentExpertJobMutationContext(session *gameSession) (*dnfexpertjob.Catalog, *pvfDungeonDropCatalog, *dnfexpertjob.Owner, string, string, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, nil, nil, "", "", dnfexpertjob.ErrOwnerUnavailable
	}
	jobCatalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, nil, nil, "", "", dnfexpertjob.ErrOwnerUnavailable
	}
	owner, err := dnfexpertjob.NewOwner(repositories)
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if accountID == "" {
		return nil, nil, nil, "", "", dnfexpertjob.ErrOwnerUnavailable
	}
	return jobCatalog, items, owner, accountID, characterID, nil
}

func currentExpertJobCharacterState(character *dnfrepo.CharacterRecord) (byte, int64, error) {
	if character == nil {
		return 0, 0, dnfexpertjob.ErrCharacterNotFound
	}
	rawJobType := character.Stats["expert_job_type"]
	if rawJobType < int64(dnfexpertjob.EnchanterType) || rawJobType > int64(dnfexpertjob.DollControllerType) {
		return 0, 0, dnfexpertjob.ErrJobUnsupported
	}
	jobType := byte(rawJobType)
	experience := character.Stats["expert_job_exp"]
	if experience < 0 {
		experience = 0
	}
	return jobType, experience, nil
}

func consumeCurrentExpertJobRecipe(
	character *dnfrepo.CharacterRecord,
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	materials []dnfexpertjob.RecipeEntry,
	gold int64,
) (bool, error) {
	if character == nil {
		return false, dnfexpertjob.ErrCharacterNotFound
	}
	if inventory == nil {
		return false, dnfexpertjob.ErrInventoryNotFound
	}
	required := make(map[int64]int64, len(materials))
	for _, material := range materials {
		if material.ItemID <= 0 || material.Count <= 0 || required[material.ItemID] > math.MaxInt64-material.Count {
			return false, dnfexpertjob.ErrRecipeUnavailable
		}
		required[material.ItemID] += material.Count
	}
	if gold < 0 {
		return false, dnfexpertjob.ErrRecipeUnavailable
	}
	if gold > 0 {
		walletGold, goldErr := currentExpertJobWalletGold(character)
		if goldErr != nil || walletGold < gold {
			return false, errCurrentExpertJobInsufficientMaterials
		}
	}
	// Cube fragments and souls live in fixed account-shared slots.  The
	// crafting UI counts those balances, so the transaction must validate and
	// consume them from the same owner instead of searching only the backpack.
	accountChanged := false
	for itemID, count := range required {
		fixedSlot, shared := currentExpertJobSharedMaterialSlot(itemID)
		if !shared {
			continue
		}
		if account == nil || account.Slots == nil {
			return false, errCurrentExpertJobInsufficientMaterials
		}
		key := dnfrepo.AccountSharedInventorySlotKey(fixedSlot)
		stack, found := account.Slots[key]
		if !found || stack.ItemID != itemID || stack.Count < count || currentNPCShopItemLocked(stack) {
			return false, errCurrentExpertJobInsufficientMaterials
		}
	}
	type candidate struct {
		key  string
		slot int16
	}
	candidates := make(map[int64][]candidate, len(required))
	available := make(map[int64]int64, len(required))
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || slot <= 0 || stack.Count <= 0 || currentNPCShopItemLocked(stack) {
			continue
		}
		if _, shared := currentExpertJobSharedMaterialSlot(stack.ItemID); shared {
			continue
		}
		if _, needed := required[stack.ItemID]; !needed {
			continue
		}
		available[stack.ItemID] += stack.Count
		candidates[stack.ItemID] = append(candidates[stack.ItemID], candidate{key, slot})
	}
	for itemID, count := range required {
		if _, shared := currentExpertJobSharedMaterialSlot(itemID); shared {
			continue
		}
		if available[itemID] < count {
			return false, errCurrentExpertJobInsufficientMaterials
		}
	}
	for itemID, count := range required {
		if fixedSlot, shared := currentExpertJobSharedMaterialSlot(itemID); shared {
			key := dnfrepo.AccountSharedInventorySlotKey(fixedSlot)
			stack := account.Slots[key]
			stack.Count -= count
			if stack.Count == 0 {
				delete(account.Slots, key)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, fixedSlot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				account.Slots[key] = stack
			}
			accountChanged = true
			continue
		}
		sort.Slice(candidates[itemID], func(i, j int) bool { return candidates[itemID][i].slot < candidates[itemID][j].slot })
		remaining := count
		for _, candidate := range candidates[itemID] {
			stack := inventory.Slots[candidate.key]
			consumed := min(remaining, stack.Count)
			stack.Count -= consumed
			remaining -= consumed
			if stack.Count == 0 {
				delete(inventory.Slots, candidate.key)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, candidate.slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[candidate.key] = stack
			}
			if remaining == 0 {
				break
			}
		}
	}
	if gold > 0 {
		walletGold, _ := currentExpertJobWalletGold(character)
		currentExpertJobSetWalletGold(character, walletGold-gold)
	}
	return accountChanged, nil
}

func currentExpertJobSharedMaterialSlot(itemID int64) (int16, bool) {
	// 走权威表（repository/account_shared_materials.go），不再自己查 bridge 的派生视图。
	return dnfrepo.AccountSharedWarehouseSlotForItem(itemID)
}

// grantCurrentExpertJobCompoundReward 把一个副职业配方产物送到它的落点：
// 有固定格（六个晶块 / 六个灵魂）→ 先进账号共享仓；装不下的余量 → 角色背包。
// 返回账号记录是否被改动（调用方据此提交账号记录）。
//
// 抽成独立函数是为了**可测**：配方产物这一层过去直接调 grantCurrentCeraShopProduct，
// 无色/灵魂类产物会留在背包（2026-09-21 与 NPC 商店金币购买同类的漏点）。
func grantCurrentExpertJobCompoundReward(
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
) (bool, error) {
	shared := grantCurrentAccountSharedFirst(account, definition, count, "expert_job_compound")
	if shared.Remainder > 0 {
		if _, grantErr := grantCurrentCeraShopProduct(inventory, definition, shared.Remainder, mainSlotCount); grantErr != nil {
			return false, grantErr
		}
	}
	return shared.Changed, nil
}

// grantCurrentExpertJobExtractionReward 把一个提取产物送到它的落点，并给出 ACK 行。
//
// 提取产物以晶块/灵魂为主，所以这条路径同样必须先过账号共享落点。ACK 行与已修好的
// 分解机提取（expert_job_store.go）保持同一口径：**每个真正落库的格子报一条，数量报
// 该格的增量**（不是计划总量）—— 账号固定槽与溢出的背包槽各自成行。
func grantCurrentExpertJobExtractionReward(
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
) ([]currentExpertJobExtractionMaterial, bool, error) {
	if inventory == nil || count == 0 {
		return nil, false, dnfexpertjob.ErrExtractionInvalid
	}
	// 角色侧可能把余量拆到多行（受 [stack limit] 限制），所以先记下背包里同 id 各行
	// 的旧数量，落库后按"该槽增量"逐行报 —— 只报 slots[0] 配全部余量会让 ACK 撒谎。
	previousCounts := make(map[int16]int64)
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == dnfrepo.MainInventoryListType && stack.ItemID == int64(definition.ItemID) && stack.Count > 0 {
			previousCounts[slot] = stack.Count
		}
	}
	shared := grantCurrentAccountSharedFirst(account, definition, count, "expert_job_extraction")
	rows := make([]currentExpertJobExtractionMaterial, 0, 2)
	if shared.Changed {
		rows = append(rows, currentExpertJobExtractionMaterial{
			Slot:   shared.Slot,
			ItemID: definition.ItemID,
			Count:  uint32(shared.Placed),
		})
	}
	if shared.Remainder > 0 {
		slots, grantErr := grantCurrentCeraShopProduct(inventory, definition, shared.Remainder, mainSlotCount)
		if grantErr != nil {
			return nil, false, grantErr
		}
		if len(slots) == 0 {
			return nil, false, dnfexpertjob.ErrExtractionInvalid
		}
		for _, rawSlot := range slots {
			slot := int16(rawSlot)
			stack, found := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)]
			if !found || stack.Count <= 0 {
				return nil, false, dnfexpertjob.ErrExtractionInvalid
			}
			added := stack.Count - previousCounts[slot]
			if added <= 0 {
				// 该槽只是被 touch 到、没真正增加数量：不发这一行 ACK。
				continue
			}
			rows = append(rows, currentExpertJobExtractionMaterial{
				Slot:   slot,
				ItemID: definition.ItemID,
				Count:  uint32(added),
			})
		}
	}
	return rows, shared.Changed, nil
}

func currentExpertJobEquipmentState(stack dnfrepo.ItemStack) int {
	if sceneInventoryExtraUint32(stack.Extra, "chronicle_option_count", "chronicle_count") > 0 {
		return 2
	}
	if sceneInventoryExtraByte(stack.Extra, "amplify_type", "amplification_type", "byte_13", "value_13", "value_c")&0x80 != 0 {
		return 1
	}
	return 0
}

func currentExpertJobEnchantUpgradeCount(stack dnfrepo.ItemStack) byte {
	if value := sceneInventoryExtraByte(stack.Extra, "byte_12", "value_12", "enchant_upgrade_count"); value != 0 {
		return value
	}
	if len(stack.RawEntry) > 0x12 {
		return stack.RawEntry[0x12]
	}
	return 0
}

func currentExpertJobSetEnchantUpgradeCount(stack *dnfrepo.ItemStack, value byte) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 2)
	}
	stack.Extra["byte_12"] = strconv.Itoa(int(value))
	stack.Extra["enchant_upgrade_count"] = strconv.Itoa(int(value))
	if len(stack.RawEntry) == currentItemListEntryWireSize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		stack.RawEntry[0x12] = value
	}
}

// grantCurrentExpertJobBead places the completed bead with its card-upgrade
// byte already attached. Generic reward placement knows only the item ID and
// would merge a +N bead into a base bead (or vice versa) before this byte was
// written, changing every item in the destination stack. Bead stack identity
// therefore includes the same +0x12 value used by card movement and direct
// enchant validation.
func grantCurrentExpertJobBead(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	upgradeCount byte,
) (int16, error) {
	if inventory == nil || definition.ItemID == 0 || definition.Kind != dungeonDropItemStackable ||
		definition.SlotStart < 0 || definition.SlotEnd < definition.SlotStart {
		return 0, dnfexpertjob.ErrRecipeUnavailable
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	// PVF [stack limit] 是角色背包（list 0）这个容器里该道具的总量上限：额度按容器里
	// 同 id 总量算，不是一个格子的余量。没写 [stack limit] 的道具沿用旧行为
	// （StackLimit <= 0 时助手返回 MaxInt32，不做总量限制）。
	//
	// 这条路径刻意不套用 CanOpenContainerItemRow（"同 id 只能有一行"）：宝珠行的堆身份
	// 还包含 +0x12 附魔等级（见上面的函数注释），而客户端的 op19 移动/整理本身就按这个
	// 身份分堆 —— inventory 包的 canStack 拒绝跨等级合并，
	// TestOwnerMoveAndSortKeepUpgradedMonsterCardSeparateFromBaseStack 也钉住了"基础
	// 宝珠 4 颗 + 满级宝珠 1 颗同容器两行"这一布局。若在此禁止开第二行，满级附魔师只要
	// 背包里还有一颗基础宝珠就再也做不出 +N 宝珠（整包被拒绝）。总量上限仍然生效：
	// 额度用尽的宝珠和并包失败一样，走下面既有的 errDungeonPickupInventoryFull 拒绝路径。
	space := int64(math.MaxInt32)
	if definition.Kind == dungeonDropItemStackable && definition.StackLimit > 0 {
		space = dnfrepo.ContainerItemSpace(
			inventory.Slots,
			dnfrepo.MainInventoryListType,
			int64(definition.ItemID),
			definition.StackLimit,
		)
	}
	for slot := definition.SlotStart; slot <= definition.SlotEnd; slot++ {
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		stack, found := inventory.Slots[key]
		if !found || stack.ItemID != int64(definition.ItemID) || stack.Bind || stack.Count <= 0 ||
			!currentItemStackExpirationMatches(stack, definition.ExpirationDate) ||
			currentExpertJobEnchantUpgradeCount(stack) != upgradeCount {
			continue
		}
		limit := int64(math.MaxInt32)
		if definition.StackLimit > 0 {
			limit = definition.StackLimit
		}
		if stack.Count >= limit || space <= 0 {
			continue
		}
		stack.Count++
		space--
		currentExpertJobSetEnchantUpgradeCount(&stack, upgradeCount)
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[key] = stack
		return slot, nil
	}
	for slot := definition.SlotStart; slot <= definition.SlotEnd; slot++ {
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		// 总量额度已经用尽就不再入包：宝珠和并包不下时一样，走下面既有的
		// errDungeonPickupInventoryFull 拒绝路径，绝不把容器写到 [stack limit] 之上。
		if space <= 0 {
			break
		}
		stack, err := buildCurrentDungeonPickupStack(slot, definition, 1)
		if err != nil {
			return 0, err
		}
		currentExpertJobSetEnchantUpgradeCount(&stack, upgradeCount)
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[key] = stack
		return slot, nil
	}
	return 0, errDungeonPickupInventoryFull
}

var errCurrentExpertJobInsufficientMaterials = errors.New("expert job materials are insufficient")

func currentExpertJobCompoundError(err error) byte {
	switch {
	case errors.Is(err, errCurrentExpertJobInsufficientMaterials):
		return 21
	case errors.Is(err, dnfexpertjob.ErrLevelTooLow):
		return 14
	case errors.Is(err, errDungeonPickupInventoryFull), errors.Is(err, errCurrentDisjointRewardInvalid):
		return 4
	default:
		return 19
	}
}

func currentExpertJobExtractionError(err error) byte {
	switch {
	case errors.Is(err, errDungeonPickupInventoryFull), errors.Is(err, errCurrentDisjointRewardInvalid):
		return 4
	case errors.Is(err, dnfexpertjob.ErrExtractorInvalid), errors.Is(err, dnfexpertjob.ErrExtractionInvalid):
		return 13
	default:
		return 19
	}
}

func buildCurrentExpertJobCompoundSuccessBody(plan dnfexpertjob.CompoundPlan) []byte {
	w := packetWriter{}
	w.writeByte(1)
	w.writeByte(byte(len(plan.AttemptedOutputs)))
	for _, output := range plan.AttemptedOutputs {
		w.writeUint32(uint32(output.ItemID))
		w.writeUint32(uint32(output.Count))
	}
	w.writeUint32(uint32(plan.SuccessCount))
	w.writeUint32(uint32(plan.FailureCount))
	w.writeByte(0)
	return w.bytes()
}

func buildCurrentExpertJobExtractionSuccessBody(result currentExpertJobExtractionResult) []byte {
	w := packetWriter{}
	w.writeByte(1)
	w.writeByte(result.TargetList)
	w.writeUint16(uint16(result.TargetSlot))
	w.writeByte(byte(len(result.Materials)))
	for _, material := range result.Materials {
		w.writeUint16(uint16(material.Slot))
		w.writeUint32(material.ItemID)
		w.writeUint32(material.Count)
	}
	return w.bytes()
}
