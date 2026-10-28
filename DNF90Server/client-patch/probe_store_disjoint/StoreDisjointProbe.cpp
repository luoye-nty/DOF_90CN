// StoreDisjointProbe.dll -- standalone, READ-ONLY probe for the personal-store
// (expert-job / 副职�? disassembly machine's "disassemble all" flow.
//
// It is deliberately separate from 90CN.dll. Nothing here changes game
// behaviour: every detour calls the original and returns its exact result.
// Merge into client-patch/90CN.cpp only after the probe's questions are
// answered.
//
// ---------------------------------------------------------------------------
// Why this probe exists
//
// The plan is to batch op235 (CmdPacketRequestDisjointItem, the player/store
// disassembly machine) the way op26 (CmdPacketDisjointItem, the NPC/system
// machine) is already batched: suppress the per-item sends, emit ONE marked
// packet carrying every selected slot, and let the server commit it in one
// transaction. The op26 batch works because the op26 loop calls its per-item
// sender N times inside ONE pass.
//
// Static analysis says the op235 loop (sub_2C38840) has the same shape as the
// op26 loop (sub_2C38260), but four things must be confirmed on a live client
// before writing the hook, because they change its design:
//
//   Q1  Does one pass of sub_2C38840 call the sender N times (multi-select), or
//       only once (single-item path, one round trip each)?
//       Measured as "EXIT ... sends=<N>" per pass, for op235 AND for op26 as a
//       control. If op26 shows sends=N and op235 shows sends=1, the hook must
//       accumulate across passes instead of copying the op26 template.
//   Q2  Is [win+0x1EC] == 0 during "disassemble all"? Non-zero means the loop
//       takes the single-item branch and stops after the first send.
//   Q3  Is [win+0x1B8] == 1 while the loop runs? (2 after the loop is expected.)
//   Q4  Is [listMgr+0x7C] == 0 at every send? The native op235 sender SKIPS the
//       packet when that byte is non-zero but still returns TRUE, so a batch
//       hook has to replicate that check or it will batch items the client
//       meant to skip.
//
// Extra context captured per pass: the selection vector and its element count,
// [win+0x180] store owner id, [win+0x190] machine endurance, [win+0x1B0] mode /
// part index, plus every call to the window state setter sub_2C313A0 with
// timestamps -- so "N packets left at once" vs "one packet per reply" is
// visible directly in the log.
//
// ---------------------------------------------------------------------------
// Addresses (module base 0x00400000, verified against
// D:\test\地下城与勇士\DNF.exe):
//
//   sub_2C38840  op235 loop    "disassemble all" driver, store machine
//                              __thiscall void f(this), ret
//   sub_1F30B80  op235 sender  __thiscall bool f(this, slot, ownerID), ret 8
//                              push 0xEB -> WriteU16 -> WriteI16(ownerID)
//                              -> WriteI16(slot) -> WriteU8(list) -> Flush
//   sub_2C38260  op26 loop     same driver for the NPC machine (CONTROL)
//   sub_1F30630  op26 sender   __thiscall bool f(this, slot, context), ret 8
//   sub_2C313A0  state setter  __thiscall void f(this, newState), ret 4
//                              writes [win+0x1B8]; the 2->1 reset that re-arms a
//                              loop pass comes through here
//
// Log: <game dir>\StoreDisjointProbe.log   Config: ...\StoreDisjointProbe.ini

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>
#include <stdlib.h>

#define ARRAY_COUNT(a) (sizeof(a) / sizeof((a)[0]))

