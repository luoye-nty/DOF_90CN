// ============================================================================
//  BannerProbe.dll -- standalone diagnostic for the expert-job
//  "副职业资格认证" banner that opens when a pet is unequipped inside a dungeon
//  and the player then returns to town.
//
//  WHY A SEPARATE DLL: the shipping 90CN.dll is Themida-protected by the
//  operator. This module therefore installs NOTHING on its own: every hook is a
//  CHAIN onto the E9 detour 90CN.dll already wrote at the same target. It reads
//  the existing rel32, points the E9 at its own observer, and the observer
//  tail-jumps to the address 90CN's E9 pointed at. If the expected E9 is not
//  present the observer fails closed and retries - it never overwrites a target
//  it does not understand.
//
//  WHAT IT ANSWERS:
//    * Which class-0/class-1 packet the client dispatched immediately before it
//      builds the expert-job UI ("recent" ring, 12 entries, with ticks).
//    * Which client code decided to build it (return-address backtrace).
//    * What the live actor's expert-job fields held at that moment.
//
//  Output: banner_probe.log next to DNF.exe. Delete the DLL (and the log) when
//  done; nothing else in the client was modified.
// ============================================================================
#include <windows.h>
#include <stdarg.h>
#include <stdint.h>
#include <stddef.h>
#include <tlhelp32.h>

// ---------------------------------------------------------------------------
// targets (RVAs in DNF.exe; the module base is resolved at runtime)
// ---------------------------------------------------------------------------
static const uintptr_t kClass0DispatchRva = 0x01E5CA00;  // sub_225CA00
static const uintptr_t kClass1DispatchRva = 0x01E5C8A0;  // sub_225C8A0
static const uintptr_t kExpertJobMenuOwnerRva = 0x02D00270; // sub_3100270
static const uintptr_t kExpertJobSystemMenuRva = 0x02CFF3F0; // sub_30FF3F0
static const uintptr_t kExpertJobProfileRva = 0x02C6E210;    // sub_306E210
static const uintptr_t kObjectManagerPointerRva = 0x04DB2728; // dword_51B2728
static const uintptr_t kCurrentActorRva = 0x0229A050;         // sub_269A050
static const DWORD     kActorExpertJobTypeOffset = 0x368;

static uintptr_t g_base = 0;
static char g_logPath[MAX_PATH] = { 0 };

// ---------------------------------------------------------------------------
// logging (no CRT: CreateFileW/WriteFile + wsprintfA)
// ---------------------------------------------------------------------------
static CRITICAL_SECTION g_logLock;
static HANDLE g_log = INVALID_HANDLE_VALUE;
static DWORD g_t0 = 0;

static DWORD NowMs(void)
{
    return GetTickCount() - g_t0;
}

static void Log(const char* format, ...)
{
    char body[1024];
    va_list args;
    va_start(args, format);
    wvsprintfA(body, format, args);
    va_end(args);

    char line[1200];
    wsprintfA(line, "[%8u] %s\r\n", (unsigned int)NowMs(), body);

    EnterCriticalSection(&g_logLock);
    if (g_log != INVALID_HANDLE_VALUE) {
        DWORD written = 0;
        DWORD length = 0;
        while (line[length] != 0) ++length;
        WriteFile(g_log, line, length, &written, NULL);
    }
    LeaveCriticalSection(&g_logLock);
}

static HMODULE g_selfModule = NULL;

