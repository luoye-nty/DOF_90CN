// service.go 负责 DNF bridge 生命周期、目录加载和监听器管理。
// 这里不写玩法状态，只装配频道服与 game 端口的 transport 边界。
package dnfbridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	runtimedebug "runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/channelinfo"
	dnfcharstat "longheng.io/server/internal/modules/dnf/charstat"
	dnfcombatpower "longheng.io/server/internal/modules/dnf/combatpower"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dproto"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfepicbook "longheng.io/server/internal/modules/dnf/epicbook"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	dnfjoust "longheng.io/server/internal/modules/dnf/joust"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/party"
	dnfpet "longheng.io/server/internal/modules/dnf/pet"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfpvp "longheng.io/server/internal/modules/dnf/pvp"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/wedding"
	"longheng.io/server/internal/modules/dnf/worldmap"
	appkit "longheng.io/server/internal/platform/app"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Service 是 DNF 最新客户端 bridge 的生命周期组件。
type Service struct {
	logger  *slog.Logger
	options options
	// launcherAuthHTTPClient performs the loopback verify-game-ticket call for
	// external game connections when launcher auth is enabled; nil otherwise.
	launcherAuthHTTPClient *http.Client
	// launcherIdentity pins the deployment's launcher. It is never nil: a
	// disabled verifier keeps every legacy account-binding path reachable.
	launcherIdentity *launcherIdentityVerifier
	// launcherIdentityErr is a launcher identity configuration error. It is
	// surfaced by Start so a misconfigured server refuses to serve rather than
	// quietly running unbound.
	launcherIdentityErr error
	// localDebugMode is supplied only by the local composition root. It is
	// intentionally process-local and never persisted with character data.
	localDebugMode             bool
	localDebugStrikeOnSkillUse bool
	// debugKillRoomHotkey enables the private "KRM1" room-clear request used by
	// the standalone LYPlugins test DLL. It is independent of localDebugMode so
	// enabling the test hotkey does not also grant the speed/rank debug profile.
	debugKillRoomHotkey   bool
	debugKillRoomInFlight int32

	// petEggProbeOnce 装配进程级的"宠物蛋"判据（见 pet_egg_item.go）；petEggCache
	// 缓存模板级判定结果，避免每次对账都重新解析 PVF 文档。
	petEggProbeOnce sync.Once
	petEggCache     sync.Map

	// chatMu 保护下面两张 1:1 会话表（见 town_chat_conversation.go）。它是服务端
	// 自己记的"这个客户端的聊天窗口里是谁"：当前客户端的聊天请求里没有可用的对端
	// 字段，对端只能由服务端维护。会话是纯内存状态，随 session 结束回收，不落盘。
	chatMu                   sync.Mutex
	chatConversations        map[uint64]*currentChatConversation
	chatConversationByMember map[uint16]*currentChatConversation
	chatConversationSeq      uint32

	mu            sync.Mutex
	catalog       *channelcatalog.Catalog
	channelScript []byte
	listeners     []net.Listener
	// gameListeners 按游戏端口登记监听，供热重载时对账（gm_channel_reload.go）。
	// 与 listeners 同步维护：listeners 仍是 Stop 的唯一清理来源，gameListeners 只提供
	// "哪个端口对应哪个 listener"的反查能力。
	gameListeners map[int]net.Listener
	// runCtx 是 Start 建立的运行期上下文，供热重载时为新启用的频道开监听。
	// 由 s.mu 保护，Stop 时置 nil。
	runCtx       context.Context
	conns        map[net.Conn]struct{}
	gameSessions map[uint16]*gameSession
	// onlineSeats tracks every authenticated game connection that currently
	// occupies an online seat, whether or not it has bound a character yet
	// (role-select / loading connections included). Guarded by s.mu. It is
	// the authoritative source for online-capacity accounting: a server at
	// its limit must reject a new launcher verify immediately instead of
	// overselling a seat to a connection that is still connecting.
	onlineSeats         map[*gameSession]struct{}
	onlineReportTrigger chan struct{}

	pvpMu                     sync.Mutex
	pvpRooms                  map[uint16]*currentPVPRoom
	pvpMatching               []*currentPVPMatchingRequest // protected by pvpMu
	nextPVPMatchingToken      uint32
	pvpSubscribers            map[*gameSession]int
	pvpMapOrder               []int64                 // protected by pvpMu
	pvpDailyCatalog           *dnfpvp.Catalog         // protected by initialEquipmentMu
	pvpTotalCatalog           *dnfpvp.Catalog         // protected by initialEquipmentMu
	pvpFatigueRules           *currentPVPFatigueRules // protected by initialEquipmentMu
	nextGameSessionGeneration uint64
	nextDungeonLifecycleToken atomic.Uint64
	nextPartyRoutingKey       atomic.Uint32
	guildMu                   sync.Mutex
	guildDungeonCatalog       currentGuildDungeonCatalogCache
	guildCompoundCatalog      currentGuildCompoundCatalogCache
	guildCompoundCandidates   currentGuildCompoundCandidates
	guildCreateMu             sync.Mutex
	friendMu                  sync.Mutex
	// weddingMu 串行化婚姻手势（求婚/应答/戒指/心意点/婚房），并保护
	// weddingInvitations。获取顺序必须是 weddingMu 先于 mu（见 game_sessions.go），
	// 与上游一致，避免会话绑定/解绑路径与婚姻事务互相等待。
	weddingMu                   sync.Mutex
	weddingInvitations          map[uint16]*currentWeddingInvitation
	weddingCatalogMu            sync.Mutex
	weddingCatalog              *wedding.Catalog
	weddingCatalogLoadErr       error
	weddingCapsuleCatalog       *wedding.CapsuleCatalog
	weddingCapsuleCatalogErr    error
	tradeMu                     sync.Mutex
	itemTrades                  map[uint16]*onlineItemTrade
	privateStoreOpMu            sync.Mutex
	privateStoreMu              sync.Mutex
	privateStores               map[uint16]*currentPrivateStore
	privateStoreVisitors        map[*gameSession]uint16
	nextPrivateStoreGeneration  atomic.Uint64
	privateStoreItemEligibility func(uint32) bool
	auctionMu                   sync.Mutex
	nextAuctionListingID        atomic.Uint64
	auctionItemEligibility      func(uint32) bool
	auctionCategoryItem         func(uint16, uint32) bool
	expertJobStoreMu            sync.Mutex
	expertJobStoreOpMu          sync.Mutex
	skillSwitchInventoryMu      sync.Mutex
	expertJobStores             map[uint16]*currentExpertJobStore
	expertJobVisitors           map[*gameSession]uint16
	raidMu                      sync.Mutex
	raidStartMu                 sync.Mutex
	raids                       map[uint32]*runtimeRaidState
	// raidPhaseCompletionRetries 是"阶段收尾重试"的预算，按 RaidKey 计：
	// 门还没关完时 finishRuntimeRaidPhase 会把自己再排一次，直到所有人都回城
	// 或预算用尽（见 scheduleRuntimeRaidPhaseCompletionRetry）。
	raidPhaseCompletionMu      sync.Mutex
	raidPhaseCompletionRetries map[uint32]int
	raidSituationValues        map[runtimeRaidMonsterRuntimeKey][]uint32
	raidBuffActivations        map[runtimeRaidBuffActivationKey]runtimeRaidBuffActivation
	raidReviveUses             map[runtimeRaidReviveCoinKey]uint32
	raidReviveGrants           map[runtimeRaidReviveCoinKey]uint32
	raidPeerAdmissions         map[runtimeRaidPeerAdmissionKey]runtimeRaidPeerAdmission
	raidDisconnects            map[uint16]runtimeRaidDisconnectReservation
	// raidPhaseDeaths 是本阶段整团的死亡次数（0x0259 的 deaths 字段），按
	// (RaidKey, Phase) 计；与 raidReviveUses 共用 raidMu。见 raid_phase_death_count.go。
	raidPhaseDeaths map[runtimeRaidPhaseDeathKey]uint32
	nextRaidSeq     uint32
	raidAntonMu     sync.Mutex
	raidAnton       *dnfraid.AntonConfig
	raidAntonErr    error
	raidLukeMu      sync.Mutex
	raidLuke        *dnfraid.LukeConfig
	raidLukeErr     error
	// raidFiendWar 是 etc/raid/fiendwar/fiendwar.etc：招募标量 + 30 区域地图 +
	// trigger/behavior 规则对 + 奖励。与 raidAnton/raidLuke 同形：一次读档、缓存，
	// 失败也缓存（缺档只报一次）。见 raid_fiendwar_runtime.go。
	raidFiendWarMu  sync.Mutex
	raidFiendWar    *dnfraid.FiendWarConfig
	raidFiendWarErr error
	// raidLukeCardSet 是 etc/raid/luke/LukeCardSet.etc（塔罗牌面的图案表 + 22 张
	// 大阿卡纳），只被 1181 的下发路径读，见 raid_luke_card_display.go。
	raidLukeCardSetMu  sync.Mutex
	raidLukeCardSet    *dnfraid.LukeCardSetConfig
	raidLukeCardSetErr error
	// raidLukeMonsterList 是 etc/raid/luke/luke.etc.buff 的 [RAID MONSTER LIST]
	// （每个迷宫真会刷的 named/boss），1181 的牌面按它推，见 raid_luke_card_display.go。
	raidLukeMonsterListMu  sync.Mutex
	raidLukeMonsterList    *dnfraid.LukeRaidMonsterListConfig
	raidLukeMonsterListErr error
	raidAntonRewardMu      sync.Mutex
	raidAntonReward        *antonRewardPVFCatalog
	raidAntonRewardErr     error
	raidLukeRewardMu       sync.Mutex
	raidLukeReward         *antonRewardPVFCatalog
	raidLukeRewardErr      error
	raidRewardOverridesMu  sync.Mutex
	raidRewardOverrides    *raidRewardOverrides
	raidRewardCommitMu     sync.Mutex
	raidRewardFlowMu       sync.Mutex
	raidRewardCompleted    map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion
	raidRewardPublished    map[runtimeRaidRewardCompletionKey]*runtimeRaidRewardPublication
	// 上面两张表的插入顺序。键含单调递增的 Token、RaidKey 永不复用，所以每条都是
	// 新键；没有这个顺序就无法在超限时淘汰最旧的一条。见 raid_reward_bounds.go。
	raidRewardCompletedOrder []runtimeRaidRewardCompletionKey
	raidRewardPublishedOrder []runtimeRaidRewardCompletionKey
	// finishedRaidLeavers 记录"攻坚已被释放后"已经点过退出的成员，按 raid key 分组：
	// 这时 s.raids 里已经没有状态，剩下的名册只能由 completion 记录减去这些人生成
	// （见 markFinishedRuntimeRaidLeaver）。
	finishedRaidLeaverMu sync.Mutex
	finishedRaidLeavers  map[uint32]map[uint16]bool
	raidRewardRandom     func() uint64
	packetLog            atomic.Pointer[packetLogger]
	// records 是三类审计记录（点券消费/对话/副本）的追加写入器，见 activity_record.go。
	// 与 packetLog 同样用 atomic：玩法主路径会在任意协程调 recordActivity，
	// 而 Start/Stop 会在生命周期线程创建/关闭它。
	records             atomic.Pointer[activityRecorder]
	partyUDPRelay       *currentPartyUDPRelay
	partyUDPProbe       *currentPartyUDPProbe
	partyEndpointMu     sync.Mutex
	partyEndpoints      currentPartyUDPEndpointRegistry
	partyUDPOwnerPID    func(netip.AddrPort) (uint32, bool)
	runtimePartyManager *party.RuntimePartyManager
	// partyPublishFormation maps a party to the token of its live formation-retry
	// epoch; see party_publish_retry.go. It has its own mutex (never s.mu) because
	// a retry callback runs from the gameplay timer queue and must not contend
	// with, or invert against, the session/party locks taken by a publish.
	partyPublishMu        sync.Mutex
	partyPublishSerial    uint64
	partyPublishFormation map[int]uint64
	// raidCost 是每场攻坚的服务端成本计数（见 raid_cost_summary.go）：慢事件、
	// 安装分片、写阻塞、中继端口占用等聚合成一行团本记录，供"收不到玩家日志"
	// 时判定这场到底卡没卡、掉没掉。它有自己的锁，绝不在 gameplay 锁内取用。
	raidCost                 runtimeRaidCostRegistry
	partyTeleportMu          sync.Mutex
	partyTeleportPending     map[int]*currentPartyTeleportSession
	partyTeleportLastRequest map[int]currentPartyTeleportRequest
	partyTeleportToken       uint64
	cancel                   context.CancelFunc
	wg                       sync.WaitGroup
	packetID                 uint64
	clientAccounts           clientAccountRegistry
	robotSessionSecret       []byte
	robotSessionReplayMu     sync.Mutex
	robotSessionReplays      map[[32]byte]time.Time
	// characterCreateMu serializes the name/slot/numeric-ID reservation shared
	// by native character creation and loopback GM robot provisioning. The
	// repository creation transaction makes one aggregate atomic, but the
	// three reservations are account/global reads that must not race each
	// other before that transaction begins.
	characterCreateMu sync.Mutex

	initialEquipmentMu      sync.Mutex
	initialEquipmentArchive *platformpvf.Archive
	initialEquipmentByJob   map[byte][]initialEquipmentEntry
	initialEquipmentLoadErr error
	pvfItemCatalogMu        sync.Mutex
	pvfItemCatalog          *pvfDungeonDropCatalog
	pvfItemCatalogLoadErr   error
	gmGameplaySettingsMu    sync.Mutex
	gmGameplaySettingsReady bool
	gmHellEpicDropRatePct   int
	// gmEpicDropRatePotionTestMultiplierPct 是"城镇增益药剂临时测试倍率"的缓存
	// （0 = 关闭，倍数按 PVF 表走）。见 gm_tool_dungeon.go 里那个常量的说明。
	gmEpicDropRatePotionTestMultiplierPct int
	gmGameplaySettingsAt                  time.Time
	automaticRobotAppearanceMu            sync.Mutex
	automaticRobotAvatarConfig            *currentAvatarFittingRoomConfig
	automaticRobotAvatarLoadErr           error
	chatEmoticonCatalogMu                 sync.Mutex
	chatEmoticonCatalog                   []currentChatEmoticonEntry
	chatEmoticonCatalogLoadErr            error
	vendingMachineCatalogMu               sync.Mutex
	vendingMachineCatalog                 *currentVendingMachineCatalog
	vendingMachineCatalogLoadErr          error
	vendingMachineRoll                    func(uint64) (uint64, error)
	vendingMachineCompletionKey           func() (string, error)
	joustCatalogMu                        sync.Mutex
	joustCatalog                          *dnfjoust.Catalog
	joustCatalogLoadErr                   error
	joustHistoryMu                        sync.Mutex
	joustOperationMu                      sync.Mutex
	spendTimeCatalogMu                    sync.Mutex
	spendTimeCatalogState                 *currentSpendTimeRuntimeCatalog
	spendTimeCatalogLoadErr               error
	attendanceMarbleMu                    sync.Mutex
	attendanceMarbleCatalog               *attendanceMarbleCatalog
	attendanceMarbleLoadErr               error
	laborDayMu                            sync.Mutex
	laborDayCatalog                       *dnfonlineevent.LaborDayCatalog
	monthlyAttendanceMu                   sync.Mutex
	monthlyAttendanceCatalog              *dnfonlineevent.MonthlyCatalog
	weeklyAttendanceMu                    sync.Mutex
	weeklyAttendanceCatalog               *dnfonlineevent.WeeklyCatalog
	returneeLuckyMu                       sync.Mutex
	returneeCatalog                       *dnfonlineevent.ReturneeCatalog
	luckyCharacterCatalog                 *currentLuckyCatalog
	legacyLevelMu                         sync.Mutex
	legacyLevelCatalog                    *dnfonlineevent.LegacyLevelCatalog
	levelAimingMu                         sync.Mutex
	levelAimingCatalog                    *dnfonlineevent.LevelAimingCatalog
	pcroomPeriodLoopOnce                  sync.Once
	pcroomPeriodLoop                      bool
	expertJobMu                           sync.Mutex
	expertJobCatalog                      *dnfexpertjob.Catalog
	expertJobCatalogLoadErr               error
	petCatalogMu                          sync.Mutex
	petCatalog                            *dnfpet.PVFCatalog
	petCatalogLoadErr                     error
	collectboxMu                          sync.Mutex
	collectboxCatalog                     *currentCollectboxCatalog
	collectboxCatalogLoadErr              error
	epicBookMu                            sync.Mutex
	epicBookCatalog                       *dnfepicbook.Catalog
	epicBookCatalogLoadErr                error
	hellPartyMu                           sync.Mutex
	hellPartyCatalog                      *dnfhellparty.Catalog
	hellPartyCatalogLoadErr               error
	hellPartyDropMu                       sync.Mutex
	hellPartyDropCatalog                  *dnfhellparty.DropCatalog
	hellPartyFragmentPools                map[int][]currentDungeonHellPartyFragmentCandidate
	hellPartyDropCatalogLoadErr           error
	premiumCatalogMu                      sync.Mutex
	premiumCatalog                        *currentPremiumCatalog
	premiumCatalogLoadErr                 error
	weaknessCostMu                        sync.Mutex
	weaknessCostTable                     *currentWeaknessCostTable
	weaknessCostErr                       error
	disjointConfigMu                      sync.Mutex
	disjointPVFConfig                     *currentDisjointPVFConfig
	disjointPVFConfigErr                  error
	repairCostMu                          sync.Mutex
	repairCostCatalog                     *currentRepairCostCatalog
	repairCostLoadErr                     error
	equipmentRegenerationMu               sync.Mutex
	equipmentRegenerationCatalog          *currentEquipmentRegenerationCatalog
	equipmentRegenerationLoadErr          error
	npcShopCatalogMu                      sync.Mutex
	npcShopCatalog                        *currentNPCShopCatalog
	npcShopCatalogLoadErr                 error
	adventureGroupMu                      sync.Mutex
	adventureGroupLoginMu                 sync.Mutex
	adventureGroupRuntimeMu               sync.Mutex
	adventureRepresentativeMu             sync.Mutex
	adventureGroupTable                   *adventuregroup.Tables
	adventureGroupLoadErr                 error
	honorMu                               sync.Mutex
	honorTable                            *dnfhonor.Tables
	honorLoadErr                          error
	initialSkillsMu                       sync.Mutex
	initialSkillsByJob                    map[byte][]initialSkillEntry
	skillCatalog                          *dnfskill.Table
	initialSPTable                        map[int]int
	initialTPTable                        map[int]int
	experienceMu                          sync.Mutex
	experienceTable                       *dnfprogression.ExperienceTable
	experienceTableLoadErr                error
	professionMu                          sync.Mutex
	professionProfiles                    *dnfprofession.Profiles
	professionProfilesErr                 error
	questCatalogMu                        sync.Mutex
	questCatalog                          *dnfquest.Catalog
	questCatalogLoadErr                   error
	ceraShopMu                            sync.Mutex
	ceraShopCatalog                       *pvfCeraShopCatalog
	ceraShopCatalogLoadErr                error
	ceraShopPurchaseMu                    sync.Mutex
	premiumWeb                            *currentPremiumWebPortal
	deathTowerMu                          sync.Mutex
	deathTowerStageMapIDs                 []int
	deathTowerBasisLevel                  int
	titleBookMappingMu                    sync.Mutex
	titleBookMappingCache                 map[int32]titleBookMappingEntry
	onlinePlayers                         *OnlinePlayerManager
	characterStatsMu                      sync.Mutex
	characterStats                        *dnfcharstat.Table
	characterStatsLoadErr                 error
	equipmentStatsMu                      sync.Mutex
	equipmentStatPaths                    map[int64]string
	equipmentStats                        map[int64]dnfcharstat.Vector
	equipmentExpAdvantages                map[int64]int64
	equipmentStatsLoadErr                 error
	combatPowerMu                         sync.Mutex
	combatPowerCatalog                    *dnfcombatpower.Catalog
	combatPowerCatalogErr                 error
	townCatalogMu                         sync.RWMutex
	townCatalog                           *dnftown.Table
	worldMapMu                            sync.RWMutex
	worldMapTable                         *worldmap.Table
	worldMapResolver                      *worldmap.Resolver
	dungeonAdmissionCatalog               *dnfdungeonadmission.Catalog
	dungeonAdmissionReceiptOnce           sync.Once
	dungeonAdmissionReceiptNonce          string
	dungeonAdmissionReceiptErr            error
	sequentialDungeonMu                   sync.Mutex
	sequentialDungeonCatalog              *currentSequentialDungeonCatalog
	sequentialDungeonCatalogErr           error
	dungeonMonsterTable                   *pvfDungeonMonsterCatalog
	dungeonAICharacterTable               *pvfDungeonAICharacterCatalog
	dungeonTutorialScripts                *pvfDungeonTutorialScriptCatalog
	dungeonChoice                         func(int) (int, error)
	dungeonSeed                           func() (uint32, error)
	gameplayTimers                        gameplayTimeQueue
	repositoryProvider                    func() (dnfrepo.Group, bool)
	initialTownEntryWait                  func(time.Duration)
	initialSceneMemoryTrimOnce            sync.Once
	dprotoProvider                        dproto.Provider

	// Personal-teleport (C2S op470) lock, kept in its own block so it does not
	// re-align the field groups above. Keyed by CHARACTER id: the client keeps
	// its countdown on the logged-in user object, so two characters of one
	// account each own an entry. The server mirrors it so a request cannot
	// bypass the client-side gate.
	soloTeleportCooldownMu    sync.Mutex
	soloTeleportCooldownUntil map[uint16]time.Time
	// soloTeleportClock is a test hook for the expiry above; nil means time.Now.
	soloTeleportClock func() time.Time
	// purchaseLimitClock 是 PVF 限购窗口取值用的时钟；nil 表示 time.Now。
	purchaseLimitClock func() time.Time
}

