// ===========================================================================
// RenameLogNames.dll - standalone module that fills the 角色名 column of the
// 角色名称变更记录 (character name-change record) window.
//
// STATUS (v5): RETIRED FOR NORMAL USE - NOW A COMPATIBILITY SHIM.
//   The row format was re-derived from the client itself on 2026-09-23:
//   0x3457C80, the third-cell reader called by the opcode-733 handler, is a
//   length-prefixed string reader (u32 length + that many GBK bytes), NOT a table
//   lookup. The row already carries the previous name, so the server now sends it
//   inline and this module is not needed. It stays injectable only so that an
//   older server build (names in a trailing block) keeps working; against the
//   current server it detects the native layout and does nothing.
//
// WHY THE COLUMN WAS EMPTY BEFORE
//   An earlier server revision wrote a zero "key" where the length belongs. A zero
//   length makes the reader fall back to the static string resource at 0x4B9958C
//   (.rdata, read-only), which this client build resolves to an empty string. This
//   module then supplied the name by hooking that same resolver.
//
// HOW THIS MODULE DOES IT (legacy bodies only)
//   The old server appended a name block after the rows:
//       u16 nameCount, then per name: u32 byteLength + GBK bytes
//   The stock client never reads past rowCount rows, so the block is inert
//   without this module.
//
//   Hook 1 - entry of the 733 handler (0x015E7D60): walk the packet with the
//     client's own layout and, only if a well-formed trailing name block is
//     present, cache the names. The walk validates every length against the
//     remaining bytes, so a native (v5) body is recognised and ignored instead of
//     being misread as a huge name count.
//   Hook 2 - the string resource resolver (0x34D9B20): the handler calls it once
//     per row for 0x4B9958C and wcsncpy's the result into the third cell. When
//     the argument is that exact resource and names are available, hand back the
//     next cached name instead.
//
//   Everything is observational otherwise: no other packet, no game state and no
//   memory outside those two functions is written. If the block is absent, the
//   hook chain degrades to the stock behaviour (empty column).
//
// Log: <game dir>\RenameLogNames.log
// ===========================================================================

#define WIN32_LEAN_AND_MEAN
#define _CRT_SECURE_NO_WARNINGS
#include <windows.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>

// ---------------------------------------------------------------------------
// Addresses in DNF.exe. The client's ImageBase is 0x400000, so every constant
// below is the VA minus 0x400000 (verified against client-patch\bin\DNF.exe with
// tools\_renamelog_verify_addrs.py):
//
//   VA 0x015E7D60 -> RVA 0x011E7D60   opcode 733 handler (class-0 registry dump
//                                     _bc_RegisterClass0.txt: handler=0x015E7D60)
//   VA 0x034D9B20 -> RVA 0x030D9B20   string resource resolver
//   VA 0x04B9958C -> RVA 0x0479958C   the third cell's static string resource
//   VA 0x052F8500 -> RVA 0x04EF8500   packet cursor
//   VA 0x052F8504 -> RVA 0x04EF8504   packet remaining-byte counter
// ---------------------------------------------------------------------------
static const unsigned int kRenameLogHandlerRva = 0x011E7D60;
static const unsigned int kStringResolveRva = 0x030D9B20;
static const unsigned int kThirdCellResourceRva = 0x0479958C;
static const unsigned int kPacketCursorRva = 0x04EF8500;
static const unsigned int kPacketRemainingRva = 0x04EF8504;

// Maximum rows / names we accept from one packet. The server caps history at 12
// rows, so this is only a sanity bound against a malformed body.
static const unsigned kMaxRows = 64;
static const unsigned kMaxNames = 64;
static const unsigned kMaxNameBytes = 64;
static const unsigned kMaxNameUnits = 30;   // the client copies at most 31 wchars
// The client's third-cell reader (0x3457C80) rejects a length of 0x1F or more and
// consumes no name bytes in that case; the walk below has to mirror that rule or
// its row stride would disagree with the handler's.
static const unsigned kClientNameLengthLimit = 0x1F;

