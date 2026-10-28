//go:build !paid

// 免费版页面组装：不编译 internal/manage 与 remote_tab.go（build tag 隔离），
// 主窗口保留基础三页（一键启动/认证服务/封禁管理）+ 账号管理（GM 直连，免费可用）
// + 背包编辑（GM Tool 直连，免费可用）；
// CDK 管理与远程管理仅付费版开放。

package main

import "github.com/lxn/walk/declarative"

// mainPages 页面集合：免费版 = 基础三页 + 账号管理（第4页，GM 直连免费可用）+ 背包编辑。
// 付费版页面集合同样由此装配（见 remote_tab.go 的构建说明），账号管理/背包编辑两端一致。
func (a *App) mainPages() []declarative.TabPage {
	return []declarative.TabPage{
		{Title: "一键启动", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.serviceTab()}},
		{Title: "认证服务", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.authTab()}},
		{Title: "封禁管理", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.banTab()}},
		{Title: "账号管理", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.accountTab()}},
		{Title: "背包编辑", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.inventoryTab()}},
		{Title: "频道管理", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.channelTab()}},
		{Title: "数据库备份", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.backupTab()}},
		{Title: "运行维护", Content: declarative.Composite{Layout: declarative.VBox{MarginsZero: true, SpacingZero: true}, Children: a.maintTab()}},
	}
}

// maybeStartManage 免费版不提供后台管理服务（空实现）。
func (a *App) maybeStartManage() {}
