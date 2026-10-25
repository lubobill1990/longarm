package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"
)

// stunAnswers sends a STUN binding request to addr (UDP) and reports
// whether a binding success response with the same transaction id arrives.
func stunAnswers(addr string) bool {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return false
	}
	defer c.Close()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001) // binding request, no attributes
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)
	rand.Read(req[8:20])
	buf := make([]byte, 1500)
	for range 3 {
		c.Write(req)
		c.SetReadDeadline(time.Now().Add(time.Second))
		n, err := c.Read(buf)
		if err == nil && n >= 20 && binary.BigEndian.Uint16(buf[0:]) == 0x0101 && bytes.Equal(buf[8:20], req[8:20]) {
			return true
		}
	}
	return false
}
