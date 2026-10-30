using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using SharpCompress.Archives;
using SharpCompress.Common;

namespace DNFLogin.Services
{
    /// <summary>自我更新的阶段（供更新窗口显示文案）。</summary>
    public enum LauncherUpdateStage
    {
        /// <summary>正在下载更新包。</summary>
        Downloading,
        /// <summary>正在解压并挑出登录器 exe。</summary>
        Extracting,
        /// <summary>正在落地新登录器。</summary>
        Staging,
    }

    /// <summary>一次进度回调（更新窗口只读它，不做任何判断）。</summary>
    public sealed class LauncherUpdateProgress
    {
        public LauncherUpdateStage Stage { get; init; }
        public string Message { get; init; } = "";
        /// <summary>0~100；总量未知（服务端没给 Content-Length）时为 0。</summary>
        public int Percent { get; init; }
        public long Done { get; init; }
        public long Total { get; init; }
        public double SpeedMBps { get; init; }
    }

    /// <summary>更新计划：下载解压完成后算出的一组路径，交给 <see cref="LauncherSelfUpdater.ApplyAndRestart"/>。</summary>
    public sealed class LauncherUpdatePlan
    {
        /// <summary>当前正在运行的登录器 exe（更新后要被彻底删除的那个）。</summary>
        public string OldExePath { get; init; } = "";
        /// <summary>新登录器最终落地路径。</summary>
        public string FinalExePath { get; init; } = "";
        /// <summary>新登录器暂存路径（同目录下的 .dnfupdate 文件，由替换脚本改名到 FinalExePath）。</summary>
        public string StagedExePath { get; init; } = "";
        /// <summary>解压用的临时目录（替换完成后由脚本删除）。</summary>
        public string TempDir { get; init; } = "";
        /// <summary>下载下来的原始更新包路径（排障用）。</summary>
        public string PackagePath { get; init; } = "";
    }

    /// <summary>
    /// 登录器自我更新：下载 → 解压 → 挑出新登录器 → 暂存到目标目录 → 交给一个
    /// **独立的 cmd 脚本**完成"等本进程退出 → 彻底删除原登录器 → 落地新登录器 → 启动它"。
    ///
    /// ── 为什么必须用一个外部脚本 ──────────────────────────────────────────────
    /// Windows 上正在运行的 exe 自己删不掉自己（文件被映射锁定），所以"删除原登录器"
    /// 这件事只能由另一个进程在本进程退出之后做。这里选 cmd 而不是 PowerShell：
    /// cmd 一定存在、启动快、不依赖执行策略。
    ///
    /// ── 中文路径怎么处理（关键坑）────────────────────────────────────────────
    /// 登录器文件名含中文（如"YH登录器-90外网版.exe"），而 .bat 文件内容是按控制台
    /// OEM 代码页解释的 —— 把中文路径写进 .bat 会乱码，`del` 直接失败。
    /// 解法：**bat 内容保持纯 ASCII**，所有真实路径通过环境变量传进去
    /// （ProcessStartInfo.Environment 是 Unicode，cmd 展开 %VAR% 时也是 Unicode）。
    /// 因此下面那段脚本里找不到任何中文，路径全部来自 %DNFUPD_*%。
    ///
    /// ── 桌面快捷方式 ────────────────────────────────────────────────────────
    /// 新登录器用**自己的文件名**落地，旧文件名可能和新文件名不同；如果不管，
    /// 桌面上那个指向旧 exe 的快捷方式就变成了死图标。所以这里会尽力迁移一次
    /// （见 EmbeddedClientDetector.MigrateDesktopShortcut），失败只记日志、不阻断更新。
    /// </summary>
    public static class LauncherSelfUpdater
    {
        /// <summary>
        /// 整个更新包的下载上限。单文件登录器约 130 MB，不能沿用短接口的 30 秒超时，
        /// 否则慢速网络会在正常下载过程中被强制判定失败。
        /// </summary>
        private static readonly TimeSpan DownloadTimeout = TimeSpan.FromMinutes(30);

        /// <summary>挑登录器 exe 时的体积下限：小于这个大小的 exe 基本不可能是登录器（单文件发布约 14MB）。</summary>
        private const long MinLauncherExeBytes = 512 * 1024;

        /// <summary>暂存文件后缀（不是 .exe，避免被"从包里找 exe"的逻辑误认）。</summary>
        private const string StagedSuffix = ".dnfupdate";

