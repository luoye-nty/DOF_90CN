package main

// 服务器选项页：全服公告（喇叭广播）发送；PVF 加密占位（501 壳子）。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// psMaxDailyLimit 账号级每日限额上限（0 = 不限），与运维工具 psMaxDailyLimit 同口径。
const psMaxDailyLimit = 100000000

// 公告颜色（DNF 喇叭实际字节序 0xAABBGGRR：低字节=R，高字节=A）。
var announceColorNames = []string{"红色", "蓝色", "绿色", "黄色", "白色", "粉色", "橙色"}
var announceColorValues = []string{
	"0xFF0000FF", // 红 R=FF G=00 B=00
	"0xFFFF0000", // 蓝 R=00 G=00 B=FF
	"0xFF00FF00", // 绿 R=00 G=FF B=00
	"0xFF00FFFF", // 黄 R=FF G=FF B=00
	"0xFFFFFFFF", // 白
	"0xFFFF00FF", // 粉 R=FF G=00 B=FF
	"0xFF0080FF", // 橙 R=FF G=80 B=00
}

func buildAnnounceTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "服务器选项",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		// 本页堆了 8 个分组（公告/泡点/注册赠送/注册限制+注册码/更新维护/登录器版本校验/管理员/运行状态），
		// 天然高度远超一屏：套一层 ScrollView，屏幕再小也能滚到底点按钮。
		Children: []decl.Widget{
			decl.ScrollView{
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.GroupBox{
						Title:  "全服公告（喇叭广播）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "公告内容（≤100 字，可换行；发送后实时推送到所有在线 90 版登录器）"},
							decl.TextEdit{
								AssignTo: &a.announceEdit,
								MinSize:  decl.Size{Width: 0, Height: 60},
								VScroll:  true,
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "颜色："},
									decl.ComboBox{
										AssignTo: &a.announceColorCb,
										Model:    announceColorNames,
									},
									decl.PushButton{
										AssignTo:  &a.announceBtn,
										Text:      "发送公告",
										OnClicked: func() { a.onAnnounceSend() },
									},
									decl.Label{AssignTo: &a.announceMsgL, Text: "尚未发送", TextColor: walk.RGB(120, 120, 120)},
								},
							},
						},
					},
					decl.GroupBox{
						Title:  "定时公告（一次性 / 每天 / 每周，可多条排队）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "到点由服务器端自动发送，走的就是上面「发送公告」同一条链路（游戏内喇叭 + 在线登录器实时通知）。"},
							decl.Label{Text: "同一周期只发一次；运维工具没开导致错过窗口不补发（运维工具日志会记录）。一次性公告到点后不会重复发送，可删除或停用。"},
							// 新增/编辑一行：填内容与时间 → 「添加到列表」；
							// 选中表格某行后可「更新选中」（把当前输入覆盖到那一行）。
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "内容(≤100字):"},
									decl.LineEdit{
										AssignTo:    &a.asContentLE,
										MaxLength:   100,
										MinSize:     decl.Size{Width: 320, Height: 0},
										ToolTipText: "到点发送的公告内容（与「发送公告」同一条链路）",
									},
									decl.Label{Text: "颜色："},
									decl.ComboBox{
										AssignTo:     &a.asColorCb,
										Model:        announceColorNames,
										CurrentIndex: 1,
									},
								},
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "方式："},
									decl.ComboBox{
										AssignTo:              &a.asModeCb,
										Model:                 []string{"每天", "一次性", "每周"},
										OnCurrentIndexChanged: func() { a.asSyncModeFields() },
									},
									decl.Label{Text: "时间："},
									// 时分改成两个下拉（小时 0-23 / 分钟 0-59）：手打 "HH:MM" 容易错，
									// 下拉从源头消掉格式类错误（服务端协议仍是 "HH:MM"）。
									decl.ComboBox{
										AssignTo:     &a.asTimeHourCb,
										Model:        timeHourLabels,
										CurrentIndex: 20,
										MinSize:      decl.Size{Width: 60, Height: 0},
										ToolTipText:  "小时（0-23）",
									},
									decl.Label{Text: "时"},
									decl.ComboBox{
										AssignTo:     &a.asTimeMinCb,
										Model:        timeMinuteLabels,
										CurrentIndex: 0,
										MinSize:      decl.Size{Width: 60, Height: 0},
										ToolTipText:  "分钟（0-59）",
									},
									decl.Label{Text: "分"},
									decl.Label{AssignTo: &a.asDateLabel, Text: "日期(YYYY-MM-DD)："},
									decl.LineEdit{
										AssignTo: &a.asDateLE,
										Text:     time.Now().Format("2006-01-02"),
										MinSize:  decl.Size{Width: 120, Height: 0},
										MaxSize:  decl.Size{Width: 130, Height: 0},
									},
									decl.Label{AssignTo: &a.asWeekdayLabel, Text: "星期："},
									decl.ComboBox{
										AssignTo: &a.asWeekdayCb,
										Model:    []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"},
									},
									decl.PushButton{
										AssignTo:  &a.asAddBtn,
										Text:      "添加到列表",
										OnClicked: func() { a.onAnnounceScheduleAdd() },
									},
									decl.PushButton{
										AssignTo:  &a.asUpdateBtn,
										Text:      "更新选中行",
										OnClicked: func() { a.onAnnounceScheduleUpdate() },
									},
								},
							},
							decl.TableView{
								AssignTo:   &a.asTable,
								CheckBoxes: true,
								Model:      new(asTableModel),
								Columns: []decl.TableViewColumn{
									{Title: "启用", Width: 50},
									{Title: "内容", Width: 300},
									{Title: "方式", Width: 70},
									{Title: "时间", Width: 60},
									{Title: "日期/星期", Width: 100},
									{Title: "下次发送", Width: 130},
									{Title: "最近已处理", Width: 100},
								},
								MinSize: decl.Size{Width: 0, Height: 150},
								// 双击某行：把它回填到上面的输入框，便于改完「更新选中行」。
								OnItemActivated: func() { a.onAnnounceSchedulePick() },
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo:  &a.asSaveBtn,
										Text:      "保存全部到服务器",
										OnClicked: func() { a.onAnnounceScheduleSave() },
									},
									decl.PushButton{
										AssignTo:  &a.asDelBtn,
										Text:      "删除选中行",
										OnClicked: func() { a.onAnnounceScheduleDelete() },
									},
									decl.PushButton{
										AssignTo:  &a.asRefreshBtn,
										Text:      "刷新列表",
										OnClicked: func() { a.onAnnounceScheduleRefresh(false) },
									},
									decl.Label{AssignTo: &a.asMsgL, Text: "尚未加载", TextColor: walk.RGB(120, 120, 120)},
								},
							},
							decl.Label{Text: "提示：勾选框 = 是否启用该条；表格改动只在本页生效，点「保存全部到服务器」后才会写入服务器并开始计时。"},
						},
					},
					decl.GroupBox{
						Title:  "服务器公告栏（显示在登录器登录页左侧）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "这里是公告板（标题 + 日期，无正文），玩家每次打开登录器都会看到；停用的条目保留配置但不显示。"},
							decl.Label{Text: "与上面「全服公告」不同：全服公告是游戏内滚动的喇叭公告，这里只是登录器上的静态公告板。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "标题(≤60字):"},
									decl.LineEdit{
										AssignTo:    &a.ntTitleLE,
										MaxLength:   60,
										MinSize:     decl.Size{Width: 380, Height: 0},
										ToolTipText: "公告板上一行文字（例如：9 月 24 日 20:00 全服维护）",
									},
									decl.Label{Text: "日期(可空,YYYY-MM-DD):"},
									// 注意：walk 的 LineEdit 会按 MaxLength 反推理想宽度（10 字符 → 会被算得很窄），
									// 所以这里必须显式给 MinSize，否则日期框会被压成一条缝（2026-09-24 修）。
									decl.LineEdit{
										AssignTo:    &a.ntDateLE,
										Text:        time.Now().Format("2006-01-02"),
										MaxLength:   10,
										MinSize:     decl.Size{Width: 130, Height: 0},
										MaxSize:     decl.Size{Width: 140, Height: 0},
										ToolTipText: "留空则不显示日期",
									},
									decl.PushButton{
										AssignTo:  &a.ntAddBtn,
										Text:      "添加到列表",
										OnClicked: func() { a.onNoticesAdd() },
									},
									decl.PushButton{
										AssignTo:  &a.ntUpdateBtn,
										Text:      "更新选中行",
										OnClicked: func() { a.onNoticesUpdate() },
									},
								},
							},
							decl.TableView{
								AssignTo:   &a.ntTable,
								CheckBoxes: true,
								Model:      new(ntTableModel),
								Columns: []decl.TableViewColumn{
									{Title: "启用", Width: 50},
									{Title: "标题", Width: 460},
									{Title: "日期", Width: 110},
								},
								MinSize: decl.Size{Width: 0, Height: 130},
								// 双击某行：回填到输入框，改完点「更新选中行」。
								OnItemActivated: func() { a.onNoticesPick() },
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo:  &a.ntSaveBtn,
										Text:      "保存全部到服务器",
										OnClicked: func() { a.onNoticesSave() },
									},
									decl.PushButton{
										AssignTo:  &a.ntDelBtn,
										Text:      "删除选中行",
										OnClicked: func() { a.onNoticesDelete() },
									},
									decl.PushButton{
										AssignTo:  &a.ntRefreshBtn,
										Text:      "刷新列表",
										OnClicked: func() { a.onNoticesRefresh(false) },
									},
									decl.Label{AssignTo: &a.ntMsgL, Text: "尚未加载", TextColor: walk.RGB(120, 120, 120)},
								},
							},
							decl.Label{Text: "提示：表格改动只在本页生效，点「保存全部到服务器」后登录器才会看到（玩家重开登录器即刷新）。"},
						},
					},
					decl.GroupBox{
						Title:  "可加载 DLL（登录器启动时校验并自动注入）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "先在本机选中要用的 dll → 管理工具算出文件名与 SHA256 → 保存到服务器；dll 本体请自行随客户端分发（放 DNF.exe 同目录）。"},
							decl.Label{Text: "登录器（外网版）在启动自检与玩家点登录时各拉一次清单：客户端根目录没有该 dll、或哈希对不上时，按下面选的策略处理。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo:    &a.dlPickBtn,
										Text:        "选择本地 DLL…",
										OnClicked:   func() { a.onDllPickFile() },
										ToolTipText: "只读取文件名与 SHA256，文件本体不会上传",
									},
									decl.Label{Text: "文件名:"},
									decl.LineEdit{
										AssignTo:    &a.dlNameLE,
										MinSize:     decl.Size{Width: 160, Height: 0},
										ToolTipText: "客户端根目录下的文件名（不能带路径），例如 XiuXiu.dll",
									},
									decl.Label{Text: "SHA256:"},
									decl.LineEdit{
										AssignTo:    &a.dlHashLE,
										MinSize:     decl.Size{Width: 380, Height: 0},
										ReadOnly:    true,
										ToolTipText: "选择文件后自动算出（只读）；保存到服务器的是这个哈希",
									},
								},
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "校验不通过时:"},
									decl.ComboBox{
										AssignTo:     &a.dlPolicyCb,
										Model:        []string{"跳过注入（照常进游戏）", "拒绝启动游戏（登录器弹窗提示）"},
										CurrentIndex: 0,
										MinSize:      decl.Size{Width: 240, Height: 0},
										ToolTipText:  "「拒绝启动」适合反作弊模块这类必须存在的 dll；「跳过注入」适合可选功能 dll",
									},
									decl.PushButton{
										AssignTo:  &a.dlAddBtn,
										Text:      "添加到列表",
										OnClicked: func() { a.onDllAdd() },
									},
									decl.PushButton{
										AssignTo:  &a.dlUpdateBtn,
										Text:      "更新选中行",
										OnClicked: func() { a.onDllUpdate() },
									},
									decl.Label{Text: "（登录器校验通过后，会在启动游戏时把该 dll 注入客户端进程）"},
								},
							},
							decl.TableView{
								AssignTo:   &a.dlTable,
								CheckBoxes: true,
								Model:      new(dlTableModel),
								Columns: []decl.TableViewColumn{
									{Title: "启用", Width: 50},
									{Title: "文件名", Width: 160},
									{Title: "SHA256", Width: 460},
									{Title: "校验不通过时", Width: 190},
								},
								MinSize:         decl.Size{Width: 0, Height: 130},
								OnItemActivated: func() { a.onDllPick() },
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo:  &a.dlSaveBtn,
										Text:      "保存全部到服务器",
										OnClicked: func() { a.onDllSave() },
									},
									decl.PushButton{
										AssignTo:  &a.dlDelBtn,
										Text:      "删除选中行",
										OnClicked: func() { a.onDllDelete() },
									},
									decl.PushButton{
										AssignTo:  &a.dlRefreshBtn,
										Text:      "刷新列表",
										OnClicked: func() { a.onDllRefresh(false) },
									},
									decl.Label{AssignTo: &a.dlMsgL, Text: "尚未加载", TextColor: walk.RGB(120, 120, 120)},
								},
							},
							decl.Label{Text: "提示：改动只在本页生效，点「保存全部到服务器」后才写入；玩家那边下次打开登录器即生效。"},
						},
					},
					decl.GroupBox{
						Title:  "泡点规则（实时生效并持久化到服务器 config.json）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "只给「游戏角色真正在线」的账号发放；发放成功后按账号隔离推送绿字到账提示，掉线重连后重新计时。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.psChk,
										Text:     "启用泡点",
									},
									decl.Label{Text: "发放间隔（秒，10~86400）"},
									decl.LineEdit{
										AssignTo: &a.psIntLE,
										Text:     "300",
										MaxSize:  decl.Size{Width: 90, Height: 0},
									},
									decl.Label{Text: "每轮发放数量（1~1000000）"},
									decl.LineEdit{
										AssignTo: &a.psPtsLE,
										Text:     "100",
										MaxSize:  decl.Size{Width: 90, Height: 0},
									},
									decl.Label{Text: "发放币种"},
									decl.ComboBox{
										AssignTo:     &a.psCurCb,
										Model:        psCurrencyLabels,
										CurrentIndex: 0,
										MaxSize:      decl.Size{Width: 130, Height: 0},
									},
									decl.Label{Text: "账号每日限额（数量，0 或留空 = 不限）"},
									decl.LineEdit{
										AssignTo: &a.psLimitLE,
										Text:     "0",
										MaxSize:  decl.Size{Width: 100, Height: 0},
									},
									decl.PushButton{
										AssignTo:  &a.psSaveBtn,
										Text:      "保存配置",
										OnClicked: func() { a.onPointsinkSave() },
									},
									decl.PushButton{
										AssignTo:  &a.psRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onPointsinkRefresh(false) },
									},
								},
							},
						},
					},
					decl.GroupBox{
						Title:  "注册赠送（新账号注册成功立即到账）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "仅对新注册账号一次性赠送：注册成功即写入账号余额（点券 dnf_account_metadata/account_cera，代币券 account_token_cera），与角色是否在线无关，入账失败不影响注册。"},
							decl.Label{Text: "币种二选一：点券（充值类，商城通用）或代币券（绑定类，拍卖/寄售那条线发的就是它）。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.rbChk,
										Text:     "启用注册赠送",
									},
									decl.Label{Text: "每新账号（1~1000000）"},
									decl.LineEdit{
										AssignTo: &a.rbPtsLE,
										Text:     "100",
										MaxSize:  decl.Size{Width: 90, Height: 0},
									},
									decl.Label{Text: "币种"},
									decl.ComboBox{
										AssignTo:     &a.rbCurCb,
										Model:        rbCurrencyLabels,
										CurrentIndex: 0,
										MaxSize:      decl.Size{Width: 110, Height: 0},
									},
									decl.PushButton{
										AssignTo:  &a.rbSaveBtn,
										Text:      "保存配置",
										OnClicked: func() { a.onRegisterBonusSave() },
									},
									decl.PushButton{
										AssignTo:  &a.rbRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onRegisterBonusRefresh(false) },
									},
								},
							},
							decl.Label{AssignTo: &a.rbStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "注册限制（关闭后玩家无法自助注册，仅管理员可建号；可按 IP 限制注册数量；可要求注册码）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "「关闭玩家自助注册」开启后，登录器注册接口将被服务器直接拦截并提示“当前服务器已限制注册，请联系管理员”；已有账号登录不受影响。"},
							decl.Label{Text: "「每 IP 最多注册」在自助注册仍然开放时，额外按注册来源 IP 计数拦截；管理员建号不占额度。填 -1 = 不限制，填 0 = 不允许注册（等于全拒）。"},
							decl.Label{Text: "「注册需要注册码」开启后，自助注册仍开放但必须先填一枚未使用的注册码；登录器会在自检时得知并多显示一个注册码输入框。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.regChk,
										Text:     "关闭玩家自助注册",
									},
									decl.CheckBox{
										AssignTo: &a.regCodeChk,
										Text:     "注册需要注册码",
									},
									decl.Label{Text: "每 IP 最多注册:"},
									decl.LineEdit{
										AssignTo:    &a.regMaxIPLE,
										Text:        "-1",
										MaxLength:   5,
										MinSize:     decl.Size{Width: 70, Height: 0},
										ToolTipText: "同一个来源 IP 允许注册的账号数上限：-1=不限制，0=不允许注册，1~1000=每个 IP 最多这么多个。留空按 -1（不限制）处理。",
									},
									decl.Label{Text: "个（-1=不限制，0=不允许注册）"},
									decl.PushButton{
										AssignTo:  &a.regSaveBtn,
										Text:      "保存配置",
										OnClicked: func() { a.onRegistrationLimitSave() },
									},
									decl.PushButton{
										AssignTo:  &a.regRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onRegistrationLimitRefresh(false) },
									},
								},
							},
							decl.Label{AssignTo: &a.regStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
							// ---- 注册码生成区（一次性码：一码一人，用完即失效）----
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "生成注册码:"},
									decl.LineEdit{
										AssignTo:    &a.regCodeCount,
										Text:        "1",
										MaxLength:   3,
										MinSize:     decl.Size{Width: 50, Height: 0},
										ToolTipText: "一次生成几枚（1~200）。码为 8 位短码，形如 ABCD-EFGH，一次性：注册用过就失效。",
									},
									decl.PushButton{
										AssignTo:  &a.regCodeGenBtn,
										Text:      "生成",
										OnClicked: func() { a.onRegCodeGenerate() },
									},
									decl.Label{Text: "显示:"},
									decl.ComboBox{
										AssignTo:              &a.regCodeFilter,
										Model:                 regCodeFilterOptions,
										CurrentIndex:          0,
										OnCurrentIndexChanged: func() { a.onRegCodeFilterChanged() },
									},
									decl.PushButton{
										AssignTo: &a.regCodeReloadBtn,
										Text:     "刷新列表",
										OnClicked: func() {
											a.onRegCodeRefresh(false)
										},
										ToolTipText: "重新拉取注册码列表（与顶部「刷新全部配置」走同一个接口，按当前「显示」条件查询）。\n切换「显示」下拉也会自动刷新。",
									},
									decl.PushButton{
										AssignTo:  &a.regCodeCopyBtn,
										Text:      "复制选中",
										OnClicked: func() { a.onRegCodeCopy() },
									},
									decl.PushButton{
										AssignTo:  &a.regCodeRevokeBtn,
										Text:      "作废选中(未使用)",
										OnClicked: func() { a.onRegCodeRevoke() },
									},
								},
							},
							decl.ListBox{
								AssignTo: &a.regCodeList,
								MinSize:  decl.Size{Width: 0, Height: 110},
								Font:     decl.Font{Family: "Consolas", PointSize: 9},
							},
							decl.Label{AssignTo: &a.regCodeStateL, Text: "注册码：尚未查询", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "更新维护（开启后玩家无法登录、无法进游戏；注册照常；白名单账号可进）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "勾选并保存后：服务端立刻拒绝玩家登录与新的游戏连接，并唤醒所有已打开的登录器 —— 登录按钮变「正在维护」且不可点；正在游戏中的玩家会被登录器直接结束游戏进程。"},
							decl.Label{Text: "取消勾选保存后：所有已打开的登录器立刻收到通知，登录按钮恢复可用，不需要玩家重开登录器。"},
							decl.Label{Text: "白名单里的账号在维护期间仍可登录与进游戏（管理员自测用）；多个账号用逗号或空格分隔。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.maintChk,
										Text:     "开启更新维护",
									},
									decl.Label{Text: "白名单账号:"},
									decl.LineEdit{
										AssignTo:    &a.maintAllowLE,
										MinSize:     decl.Size{Width: 320, Height: 0},
										ToolTipText: "维护期间允许登录/进游戏的账号名，逗号或空格分隔（留空=谁都进不去）。最多 50 个。",
									},
									decl.PushButton{
										AssignTo:  &a.maintSaveBtn,
										Text:      "保存维护配置",
										OnClicked: func() { a.onMaintenanceSave() },
									},
									decl.PushButton{
										AssignTo:  &a.maintRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onMaintenanceRefresh(false) },
									},
								},
							},
							decl.Label{AssignTo: &a.maintStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "双开开关（允许/禁止玩家双开；关闭后新登录器不能再开，**已在运行的副账号不会被踢下线**）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "勾选「允许双开」并保存后：玩家登录器上的「双开」勾选框可用，点「副账号登录」可以再拉一个游戏窗口（线上实际最大 2 开）。"},
							decl.Label{Text: "取消勾选并保存后：玩家下次打开登录器时，「双开」勾选框与「副账号登录」按钮变灰不可点（按钮文案变成「双开已关闭」），点下去也会被拦下并提示「服务器已关闭双开，如需开启请联系管理员」。"},
							decl.Label{Text: "⚠ 关闭只是「以后不许再开」：**已经在运行的副账号不会被踢下线**。这一点与「更新维护」故意不同 —— 维护会立刻结束游戏进程，双开开关不碰任何进程。要马上把双开的人赶下线，请用维护或在「在线列表」里踢人。", TextColor: walk.RGB(190, 0, 0)},
							decl.Label{Text: "⚠ 只对**认识这个开关的新登录器**生效：老登录器照旧能双开。服务端不参与双开许可判定（不同账号双开在服务端本来就是合法的），所以这是「新登录器自觉遵守」，不是硬拦。"},
							decl.Label{Text: "适用版本：免费版与外网版登录器**都**生效（双开功能不在版本条件编译里）。"},
							decl.Label{Text: "从未在后台设置过时（老配置）按「允许双开」处理 —— 升级运维工具不会让全服双开突然消失。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.moChk,
										Text:     "允许双开（多开）",
									},
									decl.PushButton{
										AssignTo:  &a.moSaveBtn,
										Text:      "保存双开开关",
										OnClicked: func() { a.onMultiOpenSave() },
									},
									decl.PushButton{
										AssignTo:  &a.moRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onMultiOpenRefresh(false) },
									},
								},
							},
							decl.Label{AssignTo: &a.moStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "登录器版本校验（开启后：版本不符的登录器在自检阶段强制更新，更新完成前无法进入登录界面）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "勾选并保存后：登录器在「启动自检阶段」与「点登录时」各上报一次自己的版本号，与下面显示的版本号完全一致才放行；"},
							decl.Label{Text: "不一致 → 登录器立刻停在更新窗口，从「登录器下载直链」下载新版本 → 删掉原登录器 → 自动打开新版本登录器。更新完成前禁止进入登录界面。"},
							decl.Label{Text: "⚠ 旧版登录器（没有版本校验代码的老包）不会被服务端识别，它拿不到**任何**接口：不能注册、不能登录、探测不到在线、拉不到公告/泡点，只会看到一句「登录器版本过旧，请联系管理员下载新版登录器」。", TextColor: walk.RGB(190, 0, 0)},
							decl.Label{Text: "⚠ 所以开启前请先把新版登录器发出去（挂上直链、群里通知），等大部分玩家换完再开这个开关：一开就是硬切换，还没更新的玩家立刻什么都做不了。", TextColor: walk.RGB(190, 0, 0)},
							decl.Label{Text: "⚠ 版本号是运维工具内置的（发布新登录器时同步更新运维工具并重启认证服务），本页只读展示、不可修改，避免手滑填错把全服锁在更新界面。"},
							decl.Label{Text: "⚠ 「登录器下载直链」只给新登录器自动更新用；没填的话新登录器更新不了、只能提示「请联系管理员」（旧登录器反正都看到那句，不看这个直链）。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.lvChk,
										Text:     "登录器版本校验",
									},
									decl.Label{Text: "登录器下载直链:"},
									decl.LineEdit{
										AssignTo: &a.lvUrlLE,
										MinSize:  decl.Size{Width: 420, Height: 0},
										ToolTipText: "登录器的下载直链，压缩包（zip/7z/rar），包里只放登录器一个 exe（名字随便，登录器自己识别）。\n" +
											"例：https://example.com/launcher/异环登录器.zip   （注意：直链本身不能含中文/空格）",
									},
									decl.PushButton{
										AssignTo:  &a.lvSaveBtn,
										Text:      "保存版本校验",
										OnClicked: func() { a.onLauncherVersionSave() },
									},
									decl.PushButton{
										AssignTo:  &a.lvRefresh,
										Text:      "刷新状态",
										OnClicked: func() { a.onLauncherVersionRefresh(false) },
									},
								},
							},
							decl.Label{AssignTo: &a.lvStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "管理员建号 / 改密 / 强制下线（不受注册限制影响，建号自动套用注册赠送规则）",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.Label{Text: "账号名 3~32 位（字母数字 _ - @ .），密码至少 8 位，救援代码为 6 位数字、可留空。"},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "账号"},
									decl.LineEdit{
										AssignTo: &a.adUserLE,
										MinSize:  decl.Size{Width: 150, Height: 0},
									},
									decl.Label{Text: "密码"},
									decl.LineEdit{
										AssignTo:     &a.adPassLE,
										MinSize:      decl.Size{Width: 140, Height: 0},
										PasswordMode: true,
									},
									decl.Label{Text: "救援代码"},
									decl.LineEdit{
										AssignTo:    &a.adRescueLE,
										MaxLength:   6,
										MinSize:     decl.Size{Width: 90, Height: 0},
										ToolTipText: "6 位数字，玩家用它在登录器「找回密码」时验证身份；留空表示不设置。",
									},
									decl.PushButton{
										AssignTo:  &a.adRegBtn,
										Text:      "创建账号",
										OnClicked: func() { a.onAdminRegister() },
									},
								},
							},
							decl.Composite{
								Layout: decl.HBox{},
								Children: []decl.Widget{
									decl.Label{Text: "改密账号"},
									decl.LineEdit{
										AssignTo: &a.adPwUserLE,
										MinSize:  decl.Size{Width: 130, Height: 0},
									},
									decl.Label{Text: "新密码"},
									decl.LineEdit{
										AssignTo:     &a.adPwNewLE,
										MinSize:      decl.Size{Width: 140, Height: 0},
										PasswordMode: true,
									},
									decl.PushButton{
										AssignTo:  &a.adPwBtn,
										Text:      "修改密码",
										OnClicked: func() { a.onAdminChangePassword() },
									},
									decl.Label{Text: "强踢账号"},
									decl.LineEdit{
										AssignTo: &a.adKoUserLE,
										MinSize:  decl.Size{Width: 130, Height: 0},
									},
									decl.PushButton{
										AssignTo:  &a.adKoBtn,
										Text:      "强制下线",
										OnClicked: func() { a.onAdminForceLogout() },
									},
								},
							},
							decl.Label{AssignTo: &a.adMsgL, Text: "建号 / 改密 / 强制下线需服务器认证服务在运行", TextColor: walk.RGB(120, 120, 120)},
						},
					},
					decl.GroupBox{
						Title:  "运行状态",
						Layout: decl.VBox{},
						Children: []decl.Widget{
							decl.TextEdit{
								AssignTo: &a.psStatus,
								ReadOnly: true,
								VScroll:  true,
								MinSize:  decl.Size{Width: 0, Height: 150},
							},
						},
					},
					decl.Label{Text: "注：需服务器端（付费版运维工具）已启用远程管理服务且认证服务在运行；泡点点券通过服务器 GM 回环发放；注册赠送在认证服务注册接口内入账。"},
				},
			},
		},
	}
}

