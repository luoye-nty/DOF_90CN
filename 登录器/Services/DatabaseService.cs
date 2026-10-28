using System;
using System.Collections.Generic;
using System.IO;
using System.Text;
using System.Threading.Tasks;
using Microsoft.Data.Sqlite;

namespace DNFLogin
{
    /// <summary>
    /// 数据库直连服务：连接检测、账号认证、账号注册。
    /// 表结构对齐旧版 Go control/account_auth.go 的 dnf_login_accounts：
    ///   account_id VARCHAR(128)/TEXT, username VARCHAR(64)/TEXT, password_hash VARBINARY/BLOB,
    ///   state VARCHAR(32) default 'active', created_at/updated_at, last_login_at
    /// MySQL 模式基于纯 C# MySQL Wire Protocol（Services/MySqlClient.cs）+ 纯 C# BCrypt（Services/Bcrypt.cs）；
    /// SQLite 模式基于 Microsoft.Data.Sqlite 直连 dnf90.db（与服务端共用同一账号表）。
    /// 两种模式共用同一套账号校验规则与 bcrypt 哈希。
    /// </summary>
    public static class DatabaseService
    {
        /// <summary>测试数据库连接（在线检测：能连上即成功）。按 AppSettings.Mode 分流：sqlite 检测文件，portable 检测 MySQL。</summary>
        public static async Task<bool> TestConnectionAsync(AppSettings s)
        {
            if (IsSqlite(s))
            {
                return TestSqliteConnection(s);
            }

            try
            {
                await using var client = new Services.MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 5);
                return true;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>
        /// 验证账号：SELECT account_id, password_hash, state ... WHERE username = ?
        /// bcrypt 校验 + state=active，成功则更新 last_login_at。
        /// 返回 (是否成功, 错误信息, 账号ID)。账号ID供启动游戏后绑定进程使用。
        /// </summary>
        public static async Task<(bool Ok, string Error, string AccountId, bool HasRescueCode)> AuthenticateAsync(AppSettings s, string account, string password)
        {
            if (IsSqlite(s))
            {
                return await AuthenticateSqlite(s, account, password);
            }

            try
            {
                await using var client = new Services.MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 5);
                await EnsureMySqlSchemaAsync(client);

                var rows = await client.QueryAsync(
                    "SELECT account_id, password_hash, state, rescue_code_hash FROM dnf_login_accounts WHERE username = '" +
                    EscapeSql(NormalizeUsername(account)) + "'");

                if (rows.Count == 0)
                {
                    return (false, "账号或密码错误", string.Empty, false);
                }

                var accountId = rows[0]["account_id"]?.ToString() ?? "";
                // password_hash 为 VARBINARY，按 byte[] 读取后转 UTF-8（bcrypt 为 ASCII 文本）
                var passwordHash = rows[0]["password_hash"]?.ToString() ?? "";
                var state = rows[0]["state"]?.ToString() ?? "";
                // 老号无救援代码：rescue_code_hash 为空（NULL / 空字符串 / 旧库未迁移列时查询报错除外）
                var rescueHash = rows[0].TryGetValue("rescue_code_hash", out var rv)
                    ? (rv?.ToString() ?? "")
                    : "";
                var hasRescueCode = !string.IsNullOrWhiteSpace(rescueHash);

                if (!Services.Bcrypt.Verify(password, passwordHash))
                {
                    return (false, "账号或密码错误", string.Empty, false);
                }

                if (state != "active")
                {
                    return (false, "该账号已被禁用", string.Empty, false);
                }

                await client.QueryAsync(
                    "UPDATE dnf_login_accounts SET last_login_at = NOW(6), updated_at = NOW(6) WHERE account_id = '" +
                    EscapeSql(accountId) + "'");

                return (true, string.Empty, accountId, hasRescueCode);
            }
            catch (Exception ex)
            {
                return (false, FriendlyDbError(ex), string.Empty, false);
            }
        }

