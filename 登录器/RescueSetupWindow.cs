using System;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Media;

namespace DNFLogin
{
    /// <summary>
    /// 登录后救援代码补设引导窗（90 本地/远程通用）：
    /// 检测到老号未设置救援代码时弹出，登录态下补设 6 位数字救援代码，可跳过。
    /// 远程：POST /launcher/v1/rescue-code（凭 ticket）；本地：直连库 UPDATE rescue_code_hash。
    /// </summary>
    public class RescueSetupWindow : Window
    {
        private readonly AppSettings _settings;
        private readonly bool _isRemote;
        private readonly string _account;
        private readonly string _ticket;

        private TextBox _codeBox = null!;
        private TextBlock _statusText = null!;
        private Button _submitButton = null!;
        private bool _busy;

        /// <summary>用户是否已成功补设救援代码</summary>
        public bool Succeeded { get; private set; }

        public RescueSetupWindow(AppSettings settings, bool isRemote, string account, string ticket)
        {
            _settings = settings;
            _isRemote = isRemote;
            _account = account;
            _ticket = ticket;

            Title = "设置救援代码";
            Width = 420;
            SizeToContent = SizeToContent.Height;
            WindowStartupLocation = WindowStartupLocation.CenterOwner;
            ResizeMode = ResizeMode.NoResize;
            Background = new SolidColorBrush(Color.FromRgb(0x20, 0x24, 0x2A));
            Foreground = Brushes.White;
            FontSize = 13;

            BuildUi();
            Loaded += (_, _) => { _codeBox.Focus(); };
        }

        private void BuildUi()
        {
            var root = new StackPanel { Margin = new Thickness(20), MaxWidth = 380 };

            root.Children.Add(new TextBlock
            {
                Text = $"账号 {_account} 尚未设置救援代码",
                FontSize = 15,
                FontWeight = FontWeights.SemiBold,
                Foreground = Brushes.White
            });

            root.Children.Add(new TextBlock
            {
                Text = "救援代码是忘记密码时找回账号的唯一凭证，注册新号时已必填；老号建议现在补设。当前为" +
                       (_isRemote ? "远程模式" : "本地模式") + "，设置后立即生效。",
                TextWrapping = TextWrapping.Wrap,
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 10, 0, 0),
                LineHeight = 20
            });

            _codeBox = new TextBox
            {
                Height = 34,
                FontSize = 15,
                Margin = new Thickness(0, 14, 0, 0),
                Padding = new Thickness(8, 4, 8, 4),
                MaxLength = 6,
                VerticalContentAlignment = VerticalAlignment.Center,
                HorizontalContentAlignment = HorizontalAlignment.Center,
                Background = new SolidColorBrush(Color.FromRgb(0x2A, 0x30, 0x38)),
                Foreground = Brushes.White,
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x66, 0xFF, 0xFF, 0xFF)),
                CaretBrush = Brushes.White
            };
            _codeBox.PreviewTextInput += CodeBox_OnPreviewTextInput;
            DataObject.AddPastingHandler(_codeBox, CodeBox_OnPaste);
            root.Children.Add(_codeBox);

            _statusText = new TextBlock
            {
                Text = "请输入 6 位数字救援代码",
                Foreground = new SolidColorBrush(Color.FromArgb(0x99, 0xFF, 0xFF, 0xFF)),
                FontSize = 12,
                Margin = new Thickness(0, 6, 0, 0)
            };
            root.Children.Add(_statusText);

            var buttonRow = new StackPanel
            {
                Orientation = Orientation.Horizontal,
                HorizontalAlignment = HorizontalAlignment.Right,
                Margin = new Thickness(0, 16, 0, 0)
            };

            var skipButton = new Button
            {
                Content = "暂不设置",
                Width = 88,
                Height = 32,
                Margin = new Thickness(0, 0, 10, 0),
                Background = Brushes.Transparent,
                Foreground = new SolidColorBrush(Color.FromRgb(0xAA, 0xAA, 0xAA)),
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x55, 0xFF, 0xFF, 0xFF)),
                Cursor = Cursors.Hand
            };
            skipButton.Click += (_, _) => Close();
            buttonRow.Children.Add(skipButton);

            _submitButton = new Button
            {
                Content = "设置",
                Width = 110,
                Height = 32,
                Background = new SolidColorBrush(Color.FromRgb(0xE8, 0x4A, 0x3A)),
                Foreground = Brushes.White,
                FontWeight = FontWeights.SemiBold,
                BorderThickness = new Thickness(0),
                Cursor = Cursors.Hand
            };
            _submitButton.Click += async (_, _) => await SubmitAsync();
            buttonRow.Children.Add(_submitButton);

            root.Children.Add(buttonRow);
            Content = root;
        }

        private static void CodeBox_OnPreviewTextInput(object sender, TextCompositionEventArgs e)
        {
            foreach (var ch in e.Text)
            {
                if (ch < '0' || ch > '9')
                {
                    e.Handled = true;
                    return;
                }
            }
        }

        private static void CodeBox_OnPaste(object sender, DataObjectPastingEventArgs e)
        {
            if (!e.DataObject.GetDataPresent(typeof(string)))
            {
                e.CancelCommand();
                return;
            }

            var text = (string)e.DataObject.GetData(typeof(string))!;
            foreach (var ch in text)
            {
                if (ch < '0' || ch > '9')
                {
                    e.CancelCommand();
                    return;
                }
            }
        }

        private async Task SubmitAsync()
        {
            if (_busy)
            {
                return;
            }

            var code = _codeBox.Text?.Trim() ?? string.Empty;
            if (!DatabaseService.IsValidRescueCode(code))
            {
                _statusText.Text = "救援代码须为 6 位数字";
                _statusText.Foreground = new SolidColorBrush(Color.FromRgb(0xFF, 0x6B, 0x6B));
                return;
            }

            _busy = true;
            _submitButton.IsEnabled = false;
            _statusText.Text = "正在设置…";
            _statusText.Foreground = new SolidColorBrush(Color.FromArgb(0x99, 0xFF, 0xFF, 0xFF));

            string? error;
            if (_isRemote)
            {
                var (ok, err) = await Services.RemoteAuthService.SetRescueCodeAsync(_settings, _ticket, code);
                error = ok ? null : err;
            }
            else
            {
                error = await DatabaseService.UpdateRescueCodeAsync(_settings, _account, code);
            }

            if (error is not null)
            {
                _statusText.Text = error;
                _statusText.Foreground = new SolidColorBrush(Color.FromRgb(0xFF, 0x6B, 0x6B));
                _busy = false;
                _submitButton.IsEnabled = true;
                return;
            }

            Succeeded = true;
            Close();
        }
    }
}
