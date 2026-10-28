// 本文件负责背包 owner 的真实库存变更边界。
// handler 只提交命令；这里只写 repository，不生成旧客户端成功 ACK。
package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/creaturestate"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	"longheng.io/server/internal/modules/dnf/experiencebonus"
	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	"longheng.io/server/internal/modules/dnf/itemquality"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var (
	ErrOwnerUnavailable                    = errors.New("inventory owner unavailable")
	ErrCharacterRequired                   = errors.New("selected character id required")
	ErrInventoryNotFound                   = errors.New("inventory record not found")
	ErrUnsupportedList                     = errors.New("inventory list type is not supported")
	ErrSlotNotFound                        = errors.New("inventory slot not found")
	ErrItemMismatch                        = errors.New("inventory item mismatch")
	ErrSellPriceMissing                    = errors.New("sell price evidence is missing")
	ErrSellPriceInvalid                    = errors.New("sell price is invalid")
	ErrWalletTxnRequired                   = errors.New("wallet transaction is required")
	ErrItemLocked                          = errors.New("inventory item is equipment locked")
	ErrMoveRequiresEquipmentOwner          = errors.New("move requires equipment owner")
	ErrQuickSlotCharmAlreadyEquipped       = errors.New("only one quick-slot charm equipment may be equipped")
	ErrQuickSlotCharmValidationUnavailable = errors.New("quick-slot charm validation requires the active PVF catalog")
	ErrMoveAccountCargoUnsupported         = errors.New("account cargo move is unsupported")
	// ErrMoveDestinationFull 表示跨容器移动时目标容器既没有该物品能落进去的空格，
	// 也没有同类可叠格，而且客户端声明的那一格放不下这一堆。此时必须回一个明确的
	// 失败包（客户端显示"物品栏空间不足"），不能再互换：op19 的 ACK 只带一个目标槽，
	// 被换出去的那件在客户端会一直"消失"到小退。
	ErrMoveDestinationFull = errors.New("move destination container has no room for the item")
	// ErrMoveCarryLimitExceeded 表示目标容器里该道具的总量已经达到 PVF [stack limit]。
	// 口径：一个容器（背包 list0 / 个人仓库 list2 / 账号金库 list12）里同一道具 id 的
	// 总量不得超过 [stack limit]；PVF 没有声明 [stack limit] 的道具（装备等）不受限。
	ErrMoveCarryLimitExceeded     = errors.New("move would exceed the PVF stack limit for that container")
	ErrAccountCargoNotCreated     = errors.New("account cargo is not created")
	ErrAccountCargoSlotOutOfRange = errors.New("account cargo slot is outside current capacity")
	// ErrAccountCargoItemNotAllowed 表示这件道具不允许存进账号金库（list 12）。
	// 判定来自当前运行时 PVF 的 [attach type]：只有 free / account / sealing 放行，
	// 其余（trade / trade delete / trade limit / sealing trade / 空值）以及"读不出来"
	// （没有 PVF 解析器、模板不在 stackable/equipment 列表里）一律按不可交易拒绝。
	ErrAccountCargoItemNotAllowed = errors.New("item is not allowed in the account cargo")
	// ErrCrossContainerSwapNotAllowed 表示这次移动唯一的出路是"跨容器互换"，而运营口径
	// 2026-09-21 起跨容器一律不互换：互换会把对方容器里那件搬进来（账号金库这边就是
	// "任务道具/SS 装备跟着被拖进金库"），而 op19 的 ACK 只带一个目标槽，客户端不知道
	// 被换出去的东西去了哪。目标容器还有空格时走的是顺延落点，不会走到这里。
	ErrCrossContainerSwapNotAllowed = errors.New("cross-container slot swap is not allowed")
	// ErrEmblemWarehouseForbidden 表示徽章（PVF [stackable type] = [avatar emblem]）
	// 不允许存进仓库。
	//
	// 运营口径 2026-10-03：徽章页 289..351 是它唯一该在的地方（9 行 × 7 格 = 63），
	// 放进个人仓库/账号金库会让"徽章栏满了"这个状态被绕过 —— 玩家把几摞徽章挪进仓库，
	// 页面上就空出格子，而分解产出与邮件领取都只按徽章页找落点，页面状态与真实持有量
	// 从此对不上。所以**放入一律拒绝**；历史已经躺在仓库里的那些不拦取出（否刚玩家
	// 的徽章就锁死在仓库里了），拖出来时会按 PVF 页落回徽章页
	// （见 redirectMainMoveToItemPage）。
	ErrEmblemWarehouseForbidden            = errors.New("avatar emblem is not allowed in a warehouse")
	ErrSortRequiresEquipmentOwner          = errors.New("sort requires equipment owner")
	ErrRepairRequiresEquipmentOwner        = errors.New("repair requires equipment owner")
	ErrRepairAllRequiresOwners             = errors.New("repair all requires equipment and wallet owners")
	ErrRepairDurabilityMissing             = errors.New("repair durability evidence is missing")
	ErrRepairDurabilityInvalid             = errors.New("repair durability evidence is invalid")
	ErrRepairCostMissing                   = errors.New("repair cost evidence is missing")
	ErrRepairCostInvalid                   = errors.New("repair cost evidence is invalid")
	ErrDeleteRequiresEquipmentOwner        = errors.New("equipment delete requires a typed equipment owner and proven ACK")
	ErrPetInventoryOwnerRequired           = errors.New("pet inventory mutation requires a typed pet owner")
	ErrUseStackableContractRequired        = errors.New("use stackable requires PVF-proven [waste] provenance and stable item identity")
	ErrStackableUsePlaceContract           = errors.New("stackable item is not usable in the active PVF scene")
	ErrTransformationContract              = errors.New("transformation item requires a complete runtime-PVF action and compatible scene")
	ErrProjectileContract                  = errors.New("projectile item requires a complete runtime-PVF action and compatible dungeon")
	ErrPetFeedResolveFailed                = errors.New("pet feed runtime-PVF resolution failed")
	ErrPetFeedContractRequired             = errors.New("pet feed requires a complete runtime-PVF [feed] contract")
	ErrPetFeedEquippedCreatureRequired     = errors.New("pet feed requires one equipped creature")
	ErrInventoryTransactionRequired        = errors.New("inventory mutation requires a character item transaction")
	ErrCharacterAssetTxnRequired           = errors.New("inventory mutation requires a character asset transaction")
	ErrAccountSharedOwnerUnavailable       = errors.New("account-shared inventory owner unavailable")
	ErrReservedSlotRelocationFull          = errors.New("reserved main inventory slot cannot be relocated into the proven item range")
	ErrAccountRequired                     = errors.New("authenticated account id required")
	ErrAccountNotFound                     = errors.New("authenticated account record not found")
	ErrEnchantResolverRequired             = errors.New("enchant by bead requires a runtime-PVF resolver")
	ErrEnchantRequiresEquipmentOwner       = errors.New("worn equipment enchant requires a typed equipment owner")
	ErrUpgradeTicketResolverRequired       = errors.New("upgrade ticket requires a runtime-PVF resolver")
	ErrPremiumContractResolveFailed        = errors.New("premium contract runtime-PVF resolution failed")
	ErrExperienceConsumableResolveFailed   = errors.New("experience consumable runtime-PVF resolution failed")
	ErrExperienceConsumableDungeonRequired = errors.New("experience consumable requires an active dungeon")
	ErrDirectExperienceConsumableContract  = errors.New("direct experience consumable requires an atomic PVF progression contract")
	ErrTPBookContract                      = errors.New("TP book requires an atomic PVF character, skill, and inventory contract")
	ErrLevelUpTicketContract               = errors.New("level-up ticket requires an atomic progression, quest, and inventory contract")
	ErrIncreaseStatusBookContract          = errors.New("increase-status book requires a runtime-PVF SP/TP or immediate-experience book in the addressed main-inventory slot")
	ErrRandomRewardResolveFailed           = errors.New("random reward item runtime-PVF resolution failed")
	ErrRandomRewardContractRequired        = errors.New("random reward item requires a complete runtime-PVF outcome table")
	ErrRandomRewardInventoryFull           = errors.New("random reward item result has no compatible inventory slot")
	ErrUpgradeDestroyBonusContract         = errors.New("upgrade destruction compensation requires a complete runtime-PVF stack contract")
	ErrUpgradeDestroyBonusInventoryFull    = errors.New("upgrade destruction compensation has no compatible inventory slot")
	ErrQuestCompletionTicketResolveFailed  = errors.New("quest completion ticket runtime-PVF resolution failed")
	ErrQuestCompletionTicketContract       = errors.New("quest completion ticket requires an atomic quest/inventory contract")
	ErrFatiguePotionContract               = errors.New("fatigue potion requires an atomic character/inventory contract")
	ErrRentalPointContract                 = errors.New("rental point item requires an atomic account/character/inventory contract")
	ErrRentalPointLimit                    = errors.New("rental point wallet is already at its limit")
	ErrStoryBookContract                   = errors.New("story book requires an atomic quest/inventory contract")
	ErrDungeonAdmissionTicketContract      = errors.New("dungeon admission ticket requires an atomic allowance/inventory contract")
	ErrRepairNotRepairable                 = errors.New("inventory item is not repairable by PVF durability evidence")
	ErrRepairGoldInsufficient              = errors.New("repair gold is insufficient")
	ErrMonsterCardUpgradeResolverRequired  = errors.New("monster card upgrade requires a runtime-PVF resolver")
)

// op80 error codes. The current EXE's class-1 handler sub_1D2F410 takes the
// code at [ebp+0x10] and, after the shared table sub_32DDC70 declines it,
// indexes the 242-entry byte table at 0x1D304A0 into the 24-way jump table at
// 0x1D30440. Only 0 4 7 8 9 10 13 14 17 19 21 22 23 95 103 104 118 126 175
// 179 206 214 217 235 241 have a branch; everything else lands on the default
// at 0x1D3032F, which is dstr 1662 "强化装备出现错误， 错误代码 : %d" -- the
// bare number a player reported on 2026-09-09.
//
// Each constant below names the dstr text its branch renders, read out of
// docs/client-dstr-zh.txt.
const (
	// 4 -> 1658 缺少物品
	upgradeErrorInvalidTarget byte = 4
	// 10 -> 1660 金币不足
	upgradeErrorInsufficientGold byte = 10
	// 4 -> 1658 缺少物品. This used to be 21, which renders 70132
	// "物品栏空间不足， 无法进行强化" -- a bag-space message for what is
	// actually an unusable protection ticket in the optional slot.
	upgradeErrorUnsupportedOptionalTicket byte = 4
	// 22 -> 1661 材料不足
	upgradeErrorInvalidMaterial byte = 22
	// 23 -> 1653 该道具无法进行强化和赋予异次元气息。
	upgradeErrorWrongMode byte = 23
	// 7 -> 20000 无法强化未修理的装备
	upgradeErrorDurability byte = 7
	// 95 -> 1654 该物品无法继续强化
	upgradeErrorMaxLevel byte = 95
	// 175 -> 21304 无法对未净化的装备进行装备增幅。 The tree used 174, one
	// below the real code, so the client printed "错误代码 : 174" instead.
	upgradeErrorAmplifyNotIdentified byte = 175
	// 214 -> 42232 在物品已锁定状态下， 您无法进行当前操作。 Was 213, likewise
	// one below.
	upgradeErrorLocked                     byte = 214
	upgradeResultCodeSuccess               byte = 0
	upgradeResultCodeFailNoChange          byte = 1
	upgradeResultCodeDowngrade             byte = 2
	upgradeResultCodeDestroy               byte = 3
	upgradeMaximumCurrentLevelBeforeReject byte = 30
	currentUpgradeSuccessBase                   = 100000
)

// Enchant-by-bead client-visible error codes, matching the current EXE S2C
// 0x0110 handler (sub_1D0B960): 0x11 -> text 22223, 0x13 -> text 46371,
// 0x17 -> text 27162; any other value falls back to the generic text 44171.
const (
	enchantErrorInvalidBead   byte = 0x11
	enchantErrorInvalidTarget byte = 0x13
	enchantErrorUnsupported   byte = 0x17
)

// Owner 是背包资产的 durable owner 边界。
// 当前只改 repository 状态；旧客户端 ACK/刷新顺序必须在 EXE 证据闭合后再开放。
type Owner struct {
	repo                dnfrepo.InventoryRepository
	equipment           dnfrepo.EquipmentRepository
	accountRepo         dnfrepo.AccountInventoryRepository
	accounts            dnfrepo.AccountRepository
	characters          dnfrepo.CharacterRepository
	items               dnfrepo.CharacterItemUnitOfWork
	accountItems        dnfrepo.AccountCharacterItemUnitOfWork
	assets              dnfrepo.CharacterAssetUnitOfWork
	accountAssets       dnfrepo.AccountCharacterAssetUnitOfWork
	rentalAssets        dnfrepo.RentalAssetUnitOfWork
	pets                dnfrepo.PetRepository
	petItems            dnfrepo.CharacterPetUnitOfWork
	settlements         dnfrepo.CharacterSettlementUnitOfWork
	dungeonAdmission    *dnfdungeonadmission.Owner
	chronicleRandom     func(int) int
	cardUpgradeRandom   func(int) int
	upgradeRandom       func(int) int
	statChangeRandom    func(int) int
	enchantRandom       func(int) int
	now                 func() time.Time
	stackMerge          alignedcmd.StackMergeResolver
	upgradeProtection   alignedcmd.UpgradeProtectionResolver
	upgradeRefundMailer alignedcmd.UpgradeRefundMailer
	inItemTransaction   bool
	inAccountItemTx     bool
	inPetTransaction    bool
	// settings 只用于读取角色的容器状态（已解锁的背包/仓库格范围）。op19 跨容器
	// 落点修正需要一个"证明可见"的槽位范围，读不到时一律不放宽判定。
	settings dnfrepo.SettingsRepository
	// accountWarehouseSlot 解析"这类道具的固定晶体/灵魂仓库格"（六个晶块 354..359、
	// 六个灵魂 360..365）。布局由 bridge 用实测表注入；本包只用它把"落点是背包"的
	// 晶体/灵魂改回固定格，见 redirectAccountSharedWarehouseDestination。
	accountWarehouseSlot func(itemID int64) (int16, bool)
}

// SetAccountWarehouseSlotResolver installs the fixed crystal/soul warehouse slot
// of the item families that own one (six cubes at 354..359, six souls at
// 360..365). dnfbridge owns the live-verified client layout; this package only
// applies the landing rule to op19 moves.
func (o *Owner) SetAccountWarehouseSlotResolver(resolver func(itemID int64) (int16, bool)) {
	if o != nil {
		o.accountWarehouseSlot = resolver
	}
}

// SetStackMergeResolver installs the active-runtime-PVF authority used by
// stack moves and upgrade-destruction rewards. It is intentionally
// request-scoped and is copied into transaction owners with the rest of Owner.
func (o *Owner) SetStackMergeResolver(resolver alignedcmd.StackMergeResolver) {
	if o != nil {
		o.stackMerge = resolver
	}
}

// SetUpgradeRefundMailer installs the delivery used for destruction refund
// rows the bag could not take. It runs after the asset transaction commits,
// because the character-asset transaction covers characters, inventory and
// equipment but not mailboxes, and the upgrade target may sit on the equipment
// page -- so the mail cannot share the upgrade's transaction. The callback
// owns its own logging; a failure never fails the already-committed upgrade,
// it only leaves the rows reported in UpgradeResult.DestroyRefundOverflow.
func (o *Owner) SetUpgradeRefundMailer(mailer alignedcmd.UpgradeRefundMailer) {
	if o != nil {
		o.upgradeRefundMailer = mailer
	}
}

// SetUpgradeProtectionResolver installs the request-scoped active-PVF proof
// for the exact optional protection-ticket slot carried by op50.
func (o *Owner) SetUpgradeProtectionResolver(resolver alignedcmd.UpgradeProtectionResolver) {
	if o != nil {
		o.upgradeProtection = resolver
	}
}

// DeleteResult 描述一次删除物品写库结果，不代表已经允许回包。
// DeletedAccountMaterial 仅在本次删除实际扣除了账号共享晶体/灵魂材料槽
// （dnfrepo.IsAccountSharedInventorySlot）时为真；是否处于副本由 bridge 判定。
type DeleteResult struct {
	CharacterID            string
	Removed                []DeletedItem
	Changed                bool
	DeletedAccountMaterial bool
}

// SellResult 描述一次出售物品写库结果；金币为 0 时只涉及背包 repo。
type SellResult struct {
	CharacterID string
	Sold        DeletedItem
	GoldDelta   int64
	UpdatedGold int64
	Changed     bool
}

// MoveResult 描述一次物品格子移动的写库结果；它不代表已经可以向旧客户端回成功 ACK。
type MoveResult struct {
	CharacterID          string
	SourceListType       byte
	SourceSlotIndex      int16
	DestinationListType  byte
	DestinationSlotIndex int16
	MoveCount            int64
	Mode                 string
	// RedirectedReservedMainSlot is true when the owner committed the move to an
	// endpoint other than the one the client declared, so the ACK (and any
	// precise slot refresh built from the same command) must use the committed
	// endpoints instead of the request's. Two cases set it: a personal-cargo
	// withdrawal that named the current client's slot-0 gold wallet, and a
	// cross-container move whose client-declared destination endpoint was empty
	// on the client but already occupied in the durable record.
	RedirectedReservedMainSlot bool
	RefreshListTypes           []byte
	Refresh                    map[byte]map[string]dnfrepo.ItemStack
	Changed                    bool
	// EmptiedAccountSharedSlots 是这次移动**搬空**的账号共享晶体/灵魂槽（list 0,
	// 354..365）。op19 的 ACK 只声明源/目标端点，而客户端不会据它重绘右侧晶体/灵魂
	// 面板，于是搬空的那一格会留下一行"幽灵行"：现场 2026-09-17，把无色小晶块从晶体
	// 仓库拖到个人仓库后，面板仍显示原来的 40449，且那一行点不动（东西已经不在仓库里）。
	// bridge 必须在 ACK 之后把这几格按 (itemID=0, count=0) 的"空格"行补发一次。
	EmptiedAccountSharedSlots []int16
	// ClearedRedirectedMainSlot 是**客户端声明的背包落点**、但被改写到账号共享固定格的
	// 那一格（list 0）。op19 是乐观移动：客户端在发包前就已经把物品落在它自己声明的那一格
	// 上了，而 ACK 只回执真实落点 —— 不补一条删除行，那一格会留下"幽灵行"，玩家再往外拖时
	// 客户端连 op19 都不发（2026-09-21 现场：晶体仓库→个人仓库→背包(声明 0:156) 之后
	// 那一格点不动，13:20:08 起该连接零报文；DB 里 0:156 是空的、0:357 才是 69 个）。
	//
	// 只有该格在持久化记录里**确实是空的**时才填：这样 bridge 补发的必然是一行删除行
	// （背包空格 = 0xFFFFFFFF），不会盖掉任何真实物品，也避开 2026-09-13 那条"补发物品行
	// 会被客户端当成额外变化套到它自己的 op19 对象上"的坑。
	ClearedRedirectedMainSlot int16
	// SortLockSignals 是移动后仍被整理锁定钉住的端点槽位，需要补发 class1/op713。
	SortLockSignals []SortLockSignal
}

// SortResult 描述一次整理物品栏的持久化结果。客户端列表刷新由 bridge
// 在 ACK 后从角色、账号共享仓库与金币所有者重新构造，不能复用这里只含角色
// 背包格子的局部快照。
type SortResult struct {
	CharacterID string
	ListType    byte
	Category    byte
	StartSlot   int16
	EndSlot     int16
	MovedCount  int
	Mode        string
	Changed     bool
}

// RepairResult 描述一次耐久修理的写库结果；当前只允许零金币单件修理回成功 ACK。
// 正价扣费、全修和装备刷新仍需 wallet/equipment owner 闭合后再开放。
type RepairResult struct {
	CharacterID   string
	ListType      byte
	SlotIndex     int16
	OldDurability int64
	NewDurability int64
	Cost          int64
	UpdatedGold   int64
	Changed       bool
	// FreeRepair marks the 魔王契约 auto-repair path (premium type 586).
	FreeRepair bool
}

// UpgradeResult describes one equipment reinforcement/amplification attempt.
// Success=false is still a handled client-visible result and maps to an op50
// failure ACK; Changed only means repository state changed.
type UpgradeResult struct {
	CharacterID string
	Mode        string
	Success     bool
	ErrorCode   byte
	// TargetListType is echoed at byte[14] of the op50 success ACK; the client
	// repaints the slot in the list named there, so a worn target must say 3.
	TargetListType               byte
	TargetSlotIndex              int16
	TargetItemTemplateID         int32
	MaterialSlotIndex            int16
	OptionalTicketSlotIndex      int16
	ProtectionTicketSlotIndex    int16
	ProtectionTicketItemID       int64
	ProtectionTicketUsed         bool
	ProtectionTicketRemaining    int64
	ProtectionTicketUpdatedStack dnfrepo.ItemStack
	OldLevel                     byte
	NewLevel                     byte
	ResultCode                   byte
	UpgradeSucceeded             bool
	MaterialRemainingStackCount  int64
	GoldCost                     int64
	UpdatedGold                  int64
	MainRefresh                  map[string]dnfrepo.ItemStack
	TargetUpdatedStack           dnfrepo.ItemStack
	MaterialUpdated              bool
	MaterialUpdatedStack         dnfrepo.ItemStack
	Changed                      bool
	DestroyBonusSlot             int16
	DestroyBonusItemID           int32
	DestroyBonusCount            int32
	DestroyBonusSlots            []int16
	// DestroyExtraRewards are the fixed rows granted after the compensation
	// item (异界 equipment refunds 异次元碎片); their slots are also appended
	// to DestroyBonusSlots for the incremental refresh.
	DestroyExtraRewards []UpgradeDestroyGrantedReward
	// DestroyRefundOverflow are best-effort refund rows the bag had no room
	// for. They are handed to the upgrade refund mailer after the transaction
	// commits; a row listed here was neither placed in a slot nor reported in
	// the op50 body.
	DestroyRefundOverflow []alignedcmd.UpgradeDestroyReward
	// DestroyRefundMailed is set once the mailer accepted the overflow.
	DestroyRefundMailed bool
}

// UpgradeDestroyGrantedReward is one granted destruction reward row as the
// op50 success body reports it.
type UpgradeDestroyGrantedReward struct {
	Slot   int16
	ItemID int32
	Count  int32
}

// EnchantResult describes one enchant-by-bead attempt. Success=false is still
// a handled client-visible result and maps to a 0x0110 failure ACK carrying
// ErrorCode; Changed only means repository state changed.
type EnchantResult struct {
	CharacterID         string
	Success             bool
	ErrorCode           byte
	TargetListType      byte
	TargetSlotIndex     int16
	TargetItemID        int64
	BeadSlotIndex       int16
	BeadItemID          int64
	CardItemID          int64
	EnchantUpgradeCount byte
	BeadRemainingCount  int64
	TargetUpdatedStack  dnfrepo.ItemStack
	BeadUpdatedStack    dnfrepo.ItemStack
	MainRefresh         map[string]dnfrepo.ItemStack
	Changed             bool
}

// MonsterCardUpgradeResult describes one processed op603 attempt. Success is
// the generic command result, while UpgradeSucceeded is the probability roll.
// A failed roll still consumes one material card and the fixed gold cost.
type MonsterCardUpgradeResult struct {
	CharacterID      string
	Success          bool
	ErrorCode        byte
	TargetSlot       int16
	MaterialSlot     int16
	ResultSlot       int16
	TargetItemID     int64
	UpgradeSucceeded bool
	UpgradeCount     byte
	Chance           int
	GoldCost         int64
	UpdatedGold      int64
	ChangedSlots     []int16
	Changed          bool
}

// DeletedItem 是已扣减物品的审计摘要。
// UseStackableResult 描述一次消耗品使用写库结果；premium 通知仍由上层 S2C writer 闭合。
// UseStackableResult describes a typed stackable-use result once that owner is implemented.

// UseStackableResult describes a typed stackable-use result once that owner is implemented.
type UseStackableResult struct {
	CharacterID                       string
	AccountID                         string
	ListType                          byte
	SlotIndex                         int16
	InstanceValue                     int32
	ItemID                            int64
	RemainingCount                    int64
	Changed                           bool
	PVFPath                           string
	StackableType                     string
	PremiumActivated                  bool
	PremiumType                       int64
	PremiumRemainingSeconds           int64
	PremiumActivations                []PremiumActivation
	ExperienceBonusActivated          bool
	ExperienceBonusPercent            int64
	ExperienceBonusRemainingSeconds   int64
	DirectExperienceApplied           bool
	DirectExperienceGain              uint32
	DirectExperiencePreviousLevel     int
	DirectExperienceNewLevel          int
	DirectExperiencePreviousTotal     uint32
	DirectExperienceNewTotal          uint32
	DirectExperienceSPGain            int
	DirectExperienceTPGain            int
	TPBookApplied                     bool
	TPBookDelta                       int
	TPBookPreviousRemaining           int
	TPBookNewRemaining                int
	SkillPointBookKind                alignedcmd.SkillPointBookKind
	SkillPointBookSPDelta             int
	SkillPointBookTPDelta             int
	SkillPointBookTPReset             bool
	SkillPointBookPreviousSP          int
	SkillPointBookNewSP               int
	SkillPointBookPreviousTP          int
	SkillPointBookNewTP               int
	LevelUpTicketApplied              bool
	LevelUpTicketRange                int64
	ReviveCoinWalletUpdated           bool
	ReviveCoinWalletTotal             int64
	RandomRewardItemID                int64
	RandomRewardSlots                 []int16
	PetFeedApplied                    bool
	PetFeedPetKey                     string
	PetFeedSatietyBefore              int64
	PetFeedSatietyAfter               int64
	PetFeedSatietyDelta               int64
	PetFeedEffectDurationSeconds      int64
	PetFeedSkillChargePercent         int64
	QuestCompletionTicketApplied      bool
	QuestCompletionAction             string
	QuestCompletionCount              int
	QuestCompletionProfessionChange   bool
	StoryBookApplied                  bool
	StoryBookQuestID                  int64
	StoryBookInitTrigger              uint32
	TransformationApplied             bool
	TransformationAction              string
	ProjectileApplied                 bool
	ProjectileEffectID                int64
	AvatarFittingRoomOpened           bool
	CustomAbilityGroupConverterOpened bool
	FatiguePotionApplied              bool
	FatigueBefore                     int64
	FatigueAfter                      int64
	FatigueRecovery                   int64
	// DailyUseItemID / DailyUseUsed / DailyUseLimit carry the item's own
	// 每天可用次数 state after this use (PVF [total usable count]). The client
	// paints that line from its own op541 state, so the bridge needs these
	// numbers to project them; they are item-agnostic on purpose.
	DailyUseItemID int64
	DailyUseUsed   int64
	DailyUseLimit  int64
	// CooltimeItemID / CooltimeExpireAt carry the durable cooldown window this use
	// started (PVF [cool time] + [cooltime maintenance]); zero means the item has
	// no maintained cooldown. The bridge uses them only for logging — the client
	// starts its own local cooldown on use, and the restore path is the
	// character-select list (notification 172).
	CooltimeItemID                int64
	CooltimeExpireAt              int64
	DungeonAdmissionTicketApplied bool
	DungeonAdmissionAction        string
	DungeonAdmissionPolicyCount   int
	DungeonAdmissionBonus         uint32
	DungeonAdmissionRemainingUses uint32
	RentalPointApplied            bool
	RentalPointBefore             uint32
	RentalPointGranted            uint32
	RentalPointAfter              uint32
	RentalPointLimit              uint32
	RentalPointAtLimit            bool
	// 扩容券（[action type] [upgrade inventory] / [upgrade cargo] /
	// [upgrade account cargo]）落地的结果：档位推进了多少、要重建哪个列表。
	// ContainerUpgradeListType 是 0（背包）/2（金库）/12（帐号金库）；
	// ContainerUpgradeNeedsCargoPad 为真时 bridge 必须先发 op391 重建仓库底板，
	// 否则新解锁的格子仍画着锁（现场 2026-09-18 的既有结论）。
	ContainerUpgradeApplied       bool
	ContainerUpgradeKind          alignedcmd.ContainerUpgradeKind
	ContainerUpgradeBefore        int64
	ContainerUpgradeAfter         int64
	ContainerUpgradeListType      byte
	ContainerUpgradeNeedsCargoPad bool
	// 城镇自施放增益药剂（`[stackable type] [expert town potion]` + 无 [action type]
	// + [stat change duration]，例：史诗Buff药剂 490002458）落地的结果。
	// StatChangePotionSeconds 是增益窗口长度；EpicDropRatePercent > 0 表示这次真的
	// 落了深渊派对史诗掉落率增益（200 = 2 倍），EpicDropRateExpireAt 是到期 Unix 秒。
	StatChangePotionApplied bool
	StatChangePotionSeconds int64
	EpicDropRatePercent     int64
	// EpicDropRateBuffID 是掷点得到的 buff id（etc/variousbufflist.etc 的 [index]；
	// 1136=史诗Buff药剂、1090/1091/1092=赛丽亚三档、1239/1240=小灯笼大/小瓶，
	// 0 = 走 [explain] 回退路径没有 buff id）。客户端画图标就是按它取 icon index。
	EpicDropRateBuffID int64
	// PreviousEpicDropRateBuffID 是这次覆盖掉的上一个 buff id（同族只占一条 buff 槽，
	// 换档时要先撤掉它）；0 表示之前没有生效中的 buff。
	PreviousEpicDropRateBuffID int64
	EpicDropRateExpireAt       int64
	// UpgradeRateKind 是这次落的"强化/增幅成功率"窗口作用对象：
	// "reinforce" / "amplify" / ""（这件药剂不涉及）。服主 2026-10-03：
	// "看看增幅的秘密 强化秘药这类增加强化增幅成功率的道具 也一并修复了"。
	UpgradeRateKind string
	// UpgradeRatePercent 是成功率加成（百分点）：赛丽亚三档 10/20/30，
	// PVF 没有数字的那几件取 statbuff 侧的固定常量（10）。
	UpgradeRatePercent int64
	// UpgradeRateBuffID 是掷点得到的 buff id（1097/1098/1099…；0 = PVF 无候选表）。
	UpgradeRateBuffID int64
	// PreviousUpgradeRateBuffID 是被这次覆盖掉的旧 buff id（换档时先撤掉它）。
	PreviousUpgradeRateBuffID int64
	UpgradeRateExpireAt       int64
	// DungeonClearExpPercent / BuffID / ExpireAt 是"地下城通关经验值 +N%"窗口
	// （赛丽亚的幸运经验药剂 10008222 的三档 50/100/200）。经验由服务端发，所以这一族
	// 必须服务端算；buff id 仍然要注册给客户端（图标）。
	DungeonClearExpPercent  int64
	DungeonClearExpBuffID   int64
	DungeonClearExpExpireAt int64
	// ClientBuffFamily / ClientBuffID / Percent / ExpireAt 是"客户端自己施加"的那一族
	// （城镇移速 village_move_speed、进图属性 move_speed…）。服务端只掷点 + 注册 buff +
	// 记到期，效果本身由客户端按同一份 buff 表施加。窗口按 family 分开存。
	ClientBuffFamily   string
	ClientBuffID       int64
	ClientBuffPercent  int64
	ClientBuffExpireAt int64
}

// PremiumActivation is one durable premium row that must be projected after
// a contract item commits. Auction coupons activate both their auction row and
// the bundled private-store license row.
type PremiumActivation struct {
	PremiumType      int64
	RemainingSeconds int64
}

// DeletedItem records one applied inventory removal.
type DeletedItem struct {
	ListType       byte
	SlotIndex      int16
	ItemID         int64
	RequestedCount int64
	AppliedCount   int64
	RemainingCount int64
}

// NewOwner 创建背包 owner；缺少背包仓储时拒绝处理，避免 handler 直接写状态。
func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.Inventory == nil {
		return nil, ErrOwnerUnavailable
	}
	admissionOwner, _ := dnfdungeonadmission.NewOwner(repos)
	return &Owner{
		repo:              repos.Inventory,
		equipment:         repos.Equipment,
		accountRepo:       repos.AccountInventory,
		accounts:          repos.Account,
		characters:        repos.Character,
		items:             repos.CharacterItems,
		accountItems:      repos.AccountItems,
		assets:            repos.CharacterAssets,
		accountAssets:     repos.AccountAssets,
		rentalAssets:      repos.RentalAssets,
		pets:              repos.Pet,
		petItems:          repos.CharacterPets,
		settlements:       repos.CharacterSettlement,
		settings:          repos.Settings,
		dungeonAdmission:  admissionOwner,
		chronicleRandom:   rand.Intn,
		cardUpgradeRandom: rand.Intn,
		upgradeRandom:     rand.Intn,
		statChangeRandom:  rand.Intn,
		enchantRandom:     rand.Intn,
		now:               time.Now,
	}, nil
}

func (o *Owner) withinInventoryTransaction(ctx context.Context, characterID string, apply func(*Owner) error) error {
	if o.items == nil {
		return ErrInventoryTransactionRequired
	}
	return dnfrepo.WithinCharacterItemsScoped(ctx, o.items, characterID, func(inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		txOwner := *o
		txOwner.repo = inventory
		txOwner.equipment = equipment
		txOwner.inItemTransaction = true
		return apply(&txOwner)
	})
}

func (o *Owner) withinCharacterAssetTransaction(ctx context.Context, characterID string, apply func(*Owner) error) error {
	if o.assets == nil {
		return ErrCharacterAssetTxnRequired
	}
	return o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		txOwner := *o
		txOwner.characters = characters
		txOwner.repo = inventory
		txOwner.equipment = equipment
		txOwner.inItemTransaction = true
		return apply(&txOwner)
	})
}

func (o *Owner) withinAccountCharacterAssetTransaction(ctx context.Context, accountID string, characterID string, apply func(*Owner) error) error {
	if o.accountAssets == nil {
		return dnfrepo.ErrAccountCharacterAssetTransactionUnavailable
	}
	return o.accountAssets.WithinAccountCharacterAssets(ctx, accountID, characterID, func(accountInventory dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		txOwner := *o
		txOwner.accountRepo = accountInventory
		txOwner.characters = characters
		txOwner.repo = inventory
		txOwner.equipment = equipment
		txOwner.inItemTransaction = true
		return apply(&txOwner)
	})
}

