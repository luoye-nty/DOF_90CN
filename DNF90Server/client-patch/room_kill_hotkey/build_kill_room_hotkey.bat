@echo off
rem ===========================================================================
rem KillRoomHotkey.dll build + deploy
rem
rem   source : client-patch\room_kill_hotkey\KillRoomHotkey.cpp
rem   module : <game dir>\LYPlugins\KillRoomHotkey.dll
rem   log    : <game dir>\KillRoomHotkey.log
rem
rem The launcher injects every DLL in LYPlugins, so no manual selection is
rem needed. The server must be started with DNFBRIDGE_DEBUG_KILL_ROOM=true.
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Software\Microsoft\VisualStudio\Product\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" (
  echo [ERROR] MSBuild not found
  exit /b 10
)

pushd "%~dp0"
"%MSB%" KillRoomHotkey.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0Release\KillRoomHotkey.dll

rem Locate the game directory by looking for DNF.exe under the server folder,
rem so this batch stays ASCII-only (the client folder name is Chinese).
set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\dof\90cn\90server' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] game directory not found; dll stays in Release\
  popd
  exit /b 0
)
set "PLUGINDIR=%GAMEDIR%\LYPlugins"
if not exist "%PLUGINDIR%" mkdir "%PLUGINDIR%"
copy /y "Release\KillRoomHotkey.dll" "%PLUGINDIR%\KillRoomHotkey.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy into "%PLUGINDIR%" - close the game first
  popd
  exit /b 0
)
echo OK  module : %PLUGINDIR%\KillRoomHotkey.dll
popd
exit /b 0
