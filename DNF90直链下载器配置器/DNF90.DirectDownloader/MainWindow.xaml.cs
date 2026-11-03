using System.Diagnostics;
using System.IO;
using System.Windows;
using DNF90.Downloader.Shared;

namespace DNF90.DirectDownloader;

public partial class MainWindow : Window
{
    private readonly DownloaderConfig _config;
    private CancellationTokenSource? _cts;
    private bool _isDownloading;

    public MainWindow(DownloaderConfig config)
    {
        InitializeComponent();
        _config = config;
        Title = config.DownloaderName;
        DownloaderNameText.Text = config.DownloaderName;
        DownloadPathTextBox.Text = DownloaderPathHelper.DefaultDownloadDirectory(
            config.DefaultDisk,
            config.DefaultDownloadFolderName);
    }

    private void BrowseButton_Click(object sender, RoutedEventArgs e)
    {
        if (_isDownloading)
        {
            return;
        }

        using var dialog = new System.Windows.Forms.FolderBrowserDialog
        {
            Description = "选择文件下载保存目录",
            UseDescriptionForTitle = true,
            ShowNewFolderButton = true,
            SelectedPath = Directory.Exists(DownloadPathTextBox.Text)
                ? DownloadPathTextBox.Text
                : Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
        };
        if (dialog.ShowDialog() == System.Windows.Forms.DialogResult.OK)
        {
            DownloadPathTextBox.Text = dialog.SelectedPath;
        }
    }

    private async void StartButton_Click(object sender, RoutedEventArgs e)
    {
        if (_isDownloading)
        {
            return;
        }

        try
        {
            var targetDirectory = DownloaderPathHelper.NormalizeSelectedDirectory(
                DownloadPathTextBox.Text,
                _config.DefaultDownloadFolderName);
            DownloadPathTextBox.Text = targetDirectory;
            Directory.CreateDirectory(targetDirectory);

            _cts?.Dispose();
            _cts = new CancellationTokenSource();
            SetDownloading(true);
            var progress = new Progress<DownloadProgress>(OnProgress);
            var result = await DirectDownloadEngine.DownloadAllAsync(
                _config,
                targetDirectory,
                progress,
                _cts.Token);

            SetCompleted();
            StatusText.Text = "全部下载完成";
            DetailText.Text = targetDirectory;

            if (OpenInstallerCheckBox.IsChecked != true)
            {
                MessageBox.Show(this,
                    "全部文件已下载完成。\n\n下载位置：\n" + targetDirectory,
                    "下载完成", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }

            var executables = result.Items
                .Where(item =>
                    item.FilePath.EndsWith(".exe", StringComparison.OrdinalIgnoreCase) &&
                    File.Exists(item.FilePath))
                .ToList();
            if (executables.Count == 0)
            {
                MessageBox.Show(this,
                    "下载已完成，但下载内容中没有找到 EXE 安装程序。\n\n请手动解压安装。",
                    "请手动解压安装", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }
            if (executables.Count > 1)
            {
                MessageBox.Show(this,
                    "下载内容中包含多个 EXE 文件，请进入下载目录手动选择要运行的安装程序：\n\n" +
                    targetDirectory,
                    "请手动选择安装程序", MessageBoxButton.OK, MessageBoxImage.Information);
                return;
            }

            try
            {
                Process.Start(new ProcessStartInfo(executables[0].FilePath)
                {
                    WorkingDirectory = targetDirectory,
                    UseShellExecute = true,
                });
                Close();
            }
            catch (Exception ex)
            {
                MessageBox.Show(this,
                    "下载已完成，但启动安装程序失败：\n\n" + ex.Message,
                    "启动安装程序失败", MessageBoxButton.OK, MessageBoxImage.Warning);
            }
        }
        catch (OperationCanceledException)
        {
            SetReadyAfterFailure();
            StatusText.Text = "下载已取消";
            DetailText.Text = "可以修改下载位置后重新开始";
        }
        catch (Exception ex)
        {
            SetReadyAfterFailure();
            StatusText.Text = "下载失败";
            DetailText.Text = ex.Message;
            MessageBox.Show(this, "下载失败：\n\n" + ex.Message,
                "下载失败", MessageBoxButton.OK, MessageBoxImage.Error);
        }
    }

    private void OnProgress(DownloadProgress progress)
    {
        StatusText.Text = progress.Status;
        if (progress.Total > 0)
        {
            DownloadProgress.IsIndeterminate = false;
            DownloadProgress.Value = Math.Clamp(progress.OverallPercent, 0, 100);
            DetailText.Text =
                $"{progress.FileName}  " +
                $"{progress.Done / 1024.0 / 1024.0:0.0} / {progress.Total / 1024.0 / 1024.0:0.0} MB  " +
                $"{progress.SpeedMBps:0.0} MB/s";
        }
        else
        {
            DownloadProgress.IsIndeterminate = true;
            DetailText.Text = progress.FileName;
        }
    }

    private void SetDownloading(bool downloading)
    {
        _isDownloading = downloading;
        StartButton.IsEnabled = !downloading;
        DownloadPathTextBox.IsEnabled = !downloading;
        BrowseButton.IsEnabled = !downloading;
        OpenInstallerCheckBox.IsEnabled = !downloading;
        LeftButton.IsEnabled = true;
        LeftButton.Content = downloading ? "取消下载" : "关闭";
        DownloadProgress.IsIndeterminate = false;
        DownloadProgress.Value = 0;
    }

    private void SetCompleted()
    {
        _isDownloading = false;
        StartButton.IsEnabled = false;
        StartButton.Content = "下载完成";
        DownloadPathTextBox.IsEnabled = false;
        BrowseButton.IsEnabled = false;
        OpenInstallerCheckBox.IsEnabled = false;
        LeftButton.IsEnabled = true;
        LeftButton.Content = "关闭";
        DownloadProgress.IsIndeterminate = false;
        DownloadProgress.Value = 100;
    }

    private void SetReadyAfterFailure()
    {
        _isDownloading = false;
        StartButton.IsEnabled = true;
        StartButton.Content = "重新下载";
        DownloadPathTextBox.IsEnabled = true;
        BrowseButton.IsEnabled = true;
        OpenInstallerCheckBox.IsEnabled = true;
        LeftButton.IsEnabled = true;
        LeftButton.Content = "关闭";
        DownloadProgress.IsIndeterminate = false;
        DownloadProgress.Value = 0;
    }

    private void LeftButton_Click(object sender, RoutedEventArgs e)
    {
        if (_isDownloading)
        {
            LeftButton.IsEnabled = false;
            StatusText.Text = "正在取消下载…";
            _cts?.Cancel();
            return;
        }
        Close();
    }

    protected override void OnClosing(System.ComponentModel.CancelEventArgs e)
    {
        if (_isDownloading)
        {
            e.Cancel = true;
            return;
        }
        _cts?.Dispose();
        base.OnClosing(e);
    }
}
