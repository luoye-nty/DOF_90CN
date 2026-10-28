// ===========================================================================
// CubeProbe.dll —— 装扮合成器（含梦想/绝对合成器 op1119）客户端只读探针
//
// 它是什么：
//   一个独立注入的探针模块。**不属于 90CN 工程，不修改、不链接、不依赖
//   90CN.dll / 90CNLua.dll 的任何文件或符号**，可以同时注入（见下面的
//   "与 90CN 共存" 一节）。
//
// 它解决什么问题：
//   现场已知：梦想装扮合成器（op1119 / 0x45F）按下去以后，服务端确实收到了
//   请求，但客户端"没有结果、鼠标不能动"。要判定的就是三件事：
//     ① 客户端到底把哪个包发出去了（opcode / 包体 / 调用栈）；
//     ② 客户端到底在等什么回包 —— 它有没有给某个 opcode 注册过处理器；
//     ③ 服务端真回了以后，客户端有没有去查表分发这个 opcode。
//   这三件事只看服务端日志答不出来，必须从客户端内部取证。
//
// 原则（沿用 SceneProbe / StoreDisjointProbe 的约定）：
//   1. 只读。不改游戏状态、不改包体、不塞包、不改任何字段；
//      唯一的写操作是给三个函数头写 5 字节 E9 跳转（挂自己的日志钩子）。
//   2. 打补丁前逐字节校验目标前导。不匹配就整个放弃并记日志 ——
//      绝不在版本不符的客户端上乱改。
//   3. 每次读客户端内存都用 SEH 包住，探针出错不能带崩客户端。
//   4. 日志写 DNF.exe 同目录的 CubeProbe.log，不与 90CN_trace.log 混。
//   5. 不碰 90CN 的任何文件、符号或日志。
//
// 与 90CN 共存（重要）：
//   90CN.dll 在生产客户端上已经勾了同一个函数的函数头（写 E9）。
//   本探针安装时先读目标首字节：
//     · 首字节是 E9  -> 说明前面已经有钩子（正常情况下就是 90CN）。
//                        本探针只覆盖这 5 字节，并把"链的下一跳"记成
//                        90CN 那个 E9 的目标地址；自己的钩子干完活直接
//                        jmp 过去。90CN 的行为完全不变。
//     · 首字节是原生前导（90CN 没装或已卸载）-> 把前导整指令搬到跳板，
//                        钩子干完活 jmp 跳板，跳板执行原前导再跳回函数体。
//   两种情况都在日志里写明 mode=chain / mode=native。
//   注意：本探针必须在 90CN 装好之后再注入（正常流程就是如此：90CN 随
//   ijl15 代理在进程启动时加载，本探针是人工注入）。
//
// 取证目标（RVAs，镜像基址 0x00400000；与 90CN.cpp 的常量一致）：
//   0x02FAAFE0  C2S 上层包体编码边界（sub_33AAFE0）
//               契约：ret 14h，arg0=msg(opcode) arg1=明文包体 arg2=包体长度
//   0x02FAB0A0  S2C 上层包体解码边界（sub_33AB0A0），契约同上
//   0x01E62430  分发查表 sub_2262430（__thiscall，ecx=注册表，栈上 1 个 opcode）
//               客户端每次要把一个 S2C 包交给处理器时都会走这里 ——
//               "客户端有没有为这个 opcode 注册处理器" 的最直接实证。
//   0x01E628F0  class-1 注册表单例 getter（只读调用，不打补丁）
//   0x01E62A80  class-0 注册表单例 getter（只读调用，不打补丁）
//
// 编译：client-patch\probe_cube\build_cube_probe.bat（MSBuild Win32 Release）
// ===========================================================================

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <tlhelp32.h>
#include <stdint.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>
#include <stdlib.h>
#include <share.h>

// ---------------------------------------------------------------- 客户端 RVA

static const uintptr_t kRvaC2SEncode       = 0x02FAAFE0;  // sub_33AAFE0
static const uintptr_t kRvaS2CDecode       = 0x02FAB0A0;  // sub_33AB0A0
static const uintptr_t kRvaDispatchLookup  = 0x01E62430;  // sub_2262430
static const uintptr_t kRvaClass1Registry  = 0x01E628F0;  // sub_22628F0
static const uintptr_t kRvaClass0Registry  = 0x01E62A80;  // sub_2262A80

// 目标函数头前导（必须逐字节匹配才允许打补丁）
//   C2S/S2C:  55             push ebp
//             8B EC          mov  ebp, esp
//             53             push ebx
//             8B 5D 10       mov  ebx, [ebp+0x10]   -> 共 7 字节，整指令边界
static const unsigned char kPrologueCodec[7] = { 0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x10 };
//   分发查表: 55             push ebp
//             8B EC          mov  ebp, esp
//             83 79 08 00    cmp  dword ptr [ecx+8], 0 -> 共 7 字节
static const unsigned char kPrologueLookup[7] = { 0x55, 0x8B, 0xEC, 0x83, 0x79, 0x08, 0x00 };

#define COPY_LEN_CODEC   7
#define COPY_LEN_LOOKUP  7

// -------------------------------------------------------------------- 配置

struct ProbeConfig {
    int      logAllC2S;         // 非 arm 期间也记 C2S（只有 op/len 一行，很小）
    int      logAllS2C;         // 非 arm 期间也记 S2C（默认 0，只记 watch 表）
    int      armSeconds;        // 见到 trigger opcode 后的全量抓取窗口（秒）
    int      briefBodyBytes;    // 非 arm 期间 watch 包体打印上限
    int      fullBodyBytes;     // arm / trigger 包体打印上限
    int      stackFrames;       // 调用栈帧数上限
    int      registryDumpOnStart;
    int      registryDumpDelayMs;
    int      registryScanMax;   // 注册表扫描 opcode 上限
    int      lookupResolve;     // 在分发查表钩子里把结果也查出来（只读取值）
    int      lookupLogAll;      // 所有查表都记（arm 期间强制记）
    int      lookupLogWatch;    // watch 表 opcode 的查表一定记
    int      hotkeyVk;          // 热键：立刻转储注册表 + 记一行标记（默认 F9=120）
    int      installDelayMs;    // 注入后等多久再挂钩
    unsigned trigger[16];  int triggerCount;
    unsigned watch[64];    int watchCount;
};

