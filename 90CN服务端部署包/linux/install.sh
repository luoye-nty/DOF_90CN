#!/usr/bin/env bash
# ============================================================
#  DNF90 服务端 · Ubuntu 一键安装脚本
#  用法：  sudo bash install.sh
#  支持：Ubuntu 20.04 / 22.04 / 24.04（64 位）
#  作用：安装数据库 -> 创建数据库 -> 部署服务端 -> 装 systemd 服务 -> 放行端口 -> 启动
# ============================================================
set -euo pipefail

INSTALL_DIR="/opt/dnf90"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/conf/dnf90.env"

RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; CYAN='\033[36m'; NC='\033[0m'
say()  { echo -e "${CYAN}[DNF90]${NC} $*"; }
ok()   { echo -e "${GREEN}[  OK ]${NC} $*"; }
warn() { echo -e "${YELLOW}[ WARN]${NC} $*"; }
die()  { echo -e "${RED}[FAIL]${NC} $*" >&2; exit 1; }

# ---------- 0. 前置检查 ----------
[ "$(id -u)" = "0" ] || die "请用 root 运行：sudo bash install.sh"
[ -f "$ENV_FILE" ] || die "找不到配置文件 $ENV_FILE"

# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
PUBLIC_IP="${PUBLIC_IP:-}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-}"
ALLOW_REGISTRATION="${ALLOW_REGISTRATION:-true}"
REGISTRATION_MODE="${REGISTRATION_MODE:-open}"
MAX_REGISTER_PER_IP="${MAX_REGISTER_PER_IP:-0}"
REGION_NAME="${REGION_NAME:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
ADMIN_PORT="${ADMIN_PORT:-18114}"
OPEN_ADMIN_FIREWALL="${OPEN_ADMIN_FIREWALL:-true}"

# ---------- 1. 公网 IP ----------
if [ -z "$PUBLIC_IP" ]; then
  say "配置里没填 PUBLIC_IP，尝试自动识别公网 IP ..."
  PUBLIC_IP="$(curl -fsS --max-time 8 https://api.ipify.org 2>/dev/null || true)"
  [ -z "$PUBLIC_IP" ] && PUBLIC_IP="$(curl -fsS --max-time 8 https://ifconfig.me 2>/dev/null || true)"
  if [ -z "$PUBLIC_IP" ]; then
    PUBLIC_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    warn "识别不到公网 IP，暂时使用内网地址 $PUBLIC_IP（公网联机请把 conf/dnf90.env 里的 PUBLIC_IP 填对后重跑）"
  fi
fi
say "对外公网 IP = ${GREEN}$PUBLIC_IP${NC}"

# ---------- 2. 随机密码 / 令牌 ----------
if [ -z "$MYSQL_PASSWORD" ]; then
  MYSQL_PASSWORD="dnf90$( { tr -dc 'a-zA-Z0-9' </dev/urandom 2>/dev/null || true; } | head -c 16 || true )"
  warn "MYSQL_PASSWORD 为空，已自动生成：${GREEN}$MYSQL_PASSWORD${NC}  （已写入 conf，请自行保存）"
fi
gen_hex() { if command -v openssl >/dev/null 2>&1; then openssl rand -hex "$1"; else head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; fi; }

ADMIN_TOKEN="adm_$(gen_hex 16)"
INTERNAL_TOKEN="$(gen_hex 32)"
ACCOUNT_ID="dnf:$(gen_hex 16)"
ADMIN_SESSION_SECRET="$(gen_hex 32)"

if [ -z "$ADMIN_PASSWORD" ]; then
  ADMIN_PASSWORD="dnf90$(gen_hex 6)"
  warn "ADMIN_PASSWORD 为空，已自动生成网页后台密码：${GREEN}$ADMIN_PASSWORD${NC}"
fi