        /// <summary>救援代码规则：6 位数字。注册必填；老号可在登录后补设。</summary>
        public static bool IsValidRescueCode(string? code)
        {
            if (string.IsNullOrWhiteSpace(code) || code.Length != 6)
            {
                return false;
            }
            foreach (var ch in code)
            {
                if (ch < '0' || ch > '9')
                {
                    return false;
                }
            }
            return true;
        }

        /// <summary>
        /// 注册账号：规则对齐旧版 Go normalizeLocalUsername / validateLocalPassword，
        /// 直接写数据库（bcrypt 哈希密码）。rescueCode 为 6 位数字救援代码（找回密码用，注册必填）。
        /// 返回空字符串表示成功，否则为错误信息。
        /// </summary>
        public static async Task<string> RegisterAsync(AppSettings s, string account, string password, string rescueCode)
        {
            if (!IsValidRescueCode(rescueCode))
            {
                return "救援代码须为 6 位数字，请重新输入";
            }

            var username = NormalizeUsername(account);
            if (username.Length < 3 || username.Length > 32)
            {
                return "账号长度须为 3-32 个字符";
            }

            foreach (var ch in username)
            {
                if (!char.IsLetterOrDigit(ch) && ch != '_' && ch != '-' && ch != '.')
                {
                    return "账号只能包含字母、数字、下划线、中划线、点";
                }
            }

            if (password.Length < 8 || password.Length > 72)
            {
                return "密码长度须为 8-72 个字符";
            }

            if (IsSqlite(s))
            {
                return await RegisterSqlite(s, username, password, rescueCode);
            }

            try
            {
                await using var client = new Services.MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 5);
                await EnsureMySqlSchemaAsync(client);

                var check = await client.QueryAsync(
                    "SELECT 1 AS exists_flag FROM dnf_login_accounts WHERE username = '" +
                    EscapeSql(username) + "' LIMIT 1");

                if (check.Count > 0)
                {
                    return "该账号已注册";
                }

                var hash = Services.Bcrypt.HashPassword(password);
                var rescueHash = Services.Bcrypt.HashPassword(rescueCode);
                var accountId = "wpf-" + Guid.NewGuid().ToString("N");

                await client.QueryAsync(
                    "INSERT INTO dnf_login_accounts (account_id, username, password_hash, rescue_code_hash, state, created_at, updated_at) " +
                    "VALUES ('" + EscapeSql(accountId) + "', '" + EscapeSql(username) + "', '" + EscapeSql(hash) +
                    "', '" + EscapeSql(rescueHash) + "', 'active', NOW(6), NOW(6))");

                return string.Empty;
            }
            catch (Exception ex)
            {
                if (ex.Message.Contains("1062") || ex.Message.Contains("Duplicate"))
                {
                    return "该账号已注册";
                }
                return FriendlyDbError(ex);
            }
        }

        /// <summary>账号规范化（对齐 Go：小写 + 去首尾空白）</summary>
        public static string NormalizeUsername(string account)
        {
            return (account ?? string.Empty).Trim().ToLowerInvariant();
        }

