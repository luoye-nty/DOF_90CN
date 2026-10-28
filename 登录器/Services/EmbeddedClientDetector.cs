using System;
using System.Collections.Generic;
using System.IO;
using System.Reflection;
using System.Runtime.InteropServices;

namespace DNFLogin.Services
{
    /// <summary>
    /// 客户端目录自动识别（登录器嵌入客户端目录场景）：
    /// 登录器 exe 所在目录本身就是 DNF 客户端目录（存在 DNF.exe / NoPack.exe）时，
    /// 自动把当前版本 GamePath 指向该目录下的客户端 exe，并在桌面补建登录器快捷方式，
    /// 玩家无需再到设置页手动选择客户端目录。
    /// 不在客户端目录运行时不做任何动作（不改配置、不建快捷方式）。
    /// </summary>
    public static class EmbeddedClientDetector
    {
        private static readonly string[] ClientExeNames = { "DNF.exe", "NoPack.exe" };

        /// <summary>当前登录器 exe 完整路径（单文件发布时为宿主 exe）</summary>
        public static string LauncherExePath { get; } = Environment.ProcessPath ?? "";

        /// <summary>登录器所在目录</summary>
        public static string LauncherDirectory { get; } = ResolveLauncherDirectory();

        /// <summary>
        /// 入口：登录器位于客户端目录时执行自动识别 + 桌面快捷方式补齐。
        /// 返回本次实际执行动作描述；无需处理时返回空字符串。
        /// 任何失败都不阻断登录器启动。
        /// </summary>
        public static string AutoDetectAndPrepareShortcut()
        {
            var clientExe = FindClientExeInLauncherDir();
            if (clientExe is null)
            {
                return "";
            }

            var actions = new List<string>();

            // 1) 自动识别：仅当前版本 GamePath 缺失/无效才写入，绝不覆盖已有有效配置
            var profile = AppConfig.CurrentProfile;
            var needFill = string.IsNullOrWhiteSpace(profile.GamePath) ||
                           !File.Exists(profile.GamePath);
            if (needFill &&
                !string.Equals(profile.GamePath, clientExe, StringComparison.OrdinalIgnoreCase))
            {
                profile.GamePath = clientExe;
                profile.CopyToTopLevel(AppConfig.Settings);
                AppConfig.Save();
                actions.Add("自动识别客户端目录");
            }

            // 2) 桌面快捷方式：不存在时补齐；已有同目标快捷方式则跳过
            if (EnsureDesktopShortcut())
            {
                actions.Add("创建桌面快捷方式");
            }

            return string.Join("，", actions);
        }

        /// <summary>登录器所在目录是否属于客户端目录（返回匹配到的客户端 exe 完整路径；不存在返回 null）</summary>
        public static string? FindClientExeInLauncherDir()
        {
            if (string.IsNullOrEmpty(LauncherDirectory))
            {
                return null;
            }

            foreach (var name in ClientExeNames)
            {
                var full = Path.Combine(LauncherDirectory, name);
                if (File.Exists(full))
                {
                    return full;
                }
            }

            return null;
        }

        /// <summary>登录器所在目录是否为有效客户端目录</summary>
        public static bool IsInClientDirectory() => FindClientExeInLauncherDir() is not null;

        /// <summary>桌面快捷方式路径：与登录器 exe 同名（如 YH登录器.lnk）</summary>
        public static string DesktopShortcutPath()
        {
            var desktop = Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory);
            var name = Path.GetFileNameWithoutExtension(LauncherExePath);
            if (string.IsNullOrWhiteSpace(name))
            {
                name = "DNF登录器";
            }

            return Path.Combine(desktop, name + ".lnk");
        }

