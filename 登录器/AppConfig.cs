using System;
using System.Collections.Generic;
using System.IO;
using System.Text.Json;

namespace DNFLogin
{
    /// <summary>
    /// 关闭窗口时的行为
    /// </summary>
    public enum CloseAction
    {
        /// <summary>最小化到系统托盘</summary>
        MinimizeToTray,

        /// <summary>退出启动器</summary>
        Exit
    }

    /// <summary>
    /// 登录模式
    /// </summary>
    public enum LoginMode
    {
        /// <summary>本地模式：直连 MySQL 验证，固定 GameOuterToken，启动后 PID 绑定账号</summary>
        Local,

        /// <summary>远程模式：S4 走 7002 认证直连；90 走 HTTPS 取 ticket + LANA env 注入直连，顶号由服务端踢包</summary>
        Remote
    }

    /// <summary>
    /// 与旧版 Go 登录器对齐的固定启动参数令牌
    /// </summary>
    public static class GameTokens
    {
        /// <summary>第 5 段 GameOuterToken：instance.json 固定值，所有账号共用（本地不卡连接的关键）</summary>
        public const string GameOuterToken = "de509f65e9ccaae621cb7278fc2b8e6c";

        /// <summary>第 13 段 tcls 固定值</summary>
        public const string Tcls = "9n2b1c8r3w7y";

        /// <summary>末段固定值</summary>
        public const string Tail = "19847";

        /// <summary>多账号转发总部控制端口门禁令牌（转发器 --control-token 需同值），暂时写死内置，玩家不可见</summary>
        public const string DefaultControlToken = "dnf90-hq-2026";
    }

    /// <summary>
    /// DLL 注入配置项
    /// </summary>
    public sealed class DllEntry
    {
        public string Path { get; set; } = "";

        public bool Enabled { get; set; } = true;
    }

    /// <summary>
    /// 客户端版本配置（90 / S4A21 / S4A12 各自独立）
    /// 每个版本拥有独立的游戏路径、服务端根目录、服务器配置、AdminToken 与登录模式。
    /// </summary>
    public sealed class ClientProfile
    {
        /// <summary>版本唯一标识：90 / S4A21 / S4A12</summary>
        public string Id { get; set; } = "90";

        /// <summary>版本显示名</summary>
        public string Name { get; set; } = "90";

        // 服务器配置六项
        public string Mode { get; set; } = "portable";
        public string Host { get; set; } = "127.0.0.1";
        public string Port { get; set; } = "13306";
        public string Name2 { get; set; } = "dnf_local";
        public string User { get; set; } = "root";
        public string Password { get; set; } = "aa123456";

        // SQLite 模式数据库文件路径
        public string DatabasePath { get; set; } = "";

        // 登录模式（Local / Remote）与远程服务器配置
        public string LoginMode { get; set; } = BuildParameters.LoginMode;
        public string RemoteServerUrl { get; set; } = "";
        public string RemoteIp { get; set; } = BuildParameters.RemoteIp;
        public string RemotePort { get; set; } = BuildParameters.RemotePort;
        public string RemoteCAFile { get; set; } = "";
        public string InviteCode { get; set; } = "";


        // 转发器总部（多账号方案）：登录前 POST /auth 上报账号做源IP关联 + 顶号；
        // 客户端连接总部转发端口（RemotePort），总部按源IP拉起独立子进程进频道
        public string HeadquartersUrl { get; set; } = "";
        public string ControlToken { get; set; } = "";

        // 服务端管理接口令牌（本地模式 PID 绑定）
        public string AdminToken { get; set; } = "";

        // 游戏
        public string GamePath { get; set; } = "";

        // 服务端一键启动根目录
        public string ServerRootPath { get; set; } = "";

        /// <summary>
        /// 客户端启动参数串（DNF.exe 后的完整参数，如 "9?127.0.0.1?7001?..."）。
        /// 90 留空走登录器内置 16 段参数逻辑；S4A21/S4A12 预填各自"本地游戏S4.bat"脚本内容。
        /// </summary>
        public string LaunchArgs { get; set; } = "";

