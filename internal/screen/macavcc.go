package screen

import "encoding/binary"

// VideoToolbox speaks AVCC (NAL units with a big-endian length before
// each); the wire and the other OSes speak Annex B (start codes). These
// convert between the two for h264_darwin.go.

// annexBNALs splits an Annex B access unit into its NAL units (without
// start codes).
func annexBNALs(au []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(au); {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, trimZeros(au[start:i]))
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(au) {
		nals = append(nals, au[start:])
	}
	var out [][]byte
	for _, n := range nals {
		if len(n) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// trimZeros drops the zero bytes before the next start code (the first
// byte of a 4-byte start code, or trailing_zero_8bits).
func trimZeros(n []byte) []byte {
	for len(n) > 0 && n[len(n)-1] == 0 {
		n = n[:len(n)-1]
	}
	return n
}

// appendAVCC appends nals with 4-byte lengths.
func appendAVCC(dst []byte, nals ...[]byte) []byte {
	for _, n := range nals {
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(n)))
		dst = append(dst, n...)
	}
	return dst
}

// appendAnnexB appends AVCC data (lengths of lenSize bytes) as Annex B;
// false when the lengths do not add up.
func appendAnnexB(dst, avcc []byte, lenSize int) ([]byte, bool) {
	if lenSize < 1 || lenSize > 4 {
		return dst, false
	}
	for len(avcc) > 0 {
		if len(avcc) < lenSize {
			return dst, false
		}
		n := 0
		for _, b := range avcc[:lenSize] {
			n = n<<8 | int(b)
		}
		avcc = avcc[lenSize:]
		if n > len(avcc) {
			return dst, false
		}
		dst = append(dst, 0, 0, 0, 1)
		dst = append(dst, avcc[:n]...)
		avcc = avcc[n:]
	}
	return dst, true
}
