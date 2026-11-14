using System.Text;

namespace DNF90.Setup.Shared;

internal static class ZipEntryFilenameDecoder
{
    private const uint EndOfCentralDirectorySignature = 0x06054b50;
    private const uint Zip64EndOfCentralDirectorySignature = 0x06064b50;
    private const uint Zip64LocatorSignature = 0x07064b50;
    private const uint CentralDirectoryHeaderSignature = 0x02014b50;
    private const ushort Utf8Flag = 0x0800;

    private static readonly UTF8Encoding StrictUtf8 = new(false, true);
    private static readonly Encoding Gbk;
    private static readonly Encoding Cp437;

    static ZipEntryFilenameDecoder()
    {
        Encoding.RegisterProvider(CodePagesEncodingProvider.Instance);
        Gbk = Encoding.GetEncoding(
            936,
            EncoderFallback.ExceptionFallback,
            DecoderFallback.ExceptionFallback);
        Cp437 = Encoding.GetEncoding(
            437,
            EncoderFallback.ExceptionFallback,
            DecoderFallback.ExceptionFallback);
    }

    public static IReadOnlyList<string> Read(Stream stream, int expectedCount)
    {
        if (!stream.CanSeek || expectedCount <= 0)
        {
            return Array.Empty<string>();
        }

        var originalPosition = stream.Position;
        try
        {
            var directory = FindCentralDirectory(stream);
            if (directory is null || directory.Value.EntryCount != expectedCount)
            {
                return Array.Empty<string>();
            }

            stream.Position = directory.Value.Offset;
            var names = new List<string>(expectedCount);
            for (var index = 0; index < expectedCount; index++)
            {
                var header = ReadExactly(stream, 46);
                if (ReadUInt32(header, 0) != CentralDirectoryHeaderSignature)
                {
                    return Array.Empty<string>();
                }

                var flags = ReadUInt16(header, 8);
                var nameLength = ReadUInt16(header, 28);
                var extraLength = ReadUInt16(header, 30);
                var commentLength = ReadUInt16(header, 32);
                var rawName = ReadExactly(stream, nameLength);
                names.Add(DecodeEntryName(rawName, flags));
                stream.Seek(extraLength + commentLength, SeekOrigin.Current);
            }
            return names;
        }
        catch (InvalidDataException)
        {
            return Array.Empty<string>();
        }
        catch (IOException)
        {
            return Array.Empty<string>();
        }
        finally
        {
            stream.Position = originalPosition;
        }
    }

    private static (int EntryCount, long Offset)? FindCentralDirectory(Stream stream)
    {
        var tailLength = (int)Math.Min(stream.Length, 65557);
        if (tailLength < 22)
        {
            return null;
        }

        stream.Position = stream.Length - tailLength;
        var tail = ReadExactly(stream, tailLength);
        var eocdIndex = FindSignatureBackward(tail, EndOfCentralDirectorySignature);
        if (eocdIndex < 0 || eocdIndex + 22 > tail.Length)
        {
            return null;
        }

        var entryCount = ReadUInt16(tail, eocdIndex + 10);
        var directorySize = ReadUInt32(tail, eocdIndex + 12);
        var directoryOffset = ReadUInt32(tail, eocdIndex + 16);
        if (entryCount != ushort.MaxValue &&
            directorySize != uint.MaxValue &&
            directoryOffset != uint.MaxValue)
        {
            return (entryCount, directoryOffset);
        }

        var eocdAbsoluteOffset = stream.Length - tailLength + eocdIndex;
        var locatorOffset = eocdAbsoluteOffset - 20;
        if (locatorOffset < 0)
        {
            return null;
        }

        stream.Position = locatorOffset;
        var locator = ReadExactly(stream, 20);
        if (ReadUInt32(locator, 0) != Zip64LocatorSignature)
        {
            return null;
        }

        var zip64Offset = checked((long)ReadUInt64(locator, 8));
        if (zip64Offset < 0 || zip64Offset + 56 > stream.Length)
        {
            return null;
        }

        stream.Position = zip64Offset;
        var zip64 = ReadExactly(stream, 56);
        if (ReadUInt32(zip64, 0) != Zip64EndOfCentralDirectorySignature)
        {
            return null;
        }

        var zip64EntryCount = checked((int)ReadUInt64(zip64, 32));
        var zip64DirectoryOffset = checked((long)ReadUInt64(zip64, 48));
        return (zip64EntryCount, zip64DirectoryOffset);
    }

    private static string DecodeEntryName(byte[] rawName, ushort flags)
    {
        if ((flags & Utf8Flag) != 0 && TryDecode(StrictUtf8, rawName, out var flaggedUtf8))
        {
            return flaggedUtf8;
        }

        if (TryDecode(StrictUtf8, rawName, out var utf8))
        {
            return utf8;
        }
        if (TryDecode(Gbk, rawName, out var gbk))
        {
            return gbk;
        }
        if (TryDecode(Cp437, rawName, out var cp437))
        {
            return cp437;
        }
        return Encoding.UTF8.GetString(rawName);
    }

    private static bool TryDecode(Encoding encoding, byte[] bytes, out string value)
    {
        try
        {
            value = encoding.GetString(bytes);
            return true;
        }
        catch (DecoderFallbackException)
        {
            value = "";
            return false;
        }
    }

    private static int FindSignatureBackward(byte[] bytes, uint signature)
    {
        for (var index = bytes.Length - 4; index >= 0; index--)
        {
            if (ReadUInt32(bytes, index) == signature)
            {
                return index;
            }
        }
        return -1;
    }

    private static byte[] ReadExactly(Stream stream, int count)
    {
        var buffer = new byte[count];
        var offset = 0;
        while (offset < count)
        {
            var read = stream.Read(buffer, offset, count - offset);
            if (read == 0)
            {
                throw new EndOfStreamException();
            }
            offset += read;
        }
        return buffer;
    }

    private static ushort ReadUInt16(byte[] bytes, int offset)
        => (ushort)(bytes[offset] | (bytes[offset + 1] << 8));

    private static uint ReadUInt32(byte[] bytes, int offset)
        => (uint)(bytes[offset] |
                  (bytes[offset + 1] << 8) |
                  (bytes[offset + 2] << 16) |
                  (bytes[offset + 3] << 24));

    private static ulong ReadUInt64(byte[] bytes, int offset)
        => ReadUInt32(bytes, offset) |
           ((ulong)ReadUInt32(bytes, offset + 4) << 32);
}
