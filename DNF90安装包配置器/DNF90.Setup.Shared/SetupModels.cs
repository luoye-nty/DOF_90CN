using System.Text.Json.Serialization;

namespace DNF90.Setup.Shared;

public enum SetupSourceMode
{
    Embedded = 1,
    ExternalPackages = 2,
}

public enum ReleaseContent
{
    Full = 0,
    PvfOnly = 1,
    ImagePacksOnly = 2,
    RemainingOnly = 3,
}

public sealed record SetupFooterInfo(
    SetupSourceMode Mode,
    long PayloadOffset,
    long PayloadLength,
    int FormatVersion);

public sealed class SetupManifest
{
    public string PayloadFormat { get; set; } = "dnf90-setup/1";
    public string ProductName { get; set; } = "落叶DNF";
    public string PackagePrefix { get; set; } = "DNF_LY";
    public ReleaseContent ReleaseContent { get; set; } = ReleaseContent.Full;
    public string Version { get; set; } = "";
    public string SourceFolderName { get; set; } = "";
    public DateTimeOffset CreatedAt { get; set; } = DateTimeOffset.Now;
    public long TotalUncompressedBytes { get; set; }
    public long TotalCompressedBytes { get; set; }
    public string InstallFolderName { get; set; } = "地下城与勇士";
    public string LauncherRelativePath { get; set; } = "落叶DNF登录器.exe";
    public List<SetupPackageInfo> Packages { get; set; } = new();
}

public sealed class SetupPackageInfo
{
    public string Id { get; set; } = "";
    public string DisplayName { get; set; } = "";
    public string FileName { get; set; } = "";
    public long UncompressedBytes { get; set; }
    public long CompressedBytes { get; set; }
    public int FileCount { get; set; }
    public string Sha256 { get; set; } = "";

    [JsonIgnore]
    public string LocalPath { get; set; } = "";
}

public sealed record SourceFileInfo(string FullPath, string RelativePath, long Length);

public sealed class ClientAnalysis
{
    public string ClientRoot { get; init; } = "";
    public List<SourceFileInfo> ImagePackFiles { get; init; } = new();
    public List<SourceFileInfo> ScriptFiles { get; init; } = new();
    public List<SourceFileInfo> Cn90Files { get; init; } = new();
    public List<SourceFileInfo> OtherFiles { get; init; } = new();

    public long TotalBytes =>
        ImagePackFiles.Sum(x => x.Length) +
        ScriptFiles.Sum(x => x.Length) +
        Cn90Files.Sum(x => x.Length) +
        OtherFiles.Sum(x => x.Length);

    public int TotalFiles =>
        ImagePackFiles.Count + ScriptFiles.Count + Cn90Files.Count + OtherFiles.Count;
}

public sealed record BuilderProgress(string Status, string Detail, double Percent);

public sealed record InstallProgress(string Status, string Detail, double Percent);

public sealed record BuildOptions(
    string ClientRoot,
    string OutputDirectory,
    string SetupBaseName,
    string Version,
    string PackagePrefix,
    string LauncherPath,
    string InstallFolderName,
    ReleaseContent ReleaseContent,
    long ImagePackTargetBytes,
    string InstallerTemplatePath);

public sealed record BuildResult(
    string SetupPath,
    string? PackagesDirectory,
    SetupManifest Manifest);
