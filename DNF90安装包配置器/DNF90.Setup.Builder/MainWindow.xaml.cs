using System.Diagnostics;
using System.IO;
using System.Windows;
using DNF90.Setup.Shared;

namespace DNF90.Setup.Builder;

public partial class MainWindow : Window
{
    private ClientAnalysis? _analysis;
    private bool _busy;
    private readonly InstallerBuilderSettings? _startupSettings;
    private sealed record LauncherCandidate(string FileName, string FullPath);

    public MainWindow()
    {
        InitializeComponent();
        _startupSettings = InstallerBuilderSettingsStore.Load();
        if (_startupSettings is not null)
        {
            ApplySettings(_startupSettings);
        }
        Loaded += MainWindow_Loaded;
    }

    private async void MainWindow_Loaded(object sender, RoutedEventArgs e)
    {
        if (_startupSettings is null ||
            string.IsNullOrWhiteSpace(_startupSettings.ClientPath) ||
            !Directory.Exists(_startupSettings.ClientPath))
        {
            return;
        }

        await LoadClientDirectoryAsync(
            _startupSettings.ClientPath,
            _startupSettings.LauncherPath,
            restoring: true);
    }

    private async void SelectClientButton_Click(object sender, RoutedEventArgs e)
    {
        if (_busy)
        {
            return;
        }

        using var dialog = new System.Windows.Forms.FolderBrowserDialog
        {
            Description = "选择包含 DNF.exe、ImagePacks2、Script.pvf 的游戏客户端目录",
            UseDescriptionForTitle = true,
            ShowNewFolderButton = false,
            SelectedPath = Directory.Exists(ClientPathTextBox.Text)
                ? ClientPathTextBox.Text
                : Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
        };
        if (dialog.ShowDialog() != System.Windows.Forms.DialogResult.OK)
        {
            return;
        }

        await LoadClientDirectoryAsync(dialog.SelectedPath, null, restoring: false);
    }

    private async Task LoadClientDirectoryAsync(
        string selectedPath,
        string? preferredLauncherPath,
        bool restoring)
    {
        try
        {
            SetBusy(true);
            StatusText.Text = restoring ? "正在恢复上次参数…" : "正在扫描客户端…";
            DetailText.Text = selectedPath;
            LauncherComboBox.ItemsSource = null;
            LauncherComboBox.SelectedItem = null;
            _analysis = await Task.Run(() => ClientPackager.Analyze(selectedPath));
            ClientPathTextBox.Text = _analysis.ClientRoot;
            ClientSummaryText.Text =
                $"共 {_analysis.TotalFiles:N0} 个文件，{FormatBytes(_analysis.TotalBytes)}。" +
                $"ImagePacks2 {_analysis.ImagePackFiles.Count:N0} 个，" +
                $"其他剩余文件 {_analysis.OtherFiles.Count:N0} 个。";
            var launcherCandidates = Directory.EnumerateFiles(
                    _analysis.ClientRoot, "*.exe", SearchOption.TopDirectoryOnly)
                .Where(path =>
                    !string.Equals(Path.GetFileName(path), "DNF.exe", StringComparison.OrdinalIgnoreCase) &&
                    !string.Equals(Path.GetFileName(path), "NoPack.exe", StringComparison.OrdinalIgnoreCase))
                .Select(path => new LauncherCandidate(Path.GetFileName(path), Path.GetFullPath(path)))
                .OrderBy(candidate => candidate.FileName, StringComparer.OrdinalIgnoreCase)
                .ToList();
            LauncherComboBox.ItemsSource = launcherCandidates;
            LauncherComboBox.SelectedItem =
                launcherCandidates.FirstOrDefault(candidate =>
                    !string.IsNullOrWhiteSpace(preferredLauncherPath) &&
                    string.Equals(candidate.FullPath, preferredLauncherPath, StringComparison.OrdinalIgnoreCase)) ??
                launcherCandidates.FirstOrDefault(candidate =>
                    candidate.FileName.Contains("登录器", StringComparison.OrdinalIgnoreCase)) ??
                (launcherCandidates.Count == 1 ? launcherCandidates[0] : null);
            if (string.IsNullOrWhiteSpace(OutputPathTextBox.Text))
            {
                OutputPathTextBox.Text = Path.Combine(
                    Path.GetDirectoryName(_analysis.ClientRoot.TrimEnd(
                        Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar))!,
                    "DNF90安装包输出");
            }

            if ((!restoring || string.IsNullOrWhiteSpace(VersionTextBox.Text)) &&
                LauncherComboBox.SelectedItem is LauncherCandidate selectedLauncher)
            {
                var productVersion = FileVersionInfo.GetVersionInfo(selectedLauncher.FullPath).ProductVersion;
                if (!string.IsNullOrWhiteSpace(productVersion))
                {
                    VersionTextBox.Text = productVersion.Split('+')[0].Trim();
                }
            }
            StatusText.Text = launcherCandidates.Count == 0
                ? "未找到可选的登录器 EXE"
                : restoring ? "已恢复上次参数" : "客户端扫描完成";
            BuildButton.IsEnabled = LauncherComboBox.SelectedItem is not null;
        }
        catch (Exception ex)
        {
            _analysis = null;
            LauncherComboBox.ItemsSource = null;
            LauncherComboBox.SelectedItem = null;
            ClientSummaryText.Text = ex.Message;
            StatusText.Text = restoring ? "上次保存的客户端目录不可用" : "客户端目录无效";
            if (!restoring)
            {
                MessageBox.Show(this, ex.Message, "无法读取客户端",
                    MessageBoxButton.OK, MessageBoxImage.Warning);
            }
        }
        finally
        {
            SetBusy(false);
        }
    }

