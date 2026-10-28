package dnfbridge

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 卢克攻坚战一阶段的塔罗牌显示包：opcode 1181 (0x49D),
// LUKE_RAID_CARD_DISPLAY。客户端侧没有对应的上行请求包（名字表里含 LUKE 的
// 包全是 ENUM_NOTIPACKET_*），1181 只能由服务端推；逐字段逆向见
// tools\_rev_卢克塔罗牌_1181规格_20260929.md。
//
// 下发时机（官方系统说明，2017-06-22 国服开放卢克团队模式的更新公告）：
//
//	「塔罗卡牌在团队频道入场时可以看见1次，之后可以在攻坚战进行中的状况表上确认。」
//	「塔罗卡牌以卢克团队模式进行的当日6点为基准进行变更，所有频道的塔罗卡牌统一相同。」
//
// ⇒ 推送点 = 成员走进卢克攻坚队待机室的那一次，每个成员每次进入恰好一包，
// openWindowFlag = 1（自动弹窗）；牌面随后留在客户端本地，战况表自己重开窗口，
// 所以服务端**不重连补发、不周期重发**——这一点是有意的：那会变成刷屏源，
// 而且客户端没有任何字段能区分「补发」与「今天的牌变了」。
//
// 牌面本身官方未公开选牌规则（客户端不校验 P、不去重、没有「未翻开」哨兵，
// 见规格 §8.3），下面这套选法是**本实现的约定**，不是逆向结论。

// lukeCardSlotsPerPattern 是一个图案的卡位数 = `[CARD POS]` 的 (x,y) 对数，也就是
// 包体里每条记录的 K。出货 PVF 的 8 个图案全部是 7（规格 §5.2.1；客户端侧的上界
// 就是 `sar edx,3` 数出来的 `[CARD POS]` 对数，规格 §7.4）。它不是发出去的常量：
// K 实际从解析到的 `[CARD POS]` 数来，这里只做一致性守卫——形状与已逆向的包体
// 不一致时宁可不发（fail closed）。客户端按该对数遍历、并与包内 P 列表同步推进，
// 所以两者长度必须一致。
const lukeCardSlotsPerPattern = 7

// runtimeRaidLukeTarotSanctums 是一阶段三个圣所的发包顺序。顺序本身客户端不校验，
// 用 luke.etc [DEFAULT OBJECT COUNT] 的书写顺序（诞生/湮灭/扩散）。
var runtimeRaidLukeTarotSanctums = []uint32{
	lukeBirthSanctumMapID,
	lukeExtinctionSanctumMapID,
	lukeSpreadSanctumMapID,
}

// runtimeRaidLukeCardSetRecordCount 是 luke.etc `[CARD SET MAP INFO]` 的记录条数。
// 该段是扁平列表（出货 15 个数：0 0 0 3 3 3 6 6 6 9 9 9 12 12 12），客户端按每 3 个
// 数字 push 进 3 字节元素的向量，1181 的字段 1 正是拿这个条数做上界（规格 §1.1/§8.1.2）。
// 定义点与迷宫侧共用（runtimeRaidLukeTarotCardSetRecords）。
func runtimeRaidLukeCardSetRecordCount(cardSetMapInfo []int) int {
	return runtimeRaidLukeTarotCardSetRecords(cardSetMapInfo)
}

// runtimeRaidLukeTarotCardSetIndex 是「当天那一套牌」的下标（0..记录条数-1）。
//
// 它只依赖 tarot day：官方文案写明全频道统一，且客户端只把它当合法性字节用
// （越界则整包丢弃，之后再无引用，规格 §8.1.1），所以它既不带圣所也不带攻坚队。
// **它与 raid_luke_sanctum_maze.go 用的是同一个抽取器**：1181 里这个字节代表的那条
// [CARD SET MAP INFO] 记录，正是当天三个圣所走的迷宫组所在的那条 —— 一个概念只留
// 一个数字，免得日后有人以为"显示用的套"和"实际走的组"是两回事。
func runtimeRaidLukeTarotCardSetIndex(raidDay int, cardSetMapInfo []int) (int, int) {
	records := runtimeRaidLukeCardSetRecordCount(cardSetMapInfo)
	if records <= 0 {
		return 0, 0
	}
	return runtimeRaidLukeTarotCardSetRecord(raidDay, records), records
}

