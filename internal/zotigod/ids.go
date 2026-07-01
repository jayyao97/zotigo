package zotigod

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

func newZotigodID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return prefix + "_" + hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