// Delete 按 EXE/C# 已确认的删除语义扣减物品。
// 这里只保存背包字段；成功 ACK 与背包刷新顺序由上层在证据闭合后处理。
func (o *Owner) Delete(ctx context.Context, cmd Command) (DeleteResult, error) {
	if o == nil || o.repo == nil {
		return DeleteResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return DeleteResult{}, ErrCharacterRequired
	}
	if err := checkDeleteListType(cmd.SourceListType); err != nil {
		return DeleteResult{}, err
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	mutations := deleteMutations(cmd)
	if len(mutations) == 0 {
		return DeleteResult{}, ErrSlotNotFound
	}
	usesAccountShared := false
	for _, mutation := range mutations {
		if dnfrepo.IsAccountSharedInventorySlot(mutation.listType, mutation.slotIndex) {
			usesAccountShared = true
			break
		}
	}
	if usesAccountShared {
		accountID := strings.TrimSpace(cmd.AccountID)
		if accountID == "" {
			return DeleteResult{}, ErrAccountRequired
		}
		if o.accountRepo == nil || o.accountItems == nil {
			return DeleteResult{}, ErrAccountSharedOwnerUnavailable
		}
		if !o.inAccountItemTx {
			var result DeleteResult
			err := o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
				txOwner := *o
				txOwner.accountRepo = accounts
				txOwner.repo = characters
				txOwner.inAccountItemTx = true
				var err error
				result, err = txOwner.Delete(ctx, cmd)
				return err
			})
			return result, err
		}
	} else if !o.inItemTransaction {
		var result DeleteResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Delete(ctx, cmd)
			return err
		})
		return result, err
	}
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return DeleteResult{}, err
	}
	if !ok {
		return DeleteResult{}, ErrInventoryNotFound
	}

	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if record.Warehouse == nil {
		record.Warehouse = make(map[string]dnfrepo.ItemStack)
	}

	var account dnfrepo.AccountInventoryRecord
	if usesAccountShared {
		accountID := strings.TrimSpace(cmd.AccountID)
		account, _, err = o.accountRepo.Load(ctx, accountID)
		if err != nil {
			return DeleteResult{}, err
		}
		account = dnfrepo.CloneAccountInventory(account)
		account.AccountID = accountID
		if account.Slots == nil {
			account.Slots = make(map[string]dnfrepo.ItemStack)
		}
	}

	changedSlots := false
	changedWarehouse := false
	changedAccount := false
	var deletedEggs []deletedEggRow
	result := DeleteResult{CharacterID: characterID, Removed: make([]DeletedItem, 0, len(mutations))}
	for _, mutation := range mutations {
		accountOwned := dnfrepo.IsAccountSharedInventorySlot(mutation.listType, mutation.slotIndex)
		var items map[string]dnfrepo.ItemStack
		var field dnfrepo.InventoryField
		if accountOwned {
			items = account.Slots
		} else {
			items, field = itemMapForList(&record, mutation.listType)
		}
		key := slotKey(mutation.listType, mutation.slotIndex)
		stack, ok := items[key]
		if !ok {
			return DeleteResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, mutation.listType, mutation.slotIndex)
		}
		if isEquipmentLocked(stack) {
			return DeleteResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrItemLocked, mutation.listType, mutation.slotIndex)
		}
		if mutation.listType == listTypePet && stackHasLivePetBinding(stack) {
			// 宠物栏（list 7）：宠物蛋可以丢，已孵化的宠物不行 —— 后者的图鉴/装备
			// 状态在 pet owner 手里。
			//
			// 但只看 serial 是不够的：历史上 creaturestate 把 `equipment/creature/`
			// 下的**蛋**当成"直接发放的宠物"，给它写了 serial 并建出宠物记录，于是
			// 蛋在这里被当成"已绑定宠物"拒绝 —— 现场就是「宠物蛋无法丢弃」。
			// 蛋按运行时 PVF 的 `[output index]` 判定；判定不了（探针未装配）时保持
			// 原样拒绝，绝不放开真正的宠物。
			if !creaturestate.IsPetEgg(stack.ItemID) {
				return DeleteResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrPetInventoryOwnerRequired, mutation.listType, mutation.slotIndex)
			}
			deletedEggs = append(deletedEggs, deletedEggRow{slotIndex: mutation.slotIndex, itemID: stack.ItemID})
		}
		if mutation.itemID != 0 && stack.ItemID != int64(mutation.itemID) {
			return DeleteResult{}, fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, mutation.listType, mutation.slotIndex, mutation.itemID, stack.ItemID)
		}

		applied := normalizeDeleteCount(stack.Count, mutation.count)
		remaining := stack.Count - applied
		if remaining <= 0 {
			delete(items, key)
			remaining = 0
		} else {
			stack.Count = remaining
			items[key] = stack
		}
		if accountOwned {
			changedAccount = true
			// 账号共享晶体/灵魂材料槽（Main 虚拟底部槽）在副本内删除后需要
			// 补一条空的 S0/0012 库存动作完成通知，否则客户端忙状态会带到
			// 下一地图，普通消耗品在客户端门禁处被拦下（us90 同口径）。
			result.DeletedAccountMaterial = true
		} else if field == dnfrepo.InventoryFieldWarehouse {
			changedWarehouse = true
		} else {
			changedSlots = true
		}
		result.Removed = append(result.Removed, DeletedItem{
			ListType:       mutation.listType,
			SlotIndex:      mutation.slotIndex,
			ItemID:         stack.ItemID,
			RequestedCount: mutation.count,
			AppliedCount:   applied,
			RemainingCount: remaining,
		})
	}

	fields := make([]dnfrepo.InventoryField, 0, 2)
	if changedSlots {
		fields = append(fields, dnfrepo.InventoryFieldSlots)
	}
	if changedWarehouse {
		fields = append(fields, dnfrepo.InventoryFieldWarehouse)
	}
	if len(fields) == 0 && !changedAccount {
		return result, nil
	}
	now := time.Now()
	if len(fields) > 0 {
		record.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, fields...); err != nil {
			return DeleteResult{}, err
		}
	}
	if changedAccount {
		account.UpdatedAt = now
		if err := o.accountRepo.Save(ctx, account); err != nil {
			return DeleteResult{}, err
		}
	}
	result.Changed = true
	if len(deletedEggs) > 0 {
		o.pruneDeletedEggPetEntries(ctx, characterID, deletedEggs)
	}
	return result, nil
}

// deletedEggRow 是一次成功删除的蛋行，用于清理历史上误建在它上面的宠物记录。
type deletedEggRow struct {
	slotIndex int16
	itemID    int64
}

// pruneDeletedEggPetEntries 移除挂在已删除蛋行上的宠物记录。
//
// 整段都是 best-effort：道具本身已经删掉了，这里失败只留下一条不被任何背包行
// 引用的孤儿宠物记录，绝不能反过来让丢弃失败。
func (o *Owner) pruneDeletedEggPetEntries(ctx context.Context, characterID string, eggs []deletedEggRow) {
	if o == nil || o.pets == nil || len(eggs) == 0 {
		return
	}
	petRecord, found, err := o.pets.Load(ctx, characterID)
	if err != nil || !found || len(petRecord.Entries) == 0 {
		return
	}
	petRecord = dnfrepo.ClonePet(petRecord)
	changed := false
	for key, entry := range petRecord.Entries {
		if entry.SourceListType != listTypePet || petRecord.EquippedKey == key {
			continue
		}
		for _, egg := range eggs {
			if entry.ItemID != egg.itemID || entry.SourceSlotIndex != egg.slotIndex {
				continue
			}
			delete(petRecord.Entries, key)
			changed = true
			break
		}
	}
	if !changed {
		return
	}
	petRecord.UpdatedAt = time.Now()
	_ = dnfrepo.SavePetFields(ctx, o.pets, petRecord, dnfrepo.PetFieldEntries)
}

// Sell 校验出售价格证据并扣减物品。
// 正价格出售需要钱包和背包同事务提交；当前仓储没有跨 repo 事务实现时拒绝，避免半提交。
func (o *Owner) Sell(ctx context.Context, cmd Command) (SellResult, error) {
	if o == nil || o.repo == nil {
		return SellResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return SellResult{}, ErrCharacterRequired
	}
	if err := checkInventoryRemovalListType(cmd.SourceListType); err != nil {
		return SellResult{}, err
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result SellResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Sell(ctx, cmd)
			return err
		})
		return result, err
	}
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return SellResult{}, err
	}
	if !ok {
		return SellResult{}, ErrInventoryNotFound
	}

	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if record.Warehouse == nil {
		record.Warehouse = make(map[string]dnfrepo.ItemStack)
	}

	items, field := itemMapForList(&record, cmd.SourceListType)
	key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	stack, ok := items[key]
	if !ok {
		return SellResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if isEquipmentLocked(stack) {
		return SellResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrItemLocked, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	sellGold, err := sellGoldOf(stack)
	if err != nil {
		return SellResult{}, err
	}
	if o.characters == nil {
		return SellResult{}, fmt.Errorf("%w: character repository missing", ErrWalletTxnRequired)
	}
	if o.characters == nil {
		return SellResult{}, fmt.Errorf("%w: character repository missing", ErrWalletTxnRequired)
	}
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return SellResult{}, err
	}
	if !ok {
		return SellResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}

	applied := normalizeDeleteCount(stack.Count, int64(cmd.Count))
	goldDelta := sellGold * applied
	if goldDelta != 0 {
		return SellResult{}, fmt.Errorf("%w: goldDelta=%d", ErrWalletTxnRequired, goldDelta)
	}

	remaining := stack.Count - applied
	if remaining <= 0 {
		delete(items, key)
		remaining = 0
	} else {
		stack.Count = remaining
		items[key] = stack
	}

	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, field); err != nil {
		return SellResult{}, err
	}
	return SellResult{
		CharacterID: characterID,
		Sold: DeletedItem{
			ListType:       cmd.SourceListType,
			SlotIndex:      cmd.SourceSlotIndex,
			ItemID:         stack.ItemID,
			RequestedCount: int64(cmd.Count),
			AppliedCount:   applied,
			RemainingCount: remaining,
		},
		GoldDelta:   goldDelta,
		UpdatedGold: character.Stats["gold"],
		Changed:     true,
	}, nil
}

// Move 按当前已验证的安全子集移动背包格子，并只写 inventory repository。
// 装备穿脱、账号仓库和成功 ACK/刷新顺序仍需要 EXE/MCP 证据闭合后交给对应 owner。
func (o *Owner) Move(ctx context.Context, cmd Command) (MoveResult, error) {
	if o == nil || o.repo == nil {
		return MoveResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return MoveResult{}, ErrCharacterRequired
	}
	if cmd.SourceListType == listTypeAccountCargo || cmd.DestinationListType == listTypeAccountCargo {
		characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
		return o.moveAccountCargo(ctx, characterID, cmd)
	}
	if err := checkMoveListType(cmd.SourceListType); err != nil {
		return MoveResult{}, err
	}
	if err := checkMoveListType(cmd.DestinationListType); err != nil {
		return MoveResult{}, err
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	petSegment, petMove := petInventoryMoveSegment(cmd)
	if cmd.SourceListType == listTypePet && cmd.DestinationListType == listTypePet {
		if !petMove {
			return MoveResult{}, fmt.Errorf("%w: pet inventory move crosses container segments src=%d dst=%d", ErrUnsupportedList, cmd.SourceSlotIndex, cmd.DestinationSlotIndex)
		}
		if !o.inPetTransaction {
			return o.movePetInventory(ctx, characterID, cmd, petSegment)
		}
	}
	// 晶体/灵魂类道具（各有唯一固定仓库格的十二个家族）不允许落在角色背包里：客户端把
	// "从个人仓库/账号金库取回"的落点声明成材料背包格，这里按固定格改回落点，见
	// redirectAccountSharedWarehouseDestination。
	cmd, warehouseRedirect, clearedMainSlot, err := o.redirectAccountSharedWarehouseDestination(ctx, cmd, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	if dnfrepo.IsAccountSharedInventorySlot(cmd.SourceListType, cmd.SourceSlotIndex) ||
		dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		result, err := o.moveAccountShared(ctx, characterID, cmd)
		if err != nil {
			return MoveResult{}, err
		}
		if warehouseRedirect {
			result.RedirectedReservedMainSlot = true
			// 客户端声明的那一格必须被显式清掉：它自己的乐观移动已经把那件物品画在那里，
			// 而 ACK 只回执真实落点（固定格）。少了这一行，客户端从此留下一格点不动的
			// 幽灵行（2026-09-21 现场）。
			result.ClearedRedirectedMainSlot = clearedMainSlot
		}
		return result, nil
	}
	if !o.inItemTransaction {
		var result MoveResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Move(ctx, cmd)
			return err
		})
		return result, err
	}
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	if !ok {
		return MoveResult{}, ErrInventoryNotFound
	}

	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if record.Warehouse == nil {
		record.Warehouse = make(map[string]dnfrepo.ItemStack)
	}

	// 徽章（PVF [stackable type] = [avatar emblem]）禁存仓库：运营口径 2026-10-03。
	//
	// 必须在**任何**仓库重定向之前判定：redirectAccountSharedWarehouseDestination 会把
	// 落点改到 354..365，那是账号仓 owner，先改就拦不住了。取出方向不在本闸门范围内
	// （只有"落点是仓库"才拦），历史留在仓库里的徽章仍可拖回背包。
	if cmd.SourceListType != cmd.DestinationListType &&
		isWarehouseDestination(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		sourceItems, _ := itemMapForList(&record, cmd.SourceListType)
		if source, occupied := sourceItems[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]; occupied &&
			o.isAvatarEmblem(source) {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d item=%d",
				ErrEmblemWarehouseForbidden, cmd.DestinationListType, cmd.DestinationSlotIndex, source.ItemID)
		}
	}

	var redirectedReservedMainSlot bool
	cmd, redirectedReservedMainSlot, err = o.redirectPersonalCargoMoveFromGoldWalletSlot(cmd, record)
	if err != nil {
		return MoveResult{}, err
	}
	var pageRedirect bool
	cmd, pageRedirect, err = o.redirectMainMoveToItemPage(ctx, cmd, record, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	redirectedReservedMainSlot = redirectedReservedMainSlot || pageRedirect

	// 落点在未解锁的个人仓库格：运营口径 2026-09-18 —— 顺延到已解锁区（先叠加同类，
	// 再从第一格起找空格），真的满了才回"物品栏空间不足"。客户端允许往未解锁格拖，
	// 但物品写进去等于消失，绝不能照它声明的槽位落库（现场：拖进未解锁格居然直接放进去了）。
	if isPersonalCargoListType(cmd.DestinationListType) {
		previewDestination, _ := itemMapForList(&record, cmd.DestinationListType)
		previewSource, _ := itemMapForList(&record, cmd.SourceListType)
		if movingStack, found := previewSource[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]; found {
			moveCount := o.normalizeMoveCount(movingStack, int64(cmd.MoveCount))
			excludeSourceSlot := int16(-1)
			if cmd.SourceListType == cmd.DestinationListType {
				excludeSourceSlot = cmd.SourceSlotIndex
			}
			resolution := o.resolveLockedPersonalCargoDestination(
				ctx,
				cmd.DestinationListType,
				cmd.DestinationSlotIndex,
				previewDestination,
				o.canonicalStackForMove(movingStack),
				moveCount,
				excludeSourceSlot,
				characterID,
			)
			switch {
			case resolution.Locked && cmd.SourceListType == cmd.DestinationListType:
				// 仓库内操作（同容器）：落到未解锁格**不适用**"顺延到第一个空格"那套，
				// 按运营口径直接回原位（拒绝）。顺延只服务于跨容器搬入。
				return MoveResult{}, fmt.Errorf(
					"%w: 仓库内拖动落在未解锁格 %d，直接回原位",
					ErrMoveDestinationFull,
					cmd.DestinationSlotIndex,
				)
			case resolution.Redirected:
				cmd.DestinationSlotIndex = resolution.Slot
				redirectedReservedMainSlot = true
			case resolution.Locked:
				return MoveResult{}, fmt.Errorf(
					"%w: 个人仓库已解锁格全满，落点 %d 未解锁",
					ErrMoveDestinationFull,
					cmd.DestinationSlotIndex,
				)
			}
		}
	}

	srcItems, srcField := itemMapForList(&record, cmd.SourceListType)
	dstItems, dstField := itemMapForList(&record, cmd.DestinationListType)
	srcKey := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	dstKey := slotKey(cmd.DestinationListType, cmd.DestinationSlotIndex)
	result := MoveResult{
		CharacterID:                characterID,
		SourceListType:             cmd.SourceListType,
		SourceSlotIndex:            cmd.SourceSlotIndex,
		DestinationListType:        cmd.DestinationListType,
		DestinationSlotIndex:       cmd.DestinationSlotIndex,
		MoveCount:                  int64(cmd.MoveCount),
		Mode:                       "noop",
		RedirectedReservedMainSlot: redirectedReservedMainSlot,
	}
	if srcField == dstField && srcKey == dstKey {
		return result, nil
	}

	source, sourceOK := srcItems[srcKey]
	destination, destinationOK := dstItems[dstKey]
	if sourceOK {
		source = o.canonicalStackForMove(source)
	}
	if destinationOK {
		destination = o.canonicalStackForMove(destination)
	}
	if !sourceOK && !destinationOK {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if sourceOK && source.Count <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}
	if !sourceOK {
		if destination.Count <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		moveCount := o.normalizeMoveCount(destination, int64(cmd.MoveCount))
		if moveCount <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		// 反向：物品其实从声明的目标格搬进源容器，携带上限按源容器算。
		if err := o.validateCarryLimitOnMove(
			cmd.DestinationListType, cmd.SourceListType, cmd.SourceSlotIndex, srcItems, destination, moveCount,
		); err != nil {
			return MoveResult{}, err
		}
		excludedSlots := []int16{cmd.SourceSlotIndex}
		if cmd.SourceListType == cmd.DestinationListType {
			excludedSlots = append(excludedSlots, cmd.DestinationSlotIndex)
		}
		// 反向落点（声明的 source 端）在未解锁格：必须顺延，绝不能把物品放回看不见的格子。
		// 现场 2026-09-18：仓库/金库内部把已有物品拖到未解锁格，客户端把落点写在 source 端。
		skipStackSearch := false
		if isPersonalCargoListType(cmd.SourceListType) {
			resolution := o.resolveLockedPersonalCargoDestination(
				ctx,
				cmd.SourceListType,
				cmd.SourceSlotIndex,
				srcItems,
				destination,
				moveCount,
				cmd.DestinationSlotIndex,
				characterID,
			)
			if resolution.Locked {
				if cmd.SourceListType == cmd.DestinationListType {
					// 仓库内操作（同容器）：落到未解锁格直接回原位，不顺延。
					return MoveResult{}, fmt.Errorf(
						"%w: 仓库内拖动落在未解锁格 %d，直接回原位",
						ErrMoveDestinationFull,
						cmd.SourceSlotIndex,
					)
				}
				if !resolution.Redirected {
					return MoveResult{}, fmt.Errorf(
						"%w: 个人仓库已解锁格全满，落点 %d 未解锁",
						ErrMoveDestinationFull,
						cmd.SourceSlotIndex,
					)
				}
				if resolution.StackFound {
					target := mergeIntoStack(resolution.Stack, cmd.SourceListType, resolution.Slot, destination, moveCount)
					if destination.Count <= moveCount {
						delete(dstItems, dstKey)
					} else {
						destination.Count -= moveCount
						updateStackRawAmount(&destination)
						dstItems[dstKey] = destination
					}
					srcItems[slotKey(cmd.SourceListType, resolution.Slot)] = target
					result.MoveCount = moveCount
					result.Mode = "auto_stack_reverse"
					result.SourceSlotIndex = resolution.Slot
					result.RedirectedReservedMainSlot = true
					result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType)
					if err != nil {
						return MoveResult{}, err
					}
					return o.saveMoveResult(ctx, record, srcField, dstField, result)
				}
				cmd.SourceSlotIndex = resolution.Slot
				srcKey = slotKey(cmd.SourceListType, resolution.Slot)
				result.SourceSlotIndex = resolution.Slot
				result.RedirectedReservedMainSlot = true
				skipStackSearch = true
			}
		}
		if !skipStackSearch && !isMainItemQuickSlot(cmd.SourceListType, cmd.SourceSlotIndex) {
			if targetSlot, target, ok := o.findCompatibleStackSlot(
				srcItems,
				cmd.SourceListType,
				destination,
				moveCount,
				excludedSlots...,
			); ok {
				target = mergeIntoStack(target, cmd.SourceListType, targetSlot, destination, moveCount)
				if destination.Count <= moveCount {
					delete(dstItems, dstKey)
				} else {
					destination.Count -= moveCount
					updateStackRawAmount(&destination)
					dstItems[dstKey] = destination
				}
				srcItems[slotKey(cmd.SourceListType, targetSlot)] = target
				result.MoveCount = moveCount
				result.Mode = "auto_stack_reverse"
				result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType, cmd.DestinationListType)
				if err != nil {
					return MoveResult{}, err
				}
				return o.saveMoveResult(ctx, record, srcField, dstField, result)
			}
		}
		if o.canSplitStack(destination, moveCount) {
			moved := cloneStack(destination)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			destination.Count -= moveCount
			updateStackRawAmount(&destination)
			dstItems[dstKey] = destination
			srcItems[srcKey] = preparedMoveStack(moved, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
			result.MoveCount = moveCount
			result.Mode = "reverse_split"
			if cmd.SourceListType == cmd.DestinationListType {
				result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType)
				if err != nil {
					return MoveResult{}, err
				}
			}
			return o.saveMoveResult(ctx, record, srcField, dstField, result)
		}
		delete(dstItems, dstKey)
		srcItems[srcKey] = preparedMoveStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
		result.MoveCount = destination.Count
		result.Mode = "reverse_move"
		return o.saveMoveResult(ctx, record, srcField, dstField, result)
	}

	moveCount := o.normalizeMoveCount(source, int64(cmd.MoveCount))
	if moveCount <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}
	// 携带上限（PVF [stack limit]）按容器总量算：跨容器搬入前先卡，超了直接拒绝，
	// 不允许靠"另寻空格"绕过去。
	if err := o.validateCarryLimitOnMove(
		cmd.SourceListType, cmd.DestinationListType, cmd.DestinationSlotIndex, dstItems, source, moveCount,
	); err != nil {
		return MoveResult{}, err
	}
	if !destinationOK {
		excludedSlots := []int16{cmd.DestinationSlotIndex}
		if cmd.SourceListType == cmd.DestinationListType {
			excludedSlots = append(excludedSlots, cmd.SourceSlotIndex)
		}
		if !isMainItemQuickSlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
			if targetSlot, target, ok := o.findCompatibleStackSlot(
				dstItems,
				cmd.DestinationListType,
				source,
				moveCount,
				excludedSlots...,
			); ok {
				target = mergeIntoStack(target, cmd.DestinationListType, targetSlot, source, moveCount)
				if source.Count <= moveCount {
					delete(srcItems, srcKey)
				} else {
					source.Count -= moveCount
					updateStackRawAmount(&source)
					srcItems[srcKey] = source
				}
				dstItems[slotKey(cmd.DestinationListType, targetSlot)] = target
				result.MoveCount = moveCount
				result.Mode = "auto_stack"
				if cmd.SourceListType != cmd.DestinationListType {
					// 跨容器：物品没有落进客户端拖过去的那一格，而是并进了目标容器里
					// 另一处同类堆。真实落点必须写进 ACK，否则客户端会把这一堆画在原
					// 本的空格上；同时绝不能走整 list 刷新 —— list-0 的保留槽 0（金币
					// 钱包行）会被一起下发，现场就是"获得了 N 金币"+ 物品行错位
					// （2026-09-13 05:40:28 抓包 refreshLists=[0 2] 复现）。
					// 用 "stack" 让 bridge 按真实端点补精确槽位刷新，与显式合并同一形状。
					result.Mode = "stack"
					result.DestinationSlotIndex = targetSlot
					result.RedirectedReservedMainSlot = true
					return o.saveMoveResult(ctx, record, srcField, dstField, result)
				}
				result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType, cmd.DestinationListType)
				if err != nil {
					return MoveResult{}, err
				}
				return o.saveMoveResult(ctx, record, srcField, dstField, result)
			}
		}
		if o.canSplitStack(source, moveCount) {
			moved := cloneStack(source)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
			dstItems[dstKey] = preparedMoveStack(moved, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
			result.MoveCount = moveCount
			result.Mode = "split"
			if cmd.SourceListType == cmd.DestinationListType {
				result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType)
				if err != nil {
					return MoveResult{}, err
				}
			}
			return o.saveMoveResult(ctx, record, srcField, dstField, result)
		}
		delete(srcItems, srcKey)
		dstItems[dstKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
		result.MoveCount = source.Count
		result.Mode = "move"
		return o.saveMoveResult(ctx, record, srcField, dstField, result)
	}

	// op19 跨容器落点修正：目标格被占且跨容器时，绝不能互换 —— op19 的 ACK 只带一个
	// 目标槽，被换出去的那件客户端不知道去了哪，会"消失"到小退（账号金库只有一格时
	// 就是这个现象）。所以只剩三条出路，按 86JP
	// InventoryMoveService.ApplyCrossSpaceInsertToOccupiedDestination 的顺序：
	//   ① 与声明格同类可叠 → 就地合并（由下面的常规 canStack 分支执行）；
	//   ② 目标容器里另一处同类可叠格 → 并进那一行，不新开第二行；
	//   ③ 目标容器里第一个已解锁空格 → 落过去，真实落点写进 ACK。
	// 三条都不成立（容器真的满/读不到容器状态）才明确回"空间不足"。
	//
	// ②③ 以前只在"客户端声明目标格为空、服务端却有行"时才跑；玩家正常丢在占用格上的
	// 手势（客户端带着它看见的那一行）会被直接拒掉，即使目标容器还空着好几格 ——
	// 2026-09-27 现场两个方向都是这个现象，已修（见 crossContainerOccupiedDestinationRedirect）。
	// 主快捷栏（3..8 的互换是常规操作）与账号共享晶体/灵魂槽保持原有语义，不在本规则内。
	if cmd.SourceListType != cmd.DestinationListType &&
		!isMainItemQuickSlot(cmd.DestinationListType, cmd.DestinationSlotIndex) &&
		!dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		if redirect, ok := o.crossContainerOccupiedDestinationRedirect(ctx, cmd, dstItems, destination, source, moveCount, characterID); ok {
			targetKey := slotKey(cmd.DestinationListType, redirect.Slot)
			result.MoveCount = moveCount
			if redirect.Mode == "stack" {
				target := mergeIntoStack(redirect.Stack, cmd.DestinationListType, redirect.Slot, source, moveCount)
				if source.Count <= moveCount {
					delete(srcItems, srcKey)
				} else {
					source.Count -= moveCount
					updateStackRawAmount(&source)
					srcItems[srcKey] = source
				}
				dstItems[targetKey] = target
				result.Mode = "stack"
			} else if o.canSplitStack(source, moveCount) {
				moved := cloneStack(source)
				moved.Count = moveCount
				updateStackRawAmount(&moved)
				source.Count -= moveCount
				updateStackRawAmount(&source)
				srcItems[srcKey] = source
				dstItems[targetKey] = preparedMoveStack(moved, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, redirect.Slot)
				result.Mode = "split"
			} else {
				delete(srcItems, srcKey)
				dstItems[targetKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, redirect.Slot)
				result.Mode = "move"
			}
			// ACK 与精确槽位刷新都必须报真实落点，不能回显客户端声明的槽位。
			result.DestinationSlotIndex = redirect.Slot
			result.RedirectedReservedMainSlot = true
			// 这里刻意不调用 withMoveRefresh：整 list 刷新会把 list-0 的保留槽 0（金币
			// 钱包行）一起下发，历史上造成过"获得金币/金币归 0"。落点已经写进 ACK，
			// 客户端据此自己重画两个端点即可。
			return o.saveMoveResult(ctx, record, srcField, dstField, result)
		}
		if !o.canStack(source, destination, moveCount) {
			// 目标容器没有空格也没有同类可叠格，声明的那一格又放不下这一堆：
			// 回失败包让客户端弹"物品栏空间不足"，而不是偷偷换位。
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d item=%d count=%d", ErrMoveDestinationFull, cmd.DestinationListType, cmd.DestinationSlotIndex, source.ItemID, moveCount)
		}
	}

	if o.canStack(source, destination, moveCount) {
		destination = mergeIntoStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, source, moveCount)
		if source.Count <= moveCount {
			delete(srcItems, srcKey)
		} else {
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
		}
		dstItems[dstKey] = destination
		result.MoveCount = moveCount
		result.Mode = "stack"
		if isMainPersonalCargoCrossList(cmd.SourceListType, cmd.DestinationListType) {
			// The bridge can refresh the exact source and destination rows for
			// this explicit cross-container merge. Avoid constructing full
			// list snapshots that invalidate the client's active op19 objects.
			return o.saveMoveResult(ctx, record, srcField, dstField, result)
		}
		result, err = o.withMoveRefresh(ctx, cmd, result, &record, cmd.SourceListType, cmd.DestinationListType)
		if err != nil {
			return MoveResult{}, err
		}
		return o.saveMoveResult(ctx, record, srcField, dstField, result)
	}

	srcItems[srcKey] = preparedMoveStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
	dstItems[dstKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
	result.MoveCount = source.Count
	result.Mode = "swap"
	// 2026-09-13 回滚记录：这里曾加上
	//   withMoveRefresh(ctx, cmd, result, &record, SourceListType, DestinationListType)
	// 目的是让客户端看到跨容器交换后的两行。但按整个 list 刷新会把 list-0 的保留槽 0
	// （金币钱包）也带下去，而那条路径不经过钱包投影，结果客户端弹出"获得 102622810 金币"
	// 并且同物品叠加时金币显示为 0。该写法已回滚；若要再做，只能按"精确槽位"下发
	// （result.Refresh 里只放源/目标格，或走 incrementalSlots 通道），绝不能整 list 刷新。
	return o.saveMoveResult(ctx, record, srcField, dstField, result)
}

// redirectAccountSharedWarehouseDestination 把"落点是角色背包"的晶体/灵魂类道具改回
// 它在晶体/灵魂仓库里的固定格。
//
// 运营口径 2026-09-17：把晶体从个人仓库/账号金库取回时，客户端声明的落点是材料背包格
// （现场 0:156、0:121、0:150），于是无色小晶块落在背包里，而晶体仓库面板那一格还是空的。
// 这类道具在自己账号里有唯一的家：六个晶块固定占 list 0 的 354..359、六个灵魂占
// 360..365（dnfbridge 的 currentDisjointWarehouseFixedSlots，2026-07-15 实测并经运营
// 确认）。其它入包路径（副本拾取、邮件领取、合成产出、NPC 商店材料）本来就按固定格落库，
// 只有玩家手拖这条 op19 路径漏了，所以在这里补齐。
//
// 不接管的情形（保持客户端声明的落点）：
//   - 落点已经是账号共享槽：客户端自己就声明对了；
//   - 这件道具没有固定格（元素结晶等所有其它可叠加道具）；
//   - 固定格被**别的**道具占着（历史脏数据）——绝不驱赶别人的行；
//   - 同 id 合并会超上限——grant 路径的做法是溢出到背包，这里保持一致，不做跨容器互换
//     （op19 的 ACK 只带一个目标槽，换出去的那件客户端不知道去了哪）。
//
// 返回的 bool 表示落点被改过，调用方据此把 RedirectedReservedMainSlot 写进结果，让 ACK
// 回显真正落库的端点；第二个返回值是"客户端声明的那一格"（仅当它在持久化记录里确实是空的
// 时才非 0），调用方把它交给 bridge 补一条删除行 —— 客户端发包前已经乐观地画在那一格上了。
// 这一步只读快照：真正的提交仍在账号事务里重新加载并校验。
func (o *Owner) redirectAccountSharedWarehouseDestination(
	ctx context.Context,
	cmd Command,
	characterID string,
) (Command, bool, int16, error) {
	if o == nil || o.accountWarehouseSlot == nil ||
		cmd.DestinationListType != listTypeMain ||
		dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		return cmd, false, 0, nil
	}
	source, record, found, err := o.accountWarehouseMoveSource(ctx, cmd, characterID)
	if err != nil || !found || source.ItemID <= 0 || source.Count <= 0 {
		return cmd, false, 0, err
	}
	slot, fixed := o.accountWarehouseSlot(source.ItemID)
	if !fixed || slot < dnfrepo.CrystalWarehouseFirstSlot || slot > dnfrepo.SoulWarehouseLastSlot ||
		slot == cmd.DestinationSlotIndex {
		return cmd, false, 0, nil
	}
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" || o.accountRepo == nil {
		return cmd, false, 0, nil
	}
	account, _, err := o.accountRepo.Load(ctx, accountID)
	if err != nil {
		return cmd, false, 0, err
	}
	if occupied, exists := account.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]; exists && occupied.ItemID > 0 {
		if occupied.ItemID != source.ItemID ||
			!o.canStack(source, occupied, o.normalizeMoveCount(source, int64(cmd.MoveCount))) {
			return cmd, false, 0, nil
		}
	}
	// 声明格在服务端确实是空的才回报（bridge 会按"背包空格 = 0xFFFFFFFF 删除行"补发）。
	// 读不到记录时保守地不回报：宁可维持改造前的行为，也不拿不准的状态去删客户端的格子。
	clearedMainSlot := int16(0)
	if record.CharacterID != "" {
		if _, occupied := record.Slots[slotKey(listTypeMain, cmd.DestinationSlotIndex)]; !occupied {
			clearedMainSlot = cmd.DestinationSlotIndex
		}
	}
	cmd.DestinationSlotIndex = slot
	return cmd, true, clearedMainSlot, nil
}

