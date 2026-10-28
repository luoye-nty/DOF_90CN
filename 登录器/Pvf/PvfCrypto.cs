using System;
using System.IO;
using System.IO.Compression;
using System.Text;

namespace DNFLogin.Pvf;

/// <summary>
/// DNF PVF 归档解密与编码工具。
/// 自 Go 版 GM 工具 vendor/longheng.io/server/internal/platform/pvf/crypto.go、decode.go 逐行照抄移植。
/// </summary>
internal static class PvfCrypto
{
    /// <summary>旧版归档 header 的 0x55 guard，解密前先对 [24,28) 异或。</summary>
    public static void DecryptGuard(byte[] buf)
    {
        if (buf.Length < 28)
            return;
        for (int idx = 24; idx < 28; idx++)
            buf[idx] ^= 0x55;
    }

    /// <summary>旧版归档：magic=0x269EC3（header / GRPI / BodY）。</summary>
    public static void Decrypt(string key, byte[] buf) => DecryptCore(key, buf, 0x269EC3);

    /// <summary>旧版字符串池：magic=0x269EC9（sTrA / sTrW）。</summary>
    public static void Decrypt2(string key, byte[] buf) => DecryptCore(key, buf, 0x269EC9);

    /// <summary>新版 protected_nkpi：magic=0x269EC3（hEAd / grpi / bODy）。</summary>
    public static void DecryptProtected(string key, byte[] buf) => DecryptProtectedCore(key, buf, 0x269EC3);

    /// <summary>新版 protected_nkpi 字符串池：magic=0x269EC9（StRa / StRw）。</summary>
    public static void DecryptPString(string key, byte[] buf) => DecryptProtectedCore(key, buf, 0x269EC9);

    /// <summary>
    /// 旧版 DNF PVF 的 ANSI 字节密钥流解密。密钥 HeaD / sTrA / sTrW / GRPI / BodY。
    /// </summary>
    public static void DecryptCore(string key, byte[] buf, uint magic)
    {
        if (string.IsNullOrEmpty(key) || buf.Length == 0)
            return;
        byte[] k = Encoding.ASCII.GetBytes(key);
        if (k.Length < 4)
            return;

        int size = buf.Length;
        int tail = size;
        uint seed = 0x76826701u * (uint)k[0] +
                    0x1C1u * ((uint)k[3] + 0x1C1u * ((uint)k[2] + 0x1C1u * (uint)k[1]));

        if (size >= 4)
        {
            int quadCount = size >> 2;
            tail = size - (quadCount << 2);
            for (int idx = 0; idx < quadCount; idx++)
            {
                uint t1 = 0x343FD * seed + magic;
                seed = 0x343FD * t1 + magic;
                uint xorKey = ((seed >> 16) & 0xFFFF) + (t1 & 0xFFFF0000);
                int off = idx << 2;
                uint value = ReadUInt32(buf, off) ^ xorKey;
                WriteUInt32(buf, off, value);
            }
        }

        if (tail > 0)
        {
            uint t1 = 0x343FD * seed + magic;
            uint t2 = 0x343FD * t1 + magic;
            uint finalKey = (t1 & 0xFFFF0000) + ((t2 >> 16) & 0xFFFF);
            byte[] keyBytes = BitConverter.GetBytes(finalKey);
            int start = size - tail;
            for (int idx = 0; idx < tail; idx++)
                buf[start + idx] ^= keyBytes[idx];
        }
    }