        /// <summary>
        /// S4 系列（S4A12 / S4A21）账号注册：直连 S4 服务端 SQLite 数据库（Data/inventory.db）的 accounts 表。
        /// S4 服务端（DfoServer）无 MySQL，登录时按 m_id 查 accounts，账号不存在则自动创建且不校验密码；
        /// 登录器注册的目标就是预写 accounts（m_id 唯一约束做查重）。返回空字符串表示成功，否则为错误信息。
        /// </summary>
        public static async Task<string> RegisterS4Async(AppSettings s, string account, string password)
        {
            var username = NormalizeUsername(account);
            if (username.Length < 3 || username.Length > 32)
            {
                return "账号长度须为 3-32 个字符";
            }

            foreach (var ch in username)
            {
                if (!char.IsLetterOrDigit(ch) && ch != '_' && ch != '-' && ch != '.')
                {
                    return "账号只能包含字母、数字、下划线、中划线、点";
                }
            }

            if (password.Length < 8 || password.Length > 72)
            {
                return "密码长度须为 8-72 个字符";
            }

            var path = ResolveS4DatabasePath(s);
            if (string.IsNullOrWhiteSpace(path) || !File.Exists(path))
            {
                return string.IsNullOrWhiteSpace(path)
                    ? "未配置 S4 服务端目录（ServerRootPath 为空），无法定位 inventory.db。"
                    : "S4 服务端数据库不存在：" + path + "（请检查“设置-服务端目录”配置）";
            }

            try
            {
                await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                await conn.OpenAsync();

                await using (var check = conn.CreateCommand())
                {
                    check.CommandText = "SELECT 1 FROM accounts WHERE m_id = $u LIMIT 1;";
                    check.Parameters.AddWithValue("$u", username);
                    var existing = await check.ExecuteScalarAsync();
                    if (existing != null)
                    {
                        return "该账号已注册";
                    }
                }

                var hash = Services.Bcrypt.HashPassword(password);
                await using var insert = conn.CreateCommand();
                insert.CommandText = "INSERT INTO accounts (m_id, password_hash) VALUES ($u, $h);";
                insert.Parameters.AddWithValue("$u", username);
                insert.Parameters.AddWithValue("$h", hash);
                await insert.ExecuteNonQueryAsync();

                return string.Empty;
            }
            catch (Exception ex)
            {
                if (ex.Message.Contains("UNIQUE", StringComparison.OrdinalIgnoreCase)
                    || ex.Message.Contains("constraint", StringComparison.OrdinalIgnoreCase))
                {
                    return "该账号已注册";
                }
                if (ex.Message.Contains("locked", StringComparison.OrdinalIgnoreCase)
                    || ex.Message.Contains("busy", StringComparison.OrdinalIgnoreCase))
                {
                    return "S4 服务端数据库正被占用（服务端可能正在写库），请稍后重试或先停止服务端再注册。";
                }
                return "S4 注册失败：服务端数据库写入失败，请确认服务端目录可写、数据库文件未被占用后重试。";
            }
        }

        /// <summary>
        /// S4 系列（S4A12 / S4A21）账号校验：直连 S4 服务端 SQLite 数据库（inventory.db）的 accounts 表。
        /// S4 服务端（DfoServer）不校验账号密码（GetByMid 未命中自动创建且不校验密码），
        /// 因此账号存在性与密码正确性必须由登录器承担：
        /// 账号不存在 → "账号不存在，请先注册"；密码与注册时 bcrypt 哈希不匹配 → "账号或密码错误"。
        /// </summary>
        public static async Task<(bool Ok, string Error, string AccountId)> AuthenticateS4Async(AppSettings s, string account, string password)
        {
            var username = NormalizeUsername(account);
            var path = ResolveS4DatabasePath(s);
            if (string.IsNullOrWhiteSpace(path) || !File.Exists(path))
            {
                return (false, string.IsNullOrWhiteSpace(path)
                    ? "未配置 S4 服务端目录（ServerRootPath 为空），无法定位 inventory.db。"
                    : "S4 服务端数据库不存在：" + path + "（请检查“设置-服务端目录”配置）", string.Empty);
            }

            try
            {
                await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                await conn.OpenAsync();

                await using var cmd = conn.CreateCommand();
                cmd.CommandText = "SELECT m_id, password_hash FROM accounts WHERE m_id = $u LIMIT 1;";
                cmd.Parameters.AddWithValue("$u", username);
                await using var reader = await cmd.ExecuteReaderAsync();

                if (!await reader.ReadAsync())
                {
                    return (false, "账号不存在，请先注册", string.Empty);
                }

                var hash = ReadBlobAsUtf8(reader, 1);
                if (string.IsNullOrWhiteSpace(hash) || !Services.Bcrypt.Verify(password, hash))
                {
                    return (false, "账号或密码错误", string.Empty);
                }

                return (true, string.Empty, username);
            }
            catch (Exception)
            {
                return (false, "S4 账号校验失败：服务端数据库读取失败，请确认服务端目录配置正确、数据库文件未被占用后重试。", string.Empty);
            }
        }

