package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	dungeonDropStackableList       = "stackable/stackable.lst"
	dungeonDropEquipmentList       = "equipment/equipment.lst"
	currentPVFExpirationDateLayout = "2006-01-02 15:04:05"
	// dungeonDropEquipmentPoolMaxWorkers 是通用装备池建池时的扫描并发上限。
	// 全量 14 万件 .equ 单线程约 5 秒，并发到 16 路后是秒级；上限 16 是为了不在
	// 小机器上把启动期的 CPU 抢满。
	dungeonDropEquipmentPoolMaxWorkers = 16
)

var currentPVFExpirationDateLocation = time.FixedZone("CST", 8*60*60)

var (
	errDungeonDropSourceRequired = errors.New("dnf dungeon drop PVF source is required")
	errDungeonDropItemUnresolved = errors.New("dnf dungeon drop item is not present in a supported PVF item list")
)

type dungeonDropItemKind string

const (
	dungeonDropItemStackable dungeonDropItemKind = "stackable"
	dungeonDropItemEquipment dungeonDropItemKind = "equipment"
)

// dungeonMonsterDropPoolEntry is one exact (item id, weight) pair from a
// monster document's [item] section. Weight selects an item within the pool;
// it is not treated as a drop probability.
type dungeonMonsterDropPoolEntry struct {
	ItemID uint32
	Weight uint32
}

type dungeonDropItemDefinition struct {
	ItemID             uint32
	Kind               dungeonDropItemKind
	PVFPath            string
	Name               string
	IconPath           string
	IconIndex          int64
	UsableJobs         []string
	PackageRewardIDs   []uint32
	Explain            string
	AttachType         string
	AuctionRuleFound   bool
	AuctionAllowed     bool
	EquipmentType      string
	CustomAbilityType  string
	CustomAbilityCount int64
	Grade              int64
	Rarity             int64
	MinimumLevel       int64
	MaximumLevel       int64
	CreationRate       int64
	StackableType      string
	EquipmentEffectID  uint16
	StackLimit         int64
	SlotStart          int16
	SlotEnd            int16
	Durability         uint16
	ExpirationDate     time.Time
	UsablePeriodDays   int64
	// DailyDeleteItem 是 PVF 的 `[daily delete item] 1`：该物品在每日重置时删除。
	// 与 ExpirationDate / UsablePeriodDays 无关，见 daily_delete_item.go。
	DailyDeleteItem bool
	Price           int64
	PriceFound      bool
	ActionType      string
	// LotteryUseCost 是 PVF `[lottery use cost]`：使用这类"抽奖消耗品"时要扣的资源量。
	// LotteryEntries 是它的 `[int data]` 抽奖表，按仓库既有的 (item, weight, count)
	// 三元组解析 —— 与 [booster info] 的奖励项同一个形状（见本文件 line 510 的注释）。
	// 目前只有 `[monster tarot card]`（卢克塔罗牌，例：stackable/10159001/10159697.stk）
	// 用到；字段是**只读解析**，还没有任何使用路径消费它们。
	LotteryUseCost int64
	LotteryEntries []dungeonDropLotteryEntry
	ActionValues   *dungeonDropActionValues
	// FavorNPCIDs 是 PVF `[npc select category]` 的 NPC 白名单：`[action type] [use favor point] N`
	// 类道具（例：充满爱慕的信 490700963）用弹窗选 NPC 时，客户端只列这些 NPC；
	// 服务端也按它校验（2026-10-16，见 dnfbridge/returnee_npc_gift.go 的信件分支）。
	FavorNPCIDs         []int64
	ActionVillageOnly   bool
	ActionUsableVillage bool
	ActionUsableDungeon bool
	// InventoryListType is the list a reward of this item is granted into. The
	// current PVF has no per-item list, so it stays at the main inventory (0)
	// and callers assert that before granting.
	InventoryListType         byte
	ActionUsableSeriaRoom     bool
	ActionDisallowAssault     bool
	ActionUsableDungeonIDs    []int64
	LimitDungeonIDs           []int64
	ActionStringData          []string
	ActionIntData             []int64
	DamageFontIndex           uint16
	DamageFontExpirationMode  alignedcmd.DamageFontExpirationMode
	DamageFontPeriodDays      int64
	DamageFontFixedExpiration time.Time
	// HP/MP recovery mirrors the runtime PVF `[hp recovery]` / `[mp recovery]`
	// sections as `<operator> <amount> <duration>`. Operator is `+` for a flat
	// amount and `%` for a percentage of the drinker's maximum. See
	// parseCurrentConsumableRecovery for why the second value is not a maximum.
	HasHPRecovery          bool
	HPRecoveryOperator     string
	HPRecoveryAmount       int64
	HPRecoveryDurationMS   int64
	HasMPRecovery          bool
	MPRecoveryOperator     string
	MPRecoveryAmount       int64
	MPRecoveryDurationMS   int64
	ExpBonusPercent        int64
	EffectDurationMS       int64
	ExperienceSourceKey    uint32
	DirectExperienceType   string
	DirectExperienceValues []int64
	LevelUpRange           int64
	LevelUpOutcomes        []levelUpTicketOutcome
	TotalUsableCount       int64
	// 冷却（PVF `[cool time]` 毫秒 + `[cooltime group]` + `[cooltime maintenance]`）。
	// 只有声明了 `[cooltime maintenance]`（且值非零）的道具才由服务端**持久保留**冷却：
	// 客户端的冷却门是进程内内存（cmd/scratch/potionpatch 已记"no packet is sent while
	// the client blocks the action"），所以小退重上就没了。现场 2026-09-17 运营报
	// "冷却时间为1天的消耗品 小退重上 冷却时间就消失了"（首日度假礼盒 490702504：
	// [cool time] 86400000、[cooltime group] 66、[cooltime maintenance] 1 1 1）。
	// 86 参考服务端同口径：HasCooltimeMaintenance + CoolTime + ResolveCooltimeGroup。
	CooltimeMS                   int64
	CooltimeGroup                int64
	HasCooltimeMaintenance       bool
	UseActionPacket              int64
	PetFeedEffectDurationSeconds int64
	PetFeedSkillChargePercent    int64
	PetFeedSatietyDelta          int64
	CargoTransportStone          bool
	CargoTransportStoneType      int64
	CreatureTransportStone       bool
	CreatureTransportStoneType   int64
	ResealLevelMin               int64
	ResealLevelMax               int64
	ResealOneShot                bool
	HasGrowthType                bool
	PeriodEquipment              bool
	// 限购：PVF `[purchase limit]`（`` `scope` `period` count ``）或旧字段
	// `[daily purchase limit]`（`` `scope` count ``，周期=每天）。
	PurchaseLimitFound bool
	PurchaseLimit      purchaselimit.Policy
	// PurchaseLimitError 记录字段存在但形状不认识的情况；此时按“无策略”处理，
	// 以免因为一个畸形字段把整件道具判为不可解析。
	PurchaseLimitError string
}

type dungeonDropActionValues struct {
	Values []int64
}

type levelUpTicketOutcome struct {
	Range  int64
	Weight int64
}

type dungeonDropItemReference struct {
	kind dungeonDropItemKind
	path string
}

// dungeonDropGradeRarityKey mirrors the C# domain pool key.  It is not a
// protocol value: grade and rarity are read from the runtime PVF and only
// select a real item definition before the existing op38 item wire is built.
type dungeonDropGradeRarityKey struct {
	Grade  int64
	Rarity int64
}