# ---------- 2.5 停掉旧的同名服务（避免端口占用 / 崩溃重启循环干扰安装）----------
if systemctl list-unit-files 2>/dev/null | grep -q '^dnf90server\.service\|^dnf90auth\.service\|^dnf90admin\.service'; then
  say "停止旧的 dnf90 服务 ..."
  systemctl stop dnf90server.service dnf90auth.service dnf90admin.service 2>/dev/null || true
  systemctl reset-failed dnf90server.service dnf90auth.service dnf90admin.service 2>/dev/null || true
  ok "旧服务已停止"
fi

# ---------- 3. 安装数据库 (MySQL 8) ----------
say "检查数据库 ..."
if ! command -v mysqld >/dev/null 2>&1 && ! command -v mariadbd >/dev/null 2>&1; then
  say "未安装 MySQL，开始安装（需要联网，约 1-3 分钟）..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y
  apt-get install -y mysql-server || apt-get install -y mariadb-server
else
  ok "数据库已安装"
fi

if systemctl list-unit-files 2>/dev/null | grep -q '^mysql\.service'; then DB_SVC=mysql; else DB_SVC=mariadb; fi
systemctl enable --now "$DB_SVC" >/dev/null 2>&1 || true
sleep 3

say "创建数据库 dnf_local 与账号 dnf90 ..."
mysql --protocol=socket -uroot <<SQL
CREATE DATABASE IF NOT EXISTS dnf_local CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE USER IF NOT EXISTS 'dnf90'@'%' IDENTIFIED BY '${MYSQL_PASSWORD}';
ALTER USER 'dnf90'@'%' IDENTIFIED BY '${MYSQL_PASSWORD}';
GRANT ALL PRIVILEGES ON dnf_local.* TO 'dnf90'@'%';
FLUSH PRIVILEGES;
SQL
ok "数据库就绪"

say "验证数据库账号能否用 TCP 登录（Go 程序走的是这条路）..."
if mysql -h127.0.0.1 -P3306 -udnf90 -p"$MYSQL_PASSWORD" -e "SELECT 1" dnf_local >/dev/null 2>&1; then
  ok "数据库账号验证通过"
else
  warn "dnf90 账号 TCP 登录失败，尝试改用 mysql_native_password 认证插件 ..."
  mysql --protocol=socket -uroot <<SQL >/dev/null 2>&1 || true
ALTER USER 'dnf90'@'%' IDENTIFIED WITH mysql_native_password BY '${MYSQL_PASSWORD}';
FLUSH PRIVILEGES;
SQL
  if mysql -h127.0.0.1 -P3306 -udnf90 -p"$MYSQL_PASSWORD" -e "SELECT 1" dnf_local >/dev/null 2>&1; then
    ok "已切换认证插件，验证通过"
  else
    warn "数据库账号仍无法 TCP 登录，请检查 MySQL（bind-address 是否为 127.0.0.1、端口 3306）"
  fi
fi

MYSQL_DSN="dnf90:${MYSQL_PASSWORD}@tcp(127.0.0.1:3306)/dnf_local?parseTime=true&charset=utf8mb4,utf8&interpolateParams=true"

# ---------- 4. 部署文件 ----------
say "部署文件到 $INSTALL_DIR ..."
mkdir -p "$INSTALL_DIR"/{bin,conf,logs,admin,auth/tls,runtime/logs,runtime/configs/dnf,runtime/configs/servergroup,runtime/data/dnf}

# 按 CPU 架构选对应二进制（本包同时带 x86_64 与 ARM64）
case "$(uname -m)" in
  x86_64|amd64)  BIN_SRC="$SCRIPT_DIR/bin" ;;
  aarch64|arm64) BIN_SRC="$SCRIPT_DIR/bin/arm64" ;;
  *) die "不支持的 CPU 架构：$(uname -m)（本包只带 x86_64 与 ARM64）" ;;
