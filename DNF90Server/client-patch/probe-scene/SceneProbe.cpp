// SceneProbe.dll -- 独立只读探针（不属于 90CN，不上生产）
//
// 目的：拿到「孵化场房间里到底有哪些对象」的实证，用来判定
//       「多出来的门 / 前进指向」是不是上一个房间（能量阻截战 218）的
//       客户端自建几何没被拆掉。
//
// 原则（按“仔细点”的要求）：
//   1. 只读。不写任何游戏状态，不改任何字段，只读内存 + 写自己的日志文件。
//   2. 打补丁前必须校验目标字节（expected），不匹配就整个放弃并记日志，
//      绝不在版本不符的客户端上乱改。
//   3. 任何异常都不向外抛：SEH 包住每次读内存，探针自己崩掉也不能带崩客户端。
//   4. 日志写到 DNF.exe 同目录的 SceneProbe.log，不与 90CN_trace.log / RaidProbe.log 混。
//   5. 不碰 90CN 的任何文件、符号或日志，可以同时注入。
//
// 钩子目标（TARGETS 表）当前为空：注入后只加载、只写启动行，不做任何 patch。
// 目标地址必须先反汇编确认再填 —— 结构已经就位，填一行就能用。

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>

// ---------------------------------------------------------------- 日志

static CRITICAL_SECTION g_logLock;
static FILE* g_log = NULL;
static char g_logPath[MAX_PATH] = {0};

static void LogLine(const char* fmt, ...) {
    if (!g_log) return;
    EnterCriticalSection(&g_logLock);
    SYSTEMTIME st;
    GetLocalTime(&st);
    fprintf(g_log, "[%04d-%02d-%02d %02d:%02d:%02d.%03d] ",
            st.wYear, st.wMonth, st.wDay, st.wHour, st.wMinute, st.wSecond, st.wMilliseconds);
    va_list ap;
    va_start(ap, fmt);
    vfprintf(g_log, fmt, ap);
    va_end(ap);
    fputc('\n', g_log);
    fflush(g_log);              // 随时可读，不依赖退出时刷新
    LeaveCriticalSection(&g_logLock);
}

static void OpenLog(void) {
    char dir[MAX_PATH];
    GetModuleFileNameA(NULL, dir, MAX_PATH);          // 宿主进程 = DNF.exe
    char* slash = strrchr(dir, '\\');
    if (slash) *(slash + 1) = 0; else dir[0] = 0;
    _snprintf_s(g_logPath, sizeof(g_logPath), _TRUNCATE, "%sSceneProbe.log", dir);
    fopen_s(&g_log, g_logPath, "a");
}

// ---------------------------------------------------- 安全的只读内存访问

