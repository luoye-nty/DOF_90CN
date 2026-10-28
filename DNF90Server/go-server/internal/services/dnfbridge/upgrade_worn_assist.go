package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 「已装备道具」对强化/增幅的助力：费用折扣与成功率加成。
//
// 现场 2026-10-03（服主报，附强化确认框截图）：
//
//	未佩戴赫尔德 273960 金币；佩戴赫尔德 263002 金币
//	273960 × (100-4) / 100 = 263001.6 → 263002
//
// 也就是说**客户端自己按 PVF 算好了折扣并显示**，而服务端按原价扣，玩家白掏那 4%。
//
// 两个字段都来自 PVF，**不需要硬编码**（服主一开始以为要硬编码，因为字段名不直观）：
//
//	[upgrade cost discount]           4     ← 强化费用降低 4%
//	[increase upgrade compound rate]  400   ← 强化成功率 +400（单位 0.01% ⇒ +4.00%）
//
// 谁能带这两个字段（同一份 PVF 里数出来的）：`[increase upgrade compound rate]` 在 7 件上
// ——赫尔德 hilda.equ / riz.equ / riz_jin.equ / 新年赫尔德 / 3 个称号（craftsmanship.equ 等）；
// `[upgrade cost discount]` 主要在布甲腰带上（belt_cl_2choro41x / belt_cl_3choro2x…）。
// 所以这里按"**所有已装备道具求和**"实现，而不是只认宠物 —— 传承腰带那批同理生效。
//
// 客户端把折扣算在**费用展示**上；成功率那一项客户端算不了（成功与否是服务端掷的），
// 所以服务端必须自己把它加进成功权重里（PVF 单位 0.01%，400 ⇒ ×1.04）。

const (
	currentUpgradeCostDiscountField   = "upgrade cost discount"
	currentUpgradeCompoundRateField   = "increase upgrade compound rate"
	currentUpgradeCompoundRateDivisor = 10000 // PVF 的 0.01% 单位 ⇒ 百分比
)

var (
	currentUpgradeCostDiscountPattern = regexp.MustCompile(
		`(?m)^\s*\[upgrade cost discount\]\s*\n\s*(-?\d+)`)
	currentUpgradeCompoundRatePattern = regexp.MustCompile(
		`(?m)^\s*\[increase upgrade compound rate\]\s*\n\s*(-?\d+)`)
)

// currentWornUpgradeAssistResult 是聚合结果 + 排查用的证据（哪张记录读到了什么）。
type currentWornUpgradeAssistResult struct {
	DiscountPercent int64
	CompoundRate    int64
	// WornEquipment 是装备记录里的穿戴条目数（腰带/称号等）。
	WornEquipment int
	// PetEquipped / PetItemID 说明宠物记录里有没有"出战"的那只、它是哪件道具。
	PetEquipped bool
	PetItemID   int64
	// Items 是**每件已装备道具**这一次解析的完整证据链（id → 路径 → 原文 → 两个字段）。
	Items []currentWornUpgradeAssistItem
}

// currentWornUpgradeAssistItem 是一件道具这一轮解析的断点证据。
//
// 为什么要留这么细：现场 2026-10-03 第一版日志只打了**聚合值** 0/0，看到"都读到 0"
// 却分不清是"id 不在 lst 里"、"路径读不到文本"、还是"读到文本但正则没中"，
// 白跑了一轮部署。现在每件道具都把 ListedPath（lst 原文）与 ArchivePath（真正读到的
// 归档路径）分开打，两者不一致就说明走了相对路径兜底，一眼定位。
type currentWornUpgradeAssistItem struct {
	ItemID int64
	// ListedPath 是 equipment/equipment.lst 里记的那条路径。
	ListedPath string
	// ArchivePath 是**真正读到文本**的归档路径。
	ArchivePath string
	TextBytes   int
	Discount    int64
	Compound    int64
	// Err 非空表示这件道具没读成（值已压成无空格的短串，便于当成日志字段）。
	Err string
}

// petEvidence 取出"出战宠物"那一件的证据；没佩戴宠物时返回零值。
func (r currentWornUpgradeAssistResult) petEvidence() currentWornUpgradeAssistItem {
	if !r.PetEquipped || r.PetItemID <= 0 {
		return currentWornUpgradeAssistItem{}
	}
	for _, item := range r.Items {
		if item.ItemID == r.PetItemID {
			return item
		}
	}
	return currentWornUpgradeAssistItem{ItemID: r.PetItemID}
}

