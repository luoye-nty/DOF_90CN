/*
 * DNFHelper.c - 登录器内嵌 Helper DLL（精简版）
 *
 * 功能：顺图（Alt+方向键过图）+ 自动拾取（op43 发包）
 * 门控：命名共享内存 Local\YYBDNFHelper90，由登录器进程创建并写入开关标志；
 *       开关关闭（或共享内存不存在）时本 DLL 完全静默，不执行任何动作。
 *
 * 设计要点：
 *  - 无 ini、无日志文件，注入后无感
 *  - 公告由登录器写入共享内存 announce_seq/announce_msg，DLL 检测 seq 变化
 *    自动上喇叭广播；msg 先拷入 static 缓冲再发送（喇叭异步渲染须稳定内存）
 *  - 由登录器以随机文件名释放到临时目录并注入，注入后删除防提取
 *  - 对应 90 版本，地址见 基址.h
 *
 * 编译: build.bat (gcc -m32 -shared -static-libgcc -O2)
 */

#include "基址.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ──── 共享内存门控 ──── */
#define SHARED_MAP_NAME "Local\\YYBDNFHelper90"

typedef struct {
    volatile LONG pass_enabled;    /* 0x00 顺图开关 */
    volatile LONG pickup_enabled;  /* 0x04 拾取开关 */
    volatile LONG announce_seq;    /* 0x08 公告自增序号 */
    volatile LONG announce_color;  /* 0x0C 公告颜色 RGBA */
    wchar_t      announce_msg[200];/* 0x10 公告内容 UTF-16 */
} HelperState;
/* HelperState 固定 0x1A0=416 字节，其后为私有通知区：8 槽 × 544B
   { account[128] ASCII, color int32 @128, seq int32 @132, msg wchar[200] @136, ts int64 @536 }
   account_id 实际为 "dnf:"+sha256hex（68 字符），64B 放不下，故扩到 128。 */
#define PRIV_BASE_OFF     0x1A0
#define PRIV_SLOTS        8
#define PRIV_ENTRY_SIZE   544
#define PRIV_ACC_OFF      0
#define PRIV_ACC_LEN      128
#define PRIV_COLOR_OFF    128
#define PRIV_SEQ_OFF      132
#define PRIV_MSG_OFF      136
#define PRIV_TS_OFF       536
#define MAP_TOTAL         4768  /* 416 + 8*544 */

static HANDLE g_map = NULL;
static HelperState* g_state = NULL;
static int g_map_retry = 0;

static volatile int g_running = 1;
static int g_pass_lf[4] = { 0, 0, 0, 0 };  /* 顺图键上次状态 */
static int g_secondary = 0;                /* 副账号进程标志：1=完全静默(仅顺图/拾取) */
static LONG g_last_announce_seq = 0;       /* 上次已广播的公告序号 */
static wchar_t g_ann_cpy[200];             /* 共享内存公告文本的 static 拷贝（喇叭异步渲染须稳定缓冲） */
static char g_account[PRIV_ACC_LEN] = { 0 };   /* 本进程账号（DNF_ACCOUNT_ID） */
static LONG g_slot_seen[PRIV_SLOTS];       /* 私有区各槽上次已提示的序号 */
static int g_priv_inited = 0;              /* 私有区首映射只记基线，不补发历史 */
static wchar_t g_priv_cpy[PRIV_SLOTS][200];/* 私有提示 static 拷贝：每槽独立缓冲，防连发互相覆盖 */

/* 打开/映射共享内存；失败时每 20 轮(~1s)重试一次，直到登录器创建或进程退出 */
static void ensure_shared(void)
{
    if (g_state) return;
    if (!g_map) {
        if ((g_map_retry++ % 20) != 0) return;
        g_map = OpenFileMappingA(FILE_MAP_ALL_ACCESS, FALSE, SHARED_MAP_NAME);
        if (!g_map) return;
    }
    g_state = (HelperState*)MapViewOfFile(g_map, FILE_MAP_ALL_ACCESS, 0, 0, MAP_TOTAL);
}

static void release_shared(void)
{
    if (g_state) { UnmapViewOfFile(g_state); g_state = NULL; }
    if (g_map)   { CloseHandle(g_map);        g_map = NULL; }
}

/* ──── 基础读取 ──── */
static __inline int read_int(int addr)
{
    if (addr < 0x00400000 || addr > 0x7FFEFFFF) return 0;
    return *(volatile int*)addr;
}

