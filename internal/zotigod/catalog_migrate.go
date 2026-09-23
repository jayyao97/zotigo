package zotigod

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/jayyao97/zotigo/core/workspace"
)

func runCatalogMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("zotigod catalog-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "Existing catalog root (required); stop all daemons using it first")
	target := fs.Uint("to", 0, "Target schema version (required); downgrading discards removed fields")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *target == 0 || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: zotigod catalog-migrate --root <catalog-root> --to <version>")
		return 2
	}
	if err := workspace.MigrateCatalog(context.Background(), *root, *target); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Catalog migrated to schema %d.\n", *target)
	return 0
}
