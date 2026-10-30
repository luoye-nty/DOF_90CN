using AduSkin.Controls.Metro;
using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Media.Animation;

namespace DNFLogin
{
    /// <summary>
    /// 启动自检结果：服务器配置是否缺失、游戏目录是否缺失、缺失资源列表
    /// </summary>
    public sealed class StartupCheckResult
    {
        public bool ServerConfigMissing { get; set; }
        public bool AdminTokenMissing { get; set; }
        public bool GamePathMissing { get; set; }
        public List<string> MissingResources { get; } = new();
    }

    /// <summary>
    /// 启动器主界面（界面演示版）：展示启动界面与进度条效果。
    /// </summary>
    public partial class MainWindow : AduWindow
    {
        private static readonly TimeSpan LoadingAnimationDuration = TimeSpan.FromSeconds(8);
        private static readonly TimeSpan CharacterRevealDuration = TimeSpan.FromSeconds(5.7);
        private readonly TaskCompletionSource<bool> _introFinished =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private Storyboard? _introStoryboard;
        private bool _hasShown;
        private bool _loginWindowOpened;
        private bool _checksCompleted;
        private bool _animationCompleted;
        private bool _launcherUpdating;
        private double _progressTarget;

        /// <summary>最近一次启动自检结果，供 LoginWindow 读取后自动跳转设置页</summary>
        public static StartupCheckResult? LastCheckResult { get; set; }

        public MainWindow()
        {
            InitializeComponent();
            Title = "";
        }

        private async void Window_Loaded(object sender, RoutedEventArgs e)
        {
            StartLoadingIntro();

            // Local design-review mode: render the full intro without touching
            // launcher settings, network services, shortcuts, or client files.
            if (string.Equals(Environment.GetEnvironmentVariable("DNF90_LOADING_PREVIEW"), "1",
                    StringComparison.OrdinalIgnoreCase))
            {
                App.LogStartup("LoadingIntro: preview mode enabled; startup checks skipped");
                return;
            }

            await RunDemoFlowAsync();
        }

        protected override void OnContentRendered(EventArgs e)
        {
            base.OnContentRendered(e);

            if (_hasShown)
            {
                return;
            }

            _hasShown = true;
            _ = RevealAfterFirstPaintAsync();
        }

        private async Task RevealAfterFirstPaintAsync()
        {
            // Keep the native splash visible while this window finishes its first
            // composition off-screen. Only then move it into place and dismiss the
            // splash, preventing the uncovered white frame between them.
            await Dispatcher.InvokeAsync(() => { }, System.Windows.Threading.DispatcherPriority.Render);
            await Task.Delay(220).ConfigureAwait(true);
            CenterOnScreen();
            await Dispatcher.InvokeAsync(() => { }, System.Windows.Threading.DispatcherPriority.Render);
            // Keep the transparent splash above the loading window until the
            // first animated frames have actually reached the compositor.
            await Task.Delay(560).ConfigureAwait(true);
            App.CloseStartupSplash();
            FadeInWindow();
        }

        private void CenterOnScreen()
        {
            var workArea = SystemParameters.WorkArea;
            Left = workArea.Left + Math.Max(0, (workArea.Width - ActualWidth) / 2);
            Top = workArea.Top + Math.Max(0, (workArea.Height - ActualHeight) / 2);
            WindowStartupLocation = WindowStartupLocation.Manual;
        }

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

        private void StartLoadingIntro()
        {
            if (_introStoryboard is not null)
            {
                return;
            }

            _introStoryboard = (Storyboard)FindResource("LoadingIntroStoryboard");
            // Progress fill/glow move by width; their scale placeholder stays neutral.
            ProgressFillScale.ScaleX = 1;
            ProgressGlowScale.ScaleX = 1;
            _introStoryboard.Begin(this, HandoffBehavior.SnapshotAndReplace, true);
            _ = CompleteCharacterRevealAsync();
        }