const initialSceneMemoryTrimDelay = 5 * time.Second

func (s *Service) scheduleInitialSceneMemoryTrim() {
	if s == nil {
		return
	}
	s.initialEquipmentMu.Lock()
	archiveLoaded := s.initialEquipmentArchive != nil
	s.initialEquipmentMu.Unlock()
	if !archiveLoaded {
		return
	}
	s.scheduleProcessMemoryTrim(initialSceneMemoryTrimDelay, runtimedebug.FreeOSMemory)
}

func (s *Service) scheduleProcessMemoryTrim(delay time.Duration, trim func()) {
	if s == nil || trim == nil {
		return
	}
	s.initialSceneMemoryTrimOnce.Do(func() {
		time.AfterFunc(delay, trim)
	})
}

// ServiceOptions 描述 dnfbridge 对外注入的项目侧依赖。
// 这里只保存依赖入口，不直接创建 MySQL/Redis，也不把玩法状态塞进 bridge。
type ServiceOptions struct {
	RepositoryProvider         func() (dnfrepo.Group, bool)
	DprotoProvider             dproto.Provider
	LocalDebugMode             bool
	LocalDebugStrikeOnSkillUse bool
	DebugKillRoomHotkey        bool
	// RobotSessionSecret authenticates the private-LAN dofrobot adapter. The
	// value is process-only: it must never be logged or persisted by bridge.
	RobotSessionSecret string
}

