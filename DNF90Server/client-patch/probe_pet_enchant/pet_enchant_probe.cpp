// 宠物附魔结果窗 —— 独立诊断 DLL（自行注入；不修改 90CN.dll，不改变游戏行为）
//
// 背景：附魔（op272 / ENCHANT_BY_BEAD）成功后，客户端结果弹窗（"附魔成功!!"）显示的
// 物品与玩家实际附魔的宠物不一致；只带 1 只宠物时显示成宠物装备，多只时显示成另一只
// 宠物；而把 op272 回包的槽位改成"刚下发那一行"的对象后弹窗干脆不出现。
//
// 客户端成功分支 sub_1D0B960 的逻辑（已反汇编核对）：
//     cmp  byte ptr [ebp+0xC], 0     ; 结果字节（0 = 失败）
//     ...
//     call sub_34E3970               ; 读 1 字节 -> [ebp+0xF] 物品空间
//     call sub_34E39E0               ; 读 2 字节 -> [ebp-4]    槽位
//     list==7 -> ecx=[0x51B2780]     ; 选择视图（不是原始容器！）
//     list==3 -> 装备 vtable[0xB28]
//     其它    -> ecx=[0x51B2768]
//     call sub_2326860               ; 按值解析物品对象
//     mov  esi, eax
//     mov  ecx, [0x51B2764]          ; <- 本 DLL 的 hook 点（0x01D0B9F6，6 字节）
//     push 0 / push esi / push 0x3D9 ; UI 985 = 结果弹窗
//     call sub_2FDD1A0
//
// 本 DLL 只做两件事：
//   1) 在 0x01D0B9F6 记录回包里的 (list, slot)、原生解析出的物品对象与物品 id；
//   2) 顺带把 0x51B2768 + i*4 这 16 个全局视图逐一用同一个解析函数探测一遍，
//      看哪个视图里按该槽位能查到被附魔的那件东西（物品 id 直接可辨：宠物 /
//      宠物装备 / 其它）。
// 被替换的指令会原样重放，寄存器、栈、返回值全部按原样恢复，不产生任何行为变化。
//
// 日志：客户端目录下的 pet_enchant_probe.log

#include <windows.h>
#include <stdarg.h>
#include <stdint.h>

// ---- 客户端 DNF.exe（image base 0x400000）里的 RVA ----
static const uintptr_t kSiteA         = 0x0190B9F6;  // mov ecx, [0x51B2764]  (6 字节)
static const uintptr_t kSiteAResume   = 0x0190B9FC;  // push 0
static const uintptr_t kSiteB         = 0x019739B9;  // movsx eax, word ptr [ebp-17Ch] (7 字节)
static const uintptr_t kSiteBResume   = 0x019739C0;  // 其后
static const uintptr_t kLookupRva     = 0x01F26860;  // sub_2326860(view, index)
static const uintptr_t kViewsRva      = 0x04DB2768;  // 0x51B2768 .. 0x51B27A7（16 个）
static const uintptr_t kTemplateReadRva = 0x003CA070; // sub_7CA070

typedef void* (__thiscall *LookupFn)(void* self, int index);
typedef void* (__thiscall *ItemDataFn)(void* item);
typedef uint32_t (__thiscall *TemplateReadFn)(void* data);

static uintptr_t g_exeBase = 0;
static CRITICAL_SECTION g_logLock;
static char g_logPath[MAX_PATH];
static LONG g_ackLogs = 0;
static LONG g_rowLogs = 0;

static uintptr_t g_siteAResume = 0;
static uintptr_t g_siteBResume = 0;

// ---------------------------------------------------------------- logging ----

static void LogOpen(void)
{
    char exePath[MAX_PATH];
    DWORD len = GetModuleFileNameA(NULL, exePath, MAX_PATH);
    if (len == 0 || len >= MAX_PATH) {
        return;
    }
    char* slash = exePath + len;
    while (slash > exePath && *slash != '\\') {
        --slash;
    }
    *(slash + 1) = 0;
    wsprintfA(g_logPath, "%spet_enchant_probe.log", exePath);
}