        /// <summary>从全局顶层字段同步（老配置迁移 / 设置页保存后同步）</summary>
        public void CopyFromTopLevel(AppSettings top)
        {
            Mode = top.Mode;
            Host = top.Host;
            Port = top.Port;
            Name2 = top.Name;
            User = top.User;
            Password = top.Password;
            DatabasePath = top.DatabasePath;
            LoginMode = top.LoginMode;
            RemoteServerUrl = top.RemoteServerUrl;
            RemoteIp = top.RemoteIp;
            RemotePort = top.RemotePort;
            RemoteCAFile = top.RemoteCAFile;
            InviteCode = top.InviteCode;
            HeadquartersUrl = top.HeadquartersUrl;
            ControlToken = top.ControlToken;
            AdminToken = top.AdminToken;
            GamePath = top.GamePath;
            ServerRootPath = top.ServerRootPath;
        }

        /// <summary>把本版本配置写回全局顶层字段（版本切换时镜像，老代码零改动读顶层）</summary>
        public void CopyToTopLevel(AppSettings top)
        {
            top.Mode = Mode;
            top.Host = Host;
            top.Port = Port;
            top.Name = Name2;
            top.User = User;
            top.Password = Password;
            top.DatabasePath = DatabasePath;
            top.LoginMode = LoginMode;
            top.RemoteServerUrl = RemoteServerUrl;
            top.RemoteIp = RemoteIp;
            top.RemotePort = RemotePort;
            top.RemoteCAFile = RemoteCAFile;
            top.InviteCode = InviteCode;
            top.HeadquartersUrl = HeadquartersUrl;
            top.ControlToken = ControlToken;
            top.AdminToken = AdminToken;
            top.GamePath = GamePath;
            top.ServerRootPath = ServerRootPath;
        }

        /// <summary>生成三个版本的默认配置（90 / S4A21 / S4A12）</summary>
        public static List<ClientProfile> CreateDefaults()
        {
            return new List<ClientProfile>
            {
                new() { Id = "90", Name = "90" },
                new()
                {
                    Id = "S4A21",
                    Name = "S4A21",
                    // 本地游戏S4.bat（S4A21_CN 客户端）原样内容
                    LaunchArgs = "9?127.0.0.1?7001?10038?de509f65e9ccaae621cb7278fc2b8e6c?1?0?6?0?2?1???0?0?0?9n2b1c8r3w7y?0?0?0?0?0?2?0?0?0?0?0?0?0?0?0?0?0?4D87"
                },
                new()
                {
                    Id = "S4A12",
                    Name = "S4A12",
                    // 本地游戏S4.bat（夜空魅影 客户端）原样内容
                    LaunchArgs = "99?127.0.0.1?7001?10038?de509f65e9ccaae621cb7278fc2b8e6c?01?1?0?0?0?0?1?9n2b1c8r3w7y?0?0?19847"
                }
            };
        }
    }

    /// <summary>
    /// 按键连发配置项（一个按键一行）
    /// </summary>
    public sealed class AutoFireKeyConfig
    {
        /// <summary>显示键名，如 A / X / F1 / Space / LCtrl（空行不生效）</summary>
        public string Key { get; set; } = "";

        /// <summary>连发间隔（毫秒），最小 1</summary>
        public int IntervalMs { get; set; } = 10;
    }

    /// <summary>
    /// 持久化设置（AppData\DNFLogin\config.json）
    /// </summary>
    public sealed class AppSettings
    {
        // 服务器配置六项（模式保留，仅作标记；数据库连接字段即服务器配置）
        public string Mode { get; set; } = "portable";
        public string Host { get; set; } = "127.0.0.1";
        public string Port { get; set; } = "13306";
        public string Name { get; set; } = "dnf_local";
        public string User { get; set; } = "root";
        public string Password { get; set; } = "aa123456";

        // SQLite 模式数据库文件路径（来自 instance.json database.path，相对 runtime 或绝对路径）
        public string DatabasePath { get; set; } = "";

        // 登录模式（Local / Remote）与远程服务器配置
        public string LoginMode { get; set; } = BuildParameters.LoginMode;
        public string RemoteServerUrl { get; set; } = "";
        public string RemoteIp { get; set; } = BuildParameters.RemoteIp;
        public string RemotePort { get; set; } = BuildParameters.RemotePort;
        public string RemoteCAFile { get; set; } = "";
        public string InviteCode { get; set; } = "";


        // 转发器总部（多账号方案）：登录前 POST /auth 上报账号做源IP关联 + 顶号；
        // 客户端连接总部转发端口（RemotePort），总部按源IP拉起独立子进程进频道
        public string HeadquartersUrl { get; set; } = "";
        public string ControlToken { get; set; } = "";

