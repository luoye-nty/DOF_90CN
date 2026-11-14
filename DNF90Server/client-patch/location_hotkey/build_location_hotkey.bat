@echo off
setlocal EnableExtensions
set "MSB=D:\Software\Microsoft\VisualStudio\Product\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" (
  echo [ERROR] MSBuild not found
  exit /b 10
)

pushd "%~dp0"
"%MSB%" LocationHotkey.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo OK  build : %~dp0build\LocationHotkey.dll

set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem -LiteralPath 'D:\dof\90cn' -Filter DNF.exe -File -Recurse -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.DirectoryName 'LYPlugins') } | Select-Object -First 1).DirectoryName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] game directory not found; dll stays in build\
  popd
  exit /b 0
)
set "PLUGINDIR=%GAMEDIR%\LYPlugins"
if not exist "%PLUGINDIR%" mkdir "%PLUGINDIR%"
copy /y "build\LocationHotkey.dll" "%PLUGINDIR%\LocationHotkey.dll" >nul
if errorlevel 1 (
  echo [WARN] cannot copy into "%PLUGINDIR%" - close the game first
  popd
  exit /b 0
)
echo OK  module : %PLUGINDIR%\LocationHotkey.dll
popd
exit /b 0
