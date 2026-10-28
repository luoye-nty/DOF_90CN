using System;
using System.Buffers.Binary;
using System.Security.Cryptography;
using System.Text.Json;
using System.Text.Json.Serialization;
using Org.BouncyCastle.Crypto.Parameters;
using Org.BouncyCastle.Crypto.Signers;

namespace DNFLogin.Services.ClientYh
{
    /// <summary>
    /// client.yh 载荷（AES-256-GCM 密文内的明文 JSON，字段语义与改造方案 §7.1 一致）
    /// </summary>
    public sealed class ClientYhPayload
    {
        /// <summary>目标发行版：90 / S4A21 / S4A12（须与 EditionProfileId 一致，否则拒绝启动）</summary>
        [JsonPropertyName("target")]
        public string Target { get; set; } = "";

        /// <summary>服务器 IP（写入 Settings.RemoteIp / CurrentProfile.RemoteIp）</summary>
        [JsonPropertyName("server_ip")]
        public string ServerIp { get; set; } = "";

        /// <summary>认证端口（90=18112；S4=7002。写入 RemotePort 顶层字段）</summary>
        [JsonPropertyName("auth_port")]
        public int AuthPort { get; set; }

        /// <summary>远程服务 URL（写入 Settings.RemoteServerUrl；无则留空）</summary>
        [JsonPropertyName("remote_url")]
        public string RemoteUrl { get; set; } = "";

        /// <summary>默认邀请码（写入 Settings.InviteCode；无则留空）</summary>
        [JsonPropertyName("invite")]
        public string Invite { get; set; } = "";

        /// <summary>签发时间（ISO-8601 UTC；仅审计用，不参与校验）</summary>
        [JsonPropertyName("issued_at")]
        public string IssuedAt { get; set; } = "";
    }

    /// <summary>client.yh 验签/解析结果</summary>
    public sealed class ClientYhVerifyResult
    {
        public bool Ok { get; set; }
        public string Error { get; set; } = "";
        public ClientYhPayload? Payload { get; set; }
    }

    /// <summary>
    /// client.yh 文件格式与加密验签原语（M4，外网版核心）。
    ///
    /// 二进制布局（与改造方案 §7.1 一致，Nonce 采用 GCM 标准 96-bit/12 字节）：
    ///   偏移  长度  内容
    ///   0     4     魔数 0xE8 0x0D 0x46 0x4C
    ///   4     1     格式版本 FORMAT_VER = 1
    ///   5     1     载荷算法标识（1 = AES-256-GCM）
    ///   6     12    AES-GCM Nonce（96-bit，GCM 标准长度）
    ///   18    4     载荷块长度 L（= 密文长度 + 16 字节 GCM Tag，大端序）
    ///   22    L     载荷块 = AES-256-GCM(密文 || Tag)
    ///   22+L  64    Ed25519 签名（对 [0 .. 22+L) 全部字节签名）
    ///
    /// 两层防护：AES-256-GCM 加密载荷（密钥混淆内置 exe）；Ed25519 作者私钥对全文件签名（公钥内置 exe）。
    /// 本类为纯原语层（不依赖 WPF / AppConfig），由登录器（验签/解密）与桌面签发工具（生成/签发）共享。
    /// </summary>
    public static class ClientYhFormat
    {
        public const int MagicLen = 4;
        public const byte FormatVersion = 1;
        public const byte AlgAes256Gcm = 1;
        public const int NonceLen = 12;
        public const int LenFieldLen = 4;
        public const int HeaderLen = MagicLen + 1 + 1 + NonceLen + LenFieldLen; // = 22
        public const int GcmTagLen = 16;
        public const int SignatureLen = 64;

        private static readonly byte[] Magic = { 0xE8, 0x0D, 0x46, 0x4C };

        private static readonly JsonSerializerOptions JsonOptions = new()
        {
            PropertyNameCaseInsensitive = true
        };

