package schema

import (
	"fmt"
	"sort"
)

// Class is the closed enum of failure classes cairn will emit.
//
// The wire representation is the string, never an ordinal. Reordering the
// constants below is therefore harmless; renaming or removing one is a breaking
// change that requires a schema version bump (CLAUDE.md §4).
//
// A Class names what an event *evidences*, not what caused a job to die. See
// DESIGN.md — this distinction is what keeps the enum closable.
type Class string

const (
	// ClassUnknown is emitted when a producer yields an event cairn recognizes
	// as significant but cannot classify. Invariant §2.6 forbids hard-failing on
	// an unknown stack, so there must always be somewhere for such events to go.
	ClassUnknown Class = "unknown"

	// Application-level outcomes. These exist so that an ordinary failing job
	// does not silently become ClassUnknown and drown the signal.
	ClassAppNonzeroExit Class = "app.nonzero_exit"
	ClassAppSegfault    Class = "app.segfault"

	// Resource exhaustion.
	//
	// Host/cgroup OOM and CUDA OOM are separate members because the remediation
	// differs entirely: one is --mem, the other is batch size or model sharding.
	ClassResourceOOM              Class = "resource.oom"
	ClassResourceOOMGPU           Class = "resource.oom_gpu"
	ClassResourceWalltimeExceeded Class = "resource.walltime_exceeded"
	ClassResourceDiskQuota        Class = "resource.disk_quota"

	// Scheduler-observed job and node state transitions.
	ClassSchedNodeFail  Class = "sched.node_fail"
	ClassSchedPreempted Class = "sched.preempted"
	ClassSchedCancelled Class = "sched.cancelled"
	ClassSchedRequeued  Class = "sched.requeued"

	// Authentication. Munge gets its own member because a Munge failure has a
	// specific, recognizable remediation (clock skew or key distribution) that
	// generic credential failure does not.
	ClassAuthMunge      Class = "auth.munge"
	ClassAuthCredential Class = "auth.credential"

	// Fabric.
	ClassFabricLinkFlap    Class = "fabric.link_flap"
	ClassFabricCongestion  Class = "fabric.congestion"
	ClassFabricNCCLTimeout Class = "fabric.nccl_timeout"

	// GPU. ClassGPUXid earns a member of its own because Xid is by volume the
	// single most common real-world GPU signal, and the Xid number itself
	// carries most of the diagnostic content.
	ClassGPUDriverMismatch Class = "gpu.driver_mismatch"
	ClassGPUXid            Class = "gpu.xid"
	ClassGPUECC            Class = "gpu.ecc"
	ClassGPUFallenOffBus   Class = "gpu.fallen_off_bus"

	// Storage.
	ClassStorageMountMissing Class = "storage.mount_missing"
	ClassStorageIOError      Class = "storage.io_error"
	ClassStorageStaleHandle  Class = "storage.stale_handle"

	// Divergence of a node from its fleet siblings or from declared intent
	// (CLAUDE.md §7). The family covers node-level state that is supposed to
	// match something — the other 47 nodes, or the site's own configuration.
	ClassConfigDrift Class = "config.drift"

	// ClassConfigClockSkew is a host clock that disagrees with the fleet.
	//
	// Added while building fixture 006, which is a Munge failure whose actual
	// cause is a 312-second clock error. Without this member the causal
	// observation had nowhere to go and would have been recorded as
	// ClassUnknown — the corpus arguing with the enum, which is why CLAUDE.md
	// §0.3 puts fixtures before the code they test.
	//
	// It earns a member rather than an attr because clock skew breaks three
	// unrelated things — Munge authentication, the (node, jobid, time) join, and
	// scheduler bookkeeping — and its remediation is specific and unlike any
	// other member's.
	ClassConfigClockSkew Class = "config.clock_skew"
)

// allClasses is the authoritative membership list. testdata/classes.golden
// mirrors it; a test diffs the two so that removing or renaming a member fails
// CI rather than passing quietly.
var allClasses = []Class{
	ClassUnknown,

	ClassAppNonzeroExit,
	ClassAppSegfault,

	ClassResourceOOM,
	ClassResourceOOMGPU,
	ClassResourceWalltimeExceeded,
	ClassResourceDiskQuota,

	ClassSchedNodeFail,
	ClassSchedPreempted,
	ClassSchedCancelled,
	ClassSchedRequeued,

	ClassAuthMunge,
	ClassAuthCredential,

	ClassFabricLinkFlap,
	ClassFabricCongestion,
	ClassFabricNCCLTimeout,

	ClassGPUDriverMismatch,
	ClassGPUXid,
	ClassGPUECC,
	ClassGPUFallenOffBus,

	ClassStorageMountMissing,
	ClassStorageIOError,
	ClassStorageStaleHandle,

	ClassConfigDrift,
	ClassConfigClockSkew,
}

var classSet = func() map[Class]struct{} {
	m := make(map[Class]struct{}, len(allClasses))
	for _, c := range allClasses {
		if _, dup := m[c]; dup {
			panic("schema: duplicate class in allClasses: " + string(c))
		}
		m[c] = struct{}{}
	}
	return m
}()

// AllClasses returns every member of the enum, sorted by wire string.
// The returned slice is a copy; callers may sort or mutate it freely.
func AllClasses() []Class {
	out := make([]Class, len(allClasses))
	copy(out, allClasses)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether c is a member of the enum.
func (c Class) Valid() bool {
	_, ok := classSet[c]
	return ok
}

func (c Class) String() string { return string(c) }

// Family returns the portion of the class before the first dot: "gpu" for
// "gpu.xid", "unknown" for "unknown". Used for grouping in output; it carries no
// semantics the enum itself does not already express.
func (c Class) Family() string {
	for i := 0; i < len(c); i++ {
		if c[i] == '.' {
			return string(c[:i])
		}
	}
	return string(c)
}

// ParseClass converts a wire string to a Class, rejecting non-members.
//
// It deliberately does not fall back to ClassUnknown: an unrecognized string on
// the wire means a version mismatch between producer and consumer, which the
// caller must handle knowingly. Collectors that cannot classify an observation
// should select ClassUnknown themselves.
func ParseClass(s string) (Class, error) {
	c := Class(s)
	if !c.Valid() {
		return "", fmt.Errorf("schema: unknown class %q (schema version %d)", s, Version)
	}
	return c, nil
}
