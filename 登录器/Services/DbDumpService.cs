using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.IO.Compression;
using System.Linq;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Data.Sqlite;

namespace DNFLogin.Services
{
    // ==================== 数据模型 ====================

    /// <summary>备份元信息（zip 内 meta.json 结构）</summary>
    public sealed class DbDumpMeta
    {
        /// <summary>来源库类型："mysql" / "sqlite"</summary>
        public string SourceType { get; set; } = "";

        /// <summary>备份格式版本</summary>
        public string Version { get; set; } = "1.0";

        /// <summary>导出时间（yyyy-MM-dd HH:mm:ss）</summary>
        public string ExportedAt { get; set; } = "";

        /// <summary>各表元信息（含原始 CREATE TABLE，用于跨库保真建表）</summary>
        public List<DbDumpTableMeta> Tables { get; set; } = new();
    }

    /// <summary>单表元信息</summary>
    public sealed class DbDumpTableMeta
    {
        /// <summary>表名（裸名，不带引号）</summary>
        public string Name { get; set; } = "";

        /// <summary>原始 CREATE TABLE 语句（来源库原样，最保真）</summary>
        public string SourceDdl { get; set; } = "";

        /// <summary>导出的行数</summary>
        public int RowCount { get; set; }
    }

    /// <summary>导出结果</summary>
    public sealed class DbDumpExportResult
    {
        public int TableCount { get; set; }
        public long RowCount { get; set; }
        public string ZipPath { get; set; } = "";
    }

    /// <summary>导入结果统计</summary>
    public sealed class DbDumpImportResult
    {
        public int Succeeded { get; set; }
        public int Failed { get; set; }
        public List<string> FailedTables { get; } = new();
        public List<string> SkippedTables { get; } = new();
    }

    /// <summary>冲突处理策略</summary>
    public enum DbDumpConflictAction
    {
        Cancel,
        Overwrite, // 覆盖：DROP TABLE IF EXISTS 重建
        Skip,      // 跳过该表
        Rename     // 改名导入（表名加时间后缀）
    }

    /// <summary>
    /// 数据库全库导出 / 导入服务。
    /// 导出：逐表生成通用 SQL（DROP + CREATE + INSERT，字符串统一 '' 转义、二进制 X'hex'），
    ///       连同 meta.json（含每表原始 DDL）打包为 zip。
    /// 导入：校验 zip → 读取 meta → 类型映射/DDL 转换 → 逐表事务导入（MySQL 结构语句在事务外、
    ///       INSERT 在事务内；SQLite 整表一个事务）。
    /// 通用 SQL 兼容性说明：MySQL 连接后设置会话 sql_mode 追加 NO_BACKSLASH_ESCAPES，
    ///       使反斜杠按普通字符处理，与 SQLite 默认行为一致，因此字符串转义统一为 '' 双写单引号。
    /// </summary>
    public static class DbDumpService
    {
        private const string MetaEntryName = "meta.json";
        private const string SqlEntryName = "dump.sql";

        // zip 解析缓存：同一备份多次逐表导入时避免重复读取/解析
        private static string? _cacheZipPath;
        private static DbDumpMeta? _cacheMeta;
        private static Dictionary<string, string>? _cacheSections;

        /// <summary>是否 SQLite 模式（与服务端共用 dnf90.db）</summary>
        public static bool IsSqlite(DNFLogin.AppSettings s)
        {
            return string.Equals(s.Mode, "sqlite", StringComparison.OrdinalIgnoreCase);
        }

        // ==================== 导出 ====================

