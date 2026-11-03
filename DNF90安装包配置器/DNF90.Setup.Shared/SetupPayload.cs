using System.Text;
using System.Text.Json;

namespace DNF90.Setup.Shared;

public sealed class SetupPayload : IDisposable
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);
    private readonly FileStream? _embeddedStream;
    private readonly string? _packagesDirectory;
    private readonly Dictionary<SetupPackageInfo, long> _embeddedOffsets = new();

    private SetupPayload(
        SetupFooterInfo footer,
        SetupManifest manifest,
        FileStream? embeddedStream,
        string? packagesDirectory)
    {
        Footer = footer;
        Manifest = manifest;
        _embeddedStream = embeddedStream;
        _packagesDirectory = packagesDirectory;
    }

    public SetupFooterInfo Footer { get; }
    public SetupManifest Manifest { get; }
    public IReadOnlyList<SetupPackageInfo> Packages => Manifest.Packages;

    public static SetupPayload Open(string setupPath)
    {
        var footer = SetupFooter.TryRead(setupPath)
            ?? throw new InvalidOperationException("当前 EXE 不是有效的 DNF90 安装程序。");

        if (footer.Mode == SetupSourceMode.Embedded)
        {
            var stream = new FileStream(
                setupPath, FileMode.Open, FileAccess.Read, FileShare.Read, 1024 * 1024,
                FileOptions.RandomAccess);
            try
            {
                stream.Position = footer.PayloadOffset;
                using var reader = new BinaryReader(stream, Encoding.UTF8, leaveOpen: true);
                var manifestLength = reader.ReadInt64();
                if (manifestLength <= 0 || manifestLength > 16 * 1024 * 1024)
                {
                    throw new InvalidDataException("安装器清单长度异常。");
                }

                var manifestStart = footer.PayloadOffset + sizeof(long);
                if (manifestStart + manifestLength > footer.PayloadOffset + footer.PayloadLength)
                {
                    throw new InvalidDataException("安装器清单超出有效负载范围。");
                }

                var manifestBytes = reader.ReadBytes((int)manifestLength);
                if (manifestBytes.Length != manifestLength)
                {
                    throw new EndOfStreamException("安装器清单读取不完整。");
                }

                var manifest = JsonSerializer.Deserialize<SetupManifest>(manifestBytes, JsonOptions)
                    ?? throw new InvalidDataException("安装器清单为空。");
                ValidateManifest(manifest);

                var payload = new SetupPayload(footer, manifest, stream, null);
                var cursor = manifestStart + manifestLength;
                var payloadEnd = footer.PayloadOffset + footer.PayloadLength;
                foreach (var package in manifest.Packages)
                {
                    if (package.CompressedBytes <= 0 || cursor + package.CompressedBytes > payloadEnd)
                    {
                        throw new InvalidDataException("安装包分区超出有效负载范围：" + package.FileName);
                    }
                    payload._embeddedOffsets[package] = cursor;
                    cursor += package.CompressedBytes;
                }
                if (cursor != payloadEnd)
                {
                    throw new InvalidDataException("安装器负载长度与清单不一致。");
                }
                return payload;
            }
            catch
            {
                stream.Dispose();
                throw;
            }
        }

        SetupManifest externalManifest;
        using (var stream = new FileStream(
                   setupPath, FileMode.Open, FileAccess.Read, FileShare.Read, 1024 * 1024,
                   FileOptions.RandomAccess))
        {
            stream.Position = footer.PayloadOffset;
            using var reader = new BinaryReader(stream, Encoding.UTF8, leaveOpen: true);
            var manifestLength = reader.ReadInt64();
            if (manifestLength <= 0 || manifestLength > 16 * 1024 * 1024 ||
                footer.PayloadOffset + sizeof(long) + manifestLength >
                footer.PayloadOffset + footer.PayloadLength)
            {
                throw new InvalidDataException("分卷安装清单长度异常。");
            }
            var manifestBytes = reader.ReadBytes((int)manifestLength);
            externalManifest = JsonSerializer.Deserialize<SetupManifest>(manifestBytes, JsonOptions)
                ?? throw new InvalidDataException("分卷安装清单为空。");
        }
        ValidateManifest(externalManifest);

        var packagesDirectory = ResolvePackagesDirectory(setupPath);
        foreach (var package in externalManifest.Packages)
        {
            var filePath = Path.Combine(packagesDirectory, Path.GetFileName(package.FileName));
            if (!File.Exists(filePath))
            {
                throw new FileNotFoundException("缺少安装分卷：" + package.FileName, filePath);
            }
            if (new FileInfo(filePath).Length != package.CompressedBytes)
            {
                throw new InvalidDataException("安装分卷大小不正确：" + package.FileName);
            }
        }

        return new SetupPayload(footer, externalManifest, null, packagesDirectory);
    }

    public Stream OpenPackageStream(SetupPackageInfo package)
    {
        if (_embeddedStream is not null)
        {
            if (!_embeddedOffsets.TryGetValue(package, out var offset))
            {
                throw new InvalidOperationException("安装包不在内嵌负载索引中：" + package.FileName);
            }
            return new SubStream(_embeddedStream, offset, package.CompressedBytes);
        }

        if (string.IsNullOrWhiteSpace(_packagesDirectory))
        {
            throw new InvalidOperationException("未找到分卷安装目录。");
        }

        return new FileStream(
            Path.Combine(_packagesDirectory, Path.GetFileName(package.FileName)),
            FileMode.Open, FileAccess.Read, FileShare.Read, 1024 * 1024,
            FileOptions.Asynchronous | FileOptions.SequentialScan);
    }

    private static string ResolvePackagesDirectory(string setupPath)
    {
        return Path.GetDirectoryName(Path.GetFullPath(setupPath))
            ?? throw new InvalidOperationException("无法确定安装器所在目录。");
    }

    private static void ValidateManifest(SetupManifest manifest)
    {
        if (!string.Equals(manifest.PayloadFormat, "dnf90-setup/1", StringComparison.Ordinal))
        {
            throw new InvalidDataException("不支持的安装包格式：" + manifest.PayloadFormat);
        }
        if (string.IsNullOrWhiteSpace(manifest.Version) || manifest.Packages.Count == 0)
        {
            throw new InvalidDataException("安装清单缺少版本号或文件分区。");
        }
        if (manifest.ReleaseContent == ReleaseContent.Full &&
            !manifest.Packages.Any(x =>
                string.Equals(x.Id, "remaining-files", StringComparison.OrdinalIgnoreCase)))
        {
            throw new InvalidDataException("全量安装清单缺少登录器与剩余配件分区。");
        }
    }

    public void Dispose()
    {
        _embeddedStream?.Dispose();
    }
}
