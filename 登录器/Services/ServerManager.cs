using System;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 从 instance.json 读取的服务端关键配置
    /// </summary>
    public sealed class ServerInfo
    {
        public string RootPath { get; set; } = "";
        public string DatabaseMode { get; set; } = "";   // portable=MySQL / sqlite
        public string DatabasePath { get; set; } = "";
        public string DatabaseHost { get; set; } = "";
        public int DatabasePort { get; set; }
        public string DatabaseName { get; set; } = "";
        public string DatabaseUser { get; set; } = "";
        public string DatabasePassword { get; set; } = "";
        public string InstallationId { get; set; } = "";
        public string AdvertiseIp { get; set; } = "";
        public string ChannelListen { get; set; } = "";
        public string AdminListen { get; set; } = "";
        public string AdminToken { get; set; } = "";
        public string ShardId { get; set; } = "";
        public string PvfPath { get; set; } = "";
        public string ChannelInfoPath { get; set; } = "";
        public string ClientDirectory { get; set; } = "";
        public string GameOuterToken { get; set; } = "";
    }

    /// <summary>
    /// 服务端初始化/运行状态探测结果（只读，不产生副作用）
    /// </summary>
    public sealed class ServerInspection
    {
        /// <summary>根目录路径</summary>
        public string RootPath { get; set; } = "";

        /// <summary>根目录是否存在</summary>
        public bool RootExists { get; set; }

        /// <summary>instance.json 是否存在</summary>
        public bool InstanceExists { get; set; }

        /// <summary>instance.json 是否可解析</summary>
        public bool InstanceValid { get; set; }

        /// <summary>数据库模式（portable=MySQL / sqlite，来自 instance.json）</summary>
        public string DatabaseMode { get; set; } = "";

        /// <summary>runtime\mysql 目录是否存在</summary>
        public bool MySqlDirExists { get; set; }

        /// <summary>MySQL data 是否已初始化（data\mysql 存在或 data-state.json phase=ready）</summary>
        public bool MySqlDataReady { get; set; }

        /// <summary>SQLite 数据库文件是否存在</summary>
        public bool SqliteDbExists { get; set; }

        /// <summary>业务库表数量；无法查询为 -1</summary>
        public int TableCount { get; set; } = -1;

        /// <summary>13306 端口（MySQL）是否已监听</summary>
        public bool MySqlPortOpen { get; set; }

        /// <summary>18111 管理端口（DNF90Server）是否已监听</summary>
        public bool AdminPortOpen { get; set; }

        /// <summary>频道端口是否已监听</summary>
        public bool ChannelPortOpen { get; set; }

        /// <summary>是否需要一键初始化（instance.json 缺失或数据库未就绪）</summary>
        public bool NeedsInit { get; set; }

        /// <summary>是否可直接启动</summary>
        public bool Ready { get; set; }

        /// <summary>状态摘要（展示给用户）</summary>
        public string Message { get; set; } = "";
    }

    /// <summary>
    /// DNF90 服务端一键启动/停止/配置管理。
    /// 启动链路对齐 DNF90Launcher：MySQL 模式先启 mysqld（13306）再启 DNF90Server（18111），
    /// SQLite 模式直接启 DNF90Server；停止先杀 Server 再杀本目录下的 mysqld。
    /// </summary>
    public static class ServerManager
    {
        static ServerManager()
        {
            // 注册代码页提供程序，使 GBK(936) 编码在 .NET Core 下可用（写 mysql.ini 需要）
            Encoding.RegisterProvider(CodePagesEncodingProvider.Instance);
        }

        /// <summary>mysqld 端口（由 mysql.ini 固定）</summary>
        public const int MySqlPort = 13306;

        /// <summary>服务端管理 HTTP 端口</summary>
        public const int AdminPort = 18111;

        /// <summary>mysqld 启动参数（--console 便于日志输出，--no-monitor 不派生守护进程）</summary>

        /// <summary>DNF90Server 启动参数（工作目录为 runtime，与原版 DNF90Control 一致）</summary>
        public const string ServerArgs = "-config configs\\dnfbridge.toml";

        /// <summary>探测 TCP 端口是否可连</summary>
        public static bool IsPortOpen(string host, int port)
        {
            try
            {
                using var client = new TcpClient();
                var ar = client.BeginConnect(host, port, null, null);
                return ar.AsyncWaitHandle.WaitOne(300) && client.Connected;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>探测 UDP 端口是否在线（WireGuard 对未知包静默丢弃）：
        /// 对端回 ICMP Port Unreachable（ConnectionReset）视为端口关闭；
        /// 超时无响应视为端口在听（在线）。</summary>
        public static bool IsUdpPortOpen(string host, int port, int timeoutMs = 2500)
        {
            try
            {
                using var udp = new UdpClient(host, port);
                udp.Client.ReceiveTimeout = timeoutMs;
                udp.Send(new byte[] { 0xaa }, 1);
                try
                {
                    var ep = new IPEndPoint(IPAddress.Any, 0);
                    udp.Receive(ref ep);
                    return true;
                }
                catch (SocketException ex)
                {
                    return ex.SocketErrorCode == SocketError.TimedOut;
                }
            }
            catch (SocketException ex)
            {
                return ex.SocketErrorCode != SocketError.ConnectionReset;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>mysqld 是否已在运行（按端口判断）</summary>
        public static bool MySqlRunning => IsPortOpen("127.0.0.1", MySqlPort);

        /// <summary>DNF90Server 是否已在运行（按管理端口判断）</summary>
        public static bool ServerRunning => IsPortOpen("127.0.0.1", AdminPort);

        // ==================== S4 系列（S4A12 / S4A21）服务端 ====================

        /// <summary>S4 系列频道监听端口（DfoServer，客户端通过该端口连接）</summary>
        public const int S4ChannelPort = 7001;
        /// <summary>S4 系列 7002 认证端口（Ed25519 验签/防重放/账号校验收口，s4authcore.dll 直连）。</summary>
        public const int S4AuthPort = 7002;

        /// <summary>S4 系列 Web GM 端口（ASP.NET Core GM 服务）</summary>
        public const int S4GmPort = 5050;

        /// <summary>
        /// S4 版本在线判定探针端口（与 S4OpsTool probe.go 约定对齐）：
        /// S4A21 → 18888，S4A12 → 16666。两版本 DfoServer 共用 7001 频道口，
        /// S4OpsTool 在服务端启动成功后才开放对应探针口，登录器据此区分版本在线状态。
        /// </summary>
        public static int S4ProbePortForProfile(string profileId)
        {
            var p = profileId ?? "";
            if (p.IndexOf("A21", StringComparison.OrdinalIgnoreCase) >= 0)
            {
                return 18888;
            }
            if (p.IndexOf("A12", StringComparison.OrdinalIgnoreCase) >= 0)
            {
                return 16666;
            }
            return 0;
        }

        /// <summary>S4 系列服务端是否运行（按频道端口判断；仅 7001 端口无法区分 A12/A21，多版本在线判定请用 S4ServerRunningForRoot）</summary>
        public static bool S4ServerRunning => IsPortOpen("127.0.0.1", S4ChannelPort);

        /// <summary>是否存在主模块路径匹配指定 exe 的进程（用于区分同端口多版本的 S4 服务端实例）</summary>
        private static bool HasProcessMatching(string processName, string exePath)
        {
            if (string.IsNullOrWhiteSpace(exePath))
            {
                return false;
            }
            foreach (var p in Process.GetProcessesByName(processName))
            {
                try
                {
                    if (string.Equals(p.MainModule?.FileName ?? "", exePath, StringComparison.OrdinalIgnoreCase))
                    {
                        return true;
                    }
                }
                catch
                {
                    // 进程已退出或权限不足，忽略
                }
            }
            return false;
        }

        /// <summary>
        /// 按版本判断指定根目录的 S4 服务端是否运行：
        /// 频道端口 7001 在线 且 存在主模块路径匹配该目录 DfoServer.exe 的进程。
        /// 未配置根目录或无法定位 exe 时判定离线，避免 A21 运行导致 A12 误判在线。
        /// </summary>
        public static bool S4ServerRunningForRoot(string rootPath)
        {
            if (!S4ServerRunning)
            {
                return false;
            }
            var exe = LocateS4Server(rootPath);
            return exe is not null && HasProcessMatching("DfoServer", exe);
        }

        /// <summary>是否属于 S4 系列版本（非 90 即 S4）</summary>
        public static bool IsS4Profile(string profileId)
            => !string.Equals(profileId, "90", StringComparison.OrdinalIgnoreCase);

        /// <summary>
        /// 定位 S4 系列 DfoServer.exe：
        /// 支持 根目录 / bin / runtime（旧布局）、dist\win-x64（publish.bat 发布产物）、
        /// Server\DfoServer\bin\Debug|Release（build.bat 编译产物）。
        /// </summary>
        public static string? LocateS4Server(string rootPath)
        {
            if (string.IsNullOrWhiteSpace(rootPath))
            {
                return null;
            }

            var candidates = new[]
            {
                Path.Combine(rootPath, "DfoServer.exe"),
                Path.Combine(rootPath, "bin", "DfoServer.exe"),
                Path.Combine(rootPath, "runtime", "DfoServer.exe"),
                Path.Combine(rootPath, "dist", "win-x64", "DfoServer.exe"),
                Path.Combine(rootPath, "Server", "DfoServer", "bin", "Debug", "DfoServer.exe"),
                Path.Combine(rootPath, "Server", "DfoServer", "bin", "Release", "DfoServer.exe"),
            };
            return candidates.FirstOrDefault(File.Exists);
        }

        /// <summary>定位 S4 服务端根目录下的编译脚本 build.bat（服务端源码根目录标志）</summary>
        public static string? LocateS4BuildScript(string rootPath)
        {
            if (string.IsNullOrWhiteSpace(rootPath))
            {
                return null;
            }

            var script = Path.Combine(rootPath, "build.bat");
            return File.Exists(script) ? script : null;
        }

        /// <summary>定位服务端目录内的 Script.pvf（exe 输出目录优先，其次源码 Data\Pvf）</summary>
        public static string? LocateS4Pvf(string rootPath)
        {
            var exe = LocateS4Server(rootPath);
            if (exe is not null)
            {
                var pvf = Path.Combine(Path.GetDirectoryName(exe)!, "Data", "Pvf", "Script.pvf");
                if (File.Exists(pvf))
                {
                    return pvf;
                }
            }

            var src = Path.Combine(rootPath, "Server", "DfoServer", "Data", "Pvf", "Script.pvf");
            return File.Exists(src) ? src : null;
        }

        /// <summary>S4 数据库文件是否存在（exe 输出目录优先，其次源码 Data；A21 首次启动会自动创建）</summary>
        public static bool S4DatabaseExists(string rootPath)
        {
            var exe = LocateS4Server(rootPath);
            if (exe is not null)
            {
                var db = Path.Combine(Path.GetDirectoryName(exe)!, "Data", "inventory.db");
                if (File.Exists(db))
                {
                    return true;
                }
            }

            return File.Exists(Path.Combine(rootPath, "Server", "DfoServer", "Data", "inventory.db"));
        }

        /// <summary>探测可用的 dotnet 可执行文件（PATH 优先，其次 dotnet-install 用户目录）</summary>
        private static string? LocateDotnet()
        {
            try
            {
                using var p = new Process
                {
                    StartInfo = new ProcessStartInfo
                    {
                        FileName = "dotnet",
                        Arguments = "--version",
                        UseShellExecute = false,
                        RedirectStandardOutput = true,
                        RedirectStandardError = true,
                        CreateNoWindow = true
                    }
                };
                if (p.Start() && p.WaitForExit(15000) && p.ExitCode == 0)
                {
                    return "dotnet";
                }
            }
            catch
            {
                // PATH 中无 dotnet
            }

            var userInstall = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                "Microsoft", "dotnet", "dotnet.exe");
            return File.Exists(userInstall) ? userInstall : null;
        }

        /// <summary>自动安装 .NET SDK 编译环境：优先 winget（系统级、进 PATH），失败降级官方 dotnet-install.ps1（用户级）</summary>
        public static bool InstallDotNetSdk(Action<string>? log)
        {
            log?.Invoke("未检测到 .NET SDK，开始自动安装编译环境 ...");

            // 1) winget 优先：安装到系统级，build.bat 可直接探测到 dotnet
            try
            {
                var winget = new Process
                {
                    StartInfo = new ProcessStartInfo
                    {
                        FileName = "winget",
                        Arguments = "install --id Microsoft.DotNet.SDK.10 --silent --accept-package-agreements --accept-source-agreements",
                        UseShellExecute = false,
                        RedirectStandardOutput = true,
                        RedirectStandardError = true,
                        CreateNoWindow = true
                    }
                };
                if (winget.Start())
                {
                    log?.Invoke("正在通过 winget 安装 .NET 10 SDK，请稍候（可能需要几分钟）...");
                    _ = winget.StandardOutput.ReadToEndAsync();
                    _ = winget.StandardError.ReadToEndAsync();
                    if (winget.WaitForExit(600000) && winget.ExitCode == 0)
                    {
                        log?.Invoke("winget 安装 .NET SDK 完成。");
                        return LocateDotnet() is not null;
                    }

                    log?.Invoke($"winget 安装未成功（退出码 {winget.ExitCode}），尝试官方安装脚本 ...");
                }
            }
            catch
            {
                log?.Invoke("未找到 winget，尝试官方安装脚本 ...");
            }

            // 2) dotnet-install.ps1 官方脚本（用户级，安装到 %LOCALAPPDATA%\Microsoft\dotnet）
            try
            {
                var script = Path.Combine(Path.GetTempPath(), "dotnet-install-" + Guid.NewGuid().ToString("N") + ".ps1");
                var installDir = Path.Combine(
                    Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                    "Microsoft", "dotnet");
                using (var web = new System.Net.Http.HttpClient())
                {
                    var bytes = web.GetByteArrayAsync("https://dot.net/v1/dotnet-install.ps1").GetAwaiter().GetResult();
                    File.WriteAllBytes(script, bytes);
                }

                var psi = new ProcessStartInfo
                {
                    FileName = "powershell.exe",
                    Arguments = $"-NoProfile -ExecutionPolicy Bypass -File \"{script}\" -Channel 10.0 -InstallDir \"{installDir}\"",
                    UseShellExecute = false,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true,
                    CreateNoWindow = true
                };
                using var ps = Process.Start(psi);
                if (ps is not null)
                {
                    log?.Invoke("正在通过官方脚本安装 .NET 10 SDK 到用户目录，请稍候（可能需要几分钟）...");
                    _ = ps.StandardOutput.ReadToEndAsync();
                    _ = ps.StandardError.ReadToEndAsync();
                    if (ps.WaitForExit(600000) && ps.ExitCode == 0)
                    {
                        log?.Invoke("dotnet-install 安装完成。");
                        return LocateDotnet() is not null;
                    }

                    log?.Invoke($"官方脚本安装未成功（退出码 {ps.ExitCode}）。");
                }
            }
            catch (Exception ex)
            {
                log?.Invoke($"自动安装 .NET SDK 失败：{ex.Message}");
            }

            return false;
        }

        /// <summary>编译 S4 服务端：dotnet build Server\DfoServer.sln -c Debug（等效 build.bat，避免 pause 卡住）</summary>
        public static bool BuildS4Server(string rootPath, Action<string>? log)
        {
            if (LocateS4BuildScript(rootPath) is null)
            {
                log?.Invoke("未找到 build.bat，无法编译服务端。");
                return false;
            }

            var dotnet = LocateDotnet();
            if (dotnet is null)
            {
                log?.Invoke("未检测到 .NET SDK，请先安装编译环境。");
                return false;
            }

            try
            {
                var psi = new ProcessStartInfo
                {
                    FileName = dotnet,
                    WorkingDirectory = rootPath,
                    UseShellExecute = false,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true,
                    CreateNoWindow = true
                };
                psi.ArgumentList.Add("build");
                psi.ArgumentList.Add(Path.Combine("Server", "DfoServer.sln"));
                psi.ArgumentList.Add("-c");
                psi.ArgumentList.Add("Debug");

                using var p = Process.Start(psi);
                if (p is null)
                {
                    log?.Invoke("编译进程启动失败。");
                    return false;
                }

                log?.Invoke("开始编译服务端（dotnet build -c Debug），请稍候 ...");
                var stdout = p.StandardOutput.ReadToEndAsync();
                var stderr = p.StandardError.ReadToEndAsync();
                if (!p.WaitForExit(600000))
                {
                    try
                    {
                        p.Kill(true);
                    }
                    catch
                    {
                        // 进程已退出
                    }

                    log?.Invoke("编译超时（10 分钟），已中止。");
                    return false;
                }

                var output = stdout.Result + stderr.Result;
                if (p.ExitCode == 0)
                {
                    log?.Invoke("编译成功。");
                    return true;
                }

                log?.Invoke($"编译失败（退出码 {p.ExitCode}），错误摘要：");
                var errors = output.Split(new[] { '\r', '\n' }, StringSplitOptions.RemoveEmptyEntries)
                    .Where(l => l.Contains("error", StringComparison.OrdinalIgnoreCase))
                    .Take(10);
                foreach (var e in errors)
                {
                    log?.Invoke("  " + e.Trim());
                }
                return false;
            }
            catch (Exception ex)
            {
                log?.Invoke($"编译失败：{ex.Message}");
                return false;
            }
        }

        /// <summary>从客户端目录复制 Script.pvf 到服务端：源码 Data\Pvf（编译自动带出）+ 已编译输出目录 Data\Pvf（立即生效）</summary>
        public static bool CopyPvfToServer(string clientRoot, string rootPath, Action<string>? log)
        {
            var src = Path.Combine(clientRoot, "Script.pvf");
            if (!File.Exists(src))
            {
                log?.Invoke($"客户端目录未找到 Script.pvf：{src}");
                return false;
            }

            var copied = false;

            // 1) 源码目录（重新编译后依然存在）
            var srcDir = Path.Combine(rootPath, "Server", "DfoServer", "Data", "Pvf");
            try
            {
                Directory.CreateDirectory(srcDir);
                File.Copy(src, Path.Combine(srcDir, "Script.pvf"), true);
                log?.Invoke($"已复制 Script.pvf 到服务端源码目录：{srcDir}");
                copied = true;
            }
            catch (Exception ex)
            {
                log?.Invoke($"复制到源码目录失败：{ex.Message}");
            }

            // 2) 已编译输出目录（当前实例立即生效）
            var exe = LocateS4Server(rootPath);
            if (exe is not null)
            {
                var outDir = Path.Combine(Path.GetDirectoryName(exe)!, "Data", "Pvf");
                try
                {
                    Directory.CreateDirectory(outDir);
                    File.Copy(src, Path.Combine(outDir, "Script.pvf"), true);
                    log?.Invoke($"已复制 Script.pvf 到服务端输出目录：{outDir}");
                    copied = true;
                }
                catch (Exception ex)
                {
                    log?.Invoke($"复制到输出目录失败：{ex.Message}");
                }
            }

            return copied;
        }

        /// <summary>启动 S4 系列服务端：DfoServer.exe --server-ip 127.0.0.1（对齐 start-server.bat）</summary>
        public static void StartS4(string rootPath, Action<string>? log)
        {
            var exe = LocateS4Server(rootPath);
            if (exe is null)
            {
                log?.Invoke("未找到 DfoServer.exe（根目录/bin/runtime 均不存在），请确认服务端目录结构。");
                return;
            }

            if (S4ServerRunning)
            {
                log?.Invoke("频道端口 7001 已被占用，服务端可能已在运行。");
                return;
            }

            // 缺 PVF 时 DfoServer 会立即退出（86JP 报 Script.pvf not found），启动前明确提示
            if (LocateS4Pvf(rootPath) is null)
            {
                log?.Invoke("服务端缺少 Script.pvf，DfoServer 无法启动。请将客户端目录下的 Script.pvf 复制到服务端 Data/Pvf/ 目录后重试。");
                return;
            }

            var psi = new ProcessStartInfo
            {
                FileName = exe,
                WorkingDirectory = Path.GetDirectoryName(exe)!,
                UseShellExecute = false,
                CreateNoWindow = true,
            };
            psi.ArgumentList.Add("--server-ip");
            psi.ArgumentList.Add("127.0.0.1");

            try
            {
                var p = Process.Start(psi);
                log?.Invoke($"已启动 DfoServer（PID {p?.Id}），等待频道端口 7001 ...");
                if (WaitForPort(S4ChannelPort, 30))
                {
                    log?.Invoke("服务端启动成功：频道端口 7001 已就绪。");
                }
                else
                {
                    log?.Invoke("等待 30 秒后频道端口仍未就绪，请查看 DfoServer 日志确认启动是否正常。");
                }
            }
            catch (Exception ex)
            {
                log?.Invoke($"启动 DfoServer 失败：{ex.Message}");
            }
        }

        /// <summary>停止 S4 系列服务端：杀掉位于该服务端目录下的 DfoServer 进程</summary>
        public static void StopS4(string rootPath, Action<string>? log)
        {
            try
            {
                var exePath = LocateS4Server(rootPath);
                var killed = 0;
                foreach (var p in Process.GetProcessesByName("DfoServer"))
                {
                    try
                    {
                        var main = p.MainModule?.FileName ?? "";
                        if (exePath is not null && !string.Equals(main, exePath, StringComparison.OrdinalIgnoreCase))
                        {
                            continue; // 只杀本目录实例，避免误杀其它服务端
                        }
                        p.Kill();
                        p.WaitForExit(3000);
                        killed++;
                    }
                    catch
                    {
                        // 进程已退出或权限不足，忽略
                    }
                }
                log?.Invoke(killed > 0 ? $"已停止 {killed} 个 DfoServer 进程。" : "未发现运行中的 DfoServer 进程。");
            }
            catch (Exception ex)
            {
                log?.Invoke($"停止 DfoServer 失败：{ex.Message}");
            }
        }

        /// <summary>按版本选择服务端启动入口（90 走 DNF90Server，S4 系列走 DfoServer）</summary>
        public static void StartForProfile(string profileId, string rootPath, Action<string>? log)
        {
            if (IsS4Profile(profileId))
            {
                StartS4(rootPath, log);
            }
            else
            {
                Start(rootPath, log);
            }
        }

        /// <summary>按版本选择服务端停止入口</summary>
        public static void StopForProfile(string profileId, string rootPath, Action<string>? log)
        {
            if (IsS4Profile(profileId))
            {
                StopS4(rootPath, log);
            }
            else
            {
                Stop(rootPath, log);
            }
        }

        /// <summary>按版本判断服务端是否运行（不带根目录：S4 系列仅按 7001 端口，用于测试连接等场景）</summary>
        public static bool ServerRunningForProfile(string profileId)
            => IsS4Profile(profileId) ? S4ServerRunning : ServerRunning;

        /// <summary>按版本 + 服务端根目录判断服务端是否运行（在线状态/登录闸控用，S4 系列能区分 A12/A21 实例）</summary>
        public static bool ServerRunningForProfile(string profileId, string rootPath)
            => IsS4Profile(profileId) ? S4ServerRunningForRoot(rootPath) : ServerRunning;

        /// <summary>读取服务端配置；路径无效或解析失败返回 null</summary>
        public static ServerInfo? LoadInfo(string rootPath)
        {
            try
            {
                var instancePath = Path.Combine(rootPath, "runtime", "config", "instance.json");
                if (!File.Exists(instancePath))
                {
                    return null;
                }

                using var doc = JsonDocument.Parse(File.ReadAllText(instancePath));
                var root = doc.RootElement;
                var info = new ServerInfo { RootPath = rootPath };

                if (root.TryGetProperty("database", out var db))
                {
                    info.DatabaseMode = GetStr(db, "mode");
                    info.DatabasePath = GetStr(db, "path");
                    info.DatabaseHost = GetStr(db, "host");
                    info.DatabasePort = db.TryGetProperty("port", out var dbp) && dbp.ValueKind == JsonValueKind.Number ? dbp.GetInt32() : 0;
                    info.DatabaseName = GetStr(db, "name");
                    info.DatabaseUser = GetStr(db, "user");
                    info.DatabasePassword = GetStr(db, "password");
                }

                if (root.TryGetProperty("installationId", out var inst))
                {
                    info.InstallationId = inst.ValueKind == JsonValueKind.String ? inst.GetString() ?? "" : "";
                }

                if (root.TryGetProperty("server", out var server))
                {
                    info.AdvertiseIp = GetStr(server, "advertiseIp");
                    info.ChannelListen = GetStr(server, "channelListen");
                    info.AdminListen = GetStr(server, "adminListen");
                    info.AdminToken = GetStr(server, "adminToken");
                }

                if (root.TryGetProperty("game", out var game))
                {
                    info.ShardId = GetStr(game, "shardId");
                    info.PvfPath = GetStr(game, "pvfPath");
                    info.ChannelInfoPath = GetStr(game, "channelInfoPath");
                }

                if (root.TryGetProperty("client", out var client))
                {
                    info.ClientDirectory = GetStr(client, "directory");
                }

                if (root.TryGetProperty("protocol", out var protocol))
                {
                    info.GameOuterToken = GetStr(protocol, "gameOuterToken");
                }

                return info;
            }
            catch
            {
                return null;
            }
        }

        /// <summary>
        /// 只读探测服务端初始化与运行状态，供 UI 展示与初始化判定。不产生副作用。
        /// </summary>
        public static ServerInspection InspectServer(string rootPath)
        {
            var ins = new ServerInspection { RootPath = rootPath };
            if (string.IsNullOrWhiteSpace(rootPath) || !Directory.Exists(rootPath))
            {
                ins.Message = "目录不存在";
                return ins;
            }

            ins.RootExists = true;
            var runtime = Path.Combine(rootPath, "runtime");
            var instancePath = Path.Combine(runtime, "config", "instance.json");
            ins.InstanceExists = File.Exists(instancePath);
            var info = LoadInfo(rootPath);
            ins.InstanceValid = info is not null;
            ins.DatabaseMode = info?.DatabaseMode ?? "";

            if (!ins.InstanceExists)
            {
                ins.NeedsInit = true;
                ins.Message = "未初始化：缺少 runtime\\config\\instance.json";
                return ins;
            }

            if (!ins.InstanceValid)
            {
                ins.NeedsInit = true;
                ins.Message = "instance.json 解析失败，请检查文件格式";
                return ins;
            }

            if (string.Equals(ins.DatabaseMode, "sqlite", StringComparison.OrdinalIgnoreCase))
            {
                var dbPath = string.IsNullOrWhiteSpace(info!.DatabasePath)
                    ? Path.Combine(runtime, "data", "dnf90.db")
                    : Path.IsPathRooted(info.DatabasePath)
                        ? info.DatabasePath
                        : Path.Combine(runtime, info.DatabasePath);
                ins.SqliteDbExists = File.Exists(dbPath);
                if (!ins.SqliteDbExists)
                {
                    ins.NeedsInit = true;
                    ins.Message = $"SQLite 数据库不存在：{dbPath}";
                    return ins;
                }

                ins.Message = "SQLite 就绪";
                ins.Ready = true;
            }
            else
            {
                ins.MySqlDirExists = Directory.Exists(Path.Combine(runtime, "mysql"));
                ins.MySqlDataReady = IsMySqlDataReady(runtime);
                if (!ins.MySqlDirExists || !ins.MySqlDataReady)
                {
                    ins.NeedsInit = true;
                    ins.Message = "MySQL 未初始化：runtime\\mysql 缺失或 data 未就绪";
                    return ins;
                }

                var bizDb = string.IsNullOrWhiteSpace(info!.DatabaseName) ? "dnf_local" : info.DatabaseName;
                ins.TableCount = QueryTableCount(info, Path.Combine(runtime, "mysql", "server"), bizDb);
                ins.Message = ins.TableCount >= 0
                    ? $"MySQL 就绪（业务库 {ins.TableCount} 张表）"
                    : "MySQL 就绪（业务库表状态待启动后确认）";
                ins.Ready = true;
            }

            ins.MySqlPortOpen = IsPortOpen("127.0.0.1", MySqlPort);
            ins.AdminPortOpen = IsPortOpen("127.0.0.1", AdminPort);
            ins.ChannelPortOpen = TryParseListenPort(info?.ChannelListen ?? "", out var cp) && IsPortOpen("127.0.0.1", cp);

            if (ins.AdminPortOpen)
            {
                ins.Message += " · 服务端运行中";
            }

            return ins;
        }

        /// <summary>
        /// 启动前验证：结构完整、instance.json 可解析、数据库就绪。
        /// 返回 (是否可启动, 问题列表)；Errors 为空表示可启动。
        /// </summary>
        public static (bool Ok, List<string> Errors) ValidateServer(string rootPath, Action<string>? log)
        {
            var errors = new List<string>();
            var runtime = Path.Combine(rootPath, "runtime");

            var serverExe = Path.Combine(runtime, "bin", "DNF90Server.exe");
            if (!File.Exists(serverExe))
            {
                errors.Add($"缺少 DNF90Server.exe：{serverExe}");
            }

            var instancePath = Path.Combine(runtime, "config", "instance.json");
            if (!File.Exists(instancePath))
            {
                errors.Add("缺少 instance.json，请先执行一键初始化");
            }

            var info = LoadInfo(rootPath);
            if (info is null)
            {
                if (File.Exists(instancePath))
                {
                    errors.Add("instance.json 解析失败，请检查文件格式");
                }
            }
            else
            {
                if (string.Equals(info.DatabaseMode, "portable", StringComparison.OrdinalIgnoreCase))
                {
                    if (!IsMySqlDataReady(runtime))
                    {
                        // data 未初始化：不拦截，交给原版 DNF90Control start 的 first-start 流程自动初始化
                        log?.Invoke("MySQL 数据未初始化，首次启动时将自动初始化（预计 20-60 秒）");
                    }
                    else if (MySqlRunning && !HasProcessInRoot("mysqld", rootPath))
                    {
                        errors.Add("端口 13306 已被其他 MySQL 实例占用，本目录 mysqld 未运行");
                    }
                }
                else
                {
                    var dbPath = string.IsNullOrWhiteSpace(info.DatabasePath)
                        ? Path.Combine(runtime, "data", "dnf90.db")
                        : Path.IsPathRooted(info.DatabasePath)
                            ? info.DatabasePath
                            : Path.Combine(runtime, info.DatabasePath);
                    if (!File.Exists(dbPath))
                    {
                        errors.Add($"SQLite 数据库不存在：{dbPath}");
                    }
                }
            }

            // data-state.json 若存在则必须为无 BOM UTF-8（原版 UTF-8 BOM 在中文路径下会导致状态误判）
            var dataState = Path.Combine(runtime, "mysql", "data-state.json");
            if (File.Exists(dataState))
            {
                var bytes = File.ReadAllBytes(dataState);
                if (bytes.Length >= 3 && bytes[0] == 0xEF && bytes[1] == 0xBB && bytes[2] == 0xBF)
                {
                    errors.Add("data-state.json 含 UTF-8 BOM，可能导致状态读取异常");
                }
            }

            if (errors.Count == 0)
            {
                log?.Invoke("启动前验证通过");
            }
            else
            {
                log?.Invoke("启动前验证未通过：");
                foreach (var e in errors)
                {
                    log?.Invoke($"  - {e}");
                }
            }

            return (errors.Count == 0, errors);
        }

        /// <summary>
        /// 将 instance.json 的关键配置同步到登录器设置（AdminToken / 数据库连接 / 模式），
        /// 保证登录器绑定账号时 X-Admin-Token 与服务端一致。
        /// </summary>
        public static void SyncFromInstance(string rootPath)
        {
            var info = LoadInfo(rootPath);
            if (info is null)
            {
                return;
            }

            var s = DNFLogin.AppConfig.Settings;
            if (!string.IsNullOrWhiteSpace(info.AdminToken))
            {
                s.AdminToken = info.AdminToken;
            }

            if (!string.IsNullOrWhiteSpace(info.DatabaseHost))
            {
                s.Host = info.DatabaseHost;
            }

            if (!string.IsNullOrWhiteSpace(info.DatabasePath))
            {
                s.DatabasePath = info.DatabasePath;
            }

            if (info.DatabasePort > 0)
            {
                s.Port = info.DatabasePort.ToString();
            }

            if (!string.IsNullOrWhiteSpace(info.DatabaseName))
            {
                s.Name = info.DatabaseName;
            }

            if (!string.IsNullOrWhiteSpace(info.DatabaseUser))
            {
                s.User = info.DatabaseUser;
            }

            if (!string.IsNullOrWhiteSpace(info.DatabasePassword))
            {
                s.Password = info.DatabasePassword;
            }

            s.Mode = string.Equals(info.DatabaseMode, "sqlite", StringComparison.OrdinalIgnoreCase) ? "sqlite" : "portable";
            DNFLogin.AppConfig.Save();
        }

        /// <summary>判断 MySQL data 是否已初始化（data\mysql 存在或 data-state.json phase=ready）</summary>
        private static bool IsMySqlDataReady(string runtime)
        {
            var dataDir = Path.Combine(runtime, "mysql", "data");
            var dataState = Path.Combine(runtime, "mysql", "data-state.json");
            if (Directory.Exists(Path.Combine(dataDir, "mysql")))
            {
                return true;
            }

            if (File.Exists(dataState))
            {
                try
                {
                    using var doc = JsonDocument.Parse(File.ReadAllText(dataState));
                    return doc.RootElement.TryGetProperty("phase", out var ph) && ph.GetString() == "ready";
                }
                catch
                {
                    return false;
                }
            }

            return false;
        }

        /// <summary>从 "0.0.0.0:7001" 形式监听地址解析端口；失败返回 false</summary>
        private static bool TryParseListenPort(string listen, out int port)
        {
            port = 0;
            if (string.IsNullOrWhiteSpace(listen))
            {
                return false;
            }

            var idx = listen.LastIndexOf(':');
            return idx >= 0 && int.TryParse(listen[(idx + 1)..], out port);
        }

        /// <summary>
        /// 切换数据库模式并写回 instance.json。
        /// mode: "portable"（MySQL）/ "sqlite"。
        /// </summary>
        public static bool SwitchDatabase(string rootPath, string mode, Action<string>? log)
        {
            if (mode != "portable" && mode != "sqlite")
            {
                log?.Invoke($"未知数据库模式: {mode}");
                return false;
            }

            try
            {
                if (!EnsureInstance(rootPath, mode, log))
                {
                    return false;
                }

                var instancePath = Path.Combine(rootPath, "runtime", "config", "instance.json");

                var json = File.ReadAllText(instancePath);
                var node = JsonNode.Parse(json)?.AsObject();
                if (node is null || node["database"] is not JsonObject db)
                {
                    log?.Invoke("instance.json 缺少 database 节点");
                    return false;
                }

                db["mode"] = mode;
                if (mode == "sqlite" && string.IsNullOrWhiteSpace(GetStrNode(db, "path")))
                {
                    db["path"] = "data/dnf90.db";
                }

                File.WriteAllText(instancePath, node.ToJsonString(new JsonSerializerOptions { WriteIndented = true }));

                // 同步 DNF90Server 实际读取的 logic.toml，否则切 SQLite 后服务端仍按旧 mysql 驱动连 13306
                var syncInfo = LoadInfo(rootPath);
                if (syncInfo is not null)
                {
                    SyncLogicToml(syncInfo, log);
                }

                log?.Invoke($"已切换数据库模式: {(mode == "portable" ? "MySQL" : "SQLite")}");
                return true;
            }
            catch (Exception ex)
            {
                log?.Invoke($"切换失败: {ex.Message}");
                return false;
            }
        }

        /// <summary>
        /// 确保 instance.json 存在：干净包/未初始化时调用原版 DNF90Control database select 生成（含解压 bundled MySQL）。
        /// mode 取值 portable(mysql)/sqlite，仅在 instance.json 缺失时生效。
        /// </summary>
        public static bool EnsureInstance(string rootPath, string mode, Action<string>? log)
        {
            var instancePath = Path.Combine(rootPath, "runtime", "config", "instance.json");
            if (File.Exists(instancePath))
            {
                return true;
            }

            if (!EnsureRuntimeLayout(rootPath, log))
            {
                return false;
            }

            log?.Invoke("未找到 instance.json，正在通过原版控制器初始化服务端配置...");
            var control = Path.Combine(rootPath, "runtime", "bin", "DNF90Control.exe");
            if (!File.Exists(control))
            {
                log?.Invoke($"找不到原版控制器: {control}，请确认服务端目录完整");
                return false;
            }

            var modeArg = string.Equals(mode, "portable", StringComparison.OrdinalIgnoreCase) ? "mysql" : "sqlite";
            var env = new Dictionary<string, string> { ["DNF90_PROJECT_ROOT"] = rootPath };
            var (ok, output) = RunProcessCapture(control, $"database select --mode {modeArg} --prepare", rootPath, env);
            log?.Invoke(output.Trim());
            if (!ok || !File.Exists(instancePath))
            {
                log?.Invoke("自动初始化服务端配置失败，请检查上方输出");
                return false;
            }

            return true;
        }

        /// <summary>
        /// 一键启动服务端：直接调用原版 DNF90Control.exe start，由原版完成
        /// 配置生成（instance.json/dnfbridge.toml/logic.toml/plan.json/mysql.ini）、
        /// 数据库初始化与启动、服务端拉起与就绪等待。登录器不做任何文件生成。
        /// 阻塞直至原版 start 返回（成功=0，失败=非0并携带错误输出）。
        /// </summary>
        public static void Start(string rootPath, Action<string>? log)
        {
            var control = Path.Combine(rootPath, "runtime", "bin", "DNF90Control.exe");
            if (!File.Exists(control))
            {
                log?.Invoke($"找不到 DNF90Control.exe: {control}");
                log?.Invoke("请确认已选择正确的服务端根目录（应包含 runtime\\bin\\DNF90Control.exe）");
                return;
            }

            // 启动前清理本服务端目录下的相关残留进程（按路径匹配，不影响其他目录的同名进程）
            // 先停服务端与控制台，再停数据库，避免残留进程占用端口/文件导致原版 start 拒绝
            foreach (var name in new[] { "DNF90Server", "DNF90Control", "mysqld" })
            {
                if (HasProcessInRoot(name, rootPath))
                {
                    log?.Invoke($"检测到残留进程 {name}，正在停止 ...");
                    KillByRoot(name, rootPath);
                    Thread.Sleep(500);
                    log?.Invoke($"已停止残留进程 {name}");
                }
            }

            // 补齐便携运行库/MySQL 包：源码树本地启动时 runtime\vendor 可能为空，
            // 从 deploy\vendor 回填，避免 DNF90Control 预检因缺文件直接失败。
            EnsureRuntimeLayout(rootPath, log);

            log?.Invoke("正在调用原版控制器启动服务端 (DNF90Control.exe start) ...");
            log?.Invoke("首次启动会自动生成 instance.json、dnfbridge.toml 等配置并初始化数据库，请耐心等待");

            try
            {
                var psi = new ProcessStartInfo(control)
                {
                    Arguments = "start",
                    WorkingDirectory = rootPath,
                    CreateNoWindow = true,
                    UseShellExecute = false,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true
                };
                psi.EnvironmentVariables["DNF90_PROJECT_ROOT"] = rootPath;

                using var proc = Process.Start(psi);
                if (proc is null)
                {
                    log?.Invoke("启动 DNF90Control 失败：进程未创建");
                    return;
                }

                var lines = new List<string>();
                void OnLine(string? line)
                {
                    if (line is null) return;
                    lock (lines) lines.Add(line);
                    log?.Invoke(line);
                }
                proc.OutputDataReceived += (_, e) => OnLine(e.Data);
                proc.ErrorDataReceived += (_, e) => OnLine(e.Data);
                proc.BeginOutputReadLine();
                proc.BeginErrorReadLine();

                if (!proc.WaitForExit(180000))
                {
                    log?.Invoke("DNF90Control start 超时（180s），原版仍未返回；请稍候用「刷新状态」确认服务端是否已就绪");
                    return;
                }

                if (proc.ExitCode == 0)
                {
                    log?.Invoke("服务端已由原版控制器确认就绪");
                }
                else
                {
                    log?.Invoke($"DNF90Control start 失败（ExitCode={proc.ExitCode}），上方输出即失败原因");
                }
            }
            catch (Exception ex)
            {
                log?.Invoke($"调用 DNF90Control start 失败: {ex.Message}");
            }
        }

        /// <summary>
        /// 启动/初始化前补齐 runtime 目录布局：发布包（deploy 形态）中 templates/assets/vendor
        /// 可能只存在于 deploy\ 下，而 DNF90Control 期望它们在 runtime\ 下
        /// （templates 还是项目根识别的强制锚点）。幂等：只补缺失文件，绝不覆盖 runtime 已有文件。
        /// </summary>
        private static bool EnsureRuntimeLayout(string rootPath, Action<string>? log)
        {
            try
            {
                var deploy = Path.Combine(rootPath, "deploy");
                var runtime = Path.Combine(rootPath, "runtime");
                if (!Directory.Exists(deploy) || !Directory.Exists(runtime))
                {
                    // 非发布包形态（源码树/已部署树），runtime 已完整，无需部署
                    return true;
                }

                var ok = true;
                foreach (var sub in new[] { "templates", "assets", "vendor" })
                {
                    var src = Path.Combine(deploy, sub);
                    var dst = Path.Combine(runtime, sub);
                    if (!Directory.Exists(src))
                    {
                        continue;
                    }
                    ok &= CopyMissingTree(src, dst, log);
                }
                return ok;
            }
            catch (Exception ex)
            {
                log?.Invoke($"补齐运行时布局失败：{ex.Message}");
                return false;
            }
        }

        /// <summary>递归复制 sourceDir 下缺失的文件到 destDir；已存在则跳过（不覆盖）。</summary>
        private static bool CopyMissingTree(string sourceDir, string destDir, Action<string>? log)
        {
            var ok = true;
            Directory.CreateDirectory(destDir);
            foreach (var srcPath in Directory.EnumerateFiles(sourceDir, "*", SearchOption.AllDirectories))
            {
                var rel = Path.GetRelativePath(sourceDir, srcPath);
                var dstPath = Path.Combine(destDir, rel);
                if (File.Exists(dstPath))
                {
                    continue;
                }
                var dstParent = Path.GetDirectoryName(dstPath);
                if (!string.IsNullOrEmpty(dstParent))
                {
                    Directory.CreateDirectory(dstParent);
                }
                try
                {
                    File.Copy(srcPath, dstPath, false);
                    log?.Invoke($"已补齐运行时文件：{rel}");
                }
                catch (Exception ex)
                {
                    log?.Invoke($"补齐运行时文件失败：{rel}（{ex.Message}）");
                    ok = false;
                }
            }
            return ok;
        }

        /// <summary>
        /// 一键停止服务端：直接调用原版 DNF90Control.exe stop，由原版安全停止
        /// DNF90Server 与便携数据库。登录器不直接杀进程。
        /// </summary>
        public static void Stop(string rootPath, Action<string>? log)
        {
            var control = Path.Combine(rootPath, "runtime", "bin", "DNF90Control.exe");
            if (!File.Exists(control))
            {
                log?.Invoke($"找不到 DNF90Control.exe: {control}");
                return;
            }

            log?.Invoke("正在调用原版控制器停止服务端 (DNF90Control.exe stop) ...");
            var env = new Dictionary<string, string> { ["DNF90_PROJECT_ROOT"] = rootPath };
            var (ok, output) = RunProcessCapture(control, "stop", rootPath, env);
            log?.Invoke(output.Trim());
            log?.Invoke(ok ? "停止完成" : "停止失败（上方为原版输出）");
        }

        /// <summary>等待端口就绪，最多 timeoutSeconds 秒</summary>
        private static bool WaitForPort(int port, int timeoutSeconds)
        {
            var deadline = DateTime.UtcNow.AddSeconds(timeoutSeconds);
            while (DateTime.UtcNow < deadline)
            {
                if (IsPortOpen("127.0.0.1", port))
                {
                    return true;
                }

                Thread.Sleep(500);
            }

            return IsPortOpen("127.0.0.1", port);
        }

        /// <summary>是否存在主模块路径位于 rootPath 下的指定进程</summary>
        private static bool HasProcessInRoot(string processName, string rootPath)
        {
            foreach (var p in Process.GetProcessesByName(processName))
            {
                try
                {
                    var path = p.MainModule?.FileName ?? "";
                    if (path.StartsWith(rootPath, StringComparison.OrdinalIgnoreCase))
                    {
                        return true;
                    }
                }
                catch
                {
                    // 无权限读取模块时忽略
                }
            }

            return false;
        }

        /// <summary>结束主模块路径位于 rootPath 下的指定进程（含子进程树）</summary>
        private static void KillByRoot(string processName, string rootPath)
        {
            foreach (var p in Process.GetProcessesByName(processName))
            {
                try
                {
                    var path = p.MainModule?.FileName ?? "";
                    if (!path.StartsWith(rootPath, StringComparison.OrdinalIgnoreCase))
                    {
                        continue;
                    }

                    try
                    {
                        using (p)
                        {
                            p.Kill(entireProcessTree: true);
                            p.WaitForExit(3000);
                        }
                    }
                    catch
                    {
                        // 进程已退出
                    }
                }
                catch
                {
                    // 无权限读取模块时忽略
                }
            }
        }

        private static string GetStr(JsonElement obj, string name)
        {
            return obj.TryGetProperty(name, out var v) && v.ValueKind == JsonValueKind.String ? v.GetString() ?? "" : "";
        }

        private static string GetStrNode(JsonObject obj, string name)
        {
            return obj.TryGetPropertyValue(name, out var v) && v is JsonValue jv ? jv.GetValue<string>() : "";
        }

        /// <summary>
        /// 同步 configs\dnf\logic.toml 的 repository 段，使其与 instance.json 的 database.mode 一致。
        /// 对齐原版 DNF90Control start 的生成结果，幂等，可反复调用。
        /// </summary>
        public static bool SyncLogicToml(ServerInfo info, Action<string>? log)
        {
            try
            {
                var path = Path.Combine(info.RootPath, "runtime", "configs", "dnf", "logic.toml");
                var dir = Path.GetDirectoryName(path);
                if (dir is not null)
                {
                    Directory.CreateDirectory(dir);
                }

                string content;
                if (string.Equals(info.DatabaseMode, "portable", StringComparison.OrdinalIgnoreCase))
                {
                    var host = string.IsNullOrWhiteSpace(info.DatabaseHost) ? "127.0.0.1" : info.DatabaseHost;
                    var port = info.DatabasePort > 0 ? info.DatabasePort : MySqlPort;
                    var name = string.IsNullOrWhiteSpace(info.DatabaseName) ? "dnf_local" : info.DatabaseName;
                    var user = string.IsNullOrWhiteSpace(info.DatabaseUser) ? "root" : info.DatabaseUser;
                    var pass = info.DatabasePassword;
                    var shard = string.IsNullOrWhiteSpace(info.ShardId) ? "9999" : info.ShardId;
                    content = $"""
[repository]
enabled = true
driver = "mysql"
mysql_dsn = "{user}:{pass}@tcp({host}:{port})/{name}?parseTime=true&charset=utf8mb4,utf8"
mysql_max_open_conns = 32
mysql_max_idle_conns = 8
mysql_conn_max_lifetime_seconds = 300
shard_id = "{shard}"
table_prefix = "dnf"
server_group_plan_file = "configs/servergroup/plan.json"
auto_create_schema = true
csharp_legacy_schema = true
create_databases = false
""";
                }
                else
                {
                    var sqlitePath = string.IsNullOrWhiteSpace(info.DatabasePath) ? "data/dnf90.db" : info.DatabasePath.Replace('\\', '/');
                    content = $"""
[repository]
enabled = true
driver = "sqlite"
sqlite_path = "{sqlitePath}"
redis_enabled = false
""";
                }

                File.WriteAllText(path, content, new UTF8Encoding(false));
                log?.Invoke($"已同步 repository 配置: {(string.Equals(info.DatabaseMode, "portable", StringComparison.OrdinalIgnoreCase) ? "MySQL" : "SQLite")}");
                return true;
            }
            catch (Exception ex)
            {
                log?.Invoke($"同步 logic.toml 失败: {ex.Message}");
                return false;
            }
        }

        /// <summary>查询业务库表数量；连接失败返回 -1</summary>
        private static int QueryTableCount(ServerInfo info, string mysqlRoot, string dbName)
        {
            try
            {
                var mysqlCli = Path.Combine(mysqlRoot, "server", "bin", "mysql.exe");
                var pass = info.DatabasePassword;
                var (ok, output) = RunProcessCapture(mysqlCli,
                    $"--protocol=tcp -h 127.0.0.1 -P {MySqlPort} -u root -p{pass} -N -B -e \"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='{dbName}';\"",
                    "");
                if (!ok)
                {
                    return -1;
                }

                var line = output.Trim().Split('\n')[0].Trim();
                return int.TryParse(line, out var n) ? n : -1;
            }
            catch
            {
                return -1;
            }
        }

        /// <summary>执行一次性命令并捕获输出；返回 (是否成功, 输出文本)</summary>
        private static (bool, string) RunProcessCapture(string file, string args, string workingDirectory, Dictionary<string, string>? env = null)
        {
            try
            {
                var psi = new ProcessStartInfo(file)
                {
                    Arguments = args,
                    WorkingDirectory = workingDirectory,
                    CreateNoWindow = true,
                    UseShellExecute = false,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true
                };
                if (env is not null)
                {
                    foreach (var kv in env)
                    {
                        psi.EnvironmentVariables[kv.Key] = kv.Value;
                    }
                }

                using var proc = Process.Start(psi);
                if (proc is null)
                {
                    return (false, "进程未创建");
                }

                var stdout = proc.StandardOutput.ReadToEnd();
                var stderr = proc.StandardError.ReadToEnd();
                proc.WaitForExit(120000);
                return (proc.ExitCode == 0, stdout + stderr);
            }
            catch (Exception ex)
            {
                return (false, ex.Message);
            }
        }

    }
}
