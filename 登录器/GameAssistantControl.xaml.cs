using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.Linq;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Threading;
using DNFLogin.Services;

namespace DNFLogin
{
    /// <summary>
    /// 游戏助手页（DPS 开关 + 按键连发配置）
    /// </summary>
    public partial class GameAssistantControl : UserControl
    {
        /// <summary>
        /// 连发按键配置列表（与界面绑定，关闭时写回 JSON）
        /// </summary>
        private readonly ObservableCollection<AutoFireKeyConfig> _autoFireKeys = new();

        /// <summary>
        /// 正在等待录入的连发按键行（非 null 表示处于录入模式）
        /// </summary>
        private AutoFireKeyConfig? _capturingRow;

        /// <summary>
        /// 连发间隔输入防抖计时器：用户连续输入时避免频繁重启引擎
        /// </summary>
        private readonly DispatcherTimer _autoFireApplyDebounce = new() { Interval = TimeSpan.FromMilliseconds(400) };

        /// <summary>
        /// 版本门控提示文本（非 90 版本插入的置灰说明），切换版本时先移除再重建
        /// </summary>
        private TextBlock? _versionGateTip;

        public GameAssistantControl()
        {
            InitializeComponent();

            // ASSIST-OFF-EXTERNAL（2026-09-20）：外网版整页停用游戏助手（该功能当前有问题，待修）。
            // 页面本身已被 LoginWindow.ApplyExternalUiLock 折叠；这里直接跳过全部初始化 ——
            // 不回填开关、不写共享内存、不启动连发引擎 —— 因为配置（%APPDATA%\DNFLogin\config.json）
            // 是免费版/外网版共享的，玩家在免费版开过的开关会残留，只隐藏 UI 挡不住它生效。
            // 恢复：删掉这个 #if 块（连同 LoginWindow 里搜 ASSIST-OFF-EXTERNAL 的三处闸门）。
#if EXTERNAL
            return;
#else
            // 按键连发：数据源 + 从配置回填 + 开关状态
            AutoFireList.ItemsSource = _autoFireKeys;
            foreach (var k in AppConfig.Settings.AutoFireKeys)
            {
                _autoFireKeys.Add(new AutoFireKeyConfig { Key = k.Key, IntervalMs = k.IntervalMs });
            }

            ToggleAutoFire.IsChecked = AppConfig.Settings.AutoFireEnabled;
            AutoFireConfigArea.Visibility = AppConfig.Settings.AutoFireEnabled ? Visibility.Visible : Visibility.Collapsed;

            // DPS 开关回填
            ToggleDps.IsChecked = AppConfig.Settings.DpsEnabled;

            // 顺图/拾取开关回填：同时把状态写入共享内存（若尚未注入，注入时以共享内存为准）
            ToggleHelperPass.IsChecked = AppConfig.Settings.HelperPassEnabled;
            ToggleHelperPickup.IsChecked = AppConfig.Settings.HelperPickupEnabled;
            Services.HelperSharedMemory.SetPassEnabled(AppConfig.Settings.HelperPassEnabled);
            Services.HelperSharedMemory.SetPickupEnabled(AppConfig.Settings.HelperPickupEnabled);

            // 录入模式需要捕获窗口级键盘事件：挂到宿主窗口
            Loaded += (_, _) =>
            {
                if (Window.GetWindow(this) is Window w)
                {
                    w.PreviewKeyDown += GameAssistant_PreviewKeyDown;
                }
            };
            Unloaded += (_, _) =>
            {
                if (Window.GetWindow(this) is Window w)
                {
                    w.PreviewKeyDown -= GameAssistant_PreviewKeyDown;
                }
            };

            _autoFireApplyDebounce.Tick += (_, _) =>
            {
                _autoFireApplyDebounce.Stop();
                ApplyAutoFire();
            };

            ApplyVersionGating();
#endif
        }

        /// <summary>
        /// 版本切换后刷新：清理旧版本门控状态（恢复卡片 + 移除提示），按当前版本重新应用。
        /// 由 LoginWindow 在版本切换时调用，保证游戏助手页"当前版本"显示与实际选中版本同步。
        /// </summary>
        public void RefreshForVersion()
        {
#if EXTERNAL
            // ASSIST-OFF-EXTERNAL：外网版整页停用，构造函数已提前返回，这里同样不做事。
            return;
#else
            // 清理上一次门控：恢复卡片可用并移除旧提示
            if (_versionGateTip != null)
            {
                RootStack.Children.Remove(_versionGateTip);
                _versionGateTip = null;
            }

            foreach (var card in new FrameworkElement[] { DpsCard, HelperPassCard, HelperPickupCard })
            {
                card.IsEnabled = true;
                card.Opacity = 1.0;
            }

            ApplyVersionGating();
#endif
        }

