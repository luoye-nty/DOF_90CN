#if LVERCHECK
using System;
using System.Threading.Tasks;
using System.Windows;

namespace DNFLogin.Services
{
    /// <summary>版本校验闸门的判定结果（调用方按它决定"继续" / "中止" / "退出本进程"）。</summary>
    public enum LauncherVersionGateResult
    {
        /// <summary>放行：没开校验，或版本一致。</summary>
        Pass,
        /// <summary>禁止进入（本函数内已弹窗说明原因）。调用方必须中止当前流程。</summary>
        Blocked,
        /// <summary>已拉起新版本登录器，本进程**必须立刻退出**（替换脚本正在等它退出）。</summary>
        UpdateLaunched,
    }

    /// <summary>
    /// 登录器版本校验闸门（**仅外网版**编译，且只在远程模式下生效）。
    ///
    /// ── 整体流程（三端联动的登录器侧）────────────────────────────────────────
    ///   1. 向运维工具 GET /launcher/v1/version-check（免 ticket）拿
    ///      { enabled, required_version, download_url, configured }；
    ///   2. 没开校验 → 放行（与改动前完全一致）；
    ///   3. 版本与 LauncherVersion.Current **完全一致** → 放行；
    ///   4. 版本不符：
    ///        · 服务端没填直链 → 弹「请联系管理员」并**禁止进入登录界面**；
    ///        · 有直链         → 弹强制更新窗口，下载 → 解压 → 删原登录器 → 打开新版本，
    ///                           **更新完成前禁止进入登录界面**。
    ///
    /// ── 两个调用点（需求确认过的口径：两处都要查）──────────────────────────────
    ///   · 启动自检阶段：MainWindow.RunDemoFlowAsync（自检进度条走到一半左右时）；
    ///   · 玩家点「登录」时：LoginWindow.TryPreLoginGateAsync（三条登录路径共用）。
    ///   第二次复查是为了防止"自检通过之后服务端才发布新版本、拿着旧登录器继续登进去"。
    ///
    /// ── 拿不到版本信息时怎么办（需求确认过：严格档）────────────────────────────
    /// **弹窗提示并禁止进入**。这与其他自检项（维护/公告/DLL 拉取失败静默放行）的口径不同，
    /// 是有意为之：开着校验却放行，等于给了一条"把网掐了就能绕过校验"的路。
    /// 唯一的例外是"服务器地址根本没配"——那种情况必须放行，否则玩家永远进不了
    /// 设置页去填地址（见 ShouldSkip）。
    /// </summary>
    public static class LauncherVersionGuard
    {
        /// <summary>
        /// 启动自检阶段的版本校验。
        /// 返回 UpdateLaunched 时调用方必须立刻 return（本进程即将退出，别再往下走自检）。
        /// </summary>
        public static Task<LauncherVersionGateResult> RunSelfCheckAsync(Window? owner)
            => CheckCoreAsync(owner, "启动自检");

        /// <summary>
        /// 玩家点「登录」时的版本复查（登录前闸门里调用，认证之前）。
        /// </summary>
        public static Task<LauncherVersionGateResult> CheckBeforeLoginAsync(Window? owner)
            => CheckCoreAsync(owner, "登录前");

        /// <summary>
        /// 是否跳过校验：只有"根本没配服务器地址"这一种情况跳过。
        ///
        /// 为什么必须跳过而不是拦下：服务器地址没配时登录器会引导玩家去「设置」页填写，
        /// 而设置页在登录界面后面。若在这里拦死，玩家就永远无法完成配置 —— 死锁。
        /// （登录器在登录点击时还会再查一次，配置补好后自然会走到真正的校验。）
        /// </summary>
        private static bool ShouldSkip()
        {
            if (AppConfig.Mode != LoginMode.Remote)
            {
                return true; // 本地模式（免费版/内网直连）不走远程接口，无从校验
            }
            var settings = AppConfig.Settings;
            if (string.IsNullOrWhiteSpace(settings.RemoteIp) && string.IsNullOrWhiteSpace(settings.Host))
            {
                return true;
            }
            return false;
        }

