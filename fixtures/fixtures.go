// Package fixtures loads and validates cairn's incident corpus.
//
// The corpus is both the test suite and the eval set (CLAUDE.md §0.3), and those
// two roles pull in different directions. As a test suite it must be exact: an
// expected event stream is compared byte-for-byte. As an eval set it must be
// honest about provenance: a fixture invented to exercise a code path is not
// evidence that cairn classifies real incidents correctly, and must never be
// counted as though it were.
//
// The `synthetic` flag in each fixture's meta.yaml is what keeps those roles
// apart. Loaders that measure accuracy use Real(); loaders that exercise code
// use everything.
package fixtures

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/touchelos/cairn/corpuspath"
	"github.com/touchelos/cairn/schema"

	"gopkg.in/yaml.v3"
)

// Capability records what access level the fixture was captured at. Invariant
// §2.2 is that cairn runs unprivileged, so a corpus consisting only of
// root-captured incidents would prove nothing about the deployment that matters.
type Capability string

const (
	// CapUnprivileged: captured as an ordinary user on a login node or inside a
	// job — the access level cairn must work at.
	CapUnprivileged Capability = "unprivileged"
	// CapRoot: captured with elevated access. Root buys better data but never
	// gates the tool, so these fixtures test the richer path, not the required one.
	CapRoot Capability = "root"
)

// Scheduler identifies the scheduler the incident came from.
type Scheduler struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// Meta is the contents of a fixture's meta.yaml.
type Meta struct {
	// ID must equal the fixture's directory name.
	ID    string `yaml:"id"`
	Title string `yaml:"title"`

	// Cluster is the redacted cluster name the incident came from.
	//
	// It is recorded here rather than derived from the expected events because a
	// collector has to be *told* which cluster it is running against — a cluster
	// name is site configuration, not something sacct output reveals. Deriving it
	// from the expected stream would let a replay harness quietly supply the
	// answer it is meant to be checking.
	Cluster string `yaml:"cluster"`

	// Synthetic marks a fixture that was authored rather than observed.
	//
	// This flag is load-bearing, not documentation. Synthetic fixtures exercise
	// the harness and serve as per-class templates; they are excluded from any
	// accuracy measurement, because measuring a classifier against incidents
	// written to suit it is measuring nothing.
	Synthetic bool `yaml:"synthetic"`

	// ExpectedClasses is every class the fixture should produce. It is checked
	// against the actual expected event stream, so the two cannot drift.
	ExpectedClasses []string `yaml:"expected_classes"`

	// ExpectedRootCause is the human-language answer: what actually went wrong.
	// This is the eval target — the classes are intermediate evidence, and a run
	// that produces every right class and the wrong conclusion has still failed.
	ExpectedRootCause string `yaml:"expected_root_cause"`

	// Incident says what this fixture is about: which job, and which node the
	// captures were taken on.
	//
	// A collector is invoked with a job to investigate — that is the whole shape
	// of `cairn context --job <id>` — so a replay harness has to supply one.
	// Deriving it from the expected events would let the harness hand the
	// collector the answer it is meant to be finding.
	//
	// Node doubles as the capture host. Log files written by a daemon carry no
	// hostname of their own, so events parsed out of slurmd.log inherit the node
	// the file was read from.
	Incident Incident `yaml:"incident"`

	Producers  []string   `yaml:"producers"`
	Capability Capability `yaml:"capability"`
	Scheduler  Scheduler  `yaml:"scheduler"`

	// RedactedBy and RedactionMethod record who performed the redaction and how.
	// Required for real fixtures (CLAUDE.md §3): an unattributed redaction is an
	// unreviewed one.
	RedactedBy      string `yaml:"redacted_by"`
	RedactionMethod string `yaml:"redaction_method"`

	Notes string `yaml:"notes"`
}

// Incident identifies what a fixture is about.
type Incident struct {
	Job  string `yaml:"job"`
	Node string `yaml:"node"`
}

// Fixture is a loaded incident.
type Fixture struct {
	// Dir is the path the fixture was loaded from.
	Dir  string
	Meta Meta

	// InputFiles are the redacted producer outputs, relative to Dir, sorted.
	InputFiles []string

	// Expected is the decoded event stream a correct collector run must produce.
	Expected []schema.Event

	// ExpectedRaw is the exact bytes of expected/events.json. Phase 1 compares
	// against these bytes, not against the decoded events: canonical form is part
	// of the contract (invariant §2.7), so a run that produces the right events
	// in the wrong bytes has still broken something.
	ExpectedRaw []byte
}

