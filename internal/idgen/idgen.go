package idgen

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var fallbackCounter atomic.Uint64

// New returns IDs in the form "<prefix>-<first8charsOfUUID>".
func New(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "ID"
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := uint64(time.Now().UnixNano()) + fallbackCounter.Add(1)
		return fmt.Sprintf("%s-%08x", p, uint32(n))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%08x", p, binary.BigEndian.Uint32(b[0:4]))
}
