// ===========================================================================
// CubeProbe 自检程序（不属于交付物，仅用于验证钩子机制本身）
//
// 它把 CubeProbe.cpp 用 CUBEPROBE_SELFTEST 编译进来，然后在**自己进程里**用
// 手工合成的机器码当"目标函数"，把两条挂钩路径都跑一遍：
//
//   ① native 模式：目标前导是原生的（55 8B EC 53 8B 5D 10 / 55 8B EC 83 79 08 00）
//      -> 钩子装好后调用它，必须：日志有记录 + 原函数体照常执行 + 返回值不变。
//   ② chain  模式：目标首字节已经是 E9（模拟 90CN 已经勾过）
//      -> 钩子装好后调用它，必须：日志有记录 + 下游那个 E9 目标照常执行。
//   ③ dispatch-lookup 转发器的寄存器/栈偏移：用 __thiscall 假函数验证
//      钩子读到的 ecx(registry) 与栈上 opcode 和真实调用一致。
//
// 全绿才说明 naked 转发器的 esp 偏移、跳板、E9 链都没问题 ——
// 这样注入真实客户端时才有底。
//
// 构建：build_cube_probe_selftest.bat
// ===========================================================================

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

extern "C" int  CubeProbeTestInit(const char* logPath);
extern "C" int  CubeProbeTestInstall(
    const char* name, uintptr_t target, const unsigned char* expected,
    size_t expectedLen, size_t copyLen, void* detour, int role);
extern "C" long CubeProbeTestEventCount(void);
extern "C" void* CubeProbeTestDetourC2S(void);
extern "C" void* CubeProbeTestDetourLookup(void);

static int g_failures = 0;

static void Check(int ok, const char* what) {
    printf("  [%s] %s\n", ok ? "PASS" : "FAIL", what);
    if (!ok) ++g_failures;
}

// ---- 假目标用的可写全局（机器码用绝对地址写它们）----
static volatile int g_arg0 = -1;
static volatile int g_arg2 = -1;
static volatile int g_chainHits = 0;
static volatile unsigned g_lookupEcx = 0;
static volatile unsigned g_lookupOp = 0;

static const unsigned char kPrologueCodec[7] = { 0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x10 };
static const unsigned char kPrologueLookup[7] = { 0x55, 0x8B, 0xEC, 0x83, 0x79, 0x08, 0x00 };

