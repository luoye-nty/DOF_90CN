using System;
using System.IO;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;

namespace DNFLogin
{
    /// <summary>
    /// 自检服务端状态弹窗：同时展示 90 / S4A21 / S4A12 三个版本的服务端状态。
    /// 未选择服务端目录的版本显示"未选择服务端目录"；已配置但未运行的版本提供"启动"按钮一键开启。
    /// </summary>
    public partial class StartServerDialog : Window
    {
        private static readonly Brush StatusGreen = new SolidColorBrush(Color.FromRgb(0x4C, 0xAF, 0x50));
        private static readonly Brush StatusRed = new SolidColorBrush(Color.FromRgb(0xE5, 0x73, 0x73));
        private static readonly Brush StatusGray = new SolidColorBrush(Color.FromRgb(0x9E, 0x9E, 0x9E));
        private static readonly Brush StatusYellow = new SolidColorBrush(Color.FromRgb(0xFF, 0xB7, 0x4D));

        public StartServerDialog()
        {
            InitializeComponent();
            Loaded += async (_, _) => await RefreshAllAsync();
        }

        /// <summary>逐版本刷新状态：未选路径→灰字提示；已选路径→按运行状态显示并决定是否显示"启动"按钮</summary>
        private async Task RefreshAllAsync()
        {
            await RefreshRowAsync("90", Name90, Status90, Start90Button);
            await RefreshRowAsync("S4A21", NameA21, StatusA21, StartA21Button);
            await RefreshRowAsync("S4A12", NameA12, StatusA12, StartA12Button);
        }

        private async Task RefreshRowAsync(string profileId, TextBlock nameTb, TextBlock statusTb, Button startBtn)
        {
            var profile = AppConfig.Settings.Profiles.Find(p => p.Id == profileId);
            var root = profile?.ServerRootPath?.Trim() ?? "";
            startBtn.Visibility = Visibility.Collapsed;
            startBtn.IsEnabled = true;

            if (string.IsNullOrWhiteSpace(root) || !Directory.Exists(root))
            {
                statusTb.Text = "未选择服务端目录";
                statusTb.Foreground = StatusGray;
                return;
            }

            var running = await Task.Run(() => Services.ServerManager.ServerRunningForProfile(profileId, root));
            if (running)
            {
                statusTb.Text = "运行中";
                statusTb.Foreground = StatusGreen;
                return;
            }

            statusTb.Text = "未启动";
            statusTb.Foreground = StatusRed;
            startBtn.Visibility = Visibility.Visible;
        }

        private async void Start90Button_Click(object sender, RoutedEventArgs e)
            => await StartVersionAsync("90", Start90Button, Status90);

        private async void StartA21Button_Click(object sender, RoutedEventArgs e)
            => await StartVersionAsync("S4A21", StartA21Button, StatusA21);

        private async void StartA12Button_Click(object sender, RoutedEventArgs e)
            => await StartVersionAsync("S4A12", StartA12Button, StatusA12);

        /// <summary>后台启动指定版本服务端（90=MySQL/SQLite 切换后启动；S4=DfoServer 启动），完成后回查状态</summary>
        private async Task StartVersionAsync(string profileId, Button btn, TextBlock statusTb)
        {
            var profile = AppConfig.Settings.Profiles.Find(p => p.Id == profileId);
            var root = profile?.ServerRootPath?.Trim() ?? "";

            btn.IsEnabled = false;
            btn.Visibility = Visibility.Visible;
            statusTb.Text = "启动中...";
            statusTb.Foreground = StatusYellow;

            var lastLog = string.Empty;
            try
            {
                await Task.Run(() =>
                {
                    if (Services.ServerManager.IsS4Profile(profileId))
                    {
                        Services.ServerManager.StartS4(root, msg => lastLog = msg);
                    }
                    else
                    {
                        var mode = string.IsNullOrWhiteSpace(profile?.Mode) ? "portable" : profile.Mode;
                        Services.ServerManager.SwitchDatabase(root, mode, msg => lastLog = msg);
                        Services.ServerManager.Start(root, msg => lastLog = msg);
                    }
                });

                // 给服务端留出启动时间后回查
                await Task.Delay(2000);
                var running = await Task.Run(() => Services.ServerManager.ServerRunningForProfile(profileId, root));
                if (running)
                {
                    statusTb.Text = "运行中";
                    statusTb.Foreground = StatusGreen;
                    btn.Visibility = Visibility.Collapsed;
                    return;
                }

                statusTb.Text = string.IsNullOrWhiteSpace(lastLog) ? "启动失败" : lastLog;
                statusTb.Foreground = StatusRed;
            }
            catch (Exception)
            {
                statusTb.Text = "启动失败，请检查服务端配置与端口占用情况后重试";
                statusTb.Foreground = StatusRed;
            }

            btn.IsEnabled = true;
        }

        private void CloseButton_Click(object sender, RoutedEventArgs e)
        {
            DialogResult = true;
        }
    }
}
