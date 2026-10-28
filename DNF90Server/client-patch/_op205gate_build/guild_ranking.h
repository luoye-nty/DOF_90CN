#pragma once
#include <stdint.h>
#include <stddef.h>
#include <string.h>

struct GuildRankSnapshot {
    uint16_t character;
    uint32_t rank;
    uint32_t experience;
};

inline bool DecodeGuildRankBody(const unsigned char* body, size_t length,
                                GuildRankSnapshot* output)
{
    if (!body || !output || length != 12 || body[0] != 4 || body[1] != 1)
        return false;
    GuildRankSnapshot value = {};
    memcpy(&value.character, body + 2, 2);
    memcpy(&value.rank, body + 4, 4);
    memcpy(&value.experience, body + 8, 4);
    if (!value.character) return false;
    *output = value;
    return true;
}

static void* g_originalGuildRankFrameGetter = nullptr;

inline const wchar_t* GuildWarErrorText(uint16_t opcode, unsigned char code)
{
    if (opcode != 841 && opcode != 842 && opcode != 1345 && opcode != 1346 && opcode != 1364)
        return nullptr;
    switch (code) {
    case 240: return L"召唤并设置守护神后，可以参加公会战。";
    case 241: return L"战争正在进行中，请先完成当前战争。";
    case 242: return L"今日参与次数已用完，或该竞争公会已挑战。";
    case 243: return L"斗争的精气不足。";
    case 244: return L"你没有公会基地战争权限。";
    case 245: return L"请先加入公会后再参与公会战。";
    case 250: return L"任务或挑战信息已变化，请重新打开公会战窗口。";
    case 251: return L"任务尚未完成，达到任务要求后可领取奖励。";
    case 252: return L"背包空间不足，请整理背包后再领取奖励。";
    case 253: return L"公会战数据暂未读取成功，请稍后重试。";
    case 254: return L"暂无可添加的竞争公会。请让其他公会召唤并设置守护神后再试；本次未扣除斗争的精气。";
    case 255: return L"当前处于休战时间，或本次战争已经结束。";
    default: return nullptr;
    }
}

typedef void* (__thiscall* GuildWarNoticeOpenFn)(void*, uintptr_t, int, unsigned int);

inline bool OpenGuildWarErrorNotice(const unsigned char* packet, size_t length,
                                   void* scene, GuildWarNoticeOpenFn open)
{
    if (!packet || length != 18 || packet[0] != 1 || packet[16] != 0 || !scene || !open)
        return false;
    uint32_t declaredLength = 0;
    uint16_t opcode = 0;
    memcpy(&declaredLength, packet + 3, sizeof(declaredLength));
    memcpy(&opcode, packet + 1, sizeof(opcode));
    if (declaredLength != length) return false;
    const wchar_t* message = GuildWarErrorText(opcode, packet[17]);
    if (!message) return false;
    // E83CA0..E83CBB opens the native announcement owner 0x298 with
    // (text, group=0). 2FE12E0 is gated by a function returning false in
    // this EXE and therefore discards these messages.
    open(scene, 0x298, static_cast<int>(reinterpret_cast<uintptr_t>(message)), 0);
    return true;
}
static void (__stdcall* g_guildRankFrameUpdate)(void*) = nullptr;

// Call-site FB5492 has the live GuildMain2017 owner in ESI and the original
// getter's this pointer in ECX. Preserve GP/flags/FPU/SSE state, then tail-call
// that getter so its return value and native caller stack remain unchanged.
__declspec(naked) static void ProxyGuildRankFrame()
{
    __asm {
        pushfd
        pushad
        mov ebp, esp
        sub esp, 528
        and esp, -16
        fxsave [esp]
        push esi
        call dword ptr [g_guildRankFrameUpdate]
        fxrstor [esp]
        mov esp, ebp
        popad
        popfd
        jmp dword ptr [g_originalGuildRankFrameGetter]
    }
}