esac
[ -f "$BIN_SRC/dnf90server" ] || die "找不到 $BIN_SRC/dnf90server"
say "CPU 架构 $(uname -m) -> 使用 $BIN_SRC"

install -m 0755 "$BIN_SRC/dnf90server" "$INSTALL_DIR/bin/dnf90server"
install -m 0755 "$BIN_SRC/dnf90auth"   "$INSTALL_DIR/bin/dnf90auth"
install -m 0755 "$BIN_SRC/dnf90admin"  "$INSTALL_DIR/bin/dnf90admin"

# 清理历史遗留：早期版本会在「二进制同级目录」误生成一份默认 config.json，
# 认证服务若读到它就会用错端口(13306)/账号(root)，表现为起不来。这里直接删掉。
rm -f "$INSTALL_DIR/bin/config.json" "$INSTALL_DIR/bin/config.json.tmp-"*

# 资产：PVF 与频道表
for f in Script.pvf channel_info.etc; do
  if [ -f "$SCRIPT_DIR/data/dnf/$f" ]; then
    install -m 0644 "$SCRIPT_DIR/data/dnf/$f" "$INSTALL_DIR/runtime/data/dnf/$f"
  elif [ -f "$SCRIPT_DIR/../runtime/data/dnf/$f" ]; then
    install -m 0644 "$SCRIPT_DIR/../runtime/data/dnf/$f" "$INSTALL_DIR/runtime/data/dnf/$f"
  else
    die "缺少资产文件 $f（应放在 linux/data/dnf/ 下）"
  fi
done
ok "二进制与资产已就位"

# ---------- 5. 生成配置 ----------
say "生成配置文件 ..."

cat > "$INSTALL_DIR/conf/server.env" <<EOF
# 由 install.sh 自动生成，不要手工加引号
DNFBRIDGE_SERVER_IP=${PUBLIC_IP}
DNFBRIDGE_CHANNEL_LISTEN=0.0.0.0:7001
DNFBRIDGE_GAME_LISTEN_HOST=0.0.0.0
DNFBRIDGE_UDP_BIND_IP=0.0.0.0
DNFBRIDGE_LAUNCHER_AUTH_ENABLED=true
DNFBRIDGE_LAUNCHER_AUTH_VERIFY_URL=http://127.0.0.1:19091/internal/verify-game-ticket
DNFBRIDGE_LAUNCHER_AUTH_INTERNAL_TOKEN=${INTERNAL_TOKEN}
DNFBRIDGE_LAUNCHER_AUTH_TIMEOUT=3s
DNFBRIDGE_GAME_UPPER_HEADER=server16
DNFBRIDGE_GAME_UPPER_BODY_CODEC=plaintext
DNFBRIDGE_GAME_UPPER_CLIENT_BODY_CODEC=plaintext
DNFBRIDGE_GAME_OUTER_TOKEN=de509f65e9ccaae621cb7278fc2b8e6c
DNFBRIDGE_DPROTO_MODE=legacy_patch
DNFBRIDGE_CHANNEL_SERVER_INDEX=1
DNFBRIDGE_CHANNEL_ADVERTISE_SERVER_INDEX=1
DNFBRIDGE_REGION_ID=1
DNFBRIDGE_PARTY_UDP_RELAY_ENABLED=true
DNFBRIDGE_PARTY_UDP_RELAY_PORT_START=30000
DNFBRIDGE_PARTY_UDP_RELAY_PORT_COUNT=1024
DNFBRIDGE_PVF_PATH=data/dnf/Script.pvf
DNFBRIDGE_CHANNEL_INFO_PATH=data/dnf/channel_info.etc
DNFBRIDGE_CHANNEL_INFO_BODY_MODE=latest
DNFBRIDGE_DUNGEON_BATCH_PICKUP_ENABLED=true
DNFBRIDGE_ACCOUNT_ID=${ACCOUNT_ID}
DNFBRIDGE_PACKET_LOG=false
# 在线活动「累计 25 次」的累计进度是否循环（on=领完四档后次日从 0 次重新累计）
DNF90_PCROOM_PERIOD_LOOP=${PCROOM_PERIOD_LOOP:-on}
DNFBRIDGE_DEBUG_MODE=false
DNFBRIDGE_DEBUG_STRIKE_ON_SKILL_USE=false
# 本地测试 DLL 的 ~ 秒杀当前房间开关（KRM1），正式服保持 false
DNFBRIDGE_DEBUG_KILL_ROOM=false
DNFBRIDGE_ROBOT_ENABLED=false
LONGHENG_DNF_CONFIG=configs/dnf/logic.toml
DNFBRIDGE_GAME_INITIAL_MODE=notice
DNFBRIDGE_GAME_PRE_BOOTSTRAP=none
DNFBRIDGE_GAME_POST_BOOTSTRAP=none
DNFBRIDGE_PACKET_LOG_PATH=logs/packet_log.txt
DNF_SCRIPT_VERSION=61
EOF

