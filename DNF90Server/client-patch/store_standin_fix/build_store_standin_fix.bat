@echo off
rem ===========================================================================
rem StoreStandInFix build + deploy. Standalone experiment module: it is NOT
rem part of 90CN.dll and must not be merged until its verification passes.
rem
rem   source : client-patch\store_standin_fix\StoreStandInFix.cpp
rem   module : <game dir>\StoreStandInFix.dll   (+ StoreStandInFix.ini)
rem   log    : <game dir>\StoreStandInFix.log   (appended at runtime)
rem
rem The game directory is located by searching D:\test for a folder holding
rem DNF.exe, so this file stays ASCII-only.
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"

if not exist "%MSB%" (
  echo [ERROR] MSBuild not found: %MSB%
  exit /b 10
)

pushd "%~dp0"
"%MSB%" StoreStandInFix.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0Release\StoreStandInFix.dll

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\test' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] could not locate the game directory under D:\test; dll stays in Release\
  popd
  exit /b 0
)

copy /y "Release\StoreStandInFix.dll" "%GAMEDIR%\StoreStandInFix.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy StoreStandInFix.dll into %GAMEDIR% - close the game first
) else (
  echo OK  module : %GAMEDIR%\StoreStandInFix.dll
)

if not exist "%GAMEDIR%\StoreStandInFix.ini" (
  copy /y "StoreStandInFix.ini" "%GAMEDIR%\StoreStandInFix.ini" >nul
  echo OK  config : %GAMEDIR%\StoreStandInFix.ini
) else (
  echo OK  config : %GAMEDIR%\StoreStandInFix.ini (existing file kept)
)

echo OK  log    : %GAMEDIR%\StoreStandInFix.log
popd
exit /b 0