// accountWarehouseMoveSource 读这次移动**源端**那一行，用来判断搬的是哪件道具。源端可能
// 在角色容器（背包/个人仓库），也可能已经在账号共享晶体/灵魂槽里。第二个返回值是读源端时
// 顺带拿到的角色记录（源端在账号仓时为"未加载"的零值），调用方据此判断落点格是否为空，
// 避免为了同一件事再读一次库。
func (o *Owner) accountWarehouseMoveSource(
	ctx context.Context,
	cmd Command,
	characterID string,
) (dnfrepo.ItemStack, dnfrepo.InventoryRecord, bool, error) {
	if dnfrepo.IsAccountSharedInventorySlot(cmd.SourceListType, cmd.SourceSlotIndex) {
		accountID := strings.TrimSpace(cmd.AccountID)
		if o.accountRepo == nil || accountID == "" {
			return dnfrepo.ItemStack{}, dnfrepo.InventoryRecord{}, false, nil
		}
		account, _, err := o.accountRepo.Load(ctx, accountID)
		if err != nil {
			return dnfrepo.ItemStack{}, dnfrepo.InventoryRecord{}, false, err
		}
		stack, ok := account.Slots[dnfrepo.AccountSharedInventorySlotKey(cmd.SourceSlotIndex)]
		return stack, dnfrepo.InventoryRecord{}, ok, nil
	}
	if o.repo == nil {
		return dnfrepo.ItemStack{}, dnfrepo.InventoryRecord{}, false, nil
	}
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil || !found {
		return dnfrepo.ItemStack{}, dnfrepo.InventoryRecord{}, false, err
	}
	items, _ := itemMapForList(&record, cmd.SourceListType)
	stack, ok := items[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	return stack, record, ok, nil
}

// redirectPersonalCargoMoveFromGoldWalletSlot prevents an ordinary item from
// ever being persisted in main-list slot 0.  The current client owns that row
// as its gold-wallet object, although op19 can name it as an empty backpack
// destination.  The moving item's active-PVF page is the only authority for a
// replacement slot; no fixed consumable/material range is assumed here.
func (o *Owner) redirectPersonalCargoMoveFromGoldWalletSlot(cmd Command, record dnfrepo.InventoryRecord) (Command, bool, error) {
	if !isMainPersonalCargoCrossList(cmd.SourceListType, cmd.DestinationListType) {
		return cmd, false, nil
	}
	mainIsSource := cmd.SourceListType == listTypeMain && cmd.SourceSlotIndex == 0
	mainIsDestination := cmd.DestinationListType == listTypeMain && cmd.DestinationSlotIndex == 0
	if !mainIsSource && !mainIsDestination {
		return cmd, false, nil
	}
	if stack, occupied := record.Slots[slotKey(listTypeMain, 0)]; occupied && stack.ItemID != 0 && stack.Count > 0 {
		return cmd, false, fmt.Errorf("%w: gold-wallet slot already contains item=%d", ErrReservedSlotRelocationFull, stack.ItemID)
	}

	warehouseSlot := cmd.SourceSlotIndex
	if mainIsSource {
		warehouseSlot = cmd.DestinationSlotIndex
	}
	moving, found := record.Warehouse[slotKey(listTypePersonalCargo, warehouseSlot)]
	if !found || moving.ItemID <= 0 || moving.Count <= 0 {
		return cmd, false, fmt.Errorf("%w: personal-cargo list=%d slot=%d", ErrSlotNotFound, listTypePersonalCargo, warehouseSlot)
	}
	if o.stackMerge == nil {
		return cmd, false, fmt.Errorf("%w: item=%d active-PVF page resolver unavailable", ErrReservedSlotRelocationFull, moving.ItemID)
	}
	resolution, resolveErr := o.stackMerge(moving.ItemID)
	if resolveErr != nil || resolution.ItemID != moving.ItemID || resolution.SlotStart <= 0 || resolution.SlotEnd < resolution.SlotStart {
		return cmd, false, fmt.Errorf("%w: item=%d page=%d..%d resolve=%v", ErrReservedSlotRelocationFull, moving.ItemID, resolution.SlotStart, resolution.SlotEnd, resolveErr)
	}
	for slot := resolution.SlotStart; slot <= resolution.SlotEnd; slot++ {
		stack, occupied := record.Slots[slotKey(listTypeMain, slot)]
		if occupied && stack.ItemID != 0 && stack.Count > 0 {
			continue
		}
		if mainIsSource {
			cmd.SourceSlotIndex = slot
		} else {
			cmd.DestinationSlotIndex = slot
		}
		return cmd, true, nil
	}
	return cmd, false, fmt.Errorf("%w: item=%d page=%d..%d", ErrReservedSlotRelocationFull, moving.ItemID, resolution.SlotStart, resolution.SlotEnd)
}

// saveMoveResult enforces the quick-slot equipment invariant against the
// final staged record, immediately before persistence.  Checking the final
// layout covers ordinary moves, swaps, reverse moves and stack branches with
// one rule instead of attempting to infer the client's intended direction.
func (o *Owner) saveMoveResult(
	ctx context.Context,
	record dnfrepo.InventoryRecord,
	srcField dnfrepo.InventoryField,
	dstField dnfrepo.InventoryField,
	result MoveResult,
) (MoveResult, error) {
	if err := o.validateMainQuickSlotCharmExclusivity(record); err != nil {
		return MoveResult{}, err
	}
	// 86JP SendMoveSortLockSignals：搬完后仍被整理锁定钉住的端点槽位要补发 op713，
	// 否则客户端的 padlock 会留在物品刚离开的旧槽位上。
	result.SortLockSignals = moveSortLockSignals(&record, result)
	return saveMoveResult(ctx, o.repo, record, srcField, dstField, result)
}

// validateMainQuickSlotCharmExclusivity implements the current client's
// quick-slot rule for PVF [charm] equipment.  The six quick slots (3..8) can
// contain consumables normally, but only one [charm] equipment may be active
// among them.  This deliberately reads the live PVF resolution rather than
// trusting saved Extra data, item names, or a list of historical charm IDs.
//
// A nil resolver is retained for focused domain tests and old offline tools;
// the current op19 route always installs the active-PVF resolver.  An item
// that no longer resolves in the active archive is not reclassified from
// stale data here, which still lets a player remove legacy rows safely.
func (o *Owner) validateMainQuickSlotCharmExclusivity(record dnfrepo.InventoryRecord) error {
	if o == nil || o.stackMerge == nil || len(record.Slots) == 0 {
		return nil
	}
	charmSlots := make([]int16, 0, 2)
	charmItems := make([]int64, 0, 2)
	for slot := int16(3); slot <= 8; slot++ {
		stack, found := record.Slots[slotKey(listTypeMain, slot)]
		if !found || stack.ItemID <= 0 || stack.Count <= 0 {
			continue
		}
		resolution, err := o.stackMerge(stack.ItemID)
		if err != nil || resolution.ItemID != stack.ItemID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(resolution.Kind), "equipment") ||
			!strings.EqualFold(strings.TrimSpace(resolution.EquipmentType), "[charm]") {
			continue
		}
		charmSlots = append(charmSlots, slot)
		charmItems = append(charmItems, stack.ItemID)
	}
	if len(charmSlots) <= 1 {
		return nil
	}
	return fmt.Errorf("%w: slots=%v items=%v", ErrQuickSlotCharmAlreadyEquipped, charmSlots, charmItems)
}

func isMainPersonalCargoCrossList(sourceListType, destinationListType byte) bool {
	return sourceListType != destinationListType &&
		(sourceListType == listTypeMain || sourceListType == listTypePersonalCargo) &&
		(destinationListType == listTypeMain || destinationListType == listTypePersonalCargo)
}

func (o *Owner) movePetInventory(ctx context.Context, characterID string, cmd Command, segment byte) (MoveResult, error) {
	if o.petItems == nil || o.pets == nil {
		return MoveResult{}, ErrPetInventoryOwnerRequired
	}
	var result MoveResult
	err := o.petItems.WithinCharacterPets(ctx, characterID, func(inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository, pets dnfrepo.PetRepository) error {
		txOwner := *o
		txOwner.repo = inventory
		txOwner.pets = pets
		txOwner.inItemTransaction = true
		txOwner.inPetTransaction = true
		if segment == petInventorySegmentCreature {
			if _, err := creaturestate.ReconcileInventory(ctx, characterID, inventory, equipment, pets); err != nil {
				return err
			}
		}
		var err error
		result, err = txOwner.Move(ctx, cmd)
		if err != nil {
			return err
		}
		if segment == petInventorySegmentCreature && result.Changed {
			_, err = creaturestate.ReconcileInventory(ctx, characterID, inventory, equipment, pets)
		}
		return err
	})
	return result, err
}

const (
	petInventorySegmentCreature byte = iota + 1
	petInventorySegmentArtifact
	petInventorySegmentConsumable
)

func petInventoryMoveSegment(cmd Command) (byte, bool) {
	if cmd.SourceListType != listTypePet || cmd.DestinationListType != listTypePet {
		return 0, false
	}
	source := petInventorySlotSegment(cmd.SourceSlotIndex)
	destination := petInventorySlotSegment(cmd.DestinationSlotIndex)
	return source, source != 0 && source == destination
}

func petInventorySlotSegment(slot int16) byte {
	switch {
	case slot >= 0 && slot <= 139:
		return petInventorySegmentCreature
	case slot >= 140 && slot <= 188:
		return petInventorySegmentArtifact
	case slot >= 189 && slot <= 238:
		return petInventorySegmentConsumable
	default:
		return 0
	}
}

// Sort 按当前客户端已确认的 category 段整理物品栏，并只保存对应 inventory 字段。
// 当前 Go 仓储还没有 sort-lock 表，所以这里只实现无锁安全子集；
// Main/Avatar/PersonalCargo 的专用 op13 刷新均由 handler 在成功 ACK 后发送。
// Pet/list7 同时拥有 PetRecord 状态，必须由 typed pet owner 整体整理，通用 inventory owner fail-close。
func (o *Owner) Sort(ctx context.Context, cmd Command) (SortResult, error) {
	if o == nil || o.repo == nil {
		return SortResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return SortResult{}, ErrCharacterRequired
	}
	if err := checkSortListType(cmd.SourceListType); err != nil {
		return SortResult{}, err
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if cmd.SourceListType == listTypeAccountCargo {
		// 账号金库不在角色 inventory 记录里，必须走账号 owner 的事务。
		return o.sortAccountCargo(ctx, characterID, cmd)
	}
	if !o.inItemTransaction {
		var result SortResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Sort(ctx, cmd)
			return err
		})
		return result, err
	}
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return SortResult{}, err
	}
	if !ok {
		return SortResult{}, ErrInventoryNotFound
	}

	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if record.Warehouse == nil {
		record.Warehouse = make(map[string]dnfrepo.ItemStack)
	}

	items, field := itemMapForList(&record, cmd.SourceListType)
	start, end, ok := sortSegment(cmd.SourceListType, cmd.Category)
	result := SortResult{
		CharacterID: characterID,
		ListType:    cmd.SourceListType,
		Category:    cmd.Category,
		StartSlot:   start,
		EndSlot:     end,
		Mode:        "noop",
	}
	if !ok {
		return result, nil
	}
	moved, changed, err := sortSegmentItems(items, cmd.SourceListType, start, end)
	if err != nil {
		return SortResult{}, err
	}
	result.MovedCount = moved
	if !changed {
		return result, nil
	}

	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, field); err != nil {
		return SortResult{}, err
	}
	result.Mode = "sort"
	result.Changed = true
	return result, nil
}

// Repair 按当前仓储中已有的耐久证据修复背包/仓库装备。
// 这里只允许 repair_gold=0 的安全子集；穿戴装备、全修和金币扣减继续交给后续 equipment/wallet owner。
// Repair repairs one bag/cargo equipment stack through the current runtime-PVF
// contract: PVF evidence prices the job, gold and durability commit in
// the same character-asset transaction, body[5]=1 with an active 魔王契约
// (586) is free, and body[7]=1 quick repair pays the pricetable rate.
// slot=-1 (repair all) is owned by the equipment owner upstream.
func (o *Owner) Repair(ctx context.Context, cmd Command, costResolver alignedcmd.RepairCostResolver) (RepairResult, error) {
	if o == nil || o.repo == nil || o.assets == nil {
		return RepairResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return RepairResult{}, ErrCharacterRequired
	}
	if cmd.SourceSlotIndex == -1 {
		return RepairResult{}, ErrRepairAllRequiresOwners
	}
	if err := checkRepairListType(cmd.SourceListType); err != nil {
		return RepairResult{}, err
	}
	if costResolver == nil {
		return RepairResult{}, ErrRepairCostMissing
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result RepairResult
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		record, ok, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		record.CharacterID = characterID
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if record.Warehouse == nil {
			record.Warehouse = make(map[string]dnfrepo.ItemStack)
		}

		items, field := itemMapForList(&record, cmd.SourceListType)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, ok := items[key]
		if !ok {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		oldDurability, _, err := repairDurabilityOf(stack)
		if err != nil {
			return err
		}
		evidence, err := costResolver(stack.ItemID)
		if err != nil {
			return err
		}
		maxDurability := evidence.MaxDurability
		if maxDurability <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d item=%d", ErrRepairNotRepairable, cmd.SourceListType, cmd.SourceSlotIndex, stack.ItemID)
		}
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			return fmt.Errorf("%w: character=%s has no stats", ErrWalletTxnRequired, characterID)
		}
		gold := character.Stats["gold"]
		result = RepairResult{
			CharacterID:   characterID,
			ListType:      cmd.SourceListType,
			SlotIndex:     cmd.SourceSlotIndex,
			OldDurability: oldDurability,
			NewDurability: maxDurability,
		}
		if oldDurability >= maxDurability {
			result.NewDurability = oldDurability
			result.UpdatedGold = gold
			return nil
		}
		cost := dnfequip.CalcRepairCost(evidence, oldDurability, int(upgradeLevelOf(stack)), cmd.QuickRepair)
		freeRepair := false
		statsChanged := false
		if cost != 0 && cmd.AutoRepair && o.premiumActive(ctx, cmd.AccountID, premium.DevilSlotType(premium.DevilSlotAutoRepair)) {
			if premium.TryConsumeDaily(&character, premium.DevilSlotAutoRepair, time.Now().UTC()) {
				cost = 0
				freeRepair = true
				statsChanged = true
			}
		}
		if cost > gold {
			return fmt.Errorf("%w: cost=%d gold=%d", ErrRepairGoldInsufficient, cost, gold)
		}
		if cost > 0 {
			character.Stats["gold"] = gold - cost
			statsChanged = true
		}
		if statsChanged {
			character.UpdatedAt = time.Now()
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}

		stack = cloneStack(stack)
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 1)
		}
		stack.Extra["durability"] = strconv.FormatInt(maxDurability, 10)
		// 耐久字段的位置**取决于行布局**，不能无条件按旧布局写：
		//   - 当前 0x77 行：品级种子 +0x06、打包的强化/属性字节 +0x0A、耐久 u16 +0x0B（高字节 +0x0C）；
		//   - 旧构造行：创建值 +0x05、打包强化 +0x09、耐久 u16 +0x0A。
		// 旧代码对任何 len>=12 的行都写 [10:12]，于是给当前行把"最大耐久"直接写进了强化字节，
		// 同时耐久只写了一半（高字节留在 +0x0C）——现场 2026-09-18：脱下→修理→调整品级之后，
		// 装备凭空多出强化等级（数值正好等于最大耐久，如腰带 +25、上衣 +33），而且这次品级
		// 调整补发的单行 op14 刷新正好把带毒的属性字节重画了出来。
		if raw, ok := currentRawEntry(stack); ok {
			stack.RawEntry = append([]byte(nil), raw...)
			binary.LittleEndian.PutUint16(stack.RawEntry[0x0B:0x0D], uint16(maxDurability))
		} else if raw, ok := legacyRawEntry(stack); ok {
			stack.RawEntry = append([]byte(nil), raw...)
			binary.LittleEndian.PutUint16(stack.RawEntry[10:12], uint16(maxDurability))
		} else if len(stack.RawEntry) >= 12 {
			// 认不出的行形状：保持历史行为（旧构造布局），绝不改 +0x0A。
			stack.RawEntry = append([]byte(nil), stack.RawEntry...)
			stack.RawEntry[10] = byte(maxDurability)
			stack.RawEntry[11] = byte(maxDurability >> 8)
		}
		items[key] = stack

		record.UpdatedAt = time.Now()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, record, field); err != nil {
			return err
		}
		result.Cost = cost
		result.UpdatedGold = gold - cost
		result.Changed = true
		result.FreeRepair = freeRepair
		return nil
	})
	return result, err
}

// premiumActive reports whether the account currently holds an active premium
// contract of the given type. A missing account repository or record reads as
// inactive, so only the explicit free-repair bonus is gated by it.
func (o *Owner) premiumActive(ctx context.Context, accountID string, premiumType int64) bool {
	if o == nil || o.accounts == nil || strings.TrimSpace(accountID) == "" {
		return false
	}
	account, ok, err := o.accounts.Load(ctx, accountID)
	if err != nil || !ok {
		return false
	}
	return premium.Active(account, premiumType, time.Now().UTC())
}

// Upgrade applies the currently tested current-EXE upgrade flow for
// backpack equipment: validate target, consume the runtime-PVF material from
// either character inventory or account-shared crystal/soul slots, persist the
// packed upgrade byte, then let the handler send op50 + op0D.
func (o *Owner) Upgrade(ctx context.Context, cmd Command) (UpgradeResult, error) {
	base := newUpgradeResult(cmd, upgradeErrorInvalidTarget)
	if o == nil || o.repo == nil {
		return UpgradeResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UpgradeResult{}, ErrCharacterRequired
	}
	if cmd.Mode != "reinforce" && cmd.Mode != "amplify" {
		return newUpgradeResult(cmd, upgradeErrorWrongMode), nil
	}
	// List 38 (公会徽章) reaches this path only as a fully-resolved reinforce:
	// its level, gold cost, material and success weight all come from the native
	// flag_upgrade_table row the aligned resolver selected, so none of them may
	// be missing. Every other list keeps the pre-existing behaviour, including
	// the worn list-3 reinforce this tree added.
	if cmd.TargetListType == listTypeGuildMedal &&
		(!cmd.UpgradeGuildMedal || cmd.Mode != "reinforce" || cmd.TargetSlotIndex < 0 ||
			cmd.TargetSlotIndex > 48 || cmd.OptionalTicketSlot >= 0 || cmd.UpgradePolicyError != "") {
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result UpgradeResult
		wrap := o.withinCharacterAssetTransaction
		// A destruction refund that owns a fixed warehouse slot writes the
		// account record too, so it needs the wider transaction even when the
		// material itself is character-owned (every 增幅 attempt: the 增幅器 is
		// a backpack stackable, the refunded souls and cubes are not). An
		// owner without the account-asset transaction keeps the old scope and
		// falls back to the backpack rather than failing the upgrade.
		if upgradeCommandRequiresAccountMaterial(cmd) ||
			(upgradeDestroyRefundsWarehouse(cmd) &&
				o.accountAssets != nil && strings.TrimSpace(cmd.AccountID) != "") {
			wrap = func(ctx context.Context, characterID string, apply func(*Owner) error) error {
				return o.withinAccountCharacterAssetTransaction(ctx, cmd.AccountID, characterID, apply)
			}
		}
		err := wrap(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Upgrade(ctx, cmd)
			return err
		})
		if err != nil {
			return UpgradeResult{}, err
		}
		if result.CharacterID == "" {
			result = base
			result.CharacterID = characterID
		}
		// Mail the refund rows the bag refused, now that the upgrade itself is
		// durable. This cannot join the transaction above: it covers
		// characters, inventory and equipment, while mailboxes live in their
		// own unit of work. A mail failure is the mailer's to report -- the
		// destruction has already committed and must not be undone for it.
		if len(result.DestroyRefundOverflow) > 0 && o.upgradeRefundMailer != nil {
			if mailErr := o.upgradeRefundMailer(ctx, characterID, result.DestroyRefundOverflow); mailErr == nil {
				result.DestroyRefundMailed = true
			}
		}
		return result, nil
	}

	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UpgradeResult{}, err
	}
	if !ok {
		return UpgradeResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}

	result := base
	result.CharacterID = characterID
	items := record.Slots
	// A worn target (op50 mode word 0x03xx) lives in the equipment record, not
	// the backpack. It is read through the same entry-to-stack projection the
	// enchant and forging owners use, mutated by the shared level logic below,
	// and written back to its EquipmentEntry before the transaction commits.
	// The material, protection ticket and destroy bonus stay in the backpack.
	targetEquipped := cmd.TargetListType == listTypeEquipment
	var equipmentRecord dnfrepo.EquipmentRecord
	var targetEntry dnfrepo.EquipmentEntry
	equipmentKey := strconv.Itoa(int(cmd.TargetSlotIndex))
	targetKey := slotKey(cmd.TargetListType, cmd.TargetSlotIndex)
	var target dnfrepo.ItemStack
	switch {
	case targetEquipped:
		if o.equipment == nil {
			return UpgradeResult{}, ErrUpgradeEquipmentOwnerUnavailable
		}
		var found bool
		equipmentRecord, found, err = o.equipment.Load(ctx, characterID)
		if err != nil {
			return UpgradeResult{}, err
		}
		if !found {
			return result, nil
		}
		equipmentRecord = dnfrepo.CloneEquipment(equipmentRecord)
		targetEntry, ok = equipmentRecord.Entries[equipmentKey]
		if !ok || targetEntry.ItemID <= 0 || targetEntry.SlotIndex != cmd.TargetSlotIndex {
			return result, nil
		}
		target = forgingStackFromEquipmentEntry(targetEntry)
	case cmd.TargetListType == listTypeMain, cmd.TargetListType == listTypeGuildMedal:
		// Both live in the backpack-style slot map: the 公会徽章 page is a set of
		// list-38 rows inside the same inventory record.
		target, ok = items[targetKey]
	default:
		return result, nil
	}
	if !ok || target.Count <= 0 || target.ItemID != int64(cmd.TargetItemTemplateID) || !isUpgradeableEquipmentStack(target) {
		return result, nil
	}
	if isEquipmentLocked(target) {
		result.ErrorCode = upgradeErrorLocked
		return result, nil
	}
	if !upgradeDurabilityOK(target) {
		result.ErrorCode = upgradeErrorDurability
		return result, nil
	}
	oldLevel := upgradeLevelOf(target)
	if cmd.TargetListType == listTypeGuildMedal {
		// An expired flag cannot be upgraded, and the resolved row has to match
		// the stored stack exactly: one flag in the slot, at the level the row
		// was resolved for, with a usable gold/material/weight triple.
		if !target.ExpireAt.IsZero() && !target.ExpireAt.After(time.Now()) {
			return result, nil
		}
		if target.Count != 1 || int(oldLevel) != cmd.UpgradePolicyLevel || cmd.UpgradeGoldCost < 0 ||
			cmd.UpgradeMaterialItemID <= 0 || cmd.UpgradeMaterialCount <= 0 ||
			cmd.UpgradeSuccessWeight < 0 || cmd.UpgradeSuccessWeight > currentUpgradeSuccessBase {
			return result, nil
		}
	}
	result.OldLevel = oldLevel
	result.NewLevel = oldLevel
	portableTool := cmd.UpgradePortableAmplifier || cmd.UpgradePortableReinforcer
	if portableTool {
		if (cmd.UpgradePortableAmplifier && cmd.Mode != "amplify") ||
			(cmd.UpgradePortableReinforcer && cmd.Mode != "reinforce") ||
			cmd.UpgradePortableMinimum < 0 || cmd.UpgradePortableMaximum < cmd.UpgradePortableMinimum ||
			int64(oldLevel) < cmd.UpgradePortableMinimum || int64(oldLevel) > cmd.UpgradePortableMaximum ||
			cmd.UpgradeTargetLevelMinimum < 0 || cmd.UpgradeTargetLevelMaximum < cmd.UpgradeTargetLevelMinimum ||
			cmd.UpgradeTargetMinimumLevel < cmd.UpgradeTargetLevelMinimum || cmd.UpgradeTargetMinimumLevel > cmd.UpgradeTargetLevelMaximum {
			result.ErrorCode = upgradeErrorInvalidTarget
			return result, nil
		}
	}
	if oldLevel > upgradeMaximumCurrentLevelBeforeReject {
		result.ErrorCode = upgradeErrorMaxLevel
		return result, nil
	}

	amplifyType, amplifyValue := upgradeAmplifyState(target)
	switch cmd.Mode {
	case "reinforce":
		if amplifyType != 0 {
			result.ErrorCode = upgradeErrorWrongMode
			return result, nil
		}
	case "amplify":
		if amplifyType == 0 {
			result.ErrorCode = upgradeErrorWrongMode
			return result, nil
		}
		if amplifyValue <= 0 || (amplifyType&0x80) != 0 {
			result.ErrorCode = upgradeErrorAmplifyNotIdentified
			return result, nil
		}
	}

	var protectionKey string
	var protectionTicket dnfrepo.ItemStack
	var protectionResolution alignedcmd.UpgradeProtectionResolution
	hasProtectionTicket := cmd.OptionalTicketSlot >= 0
	if hasProtectionTicket {
		if cmd.OptionalTicketSlot == cmd.TargetSlotIndex ||
			cmd.OptionalTicketSlot == cmd.MaterialSlotIndex ||
			!upgradeProtectionSlotAllowed(cmd.OptionalTicketSlot) ||
			dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.OptionalTicketSlot) {
			result.ErrorCode = upgradeErrorUnsupportedOptionalTicket
			return result, nil
		}
		protectionKey = slotKey(listTypeMain, cmd.OptionalTicketSlot)
		stack, found := items[protectionKey]
		// A past [expiration date] does not disqualify a protection ticket on
		// this server: nothing here acts on item expiry -- the reconcile pass
		// only normalizes the row's projection, it never removes or blocks an
		// expired stack -- and the client itself counts held tickets without
		// checking it (dstr 72968 「因为您拥有装备强化保护券…」). Enforcing it
		// here alone meant a player watched a piece blow up while holding ten
		// tickets, because most of this PVF's protection tickets are dead 2010
		// / 2015 event items (7804, 10007840, 10004511, 10007841).
		if !found || stack.ItemID <= 0 || stack.Count <= 0 || o.upgradeProtection == nil {
			result.ErrorCode = upgradeErrorUnsupportedOptionalTicket
			return result, nil
		}
		protectionResolution, err = o.upgradeProtection(stack.ItemID)
		if err != nil || strings.TrimSpace(protectionResolution.TicketMode) == "" {
			result.ErrorCode = upgradeErrorUnsupportedOptionalTicket
			return result, nil
		}
		if !strings.EqualFold(strings.TrimSpace(protectionResolution.TicketMode), cmd.Mode) {
			result.ErrorCode = upgradeErrorWrongMode
			return result, nil
		}
		if protectionResolution.FailureRetainLevel < 0 || protectionResolution.FailureRetainLevel > 30 {
			result.ErrorCode = upgradeErrorUnsupportedOptionalTicket
			return result, nil
		}
		protectionTicket = stack
		result.ProtectionTicketItemID = stack.ItemID
		result.ProtectionTicketRemaining = stack.Count
	}

	const (
		upgradeMaterialNone = iota
		upgradeMaterialMain
		upgradeMaterialAccount
	)
	var materialKey string
	var material dnfrepo.ItemStack
	// accountShared is the account-owned warehouse record. It backs both the
	// crystal/soul upgrade material and the destruction refund rows that own a
	// fixed warehouse slot, so it is loaded at most once and saved at most
	// once no matter which of the two touched it.
	var accountShared dnfrepo.AccountInventoryRecord
	accountSharedLoaded := false
	accountSharedDirty := false
	materialOwner := upgradeMaterialNone
	requiredMaterialItemID, requiredMaterialCount, hasPVFMaterial := upgradeMaterialRequirement(cmd)
	// The material must be a different stack from the target. Compare the list
	// too: a list-3 (worn) or list-38 (公会徽章) target is reinforced with a
	// backpack material, and a backpack slot may legitimately carry the same
	// index as the equipment/medal slot. For a list-0 target both indices name
	// the same namespace, so the original rejection is unchanged.
	if cmd.MaterialSlotIndex >= 0 && cmd.MaterialSlotIndex == cmd.TargetSlotIndex && cmd.TargetListType == listTypeMain {
		result.ErrorCode = upgradeErrorInvalidMaterial
		return result, nil
	}
	if hasPVFMaterial {
		if fixedSlot, accountOwned := upgradeAccountSharedMaterialSlot(requiredMaterialItemID); accountOwned {
			if o.accountRepo == nil || strings.TrimSpace(cmd.AccountID) == "" {
				result.ErrorCode = upgradeErrorInvalidMaterial
				return result, nil
			}
			account, found, err := o.accountRepo.Load(ctx, cmd.AccountID)
			if err != nil {
				return UpgradeResult{}, err
			}
			if !found || account.Slots == nil {
				result.ErrorCode = upgradeErrorInvalidMaterial
				return result, nil
			}
			accountShared = dnfrepo.CloneAccountInventory(account)
			accountShared.AccountID = strings.TrimSpace(cmd.AccountID)
			accountSharedLoaded = true
			materialKey = dnfrepo.AccountSharedInventorySlotKey(fixedSlot)
			stack, ok := accountShared.Slots[materialKey]
			if !ok || stack.ItemID != requiredMaterialItemID || stack.Count < requiredMaterialCount {
				result.ErrorCode = upgradeErrorInvalidMaterial
				return result, nil
			}
			material = stack
			materialOwner = upgradeMaterialAccount
		} else {
			if cmd.MaterialSlotIndex < 0 || dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.MaterialSlotIndex) {
				result.ErrorCode = upgradeErrorInvalidMaterial
				return result, nil
			}
			materialKey = slotKey(listTypeMain, cmd.MaterialSlotIndex)
			stack, ok := items[materialKey]
			if !ok || stack.ItemID != requiredMaterialItemID || stack.Count < requiredMaterialCount ||
				(portableTool && !stack.ExpireAt.IsZero() && !stack.ExpireAt.After(time.Now())) {
				result.ErrorCode = upgradeErrorInvalidMaterial
				return result, nil
			}
			material = stack
			materialOwner = upgradeMaterialMain
		}
	} else if cmd.MaterialSlotIndex >= 0 {
		if dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.MaterialSlotIndex) {
			result.ErrorCode = upgradeErrorInvalidMaterial
			return result, nil
		}
		materialKey = slotKey(listTypeMain, cmd.MaterialSlotIndex)
		stack, ok := items[materialKey]
		if !ok || stack.Count <= 0 {
			result.ErrorCode = upgradeErrorInvalidMaterial
			return result, nil
		}
		requiredMaterialCount = 1
		material = stack
		materialOwner = upgradeMaterialMain
	}

	// ensureAccountSharedRecord hands out the one warehouse record this
	// upgrade may mutate. Unlike the material path a missing record is not an
	// error here: a destruction refund simply falls back to the backpack when
	// the account warehouse cannot be reached.
	ensureAccountSharedRecord := func() (*dnfrepo.AccountInventoryRecord, bool) {
		if accountSharedLoaded {
			return &accountShared, true
		}
		if o.accountRepo == nil || strings.TrimSpace(cmd.AccountID) == "" {
			return nil, false
		}
		account, found, loadErr := o.accountRepo.Load(ctx, cmd.AccountID)
		if loadErr != nil {
			return nil, false
		}
		if !found {
			account = dnfrepo.AccountInventoryRecord{}
		}
		accountShared = dnfrepo.CloneAccountInventory(account)
		accountShared.AccountID = strings.TrimSpace(cmd.AccountID)
		if accountShared.Slots == nil {
			accountShared.Slots = make(map[string]dnfrepo.ItemStack)
		}
		accountSharedLoaded = true
		return &accountShared, true
	}
	// warehouseRefundSlots are the fixed warehouse slots a destruction refund
	// wrote into, collected so the op0D/op14 refresh can carry their new
	// counts: they live in the account record, not in this character's
	// backpack map.
	var warehouseRefundSlots []int16
	// grantUpgradeDestroyRefund places one destruction refund row: warehouse
	// stackables (the six cube fragments and six souls) go to their single
	// fixed account-shared slot the way the 分解机 places them, and only the
	// remainder -- a stack-limit overflow, a foreign stack squatting on the
	// fixed slot, or an unreachable warehouse -- falls back to the backpack.
	// The returned remainder is what neither could hold, so a best-effort row
	// that is half placed only mails the half that was refused.
	grantUpgradeDestroyRefund := func(itemID int64, count int64) ([]int16, int64, error) {
		remaining := count
		var slots []int16
		if fixedSlot, warehouse := upgradeAccountSharedMaterialSlot(itemID); warehouse {
			if account, ok := ensureAccountSharedRecord(); ok {
				left := o.grantUpgradeDestroyWarehouseBonus(account, fixedSlot, itemID, remaining)
				if left < remaining {
					accountSharedDirty = true
					warehouseRefundSlots = append(warehouseRefundSlots, fixedSlot)
					slots = append(slots, fixedSlot)
				}
				remaining = left
			}
		}
		if remaining <= 0 {
			return slots, 0, nil
		}
		bagSlots, grantErr := o.grantUpgradeDestroyBonus(items, itemID, remaining)
		if grantErr != nil {
			return slots, remaining, grantErr
		}
		return append(slots, bagSlots...), 0, nil
	}

	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return UpgradeResult{}, err
	}
	if !ok {
		return UpgradeResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 1)
	}
	result.UpdatedGold = character.Stats["gold"]
	goldCost := upgradeGoldCostOf(target, cmd.Mode, oldLevel)
	if cmd.UpgradeGoldCostResolved {
		goldCost = cmd.UpgradeGoldCost
	}
	if cmd.TargetListType == listTypeGuildMedal {
		// The flag table states its own gold cost; the equipment table's
		// rarity/level formula does not describe a 公会徽章. Upstream applies the
		// resolved cost unconditionally here, so a direct owner call is priced
		// from the policy rather than from a stale 0x77 upgrade_gold extra.
		goldCost = cmd.UpgradeGoldCost
	}
	if portableTool {
		goldCost = 0
	}
	if goldCost > result.UpdatedGold {
		result.ErrorCode = upgradeErrorInsufficientGold
		result.GoldCost = goldCost
		return result, nil
	}

	// RNG: determine success or failure based on PVF success weight.
	// 权重 <= 0 说明调用方没给策略：生产路径只有 handler.go:Upgrade 一处，
	// 它拿不到 PVF 强化策略时会**直接拒绝**（见 handler.go 的 fail-closed 分支），
	// 所以这里保留历史回落只为直接调用 owner 的夹具/脚本。
	successWeight := cmd.UpgradeSuccessWeight
	if successWeight <= 0 && cmd.TargetListType != listTypeGuildMedal {
		successWeight = currentUpgradeSuccessBase // backward compat: guaranteed success
	}
	// 强化/增幅成功率药剂（强化秘药、强化的奥秘、永恒的增幅的秘密、赛丽亚的幸运强化药剂）
	// 在这里生效：**掷点之前**按 PVF 的 param 放大权重。窗口本身是角色级的、带到期时间，
	// 过期会被 statbuff.ActiveRate 判掉。见 stat_change_potion.go 的
	// applyCurrentUpgradeSuccessWindow。
	{
		upgradeNow := time.Now().UTC()
		if o.now != nil {
			upgradeNow = o.now().UTC()
		}
		successWeight = applyCurrentUpgradeSuccessWindow(successWeight, character, cmd.Mode, upgradeNow)
	}
	roll := o.upgradeRandom
	if roll == nil {
		roll = rand.Intn
	}
	rolled := roll(currentUpgradeSuccessBase)
	upgradeSuccess := rolled < successWeight
	if !upgradeSuccess && cmd.UpgradePenaltyType == 3 && !hasProtectionTicket && cmd.OptionalTicketSlot < 0 {
		autoSlot, autoStack, autoResolution, found, resolveErr := o.firstUpgradeProtectionTicket(items, cmd.Mode, cmd.TargetSlotIndex, cmd.MaterialSlotIndex)
		if resolveErr != nil {
			return UpgradeResult{}, resolveErr
		}
		if found {
			hasProtectionTicket = true
			protectionKey = slotKey(listTypeMain, autoSlot)
			protectionTicket = autoStack
			protectionResolution = autoResolution
			result.ProtectionTicketSlotIndex = autoSlot
			result.ProtectionTicketItemID = autoStack.ItemID
			result.ProtectionTicketRemaining = autoStack.Count
		}
	}

	var newLevel byte
	var resultCode byte
	if upgradeSuccess {
		newLevel = byte(int(oldLevel) + 1)
		resultCode = upgradeResultCodeSuccess
		target = cloneStack(target)
		setUpgradeLevel(&target, newLevel)
		items[targetKey] = target
	} else {
		newLevel = oldLevel
		switch cmd.UpgradePenaltyType {
		case 3: // destroy
			if hasProtectionTicket {
				resultCode = upgradeResultCodeDowngrade
				retainLevel := protectionResolution.FailureRetainLevel
				if retainLevel > int(oldLevel) {
					retainLevel = int(oldLevel)
				}
				newLevel = byte(retainLevel)
				target = cloneStack(target)
				setUpgradeLevel(&target, newLevel)
				items[targetKey] = target
				result.ProtectionTicketUsed = true
			} else {
				resultCode = upgradeResultCodeDestroy
				delete(items, targetKey)
				// Grant destruction compensation through the active PVF stack and
				// inventory-page contract. The compensation is a material stackable,
				// not equipment merely because the destroyed source occupied an
				// equipment-page slot.
				if cmd.UpgradeDestroyBonusItemID > 0 && cmd.UpgradeDestroyBonusCount > 0 {
					destroySlots, _, grantErr := grantUpgradeDestroyRefund(
						int64(cmd.UpgradeDestroyBonusItemID),
						int64(cmd.UpgradeDestroyBonusCount),
					)
					if grantErr != nil {
						return UpgradeResult{}, grantErr
					}
					result.DestroyBonusSlot = destroySlots[0]
					result.DestroyBonusSlots = destroySlots
					result.DestroyBonusItemID = int32(cmd.UpgradeDestroyBonusItemID)
					result.DestroyBonusCount = int32(cmd.UpgradeDestroyBonusCount)
				}
				for _, extra := range cmd.UpgradeDestroyExtraRewards {
					if extra.ItemID <= 0 || extra.Count <= 0 {
						continue
					}
					extraSlots, unplaced, grantErr := grantUpgradeDestroyRefund(int64(extra.ItemID), int64(extra.Count))
					if grantErr != nil {
						// A best-effort row that does not fit is dropped: the
						// destruction and its PVF compensation have to stand
						// even when the bag is full. Only the part neither the
						// warehouse nor the bag took is mailed.
						if extra.BestEffort && errors.Is(grantErr, ErrUpgradeDestroyBonusInventoryFull) {
							overflow := extra
							if unplaced > 0 && unplaced <= int64(extra.Count) {
								overflow.Count = int(unplaced)
							}
							result.DestroyRefundOverflow = append(result.DestroyRefundOverflow, overflow)
							// The warehouse half, if any, did land and still
							// has to be repainted.
							result.DestroyBonusSlots = append(result.DestroyBonusSlots, extraSlots...)
							if len(extraSlots) > 0 {
								result.DestroyExtraRewards = append(result.DestroyExtraRewards, UpgradeDestroyGrantedReward{
									Slot:   extraSlots[0],
									ItemID: int32(extra.ItemID),
									Count:  int32(int64(extra.Count) - unplaced),
								})
							}
							continue
						}
						return UpgradeResult{}, grantErr
					}
					result.DestroyBonusSlots = append(result.DestroyBonusSlots, extraSlots...)
					result.DestroyExtraRewards = append(result.DestroyExtraRewards, UpgradeDestroyGrantedReward{
						Slot:   extraSlots[0],
						ItemID: int32(extra.ItemID),
						Count:  int32(extra.Count),
					})
				}
			}
		case 1: // downgrade
			resultCode = upgradeResultCodeDowngrade
			if oldLevel > 0 {
				newLevel = oldLevel - 1
			}
			target = cloneStack(target)
			setUpgradeLevel(&target, newLevel)
			items[targetKey] = target
		case 2: // downgrade by the PVF row's explicit value
			resultCode = upgradeResultCodeDowngrade
			penalty := cmd.UpgradePenaltyValue
			if penalty < 0 {
				penalty = 0
			}
			if penalty > int(oldLevel) {
				penalty = int(oldLevel)
			}
			newLevel = byte(int(oldLevel) - penalty)
			target = cloneStack(target)
			setUpgradeLevel(&target, newLevel)
			items[targetKey] = target
		default: // 0: fail, no change
			resultCode = upgradeResultCodeFailNoChange
		}
	}

	if result.ProtectionTicketUsed {
		protectionTicket = cloneStack(protectionTicket)
		protectionTicket.Count--
		result.ProtectionTicketRemaining = protectionTicket.Count
		if protectionTicket.Count <= 0 {
			delete(items, protectionKey)
			result.ProtectionTicketRemaining = 0
			result.ProtectionTicketUpdatedStack = dnfrepo.ItemStack{}
		} else {
			updateStackRawAmount(&protectionTicket)
			items[protectionKey] = protectionTicket
			result.ProtectionTicketUpdatedStack = protectionTicket
		}
	}

	if materialOwner != upgradeMaterialNone {
		// The destruction refund above may already have merged into this very
		// stack: the upgrade material is usually one of the fixed warehouse
		// stackables itself (无色小晶块 owns slot 358, the souls own 360-365)
		// and a destroyed piece refunds exactly those into the same fixed
		// slot. material is the copy captured before that grant, so writing it
		// back erased the refund while op80 and the op14 refresh went on
		// advertising it -- live 2026-09-14 22:03, character 1, +14 -> +15
		// destruction promised 3037 x368 in slot 358 and the durable stack
		// only ever lost the 200 cubes the attempt consumed (48316 -> 48116,
		// which is also why 小退 could not show it). Re-base the consumption on
		// what the record holds now, so the two writers add up instead of the
		// Re-base the consumption on what the record holds now, so the two
		// writers add up instead of the later one winning. Anything unexpected
		// (a different item, a stack that somehow shrank) keeps the captured
		// copy, which is the historical behavior.
		base := material
		switch materialOwner {
		case upgradeMaterialMain:
			if live, ok := items[materialKey]; ok && live.ItemID == material.ItemID &&
				live.ItemID > 0 && live.Count >= material.Count {
				base = live
			}
		case upgradeMaterialAccount:
			if live, ok := accountShared.Slots[materialKey]; ok && live.ItemID == material.ItemID &&
				live.ItemID > 0 && live.Count >= material.Count {
				base = live
			}
		}
		material = cloneStack(base)
		material.Count -= requiredMaterialCount
		result.MaterialRemainingStackCount = material.Count
		result.MaterialUpdated = true
		if material.Count <= 0 {
			result.MaterialRemainingStackCount = 0
			result.MaterialUpdatedStack = dnfrepo.ItemStack{}
			switch materialOwner {
			case upgradeMaterialMain:
				delete(items, materialKey)
			case upgradeMaterialAccount:
				delete(accountShared.Slots, materialKey)
			}
		} else {
			updateStackRawAmount(&material)
			result.MaterialUpdatedStack = material
			switch materialOwner {
			case upgradeMaterialMain:
				items[materialKey] = material
			case upgradeMaterialAccount:
				accountShared.Slots[materialKey] = material
			}
		}
		if materialOwner == upgradeMaterialAccount {
			accountSharedDirty = true
		}
	}
	// One save for both writers: the consumed crystal/soul material and the
	// destruction refund rows that landed in their fixed warehouse slots.
	if accountSharedDirty && o.accountRepo != nil {
		accountShared.UpdatedAt = time.Now()
		if err := o.accountRepo.Save(ctx, accountShared); err != nil {
			return UpgradeResult{}, err
		}
	}
	if goldCost > 0 {
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 1)
		}
		character.Stats["gold"] -= goldCost
		character.UpdatedAt = time.Now()
		if err := dnfrepo.SaveCharacterFields(ctx, o.characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return UpgradeResult{}, err
		}
		result.GoldCost = goldCost
		result.UpdatedGold = character.Stats["gold"]
	}

	if targetEquipped {
		// The shared level logic above addressed the target through the list-3
		// key of the backpack map. That key must never be persisted there; move
		// the outcome back into the worn entry (or drop the entry on destroy).
		updatedTarget, changed := items[targetKey]
		delete(items, targetKey)
		switch {
		case resultCode == upgradeResultCodeDestroy:
			delete(equipmentRecord.Entries, equipmentKey)
			target = dnfrepo.ItemStack{}
		case changed:
			targetEntry.RawEntry = append([]byte(nil), updatedTarget.RawEntry...)
			targetEntry.Extra = cloneStringMapForForging(updatedTarget.Extra)
			equipmentRecord.Entries[equipmentKey] = targetEntry
			target = updatedTarget
		}
	}
	now := time.Now()
	record.UpdatedAt = now
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return UpgradeResult{}, err
	}
	if targetEquipped {
		equipmentRecord.CharacterID = characterID
		equipmentRecord.UpdatedAt = now
		if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
			return UpgradeResult{}, err
		}
	}
	result.Success = true
	result.ErrorCode = 0
	result.NewLevel = newLevel
	result.ResultCode = resultCode
	result.UpgradeSucceeded = upgradeSuccess
	if targetEquipped {
		result.TargetUpdatedStack = cloneStack(target)
	} else if updatedTarget, ok := items[targetKey]; ok {
		result.TargetUpdatedStack = updatedTarget
	} else {
		result.TargetUpdatedStack = dnfrepo.ItemStack{}
	}
	result.MainRefresh = cloneItemMap(record.Slots)
	// The warehouse rows a destruction refund wrote live in the account
	// record, but the client paints them in the same list 0. Overlay them so
	// the incremental refresh reports their new counts instead of the stale
	// character-side view.
	for _, slot := range warehouseRefundSlots {
		if stack, ok := accountShared.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]; ok {
			result.MainRefresh[slotKey(listTypeMain, slot)] = cloneStack(stack)
		}
	}
	result.Changed = true
	return result, nil
}