// New 使用框架环境创建 DNF bridge 服务。
func New(env *appkit.Env) *Service {
	return NewWithOptions(env, ServiceOptions{})
}

// NewWithOptions 使用框架环境和项目侧依赖创建 DNF bridge 服务。
func NewWithOptions(env *appkit.Env, serviceOptions ServiceOptions) *Service {
	if env == nil {
		env = &appkit.Env{}
	}
	core := env.Core()
	service := &Service{
		logger:                  core.Logger,
		options:                 optionsFromConfig(core.Config),
		localDebugMode:          serviceOptions.LocalDebugMode && strings.EqualFold(core.Config.Service.Environment, "local"),
		debugKillRoomHotkey:     serviceOptions.DebugKillRoomHotkey,
		conns:                   make(map[net.Conn]struct{}),
		gameSessions:            make(map[uint16]*gameSession),
		onlineSeats:             make(map[*gameSession]struct{}),
		onlineReportTrigger:     make(chan struct{}, 1),
		itemTrades:              make(map[uint16]*onlineItemTrade),
		weddingInvitations:      make(map[uint16]*currentWeddingInvitation),
		privateStores:           make(map[uint16]*currentPrivateStore),
		privateStoreVisitors:    make(map[*gameSession]uint16),
		expertJobStores:         make(map[uint16]*currentExpertJobStore),
		expertJobVisitors:       make(map[*gameSession]uint16),
		raids:                   make(map[uint32]*runtimeRaidState),
		raidRewardOverrides:     newRaidRewardOverrides(),
		initialEquipmentArchive: env.PVF,
		repositoryProvider:      serviceOptions.RepositoryProvider,
		dprotoProvider:          serviceOptions.DprotoProvider,
		clientAccounts:          newClientAccountRegistry(),
		partyEndpoints:          newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID:        localUDPEndpointOwnerPID,
		runtimePartyManager:     party.NewRuntimePartyManager(),
		robotSessionSecret:      []byte(strings.TrimSpace(serviceOptions.RobotSessionSecret)),
		robotSessionReplays:     make(map[[32]byte]time.Time),
	}
	service.localDebugStrikeOnSkillUse = service.localDebugMode && serviceOptions.LocalDebugStrikeOnSkillUse
	service.launcherAuthHTTPClient = newLauncherAuthVerifyClient(service.options)
	identity, identityErr := newLauncherIdentityVerifier(
		service.options.launcherIdentityMode,
		service.options.launcherIdentityPublicKeys,
	)
	if identityErr != nil {
		// Fail closed on a broken configuration: a server that looks bound but
		// silently accepts anyone is worse than one that refuses to start, so
		// the error is held and returned by Start.
		service.launcherIdentityErr = identityErr
		identity = &launcherIdentityVerifier{mode: LauncherIdentityModeOff}
	}
	service.launcherIdentity = identity
	if service.launcherIdentity.enabled() {
		service.logInfo("dnfbridge launcher identity configured",
			"mode", service.launcherIdentity.modeName(),
			"server_id", service.launcherIdentity.serverID,
			"public_keys", len(service.launcherIdentity.keys))
	}
	service.nextAuctionListingID.Store(uint64(time.Now().Unix()))
	service.gameplayTimers = newProcessGameplayTimeQueue(func(name string, recovered any) {
		service.logWarn("dnfbridge gameplay timer callback panic", "timer_name", name, "recovered", recovered)
	})
	service.onlinePlayers = newOnlinePlayerManager()
	_, _ = service.currentDungeonAdmissionReceiptNonce()
	service.registerDungeonRuntimeDebugRoute(env)
	return service
}

