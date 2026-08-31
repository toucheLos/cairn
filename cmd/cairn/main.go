package main

import (
	"fmt"
	"os"

	"github.com/touchelos/cairn/schema"
)

// Phase 0 ships nothing runnable. This exists so that `go build ./...` covers
// the command package and so that anyone who builds and runs the binary is told
// plainly where the project is, rather than being met with a stub that looks
// like it might work.
func main() {
	fmt.Fprintf(os.Stderr, `cairn: Phase 0 (foundations). Nothing is shipped.

There is no working command yet. This repository currently contains the frozen
event schema (version %d, %d classes), the fixture corpus format, the redaction
scanner, and the test harness.

  cairn context --job <id>   Phase 2
  cairn doctor               Phase 2
  cairn diff <node>          Phase 3
  cairn init                 Phase 3

See CLAUDE.md for the roadmap and README.md for how to work in this repository.
`, schema.Version, len(schema.AllClasses()))
	os.Exit(1)
}