// Classes returns every distinct class in the expected stream, sorted.
func (f *Fixture) Classes() []schema.Class {
	seen := map[schema.Class]struct{}{}
	for _, e := range f.Expected {
		seen[e.Class] = struct{}{}
	}
	out := make([]schema.Class, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

const (
	metaFile     = "meta.yaml"
	inputDir     = "input"
	expectedDir  = "expected"
	expectedFile = "events.json"
)

// Load reads and fully validates a single fixture directory.
func Load(dir string) (*Fixture, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", dir, err)
	}

	var m Meta
	dec := yaml.NewDecoder(strings.NewReader(string(metaBytes)))
	dec.KnownFields(true) // an unrecognized key is a typo, not an extension
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("fixture %s: parsing %s: %w", dir, metaFile, err)
	}

	f := &Fixture{Dir: dir, Meta: m}

	entries, err := os.ReadDir(filepath.Join(dir, inputDir))
	if err != nil {
		return nil, fmt.Errorf("fixture %s: reading %s/: %w", dir, inputDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			f.InputFiles = append(f.InputFiles, e.Name())
		}
	}
	sort.Strings(f.InputFiles)

	raw, err := os.ReadFile(filepath.Join(dir, expectedDir, expectedFile))
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", dir, err)
	}
	f.ExpectedRaw = raw

	evs, err := schema.DecodeEvents(raw)
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %s/%s: %w", dir, expectedDir, expectedFile, err)
	}
	f.Expected = evs

	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// LoadAll reads every fixture under root, in directory-name order.
func LoadAll(root string) ([]*Fixture, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading fixture root %s: %w", root, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	out := make([]*Fixture, 0, len(dirs))
	for _, d := range dirs {
		f, err := Load(filepath.Join(root, d))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Real returns only the fixtures derived from observed incidents.
//
// Accuracy is reported over these and nothing else. A number computed over
// authored fixtures would measure how well cairn agrees with its own authors,
// and CLAUDE.md §9 names the eval harness as a credibility asset — which it
// stops being the moment that distinction blurs.
func Real(fs []*Fixture) []*Fixture {
	out := make([]*Fixture, 0, len(fs))
	for _, f := range fs {
		if !f.Meta.Synthetic {
			out = append(out, f)
		}
	}
	return out
}

// Validate enforces every rule a fixture must satisfy.
func (f *Fixture) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("fixture %s: "+format, append([]any{f.Dir}, args...)...)
	}

	if f.Meta.ID == "" {
		return fail("meta.id is empty")
	}
	if base := filepath.Base(f.Dir); f.Meta.ID != base {
		return fail("meta.id is %q but the directory is named %q; they must match so a "+
			"fixture cannot be renamed without its identity following", f.Meta.ID, base)
	}
	if f.Meta.Title == "" {
		return fail("meta.title is empty")
	}
	if f.Meta.Cluster == "" {
		return fail("meta.cluster is empty")
	}
	if f.Meta.ExpectedRootCause == "" {
		return fail("meta.expected_root_cause is empty; it is the eval target, not a comment")
	}
	switch f.Meta.Capability {
	case CapUnprivileged, CapRoot:
	default:
		return fail("meta.capability is %q, want %q or %q", f.Meta.Capability, CapUnprivileged, CapRoot)
	}
	if f.Meta.Incident.Job != "" {
		if _, err := schema.ParseJobID(f.Meta.Incident.Job); err != nil {
			return fail("meta.incident.job: %w", err)
		}
	}
	if len(f.InputFiles) == 0 {
		return fail("%s/ is empty; a fixture with no producer output tests nothing", inputDir)
	}

	// Provenance. Real fixtures must record who redacted them and how; §3 makes
	// hand redaction the process, and an unattributed redaction is unreviewed.
	if !f.Meta.Synthetic {
		if f.Meta.RedactedBy == "" {
			return fail("meta.redacted_by is required for a non-synthetic fixture (CLAUDE.md §3)")
		}
		if f.Meta.RedactionMethod == "" {
			return fail("meta.redaction_method is required for a non-synthetic fixture (CLAUDE.md §3)")
		}
	}

	for _, p := range f.Meta.Producers {
		if !schema.Source(p).Valid() {
			return fail("meta.producers lists unknown producer %q", p)
		}
	}

	// Declared classes must be enum members.
	declared := map[schema.Class]struct{}{}
	for _, c := range f.Meta.ExpectedClasses {
		cls, err := schema.ParseClass(c)
		if err != nil {
			return fail("meta.expected_classes: %w", err)
		}
		declared[cls] = struct{}{}
	}

	// Declared classes and the actual expected stream must agree exactly, in both
	// directions. A fixture whose declaration has drifted from its events is
	// worse than no fixture: it passes the golden comparison while documenting
	// something that is no longer true.
	actual := map[schema.Class]struct{}{}
	for _, c := range f.Classes() {
		actual[c] = struct{}{}
	}
	for c := range declared {
		if _, ok := actual[c]; !ok {
			return fail("meta.expected_classes declares %q but no expected event has that class", c)
		}
	}
	for c := range actual {
		if _, ok := declared[c]; !ok {
			return fail("expected events contain class %q but meta.expected_classes does not declare it", c)
		}
	}

	// Producers must likewise match.
	declaredSrc := map[schema.Source]struct{}{}
	for _, p := range f.Meta.Producers {
		declaredSrc[schema.Source(p)] = struct{}{}
	}
	for _, e := range f.Expected {
		if _, ok := declaredSrc[e.Source]; !ok {
			return fail("expected events include source %q but meta.producers does not declare it", e.Source)
		}
	}

	// Every event must belong to the cluster the meta declares. A fixture is one
	// incident on one cluster; a stray cluster name is a redaction slip, and a
	// declaration that disagrees with the events is a fixture nobody re-read.
	for _, e := range f.Expected {
		if string(e.Cluster) != f.Meta.Cluster {
			return fail("meta.cluster is %q but an expected event has cluster %q",
				f.Meta.Cluster, e.Cluster)
		}
	}

	// Canonical form. Checked here rather than only in Phase 1 because a golden
	// file that is merely valid will start failing spuriously the first time a
	// collector emits the same events in a different order.
	ok, want, err := schema.IsCanonical(f.ExpectedRaw)
	if err != nil {
		return fail("%s/%s: %w", expectedDir, expectedFile, err)
	}
	if !ok {
		return fail("%s/%s is not in canonical form.\n--- want ---\n%s\n--- got ---\n%s",
			expectedDir, expectedFile, want, f.ExpectedRaw)
	}

	return nil
}