// Name 返回生命周期组件名。
func (s *Service) Name() string {
	return "dnfbridge-service"
}

// Start 加载外部 channel_info.etc，并启动频道服和 game 端口监听。
func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.validateDprotoConfiguration(); err != nil {
		return err
	}
	if s.launcherIdentityErr != nil {
		return fmt.Errorf("dnfbridge launcher identity: %w", s.launcherIdentityErr)
	}
	catalog, channelScript, err := s.loadChannelAssets()
	if err != nil {
		return err
	}
	// 表能起来但玩家侧会受影响的状态（缺默认城镇频道 / 没有特殊区域频道）在启动时也要可见，
	// 手改过 channel_info.etc 的人不该只在热重载里才看到。
	for _, warning := range currentChannelTableWarnings(catalog, true) {
		s.logWarn("dnfbridge channel table warning",
			"warning", warning,
			"channel_info", s.options.channelInfoFile)
	}
	packetLog, err := s.openPacketLog()
	if err != nil {
		return err
	}
	// 审计记录器：目录不可写时 newActivityRecorder 返回 nil，后续 recordActivity 自动空转，
	// 不影响服务启动——记录是旁路，不能因为磁盘/权限问题把服务端拦在门外。
	records := newActivityRecorder(s.options.recordDir, s.logger)
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.catalog = catalog
	s.channelScript = append([]byte(nil), channelScript...)
	s.runCtx = runCtx
	s.cancel = cancel
	s.packetLog.Store(packetLog)
	s.records.Store(records)
	if s.initialTownEntryWait == nil {
		// Initial-town visual delay is disabled. Production never blocks the
		// server packet stream at the retained test boundary.
		s.initialTownEntryWait = func(time.Duration) {}
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.mu.Unlock()

	// 2026-09-12: both live sessions stopped answering right after login while the
	// accept loop stayed healthy -- the signature of a handler blocked on a shared
	// mutex (the read loop shares the session goroutine, so a blocked handler stops
	// every later RECV from being logged at all). Keep a rolling goroutine dump on
	// disk so the next occurrence names the exact frame instead of being inferred.
	s.startGoroutineWatchdog(runCtx)

	// 2026-09-25: the same class of question again, one level finer. The
	// watchdog above dumps every 20s, which is far too coarse to land inside a
	// 4-10s stall. This sampler is default-off (DNF90_STALL_SAMPLER) and
	// appends an all-goroutine stack snapshot only when the snapshot changed,
	// so it names the exact frame a slow path is sitting in -- including a
	// frame nobody thought to instrument, which is what the five existing
	// step probes in completeRuntimePartyDungeonLoading failed to do.
	s.startStallStackSampler(runCtx)

	// PVF preload stages run in a fixed order before any listener opens. Keep
	// one row per stage: the order, each loader's own wrapped error text, and
	// the shared cleanup path are observable startup behavior. The loop must
	// not re-wrap loader errors or change what is reset on failure; name only
	// labels the stage for audit, it is never merged into the returned error.
	pvfPreloads := []struct {
		name string
		load func(context.Context) error
	}{
		{"initial equipment index", s.preloadInitialEquipmentIndex},
		{"item catalog", s.preloadPVFItemCatalog},
		{"pet catalog", s.preloadPetPVFCatalog},
		{"expert job catalog", s.preloadExpertJobCatalog},
		{"adventure group table", s.preloadAdventureGroupTable},
		{"honor table", s.preloadHonorTable},
		{"town catalog", s.preloadTownCatalog},
		{"skill catalog", s.preloadSkillCatalog},
		{"profession profiles", s.preloadProfessionProfiles},
		{"quest catalog", s.preloadQuestCatalog},
		{"dungeon world map", s.preloadDungeonWorldMap},
		{"character stat table", s.preloadCharacterStatTable},
		{"equipment stat index", s.preloadEquipmentStatIndex},
		{"combat power affix catalog", s.preloadCombatPowerAffixCatalog},
		{"epic book catalog", s.preloadEpicBookCatalog},
		{"hell party catalog", s.preloadHellPartyCatalog},
		{"hell party drop catalog", s.preloadHellPartyDropCatalog},
		{"dungeon drop catalog", s.preloadDungeonDropCatalog},
		// 把 itemRefs 索引里的全部道具定义解析好，消掉玩家进城时的逐道具冷解析。
		// 依赖上一行的 catalog，必须排在它后面；代价与失败语义见
		// dungeon_drop_item_preload.go。
		{"dungeon drop item definitions", s.preloadDungeonDropItemDefinitions},
		{"gold carry limit table", s.preloadCarryGoldLevelLimits},
	}
	for _, preload := range pvfPreloads {
		if err := preload.load(runCtx); err != nil {
			cancel()
			closeErr := packetLog.Close()
			s.mu.Lock()
			s.catalog = nil
			s.cancel = nil
			s.packetLog.Store(nil)
			s.mu.Unlock()
			return errors.Join(err, closeErr)
		}
	}
	// Build the PVF catalogs that would otherwise be created lazily inside the
	// first player request that needs one. Measured 2026-09-26 on a cold server:
	// the first town entry built three of them inside that player's own session
	// event loop and blocked the session for 1589ms (op143
	// ENUM_CMDPACKET_CHANGE_TUTORIAL_FLAG); the same entry 30 seconds later, with
	// the catalogs warm, took 491ms. Deliberately best effort, and deliberately
	// not rows in pvfPreloads -- see warmOptionalPVFCatalogs for both reasons.
	s.warmOptionalPVFCatalogs(runCtx)
	if err := s.restoreRuntimeCreatedRaidSeed(); err != nil {
		cancel()
		closeErr := packetLog.Close()
		s.mu.Lock()
		s.catalog = nil
		s.cancel = nil
		s.packetLog.Store(nil)
		s.mu.Unlock()
		return errors.Join(err, closeErr)
	}
	// Startup builds several short-lived PVF document indexes before retaining
	// their compact domain catalogs. Force one collection/scavenge at this fixed
	// boundary so those temporary token graphs and spans do not remain resident
	// until normal gameplay eventually reaches the next GC target. This runs
	// before listeners open, so it cannot add a pause to an active session.
	runtimedebug.FreeOSMemory()
	gamePorts := s.gameListenPorts(catalog)
	if len(gamePorts) == 0 {
		cancel()
		closeErr := packetLog.Close()
		s.mu.Lock()
		s.catalog = nil
		s.cancel = nil
		s.packetLog.Store(nil)
		s.mu.Unlock()
		return errors.Join(fmt.Errorf("dnfbridge has no game listen ports"), closeErr)
	}
	partyRelay, err := newCurrentPartyUDPRelay(
		s.options.partyUDPRelayEnabled,
		s.options.serverIP,
		s.options.partyUDPRelayPortStart,
		s.options.partyUDPRelayPortCount,
		func(message string, args ...any) { s.logWarn(message, args...) },
	)
	if err != nil {
		cancel()
		closeErr := packetLog.Close()
		s.mu.Lock()
		s.catalog = nil
		s.cancel = nil
		s.packetLog.Store(nil)
		s.mu.Unlock()
		return errors.Join(err, closeErr)
	}
	// The relay's own diagnostics: prose warnings for its lifecycle, and named
	// packet-log events for the forwarding budget, so a throttled leg is
	// greppable as game-party-udp-relay-throttled.
	partyRelay.logEvent = func(kind string, args ...any) { s.logPacketEvent(kind, args...) }
	if s.options.packetLogEnabled {
		// The dump is a diagnostic, not a transcript. Party traffic between two
		// clients in one town runs at thousands of datagrams per second, and
		// writing every payload as hex put 228 MB into the packet log in five
		// minutes, which starved the server's own packet path and made the client
		// look unresponsive. Sample it the way the relay already samples its own
		// trace: the head of the stream in full, then every thousandth datagram.
		relayDumps := make(map[currentPartyUDPRelayLegKey]int)
		var relayDumpsMu sync.Mutex
		partyRelay.payloadDump = func(leg currentPartyUDPRelayLegKey, source *net.UDPAddr, payload []byte) {
			relayDumpsMu.Lock()
			relayDumps[leg]++
			seen := relayDumps[leg]
			relayDumpsMu.Unlock()
			if !currentPartyUDPRelayDumpDatagram(seen) {
				return
			}
			s.logPacketEvent("party-udp-relay-payload",
				"party_id", leg.partyID,
				"owner_id", leg.ownerID,
				"peer_id", leg.peerID,
				"source", source.String(),
				"bytes", len(payload),
				"packet_count", seen,
				"sampled", seen > currentPartyUDPRelayDumpHeadPackets,
				"payload_hex", hex.EncodeToString(payload))
		}
	}
	partyProbeBindIP := s.options.partyUDPBindIP
	if partyProbeBindIP == "" {
		partyProbeBindIP = s.options.serverIP
	}
	partyProbe, err := newCurrentPartyUDPProbe(
		runCtx,
		partyProbeBindIP,
		[]int{s.options.initialUDPPort1, s.options.initialUDPPort2},
		s.observeCurrentPartyUDPEndpoint,
		func(message string, args ...any) { s.logInfo(message, args...) },
	)
	if err != nil {
		cancel()
		_ = partyRelay.Close()
		closeErr := packetLog.Close()
		s.mu.Lock()
		s.catalog = nil
		s.cancel = nil
		s.packetLog.Store(nil)
		s.mu.Unlock()
		return errors.Join(err, closeErr)
	}
	s.mu.Lock()
	s.partyUDPRelay = partyRelay
	s.partyUDPProbe = partyProbe
	s.mu.Unlock()

	if err := s.listen(runCtx, s.options.channelListen, s.handleChannelConn); err != nil {
		cancel()
		_ = partyRelay.Close()
		_ = partyProbe.Close()
		closeErr := packetLog.Close()
		s.mu.Lock()
		s.catalog = nil
		s.cancel = nil
		s.packetLog.Store(nil)
		s.partyUDPRelay = nil
		s.partyUDPProbe = nil
		s.mu.Unlock()
		return errors.Join(err, closeErr)
	}
	s.startGameplayTimeQueue(runCtx)
	s.registerDailyQuestResetTimer()
	s.startCurrentJoustScheduler(runCtx)
	s.startLauncherOnlineReport(runCtx)

	for _, port := range gamePorts {
		if err := s.listenGamePort(runCtx, port); err != nil {
			cancel()
			_ = s.Stop(context.Background())
			return err
		}
	}
	s.logInfo("dnfbridge started",
		"channel_listen", s.options.channelListen,
		"channel_info", s.options.channelInfoFile,
		"channel_server_index", s.channelServerID(),
		"advertise_server_index", s.channelAdvertiseID(),
		"channel_info_body_mode", s.channelInfoBodyMode(),
		"game_initial_mode", s.gameInitialMode(),
		"game_pre_bootstrap", s.gamePreBootstrapMode(),
		"game_post_bootstrap", s.gamePostBootstrapMode(),
		"game_upper_header", s.gameUpperHeaderMode(),
		"game_upper_client_body_codec", s.gameUpperClientBodyCodecMode(),
		"game_dproto_mode", s.gameDprotoMode(),
		"game_listen_host", s.options.gameListenHost,
		"game_ports", len(gamePorts),
		"server_ip", s.options.serverIP,
		"party_udp_probe_ports", fmt.Sprintf("%d,%d", s.options.initialUDPPort1, s.options.initialUDPPort2),
		"party_udp_relay", partyRelay.Enabled(),
		"party_udp_relay_port_start", s.options.partyUDPRelayPortStart,
		"party_udp_relay_port_count", s.options.partyUDPRelayPortCount)
	return nil
}

