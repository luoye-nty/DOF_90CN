using System.Threading.Tasks;

namespace DNFLogin.Services.ClientYh
{
    /// <summary>
    /// 【已去授权】原 client.yh 在线授权验证服务器客户端。
    ///
    /// 现状：
    ///   - 类里**不再包含任何验证服务器地址、证书指纹、密钥**；
    ///   - ClientYhGuard.RunStartupCheck 已直接放行，不再上报 client.yh；
    ///   - RemoteAuthService 也不再申请 nonce / 上报 yh_ticket；
    ///   - 下面两个入口保留为"恒不联网"的空壳，只为不打断历史引用。
    ///
    /// 若将来要恢复在线授权：需要重新填入验证服务器地址与证书 DER SHA-256
    /// 指纹、把 ClientYhGuard 的上报流程接回，并把服务端
    /// internal/server/yhcheck.go 的核销重新装配起来。
    /// </summary>
    internal static class YhAuthClient
    {
        /// <summary>verify 响应（原服务端 resp{ok,code,msg,data} 解包后）</summary>
        public sealed class VerifyResult
        {
            public bool Ok { get; set; }
            public string Code { get; set; } = "";
            public string Msg { get; set; } = "";
            public string Ticket { get; set; } = "";
            public ClientYhPayload? Payload { get; set; }
        }

        /// <summary>
        /// 【已去授权】原：上报 client.yh 原始字节换取票据与明文载荷。
        /// 现在恒返回失败，且**不发起任何网络请求**（去授权版本没有可用地址）。
        /// </summary>
        public static Task<VerifyResult> VerifyAsync(
            byte[] yhBytes, string target, string targetIp, string machineId)
        {
            var result = new VerifyResult
            {
                Ok = false,
                Code = "ERR_AUTH_DISABLED",
                Msg = "在线授权已移除（去授权版本），本调用不再联网。"
            };
            return Task.FromResult(result);
        }

        /// <summary>
        /// 【已去授权】原：POST /api/yh/nonce 申请一次性 nonce。
        /// 现在恒返回失败，且**不发起任何网络请求**。
        /// </summary>
        public static Task<(bool Ok, string Error, string Nonce)> RequestNonceAsync(string ticket)
        {
            return Task.FromResult<(bool, string, string)>(
                (false, "在线授权已移除（去授权版本），本调用不再联网。", ""));
        }
    }
}
