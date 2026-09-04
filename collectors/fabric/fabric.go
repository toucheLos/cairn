// Package fabric reads the InfiniBand stack.
//
// # Why this collector emits no events
//
// It is not unfinished. `ibstat` prints a snapshot — device, port, link state,
// physical state, rate — and **no timestamp, ever**. cairn's rule for a producer
// with no time of its own is set by collectors/gpu: warn, and emit nothing,
// because inventing one from the wall clock breaks invariant §2.7. nvidia-smi at
// least prints a header time and the missing case is an anomaly; for ibstat it
// is every case, so applying the same rule honestly means no events at all.
//
// Stamping the snapshot with the collection window's end was the tempting
// alternative and is worse than it looks. Live, that window ends when the job
// did — often hours ago — so a port that is down *now* would be placed in the
// middle of a past incident, which is not merely imprecise but actively
// misleading.
//
// The fabric evidence that *is* timestamped already reaches cairn: the mlx5
// kernel messages in journald carry a time, and collectors/journal emits them as
// fabric.link_flap. Nothing is lost by this collector staying quiet.
//
// What the snapshot *is* good for is state. A port that is Down while its 47
// siblings are Active is exactly the fleet-relative signal CLAUDE.md §7 asks
// for, and Phase 3 already built the machinery: site.CaptureNode records port
// state as a drift key and `cairn diff` reports the divergence, using
// NodeProfile.CapturedAt so nothing has to be invented. ParseIbstat below is
// shared with site/ for precisely that.
//
// So this collector exists to answer one question `doctor` could not otherwise
// answer: can cairn see the fabric at all? Before it, an unreadable fabric and
// an unimplemented one looked identical, and those call for opposite responses.
package fabric

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

// Port is one port of one HCA, as ibstat reported it.
type Port struct {
	// Device is the HCA name, e.g. mlx5_0.
	Device string
	// Number is the port number within the device.
	Number string
	// State is the logical link state: Active, Down, Initializing.
	State string
	// PhysState is the physical state: LinkUp, Polling, Disabled.
	//
	// Worth keeping alongside State because the pair distinguishes causes that
	// matter: Down/Polling is a port looking for a peer — a pulled or failing
	// cable — while Down/Disabled was switched off by somebody.
	PhysState string
	// Rate is as printed, with no unit assumed. ibstat prints a bare number on
	// some versions and "200 Gb/sec" on others, and a down port reports the
	// negotiation floor rather than the link's real capability.
	Rate string
	// LinkLayer is "InfiniBand" or "Ethernet". A Mellanox card in Ethernet mode
	// is RoCE, and the difference decides whether perfquery means anything here.
	LinkLayer string
}

// ID is the stable identifier for a port: "mlx5_0:1".
func (p Port) ID() string { return p.Device + ":" + p.Number }

// Healthy reports whether the port is carrying traffic.
func (p Port) Healthy() bool { return strings.EqualFold(p.State, "Active") }

// Summary renders the port's state compactly, for a drift key's value.
func (p Port) Summary() string {
	s := strings.TrimSpace(p.State + " " + p.PhysState)
	if p.Rate != "" {
		s += " " + p.Rate
	}
	return s
}

// ParseIbstat reads `ibstat` output into per-port state.
//
// The single ibstat parser in this repo. site/discover.go had its own until this
// package existed, and one parser used by both the collector and the site
// profile means a fix to either lands in both — the nvidia-smi header patterns
// are duplicated across two packages and that is a mistake, not a precedent.
//
// Unrecognized lines are ignored rather than warned about individually. ibstat
// prints a dozen fields per port that cairn has no use for, and warning on each
// would reproduce the 353,526-warnings bug §5 records (CLAUDE.md).
func ParseIbstat(out []byte) ([]Port, []string) {
	var (
		ports    []Port
		warnings []string
		device   string
		cur      *Port
	)

	flush := func() {
		if cur != nil {
			ports = append(ports, *cur)
			cur = nil
		}
	}

	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "CA '"):
			flush()
			device = strings.Trim(strings.TrimPrefix(line, "CA "), "'")

		case strings.HasPrefix(line, "Port ") && strings.HasSuffix(line, ":"):
			flush()
			num := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "Port ")), ":")
			if device == "" {
				// A port before any CA line means the capture is truncated or
				// from a tool cairn does not recognize. Recorded once, and
				// parsing continues (§2.6).
				warnings = append(warnings,
					"ibstat: a port appeared before any CA line; the capture may be truncated")
			}
			cur = &Port{Device: device, Number: num}

		case cur != nil && strings.HasPrefix(line, "State:"):
			cur.State = value(line, "State:")
		case cur != nil && strings.HasPrefix(line, "Physical state:"):
			cur.PhysState = value(line, "Physical state:")
		case cur != nil && strings.HasPrefix(line, "Rate:"):
			cur.Rate = value(line, "Rate:")
		case cur != nil && strings.HasPrefix(line, "Link layer:"):
			cur.LinkLayer = value(line, "Link layer:")
		}
	}
	flush()

	for _, p := range ports {
		if p.State == "" {
			warnings = append(warnings, fmt.Sprintf(
				"ibstat: port %s reported no state; cairn cannot tell whether it is up", p.ID()))
		}
	}
	return ports, warnings
}

func value(line, prefix string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, prefix))
}

// Collector reads the fabric. It reports what it can see and emits no events;
// see the package doc for why that is the design rather than a gap.
type Collector struct{}

func New() *Collector { return &Collector{} }

func (c *Collector) Source() schema.Source { return schema.SourceFabric }

func (c *Collector) Collect(ctx context.Context, env collectors.Env, req collectors.Request) collectors.Result {
	res := collectors.Result{Source: schema.SourceFabric}

	cap := collectors.Capability{
		Name:  "ibstat",
		Level: collectors.LevelUnprivileged,
		Reveals: "whether this host's InfiniBand ports are up, at what rate, and " +
			"whether the card is in Ethernet mode — the state `cairn diff` compares against a node's siblings",
	}

	out, err := env.Run(ctx, "ibstat")
	if err != nil {
		if errors.Is(err, collectors.ErrNotFound) {
			cap.Detail = "ibstat not present; this host has no InfiniBand tooling, or the fabric is Ethernet"
		} else {
			cap.Detail = err.Error()
		}
		res.Capabilities = append(res.Capabilities, cap)
		return res
	}

	ports, warns := ParseIbstat(out)
	cap.Available = true
	cap.Detail = fmt.Sprintf("%d port(s)", len(ports))
	if down := downPorts(ports); len(down) > 0 {
		// Surfaced on the capability line because it is the one thing an
		// operator running `doctor` on a suspect node wants to see immediately,
		// and this collector has no event to put it in.
		cap.Detail += fmt.Sprintf("; %s not Active", strings.Join(down, " "))
	}
	res.Capabilities = append(res.Capabilities, cap)
	res.Warnings = append(res.Warnings, warns...)

	// No events, deliberately. See the package doc: ibstat carries no timestamp,
	// the timestamped fabric evidence reaches cairn through journald, and the
	// state this reads is compared by `cairn diff` rather than placed on a
	// timeline. A test asserts this stays true, so changing it is deliberate.
	return res
}

func downPorts(ports []Port) []string {
	var out []string
	for _, p := range ports {
		if !p.Healthy() {
			out = append(out, p.ID())
		}
	}
	return out
}