static void Log(const char* text)
{
    if (g_logPath[0] == 0) {
        return;
    }
    EnterCriticalSection(&g_logLock);
    HANDLE file = CreateFileA(g_logPath, FILE_APPEND_DATA, FILE_SHARE_READ,
        NULL, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
    if (file != INVALID_HANDLE_VALUE) {
        SYSTEMTIME st;
        GetLocalTime(&st);
        char line[1200];
        int prefix = wsprintfA(line, "[%04d-%02d-%02d %02d:%02d:%02d.%03d] %s\r\n",
            st.wYear, st.wMonth, st.wDay, st.wHour, st.wMinute, st.wSecond,
            st.wMilliseconds, text);
        DWORD written = 0;
        WriteFile(file, line, (DWORD)prefix, &written, NULL);
        CloseHandle(file);
    }
    LeaveCriticalSection(&g_logLock);
}

static void Logf(const char* fmt, ...)
{
    char text[1024];
    va_list args;
    va_start(args, fmt);
    wvsprintfA(text, fmt, args);
    va_end(args);
    Log(text);
}

// ------------------------------------------------------------ item helpers ----

// 与 90CN.cpp 的 ReadCurrentItemTemplateID 同一套路：vtable[0x28/4] 取 itemData，
// 再从 itemData+0x14 读模板 id；全程 SEH 保护，读不到就返回 0。
static uint32_t ReadItemID(void* item)
{
    if (item == NULL || g_exeBase == 0) {
        return 0;
    }
    __try {
        void** vtable = *reinterpret_cast<void***>(item);
        if (vtable == NULL) {
            return 0;
        }
        ItemDataFn itemDataFn =
            reinterpret_cast<ItemDataFn>(vtable[0x28 / sizeof(void*)]);
        if (itemDataFn == NULL) {
            return 0;
        }
        void* itemData = itemDataFn(item);
        if (itemData == NULL) {
            return 0;
        }
        TemplateReadFn readTemplate =
            reinterpret_cast<TemplateReadFn>(g_exeBase + kTemplateReadRva);
        return readTemplate(reinterpret_cast<unsigned char*>(itemData) + 0x14);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return 0;
    }
}

static void* SafeLookup(void* view, int index, uint32_t* outID)
{
    if (view == NULL || g_exeBase == 0) {
        if (outID) *outID = 0;
        return NULL;
    }
    void* item = NULL;
    __try {
        LookupFn fn = reinterpret_cast<LookupFn>(g_exeBase + kLookupRva);
        item = fn(view, index);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        if (outID) *outID = 0xFFFFFFFFu;
        return NULL;
    }
    if (outID) {
        *outID = ReadItemID(item);
    }
    return item;
}

static void ProbeViews(int slot)
{
    static const int kProbeSlots[] = { 0, 1, 2, 3, 4, 5, 6, 140 };
    for (int i = 0; i < 16; ++i) {
        void* view = NULL;
        __try {
            view = *reinterpret_cast<void**>(g_exeBase + kViewsRva + i * 4);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            view = NULL;
        }
        if (view == NULL) {
            continue;
        }
        char ids[512];
        ids[0] = 0;
        char* cursor = ids;
        for (int k = 0; k < (int)(sizeof(kProbeSlots) / sizeof(kProbeSlots[0])); ++k) {
            uint32_t id = 0;
            SafeLookup(view, kProbeSlots[k], &id);
            cursor += wsprintfA(cursor, "%d:%u ", kProbeSlots[k], id);
        }
        Logf("      view[%d]=%p ids{ %s}", i, view, ids);
    }
}

// --------------------------------------------------------------- hook sites ---

// 0x01D0B9F6 处被替换的指令是 mov ecx, [0x51B2764]；ebp+0xF = 物品空间（1 字节），
// ebp-4 = 槽位（低 16 位），esi = 原生解析出的物品对象。
// 结果窗到底画哪件：客户端全局 0x518B798（"结果窗目标 key"）决定，和窗口参数里的物品无关。
// 这里把它当前的值、以及它经每个候选视图解析出的物品 id 全部记下来：
// 只要弹窗显示的那件东西出现在下面某一行里，就能确定窗口用的是哪个视图，从而知道该往
// 0x518B798 写什么值才能让弹窗显示被附魔的宠物。
static const uintptr_t kResultWindowKeyRva = 0x04D8B798;  // 0x518B798
static const uintptr_t kResultWindowModeRva = 0x010DB294; // 0x50DB294

static void LogResultWindowKey(void)
{
    if (g_exeBase == 0) {
        return;
    }
    uint32_t key = 0;
    uint32_t mode = 0;
    __try {
        key = *reinterpret_cast<uint32_t*>(g_exeBase + kResultWindowKeyRva);
        mode = *reinterpret_cast<uint32_t*>(g_exeBase + kResultWindowModeRva);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("[key] read failed");
        return;
    }
    Logf("[key] result-window key=0x%08X mode=%u", key, mode);
    for (int i = 0; i < 8; ++i) {
        void* view = NULL;
        __try {
            view = *reinterpret_cast<void**>(g_exeBase + kViewsRva + i * 4);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            view = NULL;
        }
        if (view == NULL) {
            continue;
        }
        uint32_t id = 0;
        SafeLookup(view, static_cast<int>(key), &id);
        if (id != 0) {
            Logf("      key-resolve view[%d]=%p id=%u", i, view, id);
        }
    }
}

// v5：定位"结果窗渲染时用的那处残留槽位状态"。
// 已知：弹窗画的是 view[6][140]（= 上一次宠物装备附魔的槽位），而本次 ACK 里的
// 物品/槽位(0)/key(0x518B798=0) 全都是对的。所以渲染时读的是别处保存的状态。
// 这里在附魔成功那一刻：解析出"宠物物品对象"和"140 号槽的物品对象"，再按窗口 id
// 枚举 UI 管理器里的窗口对象，逐个扫描其中是否出现 140 这个槽位值或这两个物品指针。
typedef void* (__thiscall *UiWindowGetFn)(void* manager, int windowId);

static void ScanForNeedles(void* region, unsigned int size, const char* label,
    unsigned int slotValue, void* artifactItem, void* petItem)
{
    if (region == NULL) {
        return;
    }
    unsigned int hits = 0;
    unsigned char* base = reinterpret_cast<unsigned char*>(region);
    for (unsigned int off = 0; off + 4 <= size && hits < 10; off += 4) {
        unsigned int v = 0;
        __try {
            v = *reinterpret_cast<unsigned int*>(base + off);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            return;
        }
        const char* what = NULL;
        if (v == slotValue) {
            what = "SLOT-VALUE";
        } else if (artifactItem != NULL && v == (unsigned int)(uintptr_t)artifactItem) {
            what = "ARTIFACT-PTR";
        } else if (petItem != NULL && v == (unsigned int)(uintptr_t)petItem) {
            what = "PET-PTR";
        }
        if (what != NULL) {
            Logf("      %s+0x%03X = 0x%08X  %s", label, off, v, what);
            hits++;
        }
    }
}

// v6：直接在进程内存里找"谁保存了那一刻残留的物品对象指针"。
// v5 已证明：附魔那一刻 view[6][140] 的物品对象指针是 5A421C00（绿色鹰翼图腾），
// 而弹窗渲染时用的就是它。这里按 4 字节对齐扫描可读内存（分块读、总量与命中都设上限、
// 全程 SEH 保护，只读），把保存该指针的地址连同上下文一起记下来。
static uintptr_t g_exeEnd = 0;
static uintptr_t g_selfBase = 0;
static uintptr_t g_selfEnd = 0;

static const char* RegionOwner(unsigned int addr)
{
    if (addr >= (unsigned int)g_exeBase && addr < (unsigned int)g_exeEnd) {
        return "exe";
    }
    if (g_selfBase && addr >= (unsigned int)g_selfBase && addr < (unsigned int)g_selfEnd) {
        return "probe";
    }
    return "other";
}

static void ScanProcessForPointer(unsigned int target)
{
    if (target == 0) {
        return;
    }
    SYSTEM_INFO si;
    GetSystemInfo(&si);
    unsigned char* addr = reinterpret_cast<unsigned char*>(si.lpMinimumApplicationAddress);
    unsigned char* limit = reinterpret_cast<unsigned char*>(si.lpMaximumApplicationAddress);
    const unsigned long long kScanBudget = 96ull << 20;   // 最多扫 96MB 可读内存
    unsigned long long scanned = 0;
    unsigned int hits = 0;
    static unsigned char buffer[64 * 1024];
    MEMORY_BASIC_INFORMATION mbi;

    while (addr < limit && hits < 12 && scanned < kScanBudget) {
        if (VirtualQuery(addr, &mbi, sizeof(mbi)) != sizeof(mbi)) {
            break;
        }
        unsigned int regionSize = static_cast<unsigned int>(mbi.RegionSize);
        bool readable = mbi.State == MEM_COMMIT &&
            (mbi.Protect & (PAGE_READONLY | PAGE_READWRITE | PAGE_WRITECOPY |
                            PAGE_EXECUTE_READ | PAGE_EXECUTE_READWRITE | PAGE_EXECUTE_WRITECOPY)) != 0 &&
            (mbi.Protect & (PAGE_NOACCESS | PAGE_GUARD)) == 0;
        if (readable && regionSize >= 4 && regionSize <= (64u << 20)) {
            unsigned char* base = reinterpret_cast<unsigned char*>(mbi.BaseAddress);
            for (unsigned int off = 0; off + 4 <= regionSize; off += sizeof(buffer)) {
                unsigned int want = regionSize - off;
                unsigned int chunk = want < sizeof(buffer) ? want : sizeof(buffer);
                SIZE_T got = 0;
                bool ok = false;
                __try {
                    ok = ReadProcessMemory(GetCurrentProcess(), base + off, buffer, chunk, &got) != 0;
                }
                __except (EXCEPTION_EXECUTE_HANDLER) {
                    ok = false;
                }
                if (!ok) {
                    break;
                }
                for (unsigned int i = 0; i + 4 <= static_cast<unsigned int>(got); i += 4) {
                    unsigned int v = *reinterpret_cast<unsigned int*>(buffer + i);
                    if (v != target) {
                        continue;
                    }
                    unsigned int at = static_cast<unsigned int>(
                        reinterpret_cast<uintptr_t>(base + off + i));
                    Logf("      HIT neighbor-ptr at 0x%08X (%s) region=0x%08X+0x%X",
                        at, RegionOwner(at), static_cast<unsigned int>(reinterpret_cast<uintptr_t>(base)),
                        regionSize);
                    for (int k = -4; k <= 4; ++k) {
                        unsigned int probe = at + static_cast<unsigned int>(k * 4);
                        unsigned int value = 0;
                        __try {
                            value = *reinterpret_cast<unsigned int*>(probe);
                        }
                        __except (EXCEPTION_EXECUTE_HANDLER) {
                            value = 0;
                        }
                        if (value != 0) {
                            Logf("          [%+d] 0x%08X = 0x%08X", k, probe, value);
                        }
                    }
                    if (++hits >= 12) {
                        break;
                    }
                }
                scanned += got;
                if (hits >= 12 || scanned >= kScanBudget) {
                    break;
                }
            }
        }
        addr += regionSize ? regionSize : 0x1000;
    }
    Logf("[scan] pointer scan done scanned=%uMB hits=%u", static_cast<unsigned>(scanned >> 20), hits);
}

static void ScanUiStateForStaleSlot(int slot)
{
    if (g_exeBase == 0) {
        return;
    }
    void* petView = NULL;
    void* uiManager = NULL;
    __try {
        petView = *reinterpret_cast<void**>(g_exeBase + kViewsRva + 6 * 4);
        uiManager = *reinterpret_cast<void**>(g_exeBase + 0x04DB2764);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        Log("[scan] view/manager read failed");
        return;
    }
    void* petItem = SafeLookup(petView, slot, NULL);
    void* artifactItem = SafeLookup(petView, 140, NULL);
    Logf("[scan] slot=%d pet=%p(pet_id=%u) slot140=%p(id=%u) ui_manager=%p",
        slot, petItem, ReadItemID(petItem), artifactItem, ReadItemID(artifactItem), uiManager);
    if (artifactItem != NULL && petItem != NULL && artifactItem != petItem) {
        // v6 的全进程指针扫描只命中本探针自己的栈拷贝、且占满预算，已停用：
        // 改为在 v7 用"结果窗打开函数"钩子直接看是谁用什么物品开的窗。
        Logf("[scan] skipping process pointer scan (see [open] lines instead)");
    }
    if (uiManager != NULL) {
        ScanForNeedles(uiManager, 0x800, "uimgr", static_cast<unsigned int>(slot),
            artifactItem, petItem);
    }
}

void __cdecl LogEnchantAck(unsigned int listType, int slot, void* nativeItem)
{
    if (InterlockedIncrement(&g_ackLogs) > 60) {
        return;
    }
    Logf("[ack] list=%u slot=%d native=%p native_id=%u", listType, slot, nativeItem,
        ReadItemID(nativeItem));
    if (listType == 7) {
        ProbeViews(slot);
        LogResultWindowKey();
        ScanUiStateForStaleSlot(slot);
    }
}

__declspec(naked) void ProxyEnchantAck()
{
    __asm {
        // 0x01D0B9F6 原本是"没有函数调用"的位置：其后紧接 push/push/push/call 开结果窗。
        // 插进来的 C 调用会破坏 xmm0-7 与 x87 栈（CRT/系统 DLL 内部都会用），
        // 因此这里把两类易失状态一并保存/恢复，保证对原生代码完全透明。
        pushfd
        pushad
        sub  esp, 240                 // 128(xmm) + 108(x87 fnsave) + 4(对齐)
        movups xmmword ptr [esp + 0], xmm0
        movups xmmword ptr [esp + 16], xmm1
        movups xmmword ptr [esp + 32], xmm2
        movups xmmword ptr [esp + 48], xmm3
        movups xmmword ptr [esp + 64], xmm4
        movups xmmword ptr [esp + 80], xmm5
        movups xmmword ptr [esp + 96], xmm6
        movups xmmword ptr [esp + 112], xmm7
        fnsave [esp + 128]

        movzx eax, byte ptr [ebp + 0Fh]
        movsx ecx, word ptr [ebp - 4]
        push esi
        push ecx
        push eax
        call LogEnchantAck
        add  esp, 12

        frstor [esp + 128]
        movups xmm0, xmmword ptr [esp + 0]
        movups xmm1, xmmword ptr [esp + 16]
        movups xmm2, xmmword ptr [esp + 32]
        movups xmm3, xmmword ptr [esp + 48]
        movups xmm4, xmmword ptr [esp + 64]
        movups xmm5, xmmword ptr [esp + 80]
        movups xmm6, xmmword ptr [esp + 96]
        movups xmm7, xmmword ptr [esp + 112]
        add  esp, 240
        popad
        popfd
        // 重放 0x01D0B9F6 的原始指令：mov ecx, dword ptr [0x51B2764]。
        // 必须按字节写：MSVC 内联汇编会把 "mov ecx, dword ptr [0x51B2764]" 编成
        // mov ecx, imm32（把地址当立即数），ecx 就变成了 0x51B2764 而不是管理器指针，
        // 于是紧随其后的开结果窗 call 拿到垃圾 this，弹窗彻底不出现。
        // 现场 2026-09-19：解析完全正确（native_id=被附魔的宠物）却永不弹窗，就是这个原因。
        _emit 0x8B        // mov ecx, dword ptr [0x51B2764]
        _emit 0x0D
        _emit 0x64
        _emit 0x27
        _emit 0x1B
        _emit 0x05
        jmp  dword ptr [g_siteAResume]
    }
}

// op14 行处理里"同模板已存在物品"的路径：esi = 物品对象，ebp-0x17D = 列表，ebp-0x17C = 槽位。
// 若 90CN.dll 已经在这里挂钩（首字节 0xE9），本 DLL 不抢这个点，只记录跳过。
void __cdecl LogPetRow(unsigned int listType, int slot, void* item)
{
    if (listType != 7 && !(listType == 3 && slot == 26)) {
        return;
    }
    if (InterlockedIncrement(&g_rowLogs) > 60) {
        return;
    }
    Logf("[row] list=%u slot=%d item=%p item_id=%u", listType, slot, item, ReadItemID(item));
}

__declspec(naked) void ProxyPetRow()
{
    __asm {
        pushfd
        pushad
        sub  esp, 240
        movups xmmword ptr [esp + 0], xmm0
        movups xmmword ptr [esp + 16], xmm1
        movups xmmword ptr [esp + 32], xmm2
        movups xmmword ptr [esp + 48], xmm3
        movups xmmword ptr [esp + 64], xmm4
        movups xmmword ptr [esp + 80], xmm5
        movups xmmword ptr [esp + 96], xmm6
        movups xmmword ptr [esp + 112], xmm7
        fnsave [esp + 128]

        movzx eax, byte ptr [ebp - 17Dh]
        movsx ecx, word ptr [ebp - 17Ch]
        push esi
        push ecx
        push eax
        call LogPetRow
        add  esp, 12

        frstor [esp + 128]
        movups xmm0, xmmword ptr [esp + 0]
        movups xmm1, xmmword ptr [esp + 16]
        movups xmm2, xmmword ptr [esp + 32]
        movups xmm3, xmmword ptr [esp + 48]
        movups xmm4, xmmword ptr [esp + 64]
        movups xmm5, xmmword ptr [esp + 80]
        movups xmm6, xmmword ptr [esp + 96]
        movups xmm7, xmmword ptr [esp + 112]
        add  esp, 240
        popad
        popfd
        movsx eax, word ptr [ebp - 17Ch]
        jmp  dword ptr [g_siteBResume]
    }
}

// ------------------------------------------------------------------- install ---

static bool BytesMatch(const unsigned char* at, const unsigned char* want, size_t n)
{
    for (size_t i = 0; i < n; ++i) {
        if (at[i] != want[i]) {
            return false;
        }
    }
    return true;
}

static bool PatchJump(uintptr_t site, size_t len, void* proxy)
{
    DWORD oldProtect = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(site), len, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        return false;
    }
    unsigned char* at = reinterpret_cast<unsigned char*>(site);
    at[0] = 0xE9;
    intptr_t relative = reinterpret_cast<intptr_t>(proxy) - static_cast<intptr_t>(site + 5);
    *reinterpret_cast<int32_t*>(at + 1) = static_cast<int32_t>(relative);
    for (size_t i = 5; i < len; ++i) {
        at[i] = 0x90;
    }
    DWORD ignored = 0;
    VirtualProtect(reinterpret_cast<void*>(site), len, oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), reinterpret_cast<void*>(site), len);
    return true;
}