cat > "$INSTALL_DIR/runtime/configs/dnfbridge.toml" <<EOF
[service]
name = "dnfbridge"
node_id = "dnfbridge-pub-1"
# 必须保持 "local"：该环境走单机 profile 校验（内存型 bus/cache/lock、无 Redis/NATS 依赖）。
# 改成 "prod"/"staging" 会触发生产级严格校验，要求 Redis、otel、审计等组件，直接启动失败。
environment = "local"
public_addr = "${PUBLIC_IP}:7001"
private_addr = "127.0.0.1:18111"

[cluster]
id = "local"
gid = 2
virtual_gid = 2
shard_id = 9999
zone = "local"
version = "local"
data_version = "local"

[admin]
listen = "127.0.0.1:18111"
token = "${ADMIN_TOKEN}"

[metrics]
prometheus_enabled = true
require_admin_token = false

[pvf]
enabled = true
path = "data/dnf/Script.pvf"
max_bytes = 536870912
preload_chunks = false
chunk_cache_bytes = 100663296
text_cache_bytes = 67108864

[server_group]
enabled = true
store_kind = "file"
plan_file = "configs/servergroup/plan.json"

[topology]
enabled = false

[idempotency]
kind = "memory"

[registry]
kind = "memory"

[bus]
kind = "memory"

[cache]
kind = "memory"

[lock]
kind = "memory"

[presence]
kind = "memory"

[worker]
size = 16
queue = 512

[profile_store]
store_kind = "memory"
EOF

cat > "$INSTALL_DIR/runtime/configs/dnf/logic.toml" <<EOF
[repository]
enabled = true
driver = "mysql"
mysql_dsn = "${MYSQL_DSN}"
mysql_max_open_conns = 64
mysql_max_idle_conns = 16
mysql_conn_max_lifetime_seconds = 300
shard_id = "9999"
table_prefix = "dnf"
server_group_plan_file = "configs/servergroup/plan.json"
auto_create_schema = true
csharp_legacy_schema = true
create_databases = false
EOF

cat > "$INSTALL_DIR/runtime/configs/servergroup/plan.json" <<'EOF'
{
  "version": "dnf90-local-v1",
  "shards": [
    { "id": "9999", "group_id": "logic-local-1", "state": "open",
      "open_at": "2026-01-01T00:00:00Z", "public_open_at": "2026-01-01T00:00:00Z" }
  ],
  "groups": [
    { "id": "logic-local-1", "service": "logic", "member_id": "logic-local-1", "state": "open" }
  ],
  "routes": [
    { "feature": "dnf_repository", "shard_id": "9999", "group_id": "logic-local-1", "state": "open",
      "meta": { "dnf.repository.read_database": "dnf_local", "dnf.repository.write_database": "dnf_local" } }
  ]
}
EOF