/* ---- 系统公告：直接构造消息结构体经 CALL_MSG_DISPATCH 投递（type=0x1E 喇叭）----
 * 旧路径 CALL_FORMAT_MSG+CALL_HORN_NOTIFY 会忽略 color 参数、按 type 查表取色，
 * 导致公告/泡点颜色无法控制（都成深色）。改为显式填 desc.color 与 type=0x1E，
 * 既保持喇叭通道又能指定颜色。msg 先拷入 64 槽环形 static 缓冲保证稳定。 */
static void system_notify(const wchar_t* msg, int color)
{
    int shop = read_int(ADDR_SHOP_BASE);
    if (!shop) return;
    if (!msg || msg[0] == L'\0') return;

    /* this = [shop+0x50]（公告对象） */
    int this_obj = *(int*)(shop + 0x50);
    if (!this_obj) return;

    static wchar_t s_ring[64][200];
    static int s_ring_i = 0;
    wchar_t* buf = s_ring[s_ring_i];
    s_ring_i = (s_ring_i + 1) & 63;
    int n = 0;
    while (n < 199 && msg[n] != L'\0') { buf[n] = msg[n]; n++; }
    buf[n] = L'\0';

    /* 0x40 字节消息结构体，用初始化函数填默认值 */
    char desc[0x40];
    __asm__ __volatile__(
        "movl %[dst], %%ecx\n\t"
        "call *%[init]\n\t"
        :
        : [dst]"r"(desc), [init]"r"(CALL_MSG_INIT)
        : "eax","ecx","edx","memory"
    );

    /* 填消息文本 + 颜色 + 类型（type=0x1E 保持喇叭） */
    *(const wchar_t**)(desc + 0x04) = buf;
    *(unsigned int*)(desc + 0x14) = (unsigned int)color;
    *(unsigned int*)(desc + 0x18) = 0x1E;

    /* 调用消息投递 */
    __asm__ __volatile__(
        "pushl %[desc]\n\t"
        "movl  %[obj], %%ecx\n\t"
        "call  *%[fn]\n\t"
        :
        : [desc]"r"(desc), [obj]"r"(this_obj), [fn]"r"(CALL_MSG_DISPATCH)
        : "eax","ecx","edx","memory"
    );
}

/* ──── 聊天栏消息（CALL_MSG_DISPATCH type=2，UTF-16） ──── */
/* 泡点到账提示走聊天栏，与全服公告的喇叭通道(type=30)分开，避免相互干扰。 */
static void chat_notify(const wchar_t* msg, unsigned int type, unsigned int color)
{
    int shop = read_int(ADDR_SHOP_BASE);
    if (!shop) return;
    if (!msg || msg[0] == L'\0') return;

    /* this = [shop+0x50]（同喇叭公告对象） */
    int this_obj = *(int*)(shop + 0x50);
    if (!this_obj) return;

    /* 拷贝到独立环形缓冲，保证异步渲染期间稳定 */
    static wchar_t s_chat_ring[16][200];
    static int s_chat_i = 0;
    wchar_t* buf = s_chat_ring[s_chat_i];
    s_chat_i = (s_chat_i + 1) & 15;
    int n = 0;
    while (n < 199 && msg[n] != L'\0') { buf[n] = msg[n]; n++; }
    buf[n] = L'\0';

    /* 0x40 字节消息结构体，用初始化函数填默认值 */
    char desc[0x40];
    __asm__ __volatile__(
        "movl %[dst], %%ecx\n\t"
        "call *%[init]\n\t"
        :
        : [dst]"r"(desc), [init]"r"(CALL_MSG_INIT)
        : "eax","ecx","edx","memory"
    );

    /* 填入消息文本 + 颜色 + 类型 */
    *(const wchar_t**)(desc + 0x04) = buf;
    *(unsigned int*)(desc + 0x14) = color;
    *(unsigned int*)(desc + 0x18) = type;

    /* 调用消息投递 */
    __asm__ __volatile__(
        "pushl %[desc]\n\t"
        "movl  %[obj], %%ecx\n\t"
        "call  *%[fn]\n\t"
        :
        : [desc]"r"(desc), [obj]"r"(this_obj), [fn]"r"(CALL_MSG_DISPATCH)
        : "eax","ecx","edx","memory"
    );
}