// 打包 EXE 会较晚才恢复 .text，所以等待签名出现再挂钩。
static bool WaitForBytes(uintptr_t site, const unsigned char* want, size_t len, int limitMs)
{
    int waited = 0;
    while (waited < limitMs) {
        if (BytesMatch(reinterpret_cast<const unsigned char*>(site), want, len)) {
            return true;
        }
        Sleep(200);
        waited += 200;
    }
    return false;
}

// v7：钩住"结果窗打开函数" sub_2FDD1A0 的入口（10 字节 prologue 整段重放），
// 记录每一次以窗口 id 0x3D9 开窗时：传进去的物品对象及其 id、第三个参数、以及
// **调用方返回地址**（= 到底哪段代码开的窗，是 op272 成功分支还是别的 UI 流程）。
// 这一条直接回答"弹窗画的是谁给的东西"。
static const uintptr_t kWindowOpenRva = 0x02BDD1A0;       // VA 0x02FDD1A0 - 0x400000
static const uintptr_t kWindowOpenResumeRva = 0x02BDD1AA; // VA 0x02FDD1AA - 0x400000
static uintptr_t g_windowOpenResume = 0;
static LONG g_windowOpenLogs = 0;

void __cdecl LogWindowOpen(unsigned int caller, unsigned int windowId, void* item,
    unsigned int arg3, void* self)
{
    if (windowId != 0x3D9) {
        return;
    }
    if (InterlockedIncrement(&g_windowOpenLogs) > 40) {
        return;
    }
    unsigned int rva = caller >= 0x400000 ? caller - 0x400000 : caller;
    Logf("[open] window=0x%X item=%p item_id=%u arg3=0x%X self=%p caller=0x%08X (rva=0x%06X)",
        windowId, item, ReadItemID(item), arg3, self, caller, rva);
}