        private async Task CompleteCharacterRevealAsync()
        {
            await Task.Delay(CharacterRevealDuration).ConfigureAwait(true);
            if (_animationCompleted)
            {
                return;
            }

            try
            {
                // The character is already fully visible at this point. Pause here
                // instead of spending the remaining tail on an idle final frame.
                _introStoryboard?.Pause(this);
            }
            catch
            {
                // The storyboard may already be closing with the window.
            }

            App.LogStartup("LoadingIntro: 人物完整出现，动画进入最终帧");
            MarkIntroComplete();
        }

        private void LoadingIntroStoryboard_Completed(object? sender, EventArgs e)
        {
            MarkIntroComplete();
        }

        private void MarkIntroComplete()
        {
            if (_animationCompleted)
            {
                return;
            }

            _animationCompleted = true;
            SkipAnimationButton.Visibility = Visibility.Collapsed;
            App.LogStartup("LoadingIntro: 角色展示完成，停在最终帧等待自检");
            _introFinished.TrySetResult(true);
        }

        private void SkipAnimationButton_Click(object sender, RoutedEventArgs e)
        {
            TrySkipAnimation("鼠标");
        }

        private void Window_PreviewKeyDown(object sender, KeyEventArgs e)
        {
            if (_launcherUpdating)
            {
                return;
            }

            if (e.Key == Key.Space && SkipAnimationButton.Visibility == Visibility.Visible)
            {
                e.Handled = true;
                TrySkipAnimation("Space");
            }
        }

        private void TrySkipAnimation(string source)
        {
            if (_launcherUpdating || _animationCompleted || !_checksCompleted)
            {
                return;
            }

            App.LogStartup("LoadingIntro: 用户通过" + source + "跳过动画");
            try
            {
                _introStoryboard?.Seek(this, LoadingAnimationDuration, TimeSeekOrigin.BeginTime);
                _introStoryboard?.Pause(this);
            }
            catch (Exception ex)
            {
                App.LogStartup("LoadingIntro: 跳过动画定位失败 -> " + ex.Message);
            }

            MarkIntroComplete();
        }

