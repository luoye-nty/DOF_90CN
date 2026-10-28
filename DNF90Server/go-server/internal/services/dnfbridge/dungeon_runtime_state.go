package dnfbridge

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

var dungeonRuntimePayloadReleaseCount atomic.Uint64

var (
	errDungeonWorldMapUnavailable        = errors.New("dnf dungeon worldmap runtime is unavailable")
	errDungeonMonsterCatalogUnavailable  = errors.New("dnf dungeon monster catalog is unavailable")
	errDungeonCharacterRequired          = errors.New("dnf dungeon selected character is required")
	errDungeonCharacterNotFound          = errors.New("dnf dungeon selected character was not found")
	errDungeonAccountMismatch            = errors.New("dnf dungeon selected character account mismatch")
	errDungeonQuestRepositoryUnavailable = errors.New("dnf dungeon quest repository is unavailable")
	errDungeonInfernoLocked              = errors.New("dnf inferno dungeon is not unlocked by its PVF quest")
	errDungeonDifficultyInvalid          = errors.New("dnf dungeon difficulty is invalid")
	errDungeonLevelTooLow                = dnfdungeon.ErrEntryLevelTooLow
	errDungeonFatigueUnknown             = dnfdungeon.ErrEntryFatigueUnknown
	errDungeonFatigueExhausted           = dnfdungeon.ErrEntryFatigueExhausted
	errDungeonPartyLimit                 = dnfdungeon.ErrEntryPartyLimit
	errDungeonAlreadyActive              = errors.New("dnf dungeon session is already active")
	errDungeonRuntimeRetired             = errors.New("dnf dungeon runtime generation is already retired")
	errDungeonChoiceRequired             = errors.New("dnf dungeon explicit PVF choice is required")
	errDungeonMonsterOwnerMismatch       = errors.New("dnf dungeon monster death related actor object key mismatch")
)

type currentDungeonRuntimeLifecycle byte

const (
	currentDungeonRuntimePrepared currentDungeonRuntimeLifecycle = iota
	currentDungeonRuntimeActive
	currentDungeonRuntimeRetired
)

