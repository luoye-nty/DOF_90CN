using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using DNFLogin.Services;

namespace DNFLogin
{
    /// <summary>
    /// 设置页（原设置窗口迁移）：基础设置 / 导出导入 / 关于 三个子页面
    /// </summary>
    public partial class SettingsControl : UserControl
    {
        public SettingsControl()
        {
            InitializeComponent();

            // 「关于」页版本号：绑定到 LauncherVersion.DisplayText（= 常量的唯一显示出口）。
            // 为什么不在 XAML 里写死：这个版本号同时是登录器自检阶段上报给运维工具、
            // 用于版本校验的那个值。写死两份必然漂移，届时"关于页显示 1.5.5、
            // 服务端却收到别的值"会变成很难查的问题。
            AboutVersionText.Text = LauncherVersion.DisplayText;

            // 2026-10-15：本版不需要数据库备份/导入功能，隐藏"备份/导入数据"子导航
            NavTransfer.Visibility = Visibility.Collapsed;
            // 2026-10-15：「关于」页删除 DC 群那一行（折叠后 StackPanel 会跳过它，不会留空白行）
            AboutDcLinkText.Visibility = Visibility.Collapsed;

            // 子导航订阅（XAML 中未绑定事件）
            NavBase.Checked += SubNav_Checked;
            NavTransfer.Checked += SubNav_Checked;
            NavAbout.Checked += SubNav_Checked;

            // 基础设置回填
            ToggleExitPopup.IsChecked = AppConfig.Settings.ExitGamePopup;
            ToggleExitPopup.Checked += ToggleExitPopup_Changed;
            ToggleExitPopup.Unchecked += ToggleExitPopup_Changed;

            if (AppConfig.CloseAction == CloseAction.MinimizeToTray)
            {
                RadioTray.IsChecked = true;
            }
            else
            {
                RadioExit.IsChecked = true;
            }

            BackgroundFolderBox.Text = AppConfig.Settings.BackgroundFolder;

            if (AppConfig.Settings.BackgroundShuffle)
            {
                RadioBgShuffle.IsChecked = true;
            }
            else
            {
                RadioBgSequential.IsChecked = true;
            }
        }

        /// <summary>
        /// 「关于」页 Discord 邀请链接：用系统默认浏览器打开。
        /// 为什么必须自己处理 RequestNavigate：WPF 的 Hyperlink 默认把跳转交给宿主导航容器
        /// （NavigationWindow / Frame），本项目是普通 Window，没有 NavigationService，
        /// 不接管点击的话点下去毫无反应。这里显式 UseShellExecute 交给 Shell 打开，
        /// 无论玩家装的默认浏览器是什么都能正常拉起；失败时把链接显示出来让玩家手动访问。
        /// </summary>
        private void AboutDcLink_RequestNavigate(object sender, System.Windows.Navigation.RequestNavigateEventArgs e)
        {
            try
            {
                System.Diagnostics.Process.Start(new System.Diagnostics.ProcessStartInfo
                {
                    FileName = e.Uri.AbsoluteUri,
                    UseShellExecute = true
                });
            }
            catch (Exception ex)
            {
                MessageBox.Show("无法自动打开 Discord 链接，请手动在浏览器中访问：\n"
                                + e.Uri.AbsoluteUri + "\n\n原因：" + ex.Message,
                    "提示", MessageBoxButton.OK, MessageBoxImage.Warning);
            }

            // 标记已处理：阻止 WPF 再去走它自己那套导航（会抛"找不到导航容器"之类的异常）
            e.Handled = true;
        }

        /// <summary>
        /// 关窗兜底保存：改动本身已即时保存，这里再刷一次，避免丢失最后一次输入
        /// </summary>
        public void SaveState()
        {
            AppConfig.Settings.BackgroundFolder = BackgroundFolderBox.Text.Trim();
            AppConfig.Save();
        }

        // ── 子导航：基础设置 / 导出导入 / 关于 ────────────────

        private void SubNav_Checked(object sender, RoutedEventArgs e)
        {
            if (sender is not RadioButton rb)
            {
                return;
            }

            PageSettings.Visibility = ReferenceEquals(rb, NavBase) ? Visibility.Visible : Visibility.Collapsed;
            PageTransfer.Visibility = ReferenceEquals(rb, NavTransfer) ? Visibility.Visible : Visibility.Collapsed;
            PageAbout.Visibility = ReferenceEquals(rb, NavAbout) ? Visibility.Visible : Visibility.Collapsed;
        }

