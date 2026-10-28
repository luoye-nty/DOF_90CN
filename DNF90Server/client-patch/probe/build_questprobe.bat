@echo off
rem ===========================================================================
rem QuestProbe build + deploy. Fixed names, no per-run suffixes:
rem   source : client-patch\probe\QuestProbe.cpp
rem   module : <game dir>\QuestProbe.dll
rem   log    : <game dir>\QuestProbe.log   (appended by the module at runtime)
rem
rem The game directory is located by searching D:\test for a folder that holds
rem DNF.exe, so this file stays ASCII-only and needs no encoding care.
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"

if not exist "%MSB%" (
  echo [ERROR] MSBuild not found: %MSB%
  exit /b 10
)

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\test' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [ERROR] could not locate the game directory under D:\test
  exit /b 11
)

pushd "%~dp0"
"%MSB%" QuestProbe.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)

copy /y "Release\QuestProbe.dll" "%GAMEDIR%\QuestProbe.dll" >nul
if errorlevel 1 (
  echo [ERROR] cannot replace %GAMEDIR%\QuestProbe.dll - close the game first
  popd
  exit /b 2
)

echo OK  module : %GAMEDIR%\QuestProbe.dll
echo OK  log    : %GAMEDIR%\QuestProbe.log
popd
exit /b 0