type runtimeDungeonState struct {
	Request dungeoncmd.SelectDungeonRequest
	// ProtocolMode is the validated op16 mode mirrored into op28.
	ProtocolMode currentDungeonProtocolMode
	Dungeon      worldmap.Dungeon
	// clientDungeonID is the selector/resource identity projected to the current
	// EXE. In-dungeon op28/op29 packets must continue to use Dungeon.ID.
	// Single Anton owns dedicated PVF dungeons 5314-5322 on the server while
	// the current client keeps its native Raid objective table under 210-224.
	clientDungeonID int64
	MazeIndex       int
	Character       dnfrepo.CharacterRecord
	Session         *worldmap.DungeonSession
	Room            *runtimeDungeonRoom
	DropOwner       *runtimeDungeonDropOwner
	HellParty       *currentDungeonHellPartyOwner
	NextObjectKey   uint32
	BossCoordinate  worldmap.RoomCoordinate
	BossSet         bool
	// entryCharacterLevel 是**进图那一刻**的角色等级快照。"推荐地下城通关"
	// 判定必须用它而不是 runtime.Character.Level：本局的小怪与翻牌经验会把等级
	// 推过推荐区间上限，用实时等级会让"推荐副本"在打完最后一张图时判成不推荐，
	// 于是月度签到/周签到/婚姻胶囊这些挂在"推荐副本通关"上的活动全部漏记。
	// 0 表示没有快照，调用方回落到实时等级。
	entryCharacterLevel        int
	LayeredMapIndex            int
	LayeredMapActive           bool
	LayerChains                map[worldmap.RoomCoordinate]*runtimeDungeonLayerChain
	StoryStages                []worldmap.DungeonStoryStage
	StoryStageIndex            int
	Seed                       uint32
	RandomizedObjectSelections [][]currentDungeonRandomizedObjectSelection
	RandomizedObjectGroups     [][]currentDungeonStartMapRidableEntry
	RandomizedObjectsPlanned   bool
	RidableObjects             *currentDungeonRidableObjectOwner
	partyDungeonOwner          *runtimePartyDungeonOwner
	partyMemberIndex           byte
	partyMemberIndexed         bool
	Rooms                      map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit
	startedAt                  time.Time
	// bossDieCheckObserved* records that the client already spent its single
	// op117 in this room, whatever the server did with it. The tutorial final
	// room defers to op117 when a boss-ranked monster exists; without this
	// marker a client that sends op117 before the room's last blocker dies
	// leaves the run waiting for a packet that will never come again.
	bossDieCheckObserved              bool
	bossDieCheckObservedRoom          worldmap.RoomCoordinate
	bossDieCheckObservedMapID         int64
	bossDieCheckPending               bool
	bossDieCheckPendingRequest        dungeoncmd.BossDieCheckRequest
	bossDieCheckAccepted              bool
	bossDieCheckRelatedActorObjectKey uint16
	bossDieCheckTargetObjectKey       uint16
	bossDieCheckResponseSent          bool
	bossEntranceConditionComplete     bool
	bossEntranceGateNotificationSent  bool
	// actionObjects is the run's dungeon-symbol runtime: the passive-object
	// scripts, their trigger state and the symbols they share. Built lazily on
	// the first room that places a driving object.
	// （上游 b402882 的物件脚本子系统；本树其余字段未动。）
	actionObjects                  *runtimeDungeonActionObjectState
	ordinaryFinalRoomClearAccepted bool
	tutorialFinalRoomClearPending  bool
	tutorialFinalRoomClearAccepted bool
	// Clear-card gold is driven by how much of the dungeon was actually
	// killed, so the run tallies its own deaths as they are accounted. The
	// weighting (champion x2, boss x4) belongs to the gold formula, not here.
	clearKillNormal   int64
	clearKillChampion int64
	clearKillBoss     int64
	// colorlessSpent 本次副本运行里"技能消耗的无色小晶块"数量（op18 DELETE_ITEM 累计）。
	// 只统计技能消耗：晶体契约每 30 秒自动扣的那 1 个不走 op18，不会计入。
	// 用途：判断"技能放了很多却一个无色都没掉"这类外挂特征（见 dungeon_colorless_spend.go）。
	colorlessSpent                    int64
	settlementPhase                   currentDungeonSettlementPhase
	settlementEntrySent               bool
	settlementEntryAt                 time.Time
	settlementPlayResultReceived      bool
	settlementPlayResultBody          []byte
	settlementPlayResultDynamicRows   uint8
	settlementPlayResultOptionalField bool
	settlementStatisticReceived       bool
	settlementStatisticBody           []byte
	settlementResultPlan              *currentDungeonSettlementPacketPlan
	settlementResultNoticeSent        bool
	settlementCharacterStateSent      bool
	settlementClearRewardSent         bool
	// settlementTowerRewardSent marks the tower clear window (op348/op1263) as
	// published. A tower has no ordinary card reward, so its exit readiness is
	// gated on this flag instead of settlementClearRewardSent.
	settlementTowerRewardSent         bool
	settlementTowerInventorySent      bool
	settlementTowerCountsSent         bool
	towerRewardUpdates                []currentItemListEntry
	settlementLuckyStarResolved       bool
	settlementLuckyStarRefreshPending bool
	// settlementWeddingCapsuleSent 保证 1016 号婚姻成长胶囊进度行每次结算只发一次
	// （与 TowerReward 系列同样的"一次性窗口"语义）。
	settlementWeddingCapsuleSent      bool
	settlementLuckyStarBefore         uint32
	settlementLuckyStarApplied        uint32
	settlementLuckyStarAfter          uint32
	settlementLuckyStarLimit          uint32
	settlementLuckyStarNoticeSent     bool
	settlementLuckyStarStateSent      bool
	settlementCardScrollStateSent     bool
	settlementCardRightStateSent      bool
	settlementCardLayoutSent          bool
	settlementCardSelectionKnown      bool
	settlementCardSelected            byte
	settlementCardSelectionSent       bool
	settlementCardRewardCommitted     bool
	settlementCardSideSelectionKnown  [dungeonCardSideCount]bool
	settlementCardSideMember          [dungeonCardSideCount]byte
	settlementCardSideSelectionSent   [dungeonCardSideCount]bool
	settlementCardSideRewardCommitted [dungeonCardSideCount]bool
	settlementCardSidePosition        [dungeonCardSideCount]byte
	settlementCardSidePositionKnown   [dungeonCardSideCount]bool
	partyCardSelectionSeen            [dungeonCardSideCount][dungeonCardSlotsPerSide]bool
	partyCardSelectionPending         [][]byte
	settlementCardAutoFlipGeneration  uint64
	settlementCardAutoFlipTimerName   string
	settlementCardExitAckSent         bool
	settlementCardRewardState         *dungeonCardState
	// settlementPaidCardCost is the gold-card commission the current EXE draws
	// under 黄金卡牌 N. The 86JP reference fills the same op35 field from its
	// own [gold card cost table] lookup (ClearRewardGenerator.GetPaidCardCost ->
	// DungeonNotificationBuilder "PAID-CARD COMMISSION"), so a zero here is what
	// makes every gold card render as 0 金币.
	settlementPaidCardCost                 uint32
	settlementMonsterExperienceTotal       uint32
	settlementMonsterGrowthContractBonus   uint32
	settlementMonsterEquipmentBonus        uint32
	settlementMonsterAttendanceMarbleBonus uint32
	// settlementMonsterWelcomeBonus 累计本局怪物的"回归冒险家欢迎经验"，
	// settlementClearWelcomeBonus 是通关经验的同一项。后者用指针是为了区分
	// "还没算过"（nil，需要现算并在重试时钉住）与"算过且为 0"（合法值）。
	settlementMonsterWelcomeBonus uint32
	settlementClearWelcomeBonus   *uint32
	// settlementClearExperiencePresentation 冻结"这一次通关的经验分类"（Base 四格 +
	// Bonus 全表）。出包计划可能重试，而重试之间**资格会变**（例如回归/幸运角色的
	// 有效期到期、队伍解散），重算会给出与已提交回执不同的分类，面板数字就和真实
	// 入账对不上了。所以第一次算完就钉在这里，后续重试直接复用。
	settlementClearExperiencePresentation *currentDungeonClearExperiencePresentation
	// settlementMonsterLuckyCharacterBonus 累计本局"幸运角色"的怪物经验加成。
	settlementMonsterLuckyCharacterBonus uint32
	// 签到弹珠（onlineevent marble）的增益位掩码。进图时一次性从角色状态读取并
	// 缓存在这里，避免每只怪都去查角色表；bit0..3 对应 buff 29..32。
	attendanceMarbleBuffs                byte
	attendanceMarbleBuffsLoaded          bool
	attendanceMarbleReminderSent         bool
	settlementMonsterFatigueBatteryBonus uint32
	settlementChampionExperience         uint32
	settlementSuperChampionExperience    uint32
	settlementBossExperience             uint32
	// The current result reader resolves the dynamic op35 rows by the
	// monster object key and uses the value as that kill's awarded EXP.
	// Keep this presentation receipt separate from the classified totals.
	settlementMonsterExperienceByObjectKey map[uint32]uint32
	// Main-quest result-window values are presentation-only. Freeze them before
	// the settlement commit so a packet-plan retry cannot recalculate against
	// the already-advanced character level.
	settlementStoryQuestBaseResolved     bool
	settlementStoryQuestBase             uint32
	settlementStoryQuestTotal            uint32
	tutorialCompletionPersisted          bool
	clearMapCompletionPhaseAPersisted    bool
	clearMapCompletionKey                string
	clearMapCompletionAt                 time.Time
	clearMapCompletionQuestIDs           []int64
	clearMapCompletionCompletedQuestIDs  []int64
	clearMapCompletionPendingQuestIDs    []int64
	clearMapCompletionActiveSnapshotSent bool
	clearMapCompletionNotificationClosed bool
	tutorialCompletedReentry             bool
	tutorialCompletedReentryExitSent     bool
	tutorialFinalFlagAckSent             bool
	tutorialInitialUserStateSent         bool
	townReturnPending                    bool
	townReturnOp24Sent                   bool
	townReturnPartyDetached              bool
	// townReturnKeepParty marks a WHOLE-party exit (a raid objective closing a
	// gate, or a squad leader leading the squad home from the 战况表). The
	// interrupted-run route detaches a member who leaves alone; a whole-party
	// exit must keep the squad together.
	townReturnKeepParty bool
	// townReturnKeepsSquadLabel marks a return that must NOT clear the member's
	// Fiend War squad label even though it is not a whole-party exit.
	//
	// It exists for the returns the SERVER drives while the squad is meant to stay
	// together: the failure tail that puts everybody back in the waiting room, and
	// the auto-return a cleared area performs. Neither is 「队内成员单独自己回城」
	// (the operator's wording for the gesture that clears a 编制), and the failure
	// tail must keep every squad label by ruling
	// 「攻坚失败后 … 攻坚队不应该被解散」. Only Fiend War sets it, and
	// currentDungeonTownReturnClearsRaidSquad double-gates it on the channel, so
	// Anton and Luke are untouched. See raid_squad_sync.go for the rule.
	townReturnKeepsSquadLabel bool
	// townReturnWeaknessExemption remembers which rule spared (or did not
	// spare) the forced exit so the premium refresh a spent 魔王契约 needs can
	// be published after the town player state exists, not before it.
	townReturnWeaknessExemption currentWeaknessExemption
	// An interrupted return arms the current dungeon actor's native item-use
	// wait byte before C2S op42/op132 is emitted.  Clear that exact actor before
	// any town mode0/mode1 packet can replace the client's active object.
	townReturnDungeonGateResetSent bool
	consumableUseGateResetSent     bool
	townReturnRequestMsgID         uint16
	townReturnSource               string
	townReturnOrigin               currentTownPositionSnapshot
	townReturnTransition           currentDungeonTownTransition
	lifecycleToken                 uint64
	lifecycleState                 currentDungeonRuntimeLifecycle
	lifecycleEndReason             string
	raidDungeon                    runtimeRaidDungeonBinding
	// raidSingleMonsterLevel is set only for Fiend War single mode. The
	// client derives in-room monster stats from the actor level carried by the
	// start-map packet, so this is the server-side difficulty control.
	raidSingleMonsterLevel uint8
	// raidNativeSettlementFinished is committed only after the current EXE has
	// received the final single-Raid state=6 notification.  The native Raid
	// movie/two-card flow deliberately suppresses ordinary dungeon op31/op35,
	// so its F12/op72 exit must not depend on settlementClearRewardSent.
	raidNativeSettlementFinished bool
	deathReturnWaiting           bool
	deathReturnGeneration        uint64
	deathReturnTimerName         string
	deathReturnDueAt             time.Time
	deathReturnTransition        currentDungeonTownTransition
	deathReviveCommitted         bool
	deathReviveGeneration        uint64
	deathReviveTargetObjectKey   uint16
	deathReviveConsume           currentDungeonReviveCoinConsumeResult
	// fiendWarReviveCount 是超时空一次副本尝试里已用掉的复活币数。它按
	// (副本, 座位) 共享 —— 一个小队一起打一个区域，额度是整队共用的 ——
	// 所以小队成员各自的副本实例读的是同一个值 (见 dungeon_death_return.go)。
	fiendWarReviveCount                  int64
	forgottenLandTimerActive             bool
	forgottenLandTimerGeneration         uint64
	forgottenLandTimerName               string
	forgottenLandDeadline                time.Time
	forgottenLandTownTransition          currentDungeonTownTransition
	crystalContractTimerActive           bool
	crystalContractTimerGeneration       uint64
	crystalContractTimerName             string
	crystalContractTimerDueAt            time.Time
	altar                                *currentDungeonAltarRuntime
	actorPositionX                       uint16
	actorPositionY                       uint16
	actorPositionValid                   bool
	settlementDungeonPermissionSent      bool
	tournamentClearRewardSent            bool
	tournamentRewardExperience           uint32
	tournamentRewardGold                 uint32
	tournamentRewardAutoSelectGeneration uint64
	tournamentRewardAutoSelectTimerName  string
	suspiciousVillageElevator            currentDungeonElevatorState
	tournament                           *worldmap.TournamentRuntime
	payloadReleased                      bool

	// settlementFatigueExhaustedSlots is the party's "who has run out of fatigue"
	// snapshot, indexed by party slot.
	//
	// It is taken by the op70 entry point BEFORE it takes session.dungeon.mu,
	// because collecting it needs every member's own session lock and this codebase
	// only ever holds one session lock at a time. currentDungeonCardLayoutSlots
	// turns a true slot into the 0 that the client reads as 「玩家或某队员的疲劳值为0，
	// 无法再挑战」(dstr 475) and as the disabled 再次挑战 button. Only runs that
	// actually charge fatigue fill it in; see currentDungeonRetryGatesOnFatigue.
	settlementFatigueExhaustedSlots [dungeonCardSlotsPerSide]bool

	// --- 公会贡献度 (guild contribution) state, from the upstream guild branch. ---
	//
	// guildContributionEntry is the guild-membership snapshot taken at dungeon
	// ENTRY. The contribution rules pay out per clear, but they must be judged
	// against the guild as it was when the run started: a member who joins or
	// leaves mid-run must not change who the clear credits, and the settlement
	// re-checks this snapshot against the live registry (Registry.
	// ContributionEligible) before paying anything.
	//
	// guildContributionUpdates carries the item rows the delivered 贡献币 changed,
	// so the settlement can replay them as one op14 batch instead of re-reading
	// the inventory.
	//
	// Deliberately NOT modelled here: upstream's guildDungeonID / guildDungeonCycle
	// / guildDungeonRewardUpdates / guildDungeonStateSent / guildDungeonFatiguePaid,
	// and the guild war's key/instance/state trio. This tree identifies a 公会地下城
	// by looking its dungeon id back up in the catalog rather than by remembering a
	// flag (see wedding_ceremony.go currentWeddingGuildDungeon, which records that
	// decision and is what the wedding feature already depends on), and our 公会地下城
	// is the weekly gauge design in modules/dnf/guild/dungeon.go, not upstream's
	// monthly DungeonCycle. guildWarID is kept only as the "this run is a 公会战 run"
	// gate that the contribution rules read; the rest of the war state lands with
	// the 公会战 step.
	guildContributionEntry   dnfguild.ContributionEntry
	guildContributionUpdates []currentItemListEntry
	guildContributionSent    bool
	guildWarID               uint32
	// guildWarKey is the 公会战 battle key this run belongs to, captured at entry and
	// re-checked by the war timer so a stale timer cannot clear a newer battle.
	guildWarKey string
}

