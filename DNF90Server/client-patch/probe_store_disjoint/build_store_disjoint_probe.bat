@echo off
rem ===========================================================================
rem StoreDisjointProbe build + deploy.
rem
rem Standalone READ-ONLY probe for the personal-store disassembly machine's
rem "disassemble all" flow. It is NOT part of 90CN.dll and must not be merged
rem until its questions are answered (see README.md).
rem
rem   source : client-patch\probe_store_disjoint\StoreDisjointProbe.cpp
rem   module : <game dir>\StoreDisjointProbe.dll   (+ StoreDisjointProbe.ini)
rem   log    : <game dir>\StoreDisjointProbe.log   (appended at runtime)
rem
rem The game directory is located by searching D:\test for a folder holding
rem DNF.exe, so this file stays ASCII-only.
rem
rem NOTE: deploying the DLL into the game directory does NOT load it. Inject it
rem with whatever injector you normally use (same path as StoreStandInFix.dll).
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"

if not exist "%MSB%" (
  echo [ERROR] MSBuild not found: %MSB%
  exit /b 10
)

pushd "%~dp0"
"%MSB%" StoreDisjointProbe.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0Release\StoreDisjointProbe.dll

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\test' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] could not locate the game directory under D:\test; dll stays in Release\
  popd
  exit /b 0
)

copy /y "Release\StoreDisjointProbe.dll" "%GAMEDIR%\StoreDisjointProbe.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy StoreDisjointProbe.dll into %GAMEDIR% - close the game first
) else (
  echo OK  module : %GAMEDIR%\StoreDisjointProbe.dll
)

if not exist "%GAMEDIR%\StoreDisjointProbe.ini" (
  copy /y "StoreDisjointProbe.ini" "%GAMEDIR%\StoreDisjointProbe.ini" >nul
  echo OK  config : %GAMEDIR%\StoreDisjointProbe.ini
) else (
  echo OK  config : %GAMEDIR%\StoreDisjointProbe.ini (existing file kept)
)

echo OK  log    : %GAMEDIR%\StoreDisjointProbe.log
echo.
echo NEXT: inject %GAMEDIR%\StoreDisjointProbe.dll into DNF.exe, then follow
echo       the run sheet in client-patch\probe_store_disjoint\README.md
popd
exit /b 0
