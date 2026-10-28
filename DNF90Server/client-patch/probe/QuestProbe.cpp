// ===========================================================================
// QuestProbe.dll - standalone diagnostic module for the 90CN DNF client.
//
// WHY A SEPARATE DLL
//   The launcher verifies and rebuilds 90CN.dll on every login, and 90CN
//   already owns several inline hooks - notably the outgoing cipher-encode
//   function, which is exactly the address a packet tracer would want. A
//   standalone module is injected once, survives the 90CN rebuild cycle, and
//   never contends for a patched prologue.
//
// WHAT IT IS FOR
//   After op239 (give up expert job) the qualification quest cannot be
//   accepted again until the character is re-selected. The server side is
//   already proven correct by packet evidence: op21 lists 2702/2708/2710/2712
//   again right after the give-up, and the op356 completed-quest byte for the
//   given-up quest is 0 at the same moment. Yet the client refuses and never
//   sends op31, so the gate lives inside the client's own quest model, which
//   packet logs cannot show.
//
//   PHASE A (no patching at all): resolve the opcode-registry callback for
//     every quest opcode in both dispatch classes and dump the first bytes at
//     each callback, so the real handlers can be disassembled offline.
//
//   PHASE B (entry observation only): for callbacks whose prologue is a whole
//     instruction boundary we recognise, install a trampoline. A single
//     convention-agnostic proxy covers every slot; each hook site gets a small
//     generated stub that pushes its slot number and jumps to that proxy. The
//     proxy snapshots the register file, logs the slot plus the first three
//     stack words and any packet-looking body, restores everything, drops the
//     slot word and jumps into the trampoline. The original function therefore
//     still sees the caller's stack untouched and its own `ret` returns
//     straight to the real caller. No packet is modified, no state is written.
//
// Output: <game dir>\QuestProbe.log
// ===========================================================================

#define WIN32_LEAN_AND_MEAN
#define _CRT_SECURE_NO_WARNINGS
#include <windows.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>

// ---------------------------------------------------------------------------
// RVA constants. Same EXE as 90CN.dll targets (DNF.exe, ImageBase 0x400000).
// These are file RVAs, so VA = base + RVA.
// ---------------------------------------------------------------------------
static const unsigned int kClass0RegistryRva = 0x01E62A80; // sub_2262A80
static const unsigned int kClass1RegistryRva = 0x01E628F0; // sub_22628F0
static const unsigned int kDispatchLookupRva = 0x01E62430; // sub_2262430

typedef void* (__cdecl* DispatchRegistryFn)();
typedef void* (__thiscall* DispatchLookupFn)(void*, unsigned int);

static unsigned char* g_base = nullptr;
static CRITICAL_SECTION g_logLock;
static bool g_logReady = false;

// Off by default. The registry sweep is pure observation and needs no code
// patching at all, and it is the part that actually carries the answer (the
// handler map). Entry hooks are opt-in via QUESTPROBE_HOOKS=1 because they
// rewrite live prologues, which is the only part of this module that can take
// the client down.
static bool g_hooksEnabled = false;

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------
static void LogLine(const char* format, ...)
{
    if (!g_logReady) return;

    char message[1024] = { 0 };
    va_list args;
    va_start(args, format);
    _vsnprintf(message, sizeof(message) - 1, format, args);
    va_end(args);

    SYSTEMTIME now;
    GetLocalTime(&now);

    EnterCriticalSection(&g_logLock);

    wchar_t exePath[MAX_PATH] = { 0 };
    GetModuleFileNameW(nullptr, exePath, MAX_PATH);
    wchar_t* slash = wcsrchr(exePath, L'\\');
    if (slash) slash[1] = L'\0';

    wchar_t logPath[MAX_PATH] = { 0 };
    _snwprintf(logPath, MAX_PATH - 1, L"%sQuestProbe.log", exePath);

    HANDLE file = CreateFileW(logPath, FILE_APPEND_DATA, FILE_SHARE_READ,
        nullptr, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file != INVALID_HANDLE_VALUE) {
        char line[1200] = { 0 };
        int length = _snprintf(line, sizeof(line) - 1,
            "[%04d-%02d-%02d %02d:%02d:%02d.%03d] [QuestProbe] [pid=%u tid=%u] %s\r\n",
            now.wYear, now.wMonth, now.wDay, now.wHour, now.wMinute, now.wSecond,
            now.wMilliseconds, GetCurrentProcessId(), GetCurrentThreadId(), message);
        if (length > 0) {
            DWORD written = 0;
            WriteFile(file, line, static_cast<DWORD>(length), &written, nullptr);
        }
        CloseHandle(file);
    }

    LeaveCriticalSection(&g_logLock);
}

static void FormatBytes(const unsigned char* data, int length, char* out, size_t outCount)
{
    if (!out || outCount == 0) return;
    out[0] = '\0';
    if (!data || length <= 0) return;
    size_t used = 0;
    for (int i = 0; i < length; ++i) {
        if (used + 4 >= outCount) break;
        int written = _snprintf(out + used, outCount - used - 1, "%02X ", data[i]);
        if (written <= 0) break;
        used += static_cast<size_t>(written);
    }
}

// ---------------------------------------------------------------------------
// Slots. One generated stub per hook site, one shared proxy.
//
// Stack at proxy entry, because the stub pushed its slot before jumping:
//   [esp+0]=slot  [esp+4]=return address  [esp+8]=word0  [esp+12]=w1  [esp+16]=w2
// ---------------------------------------------------------------------------
constexpr int kProbeSlotCount = 24;

static void* g_trampolines[kProbeSlotCount] = {};
static void* g_stubs[kProbeSlotCount] = {};
static unsigned int g_slotOpcode[kProbeSlotCount] = {};
static unsigned int g_slotClass[kProbeSlotCount] = {};
static volatile LONG g_slotHits[kProbeSlotCount] = {};
static volatile LONG g_slotLogged[kProbeSlotCount] = {};

