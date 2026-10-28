package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"sync"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 公会地下城 (guild dungeon) server side.
//
// The client refuses to enter a guild dungeon entirely on its own:
// sub_3424A10 sees the runtime "is a guild dungeon" byte at +0x909 and asks
// sub_1142E10 whether the stage is unlocked, which reads a registry the server
// has to seed. With that registry empty the client shows dstr 60457
// 「该地下城已锁定， 无法进入。」 and never sends an entry request, so nothing
// server-side can help -- the packet below is the only lever.
//
// opcode 741 (0x2E5) -> sub_1146E70, registered in the same table as op14/op15/
// op33 (sub_2262A00 hands all three registration blocks the same registry), so
// it goes out with classification 0. Body, decoded from the parser:
//
//	u8  flag
//	u32 v1, u32 v2, u32 v3
//	u32 count
//	count * (u32 dungeon index, u32 clear count)
//
// The second value is the number of CLEARS, not the gauge. The client derives
// the displayed 净化率 itself as clears * [gauge on clear] / [gauge limit],
// which two live readings pin exactly: sending 1 rendered 1.5% (8500's
// [gauge on clear] is 150 of 10000), and sending 9000 rendered 100% because it
// was taken as nine thousand clears and clamped. The gauge stays the server's
// stored truth -- it is what the PVF describes and what conquest compares
// against -- and is divided back into clears on the wire.
//
// 那个 clears * [gauge on clear] / [gauge limit] 的除法在客户端自己的 PVF 里，
// 服务端改不动。所以"每次通关的净化率翻倍"只能在入账侧做，见
// currentGuildDungeonGaugeClearMultiplier。
const currentGuildDungeonUnlockMsg uint16 = 741

// currentGuildDungeonUnlockFlag is the leading byte. 1 is what makes the client
// accept the table; the three u32 that follow are not understood yet and are
// sent as the widest harmless range, so they cannot narrow anything.
const (
	currentGuildDungeonUnlockFlag byte   = 1
	currentGuildDungeonUnlockV1   uint32 = 20200101
	currentGuildDungeonUnlockV2   uint32 = 20991231
	currentGuildDungeonUnlockV3   uint32 = 0
)

type currentGuildDungeonCatalogCache struct {
	once    sync.Once
	catalog dnfguild.DungeonCatalog
	err     error
}

// currentGuildDungeonCatalog parses the PVF layout once per process.
func (s *Service) currentGuildDungeonCatalog() (dnfguild.DungeonCatalog, error) {
	s.guildDungeonCatalog.once.Do(func() {
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			s.guildDungeonCatalog.err = err
			return
		}
		s.guildDungeonCatalog.catalog, s.guildDungeonCatalog.err =
			dnfguild.LoadDungeonCatalog(items.source)
	})
	return s.guildDungeonCatalog.catalog, s.guildDungeonCatalog.err
}

// currentGuildDungeonGauges returns the selected character's guild gauges. A
// character with no guild gets nil, which still publishes the stages that have
// no prerequisite -- matching the client, where 伤城 is always open.
func (s *Service) currentGuildDungeonGauges(session *gameSession) (map[uint32]uint32, uint32) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, 0
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var (
		gauges  map[uint32]uint32
		guildID uint32
	)
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, found := registry.GuildForCharacter(characterID)
		if !found || guild == nil {
			return nil
		}
		guildID = guild.ID
		gauges = registry.DungeonGauges(guild.ID)
		return nil
	}); err != nil {
		s.logGameEvent(session, "game-guild-dungeon-gauges-unavailable",
			"char_id", session.selectedCharacterID, "error", err)
	}
	return gauges, guildID
}

