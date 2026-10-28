using System.IO.MemoryMappedFiles;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;

namespace DNFLogin.Services
{
    /// <summary>
    /// 游戏助手 Helper DLL 共享内存门控：登录器进程创建命名共享内存，
    /// 写入顺图/拾取开关标志、全服公告，以及按账号隔离的私有到账提示；
    /// 注入到 DNF.exe 的 Helper DLL 读取。开关关闭（或共享内存不存在）时 DLL 完全静默。
    /// 私有区为固定槽位（8 个），每条消息携带目标账号，DLL 只展示与自身 DNF_ACCOUNT_ID
    /// 匹配的槽位消息，多开不串号。
    /// </summary>
    public static class HelperSharedMemory
    {
        private const string MapName = @"Local\YYBDNFHelper90";
        private const string PrivLockName = @"Local\YYBDNFHelper90PrivLock";
        // HelperState: 顺图(4) + 拾取(4) + 公告序号(4) + 公告颜色(4) + 公告内容(200 wchar = 400B) = 416
        private const int BaseSize = 416;
        // 私有通知区：8 槽 × 544B（账号 128B + 颜色 4 + 序号 4 + 内容 200wchar=400B + 时间戳 8B）
        // account_id 实际为 "dnf:"+sha256hex（68 字符），64B 放不下，故扩到 128B。
        private const int PrivSlots = 8;
        private const int PrivEntrySize = 544;
        private const int PrivBaseOffset = BaseSize;
        private const int PrivAccountOffset = 0;    // ASCII 账号（128B）
        private const int PrivAccountLen = 128;
        private const int PrivColorOffset = 128;
        private const int PrivSeqOffset = 132;
        private const int PrivMsgOffset = 136;      // UTF-16LE 内容（200 wchar）
        private const int PrivMsgWCells = 200;
        private const int PrivTsOffset = 536;       // int64 unix 秒
        private const int MapSize = BaseSize + PrivSlots * PrivEntrySize;
        private static readonly TimeSpan SlotLease = TimeSpan.FromHours(2);

        private const long PassOffset = 0;
        private const long PickupOffset = 4;
        private const long AnnounceSeqOffset = 8;
        private const long AnnounceColorOffset = 12;
        private const long AnnounceMsgOffset = 16;
        private const int AnnounceMsgWCells = 200;

        private static MemoryMappedFile? _map;
        private static MemoryMappedViewAccessor? _view;
        private static Mutex? _privLock;
        private static readonly object _mapLock = new object();

        /// <summary>确保共享内存已创建（幂等）。登录器注入/写开关前调用。</summary>
        public static void EnsureCreated()
        {
            if (_map != null) return;
            lock (_mapLock)
            {
                if (_map != null) return;
                try
                {
                    _map = MemoryMappedFile.CreateOrOpen(MapName, MapSize);
                    _view = _map.CreateViewAccessor(0, MapSize);
                    // 初始化为关闭状态（防止残留上次运行的值）；公告区一并清零（不补发历史公告）
                    WriteInt32(PassOffset, 0);
                    WriteInt32(PickupOffset, 0);
                    WriteInt32(AnnounceSeqOffset, 0);
                    WriteInt32(AnnounceColorOffset, 0);
                    ClearWChars(AnnounceMsgOffset);
                    _privLock = new Mutex(false, PrivLockName);
                }
                catch
                {
                    _map = null;
                    _view = null;
                }
            }
        }

        /// <summary>顺图开关（0=关 1=开）</summary>
        public static void SetPassEnabled(bool enabled)
        {
            EnsureCreated();
            WriteInt32(PassOffset, enabled ? 1 : 0);
        }

        /// <summary>拾取开关（0=关 1=开）</summary>
        public static void SetPickupEnabled(bool enabled)
        {
            EnsureCreated();
            WriteInt32(PickupOffset, enabled ? 1 : 0);
        }

        /// <summary>推送全服公告：先写 UTF-16 内容，最后写自增序号（序号后写，DLL 看到序号变化即代表内容完整就绪）。</summary>
        public static void SetAnnouncement(string msg, uint argbColor, int seq)
        {
            EnsureCreated();
            if (_view == null) return;
            WriteWChars(AnnounceMsgOffset, (msg ?? "").ToCharArray());
            WriteInt32(AnnounceColorOffset, unchecked((int)argbColor));
            WriteInt32(AnnounceSeqOffset, seq);
        }