        /// <summary>
        /// 定位 S4 服务端 SQLite 数据库文件（Data/inventory.db）。
        /// 服务端规则：AppContext.BaseDirectory\Data\inventory.db，即运行中 DfoServer.exe 所在目录（bin\Debug / dist\win-x64）下的 Data。
        /// 登录器 ServerRootPath 通常是服务端源码根（含 build.bat），必须先经 LocateS4Server 找到 exe 输出目录再取 Data；
        /// 找不到 exe 时退回已知结构候选路径。
        /// </summary>
        public static string ResolveS4DatabasePath(AppSettings s)
        {
            var root = s.ServerRootPath;
            if (string.IsNullOrWhiteSpace(root))
            {
                return string.Empty;
            }

            root = Path.GetFullPath(root);

            // 1) 优先 exe 输出目录：运行中（编译后）的数据库在 DfoServer.exe 同目录 Data 下
            var exe = Services.ServerManager.LocateS4Server(root);
            if (exe is not null)
            {
                var exeDir = Path.GetDirectoryName(exe);
                if (!string.IsNullOrWhiteSpace(exeDir))
                {
                    var db = Path.Combine(exeDir, "Data", "inventory.db");
                    if (File.Exists(db))
                    {
                        return db;
                    }
                }
            }

            // 2) 已知结构候选：编译/发布输出目录优先，源码 Data 兜底
            var candidates = new[]
            {
                Path.Combine(root, "Server", "DfoServer", "bin", "Debug", "Data", "inventory.db"),
                Path.Combine(root, "Server", "DfoServer", "bin", "Release", "Data", "inventory.db"),
                Path.Combine(root, "dist", "win-x64", "Data", "inventory.db"),
                Path.Combine(root, "bin", "Debug", "Data", "inventory.db"),
                Path.Combine(root, "bin", "Release", "Data", "inventory.db"),
                Path.Combine(root, "Data", "inventory.db"),
            };
            foreach (var candidate in candidates)
            {
                if (File.Exists(candidate))
                {
                    return candidate;
                }
            }

            // 3) 未找到：返回最可能的运行时路径（exe 目录 Data），供报错提示定位
            if (exe is not null && !string.IsNullOrWhiteSpace(Path.GetDirectoryName(exe)))
            {
                return Path.Combine(Path.GetDirectoryName(exe)!, "Data", "inventory.db");
            }
            return Path.Combine(root, "Data", "inventory.db");
        }

        /// <summary>
        /// 确保 MySQL 模式存在 dnf_login_accounts 表。
        /// 该表由干净包 SQLite dnf90.db 预置；MySQL 模式下 DNF90Server 建表时不会创建它，
        /// 因此登录器首次建连后按相同结构自动补建（幂等）。
        /// </summary>
        private static async Task EnsureMySqlSchemaAsync(Services.MySqlClient client)
        {
            const string ddl =
                "CREATE TABLE IF NOT EXISTS dnf_login_accounts (" +
                "account_id VARCHAR(128) NOT NULL, " +
                "username VARCHAR(64) NOT NULL, " +
                "password_hash VARBINARY(60) NOT NULL, " +
                "rescue_code_hash VARCHAR(100) NULL, " +
                "state VARCHAR(32) NOT NULL DEFAULT 'active', " +
                "created_at DATETIME(6) NOT NULL, " +
                "updated_at DATETIME(6) NOT NULL, " +
                "last_login_at DATETIME(6) NULL, " +
                "PRIMARY KEY (account_id), " +
                "UNIQUE KEY uk_dnf_login_accounts_username (username)" +
                ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci";
            await client.QueryAsync(ddl);

            // 兼容旧库：老表无 rescue_code_hash 列时补列（重复添加会报 1060，忽略即可）
            try
            {
                await client.QueryAsync(
                    "ALTER TABLE dnf_login_accounts ADD COLUMN rescue_code_hash VARCHAR(100) NULL");
            }
            catch (Exception ex)
            {
                if (!ex.Message.Contains("1060") && !ex.Message.Contains("Duplicate column"))
                {
                    throw;
                }
            }
        }

        /// <summary>SQL 字符串转义（防注入）</summary>
        public static string EscapeSql(string value)
        {
            return (value ?? string.Empty)
                .Replace("\\", "\\\\")
                .Replace("'", "\\'")
                .Replace("\0", "\\0")
                .Replace("\n", "\\n")
                .Replace("\r", "\\r");
        }