    private void SelectOutputButton_Click(object sender, RoutedEventArgs e)
    {
        if (_busy)
        {
            return;
        }

        using var dialog = new System.Windows.Forms.FolderBrowserDialog
        {
            Description = "选择安装包输出目录",
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

    private async void BuildButton_Click(object sender, RoutedEventArgs e)
    {
        if (_busy || _analysis is null)
        {
            return;
        }

        if (string.IsNullOrWhiteSpace(OutputPathTextBox.Text))
        {
            MessageBox.Show(this, "请选择输出目录。", "无法生成",
                MessageBoxButton.OK, MessageBoxImage.Warning);
            return;
        }

        var releaseContent = GetSelectedReleaseContent();
        var launcherPath = LauncherComboBox.SelectedValue as string;
        if (string.IsNullOrWhiteSpace(launcherPath))
        {
            MessageBox.Show(this, "请选择客户端登录器文件。", "无法生成",
                MessageBoxButton.OK, MessageBoxImage.Warning);
            return;
        }
        long splitMb = 4096;
        var usesImagePacks = releaseContent is ReleaseContent.Full or ReleaseContent.ImagePacksOnly;
        if (usesImagePacks &&
            (!long.TryParse(SplitSizeTextBox.Text.Trim(), out splitMb) || splitMb < 64))
        {
            MessageBox.Show(this, "ImagePacks2 分包目标必须是大于等于 64 的整数（MB）。",
                "参数错误", MessageBoxButton.OK, MessageBoxImage.Warning);
            return;
        }

        try
        {
            SetBusy(true);
            BuildProgress.Value = 0;
            StatusText.Text = "正在准备安装器模板…";
            var templatePath = await Task.Run(EmbeddedInstallerRuntime.Extract);
            var options = new BuildOptions(
                _analysis.ClientRoot,
                OutputPathTextBox.Text.Trim(),
                SetupNameTextBox.Text.Trim(),
                VersionTextBox.Text.Trim(),
                PackagePrefixTextBox.Text.Trim(),
                launcherPath,
                InstallFolderNameTextBox.Text.Trim(),
                releaseContent,
                splitMb * 1024L * 1024L,
                templatePath);
            var progress = new Progress<BuilderProgress>(value =>
            {
                StatusText.Text = value.Status;
                DetailText.Text = value.Detail;
                BuildProgress.Value = Math.Clamp(value.Percent, 0, 100);
            });

            var result = await ClientPackager.BuildAsync(_analysis, options, progress);
            string? settingsWarning = null;
            try
            {
                InstallerBuilderSettingsStore.Save(CaptureSettings());
            }
            catch (Exception settingsError)
            {
                settingsWarning = "安装包已生成，但保存 INI 参数失败：\n" + settingsError.Message;
            }
            StatusText.Text = "安装程序生成完成";
            DetailText.Text = result.SetupPath;
            OpenOutputButton.IsEnabled = true;
            SetBusy(false);
            MessageBox.Show(this,
                "安装程序已生成：\n\n" + result.SetupPath +
                "\n\n分卷 ZIP 与安装器位于同一目录：\n" + (result.PackagesDirectory ?? OutputPathTextBox.Text) +
                "\n\n分发时请保持安装器 EXE 与对应 ZIP 文件在一起。" +
                (settingsWarning is null ? "" : "\n\n" + settingsWarning),
                "生成成功", MessageBoxButton.OK, MessageBoxImage.Information);
        }
        catch (Exception ex)
        {
            StatusText.Text = "生成失败";
            DetailText.Text = ex.Message;
            MessageBox.Show(this, "生成安装程序失败：\n\n" + ex.Message,
                "生成失败", MessageBoxButton.OK, MessageBoxImage.Error);
            SetBusy(false);
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

    private void PackageNameInput_TextChanged(object sender, System.Windows.Controls.TextChangedEventArgs e)
    {
        if (PackageNameExampleText is null || VersionTextBox is null || PackagePrefixTextBox is null)
        {
            return;
        }
        var prefix = string.IsNullOrWhiteSpace(PackagePrefixTextBox.Text)
            ? "DNF_LY"
            : PackagePrefixTextBox.Text.Trim();
        var version = string.IsNullOrWhiteSpace(VersionTextBox.Text)
            ? "版本号"
            : VersionTextBox.Text.Trim();
        PackageNameExampleText.Text = $"示例：{prefix}_{version}_001.zip";
    }

    private void LauncherComboBox_SelectionChanged(
        object sender,
        System.Windows.Controls.SelectionChangedEventArgs e)
    {
        if (BuildButton is not null)
        {
            BuildButton.IsEnabled = !_busy && _analysis is not null &&
                                    LauncherComboBox.SelectedItem is not null;
        }
    }

    private void ReleaseContent_Changed(object sender, RoutedEventArgs e)
    {
        if (SplitSizeTextBox is null || FullReleaseRadio is null || PvfReleaseRadio is null ||
            NpkReleaseRadio is null || AccessoryReleaseRadio is null)
        {
            return;
        }

        var content = GetSelectedReleaseContent();
        SplitSizeTextBox.IsEnabled = !_busy &&
                                     content is ReleaseContent.Full or ReleaseContent.ImagePacksOnly;
    }

    private ReleaseContent GetSelectedReleaseContent()
    {
        if (PvfReleaseRadio.IsChecked == true)
        {
            return ReleaseContent.PvfOnly;
        }
        if (NpkReleaseRadio.IsChecked == true)
        {
            return ReleaseContent.ImagePacksOnly;
        }
        if (AccessoryReleaseRadio.IsChecked == true)
        {
            return ReleaseContent.RemainingOnly;
        }
        return ReleaseContent.Full;
    }

    private void ApplySettings(InstallerBuilderSettings settings)
    {
        if (!string.IsNullOrWhiteSpace(settings.ClientPath))
        {
            ClientPathTextBox.Text = settings.ClientPath;
        }
        if (!string.IsNullOrWhiteSpace(settings.OutputPath))
        {
            OutputPathTextBox.Text = settings.OutputPath;
        }
        if (!string.IsNullOrWhiteSpace(settings.Version))
        {
            VersionTextBox.Text = settings.Version;
        }
        if (!string.IsNullOrWhiteSpace(settings.ImagePackTargetMb))
        {
            SplitSizeTextBox.Text = settings.ImagePackTargetMb;
        }
        if (!string.IsNullOrWhiteSpace(settings.SetupName))
        {
            SetupNameTextBox.Text = settings.SetupName;
        }
        if (!string.IsNullOrWhiteSpace(settings.PackagePrefix))
        {
            PackagePrefixTextBox.Text = settings.PackagePrefix;
        }
        if (!string.IsNullOrWhiteSpace(settings.InstallFolderName))
        {
            InstallFolderNameTextBox.Text = settings.InstallFolderName;
        }

        FullReleaseRadio.IsChecked = settings.ReleaseContent == ReleaseContent.Full;
        PvfReleaseRadio.IsChecked = settings.ReleaseContent == ReleaseContent.PvfOnly;
        NpkReleaseRadio.IsChecked = settings.ReleaseContent == ReleaseContent.ImagePacksOnly;
        AccessoryReleaseRadio.IsChecked = settings.ReleaseContent == ReleaseContent.RemainingOnly;
    }

    private InstallerBuilderSettings CaptureSettings()
    {
        return new InstallerBuilderSettings
        {
            ClientPath = _analysis?.ClientRoot ?? ClientPathTextBox.Text.Trim(),
            LauncherPath = LauncherComboBox.SelectedValue as string ?? "",
            OutputPath = OutputPathTextBox.Text.Trim(),
            Version = VersionTextBox.Text.Trim(),
            ImagePackTargetMb = SplitSizeTextBox.Text.Trim(),
            SetupName = SetupNameTextBox.Text.Trim(),
            PackagePrefix = PackagePrefixTextBox.Text.Trim(),
            InstallFolderName = InstallFolderNameTextBox.Text.Trim(),
            ReleaseContent = GetSelectedReleaseContent(),
        };
    }

    private void SetBusy(bool busy)
    {
        _busy = busy;
        BuildButton.IsEnabled = !busy && _analysis is not null &&
                                LauncherComboBox.SelectedItem is not null;
        VersionTextBox.IsEnabled = !busy;
        SplitSizeTextBox.IsEnabled = !busy &&
                                     GetSelectedReleaseContent() is ReleaseContent.Full or ReleaseContent.ImagePacksOnly;
        SetupNameTextBox.IsEnabled = !busy;
        PackagePrefixTextBox.IsEnabled = !busy;
        InstallFolderNameTextBox.IsEnabled = !busy;
        LauncherComboBox.IsEnabled = !busy;
        FullReleaseRadio.IsEnabled = !busy;
        PvfReleaseRadio.IsEnabled = !busy;
        NpkReleaseRadio.IsEnabled = !busy;
        AccessoryReleaseRadio.IsEnabled = !busy;
    }

    protected override void OnClosed(EventArgs e)
    {
        EmbeddedInstallerRuntime.Cleanup();
        base.OnClosed(e);
    }

    private static string FormatBytes(long bytes)
    {
        if (bytes >= 1024L * 1024 * 1024)
        {
            return $"{bytes / 1024.0 / 1024.0 / 1024.0:0.00} GB";
        }
        return $"{bytes / 1024.0 / 1024.0:0.0} MB";
    }
}