extern "C" void __cdecl LogHandlerEntry(unsigned int slot,
    unsigned int word0, unsigned int word1, unsigned int word2)
{
    if (slot >= static_cast<unsigned int>(kProbeSlotCount)) return;
    InterlockedIncrement(&g_slotHits[slot]);
    if (InterlockedIncrement(&g_slotLogged[slot]) > 64) return;

    // One of the stack words is normally the packet object, whose layout the
    // 90CN dispatch trace established: [class u8][opcode u16][len u32] with the
    // body at +16. Probe all three and report whichever looks like it, so the
    // argument layout of this handler is learned rather than assumed.
    char detail[768] = { 0 };
    int used = 0;
    const unsigned int words[3] = { word0, word1, word2 };
    for (int i = 0; i < 3; ++i) {
        unsigned int word = words[i];
        if (word < 0x10000) {
            used += _snprintf(detail + used, sizeof(detail) - used - 1, " w%d=0x%08X", i, word);
            continue;
        }
        bool described = false;
        __try {
            const unsigned char* p = reinterpret_cast<const unsigned char*>(word);
            unsigned int length = *reinterpret_cast<const unsigned int*>(p + 3);
            if (length >= 16 && length <= 40000) {
                unsigned int packetOpcode = *reinterpret_cast<const unsigned short*>(p + 1);
                char body[400] = { 0 };
                unsigned int bodyLength = length - 16;
                if (bodyLength > 96) bodyLength = 96;
                FormatBytes(p + 16, static_cast<int>(bodyLength), body, sizeof(body));
                used += _snprintf(detail + used, sizeof(detail) - used - 1,
                    " w%d=PACKET cls=%u op=%u len=%u body=[%s]",
                    i, *p, packetOpcode & 0xFFFFu, length, body);
                described = true;
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            used += _snprintf(detail + used, sizeof(detail) - used - 1,
                " w%d=0x%08X(read fault)", i, word);
            described = true;
        }
        if (!described) {
            used += _snprintf(detail + used, sizeof(detail) - used - 1, " w%d=0x%08X", i, word);
        }
        if (used < 0) used = 0;
        if (static_cast<size_t>(used) >= sizeof(detail) - 8) break;
    }

    LogLine("handler slot=%d class=%u opcode=%u hits=%d%s",
        static_cast<int>(slot), g_slotClass[slot], g_slotOpcode[slot],
        static_cast<int>(g_slotHits[slot]), detail);
}

__declspec(naked) void ProxyQuestHandler()
{
    __asm {
        pushad
        pushfd
        // pushfd leaves: slot [esp+36], ret [esp+40], w0 [esp+44], w1 [esp+48], w2 [esp+52]
        mov eax, dword ptr [esp + 52]
        push eax
        mov eax, dword ptr [esp + 52]
        push eax
        mov eax, dword ptr [esp + 52]
        push eax
        mov eax, dword ptr [esp + 48]
        push eax
        call LogHandlerEntry
        add esp, 16
        popfd
        popad
        // Drop the stub's slot word so the original function's ret finds the
        // real caller's return address, then enter its relocated prologue.
        mov ecx, dword ptr [esp]
        add esp, 4
        lea eax, g_trampolines
        jmp dword ptr [eax + ecx * 4]
    }
}

// ---------------------------------------------------------------------------
// Generated stub: push <slot>; jmp <proxy>
// ---------------------------------------------------------------------------
static void* BuildStub(int slot)
{
    unsigned char* code = reinterpret_cast<unsigned char*>(
        VirtualAlloc(nullptr, 32, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!code) return nullptr;

    int offset = 0;
    if (slot < 128) {
        code[offset++] = 0x6A;                       // push imm8
        code[offset++] = static_cast<unsigned char>(slot);
    } else {
        code[offset++] = 0x68;                       // push imm32
        *reinterpret_cast<int*>(code + offset) = slot;
        offset += 4;
    }
    code[offset++] = 0xE9;                           // jmp rel32
    intptr_t relative = reinterpret_cast<intptr_t>(&ProxyQuestHandler) -
        (reinterpret_cast<intptr_t>(code + offset) + 5);
    *reinterpret_cast<intptr_t*>(code + offset) = relative;
    FlushInstructionCache(GetCurrentProcess(), code, 32);
    return code;
}

// ---------------------------------------------------------------------------
// Safe prologue relocation. Only whole-instruction prologues are accepted;
// anything else is reported and left alone.
// ---------------------------------------------------------------------------
static int MatchPrologue(const unsigned char* code, unsigned char* stolen)
{
    // push ebp; mov ebp,esp; push -1
    if (code[0] == 0x55 && code[1] == 0x8B && code[2] == 0xEC && code[3] == 0x6A && code[4] == 0xFF) {
        memcpy(stolen, code, 5);
        return 5;
    }
    // mov edi,edi; push ebp; mov ebp,esp
    if (code[0] == 0x8B && code[1] == 0xFF && code[2] == 0x55 && code[3] == 0x8B && code[4] == 0xEC) {
        memcpy(stolen, code, 5);
        return 5;
    }
    // push ebp; mov ebp,esp; sub esp,imm8
    if (code[0] == 0x55 && code[1] == 0x8B && code[2] == 0xEC && code[3] == 0x83 && code[4] == 0xEC) {
        memcpy(stolen, code, 6);
        return 6;
    }
    // push ebp; mov ebp,esp; push ebx; push esi; push edi  (or any 3 pushes)
    if (code[0] == 0x55 && code[1] == 0x8B && code[2] == 0xEC &&
        (code[3] == 0x53 || code[3] == 0x56 || code[3] == 0x57) &&
        (code[4] == 0x53 || code[4] == 0x56 || code[4] == 0x57) &&
        (code[5] == 0x53 || code[5] == 0x56 || code[5] == 0x57)) {
        memcpy(stolen, code, 6);
        return 6;
    }
    // push ebx; push esi; push edi; push ebp; mov ebp,esp
    if (code[0] == 0x53 && code[1] == 0x56 && code[2] == 0x57 &&
        code[3] == 0x55 && code[4] == 0x8B && code[5] == 0xEC) {
        memcpy(stolen, code, 6);
        return 6;
    }
    return 0;
}

static int g_nextSlot = 0;

static bool InstallObservationHook(unsigned int classID, unsigned int opcode, void* target)
{
    if (g_nextSlot >= kProbeSlotCount) return false;
    int slot = g_nextSlot;
    if (!target) return false;

    unsigned char* code = reinterpret_cast<unsigned char*>(target);
    unsigned char prologue[8] = { 0 };
    __try {
        memcpy(prologue, code, sizeof(prologue));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("slot=%d opcode=%u target=%p not readable", slot, opcode, target);
        return false;
    }

    if (prologue[0] == 0xE9 || prologue[0] == 0xEB) {
        LogLine("opcode=%u target=%p already hooked (0x%02X) - skipped",
            opcode, target, prologue[0]);
        return false;
    }

    unsigned char stolen[8] = { 0 };
    int stolenLength = MatchPrologue(prologue, stolen);
    if (!stolenLength) {
        char raw[64] = { 0 };
        FormatBytes(prologue, 8, raw, sizeof(raw));
        LogLine("opcode=%u target=%p unknown prologue [%s] - left alone", opcode, target, raw);
        return false;
    }

    void* stub = BuildStub(slot);
    unsigned char* trampoline = reinterpret_cast<unsigned char*>(
        VirtualAlloc(nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!stub || !trampoline) {
        LogLine("slot=%d opcode=%u allocation failed", slot, opcode);
        return false;
    }

    // Relocated prologue + jump back to the instruction after it.
    memcpy(trampoline, stolen, static_cast<size_t>(stolenLength));
    trampoline[stolenLength] = 0xE9;
    intptr_t back = reinterpret_cast<intptr_t>(code + stolenLength) -
        (reinterpret_cast<intptr_t>(trampoline + stolenLength) + 5);
    memcpy(trampoline + stolenLength + 1, &back, sizeof(back));

    DWORD oldProtect = 0;
    if (!VirtualProtect(code, static_cast<SIZE_T>(stolenLength),
            PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("slot=%d opcode=%u VirtualProtect failed err=%lu", slot, opcode, GetLastError());
        return false;
    }

    unsigned char patch[8] = { 0 };
    patch[0] = 0xE9;
    intptr_t relative = reinterpret_cast<intptr_t>(stub) -
        (reinterpret_cast<intptr_t>(code) + 5);
    memcpy(patch + 1, &relative, sizeof(relative));
    for (int i = 5; i < stolenLength; ++i) patch[i] = 0x90;

    memcpy(code, patch, static_cast<size_t>(stolenLength));
    VirtualProtect(code, static_cast<SIZE_T>(stolenLength), oldProtect, &oldProtect);
    FlushInstructionCache(GetCurrentProcess(), code, static_cast<SIZE_T>(stolenLength));

    g_trampolines[slot] = trampoline;
    g_stubs[slot] = stub;
    g_slotOpcode[slot] = opcode;
    g_slotClass[slot] = classID;
    ++g_nextSlot;

    LogLine("slot=%d OBSERVING class=%u opcode=%u target=%p stolen=%d trampoline=%p stub=%p",
        slot, classID, opcode, target, stolenLength, trampoline, stub);
    return true;
}

// ---------------------------------------------------------------------------
// Phase A + B
// ---------------------------------------------------------------------------
static const unsigned int kQuestOpcodes[] = {
    21,   // acceptable / all-task definition list
    31,   // accept quest (request and success ACK)
    32,   // give up quest
    34,   // finish quest
    356,  // clear-quest (completed visibility) table
    574,  // active quest snapshot
    205,  // expert-job info notification
    239,  // give up expert job
    33,   // set quest trigger
};

static int g_resolvePass = 0;

static int ResolveAndProbe()
{
    int pass = g_resolvePass++;
    int resolvedHandlers = 0;
    DispatchRegistryFn registryFn0 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass0RegistryRva);
    DispatchRegistryFn registryFn1 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass1RegistryRva);
    DispatchLookupFn lookupFn = reinterpret_cast<DispatchLookupFn>(
        g_base + kDispatchLookupRva);

    for (unsigned int classID = 0; classID <= 1; ++classID) {
        for (unsigned int i = 0;
             i < sizeof(kQuestOpcodes) / sizeof(kQuestOpcodes[0]); ++i) {
            unsigned int opcode = kQuestOpcodes[i];
            void* registry = nullptr;
            void* record = nullptr;
            unsigned int callbackOffset = 0;
            unsigned int contextOffset = 0;
            __try {
                registry = (classID == 0 ? registryFn0 : registryFn1)();
                record = registry ? lookupFn(registry, opcode) : nullptr;
                if (record) {
                    uintptr_t callback = *reinterpret_cast<uintptr_t*>(record);
                    uintptr_t context = *(reinterpret_cast<uintptr_t*>(record) + 1);
                    if (callback > reinterpret_cast<uintptr_t>(g_base) &&
                        callback < reinterpret_cast<uintptr_t>(g_base) + 0x6000000) {
                        callbackOffset = static_cast<unsigned int>(
                            callback - reinterpret_cast<uintptr_t>(g_base));
                    }
                    if (context > reinterpret_cast<uintptr_t>(g_base) &&
                        context < reinterpret_cast<uintptr_t>(g_base) + 0x6000000) {
                        contextOffset = static_cast<unsigned int>(
                            context - reinterpret_cast<uintptr_t>(g_base));
                    }
                }
            }
            __except (EXCEPTION_EXECUTE_HANDLER) {
                LogLine("registry read fault class=%u opcode=%u code=0x%08X",
                    classID, opcode, GetExceptionCode());
                continue;
            }

            if (!record) {
                if (pass == 0) {
                    LogLine("registry class=%u opcode=%u registry=%p record=0 (no handler)",
                        classID, opcode, registry);
                }
                continue;
            }
            ++resolvedHandlers;

            char head[64] = { 0 };
            if (callbackOffset) {
                __try {
                    FormatBytes(g_base + callbackOffset, 16, head, sizeof(head));
                }
                __except (EXCEPTION_EXECUTE_HANDLER) {
                    strcpy(head, "<unreadable>");
                }
            }

            bool hooked = false;
            if (g_hooksEnabled && callbackOffset) {
                hooked = InstallObservationHook(classID, opcode,
                    reinterpret_cast<void*>(g_base + callbackOffset));
            }
            if (pass == 0 || hooked) {
                LogLine("registry class=%u opcode=%u registry=%p record=%p callback_rva=0x%08X "
                        "context_rva=0x%08X hooked=%d head=[%s]",
                    classID, opcode, registry, record, callbackOffset, contextOffset,
                    hooked ? 1 : 0, head);
            }
        }
    }

    LogLine("resolve pass %d finished: handlers=%d hooks=%d of %d%s",
        pass, resolvedHandlers, g_nextSlot, kProbeSlotCount,
        g_hooksEnabled ? "" : " (hooks disabled - read-only sweep)");
    return resolvedHandlers;
}

// ===========================================================================
// FIX CANDIDATE - expert-job give-up must refresh the client quest model.
//
// This whole block is self-contained on purpose: it is the unit that moves into
// 90CN.dll once it is proven here.
//
// THE DEFECT (established 2026-09-15 by disassembling the handlers this probe
// resolved, and by packet evidence on the server side):
//   * class1 op31 (accept quest ACK), class1 op32 (give up quest ACK),
//     class1 op34 (finish quest ACK), class1 op205, and class0 op21/op356/op574
//     all touch the client quest-manager singleton at VA 0x051B2798.
//   * class1 op239 (give up expert job ACK, VA 0x01D20590) does NOT. It only
//     updates the expert-job/character model through [0x51B2728].
//   So when an expert job is given up, the qualification quests stay marked
//   completed in the client's quest model forever; the quest window then offers
//   no accept, the client never sends op31, and only a character re-selection
//   (which rebuilds the model from the login ACK) clears it. The server is
//   correct throughout: op21 lists the quests again and the op356 byte is 0 at
//   the same moment.
//
// THE PATCH: the success path of the client's op239 handler ends with
//
//     0x1D20709  mov  ecx, esi
//     0x1D2070B  call 0x2692400      <-- unconditional, last call on success
//     0x1D20710  pop  edi ...
//     0x1D20715  ret
//
// so redirecting that one call site gives "run after the client finished its own
// updates", which is required: the cleanup must not be overwritten by anything
// the handler does later. The callee pops no stack bytes (__thiscall, 0 args),
// so the replacement is declared __fastcall (ECX/EDX, plain ret) and matches the
// original's stack contract exactly. The original is still called first, so the
// client's own behaviour is unchanged.
//
// The cleanup then replays, for every expert-job transition quest, the same
// quest-manager call the client itself uses when a quest leaves the model
// (op32 calls it with the quest id right after its ACK): VA 0x01A02030.
//
// Enable/disable at runtime with QUESTPROBE_EXPERTJOB_FIX=0. Every step is
// logged, including the patched call site and each removed quest id.
// ===========================================================================
static const unsigned int kOp239HandlerRva = 0x01920590;
static const unsigned int kOp239SuccessTailCallRva = 0x0192070B; // call 0x2692400
static const unsigned int kOp239SuccessTailCalleeRva = 0x02292400;
static const unsigned int kQuestManagerPointerRva = 0x04DB2798;
static const unsigned int kQuestRemoveEntryRva = 0x01602030;
// op32 calls this as (questID, 0) and (questID, 2) and uses the returned
// pointer, so it is a pure lookup into the client's quest model: __thiscall,
// two stack arguments, ret 8. Reading through it shows exactly what state the
// client holds for a quest - which is the measurement this probe now needs.
static const unsigned int kQuestLookupEntryRva = 0x01609400;

// PVF `[job change quest] 20` set, identical to the server's
// Catalog.ExpertJobTransitionQuestIDs on the real Script.pvf (pinned by
// catalog_real_pvf_test.go), which is exactly the set the give-up clears.
static const unsigned short kExpertJobTransitionQuestIDs[] = {
    2702, 2708, 2710, 2712, 10020, 10021,
    11007, 11009, 11013, 11014, 11015, 11016, 11017, 11018, 11019,
};

typedef void (__fastcall* Op239TailCalleeFn)(void* self, void* edx);
typedef void (__thiscall* QuestRemoveEntryFn)(void* self, unsigned int questID);
typedef void* (__thiscall* QuestLookupEntryFn)(void* self, unsigned int questID, unsigned int mode);

static bool g_expertJobFixEnabled = false;
static volatile LONG g_expertJobFixRuns = 0;

// ---------------------------------------------------------------------------
// Quest-state observation. Three quests, two lookup modes, 32 bytes of the
// returned entry each. The point is the DIFF between "just gave up the expert
// job" (accept refused) and "after a character re-selection" (accept works):
// whatever field differs is the gate the client consults, and no amount of
// guessing on the server side has found it.
// ---------------------------------------------------------------------------
static const unsigned short kObservedQuestIDs[] = { 2710, 2702, 10020 };

// ---------------------------------------------------------------------------
// Accept-path gates. The only code in the client that writes opcode 31 is
// sub_1C53100(questID); it has no gate of its own. Every caller refuses first:
//
//   0x1147F09  call 0x1A028C0     ; guard 1 (this = [0x51B2798], arg = quest id)
//   0x1147F0E  test al, al
//   0x1147F10  jne  skip          ; nonzero -> the accept is never sent
//   0x1147F19  call 0x19EECD0     ; guard 2
//   0x1147F1E  test al, al
//   0x1147F20  jne  skip
//   0x1147F2A  call 0x1C53100     ; send the accept
//
// Both guards are read-only predicates on the quest manager, so they can be
// CALLED for observation without patching anything. Guard 2 walks the container
// at [manager+8]..[manager+0xC] with stride 0xC and returns true when the quest
// id is present; that container's contents are dumped too, which is what tells
// us what the client believes about the given-up qualification quest.
// ---------------------------------------------------------------------------
static const unsigned int kGuard1Rva = 0x016028C0;
static const unsigned int kGuard2Rva = 0x015EECD0;
static const unsigned int kQuestAcceptSenderRva = 0x01853100;

// A plain __thiscall caller. MSVC refuses to reinterpret_cast a code address
// into a pointer-to-member-function, so this thunk does the call explicitly:
// the guard is __thiscall (this in ECX, one stack argument, and its own `ret 4`
// pops that argument), which leaves the stack balanced for the C caller.
extern "C" unsigned char __cdecl CallThiscallGuard(
    void* target, void* self, unsigned int questID);

__declspec(naked) unsigned char __cdecl CallThiscallGuard(
    void* target, void* self, unsigned int questID)
{
    __asm {
        push ebp
        mov ebp, esp
        mov ecx, dword ptr [ebp + 8]      // self  -> this
        mov eax, dword ptr [ebp + 12]     // questID
        push eax
        call dword ptr [ebp + 16]         // guard; its `ret 4` pops our argument
        pop ebp
        ret
    }
}

// ---------------------------------------------------------------------------
// Guard observation at the real call sites.
//
// Each of the two accept callers tests both guards and skips the send when one
// is nonzero. Redirecting the four guard call sites gives the ACTUAL values the
// client sees when the player clicks accept, which is the only reliable way to
// learn which one refuses: the guards cannot be called from a worker thread
// (guard 1 dereferences its lookup result before null-checking it).
//
// A call-site redirect has to reproduce the callee's contract exactly: both
// guards are __thiscall with one stack argument and pop it themselves (`ret 4`),
// so the proxies end in `ret 4` and hand the original a duplicate of the
// argument so its own `ret 4` has something to pop.
// ---------------------------------------------------------------------------
constexpr int kGuardSiteCount = 4;
static void* g_guardTrampolines[kGuardSiteCount] = {};

struct GuardCallSite {
    unsigned int siteRva;
    unsigned int calleeRva;
    const char* label;
};

// caller A (VA 0x00CAB5B0) and caller B (VA 0x01147ED0), guard 1 then guard 2.
// Both fields are FILE RVAs: the call sites at VA 0xCAB729/0xCAB739/0x1147F09/
// 0x1147F19 and the callees at VA 0x1A028C0 (RVA 0x16028C0) and VA 0x19EECD0
// (RVA 0x15EECD0). Writing the VAs here instead was the first attempt's bug: the
// prologue read landed 0x400000 bytes high and all four hooks were skipped.
static const GuardCallSite kGuardCallSites[kGuardSiteCount] = {
    { 0x008AB729, 0x016028C0, "A/guard1" },
    { 0x008AB739, 0x015EECD0, "A/guard2" },
    { 0x00D47F09, 0x016028C0, "B/guard1" },
    { 0x00D47F19, 0x015EECD0, "B/guard2" },
};

extern "C" void __cdecl LogGuardEntry(unsigned int slot, void* manager, unsigned int questID)
{
    if (slot >= static_cast<unsigned int>(kGuardSiteCount)) return;
    LogLine("guard-call %s manager=%p quest=%u",
        kGuardCallSites[slot].label, manager, questID);
}

extern "C" void __cdecl LogGuardResult(unsigned int slot, unsigned int result)
{
    if (slot >= static_cast<unsigned int>(kGuardSiteCount)) return;
    LogLine("guard-call %s -> %u%s", kGuardCallSites[slot].label, result & 0xFFu,
        (result & 0xFFu) ? "  (nonzero: this gate REFUSES the accept)" : "");
}

__declspec(naked) void GuardProxy0()
{
    __asm {
        pushad
        pushfd
        mov eax, dword ptr [esp + 40]
        push eax
        push ecx
        push 0
        call LogGuardEntry
        add esp, 12
        popfd
        popad
        push dword ptr [esp + 8]
        lea eax, g_guardTrampolines
        call dword ptr [eax + 0]
        pushad
        pushfd
        movzx eax, al
        push eax
        push 0
        call LogGuardResult
        add esp, 8
        popfd
        popad
        ret 4
    }
}

__declspec(naked) void GuardProxy1()
{
    __asm {
        pushad
        pushfd
        mov eax, dword ptr [esp + 40]
        push eax
        push ecx
        push 1
        call LogGuardEntry
        add esp, 12
        popfd
        popad
        push dword ptr [esp + 8]
        lea eax, g_guardTrampolines
        call dword ptr [eax + 4]
        pushad
        pushfd
        movzx eax, al
        push eax
        push 1
        call LogGuardResult
        add esp, 8
        popfd
        popad
        ret 4
    }
}

__declspec(naked) void GuardProxy2()
{
    __asm {
        pushad
        pushfd
        mov eax, dword ptr [esp + 40]
        push eax
        push ecx
        push 2
        call LogGuardEntry
        add esp, 12
        popfd
        popad
        push dword ptr [esp + 8]
        lea eax, g_guardTrampolines
        call dword ptr [eax + 8]
        pushad
        pushfd
        movzx eax, al
        push eax
        push 2
        call LogGuardResult
        add esp, 8
        popfd
        popad
        ret 4
    }
}

__declspec(naked) void GuardProxy3()
{
    __asm {
        pushad
        pushfd
        mov eax, dword ptr [esp + 40]
        push eax
        push ecx
        push 3
        call LogGuardEntry
        add esp, 12
        popfd
        popad
        push dword ptr [esp + 8]
        lea eax, g_guardTrampolines
        call dword ptr [eax + 12]
        pushad
        pushfd
        movzx eax, al
        push eax
        push 3
        call LogGuardResult
        add esp, 8
        popfd
        popad
        ret 4
    }
}

static void* const g_guardProxies[kGuardSiteCount] = {
    reinterpret_cast<void*>(&GuardProxy0),
    reinterpret_cast<void*>(&GuardProxy1),
    reinterpret_cast<void*>(&GuardProxy2),
    reinterpret_cast<void*>(&GuardProxy3),
};

static void* CreateRelocatedTrampoline(void* target)
{
    unsigned char prologue[8] = { 0 };
    unsigned char stolen[8] = { 0 };
    __try {
        memcpy(prologue, target, sizeof(prologue));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return nullptr;
    }
    int length = MatchPrologue(prologue, stolen);
    if (!length) return nullptr;

    unsigned char* trampoline = reinterpret_cast<unsigned char*>(
        VirtualAlloc(nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!trampoline) return nullptr;
    memcpy(trampoline, stolen, static_cast<size_t>(length));
    trampoline[length] = 0xE9;
    intptr_t back = reinterpret_cast<intptr_t>(
        reinterpret_cast<unsigned char*>(target) + length) -
        (reinterpret_cast<intptr_t>(trampoline + length) + 5);
    memcpy(trampoline + length + 1, &back, sizeof(back));
    FlushInstructionCache(GetCurrentProcess(), trampoline, 64);
    return trampoline;
}

static bool RedirectCallSite(unsigned int siteRva, unsigned int expectedCalleeRva, void* proxy)
{
    unsigned char* site = g_base + siteRva;
    unsigned char original[5] = { 0 };
    __try {
        memcpy(original, site, sizeof(original));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("guard-call site 0x%08X not readable", siteRva);
        return false;
    }
    if (original[0] != 0xE8) {
        LogLine("guard-call site 0x%08X is not a relative call (0x%02X)", siteRva, original[0]);
        return false;
    }
    int existing = 0;
    memcpy(&existing, original + 1, sizeof(existing));
    uintptr_t actual = reinterpret_cast<uintptr_t>(site + 5) + existing;
    if (actual != reinterpret_cast<uintptr_t>(g_base + expectedCalleeRva)) {
        LogLine("guard-call site 0x%08X targets 0x%08X, expected 0x%08X",
            siteRva, static_cast<unsigned int>(actual),
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_base + expectedCalleeRva)));
        return false;
    }

    unsigned char patch[5] = { 0 };
    patch[0] = 0xE8;
    intptr_t relative = reinterpret_cast<intptr_t>(proxy) -
        (reinterpret_cast<intptr_t>(site) + 5);
    memcpy(patch + 1, &relative, sizeof(relative));

    DWORD oldProtect = 0;
    if (!VirtualProtect(site, sizeof(patch), PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("guard-call site 0x%08X VirtualProtect failed err=%lu", siteRva, GetLastError());
        return false;
    }
    memcpy(site, patch, sizeof(patch));
    VirtualProtect(site, sizeof(patch), oldProtect, &oldProtect);
    FlushInstructionCache(GetCurrentProcess(), site, sizeof(patch));
    return true;
}

static void InstallGuardObservers()
{
    // Off by default. The call-site redirects are behaviour-preserving, but the
    // 2026-09-15 round showed the accepted quest never registering in the client
    // while they were active, so they are opt-in until that is separated out:
    // QUESTPROBE_GUARD_HOOKS=1 turns them on.
    wchar_t guardValue[8] = { 0 };
    DWORD guardLength = GetEnvironmentVariableW(L"QUESTPROBE_GUARD_HOOKS", guardValue, 8);
    if (!(guardLength > 0 && guardValue[0] == L'1')) {
        LogLine("guard-call observers disabled (QUESTPROBE_GUARD_HOOKS=1 enables them)");
        return;
    }
    for (int i = 0; i < kGuardSiteCount; ++i) {
        const GuardCallSite& entry = kGuardCallSites[i];
        if (!g_guardTrampolines[i]) {
            g_guardTrampolines[i] = CreateRelocatedTrampoline(
                reinterpret_cast<void*>(g_base + entry.calleeRva));
            if (!g_guardTrampolines[i]) {
                LogLine("guard-call %s: cannot relocate prologue, not observed", entry.label);
                continue;
            }
        }
        bool ok = RedirectCallSite(entry.siteRva, entry.calleeRva, g_guardProxies[i]);
        LogLine("guard-call %s installed=%d site=0x%08X callee=0x%08X trampoline=%p",
            entry.label, ok ? 1 : 0, static_cast<unsigned int>(
                reinterpret_cast<uintptr_t>(g_base + entry.siteRva)),
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_base + entry.calleeRva)),
            g_guardTrampolines[i]);
    }
}

