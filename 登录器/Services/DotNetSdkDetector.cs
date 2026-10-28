using System;
using System.Diagnostics;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>
    /// .NET SDK 检测：启动时判断本机是否安装 .NET SDK（dotnet --list-sdks）。
    /// 仅用于服务端构建/更新等需要 SDK 的场景提示，不阻塞登录器本体启动。
    /// </summary>
    public static class DotNetSdkDetector
    {
        /// <summary>返回已安装的 SDK 版本列表；dotnet 不存在/检测失败时返回空列表。</summary>
        public static async Task<string[]> GetInstalledSdksAsync()
        {
            try
            {
                using var p = new Process
                {
                    StartInfo = new ProcessStartInfo
                    {
                        FileName = "dotnet",
                        Arguments = "--list-sdks",
                        UseShellExecute = false,
                        RedirectStandardOutput = true,
                        RedirectStandardError = true,
                        CreateNoWindow = true
                    }
                };

                if (!p.Start())
                {
                    return Array.Empty<string>();
                }

                var stdout = await p.StandardOutput.ReadToEndAsync().ConfigureAwait(false);
                await p.WaitForExitAsync().ConfigureAwait(false);

                if (p.ExitCode != 0)
                {
                    return Array.Empty<string>();
                }

                var lines = stdout.Split(new[] { '\r', '\n' }, StringSplitOptions.RemoveEmptyEntries);
                return lines;
            }
            catch (Exception)
            {
                // dotnet 命令不存在（Win32Exception）或其它异常 → 视为未安装
                return Array.Empty<string>();
            }
        }

        /// <summary>是否为便携版（self-contained）发布：无需系统安装 .NET 也可运行</summary>
        public static bool IsSelfContained()
        {
            // 单文件 self-contained 发布时，运行时目录不带 runtimeconfig；此处仅作提示用
            var baseDir = AppContext.BaseDirectory;
            var runtimeCfg = System.IO.Path.Combine(baseDir, "YH登录器.runtimeconfig.json");
            return !System.IO.File.Exists(runtimeCfg);
        }
    }
}
