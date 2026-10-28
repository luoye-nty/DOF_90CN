package dnfbridge

// op515（use stackable action）里 action=157（`[action type] [use favor point]`，例：充满爱慕的信）：
// **一次使用 = 一张券换 1000 点好感度**，收尾全部在这里做完。
//
// 线上抓包（2026-10-16 20:0x，角色 3）：
//
//	body 19 字节：u16 槽位@0 = 0x44(68)，u32 动作号@7 = 0x9d(157)，u32@11 = 选中的 NPC
//	（实测 17 / 384 / 7 —— 三个值都在该道具 `[npc select category]` 白名单里）
//
// 2026-10-16 20:19 抓包复核：两次点击的包体**完全一致**（都是槽位 68 + 动作 157），
// 只有 +11 的 NPC 不同（384 → 7 = G.S.D.）。所以"右键使用"和"弹窗里选完 NPC 按按钮"
// 在服务端看不出区别 —— 按官方语义就是同一个操作：拿手上的券给这个 NPC 加 1000 点。
// 之前按"先登记、再落地"拆两段，结果第二段永远进不去（槽位里还有券），每次点击都只是
// "再用一次"，于是：券一张张被扣掉、好感度永远是 0、弹窗一直不关。

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// tryCurrentFavorLetterAction 在 op515 上接管 action=157：扣 1 张券 + 给包里的 NPC 加点数 + 回执。
// 返回 handled=false 表示这一包不是那条动作（交给原有链路）。
func (s *Service) tryCurrentFavorLetterAction(session *gameSession, typ uint16, body []byte) (bool, error) {
	if session == nil || dnfenum.CmdPacket(typ) != dnfenum.CmdPacketUseStackableAction || len(body) != 19 {
		return false, nil
	}
	if binary.LittleEndian.Uint32(body[7:11]) != dnfinventory.FavorLetterActionIndex() {
		return false, nil
	}
	slot := int16(binary.LittleEndian.Uint16(body[0:2]))
	npc := binary.LittleEndian.Uint32(body[11:15])
	if slot < 0 || npc == 0 {
		return false, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return false, nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	fail := func(code byte, reason string) (bool, error) {
		s.logGameEvent(session, "game-npc-favor-letter-rejected",
			"code", code, "reason", reason, "npc", npc, "slot", slot)
		return true, s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseStackableAction), []byte{0, code}, 1)
	}
	stack, holding, loadErr := s.currentFavorUseSlotItem(session, repos, 0, slot)
	if loadErr != nil || !holding || stack.ItemID <= 0 {
		return false, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return fail(101, "pvf_unavailable")
	}
	definition, err := catalog.ResolveItem(uint32(stack.ItemID))
	if err != nil || !strings.EqualFold(strings.TrimSpace(definition.ActionType), currentPVFUseFavorPointAction) {
		return false, nil // 这一格不是好感度信件：交给原有链路
	}
	if definition.ActionValues == nil || len(definition.ActionValues.Values) == 0 || definition.ActionValues.Values[0] <= 0 {
		return fail(17, "letter_missing_favor_points")
	}
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
	// 扣 1 张券 + 给该 NPC 加点（同一事务；GiveNPCFavorItem 会自己校验槽位与数量）。
	result, err := dnfonlineevent.GiveNPCFavorItem(ctx, repos, maxPoints, npc,
		s.accountIDForSession(session), characterID, 0, slot,
		definition.ActionValues.Values[0], s.gameplayNow())
	if err != nil {
		code := byte(100)
		if errors.Is(err, dnfonlineevent.ErrNPCFavorItem) {
			code = 17
		}
		return fail(code, err.Error())
	}
	// 顺手把"待落地"标记清掉（历史上登记过；现在不再依赖它）。
	if latest, ok, loadErr := repos.Character.Load(ctx, characterID); loadErr == nil && ok {
		latest = dnfrepo.CloneCharacter(latest)
		dnfinventory.ClearPendingFavorLetter(&latest)
		_ = dnfrepo.SaveCharacterFields(ctx, repos.Character, latest, dnfrepo.CharacterFieldStats)
	}
	// 回执只发两条，且"操作回执"只发**一条**：
	//   ① op14 行刷新（把那一格的绝对数量发过去 —— 上一版去掉它之后，客户端数量不实时刷新，
	//      要重选角色才对，现场 4→1 就是这么来的）；
	//   ② 好感度回执（1, action, npc u32, u16 本次增加, u32 累计, u8）。
	//      2026-10-16 20:5x 现场：op515 上发这条时数量已经实时刷新，但**客户端弹窗仍不关**，
	//      说明"好感度/收尾"那一侧的处理器很可能绑在赠礼链用的 **op812** 上（客户端只有
	//      op812 这一条被证明过要处理 NPC 好感度回包）。这里改在 op812 上发同一条形状；
	//      op515 上不再发任何东西 —— 同一请求回两条 op515 会让客户端把一次使用算成两次。
	entry := currentItemListEntryFromStack(0, slot, result.Stack)
	if result.Remaining == 0 {
		entry = currentItemListEntry{}
		entry.patchCore(slot, 0xFFFFFFFF, 0)
	}
	if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, []currentItemListEntry{entry}), 0); err != nil {
		return true, err
	}
	var favor packetWriter
	favor.writeByte(1)
	favor.writeByte(0)
	favor.writeUint32(npc)
	favor.writeUint16(uint16(result.Added))
	favor.writeUint32(uint32(result.Points))
	favor.writeByte(0)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRequestNPCFavorOperation), favor.bytes(), 1); err != nil {
		return true, err
	}
	s.logGameEvent(session, "game-npc-favor-letter-committed",
		"npc", npc, "path", "op515_single_step", "letter_item", stack.ItemID,
		"affection_added", result.Added, "affection", result.Points, "cap", maxPoints,
		"remaining", result.Remaining)
	return true, nil
}