// Enchant applies one monster card carried by a bead stackable to one main
// inventory equipment entry, one worn list-3 equipment entry, one list-7
// creature, or the worn creature at list-3 slot 26. Ordinary/list-7 rows use
// +0x0E i32 card item id and +0x12 u8 upgrade count; the legacy
// equipped-creature row uses +0x10 and +0x14.
// Creature targets additionally update their typed PetEntry in the same
// character-pet transaction. The server never resolves card stat values,
// because the client renders them from its own card PVF.
func (o *Owner) Enchant(ctx context.Context, cmd Command, resolver alignedcmd.EnchantBeadResolver) (EnchantResult, error) {
	base := EnchantResult{
		ErrorCode:       enchantErrorInvalidBead,
		TargetListType:  cmd.TargetListType,
		TargetSlotIndex: cmd.TargetSlotIndex,
		BeadSlotIndex:   cmd.BeadSlotIndex,
	}
	if o == nil || o.repo == nil {
		return EnchantResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return EnchantResult{}, ErrCharacterRequired
	}
	if resolver == nil {
		return EnchantResult{}, ErrEnchantResolverRequired
	}
	equippedCreatureTarget := cmd.TargetListType == listTypeEquipment && cmd.TargetSlotIndex == 26
	equippedEquipmentTarget := cmd.TargetListType == listTypeEquipment && !equippedCreatureTarget
	if cmd.BeadListType != listTypeMain ||
		(cmd.TargetListType != listTypeMain && cmd.TargetListType != listTypePet && !equippedCreatureTarget && !equippedEquipmentTarget) {
		base.ErrorCode = enchantErrorUnsupported
		return base, nil
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if (cmd.TargetListType == listTypePet || equippedCreatureTarget) && !o.inPetTransaction {
		if o.petItems == nil || o.pets == nil || o.equipment == nil {
			return EnchantResult{}, ErrPetInventoryOwnerRequired
		}
		var result EnchantResult
		err := o.petItems.WithinCharacterPets(ctx, characterID, func(inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository, pets dnfrepo.PetRepository) error {
			txOwner := *o
			txOwner.repo = inventory
			txOwner.equipment = equipment
			txOwner.pets = pets
			txOwner.inItemTransaction = true
			txOwner.inPetTransaction = true
			if _, err := creaturestate.ReconcileInventory(ctx, characterID, inventory, equipment, pets); err != nil {
				return err
			}
			var err error
			result, err = txOwner.Enchant(ctx, cmd, resolver)
			return err
		})
		if err != nil {
			return EnchantResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}
	if (cmd.TargetListType == listTypeMain || equippedEquipmentTarget) && !o.inItemTransaction {
		var result EnchantResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.Enchant(ctx, cmd, resolver)
			return err
		})
		if err != nil {
			return EnchantResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return EnchantResult{}, err
	}
	if !ok {
		return EnchantResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}

	result := base
	items := record.Slots
	beadKey := slotKey(listTypeMain, cmd.BeadSlotIndex)
	bead, ok := items[beadKey]
	if !ok || bead.Count <= 0 {
		return result, nil
	}
	result.BeadItemID = bead.ItemID
	if cmd.TargetListType == cmd.BeadListType && cmd.TargetSlotIndex == cmd.BeadSlotIndex {
		result.ErrorCode = enchantErrorInvalidTarget
		return result, nil
	}
	targetKey := slotKey(cmd.TargetListType, cmd.TargetSlotIndex)
	var target dnfrepo.ItemStack
	var targetEquipment dnfrepo.EquipmentEntry
	var equipmentRecord dnfrepo.EquipmentRecord
	if equippedCreatureTarget || equippedEquipmentTarget {
		if o.equipment == nil {
			return EnchantResult{}, ErrEnchantRequiresEquipmentOwner
		}
		equipmentRecord, ok, err = o.equipment.Load(ctx, characterID)
		if err != nil {
			return EnchantResult{}, err
		}
		if !ok {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
		equipmentRecord = dnfrepo.CloneEquipment(equipmentRecord)
		targetEquipment, ok = equipmentRecord.Entries[strconv.Itoa(int(cmd.TargetSlotIndex))]
		if !ok || targetEquipment.ItemID <= 0 || targetEquipment.SlotIndex != cmd.TargetSlotIndex {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
		if equippedCreatureTarget {
			target = equippedCreatureStack(targetEquipment)
		} else {
			target = forgingStackFromEquipmentEntry(targetEquipment)
		}
	} else {
		target, ok = items[targetKey]
		if !ok || target.Count <= 0 {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
	}
	result.TargetItemID = target.ItemID
	if !equipmentStackCountIsSingle(target) {
		// 一件宝珠只能附一件装备：历史堆叠行（装备 Count>1）一律拒绝。
		result.ErrorCode = enchantErrorInvalidTarget
		return result, nil
	}

	resolution, err := resolver(bead.ItemID, target.ItemID)
	if err != nil {
		return EnchantResult{}, err
	}
	randomTitleEnchant := resolution.RandomTitleEnchantConfigID != 0 || resolution.RandomTitleEnchantRowID != 0
	if randomTitleEnchant && (resolution.RandomTitleEnchantConfigID == 0 || resolution.RandomTitleEnchantRowID == 0 || cmd.TargetListType != listTypeMain) {
		result.ErrorCode = enchantErrorUnsupported
		return result, nil
	}
	if !randomTitleEnchant && resolution.CardItemID <= 0 {
		return result, nil
	}
	if !randomTitleEnchant {
		result.CardItemID = resolution.CardItemID
	}
	if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") {
		result.ErrorCode = enchantErrorInvalidTarget
		return result, nil
	}
	if len(resolution.TargetWhitelist) > 0 && !int64SliceContains(resolution.TargetWhitelist, target.ItemID) {
		result.ErrorCode = enchantErrorInvalidTarget
		return result, nil
	}
	if !stringSliceContainsFold(resolution.AllowedEquipmentTypes, resolution.TargetEquipmentType) {
		result.ErrorCode = enchantErrorInvalidTarget
		return result, nil
	}
	var petRecord dnfrepo.PetRecord
	var petEntryKey string
	var petEntry dnfrepo.PetEntry
	if cmd.TargetListType == listTypePet || equippedCreatureTarget {
		if (!equippedCreatureTarget && (cmd.TargetSlotIndex < 0 || cmd.TargetSlotIndex > 139)) ||
			(equippedCreatureTarget && cmd.TargetSlotIndex != 26) ||
			!strings.EqualFold(strings.Trim(strings.TrimSpace(resolution.TargetEquipmentType), "[]"), "creature") {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
		if o.pets == nil {
			return EnchantResult{}, ErrPetInventoryOwnerRequired
		}
		var found bool
		petRecord, found, err = o.pets.Load(ctx, characterID)
		if err != nil {
			return EnchantResult{}, err
		}
		if !found {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
		petRecord = dnfrepo.ClonePet(petRecord)
		if equippedCreatureTarget {
			petEntryKey, petEntry, found = enchantEquippedPetEntryForTarget(petRecord, target, cmd.TargetSlotIndex)
		} else {
			petEntryKey, petEntry, found = enchantPetEntryForTarget(petRecord, target, cmd.TargetSlotIndex)
		}
		if !found {
			result.ErrorCode = enchantErrorInvalidTarget
			return result, nil
		}
	}
	upgradeCount := enchantUpgradeCountOf(bead)
	if randomTitleEnchant && upgradeCount != 0 {
		result.ErrorCode = enchantErrorUnsupported
		return result, nil
	}
	if len(resolution.UpgradeCounts) > 0 {
		if !int64SliceContains(resolution.UpgradeCounts, int64(upgradeCount)) {
			result.ErrorCode = enchantErrorUnsupported
			return result, nil
		}
	} else if upgradeCount != 0 {
		result.ErrorCode = enchantErrorUnsupported
		return result, nil
	}

	target = cloneStack(target)
	if randomTitleEnchant {
		setRandomTitleEnchantFields(&target, resolution.RandomTitleEnchantConfigID, resolution.RandomTitleEnchantRowID)
	} else {
		setEnchantFields(&target, resolution.CardItemID, upgradeCount)
		if cmd.TargetListType == listTypePet {
			setPetStackEnchantFields(&target, resolution.CardItemID, upgradeCount)
		}
	}
	if equippedCreatureTarget {
		setEquippedCreatureEnchantFields(&targetEquipment, resolution.CardItemID, upgradeCount)
		equipmentRecord.Entries[strconv.Itoa(int(cmd.TargetSlotIndex))] = targetEquipment
	} else if equippedEquipmentTarget {
		targetEquipment.RawEntry = append([]byte(nil), target.RawEntry...)
		targetEquipment.Extra = cloneEnchantExtra(target.Extra)
		equipmentRecord.Entries[strconv.Itoa(int(cmd.TargetSlotIndex))] = targetEquipment
	} else {
		items[targetKey] = target
	}
	result.TargetUpdatedStack = cloneStack(target)
	if cmd.TargetListType == listTypePet || equippedCreatureTarget {
		setPetEntryEnchantFields(&petEntry, resolution.CardItemID, upgradeCount)
		petRecord.Entries[petEntryKey] = petEntry
	}

	bead = cloneStack(bead)
	bead.Count--
	result.BeadRemainingCount = bead.Count
	if bead.Count <= 0 {
		delete(items, beadKey)
		result.BeadRemainingCount = 0
		result.BeadUpdatedStack = dnfrepo.ItemStack{ItemID: -1}
	} else {
		updateStackRawAmount(&bead)
		items[beadKey] = bead
		result.BeadUpdatedStack = cloneStack(bead)
	}

	now := time.Now()
	record.UpdatedAt = now
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return EnchantResult{}, err
	}
	if equippedCreatureTarget || equippedEquipmentTarget {
		equipmentRecord.CharacterID = characterID
		equipmentRecord.UpdatedAt = now
		if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
			return EnchantResult{}, err
		}
	}
	if cmd.TargetListType == listTypePet || equippedCreatureTarget {
		petRecord.CharacterID = characterID
		petRecord.UpdatedAt = now
		if err := dnfrepo.SavePetFields(ctx, o.pets, petRecord, dnfrepo.PetFieldEntries); err != nil {
			return EnchantResult{}, err
		}
	}
	result.Success = true
	result.ErrorCode = 0
	result.EnchantUpgradeCount = upgradeCount
	result.MainRefresh = cloneItemMap(record.Slots)
	result.Changed = true
	return result, nil
}

func enchantPetEntryForTarget(record dnfrepo.PetRecord, target dnfrepo.ItemStack, slot int16) (string, dnfrepo.PetEntry, bool) {
	serial := firstExtraInt(target.Extra, 0,
		"creature_serial_or_handle", "creature_key", "creature_serial", "pet_serial", "serial", "handle", "instance_value", "item_uid")
	if serial <= 0 || serial > math.MaxUint32 {
		return "", dnfrepo.PetEntry{}, false
	}
	for key, entry := range record.Entries {
		entrySerial := uint64(entry.CreatureKey)
		if entrySerial == 0 {
			parsed, err := strconv.ParseUint(strings.TrimSpace(entry.PetKey), 0, 32)
			if err != nil || parsed == 0 {
				parsed, err = strconv.ParseUint(strings.TrimSpace(key), 0, 32)
			}
			if err == nil {
				entrySerial = parsed
			}
		}
		if entrySerial != uint64(serial) || entry.ItemID != target.ItemID ||
			entry.SourceListType != listTypePet || entry.SourceSlotIndex != slot {
			continue
		}
		return key, entry, true
	}
	return "", dnfrepo.PetEntry{}, false
}

func enchantEquippedPetEntryForTarget(record dnfrepo.PetRecord, target dnfrepo.ItemStack, slot int16) (string, dnfrepo.PetEntry, bool) {
	serial := firstExtraInt(target.Extra, 0,
		"creature_serial_or_handle", "creature_key", "creature_serial", "pet_serial", "serial", "handle", "instance_value", "item_uid")
	if serial <= 0 || serial > math.MaxUint32 || slot != 26 {
		return "", dnfrepo.PetEntry{}, false
	}
	for key, entry := range record.Entries {
		entrySerial := uint64(entry.CreatureKey)
		if entrySerial == 0 {
			parsed, err := strconv.ParseUint(strings.TrimSpace(entry.PetKey), 0, 32)
			if err != nil || parsed == 0 {
				parsed, err = strconv.ParseUint(strings.TrimSpace(key), 0, 32)
			}
			if err == nil {
				entrySerial = parsed
			}
		}
		if entrySerial == uint64(serial) && entry.ItemID == target.ItemID &&
			entry.SourceListType == listTypeEquipment && entry.SourceSlotIndex == slot {
			return key, entry, true
		}
	}
	return "", dnfrepo.PetEntry{}, false
}

func equippedCreatureStack(entry dnfrepo.EquipmentEntry) dnfrepo.ItemStack {
	extra := cloneEnchantExtra(entry.Extra)
	if extra == nil {
		extra = make(map[string]string, 4)
	}
	if len(entry.RawEntry) >= 28 {
		serial := binary.LittleEndian.Uint32(entry.RawEntry[24:28])
		if serial != 0 {
			extra["creature_serial_or_handle"] = strconv.FormatUint(uint64(serial), 10)
		}
	}
	extra["item_kind"] = "equipment"
	extra["equipment_type"] = "creature"
	return dnfrepo.ItemStack{ItemID: entry.ItemID, Count: 1, Bind: entry.Bind, ExpireAt: entry.ExpireAt, RawEntry: append([]byte(nil), entry.RawEntry...), Extra: extra}
}

func cloneEnchantExtra(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in)+4)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func setEquippedCreatureEnchantFields(entry *dnfrepo.EquipmentEntry, cardItemID int64, upgradeCount byte) {
	if entry == nil || cardItemID < 0 || cardItemID > math.MaxUint32 {
		return
	}
	if entry.Extra == nil {
		entry.Extra = make(map[string]string, 8)
	}
	value := strconv.FormatInt(cardItemID, 10)
	entry.Extra["value_a"] = value
	entry.Extra["enchant_card_id"] = value
	entry.Extra["pet_enchant_card_item_id"] = value
	entry.Extra["byte_12"] = strconv.Itoa(int(upgradeCount))
	entry.Extra["enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
	entry.Extra["pet_enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
	if len(entry.RawEntry) >= 0x15 {
		entry.RawEntry = append([]byte(nil), entry.RawEntry...)
		binary.LittleEndian.PutUint32(entry.RawEntry[0x10:0x14], uint32(cardItemID))
		entry.RawEntry[0x14] = upgradeCount
	}
}

func setPetEntryEnchantFields(entry *dnfrepo.PetEntry, cardItemID int64, upgradeCount byte) {
	if entry == nil {
		return
	}
	if entry.Extra == nil {
		entry.Extra = make(map[string]string, 6)
	}
	entry.Extra["value_a"] = strconv.FormatInt(cardItemID, 10)
	entry.Extra["enchant_card_id"] = strconv.FormatInt(cardItemID, 10)
	entry.Extra["pet_enchant_card_item_id"] = strconv.FormatInt(cardItemID, 10)
	entry.Extra["byte_12"] = strconv.Itoa(int(upgradeCount))
	entry.Extra["enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
	entry.Extra["pet_enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
	if len(entry.RawEntry) == currentItemListEntrySize {
		entry.RawEntry = append([]byte(nil), entry.RawEntry...)
		binary.LittleEndian.PutUint32(entry.RawEntry[0x0E:0x12], uint32(cardItemID))
		entry.RawEntry[0x12] = upgradeCount
	}
}

// enchantUpgradeCountOf reads the bead's own dynamic upgrade count, which is
// carried in the same common-entry +0x12 byte that receives it on the target.
func enchantUpgradeCountOf(stack dnfrepo.ItemStack) byte {
	if value := firstExtraInt(stack.Extra, 0, "byte_12", "value_12", "enchant_upgrade_count"); value != 0 {
		return byte(value)
	}
	if raw, ok := currentRawEntry(stack); ok {
		return raw[0x12]
	}
	return 0
}

// setEnchantFields writes the enchant card id (+0x0E i32) and upgrade count
// (+0x12 u8) through the Extra/RawEntry dual-write contract used by the
// current-EXE 0x77 item entry serializer.
func setEnchantFields(stack *dnfrepo.ItemStack, cardItemID int64, upgradeCount byte) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
	}
	stack.Extra["value_a"] = strconv.FormatInt(cardItemID, 10)
	stack.Extra["enchant_card_id"] = strconv.FormatInt(cardItemID, 10)
	stack.Extra["byte_12"] = strconv.Itoa(int(upgradeCount))
	stack.Extra["enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		value := uint32(cardItemID)
		stack.RawEntry[0x0E] = byte(value)
		stack.RawEntry[0x0F] = byte(value >> 8)
		stack.RawEntry[0x10] = byte(value >> 16)
		stack.RawEntry[0x11] = byte(value >> 24)
		stack.RawEntry[0x12] = upgradeCount
	}
}

func setPetStackEnchantFields(stack *dnfrepo.ItemStack, cardItemID int64, upgradeCount byte) {
	if stack == nil || stack.Extra == nil || cardItemID < 0 || cardItemID > math.MaxUint32 {
		return
	}
	value := strconv.FormatInt(cardItemID, 10)
	stack.Extra["pet_enchant_card_item_id"] = value
	stack.Extra["pet_enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount))
}

func setRandomTitleEnchantFields(stack *dnfrepo.ItemStack, configID, rowID uint16) {
	if stack == nil || configID == 0 || rowID == 0 {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 8)
	}
	stack.Extra["value_a"] = "0"
	stack.Extra["enchant_card_id"] = "0"
	stack.Extra["byte_12"] = "0"
	stack.Extra["enchant_upgrade_count"] = "0"
	stack.Extra["random_title_enchant_config_id"] = strconv.FormatUint(uint64(configID), 10)
	stack.Extra["random_title_enchant_row_id"] = strconv.FormatUint(uint64(rowID), 10)
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		for index := 0; index < 4; index++ {
			stack.RawEntry[0x0E+index] = 0
		}
		stack.RawEntry[0x12] = 0
		binary.LittleEndian.PutUint16(stack.RawEntry[0x61:0x63], configID)
		binary.LittleEndian.PutUint16(stack.RawEntry[0x63:0x65], rowID)
	}
}

func int64SliceContains(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func stringSliceContainsFold(values []string, needle string) bool {
	trimmed := strings.TrimSpace(needle)
	if trimmed == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), trimmed) {
			return true
		}
	}
	return false
}

// UseStackable consumes one repository-backed item through the current op44
// contract. Main-inventory items remain PVF-proven [waste]; list-7 pet feed is
// handled by its own PVF-proven, character-pet transaction below.
// When premiumResolver proves the item is a premiumlist_new.etc contract item
// ([etc] family, never [waste]), the use activates the account-level premium
// contract instead of the waste path, inside one rental-asset transaction
// (item decrement + account premium upsert).
func (o *Owner) UseStackable(ctx context.Context, cmd Command, premiumResolver alignedcmd.PremiumContractResolver) (UseStackableResult, error) {
	return o.useStackableWithResolvers(ctx, cmd, premiumResolver, nil, nil, nil, nil, nil, nil, nil)
}

func (o *Owner) UseStackableWithResolvers(
	ctx context.Context,
	cmd Command,
	premiumResolver alignedcmd.PremiumContractResolver,
	experienceResolver alignedcmd.ExperienceConsumableResolver,
	stackableResolver ...alignedcmd.StackableUseResolver,
) (UseStackableResult, error) {
	return o.useStackableWithResolvers(ctx, cmd, premiumResolver, experienceResolver, nil, nil, nil, nil, nil, nil, stackableResolver...)
}

func (o *Owner) UseStackableWithAllResolvers(
	ctx context.Context,
	cmd Command,
	premiumResolver alignedcmd.PremiumContractResolver,
	experienceResolver alignedcmd.ExperienceConsumableResolver,
	stackableResolver alignedcmd.StackableUseResolver,
	questTicketPlanner alignedcmd.QuestCompletionTicketPlanner,
	directExperiencePlanner alignedcmd.DirectExperienceConsumablePlanner,
	storyBookPlanner ...alignedcmd.StoryBookPlanner,
) (UseStackableResult, error) {
	var planner alignedcmd.StoryBookPlanner
	if len(storyBookPlanner) > 0 {
		planner = storyBookPlanner[0]
	}
	return o.useStackableWithResolvers(ctx, cmd, premiumResolver, experienceResolver, questTicketPlanner, directExperiencePlanner, planner, nil, nil, nil, stackableResolver)
}

// UseStackableWithAllResolversAndSkillCatalog adds the immutable active-PVF
// skill catalog needed only by TP reset books. Other consumables retain the
// existing entry point and do not depend on skill data.
func (o *Owner) UseStackableWithAllResolversAndSkillCatalog(
	ctx context.Context,
	cmd Command,
	premiumResolver alignedcmd.PremiumContractResolver,
	experienceResolver alignedcmd.ExperienceConsumableResolver,
	stackableResolver alignedcmd.StackableUseResolver,
	questTicketPlanner alignedcmd.QuestCompletionTicketPlanner,
	directExperiencePlanner alignedcmd.DirectExperienceConsumablePlanner,
	storyBookPlanner alignedcmd.StoryBookPlanner,
	skillCatalog *dnfskill.Table,
	initialSkillLevels map[uint16]int,
	skillPointBaseline *dnfrepo.SkillPointState,
) (UseStackableResult, error) {
	return o.useStackableWithResolvers(
		ctx,
		cmd,
		premiumResolver,
		experienceResolver,
		questTicketPlanner,
		directExperiencePlanner,
		storyBookPlanner,
		skillCatalog,
		initialSkillLevels,
		skillPointBaseline,
		stackableResolver,
	)
}

// UseEventServerLevelUp executes the current NoPack op418 contract. The wire
// request intentionally carries no item identity, so the durable source row is
// loaded first, resolved through the active PVF, and then checked again inside
// useLevelUpTicket's character settlement before anything is consumed.
func (o *Owner) UseEventServerLevelUp(
	ctx context.Context,
	cmd Command,
	stackableResolver alignedcmd.StackableUseResolver,
	directExperiencePlanner alignedcmd.DirectExperienceConsumablePlanner,
	questTicketPlanner alignedcmd.QuestCompletionTicketPlanner,
	inDungeon bool,
) (UseStackableResult, error) {
	if o == nil || o.repo == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.Operation != "event_server_level_up" || cmd.SelectedCharacterID == 0 ||
		cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 ||
		cmd.SourceInstanceValue != 0 || cmd.ItemTemplateID != 0 || cmd.ReservedValue != 0 ||
		stackableResolver == nil || directExperiencePlanner == nil || questTicketPlanner == nil {
		return UseStackableResult{}, ErrLevelUpTicketContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if stack.ItemID <= 0 || stack.ItemID > math.MaxInt32 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrLevelUpTicketContract, stack.ItemID)
	}
	resolution, err := stackableResolver(stack.ItemID)
	if err != nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrQuestCompletionTicketResolveFailed, stack.ItemID, err)
	}
	if resolution.ItemID != stack.ItemID || resolution.DirectExperience == nil ||
		resolution.QuestCompletionTicket == nil ||
		!strings.EqualFold(strings.TrimSpace(resolution.DirectExperience.EffectType), "[level up ticket]") {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d is not a PVF level-up ticket", ErrLevelUpTicketContract, stack.ItemID)
	}
	if inDungeon && resolution.QuestCompletionTicket.VillageOnly {
		return UseStackableResult{}, ErrQuestCompletionTicketContract
	}
	cmd.ItemTemplateID = int32(stack.ItemID)
	return o.useLevelUpTicket(ctx, cmd, resolution, directExperiencePlanner, questTicketPlanner)
}

// UseIncreaseStatusBook executes the current NoPack op30 contract. Like op418
// the wire request carries no item identity at all, so the durable source row
// is loaded first, resolved through the active PVF, and then checked again
// inside the settlement of whichever owner the resolution selects. Only the two
// families the client actually sends op30 for are admitted: the [etc] SP/TP
// point books and the immediate [increase status type] experience books. A
// level-up ticket resolves through the same PVF fields but belongs to op418 and
// is refused here so its quest settlement cannot run without its own route.
func (o *Owner) UseIncreaseStatusBook(
	ctx context.Context,
	cmd Command,
	stackableResolver alignedcmd.StackableUseResolver,
	directExperiencePlanner alignedcmd.DirectExperienceConsumablePlanner,
	skillCatalog *dnfskill.Table,
	initialSkillLevels map[uint16]int,
	skillPointBaseline *dnfrepo.SkillPointState,
) (UseStackableResult, error) {
	if o == nil || o.repo == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.Operation != "increase_status_book" || cmd.SelectedCharacterID == 0 ||
		cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 ||
		cmd.SourceInstanceValue != 0 || cmd.ItemTemplateID != 0 || cmd.ReservedValue != 0 ||
		stackableResolver == nil {
		return UseStackableResult{}, ErrIncreaseStatusBookContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if stack.ItemID <= 0 || stack.ItemID > math.MaxInt32 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrIncreaseStatusBookContract, stack.ItemID)
	}
	resolution, err := stackableResolver(stack.ItemID)
	if err != nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrIncreaseStatusBookContract, stack.ItemID, err)
	}
	if resolution.ItemID != stack.ItemID {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolved as %d", ErrIncreaseStatusBookContract, stack.ItemID, resolution.ItemID)
	}
	cmd.ItemTemplateID = int32(stack.ItemID)
	if resolution.TPBook != nil {
		return o.useTPBook(ctx, cmd, resolution, skillCatalog, initialSkillLevels, skillPointBaseline)
	}
	if resolution.DirectExperience != nil &&
		!strings.EqualFold(strings.TrimSpace(resolution.DirectExperience.EffectType), "[level up ticket]") {
		if directExperiencePlanner == nil {
			return UseStackableResult{}, ErrIncreaseStatusBookContract
		}
		return o.useDirectExperienceConsumable(ctx, cmd, resolution, directExperiencePlanner)
	}
	return UseStackableResult{}, fmt.Errorf(
		"%w: item=%d path=%q is neither an SP/TP point book nor an immediate experience book",
		ErrIncreaseStatusBookContract, stack.ItemID, resolution.PVFPath)
}

