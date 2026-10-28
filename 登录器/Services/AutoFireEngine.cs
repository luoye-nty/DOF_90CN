using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Text;

namespace DNFLogin.Services
{
    /// <summary>
    /// 游戏助手 - 按键连发引擎（AHK Worker 子进程托管版）。
    /// 连发核心由 AHK 自己的代码（DAF 同款逻辑）执行：
    /// worker 已由 Ahk2Exe 编译为独立 exe（YHHelper.exe）内嵌，
    /// 启动时仅释放单个 exe 并以子进程运行；进程名 / 文件均无 AutoHotkey 痕迹。
    /// 登录器只负责把设置（扫描码:间隔 列表）通过命令行参数交给 AHK，
    /// 由 AHK 自行完成前台判定、物理键检测、SendEvent vkFFscXX 完整敲击与定时调度。
    /// 生命周期由 Job Object(KILL_ON_JOB_CLOSE) 绑定：登录器退出或被强杀时 worker 自动终止。
    /// 设置界面无感知：Start/Stop 签名与之前一致。
    /// </summary>
    public static class AutoFireEngine
    {
        private const string RuntimeSubDir = "AutoFire";
        // 2026-10-15：释放到 %AppData%\DNFLogin\AutoFire\ 的文件名 = LYHelper.exe（连发 worker 的
        // 托盘文案就叫这个，进程名也跟着它走）。改名的同时对 worker 的托盘图标换成
        // 内嵌的 DNF.ico，见 Assets\AutoFireWorker.ahk 的编译说明。
        private const string RuntimeExeName = "LYHelper.exe";
        private const string ResourceExeName = "DNFLogin.Resources.YHHelper.exe";

        private static readonly object _lock = new();
        private static Process? _proc;
        private static IntPtr _job = IntPtr.Zero;
        private static string? _runtimeDir;

        /// <summary>引擎是否在运行（worker 进程存活）</summary>
        public static bool IsRunning
        {
            get
            {
                lock (_lock)
                {
                    return _proc != null && !_proc.HasExited;
                }
            }
        }

        /// <summary>
        /// 以配置快照启动连发（幂等：会先停止旧 worker）。
        /// 空键名 / 间隔小于 1 的配置行自动跳过。
        /// </summary>
        public static void Start(IEnumerable<AutoFireKeyConfig> keys)
        {
            Stop();

            List<KeyBinding> bindings;
            lock (_lock)
            {
                bindings = keys
                    .Where(k => !string.IsNullOrWhiteSpace(k.Key) && k.IntervalMs >= 1)
                    .Select(CreateBinding)
                    .Where(b => b is not null)
                    .Cast<KeyBinding>()
                    .ToList();
            }

            if (bindings.Count == 0) return;

            try
            {
                var exePath = EnsureRuntime();

                var args = string.Join("|", bindings.Select(b => $"sc{b.ScanHex}:{b.IntervalMs}"));
                var psi = new ProcessStartInfo
                {
                    FileName = exePath,
                    Arguments = $"\"{args}\"",
                    UseShellExecute = false,
                    CreateNoWindow = true,
                    WindowStyle = ProcessWindowStyle.Hidden,
                };

                var p = new Process { StartInfo = psi };
                if (!p.Start()) return;

                // Job Object 绑定生命周期：登录器退出/被杀时 worker 随之终止
                _job = CreateJobObject(IntPtr.Zero, null);
                var info = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
                info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                uint size = (uint)Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
                SetInformationJobObject(_job, JobObjectExtendedLimitInformation, ref info, size);
                AssignProcessToJobObject(_job, p.Handle);

                lock (_lock)
                {
                    _proc = p;
                }

            }
            catch
            {
            }
        }

        /// <summary>停止引擎：结束 worker 子进程</summary>
        public static void Stop()
        {
            Process? p;
            lock (_lock)
            {
                p = _proc;
                _proc = null;
            }

            if (p != null)
            {
                try
                {
                    if (!p.HasExited)
                    {
                        p.Kill();
                    }
                    p.WaitForExit(1000);
                }
                catch
                {
                    // 进程已退出或无权操作，忽略
                }
                p.Dispose();
            }

            if (_job != IntPtr.Zero)
            {
                CloseHandle(_job);
                _job = IntPtr.Zero;
            }
        }

        /// <summary>当前进程是否以管理员权限运行（DNF 游戏若以管理员运行，登录器必须同为管理员才能注入输入）</summary>
        private static bool IsAdministrator()
        {
            try
            {
                using var identity = System.Security.Principal.WindowsIdentity.GetCurrent();
                var principal = new System.Security.Principal.WindowsPrincipal(identity);
                return principal.IsInRole(System.Security.Principal.WindowsBuiltInRole.Administrator);
            }
            catch
            {
                return false;
            }
        }

        /// <summary>确保运行时目录存在且 YHHelper.exe 已释放（与配置同目录，权限有保障）</summary>
        private static string EnsureRuntime()
        {
            if (_runtimeDir is not null && File.Exists(_runtimeDir))
            {
                return _runtimeDir;
            }

            var dir = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData),
                "DNFLogin", RuntimeSubDir);
            Directory.CreateDirectory(dir);

