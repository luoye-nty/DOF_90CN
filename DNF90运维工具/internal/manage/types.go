//go:build paid

// Package manage 提供 DNF90 后台远程管理服务（服务器端 P0+P1，仅赞助版编译）。
// 所有端点统一 /manage/v1/*，除 auth/login 外均需校验 X-Manage-Token；
// 响应统一 JSON 信封 {"ok":true,"data":...,"error":""}。
// 本包只做协议编排与鉴权，具体服务器启停 / 封禁 / CDK 等能力通过 Bridge
// 复用主程序既有实现（serverdir / ops / store / GUI 辅助函数），不在此重复业务逻辑。
package manage

import (
	"context"
	"time"

	"dnf90ops/internal/channelstore"
	"dnf90ops/internal/config"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"
)

// Bridge 由主程序（package main，仅 paid 构建）注入并实现。
type Bridge struct {
	// ServerRoot 返回当前服务端根目录（config.ServerRoot）。
	ServerRoot func() string
	// PublicIP 返回当前配置的公网 IP（可能为空，由调用方尝试探测补充）。
	PublicIP func() string

	// StartServer / StopServer 复用一键启动/停止核心链路（同步执行）。
	StartServer func(ctx context.Context) error
	StopServer  func(ctx context.Context) error

	// AuthRunning 返回认证服务（公网 HTTPS 账号 API）是否在运行。
	AuthRunning func() bool
	// Inspect 复用 serverdir.Inspect 探测服务端目录状态。
	Inspect func(root string) *serverdir.Inspection
	// Store 返回当前认证服务连接池；未运行时返回 nil。
	Store func() *store.Store
	// OnlineCount 返回当前在线账号数（与 GUI 轮询同源）。
	OnlineCount func() int

	// BanNow 复用 GUI 封禁语义：banType=account 时 username 用于解析 account_id，
	// banType=triple 时按 username 一键三封（账号+最近 IP+最近机器码）；
	// 封禁后按 GUI 同源逻辑踢下线并写 GM 事件。warnings 为可忽略的非致命提示。
	BanNow func(ctx context.Context, banType, banValue, username, reason string, days int) (BanNowResult, error)
	// BanKick 复用 GUI 踢下线逻辑（GM kick-sessions，带 X-Admin-Token）。
	BanKick func(ctx context.Context, banType, banValue string) (int, error)

	// RedeemCreate 复用 store.CreateRedeemCodeBatch 批量生成（返回全部新码）。
	RedeemCreate func(ctx context.Context, name string, count, maxRedeems int, contentRaw, rewardJSON, remark, createdBy string) ([]string, error)
	// RedeemExported 生成成功后回调（对齐 GUI 自动导出 TXT 行为；可为 nil）。
	RedeemExported func(ctx context.Context, name string, codes []string)
	// RetryRedeemMails 复用 GUI CDK 补发逻辑：对指定码下 mail_status=failed 的
	// 流水逐个 GM 发系统邮件并回写结果。返回成功/失败条数。
	RetryRedeemMails func(ctx context.Context, code string) (sent, failed int, err error)

	// PublishAnnounce 写库并向所有等待中的登录器长轮询广播全服公告；返回公告 id。
	PublishAnnounce func(ctx context.Context, content, color string) (int64, error)

	// PointsinkStatus 返回泡点运行状态快照。
	PointsinkStatus func() PointsinkStatus
	// PointsinkApply 校验并持久化泡点配置；enabled/intervalSeconds/points/dailyLimit/currency
	// 以本次入参为准。dailyLimit = 账号级每日限额（当前币种的数量），0 = 不限。
	// currency 接受 cera（点券，缺省）/ token_cera（代币券）/ happy_token_cera（欢乐代币券）；
	// 空值按点券处理（兼容老管理端）。
	PointsinkApply func(ctx context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string) (PointsinkStatus, error)

	// RegisterBonusStatus 返回注册赠送配置快照。
	RegisterBonusStatus func() RegisterBonusStatus
	// RegisterBonusApply 校验并持久化注册赠送配置；enabled/points/currency 以本次入参为准。
	// currency 只接受 cera（点券）或 token_cera（代币券）。
	RegisterBonusApply func(ctx context.Context, enabled bool, points int, currency string) (RegisterBonusStatus, error)

	// RegistrationClosedStatus 返回当前注册限制配置快照（关闭开关 + 每 IP 上限 + 是否需要注册码）。
	RegistrationClosedStatus func() RegistrationClosedStatus
	// RegistrationClosedApply 校验并持久化注册限制配置；closed / maxPerIP / codeRequired
	// 以本次入参为准（maxPerIP -1=不限制、0=不允许注册、1~1000=每个 IP 上限；
	// codeRequired=true 表示"注册开放也要填注册码"；越界返回错误且不落盘）。
	RegistrationClosedApply func(ctx context.Context, closed bool, maxPerIP int, codeRequired bool) (RegistrationClosedStatus, error)

	// RegistrationCodeCreate 批量生成注册码（邀请码短码），返回生成成功的码。
	RegistrationCodeCreate func(ctx context.Context, count int) ([]string, error)
	// RegistrationCodeList 列出注册码（state 为空=全部；unused/used/revoked）。
	RegistrationCodeList func(ctx context.Context, state string, limit int) ([]RegistrationCode, error)
	// RegistrationCodeRevoke 作废未使用的注册码，返回实际作废条数。
	RegistrationCodeRevoke func(ctx context.Context, codes []string) (int, error)

	// MaintenanceStatus 返回更新维护配置快照（开关 + 白名单账号）。
	MaintenanceStatus func() MaintenanceStatus
	// MaintenanceApply 校验并持久化更新维护配置；**成功后必须广播**给所有登录器
	// （开始维护 → 按钮熄灭 + 结束游戏进程；取消维护 → 按钮立刻亮起）。
	MaintenanceApply func(ctx context.Context, enabled bool, allowUsers []string) (MaintenanceStatus, error)

	// LauncherVersionStatus 返回登录器版本校验配置快照（开关 + 直链 + 只读的内置版本号）。
	LauncherVersionStatus func() LauncherVersionStatus
	// LauncherVersionApply 校验并持久化登录器版本校验配置（开关 + 直链一起写，失败整体回滚）。
	//
	// 允许 enabled=true 且 downloadURL 为空：那是需求确认过的分支（登录器弹「请联系管理员」），
	// 不在这里拒绝，由管理工具 UI 负责标红提醒。
	// 不需要广播：登录器在自检阶段与点登录时各主动查一次，没有推送需求。
	LauncherVersionApply func(ctx context.Context, enabled bool, downloadURL string) (LauncherVersionStatus, error)

	// MultiOpenStatus 返回双开（多开）开关配置快照。
	MultiOpenStatus func() MultiOpenStatus
	// MultiOpenApply 校验并持久化双开开关（以本次入参为准的整组写入，落盘失败整体回滚）。
	//
	// enabled=false 只表示"以后不许再开"：**不会**结束任何已在运行的副账号游戏进程
	// （与「更新维护」故意不同，维护会 KillAll）。不需要广播：登录器自检阶段主动查一次。
	MultiOpenApply func(ctx context.Context, enabled bool) (MultiOpenStatus, error)

	// WelfareStatus 返回在线福利状态快照（三档期配置 + 下次计划时刻）。
	WelfareStatus func() WelfareStatus
	// WelfareApply 校验并持久化在线福利配置（三档期一次写，失败整体回滚）。
	WelfareApply func(ctx context.Context, cfg config.WelfareConfig) (WelfareStatus, error)
	// WelfareTry 立即试发某个档期（不判定时间、不修改 last_runs）。
	WelfareTry func(ctx context.Context, kind string) (WelfareRunResult, error)
	// WelfareApplySlot 只应用一个档期的配置（kind=daily/weekly/monthly）。
	// 与整批 WelfareApply 的区别：不会因为别的档期"已启用但没奖励"而挡住本次保存。
	WelfareApplySlot func(ctx context.Context, kind string, slot config.WelfareSlot) (WelfareStatus, error)

	// AnnounceScheduleStatus 返回定时公告列表（含服务端算好的下次计划时刻）。
	AnnounceScheduleStatus func() []ScheduledAnnouncementStatus
	// AnnounceScheduleApply 校验并持久化定时公告列表（非法整批拒绝，不落盘）。
	AnnounceScheduleApply func(ctx context.Context, items []config.ScheduledAnnouncement) ([]ScheduledAnnouncementStatus, error)

	// NoticesList 返回登录器「服务器公告」栏的全部条目（含停用的，供管理工具维护）。
	NoticesList func(ctx context.Context) ([]store.LauncherNotice, error)
	// NoticesReplace 整表替换公告栏（表格里没有的条目等于删除），返回替换后的列表。
	NoticesReplace func(ctx context.Context, items []store.LauncherNotice) ([]store.LauncherNotice, error)

	// AccountNamesByID 批量把账号主键（dnf:<sha256(用户名)>，不可逆）翻成登录账号名。
	// 两处在用：① 记录查询回显（同一页记录里的 account_id 一起翻）；
	// ② POST /manage/v1/account/names —— 管理工具把界面上所有要显示的账号主键批量翻成真名。
	// 只返回能匹配到的账号；查不到的主键由调用方原样显示（翻译失败不能变成"看不见"）。
	AccountNamesByID func(ctx context.Context, accountIDs []string) (map[string]string, error)
	// ResolveAccountInput 把用户填的「账号主键 或 用户名」统一成账号主键
	// （记录查询的账号条件支持直接输入用户名；查不到时原样返回，不报错）。
	ResolveAccountInput func(ctx context.Context, value string) (string, error)

	// DLLList 返回"服务端指定 DLL"清单（含停用条目，供管理工具维护）。
	DLLList func(ctx context.Context) ([]store.LauncherDLL, error)
	// DLLReplace 整表替换清单（没带的条目等于删除），返回替换后的列表。
	DLLReplace func(ctx context.Context, items []store.LauncherDLL) ([]store.LauncherDLL, error)

	// ---- 频道管理（落盘编辑 channel_info.etc，重启 DNF90Server 后生效） ----
	// ChannelsStatus 读取当前 channel_info.etc 的频道目录与启用状态。
	ChannelsStatus func(ctx context.Context) (ChannelStatus, error)
	// ChannelsApply 把 channelIDs 批量设为 enabled（true=开启 / false=关闭）。
	// 只增删 [server] 段里的频道行，文件其余部分（BOM/注释/dungeon 段/缩进）逐字节保持不变；
	// 写入前自动备份为 channel_info.etc.bak_<时间戳>。返回本次真正发生变化的频道号与新状态。
	// 关闭被保护的频道（引导/默认城镇/最后一个普通频道）时返回错误，不落盘。
	ChannelsApply func(ctx context.Context, channelIDs []int, enabled bool) ([]int, ChannelStatus, error)
	// ChannelsReload 请求 DNF90Server 热重载频道表：重读 channel_info.etc、
	// 热替换内存里的频道目录与客户端频道脚本，并对账游戏端口监听。**不需要重启进程**。
	// 失败只代表"这次没能不重启就生效"，文件改动仍然有效，调用方应降级到重启路径。
	ChannelsReload func(ctx context.Context) (ChannelReloadStatus, error)

	// AdminRegister 管理员建号（绕过注册限制，mode 固定 open；自动按注册赠送规则登记点券）。返回账号 id。
	AdminRegister func(ctx context.Context, username, password, email, rescueCode string) (string, error)
	// AdminChangePassword 管理员改密（改密后强制作废该账号全部会话）。
	AdminChangePassword func(ctx context.Context, username, newPassword string) error
	// AdminForceLogout 强制指定账号下线（作废全部会话）。返回被踢会话数。
	AdminForceLogout func(ctx context.Context, username string) (int64, error)
	// AdminKickUsers 批量踢下线（管理工具在线列表「踢下选中 / 全部踢下」）：
	// 对每个账号做三件事 —— GM 立即断开在线游戏连接、写 GM 踢人事件（登录器据此结束
	// 本地游戏进程）、作废该账号全部登录会话（ticket）。整体成功返回汇总结果。
	AdminKickUsers func(ctx context.Context, usernames []string) (AdminKickResult, error)

	// ---- 账号管理（阶段1，设计 §5.3） ----
	// AcctGMCall 转发 DNF90Server 原生 GM 接口（actor 固定传 "manage"）；
	// 本闭包只做直连转发，不写 audit——account 端点审计由 handlers 统一落 audit/manage_account.jsonl。
	AcctGMCall func(ctx context.Context, method, path string, body, out any, actor string) (string, error)
	// AcctGMTCall 转发 DNF90Server 的 GM Tool 接口（/dnf/gm/*，信封 {success,data,error}）。
	// 与 AcctGMCall 是两条并行通道：旧原生接口 /local/dnf-gm/* 不提供背包与邮箱定向删除，
	// 而 /dnf/gm/* 不提供钱包与踢人。operation 用于服务端危险操作确认头（X-Admin-Confirm），
	// 必须传服务端注册时的操作 ID 常量（含 {character_id} 字面量），由主程序侧统一换算。
	AcctGMTCall func(ctx context.Context, method, path, operation string, body, out any, actor string) error
	// AcctDungeons 返回主程序嵌入的 dungeons_max_difficulty.json（AC-10 副本解锁表）。
	AcctDungeons func() (map[string]int, error)
	// AcctAudit 追加一条 /manage/v1/account/* 审计（detail 由 handlers 组装，含 ts/remote_ip/token hash 等）。
	AcctAudit func(ctx context.Context, actor, method, path string, detail map[string]any)
	// ---- 批量任务的目标来源（只读；未注入时退回 bridgeStore 直连同一份查询）----
	//
	// 为什么单列出来：批量任务（群发物品 / 批量资产邮件 / 批量清空邮箱）都要"全服遍历"，
	// 而遍历的数据源是认证服务连接池里的 store —— 把它明确成一个可注入的只读口，
	// 服务端契约测试才能在不起 MySQL 的情况下把"收集 → 逐个发放"整条链路跑通。
	// 三个闭包注入的都必须是与下列 store 方法**同一份**查询，不能各写一套口径。
	//
	// AcctOnlineAccounts 在线账号列表（scope=online-characters 的目标来源）。
	AcctOnlineAccounts func(ctx context.Context, limit int) ([]store.OnlineAccount, error)
	// AcctAccountsPage 按 account_id 升序分页取账号（scope=all-accounts / all-characters 的遍历来源）。
	AcctAccountsPage func(ctx context.Context, offset, limit int) ([]store.Account, error)
	// AcctServerFirstCharacter 全服最早创建的角色（scope=server-first-character，只读查库、不走回环口）。
	AcctServerFirstCharacter func(ctx context.Context) (store.ServerFirstCharacter, bool, error)

	// ---- 数据库备份（设计 P1 三端备份闭环：管理工具远程读写运维配置） ----
	// BackupStatus 返回自动备份配置与备份目录/运行态快照。
	BackupStatus func() BackupStatus
	// BackupApply 校验并持久化自动备份配置（enabled / intervalHours / keep 以本次入参为准；
	// 基准时刻重置为当前时间，与 GUI「保存设置」语义一致）。
	BackupApply func(ctx context.Context, enabled bool, intervalHours, keep int) (BackupStatus, error)
	// BackupList 返回备份目录下全部备份文件（新→旧）。
	BackupList func() ([]BackupFileEntry, error)
	// BackupNow 立即执行一次在线备份（不停止服务端，与 GUI「立即备份」按钮同链路）。
	BackupNow func(ctx context.Context) (BackupNowResult, error)
	// BackupRestore 执行覆盖还原：若服务端（认证/DNF90Server/MySQL）正在运行则先自动
	// 执行一键停止链路，随后幂等拉起便携 MySQL 并还原指定备份；logf 透传还原日志。
	// 还原属覆盖操作，管理端 UI 负责前置二次确认。
	BackupRestore func(ctx context.Context, zipName string, logf func(string)) (BackupRestoreResult, error)
}

