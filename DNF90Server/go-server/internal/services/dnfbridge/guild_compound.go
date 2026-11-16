package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 公会勋章合成（op864）与守护珠合成（op835）。
//
// 这两个 opcode 此前在服务端**只有枚举名、没有任何处理器**，所以客户端发出去之后
// 石沉大海 —— 玩家看到的就是"点了合成没反应"。
//
// # 请求
//
// 两个包的包体都是 37 字节，结构相同（2026-09-15 实测两笔请求 + 比对数据库里的
// list 38 内容逐字段确认）：
//
//	byte[0..11]  12 字节客户端自带的校验/噪声，服务端不解析
//	byte[12]     u8  标志位（实测勋章 0x00、守护珠 0x80，含义未知，不参与裁决）
//	u32          材料件数
//	u32          被合成的稀有度（选配方行）
//	u32 slot[4]  list-38 槽位，-1 表示空位
//
// 实测样本：
//
//	勋章   count=2 rarity=0 slots=[0,1,-1,-1]   角色 38:0/38:1 都是 100380023（稀有度 0）
//	守护珠 count=4 rarity=2 slots=[49,49,49,49] 角色 38:49 是 90002 ×10（稀有度 2）
//
// 件数是 **2..4 的区间**，不是定值：客户端的合成界面就允许放 2 到 4 件，放得越多
// 成功率越高（玩家实测）。配方表只写了上限，下限 2 来自界面行为。
//
// 稀有度字段在**混档**时客户端到底写哪一档，目前没有样本。服务端的处理是：允许
// 材料稀有度**高于**声明档（玩家自己亏，不该被拒），只挡住低于声明档的（那等于
// 用垃圾换好货），并且把每件材料的真实稀有度都写进 game-guild-compound-committed
// 日志，等实测样本来定案。
//
// # 响应
//
// class-1，与 op22/op95 同一个约定：body[0] 是成功标志，失败时 body[1] 是错误码。
// 反汇编 sub_1D2A7E0（op864）与 sub_1D2A660（op835），两者形状完全一致：
//
//	成功：body = [1] + u32 产物 ItemID   （客户端 readU32 后建图标弹结果）
//	失败：body = [0, code]
//	     code 60 -> dstr 49416「正在使用个人商店， 无法进行合成。」
//	     code 95 -> dstr 33345「请整理物品栏的空间后再试。」
//	     其它    -> dstr 49417「无法进行合成。」
//	     （先过 sub_32DDC70 的通用码表，所以别用 12/54/77/105/114/115/122/134/
//	      136/137/167/208/210/214/229/230/232/234/237/239 这些已被占用的码）
//
// # 合成未升级不是失败
//
// guilddungeoncompound.etc 的成功率只决定产物是否提升稀有度，不决定是否产出物品。
// 掷骰未中时仍返回一件材料当前稀有度的勋章/守护珠，并照常走成功 ACK。只有请求
// 非法、材料不符、配方件数不支持或 list-38 无空间时，才返回失败 ACK。
const (
	currentGuildCompoundFlagMsg = uint16(dnfenum.CmdPacketCompoundFlag)
	currentGuildCompoundGemMsg  = uint16(dnfenum.CmdPacketCompoundGem)

	currentGuildCompoundRequestSize   = 37
	currentGuildCompoundSlotCount     = 4
	currentGuildCompoundHeaderSize    = 13
	currentGuildCompoundEmptySlot     = int32(-1)
	currentGuildCompoundErrorRejected = byte(1)
	// 产物放不下：显示「请整理物品栏的空间后再试。」。
	currentGuildCompoundErrorNoSpace = byte(95)
)

var (
	errCurrentGuildCompoundRequestMalformed  = errors.New("dnf guild compound request is malformed")
	errCurrentGuildCompoundMaterialInvalid   = errors.New("dnf guild compound material is invalid")
	errCurrentGuildCompoundRecipeMissing     = errors.New("dnf guild compound recipe is missing")
	errCurrentGuildCompoundRecipeUnavailable = errors.New("dnf guild compound recipe cannot accept materials")
	errCurrentGuildCompoundNoCandidate       = errors.New("dnf guild compound has no result candidate")
	errCurrentGuildCompoundOwnerUnavailable  = errors.New("dnf guild compound owner is unavailable")
	errCurrentGuildCompoundNoSpace           = errors.New("dnf guild compound result has no free list-38 slot")
)

