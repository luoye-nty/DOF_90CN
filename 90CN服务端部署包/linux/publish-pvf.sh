#!/usr/bin/env bash
# ============================================================================
#  发布客户端 Script.pvf 自动更新包（登录器"检测到不一致 → 点更新 → 下载替换"）
# ============================================================================
#
#  用法：
#    bash publish-pvf.sh <新的 Script.pvf 路径> <压缩包直链URL> [版本号] [整包客户端直链]
#    bash publish-pvf.sh --check <Script.pvf 路径> [直链URL]     # 只看 SHA/配置，不写库
#    bash publish-pvf.sh --list                                   # 查看当前发布记录
#    bash publish-pvf.sh --unpublish                              # 下架（登录器不再提示更新）
#
#  例：
#    bash publish-pvf.sh /root/new/Script.pvf "https://你的直链/落叶PVF-1017.zip" 2026.10.17
#
#  工作原理（三端联动的 PVF 更新链）：
#    · 认证服务对外接口 GET /launcher/v1/pvf/latest 会下发"当前发布"的
#      pvf_version / file_sha256 / download_url / client_download_url；
#    · 登录器（90 · 远程模式）启动自检时拿本机 DNF.exe 同目录 Script.pvf 的
#      SHA-256 与 file_sha256 比对，不一致就把登录按钮切成「更新」并禁止登录；
#    · 点「更新」后从 download_url 下载压缩包，解压覆盖客户端目录，再复核 SHA。
#
#  两个必须注意的点：
#    1. 压缩包（zip/7z/rar 均可）里请把 Script.pvf 放在**包根目录**，
#       登录器是"按条目解压到客户端目录"，嵌套一层目录会落到子目录里、SHA 复核失败。
#    2. 服务端自己那份 /opt/dnf90/runtime/data/dnf/Script.pvf 也要同步替换，
#       否则游戏服读到的还是旧数据（这份只影响服务端逻辑，不参与上面的比对）。
#
#  key_set 留空 = 不做 PVF 换钥（客户端 Script.pvf 未加密时就是这样）；
#  pvf_enabled=1 只用来打开"发布 / 更新校验"，不会给客户端注入密钥。
set -euo pipefail

CFG="${DNF90_AUTH_CONFIG:-/opt/dnf90/auth/config.json}"

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

# ---------- 读取认证服务配置里的 MySQL 连接信息（与 dnf90auth 同一份） ----------
load_mysql() {
  [ -f "$CFG" ] || { echo "找不到认证服务配置：$CFG" >&2; exit 1; }
  command -v python3 >/dev/null || { echo "需要 python3 解析 $CFG" >&2; exit 1; }
  local line
  line=$(python3 - "$CFG" <<'PY'
import json, sys
c = json.load(open(sys.argv[1], encoding="utf-8"))["mysql"]
print("{} {} {} {} {}".format(c["host"], c["port"], c["user"], c["password"], c["dbname"]))
PY
)
  read -r MYH MYP MYU MYPW MYD <<<"$line"
  MYSQL=(mysql -h"$MYH" -P"$MYP" -u"$MYU" -p"$MYPW" --default-character-set=utf8mb4 "$MYD")
}

ensure_table() {
  "${MYSQL[@]}" -e "CREATE TABLE IF NOT EXISTS pvf_keys (
  id            BIGINT       NOT NULL AUTO_INCREMENT,
  file_sha256   CHAR(64)     NOT NULL,
  pvf_version   VARCHAR(64)  NOT NULL,
  key_set       VARCHAR(96)  NOT NULL,
  active        TINYINT(1)   NOT NULL DEFAULT 1,
  download_url  VARCHAR(1024) NOT NULL DEFAULT '',
  pvf_enabled   TINYINT(1)   NOT NULL DEFAULT 1,
  client_download_url VARCHAR(1024) NOT NULL DEFAULT '',
  created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_pvf_keys_sha (file_sha256)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;" >/dev/null
}

load_mysql

