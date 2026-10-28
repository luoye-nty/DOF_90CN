using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Interop;
using System.Windows.Media;
using System.Windows.Media.Animation;
using System.Windows.Media.Imaging;
using System.Windows.Threading;
using AduSkin.Controls.Metro;

namespace DNFLogin
{
    /// <summary>
    /// 登录窗口：本地模式直连 MySQL 验证（固定 GameOuterToken + PID 绑定多账号），
    /// 远程模式走 HTTPS API 获取会话凭证（注入 DNF_REMOTE_AUTH_* 环境变量，无 PID 绑定）。
    /// 支持双开副登录框、bcrypt 登录验证、16 段参数启动 DNF.exe + 环境变量注入、
    /// DLL 注入、进程托管、连接检测（在线/离线）、JSON 配置 + 凭据管理器。
    /// </summary>
    public partial class LoginWindow : AduWindow
    {
        // ── Win32 常量（仅 WM_NCHITTEST） ──────────────────────
        private const int WM_NCHITTEST = 0x0084;
        private const int HTCLIENT = 1;

        // ── 资源 ────────────────────────────────────────────────
        private static readonly Uri OnlineSuccessUri = new("pack://application:,,,/assets/icon-message-success.png", UriKind.Absolute);
        private static readonly Uri OnlineErrorUri = new("pack://application:,,,/assets/icon-message-error.png", UriKind.Absolute);

        // ── 轮换背景资源（显示顺序：default → 1..6 → default 循环）──
        // 注意：pack:// 资源名在单文件打包后为小写，路径必须全小写才能命中
        private static readonly Uri[] BackgroundUris =
        {
            new("pack://application:,,,/assets/default-bg-img.png", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground1.jpg", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground2.jpg", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground3.png", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground4.jpg", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground5.png", UriKind.Absolute),
            new("pack://application:,,,/assets/rotatingbackground6.png", UriKind.Absolute),
        };

        private const int BgRotationSeconds = 5;      // 每 5 秒轮换一次
        private const int BgFadeMilliseconds = 1200;  // 交叉淡入淡出时长

        // ── 字段 ────────────────────────────────────────────────
        private HwndSource? _hwndSource;
        private readonly List<BitmapImage> _bgImages = new();
        private readonly List<int> _playOrder = new(); // 播放顺序（顺序=自然序，随机=洗牌后）
        private int _playPos;                          // 当前播放位置（显示 _playOrder[_playPos]）
        private DispatcherTimer? _bgTimer;
        private int _bgIndex;
        private bool _hasShown;
        private bool _isLoggingIn;
        private bool _rescuePromptShown;                    // 主框登录后老号无救援代码是否已弹补设引导（防重复/双开各弹一次）
        private bool _isSecondaryLoggingIn;
        private bool _mainLoggedIn;
        private bool _secondaryLoggedIn;
        private bool _forceClose;
        private bool _isOnline;
        private bool _startupCheckHandled;
        private DispatcherTimer? _onlineTimer;
        private System.Windows.Forms.NotifyIcon? _trayIcon;
        private string? _remoteTicket;                          // 90 远程登录拿到的 ticket（LANA 握手）
        private string? _remoteAccountId;                       // 90 远程登录拿到的 account_id
        private Services.LanaForwarder? _lanaForwarder;         // 90 远程本地转发器

        // 90 远程已登录槽位会话（双开各槽独立记录；_remoteTicket 仅指向最近登录会话，
        // 轮询 GM 踢人事件时按槽位逐一查询，避免双开时先登录账号的事件被后登录覆盖丢失）
        private readonly List<(int Slot, string Ticket, string AccountId, string Username)> _remoteSessions = new();

        /// <summary>
        /// 保护 _remoteSessions。
        ///
        /// **必须有**：这个 List 现在被两个后台线程读（公告长轮询、GM 事件长轮询），
        /// 同时被 UI 线程写（登录登记会话 / 退出时移除槽位）。
        /// `List&lt;T&gt;` 不是线程安全的：并发 `Add` 撞上 `ToArray()`/`FirstOrDefault()`
        /// 可能抛异常，也可能让读方拿到一个"半成品"快照 —— 后者的表现正是
        /// "某次轮询悄悄什么都没做"，也就是我们刚花很久才定位的那种偶发不弹。
        /// 所有访问一律走 SnapshotRemoteSessions / 下面的加锁写法。
        /// </summary>
        private readonly object _remoteSessionsLock = new();

        /// <summary>取一份 _remoteSessions 快照（后台线程遍历时用这个，不要直接读字段）。</summary>
        private (int Slot, string Ticket, string AccountId, string Username)[] SnapshotRemoteSessions()
        {
            lock (_remoteSessionsLock)
            {
                return _remoteSessions.ToArray();
            }
        }

        /// <summary>当前已登记的远程会话数。</summary>
        private int RemoteSessionCount()
        {
            lock (_remoteSessionsLock)
            {
                return _remoteSessions.Count;
            }
        }

        /// <summary>取第一个远程会话（公告长轮询用）。没有会话时返回 default。</summary>
        private (int Slot, string Ticket, string AccountId, string Username) FirstRemoteSession()
        {
            lock (_remoteSessionsLock)
            {
                return _remoteSessions.Count > 0 ? _remoteSessions[0] : default;
            }
        }
        /// <summary>
        /// GM 事件游标：**按账号各存一个**（不能所有槽位共用一个）。
        ///
        /// 为什么必须按账号分：`/launcher/v1/gm-events` 是按 ticket 的账号过滤的，
        /// 而多开时各槽位共用一个游标会导致——先轮询的账号把游标推高，后轮询账号的
        /// **小 id 事件永远查不出来**（服务端只回 `id &gt; after_id` 的）。
        /// 表现就是"多开时其中一个号永远收不到到账提示"。
        /// </summary>
        private readonly Dictionary<string, long> _gmCursorByAccount = new(StringComparer.OrdinalIgnoreCase);
        /// <summary>保护 _gmCursorByAccount（10 秒兜底轮询与长轮询两条通道并发访问）。</summary>
        private readonly object _gmEventLock = new();
        /// <summary>
        /// 本次会话的"事件起始时刻"（RFC3339）：轮询时作为 since 带给服务端。
        ///
        /// 它**取代**了以前那个跨会话持久化的 id 游标（AppConfig.LastGmEventId）。
        /// 换掉的原因（2026-09-28 线上实测）：id 游标一旦比服务端现有事件的 id 还大，
        /// `id &gt; after_id` 就永远返回空 —— 典型触发场景是数据库重建/清表/换库后 id 从 1
        /// 重新开始，而登录器里存着 56232。表现是"轮询正常、没有任何报错、但泡点到账
        /// 提示再也不弹"，且毫无痕迹可查。
        /// 用"会话起始时刻"就没有这个问题：每次启动都是全新的时间窗，天然自愈。
        ///
        /// 故意回拨一个安全余量，容忍登录器与服务端的时钟偏差（见 GmEventSinceMargin）。
        /// </summary>
        private string _gmEventSince = "";
        private static readonly TimeSpan GmEventSinceMargin = TimeSpan.FromMinutes(10);
        private CancellationTokenSource? _gmEventCts;           // GM 事件长轮询取消源（实时推送）
        private bool _gmEventLoopStarted;                       // 防止重复启动
        /// <summary>服务端没有 /gm-events/wait（老版运维工具）→ 关掉长轮询，只留 10 秒兜底轮询。</summary>
        private bool _gmEventWaitUnsupported;
        /// <summary>长轮询失败后的退避时长。**必须有**：404/网络错误是秒回的，
        /// 不退避就会变成每秒几百次的死循环，把认证服务和本机都拖住。</summary>
        private const int GmEventWaitBackoffMs = 5000;
        /// <summary>
        /// 到账提示的"保鲜期"：只弹这个时间窗内的 cera_granted 事件。
        ///
        /// 替代原来的 `PointsinkEnabled == false 就整类丢掉`。那条规则有两个毛病：
        ///  1. **会吞掉刚开启后的第一批提示** —— 登录器缓存的开关键是上一次 10 秒轮询拿到的，
        ///     管理员刚打开泡点的那一小段时间里它还是 false，这期间发出去的点券永远不弹；
        ///  2. 判据是"开关"而不是"事件新不新"，而它真正想防的是"登录器重启后回放历史提示"。
        /// 改成按时间窗判断后：老事件照样不弹（重启不回放），新事件一律弹（与开关无关）。
        /// 时钟偏差容忍度就是这个窗口本身（10 分钟足够覆盖常见偏差）。
        /// </summary>
        private static readonly TimeSpan GmEventFreshWindow = TimeSpan.FromMinutes(10);
        /// <summary>限流日志用：公告长轮询最近一次失败的时刻。</summary>
        private DateTime _lastAnnounceFailureLogAt;
        /// <summary>限流日志用：GM 事件短轮询最近一次失败的时刻。</summary>
        private DateTime _gmPollFailureLogAt;
        /// <summary>本次会话已经记过几条"到账提示已写私有槽"（只记前几条，避免刷屏）。</summary>
        private int _privNotifyLogged;
        private CancellationTokenSource? _announceCts;          // 全服公告长轮询取消源
        private long _lastAnnounceId;                           // 公告游标（全局自增 ID）

        // 更新维护：长轮询服务端维护状态（免 ticket —— 登录界面还没登录时也必须能收到）
        private CancellationTokenSource? _maintCts;             // 维护长轮询取消源
        private bool _maintLoopStarted;                         // 防止重复启动
        private bool _maintEnabled;                             // 服务端当前是否维护中
        private bool _maintAllowed;                             // 主账号是否在维护白名单里
        // 副账号（双开）的白名单状态：白名单是按**账号名**匹配的，
        // 主账号是管理员不代表副账号也能进（线上反馈过这个洞）。
        private bool _maintAllowedSecondary;
        private DispatcherTimer? _maintAccountTimer;            // 账号名变化后复查白名单的防抖定时器

        // 双开（多开）后台开关：false = 服务器已关闭双开（勾选框与副账号按钮置灰、硬拦）。
        //
        // ★ 三态语义：只有"服务端明确说 false"才会把它置成 false ★
        //   初值 true = 允许，也就是改造前的现有行为；自检没跑到、请求失败、老运维工具
        //   没有这个端点、后台从未设置过该开关 —— 全部保持 true（保持允许双开）。
        //   状态来源与降级口径见 Services/RemoteAuthService.cs 的 MultiOpenEnabled。
        //
        // ⚠ 关闭只是"以后不许再开"：**绝不**结束任何已在运行的副账号游戏进程
        //   （与维护是两回事 —— 维护会 GameProcessManager.KillAll()，这里一个进程都不碰）。
        private bool _multiOpenEnabled = true;
        /// <summary>关闭双开时给玩家看的原因（勾选框 ToolTip / Toast / 弹窗统一用它，避免多处文案漂移）。</summary>
        private const string MultiOpenDisabledText = "服务器已关闭双开，如需开启请联系管理员";
        /// <summary>关闭双开时副账号登录按钮上的文案。</summary>
        private const string MultiOpenDisabledButtonText = "双开已关闭";
        /// <summary>
        /// 双开开关**实时推送**长轮询（照维护/GM 事件那套）：
        /// 运营在后台一点开关，服务端广播唤醒这条挂起的请求，已打开的登录器立刻变灰/恢复。
        ///
        /// 它只是**补充**，不是替换 —— 原来的三条时机一条都不动：
        ///   ① 启动自检拉一次短端点（MainWindow 自检 + Window_Loaded 应用）；
        ///   ② 副账号登录前复查一次（MultiOpenBlocksSecondaryLoginAsync）；
        ///   ③ 老运维工具根本没有长轮询端点 → 404 → 本条硬停，只剩上面两条（行为同改造前）。
        /// </summary>
        private CancellationTokenSource? _multiOpenCts;         // 双开开关长轮询取消源
        private bool _multiOpenLoopStarted;                     // 防止重复启动（幂等）
        /// <summary>服务端没有 /multi-open/wait（老版运维工具）→ 关掉长轮询，只留自检 + 副登录前复查。</summary>
        private bool _multiOpenWaitUnsupported;
        /// <summary>长轮询失败后的退避时长（与 GmEventWaitBackoffMs 同值）。**必须有**：
        /// 网络错误是秒回的，不退避就会变成每秒几百次的死循环，把认证服务和本机都拖住。</summary>
        private const int MultiOpenWaitBackoffMs = 5000;
        /// <summary>限流日志用：双开长轮询最近一次失败的时刻。</summary>
        private DateTime _multiOpenWaitFailureLogAt;

        // 服务器公告栏（登录页左侧公告板，管理工具「服务器公告」维护）
        private CancellationTokenSource? _noticeCts;            // 公告刷新循环取消源
        private bool _noticeLoopStarted;                        // 防止重复启动
        private const double NoticePanelFullWidth = 520;         // 无副登录框时的宽度
        private const double NoticePanelNarrowWidth = 300;       // 双开（副登录框可见）时收窄，避免顶到左侧版本面板
        // 公告板的水平微调（RenderTransform.X，不参与布局）：取值必须保证公告板左边缘
        // 不被窗口裁掉——双开时布局槽起点只有 244，所以平移量不能超过 -244（见 UpdateNoticePanelWidth）。
        private const double NoticePanelFullShiftX = -320;       // 无双开：槽起点 358，-320 安全
        private const double NoticePanelNarrowShiftX = -200;     // 双开：槽起点 244，-200 留 44px 余量
        private static readonly TimeSpan NoticeRefreshInterval = TimeSpan.FromMinutes(10);

#if EXTERNAL
        // 服务端指定 DLL：本次启动前的校验结果（校验通过的会在进程起来后注入）。
        private List<Services.ServerDllLoader.CheckResult> _serverDllChecks = new();
#endif

        // ── PVF 自动更新（PVFUPDATE）────────────────────────────────────────────
        // 免费版与外网版都要编这段：它只依赖 /launcher/v1/pvf/latest（免票接口），
        // 跟"免费版登录器 ↔ 免费版运维工具 / 外网版 ↔ 赞助版"的身份互斥（X-Client-Edition）
        // 没有任何关系。挂在 EXTERNAL 上会导致：用免费版登录器（服务端唯一放行的身份）
        // 时整段被裁掉，玩家永远看不到客户端更新提示 —— 2026-10-17 就是这么踩的。
        private Services.PvfLatestInfo? _pvfLatest;            // 服务端最新发布信息（enabled=true 且已发布时非空）
        private bool _pvfEnabled;                               // 服务端 PVF 校验总开关
        private bool _pvfUpdateRequired;                        // 本机 Script.pvf 落后于服务端最新发布，需要更新
        private bool _pvfDownloadRequired;                      // 本机无客户端（DNF.exe）且服务端提供整包直链，需要下载完整客户端
        private bool _pvfChecking;                              // 自检进行中（防并发）
        private bool _pvfBusy;                                  // 更新流程进行中（下载/解压）
        private DispatcherTimer? _pvfPollTimer;                 // 30 秒后台轮询

        // ── 版本切换加载动画 ────────────────────────────────────
        private readonly DoubleAnimation _versionLoadingAnim = new(0, 360, TimeSpan.FromMilliseconds(1200))
        {
            RepeatBehavior = RepeatBehavior.Forever
        };
        private int _versionSwitchPending; // 并发切换计数，归零才隐藏转圈
        private bool _versionConfirmLock;  // 版本切换确认防重入：还原选中期间置 true，避免递归弹窗

        public LoginWindow()
        {
            App.LogStartup("LoginWindow: 构造函数开始");
            // 事件起始时刻：取代以前那个跨会话持久化的 id 游标（原因见 _gmEventSince 注释）。
            // 回拨一个余量容忍时钟偏差；真正的"历史回放"由 IsFreshGmEvent 再兜一层。
            _gmEventSince = DateTime.Now.Subtract(GmEventSinceMargin)
                .ToString("yyyy-MM-ddTHH:mm:sszzz", System.Globalization.CultureInfo.InvariantCulture);
            InitializeComponent();
            Title = "";
            SetupBackground();

            // 默认选中"开始游戏"页（XAML 不设 IsChecked，避免事件在控件初始化前触发）
            NavGame.IsChecked = true;

            // 2026-10-15 放开：免费版（Free）构建也提供 CDK 兑换页。
            //
            // 原逻辑是 `#if !(EXTERNAL && V90)` → 只有 90 外网版才有入口，免费版在这里把
            // 导航项与页面一起折叠。但服务端那侧本来就带 CDK：dnf90auth 的 redeem.go 没有
            // build tag，/launcher/v1/characters 与 /launcher/v1/redeem 在免费版编译里照样
            // 注册（server.go 的 editionGuard 只校验 X-Client-Edition 与本端一致，免费版登录器
            // 发的正是 free，能过闸），redeem_codes / redeem_redemptions 两张表也已由
            // EnsureRedeemSchema 在 dnf_local 里建好。所以 CDK 不需要改服务端、不需要重编
            // 运维工具，缺的只是登录器这层 UI 闸门。
            //
            // 注意不要反过来用 `-p:Edition=Ext90` 编"90 外网版"登录器：那会把
            // X-Client-Edition 变成 paid，而本机运维工具是免费版，全部 /launcher/v1/* 会直接
            // 403 edition-mismatch（连登录都进不去）。
            //
            // S4 系列（A21/A12）没有 90 的 ticket 兑换链路，仍在 Nav_Checked 里折叠。

            // 进程托管：游戏退出恢复登录状态、全部退出弹回界面
            GameProcessManager.GameExited += OnGameExited;
            GameProcessManager.AllGamesExited += OnAllGamesExited;

#if PVFUPDATE
            // 客户端路径配置变化 → 立即做一次 PVF 更新自检（未配置时静默跳过）
            ConfigPage.ClientPathChanged += OnClientPathChanged;
#endif
            // 更新维护：账号名变化后复查白名单（白名单里的管理员在维护期间照样能登录，
            // 而"要不要点亮按钮"取决于当前填的账号名，所以必须在输入时复查）。
            if (AccountTextBox is not null)
            {
                AccountTextBox.TextChanged += OnMaintenanceAccountChanged;
            }
            if (SecondaryAccountTextBox is not null)
            {
                SecondaryAccountTextBox.TextChanged += OnMaintenanceAccountChanged;
            }
            _maintAccountTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(700) };
            _maintAccountTimer.Tick += MaintenanceAccountTimer_Tick;
#if EXTERNAL
            // 外网版（M3）：构造末尾执行 UI 收敛 + 锁版本配套（隐藏版本切换/GM/DLL 入口与页）
            ApplyExternalUiLock();
#endif
            App.LogStartup("LoginWindow: 构造函数完成");
        }

        private void Window_Loaded(object sender, RoutedEventArgs e)
        {
            App.LogStartup("LoginWindow: Window_Loaded 触发");

            // 禁用 AduWindow 标题栏命中测试，避免顶部按钮点不到
            if (GetTemplateChild("PART_BorderTop") is FrameworkElement borderTop)
            {
                borderTop.IsHitTestVisible = false;
            }

            // 2026-10-15：版本选择面板已隐藏（只保留第一个版本），这里不再"恢复上次使用的版本"，
            // 而是固定切到 profile 列表里的第一个（默认 90），避免上次选过其它版本后界面无入口可切回。
            var profiles = AppConfig.Settings.Profiles;
            var firstProfileId = (profiles != null && profiles.Count > 0 && !string.IsNullOrWhiteSpace(profiles[0].Id))
                ? profiles[0].Id
                : "90";
            AppConfig.SwitchProfile(firstProfileId);
            // IsChecked=true 触发 Checked 事件刷新全 UI
            SelectVersion(firstProfileId);

            // ── 2026-10-15：固定默认配置（"游戏配置"页已隐藏，玩家不需要也无法修改）──
            // 客户端目录默认 = 登录器所在目录下的 DNF.exe（例如 D:\dof\90cn\90server\DNF\DNF.exe），
            // 找不到再退到上级目录里的 DNF\ 或 地下城与勇士\；登录模式固定远程模式、远程 IP 固定。
            try
            {
                var launcherDir = AppDomain.CurrentDomain.BaseDirectory.TrimEnd('\\', '/');
                var candidates = new List<string>
                {
                    System.IO.Path.Combine(launcherDir, "DNF.exe"),
                    System.IO.Path.Combine(launcherDir, "..", "DNF", "DNF.exe"),
                    System.IO.Path.Combine(launcherDir, "..", "地下城与勇士", "DNF.exe"),
                };

                string detectedClient = null;
                foreach (var candidate in candidates)
                {
                    var full = System.IO.Path.GetFullPath(candidate);
                    if (System.IO.File.Exists(full))
                    {
                        detectedClient = full;
                        break;
                    }
                }

                var settings = AppConfig.Settings;
                var currentProfile = AppConfig.CurrentProfile;

                if (detectedClient != null)
                {
                    settings.GamePath = detectedClient;
                    currentProfile.GamePath = detectedClient;
                }
                else if (string.IsNullOrWhiteSpace(settings.GamePath))
                {
                    settings.GamePath = System.IO.Path.GetFullPath(candidates[0]);
                    currentProfile.GamePath = settings.GamePath;
                }

                settings.LoginMode = BuildParameters.LoginMode;
                settings.RemoteIp = BuildParameters.RemoteIp;
                settings.RemotePort = BuildParameters.RemotePort;
                currentProfile.LoginMode = settings.LoginMode;
                currentProfile.RemoteIp = settings.RemoteIp;
                currentProfile.RemotePort = settings.RemotePort;
                AppConfig.Save();
                App.LogStartup($"默认配置已固定：client={settings.GamePath} mode={settings.LoginMode} ip={settings.RemoteIp}:{settings.RemotePort}");
            }
            catch (Exception ex)
            {
                App.LogStartup("固定默认配置失败：" + ex.Message);
            }

            // LYPlugins：登录器同目录下该文件夹里的 DLL 默认全部勾选注入（DLL 管理页已隐藏，不再需要手动选）
            DllManageControl.EnsureDefaultPluginDlls();

            RestoreSavedLogin();
            _ = CheckOnlineAsync();
            StartOnlineTimer();
            // 更新维护长轮询：一打开就挂上（免 ticket），维护开始/结束都能秒级生效
            StartMaintenanceLoopIfNeeded();
            // 双开（多开）后台开关：按启动自检阶段拿到的状态刷一次 UI（不弹 Toast）。
            // ★ 三态降级：MultiOpenEnabled == null（自检没跑到/失败/老运维工具/后台没设置过）
            //   时按"允许"处理 —— 保持允许双开，绝不把"没拉到"当成"关闭"。
            _ = ApplyMultiOpenStateAsync(Services.RemoteAuthService.MultiOpenEnabled ?? true, notify: false);
            // 双开开关长轮询（免 ticket）：后台一关，已经开着的登录器**立刻**变灰。
            // 只是补充：上面的自检初值与"副登录前复查"一条都不动（老运维工具没有长轮询端点）。
            StartMultiOpenLoopIfNeeded();
            // GM 事件长轮询：已登录槽位的到账/踢人事件实时推送（兜底轮询仍保留）
            StartGmEventLoopIfNeeded();
            // 服务器公告栏：先用自检阶段拉到的内容渲染，之后每 10 分钟刷新一次
            StartNoticeLoopIfNeeded();

            // 启动自检结果处理：缺失配置时自动跳设置页 + Toast 提示
            HandleStartupCheckAfterLoad();

#if PVFUPDATE
            // PVF 自动更新：启动自检一次 + 30 秒后台轮询（90 远程且已配置路径时生效，其余静默）
            StartPvfAutoUpdate();
#endif
            App.LogStartup("LoginWindow: Window_Loaded 处理完成");
        }

        protected override void OnSourceInitialized(EventArgs e)
        {
            base.OnSourceInitialized(e);
            _hwndSource = (HwndSource?)PresentationSource.FromVisual(this);
            _hwndSource?.AddHook(WndProc);
        }

        /// <summary>
        /// AduWindow 在 Win32 层把顶部区域判为标题栏（HT_CAPTION），拦截发生在 WPF 命中测试之前，
        /// 导致顶部自定义按钮"不跟手"。这里在按钮区域强制返回 HTCLIENT，把点击交还给 WPF 层。
        /// </summary>
        private IntPtr WndProc(IntPtr hwnd, int msg, IntPtr wParam, IntPtr lParam, ref bool handled)
        {
            // 顶部按钮命中测试
            if (msg != WM_NCHITTEST || _hwndSource is null)
            {
                return IntPtr.Zero;
            }

            int screenX = (short)(lParam.ToInt32() & 0xFFFF);
            int screenY = (short)((lParam.ToInt32() >> 16) & 0xFFFF);
            var clientPoint = PointFromScreen(new System.Windows.Point(screenX, screenY));

            foreach (var btn in new System.Windows.Controls.Button[] { MinimizeButton, CloseButton })
            {
                if (btn is null || !btn.IsVisible)
                {
                    continue;
                }

                var origin = btn.TranslatePoint(new System.Windows.Point(0, 0), this);
                var rect = new System.Windows.Rect(origin.X, origin.Y, btn.ActualWidth, btn.ActualHeight);
                rect.Inflate(3, 3);

                if (rect.Contains(clientPoint))
                {
                    handled = true;
                    return new IntPtr(HTCLIENT);
                }
            }

            return IntPtr.Zero;
        }

