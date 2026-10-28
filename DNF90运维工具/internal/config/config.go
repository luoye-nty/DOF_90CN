// Package config 负责 DNF90运维工具的配置文件管理。
// 首次运行自动生成 config.json（含内部 token 随机初值），之后以文件为准。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MySQLConfig 对应设计文档 2.2：账号库建在 instance 配置的 MySQL 库内。
type MySQLConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
}

// PublicConfig 公网 HTTPS 账号 API（:18112）。
type PublicConfig struct {
	ListenAddr    string `json:"listen_addr"`    // ":18112"
	AdvertiseAddr string `json:"advertise_addr"` // 登录器可见的公网地址 IP:port，可留空
	TLSCert       string `json:"tls_cert"`       // PEM cert，不存在则自动生成自签证书
	TLSKey        string `json:"tls_key"`        // PEM key
}

// InternalConfig 回环内部验证 API（127.0.0.1:19091）。
type InternalConfig struct {
	ListenAddr       string `json:"listen_addr"`        // "127.0.0.1:19091"
	Token            string `json:"token"`              // 与 dnfbridge 共享的 X-Internal-Token
	TicketTTLMinutes int    `json:"ticket_ttl_minutes"` // 登录换票 ticket 有效期（分钟）
}

// DNFRunConfig dnfbridge.exe 一键启停参数。
type DNFRunConfig struct {
	Executable string   `json:"executable"`  // dnfbridge 可执行文件绝对路径
	WorkingDir string   `json:"working_dir"` // 工作目录（默认取可执行文件所在目录）
	ExtraArgs  []string `json:"extra_args"`  // 附加启动参数
	ExtraEnv   []string `json:"extra_env"`   // 附加 K=V 环境变量
	StatusPort int      `json:"status_port"` // game_up/运行状态探测端口，默认 7001
}

// LicenseConfig 赞助版在线授权（CDK+心跳）。
//
// Kind/ExpireAt 等为「最近一次校验结果」的本地快照，仅用于界面与远程状态页展示；
// 是否放行始终由授权服务器判定，客户端本地时间不参与决策。
type LicenseConfig struct {
	CDK string `json:"cdk"` // 授权 CDK；首次验证通过后落盘，之后启动自动验证

	Kind       string `json:"kind,omitempty"`        // forever / month / quarter / year
	ExpireAt   string `json:"expire_at,omitempty"`   // 到期时间(UTC RFC3339)；永久卡为空
	RemainDays int    `json:"remain_days,omitempty"` // 最近一次校验时的剩余天数；永久卡为 -1
	CheckedAt  string `json:"checked_at,omitempty"`  // 最近一次成功校验的本机时间(RFC3339，展示用)
}

// PointsinkConfig 泡点（在线账号每 interval 秒获得 points 个 Currency 币种）。
// 配置由管理端（家用电脑侧）通过 /manage/v1/pointsink 写入后落盘；
// 本工具启动时若已启用则自动后台运行，无需人工介入。
type PointsinkConfig struct {
	Enabled         bool `json:"enabled"`          // 是否启用泡点发放
	IntervalSeconds int  `json:"interval_seconds"` // 发放间隔（秒），下限 10
	Points          int  `json:"points"`           // 每轮每账号发放数量，上限 1000000
	// DailyLimit 账号级每日限额（当前币种的数量）：同一账号一天（自然日）通过泡点最多能拿这么多。
	// 0 = 不限额。上限与"每轮数量"同量级（1 亿），避免写进配置的数值把客户端整型撑爆。
	// 达到限额的那一刻起该账号当天不再发放；最后一轮按"剩余额度"发（例如限额 1000、
	// 每轮 300、已发 900 → 这一轮只发 100），保证"最多不超过限额"这条口径成立。
	DailyLimit int `json:"daily_limit"`
	// Currency 发放币种（2026-10-05 起可选）：
	//	"cera"            = 点券（默认，与旧配置兼容）
	//	"token_cera"      = 代币券（绑定类点券，拍卖/寄售那条线发的就是它）
	//	"happy_token_cera"= 欢乐代币券
	// 空值/非法值一律按点券处理（见 rbNormalizeCurrency），所以老配置不用改。
	//
	// 注意：限额台账 launcher_pointsink_daily **不区分币种**（沿用注册赠送那套"全局单币种开关"
	// 模型）。所以当天中途改币种时，今天已经累计的额度会带到新币种上。
	Currency string `json:"currency,omitempty"`
}

// RegisterBonusConfig 注册赠送：新账号注册成功后立即入账 points 个 Currency 币种。
// 配置由管理端（家用电脑侧）通过 /manage/v1/register-bonus 写入后落盘；
// 默认关闭，数值上限与泡点对齐（1~1000000）。
type RegisterBonusConfig struct {
	Enabled bool `json:"enabled"` // 是否启用注册赠送
	Points  int  `json:"points"`  // 每新账号赠送数量，上限 1000000
	// Currency 赠送币种（2026-09-30 起可选）：
	//	"cera"       = 点券（默认，与旧配置兼容）
	//	"token_cera" = 代币券（绑定类点券，拍卖/寄售那条线发的就是它）
	// 空值/非法值一律按点券处理（见 rbNormalizeCurrency），所以老配置不用改。
	Currency string `json:"currency,omitempty"`
}