static bool OpenLog(void)
{
    char exePath[MAX_PATH] = { 0 };
    if (GetModuleFileNameA(NULL, exePath, MAX_PATH) == 0) return false;
    char* slash = exePath;
    for (char* scan = exePath; *scan; ++scan) {
        if (*scan == '\\' || *scan == '/') slash = scan;
    }
    if (slash == exePath) return false;
    slash[1] = 0;

    // Name the log after this module so two probe builds never share a file.
    char selfPath[MAX_PATH] = { 0 };
    char stem[64] = "banner_probe";
    if (g_selfModule &&
        GetModuleFileNameA(g_selfModule, selfPath, MAX_PATH) != 0) {
        const char* selfSlash = selfPath;
        for (const char* scan = selfPath; *scan; ++scan) {
            if (*scan == '\\' || *scan == '/') selfSlash = scan;
        }
        ++selfSlash;
        int index = 0;
        while (*selfSlash && *selfSlash != '.' && index < 60) {
            stem[index++] = *selfSlash++;
        }
        stem[index] = 0;
    }
    wsprintfA(g_logPath, "%s%s.log", exePath, stem);
    g_log = CreateFileA(g_logPath, GENERIC_WRITE, FILE_SHARE_READ, NULL,
        CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
    return g_log != INVALID_HANDLE_VALUE;
}

// ---------------------------------------------------------------------------
// recent-dispatch ring
// ---------------------------------------------------------------------------
static const int kRingDepth = 12;
static volatile LONG g_ringWrite = 0;
static unsigned short g_ringClass[kRingDepth] = { 0 };
static unsigned short g_ringOpcode[kRingDepth] = { 0 };
static unsigned int g_ringTick[kRingDepth] = { 0 };

static void FormatActorExpertJob(char* out, size_t outSize);

static void LogActorSnapshot(const char* tag)
{
    char actor[768] = { 0 };
    FormatActorExpertJob(actor, sizeof(actor));
    Log("%s %s", tag, actor);
}

static void RecordDispatch(unsigned int classID, int packet)
{
    __try {
        if (!packet) return;
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        unsigned int opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        unsigned int length = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (opcode == 0 || opcode > 20000 || length > 0x10000) return;
        LONG slot = InterlockedIncrement(&g_ringWrite) - 1;
        int index = static_cast<int>(slot) & (kRingDepth - 1);
        g_ringClass[index] = static_cast<unsigned short>(classID);
        g_ringOpcode[index] = static_cast<unsigned short>(opcode);
        g_ringTick[index] = NowMs();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("ring read exception class=%u packet=%08X", classID,
            (unsigned int)packet);
        return;
    }
    // Actor state at the scene-rebuild boundaries, so the expert-job field can
    // be read before and after 90CN's projection instead of guessed.
    if (classID == 0) {
        __try {
            unsigned int opcode = *reinterpret_cast<const unsigned short*>(
                reinterpret_cast<const unsigned char*>(packet) + 1);
            unsigned int mode = *reinterpret_cast<const unsigned char*>(packet + 16);
            if (opcode == 2 && (mode == 0 || mode == 1)) {
                char tag[48];
                wsprintfA(tag, "actor-at-op2-mode%u", mode);
                LogActorSnapshot(tag);
            } else if (opcode == 24 || opcode == 1369 || opcode == 14) {
                char tag[48];
                wsprintfA(tag, "actor-at-op%u", opcode);
                LogActorSnapshot(tag);
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }
}

static void FormatRing(char* out, size_t outSize)
{
    out[0] = 0;
    LONG write = InterlockedCompareExchange(&g_ringWrite, 0, 0);
    size_t used = 0;
    for (int step = kRingDepth - 1; step >= 0; --step) {
        LONG sequence = write - 1 - step;
        if (sequence < 0) continue;
        int index = static_cast<int>(sequence) & (kRingDepth - 1);
        char entry[48];
        wsprintfA(entry, "c%u/%u@%u ",
            (unsigned int)g_ringClass[index],
            (unsigned int)g_ringOpcode[index],
            (unsigned int)g_ringTick[index]);
        size_t length = 0;
        while (entry[length] != 0) ++length;
        if (used + length + 1 >= outSize) break;
        for (size_t i = 0; i <= length; ++i) out[used + i] = entry[i];
        used += length;
    }
}

// ---------------------------------------------------------------------------
// backtrace
// ---------------------------------------------------------------------------
typedef USHORT(WINAPI* CaptureStackBackTraceFn)(ULONG, ULONG, PVOID*, PULONG);

static CaptureStackBackTraceFn ResolveCaptureStack(void)
{
    static CaptureStackBackTraceFn resolved = nullptr;
    static bool tried = false;
    if (!tried) {
        tried = true;
        HMODULE ntdllModule = GetModuleHandleA("ntdll.dll");
        if (ntdllModule) {
            resolved = reinterpret_cast<CaptureStackBackTraceFn>(
                GetProcAddress(ntdllModule, "RtlCaptureStackBackTrace"));
        }
        if (!resolved) {
            HMODULE kernel32Module = GetModuleHandleA("kernel32.dll");
            if (kernel32Module) {
                resolved = reinterpret_cast<CaptureStackBackTraceFn>(
                    GetProcAddress(kernel32Module, "CaptureStackBackTrace"));
            }
        }
        if (!resolved) {
            HMODULE kernelbaseModule = GetModuleHandleA("kernelbase.dll");
            if (kernelbaseModule) {
                resolved = reinterpret_cast<CaptureStackBackTraceFn>(
                    GetProcAddress(kernelbaseModule, "CaptureStackBackTrace"));
            }
        }
    }
    return resolved;
}

static void FormatBackTrace(char* out, size_t outSize)
{
    out[0] = 0;
    CaptureStackBackTraceFn captureStack = ResolveCaptureStack();
    if (!captureStack) {
        wsprintfA(out, "unavailable(ntdll=0x%08X)",
            (unsigned int)reinterpret_cast<uintptr_t>(
                GetModuleHandleA("ntdll.dll")));
        return;
    }
    void* frames[12] = { 0 };
    USHORT count = captureStack(1, 12, frames, nullptr);
    size_t used = 0;
    for (USHORT i = 0; i < count; ++i) {
        uintptr_t address = reinterpret_cast<uintptr_t>(frames[i]);
        char entry[24];
        if (g_base && address > g_base && address < g_base + 0x08000000) {
            wsprintfA(entry, "%08X ", (unsigned int)(address - g_base));
        } else {
            wsprintfA(entry, "[%08X] ", (unsigned int)(address & 0xFFFFFFFFu));
        }
        size_t length = 0;
        while (entry[length] != 0) ++length;
        if (used + length + 1 >= outSize) break;
        for (size_t j = 0; j <= length; ++j) out[used + j] = entry[j];
        used += length;
    }
}

// ---------------------------------------------------------------------------
// actor expert-job snapshot (read-only)
// ---------------------------------------------------------------------------
static void FormatActorExpertJob(char* out, size_t outSize)
{
    out[0] = 0;
    __try {
        void* objectManager = *reinterpret_cast<void**>(
            g_base + kObjectManagerPointerRva);
        typedef void* (__stdcall* CurrentActorFn)(void*);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_base + kCurrentActorRva);
        void* actor = objectManager && currentActorFn
            ? currentActorFn(objectManager)
            : nullptr;
        if (!actor) {
            wsprintfA(out, "no-actor");
            return;
        }
        // Raw window around the expert-job state so the field layout can be
        // identified from the log instead of guessed.
        unsigned int raw[20] = { 0 };
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(actor);
        for (int i = 0; i < 20; ++i) {
            raw[i] = *reinterpret_cast<const unsigned int*>(
                bytes + kActorExpertJobTypeOffset - 0x28 + i * 4);
        }
        wsprintfA(out,
            "actor=%08X at=%u raw[340]=%08X %08X %08X %08X %08X %08X %08X %08X "
            "%08X %08X %08X %08X %08X %08X %08X %08X %08X %08X %08X %08X",
            (unsigned int)(reinterpret_cast<uintptr_t>(actor) & 0xFFFFFFFFu),
            (unsigned int)kActorExpertJobTypeOffset,
            raw[0], raw[1], raw[2], raw[3], raw[4], raw[5], raw[6], raw[7],
            raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14],
            raw[15], raw[16], raw[17], raw[18], raw[19]);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        wsprintfA(out, "actor-read-exception");
    }
}

// ---------------------------------------------------------------------------
// observers: every one is a naked tail-jump to 90CN's original target
// ---------------------------------------------------------------------------
extern "C" void* g_originalClass0 = nullptr;
extern "C" void* g_originalClass1 = nullptr;
extern "C" void* g_originalMenuOwner = nullptr;
extern "C" void* g_originalSystemMenu = nullptr;
extern "C" void* g_originalProfile = nullptr;

extern "C" void __cdecl ProbeClass0Packet(int packet)
{
    RecordDispatch(0, packet);
}

extern "C" void __cdecl ProbeClass1Packet(int packet)
{
    RecordDispatch(1, packet);
}

static void LogBuild(const char* tag)
{
    char ring[512] = { 0 };
    char backtrace[384] = { 0 };
    char actor[160] = { 0 };
    FormatRing(ring, sizeof(ring));
    FormatBackTrace(backtrace, sizeof(backtrace));
    FormatActorExpertJob(actor, sizeof(actor));
    Log("%s tick=%u %s recent=%s backtrace=%s", tag, (unsigned int)NowMs(),
        actor, ring, backtrace);
}

extern "C" void __cdecl ProbeMenuOwnerBuild(void)
{
    LogBuild("expert-job-menu-owner-build");
}

extern "C" void __cdecl ProbeSystemMenuBuild(void)
{
    LogBuild("expert-job-system-menu-build");
}

extern "C" void __cdecl ProbeProfileBuild(void)
{
    LogBuild("expert-job-profile-build");
}

// class-0 dispatch: (a1, packet). After pushad+pushfd the arguments sit at
// [esp+40] (a1) and [esp+44] (packet).
__declspec(naked) void DetourClass0(void)
{
    __asm {
        pushad
        pushfd
        push dword ptr[esp + 44]
        call ProbeClass0Packet
        add esp, 4
        popfd
        popad
        jmp dword ptr[g_originalClass0]
    }
}

// class-1 dispatch: (packet, a2, a3) - the packet is the first argument.
__declspec(naked) void DetourClass1(void)
{
    __asm {
        pushad
        pushfd
        push dword ptr[esp + 40]
        call ProbeClass1Packet
        add esp, 4
        popfd
        popad
        jmp dword ptr[g_originalClass1]
    }
}

__declspec(naked) void DetourMenuOwner(void)
{
    __asm {
        pushad
        pushfd
        call ProbeMenuOwnerBuild
        popfd
        popad
        jmp dword ptr[g_originalMenuOwner]
    }
}

__declspec(naked) void DetourSystemMenu(void)
{
    __asm {
        pushad
        pushfd
        call ProbeSystemMenuBuild
        popfd
        popad
        jmp dword ptr[g_originalSystemMenu]
    }
}

__declspec(naked) void DetourProfile(void)
{
    __asm {
        pushad
        pushfd
        call ProbeProfileBuild
        popfd
        popad
        jmp dword ptr[g_originalProfile]
    }
}

// ---------------------------------------------------------------------------
// chain installation
// ---------------------------------------------------------------------------
static bool IsReadableCode(uintptr_t address, size_t length)
{
    if (address == 0) return false;
    MEMORY_BASIC_INFORMATION info;
    if (VirtualQuery(reinterpret_cast<void*>(address), &info, sizeof(info)) == 0)
        return false;
    if (info.State != MEM_COMMIT) return false;
    if (info.Protect != PAGE_EXECUTE && info.Protect != PAGE_EXECUTE_READ &&
        info.Protect != PAGE_EXECUTE_READWRITE &&
        info.Protect != PAGE_EXECUTE_WRITECOPY)
        return false;
    return reinterpret_cast<uintptr_t>(info.BaseAddress) + info.RegionSize >=
        address + length;
}

// Suspends every other thread in this process while a 5-byte branch is written,
// so no thread can execute a half-written instruction. The scope deliberately
// performs no logging or allocation: it only wraps the VirtualProtect write.
class ThreadFreeze
{
public:
    ThreadFreeze() : count_(0)
    {
        HANDLE snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0);
        if (snapshot == INVALID_HANDLE_VALUE) return;
        THREADENTRY32 entry;
        entry.dwSize = sizeof(entry);
        const DWORD selfProcess = GetCurrentProcessId();
        const DWORD selfThread = GetCurrentThreadId();
        if (Thread32First(snapshot, &entry)) {
            do {
                if (entry.th32OwnerProcessID == selfProcess &&
                    entry.th32ThreadID != selfThread) {
                    HANDLE thread = OpenThread(
                        THREAD_SUSPEND_RESUME | THREAD_QUERY_INFORMATION,
                        FALSE, entry.th32ThreadID);
                    if (!thread) continue;
                    if (count_ < kMaxFrozen &&
                        SuspendThread(thread) != static_cast<DWORD>(-1)) {
                        handles_[count_++] = thread;
                    } else {
                        CloseHandle(thread);
                    }
                }
                entry.dwSize = sizeof(entry);
            } while (Thread32Next(snapshot, &entry));
        }
        CloseHandle(snapshot);
    }

    ~ThreadFreeze()
    {
        for (int i = 0; i < count_; ++i) {
            ResumeThread(handles_[i]);
            CloseHandle(handles_[i]);
        }
    }

private:
    static const int kMaxFrozen = 256;
    HANDLE handles_[kMaxFrozen];
    int count_;
};

