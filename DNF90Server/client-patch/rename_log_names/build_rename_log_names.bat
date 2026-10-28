@echo off
rem ===========================================================================
rem RenameLogNames build. Fixed names, no per-run suffixes:
rem   source : client-patch\rename_log_names\RenameLogNames.cpp
rem   module : client-patch\rename_log_names\Release\RenameLogNames.dll
rem   log    : <game dir>\RenameLogNames.log   (appended by the module)
rem
rem The DLL is NOT copied into the game directory: injection is done by the
rem operator. This file only builds it and prints where it landed.
rem ===========================================================================
setlocal EnableExtensions
set "MSB=D:\Program Files\Microsoft Visual Studio\18\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" set "MSB=C:\Program Files\Microsoft Visual Studio\2022\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" set "MSB=C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\MSBuild\Current\Bin\MSBuild.exe"
if not exist "%MSB%" (
  echo [ERROR] MSBuild not found.
  exit /b 10
)

pushd "%~dp0"
"%MSB%" RenameLogNames.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] build failed
  popd
  exit /b 1
)
echo.
echo OK  module : %~dp0Release\RenameLogNames.dll
echo     inject it into DNF.exe yourself; the module logs to ^<game dir^>\RenameLogNames.log
popd
exit /b 0