static unsigned char* g_base = nullptr;
static FILE* g_log = nullptr;
static CRITICAL_SECTION g_logLock;
static bool g_logReady = false;

static wchar_t g_names[kMaxNames][kMaxNameUnits + 1] = {};
static unsigned g_nameCount = 0;
static unsigned g_nameIndex = 0;

// 10 bytes: the three whole prologue instructions including the full
// `push <SEH handler>` immediate. Stealing fewer splits that instruction.
static const size_t kHandlerPrologueLen = 10;

struct HookFrame
{
    void* target;
    void* trampoline;
};

static HookFrame g_handlerHook = { nullptr, nullptr };
static HookFrame g_resolveHook = { nullptr, nullptr };

typedef wchar_t* (__cdecl* StringResolveFn)(void* entry);

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------
static void LogLine(const char* format, ...)
{
    if (!g_logReady) return;
    EnterCriticalSection(&g_logLock);
    if (g_log)
    {
        SYSTEMTIME now;
        GetLocalTime(&now);
        fprintf(g_log, "[%04d-%02d-%02d %02d:%02d:%02d.%03d] ",
            now.wYear, now.wMonth, now.wDay,
            now.wHour, now.wMinute, now.wSecond, now.wMilliseconds);
        va_list args;
        va_start(args, format);
        vfprintf(g_log, format, args);
        va_end(args);
        fputc('\n', g_log);
        fflush(g_log);
    }
    LeaveCriticalSection(&g_logLock);
}

static void OpenLog()
{
    char path[MAX_PATH] = { 0 };
    GetModuleFileNameA(nullptr, path, MAX_PATH - 1);
    char* slash = strrchr(path, '\\');
    if (slash) *(slash + 1) = '\0';
    strncat(path, "RenameLogNames.log", MAX_PATH - strlen(path) - 1);
    g_log = fopen(path, "a");
    g_logReady = (g_log != nullptr);
    InitializeCriticalSection(&g_logLock);
}