        protected override void OnContentRendered(EventArgs e)
        {
            base.OnContentRendered(e);
            App.LogStartup("LoginWindow: OnContentRendered 触发（_hasShown=" + _hasShown + "）");

            if (_hasShown)
            {
                return;
            }

            _hasShown = true;
            FadeInWindow();
            App.LogStartup("LoginWindow: FadeIn 淡入已启动");
        }

        protected override void OnClosing(CancelEventArgs e)
        {
            App.LogStartup("LoginWindow: OnClosing（_forceClose=" + _forceClose + "，CloseAction=" + AppConfig.CloseAction + "）");

            if (!_forceClose && AppConfig.CloseAction == CloseAction.MinimizeToTray)
            {
                // 拦截关闭，隐藏到系统托盘
                e.Cancel = true;
                HideToTray();
                App.LogStartup("LoginWindow: OnClosing 已拦截，最小化到托盘");
                return;
            }

            // 真正退出：结束所有托管的 DNF.exe 进程 + 清理托盘图标
            _bgTimer?.Stop();
            _announceCts?.Cancel();
            _maintCts?.Cancel();
            _gmEventCts?.Cancel();
            _multiOpenCts?.Cancel();
            _maintAccountTimer?.Stop();
            _noticeCts?.Cancel();

            // TEMP-8BOX：先关掉 3~8 号临时登录框。
            // 它们刻意不设 Owner（HideToTray 时不该跟着隐藏），所以主窗关闭**不会**带走它们；
            // 而 App 用的是默认 ShutdownMode=OnLastWindowClose，漏了这一步就会出现
            // "托盘和主窗都没了、进程还活着，只剩 6 个临时框要求逐个关闭"的假死状态。
            CloseTempExtraLoginWindows();

            // 关窗兜底保存设置页状态（改动本身已即时保存）
            SettingsPage.SaveState();

            GameProcessManager.KillAll();
            DisposeTrayIcon();

            // 退出前询问是否关闭本地服务端（本地模式且服务端正在运行时）
            TryPromptStopServerOnExit();

            base.OnClosing(e);
        }

        /// <summary>
        /// 退出登录器时，若本地服务端正在运行则询问是否一并关闭。
        /// 仅本地模式且已配置服务端根目录时生效；远程模式不管理本地服务端。
        /// </summary>
        private void TryPromptStopServerOnExit()
        {
            if (AppConfig.Mode != LoginMode.Local)
            {
                return;
            }

            var root = AppConfig.Settings.ServerRootPath;
            if (string.IsNullOrWhiteSpace(root) || !Services.ServerManager.ServerRunning)
            {
                return;
            }

            var confirm = MessageBox.Show(
                "本地服务端正在运行，退出登录器时是否同时关闭服务端？",
                "退出确认",
                MessageBoxButton.YesNo,
                MessageBoxImage.Question,
                MessageBoxResult.No);
            if (confirm != MessageBoxResult.Yes)
            {
                return;
            }

            Services.ServerManager.Stop(root, _ => { });
        }

        private void MinimizeButton_Click(object sender, RoutedEventArgs e)
        {
            WindowState = WindowState.Minimized;
        }

        private void CloseButton_Click(object sender, RoutedEventArgs e)
        {
            // 已记住关闭行为（CloseActionAsked=1）：直接按行为执行（OnClosing 按 CloseAction 拦截到托盘或退出）
            if (AppConfig.Settings.CloseActionAsked == 1)
            {
                Close();
                return;
            }

            // 未询问过（0/缺失）：弹窗询问 关闭登录器 / 最小化到托盘，可勾选记住并写入 JSON
            ShowCloseActionDialog();
        }

        /// <summary>
        /// 首次点击右上角 X 的询问弹窗：选择关闭行为，可勾选"记住此选项，以后不再询问"。
        /// 弹窗仅收集选择，勾选记住时同步写入 JSON（CloseActionAsked=1 后不再询问，选择存于 CloseAction）。
        /// </summary>
        private void ShowCloseActionDialog()
        {
            var dlg = new CloseActionDialog { Owner = this };
            if (dlg.ShowDialog() != true)
            {
                return;
            }

            if (dlg.Remember)
            {
                AppConfig.Settings.CloseAction = dlg.SelectedAction;
                AppConfig.Settings.CloseActionAsked = 1;
                AppConfig.Save();
            }

            if (dlg.SelectedAction == CloseAction.Exit.ToString())
            {
                // 真正退出：结束托管 DNF 进程等由 OnClosing 处理
                _forceClose = true;
                Close();
            }
            else
            {
                HideToTray();
            }
        }

        // ── 顶部导航：5 页常驻内存，Visibility 切换 ─────────────

#if EXTERNAL
        /// <summary>
        /// 外网版 UI 收敛（M3，仅 EXTERNAL 编译）：折叠版本切换面板（版本标签 2026-10-15 已整块移除，
        /// 登录框上方只保留"登录游戏"标题）、GM 工具入口、DLL 管理入口与对应内容页；
        /// 登录模式只读展示"远程模式"（AppConfig 已锁 Remote）。Free 版不编译，行为零变化。
        /// </summary>
        private void ApplyExternalUiLock()
        {
            VersionPanel.Visibility = Visibility.Collapsed;
            NavGm.Visibility = Visibility.Collapsed;
            NavDll.Visibility = Visibility.Collapsed;
            GmPage.Visibility = Visibility.Collapsed;
            S4GmPage.Visibility = Visibility.Collapsed;
            DllPage.Visibility = Visibility.Collapsed;

            // ASSIST-OFF-EXTERNAL（2026-09-20）：游戏助手页在外网版整页停用。
            // 原因：该功能当前有问题（待修），外网版先不给玩家用。这里只做 UI 折叠，
            // 功能侧还有三处配套闸门（App 启动连发、游戏助手控件初始化、
            // 启动游戏时强制把顺图/拾取门控写 0）—— 四处合起来才算停用：只折叠 UI 不够，
            // 配置是四个版本共享的 %APPDATA%\DNFLogin\config.json，开关残留照样会生效。
            // ⚠ 注意：Helper DLL（DNFHelper）本身**必须照常注入**，它同时承担外网版的
            //   PVF 换钥补丁，跟着一起关会导致客户端解不开加密 PVF（2026-09-20 踩过，已回退）。
            // 恢复步骤：删掉本段 + 用 grep 搜 ASSIST-OFF-EXTERNAL 把其余闸门一起删掉。
            NavAssistant.Visibility = Visibility.Collapsed;
            AssistantPage.Visibility = Visibility.Collapsed;

            ModeStatusText.Text = "远程模式";
        }
#endif

        /// <summary>
        /// 顶部导航选中切换：显示对应页 + 玻璃层显隐 + 背景进度条仅开始游戏页显示。
        /// 切到游戏配置页时重读配置并刷新在线状态（原设置窗口关闭后的行为）。
        /// </summary>
        private void Nav_Checked(object sender, RoutedEventArgs e)
        {
            if (sender is not RadioButton rb)
            {
                return;
            }

            // 2026-10-15：GM 工具 / 游戏配置 / DLL 管理 三个入口已隐藏（本版不开放这三页），
            // 这里兜底拦截任何编程性选中（例如"客户端没配置时自动跳游戏配置页"），
            // 避免对应页面被非入口路径打开。
            if (ReferenceEquals(rb, NavGm) || ReferenceEquals(rb, NavConfig) || ReferenceEquals(rb, NavDll))
            {
                NavGame.IsChecked = true;
                return;
            }

#if EXTERNAL
            // 外网版（M3）：GM 工具入口与 DLL 管理入口运行期隐藏（ApplyExternalUiLock 折叠），
            // 此处兜底拦截任何编程性选中，避免 GM/DLL 页闪现或被非入口路径打开。
            // ASSIST-OFF-EXTERNAL：游戏助手页同样在外网版停用（入口已在 ApplyExternalUiLock 折叠），
            // 这里一并兜底拦截，防止编程性选中让该页闪出来。恢复时删掉 NavAssistant 这个判断即可。
            if (ReferenceEquals(rb, NavGm) || ReferenceEquals(rb, NavDll) || ReferenceEquals(rb, NavAssistant))
            {
                NavGame.IsChecked = true;
                return;
            }
#else
            // GM 工具入口限制：A12/A21 版开发中暂不开放；90 版仅本地模式可用（远程模式待重做）
            if (ReferenceEquals(rb, NavGm))
            {
                var curProfileId = AppConfig.CurrentProfile?.Id ?? "90";
                if (Services.ServerManager.IsS4Profile(curProfileId))
                {
                    MessageBox.Show("GM 工具开发中，暂不开放。", "GM 工具", MessageBoxButton.OK, MessageBoxImage.Information);
                    NavGame.IsChecked = true;
                    return;
                }

                if (AppConfig.Mode == LoginMode.Remote)
                {
                    MessageBox.Show("GM 工具仅本地模式可用，90 版远程模式暂不开放。", "GM 工具", MessageBoxButton.OK, MessageBoxImage.Information);
                    NavGame.IsChecked = true;
                    return;
                }
            }
#endif

            var isGamePage = ReferenceEquals(rb, NavGame);

            PageLogin.Visibility = isGamePage ? Visibility.Visible : Visibility.Collapsed;
            ConfigPage.Visibility = ReferenceEquals(rb, NavConfig) ? Visibility.Visible : Visibility.Collapsed;
            AssistantPage.Visibility = ReferenceEquals(rb, NavAssistant) ? Visibility.Visible : Visibility.Collapsed;
            DllPage.Visibility = ReferenceEquals(rb, NavDll) ? Visibility.Visible : Visibility.Collapsed;
            var gmVisible = ReferenceEquals(rb, NavGm);
            var isS4 = Services.ServerManager.IsS4Profile(AppConfig.Settings.CurrentProfileId);
            GmPage.Visibility = gmVisible && !isS4 ? Visibility.Visible : Visibility.Collapsed;
            S4GmPage.Visibility = gmVisible && isS4 ? Visibility.Visible : Visibility.Collapsed;
            SettingsPage.Visibility = ReferenceEquals(rb, NavSettings) ? Visibility.Visible : Visibility.Collapsed;
            // CDK 页只对 90 版本开放（S4A21/S4A12 走 S4Gm 链路，没有 ticket 兑换接口）：
            // 非 90 版本把导航入口一起折叠，避免点进去是一片空页。
            var cdkSupported = !isS4;
            if (!cdkSupported && ReferenceEquals(rb, NavCdk))
            {
                NavGame.IsChecked = true;
                return;
            }
            NavCdk.Visibility = cdkSupported ? Visibility.Visible : Visibility.Collapsed;
            CdkPage.Visibility = cdkSupported && ReferenceEquals(rb, NavCdk) ? Visibility.Visible : Visibility.Collapsed;

            // 切入 CDK 页时注入当前已登录会话快照（内部自动刷新角色列表）。
            // 原来只在 90 外网版编译（#if EXTERNAL && V90），免费版放开 CDK 后同样需要这一句，
            // 否则页面拿不到 ticket，玩家只能走"手动登录"那条路。
            if (ReferenceEquals(rb, NavCdk))
            {
                CdkPage.PushSessions(BuildCdkSessionSnapshot());
            }

            // 玻璃层：仅非开始游戏页铺满导航条下方
            GlassOverlay.Visibility = isGamePage ? Visibility.Collapsed : Visibility.Visible;

            // 背景轮换进度条：仅开始游戏页显示
            if (isGamePage)
            {
                ShowBgProgress();
            }
            else
            {
                HideBgProgress();
            }

            // 切到开始游戏页/游戏配置页时即时刷新在线状态：
            // 配置页打开需重读磁盘配置（设置可能已改动）；切回开始游戏页补一次即时刷新，
            // 避免状态停留在旧值、要再切一次页才更新。
            if (ReferenceEquals(rb, NavConfig))
            {
                AppConfig.Reload();
            }
            if (isGamePage || ReferenceEquals(rb, NavConfig))
            {
                _ = CheckOnlineAsync();
            }

#if !EXTERNAL
            // GM 工具页打开时从服务端配置联动刷新连接信息（初始化/启动后自动生效）。
            // 外网版（EXTERNAL）GM 页不可达且入口已隐藏，此段不编译。
            if (ReferenceEquals(rb, NavGm))
            {
                if (isS4)
                {
                    S4GmPage.ReloadEnvironment();
                }
                else
                {
                    GmPage.SyncFromLoginConfig();
                }
            }
#endif
        }

        /// <summary>
        /// 构建 CDK 页会话快照：来自登录成功保存的 _remoteSessions（Slot/Ticket/AccountId/Username）。
        /// 原来整段包在 `#if EXTERNAL && V90` 里，免费版放开 CDK 后去掉闸门 —— 依赖的
        /// SnapshotRemoteSessions 和 _remoteSessions 本身都不带条件编译，免费版可用。
        /// </summary>
        private List<CdkRedeemControl.CdkSessionInfo> BuildCdkSessionSnapshot()
        {
            var list = new List<CdkRedeemControl.CdkSessionInfo>();
            foreach (var s in SnapshotRemoteSessions())
            {
                if (string.IsNullOrWhiteSpace(s.Ticket))
                {
                    continue;
                }

                list.Add(new CdkRedeemControl.CdkSessionInfo(
                    s.Slot,
                    s.Ticket,
                    s.AccountId ?? string.Empty,
                    s.Username ?? string.Empty));
            }

            return list;
        }

        /// <summary>按版本 Id 选中对应版本 RadioButton（不存在则回退 90）</summary>
        private void SelectVersion(string id)
        {
            var target = id switch
            {
                "S4A21" => VersionRadioS4A21,
                "S4A12" => VersionRadioS4A12,
                _ => VersionRadio90
            };

            if (target is not null && target.IsChecked != true)
            {
                target.IsChecked = true;
            }
        }

        // 版本切换：切换当前版本配置（旧版本顶层字段镜像回 profile，新版本加载到顶层），
        // 90 完整表单；S4A21/S4A12 本地模式表单可用，远程模式置灰"正在开发"
        private void VersionRadio_Checked(object sender, RoutedEventArgs e)
        {
            // ★ 判空哨兵，别删 ★
            // XAML 里 VersionRadio90 带 IsChecked="True"，会在 InitializeComponent() 解析
            // XAML 期间就触发本回调；这时窗口里位置靠后的控件（LoginFormPanel、NavGame…）
            // 还没被创建，全是 null。少了这道判断，New LoginWindow() 会直接抛
            // NullReferenceException，外层启动流程卡在加载界面（表现为"双击后加载到 100%
            // 就不动了"）。2026-10-15 踩过一次：当时的哨兵是 LoginVersionText，删掉那个
            // 版本标签时一起删了。现在改用本方法真正要用的 LoginFormPanel 当哨兵。
            if (sender is not RadioButton rb || LoginFormPanel is null)
            {
                return;
            }

            // 点"否"还原选中期间会再次触发本事件：此时仅恢复 UI 选中态，
            // 不做任何切换/清理副作用（游戏进程与登录态保持不变）
            if (_versionConfirmLock)
            {
                return;
            }

            var oldId = AppConfig.Settings.CurrentProfileId;
            var id = rb.Content?.ToString() switch
            {
                "S4A21" => "S4A21",
                "S4A12" => "S4A12",
                _ => "90"
            };

            // 兜底：游戏运行时禁止直接切换版本（会强杀进程导致"闪退"）。
            // 用户确认继续才放行；取消则还原原选中版本，且不进入后续切换逻辑。
            if (!_versionConfirmLock && GameProcessManager.AnyRunning && !string.Equals(oldId, id, StringComparison.Ordinal))
            {
                var confirm = MessageBox.Show(
                    "当前有游戏正在运行，切换版本将强制结束游戏进程。是否继续？",
                    "版本切换确认",
                    MessageBoxButton.YesNo,
                    MessageBoxImage.Warning,
                    MessageBoxResult.No);
                if (confirm != MessageBoxResult.Yes)
                {
                    _versionConfirmLock = true;
                    try
                    {
                        SelectVersion(oldId);
                    }
                    finally
                    {
                        _versionConfirmLock = false;
                    }
                    return;
                }
            }

            // 配置层：切换 CurrentProfileId，并镜像顶层字段（老代码统一读顶层，零改动按版本生效）
            AppConfig.SwitchProfile(id);
            var is90 = id == "90";

            // 三个版本均显示登录表单，A12/A21 与 90 布局一致（无额外提示条）
            LoginFormPanel.Visibility = Visibility.Visible;

            // 双开仅 90 版本支持：A12/A21 隐藏双开勾选与副登录框
            if (!is90)
            {
                DualCheckBox.IsChecked = false;
                SecondaryLoginPanel.Visibility = Visibility.Collapsed;
                // 副登录框被收起 → 公告板恢复原宽（否则切版本后会一直窄着）
                UpdateNoticePanelWidth();
            }
            DualCheckBox.Visibility = is90 ? Visibility.Visible : Visibility.Collapsed;
            // 忘记密码（救援代码找回）仅 90 版本支持：S4 系列（A12/A21）隐藏入口
            ForgotPasswordButton.Visibility = is90 ? Visibility.Visible : Visibility.Collapsed;

#if EXTERNAL
            // 外网版（M3）：GM 工具入口与 GM 内容页固定隐藏（入口折叠在 ApplyExternalUiLock），
            // 此处切换版本后双保险收起，避免绕过入口直接看到 GM 内容。
            GmPage.Visibility = Visibility.Collapsed;
            S4GmPage.Visibility = Visibility.Collapsed;
            if (NavGm.IsChecked == true)
            {
                NavGame.IsChecked = true;
            }
#else
            // GM 工具入口限制（90 版仅本地模式可用，远程模式待重做；A12/A21 版开发中暂不开放）：
            // 导航入口保持可见（点击拦截与提示在 Nav_Checked）；此处切换版本/模式后若停留在
            // GM 页且新版本不满足条件则自动收起，避免绕过入口直接看到 GM 内容。
            var gmAllowed = is90 && AppConfig.Mode != LoginMode.Remote;
            NavGm.Visibility = Visibility.Visible;
            if (!gmAllowed)
            {
                GmPage.Visibility = Visibility.Collapsed;
                S4GmPage.Visibility = Visibility.Collapsed;
                if (NavGm.IsChecked == true)
                {
                    NavGame.IsChecked = true;
                }
            }
            else if (S4GmPage.Visibility == Visibility.Visible)
            {
                S4GmPage.Visibility = Visibility.Collapsed;
                GmPage.Visibility = Visibility.Visible;
            }
#endif

            // 游戏配置页跟随当前版本刷新（服务端/路径/服务器配置按版本隔离；探测已移后台线程）
            var refreshTask = ConfigPage.RefreshFromProfileAsync();

            // 游戏助手页跟随当前版本刷新（DPS/顺图/拾取版本门控提示与置灰状态同步）
            AssistantPage.RefreshForVersion();

            // 已登录状态跨版本无效：切换版本后清空登录态，避免旧版本进程与新版混淆
            if (_mainLoggedIn || _secondaryLoggedIn)
            {
                GameProcessManager.KillAll();
                _mainLoggedIn = false;
                _secondaryLoggedIn = false;
                MainLoggedInText.Visibility = Visibility.Collapsed;
                SecondaryLoggedInText.Visibility = Visibility.Collapsed;
                RefreshLoginButtons();
            }

            // 版本切换联动刷新：按新版本独立判定服务端在线状态并刷新登录闸控
            if (OnlineStatusText is not null)
            {
                _ = FinishVersionSwitchAsync(refreshTask);
            }
            else
            {
                _versionSwitchPending = Math.Max(0, _versionSwitchPending - 1);
                if (_versionSwitchPending == 0)
                {
                    HideVersionLoading();
                }
            }
        }

        // ── 版本切换转圈：异步在线检测期间显示加载动画，全部完成才隐藏 ──

        private void ShowVersionLoading()
        {
            if (!IsLoaded)
            {
                return;
            }
            VersionLoadingOverlay.Visibility = Visibility.Visible;
            VersionLoadingRotate.BeginAnimation(RotateTransform.AngleProperty, _versionLoadingAnim);
        }

        private void HideVersionLoading()
        {
            VersionLoadingOverlay.Visibility = Visibility.Collapsed;
            VersionLoadingRotate.BeginAnimation(RotateTransform.AngleProperty, null);
        }

        private async Task FinishVersionSwitchAsync(Task? extra = null)
        {
            try
            {
                if (extra is not null)
                {
                    await extra;
                }
                await CheckOnlineAsync();
            }
            finally
            {
                _versionSwitchPending = Math.Max(0, _versionSwitchPending - 1);
                if (_versionSwitchPending == 0)
                {
                    HideVersionLoading();
                }
            }
        }


        // ── 启动回填与在线检测 ─────────────────────────────────

        /// <summary>
        /// 记住密码勾选时启动自动回填主框账号密码（凭据管理器按账号名保存）
        /// </summary>
        private void RestoreSavedLogin()
        {
            var lastAccount = AppConfig.Settings.GetLastAccount(AppConfig.CurrentProfile.Id);
            if (string.IsNullOrWhiteSpace(lastAccount))
            {
                return;
            }

            if (CredentialManagerService.TryRead(AppConfig.CurrentProfile.Id, lastAccount, out var savedPassword))
            {
                AccountTextBox.Text = lastAccount;
                PasswordBox.Password = savedPassword;
                RememberCheckBox.IsChecked = true;
            }
        }

        /// <summary>
        /// 远程转发器端口探测：TCP 可连即视为开放。
        /// 公网场景超时放宽到 3 秒（本地探测 300ms 对公网 RTT 过短，易误判离线）。
        /// </summary>
        private static bool ProbeRemotePort(string host, int port)
        {
            try
            {
                using var client = new System.Net.Sockets.TcpClient();
                var connect = client.ConnectAsync(host, port);
                return connect.Wait(TimeSpan.FromSeconds(3)) && client.Connected;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>每 10 秒轮询服务器状态，刷新在线/离线与人数；切页另有即时刷新兜底</summary>
        private void StartOnlineTimer()
        {
            _onlineTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(10) };
            _onlineTimer.Tick += async (_, _) => await CheckOnlineAsync();
            _onlineTimer.Start();
        }

        private async Task CheckOnlineAsync()
        {
            var isRemote = AppConfig.Mode == LoginMode.Remote;

            bool online;
            string statusText;

            if (isRemote)
            {
                var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                    ? AppConfig.Settings.RemoteIp.Trim()
                    : AppConfig.Settings.Host;

                var profileId = AppConfig.CurrentProfile?.Id ?? "90";
                if (Services.ServerManager.IsS4Profile(profileId))
                {
                    // 远程模式（S4 直连）：按版本探测探针口判定在线 —— A21 探 18888 / A12 探 16666，
                    // 探针口由 S4OpsTool 在服务端启动成功后开放（两版本 DfoServer 共用 7001 频道口，
                    // 仅探 7001 无法区分 A12/A21，同服任一启动两版都会误判在线）；
                    // 7002 认证是否就绪留给登录动作校验，不参与在线状态判定
                    var probePort = Services.ServerManager.S4ProbePortForProfile(profileId);
                    if (probePort <= 0)
                    {
                        probePort = Services.ServerManager.S4ChannelPort; // 兜底：识别不到版本回退探 7001
                    }
                    var sw = System.Diagnostics.Stopwatch.StartNew();
                    var ok = await Task.Run(() =>
                        ProbeRemotePort(host, probePort));
                    sw.Stop();
                    online = ok && !string.IsNullOrWhiteSpace(host);
                    statusText = online ? $"在线 · {sw.ElapsedMilliseconds}ms" : "离线";
                }
                else
                {
                    // 90 远程：探测认证 API（18112）TCP 可达即视为在线；账号校验留给登录动作。
                    var authPort = 18112;
                    var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                    if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
                    {
                        authPort = parsedPort;
                    }
                    var sw = System.Diagnostics.Stopwatch.StartNew();
                    var ok = await Task.Run(() => ProbeRemotePort(host, authPort));
                    sw.Stop();
                    online = ok && !string.IsNullOrWhiteSpace(host);
                    statusText = online ? $"在线 · {sw.ElapsedMilliseconds}ms" : "离线";
                    if (online)
                    {
                        // 90 远程：顺带拉取在线人数与上限（运维工具 /launcher/v1/health）；
                        // 免费版显示 “x/8”，付费版 max_online=0（不限制）只显示当前人数。
                        var health = await Services.RemoteAuthService.GetHealthAsync(AppConfig.Settings, host, authPort);
                        if (health.Ok)
                        {
                            // 版本互斥：免费版登录器仅连免费版运维工具，外网版登录器仅连赞助版运维工具；
                            // 服务端强制 403，此处提前提示并阻止登录按钮
                            var mismatchReason = Services.RemoteAuthService.EditionMismatchReason(
                                Services.RemoteAuthService.LastServerEdition);
                            if (!string.IsNullOrWhiteSpace(mismatchReason))
                            {
                                online = false;
                                statusText = "版本不匹配";
                                ShowStatus(mismatchReason, Brushes.OrangeRed);
                            }
                            else
                            {
                                // 2026-10-15：不再展示在线人数/上限，只保留 "在线 · xxms"
                                statusText = $"在线 · {sw.ElapsedMilliseconds}ms";
                            }
                        }

                        // 90 远程已登录：心跳续期 + 轮询 GM 踢人事件（封禁后关闭对应游戏进程）
                        await PollRemoteGmKickAsync(host, authPort);
                    }
                }
            }
            else
            {
                // 本地模式：90 版 = 管理端口 18111 可达；S4 系列 = 频道端口 7001 可达
                // + DfoServer 进程路径匹配当前版本根目录（区分 A12/A21，避免共用 7001 误判）。
                // 90 的 InspectServer 内部会启动 mysql.exe 子进程查询表数量并做端口探测，
                // 放后台线程执行，避免切到配置页时阻塞 UI 造成卡顿。
                var rootPath = AppConfig.Settings.ServerRootPath;
                var profileId = AppConfig.CurrentProfile?.Id ?? "90";
                if (Services.ServerManager.IsS4Profile(profileId))
                {
                    online = await Task.Run(() => Services.ServerManager.S4ServerRunningForRoot(rootPath));
                    statusText = online ? "在线" : "离线";
                }
                else
                {
                    var ins = await Task.Run(() => Services.ServerManager.InspectServer(rootPath));
                    online = ins.AdminPortOpen;
                    if (!online && string.IsNullOrWhiteSpace(rootPath))
                    {
                        // 未配置服务端根目录时，回退探测默认管理端口 18111
                        online = await Task.Run(() => Services.ServerManager.ServerRunning);
                    }
                    statusText = online ? "在线" : "离线";
                }
            }

            _isOnline = online;
            OnlineStatusText.Text = statusText;
            OnlineStatusText.Foreground = new SolidColorBrush(
                online ? Color.FromRgb(0x2E, 0xCC, 0x71) : Color.FromRgb(0xE8, 0x4A, 0x3A));
            OnlineStatusImage.Source = LoadImage(online ? OnlineSuccessUri : OnlineErrorUri);
            ModeStatusText.Text = isRemote ? "远程模式" : "本地模式";
            RefreshLoginButtons();
        }

        /// <summary>返回 90 远程认证服务端点（host, port）；host 为空表示未配置。</summary>
        private static (string Host, int Port) GetRemoteAuthEndpoint()
        {
            var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                ? AppConfig.Settings.RemoteIp.Trim()
                : (AppConfig.Settings.Host ?? "").Trim();
            var port = 18112;
            var cfgPort = AppConfig.CurrentProfile?.RemotePort;
            if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsed))
            {
                port = parsed;
            }
            return (host, port);
        }