    /// <summary>
    /// 新版 DNF PVF（23.4.15.0 之后 protected_nkpi）的 UTF-16 seed 密钥流解密。
    /// 密钥 hEAd / StRa / StRw / grpi / bODy。仅用于 protected_nkpi 归档的
    /// header、HASH、GRPI、字符串池和 body chunk 解密。
    /// </summary>
    public static void DecryptProtectedCore(string key, byte[] buf, uint magic)
    {
        if (string.IsNullOrEmpty(key) || buf.Length == 0)
            return;
        // Go: words := utf16.Encode([]rune(key))。DNF 密钥均为 BMP 内 ASCII 字符，
        // .NET string 内部即 UTF-16 code units，ToCharArray 与其等价。
        char[] words = key.ToCharArray();
        if (words.Length < 4)
            return;

        int size = buf.Length;
        int tail = size;
        uint seed = 0x339E9711u * (uint)words[0] +
                    0x393u * ((uint)words[3] + 0x393u * ((uint)words[2] + 0x393u * (uint)words[1]));

        if (size >= 4)
        {
            int quadCount = size >> 2;
            tail = size - (quadCount << 2);
            for (int idx = 0; idx < quadCount; idx++)
            {
                uint t1 = 0x343FD * seed + magic;
                seed = 0x343FD * t1 + magic;
                // Go: xorKey := (t1 & 0xFFFF0000) + (seed >> 16)
                uint xorKey = (t1 & 0xFFFF0000) + (seed >> 16);
                int off = idx << 2;
                uint value = ReadUInt32(buf, off) ^ xorKey;
                WriteUInt32(buf, off, value);
            }
        }

        if (tail > 0)
        {
            uint t1 = 0x343FD * seed + magic;
            uint t2 = 0x343FD * t1 + magic;
            uint finalKey = (t1 & 0xFFFF0000) + (t2 >> 16);
            byte[] keyBytes = BitConverter.GetBytes(finalKey);
            int start = size - tail;
            for (int idx = 0; idx < tail; idx++)
                buf[start + idx] ^= keyBytes[idx];
        }
    }

    /// <summary>zlib 解压（对应 Go zlib.NewReader + io.ReadAll）。</summary>
    public static byte[] ZlibBytes(byte[] data)
    {
        using var input = new MemoryStream(data, writable: false);
        using var zlib = new ZLibStream(input, CompressionMode.Decompress);
        using var output = new MemoryStream();
        zlib.CopyTo(output);
        return output.ToArray();
    }

    /// <summary>zlib 压缩（对应 Go zlib.NewWriter）。</summary>
    public static byte[] CompressZlib(byte[] data)
    {
        using var output = new MemoryStream();
        using (var zlib = new ZLibStream(output, CompressionLevel.Optimal, leaveOpen: true))
            zlib.Write(data, 0, data.Length);
        return output.ToArray();
    }

    /// <summary>
    /// 从缓冲区读取 NUL 结尾的字符串。优先按严格 UTF-8 解码；
    /// 失败则用 GB18030 兜底（对应 Go readUTF8String 的 simplifiedchinese.GB18030 分支）。
    /// </summary>
    public static string ReadUtf8String(byte[] buf, int start)
    {
        if (start < 0 || start >= buf.Length)
            return "";
        int end = Array.IndexOf(buf, (byte)0, start);
        if (end < 0)
            return "";
        byte[] raw = new byte[end - start];
        Array.Copy(buf, start, raw, 0, raw.Length);
        try
        {
            return new UTF8Encoding(encoderShouldEmitUTF8Identifier: false, throwOnInvalidBytes: true).GetString(raw);
        }
        catch (DecoderFallbackException)
        {
            try { return Encoding.GetEncoding("GB18030").GetString(raw); }
            catch { return Encoding.UTF8.GetString(raw); }
        }
    }

    /// <summary>从缓冲区读取 NUL(NUL) 结尾的 UTF-16LE 字符串。</summary>
    public static string ReadUtf16String(byte[] buf, int start)
    {
        if (start < 0 || start >= buf.Length)
            return "";
        int end = start;
        while (end + 1 < buf.Length)
        {
            if (buf[end] == 0 && buf[end + 1] == 0)
                break;
            end += 2;
        }
        if (end <= start)
            return "";
        byte[] raw = new byte[end - start];
        Array.Copy(buf, start, raw, 0, raw.Length);
        return DecodeUtf16LE(raw);
    }

    /// <summary>UTF-16LE 字节解码为 .NET string（对应 Go decodeUTF16LE）。</summary>
    public static string DecodeUtf16LE(byte[] data)
    {
        if (data.Length < 2)
            return "";
        int unitCount = data.Length / 2;
        char[] chars = new char[unitCount];
        for (int i = 0; i < unitCount; i++)
            chars[i] = (char)(data[i * 2] | (data[i * 2 + 1] << 8));
        return new string(chars);
    }

    public static uint ReadUInt32(byte[] data, int offset) =>
        (uint)(data[offset] | (data[offset + 1] << 8) | (data[offset + 2] << 16) | (data[offset + 3] << 24));

    public static int ReadInt32(byte[] data, int offset) =>
        unchecked((int)ReadUInt32(data, offset));

    public static void WriteUInt32(byte[] data, int offset, uint value)
    {
        data[offset] = (byte)value;
        data[offset + 1] = (byte)(value >> 8);
        data[offset + 2] = (byte)(value >> 16);
        data[offset + 3] = (byte)(value >> 24);
    }
}
