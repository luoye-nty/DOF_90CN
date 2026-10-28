@echo off
setlocal EnableExtensions
set "MSB=D:\Software\Microsoft\VisualStudio\Product\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" (
  echo [ERROR] MSBuild not found
  exit /b 10
)
pushd "%~dp0"
"%MSB%" PvpGateFix.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  popd
  exit /b 1
)
set "GAMEDIR="
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "(Get-ChildItem 'D:\dof\90cn\90server' -Directory -ErrorAction SilentlyContinue | Where-Object { Test-Path (Join-Path $_.FullName 'DNF.exe') } | Select-Object -First 1).FullName"`) do set "GAMEDIR=%%I"
if "%GAMEDIR%"=="" (
  echo [WARN] game directory not found
  popd
  exit /b 0
)
set "PLUGINDIR=%GAMEDIR%\LYPlugins"
if not exist "%PLUGINDIR%" mkdir "%PLUGINDIR%"
copy /y "Release\PvpGateFix.dll" "%PLUGINDIR%\PvpGateFix.dll" >nul
echo OK  module : %PLUGINDIR%\PvpGateFix.dll
popd
exit /b 0