type dungeonDropWeightedItem struct {
	ItemID uint32
	Weight uint32
}

// dungeonDropGenericPoolAudit 是通用装备池的构成快照。它存在的理由是：
// 「某个稀有度在某个等级段一件候选都没有」这种洞以前完全不可见——建池失败只会
// 让 SelectGenericEquipment 静默降级成 rarity 0，产出照旧、日志干净。预载阶段把
// 这份快照打进服务器日志，同类问题下次会自己冒出来。
type dungeonDropGenericPoolAudit struct {
	Scanned               int
	EquipmentMissing      int
	ReadFailed            int
	RarityMissing         int
	CreationRateAbsent    int
	LevelOutOfRange       int
	ExcludedEquipmentType int
	Buckets               int
	Items                 int
	ByRarity              [7]int
	LowestLevel           [7]int
	HighestLevel          [7]int
	Fallbacks             uint64
}

// pvfDungeonDropCatalog owns only static PVF mapping. It deliberately does
// not decide how often a monster drops, allocate a scene slot, or mutate the
// player's inventory. Those are separate runtime/asset transactions.
type pvfDungeonDropCatalog struct {
	mu              sync.Mutex
	gmMailOptionMu  sync.Mutex
	gmAvatarSetMu   sync.Mutex
	gmAvatarIndexMu sync.Mutex

	source       dnfpvf.Source
	monsterPaths map[int64]string
	itemRefs     map[uint32]dungeonDropItemReference
	// upgradeAssistPath* 是 equipment.lst 的 "id -> 路径" 全表缓存，只给"已装备道具的
	// 强化助力"那条路用（upgrade_worn_assist.go）：目录索引 itemRefs 只收它关心的 kind，
	// 宠物那类 .equ 可能不在里面，所以那边需要一份不按 kind 过滤的索引。
	upgradeAssistPathMu    sync.Mutex
	upgradeAssistPathIndex map[uint32]string
	upgradeAssistPathErr   error
	stackableIDs           []uint32
	equipmentIDs           []uint32
	poolCache              map[int64][]dungeonMonsterDropPoolEntry
	itemCache              map[uint32]dungeonDropItemDefinition
	experienceSourceKeys   map[uint32]uint32
	gmEmblemOptions        map[uint32][]GMItemSummary
	gmEnchantOptions       map[uint32][]GMItemSummary
	gmAvatarSetIDs         []uint32
	gmAvatarSetReady       bool
	gmAvatarSetsByJob      map[string][]GMAvatarSetSummary

	// The generic pools deliberately stay separate from itemCache.  They are
	// built once, either by the service's "dungeon drop catalog" preload stage
	// or lazily on the first actual generic drop roll, and have no effect on
	// packet layout or persistence.
	genericLoaded     bool
	genericErr        error
	genericStackables map[dungeonDropGradeRarityKey][]uint32
	genericEquipment  map[dungeonDropGradeRarityKey][]dungeonDropWeightedItem
	genericAudit      dungeonDropGenericPoolAudit
	// 「城镇增益药剂」的 buff 表（etc/variousbuffstackablelist.etc +
	// etc/variousbufflist.etc）。惰性加载：合成测试源里没有这两张表时必须能优雅降级到
	// `[explain]` 文字回退，不能让整条 op44 因为读不到表而失败。
	variousBuffOnce sync.Once
	variousBuffs    currentVariousBuffTables
	variousBuffErr  error
	// genericFallbacks counts every request whose rarity had no candidate and
	// was degraded to rarity 0.  The degradation itself predates this field
	// (the reference implementation also passes allowFallback), but it used to
	// be invisible; callers now diff this map and log the delta.
	genericFallbacks map[dungeonDropGradeRarityKey]uint64
}

func newPVFDungeonDropCatalog(source dnfpvf.Source) (*pvfDungeonDropCatalog, error) {
	if source == nil {
		return nil, errDungeonDropSourceRequired
	}
	monsterPaths, _, err := loadDungeonDropList(source, "monster/monster.lst")
	if err != nil {
		return nil, fmt.Errorf("load dungeon drop monster list: %w", err)
	}
	stackablePaths, stackableIDs, err := loadDungeonDropList(source, dungeonDropStackableList)
	if err != nil {
		return nil, fmt.Errorf("load dungeon drop stackable list: %w", err)
	}
	equipmentPaths, equipmentIDs, err := loadDungeonDropList(source, dungeonDropEquipmentList)
	if err != nil {
		return nil, fmt.Errorf("load dungeon drop equipment list: %w", err)
	}
	itemRefs := make(map[uint32]dungeonDropItemReference, len(stackablePaths)+len(equipmentPaths))
	for itemID, itemPath := range stackablePaths {
		if itemID <= 0 || itemID > math.MaxUint32 {
			continue
		}
		itemRefs[uint32(itemID)] = dungeonDropItemReference{kind: dungeonDropItemStackable, path: itemPath}
	}
	for itemID, itemPath := range equipmentPaths {
		if itemID <= 0 || itemID > math.MaxUint32 {
			continue
		}
		if _, exists := itemRefs[uint32(itemID)]; !exists {
			itemRefs[uint32(itemID)] = dungeonDropItemReference{kind: dungeonDropItemEquipment, path: itemPath}
		}
	}
	experienceSourceKeys, err := loadExperienceSourceKeys(source)
	if err != nil {
		// Older and focused test PVFs may omit this optional table. The item
		// effect remains usable; only the legacy source-key hint is unavailable.
		experienceSourceKeys = make(map[uint32]uint32)
	}
	return &pvfDungeonDropCatalog{
		source:               source,
		monsterPaths:         monsterPaths,
		itemRefs:             itemRefs,
		stackableIDs:         append([]uint32(nil), stackableIDs...),
		equipmentIDs:         append([]uint32(nil), equipmentIDs...),
		poolCache:            make(map[int64][]dungeonMonsterDropPoolEntry),
		itemCache:            make(map[uint32]dungeonDropItemDefinition),
		genericFallbacks:     make(map[dungeonDropGradeRarityKey]uint64),
		experienceSourceKeys: experienceSourceKeys,
		gmEmblemOptions:      make(map[uint32][]GMItemSummary),
		gmEnchantOptions:     make(map[uint32][]GMItemSummary),
		gmAvatarSetsByJob:    make(map[string][]GMAvatarSetSummary),
	}, nil
}

const dungeonExperienceSourceStackableList = "etc/variousbuffstackablelist.etc"

func loadExperienceSourceKeys(source dnfpvf.Source) (map[uint32]uint32, error) {
	text, err := source.ReadText(dungeonExperienceSourceStackableList)
	if err != nil {
		return nil, err
	}
	document, err := dnfpvf.Parse(dungeonExperienceSourceStackableList, text)
	if err != nil {
		return nil, err
	}
	keys := make(map[uint32]uint32)
	tokens := document.Tokens
	for i := 0; i < len(tokens); i++ {
		if !isPVFSectionToken(tokens[i], "index") {
			continue
		}
		var itemID, sourceKey int64
		for j := i + 1; j < len(tokens); j++ {
			if isPVFSectionToken(tokens[j], "index") || isPVFSectionToken(tokens[j], "/index") {
				break
			}
			if itemID == 0 && tokens[j].Kind == dnfpvf.TokenInt {
				itemID = tokens[j].Int
				continue
			}
			if isPVFSectionToken(tokens[j], "buff list") {
				for k := j + 1; k < len(tokens); k++ {
					if tokens[k].Kind == dnfpvf.TokenInt {
						sourceKey = tokens[k].Int
						break
					}
					if isPVFSectionToken(tokens[k], "index") || isPVFSectionToken(tokens[k], "/index") {
						break
					}
				}
			}
		}
		if itemID > 0 && itemID <= math.MaxUint32 && sourceKey > 0 && sourceKey <= math.MaxUint32 {
			keys[uint32(itemID)] = uint32(sourceKey)
		}
	}
	return keys, nil
}

