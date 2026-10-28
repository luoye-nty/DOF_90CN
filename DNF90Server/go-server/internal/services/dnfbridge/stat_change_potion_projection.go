package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 城镇增益药剂（史诗Buff药剂一族）真正让**图标出现**的那一发。
//
// 现场 2026-10-03（服主报，测试x = char 11）：药剂修好"能用"之后，客户端只弹了一行
// "[史诗Buff药剂]的效果时间还剩10分钟"，buff 栏里没有图标。原因是 class0/op174
// （load effect item info）在这个客户端里是**效果道具的倒计时列表**，不是 buff 注册。
//
// ★ 2026-10-03 第四轮：通知号在**我们自己的客户端**里定标，不照抄参考服务端 ★
//
// 服主指出"A21 客户端与我们不同，协议内容和协议号可能都不一样，不能直接照抄"——复核后
// 确认他是对的。前几轮用的 488/489/490 是按 A21 枚举（CHURN_GRADE_ACTION=486）推出来的，
// 而本客户端的 NOTIPACKET 基址是 **485**。定标方法（全部用本仓库**现场验证过**的号）：
//
//	客户端启动时把 294 个 ENUM_NOTIPACKET_* 名字按枚举顺序注册进连续全局
//	（0x52bc170 + 4*i，脚本 tools/_gb_noti_table_re.py），于是 id = base + slot；
//
//	slot  98 RAID_SET_SYMBOL                 ↔ raid/body.go RaidSymbolMsgID        = 0x247 = 583
//	slot  99 RAID_DUNGEON_PARTICIPATION_INFO ↔ RaidParticipationMsgID             = 0x248 = 584
//	slot 100 RAID_DUNGEON_STATE              ↔ RaidDungeonStateMsgID              = 0x249 = 585
//	slot 101 RAID_CHANGE_DUNGEON_STATE       ↔ RaidDungeonChangeMsgID             = 0x24a = 586
//	slot 102 RAID_STATE                      ↔ RaidStateMsgID                     = 0x24b = 587
//	    ⇒ base = 485（五个连号名字对五个连号 opcode，偏移恒为 485）
//	slot  95 SEND_DECK_INFO                  ↔ inventory/guardian_shield_protocol.go
//	                                            currentGuardianShieldDeckNotificationMsgID = 580
//	    ⇒ 独立锚点：580 - 95 = 485 ✓（"牌组信息"语义也吻合）
//
// 所以本客户端的 buff 三兄弟是 **487 / 488 / 489**：
//
//	487 CHARACTER_ADD_BUFF      建/更新 buff 条目（图标靠这一发）
//	488 CHARACTER_DEL_BUFF      删
//	489 CHARACTER_BUFF_DUNGEON  置"地下城内激活"标志（列表必须是**完整**激活集合）
//
// 之前发的 488/489/490 在本客户端分别是 DEL_BUFF / BUFF_DUNGEON /
// USER_FATIGUE_ACCELERATION_STATE —— 也就是说 ADD_BUFF **一次都没发出去过**，图标当然
// 不会出现（这解释了前面连续几次部署都没图标）。
//
// 协议**形状**仍沿用参考实现（形状与号无关，且与我们实测一致）：
// ADD = u8 1 + i32 buffId + 3×i32 0；DEL = u8 count + i32 ids…；
// BUFF_DUNGEON = u16 count + i32 ids…（空列表清本地激活标志）。
// 三个 i32 状态字段参考实现一律填 0（持续时长由 buff 定义表的 `[apply type] expire_time`
// 与客户端自己的窗口决定），我们也填 0。
const (
	currentCharacterAddBuffMsgID     uint16 = 487 // ENUM_NOTIPACKET_CHARACTER_ADD_BUFF
	currentCharacterRemoveBuffMsgID  uint16 = 488 // ENUM_NOTIPACKET_CHARACTER_DEL_BUFF
	currentCharacterBuffDungeonMsgID uint16 = 489 // ENUM_NOTIPACKET_CHARACTER_BUFF_DUNGEON
)