        /// <summary>
        /// 将数据库连接/操作异常转换为用户友好提示。
        /// 连接被拒（SocketException 10061，中文系统消息"由于目标计算机积极拒绝，无法连接"）
        /// 通常意味着服务端数据库未启动，直接引导用户重启服务端，避免展示晦涩的系统异常文案。
        /// </summary>
        private static string FriendlyDbError(Exception ex)
        {
            var msg = ex.Message ?? string.Empty;
            var isRefused = ex is System.Net.Sockets.SocketException
                || msg.Contains("积极拒绝", StringComparison.Ordinal)
                || msg.Contains("refused", StringComparison.OrdinalIgnoreCase)
                || msg.Contains("Connection refused", StringComparison.OrdinalIgnoreCase);

            if (isRefused)
            {
                return "数据库连接失败：服务端未启动或无响应。请先启动服务端；若已启动仍失败，可重启一次服务端后再试。";
            }
            if (msg.Contains("1045", StringComparison.Ordinal) ||
                msg.Contains("Access denied", StringComparison.OrdinalIgnoreCase))
            {
                return "数据库连接失败：数据库账号或密码错误，请在 设置-服务器配置 中检查数据库账号。";
            }
            if (msg.Contains("1049", StringComparison.Ordinal) ||
                msg.Contains("Unknown database", StringComparison.OrdinalIgnoreCase))
            {
                return "数据库连接失败：数据库名称错误，请在 设置-服务器配置 中检查数据库名。";
            }
            return "数据库连接失败：请检查数据库服务是否已启动、连接地址与端口是否正确；若仍无法解决，请查看服务端数据库配置。";
        }

        // ==================== SQLite 模式 ====================

        private static bool IsSqlite(AppSettings s)
        {
            return string.Equals(s.Mode, "sqlite", StringComparison.OrdinalIgnoreCase);
        }

        /// <summary>
        /// 解析 SQLite 数据库文件绝对路径。
        /// instance.json database.path 为相对 runtime 的路径（如 data/dnf90.db），
        /// 绝对路径直接使用；路径无法确定时返回空字符串。
        /// </summary>
        public static string ResolveSqlitePath(AppSettings s)
        {
            var p = s.DatabasePath;
            if (string.IsNullOrWhiteSpace(p))
            {
                p = "data/dnf90.db";
            }

            string full;
            if (Path.IsPathRooted(p))
            {
                full = p;
            }
            else if (!string.IsNullOrWhiteSpace(s.ServerRootPath))
            {
                full = Path.Combine(s.ServerRootPath, "runtime", p);
            }
            else
            {
                full = Path.Combine(Environment.CurrentDirectory, p);
            }

            return Path.GetFullPath(full);
        }