func gameListenAddress(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ":" + strconv.Itoa(port)
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// Stop 关闭监听并等待连接处理退出。
func (s *Service) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	cancel := s.cancel
	listeners := append([]net.Listener(nil), s.listeners...)
	packetLog := s.packetLog.Load()
	records := s.records.Load()
	partyRelay := s.partyUDPRelay
	partyProbe := s.partyUDPProbe
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.listeners = nil
	s.gameListeners = nil
	s.runCtx = nil
	s.cancel = nil
	s.packetLog.Store(nil)
	s.records.Store(nil)
	s.partyUDPRelay = nil
	s.partyUDPProbe = nil
	s.channelScript = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var stopErr error
	for _, listener := range listeners {
		stopErr = errors.Join(stopErr, listener.Close())
	}
	for _, conn := range conns {
		stopErr = errors.Join(stopErr, conn.Close())
	}
	if partyRelay != nil {
		stopErr = errors.Join(stopErr, partyRelay.Close())
	}
	if partyProbe != nil {
		stopErr = errors.Join(stopErr, partyProbe.Close())
	}
	stopErr = errors.Join(stopErr, packetLog.Close())
	// 记录器先关：关闭后再来的 recordActivity 会因为 s.records 为空而空转，
	// 不会往一个已经关掉的文件句柄里写。
	records.Close()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return errors.Join(stopErr, ctx.Err())
	case <-done:
		return stopErr
	}
}