        /// <summary>刷新泡点公开信息（失败静默，保留上次 PointsinkEnabled 值）。</summary>
        private async Task RefreshPointsinkAsync(string host, int authPort)
        {
            _ = await Services.RemoteAuthService.GetPointsinkAsync(
                AppConfig.Settings, host, authPort).ConfigureAwait(false);
        }

        /// <summary>
        /// 90 远程在线周期任务：对所有已登录槽位做心跳续期（维持运维工具在线列表），
        /// 并拉取本账号新增 GM 事件；命中 kick 事件说明该账号被管理员封禁，
        /// 只关闭该账号所在槽位的游戏进程（双开不同账号互不影响，同账号双开全部退出）。
        /// </summary>
        private async Task PollRemoteGmKickAsync(string host, int authPort)
        {
            if (RemoteSessionCount() == 0)
            {
                return;
            }

            // 周期性刷新泡点公开信息：后台开启/修改泡点后，游戏内一个轮询周期（≤10s）内实时生效
            await RefreshPointsinkAsync(host, authPort);

            foreach (var session in SnapshotRemoteSessions())
            {
                try
                {
                    // 心跳续期：失败不中断（ticket 过期会在下次登录时重新获取）
                    await Services.RemoteAuthService.HeartbeatAsync(
                        AppConfig.Settings, host, authPort, session.Ticket);

                    var gm = await Services.RemoteAuthService.GetGMEventsAsync(
                        AppConfig.Settings, host, authPort, session.Ticket, GmCursorFor(session.AccountId), _gmEventSince);
                    if (!gm.Ok)
                    {
                        // 同样以前是静默的：ticket 失效会让到账提示**永远不弹**且毫无痕迹。
                        LogThrottled(ref _gmPollFailureLogAt, 60, "GM 事件短轮询失败（" + gm.Error + "）");
                        continue;
                    }

                    ApplyGmEvents(session.AccountId, session.Slot, session.Username, gm.Events);
                }
                catch (Exception)
                {
                    // 单槽轮询失败不阻塞其它槽位，也不打扰用户
                }
            }
        }

        /// <summary>
        /// 处理一批 GM 事件（10 秒兜底轮询与长轮询**共用**这一条处理路径）。
        ///
        /// ── 两个曾经踩过的坑（2026-09-28 修）──────────────────────────────────
        /// 1. **游标必须先判后推、且按账号分开**。旧写法是"先把 _lastGmEventId 推到本批最大值，
        ///    再判断这条要不要处理"，于是任何一次跳过（该槽游戏没在跑、泡点刚好是关的）
        ///    都会把那条事件**永久吃掉**：游标已经越过它了，下次再也查不出来。
        ///    多开时更糟——所有槽位共用一个游标，后轮询的账号会被先轮询的账号推高的游标
        ///    直接盖掉自己的事件。现在每个账号各一个游标，且只对"真的要处理"的事件推进。
        /// 2. **到账提示不再要求"该槽游戏正在运行"**。写共享内存本身是安全的：
        ///    Helper DLL 首次映射只记录序号基线、不补发历史，所以游戏没开时写进去不会被误弹；
        ///    而要求"必须正在运行"反而会把"游戏刚启动那几秒到的点券"整条丢掉。
        ///    kick 事件仍然要求进程在跑（那时才有进程可关）。
        ///
        /// 调用方可能是 UI 线程（10 秒轮询）或后台线程（长轮询），所以 UI 操作一律回派发器。
        /// </summary>
        private void ApplyGmEvents(string accountId, int slot, string username, IEnumerable<Services.RemoteAuthService.GMEventItem> events)
        {
            List<Services.RemoteAuthService.GMEventItem> fresh;
            lock (_gmEventLock)
            {
                // 游标从 0 起（本次会话内单调推进），**不再继承任何持久化值** ——
                // 跨会话的 id 游标是这次"泡点提示永远不弹"的根因，见 _gmEventSince 注释。
                _gmCursorByAccount.TryGetValue(accountId, out var cursor);
                fresh = new List<Services.RemoteAuthService.GMEventItem>();
                foreach (var ev in events)
                {
                    if (ev is null || ev.Id <= cursor)
                    {
                        continue; // 另一条通道已经处理过（长轮询与兜底轮询会重叠）
                    }
                    if (!string.Equals(ev.AccountId, accountId, StringComparison.OrdinalIgnoreCase))
                    {
                        continue; // 服务端已按账号过滤，这里再兜一层，避免串号
                    }
                    cursor = ev.Id;
                    fresh.Add(ev);
                }
                _gmCursorByAccount[accountId] = cursor;
            }

            foreach (var ev in fresh)
            {
                // 泡点到账：写共享内存私有区，由 Helper DLL 只对本人弹绿字，不关游戏。
                if (string.Equals(ev.EventType, "cera_granted", StringComparison.OrdinalIgnoreCase))
                {
                    // 只弹"新鲜"的到账提示：登录器重启后不回放历史（见 IsFreshGmEvent）。
                    // 这里**不再看 PointsinkEnabled** —— 那条会把"刚开泡点后的第一批提示"吞掉。
                    if (!IsFreshGmEvent(ev, GmEventFreshWindow))
                    {
                        continue;
                    }
                    if (!string.IsNullOrWhiteSpace(ev.Reason))
                    {
                        Services.HelperSharedMemory.SetPrivateNotify(accountId, ev.Reason, 0xFF00FF00);
                        // 只记前 5 条：泡点是 10 秒一次，全记会把 startup.log 刷爆。
                        // 这 5 条足够判定"登录器到底有没有把提示写进共享内存"。
                        if (_privNotifyLogged < 5)
                        {
                            _privNotifyLogged++;
                            App.LogStartup("泡点到账提示：已写私有槽 account=" + accountId + " msg=" + ev.Reason);
                        }
                    }
                    continue;
                }

                // kick：管理员封禁/踢下线，仅关闭该账号所在槽位的游戏进程
                if (string.Equals(ev.EventType, "kick", StringComparison.OrdinalIgnoreCase))
                {
                    if (!GameProcessManager.IsRunning(slot))
                    {
                        continue;
                    }
                    RunOnUi(() =>
                    {
                        GameProcessManager.KillSlot(slot);
                        if (!IsVisible)
                        {
                            RestoreFromTray();
                        }
                        var kicked = string.Equals(ev.Reason, "踢下线", StringComparison.OrdinalIgnoreCase);
                        ShowStatus(kicked
                            ? $"账号 {username} 已被管理员踢下线，游戏已强制关闭"
                            : $"账号 {username} 已被管理员封禁，游戏已强制关闭", Brushes.OrangeRed);
                    });
                }
            }
        }

        /// <summary>某账号当前的 GM 事件游标（长轮询的 after_id 用它）。本次会话内有效，从 0 起。</summary>
        private long GmCursorFor(string accountId)
        {
            lock (_gmEventLock)
            {
                return _gmCursorByAccount.TryGetValue(accountId, out var cursor) ? cursor : 0;
            }
        }

        /// <summary>回 UI 线程执行；已经在 UI 线程时直接跑（避免无谓的派发往返）。</summary>
        private void RunOnUi(Action action)
        {
            if (Dispatcher.CheckAccess())
            {
                action();
                return;
            }
            Dispatcher.Invoke(action);
        }

        /// <summary>
        /// 限流日志：同一条消息最快每 intervalSeconds 秒记一次。
        ///
        /// 为什么需要：轮询类代码一旦失败就是"每 5 秒一次"，全记会把 startup.log 刷爆，
        /// 而不记（改造前的写法）又会让"公告再也不弹"这种事完全查不到痕迹。
        /// </summary>
        private static void LogThrottled(ref DateTime lastAt, int intervalSeconds, string message)
        {
            var now = DateTime.Now;
            if ((now - lastAt).TotalSeconds < intervalSeconds)
            {
                return;
            }
            lastAt = now;
            App.LogStartup(message);
        }

        // ── GM 事件长轮询（实时推送）────────────────────────────────────────────