// ---------------------------------------------------------------------------
// 每天可用次数 (PVF [total usable count]) 的通用预留与回滚。
//
// The client paints its 每天可用次数 line from op541 alone, so an item the
// server never counts renders 0/N forever and is never refused. The counter is
// one durable stat row per item, sharing the 06:00 Beijing reset of the
// fatigue counters, and the slot is reserved before the use commits so a
// rejected use can never burn part of the allowance.
// ---------------------------------------------------------------------------

// ErrDailyItemUseLimit marks the "[total usable count] reached for today"
// rejection so callers can answer the ordinary failure without consuming.
//
// 计数与判定本身已经收进 itemdailylimit：那是所有消费路径（op44、op515、开箱、
// 礼包）共用的一份实现。这里的两个名字保留成别名，既有调用点与 errors.Is/As
// 判断一字不改，拒绝文本也保持原样。
var ErrDailyItemUseLimit = dnfitemdailylimit.ErrLimitReached

// DailyItemUseLimitError reports the numbers behind that rejection.
type DailyItemUseLimitError = dnfitemdailylimit.LimitReachedError

// DailyItemUseReservation is one reserved 每天可用次数 slot. Reserved stays
// false when the item declares no cap, in which case nothing was read or
// written.
type DailyItemUseReservation struct {
	ItemID   int64
	Used     int64
	Limit    int64
	Reserved bool
}

func dailyItemUseStatKey(itemID int64) string {
	return dnfitemdailylimit.Key(itemID)
}

// dailyItemUseUsed reads today's used count, honouring the legacy
// fatigue_potion_used_* spelling so counters written before the generic key
// existed are not lost within the same day.
func dailyItemUseUsed(stats map[string]int64, itemID int64) int64 {
	return dnfitemdailylimit.Used(stats, itemID)
}

func dailyItemUseTime(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// ReserveDailyItemUse consumes one daily slot for itemID. A limit of zero or
// less means the item has no [total usable count]: the zero reservation is
// returned without touching the database.
func (o *Owner) ReserveDailyItemUse(
	ctx context.Context,
	characterID string,
	itemID int64,
	limit int64,
	now time.Time,
) (DailyItemUseReservation, error) {
	if o == nil || o.assets == nil || limit <= 0 || itemID <= 0 {
		return DailyItemUseReservation{}, nil
	}
	now = dailyItemUseTime(now)
	reservation := DailyItemUseReservation{ItemID: itemID, Limit: limit}
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 4)
		}
		// Consume 内部先走 06:00 服务日边界，再判定并自增，与这里原来手写的那三行
		// 完全同序同键；上限<=0 时它是空操作（PVF 没声明就不读不写）。
		used, consumeErr := dnfitemdailylimit.Consume(character.Stats, itemID, limit, now, character.Level)
		if consumeErr != nil {
			return consumeErr
		}
		character.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		reservation.Used = used
		reservation.Reserved = true
		return nil
	})
	if err != nil {
		return DailyItemUseReservation{}, err
	}
	return reservation, nil
}

// ReleaseDailyItemUse returns a reserved slot when the actual use failed, so a
// rejected command never burns part of the player's daily allowance.
func (o *Owner) ReleaseDailyItemUse(
	ctx context.Context,
	characterID string,
	reservation DailyItemUseReservation,
	now time.Time,
) error {
	if o == nil || o.assets == nil || !reservation.Reserved || reservation.ItemID <= 0 {
		return nil
	}
	now = dailyItemUseTime(now)
	return o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			return nil
		}
		key := dailyItemUseStatKey(reservation.ItemID)
		used := character.Stats[key]
		if used <= 0 {
			return nil
		}
		dnfitemdailylimit.Release(character.Stats, reservation.ItemID)
		character.UpdatedAt = now
		return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
	})
}

// ---------------------------------------------------------------------------
// 冷却（PVF [cool time] + [cooltime group] + [cooltime maintenance]）的持久预留。
//
// 客户端的冷却门只在进程内内存里（cmd/scratch/potionpatch 已记："no packet is sent
// while the client blocks the action"），所以小退重上就没了。声明了
// [cooltime maintenance] 的道具必须由服务端落一份到期时间：用过就在窗口内拒绝再次使用，
// 并在选角/回城时按类 0 通知 172 把仍在冷却的道具重放给客户端。
// ---------------------------------------------------------------------------

// ErrItemCooltimeActive marks the "still cooling down" rejection so callers can
// answer the ordinary failure without consuming the item.
var ErrItemCooltimeActive = errors.New("item cooltime is still active")

// ItemCooltimeReservation is one reserved cooldown window. Reserved stays false
// when the item declares no maintained cooldown, in which case nothing was read
// or written.
type ItemCooltimeReservation struct {
	ItemID   int64
	ExpireAt int64
	// PreviousExpireAt/HadPrevious let a failed use restore exactly what was
	// there before, instead of dropping an older running window.
	PreviousExpireAt int64
	HadPrevious      bool
	Reserved         bool
}

// ReserveItemCooltime checks itemID's cooldown window and, when it is free,
// writes the new expiry in the same transaction. seconds<=0 or itemID<=0 means
// the item declares no maintained cooldown: nothing is read or written.
//
// groupOf resolves the PVF cooltime group of an already-stored item id so two
// items of the same group share one window (PVF `[cooltime group]`); it may be
// nil when the caller cannot resolve groups.
func (o *Owner) ReserveItemCooltime(
	ctx context.Context,
	characterID string,
	itemID int64,
	seconds int64,
	group int64,
	groupOf func(int64) int64,
	now time.Time,
) (ItemCooltimeReservation, error) {
	if o == nil || o.assets == nil || itemID <= 0 || seconds <= 0 {
		return ItemCooltimeReservation{}, nil
	}
	now = dailyItemUseTime(now)
	reservation := ItemCooltimeReservation{ItemID: itemID}
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 4)
		}
		nowUnix := now.Unix()
		if blocked, active := dnfitemcooldown.Blocks(character.Stats, nowUnix, itemID, group, groupOf); active {
			return fmt.Errorf("%w: item=%d blocked_by=%d until=%d", ErrItemCooltimeActive, itemID, blocked.ItemID, blocked.ExpireAt)
		}
		if previous, found := dnfitemcooldown.ExpireAt(character.Stats, itemID); found {
			reservation.PreviousExpireAt = previous
			reservation.HadPrevious = true
		}
		dnfitemcooldown.Prune(character.Stats, nowUnix)
		reservation.ExpireAt = nowUnix + seconds
		dnfitemcooldown.Set(character.Stats, itemID, reservation.ExpireAt)
		character.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		reservation.Reserved = true
		return nil
	})
	if err != nil {
		return ItemCooltimeReservation{}, err
	}
	return reservation, nil
}

// ReleaseItemCooltime restores the pre-use window after a failed use, so a
// rejected command never starts a cooldown the player did not get to spend.
func (o *Owner) ReleaseItemCooltime(
	ctx context.Context,
	characterID string,
	reservation ItemCooltimeReservation,
	now time.Time,
) error {
	if o == nil || o.assets == nil || !reservation.Reserved || reservation.ItemID <= 0 {
		return nil
	}
	now = dailyItemUseTime(now)
	return o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			return nil
		}
		if reservation.HadPrevious {
			dnfitemcooldown.Set(character.Stats, reservation.ItemID, reservation.PreviousExpireAt)
		} else {
			dnfitemcooldown.Clear(character.Stats, reservation.ItemID)
		}
		character.UpdatedAt = now
		return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
	})
}

func (o *Owner) useStackableWithResolvers(
	ctx context.Context,
	cmd Command,
	premiumResolver alignedcmd.PremiumContractResolver,
	experienceResolver alignedcmd.ExperienceConsumableResolver,
	questTicketPlanner alignedcmd.QuestCompletionTicketPlanner,
	directExperiencePlanner alignedcmd.DirectExperienceConsumablePlanner,
	storyBookPlanner alignedcmd.StoryBookPlanner,
	skillCatalog *dnfskill.Table,
	initialSkillLevels map[uint16]int,
	skillPointBaseline *dnfrepo.SkillPointState,
	stackableResolver ...alignedcmd.StackableUseResolver,
) (UseStackableResult, error) {
	if o == nil || o.repo == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UseStackableResult{}, ErrCharacterRequired
	}
	if err := checkUseStackableRequestListType(cmd.SourceListType); err != nil {
		return UseStackableResult{}, err
	}
	if cmd.SourceInstanceValue == 0 || cmd.ItemTemplateID <= 0 || cmd.ReservedValue != 0 {
		return UseStackableResult{}, fmt.Errorf(
			"%w: instance=0x%08X item=%d reserved=0x%08X",
			ErrUseStackableContractRequired,
			uint32(cmd.SourceInstanceValue),
			cmd.ItemTemplateID,
			cmd.ReservedValue,
		)
	}
	if cmd.SourceListType == listTypePet {
		if petInventorySlotSegment(cmd.SourceSlotIndex) != petInventorySegmentConsumable {
			return UseStackableResult{}, fmt.Errorf(
				"%w: list=%d slot=%d is outside pet-consumable slots 189..238",
				ErrPetFeedContractRequired,
				cmd.SourceListType,
				cmd.SourceSlotIndex,
			)
		}
		if len(stackableResolver) == 0 || stackableResolver[0] == nil {
			return UseStackableResult{}, ErrPetFeedResolveFailed
		}
		resolution, err := stackableResolver[0](int64(cmd.ItemTemplateID))
		if err != nil {
			return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrPetFeedResolveFailed, cmd.ItemTemplateID, err)
		}
		return o.usePetFeed(ctx, cmd, resolution)
	}
	var stackableResolution alignedcmd.StackableUseResolution
	if len(stackableResolver) > 0 && stackableResolver[0] != nil {
		var resolveErr error
		stackableResolution, resolveErr = stackableResolver[0](int64(cmd.ItemTemplateID))
		if resolveErr != nil {
			return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrQuestCompletionTicketResolveFailed, cmd.ItemTemplateID, resolveErr)
		}
		if stackableResolution.DirectExperience != nil && stackableResolution.QuestCompletionTicket != nil &&
			strings.EqualFold(strings.TrimSpace(stackableResolution.DirectExperience.EffectType), "[level up ticket]") {
			return o.useLevelUpTicket(ctx, cmd, stackableResolution, directExperiencePlanner, questTicketPlanner)
		}
		if stackableResolution.QuestCompletionTicket != nil {
			return o.useQuestCompletionTicket(ctx, cmd, stackableResolution, questTicketPlanner)
		}
		if stackableResolution.StoryBook != nil {
			return o.useStoryBook(ctx, cmd, stackableResolution, storyBookPlanner)
		}
		if stackableResolution.DirectExperience != nil {
			return o.useDirectExperienceConsumable(ctx, cmd, stackableResolution, directExperiencePlanner)
		}
		if stackableResolution.TPBook != nil {
			return o.useTPBook(ctx, cmd, stackableResolution, skillCatalog, initialSkillLevels, skillPointBaseline)
		}
		if stackableResolution.DungeonAdmissionTicket != nil {
			return o.useDungeonAdmissionTicket(ctx, cmd, stackableResolution)
		}
		if stackableResolution.Transformation != nil {
			if err := validateTransformationUse(cmd, *stackableResolution.Transformation); err != nil {
				return UseStackableResult{}, err
			}
		}
		if stackableResolution.Projectile != nil {
			if err := validateProjectileUse(cmd, *stackableResolution.Projectile); err != nil {
				return UseStackableResult{}, err
			}
		}
		if stackableResolution.AvatarFittingRoomTicket != nil {
			return o.openAvatarFittingRoom(ctx, cmd, stackableResolution)
		}
		if stackableResolution.CustomAbilityGroupConverter != nil {
			return o.openCustomAbilityGroupConverter(ctx, cmd, stackableResolution)
		}
		// 塔罗牌（PVF `[monster tarot card]`）：金币与道具必须同笔，所以它走
		// withinCharacterAssetTransaction，而不是上面这些只有背包的路径。
		if stackableResolution.TarotLottery != nil {
			return o.UseTarotLotteryCard(ctx, cmd, *stackableResolution.TarotLottery)
		}
		// 城镇自施放增益药剂（史诗Buff药剂一族）：背包减 1 与角色增益窗口必须同笔，
		// 所以它也走账号+角色+背包同一条事务（见 stat_change_potion.go）。
		if stackableResolution.StatChangePotion != nil {
			return o.useStatChangePotion(ctx, cmd, stackableResolution)
		}
	}

	if premiumResolver != nil {
		resolution, err := premiumResolver(int64(cmd.ItemTemplateID))
		if err != nil {
			return UseStackableResult{}, fmt.Errorf(
				"%w: item=%d: %v",
				ErrPremiumContractResolveFailed,
				cmd.ItemTemplateID,
				err,
			)
		}
		if resolution.PremiumType > 0 {
			return o.usePremiumContract(ctx, cmd, resolution)
		}
	}
	if experienceResolver != nil {
		resolution, err := experienceResolver(int64(cmd.ItemTemplateID))
		if err != nil {
			return UseStackableResult{}, fmt.Errorf(
				"%w: item=%d: %v",
				ErrExperienceConsumableResolveFailed,
				cmd.ItemTemplateID,
				err,
			)
		}
		if resolution.BonusPercent > 0 {
			return o.useExperienceConsumable(ctx, cmd, resolution)
		}
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result UseStackableResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.useStackableWithResolvers(ctx, cmd, premiumResolver, nil, questTicketPlanner, directExperiencePlanner, storyBookPlanner, skillCatalog, initialSkillLevels, skillPointBaseline, stackableResolver...)
			return err
		})
		return result, err
	}

	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !ok {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	if record.Slots == nil {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	stack, ok := record.Slots[key]
	if !ok || stack.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if stack.ItemID != int64(cmd.ItemTemplateID) {
		return UseStackableResult{}, fmt.Errorf(
			"%w: list=%d slot=%d want=%d got=%d",
			ErrItemMismatch,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			cmd.ItemTemplateID,
			stack.ItemID,
		)
	}
	if stackableResolution.UsePlace != nil {
		if err := validateStackableUsePlace(cmd, *stackableResolution.UsePlace); err != nil {
			return UseStackableResult{}, err
		}
	}
	pvfPath, stackableType, ok := pvfProvenWasteStack(stack)
	if !ok && len(stackableResolver) > 0 && stackableResolver[0] != nil {
		resolution := stackableResolution
		var resolveErr error
		if resolution.ItemID == 0 {
			resolution, resolveErr = stackableResolver[0](stack.ItemID)
		}
		durablePVFPath := strings.TrimSpace(stack.Extra["pvf_path"])
		resolvedType := strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", ""))
		transformationAuthorized := resolution.Transformation != nil && validateTransformationUse(cmd, *resolution.Transformation) == nil
		projectileAuthorized := resolution.Projectile != nil && validateProjectileUse(cmd, *resolution.Projectile) == nil
		dynamicDungeonWaste := resolution.UsePlace != nil &&
			len(resolution.UsePlace.ActionUsableDungeonIDs) > 0 &&
			validateStackableUsePlace(cmd, *resolution.UsePlace) == nil
		pathAuthorized := durablePVFPath != "" && strings.EqualFold(durablePVFPath, strings.TrimSpace(resolution.PVFPath))
		if resolveErr == nil && resolution.ItemID == stack.ItemID && (pathAuthorized || dynamicDungeonWaste) &&
			(strings.HasPrefix(strings.ToLower(resolvedType), "[waste]") || transformationAuthorized || projectileAuthorized) {
			pvfPath, stackableType, ok = strings.TrimSpace(resolution.PVFPath), resolvedType, true
		}
	}
	if !ok {
		return UseStackableResult{}, fmt.Errorf(
			"%w: list=%d slot=%d item=%d",
			ErrUseStackableContractRequired,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			stack.ItemID,
		)
	}
	reviveCoinConsumable := dnfrevivecoin.IsConsumable(stack)

	// 修炼道场 (5000) is a sandbox: the consumable is usable - the caller still
	// computes and sends the recovery frame from this result - but the stack must
	// stay untouched, so nothing is deducted and the ack carries the unchanged
	// count. Every durable side effect below is skipped for that run.
	remaining := stack.Count
	if !cmd.ConsumableFree {
		remaining = stack.Count - 1
		if remaining == 0 {
			delete(record.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			updateStackRawAmount(&stack)
			record.Slots[key] = stack
		}
	}
	var reviveCoinWalletTotal int64
	if reviveCoinConsumable && !cmd.ConsumableFree {
		reviveCoinWalletTotal, err = dnfrevivecoin.Grant(
			&record,
			1,
			"current_exe_op44_coin_general",
		)
		if err != nil {
			return UseStackableResult{}, err
		}
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return UseStackableResult{}, err
	}
	result := UseStackableResult{
		CharacterID:    characterID,
		AccountID:      cmd.AccountID,
		ListType:       cmd.SourceListType,
		SlotIndex:      cmd.SourceSlotIndex,
		InstanceValue:  cmd.SourceInstanceValue,
		ItemID:         stack.ItemID,
		RemainingCount: remaining,
		// A sandbox run keeps the stack count, but the caller must still publish
		// the authoritative item row: the client adjusts its own stack on use and
		// without the row the cell stays empty until the next inventory reload
		// (live 2026-09-12: the item vanished until the player relogged).
		Changed:                 true,
		PVFPath:                 pvfPath,
		StackableType:           stackableType,
		ReviveCoinWalletUpdated: reviveCoinConsumable,
		ReviveCoinWalletTotal:   reviveCoinWalletTotal,
	}
	if stackableResolution.Transformation != nil {
		result.TransformationApplied = true
		result.TransformationAction = stackableResolution.Transformation.ActionType
	}
	if stackableResolution.Projectile != nil {
		result.ProjectileApplied = true
		result.ProjectileEffectID = stackableResolution.Projectile.EffectID
	}
	return result, nil
}

func (o *Owner) useDungeonAdmissionTicket(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	ticket := resolution.DungeonAdmissionTicket
	actionPacketMatches := (cmd.Operation == "use_stackable" && ticket != nil && ticket.ActionPacket == 0) ||
		(cmd.Operation == "use_stackable_action" && ticket != nil && ticket.ActionPacket == cmd.ActionIndex && cmd.ActionIndex == dungeonAdmissionTicketActionIndex)
	if o == nil || o.dungeonAdmission == nil || ticket == nil ||
		cmd.SourceListType != listTypeMain || cmd.InDungeon ||
		resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" ||
		!strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")), "[waste]") ||
		strings.TrimSpace(ticket.ActionType) == "" || len(ticket.Grants) == 0 ||
		(!ticket.VillageAllowed && !ticket.SeriaRoomAllowed) || !actionPacketMatches {
		return UseStackableResult{}, ErrDungeonAdmissionTicketContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	now := time.Now().UTC()
	if o.now != nil {
		now = o.now().UTC()
	}
	var remaining int64
	allowance, err := o.dungeonAdmission.GrantAllowances(
		ctx,
		characterID,
		ticket.Grants,
		ticket.UseLimit,
		now,
		func(tx dnfrepo.Group) error {
			if tx.Character == nil || tx.Inventory == nil {
				return ErrDungeonAdmissionTicketContract
			}
			character, found, loadErr := tx.Character.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !found || strings.TrimSpace(character.CharacterID) != characterID ||
				(strings.TrimSpace(cmd.AccountID) != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID)) ||
				(ticket.MinimumLevel > 0 && int64(character.Level) < ticket.MinimumLevel) ||
				(ticket.MaximumLevel > 0 && int64(character.Level) > ticket.MaximumLevel) {
				return ErrDungeonAdmissionTicketContract
			}
			inventory, found, loadErr := tx.Inventory.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !found {
				return ErrInventoryNotFound
			}
			inventory = dnfrepo.CloneInventory(inventory)
			key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
			stack, found := inventory.Slots[key]
			if !found || stack.Count <= 0 {
				return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
			}
			if stack.ItemID != resolution.ItemID {
				return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
			}
			durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
			durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
			durableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", "")))
			if durablePath == "" || !strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
				(durableKind != "" && durableKind != "stackable") ||
				(durableType != "" && durableType != "[waste]") {
				return fmt.Errorf("%w: item=%d durable_path=%q durable_kind=%q durable_type=%q", ErrDungeonAdmissionTicketContract, stack.ItemID, durablePath, durableKind, durableType)
			}
			remaining = stack.Count - 1
			if remaining == 0 {
				delete(inventory.Slots, key)
			} else {
				stack = cloneStack(stack)
				stack.Count = remaining
				updateStackRawAmount(&stack)
				inventory.Slots[key] = stack
			}
			inventory.UpdatedAt = now
			return dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots)
		},
	)
	if err != nil {
		return UseStackableResult{}, err
	}
	bonus := uint32(0)
	if len(ticket.Grants) > 0 {
		bonus = ticket.Grants[0].Amount
	}
	return UseStackableResult{
		CharacterID: characterID, AccountID: cmd.AccountID, ListType: cmd.SourceListType,
		SlotIndex: cmd.SourceSlotIndex, InstanceValue: cmd.SourceInstanceValue,
		ItemID: resolution.ItemID, RemainingCount: remaining, Changed: true,
		PVFPath: resolution.PVFPath, StackableType: resolution.StackableType,
		DungeonAdmissionTicketApplied: true, DungeonAdmissionAction: ticket.ActionType,
		DungeonAdmissionPolicyCount: len(allowance.Snapshots), DungeonAdmissionBonus: bonus,
		DungeonAdmissionRemainingUses: allowance.RemainingUses,
	}, nil
}

const dungeonAdmissionTicketActionIndex uint32 = 1

// UseDungeonAdmissionTicketAction handles the current op515/action-1 family.
// The request contains only a dynamic source slot, so item identity is loaded
// from storage, resolved through the active PVF, and revalidated again inside
// the shared allowance+inventory settlement.
func (o *Owner) UseDungeonAdmissionTicketAction(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	inDungeon bool,
) (UseStackableResult, error) {
	if o == nil || o.repo == nil || resolver == nil || cmd.SelectedCharacterID == 0 ||
		cmd.Operation != "use_stackable_action" || cmd.ActionIndex != dungeonAdmissionTicketActionIndex ||
		cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 || inDungeon {
		return UseStackableResult{}, ErrDungeonAdmissionTicketContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 || stack.ItemID <= 0 || stack.ItemID > math.MaxInt32 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	resolution, err := resolver(stack.ItemID)
	if err != nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrDungeonAdmissionTicketContract, stack.ItemID, err)
	}
	if resolution.ItemID != stack.ItemID || resolution.DungeonAdmissionTicket == nil ||
		resolution.DungeonAdmissionTicket.ActionPacket != dungeonAdmissionTicketActionIndex {
		return UseStackableResult{}, ErrDungeonAdmissionTicketContract
	}
	cmd.ItemTemplateID = int32(stack.ItemID)
	cmd.InDungeon = false
	return o.useDungeonAdmissionTicket(ctx, cmd, resolution)
}

