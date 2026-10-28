using System;
using System.Security.Cryptography;
using System.Text;

namespace DNFLogin.Services.ClientYh
{
    /// <summary>
    /// 外网版 client.yh 密钥材料。
    ///
    /// V13 授权上收（第②步）后：
    ///   - EXTERNAL 编译（登录器外网版）不再内置任何密钥常量与本地验签/解密能力——
    ///     授权裁决整体上收验证服务器，内置密钥只会成为可被逆向提取的攻击面，故整段剔除。
    ///   - 非 EXTERNAL 编译（ClientYhSigner keygen / 桌面签发工具）保留原混淆常量与 Obfuscate，
    ///     供作者签发与 keygen 回填使用；签发产物 client.yh 的验签密钥只配置在 authserver 环境变量。
    ///
    /// 注意：keygen 轮换后必须同步 authserver 的 YH_PUBKEY_HEX/YH_AESKEY_HEX，否则线上验签失败。
    /// </summary>
    internal static class ClientYhKeys
    {
#if !EXTERNAL
        /// <summary>掩码盐（客户端与签发工具两端写死一致；仅作常规逆向门槛，非高强度保密）</summary>
        private const string MaskSalt = "DNFLogin.ClientYh.KeyMask.v1";

        /// <summary>Ed25519 公钥 32B XOR 掩码后的 Base64（由 keygen 输出回填）</summary>
        private const string PublicKeyObfBase64 = "06d1ojjFxOw1CpzttIiewLTKieNjGq5LetVZnV+I3Js=";

        /// <summary>AES-256 密钥 32B XOR 掩码后的 Base64（由 keygen 输出回填）</summary>
        private const string AesKeyObfBase64 = "cxO2TRcCsJcaGmRLyjowqrL6hCXMxfI++wkQG5ozp0c=";

        private static byte[] GetMask() => SHA256.HashData(Encoding.UTF8.GetBytes(MaskSalt));

        /// <summary>供 ClientYhSigner keygen 使用：32B 材料生成可回填到本文件常量的混淆 Base64</summary>
        public static string Obfuscate(byte[] raw32)
        {
            if (raw32 is null || raw32.Length != 32)
            {
                throw new ArgumentException("材料必须为 32 字节", nameof(raw32));
            }

            return Convert.ToBase64String(XorWithMask(raw32));
        }

        private static byte[] XorWithMask(byte[] raw32)
        {
            var mask = GetMask();
            var result = new byte[32];
            for (var i = 0; i < 32; i++)
            {
                result[i] = (byte)(raw32[i] ^ mask[i]);
            }

            return result;
        }

        private static byte[] Unobfuscate(string base64)
        {
            var raw = Convert.FromBase64String(base64);
            if (raw.Length != 32)
            {
                throw new InvalidOperationException("ClientYh 密钥常量长度必须为 32 字节");
            }

            return XorWithMask(raw);
        }

        /// <summary>Ed25519 验签公钥（32B）—— 仅签发端/诊断工具可见；EXTERNAL 登录器不含此实现</summary>
        public static byte[] GetPublicKey() => Unobfuscate(PublicKeyObfBase64);

        /// <summary>AES-256-GCM 解密密钥（32B）—— 仅签发端/诊断工具可见；EXTERNAL 登录器不含此实现</summary>
        public static byte[] GetAesKey() => Unobfuscate(AesKeyObfBase64);
#else
        // EXTERNAL（登录器外网版）：本地不内置任何 client.yh 密钥。
        // 以下防御性占位仅为防止历史残留引用编译失败，运行时调用即视为误用。
        public static string Obfuscate(byte[] raw32) =>
            throw new NotSupportedException("EXTERNAL 构建不包含 ClientYh 密钥");

        public static byte[] GetPublicKey() =>
            throw new NotSupportedException("EXTERNAL 构建不包含 ClientYh 密钥");

        public static byte[] GetAesKey() =>
            throw new NotSupportedException("EXTERNAL 构建不包含 ClientYh 密钥");
#endif
    }
}
