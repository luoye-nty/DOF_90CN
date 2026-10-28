using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Threading.Tasks;
using DNFLogin.Services;
using Microsoft.Data.Sqlite;

namespace DNFLogin.Gm
{
    /// <summary>
    /// GM 工具数据库访问封装：MySQL（MySqlClient）/ SQLite（Microsoft.Data.Sqlite）按 Mode 分流。
    /// 单连接串行执行，支持 BEGIN/COMMIT/ROLLBACK 事务与 X'hex' 二进制写入。
    /// </summary>
    public sealed class GmDatabaseService : IAsyncDisposable
    {
        private MySqlClient? _client;
        private SqliteConnection? _sqlite;
        private GmSettings? _settings;
        private bool _inTransaction;

        public bool IsConnected => _client is not null || _sqlite is not null;

        public bool IsSqlite => string.Equals(_settings?.Mode, "sqlite", StringComparison.OrdinalIgnoreCase);

        /// <summary>解析 SQLite 库绝对路径：相对路径基于服务端 runtime 目录（instance.json 所在层级）</summary>
        public static string ResolveSqlitePath(GmSettings settings)
        {
            var p = (settings.DatabasePath ?? "").Trim();
            if (p.Length == 0)
            {
                p = "data/dnf90.db";
            }
            if (Path.IsPathRooted(p))
            {
                return Path.GetFullPath(p);
            }
            // InstancePath = {root}\runtime\config\instance.json → root = 上三级目录
            var instanceFile = (settings.InstancePath ?? "").Trim();
            if (instanceFile.Length > 0 && File.Exists(instanceFile))
            {
                var root = Path.GetDirectoryName(Path.GetDirectoryName(Path.GetDirectoryName(instanceFile)));
                if (!string.IsNullOrWhiteSpace(root))
                {
                    return Path.GetFullPath(Path.Combine(root, "runtime", p));
                }
            }
            return Path.GetFullPath(p);
        }

        public async Task ConnectAsync(GmSettings settings)
        {
            _settings = settings;
            _inTransaction = false;
            if (string.Equals(settings.Mode, "sqlite", StringComparison.OrdinalIgnoreCase))
            {
                var path = ResolveSqlitePath(settings);
                if (!File.Exists(path))
                {
                    throw new InvalidOperationException($"SQLite 数据库文件不存在：{path}");
                }
                var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                await conn.OpenAsync().ConfigureAwait(false);
                _sqlite = conn;
                _client = null;
                return;
            }

            var c = new MySqlClient();
            await c.ConnectAsync(settings.Host, settings.Port, settings.User, settings.Password, settings.Database);
            _client = c;
            _sqlite = null;
        }

        public async Task CloseAsync()
        {
            if (_sqlite is not null)
            {
                try { await _sqlite.DisposeAsync(); } catch { }
                _sqlite = null;
            }
            if (_client is not null)
            {
                try { await _client.DisposeAsync(); } catch { }
                _client = null;
            }
            _inTransaction = false;
        }

        /// <summary>确保连接存活，断开时按当前配置重连</summary>
        private async Task EnsureConnectedAsync()
        {
            if (_client is not null || _sqlite is not null)
            {
                return;
            }
            if (_settings is null)
            {
                throw new InvalidOperationException("尚未配置数据库连接");
            }
            await ConnectAsync(_settings);
        }

        /// <summary>执行查询并返回结果集（列名忽略大小写）</summary>
        public async Task<List<Dictionary<string, object?>>> QueryAsync(string sql)
        {
            await EnsureConnectedAsync();
            if (_sqlite is not null)
            {
                return await SqliteQueryAsync(sql, false).ConfigureAwait(false);
            }
            return await _client!.QueryAsync(sql);
        }

        /// <summary>执行查询，二进制列保留 byte[]</summary>
        public async Task<List<Dictionary<string, object?>>> QueryBinaryAsync(string sql)
        {
            await EnsureConnectedAsync();
            if (_sqlite is not null)
            {
                return await SqliteQueryAsync(sql, true).ConfigureAwait(false);
            }
            return await _client!.QueryAsync(sql, true);
        }

        private async Task<List<Dictionary<string, object?>>> SqliteQueryAsync(string sql, bool binary)
        {
            var rows = new List<Dictionary<string, object?>>();
            await using var cmd = _sqlite!.CreateCommand();
            cmd.CommandText = sql;
            await using var reader = await cmd.ExecuteReaderAsync().ConfigureAwait(false);
            var names = new string[reader.FieldCount];
            for (var i = 0; i < reader.FieldCount; i++)
            {
                names[i] = reader.GetName(i);
            }
            while (await reader.ReadAsync().ConfigureAwait(false))
            {
                var row = new Dictionary<string, object?>(StringComparer.OrdinalIgnoreCase);
                for (var i = 0; i < reader.FieldCount; i++)
                {
                    row[names[i]] = reader.IsDBNull(i) ? null : reader.GetValue(i);
                }
                rows.Add(row);
            }
            return rows;
        }

