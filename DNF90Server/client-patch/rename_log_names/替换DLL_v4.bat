@echo off
chcp 65001 >nul
setlocal EnableExtensions
title 替换 RenameLogNames.dll 为 9 字节行版（v4）

rem 客户端运行时 DLL 被占用无法覆盖，所以 v4 先编到 Release_v4\。
rem 关掉游戏客户端后运行本脚本，即可把它换到注入器使用的正式路径。

set "NEW=%~dp0Release_v4\RenameLogNames.dll"
set "OLD=%~dp0Release\RenameLogNames.dll"

tasklist /FI "IMAGENAME eq DNF.exe" 2>nul | find /I "DNF.exe" >nul
if not errorlevel 1 (
  echo [错误] DNF.exe 还在运行，请先完全退出游戏客户端再运行本脚本。
  pause
  exit /b 12
)

if not exist "%NEW%" (
  echo [错误] 找不到 %NEW%
  pause
  exit /b 10
)

if exist "%OLD%" (
  copy /y "%OLD%" "%OLD%.bak_v3" >nul
  echo   旧版已备份为 RenameLogNames.dll.bak_v3
)

copy /y "%NEW%" "%OLD%" >nul
if errorlevel 1 (
  echo [错误] 覆盖失败，请确认客户端已退出。
  pause
  exit /b 14
)

echo   OK  已替换: %OLD%
echo       新版本 = 9 字节行布局（name block 偏移 1+9*rows）
certutil -hashfile "%OLD%" SHA256 | findstr /r /v "hash CertUtil"
pause
exit /b 0
