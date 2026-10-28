// PvpGateFix.dll -- one-byte client patch for the arena entry gate.
//
// Current client EXE (image base 0x00400000):
//   RVA 0x2FE0EB3 : 75 04   jne 0x33E0EB9
// The block right before it is reached with eax = roster row:
//   cmp dword ptr [eax+0x1c], 8
//   jne 0x33E0EB9          <-- we force this branch
//   push 2 / push 0x18A    <-- "该角色没有完成风云崛起(1/5)" popup
//   jmp  exit
//
// Forcing the branch to always continue skips only the popup; the arena
// channel switch itself is untouched. If the bytes at the target do not match,
// nothing is patched.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdio.h>

namespace {

constexpr uintptr_t kPatchRva = 0x2FE0EB3u;
const unsigned char kExpected[2] = {0x75, 0x04};
const unsigned char kPatched[2] = {0xEB, 0x04};

// The other exit of the same gate: show message 0x18A and jump away.
//   push 0 / mov ecx,[global] / push esi / push 0x18A / call / jmp exit
// NOP-ing the whole block lets execution fall through to the normal path
// (0x33E0EA7) without the popup.
constexpr uintptr_t kMessageRva = 0x2FE0E8Fu;
const unsigned char kMessageExpected[24] = {
    0x6A, 0x00, 0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05,
    0x56, 0x68, 0x8A, 0x01, 0x00, 0x00, 0xE8, 0xFE,
    0xC2, 0xBF, 0xFF, 0xE9, 0x30, 0x0B, 0x00, 0x00};

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
    wcscat_s(logPath, L"PvpGateFix.log");
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

DWORD WINAPI Worker(LPVOID)
{
    ResolveLogPath();
    uintptr_t base = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    if (!base) {
        Log("disabled: no module base");
        return 1;
    }
    unsigned char* target = reinterpret_cast<unsigned char*>(base + kPatchRva);
    if (target[0] != kExpected[0] || target[1] != kExpected[1]) {
        Log("disabled: bytes at 0x%08X are %02X %02X, expected 75 04",
            static_cast<unsigned int>(base + kPatchRva), target[0], target[1]);
        return 1;
    }
    DWORD oldProtect = 0;
    if (!VirtualProtect(target, sizeof(kPatched), PAGE_EXECUTE_READWRITE, &oldProtect)) {
        Log("disabled: VirtualProtect failed error=%lu", GetLastError());
        return 1;
    }
    target[0] = kPatched[0];
    target[1] = kPatched[1];
    FlushInstructionCache(GetCurrentProcess(), target, sizeof(kPatched));
    DWORD ignored = 0;
    VirtualProtect(target, sizeof(kPatched), oldProtect, &ignored);
    Log("patched pvp gate at 0x%08X (75 04 -> EB 04)",
        static_cast<unsigned int>(base + kPatchRva));

    unsigned char* messageBlock = reinterpret_cast<unsigned char*>(base + kMessageRva);
    if (memcmp(messageBlock, kMessageExpected, sizeof(kMessageExpected)) != 0) {
        Log("message block mismatch at 0x%08X, left untouched",
            static_cast<unsigned int>(base + kMessageRva));
        return 0;
    }
    if (!VirtualProtect(messageBlock, sizeof(kMessageExpected), PAGE_EXECUTE_READWRITE, &oldProtect)) {
        Log("message block VirtualProtect failed error=%lu", GetLastError());
        return 0;
    }
    memset(messageBlock, 0x90, sizeof(kMessageExpected));
    FlushInstructionCache(GetCurrentProcess(), messageBlock, sizeof(kMessageExpected));
    VirtualProtect(messageBlock, sizeof(kMessageExpected), oldProtect, &ignored);
    Log("patched pvp message block at 0x%08X (nop 24 bytes)",
        static_cast<unsigned int>(base + kMessageRva));
    return 0;
}

} // namespace

BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID)
{
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(instance);
        HANDLE thread = CreateThread(nullptr, 0, &Worker, nullptr, 0, nullptr);
        if (thread) CloseHandle(thread);
    }
    return TRUE;
}
