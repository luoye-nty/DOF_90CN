using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Security.Cryptography;
using System.Text;

namespace DNFLogin.Pvf;

/// <summary>PVF 归档格式。</summary>
public enum ArchiveFormat
{
    Unknown,
    /// <summary>旧版 DNF PVF `nkpi` 归档（ANSI 字节密钥流）。</summary>
    Nkpi,
    /// <summary>23.4.15.0 之后使用 UTF-16 seed 密钥流的 `protected_nkpi` 归档。</summary>
    ProtectedNkpi,
}

public class PvfException : Exception
{
    public PvfException(string message) : base(message) { }
    public PvfException(string message, Exception inner) : base(message, inner) { }
}

/// <summary>归档内的文件条目（元信息）。</summary>
public sealed class PvfFile
{
    public int Index { get; set; }
    public string Path { get; set; } = "";
    public string Name { get; set; } = "";
    public string ArchivePath { get; set; } = "";
    public int DataType { get; set; }
    public int Size { get; set; }
}

internal sealed class FileItem
{
    public int NameOffset;
    public int PathOffset;
    public int ChunkIndex;
    public int DataOffset;
    public int DataSize;
    public int DataType;
}

internal sealed class GroupItem
{
    public int CompressedSize;
    public int OriginalSize;
}

/// <summary>解密后的 0x30 字节 PVF 头关键字段。</summary>
internal sealed class PvfHeader
{
    public int FileCount;
    public int BodySize;
    public int GroupCount;
    public int HashSize;
    public int NameSize;
}

/// <summary>
/// DNF PVF 归档读取器：启动期把 PVF 读入内存，支持旧版 nkpi 与新版 protected_nkpi
/// 自动探测，提供按路径读取脚本文本 / 原始字节的能力。
/// 自 Go 版 vendor/longheng.io/server/internal/platform/pvf 照抄移植
/// （archive.go / parse.go / read.go / path.go / cache.go / flight.go 的语义）。
/// </summary>
public sealed class PvfArchive : IPvfSource
{
    private const int HeaderSize = 0x30;
    private const int FileItemSize = 0x18;
    private const int GroupItemSize = 8;
    private const uint MagicSignature = 0x69706B6E; // 'nkpi'

    private const long DefaultChunkCacheBytes = 256L * 1024 * 1024;
    private const long DefaultTextCacheBytes = 256L * 1024 * 1024;

    public ArchiveFormat Format { get; private set; } = ArchiveFormat.Unknown;
    public string SourcePath { get; private set; } = "";
    public long SourceSize { get; private set; }
    public string Checksum { get; private set; } = "";
    public int FileCount => _files.Count;
    public int GroupCount => _groups.Count;
    public int CachedChunks => _chunks.Count;
    public int CachedTexts => _texts.Count;

    private byte[] _data = Array.Empty<byte>();
    private List<PvfFile> _files = new();
    private List<FileItem> _items = new();
    private List<GroupItem> _groups = new();
    private Dictionary<string, int> _pathIdx = new();
    private int _bodyOff;
    private byte[] _strA = Array.Empty<byte>();
    private byte[] _strW = Array.Empty<byte>();
    private readonly ByteLruCache<byte[]> _chunks;
    private readonly ByteLruCache<string> _texts;
    private readonly object _lock = new();

    private PvfArchive(byte[] data, string path, long size, string checksum,
                       long chunkCacheBytes, long textCacheBytes)
    {
        _data = data;
        SourcePath = path;
        SourceSize = size;
        Checksum = checksum;
        _chunks = new ByteLruCache<byte[]>(chunkCacheBytes > 0 ? chunkCacheBytes : DefaultChunkCacheBytes);
        _texts = new ByteLruCache<string>(textCacheBytes > 0 ? textCacheBytes : DefaultTextCacheBytes);
        Parse();
    }

    /// <summary>从磁盘加载 PVF（整文件读入内存，后续不再访问磁盘）。</summary>
    public static PvfArchive Open(string path)
    {
        if (string.IsNullOrWhiteSpace(path))
            throw new PvfException("pvf path is required");
        var info = new FileInfo(path);
        if (!info.Exists)
            throw new PvfException($"stat pvf: {path}");
        byte[] data = File.ReadAllBytes(path);
        string sum = Convert.ToHexString(SHA256.HashData(data)).ToLowerInvariant();
        return new PvfArchive(data, path, data.Length, sum, 0, 0);
    }