// describeItems 把每件道具的证据压成**一个没有空格**的日志字段，形如
//
//	400990076|creature/hilda/hilda.equ|equipment/creature/hilda/hilda.equ|743|4|400
//
// 字段依次是 id | lst 原文路径 | 实际归档路径 | 文本字节数 | 折扣 | 成功率；
// 读失败时最后一栏是原因（例：...|0|0|read-text-failed），多件之间用 ";" 隔开。
func (r currentWornUpgradeAssistResult) describeItems() string {
	if len(r.Items) == 0 {
		return "-"
	}
	var builder strings.Builder
	for index, item := range r.Items {
		if index > 0 {
			builder.WriteString(";")
		}
		fmt.Fprintf(&builder, "%d|%s|%s|%d|%d|%d|%s",
			item.ItemID, item.ListedPath, item.ArchivePath,
			item.TextBytes, item.Discount, item.Compound, item.Err)
	}
	return builder.String()
}

// currentWornUpgradeAssist 汇总角色**已装备**道具上的两项强化助力。
//
// 两个来源都要看（现场 2026-10-03 第一次只看了前者，结果宠物没被算进去）：
//
//	装备记录 dnfrepo.EquipmentRecord —— 腰带、称号这些普通穿戴槽；
//	宠物记录 dnfrepo.PetRecord       —— **穿戴中的宠物在 PetRecord.EquippedKey 上**，
//	                                    它不在装备记录里（宠物是独立的 list 7 + 独立记录）。
//
// 每件道具按 PVF 原文里的字段求和。任何一件读不到都只是**少算那一件**，不会让强化失败。
func (s *Service) currentWornUpgradeAssist(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
) currentWornUpgradeAssistResult {
	var result currentWornUpgradeAssistResult
	if s == nil || strings.TrimSpace(characterID) == "" {
		return result
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return result
	}
	itemIDs := make([]int64, 0, 16)
	seen := make(map[int64]struct{}, 16)
	appendItem := func(itemID int64) {
		if itemID <= 0 {
			return
		}
		if _, duplicate := seen[itemID]; duplicate {
			return
		}
		seen[itemID] = struct{}{}
		itemIDs = append(itemIDs, itemID)
	}
	if repositories.Equipment != nil {
		if record, found, loadErr := repositories.Equipment.Load(ctx, characterID); loadErr == nil && found {
			for _, entry := range record.Entries {
				if entry.ItemID > 0 {
					result.WornEquipment++
				}
				appendItem(entry.ItemID)
			}
		}
	}
	if repositories.Pet != nil {
		if record, found, loadErr := repositories.Pet.Load(ctx, characterID); loadErr == nil && found {
			// **只有出战（佩戴）的那只才算。** `PetRecord.EquippedKey` 就是"穿戴栏"在服务端的
			// 口径：equip/pet_sync.go 在宠物进/出穿戴槽时写它，卸下会清空。
			//
			// 服主 2026-10-03："这个宠物只有佩戴以后才生效 不佩戴是不生效的 直接扫穿戴栏不就行了"
			// —— 所以这里**不做**"key 为空就扫全部宠物"的兜底：那会把背包里的宠物也算进去，
			// 等于没佩戴也享受折扣。key 为空 = 没佩戴 = 不加成（fail-closed）。
			if key := strings.TrimSpace(record.EquippedKey); key != "" {
				if entry, ok := record.Entries[key]; ok {
					result.PetEquipped = true
					result.PetItemID = entry.ItemID
					appendItem(entry.ItemID)
				}
			}
		}
	}
	for _, itemID := range itemIDs {
		item := s.currentPVFUpgradeAssist(catalog, itemID)
		result.DiscountPercent += item.Discount
		result.CompoundRate += item.Compound
		result.Items = append(result.Items, item)
	}
	if result.DiscountPercent > 100 {
		result.DiscountPercent = 100
	}
	if result.DiscountPercent < 0 {
		result.DiscountPercent = 0
	}
	if result.CompoundRate < 0 {
		result.CompoundRate = 0
	}
	return result
}