static void DumpAcceptGates(const char* tag)
{
    void* manager = nullptr;
    __try {
        manager = *reinterpret_cast<void**>(g_base + kQuestManagerPointerRva);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("accept-gate %s manager read fault", tag);
        return;
    }
    if (!manager) {
        LogLine("accept-gate %s manager=null", tag);
        return;
    }

    // The guards are deliberately NOT called from here. Guard 1 dereferences its
    // lookup result before null-checking it (`call 0x1F96210` then
    // `mov ecx, [eax]` at VA 0x01A028EA), so calling it off the packet thread
    // faults with 0xC0000005 - which is exactly what the first version of this
    // probe did three times per tick. They are observed at their real call sites
    // in the accept path instead; see the guard proxies above.

    // Guard 2's container: [manager+8] begin, [manager+0xC] end, stride 0xC,
    // each entry points at a row whose first dword is the quest id.
    void* begin = nullptr;
    void* end = nullptr;
    __try {
        begin = *reinterpret_cast<void**>(reinterpret_cast<unsigned char*>(manager) + 8);
        end = *reinterpret_cast<void**>(reinterpret_cast<unsigned char*>(manager) + 0xC);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("accept-gate %s container read fault", tag);
        return;
    }
    if (!begin || !end || end < begin) {
        LogLine("accept-gate %s container begin=%p end=%p", tag, begin, end);
        return;
    }
    ptrdiff_t bytes = reinterpret_cast<unsigned char*>(end) -
        reinterpret_cast<unsigned char*>(begin);
    long count = static_cast<long>(bytes / 0xC);
    LogLine("accept-gate %s container begin=%p end=%p entries=%ld", tag, begin, end, count);

    char ids[512] = { 0 };
    int used = 0;
    long limit = count > 24 ? 24 : count;
    for (long i = 0; i < limit; ++i) {
        __try {
            unsigned char* row = reinterpret_cast<unsigned char*>(begin) + i * 0xC;
            void* inner = *reinterpret_cast<void**>(row);
            unsigned int id = inner ? *reinterpret_cast<unsigned int*>(inner) : 0xFFFFFFFFu;
            used += _snprintf(ids + used, sizeof(ids) - used - 1, " %u", id);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            used += _snprintf(ids + used, sizeof(ids) - used - 1, " <fault>");
        }
        if (used < 0 || static_cast<size_t>(used) >= sizeof(ids) - 16) break;
    }
    LogLine("accept-gate %s container ids:%s", tag, ids);
}


