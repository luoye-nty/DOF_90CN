using System;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;

namespace DNFLogin
{
    /// <summary>
    /// DLL 注入：x64 目标走 CreateRemoteThread(LoadLibraryW)；32 位（WOW64）目标走跨位数注入
    /// （EnumProcessModulesEx 枚举 32 位模块 + PE 导出表解析定位 LoadLibraryW + ntdll NtCreateThreadEx）。
    /// </summary>
    public static class DllInjector
    {
        private const uint PROCESS_CREATE_THREAD = 0x0002;
        private const uint PROCESS_QUERY_INFORMATION = 0x0400;
        private const uint PROCESS_VM_OPERATION = 0x0008;
        private const uint PROCESS_VM_WRITE = 0x0020;
        private const uint PROCESS_VM_READ = 0x0010;
        private const uint MEM_COMMIT = 0x1000;
        private const uint MEM_RESERVE = 0x2000;
        private const uint MEM_RELEASE = 0x8000;
        private const uint PAGE_READWRITE = 0x04;
        private const uint INFINITE = 0xFFFFFFFF;

        // EnumProcessModulesEx 过滤标志：仅枚举 32 位模块
        private const uint LIST_MODULES_32BIT = 0x01;

        // NtCreateThreadEx 线程访问权限（含 SYNCHRONIZE，可 WaitForSingleObject）
        private const uint THREAD_ALL_ACCESS = 0x1FFFFF;

