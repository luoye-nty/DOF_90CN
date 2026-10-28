package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// etc/chn_absolute_bind_cube.etc 是"绝对绑定 / 梦想装扮合成器"（op1119）的输出权威表。
// 真实文件结构（证据见 梦想装扮合成器op1119_回包格式定案_20261004.md）：
//
//	[absolute bind plus value]        每个部位三档"价值"
//	`hat` 132 264 792 `hair` 198 264 792 …
//	[/absolute bind plus value]
//	[output avatar goal value]        每个部位的目标值（8 件材料合计要达标）
//	`hat` 2970 …
//	[/output avatar goal value]
//	[absolute bind cube]
//	0                                 ← 套装序号（= 道具 [action type] [absolute bind cube] 0 8 2 0 0 的第一个数）
//	[absolute bind plus rare]
//	`[swordman]` `hair` 101560739 `hat` 101550582 …
//	[/absolute bind plus rare]
//	[/absolute bind cube]
//	[absolute bind cube]
//	1 …（下一套）
//
// 两个必须注意的点：
//
//  1. `[absolute bind cube]` 在文件里**重复出现**（一套一个），所以不能用
//     currentAvatarCompoundFirstSectionInts 那种"取第一个同名段"的写法，必须按顺序
//     遍历 section 并记住当前套装序号；
//  2. 同一个输出道具会在**多个职业名下重复**（`[swordman]` / `[demonic swordman]` /
//     `[knight]` 共用同一批 101560739 等），所以职业校验要用"该道具是否挂在这个职业名下"，
//     不能只留一条记录。
const currentAvatarBindPlusPVFPath = "etc/chn_absolute_bind_cube.etc"

// 部位顺序与 PVF 里的键一致；客户端 op1119 的 8 格材料也按这个顺序上报。
var currentAvatarBindPlusPartNames = []string{"hair", "hat", "face", "neck", "coat", "pants", "belt", "shoes"}

type currentAvatarBindPlusOutput struct {
	SetIndex int64
	Part     string
	ItemID   uint32
}

type currentAvatarBindPlusConfig struct {
	PVFPath string
	// Outputs: 输出道具 → 它的套装序号与部位（同一道具在多职业下重复时取第一条，
	// 套装序号与部位一致）。
	Outputs map[uint32]currentAvatarBindPlusOutput
	// Jobs: 输出道具 → 允许请求它的职业键集合（currentAvatarBindPlusKey 归一化后的名字）。
	Jobs map[uint32]map[string]struct{}
	// Values / Goals 来自 [absolute bind plus value] / [output avatar goal value]。
	// 当前只解析保存：客户端在发送前已经自己校验过价值，服务端先用"件数"口径放行，
	// 留这两个表是为了下一步收紧（见文档"待办"）。
	Values map[string][]int64
	Goals  map[string]int64
}

// currentAvatarBindPlusKey 把职业/段落名归一化成可比较的键：
// `[at gunner]` / CompoundAvatar_atgunner.etc / at_gunner 都变成 "atgunner"。
func currentAvatarBindPlusKey(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func currentAvatarBindPlusIsPart(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strings.Trim(value, "`")))
	for _, part := range currentAvatarBindPlusPartNames {
		if normalized == part {
			return true
		}
	}
	return false
}

func loadCurrentAvatarBindPlusConfig(source dnfpvf.Source) (currentAvatarBindPlusConfig, error) {
	if source == nil {
		return currentAvatarBindPlusConfig{}, errCurrentAvatarBindPlusUnavailable
	}
	text, err := source.ReadText(currentAvatarBindPlusPVFPath)
	if err != nil {
		return currentAvatarBindPlusConfig{}, errors.Join(errCurrentAvatarBindPlusUnavailable, err)
	}
	document, err := dnfpvf.Parse(currentAvatarBindPlusPVFPath, text)
	if err != nil {
		return currentAvatarBindPlusConfig{}, errors.Join(errCurrentAvatarBindPlusUnavailable, err)
	}
	config := currentAvatarBindPlusConfig{
		PVFPath: currentAvatarBindPlusPVFPath,
		Outputs: make(map[uint32]currentAvatarBindPlusOutput),
		Jobs:    make(map[uint32]map[string]struct{}),
		Values:  make(map[string][]int64),
		Goals:   make(map[string]int64),
	}
	currentSet := int64(-1)
	for _, section := range document.Sections {
		// 收尾段 `[/absolute bind cube]` 的 Name 是 "/absolute bind cube"：
		// 归一化会把斜杠吃掉、和开头段撞名，所以先按斜杠判掉。
		if strings.HasPrefix(strings.TrimSpace(section.Name), "/") {
			continue
		}
		sectionKey := currentAvatarBindPlusKey(section.Name)
		switch sectionKey {
		case "absolutebindcube":
			values := currentAvatarCompoundTokenInts(currentDisjointSectionTokens(document, section))
			if len(values) == 0 {
				return currentAvatarBindPlusConfig{}, fmt.Errorf("%w: %s [%s] 没有套装序号",
					errCurrentAvatarBindPlusUnavailable, currentAvatarBindPlusPVFPath, section.Name)
			}
			currentSet = values[0]
		case "absolutebindplusrare":
			if currentSet < 0 {
				continue
			}
			job := ""
			pendingPart := ""
			for _, token := range currentDisjointSectionTokens(document, section) {
				if token.Kind == dnfpvf.TokenInt {
					if pendingPart == "" || job == "" {
						pendingPart = ""
						continue
					}
					itemID := token.Int
					if itemID <= 0 || itemID > math.MaxUint32 {
						pendingPart = ""
						continue
					}
					output := currentAvatarBindPlusOutput{
						SetIndex: currentSet,
						Part:     pendingPart,
						ItemID:   uint32(itemID),
					}
					if _, exists := config.Outputs[output.ItemID]; !exists {
						config.Outputs[output.ItemID] = output
					}
					if config.Jobs[output.ItemID] == nil {
						config.Jobs[output.ItemID] = make(map[string]struct{}, 4)
					}
					config.Jobs[output.ItemID][job] = struct{}{}
					pendingPart = ""
					continue
				}
				value := strings.TrimSpace(token.Value)
				if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
					job = currentAvatarBindPlusKey(value)
					pendingPart = ""
					continue
				}
				if currentAvatarBindPlusIsPart(value) {
					pendingPart = strings.ToLower(strings.TrimSpace(strings.Trim(value, "`")))
				}
			}
		case "absolutebindplusvalue", "outputavatargoalvalue":
			part := ""
			values := make([]int64, 0, 3)
			flush := func() {
				if part == "" || len(values) == 0 {
					return
				}
				if sectionKey == "absolutebindplusvalue" {
					config.Values[part] = append([]int64(nil), values...)
				} else {
					config.Goals[part] = values[0]
				}
			}
			for _, token := range currentDisjointSectionTokens(document, section) {
				if token.Kind == dnfpvf.TokenInt {
					values = append(values, token.Int)
					continue
				}
				value := strings.TrimSpace(token.Value)
				if currentAvatarBindPlusIsPart(value) {
					flush()
					part = strings.ToLower(strings.TrimSpace(strings.Trim(value, "`")))
					values = values[:0]
				}
			}
			flush()
		}
	}
	if len(config.Outputs) == 0 {
		return currentAvatarBindPlusConfig{}, fmt.Errorf("%w: %s 没有解析出任何 [absolute bind plus rare] 输出",
			errCurrentAvatarBindPlusUnavailable, currentAvatarBindPlusPVFPath)
	}
	return config, nil
}

