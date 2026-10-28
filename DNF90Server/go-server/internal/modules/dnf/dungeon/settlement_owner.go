package dungeon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/honor"
	"longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

const (
	settlementReceiptPrefix  = "dungeon_settlement_receipt_"
	settlementReceiptVersion = int64(1)
)

var ErrSettlementStateInvalid = errors.New("dungeon settlement persisted state is invalid")

type SettlementCommand struct {
	AccountID               string
	CharacterID             string
	CompletionKey           string
	Tables                  *progression.Tables
	Experience              uint32
	RecommendedDungeonClear bool
	// WeddingCapsule 是婚姻成长胶囊的 PVF 规则；仅当"已婚角色通关推荐副本"时
	// 由 bridge 解析并传入，其它情况为 nil，本 owner 会整段跳过。
	WeddingCapsule *wedding.CapsuleCatalog
	// 活动（onlineevent）进度闸门。这三项都必须在本事务内推进，否则"通关即签到"
	// 这条链路就断了——签到类活动的进度只在副本结算里累加，没有第二个写入方。
	//
	//   MonthlyAttendance  月度签到（活动 2370）：推荐副本通关计数，每日 2 次
	//   WeeklyAttendance   周周签到（活动 2208）：需要目录来判定当周与补签
	//   AttendanceMarble   签到弹珠：通关清次数决定额外骰子
	//
	// 三者都只在 RecommendedDungeonClear 为真时生效。
	MonthlyAttendance bool
	WeeklyAttendance  *onlineevent.WeeklyCatalog
	AttendanceMarble  *onlineevent.MarbleCatalog
	AdventureRuntime  *adventuregroup.RuntimeConfig
	HonorExpertTables *honor.Tables
	MaximumCharacterLevel int
	// RentalPointAward and RentalPointLimit are resolved from the current PVF
	// rental catalog and committed with the dungeon completion receipt.
	RentalPointAward uint32
	RentalPointLimit uint32
	// AllowZeroExperience opts a completion that legitimately pays no clear
	// experience into the ledger. The zero-experience guard below exists to catch
	// a caller whose clear-exp computation silently produced nothing; a raid gate
	// is the one real case (its reward is the raid phase settlement, not character
	// growth), so it has to ask explicitly instead of the guard being relaxed for
	// everyone.
	AllowZeroExperience bool
	// Tower carries a Tower of Despair/Grief clear: its ten frozen reward slots
	// and the entry pool it consumes. Nil for every ordinary dungeon.
	Tower *TowerClear
	// GuildContribution carries the 公会贡献度 events of this clear. Nil for
	// every non-member and for clears that never took a roster snapshot.
	GuildContribution *GuildContributionClear
	UpdatedAt         time.Time
}

type SettlementResult struct {
	Character          dnfrepo.CharacterRecord
	Skill              dnfrepo.SkillRecord
	Inventory          dnfrepo.InventoryRecord
	ExperienceGain     uint32
	HonorExpertGain    uint32
	SPGain             int
	TPGain             int
	LuckyStarBefore    uint32
	LuckyStarApplied   uint32
	LuckyStarAfter     uint32
	LuckyStarAttempted bool
	Replayed           bool
}

