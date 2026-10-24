// Command maintenance-manager serves maintenance pages, interstitials and
// banners for apps behind Envoy Gateway. See docs/HANDOVER.md.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
)

// version is set at build time (-ldflags "-X main.version=X.Y.Z", see .ko.yaml).
var version = "dev"

const usage = `usage: maintenance-manager <command>

commands:
  serve     run the authz server, the controller and the editor
  cleanup   remove every policy and ReferenceGrant this install created (the uninstall hook runs it)
  version   print the version`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], log)
	case "cleanup":
		return cleanup(log)
	case "version":
		fmt.Println(version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "maintenance-manager: unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
}

// logrFromSlog lets controller-runtime log through the same JSON handler.
func logrFromSlog(l *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(l.Handler())
}