// currentPVFUpgradeAssist 从一件道具的 PVF 原文里读出这两个字段，并留下断点证据。
//
// 为什么直接读原文而不是加进 dungeonDropItemDefinition：这两个字段只有强化这一条路径用，
// 定义结构里已经有 40+ 个字段；直接读原文的改动面最小，也不会影响别的解析结果。
//
// 现场 2026-10-03 的断点（第二轮）就死在下面这行读文本的方式上，见注释。
func (s *Service) currentPVFUpgradeAssist(
	catalog *pvfDungeonDropCatalog,
	itemID int64,
) currentWornUpgradeAssistItem {
	item := currentWornUpgradeAssistItem{ItemID: itemID}
	if catalog == nil || catalog.source == nil || itemID <= 0 {
		item.Err = "no-pvf-source"
		return item
	}
	item.ListedPath = s.upgradeAssistPathForItem(catalog, itemID)
	if item.ListedPath == "" {
		item.Err = "item-id-not-in-equipment-lst"
		return item
	}
	// **必须走 readDungeonDropText，不能直接 catalog.source.ReadText(ListedPath)。**
	//
	// equipment.lst 里的路径是**相对 equipment/ 目录**记的 —— 而且这不只是宠物那一类：
	//
	//	equipment.lst 原文 : 400990076 `creature/hilda/hilda.equ`
	//	                    `character/priest/weapon/axe/108040415.equ`
	//	归档里的真名       : equipment/creature/hilda/hilda.equ
	//	                    equipment/character/priest/weapon/axe/108040415.equ
	//
	// 于是直接 ReadText 一定 not found、两个字段永远取到 0：实测线上 equipment.lst 里
	// 相对写法的 .equ 抽 20000 条，裸 ReadText 读到 **0** 条，走解析链读到 19370 条。
	// 这也解释了为什么测试夹具（当时手写成带 equipment/ 前缀）全绿、线上却无效。
	// readDungeonDropText 是 ResolveItem 一直在用的同一条解析链（raw → root 拼接 → (r) 变体），
	// 这里复用它而不是自己拼路径，避免两套口径漂移。
	archivePath, text, err := readDungeonDropText(
		catalog.source, string(dungeonDropItemEquipment), item.ListedPath)
	if err != nil || text == "" {
		item.Err = "read-text-failed"
		if err != nil {
			item.Err = currentUpgradeAssistErrText(err)
		}
		return item
	}
	item.ArchivePath = archivePath
	item.TextBytes = len(text)
	item.Discount, item.Compound = currentUpgradeAssistFromText(text)
	return item
}

// currentUpgradeAssistErrText 把错误压成无空格的短串，保证它当一个日志字段用。
func currentUpgradeAssistErrText(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), "_")
	if len(message) > 120 {
		message = message[:120]
	}
	return message
}

// upgradeAssistPathForItem 解析"道具 id → equipment.lst 里记的那条路径"（**原文，未做归档解析**）。
//
// 先查目录自带的索引，查不到再**直接从 equipment/equipment.lst 现查一次**（结果缓存）。
//
// 2026-10-03 那次排查一度以为"目录索引里没有这条 id"就是根因，于是加了这条兜底；
// 结果线上字段依旧是 0 —— 因为**真正的断点在读到路径之后的 ReadText**（见
// currentPVFUpgradeAssist 那段注释：lst 记的是相对 equipment/ 的路径）。
// 兜底本身留着无害（目录索引确实按 kind 过滤，宠物这种 .equ 不一定在里面），
// 但**它当时不是根因**，别再被这行注释带偏。
func (s *Service) upgradeAssistPathForItem(catalog *pvfDungeonDropCatalog, itemID int64) string {
	if catalog == nil || itemID <= 0 || itemID > math.MaxUint32 {
		return ""
	}
	if reference, found := catalog.itemRefs[uint32(itemID)]; found {
		if path := strings.TrimSpace(reference.path); path != "" {
			return path
		}
	}
	index, err := s.currentEquipmentPathIndex(catalog)
	if err != nil {
		return ""
	}
	return index[uint32(itemID)]
}

// currentEquipmentPathIndex 惰性解析 equipment/equipment.lst 的 "id → 路径" 全表。
//
// equipment.lst 是一座大表，全库上万条；这里只在"目录索引没查到"时才解析一次并缓存，
// 之后每次强化都只是查 map。
func (s *Service) currentEquipmentPathIndex(catalog *pvfDungeonDropCatalog) (map[uint32]string, error) {
	if s == nil || catalog == nil || catalog.source == nil {
		return nil, errDungeonDropSourceRequired
	}
	catalog.upgradeAssistPathMu.Lock()
	defer catalog.upgradeAssistPathMu.Unlock()
	if catalog.upgradeAssistPathIndex != nil {
		return catalog.upgradeAssistPathIndex, nil
	}
	if catalog.upgradeAssistPathErr != nil {
		return nil, catalog.upgradeAssistPathErr
	}
	text, err := catalog.source.ReadText(dungeonDropEquipmentList)
	if err != nil {
		catalog.upgradeAssistPathErr = err
		return nil, err
	}
	index := make(map[uint32]string, 8192)
	for _, match := range currentEquipmentListEntryPattern.FindAllStringSubmatch(text, -1) {
		id, parseErr := strconv.ParseUint(strings.TrimSpace(match[1]), 10, 32)
		if parseErr != nil {
			continue
		}
		path := strings.TrimSpace(match[2])
		if path == "" {
			continue
		}
		index[uint32(id)] = path
	}
	if len(index) == 0 {
		err := fmt.Errorf("equipment list %s parsed to an empty index", dungeonDropEquipmentList)
		catalog.upgradeAssistPathErr = err
		return nil, err
	}
	catalog.upgradeAssistPathIndex = index
	return index, nil
}

