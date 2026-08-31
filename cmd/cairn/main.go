// Command cairn answers "why did this job die" in one command.
//
// Invariant §2.5: one static binary, no daemon, no server, no database. Built
// with the standard library only, so it cross-compiles to an old login node
// without a toolchain argument.
//
//	cairn context --job <id>   evidence for one job, ready to paste into an LLM
//	cairn doctor               what each collector can and cannot see, and why
//	cairn miss --job <id>      record a case cairn got wrong
//
// `diff` and `init` are Phase 3.
package main

import (
	"fmt"
	"os"
)

const usage = `cairn — why did this job die

usage: cairn <command> [flags]

commands:
  context   evidence for one job, deterministically ordered and token-budgeted
  doctor    what each collector can and cannot see, and why
  miss      record a case cairn got wrong, to drive what gets built next
  version   schema version and build information

Run "cairn <command> -h" for the flags of a command.

cairn is read-only. It runs unprivileged, stores no logs, and works with
inference switched off — it produces the evidence, it does not call a model.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "context":
		err = runContext(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "miss":
		err = runMiss(os.Args[2:])
	case "version":
		err = runVersion(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "cairn: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "cairn: %v\n", err)
		os.Exit(1)
	}
}