func buildCurrentGuildDungeonUnlockBody(
	catalog dnfguild.DungeonCatalog,
	gauges map[uint32]uint32,
) []byte {
	body := make([]byte, 0, 17+8*len(catalog.Stages))
	body = append(body, currentGuildDungeonUnlockFlag)
	for _, value := range []uint32{
		currentGuildDungeonUnlockV1,
		currentGuildDungeonUnlockV2,
		currentGuildDungeonUnlockV3,
		uint32(len(catalog.Stages)),
	} {
		body = binary.LittleEndian.AppendUint32(body, value)
	}
	for _, stage := range catalog.Stages {
		body = binary.LittleEndian.AppendUint32(body, stage.Index)
		body = binary.LittleEndian.AppendUint32(body, currentGuildDungeonClears(stage, gauges[stage.Index]))
	}
	return body
}

// currentGuildDungeonGaugeClearMultiplier 是一次通关记几份净化槽。
//
// 客户端自己按 clears * [gauge on clear] / [gauge limit] 算净化率，8500 的
// [gauge on clear] 是 10000 里的 150，所以原版一次通关 1.5%。运营要求把这个速度
// 翻倍（2026-09-15），而客户端那个除法在它自己的 PVF 里、服务端改不动，所以只能
// 在**入账**侧做：一次通关记 2×[gauge on clear]，于是 currentGuildDungeonClears
// 除回来得到 2，客户端显示 3%。
//
// 这样做的好处是净化槽仍然是服务端的唯一真相：解锁、征服、99%/100% 的公会贡献
// 证书全都按同一个被翻倍的槽值比较，不会出现"显示 3% 但奖励按 1.5% 结算"。
const currentGuildDungeonGaugeClearMultiplier uint32 = 2

// currentGuildDungeonGaugeOnClear 是一次通关实际记入的槽值。
func currentGuildDungeonGaugeOnClear(stage dnfguild.DungeonStage) uint32 {
	if stage.GaugeOnClear == 0 {
		return 0
	}
	if stage.GaugeOnClear > ^uint32(0)/currentGuildDungeonGaugeClearMultiplier {
		return stage.GaugeOnClear
	}
	return stage.GaugeOnClear * currentGuildDungeonGaugeClearMultiplier
}

// currentGuildDungeonClears converts a stored gauge into the clear count the
// client expects, clamped to a full bar so an over-credited gauge cannot render
// past 100%.
func currentGuildDungeonClears(stage dnfguild.DungeonStage, gauge uint32) uint32 {
	if stage.GaugeOnClear == 0 {
		return 0
	}
	if stage.GaugeLimit != 0 && gauge > stage.GaugeLimit {
		gauge = stage.GaugeLimit
	}
	return gauge / stage.GaugeOnClear
}

// sendCurrentGuildDungeonState publishes the selected character's guild
// progression. Must run after the town player state exists, like every other
// state republish on this route. Failures are logged and swallowed: a guild
// dungeon table must never be able to break a town entry.
func (s *Service) sendCurrentGuildDungeonState(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	catalog, err := s.currentGuildDungeonCatalog()
	if err != nil {
		s.logGameEvent(session, "game-guild-dungeon-catalog-unavailable",
			"char_id", session.selectedCharacterID, "source", source, "error", err)
		return
	}
	gauges, guildID := s.currentGuildDungeonGauges(session)
	body := buildCurrentGuildDungeonUnlockBody(catalog, gauges)
	unlocked := make([]uint32, 0, len(catalog.Stages))
	for _, stage := range catalog.Stages {
		if catalog.Unlocked(gauges, stage.Index) {
			unlocked = append(unlocked, stage.Index)
		}
	}
	s.logGameEvent(session, "game-guild-dungeon-state-send",
		"char_id", session.selectedCharacterID,
		"source", source,
		"guild_id", guildID,
		"msg_id", currentGuildDungeonUnlockMsg,
		"classification", 0,
		"stage_count", len(catalog.Stages),
		"unlocked", unlocked,
		"gauges", gauges,
		"body_len", len(body),
		"client_handler", "sub_1146E70")
	if err := s.sendGameUpperRawClassCodec(
		session, currentGuildDungeonUnlockMsg, body, 0, true,
	); err != nil {
		s.logGameEvent(session, "game-guild-dungeon-state-send-failed",
			"char_id", session.selectedCharacterID, "source", source, "error", err)
	}
	// op741 carries the per-stage purification only; the reward-box number is a
	// separate single-u32 packet the client keeps in the same panel object.
	s.sendCurrentGuildDungeonRepeatBonusProgress(session, source)
}

