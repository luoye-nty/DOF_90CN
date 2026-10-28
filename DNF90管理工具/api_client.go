package main

// 管理 API 客户端：直连服务器端赞助版运维工具 HTTPS :18113 的 /manage/v1/*。
// 证书策略：自签证书不做链校验，改为对等证书 SHA256 指纹比对
// （首连由 conn_tab 弹窗人工核对后保存，后续每次请求前校验，不匹配拒绝）。
// 协议信封：{"ok":true,"data":{...},"error":""}；除 auth/login 外需 X-Manage-Token。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError 非 2xx 或信封 ok=false 的统一错误。
type APIError struct {
	Status int
	Code   string // 服务器信封 error 字段或 http 兜底
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
}

// IsUnauthorized 判断是否为管理 token 失效（需重新登录）。
func (e *APIError) IsUnauthorized() bool {
	return e.Status == http.StatusUnauthorized && e.Code == "unauthorized"
}

// IsNotImplemented 判断服务器端返回 501（占位端点未实装）。
func (e *APIError) IsNotImplemented() bool {
	return e.Status == http.StatusNotImplemented
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// ---------------- 响应结构（与服务器端 internal/manage 序列化字段对齐） ----------------

type manageServer struct {
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

type manageAuthService struct {
	Running     bool   `json:"running"`
	PublicAddr  string `json:"public_addr"`
	InternalURL string `json:"internal_url"`
}

type manageMySQL struct {
	Configured string `json:"configured"`
	Alive      bool   `json:"alive"`
	Mode       string `json:"mode"`
}

type manageCDKSummary struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	Disabled    int `json:"disabled"`
	Redemptions int `json:"redemptions"`
	FailedMails int `json:"failed_mails"`
}

type manageLicense struct {
	Edition string `json:"edition"`
	CDKSet  bool   `json:"cdk_set"`

	// 限时卡信息（老服务端不返回这些字段，零值即"未知"）
	Kind       string `json:"kind"`        // forever / month / quarter / year；空=未知
	ExpireAt   string `json:"expire_at"`   // 到期时间(UTC RFC3339)；永久卡为空
	RemainDays int    `json:"remain_days"` // -1=永久；-2=未知；>=0=剩余天数
	CheckedAt  string `json:"checked_at"`  // 服务器侧最近一次成功校验时间
}

type managePairing struct {
	Enabled bool `json:"enabled"`
}

type manageCert struct {
	CertPath string `json:"cert_path"`
	KeyPath  string `json:"key_path"`
	Exists   bool   `json:"exists"`
}

type manageStatus struct {
	Server  manageServer      `json:"server"`
	Auth    manageAuthService `json:"auth_service"`
	MySQL   manageMySQL       `json:"mysql"`
	Online  int               `json:"online"`
	CDK     manageCDKSummary  `json:"cdk"`
	License manageLicense     `json:"license"`
	Pairing managePairing     `json:"pairing"`
	Cert    manageCert        `json:"cert"`
	Listen  string            `json:"listen_addr"`
}

type loginData struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in_seconds"`
}

type BanOut struct {
	ID        int64  `json:"id"`
	BanType   string `json:"ban_type"`
	BanValue  string `json:"ban_value"`
	Username  string `json:"username"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	BannedAt  string `json:"banned_at"`
	ExpiresAt string `json:"expires_at"`
	RevokedAt string `json:"revoked_at"`
	State     string `json:"state"`
}

type BanListData struct {
	Items []BanOut `json:"items"`
	Total int      `json:"total"`
}

type BanNowResult struct {
	Type     string   `json:"type"`
	Value    string   `json:"value"`
	Kicked   int      `json:"kicked"`
	Warnings []string `json:"warnings"`
}

type CdkOut struct {
	ID            int64  `json:"id"`
	RedeemCode    string `json:"redeem_code"`
	Name          string `json:"name"`
	ContentRaw    string `json:"content_raw"`
	MaxRedeems    int    `json:"max_redeems"`
	RedeemedCount int    `json:"redeemed_count"`
	RewardJSON    string `json:"reward_json"`
	Enabled       bool   `json:"enabled"`
	Remark        string `json:"remark"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     string `json:"created_at"`
}

type CdkListData struct {
	Items       []CdkOut           `json:"items"`
	Total       int                `json:"total"`
	Redemptions []RedeemRedemption `json:"redemptions"`
}

// RedeemRedemption 兑换流水（GET /manage/v1/redeem-codes?redemptions=1&code= 附带）。
type RedeemRedemption struct {
	ID            int64  `json:"id"`
	RedeemCode    string `json:"redeem_code"`
	AccountID     string `json:"account_id"`
	Username      string `json:"username"`
	CharacterID   string `json:"character_id"`
	CharacterName string `json:"character_name"`
	MailStatus    string `json:"mail_status"`
	MailError     string `json:"mail_error"`
	RemoteIP      string `json:"remote_ip"`
	RedeemedAt    string `json:"redeemed_at"`
}

// PointsinkStatus 泡点状态（与服务器端 manage.PointsinkStatus 对齐）。
type PointsinkStatus struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	Points          int  `json:"points"`
	// DailyLimit 账号级每日限额（当前币种的数量）：同一账号一天最多通过泡点获得多少；0 = 不限。
	DailyLimit int `json:"daily_limit"`
	// Currency 发放币种：cera=点券 / token_cera=代币券 / happy_token_cera=欢乐代币券。
	Currency string `json:"currency"`
	// CurrencyLabel 币种中文名（服务器给，避免每个客户端各写一份映射）。
	CurrencyLabel string `json:"currency_label"`
	Running       bool   `json:"running"`
	Awarded       int64  `json:"awarded"`
	GrantedPoints int64  `json:"granted_points"`
	ErrorCount    int64  `json:"error_count"`
	LastGrantAt   int64  `json:"last_grant_at"`
	NextGrantAt   int64  `json:"next_grant_at"`
}

// RegisterBonusStatus 注册赠送状态（与服务器端 manage.RegisterBonusStatus 对齐）。
type RegisterBonusStatus struct {
	Enabled bool `json:"enabled"`
	Points  int  `json:"points"`
	// Currency 赠送币种：cera=点券（默认）/ token_cera=代币券。
	Currency string `json:"currency"`
}

// RegistrationLimitStatus 注册限制配置快照（与服务器端 manage.RegistrationClosedStatus 对齐）。
type RegistrationLimitStatus struct {
	Closed bool `json:"closed"`
	// MaxPerIP 每个来源 IP 允许注册的账号数上限：-1=不限制，0=不允许注册，1~1000=上限。
	MaxPerIP int `json:"max_per_ip"`
	// CodeRequired 是否需要注册码：true 时即使自助注册开放，注册也必须填一枚未使用的注册码。
	// 与 MaxPerIP 是并行的两道闸（都要满足）；登录器自检据此显示注册码输入框。
	CodeRequired bool `json:"code_required"`
}

// RegistrationCode 注册码（邀请码）列表项（与服务器端 manage.RegistrationCode 对齐）。
type RegistrationCode struct {
	Code      string `json:"code"`       // 短码，形如 ABCD-EFGH
	State     string `json:"state"`      // unused / used / revoked
	UsedBy    string `json:"used_by"`    // 使用者：**账号主键**（dnf:<sha256>），显示前必须过 acctNameText 翻成真名
	UsedAt    string `json:"used_at"`    // 使用时间（RFC3339，空=未用）
	CreatedAt string `json:"created_at"` // 生成时间
}

// RegistrationCodeListData GET /manage/v1/registration-codes 的 data。
type RegistrationCodeListData struct {
	Items  []RegistrationCode `json:"items"`
	Total  int                `json:"total"`
	Unused int                `json:"unused"`
}

// MaintenanceStatus 更新维护配置快照（与服务器端 manage.MaintenanceStatus 对齐）。
type MaintenanceStatus struct {
	// Enabled 维护总开关：true 时禁止玩家登录与进游戏（注册照常开放）。
	Enabled bool `json:"enabled"`
	// AllowUsers 维护期间仍可登录/进游戏的账号名白名单（管理员自测用）。
	AllowUsers []string `json:"allow_users"`
}

// LauncherVersionStatus 登录器版本校验配置快照（与服务器端 manage.LauncherVersionStatus 对齐）。
//
// 语义：开关打开后，登录器（外网版 · 远程模式）在启动自检阶段与点登录时各上报一次
// 自己的版本号，与 RequiredVersion 完全一致才放行；不一致 → 登录器按 DownloadURL
// 下载新版本 → 删掉原登录器 → 自动拉起新登录器，**更新完成前禁止进入登录界面**。
type LauncherVersionStatus struct {
	// Enabled 校验总开关。
	Enabled bool `json:"enabled"`
	// DownloadURL 登录器下载直链（压缩包 zip/7z/rar）。
	// Enabled=true 且本字段为空 = 登录器会弹「请联系管理员」并禁止进入（危险配置，UI 要标红）。
	DownloadURL string `json:"download_url"`
	// RequiredVersion 服务端内置要求的登录器版本号（**只读**）。
	// 来自运维工具编译期常量（launcher_version.go 的 builtinLauncherVersion），
	// 管理工具只展示、不可修改 —— 发布新登录器时改运维工具那一处并重启认证服务。
	RequiredVersion string `json:"required_version"`
}