/* ──── 私有到账提示（泡点点券等按账号隔离绿字） ──── */
/* 轮询私有区各槽位：序号变化且账号等于本进程账号才提示；首映射只记录基线不补发。
   msg 先拷入本槽 static 缓冲再发送：喇叭异步渲染，直传共享内存视图会乱码。 */
static void priv_poll(void)
{
    if (!g_state || g_account[0] == '\0') return;

    char* base = (char*)g_state + PRIV_BASE_OFF;
    if (!g_priv_inited) {
        for (int i = 0; i < PRIV_SLOTS; i++) {
            char* e = base + i * PRIV_ENTRY_SIZE;
            g_slot_seen[i] = *(volatile LONG*)(e + PRIV_SEQ_OFF);
        }
        g_priv_inited = 1;
        return;
    }

    for (int i = 0; i < PRIV_SLOTS; i++) {
        char* e = base + i * PRIV_ENTRY_SIZE;
        LONG seq = *(volatile LONG*)(e + PRIV_SEQ_OFF);
        if (seq == g_slot_seen[i]) continue;
        g_slot_seen[i] = seq;
        if (seq == 0) continue;
        if (memcmp(e + PRIV_ACC_OFF, g_account, PRIV_ACC_LEN) != 0) continue; /* 非本人不显示 */
        if (*(volatile wchar_t*)(e + PRIV_MSG_OFF) == L'\0') continue;
        int nc = 0;
        while (nc < 199 && *(volatile wchar_t*)(e + PRIV_MSG_OFF + nc * 2) != L'\0') {
            g_priv_cpy[i][nc] = *(volatile wchar_t*)(e + PRIV_MSG_OFF + nc * 2);
            nc++;
        }
        g_priv_cpy[i][nc] = L'\0';
        int color = *(volatile int*)(e + PRIV_COLOR_OFF);
        if (color == 0) color = 0xFF00FF00;
        system_notify(g_priv_cpy[i], color);
    }
}

/* 地图对象 == 0 即城镇 */
static int is_in_town(void)
{
    int player = read_int(ADDR_PLAYER_BASE);
    if (!player) return 1;
    return read_int(player + OFF_MAP) == 0;
}

/* ──── 顺图: 三级指针链获取 pass 对象 ──── */
static int get_pass_object(void)
{
    int chain1 = read_int(ADDR_PASS_CHAIN_1);
    if (!chain1) return 0;
    int chain2 = read_int(chain1 + OFF_PASS_CHAIN_2);
    if (!chain2) return 0;
    int pass = read_int(chain2 + OFF_PASS);
    if (!pass || pass < 0x00400000 || pass > 0x7FFEFFFF) return 0;
    return pass;
}

/* 过图: direction 0左 1右 2上 3下 */
static void door_pass(int direction)
{
    int pass = get_pass_object();
    if (!pass) return;

    /* 冷却绕过: 每次调用前清零初始化标志与时间戳, 强制走首次路径 */
    *(volatile int*)0x51872A4 &= ~1;
    *(volatile int*)0x51872A0 = 0;

    __asm__ __volatile__(
        "pushl $-1\n\t"         /* room_y = -1 (auto) */
        "pushl $-1\n\t"         /* room_x = -1 (auto) */
        "pushl $0\n\t"          /* gap */
        "pushl $0\n\t"          /* reserved */
        "pushl $0\n\t"          /* list_end */
        "pushl $0\n\t"          /* list_begin */
        "pushl $0\n\t"          /* [ebp+0xC] = NULL */
        "pushl %[dir]\n\t"      /* direction */
        "movl  %[pass], %%ecx\n\t"
        "movl  %[fn], %%eax\n\t"
        "call  *%%eax\n\t"
        :
        : [dir]"r"(direction), [pass]"r"(pass), [fn]"i"(CALL_DOOR_PASS)
        : "eax","ecx","edx","memory"
    );
}

/* ──── 拾取: op43 组包发包 ──── */
static int decrypt_drop_key(int addr)
{
    int e = read_int(addr);               /* [obj+0xA0] */
    int db = read_int(ADDR_DECRYPT);
    if (!db || !e) return 0;
    int edx = e >> 16;
    edx = read_int(db + edx * 4 + 36);
    e = e & 0xFFFF;
    e = read_int(edx + e * 4 + 8468);
    edx = e & 0xFFFF;
    int esi = (edx << 16) | edx;
    esi ^= read_int(addr + 4);            /* [obj+0xA4] */
    return esi;
}