// BackupStatus 自动备份运行快照（GET /manage/v1/backup/config 返回）。
type BackupStatus struct {
	Enabled       bool   `json:"enabled"`        // 是否启用自动备份
	IntervalHours int    `json:"interval_hours"` // 间隔小时（1~168）
	Keep          int    `json:"keep"`           // 成功后最多保留多少份备份（1~500）
	BaseAt        string `json:"base_at"`        // 基准时刻 RFC3339；空=未设置
	BackupDir     string `json:"backup_dir"`     // 服务器端备份目录（运维工具 exe 同目录 backups\）
	NextRunAt     string `json:"next_run_at"`    // 下次自动备份时刻 RFC3339；空=未启用或未设置基准
	// Overdue=true 表示"已经到点但还没执行"（工具忙或上次失败正在重试）——这时
	// NextRunAt 会显示成下一个未来时刻，只看它会误以为一切正常，所以单独标出来。
	Overdue       bool   `json:"overdue"`
	DueAt         string `json:"due_at,omitempty"` // 已到点但未执行的那个到点时刻 RFC3339
	ServerRunning bool   `json:"server_running"`   // 服务端进程/监听运行中（还原需先停服）
	AuthRunning   bool   `json:"auth_running"`     // 认证服务（公网账号 API）运行中
	MySQLMode     bool   `json:"mysql_mode"`       // 是否 MySQL 模式（备份仅 MySQL 模式提供）
	MySQLAlive    bool   `json:"mysql_alive"`      // 连接池存活（认证服务运行时探活）
}