// WelfareReward 在线福利的一条奖励。
type WelfareReward struct {
	// Kind：item=道具 / cera=点券 / black_diamond=黑钻天数。
	Kind           string `json:"kind"`
	ItemID         uint32 `json:"item_id,omitempty"`
	Amount         uint32 `json:"amount,omitempty"` // 道具数量 / 点券数量 / 黑钻天数
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
	// Label 展示名（管理工具里填的物品名，便于列表一眼看懂；发放不依赖它）。
	Label string `json:"label,omitempty"`
}

// WelfareSlot 在线福利的一个档期（每日 / 每周 / 每月）。
type WelfareSlot struct {
	Enabled bool            `json:"enabled"`
	Time    string          `json:"time"`              // "HH:MM"（本地时间）
	Weekday int             `json:"weekday,omitempty"` // 1=周一 … 7=周日（每周档）
	Day     int             `json:"day,omitempty"`     // 1..28（每月档；不超过 28，避免"每月 31 号"这种不存在的日期）
	Subject string          `json:"subject,omitempty"` // 邮件标题（留空用「在线福利」）
	Notice  string          `json:"notice,omitempty"`  // 全服公告文案（留空=不发公告）
	Rewards []WelfareReward `json:"rewards,omitempty"`
}

// ScheduledAnnouncement 定时公告（管理工具「服务器选项 → 定时公告」维护，多条排队）。
type ScheduledAnnouncement struct {
	// ID 服务端分配的稳定标识（管理端按它增删改；空=新建时由服务端生成）。
	ID string `json:"id"`
	// Content 公告内容（≤100 字，与立即发送同一条链路）。
	Content string `json:"content"`
	// Color 颜色（0xAABBGGRR，与立即发送的取值一致；空用默认蓝色）。
	Color string `json:"color,omitempty"`
	// Enabled 停用后保留配置但不发送。
	Enabled bool `json:"enabled"`
	// Mode：once=指定日期时间发一次；daily=每天；weekly=每周某天。
	Mode    string `json:"mode"`
	Time    string `json:"time"`              // "HH:MM"
	Date    string `json:"date,omitempty"`    // once：YYYY-MM-DD
	Weekday int    `json:"weekday,omitempty"` // weekly：1=周一 … 7=周日
	// LastRun 最近一次已处理的周期键（once=日期本身 / daily=YYYY-MM-DD / weekly=YYYY-Www）。
	// 落盘保存：运维工具重启后不能把今天已经发过的公告再发一遍。
	LastRun string `json:"last_run,omitempty"`
	// CreatedAt 便于列表按加入顺序展示。
	CreatedAt string `json:"created_at,omitempty"`
}

// WelfareConfig 在线福利：三套独立奖励定时向在线玩家发放 + 自定义公告文案。
//
// 发放口径（已与需求确认）：
//   - 道具：按**在线角色**各发一封（同一账号双开都收到）；
//   - 点券/黑钻：账号级权益，按**账号去重**只发一次；
//   - 同一档期只发一次；错过窗口（工具没开/服务器维护）**不补发**，只记日志。
type WelfareConfig struct {
	Daily   WelfareSlot `json:"daily"`
	Weekly  WelfareSlot `json:"weekly"`
	Monthly WelfareSlot `json:"monthly"`
	// LastRuns 各档期最近一次"已处理"的周期键（daily=2026-09-24 / weekly=2026-W39 / monthly=2026-09）。
	// 落盘保存：运维工具重启后不能把当天已经发过的福利再发一遍。
	LastRuns map[string]string `json:"last_runs,omitempty"`
}

// AutoBackupConfig 自动备份：从「配置保存时刻」起定时，每隔 IntervalHours 小时备份一次，无视跨天。
// 配置可在运维工具本地「自动备份」页设置；赞助版可通过管理工具远程读写。
type AutoBackupConfig struct {
	Enabled       bool   `json:"enabled"`           // 是否启用自动备份
	IntervalHours int    `json:"interval_hours"`    // 备份间隔（小时），默认 1，上限 168
	BaseAt        string `json:"base_at,omitempty"` // 基准时刻 RFC3339：保存/应用配置时的当前时刻；后续备份 = BaseAt + N*k
	// Keep 备份目录最多保留多少个 zip（成功备份后按时间删掉更旧的）。
	//
	// 为什么必须有这个上限：日志有轮转、备份没有 —— 早先每次备份只新增文件、从不清理，
	// 定时备份（默认 1 小时一次）会无限占盘，直到把磁盘写满。
	// 0/负数/超上限都按 AutoBackupKeepDefault 处理（上限 AutoBackupKeepMax）。
	Keep int `json:"keep,omitempty"`
}

const (
	// AutoBackupKeepDefault 未配置时的默认保留份数。
	AutoBackupKeepDefault = 20
	// AutoBackupKeepMax 保留份数上限（防止填个 100000 等于没限制）。
	AutoBackupKeepMax = 500
	// AutoBackupIntervalMax 备份间隔上限（小时）。
	AutoBackupIntervalMax = 168
)