func isPVFSectionToken(token dnfpvf.Token, name string) bool {
	return token.Kind == dnfpvf.TokenSection && strings.EqualFold(strings.TrimSpace(token.Value), name)
}

func loadDungeonDropList(source dnfpvf.Source, listPath string) (map[int64]string, []uint32, error) {
	text, err := source.ReadText(listPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", listPath, err)
	}
	document, err := dnfpvf.Parse(listPath, text)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", listPath, err)
	}
	entries := dnfpvf.ParseList(document)
	out := make(map[int64]string, len(entries))
	order := make([]uint32, 0, len(entries))
	for _, entry := range entries {
		clean := cleanDungeonDropPath(entry.Path)
		if entry.ID <= 0 || entry.ID > math.MaxUint32 || clean == "" {
			continue
		}
		if _, exists := out[entry.ID]; !exists {
			out[entry.ID] = clean
			order = append(order, uint32(entry.ID))
		}
	}
	return out, order, nil
}

func (c *pvfDungeonDropCatalog) MonsterPool(monsterID int64) ([]dungeonMonsterDropPoolEntry, error) {
	if c == nil || c.source == nil {
		return nil, errDungeonDropSourceRequired
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.poolCache[monsterID]; ok {
		return cloneDungeonMonsterDropPool(cached), nil
	}
	listedPath, ok := c.monsterPaths[monsterID]
	if !ok {
		return nil, nil
	}
	docPath, text, err := readDungeonDropText(c.source, "monster", listedPath)
	if err != nil {
		return nil, fmt.Errorf("read drop pool monster=%d path=%s: %w", monsterID, listedPath, err)
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse drop pool monster=%d path=%s: %w", monsterID, docPath, err)
	}
	pool := parseDungeonMonsterDropPool(document)
	c.poolCache[monsterID] = cloneDungeonMonsterDropPool(pool)
	return cloneDungeonMonsterDropPool(pool), nil
}

func (c *pvfDungeonDropCatalog) ResolveItem(itemID uint32) (dungeonDropItemDefinition, error) {
	if c == nil || c.source == nil {
		return dungeonDropItemDefinition{}, errDungeonDropSourceRequired
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.itemCache[itemID]; ok {
		return cached, nil
	}
	reference, ok := c.itemRefs[itemID]
	if !ok {
		return dungeonDropItemDefinition{}, fmt.Errorf("%w: item_id=%d", errDungeonDropItemUnresolved, itemID)
	}
	root := string(reference.kind)
	docPath, text, err := readDungeonDropText(c.source, root, reference.path)
	if err != nil {
		return dungeonDropItemDefinition{}, fmt.Errorf("read drop item=%d path=%s: %w", itemID, reference.path, err)
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return dungeonDropItemDefinition{}, fmt.Errorf("parse drop item=%d path=%s: %w", itemID, docPath, err)
	}
	definition := dungeonDropItemDefinition{ItemID: itemID, Kind: reference.kind, PVFPath: docPath}
	definition.Name, _ = document.Text("name")
	definition.IconPath, _ = document.Text("icon")
	definition.IconIndex, _ = document.Int("icon")
	definition.UsableJobs = append([]string(nil), document.Texts("usable job")...)
	if packageValues := document.Ints("package data"); len(packageValues) >= 2 {
		definition.PackageRewardIDs = make([]uint32, 0, len(packageValues)/2)
		for index := 0; index+1 < len(packageValues); index += 2 {
			itemID := packageValues[index]
			if itemID > 0 && itemID <= math.MaxUint32 {
				definition.PackageRewardIDs = append(definition.PackageRewardIDs, uint32(itemID))
			}
		}
	}
	definition.Explain = strings.Join(document.Texts("explain"), "\n")
	definition.Rarity, _ = document.Int("rarity")
	definition.MinimumLevel, _ = document.Int("minimum level")
	definition.CreationRate, _ = document.Int("creation rate")
	definition.ExperienceSourceKey = c.experienceSourceKeys[itemID]
	definition.AttachType, _ = document.Text("attach type")
	if policy, found, err := parseCurrentPurchaseLimitSection(itemID, document); err != nil {
		definition.PurchaseLimitError = err.Error()
	} else if found {
		definition.PurchaseLimitFound = true
		definition.PurchaseLimit = policy
	}
	if available, found := document.Int("available auction"); found {
		definition.AuctionRuleFound = true
		definition.AuctionAllowed = available != 0
	}
	if price, found := document.Int("price"); found && price >= 0 {
		definition.Price = price
		definition.PriceFound = true
	}
	expirationDate, err := parseCurrentPVFExpirationDate(document)
	if err != nil {
		return dungeonDropItemDefinition{}, fmt.Errorf("parse drop item=%d path=%s expiration date: %w", itemID, docPath, err)
	}
	definition.ExpirationDate = expirationDate
	if usablePeriod, found := document.Int("usable period"); found && usablePeriod > 0 {
		definition.UsablePeriodDays = usablePeriod
	}
	// `[daily delete item] 1` 是和上面那套期限字段**完全独立**的一条规则：物品在
	// 每日重置（服务日边界，北京 06:00）时被删除，客户端把它渲染成"期限 1 天"。
	//
	// 它必须单独读，原因见 currentDailyDeleteItemStateKey 的注释：剥离期限的两个
	// 工具（cmd/scratch/pvfremoveitemexpiry、removeitemexpirydb）摘的是
	// [expiration date] / [usable datetime] / [usable period] /
	// [usable period after unsealing]，**不含这一条**，所以它在本服的运行时 PVF
	// 里仍然有效，而上面那几个字段已经被有意屏蔽了。
	if daily, found := document.Int("daily delete item"); found && daily != 0 {
		definition.DailyDeleteItem = true
	}
	if reference.kind == dungeonDropItemEquipment {
		definition.SlotStart, definition.SlotEnd = 9, 64
		definition.EquipmentType, _ = document.Text("equipment type")
		definition.CustomAbilityType, _ = document.Text("custom ability type")
		definition.CustomAbilityCount, _ = document.Int("custom ability count")
		definition.Grade, _ = document.Int("grade")
		_, definition.HasGrowthType = document.Section("growth type")
		_, definition.PeriodEquipment = document.Section("period equipment")
		for _, name := range []string{"durability", "maximum durability", "max durability"} {
			value, found := document.Int(name)
			if !found || value <= 0 {
				continue
			}
			if value > math.MaxUint16 {
				value = math.MaxUint16
			}
			definition.Durability = uint16(value)
			break
		}
	} else {
		definition.StackableType, _ = document.Text("stackable type")
		parseCurrentTransportStoneDefinition(document, &definition)
		// 使用类动作：`[action type] \`[use favor point]\` 1000` 这类道具的动作标签与数值
		// 以前只在"伤害字体"那条专属解析里读，stackable 通用分支只拿它推断 stackable type。
		// 2026-10-16：这里把标签/数值留到定义上，`[use favor point]` 再带上
		// `[npc select category]` 的 NPC 白名单，供 op812 信件分支使用。
		if actionLabel, found := document.Text("action type"); found {
			definition.ActionType = strings.TrimSpace(actionLabel)
			if values := document.Ints("action type"); len(values) > 0 {
				definition.ActionValues = &dungeonDropActionValues{Values: append([]int64(nil), values...)}
			}
			if strings.EqualFold(definition.ActionType, currentPVFUseFavorPointAction) {
				definition.FavorNPCIDs = append([]int64(nil), document.Ints("npc select category")...)
			}
		}
		if resealLevels := document.Ints("reseal lv"); len(resealLevels) >= 2 {
			definition.ResealLevelMin = resealLevels[0]
			definition.ResealLevelMax = resealLevels[1]
		}
		_, definition.ResealOneShot = document.Section("reseal oneshot")
		if strings.TrimSpace(definition.StackableType) == "" {
			actionType, _ := document.Text("action type")
			definition.StackableType = currentPVFInferredStackableType(actionType)
		}
		if effectType, found := document.Text("increase status type"); found {
			definition.DirectExperienceType = strings.TrimSpace(effectType)
			definition.DirectExperienceValues = append([]int64(nil), document.Ints("increase status type")...)
		}
		if levelUpRange, found := document.Int("levelup range"); found && levelUpRange > 0 {
			definition.LevelUpRange = levelUpRange
			for _, group := range parseMagicBoxBoosterGroups(document) {
				for _, entry := range group.Entries {
					// Level-up ticket [booster info]/[etc] reuses the
					// ordinary (item, weight, count) triple as
					// (sentinel=1, probability, gained levels).
					if entry.ItemID == 1 && entry.Weight > 0 && entry.Count > 0 && entry.Count <= levelUpRange {
						definition.LevelUpOutcomes = append(definition.LevelUpOutcomes, levelUpTicketOutcome{
							Range: entry.Count, Weight: entry.Weight,
						})
					}
				}
			}
		}
		if maximumLevel, found := document.Int("maximum level"); found && maximumLevel > 0 {
			definition.MaximumLevel = maximumLevel
		}
		if totalUsableCount, found := document.Int("total usable count"); found && totalUsableCount > 0 {
			definition.TotalUsableCount = totalUsableCount
		}
		// `[lottery use cost]` + `[int data]`：抽奖类消耗品（[monster tarot card]）的
		// 花费与奖池。两者都只在道具自己声明时才填，缺一个就整条不成立（由使用侧
		// fail closed 判断），这里不做任何语义推断。
		if lotteryUseCost, found := document.Int("lottery use cost"); found && lotteryUseCost > 0 {
			definition.LotteryUseCost = lotteryUseCost
		}
		definition.LotteryEntries = parseDungeonDropLotteryEntries(document.Ints("int data"))
		// 冷却：`[cool time]` 毫秒 + `[cooltime group]` + `[cooltime maintenance]`。
		// maintenance 段存在且至少一个值非零 = 这个冷却要**跨会话保留**，由服务端落库；
		// 只有 [cool time] 而没有 maintenance 的道具（例如 10 秒冷却的红蓝药）保持纯客户端
		// 语义，服务端不记、不下发——否则会多出一堆无意义的行。
		if cooltimeMS, found := document.Int("cool time"); found && cooltimeMS > 0 {
			definition.CooltimeMS = cooltimeMS
		}
		if cooltimeGroup, found := document.Int("cooltime group"); found && cooltimeGroup > 0 {
			definition.CooltimeGroup = cooltimeGroup
		}
		for _, value := range document.Ints("cooltime maintenance") {
			if value != 0 {
				definition.HasCooltimeMaintenance = true
				break
			}
		}
		if useActionPacket, found := document.Int("use action packet"); found && useActionPacket > 0 {
			definition.UseActionPacket = useActionPacket
		}
		if normalizeDungeonDropStackableType(definition.StackableType) == "[feed]" {
			values := document.Ints("int data")
			if len(values) >= 3 {
				definition.PetFeedEffectDurationSeconds = values[0]
				definition.PetFeedSkillChargePercent = values[1]
				definition.PetFeedSatietyDelta = values[2]
			}
		}
		if rate, found := document.Number("exp bonus rate"); found && rate > 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0) {
			percent := math.Round(rate * 100)
			if percent > 0 && percent <= math.MaxInt64 {
				definition.ExpBonusPercent = int64(percent)
			}
		}
		if duration, found := document.Int("stat change duration"); found && duration > 0 {
			definition.EffectDurationMS = duration
		}
		if effectID, found := document.Int("int data"); found && effectID > 0 && effectID <= math.MaxUint16 {
			definition.EquipmentEffectID = uint16(effectID)
		}
		if limit, found := document.Int("stack limit"); found && limit > 0 {
			definition.StackLimit = limit
		}
		definition.SlotStart, definition.SlotEnd = dungeonDropStackableSlotRange(definition.StackableType)
		parseCurrentConsumableRecovery(document, &definition)
		if err := parseCurrentDamageFontDefinition(document, &definition); err != nil {
			return dungeonDropItemDefinition{}, fmt.Errorf("parse drop item=%d path=%s damage font: %w", itemID, docPath, err)
		}
	}
	c.itemCache[itemID] = definition
	return definition, nil
}

// parseCurrentConsumableRecovery reads the top-level `[hp recovery]` and
// `[mp recovery]` sections of a stackable document.
//
// The layout is `<operator> <amount> <duration>`, NOT a `<min> <max>` roll
// range. The runtime PVF settles this outright: 专家HP药剂 declares
// `+ 1080 1000` and 达人HP药剂 `+ 1520 1000`, so the second value is smaller
// than the first and cannot be a maximum; every one of the twelve ordinary
// HP/MP potions carries exactly 1000 there while their first values climb
// 70/180/410/710/1080/1520 in tier order, which is the healed amount. Across
// the whole archive the second value only ever takes a handful of values
// (500/1000/2000) unrelated to the first.
//
// The operator is the section's text token: `+` is a flat amount and `%` is a
// percentage of the drinker's maximum. Both are real - 646 stackables declare a
// recovery section, 67 HP and 54 MP of them with `+` and the rest with `%`.
// The duration is kept only so a later reader is not tempted to re-derive it;
// nothing on the wire consumes it, because the recovery ACK carries a single
// scalar.
func parseCurrentConsumableRecovery(document *dnfpvf.Document, definition *dungeonDropItemDefinition) {
	if document == nil || definition == nil {
		return
	}
	for _, recovery := range []struct {
		section  string
		found    *bool
		operator *string
		amount   *int64
		duration *int64
	}{
		{"hp recovery", &definition.HasHPRecovery, &definition.HPRecoveryOperator, &definition.HPRecoveryAmount, &definition.HPRecoveryDurationMS},
		{"mp recovery", &definition.HasMPRecovery, &definition.MPRecoveryOperator, &definition.MPRecoveryAmount, &definition.MPRecoveryDurationMS},
	} {
		operator, present := document.Text(recovery.section)
		values := document.Ints(recovery.section)
		if !present && len(values) == 0 {
			continue
		}
		*recovery.found = true
		*recovery.operator = strings.TrimSpace(operator)
		if len(values) >= 1 {
			*recovery.amount = values[0]
		}
		if len(values) >= 2 {
			*recovery.duration = values[1]
		}
	}
}

// parseCurrentTransportStoneDefinition accepts both transport-stone layouts
// present in current and legacy PVFs. Some older documents expose a dedicated
// [cargo transport stone]/[creature transport stone] section. Current 90CN
// documents instead encode the same type as the integer payload of
// [action type], for example `[cargo transport stone] 8`. Treat the action
// name and its integer as one contract; item names and archive paths are not
// authoritative enough to infer a transfer rule.
func parseCurrentTransportStoneDefinition(document *dnfpvf.Document, definition *dungeonDropItemDefinition) {
	if document == nil || definition == nil {
		return
	}
	definition.CargoTransportStoneType, definition.CargoTransportStone = document.Int("cargo transport stone")
	definition.CreatureTransportStoneType, definition.CreatureTransportStone = document.Int("creature transport stone")
	actionType, found := document.Text("action type")
	actionValues := document.Ints("action type")
	if !found || len(actionValues) == 0 {
		return
	}
	switch normalizeDungeonDropStackableType(actionType) {
	case "[cargo transport stone]":
		definition.CargoTransportStoneType = actionValues[0]
		definition.CargoTransportStone = true
	case "[creature transport stone]":
		definition.CreatureTransportStoneType = actionValues[0]
		definition.CreatureTransportStone = true
	}
}

// currentPVFUseFavorPointAction 是 PVF 里"直接加 NPC 好感度"这一动作标签
// （充满爱慕的信 490700963、490700918、10157249/10157250/10157251）。
const currentPVFUseFavorPointAction = "[use favor point]"

// currentPVFInferredStackableType covers only a proven malformed current-PVF
// document. Item 490008569 omits [stackable type] but carries the dedicated
// [gold kaleido box] action, while the other 26 golden-kaleido templates use
// [gold kaleido]. Keep this exact and fail closed for every unknown omission;
// the server must not invent a category from an item name or archive path.
func currentPVFInferredStackableType(actionType string) string {
	switch normalizeDungeonDropStackableType(actionType) {
	case "[gold kaleido box]":
		return "[gold kaleido]"
	default:
		return ""
	}
}

func parseCurrentDamageFontDefinition(document *dnfpvf.Document, definition *dungeonDropItemDefinition) error {
	if document == nil || definition == nil {
		return nil
	}
	definition.ActionStringData = append([]string(nil), document.Texts("string data")...)
	definition.ActionIntData = append([]int64(nil), document.Ints("int data")...)
	definition.ActionUsableDungeonIDs = append([]int64(nil), document.Ints("action usable dungeon")...)
	definition.LimitDungeonIDs = append([]int64(nil), document.Ints("limit dungeon")...)
	for _, place := range document.Texts("action usable place") {
		switch strings.ToLower(strings.TrimSpace(place)) {
		case "[village]":
			definition.ActionUsableVillage = true
			definition.ActionVillageOnly = true
		case "[dungeon]":
			definition.ActionUsableDungeon = true
		case "[seria room]":
			definition.ActionUsableSeriaRoom = true
		case "[dont use assult]":
			definition.ActionDisallowAssault = true
		}
	}
	actionType, found := document.Text("action type")
	if !found {
		return nil
	}
	definition.ActionType = strings.TrimSpace(actionType)
	if values := document.Ints("action type"); len(values) > 0 {
		definition.ActionValues = &dungeonDropActionValues{Values: append([]int64(nil), values...)}
	}
	if !strings.EqualFold(definition.ActionType, "[add damage font skin]") {
		return nil
	}
	fontIndex, found := document.Int("index")
	if !found || fontIndex <= 0 || fontIndex > math.MaxUint16 {
		return fmt.Errorf("font index %d invalid", fontIndex)
	}
	definition.DamageFontIndex = uint16(fontIndex)

	texts := document.Texts("expiration info")
	ints := document.Ints("expiration info")
	for index, token := range texts {
		switch {
		case strings.EqualFold(strings.TrimSpace(token), "[period]"):
			if len(ints) == 0 || ints[0] <= 0 {
				return fmt.Errorf("period expiration missing positive days")
			}
			definition.DamageFontExpirationMode = alignedcmd.DamageFontExpirationPeriod
			definition.DamageFontPeriodDays = ints[0]
			return nil
		case strings.EqualFold(strings.TrimSpace(token), "[date]"):
			if index+1 >= len(texts) {
				return fmt.Errorf("fixed expiration missing date")
			}
			expiresAt, err := time.ParseInLocation(currentPVFExpirationDateLayout, strings.TrimSpace(texts[index+1]), currentPVFExpirationDateLocation)
			if err != nil {
				return fmt.Errorf("fixed expiration %q: %w", texts[index+1], err)
			}
			if expiresAt.Unix() <= 0 || expiresAt.Unix() > math.MaxUint32 {
				return fmt.Errorf("fixed expiration unix %d exceeds current u32 wire", expiresAt.Unix())
			}
			definition.DamageFontExpirationMode = alignedcmd.DamageFontExpirationFixed
			definition.DamageFontFixedExpiration = expiresAt.UTC()
			return nil
		case strings.EqualFold(strings.TrimSpace(token), "[unlimit]"):
			definition.DamageFontExpirationMode = alignedcmd.DamageFontExpirationUnlimited
			return nil
		}
	}
	return fmt.Errorf("expiration info is missing a supported period/date/unlimit mode")
}

func parseCurrentPVFExpirationDate(document *dnfpvf.Document) (time.Time, error) {
	if document == nil {
		return time.Time{}, nil
	}
	raw, found := document.Text("expiration date")
	raw = strings.TrimSpace(raw)
	if !found || raw == "" {
		return time.Time{}, nil
	}
	expirationDate, err := time.ParseInLocation(currentPVFExpirationDateLayout, raw, currentPVFExpirationDateLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("value=%q layout=%q: %w", raw, currentPVFExpirationDateLayout, err)
	}
	unix := expirationDate.Unix()
	if unix <= 0 || unix > math.MaxUint32 {
		return time.Time{}, fmt.Errorf("value=%q unix=%d exceeds current u32 item wire", raw, unix)
	}
	return expirationDate.UTC(), nil
}

// SelectGenericStackable applies the C# MonsterDropConfig candidate rule to
// the active PVF: candidates are uniform within (grade, rarity), then fall
// back to rarity zero exactly as the reference implementation does.  It does
// not create a scene object or write an inventory row.
func (c *pvfDungeonDropCatalog) SelectGenericStackable(
	rng *currentDungeonDropLCG,
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) (uint32, bool, error) {
	if rng == nil {
		return 0, false, errDungeonDropSourceRequired
	}
	if err := c.ensureGenericPools(); err != nil {
		return 0, false, err
	}
	candidates := c.genericStackableCandidates(monsterLevel, gradeDown, gradeUp, rarity)
	if len(candidates) == 0 && rarity > 0 {
		candidates = c.genericStackableCandidates(monsterLevel, gradeDown, gradeUp, 0)
	}
	if len(candidates) == 0 {
		return 0, false, nil
	}
	return candidates[rng.Next(len(candidates))], true, nil
}

// SelectGenericEquipment is the weighted counterpart of
// SelectGenericStackable.  The pool's weight is the equipment document's own
// [creation rate]; it is a domain weight only and is never serialized into the
// current EXE item wire.
//
// When the requested rarity has no candidate at all the call degrades to
// rarity 0 — the reference implementation does the same (allowFallback). The
// degradation is recorded so a caller can surface it; before this it was
// silent, which is exactly how a whole rarity could stop dropping without
// anyone noticing.
func (c *pvfDungeonDropCatalog) SelectGenericEquipment(
	rng *currentDungeonDropLCG,
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) (uint32, bool, error) {
	if rng == nil {
		return 0, false, errDungeonDropSourceRequired
	}
	if err := c.ensureGenericPools(); err != nil {
		return 0, false, err
	}
	candidates := c.genericEquipmentCandidates(monsterLevel, gradeDown, gradeUp, rarity)
	if len(candidates) == 0 && rarity > 0 {
		c.recordGenericEquipmentFallback(monsterLevel, rarity)
		candidates = c.genericEquipmentCandidates(monsterLevel, gradeDown, gradeUp, 0)
	}
	if len(candidates) == 0 {
		return 0, false, nil
	}
	var total uint64
	for _, candidate := range candidates {
		if candidate.ItemID == 0 || candidate.Weight == 0 || math.MaxUint64-total < uint64(candidate.Weight) {
			continue
		}
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		return 0, false, nil
	}
	want := uint64(rng.NextUint32()) % total
	var cumulative uint64
	for _, candidate := range candidates {
		if candidate.ItemID == 0 || candidate.Weight == 0 || math.MaxUint64-cumulative < uint64(candidate.Weight) {
			continue
		}
		cumulative += uint64(candidate.Weight)
		if want < cumulative {
			return candidate.ItemID, true, nil
		}
	}
	return 0, false, nil
}

func (c *pvfDungeonDropCatalog) genericStackableCandidates(
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collectGenericStackablesLocked(monsterLevel, gradeDown, gradeUp, rarity)
}

func (c *pvfDungeonDropCatalog) genericEquipmentCandidates(
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) []dungeonDropWeightedItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collectGenericEquipmentLocked(monsterLevel, gradeDown, gradeUp, rarity)
}