// onAnnounceSend 校验内容与颜色后二次确认发送全服公告。
func (a *App) onAnnounceSend() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	content := strings.TrimSpace(a.announceEdit.Text())
	if content == "" {
		a.announceMsgL.SetText("公告内容不能为空")
		return
	}
	if utf8.RuneCountInString(content) > 100 {
		a.announceMsgL.SetText("公告内容超过 100 字")
		return
	}
	idx := a.announceColorCb.CurrentIndex()
	color := announceColorValues[0]
	if idx >= 0 && idx < len(announceColorValues) {
		color = announceColorValues[idx]
	}
	if !a.confirm("发送全服公告", "将向全服发送以下公告：\n\n"+content+"\n\n是否发送？") {
		return
	}
	a.announceBtn.SetEnabled(false)
	defer func() { a.sync(func() { a.announceBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		id, err := c.Announce(ctx, content, color)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("公告发送失败", err)
			}
			return
		}
		a.sync(func() {
			a.announceMsgL.SetText(fmt.Sprintf("已发送（ID %d）", id))
		})
	}()
}

// onPointsinkRefresh 拉取服务器泡点状态并刷新 UI。
func (a *App) onPointsinkRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	a.psSetBusy(true)
	go func() {
		defer a.sync(func() { a.psSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.PointsinkStatus(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询泡点状态失败", err)
				}
			}
			return
		}
		a.sync(func() {
			a.applyPointsinkToUI(st)
		})
	}()
}