// currentCharacterAddBuffEvidence 记录这一组号是怎么定出来的（写进包日志，便于以后复核）。
const currentCharacterAddBuffEvidence = "notipacket base 485 calibrated on this client (raid 583..587=slot 98..102, deck-info 580=slot 95); ADD_BUFF=487 DEL_BUFF=488 BUFF_DUNGEON=489"

// buildCurrentCharacterBuffDungeonBody 是 CHARACTER_BUFF_DUNGEON 的记录体：
// u16 count + 每项 u32 buffId。空列表（count=0）用来清掉本地激活标志。
func buildCurrentCharacterBuffDungeonBody(buffIDs []int32) []byte {
	rows := make([]int32, 0, len(buffIDs))
	for _, buffID := range buffIDs {
		if buffID <= 0 {
			continue
		}
		rows = append(rows, buffID)
		if len(rows) == 0xFFFF {
			break
		}
	}
	body := make([]byte, 2+4*len(rows))
	binary.LittleEndian.PutUint16(body[0:2], uint16(len(rows)))
	for index, buffID := range rows {
		binary.LittleEndian.PutUint32(body[2+4*index:6+4*index], uint32(buffID))
	}
	return body
}

// buildCurrentCharacterAddBuffBody 是 CHARACTER_ADD_BUFF 的记录体（17 字节）。
func buildCurrentCharacterAddBuffBody(buffID int32) []byte {
	body := make([]byte, 17)
	body[0] = 1
	binary.LittleEndian.PutUint32(body[1:5], uint32(buffID))
	// 三个状态字段：参考实现全 0。
	return body
}

// buildCurrentCharacterRemoveBuffBody 是 CHARACTER_REMOVE_BUFF 的记录体。
func buildCurrentCharacterRemoveBuffBody(buffIDs []int32) []byte {
	rows := make([]int32, 0, len(buffIDs))
	for _, buffID := range buffIDs {
		if buffID <= 0 {
			continue
		}
		rows = append(rows, buffID)
		if len(rows) == 0xFF {
			break
		}
	}
	if len(rows) == 0 {
		return nil
	}
	body := make([]byte, 1+4*len(rows))
	body[0] = byte(len(rows))
	for index, buffID := range rows {
		binary.LittleEndian.PutUint32(body[1+4*index:5+4*index], uint32(buffID))
	}
	return body
}