    /// <summary>从内存字节加载 PVF（自动复制，不持有调用方缓冲区）。</summary>
    public static PvfArchive OpenBytes(byte[] data)
    {
        if (data == null || data.Length == 0)
            throw new PvfException("empty pvf data");
        byte[] copied = (byte[])data.Clone();
        string sum = Convert.ToHexString(SHA256.HashData(copied)).ToLowerInvariant();
        return new PvfArchive(copied, "", copied.Length, sum, 0, 0);
    }

    /// <summary>当前格式字符串，供兼容排查使用。</summary>
    public string FormatName => Format switch
    {
        ArchiveFormat.Nkpi => "nkpi",
        ArchiveFormat.ProtectedNkpi => "protected_nkpi",
        _ => "",
    };

    /// <summary>归档内全部文件元信息副本。</summary>
    public List<PvfFile> Files()
    {
        lock (_lock)
            return new List<PvfFile>(_files);
    }

    /// <summary>按归一化路径查找文件。</summary>
    public PvfFile? FindFile(string relativePath)
    {
        lock (_lock)
        {
            if (!_pathIdx.TryGetValue(PathKey(relativePath), out int idx))
                return null;
            return _files[idx];
        }
    }

    /// <summary>按路径读取脚本文本（dataType=1 解码为脚本，dataType=3 按 UTF-16LE）。</summary>
    public string ReadText(string relativePath)
    {
        lock (_lock)
        {
            if (!_pathIdx.TryGetValue(PathKey(relativePath), out int idx))
                throw new PvfException($"pvf file not found: {relativePath}");
            return ReadTextIndexLocked(idx);
        }
    }

    /// <summary>按路径读取原始文件字节。</summary>
    public byte[] ReadRaw(string relativePath)
    {
        lock (_lock)
        {
            if (!_pathIdx.TryGetValue(PathKey(relativePath), out int idx))
                throw new PvfException($"pvf file not found: {relativePath}");
            return ReadRawIndexLocked(idx);
        }
    }

    /// <summary>按文件表下标读取脚本文本。</summary>
    public string FileText(int idx) => ReadTextIndexLocked(idx);

    /// <summary>按文件表下标读取原始字节。</summary>
    public byte[] FileRaw(int idx) => ReadRawIndexLocked(idx);

    /// <summary>
    /// 解析脚本 token 的字符串池值。type-1 脚本中标签与引号值都是共享字符串池的
    /// 编码偏移（magic offset），调用方必须保持该偏移不变，除非重建所有引用脚本的 token。
    /// </summary>
    public string ResolveString(int magicOffset)
    {
        if (magicOffset < 0)
            return "";
        if ((magicOffset & 1) != 0)
            return PvfCrypto.ReadUtf16String(_strW, (magicOffset >> 1) * 2);
        return PvfCrypto.ReadUtf8String(_strA, magicOffset >> 1);
    }

    /// <summary>
    /// 为请求的共享字符串值定位本归档中现存的 token offset。
    /// 供 type-1 脚本安全移植使用：外部 PVF 的原始 token 偏移是归档局部的，
    /// 复用前必须先按值解析。
    /// </summary>
    public Dictionary<string, int> FindStringMagicOffsets(List<string> values)
    {
        var result = new Dictionary<string, int>();
        if (values == null || values.Count == 0)
            return result;
        var wanted = new HashSet<string>(values.Where(v => !string.IsNullOrEmpty(v)));

        int start = 0;
        while (start < _strA.Length && result.Count < wanted.Count)
        {
            int end = Array.IndexOf(_strA, (byte)0, start);
            if (end < 0)
                break;
            if (end > start)
            {
                string value = PvfCrypto.ReadUtf8String(_strA, start);
                if (wanted.Contains(value) && !result.ContainsKey(value))
                    result[value] = start << 1;
            }
            start = end + 1;
        }
        start = 0;
        while (start + 1 < _strW.Length && result.Count < wanted.Count)
        {
            int end = start;
            while (end + 1 < _strW.Length && !(_strW[end] == 0 && _strW[end + 1] == 0))
                end += 2;
            if (end > start)
            {
                string value = PvfCrypto.ReadUtf16String(_strW, start);
                if (wanted.Contains(value) && !result.ContainsKey(value))
                    result[value] = ((start / 2) << 1) | 1;
            }
            start = end + 2;
        }
        return result;
    }

