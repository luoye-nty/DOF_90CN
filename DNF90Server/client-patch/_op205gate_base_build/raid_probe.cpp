// ============================================================================
//  MERGED INTO 90CN.dll -- 2026-09-17
//
//  This file used to be the whole of RaidProbe.dll: a module the operator
//  injected into DNF.exe after 90CN.dll. It is now one translation unit of the
//  90CN project (client-patch\raid_probe.cpp, listed in 90CN.vcxproj) and its
//  entry point is InstallRaidProbe(), which 90CN's install worker calls once its
//  own hooks are installed.
//
//  WHY THE ORDER MATTERS: the SceneUiOpen observer below is a TAIL-JUMP that
//  chains onto an E9 hook somebody else already wrote at the target. Injection
//  used to guarantee that "somebody else" was 90CN.dll, because 90CN loaded
//  first. Merged into one module the guarantee has to be explicit, so
//  InstallRaidProbe() is called at the END of 90CN's install sequence -- and the
//  observer still refuses to install if the E9 is not there, exactly as before.
//
//  WHAT DID NOT CHANGE: the hook plan (kPlan), the fail-closed byte-signature
//  checks, RaidProbe.log written next to the client, and the dry-run self-test
//  that runs in any host that is not DNF.exe.
//
//  WHAT ELSE CHANGED (same day): the module reads no INI file and no environment
//  variable any more. Its RaidProbe.ini switches became the compile-time
//  constants in raidProbeConfig below, and the create-window owner restore --
//  never enabled in RaidProbe.ini -- is gone with its switch.
//
//  WHAT IS GONE: RaidProbe.vcxproj, build_raidprobe.bat and the separately
//  injected RaidProbe.dll. Nothing else consumed them.
// ============================================================================
//
// ============================================================================
//  RaidProbe.dll  --  standalone diagnostic probe for the Anton raid
//                     "闂傚倸鍊风粈渚€骞栭锕€绠悗锝庡枛闂傤垶鏌ㄥ┑鍡╂Ц闁绘帒鐏氶妵鍕箳瀹ュ牆鍘″銈忕稻閻擄繝骞婂璺虹厸闁告劧绲芥禍?/ 闂傚倸鍊风粈渚€骞夐敓鐘偓鍐幢濡炴洘妞介獮瀣熆椤戭剙瀚欢鐐测攽閻愵亜鐏柛鐘崇墵瀹曞綊骞嗚閺嗭箓鏌涢妷銏℃珦闁衡偓? selection window.
//
//  ---------------------------------------------------------------------------
//  REVISION 2 -- why v1 hard-crashed the game (2026-09-15 22:36:37)
//  ---------------------------------------------------------------------------
//  v1 wrote its log with:
//        _wfopen_s(&fp, path, L"a, ccs=UTF-8");   // wide-oriented stream
//        fprintf(fp, "...");                     // NARROW write -> invalid
//
//  `ccs=UTF-8` opens a *wide* stream; the matching writer is fwprintf (the
//  working XLog.h in this project uses exactly that). Calling the narrow
//  fprintf on it tripped the CRT's invalid-parameter path:
//
//        _invalid_parameter_noinfo_noreturn -> __fastfail(FAST_FAIL_INVALID_ARG)
//        RVA 0x8417:  push 5 / pop ecx / int 0x29
//
//  which Windows recorded as:
//        Faulting module : RaidProbe.dll
//        Exception code  : 0xc0000409  (STATUS_STACK_BUFFER_OVERRUN)
//        Fault offset    : 0x00008417
//
//  _wfopen_s had already created the file, so RaidProbe.log existed at 0 bytes
//  and not a single hook was ever installed.
//
//  v2 therefore uses NO CRT for I/O or formatting:
//        * CreateFileW / WriteFile          (kernel32)
//        * wsprintfA / wvsprintfA           (user32)
//  and, as a belt-and-braces net, installs a no-op invalid-parameter handler so
//  that no CRT misuse can ever fastfail the host again.
//
//  ---------------------------------------------------------------------------
//  WHAT IT ANSWERS
//  ---------------------------------------------------------------------------
//  Clicking "闂傚倸鍊风粈渚€骞栭锕€绠悗锝庡枛闂傤垶鏌ㄥ┑鍡╂Ц闁绘帒鐏氶妵鍕箳瀹ュ牆鍘″銈忕稻閻擄繝骞婂璺虹厸闁告劧绲芥禍? in ui/raidwindow/raidselectsinglemodewindow.xui does
//  nothing and the client sends no packet. The native handler is
//
//      void __cdecl sub_1EF2C70(uint32_t controlId)      VA 0x01EF2C70
//          controlId 0x1388 (5000) = 闂傚倸鍊风粈渚€骞栭锕€绠悗锝庡枛闂傤垶鏌ㄥ┑鍡╂Ц闁绘帒鐏氶妵鍕箳瀹ュ牆鍘″銈忕稻閻擄繝骞婂璺虹厸闁告劧绲芥禍? (team, 20 members)
//          any other id            = 闂傚倸鍊风粈渚€骞夐敓鐘偓鍐幢濡炴洘妞介獮瀣熆椤戭剙瀚欢鐐测攽閻愵亜鐏柛鐘崇墵瀹曞綊骞嗚閺嗭箓鏌涢妷銏℃珦闁衡偓?//
//  and the 0x1388 path is:
//
//      0x1EF2E60  cmp dword [ebp+8], 0x1388
//      0x1EF2E67  jne 0x1EF2FB8                  ; -> 闂傚倸鍊风粈渚€骞夐敓鐘偓鍐幢濡炴洘妞介獮瀣熆椤戭剙瀚欢鐐测攽閻愵亜鐏柛鐘崇墵瀹曞綊骞嗚閺嗭箓鏌涢妷銏℃珦闁衡偓?branch
//      0x1EF2E6D  call 0x1CF2A70                 ; GetScene()
//      0x1EF2E74  je   0x1EF2FDC                 ; * silent bail
//      0x1EF2E81  call 0x1CF26E0                 ; GetSub(scene)
//      0x1EF2E88  je   0x1EF2FDC                 ; * silent bail
//      0x1EF2E8F  mov  byte [0x5195BB8], 1       ; team-mode flag
//      0x1EF2E96  call 0x3417020                 ; the real action
//
//  0x1EF2FDC is a shared, silent cleanup exit, so "nothing happens" means one
//  of those two checks failed, or the click never reached the handler at all.
//
//  HOOKS (all read-only observers; every one calls the original through its
//  trampoline, so no game behaviour is altered)
//      0x01EF2C70  mode-select handler entry/exit + the two checks evaluated
//      0x03417020  the action the 0x1388 path calls
//      0x017B22C0  GetMyRaid()  -- non-null means the create window never opens
//      0x017B1DE0  create-attack-party entry; logs the UI owner byte it pushes
//      0x01B49240  the C2S 664 CREATE_RAID sender
//      0x02FDD1A0  SceneUiOpen -- observe-only tail-jump (90CN.dll owns it)
//      0x010384A0  raid mode-select window handler
//      ---- v11, added below: four observers on the S2C raid packet path ----
//      0x01D71CA0  S2C op591 (0x24F) entry        -> [591] key/mode
//      0x017BF120  member-array reader            -> [17BF120] obj/count
//      0x00A42AB0  op1359 handler tail target     -> [A42AB0] mgr
//      0x017BDF60  raid UI refresh notification   -> [17BDF60] caller
//
//  FAIL-CLOSED: every target is byte-signature verified before it is patched,
//  and the trampoline is published before the patch is enabled.
//
// ============================================================================
//  REVISION 3 (v11) -- the four S2C observers
// ============================================================================
//  Added to the module above; nothing existing changed behaviour. All four are
//  OBSERVE-ONLY: each detour is `pushad / log / popad / jmp <trampoline>`, so
//  no register, argument or return value of the hooked function is altered, and
//  the relocated prologue is replayed byte-for-byte before control returns to
//  target+relocLen.
//
//  WHY THESE FOUR (from this project's own raid RE, tools/raid_opcode_table.txt
//  and DNF90_閫嗗悜淇绗旇.md): every S2C handler reads its body through ONE
//  global cursor pair --
//        0x052F8500  cursor     (pointer into the current packet body)
//        0x052F8504  remaining
//  -- and every read advances it, so at a handler's entry the cursor is already
//  positioned on the field that handler reads next. That is what makes these
//  observers cheap and non-invasive: they read the cursor, not the handler.
//
//  PATCH SHAPES (verified byte-for-byte against client-patch\bin\DNF.exe):
//      0x01D71CA0 / 0x017BF120 / 0x017BDF60
//            55                push ebp        \  relocated into the trampoline
//            8B EC             mov  ebp, esp    |  lens = {1,2,2}  (5 bytes)
//            6A FF             push -1         /
//            68 xx xx xx xx    push imm32      <- NOT patched, stays in place
//          patchLen = 5: a 5-byte E9 replaces exactly those three instructions --
//          it lands on the `push imm32` boundary. The trampoline relays the same
//          5 bytes and returns to target+5, where the ORIGINAL `push imm32` still
//          is, so that instruction really executes. Relayed length == patched
//          length, so InstallHook()'s invariant (sum(lens) == patchLen) and every
//          other hook's install path are untouched.
//      0x00A42AB0
//            C6 81 1E 0E 00 00 01   mov byte ptr [ecx+0xE1E], 1     (7 bytes)
//          patchLen = 7, lens = {7}: patching 5 would cut this instruction in half.
//
//  LOG VOLUME: 591 and 17BF120 sit on per-packet paths, so each hook has its
//  own line budget (kHookLogCap) on top of Log()'s global 20 000-line cap, and
//  drops to silence once it is spent.
//
//  SELF-TEST (never runs inside the game): in any host that is not DNF.exe the
//  live install correctly fails closed on every signature, which says nothing
//  about the plan itself. So the module then DRY-RUNS the whole plan against the
//  real bytes of DNF.exe read from disk (PE VA -> file offset), installing each
//  hook into a private copy of its target page, and finally EXECUTES the four
//  v11 observers once each through their own installed hook with a synthetic
//  packet cursor mapped at the address they read. That proves the VA, the lens
//  table, the patch length, the relayed prologue, the stack discipline and the
//  log line -- without launching the game and without touching game memory.
// ============================================================================
//  REVISION 4 (v12) -- one fail-closed byte patch, and NOT a hook
// ============================================================================
//  Everything from v11 is unchanged. Added: the "claim patch" at
//  0x01D7222C -- a single 5-byte rewrite that removes one call in the mode-3
//  branch of the 0x24F (raid object / roster) handler:
//
//      0x01D72219  mov  eax, [ebp-0x14]   ; raid_key
//      0x01D7221E  shr  ecx, 0x10         ; raid_key >> 16
//      0x01D72221  cmp  ecx, ebx          ; ebx = the client's own channel
//      0x01D72223  jne  0x01D7225C        ; not our channel -> skip
//      0x01D7222B  50                     ; push eax -- the argument, and it is
//                                         ; OUTSIDE the 5 patched bytes
//      0x01D7222C  call 0x017C3470        ; E8 3F 12 A5 FF -> 58 90 90 90 90
//
//  sub_17C3470 writes [sceneOwner+4] = raid_key (and swaps the raid window:
//  closes 0x76/0x74/0x77, opens 0x75). Without the call a foreign raid's roster
//  still arrives and still renders, but the client no longer adopts that raid
//  as its own. That setter has THREE call sites and only this one is patched:
//  0x01D0438B (sub_1D04340, the 664 response handler) is the legitimate
//  "I am a member" path, and 0x01D71EF4 (mode 0 of this same handler, i.e. the
//  server's broadcastRuntimeRaidCreate, which only members receive) may be
//  load-bearing for members. Both are deliberately left alone.
//
//  WHY `pop eax` AND NOT FIVE NOPs -- the one thing here that must never be
//  "simplified": the argument push at 0x01D7222B is one byte BEFORE the patch,
//  so it stays, and sub_17C3470 is callee-cleanup (its only exit is
//  `mov esp,ebp; pop ebp; ret 4` at 0x017C3703, and all three call sites push
//  one argument and never `add esp,4`), i.e. the CALLEE is what pops that
//  argument. Remove the call and nobody pops it: esp then stays 4 bytes low from
//  0x01D72231 all the way to the epilogue at 0x01D722A2, whose restores are
//  esp-relative (`pop ecx / pop edi / pop esi / pop ebx`), so the handler's
//  caller would get edi/esi/ebx back one slot out. `pop eax` is the one-byte
//  instruction that consumes exactly that argument -- and eax already held the
//  pushed value, so no register and no flag changes. Five NOPs are NOT
//  equivalent, and the self-test's "claim-patch verify" line fails if the first
//  byte is not 0x58.
//
//  It is NOT in kPlan[]: kPlan[] describes detours, and this is a plain,
//  complete rewrite of one instruction (`E8 rel32` = exactly the 5 patched
//  bytes), so there is no trampoline, no relocated prologue and no slot for
//  another thread to race with. It is a behaviour change, not an observation,
//  so it is gated by raidProbeConfig::kApplyClaimPatch (a compile-time constant
//  now that the module reads no INI) and is FAIL-CLOSED:
//  the 5 bytes are read and compared with the expected original first, and on
//  any mismatch the patch is refused and the client is left untouched.
//
//  The self-test host also dry-runs it against DNF.exe on disk, next to the
//  plan dry-run, and prints
//      claim-patch dry-run: bytes at 01D7222C = E8 3F 12 A5 FF  (MATCH)
//      claim-patch verify: 01D7222C = 58 (pop eax), 5 bytes, next insn at
//                          01D72231  OK
//  or a FAIL that names the bytes actually found.
// ============================================================================

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <intrin.h>
#include <stdlib.h>
#include <stdint.h>

#pragma comment(lib, "user32.lib")

// ---------------------------------------------------------------------------
// Safety net: never let this module trip the CRT invalid-parameter fastfail.
// ---------------------------------------------------------------------------
static void __cdecl NoOpInvalidParameter(const wchar_t*, const wchar_t*,
                                         const wchar_t*, unsigned, uintptr_t)
{
    // deliberately empty: the _s functions will return their error code
}

static const uintptr_t kImageBase = 0x00400000;   // DNF.exe preferred base

// Resolved once at startup from GetModuleHandleW(NULL). Declared up here so
// every helper below, including the string dumper, can use it.
static uintptr_t g_base = kImageBase;

// This module's own HMODULE, resolved by InstallRaidProbe() from an address
// inside it -- the dry-run self-test needs the module directory to find DNF.exe
// on disk.
static HMODULE g_selfModule = NULL;
static wchar_t g_selfDir[MAX_PATH] = { 0 };

// Captures the module directory (with its trailing backslash) into g_selfDir.
static void ResolveSelfDirectory(HMODULE selfModule)
{
    if (!selfModule) return;
    wchar_t path[MAX_PATH] = { 0 };
    DWORD n = GetModuleFileNameW(selfModule, path, MAX_PATH);
    if (n == 0 || n >= MAX_PATH) return;
    wchar_t* slash = path;
    for (wchar_t* p = path; *p; ++p) if (*p == L'\\') slash = p;
    if (*slash != L'\\') return;
    *(slash + 1) = 0;
    size_t i = 0;
    for (; path[i] && i < MAX_PATH - 1; i++) g_selfDir[i] = path[i];
    g_selfDir[i] = 0;
}

// Absolute VA -> runtime address, for the kPlan[] entries and the absolute
// constants below (image base 0x400000). The kRva* constants further down are
// image-relative and are used as `g_base + kRva` directly; mixing the two
// conventions silently produces an address that is always wrong, which is the
// mistake documented at kVaCreateButtonGetMyRaidCaller.
static inline uintptr_t Va(uintptr_t va) { return g_base + (va - kImageBase); }

// ---------------------------------------------------------------------------
// Logging -- raw Win32, no CRT
// ---------------------------------------------------------------------------
static CRITICAL_SECTION g_logLock;
static HANDLE           g_log = INVALID_HANDLE_VALUE;
static volatile LONG    g_started = 0;
static DWORD            g_t0 = 0;

static DWORD NowMs() { return GetTickCount() - g_t0; }