// sendCurrentStatChangePotionBuffProjection 在角色容器可见之后，把仍然生效的
// 城镇增益药剂 buff 补发一次（对应参考实现的 EpicBuffPotionInitBodyBuilder）。
//
// 没有生效窗口就不发包：客户端登录时本来就是空的，而每次回城都补发一遍的话，
// 同一条 buff 会被反复 add。
func (s *Service) sendCurrentStatChangePotionBuffProjection(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		session.statChangePotionBuffProjectionSent {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return err
	}
	now := s.gameplayNow().UTC()
	active := s.currentStatChangePotionActiveWindows(character, now)
	if len(active) == 0 {
		// 一个生效窗口都没有：客户端不会自己摘图标（参考服务端的 buff 注册不带时长，靠它
		// 自己的定时器到期后补发 CHARACTER_DEL_BUFF）。所以在回城/登录取个天然检查点上，
		// 把这一族**所有** buff id 清一遍 —— 清一个客户端本来就没有的 buff 是无害的，
		// 而漏清会留下一个永远不掉的图标。
		s.cancelCurrentStatChangePotionTimer(session, "window_already_expired")
		return s.clearCurrentStatChangePotionBuffs(session, source)
	}
	// 恢复每一族：op174（效果道具倒计时）+ op487（buff 注册，图标）+ op489（激活）。
	// 只在恢复路径发 op174 —— 首次使用由客户端自己按 op44 本地登记并打那行提示
	//（服主 2026-10-03："药剂刚能用那一版提示只有一行"）。与经验药水那条完全同形。
	buffIDs := make([]int32, 0, len(active))
	latestExpire := time.Time{}
	for _, window := range active {
		if window.BuffID <= 0 || window.BuffID > math.MaxInt32 {
			continue
		}
		if err := s.sendCurrentStatChangePotionEffectItemInfoForWindow(session, window, source); err != nil {
			return err
		}
		// 只发"建条目"那一发：激活列表由下面**一次性**按完整集合发（否则同一发会重复，
		// 客户端会先清空再置位两次，白跑一遍）。
		if err := s.sendCurrentCharacterAddBuffOnly(session, int32(window.BuffID),
			"restore_"+source); err != nil {
			return err
		}
		buffIDs = append(buffIDs, int32(window.BuffID))
		if window.ExpireAt.After(latestExpire) {
			latestExpire = window.ExpireAt
		}
	}
	if len(buffIDs) == 0 {
		s.cancelCurrentStatChangePotionTimer(session, "no_projectable_window")
		return s.clearCurrentStatChangePotionBuffs(session, source)
	}
	// 第二发：把**完整**的激活集合发过去（客户端会先清空本地激活标志再按列表置位）。
	activateBody := buildCurrentCharacterBuffDungeonBody(buffIDs)
	if err := s.sendGameUpperRawClass(session, currentCharacterBuffDungeonMsgID, activateBody, 0); err != nil {
		return err
	}
	// op487 不带时长：按角色记录里的**真实**到期时刻武装"到期摘图标"定时器。多个族同时
	// 生效时按最晚的那个排，到点再重判（fire 会重新读角色，谁的点到了摘谁）。
	// 换频道/重登是新会话，所以这一发不会因为"每会话只补一次"而被跳过。
	s.armCurrentStatChangePotionTimer(session, session.selectedCharacterID, latestExpire, "restore_"+source)
	session.statChangePotionBuffProjectionSent = true
	return nil
}

// currentStatChangePotionWindow 是一个仍然生效、需要恢复给客户端的窗口。
type currentStatChangePotionWindow struct {
	ItemID   int64
	BuffID   int64
	Percent  int64
	ExpireAt time.Time
	Family   string
	Kind     string
}

// currentStatChangePotionActiveWindows 枚举角色身上**所有**仍然生效的这一族窗口：
//
//	掉落率（statbuff.ActiveEpicDropRate）
//	强化 / 增幅成功率（KindUpgradeSuccess / KindAmplifySuccess）
//	地下城通关经验（KindDungeonClearExp）
//	客户端自施放的那些族（城镇移速 village_move_speed、进图属性 move_speed …）
//
// 各族互不覆盖、各按自己的到期时间结束，所以必须逐个枚举 —— 只认掉落率那一族的话，
// 城镇移速/经验这类 buff 重登之后就再也回不来了。
func (s *Service) currentStatChangePotionActiveWindows(
	character dnfrepo.CharacterRecord,
	now time.Time,
) []currentStatChangePotionWindow {
	out := make([]currentStatChangePotionWindow, 0, 4)
	if effect, ok := statbuff.ActiveEpicDropRate(character, now); ok {
		out = append(out, currentStatChangePotionWindow{
			ItemID: effect.ItemID, BuffID: effect.BuffID, Percent: effect.MultiplierPercent,
			ExpireAt: effect.ExpireAt, Kind: "epic_drop_rate",
		})
	}
	for _, kind := range []statbuff.Kind{statbuff.KindUpgradeSuccess, statbuff.KindAmplifySuccess,
		statbuff.KindDungeonClearExp} {
		if window, ok := statbuff.ActiveRate(character, kind, now); ok {
			out = append(out, currentStatChangePotionWindow{
				ItemID: window.ItemID, BuffID: window.BuffID, Percent: window.Percent,
				ExpireAt: window.ExpireAt, Kind: string(kind),
			})
		}
	}
	for _, family := range s.currentClientBuffFamilies() {
		if window, ok := statbuff.ActiveClientBuff(character, family, now); ok {
			out = append(out, currentStatChangePotionWindow{
				ItemID: window.ItemID, BuffID: window.BuffID, Percent: window.Percent,
				ExpireAt: window.ExpireAt, Family: window.Family, Kind: "client_buff",
			})
		}
	}
	return out
}

