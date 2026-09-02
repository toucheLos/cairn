// Command scan-fixtures checks fixture content for unredacted material.
//
// Usage:
//
//	scan-fixtures [path ...]          scan paths on disk (default: fixtures/)
//	scan-fixtures -name PATH          scan content on stdin, reporting it as PATH
//
// The -name form exists for the pre-commit hook, which feeds staged blob content
// rather than the working tree: a file can be staged clean and then modified,
// and it is the staged bytes that would be committed.
//
// Exit status is 1 when anything is found, so it works as a hook and in CI
// without further plumbing.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/touchelos/cairn/redact/scan"
)

func main() {
	name := flag.String("name", "", "read content from stdin and report it under this path")
	flag.Parse()

	var findings []scan.Finding
	var scanned int

	if *name != "" {
		content, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan-fixtures: reading stdin: %v\n", err)
			os.Exit(2)
		}
		// The same test the directory walk applies below.
		//
		// Without it the two entry points disagree about what fixture data is,
		// and they are meant to be the same check seen from two directions:
		// `make scan-fixtures` walks the tree, the pre-commit hook pipes one
		// staged blob. The disagreement is not theoretical — it fired on
		// fixtures/README.md, whose documented example of a .redaction-ok entry
		// contains the very string that entry exists to permit.
		if !scan.IsFixtureData(*name) {
			return
		}
		sup, err := loadSuppressions(filepath.Dir(*name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan-fixtures: %v\n", err)
			os.Exit(2)
		}
		scanned = 1
		findings = scan.ScanWith(*name, content, sup)
	} else {
		paths := flag.Args()
		if len(paths) == 0 {
			paths = []string{"fixtures"}
		}
		files, err := collect(paths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan-fixtures: %v\n", err)
			os.Exit(2)
		}
		for _, f := range files {
			content, err := os.ReadFile(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "scan-fixtures: reading %s: %v\n", f, err)
				os.Exit(2)
			}
			sup, err := loadSuppressions(filepath.Dir(f))
			if err != nil {
				fmt.Fprintf(os.Stderr, "scan-fixtures: %v\n", err)
				os.Exit(2)
			}
			scanned++
			findings = append(findings, scan.ScanWith(f, content, sup)...)
		}
	}

	if len(findings) == 0 {
		fmt.Fprintf(os.Stderr, "scan-fixtures: %d file(s) scanned, nothing found\n", scanned)
		return
	}

	for _, f := range findings {
		fmt.Println(f)
	}
	fmt.Fprintf(os.Stderr, "\nscan-fixtures: %d finding(s) across %d file(s) scanned.\n", len(findings), scanned)
	fmt.Fprintf(os.Stderr,
		"Redact these by hand before committing (CLAUDE.md §3).\n"+
			"If a match is genuinely safe, add a %q comment on that line explaining why.\n", scan.Annotation)
	os.Exit(1)
}

// collect walks the given paths and returns the files to scan, sorted.
//
// Binary files are skipped rather than scanned: the rules are line-oriented, and
// a binary blob produces noise instead of findings. A fixture should not contain
// binaries in the first place, so this is reported rather than silently ignored.
func collect(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if !scan.IsFixtureData(path) {
				return nil
			}
			if isBinary(path) {
				fmt.Fprintf(os.Stderr, "scan-fixtures: skipping apparently binary file %s\n", path)
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadSuppressions reads the sidecar for dir, if present. Suppressions apply to
// the directory they sit in and are not inherited: a blanket exception applied
// to a whole tree is how a scanner stops finding anything.
func loadSuppressions(dir string) (scan.Suppressions, error) {
	path := filepath.Join(dir, scan.SuppressionFile)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return scan.ParseSuppressions(path, content)
}

func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8000)
	n, _ := f.Read(buf)
	return strings.IndexByte(string(buf[:n]), 0) >= 0
}