// CommitSettlement atomically advances experience and the SP/TP ledger and
// writes an idempotency receipt tied to the accepted dungeon completion.
func (o *Owner) CommitSettlement(ctx context.Context, cmd SettlementCommand) (SettlementResult, error) {
	if o == nil || o.settlements == nil || strings.TrimSpace(cmd.CharacterID) == "" ||
		strings.TrimSpace(cmd.AccountID) == "" || strings.TrimSpace(cmd.CompletionKey) == "" ||
		cmd.Tables == nil {
		return SettlementResult{}, ErrOwnerUnavailable
	}
	// A tower clear pays its reward through the tower window rather than clear
	// experience, so a zero-experience tower settlement is legitimate.
	// 同理：签到类活动即使这一笔不带经验（例如免经验的活动副本），只要它会推进
	// 活动进度，也必须放行——否则玩家通关了却什么都没记上。
	if cmd.Experience == 0 && !cmd.AllowZeroExperience && cmd.Tower == nil &&
		!(cmd.RecommendedDungeonClear && (cmd.RentalPointAward > 0 || cmd.WeddingCapsule != nil ||
			cmd.MonthlyAttendance || cmd.WeeklyAttendance != nil || cmd.AttendanceMarble != nil)) {
		return SettlementResult{}, ErrSettlementStateInvalid
	}
	ctx = contextOrBackground(ctx)
	wantHash := sha256.Sum256([]byte(cmd.CompletionKey))
	now := updatedAtOrNow(cmd.UpdatedAt)

	var result SettlementResult
	err := o.settlements.WithinCharacterSettlement(ctx, cmd.CharacterID, func(tx dnfrepo.Group) error {
		character, found, err := tx.Character.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != cmd.CharacterID ||
			strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID) {
			return ErrSettlementStateInvalid
		}
		skill, found, err := tx.Skill.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || skill.CharacterID != cmd.CharacterID {
			return ErrSettlementStateInvalid
		}
		inventory, found, err := tx.Inventory.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found || inventory.CharacterID != cmd.CharacterID {
			return ErrSettlementStateInvalid
		}
		character = dnfrepo.CloneCharacter(character)
		skill = dnfrepo.CloneSkill(skill)
		inventory = dnfrepo.CloneInventory(inventory)
		if settlementReceiptMatches(character.Stats, wantHash) {
			receipt, err := settlementReceiptValues(character, skill)
			if err != nil {
				return err
			}
			result = SettlementResult{
				Character: character, Skill: skill, Inventory: inventory,
				ExperienceGain:     receipt.ExperienceGain,
				SPGain:             receipt.SPGain,
				LuckyStarBefore:    receipt.LuckyStarBefore,
				LuckyStarApplied:   receipt.LuckyStarApplied,
				LuckyStarAfter:     receipt.LuckyStarAfter,
				LuckyStarAttempted: receipt.LuckyStarAttempted,
				Replayed:           true,
			}
			return nil
		}
		experience, present := character.Stats["exp"]
		if !present || experience < 0 || uint64(experience) > math.MaxUint32 ||
			character.Level <= 0 || skill.Points.SyncedLevel != character.Level {
			return ErrSettlementStateInvalid
		}
		maximumLevel := cmd.MaximumCharacterLevel
		if maximumLevel <= 0 {
			maximumLevel = progression.MaximumCharacterLevel
		}
		planned, err := PlanSettlementProgressionAtCap(
			cmd.Tables,
			character.Level,
			uint32(experience),
			cmd.Experience,
			skill.Points,
			maximumLevel,
		)
		if err != nil {
			return err
		}
		maximumLevelExperience := maximumLevelExperienceGain(
			character.Level,
			uint32(experience),
			cmd.Experience,
			cmd.MaximumCharacterLevel,
			cmd.Tables,
		)
		character.Level = planned.Experience.NewLevel
		character.Stats["exp"] = int64(planned.Experience.NewExperience)
		appliedHonorExpertGain := uint32(0)
		if maximumLevelExperience > 0 && cmd.HonorExpertTables != nil {
			currentExpert, err := honor.ExpertProgressFromStats(character.Stats)
			if err != nil {
				return err
			}
			nextExpert, err := cmd.HonorExpertTables.AdvanceExpert(currentExpert, uint64(maximumLevelExperience))
			if err != nil {
				return err
			}
			expertStats, err := honor.ExpertStats(nextExpert)
			if err != nil {
				return err
			}
			for key, value := range expertStats {
				character.Stats[key] = value
			}
			appliedHonorExpertGain = maximumLevelExperience
		}
		if cmd.RecommendedDungeonClear {
			// 签到弹珠：通关清次数决定额外骰子（4/8/12 次档）。必须在推荐次数
			// 自增之前跑，它读的是"这一局之前"的累计清次数。
			if err := onlineevent.RecordMarbleClear(&character, cmd.AttendanceMarble, now); err != nil {
				return err
			}
			count := character.Stats[adventuregroup.RecommendedDungeonClearStatKey]
			if count < 0 {
				return ErrSettlementStateInvalid
			}
			if count < math.MaxUint16 {
				character.Stats[adventuregroup.RecommendedDungeonClearStatKey] = count + 1
			}
			// 已婚角色的推荐副本通关同时累加婚姻成长胶囊进度。与推荐次数同事务，
			// 所以重放同一 CompletionKey 不会重复计入（回执幂等门在上层）。
			if cmd.WeddingCapsule != nil {
				if err := wedding.AccumulateCapsuleClear(&character, cmd.WeddingCapsule); err != nil {
					return err
				}
			}
		}
		// 月度/周签到的进度落在**账号**上，所以有它们就必须把账号行读进同一事务。
		monthlyClear := cmd.MonthlyAttendance && cmd.RecommendedDungeonClear
		weeklyClear := cmd.WeeklyAttendance != nil && cmd.RecommendedDungeonClear
		// 骁勇结晶从 2026-09-24 起对**任意等级**的经验记账（不再只收满级溢出），
		// 所以只要这笔结算带了经验、又给了冒险团配置，就必须把账号记录读进来
		// 同事务提交；否则未满级角色通关会被 ErrSettlementStateInvalid 卡住。
		needsAccount := monthlyClear || weeklyClear || cmd.RentalPointAward > 0 || cmd.Tower != nil ||
			(cmd.AdventureRuntime != nil && (cmd.Experience > 0 || maximumLevelExperience > 0))
		var account dnfrepo.AccountRecord
		accountLoaded := false
		if needsAccount {
			if tx.Account == nil {
				return ErrSettlementStateInvalid
			}
			loaded, accountFound, err := tx.Account.Load(ctx, cmd.AccountID)
			if err != nil {
				return err
			}
			if !accountFound || strings.TrimSpace(loaded.AccountID) != strings.TrimSpace(cmd.AccountID) {
				// 账号行缺失时的取舍与怪物击杀一致：这笔结算里只要有**满级部分**
				// （进胶囊槽/名誉经验）或者**租赁点券**要落，就整笔失败 —— 那两项
				// 是原版规则，静默丢掉比报错更糟；只有"未满级经验镜像进骁勇结晶"
				// 时跳过记账、继续照常发角色经验与奖励，不让一行账本缺失把玩家的
				// 通关结算整个卡死（"改了A把B弄坏"正是要避免的）。
				//
				// 签到同理：账号行不在就没法记签到进度，但也不能因此把整笔通关
				// 卡死——角色经验/技能/背包的写入还在下面。
				if cmd.RentalPointAward > 0 || maximumLevelExperience > 0 || cmd.Tower != nil {
					return ErrSettlementStateInvalid
				}
				// 注意：这里不能 return —— 角色经验/技能/背包的写入还在下面。
				needsAccount = false
			} else {
				account = dnfrepo.CloneAccount(loaded)
				accountLoaded = true
			}
		}
		if accountLoaded {
			if monthlyClear {
				if err := onlineevent.RecordMonthlyClear(&account, now); err != nil {
					return err
				}
			}
			if weeklyClear {
				if err := onlineevent.RecordWeeklyClear(&account, cmd.WeeklyAttendance, now); err != nil {
					return err
				}
			}
		}
		if cmd.RentalPointAward > 0 {
			if cmd.RentalPointLimit == 0 || cmd.RentalPointAward > cmd.RentalPointLimit {
				return ErrSettlementStateInvalid
			}
			current, err := parseLuckyStars(account.Metadata[LuckyStarMetadataKey])
			if err != nil {
				return fmt.Errorf("%w: invalid %s metadata: %v", ErrSettlementStateInvalid, LuckyStarMetadataKey, err)
			}
			result.LuckyStarBefore = current
			result.LuckyStarAfter = current
			result.LuckyStarAttempted = true
			if current < cmd.RentalPointLimit {
				applied := cmd.RentalPointAward
				available := cmd.RentalPointLimit - current
				if applied > available {
					applied = available
				}
				result.LuckyStarApplied = applied
				result.LuckyStarAfter = current + applied
				if applied > 0 {
					if account.Metadata == nil {
						account.Metadata = make(map[string]string)
					}
					account.Metadata[LuckyStarMetadataKey] = strconv.FormatUint(uint64(result.LuckyStarAfter), 10)
				}
			}
		}
		writeSettlementReceipt(
			character.Stats,
			wantHash,
			cmd.Experience,
			planned.SkillPoints.SPGain,
			planned.Experience.NewLevel,
			planned.Experience.NewExperience,
			result.LuckyStarAttempted,
			result.LuckyStarBefore,
			result.LuckyStarApplied,
			result.LuckyStarAfter,
		)
		character.UpdatedAt = now
		skill.Points = planned.SkillPoints.New
		skill.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill, dnfrepo.SkillFieldPoints); err != nil {
			return err
		}
		if accountLoaded && cmd.AdventureRuntime != nil && (cmd.Experience > 0 || maximumLevelExperience > 0) {
			// 骁勇结晶 / 成长胶囊槽 / 账号名誉经验的口径只在 adventuregroup 里
			// 定义一次：怪物击杀与任务完成两条路也用同一个函数记入，否则同一笔
			// 经验会在不同路径上算出不同结果。
			//   AwardedExperience      = 这次通关经验全额 → 骁勇结晶（任意等级）
			//   MaximumLevelExperience = 其中的满级溢出 → 胶囊槽 + 名誉经验
			if _, err := adventuregroup.ApplyExperienceCredit(
				&account,
				*cmd.AdventureRuntime,
				adventuregroup.ExperienceCredit{
					AwardedExperience:      uint64(cmd.Experience),
					MaximumLevelExperience: uint64(maximumLevelExperience),
				},
				now,
			); err != nil {
				return err
			}
		}
		if accountLoaded {
			account.UpdatedAt = now
			if err := tx.Account.Save(ctx, account); err != nil {
				return err
			}
		}
		if cmd.Tower != nil {
			// Consume the entry pool (character entry first, then the account
			// pool), then apply the frozen ten reward slots. Both live in this
			// transaction so a failed grant leaves the floor and the pools
			// untouched.
			if !accountLoaded {
				return ErrSettlementStateInvalid
			}
			if err := recordTowerEntryClear(&account, &character, cmd.Tower.Kind, now); err != nil {
				return err
			}
			if err := applyTowerClear(&character, &inventory, cmd.Tower, now); err != nil {
				return err
			}
			character.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			account.UpdatedAt = now
			if err := tx.Account.Save(ctx, account); err != nil {
				return err
			}
			inventory.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		result = SettlementResult{
			Character: character, Skill: skill, Inventory: inventory,
			ExperienceGain:     cmd.Experience,
			HonorExpertGain:    appliedHonorExpertGain,
			SPGain:             planned.SkillPoints.SPGain,
			TPGain:             planned.SkillPoints.TPGain,
			LuckyStarBefore:    result.LuckyStarBefore,
			LuckyStarApplied:   result.LuckyStarApplied,
			LuckyStarAfter:     result.LuckyStarAfter,
			LuckyStarAttempted: result.LuckyStarAttempted,
		}
		return nil
	})
	if err != nil {
		return SettlementResult{}, err
	}
	return result, nil
}

