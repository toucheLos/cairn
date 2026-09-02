// Package corpuspath locates the private corpus.
//
// It is a separate package for one reason: the shipped binary links no
// third-party code (scripts/verify-guards.sh §8), and importing `fixtures` for
// two constants would pull gopkg.in/yaml.v3 — a dependency of the fixture
// loader — straight into cmd/cairn. The guard caught exactly that.
//
// So the path convention lives here, with no dependencies, and both the loader
// and `cairn capture` use it. One definition, no drift.
package corpuspath

import (
	"fmt"
	"os"
	"path/filepath"
)

// Env names the environment variable pointing at the private corpus.
const Env = "CAIRN_CORPUS"

// Default is the in-tree directory the private corpus lives in.
//
// It is gitignored, scripts/pre-commit refuses to stage anything under it, and
// scripts/check-boundary.sh asserts in CI that it was never committed. See
// CLAUDE.md §3 for why observed incidents never reach this repository.
const Default = "corpus"

// Find locates the private corpus, returning "" when there is none.
//
// Finding nothing is the normal case and never an error. CI has no private
// corpus and must not have one: the public test run is synthetic-only by
// construction, which is what stops an observed fixture ever being needed to
// make it pass.
//
// The relative probes cover the two places a caller runs from — the repository
// root for the binary, and a package directory for `go test`.
func Find() (string, error) {
	if v := os.Getenv(Env); v != "" {
		if isDir(v) {
			return v, nil
		}
		// Named explicitly and absent, so this is a real error: the operator
		// asked for that corpus, and silently running without it would report an
		// accuracy number measured over nothing.
		return "", fmt.Errorf("%s=%s is not a directory", Env, v)
	}
	for _, p := range []string{Default, filepath.Join("..", Default)} {
		if isDir(p) {
			return p, nil
		}
	}
	return "", nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