        /// <summary>
        /// 按当前版本禁用不支持的辅助项：
        /// DPS 覆盖层 / 顺图 / 自动拾取 为 90 专属（Helper DLL + 18111 链路），
        /// S4A21/S4A12 架构下无效，直接置灰并提示；按键连发（全局共享 DLL）保留可用。
        /// </summary>
        private void ApplyVersionGating()
        {
            if (AppConfig.CurrentProfile.Id == "90")
            {
                return;
            }

            foreach (var card in new FrameworkElement[] { DpsCard, HelperPassCard, HelperPickupCard })
            {
                card.IsEnabled = false;
                card.Opacity = 0.45;
            }

            _versionGateTip = new TextBlock
            {
                Text = "当前版本（" + AppConfig.CurrentProfile.Name + "）不支持 DPS / 顺图 / 自动拾取，仅按键连发可用",
                Foreground = (System.Windows.Media.Brush)new System.Windows.Media.BrushConverter().ConvertFrom("#FFE8A23A")!,
                FontSize = 12,
                Margin = new Thickness(0, 10, 0, 0),
                TextWrapping = TextWrapping.Wrap
            };
            RootStack.Children.Insert(1, _versionGateTip);
        }

        /// <summary>DPS 开关变化，同步到全局配置并保存；关闭时立即停止所有 DPS 覆盖层与 hook</summary>
        private void ToggleDps_Changed(object sender, RoutedEventArgs e)
        {
            var enabled = ToggleDps.IsChecked == true;
            AppConfig.Settings.DpsEnabled = enabled;
            AppConfig.Save();

            // 关闭分支：DPS 会话仅由启动游戏时 Start 创建，无随开关关闭的停止路径，
            // 必须显式调用 StopAll 停止所有运行中的覆盖层与 inline hook，避免残留
            if (!enabled)
            {
                Services.DpsOverlayManager.StopAll();
            }
        }

        /// <summary>顺图开关变化：写回 JSON + 写入共享内存（DLL 实时门控）</summary>
        private void ToggleHelperPass_Changed(object sender, RoutedEventArgs e)
        {
            bool enabled = ToggleHelperPass.IsChecked == true;
            AppConfig.Settings.HelperPassEnabled = enabled;
            AppConfig.Save();
            Services.HelperSharedMemory.SetPassEnabled(enabled);
        }

        /// <summary>拾取开关变化：写回 JSON + 写入共享内存（DLL 实时门控）</summary>
        private void ToggleHelperPickup_Changed(object sender, RoutedEventArgs e)
        {
            bool enabled = ToggleHelperPickup.IsChecked == true;
            AppConfig.Settings.HelperPickupEnabled = enabled;
            AppConfig.Save();
            Services.HelperSharedMemory.SetPickupEnabled(enabled);
        }

        /// <summary>连发总开关变化：显示/隐藏配置区，并立即启动/停止引擎</summary>
        private void ToggleAutoFire_Changed(object sender, RoutedEventArgs e)
        {
            bool enabled = ToggleAutoFire.IsChecked == true;
            AutoFireConfigArea.Visibility = enabled ? Visibility.Visible : Visibility.Collapsed;
            ApplyAutoFire();
        }

        /// <summary>
        /// 把当前连发配置写回 JSON，并按开关状态启动/停止引擎。
        /// 列表增删、按键录入、间隔修改均调用此方法实现即时生效。
        /// </summary>
        private void ApplyAutoFire()
        {
            AppConfig.Settings.AutoFireEnabled = ToggleAutoFire.IsChecked == true;
            AppConfig.Settings.AutoFireKeys = _autoFireKeys.ToList();
            AppConfig.Save();

            if (AppConfig.Settings.AutoFireEnabled)
            {
                AutoFireEngine.Start(_autoFireKeys);
            }
            else
            {
                AutoFireEngine.Stop();
            }
        }

        /// <summary>间隔输入变化：防抖 400ms 后应用到引擎，避免每敲一个字符就重启引擎</summary>
        private void AutoFireInterval_TextChanged(object sender, TextChangedEventArgs e)
        {
            if (!IsLoaded)
            {
                return;
            }

            _autoFireApplyDebounce.Stop();
            _autoFireApplyDebounce.Start();
        }

        /// <summary>添加一行连发按键配置</summary>
        private void AutoFireAdd_Click(object sender, RoutedEventArgs e)
        {
            _autoFireKeys.Add(new AutoFireKeyConfig { Key = "", IntervalMs = 10 });
            ApplyAutoFire();
        }

        /// <summary>删除该行连发按键配置（每个按键独立的删除按钮）</summary>
        private void AutoFireRow_Delete_Click(object sender, RoutedEventArgs e)
        {
            if (sender is FrameworkElement fe && fe.DataContext is AutoFireKeyConfig cfg)
            {
                _autoFireKeys.Remove(cfg);
                if (ReferenceEquals(_capturingRow, cfg))
                {
                    _capturingRow = null;
                }

                ApplyAutoFire();
            }
        }

