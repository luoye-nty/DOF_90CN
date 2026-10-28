// StoreDisjointBatch.dll -- standalone client-side batching for the personal
// store (expert-job / 副职业) disassembly machine's "disassemble all" flow.
//
// INDEPENDENT MODULE. It is deliberately NOT part of 90CN.dll: prove it on a
// live client first, merge only afterwards.
//
// ---------------------------------------------------------------------------
// What it does
//
// The store machine's "disassemble all" already sends every selected item
// back-to-back in one pass, without waiting for replies -- measured live
// 2026-09-23 with the read-only probe in client-patch\probe_store_disjoint:
//
//   ENTER 235 #363  part=0xF  sel=8  st=0x1  pend=0x00000000  owner=5
//   SEND235 #1..8   slot=9,10,11,12,13,14,15,16   owner=5   mgr7C=0x00  (same ms)
//   EXIT  235 #903  sends=8  refused=0  selAtEntry=8  sendSpanMs=0
//                   | after: st=0x2  f434=8
//
// So the client is not the bottleneck: the server gets N separate commands, and
// each one rebuilds 5 container lists, posts a gold packet to the shop owner and
// republishes op205. This module collapses those N commands into ONE:
//
//   * hook the loop   sub_2C38840 (RVA 0x02C38840) -- arms a capture when the
//     selection holds >= 2 items;
//   * hook the sender  sub_1F30B80 (RVA 0x01F30B80) -- records (slot, ownerID)
//     and emits nothing while the capture is armed, returning TRUE so the loop
//     keeps advancing (returning false sends the loop into its failure tail);
//   * after the loop, if the captured count equals the selection count, emit ONE
//     marked op235 through the client's own writer:
//
//       u16 235 | u16 ownerID | u16 0xFFFF | u8 listType | u32 "B35\x01"
//       | u8 count | u16 slots[count]
//
//     Otherwise (any surprise at all) replay the captured sends through the
//     original sender, which is exactly the native behaviour.
//
// The server side (go-server .../expert_job_store.go) recognises that envelope,
// commits every slot in ONE transaction and answers with ONE merged reply plus
// an authoritative per-slot op14 refresh. Envelopes are additive: a client
// without this DLL keeps sending ordinary 5-byte requests and is unaffected.
//
// ---------------------------------------------------------------------------
// Modes (StoreDisjointBatch.ini next to the DLL, default batch):
//   mode=log     hook everything, log what it would batch, but let every packet
//                go out normally. Use this first to confirm the slots.
//   mode=batch   suppress the per-item sends and emit the single batch packet.
//   mode=pass    install nothing.
//
// Log: <game dir>\StoreDisjointBatch.log

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
// Hook targets (RVAs; the live address is module base + RVA).
// ---------------------------------------------------------------------------
const unsigned char kPro235Loop[] = {0x55, 0x8B, 0xEC, 0x6A, 0xFF,      // push ebp; mov ebp,esp; push -1
                                     0x68, 0xB8, 0x0A, 0xED, 0x03};     // push <SEH handler>
const unsigned char kPro235Send[] = {0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x18, // push ebp; mov ebp,esp; sub esp,0x18
                                     0x56, 0x8B, 0xF1};                  // push esi; mov esi,ecx
const unsigned int kRva235Loop = 0x02C38840u;
const unsigned int kRva235Send = 0x01F30B80u;
const unsigned int kLen235Loop = 5;  // whole instructions: 1+2+2
const unsigned int kLen235Send = 6;  // whole instructions: 1+2+3