func (s *Service) listen(ctx context.Context, addr string, handler func(context.Context, net.Conn)) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, listener)
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ctx, listener, handler)
	return nil
}

// listenGamePort 打开一个游戏端口监听，并按端口登记，供热重载时按频道开关对账。
// 只用于游戏端口；频道口（channelListen）仍走 listen()。
func (s *Service) listenGamePort(ctx context.Context, port int) error {
	addr := gameListenAddress(s.options.gameListenHost, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.mu.Lock()
	if s.gameListeners == nil {
		s.gameListeners = make(map[int]net.Listener)
	}
	// 同一端口若已有监听（重复对账），保留先到的那个，并把这个多余的关掉。
	// 注意要在登记进 s.listeners **之前**判断，否则会给 Stop 留一个已关闭的孤儿 listener。
	if existing := s.gameListeners[port]; existing != nil {
		s.mu.Unlock()
		_ = listener.Close()
		return nil
	}
	s.gameListeners[port] = listener
	s.listeners = append(s.listeners, listener)
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ctx, listener, s.handleGameConn)
	return nil
}

// closeGamePort 关闭某个游戏端口的监听并解除登记。
//
// 注意语义：只停止接受**新**连接。net.Listener.Close 不会断开已经 Accept 的连接，
// 所以关闭频道不会把站在该频道里的玩家踢下线——这正是热重载想要的行为。
func (s *Service) closeGamePort(port int) error {
	s.mu.Lock()
	listener := s.gameListeners[port]
	delete(s.gameListeners, port)
	if listener != nil {
		kept := make([]net.Listener, 0, len(s.listeners))
		for _, item := range s.listeners {
			if item != listener {
				kept = append(kept, item)
			}
		}
		s.listeners = kept
	}
	s.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

// currentAcceptLoopFailureLimit 是 accept 连续失败多少次后放弃该监听。
//
// 存在的理由：Accept 的错误有两类——一类是监听被关闭（必须退出，见下），另一类
// 是资源类临时错误（EMFILE 之类，值得重试）。如果某天出现"永久性且非 ErrClosed"
// 的错误，无上限重试同样会变成空转 + 日志刷屏，所以给一个上限兜底。
const currentAcceptLoopFailureLimit = 32

func (s *Service) acceptLoop(ctx context.Context, listener net.Listener, handler func(context.Context, net.Conn)) {
	defer s.wg.Done()
	consecutiveFailures := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			// 监听被关闭时必须直接退出。
			//
			// 频道热重载会关掉"已停用频道"的游戏端口（gm_channel_reload.go → closeGamePort），
			// 那条路径**不会**取消运行期 ctx（取消会把整个服务停掉）。此时 Accept 会立刻
			// 返回 closed 错误，只判 ctx.Err() 就会 continue → 死循环：一个核 100% 空转，
			// 而且每圈写一条 warn，日志会被刷爆。Stop 是先 cancel 再 Close listener，所以
			// 老路径掩盖了这个坑，热重载才把它暴露出来。
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			consecutiveFailures++
			if consecutiveFailures >= currentAcceptLoopFailureLimit {
				s.logWarn("dnfbridge accept loop stopped after repeated failures",
					"addr", listener.Addr().String(), "failures", consecutiveFailures, "error", err)
				return
			}
			s.logWarn("dnfbridge accept failed", "addr", listener.Addr().String(), "error", err)
			continue
		}
		consecutiveFailures = 0
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.trackConn(conn)
			defer s.untrackConn(conn)
			handler(ctx, conn)
		}()
	}
}

