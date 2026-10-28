// 本文件负责 DNF bridge 的 game 端口连接、混合帧分发和登录前 wire 回包。
// 这里只处理客户端字节协议兼容，不持有玩家持久化状态或玩法规则。
package dnfbridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dproto"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/platform/wineenv"
)

type gameSession struct {
	// completedRunTownReturnOrigin is the town origin frozen when the character
	// entered the run that has just been retired. A completed settlement outlives
	// its runtime: 选择其他地下城 retires it before the client's 返回城镇 arrives, and
	// without this stash that return has no origin left and falls back to the
	// persisted login town -- the Seria room (38/1). The party then lands in a
	// private scene, cannot see each other there, and the client refuses the next
	// dungeon entry with 队员不在附近. Guarded by townMu, like the selector origin.
	completedRunTownReturnOrigin currentTownPositionSnapshot

	conn net.Conn
	// writer owns this session's blocking socket writes so the event loop never
	// parks on a client that stopped reading. See game_session_writer.go for the
	// live 48-second stall measurement that made it necessary. nil means "no
	// writer": the write stays synchronous, which is what every test that builds
	// a bare session expects.
	writer                 *gameSessionWriter
	connID                 string
	accountID              string
	expiredItemSweepMu     sync.Mutex
	expiredItemSweepAt     time.Time
	expiredItemSweepCharID uint32
	machineID              string
	accountSourceIP        netip.Addr
	remoteClientPID        uint32
	robotSessionBound      bool
	// auctionLastSearchOpcode/Body remember the last auction search this
	// session ran so a successful 竞拍 can replay it. The client's own op185
	// success path erases the row it acted on (sub_30071B0) - correct for a
	// buyout, wrong for a bid, and not something the server can suppress - so
	// the list has to be re-sent or it stays empty until the player searches
	// again. Only ever touched under Service.auctionMu.
	auctionLastSearchOpcode uint16
	auctionLastSearchBody   []byte
	clientPID               uint32
	// loginIdentity is the per-login identity used to tell "another client on the
	// same account" from "this client opened another connection". Local play uses
	// clientPID; off this machine clientPID is always 0, so the in-band
	// class-1/op1 credential gate stores its login-token digest here instead. An
	// empty value means the identity is unknown, and duplicate-login enforcement
	// deliberately does nothing rather than risk kicking the player's own channel
	// switch. See currentDuplicateLoginWouldDisplace.
	loginIdentity string
	// duplicateLoginKicked marks a session a duplicate-login kick already owns, so
	// the ~100 connections one client opens at startup cannot kick each other in a
	// loop and a displaced session is not displaced twice. Guarded by Service.mu.
	duplicateLoginKicked   bool
	raidAvailabilitySent   bool
	raidDirectoryReadySent bool
	raidDirectory          runtimeRaidDirectorySubscription
	// raidBrowseTarget names the raid this session's attack-party window is
	// currently browsing without having joined it. op1359 is the client's poll
	// while that window is open, so a target that has not been refreshed for
	// runtimeRaidBrowseTTL belongs to a window that was closed. Guarded by
	// Service.mu, exactly like raidDirectory above.
	//
	// raidBrowseMembers is the fingerprint of the member id set last delivered as
	// the identity payload (member actors + 0x0250 + 819 + 0x0256). It only decides
	// whether that heavier part is replayed when the roster changes; it never
	// suppresses the 0x024F mode=3 roster push itself.
	raidBrowseTarget             uint32
	raidBrowseAt                 time.Time
	raidBrowseMembers            uint64
	raidProjectionMu             sync.Mutex
	raidRejoinProjectionSent     bool
	raidRejoinProjectionRaidKey  uint32
	raidRejoinProjectionRevision uint64
	raidEndedProjectionRaidKey   uint32
	// 超时空 1452..1460 战况面板的按会话增量基线。
	//
	// fiendWarBoard 是这个会话上一次收到的战场状态；下一次 tick 只发与它不同的行。
	// fiendWarBoardMu 串行化这次比较 —— 面板既由攻坚 tick 发布，也由每个成员自己的
	// 包触发，两个发布者同时跑会各自按一份已经被对方换掉的基线算增量。
	fiendWarBoardMu sync.Mutex
	fiendWarBoard   runtimeRaidState
	// fiendWarBoardFixtureSignature 是上一次为战况表算出的"每个设施区域画哪只怪"。
	// 变了才记一行 game-fiendwar-board-fixtures，用来和进图那条
	// game-fiendwar-maze-selected 对照（真机上报过表上画的与进去打的不一致）。
	fiendWarBoardFixtureSignature string
	// 超时空"走进区域即开战"的去重。城镇移动路径与战场 tick 都会调
	// tryRuntimeFiendWarEncounter，没有这组记忆两边会为同一次抵达各开一个实例。
	fiendWarEncounterRaidKey uint32
	fiendWarEncounterArea    byte
	fiendWarEncounterAt      time.Time
	// fiendWarPlannedEncounterUntil 是"这一脚按进图的计划正在同一条信箱里跑"的
	// 截止时刻（守卫见 runtimeFiendWarPlannedEncounterPending）。它挡的是战况表
	// 1 Hz 心跳与计划进图之间的一场真实竞态：计划的这一趟在开工前就把本人的实时
	// 城镇坐标推到了**目标区域**（raid_fiendwar_battle.go 的
	// setCurrentTownPositionSceneFromTransitionLocked），而副本 runtime 要几百毫秒
	// 后才落地。心跳若正好落在这一段里，plan==nil 那一路会把"我站在目标区域"当成
	// 名单锚点，算出**空名单**、只带自己一个人进图，并且把随后到达的整队进图顶掉
	//（那时 runtime 已存在，计划那一趟直接返回）。
	//
	// 真机 2026-10-01（本地 00:03:53）就是它：计划那一脚在 53.743 发包，战况表
	// 心跳的 1 Hz 刻度在 53.73 落在 43.73+n 的节奏上、53.790 就把单人副本开出来
	// 了，char 22 留在城镇原地；42 秒后 16:04:35 那一脚因为 op36(35.786) 落在心跳
	// 刻度(35.73)**之后**，心跳先跑且当时人还在格 4（该格无怪，心跳自然放弃），
	// 于是整队进图成功 —— 同一个操作时快时慢，差别只在按键落在刻度前还是后。
	fiendWarPlannedEncounterUntil time.Time
	// raidParticipationReplayMu guards raidParticipationReplay below. It is a leaf
	// lock: the rebuilt-scene replay takes one session's lock at a time and takes
	// nothing else while holding it, so it cannot invert against wireMu/dungeon.mu.
	raidParticipationReplayMu sync.Mutex
	// raidParticipationReplay is the 0x0248 (584) participation row set this client
	// was last re-stated with by the rebuilt-scene self-heal
	// (replayRuntimeRaidParticipationRows) -- i.e. the rows its 战况表 was last told
	// about at a moment when its own scene was known to exist.
	//
	// The replay has to stay (it is what puts back a 584 row the client dropped
	// while its scene was being rebuilt, see raid_current_compat.go), but the
	// client's op=1 branch (sub_17C0F20) POPS the chat line 32048
	// 「其中1支战队已进入[%s]地下城。」 for every row it inserts, so replaying on
	// every finish-loading printed that line once per ROOM (服主实测 2026-09-29:
	// "在卢克/安徒恩副本里每过一个房间，聊天框就弹一次"). A room change inside one
	// gate does not change the row set, so it no longer replays; a real gate change
	// does, and that is exactly the moment the client can lose the row.
	raidParticipationReplay currentRaidParticipationReplayLedger
	channel                 channelcatalog.Channel
	// pvpQuestBootstrapSent records that the arena channel connection already
	// received the completed-quest bitmap. The client evaluates its PVP
	// eligibility gate on that connection before selecting a character.
	pvpQuestBootstrapSent bool
	residentChannel       channelcatalog.Channel
	packetSeq             uint64
	sequence              uint32
	upperSeq              uint16
	// stall 是**只读诊断**用的卡住计量器（见 dungeon_stall_diagnostics.go）：
	// 记录"同一个目标被连续拒绝了多少次"，以及本会话最后发出去的 upper 包。
	// 不参与任何判定、不发包；裸会话（测试构造的）为 nil，诊断自动关闭。
	stall *dungeonStallTracker
	// s2cCipher 是这条连接的 S2C 回包加解密器（见 protocol/s2ccipher.go）。
	// nil = 明文。只在 LANA 握手协商成功（客户端答 version 3）时才有值，
	// 因此"旧客户端/未开开关"的连接天然保持明文，不会收到乱码。
	s2cCipher *dnfproto.S2CCipher
	// lukeCardDisplayMu guards lukeCardDisplayPoppedDays below. It is a leaf
	// lock, exactly like raidParticipationReplayMu above: nothing else is taken
	// while it is held, and the claim is released before any packet is built.
	lukeCardDisplayMu sync.Mutex
	// lukeCardDisplayPoppedDays is the per-login-session ledger of the one tarot
	// pop-up the official behaviour grants. 服主 2026-09-29 的官方口径:
	//
	//	「玩家在每次登陆后，第一次进入卢克 RAID 频道时，离开赛利亚旅馆会自动
	//	  展示一次塔罗牌。」
	//
	// ⇒ the key is the **tarot day**, not a raid: the card set is global PVF
	// configuration that does not depend on any raid (raid_luke_card_display.go).
	// The FIRST 1181 of a tarot day in one session goes out with
	// openWindowFlag = 1 and records that day here; every later 1181 in the same
	// session (landing in the town again, coming back from a cleared gate,
	// walking into the waiting room) still refreshes the cards but carries
	// openWindowFlag = 0, so the packet can never become a second pop-up.
	// 重新登录 = 新的 gameSession = 空账本 ⇒ 再弹一次, which is exactly
	// "每次登陆后…展示一次".
	lukeCardDisplayPoppedDays map[int]bool
	ctx                       context.Context
	// wireMu owns the complete per-session wire boundary: sequence
	// allocation, DPROTO state transitions, and socket writes.  A gameplay
	// timer can send concurrently with the connection reader, so callers
	// must not allocate a sequence before acquiring this lock.
	wireMu sync.Mutex
	dproto dproto.Session
	// upperWriteBatch is protected by wireMu. It is used only for explicit
	// multi-result gestures whose independently encoded native frames should be
	// delivered to the client in one socket write.
	upperWriteBatchActive bool
	upperWriteBatch       []byte

	endpointHandshakeMu              sync.Mutex
	currentChannelResidentNoticeSent bool
	gameEndpointSuccessSent          bool
	auctionServiceOpenMask           byte
	connectionTownActorOwnerChannel  byte
	events                           *gameSessionEvents
	characterGeneration              uint64
	// levelAimingCreatedID 是"升级预约"活动（onlineevent level_aiming，活动 222）里
	// 本会话刚创建的那个角色 ID；0 表示本次创建的不是预约角色。建号时写入、
	// 选角界面同步时读回，用来判断预约响应是否属于本会话创建的角色。
	levelAimingCreatedID uint16
	// weddingStateSent 记录本次会话是否已下发过 1000 号婚姻状态行。原生登录流程里
	// 它是一次性事件；重复下发会重置伴侣预览（见 online_player_manager.go）。
	weddingStateSent                     atomic.Bool
	selectedCharacterID                  uint16
	pvpRoom                              *currentPVPRoom // protected by Service.pvpMu
	assault                              *currentAssault // protected by Service.pvpMu
	selectCharacterInitMu                sync.Mutex
	selectCharacterInitActive            bool
	selectCharacterInitCharacterID       uint16
	privateStoreStateMu                  sync.Mutex
	privateStorePending                  *currentPrivateStorePurchasePublication
	blackDiamondMu                       sync.RWMutex
	blackDiamondExpiresAt                time.Time
	dailyFatigueReset                    currentDailyFatigueResetClock
	selectedCollectboxIndex              byte
	selectedCollectboxCharacterID        uint16
	collectboxInitializationSent         bool
	collectboxSceneProjectionSent        bool
	collectboxSelectionRefreshSent       bool
	selectedEpicBookBootstrapCharacterID uint16
	rosterRequested                      bool
	activityNameMu                       sync.Mutex
	activityNameCharacterID              uint16
	activityName                         string
	channelReconnect                     bool
	// channelReconnectNewCharacter marks a reconnect transport that carried a
	// different character than the registered client last selected. It still
	// owns a fresh login: the select ACK and the town route must both fall back
	// to the ordinary initial town instead of the persisted reconnect location.
	channelReconnectNewCharacter         bool
	townActorOwnerChannel                byte
	representAccountNamePending          bool
	representAccountNameRegistrationSent bool
	pendingCharacterRosterBootstrap      bool
	emptyRosterSlotProbePending          bool
	selectorAdventureInfoPending         bool
	selectorAdventureInfoSent            bool
	selectorAdventureInfoSlot            uint16
	enterSelectDungeonSent               bool
	enterSelectDungeonAckSent            bool
	enterSelectDungeonPassiveParty       bool
	enterSelectDungeonContextSent        bool
	backToVillageEnterSelectPending      bool
	backToVillageSelectorDuplicateGuard  bool
	// dungeonEntryItemRefusedAt records when the server last answered an entry
	// attempt (or a 再次挑战) with the op16 "缺少入场材料" failure. The current
	// client answers that failure with its own bodyless op132 BACK_2_VILLAGE
	// reflex (measured 6~70ms later, live 2026-09-24 12:44:46 and 13:08:23), which
	// would throw the player back to town even though they only wanted to see the
	// prompt. handleDungeonBackToVillage swallows exactly one such reflex inside
	// currentDungeonShortageEscapeWindow and re-projects the dungeon-select page
	// instead; a later op132 (the player really clicking 返回城镇) returns to town
	// as usual. Guarded by dungeon.mu.
	dungeonEntryItemRefusedAt  time.Time
	sceneBootstrapTailMu       sync.Mutex
	sceneBootstrapTailDeferred bool
	sceneBootstrapTailSent     bool
	// blacklistListAnswerPending records that the client asked for its blacklist
	// (class-1 op120) while the select-character scene tail was still pending. The
	// answer is held back until the tail commits, because a row the client builds in
	// that window loses its 黑名单日期 (and its level): live 2026-09-22 23:46, the client
	// asked six times, the last three landed inside a route transition that had
	// re-deferred the tail, and the panel dropped the date the earlier answers had
	// already rendered. See sendRequestBlacklistState.
	blacklistListAnswerPending bool
	// premiumPanelFlagConsumed mirrors the client's one-shot "show the contract
	// panel" flag, which its own class1/op4 SELECT_CHARACTER handler arms and the
	// first class1/op903 body with serviceType 1 clears. Only the login/entry
	// scene tail sends serviceType 1; every later send stays silent so a session
	// whose tail never ran cannot pop that panel inside a dungeon (live
	// 2026-09-16T14:23:54Z, conn game-000209). atomic: the tail holds
	// sceneBootstrapTailMu while sending and every other sender reads this.
	premiumPanelFlagConsumed             atomic.Bool
	sceneBootstrapActorGeneration        uint64
	sceneBootstrapTailPacketIndex        int
	sceneBootstrapTailObjectMode1Pending bool
	sceneBootstrapTailPostStage          currentDeferredSelectSceneTailPostStage
	sceneBootstrapFriendListPacketIndex  int
	runtimeAfterBlacklistSent            bool
	runtimeFinishLoadingGateSent         bool
	fpsFinishLoadingGateSent             bool
	selectedUserInfoRefreshSent          bool
	selectedUserInfoMode3Sent            bool
	currentSceneObjectListSent           bool
	selectedItemListRefreshSent          bool
	selectedItemListBootstrapCharacterID uint16
	selectedEquipmentUpdateSent          bool
	selectedCreatureStateTableSent       bool
	selectedRentalWalletStateSent        bool
	expertJobInfoCharacterID             uint16
	// expertJobProjectionBody is the last class-0/op205 body this client
	// accepted. Scene reconstruction compares against it so an unchanged
	// auxiliary profession is never replayed as a fresh acquisition; see
	// sendCurrentExpertJobInfoForSceneRebuild.
	expertJobProjectionBody             []byte
	selectPreviewObjectStateSent        bool
	selectPreviewActorRemoved           bool
	preDungeonContextPlayerStateSent    bool
	postStartMapPlayerStateSent         bool
	dungeonSceneSkillInfoSent           bool
	trainingRoomBuffRegistrationPending bool
	deferredDungeonUserStateObjectKey   uint16
	currentFinishLoadingStateSent       bool
	currentFinishLoadingCompletionSent  bool
	postFinishLoadingPlayerStateSent    bool
	experienceConsumableProjectionSent  bool
	// itemCooltimeProjectionSent 保证每个选中角色只重放一次持久冷却列表
	// （类 0 通知 172，见 item_cooltime_projection.go）。
	itemCooltimeProjectionSent bool
	// statChangePotionBuffProjectionSent 保证"城镇增益药剂"（史诗Buff药剂一族）的 buff
	// 每个选中角色只补发一次（类 0 通知 487 CHARACTER_ADD_BUFF，见
	// stat_change_potion_projection.go；对应参考服务端 EpicBuffPotionInitBodyBuilder 的
	// 登录补发语义）。
	statChangePotionBuffProjectionSent bool
	// 城镇增益药剂的"到期摘图标"定时器状态（详见 stat_change_potion_expiry.go）。
	// 会话级：药剂在城镇也生效，所以它不属于副本 runtime。
	statChangePotionTimerMu                      sync.Mutex
	statChangePotionTimerActive                  bool
	statChangePotionTimerName                    string
	statChangePotionTimerGeneration              uint64
	statChangePotionTimerCharacterID             uint16
	statChangePotionTimerDueAt                   time.Time
	returnTownFinishLoadingAckOnly               bool
	confirmedDungeonReturnStatePending           bool
	confirmedDungeonReturnActionTableSent        bool
	confirmedDungeonReturnFinalGateResetSent     bool
	confirmedDungeonReturnRouteArmed             bool
	confirmedDungeonReturnTransition             currentDungeonTownTransition
	initialTownRouteCharacterID                  uint16
	initialTownRouteStage                        currentInitialTownRouteStage
	initialTownGuardianGemRefreshSent            bool
	initialTownGuardianShieldDeckSent            bool
	initialTownGuardianShieldAppearanceSent      bool
	chatEmoticonCatalogSent                      bool
	initialTownActorSceneSnapshotSent            bool
	initialTownLocationNotificationsSent         bool
	initialTownQuestSnapshotsSent                bool
	initialTownQuestHUDRefreshSent               bool
	initialTownGuildStateProjectionSent          bool
	initialTownSkillInfoPrepared                 bool
	initialTownSkillInfoSent                     bool
	initialTownSkillInfo                         currentSceneSkillInfoProjection
	initialTownMode3RefreshPending               bool
	completedTownReturnBuffRegistrationPending   bool
	skillSwitchInventoryAllDataCharacterID       uint16
	skillSwitchInventoryAllDataGeneration        uint64
	initialTownLegacySceneReadyAccepted          bool
	initialTownGameOptionRestoreCharacterID      uint16
	initialTownActorUserStateRestoreCharacterID  uint16
	currentGameOptionMu                          sync.Mutex
	currentGameOptionMainRestoreCharacterID      uint16
	currentGameOptionNativeApplyBody             []byte
	currentGameOptionNativeApplyKeymapBody       []byte
	currentGameOptionNativeApplyMainAt           time.Time
	currentGameOptionNativeApplyKeymapAt         time.Time
	initialTownCharacterOptionRestoreCharacterID uint16
	initialTownCharacterOptionRestoreBody        []byte
	initialTownCharacterOptionRestoreAt          time.Time
	initialTownCharacterOptionCorrectionSent     bool
	// characterOptionActorApplyCharacterID marks the character whose option
	// table has already been replayed once the actor exists. The scene-init
	// send lands before that and the client's own applier is a no-op then, so
	// the replay is what makes the saved skill-cooldown list take effect.
	characterOptionActorApplyCharacterID uint16
	// dailyQuestReset* remember the character and service day whose daily
	// quests were already reconciled on this session, so the login-read
	// catch-up runs once per day instead of on every selected-character read.
	dailyQuestResetCharacterID              uint16
	dailyQuestResetServiceDayID             int64
	initialTownAdventureOverheadRefreshSent bool
	initialTownActorDisplaySent             bool
	initialTownActorPlacementSent           bool
	initialTownDungeonPermissionSent        bool
	initialTownCombatPowerAffixesSent       bool
	initialTownMoodProjectionSent           bool
	townPostTransition                      townPostTransitionState
	crystalContractMu                       sync.Mutex
	crystalContractTownUIReadyStateSent     bool
	auraSkinMu                              sync.Mutex
	auraSkinTownUIReadyStateSent            bool
	townSceneReadyCharacterID               uint16
	townPositionSnapshot                    currentTownPositionSnapshot
	townMeetingNearbyNPCs                   map[int64]struct{}
	townPrevVillageSnapshot                 currentTownPositionSnapshot
	townSelectorOriginSnapshot              currentTownPositionSnapshot
	townSelectorOriginBound                 bool
	// townSelectorRetiredPresence remembers the town roster record that opening
	// the dungeon-selection page retired, so closing the page can register the
	// exact same actor again. It has its own leaf mutex rather than townMu:
	// every route that gives the character a real registration back drops the
	// memo, and several of them (SET_USER_AREA, prev-village, the initial town
	// route) already hold townMu across their whole body.
	townSelectorPresenceMu                 sync.Mutex
	townSelectorRetiredPresence            onlinePlayerInfo
	townSelectorRetiredPresenceCharacterID uint16
	townTransportEnterSelectPending        bool
	returnSelectTownReentryPending         bool
	returnSelectPreviousCharacterID        uint16
	returnSelectRetainedActorRemoved       bool
	// accountSharedInventoryRebindPending is armed by a return-to-selector
	// (op7). The next selected character's HUD-ready boundary replays only the
	// account-owned crystal/soul rows (354..365) through op14 so the surviving
	// right-hand widgets rebind after the container reset.
	accountSharedInventoryRebindPending bool
	// A completed/interrupted dungeon return deliberately suppresses mode0's
	// one-shot actor-state event because the dungeon actor generation is still
	// installed. The current client can keep that event's native source vector
	// empty after op7 tears the actor down; remember the condition across the
	// selector reset so the first replacement actor does not dereference it.
	returnSelectActorStateEventSuppressionPending bool
	questReplay                                   questReplayState
	// questProgress serializes durable dungeon quest writes while keeping
	// repository I/O off the packet/session event owner. Shutdown drains the
	// wait group so an acknowledged kill or quest-item grant is not lost.
	questProgressMu        sync.Mutex
	questProgressCond      *sync.Cond
	questProgressQueued    uint64
	questProgressPersisted uint64
	questProgressWG        sync.WaitGroup
	guild                  guildSessionState
	party                  partySessionState
	// guildBuffProjection remembers the two 公会 BUFF rows (Guild_Status /
	// Guild_Status1) this session's HUD currently shows, so a re-projection can
	// send the exact op488 removals before the op487 adds. Guarded by its own
	// mutex; see guild_buff_projection.go.
	guildBuffProjection currentGuildBuffProjection
	// partyRoutingKey is the server-assigned identity published in the login
	// success packet and repeated by remote peers' class0/op11 rows.  It is
	// connection-scoped: sharing one global token makes NoPack bind both party
	// actors to the same reliable-UDP route and leaves one HUD row at 0/4.
	partyRoutingKey uint32
	partyPathMTU    uint32
	partyPeerMu     sync.Mutex
	partyPeer       currentPartyPeerEndpointRegistration
	// partyActorProjection is the last complete, owner-produced mode0/mode1
	// pair that this selected character successfully wrote to its own client.
	// Party dungeon admission freezes these exact rows; rebuilding another
	// member's row from the receiver's scene context changes native actor
	// identity and breaks durable peer state such as riding/transformation and
	// hit ownership.
	partyActorProjectionMu sync.RWMutex
	partyActorProjection   currentPartyActorProjection
	lottery                lotteryState
	vendingMachine         currentVendingMachineSessionState
	dungeon                dungeonSessionState
	// potionCast pairs a committed dungeon consumable delete (op18) with the
	// cast (op38) the client sends right after it. It has its own leaf mutex:
	// the delete is recorded from the aligned command path, which already holds
	// other session locks.
	potionCast currentPotionCastState
	townMu     sync.Mutex
	petGrowth  petGrowthClockState
	spendTime  currentSpendTimeClockState
	// 活动（onlineevent）的会话级投影缓存。三者都按"内容变化才下发"的语义使用：
	// monthlyAttendance / weeklyAttendance 钉住上一次下发的 op1206 窄记录，
	// returneeProjection 钉住上一次下发的 176 字节回归状态体。
	monthlyAttendance            currentMonthlyAttendanceProjection
	weeklyAttendance             currentWeeklyAttendanceProjection
	returneeProjection           currentReturneeProjection
	returneeTimeCharacter        uint16
	returneeTimeGeneration       uint64
	returneeTimeExpiresAt        int64
	returneePreparedExpiresAt    int64
	returneePreparedCharacter    uint16
	returneePreparedGeneration   uint64
	returneeActive               bool
	luckyPreparedCharacter       uint16
	luckyPreparedGeneration      uint64
	luckyPreparedActorGeneration uint64
	returneeConnectionObserved   bool
	commandFallback              currentCommandFallbackSessionState
	darkKnightCombo              darkKnightComboSessionState
	customAbilityConverter       currentCustomAbilityGroupConverterSessionState
}

