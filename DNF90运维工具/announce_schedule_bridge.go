//go:build paid

// 定时公告的 manage 桥接（仅赞助版）：把 App 侧的定时公告视图映射成管理工具的 API 视图。
//
// 为什么单开一个 paid 文件：announce_schedule.go 是两版共用的（免费版也照样编译），
// 而 manage 包只在 paid 构建里存在 —— 跨版共用的文件不能 import 它。
package main

import (
	"dnf90ops/internal/manage"
)

// announceScheduleBridge 把 App 视图转成管理工具视图（内嵌配置 + next_run）。
func announceScheduleBridge(in []announceScheduleItem) []manage.ScheduledAnnouncementStatus {
	out := make([]manage.ScheduledAnnouncementStatus, 0, len(in))
	for _, it := range in {
		out = append(out, manage.ScheduledAnnouncementStatus{
			ScheduledAnnouncement: it.ScheduledAnnouncement,
			NextRun:               it.NextRun,
		})
	}
	return out
}