        /// <summary>执行写操作（UPDATE/INSERT/DELETE 等）</summary>
        public Task ExecAsync(string sql) => QueryAsync(sql);

        public async Task BeginAsync()
        {
            await ExecAsync("BEGIN");
            _inTransaction = true;
        }

        public async Task CommitAsync()
        {
            await ExecAsync("COMMIT");
            _inTransaction = false;
        }

        public async Task RollbackAsync()
        {
            if (_inTransaction)
            {
                try { await ExecAsync("ROLLBACK"); } catch { }
                _inTransaction = false;
            }
        }

        public async ValueTask DisposeAsync()
        {
            await CloseAsync();
        }

        // ── SQL 值工具 ─────────────────────────────────────────

        /// <summary>SQL 字符串字面量转义（仅转单引号，MySQL 与 SQLite 语义一致；Go 版直接拼接 SQL，此处等价处理 '）</summary>
        public static string Esc(string? value)
        {
            if (string.IsNullOrEmpty(value))
            {
                return "''";
            }
            return "'" + value.Replace("'", "''") + "'";
        }

        /// <summary>数值字面量（null → NULL）</summary>
        public static string Num(long? value) => value is null ? "NULL" : value.Value.ToString(CultureInfo.InvariantCulture);

        public static string Num(int? value) => value is null ? "NULL" : value.Value.ToString(CultureInfo.InvariantCulture);

        /// <summary>datetime 字面量（UTC，与 Go 版存 UTC 一致）</summary>
        public static string Utc(DateTime value) => "'" + value.ToUniversalTime().ToString("yyyy-MM-dd HH:mm:ss", CultureInfo.InvariantCulture) + "'";

        /// <summary>byte[] 字面量（X'hex'，供 raw 物品写入）</summary>
        public static string Hex(byte[] value) => "X'" + Convert.ToHexString(value) + "'";

        /// <summary>从结果行取字符串（NULL → ""）</summary>
        public static string Str(Dictionary<string, object?> row, string column)
        {
            foreach (var kv in row)
            {
                if (string.Equals(kv.Key, column, StringComparison.OrdinalIgnoreCase))
                {
                    return kv.Value is null ? "" : Convert.ToString(kv.Value, CultureInfo.InvariantCulture) ?? "";
                }
            }
            return "";
        }

        /// <summary>从结果行取 long（NULL/空 → 0）</summary>
        public static long Lng(Dictionary<string, object?> row, string column)
        {
            var s = Str(row, column);
            if (long.TryParse(s, NumberStyles.Any, CultureInfo.InvariantCulture, out var v))
            {
                return v;
            }
            return 0;
        }

        /// <summary>从结果行取 int</summary>
        public static int Int(Dictionary<string, object?> row, string column) => (int)Lng(row, column);

        /// <summary>从结果行取可空 int（NULL → null）</summary>
        public static int? NullInt(Dictionary<string, object?> row, string column)
        {
            foreach (var kv in row)
            {
                if (string.Equals(kv.Key, column, StringComparison.OrdinalIgnoreCase))
                {
                    if (kv.Value is null)
                    {
                        return null;
                    }
                    var s = Convert.ToString(kv.Value, CultureInfo.InvariantCulture) ?? "";
                    return int.TryParse(s, NumberStyles.Any, CultureInfo.InvariantCulture, out var v) ? v : 0;
                }
            }
            return null;
        }

        /// <summary>从结果行取可空 DateTime（NULL → null）</summary>
        public static DateTime? NullDateTime(Dictionary<string, object?> row, string column)
        {
            var s = Str(row, column);
            if (string.IsNullOrEmpty(s))
            {
                return null;
            }
            if (DateTime.TryParse(s, CultureInfo.InvariantCulture, DateTimeStyles.AssumeUniversal, out var dt))
            {
                return dt.ToUniversalTime();
            }
            return null;
        }

        /// <summary>从结果行取原始 byte[]（无则 null）</summary>
        public static byte[]? Raw(Dictionary<string, object?> row, string column)
        {
            foreach (var kv in row)
            {
                if (string.Equals(kv.Key, column, StringComparison.OrdinalIgnoreCase) && kv.Value is byte[] b)
                {
                    return b;
                }
            }
            return null;
        }
    }
}