type guildSessionState struct {
	mu                           sync.Mutex
	pendingName                  string
	nameChecked                  bool
	promotion                    string
	createCommitted              bool
	createInvitation             *currentGuildCreateInvitation // guarded by Service.guildCreateMu
	memberListRequested          bool
	recommendationReplayPending  bool
	recommendationReplayGuildIDs []uint32
}

// partySessionState 持有组队投影与待处理邀请的会话状态。
// mu 守护 state 与 pending 邀请字段；state 的地址会传给 alignedcmd.Route，
// 迁移后仍为同一字段地址，锁序（party 锁内不嵌套 wireMu）保持不变。
// partySessionState is a per-client packet projection only. Membership,
// invitations, slots, leader replacement, and session generations are owned
// by party.RuntimePartyManager; this cache exists solely to shape the current
// EXE's existing party packets.
type partySessionState struct {
	mu    sync.Mutex
	state alignedcmd.PartyState
	// frameIdentities remembers, per projected actor, the identity of the last
	// populated op9 kind-0 party record this receiver accepted. The client keys
	// its party object by the (ownerChannel, objectKey) pair - current EXE
	// sub_26A1200 builds an eight-byte lookup key from both arguments - and the
	// populated record is addressed to the shared party model id, never to the
	// actor's scene key. The record that empties that object therefore has to
	// repeat the same pair, and the party id is already gone from every state
	// this session holds by the time the clearing record is built.
	frameIdentities map[uint16]runtimePartyFrameIdentity
}