func maximumLevelExperienceGain(
	previousLevel int,
	previousExperience uint32,
	gain uint32,
	maximumLevel int,
	tables *progression.Tables,
) uint32 {
	if maximumLevel <= 0 || previousLevel <= 0 || previousLevel > maximumLevel || gain == 0 || tables == nil {
		return 0
	}
	if previousLevel >= maximumLevel {
		return gain
	}
	entryExperience, err := tables.ThresholdToNext(maximumLevel - 1)
	if err != nil {
		return 0
	}
	total := uint64(previousExperience) + uint64(gain)
	if total <= uint64(entryExperience) {
		return 0
	}
	excess := total - uint64(entryExperience)
	if excess > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(excess)
}

func PlanSettlementProgression(
	tables *progression.Tables,
	level int,
	total uint32,
	gain uint32,
	points dnfrepo.SkillPointState,
) (progression.ExperienceSkillPointPlan, error) {
	return PlanSettlementProgressionAtCap(tables, level, total, gain, points, progression.MaximumCharacterLevel)
}

func PlanSettlementProgressionAtCap(
	tables *progression.Tables,
	level int,
	total uint32,
	gain uint32,
	points dnfrepo.SkillPointState,
	maximumLevel int,
) (progression.ExperienceSkillPointPlan, error) {
	if tables == nil || level <= 0 || points.SyncedLevel != level || maximumLevel <= 0 ||
		maximumLevel > progression.MaximumCharacterLevel || level > maximumLevel {
		return progression.ExperienceSkillPointPlan{}, ErrSettlementStateInvalid
	}
	newTotal := uint64(total) + uint64(gain)
	if newTotal > math.MaxUint32 {
		newTotal = math.MaxUint32
	}
	result := progression.ExperienceResult{
		PreviousLevel:      level,
		PreviousExperience: total,
		Gain:               gain,
		NewLevel:           level,
		NewExperience:      uint32(newTotal),
	}
	for result.NewLevel < maximumLevel {
		threshold, err := tables.ThresholdToNext(result.NewLevel)
		if err != nil {
			return progression.ExperienceSkillPointPlan{}, err
		}
		if result.NewExperience < threshold {
			break
		}
		if _, ok := tables.SkillPointsAtLevel(result.NewLevel + 1); !ok {
			return progression.ExperienceSkillPointPlan{}, ErrSettlementStateInvalid
		}
		result.NewLevel++
	}
	// Max-level ordinary EXP stays at the entry threshold. The caller still
	// assigns the full post-cap award to HonorExpert.
	if result.NewLevel == maximumLevel && maximumLevel > 1 {
		entryThreshold, err := tables.ThresholdToNext(maximumLevel - 1)
		if err != nil {
			return progression.ExperienceSkillPointPlan{}, err
		}
		if result.NewExperience > entryThreshold {
			result.NewExperience = entryThreshold
			result.Saturated = true
		}
	}
	result.LevelsGained = result.NewLevel - result.PreviousLevel
	advance, err := tables.AdvanceSkillPoints(points, result.NewLevel)
	if err != nil {
		return progression.ExperienceSkillPointPlan{}, err
	}
	return progression.ExperienceSkillPointPlan{Experience: result, SkillPoints: advance}, nil
}

