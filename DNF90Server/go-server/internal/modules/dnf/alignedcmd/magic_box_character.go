package alignedcmd

import (
	"strconv"

	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// MagicBoxGroupsForCharacter 过滤掉与角色职业/等级不匹配的 `[charactor]` 组。
//
// [booster info] 里一个 `[charactor] \`[swordman]\` 1 99 1 <item> <weight> <count>`
// 就是一组"只发给该职业"的奖励。礼盒一旦按职业分支（例如豪华婚礼化妆箱：鬼剑士
// 拿男鬼剑时装、女鬼剑士拿 at swordman 那组），就**只能发开启者自己那一组**；
// 不过滤的话会把所有职业的时装/道具一起发出去。
//
// Job 为空的分组（普通 [etc]/[stackable]/[avatar] 组）不受影响，原样保留。
func MagicBoxGroupsForCharacter(groups []MagicBoxRewardGroup, character dnfrepo.CharacterRecord) []MagicBoxRewardGroup {
	job, err := strconv.Atoi(character.Job)
	result := make([]MagicBoxRewardGroup, 0, len(groups))
	for _, group := range groups {
		if group.Job != "" &&
			(err != nil || !jobmap.MatchesQuestTag(group.Job, job) ||
				int64(character.Level) < group.MinLevel || int64(character.Level) > group.MaxLevel) {
			continue
		}
		result = append(result, group)
	}
	return result
}