// runtimePartyFrameIdentity is the (objectKey, ownerChannel) pair one populated
// op9 kind-0 party record was addressed to.
type runtimePartyFrameIdentity struct {
	modelKey     uint16
	ownerChannel byte
}

// questReplayState 持有任务接受/触发/完成三条回放的幂等去重状态。
// 每把锁各自守护对应 map 的懒初始化与读写，三把锁互不嵌套，保持原有临界区。
type questReplayState struct {
	acceptMu           sync.Mutex
	acceptAcknowledged map[currentAcceptQuestReplayKey]struct{}
	giveUpMu           sync.Mutex
	giveUpAnswered     map[currentGiveUpQuestReplayKey]struct{}
	triggerMu          sync.Mutex
	triggerNoop        map[currentSetQuestTriggerReplayKey]struct{}
	finishMu           sync.Mutex
	finishAnswered     map[currentFinishQuestReplayKey]struct{}
}

// lotteryState 持有魔盒抽奖待确认流程的会话状态。
// mu 守护待确认标记、栏位、双倍标记与发起时间，消费时清零的时机保持不变。
type lotteryState struct {
	mu            sync.Mutex
	pending       bool
	pendingSlot   int16
	pendingDouble bool
	pendingAt     time.Time
}

// petGrowthClockState 持有宠物饱食度时钟的会话状态。
// mu 守护时钟模式、锚点、代数与定时器名的全部读写（含 timequeue 回调），
// 停止/切换时的结算与归零时机保持不变。
type petGrowthClockState struct {
	mu          sync.Mutex
	mode        currentPetGrowthClockMode
	characterID uint16
	anchor      time.Time
	generation  uint64
	timerName   string
}