// Reads the rel32 target of an existing E9 detour and points that E9 at our own
// observer. Fails closed when the E9 is not there (90CN has not installed yet).
static bool ChainOntoExistingHook(uintptr_t target, void* observer,
    void** outOriginal)
{
    if (!IsReadableCode(target, 5)) return false;
    const unsigned char* bytes = reinterpret_cast<const unsigned char*>(target);
    if (bytes[0] != 0xE9) return false;
    int32_t relative = *reinterpret_cast<const int32_t*>(bytes + 1);
    uintptr_t original = target + 5 + static_cast<intptr_t>(relative);
    if (original == target || !IsReadableCode(original, 1)) return false;

    int32_t mine = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(observer) - static_cast<intptr_t>(target + 5));
    {
        ThreadFreeze freeze;
        DWORD previousProtect = 0;
        if (!VirtualProtect(reinterpret_cast<void*>(target), 5,
            PAGE_EXECUTE_READWRITE, &previousProtect)) {
            return false;
        }
        *reinterpret_cast<unsigned char*>(target) = 0xE9;
        *reinterpret_cast<int32_t*>(target + 1) = mine;
        DWORD ignored = 0;
        VirtualProtect(reinterpret_cast<void*>(target), 5, previousProtect,
            &ignored);
    }
    FlushInstructionCache(GetCurrentProcess(),
        reinterpret_cast<void*>(target), 5);
    *outOriginal = reinterpret_cast<void*>(original);
    return true;
}