case "${1:-}" in
  --list)
    ensure_table
    COUNT=$("${MYSQL[@]}" -N -B -e "SELECT COUNT(*) FROM pvf_keys WHERE active = 1;")
    if [ "$COUNT" = "0" ]; then
      echo "当前没有生效的发布记录 → 登录器不会提示 PVF 更新。"
    else
      "${MYSQL[@]}" -e "SELECT id, pvf_version, file_sha256, active, pvf_enabled, download_url FROM pvf_keys ORDER BY id DESC LIMIT 10;"
    fi
    exit 0
    ;;
  --unpublish)
    ensure_table
    "${MYSQL[@]}" -e "UPDATE pvf_keys SET active = 0 WHERE active = 1;"
    echo "已下架：登录器不再提示客户端更新（/launcher/v1/pvf/latest 会返回 enabled=false）。"
    exit 0
    ;;
  --check|--dry-run)
    PVF="${2:?用法: publish-pvf.sh --check <Script.pvf> [直链URL]}"
    URL="${3:-}"
    ARG_VER="${4:-}"
    ARG_CLIENT="${5:-}"
    DRY=1
    ;;
  ""|-h|--help)
    usage
    ;;
  *)
    PVF="$1"
    URL="${2:?缺少压缩包直链 URL}"
    ARG_VER="${3:-}"
    ARG_CLIENT="${4:-}"
    DRY=0
    ;;
esac

VER="${ARG_VER:-$(date +%Y.%m.%d)}"
CLIENT_URL="$ARG_CLIENT"

[ -f "$PVF" ] || { echo "找不到文件：$PVF" >&2; exit 1; }
case "$URL" in
  ''|*\'*) [ "$DRY" = 1 ] || { echo "直链不能为空，且不能包含单引号" >&2; exit 1; } ;;
  http://*|https://*) ;;
  *) [ "$DRY" = 1 ] || { echo "直链必须以 http:// 或 https:// 开头：$URL" >&2; exit 1; } ;;
esac

SHA=$(sha256sum "$PVF" | awk '{print $1}')
SIZE=$(stat -c%s "$PVF" 2>/dev/null || stat -f%z "$PVF")
echo "MySQL      : $MYU@$MYH:$MYP/$MYD"
echo "Script.pvf : $PVF ($SIZE 字节)"
echo "SHA-256    : $SHA"
echo "版本号     : $VER"
echo "直链       : ${URL:-（未填，仅检查）}"
[ -n "$CLIENT_URL" ] && echo "整包直链   : $CLIENT_URL"

if [ "$DRY" = 1 ]; then
  cat <<EOF

—— 以上为 --check 结果，未写库。
发布命令：
  bash $0 "$PVF" "你的压缩包直链" $VER
EOF
  exit 0
fi

ensure_table
# 与运维工具 UpsertPvfKey 的语义一致：先让旧的 active 记录全部下架，再按 sha 写入当前发布。
"${MYSQL[@]}" -e "UPDATE pvf_keys SET active = 0 WHERE active = 1;"
"${MYSQL[@]}" <<SQL
INSERT INTO pvf_keys (file_sha256, pvf_version, key_set, active, download_url, pvf_enabled, client_download_url)
VALUES ('$SHA', '$VER', '', 1, '$URL', 1, '$CLIENT_URL')
ON DUPLICATE KEY UPDATE pvf_version = VALUES(pvf_version), key_set = '',
  active = 1, download_url = VALUES(download_url), pvf_enabled = 1,
  client_download_url = VALUES(client_download_url);
SQL

echo
echo "当前生效的发布记录："
"${MYSQL[@]}" -e "SELECT id, pvf_version, file_sha256, download_url, active, pvf_enabled FROM pvf_keys WHERE active = 1;"
echo
echo "已发布。玩家端效果：重新打开登录器 → 自检发现本机 Script.pvf 与发布版不一致"
echo "→ 按钮变「更新」→ 下载直链压缩包解压覆盖 → 复核 SHA 后即可登录。"
echo "别忘了同步服务端 /opt/dnf90/runtime/data/dnf/Script.pvf（不影响上面的比对）。"
