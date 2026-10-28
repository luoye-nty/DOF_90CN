package dnfbridge

// activity_hooks.go 把三类审计记录接到玩法主路径上，并暴露 GM 查询接口。
//
// 记录器本身（落盘/轮转/检索）在 activity_record.go；这里只负责
// "在正确的时机、用尽量全的字段" 调用它。
//
// 三条硬约束（记录是旁路，不能反过来影响玩法）：
//  1. recordActivity 可从任意协程调用，永不 panic、永不阻塞在主路径上；
//  2. 记录字段取不到就留空，绝不为凑字段去改仓储/阻塞协议流程；
//  3. 角色名做了会话级缓存——聊天很频繁，不能每条消息都查一次角色表。

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfauction "longheng.io/server/internal/modules/dnf/auction"
)

// recordActivity 追加一条审计记录。记录器未启用（目录不可写/未启动）时直接空转。
func (s *Service) recordActivity(kind string, fields map[string]any) {
	if s == nil || fields == nil {
		return
	}
	recorder := s.records.Load()
	if recorder == nil {
		return
	}
	recorder.record(kind, fields, s.logger)
}

// activitySessionFields 组装"这个会话是谁"的公共字段。
// 三个类别都要这几项，集中一处避免各埋点写法不一致。
//
// 注意这里**不能**用 s.accountIDForSession：那个函数在开启启动器鉴权且会话未绑定
// 账号时会写日志并主动断开连接。审计埋点是旁路，绝不能因为记录一条聊天就把玩家
// 踢下线。这里只读会话上已经绑定好的账号，读不到就留空。
func (s *Service) activitySessionFields(session *gameSession) map[string]any {
	fields := map[string]any{}
	if session == nil {
		return fields
	}
	if session.selectedCharacterID != 0 {
		fields["character_id"] = strconv.Itoa(int(session.selectedCharacterID))
	}
	if name := s.activityCharacterName(session); name != "" {
		fields["character_name"] = name
	}
	if accountID := strings.TrimSpace(session.accountID); accountID != "" {
		fields["account_id"] = accountID
	}
	if session.channel.ID != 0 {
		fields["channel_id"] = session.channel.ID
	}
	return fields
}

// activityCharacterName 取当前选中角色的名字，带会话级缓存。
//
// 为什么必须缓存：对话记录挂在每一次发言上，而角色名在 gameSession 里没有常驻字段，
// 只能查一次角色表。每条消息查一次库会给聊天加上一次 DB 往返，代价与收益不成比例；
// 角色名在同一次登录期间不会变，缓存到 selectedCharacterID 变化为止即可。
func (s *Service) activityCharacterName(session *gameSession) string {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return ""
	}
	session.activityNameMu.Lock()
	if session.activityNameCharacterID == session.selectedCharacterID && session.activityName != "" {
		name := session.activityName
		session.activityNameMu.Unlock()
		return name
	}
	session.activityNameMu.Unlock()

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	record, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return ""
	}
	session.activityNameMu.Lock()
	session.activityNameCharacterID = session.selectedCharacterID
	session.activityName = record.Name
	session.activityNameMu.Unlock()
	return record.Name
}

// ================= 对话记录 =================

