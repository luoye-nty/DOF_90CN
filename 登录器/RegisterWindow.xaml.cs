using System;
using System.ComponentModel;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Media.Imaging;

namespace DNFLogin
{
    /// <summary>
    /// 注册弹窗（界面演示版）：展示注册界面，验证码本地生成，仅限本地演示。
    /// </summary>
    public partial class RegisterWindow : Window
    {
        private static readonly Random Rng = new();

        private bool _isSubmitting;
        private string _currentCaptcha = string.Empty;

        /// <summary>注册成功后返回给登录界面的账号（规范化小写），未注册成功为 null</summary>
        public string? RegisteredAccount { get; private set; }

        public RegisterWindow()
        {
            InitializeComponent();
            _ = GenerateCaptchaAsync();
            // 打开就实时拉一次注册信息（注册码开关/是否已关闭自助注册）。
            // 不用等 Window_Loaded：这个请求很轻，早一点拿到就能早一点把注册码框显示出来。
            Loaded += async (_, _) => await RefreshRegistrationInfoAsync();
        }

        private void Window_Loaded(object sender, RoutedEventArgs e)
        {
            // 救援代码为 90 版本（本地+远程）找回密码专用；S4 系列（A12/A21）不支持，隐藏输入区
            if (Services.ServerManager.IsS4Profile(AppConfig.Settings.CurrentProfileId))
            {
                RescueCodePanel.Visibility = Visibility.Collapsed;
            }

            // 注册码：只有运维工具「注册需要注册码」开着时才显示（自检阶段拉到的开关）。
            // 未开启 / 没拉到（null）时保持 XAML 里的 Collapsed —— 注册界面与改动前完全一致。
            ApplyRegistrationCodePanel();
        }

        /// <summary>
        /// 按当前拿到的注册闸门状态显示/隐藏「注册码」输入区。
        /// </summary>
        private void ApplyRegistrationCodePanel()
        {
            RegistrationCodePanel.Visibility =
                Services.RemoteAuthService.RegistrationCodeRequired == true
                    ? Visibility.Visible
                    : Visibility.Collapsed;
        }