static ProbeConfig g_cfg;

static void ConfigDefaults(void) {
    static const unsigned kTrigger[] = { 1119, 99, 63 };
    static const unsigned kWatch[] = {
        1119, 99, 63, 80, 81, 82,                 // 合成器家族 / 强化 / 品级调整
        247, 248, 249, 250, 261,                  // 公会金库
        1100, 1112, 1128, 1133,                   // 已知有 S2C 注册的号段
        45, 46, 47, 48, 49, 50                    // 概率提示/结果窗常用号段
    };
    memset(&g_cfg, 0, sizeof(g_cfg));
    g_cfg.logAllC2S = 1;
    g_cfg.logAllS2C = 0;
    g_cfg.armSeconds = 25;
    g_cfg.briefBodyBytes = 48;
    g_cfg.fullBodyBytes = 512;
    g_cfg.stackFrames = 14;
    g_cfg.registryDumpOnStart = 1;
    g_cfg.registryDumpDelayMs = 6000;
    g_cfg.registryScanMax = 4096;
    g_cfg.lookupResolve = 1;
    g_cfg.lookupLogAll = 0;
    g_cfg.lookupLogWatch = 1;
    g_cfg.hotkeyVk = 0x78;      // VK_F9
    g_cfg.installDelayMs = 800;
    g_cfg.triggerCount = (int)(sizeof(kTrigger) / sizeof(kTrigger[0]));
    memcpy(g_cfg.trigger, kTrigger, sizeof(kTrigger));
    g_cfg.watchCount = (int)(sizeof(kWatch) / sizeof(kWatch[0]));
    memcpy(g_cfg.watch, kWatch, sizeof(kWatch));
}

static char g_exeDir[MAX_PATH] = { 0 };
static char g_iniPath[MAX_PATH] = { 0 };
static char g_logPath[MAX_PATH] = { 0 };

static int IniInt(const char* key, int fallback) {
    if (!g_iniPath[0]) return fallback;
    return (int)GetPrivateProfileIntA("probe", key, fallback, g_iniPath);
}

static int IniList(const char* key, unsigned* out, int cap, const unsigned* fallback, int fallbackCount) {
    char buf[1024] = { 0 };
    DWORD got = 0;
    if (!g_iniPath[0]) got = 0;
    else got = GetPrivateProfileStringA("probe", key, "", buf, sizeof(buf) - 1, g_iniPath);
    if (got == 0) {
        int n = fallbackCount < cap ? fallbackCount : cap;
        for (int i = 0; i < n; ++i) out[i] = fallback[i];
        return n;
    }
    int count = 0;
    const char* p = buf;
    while (*p && count < cap) {
        while (*p == ' ' || *p == '\t' || *p == ',') ++p;
        if (!*p) break;
        char* end = nullptr;
        long v = strtol(p, &end, 0);   // 支持 1119 或 0x45F
        if (end == p) break;
        out[count++] = (unsigned)v;
        p = end;
    }
    if (count == 0) {
        int n = fallbackCount < cap ? fallbackCount : cap;
        for (int i = 0; i < n; ++i) out[count++] = fallback[i];
    }
    return count;
}

static void LoadConfig(void) {
    unsigned tmp[64];
    ConfigDefaults();
    _snprintf_s(g_iniPath, sizeof(g_iniPath), _TRUNCATE, "%sCubeProbe.ini", g_exeDir);

    g_cfg.logAllC2S = IniInt("log_all_c2s", g_cfg.logAllC2S);
    g_cfg.logAllS2C = IniInt("log_all_s2c", g_cfg.logAllS2C);
    g_cfg.armSeconds = IniInt("arm_seconds", g_cfg.armSeconds);
    g_cfg.briefBodyBytes = IniInt("brief_body_bytes", g_cfg.briefBodyBytes);
    g_cfg.fullBodyBytes = IniInt("full_body_bytes", g_cfg.fullBodyBytes);
    g_cfg.stackFrames = IniInt("stack_frames", g_cfg.stackFrames);
    g_cfg.registryDumpOnStart = IniInt("registry_dump_on_start", g_cfg.registryDumpOnStart);
    g_cfg.registryDumpDelayMs = IniInt("registry_dump_delay_ms", g_cfg.registryDumpDelayMs);
    g_cfg.registryScanMax = IniInt("registry_scan_max", g_cfg.registryScanMax);
    g_cfg.lookupResolve = IniInt("lookup_resolve", g_cfg.lookupResolve);
    g_cfg.lookupLogAll = IniInt("lookup_log_all", g_cfg.lookupLogAll);
    g_cfg.lookupLogWatch = IniInt("lookup_log_watch", g_cfg.lookupLogWatch);
    g_cfg.hotkeyVk = IniInt("hotkey_vk", g_cfg.hotkeyVk);
    g_cfg.installDelayMs = IniInt("install_delay_ms", g_cfg.installDelayMs);
    if (g_cfg.armSeconds < 0) g_cfg.armSeconds = 0;
    if (g_cfg.fullBodyBytes <= 0) g_cfg.fullBodyBytes = 1;
    if (g_cfg.fullBodyBytes > 4096) g_cfg.fullBodyBytes = 4096;
    if (g_cfg.briefBodyBytes < 0) g_cfg.briefBodyBytes = 0;
    if (g_cfg.stackFrames < 0) g_cfg.stackFrames = 0;
    if (g_cfg.stackFrames > 48) g_cfg.stackFrames = 48;
    if (g_cfg.registryScanMax <= 0) g_cfg.registryScanMax = 2048;
    if (g_cfg.registryScanMax > 65536) g_cfg.registryScanMax = 65536;

    g_cfg.triggerCount = IniList("trigger_opcodes", tmp, 16, g_cfg.trigger, g_cfg.triggerCount);
    memcpy(g_cfg.trigger, tmp, sizeof(unsigned) * g_cfg.triggerCount);
    g_cfg.watchCount = IniList("watch_opcodes", tmp, 64, g_cfg.watch, g_cfg.watchCount);
    memcpy(g_cfg.watch, tmp, sizeof(unsigned) * g_cfg.watchCount);
}