// chatRecordText 把聊天原文解码成可读文本。
//
// 客户端聊天正文是 GBK 字节（与角色名、NPC 文本同一套编码），直接落盘会得到乱码，
// 查记录的人就看不懂了。解不出来时原样保留字节转成的字符串：宁可留一段乱码，
// 也不要丢掉玩家到底发了什么这个事实。
func chatRecordText(message []byte) string {
	if len(message) == 0 {
		return ""
	}
	ascii := true
	for _, value := range message {
		if value >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return string(message)
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(message)
	if err != nil {
		return string(message)
	}
	return string(decoded)
}

// recordChat 记一条对话。result 传 "ok" / "rejected"，rejected 时 reason 必填。
//
// message 收原始字节（客户端发来的 GBK），解码在这里做，调用点不必关心编码。
// 字段刻意保留完整原文——运营/管理端查"谁在喊什么"是这类记录的主要用途，
// 截断会让它失去意义。文件轮转与保留期由记录器控制，不在这里做二次裁剪。
func (s *Service) recordChat(
	session *gameSession,
	mode byte,
	modeName string,
	message []byte,
	recipients int,
	itemLinks int,
	result string,
	reason string,
	target map[string]any,
) {
	if s == nil {
		return
	}
	text := chatRecordText(message)
	fields := s.activitySessionFields(session)
	fields["mode"] = mode
	fields["mode_name"] = modeName
	fields["message"] = text
	fields["message_len"] = len([]rune(text))
	fields["message_bytes"] = len(message)
	if result != "ok" && len(message) > 0 {
		// 被拒绝的消息正文也留一份十六进制，便于核对解码是否正确。
		fields["message_hex"] = hex.EncodeToString(message)
	}
	fields["recipients"] = recipients
	fields["item_links"] = itemLinks
	fields["result"] = result
	if reason != "" {
		fields["reason"] = reason
	}
	for key, value := range target {
		fields[key] = value
	}
	s.recordActivity(recordKindChat, fields)
}

// currentTownChatModeName 聊天模式的中文名（记录里给人看，比数字好读）。
func currentTownChatModeName(mode byte) string {
	switch mode {
	case currentTownChatWhisperMode:
		return "私聊"
	case currentTownChatOneToOneMode:
		return "1:1会话"
	case currentTownChatPeerMenuMode:
		return "好友菜单消息"
	case currentTownChatMegaphoneMode:
		return "服务器喇叭"
	case currentTownChatChannelMegaphoneMode:
		return "频道喇叭"
	case currentTownChatAreaMode:
		return "区域"
	case currentTownChatArenaMode:
		return "决斗场"
	case currentTownChatPartyMode:
		return "队伍"
	case currentTownChatGuildMode, currentTownChatGuildLegacyMode:
		return "公会"
	case currentTownChatRaidMode:
		return "攻坚队"
	default:
		return fmt.Sprintf("未知模式(%d)", mode)
	}
}

// ================= 点券消费记录 =================

// recordCeraSpend 记一次点券消费。
//
// 余额前后值能拿到就填：运营最常问的是"这条消费之后他还剩多少"，
// 只写扣了多少没法回答这个问题。
func (s *Service) recordCeraSpend(session *gameSession, fields map[string]any) {
	if s == nil {
		return
	}
	merged := s.activitySessionFields(session)
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordKindCera, merged)
}

// recordAlignedCeraSpend 记一条由 aligned 命令模块上报的点券消费。
//
// 目前只有金库创建/升级走这条路（模块自己不经过商城结账）。金额为 0 表示
// 这条命令没有点券消费，直接跳过——不能让"每次 op 都写一条 0 元消费"。
func (s *Service) recordAlignedCeraSpend(session *gameSession, result alignedcmd.Result) {
	if s == nil || result.CeraSpent <= 0 {
		return
	}
	fields := map[string]any{
		"result":          "ok",
		"source":          "aligned_command",
		"operation":       result.Operation,
		"payment_mode":    "点券",
		"payment_name":    "点券",
		"cera_spent":      result.CeraSpent,
		"cera_after":      result.CeraAfter,
		"cera_before":     result.CeraAfter + result.CeraSpent,
		"cera_spent_note": result.CeraSpentNote,
	}
	s.recordCeraSpend(session, fields)
}