// runtimeRaidLukeTarotPatternIndex 选一个圣所今天启动哪一套防御体系（包体字段 B，
// 即 `[PATTERN] <地牢ID> <图案序号>` 的序号）。
//
// 官方文案：「卢克手下的怪物们以塔罗牌的形式展现，且分配在源生宫殿(1阶段)的各个
// 地下城中。随着塔罗牌分配的形态不同，各个地下城的防御体系启动。」⇒ B 就是
// 「该圣所今天启动哪套防御体系」，其 `[TOOLTIP TEXT]` 即那套系统的名字。
// 出货 PVF 里：5054 → 属性值弱化系统 / 防御体系；5055 → 元素防御系统(冰/光/火/暗)；
// 5056 → 超合金装甲系统 / 反对魔法系统。
//
// 本实现的约定：沿用既有的每日抽取器 runtimeRaidLukeTarotGroup(day, 地牢ID, n)，
// 即同一天同一圣所全服取同一套（与「所有频道统一」一致），换日即变。
// **本次不动它的选法**：「每天一套牌」怎么映射到各圣所各自的图案序号没有证据
// （各圣所 [PATTERN] 段数是 2/4/2，不是同一张表的三个分量），所以 B 保持按
// (day, 地牢ID) 抽；改的只是"走哪组迷宫"。
func runtimeRaidLukeTarotPatternIndex(raidDay int, dungeonID uint32, patterns []int) (int, bool) {
	if len(patterns) == 0 {
		return 0, false
	}
	return patterns[runtimeRaidLukeTarotGroup(raidDay, int(dungeonID), len(patterns))], true
}

// runtimeRaidLukeTarotCards 从一个圣所那份 22 张大阿卡纳里选 7 张互不重复的牌。
//
// **本实现的约定**（官方选牌规则未知，客户端也不校验、不去重）：
// 按 (tarot day, 圣所地牢 ID, 图案序号) 对每张大阿卡纳做一次确定性哈希，按键升序取前
// want 张。因此同一场攻坚队、同一天、同一圣所每次算出同一批牌（可复现、可对账），
// 换一天就换一批；不同圣所、不同图案也各自不同。哈希键里带卡号本身，所以天然不会重复。
func runtimeRaidLukeTarotCards(raidDay int, dungeonID uint32, pattern int, cardIDs []uint32, want int) []uint32 {
	if want <= 0 || len(cardIDs) < want {
		return nil
	}
	type rankedCard struct {
		score  uint32
		cardID uint32
	}
	ranked := make([]rankedCard, 0, len(cardIDs))
	for _, cardID := range cardIDs {
		hash := fnv.New32a()
		_, _ = fmt.Fprintf(hash, "luke-tarot-card:%d:%d:%d:%d", raidDay, dungeonID, pattern, cardID)
		ranked = append(ranked, rankedCard{score: hash.Sum32(), cardID: cardID})
	}
	sort.Slice(ranked, func(left, right int) bool {
		if ranked[left].score != ranked[right].score {
			return ranked[left].score < ranked[right].score
		}
		// 同分时按卡号定序，保证结果与卡表顺序无关（可复现）。
		return ranked[left].cardID < ranked[right].cardID
	})
	cards := make([]uint32, 0, want)
	for _, entry := range ranked[:want] {
		cards = append(cards, entry.cardID)
	}
	return cards
}

// lukeCardEmptySlot is what a slot carries when the day's mazes do not fill it.
//
// The client draws one arcanum per slot by looking the value up in its own
// `[CARD]` table and **silently draws nothing for a value that misses** (see
// raid.LukeCardSetConfig.HasCard). All 22 shipped keys are five-digit monster
// ids, so the inactive-id sentinel can never collide with one, and a short day
// therefore leaves a quietly empty slot instead of a wrong card. Zero would be
// the other natural marker, but BuildLukeCardDisplayBody rejects it as a shape
// error, so the empty slot has to be a real u32 that simply is not a card.
const lukeCardEmptySlot uint32 = math.MaxUint32