        // ── 基础设置 ─────────────────────────────────────

        private void ToggleExitPopup_Changed(object sender, RoutedEventArgs e)
        {
            AppConfig.Settings.ExitGamePopup = ToggleExitPopup.IsChecked == true;
            AppConfig.Save();
        }

        /// <summary>关闭窗口行为：最小化到托盘 / 退出启动器</summary>
        private void CloseBehavior_Checked(object sender, RoutedEventArgs e)
        {
            AppConfig.Settings.CloseAction = RadioTray.IsChecked == true
                ? nameof(CloseAction.MinimizeToTray)
                : nameof(CloseAction.Exit);
            AppConfig.Save();
        }

        /// <summary>背景目录输入即时保存</summary>
        private void BackgroundFolderBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            AppConfig.Settings.BackgroundFolder = BackgroundFolderBox.Text.Trim();
            AppConfig.Save();
        }

        /// <summary>选择背景图片目录</summary>
        private void BrowseBackgroundButton_Click(object sender, RoutedEventArgs e)
        {
            var dlg = new Microsoft.Win32.OpenFolderDialog
            {
                Title = "选择背景图片目录",
                Multiselect = false
            };
            if (!string.IsNullOrWhiteSpace(BackgroundFolderBox.Text.Trim()))
            {
                dlg.FolderName = BackgroundFolderBox.Text.Trim();
            }

            if (dlg.ShowDialog() == true)
            {
                BackgroundFolderBox.Text = dlg.FolderName;
            }
        }

        /// <summary>背景轮播方式：顺序 / 随机</summary>
        private void BgOrder_Checked(object sender, RoutedEventArgs e)
        {
            AppConfig.Settings.BackgroundShuffle = RadioBgShuffle.IsChecked == true;
            AppConfig.Save();
        }

        /// <summary>清除所有配置（危险操作）：删除配置文件后退出启动器</summary>
        private void ClearAllConfigButton_Click(object sender, RoutedEventArgs e)
        {
            var r = MessageBox.Show(
                "确定要清除所有配置吗？\n\n将删除账号、密码、服务器配置、DLL 列表等全部本地数据，且不可恢复！",
                "清除所有配置",
                MessageBoxButton.YesNo,
                MessageBoxImage.Warning);
            if (r != MessageBoxResult.Yes)
            {
                return;
            }

            try
            {
                AppConfig.Save();
                if (File.Exists(AppConfig.ConfigPath))
                {
                    File.Delete(AppConfig.ConfigPath);
                }
            }
            catch (Exception)
            {
                MessageBox.Show("清除配置失败，请先关闭正在使用登录器配置的进程后重试。", "清除所有配置", MessageBoxButton.OK, MessageBoxImage.Error);
                return;
            }

            // 阻止退出流程（关窗兜底 SaveState 等）把旧配置重新写回磁盘
            AppConfig.ConfigCleared = true;

            // 清除成功后自动重启登录器（以全新配置状态启动）
            try
            {
                // 先释放单实例互斥，否则新实例会因"已有实例"而立即退出
                ((App)Application.Current).PrepareForRestart();

                System.Diagnostics.Process.Start(new System.Diagnostics.ProcessStartInfo
                {
                    FileName = Environment.ProcessPath,
                    UseShellExecute = true
                });
            }
            catch (Exception)
            {
                MessageBox.Show("清除配置成功，但自动重启失败，请手动重新打开登录器。",
                    "清除所有配置", MessageBoxButton.OK, MessageBoxImage.Warning);
            }

            Application.Current.Shutdown();
        }

        // ── 导出 / 导入 ─────────────────────────────────

