@echo off
rem ===========================================================================
rem CubeProbe build + deploy.
rem
rem Standalone READ-ONLY probe for the avatar-compound family, specifically the
rem dream / absolute bind cube (op1119) hang. It is NOT part of 90CN.dll and
rem shares no file or symbol with it -- it only chains onto 90CN's existing
rem function-head hook (see CubeProbe.cpp header).
rem
rem   source : client-patch\probe_cube\CubeProbe.cpp
rem   module : <game dir>\CubeProbe.dll   (+ CubeProbe.ini)
rem   log    : <game dir>\CubeProbe.log   (appended at runtime)
rem
rem The game directory is located by searching D:\test for a folder holding
rem DNF.exe, so this file stays ASCII-only.
rem
rem NOTE: copying the DLL into the game directory does NOT load it. Inject it
rem with the same injector used for SceneProbe.dll / StoreStandInFix.dll,
rem AFTER the client is up (90CN.dll loads at process start; the probe must be
rem injected later so it can chain onto 90CN's hook).
rem ===========================================================================
setlocal EnableExtensions
pushd "%~dp0"

set "MSB="
for %%P in (
  "D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"
  "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\MSBuild\Current\Bin\MSBuild.exe"
  "C:\Program Files\Microsoft Visual Studio\2022\Community\MSBuild\Current\Bin\MSBuild.exe"
  "C:\Program Files (x86)\Microsoft Visual Studio\2019\BuildTools\MSBuild\Current\Bin\MSBuild.exe"
) do if not defined MSB if exist %%P set "MSB=%%~P"

if not defined MSB (
  echo [ERROR] MSBuild.exe not found
  popd
  exit /b 10
)

"%MSB%" CubeProbe.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0Release\CubeProbe.dll

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\test' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] could not locate the game directory under D:\test; dll stays in Release\
  popd
  exit /b 0
)

copy /y "Release\CubeProbe.dll" "%GAMEDIR%\CubeProbe.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy CubeProbe.dll into %GAMEDIR% - close the game first
) else (
  echo OK  module : %GAMEDIR%\CubeProbe.dll
)

if not exist "%GAMEDIR%\CubeProbe.ini" (
  copy /y "CubeProbe.ini" "%GAMEDIR%\CubeProbe.ini" >nul
  echo OK  config : %GAMEDIR%\CubeProbe.ini
) else (
  echo OK  config : %GAMEDIR%\CubeProbe.ini (existing file kept)
)

echo OK  log    : %GAMEDIR%\CubeProbe.log
echo.
echo NEXT: inject %GAMEDIR%\CubeProbe.dll into DNF.exe, then follow
echo       client-patch\probe_cube\README.md
popd
exit /b 0