static void DumpQuestEntry(void* manager, unsigned int questID, unsigned int mode,
    const char* tag)
{
    QuestLookupEntryFn lookup = reinterpret_cast<QuestLookupEntryFn>(
        g_base + kQuestLookupEntryRva);
    void* entry = nullptr;
    __try {
        entry = lookup(manager, questID, mode);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("quest-state %s q=%u mode=%u lookup fault code=0x%08X",
            tag, questID, mode, GetExceptionCode());
        return;
    }
    if (!entry) {
        LogLine("quest-state %s q=%u mode=%u entry=null", tag, questID, mode);
        return;
    }

    unsigned char bytes[32] = { 0 };
    __try {
        memcpy(bytes, entry, sizeof(bytes));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("quest-state %s q=%u mode=%u entry=%p read fault", tag, questID, mode, entry);
        return;
    }

    char hex[128] = { 0 };
    FormatBytes(bytes, static_cast<int>(sizeof(bytes)), hex, sizeof(hex));
    LogLine("quest-state %s q=%u mode=%u entry=%p [%s]", tag, questID, mode, entry, hex);
}

static void DumpQuestModel(const char* tag)
{
    void* manager = nullptr;
    __try {
        manager = *reinterpret_cast<void**>(g_base + kQuestManagerPointerRva);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("quest-state %s manager read fault", tag);
        return;
    }
    if (!manager) {
        LogLine("quest-state %s manager=null", tag);
        return;
    }
    for (unsigned int i = 0;
         i < sizeof(kObservedQuestIDs) / sizeof(kObservedQuestIDs[0]); ++i) {
        DumpQuestEntry(manager, kObservedQuestIDs[i], 0, tag);
        DumpQuestEntry(manager, kObservedQuestIDs[i], 2, tag);
    }
}

