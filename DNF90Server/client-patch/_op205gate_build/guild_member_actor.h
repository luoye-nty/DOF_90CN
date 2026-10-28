#pragma once

#include <stdint.h>

typedef void* (__thiscall* GuildMemberActorLookupFn)(void*, int, const wchar_t*);
static GuildMemberActorLookupFn g_guildMemberActorLookup = nullptr;
static int g_guildMemberLocalRegion = 1;

// sub_1D21FD0 binds each member+60 through sub_2699B10(region, name).
// sub_26A6F80 repeats this during the mandatory end-of-list/native refresh.
// The local scene actor uses region 0, while social commands require 1..249.
// Retain native matches; recover only the selected local actor by native name
// lookup. Other regions and other actors keep the native null result.
inline void* __fastcall ProxyGuildMemberActorLookup(
    void* manager, void*, int region, const wchar_t* name)
{
    void* found = g_guildMemberActorLookup(manager, region, name);
    if (found || !manager || !name || region != g_guildMemberLocalRegion) {
        return found;
    }
    void* current = *reinterpret_cast<void**>(
        static_cast<unsigned char*>(manager) + 0x70);
    if (!current || *reinterpret_cast<const int32_t*>(
            static_cast<const unsigned char*>(current) + 0x38C) != 0) {
        return nullptr;
    }
    void* local = g_guildMemberActorLookup(manager, 0, name);
    return local == current ? local : nullptr;
}
