package redact_test

import (
	"strings"
	"testing"
	"time"

	"github.com/touchelos/cairn/redact"
	"github.com/touchelos/cairn/redact/scan"
	"github.com/touchelos/cairn/schema"
)

var salt = []byte("a-test-salt-of-sufficient-length")

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := schema.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func bundle(t *testing.T) schema.Bundle {
	t.Helper()
	return schema.Bundle{
		Cluster: "hpc-prod",
		Window: schema.Window{
			Start: ts(t, "2026-03-04T09:00:00.000000000Z"),
			End:   ts(t, "2026-03-04T10:00:00.000000000Z"),
		},
		Clocks: []schema.ClockOffset{
			{Source: schema.SourceJournal, Node: "compute042", OffsetNanos: -1e9, Method: "journal_realtime_delta"},
		},
		Redaction: schema.Redaction{Mode: "none"},
		Events: []schema.Event{
			{
				TS: ts(t, "2026-03-04T09:14:02.000000000Z"), Cluster: "hpc-prod", Node: "compute042",
				Source: schema.SourceSlurm, Class: schema.ClassResourceOOM,
				Detail: schema.Detail{
					Signature: "slurm.sacct.state.OUT_OF_MEMORY",
					Attrs: map[string]string{
						"state":   "OUT_OF_MEMORY",
						"user":    "asmith",
						"account": "TG-CHE200098",
						// A free-form value carrying two identifiers inside a path.
						"cgroup": "/sys/fs/cgroup/slurm/uid_1001/job_918273/compute042",
					},
				},
			},
			{
				TS: ts(t, "2026-03-04T09:14:03.000000000Z"), Cluster: "hpc-prod", Node: "compute043",
				Source: schema.SourceStorage, Class: schema.ClassStorageIOError,
				Detail: schema.Detail{
					Signature: "nfs.server_not_responding",
					// A username that appears in no `user` attr anywhere.
					Attrs: map[string]string{"mount": "/home/bjones/scratch", "fs_type": "nfs"},
				},
			},
		},
	}
}

func mustRedact(t *testing.T, b schema.Bundle) schema.Bundle {
	t.Helper()
	r, err := redact.New(redact.ModePseudonymize, salt)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Bundle(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNoOriginalSurvives is the claim the whole package exists to make: after
// redaction, nothing identifying from the input appears anywhere in the encoded
// output. CLAUDE.md §10 — an unredacted hostname on an outbound path is a bug.
func TestNoOriginalSurvives(t *testing.T) {
	out := mustRedact(t, bundle(t))
	encoded, err := out.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)

	for _, secret := range []string{
		"hpc-prod", "compute042", "compute043", "asmith", "bjones", "TG-CHE200098",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived redaction:\n%s", secret, got)
		}
	}
}

// TestRedactedBundlePassesTheScanner closes the loop between the two halves of
// redact/: whatever this package emits must satisfy the scanner that guards the
// corpus. If they disagree, one of them is wrong and neither can be trusted.
func TestRedactedBundlePassesTheScanner(t *testing.T) {
	out := mustRedact(t, bundle(t))
	encoded, err := out.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range scan.Scan("bundle.json", encoded) {
		t.Errorf("the redactor produced output its own scanner rejects: %s", f)
	}
}

// TestEvidenceSurvives: redaction must not destroy the diagnostic content. A
// bundle that is safe but useless has not solved the problem.
func TestEvidenceSurvives(t *testing.T) {
	out := mustRedact(t, bundle(t))
	encoded, _ := out.Encode()
	got := string(encoded)

	for _, keep := range []string{
		"OUT_OF_MEMORY",                   // the state
		"slurm.sacct.state.OUT_OF_MEMORY", // the signature
		"job_918273",                      // the job id inside the cgroup path
		"/sys/fs/cgroup/slurm",            // the path's shape
		"nfs",                             // the filesystem type
		"2026-03-04T09:14:02",             // the timestamp
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction destroyed evidence: %q is missing from\n%s", keep, got)
		}
	}
}

// TestStableAcrossBundles is why pseudonyms are derived rather than counted.
// Two bundles from the same site must agree about who is who, or incidents
// cannot be compared over time and SaltID means nothing.
func TestStableAcrossBundles(t *testing.T) {
	first := mustRedact(t, bundle(t))

	// A second bundle with a different, larger set of hosts. Counting-based
	// assignment would renumber everything.
	b := bundle(t)
	b.Events = append([]schema.Event{{
		TS: ts(t, "2026-03-04T09:00:00.000000000Z"), Cluster: "hpc-prod", Node: "compute001",
		Source: schema.SourceGPU, Class: schema.ClassGPUXid,
		Detail: schema.Detail{Signature: "nvidia.xid.79", Attrs: map[string]string{"xid": "79"}},
	}}, b.Events...)
	second := mustRedact(t, b)

	find := func(bd schema.Bundle, sig string) schema.Hostname {
		for _, e := range bd.Events {
			if e.Detail.Signature == sig {
				return e.Node
			}
		}
		return ""
	}
	a := find(first, "slurm.sacct.state.OUT_OF_MEMORY")
	c := find(second, "slurm.sacct.state.OUT_OF_MEMORY")
	if a == "" || a != c {
		t.Errorf("the same host got different pseudonyms in two bundles: %q vs %q", a, c)
	}
	if first.Cluster != second.Cluster {
		t.Errorf("the cluster pseudonym changed between bundles: %q vs %q", first.Cluster, second.Cluster)
	}
	if first.Redaction.SaltID != second.Redaction.SaltID {
		t.Error("SaltID differs between two bundles redacted with the same salt")
	}
}