// NormalizeAutoBackup 把自动备份配置收敛到合法区间（读写两侧都调用，避免各处各写一套默认值）。
func NormalizeAutoBackup(ab *AutoBackupConfig) {
	if ab == nil {
		return
	}
	if ab.IntervalHours < 1 {
		ab.IntervalHours = 1
	}
	if ab.IntervalHours > AutoBackupIntervalMax {
		ab.IntervalHours = AutoBackupIntervalMax
	}
	if ab.Keep <= 0 {
		ab.Keep = AutoBackupKeepDefault
	}
	if ab.Keep > AutoBackupKeepMax {
		ab.Keep = AutoBackupKeepMax
	}
}

// LogFileConfig 落盘日志（运维工具全量日志 + 敏感信息脱敏）。
// GUI 日志框只保留最近若干行、关掉工具就没了；闪退排查必须依赖落盘文件，
// 因此默认开启，并且写盘前做脱敏（adminToken / 数据库口令 / 配对码 / ticket 等）。
type LogFileConfig struct {
	Enabled   bool   `json:"enabled"`     // 默认 true
	Dir       string `json:"dir"`         // 相对运维工具配置目录；默认 logs
	MaxFileMB int    `json:"max_file_mb"` // 单文件上限 MB，默认 16，1~1024
	KeepFiles int    `json:"keep_files"`  // 最多保留最近的日志文件数，默认 20，1~500
	// Redact 写盘前脱敏。默认 true；只有在你明确保证日志不外发时才建议关掉。
	Redact bool `json:"redact"`
}

// MemoryCleanConfig 定时清理内存（回收目标进程的工作集）。
// 这里做的是 Windows EmptyWorkingSet：把闲置物理页交还系统，属"无感回收"，
// 不重启进程、不断线；代价是它不会释放 Go 堆里正在使用的内存。
// 如果内存仍持续上涨，那是真实泄漏，需要重启服务端而不是回收工作集。
type MemoryCleanConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"` // 默认 30，1~1440
	Server          bool `json:"server"`           // 回收 DNF90Server.exe
	MySQL           bool `json:"mysql"`            // 回收便携 mysqld.exe
	Self            bool `json:"self"`             // 回收运维工具自身
	LogEveryRun     bool `json:"log_every_run"`    // 每次执行都打日志（默认只在确有回收量时打）
}

// ManageConfig 远程管理服务（服务器端 P0）：HTTPS :18113 + 配对码认证。
type ManageConfig struct {
	Enabled          bool   `json:"enabled"`            // 是否启用远程管理（GUI 远程管理页开关，默认关）
	ListenAddr       string `json:"listen_addr"`        // 管理 API 监听地址，默认 ":18113"
	TLSCert          string `json:"tls_cert"`           // 管理 HTTPS 证书，不存在则自动生成自签证书（SAN 含公网 IP）
	TLSKey           string `json:"tls_key"`            // 管理 HTTPS 私钥
	PairingCodeSalt  string `json:"pairing_code_salt"`  // 配对码盐（随机 hex，开启远程管理时生成）
	PairingCodeHash  string `json:"pairing_code_hash"`  // 配对码 sha256(salt+code) hex；非空表示已设置配对码
	PairingCodePlain string `json:"pairing_code_plain"` // 配对码明文（仅 GUI 远程管理页本地展示用；鉴权只用 hash）
	TokenTTLMinutes  int    `json:"token_ttl_minutes"`  // 管理 token 有效期（分钟），默认 120
	MaxFailAttempts  int    `json:"max_fail_attempts"`  // 配对码失败锁定阈值（按来源 IP），默认 5
	LockoutMinutes   int    `json:"lockout_minutes"`    // 失败锁定时长（分钟），默认 10
}