// BackupFileEntry 备份目录单个文件条目。
type BackupFileEntry struct {
	Name    string `json:"name"`     // 文件名（还原入参仅接受文件名，防路径穿越）
	Size    int64  `json:"size"`     // 字节数
	ModTime string `json:"mod_time"` // 修改时刻 RFC3339
}

// BackupNowResult 立即备份执行结果（POST /manage/v1/backup/now 返回）。
type BackupNowResult struct {
	ZipName    string `json:"zip_name"`    // 新生成的备份文件名（backups 目录下）
	TableCount int    `json:"table_count"` // 备份的表数
	RowCount   int64  `json:"row_count"`   // 备份的总行数
}

// BackupRestoreResult 还原执行结果（POST /manage/v1/backup/restore 返回）。
type BackupRestoreResult struct {
	ZipName    string `json:"zip_name"`    // 已还原的备份文件名
	Stopped    bool   `json:"stopped"`     // 本次还原前是否执行了停服
	TableCount int    `json:"table_count"` // 还原的表数
	RowCount   int64  `json:"row_count"`   // 还原的总行数
}

// PointsinkStatus 泡点运行状态快照。
type PointsinkStatus struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	Points          int  `json:"points"`
	// DailyLimit 账号级每日限额（当前币种的数量）：同一账号一天最多通过泡点获得多少；0 = 不限。
	DailyLimit int `json:"daily_limit"`
	// Currency 发放币种：cera=点券 / token_cera=代币券 / happy_token_cera=欢乐代币券。
	Currency string `json:"currency"`
	// CurrencyLabel 币种中文名（界面直显用，避免每个客户端各写一份映射）。
	CurrencyLabel string `json:"currency_label"`
	Running       bool   `json:"running"`        // 认证服务运行中（在线数据源可用）
	Awarded       int64  `json:"awarded"`        // 本次进程累计成功发放账号次数
	GrantedPoints int64  `json:"granted_points"` // 本次进程累计发放数量（当前币种）
	ErrorCount    int64 `json:"error_count"`    // 发放失败次数
	LastGrantAt   int64 `json:"last_grant_at"`  // 最近一次成功发放 unix 秒；0=尚未发放
	NextGrantAt   int64 `json:"next_grant_at"`  // 下一轮发放的最近时刻 unix 秒；0=未在跑
}