// releaseDungeonRuntimePayload drops data that is only needed while a run is
// active. The session and lifecycle fields remain available for existing
// acknowledgement and retry paths after the runtime owner is detached.
func releaseDungeonRuntimePayload(runtime *runtimeDungeonState) {
	if runtime == nil || runtime.payloadReleased {
		return
	}
	runtime.payloadReleased = true
	runtime.Rooms = nil
	runtime.LayerChains = nil
	runtime.StoryStages = nil
	runtime.RandomizedObjectSelections = nil
	runtime.RandomizedObjectGroups = nil
	runtime.RidableObjects = nil
	runtime.DropOwner = nil
	runtime.Room = nil
	runtime.HellParty = nil
	runtime.partyDungeonOwner = nil
	runtime.altar = nil
	runtime.tournament = nil
	runtime.clearKillNormal = 0
	runtime.clearKillChampion = 0
	runtime.clearKillBoss = 0
	runtime.settlementPlayResultBody = nil
	runtime.settlementStatisticBody = nil
	runtime.settlementResultPlan = nil
	runtime.partyCardSelectionPending = nil
	runtime.settlementCardRewardState = nil
	runtime.settlementMonsterExperienceByObjectKey = nil
	runtime.clearMapCompletionQuestIDs = nil
	runtime.clearMapCompletionCompletedQuestIDs = nil
	runtime.clearMapCompletionPendingQuestIDs = nil
	dungeonRuntimePayloadReleaseCount.Add(1)
}

// retireCurrentDungeonRuntimeLocked releases every session-owned part of one
// dungeon attempt after the caller has committed its lifecycle result.  The
// caller must hold session.dungeon.mu and must publish any Raid/party terminal
// event before calling this helper.
func (s *Service) retireCurrentDungeonRuntimeLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	reason string,
) {
	if session == nil {
		return
	}
	if runtime != nil {
		// Match S4A21's compare-and-detach lifecycle: make the generation
		// unreachable from the live session before cancelling its owned work.
		// A callback already waiting on dungeon.mu will therefore observe a
		// different owner even if cancelling its queue item races with dequeue.
		if session.dungeon.runtime == runtime {
			session.dungeon.runtime = nil
			session.dungeon.runToken = 0
		}
		if runtime.lifecycleState != currentDungeonRuntimeRetired {
			runtime.lifecycleState = currentDungeonRuntimeRetired
			runtime.lifecycleEndReason = strings.TrimSpace(reason)
			// A retiring member must not hold the rest of the party at the
			// op35 reward barrier: the departed slot is dropped from the
			// expected set so the freeze can still complete for everyone else.
			s.detachRuntimePartyDungeonCardRewardsLocked(session, runtime)
			// 这次副本到此结束（返回城镇/死亡回城/被下一次选本顶掉……），
			// 记一条"离开副本"。这个分支对每个 runtime 只会进入一次，
			// 所以不需要额外的去重标志。
			s.recordDungeonExit(session, runtime, reason)
		}
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, reason)
		s.cancelCurrentForgottenLandTimerLocked(session, runtime, reason)
		s.cancelCurrentCrystalContractTimerLocked(session, runtime, reason)
		s.cancelCurrentDungeonAltarTimersLocked(runtime)
		s.cancelCurrentDungeonCardAutoFlipLocked(session, runtime, reason)
		s.cancelCurrentTournamentRewardAutoSelectLocked(session, runtime, reason)
		releaseDungeonRuntimePayload(runtime)
	} else if session.dungeon.runtime == nil {
		// Heal a detached token left by an interrupted/legacy boundary even
		// when no worldmap runtime remains to identify it.
		session.dungeon.runToken = 0
	}
	// These two owners are session-local dungeon helpers rather than part of the
	// worldmap runtime.  They must not survive a force return, a completed
	// return, or a later fresh dungeon generation.
	session.dungeon.deathTower = nil
	session.dungeon.debugLightningPending = false
}