        /// <summary>
        /// 启动自检流程：真实检查资源完整性 -> 服务器配置 -> 游戏目录 -> 进入登录界面。
        /// </summary>
        private async Task RunDemoFlowAsync()
        {
            var result = new StartupCheckResult();

            // 1) 资源完整性自检（9 个关键嵌入资源，仅校验 exe 内嵌资源）
            ReportProgress("正在检查嵌入资源", "校验 exe 内嵌资源", 5, 5);
            await Task.Delay(250).ConfigureAwait(true);

            var assets = new[]
            {
                "logo.png",
                "default-bg-img.png",
                "bg-default-Bg1.jpg",
                "icon-loginfo-menu-setting.png",
                "top-control-bar-close-icon.png",
                "top-control-bar-minimize-icon.png",
                "login-icon.ico",
                "icon-message-success.png",
                "icon-message-error.png"
            };

            for (var i = 0; i < assets.Length; i++)
            {
                var name = assets[i];
                var stepPercent = 5 + (i + 1) * 65 / assets.Length;
                var globalPercent = 5 + (i + 1) * 30 / assets.Length;
                ReportProgress("正在检查本地资源", $"校验 {name}", stepPercent, globalPercent);
                await Task.Delay(120).ConfigureAwait(true);

                if (!CheckResourceFile(name))
                {
                    result.MissingResources.Add(name);
                }
            }

            // 2) 服务器配置检查（host/port/name/user/password 任一为空即未配置）
            ReportProgress("正在检查服务器配置", "校验 host / port / name / user / password", 85, 45);
            await Task.Delay(250).ConfigureAwait(true);

            var settings = AppConfig.Settings;
            result.ServerConfigMissing =
                string.IsNullOrWhiteSpace(settings.Host) ||
                string.IsNullOrWhiteSpace(settings.Port) ||
                string.IsNullOrWhiteSpace(settings.Name) ||
                string.IsNullOrWhiteSpace(settings.User) ||
                string.IsNullOrWhiteSpace(settings.Password);

            // 2.5) Admin Token 检查：仅本地模式且 90 版需要（远程模式无 PID 绑定；A12/A21 无 AdminToken 机制）
            result.AdminTokenMissing =
                AppConfig.Mode == LoginMode.Local &&
                !Services.ServerManager.IsS4Profile(AppConfig.CurrentProfile?.Id ?? "90") &&
                string.IsNullOrWhiteSpace(settings.AdminToken);

            // 2.6) 登录器版本校验（仅外网版 · 远程模式）：
            //      向运维工具上报本机内置的版本号（LauncherVersion.Current），
            //      与运维工具内置的版本号**完全一致**才继续。
            //      不符 → 在当前加载页内强制更新（下载 → 解压 → 替换 → 自动打开新版本），
            //      **更新完成前禁止进入登录界面**；拉不到版本信息也一律禁止进入（严格档）。
            //      放在这里而不是自检最后：一是失败要尽早告诉玩家（省得先看一堆防火墙/
            //      服务端弹窗），二是版本不对时后面那些检查没有意义。
            //      注意：本地模式/未配置服务器地址时本闸门自行放行（否则玩家永远进不了
            //      设置页去填地址）——见 LauncherVersionGuard.ShouldSkip 的说明。
#if LVERCHECK
            ReportProgress("正在校验登录器版本", "向服务器核对登录器版本号", 88, 48);
            await Task.Delay(150).ConfigureAwait(true);
            try
            {
                var gate = await Services.LauncherVersionGuard
                    .RunSelfCheckAsync(this, RunInlineLauncherUpdateAsync)
                    .ConfigureAwait(true);
                if (gate != Services.LauncherVersionGateResult.Pass)
                {
                    // Blocked = 已弹窗说明原因，禁止进入；
                    // UpdateLaunched = 已拉起新版本，本进程正在退出。
                    // 两种情况都不能再往下走（尤其不能 OpenLoginWindow）。
                    App.LogStartup("RunDemoFlowAsync: 登录器版本闸门结果=" + gate + "，终止自检流程");
                    return;
                }
            }
            catch (Exception ex)
            {
                // 闸门自身异常：按"严格档"处理，禁止进入（宁可不放行，也不能静默绕过版本校验）。
                App.LogStartup("RunDemoFlowAsync: 登录器版本校验异常，已禁止进入 -> " + ex);
                MessageBox.Show("登录器版本校验失败，请联系管理员。\n\n原因：" + ex.Message,
                    "登录器版本校验失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }
#endif

            // 2.8) 客户端目录自识别：登录器已嵌入客户端目录时，自动识别当前版本 GamePath
            //      并在桌面补建登录器快捷方式（不在客户端目录运行时不做任何动作）
            try
            {
                var detectNote = Services.EmbeddedClientDetector.AutoDetectAndPrepareShortcut();
                if (!string.IsNullOrEmpty(detectNote))
                {
                    App.LogStartup("EmbeddedClientDetector: " + detectNote);
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("EmbeddedClientDetector: 自识别异常已忽略 -> " + ex.Message);
            }

            // 3) 游戏目录检查（GamePath 为空或 DNF.exe 不存在即缺失）
            ReportProgress("正在检查游戏目录", "校验 DNF.exe 路径", 95, 65);
            await Task.Delay(250).ConfigureAwait(true);

            result.GamePathMissing =
                string.IsNullOrWhiteSpace(settings.GamePath) ||
                !File.Exists(settings.GamePath);

            // 3.5) 服务端启动检测（本地模式时；远程模式不需要本地服务端）
            // 遍历三版本：任一版本服务端运行中即不再弹状态窗（本地不允许同时运行多个服务端）；
            // 仅当没有任何版本运行中、且至少一个版本配置了路径时才需要弹窗。
            var hasConfiguredServer = false;
            var anyServerRunning = false;
            ReportProgress("正在检测服务端", "检查本地服务端是否运行", 90, 70);
            await Task.Delay(250).ConfigureAwait(true);

            if (AppConfig.Mode == LoginMode.Local)
            {
                foreach (var p in AppConfig.Settings.Profiles)
                {
                    if (string.IsNullOrWhiteSpace(p.ServerRootPath) || !Directory.Exists(p.ServerRootPath))
                    {
                        continue;
                    }

                    hasConfiguredServer = true;
                    var running = await Task.Run(() => Services.ServerManager.ServerRunningForProfile(p.Id, p.ServerRootPath)).ConfigureAwait(true);
                    if (running)
                    {
                        anyServerRunning = true;
                        break;
                    }
                }
            }

            // 4) 汇总
            if (result.MissingResources.Count > 0)
            {
                ReportProgress("资源校验未通过", "缺失：" + string.Join("、", result.MissingResources), 100, 85);
            }
            else
            {
                ReportProgress("资源校验通过", "本地资源完整", 100, 85);
            }

            await Task.Delay(400).ConfigureAwait(true);

            // 5) 服务端状态：没有任何版本运行中、且至少一个版本配置了服务端路径时才弹状态窗
            //    （未选版本显示"未选择服务端目录"）；有版本运行中就不弹（不允许本地同时运行多个服务端）；
            //    三版本路径都没选也不弹。启动动作在弹窗内按版本分流完成。
            if (hasConfiguredServer && !anyServerRunning)
            {
                ReportProgress("服务端检查", "展示各版本服务端状态", 100, 90);
                await Task.Delay(250).ConfigureAwait(true);

                var dialog = new StartServerDialog { Owner = this };
                dialog.ShowDialog();
            }

            // 6) 防火墙自检（全版本）：系统防火墙开启或未完全关闭（域/专用/公用任一启用）时
            //    询问是否关闭；登录器以管理员运行（app.manifest requireAdministrator），
            //    确认后进程内直接 netsh 关闭。用户取消静默跳过，不中断流程。
            ReportProgress("正在检查防火墙", "检测系统防火墙状态", 100, 92);
            await Task.Delay(250).ConfigureAwait(true);

            if (Services.FirewallHelper.IsFirewallOn())
            {
                var fwDlg = new ConfirmDialog("关闭系统防火墙", "检测到系统防火墙未完全关闭，可能影响游戏组网，是否立即关闭？")
                {
                    Owner = this
                };
                if (fwDlg.ShowDialog() == true)
                {
                    var fwOff = Services.FirewallHelper.TurnOff();
                    ReportProgress("防火墙检查", fwOff ? "系统防火墙已关闭" : "关闭系统防火墙失败，请手动关闭或忽略", 100, 92);
                }
            }

            // 7) 90CN.dll 完整性自检（仅 90 版本且已配置游戏目录）：
            //    与登录器内嵌 90CN.dll 比对 SHA256；缺失或不一致时直接强制写入，
            //    不再出现确认框。写入失败只记录日志，登录前复检仍会阻止带错误 DLL 启动。
            var profileId90 = AppConfig.CurrentProfile?.Id ?? "90";
            var is90Profile = !Services.ServerManager.IsS4Profile(profileId90);
            var gamePath90 = AppConfig.Settings.GamePath;
            if (is90Profile && !string.IsNullOrWhiteSpace(gamePath90) && File.Exists(gamePath90))
            {
                var clientRoot90 = Path.GetDirectoryName(gamePath90) ?? string.Empty;
                var embeddedHash90 = Services.Cn90DllHelper.EmbeddedSha256();
                if (!string.IsNullOrEmpty(embeddedHash90))
                {
                    var clientHash90 = Services.Cn90DllHelper.ClientSha256(clientRoot90);
                    if (string.IsNullOrEmpty(clientHash90) ||
                        !string.Equals(clientHash90, embeddedHash90, StringComparison.OrdinalIgnoreCase))
                    {
                        ReportProgress("正在更新 90CN.dll", "正在写入登录器内置版本", 100, 93);
                        var deployError90 = Services.Cn90DllHelper.DeployTo(clientRoot90);
                        if (deployError90 is null)
                        {
                            ReportProgress("90CN.dll 检查", "90CN.dll 已强制覆盖为登录器内置版本", 100, 93);
                        }
                        else
                        {
                            App.LogStartup("RunDemoFlowAsync: 90CN.dll 自动覆盖失败 -> " + deployError90);
                        }
                    }
                }
            }

            // 7.5) 泡点信息获取（远程模式）：自检阶段从运维工具获取泡点公开信息，
            //      用于登录后跳过历史到账事件回放；失败静默不阻塞进入登录界面。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, _) = await Services.RemoteAuthService.GetPointsinkAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 泡点信息获取 " + (ok ? "成功" : "失败") +
                            " enabled=" + Services.RemoteAuthService.PointsinkEnabled);
                    }
                }
                catch (Exception ex)
                {
                    App.LogStartup("RunDemoFlowAsync: 泡点信息获取异常已忽略 -> " + ex.Message);
                }
            }