// creditCurrentGuildDungeonClear adds one clear's worth of purification to the
// character's guild. A non-guild-dungeon run, or a character with no guild, is
// a no-op.
//
// The refreshed table reaches the client from the town-return route
// (sendCurrentFullDungeonReturnToTown), which both a cleared run and a forced
// exit go through. This used to say "the next town route" while
// sendCurrentGuildDungeonState was only wired to the initial (character
// selection) route, so a freshly credited gauge - and the stage it unlocked -
// stayed invisible until the player reselected the character.
func (s *Service) creditCurrentGuildDungeonClear(session *gameSession, dungeonID int64, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		dungeonID <= 0 || dungeonID > int64(^uint32(0)) {
		return
	}
	catalog, err := s.currentGuildDungeonCatalog()
	if err != nil {
		return
	}
	stage, found := catalog.Stage(uint32(dungeonID))
	if !found || stage.GaugeOnClear == 0 {
		return
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	weekID := currentGuildDungeonWeekID(s.gameplayNow())
	var (
		guildID   uint32
		gauge     uint32
		weekReset bool
		earned    []dnfguild.DungeonReward
	)
	if err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		guild, _, membership := registry.GuildForCharacter(characterID)
		if !membership || guild == nil {
			return nil
		}
		guildID = guild.ID
		// Settle the week before crediting, so the first clear of a new week
		// starts from an empty bar rather than topping up last week's.
		reset, resetErr := registry.ResetDungeonWeek(guild.ID, weekID)
		if resetErr != nil {
			return resetErr
		}
		weekReset = reset
		previous, settled, addErr := registry.AddDungeonGauge(
			guild.ID, stage.Index, currentGuildDungeonGaugeOnClear(stage), stage.GaugeLimit)
		if addErr != nil {
			return addErr
		}
		gauge = settled
		for _, reward := range stage.RewardsCrossed(previous, settled) {
			claimed, claimErr := registry.ClaimDungeonReward(guild.ID, stage.Index, reward.GaugeRatio)
			if claimErr != nil {
				return claimErr
			}
			if claimed {
				earned = append(earned, reward)
			}
		}
		return nil
	}); err != nil {
		s.logGameEvent(session, "game-guild-dungeon-clear-credit-failed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", dungeonID, "source", source, "error", err)
		return
	}
	if guildID == 0 {
		return
	}
	s.logGameEvent(session, "game-guild-dungeon-clear-credited",
		"char_id", session.selectedCharacterID,
		"guild_id", guildID,
		"dungeon_id", stage.Index,
		"gauge_on_clear", stage.GaugeOnClear,
		"gauge_credited", currentGuildDungeonGaugeOnClear(stage),
		"gauge", gauge,
		"gauge_limit", stage.GaugeLimit,
		"conquest_gauge", stage.ConquestGauge(),
		"conquered", gauge >= stage.ConquestGauge(),
		"week_id", weekID,
		"week_reset", weekReset,
		"rewards_earned", len(earned),
		"source", source)
	for _, reward := range earned {
		s.grantCurrentGuildDungeonReward(session, guildID, stage, reward)
	}
	// 重复通关奖励与净化槽是两套独立的东西：槽只在 99%/100% 发公会贡献证书，
	// 礼盒按通关次数每 20 次发一个、没有上限。
	s.grantCurrentGuildDungeonRepeatClearBonus(session, guildID, stage.Index)
}

