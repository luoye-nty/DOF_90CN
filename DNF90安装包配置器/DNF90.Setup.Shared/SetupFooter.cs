using System.Text;

namespace DNF90.Setup.Shared;

public static class SetupFooter
{
    private const int FooterSize = 40;
    private static readonly byte[] Magic = Encoding.ASCII.GetBytes("DNF90SETUP2\0\0\0\0\0");

    public static SetupFooterInfo? TryRead(string executablePath)
    {
        try
        {
            using var stream = new FileStream(executablePath, FileMode.Open, FileAccess.Read, FileShare.ReadWrite);
            if (stream.Length < FooterSize)
            {
                return null;
            }

            stream.Seek(-FooterSize, SeekOrigin.End);
            using var reader = new BinaryReader(stream, Encoding.UTF8, leaveOpen: true);
            var magic = reader.ReadBytes(Magic.Length);
            if (!magic.SequenceEqual(Magic))
            {
                return null;
            }

            var format = reader.ReadInt32();
            var modeValue = reader.ReadInt32();
            var payloadOffset = reader.ReadInt64();
            var payloadLength = reader.ReadInt64();
            if (format != 1 || !Enum.IsDefined(typeof(SetupSourceMode), modeValue))
            {
                return null;
            }

            var mode = (SetupSourceMode)modeValue;
            if (payloadOffset < 0 || payloadLength <= 0 ||
                payloadOffset + payloadLength > stream.Length - FooterSize)
            {
                return null;
            }

            return new SetupFooterInfo(mode, payloadOffset, payloadLength, format);
        }
        catch
        {
            return null;
        }
    }

    public static void Append(Stream output, SetupSourceMode mode, long payloadOffset, long payloadLength)
    {
        using var writer = new BinaryWriter(output, Encoding.UTF8, leaveOpen: true);
        writer.Write(Magic);
        writer.Write(1);
        writer.Write((int)mode);
        writer.Write(payloadOffset);
        writer.Write(payloadLength);
        writer.Flush();
    }
}
