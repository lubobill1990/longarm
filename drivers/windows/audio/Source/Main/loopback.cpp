/*++

Longarm: the speaker-to-microphone loopback (loopback.h).

Both streams move their positions on the same clock at the same byte rate
(they run in one format), so the ring only absorbs the jitter of when each
one's position is updated: the reader waits for a cushion before it starts,
and skips ahead when it falls far behind (latency stays low).

--*/

#include "loopback.h"

#pragma code_seg()

// Two seconds of 48 kHz 16-bit stereo, in non-paged memory (a driver's data).
#define LOOPBACK_RING_SIZE (48000 * 4 * 2)

static UCHAR      g_Ring[LOOPBACK_RING_SIZE];
static KSPIN_LOCK g_Lock;
static ULONGLONG  g_Written;   // bytes ever written
static ULONGLONG  g_Read;      // bytes ever read (<= g_Written)
static BOOLEAN    g_HasFormat;
static BOOLEAN    g_Primed;    // the cushion was there: reading
static WAVEFORMATEX g_Format;  // of what is in the ring

static BOOLEAN SameFormat(const WAVEFORMATEX* a, const WAVEFORMATEX* b)
{
    return a->nSamplesPerSec == b->nSamplesPerSec &&
           a->nChannels == b->nChannels &&
           a->wBitsPerSample == b->wBitsPerSample &&
           a->nBlockAlign == b->nBlockAlign;
}

VOID LoopbackInitialize()
{
    KeInitializeSpinLock(&g_Lock);
    g_Written = g_Read = 0;
    g_HasFormat = FALSE;
    g_Primed = FALSE;
}

VOID LoopbackWrite(const BYTE* Data, ULONG Size, const WAVEFORMATEX* Format)
{
    if (Data == NULL || Format == NULL || Size == 0)
    {
        return;
    }
    KIRQL irql;
    KeAcquireSpinLock(&g_Lock, &irql);
    if (!g_HasFormat || !SameFormat(&g_Format, Format))
    {
        // Another format: what is in the ring is of no use.
        g_Format = *Format;
        g_Format.cbSize = 0;
        g_HasFormat = TRUE;
        g_Written = g_Read = 0;
        g_Primed = FALSE;
    }
    // More than the ring holds: only its last part matters.
    if (Size > LOOPBACK_RING_SIZE)
    {
        g_Written += Size - LOOPBACK_RING_SIZE;
        Data += Size - LOOPBACK_RING_SIZE;
        Size = LOOPBACK_RING_SIZE;
    }
    while (Size > 0)
    {
        ULONG at = (ULONG)(g_Written % LOOPBACK_RING_SIZE);
        ULONG run = min(Size, LOOPBACK_RING_SIZE - at);
        RtlCopyMemory(g_Ring + at, Data, run);
        g_Written += run;
        Data += run;
        Size -= run;
    }
    // The reader fell more than the ring behind: it lost that part.
    if (g_Written - g_Read > LOOPBACK_RING_SIZE)
    {
        g_Read = g_Written - LOOPBACK_RING_SIZE;
    }
    KeReleaseSpinLock(&g_Lock, irql);
}

VOID LoopbackRead(BYTE* Data, ULONG Size, const WAVEFORMATEX* Format)
{
    if (Data == NULL || Size == 0)
    {
        return;
    }
    ULONG done = 0;
    KIRQL irql;
    KeAcquireSpinLock(&g_Lock, &irql);
    if (Format != NULL && g_HasFormat && SameFormat(&g_Format, Format) && g_Format.nAvgBytesPerSec > 0)
    {
        ULONG align = max(g_Format.nBlockAlign, 1);
        ULONG cushion = g_Format.nAvgBytesPerSec / 50;  // 20 ms
        ULONG most = g_Format.nAvgBytesPerSec / 5;      // 200 ms: further behind, skip ahead
        cushion -= cushion % align;
        ULONGLONG avail = g_Written - g_Read;
        if (avail > most + Size)
        {
            g_Read = g_Written - cushion - Size;
            g_Read -= g_Read % align;
            avail = g_Written - g_Read;
        }
        if (!g_Primed && avail >= cushion + Size)
        {
            g_Primed = TRUE;
        }
        if (g_Primed)
        {
            ULONG want = (ULONG)min((ULONGLONG)Size, avail);
            want -= want % align;
            while (done < want)
            {
                ULONG at = (ULONG)(g_Read % LOOPBACK_RING_SIZE);
                ULONG run = min(want - done, LOOPBACK_RING_SIZE - at);
                RtlCopyMemory(Data + done, g_Ring + at, run);
                g_Read += run;
                done += run;
            }
            if (done < Size)
            {
                g_Primed = FALSE;  // ran dry: wait for the cushion again
            }
        }
    }
    KeReleaseSpinLock(&g_Lock, irql);
    if (done < Size)
    {
        RtlZeroMemory(Data + done, Size - done);
    }
}
