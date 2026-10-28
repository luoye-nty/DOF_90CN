# Self-test for the Anton raid probe in a NON-game 32-bit host.
#
# The probe used to be RaidProbe.dll and this script loaded that module directly.
# It now lives inside 90CN.dll (client-patch\raid_probe.cpp), so the host loads
# 90CN.dll and calls the exported Install90CNRaidProbe(): the same entry 90CN's
# install worker calls, minus Start90CNPatch (which waits up to two minutes for
# the game's signatures in a host that is not DNF.exe).
#
# Verifies: the module loads, DllMain survives, the probe installs, its dry-run
# self-test runs against the real DNF.exe on disk, RaidProbe.log is written, and
# the host process stays alive.
#
# Run it with 32-BIT PowerShell:
#   %WINDIR%\SysWOW64\WindowsPowerShell\v1.0\powershell.exe -File <this>

$ErrorActionPreference = 'Stop'
$dll = (Resolve-Path (Join-Path $PSScriptRoot '..\Release\90CN.dll')).Path

$src = @'
using System;
using System.Runtime.InteropServices;
public static class Host {
    [DllImport("kernel32", SetLastError = true, CharSet = CharSet.Unicode)]
    public static extern IntPtr LoadLibraryW(string path);
    [DllImport("kernel32", SetLastError = true, CharSet = CharSet.Ansi)]
    public static extern IntPtr GetProcAddress(IntPtr module, string name);
    [DllImport("kernel32")]
    public static extern uint GetLastError();
}
'@
Add-Type -TypeDefinition $src

Write-Host ("host is 32-bit : {0}" -f (-not [Environment]::Is64BitProcess))
Write-Host ("host exe       : {0}" -f ([System.Diagnostics.Process]::GetCurrentProcess().MainModule.FileName))
Write-Host ("module         : {0}" -f $dll)

$h = [Host]::LoadLibraryW($dll)
$err = [Host]::GetLastError()
Write-Host ("LoadLibraryW   : handle=0x{0:X}  lasterror={1}" -f [int64]$h, $err)
if ($h -eq [IntPtr]::Zero) {
    Write-Host 'RESULT: LoadLibrary FAILED'
    exit 1
}

$proc = [Host]::GetProcAddress($h, 'Install90CNRaidProbe')
Write-Host ("Install90CNRaidProbe: 0x{0:X}" -f [int64]$proc)
if ($proc -eq [IntPtr]::Zero) {
    Write-Host 'RESULT: export Install90CNRaidProbe is missing'
    exit 1
}

$before = Get-Date
$entry = [System.Runtime.InteropServices.Marshal]::GetDelegateForFunctionPointer(
    $proc, [Action])
$entry.Invoke()
Write-Host 'probe entry returned'

Write-Host 'sleeping 8s so the install thread can finish...'
Start-Sleep -Seconds 8
Write-Host 'host survived 8s'

$candidates = @(
    (Join-Path (Split-Path ([System.Diagnostics.Process]::GetCurrentProcess().MainModule.FileName)) 'RaidProbe.log'),
    (Join-Path $env:TEMP 'RaidProbe.log')
)
$found = $null
foreach ($c in $candidates) {
    if (Test-Path $c) {
        if ((Get-Item $c).LastWriteTime -ge $before) { $found = $c }
        Write-Host ("`n=== LOG: {0}  ({1} bytes) ===" -f $c, (Get-Item $c).Length)
        Get-Content $c -Tail 60 | ForEach-Object { Write-Host ("  " + $_) }
    } else {
        Write-Host ("`n(no log at {0})" -f $c)
    }
}

if (-not $found) {
    Write-Host 'RESULT: no fresh RaidProbe.log - the probe did not run'
    exit 2
}

# NOTE: deliberately NOT calling FreeLibrary. The probe owns worker threads;
# unloading the module while they run can fault inside unmapped image memory.
# The host exits right after this line, which releases everything cleanly.
Write-Host "`nRESULT: host completed cleanly (no crash), log at $found"