func dungeonRuntimeOwnsCharacter(runtime *runtimeDungeonState, characterID uint16) bool {
	return runtime != nil &&
		dnfdungeon.RuntimeOwnsCharacter(runtime.Character.CharacterID, characterID)
}

func (s *Service) preloadDungeonWorldMap(ctx context.Context) error {
	if s == nil {
		return errDungeonWorldMapUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return fmt.Errorf("preload dungeon worldmap pvf: %w", err)
	}
	table, err := worldmap.LoadSource(ctx, archive, worldmap.Options{CompactRuntime: true})
	if err != nil {
		return fmt.Errorf("preload dungeon worldmap table: %w", err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		return fmt.Errorf("preload dungeon worldmap resolver: %w", err)
	}
	monsterTable, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		return fmt.Errorf("preload dungeon monster table: %w", err)
	}
	if _, err := s.currentDungeonAdmissionReceiptNonce(); err != nil {
		return fmt.Errorf("preload dungeon admission receipt owner: %w", err)
	}
	admissionCatalog, err := dnfdungeonadmission.LoadCatalog(archive)
	if err != nil {
		return fmt.Errorf("preload dungeon admission counter catalog: %w", err)
	}
	// Death packets are sent only after drop planning completes.  Resolve all
	// lazy drop assets before listeners open so the first kill cannot block on
	// PVF parsing and item-pool construction.
	if _, err := currentDungeonMonsterDropRules(monsterTable); err != nil {
		return fmt.Errorf("preload dungeon drop rules: %w", err)
	}
	if _, err := currentDungeonGoldReferences(monsterTable); err != nil {
		return fmt.Errorf("preload dungeon gold references: %w", err)
	}
	if _, err := currentDungeonDimensionDrops(monsterTable); err != nil {
		return fmt.Errorf("preload dungeon dimension drops: %w", err)
	}
	if _, err := currentDungeonSpecialDrops(monsterTable); err != nil {
		return fmt.Errorf("preload dungeon special drops: %w", err)
	}
	dropCatalog, err := monsterTable.DropCatalog()
	if err != nil {
		return fmt.Errorf("preload dungeon drop catalog: %w", err)
	}
	if err := dropCatalog.ensureGenericPools(); err != nil {
		return fmt.Errorf("preload dungeon generic drop pools: %w", err)
	}
	aiCharacterTable, err := newPVFDungeonAICharacterCatalog(archive)
	if err != nil {
		return fmt.Errorf("preload dungeon AI character table: %w", err)
	}
	tutorialScripts, err := newPVFDungeonTutorialScriptCatalog(archive)
	if err != nil {
		return fmt.Errorf("preload dungeon tutorial script table: %w", err)
	}
	tutorialScripts.indexBasicActionDestroyTargets(archive, table)

	s.worldMapMu.Lock()
	s.worldMapTable = table
	s.worldMapResolver = resolver
	s.dungeonAdmissionCatalog = admissionCatalog
	s.dungeonMonsterTable = monsterTable
	s.dungeonAICharacterTable = aiCharacterTable
	s.dungeonTutorialScripts = tutorialScripts
	s.worldMapMu.Unlock()
	snapshot := table.Snapshot()
	tutorialSnapshot := tutorialScripts.Snapshot()
	s.logPacketEvent("dnf-dungeon-worldmap-loaded",
		"maps", snapshot.Maps,
		"areas", snapshot.Areas,
		"dungeons", snapshot.Dungeons,
		"admission_counter_policies", len(admissionCatalog.Policies()),
		"admission_counter_evidence", len(admissionCatalog.Evidence()),
		"mazes", snapshot.Mazes,
		"dungeon_refs", snapshot.DungeonRefs,
		"monsters", monsterTable.Count(),
		"AI_characters", aiCharacterTable.Count(),
		"tutorial_cinematic_entries", tutorialSnapshot.CinematicEntries,
		"tutorial_cinematics_with_destroy_targets", tutorialSnapshot.CinematicsWithTargets,
		"tutorial_maps_with_destroy_targets", tutorialSnapshot.MapsWithTargets,
		"tutorial_monster_destroy_targets", tutorialSnapshot.MonsterTargets,
		"tutorial_cinematic_read_failures", tutorialSnapshot.ReadFailures,
		"tutorial_cinematic_parse_failures", tutorialSnapshot.ParseFailures,
		"tutorial_basic_action_entries", tutorialSnapshot.BasicActionEntries,
		"tutorial_basic_actions_with_destroy_targets", tutorialSnapshot.BasicActionsWithTargets,
		"tutorial_basic_action_maps", tutorialSnapshot.BasicActionMaps,
		"tutorial_basic_action_monster_targets", tutorialSnapshot.BasicActionTargets,
		"tutorial_basic_action_read_failures", tutorialSnapshot.BasicActionReadFailures,
		"tutorial_basic_action_parse_failures", tutorialSnapshot.BasicActionParseFailures)
	return nil
}

func (s *Service) dungeonMonsterCatalog() (*pvfDungeonMonsterCatalog, error) {
	if s == nil {
		return nil, errDungeonWorldMapUnavailable
	}
	s.worldMapMu.RLock()
	defer s.worldMapMu.RUnlock()
	if s.dungeonMonsterTable == nil {
		return nil, errDungeonMonsterCatalogUnavailable
	}
	return s.dungeonMonsterTable, nil
}

func (s *Service) dungeonAICharacterCatalog() (*pvfDungeonAICharacterCatalog, error) {
	if s == nil {
		return nil, errDungeonWorldMapUnavailable
	}
	s.worldMapMu.RLock()
	defer s.worldMapMu.RUnlock()
	if s.dungeonAICharacterTable == nil {
		return nil, errDungeonAICharacterUnavailable
	}
	return s.dungeonAICharacterTable, nil
}

func (s *Service) dungeonTutorialScriptCatalog() (*pvfDungeonTutorialScriptCatalog, bool) {
	if s == nil {
		return nil, false
	}
	s.worldMapMu.RLock()
	defer s.worldMapMu.RUnlock()
	if s.dungeonTutorialScripts == nil {
		return nil, false
	}
	return s.dungeonTutorialScripts, true
}