# 注意：服务端语义里 registration_max_per_ip=0 表示「不允许注册」，
# 所以「不限」必须【不写这个字段】（缺省 = 不按 IP 限制），不能写成 0。
#
# 注册方式（conf/dnf90.env 的 REGISTRATION_MODE，2026-10-16 起支持邀请码）：
#   open   = 玩家在登录器里自助注册，是否开放由 ALLOW_REGISTRATION 决定
#   invite = 邀请码注册，一人一码；玩家必须填对邀请码才能注册。
#            首次启动时认证服务会自己生成一个「引导邀请码」写进 logs/dnf90auth.log，
#            之后用 gen_invite_codes.sql 批量发码。
case "${REGISTRATION_MODE:-open}" in
  invite|INVITE|Invite)
    REG_MODE="invite"
    REG_CLOSED="false"
    REG_CODE_LINE="  \"registration_code_required\": true,"
    ;;
  *)
    REG_MODE="open"
    REG_CODE_LINE=""
    if [ "$ALLOW_REGISTRATION" = "true" ]; then REG_CLOSED="false"; else REG_CLOSED="true"; fi
    ;;
esac
if [ -n "${MAX_REGISTER_PER_IP:-}" ] && [ "$MAX_REGISTER_PER_IP" != "0" ]; then
  REG_MAX_LINE="  \"registration_max_per_ip\": ${MAX_REGISTER_PER_IP},"
else
  REG_MAX_LINE=""
fi

cat > "$INSTALL_DIR/auth/config.json" <<EOF
{
  "server_root": "$INSTALL_DIR",
  "database_mode": "mysql",
  "public_ip": "${PUBLIC_IP}",
  "mysql": { "host": "127.0.0.1", "port": 3306, "user": "dnf90", "password": "${MYSQL_PASSWORD}", "dbname": "dnf_local" },
  "registration_mode": "${REG_MODE}",
  "registration_closed": ${REG_CLOSED},
${REG_MAX_LINE}
${REG_CODE_LINE}
  "public": { "listen_addr": ":18112", "advertise_addr": "${PUBLIC_IP}", "tls_cert": "tls/launcher.crt", "tls_key": "tls/launcher.key" },
  "internal": { "listen_addr": "127.0.0.1:19091", "token": "${INTERNAL_TOKEN}", "ticket_ttl_minutes": 30 },
  "dnfbridge": { "executable": "$INSTALL_DIR/bin/dnf90server", "working_dir": "$INSTALL_DIR/runtime", "extra_args": ["-config", "configs/dnfbridge.toml"], "status_port": 7001 },
  "manage": { "enabled": false, "listen_addr": ":18113" },
  "pointsink": { "enabled": false, "interval_seconds": 300, "points": 100, "currency": "cera" },
  "register_bonus": { "enabled": false, "points": 100, "currency": "cera" },
  "log_file": { "enabled": true, "dir": "logs", "max_file_mb": 16, "keep_files": 20, "redact": true },
  "log_max_lines": 2000,
  "launcher_version_check": true,
  "launcher_download_url": "http://stardance.top:18030/data/dof_web/90cn/落叶DNF登录器.exe"
}
EOF

# 网页管理后台配置
cat > "$INSTALL_DIR/admin/admin.json" <<EOF
{
  "listen": ":${ADMIN_PORT}",
  "password": "${ADMIN_PASSWORD}",
  "session_secret": "${ADMIN_SESSION_SECRET}",
  "game_admin_url": "http://127.0.0.1:18111",
  "game_admin_token": "${ADMIN_TOKEN}",
  "mysql": { "host": "127.0.0.1", "port": 3306, "user": "dnf90", "password": "${MYSQL_PASSWORD}", "dbname": "dnf_local" },
  "title": "DNF90 管理后台",
  "trust_proxy": false
}
EOF
chmod 600 "$INSTALL_DIR/admin/admin.json"