// RegisterBonusStatus 注册赠送配置快照。
type RegisterBonusStatus struct {
	Enabled bool `json:"enabled"`
	Points  int  `json:"points"`
	// Currency 赠送币种：cera=点券（默认）/ token_cera=代币券（2026-09-30 起可选）。
	Currency string `json:"currency"`
}

// RegistrationClosedStatus 注册限制配置快照。
type RegistrationClosedStatus struct {
	Closed bool `json:"closed"`
	// MaxPerIP 每个来源 IP 允许注册的账号数上限：-1=不限制，0=不允许注册，1~1000=上限。
	// 与 Closed 是"两道闸"：Closed 直接关掉自助注册，MaxPerIP 只按 IP 计数拦截。
	MaxPerIP int `json:"max_per_ip"`
	// CodeRequired 是否需要注册码：true 时即使自助注册是开放的，也必须填一枚未使用的注册码。
	// 与 MaxPerIP 是并行的两道闸（都要满足）。登录器自检据此显示注册码输入框。
	CodeRequired bool `json:"code_required"`
}

// RegistrationCode 注册码（邀请码）行——管理工具「限制注册」页的列表项。
type RegistrationCode struct {
	Code      string `json:"code"`       // 库里原样存的码（短码带横线，形如 ABCD-EFGH）
	State     string `json:"state"`      // unused / used / revoked
	UsedBy    string `json:"used_by"`    // 使用者账号ID（used 时）
	UsedAt    string `json:"used_at"`    // 使用时间（RFC3339，空=未用）
	CreatedAt string `json:"created_at"` // 生成时间（RFC3339）
}