func (s *Service) loadChannelAssets() (*channelcatalog.Catalog, []byte, error) {
	raw, err := osReadFile(s.options.channelInfoFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load dnf channel info: %w", err)
	}
	index, err := channelinfo.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse dnf channel info: %w", err)
	}
	catalog, err := channelcatalog.New(index, channelcatalog.Options{ServerID: s.channelServerID()})
	if err != nil {
		return nil, nil, err
	}
	script, err := s.buildChannelScript(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("build 90CN online channel script: %w", err)
	}
	return catalog, script, nil
}

func (s *Service) buildChannelScript(raw []byte) ([]byte, error) {
	if s.channelTableStrict() {
		return channelinfo.Build90CNOnlineScript(
			raw,
			s.channelServerID(),
			s.channelAdvertiseID(),
			dnfenum.BootstrapChannelID,
		)
	}
	// 宽松模式：源表没有引导频道 19 时不再让 loadChannelAssets 失败（服务端起不来）。
	// 只告警，让运维在"只开少数频道"的配置下仍能启动；要恢复旧硬校验把
	// DNFBRIDGE_CHANNEL_TABLE_STRICT 设成 1。
	script, bootstrapPresent, err := channelinfo.Build90CNOnlineScriptAllowingMissingBootstrap(
		raw,
		s.channelServerID(),
		s.channelAdvertiseID(),
		dnfenum.BootstrapChannelID,
	)
	if err != nil {
		return nil, err
	}
	if !bootstrapPresent {
		s.logWarn("dnfbridge channel table has no bootstrap channel; starting anyway",
			"bootstrap_channel", dnfenum.BootstrapChannelID,
			"channel_info", s.options.channelInfoFile,
			"reason", "bootstrap_channel_check_is_legacy_for_the_official_pvf_flow",
			"restore_strict", "DNFBRIDGE_CHANNEL_TABLE_STRICT=1")
	}
	return script, nil
}

