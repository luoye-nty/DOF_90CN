#!/usr/bin/env bash
# ============================================================
#  DNF90 网页管理后台 · 手动补装脚本（不需要重跑 install.sh）
#
#  前提：服务端已经装好（/opt/dnf90 存在，dnf90server/dnf90auth 在跑）
#
#  用法：
#    1) 上传 dnf90admin（同目录、或 /root/、或 /root/linux/bin/ 均可）
#    2) sudo bash 补装-网页后台.sh
#
#  脚本会自动：读 dnfbridge.toml 的 admin token、读 auth/config.json 的 MySQL 口令，
#  生成 admin.json、注册 systemd 服务、放行防火墙、启动并检查。
# ============================================================
set -euo pipefail

INSTALL_DIR="/opt/dnf90"
ADMIN_PORT="18114"

# ★ 想自己设后台密码就改这一行；不改则自动随机生成并在结尾打印 ★
ADMIN_PW="1248243654Qq"

RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; CYAN='\033[36m'; NC='\033[0m'
say(){  echo -e "${CYAN}[DNF90]${NC} $*"; }
ok(){   echo -e "${GREEN}[  OK ]${NC} $*"; }
warn(){ echo -e "${YELLOW}[ WARN]${NC} $*"; }
die(){  echo -e "${RED}[FAIL]${NC} $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "请用 root 运行：sudo bash $0"

# ---------- 1. 定位服务端 ----------
[ -d "$INSTALL_DIR" ] || die "找不到 $INSTALL_DIR —— 请先跑过一次 install.sh"
[ -f "$INSTALL_DIR/runtime/configs/dnfbridge.toml" ] || die "找不到 dnfbridge.toml，服务端似乎不完整"
[ -f "$INSTALL_DIR/auth/config.json" ] || die "找不到 auth/config.json，服务端似乎不完整"

# ---------- 2. 定位 dnf90admin 二进制 ----------
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN=""
for c in "$SELF_DIR/dnf90admin" "$SELF_DIR/bin/dnf90admin" /root/dnf90admin /root/linux/bin/dnf90admin "$INSTALL_DIR/bin/dnf90admin"; do
  if [ -f "$c" ]; then BIN="$c"; break; fi
done
[ -n "$BIN" ] || die "找不到 dnf90admin 二进制。请把 linux/bin/dnf90admin 上传到 /root/ 后重试"

# 架构自检（防止把 ARM 版传上来）
ARCH_BIN="$(od -An -tx1 -N20 "$BIN" | tr -d ' \n')"
case "$ARCH_BIN" in
  7f454c46*) : ;;                       # ELF magic
  *) die "$BIN 不是有效的 Linux 可执行文件（可能上传时被当成文本转换了）" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  WANT_MACHINE="3e" ;;
  aarch64|arm64) WANT_MACHINE="b7" ;;
  *) die "不支持的 CPU 架构：$(uname -m)" ;;
esac
GOT_MACHINE="$(od -An -tx1 -j18 -N1 "$BIN" | tr -d ' \n')"
[ "$GOT_MACHINE" = "$WANT_MACHINE" ] || die "二进制架构不匹配：本机 $(uname -m)，但上传的是另一种架构的 dnf90admin"