// grantCurrentGuildDungeonReward pays one [gauge ratio] block: the item into
// the character's inventory and the guild contribution into their mileage. The
// claim was already recorded, so a delivery failure is logged loudly rather
// than retried -- silently re-running it would double-pay the whole guild.
func (s *Service) grantCurrentGuildDungeonReward(
	session *gameSession,
	guildID uint32,
	stage dnfguild.DungeonStage,
	reward dnfguild.DungeonReward,
) {
	if reward.ItemID != 0 && reward.ItemCount != 0 {
		if err := s.grantCurrentGuildDungeonRewardItem(session, reward); err != nil {
			s.logGameEvent(session, "game-guild-dungeon-reward-item-failed",
				"char_id", session.selectedCharacterID,
				"guild_id", guildID,
				"dungeon_id", stage.Index,
				"gauge_ratio", reward.GaugeRatio,
				"item_id", reward.ItemID,
				"item_count", reward.ItemCount,
				"error", err)
		}
	}
	if reward.Contribute != 0 {
		if err := s.creditCurrentGuildDungeonContribution(session, reward.Contribute); err != nil {
			s.logGameEvent(session, "game-guild-dungeon-reward-contribute-failed",
				"char_id", session.selectedCharacterID,
				"guild_id", guildID,
				"dungeon_id", stage.Index,
				"gauge_ratio", reward.GaugeRatio,
				"contribute", reward.Contribute,
				"error", err)
		}
	}
	s.logGameEvent(session, "game-guild-dungeon-reward-granted",
		"char_id", session.selectedCharacterID,
		"guild_id", guildID,
		"dungeon_id", stage.Index,
		"gauge_ratio", reward.GaugeRatio,
		"item_id", reward.ItemID,
		"item_count", reward.ItemCount,
		"contribute", reward.Contribute)
}

// grantCurrentGuildDungeonRewardItem delivers the reward by mail. A cleared run
// ends in a settlement the player may be watching with a full bag, and mail is
// the one delivery path that cannot fail on inventory space.
func (s *Service) grantCurrentGuildDungeonRewardItem(session *gameSession, reward dnfguild.DungeonReward) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	_, err := s.GMSendItemMail(
		ctx,
		strconv.Itoa(int(session.selectedCharacterID)),
		reward.ItemID,
		int64(reward.ItemCount),
		currentGuildDungeonRewardMailTitle,
		currentGuildDungeonRewardMailBody,
	)
	return err
}

func (s *Service) creditCurrentGuildDungeonContribution(session *gameSession, amount uint32) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	return s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.AddDungeonContribution(characterID, amount, s.gameplayNow())
	})
}

const (
	currentGuildDungeonRewardMailTitle = "公会地下城"
	currentGuildDungeonRewardMailBody  = "公会地下城净化奖励。"
)

// 公会地下城奖励礼盒 —— PVF 布局末尾的 [repeat clear bonus reward]。
//
//	[repeat clear bonus reward]
//	10156320 1
//	[level difference] 10
//	[delete item]      10154439
//
// parseDungeonCatalog 只处理 [dungeon] 区块内的键，以及区块外的 final dungeon，
// 所以这一整节从来没有被解析过，全服务端也没有任何路径发放过 10156320 —— 礼盒
// 因此永远拿不到。净化槽（gauge）是另一套东西：它驱动解锁与征服，奖励只在
// [gauge ratio] 99/100 两档发公会贡献证书，与礼盒无关。
//
// 运营给定的规则：**每通关 N 次公会地下城发 1 个礼盒，没有上限**，也就是每通关
// 一次涨 100/N %。计数按角色累计、跨全部公会副本，存在角色 Stats 的溢出字段里
// （与虚弱的恢复时间戳同一种存法），不需要改 schema。
//
// N 在 2026-09-15 由 20 调成 10（一次通关从 5% 变成 10%）。百分比是从 N 算出来
// 的，不是另一个常量，所以改 N 一处就够。旧的 20 是运营从网络搜索来的口径，PVF
// 的 [repeat clear bonus reward] 段里没有任何次数字段，改它不用翻 PVF。
//
// 这里刻意不实现 [level difference] 与 [delete item]：它们的确切语义尚无证据，
// 而按运营规则发放不依赖它们。补齐之前不要假装实现。
const (
	currentGuildDungeonRepeatClearBonusItem uint32 = 10156320
	currentGuildDungeonRepeatClearsPerBonus int64  = 10
	currentGuildDungeonClearCountStat              = "guild_dungeon_clear_count"
	currentGuildDungeonBonusMailTitle              = "公会地下城"
	currentGuildDungeonBonusMailBody               = "公会地下城重复通关奖励。"
)

