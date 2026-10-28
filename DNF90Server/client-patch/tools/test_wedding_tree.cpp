#include "../wedding_tree.h"
#include <assert.h>

int RemainingSeconds(int expires, int now)
{
    int result, clockAfter, stackBefore, stackAfter;
    __asm {
        push edi
        push ebx
        mov eax, now
        mov ebx, expires
        mov stackBefore, esp
        push eax
        push ebx
        call WeddingTreeRemainingSeconds
        mov result, edi
        mov clockAfter, eax
        mov stackAfter, esp
        pop ebx
        pop edi
    }
    assert(stackBefore == stackAfter && clockAfter == now);
    return result;
}

int main()
{
    const int now = 1788869652;
    assert(RemainingSeconds(now + 30 * 86400, now) == 30 * 86400);
    assert(RemainingSeconds(now + 30 * 86400, now + 86400) == 29 * 86400);
    assert(RemainingSeconds(now + 1, now) == 1);
    assert(RemainingSeconds(now, now) == 0);
    assert(RemainingSeconds(now - 1, now) == 0);
    return 0;
}
