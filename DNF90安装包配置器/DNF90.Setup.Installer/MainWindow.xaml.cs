using System.Diagnostics;
using System.IO;
using System.Windows;
using DNF90.Setup.Shared;

namespace DNF90.Setup.Installer;

public partial class MainWindow : Window
{
    private readonly string _setupPath;
    private SetupPayload? _payload;
    private bool _isInstalling;
    private string _displayName = "落叶DNF";

    public MainWindow(string setupPath)
    {
        InitializeComponent();
        _setupPath = setupPath;
        Loaded += MainWindow_Loaded;
    }

    private async void MainWindow_Loaded(object sender, RoutedEventArgs e)
    {
        try
        {
            _payload = await Task.Run(() => SetupPayload.Open(_setupPath));
            _displayName = _payload.Manifest.InstallFolderName;
            Title = _displayName + " 安装程序";
            ProductNameText.Text = _displayName;
            VersionText.Text = $"安装程序  ·  客户端版本 {_payload.Manifest.Version}";
            SummaryText.Text =
                $"{_payload.Packages.Count} 个安装分区，共 {_payload.Manifest.TotalUncompressedBytes / 1024.0 / 1024.0:0.0} MB。";
            PackageList.ItemsSource = _payload.Packages.Select(package => new
            {
                package.DisplayName,
                Detail = $"{package.FileCount} 个文件  ·  {package.CompressedBytes / 1024.0 / 1024.0:0.0} MB",
            }).ToList();
            InstallPathTextBox.Text = InstallPathHelper.NormalizeConfiguredDefault(
                _payload.Manifest.InstallFolderName);
            BrowsePathButton.IsEnabled = true;
            InstallButton.IsEnabled = true;
            StatusText.Text = "准备就绪";
        }
        catch (Exception ex)
        {
            StatusText.Text = "安装包读取失败";
            DetailText.Text = ex.Message;
            MessageBox.Show(this, "无法读取安装包：\n\n" + ex.Message,
                "安装包无效", MessageBoxButton.OK, MessageBoxImage.Error);
            CloseButton.IsEnabled = true;
        }
    }

    private void BrowseButton_Click(object sender, RoutedEventArgs e)
    {
        using var dialog = new System.Windows.Forms.FolderBrowserDialog
        {
            Description = $"选择{_displayName}客户端安装目录",
            UseDescriptionForTitle = true,
            ShowNewFolderButton = true,
            SelectedPath = Directory.Exists(InstallPathTextBox.Text)
                ? InstallPathTextBox.Text
                : Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
        };
        if (dialog.ShowDialog() == System.Windows.Forms.DialogResult.OK)
        {
            InstallPathTextBox.Text = dialog.SelectedPath;
        }
    }

    private async void InstallButton_Click(object sender, RoutedEventArgs e)
    {
        if (_payload is null || _isInstalling)
        {
            return;
        }

        var target = InstallPathTextBox.Text.Trim();
        if (string.IsNullOrWhiteSpace(target))
        {
            MessageBox.Show(this, "请选择安装目录。", "无法安装",
                MessageBoxButton.OK, MessageBoxImage.Warning);
            return;
        }

        try
        {
            var fullTarget = InstallPathHelper.NormalizeTarget(
                target,
                _payload.Manifest.InstallFolderName);
            InstallPathTextBox.Text = fullTarget;
            var root = Path.GetPathRoot(fullTarget);
            if (!string.IsNullOrEmpty(root) && Directory.Exists(root))
            {
                var available = new DriveInfo(root).AvailableFreeSpace;
                var required = _payload.Manifest.TotalUncompressedBytes + 256L * 1024 * 1024;
                if (available < required)
                {
                    MessageBox.Show(this,
                        $"目标磁盘空间不足。\n\n需要约 {required / 1024.0 / 1024.0 / 1024.0:0.0} GB，" +
                        $"当前可用 {available / 1024.0 / 1024.0 / 1024.0:0.0} GB。",
                        "空间不足", MessageBoxButton.OK, MessageBoxImage.Warning);
                    return;
                }
            }

            if (Directory.Exists(fullTarget) &&
                Directory.EnumerateFileSystemEntries(fullTarget).Any())
            {
                var answer = MessageBox.Show(this,
                    "目标目录不是空目录。安装会按客户端目录结构覆盖同名文件，" +
                    "但不会主动删除其他已有文件。\n\n是否继续？",
                    "确认安装", MessageBoxButton.YesNo, MessageBoxImage.Question);
                if (answer != MessageBoxResult.Yes)
                {
                    return;
                }
            }

            SetBusy(true);
            var progress = new Progress<InstallProgress>(value =>
            {
                StatusText.Text = value.Status;
                DetailText.Text = value.Detail;
                InstallProgress.Value = Math.Clamp(value.Percent, 0, 100);
            });
            await GameInstaller.InstallAsync(_payload, fullTarget, progress);
            SetCompletedState();

            var launcherPath = Path.Combine(
                fullTarget,
                _payload.Manifest.LauncherRelativePath.Replace('/', Path.DirectorySeparatorChar));
            if (ShortcutCheckBox.IsChecked == true && File.Exists(launcherPath))
            {
                ShortcutService.TryCreateDesktopShortcut(
                    fullTarget,
                    launcherPath,
                    Path.GetFileNameWithoutExtension(launcherPath));
            }

            StatusText.Text = "安装完成";
            DetailText.Text = fullTarget;
            MessageBox.Show(this, _displayName + "客户端安装完成。", "安装成功",
                MessageBoxButton.OK, MessageBoxImage.Information);

            if (LaunchCheckBox.IsChecked == true && File.Exists(launcherPath))
            {
                try
                {
                    Process.Start(new ProcessStartInfo(launcherPath)
                    {
                        WorkingDirectory = fullTarget,
                        UseShellExecute = true,
                    });
                }
                catch (Exception ex)
                {
                    MessageBox.Show(this,
                        "客户端已安装完成，但启动登录器失败：\n\n" + ex.Message,
                        "启动失败", MessageBoxButton.OK, MessageBoxImage.Warning);
                }
            }
        }
        catch (Exception ex)
        {
            MessageBox.Show(this, "安装失败：\n\n" + ex.Message,
                "安装失败", MessageBoxButton.OK, MessageBoxImage.Error);
            SetBusy(false);
        }
    }

    private void SetBusy(bool busy)
    {
        _isInstalling = busy;
        InstallButton.IsEnabled = !busy;
        CloseButton.IsEnabled = !busy;
        InstallPathTextBox.IsEnabled = !busy;
        BrowsePathButton.IsEnabled = !busy;
        ShortcutCheckBox.IsEnabled = !busy;
        LaunchCheckBox.IsEnabled = !busy;
    }

    private void SetCompletedState()
    {
        _isInstalling = false;
        InstallButton.IsEnabled = false;
        InstallButton.Content = "安装完成";
        CloseButton.IsEnabled = true;
        InstallPathTextBox.IsEnabled = false;
        BrowsePathButton.IsEnabled = false;
        ShortcutCheckBox.IsEnabled = false;
        LaunchCheckBox.IsEnabled = false;
    }

    private void CloseButton_Click(object sender, RoutedEventArgs e) => Close();

    protected override void OnClosing(System.ComponentModel.CancelEventArgs e)
    {
        if (_isInstalling)
        {
            e.Cancel = true;
            return;
        }
        _payload?.Dispose();
        base.OnClosing(e);
    }

}