func (s *Service) dungeonWorldMap() (*worldmap.Table, *worldmap.Resolver, error) {
	if s == nil {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	s.worldMapMu.RLock()
	defer s.worldMapMu.RUnlock()
	if s.worldMapTable == nil || s.worldMapResolver == nil {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	return s.worldMapTable, s.worldMapResolver, nil
}

func (s *Service) prepareDungeonRuntime(
	ctx context.Context,
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
) (*runtimeDungeonState, worldmap.DungeonRoomScene, error) {
	prepared, preparedScene, prepareErr := s.prepareDungeonRuntimePlanned(ctx, session, request, nil)
	if prepareErr != nil {
		// The client answers a failed entry with nothing at all, so without this line a
		// "clicked the dungeon and nothing happened" report has no server-side trace:
		// the request decodes, the entry policy runs, a maze is picked, and then the
		// error is returned to a caller that stays silent. Measured 2026-09-19 13:09:
		// repeated select requests for dungeon 3 after 选择其他地下城 left exactly that
		// shape in the log.
		s.logGameEvent(session, "game-dungeon-select-entry-failed",
			"dungeon_id", request.DungeonID,
			"difficulty", request.Difficulty,
			"entry_option", request.EntryOption,
			"error", prepareErr,
			"source", "prepareDungeonRuntime")
	}
	return prepared, preparedScene, prepareErr
}

func (s *Service) prepareDungeonRuntimePlanned(
	ctx context.Context,
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
	partyPlan *runtimePartyDungeonEntryPlan,
) (*runtimeDungeonState, worldmap.DungeonRoomScene, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return nil, worldmap.DungeonRoomScene{}, errDungeonCharacterRequired
	}
	if request.DungeonID == 0 {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: dungeon=0", worldmap.ErrDungeonNotIndexed)
	}
	if request.Difficulty > 4 {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: got=%d max=4", errDungeonDifficultyInvalid, request.Difficulty)
	}
	requestedDungeonID := int64(request.DungeonID)
	canonicalDungeonID, antonRaidTranslated := s.resolveAntonRaidDungeonSelection(session, requestedDungeonID)
	if canonicalDungeonID <= 0 || canonicalDungeonID > int64(^uint32(0)) {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: dungeon=%d", worldmap.ErrDungeonNotIndexed, canonicalDungeonID)
	}
	request.DungeonID = uint32(canonicalDungeonID)
	antonRaidRequest := runtimeRaidModeForSession(session) != 0 && isAntonRaidEntryDungeonID(canonicalDungeonID)
	table, resolver, err := s.dungeonWorldMap()
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, err
	}
	character, err := s.dungeonCharacter(ctx, session.selectedCharacterID, session)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, err
	}
	// Both native tower selector icons may send their first-floor base dungeon.
	// Resolve that alias from the persisted floor before anything else reads the
	// dungeon, so the admission limit and the PVF route agree on the same layer.
	entryMode, entryModeSupported := currentDungeonProtocolModeForRoom(request.RuntimeState, request.RuntimeToken, canonicalDungeonID)
	if selected, found := table.FindDungeon(canonicalDungeonID); found && entryModeSupported && entryMode == currentDungeonProtocolModeNormal {
		canonicalDungeonID, err = s.resolveCurrentTowerDungeonSelection(character, selected)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
		request.DungeonID = uint32(canonicalDungeonID)
	}
	resolvedTowerDungeonID, skippedTowerDungeonIDs, err := resolveCurrentTowerOfDespairSoloDungeon(table, resolver, canonicalDungeonID)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("resolve tower of despair solo floor: %w", err)
	}
	if resolvedTowerDungeonID != canonicalDungeonID {
		s.logGameEvent(session, "game-dungeon-tower-of-despair-solo-floors-skipped",
			"requested_dungeon_id", canonicalDungeonID,
			"resolved_dungeon_id", resolvedTowerDungeonID,
			"skipped_dungeon_ids", skippedTowerDungeonIDs,
			"source", "pvf_layout_and_hostile_actor_readiness")
		canonicalDungeonID = resolvedTowerDungeonID
		request.DungeonID = uint32(canonicalDungeonID)
	}
	dungeon, ok := table.FindDungeon(canonicalDungeonID)
	if !ok {
		s.logGameEvent(session, "game-dungeon-select-pvf-miss",
			"requested_dungeon_id", requestedDungeonID,
			"resolved_anton_dungeon_id", canonicalDungeonID,
			"channel_id", session.channel.ID,
			"channel_type", session.channel.Type,
			"anton_raid", antonRaidRequest,
			"reason", "current_pvf_dungeon_not_indexed")
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: dungeon=%d", worldmap.ErrDungeonNotIndexed, canonicalDungeonID)
	}
	// Anton raid entrances share the ordinary SelectDungeon packet, but their
	// authorization comes from the live raid roster rather than PVF level and
	// fatigue metadata. Keep this check before the generic policy so a type-23
	// channel cannot open a raid map merely because its worldmap entry exists.
	//
	// partyPlan != nil 恰好等于"这一次是**队友**、由队长的 op16 带着走"
	// （prepareRuntimePartyDungeonEntry 是唯一传 plan 的调用方）。超时空那条门禁
	// 需要知道这个区别：队友按设计留在原格，拿"他自己/他自己的小队在哪个区域"去比
	// 目标区域永远对不上，而这正是规格 D「与小队编制无关」要放行的情况。
	// 安东/卢克不受影响 —— 那条分支逐字未动。
	if err := s.validateAntonRaidDungeonEntryForEntry(session, dungeon.ID, partyPlan != nil); err != nil {
		return nil, worldmap.DungeonRoomScene{}, err
	}
	character, err = s.dungeonCharacter(ctx, session.selectedCharacterID, session)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, err
	}
	protocolMode, modeSupported := currentDungeonProtocolModeForRoom(
		request.RuntimeState, request.RuntimeToken, dungeon.ID,
	)
	if !modeSupported {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: runtime_state=%d runtime_token=%d", errDungeonEntryModeUnsupported, request.RuntimeState, request.RuntimeToken)
	}
	if protocolMode == currentDungeonProtocolModePractice {
		if err := validatePracticeDungeonRoute(dungeon); err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
	}
	if !antonRaidRequest {
		if err := validateDungeonEntry(character, dungeon, session, protocolMode == currentDungeonProtocolModePractice); err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
	}
	s.logGameEvent(session, "game-dungeon-select-entry-policy",
		"requested_dungeon_id", requestedDungeonID,
		"resolved_anton_dungeon_id", dungeon.ID,
		"anton_raid_selector_translated", antonRaidTranslated,
		"channel_id", session.channel.ID,
		"channel_type", session.channel.Type,
		"anton_raid", antonRaidRequest,
		"ordinary_policy_skipped", antonRaidRequest,
		"raid_started", antonRaidRequest)
	questRecord := dnfrepo.QuestRecord{}
	questRecordFound := false
	if protocolMode != currentDungeonProtocolModePractice {
		questRecord, questRecordFound, err = s.dungeonQuestRecord(ctx, session.selectedCharacterID)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
		if reconciled, reconcileErr := s.reconcileActiveQuestClearLinkedSubQuestsForDungeon(ctx, session, "select_dungeon_before_maze_selection"); reconcileErr != nil {
			s.logGameEvent(session, "game-dungeon-quest-clear-linked-subquest-reconcile-failed",
				"dungeon_id", request.DungeonID,
				"difficulty", request.Difficulty,
				"source", "select_dungeon_before_maze_selection",
				"error", reconcileErr)
		} else if reconciled {
			questRecord, questRecordFound, err = s.dungeonQuestRecord(ctx, session.selectedCharacterID)
			if err != nil {
				return nil, worldmap.DungeonRoomScene{}, err
			}
		}
	}
	if err := authorizeCurrentInfernoDungeon(dungeon, questRecord); err != nil {
		s.logGameEvent(session, "game-dungeon-inferno-entry-blocked",
			"dungeon_id", dungeon.ID,
			"required_quest_ids", dungeon.Metadata.InfernoQuestIDs,
			"quest_record_found", questRecordFound,
			"reason", "pvf_inferno_challenge_quest_not_completed")
		return nil, worldmap.DungeonRoomScene{}, err
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime != nil && session.dungeon.runtime.Session != nil {
		snapshot := session.dungeon.runtime.Session.Snapshot().Run
		if snapshot.Status == worldmap.DungeonRunActive &&
			!s.runtimeRaidHatcheryTransferAllowedLocked(session, session.dungeon.runtime, dungeon.ID) {
			session.dungeon.mu.Unlock()
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("%w: dungeon=%d maze=%d", errDungeonAlreadyActive, snapshot.DungeonID, snapshot.MazeIndex)
		}
	}
	session.dungeon.mu.Unlock()

	mazeSelection := dungeonMazeSelection{}
	if partyPlan != nil {
		if partyPlan.dungeonID != dungeon.ID || partyPlan.mazeIndex < 0 || partyPlan.mazeIndex >= len(dungeon.Mazes) {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("party leader dungeon selection is invalid: dungeon=%d maze=%d", partyPlan.dungeonID, partyPlan.mazeIndex)
		}
		mazeSelection.Index = partyPlan.mazeIndex
		mazeSelection.Reason = "ordinary_party_leader_frozen_selection"
	} else if isCurrentFiendWarRaidChannel(session) {
		// 超时空的迷宫不是随机的：区域里站着哪个兵种就进对应的那张图
		// (魅魔/融合/破坏/合金 四种敌军 + 两种兽人)，所以选择由战场状态决定。
		mazeSelection, err = s.selectRuntimeFiendWarMaze(session.selectedCharacterID, dungeon)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
	} else if protocolMode == currentDungeonProtocolModePractice {
		mazeSelection, err = selectPracticeDungeonMaze(dungeon.Mazes, s.chooseDungeonIndex)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("choose practice dungeon maze: %w", err)
		}
	} else if lukeMaze, luke, lukeErr := s.selectRuntimeRaidLukeSanctumMaze(session, dungeon.ID, len(dungeon.Mazes)); lukeErr != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("choose luke sanctum maze: %w", lukeErr)
	} else if luke {
		// 卢克一阶段的圣所按它抽到的 [CARD SET MAP INFO] 组走，一次通关走组内一个迷宫
		// （raid_luke_sanctum_maze.go）。不是卢克圣所时 luke==false，落到下面原来的分支。
		mazeSelection = lukeMaze
	} else {
		var err error
		mazeSelection, err = selectDungeonMaze(dungeon.Mazes, request.Difficulty, questRecord, s.chooseDungeonIndex)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("choose dungeon maze: %w", err)
		}
	}
	mazeIndex := mazeSelection.Index
	explicitHellParty := currentDungeonExplicitHellPartyRequest(request, dungeon, mazeIndex)
	s.logGameEvent(session, "game-dungeon-maze-selected",
		"dungeon_id", request.DungeonID,
		"difficulty", request.Difficulty,
		"maze_index", mazeIndex,
		"selection_reason", mazeSelection.Reason,
		"quest_id", mazeSelection.QuestID,
		"quest_record_found", questRecordFound,
		"body_source", "runtime_pvf_quest_connection_and_persisted_quest_state")
	if explicitHellParty {
		if err := s.authorizeCurrentDungeonHellParty(ctx, session, table, dungeon.ID); err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
	}
	chooser := func(choice worldmap.DungeonMapChoice) (int64, error) {
		if partyPlan != nil {
			mapID, ok := partyPlan.maps[choice.Coordinate]
			if !ok {
				return 0, fmt.Errorf("party leader has no frozen map for room %s", choice.Coordinate)
			}
			for _, candidate := range choice.Candidates {
				if candidate.ID == mapID {
					return mapID, nil
				}
			}
			return 0, fmt.Errorf("party leader map %d is not a candidate for room %s", mapID, choice.Coordinate)
		}
		index, chooseErr := s.chooseDungeonIndex(len(choice.Candidates))
		if chooseErr != nil {
			return 0, chooseErr
		}
		return choice.Candidates[index].ID, nil
	}
	topology, err := worldmap.BuildDungeonLayout(resolver, dungeon.ID, mazeIndex, chooser)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("build dungeon layout: %w", err)
	}
	storyStages, storyErr := resolver.StoryStages(dungeon.ID, mazeIndex)
	if storyErr != nil && !errors.Is(storyErr, worldmap.ErrStoryStageMissing) {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("resolve dungeon story stages: %w", storyErr)
	}
	var bossCoordinate worldmap.RoomCoordinate
	if len(storyStages) != 0 {
		bossCoordinate = storyStages[len(storyStages)-1].Coordinate
	} else {
		bossIndex := 0
		if partyPlan != nil {
			bossIndex = -1
			for index, coordinate := range topology.Bosses {
				if coordinate == partyPlan.boss {
					bossIndex = index
					break
				}
			}
			if bossIndex < 0 {
				return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("party leader boss room %s is not in follower topology", partyPlan.boss)
			}
		} else {
			var err error
			bossIndex, err = s.chooseDungeonIndex(len(topology.Bosses))
			if err != nil {
				return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("choose dungeon boss coordinate: %w", err)
			}
		}
		bossCoordinate = topology.Bosses[bossIndex]
	}
	seed := uint32(0)
	if partyPlan != nil {
		seed = partyPlan.seed
	} else {
		var seedErr error
		seed, seedErr = s.chooseDungeonSeed()
		if seedErr != nil {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon room seed: %w", seedErr)
		}
	}
	var hellPartyOwner *currentDungeonHellPartyOwner
	if explicitHellParty {
		hellPartyOwner, err = s.prepareCurrentDungeonHellParty(table, topology, dungeon, mazeIndex, seed)
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("prepare hell party runtime: %w", err)
		}
	}
	s.logGameEvent(session, "game-dungeon-monster-rank-policy",
		"dungeon_id", dungeon.ID,
		"difficulty", request.Difficulty,
		"maze_index", mazeIndex,
		"source", "pvf_map_authored_ranks_only")
	run, err := worldmap.NewDungeonRun(topology)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon run: %w", err)
	}
	dungeonSession, err := worldmap.NewDungeonSession(run)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon session: %w", err)
	}
	scene, ok := dungeonSession.Scene()
	if !ok {
		return nil, worldmap.DungeonRoomScene{}, errors.New("created dungeon session has no current scene")
	}
	monsterCatalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, err
	}
	room, nextObjectKey, err := newRuntimeDungeonRoom(
		scene,
		monsterCatalog,
		firstDungeonMonsterObjectKey,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon monster owner: %w", err)
	}
	var aiCharacterCatalog *pvfDungeonAICharacterCatalog
	if len(scene.AICharacters) != 0 {
		aiCharacterCatalog, err = s.dungeonAICharacterCatalog()
		if err != nil {
			return nil, worldmap.DungeonRoomScene{}, err
		}
	}
	extendedPlan, err := planRuntimeDungeonExtendedActors(
		scene,
		monsterCatalog,
		aiCharacterCatalog,
		dungeon.Metadata.BasisLevel,
		nextObjectKey,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon extended actor owner: %w", err)
	}
	randomizedSelections, randomizedGroups, randomizedNextObjectKey, randomizedSkipped, err := planCurrentDungeonRandomizedObjects(
		dungeon.Mazes[mazeIndex].RandomizedObjects,
		seed,
		extendedPlan.NextObjectKey,
		dungeon.Metadata.BasisLevel,
		monsterCatalog,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("create dungeon randomized object owner: %w", err)
	}
	if len(randomizedSkipped) != 0 {
		// The map still loads; the unsupported actors simply are not announced.
		// EnergyCut (Anton phase two 218) depends on this: every candidate of its
		// first group is a passive object with team "monster", which used to fail
		// the whole gate closed.
		details := make([]string, 0, len(randomizedSkipped))
		for _, skip := range randomizedSkipped {
			details = append(details, fmt.Sprintf("g%d/c%d/def%d/family%d/team%s",
				skip.GroupIndex, skip.CandidateIndex, skip.DefinitionID, skip.Family, skip.Team))
		}
		s.logPacketEvent("game-dungeon-randomized-object-skipped",
			"dungeon_id", dungeon.ID,
			"maze_index", mazeIndex,
			"skipped_count", len(randomizedSkipped),
			"skipped", strings.Join(details, ","),
			"reason", "unsupported_randomized_object_kind_not_announced")
	}
	extendedPlan.NextObjectKey = randomizedNextObjectKey
	extendedPlan, err = s.appendCurrentDungeonHellPartyActors(
		extendedPlan,
		hellPartyOwner,
		scene,
		dungeon.Metadata.BasisLevel,
		monsterCatalog,
		aiCharacterCatalog,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("project hell party actors: %w", err)
	}
	if _, err := s.configurePVFTutorialBasicActionRoom(dungeon, scene, room); err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("configure tutorial basic-action room: %w", err)
	}
	tournament, nextObjectKey, err := s.prepareCurrentTournamentRuntime(
		table, dungeon, scene, extendedPlan.NextObjectKey,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("prepare tournament runtime: %w", err)
	}
	if err := appendCurrentTournamentActors(&extendedPlan, tournament); err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("append tournament actors: %w", err)
	}
	if tournament != nil {
		nextObjectKey = extendedPlan.NextObjectKey
	}
	ridableObjects, ridableEntries, ridableNextObjectKey, err := planCurrentDungeonStaticRidableObjects(
		dungeonSession.Rooms(), nextObjectKey,
	)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("prepare dungeon static ridable objects: %w", err)
	}
	if len(ridableEntries) != 0 {
		if len(randomizedGroups) >= int(^uint8(0)) {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf(
				"%w: randomized_groups=%d static_ridable_groups=1",
				errDungeonStartMapGroupCount, len(randomizedGroups),
			)
		}
		randomizedGroups = append(randomizedGroups, ridableEntries)
		nextObjectKey = ridableNextObjectKey
	}
	if err := room.AttachExtendedActors(extendedPlan); err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("attach dungeon extended actor owner: %w", err)
	}
	clientDungeonID := dungeon.ID
	if projectedDungeonID, projected := s.projectSingleAntonRaidClientDungeonID(session, dungeon.ID); projected {
		clientDungeonID = projectedDungeonID
	}
	altar, err := newCurrentDungeonAltarRuntime(dungeon, scene)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("prepare blood altar runtime: %w", err)
	}
	if altar != nil {
		if err := altar.configureRunSeed(seed); err != nil {
			return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("prepare blood altar special round: %w", err)
		}
	}
	runtime := &runtimeDungeonState{
		Request: request, ProtocolMode: protocolMode, Dungeon: dungeon, MazeIndex: mazeIndex,
		clientDungeonID: clientDungeonID,
		Character:       character, Session: dungeonSession, Room: room, HellParty: hellPartyOwner, NextObjectKey: nextObjectKey,
		BossCoordinate: bossCoordinate, BossSet: true,
		entryCharacterLevel: character.Level,
		LayeredMapIndex:     -1, StoryStageIndex: -1, Seed: seed,
		RandomizedObjectSelections: randomizedSelections,
		RandomizedObjectGroups:     randomizedGroups,
		RandomizedObjectsPlanned:   true,
		RidableObjects:             ridableObjects,
		LayerChains:                make(map[worldmap.RoomCoordinate]*runtimeDungeonLayerChain),
		StoryStages:                append([]worldmap.DungeonStoryStage(nil), storyStages...),
		Rooms:                      make(map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit),
		startedAt:                  s.gameplayNow(),
		tournament:                 tournament,
		altar:                      altar,
		raidSingleMonsterLevel:     s.runtimeFiendWarSingleMonsterLevel(session),
	}
	// 公会贡献度 is judged against the guild as it was when the run STARTED, so the
	// membership snapshot is taken here and the settlement re-checks it against the
	// live registry (Registry.ContributionEligible) before paying anything. Taking
	// it at settlement time instead would credit a member who joined mid-run and
	// drop one who left.
	runtime.guildContributionEntry, err = s.captureCurrentGuildContributionEntry(ctx, character.CharacterID)
	if err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("capture guild contribution membership: %w", err)
	}
	if _, err := s.applyPVFTutorialBasicActionRoomToSession(
		session,
		runtime,
		scene,
		"initial_room_before_start_map",
	); err != nil {
		return nil, worldmap.DungeonRoomScene{}, fmt.Errorf("apply tutorial basic-action initial room: %w", err)
	}
	scene, _ = dungeonSession.Scene()
	if isPVFTutorialDungeon(runtime) && hasPersistedDungeonTutorialCompletion(character) {
		runtime.tutorialCompletionPersisted = true
		runtime.tutorialCompletedReentry = true
	}
	runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)] = &runtimeDungeonRoomVisit{
		Scene:   scene,
		Room:    room,
		Seed:    seed,
		DropRNG: seed,
	}
	return runtime, scene, nil
}