func (c *pvfDungeonDropCatalog) collectGenericStackablesLocked(
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) []uint32 {
	if c == nil || gradeDown < 0 || gradeUp <= 0 {
		return nil
	}
	result := make([]uint32, 0)
	for offset := -gradeDown; offset < gradeUp; offset++ {
		grade := int64(monsterLevel) + offset
		if grade < 1 || grade > 200 {
			continue
		}
		result = append(result, c.genericStackables[dungeonDropGradeRarityKey{Grade: grade, Rarity: rarity}]...)
	}
	return result
}

func (c *pvfDungeonDropCatalog) collectGenericEquipmentLocked(
	monsterLevel int,
	gradeDown int64,
	gradeUp int64,
	rarity int64,
) []dungeonDropWeightedItem {
	if c == nil || gradeDown < 0 || gradeUp < 0 {
		return nil
	}
	result := make([]dungeonDropWeightedItem, 0)
	for offset := -gradeDown; offset <= gradeUp; offset++ {
		grade := int64(monsterLevel) + offset
		if grade < 1 || grade > 200 {
			continue
		}
		result = append(result, c.genericEquipment[dungeonDropGradeRarityKey{Grade: grade, Rarity: rarity}]...)
	}
	return result
}

func (c *pvfDungeonDropCatalog) ensureGenericPools() error {
	if c == nil || c.source == nil {
		return errDungeonDropSourceRequired
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.genericLoaded {
		return c.genericErr
	}
	c.genericLoaded = true
	c.genericStackables = make(map[dungeonDropGradeRarityKey][]uint32)
	c.genericEquipment = make(map[dungeonDropGradeRarityKey][]dungeonDropWeightedItem)
	if err := c.loadGenericStackablesLocked(); err != nil {
		c.genericErr = err
		return err
	}
	if err := c.loadGenericEquipmentLocked(); err != nil {
		c.genericErr = err
		return err
	}
	return nil
}

func (c *pvfDungeonDropCatalog) loadGenericStackablesLocked() error {
	for _, itemID := range c.stackableIDs {
		reference, found := c.itemRefs[itemID]
		if !found || reference.kind != dungeonDropItemStackable || dungeonDropExcludedStackablePath(reference.path) {
			continue
		}
		docPath, text, err := readDungeonDropText(c.source, string(dungeonDropItemStackable), reference.path)
		if err != nil {
			// The C# loader skips one unreadable stackable rather than replacing the
			// whole table with a synthetic fallback.
			continue
		}
		document, err := dnfpvf.Parse(docPath, text)
		if err != nil {
			continue
		}
		creationRate, found := document.Int("creation rate")
		if !found || creationRate <= 0 {
			continue
		}
		grade, found := document.Int("grade")
		if !found || grade <= 0 || grade > 200 {
			continue
		}
		rarity, found := document.Int("rarity")
		if !found {
			rarity = 0
		}
		if rarity < 0 || rarity > 6 {
			continue
		}
		key := dungeonDropGradeRarityKey{Grade: grade, Rarity: rarity}
		c.genericStackables[key] = append(c.genericStackables[key], itemID)
	}
	return nil
}

// dungeonDropGenericPoolSkip 说明一件 .equ 为什么没进通用装备池。审计按它分类，
// 免得"池子里没有某稀有度"变成只能靠猜的事。
type dungeonDropGenericPoolSkip int

const (
	dungeonDropGenericPoolAccepted dungeonDropGenericPoolSkip = iota
	dungeonDropGenericPoolNotEquipment
	dungeonDropGenericPoolUnreadable
	dungeonDropGenericPoolRarityMissing
	dungeonDropGenericPoolCreationRateAbsent
	dungeonDropGenericPoolLevelOutOfRange
	dungeonDropGenericPoolEquipmentTypeExcluded
)

// dungeonDropGenericEquipmentCandidate 是一次 .equ 扫描的结果。
type dungeonDropGenericEquipmentCandidate struct {
	ItemID uint32
	Key    dungeonDropGradeRarityKey
	Weight uint32
	Skip   dungeonDropGenericPoolSkip
}

// loadGenericEquipmentLocked 直接用 equipment/equipment.lst 的每个 .equ 构建通用装备
// 池：键是 PVF 自己的 ([rarity], [minimum level])，权重是 [creation rate]。这三项
// 都是掉落语义的权威值，深渊的史诗分支（dungeon_hell_party_drop.go）一直这么建池。
//
// 这里曾经读 Etc/ItemDictionary*.etc 的 (column1, column2)。实测 column2 与
// [minimum level] 100% 相等，键没错；但 column1 **不是** [rarity]，而是同一稀有度
// 的**多套别名编码**：regional 表用 9 表示稀有、5/10 表示神器、7/8 表示史诗/传说，
// base 表用 3 表示稀有、6/7 表示神器/史诗。代码只接受 0..6，于是
//   - 高等级稀有（别名 9）和神器（别名 10）一条候选都进不了池；
//   - 请求 rarity 3 时命中的反而是 base 表别名 3 的**稀有**装备。
//
// 候选为空时 SelectGenericEquipment 会降级成 rarity 0，所以 Lv90 深渊/时空裂缝/
// 魔界裂缝变成"一局三件白装、一件紫装都没有"。而 [basis of rarity dicision] 里
// 白装的权重是 0 —— 白装本来不可能被摇出来，它出现本身就是降级的指纹。
func (c *pvfDungeonDropCatalog) loadGenericEquipmentLocked() error {
	if c == nil || c.source == nil {
		return errDungeonDropSourceRequired
	}
	ids := c.equipmentIDs
	audit := dungeonDropGenericPoolAudit{Scanned: len(ids)}
	results := make([]dungeonDropGenericEquipmentCandidate, len(ids))
	workers := runtime.GOMAXPROCS(0)
	if workers > dungeonDropEquipmentPoolMaxWorkers {
		workers = dungeonDropEquipmentPoolMaxWorkers
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	if workers > 0 {
		jobs := make(chan int)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range jobs {
					results[index] = c.scanGenericEquipmentCandidate(ids[index])
				}
			}()
		}
		for index := range ids {
			jobs <- index
		}
		close(jobs)
		wait.Wait()
	}
	// 合并必须按 equipment.lst 的原始顺序：SelectGenericEquipment 对候选做前缀和取
	// 件，所以并发完成顺序一旦泄漏进切片顺序，"同一个 roll 抽到哪一件"就不再可复现。
	for _, result := range results {
		switch result.Skip {
		case dungeonDropGenericPoolAccepted:
			c.genericEquipment[result.Key] = append(c.genericEquipment[result.Key], dungeonDropWeightedItem{
				ItemID: result.ItemID,
				Weight: result.Weight,
			})
		case dungeonDropGenericPoolNotEquipment:
			audit.EquipmentMissing++
		case dungeonDropGenericPoolUnreadable:
			audit.ReadFailed++
		case dungeonDropGenericPoolRarityMissing:
			audit.RarityMissing++
		case dungeonDropGenericPoolCreationRateAbsent:
			audit.CreationRateAbsent++
		case dungeonDropGenericPoolLevelOutOfRange:
			audit.LevelOutOfRange++
		case dungeonDropGenericPoolEquipmentTypeExcluded:
			audit.ExcludedEquipmentType++
		}
	}
	audit.Buckets = len(c.genericEquipment)
	for key, candidates := range c.genericEquipment {
		if key.Rarity < 0 || key.Rarity >= int64(len(audit.ByRarity)) {
			continue
		}
		audit.Items += len(candidates)
		audit.ByRarity[key.Rarity] += len(candidates)
		if audit.LowestLevel[key.Rarity] == 0 || key.Grade < int64(audit.LowestLevel[key.Rarity]) {
			audit.LowestLevel[key.Rarity] = int(key.Grade)
		}
		if key.Grade > int64(audit.HighestLevel[key.Rarity]) {
			audit.HighestLevel[key.Rarity] = int(key.Grade)
		}
	}
	c.genericAudit = audit
	return nil
}