        /// <summary>客户端主程序名：包里即使混进了客户端也不能被当成登录器。 */
        private static readonly string[] ClientExeNames = { "DNF.exe", "NoPack.exe" };

        /// <summary>
        /// 下载更新包 → 解压 → 挑出新登录器 → 暂存到目标目录。
        /// 成功返回非 null 的计划；失败返回 (null, 可读错误)。
        /// 取消时抛 <see cref="OperationCanceledException"/>（调用方自行处理）。
        /// </summary>
        public static async Task<(LauncherUpdatePlan? Plan, string Error)> DownloadAndStageAsync(
            string downloadUrl, IProgress<LauncherUpdateProgress>? progress, CancellationToken token)
        {
            var oldExePath = EmbeddedClientDetector.LauncherExePath;
            if (string.IsNullOrWhiteSpace(oldExePath) || !File.Exists(oldExePath))
            {
                return (null, "无法定位当前登录器文件，请手动重新下载登录器。");
            }

            var workId = Guid.NewGuid().ToString("N");
            var packagePath = Path.Combine(Path.GetTempPath(), $"dnf_launcher_update_{workId}.pkg");
            var extractDir = Path.Combine(Path.GetTempPath(), $"dnf_launcher_update_{workId}");

            try
            {
                // ① 下载
                await DownloadAsync(downloadUrl, packagePath, progress, token).ConfigureAwait(false);

                // ② 解压 + 挑 exe
                progress?.Report(new LauncherUpdateProgress
                {
                    Stage = LauncherUpdateStage.Extracting,
                    Message = "正在解压更新包…",
                });
                var newExe = await Task.Run(() => ExtractAndPickLauncher(packagePath, extractDir, token), token)
                    .ConfigureAwait(false);
                if (newExe is null)
                {
                    return (null, "更新包里没有找到登录器程序。\n\n" +
                                  "请确认压缩包里放的是登录器 exe（包里只放登录器一个 exe 即可，文件名不限），" +
                                  "然后联系管理员重新上传。");
                }

                // ③ 暂存到目标目录（与旧登录器同目录，保证同盘移动）
                progress?.Report(new LauncherUpdateProgress
                {
                    Stage = LauncherUpdateStage.Staging,
                    Message = "正在准备新版本登录器…",
                });
                var targetDir = Path.GetDirectoryName(oldExePath)!;
                var finalExePath = Path.Combine(targetDir, Path.GetFileName(newExe));
                var stagedExePath = finalExePath + StagedSuffix;

                // 同名更新时 finalExePath == oldExePath：此时绝不能直接往 finalExePath 写
                // （旧进程正锁着它），必须先落到 .dnfupdate，由脚本在旧进程退出后再改名。
                try
                {
                    File.Copy(newExe, stagedExePath, overwrite: true);
                }
                catch (Exception ex)
                {
                    return (null, "无法把新登录器写入登录器目录（可能没有写入权限，或目录被占用）：" + ex.Message);
                }

                return (new LauncherUpdatePlan
                {
                    OldExePath = oldExePath,
                    FinalExePath = finalExePath,
                    StagedExePath = stagedExePath,
                    TempDir = extractDir,
                    PackagePath = packagePath,
                }, "");
            }
            catch (OperationCanceledException)
            {
                CleanupQuietly(extractDir, packagePath);
                throw;
            }
            catch (Exception ex)
            {
                CleanupQuietly(extractDir, packagePath);
                return (null, "更新失败：" + ex.Message);
            }
        }