// MultiOpenStatus 双开（多开）开关配置快照（与服务器端 manage.MultiOpenStatus 对齐）。
//
// 三态语义（服务端是 `*bool` 配置，这里用两个字段如实表达）：
//
//	Enabled=true,  Configured=false → 后台**从未设置过**（老 config.json）：
//	                                  玩家侧按"允许双开"执行（改造前的现有行为）；
//	Enabled=true,  Configured=true  → 运营明确允许双开；
//	Enabled=false, Configured=true  → 运营明确关闭 —— **只有这一种情况**登录器才关闸。
//
// 也就是说：`Configured=false` 这一态必须存在，否则"没设置过"与"明确关闭"会被混成
// 同一个 false，升级运维工具那一刻全服双开就会一起消失。
type MultiOpenStatus struct {
	// Enabled 玩家侧生效值：true = 允许双开，false = 已关闭。
	Enabled bool `json:"enabled"`
	// Configured 后台是否**显式设置过**（false = 老配置 / 从未保存过，当前按默认允许）。
	Configured bool `json:"configured"`
}

// AdminKickResult 批量踢下线结果（与服务器端 manage.AdminKickResult 对齐）。
type AdminKickResult struct {
	Requested int      `json:"requested"` // 本次请求踢下线的账号数（去重后）
	Kicked    int      `json:"kicked"`    // 实际断开的在线游戏连接数
	Sessions  int64    `json:"sessions"`  // 作废的登录票（ticket）数
	Failed    []string `json:"failed"`    // 未成功的账号及原因
}

// ---------------- 背包 / 仓库 / 邮箱（服务器端 GM Tool 通道） ----------------

// GMTCharacter 角色摘要（与服务器端 manage.GMTCharacter 对齐）。
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

// GMTMailboxClearResult 清空邮箱结果（走 /local/dnf-gm/mail/clear，支持在线并推送客户端刷新）。
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

// ---------------- 频道管理（服务器端 channel_info.etc 落盘编辑） ----------------

// ChannelEntry 单个频道（与服务器端 channelstore.Entry 对齐）。
type ChannelEntry struct {
	ID       int    `json:"id"`
	ServerID int    `json:"server_id"`
	Type     int    `json:"type"`
	Group    string `json:"group"`
	NameKey  string `json:"name_key"`
	Enabled  bool   `json:"enabled"`
	// Required 标记默认/引导频道（19 引导 / 11 默认城镇）。2026-09-30 起不再是关闭禁令，
	// 只用于 UI 提醒"关掉后会有一批客户端停在选频道界面"（服务端已把两条硬校验降级为告警）。
	Required bool `json:"required"`
	// Ordinary 是否普通城镇频道（type 11）。
	Ordinary bool `json:"ordinary"`
	// SpecialArea 是否特殊区域频道（type 22，线上为 121 远古 / 171 绝望之塔）：
	// 受"任何时刻至少留一个开启"保护。
	SpecialArea bool `json:"special_area"`
}

// ChannelStatus GET /manage/v1/channels 响应。
type ChannelStatus struct {
	Path      string         `json:"path"`
	ServerID  int            `json:"server_id"`
	Total     int            `json:"total"`
	Enabled   int            `json:"enabled"`
	Disabled  int            `json:"disabled"`
	Channels  []ChannelEntry `json:"channels"`
	Backup    string         `json:"backup,omitempty"`
	CheckedAt string         `json:"checked_at"`
}

// ChannelReloadStatus 频道热重载结果（对齐 go-server dnfbridge.GMChannelReloadResult）。
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

// ChannelApplyResult POST /manage/v1/channels/apply 响应。
type ChannelApplyResult struct {
	Changed []int         `json:"changed"`
	Status  ChannelStatus `json:"status"`
	Message string        `json:"message"`
	// HotApplied 改动是否已在运行中的服务端上热生效（无需重启）。
	HotApplied bool `json:"hot_applied"`
	// HotApplyError 热生效失败原因；非空时改动已落盘，但需要重启服务端才生效。
	HotApplyError string `json:"hot_apply_error,omitempty"`
	// Reload 热生效明细（成功时才有）。
	Reload *ChannelReloadStatus `json:"reload,omitempty"`
	// RestartRequired 仅当热生效失败时为 true。
	RestartRequired bool `json:"restart_required"`
}

// ---------------- 记录查询（点券消费 / 对话 / 副本，管理工具专属） ----------------

// RecordPage POST /manage/v1/records/query 响应。
//
// Records 保持 map：三类记录的字段集合不同（对话有 message/recipients、点券消费有
// cera_spent/cera_after、副本有 dungeon_id/elapsed_seconds……），用 map 才能让服务端
// 以后加字段时管理工具不必跟着改。
type RecordPage struct {
	Kind    string           `json:"kind"`
	Title   string           `json:"title"`
	Dir     string           `json:"dir"`
	Records []map[string]any `json:"records"`
	Count   int              `json:"count"`
	Scanned int              `json:"scanned"`
	// Offset 是本次返回的起始位置（服务端回显）。请求 offset=0 时它一定是 0，
	// 所以"翻页能力强探测"要看第二页：老服务端没有这个字段，回显必然是 0。
	Offset int `json:"offset"`
	// Limited=true 表示"还有更多记录"（本次装满了一页），可以带 Offset+Count 继续翻页。
	Limited bool `json:"limited"`
	// Retention 每类记录保留的最近文件数。
	Retention int `json:"retention_files"`
	// AccountNames account_id → 真实账号名（运维工具侧批量翻译，记录里存的是主键）。
	// 缺字段/翻译不到时按空处理：表格与详情退回显示原始主键，绝不显示成空白。
	AccountNames map[string]string `json:"account_names,omitempty"`
}

// RecordKindInfo 一类记录的元信息。
type RecordKindInfo struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// RecordFileInfo 单个记录文件的信息。
type RecordFileInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	Lines   int    `json:"lines,omitempty"`
}

// RecordMeta POST /manage/v1/records/meta 响应：记录目录、类别与文件清单。
type RecordMeta struct {
	Enabled bool             `json:"enabled"`
	Dir     string           `json:"dir"`
	Kinds   []RecordKindInfo `json:"kinds"`
	Files   []RecordFileInfo `json:"files"`
}

// ---------------- 数据库备份（服务器端运维工具 P1 备份闭环） ----------------

// BackupStatus 服务器端自动备份配置+运行态快照（GET /manage/v1/backup/config）。
type BackupStatus struct {
	Enabled       bool   `json:"enabled"`
	IntervalHours int    `json:"interval_hours"`
	Keep          int    `json:"keep"`
	BaseAt        string `json:"base_at"`
	BackupDir     string `json:"backup_dir"`
	NextRunAt     string `json:"next_run_at"`
	// Overdue=true 表示"已到点但还没执行"（运维工具忙 / 上次备份失败正在重试）。
	// 这时 NextRunAt 是下一个未来时刻，只看它会误以为一切正常。
	Overdue       bool   `json:"overdue"`
	DueAt         string `json:"due_at"`
	ServerRunning bool   `json:"server_running"`
	AuthRunning   bool   `json:"auth_running"`
	MySQLMode     bool   `json:"mysql_mode"`
	MySQLAlive    bool   `json:"mysql_alive"`
}

// BackupFileEntry 服务器端备份目录单个文件条目。
type BackupFileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// BackupRestoreResult 服务器端还原结果。
type BackupRestoreResult struct {
	ZipName    string `json:"zip_name"`
	Stopped    bool   `json:"stopped"`
	TableCount int    `json:"table_count"`
	RowCount   int64  `json:"row_count"`
}

// BackupNowResult 服务器端立即备份结果。
type BackupNowResult struct {
	ZipName    string `json:"zip_name"`
	TableCount int    `json:"table_count"`
	RowCount   int64  `json:"row_count"`
}

// ---------------- API 客户端 ----------------

type APIClient struct {
	Base string // https://host:port
	Host string
	Port int
	Fp   string // 期望的服务器证书 SHA256 指纹（十六进制大写、冒号分隔）

	token string
	hc    *http.Client
}

// NewAPIClient 构造客户端（fp 为已信任指纹；后续每次请求先做指纹校验）。
func NewAPIClient(host string, port int, fp string) *APIClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // 自签证书，身份信任由指纹比对承担
		MinVersion:         tls.VersionTLS12,
	}
	return &APIClient{
		Base: fmt.Sprintf("https://%s:%d", host, port),
		Host: host,
		Port: port,
		Fp:   fp,
		hc:   &http.Client{Transport: tr},
	}
}

func (c *APIClient) SetToken(t string) { c.token = t }
func (c *APIClient) Token() string     { return c.token }