        /// <summary>
        /// 全库导出：连接当前配置数据库，逐表生成 dump.sql + meta.json 并打包为 zip。
        /// </summary>
        /// <param name="s">当前配置</param>
        /// <param name="zipPath">zip 输出路径</param>
        /// <param name="log">进度回调（可能在工作线程调用）</param>
        /// <param name="ct">取消令牌</param>
        public static async Task<DbDumpExportResult> ExportAsync(
            DNFLogin.AppSettings s, string zipPath, Action<string>? log = null, CancellationToken ct = default)
        {
            var isSqlite = IsSqlite(s);
            var sql = new StringBuilder();
            var meta = new DbDumpMeta
            {
                SourceType = isSqlite ? "sqlite" : "mysql",
                Version = "1.0",
                ExportedAt = DateTime.Now.ToString("yyyy-MM-dd HH:mm:ss")
            };
            long totalRows = 0;

            if (isSqlite)
            {
                var dbPath = DatabaseService.ResolveSqlitePath(s);
                if (!File.Exists(dbPath))
                {
                    throw new InvalidOperationException("SQLite 数据库文件不存在：" + dbPath);
                }

                using var conn = new SqliteConnection("Data Source=" + dbPath + ";Mode=ReadWrite");
                conn.Open();

                var tables = new List<(string Name, string Ddl)>();
                using (var cmd = conn.CreateCommand())
                {
                    cmd.CommandText = "SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'";
                    using var rd = cmd.ExecuteReader();
                    while (rd.Read())
                    {
                        var name = rd.GetString(0);
                        var ddl = rd.IsDBNull(1) ? "" : rd.GetString(1);
                        if (!string.IsNullOrWhiteSpace(ddl))
                        {
                            tables.Add((name, ddl));
                        }
                    }
                }

                foreach (var (name, ddl) in tables)
                {
                    ct.ThrowIfCancellationRequested();
                    log?.Invoke("导出表 " + name + " ...");
                    var cols = GetSqliteColumns(conn, name);
                    var rows = ReadSqliteRows(conn, name);
                    AppendTableSection(sql, name, ddl, cols, rows);
                    totalRows += rows.Count;
                    meta.Tables.Add(new DbDumpTableMeta { Name = name, SourceDdl = ddl, RowCount = rows.Count });
                }
            }
            else
            {
                await using var client = new MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 10);
                // 反斜杠按普通字符处理，保证 dump.sql 字符串转义规则与 SQLite 一致
                await client.QueryAsync("SET SESSION sql_mode = CONCAT(@@sql_mode, ',NO_BACKSLASH_ESCAPES')");

                var tableRows = await client.QueryAsync(
                    "SELECT table_name AS tbl_name FROM information_schema.tables WHERE table_schema = DATABASE()");
                foreach (var row in tableRows)
                {
                    var name = row["tbl_name"]?.ToString() ?? "";
                    if (name.Length == 0)
                    {
                        continue;
                    }

                    ct.ThrowIfCancellationRequested();
                    log?.Invoke("导出表 " + name + " ...");

                    var ddlRows = await client.QueryAsync("SHOW CREATE TABLE " + QuoteIdent(name));
                    var ddl = ddlRows.Count > 0 && ddlRows[0].TryGetValue("Create Table", out var ddlVal)
                        ? ddlVal?.ToString() ?? ""
                        : "";

                    var cols = await GetMySqlColumns(client, name);
                    // preserveBinary: 二进制列（BLOB/VARBINARY 等）返回 byte[]，导出时走 X'hex'，避免被当作文本解码产生 NUL/乱码破坏 SQL 字面量
                    var data = await client.QueryAsync("SELECT * FROM " + QuoteIdent(name), true);
                    AppendTableSection(sql, name, ddl, cols, data);
                    totalRows += data.Count;
                    meta.Tables.Add(new DbDumpTableMeta { Name = name, SourceDdl = ddl, RowCount = data.Count });
                }
            }

            // 打包 zip（dump.sql + meta.json）
            var fullZip = Path.GetFullPath(zipPath);
            var dir = Path.GetDirectoryName(fullZip);
            if (!string.IsNullOrEmpty(dir))
            {
                Directory.CreateDirectory(dir);
            }

            using (var fs = File.Create(fullZip))
            {
                using var zip = new ZipArchive(fs, ZipArchiveMode.Create);
                var sqlEntry = zip.CreateEntry(SqlEntryName, CompressionLevel.Optimal);
                using (var sw = new StreamWriter(sqlEntry.Open(), new UTF8Encoding(false)))
                {
                    sw.Write(sql.ToString());
                }

                var metaEntry = zip.CreateEntry(MetaEntryName, CompressionLevel.Optimal);
                using (var sw = new StreamWriter(metaEntry.Open(), new UTF8Encoding(false)))
                {
                    sw.Write(JsonSerializer.Serialize(meta, new JsonSerializerOptions { WriteIndented = true }));
                }
            }

            log?.Invoke("打包完成：" + fullZip);
            return new DbDumpExportResult { TableCount = meta.Tables.Count, RowCount = totalRows, ZipPath = fullZip };
        }

        /// <summary>追加一张表的导出段落（DROP + CREATE + INSERT）</summary>
        private static void AppendTableSection(
            StringBuilder sql, string table, string ddl, List<string> cols, IReadOnlyList<object?[]> rows)
        {
            sql.Append("-- TABLE: ").Append(table).Append('\n');
            sql.Append("DROP TABLE IF EXISTS ").Append(QuoteIdent(table)).Append(";\n");
            var create = ddl.Trim().TrimEnd(';').Trim();
            sql.Append(create).Append(";\n");
            if (cols.Count > 0)
            {
                var colList = string.Join(",", cols.Select(QuoteIdent));
                foreach (var row in rows)
                {
                    var values = new string[row.Length];
                    for (int i = 0; i < row.Length; i++)
                    {
                        values[i] = SqlLiteral(row[i]);
                    }

                    sql.Append("INSERT INTO ").Append(QuoteIdent(table))
                       .Append(" (").Append(colList).Append(") VALUES (")
                       .Append(string.Join(",", values)).Append(");\n");
                }
            }
        }