        private async void ExportDbButton_Click(object sender, RoutedEventArgs e)
        {
            var s = AppConfig.Settings;

            // 源库可用性检查
            if (DbDumpService.IsSqlite(s))
            {
                var dbFile = DatabaseService.ResolveSqlitePath(s);
                if (string.IsNullOrWhiteSpace(dbFile) || !File.Exists(dbFile))
                {
                    MessageBox.Show("未找到本地数据库文件，请先在游戏配置页确认数据库配置。",
                        "导出失败", MessageBoxButton.OK, MessageBoxImage.Error);
                    return;
                }
            }
            else
            {
                var ok = await DatabaseService.TestConnectionAsync(s);
                if (!ok)
                {
                    MessageBox.Show("MySQL 连接失败，请先在游戏配置页确认数据库配置。",
                        "导出失败", MessageBoxButton.OK, MessageBoxImage.Error);
                    return;
                }
            }

            var dlg = new Microsoft.Win32.SaveFileDialog
            {
                Title = "导出数据库备份",
                Filter = "数据库备份 (*.zip)|*.zip",
                FileName = $"dnf_db_backup_{DateTime.Now:yyyyMMdd_HHmmss}.zip"
            };
            if (dlg.ShowDialog() != true)
            {
                return;
            }

            SetDbTransferBusy(true);
            AppendDbTransferLog("开始导出数据库备份 ...");
            try
            {
                var result = await DbDumpService.ExportAsync(s, dlg.FileName,
                    msg => Dispatcher.Invoke(() => AppendDbTransferLog(msg)));
                AppendDbTransferLog($"导出完成：{result.TableCount} 张表 / {result.RowCount} 行");
                MessageBox.Show($"导出完成：{result.TableCount} 张表 / {result.RowCount} 行\n{result.ZipPath}",
                    "导出完成", MessageBoxButton.OK, MessageBoxImage.Information);
            }
            catch (Exception ex)
            {
                AppendDbTransferLog("导出异常：" + ex.Message);
                MessageBox.Show("导出失败，请检查磁盘空间与文件占用情况后重试。", "导出失败", MessageBoxButton.OK, MessageBoxImage.Error);
            }
            finally
            {
                SetDbTransferBusy(false);
            }
        }