// onPointsinkSave 校验输入并调用服务器应用泡点配置。
func (a *App) onPointsinkSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	iv, err := strconv.Atoi(strings.TrimSpace(a.psIntLE.Text()))
	if err != nil || iv < 10 || iv > 86400 {
		a.showInfo("参数错误", "发放间隔需为 10~86400 的整数秒")
		return
	}
	pts, err := strconv.Atoi(strings.TrimSpace(a.psPtsLE.Text()))
	if err != nil || pts < 1 || pts > 1000000 {
		a.showInfo("参数错误", "每轮点券需为 1~1000000 的整数")
		return
	}
	// 每日限额：留空 = 不限（0）。用户口径是"每个账号每天最多通过泡点获得 N 个当前币种"。
	limitText := strings.TrimSpace(a.psLimitLE.Text())
	limit := 0
	if limitText != "" {
		limit, err = strconv.Atoi(limitText)
		if err != nil || limit < 0 || limit > psMaxDailyLimit {
			a.showInfo("参数错误", fmt.Sprintf("账号每日限额需为 0（不限）~%d 的整数；留空等于不限", psMaxDailyLimit))
			return
		}
	}
	// 发放币种：下拉索引 → 取值（越界回落点券，见 psCurrencyValue）。
	currencyIdx := 0
	if a.psCurCb != nil {
		currencyIdx = a.psCurCb.CurrentIndex()
	}
	currency := psCurrencyValue(currencyIdx)
	currencyText := psCurrencyText(currency, "")
	enabled := a.psChk.Checked()
	limitLine := "账号每日限额：不限"
	if limit > 0 {
		limitLine = fmt.Sprintf("账号每日限额：%d %s（同一账号每天最多拿到这么多，最后一轮按剩余额度发）", limit, currencyText)
	}
	if !a.confirm("保存泡点配置", fmt.Sprintf("将向服务器应用以下泡点规则：\n\n启用：%v\n间隔：%d 秒\n每轮：%d %s\n%s\n\n立即生效并持久化，是否继续？", enabled, iv, pts, currencyText, limitLine)) {
		return
	}
	a.psSetBusy(true)
	go func() {
		defer a.sync(func() { a.psSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.PointsinkApply(ctx, enabled, iv, pts, limit, currency)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存泡点配置失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyPointsinkInputs(st)
			a.applyPointsinkStatusText(st)
			a.showInfo("保存成功", "泡点配置已应用到服务器并持久化。")
		})
	}()
}

// psSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) psSetBusy(b bool) {
	a.psBusy = b
	if a.psSaveBtn != nil {
		a.psSaveBtn.SetEnabled(!b)
	}
	if a.psRefresh != nil {
		a.psRefresh.SetEnabled(!b)
	}
}

// applyPointsinkToUI 将远端状态回填控件与状态框。
func (a *App) applyPointsinkToUI(st *PointsinkStatus) {
	if st == nil {
		return
	}
	a.applyPointsinkInputs(st)
	a.applyPointsinkStatusText(st)
}

// applyPointsinkInputs 仅回填输入控件（勾选/间隔/数量/币种/每日限额）。
func (a *App) applyPointsinkInputs(st *PointsinkStatus) {
	if st == nil {
		return
	}
	if a.psChk != nil {
		a.psChk.SetChecked(st.Enabled)
	}
	if a.psIntLE != nil {
		a.psIntLE.SetText(strconv.Itoa(st.IntervalSeconds))
	}
	if a.psPtsLE != nil {
		a.psPtsLE.SetText(strconv.Itoa(st.Points))
	}
	if a.psCurCb != nil {
		// 币种按服务端返回的值选中；老服务端不返回该字段（空串）→ 回落点券。
		a.psCurCb.SetCurrentIndex(psCurrencyIndex(st.Currency))
	}
	if a.psLimitLE != nil {
		a.psLimitLE.SetText(strconv.Itoa(st.DailyLimit))
	}
}