// Config 运维工具整体配置。
type Config struct {
	ServerRoot         string      `json:"server_root"`   // 服务端根目录（含 runtime / START.bat 的目录）
	DatabaseMode       string      `json:"database_mode"` // sqlite | mysql（portable MySQL）
	PublicIP           string      `json:"public_ip"`     // 公网 IP（DNFBRIDGE_SERVER_IP 注入值）
	MySQL              MySQLConfig `json:"mysql"`
	RegistrationMode   string      `json:"registration_mode"`   // open / invite
	RegistrationClosed bool        `json:"registration_closed"` // true=关闭玩家自助注册（仅管理员可建号）
	// RegistrationCodeRequired 注册必须填注册码（邀请码）——**独立于 RegistrationMode**：
	//
	//	registration_mode=open   + CodeRequired=false → 老行为：填账号密码就能注册；
	//	registration_mode=open   + CodeRequired=true  → 仍然"注册开放"，但必须填对一枚
	//	                                                未使用的注册码（launcher_invites 一次性）；
	//	registration_mode=invite                     → 一直都要码（CodeRequired 不参与）。
	//
	// 注册码由管理工具「限制注册」页生成/列表/作废（短码，形如 ABCD-EFGH），
	// 玩家在登录器注册界面填写；登录器自检阶段通过 /launcher/v1/registration-info
	// 得知要不要显示注册码输入框。
	RegistrationCodeRequired bool `json:"registration_code_required,omitempty"`
	// MaintenanceMode 更新维护：true 时禁止玩家登录与进游戏（注册照常开放）。
	//
	//   - 服务端硬拦（老登录器也绕不过）：/launcher/v1/login 与内部验票
	//     /internal/verify-game-ticket 在维护期间一律拒绝（白名单账号除外）；
	//   - 登录器自检 / 长轮询拿到维护状态后把登录按钮置灰并显示"正在维护"，
	//     若玩家正在游戏中则立刻结束游戏进程；
	//   - 注册 / 登录器其余接口（公告、GM 事件、心跳）不受影响，登录器必须活着才能
	//     实时收到"维护结束"的通知。
	//
	// 落盘保存：运维工具重启后维护状态保持，避免"重启一次维护就悄悄关了"。
	MaintenanceMode bool `json:"maintenance_mode,omitempty"`
	// MaintenanceAllowUsers 维护期间仍可登录/进游戏的账号名白名单（管理员自测用）。
	// 按用户名比对（NormalizeUsername 归一、不分大小写）；空 = 谁都不放行。
	MaintenanceAllowUsers []string `json:"maintenance_allow_users,omitempty"`

	// MultiOpenEnabled 双开（多开）总开关（管理工具「服务器选项 → 双开开关」）。
	//
	// 语义（三端联动，见 multi_open.go / internal/server/multi_open.go）：
	//   - &true ：登录器允许玩家勾选「双开」并拉起副账号（线上实际最大 2 开）。
	//   - &false：登录器把「双开」勾选框置灰、副账号登录按钮置灰并改文案「双开已关闭」，
	//     同时在按钮回调里硬拦一道 —— **只是"以后不许再开"**。
	//   - nil   ：后台**从未设置过**（老 config.json 升级上来的默认值）→ 对外接口
	//     **不下发** enabled 字段 → 登录器保持"允许双开"，也就是改造前的现有行为。
	//
	// ⚠ 关闭时**绝不**踢掉已经在跑的副账号进程：与「更新维护」最大的区别就在这里
	//   （维护会 GameProcessManager.KillAll()，双开开关不碰任何进程）。
	//
	// ⚠⚠ **为什么必须是指针，而不是 bool + omitempty**（最容易出事的一处）：
	//
	//	需求同时要求两件事：①「双开现在默认打开」——升级运维工具/登录器不能让全服
	//	双开瞬间消失；②「只有明确收到 enabled:false 才关闭」。
	//	如果这里用 `bool` + `omitempty`，缺席与显式 false 在 json 里**完全无法区分**：
	//	  - 把它当 false → 任何老 config.json 一升级就变成"关闭双开"，全服玩家立刻不能双开；
	//	  - 把它当 true  → 运营关掉开关后 omitempty 不写这个键，重启又变回"开"，根本关不住。
	//	用指针就能区分"没配过"（nil）与"显式配了 false"（&false）—— 与同一个文件里
	//	RegistrationMaxPerIP 的处理完全同源（那里是"0 的语义翻转后必须用 *int"）。
	//	`omitempty` 对指针只省略 nil，所以**显式关闭能被落盘、重启后保持**。
	//
	//	登录器侧对应三态 `bool?`（null = 未知 = 允许），见 登录器
	//	Services/RemoteAuthService.cs → MultiOpenEnabled。
	MultiOpenEnabled *bool `json:"multi_open_enabled,omitempty"`

	// LauncherVersionCheck 登录器版本校验总开关（管理工具「服务器选项 → 登录器版本校验」）。
	//
	// 语义（三端联动，见 launcher_version.go / internal/server/launcher_version.go）：
	//   - true：登录器（外网版 · 远程模式）在**启动自检阶段**与**点登录时**各上报一次自己的
	//     版本号，与本端内置的 builtinLauncherVersion（launcher_version.go）**完全一致**才放行；
	//     不一致 → 登录器停在更新窗口下载新版本 → 删掉原登录器 → 自动拉起新登录器。
	//     **更新完成前禁止进入登录界面**（"不更新禁止进入"，不是只置灰按钮）。
	//   - false：完全不校验，行为与改动前一致。
	//
	// 为什么服务端要参与：老版本登录器根本不知道这回事，只靠客户端自觉挡不住；
	// 版本号只在服务端内置一份，运营改一次全服生效，不用重新发登录器。
	//
	// **老登录器怎么办**（这个开关能不能真正生效的关键）：
	// 加这个功能之前发出去的登录器永远不会来问版本 —— 只靠那个公开接口，开关对老玩家
	// 完全无效，而他们恰恰是唯一需要被强制更新的那批人。所以整个 /launcher/v1/* 外面
	// 罩了一层版本闸（internal/server/launcher_version.go 的 launcherVersionMiddleware）：
	// **请求头缺 X-Client-Launcher-Version 的一律按"老登录器"拒绝**，
	// 注册、登录、在线探测、公告、泡点、PVF、CDK、心跳全部 403，
	// 只剩一句中文提示让玩家去下载最新版（老登录器会把这句话直接显示给玩家）。
	// 唯一放行的是 /launcher/v1/version-check —— 那是"我该是什么版本"的唯一入口，
	// 拦掉它会让版本不对的登录器问不到该更新到哪一版，直接死局。
	//
	// ⚠ 运营注意：这个开关一开就是**硬切换** —— 还没换新登录器的玩家立刻什么都做不了。
	//   正确顺序是先把新版登录器发出去并通知到位，等大部分玩家换完再开。
	//
	// 落盘保存：运维工具重启后开关与直链保持，避免"重启一次校验就悄悄关了"。
	LauncherVersionCheck bool `json:"launcher_version_check,omitempty"`
	// LauncherDownloadURL 登录器下载直链（压缩包 zip/7z/rar；包里只放登录器一个 exe）。
	//
	// 用途只有一个：**给新登录器自动更新用**（经 /launcher/v1/version-check 下发的
	// download_url，登录器拿它下载 → 解压 → 替换 → 重启）。
	//
	// 分支：
	//   - 勾选校验 + 直链非空 + 版本不符（**新**登录器）→ 登录器按此直链下载并强制更新；
	//   - 勾选校验 + 直链**为空** + 版本不符（**新**登录器）→ 登录器弹「请联系管理员」并禁止进入；
	//   - 未勾选校验 → 登录器完全不校验（直链填了也不生效）。
	//
	// ⚠ **旧**登录器不看这个字段：它看到的是固定文案「登录器版本过旧，请联系管理员下载新版登录器」
	//   （见 internal/server launcherVersionBlockText）。2026-09-28 需求确认：无论有没有配直链，
	//   老登录器都只看到那一句 —— 因为老登录器的提示是 320px 宽、3 秒消失的 Toast，
	//   一条百分号编码的云盘直链在里面会折成 6 行，读不完也复制不了，反而把提示本身挤没。
	LauncherDownloadURL string `json:"launcher_download_url,omitempty"`
	// RegistrationMaxPerIP 同一个来源 IP 最多允许注册几个账号。
	//
	// 语义（改成"0 = 不允许注册"之后必须用指针，见下）：
	//   - nil：配置里没有这个字段 → **不按 IP 限制**（老配置升级上来的默认值）；
	//   - 0：不允许注册（每个来源 IP 最多 0 个，等于全拒）；
	//   - 1~1000：每个来源 IP 最多这么多个；越界按 1000（实际不限制）处理。
	//
	// 为什么必须是指针：0 的语义从"不限制"翻转成"不允许注册"后，如果继续用 int，
	// 所有老 config.json（根本没这个字段 → 解出 0）都会在升级后**突然把注册全关死**。
	// 用指针就能区分"没配过"（nil，不限制）与"显式配了 0"（全拒）。
	// 与 RegistrationClosed 是两道闸：Closed 直接关掉自助注册；MaxPerIP 按 IP 计数逐个拦。
	RegistrationMaxPerIP *int             `json:"registration_max_per_ip,omitempty"`
	Public               PublicConfig     `json:"public"`
	Internal             InternalConfig   `json:"internal"`
	DNFBridge            DNFRunConfig     `json:"dnfbridge"`
	License              LicenseConfig    `json:"license"`
	Manage               *ManageConfig    `json:"manage"`
	Pointsink            *PointsinkConfig `json:"pointsink"`
	// Welfare 在线福利：三套独立奖励（每日/每周/每月）定时向在线玩家发放 + 自定义公告文案。
	// 配置结构见本文件下方的 WelfareConfig；调度与发放逻辑在主程序 welfare.go。
	Welfare *WelfareConfig `json:"welfare"`
	// AnnounceSchedule 定时公告列表（一次性 / 每天 / 每周，多条排队可删除停用）。
	AnnounceSchedule []ScheduledAnnouncement `json:"announce_schedule,omitempty"`
	RegisterBonus    *RegisterBonusConfig    `json:"register_bonus"`
	AutoBackup       *AutoBackupConfig       `json:"autobackup"`
	LogFile          *LogFileConfig          `json:"log_file"`
	MemoryClean      *MemoryCleanConfig      `json:"memory_clean"`
	LogMaxLines      int                     `json:"log_max_lines"`            // GUI 日志最大保留行数
	AcctLocalPVF     string                  `json:"acct_local_pvf,omitempty"` // 账号管理页本地搜索用 PVF 绝对路径（空=每次探测 instance.json/手动选择）

	path string `json:"-"`
}