            // 7.6) 注册信息获取（远程模式）：自检阶段问运维工具"注册要不要注册码"，
            //      注册窗据此决定是否显示注册码输入框（服务端开了就必须填，否则注册必被拒）。
            //      失败静默：RegistrationCodeRequired 保持 null = 按"不需要"处理，与改动前一致。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, _) = await Services.RemoteAuthService.GetRegistrationInfoAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 注册信息获取 " + (ok ? "成功" : "失败") +
                            " code_required=" + Services.RemoteAuthService.RegistrationCodeRequired);
                    }
                }
                catch (Exception ex)
                {
                    App.LogStartup("RunDemoFlowAsync: 注册信息获取异常已忽略 -> " + ex.Message);
                }
            }

            // 7.6.1) 双开（多开）开关（远程模式）：自检阶段问运维工具"后台允不允许双开"。
            //      登录窗据此把「双开」勾选框与「副账号登录」按钮置灰（不隐藏 —— 这是
            //      "临时、随时改回来"的后台开关，与维护同类；隐藏只用于"版本不具备该能力"）。
            //
            //      ★ 失败必须静默且**保持允许双开** ★：拉不到（老运维工具没有这个端点、
            //      网络异常、后台从未设置过）时 MultiOpenEnabled 保持 null = 允许，
            //      绝不能把"没拉到"当成"关闭" —— 那会让全服双开在升级瞬间消失。
            //      免费版同样要走这一段（双开 UI 不在任何版本条件编译里）。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, _) = await Services.RemoteAuthService.GetMultiOpenAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 双开开关获取 " + (ok ? "成功" : "失败") +
                            " multi_open=" + (Services.RemoteAuthService.MultiOpenEnabled?.ToString() ?? "null(未知=允许)"));
                    }
                }
                catch (Exception ex)
                {
                    // 任何异常都不阻断进入登录界面：保持 MultiOpenEnabled 原值（通常 null = 允许双开）。
                    App.LogStartup("RunDemoFlowAsync: 双开开关获取异常已忽略（保持允许双开） -> " + ex.Message);
                }
            }

            // 7.7) 更新维护状态（远程模式）：自检阶段问一次"服务器是不是在维护"，
            //      正在维护则登录按钮不亮（登录窗打开后由长轮询持续跟进）。
            //      失败静默：不阻断进入登录界面，登录按钮按"未维护"处理。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, _) = await Services.RemoteAuthService.GetMaintenanceInfoAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 维护状态获取 " + (ok ? "成功" : "失败") +
                            " maintenance=" + Services.RemoteAuthService.MaintenanceEnabled);
                    }
                }
                catch (Exception ex)
                {
                    App.LogStartup("RunDemoFlowAsync: 维护状态获取异常已忽略 -> " + ex.Message);
                }
            }

            // 7.8) 服务器公告栏（远程模式）：自检阶段拉一次公告板（标题 + 日期，免 ticket），
            //      登录窗左侧「服务器公告」面板直接用这份内容渲染首屏，之后每 10 分钟自己刷新。
            //      失败静默：公告拉不到不该挡住登录。
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, info) = await Services.RemoteAuthService.GetNoticesAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 服务器公告栏获取 " + (ok ? "成功" : "失败") +
                            " 条数=" + (info?.Notices.Count ?? 0));
                    }
                }
                catch (Exception ex)
                {
                    App.LogStartup("RunDemoFlowAsync: 服务器公告栏获取异常已忽略 -> " + ex.Message);
                }
            }

            // 7.9) 服务端指定 DLL 清单（远程模式，仅外网版）：自检阶段先拉一次，
            //      登录页打开时就能按它校验客户端根目录；点"登录"时会再拉一次（防止自检后服务端改过）。
            //      失败静默：拉不到就沿用上一次清单（首次拉不到=不校验，不阻断登录）。