        /// <summary>追加一张表的导出段落（MySQL 行数据版本：值来自 Dictionary 且按列顺序取值）</summary>
        private static void AppendTableSection(
            StringBuilder sql, string table, string ddl, List<string> cols, IReadOnlyList<Dictionary<string, object?>> rows)
        {
            sql.Append("-- TABLE: ").Append(table).Append('\n');
            sql.Append("DROP TABLE IF EXISTS ").Append(QuoteIdent(table)).Append(";\n");
            var create = ddl.Trim().TrimEnd(';').Trim();
            sql.Append(create).Append(";\n");
            if (cols.Count > 0)
            {
                var colList = string.Join(",", cols.Select(QuoteIdent));
                foreach (var row in rows)
                {
                    var values = new string[cols.Count];
                    for (int i = 0; i < cols.Count; i++)
                    {
                        values[i] = row.TryGetValue(cols[i], out var v) ? SqlLiteral(v) : "NULL";
                    }

                    sql.Append("INSERT INTO ").Append(QuoteIdent(table))
                       .Append(" (").Append(colList).Append(") VALUES (")
                       .Append(string.Join(",", values)).Append(");\n");
                }
            }
        }

        /// <summary>生成通用 SQL 字面量：NULL / X'hex'（二进制）/ 数字原样 / 字符串 '' 转义</summary>
        private static string SqlLiteral(object? v)
        {
            if (v is null || v is DBNull)
            {
                return "NULL";
            }

            if (v is byte[] b)
            {
                return "X'" + Convert.ToHexString(b) + "'";
            }

            if (v is bool bo)
            {
                return bo ? "1" : "0";
            }

            if (v is long l)
            {
                return l.ToString(CultureInfo.InvariantCulture);
            }

            if (v is int i)
            {
                return i.ToString(CultureInfo.InvariantCulture);
            }

            if (v is double d)
            {
                return d.ToString("R", CultureInfo.InvariantCulture);
            }

            if (v is float f)
            {
                return f.ToString("R", CultureInfo.InvariantCulture);
            }

            if (v is decimal m)
            {
                return m.ToString(CultureInfo.InvariantCulture);
            }

            if (v is DateTime dt)
            {
                return "'" + dt.ToString("yyyy-MM-dd HH:mm:ss.fffffff") + "'";
            }

            // 字符串：含 NUL（\0）或 UTF-8 解码失败标记（U+FFFD）时按 UTF-8 字节以 X'hex' 输出。
            // 原因：SQLite tokenizer 遇字符串字面量内 NUL 会按 C 语义截断，残余乱码成为非法 token（unrecognized token）；
            // X'hex' 在 MySQL 与 SQLite 均受支持，字节语义保真。
            var sv = v.ToString() ?? "";
            if (sv.IndexOf('\0') >= 0 || sv.IndexOf('\uFFFD') >= 0)
            {
                return "X'" + Convert.ToHexString(Encoding.UTF8.GetBytes(sv)) + "'";
            }

            return "'" + sv.Replace("'", "''") + "'";
        }

        // ==================== SQLite 读取辅助 ====================

        private static List<string> GetSqliteColumns(SqliteConnection conn, string table)
        {
            using var cmd = conn.CreateCommand();
            cmd.CommandText = "SELECT * FROM " + QuoteIdent(table) + " LIMIT 0";
            using var rd = cmd.ExecuteReader();
            var list = new List<string>();
            for (int i = 0; i < rd.FieldCount; i++)
            {
                list.Add(rd.GetName(i));
            }

            return list;
        }

        private static List<object?[]> ReadSqliteRows(SqliteConnection conn, string table)
        {
            using var cmd = conn.CreateCommand();
            cmd.CommandText = "SELECT * FROM " + QuoteIdent(table);
            using var rd = cmd.ExecuteReader();
            var list = new List<object?[]>();
            while (rd.Read())
            {
                var row = new object?[rd.FieldCount];
                for (int i = 0; i < rd.FieldCount; i++)
                {
                    row[i] = rd.IsDBNull(i) ? null : rd.GetValue(i);
                }

                list.Add(row);
            }

            return list;
        }

        // ==================== MySQL 读取辅助 ====================

        private static async Task<List<string>> GetMySqlColumns(MySqlClient client, string table)
        {
            var rows = await client.QueryAsync(
                "SELECT column_name AS col_name FROM information_schema.columns " +
                "WHERE table_schema = DATABASE() AND table_name = '" + table.Replace("'", "''") + "' " +
                "ORDER BY ordinal_position");
            return rows.Select(r => r["col_name"]?.ToString() ?? "")
                       .Where(n => n.Length > 0)
                       .ToList();
        }

        // ==================== 导入 ====================