// FetchFingerprint 发起 TLS 握手并返回服务器证书 SHA256 指纹（仅取首张叶子证书）。
// 不发送任何业务数据；仅用于首连核对与后续校验。
func FetchFingerprint(host string, port int) (string, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	d := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return "", fmt.Errorf("TLS 握手失败：%w", err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", errors.New("服务器未返回证书")
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	return formatFP(sum[:]), nil
}

func formatFP(sum []byte) string {
	s := strings.ToUpper(hex.EncodeToString(sum))
	parts := make([]string, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, ":")
}

// verifyTrust 当前服务器证书指纹与已信任指纹比对，不匹配拒绝继续。
func (c *APIClient) verifyTrust() error {
	cur, err := FetchFingerprint(c.Host, c.Port)
	if err != nil {
		return err
	}
	if cur != c.Fp {
		return fmt.Errorf("服务器证书指纹不匹配，拒绝连接：\n当前 %s\n期望 %s", cur, c.Fp)
	}
	return nil
}

// do 统一请求：校验指纹 -> 组装请求 -> 解析信封。token 自动附加（login 除外）。
func (c *APIClient) do(ctx context.Context, method, path string, body any, out any) error {
	if err := c.verifyTrust(); err != nil {
		return err
	}

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if !strings.HasPrefix(path, "/manage/v1/auth/login") && c.token != "" {
		req.Header.Set("X-Manage-Token", c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 记录查询一页最多 2000 条（服务端上限），单页 JSON 可能到几 MB；
	// 4MB 的读上限会在极端情况下把响应截断成"解析失败"，这里放宽到 16MB。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return &APIError{Status: resp.StatusCode, Code: "not-found"}
		}
		return fmt.Errorf("响应解析失败（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if resp.StatusCode >= 400 || !env.OK {
		code := strings.TrimSpace(env.Error)
		if code == "" {
			code = fmt.Sprintf("http-%d", resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Code: code}
	}

	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("数据解析失败：%w", err)
		}
	}
	return nil
}

// ---------------- 端点封装 ----------------

// Login POST /manage/v1/auth/login {code} -> {token, expires_in_seconds}。
func (c *APIClient) Login(ctx context.Context, code string) (string, int64, error) {
	var out loginData
	if err := c.do(ctx, http.MethodPost, "/manage/v1/auth/login", map[string]any{"code": code}, &out); err != nil {
		return "", 0, err
	}
	return out.Token, out.ExpiresIn, nil
}

// Status GET /manage/v1/status。
func (c *APIClient) Status(ctx context.Context) (*manageStatus, error) {
	var out manageStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BanList GET /manage/v1/bans?limit=200。
func (c *APIClient) BanList(ctx context.Context) ([]BanOut, error) {
	var out BanListData
	if err := c.do(ctx, http.MethodGet, "/manage/v1/bans?limit=200", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// BanCreate POST /manage/v1/bans。ban_type: account|ip|machine|triple。
func (c *APIClient) BanCreate(ctx context.Context, banType, banValue, username, reason string, days int) (*BanNowResult, error) {
	var out BanNowResult
	req := map[string]any{
		"ban_type":  banType,
		"ban_value": banValue,
		"username":  username,
		"reason":    reason,
		"days":      days,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/bans", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BanDelete DELETE /manage/v1/bans，body {ban_type, ban_value}（服务器契约无 ID 参数）。
func (c *APIClient) BanDelete(ctx context.Context, banType, banValue string) error {
	return c.do(ctx, http.MethodDelete, "/manage/v1/bans",
		map[string]any{"ban_type": banType, "ban_value": banValue}, nil)
}

// BanKick POST /manage/v1/bans/kick {ban_type, ban_value}，返回踢下线会话数。
func (c *APIClient) BanKick(ctx context.Context, banType, banValue string) (int, error) {
	var out struct {
		Kicked int `json:"kicked"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/bans/kick",
		map[string]any{"ban_type": banType, "ban_value": banValue}, &out); err != nil {
		return 0, err
	}
	return out.Kicked, nil
}

// CdkList GET /manage/v1/redeem-codes?page=&page_size=&keyword=。
func (c *APIClient) CdkList(ctx context.Context, keyword string, page, pageSize int) (*CdkListData, error) {
	path := fmt.Sprintf("/manage/v1/redeem-codes?page=%d&page_size=%d&keyword=%s", page, pageSize, urlQueryEscape(keyword))
	var out CdkListData
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CdkRedemptions GET /manage/v1/redeem-codes?redemptions=1&code=...，返回指定码的兑换流水。
func (c *APIClient) CdkRedemptions(ctx context.Context, code string) ([]RedeemRedemption, error) {
	path := fmt.Sprintf("/manage/v1/redeem-codes?page=1&page_size=1&redemptions=1&code=%s", urlQueryEscape(code))
	var out CdkListData
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Redemptions == nil {
		out.Redemptions = []RedeemRedemption{}
	}
	return out.Redemptions, nil
}

// CdkCreate POST /manage/v1/redeem-codes，返回本批新码。
func (c *APIClient) CdkCreate(ctx context.Context, name string, count, maxRedeems int, contentRaw, rewardJSON, remark string) ([]string, error) {
	var out struct {
		Count int      `json:"count"`
		Codes []string `json:"codes"`
	}
	req := map[string]any{
		"name":        name,
		"count":       count,
		"max_redeems": maxRedeems,
		"content_raw": contentRaw,
		"reward_json": rewardJSON,
		"remark":      remark,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/redeem-codes", req, &out); err != nil {
		return nil, err
	}
	return out.Codes, nil
}

// CdkDisable DELETE /manage/v1/redeem-codes/{code}。
func (c *APIClient) CdkDisable(ctx context.Context, code string) error {
	return c.do(ctx, http.MethodDelete, "/manage/v1/redeem-codes/"+pathSegmentEscape(code), nil, nil)
}

// CdkRetryMail POST /manage/v1/redeem-codes/retry-mail {code}，code 空=全量。
func (c *APIClient) CdkRetryMail(ctx context.Context, code string) (sent, failed int, err error) {
	var out struct {
		Sent   int `json:"sent"`
		Failed int `json:"failed"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/redeem-codes/retry-mail",
		map[string]any{"code": code}, &out); err != nil {
		return 0, 0, err
	}
	return out.Sent, out.Failed, nil
}

// ServerStart / ServerStop POST /manage/v1/server/start|stop。
func (c *APIClient) ServerStart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/server/start", map[string]any{}, nil)
}
func (c *APIClient) ServerStop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/server/stop", map[string]any{}, nil)
}

// Announce 发送全服公告，返回公告 id。
func (c *APIClient) Announce(ctx context.Context, content, color string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/announce",
		map[string]any{"content": content, "color": color}, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// PvfReport POST /manage/v1/pvf/info：本地加密完成后上报
// （pvf_version + file_sha256 + key_set 96hex + download_url 更新包直链 + client_download_url 整包直链 + enabled 启用开关）。
// 返回服务器记录 id。
func (c *APIClient) PvfReport(ctx context.Context, pvfVersion, fileSHA256, keySet, downloadURL, clientDownloadURL string, enabled bool) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	req := map[string]any{
		"pvf_version":         pvfVersion,
		"file_sha256":         fileSHA256,
		"key_set":             keySet,
		"download_url":        downloadURL,
		"client_download_url": clientDownloadURL,
		"enabled":             enabled,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/pvf/info", req, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// PvfPublished 服务器当前已发布（active）的 PVF key 记录。
type PvfPublished struct {
	ID         int64  `json:"id"`
	PVFVersion string `json:"pvf_version"`
	FileSHA256 string `json:"file_sha256"`
	KeySet     string `json:"key_set"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	// DownloadURL 客户端更新包直链；ClientDownloadURL 客户端整包直链；
	// Enabled 是否启用加密校验（服务端返回，展示用）。
	DownloadURL       string `json:"download_url"`
	ClientDownloadURL string `json:"client_download_url"`
	Enabled           bool   `json:"enabled"`
}

// PvfList GET /manage/v1/pvf/info：拉取服务器当前已发布 key 记录列表
// （供查看/二次加密引用源密钥）。
func (c *APIClient) PvfList(ctx context.Context) ([]PvfPublished, error) {
	var out []PvfPublished
	if err := c.do(ctx, http.MethodGet, "/manage/v1/pvf/info", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PointsinkStatus GET /manage/v1/pointsink。
func (c *APIClient) PointsinkStatus(ctx context.Context) (*PointsinkStatus, error) {
	var out PointsinkStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/pointsink", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PointsinkApply POST /manage/v1/pointsink {enabled, interval_seconds, points, daily_limit, currency}。
// dailyLimit = 账号级每日限额（当前币种的数量），0 = 不限。
// currency 空串 = 点券（服务器按空值落成点券，兼容老版本）。
func (c *APIClient) PointsinkApply(ctx context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string) (*PointsinkStatus, error) {
	var out PointsinkStatus
	req := map[string]any{
		"enabled":          enabled,
		"interval_seconds": intervalSeconds,
		"points":           points,
		"daily_limit":      dailyLimit,
		"currency":         currency,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/pointsink", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterBonusStatus GET /manage/v1/register-bonus。
func (c *APIClient) RegisterBonusStatus(ctx context.Context) (*RegisterBonusStatus, error) {
	var out RegisterBonusStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/register-bonus", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterBonusApply POST /manage/v1/register-bonus {enabled, points, currency}。
// currency 只接受 cera（点券）或 token_cera（代币券）。
func (c *APIClient) RegisterBonusApply(ctx context.Context, enabled bool, points int, currency string) (*RegisterBonusStatus, error) {
	var out RegisterBonusStatus
	req := map[string]any{
		"enabled":  enabled,
		"points":   points,
		"currency": currency,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/register-bonus", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegistrationLimitStatus GET /manage/v1/registration-limit。
func (c *APIClient) RegistrationLimitStatus(ctx context.Context) (*RegistrationLimitStatus, error) {
	var out RegistrationLimitStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/registration-limit", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegistrationLimitApply POST /manage/v1/registration-limit {closed, max_per_ip, code_required}。
// maxPerIP：-1=不限制，0=不允许注册，1~1000=每个 IP 上限（服务端合法区间 -1~1000，越界返回 400）。
// codeRequired：注册必须填注册码（注册开放时也生效）。
func (c *APIClient) RegistrationLimitApply(ctx context.Context, closed bool, maxPerIP int, codeRequired bool) (*RegistrationLimitStatus, error) {
	var out RegistrationLimitStatus
	req := map[string]any{"closed": closed, "max_per_ip": maxPerIP, "code_required": codeRequired}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/registration-limit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegistrationCodeCreate POST /manage/v1/registration-codes {count}，返回新生成的注册码。
func (c *APIClient) RegistrationCodeCreate(ctx context.Context, count int) ([]string, error) {
	var out struct {
		Codes []string `json:"codes"`
		Count int      `json:"count"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/registration-codes", map[string]any{"count": count}, &out); err != nil {
		return nil, err
	}
	return out.Codes, nil
}

// RegistrationCodeList GET /manage/v1/registration-codes?state=&limit=。
// state 留空=全部；unused / used / revoked。
func (c *APIClient) RegistrationCodeList(ctx context.Context, state string, limit int) (*RegistrationCodeListData, error) {
	path := fmt.Sprintf("/manage/v1/registration-codes?state=%s&limit=%d", urlQueryEscape(strings.TrimSpace(state)), limit)
	var out RegistrationCodeListData
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegistrationCodeRevoke POST /manage/v1/registration-codes/revoke {codes:[...]}，返回实际作废条数。
func (c *APIClient) RegistrationCodeRevoke(ctx context.Context, codes []string) (int, error) {
	var out struct {
		Revoked   int `json:"revoked"`
		Requested int `json:"requested"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/registration-codes/revoke",
		map[string]any{"codes": codes}, &out); err != nil {
		return 0, err
	}
	return out.Revoked, nil
}

// MaintenanceStatusGet GET /manage/v1/maintenance。
func (c *APIClient) MaintenanceStatusGet(ctx context.Context) (*MaintenanceStatus, error) {
	var out MaintenanceStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/maintenance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaintenanceApply POST /manage/v1/maintenance {enabled, allow_users}。
// 服务端保存成功后会立刻广播给所有登录器（开始维护→按钮熄灭并结束游戏进程；
// 取消维护→按钮立刻亮起），所以调用方不需要再做别的动作。
func (c *APIClient) MaintenanceApply(ctx context.Context, enabled bool, allowUsers []string) (*MaintenanceStatus, error) {
	var out MaintenanceStatus
	if allowUsers == nil {
		allowUsers = []string{}
	}
	req := map[string]any{"enabled": enabled, "allow_users": allowUsers}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/maintenance", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LauncherVersionGet GET /manage/v1/launcher-version。
func (c *APIClient) LauncherVersionGet(ctx context.Context) (*LauncherVersionStatus, error) {
	var out LauncherVersionStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/launcher-version", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LauncherVersionApply POST /manage/v1/launcher-version {enabled, download_url}。
//
// 只提交这两个字段：required_version 是运维工具的编译期常量，服务端不接受管理端下发。
// 服务端**允许** enabled=true 且 downloadURL 为空（那是需求确认过的"请联系管理员"分支），
// 所以这里不做前端拦截，由调用方负责在 UI 上标红提醒。
// 与更新维护不同：保存后不需要广播 —— 登录器在自检阶段与点登录时各主动查一次。
func (c *APIClient) LauncherVersionApply(ctx context.Context, enabled bool, downloadURL string) (*LauncherVersionStatus, error) {
	var out LauncherVersionStatus
	req := map[string]any{"enabled": enabled, "download_url": strings.TrimSpace(downloadURL)}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/launcher-version", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MultiOpenGet GET /manage/v1/multi-open。
func (c *APIClient) MultiOpenGet(ctx context.Context) (*MultiOpenStatus, error) {
	var out MultiOpenStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/multi-open", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MultiOpenApply POST /manage/v1/multi-open {enabled}。
//
// enabled 必须明确给出（服务端把它当 *bool 解析，缺字段直接 400）—— 服务端是
// "以本次入参为准"的整组写入，所以不存在"少传就沿旧值"的分支。
// 保存后不需要广播：登录器在自检阶段主动查一次；而且关闭双开**不会**、
// 也**不应该**去结束任何已在运行的副账号进程。
func (c *APIClient) MultiOpenApply(ctx context.Context, enabled bool) (*MultiOpenStatus, error) {
	var out MultiOpenStatus
	req := map[string]any{"enabled": enabled}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/multi-open", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctKickUsers POST /manage/v1/account/kick {usernames:[...]}：踢下线点名的账号。
// 服务端对每个账号做三件事：断开在线游戏连接、写 GM 踢人事件（登录器关游戏进程）、作废登录票。
func (c *APIClient) AcctKickUsers(ctx context.Context, usernames []string) (*AdminKickResult, error) {
	var out AdminKickResult
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/kick",
		map[string]any{"usernames": usernames}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctKickAll POST /manage/v1/account/kick-all：踢下线当前全部在线账号（服务端先列在线再逐个踢）。
func (c *APIClient) AcctKickAll(ctx context.Context) (*AdminKickResult, error) {
	var out AdminKickResult
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/kick-all", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BatchTask 后台批量任务进度（与服务器端 manage.BatchTask 对齐）。
type BatchTask struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`  // grant-mail / welfare
	Scope      string   `json:"scope"` // all-accounts / online-characters
	Phase      string   `json:"phase"` // collecting / sending / finished
	Total      int      `json:"total"`
	Done       int      `json:"done"`
	OK         int      `json:"ok"`
	Failed     int      `json:"failed"`
	Skipped    int      `json:"skipped"`
	Status     string   `json:"status"` // running / done / canceled / failed
	Message    string   `json:"message"`
	Errors     []string `json:"errors"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
}

// Running 任务是否还在跑（管理工具据此决定继续轮询还是收尾）。
func (t *BatchTask) Running() bool { return t != nil && t.Status == "running" }

// BatchGrantRequest 群发邮件入参（与服务器端 manage.batchGrantRequest 对齐）。
type BatchGrantRequest struct {
	// Scope：all-accounts=每个账号的第一个角色（槽位最小）；online-characters=每个在线角色。
	Scope          string `json:"scope"`
	ItemID         uint32 `json:"item_id"`
	Amount         uint32 `json:"amount"`
	ReinforceLevel int    `json:"reinforce_level"`
	AmplifyLevel   int    `json:"amplify_level"`
	AmplifyType    int    `json:"amplify_type"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Bind           bool   `json:"bind"`
}

// AcctItemsGrantBatch POST /manage/v1/account/items/grant-batch：启动一次群发（立刻返回任务）。
func (c *APIClient) AcctItemsGrantBatch(ctx context.Context, req BatchGrantRequest) (*BatchTask, error) {
	var out struct {
		Task BatchTask `json:"task"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/items/grant-batch", req, &out); err != nil {
		return nil, err
	}
	return &out.Task, nil
}

// TaskGet GET /manage/v1/tasks/{id}：查询后台批量任务进度。
func (c *APIClient) TaskGet(ctx context.Context, id string) (*BatchTask, error) {
	var out BatchTask
	if err := c.do(ctx, http.MethodGet, "/manage/v1/tasks/"+pathSegmentEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TaskCancel POST /manage/v1/tasks/{id}/cancel：取消后台批量任务。
func (c *APIClient) TaskCancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/tasks/"+pathSegmentEscape(id)+"/cancel", map[string]any{}, nil)
}

// ---- 在线福利（GET/POST /manage/v1/welfare、POST /welfare/try）----

// WelfareReward 一条奖励（与服务器端 manage.WelfareRewardStatus 对齐）。
type WelfareReward struct {
	Kind           string `json:"kind"` // item / cera / black_diamond
	ItemID         uint32 `json:"item_id,omitempty"`
	Amount         uint32 `json:"amount,omitempty"`
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
	Label          string `json:"label,omitempty"`
}

// WelfareSlot 一个档期（配置 + 服务端算好的计划时刻）。
type WelfareSlot struct {
	Enabled bool            `json:"enabled"`
	Time    string          `json:"time"`
	Weekday int             `json:"weekday,omitempty"`
	Day     int             `json:"day,omitempty"`
	Subject string          `json:"subject,omitempty"`
	Notice  string          `json:"notice,omitempty"`
	Rewards []WelfareReward `json:"rewards"`
	NextRun string          `json:"next_run,omitempty"`
	LastRun string          `json:"last_run,omitempty"`
}

// WelfareStatus GET /manage/v1/welfare 的 data。
type WelfareStatus struct {
	Daily    WelfareSlot       `json:"daily"`
	Weekly   WelfareSlot       `json:"weekly"`
	Monthly  WelfareSlot       `json:"monthly"`
	LastRuns map[string]string `json:"last_runs"`
}

// WelfareApplyRequest POST /manage/v1/welfare 的请求体（与服务器端 config.WelfareConfig 对齐）。
type WelfareApplyRequest struct {
	Daily   WelfareSlot `json:"daily"`
	Weekly  WelfareSlot `json:"weekly"`
	Monthly WelfareSlot `json:"monthly"`
}

// WelfareRunResult 试发结果（与服务器端 manage.WelfareRunResult 对齐）。
type WelfareRunResult struct {
	Accounts    int  `json:"accounts"`
	Characters  int  `json:"characters"`
	ItemMails   int  `json:"item_mails"`
	CeraGranted int  `json:"cera_granted"`
	BlackGrant  int  `json:"black_grant"`
	Failed      int  `json:"failed"`
	NoticeSent  bool `json:"notice_sent"`
}

// WelfareStatusGet GET /manage/v1/welfare。
func (c *APIClient) WelfareStatusGet(ctx context.Context) (*WelfareStatus, error) {
	var out WelfareStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/welfare", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WelfareApply POST /manage/v1/welfare：整批保存三档期配置（服务端校验非法即整批拒绝）。
func (c *APIClient) WelfareApply(ctx context.Context, req WelfareApplyRequest) (*WelfareStatus, error) {
	var out WelfareStatus
	if err := c.do(ctx, http.MethodPost, "/manage/v1/welfare", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- 定时公告（服务器选项页「定时公告」） ----

// ScheduledAnnouncement 一条定时公告。
// 字段与服务器端 config.ScheduledAnnouncement / manage.ScheduledAnnouncementStatus 逐字对齐：
// id/created_at/last_run 由服务端维护（回传只是为了展示与"同一周期不重发"），
// next_run 是服务端算好的下次计划时刻（一次性公告已过点则为空）。
type ScheduledAnnouncement struct {
	ID        string `json:"id,omitempty"`
	Content   string `json:"content"`
	Color     string `json:"color,omitempty"`
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"` // once / daily / weekly
	Time      string `json:"time"` // HH:MM
	Date      string `json:"date,omitempty"`
	Weekday   int    `json:"weekday,omitempty"` // 1=周一 … 7=周日
	LastRun   string `json:"last_run,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	NextRun   string `json:"next_run,omitempty"`
}

// AnnounceScheduleGet GET /manage/v1/announce-schedule：读取定时公告列表。
func (c *APIClient) AnnounceScheduleGet(ctx context.Context) ([]ScheduledAnnouncement, error) {
	var out []ScheduledAnnouncement
	if err := c.do(ctx, http.MethodGet, "/manage/v1/announce-schedule", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AnnounceScheduleApply POST /manage/v1/announce-schedule：整批保存
// （新增 / 修改 / 删除 / 停用都在这一条请求里，服务端校验非法即整批拒绝）。
func (c *APIClient) AnnounceScheduleApply(ctx context.Context, items []ScheduledAnnouncement) ([]ScheduledAnnouncement, error) {
	var out []ScheduledAnnouncement
	req := map[string]any{"items": items}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/announce-schedule", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 服务器公告栏（服务器选项页「服务器公告」，显示在登录器登录页左侧） ----

// LauncherNotice 公告栏的一条：标题 + 日期（无正文），enabled 控制是否在登录器显示。
type LauncherNotice struct {
	ID         int64  `json:"id,omitempty"`
	Title      string `json:"title"`
	NoticeDate string `json:"notice_date,omitempty"`
	Enabled    bool   `json:"enabled"`
	SortOrder  int    `json:"sort_order,omitempty"`
}

// NoticesGet GET /manage/v1/notices：读取公告栏全部条目（含停用）。
func (c *APIClient) NoticesGet(ctx context.Context) ([]LauncherNotice, error) {
	var out []LauncherNotice
	if err := c.do(ctx, http.MethodGet, "/manage/v1/notices", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NoticesApply POST /manage/v1/notices：整表保存（表格里没有的条目等于删除）。
func (c *APIClient) NoticesApply(ctx context.Context, items []LauncherNotice) ([]LauncherNotice, error) {
	var out []LauncherNotice
	req := map[string]any{"items": items}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/notices", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 服务端指定 DLL（服务器选项页「可加载 DLL」） ----

// LauncherDLL 一条"服务端指定 DLL"：管理工具只上传文件名 + SHA256，dll 本体由服主随客户端分发。
type LauncherDLL struct {
	ID       int64  `json:"id,omitempty"`
	FileName string `json:"file_name"`
	SHA256   string `json:"sha256"`
	// OnMismatch：block=登录器弹窗并拒绝启动游戏；skip=只跳过注入照常进游戏。
	OnMismatch string `json:"on_mismatch"`
	Enabled    bool   `json:"enabled"`
	SortOrder  int    `json:"sort_order,omitempty"`
}

// DLLsGet GET /manage/v1/dlls：读取清单（含停用条目）。
func (c *APIClient) DLLsGet(ctx context.Context) ([]LauncherDLL, error) {
	var out []LauncherDLL
	if err := c.do(ctx, http.MethodGet, "/manage/v1/dlls", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DLLsApply POST /manage/v1/dlls：整表保存（清单里没有的条目等于删除）。
func (c *APIClient) DLLsApply(ctx context.Context, items []LauncherDLL) ([]LauncherDLL, error) {
	var out []LauncherDLL
	req := map[string]any{"items": items}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/dlls", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// WelfareApplySlot POST /manage/v1/welfare/slot {kind, slot}：只保存一个档期
// （不会因为别的档期"已启用但没奖励"而挡住本次保存）。
func (c *APIClient) WelfareApplySlot(ctx context.Context, kind string, slot WelfareSlot) (*WelfareStatus, error) {
	var out WelfareStatus
	req := map[string]any{"kind": kind, "slot": slot}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/welfare/slot", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WelfareTry POST /manage/v1/welfare/try {kind}：立即试发某档期（不判定时间、不影响 last_runs）。
func (c *APIClient) WelfareTry(ctx context.Context, kind string) (*WelfareRunResult, error) {
	var out WelfareRunResult
	if err := c.do(ctx, http.MethodPost, "/manage/v1/welfare/try", map[string]any{"kind": kind}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- 背包 / 仓库 / 邮箱端点封装 ----------------

// InventoryQuery POST /manage/v1/account/inventory/query {character_id}。
// 服务端一次返回角色的背包+仓库+账号金库+装备栏完整快照，调用方按 container 过滤即可。
func (c *APIClient) InventoryQuery(ctx context.Context, characterID string) (*GMTInventorySnapshot, error) {
	var out GMTInventorySnapshot
	req := map[string]any{"character_id": characterID}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/inventory/query", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InventoryDelete POST /manage/v1/account/inventory/delete {character_id, container, key, count}。
// count=0（或 ≥ 该格持有数）= 整格删除；否则按数量扣减。执行端要求角色离线。
func (c *APIClient) InventoryDelete(ctx context.Context, characterID, container, key string, count int64) (*GMTInventorySnapshot, error) {
	var out GMTInventorySnapshot
	req := map[string]any{"character_id": characterID, "container": container, "key": key, "count": count}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/inventory/delete", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InventoryClear POST /manage/v1/account/inventory/clear {character_id, container}，清空整个容器。
func (c *APIClient) InventoryClear(ctx context.Context, characterID, container string) (*GMTInventorySnapshot, error) {
	var out GMTInventorySnapshot
	req := map[string]any{"character_id": characterID, "container": container}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/inventory/clear", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MailboxQuery POST /manage/v1/account/mailbox/query {character_id}。
func (c *APIClient) MailboxQuery(ctx context.Context, characterID string) (*GMTMailboxSnapshot, error) {
	var out GMTMailboxSnapshot
	req := map[string]any{"character_id": characterID}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/mailbox/query", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MailboxDelete POST /manage/v1/account/mailbox/delete {character_id, mail_ids, force}。
// 单次最多 100 封；force=false 时执行端会拒绝删除仍带未领取金币/附件的邮件。要求角色离线。
func (c *APIClient) MailboxDelete(ctx context.Context, characterID string, mailIDs []string, force bool) (*GMTMailDeleteResult, error) {
	var out GMTMailDeleteResult
	req := map[string]any{"character_id": characterID, "mail_ids": mailIDs, "force": force}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/mailbox/delete", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MailboxClear POST /manage/v1/account/mailbox/clear {character_id, character_name, confirm}。
// 全清且不可撤销；角色在线也能用，执行端会推送客户端邮箱刷新。
func (c *APIClient) MailboxClear(ctx context.Context, characterID, characterName string) (*GMTMailboxClearResult, error) {
	var out GMTMailboxClearResult
	req := map[string]any{"character_id": characterID, "character_name": characterName, "confirm": true}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/mailbox/clear", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- 批量清空邮箱（全服邮件删除）----------------

// 批量清空邮箱范围（与服务器端 manage 包的常量对齐）。
const (
	// MailboxScopeAllCharacters 全服所有角色（"删除全服邮件"用这一档）。
	MailboxScopeAllCharacters = "all-characters"
	// MailboxScopeOnlineCharacters 只在线的角色。
	MailboxScopeOnlineCharacters = "online-characters"
	// MailboxScopeAllAccounts 每个账号的第一个角色（轻量档，用于演练/抽查）。
	MailboxScopeAllAccounts = "all-accounts"
)

// MailboxClearBatchRequest POST /manage/v1/account/mailbox/clear-batch 入参
// （与服务器端 manage.mailboxClearBatchRequest 对齐）。
type MailboxClearBatchRequest struct {
	Scope string `json:"scope"`
	// Confirm 必须为 true：这是全服级的物理删除，服务端据此拒绝误触发。
	Confirm bool `json:"confirm"`
	// DryRun 只统计目标数量与邮件数，一封都不删（建议先用它量一遍规模）。
	DryRun bool `json:"dry_run"`
	// Max 上限保护（0=用服务端默认）。
	Max int `json:"max,omitempty"`
}

// AcctMailboxClearBatch POST /manage/v1/account/mailbox/clear-batch：启动一次批量清空（立刻返回任务）。
//
// 为什么是后台任务：全服几千个角色要逐个走 GM 接口，放在一次请求里必然超时；
// 进度用 TaskGet 轮询、用 TaskCancel 取消（已清空的不回滚）。
func (c *APIClient) AcctMailboxClearBatch(ctx context.Context, req MailboxClearBatchRequest) (*BatchTask, error) {
	var out struct {
		Task BatchTask `json:"task"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/mailbox/clear-batch", req, &out); err != nil {
		return nil, err
	}
	return &out.Task, nil
}

// ---------------- 频道管理端点封装 ----------------

// ChannelsGet GET /manage/v1/channels。
func (c *APIClient) ChannelsGet(ctx context.Context) (*ChannelStatus, error) {
	var out ChannelStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/channels", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChannelsApply POST /manage/v1/channels/apply {channel_ids, enabled}。
// 返回本次真正改动的频道号与新状态；改动只落盘，需重启 DNF90Server 生效。
func (c *APIClient) ChannelsApply(ctx context.Context, channelIDs []int, enabled bool) (*ChannelApplyResult, error) {
	var out ChannelApplyResult
	req := map[string]any{"channel_ids": channelIDs, "enabled": enabled}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/channels/apply", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- 记录查询端点封装（点券消费/对话/副本/团本/金币寄售/拍卖行） ----------------

// 记录类别取值（与服务端一致：go-server activity_record.go 的 recordKinds）。
const (
	RecordKindCera    = "cera"
	RecordKindChat    = "chat"
	RecordKindDungeon = "dungeon"
	// 团本记录：服务端 raid_log.go 一直在写，但运维工具的白名单以前只放行
	// cera/chat/dungeon，导致这一整类记录在界面上根本选不到（这次一并放开）。
	RecordKindRaid = "raid"
	// 金币寄售 / 拍卖行：2026-09 新增，服务端此前只有 packet_log（高负载会丢行），
	// 上架/下架/成交/手续费/过期都查不到。
	RecordKindGoldConsign = "gold_consign"
	RecordKindAuction     = "auction"
	// 场景加载诊断：选人选场景时"引导包被推迟"这类高频诊断事件。
	// 它们原本被写进团本记录（每秒数条），把真正的团本事件淹没、还会按文件轮转把团本记录挤出去，
	// 所以单独成类（服主实测：打开团本记录满屏都是"场景尾巴包被推迟"）。
	RecordKindScene = "scene"
)

// RecordQueryParams 一次记录查询的条件。零值字段表示不过滤。
type RecordQueryParams struct {
	Kind          string
	Limit         int
	Offset        int
	Keyword       string
	CharacterID   string
	CharacterName string
	AccountID     string
	Since         string
	Until         string
}

// RecordsQuery POST /manage/v1/records/query。
//
// 记录条目字段随类别不同，所以 Records 保持 map（服务端原样透传）：
// 表格只展示几列公共信息，完整字段在详情区按 JSON 展开。
//
// offset 只在 >0 时才发出去：这样"第一页"对老服务端也完全兼容（老服务端用了
// DisallowUnknownFields，多一个字段会直接 400）。
func (c *APIClient) RecordsQuery(ctx context.Context, params RecordQueryParams) (*RecordPage, error) {
	req := map[string]any{"kind": params.Kind}
	if params.Limit > 0 {
		req["limit"] = params.Limit
	}
	if params.Offset > 0 {
		req["offset"] = params.Offset
	}
	if params.Keyword != "" {
		req["keyword"] = params.Keyword
	}
	if params.CharacterID != "" {
		req["character_id"] = params.CharacterID
	}
	if params.CharacterName != "" {
		req["character_name"] = params.CharacterName
	}
	if params.AccountID != "" {
		req["account_id"] = params.AccountID
	}
	if params.Since != "" {
		req["since"] = params.Since
	}
	if params.Until != "" {
		req["until"] = params.Until
	}
	var out RecordPage
	if err := c.do(ctx, http.MethodPost, "/manage/v1/records/query", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordsMeta POST /manage/v1/records/meta：记录目录、类别与文件清单（健康度自检）。
func (c *APIClient) RecordsMeta(ctx context.Context) (*RecordMeta, error) {
	var out RecordMeta
	if err := c.do(ctx, http.MethodPost, "/manage/v1/records/meta", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdminRegister POST /manage/v1/account/register 管理员建号（绕过注册限制，自动套注册赠送）。
// rescueCode：6 位数字救援代码（找回密码用），留空表示不设置。
func (c *APIClient) AdminRegister(ctx context.Context, username, password, rescueCode string) error {
	req := map[string]any{"username": username, "password": password, "rescue_code": rescueCode}
	return c.do(ctx, http.MethodPost, "/manage/v1/account/register", req, nil)
}

// AdminChangePassword POST /manage/v1/account/password 管理员改密。
func (c *APIClient) AdminChangePassword(ctx context.Context, username, newPassword string) error {
	req := map[string]any{"username": username, "new_password": newPassword}
	return c.do(ctx, http.MethodPost, "/manage/v1/account/password", req, nil)
}

// AdminForceLogout POST /manage/v1/account/force-logout 强制下线。
func (c *APIClient) AdminForceLogout(ctx context.Context, username string) error {
	req := map[string]any{"username": username}
	return c.do(ctx, http.MethodPost, "/manage/v1/account/force-logout", req, nil)
}

// BackupStatus GET /manage/v1/backup/config：读取服务器端自动备份配置与运行态。
func (c *APIClient) BackupStatus(ctx context.Context) (*BackupStatus, error) {
	var out BackupStatus
	if err := c.do(ctx, http.MethodGet, "/manage/v1/backup/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BackupApply POST /manage/v1/backup/config：应用自动备份配置
// （间隔 1~168 小时、保留 1~500 份，基准=服务器当前时刻）。
func (c *APIClient) BackupApply(ctx context.Context, enabled bool, intervalHours, keep int) (*BackupStatus, error) {
	var out BackupStatus
	req := map[string]any{
		"enabled":        enabled,
		"interval_hours": intervalHours,
		"keep":           keep,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/backup/config", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BackupList GET /manage/v1/backup/list：列出服务器端备份目录全部备份。
func (c *APIClient) BackupList(ctx context.Context) ([]BackupFileEntry, error) {
	var out []BackupFileEntry
	if err := c.do(ctx, http.MethodGet, "/manage/v1/backup/list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BackupNow POST /manage/v1/backup/now：远程触发一次在线备份（不停止服务端）。
func (c *APIClient) BackupNow(ctx context.Context) (*BackupNowResult, error) {
	var out BackupNowResult
	if err := c.do(ctx, http.MethodPost, "/manage/v1/backup/now", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BackupRestore POST /manage/v1/backup/restore：覆盖还原服务器端指定备份。
// 服务端检测到运行中会先自动停止（认证/DNF90Server/MySQL）再拉起 MySQL 还原。
func (c *APIClient) BackupRestore(ctx context.Context, file string) (*BackupRestoreResult, error) {
	var out BackupRestoreResult
	req := map[string]any{"file": file}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/backup/restore", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Logout 尽力调用 auth/logout；服务器端 P0+P1 未注册该端点时会返回 404。
func (c *APIClient) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/auth/logout", map[string]any{}, nil)
}

// ---------------- 账号管理（GM 直连转发）端点封装 ----------------
// 路由 /manage/v1/account/*（运维端 handlers_account.go AC-1~AC-12）。
// 数据全部经 go-server 原生 GM 接口转发；客户端不做 MySQL 直连/背包直入。

// acctCharacterRow AC-1 角色查询行（对齐 GM gmCharacterView）。
type acctCharacterRow struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

// acctMailOut 邮件结果（AC-2/AC-4/AC-5 共用，对齐 GM gmMailResult）。
type acctMailOut struct {
	MailID        string `json:"mail_id"`
	RecipientName string `json:"recipient_name"`
	Online        bool   `json:"online"`
	Refreshed     bool   `json:"refreshed"`
	RefreshError  string `json:"refresh_error"`
}

// acctWalletBody 钱包余额/变化结果（对齐 GM gmWalletResult）。
type acctWalletBody struct {
	Character acctCharacterRow `json:"character"`
	Currency  string           `json:"currency"`
	Mode      string           `json:"mode"`
	Before    int64            `json:"before"`
	After     int64            `json:"after"`
	Cera      int64            `json:"cera"`
	// TokenCera / HappyTokenCera 账号级代币券 / 欢乐代币券（背包编辑页钱包区要用）。
	TokenCera      int64 `json:"token_cera"`
	HappyTokenCera int64 `json:"happy_token_cera"`
	Gold           int64 `json:"gold"`
	Online         bool  `json:"online"`
	Refreshed      bool  `json:"refreshed"`
}

// acctWalletMailOut AC-4 资产邮件结果。
// gold 模式字段在 data 顶层；cera 模式字段嵌套在 data.result（mail_error 分支同）。
type acctWalletMailOut struct {
	Character acctCharacterRow `json:"character"`
	Currency  string           `json:"currency"`
	Mode      string           `json:"mode"`
	Amount    int64            `json:"amount"`
	Before    int64            `json:"before"`
	After     int64            `json:"after"`
	Cera      int64            `json:"cera"`
	Gold      int64            `json:"gold"`
	Online    bool             `json:"online"`
	Refreshed bool             `json:"refreshed"`
	Delivery  string           `json:"delivery"`
	Mail      *acctMailOut     `json:"mail"`
	MailError string           `json:"mail_error"`
	Message   string           `json:"message"`
}

// acctItemGrantOut AC-5 物品发放结果。
type acctItemGrantOut struct {
	ItemID         uint32       `json:"item_id"`
	ItemName       string       `json:"item_name"`
	Amount         uint32       `json:"amount"`
	Route          string       `json:"route"`
	ReinforceLevel int          `json:"reinforce_level"`
	AmplifyLevel   int          `json:"amplify_level"`
	AmplifyType    int          `json:"amplify_type"`
	Mail           *acctMailOut `json:"mail"`
	Message        string       `json:"message"`
}

// acctJobOut AC-8 转职现状（对齐 GM gmJobInfo）。
type acctJobOut struct {
	CharacterID string `json:"character_id"`
	Job         int    `json:"job"`
	FirstGrow   int    `json:"first_grow"`
	SecondGrow  int    `json:"second_grow"`
}

// acctPVFStatusOut AC-12 执行端 PVF 索引状态。
type acctPVFStatusOut struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Indexed bool   `json:"indexed"`
	Entries int    `json:"entries"`
}

// AcctCharacters POST /manage/v1/account/characters/query。
// username/characterName 至少一项；两者都空由服务端拒绝。
func (c *APIClient) AcctCharacters(ctx context.Context, username, characterName string) ([]acctCharacterRow, error) {
	req := map[string]any{}
	if username != "" {
		req["username"] = username
	}
	if characterName != "" {
		req["character_name"] = characterName
	}
	if len(req) == 0 {
		return nil, errors.New("账号名与角色名至少填写一项")
	}
	var out []acctCharacterRow
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/characters/query", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// acctNamesOut POST /manage/v1/account/names 响应。
type acctNamesOut struct {
	AccountNames map[string]string `json:"account_names"`
	Missing      []string          `json:"missing"`
}

// AcctNames POST /manage/v1/account/names：批量把账号主键翻成真实账号名。
//
// 账号主键是 dnf:<sha256(用户名)>（不可逆），"主键 → 用户名"的映射表只在服务端
// 运维工具侧的 MySQL，所以界面上凡是要显示账号的地方都走这里批量翻译
// （统一口径见 account_names.go；一次最多 2000 个主键）。
func (c *APIClient) AcctNames(ctx context.Context, accountIDs []string) (map[string]string, error) {
	if len(accountIDs) == 0 {
		return map[string]string{}, nil
	}
	var out acctNamesOut
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/names",
		map[string]any{"account_ids": accountIDs}, &out); err != nil {
		return nil, err
	}
	if out.AccountNames == nil {
		return map[string]string{}, nil
	}
	return out.AccountNames, nil
}

// onlineCharacterOut AC-13 在线角色行（服务端 onlineCharacterRow）。
type onlineCharacterOut struct {
	Username    string `json:"username"`
	AccountID   string `json:"account_id"`
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	LastIP      string `json:"last_ip"`
	MachineID   string `json:"machine_id"`
}

// onlineCharactersOut AC-13 在线角色快照响应。
type onlineCharactersOut struct {
	Characters []onlineCharacterOut `json:"characters"`
	Accounts   int                  `json:"accounts"`   // 参与检查的在线账号数
	Failures   int                  `json:"failures"`   // 角色查询失败的账号数
	CheckedAt  string               `json:"checked_at"` // 服务端快照时刻（RFC3339）
}

// AcctOnlineCharacters GET /manage/v1/account/online-characters。
//
// 取一份"在线角色"快照（点一次刷新 = 一份，不做自动轮询）：
// 服务端先取登录器会话未过期的在线账号，再逐个账号查 GM 角色接口，
// 只返回游戏内真正在线的角色（账号有会话但停在登录器/选人界面的不算）。
func (c *APIClient) AcctOnlineCharacters(ctx context.Context) (*onlineCharactersOut, error) {
	var out onlineCharactersOut
	if err := c.do(ctx, http.MethodGet, "/manage/v1/account/online-characters", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctMailSend POST /manage/v1/account/mail/send（系统邮件）。
// itemID=0 表示不带物品附件；bind 仅附件生效。
func (c *APIClient) AcctMailSend(ctx context.Context, roleName, title, body string, gold int64, itemID, amount uint32, bind bool) (*acctMailOut, error) {
	var out acctMailOut
	req := map[string]any{
		"role_name": roleName,
		"title":     title,
		"body":      body,
		"gold":      gold,
	}
	if itemID != 0 {
		req["item_id"] = itemID
		req["amount"] = amount
		req["bind"] = bind
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/mail/send", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctWalletQuery POST /manage/v1/account/wallet/query。
func (c *APIClient) AcctWalletQuery(ctx context.Context, roleName string) (*acctWalletBody, string, error) {
	var out struct {
		Wallet  acctWalletBody `json:"wallet"`
		Message string         `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/wallet/query",
		map[string]any{"role_name": roleName}, &out); err != nil {
		return nil, "", err
	}
	return &out.Wallet, out.Message, nil
}

// AcctWalletUpdate POST /manage/v1/account/wallet/update {role_name, currency, mode, amount}。
// currency：cera / token_cera / happy_token_cera / gold；mode：set / add。
// 背包编辑页的"清零/设为"用的就是它（复活币不在其中，走 InventoryDelete）。
func (c *APIClient) AcctWalletUpdate(ctx context.Context, roleName, currency, mode string, amount int64) (*acctWalletBody, string, error) {
	var out struct {
		Wallet  acctWalletBody `json:"wallet"`
		Message string         `json:"message"`
	}
	req := map[string]any{
		"role_name": roleName,
		"currency":  currency,
		"mode":      mode,
		"amount":    amount,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/wallet/update", req, &out); err != nil {
		return nil, "", err
	}
	return &out.Wallet, out.Message, nil
}

// AcctWalletMail POST /manage/v1/account/wallet/mail（gold 邮件/cera 直接入账+通知邮件）。
// 处理服务端两种 data 形态（gold 顶层平铺 / cera 嵌套 result）并统一为 acctWalletMailOut。
func (c *APIClient) AcctWalletMail(ctx context.Context, roleName, currency string, amount int64) (*acctWalletMailOut, error) {
	var data json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/wallet/mail",
		map[string]any{"role_name": roleName, "currency": currency, "amount": amount}, &data); err != nil {
		return nil, err
	}
	var probe struct {
		Result  json.RawMessage `json:"result"`
		Message string          `json:"message"`
	}
	_ = json.Unmarshal(data, &probe)
	var out acctWalletMailOut
	if len(probe.Result) > 0 && string(probe.Result) != "null" {
		if err := json.Unmarshal(probe.Result, &out); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	out.Message = probe.Message
	return &out, nil
}

// ---------------- 批量资产邮件（在线角色 / 全服首个角色）----------------

// 批量资产邮件的发放范围（与服务器端 manage 包的常量对齐）。
const (
	// WalletMailScopeOnlineChars 每个在线角色各一份。
	WalletMailScopeOnlineChars = "online-characters"
	// WalletMailScopeAllAccounts 每个账号的第一个角色（"全服"档）。
	WalletMailScopeAllAccounts = "all-accounts"
	// WalletMailScopeServerFirst 全服创建最早的那个角色（唯一一个）。
	//
	// 注意口径：按 dnf_characters.created_at 升序、再按 character_id 数字升序取第一条
	// （created_at 可为空的历史行排在最后）。它很可能命中测试号/长期不上线的号，
	// 所以界面上必须先回显"解析到的角色名 + 账号"让人确认。
	WalletMailScopeServerFirst = "server-first-character"
)

// WalletMailBatchRequest POST /manage/v1/account/wallet/mail-batch 入参
// （与服务器端 manage.walletMailBatchRequest 对齐）。
type WalletMailBatchRequest struct {
	Scope    string `json:"scope"`
	Currency string `json:"currency"` // gold（邮件附件）/ cera（直接入账 + 通知邮件）
	Amount   int64  `json:"amount"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	// DryRun 只解析目标、不发放：结果（哪些角色会被发到）显示在任务消息里。
	// 「全服首个角色」这种只有一个目标的档位尤其需要它 —— 运营点确认前得先知道那是谁。
	DryRun bool `json:"dry_run"`
	Max    int  `json:"max,omitempty"`
}

// AcctWalletMailBatch POST /manage/v1/account/wallet/mail-batch：启动一次批量资产邮件（立刻返回任务）。
//
// ⚠ cera（点券）分支不幂等：服务端是"先入账、再发通知邮件"，通知失败时钱已经到账，
// 那种目标会被单独计数且**不会重试**（重试等于重复发券）。进度里看到这个计数时不要重跑整批。
func (c *APIClient) AcctWalletMailBatch(ctx context.Context, req WalletMailBatchRequest) (*BatchTask, error) {
	var out struct {
		Task BatchTask `json:"task"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/wallet/mail-batch", req, &out); err != nil {
		return nil, err
	}
	return &out.Task, nil
}

// AcctItemGrant POST /manage/v1/account/items/grant（系统邮件附件发放）。
// reinforce/amplify/amplifyType 仅装备类生效，执行端二次校验。
func (c *APIClient) AcctItemGrant(ctx context.Context, roleName string, itemID, amount uint32, reinforce, amplify, amplifyType int) (*acctItemGrantOut, error) {
	var out acctItemGrantOut
	req := map[string]any{
		"role_name":       roleName,
		"item_id":         itemID,
		"amount":          amount,
		"reinforce_level": reinforce,
		"amplify_level":   amplify,
		"amplify_type":    amplifyType,
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/items/grant", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctSPTPSet POST /manage/v1/account/character/sptp（sp/tp 为 nil 表示不修改该字段）。
func (c *APIClient) AcctSPTPSet(ctx context.Context, characterID string, sp, tp *int) error {
	req := map[string]any{"character_id": characterID}
	if sp != nil {
		req["remaining_sp"] = *sp
	}
	if tp != nil {
		req["remaining_tp"] = *tp
	}
	return c.do(ctx, http.MethodPost, "/manage/v1/account/character/sptp", req, nil)
}

// AcctLevelSet POST /manage/v1/account/character/level（服务端校验 1..90）。
func (c *APIClient) AcctLevelSet(ctx context.Context, characterID string, level int) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/account/character/level",
		map[string]any{"character_id": characterID, "level": level}, nil)
}

// AcctJobGet GET /manage/v1/account/character/job?character_id=。
func (c *APIClient) AcctJobGet(ctx context.Context, characterID string) (*acctJobOut, error) {
	var out acctJobOut
	path := "/manage/v1/account/character/job?character_id=" + url.QueryEscape(characterID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctJobSet POST /manage/v1/account/character/job（job/first_grow/second_grow 一次性提交）。
func (c *APIClient) AcctJobSet(ctx context.Context, characterID string, job, firstGrow, secondGrow int) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/account/character/job",
		map[string]any{"character_id": characterID, "job": job, "first_grow": firstGrow, "second_grow": secondGrow}, nil)
}

// AcctCareerSet POST /manage/v1/account/character/career（**跨职业大类**转职）。
//
// 与 AcctJobSet 是服务端两条不同的路：
//   - AcctJobSet  → /local/dnf-gm/character/job：只改 grow_type，且要求 job 不变（同系换分支/觉醒）
//   - AcctCareerSet → /dnf/gm/characters/{id}/career：真写 job，重建技能、重算属性、清旧系转职任务
//
// 后者**要求角色离线**，后果是产品级的 —— 所以单开一个方法，由界面上的独立按钮显式触发，
// 不跟 AcctJobSet 合成一个"自动选路"的入口。
func (c *APIClient) AcctCareerSet(ctx context.Context, characterID string, job, firstGrow, secondGrow int) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/account/character/career",
		map[string]any{"character_id": characterID, "job": job, "first_grow": firstGrow, "second_grow": secondGrow}, nil)
}

// AcctDungeonsUnlockAll POST /manage/v1/account/dungeons/unlock-all，返回解锁副本数。
func (c *APIClient) AcctDungeonsUnlockAll(ctx context.Context, characterID string) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/dungeons/unlock-all",
		map[string]any{"character_id": characterID}, &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// AcctEquipSlotsUnlockAll POST /manage/v1/account/equip-slots/unlock-all。
func (c *APIClient) AcctEquipSlotsUnlockAll(ctx context.Context, characterID string) error {
	return c.do(ctx, http.MethodPost, "/manage/v1/account/equip-slots/unlock-all",
		map[string]any{"character_id": characterID}, nil)
}

// acctBlackDiamondOut POST /manage/v1/account/character/black-diamond 的返回。
// ExpiresAt/RemainingSeconds 是**激活后**的账号级黑钻到期时间（黑钻同账号所有角色共享）。
type acctBlackDiamondOut struct {
	CharacterID      string `json:"character_id"`
	AccountID        string `json:"account_id"`
	Days             int    `json:"days"`
	ExpiresAt        int64  `json:"expires_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Fatigue          int64  `json:"fatigue"`
	FatigueLimit     int64  `json:"fatigue_limit"`
	Online           bool   `json:"online"`
	Refreshed        bool   `json:"refreshed"`
	RefreshError     string `json:"refresh_error"`
	Deferred         bool   `json:"deferred"`
	Message          string `json:"message"`
}

// AcctBlackDiamond POST /manage/v1/account/character/black-diamond（黑钻按天算）。
//
// 黑钻是账号级权益：执行端按 max(现在, 当前到期时间) + 天数 计算新到期时间，
// 因此对同一账号重复提交就是"续期叠加"；在线角色会顺带刷新原生黑钻状态包。
func (c *APIClient) AcctBlackDiamond(ctx context.Context, characterID string, days int) (*acctBlackDiamondOut, error) {
	var out acctBlackDiamondOut
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/character/black-diamond",
		map[string]any{"character_id": characterID, "days": days}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// acctBlackDiamondStatusOut POST /manage/v1/account/character/black-diamond/query 的返回。
// Source 为 "server"（执行端实时查询）或 "database"（执行端直读游戏库兜底）。
type acctBlackDiamondStatusOut struct {
	CharacterID      string `json:"character_id"`
	CharacterName    string `json:"character_name"`
	AccountID        string `json:"account_id"`
	Active           bool   `json:"active"`
	ExpiresAt        int64  `json:"expires_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	RemainingDays    int64  `json:"remaining_days"`
	Fatigue          int64  `json:"fatigue"`
	FatigueLimit     int64  `json:"fatigue_limit"`
	Online           bool   `json:"online"`
	ServerNow        int64  `json:"server_now"`
	Source           string `json:"source"`
	Message          string `json:"message"`
}

// AcctBlackDiamondQuery POST /manage/v1/account/character/black-diamond/query（只读，不改数据）。
func (c *APIClient) AcctBlackDiamondQuery(ctx context.Context, characterID string) (*acctBlackDiamondStatusOut, error) {
	var out acctBlackDiamondStatusOut
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/character/black-diamond/query",
		map[string]any{"character_id": characterID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcctPVFIndexStatus POST /manage/v1/account/pvf/index/status（pvfPath 空=执行端 instance.json 路径）。
func (c *APIClient) AcctPVFIndexStatus(ctx context.Context, pvfPath string) (*acctPVFStatusOut, error) {
	var out acctPVFStatusOut
	if err := c.do(ctx, http.MethodPost, "/manage/v1/account/pvf/index/status",
		map[string]any{"pvf_path": pvfPath}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func pathSegmentEscape(s string) string { return url.PathEscape(s) }