        /// <summary>SQLite 连接测试：文件存在且可打开（ReadWrite 模式，确保可写）即成功</summary>
        private static bool TestSqliteConnection(AppSettings s)
        {
            var path = ResolveSqlitePath(s);
            if (!File.Exists(path))
            {
                return false;
            }

            try
            {
                using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                conn.Open();
                using var cmd = conn.CreateCommand();
                cmd.CommandText = "SELECT 1";
                cmd.ExecuteScalar();
                return true;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>SQLite 账号认证：直连 dnf90.db，逻辑与 MySQL 分支一致</summary>
        private static async Task<(bool Ok, string Error, string AccountId, bool HasRescueCode)> AuthenticateSqlite(AppSettings s, string account, string password)
        {
            var path = ResolveSqlitePath(s);
            if (!File.Exists(path))
            {
                return (false, "SQLite 数据库文件不存在：" + path, string.Empty, false);
            }

            try
            {
                await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                await conn.OpenAsync();
                EnsureSqliteSchema(conn);

                await using (var cmd = conn.CreateCommand())
                {
                    cmd.CommandText = "SELECT account_id, password_hash, state, rescue_code_hash FROM dnf_login_accounts WHERE username = $u";
                    cmd.Parameters.AddWithValue("$u", NormalizeUsername(account));
                    await using var reader = await cmd.ExecuteReaderAsync();

                    if (!await reader.ReadAsync())
                    {
                        return (false, "账号或密码错误", string.Empty, false);
                    }

                    var accountId = reader.GetString(0);
                    // password_hash 为 BLOB，按 byte[] 读取后转 UTF-8（bcrypt 为 ASCII 文本）
                    var passwordHash = ReadBlobAsUtf8(reader, 1);
                    var state = reader.GetString(2);
                    var rescueHash = ReadBlobAsUtf8(reader, 3);
                    var hasRescueCode = !string.IsNullOrWhiteSpace(rescueHash);

                    if (!Services.Bcrypt.Verify(password, passwordHash))
                    {
                        return (false, "账号或密码错误", string.Empty, false);
                    }

                    if (state != "active")
                    {
                        return (false, "该账号已被禁用", string.Empty, false);
                    }

                    await using var update = conn.CreateCommand();
                    update.CommandText =
                        "UPDATE dnf_login_accounts SET last_login_at = $now, updated_at = $now WHERE account_id = $id";
                    update.Parameters.AddWithValue("$now", UtcNowString());
                    update.Parameters.AddWithValue("$id", accountId);
                    await update.ExecuteNonQueryAsync();

                    return (true, string.Empty, accountId, hasRescueCode);
                }
            }
            catch (Exception ex)
            {
                return (false, FriendlyDbError(ex), string.Empty, false);
            }
        }

        /// <summary>SQLite 账号注册：直连 dnf90.db，查重后 INSERT（bcrypt 哈希），时间格式对齐服务端</summary>
        private static async Task<string> RegisterSqlite(AppSettings s, string username, string password, string rescueCode)
        {
            var path = ResolveSqlitePath(s);
            if (!File.Exists(path))
            {
                return "SQLite 数据库文件不存在：" + path;
            }

            try
            {
                await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                await conn.OpenAsync();
                EnsureSqliteSchema(conn);

                await using (var check = conn.CreateCommand())
                {
                    check.CommandText = "SELECT 1 FROM dnf_login_accounts WHERE username = $u LIMIT 1";
                    check.Parameters.AddWithValue("$u", username);
                    if (await check.ExecuteScalarAsync() is not null)
                    {
                        return "该账号已注册";
                    }
                }

                var hash = Services.Bcrypt.HashPassword(password);
                var rescueHash = Services.Bcrypt.HashPassword(rescueCode);
                var accountId = "wpf-" + Guid.NewGuid().ToString("N");
                var now = UtcNowString();

                await using var insert = conn.CreateCommand();
                insert.CommandText =
                    "INSERT INTO dnf_login_accounts (account_id, username, password_hash, rescue_code_hash, state, created_at, updated_at) " +
                    "VALUES ($id, $u, $h, $rh, 'active', $now, $now)";
                insert.Parameters.AddWithValue("$id", accountId);
                insert.Parameters.AddWithValue("$u", username);
                insert.Parameters.AddWithValue("$h", hash);
                insert.Parameters.AddWithValue("$rh", rescueHash);
                insert.Parameters.AddWithValue("$now", now);
                await insert.ExecuteNonQueryAsync();

                return string.Empty;
            }
            catch (Exception ex)
            {
                if (ex.Message.Contains("UNIQUE"))
                {
                    return "该账号已注册";
                }
                return FriendlyDbError(ex);
            }
        }

        /// <summary>
        /// SQLite 表结构迁移：建表（含 rescue_code_hash）并对旧库补列。
        /// rescue_code_hash 为 NULL 时 Microsoft.Data.Sqlite 存储为 DBNull，按空字符串处理。
        /// </summary>
        private static void EnsureSqliteSchema(SqliteConnection conn)
        {
            using var create = conn.CreateCommand();
            create.CommandText =
                "CREATE TABLE IF NOT EXISTS dnf_login_accounts (" +
                "account_id TEXT NOT NULL, " +
                "username TEXT NOT NULL, " +
                "password_hash BLOB NOT NULL, " +
                "rescue_code_hash BLOB NULL, " +
                "state TEXT NOT NULL DEFAULT 'active', " +
                "created_at TEXT NOT NULL, " +
                "updated_at TEXT NOT NULL, " +
                "last_login_at TEXT NULL, " +
                "PRIMARY KEY (account_id), " +
                "UNIQUE (username))";
            create.ExecuteNonQuery();

            try
            {
                using var alter = conn.CreateCommand();
                alter.CommandText = "ALTER TABLE dnf_login_accounts ADD COLUMN rescue_code_hash BLOB NULL";
                alter.ExecuteNonQuery();
            }
            catch (Exception ex)
            {
                // 列已存在（旧库已迁移/新表已含该列）时 SQLite 报 duplicate column name，忽略
                if (!ex.Message.Contains("duplicate column", StringComparison.OrdinalIgnoreCase))
                {
                    throw;
                }
            }
        }

        /// <summary>读取 SQLite BLOB 列为 UTF-8 文本（兼容 TEXT/BLOB 两种存储）</summary>
        private static string ReadBlobAsUtf8(SqliteDataReader reader, int ordinal)
        {
            if (reader.IsDBNull(ordinal))
            {
                return string.Empty;
            }

            var val = reader.GetValue(ordinal);
            if (val is byte[] bytes)
            {
                return Encoding.UTF8.GetString(bytes);
            }
            return Convert.ToString(val) ?? string.Empty;
        }

        /// <summary>UTC 时间字符串，格式对齐服务端 Go 写入（如 2026-08-23 04:50:43.9657011 +0000 UTC）</summary>
        private static string UtcNowString()
        {
            return DateTime.UtcNow.ToString("yyyy-MM-dd HH:mm:ss.fffffff +0000 UTC");
        }

        // ==================== 救援代码（90 本地模式） ====================

        /// <summary>
        /// 90 本地：为账号补设/覆盖救援代码（登录后引导老号使用）。
        /// 新值校验 6 位数字，bcrypt 落库。返回空字符串表示成功，否则为错误信息。
        /// </summary>
        public static async Task<string> UpdateRescueCodeAsync(AppSettings s, string account, string rescueCode)
        {
            if (!IsValidRescueCode(rescueCode))
            {
                return "救援代码须为 6 位数字，请重新输入";
            }

            var username = NormalizeUsername(account);
            var rescueHash = Services.Bcrypt.HashPassword(rescueCode);

            if (IsSqlite(s))
            {
                var path = ResolveSqlitePath(s);
                if (!File.Exists(path))
                {
                    return "SQLite 数据库文件不存在：" + path;
                }

                try
                {
                    await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                    await conn.OpenAsync();
                    EnsureSqliteSchema(conn);

                    await using (var check = conn.CreateCommand())
                    {
                        check.CommandText = "SELECT 1 FROM dnf_login_accounts WHERE username = $u LIMIT 1";
                        check.Parameters.AddWithValue("$u", username);
                        if (await check.ExecuteScalarAsync() is null)
                        {
                            return "账号不存在";
                        }
                    }

                    await using var update = conn.CreateCommand();
                    update.CommandText =
                        "UPDATE dnf_login_accounts SET rescue_code_hash = $rh, updated_at = $now WHERE username = $u";
                    update.Parameters.AddWithValue("$rh", rescueHash);
                    update.Parameters.AddWithValue("$now", UtcNowString());
                    update.Parameters.AddWithValue("$u", username);
                    await update.ExecuteNonQueryAsync();
                    return string.Empty;
                }
                catch (Exception ex)
                {
                    return FriendlyDbError(ex);
                }
            }

            try
            {
                await using var client = new Services.MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 5);
                await EnsureMySqlSchemaAsync(client);

                var check = await client.QueryAsync(
                    "SELECT 1 AS exists_flag FROM dnf_login_accounts WHERE username = '" +
                    EscapeSql(username) + "' LIMIT 1");
                if (check.Count == 0)
                {
                    return "账号不存在";
                }

                await client.QueryAsync(
                    "UPDATE dnf_login_accounts SET rescue_code_hash = '" + EscapeSql(rescueHash) +
                    "', updated_at = NOW(6) WHERE username = '" + EscapeSql(username) + "'");
                return string.Empty;
            }
            catch (Exception ex)
            {
                return FriendlyDbError(ex);
            }
        }

        /// <summary>
        /// 90 本地：凭 账号+救援代码 找回密码（无原密码）。
        /// 校验账号存在且救援代码 bcrypt 匹配后更新密码，返回空字符串表示成功。
        /// </summary>
        public static async Task<(bool Ok, string Error)> ResetPasswordByRescueCodeAsync(
            AppSettings s, string account, string rescueCode, string newPassword)
        {
            if (newPassword.Length < 8 || newPassword.Length > 72)
            {
                return (false, "密码长度须为 8-72 个字符");
            }

            var username = NormalizeUsername(account);
            string rescueHashInDb;

            if (IsSqlite(s))
            {
                var path = ResolveSqlitePath(s);
                if (!File.Exists(path))
                {
                    return (false, "SQLite 数据库文件不存在：" + path);
                }

                try
                {
                    await using var conn = new SqliteConnection($"Data Source={path};Mode=ReadWrite");
                    await conn.OpenAsync();
                    EnsureSqliteSchema(conn);

                    await using var query = conn.CreateCommand();
                    query.CommandText = "SELECT rescue_code_hash FROM dnf_login_accounts WHERE username = $u LIMIT 1";
                    query.Parameters.AddWithValue("$u", username);
                    var raw = await query.ExecuteScalarAsync();
                    if (raw is null || raw is DBNull)
                    {
                        return (false, "账号不存在或未设置救援代码");
                    }
                    // 直接转换：raw 可能为 byte[]（BLOB）或 string
                    rescueHashInDb = raw switch
                    {
                        byte[] b => System.Text.Encoding.UTF8.GetString(b),
                        string str => str,
                        _ => ""
                    };

                    if (string.IsNullOrWhiteSpace(rescueHashInDb))
                    {
                        return (false, "该账号尚未设置救援代码，无法找回密码");
                    }
                    if (!Services.Bcrypt.Verify(rescueCode, rescueHashInDb))
                    {
                        return (false, "账号或救援代码错误");
                    }

                    var newHash = Services.Bcrypt.HashPassword(newPassword);
                    await using var update = conn.CreateCommand();
                    update.CommandText =
                        "UPDATE dnf_login_accounts SET password_hash = $ph, updated_at = $now WHERE username = $u";
                    update.Parameters.AddWithValue("$ph", newHash);
                    update.Parameters.AddWithValue("$now", UtcNowString());
                    update.Parameters.AddWithValue("$u", username);
                    await update.ExecuteNonQueryAsync();
                    return (true, string.Empty);
                }
                catch (Exception ex)
                {
                    return (false, FriendlyDbError(ex));
                }
            }

            try
            {
                await using var client = new Services.MySqlClient();
                await client.ConnectAsync(s.Host, int.Parse(s.Port), s.User, s.Password, s.Name, 5);
                await EnsureMySqlSchemaAsync(client);

                var rows = await client.QueryAsync(
                    "SELECT rescue_code_hash FROM dnf_login_accounts WHERE username = '" +
                    EscapeSql(username) + "' LIMIT 1");
                if (rows.Count == 0)
                {
                    return (false, "账号不存在或未设置救援代码");
                }
                var stored = rows[0]["rescue_code_hash"]?.ToString() ?? "";
                if (string.IsNullOrWhiteSpace(stored))
                {
                    return (false, "该账号尚未设置救援代码，无法找回密码");
                }
                if (!Services.Bcrypt.Verify(rescueCode, stored))
                {
                    return (false, "账号或救援代码错误");
                }

                var newHash = Services.Bcrypt.HashPassword(newPassword);
                await client.QueryAsync(
                    "UPDATE dnf_login_accounts SET password_hash = '" + EscapeSql(newHash) +
                    "', updated_at = NOW(6) WHERE username = '" + EscapeSql(username) + "'");
                return (true, string.Empty);
            }
            catch (Exception ex)
            {
                return (false, FriendlyDbError(ex));
            }
        }
    }
}
