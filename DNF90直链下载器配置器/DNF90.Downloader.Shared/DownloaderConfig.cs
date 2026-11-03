namespace DNF90.Downloader.Shared;

public sealed class DownloaderConfig
{
    public string DownloaderName { get; set; } = "DNF直链下载器";
    public List<string> DownloadUrls { get; set; } = new();
    public string DefaultDisk { get; set; } = "D";
    public string DefaultDownloadFolderName { get; set; } = "地下城与勇士安装文件";
}

public sealed record DownloadProgress(
    int UrlIndex,
    int UrlCount,
    string FileName,
    string Status,
    long Done,
    long Total,
    double SpeedMBps,
    double OverallPercent);

public sealed record DownloadItemResult(string Url, string FilePath);

public sealed record DownloadResult(IReadOnlyList<DownloadItemResult> Items);