    /// <summary>type-1 脚本原始字节解码为可读文本（照抄 decode.go decodeScript）。</summary>
    public string DecodeScript(byte[] raw)
    {
        int lineCount = raw.Length / 5;
        if (lineCount == 0)
            return "";
        var b = new StringBuilder(raw.Length * 2);
        for (int idx = 0; idx < lineCount; idx++)
        {
            int off = idx * 5;
            byte tokenType = raw[off];
            int value = PvfCrypto.ReadInt32(raw, off + 1);
            switch (tokenType)
            {
                case 0:
                    b.Append(value).Append(' ');
                    break;
                case 2:
                    b.Append(BitConverter.Int32BitsToSingle(value).ToString("F2")).Append(' ');
                    break;
                case 3:
                    b.Append('\n').Append(ResolveString(value)).Append('\n');
                    break;
                case 5:
                    b.Append("\n{5=``}");
                    break;
                case 6:
                    b.Append('`').Append(ResolveString(value)).Append("` ");
                    break;
                case 7:
                    b.Append("\n{7=``}");
                    break;
            }
        }
        return b.ToString();
    }

    // ---------- 解析核心（照抄 parse.go） ----------

    private void Parse()
    {
        if (_data.Length < HeaderSize)
            throw new PvfException("pvf archive is invalid: header too short");
        if (TryParseNkpi())
            return;
        if (TryParseProtectedNkpi())
            return;
        throw new PvfException("pvf archive is invalid: unsupported archive format");
    }

    private bool TryParseNkpi()
    {
        ResetParseState();
        byte[] header = new byte[HeaderSize];
        Array.Copy(_data, header, HeaderSize);
        // 注：Go 版在 parseNKPI 中调用 decryptGuard(header)（对 [24,28) 异或 0x55），
        // 实测 S4A21 旧版 nkpi 经 guard 后 fileCount 会错乱导致打不开；
        // 旧版 PvfLib 打开时亦不调用 DecryptGuard，故此处不做 guard。
        PvfCrypto.Decrypt("HeaD", header);
        if (PvfCrypto.ReadUInt32(header, 0) != MagicSignature)
            return false;
        var pvfHeader = ParseHeaderSizes(header);
        if (!ValidateHeader(pvfHeader))
            return false;
        return TryParseNkpiTables(pvfHeader, false);
    }

    private bool TryParseProtectedNkpi()
    {
        ResetParseState();
        byte[] header = new byte[HeaderSize];
        Array.Copy(_data, header, HeaderSize);
        PvfCrypto.DecryptProtected("hEAd", header);
        if (PvfCrypto.ReadUInt32(header, 0) != MagicSignature)
            return false;
        var pvfHeader = ParseHeaderSizes(header);
        if (!ValidateHeader(pvfHeader))
            return false;
        return TryParseNkpiTables(pvfHeader, true);
    }

    private bool TryParseNkpiTables(PvfHeader header, bool protectedMode)
    {
        int pos = HeaderSize;
        long tableSize = CheckedMul(header.FileCount, FileItemSize);
        if (tableSize < 0)
            return false;
        pos += (int)tableSize;
        pos += header.HashSize;
        int nameOffset = pos;
        pos += header.NameSize;
        int groupOffset = pos;
        long groupSize = CheckedMul(header.GroupCount, GroupItemSize);
        if (groupSize < 0)
            return false;
        pos += (int)groupSize;
        if (pos < 0 || pos > _data.Length || header.BodySize > _data.Length - pos)
            return false;
        _bodyOff = pos;

        byte[] nameBytes = new byte[header.NameSize];
        Array.Copy(_data, nameOffset, nameBytes, 0, header.NameSize);
        if (protectedMode)
        {
            if (!TryBuildPStringBuffers(nameBytes))
                return false;
            Format = ArchiveFormat.ProtectedNkpi;
        }
        else
        {
            BuildStringBuffers(nameBytes);
            Format = ArchiveFormat.Nkpi;
        }

        byte[] groupBytes = new byte[(int)groupSize];
        Array.Copy(_data, groupOffset, groupBytes, 0, (int)groupSize);
        if (protectedMode)
            PvfCrypto.DecryptProtected("grpi", groupBytes);
        else
            PvfCrypto.Decrypt("GRPI", groupBytes);
        if (!TryParseGroups(groupBytes, header.GroupCount))
            return false;
        if (!TryParseFiles(tableOffset: HeaderSize, header.FileCount))
            return false;
        return true;
    }

