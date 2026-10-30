using System;
using System.IO;
using System.Reflection;
using System.Security.Cryptography;

namespace DNFLogin.Services
{
    /// <summary>
    /// 90CN.dll 内嵌资源完整性辅助：读取程序集内嵌的 90CN.dll（DNFLogin.Resources.90CN.dll，
    /// csproj EmbeddedResource 全版本携带）SHA256，与客户端目录中的 90CN.dll 比对，
    /// 由调用方（启动自检 / 启动游戏前复检）在缺失或哈希不一致时直接写入/覆盖（不弹确认、不备份）。
    /// </summary>
    public static class Cn90DllHelper
    {
        private const string ResourceName = "DNFLogin.Resources.90CN.dll";
        private const string ClientFileName = "90CN.dll";

        /// <summary>读取内嵌 90CN.dll 的 SHA256（hex 小写）。内嵌资源缺失/读取异常返回 null。</summary>
        public static string? EmbeddedSha256()
        {
            try
            {
                using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName);
                if (stream is null)
                {
                    return null;
                }

                using var sha = SHA256.Create();
                return Convert.ToHexString(sha.ComputeHash(stream)).ToLowerInvariant();
            }
            catch
            {
                return null;
            }
        }

        /// <summary>计算客户端目录 90CN.dll 的 SHA256（hex 小写）。文件不存在/读取异常返回 null。</summary>
        public static string? ClientSha256(string clientRoot)
        {
            try
            {
                var path = Path.Combine(clientRoot, ClientFileName);
                if (!File.Exists(path))
                {
                    return null;
                }

                using var fs = File.OpenRead(path);
                using var sha = SHA256.Create();
                return Convert.ToHexString(sha.ComputeHash(fs)).ToLowerInvariant();
            }
            catch
            {
                return null;
            }
        }

        /// <summary>从内嵌资源提取并写入/覆盖客户端 90CN.dll。成功返回 null，失败返回错误信息。</summary>
        public static string? DeployTo(string clientRoot)
        {
            try
            {
                using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName);
                if (stream is null)
                {
                    return "登录器内嵌 90CN.dll 缺失";
                }

                var path = Path.Combine(clientRoot, ClientFileName);
                using (var fs = new FileStream(path, FileMode.Create, FileAccess.Write, FileShare.Read))
                {
                    stream.CopyTo(fs);
                }

                return null;
            }
            catch (Exception ex)
            {
                return "写入失败：" + ex.Message + "（若提示文件被占用，请先关闭游戏）";
            }
        }
    }
}
