using System.Security.Cryptography;
using System.Text;
using DNF90.Setup.Shared;

if (args.Length < 1)
{
    Console.Error.WriteLine("Usage: DNF90.Setup.SmokeTest <installer-template.exe>");
    return 2;
}

var template = Path.GetFullPath(args[0]);
if (!File.Exists(template))
{
    Console.Error.WriteLine("Installer template not found: " + template);
    return 2;
}

var work = Path.Combine(Path.GetTempPath(), "DNF90SetupSmoke-" + Guid.NewGuid().ToString("N"));
var client = Path.Combine(work, "client");
var output = Path.Combine(work, "output");
var installedSplit = Path.Combine(work, "installed-split");

try
{
    Directory.CreateDirectory(work);
    CreateFile(Path.Combine(client, "Script.pvf"), 1024 * 1024, 1);
    var launcherPath = Path.Combine(client, "自定义登录器.exe");
    CreateFile(launcherPath, 64 * 1024, 2);
    CreateFile(Path.Combine(client, "90CN.dll"), 128 * 1024, 3);
    CreateFile(Path.Combine(client, "DNF.exe"), 48 * 1024, 4);
    CreateFile(Path.Combine(client, "config", "settings.ini"), 4096, 5);
    CreateFile(Path.Combine(client, "ImagePacks2", "large-a.npk"), 65L * 1024 * 1024, 6);
    CreateFile(Path.Combine(client, "ImagePacks2", "nested", "small-b.npk"), 2 * 1024 * 1024, 7);
    CreateFile(Path.Combine(client, ".dnf90-install-manifest.json"), 128, 8);

    var analysis = ClientPackager.Analyze(client);
    Assert(analysis.ScriptFiles.Count == 1, "Script.pvf was not isolated");
    Assert(analysis.Cn90Files.Count == 1, "90CN.dll was not isolated");
    Assert(analysis.ImagePackFiles.Count == 2, "ImagePacks2 file count mismatch");
    Assert(analysis.OtherFiles.All(x =>
        !x.RelativePath.EndsWith(".dnf90-install-manifest.json", StringComparison.OrdinalIgnoreCase)),
        "Install manifest should not enter remaining-files");
    Assert(
        string.Equals(
            InstallPathHelper.NormalizeTarget(@"D:\"),
            @"D:\地下城与勇士",
            StringComparison.OrdinalIgnoreCase),
        "Drive root must normalize to the 地下城与勇士 subdirectory");
    Assert(
        string.Equals(
            InstallPathHelper.NormalizeTarget(@"D:\Games"),
            @"D:\Games",
            StringComparison.OrdinalIgnoreCase),
        "A normal selected folder must not gain another subdirectory");
    Assert(
        string.Equals(
            InstallPathHelper.NormalizeTarget(@"D:\", "自定义客户端"),
            @"D:\自定义客户端",
            StringComparison.OrdinalIgnoreCase),
        "Drive root must use the configured install folder name");

    var sourceHashes = Directory.EnumerateFiles(client, "*", SearchOption.AllDirectories)
        .Where(path => !path.EndsWith(".dnf90-install-manifest.json", StringComparison.OrdinalIgnoreCase))
        .ToDictionary(
            path => Path.GetRelativePath(client, path).Replace('\\', '/'),
            path => Hash(path),
            StringComparer.OrdinalIgnoreCase);

    var splitOutput = Path.Combine(output, "split");
    var splitResult = await ClientPackager.BuildAsync(
        analysis,
        new BuildOptions(
            client, splitOutput, "Setup", "smoke", "DNF_LY", launcherPath,
            InstallPathHelper.DefaultInstallFolderName,
            ReleaseContent.Full,
            64L * 1024 * 1024, template),
        null);
    Assert(SetupFooter.TryRead(splitResult.SetupPath)?.Mode == SetupSourceMode.ExternalPackages,
        "External-package setup footer missing");
    Assert(splitResult.PackagesDirectory is not null && Directory.Exists(splitResult.PackagesDirectory),
        "External package directory missing");
    Assert(!Directory.Exists(Path.Combine(splitOutput, "Setup.packages")),
        "A Setup.packages directory must not be created");
    Assert(Directory.GetFiles(splitResult.PackagesDirectory!, "*.zip").Length == 4,
        "External package directory should contain exactly four .zip packages");
    Assert(!File.Exists(Path.Combine(splitResult.PackagesDirectory!, "manifest.json")),
        "External package directory should not need a separate manifest.json");

    using (var payload = SetupPayload.Open(splitResult.SetupPath))
    {
        Assert(payload.Packages.Count == 4,
            "The full installer should discover all matching package files");
        Directory.CreateDirectory(installedSplit);
        await GameInstaller.InstallAsync(payload, installedSplit, null);
    }
    CompareTrees(sourceHashes, installedSplit);

    // Older packages may store a Chinese entry name as GBK without setting the
    // ZIP UTF-8 flag. The installer must recover the original filename.
    var legacyOutput = Path.Combine(work, "legacy-encoding");
    Directory.CreateDirectory(legacyOutput);
    foreach (var packagePath in Directory.GetFiles(splitResult.PackagesDirectory!, "*.zip"))
    {
        File.Copy(packagePath, Path.Combine(legacyOutput, Path.GetFileName(packagePath)));
    }
    File.Copy(
        splitResult.SetupPath,
        Path.Combine(legacyOutput, Path.GetFileName(splitResult.SetupPath)));
    CreateLegacyNameZip(
        Path.Combine(legacyOutput, "DNF_LY_smoke_002.zip"),
        "自定义登录器.exe",
        launcherPath);
    var installedLegacy = Path.Combine(work, "installed-legacy");
    using (var payload = SetupPayload.Open(Path.Combine(legacyOutput, "Setup.exe")))
    {
        Directory.CreateDirectory(installedLegacy);
        await GameInstaller.InstallAsync(payload, installedLegacy, null);
    }
    Assert(
        File.Exists(Path.Combine(installedLegacy, "自定义登录器.exe")),
        "Legacy GBK ZIP entry name was not decoded correctly");

    // Replace only Script.pvf in the original full installer directory and make
    // sure the first full Setup still discovers and installs every ZIP by prefix.
    CreateFile(Path.Combine(client, "Script.pvf"), 1024 * 1024, 99);
    var updatedPvfOutput = Path.Combine(work, "updated-pvf");
    var updatedPvf = await ClientPackager.BuildAsync(
        analysis,
        new BuildOptions(
            client, updatedPvfOutput, "Update", "smoke", "DNF_LY", launcherPath,
            InstallPathHelper.DefaultInstallFolderName,
            ReleaseContent.PvfOnly,
            64L * 1024 * 1024, template),
        null);
    File.Copy(
        Path.Combine(updatedPvf.PackagesDirectory!, "DNF_LY_smoke_001.zip"),
        Path.Combine(splitResult.PackagesDirectory!, "DNF_LY_smoke_001.zip"),
        overwrite: true);
    var updatedSourceHashes = Directory.EnumerateFiles(client, "*", SearchOption.AllDirectories)
        .Where(path => !path.EndsWith(".dnf90-install-manifest.json", StringComparison.OrdinalIgnoreCase))
        .ToDictionary(
            path => Path.GetRelativePath(client, path).Replace('\\', '/'),
            path => Hash(path),
            StringComparer.OrdinalIgnoreCase);
    var installedUpdated = Path.Combine(work, "installed-updated");
    using (var payload = SetupPayload.Open(splitResult.SetupPath))
    {
        Assert(payload.Packages.Count == 4,
            "The first full Setup should rediscover all matching packages");
        Directory.CreateDirectory(installedUpdated);
        await GameInstaller.InstallAsync(payload, installedUpdated, null);
    }
    CompareTrees(updatedSourceHashes, installedUpdated);

    // Selective releases start in an empty output directory, proving they do not
    // require a previous full release. Each later build must preserve unrelated
    // packages and overwrite only the selected fixed-number slots.
    var partialOutput = Path.Combine(work, "partial-output");
    var pvfOnly = await ClientPackager.BuildAsync(
        analysis,
        new BuildOptions(
            client, partialOutput, "Setup", "smoke", "DNF_LY", launcherPath,
            InstallPathHelper.DefaultInstallFolderName,
            ReleaseContent.PvfOnly,
            64L * 1024 * 1024, template),
        null);
    Assert(pvfOnly.PackagesDirectory is not null, "PVF-only packages directory missing");
    AssertPackageFiles(pvfOnly.PackagesDirectory!, "DNF_LY_smoke_001.zip");
    using (var payload = SetupPayload.Open(pvfOnly.SetupPath))
    {
        Assert(payload.Manifest.ReleaseContent == ReleaseContent.PvfOnly &&
               payload.Packages.Count == 1 && payload.Packages[0].Id == "script-pvf",
            "PVF-only manifest is incorrect");
    }

    var accessoriesOnly = await ClientPackager.BuildAsync(
        analysis,
        new BuildOptions(
            client, partialOutput, "Setup", "smoke", "DNF_LY", launcherPath,
            InstallPathHelper.DefaultInstallFolderName,
            ReleaseContent.RemainingOnly,
            64L * 1024 * 1024, template),
        null);
    AssertPackageFiles(
        accessoriesOnly.PackagesDirectory!,
        "DNF_LY_smoke_001.zip",
        "DNF_LY_smoke_002.zip");
    using (var payload = SetupPayload.Open(accessoriesOnly.SetupPath))
    {
        Assert(payload.Manifest.ReleaseContent == ReleaseContent.RemainingOnly &&
               payload.Packages.Count == 2 && payload.Packages.Count ==
               Directory.GetFiles(partialOutput, "*.zip").Length,
            "Accessories-only manifest is incorrect");
    }

    var npkOnly = await ClientPackager.BuildAsync(
        analysis,
        new BuildOptions(
            client, partialOutput, "Setup", "smoke", "DNF_LY", launcherPath,
            InstallPathHelper.DefaultInstallFolderName,
            ReleaseContent.ImagePacksOnly,
            64L * 1024 * 1024, template),
        null);
    AssertPackageFiles(
        npkOnly.PackagesDirectory!,
        "DNF_LY_smoke_001.zip",
        "DNF_LY_smoke_002.zip",
        "DNF_LY_smoke_003.zip",
        "DNF_LY_smoke_004.zip");
    using (var payload = SetupPayload.Open(npkOnly.SetupPath))
    {
        Assert(payload.Manifest.ReleaseContent == ReleaseContent.ImagePacksOnly &&
               payload.Packages.Count == 4 &&
               payload.Packages.Count == Directory.GetFiles(partialOutput, "*.zip").Length,
            "NPK-only manifest is incorrect");
    }

    Console.WriteLine("PASS");
    Console.WriteLine("split=" + splitResult.SetupPath);
    Console.WriteLine("work=" + work);
    return 0;
}
catch (Exception ex)
{
    Console.Error.WriteLine("FAIL: " + ex);
    Console.Error.WriteLine("work=" + work);
    return 1;
}
finally
{
    if (!args.Contains("--keep", StringComparer.OrdinalIgnoreCase) && Directory.Exists(work))
    {
        Directory.Delete(work, recursive: true);
    }
}

static void CreateFile(string path, long length, byte seed)
{
    Directory.CreateDirectory(Path.GetDirectoryName(path)!);
    using var stream = new FileStream(path, FileMode.Create, FileAccess.Write, FileShare.None);
    stream.SetLength(length);
    if (length > 0)
    {
        stream.Position = 0;
        stream.WriteByte(seed);
        stream.Position = Math.Max(0, length - 1);
        stream.WriteByte((byte)(seed + 1));
    }
}

static string Hash(string path)
{
    using var stream = File.OpenRead(path);
    return Convert.ToHexString(SHA256.HashData(stream));
}

static void CreateLegacyNameZip(string zipPath, string entryName, string sourcePath)
{
    Encoding.RegisterProvider(CodePagesEncodingProvider.Instance);
    var nameBytes = Encoding.GetEncoding(936).GetBytes(entryName);
    var data = File.ReadAllBytes(sourcePath);
    var crc = ComputeCrc32(data);

    using var output = new FileStream(zipPath, FileMode.Create, FileAccess.Write, FileShare.None);
    using var writer = new BinaryWriter(output, Encoding.UTF8, leaveOpen: true);
    writer.Write(0x04034b50u);
    writer.Write((ushort)20);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write(crc);
    writer.Write((uint)data.Length);
    writer.Write((uint)data.Length);
    writer.Write((ushort)nameBytes.Length);
    writer.Write((ushort)0);
    writer.Write(nameBytes);
    writer.Write(data);

    var centralOffset = checked((uint)output.Position);
    writer.Write(0x02014b50u);
    writer.Write((ushort)20);
    writer.Write((ushort)20);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write(crc);
    writer.Write((uint)data.Length);
    writer.Write((uint)data.Length);
    writer.Write((ushort)nameBytes.Length);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write(0u);
    writer.Write(0u);
    writer.Write(nameBytes);

    var centralSize = checked((uint)output.Position - centralOffset);
    writer.Write(0x06054b50u);
    writer.Write((ushort)0);
    writer.Write((ushort)0);
    writer.Write((ushort)1);
    writer.Write((ushort)1);
    writer.Write(centralSize);
    writer.Write(centralOffset);
    writer.Write((ushort)0);
}

static uint ComputeCrc32(byte[] data)
{
    var crc = 0xffffffffu;
    foreach (var value in data)
    {
        crc ^= value;
        for (var bit = 0; bit < 8; bit++)
        {
            crc = (crc >> 1) ^ (0xedb88320u & (uint)-(int)(crc & 1));
        }
    }
    return ~crc;
}

static void CompareTrees(Dictionary<string, string> expected, string installedRoot)
{
    foreach (var pair in expected)
    {
        var path = Path.Combine(installedRoot, pair.Key.Replace('/', Path.DirectorySeparatorChar));
        Assert(File.Exists(path), "Missing installed file: " + pair.Key);
        Assert(Hash(path) == pair.Value, "Installed file hash mismatch: " + pair.Key);
    }
}

static void AssertPackageFiles(string directory, params string[] expected)
{
    var actual = Directory.GetFiles(directory, "*.zip")
        .Select(Path.GetFileName)
        .OrderBy(x => x, StringComparer.OrdinalIgnoreCase)
        .ToArray();
    Assert(actual.SequenceEqual(expected.OrderBy(x => x, StringComparer.OrdinalIgnoreCase)),
        "Unexpected package files: " + string.Join(", ", actual));
}

static void Assert(bool condition, string message)
{
    if (!condition)
    {
        throw new InvalidOperationException(message);
    }
}