// scanGenericEquipmentCandidate 读一件 .equ 并判定它是否属于通用掉落池。
// 不读 itemCache、也不碰 c.mu：调用方持有 c.mu，走 ResolveItem 会自锁。
func (c *pvfDungeonDropCatalog) scanGenericEquipmentCandidate(itemID uint32) dungeonDropGenericEquipmentCandidate {
	reference, found := c.itemRefs[itemID]
	if !found || reference.kind != dungeonDropItemEquipment {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolNotEquipment}
	}
	docPath, text, err := readDungeonDropText(c.source, string(dungeonDropItemEquipment), reference.path)
	if err != nil {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolUnreadable}
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolUnreadable}
	}
	rarity, found := document.Int("rarity")
	if !found || rarity < 0 || rarity > 6 {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolRarityMissing}
	}
	creationRate, found := document.Int("creation rate")
	if !found || creationRate <= 0 || creationRate > math.MaxUint32 {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolCreationRateAbsent}
	}
	level, found := document.Int("minimum level")
	if !found || level <= 0 || level > 200 {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolLevelOutOfRange}
	}
	if equipmentType, found := document.Text("equipment type"); found && dungeonDropEquipmentPoolExcludedType(equipmentType) {
		return dungeonDropGenericEquipmentCandidate{ItemID: itemID, Skip: dungeonDropGenericPoolEquipmentTypeExcluded}
	}
	return dungeonDropGenericEquipmentCandidate{
		ItemID: itemID,
		Key:    dungeonDropGradeRarityKey{Grade: level, Rarity: rarity},
		Weight: uint32(creationRate),
	}
}

