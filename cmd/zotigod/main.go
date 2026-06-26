// Command zotigod runs the local Zotigo daemon.
package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/zotigod"
)

func main() {
	os.Exit(zotigod.Run(os.Args[1:]))
}
