package dnfbridge

import (
	"fmt"
	"math"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// 「城镇增益药剂」（史诗Buff药剂一族）的 buff 数据表。
//
// 2026-10-03 现场（服主报，测试x = char 11）：史诗Buff药剂 490002458 修好"能用"之后
// 仍然看不到 buff 图标。翻 PVF 找到两张表，这一族的效果、图标、概率**全在这里**，
// 不在 .stk 自己的文档里：
//
//	etc/variousbuffstackablelist.etc   道具 → buff + 概率
//	    [index] 490002458
//	    [probability buff list] 1136 100          ← buff 1136，权重 100
//	    [/index]
//	    [index] 10008219
//	    [probability buff list] 1090 60 1091 30 1092 10   ← 三档随机
//	    [/index]
//
//	etc/variousbufflist.etc           buff → 图标 / 说明 / 效果 / 数值
//	    [index] 1136
//	    [system group] `random_box_buff`
//	    [icon index] 183                          ← 客户端画图标用的就是这个
//	    [tooltip text] `史诗道具掉落率提升`
//	    [effect] [name] `epic_rarity_item_drop_rate` [param] 100 [/param] [/effect]
//	    [overlap] 1
//	    [apply type] [type] `expire_time` [param] 10 0 [/param] [/apply type]
//	    [/index]
//
// 于是三件事都有权威依据了：
//  1. **数值**：`epic_rarity_item_drop_rate` 的 param 就是"增加百分之多少"。
//     1136=100（2 倍）、1090/1091/1092=100/150/200（赛丽亚三档）、1239=10、1240=5。
//     以前按 `[explain]` 文字统一取 200 只对史诗Buff药剂是对的，小灯笼大瓶/小瓶其实是
//     +10%/+5%，所以现在改成读这张表；表里查不到的道具（例如 490000413 这种老活动券）
//     才回退到说明文字。
//  2. **概率**：`[probability buff list]` 是"按权重随机给一个 buff"，所以**服务端**掷点，
//     客户端不可能自己算 —— 这就是"必须下发状态"的原因。
//  3. **图标**：`[icon index]` + `[tooltip text]` 由客户端按 buff id 去这张表里取，
//     服务端只要把"你拿到了哪个 buff、还剩多久"告诉它即可（见
//     stat_change_potion_projection.go 的 class0/op174）。
const (
	currentVariousBuffStackableList = "etc/variousbuffstackablelist.etc"
	currentVariousBuffList          = "etc/variousbufflist.etc"
	// currentEpicRarityDropRateEffect 是掉落率效果的 name；param 是"增加百分比"。
	currentEpicRarityDropRateEffect = "epic_rarity_item_drop_rate"
	// currentUpgradeSuccessRateEffect 是**强化/增幅成功率**效果的 name；param 是
	// "成功率增加多少个百分点"。现场抄录（2026-10-03，cmd/scratch/pvfmember 可复核）：
	//
	//	1097 param 10  `强化成功率提升 (下级)`
	//	1098 param 20  `强化成功率提升 (中级)`
	//	1099 param 30  `强化成功率提升 (上级)`
	//	1180..1187 param 1..8（活动 SpecUp_Event_Buff4）
	//
	// 道具→buff 的掷点在同一份 etc/variousbuffstackablelist.etc 里：
	// 赛丽亚的幸运强化药剂 10008220 → `1097 60 1098 30 1099 10`。
	currentUpgradeSuccessRateEffect = "item_usual_upgrade_success_rate"
	// currentDungeonClearExpRateEffect 是"地下城通关经验值 +N%"（赛丽亚的幸运经验药剂
	// 10008222 → buff 1103/1104/1105 概率 30/50/20，param 50/100/200）。
	// 通关经验是服务端发的，所以这一族必须服务端算。
	currentDungeonClearExpRateEffect = "dungeon_clear_exp_rate"
)

// currentVariousBuffRoll 是"道具 → buff"里的一项（buff id + 权重）。
type currentVariousBuffRoll struct {
	BuffID int64
	Weight int64
	// BonusPercent 只有 clientBuffRollsForItem 会填（厂商那几族把 param 一起带出来，
	// 客户端自己施加效果时用不到它，服务端只用于日志/排查）。解析器不填，保持 0。
	BonusPercent int64
}

// currentVariousBuffEffect 是 buff 定义表里我们关心的字段。
type currentVariousBuffEffect struct {
	BuffID      int64
	SystemGroup string
	IconIndex   int64
	Tooltip     string
	// EffectName / EffectParams 来自 [effect] 段。掉率增益用 params[0] 表示 +N%。
	EffectName   string
	EffectParams []int64
	// ApplyType 来自 [apply type] 的 [type]（`expire_time` / `dungeon_one_match` ...）。
	ApplyType string
}

// currentVariousBuffTables 是两张表解析后的结果。
type currentVariousBuffTables struct {
	// ByItem 道具 id → 候选 buff（按权重）。
	ByItem map[uint32][]currentVariousBuffRoll
	// ByBuff buff id → 定义。
	ByBuff map[int64]currentVariousBuffEffect
}

// loadCurrentVariousBuffTables 读两张表。任一读不到就返回错误，由调用方决定是降级
// （回退到 `[explain]` 文字）还是让整条路径失败 —— 这里不吞错。
func loadCurrentVariousBuffTables(source dnfpvf.Source) (currentVariousBuffTables, error) {
	tables := currentVariousBuffTables{
		ByItem: make(map[uint32][]currentVariousBuffRoll),
		ByBuff: make(map[int64]currentVariousBuffEffect),
	}
	stackableText, err := source.ReadText(currentVariousBuffStackableList)
	if err != nil {
		return tables, fmt.Errorf("read %s: %w", currentVariousBuffStackableList, err)
	}
	stackableDoc, err := dnfpvf.Parse(currentVariousBuffStackableList, stackableText)
	if err != nil {
		return tables, fmt.Errorf("parse %s: %w", currentVariousBuffStackableList, err)
	}
	buffText, err := source.ReadText(currentVariousBuffList)
	if err != nil {
		return tables, fmt.Errorf("read %s: %w", currentVariousBuffList, err)
	}
	buffDoc, err := dnfpvf.Parse(currentVariousBuffList, buffText)
	if err != nil {
		return tables, fmt.Errorf("parse %s: %w", currentVariousBuffList, err)
	}
	tables.ByItem = parseCurrentVariousBuffStackables(stackableDoc)
	tables.ByBuff = parseCurrentVariousBuffEffects(buffDoc)
	return tables, nil
}

// indexBlocks 把 token 流按 `[index] ... [/index]` 切段，返回每段的 token 窗口。
func indexBlocks(document *dnfpvf.Document) [][]dnfpvf.Token {
	if document == nil {
		return nil
	}
	tokens := document.Tokens
	blocks := make([][]dnfpvf.Token, 0, 64)
	for i := 0; i < len(tokens); i++ {
		if !isPVFSectionToken(tokens[i], "index") {
			continue
		}
		for j := i + 1; j < len(tokens); j++ {
			if isPVFSectionToken(tokens[j], "/index") {
				blocks = append(blocks, tokens[i+1:j])
				i = j
				break
			}
			if isPVFSectionToken(tokens[j], "index") {
				i = j - 1
				break
			}
			if j == len(tokens)-1 {
				blocks = append(blocks, tokens[i+1:])
				i = j
			}
		}
	}
	return blocks
}

// parseCurrentVariousBuffStackables 解 [index] <道具> ([buff list]|[probability buff list]) ...
//
// 两种段名都要认：
//   - `[buff list]`            —— 确定性，一个 buff id
//   - `[probability buff list]` —— (buff id, 权重) 成对出现，服务端按权重掷点
func parseCurrentVariousBuffStackables(document *dnfpvf.Document) map[uint32][]currentVariousBuffRoll {
	out := make(map[uint32][]currentVariousBuffRoll)
	for _, block := range indexBlocks(document) {
		itemID := int64(0)
		rolls := make([]currentVariousBuffRoll, 0, 2)
		section := ""
		ints := make([]int64, 0, 6)
		flush := func() {
			switch section {
			case "buff list":
				for _, value := range ints {
					rolls = append(rolls, currentVariousBuffRoll{BuffID: value, Weight: 100})
				}
			case "probability buff list":
				if len(ints)%2 == 0 {
					for k := 0; k+1 < len(ints); k += 2 {
						rolls = append(rolls, currentVariousBuffRoll{BuffID: ints[k], Weight: ints[k+1]})
					}
				} else {
					// 退化写法：只有 buff id，没有权重 ⇒ 每项等权。
					for _, value := range ints {
						rolls = append(rolls, currentVariousBuffRoll{BuffID: value, Weight: 100})
					}
				}
			}
			ints = ints[:0]
		}
		for _, token := range block {
			if token.Kind == dnfpvf.TokenSection {
				flush()
				section = strings.ToLower(strings.TrimSpace(token.Value))
				continue
			}
			if token.Kind != dnfpvf.TokenInt {
				continue
			}
			if itemID == 0 && section == "" {
				itemID = token.Int
				continue
			}
			if section == "buff list" || section == "probability buff list" {
				ints = append(ints, token.Int)
			}
		}
		flush()
		if itemID <= 0 || itemID > math.MaxUint32 || len(rolls) == 0 {
			continue
		}
		out[uint32(itemID)] = rolls
	}
	return out
}

// parseCurrentVariousBuffEffects 解 [index] <buff id> 下的图标/说明/效果/数值。
func parseCurrentVariousBuffEffects(document *dnfpvf.Document) map[int64]currentVariousBuffEffect {
	out := make(map[int64]currentVariousBuffEffect)
	for _, block := range indexBlocks(document) {
		effect := currentVariousBuffEffect{}
		section := ""
		expectName := false
		inEffect := false
		inParam := false
		for _, token := range block {
			if token.Kind == dnfpvf.TokenSection {
				name := strings.ToLower(strings.TrimSpace(token.Value))
				section = name
				inParam = name == "param"
				if name == "effect" {
					inEffect = true
				}
				if name == "/effect" {
					inEffect = false
				}
				if name == "name" {
					expectName = true
				}
				continue
			}
			switch {
			case effect.BuffID == 0 && token.Kind == dnfpvf.TokenInt:
				effect.BuffID = token.Int
			case section == "system group" && token.Kind == dnfpvf.TokenString:
				effect.SystemGroup = cleanCurrentPVFText(token.Value)
			case section == "icon index" && token.Kind == dnfpvf.TokenInt:
				effect.IconIndex = token.Int
			case section == "tooltip text" && token.Kind == dnfpvf.TokenString:
				effect.Tooltip = cleanCurrentPVFText(token.Value)
			case expectName && token.Kind == dnfpvf.TokenString:
				effect.EffectName = cleanCurrentPVFText(token.Value)
				expectName = false
			case inEffect && inParam && token.Kind == dnfpvf.TokenInt:
				effect.EffectParams = append(effect.EffectParams, token.Int)
			case section == "apply type" && token.Kind == dnfpvf.TokenString && effect.ApplyType == "":
				// [apply type] 段里第一个字符串就是 [type] 的值（`expire_time` 等）。
				value := cleanCurrentPVFText(token.Value)
				if !strings.EqualFold(value, "type") {
					effect.ApplyType = value
				}
			}
		}
		if effect.BuffID <= 0 {
			continue
		}
		out[effect.BuffID] = effect
	}
	return out
}

func cleanCurrentPVFText(value string) string {
	return strings.TrimSpace(strings.Trim(value, "`"))
}

// dispatchBonusPercent 把 buff 的效果翻译成"数值"：只有当效果名就是 wantEffect 时才算数，
// param 缺省或非正数按"不认"处理（= 服务端不改任何东西），绝不猜。
func (effect currentVariousBuffEffect) dispatchBonusPercent(wantEffect string) (int64, bool) {
	if !strings.EqualFold(strings.TrimSpace(effect.EffectName), wantEffect) {
		return 0, false
	}
	if len(effect.EffectParams) == 0 || effect.EffectParams[0] <= 0 {
		return 0, false
	}
	return effect.EffectParams[0], true
}

// dropRateBonusPercent 把 buff 的效果翻译成"掉落率增加百分比"。
//
// 只有 `epic_rarity_item_drop_rate` 这一种效果被认；param 缺省或非正数按"不认"处理
// （= 服务端不改掉落），绝不猜。
func (effect currentVariousBuffEffect) dropRateBonusPercent() (int64, bool) {
	return effect.dispatchBonusPercent(currentEpicRarityDropRateEffect)
}

// upgradeSuccessBonusPercent 把 buff 的效果翻译成"强化/增幅成功率增加百分点"。
//
// 只认 `item_usual_upgrade_success_rate`；param 缺省或非正数按"不认"处理。
// 强化与增幅在 PVF 里共用这一个效果名（`[item upgrad buff usual upgrade]` /
// `[item upgrad buff amplify upgrade]` 是**道具侧**的 stackable type，不是 buff 效果名），
// 所以"加的是强化还是增幅"由道具的 stackable type 决定，见
// stackable_use_pvf.go 的 currentUpgradeRatePotionStackableKind。
func (effect currentVariousBuffEffect) upgradeSuccessBonusPercent() (int64, bool) {
	return effect.dispatchBonusPercent(currentUpgradeSuccessRateEffect)
}

// dungeonClearExpBonusPercent 把 buff 的效果翻译成"地下城通关经验 +N%"。
func (effect currentVariousBuffEffect) dungeonClearExpBonusPercent() (int64, bool) {
	return effect.dispatchBonusPercent(currentDungeonClearExpRateEffect)
}

// serverSideStatChangeEffects 是"服务端必须自己算"的效果名。其余效果（城镇移速、
// 进图属性…）由客户端按同一份 buff 表自己施加，服务端只负责把掷到的 buff 注册过去。
func serverSideStatChangeEffect(effectName string) bool {
	for _, want := range []string{
		currentEpicRarityDropRateEffect,
		currentUpgradeSuccessRateEffect,
		currentDungeonClearExpRateEffect,
	} {
		if strings.EqualFold(strings.TrimSpace(effectName), want) {
			return true
		}
	}
	return false
}

// clientBuffFamilies 列出本表里"客户端自施放"的那些 family（buff effect name）。
//
// 用途：登录/重登时要逐个 family 去看角色身上有没有仍然生效的窗口（各族互不覆盖、
// 各按自己的到期时间结束），所以需要一份可枚举的清单。
func (tables currentVariousBuffTables) clientBuffFamilies() []string {
	seen := make(map[string]struct{}, 4)
	for _, rolls := range tables.ByItem {
		for _, roll := range rolls {
			effect, found := tables.ByBuff[roll.BuffID]
			if !found {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(effect.EffectName))
			if name == "" || serverSideStatChangeEffect(name) {
				continue
			}
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// statChangePotionGrantableBuffIDs 列出这一族药剂**可能发出去的**全部 buff id
// （= etc/variousbuffstackablelist.etc 里出现过的所有 buff）。
//
// 用于"窗口全过期时把残留图标一次清干净"：两族（服务端算的 + 客户端自施放的）都覆盖，
// 且只覆盖这一族真会发的 buff，不会误伤别的系统。
func (tables currentVariousBuffTables) statChangePotionGrantableBuffIDs() []int32 {
	seen := make(map[int64]struct{}, 16)
	for _, rolls := range tables.ByItem {
		for _, roll := range rolls {
			if roll.BuffID > 0 && roll.BuffID <= math.MaxInt32 {
				seen[roll.BuffID] = struct{}{}
			}
		}
	}
	out := make([]int32, 0, len(seen))
	for buffID := range seen {
		out = append(out, int32(buffID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// 服务端要算的三种效果之外，剩下的都归到 family（= buff 的 effect name）下。
//
// 一件道具只会属于一个 family（赛丽亚那五件各自一族），所以这里取第一组有效候选即可；
// 若真的出现同道具多族，只认第一个有效 family，宁可少认也不混族。
func (tables currentVariousBuffTables) clientBuffRollsForItem(itemID int64) (string, []currentVariousBuffRoll) {
	if itemID <= 0 || itemID > math.MaxUint32 {
		return "", nil
	}
	rolls := tables.ByItem[uint32(itemID)]
	if len(rolls) == 0 {
		return "", nil
	}
	out := make([]currentVariousBuffRoll, 0, len(rolls))
	family := ""
	for _, roll := range rolls {
		if roll.BuffID <= 0 || roll.Weight <= 0 {
			continue
		}
		effect, found := tables.ByBuff[roll.BuffID]
		if !found {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(effect.EffectName))
		if name == "" || serverSideStatChangeEffect(name) {
			continue
		}
		if family == "" {
			family = name
		}
		if family != name {
			continue
		}
		bonus := int64(0)
		if len(effect.EffectParams) > 0 {
			bonus = effect.EffectParams[0]
		}
		out = append(out, currentVariousBuffRoll{
			BuffID:       roll.BuffID,
			Weight:       roll.Weight,
			BonusPercent: bonus,
		})
	}
	if family == "" || len(out) == 0 {
		return "", nil
	}
	return family, out
}

// rollsForItem 返回某道具在**指定效果**上的候选（权重已归一到正数）。
//
// 一件道具的概率表里可能混着不同效果（例如掉落率 buff 与强化成功率 buff），
// 所以按效果名过滤，而不是"返回全部"。
func (tables currentVariousBuffTables) rollsForItem(itemID int64, wantEffect string) []currentVariousBuffRoll {
	if itemID <= 0 || itemID > math.MaxUint32 {
		return nil
	}
	rolls := tables.ByItem[uint32(itemID)]
	if len(rolls) == 0 {
		return nil
	}
	out := make([]currentVariousBuffRoll, 0, len(rolls))
	for _, roll := range rolls {
		if roll.BuffID <= 0 || roll.Weight <= 0 {
			continue
		}
		effect, found := tables.ByBuff[roll.BuffID]
		if !found {
			continue
		}
		if _, ok := effect.dispatchBonusPercent(wantEffect); !ok {
			continue
		}
		out = append(out, roll)
	}
	return out
}

// epicDropRateRollsForItem 返回某道具的"史诗掉落率"候选。
func (tables currentVariousBuffTables) epicDropRateRollsForItem(itemID int64) []currentVariousBuffRoll {
	return tables.rollsForItem(itemID, currentEpicRarityDropRateEffect)
}

// currentVariousBuffTables 惰性读两张 buff 表。
//
// 读不到（合成测试源、PVF 缺表）返回 ok=false，调用方回退到 `[explain]` 文字判据 ——
// 这正是 490000413 这种"表里没有条目"的老活动券所依赖的路径。
func (c *pvfDungeonDropCatalog) currentVariousBuffTables() (currentVariousBuffTables, bool) {
	if c == nil || c.source == nil {
		return currentVariousBuffTables{}, false
	}
	c.variousBuffOnce.Do(func() {
		c.variousBuffs, c.variousBuffErr = loadCurrentVariousBuffTables(c.source)
	})
	if c.variousBuffErr != nil {
		return currentVariousBuffTables{}, false
	}
	if len(c.variousBuffs.ByItem) == 0 && len(c.variousBuffs.ByBuff) == 0 {
		return currentVariousBuffTables{}, false
	}
	return c.variousBuffs, true
}