namespace {

const unsigned int kImageBase = 0x00400000u;

// ---------------------------------------------------------------------------
// Hook prologues. Each must end on an instruction boundary: the first
// `length` bytes are relocated into the trampoline, so a partial instruction
// here would corrupt the original. Lengths are the shortest whole-instruction
// prefix >= 5 bytes.
// ---------------------------------------------------------------------------
const unsigned char kPro235Loop[] = {0x55, 0x8B, 0xEC, 0x6A, 0xFF,          // push ebp; mov ebp,esp; push -1
                                     0x68, 0xB8, 0x0A, 0xED, 0x03};         // push <SEH handler>
const unsigned char kPro235Send[] = {0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x18,    // push ebp; mov ebp,esp; sub esp,0x18
                                     0x56, 0x8B, 0xF1};                     // push esi; mov esi,ecx
const unsigned char kPro26Loop[] = {0x55, 0x8B, 0xEC, 0x6A, 0xFF,
                                    0x68, 0x28, 0xAF, 0xF1, 0x03};
const unsigned char kPro26Send[] = {0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x28,     // push ebp; mov ebp,esp; sub esp,0x28
                                    0xA1, 0x3C, 0x67, 0x0E, 0x05};          // mov eax,[0x50E673C]
const unsigned char kProStateSet[] = {0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08,   // push ebp; mov ebp,esp; mov eax,[ebp+8]
                                      0x56, 0x8B, 0xF1};                    // push esi; mov esi,ecx

const unsigned int kRva235Loop = 0x02C38840u;
const unsigned int kRva235Send = 0x01F30B80u;
const unsigned int kRva26Loop = 0x02C38260u;
const unsigned int kRva26Send = 0x01F30630u;
const unsigned int kRvaStateSet = 0x02C313A0u;

// ---------------------------------------------------------------------------
// Long, distinctive fingerprints of the five targets, used to RELOCATE them in
// memory when the declared prologue does not match (a different client build
// shifts every RVA). `??` is a wildcard byte: it covers build-specific
// immediates such as the SEH handler address and the /GS cookie.
//
// The two loops are separated by sub esp,0x50 (op235) vs sub esp,0x3C (op26),
// which is what makes these fingerprints unambiguous between them.
// ---------------------------------------------------------------------------
const char* kPattern235Loop =
    "55 8B EC 6A FF 68 ?? ?? ?? ?? 64 A1 00 00 00 00"
    " 50 83 EC 50 53 56 57 A1 ?? ?? ?? ?? 33 C5 50 8D"
    " 45 F4 64 A3 00 00 00 00 8B F1 89 75 E8 83";
const char* kPattern235Send =
    "55 8B EC 83 EC 18 56 8B F1 6A 02 89 75 F8 E8 DD";
const char* kPattern26Loop =
    "55 8B EC 6A FF 68 ?? ?? ?? ?? 64 A1 00 00 00 00"
    " 50 83 EC 3C 53 56 57 A1 ?? ?? ?? ?? 33 C5 50 8D"
    " 45 F4 64 A3 00 00 00 00 8B D9 89 5D EC 83";
const char* kPattern26Send =
    "55 8B EC 83 EC 28 A1 ?? ?? ?? ?? 33 C5 89 45 FC 8B 45 08 53";
const char* kPatternStateSet =
    "55 8B EC 8B 45 08 56 8B F1 89 86 B8 01 00 00 83";

const unsigned int kLen235Loop = 5;   // push ebp | mov ebp,esp | push -1            = 1+2+2
const unsigned int kLen235Send = 6;   // push ebp | mov ebp,esp | sub esp,0x18       = 1+2+3
const unsigned int kLen26Loop = 5;
const unsigned int kLen26Send = 6;
const unsigned int kLenStateSet = 6;  // push ebp | mov ebp,esp | mov eax,[ebp+8]   = 1+2+3
                                      // (5 would cut mov eax,[ebp+8] in half!)

// ---------------------------------------------------------------------------
// Window field offsets (read straight out of the two loops / their readers).
// ---------------------------------------------------------------------------
const unsigned int kOffPartIndex = 0x1B0;  // mode / part index (op26: 1,16,26; op235: 15)
const unsigned int kOffOwnerId = 0x180;    // store owner character id
const unsigned int kOffFlag184 = 0x184;
const unsigned int kOffLimit18C = 0x18C;
const unsigned int kOffEndurance = 0x190;  // written by the op235 reader
const unsigned int kOffState = 0x1B8;      // 0 idle / 1 may-send / 2 sent-waiting
const unsigned int kOffPending = 0x1EC;    // non-zero => single-item path
const unsigned int kOffSelBegin = 0x424;
const unsigned int kOffSelEnd = 0x428;
const unsigned int kOffSentCount = 0x434;
const unsigned int kOffFlag438 = 0x438;
const unsigned int kOffMgrSkip = 0x07C;    // on the list manager: non-zero skips the op235 packet

enum ProbeMode { PROBE_MODE_PROBE = 0, PROBE_MODE_QUIET = 1, PROBE_MODE_OFF = 2 };

volatile LONG g_mode = PROBE_MODE_PROBE;
volatile LONG g_logSends = 1;
volatile LONG g_control = 1;  // also hook the op26 pair as an A/B control
volatile LONG g_installed = 0;
volatile LONG g_autoRebase = 1;  // relocate hook targets by fingerprint when the
                                 // declared RVA does not match (different build)

// Cumulative counters (reported by the heartbeat thread).
volatile LONG g_pass235 = 0;
volatile LONG g_pass26 = 0;
volatile LONG g_send235 = 0;
volatile LONG g_send26 = 0;
volatile LONG g_refused235 = 0;
volatile LONG g_refused26 = 0;
volatile LONG g_skip7C235 = 0;
volatile LONG g_stateSetCalls = 0;

CRITICAL_SECTION g_logLock;
bool g_logReady = false;
wchar_t g_logPath[MAX_PATH * 2] = {0};
wchar_t g_configPath[MAX_PATH * 2] = {0};

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------
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
    if (GetPrivateProfileStringW(L"probe", L"mode", L"probe", value, ARRAY_COUNT(value),
                                 g_configPath) != 0) {
        if (_wcsicmp(value, L"quiet") == 0) {
            g_mode = PROBE_MODE_QUIET;
        } else if (_wcsicmp(value, L"off") == 0) {
            g_mode = PROBE_MODE_OFF;
        } else {
            g_mode = PROBE_MODE_PROBE;
        }
    }
    wchar_t buf[32] = {0};
    if (GetPrivateProfileStringW(L"probe", L"logSends", L"1", buf, ARRAY_COUNT(buf),
                                 g_configPath) != 0) {
        g_logSends = (buf[0] == L'0') ? 0 : 1;
    }
    if (GetPrivateProfileStringW(L"probe", L"control", L"1", buf, ARRAY_COUNT(buf),
                                 g_configPath) != 0) {
        g_control = (buf[0] == L'0') ? 0 : 1;
    }
    if (GetPrivateProfileStringW(L"probe", L"autoRebase", L"1", buf, ARRAY_COUNT(buf),
                                 g_configPath) != 0) {
        g_autoRebase = (buf[0] == L'0') ? 0 : 1;
    }
}

// ---------------------------------------------------------------------------
// SEH-guarded reads. POD only: __try cannot live in a function that needs C++
// object unwinding (C2712).
// ---------------------------------------------------------------------------
struct WindowSnapshot {
    unsigned int win;
    unsigned int partIndex;
    unsigned int ownerId;
    unsigned int flag184;
    unsigned int limit18C;
    unsigned int endurance;
    unsigned int state;
    unsigned int pending;
    unsigned int selBegin;
    unsigned int selEnd;
    unsigned int selCount;
    unsigned int sentCount;
    unsigned int flag438;
    unsigned int valid;
};

