#define WIN32_LEAN_AND_MEAN

#include "90CNLua.h"
#include "../client-patch/90CN.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdarg.h>
#include <string.h>
#include <wchar.h>

namespace
{
struct lua_State;

using LuaCFunction = int(__cdecl*)(lua_State*);
using LuaGetTop = int(__cdecl*)(lua_State*);
using LuaSetTop = void(__cdecl*)(lua_State*, int);
using LuaPushString = const char*(__cdecl*)(lua_State*, const char*);
using LuaPushCClosure = void(__cdecl*)(lua_State*, LuaCFunction, int);
using LuaPushBoolean = void(__cdecl*)(lua_State*, int);
using LuaGetTable = void(__cdecl*)(lua_State*, int);
using LuaCreateTable = void(__cdecl*)(lua_State*, int, int);
using LuaSetTable = void(__cdecl*)(lua_State*, int);
using LuaType = int(__cdecl*)(lua_State*, int);
using LuaPCall = int(__cdecl*)(lua_State*, int, int, int);
using LuaToString = const char*(__cdecl*)(lua_State*, int, size_t*);
using LuaLNewState = lua_State*(__cdecl*)();
using LuaLOpenLibs = void(__cdecl*)(lua_State*);
using QueueClientNotice = BOOL (WINAPI*)(const wchar_t*, unsigned int);
using DequeueClientEvent = BOOL (WINAPI*)(
    DNF90ClientEvent*, unsigned int);
using QueryCharacterStatSnapshot = BOOL (WINAPI*)(
    DNF90CharacterStatSnapshot*, unsigned int);
using QueryEquipmentSnapshot = BOOL (WINAPI*)(
    DNF90EquipmentSnapshot*, unsigned int);
using QueryDamageAffixSnapshot = BOOL (WINAPI*)(
    DNF90DamageAffixSnapshot*, unsigned int);
using UpdateCombatPanel = BOOL (WINAPI*)(
    const DNF90CombatPanelState*, unsigned int);
using UpdateDebugPanel = BOOL (WINAPI*)(
    const DNF90DebugPanelState*, unsigned int);
using QueueGameCommand = BOOL (WINAPI*)(
    const DNF90GameCommand*, unsigned int);
using QueryActorSnapshot = BOOL (WINAPI*)(
    DNF90ActorSnapshot*, unsigned int);
using DequeueBossMutationDecision = BOOL (WINAPI*)(
    DNF90BossMutationDecision*, unsigned int);
using WriteClientDiagnostic = BOOL (WINAPI*)(const char*, unsigned int);
using NativeLuaFactory = void* (__thiscall*)(
    void*, void*, const wchar_t*, const wchar_t*);
using NativeAiDecisionRunner = int (__thiscall*)(void*);
using NativeMonsterIndexGetter = int (__thiscall*)(void*);
using NativeMonsterCurrentHpGetter = __int64 (__thiscall*)(void*);
using NativeMonsterMaxHpGetter = __int64 (__thiscall*)(void*);
using NativeMonsterHpStateGetter = void* (__thiscall*)(void*);
using NativeMonsterStatBlockGetter = void* (__thiscall*)(void*, int);
using EncodedValueReader = void (__thiscall*)(
    const unsigned int*, const unsigned int*, unsigned int*);
using EncodedValueWriter = void (__thiscall*)(
    unsigned int*, const unsigned int*, unsigned int*);
using EncodedInt64Writer = void (__thiscall*)(
    unsigned int*, const __int64*, unsigned int*);

constexpr uintptr_t kLuaGetTopRva = 0x00002BA0;
constexpr uintptr_t kLuaSetTopRva = 0x00002BC0;
constexpr uintptr_t kLuaTypeRva = 0x00002CD0;
constexpr uintptr_t kLuaToStringRva = 0x00002DF0;
constexpr uintptr_t kLuaPushStringRva = 0x00002F80;
constexpr uintptr_t kLuaPushCClosureRva = 0x00003030;
constexpr uintptr_t kLuaGetTableRva = 0x00003150;
constexpr uintptr_t kLuaCreateTableRva = 0x000031E0;
constexpr uintptr_t kLuaSetTableRva = 0x00003360;
constexpr uintptr_t kLuaPCallRva = 0x000035F0;
constexpr uintptr_t kLuaPushBooleanRva = 0x000200E0;
constexpr uintptr_t kLuaLNewStateRva = 0x00002A40;
constexpr uintptr_t kLuaLOpenLibsRva = 0x00001530;
// sub_12E7070 is the inner Lua factory reached by the AI controller's virtual
// loader.  Its result owns lua_State* at +0x04.  The only other direct caller
// is rejected by the exact AI-controller vtable check in the proxy.
constexpr uintptr_t kNativeLuaFactoryRva = 0x00EE7070;
constexpr uintptr_t kNativeAiControllerVtableRva = 0x041CE00C;
// IDA/current-image evidence proves that every table-driven and Lua-backed
// monster controller is built by sub_261E0D0 and that sub_262ACC0 is their
// shared synchronous result runner.  The latter returns one of the stock
// action, move-method, or destination-select values according to +0x88.
constexpr uintptr_t kNativeAiDecisionRunnerRva = 0x0222ACC0;
// sub_261E0D0(controller, owner) stores the monster's AI-owner object verbatim
// at +0x08. Live Botticelli evidence proves this is not an actor registered in
// dword_51B2728's global object-key table: four Lua controllers share the same
// owner and the owner's real combat object is referenced at +0x58. The plugin
// therefore groups native states by this owner internally and exposes only a
// generated scalar monster handle; it never publishes either pointer.
constexpr size_t kNativeAiControllerOwnerOffset = 0x08;
constexpr size_t kNativeAiControllerResultKindOffset = 0x88;
constexpr size_t kNativeAiOwnerCombatObjectOffset = 0x58;

// The current EXE materializes every monster into the same four 1848-byte
// encoded stat blocks.  Mode 3 is the effective combat block.  The offsets
// below are proved by sub_240F2A0's CHANGE_STATUS switch and by
// sub_24A5B20's current 92-byte actor-state expansion:
//   4/48 = equipment physical/magical attack,
//   5/49 = equipment physical/magical defense,
//   10/11 = attack/move speed.
constexpr uintptr_t kNativeMonsterStatBlockGetterRva = 0x0200A300;
constexpr size_t kNativeMonsterStatBlockVtableOffset = 1608;
constexpr uintptr_t kNativeMonsterCurrentHpGetterRva = 0x02017340;
constexpr size_t kNativeMonsterCurrentHpGetterVtableOffset = 1052;
constexpr uintptr_t kNativeMonsterMaxHpGetterRva = 0x0200A6E0;
constexpr size_t kNativeMonsterMaxHpGetterVtableOffset = 1056;
constexpr uintptr_t kNativeMonsterHpStateGetterRva = 0x0200A330;
constexpr size_t kNativeMonsterHpStateGetterVtableOffset = 1644;
constexpr size_t kNativeMonsterHpStateOffset = 11680;
constexpr size_t kNativeMonsterEffectiveStatBlockOffset =
    4288 + (1848 * 3);
constexpr size_t kNativeMonsterMoveSpeedBaseOffset = 672;
constexpr size_t kNativeMonsterAttackSpeedBaseOffset = 688;
constexpr size_t kNativeMonsterPhysicalAttackBaseOffset = 824;
constexpr size_t kNativeMonsterPhysicalDefenseBaseOffset = 840;
constexpr size_t kNativeMonsterMagicalAttackBaseOffset = 856;
constexpr size_t kNativeMonsterMagicalDefenseBaseOffset = 872;

// sub_255A8E0/sub_255A900 prove a bounded 104-byte row per monster skill.
// Row +0 is the active remaining cooldown and row +16 is the duration scale
// used for future cooldowns.  Halving both gives the requested -50% effect
// immediately and for every later skill use.
constexpr size_t kNativeMonsterCooldownRowsBeginOffset = 4234 * 4;
constexpr size_t kNativeMonsterCooldownRowsEndOffset = 4235 * 4;
constexpr size_t kNativeMonsterCooldownRowSize = 104;
constexpr size_t kNativeMonsterCooldownRemainingOffset = 0;
constexpr size_t kNativeMonsterCooldownScaleOffset = 16;
constexpr size_t kMaximumNativeMonsterCooldownRows = 512;

constexpr uintptr_t kEncodedReadAlign0Rva = 0x030DBD20;
constexpr uintptr_t kEncodedReadAlign4Rva = 0x030DBD90;
constexpr uintptr_t kEncodedReadAlign8Rva = 0x030DBE00;
constexpr uintptr_t kEncodedReadAlign12Rva = 0x030DBE70;
constexpr uintptr_t kEncodedReadDefaultRva = 0x030DBFC0;
constexpr uintptr_t kEncodedWriteAlign0Rva = 0x030DB910;
constexpr uintptr_t kEncodedWriteAlign4Rva = 0x030DB990;
constexpr uintptr_t kEncodedWriteAlign8Rva = 0x030DBA10;
constexpr uintptr_t kEncodedWriteAlign12Rva = 0x030DBA90;
constexpr uintptr_t kEncodedWriteDefaultRva = 0x030DB7F0;
constexpr uintptr_t kEncodedInt64WriteRva = 0x030DB870;

// These two signatures belong to the current EXE's embedded Lua 5.1.5
// auxiliary runtime. The plugin calls them without patching game code.
constexpr unsigned char kLuaLNewStatePrologue[] = {
    0xE8, 0x2B, 0xE9, 0xFF, 0xFF, 0x85, 0xC0
};
constexpr unsigned char kLuaLOpenLibsPrologue[] = {
    0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x08, 0x83, 0x3D
};
constexpr unsigned char kNativeLuaFactoryPrologue[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF,
    0x68, 0x1B, 0x44, 0xCD, 0x03
};
constexpr unsigned char kNativeAiDecisionRunnerPrologue[] = {
    0x55, 0x8B, 0xEC, 0x6A, 0xFF,
    0x68, 0x88, 0xC2, 0xE2, 0x03
};

constexpr int kLuaGlobalsIndex = -10002;
constexpr int kLuaTypeNil = 0;
constexpr int kLuaTypeTable = 5;
constexpr int kLuaTypeFunction = 6;
constexpr DWORD kNativeReadyTimeoutMs = 120000;
constexpr DWORD kReloadPollMs = 1000;
constexpr LONGLONG kMaximumScriptBytes = 1024 * 1024;
constexpr size_t kMaximumLuaStates = 16;
constexpr size_t kMaximumNativeAiControllers = 256;
constexpr size_t kMaximumNativeAiPlans = 256;
constexpr ULONGLONG kNativeAiVisibleAgeMs = 5000;
constexpr DWORD kSupportedImageTimestamp = 0x5B599504;
constexpr DWORD kSupportedImageSize = 0x05FB2000;
constexpr size_t kMaximumNoticeUtf8Bytes = 2048;
constexpr size_t kMaximumNoticeCharacters = 511;
constexpr char kPluginVersion[] = "90CNLua/1.21.2";
constexpr char kCompatibilityProfile[] = "90cn-decode-bypass-v1";

enum NativeAiEntryMask : unsigned int
{
    kNativeAiEntryBeginPattern = 1u << 0,
    kNativeAiEntryProc = 1u << 1,
    kNativeAiEntryProcDamageDown = 1u << 2,
    kNativeAiEntryOnEvent = 1u << 3,
    kNativeAiEntryInit = 1u << 4,
    kNativeAiEntryEnter = 1u << 5,
    kNativeAiEntryExit = 1u << 6,
};

enum NativeAiPlanMode : unsigned int
{
    kNativeAiPlanReplace = 0,
    kNativeAiPlanOverlay = 1,
    kNativeAiPlanSmart = 2,
};

enum NativeAiPlanFieldMask : unsigned int
{
    kNativeAiPlanAction = 1u << 0,
    kNativeAiPlanMoveMethod = 1u << 1,
    kNativeAiPlanDestinationSelect = 1u << 2,
    kNativeAiPlanDestinationParam1 = 1u << 3,
    kNativeAiPlanDestinationParam2 = 1u << 4,
    kNativeAiPlanAllFields = (1u << 5) - 1,
};

enum NativeAiControllerResultKind : unsigned int
{
    kNativeAiResultDestinationSelect = 1,
    kNativeAiResultMoveMethod = 3,
};

struct LuaApi
{
    LuaLNewState newState;
    LuaLOpenLibs openLibs;
    LuaGetTop getTop;
    LuaSetTop setTop;
    LuaPushString pushString;
    LuaPushCClosure pushCClosure;
    LuaPushBoolean pushBoolean;
    LuaGetTable getTable;
    LuaCreateTable createTable;
    LuaSetTable setTable;
    LuaType type;
    LuaPCall pcall;
    LuaToString toString;
};

struct LuaStateRecord
{
    lua_State* state;
    FILETIME scriptWriteTime;
    DWORD lastPollTick;
    LONG reloadGeneration;
    bool hasWriteTime;
};

struct NativeAiControllerRecord
{
    lua_State* state;
    void* controller;
    void* owner;
    unsigned int objectKey;
    unsigned int entryMask;
    unsigned int smartRewriteCount;
    unsigned int lastOriginalAction;
    unsigned int lastSmartAction;
    unsigned int lastSmartNear;
    unsigned int lastDecisionKind;
    unsigned int decisionCount;
    unsigned int decisionOverrideCount;
    int lastNativeDecision;
    int lastAppliedDecision;
    ULONGLONG lastSmartRewriteTick;
    ULONGLONG lastDecisionTick;
    ULONGLONG lastSeenTick;
    unsigned int monsterIndex;
    unsigned int hpPercentTenths;
    __int64 currentHP;
    __int64 maxHP;
    bool hpSnapshotValid;
    bool wasHPAboveHalf;
    bool halfHealthCrossed;
    bool mutationRequestQueued;
    bool mutationAuthorized;
    bool mutationPending;
    bool mutated;
    bool mutationFailureLogged;
    bool loaded;
    bool wrapped;
};

struct NativeAiPlan
{
    unsigned int objectKey;
    unsigned int mode;
    unsigned int fieldMask;
    int action;
    int moveMethod;
    int destinationSelect;
    int destinationParam1;
    int destinationParam2;
    bool active;
};

uintptr_t g_gameBase = 0;
HMODULE g_selfModule = nullptr;
LuaApi g_lua = {};
LuaStateRecord g_states[kMaximumLuaStates] = {};
SRWLOCK g_stateLock = SRWLOCK_INIT;
DWORD g_guardTls = TLS_OUT_OF_INDEXES;
wchar_t g_scriptPath[32768] = {};
wchar_t g_aiScriptPath[32768] = {};
volatile LONG g_installStarted = 0;
volatile LONG g_reloadGeneration = 1;
volatile LONG g_missingScriptLogged = 0;
volatile LONG g_stateLimitLogged = 0;
DequeueClientEvent g_dequeueClientEvent = nullptr;
volatile LONG g_eventBridgeMissingLogged = 0;
volatile LONG g_combatBridgeMissingLogged = 0;
volatile LONG g_debugPanelBridgeMissingLogged = 0;
SRWLOCK g_nativeAiLock = SRWLOCK_INIT;
NativeAiControllerRecord
    g_nativeAiControllers[kMaximumNativeAiControllers] = {};
NativeAiPlan g_nativeAiPlans[kMaximumNativeAiPlans] = {};
NativeLuaFactory g_originalNativeLuaFactory = nullptr;
NativeAiDecisionRunner g_originalNativeAiDecisionRunner = nullptr;
volatile LONG g_nativeAiHookInstalled = 0;
volatile LONG g_nativeAiUniversalHooksInstalled = 0;
volatile LONG g_nativeAiRecordLimitLogged = 0;
volatile LONG g_nativeAiScriptMissingLogged = 0;
unsigned int g_nativeAiNextHandle = 0;
PVOID volatile g_clientDiagnosticBridge = nullptr;

void Trace(const char* format, ...)
{
    char message[1024] = {};
    va_list args;
    va_start(args, format);
    _vsnprintf_s(message, sizeof(message), _TRUNCATE, format, args);
    va_end(args);

    char line[1100] = "[90CNLua] ";
    strncat_s(line, message, _TRUNCATE);
    strncat_s(line, "\r\n", _TRUNCATE);
    OutputDebugStringA(line);

    WriteClientDiagnostic bridge = reinterpret_cast<WriteClientDiagnostic>(
        InterlockedCompareExchangePointer(
            &g_clientDiagnosticBridge, nullptr, nullptr));
    if (!bridge) {
        HMODULE transport = GetModuleHandleW(L"90CN.dll");
        WriteClientDiagnostic resolved = transport
            ? reinterpret_cast<WriteClientDiagnostic>(GetProcAddress(
                transport, "Log90CNLuaDiagnostic"))
            : nullptr;
        if (resolved) {
            PVOID prior = InterlockedCompareExchangePointer(
                &g_clientDiagnosticBridge,
                reinterpret_cast<PVOID>(resolved), nullptr);
            bridge = prior
                ? reinterpret_cast<WriteClientDiagnostic>(prior)
                : resolved;
        }
    }
    if (bridge) {
        __try {
            bridge(message, static_cast<unsigned int>(strlen(message)));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            // Diagnostics must never affect a native Lua or AI game thread.
        }
    }
}

void TraceLuaText(const char* text, size_t length)
{
    if (!text) {
        Trace("Lua log received a null value");
        return;
    }

    constexpr size_t kMaximumLogBytes = 768;
    const size_t boundedLength = length < kMaximumLogBytes
        ? length
        : kMaximumLogBytes;
    wchar_t line[1024] = L"[90CNLua][Lua] ";
    const int prefixLength = static_cast<int>(wcslen(line));
    int converted = MultiByteToWideChar(
        CP_UTF8, MB_ERR_INVALID_CHARS, text,
        static_cast<int>(boundedLength), line + prefixLength,
        static_cast<int>(_countof(line)) - prefixLength - 3);
    if (converted <= 0) {
        Trace("Lua log: %.*s", static_cast<int>(boundedLength), text);
        return;
    }
    int tail = prefixLength + converted;
    line[tail++] = L'\r';
    line[tail++] = L'\n';
    line[tail] = L'\0';
    OutputDebugStringW(line);
    Trace("Lua log: %.*s", static_cast<int>(boundedLength), text);
}

// Lua 接口：dnf90.log(text)
// 将 UTF-8 文本写入调试输出，不创建游戏目录日志。
int __cdecl NativeLuaLog(lua_State* state)
{
    if (g_lua.getTop(state) < 1) {
        Trace("dnf90.log requires one string argument");
        return 0;
    }
    size_t length = 0;
    const char* text = g_lua.toString(state, 1, &length);
    if (!text) {
        Trace("dnf90.log argument is not convertible to string");
        return 0;
    }
    TraceLuaText(text, length);
    return 0;
}

// Lua 接口：dnf90.notify(text)
// 把 UTF-8 文本转换为 UTF-16 后交给 90CN.dll 的固定容量队列；本线程不调用游戏 UI。
// 返回 true 只表示消息已入队，实际显示由 DNF 主窗口线程完成。
int __cdecl NativeLuaNotify(lua_State* state)
{
    if (g_lua.getTop(state) < 1) {
        Trace("dnf90.notify requires one string argument");
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    size_t length = 0;
    const char* text = g_lua.toString(state, 1, &length);
    if (!text || length == 0 || length > kMaximumNoticeUtf8Bytes ||
        memchr(text, '\0', length)) {
        Trace("dnf90.notify rejected invalid UTF-8 byte length=%zu", length);
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    int characters = MultiByteToWideChar(
        CP_UTF8, MB_ERR_INVALID_CHARS, text, static_cast<int>(length),
        nullptr, 0);
    if (characters <= 0 ||
        characters > static_cast<int>(kMaximumNoticeCharacters)) {
        Trace("dnf90.notify UTF-8 conversion rejected bytes=%zu chars=%d error=%lu",
            length, characters, GetLastError());
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    wchar_t wideText[kMaximumNoticeCharacters + 1] = {};
    int converted = MultiByteToWideChar(
        CP_UTF8, MB_ERR_INVALID_CHARS, text, static_cast<int>(length),
        wideText, static_cast<int>(kMaximumNoticeCharacters));
    if (converted != characters) {
        Trace("dnf90.notify UTF-8 conversion changed chars expected=%d actual=%d",
            characters, converted);
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    wideText[characters] = L'\0';

    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueueClientNotice queue = transport
        ? reinterpret_cast<QueueClientNotice>(GetProcAddress(
            transport, "Queue90CNClientNotice"))
        : nullptr;
    BOOL accepted = FALSE;
    if (queue) {
        __try {
            accepted = queue(wideText, static_cast<unsigned int>(characters));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("dnf90.notify bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    } else {
        Trace("dnf90.notify bridge export is unavailable");
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

// Lua 接口：dnf90.version()
// 返回当前插件接口版本字符串。
int __cdecl NativeLuaVersion(lua_State* state)
{
    g_lua.pushString(state, kPluginVersion);
    return 1;
}

// Lua 接口：dnf90.reload()
// 只登记重载请求；脚本在下一次原生 Lua 回调中重新加载。
int __cdecl NativeLuaReload(lua_State*)
{
    InterlockedIncrement(&g_reloadGeneration);
    return 0;
}

// Lua 接口：dnf90.is_ready()
// 返回布尔值 true，表示基础原生接口已经注册。
int __cdecl NativeLuaIsReady(lua_State* state)
{
    g_lua.pushBoolean(state, 1);
    return 1;
}

// Lua 内部接口：dnf90._clock_ms()
// 返回 Windows 单调时钟的十进制文本；Lua 包装层会把它转换为数字，供定时器调度使用。
int __cdecl NativeLuaClockMilliseconds(lua_State* state)
{
    char value[32] = {};
    sprintf_s(value, "%llu", static_cast<unsigned long long>(GetTickCount64()));
    g_lua.pushString(state, value);
    return 1;
}

// Lua 内部接口：dnf90._client_info()
// 仅返回当前进程能够可靠取得的元数据，不猜测角色、频道或游戏场景地址。
int __cdecl NativeLuaClientInfo(lua_State* state)
{
    char processId[16] = {};
    char workerThreadId[16] = {};
    char gameBase[16] = {};
    sprintf_s(processId, "%lu", GetCurrentProcessId());
    sprintf_s(workerThreadId, "%lu", GetCurrentThreadId());
    sprintf_s(gameBase, "0x%08X", static_cast<unsigned int>(g_gameBase));

    g_lua.pushString(state, processId);
    g_lua.pushString(state, workerThreadId);
    g_lua.pushString(state, gameBase);
    g_lua.pushString(state, kCompatibilityProfile);
    g_lua.pushString(state, kPluginVersion);
    return 5;
}

void PushUnsignedDecimal(lua_State* state, unsigned int value)
{
    char text[32] = {};
    sprintf_s(text, "%u", value);
    g_lua.pushString(state, text);
}

void PushSignedDecimal(lua_State* state, int value)
{
    char text[32] = {};
    sprintf_s(text, "%d", value);
    g_lua.pushString(state, text);
}

void SetLuaUnsignedTableField(
    lua_State* state, const char* name, unsigned int value)
{
    g_lua.pushString(state, name);
    PushUnsignedDecimal(state, value);
    g_lua.setTable(state, -3);
}

void SetLuaSignedTableField(
    lua_State* state, const char* name, int value)
{
    g_lua.pushString(state, name);
    PushSignedDecimal(state, value);
    g_lua.setTable(state, -3);
}

void SetLuaStringTableField(
    lua_State* state, const char* name, const char* value)
{
    g_lua.pushString(state, name);
    g_lua.pushString(state, value ? value : "");
    g_lua.setTable(state, -3);
}

bool ReadLuaUnsignedArgument(
    lua_State* state, int index, unsigned int* output)
{
    if (!output) return false;
    size_t length = 0;
    const char* value = g_lua.toString(state, index, &length);
    if (!value || length == 0 || length >= 32 ||
        memchr(value, '\0', length) || value[0] == '-') {
        return false;
    }
    char text[32] = {};
    memcpy(text, value, length);
    char* end = nullptr;
    unsigned __int64 parsed = _strtoui64(text, &end, 10);
    if (!end || end != text + length || parsed > 0xFFFFFFFFull) {
        return false;
    }
    *output = static_cast<unsigned int>(parsed);
    return true;
}

bool ReadLuaSignedArgument(lua_State* state, int index, int* output)
{
    if (!output) return false;
    size_t length = 0;
    const char* value = g_lua.toString(state, index, &length);
    if (!value || length == 0 || length >= 32 ||
        memchr(value, '\0', length)) {
        return false;
    }
    char text[32] = {};
    memcpy(text, value, length);
    char* end = nullptr;
    __int64 parsed = _strtoi64(text, &end, 10);
    if (!end || end != text + length ||
        parsed < -2147483648ll || parsed > 2147483647ll) {
        return false;
    }
    *output = static_cast<int>(parsed);
    return true;
}

bool ReadLuaUtf8Argument(
    lua_State* state, int index, char* output, size_t outputSize)
{
    if (!output || outputSize < 2) return false;
    size_t length = 0;
    const char* value = g_lua.toString(state, index, &length);
    if (!value || length == 0 || length >= outputSize ||
        memchr(value, '\0', length) ||
        MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS,
            value, static_cast<int>(length), nullptr, 0) <= 0) {
        return false;
    }
    memcpy(output, value, length);
    output[length] = '\0';
    return true;
}

// Lua 内部接口：dnf90._character_stats()
// 中文说明：读取主 DLL 从当前 EXE 已接受的 mode1/mode3 状态块中缓存的角色属性。
// 返回值：尚无快照时返回 false；成功时返回 true 以及版本、标志、HP、MP、四维和速度。
NativeAiPlan* FindNativeAiPlanLocked(unsigned int objectKey)
{
    NativeAiPlan* freePlan = nullptr;
    for (size_t index = 0; index < kMaximumNativeAiPlans; ++index) {
        NativeAiPlan& plan = g_nativeAiPlans[index];
        if (plan.objectKey == objectKey) return &plan;
        if (plan.objectKey == 0 && !freePlan) freePlan = &plan;
    }
    return freePlan;
}

int __cdecl NativeLuaAiSetPlan(lua_State* state)
{
    unsigned int objectKey = 0;
    unsigned int mode = kNativeAiPlanReplace;
    unsigned int fieldMask = kNativeAiPlanAllFields;
    int action = -1;
    int moveMethod = -1;
    int destinationSelect = -1;
    int destinationParam1 = -1;
    int destinationParam2 = -1;
    if (!ReadLuaUnsignedArgument(state, 1, &objectKey) || objectKey == 0 ||
        objectKey > 0xFFFFu ||
        !ReadLuaSignedArgument(state, 2, &action) || action < -1 ||
        action > 0xFFFF ||
        !ReadLuaSignedArgument(state, 3, &moveMethod) || moveMethod < -1 ||
        moveMethod > 2 ||
        !ReadLuaSignedArgument(state, 4, &destinationSelect) ||
        destinationSelect < -1 || destinationSelect > 100 ||
        !ReadLuaSignedArgument(state, 5, &destinationParam1) ||
        !ReadLuaSignedArgument(state, 6, &destinationParam2) ||
        !ReadLuaUnsignedArgument(state, 7, &mode) ||
        mode > kNativeAiPlanSmart ||
        !ReadLuaUnsignedArgument(state, 8, &fieldMask) ||
        fieldMask == 0 || fieldMask > kNativeAiPlanAllFields) {
        Trace("dnf90._ai_set_plan rejected invalid arguments");
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    bool accepted = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    NativeAiPlan* plan = FindNativeAiPlanLocked(objectKey);
    if (plan) {
        plan->objectKey = objectKey;
        plan->mode = mode;
        plan->fieldMask = fieldMask;
        plan->action = action;
        plan->moveMethod = moveMethod;
        plan->destinationSelect = destinationSelect;
        plan->destinationParam1 = destinationParam1;
        plan->destinationParam2 = destinationParam2;
        plan->active = true;
        accepted = true;
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    if (!accepted) {
        Trace("native AI plan limit reached limit=%zu", kMaximumNativeAiPlans);
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

int __cdecl NativeLuaAiRelease(lua_State* state)
{
    unsigned int objectKey = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &objectKey) || objectKey == 0 ||
        objectKey > 0xFFFFu) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    bool released = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiPlans; ++index) {
        if (g_nativeAiPlans[index].objectKey == objectKey) {
            g_nativeAiPlans[index] = {};
            released = true;
            break;
        }
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    g_lua.pushBoolean(state, released ? 1 : 0);
    return 1;
}

int __cdecl NativeLuaAiReleaseAll(lua_State* state)
{
    AcquireSRWLockExclusive(&g_nativeAiLock);
    memset(g_nativeAiPlans, 0, sizeof(g_nativeAiPlans));
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    g_lua.pushBoolean(state, 1);
    return 1;
}

// Called only by the Proc wrapper in a game-owned native AI Lua state.
int __cdecl NativeLuaAiGetPlan(lua_State* state)
{
    unsigned int objectKey = 0;
    NativeAiPlan selected = {};
    bool active = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (record.state == state) {
            record.lastSeenTick = GetTickCount64();
            objectKey = record.objectKey;
            break;
        }
    }
    if (objectKey != 0) {
        for (size_t index = 0; index < kMaximumNativeAiPlans; ++index) {
            const NativeAiPlan& plan = g_nativeAiPlans[index];
            if (plan.objectKey == objectKey && plan.active) {
                selected = plan;
                active = true;
                break;
            }
        }
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);

    g_lua.pushBoolean(state, active ? 1 : 0);
    PushUnsignedDecimal(state, objectKey);
    PushSignedDecimal(state, active ? selected.action : -1);
    PushSignedDecimal(state, active ? selected.moveMethod : -1);
    PushSignedDecimal(state, active ? selected.destinationSelect : -1);
    PushSignedDecimal(state, active ? selected.destinationParam1 : -1);
    PushSignedDecimal(state, active ? selected.destinationParam2 : -1);
    PushUnsignedDecimal(state,
        active ? selected.mode : kNativeAiPlanReplace);
    PushUnsignedDecimal(state, active ? selected.fieldMask : 0);
    return 9;
}

int __cdecl NativeLuaAiReportSmartAction(lua_State* state)
{
    unsigned int originalAction = 0;
    unsigned int smartAction = 0;
    unsigned int nearTarget = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &originalAction) ||
        originalAction > 0xFFFFu ||
        !ReadLuaUnsignedArgument(state, 2, &smartAction) ||
        smartAction > 0xFFFFu ||
        !ReadLuaUnsignedArgument(state, 3, &nearTarget) ||
        nearTarget > 1) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    bool recorded = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (record.state != state) continue;
        if (record.smartRewriteCount != 0xFFFFFFFFu) {
            ++record.smartRewriteCount;
        }
        record.lastOriginalAction = originalAction;
        record.lastSmartAction = smartAction;
        record.lastSmartNear = nearTarget;
        record.lastSmartRewriteTick = GetTickCount64();
        recorded = true;
        break;
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    g_lua.pushBoolean(state, recorded ? 1 : 0);
    return 1;
}

// The game recycles AI controller objects across actors and destroys an owner
// without telling this plugin, so a record's cached owner can outlive the
// monster it points at. Only the decision hook holds a pointer the game itself
// just dereferenced, so the record follows that pointer; a NULL owner clears the
// actor-scoped state instead.
void RebindNativeAiRecordOwner(NativeAiControllerRecord* record, void* owner)
{
    if (!record) return;
    if (record->owner == owner && owner != nullptr) return;
    record->owner = owner;
    record->hpSnapshotValid = false;
    record->wasHPAboveHalf = false;
    record->halfHealthCrossed = false;
    record->monsterIndex = 0;
    record->currentHP = 0;
    record->maxHP = 0;
    record->hpPercentTenths = 0;
    record->mutationRequestQueued = false;
    record->mutationAuthorized = false;
    record->mutationPending = false;
    record->mutationFailureLogged = false;
    record->mutated = false;
}

// Live 2026-09-12 20:24 (trace pid=32332): 45 first-chance 0xC0000005 faults at
// 90CNLua.dll+0x236D = `mov ecx,[edi]` reading *(owner + 0x58). The record had
// kept an owner the game had already replaced -- one sample read a UTF-16
// string pointer (0x0072006F) as the combat object, the other a wild
// 0x5FB35FE3 -- yet the actor-scoped snapshot stayed marked valid, so the Lua
// side kept requesting a boss mutation for a monster that was already gone from
// 90CN's op29 actor table (16 boss-mutation-op29-target-missing rejections).
// Refresh from the live owner and drop the snapshot when the owner is gone.
bool RefreshNativeMonsterCombatSnapshot(NativeAiControllerRecord* record, void* owner)
{
    if (!record || !owner) return false;
    __try {
        void* combat = *reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(owner) +
            kNativeAiOwnerCombatObjectOffset);
        if (!combat) return false;
        uintptr_t vtable = *reinterpret_cast<uintptr_t*>(combat);
        if (!vtable) return false;
        // A shared AI controller vtable is not sufficient to prove the shape
        // of owner +0x58. Prove the current EXE's common stat and HP entries
        // before invoking them. The +0x420 max-HP entry has no stack arguments;
        // its exact signature is part of this compatibility gate.
        NativeMonsterStatBlockGetter getStatBlock =
            *reinterpret_cast<NativeMonsterStatBlockGetter*>(
                vtable + kNativeMonsterStatBlockVtableOffset);
        if (!getStatBlock ||
            reinterpret_cast<uintptr_t>(getStatBlock) !=
                g_gameBase + kNativeMonsterStatBlockGetterRva) {
            return false;
        }
        NativeMonsterIndexGetter getIndex =
            *reinterpret_cast<NativeMonsterIndexGetter*>(vtable + 68);
        NativeMonsterCurrentHpGetter getCurrentHP =
            *reinterpret_cast<NativeMonsterCurrentHpGetter*>(
                vtable + kNativeMonsterCurrentHpGetterVtableOffset);
        NativeMonsterMaxHpGetter getMaxHP =
            *reinterpret_cast<NativeMonsterMaxHpGetter*>(
                vtable + kNativeMonsterMaxHpGetterVtableOffset);
        if (!getIndex || !getCurrentHP || !getMaxHP ||
            reinterpret_cast<uintptr_t>(getCurrentHP) !=
                g_gameBase + kNativeMonsterCurrentHpGetterRva ||
            reinterpret_cast<uintptr_t>(getMaxHP) !=
                g_gameBase + kNativeMonsterMaxHpGetterRva) {
            return false;
        }
        const int monsterIndex = getIndex(combat);
        const __int64 currentHP = getCurrentHP(combat);
        // The current EXE's +0x420 slot takes no stack arguments. Supplying
        // the old guessed (0, 1) pair leaves eight bytes on the x86 stack and
        // restores the caller's saved record pointer from the value 1.
        const __int64 maxHP = getMaxHP(combat);
        if (monsterIndex <= 0 || currentHP < 0 || maxHP <= 0 ||
            currentHP > maxHP) {
            return false;
        }
        unsigned int percentTenths = static_cast<unsigned int>(
            (static_cast<unsigned __int64>(currentHP) * 1000u) /
            static_cast<unsigned __int64>(maxHP));
        if (percentTenths > 1000) percentTenths = 1000;
        const bool aboveHalf = percentTenths > 500;
        if (record->owner == owner && record->hpSnapshotValid &&
            record->wasHPAboveHalf && !aboveHalf && currentHP > 0) {
            record->halfHealthCrossed = true;
        }
        // Rebind only after the owner's combat object proved its shape, so a
        // recycled controller can never inherit the previous monster's
        // half-health flag or mutation state.
        RebindNativeAiRecordOwner(record, owner);
        record->monsterIndex = static_cast<unsigned int>(monsterIndex);
        record->currentHP = currentHP;
        record->maxHP = maxHP;
        record->hpPercentTenths = percentTenths;
        record->wasHPAboveHalf = aboveHalf;
        record->hpSnapshotValid = true;
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        // The owner is gone (or its combat object was torn down): drop the
        // actor-scoped snapshot so the Lua side stops acting on a dead monster
        // and the next decision hook re-binds the record to a live owner.
        RebindNativeAiRecordOwner(record, nullptr);
        return false;
    }
}

uintptr_t EncodedValueReaderRva(const unsigned int* primary)
{
    if (!primary) return 0;
    switch (reinterpret_cast<uintptr_t>(primary) & 0xF) {
    case 0:
        return kEncodedReadAlign0Rva;
    case 4:
        return kEncodedReadAlign4Rva;
    case 8:
        return kEncodedReadAlign8Rva;
    case 0xC:
        return kEncodedReadAlign12Rva;
    default:
        return kEncodedReadDefaultRva;
    }
}

uintptr_t EncodedValueWriterRva(const unsigned int* primary)
{
    if (!primary) return 0;
    switch (reinterpret_cast<uintptr_t>(primary) & 0xF) {
    case 0:
        return kEncodedWriteAlign0Rva;
    case 4:
        return kEncodedWriteAlign4Rva;
    case 8:
        return kEncodedWriteAlign8Rva;
    case 0xC:
        return kEncodedWriteAlign12Rva;
    default:
        return kEncodedWriteDefaultRva;
    }
}

bool ReadNativeEncodedValue(
    const unsigned int* primary, unsigned int* value)
{
    if (!g_gameBase || !primary || !value) return false;
    const uintptr_t rva = EncodedValueReaderRva(primary);
    EncodedValueReader reader = rva
        ? reinterpret_cast<EncodedValueReader>(g_gameBase + rva)
        : nullptr;
    if (!reader) return false;
    reader(primary, primary + 1, value);
    return true;
}

bool WriteNativeEncodedValue(
    unsigned int* primary, unsigned int value)
{
    if (!g_gameBase || !primary) return false;
    const uintptr_t rva = EncodedValueWriterRva(primary);
    EncodedValueWriter writer = rva
        ? reinterpret_cast<EncodedValueWriter>(g_gameBase + rva)
        : nullptr;
    if (!writer) return false;
    writer(primary, &value, primary + 1);
    return true;
}

bool WriteNativeEncodedInt64Value(void* state, __int64 value)
{
    if (!g_gameBase || !state) return false;
    EncodedInt64Writer writer = reinterpret_cast<EncodedInt64Writer>(
        g_gameBase + kEncodedInt64WriteRva);
    writer(reinterpret_cast<unsigned int*>(state), &value,
        reinterpret_cast<unsigned int*>(
            static_cast<unsigned char*>(state) + 8));
    return true;
}

struct NativeBossMutationField
{
    unsigned int* primary;
    unsigned int original;
    unsigned int target;
};

struct NativeBossMutationCooldownRow
{
    unsigned int* remaining;
    unsigned int* scale;
    unsigned int originalRemaining;
    unsigned int targetRemaining;
    unsigned int originalScale;
    unsigned int targetScale;
};

bool ApplyNativeBossMutation(NativeAiControllerRecord* record)
{
    if (!record || !record->owner || !g_gameBase) return false;

    NativeBossMutationField fields[6] = {};
    const size_t fieldOffsets[6] = {
        kNativeMonsterAttackSpeedBaseOffset,
        kNativeMonsterMoveSpeedBaseOffset,
        kNativeMonsterPhysicalAttackBaseOffset,
        kNativeMonsterMagicalAttackBaseOffset,
        kNativeMonsterPhysicalDefenseBaseOffset,
        kNativeMonsterMagicalDefenseBaseOffset,
    };
    const unsigned int fieldMultipliers[6] = {2, 2, 3, 3, 2, 2};
    NativeBossMutationCooldownRow
        cooldownRows[kMaximumNativeMonsterCooldownRows] = {};
    size_t cooldownRowCount = 0;
    size_t writtenFields = 0;
    size_t writtenCooldownRows = 0;
    void* combat = nullptr;
    void* hpState = nullptr;
    __int64 originalHP = 0;
    __int64 maxHP = 0;
    bool hpWriteStarted = false;
    bool applied = false;
    DWORD exceptionCode = 0;

    __try {
        combat = *reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(record->owner) +
            kNativeAiOwnerCombatObjectOffset);
        if (!combat) return false;
        const uintptr_t vtable = *reinterpret_cast<uintptr_t*>(combat);
        if (!vtable) return false;

        NativeMonsterStatBlockGetter getStatBlock =
            *reinterpret_cast<NativeMonsterStatBlockGetter*>(
                vtable + kNativeMonsterStatBlockVtableOffset);
        NativeMonsterCurrentHpGetter getCurrentHP =
            *reinterpret_cast<NativeMonsterCurrentHpGetter*>(
                vtable + kNativeMonsterCurrentHpGetterVtableOffset);
        NativeMonsterMaxHpGetter getMaxHP =
            *reinterpret_cast<NativeMonsterMaxHpGetter*>(
                vtable + kNativeMonsterMaxHpGetterVtableOffset);
        NativeMonsterHpStateGetter getHPState =
            *reinterpret_cast<NativeMonsterHpStateGetter*>(
                vtable + kNativeMonsterHpStateGetterVtableOffset);
        if (!getStatBlock || !getCurrentHP || !getMaxHP || !getHPState ||
            reinterpret_cast<uintptr_t>(getStatBlock) !=
                g_gameBase + kNativeMonsterStatBlockGetterRva ||
            reinterpret_cast<uintptr_t>(getCurrentHP) !=
                g_gameBase + kNativeMonsterCurrentHpGetterRva ||
            reinterpret_cast<uintptr_t>(getMaxHP) !=
                g_gameBase + kNativeMonsterMaxHpGetterRva ||
            reinterpret_cast<uintptr_t>(getHPState) !=
                g_gameBase + kNativeMonsterHpStateGetterRva) {
            return false;
        }

        unsigned char* statBlock = static_cast<unsigned char*>(
            getStatBlock(combat, 3));
        unsigned char* expectedStatBlock =
            static_cast<unsigned char*>(combat) +
            kNativeMonsterEffectiveStatBlockOffset;
        if (!statBlock || statBlock != expectedStatBlock) return false;
        hpState = getHPState(combat);
        void* expectedHPState = static_cast<unsigned char*>(combat) +
            kNativeMonsterHpStateOffset;
        if (!hpState || hpState != expectedHPState) return false;

        for (size_t index = 0; index < 6; ++index) {
            fields[index].primary = reinterpret_cast<unsigned int*>(
                statBlock + fieldOffsets[index]);
            if (!ReadNativeEncodedValue(
                    fields[index].primary, &fields[index].original)) {
                return false;
            }
            const int signedValue = static_cast<int>(fields[index].original);
            fields[index].target = fields[index].original;
            if (signedValue > 0) {
                const unsigned __int64 multiplied =
                    static_cast<unsigned __int64>(signedValue) *
                    fieldMultipliers[index];
                if (multiplied > 0x7FFFFFFFu) return false;
                fields[index].target = static_cast<unsigned int>(multiplied);
            }
        }

        unsigned char* cooldownBegin = *reinterpret_cast<unsigned char**>(
            static_cast<unsigned char*>(combat) +
            kNativeMonsterCooldownRowsBeginOffset);
        unsigned char* cooldownEnd = *reinterpret_cast<unsigned char**>(
            static_cast<unsigned char*>(combat) +
            kNativeMonsterCooldownRowsEndOffset);
        const uintptr_t cooldownBeginAddress =
            reinterpret_cast<uintptr_t>(cooldownBegin);
        const uintptr_t cooldownEndAddress =
            reinterpret_cast<uintptr_t>(cooldownEnd);
        if ((!cooldownBegin && cooldownEnd) || (cooldownBegin && !cooldownEnd) ||
            cooldownEndAddress < cooldownBeginAddress) {
            return false;
        }
        if (cooldownBegin) {
            const size_t cooldownBytes =
                static_cast<size_t>(
                    cooldownEndAddress - cooldownBeginAddress);
            if (cooldownBytes % kNativeMonsterCooldownRowSize != 0) {
                return false;
            }
            cooldownRowCount = cooldownBytes /
                kNativeMonsterCooldownRowSize;
            if (cooldownRowCount > kMaximumNativeMonsterCooldownRows) {
                return false;
            }
        }

        for (size_t index = 0; index < cooldownRowCount; ++index) {
            unsigned char* row = cooldownBegin +
                (index * kNativeMonsterCooldownRowSize);
            NativeBossMutationCooldownRow& cooldown = cooldownRows[index];
            cooldown.remaining = reinterpret_cast<unsigned int*>(
                row + kNativeMonsterCooldownRemainingOffset);
            cooldown.scale = reinterpret_cast<unsigned int*>(
                row + kNativeMonsterCooldownScaleOffset);
            if (!ReadNativeEncodedValue(
                    cooldown.remaining, &cooldown.originalRemaining) ||
                !ReadNativeEncodedValue(
                    cooldown.scale, &cooldown.originalScale)) {
                return false;
            }

            const int remaining =
                static_cast<int>(cooldown.originalRemaining);
            cooldown.targetRemaining = cooldown.originalRemaining;
            if (remaining > 0) {
                cooldown.targetRemaining = static_cast<unsigned int>(
                    remaining / 2);
            }

            float scale = 0.0f;
            memcpy(&scale, &cooldown.originalScale, sizeof(scale));
            if (!(scale >= 0.0f) || scale > 1000000.0f) return false;
            scale *= 0.5f;
            memcpy(&cooldown.targetScale, &scale, sizeof(scale));
        }

        originalHP = getCurrentHP(combat);
        maxHP = getMaxHP(combat);
        if (originalHP <= 0 || maxHP <= 0 || originalHP > maxHP) {
            return false;
        }

        for (size_t index = 0; index < 6; ++index) {
            ++writtenFields;
            if (!WriteNativeEncodedValue(
                    fields[index].primary, fields[index].target)) {
                __leave;
            }
        }
        for (size_t index = 0; index < cooldownRowCount; ++index) {
            ++writtenCooldownRows;
            if (!WriteNativeEncodedValue(cooldownRows[index].remaining,
                    cooldownRows[index].targetRemaining) ||
                !WriteNativeEncodedValue(cooldownRows[index].scale,
                    cooldownRows[index].targetScale)) {
                __leave;
            }
        }

        hpWriteStarted = true;
        if (!WriteNativeEncodedInt64Value(hpState, maxHP)) __leave;
        if (getCurrentHP(combat) != maxHP) __leave;
        applied = true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        exceptionCode = GetExceptionCode();
    }

    if (!applied &&
        (writtenFields != 0 || writtenCooldownRows != 0 || hpWriteStarted)) {
        __try {
            if (hpWriteStarted && hpState && combat && originalHP > 0) {
                WriteNativeEncodedInt64Value(hpState, originalHP);
            }
            for (size_t index = 0;
                    index < writtenCooldownRows; ++index) {
                WriteNativeEncodedValue(cooldownRows[index].remaining,
                    cooldownRows[index].originalRemaining);
                WriteNativeEncodedValue(cooldownRows[index].scale,
                    cooldownRows[index].originalScale);
            }
            for (size_t index = 0; index < writtenFields; ++index) {
                WriteNativeEncodedValue(
                    fields[index].primary, fields[index].original);
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            if (exceptionCode == 0) exceptionCode = GetExceptionCode();
        }
    }

    if (!applied) {
        if (!record->mutationFailureLogged) {
            record->mutationFailureLogged = true;
            Trace("boss mutation apply failed monster_id=%u monster_index=%u code=0x%08lX",
                record->objectKey, record->monsterIndex, exceptionCode);
        }
        return false;
    }

    record->currentHP = maxHP;
    record->maxHP = maxHP;
    record->hpPercentTenths = 1000;
    record->hpSnapshotValid = true;
    record->wasHPAboveHalf = true;
    record->mutationFailureLogged = false;
    Trace("boss mutation attributes applied monster_id=%u monster_index=%u attack_speed=%u->%u move_speed=%u->%u physical_attack=%u->%u magical_attack=%u->%u physical_defense=%u->%u magical_defense=%u->%u cooldown_rows=%zu cooldown_scale=50%% hp=full",
        record->objectKey, record->monsterIndex,
        fields[0].original, fields[0].target,
        fields[1].original, fields[1].target,
        fields[2].original, fields[2].target,
        fields[3].original, fields[3].target,
        fields[4].original, fields[4].target,
        fields[5].original, fields[5].target,
        cooldownRowCount);
    return true;
}

int __cdecl NativeLuaAiControllers(lua_State* state)
{
    struct SnapshotRow
    {
        unsigned int objectKey;
        unsigned int ageMs;
        unsigned int takeover;
        unsigned int controllerCount;
        unsigned int stateCount;
        unsigned int nativeLua;
        unsigned int entryMask;
        unsigned int planMode;
        unsigned int fieldMask;
        unsigned int smartRewriteCount;
        unsigned int lastOriginalAction;
        unsigned int lastSmartAction;
        unsigned int lastSmartNear;
        unsigned int smartRewriteAgeMs;
        ULONGLONG lastSmartRewriteTick;
        unsigned int decisionCount;
        unsigned int decisionOverrideCount;
        unsigned int lastDecisionKind;
        int lastNativeDecision;
        int lastAppliedDecision;
        unsigned int decisionAgeMs;
        ULONGLONG lastDecisionTick;
        unsigned int monsterIndex;
        unsigned int hpPercentTenths;
        unsigned int hpSnapshotValid;
        unsigned int halfHealthCrossed;
        unsigned int mutated;
    };
    SnapshotRow rows[kMaximumNativeAiControllers] = {};
    size_t count = 0;
    ULONGLONG now = GetTickCount64();
    AcquireSRWLockShared(&g_nativeAiLock);
    for (size_t index = 0;
         index < kMaximumNativeAiControllers &&
         count < kMaximumNativeAiControllers;
         ++index) {
        const NativeAiControllerRecord& record =
            g_nativeAiControllers[index];
        if (!record.controller || record.objectKey == 0 ||
            now < record.lastSeenTick ||
            now - record.lastSeenTick > kNativeAiVisibleAgeMs) {
            continue;
        }
        size_t rowIndex = count;
        for (size_t existing = 0; existing < count; ++existing) {
            if (rows[existing].objectKey == record.objectKey) {
                rowIndex = existing;
                break;
            }
        }
        const unsigned int ageMs = static_cast<unsigned int>(
            now - record.lastSeenTick);
        if (rowIndex == count) {
            rows[rowIndex].objectKey = record.objectKey;
            rows[rowIndex].ageMs = ageMs;
            rows[rowIndex].controllerCount = 1;
            rows[rowIndex].stateCount = record.state ? 1 : 0;
            rows[rowIndex].nativeLua =
                record.state && record.wrapped ? 1 : 0;
            rows[rowIndex].entryMask = record.entryMask;
            rows[rowIndex].smartRewriteAgeMs = 0xFFFFFFFFu;
            rows[rowIndex].decisionAgeMs = 0xFFFFFFFFu;
            rows[rowIndex].monsterIndex = record.monsterIndex;
            rows[rowIndex].hpPercentTenths = record.hpPercentTenths;
            rows[rowIndex].hpSnapshotValid =
                record.hpSnapshotValid ? 1 : 0;
            rows[rowIndex].halfHealthCrossed =
                record.halfHealthCrossed ? 1 : 0;
            rows[rowIndex].mutated = record.mutated ? 1 : 0;
            ++count;
        } else {
            ++rows[rowIndex].controllerCount;
            if (record.state) ++rows[rowIndex].stateCount;
            if (record.state && record.wrapped) {
                rows[rowIndex].nativeLua = 1;
            }
            rows[rowIndex].entryMask |= record.entryMask;
            if (ageMs < rows[rowIndex].ageMs) {
                rows[rowIndex].ageMs = ageMs;
            }
            if (record.hpSnapshotValid) {
                rows[rowIndex].monsterIndex = record.monsterIndex;
                rows[rowIndex].hpPercentTenths =
                    record.hpPercentTenths;
                rows[rowIndex].hpSnapshotValid = 1;
            }
            if (record.halfHealthCrossed) {
                rows[rowIndex].halfHealthCrossed = 1;
            }
            if (record.mutated) rows[rowIndex].mutated = 1;
        }
        if (0xFFFFFFFFu - rows[rowIndex].smartRewriteCount <
            record.smartRewriteCount) {
            rows[rowIndex].smartRewriteCount = 0xFFFFFFFFu;
        } else {
            rows[rowIndex].smartRewriteCount += record.smartRewriteCount;
        }
        if (record.lastSmartRewriteTick != 0 &&
            record.lastSmartRewriteTick >=
                rows[rowIndex].lastSmartRewriteTick) {
            rows[rowIndex].lastSmartRewriteTick =
                record.lastSmartRewriteTick;
            rows[rowIndex].lastOriginalAction = record.lastOriginalAction;
            rows[rowIndex].lastSmartAction = record.lastSmartAction;
            rows[rowIndex].lastSmartNear = record.lastSmartNear;
            rows[rowIndex].smartRewriteAgeMs =
                now >= record.lastSmartRewriteTick
                    ? static_cast<unsigned int>(
                        now - record.lastSmartRewriteTick)
                    : 0;
        }
        if (0xFFFFFFFFu - rows[rowIndex].decisionCount <
            record.decisionCount) {
            rows[rowIndex].decisionCount = 0xFFFFFFFFu;
        } else {
            rows[rowIndex].decisionCount += record.decisionCount;
        }
        if (0xFFFFFFFFu - rows[rowIndex].decisionOverrideCount <
            record.decisionOverrideCount) {
            rows[rowIndex].decisionOverrideCount = 0xFFFFFFFFu;
        } else {
            rows[rowIndex].decisionOverrideCount +=
                record.decisionOverrideCount;
        }
        if (record.lastDecisionTick != 0 &&
            record.lastDecisionTick >= rows[rowIndex].lastDecisionTick) {
            rows[rowIndex].lastDecisionTick = record.lastDecisionTick;
            rows[rowIndex].lastDecisionKind = record.lastDecisionKind;
            rows[rowIndex].lastNativeDecision =
                record.lastNativeDecision;
            rows[rowIndex].lastAppliedDecision =
                record.lastAppliedDecision;
            rows[rowIndex].decisionAgeMs =
                now >= record.lastDecisionTick
                    ? static_cast<unsigned int>(
                        now - record.lastDecisionTick)
                    : 0;
        }
        for (size_t planIndex = 0;
             planIndex < kMaximumNativeAiPlans; ++planIndex) {
            if (g_nativeAiPlans[planIndex].objectKey == record.objectKey &&
                g_nativeAiPlans[planIndex].active) {
                rows[rowIndex].takeover = 1;
                rows[rowIndex].planMode = g_nativeAiPlans[planIndex].mode;
                rows[rowIndex].fieldMask =
                    g_nativeAiPlans[planIndex].fieldMask;
                break;
            }
        }
    }
    ReleaseSRWLockShared(&g_nativeAiLock);

    g_lua.createTable(state, 0, 2);
    SetLuaUnsignedTableField(
        state, "count", static_cast<unsigned int>(count));
    g_lua.pushString(state, "items");
    g_lua.createTable(state, 0, static_cast<int>(count));
    for (size_t index = 0; index < count; ++index) {
        char key[16] = {};
        sprintf_s(key, "%zu", index + 1);
        g_lua.pushString(state, key);
        g_lua.createTable(state, 0, 33);
        SetLuaUnsignedTableField(state, "monster_id", rows[index].objectKey);
        // Compatibility alias retained for scripts written against 1.15.0.
        // This is an opaque AI handle, not a global actor object key.
        SetLuaUnsignedTableField(state, "object_key", rows[index].objectKey);
        SetLuaUnsignedTableField(state, "age_ms", rows[index].ageMs);
        SetLuaUnsignedTableField(state, "takeover", rows[index].takeover);
        SetLuaUnsignedTableField(
            state, "controller_count", rows[index].controllerCount);
        SetLuaUnsignedTableField(
            state, "state_count", rows[index].stateCount);
        SetLuaUnsignedTableField(
            state, "native_lua", rows[index].nativeLua);
        SetLuaUnsignedTableField(state, "universal_control", 1);
        SetLuaUnsignedTableField(
            state, "entry_mask", rows[index].entryMask);
        SetLuaUnsignedTableField(state, "plan_mode", rows[index].planMode);
        SetLuaUnsignedTableField(state, "field_mask", rows[index].fieldMask);
        SetLuaUnsignedTableField(state, "smart_rewrite_count",
            rows[index].smartRewriteCount);
        SetLuaUnsignedTableField(state, "last_original_action",
            rows[index].lastOriginalAction);
        SetLuaUnsignedTableField(state, "last_smart_action",
            rows[index].lastSmartAction);
        SetLuaUnsignedTableField(state, "last_smart_near",
            rows[index].lastSmartNear);
        SetLuaUnsignedTableField(state, "smart_rewrite_age_ms",
            rows[index].smartRewriteAgeMs);
        SetLuaUnsignedTableField(state, "decision_count",
            rows[index].decisionCount);
        SetLuaUnsignedTableField(state, "decision_override_count",
            rows[index].decisionOverrideCount);
        SetLuaUnsignedTableField(state, "last_decision_kind",
            rows[index].lastDecisionKind);
        SetLuaSignedTableField(state, "last_native_decision",
            rows[index].lastNativeDecision);
        SetLuaSignedTableField(state, "last_applied_decision",
            rows[index].lastAppliedDecision);
        SetLuaUnsignedTableField(state, "decision_age_ms",
            rows[index].decisionAgeMs);
        SetLuaUnsignedTableField(state, "monster_index",
            rows[index].monsterIndex);
        SetLuaUnsignedTableField(state, "hp_percent_tenths",
            rows[index].hpPercentTenths);
        SetLuaUnsignedTableField(state, "hp_snapshot_valid",
            rows[index].hpSnapshotValid);
        SetLuaUnsignedTableField(state, "half_health_crossed",
            rows[index].halfHealthCrossed);
        SetLuaUnsignedTableField(state, "mutated",
            rows[index].mutated);
        SetLuaUnsignedTableField(state, "has_begin_pattern",
            (rows[index].entryMask & kNativeAiEntryBeginPattern) ? 1 : 0);
        SetLuaUnsignedTableField(state, "has_proc",
            (rows[index].entryMask & kNativeAiEntryProc) ? 1 : 0);
        SetLuaUnsignedTableField(state, "has_proc_damage_down",
            (rows[index].entryMask & kNativeAiEntryProcDamageDown) ? 1 : 0);
        SetLuaUnsignedTableField(state, "has_on_event",
            (rows[index].entryMask & kNativeAiEntryOnEvent) ? 1 : 0);
        SetLuaUnsignedTableField(state, "has_lifecycle",
            (rows[index].entryMask & (kNativeAiEntryInit |
                kNativeAiEntryEnter | kNativeAiEntryExit)) ? 1 : 0);
        g_lua.setTable(state, -3);
    }
    g_lua.setTable(state, -3);
    return 1;
}

int __cdecl NativeLuaQueueGameCommand(lua_State* state)
{
    unsigned int type = 0;
    unsigned int objectKey = 0;
    int value = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &type) ||
        !ReadLuaUnsignedArgument(state, 2, &objectKey) ||
        !ReadLuaSignedArgument(state, 3, &value)) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    DNF90GameCommand command = {};
    command.size = sizeof(command);
    command.type = type;
    command.objectKey = objectKey;
    command.value = value;

    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueueGameCommand queue = transport
        ? reinterpret_cast<QueueGameCommand>(GetProcAddress(
            transport, "Queue90CNGameCommand"))
        : nullptr;
    BOOL accepted = FALSE;
    if (queue) {
        __try {
            accepted = queue(&command, sizeof(command));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("game-command bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

int __cdecl NativeLuaRequestBossMutation(lua_State* state)
{
    unsigned int token = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &token) ||
        token == 0 || token > 0xFFFFu) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    unsigned int monsterIndex = 0;
    bool eligible = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (!record.controller || record.objectKey != token) continue;
        if (record.hpSnapshotValid && record.halfHealthCrossed &&
            record.monsterIndex != 0 && !record.mutationRequestQueued &&
            !record.mutated) {
            monsterIndex = record.monsterIndex;
            eligible = true;
        }
        break;
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    if (!eligible || monsterIndex > 0x7FFFFFFFu) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90GameCommand command = {};
    command.size = sizeof(command);
    command.type = DNF90_GAME_COMMAND_REQUEST_BOSS_MUTATION;
    command.objectKey = token;
    command.value = static_cast<int>(monsterIndex);
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueueGameCommand queue = transport
        ? reinterpret_cast<QueueGameCommand>(GetProcAddress(
            transport, "Queue90CNGameCommand"))
        : nullptr;
    BOOL accepted = FALSE;
    if (queue) {
        __try {
            accepted = queue(&command, sizeof(command));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("boss-mutation request bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    }
    if (accepted) {
        AcquireSRWLockExclusive(&g_nativeAiLock);
        for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
            NativeAiControllerRecord& record = g_nativeAiControllers[index];
            if (record.controller && record.objectKey == token &&
                record.monsterIndex == monsterIndex && !record.mutated) {
                record.mutationRequestQueued = true;
                break;
            }
        }
        ReleaseSRWLockExclusive(&g_nativeAiLock);
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

int __cdecl NativeLuaPollBossMutationDecision(lua_State* state)
{
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    DequeueBossMutationDecision dequeue = transport
        ? reinterpret_cast<DequeueBossMutationDecision>(GetProcAddress(
            transport, "Dequeue90CNBossMutationDecision"))
        : nullptr;
    DNF90BossMutationDecision decision = {};
    BOOL available = FALSE;
    if (dequeue) {
        __try {
            available = dequeue(&decision, sizeof(decision));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("boss-mutation decision bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    }
    if (!available) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (!record.controller || record.objectKey != decision.token ||
            record.monsterIndex != decision.monsterID ||
            !record.mutationRequestQueued || record.mutated) {
            continue;
        }
        record.mutationAuthorized = decision.allowed == 1;
        break;
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);

    g_lua.pushBoolean(state, 1);
    g_lua.createTable(state, 0, 7);
    SetLuaUnsignedTableField(state, "generation", decision.generation);
    SetLuaUnsignedTableField(state, "monster_id", decision.token);
    SetLuaUnsignedTableField(state, "monster_index", decision.monsterID);
    SetLuaUnsignedTableField(state, "object_key", decision.objectKey);
    SetLuaUnsignedTableField(state, "allowed", decision.allowed);
    SetLuaUnsignedTableField(state, "reason", decision.reason);
    return 2;
}

int __cdecl NativeLuaApplyBossMutation(lua_State* state)
{
    unsigned int token = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &token) ||
        token == 0 || token > 0xFFFFu) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    bool accepted = false;
    AcquireSRWLockExclusive(&g_nativeAiLock);
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (!record.controller || record.objectKey != token) continue;
        if (record.mutationRequestQueued && record.mutationAuthorized &&
            !record.mutationPending && !record.mutated) {
            record.mutationPending = true;
            record.mutationAuthorized = false;
            accepted = true;
        }
        break;
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

// Each call queues a refresh and returns the most recent completed UI-thread
// generation.  No native pointer is copied into the Lua state.
int __cdecl NativeLuaActorSnapshot(lua_State* state)
{
    unsigned int requestedObjectKey = 0;
    if (g_lua.getTop(state) >= 1 &&
        !ReadLuaUnsignedArgument(state, 1, &requestedObjectKey)) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueueGameCommand queue = transport
        ? reinterpret_cast<QueueGameCommand>(GetProcAddress(
            transport, "Queue90CNGameCommand"))
        : nullptr;
    QueryActorSnapshot query = transport
        ? reinterpret_cast<QueryActorSnapshot>(GetProcAddress(
            transport, "Query90CNActorSnapshot"))
        : nullptr;
    if (!queue || !query) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90GameCommand refresh = {};
    refresh.size = sizeof(refresh);
    refresh.type = DNF90_GAME_COMMAND_REFRESH_ACTORS;
    refresh.objectKey = requestedObjectKey;
    queue(&refresh, sizeof(refresh));

    DNF90ActorSnapshot snapshot = {};
    BOOL available = FALSE;
    __try {
        available = query(&snapshot, sizeof(snapshot));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("actor-snapshot bridge exception code=0x%08lX",
            GetExceptionCode());
    }
    if (!available || snapshot.actorCount > DNF90_ACTOR_SNAPSHOT_MAX_ACTORS) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    g_lua.pushBoolean(state, 1);
    g_lua.createTable(state, 0, 5);
    SetLuaUnsignedTableField(state, "generation", snapshot.generation);
    SetLuaUnsignedTableField(state, "ui_thread_id", snapshot.uiThreadID);
    SetLuaUnsignedTableField(state, "captured_tick", snapshot.capturedTick);
    SetLuaUnsignedTableField(state, "actor_count", snapshot.actorCount);
    g_lua.pushString(state, "actors");
    g_lua.createTable(state, 0, static_cast<int>(snapshot.actorCount));
    for (unsigned int index = 0; index < snapshot.actorCount; ++index) {
        char key[16] = {};
        sprintf_s(key, "%u", index + 1);
        const DNF90ActorSnapshotRow& row = snapshot.actors[index];
        g_lua.pushString(state, key);
        g_lua.createTable(state, 0, 5);
        SetLuaUnsignedTableField(state, "object_key", row.objectKey);
        SetLuaUnsignedTableField(state, "flags", row.flags);
        g_lua.pushString(state, "x");
        PushSignedDecimal(state, row.x);
        g_lua.setTable(state, -3);
        g_lua.pushString(state, "y");
        PushSignedDecimal(state, row.y);
        g_lua.setTable(state, -3);
        g_lua.pushString(state, "z");
        PushSignedDecimal(state, row.z);
        g_lua.setTable(state, -3);
        g_lua.setTable(state, -3);
    }
    g_lua.setTable(state, -3);
    return 2;
}

int __cdecl NativeLuaCharacterStats(lua_State* state)
{
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueryCharacterStatSnapshot query = transport
        ? reinterpret_cast<QueryCharacterStatSnapshot>(GetProcAddress(
            transport, "Query90CNCharacterStatSnapshot"))
        : nullptr;
    if (!query) {
        if (InterlockedCompareExchange(
                &g_combatBridgeMissingLogged, 1, 0) == 0) {
            Trace("combat-power character snapshot export is unavailable");
        }
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90CharacterStatSnapshot snapshot = {};
    BOOL available = FALSE;
    __try {
        available = query(&snapshot, sizeof(snapshot));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("combat-power character snapshot bridge exception code=0x%08lX",
            GetExceptionCode());
    }
    if (!available) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    InterlockedExchange(&g_combatBridgeMissingLogged, 0);

    g_lua.pushBoolean(state, 1);
    PushUnsignedDecimal(state, snapshot.generation);
    PushUnsignedDecimal(state, snapshot.validFlags);
    PushUnsignedDecimal(state, snapshot.hpMax);
    PushUnsignedDecimal(state, snapshot.mpMax);
    PushSignedDecimal(state, snapshot.strength);
    PushSignedDecimal(state, snapshot.vitality);
    PushSignedDecimal(state, snapshot.intelligence);
    PushSignedDecimal(state, snapshot.spirit);
    PushUnsignedDecimal(state, snapshot.moveSpeed);
    PushUnsignedDecimal(state, snapshot.attackSpeed);
    PushUnsignedDecimal(state, snapshot.castSpeed);
    return 12;
}

// Lua 内部接口：dnf90._equipment_snapshot()
// 中文说明：读取主 DLL 从当前 EXE 已接受的 mode1/mode3 穿戴创建列表中缓存的装备快照。
// 返回值：尚无快照时返回 false；成功时返回 true 和一个纯数据表。items 内部使用字符串序号，
//         公开 Lua 包装层会重新构造为普通的 1 起始数组，避免暴露原生栈实现细节。
int __cdecl NativeLuaEquipmentSnapshot(lua_State* state)
{
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueryEquipmentSnapshot query = transport
        ? reinterpret_cast<QueryEquipmentSnapshot>(GetProcAddress(
            transport, "Query90CNEquipmentSnapshot"))
        : nullptr;
    if (!query) {
        if (InterlockedCompareExchange(
                &g_combatBridgeMissingLogged, 1, 0) == 0) {
            Trace("combat-power equipment snapshot export is unavailable");
        }
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90EquipmentSnapshot snapshot = {};
    BOOL available = FALSE;
    __try {
        available = query(&snapshot, sizeof(snapshot));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("combat-power equipment snapshot bridge exception code=0x%08lX",
            GetExceptionCode());
    }
    if (!available ||
        snapshot.itemCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    InterlockedExchange(&g_combatBridgeMissingLogged, 0);

    g_lua.pushBoolean(state, 1);
    g_lua.createTable(state, 0, 5);
    SetLuaUnsignedTableField(state, "generation", snapshot.generation);
    SetLuaUnsignedTableField(
        state, "source_stat_generation", snapshot.sourceStatGeneration);
    SetLuaUnsignedTableField(state, "valid_flags", snapshot.validFlags);
    SetLuaUnsignedTableField(state, "item_count", snapshot.itemCount);

    g_lua.pushString(state, "items");
    g_lua.createTable(state, static_cast<int>(snapshot.itemCount), 0);
    for (unsigned int index = 0; index < snapshot.itemCount; ++index) {
        char key[16] = {};
        sprintf_s(key, "%u", index + 1);
        g_lua.pushString(state, key);
        g_lua.createTable(state, 0, 7);
        const DNF90EquippedItemSnapshot& item = snapshot.items[index];
        SetLuaUnsignedTableField(state, "item_id", item.itemID);
        SetLuaUnsignedTableField(state, "quality_seed", item.qualitySeed);
        SetLuaUnsignedTableField(state, "durability", item.durability);
        SetLuaUnsignedTableField(state, "amplify_value", item.amplifyValue);
        SetLuaUnsignedTableField(state, "actor_slot", item.actorSlot);
        SetLuaUnsignedTableField(state, "upgrade_level", item.upgradeLevel);
        SetLuaUnsignedTableField(state, "amplify_type", item.amplifyType);
        g_lua.setTable(state, -3);
    }
    g_lua.setTable(state, -3);
    return 2;
}

// Lua 内部接口：dnf90._damage_affix_snapshot()
// 中文说明：读取主 DLL 从服务端私有投影中缓存的 PVF 伤害词条；百分比单位为 0.1%。
// 返回字段：generation、valid_flags、version、white_damage_tenths、yellow_damage_tenths、
//           critical_damage_tenths、yellow_additional_tenths、critical_additional_tenths、
//           all_attack_tenths、equipped_item_count、active_set_count、pvf_equipment_score。
int __cdecl NativeLuaDamageAffixSnapshot(lua_State* state)
{
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    QueryDamageAffixSnapshot query = transport
        ? reinterpret_cast<QueryDamageAffixSnapshot>(GetProcAddress(
            transport, "Query90CNDamageAffixSnapshot"))
        : nullptr;
    if (!query) {
        if (InterlockedCompareExchange(
                &g_combatBridgeMissingLogged, 1, 0) == 0) {
            Trace("combat-power damage-affix snapshot export is unavailable");
        }
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90DamageAffixSnapshot snapshot = {};
    BOOL available = FALSE;
    __try {
        available = query(&snapshot, sizeof(snapshot));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("combat-power damage-affix snapshot bridge exception code=0x%08lX",
            GetExceptionCode());
    }
    if (!available || snapshot.equippedItemCount >
            DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS) {
        g_lua.pushBoolean(state, 0);
        return 1;
    }
    InterlockedExchange(&g_combatBridgeMissingLogged, 0);

    g_lua.pushBoolean(state, 1);
    g_lua.createTable(state, 0, 19);
    SetLuaUnsignedTableField(state, "generation", snapshot.generation);
    SetLuaUnsignedTableField(state, "valid_flags", snapshot.validFlags);
    SetLuaUnsignedTableField(state, "version", snapshot.version);
    SetLuaUnsignedTableField(state, "white_damage_tenths", snapshot.whiteDamageTenths);
    SetLuaUnsignedTableField(state, "yellow_damage_tenths", snapshot.yellowDamageTenths);
    SetLuaUnsignedTableField(state, "critical_damage_tenths", snapshot.criticalDamageTenths);
    SetLuaUnsignedTableField(state, "yellow_additional_tenths", snapshot.yellowAdditionalTenths);
    SetLuaUnsignedTableField(state, "critical_additional_tenths", snapshot.criticalAdditionalTenths);
    SetLuaUnsignedTableField(state, "all_attack_tenths", snapshot.allAttackTenths);
    SetLuaUnsignedTableField(state, "equipped_item_count", snapshot.equippedItemCount);
    SetLuaUnsignedTableField(state, "active_set_count", snapshot.activeSetCount);
    SetLuaUnsignedTableField(state, "job", snapshot.job);
    SetLuaUnsignedTableField(state, "grow_type", snapshot.growType);
    SetLuaUnsignedTableField(state, "level", snapshot.level);
    SetLuaUnsignedTableField(state, "physical_attack", snapshot.physicalAttack);
    SetLuaUnsignedTableField(state, "magical_attack", snapshot.magicalAttack);
    SetLuaUnsignedTableField(state, "independent_attack", snapshot.independentAttack);
    SetLuaUnsignedTableField(state, "pvf_equipment_score", snapshot.pvfEquipmentScore);
    SetLuaStringTableField(state, "profession", snapshot.professionUtf8);
    return 2;
}

// Lua 内部接口：dnf90._update_combat_panel(...)
// 中文说明：把 Lua 计算结果复制给主 DLL；本工作线程绝不直接创建或绘制窗口。
// 参数顺序：启用、修订号、属性版本、基础分有效、总分、基础分、装备分有效、
//           装备分、HP、MP、力量、体力、智力、精神、公式版本，随后是词条有效、
//           词条版本、白字、黄字、爆伤、黄追、爆追、所有攻击力、已穿槽数、激活套装数。
// 百分比词条统一使用 0.1% 单位。旧脚本仍可只传十四项，此时公式版本按 V1 处理；
// 新词条块必须一次传满第 16～25 项，拒绝半条状态进入主 DLL。
int __cdecl NativeLuaUpdateCombatPanel(lua_State* state)
{
    if (g_lua.getTop(state) < 14) {
        Trace("dnf90._update_combat_panel requires fourteen arguments");
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    unsigned int enabled = 0;
    unsigned int revision = 0;
    unsigned int sourceGeneration = 0;
    unsigned int baseValid = 0;
    unsigned int totalScore = 0;
    unsigned int baseScore = 0;
    unsigned int equipmentValid = 0;
    unsigned int equipmentScore = 0;
    unsigned int hpMax = 0;
    unsigned int mpMax = 0;
    int strength = 0;
    int vitality = 0;
    int intelligence = 0;
    int spirit = 0;
    unsigned int formulaVersion = 1;
    unsigned int affixValid = 0;
    unsigned int affixGeneration = 0;
    unsigned int whiteDamageTenths = 0;
    unsigned int yellowDamageTenths = 0;
    unsigned int criticalDamageTenths = 0;
    unsigned int yellowAdditionalTenths = 0;
    unsigned int criticalAdditionalTenths = 0;
    unsigned int allAttackTenths = 0;
    unsigned int equippedItemCount = 0;
    unsigned int activeSetCount = 0;
    unsigned int job = 0;
    unsigned int growType = 0;
    unsigned int level = 0;
    unsigned int physicalAttack = 0;
    unsigned int magicalAttack = 0;
    unsigned int independentAttack = 0;
    char professionUtf8[32] = "--";
    const int argumentCount = g_lua.getTop(state);
    if (!ReadLuaUnsignedArgument(state, 1, &enabled) || enabled > 1 ||
        !ReadLuaUnsignedArgument(state, 2, &revision) ||
        !ReadLuaUnsignedArgument(state, 3, &sourceGeneration) ||
        !ReadLuaUnsignedArgument(state, 4, &baseValid) || baseValid > 1 ||
        !ReadLuaUnsignedArgument(state, 5, &totalScore) ||
        !ReadLuaUnsignedArgument(state, 6, &baseScore) ||
        !ReadLuaUnsignedArgument(state, 7, &equipmentValid) ||
            equipmentValid > 1 ||
        !ReadLuaUnsignedArgument(state, 8, &equipmentScore) ||
        !ReadLuaUnsignedArgument(state, 9, &hpMax) ||
        !ReadLuaUnsignedArgument(state, 10, &mpMax) ||
        !ReadLuaSignedArgument(state, 11, &strength) ||
        !ReadLuaSignedArgument(state, 12, &vitality) ||
        !ReadLuaSignedArgument(state, 13, &intelligence) ||
        !ReadLuaSignedArgument(state, 14, &spirit) ||
        (g_lua.getTop(state) >= 15 &&
            !ReadLuaUnsignedArgument(state, 15, &formulaVersion)) ||
        (argumentCount >= 16 && argumentCount < 25) ||
        (argumentCount >= 26 && argumentCount < 32) ||
        argumentCount > 32 ||
        (argumentCount >= 25 &&
            (!ReadLuaUnsignedArgument(state, 16, &affixValid) ||
                affixValid > 1 ||
             !ReadLuaUnsignedArgument(state, 17, &affixGeneration) ||
             !ReadLuaUnsignedArgument(state, 18, &whiteDamageTenths) ||
             !ReadLuaUnsignedArgument(state, 19, &yellowDamageTenths) ||
             !ReadLuaUnsignedArgument(state, 20, &criticalDamageTenths) ||
             !ReadLuaUnsignedArgument(state, 21, &yellowAdditionalTenths) ||
             !ReadLuaUnsignedArgument(state, 22, &criticalAdditionalTenths) ||
             !ReadLuaUnsignedArgument(state, 23, &allAttackTenths) ||
             !ReadLuaUnsignedArgument(state, 24, &equippedItemCount) ||
             !ReadLuaUnsignedArgument(state, 25, &activeSetCount))) ||
        (argumentCount >= 32 &&
            (!ReadLuaUnsignedArgument(state, 26, &job) || job > 255 ||
             !ReadLuaUnsignedArgument(state, 27, &growType) || growType > 255 ||
             !ReadLuaUnsignedArgument(state, 28, &level) || level > 255 ||
             !ReadLuaUnsignedArgument(state, 29, &physicalAttack) ||
             !ReadLuaUnsignedArgument(state, 30, &magicalAttack) ||
             !ReadLuaUnsignedArgument(state, 31, &independentAttack) ||
             !ReadLuaUtf8Argument(state, 32, professionUtf8,
                 sizeof(professionUtf8)))) ||
        formulaVersion == 0) {
        Trace("dnf90._update_combat_panel rejected invalid arguments");
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90CombatPanelState panel = {};
    panel.size = sizeof(panel);
    panel.revision = revision;
    panel.sourceGeneration = sourceGeneration;
    panel.validFlags = enabled ? DNF90_COMBAT_PANEL_ENABLED : 0;
    if (baseValid) {
        panel.validFlags |= DNF90_COMBAT_PANEL_BASE_SCORE_VALID;
    }
    if (equipmentValid) {
        panel.validFlags |= DNF90_COMBAT_PANEL_EQUIPMENT_SCORE_VALID;
    }
    if (affixValid) {
        panel.validFlags |= DNF90_COMBAT_PANEL_DAMAGE_AFFIXES_VALID;
    }
    if (affixValid && argumentCount >= 32 && level > 0) {
        panel.validFlags |= DNF90_COMBAT_PANEL_IDENTITY_VALID |
            DNF90_COMBAT_PANEL_THREE_ATTACKS_VALID;
    }
    panel.formulaVersion = formulaVersion;
    panel.totalScore = totalScore;
    panel.baseAttributeScore = baseScore;
    panel.equipmentScore = equipmentScore;
    panel.hpMax = hpMax;
    panel.mpMax = mpMax;
    panel.strength = strength;
    panel.vitality = vitality;
    panel.intelligence = intelligence;
    panel.spirit = spirit;
    panel.affixGeneration = affixGeneration;
    panel.whiteDamageTenths = whiteDamageTenths;
    panel.yellowDamageTenths = yellowDamageTenths;
    panel.criticalDamageTenths = criticalDamageTenths;
    panel.yellowAdditionalTenths = yellowAdditionalTenths;
    panel.criticalAdditionalTenths = criticalAdditionalTenths;
    panel.allAttackTenths = allAttackTenths;
    panel.equippedItemCount = equippedItemCount;
    panel.activeSetCount = activeSetCount;
    panel.job = job;
    panel.growType = growType;
    panel.level = level;
    panel.physicalAttack = physicalAttack;
    panel.magicalAttack = magicalAttack;
    panel.independentAttack = independentAttack;
    memcpy(panel.professionUtf8, professionUtf8,
        sizeof(panel.professionUtf8));

    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    UpdateCombatPanel update = transport
        ? reinterpret_cast<UpdateCombatPanel>(GetProcAddress(
            transport, "Update90CNCombatPanel"))
        : nullptr;
    BOOL accepted = FALSE;
    if (update) {
        __try {
            accepted = update(&panel, sizeof(panel));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("combat-power panel bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    } else if (InterlockedCompareExchange(
            &g_combatBridgeMissingLogged, 1, 0) == 0) {
        Trace("combat-power panel update export is unavailable");
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

// Lua 内部接口：dnf90._update_debug_panel(...)
// 中文说明：提交已由现有只读快照和生命周期事件构成的调试面板状态。
// 参数顺序：启用、修订、角色有效、HP、MP、力量、智力、移速、攻速、施速、
//           装备数、战斗力有效、战斗力、事件有效、事件序号、事件类型、场景、
//           副本有效、副本、房间 X/Y、层、地图、经过房间数。
int __cdecl NativeLuaUpdateDebugPanel(lua_State* state)
{
    constexpr int kArgumentCount = 24;
    if (g_lua.getTop(state) != kArgumentCount) {
        Trace("dnf90._update_debug_panel requires %d arguments", kArgumentCount);
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    unsigned int enabled = 0;
    unsigned int revision = 0;
    unsigned int characterValid = 0;
    unsigned int hpMax = 0;
    unsigned int mpMax = 0;
    int strength = 0;
    int intelligence = 0;
    unsigned int moveSpeed = 0;
    unsigned int attackSpeed = 0;
    unsigned int castSpeed = 0;
    unsigned int equippedItemCount = 0;
    unsigned int combatValid = 0;
    unsigned int combatScore = 0;
    unsigned int eventValid = 0;
    unsigned int eventSequence = 0;
    unsigned int eventType = 0;
    unsigned int context = 0;
    unsigned int dungeonValid = 0;
    unsigned int dungeonID = 0;
    unsigned int roomX = 0;
    unsigned int roomY = 0;
    unsigned int roomLayerFlag = 0;
    unsigned int mapID = 0;
    unsigned int roomCount = 0;
    if (!ReadLuaUnsignedArgument(state, 1, &enabled) || enabled > 1 ||
        !ReadLuaUnsignedArgument(state, 2, &revision) ||
        !ReadLuaUnsignedArgument(state, 3, &characterValid) ||
            characterValid > 1 ||
        !ReadLuaUnsignedArgument(state, 4, &hpMax) ||
        !ReadLuaUnsignedArgument(state, 5, &mpMax) ||
        !ReadLuaSignedArgument(state, 6, &strength) ||
        !ReadLuaSignedArgument(state, 7, &intelligence) ||
        !ReadLuaUnsignedArgument(state, 8, &moveSpeed) ||
        !ReadLuaUnsignedArgument(state, 9, &attackSpeed) ||
        !ReadLuaUnsignedArgument(state, 10, &castSpeed) ||
        !ReadLuaUnsignedArgument(state, 11, &equippedItemCount) ||
        !ReadLuaUnsignedArgument(state, 12, &combatValid) ||
            combatValid > 1 ||
        !ReadLuaUnsignedArgument(state, 13, &combatScore) ||
        !ReadLuaUnsignedArgument(state, 14, &eventValid) || eventValid > 1 ||
        !ReadLuaUnsignedArgument(state, 15, &eventSequence) ||
        !ReadLuaUnsignedArgument(state, 16, &eventType) ||
        !ReadLuaUnsignedArgument(state, 17, &context) ||
        !ReadLuaUnsignedArgument(state, 18, &dungeonValid) || dungeonValid > 1 ||
        !ReadLuaUnsignedArgument(state, 19, &dungeonID) ||
        !ReadLuaUnsignedArgument(state, 20, &roomX) ||
        !ReadLuaUnsignedArgument(state, 21, &roomY) ||
        !ReadLuaUnsignedArgument(state, 22, &roomLayerFlag) ||
        !ReadLuaUnsignedArgument(state, 23, &mapID) ||
        !ReadLuaUnsignedArgument(state, 24, &roomCount)) {
        Trace("dnf90._update_debug_panel rejected invalid arguments");
        g_lua.pushBoolean(state, 0);
        return 1;
    }

    DNF90DebugPanelState panel = {};
    panel.size = sizeof(panel);
    panel.revision = revision;
    panel.validFlags = enabled ? DNF90_DEBUG_PANEL_ENABLED : 0;
    if (characterValid) {
        panel.validFlags |= DNF90_DEBUG_PANEL_CHARACTER_VALID;
    }
    if (dungeonValid) {
        panel.validFlags |= DNF90_DEBUG_PANEL_DUNGEON_VALID;
    }
    if (combatValid) {
        panel.validFlags |= DNF90_DEBUG_PANEL_COMBAT_SCORE_VALID;
    }
    if (eventValid) {
        panel.validFlags |= DNF90_DEBUG_PANEL_EVENT_VALID;
    }
    panel.latestEventSequence = eventSequence;
    panel.latestEventType = eventType;
    panel.context = context;
    panel.dungeonID = dungeonID;
    panel.roomX = roomX;
    panel.roomY = roomY;
    panel.roomLayerFlag = roomLayerFlag;
    panel.mapID = mapID;
    panel.roomCount = roomCount;
    panel.hpMax = hpMax;
    panel.mpMax = mpMax;
    panel.strength = strength;
    panel.intelligence = intelligence;
    panel.moveSpeed = moveSpeed;
    panel.attackSpeed = attackSpeed;
    panel.castSpeed = castSpeed;
    panel.equippedItemCount = equippedItemCount;
    panel.combatScore = combatScore;

    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    UpdateDebugPanel update = transport
        ? reinterpret_cast<UpdateDebugPanel>(GetProcAddress(
            transport, "Update90CNDebugPanel"))
        : nullptr;
    BOOL accepted = FALSE;
    if (update) {
        __try {
            accepted = update(&panel, sizeof(panel));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            Trace("debug-panel bridge exception code=0x%08lX",
                GetExceptionCode());
        }
    } else if (InterlockedCompareExchange(
            &g_debugPanelBridgeMissingLogged, 1, 0) == 0) {
        Trace("debug-panel update export is unavailable");
    }
    if (accepted) {
        InterlockedExchange(&g_debugPanelBridgeMissingLogged, 0);
    }
    g_lua.pushBoolean(state, accepted ? 1 : 0);
    return 1;
}

void SetNativeApiField(lua_State* state, const char* name, LuaCFunction function)
{
    // 调用前栈顶必须是 dnf90 表。压入键和值后，该表位于 -3。
    g_lua.pushString(state, name);
    g_lua.pushCClosure(state, function, 0);
    g_lua.setTable(state, -3);
}

bool RegisterNativeApi(lua_State* state)
{
    const int oldTop = g_lua.getTop(state);

    // 保留脚本已经添加到 dnf90 表中的其他字段，只覆盖本 DLL 所有的
    // 十个原生接口。如果 dnf90 不存在或不是表，则创建一个新表。
    g_lua.pushString(state, "dnf90");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        g_lua.pushString(state, "dnf90");
        g_lua.createTable(state, 0, 24);
        g_lua.setTable(state, kLuaGlobalsIndex);
        g_lua.pushString(state, "dnf90");
        g_lua.getTable(state, kLuaGlobalsIndex);
    }
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        Trace("failed to create the dnf90 Lua API table");
        return false;
    }

    SetNativeApiField(state, "log", &NativeLuaLog);
    SetNativeApiField(state, "notify", &NativeLuaNotify);
    SetNativeApiField(state, "version", &NativeLuaVersion);
    SetNativeApiField(state, "reload", &NativeLuaReload);
    SetNativeApiField(state, "is_ready", &NativeLuaIsReady);
    SetNativeApiField(state, "_clock_ms", &NativeLuaClockMilliseconds);
    SetNativeApiField(state, "_client_info", &NativeLuaClientInfo);
    SetNativeApiField(state, "_character_stats", &NativeLuaCharacterStats);
    SetNativeApiField(state, "_equipment_snapshot", &NativeLuaEquipmentSnapshot);
    SetNativeApiField(state, "_damage_affix_snapshot", &NativeLuaDamageAffixSnapshot);
    SetNativeApiField(state, "_update_combat_panel", &NativeLuaUpdateCombatPanel);
    SetNativeApiField(state, "_update_debug_panel", &NativeLuaUpdateDebugPanel);
    SetNativeApiField(state, "_queue_game_command", &NativeLuaQueueGameCommand);
    SetNativeApiField(state, "_request_boss_mutation", &NativeLuaRequestBossMutation);
    SetNativeApiField(state, "_poll_boss_mutation_decision", &NativeLuaPollBossMutationDecision);
    SetNativeApiField(state, "_apply_boss_mutation", &NativeLuaApplyBossMutation);
    SetNativeApiField(state, "_actor_snapshot", &NativeLuaActorSnapshot);
    SetNativeApiField(state, "_ai_controllers", &NativeLuaAiControllers);
    SetNativeApiField(state, "_ai_set_plan", &NativeLuaAiSetPlan);
    SetNativeApiField(state, "_ai_release", &NativeLuaAiRelease);
    SetNativeApiField(state, "_ai_release_all", &NativeLuaAiReleaseAll);
    g_lua.setTop(state, oldTop);
    return true;
}

bool BytesMatch(const void* address, const unsigned char* expected, size_t size)
{
    __try {
        return memcmp(address, expected, size) == 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool IsSupportedGameImage()
{
    __try {
        const auto* dos = reinterpret_cast<const IMAGE_DOS_HEADER*>(g_gameBase);
        if (!dos || dos->e_magic != IMAGE_DOS_SIGNATURE || dos->e_lfanew <= 0) {
            return false;
        }
        const auto* nt = reinterpret_cast<const IMAGE_NT_HEADERS32*>(
            g_gameBase + static_cast<uintptr_t>(dos->e_lfanew));
        return nt->Signature == IMAGE_NT_SIGNATURE &&
            nt->FileHeader.Machine == IMAGE_FILE_MACHINE_I386 &&
            nt->FileHeader.TimeDateStamp == kSupportedImageTimestamp &&
            nt->OptionalHeader.Magic == IMAGE_NT_OPTIONAL_HDR32_MAGIC &&
            nt->OptionalHeader.SizeOfImage == kSupportedImageSize;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool IsAbsolutePath(const wchar_t* path)
{
    if (!path || !path[0]) {
        return false;
    }
    if (path[0] == L'\\' && path[1] == L'\\') {
        return true;
    }
    return path[0] != L'\0' && path[1] == L':';
}

bool ResolveScriptPath()
{
    wchar_t configured[32768] = {};
    DWORD configuredLength = GetEnvironmentVariableW(
        L"DNF_LUA_SCRIPT", configured,
        static_cast<DWORD>(_countof(configured)));
    if (configuredLength >= _countof(configured)) {
        Trace("DNF_LUA_SCRIPT is too long");
        return false;
    }

    wchar_t modulePath[32768] = {};
    DWORD moduleLength = GetModuleFileNameW(
        g_selfModule, modulePath, static_cast<DWORD>(_countof(modulePath)));
    if (moduleLength == 0 || moduleLength >= _countof(modulePath)) {
        Trace("GetModuleFileNameW failed error=%lu", GetLastError());
        return false;
    }
    wchar_t* separator = wcsrchr(modulePath, L'\\');
    if (!separator) {
        Trace("plugin module path has no directory separator");
        return false;
    }
    separator[1] = L'\0';

    const wchar_t* requested = configuredLength > 0
        ? configured
        : L"lua\\init.lua";
    int written = 0;
    if (IsAbsolutePath(requested)) {
        written = _snwprintf_s(
            g_scriptPath, _countof(g_scriptPath), _TRUNCATE,
            L"%s", requested);
    } else {
        written = _snwprintf_s(
            g_scriptPath, _countof(g_scriptPath), _TRUNCATE,
            L"%s%s", modulePath, requested);
    }
    if (written < 0) {
        Trace("resolved script path is too long");
        return false;
    }
    written = _snwprintf_s(
        g_aiScriptPath, _countof(g_aiScriptPath), _TRUNCATE,
        L"%slua\\ai\\init.lua", modulePath);
    if (written < 0) {
        Trace("resolved native AI script path is too long");
        return false;
    }
    return true;
}

void TraceLuaError(lua_State* state, const char* stage)
{
    size_t length = 0;
    const char* error = g_lua.toString(state, -1, &length);
    if (!error) {
        Trace("%s failed with a non-string Lua error", stage);
        return;
    }
    const size_t bounded = length < 768 ? length : 768;
    Trace("%s failed: %.*s", stage, static_cast<int>(bounded), error);
}

bool CallDnf90Function(lua_State* state, const char* functionName,
    const char* errorStage)
{
    const int oldTop = g_lua.getTop(state);

    g_lua.pushString(state, "dnf90");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        return false;
    }

    g_lua.pushString(state, functionName);
    g_lua.getTable(state, -2);
    if (g_lua.type(state, -1) != kLuaTypeFunction) {
        g_lua.setTop(state, oldTop);
        return false;
    }

    const int result = g_lua.pcall(state, 0, 0, 0);
    if (result != 0) {
        TraceLuaError(state, errorStage);
    }
    g_lua.setTop(state, oldTop);
    return result == 0;
}

bool HasDnf90Function(lua_State* state, const char* functionName)
{
    const int oldTop = g_lua.getTop(state);
    g_lua.pushString(state, "dnf90");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        return false;
    }
    g_lua.pushString(state, functionName);
    g_lua.getTable(state, -2);
    const bool found = g_lua.type(state, -1) == kLuaTypeFunction;
    g_lua.setTop(state, oldTop);
    return found;
}

bool CallDnf90ClientEventDispatcher(lua_State* state,
    const char* eventName, const DNF90ClientEvent& event)
{
    const int oldTop = g_lua.getTop(state);
    g_lua.pushString(state, "dnf90");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        return false;
    }
    g_lua.pushString(state, "_dispatch_client_event");
    g_lua.getTable(state, -2);
    if (g_lua.type(state, -1) != kLuaTypeFunction) {
        g_lua.setTop(state, oldTop);
        return false;
    }

    char values[13][16] = {};
    const unsigned int rawValues[13] = {
        event.sequence,
        event.processID,
        event.uiThreadID,
        event.contextFlags,
        event.dungeonID,
        event.roomX,
        event.roomY,
        event.roomLayerFlag,
        event.mapID,
        event.previousRoomX,
        event.previousRoomY,
        event.previousRoomLayerFlag,
        event.previousMapID,
    };
    for (size_t index = 0; index < 13; ++index) {
        sprintf_s(values[index], "%u", rawValues[index]);
    }
    g_lua.pushString(state, eventName);
    for (size_t index = 0; index < 13; ++index) {
        g_lua.pushString(state, values[index]);
    }
    const int result = g_lua.pcall(state, 14, 0, 0);
    if (result != 0) {
        TraceLuaError(state, "client event dispatch");
    }
    g_lua.setTop(state, oldTop);
    return result == 0;
}

const char* ClientEventName(unsigned int type)
{
    switch (type) {
    case DNF90_CLIENT_EVENT_UI_READY:
        return "ui_ready";
    case DNF90_CLIENT_EVENT_UI_CLOSED:
        return "ui_closed";
    case DNF90_CLIENT_EVENT_ENTER_TOWN:
        return "enter_town";
    case DNF90_CLIENT_EVENT_ENTER_DUNGEON:
        return "enter_dungeon";
    case DNF90_CLIENT_EVENT_DUNGEON_ROOM_CHANGED:
        return "dungeon_room_changed";
    case DNF90_CLIENT_EVENT_LEAVE_DUNGEON:
        return "leave_dungeon";
    default:
        return nullptr;
    }
}

DequeueClientEvent ResolveClientEventBridge()
{
    if (g_dequeueClientEvent) return g_dequeueClientEvent;
    HMODULE transport = GetModuleHandleW(L"90CN.dll");
    if (transport) {
        g_dequeueClientEvent = reinterpret_cast<DequeueClientEvent>(
            GetProcAddress(transport, "Dequeue90CNClientEvent"));
    }
    if (!g_dequeueClientEvent &&
        InterlockedCompareExchange(&g_eventBridgeMissingLogged, 1, 0) == 0) {
        Trace("client event bridge export is unavailable");
    }
    return g_dequeueClientEvent;
}

bool ReadScriptFile(
    const wchar_t* path, const char* label,
    char** source, DWORD* sourceBytes)
{
    *source = nullptr;
    *sourceBytes = 0;

    HANDLE file = CreateFileW(path, GENERIC_READ, FILE_SHARE_READ,
        nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) {
        Trace("CreateFileW %s failed error=%lu", label, GetLastError());
        return false;
    }

    LARGE_INTEGER size = {};
    if (!GetFileSizeEx(file, &size) || size.QuadPart <= 0 ||
        size.QuadPart > kMaximumScriptBytes) {
        Trace("%s size is invalid bytes=%lld", label, size.QuadPart);
        CloseHandle(file);
        return false;
    }

    DWORD bytes = static_cast<DWORD>(size.QuadPart);
    char* buffer = reinterpret_cast<char*>(HeapAlloc(
        GetProcessHeap(), 0, static_cast<SIZE_T>(bytes) + 1));
    if (!buffer) {
        Trace("HeapAlloc script buffer failed bytes=%lu", bytes);
        CloseHandle(file);
        return false;
    }

    DWORD read = 0;
    BOOL ok = ReadFile(file, buffer, bytes, &read, nullptr);
    DWORD readError = ok ? ERROR_SUCCESS : GetLastError();
    CloseHandle(file);
    if (!ok || read != bytes) {
        Trace("ReadFile %s failed error=%lu read=%lu expected=%lu",
            label, readError, read, bytes);
        HeapFree(GetProcessHeap(), 0, buffer);
        return false;
    }
    if (memchr(buffer, '\0', bytes)) {
        Trace("%s contains an embedded NUL byte", label);
        HeapFree(GetProcessHeap(), 0, buffer);
        return false;
    }
    buffer[bytes] = '\0';
    *source = buffer;
    *sourceBytes = bytes;
    return true;
}

bool ReadScript(char** source, DWORD* sourceBytes)
{
    return ReadScriptFile(
        g_scriptPath, "script", source, sourceBytes);
}

bool RegisterNativeAiApi(lua_State* state)
{
    if (!state) return false;
    const int oldTop = g_lua.getTop(state);
    g_lua.pushString(state, "dnf90_ai");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        g_lua.pushString(state, "dnf90_ai");
        g_lua.createTable(state, 0, 2);
        g_lua.setTable(state, kLuaGlobalsIndex);
        g_lua.pushString(state, "dnf90_ai");
        g_lua.getTable(state, kLuaGlobalsIndex);
    }
    if (g_lua.type(state, -1) != kLuaTypeTable) {
        g_lua.setTop(state, oldTop);
        return false;
    }
    SetNativeApiField(state, "_get_plan", &NativeLuaAiGetPlan);
    SetNativeApiField(
        state, "_report_smart_action", &NativeLuaAiReportSmartAction);
    g_lua.setTop(state, oldTop);
    return true;
}

void* ResolveNativeAiOwner(void* controller)
{
    if (!controller) return nullptr;
    return *reinterpret_cast<void**>(
        reinterpret_cast<uintptr_t>(controller) +
        kNativeAiControllerOwnerOffset);
}

unsigned int AllocateNativeAiHandleLocked()
{
    for (unsigned int attempt = 0; attempt < 0xFFFFu; ++attempt) {
        ++g_nativeAiNextHandle;
        if (g_nativeAiNextHandle == 0 || g_nativeAiNextHandle > 0xFFFFu) {
            g_nativeAiNextHandle = 1;
        }
        bool used = false;
        for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
            if (g_nativeAiControllers[index].controller &&
                g_nativeAiControllers[index].objectKey ==
                    g_nativeAiNextHandle) {
                used = true;
                break;
            }
        }
        if (!used) return g_nativeAiNextHandle;
    }
    return 0;
}

NativeAiControllerRecord* RegisterNativeAiController(
    lua_State* state, void* controller, void* owner)
{
    NativeAiControllerRecord* freeRecord = nullptr;
    NativeAiControllerRecord* staleRecord = nullptr;
    ULONGLONG now = GetTickCount64();
    unsigned int objectKey = 0;
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        NativeAiControllerRecord& record = g_nativeAiControllers[index];
        if (record.controller && record.owner == owner &&
            record.objectKey != 0) {
            objectKey = record.objectKey;
        }
        if ((state && record.state == state) ||
            (controller && record.controller == controller)) {
            if (state) record.state = state;
            record.controller = controller;
            record.owner = owner;
            if (objectKey != 0) record.objectKey = objectKey;
            record.lastSeenTick = now;
            return &record;
        }
        if (!record.controller && !freeRecord) freeRecord = &record;
        if (record.controller && now >= record.lastSeenTick &&
            now - record.lastSeenTick > 30000 && !staleRecord) {
            staleRecord = &record;
        }
    }
    if (objectKey == 0) objectKey = AllocateNativeAiHandleLocked();
    if (objectKey == 0) return nullptr;
    NativeAiControllerRecord* record = freeRecord ? freeRecord : staleRecord;
    if (record) {
        *record = {};
        record->state = state;
        record->controller = controller;
        record->owner = owner;
        record->objectKey = objectKey;
        record->lastSeenTick = now;
    }
    return record;
}

NativeAiControllerRecord* FindNativeAiControllerLocked(void* controller)
{
    if (!controller) return nullptr;
    for (size_t index = 0; index < kMaximumNativeAiControllers; ++index) {
        if (g_nativeAiControllers[index].controller == controller) {
            return &g_nativeAiControllers[index];
        }
    }
    return nullptr;
}

int __fastcall ProxyNativeAiDecisionRunner(void* controller, void*)
{
    NativeAiDecisionRunner original = g_originalNativeAiDecisionRunner;
    const int nativeDecision = original ? original(controller) : -1;
    if (!controller) return nativeDecision;

    uintptr_t controllerVtable = 0;
    void* owner = nullptr;
    unsigned int resultKind = 0;
    __try {
        controllerVtable = *reinterpret_cast<uintptr_t*>(controller);
        owner = ResolveNativeAiOwner(controller);
        resultKind = *reinterpret_cast<unsigned int*>(
            reinterpret_cast<uintptr_t>(controller) +
            kNativeAiControllerResultKindOffset);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("universal native AI decision exception code=0x%08lX controller=%p",
            GetExceptionCode(), controller);
        return nativeDecision;
    }
    if (controllerVtable != g_gameBase + kNativeAiControllerVtableRva ||
        !owner) {
        return nativeDecision;
    }

    int appliedDecision = nativeDecision;
    const ULONGLONG now = GetTickCount64();
    AcquireSRWLockExclusive(&g_nativeAiLock);
    NativeAiControllerRecord* record =
        FindNativeAiControllerLocked(controller);
    if (!record) {
        record = RegisterNativeAiController(nullptr, controller, owner);
    }
    if (record) {
        record->lastSeenTick = now;
        // `owner` was read from this very hook invocation, so it is the actor the
        // game is driving right now; a record must never keep evaluating the
        // actor it saw first (the game recycles controller objects).
        RefreshNativeMonsterCombatSnapshot(record, owner);
        if (record->mutationPending && !record->mutated &&
            ApplyNativeBossMutation(record)) {
            void* mutatedOwner = record->owner;
            const __int64 mutatedHP = record->currentHP;
            const __int64 mutatedMaxHP = record->maxHP;
            const unsigned int mutatedHPPercentTenths =
                record->hpPercentTenths;
            for (size_t index = 0;
                    index < kMaximumNativeAiControllers; ++index) {
                NativeAiControllerRecord& sharedRecord =
                    g_nativeAiControllers[index];
                if (!sharedRecord.controller ||
                    sharedRecord.owner != mutatedOwner) {
                    continue;
                }
                sharedRecord.mutationRequestQueued = false;
                sharedRecord.mutationAuthorized = false;
                sharedRecord.mutationPending = false;
                sharedRecord.mutated = true;
                sharedRecord.mutationFailureLogged = false;
                sharedRecord.currentHP = mutatedHP;
                sharedRecord.maxHP = mutatedMaxHP;
                sharedRecord.hpPercentTenths = mutatedHPPercentTenths;
                sharedRecord.hpSnapshotValid = true;
                sharedRecord.wasHPAboveHalf = true;
                sharedRecord.halfHealthCrossed = false;
            }
        }
        record->lastDecisionTick = now;
        record->lastDecisionKind = resultKind;
        record->lastNativeDecision = nativeDecision;
        if (record->decisionCount != 0xFFFFFFFFu) {
            ++record->decisionCount;
        }

        NativeAiPlan* plan = FindNativeAiPlanLocked(record->objectKey);
        if (!record->wrapped && plan &&
            plan->objectKey == record->objectKey && plan->active) {
            if (resultKind == kNativeAiResultDestinationSelect &&
                (plan->fieldMask &
                    kNativeAiPlanDestinationSelect) != 0) {
                appliedDecision = plan->destinationSelect;
            } else if (resultKind == kNativeAiResultMoveMethod &&
                (plan->fieldMask & kNativeAiPlanMoveMethod) != 0) {
                appliedDecision = plan->moveMethod;
            } else if (resultKind != kNativeAiResultDestinationSelect &&
                resultKind != kNativeAiResultMoveMethod &&
                (plan->fieldMask & kNativeAiPlanAction) != 0) {
                appliedDecision = plan->action;
            }
        }
        record->lastAppliedDecision = appliedDecision;
        if (appliedDecision != nativeDecision &&
            record->decisionOverrideCount != 0xFFFFFFFFu) {
            ++record->decisionOverrideCount;
        }
    }
    ReleaseSRWLockExclusive(&g_nativeAiLock);
    return appliedDecision;
}

bool InstallNativeAiDecisionRunnerHook()
{
    if (InterlockedCompareExchange(
            &g_nativeAiUniversalHooksInstalled, 1, 0) != 0) {
        return g_originalNativeAiDecisionRunner != nullptr;
    }
    constexpr size_t kPatchLength = 10;
    uintptr_t target = g_gameBase + kNativeAiDecisionRunnerRva;
    if (!BytesMatch(reinterpret_cast<void*>(target),
            kNativeAiDecisionRunnerPrologue,
            sizeof(kNativeAiDecisionRunnerPrologue))) {
        Trace("universal native AI runner prologue mismatch target=0x%08X",
            static_cast<unsigned int>(target));
        return false;
    }

    unsigned char* trampoline = static_cast<unsigned char*>(VirtualAlloc(
        nullptr, kPatchLength + 5, MEM_COMMIT | MEM_RESERVE,
        PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        Trace("universal native AI trampoline allocation failed error=%lu",
            GetLastError());
        return false;
    }
    memcpy(trampoline, reinterpret_cast<void*>(target), kPatchLength);
    trampoline[kPatchLength] = 0xE9;
    *reinterpret_cast<int32_t*>(trampoline + kPatchLength + 1) =
        static_cast<int32_t>(static_cast<intptr_t>(target + kPatchLength) -
            reinterpret_cast<intptr_t>(trampoline + kPatchLength + 5));
    g_originalNativeAiDecisionRunner =
        reinterpret_cast<NativeAiDecisionRunner>(trampoline);

    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), kPatchLength,
            PAGE_EXECUTE_READWRITE, &oldProtection)) {
        Trace("universal native AI hook VirtualProtect failed error=%lu",
            GetLastError());
        g_originalNativeAiDecisionRunner = nullptr;
        VirtualFree(trampoline, 0, MEM_RELEASE);
        return false;
    }
    unsigned char jump[5] = { 0xE9 };
    *reinterpret_cast<int32_t*>(jump + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyNativeAiDecisionRunner) -
        static_cast<intptr_t>(target + 5));
    memcpy(reinterpret_cast<void*>(target), jump, sizeof(jump));
    for (size_t index = sizeof(jump); index < kPatchLength; ++index) {
        *reinterpret_cast<unsigned char*>(target + index) = 0x90;
    }
    DWORD ignored = 0;
    VirtualProtect(reinterpret_cast<void*>(target), kPatchLength,
        oldProtection, &ignored);
    FlushInstructionCache(
        GetCurrentProcess(), reinterpret_cast<void*>(target), kPatchLength);
    FlushInstructionCache(
        GetCurrentProcess(), trampoline, kPatchLength + 5);
    Trace("universal native AI decision hook installed target=0x%08X trampoline=%p",
        static_cast<unsigned int>(target), trampoline);
    return true;
}

int NativeAiGlobalType(lua_State* state, const char* name)
{
    if (!state || !name || !name[0]) return kLuaTypeNil;
    const int oldTop = g_lua.getTop(state);
    g_lua.pushString(state, name);
    g_lua.getTable(state, kLuaGlobalsIndex);
    const int type = g_lua.type(state, -1);
    g_lua.setTop(state, oldTop);
    return type;
}

unsigned int DetectNativeAiEntryMask(lua_State* state)
{
    struct Entry
    {
        const char* name;
        unsigned int mask;
    };
    const Entry entries[] = {
        {"begin_pattern", kNativeAiEntryBeginPattern},
        {"Proc", kNativeAiEntryProc},
        {"ProcDamage_Down", kNativeAiEntryProcDamageDown},
        {"OnEvent", kNativeAiEntryOnEvent},
        {"Init", kNativeAiEntryInit},
        {"Enter", kNativeAiEntryEnter},
        {"Exit", kNativeAiEntryExit},
    };
    unsigned int mask = 0;
    for (const Entry& entry : entries) {
        if (NativeAiGlobalType(state, entry.name) == kLuaTypeFunction) {
            mask |= entry.mask;
        }
    }
    return mask;
}

bool ExecuteNativeAiScript(lua_State* state, DWORD* loadedBytes)
{
    char* source = nullptr;
    DWORD sourceBytes = 0;
    if (!ReadScriptFile(
            g_aiScriptPath, "native AI script", &source, &sourceBytes)) {
        if (InterlockedCompareExchange(
                &g_nativeAiScriptMissingLogged, 1, 0) == 0) {
            Trace("native AI injection is disabled until lua\\ai\\init.lua is present");
        }
        return false;
    }
    InterlockedExchange(&g_nativeAiScriptMissingLogged, 0);

    const int oldTop = g_lua.getTop(state);
    bool success = false;
    if (RegisterNativeAiApi(state)) {
        g_lua.pushString(state, "loadstring");
        g_lua.getTable(state, kLuaGlobalsIndex);
        if (g_lua.type(state, -1) == kLuaTypeFunction) {
            g_lua.pushString(state, source);
            int result = g_lua.pcall(state, 1, 2, 0);
            if (result != 0) {
                TraceLuaError(state, "native AI loadstring");
            } else if (g_lua.type(state, -2) != kLuaTypeFunction) {
                TraceLuaError(state, "native AI compile");
            } else {
                g_lua.setTop(state, -2);
                result = g_lua.pcall(state, 0, 0, 0);
                if (result != 0) {
                    TraceLuaError(state, "native AI script");
                } else {
                    success = true;
                }
            }
        }
    }
    g_lua.setTop(state, oldTop);
    HeapFree(GetProcessHeap(), 0, source);
    if (success && loadedBytes) *loadedBytes = sourceBytes;
    return success;
}

void* __fastcall ProxyNativeLuaFactory(
    void* manager, void*, void* controller, const wchar_t* sourcePath,
    const wchar_t* resolvedPath)
{
    NativeLuaFactory original = g_originalNativeLuaFactory;
    void* wrapper = original
        ? original(manager, controller, sourcePath, resolvedPath)
        : nullptr;
    if (!wrapper || !controller) return wrapper;

    __try {
        uintptr_t controllerVtable = *reinterpret_cast<uintptr_t*>(controller);
        if (controllerVtable !=
            g_gameBase + kNativeAiControllerVtableRva) {
            return wrapper;
        }
        lua_State* state = *reinterpret_cast<lua_State**>(
            reinterpret_cast<uintptr_t>(wrapper) + sizeof(uintptr_t));
        void* owner = ResolveNativeAiOwner(controller);
        if (!state || !owner) {
            Trace("native AI Lua factory skipped controller=%p owner=%p wrapper=%p state=%p",
                controller, owner, wrapper, state);
            return wrapper;
        }

        AcquireSRWLockExclusive(&g_nativeAiLock);
        NativeAiControllerRecord* record = RegisterNativeAiController(
            state, controller, owner);
        ReleaseSRWLockExclusive(&g_nativeAiLock);
        if (!record) {
            if (InterlockedCompareExchange(
                    &g_nativeAiRecordLimitLogged, 1, 0) == 0) {
                Trace("native AI controller limit reached limit=%zu",
                    kMaximumNativeAiControllers);
            }
            return wrapper;
        }
        const unsigned int objectKey = record->objectKey;

        const unsigned int entryMask = DetectNativeAiEntryMask(state);
        DWORD loadedBytes = 0;
        const bool loaded = ExecuteNativeAiScript(state, &loadedBytes);
        const bool wrapped = loaded && NativeAiGlobalType(
            state, "__dnf90_ai_entry_wrapped") == kLuaTypeTable;
        AcquireSRWLockExclusive(&g_nativeAiLock);
        record->entryMask = entryMask;
        record->loaded = loaded;
        record->wrapped = wrapped;
        record->lastSeenTick = GetTickCount64();
        ReleaseSRWLockExclusive(&g_nativeAiLock);
        void* combatObject = *reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(owner) +
            kNativeAiOwnerCombatObjectOffset);
        Trace("native AI state captured controller=%p owner=%p combat=%p state=%p monster_id=%u entry_mask=0x%02X loaded=%d wrapped=%d bytes=%lu",
            controller, owner, combatObject, state, objectKey,
            entryMask, loaded ? 1 : 0, wrapped ? 1 : 0, loadedBytes);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("native AI capture exception code=0x%08lX controller=%p",
            GetExceptionCode(), controller);
    }
    return wrapper;
}

bool InstallNativeAiLuaFactoryHook()
{
    if (InterlockedCompareExchange(&g_nativeAiHookInstalled, 1, 0) != 0) {
        return g_originalNativeLuaFactory != nullptr;
    }
    constexpr size_t kPatchLength = 10;
    uintptr_t target = g_gameBase + kNativeLuaFactoryRva;
    if (!BytesMatch(reinterpret_cast<void*>(target),
            kNativeLuaFactoryPrologue,
            sizeof(kNativeLuaFactoryPrologue))) {
        Trace("native AI Lua factory prologue mismatch target=0x%08X",
            static_cast<unsigned int>(target));
        return false;
    }

    unsigned char* trampoline = static_cast<unsigned char*>(VirtualAlloc(
        nullptr, kPatchLength + 5, MEM_COMMIT | MEM_RESERVE,
        PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        Trace("native AI trampoline allocation failed error=%lu",
            GetLastError());
        return false;
    }
    memcpy(trampoline, reinterpret_cast<void*>(target), kPatchLength);
    trampoline[kPatchLength] = 0xE9;
    *reinterpret_cast<int32_t*>(trampoline + kPatchLength + 1) =
        static_cast<int32_t>(static_cast<intptr_t>(target + kPatchLength) -
            reinterpret_cast<intptr_t>(trampoline + kPatchLength + 5));
    g_originalNativeLuaFactory =
        reinterpret_cast<NativeLuaFactory>(trampoline);

    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), kPatchLength,
            PAGE_EXECUTE_READWRITE, &oldProtection)) {
        Trace("native AI hook VirtualProtect failed error=%lu", GetLastError());
        g_originalNativeLuaFactory = nullptr;
        VirtualFree(trampoline, 0, MEM_RELEASE);
        return false;
    }
    unsigned char jump[5] = { 0xE9 };
    *reinterpret_cast<int32_t*>(jump + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyNativeLuaFactory) -
        static_cast<intptr_t>(target + 5));
    memcpy(reinterpret_cast<void*>(target), jump, sizeof(jump));
    for (size_t index = sizeof(jump); index < kPatchLength; ++index) {
        *reinterpret_cast<unsigned char*>(target + index) = 0x90;
    }
    DWORD ignored = 0;
    VirtualProtect(reinterpret_cast<void*>(target), kPatchLength,
        oldProtection, &ignored);
    FlushInstructionCache(
        GetCurrentProcess(), reinterpret_cast<void*>(target), kPatchLength);
    FlushInstructionCache(
        GetCurrentProcess(), trampoline, kPatchLength + 5);
    Trace("native AI Lua factory hook installed target=0x%08X trampoline=%p",
        static_cast<unsigned int>(target), trampoline);
    return true;
}

bool ExecuteScript(lua_State* state)
{
    char* source = nullptr;
    DWORD sourceBytes = 0;
    if (!ReadScript(&source, &sourceBytes)) {
        return false;
    }

    const int oldTop = g_lua.getTop(state);
    bool success = false;

    if (!RegisterNativeApi(state)) {
        g_lua.setTop(state, oldTop);
        HeapFree(GetProcessHeap(), 0, source);
        return false;
    }

    // Lua 5.1's lua_getglobal is a macro: push the key and read it from
    // LUA_GLOBALSINDEX. Request two loadstring results so syntax errors remain
    // available as the second result instead of being discarded.
    g_lua.pushString(state, "loadstring");
    g_lua.getTable(state, kLuaGlobalsIndex);
    if (g_lua.type(state, -1) != kLuaTypeFunction) {
        Trace("this Lua state does not expose loadstring");
        g_lua.setTop(state, oldTop);
        HeapFree(GetProcessHeap(), 0, source);
        return false;
    }

    g_lua.pushString(state, source);
    int result = g_lua.pcall(state, 1, 2, 0);
    if (result != 0) {
        TraceLuaError(state, "loadstring");
    } else if (g_lua.type(state, -2) != kLuaTypeFunction) {
        TraceLuaError(state, "compile");
    } else {
        // Drop loadstring's second (nil) result, leaving its compiled chunk on
        // top, then execute that chunk in the same protected Lua call.
        g_lua.setTop(state, -2);
        result = g_lua.pcall(state, 0, 0, 0);
        if (result != 0) {
            TraceLuaError(state, "script");
        } else {
            success = true;
            Trace("script loaded bytes=%lu state=%p thread=%lu",
                sourceBytes, state, GetCurrentThreadId());
            CallDnf90Function(state, "on_reload", "dnf90.on_reload");
        }
    }

    g_lua.setTop(state, oldTop);
    HeapFree(GetProcessHeap(), 0, source);
    return success;
}

LuaStateRecord* FindOrCreateState(lua_State* state)
{
    LuaStateRecord* freeRecord = nullptr;
    for (size_t index = 0; index < kMaximumLuaStates; ++index) {
        if (g_states[index].state == state) {
            return &g_states[index];
        }
        if (!g_states[index].state && !freeRecord) {
            freeRecord = &g_states[index];
        }
    }
    if (freeRecord) {
        freeRecord->state = state;
        freeRecord->reloadGeneration = 0;
        freeRecord->hasWriteTime = false;
    }
    return freeRecord;
}

void MaybeReloadScript(lua_State* state)
{
    if (!state || !g_scriptPath[0] || g_guardTls == TLS_OUT_OF_INDEXES) {
        return;
    }
    if (TlsGetValue(g_guardTls)) {
        return;
    }

    DWORD now = GetTickCount();
    LONG generation = InterlockedCompareExchange(&g_reloadGeneration, 0, 0);

    AcquireSRWLockExclusive(&g_stateLock);
    LuaStateRecord* record = FindOrCreateState(state);
    if (!record) {
        if (InterlockedCompareExchange(&g_stateLimitLogged, 1, 0) == 0) {
            Trace("Lua state limit reached limit=%zu", kMaximumLuaStates);
        }
        ReleaseSRWLockExclusive(&g_stateLock);
        return;
    }

    DWORD elapsed = now - record->lastPollTick;
    if (record->lastPollTick != 0 && elapsed < kReloadPollMs &&
        record->reloadGeneration == generation) {
        ReleaseSRWLockExclusive(&g_stateLock);
        return;
    }
    record->lastPollTick = now;
    ReleaseSRWLockExclusive(&g_stateLock);

    WIN32_FILE_ATTRIBUTE_DATA attributes = {};
    bool scriptExists = GetFileAttributesExW(
        g_scriptPath, GetFileExInfoStandard, &attributes) != FALSE;
    bool shouldLoad = false;
    AcquireSRWLockExclusive(&g_stateLock);
    if (scriptExists) {
        shouldLoad = !record->hasWriteTime ||
            CompareFileTime(&record->scriptWriteTime,
                &attributes.ftLastWriteTime) != 0 ||
            record->reloadGeneration != generation;
        if (shouldLoad) {
            record->scriptWriteTime = attributes.ftLastWriteTime;
            record->hasWriteTime = true;
            record->reloadGeneration = generation;
        }
    }
    ReleaseSRWLockExclusive(&g_stateLock);

    if (!scriptExists) {
        if (InterlockedCompareExchange(&g_missingScriptLogged, 1, 0) == 0) {
            Trace("script is not present; waiting for lua\\init.lua or DNF_LUA_SCRIPT");
        }
        return;
    }
    InterlockedExchange(&g_missingScriptLogged, 0);
    if (!shouldLoad) {
        return;
    }

    TlsSetValue(g_guardTls, reinterpret_cast<void*>(1));
    __try {
        ExecuteScript(state);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("script loader exception code=0x%08lX state=%p",
            GetExceptionCode(), state);
    }
    TlsSetValue(g_guardTls, nullptr);
}

void DispatchClientEvents(lua_State* state)
{
    if (!state || g_guardTls == TLS_OUT_OF_INDEXES || TlsGetValue(g_guardTls)) {
        return;
    }
    // Keep queued lifecycle events in 90CN.dll until a successfully loaded
    // script exposes the internal dispatcher. This prevents ui_ready from
    // being lost while init.lua is temporarily missing or has a syntax error.
    if (!HasDnf90Function(state, "_dispatch_client_event")) return;

    DequeueClientEvent dequeue = ResolveClientEventBridge();
    if (!dequeue) return;

    TlsSetValue(g_guardTls, reinterpret_cast<void*>(1));
    __try {
        for (size_t index = 0; index < 32; ++index) {
            DNF90ClientEvent event = {};
            if (!dequeue(&event, sizeof(event))) break;

            const char* eventName = ClientEventName(event.type);
            if ((event.size != DNF90_CLIENT_EVENT_V1_SIZE &&
                    event.size != sizeof(event)) || !eventName ||
                event.processID != GetCurrentProcessId() ||
                event.uiThreadID == 0) {
                Trace("client event rejected size=%u type=%u sequence=%u pid=%u ui_tid=%u",
                    event.size, event.type, event.sequence,
                    event.processID, event.uiThreadID);
                continue;
            }

            if (event.type == DNF90_CLIENT_EVENT_DUNGEON_ROOM_CHANGED ||
                event.type == DNF90_CLIENT_EVENT_LEAVE_DUNGEON ||
                event.type == DNF90_CLIENT_EVENT_ENTER_TOWN) {
                AcquireSRWLockExclusive(&g_nativeAiLock);
                memset(g_nativeAiPlans, 0, sizeof(g_nativeAiPlans));
                ReleaseSRWLockExclusive(&g_nativeAiLock);
            }

            bool dispatched = CallDnf90ClientEventDispatcher(
                state, eventName, event);
            Trace("client event dispatched name=%s sequence=%u pid=%u ui_tid=%u "
                "worker_tid=%lu callback=%d context=0x%X dungeon=%u "
                "room=%u:%u layer=%u map=%u",
                eventName, event.sequence, event.processID,
                event.uiThreadID, GetCurrentThreadId(),
                dispatched ? 1 : 0, event.contextFlags,
                event.dungeonID, event.roomX, event.roomY,
                event.roomLayerFlag, event.mapID);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("client event dispatch exception code=0x%08lX state=%p",
            GetExceptionCode(), state);
    }
    TlsSetValue(g_guardTls, nullptr);
}

void DispatchTimers(lua_State* state)
{
    if (!state || g_guardTls == TLS_OUT_OF_INDEXES || TlsGetValue(g_guardTls)) {
        return;
    }

    TlsSetValue(g_guardTls, reinterpret_cast<void*>(1));
    __try {
        CallDnf90Function(state, "_dispatch_timers", "timer dispatch");
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("timer dispatch exception code=0x%08lX state=%p",
            GetExceptionCode(), state);
    }
    TlsSetValue(g_guardTls, nullptr);
}

DWORD WINAPI InstallWorker(void*)
{
    g_gameBase = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    if (!g_gameBase || !IsSupportedGameImage()) {
        Trace("unsupported main executable profile");
        return 0;
    }
    if (!ResolveScriptPath()) {
        return 0;
    }

    g_lua = {
        reinterpret_cast<LuaLNewState>(g_gameBase + kLuaLNewStateRva),
        reinterpret_cast<LuaLOpenLibs>(g_gameBase + kLuaLOpenLibsRva),
        reinterpret_cast<LuaGetTop>(g_gameBase + kLuaGetTopRva),
        reinterpret_cast<LuaSetTop>(g_gameBase + kLuaSetTopRva),
        reinterpret_cast<LuaPushString>(g_gameBase + kLuaPushStringRva),
        reinterpret_cast<LuaPushCClosure>(g_gameBase + kLuaPushCClosureRva),
        reinterpret_cast<LuaPushBoolean>(g_gameBase + kLuaPushBooleanRva),
        reinterpret_cast<LuaGetTable>(g_gameBase + kLuaGetTableRva),
        reinterpret_cast<LuaCreateTable>(g_gameBase + kLuaCreateTableRva),
        reinterpret_cast<LuaSetTable>(g_gameBase + kLuaSetTableRva),
        reinterpret_cast<LuaType>(g_gameBase + kLuaTypeRva),
        reinterpret_cast<LuaPCall>(g_gameBase + kLuaPCallRva),
        reinterpret_cast<LuaToString>(g_gameBase + kLuaToStringRva),
    };

    g_guardTls = TlsAlloc();
    if (g_guardTls == TLS_OUT_OF_INDEXES) {
        Trace("TlsAlloc failed error=%lu", GetLastError());
        return 0;
    }

    uintptr_t newStateTarget = g_gameBase + kLuaLNewStateRva;
    uintptr_t openLibsTarget = g_gameBase + kLuaLOpenLibsRva;
    DWORD waited = 0;
    while (waited < kNativeReadyTimeoutMs) {
        if (BytesMatch(reinterpret_cast<void*>(newStateTarget),
                kLuaLNewStatePrologue, sizeof(kLuaLNewStatePrologue)) &&
            BytesMatch(reinterpret_cast<void*>(openLibsTarget),
                kLuaLOpenLibsPrologue, sizeof(kLuaLOpenLibsPrologue))) {
            break;
        }
        Sleep(250);
        waited += 250;
    }
    if (waited >= kNativeReadyTimeoutMs) {
        Trace("current EXE embedded Lua runtime was not restored before timeout");
        return 0;
    }

    bool universalAiHookInstalled = InstallNativeAiDecisionRunnerHook();
    Trace("universal native AI hook result=%d",
        universalAiHookInstalled ? 1 : 0);

    bool nativeAiHookInstalled = InstallNativeAiLuaFactoryHook();
    Trace("native AI Lua hook result=%d",
        nativeAiHookInstalled ? 1 : 0);

    lua_State* state = nullptr;
    __try {
        state = g_lua.newState();
        if (state) {
            g_lua.openLibs(state);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Trace("embedded Lua initialization exception code=0x%08lX",
            GetExceptionCode());
        return 0;
    }
    if (!state) {
        Trace("embedded luaL_newstate returned null");
        return 0;
    }
    Trace("owned Lua state ready state=%p game_base=0x%08X wait_ms=%lu",
        state, static_cast<unsigned int>(g_gameBase), waited);

    MaybeReloadScript(state);
    DispatchClientEvents(state);
    DispatchTimers(state);
    for (;;) {
        Sleep(250);
        MaybeReloadScript(state);
        DispatchClientEvents(state);
        DispatchTimers(state);
    }
}
} // namespace

BOOL WINAPI Install90CNLua()
{
    if (InterlockedCompareExchange(&g_installStarted, 1, 0) != 0) {
        return TRUE;
    }
    GetModuleHandleExW(
        GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
            GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
        reinterpret_cast<LPCWSTR>(&Install90CNLua), &g_selfModule);
    if (!g_selfModule) {
        InterlockedExchange(&g_installStarted, 0);
        return FALSE;
    }

    HANDLE worker = CreateThread(nullptr, 0, InstallWorker, nullptr, 0, nullptr);
    if (!worker) {
        InterlockedExchange(&g_installStarted, 0);
        return FALSE;
    }
    CloseHandle(worker);
    return TRUE;
}

BOOL WINAPI Request90CNLuaReload()
{
    InterlockedIncrement(&g_reloadGeneration);
    return TRUE;
}