# ---------- 3. 提取配置 ----------
TOKEN="$(awk '/^\[admin\]/{f=1;next} /^\[/{f=0} f&&/^[[:space:]]*token[[:space:]]*=/{gsub(/^[[:space:]]*token[[:space:]]*=[[:space:]]*"/,"");gsub(/"[[:space:]]*$/,"");print;exit}' \
        "$INSTALL_DIR/runtime/configs/dnfbridge.toml")"
[ -n "$TOKEN" ] || die "无法从 dnfbridge.toml 的 [admin] 段读到 token"

MYSQL_PW="$(grep -o '"password"[[:space:]]*:[[:space:]]*"[^"]*"' "$INSTALL_DIR/auth/config.json" | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')"
[ -n "$MYSQL_PW" ] || die "无法从 auth/config.json 读到 MySQL 口令"

[ -n "$ADMIN_PW" ] || ADMIN_PW="dnf90$(openssl rand -hex 6 2>/dev/null || head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
SESSION_SECRET="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

say "admin token  : ${TOKEN:0:12}...（已从 dnfbridge.toml 自动读取）"
say "MySQL 口令    : 已从 auth/config.json 自动读取"
say "后台端口      : ${ADMIN_PORT}"

# ---------- 4. 停旧服务 / 装二进制 ----------
if systemctl list-unit-files 2>/dev/null | grep -q '^dnf90admin\.service'; then
  say "停止旧的 dnf90admin ..."
  systemctl stop dnf90admin.service 2>/dev/null || true
  systemctl reset-failed dnf90admin.service 2>/dev/null || true
fi

mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/admin" "$INSTALL_DIR/logs"
install -m 0755 "$BIN" "$INSTALL_DIR/bin/dnf90admin"
rm -f "$INSTALL_DIR/bin/admin.json" "$INSTALL_DIR/bin/admin.json.tmp-"* 2>/dev/null || true
ok "二进制已安装到 $INSTALL_DIR/bin/dnf90admin"

# ---------- 5. 写 admin.json ----------
cat > "$INSTALL_DIR/admin/admin.json" <<EOF
{
  "listen": ":${ADMIN_PORT}",
  "password": "${ADMIN_PW}",
  "session_secret": "${SESSION_SECRET}",
  "game_admin_url": "http://127.0.0.1:18111",
  "game_admin_token": "${TOKEN}",
  "mysql": { "host": "127.0.0.1", "port": 3306, "user": "dnf90", "password": "${MYSQL_PW}", "dbname": "dnf_local" },
  "title": "DNF90 管理后台",
  "trust_proxy": false
}
EOF
chmod 600 "$INSTALL_DIR/admin/admin.json"
ok "配置已写入 $INSTALL_DIR/admin/admin.json"

# ---------- 6. systemd ----------
cat > /etc/systemd/system/dnf90admin.service <<'EOF'
[Unit]
Description=DNF90 Web Admin (网页管理后台, HTTP 18114)
After=network-online.target mysql.service dnf90server.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=/opt/dnf90/admin
ExecStart=/opt/dnf90/bin/dnf90admin -config /opt/dnf90/admin/admin.json
Restart=always
RestartSec=3
StandardOutput=append:/opt/dnf90/logs/dnf90admin.log
StandardError=append:/opt/dnf90/logs/dnf90admin.log

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable dnf90admin.service >/dev/null 2>&1 || true
ok "systemd 服务已注册"

# ---------- 7. 防火墙 ----------
if command -v ufw >/dev/null 2>&1; then
  ufw allow "${ADMIN_PORT}/tcp" >/dev/null 2>&1 || true
  ok "ufw 已放行 ${ADMIN_PORT}/tcp"
else
  warn "未装 ufw；请自行在云服务器【安全组】放行 TCP ${ADMIN_PORT}"
fi

# ---------- 8. 启动 ----------
say "启动网页后台 ..."
systemctl restart dnf90admin.service
sleep 3

echo
echo "============================================================"
if systemctl is-active --quiet dnf90admin.service; then
  ok "网页后台运行中"
  if command -v ss >/dev/null 2>&1; then
    ss -lntp 2>/dev/null | grep -q ":${ADMIN_PORT}" && ok "端口 ${ADMIN_PORT} 已在监听" || warn "端口 ${ADMIN_PORT} 未监听，见下方日志"
  fi
  if command -v curl >/dev/null 2>&1; then
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${ADMIN_PORT}/" || true)"
    [ "$code" = "200" ] && ok "本机自测 HTTP 200（服务正常）" || warn "本机自测返回 HTTP ${code}，见下方日志"
  fi
else
  warn "网页后台没起来，最近日志如下："
  echo "----------------------------------------------------------------"
  journalctl -u dnf90admin -n 30 --no-pager || true
  echo "----------------------------------------------------------------"
fi
echo "============================================================"
echo
echo -e "  ${CYAN}=== 网页管理后台 ===${NC}"
echo -e "  地址     : ${GREEN}http://<你的公网IP>:${ADMIN_PORT}${NC}"
echo -e "  后台密码 : ${GREEN}${ADMIN_PW}${NC}"
echo    "  配置文件 : /opt/dnf90/admin/admin.json（改完 systemctl restart dnf90admin）"
echo    "  查看日志 : journalctl -u dnf90admin -f --no-pager"
echo
echo    "  别忘了云服务器【安全组】放行 TCP ${ADMIN_PORT}"
echo