// currentGuildDungeonRepeatBonusOwed is how many boxes a counter moving from
// before to after has just completed. Expressed as a difference of block
// indices rather than "after % 20 == 0" so a jump of several clears pays every
// block it crossed and a replayed call pays nothing.
func currentGuildDungeonRepeatBonusOwed(before int64, after int64) int64 {
	if before < 0 {
		before = 0
	}
	if after <= before {
		return 0
	}
	return after/currentGuildDungeonRepeatClearsPerBonus - before/currentGuildDungeonRepeatClearsPerBonus
}

// currentGuildDungeonRepeatBonusPercent is the progress toward the next box:
// one clear of currentGuildDungeonRepeatClearsPerBonus is 100/N %, so with
// N=10 it steps 0,10,...,90. A completed block reads 0 again, which is the same
// thing the box grant reports on that clear.
func currentGuildDungeonRepeatBonusPercent(count int64) int64 {
	if count <= 0 {
		return 0
	}
	return count % currentGuildDungeonRepeatClearsPerBonus * 100 / currentGuildDungeonRepeatClearsPerBonus
}

// currentGuildDungeonRepeatBonusMsg (op742) is the packet the reward-box
// progress is drawn from. Found by disassembly 2026-09-15:
//
//   - op742 registers two handlers like op741 does: sub_11470F0 on the guild
//     dungeon table and sub_1D0D380 as the failure/notice dispatcher.
//   - sub_11470F0 reads **exactly one u32** and stores it at panel+0x40
//     (0x517E380 = 0x517E340 + 0x40); it reads nothing else.
//   - 0x011457DA loads that field, formats it and draws it immediately before
//     0x01145851 places the 公会地下城奖励礼盒 icon (item 10156320) - i.e. it is
//     the number beside the box, the bar the player sees inside the dungeon.
//
// The server never sent op742, so that number stayed at its initial 0 no matter
// how many clears were credited. op741 could not carry it: its body is fully
// consumed by sub_1146E70 (u8 + 4×u32 header, then exactly two u32 per stage).
const currentGuildDungeonRepeatBonusMsg uint16 = 742

// sendCurrentGuildDungeonRepeatBonusProgress publishes the box progress.
//
// Units pinned by live reading 2026-09-15: the value is a **0..100 percentage**.
// Sending the raw clears-toward-next-box (19 of 20) rendered the in-dungeon bar
// at roughly 19%, not the 95% a count-of-twenty would have drawn, so the client
// takes the u32 as a percent directly. The format string itself stays unreadable
// statically (obfuscated, resolved through sub_34D9B20), which is why this was
// settled by A/B instead.
func (s *Service) sendCurrentGuildDungeonRepeatBonusProgress(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	count := s.currentGuildDungeonClearCount(session)
	toward := count % currentGuildDungeonRepeatClearsPerBonus
	sent := currentGuildDungeonRepeatBonusPercent(count)
	body := binary.LittleEndian.AppendUint32(make([]byte, 0, 4), uint32(sent))
	s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-send",
		"char_id", session.selectedCharacterID,
		"source", source,
		"msg_id", currentGuildDungeonRepeatBonusMsg,
		"classification", 0,
		"clear_count", count,
		"sent_value", sent,
		"candidate_clears_toward_bonus", toward,
		"candidate_percent", currentGuildDungeonRepeatBonusPercent(count),
		"client_handler", "sub_11470F0")
	if err := s.sendGameUpperRawClassCodec(
		session, currentGuildDungeonRepeatBonusMsg, body, 0, true,
	); err != nil {
		s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-send-failed",
			"char_id", session.selectedCharacterID, "source", source, "error", err)
	}
}