// openCustomAbilityGroupConverter proves the exact source row without
// consuming it. The successful op44 opens the native window; op1340 later
// atomically updates the target and decrements one device, so cancellation is
// free and an op44 write failure cannot charge the player.
func (o *Owner) openCustomAbilityGroupConverter(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	converter := resolution.CustomAbilityGroupConverter
	if converter == nil || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" ||
		!strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")), "[etc]") ||
		cmd.SourceListType != listTypeMain {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if o.characters == nil {
		return UseStackableResult{}, ErrCharacterRequired
	}
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found || character.CharacterID != characterID ||
		(converter.MinimumLevel > 0 && int64(character.Level) < converter.MinimumLevel) ||
		(converter.MaximumLevel > 0 && int64(character.Level) > converter.MaximumLevel) {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 || stack.ItemID != resolution.ItemID {
		return UseStackableResult{}, ErrItemMismatch
	}
	durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
	durableType := strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", ""))
	if durablePath == "" || !strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
		(durableType != "" && !strings.EqualFold(durableType, "[etc]")) {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	return UseStackableResult{
		CharacterID:                       characterID,
		AccountID:                         cmd.AccountID,
		ListType:                          cmd.SourceListType,
		SlotIndex:                         cmd.SourceSlotIndex,
		InstanceValue:                     cmd.SourceInstanceValue,
		ItemID:                            stack.ItemID,
		RemainingCount:                    stack.Count,
		Changed:                           false,
		PVFPath:                           resolution.PVFPath,
		StackableType:                     "[etc]",
		CustomAbilityGroupConverterOpened: true,
	}, nil
}

// openAvatarFittingRoom proves the exact ticket row without consuming it.
// The current client first uses op44 to open the window and later submits the
// actual changed parts through op1488. Consuming here would double-charge one
// ticket before the server knows how many parts are being changed.
func (o *Owner) openAvatarFittingRoom(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	ticket := resolution.AvatarFittingRoomTicket
	if ticket == nil || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" ||
		!strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")), "[etc]") ||
		cmd.SourceListType != listTypeMain || cmd.InDungeon ||
		(!ticket.VillageAllowed && !ticket.SeriaAllowed) {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if o.characters == nil {
		return UseStackableResult{}, ErrCharacterRequired
	}
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found || character.CharacterID != characterID ||
		(ticket.MinimumLevel > 0 && int64(character.Level) < ticket.MinimumLevel) ||
		(ticket.MaximumLevel > 0 && int64(character.Level) > ticket.MaximumLevel) {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 || stack.ItemID != resolution.ItemID {
		return UseStackableResult{}, ErrItemMismatch
	}
	durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
	durableType := strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", ""))
	if durablePath == "" || !strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
		(durableType != "" && !strings.EqualFold(durableType, "[etc]")) {
		return UseStackableResult{}, ErrUseStackableContractRequired
	}
	return UseStackableResult{
		CharacterID:             characterID,
		AccountID:               cmd.AccountID,
		ListType:                cmd.SourceListType,
		SlotIndex:               cmd.SourceSlotIndex,
		InstanceValue:           cmd.SourceInstanceValue,
		ItemID:                  stack.ItemID,
		RemainingCount:          stack.Count,
		Changed:                 false,
		PVFPath:                 resolution.PVFPath,
		StackableType:           resolution.StackableType,
		AvatarFittingRoomOpened: true,
	}, nil
}

func validateTransformationUse(cmd Command, transformation alignedcmd.TransformationUseResolution) error {
	actionType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(transformation.ActionType, "`", "")))
	if actionType == "" || len(transformation.FormIDs) == 0 {
		return fmt.Errorf("%w: action=%q forms=%v", ErrTransformationContract, transformation.ActionType, transformation.FormIDs)
	}
	for _, formID := range transformation.FormIDs {
		if formID <= 0 {
			return fmt.Errorf("%w: action=%q invalid form=%d", ErrTransformationContract, transformation.ActionType, formID)
		}
	}
	if cmd.InDungeon {
		if !transformation.DungeonAllowed || cmd.DungeonID <= 0 {
			return fmt.Errorf("%w: action=%q dungeon=%d is not allowed", ErrTransformationContract, transformation.ActionType, cmd.DungeonID)
		}
		if len(transformation.ActionUsableDungeonIDs) > 0 && !containsTransformationDungeonID(transformation.ActionUsableDungeonIDs, cmd.DungeonID) {
			return fmt.Errorf("%w: action=%q dungeon=%d not in action whitelist", ErrTransformationContract, transformation.ActionType, cmd.DungeonID)
		}
		if len(transformation.LimitDungeonIDs) > 0 && !containsTransformationDungeonID(transformation.LimitDungeonIDs, cmd.DungeonID) {
			return fmt.Errorf("%w: action=%q dungeon=%d not in limit whitelist", ErrTransformationContract, transformation.ActionType, cmd.DungeonID)
		}
		return nil
	}
	if !transformation.VillageAllowed && !transformation.SeriaRoomAllowed {
		return fmt.Errorf("%w: action=%q is dungeon-only", ErrTransformationContract, transformation.ActionType)
	}
	return nil
}

func containsTransformationDungeonID(values []int64, dungeonID int64) bool {
	for _, value := range values {
		if value == dungeonID {
			return true
		}
	}
	return false
}

func validateProjectileUse(cmd Command, projectile alignedcmd.ProjectileUseResolution) error {
	stackableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(projectile.StackableType, "`", "")))
	if (stackableType != "[throw]" && stackableType != "[set]") || projectile.EffectID <= 0 ||
		len(projectile.IntData) == 0 || projectile.IntData[0] != projectile.EffectID {
		return fmt.Errorf(
			"%w: type=%q effect=%d ints=%v particles=%v",
			ErrProjectileContract,
			projectile.StackableType,
			projectile.EffectID,
			projectile.IntData,
			projectile.ParticlePaths,
		)
	}
	if !cmd.InDungeon || !projectile.DungeonAllowed || cmd.DungeonID <= 0 {
		return fmt.Errorf("%w: type=%q dungeon=%d is not active", ErrProjectileContract, projectile.StackableType, cmd.DungeonID)
	}
	if len(projectile.ActionUsableDungeonIDs) > 0 && !containsTransformationDungeonID(projectile.ActionUsableDungeonIDs, cmd.DungeonID) {
		return fmt.Errorf("%w: type=%q dungeon=%d not in action whitelist", ErrProjectileContract, projectile.StackableType, cmd.DungeonID)
	}
	if len(projectile.LimitDungeonIDs) > 0 && !containsTransformationDungeonID(projectile.LimitDungeonIDs, cmd.DungeonID) {
		return fmt.Errorf("%w: type=%q dungeon=%d not in limit whitelist", ErrProjectileContract, projectile.StackableType, cmd.DungeonID)
	}
	return nil
}

func validateStackableUsePlace(cmd Command, place alignedcmd.StackableUsePlaceResolution) error {
	if cmd.InDungeon {
		if !place.DungeonAllowed || cmd.DungeonID <= 0 {
			return fmt.Errorf("%w: dungeon=%d is not allowed", ErrStackableUsePlaceContract, cmd.DungeonID)
		}
		if len(place.ActionUsableDungeonIDs) > 0 && !containsTransformationDungeonID(place.ActionUsableDungeonIDs, cmd.DungeonID) {
			return fmt.Errorf("%w: dungeon=%d not in action whitelist", ErrStackableUsePlaceContract, cmd.DungeonID)
		}
		if len(place.LimitDungeonIDs) > 0 && !containsTransformationDungeonID(place.LimitDungeonIDs, cmd.DungeonID) {
			return fmt.Errorf("%w: dungeon=%d not in limit whitelist", ErrStackableUsePlaceContract, cmd.DungeonID)
		}
		return nil
	}
	if !place.VillageAllowed && !place.SeriaRoomAllowed {
		return fmt.Errorf("%w: village/seria is not allowed", ErrStackableUsePlaceContract)
	}
	return nil
}

func (o *Owner) useLevelUpTicket(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
	directPlanner alignedcmd.DirectExperienceConsumablePlanner,
	questPlanner alignedcmd.QuestCompletionTicketPlanner,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || directPlanner == nil || questPlanner == nil ||
		resolution.DirectExperience == nil || resolution.QuestCompletionTicket == nil {
		return UseStackableResult{}, ErrLevelUpTicketContract
	}
	effect := *resolution.DirectExperience
	questTicket := *resolution.QuestCompletionTicket
	if cmd.SourceListType != listTypeMain || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		effect.ItemID != resolution.ItemID || strings.TrimSpace(resolution.PVFPath) == "" ||
		!strings.EqualFold(strings.TrimSpace(effect.PVFPath), strings.TrimSpace(resolution.PVFPath)) ||
		!strings.EqualFold(strings.TrimSpace(effect.EffectType), "[level up ticket]") ||
		len(effect.EffectValues) != 1 || effect.EffectValues[0] <= 0 ||
		!strings.EqualFold(strings.TrimSpace(questTicket.ActionType), "[quest clear]") {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrLevelUpTicketContract, cmd.ItemTemplateID, resolution)
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	updatedAt := time.Now().UTC()
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil || tx.Inventory == nil || tx.Quest == nil {
			return ErrLevelUpTicketContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != characterID {
			return ErrCharacterRequired
		}
		skill, found, err := tx.Skill.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || skill.CharacterID != characterID {
			return ErrLevelUpTicketContract
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
		}
		durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
		durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
		durableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", "")))
		resolvedType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")))
		if durableKind != "stackable" || durablePath == "" ||
			!strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
			(durableType != "" && durableType != resolvedType) {
			return fmt.Errorf("%w: item=%d durable_kind=%q durable_path=%q durable_type=%q resolved_type=%q", ErrLevelUpTicketContract, stack.ItemID, durableKind, durablePath, durableType, resolvedType)
		}

		progressionPlan, err := directPlanner(character, skill, effect, updatedAt)
		if err != nil {
			return err
		}
		if progressionPlan.Gain == 0 || progressionPlan.Character.CharacterID != characterID ||
			progressionPlan.Skill.CharacterID != characterID || len(progressionPlan.CharacterFields) == 0 ||
			progressionPlan.PreviousLevel != character.Level || progressionPlan.NewLevel <= progressionPlan.PreviousLevel ||
			progressionPlan.NewLevel-progressionPlan.PreviousLevel != int(effect.EffectValues[0]) ||
			progressionPlan.PreviousExperience != uint32(character.Stats["exp"]) ||
			progressionPlan.NewExperience <= progressionPlan.PreviousExperience ||
			progressionPlan.SPGain < 0 || progressionPlan.SPGain > math.MaxUint16 ||
			progressionPlan.TPGain < 0 || progressionPlan.TPGain > math.MaxUint16 {
			return ErrLevelUpTicketContract
		}

		questRecord, questFound, err := tx.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !questFound {
			questRecord = dnfrepo.QuestRecord{CharacterID: characterID}
		}
		// A multi-level ticket (the runtime PVF has +3 and +10 families) skips
		// every intermediate level. Its native description says those preceding
		// levels' story/ordinary chains are completed too, so the cutoff is the
		// level immediately before the committed target, not only the original
		// character level. Ticket use eligibility was already validated against
		// the real pre-upgrade character by directPlanner.
		questCharacter := dnfrepo.CloneCharacter(character)
		questCharacter.Level = progressionPlan.NewLevel - 1
		questTicket.MinimumLevel = 0
		questTicket.MaximumLevel = int64(questCharacter.Level)
		// The level-up ticket must not consume optional side stories or a Hell
		// Party unlock. The latter's normal completion writes the permission used
		// by dungeon entry and its selector state.
		questTicket.PreserveSideQuests = true
		questTicket.PreserveHellPartyUnlocks = true
		questPlan, questErr := questPlanner(questCharacter, questRecord, questTicket, updatedAt)
		if questErr != nil && !errors.Is(questErr, dnfquest.ErrCompletionTicketNoApplicableQuest) {
			return questErr
		}
		if questErr == nil {
			if len(questPlan.CompletedQuestIDs) == 0 || len(questPlan.QuestFields) == 0 ||
				strings.TrimSpace(questPlan.Quest.CharacterID) != characterID || len(questPlan.CharacterFields) != 0 ||
				questPlan.ProfessionChanged {
				return ErrLevelUpTicketContract
			}
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			updateStackRawAmount(&stack)
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = updatedAt
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, progressionPlan.Character, progressionPlan.CharacterFields...); err != nil {
			return err
		}
		if len(progressionPlan.SkillFields) > 0 {
			if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, progressionPlan.Skill, progressionPlan.SkillFields...); err != nil {
				return err
			}
		}
		if questErr == nil {
			if err := dnfrepo.SaveQuestFields(ctx, tx.Quest, questPlan.Quest, questPlan.QuestFields...); err != nil {
				return err
			}
		}
		result = UseStackableResult{
			CharacterID:                   characterID,
			AccountID:                     cmd.AccountID,
			ListType:                      cmd.SourceListType,
			SlotIndex:                     cmd.SourceSlotIndex,
			InstanceValue:                 cmd.SourceInstanceValue,
			ItemID:                        stack.ItemID,
			RemainingCount:                remaining,
			Changed:                       true,
			PVFPath:                       durablePath,
			StackableType:                 resolution.StackableType,
			DirectExperienceApplied:       true,
			DirectExperienceGain:          progressionPlan.Gain,
			DirectExperiencePreviousLevel: progressionPlan.PreviousLevel,
			DirectExperienceNewLevel:      progressionPlan.NewLevel,
			DirectExperiencePreviousTotal: progressionPlan.PreviousExperience,
			DirectExperienceNewTotal:      progressionPlan.NewExperience,
			DirectExperienceSPGain:        progressionPlan.SPGain,
			DirectExperienceTPGain:        progressionPlan.TPGain,
			LevelUpTicketApplied:          true,
			LevelUpTicketRange:            effect.EffectValues[0],
			QuestCompletionTicketApplied:  questErr == nil,
			QuestCompletionAction:         "[level up ticket]",
			QuestCompletionCount:          len(questPlan.CompletedQuestIDs),
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func (o *Owner) useDirectExperienceConsumable(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
	planner alignedcmd.DirectExperienceConsumablePlanner,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || planner == nil || resolution.DirectExperience == nil {
		return UseStackableResult{}, ErrDirectExperienceConsumableContract
	}
	effect := *resolution.DirectExperience
	if cmd.SourceListType != listTypeMain || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		effect.ItemID != resolution.ItemID || strings.TrimSpace(resolution.PVFPath) == "" ||
		!strings.EqualFold(strings.TrimSpace(effect.PVFPath), strings.TrimSpace(resolution.PVFPath)) ||
		strings.TrimSpace(effect.EffectType) == "" || len(effect.EffectValues) == 0 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrDirectExperienceConsumableContract, cmd.ItemTemplateID, resolution)
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	updatedAt := time.Now().UTC()
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil || tx.Inventory == nil {
			return ErrDirectExperienceConsumableContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != characterID {
			return ErrCharacterRequired
		}
		skill, found, err := tx.Skill.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || skill.CharacterID != characterID {
			return ErrDirectExperienceConsumableContract
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
		}
		durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
		durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
		durableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", "")))
		resolvedType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")))
		if durableKind != "stackable" || durablePath == "" ||
			!strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
			(durableType != "" && durableType != resolvedType) {
			return fmt.Errorf("%w: item=%d durable_kind=%q durable_path=%q durable_type=%q resolved_type=%q", ErrDirectExperienceConsumableContract, stack.ItemID, durableKind, durablePath, durableType, resolvedType)
		}

		plan, err := planner(character, skill, effect, updatedAt)
		if err != nil {
			return err
		}
		if plan.Gain == 0 || plan.Character.CharacterID != characterID || plan.Skill.CharacterID != characterID ||
			len(plan.CharacterFields) == 0 || plan.PreviousLevel != character.Level ||
			plan.PreviousExperience != uint32(character.Stats["exp"]) || plan.NewLevel < plan.PreviousLevel ||
			plan.NewExperience <= plan.PreviousExperience {
			return ErrDirectExperienceConsumableContract
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			updateStackRawAmount(&stack)
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = updatedAt
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, plan.Character, plan.CharacterFields...); err != nil {
			return err
		}
		if len(plan.SkillFields) > 0 {
			if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, plan.Skill, plan.SkillFields...); err != nil {
				return err
			}
		}
		result = UseStackableResult{
			CharacterID:                   characterID,
			AccountID:                     cmd.AccountID,
			ListType:                      cmd.SourceListType,
			SlotIndex:                     cmd.SourceSlotIndex,
			InstanceValue:                 cmd.SourceInstanceValue,
			ItemID:                        stack.ItemID,
			RemainingCount:                remaining,
			Changed:                       true,
			PVFPath:                       durablePath,
			StackableType:                 resolution.StackableType,
			DirectExperienceApplied:       true,
			DirectExperienceGain:          plan.Gain,
			DirectExperiencePreviousLevel: plan.PreviousLevel,
			DirectExperienceNewLevel:      plan.NewLevel,
			DirectExperiencePreviousTotal: plan.PreviousExperience,
			DirectExperienceNewTotal:      plan.NewExperience,
			DirectExperienceSPGain:        plan.SPGain,
			DirectExperienceTPGain:        plan.TPGain,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func (o *Owner) useQuestCompletionTicket(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
	planner alignedcmd.QuestCompletionTicketPlanner,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || planner == nil || resolution.QuestCompletionTicket == nil {
		return UseStackableResult{}, ErrQuestCompletionTicketContract
	}
	if cmd.SourceListType != listTypeMain || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" || strings.TrimSpace(resolution.QuestCompletionTicket.ActionType) == "" {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrQuestCompletionTicketContract, cmd.ItemTemplateID, resolution)
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	completedAt := time.Now().UTC()
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Inventory == nil || tx.Quest == nil {
			return ErrQuestCompletionTicketContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterRequired
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
		}
		durablePVFPath := strings.TrimSpace(stack.Extra["pvf_path"])
		if durablePVFPath == "" || !strings.EqualFold(durablePVFPath, strings.TrimSpace(resolution.PVFPath)) {
			return fmt.Errorf("%w: item=%d durable_path=%q resolved_path=%q", ErrQuestCompletionTicketContract, stack.ItemID, durablePVFPath, resolution.PVFPath)
		}
		questRecord, questFound, err := tx.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !questFound {
			questRecord = dnfrepo.QuestRecord{CharacterID: characterID}
		}
		plan, err := planner(character, questRecord, *resolution.QuestCompletionTicket, completedAt)
		if err != nil {
			return err
		}
		if len(plan.CompletedQuestIDs) == 0 || len(plan.QuestFields) == 0 || strings.TrimSpace(plan.Quest.CharacterID) != characterID {
			return ErrQuestCompletionTicketContract
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = completedAt
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveQuestFields(ctx, tx.Quest, plan.Quest, plan.QuestFields...); err != nil {
			return err
		}
		if len(plan.CharacterFields) > 0 {
			if strings.TrimSpace(plan.Character.CharacterID) != characterID {
				return ErrQuestCompletionTicketContract
			}
			if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, plan.Character, plan.CharacterFields...); err != nil {
				return err
			}
		}
		result = UseStackableResult{
			CharacterID:                     characterID,
			AccountID:                       cmd.AccountID,
			ListType:                        cmd.SourceListType,
			SlotIndex:                       cmd.SourceSlotIndex,
			InstanceValue:                   cmd.SourceInstanceValue,
			ItemID:                          stack.ItemID,
			RemainingCount:                  remaining,
			Changed:                         true,
			PVFPath:                         durablePVFPath,
			StackableType:                   resolution.StackableType,
			QuestCompletionTicketApplied:    true,
			QuestCompletionAction:           resolution.QuestCompletionTicket.ActionType,
			QuestCompletionCount:            len(plan.CompletedQuestIDs),
			QuestCompletionProfessionChange: plan.ProfessionChanged,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func (o *Owner) usePetFeed(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	if o.petItems == nil || o.pets == nil {
		return UseStackableResult{}, ErrPetInventoryOwnerRequired
	}
	normalizedType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")))
	if resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" ||
		normalizedType != "[feed]" ||
		resolution.PetFeedEffectDurationSeconds <= 0 ||
		resolution.PetFeedSkillChargePercent >= 0 ||
		resolution.PetFeedSatietyDelta <= 0 || resolution.PetFeedSatietyDelta > 100 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrPetFeedContractRequired, cmd.ItemTemplateID, resolution)
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result UseStackableResult
	err := dnfrepo.WithinCharacterPetsScoped(ctx, o.petItems, characterID, func(
		inventoryRepo dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
		petRepo dnfrepo.PetRepository,
	) error {
		inventoryRecord, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventoryRecord = dnfrepo.CloneInventory(inventoryRecord)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventoryRecord.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != int64(cmd.ItemTemplateID) {
			return fmt.Errorf(
				"%w: list=%d slot=%d want=%d got=%d",
				ErrItemMismatch,
				cmd.SourceListType,
				cmd.SourceSlotIndex,
				cmd.ItemTemplateID,
				stack.ItemID,
			)
		}
		durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
		durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
		durableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", "")))
		if durableKind != "stackable" || durablePath == "" ||
			!strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
			(durableType != "" && durableType != "[feed]") {
			return fmt.Errorf(
				"%w: list=%d slot=%d item=%d durable_kind=%q durable_path=%q durable_type=%q",
				ErrPetFeedContractRequired,
				cmd.SourceListType,
				cmd.SourceSlotIndex,
				stack.ItemID,
				durableKind,
				durablePath,
				durableType,
			)
		}

		petRecord, found, err := petRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPetFeedEquippedCreatureRequired
		}
		petRecord = dnfrepo.ClonePet(petRecord)
		petKey := strings.TrimSpace(petRecord.EquippedKey)
		entry, found := petRecord.Entries[petKey]
		if petKey == "" || !found || entry.ItemID <= 0 || entry.SourceListType != listTypeEquipment || entry.SourceSlotIndex != 26 {
			return fmt.Errorf("%w: equipped_key=%q", ErrPetFeedEquippedCreatureRequired, petKey)
		}
		feedUpdate, err := creaturestate.ApplyFeed(entry, resolution.PetFeedSatietyDelta)
		if err != nil {
			return err
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventoryRecord.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			updateStackRawAmount(&stack)
			inventoryRecord.Slots[key] = stack
		}
		now := time.Now()
		inventoryRecord.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventoryRecord, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if feedUpdate.Changed {
			entry.Satiety = feedUpdate.AfterSatiety
			entry.SatietyMicros = feedUpdate.AfterSatietyMicros
			petRecord.Entries[petKey] = entry
			petRecord.UpdatedAt = now
			if err := dnfrepo.SavePetFields(ctx, petRepo, petRecord, dnfrepo.PetFieldEntries); err != nil {
				return err
			}
		}

		result = UseStackableResult{
			CharacterID:                  characterID,
			AccountID:                    cmd.AccountID,
			ListType:                     cmd.SourceListType,
			SlotIndex:                    cmd.SourceSlotIndex,
			InstanceValue:                cmd.SourceInstanceValue,
			ItemID:                       stack.ItemID,
			RemainingCount:               remaining,
			Changed:                      true,
			PVFPath:                      resolution.PVFPath,
			StackableType:                resolution.StackableType,
			PetFeedApplied:               true,
			PetFeedPetKey:                petKey,
			PetFeedSatietyBefore:         int64(feedUpdate.BeforeSatiety),
			PetFeedSatietyAfter:          int64(feedUpdate.AfterSatiety),
			PetFeedSatietyDelta:          int64(feedUpdate.AppliedDelta),
			PetFeedEffectDurationSeconds: resolution.PetFeedEffectDurationSeconds,
			PetFeedSkillChargePercent:    resolution.PetFeedSkillChargePercent,
		}
		return nil
	})
	return result, err
}

func (o *Owner) useExperienceConsumable(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.ExperienceConsumableResolution,
) (UseStackableResult, error) {
	if o.rentalAssets == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return UseStackableResult{}, ErrAccountRequired
	}
	if resolution.ItemID != int64(cmd.ItemTemplateID) || resolution.BonusPercent <= 0 ||
		resolution.DurationSeconds <= 0 || strings.TrimSpace(resolution.PVFPath) == "" {
		return UseStackableResult{}, ErrExperienceConsumableResolveFailed
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result UseStackableResult
	err := dnfrepo.WithinRentalAssetsScoped(ctx, o.rentalAssets, accountID, characterID, func(accounts dnfrepo.AccountRepository, _ dnfrepo.CharacterRepository, inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		record, ok, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, ok := record.Slots[key]
		if !ok || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != int64(cmd.ItemTemplateID) {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, cmd.ItemTemplateID, stack.ItemID)
		}
		pvfPath, stackableType, reusable, proven := pvfProvenExperienceStack(stack)
		if !proven || !strings.EqualFold(strings.TrimSpace(pvfPath), strings.TrimSpace(resolution.PVFPath)) {
			return ErrUseStackableContractRequired
		}
		if reusable != resolution.Reusable {
			return ErrUseStackableContractRequired
		}

		account, found, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found {
			return ErrAccountNotFound
		}
		account = dnfrepo.CloneAccount(account)
		now := time.Now().UTC()
		effect, err := experiencebonus.ActivateForCharacter(
			&account,
			characterID,
			resolution.ItemID,
			resolution.BonusPercent,
			time.Duration(resolution.DurationSeconds)*time.Second,
			now,
		)
		if err != nil {
			return err
		}

		remaining := stack.Count
		if !resolution.Reusable {
			remaining--
			if remaining == 0 {
				delete(record.Slots, key)
			} else {
				stack = cloneStack(stack)
				stack.Count = remaining
				record.Slots[key] = stack
			}
			record.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, record, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		account.UpdatedAt = now
		if err := accounts.Save(ctx, account); err != nil {
			return err
		}
		result = UseStackableResult{
			CharacterID:                     characterID,
			AccountID:                       accountID,
			ListType:                        cmd.SourceListType,
			SlotIndex:                       cmd.SourceSlotIndex,
			InstanceValue:                   cmd.SourceInstanceValue,
			ItemID:                          stack.ItemID,
			RemainingCount:                  remaining,
			Changed:                         true,
			PVFPath:                         pvfPath,
			StackableType:                   stackableType,
			ExperienceBonusActivated:        true,
			ExperienceBonusPercent:          effect.BonusPercent,
			ExperienceBonusRemainingSeconds: int64(effect.ExpireAt.Sub(now).Seconds()),
		}
		return nil
	})
	return result, err
}

// UseSharedEffectItem handles the current class-1/type251 shared-effect item
// route. The request carries its source item template and source slot, both of
// which must match durable inventory before the runtime PVF result table can
// authorize any mutation.
func (o *Owner) UseSharedEffectItem(ctx context.Context, cmd Command, resolver alignedcmd.RandomRewardItemResolver) (UseStackableResult, error) {
	if o == nil || o.repo == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UseStackableResult{}, ErrCharacterRequired
	}
	if err := checkUseStackableListType(cmd.SourceListType); err != nil {
		return UseStackableResult{}, err
	}
	if cmd.ItemTemplateID <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrUseStackableContractRequired, cmd.ItemTemplateID)
	}
	if resolver == nil {
		return UseStackableResult{}, fmt.Errorf("%w: type251 resolver unavailable", ErrRandomRewardResolveFailed)
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result UseStackableResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.UseSharedEffectItem(ctx, cmd, resolver)
			return err
		})
		return result, err
	}

	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !ok {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	source, ok := record.Slots[key]
	if !ok || source.ItemID <= 0 || source.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if source.ItemID != int64(cmd.ItemTemplateID) {
		return UseStackableResult{}, fmt.Errorf(
			"%w: list=%d slot=%d item want=%d got=%d",
			ErrItemMismatch,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			cmd.ItemTemplateID,
			source.ItemID,
		)
	}
	resolution, err := resolver(source.ItemID)
	if err != nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrRandomRewardResolveFailed, source.ItemID, err)
	}
	if resolution.SourceItemID == 0 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrRandomRewardContractRequired, source.ItemID)
	}
	return o.useRandomRewardItem(ctx, cmd, resolution)
}

func (o *Owner) useRandomRewardItem(ctx context.Context, cmd Command, resolution alignedcmd.RandomRewardItemResolution) (UseStackableResult, error) {
	if resolution.SourceItemID != int64(cmd.ItemTemplateID) || strings.TrimSpace(resolution.SourcePVFPath) == "" ||
		!strings.EqualFold(strings.Trim(resolution.StackableType, "` []"), "random reward item") || len(resolution.Outcomes) == 0 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrRandomRewardContractRequired, cmd.ItemTemplateID)
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result UseStackableResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.useRandomRewardItem(ctx, cmd, resolution)
			return err
		})
		return result, err
	}
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !ok {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	source, ok := record.Slots[key]
	if !ok || source.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if source.ItemID != int64(cmd.ItemTemplateID) {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, cmd.ItemTemplateID, source.ItemID)
	}
	outcome, err := chooseRandomRewardItemOutcome(resolution.Outcomes)
	if err != nil {
		return UseStackableResult{}, err
	}
	remaining := source.Count - 1
	if remaining == 0 {
		delete(record.Slots, key)
	} else {
		source = cloneStack(source)
		source.Count = remaining
		updateStackRawAmount(&source)
		record.Slots[key] = source
	}
	result := UseStackableResult{
		CharacterID:    characterID,
		AccountID:      cmd.AccountID,
		ListType:       cmd.SourceListType,
		SlotIndex:      cmd.SourceSlotIndex,
		InstanceValue:  cmd.SourceInstanceValue,
		ItemID:         int64(cmd.ItemTemplateID),
		RemainingCount: remaining,
		Changed:        true,
		PVFPath:        resolution.SourcePVFPath,
		StackableType:  resolution.StackableType,
	}
	if outcome.Reward.ItemID > 0 {
		slots, err := grantRandomRewardItem(record.Slots, outcome.Reward, 1)
		if err != nil {
			return UseStackableResult{}, err
		}
		result.RandomRewardItemID = outcome.Reward.ItemID
		result.RandomRewardSlots = slots
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func chooseRandomRewardItemOutcome(outcomes []alignedcmd.RandomRewardItemOutcome) (alignedcmd.RandomRewardItemOutcome, error) {
	var total int64
	for _, outcome := range outcomes {
		if outcome.Weight <= 0 {
			continue
		}
		if total > int64(^uint64(0)>>1)-outcome.Weight {
			return alignedcmd.RandomRewardItemOutcome{}, ErrRandomRewardContractRequired
		}
		total += outcome.Weight
	}
	if total <= 0 {
		return alignedcmd.RandomRewardItemOutcome{}, ErrRandomRewardContractRequired
	}
	roll := rand.Int63n(total)
	for _, outcome := range outcomes {
		if outcome.Weight <= 0 {
			continue
		}
		if roll < outcome.Weight {
			return outcome, nil
		}
		roll -= outcome.Weight
	}
	return alignedcmd.RandomRewardItemOutcome{}, ErrRandomRewardContractRequired
}

func grantRandomRewardItem(items map[string]dnfrepo.ItemStack, reward alignedcmd.MagicBoxRewardItem, count int64) ([]int16, error) {
	if count <= 0 {
		return nil, nil
	}
	// 契约校验分两段：先做与 kind 无关的共同项，再按 kind 分派。这样"装备行"不会
	// 把可叠加那一支的条件放松掉，可叠加那一支的行为与改动前逐字相同。
	if reward.ItemID <= 0 || reward.TargetListType != listTypeMain || reward.SlotStart < 0 || reward.SlotEnd < reward.SlotStart {
		return nil, fmt.Errorf("%w: item=%d kind=%q target_list=%d slots=%d..%d", ErrRandomRewardContractRequired, reward.ItemID, reward.Kind, reward.TargetListType, reward.SlotStart, reward.SlotEnd)
	}
	// 真档奖池是**混合表**：塔罗牌 [int data] 里既有可叠加物（光之根源），也有装备
	// （血红战斧 / 阿努比斯陶瓷长靴 …）。装备行过去被下面这个 kind 判断整体拒掉，
	// 而奖池的抽取又是按权重随机的，于是"抽到装备就等于什么都没发生"。
	if strings.EqualFold(strings.TrimSpace(reward.Kind), "equipment") {
		return grantRandomRewardEquipment(items, reward, count)
	}
	if reward.Kind != "stackable" {
		return nil, fmt.Errorf("%w: item=%d kind=%q target_list=%d slots=%d..%d", ErrRandomRewardContractRequired, reward.ItemID, reward.Kind, reward.TargetListType, reward.SlotStart, reward.SlotEnd)
	}
	// Revive coins go to the fixed slot-1 wallet like every other channel;
	// the sibling useStackableWithResolvers path already does this.
	if dnfrevivecoin.IsReward(reward.ItemID) {
		if _, _, err := dnfrevivecoin.GrantSlots(items, count, "revive_coin_random_reward"); err != nil {
			return nil, err
		}
		return []int16{dnfrevivecoin.WalletSlot}, nil
	}
	remaining := count
	// PVF [stack limit] 同时是单行上限和"这个容器里该道具的总量上限"：先按容器总量算出
	// 还能收多少，并且在容器里已经有该道具的行时只并进那一行，不再开第二行。
	space := dnfrepo.ContainerItemSpace(items, listTypeMain, reward.ItemID, reward.StackLimit)
	changed := make([]int16, 0, 2)
	markChanged := func(slot int16) {
		for _, previous := range changed {
			if previous == slot {
				return
			}
		}
		changed = append(changed, slot)
	}
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		stack, found := items[key]
		if !found || stack.ItemID != reward.ItemID || stack.Count <= 0 || reward.StackLimit == 1 || !randomRewardExpirationMatches(stack, reward.ExpireAt) {
			continue
		}
		capacity := remaining
		if reward.StackLimit > 0 {
			capacity = reward.StackLimit - stack.Count
		}
		if capacity > space {
			capacity = space
		}
		if capacity <= 0 {
			continue
		}
		add := remaining
		if add > capacity {
			add = capacity
		}
		stack = cloneStack(stack)
		stack.Count += add
		updateStackRawAmount(&stack)
		items[key] = stack
		remaining -= add
		space -= add
		markChanged(slot)
	}
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(items, listTypeMain, reward.ItemID, reward.StackLimit) {
			break
		}
		insert := remaining
		if reward.StackLimit > 0 && insert > reward.StackLimit {
			insert = reward.StackLimit
		}
		if insert > space {
			insert = space
		}
		if insert <= 0 {
			break
		}
		stack := dnfrepo.ItemStack{
			ItemID:   reward.ItemID,
			Count:    insert,
			ExpireAt: reward.ExpireAt,
			Extra: map[string]string{
				"item_kind": "stackable",
				"pvf_path":  reward.PVFPath,
			},
		}
		if !reward.ExpireAt.IsZero() && reward.ExpireAt.Unix() > 0 {
			expire := strconv.FormatInt(reward.ExpireAt.Unix(), 10)
			stack.Extra["expire_time"] = expire
			stack.Extra["expire_unix"] = expire
		}
		if reward.UsablePeriodDays > 0 {
			stack.Extra["usable_period_days"] = strconv.FormatInt(reward.UsablePeriodDays, 10)
			stack.Extra["expiration_source"] = "runtime_pvf_usable_period_grant"
		}
		updateStackRawAmount(&stack)
		items[key] = stack
		remaining -= insert
		space -= insert
		markChanged(slot)
	}
	if remaining > 0 {
		return nil, fmt.Errorf("%w: item=%d remaining=%d", ErrRandomRewardInventoryFull, reward.ItemID, remaining)
	}
	return changed, nil
}

// grantRandomRewardEquipment 发放一件**装备**奖励：一格一件、绝不叠加。
//
// 现场 2026-10-03（服主报「塔罗牌用不了」）：真档奖池是混合表 ——
// stackable/10159001/10159717.stk（暗之偶像阿努比斯 塔罗牌）的
//
//	[int data]
//	108040415 175000 1  100270516 175000 1  114030107 175000 1  10159142 300000 250
//	[/int data]
//
// 前三条是装备（血红战斧 / 阿努比斯陶瓷长靴 / 战戟），第四条才是可叠加的「光之根源」。
// 装备行以前被 grantRandomRewardItem 的 kind 判断整体拒掉，抽到装备就等于**什么都没发生**。
//
// 与可叠加那一支的三点差别，逐条都有出处：
//
//  1. **不合并**。装备的 PVF 没有 [stack limit]，ContainerSpace 那边按"不受限"处理；
//     但客户端把装备画成一格一件，把 count 并进同一行会让它变成叠加物。所以这里
//     一件占一格，count > 1（真档不会出现，但契约上允许）就开多行。
//  2. **行内容按装备投影**：item_kind=equipment + pvf_path + equipment_type，RawEntry
//     用仓库自己那条 currentRawEntryForStack 生成 —— 与
//     owner_equipment_regeneration.go 发装备、booster_projection.go 的发宠物装备同一写法。
//  3. **耐久**。PVF 的 [durability] 是**满耐久**；不写进去客户端会把新装备画成 0 耐久
//     （writeCommonItemListEntry 的 +0x0B 直接取 Extra["durability"]）。
func grantRandomRewardEquipment(items map[string]dnfrepo.ItemStack, reward alignedcmd.MagicBoxRewardItem, count int64) ([]int16, error) {
	changed := make([]int16, 0, 2)
	remaining := count
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		extra := map[string]string{
			"source":          "random_reward_equipment",
			"item_kind":       "equipment",
			"pvf_path":        reward.PVFPath,
			"equipment_type":  reward.EquipmentType,
			"amount_or_count": "0",
		}
		if reward.Seal {
			extra["seal_flag"] = "1"
		}
		if reward.Durability > 0 {
			durability := strconv.FormatUint(uint64(reward.Durability), 10)
			extra["durability"] = durability
			extra["max_durability"] = durability
		}
		if !reward.ExpireAt.IsZero() && reward.ExpireAt.Unix() > 0 {
			expire := strconv.FormatInt(reward.ExpireAt.Unix(), 10)
			extra["expire_time"] = expire
			extra["expire_unix"] = expire
		}
		if reward.UsablePeriodDays > 0 {
			extra["usable_period_days"] = strconv.FormatInt(reward.UsablePeriodDays, 10)
			extra["expiration_source"] = "runtime_pvf_usable_period_grant"
		}
		stack := dnfrepo.ItemStack{
			ItemID:   reward.ItemID,
			Count:    1,
			ExpireAt: reward.ExpireAt,
			Extra:    extra,
		}
		// 品级种子由解析方掷好传进来（见 alignedcmd.MagicBoxRewardItem.QualitySeed）。
		// 不写的话 raw+0x06 会落到投影的 Count 兜底（=1），客户端把奖励装备画成"最下级"。
		if reward.QualitySeed != 0 {
			_ = itemquality.Apply(&stack, reward.QualitySeed)
		}
		stack.RawEntry = currentRawEntryForStack(slot, stack)
		items[key] = stack
		changed = append(changed, slot)
		remaining--
	}
	if remaining > 0 {
		return nil, fmt.Errorf("%w: item=%d remaining=%d", ErrRandomRewardInventoryFull, reward.ItemID, remaining)
	}
	return changed, nil
}

func randomRewardExpirationMatches(stack dnfrepo.ItemStack, expiration time.Time) bool {
	want := int64(0)
	if !expiration.IsZero() && expiration.Unix() > 0 {
		want = expiration.Unix()
	}
	got := int64(0)
	for _, key := range []string{"expire_time", "expire_unix"} {
		value, err := strconv.ParseInt(strings.TrimSpace(stack.Extra[key]), 10, 64)
		if err == nil && value > 0 {
			got = value
			break
		}
	}
	if got == 0 && !stack.ExpireAt.IsZero() && stack.ExpireAt.Unix() > 0 {
		got = stack.ExpireAt.Unix()
	}
	return got == want
}

// usePremiumContract activates one premiumlist_new.etc contract item: consume
// exactly one stack item and upsert the account-level premium expiry in the
// same rental-asset transaction, so a failure can never lose the item without
// the contract or grant the contract without the item. The resolver output is
// the only authority for type/duration; request and inventory-Extra metadata
// are never trusted.
func (o *Owner) usePremiumContract(ctx context.Context, cmd Command, resolution alignedcmd.PremiumContractResolution) (UseStackableResult, error) {
	if o.rentalAssets == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return UseStackableResult{}, ErrAccountRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result UseStackableResult
	err := dnfrepo.WithinRentalAssetsScoped(ctx, o.rentalAssets, accountID, characterID, func(accounts dnfrepo.AccountRepository, _ dnfrepo.CharacterRepository, inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		record, ok, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		if record.Slots == nil {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, ok := record.Slots[key]
		if !ok || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != int64(cmd.ItemTemplateID) {
			return fmt.Errorf(
				"%w: list=%d slot=%d want=%d got=%d",
				ErrItemMismatch,
				cmd.SourceListType,
				cmd.SourceSlotIndex,
				cmd.ItemTemplateID,
				stack.ItemID,
			)
		}

		account, found, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found {
			return ErrAccountNotFound
		}
		account = dnfrepo.CloneAccount(account)

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(record.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			record.Slots[key] = stack
		}
		now := time.Now()
		record.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}

		premium.Upsert(&account, resolution.PremiumType, resolution.DurationSeconds, 1, now)
		account.UpdatedAt = now
		if err := accounts.Save(ctx, account); err != nil {
			return err
		}

		activations := make([]PremiumActivation, 0, len(premium.ActivationTypes(resolution.PremiumType)))
		for _, activationType := range premium.ActivationTypes(resolution.PremiumType) {
			activations = append(activations, PremiumActivation{
				PremiumType:      activationType,
				RemainingSeconds: premium.ExpireAt(account, activationType) - now.Unix(),
			})
		}
		result = UseStackableResult{
			CharacterID:             characterID,
			AccountID:               accountID,
			ListType:                cmd.SourceListType,
			SlotIndex:               cmd.SourceSlotIndex,
			InstanceValue:           cmd.SourceInstanceValue,
			ItemID:                  stack.ItemID,
			RemainingCount:          remaining,
			Changed:                 true,
			PremiumActivated:        true,
			PremiumType:             resolution.PremiumType,
			PremiumRemainingSeconds: premium.ExpireAt(account, resolution.PremiumType) - now.Unix(),
			PremiumActivations:      activations,
		}
		return nil
	})
	return result, err
}

func newUpgradeResult(cmd Command, errorCode byte) UpgradeResult {
	return UpgradeResult{
		Mode:                      cmd.Mode,
		ErrorCode:                 errorCode,
		TargetListType:            cmd.TargetListType,
		TargetSlotIndex:           cmd.TargetSlotIndex,
		TargetItemTemplateID:      cmd.TargetItemTemplateID,
		MaterialSlotIndex:         cmd.MaterialSlotIndex,
		OptionalTicketSlotIndex:   cmd.OptionalTicketSlot,
		ProtectionTicketSlotIndex: -1,
	}
}

func isUpgradeableEquipmentStack(stack dnfrepo.ItemStack) bool {
	if stack.ItemID <= 0 || stack.Count <= 0 {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(stack.Extra["kind"]))
	}
	switch kind {
	case "equipment":
		// 装备一件一行：PVF 对 equipment 一律按单件处理，Count != 1 只可能是历史脏数据
		// （魔盒发奖把 N 件同名装备并进一格，现场 2026-09-27 线上 90 行 357 件）。
		// 强化/强化券是单件语义入口，一次费用只能作用一件；放整行进来就会
		// 「一次强化把 N 件全部 +1」。
		return stack.Count == 1
	case "", "unknown":
		return stack.Count == 1
	default:
		return false
	}
}

// isEquipmentKindStack 判断这一行是不是装备类行（只看 kind，不看数量）。
func isEquipmentKindStack(stack dnfrepo.ItemStack) bool {
	kind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(stack.Extra["kind"]))
	}
	return kind == "equipment"
}

// equipmentStackCountIsSingle 是「单件语义」入口的统一护栏：装备一件一行。
//
// 为什么必须有它：线上存在 Extra.item_kind=equipment 且 Count=2..49 的历史堆叠行
// （根因见魔盒发奖 grantMagicBoxReward，2026-09-27 现场 90 行 357 件）。客户端装备行的
// 0x77 里 0x06 字段是品质种子而不是数量，玩家在界面上看不见 N；于是「一份材料/一张券/
// 一次费用」的操作（强化、强化券、锻造、附魔、符文）会被作用到整行 N 件上。
// 非装备行一律放行 —— 本护栏只针对装备。
func equipmentStackCountIsSingle(stack dnfrepo.ItemStack) bool {
	if !isEquipmentKindStack(stack) {
		return true
	}
	return stack.Count == 1
}

func upgradeDurabilityOK(stack dnfrepo.ItemStack) bool {
	if stack.Extra == nil {
		return true
	}
	maxRaw := strings.TrimSpace(stack.Extra["max_durability"])
	if maxRaw == "" {
		return true
	}
	maxValue, err := strconv.ParseInt(maxRaw, 0, 64)
	if err != nil || maxValue < 0 {
		return false
	}
	currentRaw := strings.TrimSpace(stack.Extra["durability"])
	if currentRaw == "" {
		return maxValue == 0
	}
	current, err := strconv.ParseInt(currentRaw, 0, 64)
	if err != nil || current < 0 {
		return false
	}
	return current == maxValue
}

func upgradeCommandRequiresAccountMaterial(cmd Command) bool {
	itemID, _, ok := upgradeMaterialRequirement(cmd)
	if !ok {
		return dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.MaterialSlotIndex)
	}
	_, accountOwned := upgradeAccountSharedMaterialSlot(itemID)
	return accountOwned
}

// upgradeDestroyRefundsWarehouse reports whether any destruction refund row
// resolved for this attempt owns a fixed account-shared warehouse slot.
func upgradeDestroyRefundsWarehouse(cmd Command) bool {
	if cmd.UpgradeDestroyBonusItemID > 0 && cmd.UpgradeDestroyBonusCount > 0 {
		if _, warehouse := upgradeAccountSharedMaterialSlot(int64(cmd.UpgradeDestroyBonusItemID)); warehouse {
			return true
		}
	}
	for _, extra := range cmd.UpgradeDestroyExtraRewards {
		if extra.ItemID <= 0 || extra.Count <= 0 {
			continue
		}
		if _, warehouse := upgradeAccountSharedMaterialSlot(int64(extra.ItemID)); warehouse {
			return true
		}
	}
	return false
}

func upgradeMaterialRequirement(cmd Command) (int64, int64, bool) {
	itemID := int64(cmd.UpgradeMaterialItemID)
	count := int64(cmd.UpgradeMaterialCount)
	if itemID <= 0 || count <= 0 {
		return 0, 0, false
	}
	return itemID, count, true
}