# ---------- 4.6 实例信息 runtime/config/instance.json ----------
# dnf90auth 的发奖链路要用它定位游戏服 GM 管理口：
#   internal/server/redeem.go 的 gmAdminConfig() 读
#   $server_root/runtime/config/instance.json 的 server.adminListen / server.adminToken
# 缺这个文件时，CDK 兑换会以 HTTP 502「reward delivery failed」失败，
# 而且兑换码已经被核销（玩家白丢一张卡）。
# 注意：ADMIN_TOKEN 每次执行本脚本都会重新随机，所以这里必须跟着一起重写，
# 否则重装后 instance.json 里的令牌会与 dnfbridge.toml 不一致（GM 口 401）。
mkdir -p "$INSTALL_DIR/runtime/config"
cat > "$INSTALL_DIR/runtime/config/instance.json" <<EOF
{
  "installationId": "linux-${PUBLIC_IP}",
  "database": {
    "mode": "portable",
    "host": "127.0.0.1",
    "port": 3306,
    "user": "dnf90",
    "password": "${MYSQL_PASSWORD}",
    "name": "dnf_local"
  },
  "server": {
    "advertiseIp": "${PUBLIC_IP}",
    "channelListen": "0.0.0.0:7001",
    "adminListen": "127.0.0.1:18111",
    "adminToken": "${ADMIN_TOKEN}"
  },
  "game": {
    "pvfPath": "data/dnf/Script.pvf",
    "channelInfoPath": "data/dnf/channel_info.etc"
  }
}
EOF
chmod 600 "$INSTALL_DIR/runtime/config/instance.json"

ok "配置已生成"

# ---------- 5.5 配置预检（用服务端自身解析配置，把类型/字段错误提前暴露）----------
say "预检：让服务端自己解析配置（最多 10 秒）..."
if command -v ss >/dev/null 2>&1 && ss -lnt 2>/dev/null | grep -qE ':7001[[:space:]]'; then
  warn "7001 已在监听（服务正在运行），跳过预检"
else
  set +e
  PRE_OUT="$( cd "$INSTALL_DIR/runtime" && timeout 10 "$INSTALL_DIR/bin/dnf90server" -config configs/dnfbridge.toml 2>&1 | head -60 )"
  set -e
  if printf '%s' "$PRE_OUT" | grep -q "load config:"; then
    printf '%s\n' "$PRE_OUT"
    die "游戏服务端配置校验失败（见上面报错），已中止安装"
  fi
  ok "配置预检通过"
fi

# ---------- 6. systemd 服务 ----------
say "安装 systemd 服务 ..."
install -m 0644 "$SCRIPT_DIR/systemd/dnf90auth.service"   /etc/systemd/system/dnf90auth.service
install -m 0644 "$SCRIPT_DIR/systemd/dnf90server.service" /etc/systemd/system/dnf90server.service
install -m 0644 "$SCRIPT_DIR/systemd/dnf90admin.service"  /etc/systemd/system/dnf90admin.service
systemctl daemon-reload
systemctl enable dnf90auth.service dnf90server.service dnf90admin.service >/dev/null 2>&1 || true
ok "服务已注册"

# ---------- 7. 防火墙 ----------
say "配置防火墙 ..."
if command -v ufw >/dev/null 2>&1; then
  ufw allow 22/tcp >/dev/null 2>&1 || true
  ufw allow 18112/tcp >/dev/null 2>&1 || true       # 登录器认证(HTTPS)
  ufw allow 7001/tcp  >/dev/null 2>&1 || true       # 频道登录入口
  ufw allow 10000:10300/tcp >/dev/null 2>&1 || true # 各频道游戏端口
  ufw allow 2311:2312/udp   >/dev/null 2>&1 || true # 组队探测
  ufw allow 30000:31023/udp >/dev/null 2>&1 || true # 组队中转
  if [ "$OPEN_ADMIN_FIREWALL" = "true" ]; then
    ufw allow "${ADMIN_PORT}/tcp" >/dev/null 2>&1 || true  # 网页管理后台
    ok "ufw 规则已添加（含网页后台 ${ADMIN_PORT}）"
  else
    ok "ufw 规则已添加（网页后台 ${ADMIN_PORT} 未放行，只能走 SSH 隧道）"
  fi
