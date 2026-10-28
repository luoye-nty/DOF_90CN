#if EXTERNAL
using System;
using System.Collections.Generic;
using System.IO;
using System.Security.Cryptography;
using System.Text;

namespace DNFLogin.Services
{
    /// <summary>
    /// 服务端指定 DLL：校验 + 注入（仅外网版编译）。
    ///
    /// 流程（与需求确认一致）：
    ///   1. 管理工具只上传「文件名 + SHA256」，dll 本体由服主随客户端分发；
    ///   2. 登录器在客户端根目录（DNF.exe 同目录）找同名文件，算 SHA256 后比对；
    ///   3. 校验通过 → 启动游戏后在既有注入点注入；校验不通过 → 按该条的策略处理：
    ///        block = 弹窗提示并**拒绝启动游戏**；
    ///        skip  = 只跳过注入（记日志），照常进游戏；
    ///   4. 注入失败（权限/位数等）→ 弹窗提示 + 写日志，不静默。
    ///
    /// 为什么哈希要"逐字节比"而不是比长度：这是防"客户端被人塞了一个同名假 dll"的最后一道闸，
    /// 比错了等于这道闸没装。
    /// </summary>
    public static class ServerDllLoader
    {
        /// <summary>服务端下发的一条 dll 要求。</summary>
        public sealed class Requirement
        {
            public string FileName { get; set; } = "";
            public string Sha256 { get; set; } = "";
            /// <summary>block=校验不过拒绝启动游戏；skip=跳过注入。</summary>
            public string OnMismatch { get; set; } = "skip";

            public bool IsBlocking => string.Equals(OnMismatch, "block", StringComparison.OrdinalIgnoreCase);
        }

        /// <summary>一条要求的校验结论。</summary>
        public sealed class CheckResult
        {
            public Requirement Requirement { get; set; } = new Requirement();
            public bool Ok { get; set; }
            public string FullPath { get; set; } = "";
            public string Reason { get; set; } = "";
        }

        /// <summary>
        /// 校验全部要求。clientRoot 为空或目录不存在时，所有条目都按"文件缺失"处理。
        /// </summary>
        public static List<CheckResult> Validate(string clientRoot, IReadOnlyList<Requirement> requirements)
        {
            var results = new List<CheckResult>();
            if (requirements == null || requirements.Count == 0)
            {
                return results;
            }

            foreach (var requirement in requirements)
            {
                var result = new CheckResult { Requirement = requirement };
                results.Add(result);

                var fileName = (requirement.FileName ?? "").Trim();
                if (fileName.Length == 0)
                {
                    result.Reason = "服务端下发的文件名为空";
                    continue;
                }
                // 文件名净化：清单由服务端下发，只当它是"文件名"而不是"路径"。
                // 不做这层判断的话，..\..\x.dll 或 C:\x.dll 会让校验/注入指向客户端目录之外的文件。
                if (fileName != Path.GetFileName(fileName) || Path.IsPathRooted(fileName) ||
                    fileName.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0)
                {
                    result.Reason = "服务端下发的文件名非法（只允许客户端根目录下的文件名）";
                    continue;
                }
                if (string.IsNullOrWhiteSpace(clientRoot) || !Directory.Exists(clientRoot))
                {
                    result.Reason = "未配置客户端目录（无法定位 DNF.exe 所在目录）";
                    continue;
                }

                var fullPath = Path.Combine(clientRoot, fileName);
                result.FullPath = fullPath;
                if (!File.Exists(fullPath))
                {
                    result.Reason = "客户端根目录缺少该文件";
                    continue;
                }

                // 再确认一次结果仍在客户端根目录内（纵深防御；上面的 GetFileName 判断已经是主要闸门）。
                try
                {
                    var rootFull = Path.GetFullPath(clientRoot);
                    var pathFull = Path.GetFullPath(fullPath);
                    if (!pathFull.StartsWith(rootFull.TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar,
                            StringComparison.OrdinalIgnoreCase))
                    {
                        result.Reason = "服务端下发的文件名指向客户端目录之外";
                        result.FullPath = string.Empty;
                        continue;
                    }
                }
                catch (Exception ex)
                {
                    result.Reason = "解析文件路径失败：" + ex.Message;
                    result.FullPath = string.Empty;
                    continue;
                }

                string actual;
                try
                {
                    actual = ComputeSha256(fullPath);
                }
                catch (Exception ex)
                {
                    result.Reason = "读取文件失败：" + ex.Message;
                    continue;
                }

                var expected = (requirement.Sha256 ?? "").Trim();
                if (!string.Equals(actual, expected, StringComparison.OrdinalIgnoreCase))
                {
                    result.Reason = $"哈希不一致（本地 {Shorten(actual)}，服务端 {Shorten(expected)}）";
                    continue;
                }

                result.Ok = true;
            }

            return results;
        }