type currentGuildCompoundCatalogCache struct {
	once    sync.Once
	catalog dnfguild.CompoundCatalog
	err     error
}

func (s *Service) currentGuildCompoundCatalog() (dnfguild.CompoundCatalog, error) {
	s.guildCompoundCatalog.once.Do(func() {
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			s.guildCompoundCatalog.err = err
			return
		}
		s.guildCompoundCatalog.catalog, s.guildCompoundCatalog.err =
			dnfguild.LoadCompoundCatalog(items.source)
	})
	return s.guildCompoundCatalog.catalog, s.guildCompoundCatalog.err
}

type currentGuildCompoundRequest struct {
	Flag   byte
	Count  uint32
	Rarity uint32
	Slots  []int16
}

func parseCurrentGuildCompoundRequest(body []byte) (currentGuildCompoundRequest, error) {
	if len(body) != currentGuildCompoundRequestSize {
		return currentGuildCompoundRequest{}, fmt.Errorf("%w: body_len=%d want=%d",
			errCurrentGuildCompoundRequestMalformed, len(body), currentGuildCompoundRequestSize)
	}
	request := currentGuildCompoundRequest{
		Flag:   body[currentGuildCompoundHeaderSize-1],
		Count:  binary.LittleEndian.Uint32(body[13:17]),
		Rarity: binary.LittleEndian.Uint32(body[17:21]),
	}
	for index := 0; index < currentGuildCompoundSlotCount; index++ {
		offset := 21 + index*4
		raw := int32(binary.LittleEndian.Uint32(body[offset : offset+4]))
		if raw == currentGuildCompoundEmptySlot {
			continue
		}
		if raw < 0 || raw > int32(currentGuardianGemPageSlotEnd) {
			return currentGuildCompoundRequest{}, fmt.Errorf("%w: slot[%d]=%d",
				errCurrentGuildCompoundRequestMalformed, index, raw)
		}
		request.Slots = append(request.Slots, int16(raw))
	}
	if request.Count == 0 || int(request.Count) != len(request.Slots) {
		return currentGuildCompoundRequest{}, fmt.Errorf("%w: count=%d filled_slots=%d",
			errCurrentGuildCompoundRequestMalformed, request.Count, len(request.Slots))
	}
	return request, nil
}

// currentGuildCompoundResult 是一次合成的判定结果。
type currentGuildCompoundResult struct {
	Kind         dnfguild.CompoundKind
	Rarity       uint32
	NextRarity   uint32
	OutputRarity uint32
	Recipe       dnfguild.CompoundRecipe
	Materials    []uint32
	UpgradeRate  uint32
	Roll         uint32
	Upgraded     bool
	ResultItem   uint32
	ResultSlot   int16
	ChangedKeys  []string
	// Updates 是本次真正变动的 list-38 行（材料扣减/清空 + 产物）。下发用的是
	// op13 整表重建而不是这些增量行（旧客户端对侧栏容器的即时 op14 不可靠），
	// 但仍然要把它们算出来：每一行同时也是写回 stack.RawEntry 的那份权威字节，
	// 整表重建正是从 RawEntry 拼出来的。此外它进日志，出问题时能逐行对账。
	Updates []currentItemListEntry
}