// currentClientBuffFamilies 列出 PVF 里"客户端自施放"的那些 family（buff effect name）。
func (s *Service) currentClientBuffFamilies() []string {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return nil
	}
	tables, ok := catalog.currentVariousBuffTables()
	if !ok {
		return nil
	}
	return tables.clientBuffFamilies()
}

// sendCurrentStatChangePotionEffectItemInfoForWindow 是上面那一发的窗口版本。
func (s *Service) sendCurrentStatChangePotionEffectItemInfoForWindow(
	session *gameSession,
	window currentStatChangePotionWindow,
	source string,
) error {
	return s.sendCurrentStatChangePotionEffectItemInfo(session, statbuff.Effect{
		ItemID:   window.ItemID,
		BuffID:   window.BuffID,
		ExpireAt: window.ExpireAt,
	}, source)
}

// sendCurrentStatChangePotionEffectItemInfo 是登录/重登时的"效果道具倒计时"恢复
// （class0/op174）：{u8 1, u32 itemID, u32 remainingSeconds}。
//
// 客户端按 itemID 去 etc/variousbuffstackablelist.etc 认这件道具、再按掷到的 buff id
// 取 etc/variousbufflist.etc 的 [icon index]/[tooltip text]；这一发让"还剩多久"在重登
// 之后依然正确。使用当次**不能**发（会让客户端把同一个效果道具登记两次 → 两行提示）。
func (s *Service) sendCurrentStatChangePotionEffectItemInfo(
	session *gameSession,
	effect statbuff.Effect,
	source string,
) error {
	if effect.ItemID <= 0 || effect.ItemID > math.MaxUint32 {
		s.logGameEvent(session, "game-stat-change-potion-restore-skipped",
			"char_id", session.selectedCharacterID,
			"source", source,
			"item_id", effect.ItemID,
			"reason", "invalid_effect_item_id")
		return nil
	}
	remaining := int64(math.Ceil(effect.ExpireAt.Sub(s.gameplayNow().UTC()).Seconds()))
	if remaining < 1 {
		return nil
	}
	if remaining > math.MaxUint32 {
		remaining = math.MaxUint32
	}
	body := currentStatChangePotionEffectItemInfoBody(uint32(effect.ItemID), uint32(remaining))
	s.logPacketEvent("game-aligned-effect-item-info-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"item_id", effect.ItemID,
		"remaining_seconds", remaining,
		"msg_id", currentLoadEffectItemInfoMsgID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "restore_only_path_relogin_recreates_the_client_side_effect_item_timer")
	return s.sendGameUpperRawClass(session, currentLoadEffectItemInfoMsgID, body, 0)
}

// clearCurrentStatChangePotionBuffs 发 CHARACTER_REMOVE_BUFF，把这一族可能残留的
// buff id 一次清掉（id 列表直接来自 PVF 的 buff 定义表：带
// `epic_rarity_item_drop_rate` 效果的那些）。
func (s *Service) clearCurrentStatChangePotionBuffs(session *gameSession, source string) error {
	buffIDs := s.currentStatChangePotionBuffIDs()
	if len(buffIDs) == 0 {
		return nil
	}
	body := buildCurrentCharacterRemoveBuffBody(buffIDs)
	if len(body) == 0 {
		return nil
	}
	s.logPacketEvent("game-character-remove-buff-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"buff_count", int(body[0]),
		"msg_id", currentCharacterRemoveBuffMsgID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "expired_window_clears_stale_buff_icons")
	if err := s.sendGameUpperRawClass(session, currentCharacterRemoveBuffMsgID, body, 0); err != nil {
		return err
	}
	// 参考实现的 ClearAsync 是"REMOVE_BUFF → BUFF_DUNGEON(空)"：空列表把本地激活标志清掉。
	clearBody := buildCurrentCharacterBuffDungeonBody(nil)
	s.logPacketEvent("game-character-buff-dungeon-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"active_count", 0,
		"msg_id", currentCharacterBuffDungeonMsgID,
		"classification", 0,
		"body_len", len(clearBody),
		"sequence", "class0_op489_empty_list_clears_local_active_flags")
	return s.sendGameUpperRawClass(session, currentCharacterBuffDungeonMsgID, clearBody, 0)
}