        // PE 可选头 Magic：PE32 / PE32+
        private const ushort IMAGE_NT_OPTIONAL_HDR32_MAGIC = 0x10B;
        private const ushort IMAGE_NT_OPTIONAL_HDR64_MAGIC = 0x20B;

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr OpenProcess(uint access, bool inherit, int pid);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern IntPtr GetModuleHandle(string name);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Ansi)]
        private static extern IntPtr GetProcAddress(IntPtr module, string proc);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr VirtualAllocEx(IntPtr hProc, IntPtr addr, IntPtr size, uint allocType, uint protect);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool VirtualFreeEx(IntPtr hProc, IntPtr addr, IntPtr size, uint freeType);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool WriteProcessMemory(IntPtr hProc, IntPtr addr, byte[] buffer, int size, out IntPtr written);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr CreateRemoteThread(IntPtr hProc, IntPtr attr, IntPtr stack, IntPtr startAddr, IntPtr param, uint flags, out uint threadId);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern uint WaitForSingleObject(IntPtr handle, uint ms);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr handle);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool IsWow64Process(IntPtr hProc, out bool wow64);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool ReadProcessMemory(IntPtr hProc, IntPtr addr, byte[] buffer, int size, out IntPtr read);

        [DllImport("psapi.dll", SetLastError = true)]
        private static extern bool EnumProcessModulesEx(IntPtr hProc, IntPtr[] modules, uint cb, out uint needed, uint filterFlag);

        [DllImport("psapi.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern uint GetModuleBaseNameW(IntPtr hProc, IntPtr module, StringBuilder baseName, int size);

        [DllImport("ntdll.dll")]
        private static extern uint NtCreateThreadEx(
            out IntPtr threadHandle,
            uint desiredAccess,
            IntPtr objectAttributes,
            IntPtr processHandle,
            IntPtr startAddress,
            IntPtr parameter,
            uint createFlags,
            IntPtr zeroBits,
            IntPtr stackSize,
            IntPtr stackReserved,
            IntPtr attributeList);

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

        /// <summary>注入一个 DLL 到指定进程；x64 走 CreateRemoteThread，x86(WOW64) 走跨位数注入；成功返回 true</summary>
        public static bool Inject(int pid, string dllPath)
        {
            if (pid <= 0 || string.IsNullOrWhiteSpace(dllPath) || !File.Exists(dllPath))
            {
                return false;
            }

            var hProc = OpenProcess(
                PROCESS_CREATE_THREAD | PROCESS_QUERY_INFORMATION |
                PROCESS_VM_OPERATION | PROCESS_VM_WRITE | PROCESS_VM_READ,
                false, pid);
            if (hProc == IntPtr.Zero)
            {
                Log("Inject: OpenProcess 失败 err=" + Marshal.GetLastWin32Error());
                return false;
            }

            try
            {
                // DNF.exe 是 32 位进程：走跨位数注入路径（EnumProcessModulesEx + PE 导出解析 + NtCreateThreadEx）
                var wow64 = false;
                if (IsWow64Process(hProc, out wow64) && wow64)
                {
                    return InjectWow64(hProc, dllPath);
                }

                // x64 目标：保留原 CreateRemoteThread 路径
                return InjectNative64(hProc, dllPath);
            }
            catch (Exception ex) when (ex is not Win32Exception)
            {
                // 注入异常时静默失败，不阻塞登录
                return false;
            }
            finally
            {
                CloseHandle(hProc);
            }
        }

        /// <summary>x64 目标：VirtualAllocEx + WriteProcessMemory + CreateRemoteThread(LoadLibraryW)</summary>
        private static bool InjectNative64(IntPtr hProc, string dllPath)
        {
            var loadLibrary = GetProcAddress(GetModuleHandle("kernel32.dll"), "LoadLibraryW");
            if (loadLibrary == IntPtr.Zero)
            {
                return false;
            }

            var remoteMem = WriteRemoteDllPath(hProc, dllPath);
            if (remoteMem == IntPtr.Zero)
            {
                return false;
            }

            var thread = CreateRemoteThread(hProc, IntPtr.Zero, IntPtr.Zero, loadLibrary, remoteMem, 0, out _);
            if (thread == IntPtr.Zero)
            {
                VirtualFreeEx(hProc, remoteMem, IntPtr.Zero, MEM_RELEASE);
                return false;
            }

            // 等待 LoadLibraryW 完成（最多 5 秒）
            WaitForSingleObject(thread, 5000);
            CloseHandle(thread);
            VirtualFreeEx(hProc, remoteMem, IntPtr.Zero, MEM_RELEASE);
            return true;
        }

        /// <summary>
        /// WOW64（32 位）目标：跨位数注入。
        /// EnumProcessModulesEx(LIST_MODULES_32BIT) 枚举目标进程 32 位模块，ReadProcessMemory 解析
        /// PE 导出表定位 32 位 LoadLibraryW 地址，再经 ntdll NtCreateThreadEx 创建 32 位远程线程执行。
        /// </summary>
        private static bool InjectWow64(IntPtr hProc, string dllPath)
        {
            var loadLibrary = FindRemoteLoadLibraryW32(hProc);
            if (loadLibrary == IntPtr.Zero)
            {
                Log("InjectWow64: 找不到 32 位 LoadLibraryW");
                return false;
            }

            var remoteMem = WriteRemoteDllPath(hProc, dllPath);
            if (remoteMem == IntPtr.Zero)
            {
                Log("InjectWow64: VirtualAllocEx/WriteProcessMemory 失败 err=" + Marshal.GetLastWin32Error());
                return false;
            }

            var status = NtCreateThreadEx(
                out var thread,
                THREAD_ALL_ACCESS,
                IntPtr.Zero, hProc, loadLibrary, remoteMem,
                0, IntPtr.Zero, IntPtr.Zero, IntPtr.Zero, IntPtr.Zero);
            if (status != 0)
            {
                Log("InjectWow64: NtCreateThreadEx 失败 status=0x" + status.ToString("X8"));
                VirtualFreeEx(hProc, remoteMem, IntPtr.Zero, MEM_RELEASE);
                return false;
            }

            // 完成判定：这里**不能**用 WaitForSingleObject 等 5 秒。
            // NtCreateThreadEx 起的是"裸" LoadLibraryW（没有 CreateRemoteThread 的线程包装），
            // 它返回后线程后续行为未定义，句柄往往要等到超时才结束 —— 实测每次注入白等满 5 秒，
            // 结果是：守护 DLL(≈5s) + Helper DLL(≈5s) 串起来，PVF 换钥的 DNFHelper 要游戏启动
            // 10 秒后才进去，而客户端早在这之前就读过 Script.pvf 了（2026-09-21 玩家日志实锤：
            // 换钥 DLL 迟到 10.4 秒 → 不出游戏 / 安装文件错误）。
            // 改成轮询目标模块表确认 DLL 真加载进来了：正常几十毫秒返回，
            // 同时挡住另一个隐患 —— 调用方注入成功后会立刻删掉临时 DLL，必须确认已加载才能删。
            var loaded = WaitRemoteModuleLoaded32(hProc, dllPath, 3000);
            if (!loaded)
            {
                Log("InjectWow64: 已建远程线程但 3 秒内未见模块加载: " + Path.GetFileName(dllPath));
            }

            CloseHandle(thread);
            VirtualFreeEx(hProc, remoteMem, IntPtr.Zero, MEM_RELEASE);
            return loaded;
        }

        /// <summary>轮询目标进程 32 位模块表，确认指定文件名的模块已加载</summary>
        private static bool WaitRemoteModuleLoaded32(IntPtr hProc, string dllPath, int timeoutMs)
        {
            var name = Path.GetFileName(dllPath);
            if (string.IsNullOrEmpty(name))
            {
                return false;
            }

            var sw = System.Diagnostics.Stopwatch.StartNew();
            while (sw.ElapsedMilliseconds < timeoutMs)
            {
                if (RemoteModuleLoaded32(hProc, name))
                {
                    return true;
                }

                System.Threading.Thread.Sleep(20);
            }

            return false;
        }

        /// <summary>目标进程 32 位模块表里是否有该文件名的模块</summary>
        private static bool RemoteModuleLoaded32(IntPtr hProc, string moduleFileName)
        {
            if (!EnumProcessModulesEx(hProc, Array.Empty<IntPtr>(), 0, out var needed, LIST_MODULES_32BIT) || needed == 0)
            {
                return false;
            }

            var count = needed / (uint)IntPtr.Size;
            if (count == 0 || count > 512)
            {
                return false;
            }

            var modules = new IntPtr[count];
            if (!EnumProcessModulesEx(hProc, modules, needed, out _, LIST_MODULES_32BIT))
            {
                return false;
            }

            var buffer = new StringBuilder(260);
            foreach (var modBase in modules)
            {
                if (modBase == IntPtr.Zero)
                {
                    continue;
                }

                buffer.Clear();
                if (GetModuleBaseNameW(hProc, modBase, buffer, buffer.Capacity) > 0 &&
                    string.Equals(buffer.ToString(), moduleFileName, StringComparison.OrdinalIgnoreCase))
                {
                    return true;
                }
            }

            return false;
        }

        /// <summary>在目标进程内分配内存并写入 DLL 路径（含结尾 \0）；返回远程内存地址，失败返回 Zero</summary>
        private static IntPtr WriteRemoteDllPath(IntPtr hProc, string dllPath)
        {
            var pathBytes = Encoding.Unicode.GetBytes(dllPath + "\0");
            var remoteMem = VirtualAllocEx(hProc, IntPtr.Zero, new IntPtr(pathBytes.Length), MEM_COMMIT | MEM_RESERVE, PAGE_READWRITE);
            if (remoteMem == IntPtr.Zero)
            {
                return IntPtr.Zero;
            }

            if (!WriteProcessMemory(hProc, remoteMem, pathBytes, pathBytes.Length, out _))
            {
                VirtualFreeEx(hProc, remoteMem, IntPtr.Zero, MEM_RELEASE);
                return IntPtr.Zero;
            }

            return remoteMem;
        }

        /// <summary>枚举目标进程 32 位模块，解析 PE 导出表查找 LoadLibraryW 导出地址</summary>
        private static IntPtr FindRemoteLoadLibraryW32(IntPtr hProc)
        {
            // 第一轮：查询 32 位模块数组所需大小
            if (!EnumProcessModulesEx(hProc, Array.Empty<IntPtr>(), 0, out var needed, LIST_MODULES_32BIT) || needed == 0)
            {
                return IntPtr.Zero;
            }

            var count = needed / (uint)IntPtr.Size;
            if (count == 0 || count > 512)
            {
                return IntPtr.Zero;
            }

            var modules = new IntPtr[count];
            if (!EnumProcessModulesEx(hProc, modules, needed, out _, LIST_MODULES_32BIT))
            {
                return IntPtr.Zero;
            }

            // LoadLibraryW 由 32 位 kernel32.dll / kernelbase.dll 导出，命中任一即为可调用入口
            foreach (var modBase in modules)
            {
                if (modBase == IntPtr.Zero)
                {
                    continue;
                }

                var proc = GetRemoteProcAddress(hProc, modBase, "LoadLibraryW");
                if (proc != IntPtr.Zero)
                {
                    return proc;
                }
            }

            return IntPtr.Zero;
        }

        /// <summary>
        /// 跨位数读取目标进程模块导出表：解析 DOS/PE 头 + 导出目录，按名称查函数 RVA 并换算绝对地址。
        /// 适用于 64 位宿主读取 32 位目标模块（模块基址落在低 4GB，IntPtr 可承载）。
        /// </summary>
        private static IntPtr GetRemoteProcAddress(IntPtr hProc, IntPtr moduleBase, string procName)
        {
            // IMAGE_DOS_HEADER：校验 MZ，取 e_lfanew（偏移 0x3C）
            var dos = new byte[0x40];
            if (!ReadProcessMemory(hProc, moduleBase, dos, dos.Length, out _) || BitConverter.ToUInt16(dos, 0) != 0x5A4D)
            {
                return IntPtr.Zero;
            }

            var peOffset = BitConverter.ToInt32(dos, 0x3C);
            if (peOffset <= 0)
            {
                return IntPtr.Zero;
            }

            // PE 签名（"PE\0\0"） + 可选头 Magic（PE32=0x10B / PE32+=0x20B）
            var peAddr = IntPtr.Add(moduleBase, peOffset);
            if (ReadUInt32(hProc, peAddr) != 0x00004550)
            {
                return IntPtr.Zero;
            }

            var magic = ReadUInt16(hProc, IntPtr.Add(peAddr, 24));
            if (magic != IMAGE_NT_OPTIONAL_HDR32_MAGIC && magic != IMAGE_NT_OPTIONAL_HDR64_MAGIC)
            {
                return IntPtr.Zero;
            }

            // IMAGE_OPTIONAL_HEADER：DataDirectory[0]（导出表）位于可选头偏移 96，RVA 在 +0
            var exportRva = ReadUInt32(hProc, IntPtr.Add(peAddr, 24 + 96));
            if (exportRva == 0)
            {
                return IntPtr.Zero;
            }

            // IMAGE_EXPORT_DIRECTORY 关键字段：NumberOfFunctions(+20) NumberOfNames(+24)
            // AddressOfFunctions(+28) AddressOfNames(+32) AddressOfNameOrdinals(+36)
            var expAddr = RvaPtr(moduleBase, exportRva);
            var numberOfFunctions = ReadUInt32(hProc, IntPtr.Add(expAddr, 20));
            var numberOfNames = ReadUInt32(hProc, IntPtr.Add(expAddr, 24));
            var addrOfFunctions = ReadUInt32(hProc, IntPtr.Add(expAddr, 28));
            var addrOfNames = ReadUInt32(hProc, IntPtr.Add(expAddr, 32));
            var addrOfNameOrdinals = ReadUInt32(hProc, IntPtr.Add(expAddr, 36));

            if (numberOfFunctions == 0 || numberOfNames == 0 || numberOfNames > 0xFFFF)
            {
                return IntPtr.Zero;
            }

            for (uint i = 0; i < numberOfNames; i++)
            {
                var nameRva = ReadUInt32(hProc, RvaPtr(moduleBase, addrOfNames + i * 4u));
                if (nameRva == 0)
                {
                    continue;
                }

                var name = ReadAsciiString(hProc, RvaPtr(moduleBase, nameRva), 256);
                if (!string.Equals(name, procName, StringComparison.Ordinal))
                {
                    continue;
                }

                var ordinal = ReadUInt16(hProc, RvaPtr(moduleBase, addrOfNameOrdinals + i * 2u));
                var funcRva = ReadUInt32(hProc, RvaPtr(moduleBase, addrOfFunctions + ordinal * 4u));
                if (funcRva == 0)
                {
                    return IntPtr.Zero;
                }

                // LoadLibraryW 为非转发导出，模块基址 + RVA 即为 32 位入口地址
                return RvaPtr(moduleBase, funcRva);
            }

            return IntPtr.Zero;
        }

        /// <summary>RVA 转绝对地址（防御 RVA 超出有符号 32 位范围）</summary>
        private static IntPtr RvaPtr(IntPtr moduleBase, uint rva)
        {
            if (rva > 0x7FFFFFFF)
            {
                return IntPtr.Zero;
            }

            return IntPtr.Add(moduleBase, (int)rva);
        }

        private static uint ReadUInt32(IntPtr hProc, IntPtr addr)
        {
            var buf = new byte[4];
            return ReadProcessMemory(hProc, addr, buf, buf.Length, out _) ? BitConverter.ToUInt32(buf, 0) : 0;
        }

        private static ushort ReadUInt16(IntPtr hProc, IntPtr addr)
        {
            var buf = new byte[2];
            return ReadProcessMemory(hProc, addr, buf, buf.Length, out _) ? BitConverter.ToUInt16(buf, 0) : (ushort)0;
        }

        private static string? ReadAsciiString(IntPtr hProc, IntPtr addr, int maxLen)
        {
            var buf = new byte[maxLen];
            if (!ReadProcessMemory(hProc, addr, buf, buf.Length, out _))
            {
                return null;
            }

            var len = Array.IndexOf(buf, (byte)0);
            return len < 0 ? Encoding.ASCII.GetString(buf) : Encoding.ASCII.GetString(buf, 0, len);
        }
    }
}