bool ReadWindowRaw(void* win, WindowSnapshot* out) {
    __try {
        const unsigned char* p = static_cast<const unsigned char*>(win);
        out->partIndex = *reinterpret_cast<const unsigned int*>(p + kOffPartIndex);
        out->ownerId = *reinterpret_cast<const unsigned int*>(p + kOffOwnerId);
        out->flag184 = *reinterpret_cast<const unsigned int*>(p + kOffFlag184);
        out->limit18C = *reinterpret_cast<const unsigned int*>(p + kOffLimit18C);
        out->endurance = *reinterpret_cast<const unsigned int*>(p + kOffEndurance);
        out->state = *reinterpret_cast<const unsigned int*>(p + kOffState);
        out->pending = *reinterpret_cast<const unsigned int*>(p + kOffPending);
        out->selBegin = *reinterpret_cast<const unsigned int*>(p + kOffSelBegin);
        out->selEnd = *reinterpret_cast<const unsigned int*>(p + kOffSelEnd);
        out->sentCount = *reinterpret_cast<const unsigned int*>(p + kOffSentCount);
        out->flag438 = *reinterpret_cast<const unsigned int*>(p + kOffFlag438);
        return true;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool ReadU8Raw(const void* address, unsigned char* out) {
    __try {
        *out = *reinterpret_cast<const unsigned char*>(address);
        return true;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

WindowSnapshot Snapshot(void* win) {
    WindowSnapshot s;
    memset(&s, 0, sizeof(s));
    s.win = static_cast<unsigned int>(reinterpret_cast<uintptr_t>(win));
    if (!win) {
        return s;
    }
    if (!ReadWindowRaw(win, &s)) {
        return s;
    }
    s.valid = 1;
    const unsigned int begin = s.selBegin;
    const unsigned int end = s.selEnd;
    if (end >= begin && (end - begin) <= 0x10000u && ((end - begin) & 3u) == 0u) {
        s.selCount = (end - begin) / 4u;
    } else {
        s.selCount = 0xFFFFFFFFu;  // marker: implausible / unreadable pair
    }
    return s;
}

unsigned char SafeU8(const void* address) {
    unsigned char v = 0;
    if (!ReadU8Raw(address, &v)) {
        return 0xEE;  // marker: unreadable
    }
    return v;
}

// ---------------------------------------------------------------------------
// Per-thread pass bookkeeping: one loop pass must report how many sends it made.
// ---------------------------------------------------------------------------
struct PassState {
    int depth;
    int isStore;
    unsigned int passIndex;
    unsigned int sends;
    unsigned int refused;
    unsigned int skippedBy7C;
    unsigned int slot0;
    unsigned int slotN;
    DWORD firstSendTick;
    DWORD lastSendTick;
    WindowSnapshot entry;
};

thread_local PassState t_pass;

// ---------------------------------------------------------------------------
// Originals (trampolines). The stack-arg shape MUST match the target's `ret N`:
//   loops         ret    -> 0 stack args -> __fastcall(self, unused)
//   senders       ret 8  -> 2 stack args -> __fastcall(self, unused, a, b)
//   state setter  ret 4  -> 1 stack arg  -> __fastcall(self, unused, state)
// (__fastcall puts the first two parameters in ecx/edx, everything after that
//  lands on the stack in the same order the original expects.)
// ---------------------------------------------------------------------------
typedef void(__fastcall* LoopFn)(void* self, void* unused);
typedef unsigned char(__fastcall* SendFn)(void* self, void* unused, int a, int b);
typedef void(__fastcall* StateSetFn)(void* self, void* unused, int newState);

LoopFn g_orig235Loop = nullptr;
SendFn g_orig235Send = nullptr;
LoopFn g_orig26Loop = nullptr;
SendFn g_orig26Send = nullptr;
StateSetFn g_origStateSet = nullptr;

// ---------------------------------------------------------------------------
// Detours
// ---------------------------------------------------------------------------
void LogEntry(const char* flow, const WindowSnapshot& s) {
    LogLine(L"ENTER %S #%u win=0x%08X part=0x%X sel=%u [0x%08X..0x%08X] "
            L"st=0x%X pend=0x%08X owner=%u endur=%u f184=%u lim18C=0x%X f438=0x%X f434=%u",
            flow, t_pass.passIndex, s.win, s.partIndex, s.selCount, s.selBegin, s.selEnd,
            s.state, s.pending, s.ownerId, s.endurance, s.flag184, s.limit18C,
            s.flag438, s.sentCount);
}

void LogExit(const char* flow, const WindowSnapshot& after) {
    const DWORD span = (t_pass.sends > 1) ? (t_pass.lastSendTick - t_pass.firstSendTick) : 0;
    LogLine(L"EXIT  %S #%u sends=%u refused=%u wouldSkip7C=%u selAtEntry=%u "
            L"firstSlot=%u lastSlot=%u sendSpanMs=%u | after: st=0x%X pend=0x%08X "
            L"f434=%u f438=0x%X endur=%u",
            flow, t_pass.passIndex, t_pass.sends, t_pass.refused, t_pass.skippedBy7C,
            t_pass.entry.selCount,
            t_pass.sends ? t_pass.slot0 : 0xFFFFu,
            t_pass.sends ? t_pass.slotN : 0xFFFFu,
            span, after.state, after.pending, after.sentCount, after.flag438, after.endurance);
}

void __fastcall Detour235Loop(void* self, void* unused) {
    ZeroMemory(&t_pass, sizeof(t_pass));
    t_pass.depth = 1;
    t_pass.isStore = 1;
    t_pass.passIndex = static_cast<unsigned int>(InterlockedIncrement(&g_pass235));
    t_pass.firstSendTick = GetTickCount();
    t_pass.lastSendTick = t_pass.firstSendTick;
    t_pass.entry = Snapshot(self);
    LogEntry("235", t_pass.entry);

    g_orig235Loop(self, unused);

    LogExit("235", Snapshot(self));
    t_pass.depth = 0;
}

void __fastcall Detour26Loop(void* self, void* unused) {
    ZeroMemory(&t_pass, sizeof(t_pass));
    t_pass.depth = 1;
    t_pass.isStore = 0;
    t_pass.passIndex = static_cast<unsigned int>(InterlockedIncrement(&g_pass26));
    t_pass.firstSendTick = GetTickCount();
    t_pass.lastSendTick = t_pass.firstSendTick;
    t_pass.entry = Snapshot(self);
    LogEntry("26", t_pass.entry);

    g_orig26Loop(self, unused);

    LogExit("26", Snapshot(self));
    t_pass.depth = 0;
}

// IMPORTANT: the two senders return bool in AL (`test al,al; je <failure tail>`
// in the caller loop). The detour MUST forward the original's return value, so
// it is declared `unsigned char` -- never `void`.
unsigned char __fastcall Detour235Send(void* self, void* unused, int slot, int ownerID) {
    if (t_pass.depth == 1 && t_pass.isStore == 1) {
        ++t_pass.sends;
        if (t_pass.sends == 1) {
            t_pass.slot0 = static_cast<unsigned int>(slot);
            t_pass.firstSendTick = GetTickCount();
        }
        t_pass.slotN = static_cast<unsigned int>(slot);
        t_pass.lastSendTick = GetTickCount();
    }
    InterlockedIncrement(&g_send235);

    const unsigned char mgrSkip = SafeU8(reinterpret_cast<unsigned char*>(self) + kOffMgrSkip);
    if (mgrSkip != 0) {
        if (t_pass.depth == 1 && t_pass.isStore == 1) {
            ++t_pass.skippedBy7C;
        }
        InterlockedIncrement(&g_skip7C235);
    }
    if (g_logSends) {
        LogLine(L"SEND235 #%u mgr=0x%08X slot=%d owner=%d mgr7C=0x%02X -> native will %S the packet",
                t_pass.sends, static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)),
                slot, ownerID, mgrSkip, mgrSkip ? L"SKIP" : L"emit");
    }

    const unsigned char result = g_orig235Send(self, unused, slot, ownerID);
    if (!result) {
        if (t_pass.depth == 1 && t_pass.isStore == 1) {
            ++t_pass.refused;
        }
        InterlockedIncrement(&g_refused235);
        LogLine(L"SEND235 refused by native (ret=0) slot=%d owner=%d", slot, ownerID);
    }
    return result;
}

unsigned char __fastcall Detour26Send(void* self, void* unused, int slot, int context) {
    if (t_pass.depth == 1 && t_pass.isStore == 0) {
        ++t_pass.sends;
        if (t_pass.sends == 1) {
            t_pass.slot0 = static_cast<unsigned int>(slot);
            t_pass.firstSendTick = GetTickCount();
        }
        t_pass.slotN = static_cast<unsigned int>(slot);
        t_pass.lastSendTick = GetTickCount();
    }
    InterlockedIncrement(&g_send26);
    if (g_logSends) {
        LogLine(L"SEND26  #%u mgr=0x%08X slot=%d ctx=%d",
                t_pass.sends, static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)),
                slot, context);
    }
    const unsigned char result = g_orig26Send(self, unused, slot, context);
    if (!result) {
        if (t_pass.depth == 1 && t_pass.isStore == 0) {
            ++t_pass.refused;
        }
        InterlockedIncrement(&g_refused26);
        LogLine(L"SEND26 refused by native (ret=0) slot=%d ctx=%d", slot, context);
    }
    return result;
}

