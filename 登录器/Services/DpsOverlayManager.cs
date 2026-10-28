using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Linq;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using System.Windows.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 游戏助手 - DPS 伤害统计（登录器内部实现，不注入 DLL）。
    /// 方案：WriteProcessMemory 向游戏进程写入内嵌跳板（inline hook），
    /// 在伤害点 0x02561680（mov ebx,eax 处）捕获 eax:edx 64 位伤害值，key 取 [ebp+0x14] 攻击者对象分槽累加，
    /// 登录器定时 ReadProcessMemory 读取，透明置顶覆盖层按玩家分行显示排行。
    /// 名字：外部读 [key+0x400] 得名称指针，再读 UTF-16LE 角色名（不硬编码）。
    /// 城镇判定：[0x052B5BB0]+0xBC==0 视为城镇/未进副本，隐藏面板并清空数据。
    /// 归属：非本地玩家 key（特效/召唤物，如外挂技能生成的"闪电"）扫描其对象内存前 4KB，
    /// 找到指向本地玩家对象的指针（>=2 处命中）即归并到本地玩家行；队友特效指向队友对象，不会误归并。
    /// </summary>
    public static class DpsOverlayManager
    {
        private static readonly ConcurrentDictionary<int, DpsSession> _sessions = new();

        /// <summary>对指定游戏进程启动 DPS 统计（幂等）。</summary>
        public static void Start(Process process)
        {
            if (process == null || process.HasExited)
            {
                return;
            }

            var pid = process.Id;
            if (_sessions.ContainsKey(pid))
            {
                return;
            }

            try
            {
                var session = new DpsSession(process);
                _sessions[pid] = session;
                session.Begin();
            }
            catch (Exception ex)
            {
                Debug.WriteLine("[DPS] 启动失败: " + ex.Message);
            }
        }

        /// <summary>停止并清理指定进程的 DPS 统计。</summary>
        public static void Stop(int pid)
        {
            if (_sessions.TryRemove(pid, out var session))
            {
                session.Dispose();
            }
        }

        /// <summary>停止并清理所有运行中的 DPS 统计会话（DPS 开关关闭时一键停止）。</summary>
        public static void StopAll()
        {
            foreach (var pid in _sessions.Keys.ToArray())
            {
                Stop(pid);
            }
        }
    }

    /// <summary>单个游戏进程的 DPS 会话。</summary>
    internal sealed class DpsSession : IDisposable
    {
        // ---- 关键地址（基于 ImageBase 0x400000 的固定绝对地址）----
        private const uint HookAddr = 0x02561680;      // hook 点（mov ebx,eax 处，eax=伤害低32位，edx=高32位，[ebp+0x14]=攻击者对象）
        private const uint HookOffset = HookAddr - 0x400000; // 0x02161680 模块偏移
        private const uint TownBaseAddr = 0x052B5BB0;  // 城镇判定基址 / 人物基址

        // ---- 跳板汇编（x86）----
        // 槽布局（16 字节/槽 × 8 槽）：+0 伤害低32位  +4 攻击者对象指针(key)  +8 伤害高32位  +12 保留
        private static readonly byte[] TrampolineTemplate =
        {
            0x60,                // 0x00 pushad
            0x9C,                // 0x01 pushfd
            0x52,                // 0x02 push edx        ; 高32
            0x50,                // 0x03 push eax        ; 低32
            0x8B, 0x4D, 0x14,    // 0x04 mov ecx, [ebp+0x14] ; key = 攻击者对象（玩家）
            0xBF, 0x00,0x00,0x00,0x00, // 0x07 mov edi, counterBase
            0x31, 0xDB,          // 0x0C xor ebx, ebx    ; index = 0
            0x89, 0xD8,          // 0x0E mov eax, ebx
            0xC1, 0xE0, 0x04,    // 0x10 shl eax, 4      ; eax = index*16
            0x39, 0x4C, 0x07, 0x04, // 0x13 cmp [edi+eax+4], ecx
            0x74, 0x0F,          // 0x17 je 0x28 (mov eax,ebx) 保留槽索引
            0x83, 0x7C, 0x07, 0x04, 0x00, // 0x19 cmp dword [edi+eax+4], 0
            0x74, 0x08,          // 0x1E je 0x28 (mov eax,ebx) 保留槽索引
            0x43,                // 0x20 inc ebx
            0x83, 0xFB, 0x08,    // 0x21 cmp ebx, 8
            0x72, 0xE8,          // 0x24 jb 0x0E
            0x31, 0xDB,          // 0x26 xor ebx, ebx    ; 槽满覆盖槽0
            0x89, 0xD8,          // 0x28 mov eax, ebx
            0xC1, 0xE0, 0x04,    // 0x2A shl eax, 4
            0x89, 0x4C, 0x07, 0x04, // 0x2D mov [edi+eax+4], ecx  ; 写 key
            0x59,                // 0x31 pop ecx         ; 低32伤害
            0x5A,                // 0x32 pop edx         ; 高32伤害
            0xF0, 0x01, 0x0C, 0x07,    // 0x33 lock add [edi+eax], ecx
            0xF0, 0x11, 0x54, 0x07, 0x08, // 0x37 lock adc [edi+eax+8], edx
            0x9D,                // 0x3C popfd
            0x61,                // 0x3D popad
            // 0x3E 原5字节（运行时填充）
            // 0x43 E9 rel32 回跳 hook+5
        };

        private const int TrampolineLen = 0x3E;      // 跳板固定部分长度（模板长度，0x3E 字节，其后依次为原5字节+E9回跳）
        private const int SlotCount = 8;
        private const int SlotSize = 16;
        private const int CounterSize = SlotCount * SlotSize;

        private readonly Process _process;
        private IntPtr _hProcess;
        private uint _moduleBase;
        private uint _trampolineAddr;
        private uint _counterAddr;
        private byte[] _originalBytes = new byte[5];
        private bool _hooked;
        private bool _disposed;

        private Window? _overlay;
        private DispatcherTimer? _timer;
        private DispatcherTimer? _statusTimer;
        private WinEventDelegate? _winEventDelegate; // 保持引用防止回调被 GC
        private IntPtr _hookForeground;
        private IntPtr _hookMinimize;
        private IntPtr _hookMove;
        private readonly Dictionary<uint, string> _nameCache = new();
        private readonly Dictionary<uint, bool> _ownerCache = new();
        private bool _wasInTown = true;
        private bool _hasDamage; // 当前是否有伤害数据可显示（由数据 Timer 维护，状态 Timer 据此决定显隐）
        private bool _overlayVisible = true; // Show() 后窗口实际可见，初始置 true 以便首次 SetOverlayVisible(false) 生效
        private bool _dragging; // DragMove 期间置 true，禁止跟随定位避免拖动闪现
        private int _dragOffsetX = 20;
        private int _dragOffsetY = 76;
        private readonly List<PlayerRow> _rowControls = new();

        public DpsSession(Process process)
        {
            _process = process;
        }

        public void Begin()
        {
            // 等待模块加载（游戏启动后 DLL 可能需要几秒）
            if (!WaitForModule())
            {
                Dispose();
                return;
            }

            _hProcess = OpenProcess(PROCESS_ALL_ACCESS, false, _process.Id);
            if (_hProcess == IntPtr.Zero)
            {
                Dispose();
                return;
            }

            if (!InstallHook())
            {
                Dispose();
                return;
            }

            CreateOverlay();
            RegisterWinEventHooks();
            _timer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(1) };
            _timer.Tick += OnTick;
            _timer.Start();

            _statusTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(250) };
            _statusTimer.Tick += OnStatusTick;
            _statusTimer.Start();
        }

        // ---------------- Hook 安装 ----------------

        private bool WaitForModule()
        {
            for (int i = 0; i < 30; i++)
            {
                if (_process.HasExited)
                {
                    return false;
                }

                _moduleBase = GetModuleBase(_process);
                if (_moduleBase != 0)
                {
                    return true;
                }

                Thread.Sleep(500);
            }

            return false;
        }

        private uint GetModuleBase(Process process)
        {
            try
            {
                uint[] mods = new uint[1024];
                uint needed = 0;
                if (EnumProcessModulesEx(process.Handle, mods, (uint)mods.Length * 4, out needed, LIST_MODULES_32BIT))
                {
                    if (needed > 0 && mods[0] != 0)
                    {
                        return mods[0];
                    }
                }
            }
            catch
            {
                // 忽略，走兜底
            }

            // 兜底：DNF 客户端默认 ImageBase
            return 0x400000;
        }

        private bool InstallHook()
        {
            uint hookAddr = _moduleBase + HookOffset;
            uint hookEnd = hookAddr + 5;

            // 1. 备份原 5 字节
            if (!ReadProcessMemory(_hProcess, (IntPtr)hookAddr, _originalBytes, 5, out _))
            {
                return false;
            }

            // 2. 分配跳板 + 计数器（一次分配 0x200 字节）
            uint alloc = (uint)VirtualAllocEx(_hProcess, IntPtr.Zero, 0x200, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
            if (alloc == 0)
            {
                return false;
            }

            _trampolineAddr = alloc;
            _counterAddr = alloc + 0x80;

            // 3. 组装跳板
            byte[] code = new byte[TrampolineLen + 5 + 5];
            Array.Copy(TrampolineTemplate, code, TrampolineTemplate.Length);
            // 填充 counterBase：模板中 mov edi,imm32 的 imm32 位于 code[0x08..0x0B]
            // （0x04-0x06 是 mov ecx,[ebp+0x14]，0x07 是 BF 操作码，imm 从 0x08 起）
            byte[] cb = BitConverter.GetBytes(_counterAddr);
            Array.Copy(cb, 0, code, 0x08, 4);
            // 原 5 字节
            Array.Copy(_originalBytes, 0, code, TrampolineLen, 5);
            // 回跳 E9 rel32 → hookEnd
            code[TrampolineLen + 5] = 0xE9;
            uint rel = unchecked(hookEnd - (uint)(_trampolineAddr + TrampolineLen + 5 + 5));
            byte[] relb = BitConverter.GetBytes(rel);
            Array.Copy(relb, 0, code, TrampolineLen + 5 + 1, 4);

            // 4. 写入跳板
            if (!WriteProcessMemory(_hProcess, (IntPtr)_trampolineAddr, code, code.Length, out _))
            {
                return false;
            }

            // 5. 清零计数器区
            byte[] zero = new byte[CounterSize];
            WriteProcessMemory(_hProcess, (IntPtr)_counterAddr, zero, zero.Length, out _);

            // 6. 改写 hook 点：E9 跳转到跳板
            byte[] jmp = new byte[5];
            jmp[0] = 0xE9;
            uint jmpRel = unchecked(_trampolineAddr - hookEnd);
            byte[] jmpRelb = BitConverter.GetBytes(jmpRel);
            Array.Copy(jmpRelb, 0, jmp, 1, 4);

            if (!VirtualProtectEx(_hProcess, (IntPtr)hookAddr, 5, PAGE_EXECUTE_READWRITE, out _))
            {
                return false;
            }

            if (!WriteProcessMemory(_hProcess, (IntPtr)hookAddr, jmp, 5, out _))
            {
                return false;
            }

            _hooked = true;
            return true;
        }

        private void UninstallHook()
        {
            if (!_hooked || _hProcess == IntPtr.Zero)
            {
                return;
            }

            uint hookAddr = _moduleBase + HookOffset;
            VirtualProtectEx(_hProcess, (IntPtr)hookAddr, 5, PAGE_EXECUTE_READWRITE, out _);
            WriteProcessMemory(_hProcess, (IntPtr)hookAddr, _originalBytes, 5, out _);
            _hooked = false;
        }

        // ---------------- 数据读取与 UI ----------------

        // 数据刷新（1 秒）：读计数器更新行数据；显隐由 _statusTimer（250ms）统一控制
        private void OnTick(object? sender, EventArgs e)
        {
            try
            {
                if (_disposed || _process.HasExited)
                {
                    Dispose();
                    return;
                }

                ReadAndUpdate();
            }
            catch
            {
                // 单次刷新失败不致命
            }
        }

        // 前台/显隐状态（250ms 轮询，仅作兜底；即时响应由 WinEventHook 回调驱动）
        private void OnStatusTick(object? sender, EventArgs e)
        {
            try
            {
                if (_disposed || _process.HasExited)
                {
                    Dispose();
                    return;
                }

                UpdateVisibility();
            }
            catch
            {
                // 单次状态刷新失败不致命
            }
        }

        /// <summary>
        /// 统一显隐判定：最小化 / 切走即时隐藏，切回即时恢复；
        /// 拖动 DPS 时前台是覆盖层自身（fg==overlayHwnd），也视为游戏场景，避免拖动完闪没。
        /// 由 WinEventHook 回调（事件驱动，0 延迟）与 250ms 轮询（兜底）共同调用。
        /// </summary>
        private void UpdateVisibility()
        {
            if (_disposed || _process.HasExited || _overlay == null)
            {
                return;
            }

            var gameHwnd = _process.MainWindowHandle;
            bool inTown = IsInTown();

            // 刚进副本：清空上一轮数据
            if (!inTown && _wasInTown)
            {
                _wasInTown = false;
                byte[] zero = new byte[CounterSize];
                WriteProcessMemory(_hProcess, (IntPtr)_counterAddr, zero, zero.Length, out _);
                _nameCache.Clear();
                _ownerCache.Clear();
            }
            if (inTown)
            {
                _wasInTown = true;
            }

            IntPtr fg = GetForegroundWindow();
            IntPtr overlayHwnd = new System.Windows.Interop.WindowInteropHelper(_overlay).Handle;
            bool show = !IsIconic(gameHwnd)
                && (fg == gameHwnd || fg == overlayHwnd)
                && !inTown
                && _hasDamage;

            if (show)
            {
                FollowGameWindow(); // 先定位再显示，避免在旧位置闪现
            }
            SetOverlayVisible(show);
        }

        // ---------------- WinEventHook（事件驱动显隐，0 延迟）----------------

        private void RegisterWinEventHooks()
        {
            try
            {
                _winEventDelegate = OnWinEvent;
                // 前台切换：任意窗口成为前台立即回调（切走/切回/点 DPS）
                _hookForeground = SetWinEventHook(EVENT_SYSTEM_FOREGROUND, EVENT_SYSTEM_FOREGROUND,
                    IntPtr.Zero, _winEventDelegate, 0, 0, WINEVENT_OUTOFCONTEXT);
                // 游戏/覆盖层最小化、还原
                _hookMinimize = SetWinEventHook(EVENT_SYSTEM_MINIMIZESTART, EVENT_SYSTEM_MINIMIZEEND,
                    IntPtr.Zero, _winEventDelegate, 0, 0, WINEVENT_OUTOFCONTEXT);
                // 游戏窗口移动/大小变化结束
                _hookMove = SetWinEventHook(EVENT_SYSTEM_MOVESIZEEND, EVENT_SYSTEM_MOVESIZEEND,
                    IntPtr.Zero, _winEventDelegate, 0, 0, WINEVENT_OUTOFCONTEXT);
            }
            catch
            {
                // hook 失败不影响主流程，250ms 轮询兜底
            }
        }

        private void UnregisterWinEventHooks()
        {
            try
            {
                if (_hookForeground != IntPtr.Zero)
                {
                    UnhookWinEvent(_hookForeground);
                    _hookForeground = IntPtr.Zero;
                }

                if (_hookMinimize != IntPtr.Zero)
                {
                    UnhookWinEvent(_hookMinimize);
                    _hookMinimize = IntPtr.Zero;
                }

                if (_hookMove != IntPtr.Zero)
                {
                    UnhookWinEvent(_hookMove);
                    _hookMove = IntPtr.Zero;
                }
            }
            catch
            {
                // 清理失败不影响登录器
            }
        }

        private void OnWinEvent(IntPtr hWinEventHook, uint eventType, IntPtr hwnd, int idObject, int idChild, uint dwEventThread, uint dwmsEventTime)
        {
            try
            {
                if (_disposed || _overlay == null)
                {
                    return;
                }

                if (eventType == EVENT_SYSTEM_FOREGROUND)
                {
                    // 前台变化：无需看 hwnd，UpdateVisibility 内部自行判断当前前台是否游戏/覆盖层
                    UpdateVisibility();
                    return;
                }

                // 最小化/还原/移动：只关心游戏窗口与覆盖层自身
                var gameHwnd = _process.MainWindowHandle;
                IntPtr overlayHwnd = new System.Windows.Interop.WindowInteropHelper(_overlay).Handle;
                if (hwnd == gameHwnd || hwnd == overlayHwnd)
                {
                    UpdateVisibility();
                }
            }
            catch
            {
                // 回调异常忽略
            }
        }

        private bool IsInTown()
        {
            if (!ReadUInt(TownBaseAddr, out uint ptr) || ptr == 0)
            {
                return true;
            }

            if (!ReadUInt(ptr + 0xBC, out uint flag))
            {
                return true;
            }

            return flag == 0;
        }

        private bool ReadUInt(uint addr, out uint value)
        {
            value = 0;
            byte[] buf = new byte[4];
            if (!ReadProcessMemory(_hProcess, (IntPtr)addr, buf, 4, out _))
            {
                return false;
            }

            value = BitConverter.ToUInt32(buf, 0);
            return true;
        }

        private void ReadAndUpdate()
        {
            byte[] buf = new byte[CounterSize];
            if (!ReadProcessMemory(_hProcess, (IntPtr)_counterAddr, buf, buf.Length, out _))
            {
                return;
            }

            // 槽位 key = [ebp+0x14] 攻击者对象：同一玩家攻击多目标共享同一 key，天然聚合；
            // 组队时不同玩家 key 不同，各占一行；名字从 [key+0x400] 名称指针读 UTF-16LE。
            // 特效/召唤物（如 BLAST）也是独立 key，但其伤害应归属释放者，统一并入本地玩家行。
            uint localPlayer = 0;
            ReadUInt(TownBaseAddr, out uint lpPtr);
            if (lpPtr != 0)
            {
                localPlayer = lpPtr;
            }

            var damageByKey = new Dictionary<uint, ulong>();
            for (int i = 0; i < SlotCount; i++)
            {
                uint key = BitConverter.ToUInt32(buf, i * SlotSize + 4);
                uint low = BitConverter.ToUInt32(buf, i * SlotSize + 0);
                uint high = BitConverter.ToUInt32(buf, i * SlotSize + 8);
                if (key == 0)
                {
                    continue;
                }

                ulong d = ((ulong)high << 32) | low;
                if (d == 0)
                {
                    continue;
                }

                // 特效/召唤物伤害归属：key 为特效/召唤物对象（如外挂技能生成的"闪电"）时，
                // 其内存中必然存在指向施法者（释放者）的指针，但偏移未知（+0x880 实测无效）。
                // 这里直接扫描 key 对象内存前 4KB，找到值为本地玩家对象的指针即归并到本地玩家行；
                // 队友的召唤物/特效施法者是队友对象（!=本地玩家），不会被归并，仍各占一行。
                if (localPlayer != 0 && key != localPlayer)
                {
                    if (_ownerCache.TryGetValue(key, out bool ownedByLocal))
                    {
                        if (ownedByLocal)
                        {
                            key = localPlayer;
                        }
                    }
                    else
                    {
                        ownedByLocal = ScanForLocalPlayerRef(key, localPlayer);
                        _ownerCache[key] = ownedByLocal;
                        if (ownedByLocal)
                        {
                            key = localPlayer;
                        }
                    }
                }

                damageByKey.TryGetValue(key, out ulong cur);
                damageByKey[key] = cur + d;
            }

            if (damageByKey.Count == 0)
            {
                _hasDamage = false;
                return;
            }

            var ordered = damageByKey
                .Select(kv => (Key: kv.Key, Damage: kv.Value))
                .OrderByDescending(p => p.Damage)
                .ToList();
            ulong total = 0;
            foreach (var p in ordered)
            {
                total += p.Damage;
            }
            if (total == 0)
            {
                _hasDamage = false;
                return;
            }

            // 更新 UI
            for (int i = 0; i < _rowControls.Count; i++)
            {
                bool has = i < ordered.Count;
                var row = _rowControls[i];
                if (!has)
                {
                    row.SetVisible(false);
                    continue;
                }

                var p = ordered[i];
                string name = ResolveName(p.Key, i);
                double pct = (double)p.Damage / total * 100.0;
                row.SetData(name, p.Damage, pct, (double)p.Damage / ordered[0].Damage);
            }

            UpdateOverlaySize(ordered.Count);
            _hasDamage = true;
            FollowGameWindow();
        }

        private string ResolveName(uint key, int fallbackIndex)
        {
            if (_nameCache.TryGetValue(key, out var cached))
            {
                return cached;
            }

            string? name = TryReadPlayerName(key);
            if (string.IsNullOrEmpty(name))
            {
                name = "玩家" + (fallbackIndex + 1);
            }

            _nameCache[key] = name;
            return name;
        }

        private string? TryReadPlayerName(uint playerObj)
        {
            // 名称指针：[playerObj+0x400]
            byte[] ptrBuf = new byte[4];
            if (!ReadProcessMemory(_hProcess, (IntPtr)(playerObj + 0x400), ptrBuf, 4, out _))
            {
                return null;
            }

            uint namePtr = BitConverter.ToUInt32(ptrBuf, 0);
            if (namePtr == 0 || namePtr < 0x00100000 || namePtr > 0x7FFFFFFF)
            {
                return null;
            }

            // 读 UTF-16LE 名字（最多 32 字符 = 64 字节）
            byte[] nameBuf = new byte[64];
            if (!ReadProcessMemory(_hProcess, (IntPtr)namePtr, nameBuf, nameBuf.Length, out _))
            {
                return null;
            }

            var sb = new StringBuilder();
            for (int i = 0; i + 1 < nameBuf.Length; i += 2)
            {
                char c = (char)(nameBuf[i] | (nameBuf[i + 1] << 8));
                if (c == '\0')
                {
                    break;
                }

                sb.Append(c);
            }

            string name = sb.ToString().Trim();
            return name.Length >= 1 ? name : null;
        }

        /// <summary>
        /// 扫描对象内存前 4KB，统计值为 localPlayer 的 4 字节对齐指针。
        /// 命中即认为该对象（特效/召唤物）的施法者是本地玩家，伤害应并入本地玩家行。
        /// 要求至少 2 处命中，降低对象内偶然出现的同值字段导致的误归并。
        /// </summary>
        private bool ScanForLocalPlayerRef(uint obj, uint localPlayer)
        {
            if (obj == 0 || obj < 0x00100000 || obj > 0x7FFEFFFF)
            {
                return false;
            }

            byte[] buf = new byte[0x1000];
            if (!ReadProcessMemory(_hProcess, (IntPtr)obj, buf, buf.Length, out _))
            {
                return false;
            }

            int hits = 0;
            for (int i = 0; i + 4 <= buf.Length; i += 4)
            {
                uint v = BitConverter.ToUInt32(buf, i);
                if (v == localPlayer)
                {
                    hits++;
                    if (hits >= 2)
                    {
                        return true;
                    }
                }
            }

            return false;
        }

        // 单位链：1000进一级，所有级别两位小数，Q 为最高单位封顶不进级
        private static readonly string[] DpsUnits = { "k", "M", "G", "T", "P", "E", "Z", "Y", "R", "Q" };

        private static string FormatDamage(ulong d)
        {
            if (d < 1000)
            {
                return d.ToString();
            }

            double v = d;
            int idx = -1;
            while (v >= 1000 && idx < DpsUnits.Length - 1)
            {
                v /= 1000;
                idx++;
            }

            return v.ToString("0.00") + DpsUnits[idx];
        }

        // ---------------- 覆盖层窗口 ----------------

        private void CreateOverlay()
        {
            _overlay = new Window
            {
                WindowStyle = WindowStyle.None,
                AllowsTransparency = true,
                Background = Brushes.Transparent,
                ShowInTaskbar = false,
                ShowActivated = false,
                Topmost = true, // 置顶保证始终盖在游戏窗口之上；是否显示由前台检测控制
                Width = 180,
                Height = 90,
                ResizeMode = ResizeMode.NoResize
            };

            var root = new Border
            {
                Background = new SolidColorBrush(Color.FromArgb(0xE6, 0x14, 0x14, 0x18)),
                CornerRadius = new CornerRadius(8),
                BorderBrush = new SolidColorBrush(Color.FromArgb(0x66, 0x00, 0x00, 0x00)),
                BorderThickness = new Thickness(1),
                Padding = new Thickness(8)
            };

            var panel = new StackPanel();

            // 标题行（小号 DPS）
            panel.Children.Add(new TextBlock
            {
                Text = "DPS",
                Foreground = Brushes.White,
                FontSize = 10,
                FontWeight = FontWeights.Bold,
                Margin = new Thickness(0, 0, 0, 4)
            });

            // 玩家行 × 4
            for (int i = 0; i < 4; i++)
            {
                var row = new PlayerRow();
                panel.Children.Add(row.Root);
                _rowControls.Add(row);
            }

            root.Child = panel;
            _overlay.Content = root;

            // 跟随游戏窗口显示：置顶 + 前台检测控制显隐（不再建立 owner 关系，
            // owner 会让 Win32 窗口状态与 WPF Visibility 不同步，切走再切回后 DPS 无法恢复）
            _overlay.SourceInitialized += (_, _) =>
            {
                var hwnd = new System.Windows.Interop.WindowInteropHelper(_overlay!).Handle;
                int ex = GetWindowLong(hwnd, GWL_EXSTYLE);
                SetWindowLong(hwnd, GWL_EXSTYLE, ex | WS_EX_TOOLWINDOW);
            };

            // 鼠标左键按住可拖动；拖动结束后记录相对游戏窗口的偏移，跟随逻辑保持新位置。
            // _dragging 标志在 DragMove 期间禁止 FollowGameWindow 强制定位，避免每秒 Tick 把窗口拉回旧位置造成闪现
            _overlay.MouseLeftButtonDown += (_, _) =>
            {
                try
                {
                    _dragging = true;
                    _overlay.DragMove();
                }
                catch
                {
                    // 拖动失败忽略
                }
                finally
                {
                    _dragging = false;
                    if (GetWindowRect(new System.Windows.Interop.WindowInteropHelper(_overlay).Handle, out RECT overlayRect) &&
                        GetWindowRect(_process.MainWindowHandle, out RECT gameRect))
                    {
                        _dragOffsetX = overlayRect.Left - gameRect.Left;
                        _dragOffsetY = overlayRect.Top - gameRect.Top;
                    }
                }
            };

            _overlay.Show();
            SetOverlayVisible(false);
        }

        private void SetOverlayVisible(bool visible)
        {
            if (_overlay == null || _overlayVisible == visible)
            {
                return;
            }

            _overlay.Visibility = visible ? Visibility.Visible : Visibility.Collapsed;
            _overlayVisible = visible;
        }

        /// <summary>按实际玩家行数调整覆盖层尺寸（宽固定 180，行高 24px）。</summary>
        private void UpdateOverlaySize(int playerCount)
        {
            if (_overlay == null)
            {
                return;
            }

            int rows = Math.Max(1, Math.Min(4, playerCount));
            _overlay.Width = 180;
            _overlay.Height = 34 + rows * 24;
        }

        private void FollowGameWindow()
        {
            if (_overlay == null || _dragging)
            {
                return;
            }

            if (GetWindowRect(_process.MainWindowHandle, out RECT rect))
            {
                int w = 180;
                int h = (int)_overlay.Height;
                int left = rect.Left + _dragOffsetX;
                int top = rect.Top + _dragOffsetY;
                SetWindowPos(new System.Windows.Interop.WindowInteropHelper(_overlay).Handle,
                    HWND_TOPMOST, left, top, w, h, SWP_NOACTIVATE | SWP_SHOWWINDOW);
            }
        }

        // ---------------- 玩家行控件 ----------------

        private sealed class PlayerRow
        {
            public Border Root { get; }
            private readonly TextBlock _name;
            private readonly TextBlock _damage;
            private readonly Border _bar;
            private readonly Border _barFill;

            public PlayerRow()
            {
                Root = new Border { Margin = new Thickness(0, 2, 0, 2) };
                var grid = new Grid();

                // 进度条（加宽），文字叠加其上
                _bar = new Border
                {
                    Height = 20,
                    Background = new SolidColorBrush(Color.FromArgb(0x50, 0xFF, 0xFF, 0xFF)),
                    CornerRadius = new CornerRadius(3)
                };
                _barFill = new Border
                {
                    Background = new SolidColorBrush(Color.FromRgb(0x4F, 0xA8, 0xFF)),
                    CornerRadius = new CornerRadius(3),
                    HorizontalAlignment = HorizontalAlignment.Left
                };
                _bar.Child = _barFill;

                var line = new Grid();
                line.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
                line.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
                _name = new TextBlock
                {
                    Foreground = Brushes.White,
                    FontSize = 11,
                    FontWeight = FontWeights.Bold,
                    TextTrimming = TextTrimming.CharacterEllipsis,
                    Margin = new Thickness(6, 0, 4, 0),
                    VerticalAlignment = VerticalAlignment.Center
                };
                _damage = new TextBlock
                {
                    Foreground = Brushes.White,
                    FontSize = 11,
                    FontWeight = FontWeights.Bold,
                    Margin = new Thickness(0, 0, 6, 0),
                    VerticalAlignment = VerticalAlignment.Center
                };
                Grid.SetColumn(_damage, 1);
                line.Children.Add(_name);
                line.Children.Add(_damage);

                grid.Children.Add(_bar);
                grid.Children.Add(line);
                Root.Child = grid;
            }

            public void SetData(string name, ulong damage, double pct, double ratio)
            {
                Root.Visibility = Visibility.Visible;
                _name.Text = name;
                _damage.Text = FormatDamage(damage) + "  " + pct.ToString("0.0") + "%";
                double w = Math.Max(2.0, Math.Min(1.0, ratio) * _bar.ActualWidth);
                if (_bar.ActualWidth <= 0)
                {
                    w = 100; // 首次布局未完成时占位
                }

                _barFill.Width = w;
            }

            public void SetVisible(bool visible)
            {
                Root.Visibility = visible ? Visibility.Visible : Visibility.Collapsed;
            }

            private static string FormatDamage(ulong d) => DpsSession.FormatDamage(d);
        }

        // ---------------- 清理 ----------------

        public void Dispose()
        {
            if (_disposed)
            {
                return;
            }

            _disposed = true;

            try
            {
                _timer?.Stop();
                _timer = null;

                _statusTimer?.Stop();
                _statusTimer = null;

                UnregisterWinEventHooks();

                UninstallHook();

                _overlay?.Close();
                _overlay = null;

                if (_hProcess != IntPtr.Zero)
                {
                    CloseHandle(_hProcess);
                    _hProcess = IntPtr.Zero;
                }
            }
            catch
            {
                // 清理失败不影响登录器
            }
        }

        // ---------------- Win32 ----------------

        private const uint PROCESS_ALL_ACCESS = 0x1F0FFF;
        private const uint MEM_COMMIT = 0x1000;
        private const uint MEM_RESERVE = 0x2000;
        private const uint PAGE_EXECUTE_READWRITE = 0x40;
        private const uint LIST_MODULES_32BIT = 0x01;
        private const int GWL_EXSTYLE = -20;
        private const int WS_EX_TRANSPARENT = 0x20;
        private const int WS_EX_TOOLWINDOW = 0x80;
        private static readonly IntPtr HWND_TOPMOST = new(-1);
        private static readonly IntPtr HWND_TOP = new(0);
        private const uint SWP_NOACTIVATE = 0x10;
        private const uint SWP_SHOWWINDOW = 0x40;

        // ---- WinEventHook ----
        private const uint EVENT_SYSTEM_FOREGROUND = 0x0003;
        private const uint EVENT_SYSTEM_MINIMIZESTART = 0x0016;
        private const uint EVENT_SYSTEM_MINIMIZEEND = 0x0017;
        private const uint EVENT_SYSTEM_MOVESIZEEND = 0x000A;
        private const uint WINEVENT_OUTOFCONTEXT = 0x0000;

        private delegate void WinEventDelegate(IntPtr hWinEventHook, uint eventType, IntPtr hwnd, int idObject, int idChild, uint dwEventThread, uint dwmsEventTime);

        [StructLayout(LayoutKind.Sequential)]
        private struct RECT
        {
            public int Left;
            public int Top;
            public int Right;
            public int Bottom;
        }

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr OpenProcess(uint dwDesiredAccess, bool bInheritHandle, int dwProcessId);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr hObject);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool ReadProcessMemory(IntPtr hProcess, IntPtr lpBaseAddress, byte[] lpBuffer, int dwSize, out IntPtr lpNumberOfBytesRead);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool WriteProcessMemory(IntPtr hProcess, IntPtr lpBaseAddress, byte[] lpBuffer, int nSize, out IntPtr lpNumberOfBytesWritten);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern IntPtr VirtualAllocEx(IntPtr hProcess, IntPtr lpAddress, uint dwSize, uint flAllocationType, uint flProtect);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool VirtualProtectEx(IntPtr hProcess, IntPtr lpAddress, uint dwSize, uint flNewProtect, out uint lpflOldProtect);

        [DllImport("psapi.dll", SetLastError = true)]
        private static extern bool EnumProcessModulesEx(IntPtr hProcess, uint[] lphModule, uint cb, out uint lpcbNeeded, uint dwFilterFlag);

        [DllImport("user32.dll")]
        private static extern bool GetWindowRect(IntPtr hWnd, out RECT lpRect);

        [DllImport("user32.dll")]
        private static extern bool IsIconic(IntPtr hWnd);

        [DllImport("user32.dll")]
        private static extern IntPtr GetForegroundWindow();

        [DllImport("user32.dll")]
        private static extern IntPtr SetWinEventHook(uint eventMin, uint eventMax, IntPtr hmodWinEventProc, WinEventDelegate lpfnWinEventProc, uint idProcess, uint idThread, uint dwFlags);

        [DllImport("user32.dll")]
        private static extern bool UnhookWinEvent(IntPtr hWinEventHook);

        [DllImport("user32.dll")]
        private static extern bool SetWindowPos(IntPtr hWnd, IntPtr hWndInsertAfter, int X, int Y, int cx, int cy, uint uFlags);

        [DllImport("user32.dll")]
        private static extern int GetWindowLong(IntPtr hWnd, int nIndex);

        [DllImport("user32.dll")]
        private static extern int SetWindowLong(IntPtr hWnd, int nIndex, int dwNewLong);
    }
}
