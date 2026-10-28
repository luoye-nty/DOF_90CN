#pragma once

#include <stdint.h>

typedef uintptr_t(__thiscall* AssaultActorReset)(void*, int, int);
static AssaultActorReset g_assaultActorReset = nullptr;

inline uintptr_t __fastcall ProxyAssaultResultActorReset(void* actor, void*, int spectator, int flag)
{
    if (!actor || !g_assaultActorReset) {
        return 0;
    }
    // The state-6 call re-adds actors to the scene. Reinitializing a dead
    // actor resets its death animation/report latch while its HP is still 0.
    typedef int(__thiscall* IsDead)(void*);
    uintptr_t* vtable = *static_cast<uintptr_t**>(actor);
    if (reinterpret_cast<IsDead>(vtable[884 / sizeof(uintptr_t)])(actor)) {
        return 0;
    }
    return g_assaultActorReset(actor, spectator, flag);
}
