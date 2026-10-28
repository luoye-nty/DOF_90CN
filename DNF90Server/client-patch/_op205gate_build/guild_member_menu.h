#pragma once

#include <stdint.h>

inline int __fastcall ProxyGuildMenuActorRegion(void* actor, void*)
{
    // sub_325AAB0 deliberately supplies no actor for remote member menus.
    // Native sub_26908C0 only reads +0x38C; zero is a valid member region.
    return actor ? *reinterpret_cast<const int32_t*>(static_cast<const char*>(actor) + 0x38C) : 0;
}