// upgradeAccountSharedMaterialSlot 给出该道具在账号共享晶体/灵魂仓库里的固定槽。
// 这里过去是一份手写的 12 条 switch 副本，与 dnfbridge 的两处副本各自独立 ——
// 三份表任何一处漏改都会让某条途径把道具落进角色背包（2026-09-21 NPC 商店金币
// 购买的无色落在 0:154 就是这条链路的产物）。现统一走权威表
// (repository/account_shared_materials.go)。运营口径见该文件。
func upgradeAccountSharedMaterialSlot(itemID int64) (int16, bool) {
	return dnfrepo.AccountSharedWarehouseSlotForItem(itemID)
}

func upgradeProtectionSlotAllowed(slot int16) bool {
	return (slot >= 3 && slot <= 8) || (slot >= 121 && slot <= 176)
}

// firstUpgradeProtectionTicket mirrors the native/reference normal-upgrade
// owner: op50 commonly carries optional slot -1, so a destructive failure
// searches quick slots first and then the material page. Only the first live
// PVF-proved ticket for the exact reinforcement/amplification mode is selected.
// The caller consumes it in the same character-asset transaction as the
// target, upgrade material and wallet mutation.
func (o *Owner) firstUpgradeProtectionTicket(items map[string]dnfrepo.ItemStack, mode string, targetSlot int16, materialSlot int16) (int16, dnfrepo.ItemStack, alignedcmd.UpgradeProtectionResolution, bool, error) {
	if o == nil || o.upgradeProtection == nil {
		return -1, dnfrepo.ItemStack{}, alignedcmd.UpgradeProtectionResolution{}, false, nil
	}
	// The ticket's [expiration date] is deliberately not consulted: see the
	// explicit-slot path above. Most of this PVF's protection tickets are dead
	// 2010/2015 event items, nothing else on this server acts on item expiry,
	// and the client promises protection from the bag count alone.
	for _, bounds := range [][2]int16{{3, 8}, {121, 176}} {
		for slot := bounds[0]; slot <= bounds[1]; slot++ {
			if slot == targetSlot || slot == materialSlot {
				continue
			}
			stack, found := items[slotKey(listTypeMain, slot)]
			if !found || stack.ItemID <= 0 || stack.Count <= 0 {
				continue
			}
			resolution, err := o.upgradeProtection(stack.ItemID)
			if err != nil {
				return -1, dnfrepo.ItemStack{}, alignedcmd.UpgradeProtectionResolution{}, false, err
			}
			if !strings.EqualFold(strings.TrimSpace(resolution.TicketMode), mode) {
				continue
			}
			if resolution.FailureRetainLevel < 0 || resolution.FailureRetainLevel > 30 {
				return -1, dnfrepo.ItemStack{}, alignedcmd.UpgradeProtectionResolution{}, false, fmt.Errorf("invalid upgrade protection retain level item=%d level=%d", stack.ItemID, resolution.FailureRetainLevel)
			}
			return slot, stack, resolution, true, nil
		}
	}
	return -1, dnfrepo.ItemStack{}, alignedcmd.UpgradeProtectionResolution{}, false, nil
}

// firstEmptyUpgradeSlot finds the first empty main-inventory slot for destroy
// grantUpgradeDestroyWarehouseBonus stacks one destruction refund into the
// item's single fixed account-shared warehouse slot and returns the unplaced
// remainder, which the caller sends to the backpack so no refund is lost.
// The twelve warehouse stackables (six cube fragments, six souls) are the
// same set the 分解机 places there, and a blown-up piece refunds exactly what
// disassembling it would have: the destination must therefore be the same.
// A foreign stack squatting on the fixed slot is never merged into or
// evicted -- that row goes to the backpack instead.
func (o *Owner) grantUpgradeDestroyWarehouseBonus(
	account *dnfrepo.AccountInventoryRecord,
	slot int16,
	itemID int64,
	count int64,
) int64 {
	if o == nil || account == nil || itemID <= 0 || count <= 0 {
		return count
	}
	if account.Slots == nil {
		account.Slots = make(map[string]dnfrepo.ItemStack)
	}
	key := dnfrepo.AccountSharedInventorySlotKey(slot)
	stack, exists := account.Slots[key]
	if exists && stack.ItemID > 0 && stack.ItemID != itemID {
		return count
	}
	room := count
	if o.stackMerge != nil {
		if resolution, err := o.stackMerge(itemID); err == nil &&
			resolution.ItemID == itemID && resolution.StackLimit > 0 {
			if space := resolution.StackLimit - stack.Count; space < room {
				room = space
			}
		}
	}
	if room <= 0 {
		return count
	}
	if !exists || stack.ItemID <= 0 {
		stack = o.canonicalStackForMove(dnfrepo.ItemStack{
			ItemID: itemID,
			Extra:  map[string]string{"source": "upgrade_destroy_bonus"},
		})
	} else {
		stack = cloneStack(stack)
	}
	stack.Count += room
	updateStackRawAmount(&stack)
	account.Slots[key] = stack
	return count - room
}

func (o *Owner) grantUpgradeDestroyBonus(items map[string]dnfrepo.ItemStack, itemID int64, count int64) ([]int16, error) {
	if itemID <= 0 || count <= 0 {
		return nil, ErrUpgradeDestroyBonusContract
	}
	// Preserve the historical isolated-domain behavior when no runtime PVF
	// resolver is installed. The live op50 route always supplies the resolver.
	if o == nil || o.stackMerge == nil {
		destroySlot := firstEmptyUpgradeSlot(items)
		if destroySlot < 0 {
			return nil, ErrUpgradeDestroyBonusInventoryFull
		}
		bonusStack := dnfrepo.ItemStack{
			ItemID: itemID,
			Count:  count,
			Extra:  map[string]string{"source": "upgrade_destroy_bonus"},
		}
		updateStackRawAmount(&bonusStack)
		items[slotKey(listTypeMain, destroySlot)] = bonusStack
		return []int16{destroySlot}, nil
	}

	resolution, err := o.stackMerge(itemID)
	if err != nil || resolution.ItemID != itemID || resolution.StackLimit < 0 ||
		!strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") ||
		resolution.SlotStart < 0 || resolution.SlotEnd < resolution.SlotStart {
		return nil, fmt.Errorf(
			"%w: item=%d kind=%q slots=%d..%d: %v",
			ErrUpgradeDestroyBonusContract,
			itemID,
			resolution.Kind,
			resolution.SlotStart,
			resolution.SlotEnd,
			err,
		)
	}
	pvfPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.PVFPath, "\\", "/")))
	if !strings.HasPrefix(pvfPath, "stackable/") || strings.Contains(pvfPath, "../") {
		return nil, fmt.Errorf("%w: item=%d pvf_path=%q", ErrUpgradeDestroyBonusContract, itemID, resolution.PVFPath)
	}

	remaining := count
	// PVF [stack limit] 同时是单行上限和该容器里这道具的总量上限：先算容器还能收多少，
	// 有现有行就只能并进去，没有再考虑空格。PVF 未声明上限（StackLimit==0）时
	// dnfrepo 返回哨兵额度，行为与以前一致。
	space := dnfrepo.ContainerItemSpace(items, listTypeMain, itemID, resolution.StackLimit)
	changed := make([]int16, 0, 2)
	markChanged := func(slot int16) {
		for _, existing := range changed {
			if existing == slot {
				return
			}
		}
		changed = append(changed, slot)
	}
	for slot := resolution.SlotStart; slot <= resolution.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		stack, found := items[key]
		if !found || stack.ItemID != itemID || stack.Count <= 0 {
			continue
		}
		_, stackLimit, stackable := o.stackContract(stack)
		if !stackable {
			continue
		}
		capacity := int64(math.MaxInt32) - stack.Count
		if stackLimit > 0 {
			capacity = stackLimit - stack.Count
		}
		if capacity > space {
			capacity = space
		}
		if capacity <= 0 {
			continue
		}
		add := remaining
		if add > capacity {
			add = capacity
		}
		stack = o.canonicalStackForMove(cloneStack(stack))
		stack.Count += add
		updateStackRawAmount(&stack)
		items[key] = stack
		remaining -= add
		space -= add
		markChanged(slot)
	}
	for slot := resolution.SlotStart; slot <= resolution.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(items, listTypeMain, itemID, resolution.StackLimit) {
			break
		}
		insert := remaining
		limit := resolution.StackLimit
		if limit == 0 {
			limit = math.MaxInt32
		}
		if insert > limit {
			insert = limit
		}
		if insert > space {
			insert = space
		}
		if insert <= 0 {
			break
		}
		bonusStack := o.canonicalStackForMove(dnfrepo.ItemStack{
			ItemID: itemID,
			Count:  insert,
			Extra:  map[string]string{"source": "upgrade_destroy_bonus"},
		})
		updateStackRawAmount(&bonusStack)
		items[key] = bonusStack
		remaining -= insert
		space -= insert
		markChanged(slot)
	}
	if remaining > 0 {
		return nil, fmt.Errorf("%w: item=%d remaining=%d slots=%d..%d", ErrUpgradeDestroyBonusInventoryFull, itemID, remaining, resolution.SlotStart, resolution.SlotEnd)
	}
	return changed, nil
}

// firstEmptyUpgradeSlot preserves the legacy bonus placement used only when
// an isolated test does not install the active runtime-PVF resolver. It
// searches quickbar 60..69 first, then general 9..59.
func firstEmptyUpgradeSlot(items map[string]dnfrepo.ItemStack) int16 {
	for slot := int16(60); slot <= 69; slot++ {
		if _, ok := items[slotKey(listTypeMain, slot)]; !ok {
			return slot
		}
	}
	for slot := int16(9); slot <= 59; slot++ {
		if _, ok := items[slotKey(listTypeMain, slot)]; !ok {
			return slot
		}
	}
	return -1
}

func upgradeGoldCostOf(stack dnfrepo.ItemStack, mode string, oldLevel byte) int64 {
	mode = strings.ToLower(strings.TrimSpace(mode))
	levelKey := fmt.Sprintf("%s_upgrade_gold_%d", mode, oldLevel)
	return firstExtraInt(
		stack.Extra,
		0,
		levelKey,
		mode+"_upgrade_gold",
		mode+"_gold_cost",
		"upgrade_gold_"+strconv.Itoa(int(oldLevel)),
		"upgrade_gold",
		"upgrade_gold_cost",
		"gold_cost",
	)
}

// The legacy constructor summary writes a u8 slot at +0 where the current 0x77
// row writes a u16, so every current offset reads one byte high on such a row:
// +0x0A lands on the low durability byte instead of the packed reinforce byte.
// Only the packed byte has a proved legacy position, at +9.
const (
	legacyRawEntryItemIDOffset = 0x01
	legacyRawEntryPackedOffset = 0x09
	legacyRawEntryMinSize      = 0x0C
)

// currentRawEntry returns the row only when it is a complete current 0x77 row,
// so a current offset is never read out of a legacy row.
func currentRawEntry(stack dnfrepo.ItemStack) ([]byte, bool) {
	if len(stack.RawEntry) != currentItemListEntrySize {
		return nil, false
	}
	return stack.RawEntry, true
}

// legacyRawEntry returns the row when it is still the legacy constructor
// summary. The layout is identified rather than assumed: the legacy row
// carries the PVF item id at +1, where the current row carries it at +2.
func legacyRawEntry(stack dnfrepo.ItemStack) ([]byte, bool) {
	if stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) {
		return nil, false
	}
	raw := stack.RawEntry
	if len(raw) < legacyRawEntryMinSize || len(raw) >= currentItemListEntrySize {
		return nil, false
	}
	if binary.LittleEndian.Uint32(raw[legacyRawEntryItemIDOffset:legacyRawEntryItemIDOffset+4]) != uint32(stack.ItemID) {
		return nil, false
	}
	return raw, true
}

func upgradeLevelOf(stack dnfrepo.ItemStack) byte {
	packed := packedUpgradeAttrOf(stack)
	if packed != 0 {
		return packed & 0x1F
	}
	if value := firstExtraInt(stack.Extra, 0, "reinforce", "upgrade_level", "enchant_upgrade", "enchant_upgrade_count"); value != 0 {
		return byte(value) & 0x1F
	}
	return 0
}

func packedUpgradeAttrOf(stack dnfrepo.ItemStack) byte {
	if stack.Extra != nil {
		if value := firstExtraInt(stack.Extra, 0, "ext_data0", "ext0", "packed_flag_byte", "packed_flag", "packed"); value != 0 {
			return byte(value)
		}
	}
	if raw, ok := currentRawEntry(stack); ok {
		return raw[0x0A]
	}
	if raw, ok := legacyRawEntry(stack); ok {
		return raw[legacyRawEntryPackedOffset]
	}
	return 0
}

func setUpgradeLevel(stack *dnfrepo.ItemStack, level byte) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
	}
	packed := (packedUpgradeAttrOf(*stack) & 0xE0) | (level & 0x1F)
	stack.Extra["ext_data0"] = strconv.Itoa(int(packed))
	stack.Extra["packed_flag_byte"] = strconv.Itoa(int(packed))
	stack.Extra["reinforce"] = strconv.Itoa(int(level))
	stack.Extra["upgrade_level"] = strconv.Itoa(int(level))
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		stack.RawEntry[0x0A] = packed
	}
}

func updateStackRawAmount(stack *dnfrepo.ItemStack) {
	if stack == nil {
		return
	}
	value := uint32(clampInt32(stack.Count))
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[0x06:0x0A], value)
	}
	if len(stack.Extra) == 0 {
		return
	}
	extra := make(map[string]string, len(stack.Extra))
	for key, raw := range stack.Extra {
		extra[key] = raw
	}
	changed := false
	for _, key := range []string{"amount_or_count", "amount", "count", "stack", "quantity"} {
		if _, exists := extra[key]; !exists {
			continue
		}
		extra[key] = strconv.FormatUint(uint64(value), 10)
		changed = true
	}
	if changed {
		stack.Extra = extra
	}
}

func upgradeAmplifyState(stack dnfrepo.ItemStack) (byte, int64) {
	ampType := byte(firstExtraInt(stack.Extra, 0, "amplify_type", "amplification_type", "amplify_attr", "amplification_attr", "byte_13", "value_13", "value_c"))
	ampValue := firstExtraInt(stack.Extra, 0, "amplify_value", "amplification_value", "amplify_bonus", "amplification_bonus", "marker_16", "marker16", "value_d")
	if raw, ok := currentRawEntry(stack); ok {
		if ampType == 0 {
			ampType = raw[0x13]
		}
		if ampValue == 0 {
			ampValue = int64(raw[0x14]) | int64(raw[0x15])<<8
		}
	}
	prefix := fixedExtraBytes(stack.Extra, 8, "prefix_data_0e", "prefix0e", "raw_data_0e")
	if ampType == 0 && len(prefix) >= 6 {
		ampType = prefix[5]
	}
	if ampValue == 0 && len(prefix) >= 8 {
		ampValue = int64(prefix[6]) | int64(prefix[7])<<8
	}
	return ampType, ampValue
}

func pvfProvenWasteStack(stack dnfrepo.ItemStack) (string, string, bool) {
	if stack.Extra == nil || !strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "stackable") {
		return "", "", false
	}
	pvfPath := strings.TrimSpace(stack.Extra["pvf_path"])
	stackableType := strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", ""))
	if pvfPath == "" || !strings.HasPrefix(strings.ToLower(stackableType), "[waste]") {
		return "", "", false
	}
	return pvfPath, stackableType, true
}

func pvfProvenExperienceStack(stack dnfrepo.ItemStack) (string, string, bool, bool) {
	if stack.Extra == nil || !strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "stackable") {
		return "", "", false, false
	}
	pvfPath := strings.TrimSpace(stack.Extra["pvf_path"])
	stackableType := strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", ""))
	normalizedType := strings.ToLower(stackableType)
	if pvfPath == "" || (normalizedType != "[waste]" && normalizedType != "[unlimited waste]") {
		return "", "", false, false
	}
	return pvfPath, stackableType, normalizedType == "[unlimited waste]", true
}

type deleteMutation struct {
	listType  byte
	slotIndex int16
	itemID    int32
	count     int64
}

func deleteMutations(cmd Command) []deleteMutation {
	if len(cmd.DeleteEntries) == 0 {
		return []deleteMutation{{
			listType:  cmd.SourceListType,
			slotIndex: cmd.SourceSlotIndex,
			itemID:    cmd.ItemTemplateID,
			count:     int64(cmd.Count),
		}}
	}
	out := make([]deleteMutation, 0, len(cmd.DeleteEntries))
	for _, entry := range cmd.DeleteEntries {
		out = append(out, deleteMutation{
			listType:  cmd.SourceListType,
			slotIndex: entry.SlotIndex,
			itemID:    entry.ItemID,
			count:     int64(entry.DeleteCount),
		})
	}
	return out
}

func checkDeleteListType(listType byte) error {
	if listType == listTypeEquipment {
		return fmt.Errorf("%w: %d", ErrDeleteRequiresEquipmentOwner, listType)
	}
	// 宠物栏（list 7）：宠物蛋/宠物消耗品这类**没有绑定宠物实体**的行允许直接删除，
	// 现场 2026-09-18「宠物蛋无法丢弃」就是被这里一刀切拦下的。绑定到宠物图鉴的行
	// （已孵化的宠物带 serial/handle）仍然必须走 typed pet owner —— 见 Delete 里的
	// stackHasLivePetBinding 判定，绕过它会留下孤儿宠物记录。
	if listType == listTypePet {
		return nil
	}
	return checkInventoryRemovalListType(listType)
}

// stackHasLivePetBinding 报告一行宠物栏道具是否绑定了一个宠物实体。孵化的宠物在
// 发放/孵化时会被写上 serial/handle（pet owner 与 inventory owner 都写这个键），
// 而宠物蛋只是一个道具行。
func stackHasLivePetBinding(stack dnfrepo.ItemStack) bool {
	for _, key := range []string{
		"creature_serial_or_handle", "creature_serial", "pet_serial", "serial", "handle",
	} {
		value := strings.TrimSpace(stack.Extra[key])
		if value != "" && value != "0" {
			return true
		}
	}
	return false
}

func checkInventoryRemovalListType(listType byte) error {
	switch listType {
	case listTypePet:
		return fmt.Errorf("%w: %d", ErrPetInventoryOwnerRequired, listType)
	case listTypeMain, listTypePersonalCargo, listTypeAvatar:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
	}
}

func checkMoveListType(listType byte) error {
	switch listType {
	case listTypeEquipment:
		return fmt.Errorf("%w: %d", ErrMoveRequiresEquipmentOwner, listType)
	case listTypeMain, listTypePersonalCargo, listTypeAvatar, listTypePet:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
	}
}

func isMainItemQuickSlot(listType byte, slot int16) bool {
	return listType == listTypeMain && slot >= 3 && slot <= 8
}

func moveTouchesMainQuickSlot(cmd Command) bool {
	return isMainItemQuickSlot(cmd.SourceListType, cmd.SourceSlotIndex) ||
		isMainItemQuickSlot(cmd.DestinationListType, cmd.DestinationSlotIndex)
}

func checkSortListType(listType byte) error {
	switch listType {
	case listTypeEquipment:
		return fmt.Errorf("%w: %d", ErrSortRequiresEquipmentOwner, listType)
	case listTypePet:
		return fmt.Errorf("%w: %d", ErrPetInventoryOwnerRequired, listType)
	case listTypeMain, listTypePersonalCargo, listTypeAvatar, listTypeAccountCargo:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
	}
}

func checkRepairListType(listType byte) error {
	switch listType {
	case listTypeEquipment:
		return fmt.Errorf("%w: %d", ErrRepairRequiresEquipmentOwner, listType)
	case listTypeMain, listTypePersonalCargo:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
	}
}

func checkUseStackableListType(listType byte) error {
	if listType == listTypeMain {
		return nil
	}
	return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
}

func checkUseStackableRequestListType(listType byte) error {
	if listType == listTypeMain || listType == listTypePet {
		return nil
	}
	return fmt.Errorf("%w: %d", ErrUnsupportedList, listType)
}

func sortSegment(listType byte, category byte) (int16, int16, bool) {
	switch listType {
	case listTypeMain:
		switch category {
		case 1:
			return 9, 64, true
		case 2:
			return 65, 120, true
		case 3:
			return 121, 176, true
		case 4:
			return 177, 232, true
		case 10:
			return 233, 288, true
		default:
			return 0, 0, false
		}
	case listTypePet:
		switch category {
		case 5:
			return 0, 139, true
		case 6:
			return 140, 188, true
		case 7:
			// Current NoPack sub_232A9F0 accepts list-7 pet-consumable
			// slots 189 through 238 inclusive.
			return 189, 238, true
		default:
			return 0, 0, false
		}
	case listTypeAvatar:
		if category == 8 {
			return 0, 209, true
		}
	case listTypePersonalCargo:
		if category == 11 {
			return 0, 151, true
		}
	}
	return 0, 0, false
}

// sortSegmentItems 按 kind/itemID/slot 重排段内未钉住的物品，返回参与整理的数量与是否
// 真的发生移动。角色容器与账号金库共用同一套规则，避免两条整理路径的结果不一致。
//
// A sort lock is deliberately independent from the ordinary equipment
// protection lock.  It anchors its current grid cell while the remaining
// entries are compacted around it; moving the lock together with the item
// would make the client's blue sort-lock marker look like it had been
// ignored by the Sort command.
func sortSegmentItems(items map[string]dnfrepo.ItemStack, listType byte, start int16, end int16) (int, bool, error) {
	candidates := sortCandidates(items, listType, start, end)
	targetSlots := sortTargetSlots(items, listType, start, end)
	if len(candidates) == 0 {
		return 0, false, nil
	}
	if len(candidates) > len(targetSlots) {
		return 0, false, fmt.Errorf("sort target slots unavailable: candidates=%d targets=%d", len(candidates), len(targetSlots))
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.kind != right.kind {
			return left.kind < right.kind
		}
		if left.stack.ItemID != right.stack.ItemID {
			return left.stack.ItemID < right.stack.ItemID
		}
		return left.slot < right.slot
	})

	changed := false
	for index, candidate := range candidates {
		if candidate.slot != targetSlots[index] {
			changed = true
			break
		}
	}
	if !changed {
		return len(candidates), false, nil
	}

	for _, candidate := range candidates {
		delete(items, slotKey(listType, candidate.slot))
	}
	for index, candidate := range candidates {
		items[slotKey(listType, targetSlots[index])] = candidate.stack
	}
	return len(candidates), true, nil
}

type sortCandidate struct {
	slot  int16
	kind  string
	stack dnfrepo.ItemStack
}

func sortCandidates(items map[string]dnfrepo.ItemStack, listType byte, start int16, end int16) []sortCandidate {
	out := make([]sortCandidate, 0)
	for key, stack := range items {
		keyListType, slot, ok := parseSlotKey(key)
		if !ok || keyListType != listType || slot < start || slot > end {
			continue
		}
		if dnfitemlock.IsSortLocked(stack) {
			continue
		}
		out = append(out, sortCandidate{
			slot:  slot,
			kind:  sortItemKind(stack),
			stack: stack,
		})
	}
	return out
}

// sortTargetSlots returns every mutable cell in the requested sort segment.
// Locked rows remain in the map and are therefore excluded from the compaction
// targets.  The result is naturally deterministic because slot order, rather
// than map iteration order, owns the destination layout.
func sortTargetSlots(items map[string]dnfrepo.ItemStack, listType byte, start int16, end int16) []int16 {
	targets := make([]int16, 0, int(end-start)+1)
	for slot := start; slot <= end; slot++ {
		stack, found := items[slotKey(listType, slot)]
		if found && dnfitemlock.IsSortLocked(stack) {
			continue
		}
		targets = append(targets, slot)
	}
	return targets
}

func parseSlotKey(key string) (byte, int16, bool) {
	listRaw, slotRaw, ok := strings.Cut(key, ":")
	if !ok {
		return 0, 0, false
	}
	listValue, err := strconv.ParseInt(strings.TrimSpace(listRaw), 10, 16)
	if err != nil || listValue < 0 || listValue > 255 {
		return 0, 0, false
	}
	slotValue, err := strconv.ParseInt(strings.TrimSpace(slotRaw), 10, 16)
	if err != nil {
		return 0, 0, false
	}
	return byte(listValue), int16(slotValue), true
}

func sortItemKind(stack dnfrepo.ItemStack) string {
	if stack.Extra == nil {
		return "unknown"
	}
	raw := strings.TrimSpace(stack.Extra["item_kind"])
	if raw == "" {
		raw = strings.TrimSpace(stack.Extra["kind"])
	}
	if raw == "" {
		return "unknown"
	}
	return strings.ToLower(raw)
}

func itemMapForList(record *dnfrepo.InventoryRecord, listType byte) (map[string]dnfrepo.ItemStack, dnfrepo.InventoryField) {
	if listType == listTypePersonalCargo {
		return record.Warehouse, dnfrepo.InventoryFieldWarehouse
	}
	return record.Slots, dnfrepo.InventoryFieldSlots
}

func slotKey(listType byte, slotIndex int16) string {
	return fmt.Sprintf("%d:%d", listType, slotIndex)
}

func normalizeDeleteCount(stackCount int64, requested int64) int64 {
	if stackCount <= 0 {
		return 0
	}
	if requested <= 0 || requested >= stackCount {
		return stackCount
	}
	return requested
}

func normalizeMoveCount(stack dnfrepo.ItemStack, requested int64) int64 {
	if stack.Count <= 0 {
		return 0
	}
	if !isStackable(stack) {
		return stack.Count
	}
	if requested <= 0 || requested >= stack.Count {
		return stack.Count
	}
	return requested
}

func (o *Owner) normalizeMoveCount(stack dnfrepo.ItemStack, requested int64) int64 {
	if stack.Count <= 0 {
		return 0
	}
	if !o.isStackable(stack) {
		return stack.Count
	}
	if requested <= 0 || requested >= stack.Count {
		return stack.Count
	}
	return requested
}

func canSplitStack(stack dnfrepo.ItemStack, moveCount int64) bool {
	return isStackable(stack) && moveCount > 0 && moveCount < stack.Count
}

func (o *Owner) canSplitStack(stack dnfrepo.ItemStack, moveCount int64) bool {
	return o.isStackable(stack) && moveCount > 0 && moveCount < stack.Count
}

func canStack(source dnfrepo.ItemStack, destination dnfrepo.ItemStack, moveCount int64) bool {
	if moveCount <= 0 || source.Count < moveCount || destination.Count <= 0 {
		return false
	}
	if source.ItemID != destination.ItemID || source.Bind != destination.Bind || !source.ExpireAt.Equal(destination.ExpireAt) {
		return false
	}
	if monsterCardUpgradeCountOf(source) != monsterCardUpgradeCountOf(destination) {
		return false
	}
	sourcePath, sourceLimit, sourceOK := pvfStackContract(source)
	destinationPath, destinationLimit, destinationOK := pvfStackContract(destination)
	if !sourceOK || !destinationOK || sourcePath != destinationPath || sourceLimit != destinationLimit {
		return false
	}
	if sourceLimit == 0 {
		// Runtime PVF uses an omitted [stack limit] as an uncapped stackable,
		// not as evidence that the item cannot stack. Current inventory rows
		// keep these counts in a signed 32-bit value.
		return moveCount <= math.MaxInt32 && destination.Count <= math.MaxInt32-moveCount
	}
	return destination.Count <= destinationLimit-moveCount
}

func (o *Owner) canStack(source dnfrepo.ItemStack, destination dnfrepo.ItemStack, moveCount int64) bool {
	if moveCount <= 0 || source.Count < moveCount || destination.Count <= 0 {
		return false
	}
	if source.ItemID != destination.ItemID || source.Bind != destination.Bind || !source.ExpireAt.Equal(destination.ExpireAt) {
		return false
	}
	if monsterCardUpgradeCountOf(source) != monsterCardUpgradeCountOf(destination) {
		return false
	}
	sourcePath, sourceLimit, sourceOK := o.stackContract(source)
	destinationPath, destinationLimit, destinationOK := o.stackContract(destination)
	if !sourceOK || !destinationOK || sourcePath != destinationPath || sourceLimit != destinationLimit {
		return false
	}
	if sourceLimit == 0 {
		return moveCount <= math.MaxInt32 && destination.Count <= math.MaxInt32-moveCount
	}
	return moveCount <= sourceLimit && destination.Count <= destinationLimit-moveCount
}

func isStackable(stack dnfrepo.ItemStack) bool {
	_, _, ok := pvfStackContract(stack)
	return ok
}

func (o *Owner) isStackable(stack dnfrepo.ItemStack) bool {
	_, _, ok := o.stackContract(stack)
	return ok
}

func (o *Owner) stackContract(stack dnfrepo.ItemStack) (string, int64, bool) {
	if o == nil || o.stackMerge == nil {
		return pvfStackContract(stack)
	}
	resolution, err := o.stackMerge(stack.ItemID)
	if err != nil || resolution.ItemID != stack.ItemID || resolution.StackLimit < 0 ||
		!strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") || stack.Count <= 0 {
		return "", 0, false
	}
	pvfPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.PVFPath, "\\", "/")))
	if !strings.HasPrefix(pvfPath, "stackable/") || strings.Contains(pvfPath, "../") {
		return "", 0, false
	}
	if resolution.StackLimit == 0 {
		if stack.Count > math.MaxInt32 {
			return "", 0, false
		}
		return pvfPath, 0, true
	}
	return pvfPath, resolution.StackLimit, true
}

func (o *Owner) canonicalStackForMove(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	if o == nil || o.stackMerge == nil || stack.ItemID <= 0 {
		return stack
	}
	resolution, err := o.stackMerge(stack.ItemID)
	if err != nil || resolution.ItemID != stack.ItemID || resolution.StackLimit < 0 ||
		!strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") {
		return stack
	}
	pvfPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.PVFPath, "\\", "/")))
	if !strings.HasPrefix(pvfPath, "stackable/") || strings.Contains(pvfPath, "../") {
		return stack
	}
	extra := make(map[string]string, len(stack.Extra)+4)
	for key, value := range stack.Extra {
		extra[key] = value
	}
	extra["item_kind"] = "stackable"
	extra["pvf_path"] = pvfPath
	if stackableType := strings.TrimSpace(resolution.StackableType); stackableType != "" {
		extra["stackable_type"] = stackableType
	} else {
		delete(extra, "stackable_type")
	}
	if resolution.StackLimit > 0 {
		extra["stack_limit"] = strconv.FormatInt(resolution.StackLimit, 10)
	} else {
		delete(extra, "stack_limit")
	}
	stack.Extra = extra
	return stack
}

func pvfStackContract(stack dnfrepo.ItemStack) (string, int64, bool) {
	if stack.ItemID <= 0 || stack.Extra == nil {
		return "", 0, false
	}
	if !strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "stackable") {
		return "", 0, false
	}
	pvfPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["pvf_path"], "\\", "/")))
	if !strings.HasPrefix(pvfPath, "stackable/") || strings.Contains(pvfPath, "../") {
		return "", 0, false
	}
	raw := strings.TrimSpace(stack.Extra["stack_limit"])
	if raw == "" {
		if stack.Count <= 0 || stack.Count > math.MaxInt32 {
			return "", 0, false
		}
		return pvfPath, 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || stack.Count <= 0 {
		return "", 0, false
	}
	if value == 0 {
		if stack.Count > math.MaxInt32 {
			return "", 0, false
		}
		return pvfPath, 0, true
	}
	if stack.Count > value {
		return "", 0, false
	}
	return pvfPath, value, true
}

func findCompatibleStackSlot(
	items map[string]dnfrepo.ItemStack,
	listType byte,
	moving dnfrepo.ItemStack,
	moveCount int64,
	excludedSlots ...int16,
) (int16, dnfrepo.ItemStack, bool) {
	if listType != listTypeMain && listType != listTypeAvatar && listType != listTypePersonalCargo {
		return 0, dnfrepo.ItemStack{}, false
	}
	excluded := make(map[int16]struct{}, len(excludedSlots))
	for _, slot := range excludedSlots {
		excluded[slot] = struct{}{}
	}
	var selectedSlot int16
	var selected dnfrepo.ItemStack
	found := false
	for key, candidate := range items {
		keyListType, slot, ok := parseSlotKey(key)
		if !ok || keyListType != listType {
			continue
		}
		if _, skip := excluded[slot]; skip || dnfrepo.IsAccountSharedInventorySlot(listType, slot) {
			continue
		}
		if !canStack(moving, candidate, moveCount) {
			continue
		}
		if !found || slot < selectedSlot {
			selectedSlot = slot
			selected = candidate
			found = true
		}
	}
	return selectedSlot, selected, found
}

func (o *Owner) findCompatibleStackSlot(
	items map[string]dnfrepo.ItemStack,
	listType byte,
	moving dnfrepo.ItemStack,
	moveCount int64,
	excludedSlots ...int16,
) (int16, dnfrepo.ItemStack, bool) {
	if listType != listTypeMain && listType != listTypeAvatar && listType != listTypePersonalCargo {
		return 0, dnfrepo.ItemStack{}, false
	}
	excluded := make(map[int16]struct{}, len(excludedSlots))
	for _, slot := range excludedSlots {
		excluded[slot] = struct{}{}
	}
	var selectedSlot int16
	var selected dnfrepo.ItemStack
	found := false
	for key, candidate := range items {
		keyListType, slot, ok := parseSlotKey(key)
		if !ok || keyListType != listType {
			continue
		}
		if _, skip := excluded[slot]; skip || dnfrepo.IsAccountSharedInventorySlot(listType, slot) {
			continue
		}
		candidate = o.canonicalStackForMove(candidate)
		if !o.canStack(moving, candidate, moveCount) {
			continue
		}
		if !found || slot < selectedSlot {
			selectedSlot = slot
			selected = candidate
			found = true
		}
	}
	return selectedSlot, selected, found
}

// crossContainerDestinationRedirect 是一次跨容器落点修正选出的真实目标格。
// Mode 沿用既有分支名：合并到已有堆叠用 "stack"（bridge 会据此补发两个端点的精确
// 槽位刷新），搬进空格用 "move"/"split"（只回 op19 ACK）。
type crossContainerDestinationRedirect struct {
	Slot  int16
	Stack dnfrepo.ItemStack
	Mode  string
}