        /// <summary>
        /// 执行替换：迁移桌面快捷方式 → 写替换脚本 → 拉起脚本。
        /// 调用方必须在本方法返回后**尽快退出本进程**（脚本正在等它退出）。
        /// 返回 (是否已成功拉起脚本, 错误说明)。
        /// </summary>
        public static (bool Ok, string Error) ApplyAndRestart(LauncherUpdatePlan plan)
        {
            if (plan is null || string.IsNullOrWhiteSpace(plan.FinalExePath))
            {
                return (false, "更新计划无效。");
            }

            try
            {
                // 桌面快捷方式迁移（尽力而为）：新登录器用自己的文件名落地，
                // 旧名字的快捷方式会变成死图标，这里把它改成指向新文件。
                EmbeddedClientDetector.MigrateDesktopShortcut(plan.OldExePath, plan.FinalExePath);
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherSelfUpdater: 桌面快捷方式迁移失败（不影响更新）-> " + ex.Message);
            }

            try
            {
                var batPath = Path.Combine(Path.GetTempPath(), $"dnf_launcher_apply_{Guid.NewGuid():N}.bat");
                File.WriteAllText(batPath, BuildApplyScript(), new System.Text.UTF8Encoding(false));

                var psi = new ProcessStartInfo
                {
                    FileName = "cmd.exe",
                    // /c 后面跟脚本路径：cmd 自己按 Unicode 处理命令行，中文路径没问题。
                    Arguments = "/c \"" + batPath + "\"",
                    UseShellExecute = false,
                    CreateNoWindow = true,
                    WorkingDirectory = Path.GetTempPath(),
                };
                // ★ 真实路径全部走环境变量：见类注释里"中文路径"那一节。
                psi.Environment["DNFUPD_OLD"] = plan.OldExePath;
                psi.Environment["DNFUPD_NEW"] = plan.StagedExePath;
                psi.Environment["DNFUPD_FINAL"] = plan.FinalExePath;
                psi.Environment["DNFUPD_TMP"] = plan.TempDir ?? "";
                psi.Environment["DNFUPD_PKG"] = plan.PackagePath ?? "";
                psi.Environment["DNFUPD_LOG"] = Path.Combine(Path.GetTempPath(), "dnf_launcher_update.log");

                // 这个子进程不能被绑到任何 Job Object 上：它必须活过本进程的退出。
                Process.Start(psi);
                App.LogStartup("LauncherSelfUpdater: 替换脚本已拉起，准备退出本进程。old=" + plan.OldExePath +
                               " new=" + plan.FinalExePath);
                return (true, "");
            }
            catch (Exception ex)
            {
                return (false, "无法启动更新替换程序：" + ex.Message +
                               "\n\n请手动关闭登录器，然后到登录器目录里把 " +
                               Path.GetFileName(plan.StagedExePath) + " 改名为 " +
                               Path.GetFileName(plan.FinalExePath) + " 并运行。");
            }
        }

        /// <summary>
        /// 替换脚本正文。**必须是纯 ASCII**（原因见类注释），所有路径来自环境变量。
        ///
        /// 流程：
        ///   1. 循环尝试删除旧登录器 —— 删得掉就说明旧进程已经退出（运行中的 exe 删不掉），
        ///      这比解析 tasklist 输出判断 PID 可靠得多；
        ///   2. 把暂存的新登录器改名到最终路径；
        ///   3. 启动新登录器；
        ///   4. 清理临时文件与自身。
        /// </summary>
        private static string BuildApplyScript()
        {
            // 600 次 × 约 1 秒 ≈ 最长等 10 分钟；再等不到就放弃并记日志，不留一个死循环的 cmd。
            return string.Join("\r\n", new[]
            {
                "@echo off",
                "setlocal",
                "set /a dnfTries=0",
                "",
                "rem ---- 1) wait for the old launcher to exit, then delete it permanently ----",
                ":dnfWaitOld",
                "if not exist \"%DNFUPD_OLD%\" goto dnfApply",
                "attrib -r -h -s \"%DNFUPD_OLD%\" >nul 2>nul",
                "del /f /q \"%DNFUPD_OLD%\" >nul 2>nul",
                "if not exist \"%DNFUPD_OLD%\" goto dnfApply",
                "set /a dnfTries+=1",
                "if %dnfTries% GEQ 600 goto dnfGiveUp",
                "ping -n 2 127.0.0.1 >nul",
                "goto dnfWaitOld",
                "",
                "rem ---- 2) move the staged build into its final name and start it ----",
                ":dnfApply",
                "rem del does NOT use the Recycle Bin: the old launcher is gone for good.",
                "if exist \"%DNFUPD_OLD%\" del /f /q \"%DNFUPD_OLD%\" >nul 2>nul",
                "if not exist \"%DNFUPD_FINAL%\" move /y \"%DNFUPD_NEW%\" \"%DNFUPD_FINAL%\" >nul 2>nul",
                "if not exist \"%DNFUPD_FINAL%\" goto dnfRetryMove",
                "goto dnfStart",
                "",
                ":dnfRetryMove",
                "ping -n 2 127.0.0.1 >nul",
                "move /y \"%DNFUPD_NEW%\" \"%DNFUPD_FINAL%\" >nul 2>nul",
                "if not exist \"%DNFUPD_FINAL%\" goto dnfGiveUp",
                "",
                ":dnfStart",
                "start \"\" \"%DNFUPD_FINAL%\"",
                "goto dnfCleanup",
                "",
                "rem ---- 3) old process never exited: leave everything as-is and write a log ----",
                ":dnfGiveUp",
                "echo [%DATE% %TIME%] update aborted: old launcher still running or file still locked. > \"%DNFUPD_LOG%\"",
                "echo old=%DNFUPD_OLD% >> \"%DNFUPD_LOG%\"",
                "echo new=%DNFUPD_NEW% >> \"%DNFUPD_LOG%\"",
                "",
                ":dnfCleanup",
                "if exist \"%DNFUPD_NEW%\" del /f /q \"%DNFUPD_NEW%\" >nul 2>nul",
                "if not \"%DNFUPD_TMP%\"==\"\" if exist \"%DNFUPD_TMP%\" rmdir /s /q \"%DNFUPD_TMP%\" >nul 2>nul",
                "if not \"%DNFUPD_PKG%\"==\"\" if exist \"%DNFUPD_PKG%\" del /f /q \"%DNFUPD_PKG%\" >nul 2>nul",
                "del /f /q \"%~f0\" >nul 2>nul",
                "endlocal",
                "exit /b 0",
                "",
            });
        }

