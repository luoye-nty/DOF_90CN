#!/usr/bin/env bash
# DNF90 服务端 · 卸载脚本（保留数据库数据，避免误删角色）
# 用法： sudo bash uninstall.sh            # 只停服务、删程序，保留 /opt/dnf90 数据
#        sudo bash uninstall.sh --purge    # 连同 /opt/dnf90 一起删除（数据库 dnf_local 仍保留）
set -euo pipefail
INSTALL_DIR="/opt/dnf90"

[ "$(id -u)" = "0" ] || { echo "请用 root 运行：sudo bash uninstall.sh"; exit 1; }

echo "[DNF90] 停止并移除 systemd 服务 ..."
systemctl disable --now dnf90server.service dnf90auth.service dnf90admin.service 2>/dev/null || true
rm -f /etc/systemd/system/dnf90server.service /etc/systemd/system/dnf90auth.service /etc/systemd/system/dnf90admin.service
systemctl daemon-reload

if [ "${1:-}" = "--purge" ]; then
  echo "[DNF90] 删除 $INSTALL_DIR ..."
  rm -rf "$INSTALL_DIR"
  echo "[DNF90] 已删除。数据库 dnf_local 未删除（如需清理：DROP DATABASE dnf_local;）"
else
  echo "[DNF90] 程序已移除，数据保留在 $INSTALL_DIR（含配置与日志）。"
fi
echo "[DNF90] 完成。"