// Native packet writer (same helpers 90CN.dll uses for the op26 batch).
//
// 2026-09-23, verified against a disassembly of this exact build. The writer is
// a typed stream plus a separate body counter, and the two roles are easy to
// confuse:
//
//   * kRvaWriterPacketType (sub_3454EB0) BEGINS a packet. It appends a 13-byte
//     header record holding the opcode and RESETS the body byte counter
//     [writer+0x2BCC2C] to 0. It returns EAX=1 on success and 0x80000054 when
//     the writer is not ready. Calling it exactly once per packet is mandatory:
//     it is what puts the opcode (235) into the packet header, and it is what
//     restarts the body counter from zero.
//   * kRvaWriterU8/U16/U32 append ONE raw body value and add 1/2/4 to
//     [writer+0x2BCC2C]. They return the append helper's value -- NOT 0 -- on
//     success and only 0x80000054 when the writer is not ready.
//
// The native op235 sender proves the layout: writeU16(0xEB) + writeI16(ownerID)
// + writeI16(slot) + writeU8(list) + flush() puts a 5-byte body
// (05 00 11 00 00) on the wire, so the opcode never appears in the body. flush
// (sub_30566F0) then packs [header record .. +[+0x2BCC2C] bytes] and sends it.
//
// The first version of this DLL bound 0x03054EB0 as "writeU16" and used it for
// every value, so every write reset the body counter to 0 and the client put an
// op235 with body_len=0 on the wire -- captured live as
// RECV ... type=235 body_len=0 len=13 hex=01eb000d000000a6482bce3f00.
const unsigned int kRvaWriterValue = 0x03054640u; // sub_3454640: mov eax,[0x52EA4BC]; ret
const unsigned int kRvaWriterPacketType = 0x03054EB0u; // sub_3454EB0: begin packet (opcode)
const unsigned int kRvaWriterU8 = 0x03054FC0u;  // sub_3454FC0: add [ecx+0x2BCC2C],1
const unsigned int kRvaWriterU16 = 0x03054FF0u; // sub_3454FF0: add [ecx+0x2BCC2C],2
const unsigned int kRvaWriterU32 = 0x03055020u; // sub_3455020: add [ecx+0x2BCC2C],4
const unsigned int kRvaWriterFlush = 0x030566F0u;

const unsigned short kStoreDisjointOpcode = 235;
// "B35\x01" little-endian, mirroring 90CN's "B26\x01" for op26.
const unsigned int kBatchMarker = 0x01353342u;
const int kBatchMaxItems = 127;
const unsigned short kBatchSentinel = 0xFFFFu;
const unsigned char kMainInventoryListType = 0;

// Window fields (read/captured by the read-only probe).
const unsigned int kOffOwnerId = 0x180;
const unsigned int kOffState = 0x1B8;    // 0 idle / 1 may-send / 2 sent-waiting
const unsigned int kOffPending = 0x1EC;  // non-zero => single-item path
const unsigned int kOffSelBegin = 0x424;
const unsigned int kOffSelEnd = 0x428;
const unsigned int kOffMgrSkip = 0x07C;  // native sender skips the packet when non-zero
// Writer bookkeeping (from the probe's trace of the write primitives): each
// successful write does `add dword [writer+0x2BCC2C], size`, so this counter is
// the assembled byte count and is the only way to tell whether our writes landed.
const unsigned int kOffWriterPending = 0x2BCC2C;
const unsigned int kOffWriterReadyA = 0x8;
const unsigned int kOffWriterReadyB = 0x2BD068;
const unsigned int kWriterGateRefused = 0x80000054u;

enum Mode { MODE_LOG = 0, MODE_BATCH = 1, MODE_PASS = 2 };

volatile LONG g_mode = MODE_BATCH;
volatile LONG g_installed = 0;
volatile LONG g_loopCalls = 0;
volatile LONG g_batchesQueued = 0;
volatile LONG g_replays = 0;
volatile LONG g_suppressedSends = 0;
volatile LONG g_refusedSends = 0;

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
    if (GetPrivateProfileStringW(L"batch", L"mode", L"batch", value, ARRAY_COUNT(value),
                                 g_configPath) != 0) {
        if (_wcsicmp(value, L"log") == 0) {
            g_mode = MODE_LOG;
        } else if (_wcsicmp(value, L"pass") == 0) {
            g_mode = MODE_PASS;
        } else {
            g_mode = MODE_BATCH;
        }
    }
}

