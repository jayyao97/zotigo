package main

import (
	"os"

	"github.com/jayyao97/zotigo/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