#if EXTERNAL
            if (AppConfig.Mode == LoginMode.Remote)
            {
                try
                {
                    var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                        ? settings.RemoteIp.Trim()
                        : (settings.Host ?? "").Trim();
                    var port = 18112;
                    if (int.TryParse(settings.RemotePort, out var rp) && rp > 0)
                    {
                        port = rp;
                    }
                    if (!string.IsNullOrWhiteSpace(host))
                    {
                        var (ok, _, info) = await Services.RemoteAuthService.GetDllRequirementsAsync(settings, host, port).ConfigureAwait(true);
                        App.LogStartup("RunDemoFlowAsync: 可加载 DLL 清单获取 " + (ok ? "成功" : "失败") +
                            " 条数=" + (info?.Dlls.Count ?? 0));
                    }
                }
                catch (Exception ex)
                {
                    App.LogStartup("RunDemoFlowAsync: 可加载 DLL 清单获取异常已忽略 -> " + ex.Message);
                }
            }
#endif

            ReportProgress("自检完成", "即将进入登录界面", 100, 100);
            LastCheckResult = result;
            _checksCompleted = true;
            App.LogStartup("RunDemoFlowAsync: 自检完成（MissingResources=" + result.MissingResources.Count + "）");

            if (!_animationCompleted)
            {
                // 自检先完成时，不打断 8 秒开场；让玩家可以跳过剩余动画。
                SkipAnimationButton.Visibility = Visibility.Visible;
                App.LogStartup("LoadingIntro: 自检已完成，显示跳过动画按钮");
                await _introFinished.Task.ConfigureAwait(true);
            }
            else
            {
                // 动画已经停在最终帧并等待自检，给最终睁眼画面一个很短的停留。
                await Task.Delay(180).ConfigureAwait(true);
            }

            await FadeOutAndOpenLoginWindowAsync().ConfigureAwait(true);
        }

        /// <summary>
        /// 检查资源是否已嵌入 exe：仅通过 pack URI 读取程序集内嵌资源流，
        /// 磁盘上的 Assets 目录不参与校验。
        /// </summary>
        private static bool CheckResourceFile(string name)
        {
            try
            {
                var uri = new Uri($"pack://application:,,,/Assets/{name}", UriKind.Absolute);
                using var stream = Application.GetResourceStream(uri)?.Stream;
                return stream is not null && stream.Length > 0;
            }
            catch
            {
                return false;
            }
        }

        private void ReportProgress(string title, string detail, double stepPercent, double globalPercent)
        {
            // 界面只展示总进度条；逐项标题与步骤进度仍保留在启动日志中。
            _ = stepPercent;
            App.LogStartup("StartupCheck: " + title + " - " + detail);

            var target = Math.Clamp(globalPercent, 0, 100);
            if (target <= _progressTarget)
            {
                return;
            }

            _progressTarget = target;
            SetProgressBar(target, target >= 100 ? 280 : 350);
        }

        private void SetProgressBar(double percent, int durationMilliseconds)
        {
            const double trackWidth = 760;
            var targetWidth = trackWidth * Math.Clamp(percent, 0, 100) / 100.0;
            var currentWidth = double.IsNaN(ProgressFill.Width) ? ProgressFill.ActualWidth : ProgressFill.Width;
            if (durationMilliseconds <= 0)
            {
                ProgressFill.BeginAnimation(WidthProperty, null);
                ProgressGlow.BeginAnimation(WidthProperty, null);
                ProgressTip.BeginAnimation(MarginProperty, null);
                ProgressFill.Width = targetWidth;
                ProgressGlow.Width = targetWidth;
                ProgressTip.Margin = new Thickness(Math.Max(0, targetWidth - 1), 0, 0, 0);
                ProgressTip.Opacity = percent > 0.5 ? 0.95 : 0;
                return;
            }

            var duration = TimeSpan.FromMilliseconds(durationMilliseconds);
            var ease = new CubicEase { EasingMode = EasingMode.EaseOut };

            ProgressFill.BeginAnimation(
                WidthProperty,
                new DoubleAnimation(currentWidth, targetWidth, duration) { EasingFunction = ease });
            ProgressGlow.BeginAnimation(
                WidthProperty,
                new DoubleAnimation(currentWidth, targetWidth, duration) { EasingFunction = ease });

            var tipMargin = new Thickness(Math.Max(0, targetWidth - 1), 0, 0, 0);
            ProgressTip.BeginAnimation(
                MarginProperty,
                new ThicknessAnimation(ProgressTip.Margin, tipMargin, duration) { EasingFunction = ease });
            ProgressTip.Opacity = percent > 0.5 ? 0.95 : 0;
        }

        /// <summary>
        /// 加载页内登录器强更。下载进度占进度条的 0~92%，解压与暂存占剩余区间；
        /// 更新开始后禁用动画跳过，更新成功则由版本闸门退出本进程并自动重开新版本。
        /// </summary>
        private async Task<bool> RunInlineLauncherUpdateAsync(
            Services.RemoteAuthService.LauncherVersionInfo info)
        {
            _launcherUpdating = true;
            SkipAnimationButton.Visibility = Visibility.Collapsed;
            UpdateStatusText.Visibility = Visibility.Visible;
            UpdateStatusText.Text = $"发现新版本：{LauncherVersion.Current} → {info.RequiredVersion}，正在准备更新…";
            SetProgressBar(0, 0);
            App.LogStartup("LoadingIntro: 开始加载页内登录器更新 required=" + info.RequiredVersion +
                           " url=" + info.DownloadUrl);

            try
            {
                var progress = new Progress<Services.LauncherUpdateProgress>(OnInlineLauncherUpdateProgress);
                var (plan, error) = await Services.LauncherSelfUpdater
                    .DownloadAndStageAsync(info.DownloadUrl, progress, CancellationToken.None)
                    .ConfigureAwait(true);

                if (plan is null)
                {
                    return ShowInlineLauncherUpdateFailure(error);
                }

                UpdateStatusText.Text = "更新包已就绪，正在应用并重新打开登录器…";
                SetProgressBar(100, 260);
                var (ok, applyError) = Services.LauncherSelfUpdater.ApplyAndRestart(plan);
                if (!ok)
                {
                    return ShowInlineLauncherUpdateFailure(applyError);
                }

                // 给 UI 一个短暂时间显示完成状态；替换脚本已经开始等待本进程退出。
                await Task.Delay(450).ConfigureAwait(true);
                return true;
            }
            catch (Exception ex)
            {
                return ShowInlineLauncherUpdateFailure("更新失败：" + ex.Message);
            }
        }

        private void OnInlineLauncherUpdateProgress(Services.LauncherUpdateProgress progress)
        {
            if (!_launcherUpdating)
            {
                return;
            }

            var barPercent = progress.Stage switch
            {
                Services.LauncherUpdateStage.Downloading => progress.Percent * 0.92,
                Services.LauncherUpdateStage.Extracting => 96,
                Services.LauncherUpdateStage.Staging => 99,
                _ => 0,
            };
            SetProgressBar(barPercent, 180);

            if (progress.Stage == Services.LauncherUpdateStage.Downloading && progress.Total > 0)
            {
                var doneMb = progress.Done / 1024.0 / 1024.0;
                var totalMb = progress.Total / 1024.0 / 1024.0;
                UpdateStatusText.Text =
                    $"{progress.Message}  {progress.Percent}% · {doneMb:0.0} / {totalMb:0.0} MB · {progress.SpeedMBps:0.0} MB/s";
            }
            else if (progress.Stage == Services.LauncherUpdateStage.Downloading && progress.Done > 0)
            {
                var doneMb = progress.Done / 1024.0 / 1024.0;
                UpdateStatusText.Text = $"{progress.Message}  {doneMb:0.0} MB · {progress.SpeedMBps:0.0} MB/s";
            }
            else
            {
                UpdateStatusText.Text = progress.Message;
            }
        }

        private bool ShowInlineLauncherUpdateFailure(string message)
        {
            var readable = string.IsNullOrWhiteSpace(message) ? "登录器更新失败，请稍后重试。" : message;
            UpdateStatusText.Text = readable;
            App.LogStartup("LoadingIntro: 登录器更新失败 -> " + readable);
            MessageBox.Show(
                readable + "\n\n登录器无法继续进入主界面。请重新打开登录器后再试；若持续失败，请联系管理员。",
                "登录器更新失败", MessageBoxButton.OK, MessageBoxImage.Warning);
            return false;
        }

        private async Task FadeOutAndOpenLoginWindowAsync()
        {
            _launcherUpdating = false;
            SkipAnimationButton.Visibility = Visibility.Collapsed;
            UpdateStatusText.Visibility = Visibility.Collapsed;

            // Do not make the loader transparent before the login window has rendered.
            // Showing the opaque login window first avoids the desktop/white frame that
            // used to flash between the two transparent layered windows.
            Topmost = true;
            var loginWindow = OpenLoginWindow();
            if (loginWindow is not null)
            {
                await loginWindow.WaitForContentReadyAsync().ConfigureAwait(true);
                await Task.Delay(180).ConfigureAwait(true);

                // Fade the loader's own solid transition overlay to the same dark
                // colour as the login window's base. This stays inside one rendered
                // WPF surface and avoids layered-window white flashes.
                TransitionOverlay.BeginAnimation(UIElement.OpacityProperty, null);
                TransitionOverlay.Opacity = 0;
                var fadeDone = new TaskCompletionSource<bool>(
                    TaskCreationOptions.RunContinuationsAsynchronously);
                var fade = new DoubleAnimation
                {
                    From = 0,
                    To = 1,
                    Duration = TimeSpan.FromMilliseconds(600),
                    EasingFunction = new CubicEase { EasingMode = EasingMode.EaseInOut },
                    FillBehavior = FillBehavior.HoldEnd
                };
                fade.Completed += (_, _) => fadeDone.TrySetResult(true);
                App.LogStartup("OpenLoginWindow: 加载页面开始淡出");
                TransitionOverlay.BeginAnimation(UIElement.OpacityProperty, fade);
                await Task.WhenAny(fadeDone.Task, Task.Delay(750)).ConfigureAwait(true);
                App.LogStartup("OpenLoginWindow: 加载页面淡出完成");

                Topmost = false;
                loginWindow.BeginRevealAfterLoader();
                loginWindow.Activate();
            }

            Close();
            App.LogStartup("OpenLoginWindow: 登录窗口已绘制，旧加载窗口关闭");
        }

        private LoginWindow? OpenLoginWindow()
        {
            if (_loginWindowOpened)
            {
                return null;
            }

            _loginWindowOpened = true;
            App.LogStartup("OpenLoginWindow: 开始创建 LoginWindow（当前线程 UI=" + CheckAccess() + "）");

            try
            {
                // 注意：必须同步创建并 Show 之后再 Close 旧窗口，
                // 若用 Dispatcher.InvokeAsync 异步切换，可能旧窗口关闭后应用处于零窗口状态触发 OnLastWindowClose 退出
                var loginWindow = new LoginWindow();
                App.LogStartup("OpenLoginWindow: LoginWindow 构造完成");

                Application.Current.MainWindow = loginWindow;
                loginWindow.Show();
                App.LogStartup("OpenLoginWindow: LoginWindow.Show() 已调用");
                return loginWindow;
            }
            catch (Exception ex)
            {
                App.LogStartup("OpenLoginWindow: 创建/显示异常 -> " + ex);
                throw;
            }
        }
    }
}
