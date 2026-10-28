using System;
using System.IO;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 7002 认证核心 DLL（s4authcore.dll）原生封装。
    /// 核心逻辑（Ed25519 签名 + X25519 ECDH + AES-256-GCM 握手、密钥还原）全部在 Themida 加壳的
    /// 原生 DLL 内完成；本类仅负责部署与 P/Invoke，托管侧不落任何密钥/算法。
    /// 部署策略：优先加载 Helper 目录下的 s4authcore.dll（开发/调试用），
    /// 否则从嵌入资源释放到临时目录随机文件名后 LoadLibrary（发布版路径，防提取）。
    /// </summary>
    public static class S4AuthCoreNative
    {
        private const string ResourceName = "DNFLogin.Resources.s4authcore.dll";

        private static readonly object SyncRoot = new();
        private static IntPtr _module;
        private static bool _resolved;
        private static string? _deployedPath;

        // ── 认证结果（与 s4authcore.h 的 s4auth_result 严格对齐）──
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Ansi)]
        public struct S4AuthResult
        {
            public int Ok;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 512)]
            public string Msg;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 64)]
            public string MId;
            public long Ts;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 65)]
            public string SessionHex;

            /// <summary>
            /// 供 UI 展示的正确消息文本。
            /// Msg 经 ByValTStr(CharSet.Ansi) 封送时被按系统 ANSI 代码页（GBK）误读，
            /// 而 s4authcore.dll 写入的是 Go 工具下发的 UTF-8 字节，故显示乱码；
            /// 本属性做字节级还原后再 UTF-8 解码（纯 ASCII 消息往返无损）。
            /// </summary>
            public string MsgText => S4AuthCoreNative.DecodeMsg(Msg);
        }

        private delegate int DllAuthenticate(
            [MarshalAs(UnmanagedType.LPStr)] string host,
            int port,
            [MarshalAs(UnmanagedType.LPStr)] string mId,
            [MarshalAs(UnmanagedType.LPStr)] string password,
            out S4AuthResult result);

        private delegate int DllRegister(
            [MarshalAs(UnmanagedType.LPStr)] string host,
            int port,
            [MarshalAs(UnmanagedType.LPStr)] string mId,
            [MarshalAs(UnmanagedType.LPStr)] string password,
            out S4AuthResult result);

        private delegate int DllSelfCheck(
            [MarshalAs(UnmanagedType.LPStr)] System.Text.StringBuilder err,
            int errLen);

        /// <summary>是否已成功加载核心 DLL（惰性解析，首次调用认证/自检时加载）。</summary>
        public static bool IsReady
        {
            get
            {
                EnsureResolved();
                return _module != IntPtr.Zero;
            }
        }

        /// <summary>已部署的 DLL 路径（调试辅助；未部署返回 null）。</summary>
        public static string? DeployedPath => _deployedPath;

        /// <summary>
        /// 还原被 ANSI 误读的 7002 消息文本：乱码字符串按系统 ANSI 代码页还原字节，
        /// 再以严格 UTF-8 解码。字节不是合法 UTF-8 时原样返回（兼容纯 ASCII 与本地文案）。
        /// </summary>
        public static string DecodeMsg(string? msg)
        {
            if (string.IsNullOrEmpty(msg))
            {
                return msg ?? string.Empty;
            }

            try
            {
                var raw = Encoding.Default.GetBytes(msg);
                return new UTF8Encoding(false, true).GetString(raw);
            }
            catch
            {
                return msg;
            }
        }

        /// <summary>自检：密钥还原、签名/验签、CNG 可用性。返回错误说明，null 表示通过。</summary>
        public static string? SelfCheck()
        {
            if (!TryResolve(out var error))
            {
                return error;
            }

            var fn = GetProc<DllSelfCheck>("s4auth_selfcheck");
            if (fn == null)
            {
                return "s4authcore.dll 缺少导出 s4auth_selfcheck";
            }

            var err = new System.Text.StringBuilder(256);
            var rc = fn(err, err.Capacity);
            return rc == 0 ? null : err.ToString();
        }

        /// <summary>
        /// 执行 7002 认证登录（阶段1握手 + 阶段2 login 业务包收口校验）。成功返回 true 并填充 result；失败返回 false，error 为本地错误说明。
        /// </summary>
        public static bool Authenticate(string host, int port, string mId, string password, out S4AuthResult result, out string? error)
        {
            result = default;
            if (string.IsNullOrWhiteSpace(host))
            {
                error = "服务器地址为空";
                return false;
            }

            if (!TryResolve(out error))
            {
                return false;
            }

            var fn = GetProc<DllAuthenticate>("s4auth_authenticate");
            if (fn == null)
            {
                error = "s4authcore.dll 缺少导出 s4auth_authenticate";
                return false;
            }

            var rc = fn(host.Trim(), port, mId, password, out result);
            if (rc != 0)
            {
                error = string.IsNullOrWhiteSpace(result.Msg) ? "认证本地错误（网络/协议层）" : DecodeMsg(result.Msg);
                return false;
            }

            error = null;
            return true;
        }

        /// <summary>
        /// 执行 7002 远程注册（阶段1握手 + 阶段2 register 业务包，工具查重/写库收口）。成功返回 true 并填充 result；失败返回 false，error 为本地错误说明。
        /// </summary>
        public static bool Register(string host, int port, string mId, string password, out S4AuthResult result, out string? error)
        {
            result = default;
            if (string.IsNullOrWhiteSpace(host))
            {
                error = "服务器地址为空";
                return false;
            }

            if (!TryResolve(out error))
            {
                return false;
            }

            var fn = GetProc<DllRegister>("s4auth_register");
            if (fn == null)
            {
                error = "s4authcore.dll 缺少导出 s4auth_register";
                return false;
            }

            var rc = fn(host.Trim(), port, mId, password, out result);
            if (rc != 0)
            {
                error = string.IsNullOrWhiteSpace(result.Msg) ? "注册本地错误（网络/协议层）" : DecodeMsg(result.Msg);
                return false;
            }

            error = null;
            return true;
        }

        /// <summary>释放已加载模块并清理临时部署（进程退出/版本切换时调用）。</summary>
        public static void Unload()
        {
            lock (SyncRoot)
            {
                if (_module != IntPtr.Zero)
                {
                    FreeLibrary(_module);
                    _module = IntPtr.Zero;
                }

                _resolved = false;
                var path = _deployedPath;
                _deployedPath = null;
                if (path != null)
                {
                    TryDeleteDelayed(path);
                }
            }
        }

        // ── 内部：解析 DLL 路径并 LoadLibrary ──────────────────────

        private static void EnsureResolved()
        {
            if (_resolved)
            {
                return;
            }

            lock (SyncRoot)
            {
                if (_resolved)
                {
                    return;
                }

                TryResolve(out _);
            }
        }

        private static bool TryResolve(out string? error)
        {
            if (_module != IntPtr.Zero)
            {
                error = null;
                _resolved = true;
                return true;
            }

            lock (SyncRoot)
            {
                if (_module != IntPtr.Zero)
                {
                    error = null;
                    _resolved = true;
                    return true;
                }

                // 1) Helper 目录优先（开发/调试：Helper\s4authcore.dll 与 exe 同目录）
                var baseDir = AppContext.BaseDirectory;
                var helperPath = Path.Combine(baseDir, "Helper", "s4authcore.dll");
                if (File.Exists(helperPath))
                {
                    _module = LoadLibrary(helperPath);
                    if (_module != IntPtr.Zero)
                    {
                        _deployedPath = helperPath;
                        error = null;
                        _resolved = true;
                        return true;
                    }
                }

                // 2) 嵌入资源释放到临时目录（发布版单文件路径）
                string? deployed = null;
                try
                {
                    using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName);
                    if (stream == null)
                    {
                        error = "未找到内嵌 s4authcore.dll 资源";
                        _resolved = true;
                        return false;
                    }

                    deployed = Path.Combine(Path.GetTempPath(), "yyb_s4auth_" + Guid.NewGuid().ToString("N") + ".dll");
                    using (var fs = new FileStream(deployed, FileMode.CreateNew, FileAccess.Write, FileShare.None))
                    {
                        stream.CopyTo(fs);
                    }

                    _module = LoadLibrary(deployed);
                    if (_module != IntPtr.Zero)
                    {
                        _deployedPath = deployed;
                        error = null;
                        _resolved = true;
                        return true;
                    }

                    TryDeleteDelayed(deployed);
                    error = "s4authcore.dll 加载失败";
                }
                catch (Exception)
                {
                    if (deployed != null)
                    {
                        TryDeleteDelayed(deployed);
                    }

                    error = "s4authcore.dll 部署失败，请检查文件是否被占用或被杀毒软件拦截后重试";
                }

                _resolved = true;
                return false;
            }
        }

        private static TDelegate? GetProc<TDelegate>(string name) where TDelegate : Delegate
        {
            if (_module == IntPtr.Zero)
            {
                return null;
            }

            var addr = GetProcAddress(_module, name);
            return addr == IntPtr.Zero ? null : Marshal.GetDelegateForFunctionPointer<TDelegate>(addr);
        }

        private static void TryDeleteDelayed(string path)
        {
            // DLL 已加载时无法删除，登记由进程退出前统一清理；此处仅尽力而为
            try
            {
                if (File.Exists(path))
                {
                    File.Delete(path);
                }
            }
            catch
            {
                // 占用中，忽略（临时目录残留不影响功能）
            }
        }

        // ── Win32 ──────────────────────────────────────────────────

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern IntPtr LoadLibrary(string lpFileName);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr GetProcAddress(IntPtr hModule, string lpProcName);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool FreeLibrary(IntPtr hModule);
    }
}