typedef bool (*ChainFn)(uintptr_t, void*, void**);

static bool InstallOne(const char* name, uintptr_t rva, void* observer,
    void** outOriginal)
{
    uintptr_t target = g_base + rva;
    if (ChainOntoExistingHook(target, observer, outOriginal)) {
        Log("chained %s target=%08X original=%08X", name,
            (unsigned int)target, (unsigned int)*outOriginal);
        return true;
    }
    Log("chain FAILED %s target=%08X first=%02X (no 90CN E9 yet)", name,
        (unsigned int)target,
        IsReadableCode(target, 1)
            ? *reinterpret_cast<unsigned char*>(target)
            : 0xFF);
    return false;
}

static DWORD WINAPI InstallWorker(LPVOID /*parameter*/)
{
    g_base = reinterpret_cast<uintptr_t>(GetModuleHandleA(NULL));
    Log("banner probe starting: module base=%08X self=%08X",
        (unsigned int)g_base, (unsigned int)reinterpret_cast<uintptr_t>(
            GetModuleHandleA("BannerProbe.dll")));

    bool class0 = false;
    bool class1 = false;
    bool menuOwner = false;
    bool systemMenu = false;
    bool profile = false;

    for (int attempt = 0; attempt < 120; ++attempt) {
        if (!class0) {
            class0 = InstallOne("class0-dispatch", kClass0DispatchRva,
                reinterpret_cast<void*>(&DetourClass0), &g_originalClass0);
        }
        if (!class1) {
            class1 = InstallOne("class1-dispatch", kClass1DispatchRva,
                reinterpret_cast<void*>(&DetourClass1), &g_originalClass1);
        }
        if (!menuOwner) {
            menuOwner = InstallOne("expert-job-menu-owner",
                kExpertJobMenuOwnerRva,
                reinterpret_cast<void*>(&DetourMenuOwner), &g_originalMenuOwner);
        }
        if (!systemMenu) {
            systemMenu = InstallOne("expert-job-system-menu",
                kExpertJobSystemMenuRva,
                reinterpret_cast<void*>(&DetourSystemMenu), &g_originalSystemMenu);
        }
        if (!profile) {
            profile = InstallOne("expert-job-profile", kExpertJobProfileRva,
                reinterpret_cast<void*>(&DetourProfile), &g_originalProfile);
        }
        if (class0 && class1 && menuOwner && systemMenu && profile) {
            Log("all observers installed");
            return 0;
        }
        Sleep(500);
    }
    Log("gave up: class0=%d class1=%d menuOwner=%d systemMenu=%d profile=%d",
        class0 ? 1 : 0, class1 ? 1 : 0, menuOwner ? 1 : 0, systemMenu ? 1 : 0,
        profile ? 1 : 0);
    return 0;
}

BOOL WINAPI DllMain(HINSTANCE self, DWORD reason, LPVOID /*reserved*/)
{
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(self);
        g_selfModule = self;
        InitializeCriticalSection(&g_logLock);
        g_t0 = GetTickCount();
        if (!OpenLog()) return TRUE;
        HANDLE thread = CreateThread(NULL, 0, InstallWorker, NULL, 0, NULL);
        if (thread) CloseHandle(thread);
    }
    return TRUE;
}
