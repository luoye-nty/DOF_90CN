@echo off
rem ===========================================================================
rem StoreDisjointBatch build + deploy.
rem
rem Standalone module: it is NOT part of 90CN.dll and must not be merged until
rem its live verification passes (see README.md).
rem
rem   source : client-patch\store_disjoint_batch\StoreDisjointBatch.cpp
rem   module : <game dir>\StoreDisjointBatch.dll   (+ StoreDisjointBatch.ini)
rem   log    : <game dir>\StoreDisjointBatch.log   (appended at runtime)
rem
rem The game directory is located by searching D:\test for a folder holding
rem DNF.exe, so this file stays ASCII-only.
rem
rem NOTE: copying the DLL into the game directory does NOT load it. Inject it
rem with whatever injector you normally use (same as StoreStandInFix.dll).
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"

if not exist "%MSB%" (
  echo [ERROR] MSBuild not found: %MSB%
  exit /b 10
)

pushd "%~dp0"
"%MSB%" StoreDisjointBatch.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0Release\StoreDisjointBatch.dll

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\test' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] could not locate the game directory under D:\test; dll stays in Release\
  popd
  exit /b 0
)

copy /y "Release\StoreDisjointBatch.dll" "%GAMEDIR%\StoreDisjointBatch.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy into %GAMEDIR% - close the game first
) else (
  echo OK  module : %GAMEDIR%\StoreDisjointBatch.dll
)

if not exist "%GAMEDIR%\StoreDisjointBatch.ini" (
  copy /y "StoreDisjointBatch.ini" "%GAMEDIR%\StoreDisjointBatch.ini" >nul
  echo OK  config : %GAMEDIR%\StoreDisjointBatch.ini
) else (
  echo OK  config : %GAMEDIR%\StoreDisjointBatch.ini (existing file kept)
)

echo OK  log    : %GAMEDIR%\StoreDisjointBatch.log
echo.
echo NEXT: inject %GAMEDIR%\StoreDisjointBatch.dll, then follow README.md
popd
exit /b 0