        /// <summary>补齐桌面快捷方式。返回 true 表示本次实际创建；已存在且目标一致或同名指向他程序时返回 false。</summary>
        public static bool EnsureDesktopShortcut()
        {
            try
            {
                if (string.IsNullOrEmpty(LauncherExePath) || !File.Exists(LauncherExePath))
                {
                    return false;
                }

                var linkPath = DesktopShortcutPath();
                var linkDir = Path.GetDirectoryName(linkPath);
                if (string.IsNullOrEmpty(linkDir) || !Directory.Exists(linkDir))
                {
                    return false;
                }

                if (File.Exists(linkPath))
                {
                    var existingTarget = ReadShortcutTarget(linkPath);
                    if (string.IsNullOrEmpty(existingTarget) ||
                        string.Equals(existingTarget.Trim(), LauncherExePath, StringComparison.OrdinalIgnoreCase))
                    {
                        // 已存在且指向同一登录器（含读取失败场景）：跳过，避免重复创建与覆盖用户自定义快捷方式
                        return false;
                    }

                    // 死链自愈（2026-10-16）：同名快捷方式指向的登录器副本被删掉/搬走之后，
                    // 上面那条"指向别处就跳过"会让桌面永远留着一个点不开的死图标，而且登录器
                    // 换个目录部署后**再也不会**补快捷方式（玩家只看到"怎么不自动生成了"）。
                    // 这里只重建"目标文件已不存在、且目标文件名与当前登录器同名"的那一个
                    // ——即确实是登录器的另一份副本；指向其它程序的自定义快捷方式仍然一律不动。
                    var existingTargetPath = existingTarget.Trim();
                    var existingName = Path.GetFileName(existingTargetPath);
                    if (!File.Exists(existingTargetPath) &&
                        !string.IsNullOrEmpty(existingName) &&
                        string.Equals(existingName, Path.GetFileName(LauncherExePath), StringComparison.OrdinalIgnoreCase))
                    {
                        WriteShortcut(linkPath, LauncherExePath, LauncherDirectory);
                        App.LogStartup("EmbeddedClientDetector: 同名快捷方式已失效（目标不存在），已重建指向当前登录器: "
                                       + linkPath + " -> " + LauncherExePath);
                        return true;
                    }

                    App.LogStartup("EmbeddedClientDetector: 桌面已存在同名快捷方式且指向其他目标，跳过: "
                                   + linkPath + " -> " + existingTarget);
                    return false;
                }

                WriteShortcut(linkPath, LauncherExePath, LauncherDirectory);
                return true;
            }
            catch (Exception ex)
            {
                App.LogStartup("EmbeddedClientDetector: EnsureDesktopShortcut 失败 -> " + ex.Message);
                return false;
            }
        }

        /// <summary>
        /// 登录器自我更新后的桌面快捷方式迁移（供 Services.LauncherSelfUpdater 调用）。
        ///
        /// 为什么需要它：自动更新时新登录器是用**包里自己的文件名**落地的（登录器可能会改名），
        /// 旧名字的快捷方式就会变成指向已删除文件的死图标。而且新登录器只有在"自身位于客户端
        /// 目录"时才会自动补快捷方式（见 AutoDetectAndPrepareShortcut 的提前返回），
        /// 装在别处时就没人管了。
        ///
        /// 做法：
        ///   · 同名（新旧 exe 文件名相同）→ 把原快捷方式就地改指向新路径，名字不动；
        ///   · 改名 → 只有当"旧快捷方式确实指向旧 exe"时才删掉它，然后按新名字建一个。
        ///
        /// 安全约束：**只动"目标正好是旧登录器 exe"的那一个快捷方式**。
        /// 目标读不出来、或指向别处（玩家自定义的快捷方式）时一律不动。
        /// 任何失败都只记日志，绝不抛给调用方 —— 更新本身不能被快捷方式搞失败。
        /// </summary>
        public static void MigrateDesktopShortcut(string oldExePath, string newExePath)
        {
            try
            {
                if (string.IsNullOrWhiteSpace(oldExePath) || string.IsNullOrWhiteSpace(newExePath))
                {
                    return;
                }
                if (string.Equals(oldExePath.Trim(), newExePath.Trim(), StringComparison.OrdinalIgnoreCase))
                {
                    return; // 路径没变（理论上不会走到这里）：什么都不用做
                }

                var desktop = Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory);
                if (string.IsNullOrEmpty(desktop) || !Directory.Exists(desktop))
                {
                    return;
                }

                var oldBase = Path.GetFileNameWithoutExtension(oldExePath);
                var newBase = Path.GetFileNameWithoutExtension(newExePath);
                if (string.IsNullOrWhiteSpace(oldBase) || string.IsNullOrWhiteSpace(newBase))
                {
                    return;
                }

                var oldLink = Path.Combine(desktop, oldBase + ".lnk");
                if (!File.Exists(oldLink))
                {
                    return; // 桌面上本来就没有这个快捷方式：不新建（避免凭空多出一个图标）
                }

                // 只处理"确实指向旧登录器"的快捷方式：读不出目标就当作不是我们的，直接放弃。
                var target = ReadShortcutTarget(oldLink);
                if (string.IsNullOrEmpty(target) ||
                    !string.Equals(target.Trim(), oldExePath.Trim(), StringComparison.OrdinalIgnoreCase))
                {
                    App.LogStartup("EmbeddedClientDetector: 桌面快捷方式不指向旧登录器，跳过迁移: " + oldLink +
                                   " -> " + (target ?? "(读取失败)"));
                    return;
                }

                var newLink = Path.Combine(desktop, newBase + ".lnk");
                var newDir = Path.GetDirectoryName(newExePath) ?? "";
                if (string.Equals(oldBase, newBase, StringComparison.OrdinalIgnoreCase))
                {
                    // 同名：就地改指向（快捷方式名字不变，玩家无感）。
                    WriteShortcut(oldLink, newExePath, newDir);
                    App.LogStartup("EmbeddedClientDetector: 桌面快捷方式已改指向新登录器: " + oldLink);
                    return;
                }

                // 改名：删掉旧图标的 .lnk，按新名字重建一个（等价于"快捷方式跟着登录器改名"）。
                File.Delete(oldLink);
                if (!File.Exists(newLink))
                {
                    WriteShortcut(newLink, newExePath, newDir);
                    App.LogStartup("EmbeddedClientDetector: 桌面快捷方式已迁移为新名字: " + newLink);
                }
                else
                {
                    App.LogStartup("EmbeddedClientDetector: 已存在同名新快捷方式，只清除旧图标: " + newLink);
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("EmbeddedClientDetector: MigrateDesktopShortcut 失败（不影响更新）-> " + ex.Message);
            }
        }