        /// <summary>
        /// 打开注册界面时**实时**问一次服务器注册信息（注册码是否需要、注册是否已关闭）。
        ///
        /// 为什么必须现问：登录器是几分钟前启动的，自检时拉到的开关可能已经变了 ——
        /// 线上真实反馈："管理工具先开了注册码，登录器还开着，点注册仍然不要求填码"。
        /// 这里补一次实时拉取，并把注册码输入框按最新状态显示出来；
        /// 提交时（RegisterButton_Click）还会再问一次做兜底。
        /// 拉取失败保持原状（不能因为一次网络抖动把注册界面卡死）。
        /// </summary>
        private async Task RefreshRegistrationInfoAsync()
        {
            if (AppConfig.Mode != LoginMode.Remote)
            {
                return;
            }
            try
            {
                var host = !string.IsNullOrWhiteSpace(AppConfig.Settings.RemoteIp)
                    ? AppConfig.Settings.RemoteIp.Trim()
                    : (AppConfig.Settings.Host ?? "").Trim();
                if (string.IsNullOrWhiteSpace(host))
                {
                    return;
                }
                var port = 18112;
                var cfgPort = AppConfig.CurrentProfile?.RemotePort;
                if (!string.IsNullOrWhiteSpace(cfgPort) && int.TryParse(cfgPort, out var parsed))
                {
                    port = parsed;
                }
                var (ok, _, info) = await Services.RemoteAuthService.GetRegistrationInfoAsync(
                    AppConfig.Settings, host, port).ConfigureAwait(true);
                if (!ok || info is null)
                {
                    return;
                }
                ApplyRegistrationCodePanel();
                App.LogStartup("注册界面实时拉取注册信息：code_required=" + info.CodeRequired + " closed=" + info.Closed);
                if (info.Closed)
                {
                    ShowStatus("当前服务器已关闭玩家自助注册，请联系管理员建号。", Brushes.OrangeRed, "提示");
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("注册界面实时拉取注册信息异常已忽略 -> " + ex.Message);
            }
        }

        protected override void OnClosing(CancelEventArgs e)
        {
            base.OnClosing(e);
        }

        private void CloseButton_Click(object sender, RoutedEventArgs e)
        {
            Close();
        }

        private async void CaptchaImage_MouseDown(object sender, MouseButtonEventArgs e)
        {
            await GenerateCaptchaAsync();
        }

        /// <summary>
        /// 本地生成随机验证码图片（4位数字+字母，演示用）。
        /// </summary>
        private async Task GenerateCaptchaAsync()
        {
            await Task.Run(async () =>
            {
                _currentCaptcha = string.Empty;
                const string chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
                for (var i = 0; i < 4; i++)
                {
                    _currentCaptcha += chars[Rng.Next(chars.Length)];
                }

                var bitmap = new RenderTargetBitmap(110, 36, 96, 96, PixelFormats.Pbgra32);
                var drawingVisual = new DrawingVisual();

                using (var ctx = drawingVisual.RenderOpen())
                {
                    // 背景
                    ctx.DrawRectangle(new SolidColorBrush(Color.FromArgb(30, 255, 255, 255)), null, new Rect(0, 0, 110, 36));

                    // 干扰线
                    for (var i = 0; i < 3; i++)
                    {
                        ctx.DrawLine(
                            new Pen(new SolidColorBrush(Color.FromArgb(80, 255, 255, 255)), 1),
                            new Point(Rng.Next(0, 110), Rng.Next(0, 36)),
                            new Point(Rng.Next(0, 110), Rng.Next(0, 36)));
                    }

                    // 验证码字符
                    var typeface = new Typeface("Arial");
                    var pixelsPerDip = VisualTreeHelper.GetDpi(this).PixelsPerDip;
                    for (var i = 0; i < _currentCaptcha.Length; i++)
                    {
                        var angle = Rng.Next(-15, 16);
                        ctx.PushTransform(new RotateTransform(angle, 12 + i * 25, 20));
                        var text = new FormattedText(
                            _currentCaptcha[i].ToString(),
                            System.Globalization.CultureInfo.InvariantCulture,
                            FlowDirection.LeftToRight,
                            typeface, 20,
                            new SolidColorBrush(Color.FromArgb(200, 255, 255, 255)),
                            pixelsPerDip);
                        ctx.DrawText(text, new Point(8 + i * 25, 5));
                        ctx.Pop();
                    }
                }

                bitmap.Render(drawingVisual);
                bitmap.Freeze();

                await Dispatcher.InvokeAsync(() =>
                {
                    CaptchaImage.Source = bitmap;
                });
            }).ConfigureAwait(false);
        }

        private async void RegisterButton_Click(object sender, RoutedEventArgs e)
        {
            if (_isSubmitting)
            {
                return;
            }

            var account = AccountTextBox.Text?.Trim() ?? string.Empty;
            var password = PasswordBox.Password ?? string.Empty;
            var confirmPassword = ConfirmPasswordBox.Password ?? string.Empty;
            var captcha = CaptchaTextBox.Text?.Trim() ?? string.Empty;

            if (string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(password))
            {
                ShowStatus("账号和密码不能为空", Brushes.OrangeRed, "提示");
                return;
            }

            if (!string.Equals(password, confirmPassword, StringComparison.Ordinal))
            {
                ShowStatus("两次输入的密码不一致", Brushes.OrangeRed, "提示");
                return;
            }

            // 90 版本注册必填救援代码（6 位数字）；S4 系列无此功能（面板已隐藏，不校验）
            var rescueCode = RescueCodePanel.Visibility == Visibility.Visible
                ? (RescueCodeTextBox.Text?.Trim() ?? string.Empty)
                : string.Empty;
            if (!Services.ServerManager.IsS4Profile(AppConfig.Settings.CurrentProfileId)
                && !DatabaseService.IsValidRescueCode(rescueCode))
            {
                ShowStatus("救援代码须为 6 位数字，用于忘记密码时找回账号", Brushes.OrangeRed, "提示");
                return;
            }

            if (string.IsNullOrWhiteSpace(captcha))
            {
                ShowStatus("请输入验证码", Brushes.OrangeRed, "提示");
                return;
            }

            if (!string.Equals(captcha, _currentCaptcha, StringComparison.OrdinalIgnoreCase))
            {
                ShowStatus("验证码错误", Brushes.OrangeRed, "提示");
                await GenerateCaptchaAsync();
                CaptchaTextBox.Text = string.Empty;
                return;
            }

            // 置位必须在下面这些 await **之前**：RefreshRegistrationInfoAsync 是网络调用，
            // 慢网下连点"注册"会绕过 _isSubmitting 检查并发提交两次（第二次通常被"账号已注册"拒掉）。
            _isSubmitting = true;
            RegisterSubmitButton.IsEnabled = false;

            // 注册码：只在面板可见（= 服务端开了「注册需要注册码」）时必填。
            // 归一化交给服务端（大小写/横线/空格都能认），这里只做"非空"这一道本地校验。
            //
            // 提交前**再问一次**服务器：这扇窗可能已经开了很久，期间管理员可能刚把注册码打开。
            // 拉到最新状态后再决定"要不要填码"，避免出现"界面没让填、服务端却要码"的死局。
            await RefreshRegistrationInfoAsync();
            var registrationCode = RegistrationCodePanel.Visibility == Visibility.Visible
                ? (RegistrationCodeTextBox.Text?.Trim() ?? string.Empty)
                : string.Empty;
            if (RegistrationCodePanel.Visibility == Visibility.Visible && registrationCode.Length == 0)
            {
                ShowStatus("本次注册需要注册码，请向管理员索取后填入", Brushes.OrangeRed, "提示");
                RegistrationCodeTextBox.Focus();
                _isSubmitting = false;
                RegisterSubmitButton.IsEnabled = true;
                return;
            }

            // 注册分流：
            //  S4 系列（A12/A21）远程 → 7002 认证端口收口注册（工具验签/查重/写 inventory.db accounts 并回信，
            //    登录器不做本地校验；与远程登录同一套 7002 协议）
            //  其它远程模式（90）→ HTTPS API register（RemoteAuthService，规则与本地一致）
            //  S4 系列（A12/A21）本地 → 直连服务端 inventory.db 的 accounts 表
            //  本地模式 → 直连本地 MySQL/SQLite（dnf_login_accounts）
            string? error;
            var isRemote = AppConfig.Mode == LoginMode.Remote;
            var isS4 = Services.ServerManager.IsS4Profile(AppConfig.Settings.CurrentProfileId);
            if (isRemote && isS4)
            {
                // 7002 注册收口：s4auth_register（阶段1握手 + register 业务包，密码明文仅存于 7002 会话内）
                if (!Services.S4AuthCoreNative.IsReady)
                {
                    error = "认证核心组件缺失（s4authcore.dll），请使用完整版登录器";
                }
                else if (!Services.S4AuthCoreNative.Register(
                             AppConfig.Settings.RemoteIp.Trim(),
                             Services.ServerManager.S4AuthPort,
                             account,
                             password,
                             out var reg,
                             out var regNativeError))
                {
                    error = regNativeError ?? "7002 注册失败";
                }
                else if (reg.Ok != 1)
                {
                    error = string.IsNullOrWhiteSpace(reg.MsgText) ? "服务器拒绝注册" : reg.MsgText;
                }
                else
                {
                    error = null;
                }
            }
            else if (isRemote)
            {
                var (regOk, regError) = await Services.RemoteAuthService.RegisterForTicketAsync(
                    AppConfig.Settings, account, password, rescueCode, registrationCode);
                error = regOk ? null : regError;
            }
            else if (isS4)
            {
                error = await DatabaseService.RegisterS4Async(AppConfig.Settings, account, password);
            }
            else
            {
                error = await DatabaseService.RegisterAsync(AppConfig.Settings, account, password, rescueCode);
            }
            if (string.IsNullOrEmpty(error))
            {
                RegisteredAccount = DatabaseService.NormalizeUsername(account);
                ShowStatus("注册成功，即将返回登录界面", Brushes.LawnGreen, "成功");

                await Task.Delay(1200).ConfigureAwait(true);

                // 延迟期间窗口可能已被用户提前关闭（返回登录/右上角 X/Alt+F4）：
                // 窗口已非对话框状态，再设置 DialogResult 会抛 InvalidOperationException 导致进程崩溃，故先检查再返回
                if (!IsLoaded || !IsVisible)
                {
                    return;
                }

                _isSubmitting = false;
                RegisterSubmitButton.IsEnabled = true;
                HideStatus();
                DialogResult = true;
                Close();
                return;
            }

            _isSubmitting = false;
            RegisterSubmitButton.IsEnabled = true;
            ShowStatus(FriendlyError(error), Brushes.OrangeRed, "提示");
        }

        /// <summary>
        /// 注册结果错误文案统一转友好中文：
        /// 服务端错误码（invalid-username / password-too-short 等）映射为中文规则提示；
        /// 含 HTTP/英文 JSON/英文异常的原始错误不再直接展示；已是中文的文案原样保留。
        /// </summary>
        private static string FriendlyError(string? raw) =>
            Services.RemoteAuthService.ToFriendlyText(raw, "注册失败，请稍后重试或检查服务器配置");

        private void BackToLogin_Click(object sender, RoutedEventArgs e)
        {
            Close();
        }

        private void ShowStatus(string message, System.Windows.Media.Brush brush, string title)
        {
            StatusTitleText.Text = title;
            StatusBodyText.Text = message;
            StatusBodyText.Foreground = brush;
            RegisterStatusOverlay.Visibility = Visibility.Visible;
        }

        private void HideStatus()
        {
            RegisterStatusOverlay.Visibility = Visibility.Collapsed;
            StatusBodyText.Text = string.Empty;
        }

        private void StatusCloseButton_Click(object sender, RoutedEventArgs e)
        {
            HideStatus();
        }

        private void StatusOverlay_MouseDown(object sender, MouseButtonEventArgs e)
        {
            HideStatus();
        }

        private void StatusPanel_MouseDown(object sender, MouseButtonEventArgs e)
        {
            e.Handled = true;
        }
    }
}