/*++

Longarm: what is played into the virtual speaker comes out of the virtual
microphone (Longarm's remote desktop plays the viewer's microphone into the
speaker; programs on the worker record the microphone).

One ring of the speaker's last bytes for the whole driver. The speaker
stream writes what it renders, the microphone stream reads it with a small
cushion against jitter, and gives silence when there is nothing (nothing
plays, or the two streams have different formats - both endpoints default
to 48 kHz 16-bit stereo, the INF's OEM format).

--*/

#pragma once

#include "definitions.h"  // the driver's kernel and audio headers, in their order

// LoopbackInitialize: once, in DriverEntry.
VOID LoopbackInitialize();

// LoopbackWrite: the speaker rendered Size bytes at Data in Format.
VOID LoopbackWrite(_In_reads_bytes_(Size) const BYTE* Data, _In_ ULONG Size, _In_ const WAVEFORMATEX* Format);

// LoopbackRead: the microphone needs Size bytes in Format at Data.
VOID LoopbackRead(_Out_writes_bytes_(Size) BYTE* Data, _In_ ULONG Size, _In_ const WAVEFORMATEX* Format);