__declspec(naked) void ProxyWindowOpen()
{
    __asm {
        pushfd
        pushad
        sub  esp, 240
        movups xmmword ptr [esp + 0], xmm0
        movups xmmword ptr [esp + 16], xmm1
        movups xmmword ptr [esp + 32], xmm2
        movups xmmword ptr [esp + 48], xmm3
        movups xmmword ptr [esp + 64], xmm4
        movups xmmword ptr [esp + 80], xmm5
        movups xmmword ptr [esp + 96], xmm6
        movups xmmword ptr [esp + 112], xmm7
        fnsave [esp + 128]

        // 入口处：[esp]=返回地址、[esp+4]=arg1(窗口id)、[esp+8]=arg2(物品)、[esp+0xC]=arg3；
        // pushad 里 ecx 存在 [esp+4]（原 this）。上面又减了 240 + 12 字节。
        mov  eax, dword ptr [esp + 288 + 0]    // 返回地址（240 + 12 + 36 = 288）
        mov  ecx, dword ptr [esp + 288 + 4]    // arg1
        mov  edx, dword ptr [esp + 288 + 8]    // arg2
        mov  ebx, dword ptr [esp + 288 + 12]   // arg3
        mov  esi, dword ptr [esp + 4]          // 原 ecx = this
        push esi
        push ebx
        push edx
        push ecx
        push eax
        call LogWindowOpen
        add  esp, 20

        frstor [esp + 128]
        movups xmm0, xmmword ptr [esp + 0]
        movups xmm1, xmmword ptr [esp + 16]
        movups xmm2, xmmword ptr [esp + 32]
        movups xmm3, xmmword ptr [esp + 48]
        movups xmm4, xmmword ptr [esp + 64]
        movups xmm5, xmmword ptr [esp + 80]
        movups xmm6, xmmword ptr [esp + 96]
        movups xmm7, xmmword ptr [esp + 112]
        add  esp, 240
        popad
        popfd

        // 原样重放被覆盖的 10 字节 prologue：
        //   push ebp / mov ebp, esp / push -1 / push 0x03EC9A68
        _emit 0x55
        _emit 0x8B
        _emit 0xEC
        _emit 0x6A
        _emit 0xFF
        _emit 0x68
        _emit 0x68
        _emit 0x9A
        _emit 0xEC
        _emit 0x03
        jmp  dword ptr [g_windowOpenResume]
    }
}