// recordGoldConsignCeraSpend 记一条"金币寄售买入"的点券消费。
//
// 为什么单独一个函数而不是复用 recordAlignedCeraSpend：金币寄售走的是拍卖行
// 玩法模块（access_auction 的 op185/op334），**不经过 alignedcmd**，
// 所以 alignedcmd.Result.CeraSpent 永远是 0，那条路覆盖不到这里。
//
// 字段口径与商城购买对齐（result / source / operation / payment_* / cera_*），
// 运营在「记录查询 → 点券消费记录」里能直接把两类消费放在一起看：
//
//	· source="gold_consign" 用来区分来源（商城成功那条没有 source；
//	  aligned 那条是 "aligned_command"）；
//	· payment_mode 复用商城的"点券"档（0），让 payment_mode 在所有点券记录里同值可比。
//
// 调用方必须保证 charged=true（真的扣了钱）才调用本函数 —— 重放同一个 op185 时
// 钱包不会再被扣一次，重复记账会让运营看到两条一模一样的消费。
func (s *Service) recordGoldConsignCeraSpend(
	session *gameSession,
	accountID, buyerID string,
	listing dnfauction.GoldConsignListing,
	ceraBefore, ceraAfter int64,
) {
	if s == nil {
		return
	}
	fields := map[string]any{
		"result":       "ok",
		"source":       "gold_consign",
		"operation":    "buy_gold_consign",
		"payment_mode": currentCeraShopPaymentModeCera,
		"payment_name": "点券",
		// 实扣点券 = 寄售单标价。金币寄售没有"券抵扣"，标价就是实扣。
		"cera_spent":  listing.PriceCera,
		"cera_before": ceraBefore,
		"cera_after":  ceraAfter,
		// 概要列直接显示这句，运营一眼能看出买的是哪一档金币。
		"cera_spent_note": "金币寄售：" + goldConsignAmountText(listing.GoldAmount) + "金币",
		// 显式补会话字段：activitySessionFields 只读 session.accountID /
		// selectedCharacterID，而这里用的是 accountIDForSession 解析出来的值，
		// 两者不一定同源（账号没绑定时前者可能就是空的）。记录里必须有人。
		"account_id":   strings.TrimSpace(accountID),
		"character_id": strings.TrimSpace(buyerID),
		// 寄售单与卖家侧信息（卖家侧的收入是代币券，不进本类别）。
		"listing_id":    listing.ID,
		"gold_item_id":  listing.GoldItemID,
		"gold_amount":   listing.GoldAmount,
		"seller_id":     listing.SellerID,
		"seller_name":   listing.SellerName,
		"fee_percent":   listing.FeePercent,
		"proceeds_cera": listing.ProceedsCera,
	}
	s.recordCeraSpend(session, fields)
}

// goldConsignAmountText 把金币数量渲染成人读的形式（1_000_000 → "100万"）。
//
// 金币档位最大到 2 亿，直接写原始数字在概要列里很难一眼分辨量级。
func goldConsignAmountText(amount int64) string {
	switch {
	case amount <= 0:
		return "0"
	case amount%100_000_000 == 0:
		return strconv.FormatInt(amount/100_000_000, 10) + "亿"
	case amount%10_000 == 0:
		return strconv.FormatInt(amount/10_000, 10) + "万"
	default:
		return strconv.FormatInt(amount, 10)
	}
}

// ================= 金币寄售记录 =================

// recordGoldConsign 记一条金币寄售事件（上架 / 下架 / 买家买入 / 成交收款 / 流拍退回）。
//
// 形状与 recordDungeon 一致：先铺会话身份字段，再写 event / event_name，
// 最后由调用方补齐本事件自己的字段。result 缺省为 "ok"，只有"明确被拒绝"的
// 那几条才显式传 rejected + reason —— 记录里不出现"没有 result"的行：
// 少一个字段的拒绝行与成功行长得一模一样，事后没人分得清。
//
// 与「点券消费记录」的分工：那边记的是**钱**（买家实扣点券，字段口径与商城
// 购买对齐，方便两类消费同表对比），这里记的是**一单寄售的完整生命周期**
// （谁挂的、卖给了谁、手续费抽了多少、卖家实得多少、有没有真的投递出去）。
// 同一笔买入因此在两个类别里各有一行，这不是重复记账，而是两个检索口径。
func (s *Service) recordGoldConsign(session *gameSession, event string, fields map[string]any) {
	if s == nil {
		return
	}
	merged := s.activitySessionFields(session)
	merged["event"] = event
	merged["event_name"] = goldConsignEventName(event)
	merged["result"] = "ok"
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordKindGoldConsign, merged)
}

// goldConsignEventName 金币寄售事件的中文名。
func goldConsignEventName(event string) string {
	switch event {
	case "list":
		return "上架"
	case "cancel":
		return "下架"
	case "buy":
		return "买家买入"
	case "sale_delivered":
		return "成交收款"
	case "expired":
		return "流拍退回"
	default:
		return event
	}
}

// ================= 拍卖行记录 =================

