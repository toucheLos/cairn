package site

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

var update = flag.Bool("update", false, "regenerate golden files")

// fixtures is every probe fixture, and the cluster name to fall back to when
// discovery cannot find one.
var fixtures = []struct {
	dir      string
	fallback schema.ClusterName
}{
	{"slurm-lmod-gpu", "unused-cluster-a-comes-from-slurm-conf"},
	{"cpu-only", "cluster-b"},
	{"unknown-scheduler", "cluster-c"},
}

func discover(t *testing.T, dir string, fallback schema.ClusterName) Profile {
	t.Helper()
	env := collectors.NewFixtureEnv(filepath.Join("testdata", dir))
	env.Host = "node-0046"
	return Discover(context.Background(), env, fallback)
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run `go test ./site -update` to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// TestDiscoverGolden pins the profile each fixture produces.
//
// Committed goldens make a change to a probe visible as a diff to be read,
// rather than as a test that quietly still passes because it only asserted the
// parts nobody changed.
func TestDiscoverGolden(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.dir, func(t *testing.T) {
			data, err := discover(t, f.dir, f.fallback).EncodeYAML()
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, f.dir+".golden.yaml", data)
		})
	}
}

// TestDiscoverDeterministic is invariant §2.7 for discovery.
//
// Two probes of one fixture must be byte-identical. Discovery walks maps —
// mounts, environment variables, capability lists — and Go randomizes map
// iteration, so this is the test that would actually catch a missing sort.
func TestDiscoverDeterministic(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.dir, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				a, err := discover(t, f.dir, f.fallback).EncodeYAML()
				if err != nil {
					t.Fatal(err)
				}
				b, err := discover(t, f.dir, f.fallback).EncodeYAML()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(a, b) {
					t.Fatalf("run %d differs from its repeat:\n%s\nvs\n%s", i, a, b)
				}
			}
		})
	}
}

// TestUnknownStackDoesNotFail is invariant §2.6.
//
// A site running a scheduler cairn has never parsed, on a distro it does not
// know, must still produce a usable profile. The failure mode this guards
// against is a discovery pass that returns an error and leaves the admin with
// nothing to correct.
func TestUnknownStackDoesNotFail(t *testing.T) {
	p := discover(t, "unknown-scheduler", "cluster-c")
	if p.Cluster != "cluster-c" {
		t.Errorf("cluster = %q, want the fallback cluster-c", p.Cluster)
	}
	if p.Scheduler.Kind != "pbs" {
		t.Errorf("scheduler kind = %q, want pbs", p.Scheduler.Kind)
	}
	data, err := p.EncodeYAML()
	if err != nil {
		t.Fatalf("an unknown stack must still encode: %v", err)
	}
	if _, err := DecodeYAML(data); err != nil {
		t.Fatalf("an unknown stack must still decode: %v", err)
	}
	// The profile must say cairn cannot read this scheduler, not imply it did.
	var found bool
	for _, pr := range p.Probes {
		if pr.Name == "scheduler" && strings.Contains(pr.Detail, "no collector") {
			found = true
		}
	}
	if !found {
		t.Error("a scheduler with no collector must say so in its probe detail")
	}
}

// TestRoundTrip is the property that makes site.yaml safe to hand to an admin:
// what cairn writes, cairn reads back to the same thing.
func TestRoundTrip(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.dir, func(t *testing.T) {
			first, err := discover(t, f.dir, f.fallback).EncodeYAML()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeYAML(first)
			if err != nil {
				t.Fatalf("decoding what we just wrote: %v", err)
			}
			second, err := decoded.EncodeYAML()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("encode->decode->encode is not stable:\n%s\nvs\n%s", first, second)
			}
		})
	}
}