static unsigned char* AllocCode(const unsigned char* bytes, size_t len) {
    unsigned char* p = (unsigned char*)VirtualAlloc(
        NULL, 0x1000, MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
    if (!p) return NULL;
    memset(p, 0xCC, 0x1000);
    memcpy(p, bytes, len);
    FlushInstructionCache(GetCurrentProcess(), p, len);
    return p;
}

// 假 C2S 编码函数：契约与 sub_33AAFE0 一致（__stdcall，5 参，ret 14h）
static unsigned char* BuildFakeCodec(void) {
    unsigned char code[64];
    size_t n = 0;
    code[n++] = 0x55;                                   // push ebp
    code[n++] = 0x8B; code[n++] = 0xEC;                 // mov ebp, esp
    code[n++] = 0x53;                                   // push ebx
    code[n++] = 0x8B; code[n++] = 0x5D; code[n++] = 0x10; // mov ebx, [ebp+0x10]  (arg2)
    code[n++] = 0x8B; code[n++] = 0x45; code[n++] = 0x08; // mov eax, [ebp+8]     (arg0)
    code[n++] = 0xA3;                                   // mov [g_arg0], eax
    *(uint32_t*)(code + n) = (uint32_t)(uintptr_t)&g_arg0; n += 4;
    code[n++] = 0x89; code[n++] = 0x1D;                 // mov [g_arg2], ebx
    *(uint32_t*)(code + n) = (uint32_t)(uintptr_t)&g_arg2; n += 4;
    code[n++] = 0xB8; code[n++] = 0x01; code[n++] = 0x00; code[n++] = 0x00; code[n++] = 0x00; // mov eax,1
    code[n++] = 0x5B;                                   // pop ebx
    code[n++] = 0x5D;                                   // pop ebp
    code[n++] = 0xC2; code[n++] = 0x14; code[n++] = 0x00; // ret 14h
    return AllocCode(code, n);
}

// 假"90CN 代理"：只置位 g_chainHits 并返回 2
static unsigned char* BuildFakeChainTarget(void) {
    unsigned char code[32];
    size_t n = 0;
    code[n++] = 0xC7; code[n++] = 0x05;                 // mov dword ptr [g_chainHits], 1
    *(uint32_t*)(code + n) = (uint32_t)(uintptr_t)&g_chainHits; n += 4;
    code[n++] = 0x01; code[n++] = 0x00; code[n++] = 0x00; code[n++] = 0x00;
    code[n++] = 0xB8; code[n++] = 0x02; code[n++] = 0x00; code[n++] = 0x00; code[n++] = 0x00; // mov eax,2
    code[n++] = 0xC2; code[n++] = 0x14; code[n++] = 0x00; // ret 14h
    return AllocCode(code, n);
}

// 假分发查表：__thiscall(void* registry, unsigned opcode)，ret 4
static unsigned char* BuildFakeLookup(void) {
    unsigned char code[64];
    size_t n = 0;
    code[n++] = 0x55;                                   // push ebp
    code[n++] = 0x8B; code[n++] = 0xEC;                 // mov ebp, esp
    code[n++] = 0x83; code[n++] = 0x79; code[n++] = 0x08; code[n++] = 0x00; // cmp [ecx+8],0
    code[n++] = 0x89; code[n++] = 0x0D;                 // mov [g_lookupEcx], ecx
    *(uint32_t*)(code + n) = (uint32_t)(uintptr_t)&g_lookupEcx; n += 4;
    code[n++] = 0x8B; code[n++] = 0x45; code[n++] = 0x08; // mov eax, [ebp+8]
    code[n++] = 0xA3;                                   // mov [g_lookupOp], eax
    *(uint32_t*)(code + n) = (uint32_t)(uintptr_t)&g_lookupOp; n += 4;
    code[n++] = 0x33; code[n++] = 0xC0;                 // xor eax, eax
    code[n++] = 0x5D;                                   // pop ebp
    code[n++] = 0xC2; code[n++] = 0x04; code[n++] = 0x00; // ret 4
    return AllocCode(code, n);
}

typedef int (__stdcall* CodecFn)(int msg, const void* body, int len, void* out, int* outLen);
typedef void* (__thiscall* LookupFnT)(void* registry, unsigned opcode);

static char g_logFile[MAX_PATH];

static long CountInLog(const char* needle) {
    // 探针自己一直开着这个文件，所以这里必须显式共享读（CreateFile 而不是 fopen）
    HANDLE h = CreateFileA(g_logFile, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE,
                           NULL, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, NULL);
    if (h == INVALID_HANDLE_VALUE) {
        printf("      (日志打不开: %s err=%lu)\n", g_logFile, GetLastError());
        return -1;
    }
    DWORD size = GetFileSize(h, NULL);
    if (size == INVALID_FILE_SIZE || size == 0) { CloseHandle(h); return 0; }
    char* buf = (char*)malloc(size + 1);
    if (!buf) { CloseHandle(h); return -1; }
    DWORD got = 0;
    if (!ReadFile(h, buf, size, &got, NULL)) got = 0;
    CloseHandle(h);
    buf[got] = 0;
    long hits = 0;
    for (const char* p = buf; (p = strstr(p, needle)) != NULL; ++p) ++hits;
    free(buf);
    return hits;
}

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);           // 崩溃也要看得见已经跑到哪一步
    printf("CubeProbe 自检\n");
    char tempDir[MAX_PATH];
    GetTempPathA(MAX_PATH, tempDir);
    _snprintf_s(g_logFile, sizeof(g_logFile), _TRUNCATE, "%sCubeProbe_selftest.log", tempDir);
    DeleteFileA(g_logFile);

    if (!CubeProbeTestInit(g_logFile)) {
        printf("  [FAIL] 探针日志打不开：%s\n", g_logFile);
        return 2;
    }
    printf("  日志：%s\n", g_logFile);

    CodecFn codec = (CodecFn)BuildFakeCodec();
    if (!codec) { printf("  [FAIL] 假函数分配失败\n"); return 2; }

    // ---------- 0. 挂钩前的基线 ----------
    int outLen = 0;
    g_arg0 = -1; g_arg2 = -1;
    int rc = codec(1119, "BODY", 8, NULL, &outLen);
    Check(rc == 1 && g_arg0 == 1119 && g_arg2 == 8, "挂钩前：假函数本体按契约执行");

    // ---------- 1. native 模式 ----------
    long before = CubeProbeTestEventCount();
    int installed = CubeProbeTestInstall(
        "selftest-codec-native", (uintptr_t)codec, kPrologueCodec, sizeof(kPrologueCodec),
        7, CubeProbeTestDetourC2S(), 0);
    Check(installed == 1, "native 模式：前导匹配，钩子安装成功");

    g_arg0 = -1; g_arg2 = -1;
    rc = codec(1119, "BODY", 8, NULL, &outLen);
    Check(rc == 1, "native 模式：返回值仍是原函数的 1（跳板把前导搬对了）");
    Check(g_arg0 == 1119 && g_arg2 == 8, "native 模式：原函数体照常执行（参数没被破坏）");
    Check(CubeProbeTestEventCount() > before, "native 模式：探针记到了这次调用");
    Check(CountInLog("C2S op=1119 len=8") > 0, "native 模式：日志里有 C2S op=1119");
    Check(CountInLog("stack=") > 0, "native 模式：trigger opcode 打了调用栈");

    // ---------- 2. chain 模式（模拟 90CN 已经勾过）----------
    unsigned char* chainTarget = BuildFakeChainTarget();
    if (!chainTarget) { printf("  [FAIL] 链目标分配失败\n"); return 2; }
    g_chainHits = 0;
    // 把假函数头 5 字节改成 E9 -> 链目标，等价于 90CN 已装钩子的现场
    {
        DWORD old = 0;
        VirtualProtect(codec, 5, PAGE_EXECUTE_READWRITE, &old);
        unsigned char* p = (unsigned char*)codec;
        p[0] = 0xE9;
        int32_t rel = (int32_t)((uintptr_t)chainTarget - ((uintptr_t)codec + 5));
        memcpy(p + 1, &rel, sizeof(rel));
        DWORD ignored = 0;
        VirtualProtect(codec, 5, old, &ignored);
        FlushInstructionCache(GetCurrentProcess(), codec, 5);
    }
    before = CubeProbeTestEventCount();
    installed = CubeProbeTestInstall(
        "selftest-codec-chain", (uintptr_t)codec, kPrologueCodec, sizeof(kPrologueCodec),
        7, CubeProbeTestDetourC2S(), 0);
    Check(installed == 1, "chain 模式：首字节是 E9，识别为已有钩子并接受安装");

    rc = codec(1119, "BODY2", 5, NULL, &outLen);
    Check(rc == 2, "chain 模式：下游钩子的返回值透传（原来那条链没被切断）");
    Check(g_chainHits == 1, "chain 模式：下游钩子确实被调用了");
    Check(CubeProbeTestEventCount() > before, "chain 模式：探针同样记到了这次调用");
    Check(CountInLog("C2S op=1119 len=5") > 0, "chain 模式：日志里是改写后的包体长度");

    // ---------- 3. 首字节既不是 E9 也不匹配 -> 必须拒绝 ----------
    unsigned char* bogus = AllocCode((const unsigned char*)"\x90\x90\x90\x90\x90\x90\x90", 7);
    Check(bogus != NULL, "拒绝用例：分配测试内存");
    installed = CubeProbeTestInstall(
        "selftest-refuse", (uintptr_t)bogus, kPrologueCodec, sizeof(kPrologueCodec),
        7, CubeProbeTestDetourC2S(), 0);
    Check(installed == 0, "版本不符时拒绝打补丁（返回 0，不产生任何写入）");
    Check(CountInLog("REFUSED: 目标字节不匹配") > 0, "拒绝时日志写明了实际/期望字节");

    // ---------- 4. dispatch-lookup 转发器的 ecx / 栈偏移 ----------
    // 注意：假查表第一句就是 cmp [ecx+8],0（跟真函数一样），所以 ecx 必须是个
    // 合法指针，不能是随便一个数字常量。
    LookupFnT lookup = (LookupFnT)BuildFakeLookup();
    if (!lookup) { printf("  [FAIL] 假查表分配失败\n"); return 2; }
    static unsigned dummyRegistry[8] = { 0 };
    void* regA = (void*)dummyRegistry;
    g_lookupEcx = 0; g_lookupOp = 0;
    lookup(regA, 1119u);
    Check(g_lookupEcx == (unsigned)(uintptr_t)regA && g_lookupOp == 1119u,
          "查表假函数本体：ecx/栈参数正确");

    before = CubeProbeTestEventCount();
    installed = CubeProbeTestInstall(
        "selftest-lookup", (uintptr_t)lookup, kPrologueLookup, sizeof(kPrologueLookup),
        7, CubeProbeTestDetourLookup(), 2);
    Check(installed == 1, "查表：7 字节前导匹配，钩子安装成功");
    g_lookupEcx = 0; g_lookupOp = 0;
    lookup(regA, 1119u);
    Check(g_lookupEcx == (unsigned)(uintptr_t)regA && g_lookupOp == 1119u,
          "查表：钩子放行后原函数拿到的仍是正确的 ecx/opcode（pushad 偏移没算错）");
    Check(CubeProbeTestEventCount() > before, "查表：探针记到了这次分发查询");
    {
        char needle[128];
        _snprintf_s(needle, sizeof(needle), _TRUNCATE, "LOOKUP cls=-1 op=1119 registry=0x%08X",
                    (unsigned)(uintptr_t)regA);
        Check(CountInLog(needle) > 0, "查表：日志里的 registry / opcode 与真实调用一致");
    }

    printf("\n%s（失败 %d 项）\n", g_failures == 0 ? "全部通过" : "存在失败", g_failures);
    printf("自检日志：%s\n", g_logFile);
    return g_failures == 0 ? 0 : 1;
}