// recordAuction 记一条拍卖行事件
// （上架 / 下架 / 竞拍出价 / 一口价成交 / 竞拍成交 / 卖家领取收益）。
//
// 与 recordGoldConsign 同形，理由同上：拍卖行与金币寄售共用 op183/184/185，
// 但一个记的是物品竞拍（押金/竞价/一口价/手续费金币），另一个记的是金币档位
// （点券标价/代币券结算），字段集合不重叠，所以分成两个文件、两个类别。
func (s *Service) recordAuction(session *gameSession, event string, fields map[string]any) {
	if s == nil {
		return
	}
	merged := s.activitySessionFields(session)
	merged["event"] = event
	merged["event_name"] = auctionEventName(event)
	merged["result"] = "ok"
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordKindAuction, merged)
}

// auctionEventName 拍卖行事件的中文名。
func auctionEventName(event string) string {
	switch event {
	case "list":
		return "上架"
	case "cancel":
		return "下架"
	case "bid":
		return "竞拍出价"
	case "buyout":
		return "一口价成交"
	case "won":
		return "竞拍成交"
	case "payout":
		return "卖家领取收益"
	default:
		return event
	}
}

// ================= 场景加载诊断记录 =================

// recordScene 记一条场景加载/引导包（scene bootstrap tail）诊断事件。
//
// 与 recordGoldConsign / recordAuction 同形：先铺会话身份字段，再写
// event / event_name，最后由调用方补齐本事件自己的字段。
//
// 为什么要有这个包装、而不是继续用 recordRaidEvent：这批诊断事件
// （scene_tail_deferred 一族）与团本毫无关系，却被写进了 raid 类别，于是
//  1. 只要有人在选人选场景/进图，每秒就冒出好几条，真正的团本事件被淹得翻不到；
//  2. 记录器每类只保留最近 recordRetentionPerKind 个文件，这批高频写入会飞快
//     把 raid-*.jsonl 顶到上限，把真正的团本记录连旧文件一起挤出保留窗口。
//
// 所以它们必须有自己的类别（recordKindScene），而不能复用团本的写入路径。
//
// 关于会话字段：这里额外补 conn_id / character_generation / session_online 三项，
// 与 recordRaidEvent 保持一致——搬家前这些字段就在 scene_tail_deferred 记录里，
// 少了它们等于排障时丢信息（尤其 session_online 是判断"玩家还在不在线"的第一手依据）。
func (s *Service) recordScene(session *gameSession, event string, fields map[string]any) {
	if s == nil {
		return
	}
	merged := s.activitySessionFields(session)
	if session != nil {
		merged["conn_id"] = session.connID
		merged["character_generation"] = session.characterGeneration
		merged["session_online"] = session.conn != nil
	}
	merged["event"] = event
	merged["event_name"] = sceneEventName(event)
	merged["result"] = "ok"
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordKindScene, merged)
}

// sceneEventName 场景加载诊断事件的中文名。
//
// 名字与 raidEventName 里的同名分支刻意保持一致：同一个事件只是换了文件，
// 概要列显示的文字不该跟着变，否则运营会以为出现了新事件。
func sceneEventName(event string) string {
	switch event {
	case "scene_tail_deferred":
		// "被推迟"与"最终发出"共用同一个事件名，靠 result（deferred / sent）区分，
		// 这个口径搬家前就是这样，保持不变。
		return "场景尾巴包被推迟"
	default:
		return event
	}
}

// activityRejectionFields 拒绝记录的公共字段：result=rejected + reason。
//
// 统一一处是为了让"被拒绝"在全部记录类别里永远是同一对键值：运营按 result
// 过滤就能捞出所有失败尝试，reason 再回答是哪一种拒绝（余额不足 / 价格不符 /
// 已被买走……）。调用方可以继续往返回的 map 里补字段。
func activityRejectionFields(reason string) map[string]any {
	return map[string]any{"result": "rejected", "reason": reason}
}

