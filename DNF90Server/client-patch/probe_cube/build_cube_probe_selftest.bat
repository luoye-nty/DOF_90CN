@echo off
rem ===========================================================================
rem CubeProbe self-test: build + run immediately, verifying the hook machinery
rem itself (it never touches the game).
rem
rem   native trampoline / E9 chain / version-mismatch refusal / lookup forwarder
rem   register+stack offsets -- all four must be green.
rem
rem ASCII only on purpose: cmd.exe parses this file with the OEM codepage.
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
echo MSBuild: %MSB%
"%MSB%" CubeProbeSelfTest.vcxproj /p:Configuration=Release /p:Platform=Win32 /nologo /v:m
if errorlevel 1 (
  echo [ERROR] self-test build failed
  popd
  exit /b 1
)

echo.
echo ---- running self-test ----
"Release\CubeProbeSelfTest.exe"
set "RC=%ERRORLEVEL%"
echo.
if "%RC%"=="0" (echo [OK] self-test passed) else (echo [FAIL] self-test failed rc=%RC%)
popd
exit /b %RC%