void __fastcall DetourStateSet(void* self, void* unused, int newState) {
    InterlockedIncrement(&g_stateSetCalls);
    LogLine(L"STATE win=0x%08X -> %d (0x%X)  [loopDepth=%d]",
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)), newState, newState,
            t_pass.depth);
    g_origStateSet(self, unused, newState);
}

// ---------------------------------------------------------------------------
// Hook installation
// ---------------------------------------------------------------------------
struct HookSlot {
    const char* name;
    unsigned int rva;
    const unsigned char* prologue;
    unsigned int length;
    void* detour;
    void** original;
    unsigned char* address;
    volatile LONG installed;
    const char* pattern;      // wildcard fingerprint of the real function
    unsigned int resolvedRva; // where the fingerprint was found in this process
    unsigned int hits;        // how many times the fingerprint occurs
};

HookSlot g_hooks[] = {
    {"op235-loop  sub_2C38840", kRva235Loop, kPro235Loop, kLen235Loop,
     reinterpret_cast<void*>(&Detour235Loop), reinterpret_cast<void**>(&g_orig235Loop), nullptr, 0,
     kPattern235Loop, 0, 0},
    {"op235-send  sub_1F30B80", kRva235Send, kPro235Send, kLen235Send,
     reinterpret_cast<void*>(&Detour235Send), reinterpret_cast<void**>(&g_orig235Send), nullptr, 0,
     kPattern235Send, 0, 0},
    {"op26-loop   sub_2C38260", kRva26Loop, kPro26Loop, kLen26Loop,
     reinterpret_cast<void*>(&Detour26Loop), reinterpret_cast<void**>(&g_orig26Loop), nullptr, 0,
     kPattern26Loop, 0, 0},
    {"op26-send   sub_1F30630", kRva26Send, kPro26Send, kLen26Send,
     reinterpret_cast<void*>(&Detour26Send), reinterpret_cast<void**>(&g_orig26Send), nullptr, 0,
     kPattern26Send, 0, 0},
    {"state-set   sub_2C313A0", kRvaStateSet, kProStateSet, kLenStateSet,
     reinterpret_cast<void*>(&DetourStateSet), reinterpret_cast<void**>(&g_origStateSet), nullptr, 0,
     kPatternStateSet, 0, 0},
};
const unsigned int kControlHookFirst = 2;  // slots 2..3 are the op26 control pair

bool BytesMatch(const unsigned char* p, const unsigned char* want, unsigned int n) {
    return memcmp(p, want, n) == 0;
}

