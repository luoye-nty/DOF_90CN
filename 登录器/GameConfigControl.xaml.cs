using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Text;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using DNFLogin.Services;

namespace DNFLogin
{
    /// <summary>
    /// 游戏配置页（原设置窗口：服务器配置 + 服务端启动 + 客户端目录 合并）
    /// </summary>
    public partial class GameConfigControl : UserControl
    {
        /// <summary>客户端 DNF.exe 路径配置变化后触发（供主窗口 PVF 自动更新自检等监听）</summary>
        public event Action? ClientPathChanged;

        public GameConfigControl()
        {
            InitializeComponent();

            // 数据库密码默认值（XAML 中设置 Password 会触发编译警告，故在此赋值）
            DbPasswordBox.Password = string.IsNullOrEmpty(AppConfig.Settings.Password)
                ? "aa123456"
                : AppConfig.Settings.Password;

            // 从持久化配置回填服务器配置六项
            DbModeBox.SelectedValue = AppConfig.Settings.Mode;
            DbHostBox.Text = AppConfig.Settings.Host;
            DbPortBox.Text = AppConfig.Settings.Port;
            DbNameBox.Text = AppConfig.Settings.Name;
            DbUserBox.Text = AppConfig.Settings.User;

            // 登录模式回填：按持久化配置选中本地/远程（90 远程模式已实现，直接还原磁盘配置）
            ModeLocalRadio.IsChecked = !string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);
            ModeRemoteRadio.IsChecked = string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);
            AdminTokenBox.Text = AppConfig.Settings.AdminToken;
            RemoteIpBox.Text = AppConfig.Settings.RemoteIp;
            ApplyModeUi();

            // 游戏路径回填
            DnfPathBox.Text = AppConfig.Settings.GamePath;

            // 服务端一键启动：回填路径，已有路径则自动读取配置
            ServerPathBox.Text = AppConfig.Settings.ServerRootPath;
            if (System.IO.Directory.Exists(AppConfig.Settings.ServerRootPath))
            {
                RefreshServerInfo();
            }
            UpdateServerStatus();

            // 客户端/服务端路径均已回填时，延迟自动检测客户端补丁
            ApplyVersionUi();
            ScheduleAutoCheckPatch();
            UpdateDnfPathHint();
#if EXTERNAL
            // 外网版（M3）：构造末尾收敛配置页，仅保留客户端目录卡
            ApplyExternalUi();
#endif
        }

        /// <summary>
        /// 客户端目录提示按版本显示对应主程序名：
        /// 90=S5A11DNF.exe；S4A21=S4A21 客户端；S4A12=S4A12 客户端。
        /// </summary>
        private void UpdateDnfPathHint()
        {
            var id = AppConfig.CurrentProfile?.Id ?? "90";
            DnfPathHint.Text = id switch
            {
                "S4A21" => "S4A21 客户端 exe 所在目录，登录时从这里启动游戏",
                "S4A12" => "S4A12 客户端 exe 所在目录，登录时从这里启动游戏",
                _ => "S5A11DNF.exe 所在目录，登录时从这里启动游戏"
            };
        }

#if EXTERNAL
        /// <summary>
        /// 外网版 UI 收敛（M3，仅 EXTERNAL 编译）：配置页仅保留"客户端目录"卡，
        /// 其余卡片（登录模式 + Admin Token、数据库连接、远程服务器配置、服务端根目录、
        /// 服务端配置、数据库模式切换、服务端运维、补丁检测）全部折叠。
        /// 登录模式由锁版本固定为远程，远程服务器参数最终来自加密文件 client.yh（M4 接线）。
        /// Free 版不编译本方法，行为零变化。
        /// </summary>
        private void ApplyExternalUi()
        {
            ModeCard.Visibility = Visibility.Collapsed;
            AdminTokenCard.Visibility = Visibility.Collapsed;
            DbCard.Visibility = Visibility.Collapsed;
            RemoteServerCard.Visibility = Visibility.Collapsed;
            ServerRootCard.Visibility = Visibility.Collapsed;
            ServerConfigCard.Visibility = Visibility.Collapsed;
            DbSwitchCard.Visibility = Visibility.Collapsed;
            ServerOpsCard.Visibility = Visibility.Collapsed;
            PatchPanel.Visibility = Visibility.Collapsed;
        }
#endif

        /// <summary>当前是否 S4 系列版本（A12/A21）</summary>
        private bool IsS4 => Services.ServerManager.IsS4Profile(AppConfig.CurrentProfile?.Id ?? "90");

        /// <summary>
        /// 按版本显示/隐藏配置项：S4 系列（A12/A21）隐藏 MySQL 连接卡、Admin Token、数据库模式切换；
        /// 90 版恢复完整配置。
        /// </summary>
        private void ApplyVersionUi()
        {
            if (IsS4)
            {
                AdminTokenCard.Visibility = Visibility.Collapsed;
                ServerInitButton.Visibility = Visibility.Collapsed;
                PatchPanel.Visibility = Visibility.Collapsed;
                ServerRootHintText.Text = "选择 A12 / A21 服务端根目录（包含 build.bat 的目录）";
            }
            else
            {
                AdminTokenCard.Visibility = Visibility.Visible;
                ServerInitButton.Visibility = Visibility.Visible;
                PatchPanel.Visibility = Visibility.Visible;
                ServerRootHintText.Text = "选择 DNF90 服务端根目录（包含 runtime 的目录），如 D:\\DNF90-server";
            }
            // 数据库连接卡 / 数据库模式切换卡 / 模式提示文本由 ApplyModeUi 按登录模式统一管理，
            // 必须在版本分支之后调用，避免被本方法强制覆盖（否则远程模式下数据库卡仍会显示）
            ApplyModeUi();
        }

        /// <summary>
        /// 版本切换后刷新：按当前版本（AppConfig.Settings 顶层已由 SwitchProfile 镜像）重新回填全部字段。
        /// 服务端路径/游戏路径/服务器配置均按版本隔离。
        /// </summary>
        public async Task RefreshFromProfileAsync()
        {
            // 服务器配置六项 + 令牌
            DbPasswordBox.Password = string.IsNullOrEmpty(AppConfig.Settings.Password)
                ? "aa123456"
                : AppConfig.Settings.Password;
            DbModeBox.SelectedValue = AppConfig.Settings.Mode;
            DbHostBox.Text = AppConfig.Settings.Host;
            DbPortBox.Text = AppConfig.Settings.Port;
            DbNameBox.Text = AppConfig.Settings.Name;
            DbUserBox.Text = AppConfig.Settings.User;
            AdminTokenBox.Text = AppConfig.Settings.AdminToken;

            // 登录模式：按持久化配置回填本地/远程

            ModeLocalRadio.IsChecked = !string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);
            ModeRemoteRadio.IsChecked = string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);
            AdminTokenBox.Text = AppConfig.Settings.AdminToken;
            RemoteIpBox.Text = AppConfig.Settings.RemoteIp;
            ApplyModeUi();

            // 游戏路径 + 服务端根目录
            DnfPathBox.Text = AppConfig.Settings.GamePath;
            ServerPathBox.Text = AppConfig.Settings.ServerRootPath;

            if (System.IO.Directory.Exists(AppConfig.Settings.ServerRootPath))
            {
                await RefreshServerInfoAsync();
            }
            await UpdateServerStatusAsync();
            ApplyVersionUi();
            ScheduleAutoCheckPatch();
            UpdateDnfPathHint();
#if EXTERNAL
            // 外网版（M3）：刷新末尾再次收敛（ApplyVersionUi/ApplyModeUi 可能恢复被折叠卡片）
            ApplyExternalUi();
