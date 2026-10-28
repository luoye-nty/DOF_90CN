using System;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using DNFLogin.Services;

namespace DNFLogin
{
    /// <summary>
    /// 登录器强制更新窗口（版本校验不通过时弹出）。
    ///
    /// ── 语义（需求确认过的口径）──────────────────────────────────────────────
    /// · 只有一条出路：**更新成功**。更新成功 → 拉起替换脚本 → 退出本进程 →
    ///   由脚本删掉原登录器、落地新登录器并自动打开它。
    /// · 下载失败 → 停在窗口上可点「重试」（网络抖动留余地）；
    /// · 玩家点「取消」（或直接关窗）→ **退出登录器**，不许进登录界面。
    ///
    /// 所以本窗口的 DialogResult 只有两种含义：
    ///   true  = 新登录器已就绪、替换脚本已拉起，调用方必须立刻退出本进程；
    ///   false = 玩家取消，调用方同样必须退出（只是不更新）。
    ///
    /// 注意：调用方拿到 false 之后**不能**继续进登录界面 —— 这是"不更新禁止进入"的落点。
    /// </summary>
    public partial class LauncherUpdateDialog : Window
    {
        private readonly string _downloadUrl;
        private readonly string _requiredVersion;
        private CancellationTokenSource _cts = new();
        private bool _busy;
        /// <summary>已经完成后台下载解压的更新计划（重试时若已就绪可直接复用，不必重新下载）。</summary>
        private LauncherUpdatePlan? _plan;

        /// <summary>构造更新窗口。</summary>
        /// <param name="downloadUrl">登录器下载直链（服务端下发，非空）。</param>
        /// <param name="requiredVersion">服务端要求的版本号（仅用于展示）。</param>
        public LauncherUpdateDialog(string downloadUrl, string requiredVersion)
        {
            InitializeComponent();
            _downloadUrl = downloadUrl ?? "";
            _requiredVersion = (requiredVersion ?? "").Trim();

            VersionText.Text = $"当前版本：{LauncherVersion.Current}    →    需要版本：{_requiredVersion}";
            HintText.Text = "更新完成前无法进入登录界面。更新过程中请不要关闭登录器。";

            Loaded += (_, _) => _ = RunAsync();
        }

        private async Task RunAsync()
        {
            if (_busy)
            {
                return;
            }
            _busy = true;

            // 每次重试都要换一个 CTS：上一次的取消令牌不能复用。
            _cts.Dispose();
            _cts = new CancellationTokenSource();
            var token = _cts.Token;

            Dispatcher.Invoke(() =>
            {
                RetryButton.Visibility = Visibility.Collapsed;
                CancelButton.IsEnabled = true;
                CancelButton.Content = "取消";
            });

            // 关窗结果先攒着，等 _busy 复位之后再设置 DialogResult。
            // 为什么不能直接在 try/catch 里设：设置 DialogResult 会触发 Close()，
            // 而 OnClosing 里"更新中不许关窗"的守卫看的就是 _busy —— 那样会把自己弹回去，
            // 严重时抛 InvalidOperationException（DialogResult 只能用于以对话框方式显示的窗口）。
            bool? finishResult = null;

            try
            {
                var progress = new Progress<LauncherUpdateProgress>(OnProgress);
                var (plan, error) = await LauncherSelfUpdater
                    .DownloadAndStageAsync(_downloadUrl, progress, token).ConfigureAwait(true);

                if (plan is null)
                {
                    ShowRetryState(error);
                }
                else
                {
                    _plan = plan;
                    SetState("正在应用更新…", 100, false);
                    HintText.Text = "即将关闭当前登录器并自动打开新版本，请稍候…";

                    var (ok, applyError) = LauncherSelfUpdater.ApplyAndRestart(plan);
                    if (ok)
                    {
                        // 交给调用方退出本进程（替换脚本正在等我们退出）。
                        finishResult = true;
                    }
                    else
                    {
                        ShowRetryState(applyError);
                    }
                }
            }
            catch (OperationCanceledException)
            {
                // 玩家主动取消：按"退出登录器"处理（见类注释）。
                finishResult = false;
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherUpdateDialog: 更新异常 -> " + ex);
                ShowRetryState("更新失败：" + ex.Message);
            }
            finally
            {
                _busy = false;
            }

            if (finishResult.HasValue)
            {
                DialogResult = finishResult.Value;
            }
        }