func authorizeCurrentInfernoDungeon(dungeon worldmap.Dungeon, record dnfrepo.QuestRecord) error {
	if !dungeon.Metadata.InfernoDungeon {
		return nil
	}
	for _, questID := range dungeon.Metadata.InfernoQuestIDs {
		if dnfquest.IsCompleted(record, questID) {
			return nil
		}
	}
	return errDungeonInfernoLocked
}

func (s *Service) commitDungeonRuntime(session *gameSession, runtime *runtimeDungeonState) error {
	if session == nil || runtime == nil || runtime.Session == nil {
		return errDungeonWorldMapUnavailable
	}
	if err := validateCurrentDungeonRuntimeTownReturnOrigin(session, runtime); err != nil {
		return err
	}
	session.dungeon.mu.Lock()
	if runtime.payloadReleased || runtime.lifecycleState == currentDungeonRuntimeRetired {
		session.dungeon.mu.Unlock()
		return fmt.Errorf("%w: dungeon=%d maze=%d reason=%s",
			errDungeonRuntimeRetired,
			runtime.Dungeon.ID,
			runtime.MazeIndex,
			runtime.lifecycleEndReason,
		)
	}
	previousRuntime := session.dungeon.runtime
	if previousRuntime == runtime {
		session.dungeon.mu.Unlock()
		return fmt.Errorf("%w: dungeon=%d maze=%d generation=%d",
			errDungeonAlreadyActive,
			runtime.Dungeon.ID,
			runtime.MazeIndex,
			runtime.lifecycleToken,
		)
	}
	if previousRuntime != nil && previousRuntime.Session != nil {
		snapshot := previousRuntime.Session.Snapshot().Run
		hatcheryTransfer := s.runtimeRaidHatcheryTransferAllowedLocked(session, previousRuntime, runtime.Dungeon.ID)
		if snapshot.Status == worldmap.DungeonRunActive && !hatcheryTransfer {
			session.dungeon.mu.Unlock()
			return fmt.Errorf("%w: dungeon=%d maze=%d", errDungeonAlreadyActive, snapshot.DungeonID, snapshot.MazeIndex)
		}
		if !hatcheryTransfer {
			// A warp into an open hatchery is not giving up: the energy-interception
			// attempt stays valid and only its runtime is retired below.
			if err := s.notifyRuntimeRaidDungeonAbandonedLocked(session, previousRuntime); err != nil {
				session.dungeon.mu.Unlock()
				return err
			}
		} else if err := s.publishRuntimeRaidDungeonTransferAwayLocked(session, previousRuntime); err != nil {
			// The attempt stays valid, but the client's row for the gate just left
			// must not: keeping the attempt and keeping the row were the same
			// decision here, and only the first one was meant. Without this the
			// 战况表 drew the squad inside 能量阻截战 for the rest of the phase.
			//
			// Deliberately NOT fatal, unlike the abandoned notification above: that
			// one advances the raid's state machine, this one only redraws a row. A
			// member whose socket dropped mid-broadcast must not lose the hatchery
			// it just walked into over a cosmetic update -- the worst case here is
			// the stale row this call exists to remove.
			s.logPacketEvent("game-raid-hatchery-transfer-participation-failed",
				"char_id", session.selectedCharacterID,
				"raid_key", previousRuntime.raidDungeon.RaidKey,
				"left_map_id", previousRuntime.raidDungeon.MapID,
				"entered_map_id", runtime.Dungeon.ID,
				"error", err)
		}
	}
	if previousRuntime != nil {
		s.retireCurrentDungeonRuntimeLocked(session, previousRuntime, "new_dungeon_run_replaces_previous_runtime")
	}
	// Lifecycle attempts are retained by the process-wide raid state after the
	// owning socket disconnects. A per-session counter restarts at one on every
	// login and can therefore collide with an earlier closed attempt for the same
	// character. Allocate a process-wide nonce so reconnects can retry the open
	// objective without being rejected as an already-completed attempt.
	if runtime.lifecycleToken == 0 {
		runtime.lifecycleToken = s.allocateDungeonLifecycleToken()
	}
	session.dungeon.runToken = runtime.lifecycleToken
	runtime.lifecycleState = currentDungeonRuntimeActive
	runtime.lifecycleEndReason = ""
	session.dungeon.runtime = runtime
	session.dungeon.deathTower = nil
	session.dungeon.debugLightningPending = false
	session.dungeon.mu.Unlock()
	if err := s.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		session.dungeon.mu.Lock()
		if session.dungeon.runtime == runtime {
			s.retireCurrentDungeonRuntimeLocked(session, runtime, "new_dungeon_raid_selection_freeze_failed")
		}
		session.dungeon.mu.Unlock()
		return err
	}
	// 到这里这次副本已经完整提交给会话了，记一条"进入副本"。
	// 放在 freeze 之后而不是解锁之后：freeze 失败会把运行时立刻退休，那种情况
	// 不该留下一条"进入"记录。
	s.recordDungeonEnter(session, runtime)
	return nil
}

