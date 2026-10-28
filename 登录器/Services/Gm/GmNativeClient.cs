using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace DNFLogin.Gm
{
    /// <summary>instance.json 实例配置（与 Go 版 instanceConfig 对齐）</summary>
    public sealed class InstanceConfig
    {
        public InstanceServer Server { get; set; } = new();
        public InstanceDatabase Database { get; set; } = new();
        public InstanceGame Game { get; set; } = new();
    }

    public sealed class InstanceServer
    {
        public string AdminListen { get; set; } = "";
        public string AdminToken { get; set; } = "";
        public string AccountId { get; set; } = "";
    }

    public sealed class InstanceDatabase
    {
        public string Mode { get; set; } = "";
        public string Path { get; set; } = "";
        public string Host { get; set; } = "";
        public int Port { get; set; }
        public string Name { get; set; } = "";
        public string User { get; set; } = "";
        public string Password { get; set; } = "";
    }

    public sealed class InstanceGame
    {
        public string PvfPath { get; set; } = "";
    }

    // ── 原生接口响应模型 ───────────────────────────────────────

    public sealed class NativeCharacterView
    {
        public string Character_id { get; set; } = "";
        public string Account_id { get; set; } = "";
        public string Name { get; set; } = "";
        public int Level { get; set; }
        public long Gold { get; set; }
        public long Cera { get; set; }
        public bool Online { get; set; }
    }

    public sealed class NativeMailAttachment
    {
        public uint Item_id { get; set; }
        public uint Count { get; set; }
        public bool Bind { get; set; }
        public int Reinforce_level { get; set; }
        public int Amplify_level { get; set; }
        public int Amplify_type { get; set; }
    }

    public sealed class NativeMailRequest
    {
        public string Character_id { get; set; } = "";
        public string Character_name { get; set; } = "";
        public string Title { get; set; } = "";
        public string Body { get; set; } = "";
        public long Gold { get; set; }
        public List<NativeMailAttachment> Attachments { get; set; } = new();
    }

    public sealed class NativeMailResult
    {
        public string Mail_id { get; set; } = "";
        public string Recipient_character_id { get; set; } = "";
        public string Recipient_name { get; set; } = "";
        public bool Online { get; set; }
        public bool Refreshed { get; set; }
        public string Refresh_error { get; set; } = "";
    }

    public sealed class NativeWalletResult
    {
        public NativeCharacterView Character { get; set; } = new();
        public string Currency { get; set; } = "";
        public string Mode { get; set; } = "";
        public long Before { get; set; }
        public long After { get; set; }
        public long Cera { get; set; }
        public long Gold { get; set; }
        public bool Online { get; set; }
        public bool Refreshed { get; set; }
        public string Refresh_error { get; set; } = "";
    }

    public sealed class NativeSkillInfo
    {
        public string Character_id { get; set; } = "";
        public string Name { get; set; } = "";
        public int Level { get; set; }
        public int Total_sp { get; set; }
        public int Remaining_sp { get; set; }
        public int Total_tp { get; set; }
        public int Remaining_tp { get; set; }
    }

    public sealed class NativeJobInfo
    {
        public string Character_id { get; set; } = "";
        public int Job { get; set; }
        public int First_grow { get; set; }
        public int Second_grow { get; set; }
    }

    public sealed class NativeUnlockDungeonsResult
    {
        public int Count { get; set; }
    }

    public sealed class NativeCurrencyMailResult
    {
        public NativeCharacterView Character { get; set; } = new();
        public string Currency { get; set; } = "";
        public string Mode { get; set; } = "";
        public long Amount { get; set; }
        public long Before { get; set; }
        public long After { get; set; }
        public long Cera { get; set; }
        public long Gold { get; set; }
        public bool Online { get; set; }
        public bool Refreshed { get; set; }
        public string Delivery { get; set; } = "";
        public NativeMailResult Mail { get; set; } = new();
        public string Mail_error { get; set; } = "";
    }

    public sealed class NativeItemMailResult
    {
        public string Character { get; set; } = "";
        public uint Item_id { get; set; }
        public string Item_name { get; set; } = "";
        public uint Amount { get; set; }
        public string Route { get; set; } = "";
        public NativeMailResult Mail { get; set; } = new();
    }

    public sealed class NativeItemPreviewResult
    {
        public string Character { get; set; } = "";
        public uint Item_id { get; set; }
        public string Item_name { get; set; } = "";
        public uint Amount { get; set; }
        public string Kind { get; set; } = "";
        public string Route { get; set; } = "";
        public int Reinforce_level { get; set; }
        public int Amplify_level { get; set; }
        public int Amplify_type { get; set; }
    }

    /// <summary>原生 GM 接口信封</summary>
    internal sealed class NativeEnvelope
    {
        public bool Ok { get; set; }
        public string Message { get; set; } = "";
        public string Error { get; set; } = "";
        public JsonElement? Data { get; set; }
    }

    /// <summary>
    /// DNF90Server 原生 GM 接口客户端（移植 Go 版 native_admin.go）。
    /// 通过 instance.json 的 server.adminListen + server.adminToken 调用 /local/dnf-gm/*。
    /// </summary>
    public sealed class GmNativeClient
    {
        private static readonly HttpClient Http = CreateClient();

        private static HttpClient CreateClient()
        {
            var handler = new SocketsHttpHandler
            {
                Proxy = null,
                UseProxy = false,
                AutomaticDecompression = DecompressionMethods.None,
                MaxConnectionsPerServer = 2,
                PooledConnectionIdleTimeout = TimeSpan.FromSeconds(30),
                AllowAutoRedirect = false,
                ConnectTimeout = TimeSpan.FromSeconds(12),
            };
            return new HttpClient(handler) { Timeout = TimeSpan.FromSeconds(12) };
        }

        private static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

        public const long MaxWalletBalance = int.MaxValue;

        // ── instance.json 读取 ─────────────────────────────────

        public static InstanceConfig ReadInstanceConfig(GmSettings settings)
        {
            var path = (settings.InstancePath ?? "").Trim();
            if (path.Length == 0)
            {
                throw new InvalidOperationException("未配置 instance.json 路径");
            }
            if (!File.Exists(path))
            {
                throw new InvalidOperationException($"读取 instance.json: 文件不存在 {path}");
            }
            var raw = File.ReadAllText(path);
            var instance = JsonSerializer.Deserialize<InstanceConfig>(raw, JsonOpts);
            if (instance is null)
            {
                throw new InvalidOperationException("解析 instance.json 失败");
            }
            return instance;
        }

        /// <summary>instance.json 数据库模式（读取失败返回 ""）</summary>
        public static string InstanceDatabaseMode(GmSettings settings)
        {
            try
            {
                return (ReadInstanceConfig(settings).Database.Mode ?? "").Trim().ToLowerInvariant();
            }
            catch
            {
                return "";
            }
        }

        /// <summary>将 adminListen 校验为回环地址，返回 base URL（拒绝非回环）</summary>
        public static string LoopbackAdminAddress(string listen)
        {
            listen = (listen ?? "").Trim();
            var host = "";
            var port = "";
            var lastColon = listen.LastIndexOf(':');
            if (lastColon > 0 && lastColon < listen.Length - 1)
            {
                host = listen.Substring(0, lastColon).Trim();
                port = listen.Substring(lastColon + 1).Trim();
            }
            if (host.Length == 0 || port.Length == 0)
            {
                throw new InvalidOperationException($"server.adminListen 无效: {listen}");
            }
            var isLocalhost = string.Equals(host, "localhost", StringComparison.OrdinalIgnoreCase);
            IPAddress? ip = null;
            if (!isLocalhost && !IPAddress.TryParse(host, out ip))
            {
                // 尝试解析主机名
                try
                {
                    var addresses = Dns.GetHostAddresses(host);
                    ip = addresses.FirstOrDefault(a => a.AddressFamily == AddressFamily.InterNetwork
                                                       || a.AddressFamily == AddressFamily.InterNetworkV6);
                }
                catch
                {
                    ip = null;
                }
            }
            if (!isLocalhost && (ip is null || !IPAddress.IsLoopback(ip)))
            {
                throw new InvalidOperationException($"拒绝非回环管理地址 {listen}");
            }
            if (!int.TryParse(port, out var portNumber) || portNumber < 1 || portNumber > 65535)
            {
                throw new InvalidOperationException($"server.adminListen 端口无效: {port}");
            }
            var hostPart = isLocalhost ? "127.0.0.1" : host;
            return $"http://{hostPart}:{portNumber}";
        }

        // ── 核心调用 ──────────────────────────────────────────

        /// <summary>调用 DNF90Server 原生 GM 接口，返回 message；target 接收 data</summary>
        public static async Task<string> CallNativeAdminAsync(GmSettings settings, string method, string path,
            object? requestBody, object? target, CancellationToken cancellationToken = default)
        {
            var instance = ReadInstanceConfig(settings);
            var address = LoopbackAdminAddress(instance.Server.AdminListen ?? "");
            var token = (instance.Server.AdminToken ?? "").Trim();
            if (token.Length == 0 || string.Equals(token, "AUTO_GENERATE", StringComparison.OrdinalIgnoreCase))
            {
                throw new InvalidOperationException("instance.json 尚未生成 server.adminToken");
            }

            StringContent? content = null;
            if (requestBody is not null)
            {
                var raw = JsonSerializer.Serialize(requestBody, JsonOpts);
                content = new StringContent(raw, Encoding.UTF8, "application/json");
            }

            using var request = new HttpRequestMessage(new HttpMethod(method), address + path);
            request.Headers.TryAddWithoutValidation("Accept", "application/json");
            request.Headers.TryAddWithoutValidation("X-Admin-Token", token);
            if (content is not null)
            {
                request.Content = content;
            }

            HttpResponseMessage response;
            try
            {
                response = await Http.SendAsync(request, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException($"连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
            }
            using (response)
            {
                var text = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
                NativeEnvelope? envelope = null;
                try
                {
                    envelope = JsonSerializer.Deserialize<NativeEnvelope>(text, JsonOpts);
                }
                catch
                {
                    envelope = null;
                }
                if (envelope is null)
                {
                    throw new InvalidOperationException("解析 DNF90Server 原生 GM 响应失败");
                }
                if ((int)response.StatusCode < 200 || (int)response.StatusCode >= 300 || !envelope.Ok)
                {
                    var message = (envelope.Error ?? "").Trim();
                    if (message.Length == 0)
                    {
                        message = (envelope.Message ?? "").Trim();
                    }
                    if (message.Length == 0)
                    {
                        message = $"GM 服务返回异常状态（HTTP {(int)response.StatusCode}），请确认服务端 GM 功能已开启";
                    }
                    throw new InvalidOperationException($"DNF90Server 原生 GM 接口: {message}");
                }
                if (target is not null && envelope.Data.HasValue
                    && envelope.Data.Value.ValueKind != JsonValueKind.Null
                    && envelope.Data.Value.ValueKind != JsonValueKind.Undefined)
                {
                    var dataJson = envelope.Data.Value.GetRawText();
                    try
                    {
                        var parsed = JsonSerializer.Deserialize(dataJson, target.GetType(), JsonOpts);
                        if (parsed is System.Collections.IList list && target is System.Collections.IList targetList)
                        {
                            targetList.Clear();
                            foreach (var item in list)
                            {
                                targetList.Add(item);
                            }
                        }
                        else if (parsed is not null)
                        {
                            foreach (var prop in parsed.GetType().GetProperties())
                            {
                                if (prop.CanWrite && prop.GetIndexParameters().Length == 0)
                                {
                                    prop.SetValue(target, prop.GetValue(parsed));
                                }
                            }
                        }
                    }
                    catch (Exception ex)
                    {
                        throw new InvalidOperationException($"解析 DNF90Server 原生 GM 数据: {ex.Message}", ex);
                    }
                }
                return envelope.Message ?? "";
            }
        }

        // ── 原生接口封装 ──────────────────────────────────────

        public static Task<List<NativeCharacterView>> QueryCharactersAsync(GmSettings settings,
            string username, string characterName, CancellationToken cancellationToken = default)
        {
            var path = "/local/dnf-gm/characters";
            var query = new List<string>();
            if (!string.IsNullOrWhiteSpace(username))
            {
                query.Add("username=" + Uri.EscapeDataString(username.Trim()));
            }
            if (!string.IsNullOrWhiteSpace(characterName))
            {
                query.Add("character_name=" + Uri.EscapeDataString(characterName.Trim()));
            }
            if (query.Count > 0)
            {
                path += "?" + string.Join("&", query);
            }
            var rows = new List<NativeCharacterView>();
            return CallAndGetAsync(settings, path, rows, async message =>
            {
                // 校验服务端版本支持按条件查询
                if ((!string.IsNullOrWhiteSpace(username) || !string.IsNullOrWhiteSpace(characterName))
                    && message != "character query completed")
                {
                    throw new InvalidOperationException("当前运行的 DNF90Server 版本不支持登录账号/角色名查询，请替换新版服务端后重启");
                }
            }, cancellationToken);
        }

        private static async Task<List<T>> CallAndGetAsync<T>(GmSettings settings, string path, List<T> rows,
            Action<string>? onMessage, CancellationToken cancellationToken)
        {
            var message = await CallNativeAdminAsync(settings, "GET", path, null, rows, cancellationToken).ConfigureAwait(false);
            onMessage?.Invoke(message);
            return rows;
        }

        public static async Task<NativeWalletResult> WalletQueryAsync(GmSettings settings, string roleName,
            CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?> { ["character_name"] = roleName.Trim() };
            var result = new NativeWalletResult();
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/wallet/query", body, result, cancellationToken).ConfigureAwait(false);
            return result;
        }

        public static async Task<NativeWalletResult> WalletUpdateAsync(GmSettings settings, string roleName,
            string currency, string mode, long amount, CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?>
            {
                ["character_name"] = roleName.Trim(),
                ["currency"] = currency,
                ["mode"] = mode,
                ["amount"] = amount,
            };
            var result = new NativeWalletResult();
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/wallet/update", body, result, cancellationToken).ConfigureAwait(false);
            return result;
        }

        /// <summary>发送邮件（原生）</summary>
        public static async Task<string> SendMailAsync(GmSettings settings, NativeMailRequest mailRequest,
            NativeMailResult result, CancellationToken cancellationToken = default)
        {
            return await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/mail", mailRequest, result, cancellationToken).ConfigureAwait(false);
        }

        public static async Task<NativeSkillInfo> QuerySPTPAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            var result = new NativeSkillInfo();
            var path = "/local/dnf-gm/character/sptp?id=" + Uri.EscapeDataString(characterId);
            await CallNativeAdminAsync(settings, "GET", path, null, result, cancellationToken).ConfigureAwait(false);
            return result;
        }

        public static async Task UpdateSPTPAsync(GmSettings settings, string characterId, int? remainingSp,
            int? remainingTp, CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?> { ["character_id"] = characterId };
            if (remainingSp.HasValue)
            {
                body["remaining_sp"] = remainingSp.Value;
            }
            if (remainingTp.HasValue)
            {
                body["remaining_tp"] = remainingTp.Value;
            }
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/character/sptp", body, null, cancellationToken).ConfigureAwait(false);
        }

        public static async Task SetLevelAsync(GmSettings settings, string characterId, int level,
            CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?> { ["character_id"] = characterId, ["level"] = level };
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/character/level", body, null, cancellationToken).ConfigureAwait(false);
        }

        public static async Task<NativeJobInfo> QueryJobAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            var result = new NativeJobInfo();
            var path = "/local/dnf-gm/character/job?id=" + Uri.EscapeDataString(characterId);
            await CallNativeAdminAsync(settings, "GET", path, null, result, cancellationToken).ConfigureAwait(false);
            return result;
        }

        public static async Task SetJobAsync(GmSettings settings, string characterId, int job, int firstGrow,
            int secondGrow, CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?>
            {
                ["character_id"] = characterId,
                ["job"] = job,
                ["first_grow"] = firstGrow,
                ["second_grow"] = secondGrow,
            };
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/character/job", body, null, cancellationToken).ConfigureAwait(false);
        }

        public static async Task<int> UnlockDungeonsAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            var result = new NativeUnlockDungeonsResult();
            var body = new Dictionary<string, object?>
            {
                ["character_id"] = characterId,
                ["dungeons"] = GmClassData.DungeonsMaxDifficulty,
            };
            await CallNativeAdminAsync(settings, "POST", "/local/dnf-gm/dungeons/unlock-all", body, result, cancellationToken).ConfigureAwait(false);
            return result.Count;
        }

        public static async Task<Dictionary<string, object?>> UnlockEquipSlotsAsync(GmSettings settings,
            string characterId, CancellationToken cancellationToken = default)
        {
            var body = new Dictionary<string, object?> { ["character_id"] = characterId };
            var raw = await CallNativeAdminRawAsync(settings, "POST", "/local/dnf-gm/equip-slots/unlock-all",
                body, cancellationToken).ConfigureAwait(false);
            var result = new Dictionary<string, object?>();
            if (raw is not null)
            {
                var doc = JsonDocument.Parse(raw);
                if (doc.RootElement.ValueKind == JsonValueKind.Object)
                {
                    foreach (var prop in doc.RootElement.EnumerateObject())
                    {
                        result[prop.Name] = prop.Value.ValueKind == JsonValueKind.Null ? null : prop.Value.GetRawText();
                    }
                }
            }
            result["ok"] = true;
            return result;
        }

        private static async Task<string?> CallNativeAdminRawAsync(GmSettings settings, string method, string path,
            object? requestBody, CancellationToken cancellationToken)
        {
            var instance = ReadInstanceConfig(settings);
            var address = LoopbackAdminAddress(instance.Server.AdminListen ?? "");
            var token = (instance.Server.AdminToken ?? "").Trim();
            if (token.Length == 0 || string.Equals(token, "AUTO_GENERATE", StringComparison.OrdinalIgnoreCase))
            {
                throw new InvalidOperationException("instance.json 尚未生成 server.adminToken");
            }
            StringContent? content = null;
            if (requestBody is not null)
            {
                content = new StringContent(JsonSerializer.Serialize(requestBody, JsonOpts), Encoding.UTF8, "application/json");
            }
            using var request = new HttpRequestMessage(new HttpMethod(method), address + path);
            request.Headers.TryAddWithoutValidation("Accept", "application/json");
            request.Headers.TryAddWithoutValidation("X-Admin-Token", token);
            if (content is not null)
            {
                request.Content = content;
            }
            HttpResponseMessage response;
            try
            {
                response = await Http.SendAsync(request, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException($"连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
            }
            using (response)
            {
                var text = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
                NativeEnvelope? envelope = null;
                try
                {
                    envelope = JsonSerializer.Deserialize<NativeEnvelope>(text, JsonOpts);
                }
                catch
                {
                    envelope = null;
                }
                if (envelope is null)
                {
                    throw new InvalidOperationException("解析 DNF90Server 原生 GM 响应失败");
                }
                if ((int)response.StatusCode < 200 || (int)response.StatusCode >= 300 || !envelope.Ok)
                {
                    var message = (envelope.Error ?? "").Trim();
                    if (message.Length == 0)
                    {
                        message = (envelope.Message ?? "").Trim();
                    }
                    if (message.Length == 0)
                    {
                        message = $"GM 服务返回异常状态（HTTP {(int)response.StatusCode}），请确认服务端 GM 功能已开启";
                    }
                    throw new InvalidOperationException($"DNF90Server 原生 GM 接口: {message}");
                }
                return envelope.Data.HasValue ? envelope.Data.Value.GetRawText() : null;
            }
        }
    }
}