func currentUpgradeAssistFromText(text string) (discountPercent int64, compoundRate int64) {
	if match := currentUpgradeCostDiscountPattern.FindStringSubmatch(text); match != nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(match[1]), 10, 64); err == nil {
			discountPercent = value
		}
	}
	if match := currentUpgradeCompoundRatePattern.FindStringSubmatch(text); match != nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(match[1]), 10, 64); err == nil {
			compoundRate = value
		}
	}
	return discountPercent, compoundRate
}

// withCurrentWornUpgradeAssist 把上面两项叠加到 PVF 策略上。
//
//   - 费用：`cost - cost × 折扣 / 100`（与客户端展示同口径：273960 ⇒ 263002）；
//   - 成功率：`weight × (10000 + compoundRate) / 10000`（PVF 单位 0.01%），上限封在必成。
//
// 策略本身（`SuccessWeight` / `GoldCost`）来自 PVF，这里只做"装备加成"，不动别的字段。
func (s *Service) withCurrentWornUpgradeAssist(
	base func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error),
	session *gameSession,
	repositories dnfrepo.Group,
) func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
	if base == nil {
		return nil
	}
	return func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
		policy, err := base(mode, currentLevel, targetItemID, amplifyType)
		if err != nil {
			return policy, err
		}
		if s == nil || session == nil || session.selectedCharacterID == 0 {
			return policy, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		assist := s.currentWornUpgradeAssist(ctx,
			repositories, strconv.FormatUint(uint64(session.selectedCharacterID), 10))
		discount, compoundRate := assist.DiscountPercent, assist.CompoundRate
		// 无论有没有加成都要留痕：第一次上线时因为"没命中就不打日志"，排查时看不到
		// 任何线索（服主 2026-10-03 报"费用还是不对"那一轮）。日志里带上聚合结果与
		// 生效后的策略值，下一次直接从日志就能判断是哪一环没取到。
		//
		// 第二轮又补了**每件道具**的证据（pet_listed_path / pet_archive_path /
		// pet_text_bytes / pet_discount_raw / pet_compound_raw / worn_assist_items）：
		// 上一版只有聚合值 0/0，看到"读到了但字段为 0"仍旧分不清是"id 不在 lst"、
		// "路径读不到文本"还是"正则没中"。现在这几个字段各自对应一截断点。
		pet := assist.petEvidence()
		s.logPacketEvent("dnf-upgrade-worn-assist",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"mode", mode,
			"target_item_id", targetItemID,
			"cost_discount_percent", discount,
			"compound_rate_permille", compoundRate,
			"worn_equipment_entries", assist.WornEquipment,
			"pet_equipped", assist.PetEquipped,
			"pet_item_id", assist.PetItemID,
			"pet_listed_path", pet.ListedPath,
			"pet_archive_path", pet.ArchivePath,
			"pet_text_bytes", pet.TextBytes,
			"pet_discount_raw", pet.Discount,
			"pet_compound_raw", pet.Compound,
			"pet_read_error", pet.Err,
			"worn_assist_items", assist.describeItems(),
			"gold_cost_before", policy.GoldCost,
			"success_weight_before", policy.SuccessWeight)
		if discount > 0 && policy.GoldCost > 0 {
			policy.GoldCost -= policy.GoldCost * discount / 100
		}
		if compoundRate > 0 && policy.SuccessWeight > 0 {
			scaled := int64(policy.SuccessWeight) * (int64(currentUpgradeCompoundRateDivisor) + compoundRate) /
				int64(currentUpgradeCompoundRateDivisor)
			if scaled > int64(currentUpgradeSuccessBase) {
				scaled = int64(currentUpgradeSuccessBase)
			}
			policy.SuccessWeight = int(scaled)
		}
		return policy, nil
	}
}

// equipment.lst 里的 "id `path`" 行。（索引本身缓存在目录对象上，见
// pvfDungeonDropCatalog.upgradeAssistPathIndex —— 不放在包级，避免测试之间互相污染。）
var currentEquipmentListEntryPattern = regexp.MustCompile("(\\d+)\\s+`([^`]+)`")