// DefaultConfig 返回默认配置；path 字段由 Load 设置。
func DefaultConfig() *Config {
	return &Config{
		ServerRoot:   "",
		DatabaseMode: "sqlite",
		PublicIP:     "",
		MySQL: MySQLConfig{
			Host:   "127.0.0.1",
			Port:   13306,
			User:   "root",
			DBName: "dnf_local",
		},
		RegistrationMode:   "open",
		RegistrationClosed: false,
		Public: PublicConfig{
			ListenAddr: ":18112",
			TLSCert:    "tls/launcher.crt",
			TLSKey:     "tls/launcher.key",
		},
		Internal: InternalConfig{
			ListenAddr:       "127.0.0.1:19091",
			TicketTTLMinutes: 30,
		},
		DNFBridge: DNFRunConfig{
			StatusPort: 7001,
		},
		Manage: &ManageConfig{
			ListenAddr:      ":18113",
			TLSCert:         "tls/manage.crt",
			TLSKey:          "tls/manage.key",
			TokenTTLMinutes: 120,
			MaxFailAttempts: 5,
			LockoutMinutes:  10,
		},
		Pointsink: &PointsinkConfig{
			IntervalSeconds: 300,
			Points:          100,
			Currency:        "cera",
		},
		RegisterBonus: &RegisterBonusConfig{
			Points:   100,
			Currency: "cera",
		},
		AutoBackup: &AutoBackupConfig{
			IntervalHours: 1,
			Keep:          AutoBackupKeepDefault,
		},
		// 落盘日志默认开启：闪退排查的唯一凭据，默认脱敏。
		LogFile: &LogFileConfig{
			Enabled:   true,
			Dir:       "logs",
			MaxFileMB: 16,
			KeepFiles: 20,
			Redact:    true,
		},
		// 内存回收默认关闭（默认不改动服务端进程状态），勾选启用后按 30 分钟一次收服务端工作集。
		MemoryClean: &MemoryCleanConfig{
			Enabled:         false,
			IntervalMinutes: 30,
			Server:          true,
			MySQL:           false,
			Self:            true,
		},
		LogMaxLines: 2000,
	}
}