// TestDifferentSaltsDiverge: two sites must not produce the same pseudonyms, or
// a shared corpus would leak cross-site identity.
func TestDifferentSaltsDiverge(t *testing.T) {
	other, err := redact.New(redact.ModePseudonymize, []byte("a-different-salt-entirely-here!!"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.Bundle(bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	mine := mustRedact(t, bundle(t))
	if b.Events[0].Node == mine.Events[0].Node {
		t.Error("two different salts produced the same pseudonym")
	}
	if b.Redaction.SaltID == mine.Redaction.SaltID {
		t.Error("two different salts produced the same SaltID")
	}
}

// TestSaltIDLeaksNothing: the identifier travels with the bundle, so it must not
// be the salt or contain it.
func TestSaltIDLeaksNothing(t *testing.T) {
	r, err := redact.New(redact.ModePseudonymize, salt)
	if err != nil {
		t.Fatal(err)
	}
	id := r.SaltID()
	if id == "" || !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("unexpected SaltID %q", id)
	}
	if strings.Contains(id, string(salt)) {
		t.Error("SaltID contains the salt")
	}
	// A short fingerprint is deliberate: it identifies without offering material
	// for an offline attack on the salt.
	if len(id) > 24 {
		t.Errorf("SaltID is %d chars; it should be a short fingerprint, not a digest", len(id))
	}
}

// TestDeterministic: §2.7 must hold through redaction, or a redacted bundle
// cannot be diffed against another.
func TestDeterministic(t *testing.T) {
	first, err := mustRedact(t, bundle(t)).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, err := mustRedact(t, bundle(t)).Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("redaction is not deterministic on run %d:\n%s\nvs\n%s", i, first, got)
		}
	}
}

func TestUnsaltedPseudonymizationIsRefused(t *testing.T) {
	for _, s := range [][]byte{nil, {}, []byte("short")} {
		if _, err := redact.New(redact.ModePseudonymize, s); err == nil {
			t.Errorf("accepted a %d-byte salt; an unsalted pseudonym is a reversible hash of the hostname", len(s))
		}
	}
	// ModeNone needs no salt: nothing is being pseudonymized.
	if _, err := redact.New(redact.ModeNone, nil); err != nil {
		t.Errorf("ModeNone rejected a nil salt: %v", err)
	}
}

// TestModeNoneIsRecorded: a bundle that was not redacted must say so, rather
// than leaving a recipient to guess.
func TestModeNoneIsRecorded(t *testing.T) {
	r, err := redact.New(redact.ModeNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Bundle(bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if out.Redaction.Mode != string(redact.ModeNone) {
		t.Errorf("redaction mode is %q, want %q", out.Redaction.Mode, redact.ModeNone)
	}
	if out.Events[0].Node != "compute042" {
		t.Error("ModeNone altered a value")
	}
	if out.Redaction.SaltID != "" {
		t.Error("ModeNone reported a SaltID")
	}
}

// TestMappingResolvesLocally: the site keeps the key to its own bundles.
func TestMappingResolvesLocally(t *testing.T) {
	r, err := redact.New(redact.ModePseudonymize, salt)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Bundle(bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Mapping()
	if len(m) == 0 {
		t.Fatal("no mapping recorded; the site cannot resolve its own bundle")
	}
	byPseudo := map[string]string{}
	for _, e := range m {
		byPseudo[e.Pseudonym] = e.Original
	}
	if got := byPseudo[string(out.Events[0].Node)]; got != "compute042" {
		t.Errorf("mapping resolved %q to %q, want compute042", out.Events[0].Node, got)
	}

	// Mapping order must be stable, or the file a site keeps churns on every run.
	for i := 0; i < 20; i++ {
		again := r.Mapping()
		for j := range again {
			if again[j] != m[j] {
				t.Fatal("Mapping() order is not stable")
			}
		}
	}
}

// TestNonPIIAttrsAreUntouched: the PII flag in the schema is what drives this,
// so a value not marked must pass through byte-identical.
func TestNonPIIAttrsAreUntouched(t *testing.T) {
	out := mustRedact(t, bundle(t))
	for _, e := range out.Events {
		if e.Detail.Signature == "slurm.sacct.state.OUT_OF_MEMORY" {
			if e.Detail.Attrs["state"] != "OUT_OF_MEMORY" {
				t.Errorf("a non-PII attr was altered: %q", e.Detail.Attrs["state"])
			}
		}
		if e.Detail.Signature == "nfs.server_not_responding" {
			if e.Detail.Attrs["fs_type"] != "nfs" {
				t.Errorf("a non-PII attr was altered: %q", e.Detail.Attrs["fs_type"])
			}
		}
	}
}

// TestHomePathUsernameIsCaught covers the case structured fields cannot: a
// username that appears in no `user` attr on any event, only inside a path.
func TestHomePathUsernameIsCaught(t *testing.T) {
	out := mustRedact(t, bundle(t))
	for _, e := range out.Events {
		if e.Detail.Signature != "nfs.server_not_responding" {
			continue
		}
		mount := e.Detail.Attrs["mount"]
		if strings.Contains(mount, "bjones") {
			t.Errorf("a username only ever seen inside a path survived: %q", mount)
		}
		if !strings.HasPrefix(mount, "/home/user-") {
			t.Errorf("the path lost its shape: %q", mount)
		}
		if !strings.HasSuffix(mount, "/scratch") {
			t.Errorf("the path lost its tail: %q", mount)
		}
	}
}
