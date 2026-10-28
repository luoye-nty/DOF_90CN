using System;
using System.Diagnostics;
using System.IO;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;

namespace DNFLogin
{
    public partial class App : System.Windows.Application
    {
        private Mutex? _mutex;
        private EventWaitHandle? _showSignal;

        protected override void OnStartup(StartupEventArgs e)
        {
            base.OnStartup(e);

            // 全局异常兜底：未处理异常记录日志并阻止直接闪退（async void / 后台线程异常不再静默崩溃）
            DispatcherUnhandledException += OnDispatcherUnhandledException;
            TaskScheduler.UnobservedTaskException += OnUnobservedTaskException;

            // 单实例互斥：已存在实例时通知其弹出窗口，然后退出本次启动
            _mutex = new Mutex(true, @"Local\DNFLogin_SingleInstance", out bool createdNew);
            if (!createdNew)
            {
                try
                {
                    using var signal = EventWaitHandle.OpenExisting(@"Local\DNFLogin_ShowSignal");
                    signal.Set();
                }
                catch
                {
                    // 已有实例信号不可达时直接退出
                }

                Shutdown();
                return;
            }

            // 启动时读取持久化配置（AppData\DNFLogin\config.json）
            AppConfig.Load();

            // 启动自检：JSON 缺少新字段（如 CloseActionAsked）时补齐默认值
            AppConfig.SelfCheck();

#if EXTERNAL
            // M4 外网版启动自检：client.yh 验签 + target 版本一致性（同目录优先并转移覆盖到
            // %ProgramData%\DNFLogin\secure\，失败弹窗说明并拒绝启动）。仅 EXTERNAL 编译进外网版，
            // Free 版不经过该校验、零影响。置于窗口创建之前，实现早期拦截。
            if (!Services.ClientYh.ClientYhGuard.RunStartupCheck())
            {
                Shutdown();
                return;
            }
#endif

            // 若上次开启了按键连发，登录器启动即常驻连发引擎（登录器即连发本身）
            // ASSIST-OFF-EXTERNAL（2026-09-20）：按键连发属于"游戏助手"功能，外网版整块停用
            // （外网版不能再依赖"页面被折叠所以用户开不了"——配置是共享的，开关残留会照样启动引擎）。
            // 恢复：删掉这对 #if。
#if !EXTERNAL
            if (AppConfig.Settings.AutoFireEnabled)
            {
                Services.AutoFireEngine.Start(AppConfig.Settings.AutoFireKeys);
            }
#endif

            // 监听"已有实例请求弹出窗口"信号，收到后把主窗口带到前台
            _showSignal = new EventWaitHandle(false, EventResetMode.AutoReset, @"Local\DNFLogin_ShowSignal");
            var watcher = new Thread(() =>
            {
                try
                {
                    while (_showSignal.WaitOne())
                    {
                        Dispatcher.Invoke(BringCurrentWindowToFront);
                    }
                }
                catch
                {
                    // 退出时句柄被关闭，监听线程随之结束
                }
            })
            { IsBackground = true };
            watcher.Start();
        }

        private static string CrashLogPath => Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin", "crash.log");

        private static string StartupLogPath => Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin", "startup.log");

        /// <summary>
        /// 启动流程日志：追加写入 %AppData%\DNFLogin\startup.log，用于排查窗口切换/自检跳转问题。
        /// </summary>
        public static void LogStartup(string message)
        {
            try
            {
                Directory.CreateDirectory(Path.GetDirectoryName(StartupLogPath)!);
                File.AppendAllText(StartupLogPath,
                    $"[{DateTime.Now:HH:mm:ss.fff}] {message}\n");
            }
            catch
            {
                // 日志写入失败不影响主流程
            }
        }

        private void OnDispatcherUnhandledException(object sender, System.Windows.Threading.DispatcherUnhandledExceptionEventArgs e)
        {
            try
            {
                Directory.CreateDirectory(Path.GetDirectoryName(CrashLogPath)!);
                File.AppendAllText(CrashLogPath,
                    $"[{DateTime.Now:yyyy-MM-dd HH:mm:ss}] {e.Exception}\n");
            }
            catch
            {
                // 日志写入失败不阻断
            }

            MessageBox.Show("程序发生未处理异常，操作已中断。\n\n" + e.Exception.Message +
                            "\n\n详细错误已记录到：" + CrashLogPath,
                "异常提示", MessageBoxButton.OK, MessageBoxImage.Warning);
            e.Handled = true;
        }

        private void OnUnobservedTaskException(object? sender, UnobservedTaskExceptionEventArgs e)
        {
            try
            {
                Directory.CreateDirectory(Path.GetDirectoryName(CrashLogPath)!);
                File.AppendAllText(CrashLogPath,
                    $"[{DateTime.Now:yyyy-MM-dd HH:mm:ss}] [UnobservedTask] {e.Exception}\n");
            }
            catch
            {
                // 日志写入失败不阻断
            }

            e.SetObserved();
        }

        private static void BringCurrentWindowToFront()
        {
            var main = Current.MainWindow;
            switch (main)
            {
                case LoginWindow login:
                    login.BringToFront();
                    break;
                case null:
                    return;
                default:
                    if (main.WindowState == WindowState.Minimized)
                    {
                        main.WindowState = WindowState.Normal;
                    }

                    main.Show();
                    main.Activate();
                    main.Topmost = true;
                    main.Topmost = false;
                    break;
            }
        }

        /// <summary>
        /// 清除所有配置前的重启准备：释放单实例互斥与弹窗信号，
        /// 使随后启动的新实例能获得单实例所有权正常启动（否则新进程会因已有实例而立即退出）。
        /// 调用后本进程应尽快 Shutdown。
        /// </summary>
        public void PrepareForRestart()
        {
            try
            {
                _showSignal?.Close();
            }
            catch
            {
                // 忽略释放异常
            }
            _showSignal = null;

            try
            {
                _mutex?.ReleaseMutex();
            }
            catch
            {
                // 非持有线程释放会抛异常，Dispose 兜底释放句柄
            }

            try
            {
                _mutex?.Dispose();
            }
            catch
            {
                // 忽略释放异常
            }
            _mutex = null;
        }

        protected override void OnExit(ExitEventArgs e)
        {
            App.LogStartup("App.OnExit: 应用正在退出");
            // 退出时停止连发引擎并复位所有按键，避免游戏内卡键
            Services.AutoFireEngine.Stop();

            _showSignal?.Close();
            _showSignal = null;

            if (_mutex is not null)
            {
                try
                {
                    _mutex.ReleaseMutex();
                }
                catch
                {
                    // 非拥有线程释放会抛异常，忽略
                }

                _mutex.Dispose();
                _mutex = null;
            }

            base.OnExit(e);
        }
    }
}