// Load 读取配置；文件不存在时按默认值创建并落盘，首次生成随机内部 token。
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	cfg.path = path

	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// 首次运行：生成随机内部 token。
		token, terr := RandomHex(32)
		if terr != nil {
			return nil, terr
		}
		cfg.Internal.Token = token
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	// manage 段缺失（旧配置）：写默认值并落盘；显式置 null 时同样补默认。
	var manageProbe struct {
		Manage json.RawMessage `json:"manage"`
	}
	_ = json.Unmarshal(raw, &manageProbe)
	if cfg.Manage == nil {
		cfg.Manage = DefaultConfig().Manage
		if len(manageProbe.Manage) == 0 {
			_ = cfg.Save()
		}
	}
	// pointsink 段缺失（旧配置）：写默认值并落盘；显式置 null 时同样补默认。
	var pointsinkProbe struct {
		Pointsink json.RawMessage `json:"pointsink"`
	}
	_ = json.Unmarshal(raw, &pointsinkProbe)
	if cfg.Pointsink == nil {
		cfg.Pointsink = DefaultConfig().Pointsink
		if len(pointsinkProbe.Pointsink) == 0 {
			_ = cfg.Save()
		}
	}
	// register_bonus 段缺失（旧配置）：写默认值并落盘；显式置 null 时同样补默认。
	var registerBonusProbe struct {
		RegisterBonus json.RawMessage `json:"register_bonus"`
	}
	_ = json.Unmarshal(raw, &registerBonusProbe)
	if cfg.RegisterBonus == nil {
		cfg.RegisterBonus = DefaultConfig().RegisterBonus
		if len(registerBonusProbe.RegisterBonus) == 0 {
			_ = cfg.Save()
		}
	}
	// 老配置的 register_bonus 段没有 currency（或填了非法值）：收敛到点券，
	// 与"注册赠送本来就是送点券"的旧行为一致，不写盘也不改语义。
	//
	// 这里刻意**不**复用 rbNormalizeCurrency：那个函数认三种币种，而注册赠送只放行
	// 点券/代币券两种（服主 2026-09-30 的要求）。分开写才能保证以后扩展钱包币种时
	// 不会顺手把注册赠送的可选集合也撑大。
	if cfg.RegisterBonus != nil {
		switch strings.ToLower(strings.TrimSpace(cfg.RegisterBonus.Currency)) {
		case "token_cera", "token", "代币券":
			cfg.RegisterBonus.Currency = "token_cera"
		default:
			cfg.RegisterBonus.Currency = "cera"
		}
	}
	// 老配置的 pointsink 段没有 currency（或填了非法值）：收敛到点券。
	// 同样只改内存不落盘 —— 老配置（无 currency 键）行为与改动前**逐字一致**，
	// 也不会因为每次启动都"规范化"而反复写文件。
	if cfg.Pointsink != nil {
		switch strings.ToLower(strings.TrimSpace(cfg.Pointsink.Currency)) {
		case "token_cera", "token", "代币券":
			cfg.Pointsink.Currency = "token_cera"
		case "happy_token_cera", "happy_token", "happy", "欢乐代币券":
			cfg.Pointsink.Currency = "happy_token_cera"
		default:
			cfg.Pointsink.Currency = "cera"
		}
	}
	// autobackup 段缺失（旧配置）：写默认值并落盘；显式置 null 时同样补默认。
	var autoBackupProbe struct {
		AutoBackup json.RawMessage `json:"autobackup"`
	}
	_ = json.Unmarshal(raw, &autoBackupProbe)
	if cfg.AutoBackup == nil {
		cfg.AutoBackup = DefaultConfig().AutoBackup
		if len(autoBackupProbe.AutoBackup) == 0 {
			_ = cfg.Save()
		}
	}
	// 老配置里 autobackup 没有 keep 字段（或填了非法值）：收敛到合法区间。
	// 这里只改内存不落盘：等用户下次保存设置时自然写回，避免每次启动都因"规范化"而写文件。
	NormalizeAutoBackup(cfg.AutoBackup)
	// log_file 段缺失（旧配置）：补默认（默认开启落盘日志）并落盘。
	var logFileProbe struct {
		LogFile json.RawMessage `json:"log_file"`
	}
	_ = json.Unmarshal(raw, &logFileProbe)
	if cfg.LogFile == nil {
		cfg.LogFile = DefaultConfig().LogFile
		if len(logFileProbe.LogFile) == 0 {
			_ = cfg.Save()
		}
	}
	// memory_clean 段缺失（旧配置）：补默认（默认关闭）并落盘。
	var memoryCleanProbe struct {
		MemoryClean json.RawMessage `json:"memory_clean"`
	}
	_ = json.Unmarshal(raw, &memoryCleanProbe)
	if cfg.MemoryClean == nil {
		cfg.MemoryClean = DefaultConfig().MemoryClean
		if len(memoryCleanProbe.MemoryClean) == 0 {
			_ = cfg.Save()
		}
	}
	// 兼容旧配置：补齐零值字段。
	base := DefaultConfig()
	if cfg.Manage != nil {
		if cfg.Manage.ListenAddr == "" {
			cfg.Manage.ListenAddr = base.Manage.ListenAddr
		}
		if cfg.Manage.TLSCert == "" {
			cfg.Manage.TLSCert = base.Manage.TLSCert
		}
		if cfg.Manage.TLSKey == "" {
			cfg.Manage.TLSKey = base.Manage.TLSKey
		}
		if cfg.Manage.TokenTTLMinutes <= 0 {
			cfg.Manage.TokenTTLMinutes = base.Manage.TokenTTLMinutes
		}
		if cfg.Manage.MaxFailAttempts <= 0 {
			cfg.Manage.MaxFailAttempts = base.Manage.MaxFailAttempts
		}
		if cfg.Manage.LockoutMinutes <= 0 {
			cfg.Manage.LockoutMinutes = base.Manage.LockoutMinutes
		}
	}
	if cfg.Pointsink != nil {
		if cfg.Pointsink.IntervalSeconds <= 0 {
			cfg.Pointsink.IntervalSeconds = base.Pointsink.IntervalSeconds
		}
		if cfg.Pointsink.Points <= 0 {
			cfg.Pointsink.Points = base.Pointsink.Points
		}
	}
	if cfg.AutoBackup != nil {
		if cfg.AutoBackup.IntervalHours <= 0 || cfg.AutoBackup.IntervalHours > 168 {
			cfg.AutoBackup.IntervalHours = base.AutoBackup.IntervalHours
		}
	}
	if cfg.LogFile != nil {
		if strings.TrimSpace(cfg.LogFile.Dir) == "" {
			cfg.LogFile.Dir = base.LogFile.Dir
		}
		if cfg.LogFile.MaxFileMB <= 0 || cfg.LogFile.MaxFileMB > 1024 {
			cfg.LogFile.MaxFileMB = base.LogFile.MaxFileMB
		}
		if cfg.LogFile.KeepFiles <= 0 || cfg.LogFile.KeepFiles > 500 {
			cfg.LogFile.KeepFiles = base.LogFile.KeepFiles
		}
	}
	if cfg.MemoryClean != nil {
		if cfg.MemoryClean.IntervalMinutes <= 0 || cfg.MemoryClean.IntervalMinutes > 1440 {
			cfg.MemoryClean.IntervalMinutes = base.MemoryClean.IntervalMinutes
		}
	}
	if cfg.LogFile == nil {
		// 兜底：上面漏掉了 nil 分支时这里补齐，保证调用方永远拿得到非 nil 配置。
		cfg.LogFile = base.LogFile
	}
	if cfg.MemoryClean == nil {
		cfg.MemoryClean = base.MemoryClean
	}
	if cfg.DatabaseMode == "" {
		cfg.DatabaseMode = base.DatabaseMode
	}
	if cfg.MySQL.Host == "" {
		cfg.MySQL.Host = base.MySQL.Host
	}
	if cfg.MySQL.Port == 0 {
		cfg.MySQL.Port = base.MySQL.Port
	}
	if cfg.MySQL.User == "" {
		cfg.MySQL.User = base.MySQL.User
	}
	if cfg.MySQL.DBName == "" {
		cfg.MySQL.DBName = base.MySQL.DBName
	}
	if cfg.RegistrationMode == "" {
		cfg.RegistrationMode = base.RegistrationMode
	}
	if cfg.Public.ListenAddr == "" {
		cfg.Public.ListenAddr = base.Public.ListenAddr
	}
	if cfg.Public.TLSCert == "" {
		cfg.Public.TLSCert = base.Public.TLSCert
	}
	if cfg.Public.TLSKey == "" {
		cfg.Public.TLSKey = base.Public.TLSKey
	}
	if cfg.Internal.ListenAddr == "" {
		cfg.Internal.ListenAddr = base.Internal.ListenAddr
	}
	if cfg.Internal.TicketTTLMinutes <= 0 {
		cfg.Internal.TicketTTLMinutes = base.Internal.TicketTTLMinutes
	}
	if cfg.DNFBridge.StatusPort == 0 {
		cfg.DNFBridge.StatusPort = base.DNFBridge.StatusPort
	}
	if cfg.LogMaxLines <= 0 {
		cfg.LogMaxLines = base.LogMaxLines
	}
	return cfg, nil
}