extern "C" void __cdecl LogExpertJobFixRun(unsigned int runNumber)
{
    LogLine("expert-job fix: cleanup #%u starting", runNumber);
}

static void RunExpertJobQuestModelCleanup()
{
    void* manager = nullptr;
    __try {
        manager = *reinterpret_cast<void**>(g_base + kQuestManagerPointerRva);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("expert-job fix: quest-manager pointer read fault code=0x%08X",
            GetExceptionCode());
        return;
    }
    if (!manager) {
        LogLine("expert-job fix: quest manager is null, nothing to clean");
        return;
    }

    QuestRemoveEntryFn removeFn = reinterpret_cast<QuestRemoveEntryFn>(
        g_base + kQuestRemoveEntryRva);

    int removed = 0;
    for (unsigned int i = 0;
         i < sizeof(kExpertJobTransitionQuestIDs) / sizeof(kExpertJobTransitionQuestIDs[0]);
         ++i) {
        unsigned int questID = kExpertJobTransitionQuestIDs[i];
        __try {
            removeFn(manager, questID);
            ++removed;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("expert-job fix: quest %u cleanup fault code=0x%08X",
                questID, GetExceptionCode());
        }
    }
    LogLine("expert-job fix: cleanup done, manager=%p quests=%d/%d",
        manager, removed,
        static_cast<int>(sizeof(kExpertJobTransitionQuestIDs) /
                         sizeof(kExpertJobTransitionQuestIDs[0])));
}

