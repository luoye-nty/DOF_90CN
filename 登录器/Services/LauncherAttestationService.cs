using System;
using System.Net.Http;
using System.Text.Json;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>
    /// 登录器身份证明的取用入口：本地模式问服务端要 server_id 再签，远程模式直接签票。
    ///
    /// 这里的每一次失败都是"降级"而不是"报错"：服务端没开强绑定、或是老版本服务端
    /// （没有 /local/lid-info），就返回 null，调用方按老流程继续。
    /// 真正被服务端拒绝时，错误会在绑定/登录那一步明确报出来，不会在这里被吞掉。
    /// </summary>
    public static class LauncherAttestationService
    {
        /// <summary>本机服务端管理接口地址（instance.json 的 server.adminListen）。</summary>
        private const string AdminBaseUrl = "http://127.0.0.1:18111";

        /// <summary>服务端 /local/lid-info 的响应。</summary>
        private sealed class LidInfo
        {
            public bool Enabled { get; set; }
            public string Mode { get; set; } = "";
            public string ServerId { get; set; } = "";
        }

        /// <summary>最近一次探测到的服务端绑定模式（off / audit / enforce；""=未知）。</summary>
        public static string LastServerMode { get; private set; } = "";

        /// <summary>最近一次失败原因（""=无）。</summary>
        public static string LastError { get; private set; } = "";

        /// <summary>
        /// 本地模式：读取本机服务端身份信息并签一份 (server_id, pid, account) 租约。
        /// 返回 null 表示"这次不需要/无法提供证明"，调用方按老方式绑定即可。
        /// </summary>
        public static async Task<string?> BuildLocalLeaseAsync(int pid, string accountId)
        {
            LastError = "";
            if (!LauncherIdentity.Available)
            {
                // 本构建没有内置私钥，也没放外部密钥文件：明确记下来，
                // 否则开了 enforce 的服务端只会表现成"绑定失败"，很难排查。
                LastError = "本登录器未配置身份私钥";
                return null;
            }

            var token = AppConfig.Settings.AdminToken?.Trim() ?? "";
            if (string.IsNullOrEmpty(token))
            {
                LastError = "未配置 Admin Token";
                return null;
            }

            try
            {
                using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(5) };
                using var request = new HttpRequestMessage(
                    HttpMethod.Get,
                    AdminBaseUrl + "/local/lid-info");
                request.Headers.TryAddWithoutValidation("X-Admin-Token", token);

                using var response = await client.SendAsync(request).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    // 404 = 老版本服务端没有这个接口；401 = 令牌不对（绑定那一步会再报一次）。
                    LastError = "服务端未提供身份接口（HTTP " + (int)response.StatusCode + "）";
                    LastServerMode = "";
                    return null;
                }

                var json = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
                var info = JsonSerializer.Deserialize<LidInfo>(json, new JsonSerializerOptions
                {
                    PropertyNameCaseInsensitive = true
                });
                if (info is null)
                {
                    LastError = "身份接口响应格式异常";
                    return null;
                }

                LastServerMode = info.Mode ?? "";
                if (!info.Enabled)
                {
                    // 服务端没开强绑定：不出示证明，保持与旧流程完全一致。
                    return null;
                }

                var lease = LauncherIdentity.BuildLocalLease(info.ServerId, pid, accountId);
                if (lease is null)
                {
                    LastError = "签名失败：" + LauncherIdentity.LastError;
                }
                return lease;
            }
            catch (Exception ex)
            {
                LastError = "探测服务端身份失败：" + ex.Message;
                return null;
            }
        }

        /// <summary>
        /// 远程模式：把票签成租约，交给 DNF.exe 通过 DNF_LID_LEASE 注入，
        /// 由 90CN.dll 附在版本 2 的 LANA应答里。
        /// 服务端发的是版本 1 挑战（老服务端 / audit 模式）时，这份租约不会被使用，
        /// 所以对着没开强绑定的服务端多注入这个变量是安全的。
        /// </summary>
        public static string? BuildRemoteLease(string ticket)
        {
            if (string.IsNullOrWhiteSpace(ticket))
            {
                return null;
            }
            if (!LauncherIdentity.Available)
            {
                return null;
            }
            return LauncherIdentity.BuildRemoteLease(ticket);
        }
    }
}
