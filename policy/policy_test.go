package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var when = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// fakeAction is an action that exists only inside these tests.
//
// It is the reason the action set is a field rather than a package registry:
// the gates below have to be exercised against something that gets *past* the
// first one, and there must be no way for that something to reach a production
// engine. newWithActions is unexported and shippedActions has no Add.
var fakeAction = ActionSpec{Kind: "test.reversible", Undo: "undone by the test"}

// memSink records in memory. failSink always fails, which is the case that
// matters most and cannot be produced with a real file.
type memSink struct{ got []Decision }

func (m *memSink) Record(d Decision) error { m.got = append(m.got, d); return nil }
func (m *memSink) Where() string           { return "memory" }

type failSink struct{ calls int }

func (f *failSink) Record(Decision) error { f.calls++; return errors.New("disk on fire") }
func (f *failSink) Where() string         { return "failing" }

// permissive is the most dangerous policy a site could write: everything
// allowed, fleet-wide scope, dry-run off. Used to prove the gates that remain.
func permissive() Policy {
	return Policy{
		Version: ConfigVersion,
		Allow:   []Kind{fakeAction.Kind},
		Nodes:   []string{"*"},
		Jobs:    []string{"*"},
		DryRun:  false,
		Path:    "test-policy.yaml",
	}
}

// ---------- the null action set ----------

// TestNullActionSet is the claim this whole phase makes about the binary.
func TestNullActionSet(t *testing.T) {
	if got := ShippedActions(); len(got) != 0 {
		t.Fatalf("this build ships %d action(s): %v. Phase 4 builds the gate against a "+
			"null action set; actuations come after it is proven (CLAUDE.md §6).", len(got), got)
	}
}

// TestNothingExecutesWithTheShippedActionSet: even handed the most permissive
// policy imaginable, a real engine can do nothing at all.
func TestNothingExecutesWithTheShippedActionSet(t *testing.T) {
	sink := &memSink{}
	e := New(permissive(), sink, "cluster-a")

	for _, kind := range []Kind{"drain_node", "requeue_job", "rerun_health_check", "test.reversible", ""} {
		d, _ := e.Execute(Request{Kind: kind, Target: Target{Node: "node-0001"}}, when)
		if d.Allowed || d.Executed {
			t.Errorf("%q was allowed=%v executed=%v against the null action set", kind, d.Allowed, d.Executed)
		}
		if d.Gate != GateUnknownAction {
			t.Errorf("%q refused at gate %q, want %q", kind, d.Gate, GateUnknownAction)
		}
	}
}

// ---------- default-deny ----------