        // 服务端管理接口令牌：本地模式绑定账号时作为 X-Admin-Token 头
        // （替代原先探测 instance.json 的 adminToken），远程模式不参与 PID 绑定
        public string AdminToken { get; set; } = "";

        // 登录与行为
        public string LastAccount { get; set; } = "";
        /// <summary>记住的最近账号按版本隔离：profileId → 账号名（90 兼容旧 LastAccount）</summary>
        public Dictionary<string, string> LastAccountByProfile { get; set; } = new();

        /// <summary>取指定版本记住的最近账号；90 无记录时兼容旧字段 LastAccount</summary>
        public string GetLastAccount(string profileId)
        {
            if (LastAccountByProfile.TryGetValue(profileId, out var account) && !string.IsNullOrWhiteSpace(account))
            {
                return account;
            }

            if (string.IsNullOrWhiteSpace(profileId) ||
                string.Equals(profileId, "90", StringComparison.OrdinalIgnoreCase))
            {
                return LastAccount ?? string.Empty;
            }

            return string.Empty;
        }

        /// <summary>记录指定版本记住的最近账号</summary>
        public void SetLastAccount(string profileId, string account)
        {
            LastAccountByProfile[profileId] = account;
            if (string.IsNullOrWhiteSpace(profileId) ||
                string.Equals(profileId, "90", StringComparison.OrdinalIgnoreCase))
            {
                LastAccount = account;
            }
        }
        public string CloseAction { get; set; } = "MinimizeToTray";
        // 关闭行为是否已询问并记住：0或缺失=首次点击X询问；1=已记住不再询问（选择存于 CloseAction）
        public int CloseActionAsked { get; set; } = 0;

        // GM 事件游标持久化：避免登录器重启后回放历史 kick / cera_granted 事件（泡点到账重复弹公告）
        public long LastGmEventId { get; set; } = 0;
        public bool ExitGamePopup { get; set; } = true;

        // 游戏
        public string GamePath { get; set; } = "";

        // 服务端一键启动：DNF90 服务端根目录（含 runtime 的目录，空=未配置）
        public string ServerRootPath { get; set; } = "";

        // 游戏助手：DPS 伤害统计开关（默认开启）
        public bool DpsEnabled { get; set; } = true;

        // 游戏助手：按键连发总开关（默认关闭）
        public bool AutoFireEnabled { get; set; } = false;

        // 游戏助手：按键连发配置列表
        public List<AutoFireKeyConfig> AutoFireKeys { get; set; } = new();

        // 游戏助手-顺图/拾取：内嵌 Helper DLL 功能开关（共享内存门控，默认关闭）
        public bool HelperPassEnabled { get; set; } = false;
        public bool HelperPickupEnabled { get; set; } = false;

        // 背景：自定义图片目录（空=默认轮换打包进 exe 的背景图）
        public string BackgroundFolder { get; set; } = "";

        // 背景轮播方式：false=顺序轮播（按文件名自然排序），true=随机轮播（洗牌循环）
        public bool BackgroundShuffle { get; set; } = false;

        // DLL 注入勾选列表
        public List<DllEntry> Dlls { get; set; } = new();

        // ── 多版本配置（ClientProfile 拆分） ──
        /// <summary>全部客户端版本配置（90 / S4A21 / S4A12）</summary>
        public List<ClientProfile> Profiles { get; set; } = new();

        /// <summary>当前选中版本 Id</summary>
        public string CurrentProfileId { get; set; } = "90";
    }

    /// <summary>
    /// 运行期全局配置：内存态 + JSON 持久化（AppData\DNFLogin\config.json）
    /// </summary>
    public static class AppConfig
    {
        private static readonly string ConfigDirectory =
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin");

        /// <summary>配置文件路径</summary>
        public static string ConfigPath { get; } = Path.Combine(ConfigDirectory, "config.json");

        /// <summary>当前设置</summary>
        public static AppSettings Settings { get; private set; } = new();

        /// <summary>
        /// 是否已执行"清除所有配置"：置 true 后 Save() 直接跳过写入，
        /// 防止退出流程（关窗兜底等）把旧配置重新写回磁盘。
        /// </summary>
        public static bool ConfigCleared { get; set; }

        /// <summary>
        /// 关闭窗口的行为（映射到 Settings.CloseAction，重启后从 JSON 恢复）
        /// </summary>
        public static CloseAction CloseAction
        {
            get => Enum.TryParse<CloseAction>(Settings.CloseAction, out var action)
                ? action
                : CloseAction.MinimizeToTray;
            set => Settings.CloseAction = value.ToString();
        }