// ---------------------------------------------------------------------------
// Wildcard fingerprint support: "55 8B EC 6A FF 68 ?? ?? ?? ?? 64 A1" -> bytes +
// mask (0xFF = must match, 0x00 = wildcard).
// ---------------------------------------------------------------------------
bool ParsePattern(const char* text, unsigned char* bytes, unsigned char* mask,
                  unsigned int* length, unsigned int capacity) {
    unsigned int n = 0;
    const char* p = text;
    while (*p) {
        while (*p == ' ') {
            ++p;
        }
        if (!*p) {
            break;
        }
        if (n >= capacity) {
            return false;
        }
        if (p[0] == '?' && p[1] == '?') {
            bytes[n] = 0;
            mask[n] = 0;
            p += 2;
        } else {
            auto hex = [](char c) -> int {
                if (c >= '0' && c <= '9') return c - '0';
                if (c >= 'A' && c <= 'F') return c - 'A' + 10;
                if (c >= 'a' && c <= 'f') return c - 'a' + 10;
                return -1;
            };
            const int hi = hex(p[0]);
            const int lo = hex(p[1]);
            if (hi < 0 || lo < 0) {
                return false;
            }
            bytes[n] = static_cast<unsigned char>((hi << 4) | lo);
            mask[n] = 0xFF;
            p += 2;
        }
        ++n;
    }
    *length = n;
    return n > 0;
}

// Scan the module's executable sections for a fingerprint.
// Returns the number of hits; the first `maxOut` hit RVAs go into `outRvas`.
unsigned int ScanModuleForPattern(unsigned int base, const unsigned char* bytes,
                                  const unsigned char* mask, unsigned int length,
                                  unsigned int* outRvas, unsigned int maxOut) {
    const unsigned char* mod = reinterpret_cast<const unsigned char*>(
        static_cast<uintptr_t>(base));
    if (!mod || length < 4) {
        return 0;
    }
    const unsigned char* pe = mod + *reinterpret_cast<const int*>(mod + 0x3C);
    if (memcmp(pe, "PE\0\0", 4) != 0) {
        return 0;
    }
    const unsigned int nsec = *reinterpret_cast<const unsigned short*>(pe + 6);
    const unsigned int optsz = *reinterpret_cast<const unsigned short*>(pe + 20);
    const unsigned char* sec = pe + 24 + optsz;

    unsigned int hits = 0;
    for (unsigned int s = 0; s < nsec; ++s) {
        const unsigned char* hdr = sec + 40 * s;
        const unsigned int vsize = *reinterpret_cast<const unsigned int*>(hdr + 8);
        const unsigned int va = *reinterpret_cast<const unsigned int*>(hdr + 12);
        const unsigned int characteristics = *reinterpret_cast<const unsigned int*>(hdr + 36);
        if ((characteristics & 0x20000000u) == 0 || vsize < length) {  // IMAGE_SCN_MEM_EXECUTE
            continue;
        }
        const unsigned char* p = mod + va;
        const unsigned int last = vsize - length;
        for (unsigned int i = 0; i <= last; ++i) {
            bool ok = true;
            for (unsigned int k = 0; k < length; ++k) {
                if (mask[k] && p[i + k] != bytes[k]) {
                    ok = false;
                    break;
                }
            }
            if (ok) {
                if (hits < maxOut) {
                    outRvas[hits] = va + i;
                }
                ++hits;
                if (hits > 64) {  // runaway match: stop early, it is not unique
                    return hits;
                }
            }
        }
    }
    return hits;
}

void LogModuleIdentity() {
    wchar_t path[MAX_PATH * 2] = {0};
    if (GetModuleFileNameW(nullptr, path, ARRAY_COUNT(path)) == 0) {
        wcscpy_s(path, ARRAY_COUNT(path), L"<GetModuleFileNameW failed>");
    }
    const unsigned char* mod = reinterpret_cast<const unsigned char*>(GetModuleHandleW(nullptr));
    LogLine(L"EXE path : %s", path);
    if (!mod) {
        return;
    }
    const unsigned char* pe = mod + *reinterpret_cast<const int*>(mod + 0x3C);
    if (memcmp(pe, "PE\0\0", 4) != 0) {
        LogLine(L"EXE header: not a PE at module base");
        return;
    }
    const unsigned int stamp = *reinterpret_cast<const unsigned int*>(pe + 8);
    const unsigned int nsec = *reinterpret_cast<const unsigned short*>(pe + 6);
    const unsigned int entry = *reinterpret_cast<const unsigned int*>(pe + 24 + 16);
    const unsigned int imageSize = *reinterpret_cast<const unsigned int*>(pe + 24 + 56);
    const unsigned int checksum = *reinterpret_cast<const unsigned int*>(pe + 24 + 64);
    LogLine(L"EXE PE   : timestamp=0x%08X entry=0x%08X imageSize=0x%X checksum=0x%08X sections=%u",
            stamp, entry, imageSize, checksum, nsec);
}

// The section table exactly as ScanModuleForPattern sees it. If the scanner ever
// reports a hit, this proves which memory it actually searched.
void LogModuleSections(unsigned int base) {
    const unsigned char* mod = reinterpret_cast<const unsigned char*>(
        static_cast<uintptr_t>(base));
    if (!mod) {
        return;
    }
    const unsigned char* pe = mod + *reinterpret_cast<const int*>(mod + 0x3C);
    if (memcmp(pe, "PE\0\0", 4) != 0) {
        return;
    }
    const unsigned int nsec = *reinterpret_cast<const unsigned short*>(pe + 6);
    const unsigned int optsz = *reinterpret_cast<const unsigned short*>(pe + 20);
    const unsigned char* sec = pe + 24 + optsz;
    LogLine(L"scanner view: optsz=0x%X nsec=%u", optsz, nsec);
    for (unsigned int s = 0; s < nsec; ++s) {
        const unsigned char* hdr = sec + 40 * s;
        char name[9] = {0};
        memcpy(name, hdr, 8);
        const unsigned int vsize = *reinterpret_cast<const unsigned int*>(hdr + 8);
        const unsigned int va = *reinterpret_cast<const unsigned int*>(hdr + 12);
        const unsigned int rawSize = *reinterpret_cast<const unsigned int*>(hdr + 16);
        const unsigned int characteristics = *reinterpret_cast<const unsigned int*>(hdr + 36);
        LogLine(L"  sec %S va=0x%X vsize=0x%X raw=0x%X chars=0x%08X exec=%S",
                name, va, vsize, rawSize, characteristics,
                (characteristics & 0x20000000u) ? L"yes" : L"no");
    }
}