        /// <summary>点击按键按钮进入录入模式：窗口级键盘捕获，按任意键录入到该行</summary>
        private void AutoFireKey_Click(object sender, RoutedEventArgs e)
        {
            if (sender is Button btn && btn.DataContext is AutoFireKeyConfig cfg)
            {
                _capturingRow = cfg;
                btn.Content = "请按键...";
                btn.Focus(); // 确保键盘事件落在窗口内
                e.Handled = true;
            }
        }

        /// <summary>录入模式下的键盘捕获：非修饰键按下即完成录入；Esc 取消录入（不改动原值）</summary>
        private void GameAssistant_PreviewKeyDown(object sender, KeyEventArgs e)
        {
            if (_capturingRow == null)
            {
                return;
            }

            // Esc 取消本次录入
            if (e.Key == Key.Escape)
            {
                _capturingRow = null;
                RefreshKeyButtons();
                e.Handled = true;
                return;
            }

            // 修饰键单独按下不结束录入，继续等主键
            if (IsModifierKey(e.Key))
            {
                return;
            }

            string? display = KeyToDisplayName(e.Key);
            if (display != null && AutoFireEngine.KeyNameToVk(display) != 0)
            {
                _capturingRow.Key = display;
            }

            _capturingRow = null;
            RefreshKeyButtons();
            ApplyAutoFire();
            e.Handled = true;
        }

        /// <summary>刷新所有按键按钮文本（退出录入模式后恢复显示键名）</summary>
        private void RefreshKeyButtons()
        {
            foreach (var item in AutoFireList.Items)
            {
                if (AutoFireList.ItemContainerGenerator.ContainerFromItem(item) is ContentPresenter cp)
                {
                    var btn = FindVisualChild<Button>(cp);
                    if (btn != null)
                    {
                        btn.Content = ((AutoFireKeyConfig)item).Key;
                    }
                }
            }
        }

        /// <summary>按键显示名 → 引擎可解析的名称；无法识别返回 null</summary>
        private static string? KeyToDisplayName(Key key)
        {
            if (key >= Key.A && key <= Key.Z)
            {
                return key.ToString();
            }

            if (key >= Key.D0 && key <= Key.D9)
            {
                return ((char)('0' + (key - Key.D0))).ToString();
            }

            if (key >= Key.NumPad0 && key <= Key.NumPad9)
            {
                return "Numpad" + (key - Key.NumPad0);
            }

            if (key >= Key.F1 && key <= Key.F24)
            {
                return "F" + (key - Key.F1 + 1);
            }

            switch (key)
            {
                case Key.Space: return "Space";
                case Key.Enter: return "Enter";
                case Key.Tab: return "Tab";
                case Key.Back: return "Backspace";
                case Key.Escape: return "Esc";
                case Key.Up: return "Up";
                case Key.Down: return "Down";
                case Key.Left: return "Left";
                case Key.Right: return "Right";
                case Key.Home: return "Home";
                case Key.End: return "End";
                case Key.PageUp: return "PgUp";
                case Key.PageDown: return "PgDn";
                case Key.Insert: return "Ins";
                case Key.Delete: return "Del";
                case Key.OemSemicolon: return ";";
                case Key.OemQuotes: return "'";
                case Key.OemComma: return ",";
                case Key.OemPeriod: return ".";
                case Key.OemQuestion: return "/";
                case Key.OemBackslash: return "\\";
                case Key.OemOpenBrackets: return "[";
                case Key.OemCloseBrackets: return "]";
                case Key.OemMinus: return "-";
                case Key.OemPlus: return "=";
                case Key.OemTilde: return "`";
                case Key.Multiply: return "Numpad*";
                case Key.Add: return "Numpad+";
                case Key.Subtract: return "Numpad-";
                case Key.Decimal: return "Numpad.";
                case Key.Divide: return "Numpad/";
                default: return null;
            }
        }

        private static bool IsModifierKey(Key key)
        {
            return key is Key.LeftShift or Key.RightShift or Key.LeftCtrl or Key.RightCtrl
                or Key.LeftAlt or Key.RightAlt or Key.LWin or Key.RWin
                or Key.System or Key.ImeProcessed;
        }

        private static T? FindVisualChild<T>(DependencyObject parent) where T : DependencyObject
        {
            for (int i = 0; i < VisualTreeHelper.GetChildrenCount(parent); i++)
            {
                var child = VisualTreeHelper.GetChild(parent, i);
                if (child is T typed)
                {
                    return typed;
                }

                var found = FindVisualChild<T>(child);
                if (found != null)
                {
                    return found;
                }
            }

            return null;
        }
    }
}