        /// <summary>下载更新包到本地临时文件，带进度与速度。</summary>
        private static async Task DownloadAsync(
            string downloadUrl, string packagePath, IProgress<LauncherUpdateProgress>? progress, CancellationToken token)
        {
            progress?.Report(new LauncherUpdateProgress
            {
                Stage = LauncherUpdateStage.Downloading,
                Message = "正在连接更新服务器…",
            });

            var stopwatch = Stopwatch.StartNew();
            using var client = new HttpClient { Timeout = DownloadTimeout };
            using var response = await client
                .GetAsync(downloadUrl, HttpCompletionOption.ResponseHeadersRead, token).ConfigureAwait(false);
            response.EnsureSuccessStatusCode();

            var total = response.Content.Headers.ContentLength ?? -1L;
            await using var src = await response.Content.ReadAsStreamAsync(token).ConfigureAwait(false);
            await using var dst = new FileStream(packagePath, FileMode.Create, FileAccess.Write, FileShare.None, 81920, true);

            var buffer = new byte[81920];
            long done = 0;
            int read;
            while ((read = await src.ReadAsync(buffer.AsMemory(0, buffer.Length), token).ConfigureAwait(false)) > 0)
            {
                await dst.WriteAsync(buffer.AsMemory(0, read), token).ConfigureAwait(false);
                done += read;
                var elapsed = Math.Max(stopwatch.Elapsed.TotalSeconds, 0.1);
                progress?.Report(new LauncherUpdateProgress
                {
                    Stage = LauncherUpdateStage.Downloading,
                    Message = "正在下载新版本登录器…",
                    Percent = total > 0 ? (int)(done * 100 / total) : 0,
                    Done = done,
                    Total = total > 0 ? total : 0,
                    SpeedMBps = done / 1024.0 / 1024.0 / elapsed,
                });
            }

            token.ThrowIfCancellationRequested();
        }