/* 组包发包: 缓冲(43)+加密(key,4)+加密(0,1)+加密(1,1)+7×加密(0,2)+发包 */
static void pickup_send(int drop_key)
{
    int base = read_int(ADDR_SEND_BASE);
    if (!base) return;

    __asm__ __volatile__(
        "movl %[base], %%ecx\n\t"
        "pushl $43\n\t"
        "movl %[buf], %%eax\n\t"
        "call *%%eax\n\t"
        :
        : [base]"r"(base), [buf]"i"(CALL_BUF)
        : "eax","ecx","edx","memory");

    __asm__ __volatile__(
        "movl %[base], %%ecx\n\t"
        "pushl %[key]\n\t"
        "movl %[enc], %%eax\n\t"
        "call *%%eax\n\t"
        :
        : [base]"r"(base), [key]"r"(drop_key), [enc]"i"(CALL_ENC4)
        : "eax","ecx","edx","memory");

    __asm__ __volatile__(
        "movl %[base], %%ecx\n\t"
        "pushl $0\n\t"
        "movl %[enc], %%eax\n\t"
        "call *%%eax\n\t"
        :
        : [base]"r"(base), [enc]"i"(CALL_ENC1)
        : "eax","ecx","edx","memory");

    __asm__ __volatile__(
        "movl %[base], %%ecx\n\t"
        "pushl $1\n\t"
        "movl %[enc], %%eax\n\t"
        "call *%%eax\n\t"
        :
        : [base]"r"(base), [enc]"i"(CALL_ENC1)
        : "eax","ecx","edx","memory");

    for (int i = 0; i < 7; i++) {
        __asm__ __volatile__(
            "movl %[base], %%ecx\n\t"
            "pushl $0\n\t"
            "movl %[enc], %%eax\n\t"
            "call *%%eax\n\t"
            :
            : [base]"r"(base), [enc]"i"(CALL_ENC2)
            : "eax","ecx","edx","memory");
    }

    __asm__ __volatile__(
        "movl %[send], %%eax\n\t"
        "call *%%eax\n\t"
        :
        : [send]"i"(CALL_SEND)
        : "eax","ecx","edx","memory");
}

/* 遍历掉落物(type=289)逐个发包拾取 */
static void pickup_loot(void)
{
    int player = read_int(ADDR_PLAYER_BASE);
    if (!player) return;
    int map = read_int(player + OFF_MAP);
    if (!map) return;
    int head = read_int(map + OFF_HEAD);
    int tail = read_int(map + OFF_TAIL);
    if (head <= 0 || tail <= 0 || head >= tail) return;
    int count = (tail - head) / 4;
    if (count <= 0 || count > 10000) return;

    for (int i = 1; i <= count; i++) {
        int obj = read_int(head + i * 4);
        if (!obj || obj == player) continue;
        if (read_int(obj + OFF_OBJ_TYPE) != 289) continue;
        int key = decrypt_drop_key(obj + 0xA0);
        if (key <= 0) continue;
        pickup_send(key);
        Sleep(50);
    }
}