            var exePath = Path.Combine(dir, RuntimeExeName);
            var needWrite = !File.Exists(exePath);
            if (!needWrite)
            {
                var fi = new FileInfo(exePath);
                using var res = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceExeName);
                if (res is not null && fi.Length != res.Length)
                {
                    needWrite = true;
                }
            }

            if (needWrite)
            {
                using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceExeName)
                    ?? throw new InvalidOperationException("缺少内嵌资源 " + ResourceExeName);
                using var fs = new FileStream(exePath, FileMode.Create, FileAccess.Write, FileShare.Read);
                stream.CopyTo(fs);
            }

            _runtimeDir = exePath;
            return exePath;
        }

        /// <summary>单个连发键的运行时绑定（仅需扫描码 + 间隔，worker 内自行拼 vkFFscXX）</summary>
        private sealed class KeyBinding
        {
            public string ScanHex = string.Empty; // 如 "2D"
            public int IntervalMs = 10;
        }

        /// <summary>根据配置行创建运行时绑定；键名无法解析时返回 null</summary>
        private static KeyBinding? CreateBinding(AutoFireKeyConfig cfg)
        {
            var vk = KeyNameToVk(cfg.Key.Trim());
            if (vk == 0)
            {
                return null;
            }

            var scan = (byte)MapVirtualKey(vk, MAPVK_VK_TO_VSC);
            return new KeyBinding
            {
                ScanHex = scan.ToString("X2"),
                IntervalMs = Math.Max(1, cfg.IntervalMs)
            };
        }

        // ---------------- 键名映射（设置页校验仍在使用，保持 public） ----------------

        /// <summary>显示键名 → 虚拟键码（0 表示无法解析）</summary>
        public static uint KeyNameToVk(string name)
        {
            if (string.IsNullOrWhiteSpace(name))
            {
                return 0;
            }

            switch (name.ToUpperInvariant())
            {
                case "SPACE": return 0x20;
                case "ENTER": return 0x0D;
                case "TAB": return 0x09;
                case "BACKSPACE": case "BACK": return 0x08;
                case "ESC": case "ESCAPE": return 0x1B;
                case "CAPS": case "CAPSLOCK": return 0x14;
                case "UP": return 0x26;
                case "DOWN": return 0x28;
                case "LEFT": return 0x25;
                case "RIGHT": return 0x27;
                case "HOME": return 0x24;
                case "END": return 0x23;
                case "PGUP": case "PAGEUP": return 0x21;
                case "PGDN": case "PAGEDOWN": return 0x22;
                case "INS": case "INSERT": return 0x2D;
                case "DEL": case "DELETE": return 0x2E;
                case "LSHIFT": return 0xA0;
                case "RSHIFT": return 0xA1;
                case "LCTRL": case "LCONTROL": return 0xA2;
                case "RCTRL": case "RCONTROL": return 0xA3;
                case "LALT": return 0xA4;
                case "RALT": return 0xA5;
                case "LWIN": return 0x5B;
                case "RWIN": return 0x5C;
                case ";": case "SEMICOLON": return 0xBA;
                case "'": case "QUOTE": return 0xDE;
                case ",": case "COMMA": return 0xBC;
                case ".": case "DOT": return 0xBE;
                case "/": case "SLASH": return 0xBF;
                case "\\": case "BACKSLASH": return 0xDC;
                case "[": case "LBRACKET": return 0xDB;
                case "]": case "RBRACKET": return 0xDD;
                case "-": case "MINUS": return 0xBD;
                case "=": case "EQUAL": return 0xBB;
                case "`": case "GRAVE": return 0xC0;
                case "NUMPAD*": case "NUMPADMULT": return 0x6A;
                case "NUMPAD+": case "NUMPADADD": return 0x6B;
                case "NUMPAD-": case "NUMPADSUB": return 0x6D;
                case "NUMPAD.": case "NUMPADDOT": return 0x6E;
                case "NUMPAD/": case "NUMPADDIV": return 0x6F;
            }

            // 字母 / 数字 / F 键 / 小键盘数字
            var s = name.ToUpperInvariant();
            if (s.Length == 1)
            {
                char c = s[0];
                if (c >= 'A' && c <= 'Z')
                {
                    return (uint)(c - 'A' + 0x41);
                }

                if (c >= '0' && c <= '9')
                {
                    return (uint)(c - '0' + 0x30);
                }
            }

            if (s.StartsWith("F", StringComparison.Ordinal) && s.Length <= 3)
            {
                if (int.TryParse(s.Substring(1), out int f) && f >= 1 && f <= 24)
                {
                    return (uint)(f - 1 + 0x70);
                }
            }

            if (s.StartsWith("NUMPAD", StringComparison.Ordinal))
            {
                var d = s.Substring("NUMPAD".Length);
                if (d.Length == 1 && d[0] >= '0' && d[0] <= '9')
                {
                    return (uint)(d[0] - '0' + 0x60);
                }
            }

            return 0;
        }

        // ---------------- Win32 ----------------

        private const uint MAPVK_VK_TO_VSC = 0;

        private const int JobObjectExtendedLimitInformation = 9;
        private const uint JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x2000;

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern IntPtr CreateJobObject(IntPtr lpJobAttributes, string? lpName);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool SetInformationJobObject(IntPtr hJob, int JobObjectInformationClass, ref JOBOBJECT_EXTENDED_LIMIT_INFORMATION lpJobObjectInformation, uint cbJobObjectInformationLength);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool AssignProcessToJobObject(IntPtr hJob, IntPtr hProcess);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr hObject);

        [DllImport("user32.dll")]
        private static extern uint MapVirtualKey(uint uCode, uint uMapType);

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
        private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION
        {
            public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
            public IO_COUNTERS IoInfo;
            public UIntPtr ProcessMemoryLimit;
            public UIntPtr JobMemoryLimit;
            public UIntPtr PeakProcessMemoryUsed;
            public UIntPtr PeakJobMemoryUsed;
        }
    }
}