        /// <summary>远程认证服务端点（与 LoginWindow.GetRemoteAuthEndpoint 同口径）。</summary>
        private static (string Host, int Port) RemoteEndpoint()
        {
            var settings = AppConfig.Settings;
            var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                ? settings.RemoteIp.Trim()
                : (settings.Host ?? "").Trim();
            var port = 18112;
            var cfgPort = AppConfig.CurrentProfile?.RemotePort;
            if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsed))
            {
                port = parsed;
            }
            return (host, port);
        }

        private static async Task<LauncherVersionGateResult> CheckCoreAsync(Window? owner, string phase)
        {
            if (ShouldSkip())
            {
                return LauncherVersionGateResult.Pass;
            }

            var (host, port) = RemoteEndpoint();
            var settings = AppConfig.Settings;

            var (ok, error, info) = await RemoteAuthService
                .GetLauncherVersionAsync(settings, host, port).ConfigureAwait(true);

            if (!ok || info is null)
            {
                // 拿不到版本信息：按需求"严格档"处理 —— 提示并禁止进入。
                App.LogStartup("LauncherVersionGuard(" + phase + "): 无法获取版本信息，已禁止进入 -> " + error);
                MessageBox.Show(
                    "无法连接服务器校验登录器版本，请联系管理员。\n\n" +
                    "（如果你确认网络正常，请稍后重试；多次失败请把这条提示发给管理员。）" +
                    (string.IsNullOrWhiteSpace(error) ? "" : "\n\n原因：" + error),
                    "登录器版本校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                return LauncherVersionGateResult.Blocked;
            }

            if (!info.Enabled)
            {
                App.LogStartup("LauncherVersionGuard(" + phase + "): 管理员未开启版本校验，放行（本机 " +
                               LauncherVersion.Current + "）");
                return LauncherVersionGateResult.Pass;
            }

            if (info.VersionMatches)
            {
                App.LogStartup("LauncherVersionGuard(" + phase + "): 版本一致 " + LauncherVersion.Current + "，放行");
                return LauncherVersionGateResult.Pass;
            }

            // ── 版本不符 ──────────────────────────────────────────────────────
            App.LogStartup("LauncherVersionGuard(" + phase + "): 版本不符 本机=" + LauncherVersion.Current +
                           " 需要=" + info.RequiredVersion + " configured=" + info.Configured);

            if (!info.Configured || string.IsNullOrWhiteSpace(info.DownloadUrl))
            {
                // 需求分支：勾选了校验但管理员没填直链 → 弹窗提示"请联系管理员"。
                MessageBox.Show(
                    $"登录器版本不符，请联系管理员。\n\n当前版本：{LauncherVersion.Current}\n需要版本：{info.RequiredVersion}\n\n" +
                    "（管理员已开启版本校验，但没有配置下载地址，因此无法自动更新。）",
                    "登录器版本需要更新", MessageBoxButton.OK, MessageBoxImage.Warning);
                return LauncherVersionGateResult.Blocked;
            }

            return RunUpdateFlow(owner, info);
        }

        /// <summary>
        /// 弹强制更新窗口。只在"更新成功并已拉起替换脚本"时返回 UpdateLaunched。
        ///
        /// 注意 DialogResult 的两种取值在这里的含义：
        ///   true  = 新登录器已就绪，替换脚本已拉起 → 退出本进程；
        ///   false = 玩家取消（或失败后放弃）      → **同样退出**，不许进登录界面。
        /// </summary>
        private static LauncherVersionGateResult RunUpdateFlow(Window? owner, RemoteAuthService.LauncherVersionInfo info)
        {
            var dialog = new LauncherUpdateDialog(info.DownloadUrl, info.RequiredVersion);
            if (owner is not null && owner.IsLoaded)
            {
                dialog.Owner = owner;
            }

            var result = dialog.ShowDialog();
            var applied = result == true;

            App.LogStartup("LauncherVersionGuard: 更新窗口关闭 applied=" + applied + "，退出登录器");
            ShutdownForUpdate();

            // 无论更新成功还是玩家取消，都不放行 —— 需求明确"不更新禁止进入登录界面"。
            return applied ? LauncherVersionGateResult.UpdateLaunched : LauncherVersionGateResult.Blocked;
        }

        /// <summary>
        /// 退出本进程（替换脚本正在等它退出）。
        ///
        /// 用 Application.Shutdown 而不是 Environment.Exit：前者会走 OnExit，
        /// 释放单实例互斥体与各个已登录会话的心跳线程；新登录器随后启动时才不会被
        /// 残留的互斥体挡在门外。
        /// 调用后立即返回，调用方必须马上 return（不能再往下跑自检/登录逻辑）。
        /// </summary>
        private static void ShutdownForUpdate()
        {
            try
            {
                if (Application.Current is App app)
                {
                    // 让随后启动的新实例能正常拿到单实例所有权（与"清除配置重启"同一套准备动作）。
                    app.PrepareForRestart();
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherVersionGuard: PrepareForRestart 异常（继续退出）-> " + ex.Message);
            }

            try
            {
                Application.Current?.Shutdown();
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherVersionGuard: Shutdown 异常 -> " + ex.Message);
            }
        }
    }
}
#endif