// ---------------------------------------------------------------------------
// Inline hook: overwrite the prologue with an absolute indirect jump and keep the
// stolen instructions in an executable trampoline that jumps back.
//
// Two rules learned the hard way (live crash 2026-09-23 00:59:07, exception
// 0xC0000096 at trampoline+0x0A):
//   1. `prologueLen` must cover WHOLE instructions. The 733 handler starts
//      `55 | 8B EC | 6A FF | 68 <imm32>` = 1+2+2+5 = 10 bytes; stealing only the
//      leading 6 cut the `push imm32` in half, so the trampoline ran into the
//      immediate bytes and died on an illegal opcode.
//   2. Both jumps use `FF 25 <abs32>` (jmp dword ptr [slot]) rather than a
//      relative `E9`. The module can be injected far from the EXE image, and a
//      rel32 that has to reach across the whole 32-bit space can overflow.
//
// `expectedLen` may be shorter than `prologueLen`: the tail of the stolen region
// can contain a build-specific immediate (the SEH handler address above) that we
// copy verbatim but must not require to be a fixed value.
// ---------------------------------------------------------------------------
static bool InstallInlineHook(void* target, const unsigned char* expected,
                              size_t expectedLen, size_t prologueLen,
                              void* detour, HookFrame* frame)
{
    if (prologueLen < 6 || prologueLen > 16 || expectedLen > prologueLen) return false;
    unsigned char* code = (unsigned char*)target;
    if (memcmp(code, expected, expectedLen) != 0)
    {
        char got[64] = { 0 };
        char want[64] = { 0 };
        for (size_t index = 0; index < expectedLen; ++index)
        {
            _snprintf(got + index * 2, 3, "%02X", code[index]);
            _snprintf(want + index * 2, 3, "%02X", expected[index]);
        }
        LogLine("hook aborted: prologue mismatch at %p got=%s want=%s", target, got, want);
        return false;
    }

    // Layout inside the trampoline page:
    //   0x00              stolen bytes (prologueLen)
    //   prologueLen       jmp dword ptr [0x20]   <-- MUST sit directly after the
    //                                                  stolen bytes: execution
    //                                                  falls through into it
    //   0x20              void* backTarget
    //   0x24              void* detourAddress    (read by the patch in target)
    //
    // Regression note (live crash 2026-09-23 01:02:44, 0xC0000005 at
    // trampoline+0x06): an earlier revision wrote this jump at a fixed offset
    // 0x10 while the stolen window was only 6 bytes, so execution ran off the
    // end of the stolen instructions into the zero padding and executed
    // `add [eax], al` against read-only memory.
    static const size_t kJumpSlotOffset = 0x20;
    static const size_t kDetourSlotOffset = 0x24;
    if (prologueLen + 6 > kJumpSlotOffset)
    {
        LogLine("hook aborted: prologue too long for the trampoline layout");
        return false;
    }

    unsigned char* trampoline = (unsigned char*)VirtualAlloc(
        nullptr, 64, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
    if (!trampoline)
    {
        LogLine("hook aborted: trampoline allocation failed");
        return false;
    }
    memset(trampoline, 0xCC, 64);            // INT3 padding: a fall-through is loud
    memcpy(trampoline, code, prologueLen);

    unsigned char** backSlot = (unsigned char**)(trampoline + kJumpSlotOffset);
    unsigned char** detourSlot = (unsigned char**)(trampoline + kDetourSlotOffset);
    *backSlot = (unsigned char*)target + prologueLen;
    *detourSlot = (unsigned char*)detour;

    unsigned char* jumpBack = trampoline + prologueLen;
    jumpBack[0] = 0xFF;
    jumpBack[1] = 0x25;
    *(unsigned char**)(jumpBack + 2) = (unsigned char*)backSlot;

    DWORD oldProtect = 0;
    if (!VirtualProtect(code, prologueLen, PAGE_EXECUTE_READWRITE, &oldProtect))
    {
        LogLine("hook aborted: VirtualProtect failed (%lu)", GetLastError());
        VirtualFree(trampoline, 0, MEM_RELEASE);
        return false;
    }
    code[0] = 0xFF;
    code[1] = 0x25;
    *(unsigned char**)(code + 2) = (unsigned char*)detourSlot;
    for (size_t index = 6; index < prologueLen; ++index) code[index] = 0x90;
    VirtualProtect(code, prologueLen, oldProtect, &oldProtect);
    FlushInstructionCache(GetCurrentProcess(), code, prologueLen);

    frame->target = target;
    frame->trampoline = trampoline;
    LogLine("hook installed target=%p trampoline=%p stolen=%u (whole instructions)",
        target, trampoline, (unsigned)prologueLen);
    return true;
}

// ---------------------------------------------------------------------------
// Hook 1: read the name block out of the packet the handler is about to parse.
//
// The walk below mirrors the opcode-733 handler exactly, so that a body the
// current server sends (name inline in the row) is recognised and left alone:
//
//     u8  rowCount
//     per row:
//       u32 nameLen                    <- 0x3457C80 reads this
//       u8  name[max(0, nameLen)]      <- only when 0 < nameLen < 31; a length of
//                                         0 or >= 31 makes the client fall back
//                                         to its static string and consume no
//                                         name bytes at all
//       u8  serverId
//       u32 unixTime
//
// A block is accepted only when the walk lands inside the body on a small name
// count whose entries consume the remaining bytes precisely. Anything else - in
// particular a native body, where the walk ends exactly at the end of the body -
// leaves g_nameCount at 0 and the module inert.
// ---------------------------------------------------------------------------
static const unsigned char* WalkRenameLogRows(const unsigned char* body, unsigned window,
                                              unsigned rows, unsigned* outOffset)
{
    unsigned offset = 1;                       // skip the row count
    for (unsigned row = 0; row < rows; ++row)
    {
        if (offset + 4 > window) return nullptr;
        unsigned length = (unsigned)body[offset] | ((unsigned)body[offset + 1] << 8) |
            ((unsigned)body[offset + 2] << 16) | ((unsigned)body[offset + 3] << 24);
        offset += 4;
        if (length > 0 && length < kClientNameLengthLimit)
        {
            if (offset + length > window) return nullptr;
            offset += length;
        }
        if (offset + 5 > window) return nullptr;   // u8 server id + u32 timestamp
        offset += 5;
    }
    *outOffset = offset;
    return body + offset;
}

extern "C" void LoadNamesFromPacket()
{
    g_nameCount = 0;
    g_nameIndex = 0;
    if (!g_base) return;

    const unsigned char* body = *(const unsigned char**)(g_base + kPacketCursorRva);
    int remaining = *(const int*)(g_base + kPacketRemainingRva);
    // The cursor length is only a sanity hint: a body longer than any notification
    // we send is a sign this is not the packet we think it is, and skipping keeps
    // the module from walking off the end of the buffer.
    if (!body || remaining < 3 || remaining > 65536)
    {
        LogLine("packet skipped: body=%p remaining=%d", body, remaining);
        return;
    }
    unsigned window = (unsigned)remaining;

    unsigned rows = body[0];
    if (rows == 0 || rows > kMaxRows)
    {
        LogLine("packet skipped: rows=%u remaining=%d", rows, remaining);
        return;
    }

    unsigned rowsEnd = 0;
    if (!WalkRenameLogRows(body, window, rows, &rowsEnd))
    {
        LogLine("packet rows do not walk cleanly: rows=%u remaining=%d (inert)",
            rows, remaining);
        return;
    }
    if (rowsEnd >= window)
    {
        // Native layout: the previous name travels inside each row, the handler
        // renders it itself and there is nothing for this module to supply.
        LogLine("native row layout detected rows=%u remaining=%d (inert)", rows, remaining);
        return;
    }

    const unsigned char* cursor = body + rowsEnd;
    const unsigned char* end = body + window;
    if (cursor + 2 > end)
    {
        LogLine("packet has no name block: rows=%u remaining=%d", rows, remaining);
        return;
    }
    unsigned names = (unsigned)cursor[0] | ((unsigned)cursor[1] << 8);
    cursor += 2;
    if (names == 0 || names > kMaxNames)
    {
        LogLine("name block ignored: names=%u", names);
        return;
    }

    for (unsigned index = 0; index < names && index < kMaxNames; ++index)
    {
        if (cursor + 4 > end) break;
        unsigned length = (unsigned)cursor[0] | ((unsigned)cursor[1] << 8) |
            ((unsigned)cursor[2] << 16) | ((unsigned)cursor[3] << 24);
        cursor += 4;
        if (length == 0 || length > kMaxNameBytes || cursor + length > end) break;
        int units = MultiByteToWideChar(936, 0, (const char*)cursor, (int)length,
            g_names[g_nameCount], kMaxNameUnits);
        if (units <= 0)
        {
            // Not valid GBK: fall back to a byte-wise widening so the column still
            // shows something rather than dropping the row.
            units = 0;
            for (unsigned byte = 0; byte < length && units < (int)kMaxNameUnits; ++byte)
            {
                g_names[g_nameCount][units++] = (wchar_t)cursor[byte];
            }
        }
        g_names[g_nameCount][units] = L'\0';
        g_nameCount++;
        cursor += length;
    }
    // A block is only trusted when it ends exactly with the body: a count that
    // happens to look plausible inside a body that has no block must not turn into
    // a name for a row that already carries its own.
    if (cursor != end || g_nameCount != names)
    {
        LogLine("name block rejected rows=%u names=%u trailing=%d", rows, names,
            (int)(end - cursor));
        g_nameCount = 0;
        return;
    }
    LogLine("legacy name block loaded rows=%u names=%u remaining=%d", rows, g_nameCount,
        remaining);
}

// Hook 1 entry stub: the client's handler must see the register file exactly as
// its caller left it, so snapshot, load, restore, then run the real prologue.
extern "C" __declspec(naked) void RenameLogHandlerStub()
{
    __asm
    {
        pushfd
        pushad
        call LoadNamesFromPacket
        popad
        popfd
        jmp dword ptr[g_handlerHook.trampoline]
    }
}

// ---------------------------------------------------------------------------
// Hook 2: hand the cached name to the third cell.
// ---------------------------------------------------------------------------
static wchar_t* __cdecl ResolveHook(void* entry)
{
    StringResolveFn original = (StringResolveFn)g_resolveHook.trampoline;
    if (g_base && (unsigned char*)entry == g_base + kThirdCellResourceRva &&
        g_nameIndex < g_nameCount)
    {
        wchar_t* name = g_names[g_nameIndex];
        g_nameIndex++;
        LogLine("name served row=%u text=%ls", g_nameIndex - 1, name);
        return name;
    }
    return original(entry);
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------
// A marker file next to the game executable disables both hooks without having to
// remove the module: create RenameLogNames.off and re-inject/restart.
static bool HooksDisabledByMarker()
{
    char path[MAX_PATH] = { 0 };
    GetModuleFileNameA(nullptr, path, MAX_PATH - 1);
    char* slash = strrchr(path, '\\');
    if (slash) *(slash + 1) = '\0';
    strncat(path, "RenameLogNames.off", MAX_PATH - strlen(path) - 1);
    DWORD attributes = GetFileAttributesA(path);
    return attributes != INVALID_FILE_ATTRIBUTES;
}

static void InstallHooks()
{
    // Whole instructions only. The handler prologue is
    //   55        push ebp
    //   8B EC     mov ebp, esp
    //   6A FF     push -1                  (SEH scope)
    //   68 <imm>  push <handler>          (SEH handler; build specific, copied
    //                                      verbatim and therefore NOT compared)
    // The resolver prologue is
    //   55        push ebp
    //   8B EC     mov ebp, esp
    //   8B 45 08  mov eax, [ebp+8]
    static const unsigned char kHandlerPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68
    };
    static const unsigned char kResolvePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };

    void* handler = g_base + kRenameLogHandlerRva;
    void* resolve = g_base + kStringResolveRva;

    if (HooksDisabledByMarker())
    {
        LogLine("hooks disabled by RenameLogNames.off; the module stays loaded and inert");
        return;
    }

    if (!InstallInlineHook(handler, kHandlerPrologue, sizeof(kHandlerPrologue),
            kHandlerPrologueLen, (void*)&RenameLogHandlerStub, &g_handlerHook))
    {
        LogLine("handler hook NOT installed; the 角色名 column stays empty");
    }
    if (!InstallInlineHook(resolve, kResolvePrologue, sizeof(kResolvePrologue),
            sizeof(kResolvePrologue), (void*)&ResolveHook, &g_resolveHook))
    {
        LogLine("resolver hook NOT installed; the 角色名 column stays empty");
    }
}

BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID reserved)
{
    (void)instance;
    (void)reserved;
    if (reason == DLL_PROCESS_ATTACH)
    {
        DisableThreadLibraryCalls(instance);
        g_base = (unsigned char*)GetModuleHandleA(nullptr);
        OpenLog();
        LogLine("RenameLogNames v5 (retired shim) attached base=%p handler=%p resolve=%p resource=%p",
            g_base,
            g_base ? g_base + kRenameLogHandlerRva : nullptr,
            g_base ? g_base + kStringResolveRva : nullptr,
            g_base ? g_base + kThirdCellResourceRva : nullptr);
        InstallHooks();
    }
    else if (reason == DLL_PROCESS_DETACH)
    {
        LogLine("RenameLogNames detached");
        if (g_log) fclose(g_log);
    }
    return TRUE;
}
