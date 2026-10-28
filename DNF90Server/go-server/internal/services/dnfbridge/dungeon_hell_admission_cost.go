package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 深渊派对入场消耗（邀请函数量）的取值规则。
//
// 客户端副本提示里的 "进入条件 ·消耗N个深渊派对邀请函 (0)" 是客户端自己算的，
// N 来自副本自己的 [minimum required level]：
//
//	N = (40*Lv - 1800) / 100 + 10      // 整数除法，向零截断
//
// 实测（2026-09-26，角色 测试x Lv90，客户端内逐个悬停读数）：
//
//	86 列车上的海贼  Lv71 -> 20      87 夺回西部线   Lv72 -> 20
//	92 雾都赫伊斯    Lv73 -> 21      93 阿登高地     Lv74 -> 21
//	193 能源熔炉     Lv86 -> 26
//
// 五个读数全部命中该式，而且区间边界也吻合（71/72 同为 20，73/74 同为 21）。
// 换用 [basis level]（74/75/76/77/89）会得到 21/22/22/22/27，全部不符；换用角色
// 等级也不符（同一个角色读到 20/20/21/21 四个不同的数）。旧树 changlog 里
// "(40·90−1800)/100+10 = 28" 是同一个式子在 Lv90 上的取值。
//
// 真 PVF 里只有 11/205 个深渊区域副本写了 [hell party required item count]；剩下 194
// 个没写，其中 187 个的所属区域有恰好一条 [item condition]（就是 3330 邀请函），
// 7 个（镜像阿拉德 8000 与伤城 8500 一系）连 [item condition] 都没有。没有声明值时
// 必须按上面这条等级式推导：否则这些副本的入场计划建立不起来，玩家点下"深渊派对"
// 之后服务端给不出任何回执（2026-09-26 现场：193 号副本卡在选择页 26 秒）。
const (
	currentHellAdmissionLevelScale   = 40
	currentHellAdmissionLevelOffset  = 1800
	currentHellAdmissionLevelDivisor = 100
	currentHellAdmissionBaseCount    = 10
	currentHellAdmissionMinCount     = 1
)

// currentHellAdmissionCostSource* 记录这次消耗数量是从哪儿来的，只进日志：
// 有 PVF 声明值和按等级式推导，出问题时必须能一眼分清。
const (
	currentHellAdmissionCostSourceDeclared = "dungeon_hell_party_required_item_count"
	currentHellAdmissionCostSourceLevel    = "dungeon_minimum_required_level_formula"
)

// currentHellAdmissionInvitationCountForLevel is the client's own invitation
// count rule: (40*Lv-1800)/100+10 with C-style truncating division. A level that
// would price the entry at zero or below still costs one invitation.
func currentHellAdmissionInvitationCountForLevel(level int64) int64 {
	if level <= 0 {
		return 0
	}
	count := (currentHellAdmissionLevelScale*level-currentHellAdmissionLevelOffset)/
		currentHellAdmissionLevelDivisor + currentHellAdmissionBaseCount
	if count < currentHellAdmissionMinCount {
		count = currentHellAdmissionMinCount
	}
	return count
}

// currentDungeonHellAdmissionInvitationCount returns how many invitations a hell
// entry into this dungeon consumes, preferring the dungeon's own PVF value
// ([hell party required item count]) and otherwise deriving it from
// [minimum required level] exactly as the client tooltip does. The second result
// names which of the two was used.
func currentDungeonHellAdmissionInvitationCount(dungeon worldmap.Dungeon) (int64, string, error) {
	if declared := dungeon.Metadata.HellPartyRequiredItemCount; declared.Set && declared.Value > 0 {
		return declared.Value, currentHellAdmissionCostSourceDeclared, nil
	}
	level := dungeon.Metadata.MinimumRequiredLevel
	if !level.Set || level.Value <= 0 {
		return 0, "", fmt.Errorf(
			"dungeon %d has neither [hell party required item count] nor [minimum required level]",
			dungeon.ID,
		)
	}
	return currentHellAdmissionInvitationCountForLevel(level.Value), currentHellAdmissionCostSourceLevel, nil
}

// currentDungeonHellEntryRequiresInvitations reports whether this entry's
// admission item is the hell area's [item condition] -- the two branches the
// admission plan prices with the invitation count above. Those are exactly the
// entries whose client text lives in the op16 failure reason-27 block
// (0x01CFBD8B: "队伍中有队员的%s不足，无法进入深渊派对。" / 27156
// "部分队员邀请函不足，无法进入该地下城。"), so they get reason 27 instead of the
// generic reason 17. Particular-hell (魔界裂缝) entries are excluded: their plan
// comes from the area's particular-hell free pass, not from the item condition.
func currentDungeonHellEntryRequiresInvitations(runtime *runtimeDungeonState) bool {
	if runtime == nil {
		return false
	}
	if runtime.Dungeon.Metadata.InfernoDungeon {
		return true
	}
	return currentDungeonExplicitHellPartyRequest(runtime.Request, runtime.Dungeon, runtime.MazeIndex)
}
