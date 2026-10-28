//go:build paid

// 在线福利的 manage 桥接（仅赞助版）：把 App 侧的福利状态映射成管理工具的 API 视图。
//
// 为什么单开一个 paid 文件：welfare.go 是两版共用的（免费版也要能定时发福利），
// 而 manage 包只在 paid 构建里存在 —— 跨版共用的文件不能 import 它。
package main

import (
	"dnf90ops/internal/manage"
)

// welfareBridgeStatus 组装给管理工具的状态快照。
func (a *App) welfareBridgeStatus() manage.WelfareStatus {
	view := a.welfareStatus()
	return manage.WelfareStatus{
		Daily:    welfareSlotBridge(view.Daily),
		Weekly:   welfareSlotBridge(view.Weekly),
		Monthly:  welfareSlotBridge(view.Monthly),
		LastRuns: view.LastRuns,
	}
}

func welfareSlotBridge(v welfareSlotView) manage.WelfareSlotStatus {
	return manage.WelfareSlotStatus{
		Enabled: v.Enabled,
		Time:    v.Time,
		Weekday: v.Weekday,
		Day:     v.Day,
		Subject: v.Subject,
		Notice:  v.Notice,
		Rewards: welfareRewardsBridge(v.Rewards),
		NextRun: v.NextRun,
		LastRun: v.LastRun,
	}
}

func welfareRewardsBridge(in []WelfareReward) []manage.WelfareRewardStatus {
	out := make([]manage.WelfareRewardStatus, 0, len(in))
	for _, r := range in {
		out = append(out, manage.WelfareRewardStatus{
			Kind: r.Kind, ItemID: r.ItemID, Amount: r.Amount,
			ReinforceLevel: r.ReinforceLevel, AmplifyLevel: r.AmplifyLevel, AmplifyType: r.AmplifyType,
			Label: r.Label,
		})
	}
	return out
}