// Path 返回配置文件路径。
func (c *Config) Path() string { return c.path }

// saveMu 串行化配置落盘。
//
// 为什么必须放在最里层而不是靠调用方的锁：
//
//	Save 的调用方分散在互不相通的锁域里——UI 线程（persistUI / 运行维护页 / 备份页 /
//	账号管理页）、后台协程（autoPublicIP 启动后约 2.5s 写 PublicIP）、以及赞助版的
//	manage HTTPS 处理协程（regApply / pointsink / register_bonus）。
//	旧实现用固定临时名 config.json.tmp，于是两个并发 Save 会互相踩：
//	  A 写满 tmp → B 以 O_TRUNC 重写同一个 tmp（写到一半）→ A Rename 把 B 的**半截文件**
//	  发布成 config.json → B 的 Rename 报错（调用方多为 `_ =` 或仅记日志）。
//	后果不是丢一次配置，而是 config.json 变成残缺 JSON：下次启动 config.Load 失败，
//	main 只弹一句"配置加载失败"就 return，**连窗口都不出现**。
//	同时 json.MarshalIndent 会读结构体全部字段，而 autoPublicIP 正在写 PublicIP，
//	属于对 string 头部的撕裂读。
//
// 包级锁 + 唯一临时名一起上：锁保证同一进程内不交错，唯一名保证即使有别的进程
// （或未来的异步写入）也不会撞同一个 tmp 文件。
var saveMu sync.Mutex