    private void ResetParseState()
    {
        Format = ArchiveFormat.Unknown;
        _files = new List<PvfFile>();
        _items = new List<FileItem>();
        _groups = new List<GroupItem>();
        _pathIdx = new Dictionary<string, int>();
        _bodyOff = 0;
        _strA = Array.Empty<byte>();
        _strW = Array.Empty<byte>();
    }

    private PvfHeader ParseHeaderSizes(byte[] header) => new()
    {
        FileCount = PvfCrypto.ReadInt32(header, 24),
        BodySize = PvfCrypto.ReadInt32(header, 32),
        GroupCount = PvfCrypto.ReadInt32(header, 36),
        HashSize = PvfCrypto.ReadInt32(header, 40),
        NameSize = PvfCrypto.ReadInt32(header, 44),
    };

    private bool ValidateHeader(PvfHeader header) =>
        header.FileCount >= 0 && header.BodySize >= 0 && header.GroupCount >= 0 &&
        header.HashSize >= 0 && header.NameSize >= 0;

    private bool TryParseGroups(byte[] data, int count)
    {
        _groups = new List<GroupItem>(count);
        for (int idx = 0; idx < count; idx++)
        {
            int off = idx * GroupItemSize;
            int compressedSize = PvfCrypto.ReadInt32(data, off);
            int originalSize = PvfCrypto.ReadInt32(data, off + 4);
            if (compressedSize < 0 || originalSize < 0)
                return false;
            _groups.Add(new GroupItem { CompressedSize = compressedSize, OriginalSize = originalSize });
        }
        return true;
    }

    private bool TryParseFiles(int tableOffset, int count)
    {
        _items = new List<FileItem>(count);
        _files = new List<PvfFile>(count);
        for (int idx = 0; idx < count; idx++)
        {
            int off = tableOffset + idx * FileItemSize;
            if (off + FileItemSize > _data.Length)
                return false;
            var item = new FileItem
            {
                NameOffset = PvfCrypto.ReadInt32(_data, off),
                PathOffset = PvfCrypto.ReadInt32(_data, off + 4),
                ChunkIndex = PvfCrypto.ReadInt32(_data, off + 8),
                DataOffset = PvfCrypto.ReadInt32(_data, off + 12),
                DataSize = PvfCrypto.ReadInt32(_data, off + 16),
                DataType = PvfCrypto.ReadInt32(_data, off + 20),
            };
            string name = ResolveString(item.NameOffset);
            string dir = ResolveString(item.PathOffset);
            string archivePath = JoinArchivePath(dir, name);
            _items.Add(item);
            _files.Add(new PvfFile
            {
                Index = idx,
                Path = dir,
                Name = name,
                ArchivePath = archivePath,
                DataType = item.DataType,
                Size = item.DataSize,
            });
            if (archivePath.Length > 0)
            {
                string key = PathKey(archivePath);
                if (!_pathIdx.ContainsKey(key))
                    _pathIdx[key] = idx;
            }
        }
        return true;
    }

    private void BuildStringBuffers(byte[] nameBytes)
    {
        if (nameBytes.Length < 16)
            return;
        int idx = 8;
        _strA = DecryptStringBuffer(nameBytes, ref idx, "sTrA", 0xAA74472E)!;
        _strW = DecryptStringBuffer(nameBytes, ref idx, "sTrW", 0x9A82F037)!;
    }

    private bool TryBuildPStringBuffers(byte[] nameBytes)
    {
        if (nameBytes.Length < 16)
            return true;
        int idx = 8;
        byte[]? strA = DecryptPStringBuffer(nameBytes, ref idx, "StRa", 0xAA74472E);
        if (strA == null)
            return false;
        byte[]? strW = DecryptPStringBuffer(nameBytes, ref idx, "StRw", 0x9A82F037);
        if (strW == null)
            return false;
        _strA = strA!;
        _strW = strW!;
        return true;
    }