// townPostTransitionState 持有城镇入场后过渡流程的会话状态。
// mu 守护过渡代数、角色、属主频道、阶段与来源标记，arm/推进/归零时机保持不变。
type townPostTransitionState struct {
	mu           sync.Mutex
	generation   uint64
	characterID  uint16
	ownerChannel byte
	stage        currentTownPostTransitionStage
	source       string
	mode0Body    []byte
	mode1Body    []byte
}

// dungeonSessionState 持有单个地下城运行周期的会话状态。
// mu 守护 runtime、runToken 与 deathTower 的全部读写（含定时器回调与
// handler 路径），原有 dungeonMu 的临界区与锁序惯例保持不变。
type dungeonSessionState struct {
	mu                    sync.Mutex
	runtime               *runtimeDungeonState
	runToken              uint64
	deathTower            *deathTowerRuntime
	debugLightningPending bool
}

func (s *Service) handleGameConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	connID := s.nextPacketConnID("game")
	channel, ok := s.channelForConn(conn)
	if !ok {
		s.logPacketEvent("game-connect-rejected",
			"conn_id", connID,
			"remote", conn.RemoteAddr().String(),
			"local", conn.LocalAddr().String(),
			"reason", "local_port_not_in_channel_catalog")
		s.logWarn("dnfbridge rejected game connection for unknown channel port",
			"remote", conn.RemoteAddr().String(),
			"local", conn.LocalAddr().String())
		return
	}
	session := &gameSession{
		conn:            conn,
		connID:          connID,
		channel:         channel,
		residentChannel: channel,
		// The numeric channel belongs to the transport/session.  Until the
		// current EXE commits a different scene context through its native
		// protocol, the selected local actor is owned by context zero.
		townActorOwnerChannel: currentSceneObjectContext,
		ctx:                   ctx,
		stall:                 newDungeonStallTracker(),
	}
	s.assignPartyTransportIdentity(session)
	defer s.disconnectCurrentPremiumWeb(session)
	// Free the online seat when this connection closes no matter how the
	// handler returns (rejected auth, bootstrap failure, read error, ...).
	defer s.releaseOnlineSeat(session)
	accountBinding := "instance-fallback"
	lidEnforcing := s.launcherIdentity.enforcing()
	if pid, found := localTCPConnectionOwnerPID(conn); found {
		session.clientPID = pid
		// Under an enforced launcher identity the PID registry is read through
		// its attested view: a registration that carries no signed lease is a
		// local process that merely reached the loopback admin token, and it must
		// not seat a session.
		if lidEnforcing {
			accountID, registered, direct, attested := s.waitAttestedClientAccountForPID(
				ctx,
				pid,
				clientAccountRegistrationGrace,
			)
			if registered && attested {
				session.accountID = accountID
				if direct {
					accountBinding = "lid-pid"
				} else {
					accountBinding = "lid-pid-ancestor"
				}
			} else if registered {
				s.logPacketEvent("game-lid-rejected",
					"conn_id", session.connID,
					"remote", conn.RemoteAddr().String(),
					"client_pid", pid,
					"reason", "pid-registered-without-launcher-identity")
			}
		} else if accountID, registered, direct := s.waitRegisteredClientAccountForPID(
			ctx,
			pid,
			clientAccountRegistrationGrace,
		); registered {
			session.accountID = accountID
			if direct {
				accountBinding = "pid"
			} else {
				accountBinding = "pid-ancestor"
			}
		} else if !s.launcherIdentity.enabled() {
			if image, imageOK := localProcessImagePath(pid); imageOK && isCurrentGameClientImage(image) {
				// The launcher registers PID->account once per client launch and the
				// registry lives in memory. After a server restart the running
				// client's registration is gone, so its next connection (a channel
				// switch, a return from the selector) arrived unbound and the
				// bootstrap treated it like a launcher discovery probe: no
				// CHANNELINFO, town-actor owner 0, and the camera stopped following
				// the character. The socket owner is provably the game client, and
				// the profile is local single-account, so bind it to the instance
				// account the way accountIDForSession already resolves it later.
				//
				// This fallback is exactly the hole the launcher identity closes:
				// the image basename is not a credential, so any process named
				// dnf.exe would inherit the instance account. It therefore stays
				// reachable only while no launcher identity is configured.
				session.accountID = s.accountID()
				accountBinding = "pid-client-image-instance-account"
			}
		}
	} else if remoteIP := currentPartySessionRemoteAddress(session); remoteIP.IsValid() &&
		remoteIP.IsPrivate() && !remoteIP.IsLoopback() &&
		(!lidEnforcing || s.options.launcherIdentityAllowWireGuard) {
		if accountID, remotePID, registered := s.waitRegisteredRemoteClientAccount(
			ctx,
			remoteIP,
			clientAccountRegistrationGrace,
		); registered {
			session.accountID = accountID
			session.accountSourceIP = remoteIP
			session.remoteClientPID = remotePID
			accountBinding = "wireguard-source-ip"
		}
	}
	if session.accountID == "" && session.clientPID == 0 && wineenv.Detected() && !lidEnforcing {
		// Wine may return the TCP row without an owning PID, or omit the peer
		// row entirely. The project profile is local-single-account: accept the
		// launcher's authenticated account only when all live registrations
		// agree, and otherwise retain the configured instance fallback. Never
		// guess a PID because party/session process identity depends on it.
		//
		// An enforced launcher identity disqualifies this path outright: Wine
		// exposes no owning PID, so the registration cannot be tied back to the
		// connection that is asking for it.
		if accountID, registered := s.waitUniqueRegisteredClientAccount(
			ctx,
			clientAccountRegistrationGrace,
		); registered {
			session.accountID = accountID
			accountBinding = "wine-single-account"
		}
	}
	// External launcher auth gate. Local PID / WireGuard / Wine bindings
	// above already claimed this session, so a connection that still has no
	// account here is an external peer: challenge it for a launcher ticket,
	// verify through the loopback launcher API and bind the returned
	// account_id. Any failure terminates the connection before the native
	// DNF protocol flow starts.
	//
	// Both switches open this block. launcherAuthEnabled is the ticket path; the
	// launcher identity is the signed-lease path, and it must be reachable on its
	// own because a deployment can enforce the binding without running the ticket
	// verifier. Pure audit mode is the one case that keeps serving the legacy
	// path after a failed challenge, so an operator can watch real traffic before
	// switching to enforce.
	if session.accountID == "" &&
		(s.options.launcherAuthEnabled || s.launcherIdentityWantsInBandChallenge(conn)) {
		err := s.runLauncherAuthGate(ctx, session)
		switch {
		case err == nil:
			accountBinding = "remote-auth"
		case !s.options.launcherAuthEnabled && !s.launcherIdentity.enforcing():
			// Audit only: the verdict was recorded by the gate and the legacy
			// resolution below still applies, so nothing that worked before this
			// change stops working.
			accountBinding = "lid-audit-fallthrough"
			s.logPacketEvent("game-lid-audit-fallthrough",
				"conn_id", session.connID,
				"remote", conn.RemoteAddr().String(),
				"error", err)
		default:
			// The gate recorded the auth-start/ok/reject/timeout/frame/verify
			// events; reuse the plain return so the deferred conn.Close() and
			// session cleanup behave exactly like the rejected paths above.
			return
		}
	}
	// An enforced launcher identity makes an unbound session fatal. Without this
	// the connection would fall through to the instance-account fallback in
	// accountIDForSession, which is precisely the bypass the binding exists to
	// remove.
	if lidEnforcing && session.accountID == "" {
		s.logPacketEvent("game-lid-rejected",
			"conn_id", session.connID,
			"remote", conn.RemoteAddr().String(),
			"local", conn.LocalAddr().String(),
			"channel_id", channel.ID,
			"client_pid", session.clientPID,
			"reason", "no-launcher-identity-lease")
		s.logWarn("dnfbridge rejected game connection without a launcher identity lease",
			"remote", conn.RemoteAddr().String(),
			"client_pid", session.clientPID)
		return
	}
	// Occupy the online seat as soon as an account is bound, before any
	// character select/bootstrap. The capacity gate counts connecting and
	// role-select connections too, so a full server rejects the next login
	// instead of overselling the last seat into an in-flight session.
	s.occupyOnlineSeat(session)
	s.logInfo("dnfbridge game connection accepted",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String(),
		"channel_id", channel.ID,
		"channel_type", channel.Type,
		"game_port", channel.Port,
		"client_pid", session.clientPID,
		"remote_client_pid", session.remoteClientPID,
		"session_account_bound", session.accountID != "",
		"account_binding", accountBinding)
	s.logPacketEvent("game-connect",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String(),
		"channel_id", channel.ID,
		"channel_type", channel.Type,
		"game_port", channel.Port)
	if err := s.startGameSessionEvents(ctx, session); err != nil {
		s.logPacketEvent("game-session-events-start-failed", "conn_id", session.connID, "error", err)
		s.logWarn("dnfbridge start game session events failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}
	dprotoOpened := false
	defer func() {
		s.shutdownGameSessionEvents(session, dprotoOpened)
	}()
	// Inbound packets are submitted asynchronously below. Put a FIFO barrier
	// ahead of session shutdown so a peer close cannot discard commands which
	// were already fully read from the socket and accepted by the bounded
	// session queue.
	defer func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), gameSessionShutdownWait)
		defer drainCancel()
		if err := s.callGameSession(drainCtx, session, "game-inbound-drain", func() error { return nil }); err != nil &&
			!isClosedGameSessionEventError(err) {
			s.logGameEvent(session, "game-inbound-drain-failed", "error", err)
		}
	}()
	if err := s.callGameSession(ctx, session, "game-dproto-open", func() error {
		return s.openGameDprotoSession(ctx, session)
	}); err != nil {
		s.logPacketEvent("game-dproto-open-failed", "conn_id", session.connID, "error", err)
		s.logWarn("dnfbridge open native dproto session failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}
	dprotoOpened = true
	defer s.logPacketEvent("game-close",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String(),
		"channel_id", channel.ID,
		"game_port", channel.Port)
	if err := s.callGameSession(ctx, session, "game-connection-bootstrap", func() error {
		return s.sendGameConnectionBootstrap(session, time.Now())
	}); err != nil {
		s.logPacketEvent("game-connection-bootstrap-failed",
			"conn_id", session.connID,
			"remote", conn.RemoteAddr().String(),
			"local", conn.LocalAddr().String(),
			"channel_id", channel.ID,
			"error", err)
		s.logWarn("dnfbridge send game connection bootstrap failed", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}

	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		if err != nil {
			quiet := isQuietGameReadError(err)
			s.logPacketEvent("game-read-close",
				"conn_id", session.connID,
				"remote", conn.RemoteAddr().String(),
				"local", conn.LocalAddr().String(),
				"channel_id", channel.ID,
				"pending_bytes", len(buffer),
				"quiet", quiet,
				"error", err)
			if !quiet && ctx.Err() == nil {
				s.logWarn("dnfbridge read game packet failed", "remote", conn.RemoteAddr().String(), "error", err)
			}
			return
		}
		if n > 0 {
			s.logGamePacket(session, "RECV", "game-raw", chunk[:n])
		}
		buffer = append(buffer, chunk[:n]...)
		packets, remaining, skipped, err := dnfproto.SplitLatestGameStream(buffer, s.options.maxPacketBytes)
		if err != nil {
			s.logPacketEvent("game-split-error",
				"conn_id", session.connID,
				"remote", conn.RemoteAddr().String(),
				"local", conn.LocalAddr().String(),
				"channel_id", channel.ID,
				"buffer_bytes", len(buffer),
				"error", err)
			s.logWarn("dnfbridge split game stream failed", "remote", conn.RemoteAddr().String(), "error", err)
			return
		}
		if skipped > 0 {
			s.logWarn("dnfbridge game frame resynced", "remote", conn.RemoteAddr().String(), "skipped", skipped)
		}
		buffer = remaining
		for _, packet := range packets {
			packet := packet
			if err := s.postGameSessionEvent(ctx, session, "game-stream-packet", func() error {
				return s.handleGameStreamPacket(session, packet)
			}); err != nil {
				// A handler error is a bug in one command, not a transport failure.
				// Closing the socket over it turns any single packet mistake into
				// "网络连接中断" for the player, and that happened three times on
				// 2026-09-16 alone -- the raid scene rejoin, the party dungeon
				// settlement whose peer had already dropped, and a raid roster
				// projection. The session stays up, the offending command is logged
				// and dropped, and the player keeps playing.
				s.logPacketEvent("game-handler-error",
					"conn_id", session.connID,
					"remote", conn.RemoteAddr().String(),
					"channel_id", channel.ID,
					"stream_kind", uint8(packet.Kind),
					"error", err)
				s.logWarn("dnfbridge game handler failed",
					"remote", conn.RemoteAddr().String(),
					"kind", uint8(packet.Kind),
					"error", err)
				// The one error that still ends the read loop is the session's own
				// event loop going away: that is the connection being torn down, not
				// a command failing.
				if session.events != nil && !session.events.active.Load() {
					return
				}
			}
		}
	}
}

func (s *Service) channelForConn(conn net.Conn) (channelcatalog.Channel, bool) {
	port := 0
	if endpoint, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		port = endpoint.Port
	}
	return s.channelForPort(port)
}

func (s *Service) channelForPort(port int) (channelcatalog.Channel, bool) {
	catalog := s.currentCatalog()
	if catalog == nil {
		return channelcatalog.Channel{}, false
	}
	return catalog.ForPort(port)
}

func (s *Service) gameListenPorts(catalog *channelcatalog.Catalog) []int {
	if catalog == nil {
		return nil
	}
	return catalog.GamePorts()
}

func (s *Service) handleGameStreamPacket(session *gameSession, packet dnfproto.LatestGameStreamPacket) error {
	switch packet.Kind {
	case dnfproto.LatestGameStreamTransport:
		return s.handleGameFrame(session, packet.Data)
	case dnfproto.LatestGameStreamUpper:
		return s.handleGameUpper(session, packet.Data)
	case dnfproto.LatestGameStreamLegacy:
		return s.handleLegacyGamePacket(session, packet.Data)
	case dnfproto.LatestGameStreamDproto:
		return s.handleGameDprotoPacket(session, packet.Data)
	default:
		return dnfproto.ErrPacketLength
	}
}

func isQuietGameReadError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET)
}