// Save 原子写配置（先写唯一临时文件再改名），并做进程内串行化。
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config: empty path")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	saveMu.Lock()
	defer saveMu.Unlock()
	return c.saveLocked(dir)
}

// Mutate 在配置写锁内执行"改字段 → 落盘"。
//
// why：Save 内部是 json.MarshalIndent(c) —— 它会读**整份** Config，包括
// Welfare.LastRuns 这类 map 和 AnnounceSchedule 这类 slice。如果另一个协程在它读 map 的
// 同时往 map 里写，Go 运行时会直接 fatal error: concurrent map read and map write
// （recover 抓不到，整个工具进程退出）；slice 头部撕裂读同样可能让 marshal 读到野指针。
// 而 Save 的调用方分散在互不相通的锁域（UI 线程 / 调度协程 / manage HTTPS 协程），
// 只靠各自的锁串行不了"marshal 读 与 改字段写"。
//
// 所以：凡是改 Config 里的 map/slice（以及成组替换的指针字段）的调用方，都改成
// cfg.Mutate(apply, rollback)：apply 改内存，落盘失败时 rollback 恢复内存，
// 全程持有 saveMu，与任何 Save 互斥。fn 内部**不要**再调用 Save/Mutate（会自锁死）。
func (c *Config) Mutate(apply func(), rollback func()) error {
	if c.path == "" {
		return errors.New("config: empty path")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	saveMu.Lock()
	defer saveMu.Unlock()
	if apply != nil {
		apply()
	}
	if err := c.saveLocked(dir); err != nil {
		if rollback != nil {
			rollback()
		}
		return err
	}
	return nil
}

// saveLocked 真正的落盘（唯一临时文件 + 改名）；调用方必须已持有 saveMu。
func (c *Config) saveLocked(dir string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// 唯一临时名：固定名会让并发写互相截断（见 saveMu 的说明）。
	tmp, err := os.CreateTemp(dir, filepath.Base(c.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	// 先落盘再改名：避免断电/强杀后留下一个"名字对、内容空"的 config.json。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// TicketTTL 返回 ticket 有效期。
func (c *Config) TicketTTL() time.Duration {
	minutes := c.Internal.TicketTTLMinutes
	if minutes <= 0 {
		minutes = 30
	}
	return time.Duration(minutes) * time.Minute
}

// ManageTokenTTL 返回远程管理 token 有效期（分钟数非法时回退默认 120）。
func (c *Config) ManageTokenTTL() time.Duration {
	minutes := 120
	if c.Manage != nil && c.Manage.TokenTTLMinutes > 0 {
		minutes = c.Manage.TokenTTLMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// ManageLockout 返回配对码失败锁定持续时长（默认 10 分钟）。
func (c *Config) ManageLockout() time.Duration {
	minutes := 10
	if c.Manage != nil && c.Manage.LockoutMinutes > 0 {
		minutes = c.Manage.LockoutMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// ResolveDNBWorkDir 返回 dnfbridge 工作目录；为空时取可执行文件所在目录。
func (c *Config) ResolveDNBWorkDir() string {
	if dir := c.DNFBridge.WorkingDir; dir != "" {
		return dir
	}
	if exe := c.DNFBridge.Executable; exe != "" {
		return filepath.Dir(exe)
	}
	return ""
}

// RandomHex 返回 n 字节随机数的 hex 串（长度 2n）。
func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