    private byte[]? DecryptStringBuffer(byte[] data, ref int idx, string key, uint xorConst)
    {
        if (idx + 8 > data.Length)
            return null;
        uint cnt1 = PvfCrypto.ReadUInt32(data, idx);
        idx += 8; // cnt1 + 4 字节保留
        int encSize = (int)(cnt1 ^ xorConst);
        if (encSize <= 0 || idx + encSize > data.Length)
            return null;
        byte[] encrypted = new byte[encSize];
        Array.Copy(data, idx, encrypted, 0, encSize);
        idx += encSize;
        PvfCrypto.Decrypt2(key, encrypted);
        try { return PvfCrypto.ZlibBytes(encrypted); }
        catch { return null; }
    }

    private byte[]? DecryptPStringBuffer(byte[] data, ref int idx, string key, uint xorConst)
    {
        if (idx + 8 > data.Length)
            return null;
        uint encodedSize = PvfCrypto.ReadUInt32(data, idx);
        uint encodedOriginalSize = PvfCrypto.ReadUInt32(data, idx + 4);
        idx += 8;
        int encSize = (int)(encodedSize ^ xorConst);
        if (encSize <= 0 || idx + encSize > data.Length)
            return null;
        byte[] encrypted = new byte[encSize];
        Array.Copy(data, idx, encrypted, 0, encSize);
        idx += encSize;
        PvfCrypto.DecryptPString(key, encrypted);
        byte[] output;
        try { output = PvfCrypto.ZlibBytes(encrypted); }
        catch { return null; }
        // 新版字符串池头多 encodedOriginalSize 校验（Go: want = encodedOriginalSize ^ encSize）
        int want = (int)(encodedOriginalSize ^ (uint)encSize);
        if (want >= 0 && want != output.Length)
            return null;
        return output;
    }

    // ---------- 读取核心（照抄 read.go） ----------

    private string ReadTextIndexLocked(int idx)
    {
        if (_texts.TryGet(idx, out string? cached))
            return cached;
        byte[] raw = ReadRawIndexLocked(idx);
        string text;
        switch (_items[idx].DataType)
        {
            case 1:
                text = DecodeScript(raw);
                break;
            case 3:
                text = PvfCrypto.DecodeUtf16LE(raw);
                break;
            default:
                text = "";
                break;
        }
        _texts.Add(idx, text, text.Length);
        return text;
    }

    private byte[] ReadRawIndexLocked(int idx)
    {
        if (idx < 0 || idx >= _items.Count)
            throw new PvfException($"pvf file not found: index {idx}");
        var item = _items[idx];
        byte[] chunk = ChunkLocked(item.ChunkIndex);
        if (item.DataOffset < 0 || item.DataSize < 0 || item.DataOffset + item.DataSize > chunk.Length)
            throw new PvfException($"pvf archive is invalid: file {idx} data range is invalid");
        byte[] outBytes = new byte[item.DataSize];
        Array.Copy(chunk, item.DataOffset, outBytes, 0, item.DataSize);
        return outBytes;
    }

    private byte[] ChunkLocked(int idx)
    {
        if (idx < 0 || idx >= _groups.Count)
            throw new PvfException($"pvf archive is invalid: chunk {idx} is out of range");
        if (_chunks.TryGet(idx, out byte[]? cached))
            return cached;
        int prev = 0;
        if (idx > 0)
            prev = _groups[idx - 1].CompressedSize;
        int curr = _groups[idx].CompressedSize;
        if (curr <= prev)
            throw new PvfException($"pvf archive is invalid: chunk {idx} compressed range is invalid");
        int start = _bodyOff + prev;
        int size = curr - prev;
        if (start < 0 || size <= 0 || start + size > _data.Length)
            throw new PvfException($"pvf archive is invalid: chunk {idx} exceeds archive");
        byte[] encrypted = new byte[size];
        Array.Copy(_data, start, encrypted, 0, size);
        switch (Format)
        {
            case ArchiveFormat.ProtectedNkpi:
                PvfCrypto.DecryptProtected("bODy", encrypted);
                break;
            default:
                PvfCrypto.Decrypt("BodY", encrypted);
                break;
        }
        byte[] chunk;
        try { chunk = PvfCrypto.ZlibBytes(encrypted); }
        catch (Exception ex) { throw new PvfException($"pvf archive is invalid: chunk {idx} decompress: {ex.Message}", ex); }
        if (_groups[idx].OriginalSize >= 0 && _groups[idx].OriginalSize != chunk.Length)
            throw new PvfException($"pvf archive is invalid: chunk {idx} original size mismatch: want {_groups[idx].OriginalSize} got {chunk.Length}");
        _chunks.Add(idx, chunk, chunk.Length);
        return chunk;
    }