static int InList(const unsigned* list, int count, unsigned value) {
    for (int i = 0; i < count; ++i) if (list[i] == value) return 1;
    return 0;
}

// -------------------------------------------------------------------- 日志

static CRITICAL_SECTION g_logLock;
static FILE* g_log = NULL;
static volatile LONG g_probeEvents = 0;

static void LogLine(const char* fmt, ...) {
    if (!g_log) return;
    EnterCriticalSection(&g_logLock);
    SYSTEMTIME st;
    GetLocalTime(&st);
    fprintf(g_log, "[%04d-%02d-%02d %02d:%02d:%02d.%03d] [tid=%lu] ",
            st.wYear, st.wMonth, st.wDay, st.wHour, st.wMinute, st.wSecond,
            st.wMilliseconds, (unsigned long)GetCurrentThreadId());
    va_list ap;
    va_start(ap, fmt);
    vfprintf(g_log, fmt, ap);
    va_end(ap);
    fputc('\n', g_log);
    fflush(g_log);
    LeaveCriticalSection(&g_logLock);
    InterlockedIncrement(&g_probeEvents);
}

static void OpenLog(void) {
    // 必须显式 _SH_DENYNO：默认 fopen 会独占日志，游戏跑着的时候就没法把
    // CubeProbe.log 拷出来了（现场取证全靠它）。
    g_log = _fsopen(g_logPath, "a", _SH_DENYNO);
}

// ------------------------------------------------------------ 安全内存访问
// 探针读客户端内存一律走这里：失败返回 false，绝不把异常抛给调用者。