// applyPointsinkStatusText 仅刷新状态文本框。
func (a *App) applyPointsinkStatusText(st *PointsinkStatus) {
	if st == nil || a.psStatus == nil {
		return
	}
	var b strings.Builder
	unit := psCurrencyText(st.Currency, st.CurrencyLabel)
	if st.Enabled {
		fmt.Fprintf(&b, "启用：是（每 %s 自动发放 %d %s）\r\n", psHumanDuration(st.IntervalSeconds), st.Points, unit)
	} else {
		b.WriteString("启用：否\r\n")
	}
	if st.DailyLimit > 0 {
		fmt.Fprintf(&b, "账号每日限额：%d %s（同一账号每天最多拿到这么多，最后一轮按剩余额度发）\r\n", st.DailyLimit, unit)
	} else {
		b.WriteString("账号每日限额：不限\r\n")
	}
	runState := "服务端认证服务未运行（无法发放）"
	if st.Running {
		runState = "运行中"
	}
	fmt.Fprintf(&b, "认证服务数据源：%s\r\n", runState)
	fmt.Fprintf(&b, "累计发放：%d 账号 / %d %s；失败 %d 次\r\n", st.Awarded, st.GrantedPoints, unit, st.ErrorCount)
	if st.LastGrantAt > 0 {
		fmt.Fprintf(&b, "最近到账：%s\r\n", psHumanTime(st.LastGrantAt))
	} else {
		b.WriteString("最近到账：尚未发放\r\n")
	}
	if st.NextGrantAt > 0 {
		fmt.Fprintf(&b, "下一轮发放：%s\r\n", psHumanTime(st.NextGrantAt))
	} else {
		b.WriteString("下一轮发放：等待在线角色（发放起点从角色上线时刻开始计时）\r\n")
	}
	setEditText(a.psStatus, strings.TrimSuffix(b.String(), "\r\n"))
}

func psHumanDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	if sec%3600 == 0 {
		return fmt.Sprintf("%d 小时", sec/3600)
	}
	if sec%60 == 0 {
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
}

func psHumanTime(unixSec int64) string {
	return time.Unix(unixSec, 0).Format("2006-01-02 15:04:05")
}

// ---------------- 注册赠送（服务器选项页） ----------------

// rbCurrencyValues / rbCurrencyLabels 注册赠送币种下拉（索引与值一一对应）。
// 与服务端配置口径一致：cera=点券 / token_cera=代币券。
var (
	rbCurrencyValues = []string{"cera", "token_cera"}
	rbCurrencyLabels = []string{"点券", "代币券"}
)

// psCurrencyValues / psCurrencyLabels 泡点发放币种下拉。
//
// 比注册赠送多一个「欢乐代币券」：服务端钱包三种币种本来就都支持，
// 泡点走的是同一个 /local/dnf-gm/wallet/update 接口，没有理由只放两种。
// **不放金币**：金币是角色级数据、不是账号钱包，跟"账号级每日限额台账"的口径对不上。
var (
	psCurrencyValues = []string{"cera", "token_cera", "happy_token_cera"}
	psCurrencyLabels = []string{"点券", "代币券", "欢乐代币券"}
)

// psCurrencyValue 当前下拉选中的币种值（越界回落到点券）。
func psCurrencyValue(idx int) string {
	if idx < 0 || idx >= len(psCurrencyValues) {
		return psCurrencyValues[0]
	}
	return psCurrencyValues[idx]
}