// Read bytes through the SAME pointer arithmetic the scanner uses, so a
// disagreement between "scan found the pattern" and "dump shows other bytes"
// can be attributed precisely.
void LogBytesAtRva(unsigned int base, unsigned int rva, const char* label) {
    const unsigned char* p = reinterpret_cast<const unsigned char*>(
        static_cast<uintptr_t>(base)) + rva;
    LogLine(L"scan-view @0x%06X %S : %02X %02X %02X %02X %02X %02X %02X %02X",
            rva, label, p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7]);
}

// Dump 48 bytes at each declared target RVA so the real bytes can be fingerprinted
// even when nothing matches.
void DumpTargetBytes(unsigned int hookCount) {
    for (unsigned int i = 0; i < hookCount; ++i) {
        const unsigned char* p = g_hooks[i].address;
        LogLine(L"@0x%06X %S : %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X "
                L"%02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X %02X "
                L"%02X %02X %02X %02X %02X %02X %02X %02X",
                g_hooks[i].rva, g_hooks[i].name,
                p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7],
                p[8], p[9], p[10], p[11], p[12], p[13], p[14], p[15],
                p[16], p[17], p[18], p[19], p[20], p[21], p[22], p[23],
                p[24], p[25], p[26], p[27], p[28], p[29], p[30], p[31]);
    }
}

