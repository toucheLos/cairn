// Command new-fixture scaffolds an incident fixture.
//
// Adding a real incident to the corpus should be mechanical, because anything
// fiddly gets skipped under time pressure and the corpus is the moat
// (CLAUDE.md §9). This writes the directory layout, a meta.yaml template with
// every field present and explained, and the redaction checklist inline.
//
//	go run ./tools/new-fixture -title "IB link flap killed an MPI job" -slug ib-link-flap
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/touchelos/cairn/schema"
)

func main() {
	var (
		root      = flag.String("root", "fixtures", "corpus root")
		slug      = flag.String("slug", "", "short kebab-case name, e.g. ib-link-flap (required)")
		title     = flag.String("title", "", "one-line description (required)")
		synthetic = flag.Bool("synthetic", false, "mark as authored rather than observed")
		listClass = flag.Bool("classes", false, "list the class enum with its registered detail keys, then exit")
	)
	flag.Parse()

	if *listClass {
		for _, c := range schema.AllClasses() {
			fmt.Printf("%s\n", c)
			for _, k := range schema.RegisteredAttrs(c) {
				pii := ""
				if schema.AttrIsPII(c, k) {
					pii = "  (PII — redacted at the boundary)"
				}
				fmt.Printf("    %s%s\n", k, pii)
			}
		}
		return
	}

	if *slug == "" || *title == "" {
		fmt.Fprintln(os.Stderr, "new-fixture: -slug and -title are both required")
		flag.Usage()
		os.Exit(2)
	}
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(*slug) {
		fmt.Fprintf(os.Stderr, "new-fixture: slug %q must be lower-case kebab-case\n", *slug)
		os.Exit(2)
	}

	id := fmt.Sprintf("%03d-%s", nextIndex(*root), *slug)
	dir := filepath.Join(*root, id)
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(os.Stderr, "new-fixture: %s already exists\n", dir)
		os.Exit(1)
	}
	for _, sub := range []string{"input", "expected"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "new-fixture: %v\n", err)
			os.Exit(1)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "meta.yaml"), []byte(metaTemplate(id, *title, *synthetic)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "new-fixture: %v\n", err)
		os.Exit(1)
	}
	// An empty array is canonical, so the fixture is loadable from the start and
	// the harness can be run against it before any events are written.
	if err := os.WriteFile(filepath.Join(dir, "expected", "events.json"), []byte("[\n]\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "new-fixture: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created %s\n\n%s\n", dir, checklist(dir))
}

func nextIndex(root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 1
	}
	max := 0
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if len(n) >= 3 {
			if v, err := strconv.Atoi(n[:3]); err == nil && v > max {
				max = v
			}
		}
	}
	return max + 1
}

func metaTemplate(id, title string, synthetic bool) string {
	var classes []string
	for _, c := range schema.AllClasses() {
		classes = append(classes, "#   - "+string(c))
	}
	provenance := `redacted_by: ""        # REQUIRED: who performed the redaction
redaction_method: ""   # REQUIRED: how, e.g. "hand edit against the checklist"`
	if synthetic {
		provenance = `redacted_by: ""        # not required for a synthetic fixture
redaction_method: ""   # not required for a synthetic fixture`
	}

	return fmt.Sprintf(`id: %s
title: %s

# synthetic: true means this fixture was authored rather than observed.
#
# This flag is load-bearing. Synthetic fixtures exercise the harness and serve as
# templates; they are excluded from every accuracy measurement, because measuring
# a classifier against incidents written to suit it measures nothing. Set it
# honestly.
synthetic: %v

# Every class the expected event stream produces. Checked against the stream, so
# the two cannot drift apart.
#
# Valid members (run "go run ./tools/new-fixture -classes" for their detail keys):
%s
expected_classes: []

# What actually went wrong, in prose. This is the eval target — the classes are
# intermediate evidence, and a run that produces every right class and the wrong
# conclusion has still failed.
#
# Say what would distinguish this cause from its neighbours. If the evidence does
# not settle the question, say that too: a fixture whose honest answer is "the
# node stopped responding and this capture cannot say why" is more useful than one
# that pretends to certainty, because it is what catches a classifier overclaiming.
expected_root_cause: >-
  TODO

# Producers whose output is in input/: bmc, fabric, gpu, journal, slurm, storage.
producers: []

# The access level this was captured at.
#
# unprivileged — as an ordinary user on a login node or inside a job. This is the
#                level cairn must work at (invariant §2.2), so the corpus needs to
#                be mostly these.
# root         — elevated. Richer data, but not the deployment that matters.
capability: unprivileged

scheduler:
  name: slurm
  version: ""

%s

# Anything a reader needs in order to trust or interpret this fixture: what the
# capture is missing, which parts were reconstructed, what a classifier is likely
# to get wrong here.
notes: >-
  TODO
`, id, title, synthetic, strings.Join(classes, "\n"), provenance)
}

func checklist(dir string) string {
	return `Next steps
──────────

 1. Drop the raw producer output into ` + dir + `/input/.
    Keep it byte-realistic — Phase 1 collectors have to parse these files, so do
    not add explanatory comments to them. Reviewed scanner exceptions go in
    input/.redaction-ok instead.

 2. Redact by hand. The scanner is a backstop, not the process (CLAUDE.md §3).
    Work through all of it:

      hostnames        -> node-0001, node-0002, ...   consistently, same host
                          same pseudonym everywhere it appears
      cluster names    -> cluster-a
      user names       -> user-01
      uids / gids      -> 90001, 90002, ...  (the 90000+ band reads as a
                          placeholder and still parses as an integer)
      account codes    -> acct-01
      IP addresses     -> 192.0.2.x / 198.51.100.x / 203.0.113.x (RFC 5737)
      IB GUIDs         -> 0x0000000000000001, 0x0000000000000002, ...
                          reuse the same placeholder everywhere the original
                          appeared, so the fabric topology survives
      domains          -> remove entirely; a bare pseudonym host is enough
      home paths       -> /home/user-01/...
      MAC addresses    -> remove
      key material     -> remove. Never redact a key; delete it.

    Job ids, timestamps, error strings, counters, versions, and device names are
    the evidence. Keep them. Redacting those produces a fixture that tests
    nothing.

 3. Run the scanner:

      make scan-fixtures

    A finding is either something to redact or something to record in
    input/.redaction-ok with a reason. Do not silence a rule.

 4. Fill in meta.yaml. Every TODO must go.

 5. Write expected/events.json — the exact event stream a correct collector run
    must produce. It must be in canonical form; the loader checks.

 6. Run the corpus tests:

      make check

 7. If this incident needs a class the enum does not have, stop. That is a schema
    version bump and a deliberate decision (CLAUDE.md §4): add the member, record
    why in schema/CHANGELOG.md, and regenerate the golden files. Do not reach for
    "unknown" to avoid the conversation — but do use it, deliberately, when the
    honest answer is that cairn has no class for what was observed.
`
}