// ---------------------------------------------------------------------------
// SEH-guarded reads (POD only: __try cannot live where C++ objects unwind).
// ---------------------------------------------------------------------------
bool ReadWindowRaw(void* win, unsigned int* ownerId, unsigned int* state, unsigned int* pending,
                   unsigned int* selBegin, unsigned int* selEnd, unsigned int* flag184,
                   unsigned int* limit18C, unsigned int* partIndex) {
    __try {
        const unsigned char* p = static_cast<const unsigned char*>(win);
        *ownerId = *reinterpret_cast<const unsigned int*>(p + kOffOwnerId);
        *state = *reinterpret_cast<const unsigned int*>(p + kOffState);
        *pending = *reinterpret_cast<const unsigned int*>(p + kOffPending);
        *selBegin = *reinterpret_cast<const unsigned int*>(p + kOffSelBegin);
        *selEnd = *reinterpret_cast<const unsigned int*>(p + kOffSelEnd);
        *flag184 = *reinterpret_cast<const unsigned int*>(p + 0x184);
        *limit18C = *reinterpret_cast<const unsigned int*>(p + 0x18C);
        *partIndex = *reinterpret_cast<const unsigned int*>(p + 0x1B0);
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

unsigned char SafeU8(const void* address) {
    unsigned char v = 0;
    if (!ReadU8Raw(address, &v)) {
        return 0xEE;
    }
    return v;
}

// ---------------------------------------------------------------------------
// Per-thread capture: one loop pass must collect every send it made.
// ---------------------------------------------------------------------------
struct Capture {
    bool active;        // this thread is inside a hooked loop pass
    bool suppress;      // drop the native per-item packet
    bool fallback;      // something unexpected happened -> replay instead
    int expected;       // selection count read at loop entry
    int count;          // slots recorded
    int suppressed;     // slots actually dropped (what a replay must re-send)
    void* sender;       // the list manager the sends went through
    unsigned int ownerId;
    unsigned short slots[kBatchMaxItems];
};

thread_local Capture g_capture;

// ---------------------------------------------------------------------------
// Originals (trampolines). Stack-arg shape must match the target's `ret N`:
//   loop   ret   0 stack args -> __fastcall(self, unused)
//   sender ret 8 2 stack args -> __fastcall(self, unused, slot, ownerID)
// ---------------------------------------------------------------------------
typedef void(__fastcall* LoopFn)(void* self, void* unused);
typedef unsigned char(__fastcall* SendFn)(void* self, void* unused, int slot, int ownerID);

LoopFn g_orig235Loop = nullptr;
SendFn g_orig235Send = nullptr;

// Native writer entry points (filled at install time).
unsigned int g_writerValueAddress = 0;
unsigned int g_writerPacketTypeAddress = 0;
unsigned int g_writerU16Address = 0;
unsigned int g_writerU8Address = 0;
unsigned int g_writerU32Address = 0;
unsigned int g_writerFlushAddress = 0;

// Exactly the typedefs 90CN.cpp uses for the same helpers. The begin-packet call
// returns 1 on success and 0x80000054 when the writer is not ready; the body
// writers return the append helper's value and only 0x80000054 on refusal, so a
// successful body write is NOT necessarily 0.
typedef void* (__cdecl* UpperWriterValueFn)();
typedef unsigned int (__thiscall* UpperWriterScalarFn)(void*, unsigned int);
typedef void (__cdecl* UpperWriterFlushFn)();

// Read through the writer object with SEH protection.
unsigned int SafeU32(const void* address) {
    __try {
        return *reinterpret_cast<const unsigned int*>(address);
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return 0xFFFFFFFFu;
    }
}

// Emit the batch command through the client's own writer, so framing, sequence
// numbers and the send ring stay entirely native.
//
// Sequence, mirroring the native per-item op235 sender exactly:
//     beginPacket(235)            -> opcode into the header record, body counter = 0
//     writeU16(ownerID)
//     writeU16(0xFFFF)            -> the batch sentinel the server looks for
//     writeU8(listType)
//     writeU32("B35\x01")         -> marker, mirrors 90CN's "B26\x01" for op26
//     writeU8(count)
//     writeU16(slot) * count
//     flush()
// Body size = 2+2+1+4+1+2*count = 10 + 2*count, which is exactly what
// parseCurrentExpertJobStoreDisjointBatchRequest expects.
//
// Safety: the writer's own body counter [writer+0x2BCC2C] is reset by
// beginPacket and incremented by every body write, so reading it after the last
// write is an exact check that all bytes landed. On any failure nothing is
// flushed and the caller replays the suppressed sends natively -- a wrong batch
// must never cost the player their disassembly.
bool SendBatchCommand(const Capture& capture) {
    if (!g_writerValueAddress || !capture.sender || capture.count < 2 ||
        capture.count > kBatchMaxItems || capture.count != capture.expected) {
        return false;
    }
    const unsigned int expectedBytes =
        2u + 2u + 1u + 4u + 1u + 2u * static_cast<unsigned int>(capture.count);
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(g_writerValueAddress);
        UpperWriterScalarFn beginPacket =
            reinterpret_cast<UpperWriterScalarFn>(g_writerPacketTypeAddress);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(g_writerU16Address);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(g_writerU8Address);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(g_writerU32Address);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(g_writerFlushAddress);

        void* writer = value();
        if (!writer) {
            LogLine(L"batch aborted: writer is null");
            return false;
        }
        if (SafeU8(static_cast<unsigned char*>(writer) + kOffWriterReadyA) == 0) {
            LogLine(L"batch aborted: writer not ready (readyA=0 readyB=0x%02X) -> replaying natively",
                    SafeU8(static_cast<unsigned char*>(writer) + kOffWriterReadyB));
            return false;
        }

        // Begin the packet: this is what writes the opcode and zeroes the body
        // counter. Its success value is 1, not 0.
        const unsigned int typeStatus = beginPacket(writer, kStoreDisjointOpcode);
        if (typeStatus != 1u) {
            LogLine(L"batch aborted: begin packet failed status=0x%08X -> replaying natively",
                    typeStatus);
            return false;
        }

        unsigned int refused = 0;
        const unsigned int prefix[] = {
            writeU16(writer, capture.ownerId & 0xFFFFu),
            writeU16(writer, kBatchSentinel),
            writeU8(writer, kMainInventoryListType),
            writeU32(writer, kBatchMarker),
            writeU8(writer, static_cast<unsigned int>(capture.count) & 0xFFu),
        };
        for (unsigned int index = 0; index < ARRAY_COUNT(prefix); ++index) {
            if (prefix[index] == kWriterGateRefused) {
                ++refused;
            }
        }
        for (int index = 0; index < capture.count; ++index) {
            if (writeU16(writer, static_cast<unsigned int>(capture.slots[index]) & 0xFFFFu) ==
                kWriterGateRefused) {
                ++refused;
            }
        }

        const unsigned int bodyBytes =
            SafeU32(static_cast<unsigned char*>(writer) + kOffWriterPending);
        if (refused != 0 || bodyBytes != expectedBytes) {
            LogLine(L"batch aborted: body=%u want=%u refusedWrites=%u readyA=0x%02X readyB=0x%02X "
                    L"-> replaying natively",
                    bodyBytes, expectedBytes, refused,
                    SafeU8(static_cast<unsigned char*>(writer) + kOffWriterReadyA),
                    SafeU8(static_cast<unsigned char*>(writer) + kOffWriterReadyB));
            return false;  // caller replays; nothing was flushed
        }
        flush();
        LogLine(L"batch flushed: body=%u owner=%u slots=%d first=%u last=%u",
                bodyBytes, capture.ownerId, capture.count, capture.slots[0],
                capture.slots[capture.count - 1]);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine(L"batch send exception selected=%d code=0x%08X", capture.count, GetExceptionCode());
        return false;
    }
    return true;
}

// ---------------------------------------------------------------------------
// Detours
// ---------------------------------------------------------------------------
// IMPORTANT: the native sender returns bool in AL and the caller loop does
// `test al,al; je <failure tail>`, so the detour MUST forward the original's
// return value (and return TRUE when it suppresses a send). Never `void`.
unsigned char __fastcall Detour235Send(void* self, void* unused, int slot, int ownerID) {
    if (g_capture.active && slot > 0 && slot <= 0x7FFF) {
        if (g_capture.count >= kBatchMaxItems ||
            (g_capture.count > 0 && g_capture.sender != self) ||
            (g_capture.count > 0 && g_capture.ownerId != (static_cast<unsigned int>(ownerID) & 0xFFFFu))) {
            g_capture.fallback = true;
            g_capture.suppress = false;  // from here on, let every packet go out natively
        } else {
            if (g_capture.count == 0) {
                g_capture.sender = self;
                g_capture.ownerId = static_cast<unsigned int>(ownerID) & 0xFFFFu;
            }
            for (int index = 0; index < g_capture.count; ++index) {
                if (g_capture.slots[index] == static_cast<unsigned short>(slot)) {
                    g_capture.fallback = true;
                    g_capture.suppress = false;
                    break;
                }
            }
            if (g_capture.suppress) {
                g_capture.slots[g_capture.count++] = static_cast<unsigned short>(slot);
                ++g_capture.suppressed;
                InterlockedIncrement(&g_suppressedSends);
                return 1;  // TRUE: the loop keeps advancing exactly as with a real send
            }
            if (!g_capture.fallback) {
                g_capture.slots[g_capture.count++] = static_cast<unsigned short>(slot);
            }
        }
    }
    const unsigned char result = g_orig235Send(self, unused, slot, ownerID);
    if (!result) {
        InterlockedIncrement(&g_refusedSends);
    }
    return result;
}

void __fastcall Detour235Loop(void* self, void* unused) {
    if (g_capture.active) {  // re-entrant: never suppress inside a capture
        g_orig235Loop(self, unused);
        return;
    }

    const LONG mode = g_mode;
    if (mode == MODE_PASS) {
        g_orig235Loop(self, unused);
        return;
    }

    unsigned int ownerId = 0, state = 0, pending = 0, selBegin = 0, selEnd = 0;
    unsigned int flag184 = 0, limit18C = 0, partIndex = 0;
    if (!self || !ReadWindowRaw(self, &ownerId, &state, &pending, &selBegin, &selEnd,
                                &flag184, &limit18C, &partIndex)) {
        g_orig235Loop(self, unused);
        return;
    }
    int selected = 0;
    if (selEnd >= selBegin && (selEnd - selBegin) <= 0x10000u && ((selEnd - selBegin) & 3u) == 0u) {
        selected = static_cast<int>((selEnd - selBegin) / 4u);
    }
    // NOTE: kOffMgrSkip lives on the LIST MANAGER, not on the window, so it can
    // only be read from inside the sender detour (where `self` is the manager).
    // Reading it off the window would log an unrelated byte.

    // Only the plain "disassemble all" pass is batchable: an empty selection is
    // just a UI tick, and a non-zero pending pointer is the single-item path.
    // Every other pass (including all log-only passes) runs natively.
    const bool batchable = mode == MODE_BATCH && selected >= 2 &&
                           selected <= kBatchMaxItems && state == 1 && pending == 0;
    const bool observe = batchable || (mode == MODE_LOG && selected >= 2);

    ZeroMemory(&g_capture, sizeof(g_capture));
    g_capture.active = true;
    g_capture.suppress = batchable;
    g_capture.expected = selected;

    g_orig235Loop(self, unused);

    Capture capture = g_capture;
    ZeroMemory(&g_capture, sizeof(g_capture));

    if (!observe) {
        return;
    }
    InterlockedIncrement(&g_loopCalls);

    if (!capture.suppress && capture.suppressed == 0) {
        LogLine(L"log-only pass: win=0x%08X part=0x%X sel=%d state=0x%X pending=0x%08X owner=%u "
                L"f184=%u lim18C=0x%X observedSends=%d mode=%ld",
                static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)), partIndex, selected,
                state, pending, ownerId, flag184, limit18C, capture.count, mode);
        return;
    }

    bool queued = false;
    if (!capture.fallback && capture.count >= 2 && capture.count == selected &&
        capture.suppressed == capture.count) {
        queued = SendBatchCommand(capture);
    }
    if (queued) {
        InterlockedIncrement(&g_batchesQueued);
        LogLine(L"batch queued win=0x%08X part=0x%X sel=%d slots=%d owner=%u",
                static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)), partIndex, selected,
                capture.count, capture.ownerId);
        return;
    }

    // Two very different situations land here and they must be told apart:
    //   * the native loop never called the sender (its own distance / eligibility
    //     gate refused the pass) -> capture.count == 0, nothing was suppressed,
    //     and the player sees exactly what they would see without this DLL;
    //   * our envelope was refused by the writer -> the suppressed sends are put
    //     back on the wire, so the disassembly still happens.
    InterlockedIncrement(&g_replays);
    LogLine(L"batch fallback: replaying %d of %d recorded send(s) (sel=%d fallback=%d "
            L"part=0x%X state=0x%X f184=%u lim18C=0x%X) win=0x%08X",
            capture.suppressed, capture.count, selected, capture.fallback ? 1 : 0,
            partIndex, state, flag184, limit18C,
            static_cast<unsigned int>(reinterpret_cast<uintptr_t>(self)));
    for (int index = 0; index < capture.suppressed && index < capture.count; ++index) {
        g_orig235Send(capture.sender, nullptr, capture.slots[index],
                      static_cast<int>(capture.ownerId));
    }
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
};

