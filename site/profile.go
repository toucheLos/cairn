package site

import (
	"sort"
	"time"

	"github.com/touchelos/cairn/schema"
)

// ProfileVersion is the version of the site.yaml and node-profile formats.
//
// Deliberately separate from schema.Version. A profile is *state* — what this
// cluster is — while an event is an *observation*. Tying them together would
// mean that adding a probed field forced a schema version bump on every stored
// bundle, and that discovering a new kind of mount became a migration for
// consumers who only ever read events.
const ProfileVersion = 1

// Level mirrors collectors.Level for probes. Re-declared rather than imported so
// that site/ does not depend on collectors/ for a two-member enum; the Env it
// probes through is the only coupling worth having.
type Level int

const (
	LevelUnprivileged Level = iota
	LevelPrivileged
)

func (l Level) String() string {
	if l == LevelPrivileged {
		return "privileged"
	}
	return "unprivileged"
}

// Probe records one attempt to discover something about the stack.
//
// It mirrors collectors.Capability field for field, and for the same reason: a
// report of what was found is not usable without a report of what was looked
// for and missed. A site.yaml listing no fabric is ambiguous between "no
// InfiniBand here" and "nobody looked" — Reveals is what disambiguates it.
type Probe struct {
	// Name identifies the attempt, e.g. "scheduler" or "modules".
	Name string
	// Level is the access it required.
	Level Level
	// Available reports whether it found what it was looking for.
	Available bool
	// Detail says what happened, written for an admin rather than a developer.
	Detail string
	// Reveals describes what the profile is missing without it.
	Reveals string
}

// Scheduler is the batch system this site runs.
type Scheduler struct {
	// Kind is "slurm", "pbs", "lsf", "none", or "unknown". An unrecognized
	// scheduler is recorded as "unknown" and does not stop discovery
	// (invariant §2.6).
	Kind       string
	Version    string
	ConfigPath string
	Partitions []string
	QOS        []string
}

// Modules is the environment-module system.
type Modules struct {
	// Kind is "lmod", "tcl", or "none".
	Kind  string
	Roots []string
}

// Builder is a package build system with a discovered root.
type Builder struct {
	// Kind is "spack" or "easybuild".
	Kind string
	Root string
}

// OSFacts is the base system. These are the drift keys that go stale quietly:
// a node reimaged a kernel behind its siblings breaks jobs for months before
// anybody looks.
type OSFacts struct {
	ID            string // "rhel", "rocky", "ubuntu"
	VersionID     string
	KernelRelease string
	GlibcVersion  string
}

// Fabric is the high-speed interconnect.
type Fabric struct {
	// Kind is "infiniband", "roce", "ethernet", or "none".
	Kind  string
	HCAs  []string
	Rates []string
}

// GPUFacts is the accelerator stack.
type GPUFacts struct {
	// Vendor is "nvidia", "amd", or "none".
	Vendor        string
	DriverVersion string
	CUDAVersion   string
	Models        []string
	DCGM          bool
}

// Mount is one filesystem worth recording. Scratch and home are what submission
// scripts reference and what fails; the root filesystem is not interesting.
type Mount struct {
	Mountpoint string
	FSType     string
	Source     string
}

// BMCFacts records out-of-band management reachability.
type BMCFacts struct {
	// Kind is "redfish", "ipmi", or "none".
	Kind      string
	Reachable bool
}

// MetricsSystem is an existing telemetry stack. cairn reads from these where
// present rather than replacing them (CLAUDE.md §7, interop not replacement).
type MetricsSystem struct {
	// Kind is "prometheus" or "ganglia".
	Kind     string
	Endpoint string
}

// Profile is everything cairn discovered about one cluster.
//
// It is written to site.yaml, reviewed and corrected by an admin, and committed
// to git. That workflow is why the file carries comments and why decoding
// rejects unknown keys: a hand-edited file with a typo must fail loudly rather
// than silently reverting to a probed default.
type Profile struct {
	Version   int
	Cluster   schema.ClusterName
	Scheduler Scheduler
	Modules   Modules
	Builders  []Builder
	OS        OSFacts
	Fabric    Fabric
	GPU       GPUFacts
	Mounts    []Mount
	BMC       BMCFacts
	Metrics   []MetricsSystem
	Probes    []Probe
}

// Missing returns the probes that found nothing, sorted by name.
func (p Profile) Missing() []Probe {
	var out []Probe
	for _, pr := range p.Probes {
		if !pr.Available {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Drift keys. These are the §7 list — driver generation, kernel cmdline, glibc,
// module tree, mount set, munge key mtime — as stable dotted names.
//
// They are strings rather than an enum because a NodeProfile from a newer cairn
// must remain diffable by an older one: an unrecognized key is compared as data,
// which is the correct behavior for a comparison that never interprets the
// value. Compare schema.Class, which is closed precisely because it *is*
// interpreted.
const (
	KeyDriverVersion = "nvidia.driver_version"
	KeyCUDAVersion   = "nvidia.cuda_version"
	KeyKernelRelease = "kernel.release"
	KeyKernelCmdline = "kernel.cmdline"
	KeyGlibcVersion  = "glibc.version"
	KeyOSID          = "os.id"
	KeyOSVersion     = "os.version_id"
	KeyModuleRoots   = "modules.roots"
	KeyMungeKeyMtime = "munge.key_mtime"
	// KeyMountPrefix is followed by a mountpoint: "mount./scratch".
	KeyMountPrefix = "mount."
)

// NodeProfile is one node's drift keys, captured on that node.
//
// The values are a flat map rather than a struct because diff never interprets
// them — it compares them. Keeping the shape flat means adding a drift key is a
// change to the capture code alone, and neither the comparison nor the stored
// format has to learn anything.
type NodeProfile struct {
	Version int
	Cluster schema.ClusterName
	Node    schema.Hostname
	// CapturedAt is when the node was profiled.
	//
	// Unlike a bundle, a profile *does* carry a timestamp, and the difference is
	// worth stating. A bundle describes a fixed past window, so a generation
	// stamp would be noise that breaks byte-comparison (schema/bundle.go). A
	// profile describes a node *now*, and a fleet comparison across profiles
	// captured weeks apart is not a fleet comparison at all — so the staleness
	// has to be visible. diff reports the spread and refuses to hide it.
	CapturedAt time.Time
	Keys       map[string]string
	Probes     []Probe
}

// SortedKeys returns the drift keys in a stable order.
func (n NodeProfile) SortedKeys() []string {
	out := make([]string, 0, len(n.Keys))
	for k := range n.Keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