        private async void ImportDbButton_Click(object sender, RoutedEventArgs e)
        {
            var dlg = new Microsoft.Win32.OpenFileDialog
            {
                Title = "选择数据库备份",
                Filter = "数据库备份 (*.zip)|*.zip",
                CheckFileExists = true
            };
            if (dlg.ShowDialog() != true)
            {
                return;
            }
            var zipPath = dlg.FileName;
            var s = AppConfig.Settings;

            SetDbTransferBusy(true);
            AppendDbTransferLog("开始导入数据库备份 ...");
            try
            {
                // 1. 校验备份并读取元数据
                var meta = DbDumpService.ReadMeta(zipPath, out var readError);
                if (meta == null)
                {
                    MessageBox.Show("备份文件无效：" + readError, "导入失败", MessageBoxButton.OK, MessageBoxImage.Error);
                    return;
                }
                AppendDbTransferLog($"备份信息：来源 {meta.SourceType}，{meta.Tables.Count} 张表，导出于 {meta.ExportedAt}");

                // 2. 目标库可用性检查
                if (DbDumpService.IsSqlite(s))
                {
                    var dbFile = DatabaseService.ResolveSqlitePath(s);
                    if (string.IsNullOrWhiteSpace(dbFile) || !File.Exists(dbFile))
                    {
                        MessageBox.Show("未找到本地数据库文件，请先在游戏配置页确认数据库配置。",
                            "导入失败", MessageBoxButton.OK, MessageBoxImage.Error);
                        return;
                    }
                }
                else
                {
                    var ok = await DatabaseService.TestConnectionAsync(s);
                    if (!ok)
                    {
                        MessageBox.Show("MySQL 连接失败，请先在游戏配置页确认数据库配置。",
                            "导入失败", MessageBoxButton.OK, MessageBoxImage.Error);
                        return;
                    }
                }

                // 3. 展示目标库信息并询问
                var targetDesc = DbDumpService.DescribeTarget(s);
                var ask = MessageBox.Show(
                    $"目标库：{targetDesc}\n备份原库：{meta.SourceType}（导出于 {meta.ExportedAt}）\n\n是否继续导入？",
                    "确认导入", MessageBoxButton.YesNo, MessageBoxImage.Question);
                if (ask != MessageBoxResult.Yes)
                {
                    AppendDbTransferLog("用户取消导入。");
                    return;
                }

                // 4. 冲突检测：同名表 → 覆盖 / 跳过 / 改名
                var existing = await DbDumpService.ListTablesAsync(s);
                var conflicts = meta.Tables.Select(t => t.Name).Where(existing.Contains).ToList();
                var conflictAction = DbDumpConflictAction.Overwrite;
                if (conflicts.Count > 0)
                {
                    var msg = $"目标库存在 {conflicts.Count} 张同名表：\n{string.Join("、", conflicts)}\n\n" +
                              "请选择冲突处理方式：\n「是」 覆盖重建\n「否」 跳过这些表\n「取消」 改名导入（表名加时间后缀）";
                    var choice = MessageBox.Show(msg, "冲突处理", MessageBoxButton.YesNoCancel, MessageBoxImage.Question);
                    if (choice == MessageBoxResult.Cancel)
                    {
                        conflictAction = DbDumpConflictAction.Rename;
                    }
                    else if (choice == MessageBoxResult.No)
                    {
                        conflictAction = DbDumpConflictAction.Skip;
                    }
                    else
                    {
                        conflictAction = DbDumpConflictAction.Overwrite;
                    }
                }

                // 5. 逐表导入（每表一个事务）
                var result = new DbDumpImportResult();
                var renameSuffix = DateTime.Now.ToString("yyyyMMdd_HHmmss");
                foreach (var t in meta.Tables)
                {
                    var isConflict = conflicts.Contains(t.Name);
                    if (isConflict && conflictAction == DbDumpConflictAction.Skip)
                    {
                        result.SkippedTables.Add(t.Name);
                        AppendDbTransferLog("跳过冲突表 " + t.Name);
                        continue;
                    }

                    var targetName = t.Name;
                    var drop = false;
                    if (isConflict)
                    {
                        if (conflictAction == DbDumpConflictAction.Rename)
                        {
                            targetName = t.Name + "_import_" + renameSuffix;
                        }
                        else
                        {
                            drop = true; // 覆盖：DROP TABLE IF EXISTS 重建
                        }
                    }

                    AppendDbTransferLog($"导入表 {t.Name} ...");
                    var (success, error) = await Task.Run(() => DbDumpService.ImportTableAsync(
                        s, zipPath, t.Name, targetName, drop,
                        msg => Dispatcher.Invoke(() => AppendDbTransferLog(msg))));
                    if (success)
                    {
                        result.Succeeded++;
                        AppendDbTransferLog($"导入成功：{t.Name}" + (targetName != t.Name ? " → " + targetName : ""));
                    }
                    else
                    {
                        result.Failed++;
                        result.FailedTables.Add(t.Name);
                        AppendDbTransferLog($"导入失败：{t.Name} —— {error}");
                        var cont = MessageBox.Show($"表 {t.Name} 导入失败：\n{error}\n\n是否继续导入其余表？",
                            "导入失败", MessageBoxButton.YesNo, MessageBoxImage.Warning);
                        if (cont != MessageBoxResult.Yes)
                        {
                            AppendDbTransferLog("用户选择停止，中止后续导入。");
                            break;
                        }
                    }
                }

                AppendDbTransferLog($"导入完成：成功 {result.Succeeded} 表 / 失败 {result.Failed} 表"
                    + (result.SkippedTables.Count > 0 ? $" / 跳过 {result.SkippedTables.Count} 表" : ""));
                MessageBox.Show($"导入完成：成功 {result.Succeeded} 表 / 失败 {result.Failed} 表"
                    + (result.SkippedTables.Count > 0 ? $"\n跳过 {result.SkippedTables.Count} 表" : "")
                    + (result.FailedTables.Count > 0 ? "\n\n失败表：" + string.Join("、", result.FailedTables) : ""),
                    "导入完成", MessageBoxButton.OK,
                    result.Failed > 0 ? MessageBoxImage.Warning : MessageBoxImage.Information);
            }
            catch (Exception ex)
            {
                AppendDbTransferLog("导入异常：" + ex.Message);
                MessageBox.Show("导入失败，请检查备份文件是否完整、目标数据库是否可写后重试。", "导入失败", MessageBoxButton.OK, MessageBoxImage.Error);
            }
            finally
            {
                SetDbTransferBusy(false);
            }
        }

        /// <summary>追加一行到导出/导入日志框（UI 线程调用）</summary>
        private void AppendDbTransferLog(string line)
        {
            DbTransferLogBox.AppendText($"[{DateTime.Now:HH:mm:ss}] {line}\r\n");
            DbTransferLogBox.ScrollToEnd();
        }

        private void SetDbTransferBusy(bool busy)
        {
            ExportDbButton.IsEnabled = !busy;
            ImportDbButton.IsEnabled = !busy;
        }
    }
}