// psCurrencyIndex 币种值 → 下拉索引（未知值回落到点券）。
func psCurrencyIndex(value string) int {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for idx, candidate := range psCurrencyValues {
		if candidate == normalized {
			return idx
		}
	}
	return 0
}

// psCurrencyText 币种值 → 中文（确认弹窗与状态文案用）。
// 优先用服务端给的 CurrencyLabel，拿不到时退回本地映射。
func psCurrencyText(value, serverLabel string) string {
	if strings.TrimSpace(serverLabel) != "" {
		return serverLabel
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "token_cera":
		return "代币券"
	case "happy_token_cera":
		return "欢乐代币券"
	default:
		return "点券"
	}
}

// rbCurrencyValue 当前下拉选中的币种值（越界回落到点券）。
func rbCurrencyValue(idx int) string {
	if idx < 0 || idx >= len(rbCurrencyValues) {
		return rbCurrencyValues[0]
	}
	return rbCurrencyValues[idx]
}

// rbCurrencyText 币种值 → 中文（状态/确认文案用）。
func rbCurrencyText(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "token_cera") {
		return "代币券"
	}
	return "点券"
}

// rbCurrencyIndex 币种值 → 下拉索引（未知值回落到点券）。
func rbCurrencyIndex(value string) int {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for idx, candidate := range rbCurrencyValues {
		if candidate == normalized {
			return idx
		}
	}
	return 0
}

