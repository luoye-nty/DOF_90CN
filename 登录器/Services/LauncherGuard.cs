using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.IO.MemoryMappedFiles;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 登录器存活守护（反作弊基础，全版本通用：90 / S4A21 / S4A12）：
    ///  1) Job Object（JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE）：
    ///     把 DNF.exe Assign 进登录器创建的 Job。登录器进程被任务管理器结束 / 崩溃时，
    ///     内核在最后一个 Job 句柄关闭瞬间自动终止 Job 内全部游戏进程 —— 即时、不依赖 DLL 注入。
    ///  2) 心跳共享内存 + GuardWatchdog.dll：
    ///     登录器后台线程每 200ms 递增共享内存心跳（Local\YYBLauncherGuard）；
    ///     注入游戏内的 watchdog 发现心跳超过 5s 未变化（登录器被挂起 / 假死 / 退出）则自杀游戏进程，
    ///     兜底补上 Job Object 感知不到的"整进程挂起"场景。
    /// GuardWatchdog 是纯 Win32 心跳监视，不碰任何游戏版本地址，因此 90 与 S4 系列均可注入。
    /// </summary>
    public static class LauncherGuard
    {
        private const string WatchdogResource = "DNFLogin.Resources.GuardWatchdog.dll";
        private const string MapName = @"Local\YYBLauncherGuard";
        private const int MapSize = 16;
        private const int Magic = 0x47554152; // 'RAUG'，与 GuardWatchdog.c 一致
        private const int OffHeartbeat = 4;
        private const int OffPid = 8;
        private const int HeartbeatIntervalMs = 200;

        private const uint JobObjectExtendedLimitInformation = 9;
        private const uint JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000;
        private const uint PROCESS_SET_QUOTA = 0x0100;
        private const uint PROCESS_TERMINATE = 0x0001;
        private const uint PROCESS_QUERY_INFORMATION = 0x0400;

        [StructLayout(LayoutKind.Sequential)]
        private struct IO_COUNTERS
        {
            public ulong ReadOperationCount;
            public ulong WriteOperationCount;
            public ulong OtherOperationCount;
            public ulong ReadTransferCount;
            public ulong WriteTransferCount;
            public ulong OtherTransferCount;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct JOBOBJECT_BASIC_LIMIT_INFORMATION
        {
            public long PerProcessUserTimeLimit;
            public long PerJobUserTimeLimit;
            public uint LimitFlags;
            public UIntPtr MinimumWorkingSetSize;
            public UIntPtr MaximumWorkingSetSize;
            public uint ActiveProcessLimit;
            public UIntPtr Affinity;
            public uint PriorityClass;
            public uint SchedulingClass;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION
        {
            public JOBOBJECT_BASIC_LIMIT_INFORMATION Basic;
            public IO_COUNTERS IoInfo;
            public UIntPtr ProcessMemoryLimit;
            public UIntPtr JobMemoryLimit;
            public UIntPtr PeakProcessMemoryUsed;
            public UIntPtr PeakJobMemoryUsed;
        }

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern IntPtr CreateJobObject(IntPtr lpJobAttributes, string? lpName);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool SetInformationJobObject(IntPtr hJob, uint jobObjectInformationClass,
            ref JOBOBJECT_EXTENDED_LIMIT_INFORMATION lpJobObjectInformation, int cbJobObjectInformationLength);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool AssignProcessToJob(IntPtr hJob, IntPtr hProcess);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr OpenProcess(uint dwDesiredAccess, bool bInheritHandle, int dwProcessId);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr hObject);

        private static readonly object Sync = new();
        private static readonly List<string> PendingCleanup = new();
        private static IntPtr _job;
        private static MemoryMappedFile? _map;
        private static MemoryMappedViewAccessor? _view;
        private static Thread? _beatThread;
        private static volatile bool _stopBeat;
        private static int _beat;
        private static int _started;

        /// <summary>初始化守护：创建 Job + 心跳共享内存 + 启动心跳线程（幂等）。</summary>
        public static void EnsureStarted()
        {
            if (Interlocked.Exchange(ref _started, 1) == 1)
            {
                return;
            }

            lock (Sync)
            {
                try
                {
                    // 1) Job：设置 KILL_ON_JOB_CLOSE，句柄由本进程持有，进程退出时内核自动杀 Job 内进程
                    _job = CreateJobObject(IntPtr.Zero, null);
                    if (_job != IntPtr.Zero)
                    {
                        var info = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
                        info.Basic.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                        int len = Marshal.SizeOf<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>();
                        if (!SetInformationJobObject(_job, JobObjectExtendedLimitInformation, ref info, len))
                        {
                            Log("SetInformationJobObject 失败, err=" + Marshal.GetLastWin32Error());
                            CloseHandle(_job);
                            _job = IntPtr.Zero;
                        }
                    }
                    else
                    {
                        Log("CreateJobObject 失败, err=" + Marshal.GetLastWin32Error());
                    }

                    // 2) 心跳共享内存（magic + heartbeat + launcher pid）
                    _map = MemoryMappedFile.CreateOrOpen(MapName, MapSize);
                    _view = _map.CreateViewAccessor(0, MapSize);
                    _view.Write(0, Magic);
                    _view.Write(OffHeartbeat, 0);
                    _view.Write(OffPid, Environment.ProcessId);

                    // 3) 心跳线程：进程存活期间持续递增；被挂起时线程停更 → watchdog 判死
                    _stopBeat = false;
                    _beatThread = new Thread(HeartbeatLoop)
                    {
                        IsBackground = true,
                        Name = "LauncherGuardHeartbeat"
                    };
                    _beatThread.Start();

                    Log("LauncherGuard 就绪, pid=" + Environment.ProcessId + ", job=" + _job);
                }
                catch (Exception ex)
                {
                    Log("LauncherGuard 初始化异常: " + ex.Message);
                }
            }
        }

        private static void HeartbeatLoop()
        {
            while (!_stopBeat)
            {
                try
                {
                    int v = Interlocked.Increment(ref _beat);
                    _view?.Write(OffHeartbeat, v);
                }
                catch
                {
                }

                try
                {
                    Thread.Sleep(HeartbeatIntervalMs);
                }
                catch
                {
                }
            }
        }

        /// <summary>把游戏进程加入登录器 Job；失败不阻塞（watchdog 心跳仍兜底）。</summary>
        public static void AttachGame(Process game)
        {
            EnsureStarted();
            if (_job == IntPtr.Zero || game is null || game.HasExited)
            {
                return;
            }

            try
            {
                IntPtr hProc = OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE | PROCESS_QUERY_INFORMATION, false, game.Id);
                if (hProc == IntPtr.Zero)
                {
                    Log("AttachGame OpenProcess 失败 pid=" + game.Id + ", err=" + Marshal.GetLastWin32Error());
                    return;
                }

                try
                {
                    if (!AssignProcessToJob(_job, hProc))
                    {
                        Log("AssignProcessToJob 失败 pid=" + game.Id + ", err=" + Marshal.GetLastWin32Error());
                    }
                }
                finally
                {
                    CloseHandle(hProc);
                }
            }
            catch (Exception ex)
            {
                Log("AttachGame 异常: " + ex.Message);
            }
        }

        /// <summary>注入 GuardWatchdog.dll 到游戏进程（幂等安全，失败不抛异常）。</summary>
        public static void InjectWatchdog(int pid)
        {
            EnsureStarted();

            string? path = DeployToTemp();
            if (path == null)
            {
                Log("DeployToTemp 失败：嵌入资源 GuardWatchdog.dll 读取失败");
                return;
            }

            try
            {
                var ok = false;
                for (var i = 0; i < 10 && !ok; i++)
                {
                    if (i > 0) Thread.Sleep(300);
                    ok = DllInjector.Inject(pid, path);
                }

                if (!ok)
                {
                    TryDelete(path);
                    return;
                }

                if (!TryDelete(path))
                {
                    lock (Sync)
                    {
                        if (!PendingCleanup.Contains(path))
                        {
                            PendingCleanup.Add(path);
                        }
                    }
                }

                Log("GuardWatchdog 注入完成 pid=" + pid);
            }
            catch (Exception ex)
            {
                Log("GuardWatchdog 注入异常: " + ex.Message);
                TryDelete(path);
            }
        }

        /// <summary>清理注入成功后未能删除的临时 watchdog DLL（游戏进程退出后调用）。</summary>
        public static void CleanupPending()
        {
            lock (Sync)
            {
                foreach (var path in PendingCleanup)
                {
                    TryDelete(path);
                }

                PendingCleanup.Clear();
            }
        }

        private static string? DeployToTemp()
        {
            try
            {
                using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(WatchdogResource);
                if (stream == null) return null;

                string path = Path.Combine(Path.GetTempPath(), "yyb_guard_" + Guid.NewGuid().ToString("N") + ".dll");
                using (var fs = new FileStream(path, FileMode.CreateNew, FileAccess.Write, FileShare.None))
                {
                    stream.CopyTo(fs);
                }

                return path;
            }
            catch
            {
                return null;
            }
        }

        private static bool TryDelete(string path)
        {
            try
            {
                if (File.Exists(path))
                {
                    File.Delete(path);
                }

                return true;
            }
            catch
            {
                return false;
            }
        }

        private static void Log(string msg)
        {
            try
            {
                var dir = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin");
                Directory.CreateDirectory(dir);
                File.AppendAllText(Path.Combine(dir, "guard.log"),
                    DateTime.Now.ToString("HH:mm:ss.fff") + " " + msg + Environment.NewLine);
            }
            catch
            {
            }
        }
    }
}