// MaintenanceStatus 更新维护配置快照。
type MaintenanceStatus struct {
	// Enabled 维护总开关：true 时禁止玩家登录与进游戏（注册照常开放）。
	Enabled bool `json:"enabled"`
	// AllowUsers 维护期间仍可登录/进游戏的账号名白名单（管理员自测用）；空 = 谁都不放行。
	AllowUsers []string `json:"allow_users"`
}

// LauncherVersionStatus 登录器版本校验配置快照。
//
// 对应管理工具「服务器选项 → 登录器版本校验」分组。字段与主程序 lvStat 一一对应。
// 注意 RequiredVersion 是**只读**的：它来自运维工具编译期内置常量（launcher_version.go），
// 不开放给管理工具修改 —— 避免手滑填错把全服玩家全部锁在更新界面。
type LauncherVersionStatus struct {
	// Enabled 校验总开关：true 时登录器版本必须与 RequiredVersion 完全一致。
	Enabled bool `json:"enabled"`
	// DownloadURL 登录器下载直链（压缩包）。**只给新登录器自动更新用**
	// （经 /launcher/v1/version-check 下发）；为空时新登录器无法自动更新，
	// 会提示「请联系管理员」。旧登录器不看这个字段 —— 它看到的永远是固定文案
	// 「登录器版本过旧，请联系管理员下载新版登录器」。
	DownloadURL string `json:"download_url"`
	// RequiredVersion 服务端内置要求的登录器版本号（只读，编译期常量）。
	RequiredVersion string `json:"required_version"`
}

// MultiOpenStatus 双开（多开）开关配置快照。
//
// 对应管理工具「服务器选项 → 双开开关」分组。字段与主程序 moStat 一一对应。
//
// 三态说明（与 internal/server/multi_open.go 同源）：配置里的 MultiOpenEnabled 是指针，
// nil = 后台**从未设置过** → 玩家侧按"允许双开"处理（改造前的现有行为）。
// 所以这里没有第三个字段：Enabled 报的**永远是玩家侧的生效值**，
// 管理工具的勾选框必须与玩家实际能不能双开一致，不能让运营看到"没勾"却还有人在双开。
type MultiOpenStatus struct {
	// Enabled 玩家侧生效值：true = 允许双开，false = 已关闭。
	Enabled bool `json:"enabled"`
	// Configured 后台是否**显式设置过**（false = 老配置 / 从未保存过，当前按默认允许）。
	// 只用于管理工具的状态行文案，不参与玩家侧判定。
	Configured bool `json:"configured"`
}