// currentGuildDungeonClearCount reads the stored guild dungeon clear counter.
func (s *Service) currentGuildDungeonClearCount(session *gameSession) int64 {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return 0
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found || character.Stats == nil {
		return 0
	}
	count := character.Stats[currentGuildDungeonClearCountStat]
	if count < 0 {
		return 0
	}
	return count
}

// grantCurrentGuildDungeonRepeatClearBonus counts one more guild dungeon clear
// for this character and mails one 公会地下城奖励礼盒 for every completed block
// of currentGuildDungeonRepeatClearsPerBonus. The counter only ever moves
// forward, so the number of boxes owed is the difference between the old and
// the new block index -- a server that was down for several clears cannot
// double-pay, and a single call cannot skip a box either.
func (s *Service) grantCurrentGuildDungeonRepeatClearBonus(session *gameSession, guildID uint32, dungeonID uint32) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	now := s.gameplayNow().UTC()
	var before, after int64
	err := repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			characters dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			character, found, loadErr := characters.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !found {
				return nil
			}
			character = dnfrepo.CloneCharacter(character)
			if character.Stats == nil {
				character.Stats = make(map[string]int64, 1)
			}
			before = character.Stats[currentGuildDungeonClearCountStat]
			if before < 0 {
				before = 0
			}
			after = before + 1
			character.Stats[currentGuildDungeonClearCountStat] = after
			character.UpdatedAt = now
			return characters.Save(ctx, character)
		},
	)
	if err != nil {
		s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-failed",
			"char_id", session.selectedCharacterID,
			"guild_id", guildID, "dungeon_id", dungeonID, "error", err)
		return
	}
	if after == 0 {
		return
	}
	owed := currentGuildDungeonRepeatBonusOwed(before, after)
	s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-progress",
		"char_id", session.selectedCharacterID,
		"guild_id", guildID,
		"dungeon_id", dungeonID,
		"clear_count", after,
		"clears_per_bonus", currentGuildDungeonRepeatClearsPerBonus,
		"progress_percent", currentGuildDungeonRepeatBonusPercent(after),
		"bonus_owed", owed)
	for index := int64(0); index < owed; index++ {
		if _, err := s.GMSendItemMail(
			ctx,
			characterID,
			currentGuildDungeonRepeatClearBonusItem,
			1,
			currentGuildDungeonBonusMailTitle,
			currentGuildDungeonBonusMailBody,
		); err != nil {
			// The counter is already committed, so a delivery failure is logged
			// loudly rather than retried: re-running it would pay twice.
			s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-delivery-failed",
				"char_id", session.selectedCharacterID,
				"guild_id", guildID,
				"dungeon_id", dungeonID,
				"clear_count", after,
				"item_id", currentGuildDungeonRepeatClearBonusItem,
				"error", err)
			return
		}
		s.logGameEvent(session, "game-guild-dungeon-repeat-bonus-granted",
			"char_id", session.selectedCharacterID,
			"guild_id", guildID,
			"dungeon_id", dungeonID,
			"clear_count", after,
			"item_id", currentGuildDungeonRepeatClearBonusItem)
	}
}

// currentGuildDungeonWeekID is the server's existing weekly boundary (Monday
// 06:00), shared with the altar's own weekly records rather than invented here.
// The layout's [weekly reward] block names 8508 as the cycle's target but
// carries no period of its own, so the boundary is the server's, not a new one.
func currentGuildDungeonWeekID(now time.Time) int64 {
	return (premium.ServiceDayID(now) + 3) / 7
}
