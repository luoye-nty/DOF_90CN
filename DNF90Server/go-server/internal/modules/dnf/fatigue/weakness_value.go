package fatigue

// 本文件只提供一个投影函数，供决斗场(PVP)代码读取"虚弱/状态百分比"。
//
// 重要：这棵树的 stamina 语义是**惩罚等级 0..10**，不是百分比。
// 权威实现在 dnfbridge/weakness_state.go：
//
//	level     = clamp(stats["stamina"], 0, 10)
//	condition = 100 - level*9        （level=9 -> 19%，level=10 -> 10%）
//
// 决斗场代码（远端玩家 actor / 街头争霸成员行）需要一个"取状态百分比"的入口，
// 参考树把它做成了 fatigue.WeaknessValue(stats)。这里给出同名 API，但**保持本树
// 的等级语义**，因此它是 dnfbridge 那套虚弱系统的只读投影，不改变任何既有行为，
// 也不引入参考树的百分比数据模型——虚弱系统不属于本次决斗场移植范围。
const (
	// WeaknessStatKey 是惩罚等级所在的角色 stat 键。
	WeaknessStatKey = "stamina"
	// WeaknessMaximumLevel 与客户端自身的钳位一致。
	WeaknessMaximumLevel int64 = 10
	// WeaknessConditionStep 是每一级惩罚损失的状态百分比。
	WeaknessConditionStep int64 = 9
	// WeaknessHealthyValue 是客户端视为健康的状态百分比。
	WeaknessHealthyValue int64 = 100
)

// WeaknessValue 把持久化的惩罚等级投影成客户端消费的状态百分比。
// Stats 为 nil、缺键或为负时按 0 级处理（=100%）。
func WeaknessValue(stats map[string]int64) int64 {
	level := stats[WeaknessStatKey]
	if level < 0 {
		level = 0
	}
	if level > WeaknessMaximumLevel {
		level = WeaknessMaximumLevel
	}
	condition := WeaknessHealthyValue - level*WeaknessConditionStep
	if condition < 0 {
		return 0
	}
	return condition
}