HookSlot g_hooks[] = {
    {"op235-loop  sub_2C38840", kRva235Loop, kPro235Loop, kLen235Loop,
     reinterpret_cast<void*>(&Detour235Loop), reinterpret_cast<void**>(&g_orig235Loop), nullptr, 0},
    {"op235-send  sub_1F30B80", kRva235Send, kPro235Send, kLen235Send,
     reinterpret_cast<void*>(&Detour235Send), reinterpret_cast<void**>(&g_orig235Send), nullptr, 0},
};

bool BytesMatch(const unsigned char* p, const unsigned char* want, unsigned int n) {
    return memcmp(p, want, n) == 0;
}

bool InstallOne(HookSlot* slot) {
    unsigned char* target = slot->address;
    unsigned char* trampoline = static_cast<unsigned char*>(
        VirtualAlloc(nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        LogLine(L"install FAILED %S: VirtualAlloc error=%lu", slot->name, GetLastError());
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
        patch[i] = 0x90;
    }

    DWORD oldProtect = 0;
    if (!VirtualProtect(target, slot->length, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine(L"install FAILED %S: VirtualProtect error=%lu", slot->name, GetLastError());
        return false;
    }
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
    LogLine(L"  patched bytes now: %02X %02X %02X %02X %02X %02X",
            target[0], target[1], target[2], target[3], target[4], target[5]);
    return true;
}

DWORD WINAPI WorkerThread(LPVOID) {
    InitializeCriticalSection(&g_logLock);
    BuildSiblingPath(L"StoreDisjointBatch.log", g_logPath, ARRAY_COUNT(g_logPath));
    BuildSiblingPath(L"StoreDisjointBatch.ini", g_configPath, ARRAY_COUNT(g_configPath));
    g_logReady = true;
    LoadConfig();

    HMODULE exe = GetModuleHandleW(nullptr);
    const unsigned int base = static_cast<unsigned int>(reinterpret_cast<uintptr_t>(exe));
    wchar_t path[MAX_PATH * 2] = {0};
    GetModuleFileNameW(nullptr, path, ARRAY_COUNT(path));
    LogLine(L"StoreDisjointBatch loaded: process=%lu module=0x%08X mode=%ld exe=%s",
            GetCurrentProcessId(), base, g_mode, path);

    if (g_mode == MODE_PASS) {
        LogLine(L"mode=pass: nothing installed");
        return 0;
    }

    // RVAs -> live addresses. NOTE: base + RVA (getting this wrong reads
    // unrelated memory and makes every prologue check fail).
    const intptr_t imageBase = static_cast<intptr_t>(base);
    const unsigned int hookCount = static_cast<unsigned int>(ARRAY_COUNT(g_hooks));
    for (unsigned int i = 0; i < hookCount; ++i) {
        g_hooks[i].address = reinterpret_cast<unsigned char*>(
            imageBase + static_cast<intptr_t>(g_hooks[i].rva));
    }
    g_writerValueAddress = static_cast<unsigned int>(imageBase + kRvaWriterValue);
    g_writerPacketTypeAddress = static_cast<unsigned int>(imageBase + kRvaWriterPacketType);
    g_writerU16Address = static_cast<unsigned int>(imageBase + kRvaWriterU16);
    g_writerU8Address = static_cast<unsigned int>(imageBase + kRvaWriterU8);
    g_writerU32Address = static_cast<unsigned int>(imageBase + kRvaWriterU32);
    g_writerFlushAddress = static_cast<unsigned int>(imageBase + kRvaWriterFlush);

    // Wait for the code pages to be readable, then install each hook the moment
    // its own prologue matches -- never gate one hook on another target.
    const int kWaitTicks = 150;
    bool resolved[8] = {false};
    unsigned int installed = 0;
    int waited = 0;
    for (; waited < kWaitTicks; ++waited) {
        bool allDone = true;
        for (unsigned int i = 0; i < hookCount; ++i) {
            if (resolved[i]) {
                continue;
            }
            allDone = false;
            if (BytesMatch(g_hooks[i].address, g_hooks[i].prologue, g_hooks[i].length)) {
                resolved[i] = true;
                LogLine(L"prologue matched %S at 0x%08X after %d ms", g_hooks[i].name,
                        static_cast<unsigned int>(
                            reinterpret_cast<uintptr_t>(g_hooks[i].address)),
                        waited * 200);
                if (InstallOne(&g_hooks[i])) {
                    ++installed;
                }
            }
        }
        if (allDone) {
            break;
        }
        Sleep(200);
    }
    for (unsigned int i = 0; i < hookCount; ++i) {
        if (!resolved[i]) {
            LogLine(L"NOT installed %S: prologue never matched at 0x%08X "
                    L"(%02X %02X %02X %02X %02X %02X)",
                    g_hooks[i].name,
                    static_cast<unsigned int>(reinterpret_cast<uintptr_t>(g_hooks[i].address)),
                    g_hooks[i].address[0], g_hooks[i].address[1], g_hooks[i].address[2],
                    g_hooks[i].address[3], g_hooks[i].address[4], g_hooks[i].address[5]);
        }
    }
    InterlockedExchange(&g_installed, installed ? 1 : 0);
    LogLine(L"install done: %u/%u hooks live (mode=%ld)", installed, hookCount, g_mode);

    // Keep the patch applied if anything reverts it, and keep retrying a target
    // that was not readable yet.
    for (int guard = 0; guard < 5400; ++guard) {  // ~3 hours
        Sleep(2000);
        for (unsigned int i = 0; i < hookCount; ++i) {
            if (g_hooks[i].installed) {
                if (g_hooks[i].address[0] == 0xE9) {
                    continue;
                }
                LogLine(L"guard: hook %S reverted (0x%02X) at 0x%08X, reinstalling", g_hooks[i].name,
                        g_hooks[i].address[0],
                        static_cast<unsigned int>(
                            reinterpret_cast<uintptr_t>(g_hooks[i].address)));
                InstallOne(&g_hooks[i]);
                continue;
            }
            if (BytesMatch(g_hooks[i].address, g_hooks[i].prologue, g_hooks[i].length) &&
                InstallOne(&g_hooks[i])) {
                LogLine(L"late install %S after %d s", g_hooks[i].name, guard * 2);
            }
        }
    }
    LogLine(L"guard finished: loops=%ld batches=%ld replays=%ld suppressed=%ld refused=%ld",
            g_loopCalls, g_batchesQueued, g_replays, g_suppressedSends, g_refusedSends);
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