// dungeonDropEquipmentPoolExcludedType 挡掉不属于副本怪物掉落的装备类型。参考实现
// 把时装单独分池、不让普通掉落池碰到它们；这里沿用同一判断，并顺带挡住宠物、
// 称号、光环、勋章/守护珠这类由各自系统发放的类型。
func dungeonDropEquipmentPoolExcludedType(equipmentType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(equipmentType))
	if normalized == "" {
		return false
	}
	for _, excluded := range []string{"avatar", "[creature]", "[title name]", "[aura", "[flag"} {
		if strings.Contains(normalized, excluded) {
			return true
		}
	}
	return false
}

// recordGenericEquipmentFallback 记一次"请求的稀有度没有候选、降级成 rarity 0"。
func (c *pvfDungeonDropCatalog) recordGenericEquipmentFallback(monsterLevel int, rarity int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.genericFallbacks == nil {
		c.genericFallbacks = make(map[dungeonDropGradeRarityKey]uint64)
	}
	c.genericFallbacks[dungeonDropGradeRarityKey{Grade: int64(monsterLevel), Rarity: rarity}]++
}

// GenericFallbackMarks 返回"到目前为止发生过的降级"快照。调用方取 before/after 差分
// 就能在日志里点名是哪个等级、哪个稀有度的池子是空的。
func (c *pvfDungeonDropCatalog) GenericFallbackMarks() map[dungeonDropGradeRarityKey]uint64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	marks := make(map[dungeonDropGradeRarityKey]uint64, len(c.genericFallbacks))
	for key, count := range c.genericFallbacks {
		marks[key] = count
	}
	return marks
}