func (s *Service) allocateDungeonLifecycleToken() uint64 {
	if s == nil {
		return 0
	}
	for {
		token := s.nextDungeonLifecycleToken.Add(1)
		if token != 0 {
			return token
		}
	}
}

func (s *Service) dungeonCharacter(ctx context.Context, characterID uint16, sessions ...*gameSession) (dnfrepo.CharacterRecord, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, dungeoncmd.ErrOwnerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	if !found {
		return dnfrepo.CharacterRecord{}, errDungeonCharacterNotFound
	}
	character = dnfrepo.CloneCharacter(character)
	accountID := strings.TrimSpace(s.accountIDForSession(sessions...))
	if stored := strings.TrimSpace(character.AccountID); stored != "" && accountID != "" && stored != accountID {
		return dnfrepo.CharacterRecord{}, fmt.Errorf("%w: selected=%s stored=%s", errDungeonAccountMismatch, accountID, stored)
	}
	return character, nil
}

func (s *Service) dungeonQuestRecord(ctx context.Context, characterID uint16) (dnfrepo.QuestRecord, bool, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return dnfrepo.QuestRecord{}, false, errDungeonQuestRepositoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	characterKey := strconv.Itoa(int(characterID))
	record, found, err := repositories.Quest.Load(ctx, characterKey)
	if err != nil {
		return dnfrepo.QuestRecord{}, false, err
	}
	if !found {
		return dnfrepo.QuestRecord{CharacterID: characterKey}, false, nil
	}
	return dnfrepo.CloneQuest(record), true, nil
}