        /// <summary>
        /// 登录模式（Local / Remote），映射到 Settings.LoginMode
        /// </summary>
        public static LoginMode Mode
        {
            get => Enum.TryParse<LoginMode>(Settings.LoginMode, out var mode) ? mode : LoginMode.Local;
            set => Settings.LoginMode = value.ToString();
        }

#if EXTERNAL
        /// <summary>
        /// 外网版固定 EditionProfileId（M3 锁版本）：90 外网版固定 "90"，A21/A12 外网版同理。
        /// 由 Edition 构建体系（csproj DefineConstants）决定，任何分支只编译其一。
        /// </summary>
#if V90
        public const string EditionProfileId = "90";
#elif VS4A21
        public const string EditionProfileId = "S4A21";
#elif VS4A12
        public const string EditionProfileId = "S4A12";
#else
        public const string EditionProfileId = "90";
#endif
#endif

        /// <summary>
        /// 当前客户端版本配置（按 CurrentProfileId 返回；缺失时兜底默认 90）
        /// </summary>
        public static ClientProfile CurrentProfile
        {
            get
            {
                EnsureProfiles();
                return Settings.Profiles.Find(p => p.Id == Settings.CurrentProfileId)
                       ?? Settings.Profiles[0];
            }
        }

        /// <summary>
        /// 切换当前版本：把旧版本顶层字段镜像回原 profile，再加载新 profile 到顶层。
        /// 老代码统一读 AppConfig.Settings 顶层字段，零改动即可按版本生效。
        /// </summary>
        public static void SwitchProfile(string id)
        {
            EnsureProfiles();
#if EXTERNAL
            // 外网版锁版本（M3）：任何切换请求都锁定为本版 Profile；已激活本版则直接返回，避免无谓自镜像与落盘
            if (string.Equals(Settings.CurrentProfileId, EditionProfileId, StringComparison.Ordinal))
            {
                return;
            }

            id = EditionProfileId;
#endif
            var current = CurrentProfile;
            current.CopyFromTopLevel(Settings);
            Settings.CurrentProfileId = id;
            var next = CurrentProfile;
            next.CopyToTopLevel(Settings);
            Save();
        }

        /// <summary>确保三版本（90/S4A21/S4A12）齐全；老配置（无 Profiles 字段）首次加载时兜成默认 90 并补齐缺失版本</summary>
        private static void EnsureProfiles()
        {
            if (Settings.Profiles is null)
            {
                Settings.Profiles = new List<ClientProfile>();
            }

            var defaults = ClientProfile.CreateDefaults();

            // 老配置迁移：90 尚不存在时，用顶层字段生成默认 90
            var has90 = false;
            foreach (var p in Settings.Profiles)
            {
                if (p.Id == "90")
                {
                    has90 = true;
                    break;
                }
            }

            if (!has90)
            {
                var fallback = defaults[0];
                fallback.CopyFromTopLevel(Settings);
                Settings.Profiles.Add(fallback);
            }

            // 补齐缺失版本（S4A21 / S4A12）
            foreach (var def in defaults)
            {
                if (def.Id == "90")
                {
                    continue;
                }

                var exists = false;
                foreach (var p in Settings.Profiles)
                {
                    if (p.Id == def.Id)
                    {
                        exists = true;
                        break;
                    }
                }

                if (!exists)
                {
                    Settings.Profiles.Add(def);
                }
            }

            if (string.IsNullOrWhiteSpace(Settings.CurrentProfileId))
            {
                Settings.CurrentProfileId = "90";
            }
        }