// channelTableStrict 返回是否恢复"频道表必须含 11/19"的旧硬校验。
func (s *Service) channelTableStrict() bool {
	if s == nil {
		return false
	}
	return s.options.channelTableStrict
}

func (s *Service) channelServerID() int {
	if s != nil {
		if s.options.channelServerID > 0 {
			return s.options.channelServerID
		}
	}
	return 1
}

func (s *Service) channelAdvertiseID() int {
	if s != nil {
		if s.options.channelAdvertiseID >= 0 {
			return s.options.channelAdvertiseID
		}
	}
	return s.channelServerID()
}

func (s *Service) channelInfoBodyMode() string {
	if s == nil {
		return channelInfoBodyLatest
	}
	return normalizeChannelInfoBodyMode(s.options.channelInfoBodyMode)
}

func (s *Service) currentCatalog() *channelcatalog.Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalog
}

func (s *Service) currentChannelScript() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.channelScript...)
}

func (s *Service) openPacketLog() (*packetLogger, error) {
	if !s.options.packetLogEnabled {
		return nil, nil
	}
	logger, err := openPacketLogger(s.options.packetLogPath)
	if err != nil {
		// 与 records 同一条策略（见下方 newActivityRecorder 的注释）：包日志是旁路，
		// 磁盘/密钥问题不能把服务端拦在门外。但这里**绝不退化成明文**——打不开就整条
		// 通道关闭，并留下一条显式告警，让运维能立刻发现"日志没了"而不是"日志变明文了"。
		if s.logger != nil {
			s.logger.Warn(
				"dnfbridge packet log disabled: encrypted log channel could not be opened",
				"path", s.options.packetLogPath,
				"error", err,
			)
		}
		return nil, nil
	}
	return logger, nil
}

func (s *Service) logPacket(direction string, kind string, data []byte, fields ...any) {
	if suppressPacketLogLine(direction, kind, fields...) {
		return
	}
	logger := s.packetLog.Load()
	logger.Log(direction, kind, data, fields...)
}

func (s *Service) logPacketEvent(kind string, args ...any) {
	if suppressPacketLogEvent(kind, args...) {
		return
	}
	logger := s.packetLog.Load()
	logger.Event(kind, args...)
}

func suppressPacketLogLine(direction string, kind string, fields ...any) bool {
	if kind == "game-raw" {
		return true
	}
	msgID, hasMsgID := packetLogUintField(fields, "msg_id")
	typ, hasType := packetLogUintField(fields, "type")
	if hasMsgID {
		switch msgID {
		case 1276, 1516, 1518: // heartbeat / antibot / DPROTO callback noise.
			return true
		case 13:
			// op13 full container refreshes already have concise EVENT evidence.
			return direction == "SEND"
		}
	}
	if hasType {
		switch typ {
		case 35: // SET_USER_POSITION, emitted continuously while walking.
			return true
		}
	}
	return false
}

func suppressPacketLogEvent(kind string, fields ...any) bool {
	switch kind {
	case "game-town-set-user-position-captured":
		return true
	case "game-upper-check-user-connection-selected-success":
		return true
	// 攻坚高频噪声：这两种 2.5 小时内接近一千条（实测 708 + 264），而它们的全部
	// 信息量就是"同类型的门边界通知又跳过一次"——原因已经写在 kind 名里，且真正
	// 要看的那次边界现在写在 records/raid-*.jsonl（带 wall_ms / 名单 / 就绪位图）。
	// 掐掉它们不损失任何证据，只把 packet_log 里翻日志的时间还回来。
	case "game-raid-dungeon-lifecycle-skipped-already-notified":
		return true
	case "game-raid-dungeon-lifecycle-skipped-joined":
		return true
	}
	msgID, hasMsgID := packetLogUintField(fields, "msg_id")
	typ, hasType := packetLogUintField(fields, "type")
	if hasMsgID {
		switch msgID {
		case 1276, 1516, 1518:
			return true
		case 13:
			return kind == "game-upper-send-meta"
		}
	}
	if hasType && typ == 35 {
		return kind == "game-legacy-meta"
	}
	return false
}

func packetLogUintField(fields []any, name string) (uint64, bool) {
	for i := 0; i+1 < len(fields); i += 2 {
		if fmt.Sprint(fields[i]) != name {
			continue
		}
		switch v := fields[i+1].(type) {
		case uint8:
			return uint64(v), true
		case uint16:
			return uint64(v), true
		case uint32:
			return uint64(v), true
		case uint64:
			return v, true
		case uint:
			return uint64(v), true
		case int8:
			if v >= 0 {
				return uint64(v), true
			}
		case int16:
			if v >= 0 {
				return uint64(v), true
			}
		case int32:
			if v >= 0 {
				return uint64(v), true
			}
		case int64:
			if v >= 0 {
				return uint64(v), true
			}
		case int:
			if v >= 0 {
				return uint64(v), true
			}
		}
	}
	return 0, false
}

func (s *Service) trackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[conn] = struct{}{}
}

func (s *Service) untrackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

func (s *Service) nextPacketConnID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packetID++
	return fmt.Sprintf("%s-%06d", prefix, s.packetID)
}

func (s *Service) aesKey() string {
	return time.Now().Format("20060102") + "000006"
}

func (s *Service) logInfo(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

func (s *Service) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}