// GenericPoolAudit 返回建池时的构成快照。
func (c *pvfDungeonDropCatalog) GenericPoolAudit() dungeonDropGenericPoolAudit {
	if c == nil {
		return dungeonDropGenericPoolAudit{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	audit := c.genericAudit
	audit.Fallbacks = 0
	for _, count := range c.genericFallbacks {
		audit.Fallbacks += count
	}
	return audit
}

// WarmGenericPools 在建池之外什么都不做，供服务的 PVF 预载阶段调用：把一次约
// 秒级的全量 .equ 扫描放在监听端口打开之前，而不是放在第一只怪死亡的时候。
func (c *pvfDungeonDropCatalog) WarmGenericPools() error {
	return c.ensureGenericPools()
}

// preloadDungeonDropCatalog 在监听端口打开前把通用掉落池建好，并把池子构成写进
// 日志。"某个稀有度在某个等级段一件候选都没有"必须一眼可见——这次的洞就是靠静默
// 降级藏住的。
func (s *Service) preloadDungeonDropCatalog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	catalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return fmt.Errorf("preload dungeon drop catalog: %w", err)
	}
	drops, err := catalog.DropCatalog()
	if err != nil {
		return fmt.Errorf("preload dungeon drop catalog: %w", err)
	}
	if err := drops.WarmGenericPools(); err != nil {
		return fmt.Errorf("preload dungeon drop catalog: %w", err)
	}
	audit := drops.GenericPoolAudit()
	s.logPacketEvent("dnf-dungeon-drop-equipment-pool-loaded",
		"scanned", audit.Scanned,
		"buckets", audit.Buckets,
		"items", audit.Items,
		"by_rarity", audit.ByRarity,
		"lowest_level", audit.LowestLevel,
		"highest_level", audit.HighestLevel,
		"rarity_missing", audit.RarityMissing,
		"creation_rate_absent", audit.CreationRateAbsent,
		"level_out_of_range", audit.LevelOutOfRange,
		"excluded_equipment_type", audit.ExcludedEquipmentType,
		"equipment_missing", audit.EquipmentMissing,
		"read_failed", audit.ReadFailed,
		"elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

// reportCurrentDungeonGenericDropFallbacks 在掉落规划结束后对比降级标记，把**新出现**
// 的空池点名写进日志。降级本身保留（参考实现也 allowFallback），但不再无声。
func (s *Service) reportCurrentDungeonGenericDropFallbacks(
	catalog *pvfDungeonDropCatalog,
	before map[dungeonDropGradeRarityKey]uint64,
	event string,
	fields ...any,
) {
	if s == nil || catalog == nil {
		return
	}
	after := catalog.GenericFallbackMarks()
	report := make([]any, 0, len(fields)+8)
	reported := false
	for key, count := range after {
		if before[key] == count {
			continue
		}
		if !reported {
			report = append(report, fields...)
			reported = true
		}
		report = append(report,
			"empty_pool_level", key.Grade,
			"empty_pool_rarity", key.Rarity,
			"fallback_rolls", count)
	}
	if !reported {
		return
	}
	s.logGameEvent(nil, event, report...)
}

func dungeonDropExcludedStackablePath(value string) bool {
	value = strings.ToLower(cleanDungeonDropPath(value))
	for _, prefix := range []string{
		"cash/", "quest/", "recipe/", "temp/", "event/", "emblem/", "monstercard/",
	} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// 徽章页（[avatar emblem]）的格子：289..351，**9 行 × 7 格 = 63**。
//
// 这里过去写的是 289..344（8 行 56 格），少了最后一行 345..351。服主现场报的正是这一条：
// 「徽章栏最后一行不给用，一行 7 格、总共 9 行」。取证（2026-10-03）：
//   - 该 PVF 里 [avatar emblem] 的 stk 文档本身**不声明页范围**，页几何只在服务端常量里，
//     所以这个数字就是唯一权威，写错多少就少放多少；
//   - 345..353 这一段全库从未被任何系统写入过一行（逐角色普查），也不属于账号共享固定仓
//     （晶体 354..359 / 灵魂 360..365 从 354 起），即整段是空的、可用的；
//   - 后果不是"少几格"这么轻：char=1 的 56 格被 56 种徽章占满后，新徽章只能进溢出邮件，
//     而邮件领取又只能往同一页放 → 邮件永久卡住（mail 110 的 灿烂的绿色徽章[物理暴击]
//     就是被这条卡死的）。
//
// 与 repository 侧的口径一致：345..353 那一段在 auction.allocateSlot 的候选范围里本就
// 被当作普通主背包格。
const (
	currentAvatarEmblemPageSlotStart int16 = 289
	currentAvatarEmblemPageSlotEnd   int16 = 351
)

// dungeonDropStackableSlotRange follows the repository's established main
// inventory category ranges and uses the real PVF [stackable type] value.
// It is a domain placement rule, not a claim about the current pickup wire.
func dungeonDropStackableSlotRange(stackableType string) (int16, int16) {
	normalized := normalizeDungeonDropStackableType(stackableType)
	switch {
	case strings.HasPrefix(normalized, "[material]") && strings.HasSuffix(normalized, "4"):
		return 345, 359
	case strings.HasPrefix(normalized, "[material]"):
		return 121, 176
	case strings.HasPrefix(normalized, "[quest]"):
		return 177, 232
	case strings.HasPrefix(normalized, "[material expert job]"):
		return 233, 288
	case strings.HasPrefix(normalized, "[avatar emblem]"):
		return currentAvatarEmblemPageSlotStart, currentAvatarEmblemPageSlotEnd
	default:
		return 65, 120
	}
}

func dungeonDropStackablePrefersItemQuickSlots(stackableType string) bool {
	normalized := normalizeDungeonDropStackableType(stackableType)
	return strings.HasPrefix(normalized, "[waste]")
}

func normalizeDungeonDropStackableType(stackableType string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stackableType, "`", "")))
}