        /// <summary>
        /// 启动 GM 事件长轮询（90 远程模式且已登录后调用；幂等）。
        ///
        /// 为什么需要它：10 秒兜底轮询意味着"到账绿字最坏要等 10 秒"，
        /// 玩家反馈的"泡点不实时通知"就是这 0~10 秒。挂上长轮询后，运维工具一写事件
        /// 就唤醒对应账号的连接，绿字基本是**毫秒级**弹出。
        /// 兜底轮询保留不动（它还负责心跳续期，老登录器也只认它）。
        /// </summary>
        private void StartGmEventLoopIfNeeded()
        {
            if (_gmEventLoopStarted || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            _gmEventLoopStarted = true;
            _gmEventCts = new CancellationTokenSource();
            _ = GmEventLoopAsync(_gmEventCts.Token);
            App.LogStartup("GM 事件长轮询已启动（到账提示实时推送）");
        }

        private async Task GmEventLoopAsync(CancellationToken token)
        {
            while (!token.IsCancellationRequested)
            {
                try
                {
                    if (_gmEventWaitUnsupported)
                    {
                        return; // 服务端没有这个端点：长轮询整体退出，交给 10 秒兜底轮询
                    }
                    var sessions = SnapshotRemoteSessions();
                    if (sessions.Length == 0)
                    {
                        await Task.Delay(2000, token).ConfigureAwait(false);
                        continue;
                    }
                    var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                        ? AppConfig.Settings.RemoteIp.Trim()
                        : (AppConfig.Settings.Host ?? "").Trim();
                    var authPort = 18112;
                    var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                    if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
                    {
                        authPort = parsedPort;
                    }
                    if (string.IsNullOrWhiteSpace(host))
                    {
                        await Task.Delay(5000, token).ConfigureAwait(false);
                        continue;
                    }

                    // 每个已登录槽位各挂一条长轮询（服务端最多挂 25 秒才回）。
                    // 谁先返回就先处理谁，互不阻塞。
                    var waits = new List<Task>(sessions.Length);
                    foreach (var session in sessions)
                    {
                        waits.Add(WaitGmEventsForSessionAsync(host, authPort, session, token));
                    }
                    await Task.WhenAll(waits).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (Exception ex)
                {
                    App.LogStartup("GM 事件长轮询异常已忽略 -> " + ex.Message);
                    try
                    {
                        await Task.Delay(3000, token).ConfigureAwait(false);
                    }
                    catch (OperationCanceledException)
                    {
                        break;
                    }
                }
            }
        }

        private async Task WaitGmEventsForSessionAsync(
            string host, int authPort, (int Slot, string Ticket, string AccountId, string Username) session,
            CancellationToken token)
        {
            try
            {
                var gm = await Services.RemoteAuthService.WaitGMEventsAsync(
                    AppConfig.Settings, host, authPort, session.Ticket,
                    GmCursorFor(session.AccountId), _gmEventSince, token).ConfigureAwait(false);

                if (gm.EndpointMissing)
                {
                    // 服务端没有这个端点（老版运维工具）。404 是**秒回**的，
                    // 继续重挂就是每秒几百次的死循环 → 直接停掉长轮询，只留 10 秒兜底轮询。
                    _gmEventWaitUnsupported = true;
                    App.LogStartup("GM 事件长轮询：服务端未提供 /launcher/v1/gm-events/wait，已退回 10 秒兜底轮询");
                    _gmEventCts?.Cancel();
                    return;
                }
                if (!gm.Ok)
                {
                    // 网络抖动/服务端未就绪：**必须退避**再重挂（见 GmEventWaitBackoffMs 注释）。
                    await Task.Delay(GmEventWaitBackoffMs, token).ConfigureAwait(false);
                    return;
                }
                if (gm.Events.Count == 0)
                {
                    return; // 挂满 25 秒也没有事件：正常情况，立刻重挂下一轮
                }
                ApplyGmEvents(session.AccountId, session.Slot, session.Username, gm.Events);
            }
            catch (OperationCanceledException)
            {
                // 窗口关闭/重登：正常退出
            }
            catch (Exception ex)
            {
                App.LogStartup("GM 事件长轮询（槽位 " + session.Slot + "）异常已忽略 -> " + ex.Message);
                try
                {
                    await Task.Delay(GmEventWaitBackoffMs, token).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    // 正常退出
                }
            }
        }

        /// <summary>
        /// 事件是否足够新 —— 用来决定"这条到账提示要不要弹"。
        ///
        /// 服务端现在已经按 since（本次会话起始时刻）过滤过一遍，这里再兜一层：
        /// 一是容忍两端时钟偏差，二是万一 since 没传上去（老版运维工具）也不会回放历史。
        ///
        /// 解析失败时按"新"处理：宁可多弹一次提示，也不能因为格式变动把所有提示吞掉。
        /// </summary>
        private static bool IsFreshGmEvent(Services.RemoteAuthService.GMEventItem ev, TimeSpan window)
        {
            if (string.IsNullOrWhiteSpace(ev.CreatedAt))
            {
                return true;
            }
            if (!DateTime.TryParseExact(ev.CreatedAt.Trim(), "yyyy-MM-dd HH:mm:ss",
                    System.Globalization.CultureInfo.InvariantCulture,
                    System.Globalization.DateTimeStyles.None, out var created))
            {
                return true;
            }
            var age = DateTime.Now - created;
            if (age < TimeSpan.Zero)
            {
                return true; // 服务端时钟略快：当成新事件
            }
            return age <= window;
        }

        /// <summary>记录 90 远程某槽位成功登录的会话（用于心跳与 GM 事件轮询；覆盖同槽旧会话）</summary>
        private void RecordRemoteSession(int slot, string ticket, string accountId, string username)
        {
            // TEMP-8BOX：槽位扩到 8 个（主/副 + 3 号~8 号）后，这里必须按"槽位号是否合法"判断，
            // 不能只认主/副 —— 否则多开出来的账号不会被登记，心跳续期与 GM 踢人事件轮询都会漏掉它们。
            if (string.IsNullOrWhiteSpace(ticket) || string.IsNullOrWhiteSpace(accountId) ||
                slot < 0 || slot >= GameProcessManager.SlotCount)
            {
                // ★ 这里以前是**完全静默**的，但它一旦被跳过，后果是全局性的：
                //   · _remoteSessions 为空 → 公告长轮询永远不会启动 → 全服公告不弹
                //   · PollRemoteGmKickAsync 直接 return → 不心跳 → ticket 过期 →
                //     运维工具认为该账号不在线 → 泡点不发放 → 到账提示也没有
                //   也就是"公告和泡点到账一起哑掉"。所以必须留痕。
                App.LogStartup("RecordRemoteSession 被跳过（槽位=" + slot +
                               " 账号=" + (string.IsNullOrWhiteSpace(accountId) ? "(空)" : accountId) +
                               " ticket=" + (string.IsNullOrWhiteSpace(ticket) ? "(空)" : "有") +
                               "）→ 公告长轮询与 GM 事件轮询都不会启动");
                return;
            }

            lock (_remoteSessionsLock)
            {
                _remoteSessions.RemoveAll(s => s.Slot == slot);
                _remoteSessions.Add((slot, ticket, accountId, username));
            }
            StartAnnounceLoopIfNeeded();
            App.LogStartup("已记录远程会话：槽位=" + slot + " 账号=" + accountId +
                           "（公告长轮询 / 心跳 / GM 事件轮询已就绪）");
        }

        /// <summary>90 远程全服公告长轮询：独立后台任务，公告到达即写共享内存。</summary>
        private void StartAnnounceLoopIfNeeded()
        {
            if (_announceCts != null)
            {
                return;
            }
            _announceCts = new CancellationTokenSource();
            _ = AnnounceLoopAsync(_announceCts.Token);
            App.LogStartup("全服公告长轮询已启动");
        }

        private async Task AnnounceLoopAsync(CancellationToken token)
        {
            while (!token.IsCancellationRequested)
            {
                try
                {
                    var session = FirstRemoteSession();
                    if (string.IsNullOrWhiteSpace(session.Ticket))
                    {
                        await Task.Delay(2000, token).ConfigureAwait(false);
                        continue;
                    }

                    var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                        ? AppConfig.Settings.RemoteIp.Trim()
                        : AppConfig.Settings.Host;
                    var authPort = 18112;
                    var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                    if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
                    {
                        authPort = parsedPort;
                    }

                    var (ok, _, ann) = await Services.RemoteAuthService.GetAnnouncementsAsync(
                        AppConfig.Settings, host, authPort, session.Ticket, _lastAnnounceId).ConfigureAwait(false);
                    if (!ok)
                    {
                        // ★ 这里以前是**完全静默**的：401（ticket 失效）也只是一遍遍每 5 秒重试，
                        //   表现为"全服公告再也不弹"却查不到任何痕迹。现在按分钟记一条。
                        LogThrottled(ref _lastAnnounceFailureLogAt, 60,
                            "公告长轮询失败（ticket=" + (session.Ticket.Length > 8 ? session.Ticket.Substring(0, 8) + "…" : "(空)") + "）");
                        await Task.Delay(5000, token).ConfigureAwait(false);
                        continue;
                    }
                    if (ann is not null && ann.Id > _lastAnnounceId)
                    {
                        _lastAnnounceId = ann.Id;
                        Services.HelperSharedMemory.SetAnnouncement(ann.Content, ParseAnnounceColor(ann.Color), (int)ann.Id);
                        App.LogStartup("公告长轮询：收到 id=" + ann.Id + " 已写共享内存（游戏内应立刻弹绿字）");
                    }
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch
                {
                    try
                    {
                        await Task.Delay(5000, token).ConfigureAwait(false);
                    }
                    catch (OperationCanceledException)
                    {
                        break;
                    }
                }
            }
        }

        private static uint ParseAnnounceColor(string color)
        {
            if (string.IsNullOrWhiteSpace(color))
            {
                return 0xFF0000FF;
            }
            if (color.StartsWith("0x", StringComparison.OrdinalIgnoreCase))
            {
                color = color.Substring(2);
            }
            if (uint.TryParse(color, System.Globalization.NumberStyles.HexNumber, null, out var v))
            {
                return v;
            }
            return 0xFF0000FF;
        }

        // ── 更新维护（服务端维护期间玩家不能登录、不能进游戏）──────────────

        /// <summary>
        /// 启动维护状态长轮询（90 远程模式）。
        /// 免 ticket：登录界面还没登录时也必须能收到"开始/结束维护" —— 这正是
        /// "登录器已经打开、之后才勾选维护"能实时生效的原因。
        /// </summary>
        private void StartMaintenanceLoopIfNeeded()
        {
            if (_maintLoopStarted || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            _maintLoopStarted = true;
            _maintCts = new CancellationTokenSource();
            _ = MaintenanceLoopAsync(_maintCts.Token);
        }

        private async Task MaintenanceLoopAsync(CancellationToken token)
        {
            // 自检阶段已经拉过一次（静态状态里有初值），先按它刷一次 UI：
            // 否则"自检发现正在维护"与"长轮询返回"之间有个空档，按钮会先亮一下。
            await ApplyMaintenanceStateAsync(Services.RemoteAuthService.MaintenanceEnabled,
                Services.RemoteAuthService.MaintenanceAllowed, notify: false).ConfigureAwait(false);

            while (!token.IsCancellationRequested)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                        ? AppConfig.Settings.RemoteIp.Trim()
                        : AppConfig.Settings.Host;
                    var authPort = 18112; // 与公告/GM 事件同源：90 认证 API 端口
                    var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                    if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
                    {
                        authPort = parsedPort;
                    }
                    if (string.IsNullOrWhiteSpace(host))
                    {
                        await Task.Delay(5000, token).ConfigureAwait(false);
                        continue;
                    }

                    var username = await Dispatcher.InvokeAsync(() => AccountTextBox?.Text?.Trim() ?? string.Empty);
                    var (ok, _, info) = await Services.RemoteAuthService.WaitMaintenanceAsync(
                        AppConfig.Settings, host, authPort,
                        Services.RemoteAuthService.MaintenanceVersion, username, token).ConfigureAwait(false);
                    if (!ok)
                    {
                        // 网络抖动/服务端未就绪：退避后重挂，不打扰玩家。
                        await Task.Delay(3000, token).ConfigureAwait(false);
                        continue;
                    }
                    if (info is null)
                    {
                        continue;
                    }
                    await ApplyMaintenanceStateAsync(info.Enabled, info.Allowed, notify: true).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (Exception ex)
                {
                    App.LogStartup("维护长轮询异常已忽略 -> " + ex.Message);
                    try
                    {
                        await Task.Delay(5000, token).ConfigureAwait(false);
                    }
                    catch (OperationCanceledException)
                    {
                        break;
                    }
                }
            }
        }

        // ── 双开（多开）开关的实时推送（长轮询）──────────────────────────────────
        //
        // 为什么需要：双开开关原来只有"拉取式"的生效时机（启动自检读一次 + 点副账号登录前
        // 复查一次），所以**已经开着的登录器不会自动变灰**，运营关掉开关后必须让玩家重开
        // 登录器 —— 服主要求改成"后台一关，已经开着的登录器马上变灰"。
        // 做法与「更新维护」完全一致：服务端一改开关就广播唤醒挂起的长轮询。
        //
        // 注意：关双开**绝不**结束任何已在运行的副账号游戏进程（维护会 KillAll，这里不碰）。
        // 玩家侧看到的是"勾选框与副账号登录按钮变灰 + 一条提示"，已经在玩的副账号不受影响。

        /// <summary>90 认证 API 的地址与端口（RemoteIp 优先、其次 Host；与维护/GM 事件/公告同源）。</summary>
        private static (string Host, int Port) ResolveRemoteAuthEndpoint()
        {
            var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                ? AppConfig.Settings.RemoteIp.Trim()
                : (AppConfig.Settings.Host ?? "").Trim();
            var port = 18112; // 90 认证 API 端口
            var cfgPort = AppConfig.CurrentProfile?.RemotePort;
            if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
            {
                port = parsedPort;
            }
            return (host, port);
        }

        /// <summary>
        /// 启动双开开关长轮询（90 远程模式且已配置服务器地址时；幂等）。
        ///
        /// 免 ticket：登录界面还没登录时也必须能收到"后台关掉双开了" —— 这正是
        /// "已经打开的登录器不用重开就能变灰"的原因。
        /// </summary>
        private void StartMultiOpenLoopIfNeeded()
        {
            if (_multiOpenLoopStarted || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            if (string.IsNullOrWhiteSpace(ResolveRemoteAuthEndpoint().Host))
            {
                // 地址是空的（正常流程进不到这里：自检会先跳设置页）。不启动，
                // 免得每 5 秒空转一次；开关本身仍由自检 + 副登录前复查把关。
                App.LogStartup("双开开关实时推送未启动（未配置服务器地址）：实时推送不可用，" +
                    "退回启动自检 + 副登录前复查");
                return;
            }
            _multiOpenLoopStarted = true;
            _multiOpenCts = new CancellationTokenSource();
            _ = MultiOpenLoopAsync(_multiOpenCts.Token);
            App.LogStartup("双开开关长轮询已启动（后台一关，已打开的登录器立刻变灰）");
        }

        /// <summary>
        /// 双开开关长轮询循环。形状照 <see cref="MaintenanceLoopAsync"/> / GmEventLoopAsync：
        ///   - 服务端最多挂 25 秒，超时返回当前状态 → 直接重挂下一轮；
        ///   - 失败要**退避 5 秒**（别打成死循环）；
        ///   - 404（老运维工具没有这个端点）要**硬停**：404 是秒回的，不停就是每秒几百次空转。
        /// </summary>
        private async Task MultiOpenLoopAsync(CancellationToken token)
        {
            while (!token.IsCancellationRequested)
            {
                try
                {
                    if (_multiOpenWaitUnsupported)
                    {
                        return; // 服务端没有这个端点：整体退出，交给自检 + 副登录前复查
                    }
                    var (host, authPort) = ResolveRemoteAuthEndpoint();
                    if (string.IsNullOrWhiteSpace(host))
                    {
                        await Task.Delay(MultiOpenWaitBackoffMs, token).ConfigureAwait(false);
                        continue;
                    }

                    var (ok, error, info, endpointMissing) =
                        await Services.RemoteAuthService.WaitMultiOpenAsync(
                            AppConfig.Settings, host, authPort, token).ConfigureAwait(false);

                    if (endpointMissing)
                    {
                        // 老运维工具（没有 /launcher/v1/multi-open/wait）→ 硬停，别每 5 秒空转一辈子。
                        // 双开开关本身的语义**完全不变**：仍由启动自检 + 副登录前复查生效
                        //（就是改造前的那两条时机），只是后台关掉后要等玩家点副登录/重开登录器。
                        _multiOpenWaitUnsupported = true;
                        App.LogStartup("双开开关实时推送不可用（服务端未提供 /launcher/v1/multi-open/wait，" +
                            "老运维工具）：退回启动自检 + 副登录前复查");
                        _multiOpenCts?.Cancel();
                        return;
                    }
                    if (!ok)
                    {
                        // 网络抖动/服务端未就绪：**必须退避**再重挂（见 MultiOpenWaitBackoffMs）。
                        // 日志限流 60 秒一条：认证服务重启期间否则会连刷（与 GM 短轮询同款处理）。
                        if ((DateTime.UtcNow - _multiOpenWaitFailureLogAt).TotalSeconds >= 60)
                        {
                            _multiOpenWaitFailureLogAt = DateTime.UtcNow;
                            App.LogStartup("双开开关长轮询失败（退避 5 秒后重试） -> " + error);
                        }
                        await Task.Delay(MultiOpenWaitBackoffMs, token).ConfigureAwait(false);
                        continue;
                    }

                    // 服务端明确表态才落 UI。info.Enabled 为 null = 后台**从未设置过**该开关
                    // → 按"允许"处理（与 Window_Loaded 的 `MultiOpenEnabled ?? true`、
                    // RemoteAuthService.MultiOpenAllowed 同一口径；绝不能把"没设置过"当成关闭）。
                    // notify: true —— ApplyMultiOpenStateAsync 内部按 changed 判断，
                    // **只有真正变化时**才弹 Toast，所以这里不用自己比对一次。
                    await ApplyMultiOpenStateAsync(info?.Enabled ?? true, notify: true).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (Exception ex)
                {
                    App.LogStartup("双开开关长轮询异常已忽略 -> " + ex.Message);
                    try
                    {
                        await Task.Delay(MultiOpenWaitBackoffMs, token).ConfigureAwait(false);
                    }
                    catch (OperationCanceledException)
                    {
                        break;
                    }
                }
            }
        }

        // ── 服务器公告栏（登录页左侧公告板，管理工具「服务器公告」维护）──────────

        /// <summary>
        /// 启动公告栏刷新循环（仅远程模式；免 ticket —— 还没登录也要能看公告）。
        /// 首次用自检阶段已经拉到的内容直接渲染，避免开窗瞬间空一块；
        /// 之后每 10 分钟拉一次（公告板是静态内容，不需要长轮询）。
        /// </summary>
        private void StartNoticeLoopIfNeeded()
        {
            if (_noticeLoopStarted || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            _noticeLoopStarted = true;

            // 自检阶段（MainWindow.RunDemoFlowAsync）已经拉过一次：先按静态内容渲染一次
            ApplyNoticesToPanel(Services.RemoteAuthService.ServerNotices);

            _noticeCts = new CancellationTokenSource();
            _ = NoticeLoopAsync(_noticeCts.Token);
        }

        private async Task NoticeLoopAsync(CancellationToken token)
        {
            while (!token.IsCancellationRequested)
            {
                try
                {
                    await Task.Delay(NoticeRefreshInterval, token).ConfigureAwait(false);

                    var (host, authPort) = GetRemoteAuthEndpoint();
                    if (string.IsNullOrWhiteSpace(host))
                    {
                        continue;
                    }
                    var (ok, _, info) = await Services.RemoteAuthService.GetNoticesAsync(
                        AppConfig.Settings, host, authPort).ConfigureAwait(false);
                    if (!ok || info is null)
                    {
                        // 拉取失败保留上一次内容（网络抖动不该让玩家眼前的公告板消失）
                        continue;
                    }
                    ApplyNoticesToPanel(Services.RemoteAuthService.ServerNotices);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (Exception ex)
                {
                    App.LogStartup("公告栏刷新异常已忽略 -> " + ex.Message);
                }
            }
        }

        /// <summary>
        /// 把公告内容渲染到左侧公告板（UI 线程）。
        /// 一条公告都没有时整块隐藏：与其显示一个空框，不如不占地方。
        /// </summary>
        private void ApplyNoticesToPanel(IReadOnlyList<Services.RemoteAuthService.NoticeItem> notices)
        {
            Dispatcher.Invoke(() =>
            {
                if (ServerNoticePanel is null || ServerNoticeList is null)
                {
                    return;
                }

                // 全部重建：条目数量级只有个位数，重建比增量更新更好维护
                ServerNoticeList.Children.Clear();
                if (notices is null || notices.Count == 0)
                {
                    ServerNoticePanel.Visibility = Visibility.Collapsed;
                    return;
                }

                foreach (var n in notices)
                {
                    ServerNoticeList.Children.Add(BuildNoticeRow(n));
                }

                if (ServerNoticeUpdatedText is not null)
                {
                    ServerNoticeUpdatedText.Text = "更新于 " + DateTime.Now.ToString("HH:mm");
                }
                ServerNoticePanel.Visibility = Visibility.Visible;
                UpdateNoticePanelWidth();
            });
        }

        /// <summary>构造公告栏的一行：左侧日期（可空），右侧标题（自动换行）。</summary>
        private static FrameworkElement BuildNoticeRow(Services.RemoteAuthService.NoticeItem notice)
        {
            var row = new Grid { Margin = new Thickness(0, 0, 0, 10) };
            row.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
            row.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });

            var dateText = new TextBlock
            {
                Text = FormatNoticeDate(notice.NoticeDate),
                Foreground = new SolidColorBrush(Color.FromRgb(0xE8, 0x4A, 0x3A)),
                FontSize = 12,
                FontWeight = FontWeights.SemiBold,
                Margin = new Thickness(0, 1, 10, 0),
                VerticalAlignment = VerticalAlignment.Top,
                MinWidth = 42,
            };
            if (!string.IsNullOrWhiteSpace(notice.NoticeDate))
            {
                dateText.ToolTip = notice.NoticeDate;
            }
            Grid.SetColumn(dateText, 0);
            row.Children.Add(dateText);

            var titleText = new TextBlock
            {
                Text = notice.Title,
                Foreground = new SolidColorBrush(Color.FromArgb(0xE6, 0xFF, 0xFF, 0xFF)),
                FontSize = 13,
                TextWrapping = TextWrapping.Wrap,
                VerticalAlignment = VerticalAlignment.Top,
            };
            Grid.SetColumn(titleText, 1);
            row.Children.Add(titleText);

            return row;
        }

        /// <summary>日期显示成 MM-dd（省地方）；没有日期时用一个小圆点占位。</summary>
        private static string FormatNoticeDate(string date)
        {
            var raw = (date ?? "").Trim();
            if (raw.Length == 10 && raw[4] == '-')
            {
                return raw.Substring(5);
            }
            return string.IsNullOrEmpty(raw) ? "•" : raw;
        }

        /// <summary>
        /// 公告板宽度/位置随"双开"副登录框联动：副框可见时收窄到 300，
        /// 否则 520 + 副框 320 + 主框 348 + 边距在 1280 宽下会顶到左侧版本面板。
        ///
        /// 为什么这里还要一并调 RenderTransform.X：XAML 里公告板固定 X=-320（把公告板
        /// 从"登录框左边"推到窗口左侧），而 RenderTransform **不参与布局**——布局槽位置
        /// 只由容器内容宽度决定。1280 宽下（右 Margin 40）：
        ///   无双开：520+14+348=882 → 槽起点 358，平移后 [38,558) 完整可见；
        ///   双开：  300+14+320+14+348=996 → 槽起点 244，平移后 [-76,224) → 左边 76px
        ///          连标题都被窗口左边界裁掉（RootContainer 还是 ClipToBounds）。
        /// 所以副框可见时把平移量收到 -200（平移后 [44,344)，右侧与副框槽 558 留 214px 间距），
        /// 保证两种布局下公告板都完整可见。
        /// </summary>
        private void UpdateNoticePanelWidth()
        {
            if (ServerNoticePanel is null)
            {
                return;
            }
            var narrow = SecondaryLoginPanel is not null &&
                         SecondaryLoginPanel.Visibility == Visibility.Visible;
            ServerNoticePanel.Width = narrow ? NoticePanelNarrowWidth : NoticePanelFullWidth;
            if (ServerNoticePanel.RenderTransform is TranslateTransform translate)
            {
                translate.X = narrow ? NoticePanelNarrowShiftX : NoticePanelFullShiftX;
            }
        }

        /// <summary>
        /// 把维护状态应用到界面（在 UI 线程执行）：
        ///   - 维护中：主/副登录按钮显示「正在维护」且不可点；**正在游戏中的进程直接结束**
        ///     （用户要求：维护开始就把游戏断掉，不等玩家自己退）；
        ///   - 白名单账号（allowed=true）：按钮照常可用，管理员能进去自测；
        ///   - 维护结束：按钮恢复可用。
        /// notify=true 才弹 Toast；按自检初值刷新时不弹，避免刚进界面就弹一堆提示。
        /// </summary>
        private Task ApplyMaintenanceStateAsync(bool enabled, bool allowed, bool notify)
        {
            return ApplyMaintenanceStateAsync(enabled, allowed, _maintAllowedSecondary, notify);
        }

        /// <summary>
        /// 应用维护状态（带副账号白名单状态的重载）。
        /// allowed = 主账号是否在白名单；allowedSecondary = 副账号是否在白名单。
        /// </summary>
        private Task ApplyMaintenanceStateAsync(bool enabled, bool allowed, bool allowedSecondary, bool notify)
        {
            return Dispatcher.InvokeAsync(() =>
            {
                var changed = _maintEnabled != enabled || _maintAllowed != allowed;
                _maintEnabled = enabled;
                _maintAllowed = allowed;
                if (enabled)
                {
                    _maintAllowedSecondary = allowedSecondary;
                }
                else
                {
                    _maintAllowedSecondary = false;
                }

                if (enabled)
                {
                    if (notify && changed)
                    {
                        ShowStatus(
                            allowed
                                ? "服务器正在更新维护（当前账号在白名单内，可正常登录）"
                                : "服务器正在更新维护，登录暂不可用",
                            new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "维护中");
                    }
                    // 正在游戏中 → 直接结束游戏进程（含双开的所有槽位）。
                    if (GameProcessManager.AnyRunning)
                    {
                        GameProcessManager.KillAll();
                        ShowStatus("服务器开始更新维护，游戏已结束",
                            new SolidColorBrush(Color.FromRgb(0xE8, 0x4A, 0x3A)), "维护中");
                    }
                }
                else if (notify && changed)
                {
                    ShowStatus("服务器维护已结束，可以登录了",
                        new SolidColorBrush(Color.FromRgb(0x3C, 0xC8, 0x5A)), "维护结束");
                }

                RefreshLoginButtons();
            }).Task;
        }

        /// <summary>账号名变化：只有在维护期间才需要复查白名单（未维护时一个请求都不发）。</summary>
        private void OnMaintenanceAccountChanged(object sender, TextChangedEventArgs e)
        {
            if (!_maintEnabled)
            {
                return;
            }
            _maintAccountTimer?.Stop();
            _maintAccountTimer?.Start();
        }

        /// <summary>账号名输入停顿后复查：白名单里的管理员在维护期间也要能看到按钮亮起。</summary>
        private async void MaintenanceAccountTimer_Tick(object? sender, EventArgs e)
        {
            _maintAccountTimer?.Stop();
            if (!_maintEnabled || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            try
            {
                var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                    ? AppConfig.Settings.RemoteIp.Trim()
                    : AppConfig.Settings.Host;
                if (string.IsNullOrWhiteSpace(host))
                {
                    return;
                }
                var authPort = 18112;
                var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsedPort))
                {
                    authPort = parsedPort;
                }
                var username = AccountTextBox?.Text?.Trim() ?? string.Empty;
                var (ok, _, info) = await Services.RemoteAuthService.GetMaintenanceInfoAsync(
                    AppConfig.Settings, host, authPort, username).ConfigureAwait(true);
                if (!ok || info is null)
                {
                    return;
                }
                // 副账号（双开）单独问一次：白名单按账号名匹配，主账号在里面不代表副账号也在。
                var secondaryName = SecondaryAccountTextBox?.Text?.Trim() ?? string.Empty;
                var secondaryAllowed = false;
                if (!string.IsNullOrEmpty(secondaryName) &&
                    !string.Equals(secondaryName, username, StringComparison.OrdinalIgnoreCase))
                {
                    var (ok2, _, info2) = await Services.RemoteAuthService.GetMaintenanceInfoAsync(
                        AppConfig.Settings, host, authPort, secondaryName).ConfigureAwait(true);
                    if (ok2 && info2 is not null)
                    {
                        secondaryAllowed = info2.Allowed;
                    }
                }
                await ApplyMaintenanceStateAsync(info.Enabled, info.Allowed, secondaryAllowed, notify: false).ConfigureAwait(true);
            }
            catch (Exception ex)
            {
                App.LogStartup("维护白名单复查异常已忽略 -> " + ex.Message);
            }
        }

        /// <summary>槽位游戏退出后移除其远程会话（主/副均退出时列表自然清空）</summary>
        private void RemoveRemoteSession(int slot)
        {
            lock (_remoteSessionsLock)
            {
                _remoteSessions.RemoveAll(s => s.Slot == slot);
            }
        }

        /// <summary>根据在线状态/登录中/已登录刷新主副登录按钮可用性；已拉起 DNF.exe 成功后按钮文案变为“游戏运行中”（禁用灰色由模板触发器呈现）</summary>
        /// <remarks>PVF 自动更新：本机无客户端且服务端提供整包直链时主按钮变“下载客户端”；客户端落后于服务端最新发布时主按钮变“更新”（点击走更新流程），副按钮暂不可用（共用客户端目录）。</remarks>
        private void RefreshLoginButtons()
        {
            RefreshLoginButtonsCore();
            // 维护闸门放在最后统一收口：无论上面走了哪条分支（PVF 下载/更新等），
            // 维护期间该灰的按钮一定是灰的，不会被分支里的赋值又点亮。
            ApplyMaintenanceGate();
            // 双开后台开关同样在最后收口（顺序在维护之后）：维护优先显示"正在维护"，
            // 两者同时开着时玩家看到的是更严重的那个原因。
            ApplyMultiOpenGate();
        }

        /// <summary>
        /// 维护期间的按钮闸门（**按账号分别判断**）。
        ///
        /// 为什么必须分开：白名单是按账号名匹配的，双开时主账号在白名单里不代表副账号也在
        /// （线上反馈：主账号是管理员，副账号按钮却还是亮的、点了还能进）。
        /// 已登录/游戏运行中的槽位不动（只影响"还没登录"的按钮）。
        /// </summary>
        private void ApplyMaintenanceGate()
        {
            if (!_maintEnabled)
            {
                return;
            }
            if (!_maintAllowed && !_mainLoggedIn)
            {
                LoginButton.IsEnabled = false;
                LoginButton.Content = "正在维护";
            }
            if (!_maintAllowedSecondary && !_secondaryLoggedIn)
            {
                SecondaryLoginButton.IsEnabled = false;
                SecondaryLoginButton.Content = "正在维护";
            }
        }

        /// <summary>
        /// 双开（多开）后台开关的闸门：服务器关闭双开后，把「双开」勾选框与副账号登录按钮置灰。
        ///
        /// 表现是**禁用 + 提示，不隐藏**（与维护同类：临时、随时能改回来）。
        /// 隐藏只用于"这个版本根本不具备该能力"——A12/A21 的
        /// `DualCheckBox.Visibility = Collapsed` 走的是另一条路（见 SelectVersion），
        /// 两者互不冲突：非 90 版本本来就不显示，这里设 IsEnabled 也看不见。
        ///
        /// 本方法只负责"颜色/文案"，真正的硬拦在三个回调里
        /// （DualCheckBox_Checked / SecondaryLoginButton_Click / TempLoginAndLaunchAsync）——
        /// 置灰挡不住键盘、代码路径与将来改样式，硬拦才是最后一道。
        /// </summary>
        private void ApplyMultiOpenGate()
        {
            if (_multiOpenEnabled)
            {
                // 恢复允许时把闸门留下的痕迹清干净（否则 ToolTip 会一直写"已关闭"）。
                if (DualCheckBox.ToolTip is not null)
                {
                    DualCheckBox.ToolTip = null;
                }
                return;
            }

            DualCheckBox.IsEnabled = false;
            DualCheckBox.ToolTip = MultiOpenDisabledText;
            // 已经在跑的副账号保持原样（按钮显示"游戏运行中"），只置灰"还没登录"的那一个。
            if (!_secondaryLoggedIn)
            {
                SecondaryLoginButton.IsEnabled = false;
                SecondaryLoginButton.Content = MultiOpenDisabledButtonText;
            }
        }

        /// <summary>
        /// 应用双开（多开）后台开关状态。形状照 ApplyMaintenanceStateAsync：
        /// notify=true 才弹 Toast（自检初值刷新时不弹），且**只在真正变化时**才弹
        ///（内部按 changed 判断）—— 所以长轮询每 25 秒回来一次也不会反复弹提示。
        ///
        /// 调用方（三条时机，都是"补充"关系，不是替换）：
        ///   ① 启动自检（Window_Loaded，notify:false）；
        ///   ② 双开开关长轮询（MultiOpenLoopAsync，notify:true，实时推送）；
        ///   ③ 副账号登录前复查（MultiOpenBlocksSecondaryLoginAsync，notify:true）。
        ///
        /// 关闭时（enabled=false）额外做两件事，**都不碰游戏进程**：
        ///   ① 「双开」已被勾选 → 自动取消勾选（顺带走 DualCheckBox_Unchecked 收起副登录框
        ///      并收掉 TEMP-8BOX 的额外窗口），绝不把已经拉起来的副账号踢下线；
        ///   ② notify=true 时弹一条橙色提示，让玩家知道"为什么勾不了了"。
        /// </summary>
        private Task ApplyMultiOpenStateAsync(bool enabled, bool notify)
        {
            return Dispatcher.InvokeAsync(() =>
            {
                var changed = _multiOpenEnabled != enabled;
                _multiOpenEnabled = enabled;

                if (enabled)
                {
                    if (notify && changed)
                    {
                        ShowStatus("服务器已恢复双开，可以再次拉起副账号",
                            new SolidColorBrush(Color.FromRgb(0x3C, 0xC8, 0x5A)), "双开已恢复");
                    }
                }
                else
                {
                    // 直接把 IsChecked 置 false 就会走 DualCheckBox_Unchecked（收起副登录框 +
                    // 收掉临时额外窗口），不需要在这里重复一遍那段逻辑。
                    if (DualCheckBox.IsChecked == true)
                    {
                        DualCheckBox.IsChecked = false;
                    }
                    else if (SecondaryLoginPanel.Visibility == Visibility.Visible)
                    {
                        // 兜底：勾选态被别处清过但面板还开着（例如版本切换竞态），一样收起。
                        SecondaryLoginPanel.Visibility = Visibility.Collapsed;
                        UpdateNoticePanelWidth();
                    }

                    if (notify && changed)
                    {
                        ShowStatus(MultiOpenDisabledText + "（已在运行的副账号不受影响）",
                            new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "双开已关闭");
                    }
                }

                RefreshLoginButtons();
            }).Task;
        }

        /// <summary>
        /// 副账号登录前的双开开关复查（"点下去那一刻再问一次"，与维护开关同一套路）。
        ///
        /// 为什么需要：后台可能在玩家挂着登录界面时把双开关掉，而按钮的置灰状态来自自检时的
        /// **缓存**。这里现问一次；拿到明确的 false 就落闸（自动取消勾选 + Toast），
        /// 调用方据此中止本次副登录。
        ///
        /// 降级口径与自检完全一致：**只有明确收到 enabled:false 才拦**。
        /// 拉取失败、404（老运维工具没有这个端点）、后台从未设置过（data 里没有 enabled）
        /// → 一律放行，绝不因为一次网络抖动把玩家挡在门外。
        /// </summary>
        /// <returns>true = 本次副登录被双开开关拦下（调用方必须中止）。</returns>
        private async Task<bool> MultiOpenBlocksSecondaryLoginAsync(string host, int authPort)
        {
            try
            {
                var (ok, _, info) = await Services.RemoteAuthService.GetMultiOpenAsync(
                    AppConfig.Settings, host, authPort).ConfigureAwait(true);
                if (!ok || info is null)
                {
                    return false; // 拉不到：不改变任何状态（保持原值；原值通常是"允许"）
                }
                if (info.Enabled is bool known)
                {
                    // 值没变时 ApplyMultiOpenStateAsync 内部不会弹提示（按 changed 判断），
                    // 所以这里不需要再自己比对一次。
                    await ApplyMultiOpenStateAsync(known, notify: true).ConfigureAwait(true);
                }
                return !_multiOpenEnabled;
            }
            catch (Exception ex)
            {
                App.LogStartup("副登录前双开开关复查异常已忽略（保持原状态） -> " + ex.Message);
                return false;
            }
        }

        private void RefreshLoginButtonsCore()
        {
            // 更新维护的白名单账号（allowed=true，管理员自测）不受影响，继续走下面的正常逻辑；
            // 非白名单账号由 RefreshLoginButtons 末尾的 ApplyMaintenanceGate 统一置灰。

#if PVFUPDATE
            if (_pvfDownloadRequired && !_mainLoggedIn && !_secondaryLoggedIn)
            {
                LoginButton.IsEnabled = !_pvfBusy;
                SecondaryLoginButton.IsEnabled = false; // 本机尚无客户端：下载完成前禁止登录
                LoginButton.Content = "下载客户端";
                SecondaryLoginButton.Content = "副账号登 录";
                return;
            }

            if (_pvfUpdateRequired && !_mainLoggedIn)
            {
                LoginButton.IsEnabled = !_isLoggingIn && !_pvfBusy;
                SecondaryLoginButton.IsEnabled = false; // 共用客户端目录：需更新时禁止副账号以旧客户端登录
                LoginButton.Content = "更新";
                SecondaryLoginButton.Content = _secondaryLoggedIn ? "游戏运行中" : "副账号登 录";
                return;
            }
#endif

            LoginButton.IsEnabled = _isOnline && !_isLoggingIn && !_mainLoggedIn;
            SecondaryLoginButton.IsEnabled = _isOnline && !_isSecondaryLoggingIn && !_secondaryLoggedIn;

            LoginButton.Content = _mainLoggedIn ? "游戏运行中" : "登 录";
            SecondaryLoginButton.Content = _secondaryLoggedIn ? "游戏运行中" : "副账号登 录";
        }

#if PVFUPDATE
        // ── PVF 自动更新（90 外网版）────────────────────────────────

        /// <summary>客户端路径配置变化 → 立即做一次静默自检（GameConfigControl 已保存新路径）</summary>
        private void OnClientPathChanged()
        {
            _ = CheckPvfUpdateAsync(true);
        }

        /// <summary>启动 PVF 更新自检定时器（30 秒轮询）+ 立即自检一次；非 90 远程模式自动静默跳过</summary>
        private void StartPvfAutoUpdate()
        {
            StopPvfAutoUpdate();
            _pvfPollTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(30) };
            _pvfPollTimer.Tick += async (_, _) => await CheckPvfUpdateAsync(true);
            _pvfPollTimer.Start();
            // 启动首次自检走"非静默"：除了把登录按钮切成「更新」，还要在状态栏写清
            // 原因。之前这里是 silent=true，玩家只看到按钮文字变了，容易以为"没反应"。
            _ = CheckPvfUpdateAsync(false);
        }

        private void StopPvfAutoUpdate()
        {
            if (_pvfPollTimer is not null)
            {
                _pvfPollTimer.Stop();
                _pvfPollTimer = null;
            }
        }

        /// <summary>
        /// PVF 自动更新自检：向运维工具拉取最新发布信息，与本机 Script.pvf 的 SHA-256 比对，
        /// 决定是否把登录按钮切为“更新”。游戏运行中不检查（退出后由 OnGameExited 补查）。
        /// 未配置客户端路径 / 非 90 远程模式时静默返回。
        /// </summary>
        private async Task CheckPvfUpdateAsync(bool silent)
        {
            if (_pvfChecking || _pvfBusy)
            {
                return;
            }

            var profile = AppConfig.CurrentProfile;
            if (profile is null || profile.Id != "90" || AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }

            // 游戏运行中不打扰；全部退出后 OnGameExited 会立即补查一次
            if (_mainLoggedIn || _secondaryLoggedIn)
            {
                return;
            }

            // 未配置/失效的客户端路径也需拉取 latest：服务端提供整包直链时切“下载客户端”态；
            // 用户配置路径后由 ClientPathChanged 触发再查。
            _pvfChecking = true;
            try
            {
                var (ok, err, enabled, latest) = await Services.RemoteAuthService.GetPvfLatestAsync(AppConfig.Settings);
                if (!ok)
                {
                    App.LogStartup("PVF自检: 拉取发布信息失败 -> " + err);
                    if (!silent)
                    {
                        ShowStatus(err, Brushes.OrangeRed);
                    }
                    return; // 网络/服务端错误：静默轮询不打扰，保留上次状态
                }

                _pvfEnabled = enabled;
                _pvfLatest = latest;
                if (!enabled || latest is null || string.IsNullOrWhiteSpace(latest.FileSha256))
                {
                    // 服务端未启用加密 / 尚无发布版本：无需更新或下载
                    App.LogStartup("PVF自检: 服务端未发布客户端 Script.pvf（enabled=false），无需更新");
                    if (_pvfUpdateRequired || _pvfDownloadRequired)
                    {
                        _pvfUpdateRequired = false;
                        _pvfDownloadRequired = false;
                        RefreshLoginButtons();
                    }
                    return;
                }

                // 分支一：本机没有可用的 DNF.exe → 服务端配了整包直链才切“下载客户端”，否则保持普通登录态
                var gamePath = AppConfig.Settings.GamePath;
                var exeExists = !string.IsNullOrWhiteSpace(gamePath) && System.IO.File.Exists(gamePath);
                var needDownload = false;
                var needUpdate = false;
                if (!exeExists)
                {
                    needDownload = !string.IsNullOrWhiteSpace(latest.ClientDownloadUrl);
                    App.LogStartup($"PVF自检: 未配置客户端或缺少 DNF.exe（{gamePath}），整包直链=" +
                                   (string.IsNullOrWhiteSpace(latest.ClientDownloadUrl) ? "无" : "有") +
                                   $" 需要下载客户端={needDownload}");
                }
                else
                {
                    // 分支二：已有客户端 → 与本机 Script.pvf（DNF.exe 同目录）比对
                    var dir = System.IO.Path.GetDirectoryName(gamePath) ?? string.Empty;
                    var pvfPath = System.IO.Path.Combine(dir, "Script.pvf");
                    var localFp = "无";
                    needUpdate = true;
                    if (System.IO.File.Exists(pvfPath))
                    {
                        var localSha = ComputeFileSha256Hex(pvfPath);
                        localFp = localSha.Length >= 8 ? localSha.Substring(0, 8) : (localSha.Length == 0 ? "读不出" : localSha);
                        needUpdate = !string.Equals(localSha, latest.FileSha256, StringComparison.OrdinalIgnoreCase);
                    }
                    // 自检结论落盘（只记前 8 位指纹，不记完整哈希）：
                    // "有更新提示却不出现"这类问题，看这一行就能判定是没触发还是没拉到发布信息。
                    App.LogStartup($"PVF自检: 客户端目录={dir} 本机={localFp} 服务端=" +
                                   (latest.FileSha256.Length >= 8 ? latest.FileSha256.Substring(0, 8) : latest.FileSha256) +
                                   $" 需要更新={needUpdate}");
                }

                if (_pvfUpdateRequired != needUpdate || _pvfDownloadRequired != needDownload)
                {
                    _pvfUpdateRequired = needUpdate;
                    _pvfDownloadRequired = needDownload;
                    RefreshLoginButtons();
                }

                if (!silent)
                {
                    if (needDownload)
                    {
                        ShowStatus("检测到本机未安装客户端，请点击“下载客户端”安装完整客户端", Brushes.Orange);
                    }
                    else if (needUpdate)
                    {
                        ShowStatus("检测到客户端需要更新，请点击“更新”下载最新客户端", Brushes.Orange);
                    }
                }
            }
            finally
            {
                _pvfChecking = false;
            }
        }

        /// <summary>检查指定客户端目录内是否有 DNF.exe 正在运行（按进程主模块路径匹配，避免误伤其它版本）</summary>
        private static bool IsDnfRunning(string clientDir)
        {
            if (string.IsNullOrWhiteSpace(clientDir))
            {
                return false;
            }
            foreach (var p in Process.GetProcessesByName("DNF"))
            {
                try
                {
                    var mainFile = p.MainModule?.FileName;
                    if (!string.IsNullOrEmpty(mainFile) &&
                        string.Equals(System.IO.Path.GetDirectoryName(mainFile), clientDir, StringComparison.OrdinalIgnoreCase))
                    {
                        return true;
                    }
                }
                catch
                {
                    // 进程主模块访问受限（权限/已退出），忽略
                }
            }
            return false;
        }

        /// <summary>
        /// 点“更新”执行流程：确认 → 弹更新窗口（直链下载压缩包 → 解压覆盖客户端根目录）→ 完成后重新自检。
        /// 压缩包格式支持 zip/7z/rar；取消会在窗口内删除已下载的临时文件。
        /// </summary>
        private async Task RunPvfUpdateFlowAsync()
        {
            if (_pvfBusy)
            {
                return;
            }

            var gamePath = AppConfig.Settings.GamePath;
            if (string.IsNullOrWhiteSpace(gamePath))
            {
                ShowStatus("请先在游戏配置中选择 DNF.exe 路径", Brushes.OrangeRed);
                return;
            }
            var clientDir = System.IO.Path.GetDirectoryName(gamePath) ?? string.Empty;
            if (string.IsNullOrWhiteSpace(clientDir) || !System.IO.Directory.Exists(clientDir))
            {
                ShowStatus("客户端目录不存在，请检查游戏配置", Brushes.OrangeRed);
                return;
            }

            // 游戏运行中禁止覆盖
            if (_mainLoggedIn || _secondaryLoggedIn || IsDnfRunning(clientDir))
            {
                MessageBox.Show("请先关闭游戏后再更新客户端。", "无法更新", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            var latest = _pvfLatest;
            if (latest is null || string.IsNullOrWhiteSpace(latest.DownloadUrl))
            {
                MessageBox.Show("服务端未提供客户端更新下载地址，请联系服主在管理工具中配置直链后重试。",
                    "无法更新", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            var confirm = new ConfirmDialog("更新客户端",
                $"检测到客户端 Script.pvf 与服务端不一致，是否立即下载更新包并覆盖到客户端目录？\n\n" +
                $"目标目录：{clientDir}\n更新期间请勿关闭登录器或启动游戏。")
            {
                Owner = this
            };
            if (confirm.ShowDialog() != true)
            {
                return;
            }

            _pvfBusy = true;
            RefreshLoginButtons();
            var done = false;
            try
            {
                var dlg = new PvfUpdateDialog(latest.DownloadUrl, clientDir, latest.FileSha256)
                {
                    Owner = this
                };
                done = dlg.ShowDialog() == true;
            }
            finally
            {
                _pvfBusy = false;
                RefreshLoginButtons();
            }

            // 完成（或失败）后重新自检（须在 _pvfBusy 复位后执行），让按钮回到“登 录 / 更新”
            await CheckPvfUpdateAsync(true);
            ShowStatus(done ? "客户端更新完成" : "客户端更新失败或已取消", done ? Brushes.Green : Brushes.OrangeRed);
        }

        /// <summary>
        /// 点“下载客户端”执行流程：确认 → 选择目标目录 → 弹更新窗口（整包直链下载压缩包 → 解压到目标目录）
        /// → 找到 DNF.exe 写回 GamePath → 重新自检回登录态。整包为完整客户端，pvf 哈希在自检中复核。
        /// </summary>
        private async Task RunPvfDownloadFlowAsync()
        {
            if (_pvfBusy)
            {
                return;
            }

            // 当前已配置目录里有 DNF 运行中禁止下载覆盖
            var gamePath = AppConfig.Settings.GamePath;
            if (!string.IsNullOrWhiteSpace(gamePath))
            {
                var existingDir = System.IO.Path.GetDirectoryName(gamePath) ?? string.Empty;
                if (_mainLoggedIn || _secondaryLoggedIn || IsDnfRunning(existingDir))
                {
                    MessageBox.Show("请先关闭游戏后再下载/覆盖客户端。", "无法下载", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }
            }

            var latest = _pvfLatest;
            if (latest is null || string.IsNullOrWhiteSpace(latest.ClientDownloadUrl))
            {
                MessageBox.Show("服务端未提供客户端整包下载地址，请联系服主在管理工具中配置后重试。",
                    "无法下载", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            var dlg = new Microsoft.Win32.OpenFolderDialog
            {
                Title = "选择客户端安装目录（下载完成后 DNF.exe 会自动写回游戏配置）",
                Multiselect = false
            };
            if (dlg.ShowDialog() != true)
            {
                return;
            }
            var targetDir = dlg.FolderName?.Trim() ?? string.Empty;
            if (string.IsNullOrWhiteSpace(targetDir))
            {
                return;
            }

            var hasContent = System.IO.Directory.Exists(targetDir) &&
                             System.IO.Directory.EnumerateFileSystemEntries(targetDir).Any();
            var confirm = new ConfirmDialog("下载客户端",
                (hasContent
                    ? "目标目录已存在文件，下载解压将覆盖同名文件，请确认目录正确。\n\n"
                    : "将把完整客户端下载解压到该目录，下载完成前请勿关闭登录器。\n\n") +
                $"目标目录：{targetDir}\n若该目录下的 DNF.exe 正在运行，请先退出游戏。")
            {
                Owner = this
            };
            if (confirm.ShowDialog() != true)
            {
                return;
            }

            _pvfBusy = true;
            RefreshLoginButtons();
            var done = false;
            try
            {
                var doneDlg = new PvfUpdateDialog(latest.ClientDownloadUrl, targetDir, "") { Owner = this };
                done = doneDlg.ShowDialog() == true;
            }
            finally
            {
                _pvfBusy = false;
                RefreshLoginButtons();
            }

            if (!done)
            {
                ShowStatus("客户端下载失败或已取消", Brushes.OrangeRed);
                return;
            }

            // 解压完成后回填 GamePath（登录/启动/90CN.dll 复检都以 GamePath 为基准）
            var exePath = System.IO.Path.Combine(targetDir, "DNF.exe");
            if (!System.IO.File.Exists(exePath))
            {
                MessageBox.Show("客户端已下载解压，但目标目录未找到 DNF.exe，请检查整包内容或手动选择 DNF.exe。",
                    "未找到 DNF.exe", MessageBoxButton.OK, MessageBoxImage.Warning);
                ShowStatus("客户端已下载，但未找到 DNF.exe，请手动配置游戏路径", Brushes.OrangeRed);
                return;
            }

            AppConfig.Settings.GamePath = exePath;
            AppConfig.Save();
            await ConfigPage.RefreshFromProfileAsync(); // 同步游戏配置页 DNF.exe 输入框显示
            await CheckPvfUpdateAsync(true);            // _pvfBusy 已复位，自检可执行：回“登 录”或按哈希切“更新”
            ShowStatus("客户端下载完成，已就绪", Brushes.Green);
        }
#endif

        // ── 主框登录 ────────────────────────────────────────────

        private async void LoginButton_Click(object sender, RoutedEventArgs e)
        {
            if (_isLoggingIn || _mainLoggedIn)
            {
                return;
            }

#if PVFUPDATE
            // 按钮处于“下载客户端”态：先下载整包（完成后按钮回登录/更新态）
            if (_pvfDownloadRequired)
            {
                await RunPvfDownloadFlowAsync();
                return;
            }

            // 按钮处于“更新”态：走 PVF 自动更新流程而非登录
            if (_pvfUpdateRequired)
            {
                await RunPvfUpdateFlowAsync();
                return;
            }
#endif

            if (!_isOnline)
            {
                if (AppConfig.Mode == LoginMode.Local)
                {
                    ShowStatus($"当前版本（{AppConfig.CurrentProfile?.Name ?? "未知"}）服务端未启动，禁止登录", Brushes.OrangeRed);
                }
                else
                {
                    ShowStatus("当前离线，无法登录", Brushes.OrangeRed);
                }
                return;
            }

            var account = AccountTextBox.Text?.Trim() ?? string.Empty;
            var password = PasswordBox.Password ?? string.Empty;

            if (string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(password))
            {
                ShowStatus("账号或密码不能为空", Brushes.OrangeRed);
                return;
            }

            // ── 防重复触发（2026-09-26 修复「点一次登录起了两个客户端」）─────────────────
            // 必须在下面 await 之前置位！登录前兜底是两次 HTTPS 往返（各 10 秒超时，公网通常
            // 几百毫秒），这期间按钮仍是可点的、_isLoggingIn 仍是 false，于是第二次触发照样
            // 通过上面的守卫，跑出第二条完整登录流程 → 两次 LaunchGameAsync → 两个 DNF.exe。
            // 触发来源两种都真实存在：
            //   ① 双击/连点（尤其网络慢时玩家以为没反应又点一下）；
            //   ② 一次回车：主登录按钮是 IsDefault="True"，WPF 让"焦点在输入框里按回车"等同于
            //      点该按钮，而 LoginInput_KeyDown 里又调了一次本函数（已在该处补 e.Handled）。
            // 3~8 号临时多开窗口早就是"先置 _busy 再 await"的写法（TempExtraLoginWindow），
            // 这里补齐同样的语义：只要进了本函数，后面任何 await 期间都不允许第二次进入。
            // 被闸门拦下/认证失败时按原有分支各自复位，行为与修复前一致。
            _isLoggingIn = true;
            RefreshLoginButtons();
            App.LogStartup("登录请求：账号=" + account + " 槽位=主(" + GameProcessManager.MainSlot + ")");

            // 登录前兜底：用**当前填写的账号**再向服务器核对一次最新状态（维护 / 服务端指定 DLL）。
            if (!await TryPreLoginGateAsync(account))
            {
                _isLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            var (authOk, accountId, authError, authHasRescueCode, authTicket) = await TryAuthenticateAsync(account, password);
            if (!authOk)
            {
                // 账号或密码错误：清除该账号残留凭据，避免下次启动继续回填旧密码
                if (authError is not null && IsCredentialError(authError))
                {
                    CredentialManagerService.Delete(AppConfig.CurrentProfile.Id, account);
                    RememberCheckBox.IsChecked = false;
                    PasswordBox.Password = string.Empty;
                }
                _isLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            // 90 远程：记录本槽会话（心跳 + GM 踢人事件轮询使用；_remoteTicket 可能被副登录覆盖）
            if (AppConfig.Mode == LoginMode.Remote &&
                (AppConfig.CurrentProfile?.Id ?? "") == "90" &&
                !string.IsNullOrWhiteSpace(accountId))
            {
                RecordRemoteSession(GameProcessManager.MainSlot, authTicket ?? "", accountId, account);
            }
            // 90 远程：登录游戏时立即刷新一次泡点信息（不用等 10 秒轮询）
            if (AppConfig.Mode == LoginMode.Remote && (AppConfig.CurrentProfile?.Id ?? "") == "90")
            {
                var (psHost, psPort) = GetRemoteAuthEndpoint();
                if (!string.IsNullOrWhiteSpace(psHost))
                {
                    _ = RefreshPointsinkAsync(psHost, psPort);
                }
            }

            // 记住密码：主/副账号都按账号名+版本存凭据管理器（多条，跨版本不串）
            if (RememberCheckBox.IsChecked == true)
            {
                CredentialManagerService.Save(AppConfig.CurrentProfile.Id, account, password);
                AppConfig.Settings.SetLastAccount(AppConfig.CurrentProfile.Id, account);
            }
            AppConfig.Save();

            var process = await LaunchGameAsync(account, GameProcessManager.MainSlot, isSecondary: false, accountId, authTicket);
            if (process is null)
            {
                _isLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            _mainLoggedIn = true;
            _isLoggingIn = false;
            MainLoggedInText.Text = "已登录：" + account;
            MainLoggedInText.Visibility = Visibility.Visible;
            RefreshLoginButtons();

            // 老号无救援代码 → 弹补设引导（仅 90 版本，远程/本地均适用；每进程一次，双开不重复弹）
            if (!authHasRescueCode && !_rescuePromptShown &&
                !Services.ServerManager.IsS4Profile(AppConfig.CurrentProfile?.Id ?? "90"))
            {
                _rescuePromptShown = true;
                var ticket = AppConfig.Mode == LoginMode.Remote ? _remoteTicket ?? string.Empty : string.Empty;
                var rescueWindow = new RescueSetupWindow(AppConfig.Settings, AppConfig.Mode == LoginMode.Remote, account, ticket)
                {
                    Owner = this
                };
                rescueWindow.ShowDialog();
                if (rescueWindow.Succeeded)
                {
                    ShowStatus("救援代码已设置，下次可通过它找回密码", Brushes.Green);
                }
            }

            // 双开勾选且副框已填账号 → 保持窗口等待副框登录；否则最小化到托盘
            var dualFilled = DualCheckBox.IsChecked == true &&
                             !string.IsNullOrWhiteSpace(SecondaryAccountTextBox.Text);
            // ==================== TEMP-8BOX 临时功能（临时测试用） ====================
            // 原意：临时登录框还开着时不要躲进托盘（那几个框要用户手动点"登录并启动游戏"）。
            if (Temp8BoxEnabled && TempExtraLoginWindowsOpen)
            {
                dualFilled = true;
            }
            // ================== TEMP-8BOX 临时功能结束 ==============================
            if (!dualFilled)
            {
                HideToTray();
            }
        }

        // ── 副框登录 ────────────────────────────────────────────

        /// <summary>
        /// 登录前兜底校验（点"登录"那一刻现问服务器，不依赖长轮询缓存）。
        ///
        /// 为什么需要它：按钮的置灰状态来自长轮询/自检的**缓存**，存在时间窗 ——
        /// 服务端刚开维护、长轮询还没回来，或者玩家在窗口里换了账号（白名单是按账号名
        /// 匹配的），缓存都可能已经过时。登录是"要真的进游戏"的动作，所以这里再问一次：
        ///   1) 维护状态（带上本次要登录的账号名，白名单账号照常放行）；
        ///   2) 服务端指定 DLL 清单（外网版）：拉最新清单并在客户端根目录校验，
        ///      有"拒绝启动"级别的失败就直接挡住，不让玩家白等登录流程。
        /// 任一项被拦：弹窗说明 + 返回 false，调用方直接中止登录。
        /// </summary>
        /// <param name="username">本次要登录的账号名。</param>
        /// <param name="isSecondary">
        /// 本次是不是"副账号/多开窗口"的登录。只有这类登录才复查双开后台开关 ——
        /// 主账号登录不受双开开关影响（关了双开，主账号照样要能登）。
        /// </param>
        private async Task<bool> TryPreLoginGateAsync(string username, bool isSecondary = false)
        {
            if (AppConfig.Mode != LoginMode.Remote)
            {
                return true;
            }
            var (host, authPort) = GetRemoteAuthEndpoint();
            if (string.IsNullOrWhiteSpace(host))
            {
                return true;
            }

            // ⓪ 登录器版本校验（外网版）：认证之前先复查一次版本。
            //    为什么在自检阶段已经查过还要再查：自检到点登录之间可能过了很久
            //    （玩家挂着登录界面），服务端可能刚发布了新登录器；不复查就会用旧版登进去。
            //    与维护/DLL 两项不同，这一项**拿不到结果也禁止登录**（严格档，见 Guard 注释）。
            //    注意：这里必须在"输入账号密码"之前拦下 —— 让玩家输完密码才被告知去更新
            //    是很糟糕的体验。
#if LVERCHECK
            var versionGate = await Services.LauncherVersionGuard.CheckBeforeLoginAsync(this).ConfigureAwait(true);
            if (versionGate != Services.LauncherVersionGateResult.Pass)
            {
                App.LogStartup("登录前版本闸门结果=" + versionGate + "，已拒绝登录（账号=" + username + "）");
                ShowStatus("登录器版本校验未通过，禁止登录", Brushes.OrangeRed, "需要更新");
                return false;
            }
#endif

            // ① 维护状态：现问一次（结果同时刷新按钮状态，避免"点了才知道被拦"）。
            try
            {
                var (ok, _, info) = await Services.RemoteAuthService.GetMaintenanceInfoAsync(
                    AppConfig.Settings, host, authPort, username ?? string.Empty).ConfigureAwait(true);
                if (ok && info is not null)
                {
                    var secondaryAllowed = _maintAllowedSecondary;
                    if (!string.Equals((username ?? "").Trim(),
                            (AccountTextBox?.Text?.Trim() ?? ""), StringComparison.OrdinalIgnoreCase))
                    {
                        // 这次登录的不是主账号（副账号）：它的白名单状态用本次查询结果。
                        secondaryAllowed = info.Allowed;
                    }
                    await ApplyMaintenanceStateAsync(info.Enabled, _maintAllowed, secondaryAllowed, notify: true).ConfigureAwait(true);
                    if (info.Enabled && !info.Allowed)
                    {
                        App.LogStartup("登录前校验：服务器正在维护，已拒绝登录（账号=" + username + "，不在白名单）");
                        ShowStatus("服务器正在更新维护，暂不能登录", new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "维护中");
                        MessageBox.Show(
                            "服务器正在更新维护，暂时不能登录。\n\n请稍后再试；如果你是被授权的管理员账号，请确认账号名已加入维护白名单。",
                            "正在维护", MessageBoxButton.OK, MessageBoxImage.Information);
                        return false;
                    }
                }
            }
            catch (Exception ex)
            {
                // 查询失败不阻断登录（不能因为一次网络抖动把玩家挡在门外），只记日志。
                App.LogStartup("登录前维护校验异常已忽略 -> " + ex.Message);
            }

            // ①.5 双开（多开）后台开关：**只在副账号/多开窗口登录时**复查。
            //      主账号登录不受影响；拉不到结果也一律放行（降级口径见
            //      MultiOpenBlocksSecondaryLoginAsync 的说明）。
            if (isSecondary &&
                await MultiOpenBlocksSecondaryLoginAsync(host, authPort).ConfigureAwait(true))
            {
                App.LogStartup("登录前校验：服务器已关闭双开，已拒绝副账号登录（账号=" + username + "）");
                ShowStatus(MultiOpenDisabledText,
                    new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "双开已关闭");
                MessageBox.Show(
                    "服务器已关闭双开功能，暂时不能再登录副账号。\n\n" +
                    "如果确实需要双开，请联系管理员在后台开启。\n" +
                    "（已经在运行的副账号不会因此掉线。）",
                    "双开已关闭", MessageBoxButton.OK, MessageBoxImage.Information);
                return false;
            }

#if EXTERNAL
            // ② 服务端指定 DLL（外网版）：拉最新清单 + 客户端根目录校验。
            try
            {
                var (ok, _, dllInfo) = await Services.RemoteAuthService.GetDllRequirementsAsync(
                    AppConfig.Settings, host, authPort).ConfigureAwait(true);
                if (ok && dllInfo is not null)
                {
                    var requirements = new List<Services.ServerDllLoader.Requirement>();
                    foreach (var item in dllInfo.Dlls)
                    {
                        requirements.Add(new Services.ServerDllLoader.Requirement
                        {
                            FileName = item.FileName,
                            Sha256 = item.Sha256,
                            OnMismatch = item.OnMismatch,
                        });
                    }
                    var clientRoot = System.IO.Path.GetDirectoryName(AppConfig.Settings.GamePath) ?? string.Empty;
                    var checks = Services.ServerDllLoader.Validate(clientRoot, requirements);
                    if (Services.ServerDllLoader.HasBlockingFailure(checks))
                    {
                        App.LogStartup("登录前校验：服务端指定 DLL 未通过，已拒绝登录 -> " +
                            Services.ServerDllLoader.BuildSkipNote(checks));
                        ShowStatus("客户端文件校验未通过，已拒绝登录", Brushes.OrangeRed);
                        MessageBox.Show(Services.ServerDllLoader.BuildBlockMessage(checks),
                            "无法登录", MessageBoxButton.OK, MessageBoxImage.Error);
                        return false;
                    }
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("登录前 DLL 校验异常已忽略 -> " + ex.Message);
            }
#endif

            return true;
        }

        private async void SecondaryLoginButton_Click(object sender, RoutedEventArgs e)
        {
            if (_isSecondaryLoggingIn || _secondaryLoggedIn)
            {
                return;
            }

            // 硬闸门（第二道）：后台关闭双开后，副账号按钮即便被点亮也不许登录。
            // 置灰是"看得见的提示"，这里是"挡得住"的那一道 —— 键盘、样式变更、
            // 将来新增的入口都绕不过它。已在运行的副账号不受影响（那个走 _secondaryLoggedIn 分支）。
            if (!_multiOpenEnabled)
            {
                ShowStatus(MultiOpenDisabledText,
                    new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "双开已关闭");
                RefreshLoginButtons();
                return;
            }

            if (!_isOnline)
            {
                if (AppConfig.Mode == LoginMode.Local)
                {
                    ShowStatus($"当前版本（{AppConfig.CurrentProfile?.Name ?? "未知"}）服务端未启动，禁止登录", Brushes.OrangeRed);
                }
                else
                {
                    ShowStatus("当前离线，无法登录", Brushes.OrangeRed);
                }
                return;
            }

            var account = SecondaryAccountTextBox.Text?.Trim() ?? string.Empty;
            var password = SecondaryPasswordBox.Password ?? string.Empty;

            if (string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(password))
            {
                ShowStatus("副账号或密码不能为空", Brushes.OrangeRed);
                return;
            }

            // ── 防重复触发（2026-09-26，同主登录）：守卫提前到 await 之前 ──────────────
            // 副登录同样先过一遍登录前兜底（两次 HTTPS 往返），窗口期内双击/回车重入会跑出
            // 第二条流程 → 两次 LaunchGameAsync（同一个副槽位）→ 两个副客户端互相顶掉托管句柄。
            _isSecondaryLoggingIn = true;
            RefreshLoginButtons();
            App.LogStartup("登录请求：账号=" + account + " 槽位=副(" + GameProcessManager.SecondarySlot + ")");

            // 副账号同样走一遍登录前兜底：白名单是按账号名匹配的，副账号必须单独核对；
            // isSecondary=true 让它额外复查双开后台开关（主账号不复查）。
            if (!await TryPreLoginGateAsync(account, isSecondary: true))
            {
                _isSecondaryLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            var (secAuthOk, secAccountId, secAuthError, _, secAuthTicket) = await TryAuthenticateAsync(account, password);
            if (!secAuthOk)
            {
                // 账号或密码错误：清除该账号残留凭据，避免下次启动继续回填旧密码
                if (secAuthError is not null && IsCredentialError(secAuthError))
                {
                    CredentialManagerService.Delete(AppConfig.CurrentProfile.Id, account);
                    RememberCheckBox.IsChecked = false;
                    SecondaryPasswordBox.Password = string.Empty;
                }
                _isSecondaryLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            // 90 远程：记录本槽会话（心跳 + GM 踢人事件轮询使用）
            if (AppConfig.Mode == LoginMode.Remote &&
                (AppConfig.CurrentProfile?.Id ?? "") == "90" &&
                !string.IsNullOrWhiteSpace(secAccountId))
            {
                RecordRemoteSession(GameProcessManager.SecondarySlot, secAuthTicket ?? "", secAccountId, account);
            }
            // 90 远程：副登录游戏时立即刷新一次泡点信息
            if (AppConfig.Mode == LoginMode.Remote && (AppConfig.CurrentProfile?.Id ?? "") == "90")
            {
                var (psHost, psPort) = GetRemoteAuthEndpoint();
                if (!string.IsNullOrWhiteSpace(psHost))
                {
                    _ = RefreshPointsinkAsync(psHost, psPort);
                }
            }

            // 记住密码勾选时副账号密码同样保存（按账号名+版本存多条，跨版本不串）
            if (RememberCheckBox.IsChecked == true)
            {
                CredentialManagerService.Save(AppConfig.CurrentProfile.Id, account, password);
            }

            var process = await LaunchGameAsync(account, GameProcessManager.SecondarySlot, isSecondary: true, secAccountId, secAuthTicket);
            if (process is null)
            {
                _isSecondaryLoggingIn = false;
                RefreshLoginButtons();
                return;
            }

            _secondaryLoggedIn = true;
            _isSecondaryLoggingIn = false;
            SecondaryLoggedInText.Text = "已登录：" + account;
            SecondaryLoggedInText.Visibility = Visibility.Visible;
            RefreshLoginButtons();

            // 主框也已登录 → 全部登录完成，最小化到托盘
            if (_mainLoggedIn)
            {
                HideToTray();
            }
        }

        /// <summary>
        /// 验证账号：本地模式直连 MySQL（bcrypt）并返回账号ID；
        /// S4 系列（A12/A21）直连 inventory.db 校验账号存在与密码（服务端本身不校验）；
        /// 远程模式：S4 走 7002 认证直连，90 走 HTTPS 取 ticket + LANA 握手。
        /// </summary>
        private async Task<(bool Ok, string AccountId, string? Error, bool HasRescueCode, string? Ticket)> TryAuthenticateAsync(string account, string password)
        {
            // 远程模式：S4 系列走 7002 认证直连（Ed25519 + ECDH + AES-GCM，核心逻辑在
            // s4authcore.dll 内，认证通过后透传 m_id 直连 远程IP:7001）；
            // 90 走 HTTPS API 取 ticket（RemoteAuthService）后经 env 注入由 90CN.dll 完成 LANA 握手。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                var host = GetRemoteServerHost();
                if (string.IsNullOrWhiteSpace(host))
                {
                    ShowStatus("远程服务器 IP 未配置，请在 设置-服务器配置 中填写公网 IP", Brushes.OrangeRed);
                    return (false, string.Empty, "远程服务器 IP 未配置", false, null);
                }

                var profileId = AppConfig.CurrentProfile?.Id ?? "90";
                if (Services.ServerManager.IsS4Profile(profileId))
                {
                    // S4A21/S4A12：7002 认证（固定端口），服务端按 m_id 校验/封禁，通过后透传 m_id 直连
                    if (!Services.S4AuthCoreNative.IsReady)
                    {
                        ShowStatus("认证核心组件缺失（s4authcore.dll），请使用完整版登录器", Brushes.OrangeRed);
                        return (false, string.Empty, "认证核心组件未部署", true, null);
                    }

                    var (authOk, result, authError) = await Task.Run(() =>
                    {
                        var ok = Services.S4AuthCoreNative.Authenticate(
                            host, Services.ServerManager.S4AuthPort, account, password, out var r, out var e);
                        return (ok, r, e);
                    });
                    if (!authOk)
                    {
                        var friendlyAuthError = FriendlyText(authError, "7002 认证失败，请检查网络与服务器配置后重试");
                        ShowStatus(friendlyAuthError, Brushes.OrangeRed);
                        return (false, string.Empty, friendlyAuthError, true, null);
                    }

                    if (result.Ok != 1)
                    {
                        var denyMsg = string.IsNullOrWhiteSpace(result.MsgText) ? "服务器拒绝登录" : result.MsgText;
                        var friendlyDenyMsg = FriendlyText(denyMsg, "服务器拒绝登录，如有疑问请联系管理员");
                        ShowStatus(friendlyDenyMsg, Brushes.OrangeRed);
                        return (false, string.Empty, friendlyDenyMsg, true, null);
                    }

                    // S4 无救援代码体系，按已设置处理（不触发补设引导）
                    return (true, result.MId, null, true, null);
                }

                // 90 远程：走运维工具 /launcher/v1/login 拿 ticket（LANA 握手用）。
                var (loginOk, loginError, loginTicket, loginAccountId, hasRescueCode) =
                    await Services.RemoteAuthService.LoginForTicketAsync(AppConfig.Settings, account, password);
                if (!loginOk)
                {
                    ShowStatus(loginError, Brushes.OrangeRed);
                    return (false, string.Empty, loginError, false, null);
                }

                _remoteTicket = loginTicket;
                _remoteAccountId = loginAccountId;
                return (true, loginAccountId, null, hasRescueCode, loginTicket);
            }

            // S4A21/S4A12：服务端不校验账号密码（GetByMid 自动建号），校验职责在登录器：
            // 直连 inventory.db 校验账号存在 + bcrypt 密码匹配
            if (AppConfig.CurrentProfile.Id != "90")
            {
                var (s4Ok, s4Error, s4AccountId) = await DatabaseService.AuthenticateS4Async(AppConfig.Settings, account, password);
                if (!s4Ok)
                {
                    ShowStatus(s4Error, Brushes.OrangeRed);
                    return (false, string.Empty, s4Error, true, null);
                }

                return (true, s4AccountId, null, true, null);
            }

            // 本地模式：直连 MySQL/SQLite，bcrypt 校验；返回是否已设置救援代码（老号无码引导补设）
            var (dbOk, dbError, dbAccountId, dbHasRescueCode) = await DatabaseService.AuthenticateAsync(AppConfig.Settings, account, password);
            if (!dbOk)
            {
                ShowStatus(dbError, Brushes.OrangeRed);
                return (false, string.Empty, dbError, false, null);
            }

            return (true, dbAccountId, null, dbHasRescueCode, null);
        }

        /// <summary>远程模式服务器地址：优先 RemoteIp，缺省回退服务端 Host</summary>
        private static string GetRemoteServerHost()
        {
            return (string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                ? AppConfig.Settings.Host
                : AppConfig.Settings.RemoteIp)?.Trim() ?? "";
        }

        /// <summary>90 远程 LANA 认证的 game 端口清单（注入 DNF_REMOTE_AUTH_PORTS）。</summary>
        /// <remarks>
        /// 90CN.dll 只对清单内的端口做 LANA 握手；漏掉某个端口时，玩家能登录、能看到
        /// 频道列表，但选中那个频道就会掉线，服务端日志为
        /// <c>game-launcher-auth-rejected reason=auth-frame-read-failed error=unexpected EOF</c>。
        ///
        /// 因此这份清单必须完整覆盖服务端展开出的全部频道游戏端口。当前与
        /// channel_info.etc 的 118 个频道一致（10006..10253）。
        /// 改动频道表后请同步这里；将来运维工具在登录响应里下发 game_ports 后，可改为优先取服务端值。
        /// </remarks>
        private static string BuildRemoteAuthPorts()
        {
            // 运维工具目前尚未在登录响应里下发 game_ports（登录器模型已预留 GamePorts 字段），
            // 所以这里直接返回完整回退清单。等工具实现下发后，改为优先取服务端值即可。
            return DefaultRemoteAuthPorts;
        }

        /// <summary>回退清单：channel_info.etc 展开出的全部 118 个频道游戏端口。</summary>
        private const string DefaultRemoteAuthPorts =
            "10006,10010,10011,10012,10013,10014,10015,10016,10017,10018,10019,10020,10021,10023,10024,10025,10026,10027,10028," +
            "10029,10030,10041,10042,10043,10044,10045,10046,10047,10048,10049,10050,10051,10052,10053,10054,10055,10056,10057,10058,10059,10060," +
            "10091,10092,10093,10094,10095,10096,10097,10098,10099,10100," +
            "10111,10112,10113,10114,10115,10116,10117,10118,10119,10120,10121," +
            "10141,10142,10143,10144,10145,10146,10147,10148,10149,10150,10151,10152,10153,10154,10155,10156,10157,10158,10159,10160," +
            "10171,10181,10182,10183,10184,10185,10186," +
            "10191,10192,10193,10194,10195,10196,10197," +
            "10201,10202,10203,10204,10205,10206,10207,10208,10209,10210," +
            "10221,10222,10223,10231,10232,10233,10241,10242,10243,10251,10252,10253";

        /// <summary>计算文件 SHA-256（hex 小写，流式读取）。文件不存在/读取异常返回 null。</summary>
        private static string? ComputeFileSha256Hex(string filePath)
        {
            try
            {
                using var fs = new System.IO.FileStream(filePath, System.IO.FileMode.Open,
                    System.IO.FileAccess.Read, System.IO.FileShare.Read);
                using var sha = System.Security.Cryptography.SHA256.Create();
                return Convert.ToHexString(sha.ComputeHash(fs)).ToLowerInvariant();
            }
            catch
            {
                return null;
            }
        }

        /// <summary>判断认证错误是否为"账号或密码错误"（服务端明确拒绝凭据，而非网络/配置问题）</summary>
        private static bool IsCredentialError(string? error)
        {
            if (string.IsNullOrWhiteSpace(error))
            {
                return false;
            }

            return error.Contains("账号或密码错误", StringComparison.Ordinal) ||
                   error.Contains("401", StringComparison.Ordinal);
        }

        /// <summary>认证/绑定等底层返回的原始错误统一转友好中文（服务端错误码/HTTP/英文异常均兜底），中文文案原样保留</summary>
        private static string FriendlyText(string? raw, string fallback) =>
            Services.RemoteAuthService.ToFriendlyText(raw, fallback);

        // ── 启动 DNF.exe ────────────────────────────────────────

        /// <summary>
        /// 本地模式登录前预检：返回 null 表示通过，否则返回弹窗提示文案（拒绝登录）。
        /// 远程模式无 PID 绑定，不做这些检查。
        /// </summary>
        private async Task<string?> PreflightLocalModeAsync()
        {
            if (AppConfig.Mode != LoginMode.Local)
            {
                return null;
            }

            await Task.CompletedTask;

            // 1) Admin Token 已填写
            var token = AppConfig.Settings.AdminToken?.Trim() ?? "";
            if (string.IsNullOrEmpty(token))
            {
                return "Admin Token 未填写。\n请在 设置-服务器配置 中填写服务端管理接口令牌（instance.json 的 server.adminToken），否则进游戏会卡在“正在连接服务器”。";
            }

            // 2) 本机服务端管理端口可达（绑定账号依赖 127.0.0.1:18111）
            if (!Services.ServerManager.IsPortOpen("127.0.0.1", Services.ServerManager.AdminPort))
            {
                return "本机服务端未运行（127.0.0.1:18111 无响应）。\n请先启动服务端；若服务端在远程服务器上，请在 设置 中切换为“外网模式”，不要用本地模式登录。";
            }

            // 3) 配置了服务端根目录时，比对 instance.json 的 adminToken 与登录器设置是否一致
            var root = AppConfig.Settings.ServerRootPath;
            if (!string.IsNullOrWhiteSpace(root))
            {
                var info = Services.ServerManager.LoadInfo(root);
                if (info is not null && !string.IsNullOrWhiteSpace(info.AdminToken) &&
                    !string.Equals(info.AdminToken, token, StringComparison.Ordinal))
                {
                    return "Admin Token 与 instance.json 中的 adminToken 不一致。\n请在 设置-服务器配置 中填入 instance.json 对应的令牌（或点击“从 instance.json 同步”），否则服务端会拒绝账号绑定，进游戏卡在“正在连接服务器”。";
                }
            }

            return null;
        }

        /// <summary>
        /// 启动 DNF.exe：16 段启动参数 + 环境变量注入 + DLL 注入 + 进程托管。
        /// 工作目录 = DNF.exe 所在目录。
        /// </summary>
        private async Task<Process?> LaunchGameAsync(string account, int slot, bool isSecondary, string accountId, string? ticket)
        {
            // ── 槽位互斥兜底（2026-09-26）────────────────────────────────────────────
            // 同一槽位已有游戏在运行时，绝不再启动第二个客户端。上面主/副登录的守卫保证的是
            // "一次点击只跑一条流程"，这里是最后一道：任何路径（含将来新增的调用方）重复进入
            // 都只会在这一步被挡下，而不是起第二个 DNF.exe。
            // 必须挡住的原因：GameProcessManager.RegisterProcess 是直接覆盖槽位句柄的，第二个
            // 客户端会把第一个顶成"不受托管"的孤儿 —— 退出登录器时 KillAll 杀不到它，且
            // "全部游戏退出"的判定会提前触发（游戏中登录器就弹回前台）。
            // 3~8 号临时多开窗口本来就有同样的检查（TempExtraLoginWindow.DoLoginAsync），
            // 主/副路径此前漏了。正常流程不受影响：游戏退出会走 Exited 事件清空槽位。
            if (GameProcessManager.IsRunning(slot))
            {
                App.LogStartup("拒绝重复启动：槽位=" + slot + " 账号=" + account + " 该槽位已有游戏在运行");
                ShowStatus("该槽位已有游戏在运行，请先退出游戏再登录", Brushes.OrangeRed);
                return null;
            }

            var profile = AppConfig.CurrentProfile;
            var is90 = profile.Id == "90";

            // 登录闸控：本地模式必须校验当前版本服务端在线（90=18111；S4 系列=7001+进程路径匹配），
            // 服务端未启动时禁止登录/启动游戏；防止 A21 运行时误放行 A12/90。
            if (AppConfig.Mode == LoginMode.Local && !_isOnline)
            {
                var gateError = $"当前版本（{profile.Name}）服务端未启动，禁止登录。\n请在“游戏配置-服务端”中启动对应版本服务端后再试。";
                MessageBox.Show(gateError, "无法登录", MessageBoxButton.OK, MessageBoxImage.Warning);
                ShowStatus($"当前版本（{profile.Name}）服务端未启动，禁止登录", Brushes.OrangeRed);
                return null;
            }

            // 登录前预检：仅 90 检查 AdminToken/服务端在线/令牌一致性；
            // S4A21/S4A12 登录链路开发中，暂不套用 90 专属检查，不通过则弹窗拒绝登录
            if (is90)
            {
                var preflightError = await PreflightLocalModeAsync();
                if (preflightError is not null)
                {
                    MessageBox.Show(preflightError, "无法登录", MessageBoxButton.OK, MessageBoxImage.Warning);
                    ShowStatus("登录前检查未通过", Brushes.OrangeRed);
                    return null;
                }
            }

            var gamePath = AppConfig.Settings.GamePath;
            if (string.IsNullOrWhiteSpace(gamePath))
            {
                ShowStatus("请先在设置中配置 DNF.exe 路径", Brushes.OrangeRed);
                return null;
            }

            if (!System.IO.File.Exists(gamePath))
            {
                ShowStatus("DNF.exe 不存在，请先在设置中重新选择", Brushes.OrangeRed);
                return null;
            }

            var directory = System.IO.Path.GetDirectoryName(gamePath) ?? string.Empty;
            var isRemote = AppConfig.Mode == LoginMode.Remote;

            // 90CN.dll 启动前复检（仅 90 版本且 GamePath 已配置）：
            //    与登录器内嵌 90CN.dll 比对 SHA256，缺失询问安装、不一致询问替换（不备份）；
            //    用户取消或写入失败则中止本次启动并提示，不抛异常。
            if (is90)
            {
                var embeddedHash = Services.Cn90DllHelper.EmbeddedSha256();
                if (!string.IsNullOrEmpty(embeddedHash))
                {
                    var clientHash = Services.Cn90DllHelper.ClientSha256(directory);
                    var needDeploy = false;
                    if (string.IsNullOrEmpty(clientHash))
                    {
                        var installDlg = new ConfirmDialog("安装 90CN.dll", "未检测到客户端 90CN.dll，是否安装？")
                        {
                            Owner = this
                        };
                        if (installDlg.ShowDialog() != true)
                        {
                            MessageBox.Show("请先处理 90CN.dll 后再启动", "无法启动", MessageBoxButton.OK, MessageBoxImage.Warning);
                            return null;
                        }

                        needDeploy = true;
                    }
                    else if (!string.Equals(clientHash, embeddedHash, StringComparison.OrdinalIgnoreCase))
                    {
                        var replaceDlg = new ConfirmDialog("替换 90CN.dll", "90CN.dll 与登录器内置版本不一致，是否替换？")
                        {
                            Owner = this
                        };
                        if (replaceDlg.ShowDialog() != true)
                        {
                            MessageBox.Show("请先处理 90CN.dll 后再启动", "无法启动", MessageBoxButton.OK, MessageBoxImage.Warning);
                            return null;
                        }

                        needDeploy = true;
                    }

                    if (needDeploy)
                    {
                        var deployErr = Services.Cn90DllHelper.DeployTo(directory);
                        if (deployErr is not null)
                        {
                            MessageBox.Show(deployErr + "\n\n请先关闭游戏后重试。", "无法启动", MessageBoxButton.OK, MessageBoxImage.Warning);
                            ShowStatus("90CN.dll 写入失败，已中止本次启动", Brushes.OrangeRed);
                            return null;
                        }
                    }
                }
            }

#if EXTERNAL
            // 90 远程 + PVF 加密（方案B）：启动前用本机 Script.pvf 的 SHA-256 向运维工具
            // 公网 API（/launcher/v1/pvf/key）换取当前生效 key_set，注入 DNF_PVF_KEYSET；
            // DNFHelper 启动时按环境变量把 DNF.exe 内 6 处 PVF 取钥 call 改写为本机 key。
            // 404=服务端无此 pvf（客户端不一致）阻断启动；网络错误/401 也阻断，避免带错 key 进游戏。
            // Script.pvf 不存在视为未加密客户端，跳过拉取（helper 无 keyset 时静默）。
            string? pvfKeySet = null;
            // PVF 换钥诊断（每次启动只落一行，见下方 logPvfDiag）：
            // 用来一眼分清「登录器没给上钥匙」还是「服务端加密/发布没弄好」。
            // 只记分类与短指纹，不记 ticket、不记 key_set 内容、不记完整哈希与错误详情。
            var pvfFp = "-";
            void LogPvfDiag(string server, string note)
            {
                try
                {
                    App.LogStartup("PVF: fp=" + pvfFp +
                        " server=" + server +
                        " keyset=" + (string.IsNullOrEmpty(pvfKeySet) ? "no" : "yes") +
                        " -> " + note);
                }
                catch
                {
                    // 诊断日志失败不影响启动
                }
            }

            if (is90 && isRemote && ticket is not null)
            {
                // PVF 自动更新闸控：自检发现本机落后于服务端最新发布时，禁止以旧客户端登录
                if (_pvfUpdateRequired)
                {
                    LogPvfDiag("skipped", "本机Script.pvf落后于服务端发布，已阻断登录（需先更新客户端）");
                    MessageBox.Show("客户端 Script.pvf 需要更新，请先点击“更新”下载最新客户端后再登录。",
                        "PVF校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                    ShowStatus("客户端需要更新，禁止登录", Brushes.OrangeRed);
                    return null;
                }

                var scriptPvfPath = System.IO.Path.Combine(directory, "Script.pvf");
                if (System.IO.File.Exists(scriptPvfPath))
                {
                    var scriptSha = ComputeFileSha256Hex(scriptPvfPath);
                    if (!string.IsNullOrEmpty(scriptSha))
                    {
                        if (scriptSha.Length >= 8)
                        {
                            pvfFp = scriptSha.Substring(0, 8);
                        }

                        var (pvfOk, pvfError, pvfKey, pvfTag) =
                            await Services.RemoteAuthService.GetPvfKeyAsync(AppConfig.Settings, ticket, scriptSha);
                        if (pvfOk)
                        {
                            // pvfKey==null = 服务端 PVF 加密未启用：按未加密客户端放行，不注入 key_set。
                            if (pvfKey is not null)
                            {
                                pvfKeySet = pvfKey.KeySet;
                                LogPvfDiag(pvfTag, "服务端按本机指纹下发了换钥，登录器已注入（若客户端仍报安装档失败，看 helper_inject.log 的注入结果）");
                            }
                            else
                            {
                                LogPvfDiag(pvfTag, "服务端说未启用PVF加密，登录器未注入换钥（客户端若报安装档失败=服务端加密/发布没配好）");
                            }
                        }
                        else
                        {
                            LogPvfDiag(pvfTag, pvfTag == "not-found"
                                ? "服务端没有这份Script.pvf，已阻断登录（客户端版本与服务端发布不一致）"
                                : "取换钥失败，已阻断登录");
                            MessageBox.Show(pvfError, "PVF校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                            ShowStatus(pvfError, Brushes.OrangeRed);
                            return null;
                        }
                    }
                    else
                    {
                        LogPvfDiag("skipped", "本机Script.pvf读不出哈希（文件损坏或被占用），已阻断登录");
                        MessageBox.Show("无法计算本机 Script.pvf 的哈希，请重新选择客户端目录后再登录。",
                            "PVF校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                        ShowStatus("无法读取 Script.pvf，已阻止登录", Brushes.OrangeRed);
                        return null;
                    }
                }
                else
                {
                    // 本机缺少 Script.pvf：向服务端确认是否启用了 PVF 加密校验；
                    // 启用则阻断（无 PVF 无法校验/换钥），未启用才按未加密客户端放行。
                    var (latestOk, _, latestEnabled, _) =
                        await Services.RemoteAuthService.GetPvfLatestAsync(AppConfig.Settings);
                    if (latestOk && latestEnabled)
                    {
                        LogPvfDiag("pvf-missing", "本机没有Script.pvf且服务端启用了加密校验，已阻断登录（客户端不完整）");
                        MessageBox.Show("本机缺少 Script.pvf，且服务端已启用 PVF 加密校验。请先下载完整客户端或更新客户端后再登录。",
                            "PVF校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                        ShowStatus("缺少 Script.pvf，禁止登录", Brushes.OrangeRed);
                        return null;
                    }

                    LogPvfDiag(latestOk ? "no-encryption" : "latest-fail", "本机没有Script.pvf，服务端未启用加密校验，按未加密客户端放行");
                }
            }
#endif

            // 16 段参数：
            // 本地模式 host=服务器配置、channelPort=7001、initialGamePort=0、
            //   第 5 段 gameOuterToken 固定为旧版 instance.json 的 GameOuterToken（修复卡连接）；
            // 远程模式（S4 直连）host=远程服务器 IP、channelPort=7001（7002 认证通过后透传 m_id）；
            // 远程模式（90 LANA 实现）host=远程服务器 IP、channelPort=7001，账号识别由服务端
            //   验票完成（DNF_REMOTE_AUTH_* env 注入，90CN.dll 在 game 连接上握手）。
            string loginHost;
            string channelPort;
            string initialGamePort;
            string gameOuterToken;

            if (isRemote && Services.ServerManager.IsS4Profile(profile.Id))
            {
                // S4 远程直连：host=远程服务器 IP、channelPort=7001（7002 认证已通过，m_id 透传）
                loginHost = GetRemoteServerHost();
                channelPort = "7001";
                initialGamePort = "0";
                gameOuterToken = GameTokens.GameOuterToken;
            }
            else if (isRemote && ticket is not null)
            {
                // 90 远程：DNF.exe 直连服务器，90CN.dll 在 game 连接上完成 LANA 握手。
                loginHost = GetRemoteServerHost();
                channelPort = "7001";
                initialGamePort = "0";
                gameOuterToken = GameTokens.GameOuterToken;
            }
            else
            {
                loginHost = AppConfig.Settings.Host;
                channelPort = "7001";
                initialGamePort = "0";
                gameOuterToken = GameTokens.GameOuterToken;
            }

            // 启动参数：S4A21/S4A12 用版本预设参数（本地游戏S4.bat 原样内容）并注入当前登录账号；
            // 90 走内置 16 段参数逻辑（账号经 18111 管理端口按 PID 绑定，不依赖命令行账号段）。
            // S4 系列客户端 DfoServer 按启动参数第 4 段（m_id）识别账号并加载对应角色数据，
            // 原预设参数写死默认账号 10038，必须替换为当前登录账号，否则所有账号都以默认账号进游戏。
            string arguments;
            if (Services.ServerManager.IsS4Profile(profile.Id) && !string.IsNullOrWhiteSpace(profile.LaunchArgs))
            {
                var s4Segs = profile.LaunchArgs.Split('?');
                if (s4Segs.Length >= 4)
                {
                    s4Segs[3] = account;
                    if (isRemote)
                    {
                        // 远程直连：host 段替换为远程服务器 IP，端口段固定 7001
                        s4Segs[1] = loginHost;
                        s4Segs[2] = channelPort;
                    }
                }
                arguments = string.Join("?", s4Segs);
            }
            else if (!string.IsNullOrWhiteSpace(profile.LaunchArgs))
            {
                arguments = profile.LaunchArgs;
            }
            else
            {
                arguments = $"99?{loginHost}?{channelPort}?{initialGamePort}?{gameOuterToken}?01?1?0?0?0?0?1?{GameTokens.Tcls}?0?0?{GameTokens.Tail}";
            }

            var psi = new ProcessStartInfo
            {
                FileName = gamePath,
                Arguments = arguments,
                WorkingDirectory = directory,
                UseShellExecute = false
            };
            // 环境变量注入：DLL 挂钩开关 / 双开开关
            psi.Environment["DNF_HOOK_CREATE"] = "1";
            if (isSecondary)
            {
                psi.Environment["DNF_MULTI_CLIENT"] = "1";
            }
            // Inject account id for Helper DLL private notify filtering (slot ignore mismatch)
            if (!string.IsNullOrWhiteSpace(accountId))
            {
                psi.Environment["DNF_ACCOUNT_ID"] = accountId;
            }

            // 90 远程：注入 LANA 认证环境变量，由游戏目录内的 90CN.dll 在 game 连接上完成握手。
            //
            // 【端口列表必须覆盖服务端【全部】频道游戏端口】
            // 90CN.dll 只会对列表内的端口做 LANA 握手；漏掉的端口会表现为
            // 「登录成功、能看到频道列表，但选频道就掉线」——服务端日志是
            // game-launcher-auth-rejected reason=auth-frame-read-failed error=unexpected EOF。
            //
            // 下面这份与 channel_info.etc 展开出的 118 个频道端口一致（10006..10253）。
            // 以后改动频道表，请同步这里；或者让运维工具在登录响应里下发 game_ports，
            // 由 BuildRemoteAuthPorts() 优先采用（服务端目前尚未下发该字段）。
            if (isRemote && ticket is not null)
            {
                psi.Environment["DNF_REMOTE_AUTH_TICKET"] = ticket;
                psi.Environment["DNF_REMOTE_AUTH_HOST"] = GetRemoteServerHost();
                psi.Environment["DNF_REMOTE_AUTH_PORTS"] = BuildRemoteAuthPorts();

                // 机器码：90CN.dll 把它原样放进 LANA 认证帧的 machine_id 字段。
                // 在这之前 DLL 里写的是硬编码字面量 "90CN"，导致服务端看到的每个会话
                // 机器码都一样 —— 按机器码封禁踢人永远踢不到人，而误封 "90CN" 会踢全服。
                // 传真实硬件指纹（hw:<32hex>，见 Services/HardwareId.cs）；
                // 变量缺失/超长/非 ASCII 时 DLL 会退回旧的 "90CN"，老登录器不受影响。
                psi.Environment["DNF_REMOTE_AUTH_MACHINE"] = Services.HardwareId.MachineCode;

                // 登录器身份租约：90CN.dll 只把它原样附在【版本 2】的 LANA 应答里。
                // 服务端发版本 1 挑战（老服务端 / audit 模式）时这份租约根本不会被用到，
                // 所以对着没开强绑定的服务端多注入这个变量没有副作用；
                // 开了 enforce 的服务端则会因为缺少它直接拒绝连接。
                var lidLease = Services.LauncherAttestationService.BuildRemoteLease(ticket);
                if (!string.IsNullOrEmpty(lidLease))
                {
                    psi.Environment["DNF_LID_LEASE"] = lidLease;
                }
                else
                {
                    App.LogStartup("launcher identity: no remote lease to inject (" +
                        (Services.LauncherIdentity.Available ? "signing failed" : "no private key") + ")");
                }
            }

#if EXTERNAL
            // 90 PVF 加密换钥（方案B）：注入从运维工具拉取的 key_set（96 hex），
            // DNFHelper 读取后改写 DNF.exe 内 6 处 PVF 取钥点；未拉取（未加密客户端）时不注入。
            if (!string.IsNullOrEmpty(pvfKeySet))
            {
                psi.Environment["DNF_PVF_KEYSET"] = pvfKeySet;
            }
#endif

            // 90 远程：账号由 LANA 握手 + 服务端验票绑定，DNF_REMOTE_AUTH_* 环境变量已由上方注入，
            // 90CN.dll 在 game 连接上读取并完成认证，无需 VPN 隧道。

#if EXTERNAL
            // 服务端指定 DLL：登录时再拉一次清单（自检阶段已拉过一次，这里防"自检之后服务端改过"），
            // 在客户端根目录（DNF.exe 同目录）逐条校验文件名 + SHA256：
            //   block 级失败 → 弹窗说明 + **拒绝启动本次游戏**；
            //   skip  级失败 → 只记日志并跳过注入，照常进游戏。
            _serverDllChecks = new List<Services.ServerDllLoader.CheckResult>();
            if (isRemote)
            {
                var (dllHost, dllPort) = GetRemoteAuthEndpoint();
                if (!string.IsNullOrWhiteSpace(dllHost))
                {
                    await Services.RemoteAuthService.GetDllRequirementsAsync(
                        AppConfig.Settings, dllHost, dllPort).ConfigureAwait(true);
                }

                var requirements = new List<Services.ServerDllLoader.Requirement>();
                foreach (var item in Services.RemoteAuthService.ServerDllRequirements)
                {
                    requirements.Add(new Services.ServerDllLoader.Requirement
                    {
                        FileName = item.FileName,
                        Sha256 = item.Sha256,
                        OnMismatch = item.OnMismatch,
                    });
                }

                _serverDllChecks = Services.ServerDllLoader.Validate(directory, requirements);
                if (_serverDllChecks.Count > 0)
                {
                    App.LogStartup("服务端指定 DLL 校验：应校验 " + _serverDllChecks.Count + " 条，通过 " +
                        _serverDllChecks.Count(r => r.Ok) + " 条");
                    if (Services.ServerDllLoader.HasBlockingFailure(_serverDllChecks))
                    {
                        var blockMessage = Services.ServerDllLoader.BuildBlockMessage(_serverDllChecks);
                        App.LogStartup("服务端指定 DLL 校验未通过，已拒绝启动游戏：" +
                            Services.ServerDllLoader.BuildSkipNote(_serverDllChecks));
                        ShowStatus("客户端文件校验未通过，已拒绝启动游戏", Brushes.OrangeRed);
                        MessageBox.Show(blockMessage, "无法启动游戏", MessageBoxButton.OK, MessageBoxImage.Error);
                        return null;
                    }
                    var skipNote = Services.ServerDllLoader.BuildSkipNote(_serverDllChecks);
                    if (!string.IsNullOrEmpty(skipNote))
                    {
                        App.LogStartup("服务端指定 DLL 跳过注入：" + skipNote);
                    }
                }
            }
#endif

            try
            {
                var process = Process.Start(psi);
                if (process is null)
                {
                    ShowStatus("启动 DNF.exe 失败", Brushes.OrangeRed);
                    return null;
                }

                // 账号-PID 绑定：必须在游戏连接游戏服之前完成。DNF.exe 启动后立刻连接 dnfbridge，
                // 服务端按 PID 关联账号，未绑定的连接会被直接断开（日志 session_account_bound=false → abort），
                // 因此绑定要赶在 DLL 注入之前发出（对齐旧版 launchClient 顺序）；绑定失败弹窗并关闭游戏。
                // 远程模式：账号由服务端认证（S4 m_id / 90 LANA 验票）完成，无需 PID 绑定。
                if (AppConfig.Mode == LoginMode.Local && !string.IsNullOrWhiteSpace(accountId) && is90)
                {
                    var bindError = await Services.AccountBindingService.BindAsync(process.Id, accountId);
                    if (!string.IsNullOrEmpty(bindError))
                    {
                        ShowStatus(bindError, Brushes.OrangeRed);

                        // 绑定失败：服务端不认该 PID，游戏必然卡“正在连接服务器”，
                        // 弹窗强提示并关闭本次启动的 DNF.exe，拒绝本次登录。
                        try
                        {
                            process.Kill();
                            process.WaitForExit(3000);
                        }
                        catch
                        {
                            // 进程已自行退出则忽略
                        }

                        MessageBox.Show(
                            "账号绑定失败，已关闭本次启动的 DNF.exe。\n\n" +
                            "原因：" + bindError + "\n\n" +
                            "处理建议：\n" +
                            "· Admin Token 与服务端不一致时，请到 设置-服务器配置 更新令牌（可点击“从 instance.json 同步”）；\n" +
                            "· 无法连接时，请确认本机服务端管理接口已启动（127.0.0.1:18111）；若服务端在远程，请切换为外网模式。",
                            "登录失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return null;
                    }
                }

                // ⚠⚠ 注入顺序很重要，PVF 换钥的 Helper DLL 必须**第一个**注入（2026-09-21 改）。
                // 原因：DNFHelper 在 DLL_PROCESS_ATTACH 里改写 DNF.exe 的 6 处 PVF 取钥 call，
                // 必须赶在客户端读取 Script.pvf 之前装好。之前是"先注入守护 watchdog、再注入 Helper"，
                // 两个注入串行、每次又白等 5 秒（注入器的固定等待，已在 DllInjector 里改成轮询确认），
                // 结果 Helper 迟到 10.4 秒（玩家日志实锤）→ 换钥与客户端读脚本赛跑，时好时坏。
                // 现在：换钥 DLL 排第一，守护 watchdog 排它后面。
                // 这个 Helper DLL **所有版本都必须注入**：它不只是游戏助手顺图/拾取，
                // 还负责 PVF 换钥补丁（读环境变量 DNF_PVF_KEYSET，改写 6 处取钥 call）。
                // 外网版跑的是加密 PVF，少了这个 DLL 就是"拿到钥匙没人开门"。
#if EXTERNAL
                // ASSIST-OFF-EXTERNAL：外网版停用"游戏助手功能"（顺图/自动拾取），做法是把共享内存
                // 门控强制写 0。**必须写在注入之前** —— DNFHelper attach 时就读这两个标志，
                // 所以这段要排在下面的 Helper 注入前（顺序挪动时别把它落下）。
                Services.HelperSharedMemory.EnsureCreated();
                Services.HelperSharedMemory.SetPassEnabled(false);
                Services.HelperSharedMemory.SetPickupEnabled(false);
#endif
                if (is90)
                {
                    await Task.Run(() => Services.HelperDllDeployer.Inject(process.Id));
                }

                // 存活守护 watchdog 注入（全版本）：登录器被挂起/假死时心跳停更，
                // 游戏内 watchdog 5 秒内自杀游戏进程（Job 感知不到挂起，靠它兜底）。
                await Task.Run(() => Services.LauncherGuard.InjectWatchdog(process.Id));

#if !EXTERNAL
                // DLL 注入：设置页勾选列表，每个 DNF 进程都注入。
                // 仅限免费版（Free）编译：外网版严禁玩家 DLL 注入（反作弊底线），
                // 因此 EXTERNAL 下整段注入调用不编译进程序集。
                // 说明：配置是四个版本共享的 %APPDATA%\DNFLogin\config.json，
                // 玩家在免费版勾选的 DLL 会留在 Settings.Dlls 里；外网版虽然折叠了
                // DLL 管理入口（ApplyExternalUiLock），但注入若只靠 UI 隐藏，
                // 残留的勾选仍会被注入——所以这里用编译期开关做硬隔离。
                // 注意：本段只涉及玩家自定义 DLL，不影响登录器自带的
                // LauncherGuard（存活守护）、HelperDllDeployer（游戏助手）等内部 DLL。
                await Task.Run(() => DllManageControl.InjectSelectedDlls(process, AppConfig.Settings.Dlls));
#endif

#if EXTERNAL
                // 服务端指定 DLL 注入（外网版）：只注入登录前校验通过的那些（客户端根目录文件）。
                // 与"玩家自选 DLL"无关：这里注入什么是**服务端**决定的（管理工具维护的清单）。
                // 注入失败不阻断游戏（游戏已经在跑了），但必须弹窗 + 写日志，绝不静默。
                if (_serverDllChecks.Count > 0)
                {
                    var dllFailures = await Task.Run(() => Services.ServerDllLoader.Inject(process.Id, _serverDllChecks));
                    if (dllFailures.Count > 0)
                    {
                        App.LogStartup("服务端指定 DLL 注入失败：" + string.Join("；", dllFailures));
                        MessageBox.Show(
                            "以下服务器要求的 DLL 注入失败：\n\n" + string.Join("\n", dllFailures) +
                            "\n\n游戏会继续运行，但相关功能可能不可用；请尝试以管理员身份运行登录器。",
                            "DLL 注入失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                    }
                    else
                    {
                        var injected = new List<string>();
                        foreach (var result in _serverDllChecks)
                        {
                            if (result.Ok)
                            {
                                injected.Add(result.Requirement.FileName);
                            }
                        }
                        App.LogStartup("服务端指定 DLL 注入完成：" + string.Join("、", injected));
                    }
                }
#endif

#if !EXTERNAL
                // 游戏助手-DPS：开关开启时启动覆盖层（登录器内部实现，不注入 DLL）
                // 注意：必须在 UI 线程(STA)创建 WPF 覆盖层，不能用 Task.Run 包装
                if (AppConfig.Settings.DpsEnabled && is90)
                {
                    Services.DpsOverlayManager.Start(process);
                }
#endif

                // 进程托管
                GameProcessManager.RegisterProcess(slot, process, account);

                // 每次成功启动游戏落一行（账号 + 槽位 + pid，2026-09-26 加）：
                // 玩家报"点一次登录起了两个客户端"时，数一数 startup.log 里当天的
                // 「游戏已启动」行数 + helper_inject.log/guard.log 里的 pid 即可判定
                // 是不是登录器启动的第二个客户端（只有一行 = 第二个不是登录器起的）。
                // pid 读取用 try 包住：进程若已瞬间退出，Process.Id 会抛异常，不能因此
                // 让"启动成功"变成走下面 catch 的"启动失败"。
                var pidText = "?";
                try
                {
                    pidText = process.Id.ToString();
                }
                catch
                {
                    // 进程已退出，pid 读不到就不记具体值
                }

                App.LogStartup("游戏已启动：槽位=" + slot + " 账号=" + account + " pid=" + pidText);

                return process;
            }
            catch (Exception)
            {
                ShowStatus("启动 DNF.exe 失败，请确认游戏路径正确、文件未被占用或被杀毒软件拦截后重试", Brushes.OrangeRed);
                return null;
            }
        }

        // ── 进程托管事件 ────────────────────────────────────────

        /// <summary>某框对应游戏退出 → 恢复登录按钮、清除"已登录"状态</summary>
        private void OnGameExited(int slot)
        {
            // 该槽远程会话随之失效（心跳与 GM 事件轮询停止）
            RemoveRemoteSession(slot);

            // ==================== TEMP-8BOX 临时功能（临时测试用） ====================
            // 3 号 ~ 8 号槽位不属于主/副，必须在这里就地处理并 return：
            // 否则会掉进下面的 else 分支，把"副登录"的已登录状态误清掉。
            if (TempExtraLoginSlotSet.Contains(slot))
            {
                TempOnExtraSlotExited(slot);
                return;
            }
            // ================== TEMP-8BOX 临时功能结束 ==============================

            if (slot == GameProcessManager.MainSlot)
            {
                _mainLoggedIn = false;
                MainLoggedInText.Visibility = Visibility.Collapsed;
            }
            else
            {
                _secondaryLoggedIn = false;
                SecondaryLoggedInText.Visibility = Visibility.Collapsed;
            }

            RefreshLoginButtons();

#if PVFUPDATE
            // 游戏退出后进程句柄已释放：立即补做一次 PVF 更新自检（游戏运行期间轮询跳过）
            _ = CheckPvfUpdateAsync(true);
#endif

            // 游戏退出后进程句柄已释放：补删注入时未能删除的 Helper 临时 DLL
            Services.HelperDllDeployer.CleanupPending();
            Services.LauncherGuard.CleanupPending();

            // 远程模式：全部退出后释放 LANA 转发器与会话凭证（下次登录重新认证建连）
            if (!_mainLoggedIn && !_secondaryLoggedIn)
            {
                _lanaForwarder?.Dispose();
                _lanaForwarder = null;
                _remoteTicket = null;
                _remoteAccountId = null;
            }
        }

        /// <summary>全部游戏退出 → 托盘自动弹出登录器界面（exitGamePopup 开关控制）</summary>
        private void OnAllGamesExited()
        {
            if (!AppConfig.Settings.ExitGamePopup || _forceClose)
            {
                return;
            }

            if (!IsVisible)
            {
                RestoreFromTray();
            }
        }

        // ── 双开/清除/注册 ──────────────────────────────────────

        private void DualCheckBox_Checked(object sender, RoutedEventArgs e)
        {
            // 硬闸门（第三道）：后台关闭双开后绝不允许勾上。
            // 正常情况下勾选框已经被置灰、根本点不到；这里是防"键盘操作 / 样式变更 /
            // 代码路径绕过置灰"——顺手把勾去掉（会走 Unchecked 收起副登录框）并说明原因。
            if (!_multiOpenEnabled)
            {
                DualCheckBox.IsChecked = false;
                ShowStatus(MultiOpenDisabledText,
                    new SolidColorBrush(Color.FromRgb(0xE8, 0x8A, 0x2A)), "双开已关闭");
                return;
            }

            SecondaryLoginPanel.Visibility = Visibility.Visible;
            // 公告板让位：副登录框出现后收窄，避免整体超出窗口宽度
            UpdateNoticePanelWidth();

            // ==================== TEMP-8BOX 临时功能（临时测试用） ====================
            // 由 Temp8BoxEnabled 总开关控制（改成 false 即整体停用，无需改别处）。
            if (Temp8BoxEnabled)
            {
                OpenTempExtraLoginWindows();
            }
            // ================== TEMP-8BOX 临时功能结束 ==============================

            // 勾选双开且副框为空时，自动回填最近保存的、与主框不同的账号
            if (!string.IsNullOrWhiteSpace(SecondaryAccountTextBox.Text))
            {
                return;
            }

            var mainAccount = AccountTextBox.Text?.Trim() ?? string.Empty;
            var entries = CredentialManagerService.EnumerateAll(AppConfig.CurrentProfile.Id);
            entries.Sort((a, b) => b.LastWritten.CompareTo(a.LastWritten));
            foreach (var entry in entries)
            {
                if (!string.Equals(entry.Account, mainAccount, StringComparison.OrdinalIgnoreCase))
                {
                    SecondaryAccountTextBox.Text = entry.Account;
                    SecondaryPasswordBox.Password = entry.Password;
                    break;
                }
            }
        }

        private void DualCheckBox_Unchecked(object sender, RoutedEventArgs e)
        {
            SecondaryLoginPanel.Visibility = Visibility.Collapsed;
            // 副登录框收起 → 公告板恢复原宽
            UpdateNoticePanelWidth();

            // ==================== TEMP-8BOX 临时功能（临时测试用） ====================
            // 故意不加开关判断：万一在运行期把开关打开过，取消勾选时也要能收掉这些窗口
            // （开关关闭时这里就是空操作，无害）。
            CloseTempExtraLoginWindows();
            // ================== TEMP-8BOX 临时功能结束 ==============================
        }

        // ============================================================================
        // ==================== TEMP-8BOX 临时功能（临时测试用，测完注释掉） ============
        // 功能：勾选「双开」→ 额外弹出 3 号 ~ 8 号登录框（6 个）→ 一共 **8 开**。
        // 历史：2026-09-15 加四开；2026-09-16 停用；2026-09-20 启用后当日关停；
        //       2026-09-24 按要求扩到 8 开并重新启用；
        //       2026-09-25 按要求关停，同日按要求**重新启用**（当前为启用状态）。
        //
        // 关掉这个功能：把下面 Temp8BoxEnabled 改成 false 即可（其余一行都不用动）。
        // 彻底删除时：删掉本区块 + 删掉 TempExtraLoginWindow.xaml(.cs)
        //            + 删掉 GameProcessManager.cs 里带 TEMP-8BOX 标记的部分
        //            + 删掉 DualCheckBox_Checked/Unchecked、OnGameExited、HideToTray 处的开关分支。
        // ============================================================================

        /// <summary>
        /// TEMP-8BOX 总开关：true = 勾选「双开」时额外弹出 3 号 ~ 8 号登录框（共 8 开，临时测试用）；
        /// false = 多开停用（勾选双开只显示原来的副登录框，行为与加这个功能之前完全一致）。
        /// 2026-09-25：先按要求关停，同日按要求重新启用（改回 true）。
        /// </summary>
        private static bool Temp8BoxEnabled => false;

        /// <summary>TEMP-8BOX：额外登录框的槽位与标题（3 号 ~ 8 号）。</summary>
        private static readonly (int Slot, string Title)[] TempExtraLoginSlots =
        {
            (GameProcessManager.ExtraSlot3, "3 号登录"),
            (GameProcessManager.ExtraSlot4, "4 号登录"),
            (GameProcessManager.ExtraSlot5, "5 号登录"),
            (GameProcessManager.ExtraSlot6, "6 号登录"),
            (GameProcessManager.ExtraSlot7, "7 号登录"),
            (GameProcessManager.ExtraSlot8, "8 号登录"),
        };

        /// <summary>TEMP-8BOX：额外槽位集合（校验槽位号用）。</summary>
        private static readonly HashSet<int> TempExtraLoginSlotSet =
            new(TempExtraLoginSlots.Select(item => item.Slot));

        /// <summary>TEMP-8BOX：已打开的额外登录框（槽位 → 窗口；关闭后自动移除）</summary>
        private readonly Dictionary<int, TempExtraLoginWindow> _tempExtraLoginWindows = new();

        /// <summary>TEMP-8BOX：是否还有临时登录框开着（决定主窗口要不要躲到托盘）</summary>
        private bool TempExtraLoginWindowsOpen =>
            _tempExtraLoginWindows.Values.Any(window => window.IsLoaded);

        /// <summary>TEMP-8BOX：弹出 3 号 ~ 8 号登录框（已开的不重复弹，只激活）</summary>
        private void OpenTempExtraLoginWindows()
        {
            try
            {
                var opened = 0;
                foreach (var (slot, title) in TempExtraLoginSlots)
                {
                    if (_tempExtraLoginWindows.TryGetValue(slot, out var existing))
                    {
                        existing.Activate();
                        continue;
                    }
                    var window = new TempExtraLoginWindow(this, slot, title);
                    window.Closed += (_, _) => _tempExtraLoginWindows.Remove(slot);
                    _tempExtraLoginWindows[slot] = window;
                    // 不设 Owner：Owner 被 HideToTray 隐藏时，附属窗口会一起被隐藏，
                    // 而我们要的是"关掉登录器主界面后这些登录框还在"。
                    window.Show();
                    opened++;
                }

                ShowStatus(opened > 0
                    ? $"已弹出 3 号 ~ 8 号登录框（临时 8 开功能，共 8 开）"
                    : "3 号 ~ 8 号登录框已经在打开了", Brushes.Green);
            }
            catch (Exception ex)
            {
                // 临时功能，弹不出来也不能影响正常双开流程
                App.LogStartup("TEMP-8BOX 弹出额外登录框失败：" + ex.Message);
            }
        }

        /// <summary>TEMP-8BOX：关闭全部临时登录框（不杀已启动的游戏进程）</summary>
        private void CloseTempExtraLoginWindows()
        {
            try
            {
                foreach (var window in _tempExtraLoginWindows.Values.ToArray())
                {
                    window.Close();
                }
            }
            catch
            {
                // 关闭期间的竞态忽略即可
            }
            finally
            {
                _tempExtraLoginWindows.Clear();
            }
        }

        /// <summary>
        /// TEMP-8BOX：给临时登录框用——认证 → 登记该槽位会话 → 用主窗口同一条链路启动游戏。
        /// 只有 TempExtraLoginWindow 会调用它；返回 (false, 原因) 时错误已经可以显示给用户。
        /// </summary>
        internal async Task<(bool Ok, string Error)> TempLoginAndLaunchAsync(string account, string password, int slot)
        {
            // 硬闸门（第四道）：后台关闭双开后，TEMP-8BOX 的额外登录框也不许再开。
            // 当前线上 Temp8BoxEnabled=false（实际最大 2 开），但那个开关一旦被打开，
            // 这些窗口必须同样受双开开关约束 —— 否则"关双开"会被 3~8 号窗口整片绕过去。
            if (!_multiOpenEnabled)
            {
                return (false, MultiOpenDisabledText);
            }

            if (!TempExtraLoginSlotSet.Contains(slot))
            {
                return (false, "内部错误：临时多开只支持 3 号 ~ 8 号槽位");
            }

            if (!_isOnline)
            {
                return (false, AppConfig.Mode == LoginMode.Local
                    ? $"当前版本（{AppConfig.CurrentProfile?.Name ?? "未知"}）服务端未启动，禁止登录"
                    : "当前离线，无法登录");
            }

            // 登录前兜底（与主登录 1885 / 副登录 2107 同一条闸门）：临时 3~8 号窗口也必须过
            // 维护白名单校验，否则维护期间仍能从这些窗口进游戏（8 开扩容后影响面从 2 个窗口变 6 个）。
            // isSecondary=true：这些窗口属于"多开"，同样要复查双开后台开关。
            if (!await TryPreLoginGateAsync(account, isSecondary: true))
            {
                return (false, "服务器正在更新维护，暂不能登录");
            }

            var (authOk, accountId, authError, _, authTicket) = await TryAuthenticateAsync(account, password);
            if (!authOk)
            {
                return (false, string.IsNullOrWhiteSpace(authError) ? "账号或密码错误" : authError!);
            }

            // 90 远程：登记本槽位会话，否则该进程的 GM 踢人事件/心跳轮询会漏掉它。
            if (AppConfig.Mode == LoginMode.Remote &&
                (AppConfig.CurrentProfile?.Id ?? "") == "90" &&
                !string.IsNullOrWhiteSpace(accountId))
            {
                RecordRemoteSession(slot, authTicket ?? string.Empty, accountId, account);
            }

            // isSecondary: true → 注入 DNF_MULTI_CLIENT=1（多开开关），3/4 号与副号同理。
            var process = await LaunchGameAsync(account, slot, isSecondary: true, accountId, authTicket);
            if (process is null)
            {
                RemoveRemoteSession(slot);
                return (false, "启动游戏失败，详见主窗口底部状态提示");
            }

            return (true, string.Empty);
        }

        /// <summary>TEMP-8BOX：3 号 ~ 8 号槽位的游戏退出后，恢复对应登录框</summary>
        private void TempOnExtraSlotExited(int slot)
        {
            if (_tempExtraLoginWindows.TryGetValue(slot, out var window))
            {
                window.OnGameExited();
            }
        }

        // ================== TEMP-8BOX 临时功能结束 ==================================

        /// <summary>清除：不弹窗，直接清主/副两组输入框 + 凭据管理器中所有 DNFLogin_* 条目 + JSON 账号字段</summary>
        private void ClearButton_Click(object sender, RoutedEventArgs e)
        {
            AccountTextBox.Text = string.Empty;
            PasswordBox.Password = string.Empty;
            SecondaryAccountTextBox.Text = string.Empty;
            SecondaryPasswordBox.Password = string.Empty;

            CredentialManagerService.DeleteAll(AppConfig.CurrentProfile.Id);
            AppConfig.Settings.SetLastAccount(AppConfig.CurrentProfile.Id, string.Empty);
            AppConfig.Save();
        }

        private void RegisterButton_Click(object sender, RoutedEventArgs e)
        {
            if (_isLoggingIn || _mainLoggedIn)
            {
                return;
            }

            var registerWindow = new RegisterWindow
            {
                Owner = this
            };
            registerWindow.ShowDialog();

            // 注册成功后自动填写主框账号（不填密码）
            if (!string.IsNullOrWhiteSpace(registerWindow.RegisteredAccount))
            {
                AccountTextBox.Text = registerWindow.RegisteredAccount;
                PasswordBox.Password = string.Empty;
                // 新账号尚未保存本服务端凭据，取消记住密码，强制手动输入密码
                RememberCheckBox.IsChecked = false;
            }
        }

        // ── 回车快捷登录：主框 Enter → 主登录；副框 Enter → 副登录 ──

        private void LoginInput_KeyDown(object sender, KeyEventArgs e)
        {
            if (e.Key != Key.Enter)
            {
                return;
            }

            // 标记已处理（2026-09-26）：主登录按钮是 IsDefault="True"，WPF 的语义是"焦点在任何
            // 输入框里按回车 = 点这个默认按钮"，不置 Handled 时一次回车会同时触发本处理函数和
            // 默认按钮的 Click，等于点两次登录（线上"点一次起了两个客户端"的触发方式之一）。
            // 本窗口只有这 4 个输入框挂了 KeyDown，没有别的地方依赖回车继续冒泡。
            e.Handled = true;

            if (ReferenceEquals(sender, AccountTextBox) || ReferenceEquals(sender, PasswordBox))
            {
                if (!_isLoggingIn && !_mainLoggedIn)
                {
                    LoginButton_Click(sender, e);
                }
            }
            else if (ReferenceEquals(sender, SecondaryAccountTextBox) || ReferenceEquals(sender, SecondaryPasswordBox))
            {
                if (!_isSecondaryLoggingIn && !_secondaryLoggedIn)
                {
                    SecondaryLoginButton_Click(sender, e);
                }
            }
        }

        // ── 忘记密码（90 版本：账号 + 救援代码找回，无需原密码） ──

        private void ForgotPasswordButton_Click(object sender, RoutedEventArgs e)
        {
            if (_isLoggingIn || _isSecondaryLoggingIn)
            {
                return;
            }

            var profileId = AppConfig.CurrentProfile?.Id ?? "90";
            if (Services.ServerManager.IsS4Profile(profileId))
            {
                return;
            }

            var prefill = AccountTextBox.Text?.Trim() ?? string.Empty;
            var window = new ForgotPasswordWindow(
                AppConfig.Settings,
                AppConfig.Mode == LoginMode.Remote,
                prefill)
            {
                Owner = this
            };
            window.ShowDialog();
        }

        // ── 托盘 ────────────────────────────────────────────────

        /// <summary>
        /// 隐藏窗口并显示系统托盘图标。
        /// 悬浮文字固定为 LYLogin.exe（2026-10-15 需求：登录器自己的托盘 = LYLogin.exe，
        /// 连发 worker 那个托盘 = LYHelper.exe，两者别搞混）；图标用登录器内嵌的
        /// Assets\DNF.ico（与 exe 图标、窗口图标同一份），不再走 exe 关联图标那条路，
        /// 免得换壳/换名后托盘还是老图标。
        /// </summary>
        private void HideToTray()
        {
            if (_trayIcon is null)
            {
                _trayIcon = new System.Windows.Forms.NotifyIcon
                {
                    Visible = true
                };
                _trayIcon.DoubleClick += (_, _) => RestoreFromTray();

                // 右键菜单：打开主界面 / 退出
                var menu = new System.Windows.Forms.ContextMenuStrip();
                menu.Items.Add("打开主界面", null, (_, _) => BringToFront());
                menu.Items.Add(new System.Windows.Forms.ToolStripSeparator());
                menu.Items.Add("退出", null, (_, _) => ExitFromTray());
                _trayIcon.ContextMenuStrip = menu;

                // 悬浮提示文字（LYLogin.exe）
                _trayIcon.Text = "LYLogin.exe";

                // 托盘图标：优先用程序内嵌的 DNF.ico
                try
                {
                    var res = System.Windows.Application.GetResourceStream(
                        new Uri("pack://application:,,,/Assets/DNF.ico", UriKind.Absolute));
                    if (res?.Stream is { } iconStream)
                    {
                        using var ms = new System.IO.MemoryStream();
                        iconStream.CopyTo(ms);
                        ms.Position = 0;
                        // 通知区域用 16×16；ico 里没有该尺寸时由 GDI+ 就近缩放
                        _trayIcon.Icon = new System.Drawing.Icon(ms, 16, 16);
                    }
                }
                catch
                {
                    // 内嵌图标不可用时退回 exe 关联图标
                    try
                    {
                        var exePath = Environment.ProcessPath;
                        if (!string.IsNullOrEmpty(exePath))
                        {
                            _trayIcon.Icon = System.Drawing.Icon.ExtractAssociatedIcon(exePath);
                        }
                    }
                    catch
                    {
                        // 提取失败时保持默认空图标，不影响托盘功能
                    }
                }
            }
            else
            {
                _trayIcon.Visible = true;
            }

            Hide();
        }

        /// <summary>双击托盘图标恢复窗口</summary>
        private void RestoreFromTray()
        {
            if (_trayIcon is not null)
            {
                _trayIcon.Visible = false;
            }

            Show();
            WindowState = WindowState.Normal;
            Activate();
            Topmost = true;
            Topmost = false;
        }

        /// <summary>将窗口带到前台：窗口隐藏（托盘）时恢复显示，可见时直接激活</summary>
        public void BringToFront()
        {
            if (_trayIcon is not null)
            {
                RestoreFromTray();
                return;
            }

            if (WindowState == WindowState.Minimized)
            {
                WindowState = WindowState.Normal;
            }

            Show();
            Activate();
            Topmost = true;
            Topmost = false;
        }

        /// <summary>从托盘右键菜单真正退出登录器（OnClosing 中会结束所有 DNF 进程）</summary>
        private void ExitFromTray()
        {
            _forceClose = true;
            DisposeTrayIcon();
            Close();
        }

        /// <summary>清理托盘图标</summary>
        private void DisposeTrayIcon()
        {
            if (_trayIcon is null)
            {
                return;
            }

            _trayIcon.Visible = false;
            _trayIcon.Dispose();
            _trayIcon = null;
        }

        // ── 界面辅助 ────────────────────────────────────────────

        private void RootContainer_MouseLeftButtonDown(object sender, MouseButtonEventArgs e)
        {
            if (e.ButtonState == MouseButtonState.Pressed)
            {
                DragMove();
            }
        }

        private void FadeInWindow()
        {
            if (Opacity >= 1)
            {
                return;
            }

            var animation = new DoubleAnimation
            {
                To = 1,
                Duration = TimeSpan.FromMilliseconds(320),
                EasingFunction = new CubicEase { EasingMode = EasingMode.EaseOut }
            };

            BeginAnimation(OpacityProperty, animation);
        }

        private static BitmapImage LoadImage(Uri uri)
        {
            var bitmap = new BitmapImage();
            bitmap.BeginInit();
            bitmap.UriSource = uri;
            bitmap.CacheOption = BitmapCacheOption.OnLoad;
            bitmap.EndInit();
            bitmap.Freeze();
            return bitmap;
        }

        // ── 轮换背景 ────────────────────────────────────────────

        /// <summary>
        /// 初始化背景：优先使用自定义图片目录（按文件名自然排序），
        /// 目录为空/无效/无图片时回退内置打包背景图。
        /// 第一张同步加载立即显示，其余以 Background 优先级在 UI 空闲时逐张预加载，
        /// 全部就绪后启动轮换定时器（避免启动时一次性解码大图卡顿）。
        /// </summary>
        private void SetupBackground()
        {
            _bgTimer?.Stop();
            _bgTimer = null;
            _bgImages.Clear();
            _playOrder.Clear();
            _playPos = 0;
            _bgIndex = 0;

            // 构建待加载项：自定义目录 → 文件路径列表；否则 → 内置打包图
            var folder = AppConfig.Settings.BackgroundFolder?.Trim() ?? "";
            var files = Services.BackgroundService.CollectImageFiles(folder);
            List<Func<BitmapImage?>> loaders = new();
            if (files.Count > 0)
            {
                foreach (var f in files)
                {
                    loaders.Add(() => LoadImageFile(f));
                }
            }
            else
            {
                foreach (var uri in BackgroundUris)
                {
                    loaders.Add(() => LoadImage(uri));
                }
            }

            if (loaders.Count == 0)
            {
                HideBgProgress();
                return;
            }

            // 第一张同步加载，立即显示
            var first = loaders[0]();
            if (first is null)
            {
                HideBgProgress();
                return;
            }

            _bgImages.Add(first);
            BackgroundLayer1.Source = first;
            BackgroundLayer1.Opacity = 1;
            BackgroundLayer2.Opacity = 0;

            // 其余图片异步预加载，全部尝试完成后统一收尾
            int remaining = loaders.Count - 1;
            if (remaining == 0)
            {
                OnAllBackgroundLoaded();
                return;
            }

            for (int i = 1; i < loaders.Count; i++)
            {
                int idx = i;
                Dispatcher.BeginInvoke(DispatcherPriority.Background, new Action(() =>
                {
                    var img = loaders[idx]();
                    if (img is not null)
                    {
                        _bgImages.Add(img);
                    }

                    if (System.Threading.Interlocked.Decrement(ref remaining) == 0)
                    {
                        OnAllBackgroundLoaded();
                    }
                }));
            }
        }

        /// <summary>从本地文件加载图片，损坏文件返回 null（调用方跳过）</summary>
        private static BitmapImage? LoadImageFile(string path)
        {
            try
            {
                var bitmap = new BitmapImage();
                bitmap.BeginInit();
                bitmap.UriSource = new Uri(path);
                bitmap.CacheOption = BitmapCacheOption.OnLoad;
                bitmap.EndInit();
                bitmap.Freeze();
                return bitmap;
            }
            catch
            {
                return null;
            }
        }

        /// <summary>
        /// 全部图片加载尝试完毕后的收尾：
        /// 构建播放顺序并显示第一张；只有 1 张图时不轮换、进度条隐藏。
        /// </summary>
        private void OnAllBackgroundLoaded()
        {
            if (_bgImages.Count == 0)
            {
                HideBgProgress();
                return;
            }

            RebuildPlayOrder(keepFirst: true);
            BackgroundLayer1.Source = _bgImages[_playOrder[0]];
            BackgroundLayer1.Opacity = 1;
            BackgroundLayer2.Opacity = 0;

            if (_bgImages.Count < 2)
            {
                // 只有 1 张图：不轮换，进度条消失
                HideBgProgress();
                return;
            }

            ShowBgProgress();
            StartBgRotation();
        }

        /// <summary>
        /// 构建播放顺序：顺序轮播=按文件名自然序；随机轮播=洗牌循环。
        /// keepFirst=true 时第一个位置保持 0（初始同步显示的那张），其余洗牌；
        /// 一轮播完后调用 keepFirst=false 全量洗牌。
        /// </summary>
        private void RebuildPlayOrder(bool keepFirst = false)
        {
            _playOrder.Clear();
            for (int i = 0; i < _bgImages.Count; i++)
            {
                _playOrder.Add(i);
            }

            if (!AppConfig.Settings.BackgroundShuffle || _playOrder.Count <= 1)
            {
                return;
            }

            var rng = new Random();
            int start = keepFirst ? 1 : 0;
            for (int i = _playOrder.Count - 1; i > start; i--)
            {
                int j = rng.Next(start, i + 1);
                (_playOrder[i], _playOrder[j]) = (_playOrder[j], _playOrder[i]);
            }
        }

        /// <summary>重新加载背景：停止旧轮换与进度条动画，按当前配置重建（设置变更后立即生效）</summary>
        public void ReloadBackground()
        {
            _bgTimer?.Stop();
            _bgTimer = null;
            BgProgressBar.BeginAnimation(FrameworkElement.WidthProperty, null);
            SetupBackground();
        }

        private void StartBgRotation()
        {
            if (_bgTimer is not null)
            {
                return;
            }

            _bgTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(BgRotationSeconds) };
            _bgTimer.Tick += OnBgRotationTick;
            _bgTimer.Start();

            // 进度条与轮换计时同步：启动即从 0 开始线性读条
            RestartBgProgress();
        }

        /// <summary>
        /// 重置并重启底部进度条：从 0 线性增长到窗口宽度，时长与轮换周期一致（5 秒）。
        /// 无 EasingFunction 即线性增长。
        /// </summary>
        private void RestartBgProgress()
        {
            if (BgProgressBar is null || BgProgressWrap.Visibility != Visibility.Visible)
            {
                return;
            }

            var targetWidth = Math.Max(RootContainer.ActualWidth, 1);

            // 停止旧动画并归零，避免残留值
            BgProgressBar.BeginAnimation(FrameworkElement.WidthProperty, null);
            BgProgressBar.Width = 0;

            var animation = new DoubleAnimation(0, targetWidth, TimeSpan.FromSeconds(BgRotationSeconds));
            animation.Freeze();
            BgProgressBar.BeginAnimation(FrameworkElement.WidthProperty, animation);
        }

        /// <summary>显示底部进度条</summary>
        private void ShowBgProgress()
        {
            BgProgressWrap.Visibility = Visibility.Visible;
        }

        /// <summary>隐藏底部进度条</summary>
        private void HideBgProgress()
        {
            BgProgressBar.BeginAnimation(FrameworkElement.WidthProperty, null);
            BgProgressWrap.Visibility = Visibility.Collapsed;
        }

        /// <summary>
        /// 每 5 秒轮换：后层载入下一张图并淡入，前层淡出，纯交叉过渡无缩放。
        /// 播放顺序走 _playOrder：一轮播完（随机模式）重新洗牌。
        /// </summary>
        private void OnBgRotationTick(object? sender, EventArgs e)
        {
            if (_bgImages.Count < 2)
            {
                return;
            }

            // 切换完成：进度条重置，重新开始下一轮读条
            RestartBgProgress();

            _playPos++;
            if (_playPos >= _playOrder.Count)
            {
                // 一轮播完：随机模式重新洗牌，顺序模式回到开头
                RebuildPlayOrder();
                _playPos = 0;
            }

            _bgIndex = _playOrder[_playPos];

            BackgroundLayer2.Source = _bgImages[_bgIndex];
            BackgroundLayer2.Opacity = 0;

            var duration = TimeSpan.FromMilliseconds(BgFadeMilliseconds);
            var fadeIn = new DoubleAnimation(0, 1, duration)
            {
                EasingFunction = new CubicEase { EasingMode = EasingMode.EaseInOut }
            };
            var fadeOut = new DoubleAnimation(1, 0, duration)
            {
                EasingFunction = new CubicEase { EasingMode = EasingMode.EaseInOut }
            };

            fadeIn.Completed += (_, _) =>
            {
                // 过渡完成：前层承接新图，后层复位待用
                BackgroundLayer1.Source = BackgroundLayer2.Source;
                BackgroundLayer1.Opacity = 1;
                BackgroundLayer2.Opacity = 0;
            };

            BackgroundLayer2.BeginAnimation(UIElement.OpacityProperty, fadeIn);
            BackgroundLayer1.BeginAnimation(UIElement.OpacityProperty, fadeOut);
        }

        /// <summary>
        /// 提示：迁移到右上侧气泡 Toast（保留原签名，title 并入正文展示）
        /// </summary>
        private void ShowStatus(string message, System.Windows.Media.Brush brush, string? title = null)
        {
            var text = string.IsNullOrWhiteSpace(title) || string.Equals(title, "提示", System.StringComparison.Ordinal)
                ? message
                : title + "：" + message;
            ShowToast(text, brush);
        }

        // ── 右上侧 Toast 通知系统 ─────────────────────────────────

        /// <summary>
        /// 右上侧气泡 Toast：右滑入 + 淡入（约 250ms），3 秒后自动滑出收回；
        /// 多条依次排列在 ToastContainer 中，各自独立计时。
        /// </summary>
        private void ShowToast(string message, System.Windows.Media.Brush? accent = null)
        {
            var accentBrush = accent ?? new SolidColorBrush(Color.FromRgb(0xE8, 0x4A, 0x3A));

            var border = new Border
            {
                Background = new SolidColorBrush(Color.FromArgb(0xF2, 0x18, 0x18, 0x1C)),
                CornerRadius = new CornerRadius(10),
                Margin = new Thickness(0, 0, 0, 8)
            };

            var grid = new Grid();
            grid.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(3) });
            grid.ColumnDefinitions.Add(new ColumnDefinition());

            var bar = new Border
            {
                Background = accentBrush,
                CornerRadius = new CornerRadius(1.5)
            };
            Grid.SetColumn(bar, 0);

            var text = new TextBlock
            {
                Text = message,
                Foreground = Brushes.White,
                FontSize = 13,
                TextWrapping = TextWrapping.Wrap,
                VerticalAlignment = VerticalAlignment.Center,
                Margin = new Thickness(12, 10, 14, 10)
            };
            Grid.SetColumn(text, 1);

            grid.Children.Add(bar);
            grid.Children.Add(text);
            border.Child = grid;

            var translate = new TranslateTransform { X = 32 };
            border.RenderTransform = translate;
            border.Opacity = 0;
            ToastContainer.Children.Add(border);

            // 进入动画：从右滑入 + 淡入（250ms）
            var slideIn = new DoubleAnimation(32, 0, TimeSpan.FromMilliseconds(250))
            {
                EasingFunction = new CubicEase { EasingMode = EasingMode.EaseOut }
            };
            var fadeIn = new DoubleAnimation(0, 1, TimeSpan.FromMilliseconds(250));
            translate.BeginAnimation(TranslateTransform.XProperty, slideIn);
            border.BeginAnimation(OpacityProperty, fadeIn);

            // 3 秒后自动收回
            var timer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(3) };
            timer.Tick += (_, _) =>
            {
                timer.Stop();

                var slideOut = new DoubleAnimation(0, 32, TimeSpan.FromMilliseconds(250))
                {
                    EasingFunction = new CubicEase { EasingMode = EasingMode.EaseIn }
                };
                var fadeOut = new DoubleAnimation(1, 0, TimeSpan.FromMilliseconds(250));
                fadeOut.Completed += (_, _) => ToastContainer.Children.Remove(border);
                translate.BeginAnimation(TranslateTransform.XProperty, slideOut);
                border.BeginAnimation(OpacityProperty, fadeOut);
            };
            timer.Start();
        }

        /// <summary>
        /// 启动自检结果处理：缺失配置时自动打开设置页对应导航页并 Toast 提示；
        /// 缺失资源仅 Toast 提示（设置页无资源配置项）。
        /// </summary>
        private async void HandleStartupCheckAfterLoad()
        {
            if (_startupCheckHandled)
            {
                return;
            }
            _startupCheckHandled = true;

            var result = MainWindow.LastCheckResult;
            if (result is null)
            {
                return;
            }

            if (result.MissingResources.Count > 0)
            {
                ShowToast("资源缺失：" + string.Join("、", result.MissingResources), Brushes.OrangeRed);
            }

            string? page = null;
            string? toast = null;
            if (result.ServerConfigMissing && result.GamePathMissing)
            {
                page = "general";
                toast = result.AdminTokenMissing ? "请配置游戏目录、服务器和 Admin Token" : "请配置游戏目录和服务器";
            }
            else if (result.ServerConfigMissing)
            {
                page = "server";
                toast = result.AdminTokenMissing ? "请配置服务器并填写 Admin Token" : "请配置服务器";
            }
            else if (result.GamePathMissing)
            {
                page = "general";
                toast = "请选择游戏目录";
            }
            else if (result.AdminTokenMissing)
            {
                page = "server";
                toast = "请填写 Admin Token";
            }

            if (page is null)
            {
                return;
            }

            ShowToast(toast ?? "");
            // 等 Toast 滑入动画完成后再切换到对应页签（Toast 仍可见）
            await System.Threading.Tasks.Task.Delay(350);

            // 原"通用/服务器配置"两页已并入游戏配置页，统一跳该页签
            NavConfig.IsChecked = true;
        }
    }
}