else
  warn "未装 ufw，已跳过。请自行确保云服务器【安全组】放行：TCP 7001、TCP 18112、TCP 10000-10300、UDP 2311-2312、UDP 30000-31023"
fi

# ---------- 8. 启动 ----------
say "启动服务 ..."
systemctl restart dnf90auth.service
sleep 2
systemctl restart dnf90server.service
sleep 3
systemctl restart dnf90admin.service
sleep 3

echo
echo "============================================================"
FAILED=0
if systemctl is-active --quiet dnf90auth.service; then
  ok "认证服务 dnf90auth 运行中（18112）"
else
  FAILED=1
  warn "认证服务没起来，最近日志如下："
  echo "----------------------------------------------------------------"
  journalctl -u dnf90auth -n 25 --no-pager || true
  echo "----------------------------------------------------------------"
fi
if systemctl is-active --quiet dnf90server.service; then
  ok "游戏服务 dnf90server 运行中"
else
  FAILED=1
  warn "游戏服务没起来，最近日志如下："
  echo "----------------------------------------------------------------"
  journalctl -u dnf90server -n 30 --no-pager || true
  echo "----------------------------------------------------------------"
fi
if systemctl is-active --quiet dnf90admin.service; then
  ok "网页后台 dnf90admin 运行中（${ADMIN_PORT}）"
else
  FAILED=1
  warn "网页后台没起来，最近日志如下："
  echo "----------------------------------------------------------------"
  journalctl -u dnf90admin -n 25 --no-pager || true
  echo "----------------------------------------------------------------"
fi
echo "============================================================"
echo
echo -e "  公网 IP        : ${GREEN}${PUBLIC_IP}${NC}"
echo    "  数据库密码     : ${MYSQL_PASSWORD}"
echo    "  安装目录       : ${INSTALL_DIR}"
echo
echo -e "  ${CYAN}=== 网页管理后台 ===${NC}"
echo -e "  地址           : ${GREEN}http://${PUBLIC_IP}:${ADMIN_PORT}${NC}"
echo -e "  后台密码       : ${GREEN}${ADMIN_PASSWORD}${NC}"
echo    "  （后台含：点券发放 / 封禁解封 / 通关记录 / 账号查询）"
if [ "$OPEN_ADMIN_FIREWALL" != "true" ]; then
  echo -e "  ${YELLOW}注意：未放行 ufw。请在你自己电脑上做 SSH 隧道后访问 http://127.0.0.1:${ADMIN_PORT}${NC}"
  echo    "        ssh -L ${ADMIN_PORT}:127.0.0.1:${ADMIN_PORT} root@${PUBLIC_IP}"
fi
echo
echo    "  常用命令："
echo    "    查看状态   systemctl status dnf90server dnf90auth dnf90admin"
echo    "    重启全部   systemctl restart dnf90server dnf90auth dnf90admin"
echo    "    看游戏日志 journalctl -u dnf90server -f --no-pager"
echo    "    看认证日志 journalctl -u dnf90auth  -f --no-pager"
echo    "    看后台日志 journalctl -u dnf90admin -f --no-pager"
echo
echo -e "  ${YELLOW}下一步：${NC}"
echo    "    1) 云服务器【安全组】放行：TCP 18112/7001/10000-10300/${ADMIN_PORT}、UDP 2311-2312/30000-31023"
echo    "    2) 浏览器打开上面的后台地址进行运营操作"
echo    "    3) 把公网 IP 填进登录器（游戏配置 → 远程模式 → 服务器 IP），保存后即可登录"
echo