// activityIdentityOverride 显式覆盖记录里的账号/角色标识，且一律写成字符串。
//
// 两条都是硬要求：
//  1. **显式**：activitySessionFields 读的是 session.accountID / selectedCharacterID，
//     而拍卖行这几条路径用的是 accountIDForSession / selectedCharacterIDString
//     解析出来的值，两者不一定同源（账号没绑定时前者可能就是空的）；
//  2. **字符串**：运维工具只按字符串型读 account_id / character_id
//     （handlers_records.go 的 recordAccountIDs），数字型会被静默忽略，
//     账号名翻译随即失效 —— 记录里就只剩一个没有主人的数字。
func activityIdentityOverride(fields map[string]any, accountID, characterID string) {
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		fields["account_id"] = accountID
	}
	if characterID = strings.TrimSpace(characterID); characterID != "" {
		fields["character_id"] = characterID
	}
}

// ================= 副本记录 =================

// recordDungeon 记一条副本事件（enter / clear / exit）。
func (s *Service) recordDungeon(session *gameSession, event string, fields map[string]any) {
	if s == nil {
		return
	}
	merged := s.activitySessionFields(session)
	merged["event"] = event
	merged["event_name"] = dungeonEventName(event)
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordKindDungeon, merged)
}

// dungeonEventName 副本事件的中文名。
func dungeonEventName(event string) string {
	switch event {
	case "enter":
		return "进入副本"
	case "clear":
		return "通关"
	case "exit":
		return "离开副本"
	default:
		return event
	}
}

// dungeonRuntimeFields 组装一条副本记录里"这次刷的是哪个本、什么状态"的公共字段。
//
// 调用时机分三种（进入/通关/离开），三者能拿到的字段不完全一样，所以缺的就留空，
// 不为了凑字段去补一次仓储查询——副本埋点位于玩法主路径上，多查一次库就是给
// 玩家多一次卡顿。
func dungeonRuntimeFields(runtime *runtimeDungeonState) map[string]any {
	fields := map[string]any{}
	if runtime == nil {
		return fields
	}
	if runtime.Dungeon.ID != 0 {
		fields["dungeon_id"] = runtime.Dungeon.ID
	}
	if name := strings.TrimSpace(runtime.Dungeon.Metadata.Name); name != "" {
		fields["dungeon_name"] = name
	}
	if path := strings.TrimSpace(runtime.Dungeon.Path); path != "" {
		fields["dungeon_path"] = path
	}
	if runtime.clientDungeonID != 0 && runtime.clientDungeonID != runtime.Dungeon.ID {
		fields["client_dungeon_id"] = runtime.clientDungeonID
	}
	fields["maze_index"] = runtime.MazeIndex
	fields["difficulty"] = int(runtime.Request.Difficulty)
	fields["request_dungeon_id"] = runtime.Request.DungeonID
	fields["selection_mode"] = runtime.Request.SelectionMode
	fields["entry_option"] = runtime.Request.EntryOption
	fields["party_state"] = runtime.Request.PartyState
	if runtime.lifecycleToken != 0 {
		fields["lifecycle_token"] = runtime.lifecycleToken
	}
	if runtime.Character.CharacterID != "" {
		if level := runtime.Character.Level; level != 0 {
			fields["character_level"] = level
		}
	}
	fields["practice"] = currentDungeonRuntimeIsPractice(runtime)
	fields["tutorial"] = isPVFTutorialDungeon(runtime)
	if !runtime.startedAt.IsZero() {
		fields["started_at"] = runtime.startedAt.UTC().Format(time.RFC3339)
		fields["elapsed_seconds"] = int64(time.Since(runtime.startedAt).Seconds())
	}
	if runtime.clearKillNormal != 0 || runtime.clearKillChampion != 0 || runtime.clearKillBoss != 0 {
		fields["killed_normal"] = runtime.clearKillNormal
		fields["killed_champion"] = runtime.clearKillChampion
		fields["killed_boss"] = runtime.clearKillBoss
	}
	// 技能消耗的无色小晶块（判断外挂的关键指标之一）。
	// 只在真发生过时输出，避免每条进入记录都挂一个 0；晶体契约的自动扣减不计入。
	if runtime.colorlessSpent != 0 {
		fields["colorless_spent"] = runtime.colorlessSpent
	}
	return fields
}

