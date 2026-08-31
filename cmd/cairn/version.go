package main

import (
	"flag"
	"fmt"
	"runtime/debug"

	"github.com/touchelos/cairn/schema"
)

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: cairn version\n\nPrint the schema version and build information.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf("cairn (unreleased)\n")
	// The schema version is the number that matters to anyone holding a stored
	// bundle, so it is reported first-class rather than buried in a build stamp.
	fmt.Printf("schema version %d, %d classes, %d producers\n",
		schema.Version, len(schema.AllClasses()), len(schema.AllSources()))

	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = " (modified)"
				}
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			fmt.Printf("built from %s%s with %s\n", rev, dirty, bi.GoVersion)
		} else {
			fmt.Printf("built with %s\n", bi.GoVersion)
		}
	}
	return nil
}