// TestDecodeRejectsUnknownKeys is schema/DESIGN.md §8's rule applied to a file
// people hand-edit, which is where it matters most: `partitons:` must be an
// error, not a silently ignored line that leaves the tool and the file
// disagreeing about the site.
func TestDecodeRejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{
			name: "top level",
			yaml: "version: 1\ncluster: cluster-a\nschedular:\n  kind: slurm\n",
			want: `unknown key "schedular"`,
		},
		{
			name: "nested",
			yaml: "version: 1\ncluster: cluster-a\nscheduler:\n  kind: slurm\n  partitons:\n    - batch\n",
			want: `unknown key "partitons"`,
		},
		{
			name: "inside a list item",
			yaml: "version: 1\ncluster: cluster-a\nmounts:\n  - mountpoint: /scratch\n    fstyp: lustre\n",
			want: `unknown key "fstyp"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeYAML([]byte(c.yaml))
			if err == nil {
				t.Fatalf("expected an error naming the unknown key, got none")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %s", err, c.want)
			}
		})
	}
}

// TestDecodeRejectsUnsupportedYAML checks that the constructs this subset does
// not model are refused rather than misread. Accepting a flow collection and
// silently treating it as a string would be worse than failing.
func TestDecodeRejectsUnsupportedYAML(t *testing.T) {
	base := "version: 1\ncluster: cluster-a\n"
	cases := []struct{ name, yaml, want string }{
		{"tabs", base + "scheduler:\n\tkind: slurm\n", "tab character"},
		{"flow collection", base + "scheduler:\n  partitions: [batch, gpu]\n", "flow collections"},
		{"anchor", base + "scheduler: &sched\n  kind: slurm\n", "anchors"},
		{"multi document", base + "---\ncluster: cluster-b\n", "multiple documents"},
		{"odd indent", base + "scheduler:\n   kind: slurm\n", "two-space indentation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeYAML([]byte(c.yaml))
			if err == nil {
				t.Fatalf("expected %s to be rejected", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not explain %q", err, c.want)
			}
		})
	}
}

// TestDecodeRequiresMatchingVersion: a profile from a newer cairn must fail
// loudly rather than be read with fields silently missing.
func TestDecodeVersionMismatch(t *testing.T) {
	_, err := DecodeYAML([]byte("version: 99\ncluster: cluster-a\n"))
	if err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Errorf("expected a version mismatch error, got %v", err)
	}
}

// ---------- node profiles and diff ----------

func loadProfiles(t *testing.T, dir string) []NodeProfile {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []NodeProfile
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		np, err := DecodeNodeJSON(data)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		out = append(out, np)
	}
	return out
}

func TestCaptureNodeDeterministic(t *testing.T) {
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	capture := func() []byte {
		env := collectors.NewFixtureEnv(filepath.Join("testdata", "slurm-lmod-gpu"))
		env.Host = "node-0046"
		np := CaptureNode(context.Background(), env, "cluster-a", "node-0046", at)
		data, err := np.EncodeJSON()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := capture()
	for i := 0; i < 20; i++ {
		if !bytes.Equal(first, capture()) {
			t.Fatalf("capture %d differs from the first", i)
		}
	}
	checkGolden(t, "node-profile.golden.json", first)
}

// TestCaptureNodeDropsPerNodeCmdline: root= and rd.* legitimately differ on
// every machine. Comparing them would report drift on every node against every
// other node and bury the findings that matter.
func TestNormalizeCmdline(t *testing.T) {
	got := normalizeCmdline("BOOT_IMAGE=/vmlinuz-5.14 root=/dev/mapper/rl-root ro rd.lvm.lv=rl/root intel_iommu=on")
	want := "BOOT_IMAGE=/vmlinuz-5.14 intel_iommu=on"
	if got != want {
		t.Errorf("normalizeCmdline = %q, want %q", got, want)
	}
	// Order must not matter.
	a := normalizeCmdline("iommu=pt intel_iommu=on")
	b := normalizeCmdline("intel_iommu=on iommu=pt")
	if a != b {
		t.Errorf("cmdline order changed the result: %q vs %q", a, b)
	}
}

// TestCompareFindsDrift is the §7 headline: a node is interesting when it
// differs from its siblings, not when it crosses a threshold.
func TestCompareFindsDrift(t *testing.T) {
	ps := loadProfiles(t, "profiles")
	target, ok := find(ps, "node-0046")
	if !ok {
		t.Fatal("fixture node-0046 missing")
	}
	res := Compare(target, ps)
	if res.Refused != "" {
		t.Fatalf("unexpected refusal: %s", res.Refused)
	}
	if len(res.Peers) != 4 {
		t.Errorf("peers = %d, want 4 (the target must not be its own sibling)", len(res.Peers))
	}

	got := map[string]Drift{}
	for _, d := range res.Drifts {
		got[d.Key] = d
	}
	for _, key := range []string{KeyDriverVersion, KeyCUDAVersion, KeyMungeKeyMtime} {
		d, ok := got[key]
		if !ok {
			t.Errorf("expected drift on %s, found none", key)
			continue
		}
		if d.PeerCount != 4 {
			t.Errorf("%s: peer_count = %d, want 4", key, d.PeerCount)
		}
		if d.PeerMajority*2 <= d.PeerCount {
			t.Errorf("%s: majority of %d is not strict over %d peers", key, d.PeerMajority, d.PeerCount)
		}
	}
	if d, ok := got[KeyDriverVersion]; ok {
		if d.Observed != "535.104.05" || d.Expected != "550.54.14" {
			t.Errorf("driver drift = %q vs %q, want 535.104.05 vs 550.54.14", d.Observed, d.Expected)
		}
	}
	// The uniform keys must not be reported.
	for _, key := range []string{KeyKernelRelease, KeyGlibcVersion, KeyOSID} {
		if _, ok := got[key]; ok {
			t.Errorf("%s is uniform across the fleet and must not be reported as drift", key)
		}
	}
}

// TestCompareTreatsAbsenceAsDrift: the node that lost /scratch is exactly the
// node whose jobs are failing, so a missing key must not be silently excluded.
func TestCompareTreatsAbsenceAsDrift(t *testing.T) {
	ps := loadProfiles(t, "profiles")
	target, _ := find(ps, "node-0049")
	res := Compare(target, ps)

	var found bool
	for _, d := range res.Drifts {
		if d.Key == KeyMountPrefix+"/scratch" {
			found = true
			if d.Observed != Absent {
				t.Errorf("observed = %q, want %q", d.Observed, Absent)
			}
		}
	}
	if !found {
		t.Error("a node missing /scratch must be reported as drifted, not skipped")
	}
}

// TestCompareRefusesBelowMinPeers: a majority of two is not a fleet norm, and
// a confident claim derived from one is worse than no claim.
func TestCompareRefusesBelowMinPeers(t *testing.T) {
	ps := loadProfiles(t, "profiles-few")
	target, ok := find(ps, "node-0046")
	if !ok {
		t.Fatal("fixture node-0046 missing")
	}
	res := Compare(target, ps)
	if res.Refused == "" {
		t.Fatal("expected a refusal with only one sibling")
	}
	if len(res.Drifts) != 0 {
		t.Errorf("a refused comparison must report no drift, got %d", len(res.Drifts))
	}
	if !strings.Contains(res.Refused, "majority") {
		t.Errorf("the refusal must say why, got %q", res.Refused)
	}
}

// TestCompareReportsUndecided: when the fleet does not agree with itself there
// is no norm to diverge from, and naming the largest group "expected" would
// invent one.
func TestCompareReportsUndecided(t *testing.T) {
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	mk := func(node, driver string) NodeProfile {
		return NodeProfile{
			Version: ProfileVersion, Cluster: "cluster-a", Node: schema.Hostname(node),
			CapturedAt: at, Keys: map[string]string{KeyDriverVersion: driver},
		}
	}
	// Four siblings, evenly split two and two: no strict majority.
	peers := []NodeProfile{
		mk("node-0002", "550.54.14"), mk("node-0003", "550.54.14"),
		mk("node-0004", "535.104.05"), mk("node-0005", "535.104.05"),
	}
	res := Compare(mk("node-0001", "525.60.13"), peers)
	if len(res.Drifts) != 0 {
		t.Errorf("a split fleet has no norm; expected no drift, got %v", res.Drifts)
	}
	if len(res.Undecided) != 1 || res.Undecided[0] != KeyDriverVersion {
		t.Errorf("undecided = %v, want [%s]", res.Undecided, KeyDriverVersion)
	}
}

// TestDriftEventsAreValid proves the drift path produces events the schema
// accepts — including that every attr key is registered for config.drift.
func TestDriftEventsAreValid(t *testing.T) {
	ps := loadProfiles(t, "profiles")
	target, _ := find(ps, "node-0046")
	evs, err := Compare(target, ps).Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("expected drift events")
	}
	for _, e := range evs {
		if err := e.Validate(); err != nil {
			t.Errorf("invalid drift event: %v", err)
		}
		if e.Source != schema.SourceSite {
			t.Errorf("source = %q, want %q", e.Source, schema.SourceSite)
		}
		if e.Class != schema.ClassConfigDrift {
			t.Errorf("class = %q, want %q", e.Class, schema.ClassConfigDrift)
		}
		for k := range e.Detail.Attrs {
			if !schema.AttrAllowed(e.Class, k) {
				t.Errorf("attr %q is not registered for %s", k, e.Class)
			}
		}
	}
	// A bundle built from them must encode, which is what `--format json` does.
	b := schema.Bundle{
		Cluster: "cluster-a", Window: schema.Window{Start: evs[0].TS, End: evs[0].TS},
		Redaction: schema.Redaction{Mode: "none"}, Events: evs,
	}
	if _, err := b.Encode(); err != nil {
		t.Errorf("drift bundle does not encode: %v", err)
	}
}

// TestDriftEventsDeterministic: two comparisons of the same profiles must
// produce byte-identical bundles, or diffing two drift reports is meaningless.
func TestDriftEventsDeterministic(t *testing.T) {
	ps := loadProfiles(t, "profiles")
	target, _ := find(ps, "node-0046")
	encode := func() []byte {
		evs, err := Compare(target, ps).Events()
		if err != nil {
			t.Fatal(err)
		}
		b := schema.Bundle{
			Cluster: "cluster-a", Window: schema.Window{Start: evs[0].TS, End: evs[0].TS},
			Redaction: schema.Redaction{Mode: "none"}, Events: evs,
		}
		data, err := b.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := encode()
	for i := 0; i < 20; i++ {
		if !bytes.Equal(first, encode()) {
			t.Fatalf("drift bundle %d differs from the first", i)
		}
	}
}

func TestNodeProfileRejectsUnknownFields(t *testing.T) {
	_, err := DecodeNodeJSON([]byte(
		`{"profile_version":1,"cluster":"c","node":"n","captured_at":"2026-03-04T09:00:00.000000000Z","keys":{},"probes":[],"extra":1}`))
	if err == nil {
		t.Fatal("expected an unknown field to be rejected")
	}
}

// ---------- sets ----------

func TestLoadSetRejectsDuplicateClusters(t *testing.T) {
	dir := t.TempDir()
	body := "version: 1\ncluster: cluster-a\nscheduler:\n  kind: slurm\n"
	for _, name := range []string{"one.yaml", "two.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Errorf("two profiles claiming one cluster must be an error, got %v", err)
	}
}

func TestLoadSetMultiCluster(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		body := "version: 1\ncluster: cluster-" + name + "\nscheduler:\n  kind: slurm\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Profiles) != 3 {
		t.Fatalf("loaded %d profiles, want 3", len(set.Profiles))
	}
	if _, ok := set.Only(); ok {
		t.Error("a three-cluster set must not resolve to a single profile")
	}
	if _, ok := set.Find("cluster-b"); !ok {
		t.Error("cluster-b should be findable by name")
	}
	names := set.Names()
	if names[0] != "cluster-a" || names[2] != "cluster-c" {
		t.Errorf("names are not sorted: %v", names)
	}
}

func find(ps []NodeProfile, node schema.Hostname) (NodeProfile, bool) {
	for _, p := range ps {
		if p.Node == node {
			return p, true
		}
	}
	return NodeProfile{}, false
}

// TestGoldensDecode reads each committed site.yaml back.
//
// Distinct from TestDiscoverGolden, which compares bytes: this one exercises the
// decode path against the exact files in the repo, so a golden that encodes
// cleanly but cannot be parsed fails here rather than in front of an admin.
//
// It is also what scripts/verify-guards.sh points at when it proves the
// unknown-key and version-mismatch rejections actually fire. A guard aimed at a
// byte-comparison test would pass for the wrong reason — any mutation fails a
// golden diff — and so would prove nothing about the rejection it names.
func TestGoldensDecode(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.dir, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", f.dir+".golden.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			p, err := DecodeYAML(data)
			if err != nil {
				t.Fatalf("committed golden does not decode: %v", err)
			}
			if p.Cluster == "" {
				t.Error("decoded profile has no cluster name")
			}
			if p.Version != ProfileVersion {
				t.Errorf("version = %d, want %d", p.Version, ProfileVersion)
			}
		})
	}
}

// TestFabricPortDriftIsReported is the test that says the fabric design works.
//
// ibstat carries no timestamp, so collectors/fabric emits no events and the port
// snapshot becomes a drift key instead. That trade is only worth making if the
// signal actually comes out the other end: a node whose InfiniBand port is Down
// while its siblings are Active must be reported, and must be named down to the
// port.
func TestFabricPortDriftIsReported(t *testing.T) {
	ps := loadProfiles(t, "profiles")
	target, ok := find(ps, "node-0046")
	if !ok {
		t.Fatal("fixture node-0046 missing")
	}
	res := Compare(target, ps)
	if res.Refused != "" {
		t.Fatalf("unexpected refusal: %s", res.Refused)
	}

	key := KeyFabricPortPrefix + "mlx5_0:1"
	var found bool
	for _, d := range res.Drifts {
		if d.Key != key {
			continue
		}
		found = true
		if !strings.Contains(d.Observed, "Down") {
			t.Errorf("observed = %q, want the down state", d.Observed)
		}
		if !strings.Contains(d.Expected, "Active") {
			t.Errorf("expected = %q, want the sibling majority's Active", d.Expected)
		}
		// Named down to the port, not "the fabric differs": an operator has to
		// know which HCA to look at.
		if !strings.Contains(d.Key, "mlx5_0") {
			t.Errorf("the drift key does not name the device: %q", d.Key)
		}
	}
	if !found {
		t.Errorf("a node with a Down IB port was not reported as drifted; drifts were %v",
			driftKeys(res.Drifts))
	}

	// And it must reach the event stream, so a drift report can be attached to a
	// ticket like any other evidence.
	evs, err := res.Events()
	if err != nil {
		t.Fatal(err)
	}
	var inEvents bool
	for _, e := range evs {
		if e.Detail.Attrs["key"] == key {
			inEvents = true
		}
	}
	if !inEvents {
		t.Error("the fabric drift did not reach the event stream")
	}
}

func driftKeys(ds []Drift) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Key
	}
	return out
}
