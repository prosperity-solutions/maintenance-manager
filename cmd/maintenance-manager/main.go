// Command maintenance-manager serves maintenance pages, interstitials and
// banners for apps behind Envoy Gateway. See docs/HANDOVER.md.
package main

import (
	"fmt"
	"os"
)

// version is set at build time (-ldflags "-X main.version=vX.Y.Z", see .ko.yaml).
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: maintenance-manager <version>")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "maintenance-manager: unknown command %q. Run: maintenance-manager version\n", args[0])
		return 2
	}
}
