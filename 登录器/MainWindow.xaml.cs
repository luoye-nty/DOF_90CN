using AduSkin.Controls.Metro;
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.IO;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Media.Animation;
using System.Windows.Media.Imaging;

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
        private static readonly Uri DefaultBackgroundUri = new("pack://application:,,,/Assets/bg-default-Bg1.jpg", UriKind.Absolute);
        private static readonly Uri DefaultIconUri = new("pack://application:,,,/Assets/login-icon.ico", UriKind.Absolute);

        private ImageBrush? _backgroundBrush;
        private bool _hasShown;
        private bool _loginWindowOpened;

        /// <summary>最近一次启动自检结果，供 LoginWindow 读取后自动跳转设置页</summary>
        public static StartupCheckResult? LastCheckResult { get; set; }

        public MainWindow()
        {
            InitializeComponent();
            Title = "";
            SetDefaultBackground();
        }

        private void Window_Loaded(object sender, RoutedEventArgs e)
        {
            _ = RunDemoFlowAsync();
        }

        protected override void OnContentRendered(EventArgs e)
        {
            base.OnContentRendered(e);

            if (_hasShown)
            {
                return;
            }

            _hasShown = true;
            FadeInWindow();
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

        private ImageBrush? TryGetBackgroundBrush()
        {
            if (_backgroundBrush is not null)
            {
                return _backgroundBrush;
            }

            _backgroundBrush = FindName("BackgroundImageBrush") as ImageBrush;
            return _backgroundBrush;
        }

        private void SetDefaultBackground()
        {
            if (TryGetBackgroundBrush() is not ImageBrush brush)
            {
                return;
            }

            var bitmap = new BitmapImage();
            bitmap.BeginInit();
            bitmap.UriSource = DefaultBackgroundUri;
            bitmap.CacheOption = BitmapCacheOption.OnLoad;
            bitmap.EndInit();
            bitmap.Freeze();

            brush.ImageSource = bitmap;
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
            //      不符 → 弹强制更新窗口（下载 → 解压 → 删原登录器 → 自动打开新版本），
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
                var gate = await Services.LauncherVersionGuard.RunSelfCheckAsync(this).ConfigureAwait(true);
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
            //    与登录器内嵌 90CN.dll 比对 SHA256，客户端缺失时询问安装、不一致时询问替换（不备份）；
            //    用户取消或写入失败均记录到 StepDetail 后继续，不阻塞进入登录界面。
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
                    string? detail90 = null;
                    if (string.IsNullOrEmpty(clientHash90))
                    {
                        var installDlg = new ConfirmDialog("安装 90CN.dll", "未检测到客户端 90CN.dll，是否安装？")
                        {
                            Owner = this
                        };
                        if (installDlg.ShowDialog() == true)
                        {
                            var installErr = Services.Cn90DllHelper.DeployTo(clientRoot90);
                            detail90 = installErr is null ? "90CN.dll 已安装到客户端目录" : installErr;
                        }
                    }
                    else if (!string.Equals(clientHash90, embeddedHash90, StringComparison.OrdinalIgnoreCase))
                    {
                        var replaceDlg = new ConfirmDialog("替换 90CN.dll", "90CN.dll 与登录器内置版本不一致，是否替换？")
                        {
                            Owner = this
                        };
                        if (replaceDlg.ShowDialog() == true)
                        {
                            var replaceErr = Services.Cn90DllHelper.DeployTo(clientRoot90);
                            detail90 = replaceErr is null ? "90CN.dll 已替换为登录器内置版本" : replaceErr;
                        }
                    }

                    if (!string.IsNullOrEmpty(detail90))
                    {
                        ReportProgress("90CN.dll 检查", detail90, 100, 93);
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
            await Task.Delay(400).ConfigureAwait(true);

            LastCheckResult = result;
            App.LogStartup("RunDemoFlowAsync: 自检完成，准备打开 LoginWindow（MissingResources=" + result.MissingResources.Count + "）");
            OpenLoginWindow();
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
            var clampedStep = Math.Clamp(stepPercent, 0, 100);
            var clampedGlobal = Math.Clamp(globalPercent, 0, 100);

            StepTitleText.Text = title;
            StepDetailText.Text = detail;
            StepProgressBar.Value = clampedStep;
            StepPercentText.Text = $"{Math.Floor(clampedStep)}%";
            TotalProgressBar.Value = clampedGlobal;
            TotalPercentText.Text = $"{Math.Floor(clampedGlobal)}%";
        }

        private void OpenLoginWindow()
        {
            if (_loginWindowOpened)
            {
                return;
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
            }
            catch (Exception ex)
            {
                App.LogStartup("OpenLoginWindow: 创建/显示异常 -> " + ex);
                throw;
            }

            Close();
            App.LogStartup("OpenLoginWindow: 旧 MainWindow 已关闭");
        }
    }
}