        /// <summary>
        /// 解压更新包并挑出登录器 exe。
        ///
        /// 需求确认过的口径：**压缩包里只有登录器一个 exe，且文件名不能作为依据**
        /// （登录器可能会改名），所以这里按"内容特征"挑：
        ///   1. 递归收集所有 .exe；
        ///   2. 排掉客户端主程序（DNF.exe / NoPack.exe）；
        ///   3. 排掉明显不是登录器的小文件（&lt; 512KB）；
        ///   4. 剩下的：优先与当前登录器同名的；否则取体积最大的那个。
        /// 兜底：如果整包其实就是一个裸 exe（不是压缩包），直接把它当登录器。
        /// </summary>
        private static string? ExtractAndPickLauncher(string packagePath, string extractDir, CancellationToken token)
        {
            Directory.CreateDirectory(extractDir);

            try
            {
                using var fileStream = new FileStream(packagePath, FileMode.Open, FileAccess.Read, FileShare.Read);
                using var archive = ArchiveFactory.OpenArchive(fileStream);
                foreach (var entry in archive.Entries)
                {
                    token.ThrowIfCancellationRequested();
                    if (entry.IsDirectory)
                    {
                        continue;
                    }

                    var rel = entry.Key?.Replace('\\', '/') ?? "";
                    var dest = SafeCombine(extractDir, rel);
                    if (dest is null)
                    {
                        continue; // 非法条目（绝对路径 / ../ 穿越）跳过
                    }

                    var dir = Path.GetDirectoryName(dest);
                    if (!string.IsNullOrEmpty(dir))
                    {
                        Directory.CreateDirectory(dir);
                    }

                    using var stream = entry.OpenEntryStream();
                    using var outStream = new FileStream(dest, FileMode.Create, FileAccess.Write, FileShare.Read);
                    stream.CopyTo(outStream);
                }
            }
            catch (OperationCanceledException)
            {
                throw;
            }
            catch (Exception ex)
            {
                // 解压失败：可能这压根不是压缩包，而是一个裸 exe 直链。
                App.LogStartup("LauncherSelfUpdater: 解压失败，尝试按裸 exe 处理 -> " + ex.Message);
                if (LooksLikePortableExecutable(packagePath))
                {
                    var bareTarget = Path.Combine(extractDir, GuessBareExeName(packagePath));
                    File.Copy(packagePath, bareTarget, overwrite: true);
                    return bareTarget;
                }
                return null;
            }

            var candidates = Directory.EnumerateFiles(extractDir, "*.exe", SearchOption.AllDirectories)
                .Where(p => !ClientExeNames.Any(n => string.Equals(Path.GetFileName(p), n, StringComparison.OrdinalIgnoreCase)))
                .Where(p => new FileInfo(p).Length >= MinLauncherExeBytes)
                .ToList();

            if (candidates.Count == 0)
            {
                return null;
            }
            if (candidates.Count == 1)
            {
                return candidates[0];
            }

            // 多个候选：优先与当前登录器同名（发布方通常只在版本号上做区分），否则取体积最大的。
            var currentName = Path.GetFileName(EmbeddedClientDetector.LauncherExePath);
            var sameName = candidates.FirstOrDefault(
                p => string.Equals(Path.GetFileName(p), currentName, StringComparison.OrdinalIgnoreCase));
            if (sameName is not null)
            {
                return sameName;
            }

            var picked = candidates.OrderByDescending(p => new FileInfo(p).Length).First();
            App.LogStartup("LauncherSelfUpdater: 更新包里有 " + candidates.Count + " 个 exe，已选体积最大的：" +
                           Path.GetFileName(picked));
            return picked;
        }

        /// <summary>文件头是不是 PE（"MZ"）。</summary>
        private static bool LooksLikePortableExecutable(string path)
        {
            try
            {
                using var fs = File.OpenRead(path);
                return fs.ReadByte() == 'M' && fs.ReadByte() == 'Z';
            }
            catch
            {
                return false;
            }
        }

        /// <summary>
        /// 裸 exe 直链没有压缩包内的文件名。统一使用规范文件名，避免旧版本或
        /// 临时命名为“登录器.exe”的客户端在更新后继续保留中性名称。
        /// </summary>
        private static string GuessBareExeName(string packagePath)
        {
            return "落叶DNF登录器.exe";
        }

        /// <summary>条目相对路径安全拼接到目标目录；存在穿越/绝对路径时返回 null。</summary>
        private static string? SafeCombine(string root, string rel)
        {
            if (string.IsNullOrWhiteSpace(rel))
            {
                return null;
            }
            var full = Path.GetFullPath(Path.Combine(root, rel));
            var rootFull = Path.GetFullPath(root);
            if (!full.StartsWith(rootFull.TrimEnd('\\', '/') + "\\", StringComparison.OrdinalIgnoreCase) &&
                !string.Equals(full, rootFull, StringComparison.OrdinalIgnoreCase))
            {
                return null;
            }
            return full;
        }

        /// <summary>清理临时产物；失败只记日志（绝不能让清理把更新本身搞崩）。</summary>
        private static void CleanupQuietly(string extractDir, string packagePath)
        {
            try
            {
                if (!string.IsNullOrEmpty(extractDir) && Directory.Exists(extractDir))
                {
                    Directory.Delete(extractDir, recursive: true);
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherSelfUpdater: 清理解压目录失败 -> " + ex.Message);
            }

            try
            {
                if (!string.IsNullOrEmpty(packagePath) && File.Exists(packagePath))
                {
                    File.Delete(packagePath);
                }
            }
            catch (Exception ex)
            {
                App.LogStartup("LauncherSelfUpdater: 清理更新包失败 -> " + ex.Message);
            }
        }
    }
}
