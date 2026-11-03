using System.Text;
using System.Text.Json;

namespace DNF90.Downloader.Shared;

public static class DownloaderConfigFooter
{
    private const int FooterSize = 40;
    private static readonly byte[] Magic =
        Encoding.ASCII.GetBytes("DNF90DL1\0\0\0\0\0\0\0\0");
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web)
    {
        WriteIndented = true,
    };

    public static DownloaderConfig ReadCurrent()
    {
        var executablePath = Environment.ProcessPath
            ?? throw new InvalidOperationException("无法定位下载器程序。");
        return Read(executablePath);
    }

    public static DownloaderConfig Read(string executablePath)
    {
        using var stream = new FileStream(
            executablePath, FileMode.Open, FileAccess.Read, FileShare.ReadWrite);
        if (stream.Length < FooterSize)
        {
            throw new InvalidDataException("下载器配置缺失。");
        }

        stream.Seek(-FooterSize, SeekOrigin.End);
        using var reader = new BinaryReader(stream, Encoding.UTF8, leaveOpen: true);
        if (!reader.ReadBytes(Magic.Length).SequenceEqual(Magic))
        {
            throw new InvalidDataException("当前程序不是有效的 DNF 直链下载器。");
        }

        var format = reader.ReadInt32();
        _ = reader.ReadInt32();
        var offset = reader.ReadInt64();
        var length = reader.ReadInt64();
        if (format != 1 || offset < 0 || length <= 0 ||
            offset + length > stream.Length - FooterSize)
        {
            throw new InvalidDataException("下载器配置格式无效。");
        }

        stream.Position = offset;
        var bytes = reader.ReadBytes((int)length);
        if (bytes.Length != length)
        {
            throw new EndOfStreamException("下载器配置读取不完整。");
        }
        return JsonSerializer.Deserialize<DownloaderConfig>(bytes, JsonOptions)
            ?? throw new InvalidDataException("下载器配置为空。");
    }

    public static void Generate(
        string templatePath,
        string outputPath,
        DownloaderConfig config)
    {
        var json = JsonSerializer.SerializeToUtf8Bytes(config, JsonOptions);
        var tempPath = outputPath + ".building";
        File.Copy(templatePath, tempPath, overwrite: true);
        using var output = new FileStream(
            tempPath, FileMode.Append, FileAccess.Write, FileShare.None);
        var offset = output.Length;
        output.Write(json);
        using (var writer = new BinaryWriter(output, Encoding.UTF8, leaveOpen: true))
        {
            writer.Write(Magic);
            writer.Write(1);
            writer.Write(0);
            writer.Write(offset);
            writer.Write((long)json.Length);
            writer.Flush();
        }
        output.Flush(flushToDisk: true);
        output.Dispose();

        try
        {
            File.Move(tempPath, outputPath, overwrite: true);
        }
        catch (IOException ex)
        {
            throw new IOException("无法写入下载器文件，请关闭正在运行的旧下载器后重试。", ex);
        }
        finally
        {
            try
            {
                if (File.Exists(tempPath))
                {
                    File.Delete(tempPath);
                }
            }
            catch
            {
                // 临时文件清理失败不影响已生成文件。
            }
        }
    }
}