static bool InstallWindowOpenHook(void)
{
    uintptr_t target = g_exeBase + kWindowOpenRva;
    static const unsigned char kExpected[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x68, 0x9A, 0xEC, 0x03
    };
    if (!WaitForBytes(target, kExpected, sizeof(kExpected), 180000)) {
        Logf("window-open hook signature unavailable site=0x%08X", (unsigned)target);
        return false;
    }
    g_windowOpenResume = g_exeBase + kWindowOpenResumeRva;
    if (!PatchJump(target, sizeof(kExpected), reinterpret_cast<void*>(&ProxyWindowOpen))) {
        Logf("window-open hook patch failed site=0x%08X", (unsigned)target);
        return false;
    }
    Logf("window-open hook installed site=0x%08X proxy=%p resume=0x%08X",
        (unsigned)target, reinterpret_cast<void*>(&ProxyWindowOpen), (unsigned)g_windowOpenResume);
    return true;
}

static DWORD WINAPI ProbeThread(LPVOID)
{
    Sleep(3000);
    for (int i = 0; i < 600 && g_exeBase == 0; ++i) {
        HMODULE base = GetModuleHandleA(NULL);
        if (base != NULL) {
            g_exeBase = reinterpret_cast<uintptr_t>(base);
        } else {
            Sleep(200);
        }
    }
    Logf("probe alive exe_base=0x%08X pid=%u", (unsigned)g_exeBase, (unsigned)GetCurrentProcessId());
    if (g_exeBase == 0) {
        Log("DNF.exe base unavailable; abort");
        return 0;
    }
    {
        // 记录 exe 与自身模块的范围，便于把扫描命中归类。
        __try {
            unsigned char* p = reinterpret_cast<unsigned char*>(g_exeBase);
            unsigned int peOff = *reinterpret_cast<unsigned int*>(p + 0x3C);
            unsigned int optSize = *reinterpret_cast<unsigned short*>(p + peOff + 20);
            unsigned int imageSize = *reinterpret_cast<unsigned int*>(p + peOff + 24 + optSize - 4);
            g_exeEnd = g_exeBase + imageSize;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            g_exeEnd = g_exeBase + 0x10000000;
        }
        HMODULE self = NULL;
        if (GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                reinterpret_cast<LPCSTR>(&ProbeThread), &self) && self != NULL) {
            g_selfBase = reinterpret_cast<uintptr_t>(self);
            g_selfEnd = g_selfBase + 0x200000;
        }
        Logf("module ranges exe=0x%08X..0x%08X self=0x%08X..0x%08X",
            (unsigned)g_exeBase, (unsigned)g_exeEnd, (unsigned)g_selfBase, (unsigned)g_selfEnd);
    }

    static const unsigned char kExpectA[] = { 0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05 };
    uintptr_t siteA = g_exeBase + kSiteA;
    if (WaitForBytes(siteA, kExpectA, sizeof(kExpectA), 180000)) {
        g_siteAResume = g_exeBase + kSiteAResume;
        if (PatchJump(siteA, sizeof(kExpectA), reinterpret_cast<void*>(&ProxyEnchantAck))) {
            Logf("hook A installed site=0x%08X proxy=%p resume=0x%08X", (unsigned)siteA,
                reinterpret_cast<void*>(&ProxyEnchantAck), (unsigned)g_siteAResume);
        } else {
            Logf("hook A patch failed site=0x%08X", (unsigned)siteA);
        }
    } else {
        unsigned char now[8] = { 0 };
        for (int i = 0; i < 8; ++i) {
            now[i] = *reinterpret_cast<unsigned char*>(siteA + i);
        }
        Logf("hook A signature unavailable site=0x%08X bytes=%02X%02X%02X%02X%02X%02X%02X%02X",
            (unsigned)siteA, now[0], now[1], now[2], now[3], now[4], now[5], now[6], now[7]);
    }

    static const unsigned char kExpectB[] = { 0x0F, 0xBF, 0x85, 0x84, 0xFE, 0xFF, 0xFF };
    uintptr_t siteB = g_exeBase + kSiteB;
    if (BytesMatch(reinterpret_cast<const unsigned char*>(siteB), kExpectB, sizeof(kExpectB))) {
        g_siteBResume = g_exeBase + kSiteBResume;
        if (PatchJump(siteB, sizeof(kExpectB), reinterpret_cast<void*>(&ProxyPetRow))) {
            Logf("hook B installed site=0x%08X proxy=%p", (unsigned)siteB,
                reinterpret_cast<void*>(&ProxyPetRow));
        } else {
            Logf("hook B patch failed site=0x%08X", (unsigned)siteB);
        }
    } else {
        unsigned char first = *reinterpret_cast<unsigned char*>(siteB);
        Logf("hook B skipped site=0x%08X first_byte=0x%02X (already hooked elsewhere=%d)",
            (unsigned)siteB, first, first == 0xE9 ? 1 : 0);
    }

    InstallWindowOpenHook();
    return 0;
}

BOOL APIENTRY DllMain(HMODULE module, DWORD reason, LPVOID)
{
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(module);
        InitializeCriticalSection(&g_logLock);
        LogOpen();
        Logf("pet enchant probe attached module=%p", module);
        HANDLE thread = CreateThread(NULL, 0, ProbeThread, NULL, 0, NULL);
        if (thread != NULL) {
            CloseHandle(thread);
        }
    }
    return TRUE;
}