// redirectMainMoveToItemPage 把"落点在主背包、但那一页不是这件道具的页"的移动改到
// 道具真正所属的那一页（当前 PVF 页的空格，或页里已有的同类可叠行）。
//
// 现场 2026-10-03（服主报，已用解密后的包日志定证）：徽章存进个人仓库再取出时，客户端
// 声明的落点是它自己挑的第一个空格 —— 消耗品页：
//
//	05:41:28  move src=(0,289 华丽的双重徽章 ×3) dst=(2,166)   存进仓库
//	05:41:31  move src=(2,166 ×3)              dst=(0,116)   ← 取出，落进消耗品栏
//
// 徽章页是 289..351（dnfbridge.dungeonDropStackableSlotRange），116 在消耗品页里，
// 于是这一摞徽章被写到了客户端从来不渲染它的那一页上 —— 玩家看到的就是"取出后跑到
// 消耗品栏"。26 秒后同一件道具又被写进 0:113，可见是稳定复现，不是偶发。
//
// 与 redirectAccountSharedWarehouseDestination（晶体/灵魂固定格）、
// redirectPersonalCargoMoveFromGoldWalletSlot（金币钱包行 0）同属一类：
// **op19 的落点不能由客户端单方面决定**，道具的 PVF 页才是权威。
//
// 刻意不接管的情形：
//   - 源端与声明格都在道具页里：那是页内重排，客户端没声明错，什么都不用做；
//   - 声明格被**别的**道具占着：那是玩家明着做的互换，语义不该被悄悄改写；
//   - 拿不到容器状态或 PVF 页（解析失败/页非法）：fail-close，不发明格子；
//   - 道具页里既没有可叠行也没有空格：不改落点，让调用方照旧报"空间不足"。
//
// 源端在错页、目标端也在错页的**同容器**移动同样接管：这样已经落在消耗品栏里的历史
// 脏行，玩家把它拖到任意一格就会自动回到徽章页，不用手工洗数据。源端本来就在正确
// 页里的同容器移动不受影响（那是页内重排，见上一条）。
//
// 返回的 bool 表示落点被改过，调用方据此把 RedirectedReservedMainSlot 写进结果，
// 让 op19 ACK 回执真正落库的槽位。
func (o *Owner) redirectMainMoveToItemPage(
	ctx context.Context,
	cmd Command,
	record dnfrepo.InventoryRecord,
	characterID string,
) (Command, bool, error) {
	if cmd.DestinationListType != listTypeMain ||
		dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		return cmd, false, nil
	}
	srcItems, _ := itemMapForList(&record, cmd.SourceListType)
	dstItems, _ := itemMapForList(&record, cmd.DestinationListType)
	source, sourceOK := srcItems[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !sourceOK || source.ItemID <= 0 || source.Count <= 0 {
		return cmd, false, nil
	}
	source = o.canonicalStackForMove(source)
	start, end, ok := o.moveDestinationUnlockedSlotRange(ctx, cmd.DestinationListType, source, characterID)
	if !ok {
		return cmd, false, nil
	}
	if cmd.SourceListType == listTypeMain &&
		cmd.SourceSlotIndex >= start && cmd.SourceSlotIndex <= end {
		// 同容器、源端本来就在正确页：页内重排，落点由玩家决定，不改。
		return cmd, false, nil
	}
	if cmd.DestinationSlotIndex >= start && cmd.DestinationSlotIndex <= end {
		return cmd, false, nil
	}
	if destination, occupied := dstItems[slotKey(cmd.DestinationListType, cmd.DestinationSlotIndex)]; occupied {
		destination = o.canonicalStackForMove(destination)
		moveCount := o.normalizeMoveCount(source, int64(cmd.MoveCount))
		if !o.canStack(source, destination, moveCount) {
			// 声明格上是别的道具：这是玩家在做一个互换，不是"客户端挑错了空格"。
			return cmd, false, nil
		}
	}
	excludeSlot := int16(-1)
	if cmd.SourceListType == cmd.DestinationListType {
		excludeSlot = cmd.SourceSlotIndex
	}
	moveCount := o.normalizeMoveCount(source, int64(cmd.MoveCount))
	// 先并进道具页里已有的同类行（同一容器里同 id 只应有一行），没有才另开一格。
	if slot, _, found := o.findCompatibleStackSlotInSlotRange(dstItems, cmd.DestinationListType, source, moveCount, start, end, excludeSlot); found {
		cmd.DestinationSlotIndex = slot
		return cmd, true, nil
	}
	if slot, found := emptySlotInSlotRange(dstItems, cmd.DestinationListType, start, end, excludeSlot); found {
		cmd.DestinationSlotIndex = slot
		return cmd, true, nil
	}
	return cmd, false, nil
}

// reservedMainWalletSlot 是客户端当作金币钱包对象的那一行；普通物品永远不能落在
// 那里（见 redirectPersonalCargoMoveFromGoldWalletSlot）。
const reservedMainWalletSlot int16 = 0

// 当前 EXE 主背包每个标准分类都是 56 格，前 32 格随角色解锁，之后按
// CharacterContainerState.MainSlotCount（0/8/16/24，即已购买的行数）逐行开放。
// 口径与 dnfbridge.currentMainInventoryBaseSlotCount /
// currentMainInventoryCategorySlotCount 一致；未解锁的格子客户端不渲染，物品放进去
// 等于"消失"，所以这里绝不猜格子。
const (
	mainInventoryBaseSlotCount     int16 = 32
	mainInventoryCategorySlotCount int16 = 56
)

// crossContainerClientSawEmptyDestination 判断这次 op19 是不是"跨容器 + 客户端声明
// 目标格为空"。客户端用 DestinationInstance / DestinationStack 回填它那一侧该格的
// 物品实例与数量：两者都是 0 才是"我这份列表里目标格没有东西"；DestinationStack
// 非 0 说明玩家是把物品丢在客户端看得见的一行上，那次合并必须继续就地发生。
func crossContainerClientSawEmptyDestination(cmd Command) bool {
	return cmd.SourceListType != cmd.DestinationListType &&
		cmd.DestinationInstance == 0 &&
		cmd.DestinationStack <= 0
}

// crossContainerOccupiedDestinationRedirect 为"客户端声明目标格为空、服务端该格
// 却有行"的跨容器移动重新选一个落点。返回 ok=false 表示没有可证明的落点，调用方
// 保持原有语义。
//
// 顺序按"同一容器里同 id 只应有一行"来定：声明格自己就是同类可叠格就地合并，
// 否则并进容器里另一处同类可叠格；只有容器里根本没有这件道具的行时才另开一格。
func (o *Owner) crossContainerOccupiedDestinationRedirect(
	ctx context.Context,
	cmd Command,
	destinationItems map[string]dnfrepo.ItemStack,
	destination dnfrepo.ItemStack,
	moving dnfrepo.ItemStack,
	moveCount int64,
	characterID string,
) (crossContainerDestinationRedirect, bool) {
	// 账号共享晶体/灵魂槽（354..365）属于另一个 durable owner，落点只能由账号侧
	// 事务决定；把这种移动改投到角色自有格子会改变归属，所以这里不接管。
	if dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		return crossContainerDestinationRedirect{}, false
	}
	// ① 声明格自己就是同类可叠格：容器里已经有这件道具的行，就地合并，绝不新开第二行。
	if o.canStack(moving, destination, moveCount) {
		if !crossContainerClientSawEmptyDestination(cmd) {
			// 客户端看得见这一行（DestinationInstance/DestinationStack 非 0）：就地合并
			// 交给调用方的常规分支，由它决定刷新形状，这里不接管落点。
			return crossContainerDestinationRedirect{}, false
		}
		// 客户端以为目标格是空的、服务端却有行：ACK 仍按它声明的槽位回，就地合并。
		return crossContainerDestinationRedirect{Slot: cmd.DestinationSlotIndex, Stack: destination, Mode: "stack"}, true
	}
	// ② 容器里另一处同类可叠格：同样并进去，不新开行。
	if slot, stack, ok := o.findCompatibleStackSlot(destinationItems, cmd.DestinationListType, moving, moveCount, cmd.DestinationSlotIndex); ok {
		return crossContainerDestinationRedirect{Slot: slot, Stack: stack, Mode: "stack"}, true
	}
	// ③ 叠不上就只能另开一格：目标容器里第一个已解锁空格。
	//
	// 2026-09-27 现场（用户报）：这一段以前整体被 crossContainerClientSawEmptyDestination
	// 挡在门外 —— 只要客户端在包体里带了它看见的那一行（DestinationInstance /
	// DestinationStack 非 0，也就是玩家正常"丢在占用格上"的手势），函数立刻 ok=false，
	// 调用方随即回"空间不足"（错误码 4）。于是两个方向都被挡下：
	//   - 金库里锁好的频道喇叭往背包拖（(2,143) → (0,69)/(0,92)/(0,111)），
	//   - 背包里的材料往金库拖（(0,124) → (2,150)，目标格放着消耗品），
	// 而目标容器其实还空着好几格；只有丢进同类堆（走 ① 的就地合并）才给动。
	// 跨容器本来就不允许互换，所以"叠不上"之后唯一的出路就是空格 —— 与 86JP
	// InventoryMoveService.ApplyCrossSpaceInsertToOccupiedDestination 的
	// TryFindEmptyDestinationSlot 一致（账号金库路径早已是这个形状）。
	if slot, ok := o.crossContainerEmptyDestinationSlot(ctx, destinationItems, cmd.DestinationListType, moving, characterID, cmd.DestinationSlotIndex); ok {
		return crossContainerDestinationRedirect{Slot: slot, Mode: "move"}, true
	}
	return crossContainerDestinationRedirect{}, false
}

// crossContainerEmptyDestinationSlot 返回目标容器里第一个"已解锁且确实为空"的格子，
// 跳过保留的金币钱包行、快捷栏、账号共享槽与已存在的行。
func (o *Owner) crossContainerEmptyDestinationSlot(
	ctx context.Context,
	destinationItems map[string]dnfrepo.ItemStack,
	listType byte,
	moving dnfrepo.ItemStack,
	characterID string,
	takenSlot int16,
) (int16, bool) {
	start, end, ok := o.moveDestinationUnlockedSlotRange(ctx, listType, moving, characterID)
	if !ok {
		return 0, false
	}
	return emptySlotInSlotRange(destinationItems, listType, start, end, takenSlot)
}

// emptySlotInSlotRange 在 [start,end] 里挑第一个空格，跳过保留的金币钱包行、快捷栏、
// 账号共享槽与已经存在的行。调用方必须给出"证明可见"的范围（已解锁主背包页 / 金库
// 容量），这里绝不猜格子。
func emptySlotInSlotRange(
	items map[string]dnfrepo.ItemStack,
	listType byte,
	start int16,
	end int16,
	takenSlot int16,
) (int16, bool) {
	for slot := start; slot <= end; slot++ {
		if slot == takenSlot || isMainItemQuickSlot(listType, slot) ||
			dnfrepo.IsAccountSharedInventorySlot(listType, slot) ||
			(listType == listTypeMain && slot == reservedMainWalletSlot) {
			continue
		}
		if _, occupied := items[slotKey(listType, slot)]; occupied {
			continue
		}
		return slot, true
	}
	return 0, false
}

// moveDestinationUnlockedSlotRange 给出目标容器里"该物品真正可见"的槽位范围。
// 读不到角色容器状态、主背包目标拿不到当前 PVF 的物品页、或容器类型无法界定范围时
// 一律 fail-close（ok=false），宁可不改落点，也绝不发明一个格子。
func (o *Owner) moveDestinationUnlockedSlotRange(
	ctx context.Context,
	listType byte,
	moving dnfrepo.ItemStack,
	characterID string,
) (int16, int16, bool) {
	if o == nil || o.settings == nil || strings.TrimSpace(characterID) == "" {
		return 0, 0, false
	}
	state, found, err := dnfrepo.LoadCharacterContainerState(ctx, o.settings, characterID)
	if err != nil || !found {
		return 0, 0, false
	}
	switch listType {
	case listTypePersonalCargo:
		if state.PersonalCargoSlotCount == 0 {
			return 0, 0, false
		}
		return 0, int16(state.PersonalCargoSlotCount) - 1, true
	case listTypeMain:
		if o.stackMerge == nil || moving.ItemID <= 0 {
			return 0, 0, false
		}
		resolution, resolveErr := o.stackMerge(moving.ItemID)
		if resolveErr != nil || resolution.ItemID != moving.ItemID ||
			resolution.SlotStart <= 0 || resolution.SlotEnd < resolution.SlotStart {
			return 0, 0, false
		}
		end := resolution.SlotEnd
		if resolution.SlotEnd-resolution.SlotStart+1 == mainInventoryCategorySlotCount {
			if unlocked := resolution.SlotStart + mainInventoryBaseSlotCount - 1 + int16(state.MainSlotCount); end > unlocked {
				end = unlocked
			}
		}
		return resolution.SlotStart, end, true
	default:
		return 0, 0, false
	}
}

// cargoLockedDestinationResolution 描述"客户端声明的落点在未解锁区"时按运营口径解析出来的落点。
// Locked 表示落点确实在未解锁区；Redirected 表示已改到另一个已解锁格（Slot）；
// StackFound 表示那个格子上已经有同类可叠行（Stack），必须并进去而不是当成空格覆盖。
type cargoLockedDestinationResolution struct {
	Locked     bool
	Redirected bool
	Slot       int16
	Stack      dnfrepo.ItemStack
	StackFound bool
}

// isPersonalCargoListType 判断容器是不是有解锁档位、且客户端允许往未解锁格拖入的个人仓库。
func isPersonalCargoListType(listType byte) bool {
	return listType == listTypePersonalCargo
}

// isWarehouseDestination 报告一次移动的**落点**是不是仓库类容器：个人仓库(list 2)、
// 账号金库(list 12)，以及"客户端把 list-0 的 354..365 当账号共享晶体/灵魂仓"那一段。
// 快捷栏、宠物页、装扮页都不是仓库，不在此列。
func isWarehouseDestination(listType byte, slot int16) bool {
	switch listType {
	case listTypePersonalCargo, listTypeAccountCargo:
		return true
	case listTypeMain:
		return dnfrepo.IsAccountSharedInventorySlot(listType, slot)
	default:
		return false
	}
}

// isAvatarEmblem 用当前运行时 PVF 判定这一行是不是徽章（[stackable type] = [avatar emblem]）。
//
// 判定走解析器而不是留存的 Extra：PVF 是唯一权威，Extra 可能来自历史数据或被改过。
// 解析不出来一律当作"不是徽章"（fail-open）——这条规则是**新增限制**，误判成徽章会把
// 普通道具也挡在仓库外，代价比漏拦一件大。
func (o *Owner) isAvatarEmblem(stack dnfrepo.ItemStack) bool {
	if o == nil || o.stackMerge == nil || stack.ItemID <= 0 {
		return false
	}
	resolution, err := o.stackMerge(stack.ItemID)
	if err != nil || resolution.ItemID != stack.ItemID {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")))
	return normalized == "[avatar emblem]"
}

// resolveLockedPersonalCargoDestination 按运营口径（2026-09-18）处理"落点在未解锁的个人仓库格"：
//
//	① 已解锁区里已经有同 ID 的可叠行（含 PVF [stack limit] 校验）→ 并进最早的那一行；
//	② 否则落到已解锁区第一个空格（从第一格开始找，占了的往后顺延）；
//	③ 都没有（真的满了）→ Redirected=false 且 Locked=true，调用方回"物品栏空间不足"。
//
// listType/slot 是**真正接收物品**的那一端：正向移动是 destination；而客户端在金库/仓库
// 内部把已有物品拖到某个格子时，会把落点写在 source 端（服务端看到的是 reverse_move），
// 所以 reverse 分支必须传 source 端（现场 2026-09-18：那个分支没判定，物品直接进了未解锁格）。
// excludeSlot 用来排除本次移动的另一端（同容器时就是物品原来所在的那一格），避免把这一堆
// 放回它自己身上。
//
// 读不到解锁范围（没有容器状态/PVF 页信息）时 fail-close：Locked=false，完全不改落点，
// 也不发明格子。
func (o *Owner) resolveLockedPersonalCargoDestination(
	ctx context.Context,
	listType byte,
	slot int16,
	items map[string]dnfrepo.ItemStack,
	moving dnfrepo.ItemStack,
	moveCount int64,
	excludeSlot int16,
	characterID string,
) cargoLockedDestinationResolution {
	if o == nil || !isPersonalCargoListType(listType) ||
		moving.ItemID <= 0 || moving.Count <= 0 || moveCount <= 0 {
		return cargoLockedDestinationResolution{}
	}
	start, end, ok := o.moveDestinationUnlockedSlotRange(ctx, listType, moving, characterID)
	if !ok {
		return cargoLockedDestinationResolution{}
	}
	if slot >= start && slot <= end {
		return cargoLockedDestinationResolution{}
	}
	resolution := cargoLockedDestinationResolution{Locked: true}
	// ① 同类可叠行优先：同一个容器里同 ID 只应有一行。
	if target, stack, found := o.findCompatibleStackSlotInSlotRange(
		items,
		listType,
		moving,
		moveCount,
		start,
		end,
		excludeSlot,
	); found {
		resolution.Slot, resolution.Redirected = target, true
		resolution.Stack, resolution.StackFound = stack, true
		return resolution
	}
	// ② 第一个已解锁空格。
	if target, found := emptySlotInSlotRange(items, listType, start, end, excludeSlot); found {
		resolution.Slot, resolution.Redirected = target, true
	}
	return resolution
}

// findCompatibleStackSlotInSlotRange 在 [start,end] 里挑**编号最小**的同类可叠格；
// 与 findCompatibleStackSlot 的区别只有一个：范围由调用方按"已解锁"证明给出。
// excludeSlot < 0 表示不排除任何格子。
func (o *Owner) findCompatibleStackSlotInSlotRange(
	items map[string]dnfrepo.ItemStack,
	listType byte,
	moving dnfrepo.ItemStack,
	moveCount int64,
	start int16,
	end int16,
	excludeSlot int16,
) (int16, dnfrepo.ItemStack, bool) {
	for slot := start; slot <= end; slot++ {
		if slot == excludeSlot ||
			dnfrepo.IsAccountSharedInventorySlot(listType, slot) ||
			(listType == listTypeMain && slot == reservedMainWalletSlot) {
			continue
		}
		candidate, exists := items[slotKey(listType, slot)]
		if !exists || candidate.ItemID != moving.ItemID {
			continue
		}
		candidate = o.canonicalStackForMove(candidate)
		if o.canStack(moving, candidate, moveCount) {
			return slot, candidate, true
		}
	}
	return 0, dnfrepo.ItemStack{}, false
}

// carryLimitForContainer 取出该道具在"一个容器内"的总量上限（PVF [stack limit]）。
// ok=false 表示不受限：PVF 没写 [stack limit]（装备、部分老材料）或该物品不是可叠加
// 类别。limit==0 同样是"PVF 省略"，按不限处理。
func (o *Owner) carryLimitForContainer(stack dnfrepo.ItemStack) (int64, bool) {
	_, limit, ok := o.stackContract(stack)
	if !ok || limit <= 0 {
		return 0, false
	}
	return limit, true
}

// containerItemTotal 统计某个容器里同一道具 id 的总量。整理锁、绑定/期限不同的行都算
// 在一起：口径是"这道具在这个容器里一共有多少件"。实现见 dnfrepo，模块内所有入包路径
// 共用同一份口径。
func containerItemTotal(items map[string]dnfrepo.ItemStack, listType byte, itemID int64) int64 {
	return dnfrepo.ContainerItemTotal(items, listType, itemID)
}

// validateCarryLimitOnMove 在跨容器搬入之前检查"目标容器里该道具总量 + 本次搬入量"是否
// 超过 PVF [stack limit]。超了就拒绝：不能靠另寻空格绕过去（上限 1 的道具每容器只能有
// 一件，上限 1000 的道具每容器总共只能有 1000 件）。同容器移动总量不变，直接放行。
func (o *Owner) validateCarryLimitOnMove(
	sourceListType byte,
	destinationListType byte,
	destinationSlotIndex int16,
	destinationItems map[string]dnfrepo.ItemStack,
	moving dnfrepo.ItemStack,
	moveCount int64,
) error {
	if moveCount <= 0 || sourceListType == destinationListType {
		return nil
	}
	limit, capped := o.carryLimitForContainer(moving)
	if !capped {
		return nil
	}
	total := containerItemTotal(destinationItems, destinationListType, moving.ItemID)
	if total > limit-moveCount {
		return fmt.Errorf(
			"%w: item=%d list=%d slot=%d container_total=%d move=%d limit=%d",
			ErrMoveCarryLimitExceeded,
			moving.ItemID,
			destinationListType,
			destinationSlotIndex,
			total,
			moveCount,
			limit,
		)
	}
	return nil
}

func (o *Owner) withMoveRefresh(
	ctx context.Context,
	cmd Command,
	result MoveResult,
	record *dnfrepo.InventoryRecord,
	listTypes ...byte,
) (MoveResult, error) {
	if record == nil {
		return result, nil
	}
	if result.Refresh == nil {
		result.Refresh = make(map[byte]map[string]dnfrepo.ItemStack, len(listTypes))
	}
	for _, listType := range listTypes {
		if listType != listTypeMain && listType != listTypeAvatar && listType != listTypePersonalCargo {
			continue
		}
		if _, exists := result.Refresh[listType]; exists {
			continue
		}
		items, _ := itemMapForList(record, listType)
		snapshot := cloneItemMap(items)
		if listType == listTypeMain {
			if o == nil || o.accountRepo == nil {
				return MoveResult{}, ErrAccountSharedOwnerUnavailable
			}
			accountID := strings.TrimSpace(cmd.AccountID)
			if accountID == "" {
				return MoveResult{}, ErrAccountRequired
			}
			account, _, err := o.accountRepo.Load(ctx, accountID)
			if err != nil {
				return MoveResult{}, err
			}
			merged := dnfrepo.MergeAccountSharedInventory(dnfrepo.InventoryRecord{Slots: snapshot}, account)
			snapshot = merged.Slots
		}
		result.Refresh[listType] = snapshot
		result.RefreshListTypes = append(result.RefreshListTypes, listType)
	}
	return result, nil
}

func isEquipmentLocked(stack dnfrepo.ItemStack) bool {
	if stack.Extra == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Extra["equipment_lock_state"])) {
	case "1", "2", "active", "locked", "unlocking", "pending_unlock":
		return true
	default:
		return false
	}
}

func cloneStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if len(stack.Extra) == 0 {
		return stack
	}
	extra := make(map[string]string, len(stack.Extra))
	for key, value := range stack.Extra {
		extra[key] = value
	}
	stack.Extra = extra
	return stack
}

func cloneItemMap(items map[string]dnfrepo.ItemStack) map[string]dnfrepo.ItemStack {
	if len(items) == 0 {
		return map[string]dnfrepo.ItemStack{}
	}
	out := make(map[string]dnfrepo.ItemStack, len(items))
	for key, stack := range items {
		out[key] = cloneStack(stack)
	}
	return out
}

func saveMoveResult(ctx context.Context, repo dnfrepo.InventoryRepository, record dnfrepo.InventoryRecord, srcField dnfrepo.InventoryField, dstField dnfrepo.InventoryField, result MoveResult) (MoveResult, error) {
	fields := []dnfrepo.InventoryField{srcField}
	if dstField != srcField {
		fields = append(fields, dstField)
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, repo, record, fields...); err != nil {
		return MoveResult{}, err
	}
	result.Changed = true
	return result, nil
}

func sellGoldOf(stack dnfrepo.ItemStack) (int64, error) {
	if stack.Extra == nil {
		return 0, ErrSellPriceMissing
	}
	raw, ok := stack.Extra["sell_gold"]
	if !ok {
		return 0, ErrSellPriceMissing
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%w: %q", ErrSellPriceInvalid, raw)
	}
	return value, nil
}

func repairDurabilityOf(stack dnfrepo.ItemStack) (int64, int64, error) {
	if stack.Extra == nil {
		return 0, 0, ErrRepairDurabilityMissing
	}
	currentRaw := strings.TrimSpace(stack.Extra["durability"])
	maxRaw := strings.TrimSpace(stack.Extra["max_durability"])
	if currentRaw == "" || maxRaw == "" {
		return 0, 0, ErrRepairDurabilityMissing
	}
	current, err := strconv.ParseInt(currentRaw, 10, 64)
	if err != nil || current < 0 {
		return 0, 0, fmt.Errorf("%w: durability=%q", ErrRepairDurabilityInvalid, currentRaw)
	}
	maxValue, err := strconv.ParseInt(maxRaw, 10, 64)
	if err != nil || maxValue <= 0 || current > maxValue {
		return 0, 0, fmt.Errorf("%w: max_durability=%q durability=%q", ErrRepairDurabilityInvalid, maxRaw, currentRaw)
	}
	return current, maxValue, nil
}

// Upgrade-ticket (op50 ticket scene) client-visible error codes, matching the
// current packet regressions: 4 invalid target, 7 durability, 13 unsupported,
// 19 restriction/forbidden, 22 invalid material, 23 wrong mode, 95 max level,
// 174 amplify not identified, 213 item locked.
const (
	upgradeTicketErrorInvalidTarget        byte = 4
	upgradeTicketErrorDurability           byte = 7
	upgradeTicketErrorUnsupported          byte = 13
	upgradeTicketErrorForbidden            byte = 19
	upgradeTicketErrorInvalidMaterial      byte = 22
	upgradeTicketErrorWrongMode            byte = 23
	upgradeTicketErrorMaxLevel             byte = 95
	upgradeTicketErrorAmplifyNotIdentified byte = 174
	upgradeTicketErrorLocked               byte = 213
	upgradeTicketResultCodeSuccess         byte = 0
	upgradeTicketResultCodeFailureRetain   byte = 1
)

// UpgradeTicketResult describes one op50 ticket-scene attempt. Success=true
// means the operation completed and the success-layout op50 ACK must be sent
// (ResultCode carries the roll outcome); TicketResolved=false means the
// material is not an upgrade ticket and the request stays on the pending
// normal-reinforcement path.
type UpgradeTicketResult struct {
	CharacterID                 string
	Success                     bool
	TicketResolved              bool
	ErrorCode                   byte
	Mode                        string
	TargetListType              byte
	TargetSlotIndex             int16
	MaterialSlotIndex           int16
	TicketItemID                int64
	OldLevel                    byte
	NewLevel                    byte
	ResultCode                  byte
	UpgradeSucceeded            bool
	MaterialRemainingStackCount int64
	MainRefresh                 map[string]dnfrepo.ItemStack
	TargetUpdatedStack          dnfrepo.ItemStack
	MaterialUpdated             bool
	MaterialUpdatedStack        dnfrepo.ItemStack
	Changed                     bool
}

// UpgradeTicket applies one equipment reinforcement/amplify ticket (op50 with
// the ticket in the material slot) to one main-inventory equipment entry,
// following the current op50 ticket-scene regressions: on a successful
// roll the level jumps directly to the ticket's target level, on failure the
// level is retained without penalty; exactly one ticket is consumed either
// way. [enchant random] tickets validate their er_slot / er_grade / er_seal
// target restrictions and then roll one weighted er_enchant candidate instead
// of using a fixed target level. A direct-level ticket is already
// non-destructive, so its unrelated optional protection slot is ignored.
// Normal op50 reinforcement/amplification owns protection tickets separately.
// Gold cost and the 0x0056 notice broadcast are intentionally not owned by
// this direct-level ticket path.
func (o *Owner) UpgradeTicket(ctx context.Context, cmd Command, resolver alignedcmd.UpgradeTicketResolver) (UpgradeTicketResult, error) {
	base := UpgradeTicketResult{
		TicketResolved:    true,
		ErrorCode:         upgradeTicketErrorInvalidTarget,
		Mode:              cmd.Mode,
		TargetListType:    cmd.TargetListType,
		TargetSlotIndex:   cmd.TargetSlotIndex,
		MaterialSlotIndex: cmd.MaterialSlotIndex,
	}
	if o == nil || o.repo == nil {
		return UpgradeTicketResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UpgradeTicketResult{}, ErrCharacterRequired
	}
	if resolver == nil {
		return UpgradeTicketResult{}, ErrUpgradeTicketResolverRequired
	}
	if cmd.Mode != "reinforce" && cmd.Mode != "amplify" {
		base.ErrorCode = upgradeTicketErrorWrongMode
		return base, nil
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		var result UpgradeTicketResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.UpgradeTicket(ctx, cmd, resolver)
			return err
		})
		if err != nil {
			return UpgradeTicketResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UpgradeTicketResult{}, err
	}
	if !ok {
		return UpgradeTicketResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}

	result := base
	items := record.Slots
	// Same worn-target resolution as Upgrade: read the EquipmentEntry through
	// the forging projection, let the shared level logic run, write it back.
	targetEquipped := cmd.TargetListType == listTypeEquipment
	var equipmentRecord dnfrepo.EquipmentRecord
	var targetEntry dnfrepo.EquipmentEntry
	equipmentKey := strconv.Itoa(int(cmd.TargetSlotIndex))
	targetKey := slotKey(cmd.TargetListType, cmd.TargetSlotIndex)
	var target dnfrepo.ItemStack
	switch {
	case targetEquipped:
		if o.equipment == nil {
			return UpgradeTicketResult{}, ErrUpgradeEquipmentOwnerUnavailable
		}
		var found bool
		equipmentRecord, found, err = o.equipment.Load(ctx, characterID)
		if err != nil {
			return UpgradeTicketResult{}, err
		}
		if !found {
			return result, nil
		}
		equipmentRecord = dnfrepo.CloneEquipment(equipmentRecord)
		targetEntry, ok = equipmentRecord.Entries[equipmentKey]
		if !ok || targetEntry.ItemID <= 0 || targetEntry.SlotIndex != cmd.TargetSlotIndex {
			return result, nil
		}
		target = forgingStackFromEquipmentEntry(targetEntry)
	case cmd.TargetListType == listTypeMain:
		target, ok = items[targetKey]
	default:
		// A target list this ticket scene cannot serve (the list-38 公会徽章 page)
		// must hand the request back instead of claiming it: base carries
		// TicketResolved=true, and the handler's ticket pre-pass reads that as
		// "a ticket owns this attempt" and answers the op50 error ACK. Leaving it
		// true here made every 公会徽章 reinforce die on error 4 (invalid target)
		// before the ported list-38 owner path could run (live 2026-10-04 19:16:30
		// and 19:17:16). TicketResolved=false keeps the request on the normal
		// reinforce/amplify path.
		result.TicketResolved = false
		return result, nil
	}
	if !ok || target.Count <= 0 || target.ItemID != int64(cmd.TargetItemTemplateID) {
		return result, nil
	}
	if !equipmentStackCountIsSingle(target) {
		// 历史堆叠行（装备 Count>1）不允许用券：一张券只能作用一件。
		return result, nil
	}
	result.OldLevel = upgradeLevelOf(target)
	result.NewLevel = result.OldLevel
	if isEquipmentLocked(target) {
		result.ErrorCode = upgradeTicketErrorLocked
		return result, nil
	}
	if !upgradeDurabilityOK(target) {
		result.ErrorCode = upgradeTicketErrorDurability
		return result, nil
	}
	if result.OldLevel > 30 {
		result.ErrorCode = upgradeTicketErrorMaxLevel
		return result, nil
	}
	if cmd.MaterialSlotIndex < 0 || cmd.MaterialSlotIndex == cmd.TargetSlotIndex {
		result.ErrorCode = upgradeTicketErrorInvalidMaterial
		return result, nil
	}
	materialKey := slotKey(listTypeMain, cmd.MaterialSlotIndex)
	material, ok := items[materialKey]
	if !ok || material.Count <= 0 {
		result.ErrorCode = upgradeTicketErrorInvalidMaterial
		return result, nil
	}
	result.TicketItemID = material.ItemID

	resolution, err := resolver(material.ItemID, target.ItemID)
	if err != nil {
		return UpgradeTicketResult{}, err
	}
	if resolution.TicketMode == "" {
		result.TicketResolved = false
		return result, nil
	}
	result.TicketResolved = true
	if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") {
		result.ErrorCode = upgradeTicketErrorInvalidTarget
		return result, nil
	}
	if resolution.TargetUpgradeForbidden {
		result.ErrorCode = upgradeTicketErrorForbidden
		return result, nil
	}
	if !strings.EqualFold(strings.TrimSpace(resolution.TicketMode), cmd.Mode) {
		result.ErrorCode = upgradeTicketErrorWrongMode
		return result, nil
	}
	amplifyType, amplifyValue := upgradeAmplifyState(target)
	switch cmd.Mode {
	case "reinforce":
		if amplifyType != 0 {
			result.ErrorCode = upgradeTicketErrorWrongMode
			return result, nil
		}
	case "amplify":
		if amplifyType == 0 {
			result.ErrorCode = upgradeTicketErrorWrongMode
			return result, nil
		}
		if amplifyValue <= 0 || (amplifyType&0x80) != 0 {
			result.ErrorCode = upgradeTicketErrorAmplifyNotIdentified
			return result, nil
		}
	}

	weight := resolution.SuccessWeight
	targetLevel := resolution.TargetLevel
	if resolution.TicketRandom {
		if !enchantRandomRestrictionAllows(resolution.RandomTicket, resolution.TargetEquipmentType, resolution.TargetRarity, target) {
			result.ErrorCode = upgradeTicketErrorForbidden
			return result, nil
		}
		entry, ok := o.selectEnchantRandomEntry(resolution.RandomTicket.Entries)
		if !ok {
			result.ErrorCode = upgradeTicketErrorUnsupported
			return result, nil
		}
		weight = entry.Weight
		targetLevel = entry.TargetLevel
	}
	if weight < 0 {
		weight = 0
	}
	if weight > 100000 {
		weight = 100000
	}
	roll := int64(rand.Intn(100000))
	if roll < weight {
		newLevel := targetLevel
		if newLevel < 0 {
			newLevel = 0
		}
		if newLevel > 31 {
			newLevel = 31
		}
		target = cloneStack(target)
		setUpgradeLevel(&target, byte(newLevel))
		items[targetKey] = target
		result.NewLevel = byte(newLevel)
		result.UpgradeSucceeded = true
		result.ResultCode = upgradeTicketResultCodeSuccess
	} else {
		result.ResultCode = upgradeTicketResultCodeFailureRetain
	}

	material = cloneStack(material)
	material.Count--
	result.MaterialRemainingStackCount = material.Count
	result.MaterialUpdated = true
	if material.Count <= 0 {
		delete(items, materialKey)
		result.MaterialRemainingStackCount = 0
		result.MaterialUpdatedStack = dnfrepo.ItemStack{}
	} else {
		updateStackRawAmount(&material)
		items[materialKey] = material
		result.MaterialUpdatedStack = material
	}

	if targetEquipped {
		// The shared level logic wrote the mutated stack under the list-3 key of
		// the backpack map. That key must never be persisted there; move the
		// raw row and extra mirrors back into the worn entry instead.
		if updatedTarget, changed := items[targetKey]; changed {
			delete(items, targetKey)
			targetEntry.RawEntry = append([]byte(nil), updatedTarget.RawEntry...)
			targetEntry.Extra = cloneStringMapForForging(updatedTarget.Extra)
			equipmentRecord.Entries[equipmentKey] = targetEntry
			target = updatedTarget
		}
	}
	now := time.Now()
	record.UpdatedAt = now
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return UpgradeTicketResult{}, err
	}
	if targetEquipped {
		equipmentRecord.CharacterID = characterID
		equipmentRecord.UpdatedAt = now
		if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
			return UpgradeTicketResult{}, err
		}
	}
	result.Success = true
	result.ErrorCode = 0
	if targetEquipped {
		result.TargetUpdatedStack = cloneStack(target)
	} else if updatedTarget, ok := items[targetKey]; ok {
		result.TargetUpdatedStack = updatedTarget
	} else {
		result.TargetUpdatedStack = dnfrepo.ItemStack{}
	}
	result.MainRefresh = cloneItemMap(record.Slots)
	result.Changed = true
	return result, nil
}