// DropCatalog lazily derives the item/drop catalog from the same authoritative
// PVF source as the monster catalog. Synthetic tests that do not provide item
// lists therefore keep the existing zero-drop death path until a drop is
// actually requested.
func (c *pvfDungeonMonsterCatalog) DropCatalog() (*pvfDungeonDropCatalog, error) {
	if c == nil || c.source == nil {
		return nil, errDungeonDropSourceRequired
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dropCatalogLoaded {
		c.dropCatalog, c.dropCatalogErr = newPVFDungeonDropCatalog(c.source)
		c.dropCatalogLoaded = true
	}
	return c.dropCatalog, c.dropCatalogErr
}

func parseDungeonMonsterDropPool(document *dnfpvf.Document) []dungeonMonsterDropPoolEntry {
	tokens, ok := document.Section("item")
	if !ok || len(tokens) == 0 {
		return nil
	}
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	out := make([]dungeonMonsterDropPoolEntry, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		itemID, weight := values[index], values[index+1]
		if itemID <= 0 || itemID > math.MaxUint32 || weight <= 0 || weight > math.MaxUint32 {
			continue
		}
		out = append(out, dungeonMonsterDropPoolEntry{ItemID: uint32(itemID), Weight: uint32(weight)})
	}
	return out
}

func selectDungeonMonsterDrop(pool []dungeonMonsterDropPoolEntry, roll uint64) (dungeonMonsterDropPoolEntry, bool) {
	var total uint64
	for _, entry := range pool {
		if entry.ItemID == 0 || entry.Weight == 0 || math.MaxUint64-total < uint64(entry.Weight) {
			continue
		}
		total += uint64(entry.Weight)
	}
	if total == 0 {
		return dungeonMonsterDropPoolEntry{}, false
	}
	want := roll % total
	var cumulative uint64
	for _, entry := range pool {
		if entry.ItemID == 0 || entry.Weight == 0 || math.MaxUint64-cumulative < uint64(entry.Weight) {
			continue
		}
		cumulative += uint64(entry.Weight)
		if want < cumulative {
			return entry, true
		}
	}
	return dungeonMonsterDropPoolEntry{}, false
}

func readDungeonDropText(source dnfpvf.Source, root, listedPath string) (string, string, error) {
	clean := cleanDungeonDropPath(listedPath)
	candidates := []string{clean}
	root = cleanDungeonDropPath(root)
	if root != "" && !strings.HasPrefix(strings.ToLower(clean), strings.ToLower(root)+"/") {
		candidates = append(candidates, path.Join(root, clean))
	}
	// The 90 PVF marks revised files with a "(r)" basename prefix while list
	// indexes keep naming the original file; fall back to the revised name
	// only when the original is absent.
	for _, candidate := range append([]string(nil), candidates...) {
		dir, base := path.Split(candidate)
		if base == "" || strings.HasPrefix(strings.ToLower(base), "(r)") {
			continue
		}
		candidates = append(candidates, dir+"(r)"+base)
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := source.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func cleanDungeonDropPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

func cloneDungeonMonsterDropPool(values []dungeonMonsterDropPoolEntry) []dungeonMonsterDropPoolEntry {
	return append([]dungeonMonsterDropPoolEntry(nil), values...)
}
