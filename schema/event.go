// Package schema defines cairn's event structure and the closed enum of failure
// classes. It is the most important artifact in the project (CLAUDE.md §4):
// every collector produces these, every consumer reads them, and changing them
// is a versioned migration rather than an edit.
//
// The event is deliberately narrow:
//
//	{ ts, cluster, node, jobid, source, class, detail }
//
// Nothing else belongs on it. Anything that varies per-run rather than per-event
// (clock offsets, redaction mode, collection window) lives on the Bundle header.
package schema

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Version is the schema version. It appears in the bundle header, not on each
// event. Bump it for any change to the event shape or any non-additive change to
// the class enum, and record the migration in CHANGELOG.md (CLAUDE.md §10).
const Version = 1

// Field types that carry identifying information.
//
// These are distinct named types rather than bare strings so that the redaction
// boundary has something the compiler can help enforce. CLAUDE.md §10: "If a code
// path can emit an unredacted hostname, that's a bug, not a configuration
// choice." A bare string makes that bug invisible; a named type makes an
// unredacted emit visible at the signature of every function that handles one.
type (
	// ClusterName identifies a cluster. Site-assigned, so it is identifying.
	ClusterName string
	// Hostname is a node name. Almost always identifying, and almost always
	// encodes site naming conventions even after the domain is stripped.
	Hostname string
	// Username is a local account name.
	Username string
	// AccountCode is a Slurm account, project, or allocation code. Identifying
	// of the institution as well as the user.
	AccountCode string
)

// Source is the closed set of producers cairn reads. It mirrors the collector
// packages in CLAUDE.md §4.
type Source string

const (
	SourceSlurm   Source = "slurm"
	SourceJournal Source = "journal"
	SourceGPU     Source = "gpu"
	SourceFabric  Source = "fabric"
	SourceStorage Source = "storage"
	SourceBMC     Source = "bmc"
)

var allSources = []Source{
	SourceSlurm, SourceJournal, SourceGPU,
	SourceFabric, SourceStorage, SourceBMC,
}

var sourceSet = func() map[Source]struct{} {
	m := make(map[Source]struct{}, len(allSources))
	for _, s := range allSources {
		m[s] = struct{}{}
	}
	return m
}()

// AllSources returns every producer, sorted by wire string.
func AllSources() []Source {
	out := make([]Source, len(allSources))
	copy(out, allSources)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether s is a known producer.
func (s Source) Valid() bool {
	_, ok := sourceSet[s]
	return ok
}

func (s Source) String() string { return string(s) }

// Bounds on Detail. These are limits by construction, not advice: Validate
// rejects anything past them, so a collector cannot grow a detail into a log
// buffer one commit at a time (CLAUDE.md §4).
const (
	MaxAttrs        = 16
	MaxAttrValueLen = 256
	MaxSignatureLen = 128
)

// Detail is the bounded, structured payload of an event.
//
// There is deliberately no free-text field here. Invariant §2.3 is that cairn
// does not store logs; a raw-line field would violate it by accident within a
// month, and would give an unredacted hostname somewhere to hide. Instead:
//
//   - Signature names the *pattern* that matched, never the line that matched it.
//   - Attrs carries extracted, individually-registered values.
type Detail struct {
	// Signature is a stable short identifier for the matched pattern, e.g.
	// "slurm.sacct.state.OUT_OF_MEMORY" or "nvidia.xid.79". It is part of the
	// event's sort key, so it must be stable across runs and across versions of
	// the collector that produces it.
	Signature string

	// Attrs holds extracted values. Keys must be registered for the event's
	// class in attrs.go; unregistered keys are a validation error, which is what
	// keeps this from becoming a dumping ground.
	Attrs map[string]string
}

// Event is the unit of everything cairn produces.
type Event struct {
	// TS is the normalized observation time, always UTC.
	//
	// It is a single instant, not a pair. Clock skew between producers is real
	// and is handled by recording each source's measured offset once on the
	// Bundle header, rather than by widening every event. Keeping the event
	// narrow is what makes the encoding deterministic and the join tractable.
	TS time.Time

	Cluster ClusterName

	// Node may be empty. The join must represent jobid-without-node (a job that
	// never started, or an event from the controller rather than a compute node).
	Node Hostname

	// JobID may be nil. The join must represent node-without-jobid (node health,
	// fabric events, config drift — none of which belong to a job).
	JobID *JobID

	Source Source
	Class  Class
	Detail Detail
}

// ErrInvalidEvent is the sentinel wrapping every Validate failure.
var ErrInvalidEvent = errors.New("schema: invalid event")

// Validate enforces every constraint the schema claims to hold. It is called by
// the canonical encoder, so an invalid event cannot be serialized: the bounds on
// Detail are worth nothing if they are only checked in tests.
func (e Event) Validate() error {
	if e.TS.IsZero() {
		return fmt.Errorf("%w: ts is zero", ErrInvalidEvent)
	}
	if e.TS.Location() != time.UTC {
		return fmt.Errorf("%w: ts must be UTC, got %s", ErrInvalidEvent, e.TS.Location())
	}
	if e.Cluster == "" {
		return fmt.Errorf("%w: cluster is empty", ErrInvalidEvent)
	}
	if !e.Source.Valid() {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidEvent, e.Source)
	}
	if !e.Class.Valid() {
		return fmt.Errorf("%w: unknown class %q", ErrInvalidEvent, e.Class)
	}
	if e.Detail.Signature == "" {
		return fmt.Errorf("%w: detail.signature is empty", ErrInvalidEvent)
	}
	if len(e.Detail.Signature) > MaxSignatureLen {
		return fmt.Errorf("%w: detail.signature exceeds %d bytes", ErrInvalidEvent, MaxSignatureLen)
	}
	if n := len(e.Detail.Attrs); n > MaxAttrs {
		return fmt.Errorf("%w: detail has %d attrs, limit is %d", ErrInvalidEvent, n, MaxAttrs)
	}
	// Sort the keys so the error reported for a multi-key violation is the same
	// on every run. Nondeterministic error text makes failures hard to diff.
	keys := make([]string, 0, len(e.Detail.Attrs))
	for k := range e.Detail.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := e.Detail.Attrs[k]
		if len(v) > MaxAttrValueLen {
			return fmt.Errorf("%w: detail.attrs[%q] is %d bytes, limit is %d",
				ErrInvalidEvent, k, len(v), MaxAttrValueLen)
		}
		if !AttrAllowed(e.Class, k) {
			return fmt.Errorf("%w: detail.attrs[%q] is not registered for class %q (see schema/attrs.go)",
				ErrInvalidEvent, k, e.Class)
		}
	}
	if e.JobID != nil {
		if err := e.JobID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// SortEvents orders events into cairn's canonical total order.
//
// The order is total, not merely deterministic: every tiebreaker is applied, so
// two runs over the same window cannot produce different orderings even when
// timestamps collide (which they do constantly — journald and sacct routinely
// report the same second). Invariant §2.7 depends on this.
func SortEvents(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if !a.TS.Equal(b.TS) {
			return a.TS.Before(b.TS)
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		aj, bj := a.JobID.RawOrEmpty(), b.JobID.RawOrEmpty()
		if aj != bj {
			return aj < bj
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Detail.Signature < b.Detail.Signature
	})
}