func (s *Service) handleCurrentGuildCompound(
	session *gameSession,
	kind dnfguild.CompoundKind,
	body []byte,
) error {
	opcode := currentGuildCompoundGemMsg
	if kind == dnfguild.CompoundFlag {
		opcode = currentGuildCompoundFlagMsg
	}
	request, err := parseCurrentGuildCompoundRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-guild-compound-rejected",
			"kind", kind.String(), "body_len", len(body), "reason", err)
		return s.sendCurrentGuildCompoundFailure(session, opcode, currentGuildCompoundErrorRejected)
	}
	s.logGameEvent(session, "game-guild-compound-request",
		"kind", kind.String(),
		"char_id", session.selectedCharacterID,
		"flag_byte", request.Flag,
		"count", request.Count,
		"rarity", request.Rarity,
		"slots", fmt.Sprint(request.Slots))

	result, err := s.commitCurrentGuildCompound(session, kind, request)
	if err != nil {
		code := currentGuildCompoundErrorRejected
		if errors.Is(err, errCurrentGuildCompoundNoSpace) {
			code = currentGuildCompoundErrorNoSpace
		}
		s.logGameEvent(session, "game-guild-compound-rejected",
			"kind", kind.String(),
			"char_id", session.selectedCharacterID,
			"count", request.Count, "rarity", request.Rarity,
			"client_code", code, "reason", err)
		return s.sendCurrentGuildCompoundFailure(session, opcode, code)
	}

	s.logGameEvent(session, "game-guild-compound-committed",
		"kind", kind.String(),
		"char_id", session.selectedCharacterID,
		"rarity", result.Rarity,
		"next_rarity", result.NextRarity,
		"output_rarity", result.OutputRarity,
		"materials", fmt.Sprint(result.Materials),
		"material_count", request.Count,
		"max_material_count", result.Recipe.MaxMaterialCount,
		"rate_per_material", result.Recipe.RatePerMaterial,
		"upgrade_rate", result.UpgradeRate,
		"roll", result.Roll,
		"upgraded", result.Upgraded,
		"result_item", result.ResultItem,
		"result_slot", result.ResultSlot,
		"changed_rows", len(result.Updates),
		"refresh", "current_exe_op13_list38_rebuild_after_ack")

	// 发包顺序：成功 ACK -> 重建 list 38。
	//
	// # 为什么不是增量 op14
	//
	// 材料**每一次都扣对了**：日志里 op14 的 amount 逐次递减（10->8->6->4），
	// 数据库最终值也对得上。问题在客户端不刷新剩余数量。这和宠物栏那条已知问题
	// 是同一类 —— sendSelectedPetInventoryListRefresh 的注释写着"list-7 的即时
	// op14 路径在这个旧客户端上不稳定"，侧栏容器只能靠 op13 整表重建才可靠。
	// list 38 同理，所以这里改用 sendSelectedCurrentContainerListRefresh 只重建
	// list 38 一张表。
	//
	// 它比 sendSelectedCurrentContainerListsRefresh 轻得多：后者会重发 list
	// 0/1/2/7/38 五个容器（实测仅 list 0 就 9287 字节 78 行）外加 op251 锁定快照，
	// 客户端为此把所有容器重新过一遍，顺带弹出一句和合成毫无关系的 dstr 11368
	//「未设置Buff集成系统。」。
	//
	// # 为什么放在 ACK 之后
	//
	// sub_2759A70 是拿 ACK 里的产物 ID 直接建图标，不从槽位解析；随后再重建
	// list 38，让扣除后的材料和新产物同步到侧栏容器。
	refreshList := func() error {
		return s.sendSelectedCurrentContainerListRefresh(
			session, "guild_compound", currentGuildMedalInventoryListType)
	}
	if err := s.sendGameUpperRawClass(
		session,
		opcode,
		buildCurrentGuildCompoundSuccessBody(result.ResultItem),
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	return refreshList()
}

func buildCurrentGuildCompoundSuccessBody(itemID uint32) []byte {
	body := make([]byte, 0, 5)
	body = append(body, 1)
	return binary.LittleEndian.AppendUint32(body, itemID)
}

func (s *Service) sendCurrentGuildCompoundFailure(
	session *gameSession,
	opcode uint16,
	code byte,
) error {
	return s.sendGameUpperRawClass(
		session, opcode, []byte{0, code}, dnfproto.DefaultChannelClassification)
}