// WelfareRewardStatus 在线福利的一条奖励（视图）。
type WelfareRewardStatus struct {
	Kind           string `json:"kind"` // item / cera / black_diamond
	ItemID         uint32 `json:"item_id,omitempty"`
	Amount         uint32 `json:"amount,omitempty"`
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
	Label          string `json:"label,omitempty"`
}

// WelfareSlotStatus 在线福利的一个档期（视图：配置 + 计划时刻）。
type WelfareSlotStatus struct {
	Enabled bool   `json:"enabled"`
	Time    string `json:"time"`
	Weekday int    `json:"weekday,omitempty"`
	Day     int    `json:"day,omitempty"`
	Subject string `json:"subject,omitempty"`
	Notice  string `json:"notice,omitempty"`
	// Rewards 奖励列表（管理工具表格直接渲染）。
	Rewards []WelfareRewardStatus `json:"rewards"`
	// NextRun 下一个计划时刻（RFC3339，服务端算好，管理工具不重复实现日历逻辑）。
	NextRun string `json:"next_run,omitempty"`
	// LastRun 最近一次已处理的周期键（用于排查"为什么没发"）。
	LastRun string `json:"last_run,omitempty"`
}

// WelfareStatus 在线福利状态快照。
type WelfareStatus struct {
	Daily   WelfareSlotStatus `json:"daily"`
	Weekly  WelfareSlotStatus `json:"weekly"`
	Monthly WelfareSlotStatus `json:"monthly"`
	// LastRuns 各档期最近处理的周期键。
	LastRuns map[string]string `json:"last_runs"`
}

// WelfareRunResult 一次在线福利发放的结果摘要。
type WelfareRunResult struct {
	Accounts    int  `json:"accounts"`     // 在线账号数
	Characters  int  `json:"characters"`   // 在线角色数
	ItemMails   int  `json:"item_mails"`   // 发出的道具邮件数
	CeraGranted int  `json:"cera_granted"` // 点券发放笔数（账号级）
	BlackGrant  int  `json:"black_grant"`  // 黑钻发放笔数（账号级）
	Failed      int  `json:"failed"`
	NoticeSent  bool `json:"notice_sent"` // 是否发了全服公告
}

// ScheduledAnnouncementStatus 一条定时公告（视图：配置 + 服务端算好的下次计划时刻）。
// 内嵌 config.ScheduledAnnouncement：JSON 字段与配置结构逐字一致（管理工具直接渲染）。
type ScheduledAnnouncementStatus struct {
	config.ScheduledAnnouncement
	// NextRun 下一个计划时刻（RFC3339，停用/已过的一次性公告为空）。
	NextRun string `json:"next_run,omitempty"`
}

// AdminKickResult 批量踢下线结果（管理工具在线列表「踢下选中 / 全部踢下」）。
type AdminKickResult struct {
	// Requested 本次请求踢下线的账号数（去重后）。
	Requested int `json:"requested"`
	// Kicked 实际断开的在线游戏会话数合计（来自服务端 GM kick-sessions）。
	Kicked int `json:"kicked"`
	// Sessions 作废的登录会话（ticket）数合计 —— 作废后旧票无法再进游戏。
	Sessions int64 `json:"sessions"`
	// Failed 处理失败的账号名（账号不存在 / GM 回环不通等），成功的不在列表里。
	Failed []string `json:"failed"`
}

// ---- GM Tool（/dnf/gm/*）视图类型：背包 / 仓库 / 邮箱 ----
// JSON 与 go-server dnfbridge 的 GMInventoryItem / GMMailSummary 逐字对齐，
// 主程序侧（package main，account_gmt.go）另有一份同结构定义，两端必须同步修改。

// GMTCharacter GMCharacterSummary 的常用字段子集。
type GMTCharacter struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Slot        int    `json:"slot"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Online      bool   `json:"online"`
}

// GMTInventoryItem 单格物品（背包/仓库/账号金库/装备栏通用）。
type GMTInventoryItem struct {
	Container string            `json:"container"`
	Key       string            `json:"key"`
	ListType  int               `json:"list_type"`
	Slot      int               `json:"slot"`
	ItemID    int64             `json:"item_id"`
	Name      string            `json:"name"`
	Count     int64             `json:"count"`
	Bind      bool              `json:"bind"`
	ExpireAt  time.Time         `json:"expire_at,omitempty"`
	ReadOnly  bool              `json:"read_only"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// GMTInventorySnapshot 角色完整库存快照。
type GMTInventorySnapshot struct {
	Character GMTCharacter       `json:"character"`
	Items     []GMTInventoryItem `json:"items"`
}