        /// <summary>
        /// 校验 zip 并读取元信息（同时解析 dump.sql 到缓存）。
        /// </summary>
        /// <returns>meta；失败时返回 null 且 error 非空</returns>
        public static DbDumpMeta? ReadMeta(string zipPath, out string error)
        {
            error = "";
            try
            {
                if (!File.Exists(zipPath))
                {
                    error = "备份文件不存在：" + zipPath;
                    return null;
                }

                using var fs = File.OpenRead(zipPath);
                using var zip = new ZipArchive(fs, ZipArchiveMode.Read);
                var metaEntry = zip.GetEntry(MetaEntryName);
                var sqlEntry = zip.GetEntry(SqlEntryName);
                if (metaEntry is null || sqlEntry is null)
                {
                    error = "无效的备份文件：缺少 meta.json 或 dump.sql";
                    return null;
                }

                string metaJson;
                using (var sr = new StreamReader(metaEntry.Open(), Encoding.UTF8))
                {
                    metaJson = sr.ReadToEnd();
                }

                DbDumpMeta? meta;
                try
                {
                    meta = JsonSerializer.Deserialize<DbDumpMeta>(metaJson);
                }
                catch
                {
                    meta = null;
                }

                if (meta is null || string.IsNullOrEmpty(meta.SourceType))
                {
                    error = "meta.json 格式不正确";
                    return null;
                }

                _cacheZipPath = Path.GetFullPath(zipPath);
                _cacheMeta = meta;
                _cacheSections = ParseSections(sqlEntry);
                return meta;
            }
            catch (Exception)
            {
                error = "读取备份文件失败，文件可能已损坏或不是本登录器导出的备份。";
                return null;
            }
        }

        /// <summary>解析 dump.sql 为 表名 → 表段落（含 DROP/CREATE/INSERT）</summary>
        private static Dictionary<string, string> ParseSections(ZipArchiveEntry sqlEntry)
        {
            string content;
            using (var sr = new StreamReader(sqlEntry.Open(), Encoding.UTF8))
            {
                content = sr.ReadToEnd();
            }

            var map = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            const string marker = "-- TABLE: ";

            // 定位所有表头标记行，按标记位置把全文切成 表名 → 表段落。
            // 不能按 '\n' 拆行：INSERT 字符串值内可能含换行，按行解析会破坏语句完整性。
            var boundaries = new List<(string Name, int Start)>();
            int searchPos = 0;
            while (true)
            {
                int idx = content.IndexOf(marker, searchPos, StringComparison.Ordinal);
                if (idx < 0)
                {
                    break;
                }

                int lineEnd = content.IndexOf('\n', idx);
                if (lineEnd < 0)
                {
                    break;
                }

                var name = content.Substring(idx + marker.Length, lineEnd - idx - marker.Length).Trim();
                boundaries.Add((name, lineEnd + 1));
                searchPos = lineEnd + 1;
            }

            for (int i = 0; i < boundaries.Count; i++)
            {
                var end = i + 1 < boundaries.Count ? boundaries[i + 1].Start : content.Length;
                var section = content.Substring(boundaries[i].Start, end - boundaries[i].Start).TrimEnd('\r', '\n');
                map[boundaries[i].Name] = section;
            }

            return map;
        }

        /// <summary>
        /// 导入单表（每表一个事务）。
        /// </summary>
        /// <param name="s">当前配置（目标库）</param>
        /// <param name="zipPath">备份 zip 路径</param>
        /// <param name="tableName">备份中的原始表名</param>
        /// <param name="targetTableName">导入到目标库的表名（改名策略时不同）</param>
        /// <param name="dropExisting">冲突覆盖时先 DROP 重建</param>
        /// <returns>(成功, 错误信息)</returns>
        public static async Task<(bool Success, string Error)> ImportTableAsync(
            DNFLogin.AppSettings s, string zipPath, string tableName, string targetTableName,
            bool dropExisting, Action<string>? log = null)
        {
            var meta = EnsureCache(zipPath);
            if (meta is null)
            {
                return (false, "备份读取失败（请先调用 ReadMeta 校验）");
            }

            if (_cacheSections is null || !_cacheSections.TryGetValue(tableName, out var section))
            {
                return (false, "备份中不存在表：" + tableName);
            }

            var targetType = IsSqlite(s) ? "sqlite" : "mysql";
            if (!string.Equals(meta.SourceType, targetType, StringComparison.OrdinalIgnoreCase))
            {
                return (false, $"备份类型与目标库不一致：备份为 {meta.SourceType}，目标为 {targetType}，仅支持同类型导入（MySQL→MySQL / SQLite→SQLite）。");
            }

            var stmts = BuildTableStatements(meta, section, tableName, targetTableName, dropExisting, targetType);
            if (stmts.Count == 0)
            {
                return (false, "表段落为空：" + tableName);
            }

            try
            {
                if (IsSqlite(s))
                {
                    return await ImportSqliteTableAsync(s, stmts);
                }

                return await ImportMySqlTableAsync(s, stmts);
            }
            catch (Exception ex)
            {
                return (false, ex.Message);
            }
        }

