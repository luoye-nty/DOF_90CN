using System;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>
    /// 游戏进程账号绑定：启动 DNF.exe 后向本机 DNF90 服务端管理接口上报 {pid, account_id}，
    /// 服务端按进程 PID 关联账号，保证双开时各游戏进程进入各自账号，而不是全部沿用第一个账号。
    /// 管理接口令牌（X-Admin-Token）由用户在设置页服务器配置处填写并持久化到 config.json，
    /// 不再探测 instance.json（目录换名/换端时 token 易取错导致 401）。
    ///
    /// 登录器身份强绑定：服务端开了 launcheridentity 时，这个上报还必须带上
    /// "本登录器确实是这个 PID / 这个账号的发起者"的签名（lid_lease），
    /// 否则服务端会以 401 拒绝，游戏连不进频道。
    /// </summary>
    public static class AccountBindingService
    {
        /// <summary>本机服务端管理接口地址（对应 instance.json 的 server.adminListen）</summary>
        private const string AdminBaseUrl = "http://127.0.0.1:18111";

        /// <summary>把游戏进程 PID 绑定到账号；返回空字符串表示成功，否则为错误信息</summary>
        public static async Task<string> BindAsync(int pid, string accountId)
        {
            if (pid <= 0 || string.IsNullOrWhiteSpace(accountId))
            {
                return "进程或账号信息无效，无法绑定";
            }

            var token = AppConfig.Settings.AdminToken?.Trim() ?? "";
            if (string.IsNullOrEmpty(token))
            {
                return "请先在设置-服务器配置中填写 Admin Token";
            }

            // 登录器身份证明：服务端没开强绑定（或没有 /local/lid-info）时它是 null，
            // 这时请求体与加这个功能之前逐字节一致。先带证明试一次，被 400 打回
            // （老版本服务端 DisallowUnknownFields 会把 lid_lease 当成未知字段）
            // 再退回不带证明的形式，保证新登录器照样能连老服务端。
            string? lease = null;
            var leaseProbeError = "";
            try
            {
                lease = await LauncherAttestationService.BuildLocalLeaseAsync(pid, accountId)
                    .ConfigureAwait(false);
                leaseProbeError = LauncherAttestationService.LastError;
            }
            catch (Exception ex)
            {
                leaseProbeError = ex.Message;
            }

            var first = await PostBindAsync(pid, accountId, token, lease).ConfigureAwait(false);
            if (first.Ok)
            {
                return string.Empty;
            }

            if (lease is not null && first.LegacyFieldRejected)
            {
                var second = await PostBindAsync(pid, accountId, token, null).ConfigureAwait(false);
                if (second.Ok)
                {
                    return string.Empty;
                }
                return second.Message;
            }

            if (first.Message.Length > 0 && leaseProbeError.Length > 0)
            {
                // 绑定失败且身份探测也失败时，把两条线索一起给出来：
                // 开了 enforce 的服务端会把"没有证明"表现成绑定被拒。
                return first.Message + "（身份探测：" + leaseProbeError + "）";
            }
            return first.Message;
        }

        /// <summary>一次绑定请求的结果。LegacyFieldRejected 表示该服务端不认识 lid_lease。</summary>
        private readonly struct BindResult
        {
            public BindResult(bool ok, string message, bool legacyFieldRejected)
            {
                Ok = ok;
                Message = message;
                LegacyFieldRejected = legacyFieldRejected;
            }

            public bool Ok { get; }
            public string Message { get; }
            public bool LegacyFieldRejected { get; }
        }

        private static async Task<BindResult> PostBindAsync(int pid, string accountId, string token, string? lease)
        {
            try
            {
                var body = lease is null
                    ? JsonSerializer.Serialize(new { pid, account_id = accountId })
                    : JsonSerializer.Serialize(new { pid, account_id = accountId, lid_lease = lease });

                using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(5) };
                using var request = new HttpRequestMessage(
                    HttpMethod.Post,
                    AdminBaseUrl + "/local/client-account")
                {
                    Content = new StringContent(body, Encoding.UTF8, "application/json")
                };
                request.Headers.TryAddWithoutValidation("X-Admin-Token", token);

                using var response = await client.SendAsync(request).ConfigureAwait(false);
                if (response.IsSuccessStatusCode)
                {
                    return new BindResult(true, string.Empty, false);
                }

                var code = (int)response.StatusCode;
                var detail = "";
                try
                {
                    detail = (await response.Content.ReadAsStringAsync().ConfigureAwait(false)).Trim();
                }
                catch
                {
                    // 读 body 失败不影响错误分类。
                }

                if (code == 401)
                {
                    if (detail.Contains("launcher identity", StringComparison.OrdinalIgnoreCase))
                    {
                        return new BindResult(false,
                            "账号绑定失败：该服务端已开启登录器强绑定，本次登录器身份证明未被接受。\n" +
                            "请确认登录器与 launcher_identity.key 是同一套（或用 DNF90Control launcher-identity keygen 重新生成后同步两边）。",
                            false);
                    }
                    return new BindResult(false,
                        "账号绑定失败：Admin Token 无效或与服务端不一致，请在 设置-服务器配置 中更新令牌后重试。",
                        false);
                }
                if (code == 404)
                {
                    return new BindResult(false,
                        "账号绑定失败：服务端管理接口不存在，请确认服务端版本支持账号绑定。",
                        false);
                }
                if (code == 400)
                {
                    // 老服务端把 lid_lease 当未知字段直接 400；调用方会去掉证明重试。
                    return new BindResult(false,
                        "账号绑定失败（HTTP 400）：" + (detail.Length > 0 ? detail : "请求被服务端拒绝"),
                        true);
                }
                if (code >= 500)
                {
                    return new BindResult(false,
                        "账号绑定失败：服务端内部错误（HTTP " + code + "），请稍后重试。",
                        false);
                }
                return new BindResult(false,
                    "账号绑定失败（HTTP " + code + "），请稍后重试。",
                    false);
            }
            catch (Exception)
            {
                return new BindResult(false,
                    "账号绑定失败：无法连接本机服务端管理接口（127.0.0.1:18111），请确认服务端已启动后重试。",
                    false);
            }
        }

    }
}