// currentStatChangePotionBuffIDs 列出这一族药剂**可能发出去的**全部 buff id
// （= etc/variousbuffstackablelist.etc 里出现过的所有 buff，见
// tables.statChangePotionGrantableBuffIDs）。
//
// 覆盖全部五件赛丽亚药剂：掉落率 1090-1092 / 强化成功率 1097-1099 / 城镇移速 1100-1102 /
// 通关经验 1103-1105 / 进图属性 1106-1108，以及史诗Buff药剂、小灯笼、强化秘药那一批。
// 用在"一个生效窗口都没有"时的一次性清除：清一个客户端本来就没有的 buff 是无害的，
// 漏清才会留下一个永远不掉的图标。
func (s *Service) currentStatChangePotionBuffIDs() []int32 {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return nil
	}
	tables, ok := catalog.currentVariousBuffTables()
	if !ok {
		return nil
	}
	return tables.statChangePotionGrantableBuffIDs()
}

func (s *Service) sendCurrentCharacterAddBuff(session *gameSession, buffID int32, source string) error {
	body := buildCurrentCharacterAddBuffBody(buffID)
	s.logPacketEvent("game-character-add-buff-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"buff_id", buffID,
		"msg_id", currentCharacterAddBuffMsgID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "class0_op487_character_add_buff_creates_the_entry")
	if err := s.sendGameUpperRawClass(session, currentCharacterAddBuffMsgID, body, 0); err != nil {
		return err
	}
	// 第二发：把它置为**激活**。参考实现的 dungeon buff 流程永远两发一起发，
	// 注释写明 0x01EA 才是"toggles existing entries active"，且列表必须是**完整**的
	// 激活集合（客户端先清空本地标志再按列表置位）。
	activateBody := buildCurrentCharacterBuffDungeonBody([]int32{buffID})
	s.logPacketEvent("game-character-buff-dungeon-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"buff_id", buffID,
		"active_count", 1,
		"msg_id", currentCharacterBuffDungeonMsgID,
		"classification", 0,
		"body_len", len(activateBody),
		"sequence", "class0_op489_character_buff_dungeon_activates_the_entry",
		"evidence", currentCharacterAddBuffEvidence)
	return s.sendGameUpperRawClass(session, currentCharacterBuffDungeonMsgID, activateBody, 0)
}

// currentStatChangePotionEffectItemInfoBody 与 buff 一起发的倒计时记录（class0/op174）。
//
// 参考实现的 ItemStates(Effect) 与 buff 是**两套**：前者让客户端显示
// "[道具]的效果时间还剩N"，后者才是 buff 栏图标。两发都要有（现场 2026-10-03：
// 只有 op174 时服主看到的正是那行倒计时文字）。
func currentStatChangePotionEffectItemInfoBody(itemID, remainingSeconds uint32) []byte {
	return buildCurrentEffectItemInfoBody(itemID, remainingSeconds)
}

// sendCurrentCharacterAddBuffOnly 只发 class0/op487（建/更新 buff 条目），不发激活列表。
//
// 恢复路径要一次恢复多个 family，激活列表必须按**完整集合**发一次（op489 的语义是
// "先清空本地激活标志、再按列表置位"），所以这里把"建条目"和"置激活"拆开。
func (s *Service) sendCurrentCharacterAddBuffOnly(
	session *gameSession,
	buffID int32,
	source string,
) error {
	if buffID <= 0 {
		return nil
	}
	body := buildCurrentCharacterAddBuffBody(buffID)
	s.logPacketEvent("game-character-add-buff-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"buff_id", buffID,
		"msg_id", currentCharacterAddBuffMsgID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "restore_only_builds_the_entry_activation_follows_as_one_list")
	return s.sendGameUpperRawClass(session, currentCharacterAddBuffMsgID, body, 0)
}
