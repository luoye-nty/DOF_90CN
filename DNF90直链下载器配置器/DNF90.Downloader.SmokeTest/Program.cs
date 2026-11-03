using System.Net;
using System.Net.Sockets;
using System.Text;
using DNF90.Downloader.Shared;

if (args.Length < 1)
{
    Console.Error.WriteLine("Usage: DNF90.Downloader.SmokeTest <downloader-template.exe>");
    return 2;
}

var template = Path.GetFullPath(args[0]);
if (!File.Exists(template))
{
    Console.Error.WriteLine("Downloader template not found: " + template);
    return 2;
}

var work = Path.Combine(Path.GetTempPath(), "DNF90DownloaderSmoke-" + Guid.NewGuid().ToString("N"));
try
{
    Directory.CreateDirectory(work);
    using var listener = new TcpListener(IPAddress.Loopback, 0);
    listener.Start();
    var port = ((IPEndPoint)listener.LocalEndpoint).Port;
    listener.Stop();

    using var http = new HttpListener();
    http.Prefixes.Add($"http://127.0.0.1:{port}/");
    http.Start();
    var requests = new List<string>();
    var serverTask = Task.Run(async () =>
    {
        for (var i = 0; i < 2; i++)
        {
            var context = await http.GetContextAsync();
            requests.Add(context.Request.RawUrl ?? "");
            var name = i == 0 ? "client-package.zip" : "installer.exe";
            var data = i == 0
                ? Encoding.UTF8.GetBytes("fake-zip-payload")
                : Encoding.UTF8.GetBytes("fake-exe-payload");
            context.Response.StatusCode = 200;
            context.Response.ContentType = i == 0
                ? "application/zip"
                : "application/vnd.microsoft.portable-executable";
            context.Response.ContentLength64 = data.Length;
            context.Response.AddHeader(
                "Content-Disposition",
                $"attachment; filename=\"{name}\"");
            await context.Response.OutputStream.WriteAsync(data);
            context.Response.Close();
        }
    });

    var config = new DownloaderConfig
    {
        DownloaderName = "测试直链下载器",
        DefaultDisk = "D",
        DefaultDownloadFolderName = "地下城与勇士安装文件",
        DownloadUrls = new List<string>
        {
            $"http://127.0.0.1:{port}/first",
            $"http://127.0.0.1:{port}/second",
        },
    };
    var generated = Path.Combine(work, "测试直链下载器.exe");
    DownloaderConfigFooter.Generate(template, generated, config);
    var loaded = DownloaderConfigFooter.Read(generated);
    Assert(loaded.DownloaderName == config.DownloaderName, "Downloader name mismatch");
    Assert(loaded.DownloadUrls.SequenceEqual(config.DownloadUrls), "URL order mismatch");
    Assert(
        DownloaderPathHelper.DefaultDownloadDirectory("D", loaded.DefaultDownloadFolderName) ==
        @"D:\地下城与勇士安装文件",
        "Default directory mismatch");

    var downloadDirectory = Path.Combine(work, "downloads");
    var result = await DirectDownloadEngine.DownloadAllAsync(config, downloadDirectory, null);
    await serverTask;
    Assert(result.Items.Count == 2, "Expected two downloaded files");
    Assert(result.Items[0].FilePath.EndsWith("client-package.zip"), "ZIP filename mismatch");
    Assert(result.Items[1].FilePath.EndsWith("installer.exe"), "EXE filename mismatch");
    Assert(requests.SequenceEqual(new[] { "/first", "/second" }), "Downloads were not sequential");
    Assert(File.ReadAllText(result.Items[1].FilePath) == "fake-exe-payload", "Downloaded content mismatch");

    Console.WriteLine("PASS");
    Console.WriteLine("generated=" + generated);
    Console.WriteLine("downloads=" + downloadDirectory);
    return 0;
}
catch (Exception ex)
{
    Console.Error.WriteLine("FAIL: " + ex);
    return 1;
}
finally
{
    if (!args.Contains("--keep", StringComparer.OrdinalIgnoreCase) &&
        Directory.Exists(work))
    {
        Directory.Delete(work, recursive: true);
    }
}

static void Assert(bool condition, string message)
{
    if (!condition)
    {
        throw new InvalidOperationException(message);
    }
}