        /// <summary>是否存在"必须拒绝启动"的失败项。</summary>
        public static bool HasBlockingFailure(IReadOnlyList<CheckResult> results)
        {
            if (results == null)
            {
                return false;
            }
            foreach (var result in results)
            {
                if (!result.Ok && result.Requirement.IsBlocking)
                {
                    return true;
                }
            }
            return false;
        }

        /// <summary>拼"拒绝启动"的弹窗文案（逐条列出失败原因，玩家可以直接截图找服主）。</summary>
        public static string BuildBlockMessage(IReadOnlyList<CheckResult> results)
        {
            var builder = new StringBuilder();
            builder.AppendLine("客户端缺少服务器要求的文件，或文件被修改过，已拒绝启动游戏。");
            builder.AppendLine();
            builder.AppendLine("请把下面的信息发给服主，并重新安装完整客户端：");
            builder.AppendLine();
            foreach (var result in results)
            {
                if (result.Ok || !result.Requirement.IsBlocking)
                {
                    continue;
                }
                builder.AppendLine($"· {result.Requirement.FileName}：{result.Reason}");
            }
            return builder.ToString().TrimEnd();
        }

        /// <summary>拼"跳过注入"的日志文案（不打扰玩家，只留痕）。</summary>
        public static string BuildSkipNote(IReadOnlyList<CheckResult> results)
        {
            var parts = new List<string>();
            foreach (var result in results)
            {
                if (!result.Ok)
                {
                    parts.Add($"{result.Requirement.FileName}({result.Reason})");
                }
            }
            return parts.Count == 0 ? "" : string.Join("；", parts);
        }

        /// <summary>
        /// 注入校验通过的 dll。返回失败条目（文件名 + 原因），调用方负责弹窗与日志。
        /// 注入顺序按服务端返回顺序（列表顺序即 sort_order）。
        /// </summary>
        public static List<string> Inject(int pid, IReadOnlyList<CheckResult> results)
        {
            var failures = new List<string>();
            if (results == null)
            {
                return failures;
            }
            foreach (var result in results)
            {
                if (!result.Ok || string.IsNullOrWhiteSpace(result.FullPath))
                {
                    continue;
                }
                bool ok;
                try
                {
                    ok = DllInjector.Inject(pid, result.FullPath);
                }
                catch (Exception ex)
                {
                    ok = false;
                    failures.Add($"{result.Requirement.FileName}：注入异常 {ex.Message}");
                    continue;
                }
                if (!ok)
                {
                    failures.Add($"{result.Requirement.FileName}：注入失败（可能需要以管理员身份运行登录器）");
                }
            }
            return failures;
        }

        /// <summary>算文件 SHA-256（小写 hex）。</summary>
        public static string ComputeSha256(string path)
        {
            using var stream = File.OpenRead(path);
            using var sha = SHA256.Create();
            var hash = sha.ComputeHash(stream);
            var builder = new StringBuilder(hash.Length * 2);
            foreach (var b in hash)
            {
                builder.Append(b.ToString("x2"));
            }
            return builder.ToString();
        }

        private static string Shorten(string hex)
        {
            if (string.IsNullOrEmpty(hex))
            {
                return "(空)";
            }
            return hex.Length <= 12 ? hex : hex.Substring(0, 12) + "…";
        }
    }
}
#endif