    // ---------- 路径工具（照抄 path.go） ----------

    internal static string NormalizePath(string value)
    {
        value = value.Trim().Replace('\\', '/');
        while (value.StartsWith("./") || value.StartsWith("/"))
        {
            if (value.StartsWith("./"))
                value = value.Substring(2);
            else
                value = value.Substring(1);
        }
        if (value.Length == 0)
            return "";
        string clean = CleanPath(value).TrimEnd('/');
        if (clean == ".")
            return "";
        return clean;
    }

    internal static string PathKey(string value) => NormalizePath(value).ToLowerInvariant();

    private static string JoinArchivePath(string dir, string name)
    {
        if (string.IsNullOrWhiteSpace(dir))
            return NormalizePath(name);
        if (string.IsNullOrWhiteSpace(name))
            return NormalizePath(dir);
        return NormalizePath(dir + "/" + name);
    }

    /// <summary>等价 Go path.Clean（虚拟 PVF 路径，不做真实文件系统访问）。</summary>
    internal static string CleanPath(string value)
    {
        string path = value.Replace('\\', '/');
        if (path.Length == 0)
            return ".";
        bool rooted = path[0] == '/';
        string[] parts = path.Split('/');
        var stack = new List<string>(parts.Length);
        foreach (string part in parts)
        {
            switch (part)
            {
                case "":
                case ".":
                    continue;
                case "..":
                    if (stack.Count > 0 && stack[^1] != "..")
                        stack.RemoveAt(stack.Count - 1);
                    else if (!rooted)
                        stack.Add("..");
                    break;
                default:
                    stack.Add(part);
                    break;
            }
        }
        string result = string.Join("/", stack);
        if (rooted)
            result = "/" + result;
        if (result.Length == 0)
            return ".";
        return result;
    }

    private static long CheckedMul(int left, int right)
    {
        if (left < 0 || right < 0)
            return -1;
        return (long)left * right;
    }
}

/// <summary>按驻留字节数约束的简单 LRU 缓存（对应 Go byteLRU，去除并发统计）。</summary>
internal sealed class ByteLruCache<T>
{
    private readonly long _limit;
    private long _bytes;
    private readonly Dictionary<int, LinkedListNode<(int Key, T Value, long Bytes)>> _map = new();
    private readonly LinkedList<(int Key, T Value, long Bytes)> _order = new();

    public ByteLruCache(long limit)
    {
        _limit = limit > 0 ? limit : 1;
    }

    public int Count => _map.Count;

    public bool TryGet(int key, out T value)
    {
        if (_map.TryGetValue(key, out var node))
        {
            _order.Remove(node);
            _order.AddFirst(node);
            value = node.Value.Value;
            return true;
        }
        value = default!;
        return false;
    }

    public void Add(int key, T value, long size)
    {
        if (value == null || size <= 0)
            return;
        if (_map.TryGetValue(key, out var existing))
        {
            var entry = existing.Value;
            _bytes -= entry.Bytes;
            var updated = (entry.Key, value, size);
            existing.Value = updated;
            _bytes += size;
            _order.Remove(existing);
            _order.AddFirst(existing);
        }
        else
        {
            var node = _order.AddFirst((key, value, size));
            _map[key] = node;
            _bytes += size;
        }
        while (_bytes > _limit && _order.Count > 0)
        {
            var oldest = _order.Last!;
            _map.Remove(oldest.Value.Key);
            _order.RemoveLast();
            _bytes -= oldest.Value.Bytes;
        }
    }
}
