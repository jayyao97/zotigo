// Command acp runs zotigo as an ACP (Agent Client Protocol) server over stdio.
// Editors spawn this process and communicate via JSON-RPC 2.0 on stdin/stdout.
//
// Usage:
//
//	zotigo-acp
//
// The process reads JSON-RPC messages from stdin and writes responses to stdout.
// Stderr is used for logging.
package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/acpserver"
)

func main() {
	os.Exit(acpserver.Run(os.Args[1:]))
}
