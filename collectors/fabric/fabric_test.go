package fabric_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/fabric"
	"github.com/touchelos/cairn/schema"
)

// TestParseDownPort reads the capture from fixture 005, where the port is down.
//
// Against the real fixture rather than an inlined string: a parser tested only
// against text written by the same person who wrote the parser tests agreement
// with itself.
func TestParseDownPort(t *testing.T) {
	out, err := os.ReadFile("../../fixtures/005-ib-link-flap/input/ibstat.txt")
	if err != nil {
		t.Fatal(err)
	}
	ports, warns := fabric.ParseIbstat(out)
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if len(ports) != 1 {
		t.Fatalf("got %d ports, want 1", len(ports))
	}
	p := ports[0]
	if p.ID() != "mlx5_0:1" {
		t.Errorf("id = %q, want mlx5_0:1", p.ID())
	}
	// State and physical state together are the diagnosis: Down/Polling is a
	// port looking for a peer, which is a cable. Down/Disabled is a person.
	if p.State != "Down" || p.PhysState != "Polling" {
		t.Errorf("state = %q/%q, want Down/Polling", p.State, p.PhysState)
	}
	if p.Healthy() {
		t.Error("a Down port reported itself healthy")
	}
	// Kept as printed. A down port reports the negotiation floor, not the link's
	// real capability, and correcting that in the parser would be inventing.
	if p.Rate != "10" {
		t.Errorf("rate = %q, want the captured 10", p.Rate)
	}
	if got := p.Summary(); got != "Down Polling 10" {
		t.Errorf("summary = %q", got)
	}
}

// TestParseMultiplePorts: two healthy HCAs must come back as two distinct ports,
// because a drift key per port is what lets `cairn diff` name the one that
// diverged rather than saying "the fabric differs".
func TestParseMultiplePorts(t *testing.T) {
	out, err := os.ReadFile("../../site/testdata/slurm-lmod-gpu/input/ibstat.txt")
	if err != nil {
		t.Fatal(err)
	}
	ports, warns := fabric.ParseIbstat(out)
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if len(ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(ports))
	}
	for _, p := range ports {
		if !p.Healthy() {
			t.Errorf("%s: expected Active, got %q", p.ID(), p.State)
		}
		if p.LinkLayer != "InfiniBand" {
			t.Errorf("%s: link layer = %q", p.ID(), p.LinkLayer)
		}
	}
	if ports[0].ID() == ports[1].ID() {
		t.Error("two ports share an id; diff could not tell them apart")
	}
}

// TestParseMalformedWarnsRatherThanPanics is invariant §2.6 at the parser: an
// unrecognized or truncated capture is logged and skipped, never fatal.
func TestParseMalformedInput(t *testing.T) {
	cases := []struct {
		name, in string
		wantWarn string
	}{
		{"empty", "", ""},
		{"garbage", "this is not ibstat output at all\n", ""},
		{
			name:     "port before any CA",
			in:       "Port 1:\n\tState: Active\n",
			wantWarn: "truncated",
		},
		{
			name:     "port with no state",
			in:       "CA 'mlx5_0'\n\tPort 1:\n\t\tRate: 200\n",
			wantWarn: "no state",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ports, warns := fabric.ParseIbstat([]byte(c.in))
			joined := strings.Join(warns, " ")
			if c.wantWarn == "" {
				if len(ports) != 0 {
					t.Errorf("expected no ports from %q, got %d", c.name, len(ports))
				}
				return
			}
			if !strings.Contains(joined, c.wantWarn) {
				t.Errorf("warnings %v do not mention %q", warns, c.wantWarn)
			}
		})
	}
}

// TestCollectorEmitsNoEvents pins the design decision so that changing it is
// deliberate.
//
// ibstat carries no timestamp, and cairn's rule — set by collectors/gpu — is
// that a producer with no time of its own emits nothing, because inventing one
// from the wall clock breaks §2.7. The timestamped fabric evidence reaches cairn
// through journald instead. If this test starts failing, read
// collectors/fabric's package doc before making it pass.
func TestCollectorEmitsNoEvents(t *testing.T) {
	env := collectors.NewFixtureEnv("../../fixtures/005-ib-link-flap")
	env.Host = "node-0045"

	res := fabric.New().Collect(context.Background(), env,
		collectors.Request{Cluster: "cluster-a", Nodes: []schema.Hostname{"node-0045"}})

	if len(res.Events) != 0 {
		t.Errorf("the fabric collector emitted %d event(s); it must emit none "+
			"(see the package doc)", len(res.Events))
	}
	if res.Source != schema.SourceFabric {
		t.Errorf("source = %q", res.Source)
	}
	// Emitting nothing is not the same as seeing nothing, and doctor has to be
	// able to tell those apart.
	if !res.OK() {
		t.Errorf("ibstat was readable but the capability was reported unavailable: %v", res.Missing())
	}
	if !strings.Contains(res.Capabilities[0].Detail, "mlx5_0:1 not Active") {
		t.Errorf("a down port should be surfaced on the capability line, got %q",
			res.Capabilities[0].Detail)
	}
}

// TestCollectorReportsAnAbsentFabric: "no InfiniBand here" is the correct and
// complete answer on most hosts, and must not look like a failure (§2.6).
func TestCollectorReportsAnAbsentFabric(t *testing.T) {
	env := collectors.NewFixtureEnv("../../fixtures/001-oom-cgroup") // no ibstat.txt
	res := fabric.New().Collect(context.Background(), env, collectors.Request{Cluster: "cluster-a"})

	if len(res.Events) != 0 {
		t.Errorf("emitted events with no fabric present")
	}
	if res.OK() {
		t.Error("an absent ibstat should be reported as an unavailable capability")
	}
	m := res.Missing()
	if len(m) != 1 || !strings.Contains(m[0].Detail, "not present") {
		t.Errorf("missing capability is unclear: %+v", m)
	}
	if m[0].Reveals == "" {
		t.Error("a missing capability must say what it costs")
	}
}