// commitCurrentGuildCompound 在一笔事务里校验材料、扣除、掷骰并落库产物。
// 掷骰未中只表示稀有度没有提升，仍返回当前稀有度产物，因此每次合法合成都必须
// 产生一件结果。
func (s *Service) commitCurrentGuildCompound(
	session *gameSession,
	kind dnfguild.CompoundKind,
	request currentGuildCompoundRequest,
) (currentGuildCompoundResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentGuildCompoundResult{}, errCurrentGuildCompoundOwnerUnavailable
	}
	catalog, err := s.currentGuildCompoundCatalog()
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	ladder := catalog.Ladder(kind)
	recipe, found := ladder.Recipe(request.Rarity)
	if !found {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundRecipeMissing, kind, request.Rarity)
	}
	if !recipe.Compoundable() {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundRecipeUnavailable, kind, request.Rarity)
	}
	// 件数是 2..上限的区间，不是定值：客户端的合成界面允许放 2..4 件，放得越多
	// 成功率越高。第一版把它当成"必须正好 4 件"，玩家放 2 件被一句
	//「无法进行合成。」挡掉。
	if !recipe.AcceptsMaterialCount(request.Count) {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d count=%d want=%d..%d",
			errCurrentGuildCompoundMaterialInvalid, kind, request.Rarity,
			request.Count, dnfguild.CompoundMinMaterialCount, recipe.MaxMaterialCount)
	}
	upgradeRate := recipe.UpgradeRateFor(request.Count)
	nextRarity, hasNext := ladder.NextRarity(request.Rarity)
	currentCandidates, err := s.currentGuildCompoundCandidates(kind, request.Rarity)
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	if len(currentCandidates) == 0 {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundNoCandidate, kind, request.Rarity)
	}
	var upgradeCandidates []uint32
	if hasNext {
		upgradeCandidates, err = s.currentGuildCompoundCandidates(kind, nextRarity)
		if err != nil {
			return currentGuildCompoundResult{}, err
		}
		if len(upgradeCandidates) == 0 {
			return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
				errCurrentGuildCompoundNoCandidate, kind, nextRarity)
		}
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return currentGuildCompoundResult{}, errCurrentGuildCompoundOwnerUnavailable
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	now := s.gameplayNow().UTC()
	result := currentGuildCompoundResult{
		Kind:         kind,
		Rarity:       request.Rarity,
		NextRarity:   nextRarity,
		OutputRarity: request.Rarity,
		Recipe:       recipe,
		UpgradeRate:  upgradeRate,
		ResultSlot:   -1,
	}
	err = repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			_ dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			record, found, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !found {
				return errCurrentGuildCompoundOwnerUnavailable
			}
			record = dnfrepo.CloneInventory(record)
			if record.Slots == nil {
				return errCurrentGuildCompoundMaterialInvalid
			}
			// 每个槽位被点了几次就要扣几个：守护珠是可堆叠的，四个材料很可能
			// 全部来自同一格。
			need := make(map[int16]int64, len(request.Slots))
			for _, slot := range request.Slots {
				need[slot]++
			}
			materials := make([]uint32, 0, len(request.Slots))
			for _, slot := range request.Slots {
				key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slot)
				stack, present := record.Slots[key]
				if !present || stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) {
					return fmt.Errorf("%w: slot=%d empty", errCurrentGuildCompoundMaterialInvalid, slot)
				}
				definition, defErr := items.ResolveItem(uint32(stack.ItemID))
				if defErr != nil {
					return fmt.Errorf("%w: slot=%d item=%d: %v",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.ItemID, defErr)
				}
				if !currentGuildCompoundMaterialMatches(kind, definition) {
					return fmt.Errorf("%w: slot=%d item=%d is not a %s material",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.ItemID, kind)
				}
				// 勋章与守护珠都禁止混级。客户端已经限制只能放入同档材料，
				// 服务端仍必须独立校验，避免伪造请求用高稀有度材料换取
				// 当前档产物，或把不同档材料混在一起计算成功率。
				if !currentGuildCompoundMaterialRarityMatches(request.Rarity, definition.Rarity) {
					return fmt.Errorf("%w: slot=%d item=%d rarity=%d declared=%d",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.ItemID,
						definition.Rarity, request.Rarity)
				}
				if currentNPCShopItemLocked(stack) {
					return fmt.Errorf("%w: slot=%d item=%d is locked",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.ItemID)
				}
				if stack.Count < need[slot] {
					return fmt.Errorf("%w: slot=%d has %d, needs %d",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.Count, need[slot])
				}
				materials = append(materials, uint32(stack.ItemID))
			}
			result.Materials = materials

			changed := make([]string, 0, len(need)+1)
			updates := make([]currentItemListEntry, 0, len(need)+1)
			consumedSlots := make([]int16, 0, len(need))
			for slot := range need {
				consumedSlots = append(consumedSlots, slot)
			}
			sort.Slice(consumedSlots, func(i, j int) bool { return consumedSlots[i] < consumedSlots[j] })
			for _, slot := range consumedSlots {
				key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slot)
				stack := record.Slots[key]
				stack.Count -= need[slot]
				var entry currentItemListEntry
				if stack.Count <= 0 {
					delete(record.Slots, key)
					// 空格子用 itemID = 0xFFFFFFFF 的删除行表示，和 NPC 出售
					// 卖空一格时发的是同一种行。
					entry.patchCore(slot, math.MaxUint32, 0)
				} else {
					entry = currentItemListEntryFromStack(currentGuildMedalInventoryListType, slot, stack)
					stack.RawEntry = append([]byte(nil), entry.data[:]...)
					record.Slots[key] = stack
				}
				updates = append(updates, entry)
				changed = append(changed, key)
			}

			// 掷骰只决定稀有度是否升级；无论结果如何都必须产出并提交一件物品。
			result.Roll = uint32(rand.IntN(dnfguild.CompoundRateDenominator))
			result.Upgraded = result.Roll < upgradeRate
			resultCandidates := currentCandidates
			if result.Upgraded {
				result.OutputRarity = nextRarity
				resultCandidates = upgradeCandidates
			}
			resultItem, pickErr := currentGuildCompoundPickResult(
				kind,
				materials,
				result.Upgraded,
				result.OutputRarity,
				resultCandidates,
			)
			if pickErr != nil {
				return pickErr
			}
			definition, defErr := items.ResolveItem(resultItem)
			if defErr != nil {
				return defErr
			}
			// 合成结果只可能是公会勋章 / 守护珠（[flag]/[flag gem]），
			// grantCurrentNPCShopProduct 会走 list 38；mainSlotCount 不参与。
			listType, slots, grantErr := grantCurrentNPCShopProduct(&record, definition, 1, 0)
			if grantErr != nil {
				return fmt.Errorf("%w: result item=%d: %v",
					errCurrentGuildCompoundNoSpace, resultItem, grantErr)
			}
			if listType != currentGuildMedalInventoryListType || len(slots) != 1 {
				return fmt.Errorf("%w: result item=%d landed in list %d slots %v",
					errCurrentGuildCompoundNoSpace, resultItem, listType, slots)
			}
			result.ResultItem = resultItem
			result.ResultSlot = slots[0]
			resultKey := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slots[0])
			changed = append(changed, resultKey)
			updates = append(updates, currentItemListEntryFromStack(
				currentGuildMedalInventoryListType, slots[0], record.Slots[resultKey]))
			result.ChangedKeys = changed
			result.Updates = updates
			record.UpdatedAt = now
			return dnfrepo.SaveInventoryFields(ctx, inventories, record, dnfrepo.InventoryFieldSlots)
		},
	)
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	return result, nil
}

