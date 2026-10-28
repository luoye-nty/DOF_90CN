using System;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Media;

namespace DNFLogin
{
    /// <summary>
    /// 忘记密码（90 版本，本地/远程通用）：
    /// 账号 + 救援代码 + 新密码 + 确认密码，无需原密码。
    /// 远程：POST /launcher/v1/password/reset（改密成功作废在线会话）；
    /// 本地：直连库校验救援代码 bcrypt 后更新 password_hash。
    /// </summary>
    public class ForgotPasswordWindow : Window
    {
        private readonly AppSettings _settings;
        private readonly bool _isRemote;
        private readonly string _prefillAccount;

        private TextBox _accountBox = null!;
        private TextBox _rescueBox = null!;
        private PasswordBox _passwordBox = null!;
        private PasswordBox _confirmBox = null!;
        private TextBlock _statusText = null!;
        private Button _submitButton = null!;
        private bool _busy;

        public ForgotPasswordWindow(AppSettings settings, bool isRemote, string prefillAccount)
        {
            _settings = settings;
            _isRemote = isRemote;
            _prefillAccount = prefillAccount;

            Title = "找回密码";
            Width = 430;
            SizeToContent = SizeToContent.Height;
            WindowStartupLocation = WindowStartupLocation.CenterOwner;
            ResizeMode = ResizeMode.NoResize;
            Background = new SolidColorBrush(Color.FromRgb(0x20, 0x24, 0x2A));
            Foreground = Brushes.White;
            FontSize = 13;

            BuildUi();
            Loaded += (_, _) =>
            {
                if (string.IsNullOrWhiteSpace(_accountBox.Text))
                {
                    _accountBox.Focus();
                }
                else
                {
                    _rescueBox.Focus();
                }
            };
        }

        private void BuildUi()
        {
            var root = new StackPanel { Margin = new Thickness(20), MaxWidth = 390 };

            root.Children.Add(new TextBlock
            {
                Text = "通过救援代码找回密码",
                FontSize = 15,
                FontWeight = FontWeights.SemiBold,
                Foreground = Brushes.White
            });

            root.Children.Add(new TextBlock
            {
                Text = "当前为" + (_isRemote ? "远程模式" : "本地模式") + "。填写注册时设置的 6 位救援代码即可重置密码，无需原密码。",
                TextWrapping = TextWrapping.Wrap,
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 8, 0, 0),
                LineHeight = 20
            });

            root.Children.Add(new TextBlock
            {
                Text = "账号",
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 14, 0, 4)
            });
            _accountBox = MakeTextBox();
            _accountBox.Text = _prefillAccount;
            root.Children.Add(_accountBox);

