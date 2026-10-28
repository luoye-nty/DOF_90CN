using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Http;
using System.Security.Cryptography.X509Certificates;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>CDK 兑换可投放角色（GET /launcher/v1/characters 的 data 项）</summary>
    public sealed class CdkCharacter
    {
        [JsonPropertyName("character_id")] public string CharacterId { get; set; } = "";
        [JsonPropertyName("name")] public string Name { get; set; } = "";

        /// <summary>CDK 兑换角色下拉（DbComboStyle 收起态不回退 DisplayMemberPath，用 ToString 兜底显示角色名）。</summary>
        public override string ToString() => string.IsNullOrEmpty(Name) ? CharacterId : Name;
    }

    /// <summary>PVF 加密 key 下发（GET /launcher/v1/pvf/key 的 data 项）</summary>
    public sealed class PvfKeyInfo
    {
        [JsonPropertyName("pvf_version")] public string PvfVersion { get; set; } = "";
        [JsonPropertyName("file_sha256")] public string FileSha256 { get; set; } = "";
        [JsonPropertyName("key_set")] public string KeySet { get; set; } = ""; // 96 hex = 6 组 × 8B
    }

    /// <summary>PVF 最新发布信息（GET /launcher/v1/pvf/latest 的 data 项；不下发 key_set，仅作更新判断）</summary>
    public sealed class PvfLatestInfo
    {
        [JsonPropertyName("pvf_version")] public string PvfVersion { get; set; } = "";
        [JsonPropertyName("file_sha256")] public string FileSha256 { get; set; } = "";
        [JsonPropertyName("download_url")] public string DownloadUrl { get; set; } = "";
        [JsonPropertyName("client_download_url")] public string ClientDownloadUrl { get; set; } = ""; // 客户端整包直链（二期：本机无客户端时下载完整客户端）
    }

    /// <summary>远程服务器元信息（GET /launcher/v1/meta）</summary>
    public sealed class RemoteMeta
    {
        [JsonPropertyName("api_version")] public int ApiVersion { get; set; }
        [JsonPropertyName("registration_mode")] public string RegistrationMode { get; set; } = "";
        [JsonPropertyName("region_name")] public string RegionName { get; set; } = "";
        [JsonPropertyName("server_host")] public string ServerHost { get; set; } = "";
        [JsonPropertyName("channel_port")] public int ChannelPort { get; set; }
        [JsonPropertyName("initial_game_port")] public int InitialGamePort { get; set; }
        [JsonPropertyName("game_ports")] public List<int> GamePorts { get; set; } = new();
        [JsonPropertyName("tcls_crypto_material")] public string TclsCryptoMaterial { get; set; } = "";
        [JsonPropertyName("session_ttl_seconds")] public long SessionTtlSeconds { get; set; }
    }

    /// <summary>远程模式会话凭证（POST /launcher/v1/sessions）</summary>
    public sealed class RemoteSession
    {
        [JsonPropertyName("session_id")] public string SessionId { get; set; } = "";
        [JsonPropertyName("game_secret")] public string GameSecret { get; set; } = "";
        [JsonPropertyName("expires_at")] public DateTime ExpiresAt { get; set; }
        [JsonPropertyName("account_id")] public string AccountId { get; set; } = "";
        [JsonPropertyName("region_name")] public string RegionName { get; set; } = "";
        [JsonPropertyName("server_host")] public string ServerHost { get; set; } = "";
        [JsonPropertyName("channel_port")] public int ChannelPort { get; set; }
        [JsonPropertyName("initial_game_port")] public int InitialGamePort { get; set; }
        [JsonPropertyName("game_ports")] public List<int> GamePorts { get; set; } = new();
        [JsonPropertyName("tcls_crypto_material")] public string TclsCryptoMaterial { get; set; } = "";
    }

    /// <summary>
    /// 远程登录服务：封装旧版 Go remotelauncher 的 HTTPS API（meta / sessions / accounts）。
    /// 自签证书通过 RemoteCAFile 指定的 PEM 做自定义根信任，UserAgent 对齐 "90CN/1"。
    /// </summary>
    public static class RemoteAuthService
    {
        private const string UserAgent = "90CN/1";

        /// <summary>
        /// 当前登录器构建的客户端版本身份（与服务端 ServerEdition 对齐）：
        ///   free  = 免费版登录器（本地/免费远程），仅可连接免费版运维工具；
        ///   paid  = 外网版登录器（EXTERNAL），仅可连接赞助版运维工具。
        /// 90 运维工具 API（/launcher/v1/* 直连 18112）请求必须携带 X-Client-Edition 头，
        /// 服务端据此强制版本互斥，防止免费版登录器蹭赞助版运维工具（反之亦然）。
        /// </summary>
#if EXTERNAL
        public const string ClientEdition = "paid";
#else
        public const string ClientEdition = "free";
#endif

        /// <summary>最近一次 90 运维工具 /launcher/v1/health 返回的服务端版本身份（""=未知）。</summary>
        public static string LastServerEdition { get; private set; } = "";

        /// <summary>
        /// 判断当前登录器客户端版本身份与服务端版本身份是否匹配（版本互斥）。
        /// 老版运维工具未返回 edition（""）时按 free 处理（旧版运维工具属免费版逻辑）。
        /// </summary>
        public static string EditionMismatchReason(string serverEdition)
        {
            var server = string.IsNullOrWhiteSpace(serverEdition) ? "free" : serverEdition;
            if (string.Equals(server, ClientEdition, StringComparison.OrdinalIgnoreCase))
            {
                return "";
            }
            return server == "paid"
                ? "该服务器为赞助版运维工具，仅限外网版登录器连接；当前登录器为免费版，禁止连接"
                : "该服务器为免费版运维工具，仅限免费版登录器连接；当前登录器为外网版，禁止连接";
        }

        /// <summary>版本头名称：客户端发给 90 运维工具的 X-Client-Edition。</summary>
        private const string EditionHeader = "X-Client-Edition";

        /// <summary>版本头名称：客户端发给 90 运维工具的 X-Client-Launcher-Version。</summary>
        private const string LauncherVersionHeader = "X-Client-Launcher-Version";

        /// <summary>最近一次自检获取到的泡点开关状态；null 表示未知（自检未执行或失败）。</summary>
        public static bool? PointsinkEnabled { get; private set; }

        /// <summary>
        /// 最近一次自检获取到的「注册是否需要注册码」；null 表示未知。
        /// 注册窗据此决定要不要显示注册码输入框（服务端开了就必须填，否则注册会被拒）。
        /// </summary>
        public static bool? RegistrationCodeRequired { get; private set; }

        /// <summary>
        /// 最近一次得知的维护状态：true = 服务器正在更新维护（玩家不能登录、不能进游戏）。
        /// 由自检（自检阶段拉一次）与维护长轮询共同更新。
        /// </summary>
        public static bool MaintenanceEnabled { get; private set; }

        /// <summary>
        /// ★ 双开（多开）开关 —— **三态语义，只有明确收到 enabled:false 才关闭** ★
        ///
        /// 由自检（启动时拉一次 <see cref="GetMultiOpenAsync"/>）与双开长轮询
        /// （<see cref="WaitMultiOpenAsync"/>，后台一改就推送）共同更新。
        ///
        /// 取值含义（这是本功能最容易出事的地方，改动前务必读完）：
        ///   null  = **未知** = 保持"允许双开"（= 改造前的现有行为）；
        ///   true  = 服务器明确允许双开；
        ///   false = 服务器**明确关闭**了双开 —— 只有这一种情况登录器才关闸。
        ///
        /// 什么时候会是 null（全部都必须保持允许双开）：
        ///   ① 自检还没跑完 / 请求失败 / 网络异常 / 超时；
        ///   ② **老运维工具**（没有 /launcher/v1/multi-open 这个端点）→ 404；
        ///   ③ 服务器返回的 data 里**没有** enabled 字段（后台从未设置过这个开关，
        ///      服务端用"字段缺失"表达"没设置过"，与"明确关闭"是两回事）；
        ///   ④ 免费版 / 单机模式（自检里根本不拉这个接口）。
        ///
        /// ⚠ 绝不能用 `bool` 接这个字段：那样"拉不到"会被当成 false = 关闭，
        ///   于是"老运维工具 + 新登录器"会让全服双开瞬间消失。见 GetMultiOpenAsync。
        /// </summary>
        public static bool? MultiOpenEnabled { get; private set; }

        /// <summary>双开是否被允许：未知（null）一律按"允许"处理（降级语义，见 MultiOpenEnabled）。</summary>
        public static bool MultiOpenAllowed => MultiOpenEnabled != false;

        /// <summary>当前填写的账号是否在维护白名单里（true 时维护期间该账号照常能登）。</summary>
        public static bool MaintenanceAllowed { get; private set; }

        /// <summary>维护状态版本号：长轮询用它与服务端比对，判断期间是否发生过变化。</summary>
        public static long MaintenanceVersion { get; private set; }

        /// <summary>最近一次拉取到的登录器版本校验结果；null = 还没成功拉到过。</summary>
        public static LauncherVersionInfo? ServerLauncherVersion { get; private set; }

        /// <summary>
        /// 与 launcher/main.go 的 DefaultLauncherKey 保持一致的固定接口密钥。
        /// 所有 /launcher/v1/* 请求都必须携带 X-Launcher-Key 请求头，否则 launcher 返回 401。
        /// </summary>
        private const string LauncherKey = "L#v9pX!qR2$tK7@mF4^nC5&zB8*eG1";

        private const string LauncherKeyHeader = "X-Launcher-Key";
        private static readonly JsonSerializerOptions JsonOpts = new()
        {
            PropertyNameCaseInsensitive = true
        };

        /// <summary>自动获取的服务器证书缓存目录（AppData\DNFLogin\certs）</summary>
        private static readonly string CertCacheDir =
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin", "certs");

        /// <summary>
        /// 根据服务器 URL 生成自动 CA 缓存文件名（按主机+端口哈希，避免多服务器冲突）。
        /// </summary>
        private static string GetAutoCaPath(string baseUrl)
        {
            var key = baseUrl.Trim().TrimEnd('/');
            var hash = Math.Abs(key.GetHashCode()).ToString("x8");
            Directory.CreateDirectory(CertCacheDir);
            return Path.Combine(CertCacheDir, "auto-" + hash + ".cer");
        }

        /// <summary>
        /// 未配置远程服务器时返回固定令牌回退会话（与本地同一套 token）。
        /// 等用户提供远程服务器 IP/密码后走真实 API，此回退即失效。
        /// </summary>
        public static RemoteSession CreateFallbackSession(AppSettings settings)
        {
            // 远程模式下优先用 RemoteIp（公网转发器地址）；未配置时回落本地 Host。
            var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                ? settings.RemoteIp.Trim()
                : settings.Host;
            var channelPort = 7001;
            if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
            {
                channelPort = rp;
            }

            return new RemoteSession
            {
                SessionId = new string('0', 32),
                GameSecret = new string('0', 64),
                ServerHost = host,
                ChannelPort = channelPort,
                InitialGamePort = 0,
                GamePorts = new List<int> { 0 },
                TclsCryptoMaterial = GameTokens.GameOuterToken
            };
        }

        /// <summary>GET /launcher/v1/meta：获取远程服务器元信息（在线检测 / 测试连接用）</summary>
        public static async Task<(bool Ok, string Error, RemoteMeta? Meta)> GetMetaAsync(AppSettings settings)
        {
            try
            {
                var baseUrl = NormalizeBaseUrl(settings.RemoteServerUrl);
                using var client = CreateClient(settings);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/meta").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "连接服务器失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                var meta = JsonSerializer.Deserialize<RemoteMeta>(json, JsonOpts);
                return meta is null
                    ? (false, "响应格式异常", null)
                    : (true, "", meta);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 远程登录：POST /launcher/v1/sessions 创建会话。
        /// 返回会话级 TCLSCryptoMaterial（启动参数第 5 段令牌）、
        /// session_id / game_secret / server_host / game_ports（注入 DNF_REMOTE_AUTH_* 环境变量）。
        /// </summary>
        public static async Task<(bool Ok, string Error, RemoteSession? Session)> LoginAsync(
            AppSettings settings, string account, string password)
        {
            try
            {
                var baseUrl = NormalizeBaseUrl(settings.RemoteServerUrl);
                using var client = CreateClient(settings);

                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["password"] = password
                };

                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/sessions", content).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                    return (false, FriendlyApiError((int)response.StatusCode, body, "登录失败，请检查账号密码后重试"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                var session = JsonSerializer.Deserialize<RemoteSession>(json, JsonOpts);
                if (session is null || string.IsNullOrWhiteSpace(session.SessionId))
                {
                    return (false, "服务器未返回会话凭证", null);
                }

                return (true, "", session);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 90 远程登录：POST /launcher/v1/login（运维工具认证 API）。
        /// 请求 {username, password, machine_id}，响应 {account_id, ticket, expires_at, has_rescue_code}。
        /// ticket 用于 LANA 握手（dnfbridge 回调 19091 验票）。
        /// HasRescueCode=false 表示老号尚未设置救援代码（登录器据此引导补设，不阻断登录）。
        /// </summary>
        public static async Task<(bool Ok, string Error, string Ticket, string AccountId, bool HasRescueCode)> LoginForTicketAsync(
            AppSettings settings, string account, string password)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";

                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["password"] = password,
                    // 机器码：真实硬件指纹（hw:<32hex>，见 Services/HardwareId.cs）。
                    // 2026-09-24 之前是 Environment.MachineName（计算机名），改个电脑名就能绕过机器封禁。
                    ["machine_id"] = HardwareId.MachineCode,
                    // 兼容旧封禁：服务端按"新硬件码优先、旧计算机名兼容"双匹配，
                    // 历史上按计算机名写的 launcher_bans(machine) 行不会失效。
                    ["computer_name"] = HardwareId.ComputerName
                };
#if EXTERNAL
                // 【已去授权】此处原先上报 target_ip + 向验证服务器申请一次性 nonce，
                // 再把 yh_ticket/yh_nonce 带给服主由其回调 authserver 核销。
                // 现在登录包体里不再携带任何授权票据与目标 IP，只保留账号/密码/机器码。
                // 服务端侧同样已放行（internal/server yhCheckDeny 直接返回放行），
                // 所以旧版登录器即使仍带票也能正常登录。
#endif
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/login", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "登录失败，请检查账号密码后重试"), "", "", false);
                }

                using var doc = JsonDocument.Parse(body);
                var ticket = doc.RootElement.TryGetProperty("ticket", out var tp) ? tp.GetString() ?? "" : "";
                var accountId = doc.RootElement.TryGetProperty("account_id", out var ap) ? ap.GetString() ?? "" : "";
                // 老号无 rescue_code_hash 时服务端返回 false；新客户端可能连旧服务端（无此字段），
                // 按有码处理（不触发补设引导），避免误弹。
                var hasRescueCode = !doc.RootElement.TryGetProperty("has_rescue_code", out var hp)
                    || hp.ValueKind != JsonValueKind.False;
                if (string.IsNullOrWhiteSpace(ticket))
                {
                    return (false, "服务器未返回登录凭证", "", "", false);
                }
                return (true, "", ticket, accountId, hasRescueCode);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), "", "", false);
            }
        }

        /// <summary>
        /// 90 远程在线人数：GET /launcher/v1/health（运维工具认证 API）。
        /// 返回 {online_count, max_online}；max_online=0 表示不限制在线人数。
        /// </summary>
        public static async Task<(bool Ok, string Error, int OnlineCount, int MaxOnline)> GetHealthAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/health").ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "查询服务器状态失败"), 0, 0);
                }

                using var doc = JsonDocument.Parse(body);
                var server = doc.RootElement.TryGetProperty("server", out var sp)
                    ? sp
                    : default;
                int online = 0;
                int max = 0;
                if (server.ValueKind == JsonValueKind.Object)
                {
                    if (server.TryGetProperty("online_count", out var op) && op.ValueKind == JsonValueKind.Number)
                    {
                        online = op.GetInt32();
                    }
                    if (server.TryGetProperty("max_online", out var mp) && mp.ValueKind == JsonValueKind.Number)
                    {
                        max = mp.GetInt32();
                    }
                    if (server.TryGetProperty("edition", out var ep) && ep.ValueKind == JsonValueKind.String)
                    {
                        LastServerEdition = ep.GetString() ?? "";
                    }
                    else
                    {
                        LastServerEdition = ""; // 老版运维工具未返回 edition
                    }
                }
                return (true, "", online, max);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), 0, 0);
            }
        }

        /// <summary>GM 踢人事件（GET /launcher/v1/gm-events 返回项）</summary>
        public sealed class GMEventItem
        {
            [JsonPropertyName("id")] public long Id { get; set; }
            [JsonPropertyName("account_id")] public string AccountId { get; set; } = "";
            [JsonPropertyName("event_type")] public string EventType { get; set; } = "";
            [JsonPropertyName("reason")] public string Reason { get; set; } = "";
            [JsonPropertyName("created_at")] public string CreatedAt { get; set; } = "";
        }

        /// <summary>全服公告项（GET /launcher/v1/announcements/wait 返回）。</summary>
        public sealed class AnnouncementItem
        {
            [JsonPropertyName("id")] public long Id { get; set; }
            [JsonPropertyName("content")] public string Content { get; set; } = "";
            [JsonPropertyName("color")] public string Color { get; set; } = "0xFF0000FF";
        }

        /// <summary>泡点公开信息（GET /launcher/v1/pointsink 的 data 项）</summary>
        public sealed class PointsinkInfo
        {
            [JsonPropertyName("enabled")] public bool Enabled { get; set; }
            [JsonPropertyName("interval_seconds")] public int IntervalSeconds { get; set; }
            [JsonPropertyName("points")] public int Points { get; set; }
        }

        /// <summary>注册信息（GET /launcher/v1/registration-info 的 data 项）。</summary>
        public sealed class RegistrationInfo
        {
            /// <summary>注册是否需要注册码：true 时注册界面显示注册码输入框且必填。</summary>
            [JsonPropertyName("code_required")] public bool CodeRequired { get; set; }
            /// <summary>是否已关闭玩家自助注册（true 时注册必然被拒，可用于提前提示）。</summary>
            [JsonPropertyName("closed")] public bool Closed { get; set; }
        }

        /// <summary>维护信息（GET /launcher/v1/maintenance 的 data 项）。</summary>
        public sealed class MaintenanceInfo
        {
            /// <summary>是否正在更新维护：true = 玩家不能登录、不能进游戏。</summary>
            [JsonPropertyName("enabled")] public bool Enabled { get; set; }
            /// <summary>维护状态版本号（长轮询比对用）。</summary>
            [JsonPropertyName("version")] public long Version { get; set; }
            /// <summary>请求里带的 username 是否在维护白名单里（白名单账号维护期间照常能登）。</summary>
            [JsonPropertyName("allowed")] public bool Allowed { get; set; }
        }

        /// <summary>
        /// 双开（多开）开关信息（GET /launcher/v1/multi-open 的 data 项）。
        ///
        /// ★ Enabled 必须是 `bool?` 而不是 `bool` ★ —— 服务端在"后台从未设置过这个开关"
        /// 时**不下发** enabled 字段，此时反序列化结果是 null（而不是 false），
        /// 登录器据此保持"允许双开"（= 改造前的现有行为）。
        /// 用 `bool` 的话缺字段会落到 default(bool)=false，把"没设置过"误判成"明确关闭"，
        /// 于是升级运维工具那一刻全服玩家的双开会一起消失。
        /// </summary>
        public sealed class MultiOpenInfo
        {
            /// <summary>
            /// 是否允许双开。null = 服务器没有明确表态（字段缺失）→ 按"允许"处理。
            /// false = **明确关闭**（只有这一种情况才关闸）。
            /// </summary>
            [JsonPropertyName("enabled")] public bool? Enabled { get; set; }
        }

        /// <summary>登录器版本校验信息（GET /launcher/v1/version-check 的 data 项）。</summary>
        public sealed class LauncherVersionInfo
        {
            /// <summary>是否开启版本校验：false 时登录器**不再看后面任何字段**，直接放行。</summary>
            [JsonPropertyName("enabled")] public bool Enabled { get; set; }
            /// <summary>服务端内置要求的登录器版本号（与 LauncherVersion.Current 做完全一致比对）。</summary>
            [JsonPropertyName("required_version")] public string RequiredVersion { get; set; } = "";
            /// <summary>版本不符时的下载直链；空串 = 管理员没填直链（弹「请联系管理员」）。</summary>
            [JsonPropertyName("download_url")] public string DownloadUrl { get; set; } = "";
            /// <summary>服务端算好的"能不能真的更新"（= enabled &amp;&amp; 直链非空）。</summary>
            [JsonPropertyName("configured")] public bool Configured { get; set; }

            /// <summary>本机登录器版本是否满足服务端要求（未开启校验时恒为 true）。</summary>
            public bool VersionMatches =>
                !Enabled ||
                string.Equals(RequiredVersion?.Trim(), LauncherVersion.Current, StringComparison.Ordinal);
        }

        /// <summary>服务器公告栏的一条（GET /launcher/v1/notices 的 data.notices 项）。</summary>
        public sealed class NoticeItem
        {
            [JsonPropertyName("id")] public long Id { get; set; }
            [JsonPropertyName("title")] public string Title { get; set; } = "";
            /// <summary>日期 YYYY-MM-DD；空串表示这条公告不显示日期。</summary>
            [JsonPropertyName("notice_date")] public string NoticeDate { get; set; } = "";
        }

        /// <summary>公告栏响应体（data 项）。</summary>
        public sealed class NoticesInfo
        {
            [JsonPropertyName("notices")] public List<NoticeItem> Notices { get; set; } = new();
        }

        /// <summary>服务端指定 DLL 的一条（GET /launcher/v1/dlls 的 data.dlls 项）。</summary>
        public sealed class DllRequirementItem
        {
            [JsonPropertyName("file_name")] public string FileName { get; set; } = "";
            [JsonPropertyName("sha256")] public string Sha256 { get; set; } = "";
            /// <summary>block=校验不过拒绝启动游戏；skip=跳过注入。</summary>
            [JsonPropertyName("on_mismatch")] public string OnMismatch { get; set; } = "skip";
        }

        /// <summary>服务端指定 DLL 清单响应体（data 项）。</summary>
        public sealed class DllRequirementsInfo
        {
            [JsonPropertyName("dlls")] public List<DllRequirementItem> Dlls { get; set; } = new();
        }

        /// <summary>
        /// 登录器当前持有的"服务端指定 DLL"清单（自检/登录时拉取后覆盖）。
        /// 拉取失败时**保留上一次的清单**：网络抖一下不该让"必须存在的 dll"变成不校验。
        /// </summary>
        public static IReadOnlyList<DllRequirementItem> ServerDllRequirements { get; private set; } = Array.Empty<DllRequirementItem>();

        /// <summary>
        /// 拉取"服务端指定 DLL"清单：GET /launcher/v1/dlls（免 ticket）。
        /// 登录器在启动自检与玩家点登录时各拉一次，随后按客户端根目录校验 SHA256。
        /// </summary>
        public static async Task<(bool Ok, string Error, DllRequirementsInfo? Info)> GetDllRequirementsAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/dlls").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取可加载 DLL 清单失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                DllRequirementsInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<DllRequirementsInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    // 丢掉明显残缺的条目（没有文件名或没有哈希的条目无法校验，留着只会误报"缺文件"）。
                    var clean = new List<DllRequirementItem>(info.Dlls.Count);
                    foreach (var item in info.Dlls)
                    {
                        if (item is not null && !string.IsNullOrWhiteSpace(item.FileName) &&
                            !string.IsNullOrWhiteSpace(item.Sha256))
                        {
                            clean.Add(item);
                        }
                    }
                    info.Dlls = clean;
                    ServerDllRequirements = clean;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 登录器当前持有的公告栏内容（登录页左侧「服务器公告」面板直接读它）。
        /// 说明：故意做成"拉取成功才覆盖"——拉取失败时保留上一次的内容，
        /// 网络抖一下不该让玩家眼前的公告板整块消失。
        /// </summary>
        public static IReadOnlyList<NoticeItem> ServerNotices { get; private set; } = Array.Empty<NoticeItem>();

        /// <summary>公告栏是否成功拉取过（区分"服务器上没公告"与"还没拉到"）。</summary>
        public static bool ServerNoticesLoaded { get; private set; }

        /// <summary>
        /// 服务器公告栏获取：GET /launcher/v1/notices（免 ticket，登录前就能看到公告）。
        /// 失败静默返回 false，调用方保留上一次内容（不阻断任何登录流程）。
        /// </summary>
        public static async Task<(bool Ok, string Error, NoticesInfo? Info)> GetNoticesAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/notices").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取服务器公告失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                NoticesInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<NoticesInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    // 只保留有标题的条目：空标题在界面上就是一行空白（服务端已拦，这里再兜一层）。
                    var clean = new List<NoticeItem>(info.Notices.Count);
                    foreach (var n in info.Notices)
                    {
                        if (n is not null && !string.IsNullOrWhiteSpace(n.Title))
                        {
                            clean.Add(n);
                        }
                    }
                    info.Notices = clean;
                    ServerNotices = clean;
                    ServerNoticesLoaded = true;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 90 GM 事件轮询：GET /launcher/v1/gm-events?ticket=...&amp;after_id=...
        /// 登录器凭 ticket 查询自己账号是否有新的 GM 事件（封禁踢人），命中后由调用方关闭对应游戏进程。
        /// </summary>
        /// <summary>
        /// 90 GM 事件查询：GET /launcher/v1/gm-events?ticket=...&amp;after_id=...&amp;since=...
        ///
        /// since（RFC3339，可空）= 只要这个时刻之后产生的事件。登录器每次会话都带上
        /// "本次启动时刻"，于是**不依赖跨会话的 id 游标**：游标被毒化（比如服务端数据库
        /// 重建后 id 从 1 重新开始，而登录器里还存着几万的旧游标）也照样能捞到新事件。
        /// 详见运维工具 store.ListGMKickEventsAfter 的注释。
        /// </summary>
        public static async Task<(bool Ok, string Error, List<GMEventItem> Events)> GetGMEventsAsync(
            AppSettings settings, string host, int port, string ticket, long afterId, string since = "")
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var url = baseUrl + "/launcher/v1/gm-events?ticket=" +
                          Uri.EscapeDataString(ticket) +
                          "&after_id=" + afterId.ToString(System.Globalization.CultureInfo.InvariantCulture) +
                          SinceParam(since);
                var response = await client.GetAsync(url).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "查询踢人事件失败"), new List<GMEventItem>());
                }

                using var doc = JsonDocument.Parse(body);
                var list = new List<GMEventItem>();
                if (doc.RootElement.TryGetProperty("events", out var eventsProp) &&
                    eventsProp.ValueKind == JsonValueKind.Array)
                {
                    foreach (var item in eventsProp.EnumerateArray())
                    {
                        var ev = JsonSerializer.Deserialize<GMEventItem>(item.GetRawText(), JsonOpts);
                        if (ev is not null)
                        {
                            list.Add(ev);
                        }
                    }
                }
                return (true, "", list);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), new List<GMEventItem>());
            }
        }

        /// <summary>
        /// 90 泡点信息获取：GET /launcher/v1/pointsink（无需 ticket）。
        /// 启动自检阶段调用，成功后同步更新 PointsinkEnabled 静态状态。
        /// </summary>
        public static async Task<(bool Ok, string Error, PointsinkInfo? Info)> GetPointsinkAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/pointsink").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取泡点信息失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                PointsinkInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<PointsinkInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    PointsinkEnabled = info.Enabled;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 注册信息获取：GET /launcher/v1/registration-info（无需 ticket）。
        /// 启动自检阶段调用，成功后同步更新 RegistrationCodeRequired 静态状态；
        /// 注册窗打开时按该状态决定是否显示「注册码」输入框。
        /// 拉取失败时保持 null（= 按"不需要"处理，行为与改动前完全一致，不阻断任何流程）。
        /// </summary>
        public static async Task<(bool Ok, string Error, RegistrationInfo? Info)> GetRegistrationInfoAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/registration-info").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取注册信息失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                RegistrationInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<RegistrationInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    RegistrationCodeRequired = info.CodeRequired;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// ★ 双开（多开）开关获取：GET /launcher/v1/multi-open（无需 ticket）。
        /// 启动自检阶段调用一次，成功后同步更新静态状态 MultiOpenEnabled。
        ///
        /// ── 三态语义：**只有明确收到 enabled:false 才关闭** ────────────────────
        /// 返回的 Info.Enabled 是 `bool?`：
        ///   · 服务器返回 {enabled:true/false} → 明确表态，照办；
        ///   · 服务器返回 {}（后台从未设置过该开关）→ Enabled = null → **保持允许双开**；
        ///   · 请求失败 / 404（老运维工具没有这个端点）/ 网络异常 → 返回 Info=null，
        ///     调用方**不得**改动 MultiOpenEnabled，于是继续是 null = 允许。
        ///
        /// 换句话说：本方法**永远不会**把状态改成 false，除非服务端明确这么说了。
        /// 这条例外是刻意的 —— 拉不到就把全服双关掉（"老运维工具 + 新登录器"那种组合）
        /// 是本功能最严重的故障模式，比"该关没关"严重得多。
        ///
        /// 免费版同样适用：双开 UI 不在任何版本条件编译里（免费版可以自由切到 "90" profile），
        /// 所以本方法也不能放进 `#if EXTERNAL`。
        /// </summary>
        public static async Task<(bool Ok, string Error, MultiOpenInfo? Info)> GetMultiOpenAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/multi-open").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    // 老运维工具会走到这里（404）：**不改动** MultiOpenEnabled，保持原样（通常为 null = 允许）。
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取双开开关失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                MultiOpenInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<MultiOpenInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                // ★ 只有拿到**明确的** true/false 才更新静态状态；null（字段缺失）保持不动。
                if (info?.Enabled is bool explicitValue)
                {
                    MultiOpenEnabled = explicitValue;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// ★ 双开（多开）开关**实时推送**长轮询：GET /launcher/v1/multi-open/wait?known=
        ///
        /// 与 <see cref="GetMultiOpenAsync"/> 语义**完全一致**（同一个三态、同一个 data.enabled），
        /// 区别只在"服务端在没有变化时挂住等"（最多 25 秒）。挂上它之后，运营在后台一点开关，
        /// 已经开着的登录器**立刻**变灰/恢复 —— 这就是"不用让玩家重开登录器"的实现方式。
        ///
        /// 免 ticket：与短端点同源 —— 它服务的正是**还没登录的登录界面**，
        /// 玩家手里根本没有票（同 <see cref="WaitMaintenanceAsync"/> 的理由）。
        ///
        /// ── known 参数（客户端当前认知）────────────────────────────────────────
        /// 带上 MultiOpenEnabled 的当前值，服务端就能在"它的状态与我们的认知不一致"时
        /// **立刻回话**，而不是让我们挂满 25 秒。补的正是这段窗口：启动自检拉完短端点、
        /// 到长轮询真正挂上之间（网络往返 + 窗口初始化），运营刚好在这期间点了开关的话，
        /// 广播是收不到的（那会儿还没订阅）。
        /// null（从未得知过）时**不带**该参数：服务端会按"不短路"处理，纯挂起等广播。
        ///
        /// ── 三态与降级（与短端点一字不差）────────────────────────────────────
        ///   · 明确 {enabled:true/false} → 更新静态 MultiOpenEnabled；
        ///   · 明确 {}（后台从未设置过）→ 把静态状态**置回 null**（未知 = 允许）。
        ///     注意这与"拉取失败"是两回事：这是服务端的**明确表态**，不是拿不到。
        ///     置回 null 也是**防忙轮询**的关键：否则 known 会一直等于上次的明确值，
        ///     而服务端每次都回"我没有明确值"，于是每轮都立刻返回、打成死循环；
        ///   · 失败 / 超时 / 404 → **不动**静态状态（保持原值，通常为 null = 允许）。
        ///
        /// EndpointMissing=true 表示服务端根本没有这个端点（**老运维工具**，HTTP 404）——
        /// 调用方必须**停掉长轮询**并退回"启动自检 + 副登录前复查"，
        /// 绝不能继续重挂：404 是秒回的，不停就成了每秒几百次的死循环
        ///（与 <see cref="WaitGMEventsAsync"/> 的 EndpointMissing 同一套路）。
        ///
        /// 取消语义（与 <see cref="WaitMaintenanceAsync"/> 一致，别照抄 WaitGMEventsAsync）：
        ///   · **我们自己的** token 被取消（关窗/退出）→ 抛 OperationCanceledException，
        ///     调用方按正常退出处理；
        ///   · HttpClient 自身超时（40 秒还没回）→ 返回 Ok=false，让调用方退避后重挂。
        ///     绝不能也往上抛：调用方把取消当"正常退出"，一次超时就会把实时推送**永久打死**
        ///     （表现是"后台关了，登录器再也不变灰"，而且日志里什么都看不到）。
        /// </summary>
        public static async Task<(bool Ok, string Error, MultiOpenInfo? Info, bool EndpointMissing)> WaitMultiOpenAsync(
            AppSettings settings, string host, int port, CancellationToken token)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                // 超时必须大于服务端挂起上限（25 秒），否则每个长轮询都会以"超时异常"结束。
                using var client = CreateClient(settings, baseUrl, TimeSpan.FromSeconds(40));
                var url = baseUrl + "/launcher/v1/multi-open/wait";
                // known 只在**明确知道**服务端表过态时才带（null = 一无所知 = 不带）。
                if (MultiOpenEnabled is bool known)
                {
                    url += "?known=" + (known ? "true" : "false");
                }
                var response = await client.GetAsync(url, token).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    var missing = response.StatusCode == System.Net.HttpStatusCode.NotFound;
                    return (false, FriendlyApiError((int)response.StatusCode, null, "等待双开开关失败"),
                        null, missing);
                }

                var json = await response.Content.ReadAsStringAsync(token).ConfigureAwait(false);
                using var doc = JsonDocument.Parse(json);
                if (!doc.RootElement.TryGetProperty("data", out var dataProp) ||
                    dataProp.ValueKind != JsonValueKind.Object)
                {
                    // 200 但结构不认识（中间件改包/老版本）：按"拿不到"处理，**不动**静态状态。
                    return (false, "服务器返回的双开开关信息无法解析", null, false);
                }
                var info = JsonSerializer.Deserialize<MultiOpenInfo>(dataProp.GetRawText(), JsonOpts);
                if (info is null)
                {
                    return (false, "服务器返回的双开开关信息无法解析", null, false);
                }

                // ★ 只有拿到**明确的** true/false 才更新静态状态。
                if (info.Enabled is bool explicitValue)
                {
                    MultiOpenEnabled = explicitValue;
                }
                else
                {
                    // 服务端明确回了空 data（后台从未设置过）→ 认知回到"未知 = 允许"。
                    // 这一支不是失败，是正常表态（见上面的 known 说明）。
                    MultiOpenEnabled = null;
                }
                return (true, "", info, false);
            }
            catch (OperationCanceledException) when (token.IsCancellationRequested)
            {
                throw; // 窗口关闭/退出：交给调用方按正常退出处理
            }
            catch (OperationCanceledException)
            {
                // **不是我们取消的** → HttpClient 自身超时（40 秒都没回；理论上不该发生：
                // 服务端最多挂 25 秒）。按"失败"返回，让调用方退避后重挂 ——
                // 这里绝不能往上抛：调用方把取消当成"正常退出"，一次网络超时就会把
                // 实时推送**永久打死**（表现是"后台关了，登录器再也不变灰"，且毫无报错）。
                return (false, "等待双开开关超时（服务端未在预期时间内返回）", null, false);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null, false);
            }
        }

        /// <summary>
        /// 更新维护状态（短请求，免 ticket）：GET /launcher/v1/maintenance?username=
        /// 启动自检阶段拉一次；用户改账号名时也重拉一次（白名单判断依赖账号名）。
        /// 成功后同步更新静态状态（MaintenanceEnabled / MaintenanceAllowed / MaintenanceVersion）。
        /// </summary>
        public static async Task<(bool Ok, string Error, MaintenanceInfo? Info)> GetMaintenanceInfoAsync(
            AppSettings settings, string host, int port, string? username = null)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var url = baseUrl + "/launcher/v1/maintenance?username=" +
                    Uri.EscapeDataString((username ?? string.Empty).Trim());
                var response = await client.GetAsync(url).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取维护状态失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                MaintenanceInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<MaintenanceInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    MaintenanceEnabled = info.Enabled;
                    MaintenanceAllowed = info.Allowed;
                    MaintenanceVersion = info.Version;
                }
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 登录器版本校验（短请求，免 ticket）：GET /launcher/v1/version-check
        ///
        /// 登录器在**启动自检阶段**与**玩家点「登录」时**各调一次：
        ///   · Ok=true 且 Info.Enabled=false  → 管理员没开校验，任意版本放行；
        ///   · Ok=true 且 Info.VersionMatches  → 版本一致，放行；
        ///   · Ok=true 且 !VersionMatches      → 需要更新（Info.Configured=false 表示
        ///     服务端没填直链，调用方应弹「请联系管理员」而不是去下载）；
        ///   · Ok=false                        → 拿不到版本信息（运维工具没开 / 网络不通 /
        ///     版本互斥被拒）。**按需求这是"禁止进入"**，不像维护状态那样静默放行 ——
        ///     因为"开着校验但连不上"时如果放行，等于给了一条绕过校验的路。
        ///
        /// 成功后同步更新静态状态 ServerLauncherVersion，供登录窗等位置读取。
        /// </summary>
        public static async Task<(bool Ok, string Error, LauncherVersionInfo? Info)> GetLauncherVersionAsync(
            AppSettings settings, string host, int port)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/version-check").ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "获取登录器版本信息失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                LauncherVersionInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<LauncherVersionInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info is null)
                {
                    // 响应 200 但结构不认识（老版运维工具/中间件改包）：按"拿不到"处理，
                    // 绝不能当成"未开启校验"放行。
                    return (false, "服务器返回的版本校验信息无法解析，请联系管理员", null);
                }
                info.RequiredVersion = (info.RequiredVersion ?? "").Trim();
                info.DownloadUrl = (info.DownloadUrl ?? "").Trim();
                ServerLauncherVersion = info;
                return (true, "", info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// 更新维护长轮询（免 ticket）：GET /launcher/v1/maintenance/wait?version=&amp;username=
        ///
        /// 挂起等待服务端广播"开始维护/结束维护"（服务端最多挂 25 秒，超时返回当前状态）。
        /// 这是"登录器已经打开、之后才勾选维护"能实时生效的唯一途径 —— 登录界面还没登录，
        /// 拿不到 ticket，所以这个接口必须免鉴权。
        /// 返回的 Info 永远是最新状态；Ok=false 表示网络/服务端异常，调用方应退避后重挂。
        /// </summary>
        public static async Task<(bool Ok, string Error, MaintenanceInfo? Info)> WaitMaintenanceAsync(
            AppSettings settings, string host, int port, long version, string? username, CancellationToken token)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                // 超时必须大于服务端挂起上限（25 秒），否则每个长轮询都会以"超时异常"结束。
                using var client = CreateClient(settings, baseUrl, TimeSpan.FromSeconds(40));
                var url = $"{baseUrl}/launcher/v1/maintenance/wait?version={version}&username=" +
                    Uri.EscapeDataString((username ?? string.Empty).Trim());
                var response = await client.GetAsync(url, token).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, null, "等待维护状态失败"), null);
                }

                var json = await response.Content.ReadAsStringAsync(token).ConfigureAwait(false);
                MaintenanceInfo? info = null;
                using (var doc = JsonDocument.Parse(json))
                {
                    if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                        dataProp.ValueKind == JsonValueKind.Object)
                    {
                        info = JsonSerializer.Deserialize<MaintenanceInfo>(dataProp.GetRawText(), JsonOpts);
                    }
                }

                if (info != null)
                {
                    MaintenanceEnabled = info.Enabled;
                    MaintenanceAllowed = info.Allowed;
                    MaintenanceVersion = info.Version;
                }
                return (true, "", info);
            }
            catch (OperationCanceledException)
            {
                // 窗口关闭/主动取消：交给调用方判断，不算错误。
                return (false, "cancelled", null);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        /// <summary>
        /// GM 事件长轮询（实时推送）：GET /launcher/v1/gm-events/wait?ticket=&amp;after_id=
        ///
        /// 与 <see cref="GetGMEventsAsync"/> 语义完全一致（同账号、同游标），区别只在
        /// "没有事件时服务端挂住等"（最多 25 秒）。这样到账绿字不必等下一个 10 秒兜底轮询，
        /// 基本是运维工具一落库就弹。
        ///
        /// 返回 Ok=true 且 Events 为空 = 挂满 25 秒也没事件，**不是错误**，调用方直接重挂。
        /// EndpointMissing=true 表示服务端根本没有这个端点（老版运维工具，HTTP 404）——
        /// 调用方必须**停掉长轮询**并退回 10 秒兜底，绝不能继续重挂：404 是秒回的，
        /// 不停就成了每秒几百次的死循环。
        /// token 取消时抛 OperationCanceledException（调用方按正常退出处理）。
        /// </summary>
        public static async Task<(bool Ok, string Error, List<GMEventItem> Events, bool EndpointMissing)> WaitGMEventsAsync(
            AppSettings settings, string host, int port, string ticket, long afterId, string since,
            CancellationToken token)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                // 超时必须大于服务端挂起上限（25 秒），否则每轮都会以"超时异常"结束。
                using var client = CreateClient(settings, baseUrl, TimeSpan.FromSeconds(40));
                var url = $"{baseUrl}/launcher/v1/gm-events/wait?ticket=" +
                          Uri.EscapeDataString(ticket) +
                          "&after_id=" + afterId.ToString(System.Globalization.CultureInfo.InvariantCulture) +
                          SinceParam(since);
                var response = await client.GetAsync(url, token).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync(token).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    var missing = response.StatusCode == System.Net.HttpStatusCode.NotFound;
                    return (false, FriendlyApiError((int)response.StatusCode, body, "等待 GM 事件失败"),
                        new List<GMEventItem>(), missing);
                }
                using var doc = JsonDocument.Parse(body);
                if (!doc.RootElement.TryGetProperty("events", out var eventsProp) ||
                    eventsProp.ValueKind != JsonValueKind.Array)
                {
                    return (true, "", new List<GMEventItem>(), false);
                }
                var list = new List<GMEventItem>();
                foreach (var item in eventsProp.EnumerateArray())
                {
                    var ev = JsonSerializer.Deserialize<GMEventItem>(item.GetRawText(), JsonOpts);
                    if (ev is not null)
                    {
                        list.Add(ev);
                    }
                }
                return (true, "", list, false);
            }
            catch (OperationCanceledException)
            {
                throw; // 窗口关闭/重登：交给调用方按正常退出处理
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), new List<GMEventItem>(), false);
            }
        }

        /// <summary>
        /// 90 PVF 加密 key 下发：GET /launcher/v1/pvf/key?ticket=...&amp;file_sha256=...
        /// 登录器带本机 Script.pvf 的 SHA-256 换取当前生效 key_set（96 hex），供 DNFHelper 注入换钥。
        /// 返回约定：Ok=true 且 Key!=null = 命中需注入；Ok=true 且 Key==null = 服务端加密未启用（放行）；
        /// Ok=false = 阻断（404 客户端不一致 / 401 会话失效 / 网络错误）。
        /// Tag = 粗粒度分类（"key"/"no-encryption"/"not-found"/"unauthorized"/"http-N"/"net"/"bad-format"），
        /// 只给启动日志用：分类落盘、错误详情不落盘，避免日志里带出多余信息。
        /// </summary>
        public static async Task<(bool Ok, string Error, PvfKeyInfo? Key, string Tag)> GetPvfKeyAsync(
            AppSettings settings, string ticket, string fileSha256)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var url = baseUrl + "/launcher/v1/pvf/key?ticket=" +
                          Uri.EscapeDataString(ticket) +
                          "&file_sha256=" + Uri.EscapeDataString(fileSha256);
                var response = await client.GetAsync(url).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    if (response.StatusCode == System.Net.HttpStatusCode.NotFound)
                    {
                        return (false, "客户端PVF与服务端不一致，请更新客户端后再登录", null, "not-found");
                    }
                    if (response.StatusCode == System.Net.HttpStatusCode.Unauthorized)
                    {
                        return (false, "登录状态已失效，请重新登录", null, "unauthorized");
                    }
                    return (false, FriendlyApiError((int)response.StatusCode, body, "获取PVF密钥失败"), null,
                        "http-" + (int)response.StatusCode);
                }

                using var doc = JsonDocument.Parse(body);
                if (doc.RootElement.TryGetProperty("data", out var dataProp))
                {
                    if (dataProp.ValueKind == JsonValueKind.Object)
                    {
                        var key = JsonSerializer.Deserialize<PvfKeyInfo>(dataProp.GetRawText(), JsonOpts);
                        if (key is not null && key.KeySet.Length == 96)
                        {
                            return (true, "", key, "key");
                        }
                        return (false, "服务器返回的PVF密钥格式异常", null, "bad-format");
                    }
                    if (dataProp.ValueKind == JsonValueKind.Null)
                    {
                        // enabled=false / data=null：服务端 PVF 加密从未启用，按未加密客户端放行。
                        return (true, "", null, "no-encryption");
                    }
                }
                return (false, "服务器返回的PVF密钥格式异常", null, "bad-format");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null, "net");
            }
        }

        /// <summary>
        /// 90 PVF 最新发布信息：GET /launcher/v1/pvf/latest（无需 ticket）。
        /// 登录器自检/轮询据此判断是否需要更新客户端：
        /// Enabled=false = 服务端加密未启用（放行，无需更新）；Enabled=true 且 Info 非空 = 拿最新
        /// file_sha256 与本地 Script.pvf 比对，不一致则需要更新。
        /// </summary>
        public static async Task<(bool Ok, string Error, bool Enabled, PvfLatestInfo? Info)> GetPvfLatestAsync(
            AppSettings settings)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var response = await client.GetAsync(baseUrl + "/launcher/v1/pvf/latest").ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "获取PVF更新信息失败"), false, null);
                }

                using var doc = JsonDocument.Parse(body);
                var enabled = doc.RootElement.TryGetProperty("enabled", out var enProp) &&
                              enProp.ValueKind == JsonValueKind.True;
                PvfLatestInfo? info = null;
                if (doc.RootElement.TryGetProperty("data", out var dataProp) &&
                    dataProp.ValueKind == JsonValueKind.Object)
                {
                    info = JsonSerializer.Deserialize<PvfLatestInfo>(dataProp.GetRawText(), JsonOpts);
                }
                return (true, "", enabled, info);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), false, null);
            }
        }

        /// <summary>
        /// 90 全服公告长轮询：GET /launcher/v1/announcements/wait?ticket=...&amp;after_id=...
        /// 有 id&gt;afterId 的新公告立即返回；否则服务端挂起至多 25s 后返回 announcement=null。
        /// </summary>
        public static async Task<(bool Ok, string Error, AnnouncementItem? Announcement)> GetAnnouncementsAsync(
            AppSettings settings, string host, int port, string ticket, long afterId)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                var client = LongPollClient(settings, baseUrl);
                var url = baseUrl + "/launcher/v1/announcements/wait?ticket=" +
                          Uri.EscapeDataString(ticket) +
                          "&after_id=" + afterId.ToString(System.Globalization.CultureInfo.InvariantCulture);
                var response = await client.GetAsync(url).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "拉取公告失败"), null);
                }
                using var doc = JsonDocument.Parse(body);
                if (doc.RootElement.TryGetProperty("announcement", out var ap) &&
                    ap.ValueKind == JsonValueKind.Object)
                {
                    var item = JsonSerializer.Deserialize<AnnouncementItem>(ap.GetRawText(), JsonOpts);
                    return (true, "", item);
                }
                return (true, "", null);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), null);
            }
        }

        private static HttpClient? _longPollClient;
        private static readonly object _longPollClientLock = new();

        /// <summary>拼 since 查询参数；空串时返回空（老版运维工具不认这个参数，带上也无害）。</summary>
        private static string SinceParam(string since)
        {
            return string.IsNullOrWhiteSpace(since)
                ? ""
                : "&since=" + Uri.EscapeDataString(since.Trim());
        }

        /// <summary>所有请求都要带的基础头。</summary>
        private static void ApplyBaseHeaders(HttpClient client)
        {
            client.DefaultRequestHeaders.UserAgent.ParseAdd(UserAgent);
            client.DefaultRequestHeaders.TryAddWithoutValidation(LauncherKeyHeader, LauncherKey);
        }

        /// <summary>
        /// 直连 90 运维工具时要额外带的**身份头**。
        ///
        /// ★★ 所有访问 /launcher/v1/* 的 HttpClient 都必须调用本方法 ★★
        ///
        /// 运维工具那一侧有三道闸会读这些头，**少任何一个都会 403**：
        ///   X-Client-Edition           版本互斥（免费版↔免费版 / 外网版↔赞助版）
        ///   X-Client-Launcher-Version  登录器版本闸（缺失 = 老登录器 = 拒绝）
        ///
        /// 为什么单独抽出来：2026-09-28 踩过一个很贵的坑 ——
        /// 版本闸上线时只在 CreateClient 里装了版本头，而**公告长轮询走的是另一个工厂
        /// LongPollClient**。结果"开启登录器版本校验"之后，公告长轮询每条请求都被 403，
        /// 而轮询失败是**静默重试**的，于是表现成"全服公告莫名再也不弹了"，
        /// 查了很久才定位。抽成一个函数，就是为了让"新增一个客户端"时不会漏装。
        /// </summary>
        private static void ApplyOpsToolIdentityHeaders(HttpClient client)
        {
            client.DefaultRequestHeaders.TryAddWithoutValidation(EditionHeader, ClientEdition);
            client.DefaultRequestHeaders.TryAddWithoutValidation(LauncherVersionHeader, LauncherVersion.Current);
        }

        /// <summary>公告长轮询专用 HttpClient：Timeout 60s（服务端最多挂起 25s）。</summary>
        private static HttpClient LongPollClient(AppSettings settings, string baseUrl)
        {
            lock (_longPollClientLock)
            {
                if (_longPollClient != null)
                {
                    return _longPollClient;
                }
                var handler = new HttpClientHandler();
                var norm = NormalizeBaseUrl(baseUrl);
                if (!string.IsNullOrWhiteSpace(settings.RemoteCAFile) && File.Exists(settings.RemoteCAFile))
                {
                    var caPath = settings.RemoteCAFile;
                    handler.ServerCertificateCustomValidationCallback = (_, cert, _, _) =>
                        ValidateWithCaFile(caPath, cert);
                }
                else
                {
                    var autoCaPath = GetAutoCaPath(norm);
                    handler.ServerCertificateCustomValidationCallback = (_, cert, _, _) =>
                        ValidateAutoCert(autoCaPath, norm, cert);
                }
                var client = new HttpClient(handler) { Timeout = TimeSpan.FromSeconds(60) };
                ApplyBaseHeaders(client);
                // ★ 这里以前只装了 edition 头，漏了版本头 —— 就是上面注释里那个坑。
                ApplyOpsToolIdentityHeaders(client);
                _longPollClient = client;
                return client;
            }
        }

        /// <summary>
        /// 90 心跳续期：POST /launcher/v1/heartbeat {ticket}。
        /// 登录器在线周期任务调用，维持 launcher_sessions 记录（运维工具据此展示在线列表），
        /// 踢人事件查询也以 ticket 鉴权并保证会话未过期。
        /// </summary>
        public static async Task<(bool Ok, string Error)> HeartbeatAsync(
            AppSettings settings, string host, int port, string ticket)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?> { ["ticket"] = ticket };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");
                var response = await client.PostAsync(baseUrl + "/launcher/v1/heartbeat", content).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, "", "心跳续期失败"));
                }
                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>
        /// 90 远程注册：POST /launcher/v1/register（运维工具认证 API）。
        /// rescueCode：6 位数字救援代码（找回密码用），注册必填。
        /// registrationCode：玩家在注册界面填的注册码（服务端开了「注册需要注册码」时必填）。
        ///   与 settings.InviteCode 是两条独立通道：后者来自 client.yh 派发，前者是人工输入，
        ///   这里优先用玩家填的，没填才回落到配置里的码。
        /// </summary>
        public static async Task<(bool Ok, string Error)> RegisterForTicketAsync(
            AppSettings settings, string account, string password, string rescueCode, string? registrationCode = null)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";

                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["password"] = password,
                    ["rescue_code"] = rescueCode
                };
                var typedCode = (registrationCode ?? string.Empty).Trim();
                if (!string.IsNullOrWhiteSpace(typedCode))
                {
                    payload["invite_code"] = typedCode;
                }
                else if (!string.IsNullOrWhiteSpace(settings.InviteCode))
                {
                    payload["invite_code"] = settings.InviteCode;
                }

                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/register", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "注册失败，请检查输入是否符合要求后重试"));
                }

                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>
        /// 90 远程补设救援代码：POST /launcher/v1/rescue-code。
        /// 登录成功后（ticket 有效）为老号写入 6 位数字救援代码，覆盖原值。
        /// </summary>
        public static async Task<(bool Ok, string Error)> SetRescueCodeAsync(
            AppSettings settings, string ticket, string rescueCode)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";

                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?>
                {
                    ["ticket"] = ticket,
                    ["rescue_code"] = rescueCode
                };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/rescue-code", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "救援代码设置失败，请稍后重试"));
                }

                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>
        /// 90 远程救援代码找回密码：POST /launcher/v1/password/reset。
        /// 无需登录态，凭 账号+救援代码 改密；成功即作废该账号全部在线会话。
        /// </summary>
        public static async Task<(bool Ok, string Error)> PasswordResetAsync(
            AppSettings settings, string account, string rescueCode, string newPassword)
        {
            try
            {
                var host = (!string.IsNullOrWhiteSpace(settings.RemoteIp)
                    ? settings.RemoteIp.Trim()
                    : (settings.Host ?? "").Trim());
                var port = string.IsNullOrWhiteSpace(settings.RemotePort)
                    ? "18112"
                    : settings.RemotePort.Trim();
                var baseUrl = $"https://{host}:{port}";

                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["rescue_code"] = rescueCode,
                    ["new_password"] = newPassword
                };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/password/reset", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "密码重置失败，请稍后重试"));
                }

                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>POST /launcher/v1/accounts：远程注册账号（带邀请码）</summary>
        public static async Task<(bool Ok, string Error)> RegisterAsync(
            AppSettings settings, string account, string password)
        {
            try
            {
                var baseUrl = NormalizeBaseUrl(settings.RemoteServerUrl);
                using var client = CreateClient(settings);

                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["password"] = password
                };
                if (!string.IsNullOrWhiteSpace(settings.InviteCode))
                {
                    payload["invite_code"] = settings.InviteCode;
                }

                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/accounts", content).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                    return (false, FriendlyApiError((int)response.StatusCode, body, "注册失败，请检查输入是否符合要求后重试"));
                }

                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>转发器注册接口默认端口（core.py REGISTER_DEFAULT_PORT）</summary>
        private const int RegisterForwarderPort = 18113;

        /// <summary>
        /// POST http://{RemoteIp}:18113/register：路径 B（无 launcher HTTPS API）下经转发器注册账号。
        /// 转发器在服务器本地执行校验并写服务器数据库（dnf_login_accounts），校验规则与本地注册完全一致，
        /// bcrypt 同为 $2a$10。用于"注册流程和本地一样，但账号落到服务器"的外网部署场景。
        /// </summary>
        public static async Task<(bool Ok, string Error)> RegisterToForwarderAsync(
            AppSettings settings, string account, string password)
        {
            try
            {
                if (string.IsNullOrWhiteSpace(settings.RemoteIp))
                {
                    return (false, "未配置远程服务器IP：请在设置中填写服务器地址并保存后再注册");
                }

                var host = settings.RemoteIp.Trim();
                var url = $"http://{host}:{RegisterForwarderPort}/register";

                using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(10) };
                var payload = new Dictionary<string, string?>
                {
                    ["username"] = account,
                    ["password"] = password
                };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(url, content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, "", "注册服务异常，请稍后重试"));
                }

                using var doc = JsonDocument.Parse(body);
                if (doc.RootElement.TryGetProperty("ok", out var okProp) && okProp.GetBoolean())
                {
                    return (true, "");
                }

                var err = doc.RootElement.TryGetProperty("error", out var errProp) ? errProp.GetString() : "";
                return (false, ToFriendlyText(err, "服务器注册失败"));
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex));
            }
        }

        /// <summary>
        /// POST /launcher/v1/pid-bind：上报本机 DNF.exe 进程 PID 与账号绑定。
        /// 仅服务端本机场景生效（launcher 持 adminToken 转发 127.0.0.1:18111）。
        /// 远程玩家认证靠 90CN.dll HMAC，绑定失败只提示不阻断登录。
        /// 远程会话未下发 account_id 时无绑定对象，返回 null（静默跳过）。
        /// </summary>
        public static async Task<string?> BindPidAsync(AppSettings settings, int pid, string accountId)
        {
            if (string.IsNullOrWhiteSpace(accountId))
            {
                return null;
            }

            try
            {
                var baseUrl = NormalizeBaseUrl(settings.RemoteServerUrl);
                using var client = CreateClient(settings);

                var payload = new Dictionary<string, object?>
                {
                    ["pid"] = pid,
                    ["account_id"] = accountId
                };

                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(baseUrl + "/launcher/v1/pid-bind", content).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                    return FriendlyApiError((int)response.StatusCode, body, "账号绑定失败，请稍后重试");
                }

                return null;
            }
            catch (Exception ex)
            {
                return FriendlyNetError(ex);
            }
        }

        /// <summary>
        /// POST {HeadquartersUrl}/auth：多账号转发总部账号上报（源IP关联 + 同账号顶号）。
        /// 客户端连接总部转发端口（RemotePort）前必须完成上报，否则总部因无源IP映射拒绝连接。
        /// 携带 X-Control-Token（总部启用控制鉴权时必填）。失败会阻断登录。
        /// </summary>
        public static async Task<(bool Ok, string Error, bool Kicked)> NotifyHeadquartersAsync(
            AppSettings settings, string accountId, string username)
        {
            var hqUrl = (settings.HeadquartersUrl ?? string.Empty).Trim().TrimEnd('/');
            if (string.IsNullOrWhiteSpace(hqUrl))
            {
                return (true, "", false);
            }

            try
            {
                using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(10) };
                // 控制令牌为登录器内置固定值（GameTokens.DefaultControlToken），不读配置、玩家不可见
                client.DefaultRequestHeaders.TryAddWithoutValidation("X-Control-Token", GameTokens.DefaultControlToken);

                var payload = new Dictionary<string, string?>
                {
                    ["account_id"] = string.IsNullOrWhiteSpace(accountId) ? username : accountId
                };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");

                var response = await client.PostAsync(hqUrl + "/auth", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, FriendlyApiError((int)response.StatusCode, body, "通知总部失败，请稍后重试"), false);
                }

                var kicked = false;
                var reason = "";
                using (var doc = JsonDocument.Parse(body))
                {
                    if (doc.RootElement.TryGetProperty("code", out var codeProp) && codeProp.GetInt32() != 0)
                    {
                        reason = doc.RootElement.TryGetProperty("reason", out var rProp)
                            ? rProp.GetString() ?? ""
                            : "上报失败";
                        return (false, reason, false);
                    }

                    kicked = doc.RootElement.TryGetProperty("kicked", out var kProp) && kProp.GetBoolean();
                }

                return (true, "", kicked);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), false);
            }
        }

        /// <summary>
        /// 创建带 CA 信任的 HttpClient。
        /// 优先级：1) 用户指定 CA 文件 → 用该 CA 做链校验；
        ///         2) 自动缓存了服务器证书 → 用缓存证书做链校验；
        ///         3) 无任何 CA → 首次连接时自动信任并保存服务器证书（自签/自有服务器场景）。
        /// </summary>
        private static HttpClient CreateClient(AppSettings settings, string? baseUrlOverride = null, TimeSpan? timeout = null)
        {
            var handler = new HttpClientHandler();
            var baseUrl = NormalizeBaseUrl(baseUrlOverride ?? settings.RemoteServerUrl);

            if (!string.IsNullOrWhiteSpace(settings.RemoteCAFile) &&
                File.Exists(settings.RemoteCAFile))
            {
                // 用户显式指定 CA 文件
                var caPath = settings.RemoteCAFile;
                handler.ServerCertificateCustomValidationCallback = (_, cert, _, _) =>
                    ValidateWithCaFile(caPath, cert);
            }
            else
            {
                // 自动模式：先看缓存，首次连接自动信任并保存
                var autoCaPath = GetAutoCaPath(baseUrl);
                handler.ServerCertificateCustomValidationCallback = (_, cert, _, _) =>
                    ValidateAutoCert(autoCaPath, baseUrl, cert);
            }

            var client = new HttpClient(handler)
            {
                // 缺省 10 秒；长轮询（维护状态）要传更长的超时，见 WaitMaintenanceAsync。
                Timeout = timeout ?? TimeSpan.FromSeconds(10)
            };
            client.DefaultRequestHeaders.UserAgent.ParseAdd(UserAgent);
            client.DefaultRequestHeaders.TryAddWithoutValidation(LauncherKeyHeader, LauncherKey);
            // 90 运维工具 API（显式传入 baseUrlOverride=直连 18112）强制携带身份头：
            //   版本互斥（edition）+ 登录器版本闸（version），两者缺一都会被 403。
            // 旧版 remotelauncher 接口（不带 baseUrlOverride）不附加，避免影响既有 launcher。
            if (!string.IsNullOrWhiteSpace(baseUrlOverride))
            {
                ApplyOpsToolIdentityHeaders(client);
            }
            return client;
        }

        /// <summary>用指定 CA 文件校验服务器证书（CustomRootTrust 链构建）</summary>
        private static bool ValidateWithCaFile(string caPath, X509Certificate2? cert)
        {
            if (cert is null)
            {
                return false;
            }

            try
            {
                var caCert = new X509Certificate2(caPath);
                var serverCert = new X509Certificate2(cert.GetRawCertData());
                using var chain = new X509Chain();
                chain.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
                chain.ChainPolicy.CustomTrustStore.Add(caCert);
                chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
                return chain.Build(serverCert);
            }
            catch
            {
                return false;
            }
        }

        /// <summary>
        /// 自动证书校验：若已有缓存证书则按缓存校验；
        /// 若未缓存则信任当前证书并保存到缓存（首次连接自动获取）。
        /// 缓存证书与服务器证书不匹配（指纹/链校验失败）时，自动删除旧缓存并重新信任当前证书。
        /// </summary>
        private static bool ValidateAutoCert(string autoCaPath, string baseUrl, X509Certificate2? cert)
        {
            if (cert is null)
            {
                return false;
            }

            try
            {
                if (File.Exists(autoCaPath))
                {
                    // 已有缓存：按缓存校验，失败说明服务器证书已更换（如重签发），自动删除旧缓存走重新获取
                    if (ValidateWithCaFile(autoCaPath, cert))
                    {
                        return true;
                    }

                    TryDeleteAutoCert(autoCaPath);
                }

                // 首次连接 / 缓存失效重取：保存服务器证书为 CA 缓存
                File.WriteAllBytes(autoCaPath, cert.GetRawCertData());
                return true;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>尽力删除自动证书缓存文件，失败不抛出（下次连接会再次尝试）</summary>
        private static void TryDeleteAutoCert(string autoCaPath)
        {
            try
            {
                if (File.Exists(autoCaPath))
                {
                    File.Delete(autoCaPath);
                }
            }
            catch
            {
                // 忽略删除失败，缓存残留会在下次校验失败时再次尝试删除
            }
        }

        private static string NormalizeBaseUrl(string url)
        {
            return (url ?? string.Empty).Trim().TrimEnd('/');
        }

        // ==================== 用户提示中文化（2026-09 新增） ====================

        /// <summary>判断文本是否含中文字符（含中文视为已友好文案，不再二次翻译）</summary>
        private static bool ContainsChinese(string s)
        {
            foreach (var ch in s)
            {
                if (ch >= 0x4E00 && ch <= 0x9FFF)
                {
                    return true;
                }
            }
            return false;
        }

        /// <summary>
        /// 把任意用户可见原始错误文本转换为友好中文；已含中文的文本原样返回（幂等，可叠加使用）。
        /// 覆盖形态："HTTP 400：{json}"、"HTTP 401"、裸 JSON（{"code":...,"message":...}）等。
        /// 未识别语义时返回 fallback 场景文案，绝不把英文原始 JSON/异常直抛给用户。
        /// </summary>
        public static string ToFriendlyText(string? raw, string fallback)
        {
            if (string.IsNullOrWhiteSpace(raw))
            {
                return fallback;
            }

            var s = raw.Trim();
            if (ContainsChinese(s))
            {
                return s;
            }

            if (s.StartsWith("HTTP ", StringComparison.OrdinalIgnoreCase))
            {
                // 形如 "HTTP 400：{json}" / "HTTP 400：服务器未返回错误详情"
                var status = 0;
                var restStart = s.IndexOf(' ');
                if (restStart >= 0)
                {
                    var afterCode = s.IndexOf(' ', restStart + 1);
                    if (afterCode > restStart &&
                        int.TryParse(s.Substring(restStart + 1, afterCode - restStart - 1).Trim(), out var st))
                    {
                        status = st;
                    }
                    var body = afterCode > 0
                        ? s.Substring(afterCode + 1).TrimStart(' ', '：', ':').Trim()
                        : "";
                    return FriendlyApiError(status, body, fallback);
                }
            }

            var (code, message) = ExtractServerPayload(s);
            if (!string.IsNullOrWhiteSpace(code) || !string.IsNullOrWhiteSpace(message))
            {
                var mapped = MapServerCodeToChinese(code, message);
                if (mapped is not null)
                {
                    return mapped;
                }
                if (!string.IsNullOrWhiteSpace(message) && ContainsChinese(message))
                {
                    return message.Trim();
                }
            }
            return fallback;
        }

        /// <summary>HTTP/业务错误 → 友好中文（按状态码与调用场景兜底，5xx 单独提示）</summary>
        public static string FriendlyApiError(int statusCode, string? body, string fallback)
        {
            var (code, message) = ExtractServerPayload(body);
            var mapped = MapServerCodeToChinese(code, message);
            if (mapped is not null)
            {
                return mapped;
            }
            if (!string.IsNullOrWhiteSpace(message) && ContainsChinese(message))
            {
                return message.Trim();
            }
            if (statusCode == 401)
            {
                return "账号或密码错误";
            }
            if (statusCode >= 500)
            {
                return fallback + "（服务器内部错误 HTTP " + statusCode + "）";
            }
            return fallback;
        }

        /// <summary>解析服务端错误 body：兼容 {"code":..,"message":..} / {"error":..} / {"reason":..}</summary>
        private static (string? Code, string? Message) ExtractServerPayload(string? body)
        {
            if (string.IsNullOrWhiteSpace(body))
            {
                return (null, null);
            }

            var s = body.Trim();
            if (!s.StartsWith("{") || !s.EndsWith("}"))
            {
                return (null, null);
            }

            try
            {
                using var doc = JsonDocument.Parse(s);
                if (doc.RootElement.ValueKind != JsonValueKind.Object)
                {
                    return (null, null);
                }

                string? code = null;
                string? message = null;
                if (doc.RootElement.TryGetProperty("code", out var codeProp) && codeProp.ValueKind == JsonValueKind.String)
                {
                    code = codeProp.GetString();
                }
                foreach (var key in new[] { "message", "error", "reason", "msg" })
                {
                    if (doc.RootElement.TryGetProperty(key, out var msgProp) && msgProp.ValueKind == JsonValueKind.String)
                    {
                        message = msgProp.GetString();
                        break;
                    }
                }
                return (code, message);
            }
            catch (Exception)
            {
                return (null, null);
            }
        }

        /// <summary>服务端业务错误码/关键字 → 中文提示（按代码与 message 关键词双路匹配，规则对齐注册/登录校验规则）</summary>
        private static string? MapServerCodeToChinese(string? code, string? message)
        {
            var c = (code ?? string.Empty).ToLowerInvariant();
            var m = (message ?? string.Empty).Trim();
            var ml = m.ToLowerInvariant();
            bool Has(string key) =>
                c.Contains(key, StringComparison.Ordinal) ||
                ml.Contains(key, StringComparison.OrdinalIgnoreCase);

            // 登录器版本闸门（服务端 launcher-version）：**必须最先返回 null**，
            // 让调用方原样使用服务端那句中文提示。
            //
            // 为什么专门为它开一个口子：下面那张关键词表会在整条消息里做子串匹配，
            // 命中就把整句替换成它自己的文案。服务端现在发的是一句固定文案
            // （「登录器版本过旧，请联系管理员下载新版登录器」，不含任何触发词），
            // 但这个口子要一直留着 —— 它是**唯一**能保证"服务端说什么，玩家就看到什么"
            // 的开关：将来谁改文案时手滑带进 "维护"/"rate" 之类的词，
            // 没有这个口子的新登录器也会开始显示一句毫不相干的提示，而那时
            // 玩家连登录都登不上，根本无从判断。
            // 服务端侧也有一张同步的关键词表 + 单测守着（见 internal/server
            // launcher_version.go 的 launcherVersionMsgKeywords / launcherVersionBlockText）。
            if (Has("launcher-version") || Has("launcher_version"))
            {
                return null;
            }

            if (Has("banned") || Has("封禁"))
            {
                return "该账号已被封禁，如有疑问请联系管理员";
            }
            if (Has("maintenance") || Has("维护"))
            {
                return "服务器正在更新维护，请稍后再试";
            }
            if (Has("invalid-username") || Has("invalid_username") ||
                (Has("username") && (Has("3-32") || Has("3 到 32") || Has("3 to 32") || Has("too short"))))
            {
                return "用户名需 3-32 个字符";
            }
            if (Has("username") && (Has("character") || Has("characters") || Has("只能包含") || Has("字母")))
            {
                return "用户名只能包含字母、数字、下划线、中划线、点";
            }
            if (Has("password-too-short") || Has("password_too_short") ||
                Has("at least 8") || Has("8 characters") || (Has("password") && Has("too short")))
            {
                return "密码至少需要 8 个字符";
            }
            if (Has("password-too-long") || Has("password_too_long") ||
                (Has("password") && (Has("too long") || Has("72"))))
            {
                return "密码过长（最多 72 个字符）";
            }
            // ⚠ 顺序很重要（2026-09-24 线上事故）：服务端的注册码错误原文是
            // "invite code missing, invalid or already used"，里面带 "already"，
            // 如果先判下面的"已存在"规则，玩家填错注册码会被提示"该账号已注册"，
            // 完全误导。所以**注册码相关的判断必须排在"已存在"之前**。
            if (Has("registration-code-required"))
            {
                return "本次注册需要注册码，请向管理员索取后填入注册码";
            }
            if (Has("invalid-invite") || Has("invite") || Has("邀请码") || Has("注册码"))
            {
                return "注册码（邀请码）无效或已被使用，请检查后重试";
            }
            if (Has("exists") || Has("already") || Has("duplicate") || Has("taken") || Has("已注册"))
            {
                return "该账号已注册，请直接登录";
            }
            if (Has("not-found") || Has("not found") || Has("不存在") || Has("未注册"))
            {
                return "账号不存在，请先注册";
            }
            if (Has("wrong-password") || Has("invalid-credentials") || Has("invalid password") || Has("密码错误"))
            {
                return "账号或密码错误";
            }
            if (Has("rate") || Has("frequent") || Has("频繁"))
            {
                return "操作过于频繁，请稍后再试";
            }
            return null;
        }

        /// <summary>网络/证书类异常统一转中文，避免底层英文异常直接展示给用户</summary>
        private static string FriendlyNetError(Exception ex)
        {
            var m = ex?.Message ?? string.Empty;
            if (m.Contains("certificate", StringComparison.OrdinalIgnoreCase) ||
                m.Contains("证书", StringComparison.OrdinalIgnoreCase))
            {
                return "无法验证服务器证书，请检查设置中的 CA 配置，或联系服务器管理员确认证书是否更换。";
            }
            if (m.Contains("timed out", StringComparison.OrdinalIgnoreCase) ||
                m.Contains("timeout", StringComparison.OrdinalIgnoreCase) ||
                m.Contains("超时", StringComparison.OrdinalIgnoreCase))
            {
                return "连接服务器超时，请检查网络后重试。";
            }
            return "无法连接服务器，请检查网络、服务器地址与端口后重试。";
        }

        // ===== 90 外网版 CDK 兑换（/launcher/v1/characters、/launcher/v1/redeem）=====

        /// <summary>
        /// 拉取当前账号角色列表：GET /launcher/v1/characters?ticket=...
        /// 成功返回 (true, "", list)；失败返回 (false, 中文原因, 空列表)。
        /// </summary>
        public static async Task<(bool Ok, string Error, List<CdkCharacter> Characters)> GetCharactersAsync(
            AppSettings settings, string host, int port, string ticket)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var url = baseUrl + "/launcher/v1/characters?ticket=" + Uri.EscapeDataString(ticket);
                var response = await client.GetAsync(url).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, ExtractRedeemApiError((int)response.StatusCode, body, "获取角色列表失败，请稍后重试"), new List<CdkCharacter>());
                }

                using var doc = JsonDocument.Parse(body);
                if (!doc.RootElement.TryGetProperty("data", out var dataProp) ||
                    dataProp.ValueKind != JsonValueKind.Array)
                {
                    return (false, "服务器返回的角色列表格式异常", new List<CdkCharacter>());
                }

                var list = new List<CdkCharacter>();
                foreach (var item in dataProp.EnumerateArray())
                {
                    var ch = JsonSerializer.Deserialize<CdkCharacter>(item.GetRawText());
                    if (ch is not null && !string.IsNullOrWhiteSpace(ch.CharacterId))
                    {
                        list.Add(ch);
                    }
                }

                return (true, "", list);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), new List<CdkCharacter>());
            }
        }

        /// <summary>
        /// 兑换 CDK：POST /launcher/v1/redeem，body {ticket, code, character_id}。
        /// 成功返回 (true, "", name, message)——name 即 CDK 名称，message 是后端拼好的发放明细
        /// （点券到账余额、黑钻天数与到期时间、物品件数）；失败返回 (false, 中文原因, "", "")。
        /// </summary>
        public static async Task<(bool Ok, string Error, string Name, string Message)> RedeemCdkAsync(
            AppSettings settings, string host, int port, string ticket, string code, string characterId)
        {
            try
            {
                var baseUrl = $"https://{host}:{port}";
                using var client = CreateClient(settings, baseUrl);
                var payload = new Dictionary<string, string?>
                {
                    ["ticket"] = ticket,
                    ["code"] = code,
                    ["character_id"] = characterId
                };
                var content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    System.Text.Encoding.UTF8,
                    "application/json");
                var response = await client.PostAsync(baseUrl + "/launcher/v1/redeem", content).ConfigureAwait(false);
                var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    return (false, ExtractRedeemApiError((int)response.StatusCode, body, "兑换失败，请稍后重试"), "", "");
                }

                using var doc = JsonDocument.Parse(body);
                var codeOk = doc.RootElement.TryGetProperty("code", out var codeProp) &&
                             string.Equals(codeProp.GetString(), "ok", StringComparison.OrdinalIgnoreCase);
                var name = doc.RootElement.TryGetProperty("name", out var nameProp)
                    ? nameProp.GetString() ?? ""
                    : "";
                // 老后端没有 message 字段：退回只显示名称，不影响兑换结果判断。
                var message = doc.RootElement.TryGetProperty("message", out var messageProp)
                    ? messageProp.GetString() ?? ""
                    : "";
                if (!codeOk || string.IsNullOrWhiteSpace(name))
                {
                    return (false, "服务器未返回有效的兑换结果", "", "");
                }

                return (true, "", name, message);
            }
            catch (Exception ex)
            {
                return (false, FriendlyNetError(ex), "", "");
            }
        }

        /// <summary>CDK API 失败响应 → 中文原因（按后端 code 精确映射，HTTP 状态码兜底）</summary>
        private static string ExtractRedeemApiError(int statusCode, string body, string fallback)
        {
            var (code, message) = ExtractServerPayload(body);
            var c = (code ?? string.Empty).ToLowerInvariant();
            switch (c)
            {
                case "ticket-not-found":
                    return "登录已过期，请返回开始游戏页重新登录后再试";
                case "invalid-code":
                    return "兑换码无效、不存在或已停用";
                case "already-redeemed":
                    return "该账号已兑换过此兑换码，每个账号限兑 1 次";
                case "exhausted":
                    return "该兑换码次数已用完，请更换其他兑换码";
                case "character-not-owned":
                    return "所选角色不属于当前账号，请重新选择";
                case "mail-failed":
                    return "奖励邮件发放失败，请联系管理员处理";
                case "gm-unreachable":
                    return "服务器暂时不可用，请稍后再试";
                case "bad-request":
                    return "请求参数有误，请检查后重试";
                case "internal-error":
                    return "服务器内部错误，请稍后重试";
            }

            if (statusCode == 401)
            {
                return "登录已过期，请重新登录后再试";
            }

            if (statusCode >= 500)
            {
                return fallback + "（服务器内部错误 HTTP " + statusCode + "）";
            }

            if (!string.IsNullOrWhiteSpace(message) && ContainsChinese(message))
            {
                return message.Trim();
            }

            return fallback;
        }
    }
}
