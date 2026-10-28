@echo off
setlocal
call "%ProgramFiles(x86)%\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars32.bat" >nul
if errorlevel 1 exit /b %ERRORLEVEL%
cl /nologo /W4 /EHsc /Fe:"%~dp0..\..\runtime\tmp\test-wedding-tree.exe" /Fo:"%~dp0..\..\runtime\tmp\test-wedding-tree.obj" "%~dp0test_wedding_tree.cpp"
if errorlevel 1 exit /b %ERRORLEVEL%
"%~dp0..\..\runtime\tmp\test-wedding-tree.exe"
exit /b %ERRORLEVEL%