        /// <summary>
        /// 推送按账号隔离的私有提示（泡点到账绿字）：先占/复用目标账号槽位，
        /// 写账号 → 颜色 → 内容 → 序号(最后) → 时间戳，DLL 只对自身账号匹配的新序号弹提示。
        /// </summary>
        public static void SetPrivateNotify(string account, string msg, uint argbColor)
        {
            EnsureCreated();
            if (_view == null || string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(msg))
            {
                return;
            }

            var acc = Encoding.ASCII.GetBytes(account.Trim());
            if (acc.Length <= 0 || acc.Length >= PrivAccountLen)
            {
                return; // 账号为空或超长（账号应为 ASCII 字符串），放弃推送避免串号
            }

            if (_privLock == null)
            {
                return;
            }

            bool locked = false;
            try
            {
                _privLock.WaitOne(2000);
                locked = true;
            }
            catch (AbandonedMutexException)
            {
                locked = true; // 上一持有者崩溃：互斥量已移交本线程，可安全继续
            }
            catch
            {
                locked = false;
            }
            if (!locked)
            {
                return;
            }

            try
            {
                long baseAddr = _view.SafeMemoryMappedViewHandle.DangerousGetHandle().ToInt64();
                long nowTs = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
                int idx = FindPrivateSlot(baseAddr, acc, nowTs);
                if (idx < 0)
                {
                    return;
                }

                long entry = PrivBaseOffset + idx * (long)PrivEntrySize;
                // 账号
                var accBuf = new byte[PrivAccountLen];
                Buffer.BlockCopy(acc, 0, accBuf, 0, acc.Length);
                Marshal.Copy(accBuf, 0, new IntPtr(baseAddr + entry + PrivAccountOffset), accBuf.Length);
                // 颜色
                Marshal.WriteInt32(new IntPtr(baseAddr), (int)(entry + PrivColorOffset), unchecked((int)argbColor));
                // 内容（UTF-16，≤199 wchar 保证有结尾空字符）
                WriteWChars(entry + PrivMsgOffset, (msg ?? "").ToCharArray());
                // 序号 +1（后写：DLL 看到序号变化即代表本条完整就绪）
                int prev = Marshal.ReadInt32(new IntPtr(baseAddr), (int)(entry + PrivSeqOffset));
                Marshal.WriteInt32(new IntPtr(baseAddr), (int)(entry + PrivSeqOffset), prev + 1);
                // 时间戳（后写：用于槽位租约判定）
                Marshal.WriteInt64(new IntPtr(baseAddr), (int)(entry + PrivTsOffset), nowTs);
            }
            catch
            {
                // 共享内存不可用时不阻塞主流程
            }
            finally
            {
                try
                {
                    _privLock.ReleaseMutex();
                }
                catch
                {
                }
            }
        }

        /// <summary>在私有区找一个可用槽位：账号匹配优先，其次过期/空闲槽；找不到返回 -1。</summary>
        private static int FindPrivateSlot(long baseAddr, byte[] acc, long nowTs)
        {
            var buf = new byte[PrivAccountLen];
            int fallback = -1;
            long oldestTs = nowTs;
            for (int i = 0; i < PrivSlots; i++)
            {
                long entry = PrivBaseOffset + i * (long)PrivEntrySize;
                Marshal.Copy(new IntPtr(baseAddr + entry + PrivAccountOffset), buf, 0, buf.Length);
                bool match = buf.Length >= PrivAccountLen;
                for (int k = 0; match && k < PrivAccountLen; k++)
                {
                    byte expect = k < acc.Length ? acc[k] : (byte)0;
                    if (buf[k] != expect)
                    {
                        match = false;
                    }
                }
                long ts = Marshal.ReadInt64(new IntPtr(baseAddr), (int)(entry + PrivTsOffset));
                if (match)
                {
                    return i; // 账号匹配即复用（正常是上次会话残留或崩溃遗留）
                }
                if (ts == 0 || nowTs - ts > (long)SlotLease.TotalSeconds)
                {
                    fallback = i; // 空闲/租约过期优先
                    continue;
                }
                if (ts < oldestTs)
                {
                    oldestTs = ts;
                    fallback = i; // 退而求其次选最旧槽
                }
            }
            return fallback;
        }

        private static void WriteInt32(long offset, int value)
        {
            try
            {
                if (_view == null) return;
                IntPtr ptr = _view.SafeMemoryMappedViewHandle.DangerousGetHandle();
                if (ptr == IntPtr.Zero) return;
                Marshal.WriteInt32(ptr, (int)offset, value);
            }
            catch
            {
                // 共享内存不可用时不阻塞主流程
            }
        }

        private static void WriteWChars(long offset, char[] chars)
        {
            try
            {
                if (_view == null) return;
                IntPtr ptr = _view.SafeMemoryMappedViewHandle.DangerousGetHandle();
                if (ptr == IntPtr.Zero) return;

                var buffer = new char[AnnounceMsgWCells];
                int n = Math.Min(chars.Length, AnnounceMsgWCells - 1);
                Array.Copy(chars, buffer, n);
                buffer[n] = '\0';

                var bytes = System.Text.Encoding.Unicode.GetBytes(buffer); // UTF-16LE
                Marshal.Copy(bytes, 0, IntPtr.Add(ptr, (int)offset), bytes.Length);
            }
            catch
            {
            }
        }

        private static void ClearWChars(long offset)
        {
            try
            {
                if (_view == null) return;
                IntPtr ptr = _view.SafeMemoryMappedViewHandle.DangerousGetHandle();
                if (ptr == IntPtr.Zero) return;
                var zero = new byte[AnnounceMsgWCells * 2];
                Marshal.Copy(zero, 0, IntPtr.Add(ptr, (int)offset), zero.Length);
            }
            catch
            {
            }
        }
    }
}