// ---------- the private corpus ----------

// PrivateCorpusEnv names the environment variable pointing at the private
// corpus. See package corpuspath for why the definition lives there.
const PrivateCorpusEnv = corpuspath.Env

// DefaultPrivateCorpus is the in-tree directory the private corpus lives in.
const DefaultPrivateCorpus = corpuspath.Default

// PrivateCorpus locates the private corpus, returning "" when there is none.
//
// Observed incidents cannot be committed to this repository — not the raw
// producer output, not the hand-redacted output, and not the event streams
// derived from them (CLAUDE.md §3). They live here instead, and what is
// published is the taxonomy built from them plus accuracy measured over them:
// §9's "publish the methodology, not the data", as an arrangement of files
// rather than an intention.
func PrivateCorpus() (string, error) { return corpuspath.Find() }

// InProgress reports whether a directory holds a capture nobody has finished.
//
// A capture starts life with `synthetic: false` and empty redaction fields —
// `cairn capture` writes it that way deliberately, so that Validate refuses it
// until a person has done the redaction pass and said so.
//
// That refusal is right, but it must not present as a broken test suite.
// Building the corpus means twenty incidents captured over weeks, most of them
// half-finished at any moment, and a test run that fails on "no such file:
// expected/events.json" every time is one an operator stops running — taking
// the redaction scanner and every other guard down with it.
//
// So an unfinished capture is skipped and named, never loaded. It cannot reach
// Real() and cannot count toward an accuracy number, because it never becomes a
// Fixture at all. Setting redacted_by is the act that opts it back in to full
// validation.
func InProgress(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return false
	}
	var m struct {
		Synthetic  bool   `yaml:"synthetic"`
		RedactedBy string `yaml:"redacted_by"`
	}
	if yaml.Unmarshal(data, &m) != nil {
		return false
	}
	return !m.Synthetic && strings.TrimSpace(m.RedactedBy) == ""
}

// LoadCorpus loads the public corpus at publicRoot plus the private one when
// present. The second return names private captures that are not finished.
//
// Fixtures in the public root must be synthetic, and that is enforced here
// rather than only in a test. This is the function every caller goes through, so
// a real incident that somehow reached fixtures/ fails to load at all instead of
// failing one assertion in one package.
func LoadCorpus(publicRoot string) ([]*Fixture, []string, error) {
	fs, err := LoadAll(publicRoot)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range fs {
		if !f.Meta.Synthetic {
			return nil, nil, fmt.Errorf(
				"%s: an observed fixture is in the public corpus at %s. Observed "+
					"incidents belong in the private corpus (CLAUDE.md §3) and must never "+
					"be committed; move it to %s/", f.Meta.ID, publicRoot, DefaultPrivateCorpus)
		}
	}

	priv, err := PrivateCorpus()
	if err != nil {
		return nil, nil, err
	}
	if priv == "" {
		return fs, nil, nil
	}

	entries, err := os.ReadDir(priv)
	if err != nil {
		return nil, nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Meta.ID] = true
	}

	var pending []string
	for _, d := range dirs {
		full := filepath.Join(priv, d)
		if InProgress(full) {
			pending = append(pending, full)
			continue
		}
		f, err := Load(full)
		if err != nil {
			return nil, nil, err
		}
		if seen[f.Meta.ID] {
			return nil, nil, fmt.Errorf("fixture id %q appears in both the public and private corpus", f.Meta.ID)
		}
		seen[f.Meta.ID] = true
		fs = append(fs, f)
	}
	return fs, pending, nil
}
