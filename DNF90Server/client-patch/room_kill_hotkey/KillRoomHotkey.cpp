// KillRoomHotkey.dll -- standalone LYPlugins test helper.
//
// Press the key left of "1" (`~`, VK_OEM_3) inside a dungeon to ask the server
// to kill every live hostile in the room the character is standing in.
//
// Why this is a server request and not a client-side HP edit:
//   The 90CN server owns the monster death transaction. Zeroing HP in the
//   client alone leaves the room unfinished server-side: no drops, no quest
//   triggers, no settlement, and the monsters come back when the room is
//   re-entered. The request below reuses the server's own validated
//   "kill current room" path (DebugKillCurrentRoom), which replays the normal
//   per-monster death handlers, so everything downstream behaves exactly like
//   a legitimate clear.
//
// Transport:
//   The body is written through the client's own upper-channel packet writer,
//   the same functions 90CN.dll uses for its "BMU1" private control:
//     u16 opcode 1456 | u32 "KRM1" | u8 version | u8 0 | u16 token
//   1456 is an intentional hole in the current CmdPacket enum, so it can never
//   collide with a real game command. The server only accepts this envelope
//   when it was started with DNFBRIDGE_DEBUG_KILL_ROOM=true.
//
// Threading:
//   The writer belongs to the game's UI thread, so the polling thread only
//   posts a window message; the actual write happens inside the subclassed
//   window procedure.
//
// Nothing here patches game code, so removing the DLL is enough to remove the
// feature.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>

namespace {

// Client build 90CN targets. Verified against D:\dof\90cn\90server\DOF90客户端\DNF.exe:
//   0x03054640  A1 BC A4 2E 05 C3        mov eax,[0x52EA4BC]; ret
//   0x03054EB0  55 8B EC 6A FF           begin packet (writes the opcode)
//   0x03054FC0  55 8B EC 80 79 08 00     append u8  (cmp byte [ecx+8],0)
//   0x03054FF0  55 8B EC 80 79 08 00     append u16
//   0x03055020  55 8B EC 80 79 08 00     append u32
//   0x030566F0  55 8B EC B8 50 00 04 00  flush
constexpr uintptr_t kRvaWriterValue = 0x03054640u;
constexpr uintptr_t kRvaWriterBegin = 0x03054EB0u;
constexpr uintptr_t kRvaWriterU8 = 0x03054FC0u;
constexpr uintptr_t kRvaWriterU16 = 0x03054FF0u;
constexpr uintptr_t kRvaWriterU32 = 0x03055020u;
constexpr uintptr_t kRvaWriterFlush = 0x030566F0u;
// The scalar writers bail out when this byte is zero.
constexpr uintptr_t kWriterReadyOffset = 0x8u;

constexpr unsigned short kCapabilityOpcode = 1456u;
constexpr unsigned int kKillRoomMagic = 0x314D524Bu; // "KRM1" little-endian
constexpr unsigned char kKillRoomVersion = 1u;

constexpr UINT kPollIntervalMs = 25u;
constexpr DWORD kMinSendGapMs = 200u;
constexpr int kWriterWaitLimitMs = 60000;
constexpr int kWindowWaitLimitMs = 120000;

typedef void* (__cdecl* UpperWriterValueFn)();
typedef void (__thiscall* UpperWriterScalarFn)(void*, unsigned int);
typedef void (__cdecl* UpperWriterFlushFn)();
typedef BOOL (WINAPI* QueueClientNoticeFn)(const wchar_t*, unsigned int);

volatile LONG g_started = 0;
volatile LONG g_shuttingDown = 0;
HWND g_window = nullptr;
WNDPROC g_originalWindowProc = nullptr;
UINT g_killRoomMessage = 0;
DWORD g_lastSendTick = 0;
unsigned short g_token = 0;
char g_logPath[MAX_PATH] = {0};

void ResolveLogPath()
{
    wchar_t modulePath[MAX_PATH] = {0};
    DWORD length = GetModuleFileNameW(nullptr, modulePath, MAX_PATH);
    if (length == 0 || length >= MAX_PATH) return;
    for (DWORD index = length; index > 0; --index) {
        if (modulePath[index - 1] == L'\\' || modulePath[index - 1] == L'/') {
            modulePath[index] = L'\0';
            break;
        }
    }
    wchar_t logPath[MAX_PATH] = {0};
    wcscpy_s(logPath, modulePath);
    wcscat_s(logPath, L"KillRoomHotkey.log");
    WideCharToMultiByte(CP_ACP, 0, logPath, -1, g_logPath, MAX_PATH, nullptr, nullptr);
}

void Log(const char* format, ...)
{
    if (g_logPath[0] == '\0') return;
    FILE* file = nullptr;
    if (fopen_s(&file, g_logPath, "a") != 0 || !file) return;
    SYSTEMTIME now = {};
    GetLocalTime(&now);
    fprintf(file, "%04u-%02u-%02u %02u:%02u:%02u.%03u ",
        now.wYear, now.wMonth, now.wDay, now.wHour, now.wMinute, now.wSecond,
        now.wMilliseconds);
    va_list args;
    va_start(args, format);
    vfprintf(file, format, args);
    va_end(args);
    fputc('\n', file);
    fclose(file);
}

// Optional feedback through 90CN.dll's exported overlay notice. Resolved with
// GetProcAddress so this DLL keeps zero link-time dependency on 90CN.dll: if
// the compatibility DLL is absent the hotkey still works, just silently.
void ShowSentNotice()
{
    HMODULE compatibility = GetModuleHandleW(L"90CN.dll");
    if (!compatibility) return;
    QueueClientNoticeFn notice = reinterpret_cast<QueueClientNoticeFn>(
        GetProcAddress(compatibility, "Queue90CNClientNotice"));
    if (!notice) return;
    static const wchar_t kText[] = L"清房请求已发送";
    notice(kText, static_cast<unsigned int>(sizeof(kText) / sizeof(kText[0]) - 1));
}

bool BytesMatch(const void* address, const unsigned char* expected, size_t length)
{
    if (!address || !expected) return false;
    return memcmp(address, expected, length) == 0;
}

// Refuse to run against a client build whose writer entries are not the ones
// this DLL was written for. A wrong RVA would otherwise send a corrupt packet
// through an unrelated function.
bool WriterTargetsReady(uintptr_t base)
{
    static const unsigned char kValuePrologue[] = {0xA1, 0xBC, 0xA4, 0x2E, 0x05, 0xC3};
    static const unsigned char kBeginPrologue[] = {0x55, 0x8B, 0xEC, 0x6A, 0xFF};
    static const unsigned char kScalarPrologue[] = {0x55, 0x8B, 0xEC, 0x80, 0x79, 0x08, 0x00};
    static const unsigned char kFlushPrologue[] = {0x55, 0x8B, 0xEC, 0xB8, 0x50, 0x00, 0x04, 0x00};
    return BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterValue), kValuePrologue, sizeof(kValuePrologue)) &&
        BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterBegin), kBeginPrologue, sizeof(kBeginPrologue)) &&
        BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterU8), kScalarPrologue, sizeof(kScalarPrologue)) &&
        BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterU16), kScalarPrologue, sizeof(kScalarPrologue)) &&
        BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterU32), kScalarPrologue, sizeof(kScalarPrologue)) &&
        BytesMatch(reinterpret_cast<const void*>(base + kRvaWriterFlush), kFlushPrologue, sizeof(kFlushPrologue));
}

