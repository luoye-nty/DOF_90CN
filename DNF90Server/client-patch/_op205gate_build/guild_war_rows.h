#pragma once
#include <stdint.h>

static uintptr_t g_guildWarRowResume = 0;
static uintptr_t g_guildWarRowReturn = 0;

static void __stdcall UpdateGuildWarRowVisibility(unsigned char* window, unsigned int slot, int populated)
{
    if (!window || slot >= 8) return;
    // D1C170/D1C180 hide the view and challenge controls using virtual +8.
    for (unsigned int offset = 0x408; offset <= 0x448; offset += 0x40) {
        // D15225 restores challenge visibility from the real result. Restore
        // the view button here when scrolling back from a partial page.
        if (populated && offset == 0x448) break;
        void* control = *reinterpret_cast<void**>(window + offset + slot * 8);
        if (control) {
            typedef void (__thiscall* SetVisible)(void*, int);
            auto table = *reinterpret_cast<void***>(control);
            reinterpret_cast<SetVisible>(table[2])(control, populated);
        }
    }
}

// D1C140 draws all eight slots even on a partial last page. D14F87 compares
// row ID with the active opponent: an empty ID 0 equals the idle opponent 0.
// Skip empty rows before any drawing and retire buttons left by the last page.
// At this point the native SEH state is -1 and no local objects need cleanup.
__declspec(naked) static void ProxyGuildWarRow()
{
    __asm {
        pushfd
        pushad
        mov ebx, esp
        sub esp, 528
        and esp, -16
        fxsave [esp]
        xor eax, eax
        cmp dword ptr [ebp-74h], 0
        setne al
        push eax
        push dword ptr [ebp-84h]
        push dword ptr [ebp-7Ch]
        call UpdateGuildWarRowVisibility
        fxrstor [esp]
        mov esp, ebx
        popad
        popfd
        cmp dword ptr [ebp-74h], 0
        jne populated
        jmp dword ptr [g_guildWarRowReturn]
    populated:
        mov eax, [eax+2E41h]
        cmp [ebp-74h], eax
        jmp dword ptr [g_guildWarRowResume]
    }
}