// runtimeRaidLukeTarotCardsFromMazeMonsters derives a sanctum's card slots from
// the monsters that sanctum's three mazes really spawn on the day's group.
//
// The tarot faces are not decorative: every `[CARD] <monsterID>` block in
// LukeCardSet.etc is one major arcanum whose tooltip names the Luke monster it
// belongs to (00 愚者_红色小丑 59171, 02 女祭司_量产型贝奇 59873, 10 命运之轮
// 56673, 16 塔_全金属机甲斗神 56063 ...), and the value the notification carries
// is that monster id. The client owns the mapping, so naming the monsters the
// run will actually fight is exactly what makes the board show the day's real
// faces.
//
// Order: the sanctum's mazes in maze order (groupStart, +1, +2), and inside one
// maze the monster order the document lists ([NAMED] then [BOSS], see
// LukeRaidMonsterListMaze). Monsters that are not [CARD] keys are skipped, and a
// monster already used keeps its first slot, so the seven slots stay distinct.
//
// ⚠ 未证实约定：「第 n 只怪填第 n 格」没有任何客户端证据 —— 客户端只是把 7 个
// 值逐个查表画出来，槽位与怪的对应关系官方没有公开。这里按最自然的顺序填充
// （迷宫顺序 → 迷宫内出场顺序），它的可验证后果只有一条：**牌面集合**必须是当天
// 真会刷的那几只。出货 PVF 里每个组只能推出 5 只带 [CARD] 的怪
// （3 个迷宫 = 1 组中 boss + 1 只 named + 1 个圣所 boss，第三迷宫与第一迷宫共用
// 中 boss），所以 7 格里固定有 2 格是 lukeCardEmptySlot。
//
// nil when the day yields no arcanum at all: 调用方必须**不发包**（失败关闭），
// 不能发一包 7 个空槽的牌面。A non-nil result always has exactly want entries.
func runtimeRaidLukeTarotCardsFromMazeMonsters(
	dungeonID uint32,
	groupStart int,
	monsters *raid.LukeRaidMonsterListConfig,
	cardSet *raid.LukeCardSetConfig,
	want int,
) []uint32 {
	if want <= 0 || monsters == nil || cardSet == nil || groupStart < 0 {
		return nil
	}
	if _, ok := monsters.Dungeon(dungeonID); !ok {
		return nil
	}
	cards := make([]uint32, 0, want)
	used := make(map[uint32]bool, want)
	for maze := groupStart; maze < groupStart+lukeSanctumClearsPerGroup && len(cards) < want; maze++ {
		monsterIDs, ok := monsters.MazeMonsters(dungeonID, maze)
		if !ok {
			// This sanctum does not declare that maze; the group is only ever
			// accepted when all three fit, so this is a table that changed
			// shape. Stop rather than read a neighbour's monsters.
			break
		}
		for _, monsterID := range monsterIDs {
			if len(cards) >= want {
				break
			}
			if used[monsterID] || !cardSet.HasCard(monsterID) {
				continue
			}
			used[monsterID] = true
			cards = append(cards, monsterID)
		}
	}
	if len(cards) == 0 {
		return nil
	}
	for len(cards) < want {
		cards = append(cards, lukeCardEmptySlot)
	}
	return cards
}

// runtimeRaidLukeTarotGroupStart is the group of mazes a sanctum walks on one
// tarot day: the day's shared [CARD SET MAP INFO] record's own component, with
// the old candidate list as the fallback for a table whose shape does not
// address a whole group (same relationship runtimeRaidLukeSanctumMazeDrawnGroup
// has at dungeon entry). The fallback index is the tarot-day hash rather than a
// dice roll, so the card display stays deterministic within a day.
func runtimeRaidLukeTarotGroupStart(
	cardSetMapInfo []int,
	record int,
	dungeonID int,
	mazeCount int,
	raidDay int,
) (int, bool) {
	if mazeCount >= lukeSanctumClearsPerGroup {
		if start, ok := runtimeRaidLukeSanctumMazeRecordGroup(cardSetMapInfo, record, dungeonID, mazeCount); ok {
			return start, true
		}
	}
	groups := runtimeRaidLukeSanctumMazeGroups(cardSetMapInfo, mazeCount)
	if len(groups) == 0 {
		return 0, false
	}
	return groups[runtimeRaidLukeTarotCardSetRecord(raidDay, len(groups))], true
}