// TestDefaultDenyWithKnownAction is the check that would pass for the wrong
// reason if it used an unknown action: the kind here *is* implemented, so only
// the allowlist can be refusing it.
func TestDefaultDenyWithKnownAction(t *testing.T) {
	sink := &memSink{}
	e := newWithActions([]ActionSpec{fakeAction}, Deny(), sink, "cluster-a")

	d, err := e.Execute(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed {
		t.Error("an empty policy allowed a known action")
	}
	if d.Gate != GateNotAllowed {
		t.Errorf("gate = %q, want %q", d.Gate, GateNotAllowed)
	}
}

// TestZeroPolicyIsDenyAll: the zero value, a load failure and an explicit
// "allow nothing" must be indistinguishable. Any arrangement where an error
// yields a more permissive engine than success is the one that eventually
// drains a production node.
func TestZeroPolicyIsDenyAll(t *testing.T) {
	for name, p := range map[string]Policy{
		"zero value": {},
		"Deny()":     Deny(),
		"explicit":   {Version: ConfigVersion, Allow: nil, Nodes: nil, DryRun: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newWithActions([]ActionSpec{fakeAction}, p, &memSink{}, "cluster-a")
			d := e.Evaluate(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
			if d.Allowed {
				t.Errorf("%s permitted an action", name)
			}
		})
	}
}

// TestEmptyScopeIsNotEveryTarget: an empty nodes list must mean "no nodes",
// never "all nodes". The opposite reading is a one-character change away and
// would authorize the whole fleet.
func TestEmptyScopeIsNotEveryTarget(t *testing.T) {
	p := permissive()
	p.Nodes = nil
	p.Jobs = nil
	e := newWithActions([]ActionSpec{fakeAction}, p, &memSink{}, "cluster-a")

	d := e.Evaluate(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if d.Allowed {
		t.Error("an empty scope authorized a target")
	}
	if d.Gate != GateOutOfScope {
		t.Errorf("gate = %q, want %q", d.Gate, GateOutOfScope)
	}
}

func TestScopeMatching(t *testing.T) {
	p := permissive()
	p.Nodes = []string{"node-00*", "special-01"}
	e := newWithActions([]ActionSpec{fakeAction}, p, &memSink{}, "cluster-a")

	for node, want := range map[string]bool{
		"node-0001":  true,
		"special-01": true,
		"node-1234":  false,
		"other-0001": false,
		"":           false,
	} {
		d := e.Evaluate(Request{Kind: fakeAction.Kind, Target: Target{Node: node}}, when)
		if d.Allowed != want {
			t.Errorf("node %q: allowed=%v, want %v (%s)", node, d.Allowed, want, d.Explain)
		}
	}
}

// TestWrongClusterIsRefused: the likeliest way a correct policy file does damage
// is by being copied to another cluster.
func TestWrongClusterIsRefused(t *testing.T) {
	p := permissive()
	p.Cluster = "cluster-a"
	e := newWithActions([]ActionSpec{fakeAction}, p, &memSink{}, "cluster-b")

	d := e.Evaluate(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if d.Allowed {
		t.Error("a policy for cluster-a authorized an action on cluster-b")
	}
	if d.Gate != GateClusterName {
		t.Errorf("gate = %q, want %q", d.Gate, GateClusterName)
	}
}

// TestIrreversibleIsRefused: §6 permits only reversible actuations, so a spec
// that cannot say how it is undone must not run even when fully authorized.
func TestIrreversibleIsRefused(t *testing.T) {
	bad := ActionSpec{Kind: "test.irreversible"}
	if err := bad.Valid(); err == nil {
		t.Error("a spec with no Undo should not validate")
	}
	p := permissive()
	p.Allow = []Kind{bad.Kind}
	e := newWithActions([]ActionSpec{bad}, p, &memSink{}, "cluster-a")

	d := e.Evaluate(Request{Kind: bad.Kind, Target: Target{Node: "node-0001"}}, when)
	if d.Allowed {
		t.Error("an irreversible action was authorized")
	}
	if d.Gate != GateIrreversible {
		t.Errorf("gate = %q, want %q", d.Gate, GateIrreversible)
	}
}

// ---------- the audit log ----------

// TestAuditFailureDenies is the ordering that cannot be retrofitted: no record,
// no action.
func TestAuditFailureDenies(t *testing.T) {
	sink := &failSink{}
	e := newWithActions([]ActionSpec{fakeAction}, permissive(), sink, "cluster-a")

	d, err := e.Execute(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if err == nil {
		t.Fatal("a failing audit sink must produce an error")
	}
	if d.Allowed || d.Executed {
		t.Errorf("acted despite an unwritable audit log: allowed=%v executed=%v", d.Allowed, d.Executed)
	}
	if d.Gate != GateNoAudit {
		t.Errorf("gate = %q, want %q", d.Gate, GateNoAudit)
	}
}

func TestNoSinkDenies(t *testing.T) {
	e := newWithActions([]ActionSpec{fakeAction}, permissive(), nil, "cluster-a")
	d, err := e.Execute(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if err == nil || d.Allowed {
		t.Error("an engine with no audit sink must refuse to act")
	}
}

// TestDenialsAreRecorded: a denied action is the most interesting audit record
// there is, and the easiest one to forget to write.
func TestDenialsAreRecorded(t *testing.T) {
	sink := &memSink{}
	e := newWithActions([]ActionSpec{fakeAction}, Deny(), sink, "cluster-a")

	if _, err := e.Execute(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when); err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 1 {
		t.Fatalf("expected the denial to be recorded, got %d record(s)", len(sink.got))
	}
	if sink.got[0].Allowed {
		t.Error("the recorded decision says allowed")
	}
	if sink.got[0].Gate == "" {
		t.Error("the record does not say which gate refused")
	}
}

// TestEvaluateDoesNotRecord: exploring a policy must not fill the audit log with
// hypotheticals, or the log stops being worth reading.
func TestEvaluateDoesNotRecord(t *testing.T) {
	sink := &memSink{}
	e := newWithActions([]ActionSpec{fakeAction}, permissive(), sink, "cluster-a")
	e.Evaluate(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if len(sink.got) != 0 {
		t.Errorf("Evaluate wrote %d audit record(s); it must write none", len(sink.got))
	}
}

// TestDryRunDecidesWithoutExecuting, and the record must distinguish the two.
func TestDryRunDecidesWithoutExecuting(t *testing.T) {
	p := permissive()
	p.DryRun = true
	sink := &memSink{}
	e := newWithActions([]ActionSpec{fakeAction}, p, sink, "cluster-a")

	d, err := e.Execute(Request{Kind: fakeAction.Kind, Target: Target{Node: "node-0001"}}, when)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatalf("expected the permissive policy to allow: %s", d.Explain)
	}
	if d.Executed {
		t.Error("a dry run executed")
	}
	if !d.DryRun {
		t.Error("the record does not say it was a dry run")
	}
	if !strings.Contains(d.String(), "nothing executed") {
		t.Errorf("operator output does not distinguish a dry run: %q", d.String())
	}
}

func TestAuditLineIsCanonical(t *testing.T) {
	d := Decision{At: when, Allowed: false, Kind: "k", Target: Target{Node: "n"}, Gate: "g", Reason: "r"}
	line := string(d.encode())
	for _, want := range []string{`"at":`, `"allowed":false`, `"executed":false`, `"kind":"k"`, `"gate":"g"`} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line missing %s: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Error("an audit record must be one line")
	}
}

func TestFileSinkAppendsPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "audit.jsonl")
	s, err := NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Record(Decision{At: when, Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1; n != 3 {
		t.Errorf("expected 3 appended lines, got %d", n)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit log is group/world readable: %o", perm)
	}
}

// ---------- policy.yaml ----------

func TestConfigRoundTrip(t *testing.T) {
	p := Policy{
		Version: ConfigVersion, Cluster: "cluster-a",
		Allow: []Kind{"a.b"}, Nodes: []string{"node-00*"}, Jobs: []string{"*"}, DryRun: false,
	}
	first, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(first)
	if err != nil {
		t.Fatalf("cannot read back what we wrote: %v\n%s", err, first)
	}
	second, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("encode->decode->encode is not stable:\n%s\nvs\n%s", first, second)
	}
	if got.DryRun {
		t.Error("dry_run: false did not survive the round trip")
	}
}

// TestDryRunDefaultsTrueWhenAbsent: yamlsub cannot tell "absent" from "false",
// and getting this backwards would make a file that forgot the key execute for
// real.
func TestDryRunDefaultsTrueWhenAbsent(t *testing.T) {
	p, err := Decode([]byte("version: 1\ncluster: cluster-a\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.DryRun {
		t.Error("a policy that omits dry_run must still be a dry run")
	}
	if len(p.Allow) != 0 {
		t.Error("a policy that omits allow must permit nothing")
	}
}

func TestConfigRejectsUnknownKeyAndVersion(t *testing.T) {
	if _, err := Decode([]byte("version: 1\nallwo:\n  - x\n")); err == nil ||
		!strings.Contains(err.Error(), "allwo") {
		t.Errorf("a typo must be rejected by name, got %v", err)
	}
	if _, err := Decode([]byte("version: 99\n")); err == nil ||
		!strings.Contains(err.Error(), "version 99") {
		t.Errorf("a future version must be refused, got %v", err)
	}
}

// TestLoadMissingFileDeniesQuietly: a site that has not opted in is the normal
// case, not an error — but it must yield the deny-all policy.
func TestLoadMissingFileDeniesQuietly(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("a missing policy file must not be an error: %v", err)
	}
	if len(p.Allow) != 0 || !p.DryRun {
		t.Error("a missing policy file must deny everything and stay a dry run")
	}
}

// TestLoadBadFileDenies: a policy that fails to parse must not leave the caller
// with anything more permissive than one that parsed and allowed nothing.
func TestLoadBadFileDenies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("allow: [a, b]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err == nil {
		t.Fatal("expected flow-collection syntax to be refused")
	}
	if len(p.Allow) != 0 || !p.DryRun {
		t.Error("a policy that failed to load must deny everything")
	}
}