// currentAvatarBindPlusJobKey 把角色的 job 字节换算成 chn_absolute_bind_cube.etc 里
// 那个职业名（归一化后）。映射来源就是合成器自己的职业索引表 etc/compoundavatar.etc：
// `1 CompoundAvatar_swordman.etc 2 CompoundAvatar_fighter.etc …`，即 job+1 → 文件名。
func currentAvatarBindPlusJobKey(source dnfpvf.Source, job byte) (string, error) {
	if source == nil {
		return "", errCurrentAvatarBindPlusUnavailable
	}
	text, err := source.ReadText("etc/compoundavatar.etc")
	if err != nil {
		return "", errors.Join(errCurrentAvatarBindPlusUnavailable, err)
	}
	document, err := dnfpvf.Parse("etc/compoundavatar.etc", text)
	if err != nil {
		return "", errors.Join(errCurrentAvatarBindPlusUnavailable, err)
	}
	fileName := ""
	for offset := 0; offset+1 < len(document.Tokens); offset++ {
		left, right := document.Tokens[offset], document.Tokens[offset+1]
		if left.Kind != dnfpvf.TokenInt || left.Int != int64(job)+1 {
			continue
		}
		if right.Kind == dnfpvf.TokenString || right.Kind == dnfpvf.TokenIdent {
			fileName = strings.TrimSpace(right.Value)
			break
		}
	}
	if fileName == "" {
		return "", fmt.Errorf("%w: job=%d 在 etc/compoundavatar.etc 里没有对应职业文件", errCurrentAvatarBindPlusUnavailable, job)
	}
	stem := strings.TrimSuffix(fileName, path.Ext(fileName))
	stem = strings.TrimPrefix(stem, "CompoundAvatar_")
	if key := currentAvatarBindPlusKey(stem); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("%w: job=%d 职业文件=%q", errCurrentAvatarBindPlusUnavailable, job, fileName)
}

// currentAvatarBindPlusResolveOutput 解析"玩家点名的输出装扮"：
// 必须出现在 [absolute bind plus rare] 里，且必须挂在当前职业名下。
func currentAvatarBindPlusResolveOutput(
	config currentAvatarBindPlusConfig,
	jobKey string,
	itemID uint32,
) (currentAvatarBindPlusOutput, error) {
	output, exists := config.Outputs[itemID]
	if !exists {
		return currentAvatarBindPlusOutput{}, fmt.Errorf("%w: item=%d 不在 %s 的 [absolute bind plus rare] 里",
			errCurrentAvatarBindPlusOutputInvalid, itemID, config.PVFPath)
	}
	if _, allowed := config.Jobs[itemID][jobKey]; !allowed {
		return currentAvatarBindPlusOutput{}, fmt.Errorf("%w: item=%d job=%s 这个职业不能指定该输出（部位=%s 套装=%d）",
			errCurrentAvatarBindPlusOutputInvalid, itemID, jobKey, output.Part, output.SetIndex)
	}
	return output, nil
}

// currentAvatarBindPlusCubeSet reads one 绝对绑定合成器道具的套装序号：
// [action type] `[absolute bind cube] 0 8 2 0 0` 的第一个数。
func currentAvatarBindPlusCubeSet(definition dungeonDropItemDefinition) (int64, bool) {
	if !currentAvatarBindPlusDeclaresCube(definition) {
		return 0, false
	}
	if definition.ActionValues != nil && len(definition.ActionValues.Values) > 0 {
		return definition.ActionValues.Values[0], true
	}
	return 0, false
}

func currentAvatarBindPlusDeclaresCube(definition dungeonDropItemDefinition) bool {
	action := strings.ToLower(strings.TrimSpace(strings.Trim(definition.ActionType, "`")))
	return action == "[absolute bind cube]"
}
