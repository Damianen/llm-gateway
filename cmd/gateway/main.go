// Command gateway runs the llm-gateway server.
package main

import (
	"fmt"
	"os"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "llm-gateway %s: server wiring lands in Phase 1+\n", version)
	os.Exit(1)
}
