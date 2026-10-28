using System;
using System.Collections.Generic;
using System.IO;
using System.Reflection;
using System.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 游戏助手 Helper DLL 部署与注入：
    /// 从嵌入资源释放内嵌 Helper DLL 到临时目录（随机文件名）→ 注入 DNF.exe →
    /// 立即尝试删除（防提取）；删除失败则登记待清理，待游戏进程退出后补删。
    /// </summary>
    public static class HelperDllDeployer
    {
        private const string ResourceName = "DNFLogin.Resources.HelperDNFHelper.dll";

        /// <summary>待清理的临时 DLL 路径（注入成功后删除失败的）</summary>
        private static readonly List<string> PendingCleanup = new();
        private static readonly object SyncRoot = new();

        /// <summary>注入 Helper DLL 到指定进程（幂等安全，失败不抛异常）。</summary>
        public static void Inject(int pid)
        {
            InjectCore(pid);
        }

        /// <summary>共享注入主体：释放临时 DLL → 注入 → 删除/登记待清理。</summary>
        private static void InjectCore(int pid)
        {
            // 注入前确保共享内存已创建，DLL 注入后即可读到门控标志
            HelperSharedMemory.EnsureCreated();

            string? path = DeployToTemp();
            if (path == null)
            {
                Log("DeployToTemp 失败：嵌入资源 HelperDNFHelper.dll 读取失败");
                return;
            }

            try
            {
                // DNF.exe 刚启动时 32 位模块（kernel32）可能尚未完全加载，
                // 跨位数注入需枚举其 32 位 LoadLibraryW，这里重试等待模块就绪。
                var ok = false;
                for (var i = 0; i < 10 && !ok; i++)
                {
                    if (i > 0) Thread.Sleep(300);
                    ok = DllInjector.Inject(pid, path);
                    Log("注入尝试#" + (i + 1) + " pid=" + pid + " 结果=" + ok);
                }

                if (!ok)
                {
                    TryDelete(path);
                    return;
                }

                // 注入成功：立即尝试删除；失败则登记，游戏退出后补删
                if (!TryDelete(path))
                {
                    lock (SyncRoot)
                    {
                        if (!PendingCleanup.Contains(path))
                        {
                            PendingCleanup.Add(path);
                        }
                    }
                }
            }
            catch (Exception ex)
            {
                Log("注入异常: " + ex.Message);
                TryDelete(path);
            }
        }

        private static void Log(string msg)
        {
            try
            {
                var dir = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin");
                Directory.CreateDirectory(dir);
                File.AppendAllText(Path.Combine(dir, "helper_inject.log"),
                    DateTime.Now.ToString("HH:mm:ss.fff") + " " + msg + Environment.NewLine);
            }
            catch
            {
            }
        }

        /// <summary>清理所有未删除的临时 DLL（游戏进程退出 / 登录器退出时调用）。</summary>
        public static void CleanupPending()
        {
            lock (SyncRoot)
            {
                foreach (var path in PendingCleanup)
                {
                    TryDelete(path);
                }

                PendingCleanup.Clear();
            }
        }

        /// <summary>从嵌入资源释放 DLL 到 %TEMP% 随机文件名；失败返回 null。</summary>
        private static string? DeployToTemp()
        {
            try
            {
                using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName);
                if (stream == null) return null;

                string path = Path.Combine(Path.GetTempPath(), "yyb_" + Guid.NewGuid().ToString("N") + ".dll");
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

        /// <summary>尝试删除文件；删除失败返回 false（文件被进程占用时）。</summary>
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
                // 文件被 DNF.exe 占用（已加载 DLL），等待进程退出后补删
                return false;
            }
        }
    }
}