bool SendKillRoomRequest()
{
    uintptr_t base = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    if (!base) return false;

    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(base + kRvaWriterValue);
        UpperWriterScalarFn beginPacket = reinterpret_cast<UpperWriterScalarFn>(base + kRvaWriterBegin);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(base + kRvaWriterU8);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(base + kRvaWriterU16);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(base + kRvaWriterU32);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(base + kRvaWriterFlush);

        void* writer = value();
        if (!writer ||
            *reinterpret_cast<unsigned char*>(reinterpret_cast<unsigned char*>(writer) + kWriterReadyOffset) == 0) {
            Log("send skipped: upper writer not ready");
            return false;
        }

        unsigned short token = ++g_token;
        if (token == 0) token = ++g_token;

        // The body must be written in exactly this order: the u16 entry starts
        // a new packet (and resets the body cursor), the scalar entries append.
        beginPacket(writer, kCapabilityOpcode);
        writeU32(value(), kKillRoomMagic);
        writeU8(value(), kKillRoomVersion);
        writeU8(value(), 0);
        writeU16(value(), static_cast<unsigned int>(token));
        flush();

        Log("kill-room request sent token=%u", static_cast<unsigned int>(token));
        ShowSentNotice();
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("send exception code=0x%08X", static_cast<unsigned int>(GetExceptionCode()));
        return false;
    }
}

LRESULT CALLBACK ProxyWindowProc(HWND window, UINT message, WPARAM wParam, LPARAM lParam)
{
    if (message == g_killRoomMessage) {
        DWORD now = GetTickCount();
        if (now - g_lastSendTick >= kMinSendGapMs) {
            g_lastSendTick = now;
            SendKillRoomRequest();
        }
        return 0;
    }
    if (!g_originalWindowProc) return DefWindowProcW(window, message, wParam, lParam);
    return CallWindowProcW(g_originalWindowProc, window, message, wParam, lParam);
}

