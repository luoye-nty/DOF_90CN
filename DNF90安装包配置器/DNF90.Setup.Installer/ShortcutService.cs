using System.IO;

namespace DNF90.Setup.Installer;

internal static class ShortcutService
{
    public static void TryCreateDesktopShortcut(
        string installDirectory,
        string launcherPath,
        string displayName)
    {
        try
        {
            var shellType = Type.GetTypeFromProgID("WScript.Shell");
            if (shellType is null)
            {
                return;
            }

            dynamic shell = Activator.CreateInstance(shellType)!;
            var shortcutPath = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory),
                displayName + ".lnk");
            dynamic shortcut = shell.CreateShortcut(shortcutPath);
            shortcut.TargetPath = launcherPath;
            shortcut.WorkingDirectory = installDirectory;
            shortcut.IconLocation = launcherPath + ",0";
            shortcut.Description = "启动" + displayName;
            shortcut.Save();
        }
        catch
        {
            // 快捷方式失败不影响客户端文件安装。
        }
    }
}
