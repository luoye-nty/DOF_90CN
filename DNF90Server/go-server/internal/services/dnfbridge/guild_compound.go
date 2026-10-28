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
// # 「合成失败」为什么要单独发一帧
//
// 原生码表里**没有一条是"合成失败"**，而失败 ACK 又**必须发**：失败分支末尾
// 0x01D2A948 会对窗口 430 调 sub_E622C0(1) 把合成界面解锁，不发这一帧界面就卡死。
// 所以掷骰失败一定会先弹出一句笼统的「无法进行合成。」，服务端挡不住。
//
// 解法是走 90CN.dll 早就有的自定义公告通道（class0/op413 type 4），**style 2**：
// 它开的正是客户端自己那个 scene UI owner **0x298** 公告窗 —— 和上面那句
// 「无法进行合成。」是**同一个窗口**。而 90CN.dll 的 OpenNativeAnnouncementWindow
// 在开之前会先关掉已经打开的那一个，所以在 ACK **之后**补这一帧，就等于把前一句
// 顶掉、换成「合成失败」。玩家最终只看到后者。
//
// 顺序因此是死的：失败 ACK -> 重建 list 38 -> 公告帧。反过来发会被 ACK 的弹窗盖住。
const (
	currentGuildCompoundFlagMsg = uint16(dnfenum.CmdPacketCompoundFlag)
	currentGuildCompoundGemMsg  = uint16(dnfenum.CmdPacketCompoundGem)

	currentGuildCompoundRequestSize = 37
	currentGuildCompoundSlotCount   = 4
	currentGuildCompoundHeaderSize  = 13
	currentGuildCompoundEmptySlot   = int32(-1)
	// 掷骰失败与规则性拒绝共用默认分支，都会先弹「无法进行合成。」；掷骰失败
	// 随后用 0x298 公告帧把它顶成「合成失败」。
	currentGuildCompoundErrorRollFailed = byte(1)
	currentGuildCompoundErrorRejected   = byte(1)
	// 产物放不下：显示「请整理物品栏的空间后再试。」。
	currentGuildCompoundErrorNoSpace = byte(95)
	// 顶替文案。dstr 19212 的原文，这两个 handler 够不着它，只能照抄。
	currentGuildCompoundFailureNotice = "合成失败"
)

