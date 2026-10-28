@echo off
chcp 65001 >nul
setlocal EnableExtensions
title 替换 RenameLogNames.dll 为 v5（行内名字兼容版 / 可选）

rem ---------------------------------------------------------------------------
rem 先看这里：这个 DLL 现在【不需要注入】了。
rem
rem 2026-09-23 查清：角色名本来就在协议里（行的第三个字段是 u32 长度 + GBK 字节，
rem 不是"键"）。服务端已改成行内带名字，客户端自己就会显示，不再需要任何补丁。
rem
rem 推荐做法（三选一，都不影响该窗口）：
rem   1) 把 RenameLogNames.dll 从注入列表里删掉；或
rem   2) 放一个空文件 RenameLogNames.off 到游戏目录（本模块会自我禁用）；或
rem   3) 换成 v5（本脚本做的事）：它会识别新版布局并自动成为空操作。
rem
rem 为什么要换 v5：v4 的行遍历写死 "1 + 9*行数"，新版行是变长的；如果客户端还挂着
rem v4 再打开这个窗口，它会把名字字节当成"名字个数"，有越界读取的风险。
rem
rem 客户端运行时 DLL 被占用无法覆盖，所以 v5 先编到 Release_v5\。
rem 关掉游戏客户端后运行本脚本即可替换。
rem ---------------------------------------------------------------------------

set "NEW=%~dp0Release_v5\RenameLogNames.dll"
set "OLD=%~dp0Release\RenameLogNames.dll"

tasklist /FI "IMAGENAME eq DNF.exe" 2>nul | find /I "DNF.exe" >nul
if not errorlevel 1 (
  echo [错误] DNF.exe 还在运行，请先完全退出游戏客户端再运行本脚本。
  pause
  exit /b 12
)

if not exist "%NEW%" (
  echo [错误] 找不到 %NEW%
  echo        请先运行 build_rename_log_names.bat（或用 MSBuild 指定 /p:OutDir=Release_v5\）。
  pause
  exit /b 10
)

if exist "%OLD%" (
  copy /y "%OLD%" "%OLD%.bak_v4" >nul
  echo   旧版已备份为 RenameLogNames.dll.bak_v4
)

copy /y "%NEW%" "%OLD%" >nul
if errorlevel 1 (
  echo [错误] 覆盖失败，请确认客户端已退出。
  pause
  exit /b 14
)

echo   OK  已替换: %OLD%
echo       v5 = 行内名字布局下自动空操作；旧版服务端（9 字节行 + 尾部名字块）仍可用。
certutil -hashfile "%OLD%" SHA256 | findstr /r /v "hash CertUtil"
pause
exit /b 0