// Replacement for the last call on the op239 success path. Same contract as the
// original (__thiscall, zero stack args), the original still runs first.
static void __fastcall ExpertJobFixOp239Tail(void* self, void* edx)
{
    Op239TailCalleeFn original = reinterpret_cast<Op239TailCalleeFn>(
        g_base + kOp239SuccessTailCalleeRva);
    if (original) {
        original(self, edx);
    }

    LONG run = InterlockedIncrement(&g_expertJobFixRuns);
    LogLine("op239 success observed (#%d): the client's own updates for this ACK are done",
        run);
    DumpAcceptGates("after-op239");

    if (!g_expertJobFixEnabled) {
        return;
    }
    if (run <= 8) {
        LogExpertJobFixRun(static_cast<unsigned int>(run));
    }
    RunExpertJobQuestModelCleanup();
}

static bool InstallExpertJobFix()
{
    if (!g_expertJobFixEnabled) {
        LogLine("expert-job fix: disabled (QUESTPROBE_EXPERTJOB_FIX=0)");
        return false;
    }

    unsigned char* site = g_base + kOp239SuccessTailCallRva;
    unsigned char original[5] = { 0 };
    __try {
        memcpy(original, site, sizeof(original));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("expert-job fix: call site not readable");
        return false;
    }

    if (original[0] != 0xE8) {
        LogLine("expert-job fix: call site is not a relative call (0x%02X) - left alone",
            original[0]);
        return false;
    }

    int existingRelative = 0;
    memcpy(&existingRelative, original + 1, sizeof(existingRelative));
    uintptr_t existingTarget = reinterpret_cast<uintptr_t>(site + 5) + existingRelative;
    uintptr_t expectedTarget = reinterpret_cast<uintptr_t>(g_base + kOp239SuccessTailCalleeRva);
    if (existingTarget != expectedTarget) {
        LogLine("expert-job fix: call site targets 0x%08X, expected 0x%08X - left alone",
            static_cast<unsigned int>(existingTarget),
            static_cast<unsigned int>(expectedTarget));
        return false;
    }

    unsigned char patch[5] = { 0 };
    patch[0] = 0xE8;
    intptr_t relative = reinterpret_cast<intptr_t>(&ExpertJobFixOp239Tail) -
        (reinterpret_cast<intptr_t>(site) + 5);
    memcpy(patch + 1, &relative, sizeof(relative));

    DWORD oldProtect = 0;
    if (!VirtualProtect(site, sizeof(patch), PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("expert-job fix: VirtualProtect failed err=%lu", GetLastError());
        return false;
    }
    memcpy(site, patch, sizeof(patch));
    VirtualProtect(site, sizeof(patch), oldProtect, &oldProtect);
    FlushInstructionCache(GetCurrentProcess(), site, sizeof(patch));

    LogLine("expert-job fix: INSTALLED call site=0x%08X was-0x%08X now=0x%08X handler=0x%08X",
        static_cast<unsigned int>(reinterpret_cast<uintptr_t>(site)),
        static_cast<unsigned int>(expectedTarget),
        static_cast<unsigned int>(reinterpret_cast<uintptr_t>(&ExpertJobFixOp239Tail)),
        static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_base + kOp239HandlerRva)));
    return true;
}

