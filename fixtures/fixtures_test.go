package fixtures

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/toucheLos/cairn/redact/scan"
	"github.com/toucheLos/cairn/schema"
)

const root = "."

func loadAll(t *testing.T) []*Fixture {
	t.Helper()
	fs, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("no fixtures found")
	}
	return fs
}

// TestLoadAll is the corpus-wide validator: every rule in Validate runs against
// every fixture. Load() calls Validate() itself, so a failure here names the
// fixture and the rule it broke.
func TestLoadAll(t *testing.T) {
	fs := loadAll(t)
	for _, f := range fs {
		t.Logf("%-26s %d events  classes=%v  synthetic=%v",
			f.Meta.ID, len(f.Expected), f.Classes(), f.Meta.Synthetic)
	}
}

// TestCorpusIsRedacted runs the scanner over the committed corpus.
//
// The pre-commit hook is the first line of defence and CI is the second, but a
// hook is only installed if someone remembers to install it. This runs wherever
// `go test ./...` runs.
func TestCorpusIsRedacted(t *testing.T) {
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !scan.IsFixtureData(path) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sup, err := loadSuppressionsForTest(filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, f := range scan.ScanWith(path, content, sup) {
			t.Errorf("unredacted material in the committed corpus: %s", f)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking corpus: %v", err)
	}
}

func loadSuppressionsForTest(dir string) (scan.Suppressions, error) {
	content, err := os.ReadFile(filepath.Join(dir, scan.SuppressionFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return scan.ParseSuppressions(filepath.Join(dir, scan.SuppressionFile), content)
}

// TestSyntheticFixturesAreLabelled guards the boundary between the test suite
// and the eval set. A synthetic fixture that lost its flag would silently start
// counting toward an accuracy number, which is the one failure mode that makes
// the eval harness worse than not having one (CLAUDE.md §9).
func TestSyntheticFixturesAreLabelled(t *testing.T) {
	fs := loadAll(t)
	real := Real(fs)

	// Phase 0 ships seven authored fixtures and no observed ones. When the first
	// real incident lands this expectation changes — deliberately, in a commit
	// that says so.
	if len(real) != 0 {
		t.Logf("corpus now contains %d observed fixture(s); accuracy may be reported over these", len(real))
		for _, f := range real {
			if f.Meta.RedactedBy == "" || f.Meta.RedactionMethod == "" {
				t.Errorf("%s: observed fixture is missing redaction provenance", f.Meta.ID)
			}
		}
	}
	for _, f := range fs {
		if f.Meta.Synthetic && f.Meta.Notes == "" {
			t.Errorf("%s: a synthetic fixture must say in notes what it is for", f.Meta.ID)
		}
	}
}

// TestNamedFailureModesAreCovered checks the corpus against the seven modes
// CLAUDE.md §0.3 names by hand. It is a coverage floor, not a ceiling.
func TestNamedFailureModesAreCovered(t *testing.T) {
	fs := loadAll(t)
	present := map[schema.Class]string{}
	for _, f := range fs {
		for _, c := range f.Classes() {
			present[c] = f.Meta.ID
		}
	}
	for _, want := range []schema.Class{
		schema.ClassResourceOOM,              // OOM
		schema.ClassResourceWalltimeExceeded, // walltime
		schema.ClassGPUDriverMismatch,        // driver-generation mismatch
		schema.ClassSchedNodeFail,            // node-not-responding
		schema.ClassFabricLinkFlap,           // IB link flap
		schema.ClassAuthMunge,                // Munge auth failure
		schema.ClassFabricNCCLTimeout,        // NCCL hang
	} {
		if _, ok := present[want]; !ok {
			t.Errorf("no fixture covers %q, which CLAUDE.md §0.3 names explicitly", want)
		}
	}
}

// TestEveryProducerAppears: a corpus that only exercises slurm and journal would
// let the other collectors ship untested. This documents which producers still
// have no fixture rather than asserting a state the corpus has not reached.
func TestEveryProducerAppears(t *testing.T) {
	fs := loadAll(t)
	seen := map[schema.Source]bool{}
	for _, f := range fs {
		for _, e := range f.Expected {
			seen[e.Source] = true
		}
	}
	for _, s := range schema.AllSources() {
		if !seen[s] {
			t.Logf("no fixture yet exercises producer %q", s)
		}
	}
}

// TestExpectedFilesAreCanonical re-checks canonical form independently of
// Load(). Phase 1 compares collector output against these bytes, so a golden
// file that is merely valid JSON rather than canonical JSON would produce
// failures that look like collector regressions.
func TestExpectedFilesAreCanonical(t *testing.T) {
	for _, f := range loadAll(t) {
		ok, want, err := schema.IsCanonical(f.ExpectedRaw)
		if err != nil {
			t.Errorf("%s: %v", f.Meta.ID, err)
			continue
		}
		if !ok {
			t.Errorf("%s: expected/events.json is not canonical.\n--- want ---\n%s", f.Meta.ID, want)
		}
	}
}

// TestFixtureIDsAreUniqueAndOrdered keeps the corpus navigable as it grows: the
// numeric prefix is how fixtures are referred to in tickets and eval reports.
func TestFixtureIDsAreUniqueAndOrdered(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range loadAll(t) {
		if seen[f.Meta.ID] {
			t.Errorf("duplicate fixture id %q", f.Meta.ID)
		}
		seen[f.Meta.ID] = true
		if len(f.Meta.ID) < 4 || f.Meta.ID[3] != '-' {
			t.Errorf("fixture id %q should start with a three-digit prefix and a dash", f.Meta.ID)
		}
		for _, c := range f.Meta.ID[:3] {
			if c < '0' || c > '9' {
				t.Errorf("fixture id %q does not start with three digits", f.Meta.ID)
				break
			}
		}
	}
}

// TestValidateCatchesBrokenFixtures proves the validator has teeth by building
// deliberately broken fixtures in a temp dir. A validator that has never been
// seen to reject anything is a validator nobody has checked.
func TestValidateCatchesBrokenFixtures(t *testing.T) {
	const goodMeta = `id: 999-probe
title: probe
synthetic: true
expected_classes:
  - resource.oom
expected_root_cause: a probe fixture built by the validator's own test
producers:
  - slurm
capability: unprivileged
scheduler:
  name: slurm
  version: "23.11.6"
redacted_by: ""
redaction_method: ""
notes: built by TestValidateCatchesBrokenFixtures
`
	const goodEvents = `[
  {"ts":"2026-03-04T09:14:02.000000000Z","cluster":"cluster-a","node":"node-0042","jobid":{"raw":"918273.batch","base":918273,"step":"batch"},"source":"slurm","class":"resource.oom","detail":{"signature":"slurm.sacct.state.OUT_OF_MEMORY","attrs":{"state":"OUT_OF_MEMORY"}}}
]
`

	build := func(t *testing.T, name, meta, events string, withInput bool) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		mustMkdir(t, filepath.Join(dir, "input"))
		mustMkdir(t, filepath.Join(dir, "expected"))
		mustWrite(t, filepath.Join(dir, "meta.yaml"), meta)
		mustWrite(t, filepath.Join(dir, "expected", "events.json"), events)
		if withInput {
			mustWrite(t, filepath.Join(dir, "input", "sacct.txt"), "JobID State\n918273 OUT_OF_MEMORY\n")
		}
		return dir
	}

	// Baseline: the probe must load, or the negative cases below prove nothing.
	if _, err := Load(build(t, "999-probe", goodMeta, goodEvents, true)); err != nil {
		t.Fatalf("baseline probe fixture should load: %v", err)
	}

	cases := map[string]struct {
		name    string
		meta    string
		events  string
		hasInpt bool
	}{
		"id does not match directory": {
			name: "999-probe", meta: replace(goodMeta, "id: 999-probe", "id: 998-other"),
			events: goodEvents, hasInpt: true,
		},
		"empty input directory": {
			name: "999-probe", meta: goodMeta, events: goodEvents, hasInpt: false,
		},
		"missing root cause": {
			name: "999-probe",
			meta: replace(goodMeta, "expected_root_cause: a probe fixture built by the validator's own test",
				`expected_root_cause: ""`),
			events: goodEvents, hasInpt: true,
		},
		"undeclared class in events": {
			name: "999-probe", meta: replace(goodMeta, "  - resource.oom", "  - app.segfault"),
			events: goodEvents, hasInpt: true,
		},
		"declared class with no event": {
			name: "999-probe", meta: replace(goodMeta, "  - resource.oom", "  - resource.oom\n  - gpu.xid"),
			events: goodEvents, hasInpt: true,
		},
		"undeclared producer": {
			name: "999-probe", meta: replace(goodMeta, "  - slurm", "  - gpu"),
			events: goodEvents, hasInpt: true,
		},
		"unknown capability": {
			name: "999-probe", meta: replace(goodMeta, "capability: unprivileged", "capability: sudo"),
			events: goodEvents, hasInpt: true,
		},
		"unknown meta key": {
			name: "999-probe", meta: goodMeta + "unexpected_key: 1\n",
			events: goodEvents, hasInpt: true,
		},
		"real fixture without redaction provenance": {
			name: "999-probe", meta: replace(goodMeta, "synthetic: true", "synthetic: false"),
			events: goodEvents, hasInpt: true,
		},
		"non-canonical expected events": {
			// Valid JSON, correct events, wrong bytes: indentation removed.
			name: "999-probe", meta: goodMeta,
			events:  replace(goodEvents, "[\n  {", "[{"),
			hasInpt: true,
		},
		"unregistered attr in expected events": {
			name: "999-probe", meta: goodMeta,
			events:  replace(goodEvents, `"state":"OUT_OF_MEMORY"`, `"raw_line":"kernel: blah"`),
			hasInpt: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := build(t, tc.name, tc.meta, tc.events, tc.hasInpt)
			if _, err := Load(dir); err == nil {
				t.Errorf("validator accepted a fixture with: %s", name)
			}
		})
	}
}

func replace(s, old, new string) string {
	out := ""
	i := indexOf(s, old)
	if i < 0 {
		panic("test setup: substring not found: " + old)
	}
	out = s[:i] + new + s[i+len(old):]
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