func settlementReceiptMatches(stats map[string]int64, want [sha256.Size]byte) bool {
	if stats == nil || stats[settlementReceiptPrefix+"version"] != settlementReceiptVersion {
		return false
	}
	for index := 0; index < 4; index++ {
		if uint64(stats[fmt.Sprintf("%shash_%d", settlementReceiptPrefix, index)]) !=
			binary.LittleEndian.Uint64(want[index*8:(index+1)*8]) {
			return false
		}
	}
	return true
}

func writeSettlementReceipt(
	stats map[string]int64,
	hash [sha256.Size]byte,
	gain uint32,
	spGain int,
	newLevel int,
	newExperience uint32,
	luckyStarAttempted bool,
	luckyStarBefore uint32,
	luckyStarApplied uint32,
	luckyStarAfter uint32,
) {
	stats[settlementReceiptPrefix+"version"] = settlementReceiptVersion
	for index := 0; index < 4; index++ {
		stats[fmt.Sprintf("%shash_%d", settlementReceiptPrefix, index)] =
			int64(binary.LittleEndian.Uint64(hash[index*8 : (index+1)*8]))
	}
	stats[settlementReceiptPrefix+"experience_gain"] = int64(gain)
	stats[settlementReceiptPrefix+"sp_gain"] = int64(spGain)
	stats[settlementReceiptPrefix+"new_level"] = int64(newLevel)
	stats[settlementReceiptPrefix+"new_experience"] = int64(newExperience)
	stats[settlementReceiptPrefix+"lucky_star_attempted"] = 0
	if luckyStarAttempted {
		stats[settlementReceiptPrefix+"lucky_star_attempted"] = 1
	}
	stats[settlementReceiptPrefix+"lucky_star_before"] = int64(luckyStarBefore)
	stats[settlementReceiptPrefix+"lucky_star_applied"] = int64(luckyStarApplied)
	stats[settlementReceiptPrefix+"lucky_star_after"] = int64(luckyStarAfter)
}

