package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jayyao97/zotigo/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected status, check, channel, prepare, apply or worker")
	}
	fs := flag.NewFlagSet("zotigo-update", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "Installer prefix")
	component := fs.String("component", "", "desktop, daemon or web")
	channel := fs.String("channel", "", "stable or development")
	action := fs.String("action", "", "Worker action")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "recover" {
		if *component != "daemon" && *component != "web" {
			return fmt.Errorf("recovery is reserved for service startup")
		}
		return (&update.Manager{Prefix: *prefix, Component: *component}).Recover(true)
	}
	m, err := update.New(*prefix, *component)
	if err != nil {
		return err
	}
	var status update.Status
	switch args[0] {
	case "status":
		status, err = m.Status()
	case "check":
		status, err = m.Check(context.Background())
	case "channel":
		status, err = m.SetChannel(*channel)
	case "prepare", "apply":
		status, err = m.Start(args[0])
	case "cancel":
		status, err = m.CancelWaiting()
	case "worker":
		return m.Worker(*action)
	default:
		return fmt.Errorf("unknown update action")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