/* ──── worker_thread: 共享内存门控 + 顺图热键 + 拾取循环 ──── */
static DWORD WINAPI worker_thread(LPVOID p)
{
    (void)p;
    while (g_running) {
        ensure_shared();

        /* 私有到账提示：主/副账号进程都轮询，但只显示与自身 DNF_ACCOUNT_ID 匹配的消息 */
        priv_poll();

        /* 副账号进程（DNF_MULTI_CLIENT=1）：完全静默，不执行顺图/拾取，
           避免双开时两进程同时发包/过图造成副账号闪退 */
        if (g_secondary) {
            Sleep(200);
            continue;
        }
        /* 全服公告：announce_seq 变化即广播。msg 先拷入 static 缓冲再发送——
           喇叭接口为异步渲染，直接传共享内存视图/栈缓冲会读到失效内存导致乱码 */
        if (g_state) {
            LONG seq = g_state->announce_seq;
            if (seq != g_last_announce_seq) {
                g_last_announce_seq = seq;
                if (seq != 0 && g_state->announce_msg[0] != L'\0') {
                    int nc = 0;
                    while (nc < 199 && g_state->announce_msg[nc] != L'\0') {
                        g_ann_cpy[nc] = g_state->announce_msg[nc];
                        nc++;
                    }
                    g_ann_cpy[nc] = L'\0';
                    system_notify(g_ann_cpy,
                                  g_state->announce_color ? g_state->announce_color : 0xFF0000FF);
                }
            }
        }

        /* ── 90 版专属逻辑：顺图/拾取（基址.h 硬编码 90 版内存地址）── */
        int pass_on = g_state && g_state->pass_enabled;
        int pick_on = g_state && g_state->pickup_enabled;
        int in_town = is_in_town() ? 1 : 0;

        /* 顺图: Alt+方向键 (仅副本内且开关开启) */
        if (!in_town && pass_on) {
            if (GetAsyncKeyState(VK_MENU) & 0x8000) {
                int l = (GetAsyncKeyState(VK_LEFT)  & 0x8000) != 0;
                int r = (GetAsyncKeyState(VK_RIGHT) & 0x8000) != 0;
                int u = (GetAsyncKeyState(VK_UP)    & 0x8000) != 0;
                int d = (GetAsyncKeyState(VK_DOWN)  & 0x8000) != 0;
                if (l && !g_pass_lf[0]) door_pass(0);
                if (r && !g_pass_lf[1]) door_pass(1);
                if (u && !g_pass_lf[2]) door_pass(2);
                if (d && !g_pass_lf[3]) door_pass(3);
                g_pass_lf[0] = l; g_pass_lf[1] = r;
                g_pass_lf[2] = u; g_pass_lf[3] = d;
            } else {
                g_pass_lf[0] = g_pass_lf[1] = g_pass_lf[2] = g_pass_lf[3] = 0;
            }
        } else {
            g_pass_lf[0] = g_pass_lf[1] = g_pass_lf[2] = g_pass_lf[3] = 0;
        }

        /* 拾取: 仅副本内且开关开启 */
        if (!in_town && pick_on) {
            pickup_loot();
        }

        Sleep((pass_on || pick_on) ? 50 : 100);
    }
    return 0;
}

/* ──── PVF 密钥轮换补丁（90 版专用） ────
 * 登录器在启动 DNF.exe 前设置环境变量 DNF_PVF_KEYSET=<96位hex>（6 段 × 8B，
 * 顺序同管理工具 new_key_set: head hash grpi body stra strb），本 DLL attach
 * 时读取并安装补丁。行为说明：
 *  - DNF.exe 内 PVF 密钥对象经 6 处 call 0x34D9B20（通用 wide-blob 取回器）取得，
 *    消费方按返回指针读 4 WORD 作为 LCG 解密种子；
 *  - 本补丁将 6 处 call 的 rel32 目标改写为各自 stub，stub 返回本 DLL 内对应
 *    8B 新 key 的指针，绕过原 keyObj 只读种子与全局缓存，游戏即用新 key 解密；
 *  - 未设置 DNF_PVF_KEYSET（或长度非法）时完全静默，不改变任何行为；
 *  - 任一补丁点 opcode/原目标与预期不符（版本不匹配）时放弃整组安装，防止半补丁崩溃。
 * 补丁点 VA（imagebase 0x400000）与 key 段映射：
 *    0x0394A5B0(BODY) / 0x0394BA65(HEAD) / 0x0394BAF5(HASH)
 *    0x0394BC06(GRPI) / 0x03A53C1E(STRB) / 0x03A53D0D(STRA)
 */
#define PVF_IMG_BASE    0x00400000
#define PVF_GETKEY_RVA  (0x034D9B20 - PVF_IMG_BASE)  /* 原 6 处 call 共同目标 */

/* key_set 段序: 0=head 1=hash 2=grpi 3=body 4=stra 5=strb */
static unsigned char g_pvf_key[6][8];
static int g_pvf_installed = 0;

static const struct {
    unsigned va;      /* 补丁点 VA (imagebase 0x400000 时) */
    int      key_idx; /* 对应 g_pvf_key 段 */
} PVF_PATCH_SITES[] = {
    { 0x0394A5B0, 3 }, /* BODY */
    { 0x0394BA65, 0 }, /* HEAD */
    { 0x0394BAF5, 1 }, /* HASH */
    { 0x0394BC06, 2 }, /* GRPI */
    { 0x03A53C1E, 5 }, /* STRB */
    { 0x03A53D0D, 4 }, /* STRA */
};

