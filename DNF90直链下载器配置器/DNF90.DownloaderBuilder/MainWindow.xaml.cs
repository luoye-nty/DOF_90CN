using System.Diagnostics;
using System.IO;
using System.Reflection;
using System.Windows;
using DNF90.Downloader.Shared;

namespace DNF90.DownloaderBuilder;

public partial class MainWindow : Window
{
    public MainWindow()
    {
        InitializeComponent();
        OutputPathTextBox.Text = Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory),
            "DNF下载器输出");
    }

    private void SelectOutputButton_Click(object sender, RoutedEventArgs e)
    {
        using var dialog = new System.Windows.Forms.FolderBrowserDialog
        {
            Description = "选择生成的下载器保存目录",
            UseDescriptionForTitle = true,
            ShowNewFolderButton = true,
            SelectedPath = Directory.Exists(OutputPathTextBox.Text)
                ? OutputPathTextBox.Text
                : Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory),
        };
        if (dialog.ShowDialog() == System.Windows.Forms.DialogResult.OK)
        {
            OutputPathTextBox.Text = dialog.SelectedPath;
        }
    }

    private void GenerateButton_Click(object sender, RoutedEventArgs e)
    {
        try
        {
            var name = DownloaderPathHelper.NormalizeDownloaderName(
                DownloaderNameTextBox.Text);
            var disk = DownloaderPathHelper.NormalizeDisk(DiskTextBox.Text);
            var folderName = DownloaderPathHelper.NormalizeFolderName(
                FolderNameTextBox.Text);
            var urls = UrlsTextBox.Text
                .Split(new[] { "\r\n", "\n" }, StringSplitOptions.RemoveEmptyEntries)
                .Select(url => url.Trim())
                .Where(url => url.Length > 0)
                .ToList();
            if (urls.Count == 0)
            {
                throw new InvalidOperationException("请至少填写一个直链地址。");
            }
            foreach (var url in urls)
            {
                if (!Uri.TryCreate(url, UriKind.Absolute, out var uri) ||
                    (uri.Scheme != Uri.UriSchemeHttp && uri.Scheme != Uri.UriSchemeHttps))
                {
                    throw new InvalidOperationException(
                        "直链地址必须是 http 或 https 地址：\n" + url);
                }
            }

            var outputDirectory = Path.GetFullPath(OutputPathTextBox.Text.Trim());
            Directory.CreateDirectory(outputDirectory);
            var outputPath = Path.Combine(outputDirectory, name + ".exe");
            StatusText.Text = "正在生成下载器…";
            DetailText.Text = outputPath;

            var templatePath = DownloaderRuntimeTemplate.Extract();
            DownloaderConfigFooter.Generate(
                templatePath,
                outputPath,
                new DownloaderConfig
                {
                    DownloaderName = name,
                    DownloadUrls = urls,
                    DefaultDisk = disk,
                    DefaultDownloadFolderName = folderName,
                });

            StatusText.Text = "下载器生成完成";
            DetailText.Text = outputPath;
            OpenOutputButton.IsEnabled = true;
            MessageBox.Show(this,
                "直链下载器已生成：\n\n" + outputPath +
                $"\n\n共 {urls.Count} 个下载地址；默认保存到 {disk}:\\{folderName}。",
                "生成成功", MessageBoxButton.OK, MessageBoxImage.Information);
        }
        catch (Exception ex)
        {
            StatusText.Text = "生成失败";
            DetailText.Text = ex.Message;
            MessageBox.Show(this, "生成直链下载器失败：\n\n" + ex.Message,
                "生成失败", MessageBoxButton.OK, MessageBoxImage.Error);
        }
    }

    private void OpenOutputButton_Click(object sender, RoutedEventArgs e)
    {
        var path = OutputPathTextBox.Text.Trim();
        if (!Directory.Exists(path))
        {
            return;
        }
        Process.Start(new ProcessStartInfo("explorer.exe", "\"" + path + "\"")
        {
            UseShellExecute = true,
        });
    }

    protected override void OnClosed(EventArgs e)
    {
        DownloaderRuntimeTemplate.Cleanup();
        base.OnClosed(e);
    }
}

internal static class DownloaderRuntimeTemplate
{
    private const string ResourceName = "DNF90.DownloaderBuilder.Runtime.Downloader.exe";
    private static string? _temporaryDirectory;
    private static readonly object Sync = new();

    public static string Extract()
    {
        lock (Sync)
        {
            if (_temporaryDirectory is not null)
            {
                var existing = Path.Combine(_temporaryDirectory, "DNF直链下载器.exe");
                if (File.Exists(existing))
                {
                    return existing;
                }
            }

            using var resource = Assembly.GetExecutingAssembly()
                .GetManifestResourceStream(ResourceName);
            if (resource is null)
            {
                var adjacent = Path.Combine(AppContext.BaseDirectory, "DNF直链下载器.exe");
                if (File.Exists(adjacent))
                {
                    return adjacent;
                }
                throw new FileNotFoundException(
                    "配置器中没有找到下载器模板，请执行 Build-DownloaderSuite.ps1 重新构建。");
            }

            var directory = Path.Combine(
                Path.GetTempPath(),
                "DNF90DownloaderBuilder",
                Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(directory);
            var path = Path.Combine(directory, "DNF直链下载器.exe");
            using (var output = new FileStream(
                       path, FileMode.CreateNew, FileAccess.Write, FileShare.None))
            {
                resource.CopyTo(output);
            }
            _temporaryDirectory = directory;
            return path;
        }
    }

    public static void Cleanup()
    {
        lock (Sync)
        {
            if (_temporaryDirectory is null)
            {
                return;
            }
            try
            {
                Directory.Delete(_temporaryDirectory, recursive: true);
            }
            catch
            {
                // 程序退出时清理失败不影响生成结果。
            }
            _temporaryDirectory = null;
        }
    }
}
