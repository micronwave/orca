package idgen

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var fallbackCounter atomic.Uint64

// New returns IDs in the form "<prefix>-<32hexchars>" (128 bits of randomness).
func New(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "ID"
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := uint64(time.Now().UnixNano()) + fallbackCounter.Add(1)
		return fmt.Sprintf("%s-%016x%016x", p, n, fallbackCounter.Add(1))
	}
	return fmt.Sprintf("%s-%x", p, b[:])
}
