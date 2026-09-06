// Command zotigod runs the local Zotigo daemon.
package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/diagnostics"

	"github.com/jayyao97/zotigo/internal/zotigod"
)

func main() {
	finish := diagnostics.MonitorCrashes("daemon")
	code := zotigod.Run(os.Args[1:])
	finish()
	os.Exit(code)
}