        /// <summary>SQLite 导入单表：整表一个事务（DDL + INSERT 均可回滚）</summary>
        private static Task<(bool Success, string Error)> ImportSqliteTableAsync(DNFLogin.AppSettings s, List<string> stmts)
        {
            var dbPath = DatabaseService.ResolveSqlitePath(s);
            if (!File.Exists(dbPath))
            {
                return Task.FromResult<(bool, string)>((false, "SQLite 数据库文件不存在：" + dbPath));
            }

            try
            {
                using var conn = new SqliteConnection("Data Source=" + dbPath + ";Mode=ReadWrite");
                conn.Open();
                using var tx = conn.BeginTransaction();
                using (var cmd = conn.CreateCommand())
                {
                    cmd.Transaction = tx;
                    foreach (var sql in stmts)
                    {
                        cmd.CommandText = sql;
                        cmd.ExecuteNonQuery();
                    }
                }

                tx.Commit();
                return Task.FromResult<(bool, string)>((true, ""));
            }
            catch (Exception ex)
            {
                return Task.FromResult<(bool, string)>((false, ex.Message));
            }
        }

        /// <summary>MySQL 导入单表：DROP/CREATE 在事务外（MySQL DDL 隐式提交），INSERT 在事务内可回滚</summary>
        private static async Task<(bool Success, string Error)> ImportMySqlTableAsync(DNFLogin.AppSettings s, List<string> stmts)
        {
            await using var client = new MySqlClient();
            await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 10);
            await client.QueryAsync("SET SESSION sql_mode = CONCAT(@@sql_mode, ',NO_BACKSLASH_ESCAPES')");

            // 结构语句（DROP/CREATE）事务外执行
            foreach (var sql in stmts)
            {
                if (sql.StartsWith("INSERT", StringComparison.OrdinalIgnoreCase))
                {
                    break;
                }

                await client.QueryAsync(sql);
            }

            // 数据语句（INSERT）事务内执行
            var insertStmts = stmts.Where(sq => sq.StartsWith("INSERT", StringComparison.OrdinalIgnoreCase)).ToList();
            if (insertStmts.Count == 0)
            {
                return (true, "");
            }