// recordDungeonEnter 记一条"进入副本"。
func (s *Service) recordDungeonEnter(session *gameSession, runtime *runtimeDungeonState) {
	if s == nil || runtime == nil {
		return
	}
	s.recordDungeon(session, "enter", dungeonRuntimeFields(runtime))
}

// recordDungeonClear 记一条"通关"。
//
// 只记一次由调用方保证：函数内部不持任何去重状态，落到运行时字段上的
// "结算前置屏障"（tutorialCompletionPersisted / clear-map 阶段 A）才是唯一判据。
func (s *Service) recordDungeonClear(session *gameSession, runtime *runtimeDungeonState, source string) {
	if s == nil || runtime == nil {
		return
	}
	fields := dungeonRuntimeFields(runtime)
	fields["clear_source"] = source
	if runtime.Session != nil {
		snapshot := runtime.Session.Snapshot()
		fields["run_status"] = string(snapshot.Run.Status)
		fields["room"] = snapshot.Run.Current.String()
		if snapshot.Scene.Map.Map.ID != 0 {
			fields["map_id"] = snapshot.Scene.Map.Map.ID
		}
		fields["defeated_actor_count"] = len(snapshot.Scene.DefeatedObjects)
	}
	if runtime.settlementMonsterExperienceTotal != 0 {
		fields["monster_experience_total"] = runtime.settlementMonsterExperienceTotal
	}
	s.recordDungeon(session, "clear", fields)
}

// recordCompletedReturnOutcome writes the outcome of a completed-dungeon 返回城镇
// into the GM dungeon record file (runtime/logs/records/dungeon-*.jsonl).
//
// It deliberately avoids the two obvious channels:
//
//   - logGameEvent goes to packet_log.txt, which DROPS LINES under load. Measured
//     on the live file 2026-09-25T07:00Z for conn game-003214: 99 of 855 adjacent
//     pkt_seq pairs were discontinuous, i.e. ~25% of that connection's lines never
//     landed. Five live 返回城镇 hangs (2026-09-24T17:07:49Z, 2026-09-25T05:25:22Z,
//     05:25:23Z, 06:55:10Z, 07:10:01Z) were unattributable for exactly that reason:
//     every guard on the completed-return path is either silent or packet-log-only,
//     so "the marker is absent" proved nothing.
//   - logWarn goes to the slog logger, but on this deployment the server is started
//     by the one-click launcher and its stdout is not captured at all
//     (runtime/logs contains no server-*.stdout.log), so a warning there is
//     invisible.
//
// The activity recorder's queue blocks rather than drops in normal operation
// (activity_record.go: the second select waits for room and only gives up while the
// recorder is shutting down), so this is the one channel that survives both.
func (s *Service) recordCompletedReturnOutcome(session *gameSession, outcome string, fields ...any) {
	if s == nil || session == nil {
		return
	}
	merged := map[string]any{"outcome": outcome}
	for index := 0; index+1 < len(fields); index += 2 {
		key, ok := fields[index].(string)
		if !ok {
			continue
		}
		merged[key] = fields[index+1]
	}
	s.recordDungeon(session, "return-outcome", merged)
}

// recordDungeonExit 记一条"离开副本"。
//
// reason 是运行时自己的生命周期结束原因（返回城镇、死亡回城、被新一次选本替换……），
// 排障时它比"玩家走了"有用得多。
func (s *Service) recordDungeonExit(session *gameSession, runtime *runtimeDungeonState, reason string) {
	if s == nil || runtime == nil {
		return
	}
	fields := dungeonRuntimeFields(runtime)
	fields["exit_reason"] = strings.TrimSpace(reason)
	fields["cleared"] = runtime.ordinaryFinalRoomClearAccepted || runtime.tutorialFinalRoomClearAccepted ||
		runtime.bossDieCheckAccepted || runtime.settlementEntrySent
	fields["settlement_entry_sent"] = runtime.settlementEntrySent
	fields["settlement_result_sent"] = runtime.settlementResultNoticeSent
	fields["town_return_sent"] = runtime.townReturnOp24Sent
	s.recordDungeon(session, "exit", fields)
}

// ================= GM 查询接口 =================

