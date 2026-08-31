package schema

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update regenerates golden files: `go test ./schema -update`.
//
// Golden files are generated rather than hand-written so that they cannot drift
// from the code through a transcription error, and are committed so that the
// determinism they check holds across processes and machines, not just across
// two calls in one test.
var update = flag.Bool("update", false, "regenerate golden files")

func goldenPath(name string) string { return filepath.Join("testdata", name) }

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run: go test ./schema -update)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// TestClassesGolden enforces that the class enum is append-only.
//
// This is the mechanism behind CLAUDE.md §4: "Adding to it is a schema version
// bump, not a casual commit." Adding a class appends a line and requires the
// golden file to be regenerated deliberately; renaming or removing one changes
// an existing line, which shows up in review as exactly what it is — a breaking
// change to the wire format that every stored bundle depends on.
func TestClassesGolden(t *testing.T) {
	var b strings.Builder
	for _, c := range AllClasses() {
		b.WriteString(string(c))
		b.WriteByte('\n')
	}
	checkGolden(t, "classes.golden", []byte(b.String()))
}

// TestSourcesGolden applies the same discipline to the producer enum.
func TestSourcesGolden(t *testing.T) {
	var b strings.Builder
	for _, s := range AllSources() {
		b.WriteString(string(s))
		b.WriteByte('\n')
	}
	checkGolden(t, "sources.golden", []byte(b.String()))
}

// TestAttrsGolden pins the registered detail keys and their PII marking.
//
// The PII column is the load-bearing part. The redaction layer keys off it, so a
// key silently losing its PII flag would mean values that used to be
// pseudonymized start leaving the site in the clear — a change that is invisible
// in a code diff of attrs.go alone but obvious here.
func TestAttrsGolden(t *testing.T) {
	var b strings.Builder
	for _, c := range AllClasses() {
		for _, k := range RegisteredAttrs(c) {
			pii := "-"
			if AttrIsPII(c, k) {
				pii = "PII"
			}
			b.WriteString(string(c))
			b.WriteByte('\t')
			b.WriteString(k)
			b.WriteByte('\t')
			b.WriteString(pii)
			b.WriteByte('\n')
		}
	}
	checkGolden(t, "attrs.golden", []byte(b.String()))
}

// TestEventsGolden pins the canonical encoding itself.
//
// Because the golden file is committed, this test compares bytes produced now
// against bytes produced by a different process on a different machine at a
// different time. That is the actual claim of invariant §2.7, and it is not
// testable by encoding twice in one process.
func TestEventsGolden(t *testing.T) {
	got, err := EncodeEvents(sampleEvents(t))
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	checkGolden(t, "events.golden", got)
}

// TestBundleGolden pins the bundle header encoding.
func TestBundleGolden(t *testing.T) {
	b := Bundle{
		Cluster: ClusterName("cluster-a"),
		Window: Window{
			Start: mustTime(t, "2026-03-04T09:00:00.000000000Z"),
			End:   mustTime(t, "2026-03-04T10:00:00.000000000Z"),
		},
		Clocks: []ClockOffset{
			// Deliberately out of canonical order; Encode must sort them.
			{Source: SourceJournal, Node: "node-0042", OffsetNanos: -1_250_000_000, Method: "journal_realtime_delta"},
			{Source: SourceSlurm, Node: "", OffsetNanos: 0, Method: "assumed_zero"},
			{Source: SourceGPU, Node: "node-0042", OffsetNanos: 0, Method: "assumed_zero"},
		},
		Redaction: Redaction{Mode: "pseudonymize", SaltID: "sha256:0f1e2d3c"},
		Events:    sampleEvents(t),
	}
	got, err := b.Encode()
	if err != nil {
		t.Fatalf("Bundle.Encode: %v", err)
	}
	checkGolden(t, "bundle.golden", got)
}