        /// <summary>
        /// 生成一份 client.yh 文件字节（作者签发端）。
        /// </summary>
        /// <param name="payload">载荷（明文 JSON 将被加密）</param>
        /// <param name="ed25519Seed">Ed25519 私钥种子（32B，作者私密，绝不入客户端）</param>
        /// <param name="aesKey">AES-256 密钥（32B，须与登录器内置 GetAesKey 一致）</param>
        public static byte[] CreateFile(ClientYhPayload payload, byte[] ed25519Seed, byte[] aesKey)
        {
            if (payload is null)
            {
                throw new ArgumentNullException(nameof(payload));
            }

            if (ed25519Seed is null || ed25519Seed.Length != 32)
            {
                throw new ArgumentException("Ed25519 seed 必须为 32 字节", nameof(ed25519Seed));
            }

            if (aesKey is null || aesKey.Length != 32)
            {
                throw new ArgumentException("AES 密钥必须为 32 字节", nameof(aesKey));
            }

            var nonce = RandomNumberGenerator.GetBytes(NonceLen);
            var json = JsonSerializer.SerializeToUtf8Bytes(payload, JsonOptions);

            var ct = new byte[json.Length];
            var tag = new byte[GcmTagLen];
            using (var gcm = new AesGcm(aesKey, GcmTagLen))
            {
                gcm.Encrypt(nonce, json, ct, tag);
            }

            // 载荷块 = 密文 || Tag
            var body = new byte[ct.Length + tag.Length];
            Array.Copy(ct, 0, body, 0, ct.Length);
            Array.Copy(tag, 0, body, ct.Length, tag.Length);

            var total = HeaderLen + body.Length;
            var file = new byte[total + SignatureLen];

            // 头部
            Array.Copy(Magic, 0, file, 0, MagicLen);
            file[MagicLen] = FormatVersion;
            file[MagicLen + 1] = AlgAes256Gcm;
            Array.Copy(nonce, 0, file, MagicLen + 2, NonceLen);
            BinaryPrimitives.WriteInt32BigEndian(file.AsSpan(HeaderLen - LenFieldLen, LenFieldLen), body.Length);
            Array.Copy(body, 0, file, HeaderLen, body.Length);

            // Ed25519 签名 [0..total)
            var signer = new Ed25519Signer();
            signer.Init(true, new Ed25519PrivateKeyParameters(ed25519Seed, 0));
            signer.BlockUpdate(file, 0, total);
            var signature = signer.GenerateSignature();
            Array.Copy(signature, 0, file, total, SignatureLen);
            return file;
        }

        /// <summary>
        /// 校验并解析 client.yh：结构 → Ed25519 验签 → AES-256-GCM 解密 → JSON 反序列化。
        /// 验签范围覆盖整个头部与载荷块，任何篡改都会失败。
        /// </summary>
        public static ClientYhVerifyResult TryParseFile(byte[] file, byte[] aesKey, byte[] publicKey)
        {
            var result = new ClientYhVerifyResult();
            if (file is null)
            {
                result.Error = "文件数据为空";
                return result;
            }

            if (file.Length < HeaderLen + SignatureLen)
            {
                result.Error = "文件长度不足（损坏）";
                return result;
            }

            // 魔数 / 格式版本 / 算法标识
            for (var i = 0; i < MagicLen; i++)
            {
                if (file[i] != Magic[i])
                {
                    result.Error = "文件魔数不符（损坏）";
                    return result;
                }
            }

            if (file[MagicLen] != FormatVersion)
            {
                result.Error = "不支持的格式版本";
                return result;
            }

            if (file[MagicLen + 1] != AlgAes256Gcm)
            {
                result.Error = "不支持的载荷算法标识";
                return result;
            }

            var bodyLen = BinaryPrimitives.ReadInt32BigEndian(file.AsSpan(HeaderLen - LenFieldLen, LenFieldLen));
            if (bodyLen < GcmTagLen || file.Length != HeaderLen + bodyLen + SignatureLen)
            {
                result.Error = "长度字段与文件不符（损坏）";
                return result;
            }

            var total = HeaderLen + bodyLen;

            // 1) Ed25519 验签 [0..total)
            try
            {
                var verifier = new Ed25519Signer();
                verifier.Init(false, new Ed25519PublicKeyParameters(publicKey, 0));
                verifier.BlockUpdate(file, 0, total);
                var signature = file.AsSpan(total, SignatureLen).ToArray();
                if (!verifier.VerifySignature(signature))
                {
                    result.Error = "签名验证失败（文件被篡改或非授权签发）";
                    return result;
                }
            }
            catch (Exception ex)
            {
                result.Error = "签名验证异常：" + ex.Message;
                return result;
            }

            // 2) AES-256-GCM 解密（nonce → 密文+Tag → 明文 JSON）
            var nonce = file.AsSpan(MagicLen + 2, NonceLen).ToArray();
            var body = file.AsSpan(HeaderLen, bodyLen);
            var ct = body[..^GcmTagLen].ToArray();
            var tag = body[^GcmTagLen..].ToArray();
            var plain = new byte[ct.Length];
            try
            {
                using var gcm = new AesGcm(aesKey, GcmTagLen);
                gcm.Decrypt(nonce, ct, tag, plain);
            }
            catch (CryptographicException)
            {
                result.Error = "载荷解密失败（密钥不匹配或数据损坏）";
                return result;
            }
            catch (Exception ex)
            {
                result.Error = "载荷解密异常：" + ex.Message;
                return result;
            }

            try
            {
                var payload = JsonSerializer.Deserialize<ClientYhPayload>(plain, JsonOptions);
                if (payload is null || string.IsNullOrWhiteSpace(payload.Target))
                {
                    result.Error = "载荷 JSON 无效（缺少 target）";
                    return result;
                }

                result.Payload = payload;
                result.Ok = true;
                return result;
            }
            catch (Exception ex)
            {
                result.Error = "载荷 JSON 解析失败：" + ex.Message;
                return result;
            }
        }
    }
}