        private static void WriteShortcut(string linkPath, string targetExePath, string workingDir)
        {
            var shellType = Type.GetTypeFromProgID("WScript.Shell");
            if (shellType is null)
            {
                throw new InvalidOperationException("WScript.Shell COM 不可用");
            }

            var shell = Activator.CreateInstance(shellType)
                        ?? throw new InvalidOperationException("WScript.Shell 创建失败");
            object? shortcut = null;
            try
            {
                shortcut = shellType.InvokeMember("CreateShortcut",
                    BindingFlags.InvokeMethod, null, shell, new object[] { linkPath })
                           ?? throw new InvalidOperationException("CreateShortcut 返回空");

                var type = shortcut.GetType();
                type.InvokeMember("TargetPath", BindingFlags.SetProperty, null, shortcut,
                    new object[] { targetExePath });
                type.InvokeMember("WorkingDirectory", BindingFlags.SetProperty, null, shortcut,
                    new object[] { workingDir });
                type.InvokeMember("IconLocation", BindingFlags.SetProperty, null, shortcut,
                    new object[] { targetExePath + ",0" });
                type.InvokeMember("Description", BindingFlags.SetProperty, null, shortcut,
                    new object[] { "DNF 登录器快捷方式" });
                type.InvokeMember("Save", BindingFlags.InvokeMethod, null, shortcut, null);
            }
            finally
            {
                if (shortcut is not null && Marshal.IsComObject(shortcut))
                {
                    Marshal.FinalReleaseComObject(shortcut);
                }

                if (Marshal.IsComObject(shell))
                {
                    Marshal.FinalReleaseComObject(shell);
                }
            }
        }

        private static string? ReadShortcutTarget(string linkPath)
        {
            try
            {
                var shellType = Type.GetTypeFromProgID("WScript.Shell");
                if (shellType is null)
                {
                    return null;
                }

                var shell = Activator.CreateInstance(shellType);
                if (shell is null)
                {
                    return null;
                }

                object? shortcut = null;
                try
                {
                    shortcut = shellType.InvokeMember("CreateShortcut",
                        BindingFlags.InvokeMethod, null, shell, new object[] { linkPath });
                    if (shortcut is null)
                    {
                        return null;
                    }

                    return shortcut.GetType().InvokeMember("TargetPath",
                        BindingFlags.GetProperty, null, shortcut, null) as string;
                }
                finally
                {
                    if (shortcut is not null && Marshal.IsComObject(shortcut))
                    {
                        Marshal.FinalReleaseComObject(shortcut);
                    }

                    if (Marshal.IsComObject(shell))
                    {
                        Marshal.FinalReleaseComObject(shell);
                    }
                }
            }
            catch
            {
                return null;
            }
        }

        private static string ResolveLauncherDirectory()
        {
            if (!string.IsNullOrEmpty(LauncherExePath))
            {
                var dir = Path.GetDirectoryName(LauncherExePath);
                if (!string.IsNullOrEmpty(dir))
                {
                    return dir;
                }
            }

            return AppContext.BaseDirectory;
        }
    }
}