// onRegisterBonusRefresh 拉取服务器注册赠送配置并刷新 UI。
func (a *App) onRegisterBonusRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	a.rbSetBusy(true)
	go func() {
		defer a.sync(func() { a.rbSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.RegisterBonusStatus(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询注册赠送配置失败", err)
				}
			}
			return
		}
		a.sync(func() { a.applyRegisterBonusToUI(st) })
	}()
}

// onRegisterBonusSave 校验输入并调用服务器应用注册赠送配置。
func (a *App) onRegisterBonusSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	pts, err := strconv.Atoi(strings.TrimSpace(a.rbPtsLE.Text()))
	if err != nil || pts < 1 || pts > 1000000 {
		a.showInfo("参数错误", "每新账号数量需为 1~1000000 的整数")
		return
	}
	enabled := a.rbChk.Checked()
	currencyIdx := 0
	if a.rbCurCb != nil {
		currencyIdx = a.rbCurCb.CurrentIndex()
	}
	currency := rbCurrencyValue(currencyIdx)
	if !a.confirm("保存注册赠送配置", fmt.Sprintf(
		"将向服务器应用以下注册赠送规则：\n\n启用：%v\n每新账号：%d %s\n\n注册成功立即入账并持久化，是否继续？",
		enabled, pts, rbCurrencyText(currency))) {
		return
	}
	a.rbSetBusy(true)
	go func() {
		defer a.sync(func() { a.rbSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.RegisterBonusApply(ctx, enabled, pts, currency)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存注册赠送配置失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyRegisterBonusToUI(st)
			a.showInfo("保存成功", "注册赠送配置已应用到服务器并持久化。")
		})
	}()
}

// rbSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) rbSetBusy(b bool) {
	a.rbBusy = b
	if a.rbSaveBtn != nil {
		a.rbSaveBtn.SetEnabled(!b)
	}
	if a.rbRefresh != nil {
		a.rbRefresh.SetEnabled(!b)
	}
}

// applyRegisterBonusToUI 将远端配置回填控件与状态标签。
func (a *App) applyRegisterBonusToUI(st *RegisterBonusStatus) {
	if st == nil {
		return
	}
	if a.rbChk != nil {
		a.rbChk.SetChecked(st.Enabled)
	}
	if a.rbPtsLE != nil {
		a.rbPtsLE.SetText(strconv.Itoa(st.Points))
	}
	if a.rbCurCb != nil {
		a.rbCurCb.SetCurrentIndex(rbCurrencyIndex(st.Currency))
	}
	if a.rbStateL == nil {
		return
	}
	currencyText := rbCurrencyText(st.Currency)
	if st.Enabled {
		a.rbStateL.SetText(fmt.Sprintf("状态：已启用，新注册账号赠送 %d %s（配置由服务器持久化，注册成功立即入账）", st.Points, currencyText))
		a.rbStateL.SetTextColor(walk.RGB(0, 140, 0))
	} else {
		a.rbStateL.SetText(fmt.Sprintf("状态：未启用（勾选并保存后，新注册账号将自动获得 %s）", currencyText))
		a.rbStateL.SetTextColor(walk.RGB(120, 120, 120))
	}
}