// ---------------------------------------------------------------------------
// Full registry map. The first attempt at 20:10:23 ran 0 ms after attach and
// every lookup returned 0 while the registries themselves were already non-null,
// so a non-null registry does NOT mean the handlers are registered yet. Sweep a
// wide opcode range and log every opcode that does resolve, which also hands the
// offline side the complete handler map for the quest family.
// ---------------------------------------------------------------------------
static void SweepRegistryMap(const char* tag)
{
    DispatchRegistryFn registryFn0 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass0RegistryRva);
    DispatchRegistryFn registryFn1 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass1RegistryRva);
    DispatchLookupFn lookupFn = reinterpret_cast<DispatchLookupFn>(
        g_base + kDispatchLookupRva);

    const unsigned int ranges[2][2] = { { 0, 900 }, { 1, 260 } };
    for (int r = 0; r < 2; ++r) {
        unsigned int classID = ranges[r][0];
        unsigned int limit = ranges[r][1];

        unsigned int found[1200] = { 0 };
        unsigned int foundRva[1200] = { 0 };
        int foundCount = 0;
        int total = 0;

        __try {
            for (unsigned int opcode = 1; opcode <= limit; ++opcode) {
                void* registry = (classID == 0 ? registryFn0 : registryFn1)();
                if (!registry) break;
                void* record = lookupFn(registry, opcode);
                if (!record) continue;
                ++total;
                if (foundCount < 1200) {
                    uintptr_t callback = *reinterpret_cast<uintptr_t*>(record);
                    if (callback > reinterpret_cast<uintptr_t>(g_base) &&
                        callback < reinterpret_cast<uintptr_t>(g_base) + 0x6000000) {
                        found[foundCount] = opcode;
                        foundRva[foundCount] = static_cast<unsigned int>(
                            callback - reinterpret_cast<uintptr_t>(g_base));
                        ++foundCount;
                    }
                }
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("sweep(%s) fault class=%u code=0x%08X", tag, classID, GetExceptionCode());
            continue;
        }

        LogLine("sweep(%s) class=%u scanned=%u handlers=%d (full map below)",
            tag, classID, limit, total);
        for (int i = 0; i < foundCount; i += 6) {
            char line[512] = { 0 };
            int used = 0;
            for (int j = i; j < i + 6 && j < foundCount; ++j) {
                used += _snprintf(line + used, sizeof(line) - used - 1,
                    " op%u=0x%08X", found[j], foundRva[j]);
            }
            LogLine("sweep(%s) class=%u%s", tag, classID, line);
        }
    }
}

