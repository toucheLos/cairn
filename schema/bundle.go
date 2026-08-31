package schema

import (
	"bytes"
	"fmt"
	"sort"
	"time"
)

// ClockOffset records one producer's measured clock offset relative to the
// collection host.
//
// Clock skew is recorded once per (source, node) on the bundle header rather
// than per event. Putting it on the event would widen the hottest struct in the
// project to carry a value that changes a handful of times per run, and would
// invite collectors to store an unnormalized timestamp "just in case" — after
// which the join would have two notions of when something happened.
type ClockOffset struct {
	Source Source
	Node   Hostname

	// OffsetNanos is producer_clock - collector_clock. Positive means the
	// producer's clock runs ahead.
	OffsetNanos int64

	// Method records how the offset was determined, e.g. "journal_realtime_delta"
	// or "assumed_zero". "assumed_zero" is not a failure — most single-node
	// collection has no skew to measure — but it must be visible rather than
	// implied, because a wrong assumption here silently misorders the join.
	Method string
}

// Window is the closed time range a bundle covers.
type Window struct {
	Start time.Time
	End   time.Time
}

// Redaction records how a bundle was redacted, without recording anything that
// would let it be un-redacted.
type Redaction struct {
	// Mode is "none" or "pseudonymize".
	Mode string

	// SaltID identifies the salt used for pseudonymization — a fingerprint of
	// the salt, never the salt itself. Two bundles sharing a SaltID map the same
	// hostname to the same pseudonym and can therefore be compared; a bundle
	// leaving the site carries the identifier so that correlation stays possible
	// without the original names being recoverable.
	SaltID string
}

// Bundle is a complete, self-describing collection result: the unit that gets
// attached to a ticket and replayed offline (CLAUDE.md §7).
type Bundle struct {
	Cluster   ClusterName
	Window    Window
	Clocks    []ClockOffset
	Redaction Redaction
	Events    []Event
}

// Encode renders the bundle in canonical form.
//
// Note what is absent: there is no "generated_at" field. A wall-clock stamp
// would make every bundle differ from every other bundle covering the same
// window, which would break invariant §2.7 outright — and §2.7 exists so that
// `cairn diff` and the eval harness can compare two runs at all. Collection time
// is metadata about the run, not about the evidence; if it is ever needed it
// belongs beside the bundle, not inside it.
func (b Bundle) Encode() ([]byte, error) {
	if b.Cluster == "" {
		return nil, fmt.Errorf("%w: bundle cluster is empty", ErrInvalidEvent)
	}
	if b.Window.Start.After(b.Window.End) {
		return nil, fmt.Errorf("%w: bundle window starts after it ends", ErrInvalidEvent)
	}
	switch b.Redaction.Mode {
	case "none", "pseudonymize":
	default:
		return nil, fmt.Errorf("%w: unknown redaction mode %q", ErrInvalidEvent, b.Redaction.Mode)
	}

	// Sort a copy: the caller's slice order is not the caller's business to get
	// right, and canonical order is part of canonical form.
	clocks := make([]ClockOffset, len(b.Clocks))
	copy(clocks, b.Clocks)
	sort.SliceStable(clocks, func(i, j int) bool {
		if clocks[i].Source != clocks[j].Source {
			return clocks[i].Source < clocks[j].Source
		}
		return clocks[i].Node < clocks[j].Node
	})

	var cb bytes.Buffer
	cb.WriteString("[")
	for i, c := range clocks {
		if i > 0 {
			cb.WriteByte(',')
		}
		var node []byte
		if c.Node != "" {
			node = jstr(string(c.Node))
		}
		cb.Write(jobj(
			jfield{"source", jstr(string(c.Source))},
			jfield{"node", node},
			jfield{"offset_nanos", jint(c.OffsetNanos)},
			jfield{"method", jstr(c.Method)},
		))
	}
	cb.WriteString("]")

	events, err := EncodeEvents(b.Events)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(`{"schema_version":`)
	out.Write(juint(uint64(Version)))
	out.WriteString(`,"cluster":`)
	out.Write(jstr(string(b.Cluster)))
	out.WriteString(`,"window":`)
	out.Write(jobj(
		jfield{"start", jstr(FormatTime(b.Window.Start))},
		jfield{"end", jstr(FormatTime(b.Window.End))},
	))
	out.WriteString(`,"clocks":`)
	out.Write(cb.Bytes())
	out.WriteString(`,"redaction":`)
	out.Write(jobj(
		jfield{"mode", jstr(b.Redaction.Mode)},
		jfield{"salt_id", jstr(b.Redaction.SaltID)},
	))
	out.WriteString(",\"events\":")
	out.Write(events)
	out.WriteString("}\n")
	return out.Bytes(), nil
}