static void Log(const char* fmt, ...)
{
    if (g_log == INVALID_HANDLE_VALUE) return;

    // Hard cap so a runaway hot path can never fill the disk.
    static volatile LONG s_lines = 0;
    if (InterlockedIncrement(&s_lines) > 20000) return;

    char msg[768];
    va_list ap;
    va_start(ap, fmt);
    int n = wvsprintfA(msg, fmt, ap);
    va_end(ap);
    if (n < 0) n = 0;
    if (n > (int)sizeof(msg) - 1) n = (int)sizeof(msg) - 1;

    char line[832];
    int head = wsprintfA(line, "[%8u] ", (unsigned)NowMs());
    if (head < 0) head = 0;
    if (head > (int)sizeof(line) - 4) head = (int)sizeof(line) - 4;
    for (int i = 0; i < n && head + i < (int)sizeof(line) - 3; i++)
        line[head + i] = msg[i];
    int len = head + n;
    if (len > (int)sizeof(line) - 3) len = (int)sizeof(line) - 3;
    line[len++] = '\r';
    line[len++] = '\n';

    EnterCriticalSection(&g_logLock);
    DWORD written = 0;
    WriteFile(g_log, line, (DWORD)len, &written, NULL);
    LeaveCriticalSection(&g_logLock);
}