// 读内存失败一律返回 0，绝不抛异常到调用者。探针用。
static bool SafeRead(const void* addr, void* out, size_t size) {
    if (!addr || !out || size == 0) return false;
    __try {
        memcpy(out, addr, size);
        return true;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

// ------------------------------------------------------------ 内联 hook

struct InlineHook {
    const char* label;
    uintptr_t   target;      // 目标函数绝对地址
    const unsigned char* expected;  // 目标处原始字节，必须匹配才打补丁
    size_t      length;      // 被覆盖的字节数（>=5，含完整指令）
    void*       detour;
    void*       original;    // 补丁后才能填：被搬走的原始字节 + 跳回
    bool        installed;
};

// 与 90CN.cpp:11717 同一套做法：校验 expected -> VirtualProtect -> 写 E9 rel32。
// 任何一步失败都还原并记日志，返回 false，绝不半打补丁。
static bool InstallInlineHook(InlineHook& hook) {
    if (!hook.target || !hook.expected || hook.length < 5 || !hook.detour) {
        LogLine("hook %s: 参数不完整，跳过", hook.label ? hook.label : "?");
        return false;
    }
    unsigned char current[16] = {0};
    if (hook.length > sizeof(current) || !SafeRead((const void*)hook.target, current, hook.length)) {
        LogLine("hook %s: 读目标 0x%08X 失败，跳过", hook.label, (unsigned)hook.target);
        return false;
    }
    if (memcmp(current, hook.expected, hook.length) != 0) {
        LogLine("hook %s: 目标 0x%08X 字节不匹配，拒绝打补丁（客户端版本不符）",
                hook.label, (unsigned)hook.target);
        char got[3 * 16 + 1] = {0}, want[3 * 16 + 1] = {0};
        for (size_t i = 0; i < hook.length; ++i) {
            char t[8];
            _snprintf_s(t, sizeof(t), _TRUNCATE, "%02X ", current[i]);
            strcat_s(got, t);
            _snprintf_s(t, sizeof(t), _TRUNCATE, "%02X ", hook.expected[i]);
            strcat_s(want, t);
        }
        LogLine("hook %s:   实际=[%s] 期望=[%s]", hook.label, got, want);
        return false;
    }

    DWORD oldProtect = 0;
    if (!VirtualProtect((void*)hook.target, hook.length, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("hook %s: VirtualProtect 失败 error=%lu", hook.label, GetLastError());
        return false;
    }
    // 相对跳转只能覆盖 5 字节；length 更大时用 NOP 填满剩余字节。
    unsigned char* p = (unsigned char*)hook.target;
    p[0] = 0xE9;
    *(int32_t*)(p + 1) = (int32_t)((intptr_t)hook.detour - (intptr_t)(hook.target + 5));
    for (size_t i = 5; i < hook.length; ++i) p[i] = 0x90;
    DWORD ignored = 0;
    VirtualProtect((void*)hook.target, hook.length, oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), (void*)hook.target, hook.length);

    hook.installed = true;
    LogLine("hook %s: 已安装 target=0x%08X len=%u detour=0x%08X",
            hook.label, (unsigned)hook.target, (unsigned)hook.length, (unsigned)(uintptr_t)hook.detour);
    return true;
}

// --------------------------------------------------------------- 探针表

// 目标地址必须先用反汇编确认（tools\_raid_lib.py 的 dump），
// 并把该处原始字节原样抄进 expected —— 一个字节都不能猜。
static InlineHook g_hooks[] = {
    // { "scene-object-container-build", 0x0, kExpectedXxx, sizeof(kExpectedXxx), (void*)&HookXxx, nullptr, false },
};
static const size_t g_hookCount = sizeof(g_hooks) / sizeof(g_hooks[0]);

// --------------------------------------------------------------- 启动

static DWORD WINAPI ProbeThread(LPVOID) {
    LogLine("=== SceneProbe 启动 pid=%lu 目标数=%u ===",
            GetCurrentProcessId(), (unsigned)g_hookCount);
    LogLine("日志: %s", g_logPath);

    HMODULE exe = GetModuleHandleA(NULL);
    LogLine("宿主模块基址=0x%08X（DNF.exe 期望基址 0x00400000）", (unsigned)(uintptr_t)exe);
    if ((uintptr_t)exe != 0x00400000) {
        LogLine("基址不是 0x00400000：地址全部按 0x00400000 反汇编得到，本次放弃安装任何钩子。");
        return 0;
    }

    if (g_hookCount == 0) {
        LogLine("TARGETS 表为空：本次不做任何 patch（只验证注入与日志通路）。");
        return 0;
    }
    for (size_t i = 0; i < g_hookCount; ++i) InstallInlineHook(g_hooks[i]);
    return 0;
}

BOOL APIENTRY DllMain(HMODULE self, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(self);
        InitializeCriticalSection(&g_logLock);
        OpenLog();
        if (!g_log) return TRUE;                 // 打不开日志就静默退出，不影响游戏
        HANDLE t = CreateThread(NULL, 0, ProbeThread, NULL, 0, NULL);
        if (t) CloseHandle(t);
    }
    return TRUE;
}
