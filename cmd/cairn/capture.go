package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/corpuspath"
	"github.com/touchelos/cairn/redact/scan"
	"github.com/touchelos/cairn/schema"
	"github.com/touchelos/cairn/site"
)

// capture writes a fixture skeleton from a live incident.
//
// On §2.3, "no log storage": this writes raw producer output to disk, and that
// deserves a straight answer rather than a hope nobody notices. §2.3 is about
// what cairn *is* — it does not retain logs as its operating model and does not
// compete with Loki or Splunk. `fixtures/` has always been exactly this: captured
// producer output, kept deliberately, hand-redacted. capture is the intake tool
// for that corpus, not a step in the collection path, and it writes only where
// the operator points it.
//
// It never redacts. CLAUDE.md §3 makes hand redaction the process and the
// scanner the backstop, and a tool that redacted automatically would quietly
// become the process — at which point the first thing it failed to recognize
// would land in a corpus everyone had stopped reading.

func runCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		jobRaw = fs.String("job", "", "job id the incident concerns (required)")
		out    = fs.String("o", "", "fixture directory to create (default: <corpus>/<slug>)")
		slug   = fs.String("slug", "", "short kebab-case name, e.g. ib-link-flap")
		title  = fs.String("title", "", "one line describing what happened")
		before = fs.Duration("before", 15*time.Minute, "how far before the job's first event to capture")
		after  = fs.Duration("after", 5*time.Minute, "how far after the job's last event to capture")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn capture --job <id> --slug <name> --title "what happened"

Capture a real incident into a fixture skeleton: run the producers cairn reads,
save exactly what they printed, and write a meta.yaml for you to complete.

This is how the corpus gets built, and the corpus is the part of this project
that compounds (CLAUDE.md §9). It is also the step that could not previously be
automated at all — you had to know which commands to run and copy their output
under the right filenames by hand.

What it does NOT do is redact. §3 makes hand redaction the process and the
scanner a backstop; a tool that redacted automatically would become the process,
and the first thing it failed to recognize would land in the corpus unnoticed.

Output goes to the private corpus. Observed incidents are never committed to
this repository — see CLAUDE.md §3 for what may and may not be published.

After capturing:

  1. Read every file in input/ and redact by hand.
  2. make scan-fixtures     (clean, or every finding accounted for)
  3. Complete meta.yaml     — expected_root_cause is the eval target
  4. Write expected/events.json
  5. make check

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobRaw == "" {
		fs.Usage()
		return fmt.Errorf("--job is required")
	}
	job, err := schema.ParseJobID(*jobRaw)
	if err != nil {
		return err
	}
	if common.fixture != "" {
		return fmt.Errorf("--fixture replays a capture; it cannot be the source of one")
	}

	dir, err := captureDir(*out, *slug)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists; capture will not overwrite an incident", dir)
	}

	env, err := common.env()
	if err != nil {
		return err
	}
	rec := collectors.NewRecordingEnv(env)

	// The site profile names the cluster and the scheduler, which are two of the
	// meta.yaml fields a human would otherwise fill in from memory.
	set, _, err := common.sites()
	if err != nil {
		return err
	}
	profile, err := common.profileFor(set)
	if err != nil {
		return err
	}
	cluster := common.clusterNameWith(profile)

	// Collected through the ordinary path, so what is captured is exactly what
	// the collectors asked for — including the two-pass windowing, which is what
	// stops this saving a year of journal to describe twenty minutes.
	req := collectors.Request{Cluster: cluster, Job: job}
	_, results := collectForJob(rec, req, *before, *after)

	if rec.Captured() == 0 {
		return fmt.Errorf(
			"nothing was captured: no producer on this host answered. Run `cairn doctor` "+
				"to see what it can read, and capture on a node that can see job %s", job.Raw)
	}

	written, err := rec.Flush(dir)
	if err != nil {
		return err
	}
	if err := rec.Verify(dir); err != nil {
		return fmt.Errorf("this capture would not replay, so it is not usable as a fixture: %w", err)
	}

	node := schema.Hostname(env.Hostname())
	if err := writeMeta(dir, *slug, *title, cluster, job, node, profile); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "expected"), 0o700); err != nil {
		return err
	}

	fmt.Printf("captured %d file(s) into %s\n\n", len(written), dir)
	for _, w := range written {
		fmt.Printf("  input/%s\n", w)
	}
	reportCollectors(results)
	return reportScan(dir)
}

