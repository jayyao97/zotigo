package debug

import (
	"log"
	"os"
	"strings"
)

// Enabled reports whether verbose debug logging is enabled.
func Enabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("ZOTIGO_DEBUG")))
	return v != "" && v != "0" && v != "false" && v != "no"
}

// Logf writes a debug log line when ZOTIGO_DEBUG is enabled.
func Logf(format string, args ...any) {
	if !Enabled() {
		return
	}
	log.Printf("[zotigo-debug] "+format, args...)
}