// GMTMailAttachment 邮件附件摘要。
type GMTMailAttachment struct {
	ItemID   int64     `json:"item_id"`
	Count    int64     `json:"count"`
	Bind     bool      `json:"bind"`
	ExpireAt time.Time `json:"expire_at,omitempty"`
}

// GMTMail 单封邮件。
type GMTMail struct {
	MailID      string              `json:"mail_id"`
	SenderName  string              `json:"sender_name"`
	Title       string              `json:"title"`
	Body        string              `json:"body"`
	Gold        int64               `json:"gold"`
	Attachments []GMTMailAttachment `json:"attachments"`
	Read        bool                `json:"read"`
	Claimed     bool                `json:"claimed"`
	Claimable   bool                `json:"claimable"`
	Saved       bool                `json:"saved"`
	CreatedAt   time.Time           `json:"created_at,omitempty"`
	ExpireAt    time.Time           `json:"expire_at,omitempty"`
}

// GMTMailboxSnapshot 角色邮箱快照。
type GMTMailboxSnapshot struct {
	Character GMTCharacter `json:"character"`
	Mails     []GMTMail    `json:"mails"`
}

// GMTMailDeleteResult 定向删除结果。
type GMTMailDeleteResult struct {
	Deleted int                `json:"deleted"`
	Mailbox GMTMailboxSnapshot `json:"mailbox"`
}

// GMTMailboxClearResult 清空邮箱结果（/local/dnf-gm/mail/clear）。
type GMTMailboxClearResult struct {
	CharacterID        string `json:"character_id"`
	CharacterName      string `json:"character_name"`
	RemovedMails       int    `json:"removed_mails"`
	RemovedUnread      int    `json:"removed_unread"`
	RemovedClaimable   int    `json:"removed_claimable"`
	RemovedGold        int64  `json:"removed_gold"`
	RemovedAttachments int    `json:"removed_attachments"`
	Online             bool   `json:"online"`
	Refreshed          bool   `json:"refreshed"`
	RefreshError       string `json:"refresh_error,omitempty"`
}

// GMTRecordPage 记录查询结果页（服务端 GMTRecordPage 的原样透传）。
//
// Records 的每一项字段随类别不同（点券消费/对话/副本各自的字段集合都不一样），
// 所以这里保持 map：加字段不需要同步改三个工具，管理端按 key 取用即可。
type GMTRecordPage struct {
	Kind    string           `json:"kind"`
	Title   string           `json:"title"`
	Dir     string           `json:"dir"`
	Records []map[string]any `json:"records"`
	Count   int              `json:"count"`
	// Offset 本次返回的起始位置（= 请求里的 offset，由游戏服回显，见 go-server
	// dnfbridge/activity_hooks.go 的记录页结构）。
	//
	// ⚠ 这个字段必须原样回给管理工具：管理工具的"翻页能力探测"就是比对
	// "请求 offset" 与 "响应 offset"，少了它第二页恒等于 0 → 管理工具会判定
	// "对端不支持翻页"并只显示第一页（还会误导运营去重新部署服务端）。
	Offset    int  `json:"offset"`
	Scanned   int  `json:"scanned"`
	Limited   bool `json:"limited"`
	Retention int  `json:"retention_files"`
}

// RecordsQueryResult 记录查询响应：服务端原样透传的记录页 + 账号名映射。
//
// 为什么要带 account_names：记录里存的是账号主键（`dnf:<sha256(用户名)>`，不可逆），
// 而管理工具要显示"真实账号"。主键↔用户名的映射表只在运维工具侧的 MySQL
// （launcher_accounts），所以由这里批量翻译好一起回给管理工具（管理工具不连数据库）。
type RecordsQueryResult struct {
	GMTRecordPage
	// AccountNames account_id → 登录账号名。查不到的主键不会出现在这里，
	// 管理工具会退回显示原始主键（不能让"翻译不了"变成"记录看不见"）。
	AccountNames map[string]string `json:"account_names,omitempty"`
}

// GMTRecordKindInfo 一类记录的元信息。
type GMTRecordKindInfo struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// GMTRecordFileInfo 单个记录文件的信息。
type GMTRecordFileInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	Lines   int    `json:"lines,omitempty"`
}

// GMTRecordMeta 记录系统元信息：目录、类别清单、文件清单与写入健康度。
type GMTRecordMeta struct {
	Enabled bool                `json:"enabled"`
	Dir     string              `json:"dir"`
	Kinds   []GMTRecordKindInfo `json:"kinds"`
	Files   []GMTRecordFileInfo `json:"files"`
}

// ---- 频道管理（channel_info.etc 落盘编辑） ----
//
// 类型直接复用 internal/channelstore：主程序（频道管理页）与 manage（远程转发）
// 共用同一份解析/改写实现，避免两端各写一套解析器后行为漂移。

