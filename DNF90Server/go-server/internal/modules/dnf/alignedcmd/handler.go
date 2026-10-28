// 本文件定义 DNF 已对齐命令的模块化分流结果。
// 这里只承载协议分流和回包描述，不持有具体玩法状态。
package alignedcmd

import (
	"context"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

// Request 是 dnfbridge 交给 DNF 业务模块的最小上下文。
// Body 是当前包体副本，模块不得在原地修改调用方缓冲区。
type Request struct {
	Command                     byte
	CommandKnown                bool
	Opcode                      uint16
	Body                        []byte
	AccountID                   string
	SelectedCharacterID         uint16
	DojoSkills                  bool
	Repositories                dnfrepo.Group
	EquipmentPlacement          EquipmentPlacementValidator
	GuardianShield              GuardianShieldResolver
	AvatarClassifier            AvatarClassifier
	AuroraLookReplaceClassifier AuroraLookReplaceClassifier
	StackMergeResolver          StackMergeResolver
	// AccountWarehouseSlotResolver 给出"这类道具的固定晶体/灵魂仓库格"（六个晶块
	// 354..359、六个灵魂 360..365）。op19 用它把落点声明成角色背包的晶体/灵魂改回固定
	// 仓库格；没有固定格的道具返回 false，调用方一律保持客户端声明的落点。
	AccountWarehouseSlotResolver func(itemID int64) (int16, bool)
	// ItemCooltimeGroupResolver 返回某件道具的 PVF `[cooltime group]`（0 = 无组）。
	// op44 的冷却检查用它判断"已存在的冷却行是否与本次使用同组"，所以它必须能解析
	// **已经在角色冷却表里的**item id，而不是只有本次使用的那一件。
	ItemCooltimeGroupResolver func(itemID int64) int64
	// StackMergeResolverError preserves the active-PVF catalog failure that
	// prevented dnfbridge from constructing StackMergeResolver. Domain handlers
	// use it only where silently falling back would make a mutation unsafe.
	StackMergeResolverError       error
	NameTagChecker                func(itemID uint32) bool
	PetHatchResolver              PetHatchResolver
	EnchantBeadResolver           EnchantBeadResolver
	MonsterCardUpgradeResolver    MonsterCardUpgradeResolver
	MonsterCardUpgradePolicy      MonsterCardUpgradePolicy
	AmplifyItemResolver           AmplifyItemResolver
	RandomOptionResolver          RandomOptionResolver
	EquipmentRegenerationResolver EquipmentRegenerationResolver
	UpgradeTicketResolver         UpgradeTicketResolver
	UpgradeProtectionResolver     UpgradeProtectionResolver
	UpgradePolicyResolver         UpgradePolicyResolver
	UpgradeRefundMailer           UpgradeRefundMailer
	ForgingResolver               ForgingResolver
	ChronicleRefineResolver       ChronicleRefineResolver
	MagicBoxResolver              MagicBoxResolver
	MagicBoxRewardItemResolver    MagicBoxRewardItemResolver
	MagicBoxContractActivator     MagicBoxContractActivator
	RandomRewardItemResolver      RandomRewardItemResolver
	PremiumContractResolver       PremiumContractResolver
	ExperienceConsumableResolver  ExperienceConsumableResolver
	StackableUseResolver          StackableUseResolver
	DirectExperiencePlanner       DirectExperienceConsumablePlanner
	QuestCompletionTicketPlanner  QuestCompletionTicketPlanner
	StoryBookPlanner              StoryBookPlanner
	ChronicleGrowthResolver       ChronicleGrowthResolver
	InDungeon                     bool
	DungeonID                     int64
	// ConsumableFree marks a sandbox run (修炼道场) whose consumables are usable
	// but never deducted: stackable use still runs for its effect, the owner just
	// leaves the stack count alone.
	ConsumableFree      bool
	DamageFontResolver  DamageFontResolver
	DamageFontNow       time.Time
	FatiguePotionNow    time.Time
	RentalPointNow      time.Time
	RepairCostResolver  RepairCostResolver
	MailboxItemResolver MailboxItemResolver
	SkillCatalog        *dnfskill.Table
	InitialSkillLevels  map[uint16]int
	SkillPointBaseline  *dnfrepo.SkillPointState
	Party               *PartyState
}

// EquipmentPlacementRequest is the dependency-neutral bridge contract used
// by the inventory module before an item enters a worn equipment slot.
type EquipmentPlacementRequest struct {
	CharacterID     string
	ItemID          int64
	SourceListType  byte
	SourceSlotIndex int16
	TargetSlotIndex int16
}

// EquipmentPlacementValidator keeps current-PVF access in dnfbridge while
// allowing the equipment owner to enforce the check inside its item UOW.
type EquipmentPlacementValidator func(context.Context, EquipmentPlacementRequest) error

// GuardianShieldResolution is the active-PVF proof for one item admitted to
// the Guardian's five-slot shield deck. The bridge resolves archive metadata;
// the domain owner persists only this typed result inside its character-asset
// transaction.
type GuardianShieldResolution struct {
	ItemID        uint32
	PVFPath       string
	EquipmentType string
	UnlockKind    GuardianShieldUnlockKind
	ClearQuestID  int64
	MinimumLevel  int64
}

type GuardianShieldUnlockKind byte

const (
	GuardianShieldUnlockUnknown GuardianShieldUnlockKind = iota
	GuardianShieldUnlockQuest
	GuardianShieldUnlockLevel
)

// GuardianShieldResolver validates shieldwindownewdata membership and its
// quest/level authorization for the character's normalized grow type together
// with the concrete equipment file ([support weapon], usable knight, shield
// group and shield path).
type GuardianShieldResolver func(context.Context, uint32, byte) (GuardianShieldResolution, error)

// AvatarClassifier resolves an equipment template through the active PVF.
// isAvatar is true only for actor appearance slots; isClone additionally
// requires the PVF [item category] value `clear avatar`.
type AvatarClassifier func(itemID uint32) (isAvatar bool, isClone bool)

// AuroraLookReplaceClassifier resolves an equipment template through the active
// PVF and is true only for a 转职光环 -- an `[aurora avatar]` that is also a
// `clear avatar` item. A 转职光环 is therefore a clone-category item as well,
// but its semantics are the opposite of 透明克隆时装: it replaces the worn
// aura's appearance instead of borrowing it, and the covered aura is only the
// stat/link source. A nil classifier keeps the legacy clone-only behaviour.
type AuroraLookReplaceClassifier func(itemID uint32) bool

// StackMergeResolution is the active-runtime-PVF contract used when two
// durable item rows are split or merged. Inventory metadata is deliberately
// not authoritative here: old saves and grants from different systems may
// carry different tracing keys while still representing the same stackable
// template.
type StackMergeResolution struct {
	ItemID  int64
	Kind    string
	PVFPath string
	// AttachType is the verbatim runtime-PVF `[attach type]` text of the resolved
	// template (for example "`[free]`").  The inventory owner reads it through
	// NormalizeAttachType to decide whether an item may be stored in a container
	// that only accepts tradeable items: the account cargo (list 12) whitelist is
	// free / account / sealing, every other value is untradeable.  It must stay
	// verbatim so the domain compares what the active PVF actually said instead of
	// a value already reinterpreted on the bridge side.
	AttachType string
	// EquipmentType is populated for equipment definitions.  The inventory
	// owner uses it to enforce the client-visible one-at-a-time rule for
	// quick-slot charms without relying on item names or template IDs.
	EquipmentType string
	StackableType string
	StackLimit    int64
	SlotStart     int16
	SlotEnd       int16
}

// NormalizeAttachType 把 PVF `[attach type]` 的原样文本（"`[free]`"、"`[trade delete]`"）
// 归一成小写裸词（"free"、"trade delete"）。与 dnfbridge 的 normalizeMagicBoxPVFType
// 同一口径：只剥掉反引号、方括号与空白，不改写取值本身。
func NormalizeAttachType(value string) string {
	return strings.ToLower(strings.Trim(value, "` []"))
}

// StackMergeResolver resolves one durable template through the active PVF.
// A nil resolver keeps the legacy Extra-backed behavior for isolated domain
// tests, while the current game route supplies it for every op19 move.
type StackMergeResolver func(itemID int64) (StackMergeResolution, error)

// PetHatchResolution is the dependency-neutral runtime-PVF result passed from
// dnfbridge into the pet domain. Keeping this DTO here avoids an alignedcmd ->
// pet import cycle while still preventing request-body or inventory-Extra
// values from becoming authoritative hatch mappings.
type PetHatchResolution struct {
	EggItemID      int64
	HatchedItemID  int64
	EggPVFPath     string
	HatchedPVFPath string
	MinimumLevel   int
}

// PetHatchResolver resolves one real inventory egg item through the active
// runtime PVF. A nil resolver means that the pet mutation must fail closed.
type PetHatchResolver func(eggItemID int64) (PetHatchResolution, error)

// EnchantBeadResolution is the dependency-neutral runtime-PVF result passed
// from dnfbridge into the inventory domain for one enchant-by-bead attempt.
// CardItemID is the monster-card/enchant item the bead carries; zero means the
// bead template carries no enchant card and the request must be rejected as an
// invalid bead. UpgradeCounts is the card's allowed enchant-upgrade table; an
// empty table means only upgrade count zero is accepted.
type EnchantBeadResolution struct {
	CardItemID                 int64
	CardPVFPath                string
	RandomTitleEnchantConfigID uint16
	RandomTitleEnchantRowID    uint16
	TargetWhitelist            []int64
	AllowedEquipmentTypes      []string
	UpgradeCounts              []int64
	TargetEquipmentType        string
	TargetKind                 string
}

// EnchantBeadResolver resolves the bead and target templates through the
// active runtime PVF. A nil resolver means the enchant mutation must fail
// closed instead of trusting request or inventory-Extra metadata.
type EnchantBeadResolver func(beadItemID int64, targetItemID int64) (EnchantBeadResolution, error)

// MonsterCardUpgradeResolution is the active-PVF authority for one card
// template used by current NoPack op603.  The client request contains only
// inventory slots; item category, rarity, maximum upgrade count and legal
// placement range must all be derived from the runtime PVF.
type MonsterCardUpgradeResolution struct {
	ItemID         int64
	PVFPath        string
	Rarity         int
	MaximumUpgrade byte
	StackLimit     int64
	SlotStart      int16
	SlotEnd        int16
	GoldCost       int64
	CalculateConst int
}

// MonsterCardUpgradeResolver resolves one raw monster-card template through
// the active runtime PVF. A nil or incomplete result makes op603 fail closed.
type MonsterCardUpgradeResolver func(itemID int64) (MonsterCardUpgradeResolution, error)

// MonsterCardUpgradePolicy is the enchanter's own authority over a card
// upgrade, taken from the [monstercard exp] row for the character's current
// enchanter level in character/expertjob/enchanter.exj. SuccessRates is indexed
// by the target card's rarity and is a percentage; at level 1 the live table
// reads 100/70/60/50/0 and at level 11 every tier is 100. Experience is what
// one attempt feeds back into the enchanter job.
//
// A zero value means the enchanter config was unavailable, and the caller falls
// back to the rarity-gap estimate it used before this table was wired in.
type MonsterCardUpgradePolicy struct {
	Known                 bool
	Level                 int
	SuccessRates          [5]int
	MinimumExperienceGain int
	MaximumExperienceGain int
}

// AmplifyWeightedLevel is one runtime-PVF [amplification random value]
// level/weight pair carried by a specific Pure Gold material.
type AmplifyWeightedLevel struct {
	Level  byte
	Weight int64
}

// AmplifyItemResolution is the dependency-neutral runtime-PVF result used by
// class1/op204 (purify/clear) and class1/op205 (invest/twist/Pure Gold).
// Option values follow etc/amplifyitem.etc: 1..4 are the four concrete
// amplification attributes and 5 means the client-selected [all] family.
// InitialValues is keyed by the final client amplification attribute 1..4.
type AmplifyItemResolution struct {
	TargetKind            string
	TargetPVFPath         string
	TargetMinimumLevel    int64
	TargetRarity          int64
	EquipLevelConst       int64
	PurifyMaterialCount   int64
	ClearMaterialCount    int64
	InvestOption          byte
	InvestMaterialCount   int64
	ReinvestOption        byte
	ReinvestMaterialCount int64
	PureGoldOption        byte
	PureGoldMaterialCount int64
	InitialValues         map[byte]uint16
	PureGoldLevels        []AmplifyWeightedLevel
	MaterialPVFPath       string
}

// AmplifyItemResolver resolves the real material and target templates through
// the active runtime PVF. A nil resolver means the mutation must fail closed.
type AmplifyItemResolver func(materialItemID int64, targetItemID int64) (AmplifyItemResolution, error)

// RandomOptionWeightedQuantity is one runtime-PVF random-option count roll.
type RandomOptionWeightedQuantity struct {
	Quantity byte
	Weight   int64
}

// RandomOptionCandidate is one weighted, level-resolved magic-seal option.
// Type/Value1/Value2 map directly to the current NoPack raw item fields.
type RandomOptionCandidate struct {
	Type   byte
	Value1 byte
	Value2 byte
	Weight int64
}

// RandomOptionGradeBand is one [postfix grade modification] row of
// etc/randomoption/randomizedoptionoverall2.etc. Grade 1..4 are the C/B/A/S
// seal grades, MinPct/MaxPct are the inclusive percentage window the grade
// covers inside the option's own value range, and Weight is the selection
// weight (the current PVF weights 330/520/100/50, i.e. out of 1000).
type RandomOptionGradeBand struct {
	Grade  int64
	MinPct int64
	MaxPct int64
	Weight int64
}

// RandomOptionPercentWindow is one [different weight] row of
// etc/randomoption/randomizedoptionoverall2.etc: "rarity slot minPct maxPct",
// the percentile window the initial (解封) roll uses for one 1-based seal slot.
// The current PVF ships 2 1 0 25 / 2 2 10 30 / 2 3 30 55 for rare and
// 3 1 10 35 / 3 2 20 40 / 3 3 40 65 for unique, i.e. the third seal always rolls
// higher than the first.
type RandomOptionPercentWindow struct {
	Slot   int64
	MinPct int64
	MaxPct int64
}

// RandomOptionResolution is the complete runtime-PVF policy for one target.
// InitialGroups and ModifiedGroups contain the three equipment-specific option
// pools selected by optiongroupselection.etc.
type RandomOptionResolution struct {
	TargetKind           string
	TargetPVFPath        string
	TargetEquipmentKey   string
	TargetMinimumLevel   int64
	TargetRarity         int64
	Eligible             bool
	QuantityWeights      []RandomOptionWeightedQuantity
	InitialGroups        [][]RandomOptionCandidate
	ModifiedGroups       [][]RandomOptionCandidate
	BreakSealGoldCost    int64
	ModificationGoldCost int64
	// ResetGrades is the current [postfix grade modification] window table for
	// the target rarity, one row per grade. Empty means op455 cannot re-roll
	// this item's seal grades at all.
	ResetGrades []RandomOptionGradeBand
	// GradeUnmodifiable mirrors [unable to modify postfix grade] and
	// GradeGradeless mirrors [gradeless postfix]; both list option ids the
	// current PVF forbids re-rolling the seal grade of.
	GradeUnmodifiable map[byte]struct{}
	GradeGradeless    map[byte]struct{}
	// InitialSlotWindows is the current [different weight] table for the target
	// rarity: the percentile window each 1-based seal slot rolls its initial
	// value in (op401 解封).
	InitialSlotWindows []RandomOptionPercentWindow
	// ChangeSlotWindows mirrors [changed postfix] when the PVF ships it. The
	// current PVF does not, so consumers fall back to InitialSlotWindows.
	ChangeSlotWindows []RandomOptionPercentWindow
}

// RandomOptionResolver resolves the real target template through the active
// runtime PVF. Nil or incomplete results make the mutation fail closed.
type RandomOptionResolver func(targetItemID int64) (RandomOptionResolution, error)

// EquipmentRegenerationMaterial is one PVF-derived cost row for op407.
// AccountShared is derived from the server's fixed soul warehouse mapping;
// the request never supplies a material slot or item identity.
type EquipmentRegenerationMaterial struct {
	ItemID        int64
	Count         int64
	AccountShared bool
}

// EquipmentRegenerationResolution is the fully selected, active-PVF result
// for one op407 equipment compound. The inventory owner remains authoritative
// for the source row and applies this result together with every material in
// one account+character asset transaction.
type EquipmentRegenerationResolution struct {
	Eligible            bool
	SourceItemID        int64
	SourcePVFPath       string
	ResultItemID        int64
	ResultPVFPath       string
	ResultEquipmentType string
	ResultRarity        int64
	ResultMinimumLevel  int64
	ResultDurability    uint16
	QualitySeed         uint32
	Materials           []EquipmentRegenerationMaterial
	CandidateCount      int
}

// EquipmentRegenerationResolver resolves and selects a legal compound result
// from the active PVF. Nil or incomplete evidence makes op407 fail closed.
type EquipmentRegenerationResolver func(sourceItemID int64, mode uint16, part uint16) (EquipmentRegenerationResolution, error)

// UpgradeTicketRandomEntry is one weighted candidate of an [enchant random]
// ticket: the ticket scene picks exactly one entry by weight and jumps the
// equipment straight to that entry's level. Weight is out of 100000.
type UpgradeTicketRandomEntry struct {
	TargetLevel int64
	Weight      int64
}

// EnchantRandomTicket is the runtime-PVF payload shared by every [enchant
// random] stackable. The op50 reinforcement ticket scene and the op438
// weapon-forging ticket family read the same child sections: er_enchant
// carries the (level, weight-out-of-100000) candidates, er_slot restricts the
// target part (0 any, 1 weapon, 2 armor, 3 accessory), er_grade whitelists
// target rarities (empty = any) and er_seal == 1 demands a sealed target (the
// common er_seal 0/2 carries no check).
type EnchantRandomTicket struct {
	Entries         []UpgradeTicketRandomEntry
	SlotRestriction int64
	Rarities        []int64
	SealRestriction int64
}

// UpgradeTicketResolution is the dependency-neutral runtime-PVF result passed
// from dnfbridge into the inventory domain for one op50 ticket-scene upgrade
// attempt. TicketMode is "reinforce" or "amplify" when the material template
// carries [equipment (amplify )?reinforcement ticket]; an empty TicketMode
// means the material is not an upgrade ticket and the request must stay on
// the pending normal-reinforcement path. SuccessWeight is the ticket's
// success chance out of 100000 (percent*1000). TicketRandom marks the
// [enchant random] multi-candidate family (e.g. 祝福强化券 2681908/2681909),
// whose candidates and target restrictions arrive in RandomTicket and are
// checked against TargetRarity plus the target row's seal flag.
type UpgradeTicketResolution struct {
	TicketMode             string
	TicketRandom           bool
	RandomTicket           EnchantRandomTicket
	PortableAmplifier      bool
	PortableReinforcer     bool
	PortableMinimumLevel   int64
	PortableMaximumLevel   int64
	TargetMinimumLevel     int64
	TargetLevelMinimum     int64
	TargetLevelMaximum     int64
	TargetLevel            int64
	SuccessWeight          int64
	TargetUpgradeForbidden bool
	TargetEquipmentType    string
	TargetRarity           int64
	TargetKind             string
	TicketPVFPath          string
}

// UpgradeTicketResolver resolves the material and target templates through
// the active runtime PVF. A nil resolver means the ticket mutation must fail
// closed instead of trusting request or inventory-Extra metadata.
type UpgradeTicketResolver func(materialItemID int64, targetItemID int64) (UpgradeTicketResolution, error)

// UpgradeProtectionResolution is the active-PVF authority for the optional
// protection-ticket slot carried by op50. TicketMode is "reinforce" or
// "amplify". FailureRetainLevel is zero for the ordinary protection family
// and the explicit [action type] parameter for the [new ... protect
// equipment] family.
type UpgradeProtectionResolution struct {
	TicketMode         string
	FailureRetainLevel int
	TicketPVFPath      string
}

// UpgradeProtectionResolver resolves the exact stackable template found in
// the request's optional ticket slot. An empty TicketMode is not a protection
// ticket and must fail closed without consuming it.
type UpgradeProtectionResolver func(ticketItemID int64) (UpgradeProtectionResolution, error)

// UpgradePolicyResolution carries the PVF-derived success/penalty parameters
// for one normal NPC upgrade attempt (non-ticket scene).
type UpgradePolicyResolution struct {
	SuccessWeight      int // out of 100000
	PenaltyType        int // 0=no change, 1=one-level downgrade, 2=value downgrade, 3=destroy
	PenaltyValue       int
	GoldCost           int64
	MaterialItemID     int
	MaterialCount      int
	DestroyBonusItemID int
	DestroyBonusCount  int
	// DestroyExtraRewards are additional rows granted alongside the
	// destruction compensation: the disjointer.exj [chronicle set disjoint
	// result] refund for 异界 equipment, the companion trace an amplified
	// target also refunds, and this server's house rule of returning what the
	// destroyed equipment would have yielded in the 分解机.
	DestroyExtraRewards []UpgradeDestroyReward
	NoticeLevel         int // announcement threshold (-1 = disabled)

	// GuildMedal marks a guild flag (guild medal) upgrade attempt, which is
	// resolved from the native flag_upgrade_table instead of the equipment
	// upgrade table. CurrentLevel carries the pre-attempt flag level so the
	// caller can mirror it back without re-deriving it from the row payload.
	GuildMedal   bool
	CurrentLevel int
}

// UpgradeDestroyReward is one item row granted when an upgrade destroys the
// target.
type UpgradeDestroyReward struct {
	ItemID int
	Count  int
	// BestEffort rows are dropped when the inventory has no room for them
	// instead of failing the whole upgrade. The PVF-mandated compensation is
	// never best-effort; the disassembly refund this server adds on top is,
	// because losing a bonus row must not void the attempt itself.
	BestEffort bool
}

// UpgradeRefundMailer delivers destruction refund rows the bag had no room
// for. It runs after the upgrade's asset transaction commits, because that
// transaction covers characters, inventory and equipment while mailboxes have
// their own unit of work, and the destroyed target may sit on the equipment
// page. The implementation owns its logging: a mail failure must not undo an
// upgrade that already committed.
type UpgradeRefundMailer func(ctx context.Context, characterID string, rows []UpgradeDestroyReward) error

// UpgradePolicyResolver resolves the PVF upgrade table row and target-dependent
// gold cost for the given mode, current upgrade level, and equipment template.
// amplifyType is the target's 红字 attribute (0 when it carries none); the
// destruction refund needs it because the disjointer's otherverse bonus is
// keyed off it. A nil resolver means guaranteed success (backward compatible
// with pre-PVF-table behavior).
type UpgradePolicyResolver func(mode string, currentLevel int, targetItemID int64, amplifyType int) (UpgradePolicyResolution, error)

// ForgingResolution carries the runtime-PVF policy for one weapon-forging
// attempt. SuccessWeight uses the native upgrade_separate.etc base of 10000.
// The current client stores the durable forging level in its 0x77 item row;
// no request-provided item metadata is authoritative here.
type ForgingResolution struct {
	TargetKind          string
	TargetPVFPath       string
	TargetEquipmentType string
	TargetDurability    uint16
	MaxLevel            byte
	TargetLevel         byte
	IndependentAttack   int64
	SuccessWeight       int
	MaterialItemID      int64
	MaterialCount       int64
	// TicketMode is "fixed" or "additional" when the request material
	// carries [equipment separate reinforcement ticket]. An empty value keeps
	// the ordinary strong-breath forging path. TicketApplyValue is the PVF
	// source-level cap used by additional tickets.
	TicketMode       string
	TicketApplyValue int64
	TicketPVFPath    string
	// TicketRandom marks the second forging-coupon family: an [enchant random]
	// stackable whose [action type] is `[random upgrade separate]` (e.g.
	// +6锻造券 2683788). RandomTicket carries its er_enchant forging-level
	// candidates and the er_slot / er_grade / er_seal target restrictions,
	// which are checked against TargetRarity plus the target row's seal flag.
	// The domain owns the candidate roll, so the resolver leaves TargetLevel
	// and SuccessWeight unset and pre-computes the independent attack of every
	// candidate level in RandomIndependentAttacks.
	TicketRandom             bool
	RandomTicket             EnchantRandomTicket
	TargetRarity             int64
	RandomIndependentAttacks map[int64]int64
}

// ForgingResolver resolves the request material, target item and resulting
// forging level through the active runtime PVF. The material identity is
// required because op438 is shared by ordinary forging materials and
// [equipment separate reinforcement ticket] coupons.
type ForgingResolver func(materialItemID int64, targetItemID int64, currentLevel byte) (ForgingResolution, error)

// ChronicleRefineResolution is the runtime-PVF authorization for one op370
// aura application. The client derives the actual skill property from the
// imported aura script; the server persists only the canonical aura ID and
// the four branch-selector bytes carried by the current item row.
type ChronicleRefineResolution struct {
	MaterialPVFPath         string
	ImportedScriptPVFPath   string
	AuraType                byte
	CanonicalAuraOptionID   int32
	Probabilities           [2]byte
	FailureFragmentItemID   int64
	FailureCubeCount        int64
	FailureCubeItem         MagicBoxRewardItem
	FailureFragmentItem     MagicBoxRewardItem
	TargetPVFPath           string
	TargetKind              string
	TargetEquipmentType     string
	TargetEquipmentTypeCode byte
	TargetDurability        uint16
	Matched                 bool
}

// ChronicleRefineResolver validates the material import chain, target
// [character item check], and imported [check]/[skill] branch against the
// selected character. GrowType is the normalized low-nibble first class.
type ChronicleRefineResolver func(
	materialItemID int64,
	targetItemID int64,
	characterJob byte,
	firstGrowType byte,
	optionNo byte,
) (ChronicleRefineResolution, error)

// MagicBoxRewardEntry is one weighted candidate inside a reward group.
type MagicBoxRewardEntry struct {
	ItemID int64
	Weight int64
	Count  int64
	// Gold marks the official [upgradable legacy] pseudo reward whose PVF
	// item id is zero. Count is the wallet amount; it must never be projected
	// as a fabricated inventory item.
	Gold   bool
	Option byte // Historical [booster info]/[avatar] fifth field.
}

// MagicBoxRewardGroup is one independently-drawn reward pool; each group
// yields DrawCount weighted picks per open (86JP RollBoosterRewards).
//
// Job/MinLevel/MaxLevel 来自 [booster info] 的 `[charactor]` 分组：那种分组是
// **按职业发放**的（例如豪华婚礼化妆箱每个职业一组时装），只有开启者职业匹配
// 且等级在区间内时该组才参与抽取。普通分组三者为零值，表示"人人可抽"。
type MagicBoxRewardGroup struct {
	DrawCount int64
	Entries   []MagicBoxRewardEntry
	Job       string
	MinLevel  int64
	MaxLevel  int64
}

// MagicBoxResolution is the dependency-neutral runtime-PVF result passed from
// dnfbridge into the package domain for one magic-box open. Kind is "random"
// for weighted pools ([random upgradable legacy] RANDOMBOX groups and the
// [booster]/[cera booster]/[booster random] booster-info family), "package"
// for grant-all [cera package], "unsupported" for client-selected families,
// and "" when the template is not an openable box. MaterialItemID is the
// per-open material requirement (0 when the box opens free); the box itself
// is never reported as a material.
type MagicBoxResolution struct {
	Kind                string
	Groups              []MagicBoxRewardGroup
	PackageItems        []MagicBoxRewardEntry
	MaterialItemID      int64
	MaterialCountPerUse int64
	BoxPVFPath          string
}

// MagicBoxResolver resolves one box template through the active runtime PVF.
// A nil resolver means the open must fail closed instead of trusting request
// or inventory-Extra metadata.
type MagicBoxResolver func(boxItemID int64) (MagicBoxResolution, error)

// MagicBoxRewardItem carries the grant metadata of one resolved reward
// template: inventory kind, stack limit (0 = unlimited), slot range, the seal
// flag derived from its PVF [attach type], its document path, and the absolute
// expiration resolved once from a relative PVF [usable period] at grant time.
type MagicBoxRewardItem struct {
	ItemID           int64
	Kind             string
	TargetListType   byte
	EquipmentType    string
	StackLimit       int64
	SlotStart        int16
	SlotEnd          int16
	Seal             bool
	PVFPath          string
	ExpireAt         time.Time
	UsablePeriodDays int64
	Durability       uint16
	// QualitySeed 是装备奖励的品级种子（raw+0x06）。
	//
	// **由解析方掷好再传进来，Owner 自己不掷**：本包的 Owner 侧一直是纯确定性的
	// （同一份契约必然产生同一笔提交，事务里没有随机源），随机源只在 dnfbridge
	// 那一层（抽奖掷骰、掉落掷品级都已经是那样做的）。0 表示不写，交给既有投影的
	// 默认行为。
	QualitySeed uint32
	// DirectActivation marks a reward the runtime PVF defines as a premium
	// contract. Such a reward must not occupy a bag cell: the owner reports its
	// count back through MagicBoxResult.DirectActivations and the bridge extends
	// the account entitlement instead, which is what the official service does.
	DirectActivation bool
}

// MagicBoxRewardItemResolver resolves one reward template through the active
// runtime PVF for the durable grant step.
type MagicBoxRewardItemResolver func(itemID int64) (MagicBoxRewardItem, error)

// MagicBoxContractActivator extends the account entitlement for premium contract
// units a magic-box reward flow rolled without placing them in the bag. It runs
// inside the box owner's repository transaction, with the account record the
// owner is about to persist, so the entitlement and the box consumption commit
// together. Returning an error makes the owner place those units as ordinary
// rewards instead, which keeps the reward recoverable exactly as before.
type MagicBoxContractActivator func(account *dnfrepo.AccountRecord, units map[int64]int64, now time.Time) error

// RandomRewardItemOutcome is one weighted [chn random image percent] result.
// A zero Reward.ItemID is the PVF's no-item visual-effect outcome.
type RandomRewardItemOutcome struct {
	Weight int64
	Reward MagicBoxRewardItem
}

// RandomRewardItemResolution is the runtime-PVF authorization for a
// class-1/type251 shared-effect item with [stackable type] [random reward
// item]. The source is always consumed; one weighted outcome may add one
// repository-backed item.
type RandomRewardItemResolution struct {
	SourceItemID  int64
	SourcePVFPath string
	StackableType string
	Outcomes      []RandomRewardItemOutcome
}

// RandomRewardItemResolver keeps the random-reward table and reward template
// metadata in dnfbridge, rather than trusting mutable inventory Extra fields.
// A zero SourceItemID means the candidate is not this consumable family.
type RandomRewardItemResolver func(itemID int64) (RandomRewardItemResolution, error)

// PremiumContractResolution is the dependency-neutral runtime-PVF result for
// one contract item activation: the premium type and its duration in seconds,
// read from premiumlist_new.etc explicit [item]/[term] entries or a legacy
// type-level [target item]/[term] block when that type has no explicit items.
type PremiumContractResolution struct {
	ItemID          int64
	PremiumType     int64
	DurationSeconds int64
}

// PremiumContractResolver resolves one candidate item through the active
// runtime PVF. A zero PremiumType means the item is not a contract item and
// the caller must fall back to the ordinary use-stackable path.
type PremiumContractResolver func(itemID int64) (PremiumContractResolution, error)

// ExperienceConsumableResolution is the runtime-PVF authority for one
// [waste] or [unlimited waste] item carrying [exp bonus rate] and
// [stat change duration]. The PVF rate is projected as an additive percentage:
// 2.0 means +200% (3x total). Reusable follows the PVF stackable type and is
// never inferred from an item name or client request.
type ExperienceConsumableResolution struct {
	ItemID              int64
	BonusPercent        int64
	DurationSeconds     int64
	ExperienceSourceKey uint32
	PVFPath             string
	Reusable            bool
}

// ExperienceConsumableResolver returns a zero BonusPercent for ordinary
// stackables. Only current-PVF item documents may create an active effect.
type ExperienceConsumableResolver func(itemID int64) (ExperienceConsumableResolution, error)

// DirectExperienceConsumableResolution is the runtime-PVF contract for one
// immediate character-experience item. EffectType is one of the four native
// [increase status type] families and EffectValues preserves the exact PVF
// operands; names, client requests and mutable inventory metadata never select
// an experience amount.
type DirectExperienceConsumableResolution struct {
	ItemID       int64
	PVFPath      string
	EffectType   string
	EffectValues []int64
	MinimumLevel int64
	MaximumLevel int64
}

// DirectExperienceConsumablePlan couples character EXP/level and the SP/TP
// ledger. The inventory owner applies this plan and the source decrement in a
// single CharacterSettlement transaction.
type DirectExperienceConsumablePlan struct {
	Character          dnfrepo.CharacterRecord
	Skill              dnfrepo.SkillRecord
	CharacterFields    []dnfrepo.CharacterField
	SkillFields        []dnfrepo.SkillField
	Gain               uint32
	PreviousLevel      int
	NewLevel           int
	PreviousExperience uint32
	NewExperience      uint32
	SPGain             int
	TPGain             int
}

// DirectExperienceConsumablePlanner keeps runtime-PVF progression tables and
// level-stat projection in dnfbridge while the inventory owner retains the
// atomic write boundary.
type DirectExperienceConsumablePlanner func(
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	resolution DirectExperienceConsumableResolution,
	updatedAt time.Time,
) (DirectExperienceConsumablePlan, error)

// StackableUseResolution is the current-PVF authorization for an ordinary
// type-44 stackable use. It exists separately from inventory Extra so legacy
// rows with a PVF path but missing stackable_type can still be validated
// without trusting client-supplied metadata.
type StackableUseResolution struct {
	ItemID                       int64
	PVFPath                      string
	StackableType                string
	TPBook                       *TPBookResolution
	PetFeedEffectDurationSeconds int64
	PetFeedSkillChargePercent    int64
	PetFeedSatietyDelta          int64
	DirectExperience             *DirectExperienceConsumableResolution
	QuestCompletionTicket        *QuestCompletionTicketResolution
	StoryBook                    *StoryBookResolution
	Transformation               *TransformationUseResolution
	Projectile                   *ProjectileUseResolution
	UsePlace                     *StackableUsePlaceResolution
	AvatarFittingRoomTicket      *AvatarFittingRoomTicketResolution
	CustomAbilityGroupConverter  *CustomAbilityGroupConverterResolution
	DungeonAdmissionTicket       *DungeonAdmissionTicketResolution
	FatigueRecovery              int64
	FatigueMaximumRemaining      int64
	// DailyUseLimit is the item's own 每天可用次数 cap: the PVF
	// [total usable count] of ANY stackable consumable, not a fatigue-potion
	// special case. A positive value means the shared per-character daily
	// counter enforces it (inventory.Owner.ReserveDailyItemUse) and the used
	// count is projected to the client as op541; zero means the item has no cap
	// and must not cost an extra database read.
	DailyUseLimit int64
	// MaintainsCooltime 是 PVF `[cooltime maintenance]`（值非零）的语义：这个冷却要
	// **跨会话保留**，由服务端落一份到期时间，并在窗口内拒绝再次使用（客户端的冷却门
	// 只是进程内内存，小退重上就没了——现场 2026-09-17：1 天冷却的消耗品重上后又能用）。
	// CooltimeSeconds 来自 `[cool time]`（毫秒换算），CooltimeGroup 来自
	// `[cooltime group]`（同组共享一个窗口，0 = 只锁自己）。MaintainsCooltime 为 false
	// 时后两个字段必须忽略：只有 [cool time] 的道具（10 秒冷却的红蓝药）保持纯客户端语义。
	MaintainsCooltime bool
	CooltimeSeconds   int64
	CooltimeGroup     int64
	RentalPoint       *RentalPointGrantResolution
	// TarotLottery 是 PVF `[stackable type]` = `[monster tarot card]` 的抽奖契约
	// （金币花费 + 已抽中的那一项）。它的提交需要金币与道具同笔，见
	// inventory.Owner.useTarotLotteryCard。
	TarotLottery *TarotLotteryResolution
	// ContainerUpgrade 是 PVF `[action type]` 为 `[upgrade inventory]` /
	// `[upgrade cargo]` / `[upgrade account cargo]` 的扩容券契约：使用后把对应容器
	// 推进一档（背包 +8 格、金库 +16 格、帐号金库下一档）。
	//
	// 2026-10-03 现场（测试x，char 11）：这三种券使用后没效果。取证发现客户端其实
	// **有发包**：op430 UPGRADE_INVENTORY ×11（body 就是那张券的槽位 0x42/0x44）、
	// op408 UPGRADE_CARGO ×1，而服务端对这两个 opcode **完全静默**（没有任何
	// handler/路由登记，DispatchUpper 对未注册 opcode 直接 return）。所以这三条
	// action type 既没有 opcode 接线、解析器里也没有分支。
	ContainerUpgrade *ContainerUpgradeResolution
	// StatChangePotion 是 PVF `[stackable type] [expert town potion]` + **没有**
	// `[action type]` + `[stat change duration]` 这一族的"城镇自施放增益药剂"契约
	// （史诗Buff药剂 / 欧皇药水 / 史派克的欧气药水 / 小灯笼的大瓶·小瓶史诗Buff药剂 …）。
	//
	// 现场 2026-10-03（测试x = char 11）：490002458 史诗Buff药剂 连点 15 次，
	// op44 全部被 "[waste] provenance" 那道门静默拒绝（error 17）——既没消耗也没效果。
	// 这一族的效果是**服务端量**（深渊派对史诗掉落率），所以服务端必须自己记窗口，
	// 见 internal/modules/dnf/statbuff。
	StatChangePotion *StatChangePotionResolution
}

// StatChangePotionResolution 是一张城镇增益药剂的解析结果。
type StatChangePotionResolution struct {
	// EffectDurationSeconds 来自 PVF `[stat change duration]`（毫秒换算）。
	// 它既是增益窗口，也正好等于 `[cool time]`（本族都是 600000ms = 10 分钟）。
	EffectDurationSeconds int64
	// EpicDropRatePercent 是"表里查不到条目"时的回退值：说明文字里写了"掉落率"
	// 就取 200（2 倍），否则 0（= 服务端不改掉落）。
	//
	// 为什么需要回退：这一族的**权威数值**在两张 buff 表里（见下面的 BuffRolls），
	// 但 490000413 这类 2016 年老活动券在表里没有条目，而它自己的 `[explain]` 明写
	// "史诗道具掉落率增加2倍"。两条路都查不到 ⇒ 0，绝不猜。
	EpicDropRatePercent int64
	// BuffRolls 是**权威**来源：etc/variousbuffstackablelist.etc 给出"这件道具按权重
	// 随机给哪个 buff"，etc/variousbufflist.etc 给出该 buff 的
	// `epic_rarity_item_drop_rate` 数值（+N%）。现场 2026-10-03 抄录（可用
	// cmd/scratch/pvfmember 复核）：
	//
	//	490002458 → buff 1136 权重 100，param 100（+100% = 2 倍）
	//	490005358 → buff 1239 权重 100，param 10（+10%）
	//	490005359 → buff 1240 权重 100，param 5（+5%）
	//	10008219  → buff 1090/1091/1092 概率 60/30/10，param 100/150/200
	//
	// 它是**概率表**，所以只能服务端掷点 —— 客户端算不出自己拿到哪一档，
	// 这就是"必须下发状态给客户端"的原因（见 class0/op174 投影）。
	BuffRolls []StatChangePotionBuffRoll
	// UpgradeKind 是这件药剂对**强化/增幅成功率**的作用对象：
	// "reinforce"（强化）/ "amplify"（增幅）/ ""（不影响）。服主 2026-10-03：
	// "看看增幅的秘密 强化秘药这类增加强化增幅成功率的道具 也一并修复了"。
	//
	// 判定来自 PVF 的 `[stackable type]`：
	//
	//	`[item upgrad buff usual upgrade]`   → reinforce（强化的奥秘）
	//	`[item upgrad buff amplify upgrade]` → amplify（永恒的增幅的秘密）
	//
	// 而 `[expert town potion]` 户籍的药剂（赛丽亚的幸运强化药剂 10008220、强化秘药）
	// 走 UpgradeRolls / UpgradeBonusPercent。
	UpgradeKind string
	// UpgradeRolls 是"道具 → 强化成功率 buff"的**概率表**（权威来源，与 BuffRolls 同构）：
	//
	//	10008220 → buff 1097/1098/1099 概率 60/30/10，param 10/20/30
	//
	// （buff 1097/1098/1099 的 [tooltip text] 就是"强化成功率提升 (下级/中级/上级)"）。
	UpgradeRolls []StatChangePotionWeightedRoll
	// UpgradeBonusPercent 是没有概率表时的**固定加成**（百分点）。
	//
	// 为什么需要它：强化秘药、强化的奥秘、永恒的增幅的秘密这几件在 PVF 里**根本没有数字**
	// —— 两张 buff 表里都没有它们的条目，说明文字也只写"可以提升强化成功率"。
	// 所以只能由服务端给一个常量，见 dnfbridge.currentUpgradeRatePotionDefaultBonusPercent
	// （取值 10，依据是同一份 PVF 里另外两件写明了数字的道具：crystallight_10007343
	// "强化成功率增加10%，持续30秒"、fortuneaura_success "增加10%的成功率，持续60分钟"，
	// 以及"下级"档本身就是 10）。
	UpgradeBonusPercent int64
	// DungeonClearExpRolls 是"道具 → 地下城通关经验 buff"的概率表（同样来自 PVF）：
	//
	//	10008222（赛丽亚的幸运经验药剂）→ buff 1103/1104/1105 概率 30/50/20，param 50/100/200
	//
	// 通关经验是**服务端发的**，所以这一族必须服务端算（与掉落率同构）。
	DungeonClearExpRolls []StatChangePotionWeightedRoll
	// ClientBuffFamily / ClientBuffRolls 是"客户端自己施加"的那一族（服务端只负责把掷到的
	// buff 注册给客户端，客户端按同一份 buff 表施加效果）：
	//
	//	10008221（赛丽亚的幸运城镇药剂）→ buff 1100/1101/1102，`village_move_speed` 15/30/50
	//	10008223（赛丽亚的幸运属性药剂）→ buff 1106/1107/1108，`move_speed` 进图属性三档
	//
	// ClientBuffFamily 就是 buff 的 effect name（窗口按 family 分开存，互不覆盖）。
	ClientBuffFamily string
	ClientBuffRolls  []StatChangePotionWeightedRoll
}

// StatChangePotionBuffRoll 是一条"道具 → buff"的候选：buff id + 权重 + 该 buff 的
// 掉落率加成百分比（取自 buff 定义表的 effect param）。
type StatChangePotionBuffRoll struct {
	BuffID       int64
	Weight       int64
	BonusPercent int64
}

// StatChangePotionWeightedRoll 是同族药剂在**某一种服务端效果**上的候选：
// buff id + 权重 + 该 buff 的 param（掉落率百分比 / 成功率百分点 / 通关经验百分比）。
//
// 强化成功率、通关经验、客户端自施放那几族共用它 —— 形状完全一样，分成多个类型只会
// 让解析与掷点各写一遍。
type StatChangePotionWeightedRoll struct {
	BuffID       int64
	Weight       int64
	BonusPercent int64
}

// ContainerUpgradeKind 是扩容券作用的容器。
type ContainerUpgradeKind string

const (
	// ContainerUpgradeInventory 对应 PVF `[action type] [upgrade inventory]`：
	// 物品栏扩容券，每张 +8 格，上限 24（= 3 张）。
	ContainerUpgradeInventory ContainerUpgradeKind = "upgrade_inventory"
	// ContainerUpgradeCargo 对应 `[upgrade cargo]`：金库（个人仓库 list 2）扩容券。
	ContainerUpgradeCargo ContainerUpgradeKind = "upgrade_cargo"
	// ContainerUpgradeAccountCargo 对应 `[upgrade account cargo]`：帐号金库升级工具，
	// 按 accountCargoTiers 推进一档。
	ContainerUpgradeAccountCargo ContainerUpgradeKind = "upgrade_account_cargo"
)

// ContainerUpgradeResolution 是一张扩容券的解析结果。
type ContainerUpgradeResolution struct {
	Kind ContainerUpgradeKind
	// ItemID 是这张券自己（用于日志与校验）。
	ItemID int64
	// SlotIndex 是客户端声明的券所在槽位（op430/op408 的 2 字节 body 就是它）。
	SlotIndex int16
	// ListType 是券所在的容器（当前客户端都发 0 = 背包）。
	ListType byte
}

// RentalPointGrantResolution is the active-PVF contract for a [waste] item
// whose [action type] is [rent point]: using it charges the account's rental
// wallet (the 幸运星 balance) by Grant, clamped to the rental system's
// [limit point]. The wallet itself stays owned by the rental module.
type RentalPointGrantResolution struct {
	Grant uint32
	Limit uint32
}

// DungeonAdmissionTicketResolution is the active-PVF contract for a waste
// item that expands one or more repository-owned admission policies in the
// current Beijing service day. The inventory owner commits source decrement
// and all character/account allowances in one settlement transaction.
type DungeonAdmissionTicketResolution struct {
	ActionType       string
	ActionPacket     uint32
	TargetDungeonID  uint32
	Grants           []dnfdungeonadmission.AllowanceGrant
	UseLimit         dnfdungeonadmission.AllowanceUseLimit
	MinimumLevel     int64
	MaximumLevel     int64
	VillageAllowed   bool
	SeriaRoomAllowed bool
}

// StackableUsePlaceResolution carries the active PVF's explicit scene
// restriction for an ordinary [waste] action. It lets the inventory owner
// authorize old inventory rows by server-owned item identity while still
// rejecting a dungeon-only consumable outside its documented dungeon.
type StackableUsePlaceResolution struct {
	VillageAllowed         bool
	DungeonAllowed         bool
	SeriaRoomAllowed       bool
	ActionUsableDungeonIDs []int64
	LimitDungeonIDs        []int64
}

// CustomAbilityGroupConverterResolution authorizes the current PVF's
// [custom ability set group] action. op44 only opens the native individual
// custom-ability window; op1340 later owns the option change and source debit.
type CustomAbilityGroupConverterResolution struct {
	MinimumLevel int64
	MaximumLevel int64
}

// AvatarFittingRoomTicketResolution authorizes the current PVF's
// [avatar fitting room ticket] action. op44 only opens the fitting-room UI;
// the ticket is deliberately consumed later by op1488, once the client has
// submitted the complete eight-part change request.
type AvatarFittingRoomTicketResolution struct {
	MinimumLevel   int64
	MaximumLevel   int64
	VillageAllowed bool
	SeriaAllowed   bool
}

// TransformationUseResolution is the complete active-PVF authorization for
// one native transformation consumable. The server validates the current
// location before consuming the stack; the successful op44 response then lets
// the current client execute the item's own PVF action (disguise, riding or
// ridable-item) instead of fabricating a second transformation packet.
type TransformationUseResolution struct {
	ActionType             string
	FormKinds              []string
	FormIDs                []int64
	DurationSeconds        int64
	VillageAllowed         bool
	DungeonAllowed         bool
	SeriaRoomAllowed       bool
	DisallowAssault        bool
	ActionUsableDungeonIDs []int64
	LimitDungeonIDs        []int64
}

// ProjectileUseResolution is the complete active-PVF contract for one
// dungeon throw/set item. The server owns location validation and durable
// consumption; the successful native op44 acknowledgement invokes the
// current client's own projectile/passive-object action.
type ProjectileUseResolution struct {
	StackableType          string
	EffectID               int64
	IntData                []int64
	ParticlePaths          []string
	DungeonAllowed         bool
	ActionUsableDungeonIDs []int64
	LimitDungeonIDs        []int64
}

// StoryBookResolution is the exact current-PVF [story book] contract carried
// by one consumable. The client does not choose the quest ID independently;
// it is read only from the item's action values.
type StoryBookResolution struct {
	ActionType   string
	QuestID      int64
	MinimumLevel int64
	MaximumLevel int64
}

// StoryBookPlan is the quest mutation prepared from the active quest PVF.
// Inventory owns the final transaction so accepting the story and consuming
// the book cannot diverge.
type StoryBookPlan struct {
	Quest       dnfrepo.QuestRecord
	QuestFields []dnfrepo.QuestField
	QuestID     int64
	InitTrigger uint32
}

type StoryBookPlanner func(
	character dnfrepo.CharacterRecord,
	quest dnfrepo.QuestRecord,
	book StoryBookResolution,
	acceptedAt time.Time,
) (StoryBookPlan, error)

// SkillPointBookKind identifies the three legacy [etc] book effects whose
// current PVF documents have no action type.
type SkillPointBookKind byte

const (
	SkillPointBookUnknown SkillPointBookKind = iota
	SkillPointBookAddSP
	SkillPointBookAddTP
	SkillPointBookResetTP
)

// TPBookResolution is retained as the transport-neutral name used by the
// existing inventory boundary, but now models every current-PVF SP/TP book.
// Delta is authoritative only for AddSP/AddTP; ResetTP refunds purchased TP
// skill levels to their active-PVF baseline.
type TPBookResolution struct {
	Kind         SkillPointBookKind
	Delta        int64
	MinimumLevel int64
	MaximumLevel int64
}

// StackableUseResolver resolves one candidate ordinary consumable through the
// active runtime PVF. A zero ItemID means the item is not an authorized
// ordinary consumable.
type StackableUseResolver func(itemID int64) (StackableUseResolution, error)

// QuestCompletionTicketResolution is the complete runtime-PVF contract for a
// quest-completion stackable. ActionValues are the integer operands stored in
// the item's [action type] section (for example the exact quest IDs carried by
// [any quest clear]). Level bounds come from the same item document.
type QuestCompletionTicketResolution struct {
	ActionType               string
	ActionValues             []int64
	MinimumLevel             int64
	MaximumLevel             int64
	VillageOnly              bool
	PreserveSideQuests       bool
	PreserveHellPartyUnlocks bool
}

// QuestCompletionTicketPlan is a mutation plan produced from the current PVF
// quest catalog. The inventory owner applies it with the source-item decrement
// inside one CharacterSettlement transaction.
type QuestCompletionTicketPlan struct {
	Quest             dnfrepo.QuestRecord
	Character         dnfrepo.CharacterRecord
	QuestFields       []dnfrepo.QuestField
	CharacterFields   []dnfrepo.CharacterField
	CompletedQuestIDs []int64
	ProfessionChanged bool
}

// QuestCompletionTicketPlanner keeps quest-catalog interpretation outside the
// inventory package while leaving the final inventory/quest/character commit
// under one repository transaction.
type QuestCompletionTicketPlanner func(
	character dnfrepo.CharacterRecord,
	quest dnfrepo.QuestRecord,
	ticket QuestCompletionTicketResolution,
	completedAt time.Time,
) (QuestCompletionTicketPlan, error)

// ChronicleGrowthResolution is the current-PVF policy for one op271
// equipment-level emancipation request. Client item IDs and slots remain
// lookup keys that the inventory owner revalidates inside its transaction.
type ChronicleGrowthResolution struct {
	TicketItemID       int64
	MaterialItemID     int64
	UpgradeLevel       int
	TargetMinimumLevel int
	ConditionMinimum   int
	ConditionMaximum   int
	TargetRarity       int64
	EquipmentType      string
	AllowedRarities    []int64
	Probability        []ChronicleGrowthProbability
	CostPolicy         ChronicleGrowthCostPolicy
	TicketPVFPath      string
	Conversion         *ChronicleEquipmentConversionResolution
}

// ChronicleEquipmentConversionResolution is the current-PVF authority for
// the equipment [emancipate] branch of op271. The source equipment owns the
// complete material/output recipe; the ticket only selects the matching type
// and its retention limits.
type ChronicleEquipmentConversionResolution struct {
	TicketType          int64
	PreserveAttributes  bool
	Inputs              []ChronicleEquipmentConversionMaterial
	OutputItemID        int64
	OutputPVFPath       string
	OutputEquipmentType string
	OutputRarity        int64
	OutputGrade         int64
	OutputMinimumLevel  int64
	OutputDurability    uint16
	OutputQualitySeed   uint32
	BindOutput          bool
	UpgradeMaximum      int
	AmplifyMaximum      int
	ForgingMaximum      int
}

type ChronicleEquipmentConversionMaterial struct {
	ItemID int64
	Count  int64
}

type ChronicleGrowthProbability struct {
	MaximumLevel int
	Weight       int64
}

type ChronicleGrowthWeightSegment struct {
	Minimum       int
	Maximum       int
	MinimumWeight float64
	MaximumWeight float64
}

type ChronicleGrowthCostPolicy struct {
	Rarity         int64
	MaterialItemID int64
	BaseCount      int64
	Multiplier     float64
	PartWeights    map[string]float64
	LevelWeights   []ChronicleGrowthWeightSegment
	UpgradeWeights []ChronicleGrowthWeightSegment
	AmplifyWeights []ChronicleGrowthWeightSegment
	ForgingWeights []ChronicleGrowthWeightSegment
}

type ChronicleGrowthResolver func(ticketItemID int64, targetItemID int64) (ChronicleGrowthResolution, error)

// DamageFontExpirationMode describes the expiration contract carried by a
// runtime-PVF [add damage font skin] item.
type DamageFontExpirationMode byte

const (
	DamageFontExpirationUnknown DamageFontExpirationMode = iota
	DamageFontExpirationPeriod
	DamageFontExpirationFixed
	DamageFontExpirationUnlimited
)

// DamageFontResolution is the runtime-PVF authority for one damage-font item.
// A zero FontIndex or invalid resolution must not consume the source item.
type DamageFontResolution struct {
	Valid           bool
	PVFPath         string
	ActionType      string
	FontIndex       uint16
	ExpirationMode  DamageFontExpirationMode
	PeriodDays      int64
	FixedExpiration time.Time
}

// DamageFontResolver resolves one inventory source through the active PVF.
type DamageFontResolver func(itemID int64) (DamageFontResolution, error)

// RepairCostEvidence is the dependency-neutral runtime-PVF repair evidence
// for one equipment item, matching the 86JP EquipmentRepairPriceProvider
// inputs. MaxDurability <= 0 means the item is not repairable. The rates come
// from equipment/pricetable.tbl ([repair cost], [quick repair cost rate]) and
// etc/upgrade.etc [repair cost rate by upgrade level]; EquipmentType is the
// normalized PVF [equipment type] token used for the repair-all eligibility
// filter.
type RepairCostEvidence struct {
	EquipmentType   string
	MaxDurability   int64
	RepairPrice     int64
	Grade           int64
	RepairCostRate  float64
	QuickRepairRate float64
	UpgradeRates    []float64
}

// RepairCostResolver resolves one candidate equipment item through the active
// runtime PVF. A nil resolver means the repair cost mutation must fail closed
// instead of trusting request or inventory-Extra metadata.
type RepairCostResolver func(itemID int64) (RepairCostEvidence, error)

// MailboxItemResolution is the current-PVF evidence used by the exact
// NoPack mailbox flow. Price is the item's [price] value; Kind and
// EquipmentType preserve the PVF type needed to frame a mailbox attachment
// without treating mutable inventory Extra data as the type authority.
// ListType plus SlotStart/SlotEnd and StackLimit are used when an attachment
// is claimed: they put the item in its real inventory owner/page rather than
// treating the op13 main-list expansion marker as a physical slot count.
// PreferQuickSlots is derived only from the authoritative PVF stackable type
// and never from client input, mailbox metadata, or an explicit player
// inventory move.
type MailboxItemResolution struct {
	Price            int64
	PVFPath          string
	Kind             string
	EquipmentType    string
	StackableType    string
	Durability       uint16
	ListType         byte
	SlotStart        int16
	SlotEnd          int16
	StackLimit       int64
	PreferQuickSlots bool
	// EpicBookEquipmentID is populated only when the active Epic Book catalog
	// maps this exact attachment item to an equipment balance. Such a fragment
	// is account ledger currency and must never be inserted into an inventory
	// material slot. EpicBookFragmentID is retained for the native op1149 row.
	EpicBookEquipmentID uint32
	EpicBookFragmentID  uint32
}

// MailboxItemResolver resolves each durable attachment template through the
// active runtime PVF. A nil resolver makes attachment sends fail closed.
type MailboxItemResolver func(itemID uint32) (MailboxItemResolution, error)

// PartyState 保存当前已经验证的单人队伍会话字段。
// 它只属于 game 连接临时态，真正多人队伍 owner 接入前不做持久化。
type PartyState struct {
	PartyID          int
	IsLeader         bool
	UserID           uint16
	UserState        byte
	Members          []PartyMemberState
	RequestPrefix0   byte
	RequestPrefix1   byte
	NameBytes        []byte
	MemberSelectCode byte
	MaxMembers       byte
	SelectionID      uint32
	SelectionCode    byte
	SelectionValue   uint16
	RecruitFlag      byte
	TargetMode       byte
	TargetDungeonID  uint16
	ReserveLeaveFlag byte
}

// PartyMemberState 是已验证队伍包里能安全写出的队员槽位状态。
type PartyMemberState struct {
	UserID uint16
	// Slot is the native fixed party position carried by class0/op9. It is
	// independent from the Members slice index: op335 may move a member into
	// an empty slot, and op121 changes leadership without renumbering members.
	Slot      byte
	UserState byte
	HPPercent byte
	MPPercent byte
}

// Result 表示模块化分流后的处理结果。
// ResponseAllowed=false 表示证据不足或被阻断，bridge 不能伪造成功 ACK。
type Result struct {
	Decision        Decision
	Handled         bool
	ResponseAllowed bool
	Operation       string
	Reason          string
	// DailyUseStates reports per-item daily use counters that changed during this
	// command. The bridge projects them to the client's own 每日可用次数 state
	// (op541, class 1): the item tooltip renders "每天可用次数 : 剩余/上限" from
	// that state, and nothing else feeds it -- the op14 item row does not carry
	// it at all (reverse engineering 2026-09-15: the client's only writer is
	// sub_01F20590, reached solely from the op541 handler at RVA 0x0197B2B0).
	// EffectItemInfo 请 bridge 在 ACK 与物品行刷新之后，补发客户端自己的
	// effect-item 记录（class0/op174："load effect item info"，
	// body = {u8 1, u32 itemID, u32 remainingSeconds}）。
	//
	// ⚠ 语义是**恢复**，不是"使用"：首次使用时客户端已经按 op44 在本地登记了这个
	// effect item（并打出"[道具]的效果时间还剩N"那一行），服务端再发 op174 会让它登记
	// 第二次 —— 服主 2026-10-03 看到的"提示出现两次"就是这么来的（他自己给出了决定性
	// 线索："药剂刚能用那一版（服务端还没发这些包）提示只有一行"）。同一约定在
	// experience_consumable_projection.go 里早就写明了：
	//
	//	"First use is handled locally by op44; op174 recreates the same item-owned
	//	 effect with its durable remaining duration."
	//
	// 所以**所有命令的使用路径都必须让它保持 nil**；目前只有登录/重登的恢复路径会用
	// op174，而那条路走的是 bridge 自己的直发（sendCurrentStatChangePotionEffectItemInfo
	// / sendCurrentExperienceConsumableBuffProjection），不经过本字段。这个字段留着是
	// 作为"恢复类投影"的通用出口，并被测试用 `== nil` 钉住（防止有人再把它接到使用路径上）。
	//
	// CharacterAddBuff 请 bridge 补发客户端自己的 buff 注册通知（class0/op487
	// CHARACTER_ADD_BUFF，body = {u8 1, i32 buffId, i32 0, i32 0, i32 0}）——这才是
	// **buff 栏图标**的来源。
	//
	// 现场 2026-10-03：只发 op174 时服主看到的是"[史诗Buff药剂]的效果时间还剩10分钟"
	// （其实那已经是客户端本地那一次 + op174 触发的第二次），图标没有出现。协议**形状**
	// 参照机器上的 C# 参考服务端 A21（`EpicBuffPotionDefinition.BuffId = 1136` +
	// `SpecialDungeonNotificationBuilder.BuildCharacterAddBuff` 的 17 字节记录体），
	// 但**协议号必须在我们的客户端里定标**：A21 的 NOTIPACKET 基址是 486，本客户端是
	// 485（见 stat_change_potion_projection.go 顶部注释与 tools/_gb_noti_table_re.py）。
	CharacterAddBuff *CharacterAddBuffProjection
	EffectItemInfo   *EffectItemInfoProjection
	DailyUseStates   []DailyUseState
	// ItemCooltimeStates reports the durable item cooldown windows a command
	// started (PVF `[cool time]` + `[cooltime maintenance]`). Nothing is sent
	// for them: the client reads the same PVF and starts its own local cooldown
	// when it consumes the item. They exist so the bridge can log the window and
	// so a refused reuse is observable; the restore path after a relog is the
	// item-state notification 172 list (item_cooltime_projection.go).
	ItemCooltimeStates []ItemCooltimeState
	UpperResponses     []UpperResponse
	// AreaBroadcastResponses are current-client notifications delivered to
	// other players who already share the requester's concrete town area. The
	// requester receives its own feedback through UpperResponses.
	AreaBroadcastResponses []UpperResponse
	// MailboxAlarmRecipientID asks the bridge to deliver the exact class0/0x63
	// mailbox alarm to this already-online character after the sender's ACK and
	// its local post-actions are fully written. Zero means no cross-session send.
	MailboxAlarmRecipientID uint16
	// ContractActivations carries premium contract units (item id -> count) that
	// a reward flow rolled but deliberately did not place in the bag. The bridge
	// extends the account entitlement for them instead of projecting item rows.
	ContractActivations map[int64]int64
	// ItemSlotRefreshes names only the durable item rows changed by this
	// command. The bridge reloads and projects these slots through the current
	// EXE's incremental item-update protocol after every ACK succeeds.
	ItemSlotRefreshes []ItemSlotRefresh
	// ItemListRebuilds 请 bridge 在 ItemSlotRefreshes 之后，对这些 list 各发一次
	// **整表重建**（op13）。
	//
	// 用在"原地改行"的场景：这个旧客户端的增量 op14 对**同槽同 item id、只有行内
	// 某个字段变化**的更新不重绘。怪物卡片升级到满级时正好是这种情况——前几级每次
	// 都换槽位，客户端看到的是"删一个+加一个"所以重画了；最后一级留在原槽，只有
	// 升级字节 03->04，界面就一直停在旧值，要重新选角色才正常。
	//
	// 同类问题此前在 list 7（宠物栏，见 sendSelectedPetInventoryListRefresh）和
	// list 38（公会勋章/守护珠合成）上都出现过，都是靠整表重建解决的。
	ItemListRebuilds []byte
	// EquipmentSlotRefreshes names worn equipment rows changed by this command.
	// The bridge sends each slot as its own class-0 op14/list3 packet after the
	// ACK; batching or replaying the complete equipped table is unsafe while the
	// current client owns live equipment objects.
	EquipmentSlotRefreshes []int16
	// PostResponseItemSlotClears emits synthetic empty op14 rows after the
	// command ACK and before ItemSlotRefreshes. Equipment replacement needs
	// this narrow invalidation phase so the client drops the old item core
	// before receiving the authoritative converted row.
	PostResponseItemSlotClears []ItemSlotRefresh
	// PostItemSlotRefreshResponses are current-client notifications whose
	// handlers must run only after the incremental item rows above are visible.
	// Mailbox extraction uses this narrow phase for class0/op98: sending the
	// clear-first op97 page while op95 still owns the selected attachment crashes
	// the current client, while sending op98 before op14 leaves a stale item row.
	PostItemSlotRefreshResponses []UpperResponse
	// op271's current result handler reloads its target while consuming the ACK,
	// so the target op14 must precede the class1 response.
	PreResponseItemSlotRefreshes []ItemSlotRefresh
	// CharacterGoldAfter requests the native list-0 slot-0 wallet projection.
	// It is sent after ordinary item rows but before post-item callbacks such as
	// mailbox op98; slot 0 is not an inventory row and must never be represented
	// as an ItemSlotRefresh.
	CharacterGoldAfter *int64
	PostActions        []PostAction
	SeriaLuck          *SeriaLuckProjection
	CeraWallet         *CeraWalletProjection
	// CeraSpent/CeraAfter/CeraSpentNote are the audit hooks for a command that
	// debits account Cera directly instead of going through the Cera-shop
	// checkout. 金库升级（cargo owner）是当前唯一一条这样的路径：它的成功回包里
	// 只带一个"扣完之后的余额"，而点券消费记录需要"扣了多少、为什么扣"，
	// 所以由模块自己把这两项填出来，bridge 统一记账（见 game_aligned.go）。
	// CeraSpent 为 0 表示本条命令没有点券消费。
	CeraSpent     int64
	CeraAfter     int64
	CeraSpentNote string
	// EpicBookBalances contains committed absolute balances. The bridge sends
	// their native class-0 op1149 refresh after the command ACK and before any
	// mailbox row removal callback.
	EpicBookBalances []EpicBookBalanceProjection
	// CustomAbilityGroupConverter is set only after op44 has proved the source
	// row without consuming it. dnfbridge arms its op1340 continuation after
	// the successful op44 response has been written.
	CustomAbilityGroupConverter *CustomAbilityGroupConverterProjection
}

type CustomAbilityGroupConverterProjection struct {
	CharacterID     uint16
	SourceListType  byte
	SourceSlotIndex int16
	SourceInstance  int32
	SourceItemID    int64
}

type SeriaLuckProjection struct {
	Before int64
	After  int64
}

// CeraWalletProjection is the complete account wallet required by the
// current client's class0/op53 reader. Domain owners return durable values;
// dnfbridge remains responsible for the wire encoding and packet order.
type CeraWalletProjection struct {
	Cera           int64
	TokenCera      int64
	HappyTokenCera int64
}

type EpicBookBalanceProjection struct {
	FragmentID uint32
	Balance    uint32
}

type ItemSlotRefresh struct {
	ListType  byte
	SlotIndex int16
}

// PostAction describes a bridge-owned state refresh that must run only after
// every durable owner mutation and protocol ACK in UpperResponses succeeds.
// Business modules select the action; the bridge remains responsible for the
// current EXE-specific packet body and session context.
type PostAction string

const (
	PostActionRefreshSelectedActorAppearance PostAction = "refresh_selected_actor_appearance"
	// The current EXE's class-0 op342 updates occupied equipment rows on an
	// already-created town actor.  It complements the mode0 redraw above:
	// mode0 removes stale worn parts, whereas op342 attaches newly equipped
	// equipment without creating a new actor generation.
	PostActionRefreshSelectedActorExpandedEquipment PostAction = "refresh_selected_actor_expanded_equipment"
	PostActionRefreshSelectedItemContainers         PostAction = "refresh_selected_item_containers"
	// 宠物本体与宠物装备的穿脱只改两处状态：宠物背包 list 7，以及 act 穿戴槽
	// 26（宠物本体）/ 27-29（宠物装备）。list 0/1/2/38 在这次操作里一个字节都不会
	// 变。重发这 4 张无关表代价极大：当前 EXE 在 op19 ACK 之后要先把已经收到的整批
	// 容器表处理完，才轮到后面那些包。2026-09-24 公网实测（用 30 秒 op1206 心跳
	// 标定时钟差 −4.384s、抖动 ±10ms，证明链路本身没有在途延迟）：ACK 到达后 11ms
	// 就被处理，但紧随其后的宠物行与 mode0 晚了 1.65~1.71 秒——这 1.7 秒正是客户端
	// 在重建背包页。本机同一套表只要 ~70ms，因为环回的电报一次性到达、客户端只重建
	// 一次。现场表现就是"脱下后背包图标要等一下才出现""穿上后要几秒宠物才出现"。
	// 因此宠物穿脱只重建真正变化的 list 7；这与公会勋章那次把五表刷新收窄成单表
	// 刷新（见 PostActionRefreshSelectedPersonalCargo）是同一处理。
	//
	// 它与 PostActionRefreshSelectedPetInventory 的区别只在于收尾：那个动作服务于
	// 宠物改名/喂养这类消耗品，不补发锁表；而穿脱之前走的是五表刷新，末尾会补发
	// 排序锁与 op251 物品锁快照（op13 单表重建同样会丢掉客户端进程内的锁标记），
	// 所以这里必须原样保留这两发，否则会连带改掉锁图标行为。
	PostActionRefreshSelectedWornCreatureInventory PostAction = "refresh_selected_worn_creature_inventory"
	// Personal cargo sorting must rebuild only list 2. Replaying list 0 before
	// the open warehouse list makes the current client re-home stackable ticket
	// objects into the backpack during its native sort reconciliation.
	PostActionRefreshSelectedPersonalCargo PostAction = "refresh_selected_personal_cargo"
	// Moving between list 0 and the open personal cargo makes the current
	// client discard its process-local wallet and fixed account-shared rows.
	// Re-publish only those authoritative rows after the native op19 ACK; a full
	// list-0 replay would replace unrelated live inventory objects.
	PostActionRefreshSelectedMainWallets PostAction = "refresh_selected_main_wallets"
	// Equipment slot item rows are list 3.  They carry mutable equipped-item
	// instance state (for example socket/emblem tail bytes) that mode0
	// appearance refreshes do not reliably update.
	PostActionRefreshSelectedEquipmentSlots PostAction = "refresh_selected_equipment_slots"
	// Worn creature slot 26 uses the legacy pet item row and must be refreshed
	// through its existing current-client op14 path after a pet enchant.
	PostActionRefreshSelectedEquippedCreatureItem PostAction = "refresh_selected_equipped_creature_item"
	// Account cargo is list 12.  It must be rebuilt by dnfbridge from the
	// authenticated account record and account inventory, never by replaying a
	// captured cargo body or reading the selected character's settings.
	PostActionRefreshSelectedAccountCargo PostAction = "refresh_selected_account_cargo"
	// 仓库底板（cargo pad）重建：当前 EXE 的仓库/金库面板在**创建时**按容量铺格子，
	// 之后收到的 op13 列表只更新行数据，不重建底板。现场 2026-09-18：买了"扩大帐号金库"
	// 之后格子还是画着锁、实际能放东西，切一下仓库页才正常。修复方式就是照进城镇那套
	// 顺序补一发——先重置 cargo pad（op391），再发物品列表。只用于**容量发生变化**的
	// 操作（开通/升级账号金库、扩容券）。
	PostActionResetCargoPadBeforeItemLists PostAction = "reset_cargo_pad_before_item_lists"
	PostActionRefreshSelectedActorSkills   PostAction = "refresh_selected_actor_skills"
	// Immediate EXP items and level-up tickets mutate the absolute character
	// progression snapshot. The current client needs op37 before its skill and
	// quest projections; an op44 ACK and source-row refresh alone leave the HUD
	// and visible level stale.
	PostActionRefreshSelectedCharacterProgression PostAction = "refresh_selected_character_progression"
	// Fatigue potions update the persisted remaining/used counters. The bridge
	// sends the absolute current-fatigue packet after the item ACK and refresh.
	PostActionRefreshSelectedFatigue PostAction = "refresh_selected_fatigue"
	// A [rent point] item charges the account rental wallet. The bridge sends
	// the absolute wallet state (op985) after the item ACK and row refresh;
	// op985 replaces the client's cached balance, so nothing else is needed
	// for the new 幸运星 count to show.
	PostActionRefreshSelectedRentalWallet PostAction = "refresh_selected_rental_wallet"
	// Creature state is the current scene class0/op105 raw table. The bridge
	// sends it only after the selected actor is scene-ready; it is not part of
	// the class1/op19 success body.
	PostActionRefreshSelectedCreatureState PostAction = "refresh_selected_creature_state"
	// Pet-feed mutations update the creature table and equipped growth body,
	// but must not replay a list-7 op14 row immediately after op44. The legacy
	// client can crash on that row sequence, so the bridge uses this narrower
	// post-ACK projection.
	PostActionRefreshSelectedCreatureGrowthState PostAction = "refresh_selected_creature_growth_state"
	// Pet-panel consumable rows use the stable full list-7 op13 projection
	// after the native op100 rename ACK; the incremental list-7 op14 path is
	// not stable on this client.
	PostActionRefreshSelectedPetInventory PostAction = "refresh_selected_pet_inventory"
	// Crystal-contract state is current NoPack class1/op898. It must follow a
	// durable type-97 activation so the native client can bind its persisted
	// cube selection to the newly active contract immediately.
	PostActionRefreshCrystalContractState PostAction = "refresh_crystal_contract_state"
	// Damage-font ownership is a character-stat projection consumed by NOTI
	// 1239. It follows the class1 op515 ACK and source-slot refresh.
	PostActionRefreshSelectedDamageFontState PostAction = "refresh_selected_damage_font_state"
	// Party-frame state is carried inside the current NoPack class0/op9 kind0
	// actor record. The bridge rebuilds that record from the selected actor and
	// live party slots before op153/op7 update the visible frame.
	PostActionRefreshSelectedPartyFrame PostAction = "refresh_selected_party_frame"
	// Quest completion tickets mutate the completed bitmap and may unlock new
	// active/acceptable entries. The bridge reloads all three native projections
	// only after op44 ACK and the source-slot refresh have been sent.
	PostActionRefreshSelectedQuestState PostAction = "refresh_selected_quest_state"
	// The current client renders op401/op437 failure ACKs as a silent failure.
	// Ask the bridge to add a visible explanation only when the inventory owner
	// proved that the rejected magic-seal mutation was caused by insufficient
	// gold; all other rejection paths remain protocol-only.
	PostActionNotifyRandomOptionGoldInsufficient PostAction = "notify_random_option_gold_insufficient"
	// 【op455 成功后的系统提示】
	//
	// 品级调整（class-1/op81）的"已成功重调该物品的品级"是客户端自己弹出的：
	// 客户端 op81 处理器收到成功 ACK 后本地显示，服务端只发那一个回包。
	// 封印属性品级调整（class-1/op455）不同：当前客户端为 op455 注册的处理器
	// 0x2137820 只在【失败】时按错误码弹提示（cmp byte [ebp+0xc],0 / jne 直接
	// 返回），成功分支什么都不做 —— 所以成功提示必须由服务端补，否则玩家只看到
	// 词条数字变了、没有任何反馈。
	// 这里沿用与 op81 相同措辞的系统页提示（见 random_option_notice.go）。
	PostActionNotifyRandomOptionResetSuccess PostAction = "notify_random_option_reset_success"
	// Account-shared cube/soul material deleted inside an active dungeon run
	// needs the current EXE's empty S0/0012 "inventory action complete" frame;
	// its S0/0012 reader collapses the shared inventory-action wait even with
	// entry_count=0. Without it the busy state bleeds to the next map and blocks
	// ordinary consumable use (us90 opcode0012-delete_item.py account_material_in_run).
	PostActionSendDeleteItemAccountMaterialCompletion PostAction = "send_delete_item_account_material_completion"
)

// UpperResponse 描述已经有证据允许 dnfbridge 发出的 raw upper 响应。
// Body 必须是完整业务包体；如果包体已经包含成功字节，调用方不能再套 upperSuccessBody。
type UpperResponse struct {
	MsgID          uint16
	Body           []byte
	Classification byte
	AllowCodec     bool
}

// DailyUseState is one item's daily use counter after a command mutated it.
// Used counts how many times the character has consumed the item in the current
// service day; the client shows the remaining value itself (limit - used).
type DailyUseState struct {
	ItemID uint32
	Used   byte
}

// ItemCooltimeState is one item's durable cooldown window after a command
// started it. ExpireAt is a unix second: it survives a 小退/重上, unlike the
// client's own in-memory cooldown gate.
type ItemCooltimeState struct {
	ItemID   int64
	ExpireAt int64
}

// Handler 是单个 DNF 业务域的命令处理入口。
type Handler interface {
	Domain() dnfenum.AlignedDomain
	Handle(context.Context, Request) (Result, error)
}

// Registry 按业务域保存 handler，避免 dnfbridge 直接堆积各玩法 switch。
type Registry struct {
	handlers map[dnfenum.AlignedDomain]Handler
}

// NewRegistry 创建模块化命令注册表。
func NewRegistry(handlers ...Handler) Registry {
	registry := Registry{handlers: make(map[dnfenum.AlignedDomain]Handler)}
	for _, handler := range handlers {
		if handler == nil {
			continue
		}
		registry.handlers[handler.Domain()] = handler
	}
	return registry
}

// DefaultRegistry 返回当前可用的模块化注册表。
// 具体业务 handler 尚未补齐时保持空表，只做证据化 pending/blocked 分流。
func DefaultRegistry() Registry {
	return NewRegistry()
}

// Route 根据 EXE 证据分类并交给对应业务域 handler。
func (r Registry) Route(ctx context.Context, req Request) (Result, bool, error) {
	// A numeric type is only a CmdPacket inside the current EXE command
	// dispatcher. Other command classes have independent registration tables.
	if req.CommandKnown && dnfenum.GameCmd(req.Command) != dnfenum.GameCmdCommand {
		return Result{}, false, nil
	}
	decision, ok := Classify(req.Opcode)
	if !ok {
		return Result{}, false, nil
	}
	if decision.Action == ActionBlocked {
		return Result{
			Decision:        decision,
			Handled:         true,
			ResponseAllowed: false,
			Operation:       string(decision.Action),
			Reason:          decision.Reason,
		}, true, nil
	}
	handler := r.handlers[decision.Domain]
	if handler == nil {
		return Result{
			Decision:        decision,
			Handled:         true,
			ResponseAllowed: false,
			Operation:       "module_unregistered",
			Reason:          "模块 handler 尚未注册",
		}, true, nil
	}
	req.Body = append([]byte(nil), req.Body...)
	result, err := handler.Handle(ctx, req)
	if result.Decision == (Decision{}) {
		result.Decision = decision
	}
	for i := range result.UpperResponses {
		result.UpperResponses[i].Body = append([]byte(nil), result.UpperResponses[i].Body...)
	}
	for i := range result.AreaBroadcastResponses {
		result.AreaBroadcastResponses[i].Body = append([]byte(nil), result.AreaBroadcastResponses[i].Body...)
	}
	for i := range result.PostItemSlotRefreshResponses {
		result.PostItemSlotRefreshResponses[i].Body = append([]byte(nil), result.PostItemSlotRefreshResponses[i].Body...)
	}
	result.PostResponseItemSlotClears = append([]ItemSlotRefresh(nil), result.PostResponseItemSlotClears...)
	result.EpicBookBalances = append([]EpicBookBalanceProjection(nil), result.EpicBookBalances...)
	result.PostActions = append([]PostAction(nil), result.PostActions...)
	return result, true, err
}

// EffectItemInfoProjection 是客户端 effect-item 记录的内容。
type EffectItemInfoProjection struct {
	// ItemID 是触发效果的道具（客户端按它去 variousbuffstackablelist.etc 查 buff）。
	ItemID uint32
	// RemainingSeconds 是剩余秒数；<=0 表示应当清除。
	RemainingSeconds uint32
}

// CharacterAddBuffProjection 是一条 buff 注册/注销通知。
type CharacterAddBuffProjection struct {
	// BuffID 是本次掷点得到的 buff id（1136 等），必须 > 0。
	BuffID int32
	// RemoveBuffID 是同组旧 buff（换了档位/换了药剂时先撤掉它），0 表示无需注销。
	RemoveBuffID int32
	// ExpireAtUnix 是这次注册的 buff **到期时刻**（Unix 秒，0 = 不排到期定时器）。
	//
	// 为什么需要它：op487 的记录体里**没有时长**（三个状态字段一律 0），客户端不会自己
	// 摘图标。参考服务端的做法是 SendAddAsync 之后 ScheduleRemoveForCurrentEffect ——
	// 服务端到期时补发 CHARACTER_DEL_BUFF。bridge 就用这个字段武装那个定时器。
	ExpireAtUnix int64
}
