#pragma once

// sub_20C7A00 keeps the expiry in EBX and the server clock in EAX.
// The replacement call consumes two stack arguments and returns seconds in EDI.
static __declspec(naked) void WeddingTreeRemainingSeconds()
{
    __asm {
        mov edi, [esp + 4]
        sub edi, [esp + 8]
        jge remaining
        xor edi, edi
    remaining:
        ret 8
    }
}