static bool OpenLog()
{
    wchar_t path[MAX_PATH] = { 0 };
    if (GetModuleFileNameW(NULL, path, MAX_PATH) == 0) return false;
    wchar_t* slash = path;
    for (wchar_t* p = path; *p; ++p) if (*p == L'\\') slash = p;
    if (*slash == L'\\') *(slash + 1) = 0; else path[0] = 0;

    static const wchar_t kSuffix[] = L"RaidProbe.log";

    // Prefer the host executable's directory; fall back to %TEMP% so a
    // read-only or protected game directory still yields a usable log.
    const wchar_t* candidates[2] = { path, NULL };
    wchar_t temp[MAX_PATH] = { 0 };
    if (GetTempPathW(MAX_PATH, temp) != 0) candidates[1] = temp;

    for (int c = 0; c < 2; c++) {
        if (!candidates[c]) continue;
        wchar_t full[MAX_PATH] = { 0 };
        size_t used = 0;
        while (candidates[c][used]) { full[used] = candidates[c][used]; used++; }
        bool ok = true;
        for (size_t i = 0; i < (sizeof(kSuffix) / sizeof(wchar_t)); i++) {
            if (used + i >= MAX_PATH) { ok = false; break; }
            full[used + i] = kSuffix[i];
        }
        if (!ok) continue;
        g_log = CreateFileW(full, FILE_APPEND_DATA,
                            FILE_SHARE_READ | FILE_SHARE_WRITE, NULL,
                            OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
        if (g_log != INVALID_HANDLE_VALUE) return true;
    }
    return false;
}

// ---------------------------------------------------------------------------
// COMPILE-TIME CONFIGURATION -- this module reads no INI file and no
// environment variable.
//
// Every knob below used to be an external switch read from
// <host exe dir>\RaidProbe.ini. They are constants now: change a value and
// rebuild. The values are the ones this probe ran with while the Anton raid work
// was live (RaidProbe.ini carried patch_claim=1 and force_putong=1); the
// create-window owner restore, which was never enabled there, is gone with its
// switch.
// ---------------------------------------------------------------------------
namespace raidProbeConfig {
// The 5-byte rewrite at 0x01D7222C that stops the client from adopting a raid it
// merely browsed. FAIL-CLOSED: the original bytes are compared first.
constexpr bool kApplyClaimPatch = true;
// Rescue the dead BUTTON_TWENTY branch of the mode-select window by opening the
// create window ourselves when a click produced nothing at all.
constexpr bool kRescueTwentyPlayerButton = true;
}

// ---------------------------------------------------------------------------
// Trampoline relocation.
//
// THE BUG THIS EXISTS TO PREVENT (hit in the live game, 2026-09-15 22:44):
// sub_17B1DE0's first instruction is `E8 8B 0C 54 00` -- a *rel32* call. v3
// copied those five bytes verbatim into a trampoline allocated at 0x1C300000,
// but rel32 is relative to the instruction's own address, so the copy became
// `call 0x1C300005 + 0x540C8B` = 0x21740C90, far outside DNF.exe's image
// (which ends near 0x63B1590). Executing it faulted and the game's own crash
// handler (CrashDNF2.cra) took the process down on the first click of
// 缂傚倸鍊搁崐鎼佸磹妞嬪海鐭嗗ù锝堛€€閸嬫挸顫濋悡搴＄睄閻庤娲滈幊鎾伙綖濠靛牊宕夐柧蹇曟嚀閹藉姊洪懡銈呅㈡繛灞傚€曢锝囨崉閹巻鍋撻幒妤€绠涢柣妤€鐗冮幏? with no WER event.
//
// A naive byte copy is only safe when the copied instructions contain no
// relative operand. Four of the five targets are `push ebp / mov ebp,esp / ...`
// and were fine; createEntry was not.
//
// So the copy now walks an explicit per-instruction length table (the bytes
// were verified against the image, so the boundaries are known) and recomputes
// any relative branch so it keeps pointing at the same absolute destination.
// ---------------------------------------------------------------------------
static bool RelocateCopy(const BYTE* src, BYTE* dst, uintptr_t srcVa,
                         uintptr_t dstVa, const BYTE* lens, size_t count)
{
    size_t off = 0;
    for (size_t k = 0; k < count; k++) {
        size_t n = lens[k];
        if (n == 0 || n > 8) return false;          // unknown shape -> refuse
        for (size_t j = 0; j < n; j++) dst[off + j] = src[off + j];

        const BYTE* s = src + off;
        BYTE* d = dst + off;
        uintptr_t sVa = srcVa + off;
        uintptr_t dVa = dstVa + off;

        if ((s[0] == 0xE8 || s[0] == 0xE9) && n == 5) {
            int32_t rel = 0;
            for (int j = 0; j < 4; j++) ((BYTE*)&rel)[j] = s[1 + j];
            uintptr_t absTarget = sVa + 5 + (intptr_t)rel;
            int32_t nrel = (int32_t)(absTarget - (dVa + 5));
            for (int j = 0; j < 4; j++) d[1 + j] = ((BYTE*)&nrel)[j];
        } else if (s[0] == 0x0F && (s[1] & 0xF0) == 0x80 && n == 6) {
            int32_t rel = 0;
            for (int j = 0; j < 4; j++) ((BYTE*)&rel)[j] = s[2 + j];
            uintptr_t absTarget = sVa + 6 + (intptr_t)rel;
            int32_t nrel = (int32_t)(absTarget - (dVa + 6));
            for (int j = 0; j < 4; j++) d[2 + j] = ((BYTE*)&nrel)[j];
        } else if ((s[0] == 0xEB || (s[0] >= 0x70 && s[0] <= 0x7F)) && n == 2) {
            int8_t rel = (int8_t)s[1];
            uintptr_t absTarget = sVa + 2 + (intptr_t)rel;
            intptr_t delta = (intptr_t)absTarget - (intptr_t)(dVa + 2);
            if (delta < -128 || delta > 127) return false;  // cannot stay short
            d[1] = (BYTE)(int8_t)delta;
        }
        off += n;
    }
    return true;
}

// Length tables for the copied prologues (verified against the image).
static const BYTE kLens_A[] = { 1, 2, 2 };        // 55 | 8B EC | 6A FF
static const BYTE kLens_B[] = { 1, 2, 1, 3 };     // 55 | 8B EC | 53 | 8B 5D 08
static const BYTE kLens_C[] = { 1, 2, 4 };        // 56 | 8B F1 | 83 7E 08 00
static const BYTE kLens_D[] = { 5 };              // E8 rel32     <-- the rel32 case

// Arithmetic check of the relocator, run at every startup so a regression is
// visible in the log instead of in a game crash. No code is executed: the
// relocated displacement must resolve to the same absolute target as the
// original.
static bool SelfTestRelocator()
{
    BYTE src[5] = { 0xE8, 0x8B, 0x0C, 0x54, 0x00 };   // real createEntry bytes
    const uintptr_t srcVa = 0x017B1DE0;
    const uintptr_t dstVa = 0x1C300000;
    BYTE dst[5] = { 0 };
    if (!RelocateCopy(src, dst, srcVa, dstVa, kLens_D, 1)) return false;

    int32_t rel = 0;
    for (int j = 0; j < 4; j++) ((BYTE*)&rel)[j] = dst[1 + j];
    uintptr_t got = dstVa + 5 + (intptr_t)rel;

    int32_t orel = 0;
    for (int j = 0; j < 4; j++) ((BYTE*)&orel)[j] = src[1 + j];
    uintptr_t want = srcVa + 5 + (intptr_t)orel;

    // The relocated call must still land on 0x01CF2A70 and stay in the image.
    return got == want && want == 0x01CF2A70;
}

// ---------------------------------------------------------------------------
// Inline hook with an explicit, pre-verified patch length.
// The trampoline is published through `slot` BEFORE the patch is enabled, so a
// thread that enters the target during installation still reaches the original.
// ---------------------------------------------------------------------------
static bool InstallHook(uintptr_t target, void* detour, size_t patchLen,
                        const BYTE* lens, size_t lensCount, void** slot)
{
    if (patchLen < 5 || patchLen > 16) return false;

    // The length table must exactly cover the patched region. A mismatch means
    // the table and the target disagree, which is exactly the class of mistake
    // that produced the v3 game crash.
    size_t sum = 0;
    for (size_t k = 0; k < lensCount; k++) sum += lens[k];
    if (sum != patchLen) return false;

    BYTE* tramp = (BYTE*)VirtualAlloc(NULL, patchLen + 8,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
    if (!tramp) return false;

    __try {
        if (!RelocateCopy((const BYTE*)target, tramp, target, (uintptr_t)tramp,
                          lens, lensCount)) {
            VirtualFree(tramp, 0, MEM_RELEASE);
            return false;
        }
        tramp[patchLen] = 0xE9;
        *(int32_t*)(tramp + patchLen + 1) =
            (int32_t)((target + patchLen) - ((uintptr_t)tramp + patchLen + 5));

        *slot = tramp;                       // publish first
        MemoryBarrier();

        DWORD old = 0;
        if (!VirtualProtect((void*)target, patchLen,
                            PAGE_EXECUTE_READWRITE, &old)) {
            VirtualFree(tramp, 0, MEM_RELEASE);
            *slot = NULL;
            return false;
        }
        *(BYTE*)target = 0xE9;
        *(int32_t*)(target + 1) = (int32_t)((uintptr_t)detour - (target + 5));
        for (size_t i = 5; i < patchLen; i++) *(BYTE*)(target + i) = 0x90;
        VirtualProtect((void*)target, patchLen, old, &old);
        FlushInstructionCache(GetCurrentProcess(), (void*)target, patchLen);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        *slot = NULL;
        VirtualFree(tramp, 0, MEM_RELEASE);
        return false;
    }
    return true;
}

// Chaining variant: 90CN.dll already detours SceneUiOpen (sub_2FDD1A0), so its
// first byte in the live process is E9 rel32 rather than the native prologue.
// Copying those five bytes into a trampoline would jump away and never return,
// and re-pointing the trampoline at `target` would recurse into our own hook.
//
// The original has THREE stack args and is CALLEE-CLEANUP (`ret 0xC`) -- proven
// by sub_17B1DE0, which calls it and then just `ret`s with no `add esp,0xC`.
// So a naive `E8 oldDest; C3` trampoline would leave esp 12 bytes too high: the
// callee already removed the arguments, and the C3 would then pop an argument
// as if it were a return address. The trampoline must re-push the arguments for
// the inner call and clean its own on the way out:
//
//   FF 74 24 0C   push dword [esp+0x0C]   ; group
//   FF 74 24 0C   push dword [esp+0x0C]   ; mode  (each push shifts the frame)
//   FF 74 24 0C   push dword [esp+0x0C]   ; owner
//   E8 rel32      call oldDest
//   C2 0C 00      ret 0xC
//
// Net effect: exactly one call through to the other module's hook, and the
// caller's three arguments are consumed exactly once.
static bool InstallHookEx(uintptr_t target, void* detour, size_t patchLen,
                          const BYTE* lens, size_t lensCount, void** slot)
{
    BYTE first = 0;
    __try { first = *(volatile BYTE*)target; }
    __except (EXCEPTION_EXECUTE_HANDLER) { return false; }

    if (first != 0xE9) {
        return InstallHook(target, detour, patchLen, lens, lensCount, slot);
    }

    uintptr_t oldDest = 0;
    __try {
        int32_t rel = *(int32_t*)(target + 1);
        oldDest = target + 5 + (intptr_t)rel;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return false; }

    BYTE* tramp = (BYTE*)VirtualAlloc(NULL, 32, MEM_COMMIT | MEM_RESERVE,
                                      PAGE_EXECUTE_READWRITE);
    if (!tramp) return false;

    __try {
        for (int i = 0; i < 3; i++) {
            tramp[i * 4 + 0] = 0xFF;
            tramp[i * 4 + 1] = 0x74;
            tramp[i * 4 + 2] = 0x24;
            tramp[i * 4 + 3] = 0x0C;
        }
        tramp[12] = 0xE8;
        *(int32_t*)(tramp + 13) =
            (int32_t)(oldDest - ((uintptr_t)tramp + 18));
        tramp[17] = 0xC2;
        tramp[18] = 0x0C;
        tramp[19] = 0x00;

        *slot = tramp;                       // publish first
        MemoryBarrier();

        DWORD old = 0;
        if (!VirtualProtect((void*)target, 5, PAGE_EXECUTE_READWRITE, &old)) {
            *slot = NULL;
            VirtualFree(tramp, 0, MEM_RELEASE);
            return false;
        }
        *(BYTE*)target = 0xE9;
        *(int32_t*)(target + 1) = (int32_t)((uintptr_t)detour - (target + 5));
        VirtualProtect((void*)target, 5, old, &old);
        FlushInstructionCache(GetCurrentProcess(), (void*)target, 5);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        *slot = NULL;
        VirtualFree(tramp, 0, MEM_RELEASE);
        return false;
    }
    Log("      chained onto existing E9 hook at %08X -> oldDest=%08X",
        (unsigned)target, (unsigned)oldDest);
    return true;
}

// ---------------------------------------------------------------------------
// Execution test for the chained trampoline above.
//
// The trampoline is hand-written machine code, so an arithmetic argument is not
// enough -- it must actually run. A __stdcall stand-in has exactly the same
// shape as the real callee (3 stack args, callee cleans), and calling the
// trampoline through a __fastcall function pointer that matches SceneUiOpen's
// signature exercises the real stack discipline, including the `ret 0xC`.
//
// Run ONLY when the host is not DNF.exe. If this logic were ever wrong it would
// corrupt the stack, and that must never be allowed to happen in the game.
// ---------------------------------------------------------------------------
static volatile int g_fakeSeen = -1;

static int __stdcall FakeOldDest(int owner, int mode, int group)
{
    g_fakeSeen = owner * 1000000 + mode * 1000 + group;
    return 0x1234;
}

static bool SelfTestChainedTrampoline()
{
    BYTE* tramp = (BYTE*)VirtualAlloc(NULL, 32, MEM_COMMIT | MEM_RESERVE,
                                      PAGE_EXECUTE_READWRITE);
    if (!tramp) return false;

    uintptr_t oldDest = (uintptr_t)&FakeOldDest;
    for (int i = 0; i < 3; i++) {
        tramp[i * 4 + 0] = 0xFF;
        tramp[i * 4 + 1] = 0x74;
        tramp[i * 4 + 2] = 0x24;
        tramp[i * 4 + 3] = 0x0C;
    }
    tramp[12] = 0xE8;
    *(int32_t*)(tramp + 13) = (int32_t)(oldDest - ((uintptr_t)tramp + 18));
    tramp[17] = 0xC2;
    tramp[18] = 0x0C;
    tramp[19] = 0x00;
    FlushInstructionCache(GetCurrentProcess(), tramp, 20);

    typedef int (__fastcall* Fn)(void*, void*, int, int, int);
    Fn fn = (Fn)tramp;

    g_fakeSeen = -1;
    int r = fn(NULL, NULL, 0x76, 0x11, 0x22);   // owner, mode, group

    VirtualFree(tramp, 0, MEM_RELEASE);
    return (r == 0x1234) &&
           (g_fakeSeen == 0x76 * 1000000 + 0x11 * 1000 + 0x22);
}

// ---------------------------------------------------------------------------
// Decode the client's obfuscated string constants.
//
// sub_34D9B20(enc) returns a UTF-16 string; the raid mode-select handler at
// 0x010384A0 (proven by the v6 log: sceneUiOpen ... caller=01038542) compares
// the window's current mode against three of these and then either opens
// RaidCreateWindow (owner 0x76, group=1 or group=0) or does NOTHING AT ALL.
// Naming those three strings is exactly what tells us whether 闂備礁鎼幏瀣闯閿濆鐒垫い鎺嶈兌椤ｆ煡鎳氶埡鍐ｅ亾?is the
// branch that silently does nothing.
//
// The encoding is proprietary (key derived from cipher[1]&0xFE and cipher[2..3]
// by sub_34D99F0), so rather than reimplement it we simply call the game's own
// decoder. It self-initializes and is a pure function.
// ---------------------------------------------------------------------------
static const uintptr_t kRvaStrDecode = 0x030D9B20;   // sub_34D9B20

static const uintptr_t kRaidStrings[] = {
    0x04327780,   // sub_010384A0 branch #1 -> SceneUiOpen(0x76, group=1)
    0x0432775C,   // sub_010384A0 branch #2 -> SceneUiOpen(0x76, group=0)
    0x04327744,   // sub_010384A0 branch #3 -> NO window at all
    0x044D1578,   // used by sub_1B49240 (the 664 builder)
    0x044D1550,   // used by sub_1B49240
    0x044D1568,   // used by sub_1B49240
    0x044D1340,   // used by sub_1B49240
};

static void DumpRaidStrings()
{
    typedef const wchar_t* (__cdecl* DecodeFn)(void* enc);
    DecodeFn fn = (DecodeFn)(g_base + kRvaStrDecode);

    for (unsigned i = 0; i < sizeof(kRaidStrings) / sizeof(kRaidStrings[0]); i++) {
        uintptr_t va = g_base + (kRaidStrings[i] - kImageBase);
        __try {
            const wchar_t* s = fn((void*)va);
            if (!s) {
                Log("str 0x%08X = <null>", (unsigned)va);
                continue;
            }
            char utf8[320] = { 0 };
            int n = WideCharToMultiByte(CP_UTF8, 0, s, -1, utf8,
                                        (int)sizeof(utf8) - 1, NULL, NULL);
            if (n <= 0) utf8[0] = 0;
            Log("str 0x%08X = \"%s\"", (unsigned)va, utf8);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Log("str 0x%08X = <decode fault>", (unsigned)va);
        }
    }
}

static bool SignatureMatches(uintptr_t va, const BYTE* sig, size_t n)
{
    // Guarded: if this module is ever loaded into a host that is not DNF.exe,
    // the target addresses are unmapped and the check must fail, not fault.
    __try {
        return memcmp((const void*)va, sig, n) == 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

// Guarded reads of arbitrary addresses. The packet cursor, the packet body it
// points at and the game objects the v11 observers inspect are all game memory:
// any of them can be unmapped (or can be a stale pointer), so EVERY read goes
// through one of these and never dereferences bare.
static bool SafeReadBytes(uintptr_t va, BYTE* out, size_t n)
{
    __try {
        for (size_t i = 0; i < n; i++) out[i] = ((volatile BYTE*)va)[i];
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        for (size_t i = 0; i < n; i++) out[i] = 0;
        return false;
    }
}

static bool SafeReadU32(uintptr_t va, uint32_t* out)
{
    __try { *out = *(volatile uint32_t*)va; return true; }
    __except (EXCEPTION_EXECUTE_HANDLER) { *out = 0; return false; }
}

static bool SafeReadU8(uintptr_t va, unsigned* out)
{
    __try { *out = *(volatile BYTE*)va; return true; }
    __except (EXCEPTION_EXECUTE_HANDLER) { *out = 0; return false; }
}

// ASCII hex dump of a short byte run ("55 8B EC 6A FF"), for the install log:
// the bytes actually read at a target are part of the evidence, so they are
// written out next to every OK line instead of being implied by the signature.
static void HexBytes(const BYTE* b, size_t n, char* out /* >= n*3 */)
{
    static const char kHex[] = "0123456789ABCDEF";
    size_t o = 0;
    for (size_t i = 0; i < n; i++) {
        if (i) out[o++] = ' ';
        out[o++] = kHex[(b[i] >> 4) & 0x0F];
        out[o++] = kHex[b[i] & 0x0F];
    }
    out[o] = 0;
}

// Preview bytes for the "signature mismatch" log line. This MUST be guarded:
// when the check above fails because the address is unmapped, dereferencing it
// here would fault. (v2 hit exactly that -- RaidProbe.dll c0000005 at
// InstallThread+0x11E, `movzx eax, byte ptr [edi+4]`.)
static bool SafePreview(uintptr_t va, BYTE out[5])
{
    return SafeReadBytes(va, out, 5);
}

// ---------------------------------------------------------------------------
// Native game helpers we call directly (pure getters, no side effects)
// ---------------------------------------------------------------------------
// ALL addresses below are RVAs relative to the main module image base, and must
// be read as g_base + RVA. v2 mixed conventions here: kOwnerByteVa held an RVA
// (as 90CN.cpp does) but Detour_CreateEntry dereferenced it as if it were a VA,
// which logged a bogus "uiOwner=0x00".
// g_base itself is declared near the top of this file.

static const uintptr_t kRvaOwnerByte    = 0x013B1E0D;  // push imm8 in sub_17B1DE0
static const uintptr_t kRvaTeamFlag     = 0x01195BB8;  // mov byte [0x5195BB8],1
static const uintptr_t kRvaRaidService  = 0x011B2748;  // mov ecx, [0x51B2748]
static const uintptr_t kRvaGetScene     = 0x018F2A70;  // sub_1CF2A70
static const uintptr_t kRvaGetSub       = 0x018F26E0;  // sub_1CF26E0
static const uintptr_t kRvaSceneType    = 0x01E63E80;  // sub_2263E80
// Scene root pointer, as used by 90CN.cpp (kSceneRootPointerRva). This is the
// same [0x51B2764] the game itself loads before pushing a UI owner.
static const uintptr_t kRvaSceneRootPtr = 0x04DB2764;

// The client keeps one "is this UI owner open" vector per owner inside the
// scene root: begin/end at +4/+8 of a 20-byte slot at sceneRoot+0x108.
// (Same layout 90CN.cpp uses for its isOpen() checks.)
static const size_t kSceneUiVectorOffset = 0x108;
static const size_t kSceneUiVectorStride = 20;
static const unsigned kOwnerCreateWindow  = 0x76;   // RaidCreateWindow
static const unsigned kOwnerModeSelect    = 0x7A;   // RaidSelectSingleModeWindow
static const unsigned kOwnerAnnouncement  = 0x298;  // message / announcement

typedef void* (__cdecl*    GetSceneFn)(void);
typedef void* (__fastcall* GetSubFn)(void* scene);
typedef int   (__fastcall* SceneTypeFn)(void* scene);

static volatile LONG g_inModeSelect = 0;

static void* SafeGetScene()
{
    __try {
        GetSceneFn fn = (GetSceneFn)(g_base + kRvaGetScene);
        return fn();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return NULL; }
}

static void* SafeGetSub(void* scene)
{
    __try {
        GetSubFn fn = (GetSubFn)(g_base + kRvaGetSub);
        return fn(scene);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return NULL; }
}

static int SafeSceneType(void* scene)
{
    __try {
        SceneTypeFn fn = (SceneTypeFn)(g_base + kRvaSceneType);
        return fn(scene);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return -1; }
}

static unsigned SafeReadByte(uintptr_t rva, bool* ok)
{
    __try {
        unsigned v = *(volatile BYTE*)(g_base + rva);
        if (ok) *ok = true;
        return v;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        if (ok) *ok = false;
        return 0;
    }
}

// ---------------------------------------------------------------------------
// UI owner open-state probe.
//
// Rather than also detouring SceneUiOpen (sub_2FDD1A0) -- which 90CN.dll
// already hooks, and hooking on top of an existing E9 jmp cannot be chained
// safely with a copy-trampoline -- we read the same state 90CN itself reads.
// This answers "did the mode-select window actually open, and did clicking a
// button close it?" without touching any hot code path.
// ---------------------------------------------------------------------------
static void* SafeSceneRoot()
{
    __try {
        return *(void**)(g_base + kRvaSceneRootPtr);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return NULL; }
}

// -1 = unreadable, 0 = closed, 1 = open
static int SceneUiOwnerOpen(void* sceneRoot, unsigned owner)
{
    if (!sceneRoot) return -1;
    __try {
        uintptr_t vec = (uintptr_t)sceneRoot + kSceneUiVectorOffset +
                        (uintptr_t)owner * kSceneUiVectorStride;
        uint32_t begin = *(volatile uint32_t*)(vec + 4);
        uint32_t end   = *(volatile uint32_t*)(vec + 8);
        return begin != end ? 1 : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return -1; }
}

static void LogWindowState(const char* tag)
{
    void* root = SafeSceneRoot();
    int w76  = SceneUiOwnerOpen(root, kOwnerCreateWindow);
    int w7a  = SceneUiOwnerOpen(root, kOwnerModeSelect);
    int w298 = SceneUiOwnerOpen(root, kOwnerAnnouncement);
    Log("%s window76=%s window7A=%s window298=%s sceneRoot=%08X",
        tag,
        w76  < 0 ? "?" : (w76  ? "OPEN" : "closed"),
        w7a  < 0 ? "?" : (w7a  ? "OPEN" : "closed"),
        w298 < 0 ? "?" : (w298 ? "OPEN" : "closed"),
        (unsigned)(uintptr_t)root);
}

// ---------------------------------------------------------------------------
// 1. mode-select window handler   void __cdecl sub_1EF2C70(uint32_t controlId)
// ---------------------------------------------------------------------------
typedef void (__cdecl* ModeSelectFn)(uint32_t controlId);
static ModeSelectFn g_origModeSelect = NULL;

static void __cdecl Detour_ModeSelect(uint32_t controlId)
{
    uintptr_t caller = (uintptr_t)_ReturnAddress();
    const char* which = (controlId == 0x1388) ? "NORMAL/team(20p)"
                      : "other/single";

    Log("modeSelect ENTER controlId=0x%X (%u) -> %s  caller=+0x%X",
        (unsigned)controlId, (unsigned)controlId, which,
        (unsigned)(caller - kImageBase));

    if (controlId == 0x1388) {
        void* scene = SafeGetScene();
        void* sub = scene ? SafeGetSub(scene) : NULL;
        Log("modeSelect   GetScene()=%08X  GetSub(scene)=%08X  -> %s",
            (unsigned)(uintptr_t)scene, (unsigned)(uintptr_t)sub,
            (scene && sub) ? "PASS, will call 0x3417020"
                           : "FAIL, silent bail at 0x1EF2FDC");
        bool okTeam = false;
        unsigned team = SafeReadByte(kRvaTeamFlag, &okTeam);
        if (okTeam) Log("modeSelect   teamFlag[+0x1195BB8]=%u", team);
        else        Log("modeSelect   teamFlag[+0x1195BB8] unreadable");
    }

    InterlockedIncrement(&g_inModeSelect);
    if (g_origModeSelect) g_origModeSelect(controlId);
    InterlockedDecrement(&g_inModeSelect);

    Log("modeSelect EXIT  controlId=0x%X", (unsigned)controlId);
}

// ---------------------------------------------------------------------------
// 2. the action the 0x1388 path calls   void __cdecl sub_3417020(uint32_t)
// ---------------------------------------------------------------------------
typedef void (__cdecl* ActionFn)(uint32_t arg);
static ActionFn g_origAction = NULL;

static void __cdecl Detour_Action(uint32_t arg)
{
    LONG inside = InterlockedCompareExchange(&g_inModeSelect, 0, 0);
    if (inside > 0) Log("action 0x3417020 ENTER arg=%u (from mode-select)", (unsigned)arg);
    if (g_origAction) g_origAction(arg);
    if (inside > 0) Log("action 0x3417020 EXIT  arg=%u", (unsigned)arg);
}

// ---------------------------------------------------------------------------
// 2b. SceneUiOpen   sub_2FDD1A0   -- observe-only, TAIL-JUMP
//
// THE decisive hook. The v5 run proved that the mode-select window (owner 0x7A)
// opens and that clicking the single-player button leads to owner 0x76 opening
// and a 664 being built -- yet sub_1EF2C70 and sub_3417020 never ran once. So
// those are NOT the two button callbacks, and the only reliable way to find the
// real ones is to watch every window open with its owner, parameters and caller.
//
// 90CN's own hook is already on this function by the time this installs
// (InstallRaidProbe runs last in 90CN's install sequence). Rather than call through and return
// (which needs the exact stack convention and a relocated/call trampoline --
// v6's first attempt got that wrong and crashed its own self-test), this hook
// NEVER calls the original. It logs and then TAIL-JUMPS to whatever the
// pre-existing E9 already points at:
//
//      pushad ; log ; popad ; jmp [g_sceneUiOldDest]
//
// pushad/popad balance, so the stack is byte-identical to what the original
// entry expects, and the other module's proxy performs its own `ret 0xC`
// straight back to the real caller. No trampoline, no return value, no
// convention assumptions. If the target is not already E9-hooked the install
// is refused -- there is no safe fallback that would not need a trampoline.
//
// At entry: [esp]=ret, [esp+4]=uiOwner, [esp+8]=mode, [esp+12]=group.
// After pushad the same slots are at +36/+40/+44 and the return address +32.
// For the announcement window (0x298) `mode` is a text pointer, so it is only
// ever printed as hex.
// ---------------------------------------------------------------------------
static void* g_sceneUiOldDest = NULL;

extern "C" void LogSceneUiOpen(unsigned owner, unsigned mode, unsigned group,
                               unsigned caller)
{
    Log("sceneUiOpen owner=0x%X (%u) mode=0x%X group=0x%X caller=%08X",
        owner, owner, mode, group, caller);
}

extern "C" __declspec(naked) void Detour_SceneUiOpen()
{
    __asm {
        pushad
        mov  eax, [esp + 36]        // uiOwner
        mov  ecx, [esp + 40]        // mode
        mov  edx, [esp + 44]        // group
        mov  ebx, [esp + 32]        // return address = our caller
        push ebx
        push edx
        push ecx
        push eax
        call LogSceneUiOpen
        add  esp, 16
        popad
        jmp  dword ptr [g_sceneUiOldDest]
    }
}

// Installs a tail-jump observer on a target that another module already hooked
// with an E9. Returns false (and patches nothing) otherwise.
static bool InstallTailJumpHook(uintptr_t target, void* detour)
{
    BYTE first = 0;
    __try { first = *(volatile BYTE*)target; }
    __except (EXCEPTION_EXECUTE_HANDLER) { return false; }
    if (first != 0xE9) return false;

    uintptr_t oldDest = 0;
    __try {
        int32_t rel = *(int32_t*)(target + 1);
        oldDest = target + 5 + (intptr_t)rel;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { return false; }

    g_sceneUiOldDest = (void*)oldDest;      // publish before patching
    MemoryBarrier();

    DWORD old = 0;
    if (!VirtualProtect((void*)target, 5, PAGE_EXECUTE_READWRITE, &old))
        return false;
    *(BYTE*)target = 0xE9;
    *(int32_t*)(target + 1) = (int32_t)((uintptr_t)detour - (target + 5));
    VirtualProtect((void*)target, 5, old, &old);
    FlushInstructionCache(GetCurrentProcess(), (void*)target, 5);
    return true;
}

// ---------------------------------------------------------------------------
// 2c. Raid mode-select window handler   sub_010384A0
//
// This is the handler the v6 log pinned down (sceneUiOpen ... caller=01038542).
// It branches on the NAME of the clicked control:
//
//     0x010384F1  push "BUTTON_ONE"     -> match -> SceneUiOpen(0x76, 0, 1)  = 闁告娲戝Ч澶娢熼垾宕囩
//     0x01038553  push "BUTTON_TWENTY"  -> match -> SceneUiOpen(0x76, 0, 0)  = 闁哄拋鍣ｉ埀顒佺鑶╃€?/ 20濞?//     0x010385A6  push "Cancel"         -> match -> opens nothing
//     no match                          -> returns, DOES NOTHING AT ALL
//
// In the live client BUTTON_ONE works and BUTTON_TWENTY produces no window, so
// the 20-player button silently dies. Its branch, though, issues exactly the
// same SceneUiOpen(0x76, mode=0, group=0) that the unpatched sub_17B1DE0 issues
// -- the call we know works.
//
// Rather than guess which part of the name compare fails, this hook watches the
// OUTCOME and repairs it: if a click on the mode-select window changed nothing
// at all, the create window is opened directly. That is self-calibrating --
// BUTTON_ONE already opens 0x76 so it is left alone, and Cancel closes 0x7A so
// it is left alone too. Only the dead button is rescued.
//
// Calling convention: the shared exit is `ret 0x10`, so this is ecx = self plus
// FOUR stack arguments (only the first two are read natively).
// Gated by raidProbeConfig::kRescueTwentyPlayerButton (compile-time constant).
// ---------------------------------------------------------------------------
typedef void (__fastcall* RaidModeHandlerFn)(void* self, void* unused,
                                             unsigned a1, unsigned a2,
                                             unsigned a3, unsigned a4);
static RaidModeHandlerFn g_origRaidModeHandler = NULL;

// SceneUiOpen's real shape is ecx = scene root + THREE stack args (ret 0xC),
// which matches 90CN.cpp's proven 5-parameter __fastcall declaration.
typedef void* (__fastcall* SceneUiOpenCall)(void* self, void* unused,
                                            unsigned owner, int mode,
                                            unsigned group);

static void ForceCreateRaidWindow()
{
    __try {
        void* self = *(void**)(g_base + kRvaSceneRootPtr);
        SceneUiOpenCall fn =
            (SceneUiOpenCall)(g_base + (0x02FDD1A0 - kImageBase));
        fn(self, NULL, kOwnerCreateWindow, 0, 0);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("forceCreateWindow faulted");
    }
}

// Closes the mode-select window itself.
//
// Both native branches that complete the mode choice call the same virtual:
//     0x01038542  mov edx, [ebx]        ; ebx = ecx - 0x17c
//     0x01038544  mov eax, [edx + 0x78] ; vtable slot +0x78 = "close me"
//     0x01038549  call eax
// (the Cancel branch at 0x010385E3 does exactly the same.)
// The first fallback opened the create window without this, which is why the
// mode-select window was left sitting on screen behind it.
static void CloseModeSelectWindow(void* self)
{
    __try {
        void* target = (BYTE*)self - 0x17c;      // `lea ebx,[ecx-0x17c]`
        void** vtbl = *(void***)target;
        if (!vtbl) {
            Log("closeModeSelect: null vtable");
            return;
        }
        typedef void (__fastcall* CloseFn)(void*);
        CloseFn fn = (CloseFn)(*(void**)((BYTE*)vtbl + 0x78));
        if (!fn) {
            Log("closeModeSelect: null slot +0x78");
            return;
        }
        Log("closeModeSelect: calling vtable+0x78 fn=%08X target=%08X",
            (unsigned)(uintptr_t)fn, (unsigned)(uintptr_t)target);
        fn(target);
        Log("closeModeSelect: returned");
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("closeModeSelect faulted");
    }
}

static void __fastcall Detour_RaidModeHandler(void* self, void* unused,
                                              unsigned a1, unsigned a2,
                                              unsigned a3, unsigned a4)
{
    uintptr_t caller = (uintptr_t)_ReturnAddress();
    Log("modeHandler ENTER a1=0x%X a2=0x%X a3=0x%X a4=0x%X caller=%08X",
        a1, a2, a3, a4, (unsigned)caller);

    int before7a = SceneUiOwnerOpen(SafeSceneRoot(), kOwnerModeSelect);
    int before76 = SceneUiOwnerOpen(SafeSceneRoot(), kOwnerCreateWindow);

    if (g_origRaidModeHandler)
        g_origRaidModeHandler(self, unused, a1, a2, a3, a4);

    int after7a = SceneUiOwnerOpen(SafeSceneRoot(), kOwnerModeSelect);
    int after76 = SceneUiOwnerOpen(SafeSceneRoot(), kOwnerCreateWindow);
    Log("modeHandler EXIT  window7A %d->%d  window76 %d->%d",
        before7a, after7a, before76, after76);

    // Rescue the dead button: the click did nothing while the mode-select
    // window was open, and that window stayed open. Mirror the native working
    // branch exactly -- open the create window, THEN close the mode select --
    // otherwise the mode-select window is left behind on screen.
    if (raidProbeConfig::kRescueTwentyPlayerButton && a2 == 0x0A &&
        before7a == 1 && after7a == 1 && before76 != 1 && after76 != 1) {
        Log("modeHandler: click had no effect (dead 20-player button) "
            "-> forcing SceneUiOpen(0x76, 0, 0) then closing 0x7A");
        ForceCreateRaidWindow();
        CloseModeSelectWindow(self);
        Log("modeHandler: after force, window76=%d window7A=%d",
            SceneUiOwnerOpen(SafeSceneRoot(), kOwnerCreateWindow),
            SceneUiOwnerOpen(SafeSceneRoot(), kOwnerModeSelect));
    }
}

// ---------------------------------------------------------------------------
// 3. GetMyRaid   void* __fastcall sub_17B22C0(this)
//    The create-window entry is only reached when this returns NULL.
//    Only the create-raid button caller is logged; this getter has 65 call
//    sites and must not spam the log.
// ---------------------------------------------------------------------------
// NOTE ON ADDRESS CONVENTIONS -- v2 got this wrong twice, so it is spelled out:
//   * The kPlan[] targets and this constant are ABSOLUTE VAs as they appear in
//     the disassembly (image base 0x400000), so the runtime address is
//     g_base + (kVA - kImageBase).
//   * The kRva* constants above are image-relative, so the runtime address is
//     simply g_base + kRva.
// Mixing the two silently produces an address that is always wrong.
static const uintptr_t kVaCreateButtonGetMyRaidCaller = 0x03262A27;
static const uintptr_t kRvaCreateButtonGetMyRaidCaller =
    kVaCreateButtonGetMyRaidCaller - kImageBase;

typedef void* (__fastcall* GetMyRaidFn)(void* self);
static GetMyRaidFn g_origGetMyRaid = NULL;

static void* __fastcall Detour_GetMyRaid(void* self)
{
    void* r = g_origGetMyRaid ? g_origGetMyRaid(self) : NULL;
    uintptr_t ret = (uintptr_t)_ReturnAddress();
    if (ret == g_base + kRvaCreateButtonGetMyRaidCaller) {
        Log("GetMyRaid -> %08X  caller=create-raid-button%s",
            (unsigned)(uintptr_t)r,
            r ? "  => BLOCKED by GetMyRaid (error 49080 would be shown)"
              : "  => ok, entry proceeds");
    }
    return r;
}

// ---------------------------------------------------------------------------
// 4. create-attack-party entry   void __cdecl sub_17B1DE0(void)
//    Reads the UI owner byte it will push: 0x76 = RaidCreateWindow (native),
//    0x7A = RaidSelectSingleModeWindow (90CN.dll patch).
// ---------------------------------------------------------------------------
typedef void (__cdecl* CreateEntryFn)(void);
static CreateEntryFn g_origCreateEntry = NULL;

static void __cdecl Detour_CreateEntry(void)
{
    bool ok = false;
    unsigned owner = SafeReadByte(kRvaOwnerByte, &ok);
    Log("createEntry 0x17B1DE0 ENTER  uiOwner=%s (0x%02X: %s)",
        ok ? "" : "?",
        owner,
        owner == 0x7A ? "RaidSelectSingleModeWindow, 90CN patch ACTIVE"
      : owner == 0x76 ? "RaidCreateWindow, 90CN patch NOT active"
      : ok           ? "unexpected owner byte"
                     : "unreadable");

    // sub_17B1DE0 dispatches on the scene type; log it so we know which path
    // the native code will take.
    void* scene = SafeGetScene();
    if (scene) {
        int st = SafeSceneType(scene);
        Log("createEntry   scene=%08X sceneType=%d (0x%X)  idx=sceneType-0x17=%d %s",
            (unsigned)(uintptr_t)scene, st, (unsigned)st, st - 0x17,
            (st - 0x17) >= 0 && (st - 0x17) <= 0x13
                ? "IN jump-table range" : "OUT of range -> direct path");
    } else {
        Log("createEntry   GetScene() returned NULL");
    }

    // NOTE: SceneUiOpen (sub_2FDD1A0) may run a modal loop, so the matching
    // EXIT line can legitimately appear only after the window is closed.
    LogWindowState("createEntry   before-open :");
    if (g_origCreateEntry) g_origCreateEntry();
    LogWindowState("createEntry   after-return:");
    Log("createEntry 0x17B1DE0 EXIT");
}

// ---------------------------------------------------------------------------
// 5. C2S 664 CREATE_RAID sender  sub_1B49240
//    Calling convention not confirmed, so use a convention-agnostic naked
//    detour: log, then JMP (not call) into the trampoline so the original's
//    own `ret N` still performs the stack cleanup.
// ---------------------------------------------------------------------------
static void* g_tramp664 = NULL;

extern "C" void Log664Entry(uint32_t a1, uint32_t a2)
{
    // sub_1B49240 is NOT a dedicated 664 sender: it is a window message
    // handler whose second argument selects the message. Only message 0x0A
    // reaches the 664 build (the native code does `cmp ecx, 0xa` at entry).
    // Everything else is an ordinary UI message and would flood the log --
    // the v4 run produced 110 KB of it from messages 0xC/0x12/0x13 alone.
    if (a2 != 0x0A) return;
    Log("createRaid664 COMMIT msg=0x0A arg1=0x%X  (building the C2S 664 packet)",
        (unsigned)a1);
}

extern "C" __declspec(naked) void Detour_664()
{
    __asm {
        pushad
        mov  eax, [esp + 36]        // arg1 (after pushad: +32 = retaddr)
        mov  ecx, [esp + 40]        // arg2
        push ecx
        push eax
        call Log664Entry
        add  esp, 8
        popad
        jmp  dword ptr [g_tramp664]
    }
}

// ---------------------------------------------------------------------------
// 6. The packet-body cursor, and the four v11 observe-only hooks
//
// Every S2C handler in this client reads its body through ONE global cursor:
//
//      0x052F8500  cursor     -- pointer INTO the current packet body
//      0x052F8504  remaining  -- bytes left (each reader subtracts its width)
//
// and every read advances it. So at a handler's entry the cursor is already
// positioned on the field that handler is about to read, which is what lets
// these hooks observe the wire format without touching the reader primitives
// (sub_34E3970 u8 / sub_34E39E0 u16 / sub_34E3A50 u32) at all.
//
// These are ABSOLUTE VAs, so they are resolved with Va().
// ---------------------------------------------------------------------------
static const uintptr_t kVaCursor    = 0x052F8500;
static const uintptr_t kVaRemaining = 0x052F8504;

// 0x7C inside a raid member object -- the object's own member-count field, read
// next to the wire count purely so the two can be compared in the same line.
static const size_t kMemberObjCountOffset = 0x7C;

// 0xE1E inside the op1359 manager structure -- the flag byte sub_A42AB0 sets.
static const size_t kRaidInfoFlagOffset = 0xE1E;

// Per-hook line budget. 591 and 17BF120 can run once per packet, so each hook
// stops logging here and returns silently; Log() keeps its own, larger, global
// cap on top of this.
static const LONG kHookLogCap = 3000;

// True while the hook still has budget. The unguarded read is deliberate: the
// counter only ever grows, so the worst a race can do is let a few extra lines
// through.
static bool UnderCap(volatile LONG* counter, LONG cap)
{
    if (*counter >= cap) return false;
    return InterlockedIncrement(counter) <= cap;
}

// One-shot notice, so a truncated session is visible in the log instead of
// silently missing lines -- and so the call volume beyond the cap is at least
// bounded rather than unknown.
static void CapNotice(volatile LONG* raised, const char* tag,
                      volatile LONG* calls)
{
    if (InterlockedExchange(raised, 1) == 0)
        Log("%s log cap %d lines reached; further calls are not logged "
            "(calls so far: %d)", tag, (int)kHookLogCap, (int)*calls);
}

// ---------------------------------------------------------------------------
// 6a. 0x01D71CA0 -- S2C opcode 591 (0x24F): raid object / roster handler entry
//
//     [591] key=0x00170001 mode=3 cur=1AF86C0 rem=118
//
// raid_key and mode are the first two u32s of the body, and the cursor is
// already on the first of them when this handler is entered.
// ---------------------------------------------------------------------------
static void*         g_trampRaidObj591 = NULL;
static volatile LONG g_calls591        = 0;
static volatile LONG g_lines591        = 0;
static volatile LONG g_capNote591      = 0;

extern "C" void LogRaidObj591(void)
{
    InterlockedIncrement(&g_calls591);
    if (!UnderCap(&g_lines591, kHookLogCap)) {
        CapNotice(&g_capNote591, "[591]", &g_calls591);
        return;
    }

    uint32_t cur = 0, rem = 0, key = 0, mode = 0;
    bool okCur  = SafeReadU32(Va(kVaCursor), &cur);
    SafeReadU32(Va(kVaRemaining), &rem);
    bool okBody = okCur && SafeReadU32(cur, &key) && SafeReadU32(cur + 4, &mode);

    if (okBody) {
        Log("[591] key=0x%08X mode=%u cur=%08X rem=%u", key, mode, cur, rem);
    } else if (okCur) {
        Log("[591] key=? mode=? cur=%08X rem=%u (body unreadable at the cursor)",
            cur, rem);
    } else {
        Log("[591] key=? mode=? cursor[%08X] unreadable in this process",
            (unsigned)Va(kVaCursor));
    }
}

extern "C" __declspec(naked) void Detour_RaidObj591()
{
    __asm {
        pushad
        call LogRaidObj591          // no arguments: the logger reads the cursor
        popad                       // pushad/popad puts every register back,
                                    // including esp, exactly as it was
        jmp  dword ptr [g_trampRaidObj591]
    }
}

// ---------------------------------------------------------------------------
// 6b. 0x017BF120 -- member array reader
//
//     [17BF120] obj=0x12345678 count_u8=1 count_u32=0x00000001 objcount=0 ...
//
// obj       = the reader's first stack argument: [esp+4] at entry, which is
//             [esp+36] once pushad has pushed the eight registers.
// count_u8  = the BYTE the cursor is sitting on. The server writes this count
//             with a single writeByte (BuildMemberRefreshMode3Body /
//             BuildRaidObjectMode0Body), and this reader's own read sequence
//             contains u16 reads, never a u32 read of the count -- so the u8 is
//             the meaningful width and the u32 is only kept beside it as a
//             cross-check (it also exposes an unaligned read).
// objcount  = *(u8*)(obj + 0x7C), the object's own count field, for comparison.
// ---------------------------------------------------------------------------
static void*         g_trampMemberArray = NULL;
static volatile LONG g_callsMembers     = 0;
static volatile LONG g_linesMembers     = 0;
static volatile LONG g_capNoteMembers   = 0;

extern "C" void LogMemberArray(unsigned obj)
{
    InterlockedIncrement(&g_callsMembers);
    if (!UnderCap(&g_linesMembers, kHookLogCap)) {
        CapNotice(&g_capNoteMembers, "[17BF120]", &g_callsMembers);
        return;
    }

    uint32_t cur = 0, rem = 0, count32 = 0;
    unsigned count8 = 0, objcount = 0;
    bool okCur  = SafeReadU32(Va(kVaCursor), &cur);
    SafeReadU32(Va(kVaRemaining), &rem);
    bool okC8   = okCur && SafeReadU8(cur, &count8);
    bool okC32  = okCur && SafeReadU32(cur, &count32);
    bool okObj  = (obj != 0) && SafeReadU8((uintptr_t)obj + kMemberObjCountOffset,
                                           &objcount);

    if (okC8 && okC32 && okObj) {
        Log("[17BF120] obj=0x%08X count_u8=%u count_u32=0x%08X objcount=%u "
            "cur=%08X rem=%u", obj, count8, count32, objcount, cur, rem);
    } else if (!okCur) {
        Log("[17BF120] obj=0x%08X count_u8=? count_u32=? objcount=%u "
            "cursor[%08X] unreadable in this process", obj, objcount,
            (unsigned)Va(kVaCursor));
    } else {
        Log("[17BF120] obj=0x%08X cur=%08X rem=%u PARTIAL "
            "ok[count_u8=%u count_u32=%u obj+7C=%u]",
            obj, cur, rem, okC8 ? 1u : 0u, okC32 ? 1u : 0u, okObj ? 1u : 0u);
    }
}

extern "C" __declspec(naked) void Detour_MemberArray()
{
    __asm {
        pushad
        mov  eax, [esp + 36]        // first stack argument = object pointer
        push eax
        call LogMemberArray
        add  esp, 4
        popad
        jmp  dword ptr [g_trampMemberArray]
    }
}

// ---------------------------------------------------------------------------
// 6c. 0x00A42AB0 -- tail-call target of the class-0 op1359 handler
//
//     sub_1D3A150 -> sub_A42AB0, whose whole body is
//         mov byte ptr [ecx+0xE1E], 1
//         ret
//
// ecx is a large manager structure (NOT the raid object), so only that one flag
// byte is read, and only its value BEFORE the original instruction runs.
//
//     [A42AB0] mgr=0x0ABCDEF0 flag_before=0
// ---------------------------------------------------------------------------
static void*         g_trampTailFlag1359 = NULL;
static volatile LONG g_calls1359         = 0;
static volatile LONG g_lines1359         = 0;
static volatile LONG g_capNote1359       = 0;

extern "C" void LogTailFlag1359(unsigned mgr)
{
    InterlockedIncrement(&g_calls1359);
    if (!UnderCap(&g_lines1359, kHookLogCap)) {
        CapNotice(&g_capNote1359, "[A42AB0]", &g_calls1359);
        return;
    }

    unsigned flag = 0;
    if (mgr && SafeReadU8((uintptr_t)mgr + kRaidInfoFlagOffset, &flag)) {
        Log("[A42AB0] mgr=0x%08X flag_before=%u", mgr, flag);
    } else {
        Log("[A42AB0] mgr=0x%08X flag_before=? (unreadable)", mgr);
    }
}

extern "C" __declspec(naked) void Detour_TailFlag1359()
{
    __asm {
        pushad
        push ecx                    // ecx = the manager structure
        call LogTailFlag1359
        add  esp, 4
        popad                       // ecx is restored, so the relocated
                                    // `mov [ecx+0xE1E],1` sees the real pointer
        jmp  dword ptr [g_trampTailFlag1359]
    }
}

// ---------------------------------------------------------------------------
// 6d. 0x017BDF60 -- raid UI refresh notification
//
//     [17BDF60] caller=0x01D7229D
//
// The caller's return address is the original [esp] -- [esp+32] once pushad has
// pushed the eight registers.
// ---------------------------------------------------------------------------
static void*         g_trampUiRefresh = NULL;
static volatile LONG g_callsRefresh   = 0;
static volatile LONG g_linesRefresh   = 0;
static volatile LONG g_capNoteRefresh = 0;

extern "C" void LogUiRefresh(unsigned caller)
{
    InterlockedIncrement(&g_callsRefresh);
    if (!UnderCap(&g_linesRefresh, kHookLogCap)) {
        CapNotice(&g_capNoteRefresh, "[17BDF60]", &g_callsRefresh);
        return;
    }
    Log("[17BDF60] caller=0x%08X", caller);
}

extern "C" __declspec(naked) void Detour_UiRefresh()
{
    __asm {
        pushad
        mov  eax, [esp + 32]        // [esp] at entry = our caller's return
        push eax
        call LogUiRefresh
        add  esp, 4
        popad
        jmp  dword ptr [g_trampUiRefresh]
    }
}

// ---------------------------------------------------------------------------
// Hook table
// ---------------------------------------------------------------------------
static const BYTE kSig_ModeSelect[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xA4, 0x08, 0xD9, 0x03,
    0x64, 0xA1, 0x00, 0x00, 0x00, 0x00, 0x50, 0x83, 0xEC, 0x44 };
static const BYTE kSig_Action[] = {
    0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x08, 0x88, 0x1D, 0x64,
    0xDA, 0x0D, 0x05, 0xE8 };
static const BYTE kSig_GetMyRaid[] = {
    0x56, 0x8B, 0xF1, 0x83, 0x7E, 0x08, 0x00, 0x75, 0x11, 0x8B,
    0x46, 0x04, 0x83, 0xF8, 0xFF, 0x74, 0x09, 0x50, 0xE8 };
static const BYTE kSig_CreateEntry[] = {
    0xE8, 0x8B, 0x0C, 0x54, 0x00, 0x8B, 0xC8, 0xE8, 0x94, 0x20,
    0xAB, 0x00, 0x83, 0xC0, 0xE9, 0x83, 0xF8, 0x13, 0x77, 0x0E };
static const BYTE kSig_664[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xD8, 0x78, 0xD3, 0x03,
    0x64, 0xA1, 0x00, 0x00, 0x00, 0x00, 0x50, 0x83, 0xEC, 0x34 };
// Native prologue of SceneUiOpen. NOTE: 90CN.dll detours this same function,
// so in the live process the first byte will be E9 and this signature will NOT
// match -- InstallHookEx() detects that and chains instead. The signature is
// kept so the hook is refused outright on any other build.
static const BYTE kSig_SceneUiOpen[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x68, 0x9A, 0xEC, 0x03,
    0x64, 0xA1, 0x00, 0x00, 0x00, 0x00, 0x50, 0x83, 0xEC, 0x34 };
// Raid mode-select window message handler. Native prologue only -- nothing
// else hooks this one, so a signature match is required.
static const BYTE kSig_ModeHandler[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xC8, 0xC2, 0xDD, 0x03,
    0x64, 0xA1, 0x00, 0x00, 0x00, 0x00, 0x50, 0x83, 0xEC, 0x08 };

// ---- v11 targets ---------------------------------------------------------
// The three SEH prologues are signed over 10 bytes -- the 5 that the trampoline
// relays AND the 5 (`68 imm32`) that stay in place and are executed when the
// trampoline returns to target+5. A match therefore proves both halves of the
// patch. The leaf is signed over exactly the 7 bytes the E9 replaces.
// Verified against client-patch\bin\DNF.exe:
//   01D71CA0  55 8B EC 6A FF 68 FE A3 D6 03   push ebp / mov ebp,esp / push -1 / push 3D6A3FE
//   017BF120  55 8B EC 6A FF 68 73 76 CE 03   push ebp / mov ebp,esp / push -1 / push 3CE7673
//   00A42AB0  C6 81 1E 0E 00 00 01            mov byte ptr [ecx+0xE1E], 1
//   017BDF60  55 8B EC 6A FF 68 60 74 CE 03   push ebp / mov ebp,esp / push -1 / push 3CE7460
static const BYTE kSig_RaidObj591[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xFE, 0xA3, 0xD6, 0x03 };
static const BYTE kSig_MemberArray[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x73, 0x76, 0xCE, 0x03 };
static const BYTE kSig_TailFlag1359[] = {
    0xC6, 0x81, 0x1E, 0x0E, 0x00, 0x00, 0x01,
    0xC3 };   // + the `ret` the trampoline falls into at target+7
static const BYTE kSig_UiRefresh[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x60, 0x74, 0xCE, 0x03 };

struct Plan {
    uintptr_t   va;
    const char* name;
    const BYTE* sig;
    size_t      sigLen;
    size_t      patchLen;
    const BYTE* lens;       // per-instruction lengths covering patchLen
    size_t      lensCount;
    void*       detour;
    void**      slot;
    bool        allowChained;   // tolerate an existing E9 hook at the target
};

// The lens tables describe the EXACT instruction boundaries of each prologue.
// A wrong table is caught by RelocateCopy refusing the shape, never by a crash.
static const BYTE kLens_E[] = { 1, 2, 2 };      // 55 | 8B EC | 6A FF  (patchLen 5)
static const BYTE kLens_F[] = { 7 };            // C6 81 1E 0E 00 00 01

// Arithmetic check of the v11 length tables, run at every startup.
//
// These prologues contain no relative operand, so the relocated copy must come
// out byte-identical; on top of that this asserts the three properties the
// install depends on:
//   * sum(lens) == patchLen, the invariant InstallHook() enforces;
//   * the trampoline's tail jump resolves to target+patchLen -- for the three
//     SEH prologues that is target+5, i.e. the address of the `68 imm32` that
//     was NOT patched, so the original instruction is what runs next;
//   * the byte at that address really is that instruction (0x68 for the three
//     SEH prologues, 0xC3 for the leaf), which is what makes the return safe.
static bool SelfTestRelocatorNewTargets()
{
    struct Case {
        uintptr_t   va;
        const BYTE* sig;
        size_t      sigLen;
        size_t      patchLen;
        const BYTE* lens;
        size_t      lensCount;
        BYTE        inPlaceOpcode;   // what sits at target+patchLen
        const char* name;            // kept last on purpose: tools/_raidprobe_check.py
                                     // parses `{ 0xVA, "name", kSig_, sizeof, len,`
                                     // rows out of kPlan, and this table must not
                                     // look like one of them.
    };
    const Case cases[] = {
        { 0x01D71CA0, kSig_RaidObj591,   sizeof(kSig_RaidObj591),   5,
          kLens_E, 3, 0x68, "raidObj591" },
        { 0x017BF120, kSig_MemberArray,  sizeof(kSig_MemberArray),  5,
          kLens_E, 3, 0x68, "memberArray" },
        { 0x00A42AB0, kSig_TailFlag1359, sizeof(kSig_TailFlag1359), 7,
          kLens_F, 1, 0xC3, "tailFlag1359" },
        { 0x017BDF60, kSig_UiRefresh,    sizeof(kSig_UiRefresh),    5,
          kLens_E, 3, 0x68, "uiRefresh" },
    };
    const uintptr_t dstVa = 0x1C310000;

    for (unsigned i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
        const Case& c = cases[i];

        size_t relocLen = 0;
        for (size_t k = 0; k < c.lensCount; k++) {
            if (c.lens[k] == 0 || c.lens[k] > 8) return false;
            relocLen += c.lens[k];
        }
        if (relocLen != c.patchLen) return false;          // InstallHook invariant
        if (c.sigLen <= c.patchLen) return false;          // sig must also cover the
                                                           // in-place continuation
        if (c.sig[c.patchLen] != c.inPlaceOpcode) return false;

        BYTE dst[24] = { 0 };
        if (!RelocateCopy(c.sig, dst, c.va, dstVa, c.lens, c.lensCount))
            return false;
        for (size_t j = 0; j < relocLen; j++)
            if (dst[j] != c.sig[j]) return false;          // no relative operand

        dst[relocLen] = 0xE9;
        *(int32_t*)(dst + relocLen + 1) =
            (int32_t)((c.va + relocLen) - (dstVa + relocLen + 5));
        int32_t rel = 0;
        for (int j = 0; j < 4; j++) ((BYTE*)&rel)[j] = dst[relocLen + 1 + j];
        uintptr_t back = dstVa + relocLen + 5 + (intptr_t)rel;
        if (back != c.va + c.patchLen) return false;       // returns to target+patchLen

        // Printed per target, because "where does the trampoline return to" is
        // the one property that decides whether the in-place bytes still make
        // sense: sum(lens) == patchLen means the relayed bytes and the patched
        // bytes are the same 5 (or 7), so control comes back exactly on the
        // instruction that was NOT patched.
        Log("v11 lens check: %-14s va=%08X patchLen=%u sum(lens)=%u "
            "relayed=%u  trampoline tail-jmp -> target+%u (=%08X)  in-place byte "
            "%02X at +%u", c.name, (unsigned)c.va, (unsigned)c.patchLen,
            (unsigned)relocLen, (unsigned)relocLen, (unsigned)(back - c.va),
            (unsigned)back, c.sig[c.patchLen], (unsigned)c.patchLen);
    }
    return true;
}

static const Plan kPlan[] = {
    { 0x01EF2C70, "modeSelectHandler", kSig_ModeSelect,  sizeof(kSig_ModeSelect),  5,
      kLens_A, 3, (void*)&Detour_ModeSelect, (void**)&g_origModeSelect, false },
    { 0x03417020, "action3417020",     kSig_Action,      sizeof(kSig_Action),      7,
      kLens_B, 4, (void*)&Detour_Action,     (void**)&g_origAction,     false },
    { 0x017B22C0, "GetMyRaid",         kSig_GetMyRaid,   sizeof(kSig_GetMyRaid),   7,
      kLens_C, 3, (void*)&Detour_GetMyRaid,  (void**)&g_origGetMyRaid,  false },
    { 0x017B1DE0, "createEntry",       kSig_CreateEntry, sizeof(kSig_CreateEntry), 5,
      kLens_D, 1, (void*)&Detour_CreateEntry,(void**)&g_origCreateEntry,false },
    { 0x01B49240, "createRaid664",     kSig_664,         sizeof(kSig_664),         5,
      kLens_A, 3, (void*)&Detour_664,        (void**)&g_tramp664,       false },
    // allowChained = true: 90CN.dll owns SceneUiOpen too, so this one is
    // installed as an observe-only tail-jump instead of a normal hook. `slot`
    // is unused there because the detour never calls the original.
    { 0x02FDD1A0, "sceneUiOpen",       kSig_SceneUiOpen, sizeof(kSig_SceneUiOpen), 5,
      kLens_A, 3, (void*)&Detour_SceneUiOpen, NULL,                      true  },
    // Mode-select window handler: this is the one that proves which named
    // button was clicked and rescues the dead 闁哄拋鍣ｉ埀顒佺鑶╃€?branch.
    { 0x010384A0, "raidModeHandler",   kSig_ModeHandler, sizeof(kSig_ModeHandler), 5,
      kLens_A, 3, (void*)&Detour_RaidModeHandler,
      (void**)&g_origRaidModeHandler, false },

    // ---- v11: four observe-only observers on the S2C raid path ------------
    // The three SEH prologues are patched with 5 bytes -- the E9 lands exactly on
    // the `push imm32` boundary -- and the trampoline relays those same 5 bytes
    // and returns to target+5, where the ORIGINAL `push imm32` executes in
    // place. The leaf at 0x00A42AB0 is one 7-byte instruction and is patched
    // with 7, not 5. Every entry satisfies sum(lens) == patchLen, so
    // InstallHook() is exactly the shared implementation it always was.
    { 0x01D71CA0, "raidObj591",        kSig_RaidObj591,  sizeof(kSig_RaidObj591),  5,
      kLens_E, 3, (void*)&Detour_RaidObj591,  (void**)&g_trampRaidObj591,  false },
    { 0x017BF120, "memberArray",       kSig_MemberArray, sizeof(kSig_MemberArray), 5,
      kLens_E, 3, (void*)&Detour_MemberArray,(void**)&g_trampMemberArray, false },
    { 0x00A42AB0, "tailFlag1359",      kSig_TailFlag1359,sizeof(kSig_TailFlag1359),7,
      kLens_F, 1, (void*)&Detour_TailFlag1359,(void**)&g_trampTailFlag1359,false },
    { 0x017BDF60, "uiRefresh",         kSig_UiRefresh,   sizeof(kSig_UiRefresh),   5,
      kLens_E, 3, (void*)&Detour_UiRefresh,   (void**)&g_trampUiRefresh,   false },
};

// NOT in kPlan[], on purpose: the claim patch at 0x01D7222C is a plain byte
// rewrite with no detour, so it has no trampoline, no lens table, no signature
// slot and no install step to share. It lives in its own section further down
// ("THE CLAIM PATCH"), is applied once from the same startup thread, and is
// counted by nothing in the `armed: N/M hooks installed` line above.

// ===========================================================================
// PLAN DRY-RUN -- the self-test, and the only part of this file that must never
// run inside the game
// ===========================================================================
//  In the self-test host (a plain 32-bit process that does not contain the game
//  image) the live install correctly fails closed on every signature, so the
//  log would only ever say "0/N hooks installed". That proves the guard works
//  but says nothing about the plan itself, and the plan is where the real risk
//  is: a wrong VA, a wrong length table, a wrong patchLen, a detour that does
//  not survive being called.
//
//  So the same plan is replayed against the REAL bytes of DNF.exe read from
//  disk (VA -> file offset through the PE section table), each hook is
//  installed into a private copy of its target page through the SAME
//  InstallHook() the game uses, and the four v11 observers are then executed
//  once each through their own installed hook, with a synthetic packet cursor
//  mapped at the address they read.
//
//  Guarantees:
//    * nothing is written to any file -- DNF.exe is opened read-only;
//    * no game memory is touched: the only memory that changes is private pages
//      inside the self-test process;
//    * it cannot be reached inside DNF.exe (see HostLooksLikeDnf);
//    * every page it allocates is deliberately kept alive, so a stray later
//      call can only land in a live page.
// ===========================================================================

// ---------------------------------------------------------------------------
// Minimal read-only PE access, so the 82 MB client image is never mapped or
// loaded: the section table plus a handful of 16-byte reads is all that is
// needed, which matters in a 32-bit host.
// ---------------------------------------------------------------------------
struct PeSection { uint32_t va, vsize, raw, rsize; };

static PeSection g_peSections[16];
static int       g_peSectionCount = 0;
static uint32_t  g_peImageBase    = 0;
static bool      g_peOk           = false;

static void PathJoin(wchar_t* out /* MAX_PATH */, const wchar_t* dir,
                     const wchar_t* name)
{
    size_t i = 0;
    for (; dir[i] && i < MAX_PATH - 1; i++) out[i] = dir[i];
    for (size_t j = 0; name[j] && i < MAX_PATH - 1; j++) out[i++] = name[j];
    out[i] = 0;
}

static bool FileExists(const wchar_t* path)
{
    DWORD a = GetFileAttributesW(path);
    return a != INVALID_FILE_ATTRIBUTES && !(a & FILE_ATTRIBUTE_DIRECTORY);
}

// Where the client image lives, for the dry-run self-test only. There is no
// environment override any more: the candidates below cover both layouts this
// module is ever built into -- client-patch\Release\90CN.dll (the project's
// output directory) and <game dir>\90CN.dll (deployed next to the client).
static bool FindDnfExe(wchar_t* out /* MAX_PATH */)
{
    out[0] = 0;

    if (g_selfDir[0]) {
        static const wchar_t* kRel[] = {
            L"..\\..\\bin\\DNF.exe",      // probe\Release\ -> client-patch\bin
            L"..\\bin\\DNF.exe",
            L"DNF.exe",
        };
        for (int i = 0; i < 3; i++) {
            PathJoin(out, g_selfDir, kRel[i]);
            if (FileExists(out)) return true;
        }
    }

    // Deployed layout: RaidProbe.dll sitting next to DNF.exe.
    wchar_t host[MAX_PATH] = { 0 };
    if (GetModuleFileNameW(NULL, host, MAX_PATH)) {
        wchar_t* slash = host;
        for (wchar_t* p = host; *p; ++p) if (*p == L'\\') slash = p;
        if (*slash == L'\\') {
            *(slash + 1) = 0;
            PathJoin(out, host, L"DNF.exe");
            if (FileExists(out)) return true;
        }
    }

    out[0] = 0;
    return false;
}

static bool ReadFileAt(HANDLE h, uint32_t off, void* dst, DWORD n)
{
    DWORD got = 0;
    SetLastError(NO_ERROR);
    if (SetFilePointer(h, (LONG)off, NULL, FILE_BEGIN) ==
            INVALID_SET_FILE_POINTER && GetLastError() != NO_ERROR)
        return false;
    return ReadFile(h, dst, n, &got, NULL) && got == n;
}

static bool LoadPeMap(HANDLE h)
{
    BYTE hdr[0x400] = { 0 };
    if (!ReadFileAt(h, 0, hdr, sizeof(hdr))) return false;
    if (hdr[0] != 'M' || hdr[1] != 'Z') return false;

    uint32_t pe = *(uint32_t*)(hdr + 0x3C);
    if (pe + 0x100 > sizeof(hdr)) return false;
    if (hdr[pe] != 'P' || hdr[pe + 1] != 'E') return false;

    unsigned nsec  = *(uint16_t*)(hdr + pe + 6);
    unsigned optSz = *(uint16_t*)(hdr + pe + 20);
    unsigned opt   = pe + 24;
    if (nsec == 0 || nsec > 16) return false;
    if (opt + optSz + 4 > sizeof(hdr)) return false;

    g_peImageBase = *(uint32_t*)(hdr + opt + 28);
    unsigned secOff = opt + optSz;
    if (secOff + nsec * 40 > sizeof(hdr)) return false;

    for (unsigned i = 0; i < nsec; i++) {
        const BYTE* s = hdr + secOff + i * 40;
        g_peSections[i].vsize = *(uint32_t*)(s + 8);
        g_peSections[i].va    = *(uint32_t*)(s + 12);
        g_peSections[i].rsize = *(uint32_t*)(s + 16);
        g_peSections[i].raw   = *(uint32_t*)(s + 20);
    }
    g_peSectionCount = (int)nsec;
    g_peOk = true;
    return true;
}

static bool PeVaToOffset(uint32_t va, uint32_t* out)
{
    if (!g_peOk || va < g_peImageBase) return false;
    uint32_t rva = va - g_peImageBase;
    for (int i = 0; i < g_peSectionCount; i++) {
        const PeSection& s = g_peSections[i];
        uint32_t span = s.vsize > s.rsize ? s.vsize : s.rsize;
        if (rva >= s.va && rva < s.va + span) {
            *out = s.raw + (rva - s.va);
            return true;
        }
    }
    return false;
}

// ===========================================================================
// THE CLAIM PATCH -- one 5-byte rewrite, NOT a hook
// ===========================================================================
//  0x01D7222C sits in the mode-3 branch of the 0x24F (raid object / roster)
//  handler, behind this guard:
//
//      0x01D72219  mov  eax, [ebp-0x14]   ; raid_key
//      0x01D7221E  shr  ecx, 0x10         ; raid_key >> 16
//      0x01D72221  cmp  ecx, ebx          ; ebx = this client's own channel
//      0x01D72223  jne  0x01D7225C        ; not our channel -> skip the block
//      0x01D7222B  push eax               ; the call's argument -- 1 byte BEFORE
//                                         ; the patch, so it is NOT covered
//      0x01D7222C  call 0x017C3470        ; equal -> "claim me as this raid"
//
//  sub_17C3470 is the setter that writes [sceneOwner+4] = raid_key and swaps the
//  raid window around it (closes 0x76/0x74/0x77, opens 0x75). With that call
//  gone, browsing somebody else's raid still shows its roster, but the client no
//  longer adopts that raid as its own.
//
//  Of the setter's three call sites only this one is patched: 0x01D0438B in
//  sub_1D04340 (the 664 response handler) is the legitimate "I really am a
//  member" path, and 0x01D71EF4 is mode 0 of this very handler -- the server's
//  broadcastRuntimeRaidCreate, which only members receive and where the setting
//  may be load-bearing. Neither is touched.
//
//  Shape: the whole instruction is `E8 rel32` and patchLen is 5, so all five
//  bytes of it are replaced -- but the replacement is `pop eax` + four NOPs, NOT
//  five NOPs, because the argument pushed at 0x01D7222B would otherwise be left
//  on the stack (see the kClaimAfter comment below for the full argument). There
//  is nothing to relocate, no trampoline to publish and no half instruction to
//  leave behind -- which is also why this is not a kPlan[] row (see the note
//  under kPlan[]).
//
//  It changes behaviour rather than observing it, so it is gated by
//  raidProbeConfig::kApplyClaimPatch and FAIL-CLOSED: the 5 bytes are read first
//  and compared with the expected original, and anything else refuses the
//  patch and leaves the client byte-for-byte untouched.
//
//      0x01D7222C   before  E8 3F 12 A5 FF     (call 0x017C3470)
//                   after   58 90 90 90 90     (pop eax -- consumes the
//                                              argument; then four NOPs)
// ===========================================================================
static const uintptr_t kVaClaimPatch  = 0x01D7222C;
static const size_t    kClaimPatchLen = 5;
static const BYTE      kClaimBefore[kClaimPatchLen] = {
    0xE8, 0x3F, 0x12, 0xA5, 0xFF };

// The replacement is `pop eax` + four NOPs -- NOT five NOPs. This is not a
// style choice, and it must not be "tidied up" back to 90 90 90 90 90:
//
//   * 0x01D7222B is `push eax`, the ARGUMENT of the call being removed, and it
//     sits one byte BEFORE this patch, so it stays in the code.
//   * sub_17C3470 is CALLEE-CLEANUP: its only exit is `mov esp,ebp; pop ebp;
//     ret 4` at 0x017C3703, and all three call sites push exactly one argument
//     and never `add esp,4` afterwards. So the CALLEE is what pops that dword.
//   * Remove the call with NOPs and nobody pops it: esp stays 4 bytes low from
//     0x01D72231 until the epilogue at 0x01D722A2, whose register restores are
//     esp-relative (`pop ecx / pop edi / pop esi / pop ebx`) -- pop ecx would
//     eat the orphaned argument instead of the /GS cookie, and the handler's
//     caller would get edi/esi/ebx back one slot out (saved ebx thrown away by
//     `mov esp,ebp`).
//   * `pop eax` is the one-byte instruction that consumes exactly that
//     argument. eax already held the pushed value (it was pushed FROM eax), and
//     `pop` touches no flags, so no register or flag state changes at all.
//
// The self-test's "claim-patch verify" line asserts that byte 0 really is 0x58,
// so a silent revert to NOPs shows up as *** FAIL *** in the log.
static const BYTE      kClaimAfter[kClaimPatchLen] = {
    0x58, 0x90, 0x90, 0x90, 0x90 };

// Instruction widths of the replacement. Both 0x58 and 0x90 are ONE-BYTE
// instructions, so the replacement occupies exactly the same five bytes as the
// `E8 rel32` it replaces and the next instruction still starts at 0x01D72231.
// There is no disassembler in this module, so the widths are stated and the
// dry-run asserts that they add up to kClaimPatchLen.
static const BYTE kClaimAfterWidth[kClaimPatchLen] = { 1, 1, 1, 1, 1 };

// What the image holds at 0x01D72231, i.e. the instruction that must still be
// there after the patch: 8B 0D 64 27 1B 05 = mov ecx, [0x51B2764].
static const BYTE kClaimNextByte = 0x8B;

// The expected setter, i.e. what that rel32 has to decode to.
static const uintptr_t kVaClaimSetter = 0x017C3470;

static bool ConfigPatchClaim() { return raidProbeConfig::kApplyClaimPatch; }

// ---------------------------------------------------------------------------
// The live patch. Called ONCE, from the inside-DNF branch of InstallThread only
// (see the call site), so it can never rewrite memory that is not the client.
// Every outcome is logged, including the refusal, with the bytes actually read.
// ---------------------------------------------------------------------------
static void ApplyClaimPatch()
{
    if (!ConfigPatchClaim()) {
        Log("[claim-patch] disabled (raidProbeConfig::kApplyClaimPatch is false)");
        return;
    }

    uintptr_t at = Va(kVaClaimPatch);

    BYTE got[kClaimPatchLen] = { 0 };
    if (!SafeReadBytes(at, got, kClaimPatchLen)) {
        Log("[claim-patch] REFUSED: bytes at %08X are unreadable in this "
            "process, expected E8 3F 12 A5 FF - nothing patched",
            (unsigned)kVaClaimPatch);
        return;
    }

    char gotHex[kClaimPatchLen * 3] = { 0 };
    HexBytes(got, kClaimPatchLen, gotHex);

    bool match = true;
    for (size_t i = 0; i < kClaimPatchLen; i++)
        if (got[i] != kClaimBefore[i]) { match = false; break; }

    if (!match) {
        Log("[claim-patch] REFUSED: bytes at %08X are %s, expected "
            "E8 3F 12 A5 FF - nothing patched", (unsigned)kVaClaimPatch,
            gotHex);
        return;
    }

    bool patched = false;
    __try {
        DWORD old = 0;
        if (VirtualProtect((void*)at, kClaimPatchLen,
                           PAGE_EXECUTE_READWRITE, &old)) {
            for (size_t i = 0; i < kClaimPatchLen; i++)
                ((volatile BYTE*)at)[i] = kClaimAfter[i];
            // Flush while the page is still writable, then put the original
            // protection back.
            FlushInstructionCache(GetCurrentProcess(), (void*)at,
                                  kClaimPatchLen);
            VirtualProtect((void*)at, kClaimPatchLen, old, &old);
            patched = true;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) { patched = false; }

    if (!patched) {
        Log("[claim-patch] FAILED: VirtualProtect/write refused at %08X - "
            "nothing patched", (unsigned)at);
        return;
    }

    char wantHex[kClaimPatchLen * 3] = { 0 };
    HexBytes(kClaimAfter, kClaimPatchLen, wantHex);
    Log("[claim-patch] replaced call at %08X: %s -> %s (pop eax consumes the "
        "pushed arg; setter uses ret 4)", (unsigned)kVaClaimPatch, gotHex,
        wantHex);

    // Read the site back: the log should carry the evidence that `58 90 90 90
    // 90` is really in memory, not just the intent to put it there.
    BYTE now[kClaimPatchLen] = { 0 };
    if (SafeReadBytes(at, now, kClaimPatchLen)) {
        char nowHex[kClaimPatchLen * 3] = { 0 };
        HexBytes(now, kClaimPatchLen, nowHex);
        bool stuck = true;
        for (size_t i = 0; i < kClaimPatchLen; i++)
            if (now[i] != kClaimAfter[i]) { stuck = false; break; }
        Log("[claim-patch] read-back at VA %08X (runtime %08X) = %s  %s",
            (unsigned)kVaClaimPatch, (unsigned)at, nowHex,
            stuck ? "(patch is in place)" : "(MISMATCH - write did not stick)");
    } else {
        Log("[claim-patch] read-back at runtime %08X unreadable", (unsigned)at);
    }
}

// ---------------------------------------------------------------------------
// Read-only dry-run of the same site, against the DNF.exe image ON DISK.
//
// It reuses the PE map the plan dry-run has already loaded (PeVaToOffset +
// ReadFileAt) and proves four things without touching a byte of memory:
//   * 0x01D7222C resolves to a real file offset in this image;
//   * the five bytes there are exactly the ones the patch insists on;
//   * they really decode to `call 0x017C3470` -- i.e. this is the setter's call
//     site, and not merely five coincidentally-equal bytes;
//   * the REPLACEMENT (`pop eax` + 4 NOPs) still occupies exactly five bytes, so
//     the next instruction is still the image's 8B at 0x01D72231 -- printed as
//     the "claim-patch verify" line, which fails if byte 0 is not 0x58.
// So the live patch's fail-closed decision can be checked from the self-test
// host, before the module goes anywhere near the game.
// ---------------------------------------------------------------------------
static bool ClaimPatchDryRun(HANDLE h)
{
    BYTE got[kClaimPatchLen] = { 0 };
    uint32_t off = 0;
    if (!PeVaToOffset((uint32_t)kVaClaimPatch, &off) ||
        !ReadFileAt(h, off, got, (DWORD)kClaimPatchLen)) {
        Log("claim-patch dry-run: FAIL - VA %08X does not resolve to a file "
            "offset in this image, so the live patch would REFUSE",
            (unsigned)kVaClaimPatch);
        return false;
    }

    char gotHex[kClaimPatchLen * 3] = { 0 };
    char wantHex[kClaimPatchLen * 3] = { 0 };
    HexBytes(got, kClaimPatchLen, gotHex);
    HexBytes(kClaimBefore, kClaimPatchLen, wantHex);

    bool match = true;
    for (size_t i = 0; i < kClaimPatchLen; i++)
        if (got[i] != kClaimBefore[i]) { match = false; break; }

    if (!match) {
        Log("claim-patch dry-run: FAIL - bytes at %08X = %s, expected %s; this "
            "image is not the build the patch was verified against, so the live "
            "patch would REFUSE and change nothing",
            (unsigned)kVaClaimPatch, gotHex, wantHex);
        return false;
    }

    Log("claim-patch dry-run: bytes at %08X = %s  (MATCH)",
        (unsigned)kVaClaimPatch, gotHex);

    // Decode the displacement the way the CPU reads it, so the MATCH above
    // cannot be satisfied by five bytes that merely happen to be equal.
    int32_t rel = 0;
    for (int j = 0; j < 4; j++) ((BYTE*)&rel)[j] = got[1 + j];
    uintptr_t dest = kVaClaimPatch + 5 + (intptr_t)rel;
    bool targetOk = (dest == kVaClaimSetter);
    Log("claim-patch dry-run: file offset %X, %s decodes to call %08X "
        "(%s)", (unsigned)off, gotHex, (unsigned)dest,
        targetOk ? "the claim setter, as expected"
                 : "UNEXPECTED target - the live patch would still apply, "
                   "re-check the RE");

    // --- and now the bytes this patch WRITES -------------------------------
    // A second walk, of the REPLACEMENT: 0x58 (`pop eax`) and 0x90 are one-byte
    // instructions, so the five of them must add up to exactly the five bytes
    // the `E8 rel32` occupied -- that is what keeps the next instruction at
    // 0x01D72231, the address the guard's fall-through continues from. The
    // image byte there is read back and checked too, so a replacement that is
    // one byte short (which would shift the whole rest of the function) or one
    // byte long (which would eat the next instruction) cannot pass silently.
    size_t width = 0;
    for (size_t i = 0; i < kClaimPatchLen; i++) width += kClaimAfterWidth[i];

    bool firstIsPop = (kClaimAfter[0] == 0x58);
    bool restAreNop = true;
    for (size_t i = 1; i < kClaimPatchLen; i++)
        if (kClaimAfter[i] != 0x90) restAreNop = false;

    uintptr_t nextVa = kVaClaimPatch + kClaimPatchLen;
    BYTE nextByte = 0;
    bool haveNext = ReadFileAt(h, off + kClaimPatchLen, &nextByte, 1);

    bool verifyOk = firstIsPop && restAreNop && (width == kClaimPatchLen) &&
                    haveNext && (nextByte == kClaimNextByte);

    Log("claim-patch verify: %08X = %02X (%s), %u bytes, next insn at %08X  %s",
        (unsigned)kVaClaimPatch, (unsigned)kClaimAfter[0],
        firstIsPop ? "pop eax" : "NOT pop eax - the push would be orphaned",
        (unsigned)width, (unsigned)nextVa,
        verifyOk ? "OK" : "*** FAIL ***");

    if (!verifyOk) {
        Log("claim-patch verify: FAIL - first byte %s, remaining bytes %s, "
            "total width %u (must equal patchLen %u), image byte at %08X = %s",
            firstIsPop ? "is 58 (pop eax)" : "is NOT 58 (pop eax)",
            restAreNop ? "are four NOPs" : "are NOT four NOPs",
            (unsigned)width, (unsigned)kClaimPatchLen, (unsigned)nextVa,
            haveNext ? (nextByte == kClaimNextByte
                            ? "8B, as expected"
                            : "not 8B - the continuation moved")
                     : "unreadable");
        return false;
    }

    return targetOk;
}

// ---------------------------------------------------------------------------
// Calling into a dry-run hook.
//
// The detours are naked and must not be trusted with anything, so the checks
// are done in assembly: ebx/esi/edi are loaded with sentinels and the esp after
// the argument push is recorded, then both are tested after the call returns.
// A hook that scribbles on either -- or leaves the stack unbalanced -- is caught
// here instead of in the game.
// ---------------------------------------------------------------------------
static volatile LONG g_drySpExpected = 0;
static volatile LONG g_drySpOk       = 0;
static volatile LONG g_dryRegsOk     = 0;

// __cdecl, one argument: fn(arg)
extern "C" __declspec(naked) void DryRunCallCdecl(void* fn, void* arg)
{
    __asm {
        push ebx
        push esi
        push edi
        mov  ebx, 011111111h
        mov  esi, 022222222h
        mov  edi, 033333333h
        mov  eax, esp
        mov  g_drySpExpected, eax
        mov  eax, [esp + 16]            // fn  (12 saved bytes + return address)
        mov  ecx, [esp + 20]            // arg
        push ecx
        call eax
        add  esp, 4
        xor  ecx, ecx
        cmp  esp, g_drySpExpected
        sete cl
        mov  g_drySpOk, ecx
        xor  ecx, ecx
        cmp  ebx, 011111111h
        jne  dry_cdecl_done
        cmp  esi, 022222222h
        jne  dry_cdecl_done
        cmp  edi, 033333333h
        jne  dry_cdecl_done
        mov  ecx, 1
    dry_cdecl_done:
        mov  g_dryRegsOk, ecx
        pop  edi
        pop  esi
        pop  ebx
        ret
    }
}

// __fastcall with the pointer in ecx: fn(ecxValue)
extern "C" __declspec(naked) void DryRunCallEcx(void* fn, void* ecxValue)
{
    __asm {
        push ebx
        push esi
        push edi
        mov  ebx, 011111111h
        mov  esi, 022222222h
        mov  edi, 033333333h
        mov  eax, esp
        mov  g_drySpExpected, eax
        mov  eax, [esp + 16]            // fn
        mov  ecx, [esp + 20]            // argument passed in ecx
        call eax
        xor  ecx, ecx
        cmp  esp, g_drySpExpected
        sete cl
        mov  g_drySpOk, ecx
        xor  ecx, ecx
        cmp  ebx, 011111111h
        jne  dry_ecx_done
        cmp  esi, 022222222h
        jne  dry_ecx_done
        cmp  edi, 033333333h
        jne  dry_ecx_done
        mov  ecx, 1
    dry_ecx_done:
        mov  g_dryRegsOk, ecx
        pop  edi
        pop  esi
        pop  ebx
        ret
    }
}

// ===========================================================================
// Install every plan entry into a private copy of its real target page.
// Returns the number of hooks that installed cleanly, and logs one line per
// entry with the bytes actually read at the target.
// ===========================================================================
static void*    g_dryPages[16]    = { 0 };
static uint32_t g_dryRetAt[16]    = { 0 };   // where the trampoline comes back to
static uint32_t g_dryInPlaceImm[16] = { 0 }; // offset of an in-place `push imm32`
static volatile LONG g_dryPushedValue  = 0;  // what that push actually pushed
static volatile LONG g_dryEbpMinusEsp  = 0;  // frame depth when the stub is reached

// The stub the dry-run puts where the rest of the game function would be.
//
// For the three SEH prologues the trampoline's tail jump lands on the ORIGINAL
// `push imm32` that was left in place at target+5 (not patched, not relayed),
// which then falls through into this stub. So the stub can measure exactly the
// two things that prove the layout:
//
//      mov  eax, esp
//      mov  edx, ebp
//      sub  edx, eax                     ; ebp-esp = 8 when the in-place push
//                                        ; ran, 4 if it had been skipped
//      mov  [g_dryEbpMinusEsp], edx
//      mov  ecx, [esp]                   ; the value that push pushed
//      mov  [g_dryPushedValue], ecx      ; must equal the image's imm32 operand
//      mov  esp, ebp / pop ebp / ret     ; unwind the frame as the real code does
//
// It only clobbers eax/ecx/edx (caller-saved), never ebx/esi/edi/ebp, so the
// dry-run's register check stays meaningful.
static void WriteDryFrameStub(BYTE* at)
{
    BYTE* p = at;
    *p++ = 0x8B; *p++ = 0xC4;                        // mov eax, esp
    *p++ = 0x8B; *p++ = 0xD5;                        // mov edx, ebp
    *p++ = 0x29; *p++ = 0xC2;                        // sub edx, eax
    *p++ = 0x89; *p++ = 0x15;                        // mov [imm32], edx
    *(uint32_t*)p = (uint32_t)(uintptr_t)&g_dryEbpMinusEsp; p += 4;
    *p++ = 0x8B; *p++ = 0x0C; *p++ = 0x24;           // mov ecx, [esp]
    *p++ = 0x89; *p++ = 0x0D;                        // mov [imm32], ecx
    *(uint32_t*)p = (uint32_t)(uintptr_t)&g_dryPushedValue; p += 4;
    *p++ = 0x8B; *p++ = 0xE5;                        // mov esp, ebp
    *p++ = 0x5D;                                     // pop ebp
    *p++ = 0xC3;                                     // ret
}

static int PlanDryRun(void)
{
    wchar_t exe[MAX_PATH] = { 0 };
    if (!FindDnfExe(exe)) {
        Log("dry-run: DNF.exe not found next to this module or next to the host; "
            "place DNF.exe next to this module, in ..\\bin, or in ..\\..\\bin to enable the plan dry-run");
        return 0;
    }
    char exeA[MAX_PATH * 2] = { 0 };
    wchar_t full[MAX_PATH] = { 0 };
    const wchar_t* shown = exe;
    if (GetFullPathNameW(exe, MAX_PATH, full, NULL) > 0) shown = full;
    if (WideCharToMultiByte(CP_ACP, 0, shown, -1, exeA, sizeof(exeA) - 1,
                            NULL, NULL) <= 0) exeA[0] = 0;

    HANDLE h = CreateFileW(exe, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE,
                           NULL, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, NULL);
    if (h == INVALID_HANDLE_VALUE) {
        Log("dry-run: cannot open %s (win32 error %u)", exeA,
            (unsigned)GetLastError());
        return 0;
    }
    if (!LoadPeMap(h)) {
        Log("dry-run: %s is not a readable PE image", exeA);
        CloseHandle(h);
        return 0;
    }
    Log("dry-run source : %s", exeA);
    Log("dry-run image  : PE ImageBase=%08X, %d sections, VA -> file offset via "
        "the section table", (unsigned)g_peImageBase, g_peSectionCount);

    int total = (int)(sizeof(kPlan) / sizeof(kPlan[0]));
    int installed = 0;

    for (int i = 0; i < total; i++) {
        const Plan& p = kPlan[i];
        size_t relocLen = 0;
        for (size_t k = 0; k < p.lensCount; k++) relocLen += p.lens[k];

        BYTE bytes[40] = { 0 };
        uint32_t off = 0;
        // Read as much as EITHER the relocation or the signature needs, plus the
        // bytes that follow the patched region (the signature is usually longer
        // than the relocated prologue -- 20 bytes vs 5 -- and reading only
        // relocLen would compare the tail against zeros).
        size_t need = (relocLen > p.sigLen ? relocLen : p.sigLen) + 8;
        if (need > sizeof(bytes) || !PeVaToOffset((uint32_t)p.va, &off) ||
            !ReadFileAt(h, off, bytes, (DWORD)need)) {
            Log("DRYFAIL %-18s va=%08X : VA is not resolvable in this image "
                "- refused", p.name, (unsigned)p.va);
            continue;
        }

        if (memcmp(bytes, p.sig, p.sigLen) != 0) {
            char got[40 * 3] = { 0 };
            HexBytes(bytes, p.sigLen <= 16 ? p.sigLen : 16, got);
            Log("DRYFAIL %-18s va=%08X : on-disk signature mismatch (%s) "
                "- refused, nothing installed", p.name, (unsigned)p.va, got);
            continue;
        }

        char first[40 * 3] = { 0 };
        HexBytes(bytes, p.sigLen <= 16 ? p.sigLen : 16, first);

        BYTE* page = (BYTE*)VirtualAlloc(NULL, 0x1000, MEM_COMMIT | MEM_RESERVE,
                                         PAGE_EXECUTE_READWRITE);
        if (!page) {
            Log("DRYFAIL %-18s va=%08X : VirtualAlloc refused", p.name,
                (unsigned)p.va);
            continue;
        }
        for (size_t j = 0; j < relocLen; j++) page[j] = bytes[j];
        for (size_t j = relocLen; j < 32; j++) page[j] = 0xCC;
        // Restore the bytes that follow the patched region verbatim. They are
        // NOT cosmetic: with sum(lens) == patchLen the trampoline returns to
        // target+patchLen, and for the three SEH prologues that address holds
        // the original `push imm32` -- which therefore really executes, both
        // here and in the game.
        for (size_t j = relocLen; j < need; j++) page[j] = bytes[j];

        // Where control comes back to, and therefore where the dry-run stub
        // belongs. `68 imm32` is 5 bytes, so execution falls through to
        // target+relocLen+5 after the in-place push has run.
        size_t retAt = relocLen;
        if (bytes[relocLen] == 0x68) retAt = relocLen + 5;
        g_dryRetAt[i] = (uint32_t)retAt;
        g_dryInPlaceImm[i] = (retAt > relocLen) ? (uint32_t)(retAt - 4) : 0;

        // The stub stands in for the rest of the game function. A `push ebp ;
        // mov ebp,esp` frame is closed the way the real code would close it, and
        // it records the evidence that the in-place instruction really ran;
        // anything else (the 7-byte leaf) is simply followed by its own `ret`.
        if (bytes[0] == 0x55 && bytes[1] == 0x8B && bytes[2] == 0xEC) {
            WriteDryFrameStub(page + retAt);
        } else {
            page[retAt] = 0xC3;             // ret (the leaf already ends in one)
        }

        bool ok;
        if (p.allowChained) {
            // SceneUiOpen is owned by 90CN.dll, so the live install only accepts
            // a target that already starts with an E9. Synthesize that jump,
            // pointing at an empty `ret` inside the same page.
            page[0] = 0xE9;
            *(int32_t*)(page + 1) =
                (int32_t)((uintptr_t)(page + 0x100) - ((uintptr_t)page + 5));
            page[0x100] = 0xC3;
            ok = InstallTailJumpHook((uintptr_t)page, p.detour);
        } else {
            ok = InstallHook((uintptr_t)page, p.detour, p.patchLen, p.lens,
                             p.lensCount, p.slot);
        }
        if (!ok) {
            Log("DRYFAIL %-18s va=%08X : dry-run install failed (relocation or "
                "patch refused) - not installed", p.name, (unsigned)p.va);
            continue;
        }

        g_dryPages[i] = page;               // never freed, see the header above
        installed++;

        unsigned tramp = 0;
        if (p.slot && *p.slot)          tramp = (unsigned)(uintptr_t)*p.slot;
        else if (p.allowChained)        tramp = (unsigned)(uintptr_t)g_sceneUiOldDest;
        Log("OK    %-18s target=%08X  patch=%u  first=%s  trampoline=%08X  "
            "[DRY-RUN copy]", p.name, (unsigned)p.va, (unsigned)p.patchLen,
            first, tramp);
    }

    // The claim patch is not a hook, so it is not in the loop above -- but its
    // site is still checked against the same on-disk image, so a stale VA or a
    // swapped client is visible here instead of in the game.
    ClaimPatchDryRun(h);

    CloseHandle(h);
    return installed;
}

// ===========================================================================
// Execute the four v11 observers once each, through their own installed hook.
//
// A synthetic packet cursor is mapped at exactly the address the hooks read
// (g_base + RVA), so the lines they log are deterministic and checkable:
// cursor -> body must give key=0x00170001 mode=3 for 591, the field the cursor
// is aimed at must give count=2 for 17BF120 while obj+0x7C reads 0, the flag
// byte A42AB0 replays must end up 1, and 17BDF60's caller must be THIS module.
// If the page cannot be placed, the observers log their "unreadable" path
// instead -- which is the SEH guard being exercised, not a failure.
// ===========================================================================
static int DryRunExecObservers(int* testedOut)
{
    int tested = 0, passed = 0;
    int inPlace = 0, inPlaceExpected = 0;
    *testedOut = 0;

    // Scratch object/manager structure: zero-filled, so obj+0x7C = 0 and
    // flag_before = 0 are known values rather than guesses.
    BYTE* scratch = (BYTE*)VirtualAlloc(NULL, 0x2000, MEM_COMMIT | MEM_RESERVE,
                                        PAGE_READWRITE);
    if (!scratch) {
        Log("dry-run exec: scratch page refused - execution test skipped");
        return 0;
    }

    uintptr_t curVa = Va(kVaCursor);
    uintptr_t curPg = curVa & ~(uintptr_t)0xFFF;
    BYTE* fake = (BYTE*)VirtualAlloc((void*)curPg, 0x2000,
                                     MEM_COMMIT | MEM_RESERVE, PAGE_READWRITE);
    if (fake) {
        // one body page shared by 591 (key/mode at +0) and 17BF120 (count at
        // +0x100), plus the cursor/remaining pair the hooks read first.
        *(uint32_t*)(curPg + 0x1000 + 0) = 0x00170001;   // raid_key
        *(uint32_t*)(curPg + 0x1000 + 4) = 3;            // mode
        *(uint32_t*)(curPg + 0x1100)     = 2;            // member count = 2
        *(uint32_t*)curVa                = (uint32_t)(curPg + 0x1000);
        *(uint32_t*)(curVa + 4)          = 0x5A;         // remaining
        Log("dry-run exec: synthetic cursor mapped at %08X (body at %08X)",
            (unsigned)curVa, (unsigned)(curPg + 0x1000));
    } else {
        // The page is not ours, so whatever the observers read there is whatever
        // this host happens to keep at that address -- which may be readable
        // garbage or may fault into the SEH guard. Either is fine: the point of
        // running them here is that they RUN, not what they read.
        MEMORY_BASIC_INFORMATION mbi;
        if (VirtualQuery((LPCVOID)curPg, &mbi, sizeof(mbi))) {
            Log("dry-run exec: cursor page %08X is already in use by this host "
                "(state=%08X type=%08X), so a synthetic cursor cannot be placed "
                "there", (unsigned)curPg, (unsigned)mbi.State,
                (unsigned)mbi.Type);
        }
        Log("dry-run exec: in the game the cursor is %08X (base 00400000 + RVA "
            "%X); here that address is this host's own memory, so whatever the "
            "two cursor-driven observers print below is NOT packet data - read "
            "it as 'the hook ran', nothing more",
            (unsigned)kVaCursor, (unsigned)(kVaCursor - kImageBase));
    }

    int total = (int)(sizeof(kPlan) / sizeof(kPlan[0]));
    for (int i = 0; i < total; i++) {
        const Plan& p = kPlan[i];
        bool isA = (p.va == 0x01D71CA0);
        bool isB = (p.va == 0x017BF120);
        bool isC = (p.va == 0x00A42AB0);
        bool isD = (p.va == 0x017BDF60);
        if (!(isA || isB || isC || isD)) continue;

        void* page = g_dryPages[i];
        if (!page) {
            Log("dry-run exec: %-18s was not installed, not executed", p.name);
            continue;
        }

        if (fake) {   // aim the cursor at the field THIS observer reads
            *(uint32_t*)curVa = (uint32_t)(curPg + (isB ? 0x1100 : 0x1000));
            *(uint32_t*)(curVa + 4) = 0x5A;
        }

        volatile LONG* ctr = isA ? &g_calls591
                           : isB ? &g_callsMembers
                           : isC ? &g_calls1359 : &g_callsRefresh;
        LONG before = *ctr;

        g_drySpOk   = 0;
        g_dryRegsOk = 0;
        g_dryPushedValue = 0;
        g_dryEbpMinusEsp = 0;
        bool noFault = true;
        __try {
            if (isC) DryRunCallEcx(page, scratch);      // ecx = manager
            else     DryRunCallCdecl(page, scratch);    // [esp+4] = object (B)
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            noFault = false;
        }

        LONG fired = *ctr - before;
        bool spOk   = (g_drySpOk != 0);
        bool regsOk = (g_dryRegsOk != 0);

        // The three SEH prologues have to come back through the ORIGINAL
        // `push imm32` at target+patchLen: prove it by comparing the value that
        // push left on the stack with the operand stored in the image, and by
        // the frame depth (8 = three pushes, 4 if the in-place push had been
        // skipped).
        bool inPlaceOk = true;
        if (g_dryInPlaceImm[i]) {
            inPlaceExpected++;
            uint32_t want = *(uint32_t*)((BYTE*)page + g_dryInPlaceImm[i]);
            inPlaceOk = (g_dryPushedValue == (LONG)want) &&
                        (g_dryEbpMinusEsp == 8);
            if (inPlaceOk) inPlace++;
            Log("dry-run exec: %-18s returned to target+%u and the ORIGINAL "
                "push imm32 executed: pushed=%08X image operand=%08X "
                "ebp-esp=%u -> %s",
                p.name, (unsigned)p.patchLen, (unsigned)g_dryPushedValue,
                (unsigned)want, (unsigned)g_dryEbpMinusEsp,
                inPlaceOk ? "MATCH (in place, not skipped)" : "MISMATCH");
        }

        tested++;
        if (noFault && fired > 0 && spOk && regsOk && inPlaceOk) passed++;

        Log("dry-run exec: %-18s detour=%s stack=%s callee-saved=%s%s",
            p.name,
            fired > 0 ? "FIRED" : "DID-NOT-FIRE",
            spOk   ? "balanced" : "BROKEN",
            regsOk ? "intact"   : "CLOBBERED",
            noFault ? "" : "  FAULTED (SEH caught it)");
    }

    // A42AB0's original instruction must have been replayed by the trampoline:
    // the flag byte inside the scratch structure has to have become 1.
    {
        unsigned flagAfter = 0;
        if (SafeReadU8((uintptr_t)scratch + kRaidInfoFlagOffset, &flagAfter))
            Log("dry-run exec: trampoline replayed the original leaf "
                "-> [scratch+E1E] = %u (expected 1)", flagAfter);
    }

    Log("dry-run exec: %d/%d v11 observers executed cleanly (detour fired, "
        "stack balanced, callee-saved registers intact); %d/%d SEH prologues "
        "proved that the trampoline resumes at target+patchLen and the ORIGINAL "
        "push imm32 there really executes (operand matched the image)%s",
        passed, tested, inPlace, inPlaceExpected,
        fake ? "; field reads taken from the synthetic cursor"
             : "; the two cursor-driven field reads were NOT exercised here, "
               "see above");

    *testedOut = tested;
    return passed;
}

// Are we inside the game? Decided by CONTENT, not by the module base: if any
// plan target already holds its expected bytes then this process contains the
// client image (possibly relocated), and the live install path is the right one.
// A plain self-test host matches nothing here, which is what routes it to the
// dry-run instead of patching memory that does not belong to the game.
static bool HostLooksLikeDnf()
{
    for (int i = 0; i < (int)(sizeof(kPlan) / sizeof(kPlan[0])); i++) {
        const Plan& p = kPlan[i];
        if (SignatureMatches(Va(p.va), p.sig, p.sigLen)) return true;
    }
    return false;
}

// ---------------------------------------------------------------------------
// Window-state watcher: logs the open/closed state of the three raid windows
// only when it CHANGES, so the log carries a clean open/close timeline for the
// button click without any per-frame spam.
// ---------------------------------------------------------------------------
static DWORD WINAPI WatchThread(LPVOID)
{
    int last76 = -2, last7a = -2, last298 = -2;
    for (;;) {
        Sleep(200);
        void* root = SafeSceneRoot();
        int w76  = SceneUiOwnerOpen(root, kOwnerCreateWindow);
        int w7a  = SceneUiOwnerOpen(root, kOwnerModeSelect);
        int w298 = SceneUiOwnerOpen(root, kOwnerAnnouncement);
        if (w76 != last76 || w7a != last7a || w298 != last298) {
            LogWindowState("uiOwnerState CHANGE:");
            last76 = w76; last7a = w7a; last298 = w298;
        }
    }
    return 0;
}

// The install body. It used to run on a thread this module created from its own
// DllMain 1.5 s after injection; 90CN now calls it from its install worker once
// that worker's hooks are in place, so there is nothing left to wait for.
static DWORD WINAPI InstallThread(LPVOID)
{
    if (!OpenLog()) return 0;

    uintptr_t base = (uintptr_t)GetModuleHandleW(NULL);
    g_base = base;
    Log("================ RaidProbe v12 armed ================");
    Log("build: merged into 90CN.dll (client-patch\\raid_probe.cpp)");
    Log("main module base = %08X  (expected %08X)  %s",
        (unsigned)base, (unsigned)kImageBase,
        base == kImageBase ? "no relocation" : "RELOCATED, RVAs still valid");

    {
        bool ok = false;
        unsigned owner = SafeReadByte(kRvaOwnerByte, &ok);
        if (ok) {
            Log("initial uiOwner byte [+0x13B1E0D] = 0x%02X (%s)",
                owner,
                owner == 0x7A ? "0x7A mode-select -> 90CN patch IS active"
              : owner == 0x76 ? "0x76 create-window -> 90CN patch NOT active"
                              : "unexpected");
        } else {
            Log("initial uiOwner byte [+0x13B1E0D] unreadable in this host "
                "(expected when not running inside DNF.exe)");
        }
    }

    // Regression guard for the trampoline relocator. This is the check whose
    // absence let v3 crash the live game on the first click.
    Log("relocator self-test: %s (createEntry E8 rel32 must still resolve to "
        "0x01CF2A70)", SelfTestRelocator() ? "PASS" : "*** FAIL ***");
    Log("relocator self-test (v11 tables): %s (5- and 7-byte patches must land "
        "on an instruction boundary, and the relayed prologue must copy "
        "byte-for-byte)",
        SelfTestRelocatorNewTargets() ? "PASS" : "*** FAIL ***");
    // There is deliberately NO chained-trampoline self-test any more: the
    // first design (call through and return) needed a hand-written trampoline
    // whose stack discipline crashed the v6 self-test, so SceneUiOpen is now
    // observed with a tail-jump that never calls the original and therefore
    // has no stack contract to get wrong. Nothing left to self-test there.

    // Name the mode-select strings. One of these three is 闂備礁鎼幏瀣闯閿濆鐒垫い鎺嶈兌椤ｆ煡鎳氶埡鍐ｅ亾? and the
    // branch it belongs to decides whether the button opens the create window
    // or silently does nothing.
    if (base == kImageBase) {
        Log("--- raid mode-select strings (decoded by the game's own decoder) ---");
        DumpRaidStrings();
        Log("--- end strings ---");
    }

    // -----------------------------------------------------------------------

    int total = (int)(sizeof(kPlan) / sizeof(kPlan[0]));
    int installed = 0;

    // -----------------------------------------------------------------------
    // Is this the game, or a self-test host?
    //
    // Decided by CONTENT (does any plan target already hold its expected
    // bytes), never by the module base: that keeps a relocated client on the
    // live path, and keeps a host that merely happens to sit at 0x400000 off
    // it. In a self-test host nothing may be patched -- every signature check
    // would fail closed anyway -- so the plan itself is verified there instead,
    // by replaying it against the DNF.exe image on disk.
    // -----------------------------------------------------------------------
    bool insideDnf = HostLooksLikeDnf();
    if (!insideDnf) {
        Log("this host is not DNF.exe (no plan target holds its expected bytes): "
            "live install refused, running the plan DRY-RUN instead");
    }

    if (insideDnf) {
    for (int i = 0; i < total; i++) {
        const Plan& p = kPlan[i];
        uintptr_t target = base + (p.va - kImageBase);

        // The bytes actually read at the target. Reproduced in every OK/SKIP
        // line: a hook that installs is only as good as the bytes it was
        // installed over, so they are logged rather than implied.
        BYTE at[16] = { 0 };
        size_t atLen = p.sigLen <= sizeof(at) ? p.sigLen : sizeof(at);
        bool readable = SafeReadBytes(target, at, atLen);
        char first[16 * 3 + 2] = { 0 };
        if (readable) HexBytes(at, atLen, first);
        // Observe-only tail-jump entries (SceneUiOpen, which 90CN.dll also
        // detours) never call the original, so they need no signature match --
        // only a pre-existing E9 to hand control to. Refused outright otherwise.
        if (p.allowChained) {
            if (InstallTailJumpHook(target, p.detour)) {
                installed++;
                Log("OK    %-18s target=%08X  first=%s  observe-only tail-jump "
                    "-> oldDest=%08X", p.name, (unsigned)target, first,
                    (unsigned)(uintptr_t)g_sceneUiOldDest);
            } else {
                Log("SKIP  %-18s target=%08X  first=%s : not E9-hooked "
                    "(90CN's own hook installs before this one) - observe-only "
                    "hook needs a chain target, so it is not installed",
                    p.name, (unsigned)target, first);
            }
            continue;
        }

        if (!SignatureMatches(target, p.sig, p.sigLen)) {
            if (readable) {
                Log("SKIP  %-18s target=%08X : signature mismatch (%s) "
                    "- not patched", p.name, (unsigned)target, first);
            } else {
                Log("SKIP  %-18s target=%08X : unmapped in this host "
                    "- not patched", p.name, (unsigned)target);
            }
            continue;
        }

        if (!InstallHook(target, p.detour, p.patchLen, p.lens, p.lensCount,
                         p.slot)) {
            Log("FAIL  %-18s target=%08X : install failed (relocation or "
                "VirtualProtect refused) - not patched", p.name,
                (unsigned)target);
            continue;
        }
        installed++;
        Log("OK    %-18s target=%08X  patch=%u  first=%s  trampoline=%08X",
            p.name, (unsigned)target, (unsigned)p.patchLen, first,
            (unsigned)(uintptr_t)*p.slot);
    }

    Log("armed: %d/%d hooks installed.", installed, total);

    // -----------------------------------------------------------------------
    // The one byte patch that is not a hook (see "THE CLAIM PATCH" above).
    //
    // Same startup moment as the hooks and executed exactly once, from this
    // thread. It is deliberately inside the `insideDnf` branch: in a self-test
    // host the address would be this host's own memory, and the patch must
    // never be applied there -- that host verifies it against the on-disk image
    // instead (ClaimPatchDryRun).
    // -----------------------------------------------------------------------
    ApplyClaimPatch();

    LogWindowState("initial window state:");

    } else {
    // -----------------------------------------------------------------------
    // Self-test host: dry-run the same plan against the real target bytes read
    // out of DNF.exe on disk, then execute the four v11 observers once each.
    // It cannot happen inside the game (HostLooksLikeDnf), so no game memory
    // and no file is ever touched here.
    // -----------------------------------------------------------------------
    installed = PlanDryRun();
    Log("armed: %d/%d hooks installed.", installed, total);
    Log("note: DRY-RUN -- those installs went into private copies of the real "
        "target bytes in THIS process; no game memory was touched. Loaded into "
        "DNF.exe the identical path patches the live client and counts the same "
        "%d hooks.", total);

    int tested = 0;
    DryRunExecObservers(&tested);
    LogWindowState("dry-run window state:");
    }

    Log("now click the raid button, then the two buttons in the mode window.");

    HANDLE watch = CreateThread(NULL, 0, WatchThread, NULL, 0, NULL);
    if (watch) CloseHandle(watch);
    return 0;
}

// ---------------------------------------------------------------------------
// ENTRY POINT -- called by 90CN.dll, not by the loader.
//
// This was this module's DllMain while it was a DLL of its own. Merged into
// 90CN the call site is 90CN's install worker, after that worker has installed
// its own hooks: the SceneUiOpen observer chains onto 90CN's E9 there, and the
// one-shot guard below keeps a second call (a host that calls it twice) from
// installing anything twice.
//
// Written as a plain function on purpose: no CreateThread, no DLL_PROCESS_ATTACH
// state to inspect. The caller is already a worker thread.
// ---------------------------------------------------------------------------
extern "C" void InstallRaidProbe()
{
    // MUST be first: stops any later CRT misuse from fastfailing the client.
    _set_invalid_parameter_handler(NoOpInvalidParameter);
    _set_abort_behavior(0, _WRITE_ABORT_MSG | _CALL_REPORTFAULT);

    // This module's own base, taken from an address inside it, so the dry-run
    // self-test can resolve DNF.exe relative to the directory it loaded from.
    HMODULE selfModule = NULL;
    if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                            GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                            reinterpret_cast<LPCWSTR>(&InstallRaidProbe),
                            &selfModule)) {
        selfModule = NULL;
    }
    g_selfModule = selfModule;
    ResolveSelfDirectory(selfModule);

    if (InterlockedExchange(&g_started, 1) != 0) return;

    InitializeCriticalSection(&g_logLock);
    g_t0 = GetTickCount();
    InstallThread(NULL);        // synchronous: the caller already owns a thread
}