/* 各 stub: mov eax, offset g_pvf_key[idx]; ret(调用方按 cdecl 平衡栈) */
static void* pvf_stub_body(void) { return (void*)&g_pvf_key[3]; }
static void* pvf_stub_head(void) { return (void*)&g_pvf_key[0]; }
static void* pvf_stub_hash(void) { return (void*)&g_pvf_key[1]; }
static void* pvf_stub_grpi(void) { return (void*)&g_pvf_key[2]; }
static void* pvf_stub_strb(void) { return (void*)&g_pvf_key[5]; }
static void* pvf_stub_stra(void) { return (void*)&g_pvf_key[4]; }

static const void* PVF_STUB_BY_IDX[6] = {
    pvf_stub_head, pvf_stub_hash, pvf_stub_grpi,
    pvf_stub_body, pvf_stub_stra, pvf_stub_strb,
};

static int pvf_hex_val(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

/* 从环境变量读取 96hex key_set 并填入 g_pvf_key；成功返回 1 */
static int pvf_load_keyset(void)
{
    char hex[97];
    DWORD n = GetEnvironmentVariableA("DNF_PVF_KEYSET", hex, sizeof(hex));
    if (n == 0 || n >= sizeof(hex)) return 0;
    for (int i = 0; i < 6; i++) {
        for (int j = 0; j < 8; j++) {
            int hi = pvf_hex_val(hex[i * 16 + j * 2]);
            int lo = pvf_hex_val(hex[i * 16 + j * 2 + 1]);
            if (hi < 0 || lo < 0) return 0;
            g_pvf_key[i][j] = (unsigned char)((hi << 4) | lo);
        }
    }
    return 1;
}

/* 校验全部补丁点后再写；任一不符即放弃（保持未安装状态） */
static void pvf_install(void)
{
    unsigned base = (unsigned)GetModuleHandle(NULL);
    if (!base) return;

    int n = (int)(sizeof(PVF_PATCH_SITES) / sizeof(PVF_PATCH_SITES[0]));
    /* 第一遍: 只读校验 */
    for (int i = 0; i < n; i++) {
        unsigned call_va = base + (PVF_PATCH_SITES[i].va - PVF_IMG_BASE);
        if (*(volatile unsigned char*)call_va != 0xE8) return;
        int disp = *(volatile int*)(call_va + 1);
        unsigned orig = call_va + 5 + (unsigned)disp;
        if (orig != base + PVF_GETKEY_RVA) return;
    }
    /* 第二遍: 写 rel32 */
    for (int i = 0; i < n; i++) {
        unsigned call_va = base + (PVF_PATCH_SITES[i].va - PVF_IMG_BASE);
        unsigned stub = (unsigned)PVF_STUB_BY_IDX[PVF_PATCH_SITES[i].key_idx];
        int new_disp = (int)(stub - (call_va + 5));
        DWORD old;
        if (VirtualProtect((LPVOID)(call_va + 1), 4, PAGE_EXECUTE_READWRITE, &old)) {
            *(volatile int*)(call_va + 1) = new_disp;
            VirtualProtect((LPVOID)(call_va + 1), 4, old, &old);
        }
    }
    g_pvf_installed = 1;
}

/* attach 时调用: 无 keyset / 非法 keyset / 版本不匹配均静默跳过 */
static void pvf_install_if_requested(void)
{
    if (g_pvf_installed) return;
    if (!pvf_load_keyset()) return;
    pvf_install();
}

BOOL WINAPI DllMain(HINSTANCE h, DWORD reason, LPVOID reserved)
{
    (void)reserved;
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(h);
        /* 登录器启动副账号时注入 DNF_MULTI_CLIENT=1 环境变量，据此识别副进程并静默 */
        char env[80] = { 0 };
        GetEnvironmentVariableA("DNF_MULTI_CLIENT", env, sizeof(env));
        g_secondary = (env[0] == '1' && env[1] == '\0');
        /* PVF 密钥轮换补丁：需在游戏读取 Script.pvf 前装好；keyset 无效时静默跳过 */
        pvf_install_if_requested();
        /* 读取本进程账号（DNF_ACCOUNT_ID）供私有到账提示按账号过滤；
           环境值超长（>=128B 被截断）时禁用私有提示，防止截断串号 */
        GetEnvironmentVariableA("DNF_ACCOUNT_ID", g_account, sizeof(g_account));
        if (g_account[sizeof(g_account) - 1] != '\0') {
            g_account[0] = '\0';
        }
        CreateThread(NULL, 0, worker_thread, NULL, 0, NULL);
    } else if (reason == DLL_PROCESS_DETACH) {
        g_running = 0;
        release_shared();
    }
    return TRUE;
}