            await client.QueryAsync("BEGIN");
            try
            {
                foreach (var sql in insertStmts)
                {
                    await client.QueryAsync(sql);
                }

                await client.QueryAsync("COMMIT");
                return (true, "");
            }
            catch (Exception ex)
            {
                try
                {
                    await client.QueryAsync("ROLLBACK");
                }
                catch
                {
                    // 回滚失败不掩盖原始错误
                }

                return (false, ex.Message);
            }
        }

        /// <summary>由表段落构建可执行语句列表（按目标库类型做 DDL 转换、表名替换）</summary>
        private static List<string> BuildTableStatements(
            DbDumpMeta meta, string section, string tableName, string targetTableName,
            bool dropExisting, string targetType)
        {
            var list = new List<string>();
            string? drop = null;
            string? create = null;
            var inserts = new List<string>();

            // 按语句切分（而非按行）：字符串字面量内的分号/换行不会破坏语句边界
            foreach (var stmt in SplitSqlStatements(section))
            {
                var s = stmt.Trim();
                if (s.Length == 0)
                {
                    continue;
                }

                if (s.StartsWith("DROP TABLE", StringComparison.OrdinalIgnoreCase))
                {
                    drop = s;
                }
                else if (s.StartsWith("CREATE TABLE", StringComparison.OrdinalIgnoreCase))
                {
                    create = s;
                }
                else if (s.StartsWith("INSERT INTO", StringComparison.OrdinalIgnoreCase))
                {
                    inserts.Add(s);
                }
            }

            if (dropExisting && drop is not null)
            {
                list.Add(ReplaceTableName(drop, tableName, targetTableName));
            }

            if (create is not null)
            {
                var converted = ConvertCreateTable(create, meta.SourceType, targetType);
                list.Add(ReplaceTableName(converted, tableName, targetTableName));
            }

            foreach (var ins in inserts)
            {
                list.Add(ReplaceTableName(ins, tableName, targetTableName));
            }

            return list;
        }

        /// <summary>
        /// 按语句切分 SQL 文本：以 ';' 为边界，识别单引号字符串与 '' 转义、X'hex' 字面量、
        /// 反引号标识符，跳过字符串内的分号/换行，保证文本值内换行不再破坏 INSERT 切分。
        /// </summary>
        private static IEnumerable<string> SplitSqlStatements(string sql)
        {
            var sb = new StringBuilder();
            int i = 0;
            int n = sql.Length;
            while (i < n)
            {
                char c = sql[i];
                if (c == '\'')
                {
                    // 字符串字面量：支持 '' 转义（含 X'hex' 场景）
                    sb.Append(c);
                    i++;
                    while (i < n)
                    {
                        char d = sql[i];
                        sb.Append(d);
                        i++;
                        if (d == '\'')
                        {
                            if (i < n && sql[i] == '\'')
                            {
                                sb.Append('\'');
                                i++;
                                continue;
                            }

                            break; // 字符串结束
                        }
                    }

                    continue;
                }

                if (c == '`')
                {
                    // MySQL 反引号标识符
                    sb.Append(c);
                    i++;
                    while (i < n && sql[i] != '`')
                    {
                        sb.Append(sql[i]);
                        i++;
                    }

                    if (i < n)
                    {
                        sb.Append('`');
                        i++;
                    }

                    continue;
                }

                if (c == ';')
                {
                    var stmt = sb.ToString();
                    sb.Clear();
                    if (!string.IsNullOrWhiteSpace(stmt))
                    {
                        yield return stmt;
                    }

                    i++;
                    continue;
                }

                sb.Append(c);
                i++;
            }

            var tail = sb.ToString();
            if (!string.IsNullOrWhiteSpace(tail))
            {
                yield return tail;
            }
        }

        /// <summary>替换语句中的表名（统一反引号形式精确替换）</summary>
        private static string ReplaceTableName(string sql, string oldName, string newName)
        {
            if (string.Equals(oldName, newName, StringComparison.OrdinalIgnoreCase))
            {
                return sql;
            }

            var oldQuoted = QuoteIdent(oldName);
            var newQuoted = QuoteIdent(newName);
            return sql.Replace(oldQuoted, newQuoted);
        }

        /// <summary>校验 zip 缓存；未缓存或路径变化时重新读取</summary>
        private static DbDumpMeta? EnsureCache(string zipPath)
        {
            var full = Path.GetFullPath(zipPath);
            if (_cacheZipPath != null && string.Equals(_cacheZipPath, full, StringComparison.OrdinalIgnoreCase)
                && _cacheMeta != null && _cacheSections != null)
            {
                return _cacheMeta;
            }

            var meta = ReadMeta(zipPath, out _);
            return meta;
        }

        // ==================== 类型映射 / DDL 转换 ====================

        /// <summary>
        /// 跨库建表语句转换。
        /// 同类型库直接原样使用（最保真）；跨库时做必要的类型映射与语法裁剪。
        /// </summary>
        internal static string ConvertCreateTable(string sourceDdl, string sourceType, string targetType)
        {
            var ddl = sourceDdl.Trim().TrimEnd(';').Trim();
            if (string.Equals(sourceType, targetType, StringComparison.OrdinalIgnoreCase))
            {
                return NormalizeTableName(ddl);
            }

            if (string.Equals(sourceType, "mysql", StringComparison.OrdinalIgnoreCase)
                && string.Equals(targetType, "sqlite", StringComparison.OrdinalIgnoreCase))
            {
                return ConvertMySqlToSqlite(ddl);
            }

            if (string.Equals(sourceType, "sqlite", StringComparison.OrdinalIgnoreCase)
                && string.Equals(targetType, "mysql", StringComparison.OrdinalIgnoreCase))
            {
                return ConvertSqliteToMySql(ddl);
            }

            return NormalizeTableName(ddl);
        }

        /// <summary>MySQL DDL → SQLite：类型映射 + 移除 MySQL 专属语法（AUTO_INCREMENT/ENGINE/KEY/COMMENT 等）</summary>
        private static string ConvertMySqlToSqlite(string ddl)
        {
            // 去掉表尾选项（ENGINE=... DEFAULT CHARSET=... 等）：从最后一个 ')' 之后截断
            var body = ddl;
            var closeIdx = body.LastIndexOf(')');
            if (closeIdx >= 0)
            {
                body = body.Substring(0, closeIdx + 1);
            }

            var openIdx = body.IndexOf('(');
            if (openIdx < 0)
            {
                return NormalizeTableName(ddl);
            }

            var header = NormalizeTableName(body.Substring(0, openIdx)).TrimEnd();
            var inner = body.Substring(openIdx + 1, body.Length - openIdx - 2);
            var parts = SplitTopLevel(inner, ',');
            var newParts = new List<string>();

            foreach (var part in parts)
            {
                var p = part.Trim();
                if (p.Length == 0)
                {
                    continue;
                }

                var upper = p.ToUpperInvariant();
                if (upper.StartsWith("PRIMARY KEY", StringComparison.Ordinal))
                {
                    newParts.Add(Regex.Replace(p, @"(?i)\s+USING\s+\w+", ""));
                    continue;
                }

                if (upper.StartsWith("UNIQUE KEY", StringComparison.Ordinal))
                {
                    // UNIQUE KEY `name` (`col`) → UNIQUE (`col`)
                    var rest = p.Substring("UNIQUE KEY".Length).Trim();
                    var nameMatch = Regex.Match(rest, @"^`[^`]+`|^""[^""]+""|^\[[^\]]+\]|^[A-Za-z0-9_]+");
                    if (nameMatch.Success)
                    {
                        rest = rest.Substring(nameMatch.Length).Trim();
                    }

                    newParts.Add("UNIQUE " + rest);
                    continue;
                }

                if (upper.StartsWith("KEY", StringComparison.Ordinal)
                    || upper.StartsWith("INDEX", StringComparison.Ordinal)
                    || upper.StartsWith("FULLTEXT", StringComparison.Ordinal)
                    || upper.StartsWith("SPATIAL", StringComparison.Ordinal))
                {
                    // 非唯一索引 SQLite 列内不支持，丢弃
                    continue;
                }

                if (upper.StartsWith("CONSTRAINT", StringComparison.Ordinal))
                {
                    var fkIdx = upper.IndexOf("FOREIGN KEY", StringComparison.Ordinal);
                    if (fkIdx >= 0)
                    {
                        newParts.Add(p.Substring(fkIdx).Trim());
                    }

                    continue;
                }

                if (upper.StartsWith("FOREIGN KEY", StringComparison.Ordinal)
                    || upper.StartsWith("CHECK", StringComparison.Ordinal))
                {
                    newParts.Add(p);
                    continue;
                }

                newParts.Add(ConvertMySqlColumn(p));
            }

            return header + " (\n  " + string.Join(",\n  ", newParts) + "\n)";
        }

        /// <summary>转换单个 MySQL 列定义 → SQLite 兼容列定义</summary>
        private static string ConvertMySqlColumn(string col)
        {
            var s = col;
            // 移除列级字符集/排序规则
            s = Regex.Replace(s, @"(?i)\s+(CHARACTER\s+SET|CHARSET|COLLATE)\s+[`A-Za-z0-9_]+", "");
            // 移除列注释
            s = Regex.Replace(s, @"(?i)\s+COMMENT\s+'([^'\\]|\\.)*'", "");
            // 移除 UNSIGNED / ZEROFILL / AUTO_INCREMENT
            s = Regex.Replace(s, @"(?i)\s+UNSIGNED", "");
            s = Regex.Replace(s, @"(?i)\s+ZEROFILL", "");
            s = Regex.Replace(s, @"(?i)\s+AUTO_INCREMENT", "");
            // 移除 ON UPDATE CURRENT_TIMESTAMP（SQLite 不支持）
            s = Regex.Replace(s, @"(?i)\s+ON\s+UPDATE\s+CURRENT_TIMESTAMP(?:\s*\(\s*\d+\s*\))?", "");
            // DEFAULT CURRENT_TIMESTAMP(n) 去掉精度括号（SQLite 只认 CURRENT_TIMESTAMP 关键字，带括号直接 syntax error）
            s = Regex.Replace(s, @"(?i)\b(DEFAULT\s+CURRENT_TIMESTAMP)\s*\(\s*\d+\s*\)", "$1");

            // 列名后的第一个词是类型（可能带长度/精度括号）
            var m = Regex.Match(s, @"^(\s*)(`[^`]+`|""[^""]+""|\[[^\]]+\]|[A-Za-z_][A-Za-z0-9_]*)(\s+)([A-Za-z][A-Za-z0-9]*(?:\s*\([^)]*\))?)(.*)$");
            if (!m.Success)
            {
                return s;
            }

            var typeToken = m.Groups[4].Value;
            var typeName = Regex.Match(typeToken, @"[A-Za-z]+").Value.ToUpperInvariant();
            var mapped = typeName switch
            {
                "INT" or "INTEGER" or "TINYINT" or "SMALLINT" or "MEDIUMINT" or "BIGINT" => "INTEGER",
                "CHAR" or "VARCHAR" or "TEXT" or "TINYTEXT" or "MEDIUMTEXT" or "LONGTEXT"
                    or "ENUM" or "SET" or "JSON" or "YEAR" or "BIT" => "TEXT",
                "DATE" or "DATETIME" or "TIMESTAMP" or "TIME" => "TEXT",
                "DECIMAL" or "NUMERIC" or "FLOAT" or "DOUBLE" or "REAL" => "REAL",
                "BLOB" or "TINYBLOB" or "MEDIUMBLOB" or "LONGBLOB" or "BINARY" or "VARBINARY" => "BLOB",
                _ => "TEXT"
            };

            return m.Groups[1].Value + m.Groups[2].Value + m.Groups[3].Value + mapped + m.Groups[5].Value;
        }

        /// <summary>SQLite DDL → MySQL：标识符统一反引号 + 类型映射</summary>
        private static string ConvertSqliteToMySql(string ddl)
        {
            var s = NormalizeTableName(ddl);
            // 列内标识符 "col" / [col] → `col`（MySQL 默认不把双引号当标识符）
            s = Regex.Replace(s, @"""([^""]+)""", "`$1`");
            s = Regex.Replace(s, @"\[([^\]]+)\]", "`$1`");
            // 类型映射：INTEGER → INT、REAL → DOUBLE
            s = Regex.Replace(s, @"(?i)\bINTEGER\b", "INT");
            s = Regex.Replace(s, @"(?i)\bREAL\b", "DOUBLE");
            // SQLite 的 AUTOINCREMENT 关键字 MySQL 不支持
            s = Regex.Replace(s, @"(?i)\s+AUTOINCREMENT", "");
            return s;
        }

        /// <summary>统一 CREATE TABLE 后的表名为反引号形式（兼容 `x` / "x" / [x] / 裸名）</summary>
        private static string NormalizeTableName(string ddl)
        {
            var m = Regex.Match(ddl, @"(?i)^CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?([`""\[\]][^`""\[\]]+[`""\]]|[A-Za-z0-9_\.]+)");
            if (!m.Success)
            {
                return ddl;
            }

            var token = m.Groups[2].Value;
            var bare = UnquoteIdent(token);
            var quoted = QuoteIdent(bare);
            return ddl.Substring(0, m.Groups[2].Index) + quoted + ddl.Substring(m.Groups[2].Index + token.Length);
        }

        /// <summary>去除标识符外层引号（` " [ ]）</summary>
        private static string UnquoteIdent(string token)
        {
            var t = token.Trim();
            if (t.Length >= 2 && (t[0] == '`' || t[0] == '"' || t[0] == '[')
                && (t[^1] == '`' || t[^1] == '"' || t[^1] == ']'))
            {
                return t.Substring(1, t.Length - 2);
            }

            return t;
        }

        /// <summary>标识符加反引号（防御性转义反引号）</summary>
        private static string QuoteIdent(string name)
        {
            return "`" + name.Replace("`", "``") + "`";
        }

        /// <summary>按顶层逗号分割（跳过引号与括号内的逗号）</summary>
        private static List<string> SplitTopLevel(string s, char sep)
        {
            var list = new List<string>();
            int depth = 0;
            char quote = '\0';
            var sb = new StringBuilder();
            for (int i = 0; i < s.Length; i++)
            {
                var c = s[i];
                if (quote != '\0')
                {
                    sb.Append(c);
                    if (c == quote && (i == 0 || s[i - 1] != '\\'))
                    {
                        quote = '\0';
                    }

                    continue;
                }

                if (c == '\'' || c == '"' || c == '`')
                {
                    quote = c;
                    sb.Append(c);
                    continue;
                }

                if (c == '(')
                {
                    depth++;
                    sb.Append(c);
                    continue;
                }

                if (c == ')')
                {
                    depth--;
                    sb.Append(c);
                    continue;
                }

                if (c == sep && depth == 0)
                {
                    list.Add(sb.ToString());
                    sb.Clear();
                    continue;
                }

                sb.Append(c);
            }

            if (sb.Length > 0)
            {
                list.Add(sb.ToString());
            }

            return list;
        }

        // ==================== 目标库探测 ====================

        /// <summary>列出目标库当前所有业务表（用于冲突检测 / 非空判断）</summary>
        public static async Task<List<string>> ListTablesAsync(DNFLogin.AppSettings s)
        {
            if (IsSqlite(s))
            {
                var dbPath = DatabaseService.ResolveSqlitePath(s);
                if (!File.Exists(dbPath))
                {
                    return new List<string>();
                }

                using var conn = new SqliteConnection("Data Source=" + dbPath + ";Mode=ReadOnly");
                conn.Open();
                using var cmd = conn.CreateCommand();
                cmd.CommandText = "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'";
                using var rd = cmd.ExecuteReader();
                var list = new List<string>();
                while (rd.Read())
                {
                    list.Add(rd.GetString(0));
                }

                return list;
            }

            await using var client = new MySqlClient();
            await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 10);
            var rows = await client.QueryAsync(
                "SELECT table_name AS tbl_name FROM information_schema.tables WHERE table_schema = DATABASE()");
            return rows.Select(r => r["tbl_name"]?.ToString() ?? "")
                       .Where(n => n.Length > 0)
                       .ToList();
        }

        /// <summary>目标库连接描述（用于导入前二次确认展示）</summary>
        public static string DescribeTarget(DNFLogin.AppSettings s)
        {
            if (IsSqlite(s))
            {
                return "类型：SQLite\n文件：" + DatabaseService.ResolveSqlitePath(s);
            }

            return "类型：MySQL\n地址：" + s.Host + ":" + s.Port + "\n库名：" + s.Name + "\n账号：" + s.User;
        }
    }
}