func validateDungeonEntry(character dnfrepo.CharacterRecord, dungeon worldmap.Dungeon, session *gameSession, practice bool) error {
	partyCount := 1
	if session != nil {
		session.party.mu.Lock()
		if len(session.party.state.Members) > partyCount {
			partyCount = len(session.party.state.Members)
		}
		session.party.mu.Unlock()
	}
	partyLimit := dungeon.Metadata.LimitPartyCount.Value
	partyLimitSet := dungeon.Metadata.LimitPartyCount.Set
	// Blood/Ultimate Altar progression is intentionally session-owned. Until a
	// shared party altar runtime exists, admitting more than one member would
	// create divergent round/monster sequence owners on the fanout path. The
	// real 11006/11007 PVF lacks a party limit, so enforce this implementation
	// boundary explicitly and reuse the normal entry-party-limit rejection.
	if dungeon.ID == 11006 || dungeon.ID == 11007 || dungeon.BloodAltarKind != 0 {
		partyLimit = 1
		partyLimitSet = true
	}
	fatigue, fatigueKnown := dnfdungeon.CharacterStat(character.Stats, "fatigue", "fp", "疲劳")
	return dnfdungeon.ValidateEntry(dnfdungeon.EntryPolicy{
		CharacterLevel:      character.Level,
		MinimumLevel:        dungeon.Metadata.MinimumRequiredLevel.Value,
		MinimumLevelSet:     dungeon.Metadata.MinimumRequiredLevel.Set,
		PartyCount:          partyCount,
		PartyLimit:          partyLimit,
		PartyLimitSet:       partyLimitSet,
		NoFatigue:           currentDungeonIgnoresFatigue(dungeon) || practice,
		EnterWithoutFatigue: dungeon.Metadata.Flags["enter without fatigue"],
		Fatigue:             fatigue,
		FatigueKnown:        fatigueKnown,
	})
}

func (s *Service) chooseDungeonIndex(limit int) (int, error) {
	if limit <= 0 {
		return 0, worldmap.ErrMazeNotFound
	}
	if limit == 1 {
		return 0, nil
	}
	if s != nil && s.dungeonChoice != nil {
		index, err := s.dungeonChoice(limit)
		if err != nil {
			return 0, err
		}
		if index < 0 || index >= limit {
			return 0, fmt.Errorf("dungeon choice index %d outside [0,%d)", index, limit)
		}
		return index, nil
	}
	index, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, fmt.Errorf("choose dungeon random index: %w", err)
	}
	return int(index.Int64()), nil
}

func (s *Service) chooseDungeonSeed() (uint32, error) {
	if s != nil && s.dungeonSeed != nil {
		return s.dungeonSeed()
	}
	var data [4]byte
	if _, err := cryptorand.Read(data[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(data[:]), nil
}