        /// <summary>
        /// 启动时读取 JSON 填充配置；读取失败时使用默认值
        /// </summary>
        public static void Load()
        {
            try
            {
                if (!File.Exists(ConfigPath))
                {
                    Settings = new AppSettings();
                    ApplyBuildProfileDefaults();
#if EXTERNAL
                    // 外网版：无配置文件时也按本版默认落盘（锁版本在读取之前即生效）
                    ApplyEditionLock();
#endif
                    Save();
                    return;
                }

                var json = File.ReadAllText(ConfigPath);
                var loaded = JsonSerializer.Deserialize<AppSettings>(json);
                if (loaded is not null)
                {
                    Settings = loaded;
                    Settings.Dlls ??= new List<DllEntry>();
                    Settings.AutoFireKeys ??= new List<AutoFireKeyConfig>();
                    // 多版本：老配置无 Profiles 时兜成默认 90，并落盘迁移
                    EnsureProfiles();
                    // 老配置迁移：顶层 GamePath 有值而当前版本 Profile 为空时补同步，
                    // 防止历史版本"仅写顶层"的客户端目录在外网版 ApplyEditionLock（profile 覆盖顶层）
                    // 或版本切换镜像时被空 profile 覆盖，导致重启后回填丢失。
                    if (!string.IsNullOrWhiteSpace(Settings.GamePath))
                    {
                        var activeProfile = CurrentProfile;
                        if (activeProfile is not null && string.IsNullOrWhiteSpace(activeProfile.GamePath))
                        {
                            activeProfile.GamePath = Settings.GamePath;
                        }
                    }
                    ApplyBuildProfileDefaults();
#if EXTERNAL
                    // 外网版锁版本（M3）：无论磁盘里是哪个版本配置，强制当前版本为本版 Profile；
                    // 同时固定远程登录模式（远程服务器参数由加密文件 client.yh 提供，M4 接线），并落盘
                    ApplyEditionLock();
#else
                    Save();
#endif
                }
            }
            catch
            {
                // 配置损坏时回退默认值，不阻塞登录器
                Settings = new AppSettings();
                ApplyBuildProfileDefaults();
#if EXTERNAL
                ApplyEditionLock();
#endif
                Save();
            }
        }

        /// <summary>
        /// 将 DNFLogin.build.json 中固定发布的版本隔离和远程连接参数写入当前版本。
        /// 用户 AppData 中的旧配置文件不能覆盖这些私有化构建参数。
        /// </summary>
        private static void ApplyBuildProfileDefaults()
        {
            EnsureProfiles();
            var profile = Settings.Profiles.Find(p =>
                string.Equals(p.Id, BuildParameters.ProfileId, StringComparison.OrdinalIgnoreCase))
                ?? CurrentProfile;
            Settings.CurrentProfileId = profile.Id;
            profile.LoginMode = BuildParameters.LoginMode;
            profile.RemoteIp = BuildParameters.RemoteIp;
            profile.RemotePort = BuildParameters.RemotePort;
            profile.CopyToTopLevel(Settings);
        }

#if EXTERNAL
        /// <summary>
        /// 外网版锁版本核心（M3）：锁定本版 Profile 为当前版本，并把登录模式固定为远程。
        /// 仅在 EXTERNAL 分支编译；Free 版完全不参与，行为零变化。
        /// </summary>
        private static void ApplyEditionLock()
        {
            EnsureProfiles();
            var profile = Settings.Profiles.Find(p => p.Id == EditionProfileId);
            if (profile is null)
            {
                profile = Settings.Profiles[0];
            }

            // 外网版仅允许远程登录：登录模式固定为 Remote（服务器参数最终来源为 client.yh，M4 接线）
            profile.LoginMode = "Remote";
            profile.CopyToTopLevel(Settings);
            Settings.CurrentProfileId = profile.Id;
            Save();
        }
#endif

        /// <summary>
        /// 写入 JSON（保存设置同步写并重读）
        /// </summary>
        public static void Save()
        {
            if (ConfigCleared)
            {
                return;
            }

            try
            {
                Directory.CreateDirectory(ConfigDirectory);
                var json = JsonSerializer.Serialize(Settings, new JsonSerializerOptions { WriteIndented = true });
                File.WriteAllText(ConfigPath, json);
            }
            catch
            {
                // 写入失败不阻塞登录
            }
        }

        /// <summary>
        /// 启动自检：JSON 缺少 CloseActionAsked 字段时补齐（默认0=询问），
        /// 确保老配置升级后该字段存在；已有字段则不动。
        /// </summary>
        public static void SelfCheck()
        {
            try
            {
                if (!File.Exists(ConfigPath))
                {
                    return; // 无配置文件：首次运行即使用默认值，无需补齐
                }

                var json = File.ReadAllText(ConfigPath);
                if (json.Contains("\"CloseActionAsked\"", StringComparison.Ordinal))
                {
                    return;
                }

                // 缺失：按默认值落盘补齐（Settings.CloseActionAsked 默认为 0）
                Save();
            }
            catch
            {
                // 自检失败不阻塞登录器
            }
        }

        /// <summary>保存后重读，确保内存与磁盘一致</summary>
        public static void Reload() => Load();
    }
}
