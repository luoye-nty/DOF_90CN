// StoreStandInFix.dll -- standalone client-side probe/fix for the expert-job
// store stand-in that covers the store owner on OTHER players' screens.
//
// This module is deliberately separate from 90CN.dll: it exists to prove the
// fix on a live client before any of it is merged into client-patch/90CN.cpp.
//
// ---------------------------------------------------------------------------
// What the client does today (all addresses verified against the live client,
// module base 0x00400000):
//
//   op538 (CREATE_EXPERT_JOB_STORE) handler                     0x01D8AE70
//     reads kind(u8) owner(u16) name(dstr) town(u8) area(u32) x(u16) y(u16)
//           cost(u32) mode(u8) [kind==3: qualCount + quals]
//     entity = sub_0268FDF0(host, kind')      -> host+0x534   (0x3D4 bytes)
//     setters: 0x01A7F110 position, 0x01A7F340 name, 0x01A7F320 cost,
//              0x03538A60 mode -> entity+0x3C4
//     sub_02694BB0(host)                      -> finalize
//        +-- sub_0268FF40(host, kind'')       -> host+0x53C   <== THE STAND-IN
//              0x624-byte object, ctor sub_02F7E820(obj, host, kind),
//              class vtable 0x048559D4 / 0x04855984
//
//   kind'' comes from sub_02F7DF00(entity kind at +0x398): 1 -> 1, 3 -> 2, else 3.
//     entity kind 1 = enchant store, 3 = disjoint store, so the two expert-job
//     variants are kind 1 and kind 2.
//   kind 0 builds a different (0x608-byte) object via sub_02F7EBD0: that is the
//     town trade companion used by the private store (v116 permission), which
//     must keep working -- this fix never touches kind 0.
//
//   Live heap evidence: each client holds exactly ONE such stand-in, and its
//   +0x61C points at the OTHER player's store entity; a client's own store has
//   none. That is why only the other player sees the default-looking sprite.
//   Skipping the creation is the minimal, reversible experiment: the callers
//   already tolerate a null result (finalize checks `test edi,edi; je`), and the
//   store label/panel is drawn from the entity, not from this object.
//
// ---------------------------------------------------------------------------
// Modes (StoreStandInFix.ini next to the DLL, default skip12):
//   mode=skip12   return null for kind 1 and kind 2 (the fix under test)
//   mode=log      call the original, log every call (diagnostic)
//   mode=pass     call the original, no logging of normal calls
//   kinds=1,2     which kinds skip12 applies to
//
// Log: <game dir>\StoreStandInFix.log

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <tlhelp32.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#define ARRAY_COUNT(a) (sizeof(a) / sizeof((a)[0]))

namespace {

const unsigned int kImageBase = 0x00400000u;
// sub_0268FF40: get-or-create the store companion at host+0x53C.
const unsigned int kCompanionFactoryAddress = 0x0268FF40u;
// Verified prologue: push ebp; mov ebp, esp; push -1  (5 bytes, exactly the
// bytes this hook overwrites).
const unsigned char kCompanionFactoryPrologue[5] = {0x55, 0x8B, 0xEC, 0x6A, 0xFF};

enum Mode { MODE_SKIP12 = 0, MODE_LOG = 1, MODE_PASS = 2 };

volatile LONG g_mode = MODE_SKIP12;
volatile LONG g_kind1 = 1;
volatile LONG g_kind2 = 2;
volatile LONG g_installed = 0;
volatile LONG g_callCount = 0;
volatile LONG g_skipCount = 0;

typedef void* (__fastcall *CompanionFactoryFn)(void* host, void* unused, int kind);
CompanionFactoryFn g_callOriginal = nullptr;  // inside our trampoline

CRITICAL_SECTION g_logLock;
bool g_logReady = false;
wchar_t g_logPath[MAX_PATH * 2] = {0};
wchar_t g_configPath[MAX_PATH * 2] = {0};

void LogLine(const wchar_t* fmt, ...) {
    if (!g_logReady) {
        return;
    }
    wchar_t body[1024];
    va_list args;
    va_start(args, fmt);
    _vsnwprintf_s(body, ARRAY_COUNT(body), _TRUNCATE, fmt, args);
    va_end(args);

    SYSTEMTIME st;
    GetLocalTime(&st);
    wchar_t line[1200];
    _snwprintf_s(line, ARRAY_COUNT(line), _TRUNCATE,
                 L"%04u-%02u-%02u %02u:%02u:%02u.%03u  %s\r\n",
                 st.wYear, st.wMonth, st.wDay, st.wHour, st.wMinute, st.wSecond,
                 st.wMilliseconds, body);

    EnterCriticalSection(&g_logLock);
    HANDLE file = CreateFileW(g_logPath, FILE_APPEND_DATA, FILE_SHARE_READ,
                              nullptr, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file != INVALID_HANDLE_VALUE) {
        DWORD written = 0;
        int bytes = WideCharToMultiByte(CP_UTF8, 0, line, -1, nullptr, 0, nullptr, nullptr);
        if (bytes > 1) {
            char* utf8 = static_cast<char*>(HeapAlloc(GetProcessHeap(), 0, bytes));
            if (utf8) {
                WideCharToMultiByte(CP_UTF8, 0, line, -1, utf8, bytes, nullptr, nullptr);
                WriteFile(file, utf8, static_cast<DWORD>(bytes - 1), &written, nullptr);
                HeapFree(GetProcessHeap(), 0, utf8);
            }
        }
        CloseHandle(file);
    }
    LeaveCriticalSection(&g_logLock);
}

bool BuildSiblingPath(const wchar_t* name, wchar_t* out, size_t outCount) {
    HMODULE self = nullptr;
    if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                                GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                            reinterpret_cast<LPCWSTR>(&BuildSiblingPath), &self)) {
        return false;
    }
    DWORD length = GetModuleFileNameW(self, out, static_cast<DWORD>(outCount));
    if (length == 0 || length >= outCount) {
        return false;
    }
    wchar_t* separator = wcsrchr(out, L'\\');
    if (!separator) {
        return false;
    }
    separator[1] = L'\0';
    return wcscat_s(out, outCount, name) == 0;
}

