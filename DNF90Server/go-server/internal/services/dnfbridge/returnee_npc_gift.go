package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// sub_10DD340: u8 action=0, u32 NPC, u8 inventory, u16 cube slot.
// sub_1D25180: success, action, NPC, u16 affection gain, u32 total, u8 used.
func (s *Service) handleCurrentReturneeNPCGift(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	// npc 只有 8 字节包体里才有；先读出来，后面的拒绝日志要带上它
	// （现场排查时"报错了但不知道是哪个 NPC"就靠这一条 + body hex）。
	var npc uint32
	if len(body) == 8 {
		npc = binary.LittleEndian.Uint32(body[1:5])
	}
	fail := func(code byte, reason string) error {
		s.logGameEvent(session, "game-returnee-npc-gift-rejected",
			"code", code, "reason", reason, "npc", npc, "body", fmt.Sprintf("%x", body))
		return s.sendGameUpperRawClass(session, 812, []byte{0, code}, 1)
	}
	if session.selectedCharacterID == 0 || len(body) != 8 || body[0] != 0 || (body[5] != 0 && body[5] != 36) {
		return fail(17, "invalid_request")
	}
	npc = binary.LittleEndian.Uint32(body[1:5])
	slot := int16(binary.LittleEndian.Uint16(body[6:8]))

	// 2026-10-16：「充满爱慕的信」这类 `[action type] [use favor point] N` 道具跟送礼
	// 共用这个 op812，但语义不同——它消耗的是道具本身、按道具自己的 `[npc select category]`
	// 选 NPC、**不要求玩家站在该 NPC 所在区域**。线上抓包（body=0002000000246601，
	// npc=2 / container=36 / slot=358）正是因为下面那条区域检查以 code=100 被拒，
	// 客户端于是弹窗不关、道具不扣。先按"这一格是不是信件"分流。
	// 先看有没有 op515-157 登记下来的"待落地信件"：这是当前客户端的正常路径
	// （先 op515 用券，再 op812 交上选中的 NPC）。
	if handled, err := s.tryCurrentPendingFavorLetter(session, npc, body[5], slot); handled || err != nil {
		return err
	}
	// 兼容另一条形状：op812 直接带信件所在那一格（部分客户端/旧行为）。
	if handled, err := s.tryCurrentLetterFavorUse(session, npc, body[5], slot); handled || err != nil {
		return err
	}

	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if inDungeon {
		return fail(100, "dungeon_scene")
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	ready := session.townSceneReadyCharacterID == session.selectedCharacterID && position.CharacterID == session.selectedCharacterID
	session.townMu.Unlock()
	// 2026-10-16 调整（方案 2）：不再要求"该 NPC 出现在当前区域的 NPC 列表里"。
	// 原因：区域 NPC 列表来自 PVF 地图数据，实测多张图缺条目 —— 赛丽亚房间
	// （Cataclysm/Town/Elvengard/new_seria_room.map）的列表是空的、斯卡迪女王那类
	// 场景也没列全，于是"站在 NPC 面前也送不了"。客户端本来只允许与面前的 NPC 交互，
	// 所以这里只要求"在城里且场景已就绪"，NPC 是否可赠仍由后面的
	// LoadReturneeGiftRules（npcfavorsystem.etc 里有没有该 NPC 的好感度规则）把关。
	if !ready {
		return fail(100, fmt.Sprintf("town_scene_not_ready town=%d area=%d ready=%t", position.TownID, position.AreaID, ready))
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return fail(101, "pvf_unavailable")
	}
	rules, err := dnfonlineevent.LoadReturneeGiftRules(archive, npc)
	if err != nil {
		return fail(101, err.Error())
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return fail(101, "repository_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := dnfonlineevent.GiveReturneeNPCGift(ctx, repos, rules, s.accountIDForSession(session), strconv.Itoa(int(session.selectedCharacterID)), body[5], slot, s.gameplayNow())
	if err != nil {
		code := byte(100)
		switch {
		case errors.Is(err, dnfonlineevent.ErrNPCGiftLevel):
			code = 14
		case errors.Is(err, dnfonlineevent.ErrNPCGiftItem):
			code = 17
		case errors.Is(err, dnfonlineevent.ErrNPCGiftCount):
			code = 22
		case errors.Is(err, dnfonlineevent.ErrNPCGiftLimit):
			code = 102
		}
		return fail(code, err.Error())
	}
	entry := currentItemListEntryFromStack(0, slot, result.Stack)
	if result.Remaining == 0 {
		if slot >= 354 {
			entry.patchCore(slot, uint32(result.Stack.ItemID), 0)
		} else {
			entry = currentItemListEntry{}
			entry.patchCore(slot, 0xFFFFFFFF, 0)
		}
	}
	if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, []currentItemListEntry{entry}), 0); err != nil {
		return err
	}
	var w packetWriter
	w.writeByte(1)
	w.writeByte(0)
	w.writeUint32(npc)
	w.writeUint16(uint16(result.Added))
	w.writeUint32(uint32(result.Points))
	w.writeByte(byte(result.Used))
	if err := s.sendGameUpperRawClass(session, 812, w.bytes(), 1); err != nil {
		return err
	}
	s.logGameEvent(session, "game-returnee-npc-gift-committed", "npc", npc, "container", body[5], "slot", slot, "consumed", rules.Count, "remaining", result.Remaining, "affection_added", result.Added, "affection", result.Points, "daily_used", result.Used)
	return s.sendCurrentReturneeState(session, true)
}

// tryCurrentPendingFavorLetter 落地 op515-157 登记好的信件：op812 只负责带回选中的 NPC。
func (s *Service) tryCurrentPendingFavorLetter(session *gameSession, npc uint32, container byte, slot int16) (bool, error) {
	repos, ok := s.repositoryGroup()
	if !ok {
		return false, nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil || !found {
		return false, nil
	}
	pending, armed := dnfinventory.LoadPendingFavorLetter(character, s.gameplayNow())
	if !armed {
		return false, nil
	}
	fail := func(code byte, reason string) (bool, error) {
		s.logGameEvent(session, "game-npc-favor-letter-rejected",
			"code", code, "reason", reason, "npc", npc, "container", container, "slot", slot,
			"pending_item", pending.ItemID, "pending_slot", pending.SlotIndex)
		return true, s.sendGameUpperRawClass(session, 812, []byte{0, code}, 1)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return fail(101, "pvf_unavailable")
	}
	definition, err := catalog.ResolveItem(uint32(pending.ItemID))
	if err != nil || !strings.EqualFold(strings.TrimSpace(definition.ActionType), currentPVFUseFavorPointAction) {
		return fail(17, "pending_item_not_favor_letter")
	}
	if definition.ActionValues == nil || len(definition.ActionValues.Values) == 0 || definition.ActionValues.Values[0] <= 0 {
		return fail(17, "letter_missing_favor_points")
	}
	points := definition.ActionValues.Values[0]
	allowedNPC := false
	for _, id := range definition.FavorNPCIDs {
		if id > 0 && uint32(id) == npc {
			allowedNPC = true
			break
		}
	}
	if !allowedNPC {
		return fail(100, "npc_not_in_letter_category")
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return fail(101, "pvf_unavailable")
	}
	maxPoints, err := dnfonlineevent.LoadNPCFavorCap(archive, npc)
	if err != nil {
		return fail(101, err.Error())
	}
	// 券已经在 op515-157 那一步扣掉了，这里只补好感度（不再动道具）。
	result, err := dnfonlineevent.GrantNPCFavorPoints(ctx, repos, maxPoints, npc,
		characterID, points, s.gameplayNow())
	if err != nil {
		code := byte(100)
		if errors.Is(err, dnfonlineevent.ErrNPCFavorItem) {
			code = 17
		}
		return fail(code, err.Error())
	}
	if latest, ok, loadErr := repos.Character.Load(ctx, characterID); loadErr == nil && ok {
		latest = dnfrepo.CloneCharacter(latest)
		dnfinventory.ClearPendingFavorLetter(&latest)
		_ = dnfrepo.SaveCharacterFields(ctx, repos.Character, latest, dnfrepo.CharacterFieldStats)
	}
	// 这一格在 op515-157 那一步已经刷新过（数量已减），落地时不再重发行刷新，
	// 免得用一条空行把客户端上仍有的剩余数量涂掉。
	var w packetWriter
	w.writeByte(1)
	w.writeByte(0)
	w.writeUint32(npc)
	w.writeUint16(uint16(result.Added))
	w.writeUint32(uint32(result.Points))
	w.writeByte(0)
	if err := s.sendGameUpperRawClass(session, 812, w.bytes(), 1); err != nil {
		return true, err
	}
	s.logGameEvent(session, "game-npc-favor-letter-committed",
		"npc", npc, "path", "op515_arm_then_op812", "letter_item", pending.ItemID,
		"letter_slot", pending.SlotIndex, "points_configured", points,
		"affection_added", result.Added, "affection", result.Points, "remaining", result.Remaining)
	return true, nil
}

// tryCurrentLetterFavorUse 处理 `[action type] [use favor point] N` 道具（充满爱慕的信等）：
// 返回 handled=false 表示"这一格不是信件"，调用方继续走送礼分支。
func (s *Service) tryCurrentLetterFavorUse(session *gameSession, npc uint32, container byte, slot int16) (bool, error) {
	fail := func(code byte, reason string) (bool, error) {
		s.logGameEvent(session, "game-npc-favor-letter-rejected",
			"code", code, "reason", reason, "npc", npc, "container", container, "slot", slot)
		return true, s.sendGameUpperRawClass(session, 812, []byte{0, code}, 1)
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return false, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return false, nil
	}
	stack, found, err := s.currentFavorUseSlotItem(session, repos, container, slot)
	if err != nil || !found || stack.ItemID <= 0 {
		return false, nil
	}
	definition, err := catalog.ResolveItem(uint32(stack.ItemID))
	if err != nil || !strings.EqualFold(strings.TrimSpace(definition.ActionType), currentPVFUseFavorPointAction) {
		return false, nil // 不是信件：交给原来的送礼分支
	}
	if definition.ActionValues == nil || len(definition.ActionValues.Values) == 0 || definition.ActionValues.Values[0] <= 0 {
		return fail(17, "letter_missing_favor_points")
	}
	points := definition.ActionValues.Values[0]
	allowedNPC := false
	for _, id := range definition.FavorNPCIDs {
		if id > 0 && uint32(id) == npc {
			allowedNPC = true
			break
		}
	}
	if !allowedNPC {
		// 白名单就是道具 PVF 里的 `[npc select category]`。
		return fail(100, "npc_not_in_letter_category")
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return fail(101, "pvf_unavailable")
	}
	maxPoints, err := dnfonlineevent.LoadNPCFavorCap(archive, npc)
	if err != nil {
		return fail(101, err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := dnfonlineevent.GiveNPCFavorItem(ctx, repos, maxPoints, npc,
		s.accountIDForSession(session), strconv.Itoa(int(session.selectedCharacterID)),
		container, slot, points, s.gameplayNow())
	if err != nil {
		code := byte(100)
		if errors.Is(err, dnfonlineevent.ErrNPCFavorItem) {
			code = 17
		}
		return fail(code, err.Error())
	}
	// 行刷新：与送礼同一形状（账号共享仓库那一段在客户端的 list0 里就是 354+ 的槽位）。
	entry := currentItemListEntryFromStack(0, slot, result.Stack)
	if result.Remaining == 0 {
		if slot >= 354 {
			entry.patchCore(slot, uint32(result.Stack.ItemID), 0)
		} else {
			entry = currentItemListEntry{}
			entry.patchCore(slot, 0xFFFFFFFF, 0)
		}
	}
	if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, []currentItemListEntry{entry}), 0); err != nil {
		return true, err
	}
	var w packetWriter
	w.writeByte(1)
	w.writeByte(0)
	w.writeUint32(npc)
	w.writeUint16(uint16(result.Added))
	w.writeUint32(uint32(result.Points))
	w.writeByte(0)
	if err := s.sendGameUpperRawClass(session, 812, w.bytes(), 1); err != nil {
		return true, err
	}
	s.logGameEvent(session, "game-npc-favor-letter-committed",
		"npc", npc, "container", container, "slot", slot,
		"points_configured", points, "affection_added", result.Added,
		"affection", result.Points, "remaining", result.Remaining)
	return true, nil
}

// currentFavorUseSlotItem 取 (container, slot) 那一格的道具：36 = 账号共享仓库，0 = 角色背包。
func (s *Service) currentFavorUseSlotItem(session *gameSession, repos dnfrepo.Group, container byte, slot int16) (dnfrepo.ItemStack, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if container == 36 {
		account, found, err := repos.AccountInventory.Load(ctx, s.accountIDForSession(session))
		if err != nil || !found {
			return dnfrepo.ItemStack{}, false, err
		}
		stack, ok := account.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]
		return stack, ok, nil
	}
	inventory, found, err := repos.Inventory.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return dnfrepo.ItemStack{}, false, err
	}
	stack, ok := inventory.Slots[dnfrepo.InventorySlotKey(dnfrepo.MainInventoryListType, slot)]
	return stack, ok, nil
}

func (s *Service) currentTownHasGiftNPC(position currentTownPositionSnapshot, npc uint32) bool {
	area, ok := s.townArea(int64(position.TownID), int64(position.AreaID))
	if !ok || npc == 0 {
		return false
	}
	for _, spawn := range area.NPCs {
		if spawn.NPCID == int64(npc) {
			return true
		}
	}
	return false
}