func currentGuildCompoundMaterialRarityMatches(requested uint32, actual int64) bool {
	return actual >= 0 && uint64(actual) == uint64(requested)
}

// currentGuildCompoundMaterialMatches 用的是和 list-38 归位完全相同的判据。
func currentGuildCompoundMaterialMatches(
	kind dnfguild.CompoundKind,
	definition dungeonDropItemDefinition,
) bool {
	listType, _, _, err := currentMailboxPVFPlacement(definition)
	if err != nil || listType != currentGuildMedalInventoryListType {
		return false
	}
	if kind == dnfguild.CompoundFlag {
		return definition.Kind == dungeonDropItemEquipment
	}
	return definition.Kind == dungeonDropItemStackable
}

// currentGuildCompoundPickResult 在目标稀有度内选产物。守护珠在四件材料同属
// 一种属性时保持该属性：升级返回下一档，未升级返回原档；其余情况随机。
func currentGuildCompoundPickResult(
	kind dnfguild.CompoundKind,
	materials []uint32,
	upgraded bool,
	outputRarity uint32,
	candidates []uint32,
) (uint32, error) {
	if len(candidates) == 0 {
		return 0, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundNoCandidate, kind, outputRarity)
	}
	if kind == dnfguild.CompoundGem {
		if len(materials) > 0 {
			sameFamily := true
			for _, material := range materials[1:] {
				if material != materials[0] {
					sameFamily = false
					break
				}
			}
			if sameFamily {
				if upgraded {
					if successor, ok := currentGuildCompoundGemFamilySuccessor(
						materials,
						outputRarity,
						candidates,
					); ok {
						return successor, nil
					}
				} else if currentGuildCompoundCandidateContains(candidates, materials[0]) {
					return materials[0], nil
				}
			}
		}
	}
	return candidates[rand.IntN(len(candidates))], nil
}

func currentGuildCompoundCandidateContains(candidates []uint32, itemID uint32) bool {
	for _, candidate := range candidates {
		if candidate == itemID {
			return true
		}
	}
	return false
}

func guildCompoundGameplayModule() gameplayModuleDefinition {
	return gameplayModuleDefinition{
		Name: "guild-compound",
		LegacyHandlers: map[uint16]gameplayHandler{
			currentGuildCompoundFlagMsg: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentGuildCompound(session, dnfguild.CompoundFlag, request.Body)
			},
			currentGuildCompoundGemMsg: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentGuildCompound(session, dnfguild.CompoundGem, request.Body)
			},
		},
	}
}