// GMRecordQuery 记录查询条件（GM 接口入参）。
//
// Offset 与 Limit 一起构成分页：Limit 是单次返回上限（上限 2000，一次请求不可能把
// 全部记录吐出来），Offset 是跳过最新的多少条命中。要"看全部"就翻页，直到
// 返回的 Limited=false。
type GMRecordQuery struct {
	Kind          string
	Limit         int
	Offset        int
	Keyword       string
	CharacterID   string
	CharacterName string
	AccountID     string
	Since         time.Time
	Until         time.Time
}

// GMRecordPage 记录查询结果。
type GMRecordPage struct {
	Kind    string           `json:"kind"`
	Title   string           `json:"title"`
	Dir     string           `json:"dir"`
	Records []map[string]any `json:"records"`
	Count   int              `json:"count"`
	Scanned int              `json:"scanned"`
	// Offset 是本次返回的起始位置（跳过的最新命中条数），调用方翻页时回填。
	Offset int `json:"offset"`
	// Limited=true 表示"还有更多记录"（本次是装满一页返回的），可以继续用
	// Offset+Count 作为下一页起点再查。
	Limited   bool `json:"limited"`
	Retention int  `json:"retention_files"`
}

// GMRecordKindInfo 一个类别的元信息。
type GMRecordKindInfo struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// GMRecordMeta 记录系统元信息（目录、类别、文件列表）。
type GMRecordMeta struct {
	Enabled bool               `json:"enabled"`
	Dir     string             `json:"dir"`
	Kinds   []GMRecordKindInfo `json:"kinds"`
	Files   []recordFileInfo   `json:"files"`
}

// GMRecordMeta 返回记录目录与文件清单，供管理端展示"记录落在哪、有多大"。
func (s *Service) GMRecordMeta() GMRecordMeta {
	meta := GMRecordMeta{Enabled: false, Kinds: make([]GMRecordKindInfo, 0, len(recordKinds))}
	for _, kind := range recordKinds {
		meta.Kinds = append(meta.Kinds, GMRecordKindInfo{Kind: kind, Title: recordKindTitles[kind]})
	}
	recorder := s.records.Load()
	if recorder == nil {
		meta.Dir = s.options.recordDir
		return meta
	}
	meta.Enabled = true
	meta.Dir = recorder.Dir()
	meta.Files = recorder.listRecordFiles()
	return meta
}

// GMQueryRecords 检索记录。kind 必须是三类之一，否则返回 ErrGMInvalidRequest。
func (s *Service) GMQueryRecords(query GMRecordQuery) (GMRecordPage, error) {
	query.Kind = strings.ToLower(strings.TrimSpace(query.Kind))
	if !isRecordKind(query.Kind) {
		return GMRecordPage{}, fmt.Errorf("%w: 未知的记录类别 %q（可选：%s）",
			ErrGMInvalidRequest, query.Kind, strings.Join(recordKinds, " / "))
	}
	page := GMRecordPage{
		Kind:      query.Kind,
		Title:     recordKindTitles[query.Kind],
		Retention: recordRetentionPerKind,
	}
	recorder := s.records.Load()
	if recorder == nil {
		// 记录器没起来（目录不可写/服务未启动）不是错误：返回空页，让调用方看到 dir 提示。
		page.Dir = s.options.recordDir
		page.Records = []map[string]any{}
		return page, nil
	}
	page.Dir = recorder.Dir()
	limit := query.Limit
	if limit <= 0 {
		limit = 200
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	records, scanned, err := recorder.queryRecords(recordQuery{
		Kind:          query.Kind,
		Limit:         limit,
		Offset:        offset,
		Since:         query.Since,
		Until:         query.Until,
		CharacterID:   strings.TrimSpace(query.CharacterID),
		CharacterName: strings.TrimSpace(query.CharacterName),
		AccountID:     strings.TrimSpace(query.AccountID),
		Keyword:       strings.TrimSpace(query.Keyword),
	})
	if err != nil {
		return GMRecordPage{}, err
	}
	page.Records = records
	page.Count = len(records)
	page.Scanned = scanned
	page.Offset = offset
	// limited = "因为装满一页而停"，用来告诉调用方还有更多（可以带着新的 offset 继续翻页，
	// 或者缩小时间范围再查）。
	page.Limited = len(records) >= limit
	return page, nil
}
