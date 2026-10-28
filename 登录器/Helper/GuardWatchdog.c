/*
 * GuardWatchdog.c - Launcher survival watchdog DLL (anti-cheat foundation)
 *
 * Responsibility:
 *   Monitor launcher heartbeat via shared memory (Local\YYBLauncherGuard).
 *   If the launcher process is killed, crashes, or is fully suspended/frozen,
 *   the heartbeat stops advancing and this DLL terminates the game process
 *   (TerminateProcess(GetCurrentProcess(), 0)) after a timeout.
 *
 * Layering:
 *   - Launcher side also assigns DNF.exe into a Job Object with
 *     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so "launcher killed" is handled
 *     instantly by the kernel even if injection fails.
 *   - This DLL covers "launcher suspended/frozen" (Job Object cannot see it)
 *     and acts as a fallback for the kill case.
 *
 * Design notes:
 *   - Pure Win32 API, NO game version specific addresses -> safe for every
 *     DNF build (90 / S4A21 / S4A12).
 *   - No INI, no log file, no console output.
 *   - Shared memory layout (MUST match Services\LauncherGuard.cs):
 *       +0x00 int magic  ('RAUG' 0x47554152)
 *       +0x04 int heartbeat counter (incremented ~every 200ms by launcher)
 *       +0x08 int launcher pid
 *       +0x0C int reserved
 *   - If the map cannot be opened for 30s after injection, the thread just
 *     gives up silently (launcher may not be guarding this process; do not
 *     kill a game that is not under our control - not our business yet).
 *
 * Compile: gcc -m32 -shared -static-libgcc -O2 -o Helper\GuardWatchdog.dll Helper\GuardWatchdog.c
 */

#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#define GUARD_MAP_NAME   "Local\\YYBLauncherGuard"
#define GUARD_MAGIC      0x47554152   /* 'RAUG' */
#define OFF_MAGIC        0
#define OFF_HEARTBEAT    4
#define OFF_PID          8
#define OFF_TS           12

#define HEARTBEAT_TIMEOUT_MS  3000    /* launcher silent for 3s -> launcher dead/frozen */
#define CHECK_INTERVAL_MS     500
#define MAP_OPEN_RETRY_MS     30000   /* give up silently if launcher map never appears */

static volatile LONG g_stop = 0;
static HANDLE g_map = NULL;
static volatile LONG* g_heartbeat = NULL;  /* points into mapped view */

static void close_guard_map(void)
{
    if (g_heartbeat)
    {
        UnmapViewOfFile((LPVOID)((BYTE*)g_heartbeat - OFF_HEARTBEAT));
        g_heartbeat = NULL;
    }
    if (g_map)
    {
        CloseHandle(g_map);
        g_map = NULL;
    }
}

static int open_guard_map(void)
{
    if (g_map && g_heartbeat) return 1;

    g_map = OpenFileMappingA(FILE_MAP_ALL_ACCESS, FALSE, GUARD_MAP_NAME);
    if (!g_map) return 0;

    LPVOID base = MapViewOfFile(g_map, FILE_MAP_ALL_ACCESS, 0, 0, 0);
    if (!base)
    {
        CloseHandle(g_map);
        g_map = NULL;
        return 0;
    }

    if (*(volatile LONG*)base != GUARD_MAGIC)
    {
        /* stale/foreign map: do not trust it */
        UnmapViewOfFile(base);
        CloseHandle(g_map);
        g_map = NULL;
        return 0;
    }

    g_heartbeat = (volatile LONG*)((BYTE*)base + OFF_HEARTBEAT);
    return 1;
}

static DWORD WINAPI guard_thread(LPVOID lpParam)
{
    (void)lpParam;
    LONG last = -1;
    DWORD lastSeen = 0;
    DWORD start = GetTickCount();

    /* Wait for launcher heartbeat map (launcher creates it before injection). */
    while (!g_stop && !open_guard_map())
    {
        if (GetTickCount() - start >= MAP_OPEN_RETRY_MS)
        {
            /* No launcher map -> not under launcher guard. Stay silent. */
            return 0;
        }
        Sleep(200);
    }

    if (g_stop || !g_heartbeat) return 0;

    last = *g_heartbeat;
    lastSeen = GetTickCount();

    while (!g_stop)
    {
        Sleep(CHECK_INTERVAL_MS);
        LONG cur = *g_heartbeat;
        DWORD now = GetTickCount();
        if (cur != last)
        {
            last = cur;
            lastSeen = now;
            continue;
        }
        if (now - lastSeen >= HEARTBEAT_TIMEOUT_MS)
        {
            /* Launcher heartbeat frozen -> launcher killed or suspended.
               Kill the game process so anti-cheat state cannot be bypassed. */
            TerminateProcess(GetCurrentProcess(), 0);
            return 0;
        }
    }
    return 0;
}

BOOL WINAPI DllMain(HINSTANCE hInstDLL, DWORD fdwReason, LPVOID lpvReserved)
{
    (void)lpvReserved;
    if (fdwReason == DLL_PROCESS_ATTACH)
    {
        DisableThreadLibraryCalls(hInstDLL);
        HANDLE hThread = CreateThread(NULL, 0, guard_thread, NULL, 0, NULL);
        if (hThread) CloseHandle(hThread);
    }
    else if (fdwReason == DLL_PROCESS_DETACH)
    {
        InterlockedExchange(&g_stop, 1);
        close_guard_map();
    }
    return TRUE;
}
