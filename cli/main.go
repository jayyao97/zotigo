package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run(os.Args[1:]))
}