// captureDir resolves where the incident is written.
//
// Into the private corpus by default, and never into fixtures/. The public
// corpus is synthetic by construction (CLAUDE.md §3), and a tool that made it
// easy to put a real incident there would be undoing the boundary it is meant
// to serve.
func captureDir(out, slug string) (string, error) {
	if out != "" {
		if inPublicCorpus(out) {
			return "", fmt.Errorf(
				"%s is inside the public corpus. Observed incidents are never committed "+
					"(CLAUDE.md §3); capture into %s/ instead", out, corpuspath.Default)
		}
		return out, nil
	}
	if slug == "" {
		return "", fmt.Errorf("--slug is required unless -o names a directory")
	}
	if !slugOK.MatchString(slug) {
		return "", fmt.Errorf("--slug %q should be lower-case kebab, e.g. ib-link-flap", slug)
	}
	root, err := corpuspath.Find()
	if err != nil {
		return "", err
	}
	if root == "" {
		root = corpuspath.Default
	}
	return filepath.Join(root, nextIndex(root)+"-"+slug), nil
}

var slugOK = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func inPublicCorpus(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	pub, err := filepath.Abs("fixtures")
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(pub, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// nextIndex returns the next NNN prefix in a corpus directory.
func nextIndex(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "001"
	}
	max := 0
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < 3 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.Name()[:3], "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%03d", max+1)
}

// writeMeta writes a meta.yaml with everything cairn can know already filled in.
//
// synthetic is false and the redaction fields are left empty on purpose.
// fixtures.Validate refuses a non-synthetic fixture with no redacted_by, so the
// fixture cannot load — and cannot count toward an accuracy number — until a
// person has actually done the redaction pass and said so.
func writeMeta(dir, slug, title string, cluster schema.ClusterName,
	job *schema.JobID, node schema.Hostname, profile site.Profile) error {

	if title == "" {
		title = "TODO: one line describing what happened"
	}
	id := filepath.Base(dir)
	if slug == "" {
		slug = id
	}

	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", id)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "cluster: %s\n", cluster)
	b.WriteString("# The job under investigation, and the node the captures were taken on.\n")
	b.WriteString("incident:\n")
	fmt.Fprintf(&b, "  job: %q\n", job.Raw)
	fmt.Fprintf(&b, "  node: %s\n", orTODO(string(node)))
	b.WriteString("\n")
	b.WriteString("# Observed, not authored. This fixture counts toward accuracy once the\n")
	b.WriteString("# redaction fields below are filled in — until then it will not load.\n")
	b.WriteString("synthetic: false\n\n")
	b.WriteString("# TODO: the classes a correct collector must emit for this incident.\n")
	b.WriteString("expected_classes: []\n\n")
	b.WriteString("# TODO: what actually caused this, in prose. This is the eval target and\n")
	b.WriteString("# deserves real thought — it is what accuracy is measured against.\n")
	b.WriteString("expected_root_cause: >-\n  TODO\n\n")
	b.WriteString("# TODO: the producers this incident draws on.\n")
	b.WriteString("producers: []\n\n")
	b.WriteString("capability: unprivileged\n\n")
	b.WriteString("scheduler:\n")
	fmt.Fprintf(&b, "  name: %s\n", orTODO(profile.Scheduler.Kind))
	fmt.Fprintf(&b, "  version: %q\n\n", profile.Scheduler.Version)
	b.WriteString("# REQUIRED before this fixture will load. An unattributed redaction is an\n")
	b.WriteString("# unreviewed one (CLAUDE.md §3).\n")
	b.WriteString(`redacted_by: ""` + "\n")
	b.WriteString(`redaction_method: ""` + "\n\n")
	b.WriteString("notes: >-\n  TODO\n")

	return os.WriteFile(filepath.Join(dir, "meta.yaml"), []byte(b.String()), 0o600)
}