type settlementReceipt struct {
	ExperienceGain     uint32
	SPGain             int
	LuckyStarBefore    uint32
	LuckyStarApplied   uint32
	LuckyStarAfter     uint32
	LuckyStarAttempted bool
}

func settlementReceiptValues(
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
) (settlementReceipt, error) {
	stats := character.Stats
	gain, gainPresent := stats[settlementReceiptPrefix+"experience_gain"]
	spGain := stats[settlementReceiptPrefix+"sp_gain"]
	if !gainPresent || gain < 0 || gain > math.MaxUint32 || spGain < 0 || spGain > math.MaxInt ||
		stats[settlementReceiptPrefix+"new_level"] != int64(character.Level) ||
		stats[settlementReceiptPrefix+"new_experience"] != stats["exp"] ||
		skill.Points.SyncedLevel != character.Level {
		return settlementReceipt{}, ErrSettlementStateInvalid
	}
	receipt := settlementReceipt{ExperienceGain: uint32(gain), SPGain: int(spGain)}
	attempted := stats[settlementReceiptPrefix+"lucky_star_attempted"]
	if attempted == 0 {
		if stats[settlementReceiptPrefix+"lucky_star_before"] != 0 ||
			stats[settlementReceiptPrefix+"lucky_star_applied"] != 0 ||
			stats[settlementReceiptPrefix+"lucky_star_after"] != 0 {
			return settlementReceipt{}, ErrSettlementStateInvalid
		}
		return receipt, nil
	}
	if attempted != 1 {
		return settlementReceipt{}, ErrSettlementStateInvalid
	}
	before := stats[settlementReceiptPrefix+"lucky_star_before"]
	applied := stats[settlementReceiptPrefix+"lucky_star_applied"]
	after := stats[settlementReceiptPrefix+"lucky_star_after"]
	if before < 0 || before > math.MaxUint32 || applied < 0 || applied > math.MaxUint32 ||
		after < 0 || after > math.MaxUint32 || before+applied != after {
		return settlementReceipt{}, ErrSettlementStateInvalid
	}
	receipt.LuckyStarBefore = uint32(before)
	receipt.LuckyStarApplied = uint32(applied)
	receipt.LuckyStarAfter = uint32(after)
	receipt.LuckyStarAttempted = true
	return receipt, nil
}