void LoadConfig() {
    wchar_t value[64] = {0};
    if (GetPrivateProfileStringW(L"fix", L"mode", L"skip12", value, ARRAY_COUNT(value),
                                 g_configPath) != 0) {
        if (_wcsicmp(value, L"log") == 0) {
            g_mode = MODE_LOG;
        } else if (_wcsicmp(value, L"pass") == 0) {
            g_mode = MODE_PASS;
        } else {
            g_mode = MODE_SKIP12;
        }
    }
    wchar_t kinds[64] = {0};
    if (GetPrivateProfileStringW(L"fix", L"kinds", L"1,2", kinds, ARRAY_COUNT(kinds),
                                 g_configPath) != 0) {
        g_kind1 = (wcsstr(kinds, L"1") != nullptr) ? 1 : 0;
        g_kind2 = (wcsstr(kinds, L"2") != nullptr) ? 1 : 0;
    }
}

// Returns 1 when the caller should run the original implementation.
int __cdecl DecideAndLog(void* host, int kind, void** createdOut, int* skippedOut) {
    const LONG mode = g_mode;
    const bool expertStandIn =
        (kind == 1 && g_kind1) || (kind == 2 && g_kind2);
    if (mode == MODE_SKIP12 && expertStandIn) {
        *createdOut = nullptr;
        *skippedOut = 1;
        return 0;
    }
    void* created = g_callOriginal(host, nullptr, kind);
    *createdOut = created;
    *skippedOut = 0;
    return 1;
}

// Detour: thiscall-style (host in ecx, kind on the stack, callee pops it).
void* __fastcall CompanionFactoryDetour(void* host, void* /*unused*/, int kind) {
    InterlockedIncrement(&g_callCount);
    const LONG mode = g_mode;

    if (mode == MODE_SKIP12 && ((kind == 1 && g_kind1) || (kind == 2 && g_kind2))) {
        LONG skipped = InterlockedIncrement(&g_skipCount);
        LogLine(L"skip stand-in #%ld host=0x%08X kind=%d (returning null)",
                skipped, static_cast<unsigned int>(reinterpret_cast<uintptr_t>(host)),
                kind);
        return nullptr;
    }

    void* created = g_callOriginal(host, nullptr, kind);
    if (mode != MODE_PASS) {
        LogLine(L"pass host=0x%08X kind=%d -> object=0x%08X",
                static_cast<unsigned int>(reinterpret_cast<uintptr_t>(host)), kind,
                static_cast<unsigned int>(reinterpret_cast<uintptr_t>(created)));
    }
    return created;
}

bool WriteMemory(void* address, const void* bytes, size_t size, DWORD* oldProtect) {
    if (!VirtualProtect(address, size, PAGE_EXECUTE_READWRITE, oldProtect)) {
        return false;
    }
    memcpy(address, bytes, size);
    DWORD ignored = 0;
    VirtualProtect(address, size, *oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), address, size);
    return true;
}