func orTODO(s string) string {
	if strings.TrimSpace(s) == "" {
		return "TODO"
	}
	return s
}

// reportCollectors says what each producer could and could not contribute, in
// doctor's shape. A capture missing the fabric is a different fixture from one
// whose fabric was healthy, and only this distinguishes them at capture time.
func reportCollectors(results []collectors.Result) {
	var missing []collectors.Capability
	for _, r := range results {
		missing = append(missing, r.Missing()...)
	}
	if len(missing) == 0 {
		return
	}
	fmt.Printf("\nWHAT THIS HOST COULD NOT CAPTURE\n")
	for _, c := range missing {
		fmt.Printf("  %s (%s) — %s\n", c.Name, c.Level, c.Detail)
		if c.Reveals != "" {
			fmt.Printf("      lost: %s\n", c.Reveals)
		}
	}
	fmt.Printf("\nIf any of those matter to this incident, capture again on a host that can\n" +
		"see them. A fixture is only as good as what was readable when it was taken.\n")
}

// reportScan runs the redaction scanner over the capture immediately.
//
// Immediately, because this is the moment the operator is still looking at the
// terminal and still remembers what the incident was. A scanner run deferred to
// commit time is a scanner run that happens after the material has been sitting
// in a directory for a week.
func reportScan(dir string) error {
	findings, err := scanDir(dir)
	if err != nil {
		return err
	}
	fmt.Printf("\nREDACTION\n")
	if len(findings) == 0 {
		fmt.Printf("  the scanner found nothing, which is not the same as redacted.\n" +
			"  Read every file in input/ yourself — the scanner is a backstop, not the\n" +
			"  process (CLAUDE.md §3).\n")
	} else {
		printFindings(findings)
	}
	fmt.Printf("\nNext: redact input/ by hand, then complete meta.yaml and expected/events.json.\n")
	fmt.Printf("This fixture will not load until redacted_by and redaction_method are set.\n")
	return nil
}

// maxExamplesPerRule bounds what the summary prints for one rule.
//
// A real journal capture produces thousands of findings — one per matching line
// — and printing them all is the mistake this project already made once and
// fixed: 353,526 individual warnings on a single host (CLAUDE.md §5). An
// operator needs to know which *kinds* of identifying material are in the
// capture and roughly how much; the full list is what the scanner is for.
const maxExamplesPerRule = 3

// printFindings summarizes by rule rather than listing every hit.
func printFindings(findings []scan.Finding) {
	byRule := map[string][]scan.Finding{}
	var order []string
	for _, f := range findings {
		if _, seen := byRule[f.Rule]; !seen {
			order = append(order, f.Rule)
		}
		byRule[f.Rule] = append(byRule[f.Rule], f)
	}
	sort.Strings(order)

	fmt.Printf("  %d finding(s) across %d rule(s), to deal with before this fixture\n"+
		"  is usable. Summarized by kind; run `make scan-fixtures` for every line.\n\n",
		len(findings), len(order))

	for _, rule := range order {
		hits := byRule[rule]
		fmt.Printf("  %-14s %d hit(s) — %s\n", rule, len(hits), hits[0].Why)

		// Distinct matches, because a hostname repeated 900 times is one thing
		// to redact, not 900. This is what tells an operator the size of the job.
		seen := map[string]bool{}
		var distinct []string
		for _, h := range hits {
			if !seen[h.Match] {
				seen[h.Match] = true
				distinct = append(distinct, h.Match)
			}
		}
		sort.Strings(distinct)
		shown := distinct
		if len(shown) > maxExamplesPerRule {
			shown = shown[:maxExamplesPerRule]
		}
		fmt.Printf("                 %d distinct: %s", len(distinct), strings.Join(shown, ", "))
		if len(distinct) > len(shown) {
			fmt.Printf(", …")
		}
		fmt.Println()
	}
}

func scanDir(dir string) ([]scan.Finding, error) {
	var out []scan.Finding
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out = append(out, scan.Scan(path, data)...)
		return nil
	})
	return out, err
}