// ChannelStatus 频道状态快照（= channelstore.Status）。
type ChannelStatus = channelstore.Status

// ChannelEntry 单个频道（= channelstore.Entry）。
type ChannelEntry = channelstore.Entry

// ChannelReloadStatus 频道热重载结果（对齐 go-server dnfbridge.GMChannelReloadResult）。
//
// 热重载的生效范围要如实告诉使用者：新连入的登录/频道会话立刻拿到新表，
// 已经停在选频道界面的玩家需要重开列表或重新登录——客户端拿到目录后会本地缓存，
// 服务端不主动推未请求的目录包。
type ChannelReloadStatus struct {
	Path             string    `json:"path"`
	TotalChannels    int       `json:"total_channels"`
	AddedChannels    []int     `json:"added_channels"`
	RemovedChannels  []int     `json:"removed_channels"`
	GamePortsOpened  []int     `json:"game_ports_opened"`
	GamePortsClosed  []int     `json:"game_ports_closed"`
	OnlineCharacters int       `json:"online_characters"`
	Warnings         []string  `json:"warnings,omitempty"`
	ReloadedAt       time.Time `json:"reloaded_at"`
}

// BanNowResult 封禁执行摘要（POST /manage/v1/bans 返回）。
type BanNowResult struct {
	Type     string   `json:"type"`   // 实际封禁维度（triple 时为 account）
	Value    string   `json:"value"`  // 实际封禁目标
	Kicked   int      `json:"kicked"` // 在线会话匹配数
	Warnings []string `json:"warnings"`
}

// envelope 统一 JSON 响应信封。
type envelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data"`
	Error string `json:"error"`
}

// pairStatus 供 /status 与 GUI 展示配对码是否已设置（不含明文）。
type pairStatus struct {
	Enabled bool `json:"enabled"`
}

// serverStatus 服务端运行状态（serverdir.Inspection 精选字段，避免透出敏感配置）。
type serverStatus struct {
	Root          string `json:"root"`
	RootExists    bool   `json:"root_exists"`
	ServerExe     bool   `json:"server_exe_exists"`
	Instance      bool   `json:"instance_exists"`
	InstanceValid bool   `json:"instance_valid"`
	DatabaseMode  string `json:"database_mode"`
	ChannelPort   int    `json:"channel_port"`
	AdminPort     int    `json:"admin_port"`
	Running       bool   `json:"running"`
	Ready         bool   `json:"ready"`
	NeedsInit     bool   `json:"needs_init"`
	Message       string `json:"message"`
}

// authServiceStatus 认证服务状态。
type authServiceStatus struct {
	Running     bool   `json:"running"`
	PublicAddr  string `json:"public_addr"`
	InternalURL string `json:"internal_url"`
}

// mysqlStatus MySQL 状态摘要。
type mysqlStatus struct {
	Configured string `json:"configured"` // host:port/dbname
	Alive      bool   `json:"alive"`      // 认证服务运行即认为连接池可用
	Mode       string `json:"mode"`
}

// cdkSummary CDK 汇总。
type cdkSummary struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	Disabled    int `json:"disabled"`
	Redemptions int `json:"redemptions"`
	FailedMails int `json:"failed_mails"`
}

// licenseStatus 授权状态（manage 服务仅赞助版构建，固定 paid）。
// Kind/ExpireAt/CheckedAt 取自运维工具 config.json 的「最近一次校验结果」快照：
// manage 服务不反向调用授权服务器，故这几个字段是上次校验时的值（到期时间是绝对值，不失真）。
type licenseStatus struct {
	Edition string `json:"edition"` // paid
	CDKSet  bool   `json:"cdk_set"`

	Kind       string `json:"kind,omitempty"`       // forever / month / quarter / year；空=尚未用新版校验过
	ExpireAt   string `json:"expire_at,omitempty"`  // 到期时间(UTC RFC3339)；永久卡为空
	RemainDays int    `json:"remain_days"`          // 剩余天数：-1=永久，-2=未知（从未校验），>=0=剩余
	CheckedAt  string `json:"checked_at,omitempty"` // 最近一次成功校验的本机时间(RFC3339)
}

// certStatus 管理 HTTPS 证书状态。
type certStatus struct {
	CertPath string `json:"cert_path"`
	KeyPath  string `json:"key_path"`
	Exists   bool   `json:"exists"`
}

// statusResponse GET /manage/v1/status 聚合响应。
type statusResponse struct {
	Server  serverStatus      `json:"server"`
	Auth    authServiceStatus `json:"auth_service"`
	MySQL   mysqlStatus       `json:"mysql"`
	Online  int               `json:"online"`
	CDK     cdkSummary        `json:"cdk"`
	License licenseStatus     `json:"license"`
	Pairing pairStatus        `json:"pairing"`
	Cert    certStatus        `json:"cert"`
	Listen  string            `json:"listen_addr"`
}