static bool SafeRead(const void* src, void* dst, size_t size) {
    if (!src || !dst || size == 0) return false;
    __try {
        memcpy(dst, src, size);
        return true;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

static bool SafeReadU32(uintptr_t addr, unsigned* out) {
    return SafeRead((const void*)addr, out, sizeof(*out));
}

// --------------------------------------------------------------- 模块表
// 启动时枚举一次，之后地址->模块名 的解析就是纯内存查找，
// 不会在挂钩里碰 loader lock。

struct ModuleEntry {
    uintptr_t base;
    uintptr_t size;
    char      name[48];
};

static ModuleEntry g_modules[256];
static int g_moduleCount = 0;
static uintptr_t g_hostBase = 0;
static uintptr_t g_hostSize = 0;

static void EnumModules(void) {
    HANDLE snap = CreateToolhelp32Snapshot(TH32CS_SNAPMODULE, GetCurrentProcessId());
    if (snap == INVALID_HANDLE_VALUE) return;
    // UNICODE 构建下 SDK 只暴露 W 版，所以拿 W 结构再转成 ANSI 存进模块表。
    MODULEENTRY32W me;
    memset(&me, 0, sizeof(me));
    me.dwSize = sizeof(me);
    if (Module32FirstW(snap, &me)) {
        do {
            if (g_moduleCount >= (int)(sizeof(g_modules) / sizeof(g_modules[0]))) break;
            ModuleEntry& e = g_modules[g_moduleCount++];
            e.base = (uintptr_t)me.modBaseAddr;
            e.size = (uintptr_t)me.modBaseSize;
            char full[MAX_PATH] = { 0 };
            WideCharToMultiByte(CP_ACP, 0, me.szExePath, -1, full, sizeof(full) - 1, NULL, NULL);
            const char* slash = strrchr(full, '\\');
            _snprintf_s(e.name, sizeof(e.name), _TRUNCATE, "%s", slash ? slash + 1 : full);
        } while (Module32NextW(snap, &me));
    }
    CloseHandle(snap);

    // 主模块信息（含 SizeOfImage），用于判断"像不像代码指针"
    HMODULE host = GetModuleHandleA(NULL);
    g_hostBase = (uintptr_t)host;
    g_hostSize = 0;
    if (host) {
        const unsigned char* p = (const unsigned char*)host;
        __try {
            unsigned pe = *(const unsigned*)(p + 0x3C);
            if (pe < 0x1000 && memcmp(p + pe, "PE\0\0", 4) == 0) {
                unsigned opt = pe + 24;
                g_hostSize = *(const unsigned*)(p + opt + 56);   // SizeOfImage
            }
        } __except (EXCEPTION_EXECUTE_HANDLER) {
            g_hostSize = 0;
        }
    }
}

static const ModuleEntry* ModuleOf(uintptr_t addr) {
    for (int i = 0; i < g_moduleCount; ++i) {
        const ModuleEntry& e = g_modules[i];
        if (addr >= e.base && addr < e.base + e.size) return &e;
    }
    return NULL;
}

static void DescribeAddress(uintptr_t addr, char* out, size_t cap) {
    const ModuleEntry* m = ModuleOf(addr);
    if (m) _snprintf_s(out, cap, _TRUNCATE, "%s+0x%X", m->name, (unsigned)(addr - m->base));
    else   _snprintf_s(out, cap, _TRUNCATE, "0x%08X", (unsigned)addr);
}

static bool IsExecutableAddress(uintptr_t addr) {
    MEMORY_BASIC_INFORMATION mbi;
    if (VirtualQuery((LPCVOID)addr, &mbi, sizeof(mbi)) != sizeof(mbi)) return false;
    if (mbi.State != MEM_COMMIT) return false;
    DWORD prot = mbi.Protect & 0xFF;
    return prot == PAGE_EXECUTE || prot == PAGE_EXECUTE_READ ||
           prot == PAGE_EXECUTE_READWRITE || prot == PAGE_EXECUTE_WRITECOPY;
}

// ------------------------------------------------------------ 十六进制输出

static void AppendHex(char* out, size_t cap, const void* data, int len, int limit) {
    if (!out || cap == 0) return;
    size_t used = strlen(out);
    if (len <= 0 || !data || limit <= 0) return;
    int n = len < limit ? len : limit;
    unsigned char buf[16];
    static const char hex[] = "0123456789ABCDEF";
    for (int base = 0; base < n; ) {
        int chunk = n - base;
        if (chunk > (int)sizeof(buf)) chunk = (int)sizeof(buf);
        if (!SafeRead((const unsigned char*)data + base, buf, (size_t)chunk)) {
            if (used + 20 < cap) { strcat_s(out, cap, " <body-read-fault>"); }
            return;
        }
        for (int i = 0; i < chunk; ++i) {
            if (used + 4 >= cap) return;
            if (base != 0 || i != 0) out[used++] = ' ';
            out[used++] = hex[buf[i] >> 4];
            out[used++] = hex[buf[i] & 0x0F];
            out[used] = 0;
        }
        base += chunk;
    }
    if (used + 32 < cap && len > n) {
        char tail[48];
        _snprintf_s(tail, sizeof(tail), _TRUNCATE, " ...<truncated %d/%d>", n, len);
        strcat_s(out, cap, tail);
    }
}

// ------------------------------------------------------------- 调用栈还原
// x86 无 CFG，直接扫栈上"像 DNF.exe 代码段指针"的值最稳；
// 先按 EBP 链走一遍，再从 esp 向上原样扫一遍，两个结果合并去重。

static void AppendStack(char* out, size_t cap, uintptr_t ebp, uintptr_t esp) {
    if (g_cfg.stackFrames <= 0) return;
    uintptr_t frames[64];
    int count = 0;

    uintptr_t frame = ebp;
    for (int depth = 0; depth < 32 && count < g_cfg.stackFrames; ++depth) {
        if (frame < esp || frame > esp + 0x8000 || (frame & 3)) break;
        unsigned next = 0, ret = 0;
        if (!SafeReadU32(frame, &next) || !SafeReadU32(frame + 4, &ret)) break;
        if (next <= frame) break;
        if (ret && ModuleOf(ret) && IsExecutableAddress(ret)) frames[count++] = ret;
        frame = next;
    }
    for (uintptr_t slot = esp; slot < esp + 0x600 && count < g_cfg.stackFrames; slot += 4) {
        unsigned value = 0;
        if (!SafeReadU32(slot, &value)) break;
        if (!value) continue;
        if (!ModuleOf(value)) continue;
        if (!IsExecutableAddress(value)) continue;
        bool dup = false;
        for (int i = 0; i < count; ++i) if (frames[i] == value) { dup = true; break; }
        if (!dup) frames[count++] = value;
    }

    size_t used = strlen(out);
    if (used + 8 >= cap) return;
    strcat_s(out, cap, " stack=");
    for (int i = 0; i < count; ++i) {
        char desc[80];
        DescribeAddress(frames[i], desc, sizeof(desc));
        char piece[96];
        _snprintf_s(piece, sizeof(piece), _TRUNCATE, "[%d]%s ", i, desc);
        if (strlen(out) + strlen(piece) + 2 >= cap) break;
        strcat_s(out, cap, piece);
    }
}

// --------------------------------------------------------- 注册表 / 分发查表

typedef void* (__cdecl* RegistryFn)(void);
typedef void* (__thiscall* LookupFn)(void*, unsigned int);

static uintptr_t g_class0Registry = 0;
static uintptr_t g_class1Registry = 0;
static RegistryFn g_registryFn0 = NULL;
static RegistryFn g_registryFn1 = NULL;
static LookupFn   g_lookupFn = NULL;

static __declspec(thread) int t_inProbeLookup = 0;

static void RefreshRegistries(void) {
    __try {
        if (g_registryFn1) g_class1Registry = (uintptr_t)g_registryFn1();
        if (g_registryFn0) g_class0Registry = (uintptr_t)g_registryFn0();
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("registry singleton read exception code=0x%08X", GetExceptionCode());
    }
}

// 只读地解析一个 opcode 的处理器：先 class-1 再 class-0。
// 返回 1 命中，cls/callback/context 带回。
static int ResolveHandler(unsigned opcode, int* cls, uintptr_t* callback, uintptr_t* context) {
    if (!g_lookupFn) return 0;
    uintptr_t registries[2] = { g_class1Registry, g_class0Registry };
    int classes[2] = { 1, 0 };
    for (int i = 0; i < 2; ++i) {
        if (!registries[i]) continue;
        void* record = NULL;
        // 探针自己的查表不能进钩子日志（t_inProbeLookup 守住转发器）
        t_inProbeLookup = 1;
        __try {
            record = g_lookupFn((void*)registries[i], opcode);
        } __except (EXCEPTION_EXECUTE_HANDLER) {
            record = NULL;
        }
        t_inProbeLookup = 0;
        if (!record) continue;
        unsigned cb = 0, ctx = 0;
        if (!SafeReadU32((uintptr_t)record, &cb)) continue;
        SafeReadU32((uintptr_t)record + 4, &ctx);
        if (cls) *cls = classes[i];
        if (callback) *callback = (uintptr_t)cb;
        if (context) *context = (uintptr_t)ctx;
        return 1;
    }
    return 0;
}

static void DumpRegistry(const char* reason) {
    if (!g_lookupFn) {
        LogLine("REGDUMP skipped reason=%s lookup-fn-missing", reason ? reason : "?");
        return;
    }
    RefreshRegistries();
    for (int pass = 0; pass < 2; ++pass) {
        uintptr_t registry = pass == 0 ? g_class1Registry : g_class0Registry;
        int cls = pass == 0 ? 1 : 0;
        if (!registry) {
            LogLine("REGDUMP cls=%d registry=null reason=%s", cls, reason ? reason : "?");
            continue;
        }
        int handlers = 0;
        LogLine("REGDUMP begin cls=%d registry=0x%08X scan_max=%d reason=%s",
                cls, (unsigned)registry, g_cfg.registryScanMax, reason ? reason : "?");
        for (unsigned op = 0; op < (unsigned)g_cfg.registryScanMax; ++op) {
            void* record = NULL;
            // 转储期间几千次查表都不能进钩子日志，否则自己把自己刷爆
            t_inProbeLookup = 1;
            __try {
                record = g_lookupFn((void*)registry, op);
            } __except (EXCEPTION_EXECUTE_HANDLER) {
                t_inProbeLookup = 0;
                LogLine("REGDUMP cls=%d aborted op=%u exception=0x%08X", cls, op, GetExceptionCode());
                record = NULL;
                break;
            }
            t_inProbeLookup = 0;
            if (!record) continue;
            unsigned cb = 0, ctx = 0;
            if (!SafeReadU32((uintptr_t)record, &cb)) continue;
            SafeReadU32((uintptr_t)record + 4, &ctx);
            char desc[80];
            DescribeAddress((uintptr_t)cb, desc, sizeof(desc));
            ++handlers;
            // 已知有 S2C 注册的号段、以及合成器相关号段全部打星号标出来
            const char* mark = "";
            if (op == 63 || op == 99 || op == 1119 || op == 1100 || op == 1112 ||
                op == 1128 || op == 1133) mark = " <<<";
            LogLine("REG cls=%d op=%u cb=%s ctx=0x%08X%s", cls, op, desc, ctx, mark);
        }
        LogLine("REGDUMP end cls=%d handlers=%d reason=%s", cls, handlers, reason ? reason : "?");
    }
}

// ------------------------------------------------------------ 挂钩（内联）
//
// 两种模式：
//   chain  —— 目标首字节已是 E9（前面有钩子，正常就是 90CN）。
//             只覆盖这 5 字节，链的下一跳 = 原 E9 的目标；钩子干完 jmp 过去。
//   native —— 目标是原生前导。把前导整指令（7 字节）搬到跳板，
//             跳板末尾 E9 回 target+7；钩子干完 jmp 跳板。

struct HookPlan {
    const char*    name;
    uintptr_t      target;
    const unsigned char* expected;
    size_t         expectedLen;
    size_t         copyLen;
    void*          detour;
    uintptr_t*     contSlot;
    int            installed;
    int            chained;
    uintptr_t      contAddress;
};

static uintptr_t g_c2sContinue    = 0;
static uintptr_t g_s2cContinue    = 0;
static uintptr_t g_lookupContinue = 0;

static bool InstallHook(HookPlan& plan) {
    char desc[96];
    DescribeAddress(plan.target, desc, sizeof(desc));
    LogLine("HOOK '%s' target=%s expected_len=%u", plan.name, desc, (unsigned)plan.expectedLen);

    unsigned char current[16];
    memset(current, 0xCC, sizeof(current));
    if (!SafeRead((const void*)plan.target, current, plan.expectedLen)) {
        LogLine("HOOK '%s' REFUSED: 读目标内存失败", plan.name);
        return false;
    }

    size_t patchLen = 5;
    if (current[0] == 0xE9) {
        // 已经有钩子：链过去
        int rel = 0;
        memcpy(&rel, current + 1, sizeof(rel));
        uintptr_t dest = plan.target + 5 + (intptr_t)rel;
        char destDesc[96];
        DescribeAddress(dest, destDesc, sizeof(destDesc));
        // 链的目标必须在"可执行内存"里：正常情况就是 90CN.dll 的代理函数。
        if (!ModuleOf(dest) && !IsExecutableAddress(dest)) {
            LogLine("HOOK '%s' REFUSED: 已有 E9 目标 %s 不在任何模块/可执行页里，不敢链", plan.name, destDesc);
            return false;
        }
        plan.chained = 1;
        plan.contAddress = dest;
        *plan.contSlot = dest;
        LogLine("HOOK '%s' mode=chain 下游钩子=%s（90CN 的话就是它的代理函数）", plan.name, destDesc);
    } else if (memcmp(current, plan.expected, plan.expectedLen) == 0) {
        unsigned char* tramp = (unsigned char*)VirtualAlloc(
            NULL, plan.copyLen + 8, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
        if (!tramp) {
            LogLine("HOOK '%s' REFUSED: 跳板分配失败 error=%lu", plan.name, GetLastError());
            return false;
        }
        memcpy(tramp, current, plan.copyLen);
        tramp[plan.copyLen] = 0xE9;
        int32_t back = (int32_t)((plan.target + plan.copyLen) - ((uintptr_t)tramp + plan.copyLen + 5));
        memcpy(tramp + plan.copyLen + 1, &back, sizeof(back));
        FlushInstructionCache(GetCurrentProcess(), tramp, plan.copyLen + 5);
        plan.chained = 0;
        plan.contAddress = (uintptr_t)tramp;
        *plan.contSlot = (uintptr_t)tramp;
        patchLen = plan.copyLen;
        LogLine("HOOK '%s' mode=native 跳板=0x%08X 搬了 %u 字节（目标前导是原生的，说明 90CN 没勾这里）",
                plan.name, (unsigned)(uintptr_t)tramp, (unsigned)plan.copyLen);
    } else {
        char got[3 * 16 + 1] = { 0 };
        char want[3 * 16 + 1] = { 0 };
        for (size_t i = 0; i < plan.expectedLen; ++i) {
            char t[8];
            _snprintf_s(t, sizeof(t), _TRUNCATE, "%02X ", current[i]);
            strcat_s(got, t);
            _snprintf_s(t, sizeof(t), _TRUNCATE, "%02X ", plan.expected[i]);
            strcat_s(want, t);
        }
        LogLine("HOOK '%s' REFUSED: 目标字节不匹配（客户端版本不符）。实际=[%s] 期望=[%s]",
                plan.name, got, want);
        return false;
    }

    DWORD oldProtect = 0;
    if (!VirtualProtect((void*)plan.target, patchLen, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("HOOK '%s' REFUSED: VirtualProtect 失败 error=%lu", plan.name, GetLastError());
        return false;
    }
    unsigned char* p = (unsigned char*)plan.target;
    p[0] = 0xE9;
    int32_t rel = (int32_t)((uintptr_t)plan.detour - (plan.target + 5));
    memcpy(p + 1, &rel, sizeof(rel));
    for (size_t i = 5; i < patchLen; ++i) p[i] = 0x90;
    DWORD ignored = 0;
    VirtualProtect((void*)plan.target, patchLen, oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), (void*)plan.target, patchLen);

    plan.installed = 1;
    LogLine("HOOK '%s' INSTALLED target=%s patched=%u bytes detour=0x%08X",
            plan.name, desc, (unsigned)patchLen, (unsigned)(uintptr_t)plan.detour);
    return true;
}

// ------------------------------------------------------------- 钩子回调
// 这三个函数由下面的 naked 转发器调用，参数已经在转发器里从栈上取好。
// 它们只写日志，绝不改游戏状态。

extern "C" void __cdecl ProbeC2SPacket(
    unsigned msg, const void* body, int len, uintptr_t ebp, uintptr_t esp);
extern "C" void __cdecl ProbeS2CPacket(
    unsigned msg, const void* body, int len, uintptr_t ebp, uintptr_t esp);
extern "C" void __cdecl ProbeDispatchLookup(
    unsigned registry, unsigned opcode, uintptr_t ebp, uintptr_t esp);

// 全量抓取窗口：见到 trigger opcode 后 armSeconds 秒内所有包/所有查表都记。
static volatile LONG g_armedUntilTick = 0;
// 挂钩里只置位，真正的注册表转储交给工作线程（传输线程里不做重活）
static volatile LONG g_registryDumpRequest = 0;

static bool Armed(void) {
    DWORD until = (DWORD)InterlockedCompareExchange(&g_armedUntilTick, 0, 0);
    if (!until) return false;
    return (long)(GetTickCount() - until) < 0;
}

static void ArmWindow(unsigned opcode) {
    DWORD until = GetTickCount() + (DWORD)g_cfg.armSeconds * 1000u;
    InterlockedExchange(&g_armedUntilTick, (LONG)until);
    LogLine("ARM seconds=%d cause=C2S-op%u（这 %d 秒内所有 C2S/S2C/查表全量记录）",
            g_cfg.armSeconds, opcode, g_cfg.armSeconds);
    // 让工作线程去转储注册表（不该在传输线程里做重活）
    InterlockedExchange(&g_registryDumpRequest, 1);
}

extern "C" void __cdecl ProbeC2SPacket(
    unsigned msg, const void* body, int len, uintptr_t ebp, uintptr_t esp)
{
    const bool isTrigger = InList(g_cfg.trigger, g_cfg.triggerCount, msg) != 0;
    const bool isWatch = InList(g_cfg.watch, g_cfg.watchCount, msg) != 0;
    if (!isTrigger) {
        if (!isWatch && !g_cfg.logAllC2S && !Armed()) return;
    }

    bool wasArmed = Armed();
    if (isTrigger && !wasArmed) ArmWindow(msg);

    char line[2048];
    int limit = (isTrigger || wasArmed) ? g_cfg.fullBodyBytes : g_cfg.briefBodyBytes;
    _snprintf_s(line, sizeof(line), _TRUNCATE,
        "C2S op=%u len=%d %s%s body=", msg, len,
        isTrigger ? "TRIGGER " : "", wasArmed ? "ARMED " : "");
    AppendHex(line, sizeof(line), body, len, limit);
    if (isTrigger || wasArmed) AppendStack(line, sizeof(line), ebp, esp);
    LogLine("%s", line);
}

extern "C" void __cdecl ProbeS2CPacket(
    unsigned msg, const void* body, int len, uintptr_t ebp, uintptr_t esp)
{
    const bool isWatch = InList(g_cfg.watch, g_cfg.watchCount, msg) != 0;
    const bool armed = Armed();
    if (!armed && !isWatch && !g_cfg.logAllS2C) return;

    char line[2048];
    int limit = armed ? g_cfg.fullBodyBytes : g_cfg.briefBodyBytes;
    _snprintf_s(line, sizeof(line), _TRUNCATE, "S2C op=%u len=%d %s", msg, len, armed ? "ARMED " : "");

    // 客户端"认不认这个回包"的最直接判据：它的分发注册表里有没有这个 opcode。
    if (armed || isWatch) {
        int cls = -1;
        uintptr_t cb = 0, ctx = 0;
        if (ResolveHandler(msg, &cls, &cb, &ctx)) {
            char desc[80];
            DescribeAddress(cb, desc, sizeof(desc));
            char piece[160];
            _snprintf_s(piece, sizeof(piece), _TRUNCATE,
                "handler_cls=%d handler=%s ctx=0x%08X ", cls, desc, (unsigned)ctx);
            strcat_s(line, sizeof(line), piece);
        } else {
            strcat_s(line, sizeof(line), "handler=NONE ");
        }
    }
    strcat_s(line, sizeof(line), "body=");
    AppendHex(line, sizeof(line), body, len, limit);
    if (armed) AppendStack(line, sizeof(line), ebp, esp);
    LogLine("%s", line);
}

extern "C" void __cdecl ProbeDispatchLookup(
    unsigned registry, unsigned opcode, uintptr_t ebp, uintptr_t esp)
{
    // 探针自己为了解析处理器而调用的查表不记（t_inProbeLookup 守住）
    if (t_inProbeLookup) return;

    const bool armed = Armed();
    const bool isWatch = InList(g_cfg.watch, g_cfg.watchCount, opcode) != 0;
    if (!armed && !isWatch && !g_cfg.lookupLogAll) return;

    int cls = -1;
    if (registry && registry == (unsigned)g_class1Registry) cls = 1;
    else if (registry && registry == (unsigned)g_class0Registry) cls = 0;

    char line[1024];
    _snprintf_s(line, sizeof(line), _TRUNCATE, "LOOKUP cls=%d op=%u registry=0x%08X ",
                cls, opcode, registry);

    if (g_cfg.lookupResolve && g_lookupFn) {
        void* record = NULL;
        t_inProbeLookup = 1;
        __try {
            record = g_lookupFn((void*)(uintptr_t)registry, opcode);
        } __except (EXCEPTION_EXECUTE_HANDLER) {
            record = NULL;
        }
        t_inProbeLookup = 0;
        if (record) {
            unsigned cb = 0, ctx = 0;
            SafeReadU32((uintptr_t)record, &cb);
            SafeReadU32((uintptr_t)record + 4, &ctx);
            char desc[80];
            DescribeAddress((uintptr_t)cb, desc, sizeof(desc));
            char piece[160];
            _snprintf_s(piece, sizeof(piece), _TRUNCATE, "handler=%s ctx=0x%08X", desc, ctx);
            strcat_s(line, sizeof(line), piece);
        } else {
            strcat_s(line, sizeof(line), "handler=NONE（这个号没注册处理器）");
        }
    }
    AppendStack(line, sizeof(line), ebp, esp);
    LogLine("%s", line);
}

// ------------------------------------------------------------- naked 转发器
//
// 栈布局（进入转发器时）：[esp]=返回地址 [esp+4]=arg0 [esp+8]=arg1 ...
// pushfd(4) + pushad(32) 之后：arg0 在 [esp+0x28]，arg1 在 [esp+0x2C]，
// arg2 在 [esp+0x30]；pushad 保存区里 EBP 在 [esp+8]、ECX 在 [esp+0x18]。
// 这与 90CN.cpp 的 ProxyCipherEncodeFunction 完全一致。

__declspec(naked) void DetourC2S(void) {
    __asm {
        pushfd
        pushad
        mov eax, [esp + 0x28]      // msg
        mov ecx, [esp + 0x2C]      // body
        mov edx, [esp + 0x30]      // len
        mov ebx, [esp + 0x08]      // 调用时的 EBP（pushad 保存区）
        lea esi, [esp]             // 调用时的 ESP 位置
        push esi
        push ebx
        push edx
        push ecx
        push eax
        call ProbeC2SPacket
        add esp, 0x14
        popad
        popfd
        jmp dword ptr [g_c2sContinue]
    }
}

__declspec(naked) void DetourS2C(void) {
    __asm {
        pushfd
        pushad
        mov eax, [esp + 0x28]
        mov ecx, [esp + 0x2C]
        mov edx, [esp + 0x30]
        mov ebx, [esp + 0x08]
        lea esi, [esp]
        push esi
        push ebx
        push edx
        push ecx
        push eax
        call ProbeS2CPacket
        add esp, 0x14
        popad
        popfd
        jmp dword ptr [g_s2cContinue]
    }
}

__declspec(naked) void DetourLookup(void) {
    __asm {
        pushfd
        pushad
        mov eax, [esp + 0x18]      // 调用时的 ECX = registry（__thiscall）
        mov ecx, [esp + 0x28]      // 栈上唯一实参 = opcode
        mov ebx, [esp + 0x08]      // 调用时的 EBP
        lea esi, [esp]
        push esi
        push ebx
        push ecx
        push eax
        call ProbeDispatchLookup
        add esp, 0x10
        popad
        popfd
        jmp dword ptr [g_lookupContinue]
    }
}

// ------------------------------------------------------------------ 安装流

static HookPlan g_c2sPlan = {
    "c2s-encode(sub_33AAFE0)", 0, kPrologueCodec, sizeof(kPrologueCodec),
    COPY_LEN_CODEC, (void*)&DetourC2S, &g_c2sContinue, 0, 0, 0
};
static HookPlan g_s2cPlan = {
    "s2c-decode(sub_33AB0A0)", 0, kPrologueCodec, sizeof(kPrologueCodec),
    COPY_LEN_CODEC, (void*)&DetourS2C, &g_s2cContinue, 0, 0, 0
};
static HookPlan g_lookupPlan = {
    "dispatch-lookup(sub_2262430)", 0, kPrologueLookup, sizeof(kPrologueLookup),
    COPY_LEN_LOOKUP, (void*)&DetourLookup, &g_lookupContinue, 0, 0, 0
};

static void LogHostSummary(void) {
    LogLine("========== CubeProbe 启动 pid=%lu ==========", (unsigned long)GetCurrentProcessId());
    LogLine("日志文件=%s", g_logPath);
    LogLine("配置文件=%s", g_iniPath);
    LogLine("主模块=0x%08X SizeOfImage=0x%X 模块数=%d", (unsigned)g_hostBase,
            (unsigned)g_hostSize, g_moduleCount);
    for (int i = 0; i < g_moduleCount; ++i) {
        if (strstr(g_modules[i].name, "90CN") || strstr(g_modules[i].name, "DNF")) {
            LogLine("  module %s base=0x%08X size=0x%X", g_modules[i].name,
                    (unsigned)g_modules[i].base, (unsigned)g_modules[i].size);
        }
    }
    char trigger[256] = { 0 }, watch[512] = { 0 };
    for (int i = 0; i < g_cfg.triggerCount; ++i) {
        char t[16];
        _snprintf_s(t, sizeof(t), _TRUNCATE, "%u ", g_cfg.trigger[i]);
        strcat_s(trigger, t);
    }
    for (int i = 0; i < g_cfg.watchCount; ++i) {
        char t[16];
        _snprintf_s(t, sizeof(t), _TRUNCATE, "%u ", g_cfg.watch[i]);
        strcat_s(watch, t);
    }
    LogLine("config trigger=[%s] watch=[%s] arm=%ds stack=%d lookup_resolve=%d hotkey_vk=0x%02X",
            trigger, watch, g_cfg.armSeconds, g_cfg.stackFrames, g_cfg.lookupResolve, g_cfg.hotkeyVk);
}

static DWORD WINAPI ProbeWorker(LPVOID) {
    Sleep((DWORD)(g_cfg.installDelayMs > 0 ? g_cfg.installDelayMs : 0));

    g_registryFn0 = (RegistryFn)(g_hostBase + kRvaClass0Registry);
    g_registryFn1 = (RegistryFn)(g_hostBase + kRvaClass1Registry);
    g_lookupFn    = (LookupFn)(g_hostBase + kRvaDispatchLookup);
    RefreshRegistries();
    LogLine("registry class0=0x%08X class1=0x%08X lookup_fn=0x%08X",
            (unsigned)g_class0Registry, (unsigned)g_class1Registry, (unsigned)(uintptr_t)g_lookupFn);

    g_c2sPlan.target    = g_hostBase + kRvaC2SEncode;
    g_s2cPlan.target    = g_hostBase + kRvaS2CDecode;
    g_lookupPlan.target = g_hostBase + kRvaDispatchLookup;
    bool c2s = InstallHook(g_c2sPlan);
    bool s2c = InstallHook(g_s2cPlan);
    bool look = InstallHook(g_lookupPlan);
    LogLine("安装结果 c2s=%d s2c=%d lookup=%d", c2s ? 1 : 0, s2c ? 1 : 0, look ? 1 : 0);
    if (!c2s || !s2c || !look) {
        LogLine("提示：被拒的钩子没有任何副作用；其余钩子照常工作。");
    }

    if (g_cfg.registryDumpOnStart) {
        Sleep((DWORD)(g_cfg.registryDumpDelayMs > 0 ? g_cfg.registryDumpDelayMs : 0));
        DumpRegistry("startup");
    }

    // 热键 + 按需转储轮询（只轮询键盘状态，不挂钩输入路径）
    bool hotkeyDown = false;
    for (;;) {
        Sleep(60);
        if (InterlockedExchange(&g_registryDumpRequest, 0)) {
            DumpRegistry("armed");
        }
        if (!g_cfg.hotkeyVk) continue;
        bool down = (GetAsyncKeyState(g_cfg.hotkeyVk) & 0x8000) != 0;
        if (down && !hotkeyDown) {
            LogLine("HOTKEY vk=0x%02X 手动转储注册表", g_cfg.hotkeyVk);
            DumpRegistry("hotkey");
        }
        hotkeyDown = down;
    }
}

// ------------------------------------------------------- 自检用导出（可选）

#ifdef CUBEPROBE_SELFTEST
extern "C" int   CubeProbeTestInit(const char* logPath);
extern "C" int   CubeProbeTestInstall(
    const char* name, uintptr_t target, const unsigned char* expected,
    size_t expectedLen, size_t copyLen, void* detour, int role);
extern "C" long  CubeProbeTestEventCount(void);
extern "C" uintptr_t CubeProbeTestC2SContinue(void);

extern "C" int CubeProbeTestInit(const char* logPath) {
    ConfigDefaults();
    g_cfg.registryDumpOnStart = 0;
    if (logPath) _snprintf_s(g_logPath, sizeof(g_logPath), _TRUNCATE, "%s", logPath);
    InitializeCriticalSection(&g_logLock);
    OpenLog();
    if (!g_log) return 0;
    EnumModules();
    return 1;
}

// role: 0 = C2S 编码槽，1 = S2C 解码槽，2 = 分发查表槽。
// 必须是真实的续跳槽位：钩子里要往 *contSlot 写链的下一跳。
extern "C" int CubeProbeTestInstall(
    const char* name, uintptr_t target, const unsigned char* expected,
    size_t expectedLen, size_t copyLen, void* detour, int role)
{
    HookPlan plan;
    memset(&plan, 0, sizeof(plan));
    plan.name = name;
    plan.target = target;
    plan.expected = expected;
    plan.expectedLen = expectedLen;
    plan.copyLen = copyLen;
    plan.detour = detour;
    plan.contSlot = (role == 2) ? &g_lookupContinue
                 : (role == 1) ? &g_s2cContinue : &g_c2sContinue;
    return InstallHook(plan) ? 1 : 0;
}

extern "C" long CubeProbeTestEventCount(void) { return (long)g_probeEvents; }
extern "C" void* CubeProbeTestDetourC2S(void) { return (void*)&DetourC2S; }
extern "C" void* CubeProbeTestDetourS2C(void) { return (void*)&DetourS2C; }
extern "C" void* CubeProbeTestDetourLookup(void) { return (void*)&DetourLookup; }
#endif  // CUBEPROBE_SELFTEST

// ------------------------------------------------------------------ 入口

#ifndef CUBEPROBE_SELFTEST
BOOL APIENTRY DllMain(HMODULE self, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(self);
        InitializeCriticalSection(&g_logLock);

        GetModuleFileNameA(NULL, g_exeDir, MAX_PATH);
        char* slash = strrchr(g_exeDir, '\\');
        if (slash) *(slash + 1) = 0; else g_exeDir[0] = 0;
        _snprintf_s(g_logPath, sizeof(g_logPath), _TRUNCATE, "%sCubeProbe.log", g_exeDir);

        OpenLog();
        if (!g_log) return TRUE;                  // 打不开日志就静默退出，不影响游戏
        LoadConfig();
        EnumModules();
        LogHostSummary();

        HANDLE t = CreateThread(NULL, 0, ProbeWorker, NULL, 0, NULL);
        if (t) CloseHandle(t);
    }
    return TRUE;
}
#endif
