using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>
    /// 纯 C# MySQL 客户端（MySQL Wire Protocol），无任何第三方依赖。
    /// 支持 mysql_native_password 和 caching_sha2_password 两种认证方式。
    /// </summary>
    internal sealed class MySqlClient : IAsyncDisposable
    {
        private TcpClient? _tcp;
        private NetworkStream? _stream;
        private int _seq;
        private string _serverVersion = "";
        private byte[] _scramble = Array.Empty<byte>();
        private string _authPlugin = "";

        public string ServerVersion => _serverVersion;

        public async Task ConnectAsync(string host, int port, string user, string password, string database, int timeoutSeconds = 10)
        {
            _tcp = new TcpClient();
            _tcp.ReceiveTimeout = timeoutSeconds * 1000;
            _tcp.SendTimeout = timeoutSeconds * 1000;
            await _tcp.ConnectAsync(host, port);
            _stream = _tcp.GetStream();
            _seq = 0;

            // 1. Read initial handshake packet
            var handshake = await ReadPacketAsync();
            ParseHandshake(handshake);

            // 2. Build and send handshake response
            var response = BuildHandshakeResponse(user, password, database);
            await SendPacketAsync(response);

            // 3. Read auth response
            var authResp = await ReadPacketAsync();
            Diag($"AUTH authResp len={authResp.Length} code={(authResp.Length>0?authResp[0]:-1)} hex={Convert.ToHexString(authResp).Substring(0, Math.Min(60, authResp.Length * 2))}");
            if (authResp.Length == 0) return;
            int code = authResp[0];

            if (code == 0x00) // OK
            {
                // Authenticated
            }
            else if (code == 0xFF) // Error
            {
                var err = ParseError(authResp);
                throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
            }
            else if (code == 0xFE) // AuthSwitchRequest
            {
                var plugin = ReadNullTerminatedString(authResp, 1, out int pos);
                var scrambleData = new byte[authResp.Length - pos];
                Buffer.BlockCopy(authResp, pos, scrambleData, 0, scrambleData.Length);

                if (plugin == "mysql_native_password")
                {
                    var authResp2 = BuildNativePasswordResponse(password, scrambleData);
                    await SendPacketAsync(authResp2);
                    var final = await ReadPacketAsync();
                    Diag($"AUTH native final code={(final.Length>0?final[0]:-1)} hex={Convert.ToHexString(final).Substring(0, Math.Min(60, final.Length * 2))}");
                    if (final[0] == 0xFF)
                    {
                        var err = ParseError(final);
                        throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                    }
                }
                else if (plugin == "caching_sha2_password")
                {
                    var authResp2 = BuildCachingSha2PasswordResponse(password, scrambleData, true);
                    await SendPacketAsync(authResp2);
                    var final = await ReadPacketAsync();
                    Diag($"AUTH sha2 final code={(final.Length>0?final[0]:-1)} hex={Convert.ToHexString(final).Substring(0, Math.Min(60, final.Length * 2))}");
                    if (final[0] == 0x01)
                    {
                        // fast auth success
                        if (final.Length > 1 && final[1] == 0x03)
                        {
                            // fast auth success: server sends final OK after AuthMoreData(01 03)
                            var finalOk = await ReadPacketAsync();
                            Diag($"AUTH sha2 fast-success finalOk code={(finalOk.Length>0?finalOk[0]:-1)} hex={Convert.ToHexString(finalOk).Substring(0, Math.Min(60, finalOk.Length * 2))}");
                            if (finalOk.Length > 0 && finalOk[0] == 0xFF)
                            {
                                var err = ParseError(finalOk);
                                throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                            }
                        }
                        else if (final.Length > 1 && final[1] == 0x04)
                        {
                            // full auth needed - send public key request
                            await SendPacketAsync(new byte[] { 0x02 });
                            var pubKeyResp = await ReadPacketAsync();
                            Diag($"AUTH pubkey code={(pubKeyResp.Length>0?pubKeyResp[0]:-1)}");
                            if (pubKeyResp[0] == 0x01)
                            {
                                var pem = Encoding.UTF8.GetString(pubKeyResp, 1, pubKeyResp.Length - 1);
                                var encrypted = RsaEncryptPassword(password, scrambleData, pem);
                                await SendPacketAsync(encrypted);
                                var final2 = await ReadPacketAsync();
                                Diag($"AUTH final2 code={(final2.Length>0?final2[0]:-1)} hex={Convert.ToHexString(final2).Substring(0, Math.Min(60, final2.Length * 2))}");
                                if (final2[0] == 0xFF)
                                {
                                    var err = ParseError(final2);
                                    throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                                }
                            }
                        }
                    }
                    else if (final[0] == 0xFF)
                    {
                        var err = ParseError(final);
                        throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                    }
                }
            }
            else if (code == 0x01) // AuthMoreData (caching_sha2)
            {
                Diag($"AUTH code=0x01 len={authResp.Length} hex={Convert.ToHexString(authResp).Substring(0, Math.Min(60, authResp.Length * 2))}");
                if (authResp.Length > 1 && authResp[1] == 0x03)
                {
                    // fast auth success: server sends final OK after AuthMoreData(01 03)
                    var finalOk = await ReadPacketAsync();
                    Diag($"AUTH code=0x01 fast-success finalOk code={(finalOk.Length>0?finalOk[0]:-1)} hex={Convert.ToHexString(finalOk).Substring(0, Math.Min(60, finalOk.Length * 2))}");
                    if (finalOk.Length > 0 && finalOk[0] == 0xFF)
                    {
                        var err = ParseError(finalOk);
                        throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                    }
                }
                else if (authResp.Length > 1 && authResp[1] == 0x04)
                {
                    // full auth needed
                    await SendPacketAsync(new byte[] { 0x02 });
                    var pubKeyResp = await ReadPacketAsync();
                    Diag($"AUTH code=0x01 pubkey code={(pubKeyResp.Length>0?pubKeyResp[0]:-1)}");
                    if (pubKeyResp[0] == 0x01)
                    {
                        var pem = Encoding.UTF8.GetString(pubKeyResp, 1, pubKeyResp.Length - 1);
                        var encrypted = RsaEncryptPassword(password, _scramble, pem);
                        await SendPacketAsync(encrypted);
                        var final2 = await ReadPacketAsync();
                        Diag($"AUTH code=0x01 final2 code={(final2.Length>0?final2[0]:-1)} hex={Convert.ToHexString(final2).Substring(0, Math.Min(60, final2.Length * 2))}");
                        if (final2[0] == 0xFF)
                        {
                            var err = ParseError(final2);
                            throw new InvalidOperationException($"MySQL auth error: [{err.Code}] {err.Message}");
                        }
                    }
                }
            }

            // 4. If database is specified, send COM_INIT_DB
            if (!string.IsNullOrEmpty(database))
            {
                await SendCommandAsync(0x02, Encoding.UTF8.GetBytes(database));
                var initDbResp = await ReadPacketAsync();
                Diag($"AUTH initDbResp code={(initDbResp.Length>0?initDbResp[0]:-1)} hex={Convert.ToHexString(initDbResp).Substring(0, Math.Min(60, initDbResp.Length * 2))}");
                if (initDbResp[0] == 0xFF)
                {
                    var err = ParseError(initDbResp);
                    throw new InvalidOperationException($"MySQL USE database error: [{err.Code}] {err.Message}");
                }
            }
        }

        /// <summary>执行 COM_QUERY 并返回结果集（行列表，字符串语义，保持旧行为）</summary>
        public Task<List<Dictionary<string, object?>>> QueryAsync(string sql)
        {
            return QueryAsync(sql, false);
        }

        /// <summary>执行 COM_QUERY 并返回结果集（行列表）；preserveBinary=true 时二进制列（charset=63）返回 byte[]</summary>
        public async Task<List<Dictionary<string, object?>>> QueryAsync(string sql, bool preserveBinary)
        {
            var result = new List<Dictionary<string, object?>>();
            await SendCommandAsync(0x03, Encoding.UTF8.GetBytes(sql));

            var first = await ReadPacketAsync();
            if (first[0] == 0xFF)
            {
                var err = ParseError(first);
                throw new InvalidOperationException($"MySQL query error: [{err.Code}] {err.Message}");
            }
            if (first[0] == 0x00) // OK (no result set, e.g. UPDATE)
            {
                Diag($"[DIAG] OK-BRANCH first len={first.Length} hex={Convert.ToHexString(first)}\r\n");
                return result;
            }

            // TEMP DIAG
            Diag($"[DIAG] first[0]={first[0]} len={first.Length} hex={Convert.ToHexString(first).Substring(0, Math.Min(40, first.Length * 2))}\r\n");

            // Parse column count
            int colCount = (int)ReadLenEnc(first, 0, out int _);

            // Read column definitions
            var columns = new List<ColumnDef>();
            for (int i = 0; i < colCount; i++)
            {
                var colPacket = await ReadPacketAsync();
                var col = ParseColumnDef(colPacket);
                Diag($"[DIAG] col[{i}] name='{col.Name}' type={col.Type} hex={Convert.ToHexString(colPacket).Substring(0, Math.Min(60, colPacket.Length * 2))}\r\n");
                columns.Add(col);
            }

            // Read EOF (if not DEPRECATE_EOF)
            var eof = await ReadPacketAsync();
            if (eof[0] == 0xFF)
            {
                var err = ParseError(eof);
                throw new InvalidOperationException($"MySQL query error: [{err.Code}] {err.Message}");
            }
            Diag($"[DIAG] eof[0]={eof[0]} hex={Convert.ToHexString(eof)}\r\n");

            // Read rows until EOF
            while (true)
            {
                var rowPacket = await ReadPacketAsync();
                Diag($"[DIAG] rowpkt[0]={rowPacket[0]} hex={Convert.ToHexString(rowPacket).Substring(0, Math.Min(80, rowPacket.Length * 2))}\r\n");
                if (rowPacket[0] == 0xFE) // EOF
                {
                    break;
                }
                if (rowPacket[0] == 0xFF)
                {
                    var err = ParseError(rowPacket);
                    throw new InvalidOperationException($"MySQL query error: [{err.Code}] {err.Message}");
                }

                var row = ParseRow(rowPacket, columns, preserveBinary);
                result.Add(row);
            }

            return result;
        }

        private static readonly string DiagLogPath = Path.Combine(AppContext.BaseDirectory, "mysql_diag.log");

        private static void Diag(string msg)
        {
            // ===== 2026-09 需求调整：停用 mysql_diag.log 诊断日志（不再向磁盘写日志文件）=====
            // 各 Diag(...) 调用点均保留，便于后续排障时取消下方注释即可恢复写日志。
            // try
            // {
            //     var dir = Path.GetDirectoryName(DiagLogPath);
            //     if (!string.IsNullOrEmpty(dir)) Directory.CreateDirectory(dir);
            //     System.IO.File.AppendAllText(DiagLogPath, $"[{DateTime.Now:HH:mm:ss.fff}] {msg}\r\n");
            // }
            // catch
            // {
            //     // 诊断日志失败不影响主流程
            // }
            _ = DiagLogPath; // 保留路径常量引用（避免“已赋值但未使用”告警），日志功能停用后常量仍保留便于恢复
        }

        private void ParseHandshake(byte[] pkt)
        {
            int pos = 0;
            // protocol version
            byte protoVer = pkt[pos++];
            // server version (null terminated)
            _serverVersion = ReadNullTerminatedString(pkt, pos, out pos);
            // thread id (4 bytes)
            pos += 4;
            // auth-plugin-data-part-1 (8 bytes)
            byte[] scramblePart1 = new byte[8];
            Buffer.BlockCopy(pkt, pos, scramblePart1, 0, 8);
            pos += 8;
            // filler (1 byte)
            pos++;
            // capability flags lower (2 bytes)
            ushort capsLower = (ushort)(pkt[pos] | (pkt[pos + 1] << 8));
            pos += 2;
            // character set
            pos++;
            // status flags
            pos += 2;
            // capability flags upper (2 bytes)
            ushort capsUpper = (ushort)(pkt[pos] | (pkt[pos + 1] << 8));
            pos += 2;
            uint caps = (uint)(capsLower | (capsUpper << 16));
            // auth plugin data len
            byte authDataLen = pkt[pos++];
            // reserved (10 bytes)
            pos += 10;
            // auth-plugin-data-part-2
            int part2Len = Math.Max(12, authDataLen - 8);
            // Actually part2 is at least 12 bytes, up to NUL
            byte[] scramblePart2 = new byte[part2Len];
            int copyLen = Math.Min(part2Len, pkt.Length - pos);
            Buffer.BlockCopy(pkt, pos, scramblePart2, 0, copyLen);
            // Find NUL terminator in part2 for actual length
            int actualLen = 0;
            while (actualLen < copyLen && scramblePart2[actualLen] != 0) actualLen++;
            Array.Resize(ref scramblePart2, actualLen);
            pos += copyLen;

            _scramble = new byte[scramblePart1.Length + scramblePart2.Length];
            Buffer.BlockCopy(scramblePart1, 0, _scramble, 0, scramblePart1.Length);
            Buffer.BlockCopy(scramblePart2, 0, _scramble, scramblePart1.Length, scramblePart2.Length);

            // auth plugin name
            if ((caps & 0x00080000) != 0 && pos < pkt.Length)
            {
                _authPlugin = ReadNullTerminatedString(pkt, pos, out pos);
            }
            if (string.IsNullOrEmpty(_authPlugin))
            {
                _authPlugin = "mysql_native_password";
            }
        }

        private byte[] BuildHandshakeResponse(string user, string password, string database)
        {
            // Capability flags
            uint caps = 0x00000001 | // CLIENT_LONG_PASSWORD
                        0x00000200 | // CLIENT_PROTOCOL_41
                        0x00002000 | // CLIENT_TRANSACTIONS
                        0x00008000 | // CLIENT_SECURE_CONNECTION
                        0x00020000 | // CLIENT_MULTI_RESULTS
                        0x00080000 | // CLIENT_PLUGIN_AUTH
                        0x00200000;  // CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA

            if (!string.IsNullOrEmpty(database))
                caps |= 0x00000008; // CLIENT_CONNECT_WITH_DB

            using var ms = new MemoryStream();
            // capabilities (4 bytes LE)
            WriteUInt32LE(ms, caps);
            // max packet size (4 bytes LE)
            WriteUInt32LE(ms, 16777215);
            // character set (utf8mb4_general_ci = 45)
            ms.WriteByte(45);
            // reserved (23 bytes zero)
            for (int i = 0; i < 23; i++) ms.WriteByte(0);
            // username (null terminated)
            WriteNullTerminated(ms, user);

            // auth response
            byte[] authResp;
            if (_authPlugin == "caching_sha2_password")
            {
                authResp = BuildCachingSha2PasswordResponse(password, _scramble, false);
            }
            else
            {
                authResp = BuildNativePasswordResponse(password, _scramble);
            }
            // 1 byte length + data
            ms.WriteByte((byte)authResp.Length);
            ms.Write(authResp, 0, authResp.Length);

            // database (if set)
            if (!string.IsNullOrEmpty(database))
            {
                WriteNullTerminated(ms, database);
            }

            // auth plugin name (null terminated)
            WriteNullTerminated(ms, _authPlugin);

            // client attributes (none for now, skip)
            // Lenenc string for attributes (0 = empty)
            ms.WriteByte(0);

            return ms.ToArray();
        }

        private byte[] BuildNativePasswordResponse(string password, byte[] scramble)
        {
            if (string.IsNullOrEmpty(password))
            {
                return Array.Empty<byte>();
            }

            var pwdBytes = Encoding.UTF8.GetBytes(password);
            // SHA1(password)
            var stage1 = SHA1.HashData(pwdBytes);
            // SHA1(stage1)
            var stage2 = SHA1.HashData(stage1);
            // SHA1(scramble + stage2)
            var combined = new byte[scramble.Length + stage2.Length];
            Buffer.BlockCopy(scramble, 0, combined, 0, scramble.Length);
            Buffer.BlockCopy(stage2, 0, combined, scramble.Length, stage2.Length);
            var stage3 = SHA1.HashData(combined);

            // result = stage1 XOR stage3
            var result = new byte[stage1.Length];
            for (int i = 0; i < stage1.Length; i++)
            {
                result[i] = (byte)(stage1[i] ^ stage3[i]);
            }
            return result;
        }

        private byte[] BuildCachingSha2PasswordResponse(string password, byte[] scramble, bool isFast)
        {
            if (string.IsNullOrEmpty(password))
            {
                return Array.Empty<byte>();
            }
            var pwdBytes = Encoding.UTF8.GetBytes(password);
            // digest1 = SHA256(password)
            var digest1 = SHA256.HashData(pwdBytes);
            // digest2 = SHA256(digest1)
            var digest2 = SHA256.HashData(digest1);
            // digest3 = SHA256(digest2 + scramble)
            var combined = new byte[digest2.Length + scramble.Length];
            Buffer.BlockCopy(digest2, 0, combined, 0, digest2.Length);
            Buffer.BlockCopy(scramble, 0, combined, digest2.Length, scramble.Length);
            var digest3 = SHA256.HashData(combined);

            var result = new byte[digest1.Length];
            for (int i = 0; i < digest1.Length; i++)
            {
                result[i] = (byte)(digest1[i] ^ digest3[i]);
            }
            return result;
        }

        private byte[] RsaEncryptPassword(string password, byte[] scramble, string pemPublicKey)
        {
            // password XOR scramble
            var pwdBytes = Encoding.UTF8.GetBytes(password);
            // Append NUL
            var xorData = new byte[pwdBytes.Length + 1];
            Buffer.BlockCopy(pwdBytes, 0, xorData, 0, pwdBytes.Length);
            xorData[xorData.Length - 1] = 0;

            for (int i = 0; i < xorData.Length && i < scramble.Length; i++)
            {
                xorData[i] ^= scramble[i];
            }

            using var rsa = new System.Security.Cryptography.RSACryptoServiceProvider();
            rsa.ImportFromPem(pemPublicKey.ToCharArray());
            var encrypted = rsa.Encrypt(xorData, false); // PKCS#1 v1.5
            return encrypted;
        }

        // ---- Packet I/O ----

        private async Task<byte[]> ReadPacketAsync()
        {
            var header = new byte[4];
            int n = 0;
            while (n < 4)
            {
                int r = await _stream!.ReadAsync(header.AsMemory(n, 4 - n));
                if (r == 0) throw new EndOfStreamException("MySQL connection closed");
                n += r;
            }
            int len = header[0] | (header[1] << 8) | (header[2] << 16);
            _seq = header[3];

            var payload = new byte[len];
            n = 0;
            while (n < len)
            {
                int r = await _stream!.ReadAsync(payload.AsMemory(n, len - n));
                if (r == 0) throw new EndOfStreamException("MySQL connection closed");
                n += r;
            }
            return payload;
        }

        private async Task SendPacketAsync(byte[] payload)
        {
            int len = payload.Length;
            var header = new byte[4];
            header[0] = (byte)(len & 0xFF);
            header[1] = (byte)((len >> 8) & 0xFF);
            header[2] = (byte)((len >> 16) & 0xFF);
            header[3] = (byte)(++_seq & 0xFF);

            await _stream!.WriteAsync(header);
            if (len > 0)
                await _stream!.WriteAsync(payload);
            await _stream!.FlushAsync();
        }

        private async Task SendCommandAsync(byte command, byte[] arg)
        {
            _seq = -1; // 新命令序列（COM_INIT_DB/COM_QUERY）的首包 seq 必须从 0 重新开始
            var payload = new byte[1 + arg.Length];
            payload[0] = command;
            Buffer.BlockCopy(arg, 0, payload, 1, arg.Length);
            await SendPacketAsync(payload);
        }

        // ---- Protocol helpers ----

        private static string ReadNullTerminatedString(byte[] data, int start, out int end)
        {
            int i = start;
            while (i < data.Length && data[i] != 0) i++;
            end = i + 1; // skip NUL
            return Encoding.UTF8.GetString(data, start, i - start);
        }

        private static void WriteNullTerminated(Stream ms, string s)
        {
            var bytes = Encoding.UTF8.GetBytes(s);
            ms.Write(bytes, 0, bytes.Length);
            ms.WriteByte(0);
        }

        private static void WriteUInt32LE(Stream ms, uint value)
        {
            ms.WriteByte((byte)(value & 0xFF));
            ms.WriteByte((byte)((value >> 8) & 0xFF));
            ms.WriteByte((byte)((value >> 16) & 0xFF));
            ms.WriteByte((byte)((value >> 24) & 0xFF));
        }

        /// <summary>读取长度编码整数，返回 value 和 newPos</summary>
        private static ulong ReadLenEnc(byte[] data, int pos, out int newPos)
        {
            if (pos >= data.Length) { newPos = pos; return 0; }
            byte b = data[pos++];
            if (b < 0xFB)
            {
                newPos = pos;
                return b;
            }
            if (b == 0xFB) { newPos = pos; return ulong.MaxValue; } // NULL
            if (b == 0xFC)
            {
                ulong val = data[pos] | ((ulong)data[pos + 1] << 8);
                newPos = pos + 2;
                return val;
            }
            if (b == 0xFD)
            {
                ulong val = data[pos] | ((ulong)data[pos + 1] << 8) | ((ulong)data[pos + 2] << 16);
                newPos = pos + 3;
                return val;
            }
            if (b == 0xFE)
            {
                ulong val = data[pos] | ((ulong)data[pos + 1] << 8) | ((ulong)data[pos + 2] << 16) | ((ulong)data[pos + 3] << 24) |
                            ((ulong)data[pos + 4] << 32) | ((ulong)data[pos + 5] << 40) | ((ulong)data[pos + 6] << 48) | ((ulong)data[pos + 7] << 56);
                newPos = pos + 8;
                return val;
            }
            newPos = pos;
            return 0;
        }

        private static (int Code, string Message) ParseError(byte[] pkt)
        {
            int code = pkt[1] | (pkt[2] << 8);
            int pos = 3;
            if (pkt.Length > pos + 5 && pkt[pos] == '#')
            {
                pos += 6; // skip sql state
            }
            string msg = Encoding.UTF8.GetString(pkt, pos, pkt.Length - pos);
            return (code, msg);
        }

        private ColumnDef ParseColumnDef(byte[] pkt)
        {
            // catalog, schema, table, org_table, name, org_name, fixed_length, charset, column_length, type, flags, decimals
            int pos = 0;
            ReadLenEncString(pkt, ref pos); // catalog
            ReadLenEncString(pkt, ref pos); // schema
            ReadLenEncString(pkt, ref pos); // table
            ReadLenEncString(pkt, ref pos); // org_table
            var name = ReadLenEncString(pkt, ref pos); // name
            ReadLenEncString(pkt, ref pos); // org_name
            pos++; // 0x0c
            int charset = pkt[pos] | (pkt[pos + 1] << 8);
            pos += 2; // charset
            pos += 4; // column_length
            byte colType = pkt[pos++];
            pos += 2; // flags
            pos += 1; // decimals
            pos += 2; // filler

            return new ColumnDef { Name = name, Type = colType, Charset = charset };
        }

        private static string ReadLenEncString(byte[] data, ref int pos)
        {
            if (pos >= data.Length) return "";
            byte b = data[pos];
            if (b == 0xFB) { pos++; return ""; } // NULL
            int len;
            if (b < 0xFB)
            {
                len = b;
                pos++;
            }
            else if (b == 0xFC)
            {
                len = data[pos + 1] | (data[pos + 2] << 8);
                pos += 3;
            }
            else if (b == 0xFD)
            {
                len = data[pos + 1] | (data[pos + 2] << 8) | (data[pos + 3] << 16);
                pos += 4;
            }
            else if (b == 0xFE)
            {
                len = (int)(data[pos + 1] | ((ulong)data[pos + 2] << 8) | ((ulong)data[pos + 3] << 16) | ((ulong)data[pos + 4] << 24) |
                            ((ulong)data[pos + 5] << 32) | ((ulong)data[pos + 6] << 40) | ((ulong)data[pos + 7] << 48) | ((ulong)data[pos + 8] << 56));
                pos += 9;
            }
            else
            {
                pos++;
                return "";
            }

            var result = Encoding.UTF8.GetString(data, pos, len);
            pos += len;
            return result;
        }

        private Dictionary<string, object?> ParseRow(byte[] pkt, List<ColumnDef> columns, bool preserveBinary)
        {
            var row = new Dictionary<string, object?>();
            int pos = 0;
            for (int i = 0; i < columns.Count; i++)
            {
                byte b = pkt[pos];
                object? val;
                if (b == 0xFB)
                {
                    val = null;
                    pos++;
                }
                else
                {
                    // 解析 lenenc 长度前缀
                    int len;
                    if (b < 0xFB)
                    {
                        len = b;
                        pos++;
                    }
                    else if (b == 0xFC)
                    {
                        len = pkt[pos + 1] | (pkt[pos + 2] << 8);
                        pos += 3;
                    }
                    else if (b == 0xFD)
                    {
                        len = pkt[pos + 1] | (pkt[pos + 2] << 8) | (pkt[pos + 3] << 16);
                        pos += 4;
                    }
                    else if (b == 0xFE)
                    {
                        len = (int)(pkt[pos + 1] | ((ulong)pkt[pos + 2] << 8) | ((ulong)pkt[pos + 3] << 16) | ((ulong)pkt[pos + 4] << 24) |
                                        ((ulong)pkt[pos + 5] << 32) | ((ulong)pkt[pos + 6] << 40) | ((ulong)pkt[pos + 7] << 48) | ((ulong)pkt[pos + 8] << 56));
                        pos += 9;
                    }
                    else
                    {
                        val = "";
                        pos++;
                        row[columns[i].Name] = val;
                        continue;
                    }

                    if (preserveBinary && columns[i].Charset == 63 && IsBinaryType(columns[i].Type))
                    {
                        // binary 列（BLOB/VARBINARY/BINARY 等）：保留原始字节，供导出走 X'hex'，避免 UTF-8 解码产生 NUL/乱码
                        // 注意：MySQL 文本协议中数值/时间列的 charset 也是 63，必须叠加类型判定，否则普通值会被误判为二进制
                        var raw = new byte[len];
                        Buffer.BlockCopy(pkt, pos, raw, 0, len);
                        val = raw;
                    }
                    else
                    {
                        val = Encoding.UTF8.GetString(pkt, pos, len);
                    }

                    pos += len;
                }

                row[columns[i].Name] = val;
            }

            return row;
        }

        /// <summary>是否 MySQL 二进制列类型（BLOB 家族 249-252；253/254 为 VARBINARY/BINARY，需配合 charset=63 判定）</summary>
        private static bool IsBinaryType(byte t)
        {
            return t == 249 || t == 250 || t == 251 || t == 252 || t == 253 || t == 254;
        }

        private class ColumnDef
        {
            public string Name = "";
            public byte Type;
            public int Charset;
        }

        public async ValueTask DisposeAsync()
        {
            if (_stream is not null)
            {
                // Send COM_QUIT
                try { await SendPacketAsync(new byte[] { 0x01 }); } catch { }
                _stream.Dispose();
            }
            _tcp?.Dispose();
        }
    }
}