bool InstallOne(HookSlot* slot) {
    unsigned char* target = slot->address;
    unsigned char* trampoline = static_cast<unsigned char*>(
        VirtualAlloc(nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        LogLine(L"install FAILED %S: VirtualAlloc trampoline error=%lu", slot->name, GetLastError());
        return false;
    }
    memcpy(trampoline, slot->prologue, slot->length);
    trampoline[slot->length] = 0xE9;
    *reinterpret_cast<int*>(trampoline + slot->length + 1) = static_cast<int>(
        reinterpret_cast<intptr_t>(target + slot->length) -
        reinterpret_cast<intptr_t>(trampoline + slot->length + 5));
    *slot->original = trampoline;

    unsigned char patch[8];
    patch[0] = 0xE9;
    *reinterpret_cast<int*>(patch + 1) =
        static_cast<int>(reinterpret_cast<intptr_t>(slot->detour) -
                         reinterpret_cast<intptr_t>(target + 5));
    for (unsigned int i = 5; i < slot->length; ++i) {
        patch[i] = 0x90;  // pad the tail of a 6-byte prologue
    }

    DWORD oldProtect = 0;
    if (!VirtualProtect(target, slot->length, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine(L"install FAILED %S: VirtualProtect error=%lu", slot->name, GetLastError());
        return false;
    }
    // Write bytes 1..n-1 first, the opcode last, so the entry byte flips in one go.
    memcpy(target + 1, patch + 1, slot->length - 1);
    FlushInstructionCache(GetCurrentProcess(), target, slot->length);
    memcpy(target, patch, 1);
    FlushInstructionCache(GetCurrentProcess(), target, slot->length);
    DWORD ignored = 0;
    VirtualProtect(target, slot->length, oldProtect, &ignored);

    InterlockedExchange(&slot->installed, 1);
    LogLine(L"hook installed %S target=0x%08X len=%u trampoline=0x%08X detour=0x%08X",
            slot->name, static_cast<unsigned int>(reinterpret_cast<uintptr_t>(target)),
            slot->length, static_cast<unsigned int>(reinterpret_cast<uintptr_t>(trampoline)),
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(slot->detour)));
    // Prove the patch actually stuck (a protector can revert it within ms).
    LogLine(L"  patched bytes now: %02X %02X %02X %02X %02X %02X",
            target[0], target[1], target[2], target[3], target[4], target[5]);
    return true;
}

DWORD WINAPI HeartbeatThread(LPVOID) {
    LONG lastPass235 = -1, lastPass26 = -1, lastSend235 = -1, lastSend26 = -1;
    LONG lastState = -1;
    for (int tick = 0; tick < 3600; ++tick) {  // ~2 hours
        Sleep(10000);
        const LONG p235 = g_pass235, p26 = g_pass26;
        const LONG s235 = g_send235, s26 = g_send26;
        const LONG st = g_stateSetCalls;
        if (p235 == lastPass235 && p26 == lastPass26 && s235 == lastSend235 &&
            s26 == lastSend26 && st == lastState) {
            continue;
        }
        lastPass235 = p235;
        lastPass26 = p26;
        lastSend235 = s235;
        lastSend26 = s26;
        lastState = st;
        LogLine(L"HEARTBEAT pass235=%ld send235=%ld refused235=%ld skip7C=%ld | "
                L"pass26=%ld send26=%ld refused26=%ld | stateSet=%ld",
                p235, s235, g_refused235, g_skip7C235, p26, s26, g_refused26, st);
    }
    return 0;
}

DWORD WINAPI WorkerThread(LPVOID) {
    InitializeCriticalSection(&g_logLock);
    BuildSiblingPath(L"StoreDisjointProbe.log", g_logPath, ARRAY_COUNT(g_logPath));
    BuildSiblingPath(L"StoreDisjointProbe.ini", g_configPath, ARRAY_COUNT(g_configPath));
    g_logReady = true;
    LoadConfig();

    HMODULE exe = GetModuleHandleW(nullptr);
    const unsigned int base = static_cast<unsigned int>(reinterpret_cast<uintptr_t>(exe));
    LogLine(L"StoreDisjointProbe loaded: process=%lu module=0x%08X mode=%ld logSends=%ld "
            L"control=%ld autoRebase=%ld",
            GetCurrentProcessId(), base, g_mode, g_logSends, g_control, g_autoRebase);
    // Which client is this, exactly? Without this line a prologue mismatch is
    // impossible to attribute.
    LogModuleIdentity();
    LogModuleSections(base);

    if (base != kImageBase) {
        LogLine(L"module base 0x%08X != expected 0x%08X; relocated by delta=0x%X",
                base, kImageBase, base - kImageBase);
    }
    // Address arithmetic: kRvaXXX are RVAs, so the live address is base + RVA.
    // (Getting this wrong reads unrelated memory and makes every prologue check
    // fail while the fingerprint scan - which does `mod + va + i` - succeeds.)
    const intptr_t imageBase = static_cast<intptr_t>(base);
    const unsigned int hookCount = static_cast<unsigned int>(ARRAY_COUNT(g_hooks));
    for (unsigned int i = 0; i < hookCount; ++i) {
        g_hooks[i].address = reinterpret_cast<unsigned char*>(
            imageBase + static_cast<intptr_t>(g_hooks[i].rva));
        g_hooks[i].resolvedRva = g_hooks[i].rva;
    }

    // Raw dump at the declared addresses: even with no match at all this is the
    // fingerprint needed to re-derive the addresses offline.
    DumpTargetBytes(hookCount);
    LogLine(L"wanted prologues: op235-loop=%02X%02X%02X%02X%02X op235-send=%02X%02X%02X%02X%02X%02X "
            L"op26-loop=%02X%02X%02X%02X%02X op26-send=%02X%02X%02X%02X%02X%02X "
            L"state-set=%02X%02X%02X%02X%02X%02X",
            kPro235Loop[0], kPro235Loop[1], kPro235Loop[2], kPro235Loop[3], kPro235Loop[4],
            kPro235Send[0], kPro235Send[1], kPro235Send[2], kPro235Send[3], kPro235Send[4], kPro235Send[5],
            kPro26Loop[0], kPro26Loop[1], kPro26Loop[2], kPro26Loop[3], kPro26Loop[4],
            kPro26Send[0], kPro26Send[1], kPro26Send[2], kPro26Send[3], kPro26Send[4], kPro26Send[5],
            kProStateSet[0], kProStateSet[1], kProStateSet[2], kProStateSet[3], kProStateSet[4], kProStateSet[5]);

    if (g_mode == PROBE_MODE_OFF) {
        LogLine(L"mode=off: no hook installed, nothing recorded");
        return 0;
    }

    // Relocate by fingerprint, and WAIT for the protected client to expose .text.
    //
    // Measured behaviour (2026-09-23, DNF.exe sha256 8a7af614...):
    //   * at injection time the bytes at the declared RVAs are NOT the real code;
    //   * milliseconds later a fingerprint scan found the real code at the SAME
    //     RVA -- while two other targets scanned as "hit=0";
    //   * 120 s later the same address was back to the non-matching bytes.
    // Different code pages are exposed independently, so this client decrypts
    // code pages on demand. Consequence: a single read proves nothing, and a
    // short wait can miss entirely (those functions only run when the player
    // opens the disassembly machine). Keep polling until each target shows up.
    const int kWaitTicks = 150;     // tight phase: 150 * 200ms = 30 s
    const int kScanEvery = 1;       // scan every tick while it is still cheap
    bool resolved[16] = {false};
    unsigned char lastByte[16] = {0};
    for (unsigned int i = 0; i < hookCount; ++i) {
        lastByte[i] = 0xFF;
    }
    const bool wanted[16] = {true, true, true, true, true};
    unsigned int installedCount = 0;

    int waited = 0;
    for (; waited < kWaitTicks; ++waited) {
        bool allDone = true;
        const bool doScan = (waited % kScanEvery) == 0;
        for (unsigned int i = 0; i < hookCount; ++i) {
            if (!wanted[i] || (i >= kControlHookFirst && !g_control)) {
                continue;
            }
            if (resolved[i]) {
                continue;
            }
            allDone = false;
            const unsigned char first = g_hooks[i].address[0];
            if (first != lastByte[i]) {
                LogLine(L"@0x%06X %S now starts with 0x%02X (was 0x%02X) after %d ms",
                        g_hooks[i].rva, g_hooks[i].name, first, lastByte[i], waited * 200);
                lastByte[i] = first;
            }
            if (BytesMatch(g_hooks[i].address, g_hooks[i].prologue, g_hooks[i].length)) {
                resolved[i] = true;
                LogLine(L"resolved %S at declared RVA 0x%06X after %d ms (prologue matched)",
                        g_hooks[i].name, g_hooks[i].rva, waited * 200);
            } else if (doScan) {
                unsigned char bytes[64] = {0};
                unsigned char mask[64] = {0};
                unsigned int length = 0;
                if (ParsePattern(g_hooks[i].pattern, bytes, mask, &length, ARRAY_COUNT(bytes))) {
                    unsigned int found[8] = {0};
                    const unsigned int hits =
                        ScanModuleForPattern(base, bytes, mask, length, found, 8);
                    g_hooks[i].hits = hits;
                    if (hits == 1) {
                        g_hooks[i].resolvedRva = found[0];
                        const bool moved = found[0] != g_hooks[i].rva;
                        if (moved && g_autoRebase) {
                            g_hooks[i].address = reinterpret_cast<unsigned char*>(
                                imageBase + static_cast<intptr_t>(found[0]));
                        }
                        resolved[i] = true;
                        LogLine(L"resolved %S by fingerprint len=%u at 0x%06X (%s declared 0x%06X) "
                                L"after %d ms%s",
                                g_hooks[i].name, length, found[0],
                                moved ? L"MOVED from" : L"same as", g_hooks[i].rva, waited * 200,
                                (moved && !g_autoRebase)
                                    ? L" [autoRebase=0: keeping declared RVA]" : L"");
                        LogBytesAtRva(base, found[0], g_hooks[i].name);
                    }
                }
            }
            // Install the moment a target is located: never gate one hook on
            // another target appearing (the op26 control pair may stay encrypted
            // until the NPC machine is actually opened).
            if (resolved[i]) {
                if (!BytesMatch(g_hooks[i].address, g_hooks[i].prologue, g_hooks[i].length)) {
                    LogLine(L"NOT installing %S: prologue re-check failed at 0x%08X "
                            L"(%02X %02X %02X %02X %02X %02X) - will keep looking",
                            g_hooks[i].name,
                            static_cast<unsigned int>(
                                reinterpret_cast<uintptr_t>(g_hooks[i].address)),
                            g_hooks[i].address[0], g_hooks[i].address[1], g_hooks[i].address[2],
                            g_hooks[i].address[3], g_hooks[i].address[4], g_hooks[i].address[5]);
                    resolved[i] = false;
                } else if (InstallOne(&g_hooks[i])) {
                    ++installedCount;
                    InterlockedExchange(&g_installed, 1);
                    LogLine(L"live: %u hook(s) installed", installedCount);
                }
            }
        }
        if (allDone) {
            break;
        }
        Sleep(200);
    }

    unsigned int installable = 0;
    for (unsigned int i = 0; i < hookCount; ++i) {
        if (!wanted[i] || (i >= kControlHookFirst && !g_control)) {
            continue;
        }
        if (resolved[i]) {
            ++installable;
            continue;
        }
        LogLine(L"UNRESOLVED %S declared=0x%06X used=0x%06X fingerprintHits=%u "
                L"at 0x%08X: %02X %02X %02X %02X %02X %02X (wanted %02X %02X %02X %02X %02X %02X)",
                g_hooks[i].name, g_hooks[i].rva, g_hooks[i].resolvedRva, g_hooks[i].hits,
                static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_hooks[i].address)),
                g_hooks[i].address[0], g_hooks[i].address[1], g_hooks[i].address[2],
                g_hooks[i].address[3], g_hooks[i].address[4], g_hooks[i].address[5],
                g_hooks[i].prologue[0], g_hooks[i].prologue[1], g_hooks[i].prologue[2],
                g_hooks[i].prologue[3], g_hooks[i].prologue[4], g_hooks[i].prologue[5]);
    }
    LogLine(L"resolution pass done after %d ms: %u/%u targets installable",
            waited * 200, installable, g_control ? hookCount : kControlHookFirst);

    {
        HANDLE beat = CreateThread(nullptr, 0, HeartbeatThread, nullptr, 0, nullptr);
        if (beat) {
            CloseHandle(beat);
        }
    }

    LogLine(L"supervisor up: installing each target as soon as its code page is exposed, "
            L"and re-installing any hook that reverts (up to 3 hours)");

    // The protected client re-encrypts/relocates code pages; re-install anything
    // that reverts. Same mitigation as store_standin_fix, but long enough to
    // cover a whole play session, and it also keeps trying targets that were not
    // exposed yet (e.g. the op26 control pair only decrypts once the NPC machine
    // is actually opened).
    int unresolvedLogged = 0;
    for (int guard = 0; guard < 5400; ++guard) {  // 5400 * 2s = 3 hours
        Sleep(2000);
        for (unsigned int i = 0; i < hookCount; ++i) {
            if (!wanted[i] || (i >= kControlHookFirst && !g_control)) {
                continue;
            }
            // Still not located (its code page was never exposed): keep trying.
            if (!g_hooks[i].installed) {
                unsigned char bytes[64] = {0};
                unsigned char mask[64] = {0};
                unsigned int length = 0;
                bool located = BytesMatch(g_hooks[i].address, g_hooks[i].prologue,
                                          g_hooks[i].length);
                if (!located &&
                    ParsePattern(g_hooks[i].pattern, bytes, mask, &length, ARRAY_COUNT(bytes))) {
                    unsigned int found[8] = {0};
                    const unsigned int hits =
                        ScanModuleForPattern(base, bytes, mask, length, found, 8);
                    g_hooks[i].hits = hits;
                    if (hits == 1) {
                        g_hooks[i].resolvedRva = found[0];
                        if (found[0] != g_hooks[i].rva && g_autoRebase) {
                            g_hooks[i].address = reinterpret_cast<unsigned char*>(
                                imageBase + static_cast<intptr_t>(found[0]));
                        }
                        located = true;
                        LogLine(L"late resolve %S at 0x%06X after %d s (fingerprint hit=1)",
                                g_hooks[i].name, found[0], guard * 2);
                    }
                }
                if (located && BytesMatch(g_hooks[i].address, g_hooks[i].prologue,
                                          g_hooks[i].length)) {
                    if (InstallOne(&g_hooks[i])) {
                        LogLine(L"live: %S installed late after %d s", g_hooks[i].name,
                                guard * 2);
                    }
                } else if (++unresolvedLogged % 15 == 0) {
                    LogLine(L"still waiting %S at 0x%08X: %02X %02X %02X %02X %02X %02X "
                            L"(fingerprintHits=%u) after %d s -- open that machine's UI to "
                            L"expose its code page",
                            g_hooks[i].name,
                            static_cast<unsigned int>(
                                reinterpret_cast<uintptr_t>(g_hooks[i].address)),
                            g_hooks[i].address[0], g_hooks[i].address[1], g_hooks[i].address[2],
                            g_hooks[i].address[3], g_hooks[i].address[4], g_hooks[i].address[5],
                            g_hooks[i].hits, guard * 2);
                }
                continue;
            }
            // Installed: re-install whenever the protector puts the page back.
            if (g_hooks[i].address[0] == 0xE9) {
                continue;
            }
            LogLine(L"guard: hook %S reverted (0x%02X) at 0x%08X, reinstalling",
                    g_hooks[i].name, g_hooks[i].address[0],
                    static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_hooks[i].address)));
            InstallOne(&g_hooks[i]);
        }
    }
    LogLine(L"guard finished: pass235=%ld send235=%ld pass26=%ld send26=%ld stateSet=%ld",
            g_pass235, g_send235, g_pass26, g_send26, g_stateSetCalls);
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