        private void OnProgress(LauncherUpdateProgress p)
        {
            Dispatcher.Invoke(() =>
            {
                StatusText.Text = p.Message;
                Progress.Value = Math.Clamp(p.Percent, 0, 100);
                if (p.Stage == LauncherUpdateStage.Downloading && p.Total > 0)
                {
                    var doneMb = p.Done / 1024.0 / 1024.0;
                    var totalMb = p.Total / 1024.0 / 1024.0;
                    PercentText.Text = $"{p.Percent}%  {doneMb:0.0}MB / {totalMb:0.0}MB    {p.SpeedMBps:0.0}MB/s";
                }
                else
                {
                    PercentText.Text = p.Percent > 0 ? $"{p.Percent}%" : "";
                }
            });
        }

        /// <summary>失败态：显示原因 + 露出「重试」按钮（取消按钮仍在 = 退出登录器）。</summary>
        private void ShowRetryState(string message)
        {
            Dispatcher.Invoke(() =>
            {
                StatusText.Text = string.IsNullOrWhiteSpace(message) ? "更新失败。" : message;
                HintText.Text = "更新未完成，无法进入登录界面。可以点「重试」重新下载；" +
                                "若多次失败请联系管理员。";
                Progress.Value = 0;
                PercentText.Text = "";
                CancelButton.IsEnabled = true;
                RetryButton.Visibility = Visibility.Visible;
            });
        }

        private void SetState(string text, int percent, bool cancelEnabled)
        {
            Dispatcher.Invoke(() =>
            {
                StatusText.Text = text;
                Progress.Value = Math.Clamp(percent, 0, 100);
                CancelButton.IsEnabled = cancelEnabled;
            });
        }

        private void RetryButton_Click(object sender, RoutedEventArgs e)
        {
            if (_busy)
            {
                return;
            }

            // 已经下好并落地过了（上次卡在"应用更新"这一步）：直接重试应用，不用重新下载。
            if (_plan is not null)
            {
                _busy = true;
                try
                {
                    HintText.Text = "即将关闭当前登录器并自动打开新版本，请稍候…";
                    var (ok, error) = LauncherSelfUpdater.ApplyAndRestart(_plan);
                    if (ok)
                    {
                        DialogResult = true;
                        return;
                    }
                    ShowRetryState(error);
                }
                finally
                {
                    // 走到这里说明 apply 失败（成功分支已经 return，窗口即将关闭）。
                    _busy = false;
                }
                return;
            }

            _ = RunAsync();
        }

        private void CancelButton_Click(object sender, RoutedEventArgs e)
        {
            if (_busy)
            {
                // 下载/解压中：先取消任务，RunAsync 会以 false 收尾（= 退出登录器）。
                CancelButton.IsEnabled = false;
                CancelButton.Content = "正在取消…";
                StatusText.Text = "正在取消…";
                try
                {
                    _cts.Cancel();
                }
                catch
                {
                    // 取消失败不额外处理：任务结束时会走正常出口
                }
                return;
            }

            // 失败态下点「取消」= 退出登录器（不允许跳过更新进登录界面）。
            DialogResult = false;
        }

        /// <summary>
        /// 右上角没有关闭按钮（WindowStyle=None），但 Alt+F4 仍会走到这里。
        /// 更新中一律不许关窗：关掉就会出现"替换脚本等着旧进程退出、玩家却卡在自检界面"
        /// 的悬空状态。非更新中直接放行，由调用方按 DialogResult（可能为 null）处理 ——
        /// null 在调用方等价于"没更新成功"，同样会退出登录器。
        /// </summary>
        protected override void OnClosing(System.ComponentModel.CancelEventArgs e)
        {
            if (_busy)
            {
                e.Cancel = true;
                return;
            }
            base.OnClosing(e);
        }

        protected override void OnClosed(EventArgs e)
        {
            try
            {
                _cts.Cancel();
            }
            catch
            {
                // 忽略
            }
            _cts.Dispose();
            base.OnClosed(e);
        }
    }
}
