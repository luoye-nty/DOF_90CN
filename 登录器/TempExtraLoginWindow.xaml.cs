// ============================================================================
// TEMP-8BOX 临时功能（临时测试用；2026-09-15 加四开，2026-09-24 扩到八开）
//
// ⚠ 当前状态：**启用中**（2026-09-25 按要求先关停、同日按要求重新启用）。LoginWindow.xaml.cs
//   里的总开关 `Temp8BoxEnabled` 返回 true：勾选「双开」会弹出 3 号 ~ 8 号登录框（共 8 开）。
//   要关掉：把那个开关的返回值改成 false，其余一行都不用动。
//
// 开发测试用的多开登录框：每个窗口负责一个额外槽位
// （GameProcessManager.ExtraSlot3 ~ ExtraSlot8）。
//
// 为什么做成独立窗口而不是在同一页再摆几个登录框：
//   主/副登录框是并排的 320 宽面板，再来几个会把主窗口挤爆，
//   而这个功能本来就是临时的，独立窗口删起来最干净（删文件 + 删带 TEMP-8BOX 标记的块）。
//
// 彻底删除步骤见 TempExtraLoginWindow.xaml 顶部注释。
// ============================================================================

using System;
using System.Windows;
using System.Windows.Input;
using System.Windows.Media;

namespace DNFLogin
{
    public partial class TempExtraLoginWindow : Window
    {
        private static readonly SolidColorBrush SuccessBrush = new(Color.FromRgb(0x2E, 0xCC, 0x71));
        private static readonly SolidColorBrush ErrorBrush = new(Color.FromRgb(0xFF, 0x70, 0x43));
        private static readonly SolidColorBrush DefaultBrush = new(Color.FromArgb(0x99, 0xFF, 0xFF, 0xFF));

        /// <summary>主窗口引用：登录与启动复用主窗口那条完整链路（TEMP-8BOX）</summary>
        private readonly LoginWindow _mainWindow;

        /// <summary>本窗口占用的游戏槽位（GameProcessManager.ExtraSlot3 ~ ExtraSlot8）</summary>
        private readonly int _slot;

        private bool _busy;
        private bool _launched;

        internal TempExtraLoginWindow(LoginWindow mainWindow, int slot, string title)
        {
            _mainWindow = mainWindow ?? throw new ArgumentNullException(nameof(mainWindow));
            _slot = slot;

            InitializeComponent();
            TitleText.Text = title;
            Title = title + "（临时多开）";
            StatusText.Foreground = DefaultBrush;
        }

        /// <summary>本窗口是否已经成功启动过游戏（游戏退出后由主窗口回调重置）</summary>
        internal bool Launched => _launched;

        /// <summary>游戏进程退出后由主窗口回调：把这一个登录框恢复成"可再次登录"</summary>
        internal void OnGameExited()
        {
            _launched = false;
            _busy = false;
            LoginButton.IsEnabled = true;
            LoginButton.Content = "登录并启动游戏";
            SetStatus("游戏已退出，可以再次登录。", true);
        }

        private async void LoginButton_Click(object sender, RoutedEventArgs e)
        {
            await DoLoginAsync();
        }

        private async void Input_KeyDown(object sender, KeyEventArgs e)
        {
            if (e.Key == Key.Enter)
            {
                await DoLoginAsync();
            }
        }

        private async System.Threading.Tasks.Task DoLoginAsync()
        {
            if (_busy)
            {
                return;
            }

            var account = AccountTextBox.Text?.Trim() ?? string.Empty;
            var password = PasswordBox.Password ?? string.Empty;
            if (string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(password))
            {
                SetStatus("账号或密码不能为空", false);
                return;
            }

            if (GameProcessManager.IsRunning(_slot))
            {
                SetStatus("该槽位已有游戏在运行，先退出游戏再登录。", false);
                return;
            }

            _busy = true;
            LoginButton.IsEnabled = false;
            LoginButton.Content = "登录中…";
            SetStatus("正在认证并启动游戏…", true);

            try
            {
                var (ok, error) = await _mainWindow.TempLoginAndLaunchAsync(account, password, _slot);
                if (!ok)
                {
                    SetStatus(string.IsNullOrWhiteSpace(error) ? "登录或启动失败" : error, false);
                    return;
                }

                _launched = true;
                PasswordBox.Password = string.Empty;
                LoginButton.Content = "已启动（可关闭本窗口）";
                SetStatus("已登录并启动游戏：" + account, true);
            }
            finally
            {
                _busy = false;
                LoginButton.IsEnabled = !_launched;
                if (!_launched)
                {
                    LoginButton.Content = "登录并启动游戏";
                }
            }
        }

        private void CloseButton_Click(object sender, RoutedEventArgs e)
        {
            Close();
        }

        private void SetStatus(string text, bool success)
        {
            StatusText.Text = text ?? string.Empty;
            StatusText.Foreground = success ? SuccessBrush : ErrorBrush;
        }
    }
}
