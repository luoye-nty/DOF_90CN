using System;
using System.Collections.ObjectModel;
using System.IO;
using System.Linq;
using System.Windows;
using System.Windows.Controls;

namespace DNFLogin
{
    /// <summary>
    /// DLL 管理页：选择 DLL、勾选启停、移除；勾选列表在登录器启动 DNF.exe 后注入
    /// </summary>
    public partial class DllManageControl : UserControl
    {
        /// <summary>
        /// 2026-10-15：把「登录器同目录\LYPlugins」里的 DLL 默认全部加入注入列表（Enabled=true）。
        /// DLL 管理页已隐藏，玩家不再需要手动"选择 DLL"；已经在列表里的条目不会重复添加。
        /// </summary>
        public static void EnsureDefaultPluginDlls()
        {
            try
            {
                var dir = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "LYPlugins");
                if (!Directory.Exists(dir))
                {
                    return;
                }

                var added = 0;
                foreach (var file in Directory.GetFiles(dir, "*.dll"))
                {
                    var exists = false;
                    foreach (var dll in AppConfig.Settings.Dlls)
                    {
                        if (string.Equals(dll.Path, file, StringComparison.OrdinalIgnoreCase))
                        {
                            exists = true;
                            break;
                        }
                    }
                    if (exists)
                    {
                        continue;
                    }

                    AppConfig.Settings.Dlls.Add(new DllEntry { Path = file, Enabled = true });
                    ++added;
                }

                if (added > 0)
                {
                    AppConfig.Save();
                }
                App.LogStartup($"LYPlugins 默认注入：{dir}，本次新增 {added} 个 DLL（列表共 {AppConfig.Settings.Dlls.Count} 个）");
            }
            catch (Exception ex)
            {
                App.LogStartup("LYPlugins 扫描失败：" + ex.Message);
            }
        }

        /// <summary>
        /// 已选择的 DLL 列表（勾选状态存于 Enabled），游戏启动时由登录器读取并注入
        /// </summary>
        public readonly ObservableCollection<DllEntry> SelectedDlls = new();

        public DllManageControl()
        {
            InitializeComponent();

            // DLL 列表数据源 + 从配置回填
            DllList.ItemsSource = SelectedDlls;
            foreach (var dll in AppConfig.Settings.Dlls)
            {
                SelectedDlls.Add(new DllEntry { Path = dll.Path, Enabled = dll.Enabled });
            }

            UpdateDllCount();
        }

        /// <summary>选择 DLL 文件并加入注入列表</summary>
        private void SelectDllButton_Click(object sender, RoutedEventArgs e)
        {
            var dlg = new Microsoft.Win32.OpenFileDialog
            {
                Title = "选择 DLL",
                Filter = "动态链接库 (*.dll)|*.dll",
                CheckFileExists = true,
                Multiselect = true
            };

            if (dlg.ShowDialog() != true)
            {
                return;
            }

            foreach (var file in dlg.FileNames)
            {
                var exists = false;
                foreach (var item in SelectedDlls)
                {
                    if (string.Equals(item.Path, file, System.StringComparison.OrdinalIgnoreCase))
                    {
                        exists = true;
                        break;
                    }
                }
                if (exists)
                {
                    continue;
                }

                SelectedDlls.Add(new DllEntry { Path = file, Enabled = true });
            }

            SyncToConfig();
        }

        /// <summary>勾选状态变化：同步回配置并保存</summary>
        private void DllEnable_Changed(object sender, RoutedEventArgs e)
        {
            SyncToConfig();
        }

        /// <summary>移除 DLL：从列表与配置中删除</summary>
        private void DllRemove_Click(object sender, RoutedEventArgs e)
        {
            if (sender is FrameworkElement fe && fe.DataContext is DllEntry entry)
            {
                SelectedDlls.Remove(entry);
                SyncToConfig();
            }
        }

        /// <summary>
        /// DLL 注入接口：登录器在启动 DNF.exe 后调用，将勾选的 DLL 注入游戏进程。
        /// 需要管理员权限（DNF.exe 通常以管理员运行）。
        /// 外网版（EXTERNAL）**不注入任何玩家 DLL**：函数体在编译期被替换为空实现，
        /// 即便将来别处新增调用点、或玩家手工编辑共享配置里残留的勾选项，也无法注入。
        /// </summary>
        public static void InjectSelectedDlls(System.Diagnostics.Process process, IEnumerable<DllEntry> dlls)
        {
#if EXTERNAL
            // 外网版：玩家 DLL 注入在此被彻底禁用（编译期隔离，非仅 UI 隐藏）。
            System.Diagnostics.Debug.WriteLine("外网版已禁用玩家 DLL 注入，忽略 DLL 列表");
#else
            foreach (var dll in dlls)
            {
                if (!dll.Enabled || string.IsNullOrWhiteSpace(dll.Path) || !System.IO.File.Exists(dll.Path))
                {
                    continue;
                }

                var ok = DllInjector.Inject(process.Id, dll.Path);
                if (!ok)
                {
                    System.Diagnostics.Debug.WriteLine($"DLL 注入失败: {dll.Path}");
                }
            }
#endif
        }

        /// <summary>把 DLL 勾选列表同步回配置并保存</summary>
        private void SyncToConfig()
        {
            AppConfig.Settings.Dlls.Clear();
            foreach (var dll in SelectedDlls)
            {
                AppConfig.Settings.Dlls.Add(new DllEntry { Path = dll.Path, Enabled = dll.Enabled });
            }

            AppConfig.Save();
            UpdateDllCount();
        }

        private void UpdateDllCount()
        {
            var enabled = SelectedDlls.Count(d => d.Enabled);
            DllCountText.Text = $"共 {SelectedDlls.Count} 个 DLL，已启用 {enabled} 个";
        }
    }
}