// runtimeRaidLukeTarotCardsForSanctum is one sanctum's seven slots for one tarot
// day: resolve the day's group for this sanctum, then read the monsters that
// group really spawns. nil means "this sanctum's face cannot be described
// today", which the caller turns into "do not send the packet at all".
func runtimeRaidLukeTarotCardsForSanctum(
	dungeonID uint32,
	cardSetMapInfo []int,
	record int,
	raidDay int,
	monsters *raid.LukeRaidMonsterListConfig,
	cardSet *raid.LukeCardSetConfig,
	want int,
) []uint32 {
	if want <= 0 || cardSet == nil || monsters == nil {
		return nil
	}
	mazeCount, ok := monsters.MazeCount(dungeonID)
	if !ok {
		return nil
	}
	groupStart, ok := runtimeRaidLukeTarotGroupStart(cardSetMapInfo, record, int(dungeonID), mazeCount, raidDay)
	if !ok {
		return nil
	}
	return runtimeRaidLukeTarotCardsFromMazeMonsters(dungeonID, groupStart, monsters, cardSet, want)
}

// runtimeRaidLukeCardDisplayBody 组出今天这一包 1181 的包体。
//
// ok=false 表示「今天这套牌描述不出来」，调用方必须**不发包**：配置缺失、
// 图案缺失、卡位对数不是 7、大阿卡纳不足 7 张，任何一种都不发半截包。
//
// cardSetMapInfo 是 luke.etc 的 `[CARD SET MAP INFO]`（它同时决定字段 1 的上界和
// 三个圣所今天走的迷宫组所在的那条记录，见 raid_luke_sanctum_maze.go）。
// monsters 是 luke.etc.buff 的 `[RAID MONSTER LIST]`，牌面由**当天这三个圣所
// 真会刷的怪**推出（runtimeRaidLukeTarotCardsFromMazeMonsters）。
// openWindow 直接写进包体末字节：只有"本 session 第一次落到团队频道"那一包为 true。
func runtimeRaidLukeCardDisplayBody(
	cardSetMapInfo []int,
	cardSet *raid.LukeCardSetConfig,
	monsters *raid.LukeRaidMonsterListConfig,
	raidDay int,
	openWindow bool,
) ([]byte, bool, error) {
	if cardSet == nil {
		return nil, false, nil
	}
	cardSetIndex, records := runtimeRaidLukeTarotCardSetIndex(raidDay, cardSetMapInfo)
	if records <= 0 {
		return nil, false, nil
	}
	display := raid.LukeCardDisplay{
		CardSetIndex: byte(cardSetIndex),
		CardSetCount: records,
		Sanctums:     make([]raid.LukeCardDisplaySanctum, 0, len(runtimeRaidLukeTarotSanctums)),
		OpenWindow:   openWindow,
	}
	for _, dungeonID := range runtimeRaidLukeTarotSanctums {
		patterns := cardSet.PatternIndices(dungeonID)
		patternIndex, ok := runtimeRaidLukeTarotPatternIndex(raidDay, dungeonID, patterns)
		if !ok {
			return nil, false, nil
		}
		pattern, found := cardSet.Pattern(dungeonID, patternIndex)
		if !found || len(pattern.CardPositions) != lukeCardSlotsPerPattern {
			return nil, false, nil
		}
		cards := runtimeRaidLukeTarotCardsForSanctum(
			dungeonID,
			cardSetMapInfo,
			cardSetIndex,
			raidDay,
			monsters,
			cardSet,
			len(pattern.CardPositions),
		)
		if cards == nil {
			// 牌面表在、却一张都推不出来 = 今天这组怪没有一张是 [CARD]，这属于
			// 「描述不出来」：失败关闭，不发半截包。
			return nil, false, nil
		}
		display.Sanctums = append(display.Sanctums, raid.LukeCardDisplaySanctum{
			DungeonID: dungeonID,
			Pattern:   patternIndex,
			Cards:     cards,
		})
	}
	body, err := raid.BuildLukeCardDisplayBody(display)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// runtimeRaidLukeCardDisplayPopupOwed reports whether this login session still
// owes the one pop-up the official behaviour grants
// ("每次登陆后，第一次进入卢克 RAID 频道时…自动展示一次塔罗牌"). The ledger is
// keyed by tarot day, because the packet itself does not depend on a raid.
// Read-only: the ledger is only spent by markRuntimeRaidLukeCardDisplayPopupSpent,
// and only after the packet actually went out with the flag set.
func runtimeRaidLukeCardDisplayPopupOwed(session *gameSession, tarotDay int) bool {
	if session == nil {
		return false
	}
	session.lukeCardDisplayMu.Lock()
	defer session.lukeCardDisplayMu.Unlock()
	return !session.lukeCardDisplayPoppedDays[tarotDay]
}

// markRuntimeRaidLukeCardDisplayPopupSpent spends this session's one pop-up for
// this tarot day. The lock is a leaf and is released before returning.
func markRuntimeRaidLukeCardDisplayPopupSpent(session *gameSession, tarotDay int) {
	if session == nil {
		return
	}
	session.lukeCardDisplayMu.Lock()
	if session.lukeCardDisplayPoppedDays == nil {
		session.lukeCardDisplayPoppedDays = make(map[int]bool, 2)
	}
	session.lukeCardDisplayPoppedDays[tarotDay] = true
	session.lukeCardDisplayMu.Unlock()
}

// publishRuntimeRaidLukeCardDisplay 给一名玩家发一包 1181（今天的塔罗牌面）。
// 它自己吞掉全部错误并只记日志：频道入场不能被一张牌面拖垮。
//
// 官方行为（服主 2026-09-29 口径）：
//
//	「玩家在每次登陆后，第一次进入卢克 RAID 频道时，离开赛利亚旅馆会自动展示
//	  一次塔罗牌。」
//
// 所以这个包**不依赖攻坚队**。它的数据全部来自 PVF 的**全局配置**，与任何
// runtimeRaidState 都无关：
//
//	当天那套牌 = runtimeRaidLukeTarotCardSetRecord(tarotDay, 记录条数)
//	              （全服统一，只按日期）
//	牌面       = 当天那条 [CARD SET MAP INFO] 记录 + LukeCardSet.etc 的 22 张大
//	              阿卡纳 + luke.etc.buff [RAID MONSTER LIST] 里当天那三个迷宫会刷的怪
//
// 前者来自 luke.etc（服务级缓存 runtimeRaidLukeConfig），后两者来自各自的
// PVF（runtimeRaidLukeCardSetConfig / runtimeRaidLukeRaidMonsterListConfig）。
// 这里**不再**读 state.LukeConfig —— 那份配置只有在团里才有，而官方规则说的是
// 进频道，不是进团（线上 2026-09-29T12:50:46Z 那条
// `..._skipped reason=luke_team_channel_entry_without_a_raid` 就是"没团"被跳过）。
//
// 账本是**每个登录会话、每个 tarot day 一次**（不是每个团一次）：
//   - 本次登录第一次进入卢克频道城镇 → open_window=1（弹窗）；
//   - 同一次登录里之后再进（换城镇回来、重连走同一条路、op36 进待机室、通关返回
//     待机室）→ 仍然发，但 open_window=0：数据保持新鲜，不再打扰；
//   - 重新登录 = 新 session = 空账本 → 再弹一次。这正是"每次登陆后…一次"。
//
// raidKey 的两种取值（三条真实入口共用同一份账本）：
//   - 非 0：调用点已知这个团 —— op36 走进攻坚队待机室（handleTownSetUserArea）、
//     通关后落回待机室（restoreCompletedRaidWaitingPresence）。必须确实是本人的
//     卢克团，安徒恩一个字都不改（单人卢克也算：见下）。
//   - 0：与团无关的入口 —— currentRaidTownTransition 落进卢克频道城镇。
//     **没团也发**：官方规则只要求"人在卢克频道"。
//
// 单人模式（TailFlag == 1）不再特殊对待：官方规则的门槛是"进卢克 RAID 频道"，
// 与团的编制无关；这条守卫生来是因为牌面过去取自团配置，现在不取了。
func (s *Service) publishRuntimeRaidLukeCardDisplay(session *gameSession, raidKey uint32, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := session.selectedCharacterID
	if raidKey != 0 {
		state, member := s.runtimeRaidForMember(characterID)
		if !member || state.RaidKey == 0 || state.RaidKey != raidKey {
			return
		}
		// 安徒恩不进这里：塔罗牌面只属于卢克。
		if state.RaidMode != runtimeRaidOwnerLuke {
			return
		}
	} else if !isCurrentLukeRaidChannel(session) {
		// 与团无关的入口必须**确实是卢克频道**才发，安徒恩频道一个字都不改。
		return
	}
	// luke.etc 的 [CARD SET MAP INFO]：全局配置，不是某个团的配置。
	lukeConfig, err := s.runtimeRaidLukeConfig()
	if err != nil {
		s.logPacketEvent("game-raid-luke-card-display-skipped",
			"char_id", characterID,
			"raid_key", raidKey,
			"msg_id", int(raid.LukeCardDisplayMsgID),
			"source", source,
			"reason", "luke_config_pvf_unavailable",
			"error", err)
		return
	}
	cardSet, err := s.runtimeRaidLukeCardSetConfig()
	if err != nil {
		s.logPacketEvent("game-raid-luke-card-display-skipped",
			"char_id", characterID,
			"raid_key", raidKey,
			"msg_id", int(raid.LukeCardDisplayMsgID),
			"source", source,
			"reason", "card_set_pvf_unavailable",
			"error", err)
		return
	}
	monsters, err := s.runtimeRaidLukeRaidMonsterListConfig()
	if err != nil {
		s.logPacketEvent("game-raid-luke-card-display-skipped",
			"char_id", characterID,
			"raid_key", raidKey,
			"msg_id", int(raid.LukeCardDisplayMsgID),
			"source", source,
			"reason", "raid_monster_list_pvf_unavailable",
			"error", err)
		return
	}
	raidDay := runtimeRaidLukeTarotDay(s.gameplayNow())
	openWindow := runtimeRaidLukeCardDisplayPopupOwed(session, raidDay)
	body, ok, err := runtimeRaidLukeCardDisplayBody(lukeConfig.CardSetMapInfo, cardSet, monsters, raidDay, openWindow)
	if err != nil || !ok || len(body) == 0 {
		s.logPacketEvent("game-raid-luke-card-display-skipped",
			"char_id", characterID,
			"raid_key", raidKey,
			"msg_id", int(raid.LukeCardDisplayMsgID),
			"tarot_day", raidDay,
			"source", source,
			"reason", "card_display_not_describable",
			"error", err)
		return
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.LukeCardDisplayMsgID, body, 0, true); err != nil {
		s.logPacketEvent("game-raid-luke-card-display-send-failed",
			"char_id", characterID,
			"raid_key", raidKey,
			"msg_id", int(raid.LukeCardDisplayMsgID),
			"tarot_day", raidDay,
			"body_len", len(body),
			"source", source,
			"error", err)
		return
	}
	// 只有真正出去了才花掉这一天这一次的弹窗额度：发失败的那一包不算"看见过"。
	if openWindow {
		markRuntimeRaidLukeCardDisplayPopupSpent(session, raidDay)
	}
	s.logPacketEvent("game-raid-luke-card-display-send",
		"char_id", characterID,
		"raid_key", raidKey,
		"msg_id", int(raid.LukeCardDisplayMsgID),
		"tarot_day", raidDay,
		// card_set_index 是包体字段 1；card_set_records 是它被校验的上界，也就是
		// luke.etc [CARD SET MAP INFO] 的记录条数（出货数据 = 15/3 = 5）；packet_records
		// 是包体字段 N（本包几条圣所记录，出货数据 = 3）。三个名字以前混用过，别再合并。
		"card_set_index", int(body[0]),
		"card_set_records", runtimeRaidLukeCardSetRecordCount(lukeConfig.CardSetMapInfo),
		"packet_records", int(body[1]),
		"open_window", int(body[len(body)-1]),
		"body_len", len(body),
		"source", source)
}