            root.Children.Add(new TextBlock
            {
                Text = "救援代码",
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 10, 0, 4)
            });
            _rescueBox = MakeTextBox();
            _rescueBox.MaxLength = 6;
            _rescueBox.PreviewTextInput += (_, e) =>
            {
                foreach (var ch in e.Text)
                {
                    if (ch < '0' || ch > '9')
                    {
                        e.Handled = true;
                        return;
                    }
                }
            };
            root.Children.Add(_rescueBox);

            root.Children.Add(new TextBlock
            {
                Text = "新密码",
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 10, 0, 4)
            });
            _passwordBox = MakePasswordBox();
            root.Children.Add(_passwordBox);

            root.Children.Add(new TextBlock
            {
                Text = "确认新密码",
                Foreground = new SolidColorBrush(Color.FromArgb(0xCC, 0xFF, 0xFF, 0xFF)),
                Margin = new Thickness(0, 10, 0, 4)
            });
            _confirmBox = MakePasswordBox();
            root.Children.Add(_confirmBox);

            _statusText = new TextBlock
            {
                Foreground = new SolidColorBrush(Color.FromRgb(0xFF, 0x6B, 0x6B)),
                FontSize = 12,
                Margin = new Thickness(0, 8, 0, 0),
                TextWrapping = TextWrapping.Wrap
            };
            root.Children.Add(_statusText);

            var buttonRow = new StackPanel
            {
                Orientation = Orientation.Horizontal,
                HorizontalAlignment = HorizontalAlignment.Right,
                Margin = new Thickness(0, 16, 0, 0)
            };

            var cancelButton = new Button
            {
                Content = "取消",
                Width = 88,
                Height = 32,
                Margin = new Thickness(0, 0, 10, 0),
                Background = Brushes.Transparent,
                Foreground = new SolidColorBrush(Color.FromRgb(0xAA, 0xAA, 0xAA)),
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x55, 0xFF, 0xFF, 0xFF)),
                Cursor = Cursors.Hand
            };
            cancelButton.Click += (_, _) => Close();
            buttonRow.Children.Add(cancelButton);

            _submitButton = new Button
            {
                Content = "重置密码",
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

        private static TextBox MakeTextBox()
        {
            return new TextBox
            {
                Height = 32,
                Padding = new Thickness(8, 4, 8, 4),
                VerticalContentAlignment = VerticalAlignment.Center,
                Background = new SolidColorBrush(Color.FromRgb(0x2A, 0x30, 0x38)),
                Foreground = Brushes.White,
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x66, 0xFF, 0xFF, 0xFF)),
                CaretBrush = Brushes.White
            };
        }

        private static PasswordBox MakePasswordBox()
        {
            return new PasswordBox
            {
                Height = 32,
                Padding = new Thickness(8, 4, 8, 4),
                VerticalContentAlignment = VerticalAlignment.Center,
                Background = new SolidColorBrush(Color.FromRgb(0x2A, 0x30, 0x38)),
                Foreground = Brushes.White,
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x66, 0xFF, 0xFF, 0xFF)),
                CaretBrush = Brushes.White
            };
        }

        private async Task SubmitAsync()
        {
            if (_busy)
            {
                return;
            }

            var account = _accountBox.Text?.Trim() ?? string.Empty;
            var rescueCode = _rescueBox.Text?.Trim() ?? string.Empty;
            var password = _passwordBox.Password ?? string.Empty;
            var confirm = _confirmBox.Password ?? string.Empty;

            if (string.IsNullOrWhiteSpace(account))
            {
                ShowError("请输入账号");
                return;
            }
            if (!DatabaseService.IsValidRescueCode(rescueCode))
            {
                ShowError("救援代码须为 6 位数字");
                return;
            }
            if (password.Length < 8 || password.Length > 72)
            {
                ShowError("密码长度须为 8-72 个字符");
                return;
            }
            if (!string.Equals(password, confirm, StringComparison.Ordinal))
            {
                ShowError("两次输入的新密码不一致");
                return;
            }

            _busy = true;
            _submitButton.IsEnabled = false;
            _statusText.Foreground = new SolidColorBrush(Color.FromArgb(0x99, 0xFF, 0xFF, 0xFF));
            _statusText.Text = "正在重置密码…";

            string? error;
            if (_isRemote)
            {
                var (ok, err) = await Services.RemoteAuthService.PasswordResetAsync(_settings, account, rescueCode, password);
                error = ok ? null : err;
            }
            else
            {
                var (ok, err) = await DatabaseService.ResetPasswordByRescueCodeAsync(_settings, account, rescueCode, password);
                error = ok ? null : err;
            }

            if (error is not null)
            {
                ShowError(error);
                _busy = false;
                _submitButton.IsEnabled = true;
                return;
            }

            MessageBox.Show(this, "密码已重置，请使用新密码登录。", "找回密码", MessageBoxButton.OK, MessageBoxImage.Information);
            Close();
        }

        private void ShowError(string message)
        {
            _statusText.Foreground = new SolidColorBrush(Color.FromRgb(0xFF, 0x6B, 0x6B));
            _statusText.Text = message;
        }
    }
}