BOOL CALLBACK FindMainWindow(HWND window, LPARAM parameter)
{
    DWORD processID = 0;
    GetWindowThreadProcessId(window, &processID);
    if (processID != GetCurrentProcessId() || !IsWindowVisible(window) ||
        GetWindow(window, GW_OWNER) != nullptr) {
        return TRUE;
    }
    *reinterpret_cast<HWND*>(parameter) = window;
    return FALSE;
}

HWND WaitForMainWindow()
{
    int waited = 0;
    while (waited < kWindowWaitLimitMs && !g_shuttingDown) {
        HWND window = nullptr;
        EnumWindows(&FindMainWindow, reinterpret_cast<LPARAM>(&window));
        if (window) return window;
        Sleep(250);
        waited += 250;
    }
    return nullptr;
}

bool InstallWindowProc(HWND window)
{
    WNDPROC original = reinterpret_cast<WNDPROC>(GetWindowLongPtrW(window, GWLP_WNDPROC));
    if (!original) {
        Log("install failed: window has no procedure hwnd=%p", window);
        return false;
    }
    SetLastError(ERROR_SUCCESS);
    SetWindowLongPtrW(window, GWLP_WNDPROC, reinterpret_cast<LONG_PTR>(&ProxyWindowProc));
    DWORD error = GetLastError();
    if (error != ERROR_SUCCESS) {
        Log("install failed: SetWindowLongPtr hwnd=%p error=%lu", window, error);
        return false;
    }
    g_originalWindowProc = original;
    g_window = window;
    Log("window hook installed hwnd=%p original=%p message=0x%04X", window, original, g_killRoomMessage);
    return true;
}

bool ForegroundIsThisGame()
{
    HWND foreground = GetForegroundWindow();
    if (!foreground) return false;
    DWORD processID = 0;
    GetWindowThreadProcessId(foreground, &processID);
    return processID == GetCurrentProcessId();
}

DWORD WINAPI WorkerThread(LPVOID)
{
    ResolveLogPath();
    Log("loaded pid=%lu", GetCurrentProcessId());

    uintptr_t base = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    int waited = 0;
    while (!WriterTargetsReady(base) && waited < kWriterWaitLimitMs && !g_shuttingDown) {
        Sleep(250);
        waited += 250;
    }
    if (!WriterTargetsReady(base)) {
        Log("disabled: client writer entry points do not match this DLL (base=0x%08X waited_ms=%d)",
            static_cast<unsigned int>(base), waited);
        return 1;
    }
    Log("writer entry points verified base=0x%08X waited_ms=%d", static_cast<unsigned int>(base), waited);

    HWND window = WaitForMainWindow();
    if (!window) {
        Log("disabled: game main window not found");
        return 1;
    }
    g_killRoomMessage = RegisterWindowMessageW(L"LYKillRoomHotkey.v1");
    if (g_killRoomMessage == 0) {
        Log("disabled: RegisterWindowMessage failed error=%lu", GetLastError());
        return 1;
    }
    if (!InstallWindowProc(window)) return 1;
    Log("ready: press VK_OEM_3 (~) inside a dungeon room");

    bool previousDown = false;
    while (!g_shuttingDown) {
        if (!IsWindow(g_window)) {
            g_window = nullptr;
            g_originalWindowProc = nullptr;
            HWND replacement = WaitForMainWindow();
            if (replacement) InstallWindowProc(replacement);
        }
        bool down = (GetAsyncKeyState(VK_OEM_3) & 0x8000) != 0;
        if (down && !previousDown && g_window && ForegroundIsThisGame() && !IsIconic(g_window)) {
            if (!PostMessageW(g_window, g_killRoomMessage, 0, 0)) {
                Log("PostMessage failed error=%lu", GetLastError());
            }
        }
        previousDown = down;
        Sleep(kPollIntervalMs);
    }
    return 0;
}

} // namespace

BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID)
{
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(instance);
        if (InterlockedCompareExchange(&g_started, 1, 0) == 0) {
            HANDLE thread = CreateThread(nullptr, 0, &WorkerThread, nullptr, 0, nullptr);
            if (thread) {
                CloseHandle(thread);
            } else {
                InterlockedExchange(&g_started, 0);
            }
        }
    } else if (reason == DLL_PROCESS_DETACH) {
        InterlockedExchange(&g_shuttingDown, 1);
        HWND window = g_window;
        WNDPROC original = g_originalWindowProc;
        if (window && original && IsWindow(window)) {
            SetWindowLongPtrW(window, GWLP_WNDPROC, reinterpret_cast<LONG_PTR>(original));
        }
        g_window = nullptr;
        g_originalWindowProc = nullptr;
    }
    return TRUE;
}