var (
	errCurrentGuildCompoundRequestMalformed = errors.New("dnf guild compound request is malformed")
	errCurrentGuildCompoundMaterialInvalid  = errors.New("dnf guild compound material is invalid")
	errCurrentGuildCompoundRecipeMissing    = errors.New("dnf guild compound recipe is missing")
	errCurrentGuildCompoundTopOfLadder      = errors.New("dnf guild compound rarity is the top of the ladder")
	errCurrentGuildCompoundNoCandidate      = errors.New("dnf guild compound has no result candidate")
	errCurrentGuildCompoundOwnerUnavailable = errors.New("dnf guild compound owner is unavailable")
	errCurrentGuildCompoundNoSpace          = errors.New("dnf guild compound result has no free list-38 slot")
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
	Kind       dnfguild.CompoundKind
	Rarity     uint32
	NextRarity uint32
	Recipe     dnfguild.CompoundRecipe
	Materials  []uint32
	// MaterialRarities 是每件材料的真实稀有度，用来观察跨品级合成时客户端到底
	// 往 rarity 字段里写了什么。
	MaterialRarities []uint32
	MixedRarities    bool
	SuccessRate      uint32
	Roll             uint32
	Success          bool
	ResultItem       uint32
	ResultSlot       int16
	ChangedKeys      []string
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
		"materials", fmt.Sprint(result.Materials),
		"material_count", request.Count,
		"max_material_count", result.Recipe.MaxMaterialCount,
		"rate_per_material", result.Recipe.RatePerMaterial,
		"success_rate", result.SuccessRate,
		"material_rarities", fmt.Sprint(result.MaterialRarities),
		"mixed_rarities", result.MixedRarities,
		"roll", result.Roll,
		"success", result.Success,
		"result_item", result.ResultItem,
		"result_slot", result.ResultSlot,
		"changed_rows", len(result.Updates),
		"refresh", "current_exe_op13_list38_rebuild_after_ack")

	// 发包顺序：ACK -> 重建 list 38 -> （失败时）公告帧。
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
	// 失败分支末尾 0x01D2A948 会对合成窗口 430 调 sub_E622C0(1) 做一次复位，
	// 复位会重画容器视图，先发的表会被它盖掉。宠物栏那条路径记录的顺序也是
	//「durable transaction -> class1 ack -> op13 list」。成功分支不需要容器先
	// 到位：sub_2759A70 是拿 ACK 里的产物 ID 直接建图标的，不从槽位解析。
	//
	// 勋章走同一个函数、同一条顺序，所以有同样的问题，这次一起修好。
	refreshList := func() error {
		return s.sendSelectedCurrentContainerListRefresh(
			session, "guild_compound", currentGuildMedalInventoryListType)
	}
	if !result.Success {
		if err := s.sendCurrentGuildCompoundFailure(
			session, opcode, currentGuildCompoundErrorRollFailed); err != nil {
			return err
		}
		if err := refreshList(); err != nil {
			return err
		}
		// ACK 已经把「无法进行合成。」弹在 0x298 公告窗上了。这一帧走同一个窗口，
		// 而 90CN.dll 开窗前会先关掉已开的那个，于是前一句被顶替成「合成失败」。
		// 必须在 ACK 之后发，顺序反了就会被盖住。
		return s.sendNotice(session, noticeRequest{
			Surface: noticeSurfaceNativeWindow,
			Text:    currentGuildCompoundFailureNotice,
			Source:  "guild_compound_failed",
		})
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
//
// 运营口径（2026-09-15）：**失败照常消耗材料**，成功率才有意义。所以扣除发生在
// 掷骰之前，且失败也照常提交。
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
			errCurrentGuildCompoundTopOfLadder, kind, request.Rarity)
	}
	// 件数是 2..上限的区间，不是定值：客户端的合成界面允许放 2..4 件，放得越多
	// 成功率越高。第一版把它当成"必须正好 4 件"，玩家放 2 件被一句
	//「无法进行合成。」挡掉。
	if !recipe.AcceptsMaterialCount(request.Count) {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d count=%d want=%d..%d",
			errCurrentGuildCompoundMaterialInvalid, kind, request.Rarity,
			request.Count, dnfguild.CompoundMinMaterialCount, recipe.MaxMaterialCount)
	}
	successRate := recipe.SuccessRateFor(request.Count)
	nextRarity, hasNext := ladder.NextRarity(request.Rarity)
	if !hasNext {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundTopOfLadder, kind, request.Rarity)
	}
	candidates, err := s.currentGuildCompoundCandidates(kind, nextRarity)
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	if len(candidates) == 0 {
		return currentGuildCompoundResult{}, fmt.Errorf("%w: kind=%s rarity=%d",
			errCurrentGuildCompoundNoCandidate, kind, nextRarity)
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
		Kind:        kind,
		Rarity:      request.Rarity,
		NextRarity:  nextRarity,
		Recipe:      recipe,
		SuccessRate: successRate,
		ResultSlot:  -1,
	}
	mixedRarities := false
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
				// 跨品级：允许丢比声明档位**更高**的材料进去（玩家自己亏，
				// 但不该被拒），只挡住比声明档位更低的 —— 那等于用垃圾换好货。
				// 客户端在混档时到底把哪一档写进 rarity 字段还没有证据，所以
				// 这里把每件材料的真实稀有度都记进日志，等实测样本来定案。
				if definition.Rarity < 0 || uint32(definition.Rarity) < request.Rarity {
					return fmt.Errorf("%w: slot=%d item=%d rarity=%d below declared=%d",
						errCurrentGuildCompoundMaterialInvalid, slot, stack.ItemID,
						definition.Rarity, request.Rarity)
				}
				if uint32(definition.Rarity) != request.Rarity {
					mixedRarities = true
				}
				result.MaterialRarities = append(result.MaterialRarities, uint32(definition.Rarity))
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

			// 掷骰在扣除之后：失败也照常提交，材料就此消失。
			result.Roll = uint32(rand.IntN(dnfguild.CompoundRateDenominator))
			result.Success = result.Roll < successRate
			if result.Success {
				resultItem, pickErr := currentGuildCompoundPickResult(kind, materials, nextRarity, candidates)
				if pickErr != nil {
					return pickErr
				}
				definition, defErr := items.ResolveItem(resultItem)
				if defErr != nil {
					return defErr
				}
				// 【合并适配 2026-09-18】本地这一侧的 grantCurrentNPCShopProduct 多一个
				// mainSlotCount（主背包已解锁槽位/开行上限）参数。这里合成结果只会是公会
				// 勋章 / 守护珠（[flag]/[flag gem]），函数内部走 list 38 的
				// grantCurrentGuildMedalListProduct 分支 —— 下一行的断言也强制要求
				// listType == currentGuildMedalInventoryListType，永远到不了主背包分支，
				// 因此传 0（=未购买扩容页）与上游版本的 3 参数调用完全等价。
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
			}
			result.ChangedKeys = changed
			result.Updates = updates
			result.MixedRarities = mixedRarities
			record.UpdatedAt = now
			return dnfrepo.SaveInventoryFields(ctx, inventories, record, dnfrepo.InventoryFieldSlots)
		},
	)
	if err != nil {
		return currentGuildCompoundResult{}, err
	}
	return result, nil
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

// currentGuildCompoundPickResult 选产物：守护珠优先同属性升一档，其余随机。
func currentGuildCompoundPickResult(
	kind dnfguild.CompoundKind,
	materials []uint32,
	nextRarity uint32,
	candidates []uint32,
) (uint32, error) {
	if len(candidates) == 0 {
		return 0, fmt.Errorf("%w: kind=%s rarity=%d", errCurrentGuildCompoundNoCandidate, kind, nextRarity)
	}
	if kind == dnfguild.CompoundGem {
		if successor, ok := currentGuildCompoundGemFamilySuccessor(materials, nextRarity, candidates); ok {
			return successor, nil
		}
	}
	return candidates[rand.IntN(len(candidates))], nil
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
