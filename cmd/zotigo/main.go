package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/diagnostics"

	"github.com/jayyao97/zotigo/internal/cliapp"
)

func main() {
	finish := diagnostics.MonitorCrashes("cli")
	code := cliapp.Run(os.Args[1:])
	finish()
	os.Exit(code)
}
