// Command mycel is the single Mycelium binary: hub, node daemon and CLI are
// subcommands of one executable.
package main

import (
	"os"

	"github.com/richard-sen27/mycelium/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