static DWORD WINAPI ProbeWorker(LPVOID)
{
    LogLine("worker started, waiting for the client to build its opcode registries");

    DispatchRegistryFn registryFn0 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass0RegistryRva);
    DispatchRegistryFn registryFn1 = reinterpret_cast<DispatchRegistryFn>(
        g_base + kClass1RegistryRva);

    bool ready = false;
    for (int attempt = 0; attempt < 720; ++attempt) {
        void* r0 = nullptr;
        void* r1 = nullptr;
        __try {
            r0 = registryFn0 ? registryFn0() : nullptr;
            r1 = registryFn1 ? registryFn1() : nullptr;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            r0 = nullptr;
            r1 = nullptr;
        }
        if (r0 && r1) {
            LogLine("registries ready after %d ms class0=%p class1=%p",
                attempt * 250, r0, r1);
            ready = true;
            break;
        }
        Sleep(250);
    }

    if (!ready) {
        LogLine("registries never became ready - giving up (RVAs may not match this EXE)");
        return 0;
    }

    // A non-null registry does not mean the handlers are registered: the first
    // run resolved both registries 0 ms after attach and still got record=0 for
    // every opcode. Retry until a quest handler actually appears, then install.
    bool resolved = false;
    for (int attempt = 0; attempt < 900; ++attempt) {
        int handlerCount = ResolveAndProbe();
        if (handlerCount > 0) {
            LogLine("quest handlers appeared on attempt %d (%d s after the registries): %d resolved",
                attempt, attempt * 2, handlerCount);
            SweepRegistryMap("first");
            resolved = true;
            break;
        }        if ((attempt % 15) == 0) {
            LogLine("retry %d: no quest handler registered yet", attempt);
        }
        Sleep(2000);
    }

    if (!resolved) {
        LogLine("no quest handler ever resolved within %d seconds", 900 * 2);
        SweepRegistryMap("final");
    } else if (g_hooksEnabled) {
        // Modules can register late; one more sweep catches handlers that were
        // absent during the first pass (already-hooked targets are skipped).
        Sleep(90000);
        SweepRegistryMap("late");
        ResolveAndProbe();
    }

    // The fix does not depend on the registry, but install it only once the
    // client has finished booting so the call site is definitely the one we
    // validated offline.
    InstallExpertJobFix();
    InstallGuardObservers();

    int tick = 0;
    for (;;) {
        Sleep(10000);
        char summary[640] = { 0 };
        int used = 0;
        for (int i = 0; i < kProbeSlotCount; ++i) {
            if (!g_slotOpcode[i]) continue;
            used += _snprintf(summary + used, sizeof(summary) - used - 1,
                " c%u/op%u=%d", g_slotClass[i], g_slotOpcode[i],
                static_cast<int>(g_slotHits[i]));
            if (used < 0 || static_cast<size_t>(used) >= sizeof(summary) - 32) break;
        }
        LogLine("heartbeat%s", summary);

        ++tick;
        if (tick <= 240) {
            char tag[32] = { 0 };
            _snprintf(tag, sizeof(tag) - 1, "tick%d", tick);
            DumpAcceptGates(tag);
        }
    }
    return 0;
}

BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID)
{
    if (reason != DLL_PROCESS_ATTACH) return TRUE;

    DisableThreadLibraryCalls(instance);
    InitializeCriticalSection(&g_logLock);
    g_logReady = true;

    g_base = reinterpret_cast<unsigned char*>(GetModuleHandleW(nullptr));

    wchar_t hooksValue[8] = { 0 };
    DWORD hooksLength = GetEnvironmentVariableW(L"QUESTPROBE_HOOKS", hooksValue, 8);
    g_hooksEnabled = hooksLength > 0 && hooksValue[0] == L'1';

    // The cleanup proved insufficient on 2026-09-15 - it ran cleanly (15/15
    // quest ids, valid manager, no faults) and the quest was still refused - so
    // it is opt-in now. This round only MEASURES: QUESTPROBE_EXPERTJOB_FIX=1
    // re-enables the cleanup on top of the measurement.
    wchar_t fixValue[8] = { 0 };
    DWORD fixLength = GetEnvironmentVariableW(L"QUESTPROBE_EXPERTJOB_FIX", fixValue, 8);
    g_expertJobFixEnabled = fixLength > 0 && fixValue[0] == L'1';

    wchar_t modulePath[MAX_PATH] = { 0 };
    GetModuleFileNameW(nullptr, modulePath, MAX_PATH);
    LogLine("QuestProbe.dll attached base=%p module=%ls hooks=%d expert_job_fix=%d "
            "(QUESTPROBE_HOOKS=1 enables entry hooks, QUESTPROBE_EXPERTJOB_FIX=0 disables the fix)",
        g_base, modulePath, g_hooksEnabled ? 1 : 0, g_expertJobFixEnabled ? 1 : 0);

    HANDLE thread = CreateThread(nullptr, 0, ProbeWorker, nullptr, 0, nullptr);
    if (thread) {
        CloseHandle(thread);
    } else {
        LogLine("worker thread creation failed err=%lu", GetLastError());
    }
    return TRUE;
}