bool InstallHook(unsigned char* target) {
    // trampoline: original 5 bytes, then jmp back to target+5
    unsigned char* trampoline = static_cast<unsigned char*>(VirtualAlloc(
        nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        LogLine(L"install failed: VirtualAlloc trampoline error=%lu", GetLastError());
        return false;
    }
    memcpy(trampoline, kCompanionFactoryPrologue, sizeof(kCompanionFactoryPrologue));
    trampoline[5] = 0xE9;
    *reinterpret_cast<int*>(trampoline + 6) =
        static_cast<int>(reinterpret_cast<intptr_t>(target + 5) -
                         reinterpret_cast<intptr_t>(trampoline + 10));
    g_callOriginal = reinterpret_cast<CompanionFactoryFn>(trampoline);

    unsigned char patch[5];
    patch[0] = 0xE9;
    *reinterpret_cast<int*>(patch + 1) =
        static_cast<int>(reinterpret_cast<intptr_t>(&CompanionFactoryDetour) -
                         reinterpret_cast<intptr_t>(target + 5));

    DWORD oldProtect = 0;
    // Write bytes 1..4 first, the opcode last, so the entry byte flips in one go.
    if (!VirtualProtect(target, 5, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine(L"install failed: VirtualProtect error=%lu", GetLastError());
        return false;
    }
    memcpy(target + 1, patch + 1, 4);
    FlushInstructionCache(GetCurrentProcess(), target, 5);
    memcpy(target, patch, 1);
    FlushInstructionCache(GetCurrentProcess(), target, 5);
    DWORD ignored = 0;
    VirtualProtect(target, 5, oldProtect, &ignored);

    LogLine(L"hook installed target=0x%08X trampoline=0x%08X detour=0x%08X",
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(target)),
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(trampoline)),
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(&CompanionFactoryDetour)));
    return true;
}

DWORD WINAPI WorkerThread(LPVOID) {
    InitializeCriticalSection(&g_logLock);
    BuildSiblingPath(L"StoreStandInFix.log", g_logPath, ARRAY_COUNT(g_logPath));
    BuildSiblingPath(L"StoreStandInFix.ini", g_configPath, ARRAY_COUNT(g_configPath));
    g_logReady = true;
    LoadConfig();

    HMODULE exe = GetModuleHandleW(nullptr);
    const unsigned int base = static_cast<unsigned int>(reinterpret_cast<uintptr_t>(exe));
    LogLine(L"StoreStandInFix loaded: process=%lu module=0x%08X mode=%ld kinds=%ld,%ld",
            GetCurrentProcessId(), base, g_mode, g_kind1, g_kind2);

    if (base != kImageBase) {
        LogLine(L"module base 0x%08X != expected 0x%08X; addresses are relocated by delta=0x%X",
                base, kImageBase, base - kImageBase);
    }
    const intptr_t delta = static_cast<intptr_t>(base) - static_cast<intptr_t>(kImageBase);
    unsigned char* target =
        reinterpret_cast<unsigned char*>(static_cast<intptr_t>(kCompanionFactoryAddress) + delta);

    // The packed client restores .text late: wait for the verified prologue.
    for (int attempt = 0; attempt < 600; ++attempt) {
        if (memcmp(target, kCompanionFactoryPrologue, sizeof(kCompanionFactoryPrologue)) == 0) {
            LogLine(L"prologue matched after %d ms: %02X %02X %02X %02X %02X | %02X %02X %02X %02X",
                    attempt * 200, target[0], target[1], target[2], target[3], target[4],
                    target[5], target[6], target[7], target[8]);
            if (InstallHook(target)) {
                InterlockedExchange(&g_installed, 1);
                // The shipped EXE restores .text late; if anything writes the
                // original prologue back, re-install. Bounded and logged.
                for (int guard = 0; guard < 90; ++guard) {
                    Sleep(2000);
                    if (target[0] == 0xE9) {
                        continue;
                    }
                    LogLine(L"guard: hook byte reverted (0x%02X) at 0x%08X, reinstalling",
                            target[0],
                            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(target)));
                    InstallHook(target);
                }
                LogLine(L"guard finished: calls=%ld skipped=%ld", g_callCount, g_skipCount);
            }
            return 0;
        }
        Sleep(200);
    }
    LogLine(L"install aborted: prologue never matched at 0x%08X (last bytes %02X %02X %02X %02X %02X)",
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(target)),
            target[0], target[1], target[2], target[3], target[4]);
    return 0;
}

}  // namespace

BOOL APIENTRY DllMain(HMODULE module, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(module);
        HANDLE thread = CreateThread(nullptr, 0, WorkerThread, nullptr, 0, nullptr);
        if (thread) {
            CloseHandle(thread);
        }
    }
    return TRUE;
}