#endif
        }

        /// <summary>版本切换专用：端口/进程探测全部移后台线程，避免阻塞 UI（同步版供用户主动操作场景保留）</summary>
        private async Task UpdateServerStatusAsync()
        {
            if (IsS4)
            {
                var s4Running = await Task.Run(() => Services.ServerManager.S4ServerRunningForRoot(AppConfig.Settings.ServerRootPath));
                var gm = await Task.Run(() => Services.ServerManager.IsPortOpen("127.0.0.1", Services.ServerManager.S4GmPort));
                ServerStatusText.Text = $"{(s4Running ? "DfoServer 运行中" : "DfoServer 停止")} · {(gm ? "Web GM 在线" : "Web GM 离线")}";
                return;
            }

            var db = await Task.Run(() => Services.ServerManager.MySqlRunning);
            var srv = await Task.Run(() => Services.ServerManager.ServerRunning);
            ServerStatusText.Text = $"{(db ? "MySQL 运行中" : "MySQL 停止")} · {(srv ? "服务端运行中" : "服务端停止")}";
        }

        /// <summary>版本切换专用：服务端配置探测移后台线程（S4 运行判定只探测一次，原同步版重复 3 次）</summary>
        private async Task RefreshServerInfoAsync()
        {
            var root = ServerPathBox.Text.Trim();

            if (IsS4)
            {
                var exe = await Task.Run(() => Services.ServerManager.LocateS4Server(root));
                var running = await Task.Run(() => Services.ServerManager.S4ServerRunningForRoot(root));
                if (string.IsNullOrEmpty(root) || !Directory.Exists(root))
                {
                    SrvDbMode.Text = SrvAdminListen.Text = SrvChannelListen.Text = SrvShardId.Text = SrvPvfPath.Text = SrvClientDir.Text = "-";
                    ServerConfigState.Text = "未选择目录";
                    ServerInitState.Text = "目录不存在";
                    ServerInitBadge.Background = new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0xE8, 0x4A, 0x3A));
                    return;
                }

                var pvf = Services.ServerManager.LocateS4Pvf(root);
                var dbOk = Services.ServerManager.S4DatabaseExists(root);
                SrvDbMode.Text = dbOk ? "SQLite（inventory.db）" : "SQLite（inventory.db 缺失）";
                SrvAdminListen.Text = exe is null ? "-" : "Web GM 5050";
                SrvChannelListen.Text = "7001";
                SrvShardId.Text = "-";
                SrvPvfPath.Text = pvf ?? "未找到（需从客户端复制）";
                SrvClientDir.Text = DnfPathBox.Text?.Trim() ?? "-";
                var ready = exe is not null && pvf is not null;
                ServerConfigState.Text = exe is null ? "未找到 DfoServer.exe" : !ready ? "缺少 PVF" : (running ? "服务端运行中" : "服务端已就绪");
                ServerInitState.Text = exe is null ? "缺少 DfoServer" : !ready ? "缺少 PVF" : (running ? "运行中" : "已就绪");
                ServerInitBadge.Background = !ready
                    ? new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0xE8, 0x4A, 0x3A))
                    : running
                        ? new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x2E, 0x8B, 0x57))
                        : new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x3A, 0x3B, 0x40));
                ServerInitButton.Content = "初始化服务端";
                ServerInitButton.Visibility = exe is null ? Visibility.Visible : Visibility.Collapsed;
                return;
            }

            var info = await Task.Run(() => Services.ServerManager.LoadInfo(root));
            if (info is null)
            {
                SrvDbMode.Text = SrvAdminListen.Text = SrvChannelListen.Text = SrvShardId.Text = SrvPvfPath.Text = SrvClientDir.Text = "-";
            }
            else
            {
                SrvDbMode.Text = info.DatabaseMode == "portable"
                    ? "MySQL（portable）"
                    : info.DatabaseMode == "sqlite" ? "SQLite" : info.DatabaseMode;
                SrvAdminListen.Text = info.AdminListen;
                SrvChannelListen.Text = info.ChannelListen;
                SrvShardId.Text = info.ShardId;
                SrvPvfPath.Text = info.PvfPath;
                SrvClientDir.Text = info.ClientDirectory;
            }

            var ins = await Task.Run(() => Services.ServerManager.InspectServer(root));
            ServerConfigState.Text = ins.Message;
            ServerInitState.Text = BuildInitStateText(ins);
            ServerInitBadge.Background = BuildInitBadgeBrush(ins);
            ServerInitButton.Visibility = ins.NeedsInit ? Visibility.Visible : Visibility.Collapsed;
        }

        /// <summary>
        /// 登录模式单选变化：本地/远程真正切换，同步到全局配置并保存
        /// </summary>
        private void ModeRadio_Checked(object sender, RoutedEventArgs e)
        {
            if (sender is not RadioButton rb)
            {
                return;
            }

            if (rb == ModeRemoteRadio)
            {
                AppConfig.Settings.LoginMode = "Remote";
                AppConfig.Mode = LoginMode.Remote;
            }
            else if (rb == ModeLocalRadio)
            {
                AppConfig.Settings.LoginMode = "Local";
                AppConfig.Mode = LoginMode.Local;
            }

            AppConfig.Save();
            ApplyModeUi();
        }

        /// <summary>按当前登录模式显示对应配置卡片（本地→数据库/服务端配置；远程→远程服务器配置，仅填 IP）</summary>
        private void ApplyModeUi()
        {
            var remote = string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);
            var nonLocal = remote;
            // 本地模式专属卡片：数据库连接 / 服务端根目录 / 服务器配置 / 数据库模式切换 / 启停与日志
            DbCard.Visibility = nonLocal ? Visibility.Collapsed : Visibility.Visible;
            ServerRootCard.Visibility = nonLocal ? Visibility.Collapsed : Visibility.Visible;
            ServerConfigCard.Visibility = nonLocal ? Visibility.Collapsed : Visibility.Visible;
            DbSwitchCard.Visibility = nonLocal ? Visibility.Collapsed : Visibility.Visible;
            ServerOpsCard.Visibility = nonLocal ? Visibility.Collapsed : Visibility.Visible;
            // 远程模式专属卡片：服务器 IP（90 走 LANA 认证直连；S4 走 7002 认证直连 7001 游戏服）
            RemoteServerCard.Visibility = nonLocal ? Visibility.Visible : Visibility.Collapsed;
            if (RemoteModeHintText is not null)
            {
                RemoteModeHintText.Text = "远程模式：填服务器公网 IP，保存后即可登录。";
            }

            // Admin Token：远程模式无需填写（远程认证由登录器内置完成），输入框置灰
            AdminTokenBox.IsEnabled = !nonLocal;
            AdminTokenHintText.Text = nonLocal
                ? "远程模式无需填写：账号认证由登录器内置完成（S4=7002 / 90=LANA）"
                : "本地模式：绑定账号时作为管理接口令牌（X-Admin-Token），留空则启动/登录前会提示填写";

            if (IsS4)
            {
                // S4 系列无 MySQL 概念（本地走 SQLite），数据库卡片与模式切换卡无论模式均隐藏
                DbCard.Visibility = Visibility.Collapsed;
                DbSwitchCard.Visibility = Visibility.Collapsed;
                // S4 精简：登录模式切换区只保留本地/远程选项，隐藏下方说明段落
                ModeHintText.Visibility = Visibility.Collapsed;
                AdminTokenHintText.Visibility = Visibility.Collapsed;
                return;
            }

            // 非 S4 恢复登录模式/令牌说明可见（S4 精简分支已隐藏）
            ModeHintText.Visibility = Visibility.Visible;
            AdminTokenHintText.Visibility = Visibility.Visible;
            ModeHintText.Text = remote
                ? "远程：认证后直连游戏服，账号由服务器认证"
                : "本地：直连 MySQL + 固定令牌，启动后按 PID 绑定账号";
        }

        /// <summary>
        /// 远程服务器 IP 输入变化：远程模式下实时持久化（主界面在线状态/启动参数读取同一配置，
        /// 避免"配置页测试通过但主界面仍离线"——测试连接直读输入框，主界面读持久化配置）。
        /// 本地模式不清空 RemoteIp，切回远程模式时保留上次填写的 IP。
        /// </summary>
        private void RemoteIpBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            if (!string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase))
            {
                return;
            }

            var ip = RemoteIpBox.Text?.Trim() ?? "";
            if (!string.Equals(AppConfig.Settings.RemoteIp, ip, StringComparison.Ordinal))
            {
                AppConfig.Settings.RemoteIp = ip;
                AppConfig.Save();
            }
        }

        /// <summary>
        /// 客户端目录输入变化：同步保存配置并调度补丁自动检测
        /// </summary>
        private void DnfPathBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            var path = DnfPathBox.Text?.Trim() ?? "";
            if (!string.Equals(AppConfig.Settings.GamePath, path, StringComparison.Ordinal))
            {
                PersistGamePath(path);
            }

            ScheduleAutoCheckPatch();

            // 通知主窗口：客户端路径已变化（PVF 自动更新自检等场景需要拿到目录后立即比对）
            ClientPathChanged?.Invoke();
        }

        /// <summary>
        /// 选择 DNF.exe 路径（选中即写入配置并保存）
        /// </summary>
        private void BrowseDnfButton_Click(object sender, RoutedEventArgs e)
        {
            var dlg = new Microsoft.Win32.OpenFileDialog
            {
                Title = "选择 DNF.exe",
                Filter = "DNF.exe|*.exe",
                CheckFileExists = true
            };

            if (dlg.ShowDialog() == true)
            {
                if (!string.Equals(System.IO.Path.GetFileName(dlg.FileName), "DNF.exe", System.StringComparison.OrdinalIgnoreCase))
                {
                    MessageBox.Show("只能选择 DNF.exe 文件。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }

                DnfPathBox.Text = dlg.FileName;
                PersistGamePath(dlg.FileName);
            }
        }

        /// <summary>
        /// 持久化客户端目录：必须同时写入顶层镜像字段与当前版本 Profile。
        /// 仅写顶层会被外网版启动 ApplyEditionLock（profile.CopyToTopLevel）用旧 profile 值覆盖、
        /// 或在版本切换镜像时丢弃，导致重启后回填为空、每次都要重新选择。
        /// 与 EmbeddedClientDetector 的写入模式保持一致（双写 + Save）。
        /// </summary>
        private void PersistGamePath(string path)
        {
            AppConfig.Settings.GamePath = path;
            var profile = AppConfig.CurrentProfile;
            if (profile is not null)
            {
                profile.GamePath = path;
            }

            AppConfig.Save();
        }

        /// <summary>
        /// 一键清空服务器配置输入框（S4 系列清空服务端根目录）
        /// </summary>
        private void ClearServerButton_Click(object sender, RoutedEventArgs e)
        {
            if (IsS4)
            {
                ServerPathBox.Text = "";
                AppConfig.Settings.ServerRootPath = "";
                AppConfig.Save();
                RefreshServerInfo();
                UpdateServerStatus();
                return;
            }

            DbModeBox.SelectedIndex = -1;
            DbHostBox.Text = "";
            DbPortBox.Text = "";
            DbNameBox.Text = "";
            DbUserBox.Text = "";
            DbPasswordBox.Password = "";
        }

        /// <summary>S4 远程探测：公网场景放宽超时到 3 秒（300ms 对公网 RTT 过短，易误判离线）</summary>
        private static bool ProbeS4RemotePort(string host, int port)
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

        /// <summary>
        /// 测试连接：90 版直连 MySQL / 远程探测认证 API（18112）；S4 系列（A12/A21）按版本探针口在线判定
        /// （A21→18888 / A12→16666，S4OpsTool 启动成功后开放），
        /// 本地探测本机频道口 7001 + DfoServer 进程路径匹配。
        /// </summary>
        private async void TestServerButton_Click(object sender, RoutedEventArgs e)
        {
            TestServerButton.IsEnabled = false;
            TestServerButton.Content = "测试中…";

            try
            {
                var remoteMode = string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase);

                if (remoteMode && IsS4)
                {
                    // S4 远程：版本探针口开放即测试通过（A21→18888 / A12→16666，S4OpsTool 启动成功后开放；
                    // 7002 认证在登录动作中校验）
                    var ip = RemoteIpBox.Text?.Trim() ?? "";
                    if (string.IsNullOrWhiteSpace(ip))
                    {
                        MessageBox.Show("请先填写服务器 IP。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    var profileId = AppConfig.CurrentProfile?.Id ?? "90";
                    var probePort = Services.ServerManager.S4ProbePortForProfile(profileId);
                    if (probePort <= 0)
                    {
                        probePort = Services.ServerManager.S4ChannelPort; // 兜底：识别不到版本回退探 7001
                    }
                    var channel = await Task.Run(() => ProbeS4RemotePort(ip, probePort));
                    var msg = channel
                        ? $"服务器在线：{ip}:{probePort} 版本探针口开放"
                        : $"无法连接服务器{ip}，请联系服务器管理员。";
                    MessageBox.Show(msg, "测试连接", MessageBoxButton.OK,
                        channel ? MessageBoxImage.Information : MessageBoxImage.Warning);
                    return;
                }

                // 90 远程测试：探测认证 API（18112）TCP 可达即视为在线
                if (remoteMode)
                {
                    var ip = RemoteIpBox.Text?.Trim() ?? "";
                    if (string.IsNullOrWhiteSpace(ip))
                    {
                        MessageBox.Show("请先填写服务器 IP。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }
                    var online = await Task.Run(() => Services.ServerManager.IsPortOpen(ip, 18112));
                    MessageBox.Show(online ? "认证服务在线，可以登录。" : "无法连接认证服务，请检查服务器 IP 与防火墙。",
                        "测试连接", MessageBoxButton.OK,
                        online ? MessageBoxImage.Information : MessageBoxImage.Warning);
                    return;
                }

                if (IsS4)
                {
                    await Task.Delay(50);
                    var channel = Services.ServerManager.S4ServerRunning;
                    var msg = (channel ? "频道端口 7001 在线，连接正常" : "频道端口 7001 离线，服务端未运行");
                    MessageBox.Show(msg, "测试连接", MessageBoxButton.OK,
                        channel ? MessageBoxImage.Information : MessageBoxImage.Warning);
                    return;
                }

                // —— 远程模式已禁用：原远程测试分支注释保留便于恢复 ——
                var settings = CollectServerSettings();
                if (settings is null)
                {
                    return;
                }

                if (!IsS4 && string.IsNullOrWhiteSpace(AdminTokenBox.Text?.Trim()))
                {
                    MessageBox.Show("Admin Token 不能为空，请先填写服务器管理接口令牌。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }

                var ok = await DatabaseService.TestConnectionAsync(settings);
                MessageBox.Show(ok ? "连接成功，服务器在线。" : "连接失败，服务器离线。",
                    "测试连接", MessageBoxButton.OK,
                    ok ? MessageBoxImage.Information : MessageBoxImage.Warning);
            }
            finally
            {
                TestServerButton.IsEnabled = true;
                TestServerButton.Content = "测试连接";
            }
        }

        /// <summary>
        /// 保存服务器配置到 JSON（同时更新内存配置，主界面在线状态会刷新；远程模式已禁用）
        /// </summary>
        private void SaveServerButton_Click(object sender, RoutedEventArgs e)
        {
            if (IsS4)
            {
                AppConfig.Settings.AdminToken = "";

                // S4 远程模式必须持久化服务器 IP：主界面在线状态/启动参数读 Settings.RemoteIp，
                // 若只留在输入框则切回主界面时读不到，导致探测回退本地 Host 显示离线。
                if (string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase))
                {
                    var ip = RemoteIpBox.Text?.Trim() ?? "";
                    if (string.IsNullOrWhiteSpace(ip))
                    {
                        MessageBox.Show("请填写远程服务器 IP。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    AppConfig.Settings.RemoteIp = ip;
                    AppConfig.Settings.RemoteServerUrl = "";
                }

                AppConfig.Save();
                MessageBox.Show("S4 系列使用 SQLite 服务端，无需数据库/令牌配置，已保存服务端目录。", "保存成功", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }

            // Admin Token 两种模式共用，先于分支保存
            AppConfig.Settings.AdminToken = AdminTokenBox.Text?.Trim() ?? "";

            // 90 远程保存：仅保存服务器公网 IP（认证/游戏连接仅需一个 IP）
            if (string.Equals(AppConfig.Settings.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase))
            {
                var ip = RemoteIpBox.Text?.Trim() ?? "";
                if (string.IsNullOrWhiteSpace(ip))
                {
                    MessageBox.Show("请填写远程服务器 IP。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }
                AppConfig.Settings.RemoteIp = ip;
                AppConfig.Settings.RemoteServerUrl = "";
                AppConfig.Save();
                MessageBox.Show("远程服务器配置已保存。", "保存成功", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }

            // 本地模式：保存数据库连接配置
            var settings = CollectServerSettings();
            if (settings is null)
            {
                return;
            }

            AppConfig.Settings.Mode = settings.Mode;
            AppConfig.Settings.Host = settings.Host;
            AppConfig.Settings.Port = settings.Port;
            AppConfig.Settings.Name = settings.Name;
            AppConfig.Settings.User = settings.User;
            AppConfig.Settings.Password = settings.Password;
            AppConfig.Settings.DatabasePath = settings.DatabasePath;

            AppConfig.Save();

            MessageBox.Show("服务器配置已保存。", "保存成功", MessageBoxButton.OK, MessageBoxImage.Information);
        }

        /// <summary>从配置页输入框收集服务器配置；端口非法时提示并返回 null</summary>
        private AppSettings? CollectServerSettings()
        {
            var port = DbPortBox.Text?.Trim() ?? "13306";
            if (!int.TryParse(port, out _))
            {
                MessageBox.Show("端口必须是数字。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return null;
            }

            return new AppSettings
            {
                Mode = DbModeBox.SelectedValue?.ToString() ?? "portable",
                Host = DbHostBox.Text?.Trim() ?? "127.0.0.1",
                Port = port,
                Name = DbNameBox.Text?.Trim() ?? "dnf_local",
                User = DbUserBox.Text?.Trim() ?? "root",
                Password = DbPasswordBox.Password ?? "",
                ServerRootPath = AppConfig.Settings.ServerRootPath,
                DatabasePath = AppConfig.Settings.DatabasePath
            };
        }

        // ==================== 服务端一键启动 ====================

        /// <summary>追加一行日志到服务端日志框</summary>
        private void AppendServerLog(string line)
        {
            ServerLogBox.AppendText($"[{DateTime.Now:HH:mm:ss}] {line}\r\n");
            ServerLogBox.ScrollToEnd();
        }

        /// <summary>刷新运行状态文本（90：MySQL/18111；S4：7001+进程路径匹配/5050）</summary>
        private void UpdateServerStatus()
        {
            if (IsS4)
            {
                var s4Running = Services.ServerManager.S4ServerRunningForRoot(AppConfig.Settings.ServerRootPath);
                var svr = s4Running ? "DfoServer 运行中" : "DfoServer 停止";
                var gm = Services.ServerManager.IsPortOpen("127.0.0.1", Services.ServerManager.S4GmPort) ? "Web GM 在线" : "Web GM 离线";
                ServerStatusText.Text = $"{svr} · {gm}";
                return;
            }

            var db = Services.ServerManager.MySqlRunning ? "MySQL 运行中" : "MySQL 停止";
            var srv = Services.ServerManager.ServerRunning ? "服务端运行中" : "服务端停止";
            ServerStatusText.Text = $"{db} · {srv}";
        }

        /// <summary>
        /// 将登录器当前配置（AppConfig.Settings）实时同步刷新到服务器配置输入框，
        /// 保证服务端启动栏位（如数据库模式切换）变更后配置页立即显示最新配置、测试连接使用最新配置。
        /// </summary>
        private void RefreshDbCardFromSettings()
        {
            DbModeBox.SelectedValue = AppConfig.Settings.Mode;
            DbHostBox.Text = AppConfig.Settings.Host;
            DbPortBox.Text = AppConfig.Settings.Port;
            DbNameBox.Text = AppConfig.Settings.Name;
            DbUserBox.Text = AppConfig.Settings.User;
            DbPasswordBox.Password = string.IsNullOrEmpty(AppConfig.Settings.Password)
                ? "aa123456"
                : AppConfig.Settings.Password;
            AdminTokenBox.Text = AppConfig.Settings.AdminToken;
        }

        /// <summary>读取服务端配置并展示到页面（含初始化状态探测；S4 系列走 DfoServer 探测）</summary>
        private void RefreshServerInfo()
        {
            var root = ServerPathBox.Text.Trim();

            if (IsS4)
            {
                var exe = Services.ServerManager.LocateS4Server(root);
                if (string.IsNullOrEmpty(root) || !Directory.Exists(root))
                {
                    SrvDbMode.Text = SrvAdminListen.Text = SrvChannelListen.Text = SrvShardId.Text = SrvPvfPath.Text = SrvClientDir.Text = "-";
                    ServerConfigState.Text = "未选择目录";
                    ServerInitState.Text = "目录不存在";
                    ServerInitBadge.Background = new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0xE8, 0x4A, 0x3A));
                    return;
                }

                var pvf = Services.ServerManager.LocateS4Pvf(root);
                var dbOk = Services.ServerManager.S4DatabaseExists(root);
                SrvDbMode.Text = dbOk ? "SQLite（inventory.db）" : "SQLite（inventory.db 缺失）";
                SrvAdminListen.Text = exe is null ? "-" : "Web GM 5050";
                SrvChannelListen.Text = "7001";
                SrvShardId.Text = "-";
                SrvPvfPath.Text = pvf ?? "未找到（需从客户端复制）";
                SrvClientDir.Text = DnfPathBox.Text?.Trim() ?? "-";
                var ready = exe is not null && pvf is not null;
                ServerConfigState.Text = exe is null ? "未找到 DfoServer.exe" : !ready ? "缺少 PVF" : (Services.ServerManager.S4ServerRunningForRoot(root) ? "服务端运行中" : "服务端已就绪");
                ServerInitState.Text = exe is null ? "缺少 DfoServer" : !ready ? "缺少 PVF" : (Services.ServerManager.S4ServerRunningForRoot(root) ? "运行中" : "已就绪");
                ServerInitBadge.Background = !ready
                    ? new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0xE8, 0x4A, 0x3A))
                    : Services.ServerManager.S4ServerRunningForRoot(root)
                        ? new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x2E, 0x8B, 0x57))
                        : new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x3A, 0x3B, 0x40));
                ServerInitButton.Content = "初始化服务端";
                ServerInitButton.Visibility = exe is null ? Visibility.Visible : Visibility.Collapsed;
                return;
            }

            var info = Services.ServerManager.LoadInfo(root);
            if (info is null)
            {
                SrvDbMode.Text = SrvAdminListen.Text = SrvChannelListen.Text = SrvShardId.Text = SrvPvfPath.Text = SrvClientDir.Text = "-";
            }
            else
            {
                SrvDbMode.Text = info.DatabaseMode == "portable"
                    ? "MySQL（portable）"
                    : info.DatabaseMode == "sqlite" ? "SQLite" : info.DatabaseMode;
                SrvAdminListen.Text = info.AdminListen;
                SrvChannelListen.Text = info.ChannelListen;
                SrvShardId.Text = info.ShardId;
                SrvPvfPath.Text = info.PvfPath;
                SrvClientDir.Text = info.ClientDirectory;
            }

            var ins = Services.ServerManager.InspectServer(root);
            ServerConfigState.Text = ins.Message;
            ServerInitState.Text = BuildInitStateText(ins);
            ServerInitBadge.Background = BuildInitBadgeBrush(ins);
            ServerInitButton.Visibility = ins.NeedsInit ? Visibility.Visible : Visibility.Collapsed;
        }

        /// <summary>初始化徽标文案：目录不存在/未初始化/配置异常/未就绪/运行中/已就绪</summary>
        private static string BuildInitStateText(Services.ServerInspection ins)
        {
            if (!ins.RootExists)
            {
                return "目录不存在";
            }

            if (!ins.InstanceExists)
            {
                return "未初始化";
            }

            if (!ins.InstanceValid)
            {
                return "配置异常";
            }

            if (!ins.Ready)
            {
                return "未就绪";
            }

            return ins.AdminPortOpen ? "运行中" : "已就绪";
        }

        /// <summary>服务端配置就绪后，联动刷新 GM 工具页连接信息（登录器与 GM 共用同一服务端）</summary>
        private void SyncGmFromLoginConfig()
        {
            if (Window.GetWindow(this) is LoginWindow win)
            {
                win.GmPage?.SyncFromLoginConfig();
            }
        }

        /// <summary>服务端启动就绪后：GM 工具自动连接数据库 + 后台预加载 PVF 索引（不阻塞启动流程）</summary>
        private void TriggerGmAutoConnectAndPreload()
        {
            if (Window.GetWindow(this) is LoginWindow win && win.GmPage is { } gm)
            {
                _ = gm.AutoConnectAsync();
                _ = gm.PreloadPvfAsync();
            }
        }

        /// <summary>初始化徽标背景色：异常红 / 运行绿 / 就绪灰</summary>
        private static System.Windows.Media.Brush BuildInitBadgeBrush(Services.ServerInspection ins)
        {
            if (!ins.RootExists || !ins.InstanceExists || !ins.InstanceValid || !ins.Ready)
            {
                return new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0xE8, 0x4A, 0x3A));
            }

            return ins.AdminPortOpen
                ? new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x2E, 0x8B, 0x57))
                : new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromRgb(0x3A, 0x3B, 0x40));
        }

        /// <summary>路径输入变化：同步保存配置并清空旧配置展示</summary>
        private void ServerPathBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            var root = ServerPathBox.Text.Trim();
            if (!string.Equals(AppConfig.Settings.ServerRootPath, root, System.StringComparison.Ordinal))
            {
                AppConfig.Settings.ServerRootPath = root;
                AppConfig.Save();
            }

            ServerConfigState.Text = "";
            ServerInitState.Text = "未检测";
            ServerInitButton.Visibility = Visibility.Collapsed;
            SrvDbMode.Text = SrvAdminListen.Text = SrvChannelListen.Text = SrvShardId.Text = SrvPvfPath.Text = SrvClientDir.Text = "-";

            ScheduleAutoCheckPatch();
        }

        /// <summary>选择服务端根目录</summary>
        private void ServerBrowse_Click(object sender, RoutedEventArgs e)
        {
            using var dlg = new System.Windows.Forms.FolderBrowserDialog
            {
                Description = IsS4
                    ? "选择 A12 / A21 服务端根目录（包含 build.bat 的目录）"
                    : "选择 DNF90 服务端根目录（包含 runtime 的目录）",
                SelectedPath = ServerPathBox.Text.Trim(),
                ShowNewFolderButton = false
            };

            if (dlg.ShowDialog() != System.Windows.Forms.DialogResult.OK)
            {
                return;
            }

            ServerPathBox.Text = dlg.SelectedPath;
            RefreshServerInfo();

            // S4：选择完目录立即探测，缺少 DfoServer.exe 则询问是否初始化（编译服务端）
            if (IsS4 && Services.ServerManager.LocateS4Server(dlg.SelectedPath) is null)
            {
                var ask = MessageBox.Show("该目录未找到服务端程序 DfoServer.exe，是否立即初始化（编译服务端）？", "初始化服务端", MessageBoxButton.YesNo, MessageBoxImage.Question);
                if (ask == MessageBoxResult.Yes)
                {
                    _ = RunS4InitFlowAsync();
                }
            }
        }

        /// <summary>读取配置按钮</summary>
        private void ServerRefresh_Click(object sender, RoutedEventArgs e)
        {
            RefreshServerInfo();
        }

        /// <summary>应用数据库模式（改写 instance.json；仅 90 版可用）</summary>
        private void ServerSwitchDb_Click(object sender, RoutedEventArgs e)
        {
            if (IsS4)
            {
                MessageBox.Show("S4 系列使用 SQLite 数据库，无需切换模式。", "提示", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }

            var root = ServerPathBox.Text.Trim();
            if (!System.IO.Directory.Exists(root))
            {
                MessageBox.Show("请先选择有效的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            string mode;
            if (DbModeMySql.IsChecked == true)
            {
                mode = "portable";
            }
            else if (DbModeSqlite.IsChecked == true)
            {
                mode = "sqlite";
            }
            else
            {
                AppendServerLog("当前为跟随配置文件，无需切换");
                return;
            }

            if (Services.ServerManager.ServerRunning)
            {
                var ret = MessageBox.Show("服务端正在运行，切换模式需要先停止服务端。是否先停止？", "提示", MessageBoxButton.YesNo, MessageBoxImage.Question);
                if (ret != MessageBoxResult.Yes)
                {
                    return;
                }

                SetServerBusy(true);
                Services.ServerManager.Stop(root, line => Dispatcher.Invoke(() => AppendServerLog(line)));
                UpdateServerStatus();
                SetServerBusy(false);
            }

            Services.ServerManager.SwitchDatabase(root, mode, AppendServerLog);
            // 切换数据库模式后立即同步 instance.json 到登录器配置，并实时刷新服务器配置输入框，
            // 保证"测试连接"使用最新配置而非旧配置
            Services.ServerManager.SyncFromInstance(root);
            RefreshDbCardFromSettings();
            RefreshServerInfo();
            // GM 工具同步连接信息（含数据库模式），确保切库后 GM 实时识别 MySQL/SQLite
            SyncGmFromLoginConfig();
        }

        /// <summary>一键初始化：S4 系列编译服务端（build）；90 版原初始化逻辑</summary>
        private void ServerInit_Click(object sender, RoutedEventArgs e)
        {
            if (IsS4)
            {
                _ = RunS4InitFlowAsync();
                return;
            }

            var root = ServerPathBox.Text.Trim();
            if (!System.IO.Directory.Exists(root))
            {
                MessageBox.Show("请先选择有效的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            var mode = DbModeMySql.IsChecked == true ? "portable" : DbModeSqlite.IsChecked == true ? "sqlite" : "portable";
            SetServerBusy(true);
            ServerLogBox.Clear();
            AppendServerLog("开始一键初始化 ...");
            _ = Task.Run(() =>
            {
                // 1. instance.json（干净包委托原版 DNF90Control database select --prepare 生成）
                if (!Services.ServerManager.EnsureInstance(root, mode, line => Dispatcher.Invoke(() => AppendServerLog(line))))
                {
                    Dispatcher.Invoke(() =>
                    {
                        AppendServerLog("一键初始化失败：无法生成 instance.json");
                        SetServerBusy(false);
                        RefreshServerInfo();
                    });
                    return;
                }

                var info = Services.ServerManager.LoadInfo(root);
                if (info is null)
                {
                    Dispatcher.Invoke(() =>
                    {
                        AppendServerLog("一键初始化失败：读取 instance.json 失败");
                        SetServerBusy(false);
                        RefreshServerInfo();
                    });
                    return;
                }

                // 2. MySQL 模式：不预初始化 MySQL（不建空库/不写 data-state=ready），
                //    完全交给原版 DNF90Control start 的 first-start 流程自动初始化，避免首启建表超时被杀
                if (string.Equals(info.DatabaseMode, "portable", StringComparison.OrdinalIgnoreCase))
                {
                    Dispatcher.Invoke(() =>
                    {
                        AppendServerLog("一键初始化完成（MySQL 数据将在首次启动服务端时自动初始化，预计 20-60 秒）");
                        SetServerBusy(false);
                        Services.ServerManager.SyncFromInstance(root);
                        RefreshDbCardFromSettings();
                        RefreshServerInfo();
                        SyncGmFromLoginConfig();
                    });
                    return;
                }

                Dispatcher.Invoke(() =>
                {
                    AppendServerLog("SQLite 模式初始化完成");
                    SetServerBusy(false);
                    Services.ServerManager.SyncFromInstance(root);
                    RefreshDbCardFromSettings();
                    RefreshServerInfo();
                    SyncGmFromLoginConfig();
                });
            });
        }

        /// <summary>启动服务端（后台线程执行，避免卡界面；按版本分流 90/S4）</summary>
        private void ServerStart_Click(object sender, RoutedEventArgs e)
        {
            var root = ServerPathBox.Text.Trim();
            if (!System.IO.Directory.Exists(root))
            {
                MessageBox.Show("请先选择有效的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            // S4 系列：完整启动流程（探测→缺程序编译→缺 SDK 装环境→PVF 自检复制→数据库提示→启动）
            if (IsS4)
            {
                SetServerBusy(true);
                ServerLogBox.Clear();
                _ = RunS4StartFlowAsync(root);
                return;
            }

            // 启动前验证：结构/instance/数据库就绪；硬伤则中止并展示问题
            var (ok, errors) = Services.ServerManager.ValidateServer(root, null);
            if (!ok)
            {
                ServerLogBox.Clear();
                AppendServerLog("启动前验证未通过，已中止启动：");
                foreach (var err in errors)
                {
                    AppendServerLog($"  - {err}");
                }

                AppendServerLog("可先点击“一键初始化”补齐环境后再启动。");
                UpdateServerStatus();
                return;
            }

            SetServerBusy(true);
            ServerLogBox.Clear();
            AppendServerLog("开始启动服务端 ...");
            _ = Task.Run(() =>
            {
                Services.ServerManager.Start(root, line => Dispatcher.Invoke(() => AppendServerLog(line)));
                Dispatcher.Invoke(() =>
                {
                    UpdateServerStatus();
                    // 启动成功后把 instance.json 的管理令牌/数据库配置同步到登录器，保证 X-Admin-Token 一致
                    if (Services.ServerManager.ServerRunning)
                    {
                        Services.ServerManager.SyncFromInstance(root);
                        AppendServerLog("已同步服务端配置（管理令牌/数据库）到登录器");
                        RefreshDbCardFromSettings();
                        SyncGmFromLoginConfig();
                        TriggerGmAutoConnectAndPreload();
                    }

                    SetServerBusy(false);
                    RefreshServerInfo();
                });
            });
        }

        /// <summary>
        /// S4 启动全流程：
        /// 探测 build.bat（根目录标志）→ 探测 DfoServer.exe → 缺失则询问编译 → 缺 .NET SDK 询问自动安装 →
        /// 编译后复探 → PVF 自检（缺失查客户端目录并询问复制）→ 数据库提示（缺失放行，服务端自动初始化）→ 启动。
        /// </summary>
        private async Task RunS4StartFlowAsync(string root)
        {
            try
            {
                // 1) 服务端根目录标志校验（build.bat）
                var buildScript = Services.ServerManager.LocateS4BuildScript(root);
                if (buildScript is null)
                {
                    AppendServerLog("警告：所选目录未找到 build.bat，可能不是 A12 / A21 服务端源码根目录。");
                    if (MessageBox.Show("所选目录未找到 build.bat，可能不是 A12 / A21 服务端根目录。仍要继续吗？", "提示", MessageBoxButton.YesNo, MessageBoxImage.Warning) != MessageBoxResult.Yes)
                    {
                        AppendServerLog("用户取消，已中止启动。");
                        return;
                    }
                }

                // 2) 探测 DfoServer.exe，缺失则询问编译
                var exe = Services.ServerManager.LocateS4Server(root);
                if (exe is null)
                {
                    if (buildScript is null)
                    {
                        AppendServerLog("未找到 DfoServer.exe 且目录中没有 build.bat，无法自动编译，已中止。");
                        MessageBox.Show("未找到 DfoServer.exe 且目录中没有 build.bat，无法自动编译。\n请确认选择了正确的 A12 / A21 服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    var askBuild = MessageBox.Show("服务端程序 DfoServer.exe 不存在，是否立即编译服务端？", "编译服务端", MessageBoxButton.YesNo, MessageBoxImage.Question);
                    if (askBuild != MessageBoxResult.Yes)
                    {
                        AppendServerLog("用户取消编译，已中止启动。");
                        return;
                    }

                    // 3) 编译环境检测：缺 .NET SDK 则询问自动安装
                    var sdk = await Services.DotNetSdkDetector.GetInstalledSdksAsync();
                    if (sdk.Length == 0)
                    {
                        var askInstall = MessageBox.Show("未检测到 .NET SDK（编译环境），是否自动安装编译环境？", "安装编译环境", MessageBoxButton.YesNo, MessageBoxImage.Question);
                        if (askInstall != MessageBoxResult.Yes)
                        {
                            AppendServerLog("用户取消安装编译环境，已中止启动。");
                            return;
                        }

                        var installed = await Task.Run(() => Services.ServerManager.InstallDotNetSdk(line => Dispatcher.Invoke(() => AppendServerLog(line))));
                        if (!installed)
                        {
                            AppendServerLog("编译环境安装失败，无法编译服务端，已中止。");
                            MessageBox.Show("编译环境安装失败，请手动安装 .NET 10 SDK 后重试。", "安装失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                            return;
                        }

                        AppendServerLog("编译环境就绪。");
                    }

                    // 4) 自动编译（等效 build.bat：dotnet build Server\DfoServer.sln -c Debug）
                    var buildOk = await Task.Run(() => Services.ServerManager.BuildS4Server(root, line => Dispatcher.Invoke(() => AppendServerLog(line))));
                    if (!buildOk)
                    {
                        AppendServerLog("编译未成功，已中止启动。");
                        MessageBox.Show("服务端编译失败，请查看上方日志。", "编译失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    // 5) 编译完成后再次探测
                    exe = Services.ServerManager.LocateS4Server(root);
                    if (exe is null)
                    {
                        AppendServerLog("编译完成但未定位到 DfoServer.exe，已中止。");
                        MessageBox.Show("编译完成，但未能定位 DfoServer.exe，请检查服务端结构。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    AppendServerLog($"已定位服务端程序：{exe}");
                }

                // 6) PVF 自检：缺失则查客户端目录并询问复制
                if (Services.ServerManager.LocateS4Pvf(root) is null)
                {
                    AppendServerLog("服务端缺少 Script.pvf（PVF 资源文件）。");
                    var clientRoot = GetClientRoot();
                    if (clientRoot is null || !IsClientRootValid(clientRoot))
                    {
                        AppendServerLog("客户端目录未设置或无效，无法获取 PVF。");
                        MessageBox.Show("服务端缺少 Script.pvf，且客户端目录未设置。\n请在“客户端目录”中选择 DNF.exe（Script.pvf 与 DNF.exe 在同一目录）后重试。", "缺少 PVF", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    var askCopy = MessageBox.Show("服务端缺少 Script.pvf，是否从客户端目录复制到服务端？", "复制 PVF", MessageBoxButton.YesNo, MessageBoxImage.Question);
                    if (askCopy != MessageBoxResult.Yes)
                    {
                        AppendServerLog("用户取消复制 PVF，已中止启动。");
                        return;
                    }

                    var copied = await Task.Run(() => Services.ServerManager.CopyPvfToServer(clientRoot, root, line => Dispatcher.Invoke(() => AppendServerLog(line))));
                    if (!copied || Services.ServerManager.LocateS4Pvf(root) is null)
                    {
                        AppendServerLog("PVF 复制后仍未就绪，已中止启动。");
                        MessageBox.Show("PVF 复制失败，服务端仍缺少 Script.pvf，无法启动。", "缺少 PVF", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }
                }

                // 7) 数据库自检：缺失仅提示并放行（A12 编译自带 inventory.db；A21 首次启动自动初始化）
                if (!Services.ServerManager.S4DatabaseExists(root))
                {
                    AppendServerLog("提示：数据库 inventory.db 不存在，服务端首次启动将自动初始化数据库。");
                }

                // 8) 启动 DfoServer
                AppendServerLog("开始启动 DfoServer ...");
                await Task.Run(() => Services.ServerManager.StartS4(root, line => Dispatcher.Invoke(() => AppendServerLog(line))));
                Dispatcher.Invoke(() =>
                {
                    UpdateServerStatus();
                    RefreshServerInfo();
                });
            }
            finally
            {
                Dispatcher.Invoke(() => SetServerBusy(false));
            }
        }

        /// <summary>
        /// S4 初始化（编译服务端）：校验 build.bat → .NET SDK 检测（缺失询问自动安装）→ build → 刷新状态。
        /// </summary>
        private async Task RunS4InitFlowAsync()
        {
            if (!IsS4)
            {
                return;
            }

            var root = ServerPathBox.Text.Trim();
            if (!System.IO.Directory.Exists(root))
            {
                MessageBox.Show("请先选择有效的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            if (Services.ServerManager.LocateS4BuildScript(root) is null)
            {
                MessageBox.Show("所选目录未找到 build.bat，无法初始化（编译）服务端。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            SetServerBusy(true);
            ServerLogBox.Clear();
            try
            {
                AppendServerLog("开始初始化（编译）服务端 ...");

                // .NET SDK 检测，缺失询问自动安装
                var sdk = await Services.DotNetSdkDetector.GetInstalledSdksAsync();
                if (sdk.Length == 0)
                {
                    var askInstall = MessageBox.Show("未检测到 .NET SDK（编译环境），是否自动安装编译环境？", "安装编译环境", MessageBoxButton.YesNo, MessageBoxImage.Question);
                    if (askInstall != MessageBoxResult.Yes)
                    {
                        AppendServerLog("用户取消安装编译环境，已中止。");
                        return;
                    }

                    var installed = await Task.Run(() => Services.ServerManager.InstallDotNetSdk(line => Dispatcher.Invoke(() => AppendServerLog(line))));
                    if (!installed)
                    {
                        AppendServerLog("编译环境安装失败，无法编译服务端。");
                        MessageBox.Show("编译环境安装失败，请手动安装 .NET 10 SDK 后重试。", "安装失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                        return;
                    }

                    AppendServerLog("编译环境就绪。");
                }

                // build
                var ok = await Task.Run(() => Services.ServerManager.BuildS4Server(root, line => Dispatcher.Invoke(() => AppendServerLog(line))));
                if (!ok)
                {
                    AppendServerLog("初始化（编译）未成功。");
                    MessageBox.Show("服务端编译失败，请查看上方日志。", "编译失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }

                var exe = Services.ServerManager.LocateS4Server(root);
                AppendServerLog(exe is null
                    ? "初始化完成，但未定位到 DfoServer.exe，请检查服务端结构。"
                    : $"初始化完成，已定位服务端程序：{exe}");
                Dispatcher.Invoke(RefreshServerInfo);
            }
            finally
            {
                Dispatcher.Invoke(() => SetServerBusy(false));
            }
        }

        /// <summary>停止服务端（后台线程执行，避免卡界面；按版本分流 90/S4）</summary>
        private void ServerStop_Click(object sender, RoutedEventArgs e)
        {
            var root = ServerPathBox.Text.Trim();
            if (!System.IO.Directory.Exists(root))
            {
                MessageBox.Show("请先选择有效的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            if (IsS4)
            {
                SetServerBusy(true);
                AppendServerLog("停止 DfoServer ...");
                _ = Task.Run(() =>
                {
                    Services.ServerManager.StopS4(root, line => Dispatcher.Invoke(() => AppendServerLog(line)));
                    Dispatcher.Invoke(() =>
                    {
                        UpdateServerStatus();
                        SetServerBusy(false);
                        RefreshServerInfo();
                    });
                });
                return;
            }

            SetServerBusy(true);
            AppendServerLog("停止服务端 ...");
            _ = Task.Run(() =>
            {
                Services.ServerManager.Stop(root, line => Dispatcher.Invoke(() => AppendServerLog(line)));
                Dispatcher.Invoke(() =>
                {
                    UpdateServerStatus();
                    SetServerBusy(false);
                    RefreshServerInfo();
                });
            });
        }

        /// <summary>服务端操作进行中禁用相关按钮</summary>
        private void SetServerBusy(bool busy)
        {
            ServerStartButton.IsEnabled = !busy;
            ServerStopButton.IsEnabled = !busy;
            ServerRefreshButton.IsEnabled = !busy;
            ServerSwitchDbButton.IsEnabled = !busy;
            ServerInitButton.IsEnabled = !busy;
        }

        // ==================== 客户端补丁检测与安装 ====================

        /// <summary>已安装判定清单（client-patch\bin + client-lua\bin 的全部文件，不含 DNF.exe / Script.pvf / ImagePacks2）</summary>
        private static readonly string[] PatchFiles =
        {
            "90CN.dll",
            "ijl15.dll",
            "ijl15_real.dll",
            "90CNLua.dll",
            @"lua\init.lua",
            @"lua\ai\init.lua"
        };

        private DateTime _lastPatchPathChange = DateTime.MinValue;
        private bool _patchPrompting;
        private bool _installing;

        /// <summary>从 DnfPathBox 推导客户端根目录（支持 DNF.exe 完整路径或目录）</summary>
        private string? GetClientRoot()
        {
            var p = DnfPathBox.Text?.Trim() ?? "";
            if (string.IsNullOrEmpty(p))
            {
                return null;
            }

            if (Directory.Exists(p))
            {
                return p;
            }

            var dir = Path.GetDirectoryName(p);
            return string.IsNullOrEmpty(dir) ? null : dir;
        }

        /// <summary>客户端目录是否有效（存在 DNF.exe 或 NoPack.exe）</summary>
        private static bool IsClientRootValid(string root)
            => File.Exists(Path.Combine(root, "DNF.exe")) || File.Exists(Path.Combine(root, "NoPack.exe"));

        /// <summary>定位服务端根目录下的补丁安装脚本（位置固定）</summary>
        private string? GetPatchScriptPath()
        {
            var root = ServerPathBox.Text?.Trim() ?? "";
            if (string.IsNullOrEmpty(root))
            {
                return null;
            }

            var script = Path.Combine(root, "安装客户端补丁.bat");
            return File.Exists(script) ? script : null;
        }

        /// <summary>返回客户端缺失的补丁文件相对路径列表（空 = 已完整安装）</summary>
        private List<string> FindMissingPatchFiles(string clientRoot)
        {
            var missing = new List<string>();
            foreach (var f in PatchFiles)
            {
                if (!File.Exists(Path.Combine(clientRoot, f)))
                {
                    missing.Add(f);
                }
            }

            return missing;
        }

        /// <summary>路径变化后防抖调度自动检测（600ms 内无新变化才执行）</summary>
        private async void ScheduleAutoCheckPatch()
        {
#if EXTERNAL
            // 外网版（M3）：补丁面板已收敛折叠，禁止自动检测与弹窗询问（补丁安装属本地服务端部署概念）
            return;
#else
            if (IsS4)
            {
                return;
            }

            var stamp = DateTime.UtcNow;
            _lastPatchPathChange = stamp;
            await Task.Delay(600);
            if (stamp != _lastPatchPathChange)
            {
                return;
            }

            AutoCheckPatch();
#endif
        }

        /// <summary>自动检测：客户端 + 服务端路径均有效时检查补丁，缺失则弹窗询问安装</summary>
        private void AutoCheckPatch()
        {
#if EXTERNAL
            // 外网版（M3）：双保险——任何入口都不执行补丁检测/安装弹窗
            return;
#else
            var clientRoot = GetClientRoot();
            if (clientRoot == null || !IsClientRootValid(clientRoot))
            {
                return;
            }

            if (GetPatchScriptPath() == null)
            {
                return;
            }

            CheckPatch();
#endif
        }

        /// <summary>执行补丁检测并更新 UI；手动/自动共用</summary>
        private void CheckPatch()
        {
            if (IsS4)
            {
                return;
            }

            if (_installing)
            {
                return;
            }

            var clientRoot = GetClientRoot();
            if (clientRoot == null || !IsClientRootValid(clientRoot))
            {
                PatchStateText.Text = "客户端补丁：未检测";
                return;
            }

            if (GetPatchScriptPath() == null)
            {
                PatchStateText.Text = "客户端补丁：未找到安装脚本";
                InstallPatchButton.IsEnabled = false;
                return;
            }

            var missing = FindMissingPatchFiles(clientRoot);
            if (missing.Count == 0)
            {
                PatchStateText.Text = "客户端补丁：已安装";
                InstallPatchButton.Content = "已安装";
                InstallPatchButton.IsEnabled = false;
                return;
            }

            PatchStateText.Text = $"客户端补丁：缺失 {missing.Count} 项";
            InstallPatchButton.Content = "安装补丁";
            InstallPatchButton.IsEnabled = true;
            AskInstallPatch();
        }

        /// <summary>弹窗询问是否安装补丁（登录器风格深色确认框）</summary>
        private void AskInstallPatch()
        {
            if (_patchPrompting)
            {
                return;
            }

            _patchPrompting = true;
            try
            {
                var dlg = new ConfirmDialog("安装客户端补丁", "检测到客户端缺少补丁文件，是否立即安装？")
                {
                    Owner = Window.GetWindow(this)
                };
                if (dlg.ShowDialog() == true)
                {
                    _ = InstallPatchAsync();
                }
            }
            finally
            {
                _patchPrompting = false;
            }
        }

        /// <summary>后台静默执行安装脚本（自动选 MySQL、喂掉 pause），完成后按退出码更新 UI</summary>
        private async Task InstallPatchAsync()
        {
            if (_installing)
            {
                return;
            }

            var clientRoot = GetClientRoot();
            var script = GetPatchScriptPath();
            if (clientRoot == null || script == null)
            {
                return;
            }

            _installing = true;
            PatchStateText.Text = "客户端补丁：安装中…";
            InstallPatchButton.IsEnabled = false;
            CheckPatchButton.IsEnabled = false;

            try
            {
                var psi = new ProcessStartInfo
                {
                    FileName = "cmd.exe",
                    Arguments = $"/c \"\"{script}\" \"{clientRoot}\"\"",
                    UseShellExecute = false,
                    CreateNoWindow = true,
                    RedirectStandardInput = true,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true,
                    StandardOutputEncoding = new UTF8Encoding(false),
                    StandardErrorEncoding = new UTF8Encoding(false)
                };

                using var proc = Process.Start(psi);
                if (proc is null)
                {
                    PatchStateText.Text = "客户端补丁：安装失败（无法启动脚本）";
                    return;
                }

                // 自动选择数据库菜单 [1] MySQL；随后关闭 stdin 使脚本内 pause 立即通过
                proc.StandardInput.WriteLine("1");
                proc.StandardInput.Close();

                var outputTask = proc.StandardOutput.ReadToEndAsync();
                var errorTask = proc.StandardError.ReadToEndAsync();
                var exitTask = proc.WaitForExitAsync();
                var done = await Task.WhenAny(exitTask, Task.Delay(TimeSpan.FromSeconds(180)));
                if (done != exitTask)
                {
                    try
                    {
                        proc.Kill(true);
                    }
                    catch
                    {
                        // 进程已退出
                    }

                    PatchStateText.Text = "客户端补丁：安装超时";
                    InstallPatchButton.Content = "重试安装";
                    InstallPatchButton.IsEnabled = true;
                    return;
                }

                await outputTask;
                await errorTask;
                var code = proc.ExitCode;
                if (code == 0)
                {
                    PatchStateText.Text = "客户端补丁：已安装";
                    InstallPatchButton.Content = "已安装";
                    InstallPatchButton.IsEnabled = false;
                }
                else
                {
                    PatchStateText.Text = $"客户端补丁：安装失败（{DescribePatchError(code)}）";
                    InstallPatchButton.Content = "重试安装";
                    InstallPatchButton.IsEnabled = true;
                }
            }
            catch (Exception)
            {
                PatchStateText.Text = "客户端补丁：安装失败，请检查补丁脚本与文件占用情况后重试";
                InstallPatchButton.Content = "重试安装";
                InstallPatchButton.IsEnabled = true;
            }
            finally
            {
                _installing = false;
                CheckPatchButton.IsEnabled = true;
            }
        }

        /// <summary>安装脚本退出码 → 中文说明</summary>
        private static string DescribePatchError(int code)
        {
            return code switch
            {
                11 => "客户端目录未找到 DNF.exe/NoPack.exe",
                12 => "游戏正在运行，请先退出",
                13 or 14 or 15 or 19 or 21 or 22 or 23 or 24 or 25 => "服务端发布包缺少补丁文件",
                20 => "无法保存客户端目录配置",
                26 or 27 => "数据库配置失败",
                _ => $"错误码 {code}"
            };
        }

        /// <summary>手动检测补丁按钮</summary>
        private void CheckPatchButton_Click(object sender, RoutedEventArgs e)
        {
            var clientRoot = GetClientRoot();
            if (clientRoot == null || !IsClientRootValid(clientRoot))
            {
                MessageBox.Show("请先选择有效的客户端目录（包含 DNF.exe）。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            if (GetPatchScriptPath() == null)
            {
                MessageBox.Show("未找到安装脚本，请先选择包含“安装客户端补丁.bat”的服务端根目录。", "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
                return;
            }

            CheckPatch();
        }

        /// <summary>手动安装补丁按钮</summary>
        private void InstallPatchButton_Click(object sender, RoutedEventArgs e)
        {
            if (IsS4)
            {
                return;
            }

            AskInstallPatch();
        }
    }
}
